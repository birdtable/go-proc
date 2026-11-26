package sandbox

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"syscall"
)

// The program RunHolding runs is a file like any other, held to the folders: its real file, the
// path with every link in it followed, is beneath a folder the caller named or its loader's own
// folder, or RunHolding refuses it with ErrUsage before anything is set, naming that file's folder,
// and never adds the folder. The other load folders come from the program's own bytes, its run
// path and the libraries it names, and never count toward holding it: a program with $ORIGIN in
// its run path would vouch for its own folder.
//
// The kernel holds every exec too, and the walk says before the exec what the kernel would refuse
// after it. On Linux Landlock checks the right to run the file the exec opens, the real file, and
// no link on the way, as it has no right for a lookup. On macOS the profile denies every exec but
// of a file beneath the folders, and the system sandbox refuses to follow a link outside them, the
// root's /tmp and /var among them, and to start a #! script's interpreter outside them: each fails
// after RunHolding has exec'd the sandbox, with its exit 71, which a program could give too, and a
// link changed after the walk fails there, closed. So on macOS RunHolding refuses such a link
// first, naming it, and reads a #! script's first line and holds the interpreter it names as it
// holds a program, with no lookup: a program env finds is held when env runs it. A file that is
// neither a #! script nor a Mach-O program the kernel refuses with ENOEXEC, and sandbox-exec's
// execvp runs it with /bin/sh instead, so RunHolding holds /bin/sh for it the same way.

// walked is a program's path as the kernel follows it: each link it goes through, by its own
// path, in order, and file, the real file it ends on.
type walked struct {
	links []string
	file  string
}

// linkFiles is what the walk reads of the files, a seam its rows run by on every system: a file's
// mode, without following a link, and the target of a link.
type linkFiles interface {
	lstat(name string) (fs.FileMode, error)
	readlink(name string) (string, error)
}

// osLinks is this system's files, as the walk reads them.
type osLinks struct{}

func (osLinks) lstat(name string) (fs.FileMode, error) {
	st, err := os.Lstat(name)
	if err != nil {
		return 0, err
	}
	return st.Mode(), nil
}

func (osLinks) readlink(name string) (string, error) { return os.Readlink(name) }

// heldf is ErrUsage for the program prog, which the folders do not hold, and why.
func heldf(prog, why string, args ...any) error {
	return fmt.Errorf("the program %q cannot be held: %s: %w", prog, fmt.Sprintf(why, args...), errUsage)
}

// walk follows prog, an absolute path, as the kernel does: one name at a time from the root, a
// link's target read and put in its place, from the root again when it is absolute, and ".."
// taken in the folder the walk has reached, never by cleaning the text, so a ".." after a link
// leaves the link's target and not its name. It refuses past maxLinks links, as the kernel does
// with ELOOP, a name that is not there, a name after a file that is no folder, and a path that
// does not end on a regular file. Every refusal is ErrUsage, naming prog.
func walk(files linkFiles, prog string, maxLinks int) (walked, error) {
	var w walked
	cur, mode := "/", fs.ModeDir
	rest := strings.Split(strings.TrimPrefix(prog, "/"), "/")
	for len(rest) > 0 {
		name := rest[0]
		rest = rest[1:]
		switch {
		case !mode.IsDir():
			return walked{}, heldf(prog, "it cannot be found: %q is not a folder", cur)
		case name == "" || name == ".":
			continue
		case name == "..":
			cur = path.Dir(cur)
			continue
		}
		next := path.Join(cur, name)
		m, err := files.lstat(next)
		if err != nil {
			return walked{}, heldf(prog, "it cannot be found: %v", err)
		}
		if m&fs.ModeSymlink == 0 {
			cur, mode = next, m
			continue
		}
		if len(w.links) == maxLinks {
			return walked{}, heldf(prog, "it cannot be found: its path goes through more than %d links", maxLinks)
		}
		to, err := files.readlink(next)
		if err == nil && to == "" {
			err = errors.New("its target is empty")
		}
		if err != nil {
			return walked{}, heldf(prog, "it cannot be found: the link %q cannot be read: %v", next, err)
		}
		w.links = append(w.links, next)
		if path.IsAbs(to) {
			cur = "/"
		}
		rest = append(strings.Split(to, "/"), rest...)
	}
	if !mode.IsRegular() {
		return walked{}, heldf(prog, "it is not a regular file: it is the file %q", cur)
	}
	w.file = cur
	return w, nil
}

// heldProgram is nil when n holds the program prog, walked to w: its real file is beneath one of
// n's folders, and with linksHeld every link on its path is too. Otherwise it is ErrUsage naming
// the file or the first link no folder reaches, and that one's folder.
func (n named) heldProgram(prog string, w walked, linksHeld bool) error {
	reaches := func(p string) bool {
		for _, d := range slices.Concat(n.readWrite, n.readOnly) {
			if beneathSlash(p, d) {
				return true
			}
		}
		return false
	}
	if linksHeld {
		for _, l := range w.links {
			if !reaches(l) {
				return heldf(prog, "its path goes through the link %q, whose folder %q no folder held reaches, and the system sandbox refuses to follow a link there: name the program by its real file %q, or hold that folder", l, path.Dir(l), w.file)
			}
		}
	}
	if !reaches(w.file) {
		return heldf(prog, "it is the file %q, whose folder %q no folder held reaches: name it, or a folder above it, in Folders", w.file, path.Dir(w.file))
	}
	return nil
}

