//go:build unix

package proc

import (
	"context"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestAProcessThatLeftTheTreeAndHoldsTheOutputEndsTheRunWithWhyEscaped: the helper starts a
// holding child in a session of its own, which the group's kill does not reach, writes its pid
// and exits 0. The child holds both pipes; the drain after the group is killed the run closes them
// and ends with Ended{WhyEscaped} and Code -1, keeping what was read: the pid, and with Left true,
// though the group's listing, which the child left, finds nothing. The run's Drain is a quarter
// second, which the copy of stdout needs only to read the pid the helper wrote before it exited, and
// the run returns before drainWait is out: the child holds the pipes until the test kills it, so
// only the caller's Drain ends the run that soon.
//
// What it costs when this fails: a process that left the tree holds the run open for as long as
// it lives, or a run whose output was cut short by one reads as one that passed, or as one that
// left nothing running, to a caller that reads Left alone.
func TestAProcessThatLeftTheTreeAndHoldsTheOutputEndsTheRunWithWhyEscaped(t *testing.T) {
	t.Parallel()
	c := helperCmd(t, "escape")
	c.Drain = 250 * time.Millisecond
	type ran struct {
		res Result
		err error
	}
	done := make(chan ran, 1)
	go func() {
		res, err := Run(t.Context(), c)
		done <- ran{res, err}
	}()
	var got ran
	select {
	case got = <-done:
	case <-time.After(drainWait):
		// The escaped child holds the pipes until the cleanup kills it: a run that waits out a Drain
		// not its own returns no sooner. Wait it out, so the child's pid is read and the child killed.
		got = <-done
		t.Errorf("a run with a Drain of %v whose child left the tree had not returned %v after it began, want it ended by its Drain", c.Drain, drainWait)
	}
	res, err := got.res, got.err
	pid, perr := strconv.Atoi(strings.TrimSpace(string(res.Stdout)))
	if perr == nil && pid > 0 {
		// The escaped child still holds its pipes' ends, so it is alive, and the pid its own.
		t.Cleanup(func() { _ = signalPid(syscall.Kill, pid, syscall.SIGKILL) })
	}
	if !endedWith(err, WhyEscaped) || res.Code != -1 {
		t.Errorf("a run whose child left the tree: %v, Code %d, want Ended{%s} and Code -1", err, res.Code, WhyEscaped)
	}
	if !res.Left {
		t.Error("a run whose child left the tree: Left false, want true: the child outlived the kill")
	}
	if perr != nil {
		t.Errorf("a run whose child left the tree kept %q, want the pid the helper wrote: %v", res.Stdout, perr)
	}
}

// returnSlack is the slack TestARunReturnsWithinItsTimeoutGraceAndDrain allows past a run's
// Drain: the helper's start and exit, and the drain's asking of each pipe, under a loaded host. It is
// from a measure: in the package's whole run at -cpu 1 under -race, which shares one P
// among every parallel row, 300 runs of the row overshot their Drain by a median of 412 ms, 1058 ms at
// the 99th percentile and 1687 ms at most, and a run beside other work by 2.7 s; the row alone, by 18
// to 71 ms. So the overshoot is the test process's scheduling and not Run, and 5 s is the worst of
// those with about its margin again. It stays far below the 10 s past Timeout and Grace that a Run
// holding its caller by a fixed read bound makes, which the row is red for.
const returnSlack = 5 * time.Second

// TestARunReturnsWithinItsTimeoutGraceAndDrain: a run whose child left the tree and holds both
// pipes, its helper exiting at once, returns within its Drain and returnSlack of its start, and so
// within its Timeout, Grace and Drain, for a nanosecond's Drain, for a quarter second, and for a
// zero one, which is DefaultDrain: no floor and no read bound of the package's own lengthen it.
//
// What it costs when this fails: Run holds its caller past the bound the caller set, up to 10 s
// past Timeout and Grace, as the safe default once did.
func TestARunReturnsWithinItsTimeoutGraceAndDrain(t *testing.T) {
	t.Parallel()
	for _, given := range []time.Duration{time.Nanosecond, 250 * time.Millisecond, 0} {
		c := helperCmd(t, "escape")
		c.Drain, c.Timeout = given, longEnough
		drain := c.drain()
		begun := time.Now()
		res, err := Run(t.Context(), c)
		took := time.Since(begun)
		if pid, perr := strconv.Atoi(strings.TrimSpace(string(res.Stdout))); perr == nil && pid > 0 {
			t.Cleanup(func() { _ = signalPid(syscall.Kill, pid, syscall.SIGKILL) })
		}
		if !endedWith(err, WhyEscaped) || took > drain+returnSlack {
			t.Errorf("a run with a Drain of %v whose child left the tree: %v after %v, want WhyEscaped within its Drain and %v", drain, err, took, returnSlack)
		}
	}
}

// TestAGroupsBoundIsItsGraceAndItsRunsDrain: a group waits for a leader no signal reaches for
// its Grace and the drain it is given, the run's Drain, not a fixed one.
//
// What it costs when this fails: a run whose tree no signal reaches holds its caller the fixed 10 s
// past the Grace whatever Drain the caller set.
func TestAGroupsBoundIsItsGraceAndItsRunsDrain(t *testing.T) {
	t.Parallel()
	for _, drain := range []time.Duration{time.Second, drainWait, time.Minute} {
		cmd := prepare(t.Context(), Cmd{Path: "/bin/true"}, Bare())
		g, err := grouped(cmd, 2*time.Second, drain, Limits{}, nil, true)
		if err != nil {
			t.Fatal(err)
		}
		if want := 2*time.Second + drain; g.bound != want {
			t.Errorf("a group of a run with a Drain of %v: bound %v, want %v", drain, g.bound, want)
		}
	}
}

// TestAGraceSendsTheGroupSIGTERMThenSIGKILL: a run ended with a Grace sends the whole group
// SIGTERM, and SIGKILL once the Grace is gone; a leader that ignores SIGTERM is ended by the
// SIGKILL. The control: with no Grace, the group is sent SIGKILL alone. The SIGTERM is sent as the
// context ends, before the Grace's clock is set, so the order holds for a Grace of any length, and
// the test's is short.
//
// What it costs when this fails: a program given a grace to clean up is killed with no word, or
// one that ignores the word is never killed and holds the run open.
func TestAGraceSendsTheGroupSIGTERMThenSIGKILL(t *testing.T) {
	t.Parallel()
	for _, grace := range []time.Duration{10 * time.Millisecond, 0} {
		ctx, cancel := context.WithCancel(t.Context())
		cmd := prepare(ctx, Cmd{Path: "/bin/sh", Args: []string{"-c", "trap '' TERM; echo up; exec /bin/sleep 300"}}, Bare())
		// exec kills the leader WaitDelay after the cancel, as a last resort; with none, only the
		// group's own SIGKILL can end a leader that ignores SIGTERM.
		cmd.WaitDelay = 0
		g, err := grouped(cmd, grace, drainWait, Limits{}, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		k := &kills{cmd: cmd}
		g.signal = k.send
		// The leader says up once its trap is set, so the SIGTERM comes to one that ignores it.
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stdout = w
		if err := g.start(); err != nil {
			t.Fatal(err)
		}
		_ = w.Close()
		if up, err := io.ReadAll(io.LimitReader(r, 3)); err != nil || string(up) != "up\n" {
			t.Fatalf("the leader said %q (%v), want up", up, err)
		}
		cancel()
		within(t, "the group's wait after the context ended", func() { _, _ = g.wait() })
		_ = r.Close()
		k.mu.Lock()
		sigs := slices.Clone(k.sigs)
		k.mu.Unlock()
		want := []syscall.Signal{syscall.SIGKILL}
		if grace > 0 {
			want = []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL}
		}
		if len(sigs) < len(want) || !slices.Equal(sigs[:len(want)], want) || slices.Contains(sigs[len(want):], syscall.SIGTERM) {
			t.Errorf("a Grace of %v sent the group %v, want %v first and no SIGTERM after", grace, sigs, want)
		}
		if !g.wasCut() {
			t.Errorf("a Grace of %v: the cancel is not noted", grace)
		}
		var status syscall.WaitStatus
		if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
			status = ws
		}
		if !status.Signaled() || status.Signal() != syscall.SIGKILL {
			t.Errorf("a Grace of %v: the leader that ignores SIGTERM ended %v, want by SIGKILL", grace, cmd.ProcessState)
		}
	}
}
