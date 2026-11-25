//go:build unix && !linux

package proc

// place has nothing to place cmd in here.
func (*runCgroup) place(*process) error { return nil }

// passedLimit is false here: no cgroup's limit was passed.
func (*runCgroup) passedLimit() bool { return false }
