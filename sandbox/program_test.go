package sandbox

import (
	"errors"
	"io/fs"
	"os"
	"reflect"
	"strings"
	"testing"
)

// linkFake is the files a walk reads on a system that is not there: each path's mode, and each
// link's target.
type linkFake struct {
	modes   map[string]fs.FileMode
	targets map[string]string
}

func (f linkFake) lstat(name string) (fs.FileMode, error) {
	m, ok := f.modes[name]
	if !ok {
		return 0, fs.ErrNotExist
	}
	return m, nil
}

func (f linkFake) readlink(name string) (string, error) {
	to, ok := f.targets[name]
	if !ok {
		return "", fs.ErrInvalid
	}
	return to, nil
}

// tree is a linkFake with each of dirs a folder, files regular files and links links.
func tree(dirs, regular []string, links map[string]string) linkFake {
	f := linkFake{modes: map[string]fs.FileMode{}, targets: map[string]string{}}
	for _, d := range dirs {
		f.modes[d] = fs.ModeDir | 0o755
	}
	for _, r := range regular {
		f.modes[r] = 0o755
	}
	for from, to := range links {
		f.modes[from] = fs.ModeSymlink | 0o777
		f.targets[from] = to
	}
	return f
}

// TestTheWalkFollowsAProgramsPathAsTheKernelDoes: the walk ends on the real file and lists each
// link on the way by its own path, in order: a relative link from its own folder, an absolute one
// from the root, a link to a folder in the middle of the path, a chain, and a ".." after a link,
// which leaves the link's target, not its name. A name that is not there, a dangling or empty
// link, a name after a file, a path past the link limit, and a path that ends on a folder or a
// FIFO are ErrUsage, naming the program.
//
// What it costs when this fails: the file RunHolding checks against the folders is not the file
// the kernel runs, so a program outside them passes the check, or the links the system sandbox
// refuses are not the ones named.
func TestTheWalkFollowsAProgramsPathAsTheKernelDoes(t *testing.T) {
	t.Parallel()
	f := tree(
		[]string{"/held", "/other", "/other/sub", "/deep", "/deep/in"},
		[]string{"/held/real", "/other/true", "/other/sub/x"},
		map[string]string{
			"/held/rel":      "real",
			"/held/abs":      "/other/true",
			"/held/dirlink":  "../other",
			"/held/chain":    "/other/hop",
			"/other/hop":     "true",
			"/held/updir":    "/other/sub",
			"/deep/in/up":    "../../other/sub",
			"/held/dangling": "nothing",
			"/held/empty":    "",
			"/held/loop":     "/held/loop",
		},
	)
	for _, r := range []struct {
		prog  string
		file  string
		links []string
	}{
		{"/held/real", "/held/real", nil},
		{"/held/rel", "/held/real", []string{"/held/rel"}},
		{"/held/abs", "/other/true", []string{"/held/abs"}},
		{"/held/dirlink/true", "/other/true", []string{"/held/dirlink"}},
		{"/held/chain", "/other/true", []string{"/held/chain", "/other/hop"}},
		// ".." after a link leaves its target, /other/sub, for /other, as the kernel does; cleaning
		// the text would give /held/true, which is not there.
		{"/held/updir/../true", "/other/true", []string{"/held/updir"}},
		{"/deep/in/up/x", "/other/sub/x", []string{"/deep/in/up"}},
		{"//held/./real", "/held/real", nil},
	} {
		w, err := walk(f, r.prog, 40)
		if err != nil || w.file != r.file || !reflect.DeepEqual(w.links, r.links) {
			t.Errorf("walk(%q): %+v, %v; want the file %q through %q", r.prog, w, err, r.file, r.links)
		}
	}
	for _, r := range []struct{ prog, says string }{
		{"/held/gone", "it cannot be found"},
		{"/held/dangling", "it cannot be found"},
		{"/held/empty", `the link "/held/empty" cannot be read: its target is empty`},
		{"/held/real/x", `"/held/real" is not a folder`},
		{"/held/real/", `"/held/real" is not a folder`},
		{"/held/loop", "more than 40 links"},
		{"/other/sub", `it is not a regular file: it is the file "/other/sub"`},
		{"/held/dirlink", `it is not a regular file: it is the file "/other"`},
	} {
		w, err := walk(f, r.prog, 40)
		if !errors.Is(err, ErrUsage) || !strings.Contains(err.Error(), `the program "`+r.prog+`" cannot be held`) || !strings.Contains(err.Error(), r.says) {
			t.Errorf("walk(%q): %+v, %v; want ErrUsage naming it and %q", r.prog, w, err, r.says)
		}
	}
	fifo := tree([]string{"/held"}, nil, nil)
	fifo.modes["/held/fifo"] = fs.ModeNamedPipe | 0o600
	if _, err := walk(fifo, "/held/fifo", 40); !errors.Is(err, ErrUsage) || !strings.Contains(err.Error(), "it is not a regular file") {
		t.Errorf("walk of a FIFO: %v, want ErrUsage and that it is not a regular file", err)
	}
	// The limit is the system's: a chain of three links passes three and is refused at two.
	three := tree([]string{"/a"}, []string{"/a/f"}, map[string]string{"/a/1": "2", "/a/2": "3", "/a/3": "f"})
	if w, err := walk(three, "/a/1", 3); err != nil || w.file != "/a/f" {
		t.Errorf("a chain of three links with a limit of three: %+v, %v; want /a/f", w, err)
	}
	if _, err := walk(three, "/a/1", 2); !errors.Is(err, ErrUsage) || !strings.Contains(err.Error(), "more than 2 links") {
		t.Errorf("a chain of three links with a limit of two: %v, want ErrUsage", err)
	}
}

