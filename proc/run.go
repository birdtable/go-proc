// Package proc runs a program, built on os/exec, a path and an argv no shell reads, and on the
// system's tree: a process group on Unix, a Job Object on Windows, and a cgroup for Linux's limits.
// It adds a deadline, an output cap and a bare environment, a tree killed whole when the run ends,
// and the refusal of a bound the system cannot hold, never weakened. The app sets every bound in its
// Cmd: Timeout and MaxOutput, both required, and Grace, Drain, Limits, Env and Terminal. It is a
// guard against accidents, not a jail: on Unix a child that leaves its group is not reached. On
// Windows a process a system service starts on a child's behalf (WMI, Task Scheduler) is not in the
// tree. A process of the tree may duplicate the job's handle from the caller; held open, it keeps
// the job and itself alive past the caller, and by it the process may allow breakaway, clear
// kill-on-close or lift the job's limits. A process of the caller's user, in a Start tree or not,
// that duplicates the tree's job handle, marks it inheritable or opens the job by name keeps the
// tree's id live past the leader's reap: a Stop then ends what is left of that tree and returns
// nil, not ErrStale. Only that tree's own job opens, as its name is random.
package proc

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec" //nolint:depguard // proc is the runner: it starts the program its caller names, by a path, never by a name looked up, with arguments no shell reads
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/birdtable/go-proc/internal/homevars"
)

// The words of Ended.Why, a closed list.
const (
	// WhyDeadline is a run that reached its Timeout.
	WhyDeadline = "deadline"
	// WhyCap is a run whose program wrote more than MaxOutput bytes to one stream.
	WhyCap = "cap"
	// WhyCancelled is a run whose context ended before the program did.
	WhyCancelled = "cancelled"
	// WhyEscaped is a run a process still held a pipe of Cmd.Drain after the tree was killed: one
	// that left the tree, which the kill does not reach, or a member the kill ended that had not yet
	// exited. A tree that left nothing ends a run with it only while such a member holds a pipe.
	WhyEscaped = "escaped"
	// WhyUnkillable is a run whose tree no signal from this process reaches, on Unix: a process of it
	// changed its user, as sudo does. Run returns without waiting for the program, which runs on until
	// it ends by itself, and is reaped then.
	WhyUnkillable = "unkillable"
	// WhyLimit is a run whose tree passed one of its Limits, where the system says so: on Windows, where
	// the Job Object says it, and the tree is killed then; on Linux, where the run set a TreeMemory and
	// that limit ended the tree: the kernel's kill of it whole (memory.events.local's oom and
	// oom_group_kill, or before 5.17 its oom_kill), or the run's own kill on an oom, as for Contained.
	// A kill for a limit above the run's cgroup or beneath it, or the system's own, is not one, nor a
	// reach that killed nothing (an allocation that got ENOMEM), which ends as the exit says; except
	// that before 5.17 a reach with a kill of the system's own in the run's cgroup ends limit.
	WhyLimit = "limit"
)

// Ended is how a run ended other than by its program's own exit: Why is one of the Why words.
// Callers tell endings apart with errors.As.
type Ended struct{ Why string }

func (e Ended) Error() string {
	switch e.Why {
	case WhyDeadline:
		return "proc: the run reached its timeout, and its tree was killed"
	case WhyCap:
		return "proc: the program wrote more than the output cap to one stream, and its tree was killed"
	case WhyCancelled:
		return "proc: the run's context ended, and its tree was killed"
	case WhyEscaped:
		return "proc: a process that left the run's tree still held its output, which was closed unread"
	case WhyUnkillable:
		return "proc: no signal reached the run's tree, which runs on: its output was closed unread"
	case WhyLimit:
		return "proc: the run's tree passed one of its limits, and was killed"
	}
	return "proc: the run ended: " + e.Why
}

// The causes Run gives its contexts, read back to name an Ended.
var (
	errDeadline = Ended{WhyDeadline}
	errCap      = Ended{WhyCap}
	// errUnkillable is a group's wait that gave up on a tree its signals did not reach.
	errUnkillable = Ended{WhyUnkillable}
	// errLimit is a job that said its tree passed a limit.
	errLimit = Ended{WhyLimit}
)

// ownCut is a cause Run gives its own context: its Timeout, its cap or its limit. A caller's cause
// is never one, even one that errors.Is an Ended, so a run reads only its own cuts as its own.
type ownCut struct{ Ended }

// errNoTree is Run, Start and Stop on a system that gives the runner no way to kill a child's whole
// tree: it runs nothing there, rather than kill the child alone and leave what the child started
// running.
var errNoTree = fmt.Errorf("proc: this system gives the runner no way to kill a child's whole tree, so it runs nothing here: %w", errors.ErrUnsupported)

