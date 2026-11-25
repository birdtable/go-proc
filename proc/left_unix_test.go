//go:build unix

package proc

import (
	"bufio"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"
)

const (
	// chainRuns is how many groups TestAChainThatForksAndExitsIsLeft starts: each must read Left true.
	chainRuns = 3
	// chainLinks is how many links the chain makes between the listing of /proc and the stats on
	// Linux, as a /proc of thousands of processes lets it: the links listed are gone and reaped by
	// then, and those born since are not listed.
	chainLinks = 20
	// chainBound is how long the row waits for chainLinks links before it lets the stats go on: a
	// group that is frozen makes none.
	chainBound = 200 * time.Millisecond
	// chainScript is a shell that starts a chain in the background, each link of which writes its
	// count, forks the next and exits at once, and exits 0 itself. A link is a shell of its own, which
	// the one before it starts with the script and the next count as its arguments: a chain of
	// calls of one function, a shell's subshell each, ends by itself where the shell limits its
	// recursion, as dash does at 1000, and a group with none left reads Left false.
	chainScript = `s='echo "$1"; [ "$1" -gt 0 ] && { /bin/sh -c "$0" "$0" $(($1 - 1)) & }'; /bin/sh -c "$s" "$s" 100000 &`
	// chainOutlives is how many links the chain must make, event by event, to be one that does not end
	// by itself: past the 1000 a shell's recursion limit gives.
	chainOutlives = 1100
)

// TestAChainThatForksAndExitsIsLeft: a leader that starts a chain of processes, each of which forks
// the next and exits at once, and exits 0 itself, leaves a group that reads Left true, and the wait
// ends with its exit and the group killed, the chain with it. No link lives long, and one is always
// alive. On Linux the listing is /proc's names, then each one's stat, and the row holds the stats
// until the chain has made chainLinks links more, as a /proc of thousands of processes does: a link
// listed is gone by its stat, its children born after the listing, and so the group reads Left false
// unless it is frozen before the listing. A frozen group makes no link, and the
// row lets the stats go on after chainBound. A link gone is one reaped, by init or a subreaper: under
// an init that reaps nothing a link stays a zombie, which is a process left, and the row cannot go
// red. On macOS the listing is one sysctl, which the kernel makes from one list of pids taken under
// its lock: there is no gap in it a test can widen. gap runs there between the listing before the
// stop and the stop, which already holds the chain, and the row is green with the
// freeze or without it.
//
// What it costs when this fails: a caller that fails closed on a program that leaves work running
// passes one that keeps it running by forking on and on, which costs it nothing to do on purpose,
// and whose tree, unlike one by setsid, the group's listing can see.
func TestAChainThatForksAndExitsIsLeft(t *testing.T) {
	t.Parallel()
	for i := range chainRuns {
		cmd := prepare(t.Context(), Cmd{Path: "/bin/sh", Args: []string{"-c", chainScript}}, Bare())
		g, err := grouped(cmd, 0, drainWait, Limits{}, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		k := &kills{cmd: cmd}
		g.signal = k.send
		counts := chainCounts(t, cmd)
		g.gap = func() { linksMore(counts) }
		var killed, waited error
		within(t, "the group's wait", func() { killed, waited = g.wait() })
		if killed != nil && !errors.Is(killed, os.ErrProcessDone) {
			t.Errorf("run %d: the kill of a chain: %v, want it killed or found gone", i, killed)
		}
		if waited != nil || !cmd.ProcessState.Success() {
			t.Errorf("run %d: a leader that starts a chain and exits 0: %v, %v, want its exit 0", i, waited, cmd.ProcessState)
		}
		if !g.leftOver() {
			t.Errorf("run %d: a leader that leaves a chain that forks and exits: Left false, want true", i)
		}
	}
}

// TestTheChainTheLeftRowStartsDoesNotEndByItself: the chain chainScript starts, read link by link
// from the counts each writes, makes chainOutlives links, more than the 1000 at which a shell that
// limits its function recursion, as dash does, ends a chain of calls by itself, and is then ended
// with its group. The row waits on the links, not on a clock.
//
// What it costs when this fails: TestAChainThatForksAndExitsIsLeft's chain ends by itself, a few
// hundred milliseconds into a run that a busy machine delays its stop by, and the group it lists
// holds nothing, so it reads Left false for a group that was right to (7 of 50
// whole-package runs at -cpu 1 failed with "Maximum function recursion depth (1000) reached").
func TestTheChainTheLeftRowStartsDoesNotEndByItself(t *testing.T) {
	t.Parallel()
	cmd := prepare(t.Context(), Cmd{Path: "/bin/sh", Args: []string{"-c", chainScript}}, Bare())
	g, err := grouped(cmd, 0, drainWait, Limits{}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	counts := chainCounts(t, cmd)
	first, made := -1, 0
	within(t, "the chain's links", func() {
		for n := range counts {
			if first < 0 {
				first = n
			}
			if made = first - n + 1; made >= chainOutlives {
				return
			}
		}
	})
	within(t, "the group's wait", func() { _, _ = g.wait() })
	if made < chainOutlives {
		t.Errorf("the chain ended by itself after %d links, want it to make %d or more", made, chainOutlives)
	}
}

// chainCounts starts cmd, a chainScript, with its output on a pipe, and gives each count a link
// writes as it comes. The channel holds every count a run can make before its kill, so the reader
// never waits on it, and is closed once the last link has ended.
func chainCounts(t *testing.T, cmd *process) <-chan int {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	cmd.Stdout = w
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	counts := make(chan int, 1<<17)
	go func() {
		defer close(counts)
		lines := bufio.NewScanner(r)
		for lines.Scan() {
			if n, err := strconv.Atoi(lines.Text()); err == nil {
				counts <- n
			}
		}
	}()
	return counts
}

// linksMore returns once the chain has made chainLinks links past the last count read, or has
// ended, or chainBound has gone with neither.
func linksMore(counts <-chan int) {
	last := -1
	for drained := false; !drained; {
		select {
		case n, ok := <-counts:
			if !ok {
				return
			}
			last = n
		default:
			drained = true
		}
	}
	bound := time.After(chainBound)
	for {
		select {
		case n, ok := <-counts:
			if !ok || last >= 0 && n <= last-chainLinks {
				return
			}
			if last < 0 {
				last = n
			}
		case <-bound:
			return
		}
	}
}
