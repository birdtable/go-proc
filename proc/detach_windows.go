//go:build windows

package proc

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec" //nolint:depguard // proc is the runner: the leader Start starts is an exec.Cmd
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows" //nolint:depguard // arch.ports.23: the console flags and the Job Object calls are in x/sys alone
)

// jobPrefix starts every id Start gives here: a Job Object's name in this session's namespace, which
// only a process of the session opens, before 32 lowercase hex digits drawn at random.
const jobPrefix = `Local\go-proc-tree-`

// The access rights to a job that x/sys does not name, of winnt.h: JOB_OBJECT_QUERY, to read what
// it used and its processes, and JOB_OBJECT_TERMINATE, to end its processes.
const (
	jobQuery     = 0x0004
	jobTerminate = 0x0008
)

// treeIDHere is how this system names a tree: by a Job Object's name.
const treeIDHere = idJob

// errNoJob is an open of a job name no job has.
var errNoJob = errors.New("no job has the name")

// detachFromTerminal gives the leader no console and a process group of its own, so it outlives the
// window that started it. With no console, none opens: a console program started without the flag
// gets a window of its own that appears and vanishes every time a job starts, or shares the
// caller's, and dies when it closes. The process group keeps Ctrl+C in the caller's window from
// reaching it.
func detachFromTerminal(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP}
}

// detach starts cmd detached, suspended, in a guard job that ends it if this process dies before the
// tree is whole, and in a Job Object made for it under a name drawn at random, and resumes it once it
// is in that job, so every process it starts is in the job too. The tree's job does not end its
// processes when its handles close: once the guard's kill on close is lifted, the tree outlives this
// process. The leader is given a handle of the tree's job, which keeps the name while the leader
// lives, since Windows drops an object's name once its last handle closes; this process closes its
// own before it returns. A name some job already has is not taken: the job is not this one's.
func detach(cmd *process, _ procRoot, _ string) (Tree, error) { return detachWith(cmd, nil) }

// startStage is a step of a detached start once the leader is in its guard job, at which a test of
// the package acts by detachWith's at.
type startStage string

// The steps of a detached start, in order: the leader in its guard job, suspended; given its handle
// of the tree's job; in the tree's job and resumed; and the guard's kill on close lifted, the tree
// then whole.
const (
	stageGuarded startStage = "guarded"
	stageGiven   startStage = "given"
	stageResumed startStage = "resumed"
	stageFreed   startStage = "freed"
)

// detachWith is detach. at, given only by a test of the package, is called at each startStage with
// the leader's pid, before this process closes its handles of the guard and the tree's job;
// production code passes nil.
func detachWith(cmd *process, at func(startStage, int)) (Tree, error) {
	name := jobName()
	job, err := createJob(name)
	if err != nil {
		return Tree{}, err
	}
	guard, err := killingJob("leader's guard job")
	if err == nil {
		err = errors.Join(admit(guard, job, cmd, at), windows.CloseHandle(guard))
	}
	if err = errors.Join(err, windows.CloseHandle(job)); err != nil {
		return Tree{}, err
	}
	return Tree{ID: TreeID(name), Leader: cmd.Process}, nil
}

// jobName is a name for a tree's job: jobPrefix, then 32 lowercase hex digits drawn at random.
func jobName() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error: it ends the program when it cannot read
	return jobPrefix + hex.EncodeToString(b[:])
}

// admit starts cmd suspended and in guard (startHeld), gives the leader its handle of job, assigns
// it to job, resumes it, and lifts guard's kill on close. A leader not started is no tree and
// nothing runs. A leader not put in guard is killed and reaped. A leader in guard that is not given
// its handle, not assigned, not resumed, or whose guard is not lifted, is ended with guard, and with
// what it started, and reaped, so nothing started runs: what it costs is the tree, which Start does
// not give. at is detachWith's.
func admit(guard, job windows.Handle, cmd *process, at func(startStage, int)) error {
	detachFromTerminal(cmd)
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	started, err := startHeld(guard, cmd)
	switch {
	case !started:
		return err
	case err != nil:
		return errors.Join(err, cmd.Process.Kill(), cmd.Wait())
	}
	reached := func(s startStage) {
		if at != nil {
			at(s, cmd.Process.Pid)
		}
	}
	reached(stageGuarded)
	err = give(job, cmd)
	if err == nil {
		reached(stageGiven)
		err = assign(job, cmd)
	}
	if err == nil {
		err = resume(uint32(cmd.Process.Pid)) //nolint:gosec // G115: a Windows pid is a DWORD, which os keeps as an int
	}
	if err == nil {
		reached(stageResumed)
		if err = killOnClose(guard, false); err != nil {
			err = fmt.Errorf("proc: lifting the leader's guard job's kill on close: %w", err)
		}
	}
	if err != nil {
		return errors.Join(err, endGuard(guard, cmd), cmd.Wait())
	}
	reached(stageFreed)
	return nil
}

// endGuard ends every process in guard, the leader among them. A guard that cannot be ended has its
// leader killed by its own handle, so the wait after it returns.
func endGuard(guard windows.Handle, cmd *process) error {
	err := windows.TerminateJobObject(guard, 1)
	if err == nil {
		return nil
	}
	return errors.Join(fmt.Errorf("proc: ending the leader's guard job: %w", err), cmd.Process.Kill())
}

