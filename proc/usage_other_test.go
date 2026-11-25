//go:build !windows

package proc

import (
	"errors"
	"testing"
)

// TestUsagePidsAndJoinTreeAreUnsupportedOffWindows: on every system but Windows, Usage, Pids and
// JoinTree give nothing and errNoUsage, which wraps errors.ErrUnsupported, for any id, one Start
// gave included.
//
// What it costs when this fails: a caller reads a tree with no CPU time and no process as a tree
// that is idle or ended, and stops watching it, or JoinTree says a process joined a tree that Stop
// then cannot end.
func TestUsagePidsAndJoinTreeAreUnsupportedOffWindows(t *testing.T) {
	t.Parallel()
	for _, id := range []TreeID{"", "4242.17", `Local\go-proc-tree-0123456789abcdef0123456789abcdef`} {
		if u, err := Usage(id); !errors.Is(err, errNoUsage) || !errors.Is(err, errors.ErrUnsupported) || u != (TreeUsage{}) {
			t.Errorf("Usage(%q) = %+v, %v, want nothing and %v", id, u, err, errNoUsage)
		}
		if pids, err := Pids(id); !errors.Is(err, errNoUsage) || !errors.Is(err, errors.ErrUnsupported) || pids != nil {
			t.Errorf("Pids(%q) = %v, %v, want nothing and %v", id, pids, err, errNoUsage)
		}
	}
	if id, err := JoinTree(); !errors.Is(err, errNoUsage) || !errors.Is(err, errors.ErrUnsupported) || id != "" {
		t.Errorf("JoinTree() = %q, %v, want nothing and %v", id, err, errNoUsage)
	}
}
