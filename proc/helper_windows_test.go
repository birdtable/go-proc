//go:build windows

package proc

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows" //nolint:depguard // arch.ports.23: the tests read the Job Object the package makes, and a console, by the calls Windows makes and reads them with
)

// systemHelper runs a helper mode of Windows alone, and says whether there is one:
//
//   - pipe-nested <pipe> <then>: starts a holding helper, writes its own pid and the holder's to
//     the named pipe, waits for a byte back, then exits 0, or holds.
//   - runner-ok: runs a write helper with Run, inside the job this helper is in, and exits 0 when
//     it ran to its own exit with its output whole.
//   - runner-hold <pipe>: runs pipe-nested <pipe> hold with Run, and lives as long as it does.
//   - breakaway: starts a holding helper asking it to leave this helper's job, and exits 0 when
//     the start is refused.
//   - tests-job <pipe>: stands in for a test binary: starts pipe-nested <pipe> hold through
//     startInTestsJob, as the test binary starts what its tests start, and holds.
//   - busy: writes "busy" and a newline, then spends CPU time until it is killed.
//   - busytree: a detached tree's leader: starts two busy helpers with no console, writes "child1 "
//     and "child2 " and the pid of each, then spends CPU time and writes "cpu " and its own CPU time
//     in nanoseconds after each round, until it is killed; each line ended by a newline.
//   - join: joins a tree of its own by JoinTree, writes "id " and its id, starts a holding helper,
//     writes "child " and its pid, each line ended by a newline, and holds.
//   - stdin-kind: writes what its stdin handle is: "type " and GetFileType's answer, " console "
//     and whether GetConsoleMode reads it, and " process " and whether GetProcessId does, each
//     "yes" or "no", and a newline; or "none" and a newline, for no handle.
//   - out-kind <file>: writes stdin-kind's line for its stdout handle to file.
//   - dup-out: duplicates its stdout handle into its parent, the test binary, writes the handle's
//     value there and a newline, and exits 0, so its parent holds the pipe's write end.
//   - lift-hog <mib>: tries to lift the memory limit of each job it is in, by the job handles its
//     parent holds, then allocates mib MiB (liftHog).
//
// and heldModes's, consoleModes's and graceModes's.
func systemHelper(mode string, args []string) (int, bool) {
	switch {
	case mode == "pipe-nested" && len(args) == 2:
		return pipeNested(args[0], args[1]), true
	case mode == "runner-ok" && len(args) == 0:
		res, err := runner("write", "3", "0", "exit")
		if err == nil && (res.Code != 0 || string(res.Stdout) != "ooo") {
			err = fmt.Errorf("the run inside a job gave %q and Code %d, want ooo and 0", res.Stdout, res.Code)
		}
		return wrote(0, err), true
	case mode == "runner-hold" && len(args) == 1:
		res, err := runner("pipe-nested", args[0], "hold")
		if err != nil {
			return wrote(0, err), true
		}
		return res.Code, true
	case mode == "breakaway" && len(args) == 0:
		return breakaway(), true
	case mode == "tests-job" && len(args) == 1:
		return testsJobStandIn(args[0]), true
	case mode == "busy" && len(args) == 0:
		return busy(), true
	case mode == "busytree" && len(args) == 0:
		return busyTree(), true
	case mode == "join" && len(args) == 0:
		return joinOwnTree(), true
	case mode == "stdin-kind" && len(args) == 0:
		return stdinKind(), true
	case mode == "out-kind" && len(args) == 1:
		return wrote(0, os.WriteFile(args[0], []byte(handleKind(windows.STD_OUTPUT_HANDLE)), 0o600)), true
	case mode == "dup-out" && len(args) == 0:
		return dupOut(), true
	case mode == "lift-hog" && len(args) == 1:
		mib, err := strconv.Atoi(args[0])
		if err != nil {
			return 2, true
		}
		return liftHog(mib), true
	}
	return heldModes(mode, args)
}

