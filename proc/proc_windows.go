//go:build windows

package proc

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows" //nolint:depguard // arch.ports.23: proc is the host layer for a child's tree, and the Job Object calls are in x/sys alone
	"golang.org/x/term"        //nolint:depguard // arch.shared.7: proc tells a run's Stdin from a terminal, not for styling; x/term's IsTerminal in place of a hand-written copy
)

// A group is a child's Job Object: set to kill every process in it when its last handle closes,
// with no breakaway allowed, so the child and every process it starts stay in it until the job is
// ended. The child is assigned to the job before a single instruction of it runs. A job ends its
// processes by its handle, not by a pid, so a kill after the child is reaped reaches nothing else;
// and when this process dies, its handle closes and the job kills what is in it. That holds a
// program that does not take the job's handle, as Limits.TreeMemory says of its limits: a process
// of the tree may duplicate it from this process and keep it open, which keeps the job and itself
// alive past this process, or allow breakaway or clear kill-on-close by it.
type group struct {
	cmd *process
	job windows.Handle
	mu  sync.Mutex
	// ended is set once the job is closed.
	ended bool
	// cut is set once a cancel ended the job.
	cut bool
	// peak is the job's PeakProcessMemoryUsed, read as the job ends, and peakRead whether it was.
	peak     uint64
	peakRead bool
	// watch reads what the job says of its limits; nil for a job held to none.
	watch *jobWatch
	// left is whether the job held a live process once the child's wait returned (jobLeft).
	left bool
	// grace is how long the child's group has between the CTRL_BREAK_EVENT of a cancel and the
	// job's kill; zero sends none. Run gives one only to a run with Terminal, from a process with a
	// console (graceHeld).
	grace time.Duration
	// running is set once the child is in the job and resumed: a cancel before it sends no break,
	// as run ends the child before it runs.
	running bool
	// late is the job's kill a cancel with a grace leaves for later, fired closed once it has run,
	// and lateErr how it went, read only after fired. unsent is why the break was not sent, the
	// job then killed at once.
	late    *time.Timer
	fired   chan struct{}
	lateErr error
	unsent  error
}

// grouped makes the job for cmd, held to lim, and makes the context's end end it; hit is called each
// time the job says its tree passed a limit. With a grace, which Run gives only a run with Terminal
// from a process with a console, the context's end sends the child's group CTRL_BREAK_EVENT and
// kills the job grace later; with none it kills the job at once. drain has no meaning here: the
// job's kill reaches every process of it. With noTerminal, a run without Terminal, cmd
// starts with a console of its own that no window shows (CREATE_NO_WINDOW), not this process's. DETACHED_PROCESS, Start's, is
// not taken: a process with no console that starts a console program gets that program a console of
// its own, and its window, for each one, where a hidden console is shared by the programs it starts.
func grouped(cmd *process, grace, _ time.Duration, lim Limits, hit func(), noTerminal bool) (*group, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("proc: making the child's job: %w", err)
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	limit(&info, lim)
	if err := setJob(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil { //nolint:gosec // G103: the job call takes the struct Windows defines, by its address and size
		return nil, errors.Join(fmt.Errorf("proc: setting the child's job to kill on close, and its limits: %w", err), windows.CloseHandle(job))
	}
	g := &group{cmd: cmd, job: job, grace: grace}
	if lim != (Limits{}) {
		if g.watch, err = watch(job, hit); err != nil {
			return nil, errors.Join(err, windows.CloseHandle(job))
		}
	}
	flags := uint32(windows.CREATE_SUSPENDED | windows.CREATE_NEW_PROCESS_GROUP)
	if noTerminal {
		flags |= windows.CREATE_NO_WINDOW
	}
	// exec takes no ExtraFiles on Windows: their handles go to the child as Windows inherits them,
	// each as it is (Cmd.ExtraFiles).
	inherited := make([]syscall.Handle, 0, len(cmd.ExtraFiles))
	for _, f := range cmd.ExtraFiles {
		inherited = append(inherited, syscall.Handle(f.Fd()))
	}
	cmd.ExtraFiles = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags, AdditionalInheritedHandles: inherited}
	cmd.Cancel = g.cancel
	return g, nil
}