// programRule is how a system's block holds the program: walks, whether RunHolding walks its path
// and holds it at all, where a system with no block refuses anyway; maxLinks, the kernel's limit
// of links in one path; linksHeld, whether every link on the path must be beneath a folder; and
// interpreters, whether a #! script's interpreter is held as the program is.
type programRule struct {
	walks        bool
	maxLinks     int
	linksHeld    bool
	interpreters bool
}

// program is n with the program at prog's load folders added, once its path is walked and n is
// found to hold it as this system's rule says, or ErrUsage, before anything is set.
func program(n named, prog string) (named, error) {
	rule := programHere()
	w := walked{file: prog}
	if rule.walks {
		var err error
		if w, err = walk(osLinks{}, prog, rule.maxLinks); err != nil {
			return named{}, err
		}
	}
	dirs, err := loadHere(prog, w.file)
	if err != nil {
		return named{}, err
	}
	if n, err = n.holdingProgram(prog, w, rule, dirs); err != nil || !rule.interpreters {
		return n, err
	}
	return n, n.heldInterpreter(prog, w.file, rule)
}

// holdingProgram is n with the load folders dirs added, once n holds the program prog, walked to w,
// as rule says, by the caller's folders and the first of dirs alone, the loader's own folder. The
// rest come from the program's own bytes, its run path and the libraries it names, and never count
// toward holding it: a program with $ORIGIN in its run path, or a library beside it by its full
// path, would vouch for its own folder.
func (n named) holdingProgram(prog string, w walked, rule programRule, dirs []string) (named, error) {
	if rule.walks {
		if err := n.with(dirs[:min(len(dirs), 1)]).heldProgram(prog, w, rule.linksHeld); err != nil {
			return named{}, err
		}
	}
	return n.with(dirs), nil
}

// maxShebang is the bytes of a file's start in which the macOS kernel looks for a #! line's end,
// IMG_SHSIZE.
const maxShebang = 512

// heldInterpreter is nil when the program prog, whose real file is file, is a Mach-O program, or
// n holds the program that runs it as rule holds a program: a #! script's interpreter, or /bin/sh
// for a file that is neither. Otherwise it is ErrUsage naming that program, or why it cannot be
// told. It reads the file's first bytes alone and looks nothing up: a program that env finds is
// held when env runs it, by the system sandbox.
func (n named) heldInterpreter(prog, file string, rule programRule) error {
	head, err := firstBytes(file)
	if err != nil {
		return heldf(prog, "its first line cannot be read: %v", err)
	}
	interp, err := interpreter(head)
	switch {
	case err != nil:
		return heldf(prog, "%v", err)
	case interp == "":
		return nil
	}
	w, err := walk(osLinks{}, interp, rule.maxLinks)
	if err == nil {
		err = n.heldProgram(interp, w, rule.linksHeld)
	}
	switch {
	case err == nil:
		return nil
	case bytes.HasPrefix(head, []byte("#!")):
		return fmt.Errorf("the program %q is a #! script, and its interpreter is held as a program is: %w", prog, err)
	}
	return fmt.Errorf("the program %q is neither a #! script nor a Mach-O program, so it is run by %s, which is held as a program is: %w", prog, bourneShell, err)
}

// bourneShell is the program execvp runs a file with when the kernel refuses it with ENOEXEC, as
// it refuses a file that is neither a #! script nor a Mach-O program: _PATH_BSHELL.
const bourneShell = "/bin/sh"

// machO is whether a file starting with head is one the macOS kernel runs, or refuses, by its own
// format, never with ENOEXEC: a Mach-O file of 32 or 64 bits in either byte order, the other one
// refused with EBADARCH, or a universal file, whose magic is big-endian.
func machO(head []byte) bool {
	for _, magic := range [][]byte{{0xce, 0xfa, 0xed, 0xfe}, {0xcf, 0xfa, 0xed, 0xfe}, {0xfe, 0xed, 0xfa, 0xce}, {0xfe, 0xed, 0xfa, 0xcf}, {0xca, 0xfe, 0xba, 0xbe}} {
		if bytes.HasPrefix(head, magic) {
			return true
		}
	}
	return false
}

// firstBytes is the first maxShebang bytes of the file at name, or all of a shorter one. It opens
// the file without waiting, so a FIFO put there since the walk holds nothing up, and reads it only
// when it is a regular file.
func firstBytes(name string) (head []byte, err error) {
	f, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // G304: the program its caller names is what this reads
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return nil, errors.Join(errors.New("it is not a regular file"), err)
	}
	head = make([]byte, maxShebang)
	got, err := io.ReadFull(f, head)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		err = nil
	}
	return head[:got], err
}

// interpreter is the program that runs a file starting with head, as the macOS kernel and execvp
// tell it. A #! script's is the interpreter on its #! line, read as the kernel reads it: the line
// ends at the first newline or #, and the interpreter is after the #! and any spaces and tabs, up
// to the next space or tab. A Mach-O program needs none, "". Any other file, one with no #!, is
// run by bourneShell. It is an error for a #! line with no end within maxShebang bytes, no
// interpreter, or one that is not an absolute path.
func interpreter(head []byte) (string, error) {
	rest, ok := bytes.CutPrefix(head, []byte("#!"))
	switch {
	case !ok && machO(head):
		return "", nil
	case !ok:
		return bourneShell, nil
	}
	end := bytes.IndexAny(rest, "#\n")
	if end < 0 {
		return "", fmt.Errorf("its #! line has no end within its first %d bytes", maxShebang)
	}
	line := bytes.TrimLeft(rest[:end], " \t")
	if at := bytes.IndexAny(line, " \t"); at >= 0 {
		line = line[:at]
	}
	if interp := string(line); path.IsAbs(interp) {
		return interp, nil
	}
	return "", fmt.Errorf("its #! line names %q, which is not an absolute path", line)
}
