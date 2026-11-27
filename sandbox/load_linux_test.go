//go:build linux

package sandbox

import (
	"debug/elf"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestAProgramThatCannotBeReadForItsLoadFoldersIsRefusedBeforeAnythingIsSet: a #! script, a file
// that is no ELF file, a FIFO and a program that is not there are each ErrUsage from Check, naming
// the file, with the zero Block and no_new_privs still unset; RunHolding, in a process of its own,
// refuses each the same way, and the script is not run.
//
// What it costs when this fails: a script runs behind folders told for no program, a FIFO where
// the program is named holds its caller for ever, or the block is half set on a process that was
// refused.
func TestAProgramThatCannotBeReadForItsLoadFoldersIsRefusedBeforeAnythingIsSet(t *testing.T) {
	t.Parallel()
	dir := realTemp(t, "rw", "bin")
	at := func(name string) string { return filepath.Join(dir, "bin", name) }
	if err := os.WriteFile(at("script"), []byte("#!/bin/sh\necho the script ran\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(at("text"), []byte("no program\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(at("fifo"), 0o700); err != nil {
		t.Fatal(err)
	}
	f := Folders{ReadWrite: []string{filepath.Join(dir, "rw")}, ReadOnly: []string{filepath.Join(dir, "bin")}}
	for name, says := range map[string]string{
		"script": "it is a #! script, and its interpreter is the program to run",
		"text":   "it is not an ELF file",
		"fifo":   "it is not a regular file",
		"gone":   "it cannot be found",
	} {
		b, err := Check(f, at(name))
		if !errors.Is(err, ErrUsage) || errors.Is(err, ErrNoBlock) || b != (Block{}) {
			t.Errorf("Check of a %s: Block %+v, %v, want the zero Block and ErrUsage, never ErrNoBlock", name, b, err)
		}
		if err == nil || !strings.Contains(err.Error(), `the program "`+at(name)+`"`) || !strings.Contains(err.Error(), says) {
			t.Errorf("Check of a %s: %v, want the file named and %q", name, err, says)
		}
		code, said := behindTheBlock(t, at(name))
		if code != childUsage || !strings.Contains(said, `the program "`+at(name)+`"`) || !strings.Contains(said, says) || strings.Contains(said, "the script ran") {
			t.Errorf("RunHolding of a %s: exit %d, said %q; want exit %d, the file named and %q", name, code, said, childUsage, says)
		}
	}
	set, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prGetNoNewPrivs, 0, 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	if set != 0 {
		t.Errorf("no_new_privs is set after the refusals, so one set a step of the block")
	}
}

// TestAProgramStartsBehindTheBlockByTheFoldersRunHoldingAddsAndByNoFewer: this system's true, a
// program that names a loader, starts behind RunHolding and exits 0 when the caller names its own
// folder alone, and false beside it exits 1, so the program is what ran. Each is the file its links
// resolve to, and the folder named is that file's: a /usr/bin/false that links to a multicall
// program in another folder, as Ubuntu 26.04's links to Rust's coreutils, is held to that folder.
// The plant: by RunHolding's own steps with nothing left out it exits 0 again, and with any one
// folder RunHolding added left out it does not start.
//
// What it costs when this fails: the folders RunHolding adds are not the ones this system's loader
// reads, so a held program does not start, or one of them is a grant the program never needed.
func TestAProgramStartsBehindTheBlockByTheFoldersRunHoldingAddsAndByNoFewer(t *testing.T) {
	t.Parallel()
	resolved := func(name string) string {
		t.Helper()
		p, err := filepath.EvalSymlinks(filepath.Join("/bin", name))
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	prog, falseProg := resolved("true"), resolved("false")
	dirs, err := loadHere(prog, prog, []string{filepath.Dir(prog)})
	if err != nil {
		t.Fatalf("the load folders of %s: %v", prog, err)
	}
	added := named{readOnly: []string{filepath.Dir(prog)}}.with(dirs).readOnly[1:]
	if len(added) == 0 {
		t.Fatalf("%s loads from %q, none outside its own folder, so this system has no row", prog, dirs)
	}
	t.Logf("%s loads from %q, and RunHolding adds %q", prog, dirs, added)
	for _, r := range []struct {
		prog  string
		leave []string
		want  int
	}{{prog, nil, 0}, {falseProg, nil, 1}, {prog, []string{"-"}, 0}} {
		if code, said := behindTheBlock(t, r.prog, r.leave...); code != r.want {
			t.Errorf("%s behind the block with %s alone named, leaving out %q: exit %d, want %d: %s", r.prog, filepath.Dir(r.prog), r.leave, code, r.want, strings.TrimSpace(said))
		}
	}
	for _, d := range added {
		if code, said := behindTheBlock(t, prog, d); code == 0 {
			t.Errorf("%s behind the block with the load folder %s left out: exit 0, want it not to start: %s", prog, d, strings.TrimSpace(said))
		} else {
			t.Logf("with %s left out: exit %d: %s", d, code, strings.TrimSpace(said))
		}
	}
}

// TestAProgramNamingALibraryOutsideTheFoldersIsRefusedBeforeAnythingIsSet: a program in the
// folder held, which names this system's loader and a library in another folder by its full path,
// is ErrUsage from Check, naming the library and its folder, with the zero Block, and RunHolding,
// in a process of its own, refuses it the same way. With the library's folder held too, Check does
// not refuse it with ErrUsage: the folders the caller names are what the search is given. A
// library named through a link in the folder held and "..", by its full path or by a run path, is
// judged by the file the kernel opens, the link's target's, and refused the same way, though a
// library of that name is in the folder held.
//
// What it costs when this fails: a program written into a read-write folder names a library in a
// folder the caller never named, and RunHolding grants the program that folder.
func TestAProgramNamingALibraryOutsideTheFoldersIsRefusedBeforeAnythingIsSet(t *testing.T) {
	t.Parallel()
	trueFile, err := filepath.EvalSymlinks("/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	host, err := osFiles{}.object(trueFile)
	if err != nil || host.interp == "" {
		t.Fatalf("this system's true names no loader to copy: %q, %v", host.interp, err)
	}
	dir := realTemp(t, "bin", "elsewhere", "rw")
	prog, lib := filepath.Join(dir, "bin", "prog"), filepath.Join(dir, "elsewhere", "libx.so")
	if err := os.WriteFile(lib, elfSpec{dynamic: []dynEntry{}}.bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prog, elfSpec{interp: []byte(host.interp + "\x00"), dynamic: []dynEntry{{elf.DT_NEEDED, lib}}, exec: true}.bytes(), 0o700); err != nil {
		t.Fatal(err)
	}
	says := `the library "` + lib + `", which "` + prog + `" needs, is the file "` + lib + `", and its folder "` + filepath.Dir(lib) + `" is beneath no folder held`
	rw := filepath.Join(dir, "rw")
	b, err := Check(Folders{ReadWrite: []string{rw}, ReadOnly: []string{filepath.Dir(prog)}}, prog)
	if !errors.Is(err, ErrUsage) || errors.Is(err, ErrNoBlock) || b != (Block{}) || !strings.Contains(err.Error(), says) {
		t.Errorf("Check of a program naming a library outside the folders: Block %+v, %v; want the zero Block, ErrUsage, never ErrNoBlock, and %q", b, err, says)
	}
	if code, said := behindTheBlock(t, prog); code != childUsage || !strings.Contains(said, says) {
		t.Errorf("RunHolding of a program naming a library outside the folders: exit %d, said %q; want exit %d and %q", code, said, childUsage, says)
	}
	if _, err := Check(Folders{ReadWrite: []string{rw}, ReadOnly: []string{filepath.Dir(prog), filepath.Dir(lib)}}, prog); errors.Is(err, ErrUsage) {
		t.Errorf("Check with the library's folder held too: %v, want no ErrUsage", err)
	}
	// The loader opens a path as written, and the kernel takes ".." from a link's target: through
	// bin/l, a link to elsewhere/deep, bin/l/../libx.so is elsewhere/libx.so, never the libx.so in
	// bin, a decoy in the folder held.
	if err := os.MkdirAll(filepath.Join(dir, "elsewhere", "deep"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "elsewhere", "deep"), filepath.Join(dir, "bin", "l")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "libx.so"), elfSpec{dynamic: []dynEntry{}}.bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	through := filepath.Join(dir, "bin", "l") + "/.."
	for _, r := range []struct {
		name    string
		dynamic []dynEntry
	}{
		{"by-full-path", []dynEntry{{elf.DT_NEEDED, through + "/libx.so"}}},
		{"by-run-path", []dynEntry{{elf.DT_NEEDED, "libx.so"}, {elf.DT_RUNPATH, through}}},
	} {
		prog := filepath.Join(dir, "bin", r.name)
		if err := os.WriteFile(prog, elfSpec{interp: []byte(host.interp + "\x00"), dynamic: r.dynamic, exec: true}.bytes(), 0o700); err != nil {
			t.Fatal(err)
		}
		says := `the library "` + through + `/libx.so", which "` + prog + `" needs, is the file "` + lib + `", and its folder "` + filepath.Dir(lib) + `" is beneath no folder held`
		b, err := Check(Folders{ReadWrite: []string{rw}, ReadOnly: []string{filepath.Dir(prog)}}, prog)
		if !errors.Is(err, ErrUsage) || errors.Is(err, ErrNoBlock) || b != (Block{}) || !strings.Contains(err.Error(), says) {
			t.Errorf("Check of a program naming a library %s through a link and \"..\": Block %+v, %v; want the zero Block, ErrUsage, never ErrNoBlock, and %q", r.name, b, err, says)
		}
		if code, said := behindTheBlock(t, prog); code != childUsage || !strings.Contains(said, says) {
			t.Errorf("RunHolding of a program naming a library %s through a link and \"..\": exit %d, said %q; want exit %d and %q", r.name, code, said, childUsage, says)
		}
	}
}

// systemChild is the exit of the test binary as a program only one system's rows run; this system's
// rows run none.
func systemChild([]string) (int, bool) { return 0, false }
