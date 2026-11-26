// Package sandbox runs a program with the network closed, built on what the system gives: Landlock,
// by go-landlock and never in its best-effort mode, and seccomp on Linux, and the system sandbox,
// sandbox-exec, on macOS. It adds one block of them, refused and never weakened where the system
// gives less. The app names the program and, for RunHolding, its folders. Run sets the block and
// execs the program, so the program and every process it starts run behind it. The block also
// closes the ways a program asks a process outside it to act for it: a local daemon's socket, a
// terminal, and on macOS a daemon reached by mach-lookup, though not launchd itself, which a
// process reaches by its bootstrap port.
//
// On Linux the block is set on this process with no privilege and no namespace, so the program sees
// every file's real owner. What Run sets, in order, each failure refusing the run:
//
//   - no_new_privs, so nothing the program starts gains a privilege;
//   - every descriptor above stderr closed at exec, and a stdin, stdout or
//     stderr that is a socket of a family other than AF_UNIX refused, so the
//     program holds no socket that reaches the network;
//   - a Landlock ruleset, ABI 6 or later, that keeps the program from the
//     abstract Unix sockets and the signals of processes outside it, and
//     from ABI 9 from every Unix socket on a path made outside it;
//   - a seccomp filter on every thread, which kills a process that calls the
//     kernel in another architecture's convention, refuses x32 calls, refuses
//     a socket or a socket pair of any family but AF_UNIX, refuses io_uring,
//     which opens a socket without the socket call, and refuses pidfd_getfd,
//     ptrace and process_vm_writev, by which a process takes a socket another
//     of this account's processes holds, and refuses the ioctls TIOCSTI and
//     TIOCLINUX, by which a program pushes a line into the input of a shell
//     outside the block that shares its terminal. Below Landlock ABI 9 it also
//     refuses connect, sendmsg, sendmmsg, and sendto with an address, so a
//     Unix socket on a path is closed there too, and with it any the program
//     makes itself;
//   - a probe: a stream and a datagram Unix socket on a path, made before
//     the ruleset and reached then, the control, and reached again once the
//     filter is set, where each must meet the block's own refusal, EACCES
//     from ABI 9 and EPERM below, and an AF_INET socket made then and again
//     behind the filter, where it must meet the filter's EPERM on every ABI,
//     or the program is not run.
//
// On macOS the descriptors and streams are as on Linux, and the program is run by the system
// sandbox with a profile that denies every network operation, the ioctl TIOCSTI, every
// mach-lookup but the directory service's, and another process's task ports, and allows the rest:
// a signal to a process outside, and launchd, stay open there. It is not containment: launchd
// starts the account's agents outside it, and no probe proves it at run time. Of the task ports it
// refuses the control port, by which a debugger attaches and takes what a process holds, its
// sockets among them, and the read, inspect and name ports, to a caller signed with no
// entitlement; a caller signed with the debugger entitlement was not tried.
//
// Neither system stops another goroutine at the exec. On Linux, where Go marks the others as it
// makes them, a descriptor from a raw dup after the last marking reaches the program, and a caller
// with such goroutines calls Run from a process that has none. The macOS block execs by posix_spawn
// with POSIX_SPAWN_CLOEXEC_DEFAULT, where the kernel closes every descriptor but the standard
// streams whatever its mark, so a pipe another goroutine makes then, a socket it accepts, a
// descriptor it receives in an SCM_RIGHTS message and one from a raw dup do not reach the program
// The program starts there with the open-file limit the caller started with.
//
// RunHolding sets the same block and holds the program's files as well: every file is denied, reads
// included, but those beneath the folders it names, read-write or read-only. On Linux the same
// Landlock ruleset handles every file right of ABI 6 and grants them only beneath the folders, and
// the probe adds a file made outside them, read and written before the ruleset, the control, and
// refused with EACCES behind it, or the program is not run. On macOS the same profile denies every
// file read and write but beneath the folders, which reach the sandbox as parameters, never in the
// profile's text. Run names no folders, and the program's files stay open.
//
// The folders a program reads to start, its loader's and its libraries', RunHolding grants itself,
// read-only, every time: it reads the program in the same call, just before it sets the block, and
// answers the loader's own search as it goes behind the block (the rules are in load.go). A program
// it cannot answer for is ErrUsage, and nothing is widened to let it run. The program, its loader
// and each library are read alike, and each names folders alike: a file swapped after it was read
// is its owner's and its writers' to do, and so the caller's judgment of them, of the loader's and
// the libraries' files as of the program's.
//
// The program is held as a file like any other: its real file, and on macOS every symbolic link on
// its path, must be beneath a folder held, the added ones included, or it is ErrUsage naming the
// folder, which RunHolding never adds (Folders says why each system needs it). On macOS this is
// RunHolding's own check, not the system sandbox's, and a link swapped after it walked the path is
// the same owner's and writers' to do.
//
// Every error of Run that says the block could not be set matches ErrNoBlock,
// and a program that could not be started behind a block that was set does
// not, so a caller tells the two apart with errors.Is, never by the text. So
// does every error of RunHolding. ErrNoBlock wraps errors.ErrUnsupported.
//
// Check asks first: it checks the program and the folders as RunHolding does and asks the system whether it gives
// the block, and sets nothing and starts nothing. A caller that asks it before it starts its own
// work learns then, not after, that the program would be refused, and which Block the system gives:
// what it closes, and what it leaves open. What a caller may claim, what the block does not close
// and the probe are in https://github.com/birdtable/go-proc/blob/main/docs/sandbox.md.
package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"

	ll "github.com/landlock-lsm/go-landlock/landlock/syscall"
)

