package proc

import (
	"bufio"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
)

// TestALeaderNamedWithParenthesesIsReadByItsOwnFields: a process whose name holds parentheses,
// spaces and numbers, as a program's file name may, is read with its own group, not one its name
// spells.
//
// What it costs when this fails: a program named to look like a stat line makes Stop's guard read a
// group of its choosing, and a stop by id kills that group.
func TestALeaderNamedWithParenthesesIsReadByItsOwnFields(t *testing.T) {
	t.Parallel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	named := filepath.Join(t.TempDir(), "p) R 1 1 (q)")
	if err := os.Symlink(exe, named); err != nil {
		t.Fatal(err)
	}
	child := &process{Path: named, Args: []string{named, helperMode + "hold"}, SysProcAttr: &syscall.SysProcAttr{Setpgid: true}}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	pid := child.Process.Pid
	t.Cleanup(func() {
		_ = signalPid(syscall.Kill, pid, syscall.SIGKILL)
		_ = child.Wait()
	})
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	if pgid, _, err := leaderHere(pid); err != nil || pgid != pid {
		t.Errorf("leader(%d), whose stat is %q, gives group %d (%v), want %d", pid, stat, pgid, err, pid)
	}
}

// TestAnIDOfAnotherBootOrNamespaceIsStaleAndSignalsNothing: a tree's own id, with its boot's id
// changed to another, as an id kept across a reboot would be, with its pid namespace's number
// changed, as an id handed to a sibling pid namespace would be, or with its time namespace's number
// changed, as an id read in another time namespace would be, is ErrStale and signals nothing, though
// the pid and start time still name the live leader, to Stop and to Interrupt alike. Each part is
// changed alone, so each compare is seen on its own. The tree's own id stops it.
//
// What it costs when this fails: an id kept across a reboot or handed to another pid namespace names
// whatever process there took the leader's pid at the same tick, and a stop kills that process's
// group; or, in another time namespace, a live tree's start time reads otherwise.
func TestAnIDOfAnotherBootOrNamespaceIsStaleAndSignalsNothing(t *testing.T) {
	t.Parallel()
	tree, r := treeHelper(t, "hold")
	t.Cleanup(func() { _ = Stop(tree.ID) })
	_ = lines(t, r, 2)
	pid, born, at, ok := parseTreeID(tree.ID)
	parts := strings.Split(at, ".")
	if !ok || len(parts) != 3 {
		t.Fatalf("Start gave %q, want a pid, a start time, a boot and two namespaces", tree.ID)
	}
	other := func(i int) string { // the place with part i changed to another
		p := append([]string{}, parts...)
		if i == 0 {
			b := []byte(p[0])
			if b[0] == '0' {
				b[0] = '1'
			} else {
				b[0] = '0'
			}
			p[0] = string(b)
		} else {
			n, err := strconv.ParseUint(p[i], 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			p[i] = strconv.FormatUint(n+1, 10)
		}
		return strings.Join(p, ".")
	}
	rows := []struct {
		what string
		part int
	}{{"another boot's", 0}, {"another pid namespace's", 1}, {"another time namespace's", 2}}
	for _, row := range rows {
		id := TreeID(strconv.Itoa(pid) + "." + strconv.FormatUint(born, 10) + "." + other(row.part))
		for _, g := range guardedActs() {
			var calls atomic.Int32
			if err := g.act(id, counted(&calls)); !errors.Is(err, ErrStale) {
				t.Errorf("%s(%q), %s id for the live leader: %v, want ErrStale", g.name, id, row.what, err)
			}
			if n := calls.Load(); n != 0 {
				t.Errorf("%s(%q), %s id, signalled %d times, want none", g.name, id, row.what, n)
			}
		}
	}
	var calls atomic.Int32
	if err := stopTree(tree.ID, counted(&calls)); err != nil || calls.Load() == 0 {
		t.Errorf("stopTree(%q), the tree's own id: %v after %d signals, want the group killed", tree.ID, err, calls.Load())
	}
}

// standInProc is a stand-in proc mount whose self/stat names this process, and whose self/status
// is status.
func standInProc(t *testing.T, status string) string {
	t.Helper()
	proc := t.TempDir()
	if err := os.MkdirAll(filepath.Join(proc, "self"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, "self", "stat"), []byte(strconv.Itoa(os.Getpid())+" (proc.test) S 1 1 1 0 -1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proc, "self", "status"), []byte("Name:\tproc.test\n"+status), 0o600); err != nil {
		t.Fatal(err)
	}
	return proc
}

// TestStartAndStopRefuseAProcMountNotThisNamespaces: stand-ins taken for proc mounts, one whose
// self/stat names another pid, as another pid namespace's /proc would, one with no self/stat, as with
// no /proc, and one whose self/stat names this pid but whose self/status's NSpid lists two, as an
// outer pid namespace's mount does when its pid for this process equals this one's by chance, and a
// folder whose files pass every other check but which is not a proc mount itself, are each
// errProcMount, wrapping errors.ErrUnsupported: Start starts nothing, and Stop signals
// nothing, though the id names a live tree. The real mount passes, and Stop through it kills the tree.
//
// What it costs when this fails: the guard reads another namespace's process for the one a kill here
// reaches, and Stop kills a group the id never named; or, with no /proc, a live tree is ErrStale; or,
// with a folder for /proc, Stop reads a planted stat as a reaped leader's and kills its group.
func TestStartAndStopRefuseAProcMountNotThisNamespaces(t *testing.T) {
	t.Parallel()
	tree, r := treeHelper(t, "hold")
	t.Cleanup(func() { _ = Stop(tree.ID) })
	_ = lines(t, r, 2)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	another := t.TempDir()
	if err := os.MkdirAll(filepath.Join(another, "self"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(another, "self", "stat"), []byte(strconv.Itoa(os.Getpid()+1)+" (proc.test) S 1 1 1 0 -1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// An outer namespace's mount whose self/stat names this pid by chance: its NSpid lists two.
	outer := standInProc(t, "NSpid:\t"+strconv.Itoa(os.Getpid()+7)+"\t"+strconv.Itoa(os.Getpid())+"\n")
	// A folder whose files pass every other check, as a root whose /proc is a folder may hold.
	folder := standInProc(t, "NSpid:\t"+strconv.Itoa(os.Getpid())+"\n")
	if r, err := procOwn(procFS{dir: folder, standIn: true}); err != nil {
		t.Fatalf("the folder must pass every check but the mount's, so that the mount check is what refuses it: %v", err)
	} else {
		r.close()
	}
	for _, proc := range []procFS{{dir: another, standIn: true}, {dir: t.TempDir(), standIn: true}, {dir: outer, standIn: true}, {dir: folder}} {
		mark := filepath.Join(t.TempDir(), "started")
		started, err := start(Detached{Path: exe, Args: []string{helperMode + "touch", mark}}, proc)
		if !errors.Is(err, errProcMount) || !errors.Is(err, errors.ErrUnsupported) || started != (Tree{}) {
			t.Errorf("start with the proc mount %s: %+v, %v, want no tree and errProcMount", proc.dir, started, err)
		}
		if started.Leader != nil {
			_, _ = started.Leader.Wait()
		}
		if _, err := os.Stat(mark); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("start with the proc mount %s: the program ran (%v), want nothing started", proc.dir, err)
		}
		var calls atomic.Int32
		if err := stop(tree.ID, proc, counted(&calls)); !errors.Is(err, errProcMount) {
			t.Errorf("stop of a live tree with the proc mount %s: %v, want errProcMount", proc.dir, err)
		}
		if n := calls.Load(); n != 0 {
			t.Errorf("stop with the proc mount %s signalled %d times, want none", proc.dir, n)
		}
	}
	var calls atomic.Int32
	if err := stop(tree.ID, procFS{dir: procMount}, counted(&calls)); err != nil || calls.Load() == 0 {
		t.Errorf("stop through the real proc mount: %v after %d signals, want the tree killed", err, calls.Load())
	}
}

// TestStartWithNoBootIDStartsNothing: a stand-in proc mount whose self/stat names this process, so
// procOwn passes, but which has no sys/kernel/random/boot_id, as a proc mounted subset=pid has none,
// is errNoPlace, wrapping errors.ErrUnsupported, before anything starts: the program makes no file.
// Stop through it signals nothing, though the id names a live tree.
//
// What it costs when this fails: Start runs the caller's program for a moment, then reads the
// boot's id, fails and kills it, every call, and its error does not say the system gives no tree id.
func TestStartWithNoBootIDStartsNothing(t *testing.T) {
	t.Parallel()
	tree, r := treeHelper(t, "hold")
	t.Cleanup(func() { _ = Stop(tree.ID) })
	_ = lines(t, r, 2)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	subset := standInProc(t, "NSpid:\t"+strconv.Itoa(os.Getpid())+"\n")
	if r, err := procOwn(procFS{dir: subset, standIn: true}); err != nil {
		t.Fatalf("the stand-in mount must pass procOwn, so that the boot's id is what is missing: %v", err)
	} else {
		r.close()
	}
	mark := filepath.Join(t.TempDir(), "started")
	started, err := start(Detached{Path: exe, Args: []string{helperMode + "touch", mark}}, procFS{dir: subset, standIn: true})
	if started.Leader != nil {
		_, _ = started.Leader.Wait()
	}
	if !errors.Is(err, errNoPlace) || !errors.Is(err, errors.ErrUnsupported) || started != (Tree{}) {
		t.Errorf("start with no boot id: %+v, %v, want no tree and errNoPlace", started, err)
	}
	if _, err := os.Stat(mark); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("start with no boot id: the program ran (%v), want nothing started", err)
	}
	var calls atomic.Int32
	if err := stop(tree.ID, procFS{dir: subset, standIn: true}, counted(&calls)); !errors.Is(err, errNoPlace) {
		t.Errorf("stop of a live tree with no boot id: %v, want errNoPlace", err)
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("stop with no boot id signalled %d times, want none", n)
	}
}

// nsHelper starts the test binary as the helper args name, as pid 1 of new user, pid and mount
// namespaces, this process's user and group mapped to root there, with its stdin piped from the test
// and its stdout and stderr to one pipe the test reads, so a helper's error is in the lines the test
// reads; closing the stdin ends a helper that waits on it.
func nsHelper(t *testing.T, args ...string) (*bufio.Reader, io.WriteCloser) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	c := &process{Path: exe, Args: append([]string{exe}, args...), Stdout: w, Stderr: w, SysProcAttr: &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWPID | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}}
	in, err := c.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	err = startTestProcess(c)
	_ = w.Close()
	if err != nil {
		t.Fatalf("starting a helper in pid and user namespaces of its own, which this row needs: %v", err)
	}
	t.Cleanup(func() {
		_ = in.Close()
		within(t, "the namespace helper's end", func() { _ = c.Wait() })
	})
	return bufio.NewReader(r), in
}

// TestStopOfAReapedLeadersIDOverAPlantedProcIsRefusedAndSignalsNothing: in pid and mount namespaces
// of its own, a tree's leader is reaped, and a tmpfs mounted on /proc holds files that pass every
// check but the mount's: self/stat and self/status name this process, boot_id and self/ns give this
// place, and the leader's stat names it alive, with its group and start time. Stop of the tree's id
// there, and Interrupt of it, are each errProcMount, wrapping errors.ErrUnsupported, and send no
// signal, counted by a fake kill; and the scan for a group left behind, asked of the leader's group but for the leader,
// answers that one is left, which claims no group empty that may not be, not the plant's none.
//
// What it costs when this fails: in a root whose /proc is a folder, Stop of an id whose leader is
// gone reads a planted stat as the leader's, and sends SIGKILL to the group that now has its
// number.
func TestStopOfAReapedLeadersIDOverAPlantedProcIsRefusedAndSignalsNothing(t *testing.T) {
	t.Parallel()
	r, _ := nsHelper(t, helperMode+"ns-planted")
	got := lines(t, r, 4)
	if got["planted"] != "true" {
		t.Fatalf("the planted /proc does not pass every check but the mount's (%q), so the row proves nothing", got)
	}
	if got["refused"] != "true" || got["signals"] != "0" {
		t.Errorf("a stop of a reaped leader's id over a planted /proc: errProcMount %s, %s signals; want refused and none", got["refused"], got["signals"])
	}
	if got["left"] != "true" {
		t.Errorf("groupLeft over a planted /proc answers %s, want true: the plant read as no proc", got["left"])
	}
}

// TestAnIDFromASiblingPidNamespaceIsStaleThereAndItsTreeIsLeftAlone: two sibling pid namespaces of
// the same boot. The second starts a tree and is handed an id with its own leader's pid and start
// time and the first's place, as an id given in the first would be when its tree had the same pid
// at the same tick, which pids that are small there and starts that share a tick give often. The
// stop there is ErrStale, signals nothing, and the second's tree runs on.
//
// What it costs when this fails: an id handed as text to a process in another container of the same
// kernel names another live tree there, and a stop kills it, leaving the tree the id was given for
// running.
func TestAnIDFromASiblingPidNamespaceIsStaleThereAndItsTreeIsLeftAlone(t *testing.T) {
	t.Parallel()
	first, _ := nsHelper(t, helperMode+"ns-place")
	a := lines(t, first, 1)["place"]
	second, _ := nsHelper(t, helperMode+"ns-stop", a)
	got := lines(t, second, 4)
	ours, theirs := strings.Split(got["here"], "."), strings.Split(a, ".")
	if len(ours) != 3 || len(theirs) != 3 || ours[0] != theirs[0] || ours[1] == theirs[1] {
		t.Fatalf("the namespaces' places are %q and %q, want one boot and two pid namespaces", a, got["here"])
	}
	if got["stale"] != "true" || got["signals"] != "0" || got["alive"] != "true" {
		t.Errorf("an id from the sibling namespace %s, stopped in %s: stale %s, %s signals, its own tree alive %s; want ErrStale, none and alive", theirs[1], ours[1], got["stale"], got["signals"], got["alive"])
	}
}

// TestABindOverALiveLeadersStatBelowTheProcMountIsRefusedAndSignalsNothing: in pid and mount
// namespaces of its own, a plain file naming a live tree's leader as born a tick later is bound over
// the leader's /proc/<pid>/stat, below a real proc mount that passes every check. Stop of the id that
// file names is EXDEV, read by openat2 with RESOLVE_NO_XDEV below the mount, sends no signal, counted
// by a fake kill, and the leader runs on. The bind is made in mount and user namespaces of the
// row's own, as the other rows here need; where it cannot be made the row fails with its error.
//
// What it costs when this fails: a process that may mount below /proc makes the guard read a stat of
// its making as a live leader's, and a stop by an id it chose kills that leader's group.
func TestABindOverALiveLeadersStatBelowTheProcMountIsRefusedAndSignalsNothing(t *testing.T) {
	t.Parallel()
	r, _ := nsHelper(t, helperMode+"ns-bound", "openat2")
	got := lines(t, r, 8)
	if got["bound"] != "true" {
		t.Fatalf("the bind over a leader's stat could not be made (%s), so the row proves nothing", got["bound"])
	}
	if got["shows"] != "true" {
		t.Fatalf("a read of the leader's stat by its path does not give the bound file (%q), so the row proves nothing", got)
	}
	if got["refused"] != "true" || got["signals"] != "0" || got["alive"] != "true" {
		t.Errorf("a stop of an id a bound stat names: EXDEV %s, %s signals, the leader alive %s; want EXDEV, none and alive", got["refused"], got["signals"], got["alive"])
	}
}

// TestTheProcMountOpenedFirstIsTheOneRead: in pid and mount namespaces of its own, the proc mount is
// opened and checked, and then a tmpfs mounted on /proc names a live leader as born a tick later.
// Stop's guard, given the mount it opened, reads the leader through it, not the tmpfs: the stop of
// the id the tmpfs names is ErrStale and sends no signal. So it is where openat2 is absent, by openat
// below the same descriptor: taken so by the test seam, and found so where a seccomp filter answers
// openat2 with ENOSYS, as a kernel before 5.6 does, or with EPERM, as a container's filter does. Where
// the kernel gives openat2, the mount is read by it.
//
// What it costs when this fails: the guard reads each file by path, so a mount made on /proc after the
// check is read as the leader's, and a stop by an id of its choosing kills the group;
// or, where openat2 is refused, Start and Stop fail on that kernel, or read by openat where openat2
// is given, and a file bound below /proc is read as the kernel's.
func TestTheProcMountOpenedFirstIsTheOneRead(t *testing.T) {
	t.Parallel()
	for _, how := range []string{"openat2", "openat", "enosys", "eperm"} {
		t.Run(how, func(t *testing.T) {
			t.Parallel()
			r, _ := nsHelper(t, helperMode+"ns-bound", how)
			got := lines(t, r, 8)
			if got["held"] != "true" || got["heldsignals"] != "0" {
				t.Errorf("a stop through the mount opened before a tmpfs on /proc: ErrStale %s, %s signals; want ErrStale and none", got["held"], got["heldsignals"])
			}
			if want := strconv.FormatBool(how != "openat2"); got["flat"] != want {
				t.Errorf("the mount reads by openat: %s; want %s", got["flat"], want)
			}
		})
	}
}

// TestAProcFolderNotTheRootOfItsFileSystemIsRefused: a folder of a proc file system that is not its
// root, this process's /proc/<pid>, is errProcMount, with openat2 and without, before anything below
// it is read: a folder of /proc bound on /proc passes the type check alone.
//
// What it costs when this fails: a bind of a proc folder on /proc is read as the mount, and a file
// below it as a process's.
func TestAProcFolderNotTheRootOfItsFileSystemIsRefused(t *testing.T) {
	t.Parallel()
	dir := procMount + "/" + strconv.Itoa(os.Getpid())
	for _, flat := range []bool{false, true} {
		r, err := procOwn(procFS{dir: dir, noOpenat2: flat})
		if err == nil {
			r.close()
		}
		if !errors.Is(err, errProcMount) || !strings.Contains(err.Error(), "not the root") {
			t.Errorf("procOwn of %s, openat alone %t: %v; want errProcMount, not the root", dir, flat, err)
		}
	}
}

// TestBootIDIsReadOnlyOnTheProcMountsOwnFileSystem: in mount namespaces of its own, /proc/sys bound
// over itself read-only, as a container runtime binds it, still gives a place; a file holding
// another boot's id bound over /proc/sys/kernel/random/boot_id, as LXC and systemd-nspawn bind one,
// is errNoPlace wrapping EXDEV, by openat2 and by openat where a seccomp filter answers openat2 with
// ENOSYS; and a tmpfs mounted on /proc/sys with a boot_id of its own is errNoPlace.
//
// What it costs when this fails: a mount on /proc/sys, or a file bound over boot_id, hands Stop a
// boot of its choosing, and an id kept from another boot is read as this one's.
func TestBootIDIsReadOnlyOnTheProcMountsOwnFileSystem(t *testing.T) {
	t.Parallel()
	for _, how := range []string{"openat2", "enosys"} {
		t.Run(how, func(t *testing.T) {
			t.Parallel()
			r, _ := nsHelper(t, helperMode+"ns-sys", how)
			got := lines(t, r, 3)
			if got["own"] != "<nil>" {
				t.Errorf("place with /proc/sys bound over itself: %s; want a place", got["own"])
			}
			if got["bootbound"] != "true" {
				t.Errorf("place with a file bound over boot_id: errNoPlace and EXDEV %s; want both", got["bootbound"])
			}
			if got["other"] != "true" {
				t.Errorf("place with a tmpfs on /proc/sys: errNoPlace %s; want errNoPlace", got["other"])
			}
		})
	}
}

// TestStopOnLinuxSendsTheKillAlone: on Linux Stop sends a live tree's group SIGKILL and no other
// signal, no SIGSTOP before it: the kill there reaches a process mid-fork and mid-exec, so the stop
// macOS sends first would be a thing a tree's parent could see, for no gain.
//
// What it costs when this fails: a Linux tree is stopped before its kill, which its parent sees as
// a stop, for nothing the kill there needs.
func TestStopOnLinuxSendsTheKillAlone(t *testing.T) {
	t.Parallel()
	tree, r := treeHelper(t, "hold")
	t.Cleanup(func() { _ = Stop(tree.ID) })
	lines(t, r, 2)
	var sent []syscall.Signal
	k := killer{signal: func(pid int, sig syscall.Signal) error {
		sent = append(sent, sig)
		return signalPid(syscall.Kill, pid, sig)
	}, pause: func() {}}
	if err := stopTree(tree.ID, k); err != nil {
		t.Fatalf("stopTree(%q) of the live tree: %v", tree.ID, err)
	}
	if len(sent) != 1 || sent[0] != syscall.SIGKILL {
		t.Errorf("stopTree(%q) sent %v, want SIGKILL alone", tree.ID, sent)
	}
	drained(t, "the tree's leader and grandchild, once stopped", r)
	_, _ = tree.Leader.Wait()
}
