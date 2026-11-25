package proc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows" //nolint:depguard // arch.ports.23: the row gives pipeName the pipe's handle, of the type x/sys alone has
)

const (
	// pendingWithin is how long the test gives a goroutine it started to enter a Read that blocks.
	pendingWithin = 250 * time.Millisecond
	// refusedMargin is how long past its bound a refused run, or an unanswered query, may take to
	// return: the rest of Run's refusal, under -race and a loaded host.
	refusedMargin = time.Second
	// queryWithin is the bound the row on askPipeName gives its first query, short so the row is.
	queryWithin = 100 * time.Millisecond
	// endedWithin is how long a query held past its bound is given to end once the Read it waited
	// behind has ended: Windows answers it then at once.
	endedWithin = 5 * time.Second
)

// pendingRead is a pipe a goroutine of the test is blocked reading, given pendingWithin to enter its
// Read, and the Read's error once a byte written to w ends it. A Read not yet pending by then fails
// the row that needs it, with a message that says so: Windows signals no pending synchronous Read.
func pendingRead(t *testing.T) (*os.File, *os.File, <-chan error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = r.Close(), w.Close() })
	return r, w, pendingOn(t, r)
}

// pendingOn starts a goroutine of the test blocked reading r, as pendingRead does, and gives the
// Read's error once a byte written to r's other end ends it.
func pendingOn(t *testing.T, r *os.File) <-chan error {
	t.Helper()
	reading, done := make(chan struct{}), make(chan error, 1)
	go func() {
		close(reading)
		var b [1]byte
		_, err := r.Read(b[:])
		done <- err
	}()
	<-reading
	time.Sleep(pendingWithin) //nolint:forbidigo // tests.isolation.6: Windows signals no pending synchronous Read, so the reader is given time; one not yet pending fails the row with a message that says so
	select {
	case err := <-done:
		t.Fatalf("the reader's Read returned before the row: %v", err)
	default:
	}
	return done
}

// release ends the Read pendingRead started, by a byte written to w, and wants it to end clean.
func release(t *testing.T, w *os.File, read <-chan error) {
	t.Helper()
	if _, err := w.Write([]byte{'x'}); err != nil {
		t.Fatal(err)
	}
	if err := <-read; err != nil {
		t.Errorf("the reader's Read, released: %v", err)
	}
}

// runReleased runs c on a goroutine and gives what it returned within bound; a run not returned by
// then is released by a byte written to w, so the row fails and does not hang.
func runReleased(t *testing.T, c Cmd, w *os.File, bound time.Duration) (got ran, took time.Duration, held bool) {
	t.Helper()
	done := make(chan ran, 1)
	start := time.Now()
	go func() {
		res, err := Run(t.Context(), c)
		done <- ran{res, err}
	}()
	select {
	case got = <-done:
		return got, time.Since(start), false
	case <-time.After(bound):
		if _, err := w.Write([]byte{'x'}); err != nil {
			t.Fatal(err)
		}
		got = <-done
		return got, time.Since(start), true
	}
}

// TestARunWithoutTerminalIsRefusedWithinItsBoundWhenAReadIsPendingOnItsStdin: a Stdin that is a pipe
// a goroutine of the caller's is blocked reading, given to a run without Terminal, is refused with
// errPipeNameUnanswered, which wraps ErrRefused, within pipeNameWithin and refusedMargin: Windows
// does not give the pipe's name while the Read is pending, so ptyPipe cannot tell it from an MSYS or
// Cygwin pty's, and refusing is the side a run without Terminal is for. The program never starts. A run still
// held at the bound is released by a byte written to the pipe, so the row fails and does not hang.
//
// What it costs when this fails: a caller that reads its own os.Stdin in one goroutine and hands it
// to a run without Terminal waits, before the run's Timeout starts, until a line comes in; or, if the
// run is let go on, a program under mintty whose stdin is the pty may ask the caller's user there.
func TestARunWithoutTerminalIsRefusedWithinItsBoundWhenAReadIsPendingOnItsStdin(t *testing.T) {
	t.Parallel()
	r, w, read := pendingRead(t)
	path := filepath.Join(t.TempDir(), "made")
	c := helperCmd(t, "touch", path)
	c.Stdin = r
	bound := pipeNameWithin + refusedMargin
	got, took, held := runReleased(t, c, w, bound)
	if held {
		t.Errorf("a run without Terminal and a Stdin with a Read pending had not returned %v after it began, want it refused within %v", took, bound)
	} else {
		release(t, w, read)
	}
	_, statErr := os.Stat(path)
	if !errors.Is(got.err, errPipeNameUnanswered) || !errors.Is(got.err, ErrRefused) || got.res.Code != -1 || !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("a run without Terminal and a Stdin with a Read pending: %v, Code %d, file %v, want errPipeNameUnanswered, which wraps ErrRefused, Code -1 and no file", got.err, got.res.Code, statErr)
	}
}

