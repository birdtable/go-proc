package proc

import (
	"cmp"
	"errors"
	"io"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix" //nolint:depguard // arch.ports.23: the shim asks its ambient set, mounts the tree's proc and cgroup v2 and clears its capabilities by prctl, mount and capset, in x/sys alone
)

// shimMark is the argv[0] of the shim a Contained run starts its program through, the caller's own
// binary by its proc's self/exe, then its report's descriptor, the file it writes 0 to, where it
// mounts the tree's proc, the file of the tree's mounts it reads there, the program's path, the
// folder the program runs in (Cmd.Dir; empty is the caller's own) and the program's argv. The shim
// starts in the caller's folder, as the start enters none, and with no environment; the program's is on the descriptor after the
// report's, each variable ended by a NUL. A process started by hand with shimMark as its argv[0] and
// no CAP_SYS_RESOURCE ambient writes its two bytes of report to the descriptor its argv names, and
// exits 127.
const shimMark = "go-proc-contained-shim"

// shimEINTR is shimMark for a run whose shim fails the first try of each fstat, getcwd, chdir, openat2
// and fchdir with EINTR, before it is made: a test's seam, which Cmd.seam alone gives a run.
const shimEINTR = shimMark + "-eintr"

// shimRun is the argv[0] of the shim's second stage: the shim execs itself by it once it has set
// no_new_privs and cleared its capabilities, so the process that writes shimReady and execs the
// program is a new image whose every thread started with none: the Go runtime's other
// threads hold what the first stage's locked thread dropped until that exec, and the caller reads each
// thread. Its argv is the report's descriptor, the program's path and the program's argv; its
// environment is shimRunEnv alone, none of the caller's, and it reads the program's from the descriptor
// after the report's. The one after that is the caller's go-ahead (shimGo).
const shimRun = shimMark + "-run"

// shimGo is the byte the caller writes to the second stage once it has read the run's threads
// (shimTook): the stage execs the program only after it, so what the caller reads is the stage
// itself, waiting, and never the program's first steps. A caller that refuses the run writes none.
const shimGo byte = 'g'

// init is the shim, in a process started with shimMark as its argv[0]; it returns at once in every
// other. Go gives a start no hook between its clone and its exec, so the shim is a process of its own.
func init() { //nolint:gochecknoinits // the shim runs before main, in the caller's own binary
	if len(os.Args) > 7 && (os.Args[0] == shimMark || os.Args[0] == shimEINTR) {
		shim(os.Args[1], os.Args[2:7], os.Args[7:], os.Args[0] == shimEINTR)
	}
	if len(os.Args) > 3 && os.Args[0] == shimRun {
		shimExecProgram(os.Args[1], os.Args[2], os.Args[3:])
	}
}

// shim writes 0 to at[0], the tree's user.max_cgroup_namespaces, which the kernel counts for every
// namespace below, so no process of the tree makes a cgroup namespace; makes every mount of the
// tree's private, so none the host makes later reaches it; mounts the tree's proc at at[1], so its
// /proc lists no process outside it; mounts a cgroup v2 of the tree's cgroup namespace, rooted at
// the run's cgroup, over each cgroup mount at[2] lists, and enters its folder, at[4] or the caller's
// own, then takes it again by its path, through them, a Dir through a proc link among them, so the tree names no cgroup outside its own, and so the start enters no folder of the Cmd's, and a start's errno
// is never the Cmd's own;
// closes at the exec every descriptor of its own above those the run hands the tree, which Go's start
// leaves open, and refuses a directory on any the run hands it, or one it cannot check, each of
// which stays on the caller's mounts, under no cover; sets no_new_privs and clears its capabilities, so no
// exec of the tree gains one by a file's capabilities or setuid, nor unmounts what the shim mounted;
// then execs itself by shimRun, whose second stage writes shimReady, waits for the caller's go-ahead
// and execs the program at at[3]. It needs CAP_SYS_RESOURCE and CAP_SYS_ADMIN ambient, as the
// run's start gives them: a shim that holds no CAP_SYS_RESOURCE ambient, as a setuid or file-capability
// exec starts it, exits 127 at its first step, before it touches its report, and writes nothing. A step it cannot take is reported, as its byte and errno, and the shim exits. The
// second stage writes shimReady once the shim's steps are done, so a report without it is a shim that
// ended before them, and every thread that writes it started with no capability and no_new_privs set.
// The steps are on one thread, whose credentials the exec takes. Each fstat, getcwd, chdir,
// openat2 and fchdir it makes is tried again while it fails with EINTR, as a signal to the shim, the Go runtime's
// preemption among them, makes one on a slow FUSE mount fail; interrupted fails the
// first try of each so.
func shim(report string, at, argv []string, interrupted bool) {
	runtime.LockOSThread()
	// A start that gives the shim no CAP_SYS_RESOURCE ambient, as a setuid or file-capability exec
	// does, which clears the ambient set, is not the run's: it touches no descriptor, which may be one
	// an earlier init of the binary opened, and writes nothing.
	if set, err := unix.PrctlRetInt(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_IS_SET, unix.CAP_SYS_RESOURCE, 0, 0); err != nil || set != 1 {
		os.Exit(127) //nolint:forbidigo,revive // the shim is its own main
	}
	fd, err := strconv.Atoi(report)
	if err == nil {
		syscall.CloseOnExec(fd)
		step, err := shimSteps(fd, at, argv, shimTry(interrupted))
		var errno syscall.Errno
		errors.As(err, &errno)
		_, _ = unix.Write(fd, []byte{step, byte(errno)}) //nolint:errcheck // a report not written is EOF, and the shim still exits before the program
	}
	os.Exit(127) //nolint:forbidigo,revive // the shim is its own main, and a step it could not take ends it before the program
}

