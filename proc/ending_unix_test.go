//go:build unix

package proc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

// TestOnUnixALostLimitClearsEndOnlyForALeaderTheKillEnded: given the tree's error of a cgroup that
// lost its limit, a reaped leader that exited 0 on its own keeps EndExited and Code 0, and one ended
// by SIGSEGV or SIGTERM keeps EndSignal and its signal, as a loss read once the leader is reaped, in
// the drain or in the tree's last check, kills only what is left of the tree; and a leader its state
// shows ended by SIGKILL, the one signal of the run's kill, reads EndNone, where with no loss it reads
// EndSignal. A SIGKILL from outside with a loss reads EndNone too: it cannot be told from the kill.
//
// What it costs when this fails: a program that exited 0 or died of its own signal reads as one
// go-proc ended, or the run's kill on a lost limit reads as the program's own signal.
func TestOnUnixALostLimitClearsEndOnlyForALeaderTheKillEnded(t *testing.T) {
	t.Parallel()
	lost := fmt.Errorf("%w: the memory controller left the run's cgroup", errLimitLost)
	for _, c := range []struct {
		name, script string
		treeErr      error
		want         EndKind
		sig          os.Signal
		code         int
	}{
		{"a leader that exited 0 before the loss was read", "exit 0", lost, EndExited, nil, 0},
		{"a leader that died of SIGSEGV before the loss was read", "kill -SEGV $$", lost, EndSignal, syscall.SIGSEGV, -1},
		{"a leader another process sent SIGTERM before the loss was read", "kill -TERM $$", lost, EndSignal, syscall.SIGTERM, -1},
		{"a leader the kill on a lost limit ended", "kill -KILL $$", lost, EndNone, nil, -1},
		{"a leader killed with no loss", "kill -KILL $$", nil, EndSignal, syscall.SIGKILL, -1},
	} {
		cmd, g, _ := started(t, t.Context(), "/bin/sh", "-c", c.script)
		var waited error
		within(t, "the group's wait", func() { _, waited = g.wait() })
		end, sig, status := endingOf(cmd, false, c.treeErr)
		if code := exitCode(cmd, false); end != c.want || sig != c.sig || status != 0 || code != c.code {
			t.Errorf("%s (wait %v): End %q, Signal %v, Status %#x, Code %d, want %q, %v and Code %d", c.name, waited, end, sig, status, code, c.want, c.sig, c.code)
		}
	}
}

// TestOnUnixAProgramEndedByASignalFromOutsideReadsEndSignalAndNamesIt: a helper that writes its pid
// and holds, sent SIGTERM by the test, not by the run (no Timeout, no ctx, no cap reached), reads
// EndSignal with Signal SIGTERM and no Status, and Code -1 and the wait's *exec.ExitError, as before.
//
// What it costs when this fails: a caller cannot tell a program another process ended from one
// go-proc refused or ended, which all read Code -1.
func TestOnUnixAProgramEndedByASignalFromOutsideReadsEndSignalAndNamesIt(t *testing.T) {
	t.Parallel()
	res, err, sent := onPid(t, helperCmd(t, "pid-hold"), func(pid int) error {
		return signalPid(syscall.Kill, pid, syscall.SIGTERM)
	})
	_, waited := errors.AsType[*exec.ExitError](err)
	if sent != nil || !waited || res.Code != -1 || res.End != EndSignal || res.Signal != syscall.SIGTERM || res.Status != 0 {
		t.Errorf("a program sent SIGTERM from outside (%v): %v, Code %d, End %q, Signal %v, Status %#x, want the wait's error, Code -1, EndSignal and SIGTERM", sent, err, res.Code, res.End, res.Signal, res.Status)
	}
}

// TestOnUnixARunWhoseWaitFailsReadsEndNoneAndItsError: a helper that ignores SIGCHLD runs a program
// that exits 0. The kernel reaps it, so the run's wait fails with ECHILD and leaves no state, and the
// run reads EndNone, Code -1 and the wait's error, as Code read before End was added.
//
// What it costs when this fails: a call that gave its caller an error crashes the caller's process.
func TestOnUnixARunWhoseWaitFailsReadsEndNoneAndItsError(t *testing.T) {
	t.Parallel()
	res, err := Run(t.Context(), helperCmd(t, "reap-ignored"))
	if want := "end \"\" code -1 err true\n"; err != nil || string(res.Stdout) != want {
		t.Errorf("a run whose wait failed, in a helper: %v, stdout %q, stderr %q, want %q", err, res.Stdout, res.Stderr, want)
	}
}
