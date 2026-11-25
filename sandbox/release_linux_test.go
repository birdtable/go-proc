//go:build linux

package sandbox

import (
	"errors"
	"io/fs"
	"os"
	"testing"
)

// TestTheReleaseOfTheProbeLeavesNoGoroutineAndNoFolder: the probe's folder is
// released from a goroutine of its own, on a thread Landlock has not
// restricted, so the release is held to ending that goroutine and removing the
// folder, its sockets and its file. The goroutine is named by its stack, not
// counted: one of another package's code started meanwhile is no leak of the
// release's, and the release's own is given settleWait to end.
//
// What it costs when this fails: every block leaves a goroutine waiting behind
// it, or a folder of sockets in the temporary folder, and nobody is told.
func TestTheReleaseOfTheProbeLeavesNoGoroutineAndNoFolder(t *testing.T) {
	// serial: reads every goroutine of the process, which a parallel test's would add to
	h, err := linuxKernel{}.holdOutside()
	if err != nil {
		t.Fatalf("holding the probe: %v", err)
	}
	if _, err := os.Stat(h.dir); err != nil {
		t.Fatalf("the probe's folder %s is not there before the release: %v", h.dir, err)
	}
	before := goroutines()
	if err := (linuxKernel{}).release(h); err != nil {
		t.Fatalf("releasing the probe: %v", err)
	}
	for _, g := range leftOver(t, before, settleWait(t)) {
		t.Errorf("goroutine %s runs this package's code after the release:\n%s", g.id, g.stack)
	}
	if _, err := os.Stat(h.dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the probe's folder %s is still there after the release: %v", h.dir, err)
	}
}
