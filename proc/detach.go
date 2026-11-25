package proc

import (
	"errors"
	"fmt"
	"os"
	"os/exec" //nolint:depguard // proc is the runner: Start starts the program its caller names, by a path never looked up, with arguments no shell reads
)

// TreeID names a tree Start started, for Stop to end it or Interrupt to ask it to stop, from this
// process or another, as text that passes between them. It names the tree only while the tree's
// leader, the program Start started, is not reaped: on Unix the leader's process group, with the
// leader's start time, so a process that later takes the leader's pid is not taken for it; on
// Windows a Job Object named at random, which the leader holds open. On Linux an id is good only on
// the boot, in the pid namespace and in the time namespace that gave it, which it carries: a pid and
// a start time in ticks since boot name a process within those alone, so an id kept across a
// reboot, handed to another pid namespace, or read in another time namespace is ErrStale, and
// nothing is signalled.
type TreeID string

// Detached is a program to start detached: in a tree of its own, out of this process's session,
// terminal and console, so it outlives both.
type Detached struct {
	// Path is the program, taken and refused as Cmd's Path is: an absolute path, or a relative one
	// with a separator in it (./tool, bin/tool, ..\tool.exe), resolved against Dir, or against this
	// process's folder when Dir is empty, once, as Start starts it. A bare name is refused, and on
	// Windows a drive with no root after it or a root with no drive before it, and a file that is not
	// a program file, as named or as PATHEXT finds it under Dir (ErrBatch, ErrNotProgram).
	Path string
	// Args are the program's arguments after its name, each passed as one argument; no shell reads
	// them.
	Args []string
	// Dir is the folder the program runs in; empty is this process's.
	Dir string
	// Env is the program's whole environment. nil is Bare(), the safe default; an empty one is none.
	Env []string
	// Output is the file the program's stdout and stderr go to; nil, or a closed file, is the null
	// device, the safe default. The program reads nothing: its stdin is the null device.
	Output *os.File
}

// Tree is a tree Start started.
type Tree struct {
	// ID names the tree to Stop.
	ID TreeID
	// Leader is the program Start started. Its Wait reaps it, and ID names nothing from then on. A
	// caller that exits without the wait leaves the reap to the system.
	Leader *os.Process
}

// ErrStale is a Stop or an Interrupt of a tree whose leader is gone, or was reaped: nothing was
// signalled, since the id may name a process that took the leader's pid. On Windows a process of
// the tree that duplicates the job's handle, marks it inheritable or opens the job by name keeps
// the id live past the leader's reap, so a Stop ends what is left of that tree and returns nil.
var ErrStale = errors.New("proc: the tree id names no live tree, since its leader is gone or reaped, and nothing was signalled")

// errStale is what each gate returns for ErrStale: the value ErrStale was given here, so a value a
// caller or a dependency stores in ErrStale later, nil included, changes what a match finds and never
// lets Stop signal.
var errStale = ErrStale

// errNoTreeID is Start, Stop and Interrupt on a Unix with no way here to read a process's start time:
// without it a process that took a reaped leader's pid would be taken for the leader, and its group
// killed.
var errNoTreeID = fmt.Errorf("proc: this system gives no way here to tell a tree's leader from a process that took its pid later, so it starts no detached tree and stops none by id: %w", errors.ErrUnsupported)

// treeIDKind is how a system names a tree for Stop.
type treeIDKind int

const (
	// idNone is a system with no tree.
	idNone treeIDKind = iota
	// idUnread is a Unix whose process table is not read here for a start time.
	idUnread
	// idGroup is a Unix that names a tree by its process group and its leader's start time.
	idGroup
	// idJob is Windows, which names a tree by its Job Object's name.
	idJob
)

// unnamed is why a system of kind k starts no detached tree and stops none by id, or nil.
func unnamed(k treeIDKind) error {
	switch k {
	case idNone:
		return errNoTree
	case idUnread:
		return errNoTreeID
	case idGroup, idJob:
	}
	return nil
}

// errTreeID is a Stop of an id that is not one Start gives on this system.
var errTreeID = errors.New("proc: the tree id is not one Start gives on this system, and nothing was signalled")

// Start starts d's program detached and returns its tree. On Unix the program leads a session of
// its own, so the hangup a terminal sends when it closes does not reach it; on Windows it has no
// console, so none opens, and is in a Job Object of its own. On Windows a caller that is in a job
// cannot start a tree out of it: the tree's job is nested in the caller's, and a caller's job that
// ends its processes ends the tree too, and a console program the tree starts opens a console, and
// its window, unless it is started with CREATE_NO_WINDOW or DETACHED_PROCESS. Every process the
// program starts is in its tree unless it leaves, as on Unix a new session does. A d refused as Run
// refuses a Cmd's Path starts nothing, and so does a system with no tree to name, and on Linux a
// /proc that is not the root of a proc file system or not this pid namespace's own: the error then
// wraps errors.ErrUnsupported. On Linux a failed close of the descriptor that check opened is an
// error returned with the tree, which runs: a caller that has an error and a Tree with an ID stops
// the tree by it, which no later call can name again.
// Interrupt asks a started tree to stop, and Stop kills it.
func Start(d Detached) (Tree, error) {
	return start(d, procFS{dir: procMount})
}

// procMount is the proc mount the Linux guard reads, which Start and Stop first open and check is the
// root of a proc file system and this pid namespace's own (procOwn); no other system reads one.
const procMount = "/proc"

