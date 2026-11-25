package proc

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// answer is one Holds call a fake gives: the kind and the refusal.
type answer struct {
	kind PeakKind
	err  error
}

// TestHoldsTreeMemoryAnswersFromItsTwoHoldsCalls: with Holds' two answers given, HoldsTreeMemory is
// TreeMemoryHeld and the first's kind where the first, of c with ReadPeak set, gives no refusal;
// TreeMemoryJudgedAfter and the second's kind where the first wraps ErrNotHeld and the second, of c
// with TreeMemory 0 and ReadPeak set, gives no refusal and a kind other than PeakNone; and
// TreeMemoryRefused, PeakNone and no error where the second refuses, whether its refusal wraps
// ErrNotHeld or not, or gives PeakNone. A first refusal that does not wrap ErrNotHeld, the caller's
// errTimeout or errNoTree, is given as it is with TreeMemoryRefused and PeakNone, and no second call
// is made. A c with TreeMemory 0 is errTreeMemoryNone, ErrRefused and not ErrNotHeld, with no call.
// Each call is of c as given but for those two fields, Contained and Grace among the rest. These rows
// run on every system, as the answers are given here, not read from the host.
//
// What it costs when this fails: a caller told a cap is judged after runs a tree no cap holds while
// it runs and reads no peak after, or one whose Contained it asked for runs uncontained; or a caller
// told its cap is refused by this system reads a Cmd of its own that is wrong as the system's.
func TestHoldsTreeMemoryAnswersFromItsTwoHoldsCalls(t *testing.T) {
	t.Parallel()
	notHeldHere := errNoTreeMemory
	for _, r := range []struct {
		name    string
		set     func(*Cmd)
		answers []answer // what the fake Holds gives, call by call
		hold    TreeMemoryHold
		kind    PeakKind
		err     error // nil, or the refusal it is to match
		calls   int
	}{
		{"the first holds", func(*Cmd) {}, []answer{{PeakTreeCharged, nil}}, TreeMemoryHeld, PeakTreeCharged, nil, 1},
		{"the first holds a Contained c", func(c *Cmd) { c.Limits.Contained = true }, []answer{{PeakTreeCharged, nil}}, TreeMemoryHeld, PeakTreeCharged, nil, 1},
		{"the first is not held, the second reads a peak", func(*Cmd) {}, []answer{{PeakNone, notHeldHere}, {PeakProcessResident, nil}}, TreeMemoryJudgedAfter, PeakProcessResident, nil, 2},
		{"the first is not held, the second reads a tree's charge", func(c *Cmd) { c.Grace = time.Second }, []answer{{PeakNone, notHeldHere}, {PeakTreeCharged, nil}}, TreeMemoryJudgedAfter, PeakTreeCharged, nil, 2},
		{"the first is not held, the second reads none", func(*Cmd) {}, []answer{{PeakNone, notHeldHere}, {PeakNone, nil}}, TreeMemoryRefused, PeakNone, nil, 2},
		{"both are not held", func(*Cmd) {}, []answer{{PeakNone, notHeldHere}, {PeakNone, errNoGrace}}, TreeMemoryRefused, PeakNone, nil, 2},
		{"the first is not held, the second refuses a Contained c alone", func(c *Cmd) { c.Limits.Contained = true }, []answer{{PeakNone, errNoContained}, {PeakNone, errContainedAlone}}, TreeMemoryRefused, PeakNone, nil, 2},
		{"the first refuses the caller's own Cmd", func(*Cmd) {}, []answer{{PeakNone, errTimeout}}, TreeMemoryRefused, PeakNone, errTimeout, 1},
		{"the first has no tree to kill", func(*Cmd) {}, []answer{{PeakNone, errNoTree}}, TreeMemoryRefused, PeakNone, errNoTree, 1},
		{"TreeMemory 0", func(c *Cmd) { c.Limits.TreeMemory = 0 }, nil, TreeMemoryRefused, PeakNone, errTreeMemoryNone, 0},
	} {
		c := helperCmd(t, "touch", filepath.Join(t.TempDir(), "started"))
		c.Limits = Limits{TreeMemory: treeLimit}
		r.set(&c)
		var asked []Cmd
		holds := func(a Cmd) (PeakKind, error) {
			asked = append(asked, a)
			if len(asked) > len(r.answers) {
				return PeakNone, errors.New("a call past the fake's answers")
			}
			return r.answers[len(asked)-1].kind, r.answers[len(asked)-1].err
		}
		hold, kind, err := holdsTreeMemory(c, holds)
		if hold != r.hold || kind != r.kind || (r.err == nil) != (err == nil) || (r.err != nil && !errors.Is(err, r.err)) {
			t.Errorf("%s: %v, %q, %v, want %v, %q, %v", r.name, hold, kind, err, r.hold, r.kind, r.err)
		}
		if r.err != nil && errors.Is(err, ErrNotHeld) {
			t.Errorf("%s: %v matches ErrNotHeld, want a refusal of the caller's own", r.name, err)
		}
		if len(asked) != r.calls {
			t.Errorf("%s: Holds asked %d times, want %d", r.name, len(asked), r.calls)
		}
		for i, a := range asked {
			want := c
			want.ReadPeak = true
			if i == 1 {
				want.Limits.TreeMemory = 0
			}
			if !reflect.DeepEqual(a, want) {
				t.Errorf("%s: call %d asked of %+v, want %+v: c as given with ReadPeak set, TreeMemory 0 in the second", r.name, i+1, a, want)
			}
		}
	}
	if !errors.Is(errTreeMemoryNone, ErrRefused) || errors.Is(errTreeMemoryNone, ErrNotHeld) || errors.Is(errTreeMemoryNone, errors.ErrUnsupported) {
		t.Errorf("errTreeMemoryNone: %v, want ErrRefused, not ErrNotHeld or errors.ErrUnsupported", errTreeMemoryNone)
	}
}

