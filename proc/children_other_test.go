//go:build !unix

package proc

import (
	"errors"
	"testing"
)

// TestChildrenCPUIsUnsupportedOffUnix: on Windows, and every other system that is not Unix,
// ChildrenCPU gives 0 and errNoChildrenCPU, which wraps errors.ErrUnsupported and names Usage and
// JoinTree as the way to a tree's CPU time.
//
// What it costs when this fails: a caller reads no CPU time as children that used none, and bills or
// bounds them as idle.
func TestChildrenCPUIsUnsupportedOffUnix(t *testing.T) {
	t.Parallel()
	d, err := ChildrenCPU()
	if d != 0 || !errors.Is(err, errNoChildrenCPU) || !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("ChildrenCPU() = %v, %v, want 0 and %v", d, err, errNoChildrenCPU)
	}
}
