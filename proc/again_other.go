//go:build unix && !darwin

package proc

import "syscall"

// killAgain is whether a group is killed again until no live process of it
// answers. Linux's one kill reaches a fork in progress, since the kernel
// fails a fork once a fatal signal is pending, and it signals an exited
// process not yet reaped as if alive, so a kill again would never find the
// group quiet. The BSDs are not tried, so they keep the one kill.
const killAgain = false

// liveIn is false: a process here is reached by a kill of its group throughout its exec.
func liveIn(func(int, syscall.Signal) error, int, []int) bool { return false }

// freeze sends nothing and gives seen: a kill of the group here reaches a process mid-fork and
// mid-exec, so no stop goes first.
func freeze(_ killer, _ int, _ func() bool, seen []int) ([]int, error) { return seen, nil }
