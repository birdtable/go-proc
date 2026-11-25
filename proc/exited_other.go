//go:build unix && !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package proc

import "errors"

// exited cannot wait here for a process to exit without reaping it, so it
// refuses, and the member's group is killed at once: the runner runs no member
// whose group it cannot kill while the pid is still the member's.
func exited(int) error {
	return errors.New("this system gives the runner no way to wait for a member to exit and leave it unreaped, so the runner cannot kill its group safely and runs no member here")
}

// exitedNow cannot ask here either, so it refuses as exited does.
func exitedNow(pid int) (bool, error) { return false, exited(pid) }
