package proc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix" //nolint:depguard // arch.ports.23: a helper raises CAP_SYS_ADMIN by its number, the enomem helper charges its cgroup by a memfd, and the pidfd-getfd helper calls pidfd_getfd, none of which syscall gives
)

// nsModes runs the helper modes of a pid namespace of its own, each run as pid 1 of new user, pid
// and mount namespaces, which first mounts a proc of its own pid namespace over /proc:
//
//   - ns-place: writes "place " and place's answer there, then lives until its stdin ends.
//   - ns-stop <place>: starts a tree there, then stops an id with its leader's pid and start time
//     and the other place given, as an id handed over from a sibling pid namespace whose own tree
//     had the same pid at the same tick would be; writes "here " and its own place, "stale " and
//     whether that was ErrStale, "signals " and how many it sent, and "alive " and whether its own
//     leader still runs, then stops its own tree and exits 0.
//   - ns-planted: starts a tree there and reaps its leader, then mounts a tmpfs on /proc holding the
//     files the guard reads, made to pass every check but the mount's and to name the reaped leader
//     (plantProc), and stops and interrupts the tree's id through the mount check; writes "planted "
//     and whether the files pass those checks, "refused " and whether the stop and the interrupt
//     were each errProcMount, and "signals " and how many the two sent, and "left " and whether
//     groupLeft, asked of the leader's group
//     but for the leader, answers that one is left, then exits 0.
//   - ns-bound <how>: starts a tree there and reads the proc mount by openat2 where how is
//     "openat2", by openat where how is "openat" (noOpenat2), and where how is "enosys" or "eperm"
//     with openat2 answered so by a seccomp filter, as a kernel before 5.6 or a container's filter
//     answers it. Where how is "openat2" it binds a plain file naming the live leader as born a tick
//     later over the leader's /proc/<pid>/stat, and stops the id that file names; it writes "bound "
//     and true or the bind's error, "shows " and whether a read of the path gives the file, "refused
//     " and whether the stop was EXDEV, "signals " and how many it sent, and "alive " and whether
//     the leader runs on, and otherwise "bound " and how with the rest false and 0. Then it takes
//     the bind off, opens and checks the mount, mounts a tmpfs on /proc naming the leader so
//     (plantProc), and stops the id through the mount it opened before; writes "held " and whether
//     that was ErrStale, "heldsignals " and how many it sent, and "flat " and whether the mount it
//     opened reads by openat, then exits 0.
//   - ns-sys <how>: reads the proc mount by openat2 where how is "openat2", and where how is
//     "enosys" with openat2 answered so by a seccomp filter. It binds /proc/sys over itself
//     read-only, as a container runtime does, and asks place; binds a file holding another boot's
//     id over /proc/sys/kernel/random/boot_id, as LXC and systemd-nspawn do, and asks again; then
//     takes it off, mounts a tmpfs on /proc/sys holding another boot_id, and asks again. It writes
//     "own " and the first's error, "bootbound " and whether the second was errNoPlace wrapping
//     EXDEV, and "other " and whether the third was errNoPlace, then exits 0.
//
// and one of Linux's own, run as it is started:
//
//   - hog-hold <mib>: as hog, then holds.
//   - capeff: writes "capeff " and its CapEff and "nnp " and its NoNewPrivs, and exits 0.
//   - fchdir-folder <dir>: opens dir by openFolder, gives it to shimFchdir, and writes "wd " and
//     its own getcwd and "closed " and whether the descriptor is closed, then exits 0.
//
// Any other mode is cgroupModes's.
func nsModes(mode string, args []string) (int, bool) {
	switch {
	case mode == "capeff" && len(args) == 0:
		return capEff(""), true
	case mode == "fchdir-folder" && len(args) == 1:
		return fchdirFolder(args[0]), true
	case mode == "hog-hold" && len(args) == 1:
		return hogHold(args[0]), true
	case mode == "ns-place" && len(args) == 0:
		return nsPlace(), true
	case mode == "ns-stop" && len(args) == 1:
		return nsStop(args[0]), true
	case mode == "ns-planted" && len(args) == 0:
		return nsPlanted(), true
	case mode == "ns-bound" && len(args) == 1:
		return nsBound(args[0]), true
	case mode == "ns-sys" && len(args) == 1:
		return nsSys(args[0]), true
	}
	return cgroupModes(mode, args)
}

// hogHold is the hog-hold helper.
func hogHold(arg string) int {
	mib, err := strconv.Atoi(arg)
	if err != nil {
		return 2
	}
	held := hog(mib)
	code := wrote(os.Stdout.WriteString("done\n"))
	if code == 0 {
		code = then("hold")
	}
	runtime.KeepAlive(held)
	return code
}

// ownProc makes this process's mounts its own and mounts a proc of its pid namespace over /proc.
func ownProc() error {
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("making the mounts this namespace's own: %w", err)
	}
	if err := syscall.Mount("proc", "/proc", "proc", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, ""); err != nil {
		return fmt.Errorf("mounting this pid namespace's proc: %w", err)
	}
	return nil
}

// nsPlace is the ns-place helper.
func nsPlace() int {
	if err := ownProc(); err != nil {
		return wrote(0, err)
	}
	here, err := placeHere()
	if err != nil {
		return wrote(0, err)
	}
	if code := wrote(fmt.Fprintf(os.Stdout, "place %s\n", here)); code != 0 {
		return code
	}
	_, err = io.Copy(io.Discard, os.Stdin)
	return wrote(0, err)
}

// nsStop is the ns-stop helper.
func nsStop(other string) int {
	if err := ownProc(); err != nil {
		return wrote(0, err)
	}
	here, err := placeHere()
	if err != nil {
		return wrote(0, err)
	}
	exe, err := os.Executable()
	if err != nil {
		return wrote(0, err)
	}
	tree, err := Start(Detached{Path: exe, Args: []string{helperMode + "tree", "hold", strconv.Itoa(os.Getpid())}})
	if err != nil {
		return wrote(0, err)
	}
	pid, born, _, ok := parseTreeID(tree.ID)
	if !ok {
		return wrote(0, fmt.Errorf("Start gave %q, which is not a tree id", tree.ID))
	}
	handed := TreeID(strconv.Itoa(pid) + "." + strconv.FormatUint(born, 10) + "." + other)
	end, err := processEnd(pid) // opened now, while the leader is unreaped, so it names the leader
	if err != nil {
		return wrote(0, err)
	}
	var calls atomic.Int32
	stale := errors.Is(stopTree(handed, counted(&calls)), ErrStale)
	alive := runsOn(end)
	code := wrote(fmt.Fprintf(os.Stdout, "here %s\nstale %t\nsignals %d\nalive %t\n", here, stale, calls.Load(), alive))
	err = Stop(tree.ID)
	_, _ = tree.Leader.Wait()
	if code != 0 {
		return code
	}
	return wrote(0, err)
}

