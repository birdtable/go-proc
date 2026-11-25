//go:build linux

package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// restrictAlone runs linuxKernel's restrict on a thread of its own, locked
// and never unlocked, so a thread a plant restricts ends with its goroutine
// and no other test runs on it.
func restrictAlone(handledFS, scoped uint64, rules []pathRule) error {
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		done <- linuxKernel{}.restrict(handledFS, scoped, rules)
	}()
	return <-done
}

// TestARuleNeverFollowsALinkToItsFolder: restrict opens each rule's folder
// itself, never following a link, before go-landlock opens it by its
// descriptor, so a link where a folder was checked is refused with ENOTDIR,
// naming it, and no ruleset is made. The control is the folder itself, which
// opens, and reaches go-landlock.
//
// What it costs when this fails: a folder swapped for a link after RunHolding checked it is granted, and
// the program writes wherever the link points.
func TestARuleNeverFollowsALinkToItsFolder(t *testing.T) {
	t.Parallel()
	dir := realTemp(t, "real", "elsewhere")
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "elsewhere"), link); err != nil {
		t.Fatal(err)
	}
	err := restrictAlone(landlockAccessFSABI6, 0, []pathRule{{link, landlockAccessFSABI6}})
	if !errors.Is(err, syscall.ENOTDIR) {
		t.Errorf("a rule whose folder is a link: %v, want ENOTDIR, the link not followed", err)
	}
	var perr *os.PathError
	if !errors.As(err, &perr) || perr.Path != link {
		t.Errorf("a rule whose folder is a link: %v, want the link named", err)
	}
	// The control: the folder opens and its rule reaches go-landlock. With
	// no_new_privs unset on this thread the kernel then refuses to restrict
	// it, so nothing is restricted either way.
	if err := restrictAlone(landlockAccessFSABI6, 0, []pathRule{{filepath.Join(dir, "real"), landlockAccessFSABI6}}); errors.Is(err, syscall.ENOTDIR) {
		t.Errorf("the control, a real folder: %v", err)
	}
}

// TestARuleNeverFollowsALinkInAFolderAboveItsFolder: RunHolding checks a
// folder by its real path, then a folder above it is moved aside and a link
// to another folder that ends in the same names put in its place, and only
// then does restrict open the rules. The open follows no link in any name of
// the path, so it is refused with ELOOP, naming the folder, and no ruleset is
// made. The control is the same rule before the swap, which opens.
//
// What it costs when this fails: a folder above the one RunHolding checked, swapped for a link, makes the
// rule grant the folder at the link's target, which the program then writes with the caller's rights.
func TestARuleNeverFollowsALinkInAFolderAboveItsFolder(t *testing.T) {
	t.Parallel()
	dir := realTemp(t, "p/out", "q/out")
	p, out := filepath.Join(dir, "p"), filepath.Join(dir, "p", "out")
	n, err := holding(Folders{ReadWrite: []string{out}})
	if err != nil {
		t.Fatal(err)
	}
	// The control: before the swap, the checked folder opens.
	held, err := openRules(n.rules())
	for _, r := range held {
		_ = syscall.Close(r.fd)
	}
	if err != nil {
		t.Fatalf("the control, the checked folder before the swap: %v", err)
	}
	// The swap: the folder above moves aside, and a link to q takes its path.
	if err := os.Rename(p, filepath.Join(dir, "p-moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "q"), p); err != nil {
		t.Fatal(err)
	}
	err = restrictAlone(landlockAccessFSABI6, 0, n.rules())
	if !errors.Is(err, syscall.ELOOP) {
		t.Errorf("a rule whose folder above is now a link: %v, want ELOOP, the link not followed", err)
	}
	var perr *os.PathError
	if !errors.As(err, &perr) || perr.Path != out {
		t.Errorf("a rule whose folder above is now a link: %v, want the checked folder %s named", err, out)
	}
}

// TestNoRulesetFromGoLandlockIsARefusal: go-landlock gives a descriptor below
// zero, with no error, when there is nothing to enforce; restrict takes that
// as no ruleset, never as a ruleset set.
//
// What it costs when this fails: a block whose ruleset go-landlock did not make runs the program with
// nothing restricting it, and the probe alone stands between it and the files.
func TestNoRulesetFromGoLandlockIsARefusal(t *testing.T) {
	t.Parallel()
	if err := restrictAlone(0, 0, nil); !errors.Is(err, errNoRuleset) {
		t.Errorf("a ruleset that handles nothing: %v, want errNoRuleset", err)
	}
}

// created makes the file p to write, closed at exec, and closes it.
func created(p string) error {
	fd, err := syscall.Open(p, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	return syscall.Close(fd)
}

// TestARuleGrantsTheFolderOpenedEvenIfALinkTookItsPlace: a rule's folder is
// opened, then moved aside and a link to another folder put at its path, and
// only then is the ruleset made from the open descriptor and this thread
// restricted to it, with no_new_privs set, as the block sets it. A write
// beneath the folder that was opened goes through, at its new path; a write
// in the folder the link points to is refused with EACCES. go-landlock is
// given the descriptor, never the path, so the swap grants nothing.
//
// What it costs when this fails: a folder swapped for a link between RunHolding's check and the ruleset
// is granted by its path, and the program writes wherever the link points.
func TestARuleGrantsTheFolderOpenedEvenIfALinkTookItsPlace(t *testing.T) {
	t.Parallel()
	dir := realTemp(t, "a", "b")
	a, b, moved := filepath.Join(dir, "a"), filepath.Join(dir, "b"), filepath.Join(dir, "a-moved")
	held, err := openRules([]pathRule{{a, landlockAccessFSABI6}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, r := range held {
			_ = syscall.Close(r.fd)
		}
	})
	// The swap: the opened folder moves aside, and a link to b takes its path.
	if err := os.Rename(a, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(b, a); err != nil {
		t.Fatal(err)
	}
	type result struct{ restrict, inOpened, inLinked error }
	done := make(chan result, 1)
	go func() {
		// Never unlocked: the thread ends restricted, with its goroutine.
		runtime.LockOSThread()
		var r result
		if _, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0, 0); errno != 0 {
			r.restrict = errno
			done <- r
			return
		}
		if r.restrict = restrictHeld(landlockAccessFSABI6, 0, held); r.restrict == nil {
			r.inOpened = created(filepath.Join(moved, "f"))
			r.inLinked = created(filepath.Join(b, "f"))
		}
		done <- r
	}()
	r := <-done
	if r.restrict != nil {
		t.Fatalf("restricting to the opened folder: %v", r.restrict)
	}
	if r.inOpened != nil {
		t.Errorf("a write beneath the folder that was opened, now at %s: %v, want it through", moved, r.inOpened)
	}
	if !errors.Is(r.inLinked, syscall.EACCES) {
		t.Errorf("a write in %s, where the link at the rule's path points: %v, want EACCES", b, r.inLinked)
	}
	if _, err := os.Stat(filepath.Join(b, "f")); err == nil {
		t.Errorf("a file was made in %s, which no rule grants", b)
	}
}