// shimSteps takes the shim's steps and execs its second stage, and gives the step that failed and
// why. report is the descriptor of its report: every one below it is the program's. try makes a call
// that EINTR may fail.
func shimSteps(report int, at, argv []string, try func(func() error) error) (byte, error) {
	f, err := os.OpenFile(at[0], os.O_WRONLY, 0) //nolint:gosec // G304: the kernel's file, or a test seam's
	if err == nil {
		_, err = f.WriteString("0")
		err = errors.Join(err, f.Close())
	}
	if err != nil {
		return shimLimit, err
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return shimCgroup, err
	}
	if err := unix.Mount("proc", at[1], "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		return shimProc, err
	}
	if err := coverCgroups(at[2]); err != nil {
		return shimCgroup, err
	}
	// The shim's folder, the caller's own, is held by reference, so one in a cgroup mount stays in
	// the host's under the covers, as does a Dir through a proc link: the shim enters the program's,
	// Cmd.Dir or that one, then takes it again by its path, through them, by openat2 and fchdir. A Dir
	// the caller cannot enter fails here, after the start, never in it.
	if err := shimEnter(at[4], func(path string) error {
		return try(func() error { return unix.Chdir(path) })
	}, func() (wd string, err error) {
		err = try(func() (failed error) { wd, failed = unix.Getwd(); return failed })
		return wd, err
	}, func(path string) (fd int, err error) {
		err = try(func() (failed error) { fd, failed = openFolder(path); return failed })
		return fd, err
	}, func(fd int) error { return shimFchdir(fd, try) }); err != nil {
		return shimFolder, err
	}
	// A descriptor of the caller's that is not close-on-exec, one it was started with or opened
	// without O_CLOEXEC, Go's start leaves open above those the run names: it closes at the exec, so
	// the program gets only Stdin, Stdout, Stderr, ExtraFiles and File's. Linux 5.11, below
	// cgroup.kill's 5.14.
	if err := unix.CloseRange(uint(report), ^uint(0), unix.CLOSE_RANGE_CLOEXEC); err != nil {
		return shimPrivs, err
	}
	// A directory the run hands the tree stays on the caller's mounts, where ".." walks past the
	// tree's root to the caller's cgroups. Each below the report's is open, as Run refuses a closed
	// one of ExtraFiles, so one whose fstat fails, as on a FUSE mount whose server died or under a
	// seccomp filter, is refused: it may be a directory.
	for fd := range report {
		var st unix.Stat_t
		if err := try(func() error { return unix.Fstat(fd, &st) }); err != nil {
			return shimUnchecked, err
		}
		if st.Mode&unix.S_IFMT == unix.S_IFDIR {
			return shimHanded, unix.EISDIR
		}
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return shimPrivs, err
	}
	if err := unix.Capset(&unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}, &make([]unix.CapUserData, 2)[0]); err != nil {
		return shimPrivs, err
	}
	// The second stage starts, and its Go runtime sets up its memory, with transparent huge pages
	// off, which an exec keeps: the other threads of a Go image end by the exec but the kernel frees
	// their mm after it, and the huge pages a race-built runtime had made, 40 MiB of them, were still
	// charged to the run's cgroup when its program started and killed healthy runs at TreeMemory. The stage turns it on again just before it execs the program. A kernel that
	// refuses it only loses this.
	_ = unix.Prctl(unix.PR_SET_THP_DISABLE, 1, 0, 0, 0) //nolint:errcheck // best effort, as above
	// The report, the program's environment and the go-ahead go to the second stage: their
	// descriptors stay open across this exec, and close at the program's.
	for fd := report; fd < report+shimFiles; fd++ {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, 0); err != nil {
			return shimExec, err
		}
	}
	return shimExec, syscall.Exec("/proc/self/exe", slices.Concat([]string{shimRun, strconv.Itoa(report), at[3]}, argv), shimRunEnv) //nolint:forbidigo,gosec // G204: the shim's own exec, of itself, behind what was just set
}