// nsPlanted is the ns-planted helper.
func nsPlanted() int {
	if err := ownProc(); err != nil {
		return wrote(0, err)
	}
	here, err := placeHere()
	if err != nil {
		return wrote(0, err)
	}
	exe, err := os.Executable()
	if err != nil {
		return wrote(0, err)
	}
	tree, err := Start(Detached{Path: exe, Args: []string{helperMode + "argv"}})
	if err != nil {
		return wrote(0, err)
	}
	if _, err := tree.Leader.Wait(); err != nil {
		return wrote(0, err)
	}
	pid, born, _, ok := parseTreeID(tree.ID)
	if !ok {
		return wrote(0, fmt.Errorf("Start gave %q, which is not a tree id", tree.ID))
	}
	if err := plantProc(pid, born); err != nil {
		return wrote(0, err)
	}
	// The plant passes every check but the mount's: it is this process's, gives this place, and
	// names the reaped leader as alive with its group and start time.
	planted := false
	if r, err := procOwn(procFS{dir: procMount, standIn: true}); err == nil {
		at, errP := place(r)
		g, b, errL := leader(r, pid)
		planted = errP == nil && at == here && errL == nil && g == pid && b == born
		r.close()
	}
	var calls atomic.Int32
	refused := true
	for _, act := range []func(TreeID, procFS, killer) error{stop, interrupt} {
		err := act(tree.ID, procFS{dir: procMount}, counted(&calls))
		refused = refused && errors.Is(err, errProcMount) && errors.Is(err, errors.ErrUnsupported)
	}
	// The scan for a group left behind takes the plant for no proc too, and answers that one is left,
	// which claims no group empty that may not be: a scan of the plant, whose one stat is the leader's,
	// would answer none.
	left := groupLeft(pid, pid, nil)
	return wrote(fmt.Fprintf(os.Stdout, "planted %t\nrefused %t\nsignals %d\nleft %t\n", planted, refused, calls.Load(), left))
}

// plantProc mounts a tmpfs on /proc and writes there the files the guard reads, as a root whose
// /proc is a folder may hold them: self/stat and self/status naming this process, the boot_id and
// self/ns links the proc mount below gives, and the stat of pid, a leader of its own group born at
// born (statLine).
func plantProc(pid int, born uint64) error {
	boot, err := os.ReadFile(procMount + "/sys/kernel/random/boot_id")
	if err != nil {
		return err
	}
	ns := map[string]string{}
	for _, name := range []string{"pid", "time"} {
		if ns[name], err = os.Readlink(procMount + "/self/ns/" + name); err != nil {
			return err
		}
	}
	if err := syscall.Mount("tmpfs", procMount, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, ""); err != nil {
		return fmt.Errorf("mounting a tmpfs on /proc: %w", err)
	}
	self, p := strconv.Itoa(os.Getpid()), strconv.Itoa(pid)
	for _, dir := range []string{"self/ns", "sys/kernel/random", p} {
		if err := os.MkdirAll(filepath.Join(procMount, dir), 0o700); err != nil {
			return err
		}
	}
	files := map[string]string{
		"self/stat":                 self + " (proc.test) S 0 " + self + " " + self + " 0 -1\n",
		"self/status":               "Name:\tproc.test\nNSpid:\t" + self + "\n",
		"sys/kernel/random/boot_id": string(boot),
		p + "/stat":                 statLine(pid, born),
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(procMount, name), []byte(body), 0o600); err != nil {
			return err
		}
	}
	for name, link := range ns {
		if err := os.Symlink(link, filepath.Join(procMount, "self/ns", name)); err != nil {
			return err
		}
	}
	return nil
}

// statLine is a stat as /proc/<pid>/stat gives one, of pid leading its own group, born at born.
func statLine(pid int, born uint64) string {
	p := strconv.Itoa(pid)
	return p + " (proc.test) S 1 " + p + " " + p + " 0 -1 4194304 0 0 0 0 0 0 0 0 20 0 1 0 " + strconv.FormatUint(born, 10) + " 0 0\n"
}

