//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"testing"
	"unsafe"
)

// TestADescriptorTheFolderListingCannotStatDoesNotReachTheProgram: on macOS, a POSIX shared memory
// descriptor reaches the command at 3 without close-on-exec, and the program, the row child, finds
// no descriptor open above stderr. /dev/fd answers a stat only for some kinds of descriptor, and
// for a shared memory one says EBADF every time, so os.ReadDir's listing of the folder, which stats
// each entry, leaves it out, and a block that found its descriptors by that listing would leave it
// open into the exec. The block reads the folder by its names alone. The row first
// shows, in this process, that os.ReadDir leaves the descriptor out and Readdirnames keeps it, so a
// macOS or a Go that lists it fails the row rather than passing it with nothing proved.
//
// What it costs when this fails: a descriptor a caller holds open reaches the program behind the
// block, and one that names a socket or a file outside the folders reaches past it.
func TestADescriptorTheFolderListingCannotStatDoesNotReachTheProgram(t *testing.T) {
	t.Parallel()
	shm := sharedMemory(t)
	n := strconv.Itoa(int(shm.Fd()))
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Fatal(err)
	}
	listed := slices.ContainsFunc(entries, func(e os.DirEntry) bool { return e.Name() == n })
	dir, err := os.Open("/dev/fd")
	if err != nil {
		t.Fatal(err)
	}
	names, err := dir.Readdirnames(-1)
	if cerr := dir.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
	if listed || !slices.Contains(names, n) {
		t.Fatalf("/dev/fd: os.ReadDir lists the shared memory descriptor %s %v, Readdirnames %v; the row needs it left out by the first and kept by the second, or it proves nothing", n, listed, slices.Contains(names, n))
	}
	p := newProof(t)
	report := filepath.Join(t.TempDir(), "row")
	code, out := p.start(t, nil, []*os.File{shm}, p.command(true, rowWord, report, "descriptors")...)
	said, err := os.ReadFile(report)
	if code != 0 || err != nil {
		t.Fatalf("the command did not run the program: exit %d, %v: %s", code, err, out)
	}
	if string(said) != "none" {
		t.Errorf("the program held, above stderr, with a shared memory descriptor at 3 in the command: %s", said)
	}
}

// sharedMemory is a POSIX shared memory descriptor of the test's own, its name unlinked at once, made
// close-on-exec under syscall.ForkLock so no child another test starts meanwhile inherits it.
func sharedMemory(t *testing.T) *os.File {
	t.Helper()
	name, err := syscall.BytePtrFromString("/gp-netblock." + strconv.Itoa(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	syscall.ForkLock.RLock()
	fd, _, errno := syscall.Syscall(syscall.SYS_SHM_OPEN, uintptr(unsafe.Pointer(name)), uintptr(syscall.O_CREAT|syscall.O_EXCL|syscall.O_RDWR), 0o600) //nolint:gosec // G103: shm_open takes its name by address
	if errno == 0 {
		syscall.CloseOnExec(int(fd))
	}
	syscall.ForkLock.RUnlock()
	if errno != 0 {
		t.Fatalf("shm_open: %v", errno)
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_SHM_UNLINK, uintptr(unsafe.Pointer(name)), 0, 0); errno != 0 { //nolint:gosec // G103: shm_unlink takes its name by address
		t.Errorf("shm_unlink: %v", errno)
	}
	f := os.NewFile(fd, "shared memory")
	t.Cleanup(func() { _ = f.Close() })
	return f
}