// The refusals Run makes: the caller's program never starts, and each comes before anything starts,
// but the one Limits.Contained names for a shim that never said it was ready.
var (
	// ErrRefused is wrapped by each refusal, so a caller tells a refused run from one that ran: the
	// caller's program never runs, and nothing else is started for it. On Linux a run held to
	// Limits.TreeMemory may have made its cgroup before the refusal, and removed it again. One
	// refusal comes after the start: a Contained run whose shim never said it was ready. Where the
	// start skipped the shim, its program ran, unheld, to its end, and its output is not given.
	ErrRefused = errors.New("proc: the run is refused and its program was not started")

	// ErrNotHeld is matched by each refusal of what this system cannot hold here, as the caller
	// asked it, so a caller tells it from a Cmd of its own that is wrong without matching text. They are: a Limits.TreeMemory where no cgroup is delegated to the caller on
	// Linux, and on every system but Linux and Windows; a Limits.Processes on every system but
	// Windows; a Limits.Contained where Linux cannot contain the tree, and on every system but
	// Linux; a Grace above zero where this run's tree is sent no signal it can catch;
	// a File run on every system but Linux, and on Linux where /proc is no proc mount; and on Linux
	// a TreeMemory or a ReadPeak from within a run's cgroup. Each still wraps ErrRefused and
	// errors.ErrUnsupported, its text unchanged. No refusal of the caller's own Cmd matches it, and
	// a refusal that carries a system's errno need not: with ENOSYS, ENOTSUP or EOPNOTSUPP it may
	// match errors.ErrUnsupported and not ErrNotHeld, and reads as the run's own. Holds gives it
	// before a run. A run its context cut carries that context's cause, so ask for an Ended first.
	ErrNotHeld = errors.New("proc: this system cannot hold the run as asked here")

	// errRefused is what a refusal made as the run goes wraps, and what a gate asks a refusal to
	// match: the value ErrRefused was given here, as each refusal above wraps it.
	errRefused = ErrRefused
	// errNotHeld is what notHeld matches: the value ErrNotHeld was given here.
	errNotHeld = ErrNotHeld

	errEmptyPath     = fmt.Errorf("%w: Path is empty, which names no program", ErrRefused)
	errNUL           = fmt.Errorf("%w: Path, an Arg, a variable of Env, Dir or File's name holds a NUL byte, which no system passes to a program", ErrRefused)
	errDashPath      = fmt.Errorf("%w: Path begins with -, which env-style launchers read as an option, and a lone - as an empty environment, and run the next word in its place; a file whose name begins with - is written after ./", ErrRefused)
	errEqualsPath    = fmt.Errorf("%w: Path holds =, which env-style launchers read as a setting and do not run", ErrRefused)
	errRelativePath  = fmt.Errorf("%w: Path is a bare name, with no separator, so which program runs would depend on this process's PATH or folder", ErrRefused)
	errNotUnderDir   = fmt.Errorf("%w: Path names a drive with no root after it, or a root with no drive before it, so Windows would resolve it against a current folder of this process's, not against Dir", ErrRefused)
	errTimeout       = fmt.Errorf("%w: Timeout is zero or negative, and a run has a deadline", ErrRefused)
	errMaxOutput     = fmt.Errorf("%w: MaxOutput is zero or negative, and a run's output is capped", ErrRefused)
	errGrace         = fmt.Errorf("%w: Grace is negative", ErrRefused)
	errNoGrace       = notHeld{fmt.Errorf("%w: Grace is above zero, and this run's tree would be sent no signal it can catch before the kill, so a Grace would be no time given; on Windows one is sent only to a run with Terminal, from a process with a console: %w", ErrRefused, errors.ErrUnsupported)}
	errDrain         = fmt.Errorf("%w: Drain is negative", ErrRefused)
	errDrainWide     = fmt.Errorf("%w: Grace and Drain together pass the longest time.Duration, so the run's bound past its Timeout would wrap", ErrRefused)
	errTerminalStdin = fmt.Errorf("%w: Terminal is not set and Stdin is a terminal, or a console, where a program that finds no terminal asks its user, so it would ask this process's user there", ErrRefused)

	// ErrBatch is the refusal, on Windows alone, of a Path that names a batch file (.bat or .cmd),
	// as given or as PATHEXT finds it, by Run and Start. It wraps ErrRefused, and a caller matches it
	// with errors.Is to word the refusal its own way: point the Path at the program file itself. It
	// is declared on every system, so a caller's code that matches it needs no build tag, and off
	// Windows no run returns it.
	ErrBatch = fmt.Errorf("%w: Path is a batch file (.bat or .cmd), which Windows runs through cmd.exe, and cmd.exe reads its arguments by rules of its own, by which an argument can run a second command", ErrRefused)

	// ErrNotProgram is the refusal, on Windows alone, of a Path that, as given or as PATHEXT finds
	// it, names no program file (.exe or .com) and no batch file, by Run and Start. It wraps
	// ErrRefused and never ErrBatch: a batch file is refused with ErrBatch alone. It is declared on
	// every system, so a caller's code that matches it needs no build tag, and off Windows no run
	// returns it.
	ErrNotProgram = fmt.Errorf("%w: Path, or the file PATHEXT finds for it, is not a program file (.exe or .com), and Windows hands another kind to a program of its own, which may read the arguments by rules of its own", ErrRefused)

	// errBatch and errNotProgram are what Run and Start return for these refusals: each wraps the
	// value its exported name was given here, which errors.Is matches, so a value a caller or a
	// dependency stores in ErrBatch or ErrNotProgram later, nil included, changes what a match
	// finds and never what is refused.
	errBatch      = fmt.Errorf("%w", ErrBatch)
	errNotProgram = fmt.Errorf("%w", ErrNotProgram)

	errExtraNoFile = fmt.Errorf("%w: an ExtraFiles entry is nil or a closed file, which the program would find closed on Unix, so the first file it opened would take that descriptor, and on Windows an invalid handle", ErrRefused)
	errFilePath    = fmt.Errorf("%w: File and Path are both set, so which of them names the program would be unclear", ErrRefused)
	errNoFileRun   = notHeld{fmt.Errorf("%w: this system gives the runner no way here to start a program from an open file, so a run with File starts nothing: %w", ErrRefused, errors.ErrUnsupported)}
	errFileScript  = fmt.Errorf("%w: File begins with #!, a script, and what would run is the interpreter its first line names by a path, which File does not hold open", ErrRefused)
	errFileUnread  = fmt.Errorf("%w: File cannot be read at its start to tell whether it is a script", ErrRefused)
	errFileNoProc  = notHeld{fmt.Errorf("%w: /proc, below which the program would find its own file at self/fd, is not a proc mount but another folder or a link, so a file by that name there would run in File's place: %w", ErrRefused, errors.ErrUnsupported)}
)

// DefaultDrain is a run's Drain when Cmd.Drain is zero, on every system: a second. It
// is the read past the kill that lets a process this one starts beside the run, which holds a copy of
// the run's pipes until it begins its own program, let them go, so that a run's output is not cut for
// another run's start. A caller whose host starts processes slower than that under load sets Drain.
const DefaultDrain = time.Second

