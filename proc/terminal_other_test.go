//go:build !unix && !windows

package proc

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestARunWithoutTerminalStartsNothingWhereNoTreeIsKilled: on a system with no way to kill a whole
// tree, a run without Terminal is refused as one with it is: errNoTree, which wraps
// errors.ErrUnsupported, and Code -1, and the helper that would make a file makes none.
//
// What it costs when this fails: a program the caller asked to run with no terminal of its own
// runs here with the caller's, and outside any tree a deadline kills.
func TestARunWithoutTerminalStartsNothingWhereNoTreeIsKilled(t *testing.T) {
	t.Parallel()
	for _, noTerminal := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "made")
		c := helperCmd(t, "touch", path)
		c.Terminal = !noTerminal
		res, err := Run(t.Context(), c)
		if !errors.Is(err, errNoTree) || !errors.Is(err, errors.ErrUnsupported) || res.Code != -1 {
			t.Errorf("a run with Terminal %v: %v, Code %d, want errNoTree, errors.ErrUnsupported and Code -1", !noTerminal, err, res.Code)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a run with Terminal %v made the file (%v), want nothing started", !noTerminal, err)
		}
	}
}