// ErrUsage is Run or RunHolding given a program not named by its absolute
// path, or a folder RunHolding or Check would not hold, or a program whose
// load folders they cannot tell, or one the folders do not hold. Nothing is
// set or run.
var ErrUsage = errors.New("the block does not take it, so nothing is set or run")

// ErrNoBlock is matched, with errors.Is, by every error of Run and Check that says the block could
// not be set: a system that gives none, which matches ErrNoSystem too, a step the kernel refused, a standard stream that is a
// socket of another family, a probe that did not hold, and on macOS the system sandbox that could
// not be started or did not set the profile in its dry run. The program was not run. It wraps
// errors.ErrUnsupported, which every such error so matches too. Run's other errors are ErrUsage,
// and the program that could not be started behind a block that was set, which do not match it. The
// error's text stays the step's own: ErrNoBlock's text is never in it.
var ErrNoBlock = fmt.Errorf("the network block could not be set, so the program is not run: %w", errors.ErrUnsupported)

// errUsage and errNoBlock are what Run, RunHolding and Check return and match for ErrUsage and
// ErrNoBlock: the values those were given here, so a value a caller or a dependency stores in either
// later, nil included, changes what a match finds and never lets a program run unblocked.
var errUsage, errNoBlock = ErrUsage, ErrNoBlock

// noBlock is a refusal that left the block unset: it says what err says,
// and it matches ErrNoBlock as well as everything err matches.
type noBlock struct{ err error }

func (e noBlock) Error() string   { return e.err.Error() }
func (e noBlock) Unwrap() []error { return []error{errNoBlock, e.err} }

// ErrNoSystem is matched, with errors.Is, by the error of Run, RunHolding and Check on a system
// that gives no block at all, Windows and FreeBSD among them: a known gap of the system, which no
// change to the host fills. Each such error matches ErrNoBlock too. A step that this host refused
// where the system gives the block, as on Linux an older kernel, Landlock off or no seccomp filter
// for the architecture, or on macOS a system sandbox that could not be started, is the step's own
// error and matches ErrNoBlock alone, never ErrNoSystem.
var ErrNoSystem = errors.New("this system gives no network block netblock can set (Linux sets its own filter, macOS runs " +
	sandboxProgram + "), so the program is not run with the network open")

// errNoSystem is what a system with no block returns and matches for ErrNoSystem: the value it was
// given here, as errUsage and errNoBlock are, so a value stored in it later changes no refusal.
var errNoSystem = ErrNoSystem

// runNone is Run on a system with no block: it refuses, and runs nothing.
func runNone() error { return noBlock{errNoSystem} }

// Run sets the block on this process and execs the program at path, its absolute path, with args,
// its arguments after its name; environ is the environment the program gets. It returns only with
// why the program did not run: ErrUsage, the step of the block that failed, which matches
// ErrNoBlock, or the exec's error.
func Run(path string, args, environ []string) error {
	argv, err := usage(path, args)
	if err != nil {
		return err
	}
	return runHere(named{}, path, argv, environ)
}

// usage is the program's argv, path then args, in a slice of its own, or ErrUsage, naming path,
// when path is not absolute.
func usage(path string, args []string) ([]string, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("the program %q is not named by its absolute path: %w", path, errUsage)
	}
	return append([]string{path}, args...), nil
}

// Block is what the block Check found closes on this system: each field is
// true where it is closed to the program and every process it starts, and
// false where it is left open. Check returns the zero Block, nothing closed,
// with every error.
type Block struct {
	// Network is every socket that reaches the network, and a Unix socket
	// made outside the block, on a path or, on Linux, abstract.
	Network bool
	// Daemons is a daemon outside asked to act for the program, but the
	// service manager, which ServiceManager says: by its Unix socket, and on
	// macOS by mach-lookup, the directory service's alone left open.
	Daemons bool
	// Terminal is input pushed into a terminal shared with a process
	// outside, by TIOCSTI, and on Linux TIOCLINUX.
	Terminal bool
	// Signals is a signal to a process outside: Linux closes it by Landlock's
	// scope, and macOS's profile leaves it open.
	Signals bool
	// ServiceManager is the system's service manager, which starts a program
	// outside the block for whoever asks: Linux's is reached by a Unix
	// socket, which the block closes, and macOS's launchd by the bootstrap
	// port every process holds, which the profile leaves open.
	ServiceManager bool
}

// blockOn is the Block the block gives on the system goos names: the zero
// Block where it gives none.
func blockOn(goos string) Block {
	switch goos {
	case "linux":
		return Block{Network: true, Daemons: true, Terminal: true, Signals: true, ServiceManager: true}
	case "darwin":
		return Block{Network: true, Daemons: true, Terminal: true}
	}
	return Block{}
}

