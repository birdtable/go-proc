//go:build linux

package sandbox

import "testing"

// TestLinuxHoldsTheProgramsRealFileAsLandlockDoes: on Linux RunHolding walks a program's path with
// the kernel's MAXSYMLINKS of 40 and holds its real file alone, as Landlock holds the exec, and not
// the links on the way, which Landlock follows with no right asked.
//
// What it costs when this fails: RunHolding refuses a program the kernel would run, as one reached
// through /usr/bin on a system whose /bin is a link, or walks past the links the kernel follows.
func TestLinuxHoldsTheProgramsRealFileAsLandlockDoes(t *testing.T) {
	t.Parallel()
	if got, want := programHere(), (programRule{walks: true, maxLinks: 40}); got != want {
		t.Errorf("the program rule on Linux is %+v, want %+v", got, want)
	}
}