// TestARunWithoutTerminalAndAPtysPipeIsRefusedWhileItsReadIsPendingAndOnceItEnds: a pipe the test
// makes under an MSYS pty's name, whose client end a goroutine of the test is blocked reading, given
// as Stdin to a run without Terminal, is refused with errPipeNameUnanswered, which wraps ErrRefused,
// within pipeNameWithin and refusedMargin; once a byte written to its server end ends the Read, the
// same Stdin is refused with errTerminalStdin. Neither run makes its file. The row holds what the
// row on an os.Pipe, whose name is empty, cannot: that the pending Read, not the pipe's name, is
// what the first refusal is for, and that the name read once the Read ends is a pty's.
//
// What it costs when this fails: a caller under mintty with pseudo consoles off that reads its own
// os.Stdin in one goroutine and hands it to a run without Terminal has the run go ahead, and a
// program that asks at its terminal asks the caller's user there.
func TestARunWithoutTerminalAndAPtysPipeIsRefusedWhileItsReadIsPendingAndOnceItEnds(t *testing.T) {
	t.Parallel()
	server, client := ptyLike(t, fmt.Sprintf("msys-goproc%d-pty3-from-master", os.Getpid()))
	read := pendingOn(t, client)
	path := filepath.Join(t.TempDir(), "made")
	c := helperCmd(t, "touch", path)
	c.Stdin = client
	bound := pipeNameWithin + refusedMargin
	got, took, held := runReleased(t, c, server, bound)
	if held {
		t.Errorf("a run without Terminal and a pty's pipe with a Read pending had not returned %v after it began, want it refused within %v", took, bound)
	} else {
		release(t, server, read)
	}
	_, statErr := os.Stat(path)
	if !errors.Is(got.err, errPipeNameUnanswered) || !errors.Is(got.err, ErrRefused) || got.res.Code != -1 || !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("a run without Terminal and a pty's pipe with a Read pending: %v, Code %d, file %v, want errPipeNameUnanswered, which wraps ErrRefused, Code -1 and no file", got.err, got.res.Code, statErr)
	}
	res, err := Run(t.Context(), c)
	_, statErr = os.Stat(path)
	if !errors.Is(err, errTerminalStdin) || !errors.Is(err, ErrRefused) || res.Code != -1 || !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the same run once the Read on its pty's pipe ended: %v, Code %d, file %v, want errTerminalStdin, which wraps ErrRefused, Code -1 and no file", err, res.Code, statErr)
	}
}

// TestANameQueryPastItsBoundEndsWhenTheReadOnItsPipeEnds: askPipeName of a pipe a goroutine of the
// test is blocked reading gives errPipeNameUnanswered within its bound and refusedMargin. The query
// it left behind ends within endedWithin of the Read's end, and a second query of the same pipe is
// answered: nothing of the first is left waiting. The row wants an answer, not a name of some
// length, since the pipe os.Pipe makes may have an empty name, as it does on Windows 10.0.26200.
//
// What it costs when this fails: each refused run leaves an OS thread blocked for good, not only for
// as long as the caller's Read is pending, and a caller that retries leaves one a try.
func TestANameQueryPastItsBoundEndsWhenTheReadOnItsPipeEnds(t *testing.T) {
	t.Parallel()
	r, w, read := pendingRead(t)
	first := nameOf(t, r, queryWithin)
	if !errors.Is(first.err, errPipeNameUnanswered) || first.took > queryWithin+refusedMargin {
		t.Errorf("the query of a pipe with a Read pending: %v after %v, want errPipeNameUnanswered within %v", first.err, first.took, queryWithin+refusedMargin)
	}
	release(t, w, read)
	select {
	case <-first.ended:
	case <-time.After(endedWithin):
		t.Errorf("the query held past its bound had not ended %v after the Read on its pipe ended", endedWithin)
	}
	if second := nameOf(t, r, pipeNameWithin); second.err != nil {
		t.Errorf("the query of the same pipe once its Read ended: %v, want an answer", second.err)
	}
}

// query is what askPipeName gave, how long it took, and the channel closed once its goroutine ended.
type query struct {
	info  fileNameInfo
	err   error
	took  time.Duration
	ended <-chan struct{}
}

// nameOf is askPipeName of f's handle, within.
func nameOf(t *testing.T, f *os.File, within time.Duration) query {
	t.Helper()
	raw, err := f.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var q query
	if err := raw.Control(func(h uintptr) {
		start := time.Now()
		q.info, q.ended, q.err = askPipeName(windows.Handle(h), within)
		q.took = time.Since(start)
	}); err != nil {
		t.Fatal(err)
	}
	return q
}

