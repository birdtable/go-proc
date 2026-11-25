//go:build darwin

package sandbox

import (
	"os"
	"testing"
)

// TestCheckOnThisMacSaysWhichBlockItGives: on macOS, where sandbox-exec is there as the tests need,
// Check with good folders and with none is nil with macOS's Block: the network, daemons by
// mach-lookup and the terminal closed, and a signal to a process outside and launchd open.
//
// What it costs when this fails: a caller asks first on macOS and is told the signals of processes
// outside, or launchd, are closed, which the profile leaves open.
func TestCheckOnThisMacSaysWhichBlockItGives(t *testing.T) {
	t.Parallel()
	dir := realTemp(t, "rw", "ro")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for claim, f := range map[string]Folders{
		"good folders": {ReadWrite: []string{dir + "/rw"}, ReadOnly: []string{dir + "/ro"}},
		"no folders":   {},
	} {
		if b, err := Check(f, self); err != nil || b != macGives() {
			t.Errorf("Check with %s: Block %+v, %v, want macOS's, %+v", claim, b, err, macGives())
		}
	}
}