// Folders are the folders a program behind RunHolding's block may reach, each by its absolute path:
// beneath ReadWrite it reads, runs, writes, makes and removes, and beneath ReadOnly it reads and
// runs. Every other file is denied, reads included. The program itself is a file like any other, so
// its folder is among them: the folder of its real file, the one its path ends on once every
// symbolic link on it is followed, and on macOS the folder of each of those links too, as the
// system sandbox refuses to follow a link it does not reach. RunHolding and Check walk the path and
// refuse with ErrUsage a program these folders do not hold, naming the folder; they never add it.
// On Linux Landlock holds the exec of the real file as well. On macOS the system sandbox does not
// hold a program's exec, so the hold of the program is RunHolding's own check, of the path as it
// walked it. The folders of its loader and libraries are not the caller's: RunHolding adds them.
type Folders struct {
	ReadWrite []string
	ReadOnly  []string
}

// RunHolding is Run with the program's files held to folders. It takes at least one folder: Run is
// the block that leaves the files open. A folder that is not an absolute path, does not exist or is
// not a folder, one named twice, under any name, and a read-only folder beneath a read-write one,
// which could be written, are ErrUsage, and nothing is set or run. So is a program whose real file
// no folder reaches, or, on macOS, one whose path goes through a link in a folder none reaches, as
// Folders says, once the load folders are added. Every error that says the block could not be set
// matches ErrNoBlock, as Run's.
//
// To the folders it adds, read-only, those the program needs to load, which no caller names. On
// Linux they are the folder of its loader and of each library it needs, by their real paths, as
// its loader finds them behind the block: with no /etc/ld.so.cache, and with no LD_LIBRARY_PATH,
// LD_PRELOAD or LD_AUDIT, which behind the block reach only folders the caller granted itself. A
// program that names no loader, as a Go program built without cgo, adds none. Each file is read as
// the kernel and its loader read it, by its program headers. On Linux a #! script, whose
// interpreter is the program to run, a file that is no ELF file or names two loaders, a loader
// that is neither glibc's nor musl's for this machine, a library that is not found, a file found
// for one that is a program, a search list it cannot follow, and a loader or a library whose real
// folder is the root or /usr itself, which no load grants, are ErrUsage, naming the file. On macOS
// nothing is read and none is added: the system's libraries need no folder, and a #! script is not
// refused.
func RunHolding(f Folders, path string, args, environ []string) error {
	argv, err := usage(path, args)
	if err != nil {
		return err
	}
	n, err := holding(f)
	if err != nil {
		return err
	}
	if n, err = program(n, path); err != nil {
		return err
	}
	return runHere(n, path, argv, environ)
}

// loading is n with the folders the program at path, whose real file is real, needs to load, as
// this system answers them.
func loading(n named, path, real string) (named, error) {
	dirs, err := loadHere(path, real)
	if err != nil {
		return named{}, err
	}
	return n.with(dirs), nil
}

// with is n with each of dirs, real paths, held read-only, but one that is a folder of n's or
// beneath one, which the program reads already.
func (n named) with(dirs []string) named {
	for _, d := range dirs {
		within := func(held string) bool { return beneath(d, held, string(filepath.Separator)) }
		if !slices.ContainsFunc(n.readWrite, within) && !slices.ContainsFunc(n.readOnly, within) {
			n.readOnly = append(slices.Clip(n.readOnly), d)
		}
	}
	return n
}

// Check says whether Run, or RunHolding with f and the program at path, could set the block here,
// and sets nothing and starts nothing. When f names folders, path and each folder are checked as
// RunHolding checks them, the program's load folders are told and held with the rest, and what
// RunHolding would refuse is ErrUsage, naming it; with none, the check is Run's, and path is not
// read. Then the system is asked whether it gives the block: on Linux the seccomp filter for this architecture and Landlock
// at ABI 6 or later with erratum 2, and with folders /proc/self/fd, by which RunHolding names each
// folder, and a temporary folder outside them for the probe; on macOS the system sandbox's program,
// and elsewhere nothing, so the answer there is no, matching ErrNoSystem. Each such no matches ErrNoBlock. A nil error is
// a block the run would set, though a step the kernel refuses, or a probe that does not hold, can
// still refuse the run itself, and with it the Block that block is: on Linux every field true, and
// on macOS Signals and ServiceManager false.
func Check(f Folders, path string) (Block, error) {
	var n named
	if len(f.ReadWrite)+len(f.ReadOnly) > 0 {
		_, err := usage(path, nil)
		if err == nil {
			n, err = holding(f)
		}
		if err == nil {
			n, err = program(n, path)
		}
		if err != nil {
			return Block{}, err
		}
	}
	if err := checkHere(n); err != nil {
		return Block{}, err
	}
	return blockHere(), nil
}

// named is the folders a block holds the files to, each by its real path,
// with no symbolic link in it: the empty one leaves the files open.
type named struct {
	readWrite, readOnly []string
}

// holds says whether n holds the files at all.
func (n named) holds() bool { return len(n.readWrite)+len(n.readOnly) > 0 }

