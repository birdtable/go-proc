//go:build darwin

package sandbox

import "testing"

// TestMacOSHoldsTheProgramAndEveryLinkOnItsPath: on macOS RunHolding walks a program's path with
// the kernel's MAXSYMLINKS of 32 and holds its real file and every link on the way, since the
// system sandbox holds no exec and refuses to follow a link outside its folders.
//
// What it costs when this fails: a program runs from a folder the caller never named, or fails
// after the exec with the system sandbox's exit 71, which a program could give too.
func TestMacOSHoldsTheProgramAndEveryLinkOnItsPath(t *testing.T) {
	t.Parallel()
	if got, want := programHere(), (programRule{walks: true, maxLinks: 32, linksHeld: true}); got != want {
		t.Errorf("the program rule on macOS is %+v, want %+v", got, want)
	}
}
