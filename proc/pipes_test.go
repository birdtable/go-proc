//go:build unix

package proc

import (
	"bytes"
	"io"
	"os"
	"testing"
	"time"
)

// TestAPipeIsHeldOnlyWhileAWriteEndIsOpenElsewhere: of two pipes, each with a byte written and
// this process's write ends handed to keep, the one whose write end is open a second time is held
// and the other, its byte unread, is not; once the second is closed, neither is. A pipe is asked
// once and, if held, again after forkCopy: a test beside this one that starts a program holds a
// copy of every pipe end open here until that program begins.
//
// What it costs when this fails: a pipe no process holds is taken for held, so a short Drain cuts
// the output of a tree that left nothing and ends its run with WhyEscaped; or a pipe a process
// holds is taken for free, so the run waits on that process past Drain.
func TestAPipeIsHeldOnlyWhileAWriteEndIsOpenElsewhere(t *testing.T) {
	t.Parallel()
	var r, w [2]*os.File
	for i := range 2 {
		var err error
		if r[i], w[i], err = os.Pipe(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = r[i].Close() })
		if _, err := w[i].Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	other := dupFile(t, w[0])
	k, err := keep(w)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k.let() })
	if !k.held(r[0], 0) {
		t.Error("a pipe whose write end is open a second time: not held, want held")
	}
	if !freed(k, r[1], 1) {
		t.Error("a pipe whose one write end was handed to keep, a byte unread in it: held, want not")
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	if !freed(k, r[0], 0) {
		t.Error("a pipe whose second write end is closed, a byte unread in it: held, want not")
	}
}

// forkCopy is how long a test here waits out the copy of its pipe ends that a process another test
// starts holds from its fork to its exec, a millisecond or so.
const forkCopy = 250 * time.Millisecond

// freed is whether pipe i is not held, or no longer held after forkCopy.
func freed(k *kept, r *os.File, i int) bool {
	if !k.held(r, i) {
		return true
	}
	<-time.After(forkCopy)
	return !k.held(r, i)
}

// TestAPipeIsReadPastDrainForWhatItHoldsAndNoMore: tail reads a pipe that holds 12000 bytes, more
// than one of its reads takes, a write end of it still open, and gives those bytes and returns: it
// takes FIONREAD's count and waits for nothing past it.
//
// What it costs when this fails: past Drain, a pipe a process opened a write end of again after it
// was asked, as Linux lets one do through /proc/<pid>/fd, holds the run for as long as that process
// writes; or a zero Drain cuts output a tree that left nothing left buffered.
func TestAPipeIsReadPastDrainForWhatItHoldsAndNoMore(t *testing.T) {
	t.Parallel()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	// 12000 bytes fit a pipe's buffer on every Unix here, so the write returns.
	want := bytes.Repeat([]byte("held"), 3000)
	if _, err := w.Write(want); err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	var terr error
	within(t, "the tail of a pipe a write end of which is open", func() { terr = tail(r, &got) })
	if terr != nil || !bytes.Equal(got.Bytes(), want) {
		t.Errorf("the tail of a pipe holding %d bytes: %v, gave %d, want them all, in order", len(want), terr, got.Len())
	}
}

// TestAFreePipeWhoseCopyWasStoppedGivesWhatItHeld: a drain with a wait of zero finds stdout's pipe
// free, every write end closed and 4096 bytes in it unread. Its copy is held back until the drain
// has asked, then ends as the drain's deadline ends it, having read nothing. The drain returns not
// escaped, with no error and the 4096 bytes in its stdout, which only the read past Drain (tail)
// can have taken, and Stdout's writer is given those bytes too. Its pipes come from
// freePipe, so no process a test beside it starts holds them.
//
// What it costs when this fails: on a system whose copy had not reached what a program left in a
// pipe when Drain ended, a zero or short Drain loses that output, though no process held the pipe.
func TestAFreePipeWhoseCopyWasStoppedGivesWhatItHeld(t *testing.T) {
	t.Parallel()
	w := newHeard()
	to := sinksFor(w, nil)
	to[0].start()
	caps := []*capped{{max: 1 << 20, cut: func() {}, to: to[0]}, {max: 1 << 20, cut: func() {}}}
	s := &streams{stdout: caps[0], stderr: caps[1], drained: make(chan copied, 2)}
	want := bytes.Repeat([]byte("x"), 4096)
	s.readers[0], s.readers[1] = freePipe(t, want), freePipe(t, nil)
	begun := make(chan struct{})
	s.kept = &kept{asked: func(i int) {
		if i == 0 {
			close(begun)
		}
	}}
	go func() {
		<-begun
		s.drained <- copied{0, os.ErrDeadlineExceeded}
	}()
	r1 := s.readers[1]
	go func() {
		_, err := io.Copy(caps[1], r1)
		s.drained <- copied{1, err}
	}()
	var escaped bool
	var err error
	within(t, "the drain of a free pipe whose copy was stopped", func() { escaped, err = s.drain(0) })
	if ferr := to[0].finish(); ferr != nil || w.String() != string(want) {
		t.Errorf("the writer of the drained stdout: %v, %d bytes, want no error and the 4096 bytes the tail read", ferr, len(w.String()))
	}
	if err != nil || escaped || !bytes.Equal(caps[0].buf.Bytes(), want) {
		t.Errorf("the drain of a free pipe whose copy was stopped before it read: %v, escaped %t, stdout of %d bytes, want no error, not escaped, and the 4096 bytes it held", err, escaped, caps[0].buf.Len())
	}
}