// Cmd is a program to run and the bounds of its run.
type Cmd struct {
	// Path is the program: an absolute path, or a relative one with a separator in it (./tool,
	// bin/tool, ..\tool.exe), resolved against Dir, or against this process's folder when Dir is
	// empty, as exec.Cmd resolves one. A bare name, with no separator, is refused: Run looks nothing
	// up in PATH, and never in the current folder (exec.ErrDot). To run a program by its name, find
	// it with exec.LookPath, which reads PATHEXT on Windows and returns exec.ErrDot for a hit in the
	// current folder, and give the path it returns. On Windows a path that names a drive with no
	// root after it (C:tool.exe), or a root with no drive before it (\tool.exe), is refused too,
	// since Windows resolves it against a current folder of this process's, not against Dir.
	// A Path that begins with - is refused, as an env-style launcher reads it as an option; a file
	// whose name begins so is written after ./ (./-x/tool).
	// On Windows, exec gives a Path that names no file the first extension of PATHEXT that makes it
	// one, and Run starts only a program file (.exe or .com), as Path and as so found, a relative Path
	// found under Dir: Windows runs a batch file through cmd.exe and a script through its own program,
	// which would read Args by their own rules. A program that needs cmd.exe names cmd.exe. A batch
	// file is refused with ErrBatch, and any other file that is no program file with ErrNotProgram.
	Path string
	// Args are the program's arguments after its name, each passed as one argument; no shell reads
	// them.
	Args []string
	// Dir is the folder the program runs in. Empty is this process's working folder, on every system,
	// as os/exec has it; a relative Path is found from Dir, so from there when Dir is empty. NeutralDir is a folder that is no project's, for a run that needs none.
	Dir string
	// Env is the program's whole environment. nil is Bare(), the safe default; an empty one is none.
	Env []string
	// Stdin is what the program reads; nil is nothing, the system's null device, as are a nil
	// *os.File and a closed one. An open file is the program's stdin itself. Any
	// other reader Run copies to the program through a pipe, and an error the copy meets is the
	// run's error when the program's exit gives none. The run's end does not wait for the copy: a
	// Read that has not returned when the run ends is the reader's to end, and Run returns with the
	// copy still in it, which writes nothing more once the Read returns.
	Stdin io.Reader
	// Stdout, when not nil, is given the program's stdout as Run reads it: exactly the bytes Run
	// keeps of it, in order, so once Run returns what it was given equals Result.Stdout, cut at
	// MaxOutput as that is, the bytes a drain reads after the kill included. nil is none. Run calls
	// its Write from a goroutine of its own, never two at once, never after Run returns, and does
	// not return while one is in flight; a Write may carry many reads' bytes. The writer holds back
	// no read of the program's output: Timeout, the cap, the kill and Drain are as without it, and a
	// writer that is slow costs the caller only Run's return (Drain says by how much) and the memory
	// of what it has not yet been given, up to MaxOutput beside Result's own. A writer that returns
	// an error, or a count short of what it was given with none (io.ErrShortWrite), or whose Write
	// does not return (runtime.Goexit; a panic stays its own, not recovered) is given no more; the
	// run and Result go on, and Run's error wraps why and names the stream. The writer is this
	// process's, never handed to the program, so Limits.Contained's check of the descriptors handed
	// on does not see it. A run refused after its start, as a Contained run whose shim never said it
	// was ready, gives it nothing. Start takes no writer. Lines are the caller's.
	Stdout io.Writer
	// Stderr is the program's stderr as Stdout is its stdout. When Stdout and Stderr are the same
	// comparable writer, as os/exec reads them, one goroutine alone calls its Write, and one Write
	// may carry bytes of both streams in the order Run read them; its error then names the streams
	// the Write that failed carried.
	Stderr io.Writer
	// Timeout is how long the run may take, more than zero.
	Timeout time.Duration
	// Grace is how long the tree has, on Unix, between SIGTERM and SIGKILL when the run is ended;
	// zero, the safe default, kills it at once. A leader that exits ends the grace, and the rest
	// of its group is killed then. On Windows a Grace above zero is held only for a run with
	// Terminal, while this process has a console: the end sends CTRL_BREAK_EVENT to the program's
	// group, holding the program's handle while it sends, then kills the job once the Grace is
	// gone, always. A break is a request, as SIGTERM is: a program that catches it may end within
	// the Grace, and one that does not is killed at its end. A process of the tree that made a
	// group or a console of its own is sent no break, and is killed with the job. A break that
	// cannot be sent kills the job at once, and the run's error says why. Every other Windows run
	// with a Grace above zero is refused, with an error wrapping ErrRefused, ErrNotHeld and
	// errors.ErrUnsupported, and nothing starts; Holds gives the same for the Cmd. A run without
	// Terminal stays refused: its program has a console of its own (CREATE_NO_WINDOW), and a break
	// sent from this process's console did not reach one on Windows 10.0.26200.
	// A Grace is a run's alone: a tree Start started is asked to stop by Interrupt.
	Grace time.Duration
	// MaxOutput is how many bytes of each stream are kept, more than zero. A program that writes
	// more is ended with WhyCap.
	MaxOutput int
	// Limits are what the run's tree is held to while it runs; the zero Limits is none. A limit this
	// system cannot hold refuses the run, never weakened.
	Limits Limits
	// Terminal gives the program this process's terminal: on Unix the controlling terminal of this
	// process's session, on Windows this process's console. The zero value, false, is the
	// fail-closed one, the safe default: the program starts with no terminal of this process's, so
	// that a program that asks its user at the terminal, for a password or a yes, never asks this
	// process's user. The program is in the run's tree with Terminal or without it, and is ended
	// with it. Without Terminal, on Unix it leads a session of its own as well as its group, and
	// the session has no controlling terminal: /dev/tty does not open, so such a program fails at
	// once. On Windows it starts with a console of its own that no window shows, never this
	// process's: a program that reads that console waits for the Timeout, as no one types into it,
	// and a console program it starts shares the console and opens no window either. With Terminal
	// it leads its group alone, in this process's session, or shares this process's console. A
	// Stdin that is a terminal is refused without Terminal: on Windows a console, or the pipe of an
	// MSYS or Cygwin pty, which a program under mintty with pseudo consoles off has in place of a
	// console. A pipe is told by its name, which Windows does not give while a Read is pending on
	// the pipe: one whose name does not come within a second is refused too, and the query is left
	// to end with that Read, so one thread of this process is held for as long as the caller's own
	// Read is pending. A Stdin whose type Windows does not give, or a pipe whose name it refuses to
	// give or gives with a length that is odd or past its room, cannot be told from a pty's either
	// and is refused: so a socket, or another pipe Windows gives no name, is refused without
	// Terminal. A pipe whose name is empty, as an os.Pipe end's is, is no pty's, a pty's pipes
	// being named. A Stdin of handle 0, as os.Stdin is in a process that has no stdin, a service's
	// or a detached program's, runs, and the program gets no stdin. A program that finds no
	// terminal asks at its stdin, as ssh-keygen asks for a passphrase, so it would ask this
	// process's user there. A reader that reads a terminal, as bufio.NewReader(os.Stdin) does, is
	// not looked through: what it relays to the program is the caller's to choose. Stdout and
	// Stderr are what they are with Terminal. A run with Terminal asks nothing of its Stdin. Terminal
	// is refused with Limits.Contained, whose tree could type at this process's terminal as its user.
	Terminal bool
	// Drain is how long Run reads an output pipe a process still holds once the program has exited,
	// or been ended, and its tree has been killed: as given, and DefaultDrain when zero, on every
	// system. Only a process that left the tree, one the kill has not yet ended, or one
	// this process started at the same time and that has not yet begun its program, can still hold a
	// pipe then: past Drain a pipe still held is closed, what was read is kept, and the run ends with WhyEscaped. On Unix a pipe no
	// process holds then is read for what it held then, and no more, with no wait: a write end
	// opened after, as a process of the same user can open one on Linux through /proc, adds nothing
	// Run waits for, and a short Drain cuts no output of a tree that left nothing. On Windows a pipe
	// still being read then is cut whoever holds it, since what it holds cannot be counted while its
	// read is pending.
	//
	// What a Drain costs: a short one ends with WhyEscaped a run whose killed member has not yet exited, or whose pipe a process this one started at the same time still holds
	// between its fork and its exec, which took a millisecond or so in this package's tests and can
	// take longer under load, and keeps only what was read by then; a long one holds the caller that
	// long past the kill for a process that left the tree. A negative Drain is refused, and so is one
	// that with Grace passes the longest time.Duration. Drain never lengthens Timeout, the program's
	// own bound, which ends the program as without it: it is the read after the kill. Run returns at
	// most Timeout, Grace and Drain after the program starts, and with a Stdout or Stderr writer
	// later by the time the Write in flight then takes and one Write more, of what is left. On Unix
	// a tree no signal from here reaches is waited for Grace and Drain past the kill, then left to run, with WhyUnkillable, if
	// its leader, asked then without waiting, still runs: one that has exited, as a group of exits not
	// yet reaped answers macOS's kill as such a tree does, ends the run with its exit, whatever Grace
	// and Drain are.
	Drain time.Duration
	// ReadPeak asks for the most memory the run held at once, in Result.Peak, with Result.PeakRead
	// saying whether it was read: read PeakRead first, since a run with no reading leaves Peak 0.
	// What it counts differs by system, as Result.PeakKind names it, so a reading is not comparable with
	// another system's.
	//
	// On Linux it is the run's cgroup's memory.peak, PeakTreeCharged: the most memory charged to the whole tree at once,
	// every process the program started counted, its page cache and kernel memory among it, not a
	// resident set. The run takes a cgroup of its own for it, as Limits.TreeMemory does, where a cgroup
	// is delegated so (see TreeMemory); where none is, or the kernel gives no memory.peak (Linux 5.19),
	// the run goes all the same and reads none. A memory.peak that is 0, "max", past
	// MaxInt64, no number or unreadable is no reading: a tree that ran held some memory. Where this
	// process is itself in a run's cgroup, as a program a run of proc runs is, the run is refused,
	// wrapping ErrRefused and errors.ErrUnsupported, since its cgroup would go beside that one, out of
	// its limit and its kill.
	//
	// On the BSDs and darwin it is the resident set's peak the program's wait gives, PeakProcessResident, ru_maxrss, scaled
	// to bytes (darwin gives bytes): the larger of the program's own and that of each descendant it
	// waited for; a descendant left running, or one its parent did not wait for, is not in it.
	//
	// On Windows it is the job's PeakProcessMemoryUsed, PeakProcessCommitted: the most memory committed to any one process
	// of the tree. Another system reads none. A run refused, ended with WhyUnkillable, or whose
	// program never ran reads none.
	ReadPeak bool
	// ExtraFiles are open files the program inherits beyond its stdin, stdout and stderr. An entry
	// that is nil or a closed file is refused, on every system, as no file (as Stdin reads one): on
	// Unix the program would find that descriptor closed, and the first file it opened would take
	// its number. Each other entry is handed on as the standard library hands it and checked no
	// further. On Unix entry i is the program's descriptor 3+i, as exec.Cmd.ExtraFiles gives it. On
	// Windows each file's handle is inherited with the value it has here, as
	// syscall.SysProcAttr.AdditionalInheritedHandles gives it: the caller marks each handle
	// inheritable first, and tells the program its value, as by an argument.
	ExtraFiles []*os.File
	// File, when set, is the program as an open file: the file itself runs, not what its name names
	// when the run starts, so a file renamed over that name after File was opened is not the one
	// that runs. Path is then empty, and both set is refused; the program's argv[0] is File.Name(),
	// and Args, Dir and Env are as for Path. Linux alone runs one, as glibc's fexecve does: the
	// file is handed on after ExtraFiles, as descriptor n, 3+len(ExtraFiles), and the child execs
	// /proc/self/fd/<n>, so the program finds its own file open there; the process's name, as ps
	// and pkill read it, is then n. A run is refused, with an error that wraps ErrRefused and
	// errors.ErrUnsupported, when /proc is a link, a link to a proc mount included, or a folder on
	// no proc file system, as a file by that name below it would run in File's place. The check
	// reads the type of /proc itself, by path, so a file bound below /proc, and a writer of /proc
	// who replaces it between that check and the start, are not guarded against, as os/exec offers
	// no execveat (procMounted). Every other system refuses a set File, with the same wrapping. A
	// File that begins with #! is refused, as what would run is the interpreter its first line
	// names by a path; so is a File whose start cannot be read. File holds the program's file
	// alone: not its interpreter (PT_INTERP), its libraries or a binfmt_misc handler its first
	// bytes select, which the start finds by their paths, nor what the program finds by argv[0];
	// and a write to the file itself is not guarded against. The caller keeps File open until Run
	// returns, and closes it.
	File *os.File
	// seam is a test's of the package, set only by a _test.go file of it.
	seam *seams
}