// holding is RunHolding's folders, checked: each by its real path, or
// ErrUsage, before anything is set.
func holding(folders Folders) (named, error) {
	var n named
	seen := map[string]string{}
	for _, list := range []struct {
		kind  string
		names []string
		into  *[]string
	}{{"read-write", folders.ReadWrite, &n.readWrite}, {"read-only", folders.ReadOnly, &n.readOnly}} {
		for _, name := range list.names {
			resolved, err := realFolder(list.kind, name)
			if err != nil {
				return named{}, err
			}
			if first, ok := seen[resolved]; ok {
				return named{}, fmt.Errorf("the folder %q is named twice, as %s and as %q: %w", resolved, first, name, errUsage)
			}
			seen[resolved] = fmt.Sprintf("the %s folder %q", list.kind, name)
			*list.into = append(*list.into, resolved)
		}
	}
	if !n.holds() {
		return named{}, fmt.Errorf("RunHolding names no folder, and Run is the block that leaves the files open: %w", errUsage)
	}
	for _, ro := range n.readOnly {
		for _, rw := range n.readWrite {
			if beneath(ro, rw, string(filepath.Separator)) {
				return named{}, fmt.Errorf("the read-only folder %q is beneath the read-write folder %q, so it could be written: %w", ro, rw, errUsage)
			}
		}
	}
	return n, nil
}

// realFolder is name, a kind folder, by its real path, or ErrUsage when it
// is not an absolute path, does not exist or is not a folder.
func realFolder(kind, name string) (string, error) {
	if !filepath.IsAbs(name) {
		return "", fmt.Errorf("the %s folder %q is not an absolute path: %w", kind, name, errUsage)
	}
	resolved, err := filepath.EvalSymlinks(name)
	if err == nil {
		var st os.FileInfo
		if st, err = os.Stat(resolved); err == nil && !st.IsDir() {
			err = errors.New("it is not a folder")
		}
	}
	if err != nil {
		return "", fmt.Errorf("the %s folder %q cannot be held (%w): %w", kind, name, err, errUsage)
	}
	return resolved, nil
}

// beneath says whether p is dir or beneath it, by the separator sep.
// holding compares host paths by the host's separator: the folders are
// named on this host and resolved here by realFolder, Windows among them.
func beneath(p, dir, sep string) bool {
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, sep)+sep)
}

// beneathSlash is beneath for a Linux path, cleaned by the path package and compared by "/" on
// every host. The Linux block's checks compare Linux paths, checkLinux's temporary folder and
// probeOutside's probe folder, against the folders held, and their tests run that logic on a fake
// kernel on every system, Windows among them.
func beneathSlash(p, dir string) bool { return beneath(path.Clean(p), path.Clean(dir), "/") }

// landlockScopeABI is the first Landlock ABI with the scopes the block sets.
const landlockScopeABI = 6

// landlockErratumSignalScope is erratum 2 of the kernel's Landlock errata, bit 1 of the mask
// landlock_create_ruleset answers when asked for them: the fix that lets a thread signal the other
// threads of its own process when they are in another domain. Without it the signal scope is
// stricter, not weaker, and the signals of processes outside are scoped out all the same. But
// go-landlock takes a kernel at ABI 6 or later without it for ABI 5, and so cannot make the ruleset
// with the scopes, and the block refuses such a kernel: on those kernels the block is narrower than
// it was before go-landlock made the ruleset.
const landlockErratumSignalScope = 1 << 1

// The Landlock scopes the block sets, as go-landlock names them.
const (
	landlockScopeAbstractUnixSocket = ll.ScopeAbstractUnixSocket
	landlockScopeSignal             = ll.ScopeSignal
)

// landlockResolveUnixABI is the first Landlock ABI with landlockAccessFSResolveUnix
// (LANDLOCK_ACCESS_FS_RESOLVE_UNIX), the right to connect or send to a Unix socket on a path made
// outside the ruleset. Handled and granted nowhere, it refuses every such socket, and leaves those
// the program makes itself.
const (
	landlockResolveUnixABI      = 9
	landlockAccessFSResolveUnix = ll.AccessFSResolveUnix
)

// The Landlock file rights the block grants, as go-landlock names them: landlockAccessFSABI6 is
// every right ABI 6 has, EXECUTE (bit 0) to IOCTL_DEV (bit 15), among them writing, making,
// removing, truncating, linking or moving a file across folders (REFER), and an ioctl on a device;
// landlockAccessFSRead is running, reading a file and listing a folder. RESOLVE_UNIX, from ABI 9,
// is not among them, and is granted nowhere.
const (
	landlockAccessFSABI6 = ll.AccessFSIoctlDev<<1 - 1
	landlockAccessFSRead = ll.AccessFSExecute | ll.AccessFSReadFile | ll.AccessFSReadDir
)

// pathRule grants allowed beneath dir, a folder by its real path.
type pathRule struct {
	dir     string
	allowed uint64
}

// rules are n's folders as the ruleset grants them: every right of ABI 6 beneath a read-write
// folder, and reading and running beneath a read-only one.
func (n named) rules() []pathRule {
	var out []pathRule
	for _, d := range n.readWrite {
		out = append(out, pathRule{d, landlockAccessFSABI6})
	}
	for _, d := range n.readOnly {
		out = append(out, pathRule{d, landlockAccessFSRead})
	}
	return out
}

