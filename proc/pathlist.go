package proc

import (
	"errors"
	"fmt"
	"strings"

	"github.com/birdtable/go-proc/internal/homevars"
)

// ErrPathElement is a folder PathList or LocalPathList refuses, since it cannot be one element of a
// PATH value on the system named: each refusal wraps it, and nothing is joined.
var ErrPathElement = errors.New("proc: the folder cannot be one element of a PATH value on this system, and nothing was joined")

// errPathElement is what each refusal wraps: the value ErrPathElement was given here, so a value a
// caller or a dependency stores in ErrPathElement later, nil included, changes what a match finds
// and never lets a folder through.
var errPathElement = ErrPathElement

// PathList is the value of PATH that names dirs, in order, to a child on the system goos, named as
// runtime.GOOS names one and read as FileName reads it: on windows the folders are joined with ;,
// and on every other system with :. On the system named, filepath.SplitList of the value gives back
// dirs, element for element, and so does a reader that splits the value on the separator alone. No
// folders is the empty value and no error.
//
// A folder that could not be read back so is refused, with an error that wraps ErrPathElement and
// names it, by %q, and its place in dirs, and nothing is joined: on every system an empty name, which
// a reader takes for the working folder, and a name holding a NUL; on every system but windows a name
// holding :, which PATH has no escape for there; on windows a name holding ; or ". No quotes are
// written: filepath.SplitList on Windows reads a quoted element as one folder, but a reader that
// splits on ; alone reads "C:\a;b" as two, the second a relative folder.
//
// Each folder is written as given: nothing is cleaned, made absolute or dropped as a repeat, and a
// relative folder is not refused. Which folders belong on the list is the caller's to filter. goos is
// an argument, so a caller tests every system's value on any host; LocalPathList is this system's.
func PathList(goos string, dirs ...string) (string, error) {
	sep, refused, whyRefused := ":", ":", "holds :, the separator, which PATH has no escape for here"
	if goos == "windows" {
		sep, refused, whyRefused = ";", `;"`, `holds ; or ", which readers of PATH on windows split or unquote apart`
	}
	for i, dir := range dirs {
		var why string
		switch {
		case dir == "":
			why = "is empty, which a reader takes for the working folder"
		case strings.ContainsRune(dir, 0):
			why = "holds a NUL"
		case strings.ContainsAny(dir, refused):
			why = whyRefused
		default:
			continue
		}
		return "", fmt.Errorf("%w: dirs[%d], %q, %s", errPathElement, i, dir, why)
	}
	return strings.Join(dirs, sep), nil
}

// LocalPathList is the value of PATH that names dirs, in order, to a child on the system this process
// runs on, as PathList gives it for that system, refusing what PathList refuses there. Code that
// names every system's value, or tests one system's on another, calls PathList.
func LocalPathList(dirs ...string) (string, error) {
	return PathList(homevars.Here, dirs...)
}