// heldThenAfter is the answer of the two Holds calls HoldsTreeMemory replaces, as a caller took them
// before it: Holds with the cap and ReadPeak, and after an ErrNotHeld, Holds without the cap,
// reading PeakNone as refused.
func heldThenAfter(c Cmd) (TreeMemoryHold, PeakKind, error) {
	c.ReadPeak = true
	kind, err := Holds(c)
	if err == nil {
		return TreeMemoryHeld, kind, nil
	}
	if !errors.Is(err, ErrNotHeld) {
		return TreeMemoryRefused, PeakNone, err
	}
	c.Limits.TreeMemory = 0
	if kind, err := Holds(c); err == nil && kind != PeakNone {
		return TreeMemoryJudgedAfter, kind, nil
	}
	return TreeMemoryRefused, PeakNone, nil
}

// TestHoldsTreeMemoryGivesEachAnswerWhereThisSystemGivesIt: on this system, for a c held to a
// TreeMemory, a Contained one, one with a Grace, one held to Processes as well, one that reads its
// peak already, one with no Timeout and one with TreeMemory 0, HoldsTreeMemory gives the answer of
// the two Holds calls it replaces, and starts nothing. Each answer is fixed where the system gives
// it: TreeMemoryHeld and PeakProcessCommitted on Windows, which holds a TreeMemory on its Job
// Object; TreeMemoryJudgedAfter and one process's peak on darwin and the BSDs, which hold none and
// read a peak; TreeMemoryRefused with no error where neither is read. On Linux a TreeMemory is
// TreeMemoryHeld and PeakTreeCharged where Holds of a ReadPeak run reads a tree's charge here, and
// TreeMemoryRefused with no error where it reads none, as no cgroup is delegated. A Contained c is
// never TreeMemoryJudgedAfter on any system, and TreeMemoryRefused with no error where no cap is
// held while its tree runs. A c with no Timeout is errTimeout, and one with TreeMemory 0
// errTreeMemoryNone, each with TreeMemoryRefused and PeakNone, on every system. Each answer is
// logged.
//
// What it costs when this fails: a caller that asks before a run is told a cap holds, or is judged
// after, where Run refuses it or reads no peak, and learns it only after a whole run; or a Contained
// run it asked for is judged after a run nothing contained.
func TestHoldsTreeMemoryGivesEachAnswerWhereThisSystemGivesIt(t *testing.T) {
	t.Parallel()
	read := helperCmd(t, "touch", filepath.Join(t.TempDir(), "started"))
	read.ReadPeak = true
	readKind, readErr := Holds(read)
	type want struct {
		hold TreeMemoryHold
		kind PeakKind
		err  error
	}
	capped := want{TreeMemoryRefused, PeakNone, nil}
	contained := want{TreeMemoryRefused, PeakNone, nil}
	switch {
	case limitsHere == limJob:
		capped = want{TreeMemoryHeld, PeakProcessCommitted, nil}
	case limitsHere == limNone && peakKind != PeakNone:
		capped = want{TreeMemoryJudgedAfter, peakKind, nil}
	case limitsHere == limCgroup && readErr == nil && readKind == PeakTreeCharged:
		capped = want{TreeMemoryHeld, PeakTreeCharged, nil}
	}
	for _, r := range []struct {
		name string
		set  func(*Cmd)
		want *want // nil where the host decides it (a Contained c on Linux)
	}{
		{"TreeMemory", func(*Cmd) {}, &capped},
		{"TreeMemory and ReadPeak", func(c *Cmd) { c.ReadPeak = true }, &capped},
		{"TreeMemory and Processes 4", func(c *Cmd) { c.Limits.Processes = 4 }, nil},
		{"TreeMemory and a Grace of 1s", func(c *Cmd) { c.Grace = time.Second }, nil},
		{"Contained", func(c *Cmd) { c.Limits.Contained = true }, cond2(limitsHere != limCgroup, &contained)},
		{"no Timeout", func(c *Cmd) { c.Timeout = 0 }, &want{TreeMemoryRefused, PeakNone, errTimeout}},
		{"TreeMemory 0", func(c *Cmd) { c.Limits.TreeMemory = 0 }, &want{TreeMemoryRefused, PeakNone, errTreeMemoryNone}},
	} {
		mark := filepath.Join(t.TempDir(), "started")
		c := helperCmd(t, "touch", mark)
		c.Limits = Limits{TreeMemory: treeLimit}
		r.set(&c)
		hold, kind, err := HoldsTreeMemory(c)
		t.Logf("HoldsTreeMemory of %s here: %v, %q, %v", r.name, hold, kind, err)
		if _, statErr := os.Stat(mark); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("%s: HoldsTreeMemory started the program (%v), want nothing started", r.name, statErr)
		}
		if w := r.want; w != nil && (hold != w.hold || kind != w.kind || (w.err == nil) != (err == nil) || (w.err != nil && !errors.Is(err, w.err))) {
			t.Errorf("%s: %v, %q, %v, want %v, %q, %v on this system", r.name, hold, kind, err, w.hold, w.kind, w.err)
		}
		if c.Limits.Contained && hold == TreeMemoryJudgedAfter {
			t.Errorf("%s: TreeMemoryJudgedAfter, want a Contained c held or refused, as a peak read after contains nothing", r.name)
		}
		if c.Limits.TreeMemory == 0 {
			continue
		}
		wantHold, wantKind, wantErr := heldThenAfter(c)
		if hold != wantHold || kind != wantKind || !reflect.DeepEqual(err, wantErr) {
			t.Errorf("%s: %v, %q, %v; the two Holds calls give %v, %q, %v", r.name, hold, kind, err, wantHold, wantKind, wantErr)
		}
	}
}

// cond2 is w where is holds, and nil otherwise.
func cond2[T any](is bool, w *T) *T {
	if is {
		return w
	}
	return nil
}
