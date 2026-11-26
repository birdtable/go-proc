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
// not held, and starts the copy of true in the folder held, which exits 0. Nor can it start a
// script in the folder held whose interpreter is /bin/sh, or whose #! line is /bin/sh, a # and the
// copy of true in the folder held: the system sandbox holds the interpreter a script names, which
// RunHolding does not read for a program the one it runs starts. (A copy of env is killed by macOS
// outside any sandbox, so the starter is the test binary.) A #! script in the folder held
// whose interpreter is /bin/sh is ErrUsage from RunHolding and Check, naming /bin/sh, before
// anything is set; one whose interpreter is the copy of true in the folder held runs, and so does
// one whose #! line goes on after a # with a path outside, which the kernel ends at the #. A link
// in the folder held to the copy outside is TestAProgramWhoseRealFileIsNotHeldIsRefusedBeforeTheExec's row.
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
	script, shHashScript := filepath.Join(held, "script"), filepath.Join(held, "shhashscript")
	for p, body := range map[string]string{script: "#!/bin/sh\nexit 0\n", shHashScript: "#!/bin/sh#" + inside + "\n"} {
		if err := os.WriteFile(p, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []struct {
		claim string
		args  []string
		code  int
	}{
		{"/bin/echo", []string{"/bin/echo", "started"}, starterRefused},
		{"a copy of true outside the folders", []string{outside}, starterRefused},
		{"a copy of true in the folder held", []string{inside}, 0},
		{"a #!/bin/sh script in the folder held", []string{script}, starterRefused},
		{"a script in the folder held whose #! line is /bin/sh, a # and the copy of true there", []string{shHashScript}, starterRefused},
	} {
		code, said := withArgs(t, run, append([]string{starter}, r.args...)...)
		if code != r.code || strings.Contains(said, "started\n") {
			t.Errorf("the starter behind the block, starting %s: exit %d, said %q; want exit %d", r.claim, code, said, r.code)
		}
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
	hashScript := filepath.Join(held, "hashscript")
	if err := os.WriteFile(hashScript, []byte("#!"+inside+"#"+outside+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Check(Folders{ReadOnly: []string{held}}, hashScript); err != nil {
		t.Errorf("Check of a script whose #! line ends at a # after its held interpreter: %v, want nil", err)
	}
	if code, said := behindTheBlock(t, hashScript); code != 0 {
		t.Errorf("RunHolding of a script whose #! line ends at a # after its held interpreter: exit %d, said %q; want 0", code, said)
	}
}

// TestOnMacOSAFileThatIsNoProgramIsHeldToTheShellThatRunsIt: a file in the folder held that may
// run, with no #! and no Mach-O format, which the kernel refuses with ENOEXEC and execvp then runs
// with /bin/sh, is ErrUsage from RunHolding and Check, naming /bin/sh, before anything is set,
// unless /bin is held too: then Check passes it and RunHolding runs it, exit 0. The same file with
// no execute bit, mode 0600, which the kernel refuses with EACCES before any shell, is ErrUsage
// saying it may not run, with /bin held and without, and so is a #!/bin/sh script of mode 0600.
//
// What it costs when this fails: the file fails after the exec with the system sandbox's exit 71,
// which a program could give too, in place of ErrUsage saying which folder it needs, or the
// caller is told to hold all of /bin for a file no shell will run.
func TestOnMacOSAFileThatIsNoProgramIsHeldToTheShellThatRunsIt(t *testing.T) {
	t.Parallel()
	dir := realTemp(t, "held", "report")
	held := filepath.Join(dir, "held")
	plain := filepath.Join(held, "plain")
	if err := os.WriteFile(plain, []byte("exit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	says := `the program "` + plain + `" is neither a #! script nor a Mach-O program, so it is run by /bin/sh, which is held as a program is: the program "/bin/sh" cannot be held: it is the file "/bin/sh", whose folder "/bin" no folder held reaches`
	if _, err := Check(Folders{ReadOnly: []string{held}}, plain); !errors.Is(err, ErrUsage) || errors.Is(err, ErrNoBlock) || !strings.Contains(err.Error(), says) {
		t.Errorf("Check of a file that is no program, /bin not held: %v, want ErrUsage, never ErrNoBlock, and %q", err, says)
	}
	if code, said := behindTheBlock(t, plain); code != childUsage || !strings.Contains(said, says) {
		t.Errorf("RunHolding of a file that is no program, /bin not held: exit %d, said %q; want exit %d and %q", code, said, childUsage, says)
	}
	if _, err := Check(Folders{ReadOnly: []string{held, "/bin"}}, plain); err != nil {
		t.Errorf("Check of a file that is no program, /bin held: %v, want nil", err)
	}
	if code, said := heldRun(t, filepath.Join(dir, "report", "report"), "/bin", plain, nil); code != 0 {
		t.Errorf("RunHolding of a file that is no program, /bin held: exit %d, said %q; want 0", code, said)
	}

	locked, lockedScript := filepath.Join(held, "locked"), filepath.Join(held, "lockedscript")
	for p, body := range map[string]string{locked: "exit 0\n", lockedScript: "#!/bin/sh\nexit 0\n"} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mayNot := func(prog string) string {
		return `the program "` + prog + `" may not run: its real file "` + prog + `" has no execute bit`
	}
	for _, r := range []struct {
		claim, prog string
		folders     []string
	}{
		{"a file with no #! and no execute bit, /bin not held", locked, []string{held}},
		{"a file with no #! and no execute bit, /bin held", locked, []string{held, "/bin"}},
		{"a #!/bin/sh script with no execute bit, /bin not held", lockedScript, []string{held}},
		{"a #!/bin/sh script with no execute bit, /bin held", lockedScript, []string{held, "/bin"}},
	} {
		if _, err := Check(Folders{ReadOnly: r.folders}, r.prog); !errors.Is(err, ErrUsage) || errors.Is(err, ErrNoBlock) || !strings.Contains(err.Error(), mayNot(r.prog)) {
			t.Errorf("Check of %s: %v, want ErrUsage, never ErrNoBlock, and %q", r.claim, err, mayNot(r.prog))
		}
	}
	if code, said := behindTheBlock(t, locked); code != childUsage || !strings.Contains(said, mayNot(locked)) {
		t.Errorf("RunHolding of a file with no #! and no execute bit, /bin not held: exit %d, said %q; want exit %d and %q", code, said, childUsage, mayNot(locked))
	}
	if code, said := heldRun(t, filepath.Join(dir, "report", "report"), "/bin", locked, nil); code != childUsage || !strings.Contains(said, mayNot(locked)) {
		t.Errorf("RunHolding of a file with no #! and no execute bit, /bin held: exit %d, said %q; want exit %d and %q", code, said, childUsage, mayNot(locked))
	}
}

// withArgs is behindTheBlock for the program prog given args, every load folder kept.
func withArgs(t *testing.T, prog string, args ...string) (int, string) {
	t.Helper()
	return startedAs(t, argvChild, prog, args...)
}
