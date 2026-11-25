//go:build unix

package proc

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

// dupFile is a second descriptor of f, closed when the test ends unless closed before.
func dupFile(t *testing.T, f *os.File) *os.File {
	t.Helper()
	raw, err := f.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	fd, dupErr := -1, error(nil)
	if err := raw.Control(func(h uintptr) {
		syscall.ForkLock.RLock()
		defer syscall.ForkLock.RUnlock()
		if fd, dupErr = syscall.Dup(int(h)); dupErr == nil {
			syscall.CloseOnExec(fd)
		}
	}); err != nil || dupErr != nil {
		t.Fatal(err, dupErr)
	}
	d := os.NewFile(uintptr(fd), "dup")
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// freePipe is the read end of a pipe that holds data and whose one write end is closed, closed when
// the test ends. It is made, written and closed under ForkLock, so no process a test beside it
// starts holds a copy of its write end, as one forked between an os.Pipe and its Close would until
// its exec. Its read end is non-blocking, as os.Pipe's is, so it takes a deadline.
func freePipe(t *testing.T, data []byte) *os.File {
	t.Helper()
	var p [2]int
	n, err, cerr := func() (int, error, error) {
		syscall.ForkLock.RLock()
		defer syscall.ForkLock.RUnlock()
		if err := syscall.Pipe(p[:]); err != nil {
			return 0, err, nil
		}
		syscall.CloseOnExec(p[0])
		n, err := syscall.Write(p[1], data)
		return n, errors.Join(err, syscall.SetNonblock(p[0], true)), syscall.Close(p[1])
	}()
	if p[0] != 0 {
		r := os.NewFile(uintptr(p[0]), "free")
		t.Cleanup(func() { _ = r.Close() })
		if err == nil && cerr == nil && n == len(data) {
			return r
		}
	}
	t.Fatalf("a free pipe of %d bytes: wrote %d, %v, close %v", len(data), n, err, cerr)
	return nil
}