// give puts a handle of job in the leader pid, started and suspended, with the right to wait on the
// job alone and not inheritable, so the job's name lives while the leader does. The handle is put
// there by DuplicateHandle into the leader, and is never one this process marks inheritable: a
// process another goroutine of this process starts with every inheritable handle handed on, as any
// code outside exec may, would take that handle too, keep the job's name after the leader is reaped,
// and so have Stop end the job of a tree whose leader is gone, and Interrupt answer it as live. The
// leader is reached by cmd.Process's own handle, as assign reaches it.
func give(job windows.Handle, cmd *process) error {
	var given error
	err := cmd.Process.WithHandle(func(leader uintptr) {
		var held windows.Handle
		given = windows.DuplicateHandle(windows.CurrentProcess(), job, windows.Handle(leader), &held, windows.SYNCHRONIZE, false, 0)
	})
	if err = errors.Join(err, given); err != nil {
		return fmt.Errorf("proc: giving the leader a handle of its job: %w", err)
	}
	return nil
}

// createJob makes a Job Object named name, and refuses a name a job already has, which CreateJobObject
// would open in its place. x/sys's CreateJobObject does not say which it did, so the call is made
// here, where its last error is read with it.
func createJob(name string) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, fmt.Errorf("proc: the tree's job name: %w", err)
	}
	r, _, callErr := windows.NewLazySystemDLL("kernel32.dll").NewProc("CreateJobObjectW").Call(0, uintptr(unsafe.Pointer(p))) //nolint:gosec // G103: the call takes the name Windows reads, by its address
	job := windows.Handle(r)
	switch {
	case job == 0:
		return 0, fmt.Errorf("proc: making the tree's job: %w", callErr)
	case errors.Is(callErr, windows.ERROR_ALREADY_EXISTS):
		return 0, errors.Join(fmt.Errorf("proc: the tree's job name %s is a job's already, not this one's", name), windows.CloseHandle(job))
	}
	return job, nil
}

// openJob opens the Job Object named name with access. A name no job has is errNoJob.
func openJob(access uint32, name string) (windows.Handle, error) {
	job, err := openJobObject(access, name)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return 0, errNoJob
	}
	return job, err
}

// treeJob opens the job id names with access. An id Start does not give here is errTreeID, and one
// whose job is gone is ErrStale.
func treeJob(id TreeID, access uint32) (windows.Handle, error) {
	if !treeName(id) {
		return 0, errTreeID
	}
	job, err := openJob(access, string(id))
	switch {
	case errors.Is(err, errNoJob):
		return 0, errStale
	case err != nil:
		return 0, fmt.Errorf("proc: opening the job of tree %s: %w", id, err)
	}
	return job, nil
}

// killer is how Stop ends a job: terminate is TerminateJobObject; a test of the package gives one
// that counts.
type killer struct {
	terminate func(job windows.Handle, code uint32) error
}

// ender is the killer Stop uses.
func ender() killer { return killer{terminate: windows.TerminateJobObject} }

// place is "": a tree id here is a Job Object's name, which carries no boot and no namespace.
func place(procRoot) (string, error) { return "", nil }

// stopTree ends the job id names by k. The job's name is gone once no process holds a handle of it,
// the leader's among them, and a name is drawn at random, so a name that opens is the tree's.
func stopTree(id TreeID, k killer) error {
	_, err := inJob(id, jobTerminate, "stopping", func(job windows.Handle) (struct{}, error) { return struct{}{}, k.terminate(job, 1) })
	return err
}

// inJob opens the job id names with access, acts on it by act and closes it. A failed act or close
// is an error that says what it was doing to tree id; one treeJob refuses is treeJob's own.
func inJob[T any](id TreeID, access uint32, doing string, act func(windows.Handle) (T, error)) (T, error) {
	var none T
	job, err := treeJob(id, access)
	if err != nil {
		return none, err
	}
	v, err := act(job)
	if err = errors.Join(err, windows.CloseHandle(job)); err != nil {
		return none, fmt.Errorf("proc: %s tree %s: %w", doing, id, err)
	}
	return v, nil
}

// treeName is whether id is a name Start gives here: jobPrefix, then 32 lowercase hex digits.
func treeName(id TreeID) bool {
	hexed, ok := strings.CutPrefix(string(id), jobPrefix)
	return ok && len(hexed) == 32 && strings.Trim(hexed, "0123456789abcdef") == ""
}

// stopAt is stopTree: a job's name carries no place, so here is not read.
func stopAt(id TreeID, _ procRoot, _ string, k killer) error { return stopTree(id, k) }

// errNoInterrupt is Interrupt of a live tree here: there is no way to send it a signal it can catch.
var errNoInterrupt = fmt.Errorf("a tree Start started has no console (DETACHED_PROCESS), and Windows delivers CTRL_BREAK_EVENT only to processes that share the sender's console, so nothing was sent and the tree runs on: %w", errors.ErrUnsupported)

// interruptAt sends nothing: it checks id and opens its job as stopTree does, with the right to
// query it alone and none to end it, so an id Start does not give is errTreeID and one whose job is
// gone ErrStale, then closes the job and answers errNoInterrupt. k is not called.
func interruptAt(id TreeID, _ procRoot, _ string, _ killer) error {
	_, err := inJob(id, jobQuery, "interrupting", func(windows.Handle) (struct{}, error) { return struct{}{}, errNoInterrupt })
	return err
}