// testsJobStandIn is the tests-job helper. It is a helper, so startTestProcess would start plainly
// here; it calls startInTestsJob itself, as the test binary proper does.
func testsJobStandIn(pipe string) int {
	exe, err := os.Executable()
	if err != nil {
		return wrote(0, err)
	}
	child := &process{Path: exe, Args: []string{exe, helperMode + "pipe-nested", pipe, "hold"}, Stdout: os.Stdout, Stderr: os.Stderr}
	if err := startInTestsJob(child); err != nil {
		return wrote(0, err)
	}
	return then("hold")
}

// pipeNested is the pipe-nested helper.
func pipeNested(pipe, how string) int {
	child, err := helping("hold")
	if err != nil {
		return wrote(0, err)
	}
	f, err := os.OpenFile(pipe, os.O_RDWR, 0)
	if err != nil {
		return wrote(0, err)
	}
	defer f.Close()
	if code := wrote(fmt.Fprintf(f, "%d %d\n", os.Getpid(), child.Process.Pid)); code != 0 {
		return code
	}
	if _, err := f.Read(make([]byte, 1)); err != nil {
		return wrote(0, err)
	}
	return then(how)
}

// runner runs the test binary as the helper of mode with args, under Run.
func runner(mode string, args ...string) (Result, error) {
	exe, err := os.Executable()
	if err != nil {
		return Result{}, err
	}
	ctx := context.Background()
	return Run(ctx, Cmd{Path: exe, Args: append([]string{helperMode + mode}, args...), Timeout: holdFor, MaxOutput: 1 << 10, Drain: drainWait})
}

// breakaway is the breakaway helper.
func breakaway() int {
	exe, err := os.Executable()
	if err != nil {
		return wrote(0, err)
	}
	child := &process{Path: exe, Args: []string{exe, helperMode + "hold"}, SysProcAttr: &syscall.SysProcAttr{CreationFlags: windows.CREATE_BREAKAWAY_FROM_JOB}}
	err = startTestProcess(child)
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return 0
	}
	if err != nil {
		return wrote(0, err)
	}
	_ = child.Process.Kill()
	_ = child.Wait()
	return wrote(0, errors.New("a child left a job that allows no breakaway"))
}

// emptyEnvChild is the whole environment a child gets from a run whose list is empty, in a process
// with SystemRoot unset: the one entry os/exec adds (addCriticalEnv) to a Windows child whose list
// lacks it, with this process's value, which is none.
func emptyEnvChild() []string { return []string{"SYSTEMROOT="} }

// console is whether this process has a console, which a window shows.
func console() bool {
	f, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// quiet starts a console child with a console no window shows: a console program a process with no
// console starts gets a console of its own, and its window, unless told not to.
func quiet(c *process) {
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
}

// endTestProcess ends c, a process a test started, by TerminateProcess: os.Process's Kill on
// Windows. Windows ends a process by its handle, not a pid, so no pid guard is asked.
func endTestProcess(c *process) error {
	return c.Process.Kill()
}

// burn spends CPU time on rounds of arithmetic from x, and gives what it computed, which the next
// round starts from, so no round is left out.
func burn(x uint64, rounds int) uint64 {
	for i := range rounds {
		x = x*6364136223846793005 + uint64(i)
	}
	return x
}

// busy is the busy helper.
func busy() int {
	if code := wrote(os.Stdout.WriteString("busy\n")); code != 0 {
		return code
	}
	var x uint64
	for {
		x = burn(x, 1<<20)
	}
}

// ownCPU is the CPU time, user and kernel, this process has used.
func ownCPU() (time.Duration, error) {
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(windows.CurrentProcess(), &created, &exited, &kernel, &user); err != nil {
		return 0, err
	}
	return (ticks(kernel) + ticks(user)) * 100, nil
}

// ticks is a FILETIME that holds a span, not a date, in its 100-nanosecond units.
func ticks(f windows.Filetime) time.Duration {
	return time.Duration(uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime))
}

