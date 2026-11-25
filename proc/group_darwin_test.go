package proc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

// forkRuns is how many times the stress test cancels a member mid-fork. On
// the host the fault was found on, 9 runs in 24 left a process behind with
// one kill, so 24 runs with none left is the claim.
const forkRuns = 24

// forkBound is how many children the member's shell forks in its loop, after
// the one it says it forked; it never forks more. An unbounded
// loop, as this test once ran, forks until the kill lands, and when the kill
// never comes, as when the test ends before its cancel, it forks until the
// account's process table is full and every fork on the machine fails. The
// cancel comes as soon as the test reads the shell's line, so the loop has
// only to outlast the cancel's way to the kill, which on an M-series Mac
// lands within the loop's first fork: with a bound of 1, one kill still left
// a process behind in most runs. 256 forks of /bin/sleep take about 220ms
// there, the margin for a host so loaded that the test waits that long to
// cancel. A run whose loop ended before its kill is counted, and the test
// fails if every run's did, so a bound too low for the host is a red, not a
// test that proves nothing. A run holds forkBound + 2 processes at most: the
// shell, the sleep it says it forked, and the loop's.
const forkBound = 256

// forkScript is the member: it forks a sleep and says so on the fifo ($0),
// then forks forkBound ($1) more without pause, says on $2 that its loop
// ended, and waits for its children.
const forkScript = `/bin/sleep 300 & echo > "$0"; i=0; while [ "$i" -lt "$1" ]; do /bin/sleep 300 & i=$((i + 1)); done; echo > "$2"; wait`

// TestACancelMidForkLeavesNoProcessInTheGroup is the mid-fork fault: a member whose
// shell forks without pause is cancelled, and once the group's wait returns
// no process of its group is alive, in every one of forkRuns runs. On macOS
// a process forked while the group's SIGKILL lands is not reached by it, so
// one kill leaves it running, its parent 1, still in the group; the group is
// killed again until no live process answers. Each run waits on a fifo
// until the shell has forked once, so the cancel comes mid-loop, with no
// clock. Whatever ends a run, a failed wait for the fifo included, the
// group is killed before the test returns, and the loop is
// bounded by forkBound for when the test binary is killed itself.
//
// What it costs when this fails: a member cancelled by an interrupt leaves
// processes running that no run will reap or kill.
func TestACancelMidForkLeavesNoProcessInTheGroup(t *testing.T) {
	// serial: each run's shell forks forkBound sleeps without pause, so beside other tests that fork it slows their forks, and theirs can slow the first fork it waits on to the test's deadline
	ran, midFork := 0, 0
	for run := range forkRuns {
		ran++
		dir := t.TempDir()
		fifo, ended := filepath.Join(dir, "forked"), filepath.Join(dir, "ended")
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cmd, g, _ := started(t, ctx, "/bin/sh", "-c", forkScript, fifo, strconv.Itoa(forkBound), ended)
		t.Cleanup(func() {
			cancel()
			if err := g.kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				t.Errorf("run %d: the cleanup's kill of the group -%d: %v", run+1, cmd.Process.Pid, err)
			}
		})
		within(t, "the shell's first fork", func() {
			data, err := os.ReadFile(fifo)
			if err != nil || len(data) == 0 {
				t.Errorf("run %d: the fifo read %q, %v, want the shell's line", run+1, data, err)
			}
		})
		cancel()
		within(t, "the group's wait after the cancel", func() { _, _ = g.wait() })
		if _, err := os.Stat(ended); errors.Is(err, os.ErrNotExist) {
			midFork++
		}
		if err := signalPid(syscall.Kill, -cmd.Process.Pid, 0); err == nil {
			t.Errorf("run %d: a process of the group -%d is alive after its wait returned", run+1, cmd.Process.Pid)
			killed(t, cmd.Process.Pid)
		} else if !errors.Is(err, syscall.ESRCH) && !errors.Is(err, syscall.EPERM) {
			t.Errorf("run %d: the probe of the group -%d: %v, want it gone", run+1, cmd.Process.Pid, err)
		}
	}
	if ran == 0 {
		t.Fatal("no run ran")
	}
	if midFork == 0 {
		t.Errorf("each of the %d runs' loops of %d forks ended before its kill, so none was cancelled mid-fork: raise forkBound", ran, forkBound)
	}
}

// killed kills the group pgid, still alive after its wait returned, again
// until no live process answers, at most forkBound + 2 times: each kill
// reaches every process of the group then, and the shell that forks is the
// first to go, so the group has no more than that to lose.
func killed(t *testing.T, pgid int) {
	t.Helper()
	for range forkBound + 2 {
		if err := signalPid(syscall.Kill, -pgid, syscall.SIGKILL); err != nil {
			return
		}
	}
	t.Errorf("the group -%d still answered after %d kills", pgid, forkBound+2)
}

// TestAGroupOfExitsEndsTheWaitWithItsExitWhateverItsBound: a leader that has exited, not yet
// reaped, answers the cancel's kill of its group with EPERM on macOS, as a group no signal reaches
// does. With Grace and Drain zero the group's bound is zero, and the wait's answer of the exit may
// come after it: here it is held until the ask past the bound has answered. That ask finds the
// exit, so the wait ends with it, reaps the leader with its code, 3, and is not errUnkillable.
//
// What it costs when this fails: a run with a zero Grace and Drain whose program exited as its
// Timeout fell ends `unkillable` at random on macOS, Code -1, its exit lost.
func TestAGroupOfExitsEndsTheWaitWithItsExitWhateverItsBound(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cmd, g, _ := started(t, ctx, "/bin/sh", "-c", "exit 3")
	g.bound = 0
	if err := exited(cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	asked := make(chan struct{})
	g.exitedNow = func(pid int) (bool, error) {
		defer close(asked)
		return exitedNow(pid)
	}
	g.exited = func(pid int) error {
		<-asked
		return exited(pid)
	}
	cancel()
	var killed error
	within(t, "the group's wait", func() { killed, _ = g.wait() })
	if errors.Is(killed, errUnkillable) {
		t.Fatalf("a group of exits with a bound of zero: killed %v, want the leader's exit", killed)
	}
	if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 3 {
		t.Errorf("a group of exits with a bound of zero: state %v, want the leader reaped with code 3", cmd.ProcessState)
	}
}
