//go:build darwin

package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMacOSHoldsTheProgramAndEveryLinkOnItsPath: on macOS RunHolding walks a program's path with
// the kernel's MAXSYMLINKS of 32 and holds its real file, every link on the way and a #! script's
// interpreter, as the system sandbox refuses to follow a link outside its folders or to start a
// program there.
//
// What it costs when this fails: a program runs from a folder the caller never named, or fails
// after the exec with the system sandbox's exit 71, which a program could give too.
func TestMacOSHoldsTheProgramAndEveryLinkOnItsPath(t *testing.T) {
	t.Parallel()
	if got, want := programHere(), (programRule{walks: true, maxLinks: 32, linksHeld: true, interpreters: true}); got != want {
		t.Errorf("the program rule on macOS is %+v, want %+v", got, want)
	}
}

// TestBehindTheMacBlockAProgramStartsOnlyProgramsInTheFolders: a copy of the test binary in the
// folder held, run by RunHolding as a starter, cannot start /bin/echo or a copy of true in a folder
// not held, and starts the copy of true in the folder held, which exits 0. (A copy of env is killed
// by macOS outside any sandbox, so the starter is the test binary.) A #! script in the folder held
// whose interpreter is /bin/sh is ErrUsage from RunHolding and Check, naming /bin/sh, before
// anything is set; one whose interpreter is the copy of true in the folder held runs. A link in the
// folder held to the copy outside is TestAProgramWhoseRealFileIsNotHeldIsRefusedBeforeTheExec's row.
//
// What it costs when this fails: a program behind the block starts any program on the machine,
// outside the folders and their hold, or a script fails late with the system sandbox's exit 71.
func TestBehindTheMacBlockAProgramStartsOnlyProgramsInTheFolders(t *testing.T) {
	t.Parallel()
	dir := realTemp(t, "held", "other")
	held, other := filepath.Join(dir, "held"), filepath.Join(dir, "other")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	run, inside, outside := copyOf(t, self, held), copyProgram(t, held), copyProgram(t, other)
	for _, r := range []struct {
		claim string
		args  []string
		code  int
	}{
		{"/bin/echo", []string{"/bin/echo", "started"}, starterRefused},
		{"a copy of true outside the folders", []string{outside}, starterRefused},
		{"a copy of true in the folder held", []string{inside}, 0},
	} {
		code, said := withArgs(t, run, append([]string{starter}, r.args...)...)
		if code != r.code || strings.Contains(said, "started\n") {
			t.Errorf("the starter behind the block, starting %s: exit %d, said %q; want exit %d", r.claim, code, said, r.code)
		}
	}

	script := filepath.Join(held, "script")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	says := `the program "/bin/sh" cannot be held: it is the file "/bin/sh", whose folder "/bin" no folder held reaches`
	if _, err := Check(Folders{ReadOnly: []string{held}}, script); !errors.Is(err, ErrUsage) || errors.Is(err, ErrNoBlock) || !strings.Contains(err.Error(), says) {
		t.Errorf("Check of a script whose interpreter is not held: %v, want ErrUsage, never ErrNoBlock, and %q", err, says)
	}
	if code, said := behindTheBlock(t, script); code != childUsage || !strings.Contains(said, says) {
		t.Errorf("RunHolding of a script whose interpreter is not held: exit %d, said %q; want exit %d and %q", code, said, childUsage, says)
	}
	heldScript := filepath.Join(held, "heldscript")
	if err := os.WriteFile(heldScript, []byte("#!"+inside+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if code, said := behindTheBlock(t, heldScript); code != 0 {
		t.Errorf("RunHolding of a script whose interpreter is held: exit %d, said %q; want 0", code, said)
	}
}

// withArgs is behindTheBlock for the program prog given args, every load folder kept.
func withArgs(t *testing.T, prog string, args ...string) (int, string) {
	t.Helper()
	return startedAs(t, argvChild, prog, args...)
}