// busyTree is the busytree helper. Its children have no console, so none opens and no console host
// joins the tree.
func busyTree() int {
	exe, err := os.Executable()
	if err != nil {
		return wrote(0, err)
	}
	for i := 1; i <= 2; i++ {
		child := &process{Path: exe, Args: []string{exe, helperMode + "busy"}, SysProcAttr: &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS}}
		if err := startTestProcess(child); err != nil {
			return wrote(0, err)
		}
		if code := wrote(fmt.Fprintf(os.Stdout, "child%d %d\n", i, child.Process.Pid)); code != 0 {
			return code
		}
	}
	var x uint64
	for {
		x = burn(x, 1<<20)
		cpu, err := ownCPU()
		if err != nil {
			return wrote(0, err)
		}
		if code := wrote(fmt.Fprintf(os.Stdout, "cpu %d\n", cpu.Nanoseconds())); code != 0 {
			return code
		}
	}
}

// joinOwnTree is the join helper.
func joinOwnTree() int {
	id, err := JoinTree()
	if err != nil {
		return wrote(0, err)
	}
	if code := wrote(fmt.Fprintf(os.Stdout, "id %s\n", id)); code != 0 {
		return code
	}
	child, err := helping("hold")
	if err != nil {
		return wrote(0, err)
	}
	if code := wrote(fmt.Fprintf(os.Stdout, "child %d\n", child.Process.Pid)); code != 0 {
		return code
	}
	return then("hold")
}

// sendsTerm, whether a run without Terminal is sent a signal it can catch before the kill, and
// boundsEveryRead are what this system does, as the tests know it apart from what Run reads
// (graceHeld, unheldBounded), so an answer wrong for its system is red. A run with Terminal from a
// process with a console is sent CTRL_BREAK_EVENT here: grace_windows_test.go's rows prove it.
const (
	sendsTerm       = false
	boundsEveryRead = true
)

// conTimeout is the Timeout of the run a con-run helper makes: the probe holds, so the run reaches it.
const conTimeout = 3 * time.Second

// consoleModes runs the helper modes of a run with no console of its caller's, and says whether
// mode is one:
//
//   - con-run <with>: writes "caller " and its own pid; runs con-probe under Run with a Timeout of
//     conTimeout, which the probe's hold reaches, and without Terminal when with is "1"; then writes
//     what the probe and its grandchild wrote, and "end " and how the run ended: "deadline" for
//     Ended{WhyDeadline} with Code -1 and no WhyEscaped, else the error, quoted, the code and the
//     stderr.
//   - con-probe: writes "conin " and what opening CONIN$ gave, "ok" or the error; "self " and its
//     pid; "console " and the pids attached to its console, or "none"; "window " and whether its
//     console has a window, "some" or "none"; starts con-grand plainly, as a program starts a console
//     program, with its own stdout and stderr, which holds the run's output; writes "grandchild "
//     and its pid; and holds.
//   - con-grand: writes "gconsole " and the pids attached to its console, or "none", and "gwindow "
//     and whether its console has a window, "some" or "none"; and holds.
//   - con-refused <dir>: opens CONIN$, its console's input, and runs a touch helper of a file in
//     dir under Run with that as its Stdin, without Terminal, then with it; writes "with " and
//     "refused" when the first is refused with errTerminalStdin, ErrRefused and Code -1 and made no
//     file, else what it gave, and "without " and "ran" when the second made its file, else what it
//     gave.
//
// Each line is ended by a newline.
func consoleModes(mode string, args []string) (int, bool) {
	switch {
	case mode == "con-run" && len(args) == 1:
		return conRun(args[0] == "1"), true
	case mode == "con-probe" && len(args) == 0:
		return conProbe(), true
	case mode == "con-refused" && len(args) == 1:
		return conRefused(args[0]), true
	case mode == "con-grand" && len(args) == 0:
		if code := wrote(fmt.Fprintf(os.Stdout, "gconsole %s\ngwindow %s\n", attached(), window())); code != 0 {
			return code, true
		}
		return then("hold"), true
	}
	return graceModes(mode, args)
}

