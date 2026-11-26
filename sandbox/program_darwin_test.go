//go:build darwin

package sandbox

import (
	"encoding/binary"
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
// saying it may not run, with /bin held and without, and so is a #!/bin/sh script of mode 0600,
// and one of mode 0601 or 0610 that the caller owns, whose execute bit is not the caller's: the
// kernel reads the owner's bits for the owner, as access(2) asks it.
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
	othersOnly, groupOnly := filepath.Join(held, "othersonly"), filepath.Join(held, "grouponly")
	for _, f := range []struct {
		path, body string
		mode       os.FileMode
	}{
		{locked, "exit 0\n", 0o600},
		{lockedScript, "#!/bin/sh\nexit 0\n", 0o600},
		{othersOnly, "#!/bin/sh\nexit 0\n", 0o601},
		{groupOnly, "#!/bin/sh\nexit 0\n", 0o610},
	} {
		if err := os.WriteFile(f.path, []byte(f.body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(f.path, f.mode); err != nil {
			t.Fatal(err)
		}
	}
	mayNot := func(prog string) string {
		return `the program "` + prog + `" may not run: access(2) says this caller may not run its real file "` + prog + `"`
	}
	for _, r := range []struct {
		claim, prog string
		folders     []string
	}{
		{"a file with no #! and no execute bit, /bin not held", locked, []string{held}},
		{"a file with no #! and no execute bit, /bin held", locked, []string{held, "/bin"}},
		{"a #!/bin/sh script with no execute bit, /bin not held", lockedScript, []string{held}},
		{"a #!/bin/sh script with no execute bit, /bin held", lockedScript, []string{held, "/bin"}},
		{"a #!/bin/sh script of mode 0601 the caller owns, /bin held", othersOnly, []string{held, "/bin"}},
		{"a #!/bin/sh script of mode 0610 the caller owns, /bin held", groupOnly, []string{held, "/bin"}},
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
	if code, said := heldRun(t, filepath.Join(dir, "report", "report"), "/bin", othersOnly, nil); code != childUsage || !strings.Contains(said, mayNot(othersOnly)) {
		t.Errorf("RunHolding of a script of mode 0601 the caller owns, /bin held: exit %d, said %q; want exit %d and %q", code, said, childUsage, mayNot(othersOnly))
	}
}

// TestOnMacOSAFileTheKernelRefusesWithENOEXECIsHeldToTheShell: two more files the kernel refuses
// with ENOEXEC, which execvp then runs with /bin/sh, are ErrUsage from RunHolding and Check, naming
// /bin/sh, before anything is set, unless /bin is held too: a Mach-O file in the Mac's byte order
// whose filetype is a library's, MH_DYLIB, and a #! script whose interpreter, in the folder held,
// is a #! script itself, or a plain file, as the kernel starts no interpreter for a script but a
// Mach-O program. With /bin held Check passes each, and RunHolding runs each script with /bin/sh,
// which reads its #! line as a comment and exits 0. The kernel looks such an interpreter up, under
// the system sandbox, before it refuses it, so it is held as well as /bin/sh: a script whose
// interpreter is a #! script in a folder not held is ErrUsage naming that folder from Check and
// RunHolding, with /bin held. A script whose interpreter is a Mach-O program
// in the folder held is TestBehindTheMacBlockAProgramStartsOnlyProgramsInTheFolders's row. A
// universal file whose arm64 slice is that library passes Check with /bin not held, as its slices
// are not read: the limit docs/sandbox.md names, whose line
// TestTheSandboxDocNamesTheUniversalFileWhoseSlicesAreNotRead reads.
//
// What it costs when this fails: the file passes Check and fails after the exec with the system
// sandbox's exit 71, which a program could give too, in place of ErrUsage naming /bin/sh; or, for
// the universal file, the doc names a limit the code no longer has.
func TestOnMacOSAFileTheKernelRefusesWithENOEXECIsHeldToTheShell(t *testing.T) {
	t.Parallel()
	dir := realTemp(t, "held", "report")
	held := filepath.Join(dir, "held")
	library := filepath.Join(held, "library")
	header := []byte{0xcf, 0xfa, 0xed, 0xfe, 0x0c, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x06, 0x00, 0x00, 0x00}
	if err := os.WriteFile(library, append(header, make([]byte, 4080)...), 0o700); err != nil {
		t.Fatal(err)
	}
	inner, plain := filepath.Join(held, "inner"), filepath.Join(held, "plain")
	outerScript, outerPlain := filepath.Join(held, "outerscript"), filepath.Join(held, "outerplain")
	for p, body := range map[string]string{inner: "#!/usr/bin/true\n", plain: "exit 0\n", outerScript: "#!" + inner + "\n", outerPlain: "#!" + plain + "\n"} {
		if err := os.WriteFile(p, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	shell := `which is held as a program is: the program "/bin/sh" cannot be held: it is the file "/bin/sh", whose folder "/bin" no folder held reaches`
	nested := func(interp string) string {
		return `is a #! script whose interpreter's real file "` + interp + `" is no Mach-O program, which the kernel refuses to start for a script, so it is run by /bin/sh, ` + shell
	}
	for _, r := range []struct {
		claim, prog, says string
		runs              bool
	}{
		{"a Mach-O library", library, `the program "` + library + `" is neither a #! script nor a Mach-O program, so it is run by /bin/sh, ` + shell, false},
		{"a script whose interpreter is a script", outerScript, `the program "` + outerScript + `" ` + nested(inner), true},
		{"a script whose interpreter is a plain file", outerPlain, `the program "` + outerPlain + `" ` + nested(plain), true},
	} {
		if _, err := Check(Folders{ReadOnly: []string{held}}, r.prog); !errors.Is(err, ErrUsage) || errors.Is(err, ErrNoBlock) || !strings.Contains(err.Error(), r.says) {
			t.Errorf("Check of %s, /bin not held: %v, want ErrUsage, never ErrNoBlock, and %q", r.claim, err, r.says)
		}
		if code, said := behindTheBlock(t, r.prog); code != childUsage || !strings.Contains(said, r.says) {
			t.Errorf("RunHolding of %s, /bin not held: exit %d, said %q; want exit %d and %q", r.claim, code, said, childUsage, r.says)
		}
		if _, err := Check(Folders{ReadOnly: []string{held, "/bin"}}, r.prog); err != nil {
			t.Errorf("Check of %s, /bin held: %v, want nil", r.claim, err)
		}
		if !r.runs {
			continue
		}
		if code, said := heldRun(t, filepath.Join(dir, "report", "report"), "/bin", r.prog, nil); code != 0 {
			t.Errorf("RunHolding of %s, /bin held: exit %d, said %q; want 0", r.claim, code, said)
		}
	}

	outside := filepath.Join(dir, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	away, outerAway := filepath.Join(outside, "inner"), filepath.Join(held, "outeraway")
	for p, body := range map[string]string{away: "#!/usr/bin/true\n", outerAway: "#!" + away + "\n"} {
		if err := os.WriteFile(p, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	says := `the program "` + outerAway + `" is a #! script, and its interpreter is held as a program is: the program "` + away + `" cannot be held: it is the file "` + away + `", whose folder "` + outside + `" no folder held reaches`
	if _, err := Check(Folders{ReadOnly: []string{held, "/bin"}}, outerAway); !errors.Is(err, ErrUsage) || errors.Is(err, ErrNoBlock) || !strings.Contains(err.Error(), says) {
		t.Errorf("Check of a script whose interpreter is a script outside the folders, /bin held: %v, want ErrUsage, never ErrNoBlock, and %q", err, says)
	}
	if code, said := heldRun(t, filepath.Join(dir, "report", "report"), "/bin", outerAway, nil); code != childUsage || !strings.Contains(said, says) {
		t.Errorf("RunHolding of a script whose interpreter is a script outside the folders, /bin held: exit %d, said %q; want exit %d and %q", code, said, childUsage, says)
	}

	universal := filepath.Join(held, "universal")
	fat := make([]byte, 4096)
	binary.BigEndian.PutUint32(fat[0:], 0xcafebabe)
	binary.BigEndian.PutUint32(fat[4:], 1)
	binary.BigEndian.PutUint32(fat[8:], 0x0100000c)
	binary.BigEndian.PutUint32(fat[16:], 4096)
	binary.BigEndian.PutUint32(fat[20:], 4096)
	binary.BigEndian.PutUint32(fat[24:], 12)
	if err := os.WriteFile(universal, append(append(fat, header...), make([]byte, 4080)...), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Check(Folders{ReadOnly: []string{held}}, universal); err != nil {
		t.Errorf("Check of a universal file whose arm64 slice is a library, /bin not held: %v; want nil, the limit docs/sandbox.md names", err)
	}
}

// withArgs is behindTheBlock for the program prog given args, every load folder kept.
func withArgs(t *testing.T, prog string, args ...string) (int, string) {
	t.Helper()
	return startedAs(t, argvChild, prog, args...)
}
