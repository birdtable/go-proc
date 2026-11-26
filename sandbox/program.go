package sandbox

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
)

// The program RunHolding runs is a file like any other, held to the folders: its real file, the
// path with every link in it followed, is beneath a folder the block holds, or RunHolding refuses
// it with ErrUsage before anything is set, naming that file's folder, and never adds the folder.
//
// On Linux the kernel holds it too: Landlock checks the right to run the file the exec opens, the
// real file, and no link on the way, as it has no right for a lookup. A program whose real file it
// does not reach fails there after the block is set, and RunHolding refuses it first.
//
// On macOS the system sandbox does not hold it: on macOS 27.2, behind the block's own profile, a
// program in a folder no parameter names starts, and only what it reads is held. So there the
// hold is RunHolding's own check, made once, by the walk below, before the exec. A link on the
// path changed after the walk, to a file outside the folders, runs that file, still behind the
// block and with its reads and writes held: that change is the work of a writer of a folder on the
// path, the caller's judgment, as a program or a library swapped after it was read is. The system
// sandbox does hold every link on the path: it refuses to follow one outside the folders, the
// root's /tmp and /var among them, and the exec fails after RunHolding has exec'd the sandbox,
// with its exit 71, which a program could give too. RunHolding refuses such a link first, naming it.

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
// of links in one path; and linksHeld, whether every link on the path must be beneath a folder.
type programRule struct {
	walks     bool
	maxLinks  int
	linksHeld bool
}

// program is n with the program at prog's load folders added, once its path is walked and n is
// found to hold it as this system's rule says, or ErrUsage, before anything is set.
func program(n named, prog string) (named, error) {
	rule := programHere()
	if !rule.walks {
		return loading(n, prog, prog)
	}
	w, err := walk(osLinks{}, prog, rule.maxLinks)
	if err != nil {
		return named{}, err
	}
	if n, err = loading(n, prog, w.file); err != nil {
		return named{}, err
	}
	if err := n.heldProgram(prog, w, rule.linksHeld); err != nil {
		return named{}, err
	}
	return n, nil
}