// nsBound is the ns-bound helper.
func nsBound(how string) int {
	if err := ownProc(); err != nil {
		return wrote(0, err)
	}
	proc := procFS{dir: procMount, noOpenat2: how == "openat"}
	if errno, ok := map[string]syscall.Errno{"enosys": syscall.ENOSYS, "eperm": syscall.EPERM}[how]; ok {
		if err := refuseOpenat2(errno); err != nil {
			return wrote(0, err)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return wrote(0, err)
	}
	tree, err := Start(Detached{Path: exe, Args: []string{helperMode + "hold"}})
	if err != nil {
		return wrote(0, err)
	}
	defer func() {
		_ = signalPid(syscall.Kill, tree.Leader.Pid, syscall.SIGKILL)
		_, _ = tree.Leader.Wait()
	}()
	pid, born, at, ok := parseTreeID(tree.ID)
	if !ok {
		return wrote(0, fmt.Errorf("Start gave %q, which is not a tree id", tree.ID))
	}
	// The id of another process that held the leader's pid, born a tick later, and a stat that names
	// the live leader so, bound over the leader's own.
	stale := TreeID(strconv.Itoa(pid) + "." + strconv.FormatUint(born+1, 10) + "." + at)
	dir, err := os.MkdirTemp("", "bound")
	if err != nil {
		return wrote(0, err)
	}
	file, target := filepath.Join(dir, "stat"), procMount+"/"+strconv.Itoa(pid)+"/stat"
	if err := os.WriteFile(file, []byte(statLine(pid, born+1)), 0o600); err != nil {
		return wrote(0, err)
	}
	bound, shows, refused, alive := how, false, false, false
	var calls atomic.Int32
	if how == "openat2" {
		end, err := processEnd(pid)
		if err != nil {
			return wrote(0, err)
		}
		if err := syscall.Mount(file, target, "", syscall.MS_BIND, ""); err != nil {
			return wrote(fmt.Fprintf(os.Stdout, "bound %v\nshows -\nrefused -\nsignals -\nalive -\nheld -\nheldsignals -\nflat -\n", err))
		}
		b, err := os.ReadFile(target)
		bound, shows = "true", err == nil && string(b) == statLine(pid, born+1)
		refused = errors.Is(stop(stale, proc, counted(&calls)), syscall.EXDEV)
		alive = runsOn(end)
		if err := syscall.Unmount(target, 0); err != nil {
			return wrote(0, err)
		}
	}
	// The held descriptor: the mount opened and checked before a tmpfs on /proc names the leader as
	// the stale id's is read through after, not the tmpfs.
	r, err := procOwn(proc)
	if err != nil {
		return wrote(0, err)
	}
	defer r.close()
	here, err := place(r)
	if err != nil {
		return wrote(0, err)
	}
	if err := plantProc(pid, born+1); err != nil {
		return wrote(0, err)
	}
	var heldCalls atomic.Int32
	held := errors.Is(stopAt(stale, r, here, counted(&heldCalls)), ErrStale)
	return wrote(fmt.Fprintf(os.Stdout, "bound %s\nshows %t\nrefused %t\nsignals %d\nalive %t\nheld %t\nheldsignals %d\nflat %t\n", bound, shows, refused, calls.Load(), alive, held, heldCalls.Load(), r.flat))
}

// nsSys is the ns-sys helper.
func nsSys(how string) int {
	if err := ownProc(); err != nil {
		return wrote(0, err)
	}
	if how == "enosys" {
		if err := refuseOpenat2(syscall.ENOSYS); err != nil {
			return wrote(0, err)
		}
	}
	sys := procMount + "/sys"
	if err := syscall.Mount(sys, sys, "", syscall.MS_BIND, ""); err != nil {
		return wrote(0, fmt.Errorf("binding /proc/sys over itself: %w", err))
	}
	if err := syscall.Mount("", sys, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY|syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, ""); err != nil {
		return wrote(0, fmt.Errorf("making the bound /proc/sys read-only: %w", err))
	}
	_, errOwn := placeHere()
	const another = "01234567-89ab-4def-8123-456789abcdef\n"
	dir, err := os.MkdirTemp("", "boot")
	if err != nil {
		return wrote(0, err)
	}
	file, bootID := filepath.Join(dir, "boot_id"), sys+"/kernel/random/boot_id"
	if err := os.WriteFile(file, []byte(another), 0o600); err != nil {
		return wrote(0, err)
	}
	if err := syscall.Mount(file, bootID, "", syscall.MS_BIND, ""); err != nil {
		return wrote(0, fmt.Errorf("binding a file over boot_id: %w", err))
	}
	_, errBound := placeHere()
	if err := syscall.Unmount(bootID, 0); err != nil {
		return wrote(0, err)
	}
	if err := syscall.Mount("tmpfs", sys, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, ""); err != nil {
		return wrote(0, fmt.Errorf("mounting a tmpfs on /proc/sys: %w", err))
	}
	if err := os.MkdirAll(sys+"/kernel/random", 0o700); err != nil {
		return wrote(0, err)
	}
	if err := os.WriteFile(sys+"/kernel/random/boot_id", []byte(another), 0o600); err != nil {
		return wrote(0, err)
	}
	_, errOther := placeHere()
	return wrote(fmt.Fprintf(os.Stdout, "own %v\nbootbound %t\nother %t\n", errOwn,
		errors.Is(errBound, errNoPlace) && errors.Is(errBound, syscall.EXDEV), errors.Is(errOther, errNoPlace)))
}

// leftAlone is how long a process a kill did not reach is watched before it is taken to run on: a
// SIGKILL sent ends it well within it.
const leftAlone = time.Second

// runsOn is whether the process end watches still runs leftAlone after the stop: a kill that reached
// it ends it before then.
func runsOn(end <-chan error) bool {
	wait := time.NewTimer(leftAlone)
	defer wait.Stop()
	select {
	case <-end:
		return false
	case <-wait.C:
		return true
	}
}

// cgroupModes runs the helper modes of a run's cgroup on Linux:
//
//   - cgroup-self <then>: writes "cgroup " and its own cgroup v2 path, from /proc/self/cgroup, and
//     "max ", "swap " and "group " and that cgroup's memory.max, memory.swap.max and
//     memory.oom.group, each line ended by a newline; then exits 0, or holds.
//   - spawn-hog <mib>: writes "cgroup " and its own cgroup v2 path, starts hog <mib> with this
//     helper's stdout and stderr, then writes "spawned", each line ended by a newline, and waits for
//     the hog, exiting as it did, holding little of its own.
//   - deep-hog <mib>: starts hog-child <mib>, whose child is the hog, and waits for it.
//   - cgroup-move <to> <mib>: writes its pid to the cgroup.procs of cgroup v2 path <to>, writes
//     "cgroup " and its own cgroup v2 path, then holds <mib> MiB, each page written, writes "done",
//     each line ended by a newline, and exits 0.
//   - cgroup-unlimit <mib>: writes "cgroup " and its own cgroup v2 path, writes max to that cgroup's
//     memory.max, then holds <mib> MiB, each page written, writes "done", each line ended by a
//     newline, and exits 0.
//   - cgroup-child: writes "cgroup " and its own cgroup v2 path, makes a cgroup x in that cgroup,
//     writes "made x", each line ended by a newline, and exits 0.
//   - nested-peak: writes "cgroup " and its own cgroup v2 path, runs cgroup-self exit with ReadPeak,
//     then writes "refused " and whether that run was refused with ErrRefused and
//     errors.ErrUnsupported, "inrun " and whether with errInRun, "code " and its Code, and "inner "
//     and the cgroup its program named, or none, each line ended by a newline, and exits 0.
//   - peak-run <mib>: writes "cgroup " and its own cgroup v2 path; writes -1000 to its oom_score_adj,
//     so its cgroup's oom never takes it, or writes "unkillable " and why it could not and exits 0;
//     runs hog-first <mib> with ReadPeak and no Limits, then writes "limit " and whether that run
//     ended limit, "code " and its Code, "peakread " and its PeakRead, and "err " and its error,
//     each line ended by a newline, and exits 0.
//   - hog-first <mib>: writes 1000 to its own oom_score_adj, the most, which any process may raise
//     its own to, so an OOM kill of a cgroup above it takes it before any process of a lower score,
//     however much more that one holds; then is hog <mib>, or exits 1 where the write is refused.
//   - peak-then-fall <mib>: maps <mib> MiB, writes each page of it, and unmaps it, then writes
//     "current " and its own cgroup's memory.current, read once the map is gone, or "unread" where
//     there is none, and a newline, and exits 0.
//   - cgroup-escape: writes "cgroup " and its own cgroup v2 path, starts hold in a session of its own,
//     with none of this helper's stdin, stdout or stderr, as a process that left the run's tree
//     would, writes "escaped " and its pid, each line ended by a newline, and exits 0.
//   - contained <parent> <to> <try> <mib> <then>: writes "cgroup " and its own cgroup v2 path as it
//     sees it, "uid " and its uid, "gid " and its gid, and "run " and the inode of its cgroup v2
//     mount's root, which a Contained run's shim roots at the run's cgroup, the cgroup it names as
//     its run's; then tries <try>, each path <parent> and <to> a cgroup v2 path as the caller sees
//     it: move writes its pid to the cgroup.procs of <to>, other starts hold and writes its pid
//     there, max and swap write max to its run's memory.max or memory.swap.max, group writes 0 to its
//     run's memory.oom.group, kill and freeze write 1 to its run's cgroup.kill or cgroup.freeze,
//     sibling writes max to the memory.max of <to>, freeze-caller and kill-caller write 1 to the
//     cgroup.freeze or cgroup.kill of <to>, then -memory to <parent>'s cgroup.subtree_control,
//     writing "lifted " and that write's error, or nil, and "lifterrno " and its errno, or 0,
//     ready writes "ready" and a newline to the file at path <to>, as a FIFO the test reads, nest
//     moves into a child x of its run's cgroup and has
//     lift run there in a user and a cgroup namespace of its own, nestroot does the same with lift
//     making the cgroup namespace itself in a user namespace where it holds CAP_SYS_ADMIN (nest),
//     caps writes its own CapEff and NoNewPrivs as capeff does, each key led by own, then runs
//     capeff from the file at path <to>, signal writes "ppid " and its parent's pid and
//     sends signal 0 to the pid <to>, fd fstats the descriptor numbered <to>, parent stats <parent>
//     as a path from its own folder, without its leading slash, which a folder of the host's cgroup
//     mount holds and the run's root does not, and none writes nothing; writes
//     "tried " and the try's error, or nil, and "errno " and its errno, or 0; then starts hog <mib>
//     with this helper's stdout and stderr, waits for it, writes "survived", each line ended by a
//     newline, and exits 0, or holds.
//   - contained-unchecked: installs a seccomp filter, under no_new_privs, on every thread, that fails
//     an fstat of descriptor 0 with EIO, then runs contained / / none 16 exit, Contained, held to
//     64 MiB; writes the run's stdout's length as "out ", "unchecked ", "refused ", "unsupported "
//     and "eio " and whether its error wraps errContainedUnchecked, ErrRefused,
//     errors.ErrUnsupported and EIO, and "code " and its Code, each line ended by a newline, and
//     exits 0.
//   - contained-caller <parent> <caller> <try>: runs contained <parent> <caller> <try> 128 hold,
//     Contained, held to 64 MiB, with a Timeout of exitsWithin, from the cgroup the test started it
//     in, cgroup v2 path <caller>, beneath <parent>; then writes the run's stdout, "limit " and
//     whether the run ended WhyLimit, "lost " and whether it ended errLimitLost, and "code " and its
//     Code, each line ended by a newline, and exits 0.
//   - lift <run> <how>: writes max to the memory.max and the memory.swap.max of cgroup v2 path
//     <run>, where how is unshare from a cgroup namespace it first makes, exiting with the errno as
//     its code where it cannot (lift), and exits 0.
//   - nested-limit <mib>: writes "cgroup " and its own cgroup v2 path, makes cgroups a and b in it,
//     moves itself to a, gives its cgroup's children memory, holds b to 16 MiB with no swap, starts
//     hog <mib> into b with this helper's stdout and stderr, waits for it, writes "hog " and how it
//     ended, each line ended by a newline, and exits 0.
//   - enomem <mib>: writes "cgroup " and its own cgroup v2 path; writes -1000 to its oom_score_adj,
//     so its cgroup's oom finds no process to kill, or writes "unkillable " and why it could not and
//     exits 0; fallocates <mib> MiB of a memfd by one call; closes the memfd; writes "enomem " and
//     whether the fallocate failed with ENOMEM and "err " and its error, each line ended by a
//     newline, and exits 0.
//   - pidfd-getfd <fd>: as pidfdGetfd's doc says.
//   - outer-proc <pid>: as outerProc's doc says.
//   - forge-ready: as forgeReady's doc says.
//   - forge-dropped: as forgeDropped's doc says.
//   - forge-leader-exit: as forgeLeaderExit's doc says.
//   - stage <set> <argv...>: as stage's doc says.
func cgroupModes(mode string, args []string) (int, bool) {
	switch {
	case mode == "contained" && len(args) == 5:
		return contained(args[0], args[1], args[2], args[3], args[4]), true
	case mode == "contained-caller" && len(args) == 3:
		return containedCaller(args[0], args[1], args[2]), true
	case mode == "lift" && len(args) == 2:
		return lift(args[0], args[1]), true
	case mode == "cgroup-self" && len(args) == 1:
		return cgroupSelf(args[0]), true
	case mode == "spawn-hog" && len(args) == 1:
		return spawnHog(args[0]), true
	case mode == "cgroup-escape" && len(args) == 0:
		return cgroupEscape(), true
	case mode == "deep-hog" && len(args) == 1:
		child, err := helping("hog-child", args[0])
		if err != nil {
			return wrote(0, err), true
		}
		return wrote(0, child.Wait()), true
	case mode == "peak-then-fall" && len(args) == 1:
		return peakThenFall(args[0]), true
	case mode == "cgroup-move" && len(args) == 2:
		return cgroupMove(args[0], args[1]), true
	case mode == "cgroup-unlimit" && len(args) == 1:
		return cgroupUnlimit(args[0]), true
	case mode == "cgroup-child" && len(args) == 0:
		return cgroupChild(), true
	case mode == "nested-peak" && len(args) == 0:
		return nestedPeak(), true
	case mode == "peak-run" && len(args) == 1:
		return peakRun(args[0]), true
	case mode == "hog-first" && len(args) == 1:
		return hogFirst(args[0]), true
	}
	return limitModes(mode, args)
}

// limitModes runs the helper modes of what a run whose tree met TreeMemory reports, nested-limit
// and enomem, and contained-unchecked, as cgroupModes' doc says, pidfd-getfd, as pidfdGetfd's, and
// outer-proc, as outerProc's says.
func limitModes(mode string, args []string) (int, bool) {
	switch {
	case mode == "contained-unchecked" && len(args) == 0:
		return containedUnchecked(), true
	case mode == "nested-limit" && len(args) == 1:
		return nestedLimit(args[0]), true
	case mode == "enomem" && len(args) == 1:
		return enomem(args[0]), true
	case mode == "pidfd-getfd" && len(args) == 1:
		return pidfdGetfd(args[0]), true
	case mode == "outer-proc" && len(args) == 1:
		return outerProc(args[0]), true
	case mode == "forge-ready" && len(args) == 0:
		return forgeReady(), true
	case mode == "forge-dropped" && len(args) == 0:
		return forgeDropped(), true
	case mode == "forge-leader-exit" && len(args) == 0:
		return forgeLeaderExit(), true
	case mode == "stage" && len(args) > 1:
		return stage(args[0], args[1:]), true
	}
	return 0, false
}

// stage is the stage helper: it execs this binary again with argv as its whole argv, its first word
// among it, as the shim's first stage execs its second. On the thread that execs it first clears
// each capability set where set has a "c", the inheritable one a session may hand down among them,
// sets no_new_privs where set has an "n", and turns transparent huge pages off where it has a "t", as the shim's first stage does. Its descriptors and its environment go on as they are.
func stage(set string, argv []string) int {
	runtime.LockOSThread()
	if strings.Contains(set, "c") {
		if _, err := unix.PrctlRetInt(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
			return wrote(0, err)
		}
		if err := unix.Capset(&unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}, &make([]unix.CapUserData, 2)[0]); err != nil {
			return wrote(0, err)
		}
	}
	if strings.Contains(set, "n") {
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			return wrote(0, err)
		}
	}
	if strings.Contains(set, "t") {
		if err := unix.Prctl(unix.PR_SET_THP_DISABLE, 1, 0, 0, 0); err != nil {
			return wrote(0, err)
		}
	}
	return wrote(0, syscall.Exec("/proc/self/exe", argv, os.Environ())) //nolint:forbidigo,gosec // G204: this test binary again, as the shim's second stage
}