// setJob sets job's information of class to the struct Windows defines for it, at info and of size
// bytes. The struct goes by its address as a uintptr, which the compiler does not take for a
// pointer: called plainly, x/sys's call leaves the struct on the caller's stack, and a stack grown
// on the way to the system call is copied elsewhere, so Windows reads the old stack, freed, and sets
// whatever it finds there. go:uintptrescapes puts the struct on the heap, which does
// not move, and keeps it live until the call returns. between, given only by a test of the package,
// runs in the call before the system call; production code passes nil.
//
//go:uintptrescapes
func setJob(job windows.Handle, class uint32, info uintptr, size uint32, between func()) error {
	if between != nil {
		between()
	}
	_, err := windows.SetInformationJobObject(job, class, info, size)
	return err
}

// killingJob makes a job with no name, which no other process opens, set to end what is in it when
// its last handle closes. what names the job in an error.
func killingJob(what string) (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("proc: making the %s: %w", what, err)
	}
	if err := killOnClose(job, true); err != nil {
		return 0, errors.Join(fmt.Errorf("proc: setting the %s to kill on close: %w", what, err), windows.CloseHandle(job))
	}
	return job, nil
}

// killOnClose sets job to end what is in it when its last handle closes; with on false it clears
// that, and every other limit of job.
func killOnClose(job windows.Handle, on bool) error {
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if on {
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	}
	return setJob(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil) //nolint:gosec // G103: the job call takes the struct Windows defines, by its address and size
}

// queryJob reads job's information of class into the struct Windows defines for it, at info and of
// size bytes, as setJob sets it, with go:uintptrescapes and between as setJob has them.
//
//go:uintptrescapes
func queryJob(job windows.Handle, class int32, info uintptr, size uint32, between func()) error {
	if between != nil {
		between()
	}
	return windows.QueryInformationJobObject(job, class, info, size, nil)
}

// start starts the child suspended and assigns it to the job as the first call after the start
// returns (startHeld), then resumes it. A child that cannot be assigned is killed before it runs, and
// one whose context ended while it was suspended is ended with the job, before it runs.
func (g *group) start() error {
	started, err := startHeld(g.job, g.cmd)
	switch {
	case !started:
		return errors.Join(err, g.stop())
	case err != nil:
		return errors.Join(err, g.cmd.Process.Kill(), g.cmd.Wait(), g.stop())
	}
	if err := g.run(); err != nil {
		return errors.Join(err, g.stop(), g.cmd.Wait())
	}
	return nil
}

// run resumes the started child, in the job, holding the lock cancel takes. os/exec calls cancel
// from a goroutine of its own as soon as the child has started, so the context's end comes before
// the child is resumed, or once it runs there, and never between, where the job's end would leave
// no thread to resume. When it came before, the job is ended here, which ends the child, suspended
// in it: a cancel that came before the child was assigned ended a job that held nothing yet. The run
// goes on to reap it as a run its context ended.
func (g *group) run() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cut {
		if err := windows.TerminateJobObject(g.job, 1); err != nil {
			return fmt.Errorf("proc: ending the child's job, whose context ended before the child ran: %w", err)
		}
		return nil
	}
	err := resume(uint32(g.cmd.Process.Pid)) //nolint:gosec // G115: a Windows pid is a DWORD, which os keeps as an int
	g.running = err == nil
	return err
}