// conRun is the con-run helper.
func conRun(noTerminal bool) int {
	if code := wrote(fmt.Fprintf(os.Stdout, "caller %d\n", os.Getpid())); code != 0 {
		return code
	}
	exe, err := os.Executable()
	if err != nil {
		return wrote(0, err)
	}
	c := Cmd{Path: exe, Args: []string{helperMode + "con-probe"}, Timeout: conTimeout, MaxOutput: 1 << 10, Drain: drainWait, Terminal: !noTerminal}
	res, err := Run(context.Background(), c)
	if code := wrote(os.Stdout.Write(res.Stdout)); code != 0 {
		return code
	}
	how := "deadline"
	// errors.Is reads every Ended the run joined, where endedWith reads the first: an escape comes
	// after the deadline.
	if !errors.Is(err, Ended{WhyDeadline}) || errors.Is(err, Ended{WhyEscaped}) || res.Code != -1 {
		how = fmt.Sprintf("%q, Code %d, stderr %q", fmt.Sprint(err), res.Code, res.Stderr)
	}
	return wrote(fmt.Fprintf(os.Stdout, "end %s\n", how))
}

// conProbe is the con-probe helper.
func conProbe() int {
	said := "ok"
	if f, err := os.OpenFile("CONIN$", os.O_RDWR, 0); err != nil {
		said = err.Error()
	} else {
		_ = f.Close()
	}
	if code := wrote(fmt.Fprintf(os.Stdout, "conin %s\nself %d\nconsole %s\nwindow %s\n", said, os.Getpid(), attached(), window())); code != 0 {
		return code
	}
	exe, err := os.Executable()
	if err != nil {
		return wrote(0, err)
	}
	child := &process{Path: exe, Args: []string{exe, helperMode + "con-grand"}, Stdout: os.Stdout, Stderr: os.Stderr}
	if err := startTestProcess(child); err != nil {
		return wrote(0, err)
	}
	if code := wrote(fmt.Fprintf(os.Stdout, "grandchild %d\n", child.Process.Pid)); code != 0 {
		return code
	}
	return then("hold")
}

// attached is the pids of the processes attached to this process's console, as
// GetConsoleProcessList gives them, each after a space, or "none" when it has no console.
func attached() string {
	list := make([]uint32, 64)
	n, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleProcessList").Call(uintptr(unsafe.Pointer(&list[0])), uintptr(len(list))) //nolint:gosec // G103: the call writes the pids to the list, by its address
	if n == 0 {
		return "none"
	}
	pids := make([]string, 0, min(int(n), len(list)))
	for _, pid := range list[:min(int(n), len(list))] {
		pids = append(pids, strconv.FormatUint(uint64(pid), 10))
	}
	return strings.Join(pids, " ")
}

// window is whether this process's console has a window, as GetConsoleWindow says: "some" or "none".
func window() string {
	if w, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow").Call(); w != 0 {
		return "some"
	}
	return "none"
}

// conRefused is the con-refused helper.
func conRefused(dir string) int {
	conin, err := os.OpenFile("CONIN$", os.O_RDWR, 0)
	if err != nil {
		return wrote(0, err)
	}
	defer conin.Close()
	exe, err := os.Executable()
	if err != nil {
		return wrote(0, err)
	}
	said := map[bool]string{}
	for _, noTerminal := range []bool{true, false} {
		path := filepath.Join(dir, strconv.FormatBool(noTerminal))
		c := Cmd{Path: exe, Args: []string{helperMode + "touch", path}, Stdin: conin, Timeout: holdFor, MaxOutput: 1 << 10, Drain: drainWait, Terminal: !noTerminal}
		res, err := Run(context.Background(), c)
		_, statErr := os.Stat(path)
		said[noTerminal] = fmt.Sprintf("%q, Code %d, file %v, stderr %q", fmt.Sprint(err), res.Code, statErr, res.Stderr)
		switch {
		case noTerminal && errors.Is(err, errTerminalStdin) && errors.Is(err, ErrRefused) && res.Code == -1 && errors.Is(statErr, os.ErrNotExist):
			said[noTerminal] = "refused"
		case !noTerminal && err == nil && res.Code == 0 && statErr == nil:
			said[noTerminal] = "ran"
		}
	}
	return wrote(fmt.Fprintf(os.Stdout, "with %s\nwithout %s\n", said[true], said[false]))
}