// forgeReady is the forge-ready helper, the program of a start that skipped the shim: it writes the
// shim's ready byte to each descriptor from 3 up, the report's among them, keeps them open, and holds
// for holdFor unless it is killed, then writes "ran" and a newline to its stdout.
func forgeReady() int {
	for fd := 3; fd < 64; fd++ {
		_, _ = unix.Write(fd, []byte{shimReady}) // a descriptor that is not the report fails, and the helper goes on
	}
	if code := then("hold"); code != 0 {
		return code
	}
	return wrote(os.Stdout.WriteString("ran\n"))
}

// forgeDropped is the forge-dropped helper, the program of a start that skipped the shim and does what
// a forger that meant to pass that check would: it sets no_new_privs and clears each
// capability set itself, so it keeps the caller's cgroup mounts uncovered, then does what forgeReady does.
func forgeDropped() int {
	if code := dropHere(); code != 0 {
		return code
	}
	return forgeReady()
}

// dropHere sets no_new_privs and clears each capability set of the calling thread, and gives 0, or 1
// with the error said.
func dropHere() int {
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return wrote(0, err)
	}
	if _, err := unix.PrctlRetInt(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
		return wrote(0, err)
	}
	if err := unix.Capset(&unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}, &make([]unix.CapUserData, 2)[0]); err != nil {
		return wrote(0, err)
	}
	return 0
}

