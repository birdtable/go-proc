package proc

import (
	"errors"
	"fmt"
	"io"
	"sync"
)

// errWriteLeft is why a sink failed whose writer's Write left without returning, by
// runtime.Goexit or a panic.
var errWriteLeft = errors.New("its Write did not return: it called runtime.Goexit or panicked")

// sink gives a caller's writer (Cmd.Stdout, Cmd.Stderr) what Run keeps of each stream that names
// it, from a goroutine of its own: a pipe's copy only appends to pending, so a writer that is slow
// holds no read, drain or kill. One writer of both streams has one sink, so one goroutine writes it.
type sink struct {
	w             io.Writer
	mu            sync.Mutex
	more          *sync.Cond
	pending       []byte
	of            [2]bool // the streams pending holds bytes of
	ended, failed bool
	started       bool
	err           error
	done          chan struct{}
}

// sinksFor are the sinks of a run's two streams, nil for a nil writer, and one sink for both when
// stdout and stderr are the same comparable writer, as os/exec's interfaceEqual reads it.
func sinksFor(stdout, stderr io.Writer) [2]*sink {
	var s [2]*sink
	for i, w := range []io.Writer{stdout, stderr} {
		if w != nil {
			s[i] = &sink{w: w, done: make(chan struct{})}
			s[i].more = sync.NewCond(&s[i].mu)
		}
	}
	if s[1] != nil && func() (same bool) {
		defer func() { _ = recover() }() //nolint:errcheck // a panic here is == of values not comparable
		return stdout == stderr
	}() {
		s[1] = s[0]
	}
	return s
}

// add queues a copy of p, bytes Run keeps of stream i.
func (s *sink) add(i int, p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.failed && len(p) > 0 {
		s.pending, s.of[i] = append(s.pending, p...), true
		s.more.Signal()
	}
}

// start starts the giving, once the run's tree has started as asked: a run refused after its start
// gives the writer nothing, as its Result holds nothing.
func (s *sink) start() {
	s.started = true
	go s.give()
}

// give hands the writer all that is pending in one Write, until finish is called and nothing is
// left, or the writer fails.
func (s *sink) give() {
	defer close(s.done)
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		for len(s.pending) == 0 && !s.ended {
			s.more.Wait()
		}
		if len(s.pending) == 0 {
			return
		}
		p, of := s.pending, s.of
		s.pending, s.of = nil, [2]bool{}
		if s.write(p, of) != nil {
			return
		}
	}
}

// write gives p, bytes of the streams of, to the writer with s.mu let go, and takes s.mu again
// however Write leaves. A Write that fails, writes short or does not return (runtime.Goexit, a
// panic) fails the sink, which is given no more; a panic stays the writer's own, not recovered.
func (s *sink) write(p []byte, of [2]bool) (err error) {
	err = errWriteLeft
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if err != nil {
			name := map[[2]bool]string{{true, false}: "Stdout", {false, true}: "Stderr", {true, true}: "Stdout and Stderr"}[of]
			s.failed, s.pending, s.err = true, nil, fmt.Errorf("proc: the writer of %s failed, and was given no more: %w", name, err)
		}
	}()
	var n int
	if n, err = s.w.Write(p); err == nil && n < len(p) {
		err = io.ErrShortWrite
	}
	return err
}

// finish waits until the writer has been given all that was queued, or failed, and is why it
// failed. Nothing is queued after it. A sink never started gives nothing.
func (s *sink) finish() error {
	if s == nil || !s.started {
		return nil
	}
	s.mu.Lock()
	s.ended = true
	s.more.Signal()
	s.mu.Unlock()
	<-s.done
	return s.err
}
