//go:build darwin

package sandbox

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// mover is the first argument that makes the test binary a program that moves a file by one call,
// as the next three arguments name it: link, clone, rename or swap, the file and where to. Its exit
// is 0 when the call is made, moverDenied when the kernel refuses it with EPERM, and moverFailed for
// any other error, which it writes to stdout.
const mover = "sandbox-mover"

// The mover's exits when the call is not made.
const (
	moverDenied = 1
	moverFailed = 4
)

// systemChild is the exit of the test binary as a program only macOS's rows run, the mover, when
// args, the process's arguments after its name, start with mover.
func systemChild(args []string) (int, bool) {
	if len(args) == 0 || args[0] != mover {
		return 0, false
	}
	return moved(os.NewFile(1, "stdout"), args[1:]), true
}

// moved makes the call args name: link(2), clonefile(2), rename(2) or renamex_np(2) with
// RENAME_SWAP, from args[1] to args[2], and writes to out why when the call is not made.
func moved(out io.Writer, args []string) int {
	if len(args) != 3 {
		_, _ = fmt.Fprintln(out, "the mover takes a call, a file and where to")
		return moverFailed
	}
	from, to := args[1], args[2]
	var err error
	switch args[0] {
	case "link":
		err = unix.Link(from, to)
	case "clone":
		err = unix.Clonefile(from, to, 0)
	case "rename":
		err = unix.Rename(from, to)
	case "swap":
		err = unix.RenamexNp(from, to, unix.RENAME_SWAP)
	default:
		err = fmt.Errorf("no call %q", args[0])
	}
	if err == nil {
		return 0
	}
	_, _ = fmt.Fprintln(out, err)
	if errors.Is(err, unix.EPERM) {
		return moverDenied
	}
	return moverFailed
}

// TestBehindTheMacBlockNothingOutsideIsLinkedClonedOrMovedIntoAReadWriteFolder: a copy of the test
// binary in a folder held read-only, run by RunHolding with one read-write folder, makes each call
// that would bring a file or folder from outside the folders into the read-write one: link(2) of a
// file, clonefile(2) and rename(2) of a file and of a folder, and renamex_np(2) with RENAME_SWAP of
// a file and of a folder with one of its kind in the read-write folder. Each is refused with
// EPERM, and the outside file, and what it was to be swapped with, are as they were, with nothing
// new in the read-write folder. With no block the test binary makes each call, the control. (A
// hard link of a folder the kernel refuses with no block, so no row proves it.) It holds on macOS
// 27.2, as measured; the row fails the day it stops holding.
//
// What it costs when this fails: a program behind the block takes a file it may not read into a
// folder it may, under a name it reads there, or swaps one of its own for a file outside.
func TestBehindTheMacBlockNothingOutsideIsLinkedClonedOrMovedIntoAReadWriteFolder(t *testing.T) {
	t.Parallel()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := realTemp(t, "held", "rw", "out")
	held, rw, out := filepath.Join(dir, "held"), filepath.Join(dir, "rw"), filepath.Join(dir, "out")
	run := copyOf(t, self, held)
	if _, err := Check(Folders{ReadWrite: []string{rw}, ReadOnly: []string{held}}, run); err != nil {
		t.Fatalf("Check of the mover held to %s and %s: %v, want the block", rw, held, err)
	}
	// put puts a file at p holding text, or a folder holding such a file, and state is what is at
	// p: its text, or its file's, and "" for nothing there.
	put := func(p, text string, folder bool) {
		t.Helper()
		if folder {
			if err := os.Mkdir(p, 0o700); err != nil {
				t.Fatal(err)
			}
			p = filepath.Join(p, "f")
		}
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	state := func(p string, folder bool) string {
		if folder {
			p = filepath.Join(p, "f")
		}
		b, _ := os.ReadFile(p)
		return string(b)
	}
	rows := []struct {
		call   string
		folder bool
	}{{"link", false}, {"clone", false}, {"clone", true}, {"rename", false}, {"rename", true}, {"swap", false}, {"swap", true}}
	for i, r := range rows {
		claim := fmt.Sprintf("%s of an outside file", r.call)
		if r.folder {
			claim = fmt.Sprintf("%s of an outside folder", r.call)
		}
		for _, held := range []bool{false, true} {
			name := fmt.Sprintf("%d-%t", i, held)
			src, dst := filepath.Join(out, name), filepath.Join(rw, name)
			put(src, "outside "+name+"\n", r.folder)
			if r.call == "swap" {
				put(dst, "inside "+name+"\n", r.folder)
			}
			argv := []string{self, mover, r.call, src, dst}
			if held {
				argv = []string{self, argvChild, run, filepath.Join(rw, "report"), mover, r.call, src, dst}
			}
			code, said := started(t, []string{"PATH=/usr/bin:/bin"}, argv)
			said = strings.TrimSpace(said)
			if !held {
				if code != 0 || state(dst, r.folder) != "outside "+name+"\n" {
					t.Errorf("%s, with no block: exit %d, %q in the read-write folder: %s; want the call made, or the row proves nothing", claim, code, state(dst, r.folder), said)
				}
				continue
			}
			wantDst := ""
			if r.call == "swap" {
				wantDst = "inside " + name + "\n"
			}
			if code != moverDenied || state(src, r.folder) != "outside "+name+"\n" || state(dst, r.folder) != wantDst {
				t.Errorf("%s, behind the block: exit %d, outside %q, inside %q: %s; want exit %d, EPERM, and both as they were", claim, code, state(src, r.folder), state(dst, r.folder), said, moverDenied)
			} else {
				t.Logf("%s, behind the block: %s", claim, said)
			}
		}
	}
	if len(rows) < 7 {
		t.Fatalf("ran %d rows", len(rows))
	}
}
