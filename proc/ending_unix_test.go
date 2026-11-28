//go:build unix

package proc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
)

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

// reapModes runs the reap-ignored mode, and says whether mode and args are it:
//
//   - reap-ignored: ignores SIGCHLD, runs an exit-code 0 helper, and writes "end ", the run's End
//     quoted, " code ", its Code, " err " and whether its error is set, and a newline.
func reapModes(mode string, args []string) (int, bool) {
	if mode != "reap-ignored" || len(args) != 0 {
		return 0, false
	}
	exe, err := os.Executable()
	if err != nil {
		return wrote(0, err), true
	}
	signal.Ignore(syscall.SIGCHLD)
	res, err := Run(context.Background(), Cmd{
		Path: exe, Args: []string{helperMode + "exit-code", "0"}, Timeout: ttyBackstop, MaxOutput: 1 << 10, Drain: drainWait,
	})
	return wrote(fmt.Fprintf(os.Stdout, "end %q code %d err %t\n", res.End, res.Code, err != nil)), true
}
