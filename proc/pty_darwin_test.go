package proc

import (
	"bytes"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// ptyTerm grants and unlocks master's terminal and gives its name, as macOS's grantpt, unlockpt and
// ptsname do, by the same ioctls. No call of x/sys asks the name, which macOS writes to a buffer of
// 128 bytes; syscall.Syscall keeps the buffer where it is for the call.
func ptyTerm(master *os.File) (string, error) {
	fd := master.Fd()
	for _, req := range []uintptr{syscall.TIOCPTYGRANT, syscall.TIOCPTYUNLK} {
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, 0); errno != 0 {
			return "", fmt.Errorf("granting and unlocking: %w", errno)
		}
	}
	var name [128]byte
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))); errno != 0 { //nolint:gosec // G103: the ioctl writes the name to the buffer, by its address
		return "", fmt.Errorf("naming: %w", errno)
	}
	n := bytes.IndexByte(name[:], 0)
	if n <= 0 {
		return "", fmt.Errorf("naming: %q is not a name", name[:])
	}
	return string(name[:n]), nil
}
