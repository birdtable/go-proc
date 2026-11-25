//go:build unix

package proc_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/birdtable/go-proc/proc"
)

// shell runs script under /bin/sh with proc.Run, in a folder of the test's own, with an
// environment built here, so nothing the test inherited reaches the shell, and the given Timeout
// and Grace.
func shell(ctx context.Context, t *testing.T, script string, timeout, grace time.Duration) (proc.Result, error) {
	t.Helper()
	dir := t.TempDir()
	env := []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "LC_ALL=C"}
	return proc.Run(ctx, proc.Cmd{Path: "/bin/sh", Args: []string{"-c", script}, Dir: dir, Env: env, Timeout: timeout, Grace: grace, Drain: shellDrain, MaxOutput: 1 << 16})
}

// longEnough is a Timeout no test here waits out.
const longEnough = 5 * time.Minute

// shellDrain is the Drain of a shell's run: long enough for a member the kill ended to exit.
const shellDrain = 10 * time.Second

// endedWith is whether err holds a proc.Ended of why.
func endedWith(err error, why string) bool {
	var e proc.Ended
	return errors.As(err, &e) && e.Why == why
}

// TestTheTwoStreamsAreKeptApartAndAProgramsOwnExitIsItsCode: what the program writes to stdout is
// Stdout and to stderr Stderr, each in the order written, and its own exit, failed or not, is its
// Code with no error.
//
// What it costs when this fails: a caller reads a program's errors as its output, or a program that
// failed on its own as one the run ended.
func TestTheTwoStreamsAreKeptApartAndAProgramsOwnExitIsItsCode(t *testing.T) {
	t.Parallel()
	res, err := shell(t.Context(), t, "echo one; echo two >&2; echo three; echo four >&2; exit 3", longEnough, 0)
	if string(res.Stdout) != "one\nthree\n" || string(res.Stderr) != "two\nfour\n" {
		t.Errorf("the program said %q and %q, want each stream apart, in order", res.Stdout, res.Stderr)
	}
	if err != nil || res.Code != 3 {
		t.Errorf("a program that exited 3: %v, Code %d, want no error and Code 3", err, res.Code)
	}
}

// held is a pipe whose write end every process the test starts while the
// test holds it open inherits, at path: it is not closed on exec, so the
// shell and what it starts each hold it. Once the test closes its
// own copy, the read end comes to its end when every process holding one is
// gone, which is how a test waits for a process it did not start to end,
// with no clock. Nothing waits for a writer to open it, as a named pipe's
// reader does, so no process can come and go unseen.
type held struct {
	r, w *os.File
	path string
}

// holding is a held pipe, closed when the test ends.
func holding(t *testing.T) *held {
	t.Helper()
	var p [2]int
	// The fork lock keeps a process another goroutine starts from taking the
	// read end before it is marked close on exec.
	syscall.ForkLock.RLock()
	err := syscall.Pipe(p[:])
	if err == nil {
		syscall.CloseOnExec(p[0])
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	h := &held{r: os.NewFile(uintptr(p[0]), "held"), w: os.NewFile(uintptr(p[1]), "held"), path: fmt.Sprintf("/dev/fd/%d", p[1])}
	t.Cleanup(func() {
		_ = h.r.Close()
		_ = h.w.Close()
	})
	return h
}

// handed is a pipe whose write end a run hands its program alone, as its descriptor 3, the first of
// running's extra files: os.Pipe marks both ends close on exec, so no process another test starts
// holds it, and a parallel test may take one. The program writes to path, and said waits for it.
func handed(t *testing.T) *held {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = r.Close()
		_ = w.Close()
	})
	return &held{r: r, w: w, path: "/dev/fd/3"}
}

// near is what fires when most of the test's time is gone: a wait on a
// process fails there and says what it waited for, rather than run into go
// test's own deadline with no word of which wait it was. With no deadline,
// nothing fires.
func near(t *testing.T) <-chan time.Time {
	t.Helper()
	deadline, ok := t.Deadline()
	if !ok {
		return nil
	}
	return time.After(time.Until(deadline) * 9 / 10)
}

// said waits until a process holding the pipe has written line to it.
func (h *held) said(t *testing.T, line string) {
	t.Helper()
	got := make(chan string, 1)
	go func() {
		b := make([]byte, len(line))
		n, _ := io.ReadFull(h.r, b)
		got <- string(b[:n])
	}()
	select {
	case s := <-got:
		if s != line {
			t.Fatalf("the pipe said %q, want %q", s, line)
		}
	case <-near(t):
		t.Fatalf("no process said %q on the pipe with the test's deadline near", line)
	}
}

// gone closes the test's own write end and waits until every process that
// inherited one has ended. A process the program started that outlived it
// holds the pipe, and the test fails with its deadline near.
func (h *held) gone(t *testing.T, what string) {
	t.Helper()
	if err := h.w.Close(); err != nil {
		t.Fatal(err)
	}
	read := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, h.r)
		read <- err
	}()
	select {
	case err := <-read:
		if err != nil {
			t.Errorf("%s: the pipe could not be read to its end: %v", what, err)
		}
	case <-near(t):
		t.Fatalf("%s: a process the program started still holds the pipe with the test's deadline near", what)
	}
}

// TestAProcessAProgramLeftRunningEndsWithIt: the program's shell starts a sleep in the background
// and exits 0; the run kills the program's tree as it ends, so the sleep ends too, and its hold on
// the output pipe does not keep the run waiting. The shell forks the sleep before it says left, so
// the sleep holds the pipe before the program can end.
//
// What it costs when this fails: a program that leaves a process in the background holds the run
// open until that process ends, and a caller waits for minutes on nothing.
func TestAProcessAProgramLeftRunningEndsWithIt(t *testing.T) {
	// serial: every process the binary starts while the held pipe is open inherits its write end, so a process another test started would hold it and fail this one
	h := holding(t)
	res, err := shell(t.Context(), t, "sleep 300 & echo left", longEnough, 0)
	if err != nil || res.Code != 0 || string(res.Stdout) != "left\n" {
		t.Errorf("the program said %q (%v, Code %d)", res.Stdout, err, res.Code)
	}
	h.gone(t, "a program that exited 0")
}