// shimRunEnv is the whole environment of the shim's second stage: since Go 1.25 the Go runtime of a
// process in a cgroup namespace opens the cpu.max it finds under the cgroup mount, the shim's cover, and
// keeps that descriptor, which the caller's check of the run's threads reads as a descriptor on a cgroup
// file system, and every healthy run on a host that delegates cpu is refused as a forgery and killed.
// containermaxprocs=0 has the runtime close it before the
// stage's init runs. The program's environment is read from the descriptor after the report's, so this
// reaches it in no way.
var shimRunEnv = []string{"GODEBUG=containermaxprocs=0"} //nolint:gochecknoglobals // a constant slice the exec above reads

// shimFiles is how many descriptors the run hands its shim after the program's own: the report, the
// program's environment and the go-ahead, in that order.
const shimFiles = 3

// shimExecProgram is the shim's second stage. It is a process anyone can start, by shimRun as an
// argv[0], in any binary that holds this package, a setuid one or one with file capabilities among
// them: so it first asks that no_new_privs is set and that its thread holds no capability, as the
// first stage's exec leaves every thread of it, and one that does not hold exits 127 before it
// touches a descriptor, having written nothing and run nothing. Then it
// takes its three descriptors close-on-exec again, reads the program's environment, writes shimReady
// to the report, waits for shimGo on the go-ahead, and execs the program at path with argv. The
// caller reads the run's threads between the two, so it reads this stage, which holds what the shim
// left, and no step of the program's. A go-ahead that ends with no shimGo is the
// caller's refusal: the stage exits and runs nothing. A step it cannot take is reported as shimExec
// and its errno, after shimReady where that was written, and it exits.
func shimExecProgram(report, path string, argv []string) {
	fd, err := strconv.Atoi(report)
	if err == nil && shimHoldsNothing() {
		for i := range shimFiles {
			syscall.CloseOnExec(fd + i)
		}
		if errno, ok := errors.AsType[syscall.Errno](shimStart(fd, path, argv)); ok {
			_, _ = unix.Write(fd, []byte{shimExec, byte(errno)}) //nolint:errcheck,gosec // a report not written is EOF, and the shim still exits before the program; G115: the report holds an errno's low byte, as the first stage's does
		}
	}
	os.Exit(127) //nolint:forbidigo,revive // the shim is its own main, and a step it could not take ends it before the program
}

// shimHoldsNothing is whether this thread has no_new_privs set and no capability in its effective,
// permitted and inheritable sets, the ambient set lying within the last two. A call that fails is a
// no.
func shimHoldsNothing() bool {
	var held [2]unix.CapUserData
	if set, err := unix.PrctlRetInt(unix.PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0); err != nil || set != 1 {
		return false
	}
	if err := unix.Capget(&unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}, &held[0]); err != nil {
		return false
	}
	return held == [2]unix.CapUserData{}
}

// shimStart is the second stage's steps on the report fd and the two descriptors after it. It returns
// an errno for a step it could not take, and nil where the caller gave no go-ahead.
func shimStart(fd int, path string, argv []string) error {
	b, err := io.ReadAll(os.NewFile(uintptr(fd+1), "go-proc-env"))
	if err == nil {
		_, err = unix.Write(fd, []byte{shimReady})
	}
	if err != nil {
		return err
	}
	// os.File reads again past an EINTR, which the Go runtime's own signals give a read that waits.
	var word [1]byte
	if n, err := os.NewFile(uintptr(fd+2), "go-proc-go").Read(word[:]); n != 1 || err != nil || word[0] != shimGo {
		return nil
	}
	env := strings.Split(string(b), "\x00")
	// The program gets the huge pages of its own mm as any program does (shimSteps).
	if err := unix.Prctl(unix.PR_SET_THP_DISABLE, 0, 0, 0, 0); err != nil {
		return err
	}
	return syscall.Exec(path, argv, env[:len(env)-1]) //nolint:forbidigo,gosec // G204: the shim's own exec, of the program Run was given, behind what was set
}

