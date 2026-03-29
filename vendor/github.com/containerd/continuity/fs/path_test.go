package fs

import (
	"fmt"
	"testing"

	"golang.org/x/sys/unix"
)

func TestIgnoreContentCompareError(t *testing.T) {
	if !ignoreContentCompareError(fmt.Errorf("wrapped: %w", unix.EINVAL)) {
		t.Fatalf("expected EINVAL to be ignored")
	}
	if !ignoreContentCompareError(fmt.Errorf("wrapped: %w", unix.EIO)) {
		t.Fatalf("expected EIO to be ignored")
	}
	if ignoreContentCompareError(fmt.Errorf("wrapped: %w", unix.ENOENT)) {
		t.Fatalf("did not expect ENOENT to be ignored")
	}
}