// PeakKind is what a Result.Peak counts, which differs by system: a figure read with one kind is not
// comparable with one read with another.
type PeakKind string

// The kinds of a Result.Peak, a closed list. Limits.TreeMemory says which a cap holds on each system.
const (
	// PeakNone is no reading: Result.PeakRead is false.
	PeakNone PeakKind = ""
	// PeakTreeCharged is the memory a cgroup charged to the whole tree at once, every process of it
	// counted, its page cache and kernel memory among it (Linux).
	PeakTreeCharged PeakKind = "tree-charged"
	// PeakProcessResident is one process's resident set, the program's or a descendant's it waited
	// for, whichever was largest (the BSDs and darwin).
	PeakProcessResident PeakKind = "process-resident"
	// PeakProcessCommitted is the memory committed to one process of the tree, whichever committed most
	// (Windows).
	PeakProcessCommitted PeakKind = "process-committed"
)

// Result is what a run's program printed and how it exited.
type Result struct {
	// Stdout and Stderr are at most MaxOutput bytes each, the first the program wrote.
	Stdout, Stderr []byte
	// Code is the program's exit code: -1 when the run was refused or ended, or the program was
	// killed by a signal.
	Code int
	// Peak is the most memory the program held at once, in bytes, when the run set Cmd.ReadPeak and
	// PeakRead is true; 0 otherwise. Cmd.ReadPeak says what it counts on each system.
	Peak int64
	// PeakRead is whether Peak was read. Read it first: a run with no reading leaves Peak 0.
	PeakRead bool
	// PeakKind is what Peak counts, this system's kind when PeakRead is true, and PeakNone otherwise.
	PeakKind PeakKind
	// Left is whether the run's tree still held a process once its program, the tree's leader, had
	// exited, a sign the program started one it did not wait for. The tree is killed whole all the
	// same, and Code and the error are as they would be without it. A run ended by its Timeout, its
	// MaxOutput, ctx or a limit reads Left as one whose program exited, once the kill has ended the
	// leader. A process exiting at the moment of the check counts as left. A run that ends with
	// WhyUnkillable or WhyEscaped reads Left true: a process outlived the kill, or left the tree.
	//
	// On Linux and macOS it is a listing of the program's process group, the program aside: /proc on
	// Linux, the kern.proc.pgrp sysctl on macOS; any other process of the group counts, one that
	// has exited and is not yet reaped among them. The group is stopped by SIGSTOP before the
	// listing, so a chain of processes that each fork and exit is held where it is and listed, and
	// then killed as ever. On macOS the group is listed before the stop as well, and a process in
	// either listing counts: one whose exec is finishing as its parent exits is in no listing for a
	// moment after the stop. A process of the group whose parent has ended is reaped by its pid
	// namespace's init: under an init that never reaps, as a container's may be, it stays a zombie of
	// the group, and Left reads true. A listing, or the stop before it,
	// that fails is true, and so is Left on the BSDs, which list no group here. On Windows it is the
	// job's count of live processes, read after the program's wait returns and before the job is
	// closed. A console host Windows started for the tree, which ends only after the last process on
	// its console, is waited for, up to five seconds, when such hosts alone are left; one still there
	// then counts. A count that cannot be read is true.
	//
	// A process that left the tree is not listed: by setsid or setpgid on Unix, or by breaking away
	// from the job on Windows; one that holds the run's output reads Left true by WhyEscaped, one
	// that does not is not seen. On Linux a process of another user that /proc hides (hidepid) is not
	// seen either. Left false is not proof that the program started nothing.
	Left bool
}

// process is a program Run starts, as its tree and its start hold it.
type process = exec.Cmd

// notHeld is a refusal of what this system cannot hold here: it is ErrNotHeld too, and its text is
// the refusal's own.
type notHeld struct{ error }

// Is is whether target is ErrNotHeld, as it was given here.
func (notHeld) Is(target error) bool { return target == errNotHeld }

// Unwrap is the refusal, which wraps ErrRefused and errors.ErrUnsupported.
func (n notHeld) Unwrap() error { return n.error }

// Holds is the refusal Run would give c here and now before it starts anything, or nil, and the
// PeakKind a run of c reads: Run's own checks of c, in Run's order, with no program started, no
// cgroup made and nothing written. A refusal of what this system cannot hold wraps ErrNotHeld. The
// kind is PeakNone when c has no ReadPeak, on a system that reads no peak, and on Linux where no
// cgroup is delegated to the caller, as a ReadPeak run there runs without one.
//
// Run's refusal stays the authority: a nil here does not promise a run, as the host may change
// between the two, and some refusals come only as the run goes. On Linux the cgroup check reads
// what Run's reads but writes none of it: where the delegated parent's children are not yet given
// the memory controller, it is read whether the parent holds a process, which keeps it from them,
// not whether the write Run makes would take. A Contained run's shim, which runs at the start, is
// not run: a kernel, a security module or a container that refuses its user namespace, its mounts
// or its limit is met only by Run, as is a folder, a descriptor or a program the shim refuses.
func Holds(c Cmd) (PeakKind, error) {
	if err := c.refused(); err != nil {
		return PeakNone, err
	}
	if programsOnly {
		if err := unstartable(c.Path, c.Dir); err != nil {
			return PeakNone, err
		}
	}
	peaks, err := heldHere(c, c.seams().host)
	switch {
	case err != nil:
		return PeakNone, err
	case treeIDHere == idNone:
		return PeakNone, errNoTree
	case !c.ReadPeak || !peaks:
		return PeakNone, nil
	}
	return peakKind, nil
}

// drain is the run's Drain: c.Drain, or DefaultDrain when it is zero.
func (c Cmd) drain() time.Duration {
	if c.Drain == 0 {
		return DefaultDrain
	}
	return c.Drain
}

