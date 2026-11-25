//go:build linux || darwin || windows

package proc

import (
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
)

// storeNil stores nil in *v, as any package of the binary can store it in an exported var, until t
// ends, and gives the value *v held.
func storeNil(t *testing.T, v *error) error {
	t.Helper()
	was := *v
	*v = nil
	t.Cleanup(func() { *v = was })
	return was
}

// TestANilStoredInErrPathElementRefusesStill: with nil stored in ErrPathElement, PathList still
// refuses a folder that holds the separator, joins nothing, and wraps the value ErrPathElement was
// built with; once it holds that value again, errors.Is matches the refusal by it.
//
// What it costs when this fails: `proc.ErrPathElement = nil` anywhere in the binary makes a refusal
// nil, and the plain join of a folder that splits in two goes to the child.
func TestANilStoredInErrPathElementRefusesStill(t *testing.T) {
	// serial: stores nil in ErrPathElement, which the package's other tests read
	built := storeNil(t, &ErrPathElement)
	got, err := PathList("linux", "/a", "/b:c")
	if got != "" || !errors.Is(err, built) {
		t.Errorf("PathList of a folder holding : with nil in ErrPathElement: %q, %v, want nothing joined and ErrPathElement as built", got, err)
	}
	if ErrPathElement = built; !errors.Is(err, ErrPathElement) {
		t.Errorf("the refusal %v does not match ErrPathElement once it holds its value again", err)
	}
}

// TestANilStoredInErrStaleOrErrRefusedRefusesStill: with nil stored in ErrStale and in ErrRefused,
// a stop of a tree whose leader was reaped is still refused with the value ErrStale was built with
// and signals nothing, as Usage and Pids of it are refused on Windows, and a pty's name query that
// gave a refusal is still that refusal, not one Stdin's kind could not be told by. Once ErrStale
// holds its value again, errors.Is matches the stop's refusal by it.
//
// What it costs when this fails: `proc.ErrStale = nil` anywhere in the binary makes the leader's
// check give nil, and Stop signals whatever took the reaped leader's pid.
func TestANilStoredInErrStaleOrErrRefusedRefusesStill(t *testing.T) {
	// serial: stores nil in ErrStale and ErrRefused, which the package's other tests read
	stale, refused := storeNil(t, &ErrStale), storeNil(t, &ErrRefused)
	tree, r := treeHelper(t, "exit")
	got := lines(t, r, 2)
	gc, err := strconv.Atoi(got["grandchild"])
	if err != nil {
		t.Fatalf("the leader wrote %q for its grandchild's pid: %v", got["grandchild"], err)
	}
	if state, err := tree.Leader.Wait(); err != nil || !state.Success() {
		t.Fatalf("the leader, which exits 0: %v, %v", state, err)
	}
	var calls atomic.Int32
	stop := stopTree(tree.ID, counted(&calls))
	if !errors.Is(stop, stale) || calls.Load() != 0 {
		t.Errorf("stopTree(%q) with its leader reaped and nil in ErrStale: %v after %d signals, want ErrStale as built and none", tree.ID, stop, calls.Load())
	}
	// Where a tree is read, on Windows, Usage and Pids of it are ErrStale as built; elsewhere each is
	// errors.ErrUnsupported. One of the two, never both and never neither.
	_, usage := Usage(tree.ID)
	_, pids := Pids(tree.ID)
	for what, err := range map[string]error{"Usage": usage, "Pids": pids} {
		if errors.Is(err, stale) == errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("%s(%q) with nil in ErrStale: %v, want ErrStale as built, or errors.ErrUnsupported off Windows", what, tree.ID, err)
		}
	}
	if ok, err := ptyAnswer(nil, true, func() (uint32, []uint16, error) { return 0, nil, errTimeout }); ok || err != errTimeout { //nolint:errorlint // the refusal itself, not one that wraps it
		t.Errorf("ptyAnswer of a name query refused with nil in ErrRefused: %v, %v, want false and the refusal as it came", ok, err)
	}
	if ErrStale, ErrRefused = stale, refused; !errors.Is(stop, ErrStale) {
		t.Errorf("stopTree's refusal %v does not match ErrStale once it holds its value again", stop)
	}
	stillThere(t, tree, gc)
	drained(t, "the grandchild, once ended", r)
}
