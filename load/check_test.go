package load

import (
	"errors"
	"math"
	"testing"
)

// TestErrNoAverageMatchesErrUnsupported: a caller that handles every missing capability alike, by
// errors.ErrUnsupported, handles a Read with no average with them.
//
// What it costs when this fails: a caller that falls back on errors.ErrUnsupported takes a Read
// error for a failure of the machine and stops, where it would have gone on without a load.
func TestErrNoAverageMatchesErrUnsupported(t *testing.T) {
	t.Parallel()
	if !errors.Is(ErrNoAverage, errors.ErrUnsupported) {
		t.Fatalf("errors.Is(ErrNoAverage, errors.ErrUnsupported) is false: %v", ErrNoAverage)
	}
}

// TestCheckRefusesAnAverageNoKernelKeeps: an average that is not a number, is infinite or is below
// zero is refused, and every finite average from zero up passes.
//
// What it costs when this fails: Read hands a caller a NaN, which every comparison it makes reads
// as false, so a caller that waits for the load to fall below a figure waits for ever, or one that
// refuses above a figure never refuses.
func TestCheckRefusesAnAverageNoKernelKeeps(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		avg [3]float64
		ok  bool
	}{
		{[3]float64{0, 0, 0}, true},
		{[3]float64{1.5, 2, 99.99}, true},
		{[3]float64{math.NaN(), 0, 0}, false},
		{[3]float64{0, math.Inf(1), 0}, false},
		{[3]float64{0, 0, math.Inf(-1)}, false},
		{[3]float64{0, 0, -0.01}, false},
	} {
		if err := check(tc.avg); (err == nil) != tc.ok {
			t.Errorf("check(%v) = %v; want ok %v", tc.avg, err, tc.ok)
		}
	}
}
