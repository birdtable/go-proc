package proc

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix" //nolint:depguard // arch.ports.23: the test unlocks and names a pseudo-terminal of its own, by the calls Linux does it with
)

// ptyTerm unlocks master's terminal and gives its name: /dev/pts/ and the number Linux gives it.
func ptyTerm(master *os.File) (string, error) {
	fd := int(master.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		return "", fmt.Errorf("unlocking: %w", err)
	}
	n, err := unix.IoctlGetUint32(fd, unix.TIOCGPTN)
	if err != nil {
		return "", fmt.Errorf("reading its number: %w", err)
	}
	return fmt.Sprintf("/dev/pts/%d", n), nil
}
