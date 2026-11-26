//go:build linux || darwin

package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// copyProgram copies this system's true, by its real file, to the folder dir as true, and returns
// the copy's path.
func copyProgram(t *testing.T, dir string) string {
	t.Helper()
	src, err := filepath.EvalSymlinks("/usr/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "true")
	if err := os.WriteFile(dst, b, 0o700); err != nil {
		t.Fatal(err)
	}
	return dst
}

// TestAProgramWhoseRealFileIsNotHeldIsRefusedBeforeTheExec: a link in the folder RunHolding is
// given, to a copy of true in a folder it is not, is ErrUsage from RunHolding before anything is
// set and from Check, naming the copy's folder, on Linux and macOS alike; the copy named by its
// own path, with its own folder held, runs and exits 0. On macOS a link outside the folders, to a
// file held, is ErrUsage from Check naming the link, where the system sandbox would refuse it
// after the exec with its exit 71; on Linux, whose kernel follows it, Check takes it.
//
// What it costs when this fails: on Linux the program fails after the block is set, with an error
// a caller cannot tell from the program's own; on macOS it runs from a folder the caller never
// named, or exits 71 from the system sandbox, an exit a program could give too.
func TestAProgramWhoseRealFileIsNotHeldIsRefusedBeforeTheExec(t *testing.T) {
	t.Parallel()
	dir := realTemp(t, "held", "other")
	held, other := filepath.Join(dir, "held"), filepath.Join(dir, "other")
	real := copyProgram(t, other)
	link := filepath.Join(held, "t")
	if err := os.Symlink(filepath.Join("..", "other", "true"), link); err != nil {
		t.Fatal(err)
	}
	says := `it is the file "` + real + `", whose folder "` + other + `" no folder held reaches`
	if _, err := Check(Folders{ReadOnly: []string{held}}, link); !errors.Is(err, ErrUsage) || errors.Is(err, ErrNoBlock) || err == nil || !strings.Contains(err.Error(), says) {
		t.Errorf("Check of a link to a file outside the folders: %v, want ErrUsage, never ErrNoBlock, and %q", err, says)
	}
	if code, said := behindTheBlock(t, link); code != childUsage || !strings.Contains(said, says) {
		t.Errorf("RunHolding of a link to a file outside the folders: exit %d, said %q; want exit %d and %q", code, said, childUsage, says)
	}
	if code, said := behindTheBlock(t, real); code != 0 {
		t.Errorf("RunHolding of the copy by its own path: exit %d, said %q; want 0", code, said)
	}

	// A link in other, which no folder holds, to a copy of true in held.
	into := filepath.Join(other, "into")
	if err := os.Symlink(copyProgram(t, held), into); err != nil {
		t.Fatal(err)
	}
	_, err := Check(Folders{ReadOnly: []string{held}}, into)
	if programHere().linksHeld {
		says := `its path goes through the link "` + into + `", whose folder "` + other + `" no folder held reaches`
		if !errors.Is(err, ErrUsage) || err == nil || !strings.Contains(err.Error(), says) {
			t.Errorf("Check on macOS of a link outside the folders to a file held: %v, want ErrUsage and %q", err, says)
		}
	} else if errors.Is(err, ErrUsage) {
		t.Errorf("Check on Linux of a link outside the folders to a file held: %v, want no ErrUsage, as Landlock follows it", err)
	}
}