// TestRunHoldingHoldsTheProgramsRealFileAndOnMacOSEveryLinkOnItsPath: a program whose real file
// no folder held reaches is ErrUsage naming that file's folder, whatever folder its link lies in;
// one beneath a folder the caller named, or its loader's own folder, passes. With linksHeld,
// macOS's rule, a link on the path that no folder reaches is ErrUsage naming the link, though the
// real file is held; without it, Linux's rule, the same path passes. The folders are never added
// to.
//
// What it costs when this fails: on Linux a held program fails after the block is set, with an
// error a caller cannot tell from a program that failed; on macOS a program runs from a folder the
// caller never named, or fails after the exec with an exit a program could give too.
func TestRunHoldingHoldsTheProgramsRealFileAndOnMacOSEveryLinkOnItsPath(t *testing.T) {
	t.Parallel()
	n := named{readWrite: []string{"/rw"}, readOnly: []string{"/held"}}.with([]string{"/lib/load"})
	before := named{readWrite: append([]string(nil), n.readWrite...), readOnly: append([]string(nil), n.readOnly...)}
	for _, r := range []struct {
		name      string
		w         walked
		linksHeld bool
		says      string
	}{
		{"a file in a folder named", walked{file: "/held/real"}, true, ""},
		{"a file beneath a read-write folder", walked{file: "/rw/deep/x"}, true, ""},
		{"a file in its loader's own folder", walked{file: "/lib/load/ld.so"}, true, ""},
		{"a link in a folder named, to a file outside", walked{links: []string{"/held/t"}, file: "/other/true"}, false,
			`it is the file "/other/true", whose folder "/other" no folder held reaches`},
		{"the same on macOS", walked{links: []string{"/held/t"}, file: "/other/true"}, true,
			`it is the file "/other/true", whose folder "/other" no folder held reaches`},
		{"a file beside a folder named, by its name's prefix", walked{file: "/held2/x"}, false,
			`whose folder "/held2" no folder held reaches`},
		{"a link outside to a file held, on Linux", walked{links: []string{"/other/r"}, file: "/held/real"}, false, ""},
		{"a link outside to a file held, on macOS", walked{links: []string{"/other/r"}, file: "/held/real"}, true,
			`its path goes through the link "/other/r", whose folder "/other" no folder held reaches`},
		{"the root's /tmp on macOS", walked{links: []string{"/tmp"}, file: "/held/real"}, true,
			`the link "/tmp", whose folder "/" no folder held reaches`},
		{"the first of two links outside, on macOS", walked{links: []string{"/held/a", "/x/b", "/y/c"}, file: "/held/real"}, true,
			`the link "/x/b"`},
	} {
		err := n.heldProgram("/prog", r.w, r.linksHeld)
		switch {
		case r.says == "" && err != nil:
			t.Errorf("%s: %v, want it held", r.name, err)
		case r.says != "" && (!errors.Is(err, ErrUsage) || errors.Is(err, ErrNoBlock) || !strings.Contains(err.Error(), `the program "/prog" cannot be held`) || !strings.Contains(err.Error(), r.says)):
			t.Errorf("%s: %v, want ErrUsage, never ErrNoBlock, naming the program and %q", r.name, err, r.says)
		}
	}
	if !reflect.DeepEqual(n, before) {
		t.Errorf("the folders held changed to %+v, from %+v", n, before)
	}
}

