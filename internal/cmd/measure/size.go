package main

import (
	"bytes"
	"io/fs"
	"strings"
)

// Ceiling is the most non-test Go lines the module may hold, as Size counts them. It is a measured
// count: the figure Size prints for the tree at the head of the change that last raised it, with no
// headroom. A change that takes the total past it lands only once a cut has paid for it, or once the
// change itself raises Ceiling to its measured count, in a commit that says what the added lines
// are for. The module's size is held there so a pull request past the ceiling fails at its own head,
// not at the tag.
const Ceiling = 11669

// Count is the module's size in its two parts: testenv, and the runtime packages, which are every
// other package Size counts (today proc, sandbox with cmd/netblock, service, load, gitenv and
// gitenv/gittest). testenv left the module, so its part is 0 unless a folder of that name comes
// back. Ceiling holds the total alone.
type Count struct {
	Runtime int
	Testenv int
}

// Total is the lines of both parts, the figure Ceiling holds.
func (c Count) Total() int { return c.Runtime + c.Testenv }

// testenvDir is the folder whose lines Count gives as Testenv.
const testenvDir = "testenv"

// Size counts the non-test Go lines of the module's packages the way `wc -l` of their files
// does, one per newline: every .go file not named _test.go, outside internal/, testdata and
// any folder whose name starts with a dot. A file beneath testenv/ counts to Testenv, any other
// to Runtime.
func Size(fsys fs.FS) (Count, error) {
	var c Count
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipped(name) || nested(fsys, name) {
				return fs.SkipDir
			}
			return nil
		}
		if !source(name) {
			return nil
		}
		b, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		n := bytes.Count(b, []byte("\n"))
		if strings.HasPrefix(name, testenvDir+"/") {
			c.Testenv += n
		} else {
			c.Runtime += n
		}
		return nil
	})
	return c, err
}

// nested says whether the folder name, a path from the module's root, holds a go.mod of its own: it is
// another module, as tools/gate is, and none of its lines are this module's.
func nested(fsys fs.FS, name string) bool {
	if name == "." {
		return false
	}
	_, err := fs.Stat(fsys, name+"/go.mod")
	return err == nil
}

// skipped says whether the folder name, a path from the module's root, holds no package of the
// module's surface: internal/, testdata, and a folder whose name starts with a dot.
func skipped(name string) bool {
	if name == "." {
		return false
	}
	base := name[strings.LastIndex(name, "/")+1:]
	return name == "internal" || base == "testdata" || strings.HasPrefix(base, ".") || strings.HasPrefix(base, "_")
}

// source says whether the file name is a Go file a build of the package reads.
func source(name string) bool {
	base := name[strings.LastIndex(name, "/")+1:]
	return strings.HasSuffix(base, ".go") && !strings.HasSuffix(base, "_test.go") &&
		!strings.HasPrefix(base, ".") && !strings.HasPrefix(base, "_")
}