// refused is why c is not run, or nil. It reads the parsed fields, in the order a test reads them.
func (c Cmd) refused() error {
	if err := c.programRefused(); err != nil {
		return err
	}
	if err := c.nulRefused(); err != nil {
		return err
	}
	switch {
	case c.Timeout <= 0:
		return errTimeout
	case c.MaxOutput <= 0:
		return errMaxOutput
	case c.Grace < 0:
		return errGrace
	case c.Grace > 0 && !graceHeld(c.Terminal):
		return errNoGrace
	case c.Drain < 0:
		return errDrain
	case c.drain() > math.MaxInt64-c.Grace:
		return errDrainWide
	}
	if err := c.stdinRefused(); err != nil {
		return err
	}
	if err := limitsRefused(c.Limits, limitsHere); err != nil {
		return err
	}
	if c.Limits.Contained && c.Terminal {
		return errContainedTerminal
	}
	if c.Limits.Contained && c.Dir != "" {
		switch fi, err := os.Stat(c.Dir); {
		case err != nil:
			return fmt.Errorf("%w: %w", errContainedDir, err)
		case !fi.IsDir():
			return fmt.Errorf("%w: %s", errContainedDir, c.Dir)
		}
	}
	if slices.ContainsFunc(c.ExtraFiles, func(f *os.File) bool { return noFile(f) }) {
		return errExtraNoFile
	}
	if c.File != nil && !procMounted(c.procDir()) {
		return errFileNoProc
	}
	return c.scriptRefused()
}

// programRefused is why the program c names is not started, or nil: Path's refusals, or with File
// set, File's, but for those that read more of c (refused) or the file (scriptRefused).
func (c Cmd) programRefused() error {
	switch {
	case c.File == nil:
		return pathRefused(c.Path)
	case c.Path != "":
		return errFilePath
	case !fileRuns:
		return errNoFileRun
	}
	return nil
}

// nulRefused is errNUL where c's Path, an Arg, a variable of its Env, its Dir or its File's name,
// which a File run passes as argv[0] (prepare), holds a NUL, the Cmd's own on every system, before
// the limits: a start would give EINVAL, which a Contained start reads as the host's.
func (c Cmd) nulRefused() error {
	nul := func(s string) bool { return strings.IndexByte(s, 0) >= 0 }
	if nul(c.Path) || slices.ContainsFunc(c.Args, nul) || slices.ContainsFunc(c.Env, nul) || nul(c.Dir) ||
		(c.File != nil && nul(c.File.Name())) {
		return errNUL
	}
	return nil
}

// procDir is the proc mount below which a File run's child execs its file: /proc, or the seam's.
func (c Cmd) procDir() string { return cmp.Or(c.seams().procDir, "/proc") }

// scriptRefused is why c's File is refused as a script, or nil: its first two bytes are #!, or
// cannot be read to tell. They are read at offset 0, which moves no offset of the file's.
func (c Cmd) scriptRefused() error {
	if c.File == nil {
		return nil
	}
	var head [2]byte
	n, err := c.File.ReadAt(head[:], 0)
	switch {
	case err != nil && !errors.Is(err, io.EOF):
		return fmt.Errorf("%w: %w", errFileUnread, err)
	case string(head[:n]) == "#!":
		return errFileScript
	}
	return nil
}

// leftAfter is a run's Left: whether its group held a process once the leader exited, or one
// outlived the kill (both in leftOver), or one left the tree and held the output (escaped).
func leftAfter(g *group, escaped bool) bool {
	return escaped || g.leftOver()
}

// stdinRefused is why c's Stdin is refused, terminalIn's answer, when c has no Terminal; a run
// with Terminal asks nothing of its Stdin.
func (c Cmd) stdinRefused() error {
	if c.Terminal {
		return nil
	}
	return terminalIn(c.Stdin)
}

// terminalIn is why r, a run's Stdin, is refused without Terminal: errTerminalStdin when it is a
// terminal, or on Windows a console, a file isTerminal takes; or the error isTerminal gives when it
// cannot tell, which wraps ErrRefused. A Stdin that is no file reaches the program by a pipe Run
// makes, which is no terminal, and one that is no file (noFile) is the null device.
func terminalIn(r io.Reader) error {
	f, ok := r.(*os.File)
	if !ok || noFile(r) {
		return nil
	}
	is, err := isTerminal(f)
	if err != nil {
		return err
	}
	if is {
		return errTerminalStdin
	}
	return nil
}

// pathRefused is why a program at path is not started, or nil: the refusals of Path, which Run and
// Start share.
func pathRefused(path string) error {
	switch {
	case path == "":
		return errEmptyPath
	case strings.HasPrefix(path, "-"):
		return errDashPath
	case strings.ContainsRune(path, '='):
		return errEqualsPath
	case bare(path):
		return errRelativePath
	case !underDir(path):
		return errNotUnderDir
	}
	return nil
}

// bare is whether path is a name alone, with no separator: the Path exec looks up in PATH, and on
// Windows in the current folder too, by exec's own test. An absolute path is never bare.
func bare(path string) bool {
	return filepath.Base(path) == path && !filepath.IsAbs(path)
}

// underDir is whether path resolves against Dir, as exec resolves a relative Path: an absolute path,
// or a relative one that names no drive and starts at no root. On Windows exec resolves C:tool.exe
// against the current folder of drive C and \tool.exe against the root of the current drive, both
// of this process's, never against Dir. On Unix every path but an absolute one is relative to Dir.
func underDir(path string) bool {
	return filepath.IsAbs(path) || filepath.VolumeName(path) == "" && path != "" && !os.IsPathSeparator(path[0])
}

// extension is the extension of path's last name as Windows reads it: once path is cleaned and the
// trailing dots and spaces Windows drops are trimmed, from the last dot on. When the last dot is in
// a folder's name, what follows it holds a separator, so it is no extension a test below accepts.
func extension(path string) string {
	name := strings.TrimRight(filepath.Clean(path), ". ")
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		return name[i:]
	}
	return ""
}

// batch is whether path names a batch file, as Windows reads its name: .bat or .cmd, without case.
func batch(path string) bool {
	ext := extension(path)
	return strings.EqualFold(ext, ".bat") || strings.EqualFold(ext, ".cmd")
}

// program is whether path names a program file, as Windows reads its name: .exe or .com, without
// case. Windows loads such a file as a program and hands no other program its arguments.
func program(path string) bool {
	ext := extension(path)
	return strings.EqualFold(ext, ".exe") || strings.EqualFold(ext, ".com")
}

// unstartable is why Run on Windows does not start path in dir, or nil: path and the file
// exec.LookPath finds for it by PATHEXT, under dir when path is relative and dir is not empty, the
// lookup exec makes for the program it starts, must both name a program file, and a batch file
// among them is refused by its own error, errBatch, before errNotProgram. exec keeps what it finds
// apart from the Path of its exec.Cmd, so that Path is no answer, and exec takes a path whose
// extension is in PATHEXT as it is, so path is read too. A path the lookup finds nothing for is read by its name alone, and exec
// then finds nothing to start. The two lookups read the same folder, and whoever could change the
// answer between them could change the program itself.
func unstartable(path, dir string) error {
	names := []string{path}
	look := path
	if dir != "" && !filepath.IsAbs(path) {
		look = filepath.Join(dir, path)
	}
	if found, err := exec.LookPath(look); err == nil {
		names = append(names, found)
	}
	if slices.ContainsFunc(names, batch) {
		return errBatch
	}
	for _, name := range names {
		if !program(name) {
			return errNotProgram
		}
	}
	return nil
}