// TestARunWithTerminalAsksNothingOfItsStdin: a run with Terminal, whose Stdin is a pipe a
// goroutine of the caller's is blocked reading, makes its file and exits 0 before exitsWithin: its
// Stdin is not asked its name, nor anything else, so it is not refused and not held.
//
// What it costs when this fails: a caller that hands its os.Stdin, read in another goroutine, to an
// ordinary run is refused or waits, though it asked for nothing of the terminal.
func TestARunWithTerminalAsksNothingOfItsStdin(t *testing.T) {
	t.Parallel()
	r, w, read := pendingRead(t)
	path := filepath.Join(t.TempDir(), "made")
	c := helperCmd(t, "touch", path)
	c.Stdin, c.Terminal = r, true
	got, took, held := runReleased(t, c, w, exitsWithin)
	if held {
		t.Errorf("a run with Terminal and a Stdin with a Read pending had not returned %v after it began", took)
	} else {
		release(t, w, read)
	}
	if _, statErr := os.Stat(path); got.err != nil || got.res.Code != 0 || statErr != nil {
		t.Errorf("a run with Terminal and a Stdin with a Read pending: %v, Code %d, file %v, stderr %q, want a run that made its file", got.err, got.res.Code, statErr, got.res.Stderr)
	}
}

// TestARunWithoutTerminalAndAStdinOfHandleZeroRuns: a run without Terminal whose Stdin is a file of
// handle 0, as os.Stdin is in a process that has no stdin, is not refused, and its program runs
// and makes its file: ptyPipe takes the handle as no pty's before it asks Windows its type, and
// syscall.StartProcess passes the child nothing for it.
//
// What it costs when this fails: a service, a scheduled task or a detached program that hands its
// os.Stdin to a run without Terminal is refused with errStdinUntold, though its Stdin holds nothing a
// program could read as a terminal.
func TestARunWithoutTerminalAndAStdinOfHandleZeroRuns(t *testing.T) {
	t.Parallel()
	none := os.NewFile(0, "stdin")
	if none == nil {
		t.Fatal("os.NewFile gave nil for handle 0, want the file a process with no stdin has as os.Stdin")
	}
	path := filepath.Join(t.TempDir(), "made")
	c := helperCmd(t, "touch", path)
	c.Stdin = none
	res, err := Run(t.Context(), c)
	if _, statErr := os.Stat(path); err != nil || res.Code != 0 || statErr != nil {
		t.Errorf("a run without Terminal and a Stdin of handle 0: %v, Code %d, file %v, stderr %q, want a run that made its file", err, res.Code, statErr, res.Stderr)
	}
}

// TestANilFileStdinGivesTheProgramTheNullDevice: a run whose Stdin is a nil *os.File, an io.Reader
// that is not nil, gives the program the null device for its stdin, as a nil Stdin does: a handle
// of the character type that is no console and no process.
//
// What it costs when this fails: exec hands the nil file's handle on, InvalidHandle, which is the
// caller's own pseudo-handle, so the program's stdin is a handle to the caller's process, with
// which it can read and write the caller's memory.
func TestANilFileStdinGivesTheProgramTheNullDevice(t *testing.T) {
	t.Parallel()
	var f *os.File
	c := helperCmd(t, "stdin-kind")
	c.Stdin = f
	res, err := Run(t.Context(), c)
	if want := fmt.Sprintf("type %d console no process no\n", windows.FILE_TYPE_CHAR); err != nil || res.Code != 0 || string(res.Stdout) != want {
		t.Errorf("a run with a nil *os.File Stdin: %v, Code %d, wrote %q and %q, want %q", err, res.Code, res.Stdout, res.Stderr, want)
	}
}

// TestAClosedFileStdinGivesTheProgramTheNullDevice: a run whose Stdin is an *os.File already
// closed gives the program the null device for its stdin, as a nil Stdin does: a handle of the
// character type that is no console and no process.
//
// What it costs when this fails: a closed file has the handle of a nil one, InvalidHandle, which
// exec hands on and which is the caller's own pseudo-handle, so the program's stdin is a handle to
// the caller's process, with which it can read and write the caller's memory.
func TestAClosedFileStdinGivesTheProgramTheNullDevice(t *testing.T) {
	t.Parallel()
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	c := helperCmd(t, "stdin-kind")
	c.Stdin = f
	res, err := Run(t.Context(), c)
	if want := fmt.Sprintf("type %d console no process no\n", windows.FILE_TYPE_CHAR); err != nil || res.Code != 0 || string(res.Stdout) != want {
		t.Errorf("a run with a closed *os.File Stdin: %v, Code %d, wrote %q and %q, want %q", err, res.Code, res.Stdout, res.Stderr, want)
	}
}
