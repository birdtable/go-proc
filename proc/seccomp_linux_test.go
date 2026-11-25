package proc

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix" //nolint:depguard // arch.ports.23: the row filters openat2 by seccomp, whose calls and constants are in x/sys alone
)

// refuseOpenat2 installs a seccomp filter on every thread of this process that answers openat2
// with errno, as a kernel before Linux 5.6 (ENOSYS) or a container's filter (EPERM) does, and lets
// every other call through. openat2 is call 437 on every architecture, so the filter reads the
// number alone. It sets no_new_privs first, which a filter needs without CAP_SYS_ADMIN; the
// processes this one starts inherit both.
func refuseOpenat2(errno syscall.Errno) error {
	prog := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0}, // seccomp_data.nr
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.SYS_OPENAT2, Jt: 0, Jf: 1},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(errno)},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
	}
	// no_new_privs is the calling thread's, and TSYNC hands it to the rest, so both calls are made
	// on one thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("setting no_new_privs: %w", err)
	}
	fprog := unix.SockFprog{Len: uint16(len(prog)), Filter: &prog[0]}
	// With TSYNC, a thread that cannot take the filter is named by its id in r1, with no errno.
	thread, _, e := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&fprog))) //nolint:gosec // G103: seccomp takes the filter program the kernel defines, by its address
	switch {
	case e != 0:
		return fmt.Errorf("installing the seccomp filter: %w", e)
	case thread != 0:
		return fmt.Errorf("installing the seccomp filter: thread %d could not take it", thread)
	}
	return nil
}