// shimEnter enters the program's folder: dir, Cmd.Dir, by chdir, from the folder the shim started
// in where it is relative, or that folder where dir is empty; then asks the folder's path by getwd,
// opens it by open and enters it by fchdir. A chdir through a proc link, /proc/self/cwd or
// /proc/self/fd/N, goes to the folder the link holds, not by its path, so crosses no cover: the second
// step enters a folder under a cover through the cover, and getwd gives no path, ENOENT, for one not
// on the tree's mounts. It holds the folder open gave, not the path again, so a link
// swapped in at the path after getwd is refused, and an open that is refused ends the run, with no
// chdir by the path in its place.
func shimEnter(dir string, chdir func(string) error, getwd func() (string, error), open func(string) (int, error), fchdir func(int) error) error {
	if dir != "" {
		if err := chdir(dir); err != nil {
			return err
		}
	}
	wd, err := getwd()
	if err != nil {
		return err
	}
	fd, err := open(wd)
	if err != nil {
		return err
	}
	return fchdir(fd)
}

// shimFchdir enters the folder fd holds by fchdir, tried again by try, and closes fd.
func shimFchdir(fd int, try func(func() error) error) error {
	return errors.Join(try(func() error { return unix.Fchdir(fd) }), unix.Close(fd))
}

// openFolder opens path for fchdir by openat2 with RESOLVE_NO_SYMLINKS: a link is ELOOP, a refused openat2 ENOSYS or EPERM.
func openFolder(path string) (int, error) {
	return unix.Openat2(unix.AT_FDCWD, path, &unix.OpenHow{Flags: unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: unix.RESOLVE_NO_SYMLINKS})
}

// coverCgroups mounts a cgroup v2 over each mount point of a cgroup v2 or v1 mount that mounts, a
// mountinfo file, lists, the deepest first, so a shallower one covers nothing left open beneath
// it. The shim is in the tree's cgroup namespace, so each is rooted at the run's cgroup: the
// caller's cgroup, the parent's cgroup.subtree_control and every sibling's files are named by no
// path of the tree's, which the kernel's nsdelegate alone does not refuse a write of. With
// CAP_SYS_ADMIN gone, the tree unmounts none, and a mount namespace it makes in a user namespace of
// its own locks them over what they cover. Each goes on an empty read-only tmpfs mounted there
// first: the kernel refuses, EBUSY, a mount over the root of a mount of the same superblock, and
// the tree's cgroup v2 shares the host's. A line that is no mountinfo line is EINVAL.
func coverCgroups(mounts string) error {
	b, err := os.ReadFile(mounts) //nolint:gosec // G304: the tree's own proc's file, or a test seam's
	if err != nil {
		return err
	}
	points, err := cgroupMounts(string(b))
	if err != nil {
		return err
	}
	slices.SortFunc(points, func(a, b string) int { return cmp.Or(cmp.Compare(len(b), len(a)), cmp.Compare(a, b)) })
	const flags = unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC
	for _, p := range slices.Compact(points) {
		if err := unix.Mount("tmpfs", p, "tmpfs", flags|unix.MS_RDONLY, "mode=0555"); err != nil {
			return err
		}
		if err := unix.Mount("cgroup2", p, "cgroup2", flags, ""); err != nil {
			return err
		}
	}
	return nil
}

// shimTry makes a call of the shim's again while it fails with EINTR: a
// refusal for a signal is no refusal for what the call checks. interrupted fails its first try so.
func shimTry(interrupted bool) func(func() error) error {
	return func(call func() error) error {
		for once := interrupted; ; once = false {
			var err error = unix.EINTR
			if !once {
				err = call()
			}
			if !errors.Is(err, unix.EINTR) {
				return err
			}
		}
	}
}

// The steps a shim reports it could not take, and shimReady, which it writes once every step before
// its exec is done.
const (
	shimLimit     byte = 'l'
	shimProc      byte = 'm'
	shimCgroup    byte = 'g'
	shimFolder    byte = 'w'
	shimHanded    byte = 'd'
	shimUnchecked byte = 'u'
	shimPrivs     byte = 'p'
	shimExec      byte = 'x'
	shimReady     byte = 'r'
)
