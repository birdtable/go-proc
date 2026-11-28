//go:build unix

package proc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix" //nolint:depguard // arch.ports.23: the tty-probe helper reads its session, which package syscall reads on no Linux
)

// systemHelper runs a helper mode of Unix alone, and says whether there is one:
//
//   - escape: starts a holding helper in a session of its own, out of this helper's group, with
//     this helper's stdout and stderr; writes its pid and a newline to stdout, and exits 0.
//
// and ttyModes's, reapModes's, childrenModes's, answerModes's, and nsModes's on the systems that have them.
func systemHelper(mode string, args []string) (int, bool) {
	if code, ok := reapModes(mode, args); ok {
		return code, true
	}
	if code, ok := answerModes(mode, args); ok {
		return code, true
	}
	if code, ok := ttyModes(mode, args); ok {
		return code, true
	}
	if code, ok := childrenModes(mode, args); ok {
		return code, true
	}
	if mode != "escape" || len(args) != 0 {
		return nsModes(mode, args)
	}
	exe, err := os.Executable()
	if err != nil {
		return wrote(0, err), true
	}
	child := &process{Path: exe, Args: []string{exe, helperMode + "hold"}, Stdout: os.Stdout, Stderr: os.Stderr, SysProcAttr: &syscall.SysProcAttr{Setsid: true}}
	if err := child.Start(); err != nil {
		return wrote(0, err), true
	}
	return wrote(fmt.Fprintf(os.Stdout, "%d\n", child.Process.Pid)), true
}

// emptyEnvChild is the whole environment a child gets from a run whose list is empty: none.
func emptyEnvChild() []string { return []string{} }

// console is whether this process has a controlling terminal, which a hangup reaches when the
// terminal closes.
func console() bool {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// quiet starts a child as it is: a Unix child opens no window.
func quiet(*process) {}

// sendsTerm, whether a run without Terminal is sent a signal it can catch before the kill, and
// boundsEveryRead are what this system does, as the tests know it apart from what Run reads
// (graceHeld, unheldBounded), so an answer wrong for its system is red.
const (
	sendsTerm       = true
	boundsEveryRead = false
)

// ttyBackstop is the Timeout of the run a tty-run helper makes, which it reaches only when the probe
// never says it is ready: the run's context ends once the probe says so, long before.
const ttyBackstop = time.Minute

// ttyReady is what the probe writes on its ready pipe, its descriptor 3, once its grandchild holds
// the run's output.
const ttyReady = "ready\n"

// ttyModes runs the helper modes of a run with no terminal, and says whether mode is one:
//
//   - tty-run <with>: writes "ctty " and whether this helper has a controlling terminal, "some" or
//     "none"; runs tty-probe under Run, handing it a ready pipe and ending the run's context once
//     the probe writes ttyReady on it, with a Timeout of ttyBackstop, and without Terminal when with
//     is "1", or without Terminal and with ReadPeak when with is "peak"; then writes what the probe
//     wrote, and "end " and how the run ended: "cancelled" for Ended{WhyCancelled} with Code -1 and
//     no WhyEscaped, else the error, quoted, the code and the stderr. Each line is ended by a
//     newline.
//   - tty-probe: writes "tty " and what opening /dev/tty gave, "ok", "ENXIO" or the error; "self "
//     and its pid, process group and session; starts a holding helper with its own stdout and stderr,
//     which holds the run's output; writes "grandchild " and that helper's pid, group and session;
//     writes ttyReady on its descriptor 3; and holds.
func ttyModes(mode string, args []string) (int, bool) {
	switch {
	case mode == "tty-run" && len(args) == 1:
		return ttyRun(args[0]), true
	case mode == "tty-probe" && len(args) == 0:
		return ttyProbe(), true
	}
	return 0, false
}

// ttyRun is the tty-run helper.
func ttyRun(with string) int {
	word := "none"
	if console() {
		word = "some"
	}
	if code := wrote(fmt.Fprintf(os.Stdout, "ctty %s\n", word)); code != 0 {
		return code
	}
	exe, err := os.Executable()
	if err != nil {
		return wrote(0, err)
	}
	ready, said, err := os.Pipe()
	if err != nil {
		return wrote(0, err)
	}
	defer func() { _ = ready.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		b := make([]byte, len(ttyReady))
		if _, err := io.ReadFull(ready, b); err == nil && string(b) == ttyReady {
			cancel()
		}
	}()
	c := Cmd{
		Path: exe, Args: []string{helperMode + "tty-probe"}, Timeout: ttyBackstop, MaxOutput: 1 << 10, Drain: drainWait,
		Terminal: with == "0", ReadPeak: with == "peak", ExtraFiles: []*os.File{said},
	}
	res, err := Run(ctx, c)
	_ = said.Close()
	if code := wrote(os.Stdout.Write(res.Stdout)); code != 0 {
		return code
	}
	how := "cancelled"
	// errors.Is reads every Ended the run joined, where endedWith reads the first: an escape comes
	// after the cancel.
	if !errors.Is(err, Ended{WhyCancelled}) || errors.Is(err, Ended{WhyEscaped}) || res.Code != -1 {
		how = fmt.Sprintf("%q, Code %d, stderr %q", fmt.Sprint(err), res.Code, res.Stderr)
	}
	return wrote(fmt.Fprintf(os.Stdout, "end %s\n", how))
}

// ttyProbe is the tty-probe helper.
func ttyProbe() int {
	said := "ok"
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	switch {
	case errors.Is(err, syscall.ENXIO):
		said = "ENXIO"
	case err != nil:
		said = err.Error()
	default:
		_ = f.Close()
	}
	if code := wrote(fmt.Fprintf(os.Stdout, "tty %s\nself %s\n", said, sessionOf(os.Getpid()))); code != 0 {
		return code
	}
	child, err := helping("hold")
	if err != nil {
		return wrote(0, err)
	}
	if code := wrote(fmt.Fprintf(os.Stdout, "grandchild %s\n", sessionOf(child.Process.Pid))); code != 0 {
		return code
	}
	if code := wrote(os.NewFile(3, "ready").WriteString(ttyReady)); code != 0 {
		return code
	}
	return then("hold")
}

// sessionOf is process pid's id, its process group's and its session's, as "<pid> <pgid> <sid>"; a
// read that fails is -1.
func sessionOf(pid int) string {
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		pgid = -1
	}
	sid, err := unix.Getsid(pid)
	if err != nil {
		sid = -1
	}
	return fmt.Sprintf("%d %d %d", pid, pgid, sid)
}

// inherited is what a program is told of f, the first of its run's ExtraFiles: its descriptor, 3.
func inherited(_ testing.TB, _ *os.File) string { return "3" }

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
