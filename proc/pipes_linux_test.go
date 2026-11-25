package proc

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"testing"
	"time"
)

// TestAPipeReopenedThroughProcAfterItWasFoundFreeHoldsNoDrain: a drain with a wait of zero finds
// stdout's pipe free, every write end closed, 4096 bytes in it unread, and its copy not yet begun.
// Once the drain has asked, and before the copy begins, the test opens a write end of the pipe
// again through /proc/self/fd, as any process of the same user can through /proc/<pid>/fd, and
// writes to it without end. The drain returns within returnSlack, not escaped, with the 4096 bytes
// first in its stdout. Its pipes come from freePipe, so no process a test beside it starts holds
// them when the drain asks, and each copy reads the end it was given, as started's do.
//
// What it costs when this fails: a process that closes its write ends as the drain asks, and opens
// one again before the copy reads the end, holds Run past Drain for as long as it writes.
func TestAPipeReopenedThroughProcAfterItWasFoundFreeHoldsNoDrain(t *testing.T) {
	t.Parallel()
	caps := []*capped{{max: 1 << 20, cut: func() {}}, {max: 1 << 20, cut: func() {}}}
	s := &streams{stdout: caps[0], stderr: caps[1], drained: make(chan copied, 2)}
	want := bytes.Repeat([]byte("x"), 4096)
	s.readers[0], s.readers[1] = freePipe(t, want), freePipe(t, nil)
	path := procFd(t, s.readers[0])
	begun, wrote := make(chan struct{}), make(chan error, 1)
	s.kept = &kept{asked: func(i int) {
		if i != 0 {
			return
		}
		defer close(begun)
		w, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			wrote <- fmt.Errorf("opening %s for writing: %w", path, err)
			return
		}
		go func() {
			defer func() { _ = w.Close() }()
			for {
				if _, err := w.Write([]byte("more")); err != nil {
					wrote <- err
					return
				}
			}
		}()
	}}
	for i, c := range caps {
		r := s.readers[i]
		go func() {
			if i == 0 {
				<-begun
			}
			_, err := io.Copy(c, r)
			s.drained <- copied{i, err}
		}()
	}
	start := time.Now()
	var escaped bool
	var err error
	within(t, "the drain of a pipe written to without end", func() { escaped, err = s.drain(0) })
	if took := time.Since(start); took > returnSlack {
		t.Errorf("the drain of a pipe opened again after it was asked returned after %v, want within %v", took, returnSlack)
	}
	if err != nil || escaped || !bytes.HasPrefix(caps[0].buf.Bytes(), want) {
		t.Errorf("the drain of a pipe opened again after it was asked: %v, escaped %t, stdout of %d bytes, want no error, not escaped, and the 4096 bytes it held first", err, escaped, caps[0].buf.Len())
	}
	if werr := <-wrote; !errors.Is(werr, os.ErrClosed) && !errors.Is(werr, syscall.EPIPE) {
		t.Errorf("the writer of the pipe opened again: %v, want it ended by the pipe's close", werr)
	}
}

// procFd is the /proc/self/fd path of f's descriptor, asked without Fd, which would make f blocking
// and so take its deadlines away.
func procFd(t *testing.T, f *os.File) string {
	t.Helper()
	raw, err := f.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var path string
	if err := raw.Control(func(fd uintptr) { path = fmt.Sprintf("/proc/self/fd/%d", fd) }); err != nil {
		t.Fatal(err)
	}
	return path
}