// held is the probe: two Unix sockets on a path, made before the block is set, in a folder made for
// them. One is a stream that listens, the other a datagram socket. Beside them is a file, its
// owner's to read and write, for a block that holds the files.
type held struct {
	stream, dgram         int
	dir                   string
	streamPath, dgramPath string
	filePath              string
}

// reached is what met the probe's ways to its sockets and its file: a connect to the stream's path,
// a datagram sent by sendto to the datagram socket's, an AF_INET socket made, and the file opened
// to read and opened to write. nil is a way that went through.
type reached struct {
	stream, dgram error
	inet          error
	read, write   error
}

// The probe's refusals: each names the check that failed. A Control one is a refusal that would prove
// nothing, because the same act failed before the block was set.
var (
	errProbeInetAnswered = errors.New("an AF_INET socket was made behind the block, so the seccomp filter is not set")
	errProbeInetControl  = errors.New("an AF_INET socket could not be made before the block was set, so a refusal after it would not be the filter's")
	errProbeAnswered     = errors.New("a Unix socket on a path made outside the block answered behind it, so the block does not hold")
	errProbeControl      = errors.New("the probe's Unix sockets could not be reached before the block was set, so a refusal after it would not be the block's")
	errProbeFileAnswered = errors.New("a file made outside the folders was reached behind the block, so the block does not hold the files")
	errProbeFileControl  = errors.New("the probe's file could not be read and written before the block was set, so a refusal after it would not be the block's")
	errProbeInFolder     = errors.New("the probe's folder, made under the temporary folder, is beneath a folder the block grants, so a refusal of its file cannot be shown. Name a temporary folder outside them in TMPDIR")
)

// landlockAsker is the kernel's Landlock ABI and errata, which the block and Check both ask.
type landlockAsker interface {
	// landlockABI is the Landlock ABI the kernel speaks and the mask of the errata it has fixed, or
	// an error when Landlock is not built in or not enabled.
	landlockABI() (abi, errata int, err error)
}

// linuxAsker is what Check asks of Linux, none of which sets anything: with
// folders, also what RunHolding needs for them.
type linuxAsker interface {
	landlockAsker
	// procFDs says whether /proc/self/fd is there, by which restrict names
	// each rule's folder to go-landlock.
	procFDs() error
	// tempFolder is the real path of the temporary folder, under which
	// holdOutside makes the probe's folder.
	tempFolder() (string, error)
}

// streamKernel is what the block asks of the system on every system that
// blocks: the streams' marking and reading, and the exec.
type streamKernel interface {
	// closeAtExec marks every descriptor from fd up to close at exec.
	closeAtExec(fd int) error
	// stdioFamilies is the address family of each of stdin, stdout and stderr that is a socket, 0
	// for one that is not, and -1 for a socket whose family cannot be told.
	stdioFamilies() ([3]int, error)
	exec(prog string, argv, env []string) error
}

// kernel is what the Linux block asks of the system, in the order the steps ask it.
type kernel interface {
	streamKernel
	landlockAsker
	noNewPrivs() error
	// holdOutside makes the probe's two Unix sockets on a path, a stream
	// that listens and a datagram socket, and its file, in a folder of their
	// own, by its real path, before the block is set.
	holdOutside() (held, error)
	// restrict makes a ruleset that handles the file accesses handledFS, granted only by rules, and
	// the scopes, and restricts this thread to it.
	restrict(handledFS, scoped uint64, rules []pathRule) error
	// loadFilter sets the seccomp filter on every thread of the process.
	loadFilter(prog []sockFilter) error
	// reach connects a new socket to h's stream, sends a datagram to h's datagram socket by sendto,
	// and opens h's file to read and to write, and says what met each.
	reach(h held) reached
	// release closes h and removes its folder, which the thread the ruleset restricts may not.
	release(h held) error
}

// macKernel is what the macOS block asks of the system: the streams and the
// exec, and the system sandbox's dry run, which only macOS has.
type macKernel interface {
	streamKernel
	// dryRun runs path with args as a child, with no environment and no stdin, waits for it, and
	// says its exit code, -1 for one a signal, the bound or the output cap ended, and the first of
	// what it wrote to stderr; err is a child that could not be run, never one that ran.
	dryRun(path string, args []string) (code int, stderr []byte, err error)
}

// errStdioSocket is a stdin, stdout or stderr that is a socket of a family that is not AF_UNIX.
var errStdioSocket = errors.New("a standard stream of the blocked program is a socket of a family that is not AF_UNIX")

// runLinux is Run's Linux block through k, for the machine arch, with the
// files held to n: prog by its absolute path, argv with prog first.
func runLinux(k kernel, arch string, n named, prog string, argv, environ []string) error {
	// Landlock restricts the thread that asks, and the exec runs on it.
	runtime.LockOSThread()
	if err := applyLinux(k, arch, n); err != nil {
		return noBlock{fmt.Errorf("%w: the program is not run", err)}
	}
	return fmt.Errorf("%s could not be started behind the block: %w", prog, execd(k, prog, argv, environ))
}

