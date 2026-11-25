package proc

import (
	"bytes"
	"errors"
	"io"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// heard is a writer that keeps what it is given, safe to read while Run writes it, and says on got
// each time it is given more.
type heard struct {
	mu  sync.Mutex
	buf bytes.Buffer
	got chan struct{}
}

func newHeard() *heard { return &heard{got: make(chan struct{}, 1)} }

func (h *heard) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.buf.Write(p)
	select {
	case h.got <- struct{}{}:
	default:
	}
	return len(p), nil
}

func (h *heard) String() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.buf.String()
}

// until waits, for exitsWithin at most, until h holds want.
func (h *heard) until(t *testing.T, want string) {
	t.Helper()
	out := time.NewTimer(exitsWithin)
	defer out.Stop()
	for h.String() != want {
		select {
		case <-h.got:
		case <-out.C:
			t.Fatalf("the writer holds %q, want %q while the program runs", h.String(), want)
		}
	}
}

// TestAWriterHasTheFirstLineBeforeTheSecondIsPrinted: a program that prints a line, waits, then
// prints another, here the stdin helper echoing what the test feeds it a line at a time, gives
// Stdout's writer the first line while it waits: the test feeds the second only once the writer
// holds the first. The writer ends with what Result.Stdout holds.
//
// What it costs when this fails: a caller that streams a run's output, as hatch shows a test run
// live, sees nothing until the run ends.
func TestAWriterHasTheFirstLineBeforeTheSecondIsPrinted(t *testing.T) {
	t.Parallel()
	c := helperCmd(t, "stdin")
	in, feed := io.Pipe()
	w := newHeard()
	c.Stdin, c.Stdout = in, w
	type ran struct {
		res Result
		err error
	}
	done := make(chan ran, 1)
	go func() {
		res, err := Run(t.Context(), c)
		done <- ran{res, err}
	}()
	if _, err := feed.Write([]byte("one\n")); err != nil {
		t.Fatal(err)
	}
	w.until(t, "one\n")
	if _, err := feed.Write([]byte("two\n")); err != nil {
		t.Fatal(err)
	}
	if err := feed.Close(); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil || r.res.Code != 0 || string(r.res.Stdout) != "one\ntwo\n" || w.String() != string(r.res.Stdout) {
		t.Errorf("%v, Code %d, Stdout %q, the writer %q: want both one and two, the same", r.err, r.res.Code, r.res.Stdout, w.String())
	}
}

// TestAWriterIsGivenWhatResultKeepsAndStopsAtTheCap: a program that writes 150 bytes to stdout
// under a MaxOutput of 100 gives Stdout's writer the first 100, the bytes Result.Stdout holds, and
// ends with WhyCap; Stderr's writer, of a program within the cap, is given what Result.Stderr holds.
//
// What it costs when this fails: a caller's writer holds output Result does not, past the cap the
// caller set on its memory, or less than Result, so what it showed and what it kept differ.
func TestAWriterIsGivenWhatResultKeepsAndStopsAtTheCap(t *testing.T) {
	t.Parallel()
	c := helperCmd(t, "write", "150", "0", "hold")
	c.MaxOutput = 100
	w := newHeard()
	c.Stdout = w
	res, err := Run(t.Context(), c)
	if !endedWith(err, WhyCap) || len(res.Stdout) != 100 || w.String() != string(res.Stdout) {
		t.Errorf("%v, Stdout %d bytes, the writer %d bytes: want WhyCap, 100 each, the same", err, len(res.Stdout), len(w.String()))
	}
	c = helperCmd(t, "write", "0", "20", "exit")
	e := newHeard()
	c.Stderr = e
	res, err = Run(t.Context(), c)
	if err != nil || res.Code != 0 || e.String() != strings.Repeat("e", 20) || e.String() != string(res.Stderr) {
		t.Errorf("%v, Code %d, Stderr %q, the writer %q: want 20 e each", err, res.Code, res.Stderr, e.String())
	}
}

// writeFunc is a writer whose type is not comparable, so two of it are never one writer.
type writeFunc func([]byte) (int, error)

func (f writeFunc) Write(p []byte) (int, error) { return f(p) }

