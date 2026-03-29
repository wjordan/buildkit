//go:build nydus

package cache

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/pkg/labels"
	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/util/compression"
	digest "github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"github.com/containerd/nydus-snapshotter/pkg/converter"
	"github.com/containerd/nydus-snapshotter/pkg/label"
)

func init() {
	additionalAnnotations = append(
		additionalAnnotations,
		converter.LayerAnnotationNydusBlob, converter.LayerAnnotationNydusBootstrap, label.NydusRefLayer,
	)
}

// MergeNydus does two steps:
// 1. Extracts nydus bootstrap from nydus format (nydus blob + nydus bootstrap) for each layer.
// 2. Merge all nydus bootstraps into a final bootstrap (will as an extra layer).
// The nydus bootstrap size is very small, so the merge operation is fast.
func MergeNydus(ctx context.Context, ref ImmutableRef, comp compression.Config, s session.Group) (*ocispecs.Descriptor, error) {
	iref, ok := ref.(*immutableRef)
	if !ok {
		return nil, errors.Errorf("unsupported ref type %T", ref)
	}
	refs := iref.layerChain()
	if len(refs) == 0 {
		return nil, errors.Errorf("refs can't be empty")
	}

	parentDesc, parentBootstrapPath, cleanupParent, err := prepareParentBootstrap(ctx, iref, s)
	if err != nil {
		return nil, errors.Wrap(err, "prepare parent bootstrap")
	}
	if cleanupParent != nil {
		defer cleanupParent()
	}
	if parentDesc != nil {
		logrus.Warnf("MergeNydus: using parent bootstrap digest=%s path=%s", parentDesc.Digest, parentBootstrapPath)
	} else {
		logrus.Warnf("MergeNydus: no parent bootstrap found for ref=%s", iref.ID())
	}

	// Extracts nydus bootstrap from nydus format for each newly created layer.
	// Imported lazy-pulled nydus base layers do not carry mergeable per-layer
	// bootstraps, so we only merge the current ref's own layer chain and overlay
	// it on top of the inherited base bootstrap when present.
	layers := []converter.Layer{}
	layerDigests := make([]digest.Digest, 0, len(refs))
	for _, ref := range refs {
		origDesc, err := ref.ociDesc(ctx, ref.descHandlers, false)
		if err == nil {
			if _, isBootstrap := origDesc.Annotations[converter.LayerAnnotationNydusBootstrap]; isBootstrap {
				logrus.Warnf("MergeNydus: skipping bootstrap layer in current chain digest=%s", origDesc.Digest)
				continue
			}
		}

		blobDesc, err := getBlobWithCompressionWithRetry(ctx, ref, comp, s)
		if err != nil {
			return nil, errors.Wrapf(err, "get compression blob %q", comp.Type)
		}
		ra, err := ref.cm.ContentStore.ReaderAt(ctx, blobDesc)
		if err != nil {
			return nil, errors.Wrapf(err, "get reader for compression blob %q", comp.Type)
		}
		defer ra.Close()
		layers = append(layers, converter.Layer{
			Digest:   blobDesc.Digest,
			ReaderAt: ra,
		})
		layerDigests = append(layerDigests, blobDesc.Digest)
	}
	if len(layers) == 0 {
		if parentDesc != nil {
			logrus.Warnf("MergeNydus: no new nydus layers, reusing parent bootstrap digest=%s", parentDesc.Digest)
			return parentDesc, nil
		}
		return nil, errors.New("nydus blob layers can't be empty")
	}
	logrus.Warnf("MergeNydus: merging %d nydus layers=%v", len(layerDigests), layerDigests)

	// Merge all nydus bootstraps into a final nydus bootstrap.
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		opt := converter.MergeOption{
			WithTar: true,
		}
		if parentBootstrapPath != "" {
			opt.ParentBootstrapPath = parentBootstrapPath
		}
		if _, err := converter.Merge(ctx, layers, pw, opt); err != nil {
			pw.CloseWithError(errors.Wrapf(err, "merge nydus bootstrap"))
		}
	}()

	// Compress final nydus bootstrap to tar.gz and write into content store.
	cw, err := content.OpenWriter(ctx, iref.cm.ContentStore, content.WithRef("nydus-merge-"+iref.getChainID().String()))
	if err != nil {
		return nil, errors.Wrap(err, "open content store writer")
	}
	defer cw.Close()

	gw := gzip.NewWriter(cw)
	uncompressedDgst := digest.SHA256.Digester()
	compressed := io.MultiWriter(gw, uncompressedDgst.Hash())
	if _, err := io.Copy(compressed, pr); err != nil {
		return nil, errors.Wrapf(err, "copy bootstrap targz into content store")
	}
	if err := gw.Close(); err != nil {
		return nil, errors.Wrap(err, "close gzip writer")
	}

	compressedDgst := cw.Digest()
	if err := cw.Commit(ctx, 0, compressedDgst, content.WithLabels(map[string]string{
		labels.LabelUncompressed: uncompressedDgst.Digest().String(),
	})); err != nil {
		if !cerrdefs.IsAlreadyExists(err) {
			return nil, errors.Wrap(err, "commit to content store")
		}
	}
	if err := cw.Close(); err != nil {
		return nil, errors.Wrap(err, "close content store writer")
	}

	info, err := iref.cm.ContentStore.Info(ctx, compressedDgst)
	if err != nil {
		return nil, errors.Wrap(err, "get info from content store")
	}

	desc := ocispecs.Descriptor{
		Digest:    compressedDgst,
		Size:      info.Size,
		MediaType: ocispecs.MediaTypeImageLayerGzip,
		Annotations: map[string]string{
			labels.LabelUncompressed: uncompressedDgst.Digest().String(),
			// Use this annotation to identify nydus bootstrap layer.
			converter.LayerAnnotationNydusBootstrap: "true",
		},
	}

	return &desc, nil
}