// forgeLeaderExit is the forge-leader-exit helper, the program of a start that skipped the shim and
// ends its thread-group leader (the main goroutine, which runs on it as a helper starts; the helper says so and ends with 1 where it does not): the leader sets no_new_privs and clears each of its capability sets, as
// forgeDropped does, forges the ready byte as forgeReady does, starts a goroutine that holds for
// holdFor, and ends with the thread-only exit. The leader is then a zombie whose status is a shim's
// process's and whose mountinfo is EINVAL, and the threads that run on keep the caller's cgroup mounts
// and the capabilities the start gave them (a thread's own, not the leader's to drop: the runtime's
// AllThreadsSyscall answers ENOTSUP in a binary built with cgo).
func forgeLeaderExit() int {
	if unix.Gettid() != os.Getpid() {
		return wrote(0, errors.New("the main goroutine is not on the thread-group leader"))
	}
	if code := dropHere(); code != 0 {
		return code
	}
	for fd := 3; fd < 64; fd++ {
		_, _ = unix.Write(fd, []byte{shimReady})
	}
	go func() {
		<-time.NewTimer(holdFor).C
		_, _, _ = syscall.RawSyscall(unix.SYS_EXIT_GROUP, 0, 0, 0) // the leader is gone, so no return reaches an exit
	}()
	_, _, _ = syscall.RawSyscall(unix.SYS_EXIT, 0, 0, 0)
	return 0
}

// innerCmd is a Cmd that runs this test binary as the helper of mode with args, for a helper that
// runs one of its own, bounded as helperCmd's are, with Bare's Env and GORACE.
func innerCmd(mode string, args ...string) (Cmd, error) {
	exe, err := os.Executable()
	if err != nil {
		return Cmd{}, err
	}
	return Cmd{Path: exe, Args: append([]string{helperMode + mode}, args...), Env: Bare("GORACE"), Timeout: longEnough, MaxOutput: 1 << 20, Drain: drainWait}, nil
}

// ownCgroupLine writes "cgroup " and this helper's cgroup v2 path and a newline, and gives the path.
func ownCgroupLine() (string, int) {
	own, err := selfPath()
	if err != nil {
		return "", wrote(0, err)
	}
	return own, wrote(fmt.Fprintf(os.Stdout, "cgroup %s\n", own))
}

// cgroupUnlimit is the cgroup-unlimit helper.
func cgroupUnlimit(mib string) int {
	n, err := strconv.Atoi(mib)
	if err != nil {
		return 2
	}
	own, code := ownCgroupLine()
	if code != 0 {
		return code
	}
	if err := os.WriteFile(filepath.Join(cgroupMount, own, "memory.max"), []byte("max"), 0); err != nil {
		return wrote(0, err)
	}
	held := hog(n)
	code = wrote(os.Stdout.WriteString("done\n"))
	runtime.KeepAlive(held)
	return code
}

// nestedLimit is the nested-limit helper.
func nestedLimit(mib string) int {
	own, code := ownCgroupLine()
	if code != 0 {
		return code
	}
	base := filepath.Join(cgroupMount, own)
	for _, d := range []string{"a", "b"} {
		if err := os.Mkdir(filepath.Join(base, d), 0o700); err != nil {
			return wrote(0, err)
		}
	}
	// The helper leaves its cgroup first: the kernel gives the children of a cgroup that holds a
	// process no controller.
	for _, w := range [][2]string{
		{"a/cgroup.procs", strconv.Itoa(os.Getpid())}, {"cgroup.subtree_control", "+memory"},
		{"b/memory.max", strconv.Itoa(16 << 20)}, {"b/memory.swap.max", "0"},
	} {
		if err := os.WriteFile(filepath.Join(base, w[0]), []byte(w[1]), 0); err != nil {
			return wrote(0, err)
		}
	}
	b, err := os.OpenFile(filepath.Join(base, "b"), unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return wrote(0, err)
	}
	defer func() { _ = b.Close() }()
	exe, err := os.Executable()
	if err != nil {
		return wrote(0, err)
	}
	child := &process{Path: exe, Args: []string{exe, helperMode + "hog", mib}, Stdout: os.Stdout, Stderr: os.Stderr,
		SysProcAttr: &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(b.Fd())}}
	if err := startTestProcess(child); err != nil {
		return wrote(0, err)
	}
	return wrote(fmt.Fprintf(os.Stdout, "hog %v\n", child.Wait()))
}

// enomem is the enomem helper. It fills its cgroup by one fallocate, which on ENOMEM frees every
// page it took before it returns. Its cgroup is full only within that call: with no process its oom
// may kill, a page any thread of the helper touches then faults until the call has freed its pages,
// which the kernel does without the helper. Writes to the memfd, a MiB at a time, left the cgroup
// full until the helper's own close, and a runtime thread's fault before it held the helper for
// ever.
func enomem(mib string) int {
	n, err := strconv.Atoi(mib)
	if err != nil {
		return 2
	}
	if _, code := ownCgroupLine(); code != 0 {
		return code
	}
	if err := os.WriteFile("/proc/self/oom_score_adj", []byte("-1000"), 0); err != nil {
		return wrote(fmt.Fprintf(os.Stdout, "unkillable %v\n", err))
	}
	fd, err := unix.MemfdCreate("enomem", unix.MFD_CLOEXEC)
	if err != nil {
		return wrote(0, err)
	}
	werr := unix.Fallocate(fd, 0, 0, int64(n)<<20)
	cerr := unix.Close(fd)
	return wrote(fmt.Fprintf(os.Stdout, "enomem %t\nerr %v\n", errors.Is(werr, unix.ENOMEM), errors.Join(werr, cerr)))
}

