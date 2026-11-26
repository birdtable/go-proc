//go:build darwin

package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestCheckOnThisMacSaysWhichBlockItGives: on macOS, where sandbox-exec is there as the tests need,
// Check with good folders and with none is nil with macOS's Block: the network, daemons by
// mach-lookup and the terminal closed, and a signal to a process outside and launchd open. Good
// folders hold the program, named by its real file; folders that leave its folder out are ErrUsage.
//
// What it costs when this fails: a caller asks first on macOS and is told the signals of processes
// outside, or launchd, are closed, which the profile leaves open.
func TestCheckOnThisMacSaysWhichBlockItGives(t *testing.T) {
	t.Parallel()
	dir := realTemp(t, "rw", "ro")
	self, err := os.Executable()
	if err == nil {
		self, err = filepath.EvalSymlinks(self)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Check(Folders{ReadWrite: []string{dir + "/rw"}, ReadOnly: []string{dir + "/ro"}}, self); !errors.Is(err, ErrUsage) {
		t.Errorf("Check with folders that leave the program's out: %v, want ErrUsage", err)
	}
	for claim, f := range map[string]Folders{
		"good folders": {ReadWrite: []string{dir + "/rw"}, ReadOnly: []string{dir + "/ro", filepath.Dir(self)}},
		"no folders":   {},
	} {
		if b, err := Check(f, self); err != nil || b != macGives() {
			t.Errorf("Check with %s: Block %+v, %v, want macOS's, %+v", claim, b, err, macGives())
		}
	}
}
