package sandbox

import (
	"errors"
	"io/fs"
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
// one beneath a folder the caller named, or a load folder RunHolding added, passes. With linksHeld,
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
		{"a file in a load folder added", walked{file: "/lib/load/ld.so"}, true, ""},
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