// prepare is the process Run starts for c in ctx, with env as its whole environment: Path, then
// each of Args as one argument. A nil env is none, never this process's, which exec gives for nil.
func prepare(ctx context.Context, c Cmd, env []string) *process {
	if env == nil {
		env = []string{}
	}
	path, extra := c.Path, c.ExtraFiles
	if c.File != nil {
		// File goes after ExtraFiles, and the child execs it by its descriptor there (fileRuns).
		extra = append(slices.Clip(extra), c.File)
		path = c.procDir() + "/self/fd/" + strconv.Itoa(3+len(c.ExtraFiles))
	}
	cmd := exec.CommandContext(ctx, path, c.Args...) //nolint:gosec // a path the caller names, absolute or resolved against Dir, never a name looked up, or File's descriptor, with arguments no shell reads, on Windows a program file alone (programsOnly)
	if c.File != nil {
		cmd.Args[0] = c.File.Name()
	}
	cmd.Dir, cmd.Env = c.Dir, env
	// On Windows grouped moves these to the handles the child inherits, as exec takes none there.
	cmd.ExtraFiles = extra
	// Every stream Run gives cmd is a file (feed, piped), so exec copies nothing and waits on no
	// copy. Past the grace and the drain after ctx ends, exec kills a leader still running; with
	// neither, the cancel's own kill is all.
	cmd.WaitDelay = c.Grace + c.drain()
	return cmd
}

// Run runs c and returns what its program printed and how it exited. A refused c starts nothing.
//
// The program runs in a tree of its own, killed whole when the run ends: at its Timeout, when it
// writes more than MaxOutput bytes to one stream, when ctx ends, and when the program exits on its
// own, so a process it left running ends with it. The first three end the run with an Ended error
// that names why; the program's own exit, whatever its code, is no error. A process that left the
// tree and holds the output ends the run Drain later with WhyEscaped, as does a member the kill
// ended that has not exited by then; a tree that left nothing gives its whole output once each
// member the kill ended has exited, on Unix whatever Drain is. On Unix, a tree no signal
// from here reaches, as when a process of it changed its user, ends the run at most Grace and
// Drain after the kill was due, with WhyUnkillable, and runs on. A system with no way to kill a
// whole tree runs nothing and returns an error wrapping errors.ErrUnsupported.
func Run(ctx context.Context, c Cmd) (Result, error) {
	if err := c.refused(); err != nil {
		return Result{Code: -1}, err
	}
	env := c.Env
	if env == nil {
		env = Bare()
	}
	ctx, cut := context.WithCancelCause(ctx)
	defer cut(nil)
	ctx, stop := context.WithTimeoutCause(ctx, c.Timeout, ownCut{errDeadline})
	defer stop()
	if programsOnly {
		if err := unstartable(c.Path, c.Dir); err != nil {
			return Result{Code: -1}, err
		}
	}
	cmd := prepare(ctx, c, env)
	// The run's cgroup, on Linux, is made before anything starts, and removed on every way out once
	// it is: a TreeMemory it cannot be made for is refused here, and nothing starts.
	tree, err := cgroupFor(c, c.seams().host)
	if err != nil {
		return Result{Code: -1}, err
	}
	tree.bound(ctx)
	g, err := grouped(cmd, c.Grace, c.drain(), c.Limits, func() { cut(ownCut{errLimit}) }, !c.Terminal)
	if err != nil {
		return Result{Code: -1}, errors.Join(err, tree.end(false))
	}
	if err := g.inCgroup(tree); err != nil {
		return Result{Code: -1}, errors.Join(err, g.stop(), tree.end(false))
	}
	out, err := piped(cmd, c.MaxOutput, func() { cut(ownCut{errCap}) }, sinksFor(c.Stdout, c.Stderr))
	if err != nil {
		return Result{Code: -1}, errors.Join(err, g.stop(), tree.end(false))
	}
	in, err := feed(cmd, c.Stdin)
	if err != nil {
		return Result{Code: -1}, errors.Join(err, out.close(), g.stop(), tree.end(false))
	}
	if err := g.start(); err != nil {
		return Result{Code: -1}, errors.Join(startErr(ctx, tree, err), out.close(), in.close(), tree.end(false))
	}
	started := []error{in.started(), out.started(), tree.started()}
	if errors.Join(started...) != nil {
		killed, waited := g.wait()
		_, read := out.drain(c.drain())
		stopped, treeErr := in.stop(), tree.end(false)
		started[2], waited = endedOr(ctx, out, g, started[2], waited)
		return Result{Code: -1}, errors.Join(append(started, gone(killed), waited, read, stopped, treeErr)...)
	}
	// The writers are given output only now, as a run refused after its start gives none.
	out.give()
	killed, waited := g.wait()
	// A tree no signal reaches still holds the pipes, and waits on it no more.
	unkillable := errors.Is(killed, errUnkillable)
	wait := c.drain()
	if unkillable {
		wait = 0
	}
	escaped, read := out.drain(wait)
	// What the copy into a Stdin that is no file met is the run's error, as exec's wait gives it,
	// when the program's exit gives none; a copy still in the Stdin's Read is not waited for.
	if fedErr := in.stop(); waited == nil {
		waited = fedErr
	}
	// The run's cgroup is read once its tree has ended, then killed whole and removed: whether the
	// kernel killed the tree for its limit, which endings reads, and its peak.
	readPeak := c.ReadPeak && !unkillable
	treeErr := tree.end(readPeak)
	res := Result{Stdout: out.stdout.bytes(), Stderr: out.stderr.bytes(), Code: exitCode(cmd, unkillable)}
	why := endings(ctx, out, g, escaped && !unkillable)
	if len(why) > 0 {
		res.Code = -1
		// The program's exit is the kill's doing.
		waited = nil
	}
	// A run that did not ask reads none, and a peak not read names no kind.
	res.Peak, res.PeakRead, res.PeakKind = kindOf(peakOf(cmd, g, tree, readPeak))
	res.Left = leftAfter(g, escaped)
	if exit, ok := errors.AsType[*exec.ExitError](waited); ok {
		// A program's own exit is its Code; a death by a signal not the run's stays an error.
		if exit.ExitCode() >= 0 {
			waited = nil
		}
	}
	// The tree is ended and every pipe read: Run waits on the writers last, so a slow one holds no
	// kill, no drain and no cgroup's removal, and returns with no Write in flight.
	return res, errors.Join(slices.Concat(why, []error{gone(killed), waited, read, treeErr, out.given()})...)
}

// endedOr is the shim's refusal and the tree's wait, or, where endings names why the run ended, that
// and no wait: the kernel kills a shim past TreeMemory, and the run's cut at its deadline or by ctx
// kills one, before it writes its report.
func endedOr(ctx context.Context, out *streams, g *group, refused, waited error) (error, error) {
	if why := endings(ctx, out, g, false); len(why) > 0 {
		return errors.Join(why...), nil
	}
	return refused, waited
}

// startErr is err, a failed start of the run's tree: where it is ctx's own error, which exec's Start
// gives for a ctx already ended and starts nothing, the run's cut, as endings names it; else what
// tree's startFailed makes of it, which for a Contained start is a refused user namespace.
// The cut here always names one: the run's own cap and limit cut only once its tree has started.
func startErr(ctx context.Context, tree *runCgroup, err error) error {
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return errors.Join(cutBy(context.Cause(ctx))...)
	}
	return tree.startFailed(err)
}

// kindOf is a peak with the kind this system's reading counts (peakKind), or PeakNone if not read.
func kindOf(peak int64, read bool) (int64, bool, PeakKind) {
	if !read {
		return peak, false, PeakNone
	}
	return peak, true, peakKind
}

// endings are why the run ended other than by its program's own exit, in order: its output passed
// MaxOutput, its tree passed a limit, it was cut at its deadline or by ctx, and a process that left
// its tree held the output past the drain.
func endings(ctx context.Context, out *streams, g *group, escaped bool) []error {
	var why []error
	if out.over() {
		why = append(why, errCap)
	}
	if g.limited() {
		why = append(why, errLimit)
	}
	if g.wasCut() {
		why = append(why, cutBy(context.Cause(ctx))...)
	}
	if escaped {
		why = append(why, Ended{WhyEscaped})
	}
	return why
}