// pidfdGetfd is the pidfd-getfd helper: it asks pidfd_getfd, by the pidfd it was given as its
// descriptor 3, for that process's descriptor target, closes what it got, and writes "getfd " and
// the errno, 0 for none, and a newline.
func pidfdGetfd(target string) int {
	n, err := strconv.Atoi(target)
	if err != nil {
		return 2
	}
	fd, err := unix.PidfdGetfd(3, n, 0)
	if err == nil {
		_ = unix.Close(fd)
	}
	var errno syscall.Errno
	errors.As(err, &errno)
	return wrote(fmt.Fprintf(os.Stdout, "getfd %d\n", int(errno)))
}

// outerProc is the outer-proc helper: it opens /proc/<pid>/mem read-only and reads its first byte
// at the start of the first mapping /proc/<pid>/maps lists, and opens /proc/<pid>/root/, by the
// /proc it finds, and writes "mem ", "memread " and "root " and each one's errno, 0 for none,
// each line ended by a newline. memread's is ENOENT when mem did not open.
func outerProc(pid string) int {
	errnoOf := func(err error) int {
		var errno syscall.Errno
		errors.As(err, &errno)
		return int(errno)
	}
	dir := "/proc/" + pid
	read := error(unix.ENOENT)
	mem, err := os.Open(dir + "/mem")
	if err == nil {
		read = memFirstByte(dir, mem)
		_ = mem.Close()
	}
	root, rootErr := os.Open(dir + "/root/")
	if rootErr == nil {
		_ = root.Close()
	}
	return wrote(fmt.Fprintf(os.Stdout, "mem %d\nmemread %d\nroot %d\n", errnoOf(err), errnoOf(read), errnoOf(rootErr)))
}

// memFirstByte reads one byte of mem at the start of the first mapping dir's maps lists.
func memFirstByte(dir string, mem *os.File) error {
	maps, err := os.ReadFile(dir + "/maps")
	if err != nil {
		return err
	}
	start, _, _ := strings.Cut(string(maps), "-")
	at, err := strconv.ParseInt(start, 16, 64)
	if err != nil {
		return unix.EINVAL
	}
	_, err = mem.ReadAt(make([]byte, 1), at)
	return err
}

// cgroupChild is the cgroup-child helper.
func cgroupChild() int {
	own, code := ownCgroupLine()
	if code != 0 {
		return code
	}
	if err := os.Mkdir(filepath.Join(cgroupMount, own, "x"), 0o700); err != nil {
		return wrote(0, err)
	}
	return wrote(os.Stdout.WriteString("made x\n"))
}

// nestedPeak is the nested-peak helper.
func nestedPeak() int {
	if _, code := ownCgroupLine(); code != 0 {
		return code
	}
	c, err := innerCmd("cgroup-self", "exit")
	if err != nil {
		return wrote(0, err)
	}
	c.ReadPeak = true
	res, err := Run(context.Background(), c)
	inner := cgroupLines(res.Stdout)["cgroup"]
	if inner == "" {
		inner = "none"
	}
	return wrote(fmt.Fprintf(os.Stdout, "refused %t\ninrun %t\ncode %d\ninner %s\n",
		errors.Is(err, ErrRefused) && errors.Is(err, errors.ErrUnsupported), errors.Is(err, errInRun), res.Code, inner))
}

// peakRun is the peak-run helper.
func peakRun(mib string) int {
	if _, code := ownCgroupLine(); code != 0 {
		return code
	}
	if err := os.WriteFile("/proc/self/oom_score_adj", []byte("-1000"), 0); err != nil {
		return wrote(fmt.Fprintf(os.Stdout, "unkillable %v\n", err))
	}
	c, err := innerCmd("hog-first", mib)
	if err != nil {
		return wrote(0, err)
	}
	c.ReadPeak = true
	res, err := Run(context.Background(), c)
	return wrote(fmt.Fprintf(os.Stdout, "limit %t\ncode %d\npeakread %t\nerr %v\n", endedWith(err, WhyLimit), res.Code, res.PeakRead, err))
}

// spawnHog is the spawn-hog helper.
func spawnHog(mib string) int {
	if _, code := ownCgroupLine(); code != 0 {
		return code
	}
	child, err := helping("hog", mib)
	if err != nil {
		return wrote(0, err)
	}
	if code := wrote(os.Stdout.WriteString("spawned\n")); code != 0 {
		return code
	}
	return wrote(0, child.Wait())
}

// hogFirst is the hog-first helper.
func hogFirst(mib string) int {
	if err := os.WriteFile("/proc/self/oom_score_adj", []byte("1000"), 0); err != nil {
		return wrote(0, err)
	}
	n, err := strconv.Atoi(mib)
	if err != nil {
		return 2
	}
	held := hog(n)
	code := wrote(os.Stdout.WriteString("done\n"))
	runtime.KeepAlive(held)
	return code
}

// cgroupMove is the cgroup-move helper.
func cgroupMove(to, mib string) int {
	n, err := strconv.Atoi(mib)
	if err != nil {
		return 2
	}
	procs := filepath.Join(cgroupMount, to, "cgroup.procs")
	if err := os.WriteFile(procs, []byte(strconv.Itoa(os.Getpid())), 0); err != nil {
		return wrote(0, err)
	}
	own, err := selfPath()
	if err != nil {
		return wrote(0, err)
	}
	if code := wrote(fmt.Fprintf(os.Stdout, "cgroup %s\n", own)); code != 0 {
		return code
	}
	held := hog(n)
	code := wrote(os.Stdout.WriteString("done\n"))
	runtime.KeepAlive(held)
	return code
}