// startHeld starts cmd, which its SysProcAttr makes suspended, and assigns it to job, which ends
// what is in it when its last handle closes, as the first call after the start returns: no lock, no
// snapshot and no other call comes between. Run's start and Start's both start so. From that call
// on, a caller killed leaves nothing: its handle of job closes as it dies, and job ends the process,
// suspended or not. A caller killed in the window before, from CreateProcess inside exec's Start to
// the assignment, leaves the process suspended with no parent, in no job of its own. Measured on
// GitHub's windows-latest runner with the race detector, the window is about 5 ms of a 6 ms start,
// nearly all of it inside exec's Start: of 100 callers each of Start and of Run killed 0 to 9.9 ms
// after they began, 43 and 46 left a process so, every one killed 1.1 to 6.1 ms in. No order of
// calls after exec's Start can shrink it. A process left so runs nothing, and holds what it was
// given until it is ended: its output, the files handed to it, its program file and its folder. It
// stays suspended, with no parent and out of the job that would end it, until `taskkill /f /pid
// <pid>` ends it or the session it runs in ends: a restart, for a service.
//
// os/exec gives no way to start it inside job. A start of go-proc's own, the process made in job by
// CreateProcess's job list, would close the window; it makes Tree.Leader a type of go-proc's own,
// an API change left to the review before v1.0.0. The start that would close it within os/exec, a
// stub made suspended in job by the job list and the program started by exec with the stub as its
// parent (SysProcAttr.ParentProcess), is not taken: security tools flag a parent set so as a spoofed
// parent, and the program's parent pid would name the ended stub, not the caller. started is whether
// cmd started; an error with it is a process not in job, suspended, which the caller ends and reaps.
func startHeld(job windows.Handle, cmd *process) (started bool, err error) {
	if err := cmd.Start(); err != nil {
		return false, err
	}
	return true, assign(job, cmd)
}

// assign assigns the started child of cmd to job.
func assign(job windows.Handle, cmd *process) error {
	var assigned error
	err := cmd.Process.WithHandle(func(h uintptr) {
		assigned = windows.AssignProcessToJobObject(job, windows.Handle(h))
	})
	if err = cmp.Or(err, assigned); err != nil {
		return fmt.Errorf("proc: assigning the child to its job: %w", err)
	}
	return nil
}

// cancel is the context's end: unless the job is closed, it ends every process in the job, or with
// a grace sends the child's group CTRL_BREAK_EVENT and ends the job once the grace is gone. A break
// that cannot be sent ends the job at once, and why is the run's error (unsent). A child not yet
// running is sent no break: run ends it before it runs.
func (g *group) cancel() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ended {
		return os.ErrProcessDone
	}
	g.cut = true
	if g.grace == 0 || g.late != nil || !g.running {
		return g.terminate()
	}
	if err := g.sendBreak(); err != nil {
		if !errors.Is(err, os.ErrProcessDone) {
			g.unsent = err
		}
		return g.terminate()
	}
	fired := make(chan struct{})
	g.fired = fired
	g.late = time.AfterFunc(g.grace, func() {
		defer close(fired)
		g.lateErr = g.kill()
	})
	return nil
}

// sendBreak sends CTRL_BREAK_EVENT to the child's group, whose id is the child's pid, as the child
// leads a group of its own (CREATE_NEW_PROCESS_GROUP), holding the child's handle while it sends so
// the pid names no other process. A group id of 0 would be every process on this console, this
// one with them, so none is sent to it. Windows sends the break to the processes of the group that
// share this process's console: a process of the tree that made a group or a console of its own is
// sent none, and is ended with the job. A child that has exited is os.ErrProcessDone.
func (g *group) sendBreak() error {
	pid := g.cmd.Process.Pid
	if pid <= 0 {
		return fmt.Errorf("proc: the child's pid is %d, which names no group of its own, so it is sent no CTRL_BREAK_EVENT and its job is ended at once", pid)
	}
	var sent error
	err := g.cmd.Process.WithHandle(func(uintptr) {
		sent = windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(pid)) //nolint:gosec // G115: a Windows pid is a DWORD, which os keeps as an int
	})
	if errors.Is(err, os.ErrProcessDone) {
		return err
	}
	if err = cmp.Or(err, sent); err != nil {
		return fmt.Errorf("proc: sending the child's group CTRL_BREAK_EVENT, so its job was ended at once, with no grace: %w", err)
	}
	return nil
}

// kill ends every process in the job, unless the job is closed: the job's end a grace left for later.
func (g *group) kill() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ended {
		return os.ErrProcessDone
	}
	return g.terminate()
}