// ended is what a run returned.
type ended struct {
	res proc.Result
	err error
}

// running runs script as shell does, in a goroutine, with extra as its ExtraFiles, and gives what the
// run returns once it does.
func running(ctx context.Context, t *testing.T, script string, timeout, grace time.Duration, extra ...*os.File) <-chan ended {
	t.Helper()
	dir := t.TempDir()
	env := []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "LC_ALL=C"}
	done := make(chan ended, 1)
	go func() {
		res, err := proc.Run(ctx, proc.Cmd{Path: "/bin/sh", Args: []string{"-c", script}, Dir: dir, Env: env, Timeout: timeout, Grace: grace, Drain: shellDrain, MaxOutput: 1 << 16, ExtraFiles: extra})
		done <- ended{res, err}
	}()
	return done
}

// returned is what the run gives, or a failure with the test's deadline near.
func returned(t *testing.T, done <-chan ended, what string) ended {
	t.Helper()
	select {
	case e := <-done:
		return e
	case <-near(t):
		t.Fatalf("%s: the run had not returned with the test's deadline near", what)
	}
	return ended{}
}

// TestEachWayARunIsEndedEndsEveryProcessItStarted: cancelling the run's context, and the run
// reaching its Timeout, each kill the program's whole tree, not only its first process; and so does
// a Grace's SIGKILL, for a tree that ignores the SIGTERM. The shell starts both sleeps before the
// second says it is up, and then only waits, so the end comes when no process of the tree is
// starting another.
//
// What it costs when this fails: an interrupted or timed-out run leaves the program's children
// running, holding CPU and files after the caller was told the run ended.
func TestEachWayARunIsEndedEndsEveryProcessItStarted(t *testing.T) {
	// serial: every process the binary starts while the held pipe is open inherits its write end, so a process another test started would hold it and fail this one
	h := holding(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := running(ctx, t, "sleep 300 & { echo up >"+h.path+"; exec sleep 300; } & wait", longEnough, 0)
	h.said(t, "up\n")
	cancel()
	if e := returned(t, done, "a run cancelled"); !endedWith(e.err, proc.WhyCancelled) || e.res.Code != -1 {
		t.Errorf("a run cancelled: %v, Code %d, want Ended{%s}", e.err, e.res.Code, proc.WhyCancelled)
	}
	h.gone(t, "a run cancelled")

	rows := []struct {
		what, trap string
		grace      time.Duration
	}{
		{"a run that reached its Timeout", "", 0},
		{"a run that reached its Timeout with a Grace, its tree ignoring SIGTERM", "trap '' TERM; ", time.Second},
	}
	// Each row waits out its 3 s Timeout, and the Grace after it, so it runs in the release tier and
	// every native run, not the push tier's -short run. Every row's run is started before any row is
	// waited on, so the waits overlap and the test takes one Timeout and its Grace, not the sum. Each row's tree may inherit the other's pipe too, as each run starts in a
	// goroutine, and both end within the same seconds, so each pipe still comes to its end only once
	// its own row's tree is gone; a process either row leaves running holds both rows red.
	type started struct {
		h    *held
		done <-chan ended
	}
	runs := make([]started, len(rows))
	if !testing.Short() {
		for i, r := range rows {
			h := holding(t)
			runs[i] = started{h, running(t.Context(), t, r.trap+"sleep 300 & { echo up >"+h.path+"; exec sleep 300; } & wait", 3*time.Second, r.grace)}
		}
	}
	for i, r := range rows {
		t.Run(r.what, func(t *testing.T) {
			if testing.Short() {
				t.Skip("release tier: waits out a 3 s Timeout")
			}
			h, done := runs[i].h, runs[i].done
			h.said(t, "up\n")
			if e := returned(t, done, r.what); !endedWith(e.err, proc.WhyDeadline) || e.res.Code != -1 {
				t.Errorf("%s: %v, Code %d, want Ended{%s}", r.what, e.err, e.res.Code, proc.WhyDeadline)
			}
			h.gone(t, r.what)
		})
	}
}

// TestAGraceGivesTheProgramTheSIGTERMToEndOnItsOwn: a run whose context ends with a Grace sends the
// tree SIGTERM, and a program that traps it runs its trap and exits; its exit ends the grace, and
// the rest of its tree is killed then, long before the Grace is gone. The program says up on a
// handed pipe once its trap is set, and the context ends then, so the SIGTERM comes to a program
// that traps it, with no clock waited out.
//
// What it costs when this fails: a program given a grace to clean up is killed with no word, and
// leaves behind what it would have cleaned.
func TestAGraceGivesTheProgramTheSIGTERMToEndOnItsOwn(t *testing.T) {
	t.Parallel()
	h := handed(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := running(ctx, t, "trap 'echo termed; exit 0' TERM; echo up >"+h.path+"; sleep 300 & wait", longEnough, longEnough, h.w)
	h.said(t, "up\n")
	cancel()
	e := returned(t, done, "a program that traps SIGTERM")
	if !endedWith(e.err, proc.WhyCancelled) || e.res.Code != -1 || string(e.res.Stdout) != "termed\n" {
		t.Errorf("a program that traps SIGTERM: %q, %v, Code %d, want termed, and Ended{%s}", e.res.Stdout, e.err, e.res.Code, proc.WhyCancelled)
	}
}