// execd execs path with argv through k, and says why it did not: the exec's
// own error, or that the exec returned. It never returns nil.
func execd(k streamKernel, path string, argv, environ []string) error {
	err := k.exec(path, argv, environ)
	if err == nil {
		err = errors.New("the exec returned")
	}
	return err
}

// applyLinux sets the Linux block on this process through k, for the machine arch, with the files
// held to n, and says the first step that failed. Nothing is run after a failure.
func applyLinux(k kernel, arch string, n named) error {
	f, err := filterFor(arch)
	if err != nil {
		return err
	}
	if err := k.noNewPrivs(); err != nil {
		return fmt.Errorf("no_new_privs could not be set: %w", err)
	}
	if err := streams(k); err != nil {
		return err
	}
	abi, err := landlockReady(k)
	if err != nil {
		return err
	}
	// From ABI 9 Landlock refuses a Unix socket on a path made outside the
	// block and lets the program's own answer; before it, the filter refuses
	// every call that reaches a socket by its address.
	var handledFS uint64
	closeSends := abi < landlockResolveUnixABI
	if !closeSends {
		handledFS = landlockAccessFSResolveUnix
	}
	if n.holds() {
		handledFS |= landlockAccessFSABI6
	}
	prog, err := f.program(closeSends)
	if err != nil {
		return err
	}
	h, err := k.holdOutside()
	if err != nil {
		return fmt.Errorf("the probe's Unix sockets outside the block could not be made: %w", err)
	}
	err = probeOutside(h, n)
	if err == nil {
		err = restrictAndProbe(k, h, handledFS, n, prog, refusal(closeSends))
	}
	if rerr := k.release(h); err == nil && rerr != nil {
		err = fmt.Errorf("the probe's Unix sockets could not be removed: %w", rerr)
	}
	return err
}

// filterFor is the seccomp filter for the machine arch, or why there is none.
func filterFor(arch string) (filterArch, error) {
	f, ok := filterArches()[arch]
	if !ok {
		return filterArch{}, fmt.Errorf("netblock holds no seccomp filter for the architecture %q, so the network cannot be blocked", arch)
	}
	return f, nil
}

// landlockReady is the Landlock ABI k speaks, or why the block cannot be set
// on it: no Landlock, an ABI below 6, or an ABI of 6 or later without erratum
// 2. It sets nothing.
func landlockReady(k landlockAsker) (int, error) {
	abi, errata, err := k.landlockABI()
	switch {
	case err != nil:
		return 0, fmt.Errorf("landlock is not here, so the abstract Unix sockets outside cannot be scoped out: %w", err)
	case abi < landlockScopeABI:
		return 0, fmt.Errorf("landlock speaks ABI %d, and scoping out the abstract Unix sockets outside needs %d (Linux 6.12)", abi, landlockScopeABI)
	case errata&landlockErratumSignalScope == 0:
		return 0, fmt.Errorf("landlock speaks ABI %d without erratum 2, so go-landlock takes it for ABI 5 and cannot make the ruleset with the scopes; the signal scope there is stricter, not weaker", abi)
	}
	return abi, nil
}

// checkLinux is Check's ask of the Linux system through k, for the machine arch: the filter, then
// Landlock, and when n holds folders, /proc/self/fd, which RunHolding needs to name them, and a
// temporary folder not beneath them, where the probe's folder is made, with nothing set.
func checkLinux(k linuxAsker, arch string, n named) error {
	if _, err := filterFor(arch); err != nil {
		return noBlock{err}
	}
	if _, err := landlockReady(k); err != nil {
		return noBlock{err}
	}
	if !n.holds() {
		return nil
	}
	if err := k.procFDs(); err != nil {
		return noBlock{fmt.Errorf("/proc/self/fd is not there, and RunHolding names each folder to go-landlock by it: %w", err)}
	}
	temp, err := k.tempFolder()
	if err != nil {
		return noBlock{fmt.Errorf("the temporary folder, where the probe's folder is made, cannot be found: %w", err)}
	}
	for _, d := range append(append([]string{}, n.readWrite...), n.readOnly...) {
		if beneathSlash(temp, d) {
			return noBlock{fmt.Errorf("%w: the temporary folder %q is beneath %q", errProbeInFolder, temp, d)}
		}
	}
	return nil
}

// checkMac is Check's ask on macOS: the system sandbox's program, as stat
// finds it, is a file that runs.
func checkMac(stat func(string) (os.FileInfo, error)) error {
	st, err := stat(sandboxProgram)
	if err == nil && (st.IsDir() || st.Mode().Perm()&0o111 == 0) {
		err = errors.New("it is not a program")
	}
	if err != nil {
		return noBlock{fmt.Errorf("%s cannot be run, so no block can be set: %w", sandboxProgram, err)}
	}
	return nil
}

// refusal is the one error the block gives a Unix socket on a path made outside it: the filter's
// EPERM when it closes the sends, and Landlock's EACCES when the ruleset handles RESOLVE_UNIX.
func refusal(closeSends bool) syscall.Errno {
	if closeSends {
		return syscall.EPERM
	}
	return syscall.EACCES
}

