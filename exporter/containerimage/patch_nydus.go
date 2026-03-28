//go:build nydus

package containerimage

import (
	"context"

	"github.com/containerd/nydus-snapshotter/pkg/converter"
	"github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/solver"
	"github.com/moby/buildkit/util/compression"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
)

// patchImageLayers appends an extra nydus bootstrap layer
// to the manifest of nydus image, normalizes layers and
// history. The nydus bootstrap layer represents the whole
// metadata of filesystem view for the entire image.
func patchImageLayers(ctx context.Context, remote *solver.Remote, history []ocispecs.History, ref cache.ImmutableRef, opts *ImageCommitOpts, sg session.Group) (*solver.Remote, []ocispecs.History, error) {
	if opts.RefCfg.Compression.Type != compression.Nydus {
		remote, history = normalizeLayersAndHistory(ctx, remote, history, ref, opts.OCITypes)
		return remote, history, nil
	}

	// Filter out nydus bootstrap layers from base images. When the base image
	// is already nydus-formatted, its bootstrap layer ends up in the descriptor
	// list as a converted blob. It must be removed because MergeNydus creates
	// a new merged bootstrap that replaces it.
	filtered := make([]ocispecs.Descriptor, 0, len(remote.Descriptors))
	for _, d := range remote.Descriptors {
		if d.Annotations != nil {
			if _, ok := d.Annotations[converter.LayerAnnotationNydusBootstrap]; ok {
				continue
			}
		}
		filtered = append(filtered, d)
	}
	remote.Descriptors = filtered

	desc, err := cache.MergeNydus(ctx, ref, opts.RefCfg.Compression, sg)
	if err != nil {
		return nil, nil, errors.Wrap(err, "merge nydus layer")
	}
	remote.Descriptors = append(remote.Descriptors, *desc)

	remote, history = normalizeLayersAndHistory(ctx, remote, history, ref, opts.OCITypes)
	return remote, history, nil
}
