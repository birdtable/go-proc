//go:build !darwin && !linux

package load

import (
	"errors"
	"testing"
)

// TestReadRefusesWithErrNoAverage: on Windows and every system but macOS and Linux, Read returns a
// zero Reading and an error that matches ErrNoAverage and errors.ErrUnsupported.
//
// What it costs when this fails: a caller on Windows reads a load of zero, or one made up, and
// takes the machine for idle when no figure was read at all.
func TestReadRefusesWithErrNoAverage(t *testing.T) {
	t.Parallel()
	r, err := Read()
	if !errors.Is(err, ErrNoAverage) || !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("Read() error = %v; want one that matches ErrNoAverage and errors.ErrUnsupported", err)
	}
	if r != (Reading{}) {
		t.Fatalf("Read() = %+v with its error; want the zero Reading", r)
	}
}