// procFS is the proc mount start and stop read: dir, procMount but in a test of the package. standIn
// takes dir for the root of a proc file system, so a folder of planted files stands in for one past
// that check, and noOpenat2 takes the kernel for one with no openat2. Each is read in procOwn alone,
// and only a test sets it (TestNoProgramThatImportsProcLinksTesting).
type procFS struct {
	dir       string
	standIn   bool
	noOpenat2 bool
}

// start is Start, with proc the mount procOwn checks; a test of the package gives a stand-in. A
// failed close of the mount's descriptor is an error with the tree, which then runs.
func start(d Detached, proc procFS) (tree Tree, err error) {
	if err := unnamed(treeIDHere); err != nil {
		return Tree{}, err
	}
	root, err := procOwn(proc)
	if err != nil {
		return Tree{}, err
	}
	defer func() { err = errors.Join(err, root.close()) }()
	here, err := place(root)
	if err != nil {
		return Tree{}, err
	}
	if err := pathRefused(d.Path); err != nil {
		return Tree{}, err
	}
	if programsOnly {
		if err := unstartable(d.Path, d.Dir); err != nil {
			return Tree{}, err
		}
	}
	env := d.Env
	if env == nil {
		env = Bare()
	}
	cmd := exec.Command(d.Path, d.Args...) //nolint:gosec,noctx // a path the caller names, absolute or resolved against Dir, never a name looked up, with arguments no shell reads, on Windows a program file alone (programsOnly); the program outlives every context of this process
	cmd.Dir, cmd.Env = d.Dir, append([]string{}, env...)
	// A nil *os.File in the io.Writer fields would not be nil there, and a closed one is no file.
	if !noFile(d.Output) {
		cmd.Stdout, cmd.Stderr = d.Output, d.Output
	}
	return detach(cmd, root, here)
}

// Stop kills every process of the tree id names, at once: on Unix by SIGKILL to its process group,
// only while its leader is the process Start started and not yet reaped, the guard Run's kill keeps;
// on Windows by ending its Job Object. An id whose leader is gone is ErrStale, and one Start does not
// give here is refused; neither signals anything. On Linux so is a /proc that is a link, is not the
// root of a proc file system, or is not this pid namespace's own, before any file below it is read as
// a process's, with an error that wraps errors.ErrUnsupported; every read after goes below the one
// descriptor that check opened, across no mount where the kernel gives openat2, and what that leaves
// open is in procOwn's doc and the README. On Unix a group that answers the kill with EPERM is an error
// wrapping syscall.EPERM, never os.ErrProcessDone: the tree may run on. On macOS the group is
// stopped by SIGSTOP, with the same guard, before its kill, so a process the tree forks as the kill
// lands is not missed by it; a process of the tree sees a SIGSTOP before its SIGKILL,
// and a group that answers that first SIGSTOP with EPERM is the same error. Stop sends no SIGCONT:
// a tree is left stopped if the caller dies between the two, and a second Stop of the id kills it.
// On a system with no tree to name the error wraps errors.ErrUnsupported.
// Stop asks nothing first: Interrupt asks the tree to stop, with a signal its programs can catch.
//
// An id is a name, not a capability: on Unix anyone may build one for a session leader from its pid
// and start time, which the system shows, so holding one proves nothing. What holds is the kill's own
// permission check, which an id does not widen.
func Stop(id TreeID) error {
	return stop(id, procFS{dir: procMount}, ender())
}

// stop is Stop, with proc the mount procOwn checks and k the killer; a test of the package gives
// stand-ins.
func stop(id TreeID, proc procFS, k killer) error { return onTree(id, proc, k, stopAt) }

// Interrupt asks the tree id names to stop, with a signal its programs can catch, and returns
// once the signal is sent; it does not wait and it kills nothing. On Linux and macOS it sends the
// tree's process group one SIGINT, after Stop's checks, in Stop's order and under Stop's guard: an
// id Start does not give here is refused as Stop refuses it, and one whose leader is gone or
// reaped is ErrStale, neither signalling anything; a group that answers the signal as gone is
// ErrStale too, and one that answers EPERM is an error wrapping syscall.EPERM. No signal follows
// the first, and no SIGSTOP goes before it. A program that ignores SIGINT, or was started with it
// ignored, runs on: the wait, how long it lasts and the Stop after it are the caller's. On Windows
// nothing is sent and the tree runs on, with an error wrapping errors.ErrUnsupported: Start gives a
// tree no console (DETACHED_PROCESS), and Windows delivers CTRL_BREAK_EVENT only to processes that
// share the sender's console. The id is checked and its job opened first, so an id Start does not
// give and a stale one read there as they do to Stop. On a system with no tree to name the error
// is Stop's. As with Stop, an id is a name, not a capability, and the signal's own permission
// check is what holds.
func Interrupt(id TreeID) error {
	return interrupt(id, procFS{dir: procMount}, ender())
}

// interrupt is Interrupt, with proc and k as stop takes them.
func interrupt(id TreeID, proc procFS, k killer) error { return onTree(id, proc, k, interruptAt) }

// onTree is what Stop and Interrupt do before either acts, the same for both: a system that names no
// tree refuses, then the proc mount is opened and checked (procOwn) and where an id is good read
// from it (place). act, stopAt or interruptAt, then acts on id by k, reading below that one mount.
func onTree(id TreeID, proc procFS, k killer, act func(TreeID, procRoot, string, killer) error) (err error) {
	if err := unnamed(treeIDHere); err != nil {
		return err
	}
	root, err := procOwn(proc)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.close()) }()
	here, err := place(root)
	if err != nil {
		return err
	}
	return act(id, root, here, k)
}
