package main

import (
	"fmt"
	"os"

	"github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/containerd/nydus-snapshotter/pkg/converter"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintf(os.Stderr, "usage: %s <nydus-blob-path> <output-bootstrap-path>\n", os.Args[0])
		os.Exit(2)
	}

	ra, err := local.OpenReader(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "open reader: %v\n", err)
		os.Exit(1)
	}
	defer ra.Close()

	out, err := os.Create(os.Args[2])
	if err != nil {
		fmt.Fprintf(os.Stderr, "create output: %v\n", err)
		os.Exit(1)
	}
	defer out.Close()

	if _, err := converter.UnpackEntry(ra, converter.EntryBootstrap, out); err != nil {
		fmt.Fprintf(os.Stderr, "unpack bootstrap: %v\n", err)
		os.Exit(1)
	}
}
