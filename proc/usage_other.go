//go:build !windows

package proc

import (
	"errors"
	"fmt"
)

// errNoUsage is Usage, Pids and JoinTree on a system that does not keep a tree as one object it
// can read: Windows alone does, as a Job Object.
var errNoUsage = fmt.Errorf("proc: a tree's usage and processes are read, and a process joins a tree of its own, on Windows alone, where the tree is a Job Object: %w", errors.ErrUnsupported)

// usageOf refuses: no tree here is one object to read.
func usageOf(TreeID) (TreeUsage, error) { return TreeUsage{}, errNoUsage }

// pidsOf refuses: no tree here is one object to read.
func pidsOf(TreeID) ([]int, error) { return nil, errNoUsage }

// joinTree refuses: no tree here is one object to join.
func joinTree() (TreeID, error) { return "", errNoUsage }
