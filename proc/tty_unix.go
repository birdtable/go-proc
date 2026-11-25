//go:build unix

package proc

import (
	"os"

	"golang.org/x/term" //nolint:depguard // arch.shared.7: proc tells a run's Stdin from a terminal, not for styling; x/term's IsTerminal in place of a hand-written copy
)

// isTerminal is whether f is a terminal, as term.IsTerminal reads it: whether its termios read
// takes f. The read goes by f's own descriptor, through SyscallConn, as Fd would set f blocking.
// The read answers at once, a Read pending on f or not, so it gives no error.
func isTerminal(f *os.File) (bool, error) {
	raw, err := f.SyscallConn()
	if err != nil {
		return false, nil
	}
	is := false
	if err := raw.Control(func(fd uintptr) { is = term.IsTerminal(int(fd)) }); err != nil {
		return false, nil
	}
	return is, nil
}
