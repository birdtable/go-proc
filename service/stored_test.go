package service

import (
	"errors"
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

// TestANilStoredInTheExportedRefusalsRefusesStill: with nil stored in ErrAbsent, ErrPercent and
// ErrUsage, a system with no tool, a task whose command holds a %, and a unit name a tool would
// misread are still refused, each with the value its exported name was built with, ErrAbsent's with
// errors.ErrUnsupported too. Once each holds its value again, errors.Is matches each refusal by it.
//
// What it costs when this fails: `service.ErrAbsent = nil` anywhere in the binary gives a system
// with no tool an empty tool and no error, and a nil ErrPercent lets a task through whose % Task
// Scheduler reads as a variable.
func TestANilStoredInTheExportedRefusalsRefusesStill(t *testing.T) {
	// serial: stores nil in ErrAbsent, ErrPercent and ErrUsage, which the package's other tests read
	absent, percent, use := storeNil(t, &ErrAbsent), storeNil(t, &ErrPercent), storeNil(t, &ErrUsage)
	_, folders := foldersOf("plan9", 0)
	_, tool := toolOf(host{goos: "plan9"}, Plan{}, nil)
	rows := []struct {
		what string
		err  error
		want *error
		was  error
	}{
		{"the folders on a system with no tool", folders, &ErrAbsent, absent},
		{"the tool on a system with no tool", tool, &ErrAbsent, absent},
		{"a task whose command holds a %", percentFree(`<Task><Actions><Exec><Command>C:\a%PATH%</Command></Exec></Actions></Task>`), &ErrPercent, percent},
		{"a unit name that starts with -", checkName("-x"), &ErrUsage, use},
	}
	for _, r := range rows {
		if !errors.Is(r.err, r.was) {
			t.Errorf("%s, with nil stored: %v, want the refusal as built", r.what, r.err)
		}
	}
	for _, err := range []error{folders, tool} {
		if !errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("%v, with nil in ErrAbsent, does not match errors.ErrUnsupported", err)
		}
	}
	ErrAbsent, ErrPercent, ErrUsage = absent, percent, use
	for _, r := range rows {
		if !errors.Is(r.err, *r.want) {
			t.Errorf("%s: %v does not match its exported name once it holds its value again", r.what, r.err)
		}
	}
}