// cutBy is why a run whose ctx ended by cause was cut: none for its own cap and limit, which endings
// names on its own; errDeadline for its own Timeout; and for any cause the run did not give, the
// caller's, WhyCancelled and cause.
func cutBy(cause error) []error {
	own, ok := errors.AsType[ownCut](cause)
	switch {
	case !ok:
		return []error{Ended{WhyCancelled}, cause}
	case own.Ended == errDeadline:
		return []error{errDeadline}
	}
	return nil
}

// streams is a run's two output pipes, each read into a capped buffer.
type streams struct {
	stdout, stderr *capped
	readers        [2]*os.File
	writers        [2]*os.File
	// kept is what this process keeps of the write ends once the program has started, which tells
	// whether a process holds a pipe (held).
	kept    *kept
	drained chan copied
	// sinks are the caller's writers of each stream, nil for none, one sink for one writer of both.
	sinks [2]*sink
}

// give starts each sink giving its writer what was kept, once the run's tree has started as asked.
func (s *streams) give() {
	for i, k := range s.sinks {
		if k != nil && (i == 0 || k != s.sinks[0]) {
			k.start()
		}
	}
}

// given waits until each writer has been given what was kept, or has failed, and is why each failed.
func (s *streams) given() error {
	err := s.sinks[0].finish()
	if s.sinks[1] == s.sinks[0] {
		return err
	}
	return errors.Join(err, s.sinks[1].finish())
}

// copied is how the copy of pipe i ended.
type copied struct {
	i   int
	err error
}

// piped gives cmd a pipe for each stream, each kept to max bytes, cut called once when either
// would hold more, and what each keeps queued for its sink.
func piped(cmd *process, maxOutput int, cut func(), to [2]*sink) (*streams, error) {
	s := &streams{stdout: &capped{max: maxOutput, cut: cut, to: to[0]}, stderr: &capped{max: maxOutput, cut: cut, to: to[1], i: 1}, sinks: to, drained: make(chan copied, 2)}
	for i := range 2 {
		r, w, err := pipe()
		if err != nil {
			return nil, errors.Join(err, s.close())
		}
		s.readers[i], s.writers[i] = r, w
	}
	cmd.Stdout, cmd.Stderr = s.writers[0], s.writers[1]
	return s, nil
}

// started hands this process's copies of the write ends, which the child holds its own of, to
// keep, which closes them or keeps them for the drain, and starts reading the pipes.
func (s *streams) started() error {
	k, err := keep(s.writers)
	s.kept, s.writers = k, [2]*os.File{}
	for i, w := range []*capped{s.stdout, s.stderr} {
		r := s.readers[i]
		go func() {
			_, err := io.Copy(w, r)
			s.drained <- copied{i, err}
		}()
	}
	return err
}

// drain reads both pipes to their end, for wait at most. Once wait is out, each pipe still being read
// is asked whether a process holds it. One held has its read end closed, its read ends with what it
// had, and escaped is true. On Unix one no process holds has its copy stopped at once and is read
// for what it held then, by FIONREAD's count, and no more (tail): a write end opened after the
// asking, as Linux lets a process of the same user open one through /proc/<pid>/fd, adds nothing
// the drain waits for, and a zero wait cuts no output a tree that left nothing left buffered. On
// Windows every pipe still being read then is cut, as a held one is (unheldBounded).
func (s *streams) drain(wait time.Duration) (escaped bool, err error) {
	d := &draining{s: s}
	d.errs = append(d.errs, s.kept.let())
	out := time.NewTimer(wait)
	defer out.Stop()
	for left := 2; left > 0; {
		select {
		case c := <-s.drained:
			d.copied(c)
			left--
		case <-out.C:
			d.out()
		}
	}
	return d.escaped, errors.Join(append(d.errs, s.close())...)
}

// draining is a drain under way: which copies are done, which pipes the drain cut, and which it
// stopped to read for what they held.
type draining struct {
	s                   *streams
	done, shut, stopped [2]bool
	escaped             bool
	errs                []error
}

// copied takes the end of a copy. Closing a read end ends its copy with ErrClosed, and stopping it
// with ErrDeadlineExceeded, each the drain's own doing. A stopped pipe is then read for what it
// holds.
func (d *draining) copied(c copied) {
	d.done[c.i] = true
	switch {
	case d.shut[c.i] && errors.Is(c.err, os.ErrClosed):
	case d.stopped[c.i]:
		if !errors.Is(c.err, os.ErrDeadlineExceeded) {
			d.errs = append(d.errs, c.err)
		}
		d.errs = append(d.errs, tail(d.s.readers[c.i], []*capped{d.s.stdout, d.s.stderr}[c.i]))
	default:
		d.errs = append(d.errs, c.err)
	}
}

// out is wait's end: a pipe still being read is cut if a process holds it, or where every such
// pipe is cut (unheldBounded); otherwise its copy is stopped at once by a deadline already past.
func (d *draining) out() {
	for i := range 2 {
		switch {
		case d.done[i]:
		case unheldBounded || d.s.kept.held(d.s.readers[i], i):
			d.cut(i)
		default:
			if err := d.s.readers[i].SetReadDeadline(time.Now()); err != nil {
				d.errs = append(d.errs, err)
				d.cut(i)
				continue
			}
			d.stopped[i] = true
		}
	}
}

// cut closes the read end of pipe i, so its copy ends with what it read, and the run escaped.
func (d *draining) cut(i int) {
	d.escaped, d.shut[i] = true, true
	d.errs = append(d.errs, d.s.readers[i].Close())
	d.s.readers[i] = nil
}

// close closes what of the pipes is still open here, and what is kept of their write ends.
func (s *streams) close() error {
	var errs []error
	for _, f := range slices.Concat(s.readers[:], s.writers[:]) {
		if f != nil {
			errs = append(errs, f.Close())
		}
	}
	if s.kept != nil {
		errs = append(errs, s.kept.let())
	}
	s.readers, s.writers = [2]*os.File{}, [2]*os.File{}
	return errors.Join(errs...)
}

// over is whether either stream was cut at its cap.
func (s *streams) over() bool { return s.stdout.cut == nil || s.stderr.cut == nil }

// capped keeps the first max bytes written to it and drops the rest, and queues what it keeps for
// to, the sink of the caller's writer, if any, as stream i. The first write past max calls cut,
// once. One goroutine writes it, and it is read once that goroutine is done.
type capped struct {
	buf bytes.Buffer
	max int
	// cut is nil once called.
	cut func()
	to  *sink
	i   int
}

func (w *capped) Write(p []byte) (int, error) {
	room := w.max - w.buf.Len()
	kept := p[:min(max(room, 0), len(p))]
	w.buf.Write(kept)
	if w.to != nil {
		w.to.add(w.i, kept)
	}
	if len(p) > room && w.cut != nil {
		w.cut()
		w.cut = nil
	}
	return len(p), nil
}

// bytes is what was kept, nil for nothing.
func (w *capped) bytes() []byte {
	if w.buf.Len() == 0 {
		return nil
	}
	return w.buf.Bytes()
}

// exitCode is the reaped leader's exit code. An unkillable run's leader is not reaped by Run: a
// late reap writes cmd.ProcessState when the leader exits, at any time, so the state is not read,
// and the Code is -1.
func exitCode(cmd *process, unkillable bool) int {
	if unkillable {
		return -1
	}
	return cmd.ProcessState.ExitCode()
}

