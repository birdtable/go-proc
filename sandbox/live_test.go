//go:build unix || windows

package sandbox

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// started starts argv, a program by its absolute path and its arguments, with environ, the null
// device as its input and a file of the test's own as its output and errors, and returns its exit
// and what it printed.
func started(t *testing.T, environ, argv []string) (int, string) {
	t.Helper()
	outPath := filepath.Join(t.TempDir(), "out")
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = null.Close() }()
	out, err := os.Create(outPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = out.Close() }()
	p, err := os.StartProcess(argv[0], argv, &os.ProcAttr{Env: environ, Files: []*os.File{null, out, out}}) //nolint:forbidigo // a row starts the test binary itself, as the process a caller sets the block on, or a program with no block, as its control
	if err != nil {
		t.Fatal(err)
	}
	st, err := p.Wait()
	if err != nil {
		t.Fatal(err)
	}
	said, _ := os.ReadFile(outPath)
	return st.ExitCode(), string(said)
}

// heldRun starts the test binary as HoldChild with report, readOnly, prog and args, and returns the
// program's exit, or HoldChild's with why the program did not run, and what it printed.
func heldRun(t *testing.T, report, readOnly, prog string, args []string) (int, string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	code, said := started(t, os.Environ(), append([]string{self, holdChild, report, readOnly, prog}, args...))
	why, _ := os.ReadFile(report)
	if err := os.Remove(report); err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return code, strings.TrimSpace(string(why) + said)
}

// copied says whether dst is there and holds what src holds.
func copied(t *testing.T, src, dst string) bool {
	t.Helper()
	want, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	return err == nil && len(want) > 0 && bytes.Equal(got, want)
}

// TestAProgramBehindRunHoldingReadsAndWritesBeneathItsFoldersAlone: this system's own copy program,
// cp, or cmd's copy on Windows, makes each of four copies when it runs as it is, the control. Behind
// RunHolding, which holds it to a read-write folder, a read-only one and its own folder, it copies a
// file beneath the read-only folder into the read-write one, and is refused a file outside the
// folders, a copy into a folder outside them and a copy into the read-only folder: it exits 1, and
// the copy is not there. Where Check says this system gives no block, ErrNoBlock, as on Windows,
// RunHolding refuses each run before the program starts, and nothing is copied.
//
// What it costs when this fails: the files block the fake-kernel rows check is not the one a real
// program meets, and a held program reads or writes beyond its folders, or cannot reach its own.
func TestAProgramBehindRunHoldingReadsAndWritesBeneathItsFoldersAlone(t *testing.T) {
	t.Parallel()
	dir := realTemp(t, "rw", "ro", "out")
	rw, ro, out := filepath.Join(dir, "rw"), filepath.Join(dir, "ro"), filepath.Join(dir, "out")
	inside, outside := filepath.Join(ro, "in"), filepath.Join(out, "secret")
	for path, text := range map[string]string{inside: "inside\n", outside: "outside\n"} {
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	prog := copier(t)
	_, noBlock := Check(Folders{ReadWrite: []string{rw}, ReadOnly: []string{ro, filepath.Dir(prog)}}, prog)
	switch {
	case noBlock == nil:
		t.Logf("Check: this system gives the block, so %s runs behind it", prog)
	case errors.Is(noBlock, ErrNoBlock) && !errors.Is(noBlock, ErrUsage):
		t.Logf("Check: this system gives no block, so RunHolding must refuse each run: %v", noBlock)
	default:
		t.Fatalf("Check of %s held to %s and %s: %v, want nil or ErrNoBlock, never ErrUsage", prog, rw, ro, noBlock)
	}
	for _, r := range []struct {
		claim    string
		src, dst string
		copies   bool
	}{
		{"a read beneath the read-only folder and a write beneath the read-write one", inside, filepath.Join(rw, "copied"), true},
		{"a read outside the folders", outside, filepath.Join(rw, "taken"), false},
		{"a write outside the folders", inside, filepath.Join(out, "left"), false},
		{"a write beneath the read-only folder", inside, filepath.Join(ro, "written"), false},
	} {
		free := r.dst + ".free"
		if code, said := started(t, os.Environ(), append([]string{prog}, copyArgs(r.src, free)...)); code != 0 || !copied(t, r.src, free) {
			t.Errorf("%s, with no block: exit %d: %s; want the copy made, or the row proves nothing", r.claim, code, strings.TrimSpace(said))
		}
		code, said := heldRun(t, filepath.Join(rw, "report"), ro, prog, copyArgs(r.src, r.dst))
		_, err := os.Stat(r.dst)
		switch {
		case noBlock != nil:
			if code != childFailed || !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("%s, where the system gives no block: exit %d, the copy there %t: %s; want exit %d and no copy", r.claim, code, err == nil, said, childFailed)
			}
		case r.copies:
			if code != 0 || !copied(t, r.src, r.dst) {
				t.Errorf("%s, behind the block: exit %d, the copy made %t: %s; want exit 0 and the copy made", r.claim, code, copied(t, r.src, r.dst), said)
			}
		default:
			if code != 1 || !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("%s, behind the block: exit %d, the copy there %t: %s; want exit 1, the program refused, and no copy", r.claim, code, err == nil, said)
			} else {
				t.Logf("%s, behind the block: refused: %s", r.claim, said)
			}
		}
	}
}