// probeOutside refuses a probe whose folder is beneath one of n's folders,
// where the block grants its file.
func probeOutside(h held, n named) error {
	for _, d := range append(append([]string{}, n.readWrite...), n.readOnly...) {
		if beneathSlash(h.dir, d) {
			return fmt.Errorf("%w: %q is beneath %q", errProbeInFolder, h.dir, d)
		}
	}
	return nil
}

// restrictAndProbe reaches h's sockets through k, which must go through, sets the Landlock ruleset
// and the filter, and reaches them again, which must meet want on both ways: a block that lets one
// answer, or refuses it with another error, which the host may give, is not run behind. An AF_INET
// socket is made too, before and behind, where it must meet the filter's EPERM whatever want is, so
// a filter not set is shown on every ABI, Landlock refusing the Unix sockets from 9 on. When n
// holds the files, h's file is read and written as well, before the ruleset and behind it, where
// each must meet Landlock's EACCES.
func restrictAndProbe(k kernel, h held, handledFS uint64, n named, prog []sockFilter, want syscall.Errno) error {
	r := k.reach(h)
	if r.stream != nil || r.dgram != nil {
		return fmt.Errorf("%w: on the stream, %s, and on the datagram, %s", errProbeControl, wentOrNot(r.stream), wentOrNot(r.dgram))
	}
	if r.inet != nil {
		return fmt.Errorf("%w: %w", errProbeInetControl, r.inet)
	}
	if n.holds() && (r.read != nil || r.write != nil) {
		return fmt.Errorf("%w: the read, %s, and the write, %s", errProbeFileControl, wentOrNot(r.read), wentOrNot(r.write))
	}
	if err := k.restrict(handledFS, landlockScopeAbstractUnixSocket|landlockScopeSignal, n.rules()); err != nil {
		return fmt.Errorf("the landlock ruleset could not be set: %w", err)
	}
	if err := k.loadFilter(prog); err != nil {
		return fmt.Errorf("the seccomp filter could not be set on every thread: %w", err)
	}
	r = k.reach(h)
	ways := []probeWay{
		{"connect to its stream socket", r.stream, errProbeAnswered, want},
		{"datagram to its datagram socket", r.dgram, errProbeAnswered, want},
		{"AF_INET socket", r.inet, errProbeInetAnswered, syscall.EPERM},
	}
	if n.holds() {
		ways = append(ways,
			probeWay{"read of its file", r.read, errProbeFileAnswered, syscall.EACCES},
			probeWay{"write of its file", r.write, errProbeFileAnswered, syscall.EACCES})
	}
	for _, way := range ways {
		switch {
		case way.err == nil:
			return fmt.Errorf("%w: the probe's %s", way.answered, way.name)
		case !errors.Is(way.err, way.want):
			return fmt.Errorf("the probe's %s was refused with %w, but the block's refusal is %s, so the refusal may be the host's, and the block's is not shown", way.name, way.err, way.want.Error())
		}
	}
	return nil
}

// probeWay is one of the probe's ways behind the block: what met it, the
// error of a way that answered, and the block's own refusal.
type probeWay struct {
	name     string
	err      error
	answered error
	want     syscall.Errno
}

// wentOrNot says how one of the probe's ways went: through, or the error that met it.
func wentOrNot(err error) string {
	if err == nil {
		return "it went through"
	}
	return err.Error()
}

// streams marks every descriptor above stderr to close at exec and refuses a standard stream that
// is a socket of a family other than AF_UNIX, on every system that blocks.
func streams(k streamKernel) error {
	if err := k.closeAtExec(3); err != nil {
		return fmt.Errorf("the descriptors above stderr could not be marked to close at exec: %w", err)
	}
	fams, err := k.stdioFamilies()
	if err != nil {
		return fmt.Errorf("the standard streams could not be read: %w", err)
	}
	for fd, fam := range fams {
		if fam != 0 && fam != afUnix {
			return fmt.Errorf("%w: descriptor %d is of family %d", errStdioSocket, fd, fam)
		}
	}
	return nil
}

// sandboxProgram is the macOS system sandbox, by its absolute path, and
// macosProfile the profile it runs the program under, the rest allowed:
//   - every network operation denied;
//   - the ioctl TIOCSTI (_IOW('t', 114, char)) denied, which pushes a byte
//     into a terminal's input, so a shell outside the sandbox that shares
//     the terminal would run a line the program wrote;
//   - every mach-lookup denied but the directory service's, which answers
//     user and group lookups and starts nothing. A lookup is how a program
//     reaches a daemon by its name; launchd itself is reached by the
//     bootstrap port, which no clause tried (mach-bootstrap, mach-priv*, mach-register, ipc*) closes;
//   - another process's task ports denied (mach-priv-task-port). On macOS 27.2 a caller signed
//     with no entitlement is refused the control port of a target signed with get-task-allow,
//     which a debugger attaches by, and its read, inspect and name ports. A caller
//     signed with the debugger entitlement was not tried.
//
// macosNetwork and macosRest are the profile's two halves, between which a block that holds the
// files puts its file clauses, so the ioctl, mach-lookup and task-port clauses stay last.
const (
	sandboxProgram = "/usr/bin/sandbox-exec"
	macosNetwork   = `(version 1)(allow default)(deny network*)`
	macosRest      = `(deny file-ioctl (ioctl-command #x80017472))(deny mach-lookup)(allow mach-lookup (global-name "com.apple.system.opendirectoryd.libinfo"))(deny mach-priv-task-port)`
	macosProfile   = macosNetwork + macosRest
)

