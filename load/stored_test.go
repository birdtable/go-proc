package load

import (
	"errors"
	"testing"
)

// TestANilStoredInErrNoAverageRefusesStill: with nil stored in ErrNoAverage, as any package of the
// binary can, Read's refusal still matches the value ErrNoAverage was built with and
// errors.ErrUnsupported, and the cause; where Read refuses here, as on Windows, its own refusal is asked too.
// Once ErrNoAverage holds its value again, errors.Is matches the refusal by it.
//
// What it costs when this fails: `load.ErrNoAverage = nil` anywhere in the binary makes a missing
// average match neither name, so a caller that goes on without a load by errors.ErrUnsupported
// stops instead.
func TestANilStoredInErrNoAverageRefusesStill(t *testing.T) {
	// serial: stores nil in ErrNoAverage, which the package's other tests read
	was := ErrNoAverage
	ErrNoAverage = nil
	t.Cleanup(func() { ErrNoAverage = was })
	cause := errors.New("the source could not be read")
	refusals := []error{noAverage(cause)}
	if _, err := Read(); err != nil { // Windows and every system but macOS and Linux
		refusals = append(refusals, err)
	}
	for _, err := range refusals {
		if !errors.Is(err, was) || !errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("Read's refusal with nil in ErrNoAverage: %v, want ErrNoAverage as built and errors.ErrUnsupported", err)
		}
	}
	if !errors.Is(refusals[0], cause) {
		t.Errorf("Read's refusal %v does not match its cause", refusals[0])
	}
	if ErrNoAverage = was; !errors.Is(refusals[0], ErrNoAverage) {
		t.Errorf("Read's refusal %v does not match ErrNoAverage once it holds its value again", refusals[0])
	}
}