func prepareParentBootstrap(ctx context.Context, iref *immutableRef, s session.Group) (*ocispecs.Descriptor, string, func(), error) {
	layerSet := iref.layerSet()
	var (
		parentDesc *ocispecs.Descriptor
		parentRef  *immutableRef
		checked    int
	)

	if err := iref.cacheRecord.walkUniqueAncestors(func(cr *cacheRecord) error {
		if _, ok := layerSet[cr.ID()]; ok {
			return nil
		}
		checked++
		desc, err := ociDescFromRecord(ctx, cr, iref.descHandlers)
		if err != nil {
			logrus.Warnf("MergeNydus: parent candidate record=%s has no descriptor: %v", cr.ID(), err)
			return nil
		}
		if _, ok := desc.Annotations[converter.LayerAnnotationNydusBootstrap]; !ok {
			return nil
		}
		logrus.Warnf("MergeNydus: found parent bootstrap candidate record=%s digest=%s", cr.ID(), desc.Digest)
		d := desc
		parentDesc = &d
		parentRef = &immutableRef{
			cacheRecord:  cr,
			descHandlers: iref.descHandlers,
		}
		return errSkipWalk
	}); err != nil {
		return nil, "", nil, err
	}

	if parentDesc == nil || parentRef == nil {
		logrus.Warnf("MergeNydus: no parent bootstrap candidate found after checking %d ancestors", checked)
		return nil, "", nil, nil
	}

	dir, err := os.MkdirTemp("", "buildkit-nydus-parent-bootstrap-")
	if err != nil {
		return nil, "", nil, err
	}
	cleanup := func() {
		_ = os.RemoveAll(dir)
	}
	outPath := filepath.Join(dir, "image.boot")

	ra, err := readerAtForRecord(ctx, parentRef, *parentDesc, s)
	if err != nil {
		cleanup()
		return nil, "", nil, err
	}
	defer ra.Close()

	if err := unpackBootstrapLayer(ra, outPath); err != nil {
		cleanup()
		return nil, "", nil, err
	}
	logrus.Warnf("MergeNydus: unpacked parent bootstrap digest=%s to %s", parentDesc.Digest, outPath)
	return parentDesc, outPath, cleanup, nil
}

func ociDescFromRecord(ctx context.Context, cr *cacheRecord, dhs DescHandlers) (ocispecs.Descriptor, error) {
	dgst := cr.getBlob()
	if dgst == "" {
		return ocispecs.Descriptor{}, errors.Errorf("no blob set for cache record %s", cr.ID())
	}

	desc := ocispecs.Descriptor{
		Digest:      dgst,
		Size:        cr.getBlobSize(),
		Annotations: make(map[string]string),
		MediaType:   cr.getMediaType(),
	}

	if blobDesc, err := getBlobDesc(ctx, cr.cm.ContentStore, desc.Digest); err == nil {
		if blobDesc.Annotations != nil {
			desc.Annotations = blobDesc.Annotations
		}
	} else if dh, ok := dhs[desc.Digest]; ok {
		for k, v := range filterAnnotationsForSave(dh.Annotations) {
			desc.Annotations[k] = v
		}
	}

	if diffID := cr.getDiffID(); diffID != "" {
		desc.Annotations[labels.LabelUncompressed] = string(diffID)
	}
	return desc, nil
}

func readerAtForRecord(ctx context.Context, ref *immutableRef, desc ocispecs.Descriptor, s session.Group) (content.ReaderAt, error) {
	ra, err := ref.cm.ContentStore.ReaderAt(ctx, desc)
	if err == nil {
		return ra, nil
	}

	provider := lazyRefProvider{
		ref:     ref,
		desc:    desc,
		dh:      ref.descHandlers[desc.Digest],
		session: s,
	}
	return provider.ReaderAt(ctx, desc)
}

func unpackBootstrapLayer(ra content.ReaderAt, outPath string) error {
	gr, err := gzip.NewReader(io.NewSectionReader(ra, 0, ra.Size()))
	if err != nil {
		return errors.Wrap(err, "open gzip bootstrap layer")
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return errors.Wrap(err, "read bootstrap layer tar")
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if hdr.Name != converter.BootstrapFileNameInLayer && hdr.Name != "image.boot" && !strings.HasSuffix(hdr.Name, "/image.boot") {
			continue
		}
		f, err := os.Create(outPath)
		if err != nil {
			return errors.Wrap(err, "create parent bootstrap file")
		}
		defer f.Close()
		if _, err := io.Copy(f, tr); err != nil {
			return errors.Wrap(err, "write parent bootstrap file")
		}
		return nil
	}

	return errors.New("bootstrap file not found in layer")
}