// inherited is what a program is told of f, the first of its run's ExtraFiles: its handle's value,
// once the handle is marked inheritable, as Cmd.ExtraFiles asks of the caller.
func inherited(t testing.TB, f *os.File) string {
	t.Helper()
	h := windows.Handle(f.Fd())
	if err := windows.SetHandleInformation(h, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
		t.Fatal(err)
	}
	return strconv.FormatUint(uint64(h), 10)
}

// yesNo is "yes" for ok, else "no".
func yesNo(ok bool) string {
	if ok {
		return "yes"
	}
	return "no"
}

// stdinKind is the stdin-kind helper.
func stdinKind() int { return wrote(os.Stdout.WriteString(handleKind(windows.STD_INPUT_HANDLE))) }

// handleKind is the line stdin-kind writes, for the standard handle which.
func handleKind(which uint32) string {
	h, err := windows.GetStdHandle(which)
	if err != nil || h == 0 || h == windows.InvalidHandle {
		return "none\n"
	}
	kind, _ := windows.GetFileType(h)
	var mode uint32
	console := windows.GetConsoleMode(h, &mode) == nil
	_, perr := windows.GetProcessId(h)
	return fmt.Sprintf("type %d console %s process %s\n", kind, yesNo(console), yesNo(perr == nil))
}

// dupOut is the dup-out helper.
func dupOut() int {
	parent, err := windows.OpenProcess(windows.PROCESS_DUP_HANDLE, false, uint32(os.Getppid()))
	if err != nil {
		return wrote(0, err)
	}
	defer func() { _ = windows.CloseHandle(parent) }()
	var dup windows.Handle
	if err := windows.DuplicateHandle(windows.CurrentProcess(), windows.Handle(os.Stdout.Fd()), parent, &dup, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return wrote(0, err)
	}
	return wrote(fmt.Fprintf(os.Stdout, "%d\n", dup))
}

// handleSpan is how far liftHog looks among its caller's handle values, which Windows gives in
// steps of 4 from 4: far past the few hundred a test binary holds.
const handleSpan = 1 << 18

// liftHog is the lift-hog helper: it does as TestATreeProcessLiftsItsJobsMemoryLimitAsLimitsSays says,
// then allocates mib MiB as hog does and writes "done".
func liftHog(mib int) int {
	if code := liftAll(); code != 0 {
		return code
	}
	held := hog(mib)
	code := wrote(os.Stdout.WriteString("done\n"))
	runtime.KeepAlive(held)
	return code
}

// liftAll is liftHog's steps before the allocation.
func liftAll() int {
	caller, err := windows.OpenProcess(windows.PROCESS_DUP_HANDLE, false, uint32(os.Getppid()))
	if err != nil {
		return wrote(fmt.Fprintf(os.Stdout, "caller refused %v\n", err))
	}
	defer windows.CloseHandle(caller)
	if code := wrote(os.Stdout.WriteString("caller opened\n")); code != 0 {
		return code
	}
	found := 0
	self := windows.CurrentProcess()
	for v := uintptr(4); v < handleSpan; v += 4 {
		var h windows.Handle
		if windows.DuplicateHandle(caller, windows.Handle(v), self, &h, 0, false, windows.DUPLICATE_SAME_ACCESS) != nil {
			continue
		}
		if pids, err := listed(h); err == nil && slices.Contains(pids, os.Getpid()) {
			found++
			if code := wrote(fmt.Fprintf(os.Stdout, "job %#x %s\n", v, lifted(h))); code != 0 {
				return code
			}
		}
		_ = windows.CloseHandle(h)
	}
	return wrote(fmt.Fprintf(os.Stdout, "found %d\n", found))
}

