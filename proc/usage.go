package proc

import "time"

// TreeUsage is what a tree has used, as Usage reads it.
type TreeUsage struct {
	// CPU is the CPU time, user and kernel, of every process the tree has held, those that have
	// ended included.
	CPU time.Duration
	// Live is the count of the tree's processes that are running.
	Live int
}

// Usage reads what the tree id names has used: its CPU time and its count of live processes. An id
// Start or JoinTree does not give here is refused, and one whose tree is gone is ErrStale, as Stop's
// is: the tree's leader holds its name, so once the leader exits the tree is gone here, though a
// process it started may run on. On every system but Windows it is an error wrapping
// errors.ErrUnsupported.
func Usage(id TreeID) (TreeUsage, error) {
	return usageOf(id)
}

// Pids reads the ids of the processes in the tree id names, as they are at the read: a process may
// start or end the moment after. It is refused as Usage is.
func Pids(id TreeID) ([]int, error) {
	return pidsOf(id)
}

// JoinTree puts this process in a tree of its own and returns its id, so that every process it
// starts from then on is in the tree, and Usage, Pids and Stop read and end it with this process:
// Stop of the id ends this process too. This process holds the tree's name until it exits, as a
// detached tree's leader does. On Windows a process already in a job joins a job nested in it. On
// every system but Windows it is an error wrapping errors.ErrUnsupported.
func JoinTree() (TreeID, error) {
	return joinTree()
}