// terminate ends every process in the job, the caller holding the lock.
func (g *group) terminate() error {
	if err := windows.TerminateJobObject(g.job, 1); err != nil {
		return fmt.Errorf("proc: ending the child's job: %w", err)
	}
	return nil
}

// wasCut is whether a cancel ended the job.
func (g *group) wasCut() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cut
}

// wait reaps the child, reads whether the job still holds a live process, then ends and closes the
// job, so a process the child left running ends with it: a child that exits within its grace ends
// the grace, and the rest of its tree is ended then. An end a cancel left for later is stopped, or
// waited for when it has begun. killed is how the job's ends went, a break that could not be sent
// with them, and waited is cmd.Wait's.
func (g *group) wait() (killed, waited error) {
	waited = g.cmd.Wait()
	g.left = jobLeft(g.job, g.cmd.Process.Pid)
	killed = g.stop()
	g.mu.Lock()
	late, fired, unsent := g.late, g.fired, g.unsent
	g.mu.Unlock()
	if late != nil && !late.Stop() {
		<-fired
		killed = errors.Join(killed, gone(g.lateErr))
	}
	return errors.Join(unsent, killed), waited
}

// leftOver is whether the job held a live process once the child's wait returned.
func (g *group) leftOver() bool { return g.left }

// stop reads the job's peak, then ends every process in the job, reads what the job said of its
// limits to its end, and closes the job; a second stop does nothing. The job keeps its peak until
// its handle closes, so the reading, taken first, is the run's whole, whatever the end did.
func (g *group) stop() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ended {
		return nil
	}
	g.ended = true
	var ended error
	g.peak, g.peakRead = jobPeak(g.job)
	if err := windows.TerminateJobObject(g.job, 1); err != nil {
		ended = fmt.Errorf("proc: ending the child's job: %w", err)
	}
	return errors.Join(ended, g.watch.stop(), windows.CloseHandle(g.job))
}

// limited is whether the job said its tree passed a limit, read once the job is stopped.
func (g *group) limited() bool { return g.watch.limited() }

// inCgroup places nothing: the job holds the tree here, and no run has a cgroup.
func (*group) inCgroup(*runCgroup) error { return nil }

// resume resumes the threads of process pid, started suspended with the one thread. os/exec keeps
// the thread's handle to itself, so the thread is found by a snapshot of its owner.
func resume(pid uint32) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("proc: listing the threads to resume the child: %w", err)
	}
	found, err := resumed(snap, pid)
	err = errors.Join(err, windows.CloseHandle(snap))
	if err == nil && found == 0 {
		err = fmt.Errorf("proc: no thread of the child %d to resume", pid)
	}
	return err
}

// resumed resumes each thread of process pid in the snapshot snap, and says how many.
func resumed(snap windows.Handle, pid uint32) (int, error) {
	var te windows.ThreadEntry32
	te.Size = uint32(unsafe.Sizeof(te))
	found := 0
	var err error
	for err = windows.Thread32First(snap, &te); err == nil; err = windows.Thread32Next(snap, &te) {
		if te.OwnerProcessID != pid {
			continue
		}
		th, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, te.ThreadID)
		if err != nil {
			return found, fmt.Errorf("proc: opening the child's thread %d: %w", te.ThreadID, err)
		}
		_, err = windows.ResumeThread(th)
		if err = errors.Join(err, windows.CloseHandle(th)); err != nil {
			return found, fmt.Errorf("proc: resuming the child's thread %d: %w", te.ThreadID, err)
		}
		found++
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return found, fmt.Errorf("proc: listing the child's threads: %w", err)
	}
	return found, nil
}

// bareNames are the names Bare gives a child here: PATH, LANG, the home names, and systemNames.
func bareNames() []string {
	return append([]string{"PATH", "LANG", "USERPROFILE", "HOMEDRIVE", "HOMEPATH"}, systemNames()...)
}