// contained is the contained helper.
func contained(parent, to, try, mib, how string) int {
	own, err := selfPath()
	if err != nil {
		return wrote(0, err)
	}
	// The shim mounts the tree's cgroup v2 rooted at the run's cgroup, so the run is its root here.
	var root syscall.Stat_t
	if err := syscall.Stat(cgroupMount, &root); err != nil {
		return wrote(0, err)
	}
	const run = ""
	if code := wrote(fmt.Fprintf(os.Stdout, "cgroup %s\nuid %d\ngid %d\nrun %d\n", own, os.Getuid(), os.Getgid(), root.Ino)); code != 0 {
		return code
	}
	tries := map[string][2]string{
		"move":    {filepath.Join(to, "cgroup.procs"), strconv.Itoa(os.Getpid())},
		"max":     {filepath.Join(run, "memory.max"), "max"},
		"swap":    {filepath.Join(run, "memory.swap.max"), "max"},
		"group":   {filepath.Join(run, "memory.oom.group"), "0"},
		"sibling": {filepath.Join(to, "memory.max"), "max"},
		"kill":    {filepath.Join(run, "cgroup.kill"), "1"},
		"freeze":  {filepath.Join(run, "cgroup.freeze"), "1"},
	}
	var tried error
	switch w, ok := tries[try]; {
	case ok:
		tried = os.WriteFile(filepath.Join(cgroupMount, w[0]), []byte(w[1]), 0)
	case try == "other":
		tried = moveOther(to)
	case try == "freeze-caller" || try == "kill-caller":
		tried = os.WriteFile(filepath.Join(cgroupMount, to, "cgroup."+strings.TrimSuffix(try, "-caller")), []byte("1"), 0)
		lifted := os.WriteFile(filepath.Join(cgroupMount, parent, "cgroup.subtree_control"), []byte("-memory"), 0)
		var errno syscall.Errno
		errors.As(lifted, &errno)
		if code := wrote(fmt.Fprintf(os.Stdout, "lifted %v\nlifterrno %d\n", lifted, errno)); code != 0 {
			return code
		}
	case try == "ready":
		tried = os.WriteFile(to, []byte("ready\n"), 0)
	case try == "nest" || try == "nestroot":
		tried = nest(run, try == "nestroot")
	case try == "caps":
		tried = capsOf(to)
	case try == "signal":
		tried = signalOut(to)
	case try == "fd":
		n, err := strconv.Atoi(to)
		var st syscall.Stat_t
		if tried = err; err == nil {
			tried = syscall.Fstat(n, &st)
		}
	case try == "parent":
		var st syscall.Stat_t
		tried = syscall.Stat(strings.TrimPrefix(parent, "/"), &st)
	case try != "none":
		return 2
	}
	var errno syscall.Errno
	errors.As(tried, &errno)
	if code := wrote(fmt.Fprintf(os.Stdout, "tried %v\nerrno %d\n", tried, errno)); code != 0 {
		return code
	}
	child, err := helping("hog", mib)
	if err != nil {
		return wrote(0, err)
	}
	_ = child.Wait() // the kernel's kill of the hog alone is what a try of group plants
	if code := wrote(os.Stdout.WriteString("survived\n")); code != 0 {
		return code
	}
	return then(how)
}

// moveOther starts a helper that holds, a process of the tree that is not this one, and writes its
// pid to the cgroup.procs of cgroup v2 path to. The helper is left in the tree.
func moveOther(to string) error {
	other, err := helping("hold")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(cgroupMount, to, "cgroup.procs"), []byte(strconv.Itoa(other.Process.Pid)), 0)
}

// nest moves this process into x, a child cgroup it makes beneath its run's at cgroup v2 path run,
// then has a helper lift its run's limit from a cgroup namespace rooted at x: the kernel refuses a write of a file of the namespace's root alone, and run's is not
// that root. The helper starts in a user and a cgroup namespace of its own, the route of unshare -U
// -C; or, with root, in a user namespace of its own alone, holding CAP_SYS_ADMIN there as root would,
// and makes the cgroup namespace itself by unshare, the route of unshare -U -r then -C. It maps no
// uid: a start writes a map by /proc/<pid>, which names another process where /proc is not of the
// pid namespace the start runs in, as a Contained tree's is not; the files' owner still lets it
// write. A helper that exits with an errno as its code fails with that errno.
func nest(run string, root bool) error {
	x := filepath.Join(cgroupMount, run, "x")
	if err := os.Mkdir(x, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(x, "cgroup.procs"), []byte(strconv.Itoa(os.Getpid())), 0); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	a, how := &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWCGROUP}, "write"
	if root {
		a, how = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWUSER, AmbientCaps: []uintptr{unix.CAP_SYS_ADMIN}}, "unshare"
	}
	c := &process{Path: exe, Args: []string{exe, helperMode + "lift", run, how}, Stdout: os.Stdout, Stderr: os.Stderr, SysProcAttr: a}
	err = c.Run()
	if code := c.ProcessState.ExitCode(); code > 1 {
		return fmt.Errorf("%w: %w", err, syscall.Errno(code))
	}
	return err
}

// lift writes max to the memory.max and the memory.swap.max of the cgroup at cgroup v2 path run;
// where how is unshare, from a cgroup namespace it first makes on its thread, rooted at its own
// cgroup, exiting with the errno as its code where it cannot.
func lift(run, how string) int {
	if how == "unshare" {
		runtime.LockOSThread()
		var errno syscall.Errno
		if err := syscall.Unshare(syscall.CLONE_NEWCGROUP); errors.As(err, &errno) {
			_ = wrote(0, err)
			return int(errno)
		}
	}
	r := filepath.Join(cgroupMount, run)
	return wrote(0, errors.Join(os.WriteFile(filepath.Join(r, "memory.max"), []byte("max"), 0), os.WriteFile(filepath.Join(r, "memory.swap.max"), []byte("max"), 0)))
}

// capsOf writes this helper's own CapEff and NoNewPrivs as capEff does, each key led by own, then
// runs the helper at path, a copy of this one that may hold a file's capabilities, in mode capeff,
// its stdout this helper's.
func capsOf(path string) error {
	if capEff("own") != 0 {
		return errors.New("reading its own capabilities")
	}
	return (&process{Path: path, Args: []string{path, helperMode + "capeff"}, Stdout: os.Stdout, Stderr: os.Stderr}).Run()
}

// capEff writes pre and "capeff " and the CapEff of its own /proc/self/status, and pre and "nnp "
// and its NoNewPrivs, each line ended by a newline, and gives 0.
func capEff(pre string) int {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return wrote(0, err)
	}
	for line := range strings.Lines(string(b)) {
		k, v, _ := strings.Cut(line, ":")
		if k == "CapEff" || k == "NoNewPrivs" {
			if code := wrote(fmt.Fprintf(os.Stdout, "%s%s %s\n", pre, map[string]string{"CapEff": "capeff", "NoNewPrivs": "nnp"}[k], strings.TrimSpace(v))); code != 0 {
				return code
			}
		}
	}
	return 0
}

// signalOut writes "ppid " and its parent's pid, "pid " and its own, "self " and what /proc/self
// names, and "proc " and the errno of a stat of /proc/<pid>; then sends signal 0, which asks the
// kernel what SIGSTOP would, to the process of pid, which a Contained run names by its pid outside
// the tree's pid namespace: a SIGSTOP that landed would stop the test that started it, which would
// then hang, not fail. The error is the signal's, or nil.
func signalOut(pid string) error {
	n, err := strconv.Atoi(pid)
	if err != nil {
		return err
	}
	self, _ := os.Readlink("/proc/self") // a link not read is written as empty, which the row refuses
	var errno syscall.Errno
	_, err = os.Stat("/proc/" + pid)
	errors.As(err, &errno)
	if code := wrote(fmt.Fprintf(os.Stdout, "ppid %d\npid %d\nself %s\nproc %d\n", os.Getppid(), os.Getpid(), self, errno)); code != 0 {
		return fmt.Errorf("writing ppid: code %d", code)
	}
	return signalPid(syscall.Kill, n, 0)
}