// TestOneWriterOfBothStreamsIsWrittenByOneGoroutineAtATime: Stdout and Stderr set to one
// bytes.Buffer, which no lock guards, are given both streams whole, run under -race in the tiers
// that set it, which reads two Writes at once as a race. Two writers that are not comparable are
// two sinks, as os/exec reads them, and the same writer is one.
//
// What it costs when this fails: a caller that hands one writer for both, as os/exec allows, has
// its writer called from two goroutines at once and its bytes torn.
func TestOneWriterOfBothStreamsIsWrittenByOneGoroutineAtATime(t *testing.T) {
	t.Parallel()
	const n = 100000
	c := helperCmd(t, "write", strconv.Itoa(n), strconv.Itoa(n), "exit")
	var both bytes.Buffer
	c.Stdout, c.Stderr = &both, &both
	res, err := Run(t.Context(), c)
	if err != nil || res.Code != 0 || both.Len() != 2*n || bytes.Count(both.Bytes(), []byte("o")) != n {
		t.Errorf("%v, Code %d, the writer %d bytes, %d o: want %d each stream", err, res.Code, both.Len(), bytes.Count(both.Bytes(), []byte("o")), n)
	}
	f := writeFunc(func(p []byte) (int, error) { return len(p), nil })
	if s := sinksFor(f, f); s[0] == s[1] {
		t.Error("a writer that is not comparable is one sink for both streams")
	}
	if s := sinksFor(&both, &both); s[0] != s[1] {
		t.Error("one comparable writer is two sinks")
	}
}

// TestAWriterThatFailsIsGivenNoMoreAndTheRunGoesOn: a writer that returns an error, one that
// returns a short count with none, and one whose Write calls runtime.Goexit, as t.FailNow does, is
// called once and no more; the run ends by its program's exit, Result holds the whole output, and
// Run's error wraps the writer's, io.ErrShortWrite for the short one and errWriteLeft for the
// Goexit, and names the stream. The test binary lives through the Goexit.
//
// What it costs when this fails: a caller whose writer failed, as a closed connection, loses the
// run's result too, or is told nothing, or is written again after it said it could take no more;
// a writer that ends its goroutine ends the caller's whole process on a fault in proc's lock.
func TestAWriterThatFailsIsGivenNoMoreAndTheRunGoesOn(t *testing.T) {
	t.Parallel()
	broken := errors.New("the writer is broken")
	for _, r := range []struct {
		name   string
		stderr bool
		write  func([]byte) (int, error)
		want   error
		names  string
	}{
		{"an error, Stdout's", false, func([]byte) (int, error) { return 0, broken }, broken, "of Stdout"},
		{"a short count, Stderr's", true, func(p []byte) (int, error) { return len(p) - 1, nil }, io.ErrShortWrite, "of Stderr"},
		{"runtime.Goexit, Stdout's", false, func([]byte) (int, error) { runtime.Goexit(); return 0, nil }, errWriteLeft, "of Stdout"},
	} {
		c := helperCmd(t, "write", "100000", "100000", "exit")
		var calls int
		w := writeFunc(func(p []byte) (int, error) { calls++; return r.write(p) })
		if r.stderr {
			c.Stderr = w
		} else {
			c.Stdout = w
		}
		res, err := Run(t.Context(), c)
		if !errors.Is(err, r.want) || !strings.Contains(err.Error(), r.names) || res.Code != 0 || len(res.Stdout) != 100000 || len(res.Stderr) != 100000 || calls != 1 {
			t.Errorf("%s: %v, Code %d, %d and %d bytes, %d Writes: want %v naming %q, Code 0, the whole output, one Write", r.name, err, res.Code, len(res.Stdout), len(res.Stderr), calls, r.want, r.names)
		}
	}
}

// TestAWriterThatBlocksHoldsNoKill: a writer that never returns from its first Write, of the pid
// the nested helper prints of its held child, holds no kill: the child, a member of the run's
// tree, ends within a margin of Timeout while the Write is still in flight, and Run has not
// returned a second past the child's end and the run's Grace and Drain. Released, the writer is
// given the rest, and Run ends with WhyDeadline and what the writer was given in Result.Stdout.
//
// What it costs when this fails: a caller's slow writer, a terminal no one reads or a full
// socket, holds the program past the Timeout the caller set to bound it.
func TestAWriterThatBlocksHoldsNoKill(t *testing.T) {
	t.Parallel()
	// The row waits out its 3 s Timeout, so it runs in the release tier and every native run, not
	// the push tier's -short run.
	if testing.Short() {
		t.Skip("release tier: waits out a 3 s Timeout")
	}
	const timeout, margin = 3 * time.Second, 10 * time.Second
	c := helperCmd(t, "nested", "hold")
	c.Timeout, c.Drain = timeout, time.Second
	var (
		mu    sync.Mutex
		given []byte
	)
	pid, release := make(chan int, 1), make(chan struct{})
	c.Stdout = writeFunc(func(p []byte) (int, error) {
		mu.Lock()
		given = append(given, p...)
		line, whole := bytes.CutSuffix(given, []byte("\n"))
		mu.Unlock()
		if n, err := strconv.Atoi(string(line)); whole && err == nil {
			pid <- n
			<-release
		}
		return len(p), nil
	})
	begun := time.Now()
	done := make(chan error, 1)
	var res Result
	go func() {
		var err error
		res, err = Run(t.Context(), c)
		done <- err
	}()
	var child int
	select {
	case child = <-pid:
	case err := <-done:
		t.Fatalf("the run ended (%v) before the writer was given the child's pid", err)
	}
	end, err := processEnd(child)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	select {
	case err := <-end:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(timeout + margin - time.Since(begun)):
		t.Errorf("the child %d runs %s after the start, past its Timeout of %s, while the writer blocks", child, time.Since(begun), timeout)
	}
	// Run is given its Grace and Drain and a second more to return while the Write is in flight,
	// which it must not.
	select {
	case err := <-done:
		close(release)
		t.Fatalf("Run returned (%v) with its writer's Write in flight", err)
	case <-time.After(c.Grace + c.Drain + time.Second):
	}
	close(release)
	if err := <-done; !endedWith(err, WhyDeadline) || !bytes.Equal(given, res.Stdout) {
		t.Errorf("%v, Stdout %q, the writer %q: want WhyDeadline, the same", err, res.Stdout, given)
	}
}