// lifted clears JOB_OBJECT_LIMIT_JOB_MEMORY on job, and says "lifted", or "refused" and why.
func lifted(job windows.Handle) string {
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if err := queryJob(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil { //nolint:gosec // G103: the job call reads the struct Windows defines, by its address and size
		return "refused reading its limits: " + err.Error()
	}
	if info.BasicLimitInformation.LimitFlags&windows.JOB_OBJECT_LIMIT_JOB_MEMORY == 0 {
		return "holds no memory limit"
	}
	was := strconv.FormatUint(uint64(info.JobMemoryLimit), 10)
	info.BasicLimitInformation.LimitFlags &^= windows.JOB_OBJECT_LIMIT_JOB_MEMORY
	info.JobMemoryLimit = 0
	if err := setJob(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil { //nolint:gosec // G103: the job call takes the struct Windows defines, by its address and size
		return "refused lifting its limit of " + was + ": " + err.Error()
	}
	return "lifted from " + was
}

// graceModes runs the helper modes of a Grace on Windows, and says whether mode is one:
//
//   - brk-run <how> <grace>: runs brk-child <how> under Run with Terminal and a Grace of grace,
//     a Go duration, and ends the run's context once the child writes "up"; then writes "out " and
//     what the child wrote, quoted, "end " and how the run ended: "cancelled" for
//     Ended{WhyCancelled} with Code -1 and nothing else in the error, else the error, quoted, and
//     the code; and "took " and the milliseconds from the context's end to Run's return.
//   - brk-child <how>: catches the break, as os/signal gives it (os.Interrupt), writes "up", and on
//     the break writes "broken"; then exits 0 when how is "catch", and holds when it is "ignore".
//   - brk-refused <dir>: writes "console " and whether this helper has a console, "some" or
//     "none"; runs a touch helper of a file in dir under Run with Terminal and a Grace of a second,
//     and writes "run " and "refused" when it is refused with errNoGrace, ErrNotHeld, ErrRefused and
//     errors.ErrUnsupported, Code -1, no file, and Holds gives the same, else what each gave.
//   - brk-nowindow: starts brk-child catch with a console of its own that no window shows
//     (CREATE_NO_WINDOW) in a group of its own, as Run starts a run without Terminal, sends that
//     group CTRL_BREAK_EVENT once it writes "up", and writes "reached" when it writes "broken"
//     within breakSlack, else "not reached".
//
// Each line is ended by a newline.
func graceModes(mode string, args []string) (int, bool) {
	switch {
	case mode == "brk-run" && len(args) == 2:
		return brkRun(args[0], args[1]), true
	case mode == "brk-child" && len(args) == 1:
		return brkChild(args[0] == "catch"), true
	case mode == "brk-refused" && len(args) == 1:
		return brkRefused(args[0]), true
	case mode == "brk-nowindow" && len(args) == 0:
		return brkNoWindow(), true
	}
	return 0, false
}

// upWriter keeps what it is given, and calls up once it has been given "up" and a newline: Run calls
// its Write from one goroutine, never two at once.
type upWriter struct {
	got  bytes.Buffer
	up   func()
	said bool
}

func (w *upWriter) Write(p []byte) (int, error) {
	w.got.Write(p)
	if !w.said && strings.Contains(w.got.String(), "up\n") {
		w.said = true
		w.up()
	}
	return len(p), nil
}

// brkRun is the brk-run helper.
func brkRun(how, graceArg string) int {
	grace, err := time.ParseDuration(graceArg)
	if err != nil {
		return wrote(0, err)
	}
	exe, err := os.Executable()
	if err != nil {
		return wrote(0, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var cut time.Time
	w := &upWriter{up: func() { cut = time.Now(); cancel() }}
	c := Cmd{Path: exe, Args: []string{helperMode + "brk-child", how}, Timeout: longEnough, MaxOutput: 1 << 10, Drain: drainWait, Terminal: true, Grace: grace, Stdout: w}
	res, err := Run(ctx, c)
	took := time.Since(cut)
	end := "cancelled"
	// The run's error is the two a cancel gives, Ended{WhyCancelled} and the context's cause, and
	// nothing else: a break that was not sent would be a third.
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok || len(joined.Unwrap()) != 2 || !errors.Is(joined.Unwrap()[0], Ended{WhyCancelled}) || !errors.Is(joined.Unwrap()[1], context.Canceled) || res.Code != -1 {
		end = fmt.Sprintf("%q, Code %d", fmt.Sprint(err), res.Code)
	}
	return wrote(fmt.Fprintf(os.Stdout, "out %q\nend %s\ntook %d\n", res.Stdout, end, took.Milliseconds()))
}

// brkChild is the brk-child helper.
func brkChild(exit bool) int {
	broke := make(chan os.Signal, 1)
	signal.Notify(broke, os.Interrupt)
	if code := wrote(fmt.Fprintln(os.Stdout, "up")); code != 0 {
		return code
	}
	<-broke
	if code := wrote(fmt.Fprintln(os.Stdout, "broken")); code != 0 || exit {
		return code
	}
	return then("hold")
}

// brkRefused is the brk-refused helper.
func brkRefused(dir string) int {
	has := "none"
	if hasConsole() {
		has = "some"
	}
	exe, err := os.Executable()
	if err != nil {
		return wrote(0, err)
	}
	path := filepath.Join(dir, "made")
	c := Cmd{Path: exe, Args: []string{helperMode + "touch", path}, Timeout: longEnough, MaxOutput: 1 << 10, Drain: drainWait, Terminal: true, Grace: time.Second}
	_, held := Holds(c)
	res, err := Run(context.Background(), c)
	_, statErr := os.Stat(path)
	run := "refused"
	if !graceRefused(err) || !graceRefused(held) || res.Code != -1 || !errors.Is(statErr, os.ErrNotExist) {
		run = fmt.Sprintf("Run %q, Code %d, file %v, Holds %q", fmt.Sprint(err), res.Code, statErr, fmt.Sprint(held))
	}
	return wrote(fmt.Fprintf(os.Stdout, "console %s\nrun %s\n", has, run))
}

// graceRefused is whether err is the refusal of a Grace this run is not held to: errNoGrace,
// ErrNotHeld, ErrRefused and errors.ErrUnsupported.
func graceRefused(err error) bool {
	return errors.Is(err, errNoGrace) && errors.Is(err, ErrNotHeld) && errors.Is(err, ErrRefused) && errors.Is(err, errors.ErrUnsupported)
}

// brkNoWindow is the brk-nowindow helper.
func brkNoWindow() int {
	exe, err := os.Executable()
	if err != nil {
		return wrote(0, err)
	}
	child := &process{Path: exe, Args: []string{exe, helperMode + "brk-child", "catch"}, SysProcAttr: &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP}}
	out, err := child.StdoutPipe()
	if err != nil {
		return wrote(0, err)
	}
	if err := startTestProcess(child); err != nil {
		return wrote(0, err)
	}
	defer func() { _ = endTestProcess(child); _ = child.Wait() }()
	lines := make(chan string)
	go func() {
		defer close(lines)
		for s := bufio.NewScanner(out); s.Scan(); {
			lines <- s.Text()
		}
	}()
	if line := <-lines; line != "up" {
		return wrote(0, fmt.Errorf("the child wrote %q, want up", line))
	}
	said := "not reached"
	if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(child.Process.Pid)); err != nil {
		said = "not sent: " + err.Error()
	} else {
		late := time.NewTimer(breakSlack)
		defer late.Stop()
		select {
		case line := <-lines:
			if line == "broken" {
				said = "reached"
			}
		case <-late.C:
		}
	}
	return wrote(fmt.Fprintf(os.Stdout, "%s\n", said))
}

// heldModes runs the helper modes of a start whose caller a test kills, and consoleModes's, and
// says whether mode is one:
//
//   - detach-at <stage> <binary> [inherit-all]: starts a tree by detachWith whose leader is outlive
//     with this helper's pid and binary, its output this helper's stdout; at the startStage named
//     stage it writes "held " and the leader's pid and a newline, and holds there while holdWhile
//     does. With inherit-all it first starts hold-while with binary by CreateProcess with every
//     inheritable handle of this helper handed on, and writes "inheriting " and that process's pid
//     and a newline.
//   - outlive <pid> <binary>: waits for process pid to end, writes "outlived " and its own pid and a
//     newline, then holds while holdWhile does.
//   - race <start|run> <binary>: writes "go" and a newline, then starts a holding helper by
//     detachWith (start), which writes "guarded ", the leader's pid, a space and the time since the
//     go, and a newline once the leader is in its guard, then "resumed" and a newline once it is
//     resumed, before the guard is lifted, and holds once the tree is whole, before
//     detachWith returns; or by Run (run), as runner does.
func heldModes(mode string, args []string) (int, bool) {
	switch {
	case mode == "detach-at" && len(args) == 2:
		return detachAt(startStage(args[0]), args[1], false), true
	case mode == "detach-at" && len(args) == 3 && args[2] == "inherit-all":
		return detachAt(startStage(args[0]), args[1], true), true
	case mode == "outlive" && len(args) == 2:
		return outlive(args[0], args[1]), true
	case mode == "race" && len(args) == 2:
		return race(args[0], args[1]), true
	}
	return consoleModes(mode, args)
}

// detachAt is the detach-at helper.
func detachAt(stage startStage, binary string, inherit bool) int {
	exe, err := os.Executable()
	if err != nil {
		return wrote(0, err)
	}
	cmd := exec.Command(exe, helperMode+"outlive", strconv.Itoa(os.Getpid()), binary) //nolint:gosec,noctx // the test binary itself, as a helper; the tree outlives every context
	cmd.Env, cmd.Stdout, cmd.Stderr = Bare(), os.Stdout, os.Stdout
	code := 0
	_, err = detachWith(cmd, func(s startStage, pid int) {
		if s != stage {
			return
		}
		if inherit {
			process, holder, err := startInheritingAll("hold-while", binary)
			if err != nil {
				code = wrote(0, err)
				return
			}
			_ = windows.CloseHandle(process) // the test ends it, by its pid; the binary's end ends it too
			if code = wrote(fmt.Fprintf(os.Stdout, "inheriting %d\n", holder)); code != 0 {
				return
			}
		}
		if code = wrote(fmt.Fprintf(os.Stdout, "held %d\n", pid)); code == 0 {
			code = holdWhile(binary)
		}
	})
	if err != nil {
		return wrote(0, err)
	}
	return code
}

// outlive is the outlive helper.
func outlive(starter, binary string) int {
	pid, err := strconv.Atoi(starter)
	if err != nil {
		return wrote(0, fmt.Errorf("the starter's pid %q: %w", starter, err))
	}
	end, err := processEnd(pid)
	if err != nil {
		return wrote(0, err)
	}
	if err := <-end; err != nil {
		return wrote(0, err)
	}
	if code := wrote(fmt.Fprintf(os.Stdout, "outlived %d\n", os.Getpid())); code != 0 {
		return code
	}
	return holdWhile(binary)
}

// race is the race helper.
func race(kind, binary string) int {
	exe, err := os.Executable()
	if err != nil {
		return wrote(0, err)
	}
	if code := wrote(os.Stdout.WriteString("go\n")); code != 0 {
		return code
	}
	switch kind {
	case "start":
		cmd := exec.Command(exe, helperMode+"hold-while", binary) //nolint:gosec,noctx // the test binary itself, as a helper; the tree outlives every context
		cmd.Env = Bare()
		begun := time.Now()
		_, err = detachWith(cmd, func(s startStage, pid int) {
			switch s {
			case stageGuarded:
				_, _ = fmt.Fprintf(os.Stdout, "guarded %d %s\n", pid, time.Since(begun)) // a line not written is a run the test counts without its time
			case stageResumed:
				// Written before the guard's kill on close is lifted: a leader the test finds after
				// this line may outlive the kill, as a detached tree does, and is not judged.
				_, _ = os.Stdout.WriteString("resumed\n") // one not written leaves the leader judged, and it outlives only after the write
			case stageFreed:
				_ = holdWhile(binary) // the test kills this helper here, or the binary's end ends it
			}
		})
		return wrote(0, err)
	case "run":
		_, err = runner("hold-while", binary)
		return wrote(0, err)
	}
	return wrote(0, fmt.Errorf("race of %q, want start or run", kind))
}