// containedCaller is the contained-caller helper.
func containedCaller(parent, caller, try string) int {
	exe, err := os.Executable()
	if err != nil {
		return wrote(0, err)
	}
	c := Cmd{
		Path: exe, Args: []string{helperMode + "contained", parent, caller, try, "128", "hold"}, Dir: os.TempDir(), Env: Bare("GORACE"),
		Timeout: exitsWithin, MaxOutput: 1 << 20, Drain: drainWait, Limits: Limits{TreeMemory: treeLimit, Contained: true},
	}
	res, err := Run(context.Background(), c)
	if code := wrote(os.Stdout.Write(res.Stdout)); code != 0 {
		return code
	}
	return wrote(fmt.Fprintf(os.Stdout, "limit %t\nlost %t\ncode %d\n", endedWith(err, WhyLimit), errors.Is(err, errLimitLost), res.Code))
}

// containedUnchecked is the contained-unchecked helper.
func containedUnchecked() int {
	if err := failFstatOfStdin(); err != nil {
		return wrote(0, err)
	}
	exe, err := os.Executable()
	if err != nil {
		return wrote(0, err)
	}
	c := Cmd{
		Path: exe, Args: []string{helperMode + "contained", "/", "/", "none", "16", "exit"}, Dir: os.TempDir(), Env: Bare("GORACE"),
		Timeout: exitsWithin, MaxOutput: 1 << 20, Drain: drainWait, Limits: Limits{TreeMemory: treeLimit, Contained: true},
	}
	res, err := Run(context.Background(), c)
	return wrote(fmt.Fprintf(os.Stdout, "out %d\nunchecked %t\nrefused %t\nunsupported %t\neio %t\ncode %d\n", len(res.Stdout),
		errors.Is(err, errContainedUnchecked), errors.Is(err, ErrRefused), errors.Is(err, errors.ErrUnsupported), errors.Is(err, syscall.EIO), res.Code))
}

// failFstatOfStdin installs, under no_new_privs, a seccomp filter on every thread of this process
// that fails an fstat of descriptor 0 with EIO, as a filter a caller installed, or a FUSE mount
// whose server died, fails it; a process this one starts inherits it, the shim among them.
func failFstatOfStdin() error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return err
	}
	const ld, jeq, ret = unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, unix.BPF_RET | unix.BPF_K
	prog := []unix.SockFilter{
		{Code: ld, K: 0}, // seccomp_data.nr
		{Code: jeq, Jf: 3, K: unix.SYS_FSTAT},
		{Code: ld, K: 16}, // the low half of seccomp_data.args[0], on a little-endian processor
		{Code: jeq, Jf: 1, K: 0},
		{Code: ret, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EIO)},
		{Code: ret, K: unix.SECCOMP_RET_ALLOW},
	}
	fprog := unix.SockFprog{Len: uint16(len(prog)), Filter: &prog[0]}
	r, _, e := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&fprog))) //nolint:gosec // G103: seccomp(2) takes the filter by pointer, which x/sys gives no wrapper for
	switch {
	case e != 0:
		return e
	case r != 0:
		return fmt.Errorf("seccomp: thread %d could not take the filter", r)
	}
	return nil
}

// peakThenFall is the peak-then-fall helper.
func peakThenFall(mib string) int {
	n, err := strconv.Atoi(mib)
	if err != nil {
		return 2
	}
	m, err := syscall.Mmap(-1, 0, n<<20, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		return wrote(0, err)
	}
	for i := 0; i < len(m); i += 4096 {
		m[i] = 1
	}
	// The unmap gives the pages back before it returns, where a process's exit is not seen to
	// have given its pages back by the time its parent's wait returns.
	if err := syscall.Munmap(m); err != nil {
		return wrote(0, err)
	}
	own, err := selfPath()
	if err != nil {
		return wrote(0, err)
	}
	read := "unread" // the root cgroup, and a host without the memory controller, give no memory.current
	if b, err := os.ReadFile(filepath.Join(cgroupMount, own, "memory.current")); err == nil {
		read = strings.TrimSpace(string(b))
	}
	return wrote(fmt.Fprintf(os.Stdout, "current %s\n", read))
}

// selfPath is this process's cgroup v2 path, from /proc/self/cgroup.
func selfPath() (string, error) {
	b, err := os.ReadFile(selfCgroup)
	if err != nil {
		return "", err
	}
	own, ok := ownCgroup(b)
	if !ok {
		return "", fmt.Errorf("%s names no one cgroup v2 path: %q", selfCgroup, b)
	}
	return own, nil
}

// cgroupSelf is the cgroup-self helper.
func cgroupSelf(how string) int {
	own, err := selfPath()
	if err != nil {
		return wrote(0, err)
	}
	var read [3]string
	for i, name := range []string{"memory.max", "memory.swap.max", "memory.oom.group"} {
		b, err := os.ReadFile(filepath.Join(cgroupMount, own, name))
		if err != nil {
			return wrote(0, err)
		}
		read[i] = strings.TrimSpace(string(b))
	}
	if code := wrote(fmt.Fprintf(os.Stdout, "cgroup %s\nmax %s\nswap %s\ngroup %s\n", own, read[0], read[1], read[2])); code != 0 {
		return code
	}
	return then(how)
}

// cgroupEscape is the cgroup-escape helper.
func cgroupEscape() int {
	own, err := selfPath()
	if err != nil {
		return wrote(0, err)
	}
	if code := wrote(fmt.Fprintf(os.Stdout, "cgroup %s\n", own)); code != 0 {
		return code
	}
	exe, err := os.Executable()
	if err != nil {
		return wrote(0, err)
	}
	child := &process{Path: exe, Args: []string{exe, helperMode + "hold"}, SysProcAttr: &syscall.SysProcAttr{Setsid: true}}
	if err := startTestProcess(child); err != nil {
		return wrote(0, err)
	}
	return wrote(fmt.Fprintf(os.Stdout, "escaped %d\n", child.Process.Pid))
}

// fchdirFolder is the helper mode fchdir-folder: it opens dir by openFolder, enters it by
// shimFchdir, and writes its own getcwd, by the system call, and whether the descriptor is closed.
func fchdirFolder(dir string) int {
	fd, err := openFolder(dir)
	if err != nil {
		return wrote(0, err)
	}
	if err := shimFchdir(fd, func(f func() error) error { return f() }); err != nil {
		return wrote(0, err)
	}
	wd, err := unix.Getwd()
	if err != nil {
		return wrote(0, err)
	}
	_, err = unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	return wrote(fmt.Fprintf(os.Stdout, "wd %s\nclosed %t\n", wd, errors.Is(err, unix.EBADF)))
}
