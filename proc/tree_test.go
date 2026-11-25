package proc

import (
	"errors"
	"testing"
)

// TestASystemWithNoTreeToNameStartsAndStopsNothing: a system with no tree, and a Unix whose start
// times are not read here, refuse a detached start, a stop by id and an interrupt by id with an
// error of their own that wraps errors.ErrUnsupported; a system that names its trees refuses none.
// Where this system is one of the two, Start, Stop and Interrupt give that error, and nothing is
// started or signalled.
//
// What it costs when this fails: on a system with no tree a detached program runs out of reach of
// every stop, or a stop or an interrupt by id signals a group whose leader died and whose pid
// another process took.
func TestASystemWithNoTreeToNameStartsAndStopsNothing(t *testing.T) {
	t.Parallel()
	for kind, want := range map[treeIDKind]error{idNone: errNoTree, idUnread: errNoTreeID, idGroup: nil, idJob: nil} {
		got := unnamed(kind)
		if !errors.Is(got, want) || (want != nil && !errors.Is(got, errors.ErrUnsupported)) {
			t.Errorf("unnamed(%d) = %v, want %v, wrapping errors.ErrUnsupported when it is an error", kind, got, want)
		}
	}
	err := unnamed(treeIDHere)
	if err == nil {
		return
	}
	if tree, got := Start(Detached{Path: "/nowhere/prog"}); !errors.Is(got, err) || tree != (Tree{}) {
		t.Errorf("Start here gave %+v and %v, want no tree and %v", tree, got, err)
	}
	if got := Stop("2.1"); !errors.Is(got, err) {
		t.Errorf("Stop here gave %v, want %v", got, err)
	}
	if got := Interrupt("2.1"); !errors.Is(got, err) {
		t.Errorf("Interrupt here gave %v, want %v", got, err)
	}
}
