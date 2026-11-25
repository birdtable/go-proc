package proc

import "time"

// ChildrenCPU reads the CPU time, user and system, of this process's children that have ended and
// been waited for, by Run, an exec.Cmd's Wait or any other wait: the sum the system keeps for them,
// grandchildren included only where their own parent waited for them. A child still running, or one
// that ended but has not been waited for, is not in it, and the figure counts every waited child of
// this process's life, not only those of one run. On Windows it is an error wrapping
// errors.ErrUnsupported; there TreeUsage.CPU from Usage of the id JoinTree gives counts what this
// process starts from then on, and includes this process's own CPU time, which this does not.
func ChildrenCPU() (time.Duration, error) {
	return childrenCPU()
}
