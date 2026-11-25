//go:build linux || darwin

package sandbox

import "syscall"

// socketFamilies is stdioFamilies on Linux and macOS, which differ only in
// family, how a descriptor that is a socket is asked its address family.
func socketFamilies(family func(fd int) (int, error)) ([3]int, error) {
	var fams [3]int
	for fd := range fams {
		var st syscall.Stat_t
		if err := syscall.Fstat(fd, &st); err != nil {
			// A stream that is not open is no socket.
			if err == syscall.EBADF {
				continue
			}
			return fams, err
		}
		if st.Mode&syscall.S_IFMT != syscall.S_IFSOCK {
			continue
		}
		fam, err := family(fd)
		if err != nil {
			return fams, err
		}
		fams[fd] = fam
	}
	return fams, nil
}
