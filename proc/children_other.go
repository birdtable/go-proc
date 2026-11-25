//go:build !unix

package proc

import (
	"errors"
	"fmt"
	"time"
)

// errNoChildrenCPU is ChildrenCPU on a system that keeps no sum of a process's waited-for children:
// Windows, and every other system that is not Unix.
var errNoChildrenCPU = fmt.Errorf("proc: ChildrenCPU reads the CPU time of a process's waited-for children on Unix alone; on Windows, TreeUsage.CPU from Usage of the id JoinTree gives counts every process this one starts from then on, and includes this process's own CPU time, which ChildrenCPU does not: %w", errors.ErrUnsupported)

// childrenCPU refuses: no sum of waited-for children is kept here.
func childrenCPU() (time.Duration, error) { return 0, errNoChildrenCPU }