// TestAWriterBlockedInAWriteIsGivenWhatQueuedInOneWriteMore: a writer held in its first Write
// while three more lines are queued is given those three in one Write once it returns, and no
// third, the bound Cmd.Drain states: once the program is done, Run waits on the Write in flight and
// one Write more.
//
// What it costs when this fails: a caller whose writer is slow waits past the bound Drain states
// on a Write for each read Run made while the writer was held.
func TestAWriterBlockedInAWriteIsGivenWhatQueuedInOneWriteMore(t *testing.T) {
	t.Parallel()
	var calls []string
	entered, release := make(chan struct{}), make(chan struct{})
	s := sinksFor(writeFunc(func(p []byte) (int, error) {
		calls = append(calls, string(p))
		if len(calls) == 1 {
			close(entered)
			<-release
		}
		return len(p), nil
	}), nil)[0]
	s.start()
	s.add(0, []byte("one\n"))
	<-entered
	for _, line := range []string{"two\n", "three\n", "four\n"} {
		s.add(0, []byte(line))
	}
	finished := make(chan error, 1)
	go func() { finished <- s.finish() }()
	close(release)
	if err := <-finished; err != nil || len(calls) != 2 || calls[1] != "two\nthree\nfour\n" {
		t.Errorf("%v, Writes %q: want no error, one and then two, three and four in one Write", err, calls)
	}
}

// TestAWriterThatBlocksHoldsNoRead: while Stdout's writer is held in its first Write, the stdin
// helper is fed 256 KiB, past what a pipe holds, and echoes it all and exits, as Run keeps reading
// its stdout; the feed ends within exitsWithin with the writer still held. Released, the writer is
// given what Result.Stdout holds.
//
// What it costs when this fails: a caller's slow writer, a terminal no one reads or a full socket,
// stops Run reading the program's output, so the program blocks on a full pipe and runs on until
// its Timeout, held by the caller's writer and not by itself.
func TestAWriterThatBlocksHoldsNoRead(t *testing.T) {
	t.Parallel()
	const size = 256 << 10
	c := helperCmd(t, "stdin")
	in, feed := io.Pipe()
	var (
		mu    sync.Mutex
		given []byte
	)
	entered, release := make(chan struct{}), make(chan struct{})
	c.Stdin = in
	c.Stdout = writeFunc(func(p []byte) (int, error) {
		mu.Lock()
		first := len(given) == 0
		given = append(given, p...)
		mu.Unlock()
		if first {
			close(entered)
			<-release
		}
		return len(p), nil
	})
	type ran struct {
		res Result
		err error
	}
	done := make(chan ran, 1)
	go func() {
		res, err := Run(t.Context(), c)
		done <- ran{res, err}
	}()
	if _, err := feed.Write([]byte("one\n")); err != nil {
		t.Fatal(err)
	}
	<-entered
	fed := make(chan error, 1)
	go func() {
		_, err := feed.Write(bytes.Repeat([]byte("o"), size))
		fed <- errors.Join(err, feed.Close())
	}()
	select {
	case err := <-fed:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(exitsWithin):
		t.Errorf("the feed of %d bytes had not ended %s after it began, while the writer was held, want it ended", size, exitsWithin)
	}
	close(release)
	r := <-done
	if r.err != nil || r.res.Code != 0 || len(r.res.Stdout) != size+4 || !bytes.Equal(given, r.res.Stdout) {
		t.Errorf("%v, Code %d, Stdout %d bytes, the writer %d: want no error, Code 0, %d each, the same", r.err, r.res.Code, len(r.res.Stdout), len(given), size+4)
	}
}