// TestAProgramsOwnLibrariesNeverVouchForItsFolder: a program in a folder the caller did not name,
// with a library beside it by $ORIGIN in its run path or by the library's full path, is ErrUsage
// naming its folder, though the load search finds that folder, and nothing is added; named by the
// caller, it passes and the loader's folder is added. A program in its loader's own folder passes
// with no folder named. Linux's rule, over the load search's own fakes, on every system.
//
// What it costs when this fails: a bundled tool vouches for its own folder by its own bytes, and
// runs from a folder the caller never named, with the right to run every file in it.
func TestAProgramsOwnLibrariesNeverVouchForItsFolder(t *testing.T) {
	t.Parallel()
	const prog, gnu = "/opt/tool/prog", "/usr/lib/x86_64-linux-gnu"
	linux := programRule{walks: true, maxLinks: 40}
	caller := named{readWrite: []string{"/work"}}
	for _, r := range []struct {
		spelling string
		f        fake
	}{
		{"$ORIGIN", debian().file(prog, obj(glibcAMD, "libtool.so").run("$ORIGIN")).file("/opt/tool/libtool.so", obj("", "libc.so.6"))},
		{"a full path", debian().file(prog, obj(glibcAMD, "/opt/tool/libtool.so")).file("/opt/tool/libtool.so", obj("", "libc.so.6"))},
	} {
		dirs, err := loadFoldersOf(r.f, "amd64", prog, prog)
		if want := []string{gnu, "/opt/tool"}; err != nil || !reflect.DeepEqual(dirs, want) {
			t.Fatalf("%s: the load folders are %q, %v; want %q", r.spelling, dirs, err, want)
		}
		says := `it is the file "/opt/tool/prog", whose folder "/opt/tool" no folder held reaches`
		if n, err := caller.holdingProgram(prog, walked{file: prog}, linux, dirs); !errors.Is(err, ErrUsage) || !strings.Contains(err.Error(), says) || !reflect.DeepEqual(n, named{}) {
			t.Errorf("%s: by the caller's folders alone: %+v, %v; want nothing held and ErrUsage naming %q", r.spelling, n, err, says)
		}
		named := named{readOnly: []string{"/opt/tool"}}
		want := named.with([]string{gnu})
		if n, err := named.holdingProgram(prog, walked{file: prog}, linux, dirs); err != nil || !reflect.DeepEqual(n, want) {
			t.Errorf("%s: with its folder named: %+v, %v; want %+v", r.spelling, n, err, want)
		}
	}
	inLoader := gnu + "/tool"
	dirs, err := loadFoldersOf(debian().file(inLoader, obj(glibcAMD, "libc.so.6")), "amd64", inLoader, inLoader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := caller.holdingProgram(inLoader, walked{file: inLoader}, linux, dirs); err != nil {
		t.Errorf("a program in its loader's own folder: %v, want it held", err)
	}
}

// TestAScriptsInterpreterIsReadFromItsFirstLineAsMacOSReadsIt: a #! script's interpreter is the
// path after #! and any spaces and tabs, up to a space or tab, on a line that ends at a newline or
// a #, as the macOS kernel reads it. A Mach-O program of 64 or 32 bits whose filetype is
// MH_EXECUTE, a Mach-O file in the other byte order and a universal file need none. A file the
// kernel refuses with ENOEXEC is run by /bin/sh, as execvp runs it, with the reason: one with no
// #! that is no Mach-O program, a Mach-O library or object among them, and a Mach-O magic with
// nothing after it, as the kernel reads a short file padded with zeros, so 14 bytes ending in
// MH_EXECUTE's low byte are a program; and a #! line with no end in the first 512 bytes, or with
// no interpreter. A relative interpreter is an error.
//
// What it costs when this fails: RunHolding holds a file other than the one the kernel runs as the
// interpreter, and a script runs one outside the folders, or fails late with exit 71, or a file
// that runs is turned away.
func TestAScriptsInterpreterIsReadFromItsFirstLineAsMacOSReadsIt(t *testing.T) {
	t.Parallel()
	const neither, noEnd, noInterp = "is neither a #! script nor a Mach-O program", "has a #! line with no end within its first 512 bytes", "has a #! line that names no interpreter"
	for _, r := range []struct {
		head, interp, why, says string
	}{
		{"#!/bin/sh\necho", "/bin/sh", "", ""},
		{"#! \t/usr/bin/env python3 -u\n", "/usr/bin/env", "", ""},
		{"#!/held/run\targ\n", "/held/run", "", ""},
		{"#!/held/true#/x\n", "/held/true", "", ""},
		{"#!/held/run #" + strings.Repeat("x", 600), "/held/run", "", ""},
		{"\xcf\xfa\xed\xfe\x0c\x00\x00\x01\x00\x00\x00\x00\x02\x00\x00\x00 a 64-bit Mach-O program", "", "", ""},
		{"\xce\xfa\xed\xfe\x07\x00\x00\x00\x03\x00\x00\x00\x02\x00\x00\x00 a 32-bit Mach-O program", "", "", ""},
		{"\xcf\xfa\xed\xfe\x0c\x00\x00\x01\x00\x00\x00\x00\x02\x00", "", "", ""},
		{"\xcf\xfa\xed\xfe\x0c\x00\x00\x01\x00\x00\x00\x00\x06\x00\x00\x00 a 64-bit Mach-O library", "/bin/sh", neither, ""},
		{"\xce\xfa\xed\xfe\x07\x00\x00\x00\x03\x00\x00\x00\x01\x00\x00\x00 a 32-bit Mach-O object", "/bin/sh", neither, ""},
		{"\xcf\xfa\xed\xfe a 64-bit Mach-O magic and text", "/bin/sh", neither, ""},
		{"\xcf\xfa\xed\xfe", "/bin/sh", neither, ""},
		{"\xfe\xed\xfa\xcf a Mach-O file in the other byte order", "", "", ""},
		{"\xca\xfe\xba\xbe a universal file", "", "", ""},
		{"exit 0\n", "/bin/sh", neither, ""},
		{"", "/bin/sh", neither, ""},
		{" #!/bin/sh\n", "/bin/sh", neither, ""},
		{"#!/bin/sh", "/bin/sh", noEnd, ""},
		{"#!/bin/sh " + strings.Repeat("x", 600) + "\n", "/bin/sh", noEnd, ""},
		{"#!\n", "/bin/sh", noInterp, ""},
		{"#! \t\n", "/bin/sh", noInterp, ""},
		{"#! #/bin/sh\n", "/bin/sh", noInterp, ""},
		{"#!sh\n", "", "", `names "sh", which is not an absolute path`},
	} {
		head := []byte(r.head)[:min(len(r.head), maxShebang)]
		got, why, err := interpreter(head)
		switch {
		case r.says == "" && (err != nil || got != r.interp || !strings.HasPrefix(why, r.why) || (why == "") != (r.why == "")):
			t.Errorf("%q: %q, %q, %v; want %q and a reason starting %q", r.head, got, why, err, r.interp, r.why)
		case r.says != "" && (err == nil || !strings.Contains(err.Error(), r.says)):
			t.Errorf("%q: %q, %v; want an error saying %q", r.head, got, err, r.says)
		}
	}
}

// universalLimit is the line of docs/sandbox.md that names, among what the block does not close,
// the universal file whose slices RunHolding does not read.
const universalLimit = "On macOS a universal file is taken for a program by its header, and its slices are not read."

// namesTheUniversalLimit is whether doc names universalLimit below its "What the block does not
// close" heading, read with each line's breaks and indent as one space.
func namesTheUniversalLimit(doc []byte) bool {
	_, limits, ok := strings.Cut(string(doc), "\n## What the block does not close\n")
	return ok && strings.Contains(strings.Join(strings.Fields(limits), " "), universalLimit)
}

// TestTheSandboxDocNamesTheUniversalFileWhoseSlicesAreNotRead: docs/sandbox.md names, among what
// the block does not close, that a universal file is taken for a program by its header, which
// TestOnMacOSAFileTheKernelRefusesWithENOEXECIsHeldToTheShell holds as it is. The control: the doc
// with the line above the heading, or with no heading, is found not to name it.
//
// What it costs when this fails: a caller reads that every file the kernel refuses with ENOEXEC
// is refused up front, and a universal library fails after the exec with exit 71.
func TestTheSandboxDocNamesTheUniversalFileWhoseSlicesAreNotRead(t *testing.T) {
	t.Parallel()
	doc, err := os.ReadFile("../docs/sandbox.md")
	if err != nil {
		t.Fatal(err)
	}
	if !namesTheUniversalLimit(doc) {
		t.Errorf("docs/sandbox.md does not name %q below its \"What the block does not close\" heading", universalLimit)
	}
	for _, planted := range []string{
		universalLimit + "\n\n## What the block does not close\n\n  - Nothing.\n",
		"## What the block opens\n\n  - " + universalLimit + "\n",
	} {
		if namesTheUniversalLimit([]byte("# sandbox\n\n" + planted)) {
			t.Errorf("a doc with only %q is found to name the limit", planted)
		}
	}
}