// systemNames are the names a child here needs to start, or to find the system's own files, beyond
// PATH, LANG and the home names: SystemRoot, without which a Windows program may fail to load, and
// ProgramData, without which System32's OpenSSH exits 255 before it writes a byte. Each is
// machine-wide, the same for every account here, so none is the person's.
func systemNames() []string { return []string{"SystemRoot", "ProgramData"} }

// envKey is an environment name as Windows compares it: without case.
func envKey(name string) string { return strings.ToUpper(name) }

// programsOnly is whether Run starts only a program file, .exe or .com by its name: Windows runs a
// batch file through cmd.exe, and a script through a program of its own, each reading its command
// line by rules of its own, not the ones exec quotes Args for, so an Arg such as &calc runs a
// second command.
const programsOnly = true

// graceHeld is whether a run's tree is sent a signal it can catch before the kill, so a Grace
// above zero is held, not refused (errNoGrace): on Windows, a run with terminal alone, while this
// process has a console. A CTRL_BREAK_EVENT reaches only the processes that share the sender's
// console, and a run without Terminal has a console of its own (CREATE_NO_WINDOW).
func graceHeld(terminal bool) bool { return terminal && hasConsole() }

// hasConsole is whether this process has a console: CONOUT$, the console's output, opens only in
// a process attached to one.
func hasConsole() bool {
	f, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0)
	if err != nil {
		return false
	}
	return f.Close() == nil
}

// isTerminal is whether f is a terminal: a console, as term.IsTerminal reads it, by GetConsoleMode,
// which takes a console's input or output and no pipe or file; or the pipe of an MSYS or Cygwin
// pty, which a program under mintty with pseudo consoles off has in place of a console (msysPty). A
// pipe whose name does not come within pipeNameWithin, as none does while a Read is pending on it,
// cannot be told from a pty's, nor can a Stdin whose type, or whose pipe's name, Windows does not
// give: each is the error ptyPipe gives, which wraps ErrRefused.
func isTerminal(f *os.File) (bool, error) {
	raw, err := f.SyscallConn()
	if err != nil {
		return false, nil
	}
	is := false
	var unanswered error
	if err := raw.Control(func(h uintptr) {
		if term.IsTerminal(int(h)) {
			is = true
			return
		}
		is, unanswered = ptyPipe(windows.Handle(h))
	}); err != nil {
		return false, nil
	}
	return is, unanswered
}

// fileNameInfo is FILE_NAME_INFO with room for a name of MAX_PATH UTF-16 units: the name's length
// in bytes, then the name, with no NUL.
type fileNameInfo struct {
	Length uint32
	Name   [windows.MAX_PATH]uint16
}

// ptyPipe is whether h is a pipe whose name, as FileNameInfo gives it, is an MSYS or Cygwin pty's,
// as ptyAnswer reads what Windows answers. The name is asked by pipeName, within pipeNameWithin,
// and one that does not come by then is its error, errPipeNameUnanswered. A type Windows does not
// give, a name it refuses to give, and a name the struct has no room for cannot be told from a
// pty's, and are refused with errStdinUntold. A handle of 0 is no pty's and asks nothing.
func ptyPipe(h windows.Handle) (bool, error) {
	// A handle of 0 is what Windows gives a process that has no stdin, a service's or a detached
	// program's, and os.Stdin is then a file of it; syscall.StartProcess passes the child nothing
	// for it, so the child has nothing to read as a terminal. GetFileType fails on it, and that
	// failure is not a Stdin left untold.
	if h == 0 {
		return false, nil
	}
	kind, err := windows.GetFileType(h)
	return ptyAnswer(err, kind == windows.FILE_TYPE_PIPE, func() (uint32, []uint16, error) {
		info, err := pipeName(h, pipeNameWithin)
		return info.Length, info.Name[:], err
	})
}

// brokenPipe is whether err is a write to a pipe no process reads any more, as a program that
// exited before it read all its stdin leaves it: Windows says so in either of two ways.
func brokenPipe(err error) bool {
	return errors.Is(err, windows.ERROR_BROKEN_PIPE) || errors.Is(err, windows.ERROR_NO_DATA)
}
