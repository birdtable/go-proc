//go:build linux && live

package proc

import (
	"bufio"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix" //nolint:depguard // arch.ports.23: the row waits for POLLHUP by poll, whose POLLHUP is in x/sys alone
)

// TestAKilledMemberThatTouchedGigabytesLetsGoOfItsPipesWithinTheTestsDrain: a helper touches 1, 2
// and 4 GiB, three times each, writes done and holds, its stdout and stderr a pipe; once it has
// written, it is sent SIGKILL, as a run's kill sends its tree, and the time from the kill to POLLHUP
// on the pipe's read end, which comes once its write ends are closed, is logged and is within
// drainWait, the Drain the package's tests give. Linux gives back a process's memory before it
// closes its files, so the time grows with what it touched: on a linux-amd64 host of 20 CPUs, 50 to
// 63 ms a GiB, 251 ms at 4 GiB under a load of 47, which is what a Drain shorter than
// that costs such a tree, as Cmd.Drain says. The row is live: it touches gigabytes.
//
// What it costs when this fails: a member the kill ended still holds a run's pipe past drainWait,
// so the package's tests end such a run with WhyEscaped, Left true and its output cut, though
// nothing left its tree.
func TestAKilledMemberThatTouchedGigabytesLetsGoOfItsPipesWithinTheTestsDrain(t *testing.T) {
	t.Parallel()
	for _, mib := range []int{1 << 10, 2 << 10, 4 << 10} {
		for n := range 3 {
			took := killToHangUp(t, mib)
			t.Logf("%d MiB, run %d: kill to POLLHUP %v", mib, n+1, took)
			if took > drainWait {
				t.Errorf("a member that touched %d MiB let go of its pipe %v after the kill, want within the tests' Drain, %v", mib, took, drainWait)
			}
		}
	}
}

// killToHangUp starts hog-hold mib, kills it once it has written done, and gives the time from the
// kill to POLLHUP on its output's read end.
func killToHangUp(t *testing.T, mib int) time.Duration {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	child := &process{Path: exe, Args: []string{exe, helperMode + "hog-hold", strconv.Itoa(mib)}, Stdout: w, Stderr: w}
	err = startTestProcess(child)
	_ = w.Close()
	if err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(r).ReadString('\n'); err != nil || line != "done\n" {
		_ = child.Process.Kill()
		_ = child.Wait()
		t.Fatalf("hog-hold %d wrote %q (%v), want done", mib, line, err)
	}
	begun := time.Now()
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	p := []unix.PollFd{{Fd: int32(r.Fd()), Events: unix.POLLIN}} //nolint:gosec // G115: a descriptor os gave is an int, which poll keeps as an int32
	for {
		if _, err := unix.Poll(p, int(drainWait.Milliseconds())); !errors.Is(err, unix.EINTR) {
			break
		}
	}
	took := time.Since(begun)
	_ = child.Wait()
	if p[0].Revents&unix.POLLHUP == 0 {
		t.Fatalf("hog-hold %d: no POLLHUP within %v of the kill", mib, drainWait)
	}
	return took
}