// gone is err, nil when it says the tree was already gone.
func gone(err error) error {
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

// Bare is the safe default environment: nothing of this process's but PATH, LANG and the home names
// (HOME on Unix; USERPROFILE, HOMEDRIVE and HOMEPATH on Windows, with SystemRoot and ProgramData,
// which Windows programs need to load), and the names in keep, those that are set. A name in keep that is empty
// or holds = is dropped. Each name is given once, in a fixed order: the bare names, then keep's.
// With none of them set it is empty, never nil. On Windows, os/exec adds SYSTEMROOT, with the
// caller's value, to a list that has none. Needed names the bare names, and HomeEnv points the home
// names, and every other folder of the person's, at a folder of the caller's. SystemEnv is the
// system part alone.
func Bare(keep ...string) []string {
	names := slices.Concat(bareNames(), keep)
	// Empty, never nil: exec reads a nil Env as this process's whole environment.
	env := []string{}
	seen := map[string]bool{}
	for _, name := range names {
		if name == "" || strings.ContainsRune(name, '=') || seen[envKey(name)] {
			continue
		}
		seen[envKey(name)] = true
		if v, ok := os.LookupEnv(name); ok { //nolint:forbidigo // cli.config.3: Bare is the one reader, and it reads only the names it gives the child
			env = append(env, name+"="+v)
		}
	}
	return env
}

// Needed is the variables Bare keeps of this process's on this system, which a child here needs to
// run: PATH, LANG and HOME on Unix; PATH, LANG, USERPROFILE, HOMEDRIVE, HOMEPATH, SystemRoot and
// ProgramData, without which a Windows program may fail to load, on Windows. It is a new list at
// each call.
// SystemEnv gives the system part alone, with this process's values.
func Needed() []string { return bareNames() }

// SystemEnv is NAME=value of this process's for each variable a child on this system needs to start,
// or to find the system's own files, beyond PATH, LANG and the home names: on Windows SystemRoot,
// without which a Windows program may fail to load, and ProgramData, without which System32's
// OpenSSH exits before it writes a byte; none elsewhere. Each is machine-wide, never a name of the
// person's. Only names that are set are given, each with the spelling go-proc lists, SystemRoot or
// ProgramData, and this process's value. With none set it is empty, never
// nil. It is a new list at each call. Each of its names is one Bare keeps and Needed names, so a
// caller that gives a child PATH, LANG and a home of its own adds SystemEnv for the rest, and puts
// it first, append(proc.SystemEnv(), own...), which is never nil. The other way round, a nil own
// list joined to an empty SystemEnv, as it is off Windows, is nil, and a nil Env is read as Bare
// by Run and Start, and as this process's whole environment by os/exec.
func SystemEnv() []string {
	env := []string{}
	for _, name := range systemNames() {
		if v, ok := os.LookupEnv(name); ok { //nolint:forbidigo // cli.config.3: SystemEnv reads only the names it gives the child, as Bare does
			env = append(env, name+"="+v)
		}
	}
	return env
}

// HomeEnv is NAME=value for every variable a program on this system reads for the person's home,
// configuration, cache, state or temp folder, each pointed at a folder of its own beneath root,
// named for the variable in lower case: HOME, the five XDG_ folders, TMPDIR, TMP and TEMP on every
// system, and on Windows USERPROFILE, APPDATA and LOCALAPPDATA too, with HOMEDRIVE and HOMEPATH
// naming USERPROFILE's folder in two parts. Appended to an Env, Bare's among them, each replaces the
// variable of the same name, so a child that finds its folders by these variables finds none of the
// person's; an absolute path, getpwuid on Unix or the known-folder API on Windows still reaches them.
// It makes no folder: the caller makes each before the run, all but HOMEDRIVE and HOMEPATH, and gives
// an absolute root, as a relative one gives relative values that the child reads against its own Dir.
// testkit.Isolate sets the same list, which a proc test holds equal to this one.
func HomeEnv(root string) []string { return homevars.Under(root) }

// ErrHomeCache is wrapped by each refusal of HomeEnvCache, so a caller tells a folder it gave wrong
// from any other error without matching text. Each refusal names the folder it refuses.
var ErrHomeCache = errors.New("proc: HomeEnvCache refuses the folders it is given")

var (
	errHomeCacheRoot   = fmt.Errorf("%w: root is empty or relative, and the cache folder is judged apart from root only against an absolute one", ErrHomeCache)
	errHomeCacheCache  = fmt.Errorf("%w: cache is empty or relative, and a child reads a relative folder against its own Dir", ErrHomeCache)
	errHomeCacheWithin = fmt.Errorf("%w: cache is root, or a folder beneath it, so the kept cache would be part of the fresh home", ErrHomeCache)
	errHomeCacheHolds  = fmt.Errorf("%w: cache holds root, so the fresh home would be part of the kept cache", ErrHomeCache)
)

// HomeEnvCache is HomeEnv(root) with XDG_CACHE_HOME pointed at cache itself, so a child keeps its
// cache in a folder the caller keeps between runs while the rest of its home is fresh. No other
// variable changes, and no name is given twice. XDG_CACHE_HOME is the one variable on any system
// that names a cache folder alone, so the call reaches only what reads it: on Linux and the BSDs
// os.UserCacheDir follows it and moves to cache; on macOS os.UserCacheDir is $HOME/Library/Caches,
// which no variable names, and stays beneath root; on Windows it is LOCALAPPDATA, which also holds
// programs' local data and settings, and stays beneath root. A tool's own cache variables, such as
// GOCACHE and GOMODCACHE, are the caller's to set.
//
// It refuses, with an error that wraps ErrHomeCache and gives no list, an empty or relative root or
// cache, a cache that is root or beneath it, and a cache that holds root. Folders are compared as
// written, cleaned, and with case folded on every system one letter to one letter, as
// strings.EqualFold folds it; no link is followed. A file system may hold more spellings as one
// folder than that, among them two Unicode normalization forms, and ß and SS on macOS, and those
// are not compared as one, so the caller passes both folders in one spelling. It does not judge the
// cache folder's owner, mode or links, or whether it is there: that is the caller's part, as a kept
// cache is a path from one run into the next. It makes no folder, as HomeEnv makes none.
func HomeEnvCache(root, cache string) ([]string, error) {
	if root == "" || !filepath.IsAbs(root) {
		return nil, fmt.Errorf("%w: %q", errHomeCacheRoot, root)
	}
	if cache == "" || !filepath.IsAbs(cache) {
		return nil, fmt.Errorf("%w: %q", errHomeCacheCache, cache)
	}
	r, c := filepath.Clean(root), filepath.Clean(cache)
	if beneath(c, r) {
		return nil, fmt.Errorf("%w: %q beneath %q", errHomeCacheWithin, cache, root)
	}
	if beneath(r, c) {
		return nil, fmt.Errorf("%w: %q holds %q", errHomeCacheHolds, cache, root)
	}
	env := homevars.Under(root)
	for i, kv := range env {
		if strings.HasPrefix(kv, homevars.Cache+"=") {
			env[i] = homevars.Cache + "=" + c
		}
	}
	return env, nil
}

// beneath is whether the clean, absolute path is dir or a folder beneath it, as written, with case
// folded. It walks up path a folder at a time, so each compare is of whole names, never of a slice
// cut by byte count, as a case pair can differ in length (U+212A KELVIN SIGN and k). One trailing
// separator is dropped from each side before each compare: Clean leaves a share or device volume
// written without one (\\srv\share), and Dir gives it back with one (\\srv\share\).
func beneath(path, dir string) bool {
	sep := string(filepath.Separator)
	dir = strings.TrimSuffix(dir, sep)
	for p := path; ; {
		if strings.EqualFold(strings.TrimSuffix(p, sep), dir) {
			return true
		}
		up := filepath.Dir(p)
		if up == p {
			return false
		}
		p = up
	}
}
