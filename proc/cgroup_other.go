//go:build !linux

package proc

import "context"

// cgroupHost is nothing here: only Linux makes a run a cgroup of its own.
type cgroupHost struct{}

// runCgroup is nothing here: no run has a cgroup of its own.
type runCgroup struct{}

// cgroupFor is no cgroup: limitsRefused refused a TreeMemory here before, and a peak is read
// otherwise.
func cgroupFor(Cmd, cgroupHost) (*runCgroup, error) { return nil, nil }

// heldHere is whether a run of c here reads its peak, and no refusal: no cgroup is made here, and the
// peak is read by its own means where peakReads.
func heldHere(Cmd, cgroupHost) (bool, error) { return peakReads, nil }

// bound has no cgroup to bound here.
func (*runCgroup) bound(context.Context) {}

// started has nothing to close here.
func (*runCgroup) started() error { return nil }

// startFailed is err as it is here.
func (*runCgroup) startFailed(err error) error { return err }

// end has nothing to end here.
func (*runCgroup) end(bool) error { return nil }