// macosFiles is the clause that denies every file read and write, and macosRoot the one read it
// gives back, the root folder's own list, without which the system sandbox cannot start a program.
// The rest is granted beneath the folders alone.
const (
	macosFiles = `(deny file-read* file-write*)`
	macosRoot  = `(allow file-read-data (literal "/"))`
)

// macosHolding is the profile that holds the files to n, and the sandbox's -D parameters that carry
// n's folders. Each folder is a parameter, named by its list and its place alone, which the profile
// reads by (param ...), so a folder's name is never in the profile's text and no character in it
// changes the profile. With no folder it is the profile that leaves the files open.
func macosHolding(n named) (string, []string) {
	if !n.holds() {
		return macosProfile, nil
	}
	var params []string
	clause := func(rights, key string, dirs []string) string {
		if len(dirs) == 0 {
			return ""
		}
		var c strings.Builder
		c.WriteString("(allow " + rights)
		for i, d := range dirs {
			name := key + strconv.Itoa(i)
			params = append(params, "-D", name+"="+d)
			c.WriteString(` (subpath (param "` + name + `"))`)
		}
		return c.String() + ")"
	}
	ro := clause("file-read*", "RO", n.readOnly)
	rw := clause("file-read* file-write*", "RW", n.readWrite)
	return macosNetwork + macosFiles + macosRoot + ro + rw + macosRest, params
}

// runMac is Run's macOS block through k, with the files held to n: the
// streams checked and the descriptors above stderr marked, then prog run by
// the system sandbox, after a dry run of the same profile.
func runMac(k macKernel, n named, prog string, argv, environ []string) error {
	if err := streams(k); err != nil {
		return noBlock{fmt.Errorf("%w: the program is not run", err)}
	}
	profile, params := macosHolding(n)
	return sandboxed(k, profile, params, prog, argv, environ)
}

// macNoop is the program of the macOS block's dry run: it reads nothing and exits 0, and a held
// profile that grants no folder still runs it, as a system program.
const macNoop = "/usr/bin/true"

// sandboxed runs prog by the system sandbox with profile and its params, through k. The exec is the
// sandbox's: once it succeeds, nothing here runs again, and a sandbox that then fails to set the
// profile would exit before the program with a code a program might give too (65 for a profile or a
// parameter it refuses). So the same profile and params are first set in a dry run, around macNoop
// in place of the program, and one that does not exit 0 refuses the run, matching ErrNoBlock,
// before the exec. The descriptors above stderr are marked to close at exec again after the dry
// run, and one that cannot be refuses the run too. An exec that fails or returns leaves the block
// unset too. A program the sandbox cannot start, behind a profile it set, is its exit after the
// exec (71), which Run never sees.
func sandboxed(k macKernel, profile string, params []string, prog string, argv, environ []string) error {
	head := slices.Concat([]string{sandboxProgram}, params, []string{"-p", profile})
	code, said, err := k.dryRun(sandboxProgram, slices.Concat(head[1:], []string{macNoop}))
	switch {
	case err != nil:
		return noBlock{fmt.Errorf("%s could not be started for its dry run of the profile, so no block is set and %s is not run: %w", sandboxProgram, prog, err)}
	case code != 0:
		return noBlock{fmt.Errorf("%s did not set the profile in its dry run (it exited %d: %q), so no block is set and %s is not run", sandboxProgram, code, firstLine(said), prog)}
	}
	// The dry run's fork and wait sit between streams' marking and the exec, up to its bound; a
	// descriptor another goroutine opened unmarked in that time would reach the program, so the
	// descriptors are marked again. The marking before the dry run stays: its child would inherit
	// what is open otherwise. The exec takes no lock and stops no other thread, and macOS makes a
	// pipe or a socket and marks it in two calls, under ForkLock read-held, so ForkLock is held
	// from this marking into the exec, and let go where the exec returns; the dry run's fork takes
	// it, so not before. One made or received under no lock is not held off by
	// it: the exec is a posix_spawn that closes every descriptor but 0 to 2.
	syscall.ForkLock.Lock()
	defer syscall.ForkLock.Unlock()
	if err := k.closeAtExec(3); err != nil {
		return noBlock{fmt.Errorf("the descriptors above stderr could not be marked to close at exec again after the dry run, so no block is set and %s is not run: %w", prog, err)}
	}
	err = execd(k, sandboxProgram, slices.Concat(head, argv), environ)
	return noBlock{fmt.Errorf("%s could not be started, so no block is set and %s is not run: %w", sandboxProgram, prog, err)}
}

// firstLine is the first line of what a dry run wrote, which says why the
// sandbox refused: the rest is a backtrace.
func firstLine(b []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	return line
}
