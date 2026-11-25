package proc

import (
	"fmt"
	"testing"
)

// TestLeftIsWhetherTheTreeHeldAProcessOnceItsProgramExited: a program that starts a holding worker
// with none of its output and exits 0 gives Left true; one that waits for its child, and one that
// starts nothing, give Left false; each with and without ReadPeak, which on Linux starts the tree
// in a cgroup of its own where one is delegated, and with and without Terminal, which on Windows
// gives the tree a console host that ends only after the program. Each run exits 0 with no error,
// its worker killed with the tree.
//
// What it costs when this fails: a caller that fails closed on a program that leaves work running
// passes one that did, or refuses every run, as each reads Left true by a console host.
func TestLeftIsWhetherTheTreeHeldAProcessOnceItsProgramExited(t *testing.T) {
	t.Parallel()
	for _, r := range []struct {
		name string
		args []string
		want bool
	}{
		{"a program that leaves a worker running", []string{"leave"}, true},
		{"a program that waits for its child", []string{"hog-child", "1"}, false},
		{"a program that starts nothing", []string{"write", "0", "0", "exit"}, false},
	} {
		for _, peak := range []bool{false, true} {
			for _, noTerminal := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s, ReadPeak %v, Terminal %v", r.name, peak, !noTerminal), func(t *testing.T) {
					t.Parallel()
					c := helperCmd(t, r.args[0], r.args[1:]...)
					c.ReadPeak, c.Terminal = peak, !noTerminal
					res, err := Run(t.Context(), c)
					if err != nil || res.Code != 0 {
						t.Fatalf("%s: %v, Code %d, stderr %q, want exit 0", r.name, err, res.Code, res.Stderr)
					}
					if res.Left != r.want {
						t.Errorf("%s: Left %v, want %v", r.name, res.Left, r.want)
					}
				})
			}
		}
	}
}
