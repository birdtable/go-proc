package sandbox

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

// TestANilStoredInErrUsageOrErrNoBlockRefusesStill: with nil stored in ErrUsage and in ErrNoBlock,
// the relative program Run refuses and the folders RunHolding and Check refuse are still refused, each with the
// value ErrUsage was built with, before anything is set, and a block that could not be set still matches the value
// ErrNoBlock was built with and errors.ErrUnsupported. Once each holds its value again, errors.Is
// matches each refusal by it.
//
// What it costs when this fails: `sandbox.ErrUsage = nil` anywhere in the binary makes Run take a
// relative program and go on to set the block and exec it, and a nil ErrNoBlock
// makes a block that was not set match nothing a caller asks.
func TestANilStoredInErrUsageOrErrNoBlockRefusesStill(t *testing.T) {
	// serial: stores nil in ErrUsage and ErrNoBlock, which the package's other tests read
	usageWas, noBlockWas := storeNil(t, &ErrUsage), storeNil(t, &ErrNoBlock)
	_, relative := usage("bin/true", nil)
	_, checked := Check(Folders{ReadOnly: []string{"rel"}}, "/bin/true")
	refusals := map[string]error{
		"a relative program":              relative,
		"RunHolding of a relative folder": RunHolding(Folders{ReadWrite: []string{"rel"}}, "/bin/true", nil, nil),
		"Check of a relative folder":      checked,
	}
	for what, err := range refusals {
		if !errors.Is(err, usageWas) {
			t.Errorf("%s, with nil in ErrUsage: %v, want ErrUsage as built", what, err)
		}
	}
	unset := noBlock{errors.New("the step failed")}
	if !errors.Is(unset, noBlockWas) || !errors.Is(unset, errors.ErrUnsupported) {
		t.Errorf("a block not set, with nil in ErrNoBlock: %v, want ErrNoBlock as built and errors.ErrUnsupported", unset)
	}
	ErrUsage, ErrNoBlock = usageWas, noBlockWas
	for what, err := range refusals {
		if !errors.Is(err, ErrUsage) {
			t.Errorf("%s: %v does not match ErrUsage once it holds its value again", what, err)
		}
	}
	if !errors.Is(unset, ErrNoBlock) {
		t.Errorf("a block not set: %v does not match ErrNoBlock once it holds its value again", unset)
	}
}

// TestANilStoredInErrNoSystemLeavesASystemWithNoBlockMatchingItAsBuilt: with nil stored in
// ErrNoSystem, a system with no block still refuses with the value ErrNoSystem was built with, and
// still matches ErrNoBlock; once it holds its value again, errors.Is matches the refusal by it.
//
// What it costs when this fails: `sandbox.ErrNoSystem = nil` anywhere in the binary makes a system
// with no block match nothing that says so, and a caller reads it as a host that refused a step.
func TestANilStoredInErrNoSystemLeavesASystemWithNoBlockMatchingItAsBuilt(t *testing.T) {
	// serial: stores nil in ErrNoSystem, which the package's other tests read
	was := storeNil(t, &ErrNoSystem)
	err := runNone()
	if !errors.Is(err, was) || !errors.Is(err, ErrNoBlock) {
		t.Errorf("a system with no block, with nil in ErrNoSystem: %v, want ErrNoSystem as built and ErrNoBlock", err)
	}
	ErrNoSystem = was
	if !errors.Is(err, ErrNoSystem) {
		t.Errorf("a system with no block: %v does not match ErrNoSystem once it holds its value again", err)
	}
}
