//go:build darwin

package sandbox

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

// The macOS block starts the program with libSystem's posix_spawn, POSIX_SPAWN_SETEXEC and
// POSIX_SPAWN_CLOEXEC_DEFAULT, so the kernel closes at the exec every descriptor but the standard
// streams, whatever another goroutine made or received since the marking. Package
// syscall has no posix_spawn, so it is called as golang.org/x/sys/unix calls libc, with no cgo: each
// function is imported from libSystem here, a trampoline in spawn_darwin_$GOARCH.s jumps to it, and
// four of syscall's functions are linked by name:
//   - syscall.syscall6 calls a libc function on the system stack, as syscall does; syscall keeps it,
//     and its signature, for golang.org/x/sys (go.dev/issue/67401).
//   - syscall.rawSyscall6, kept the same way, does so and keeps the goroutine's P, as syscall.Exec's
//     execve does, and makes the spawn: a P given up while the runtime's exec lock is held may go to
//     a new thread, which waits on the lock, and the goroutine back with no P throws "stopm holding
//     locks" and hangs.
//   - syscall.runtime_BeforeExec and syscall.runtime_AfterExec are what syscall.Exec calls around its
//     exec: the runtime makes no thread during it, and on macOS first takes the preemption signals
//     already sent (go.dev/issue/41702). The runtime gives them to syscall alone, so a Go whose
//     linker refuses them fails this package's build, never its run.
//
// syscall.Exec does one thing more before its exec: it puts back the open-file limit the process
// started with, which the runtime raised as it started, so the program gets the limit the caller
// was given. syscall exports neither that limit, its origRlimitNofile, nor the step, so spawnExec
// first calls syscall.Exec for no path: it puts the limit back, fails with ENOENT, and leaves the
// limit where a failed syscall.Exec leaves it. No Go document promises that syscall.Exec puts
// the limit back before its execve. A Go whose exec of no path gives any other error refuses the
// run here, and one that stops putting the limit back gives the program the raised limit, which
// netblock's row for it shows red on a Mac.

//go:linkname syscall6 syscall.syscall6
func syscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err syscall.Errno) //nolint:revive // argument-limit: syscall's own signature, which the link must match

//go:linkname rawSyscall6 syscall.rawSyscall6
func rawSyscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err syscall.Errno) //nolint:revive // argument-limit: syscall's own signature, which the link must match

//go:linkname beforeExec syscall.runtime_BeforeExec
func beforeExec()

//go:linkname afterExec syscall.runtime_AfterExec
func afterExec()

//go:cgo_import_dynamic libc_posix_spawn posix_spawn "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_posix_spawnattr_init posix_spawnattr_init "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_posix_spawnattr_setflags posix_spawnattr_setflags "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_posix_spawnattr_destroy posix_spawnattr_destroy "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_posix_spawn_file_actions_init posix_spawn_file_actions_init "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_posix_spawn_file_actions_addinherit_np posix_spawn_file_actions_addinherit_np "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_posix_spawn_file_actions_destroy posix_spawn_file_actions_destroy "/usr/lib/libSystem.B.dylib"

// The trampolines' addresses, each set by its DATA line in spawn_darwin_$GOARCH.s.
var (
	spawnAddr, attrInitAddr, attrFlagsAddr, attrDestroyAddr uintptr //nolint:gochecknoglobals // set by the assembly's DATA lines, the libc functions' trampolines
	actionsInitAddr, actionsInheritAddr, actionsDestroyAddr uintptr //nolint:gochecknoglobals // set by the assembly's DATA lines, the libc functions' trampolines
)

// <spawn.h>'s flags.
const (
	spawnSetExec        = 0x0040
	spawnCloexecDefault = 0x4000
)

// spawnExec execs prog with argv and env by posix_spawn, as syscall.Exec would but that only the
// standard streams reach the program. It first puts back the open-file limit through exec,
// syscall.Exec, called for no path, and refuses, with nothing spawned, when that gives anything
// but the ENOENT an exec of no path gives. It returns only when the exec did not happen.
func spawnExec(exec func(argv0 string, argv, envv []string) error, prog string, argv, env []string) error {
	if err := exec("", nil, nil); !errors.Is(err, syscall.ENOENT) {
		if err == nil {
			err = errors.New("no error")
		}
		return fmt.Errorf("the open-file limit this process started with could not be put back: syscall.Exec of no path gave %w, where it gives %q", err, syscall.ENOENT.Error())
	}
	path, err := syscall.BytePtrFromString(prog)
	if err != nil {
		return err
	}
	argvp, err := syscall.SlicePtrFromStrings(argv)
	if err != nil {
		return err
	}
	envp, err := syscall.SlicePtrFromStrings(env)
	if err != nil {
		return err
	}
	// Each libc call below takes its argument by address, which syscall has no wrapper for (G103),
	// and gives its error as its result, never by errno, so syscall6's err is not read. attr and
	// actions are a posix_spawnattr_t and a posix_spawn_file_actions_t, pointers libc fills.
	var attr, actions uintptr
	if r, _, _ := syscall6(attrInitAddr, uintptr(unsafe.Pointer(&attr)), 0, 0, 0, 0, 0); r != 0 { //nolint:errcheck,gosec // see above
		return syscall.Errno(r)
	}
	// A failed exec's own error is the one returned, so the destroys' are not read.
	defer syscall6(attrDestroyAddr, uintptr(unsafe.Pointer(&attr)), 0, 0, 0, 0, 0) //nolint:errcheck,gosec // see above

	if r, _, _ := syscall6(actionsInitAddr, uintptr(unsafe.Pointer(&actions)), 0, 0, 0, 0, 0); r != 0 { //nolint:errcheck,gosec // see above
		return syscall.Errno(r)
	}
	defer syscall6(actionsDestroyAddr, uintptr(unsafe.Pointer(&actions)), 0, 0, 0, 0, 0) //nolint:errcheck,gosec // see above

	if r, _, _ := syscall6(attrFlagsAddr, uintptr(unsafe.Pointer(&attr)), spawnSetExec|spawnCloexecDefault, 0, 0, 0, 0); r != 0 { //nolint:errcheck,gosec // see above
		return syscall.Errno(r)
	}
	for fd := range uintptr(3) {
		if r, _, _ := syscall6(actionsInheritAddr, uintptr(unsafe.Pointer(&actions)), fd, 0, 0, 0, 0); r != 0 { //nolint:errcheck,gosec // see above
			return syscall.Errno(r)
		}
	}
	beforeExec()
	r, _, _ := rawSyscall6(spawnAddr, 0, uintptr(unsafe.Pointer(path)), uintptr(unsafe.Pointer(&actions)), uintptr(unsafe.Pointer(&attr)), uintptr(unsafe.Pointer(&argvp[0])), uintptr(unsafe.Pointer(&envp[0]))) //nolint:errcheck,gosec // see above
	afterExec()
	return syscall.Errno(r)
}
