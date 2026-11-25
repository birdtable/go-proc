package proc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix" //nolint:depguard // arch.ports.23: a row names file system types by their numbers, and asks how many bytes a pipe holds unread
)

// tookStatus is a process's status file with the lines shimTookOf reads.
func tookStatus(noNewPrivs, caps string) string {
	return "Name:\tx\nNoNewPrivs:\t" + noNewPrivs + "\nCapInh:\t0000000000000000\nCapPrm:\t0000000000000000\nCapEff:\t" + caps + "\nCapBnd:\t000001ffffffffff\nCapAmb:\t0000000000000000\n"
}

// tookCover is the root the caller shows for the shim's cover in these rows: tookRun's cgroup mount
// has the root /x and the run's cgroup is its folder run.
const tookCover = "/x/run"

// heldMounts is the mountinfo of a process whose cgroup mount the shim covered: a cgroup v2 mount, a
// tmpfs over it and a cgroup v2 of root tookCover over that, each at the same point, and a mount
// that is no cgroup beside them.
const heldMounts = "24 1 0:22 / /sys rw - sysfs sysfs rw\n" +
	"35 24 0:30 /x /sys/fs/cgroup rw,nosuid,nodev,noexec - cgroup2 cgroup2 rw,nsdelegate\n" +
	"90 35 0:41 / /sys/fs/cgroup ro,nosuid,nodev,noexec - tmpfs tmpfs ro,mode=555\n" +
	"91 90 0:30 " + tookCover + " /sys/fs/cgroup rw,nosuid,nodev,noexec - cgroup2 cgroup2 rw,nsdelegate\n"

// bindBackMounts is heldMounts with the cover built from the caller's own mount: a program that makes
// a user namespace binds the mount aside, mounts a tmpfs, binds the copy back and unmounts it, so
// the mounts have the cover's shape, and the last has the caller's root, /x, not the run's.
const bindBackMounts = "24 1 0:22 / /sys rw - sysfs sysfs rw\n" +
	"35 24 0:30 /x /sys/fs/cgroup rw,nosuid,nodev,noexec - cgroup2 cgroup2 rw,nsdelegate\n" +
	"90 35 0:41 / /sys/fs/cgroup ro,nosuid,nodev,noexec - tmpfs tmpfs ro,mode=555\n" +
	"91 90 0:30 /x /sys/fs/cgroup rw,nosuid,nodev,noexec - cgroup2 cgroup2 rw,nsdelegate\n"

// tookRun is a runCgroup whose cgroup folder, run beneath the cgroup v2 mount of root /x, and proc
// are folders in the test, with the caller's mountinfo in proc/self that says so.
func tookRun(t *testing.T) *runCgroup {
	t.Helper()
	base, proc := t.TempDir(), t.TempDir()
	dir := filepath.Join(base, "run")
	if err := os.MkdirAll(filepath.Join(proc, "self"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	self := "24 1 0:22 / /sys rw - sysfs sysfs rw\n35 24 0:30 /x " + base + " rw - cgroup2 cgroup2 rw\n"
	if err := os.WriteFile(filepath.Join(proc, "self", "mountinfo"), []byte(self), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.events"), []byte("populated 1\nfrozen 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// signal sends nothing: a refusal kills the groups cgroup.procs lists, and these are not processes.
	return &runCgroup{path: dir, proc: proc, ctx: t.Context(), signal: func(int, syscall.Signal) error { return nil }}
}

// openMounts is heldMounts with the shim's cover left out: the caller's cgroup mount, open.
const openMounts = "24 1 0:22 / /sys rw - sysfs sysfs rw\n" +
	"35 24 0:30 / /sys/fs/cgroup rw,nosuid,nodev,noexec - cgroup2 cgroup2 rw,nsdelegate\n"

// tookTree plants the status of the process pid beneath proc, and its mountinfo as heldMounts.
func tookTree(t *testing.T, proc, pid, status string) {
	t.Helper()
	dir := filepath.Join(proc, pid)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o600); err != nil {
		t.Fatal(err)
	}
	tookMounts(t, proc, pid, heldMounts)
}

// tookMounts plants mountinfo as the mounts of the process pid beneath proc.
func tookMounts(t *testing.T, proc, pid, mountinfo string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(proc, pid, "mountinfo"), []byte(mountinfo), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestShimTookOfNamesWhatAProcessLacksOfTheShimsSteps: shimTookOf, of a process whose status is
// given, answers "" for one with no_new_privs set and no capability in any set, and for one that is
// gone; and else what it lacks: no_new_privs not set, or a capability in the inheritable, permitted,
// effective or ambient set.
//
// What it costs when this fails: a program that skipped the shim, with the capabilities the run
// starts it with, and wrote the ready byte itself, is taken as held, or a run the shim set up is
// refused.
func TestShimTookOfNamesWhatAProcessLacksOfTheShimsSteps(t *testing.T) {
	t.Parallel()
	const zero = "0000000000000000"
	for _, c := range []struct{ name, status, want string }{
		{"the shim's process", tookStatus("1", zero), ""},
		{"no_new_privs not set", tookStatus("0", zero), "has not set no_new_privs"},
		{"a capability in its effective set", tookStatus("1", "0000000000200000"), "holds capabilities (CapEff 0000000000200000)"},
		{"a capability in its permitted set", strings.Replace(tookStatus("1", zero), "CapPrm:\t"+zero, "CapPrm:\t0000000000200000", 1), "holds capabilities (CapPrm 0000000000200000)"},
		{"a capability in its ambient set", strings.Replace(tookStatus("1", zero), "CapAmb:\t"+zero, "CapAmb:\t0000000000200000", 1), "holds capabilities (CapAmb 0000000000200000)"},
		{"a capability in its inheritable set", strings.Replace(tookStatus("1", zero), "CapInh:\t"+zero, "CapInh:\t0000000000200000", 1), "holds capabilities (CapInh 0000000000200000)"},
	} {
		proc := t.TempDir()
		tookTree(t, proc, "7", c.status)
		if got := (&runCgroup{proc: proc}).shimTookOf("7"); got != c.want {
			t.Errorf("%s: shimTookOf says %q, want %q", c.name, got, c.want)
		}
	}
	if got := (&runCgroup{proc: t.TempDir()}).shimTookOf("8"); got != "" {
		t.Errorf("a process that is gone: shimTookOf says %q, want \"\"", got)
	}
}

// TestShimTookChecksEveryProcessOfTheRunsCgroup: shimTook, of a cgroup whose cgroup.threads lists two
// threads, is nil where both hold what the shim leaves, and where one does not, a refusal that
// matches ErrRefused and errShimForged and names that process, though the other is the shim's: a
// helper a forging program forked before it dropped its capabilities is found too. A cgroup.threads
// that cannot be read is a refusal.
//
// What it costs when this fails: a forging program keeps a helper with CAP_SYS_ADMIN beside the
// process that dropped its own, and the caller takes the run as held.
func TestShimTookChecksEveryProcessOfTheRunsCgroup(t *testing.T) {
	t.Parallel()
	const zero = "0000000000000000"
	run := tookRun(t)
	dir, proc := run.path, run.proc
	if err := os.WriteFile(filepath.Join(dir, "cgroup.threads"), []byte("7\n9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tookTree(t, proc, "7", tookStatus("1", zero))
	tookTree(t, proc, "9", tookStatus("1", zero))
	if err := run.shimTook(); err != nil {
		t.Errorf("two processes the shim set up: %v, want nil", err)
	}
	tookTree(t, proc, "9", tookStatus("1", "0000000000200000"))
	err := run.shimTook()
	if !errors.Is(err, ErrRefused) || !errors.Is(err, errShimForged) || err == nil || !strings.Contains(err.Error(), "thread 9 ") {
		t.Errorf("a helper with a capability beside the shim's process: %v, want ErrRefused with errShimForged naming thread 9", err)
	}
	if err := os.Remove(filepath.Join(dir, "cgroup.threads")); err != nil {
		t.Fatal(err)
	}
	if err := run.shimTook(); !errors.Is(err, errShimForged) {
		t.Errorf("a cgroup.threads that cannot be read: %v, want errShimForged", err)
	}
}

// TestShimTookReadsBackTheCoversOfEachProcess: shimTook, of a cgroup whose cgroup.procs lists two
// processes that each hold what the shim leaves of its steps, is nil where both have the shim's cover
// over the cgroup mount (heldMounts), and where one has none (openMounts) a refusal that matches
// ErrRefused and errShimForged and names that process and the mount point, though the other is
// covered. A mountinfo that cannot be read, or is no mountinfo, is a refusal; one whose process is
// gone is none.
//
// What it costs when this fails: a program that skipped the shim, dropped its capabilities and set
// no_new_privs itself before it wrote the ready byte keeps the caller's cgroup mounts, and the
// caller takes the run as held.
func TestShimTookReadsBackTheCoversOfEachProcess(t *testing.T) {
	t.Parallel()
	const zero = "0000000000000000"
	run := tookRun(t)
	dir, proc := run.path, run.proc
	if err := os.WriteFile(filepath.Join(dir, "cgroup.threads"), []byte("7\n9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tookTree(t, proc, "7", tookStatus("1", zero))
	tookTree(t, proc, "9", tookStatus("1", zero))
	if err := run.shimTook(); err != nil {
		t.Errorf("two processes with the shim's covers: %v, want nil", err)
	}
	tookMounts(t, proc, "9", openMounts)
	err := run.shimTook()
	if !errors.Is(err, ErrRefused) || !errors.Is(err, errShimForged) || err == nil || !strings.Contains(err.Error(), "thread 9 ") || !strings.Contains(err.Error(), "/sys/fs/cgroup") {
		t.Errorf("a process with the cgroup mount open beside a covered one: %v, want ErrRefused with errShimForged naming thread 9 and /sys/fs/cgroup", err)
	}
	for name, mountinfo := range map[string]string{"no mountinfo line": "not a line\n"} {
		tookMounts(t, proc, "9", mountinfo)
		if err := run.shimTook(); !errors.Is(err, errShimForged) {
			t.Errorf("a mountinfo that is %s: %v, want errShimForged", name, err)
		}
	}
	if err := os.Remove(filepath.Join(proc, "9", "mountinfo")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(proc, "9", "mountinfo"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := run.shimTook(); !errors.Is(err, errShimForged) {
		t.Errorf("a mountinfo that cannot be read: %v, want errShimForged", err)
	}
	if got := run.coversOf("11", tookCover); got != "" {
		t.Errorf("a process that is gone: coversOf says %q, want \"\"", got)
	}
}

// TestCoversOfTakesATaskThatIsEndingForGoneAndALiveOneThatAnswersEINVALForForged: coversOf, where
// a task's mountinfo answers EINVAL, gives "" where its status then says zombie or dead, or is no file;
// where its status says it still runs, reads the mountinfo again, a millisecond apart, up to
// coversTries times: "" where a later read finds the task gone, a refusal ("mounts could not be
// read") after coversTries reads where it answers EINVAL to the end; and a refusal at once, on the
// first read, for any other error. A seam answers the reads.
//
// What it costs when this fails: a healthy Contained run whose shim's task is ending as the check
// reads it is refused as forged,
// or a live program whose mountinfo cannot be read is taken as held.
func TestCoversOfTakesATaskThatIsEndingForGoneAndALiveOneThatAnswersEINVALForForged(t *testing.T) {
	t.Parallel()
	state := func(s string) ([]byte, error) { return []byte("Name:\tx\nState:\t" + s + "\n"), nil }
	gone := func() ([]byte, error) { return nil, os.ErrNotExist }
	for _, c := range []struct {
		name    string
		mounts  func(try int) error // the answer of the mountinfo read, by its try
		status  func(try int) ([]byte, error)
		refused bool
		reads   int // mountinfo reads
	}{
		{"a zombie", func(int) error { return syscall.EINVAL }, func(int) ([]byte, error) { return state("Z (zombie)") }, false, 1},
		{"a dead task", func(int) error { return syscall.EINVAL }, func(int) ([]byte, error) { return state("X (dead)") }, false, 1},
		{"a task reaped, no status", func(int) error { return syscall.EINVAL }, func(int) ([]byte, error) { return gone() }, false, 1},
		{"a task that ends on its third read", func(try int) error {
			if try < 3 {
				return syscall.EINVAL
			}
			return os.ErrNotExist
		}, func(int) ([]byte, error) { return state("S (sleeping)") }, false, 3},
		{"a task that runs and answers EINVAL to the end", func(int) error { return syscall.EINVAL }, func(int) ([]byte, error) { return state("S (sleeping)") }, true, coversTries},
		{"another error", func(int) error { return syscall.EACCES }, func(int) ([]byte, error) { return state("Z (zombie)") }, true, 1},
	} {
		reads := 0
		run := &runCgroup{proc: "/p", read: func(name string) ([]byte, error) {
			if strings.HasSuffix(name, "status") {
				return c.status(reads)
			}
			reads++
			return nil, c.mounts(reads)
		}}
		got := run.coversOf("7", tookCover)
		if refused := got != ""; refused != c.refused || (c.refused && !strings.HasPrefix(got, "mounts could not be read")) || reads != c.reads {
			t.Errorf("%s: coversOf says %q after %d reads, want refused %v after %d", c.name, got, reads, c.refused, c.reads)
		}
	}
}

// TestShimTookReadsAZombieLeaderForItselfAndItsLiveThreadForTheirOwn: shimTook, of a cgroup whose
// cgroup.threads lists a thread-group leader that is a zombie, as one that called the thread-only exit is,
// and a live thread of its group, takes the zombie, whose mountinfo answers EINVAL, for gone for
// itself alone and refuses the live thread: one that kept the caller's cgroup mount open, one that
// holds a capability the leader's status does not, and one that has not set no_new_privs where the
// leader has. A leader that is a zombie beside a live thread that holds the shim's covers is nil.
//
// What it costs when this fails: a program that skipped the shim, set no_new_privs and dropped its
// capabilities, started a thread that keeps the caller's cgroup mounts and ended its leader with the
// thread-only exit, is read as gone and the caller takes the run as held.
func TestShimTookReadsAZombieLeaderForItselfAndItsLiveThreadForTheirOwn(t *testing.T) {
	t.Parallel()
	const zero = "0000000000000000"
	zombie := "State:\tZ (zombie)\n" + tookStatus("1", zero)
	for _, c := range []struct {
		name, mounts, status, want string
	}{
		{"a live thread with the covers", heldMounts, tookStatus("1", zero), ""},
		{"a live thread with the cgroup mount open", openMounts, tookStatus("1", zero), "thread 8 keeps a cgroup mount uncovered"},
		{"a live thread with a capability", heldMounts, tookStatus("1", "0000000000200000"), "thread 8 holds capabilities"},
		{"a live thread without no_new_privs", heldMounts, tookStatus("0", zero), "thread 8 has not set no_new_privs"},
	} {
		run := tookRun(t)
		if err := os.WriteFile(filepath.Join(run.path, "cgroup.threads"), []byte("7\n8\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		tookTree(t, run.proc, "7", zombie)
		tookTree(t, run.proc, "8", c.status)
		tookMounts(t, run.proc, "8", c.mounts)
		run.read = func(name string) ([]byte, error) {
			if name == filepath.Join(run.proc, "7", "mountinfo") {
				return nil, syscall.EINVAL // a zombie's
			}
			return os.ReadFile(name)
		}
		err := run.shimTook()
		if c.want == "" && err != nil || c.want != "" && (!errors.Is(err, errShimForged) || err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s, beside a zombie leader: %v, want %q", c.name, err, c.want)
		}
	}
}

// TestShimTookRefusesACoverBuiltFromTheCallersOwnMount: shimTook, of a process whose mountinfo has the
// cover's shape, a cgroup v2 on a tmpfs on a cgroup mount at one point, but whose last mount has the
// caller's root, not the run's, as a program that made a user namespace and bound the caller's mount
// back over a tmpfs has, is a refusal that names the thread; the same shape with the run's root is nil.
// A caller whose own mountinfo has no cgroup v2 mount above the run's cgroup is a refusal.
//
// What it costs when this fails: a program of the caller's own binary's init that skipped the shim and
// rebuilt the cover's shape from the caller's mount passes the covers check and keeps the caller's
// cgroup tree.
func TestShimTookRefusesACoverBuiltFromTheCallersOwnMount(t *testing.T) {
	t.Parallel()
	const zero = "0000000000000000"
	run := tookRun(t)
	if err := os.WriteFile(filepath.Join(run.path, "cgroup.threads"), []byte("9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tookTree(t, run.proc, "9", tookStatus("1", zero))
	if err := run.shimTook(); err != nil {
		t.Errorf("the shim's cover, of the run's root: %v, want nil", err)
	}
	tookMounts(t, run.proc, "9", bindBackMounts)
	if err := run.shimTook(); !errors.Is(err, errShimForged) || err == nil || !strings.Contains(err.Error(), "thread 9 keeps a cgroup mount uncovered at /sys/fs/cgroup") {
		t.Errorf("the cover's shape with the caller's root: %v, want errShimForged naming thread 9 and /sys/fs/cgroup", err)
	}
	tookMounts(t, run.proc, "9", heldMounts)
	if err := os.WriteFile(filepath.Join(run.proc, "self", "mountinfo"), []byte("24 1 0:22 / /sys rw - sysfs sysfs rw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run.shimTook(); !errors.Is(err, errShimForged) {
		t.Errorf("a caller with no cgroup v2 mount above the run: %v, want errShimForged", err)
	}
}

// TestShimTookFreezesTheRunsCgroupAcrossItsReadAndThawsIt: shimTook writes 1 to the run's cgroup.freeze
// before it reads the threads and 0 after, whatever the read found, and a thread that would show after
// the list is read, as a forger's child forked in the gap does, does not show: a seam reads the list
// and then adds a thread unless the cgroup is frozen, and shimTook, of a thread the shim set up, is
// nil with the list as it was and cgroup.freeze 0 after. A cgroup.freeze that cannot be written, and
// a cgroup.events that never says frozen 1 before the run's context ends, are each a refusal that matches
// ErrRefused and not errShimForged, with cgroup.freeze 0 after; a cgroup.events that cannot be read is one.
//
// What it costs when this fails: a forger forks a child that keeps its capabilities and the caller's
// mounts after the list is read, its parent drops its own and covers, and the check passes with the
// child unread; or a run is left frozen.
func TestShimTookFreezesTheRunsCgroupAcrossItsReadAndThawsIt(t *testing.T) {
	t.Parallel()
	const zero = "0000000000000000"
	run := tookRun(t)
	threads, freeze := filepath.Join(run.path, "cgroup.threads"), filepath.Join(run.path, "cgroup.freeze")
	if err := os.WriteFile(threads, []byte("7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tookTree(t, run.proc, "7", tookStatus("1", zero))
	run.read = func(name string) ([]byte, error) {
		b, err := os.ReadFile(name)
		if frozen, _ := os.ReadFile(freeze); name == threads && string(frozen) != "1" {
			_ = os.WriteFile(threads, []byte("7\n9\n"), 0o600) // a child forked after the list was read
		}
		return b, err
	}
	err := run.shimTook()
	after, _ := os.ReadFile(threads)
	if state, _ := os.ReadFile(freeze); err != nil || string(after) != "7\n" || string(state) != "0" {
		t.Errorf("a thread the shim set up: %v, the list after %q, cgroup.freeze %q, want nil, \"7\\n\" and \"0\"", err, after, state)
	}
	run.read = nil
	ctx, stop := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer stop()
	run.ctx = ctx
	for name, c := range map[string]struct {
		prepare func() error
	}{
		"a cgroup.freeze that cannot be written": {func() error { return os.Mkdir(freeze, 0o700) }},
		"a cgroup.events that never says frozen": {func() error {
			return os.WriteFile(filepath.Join(run.path, "cgroup.events"), []byte("populated 1\nfrozen 0\n"), 0o600)
		}},
		"a cgroup.events that cannot be read": {func() error { return os.Remove(filepath.Join(run.path, "cgroup.events")) }},
	} {
		if err := os.RemoveAll(freeze); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(run.path, "cgroup.events"), []byte("populated 1\nfrozen 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := c.prepare(); err != nil {
			t.Fatal(err)
		}
		err := run.shimTook()
		if state, _ := os.ReadFile(freeze); !errors.Is(err, ErrRefused) || errors.Is(err, errShimForged) || (name != "a cgroup.freeze that cannot be written" && string(state) != "0") {
			t.Errorf("%s: %v, cgroup.freeze %q, want ErrRefused, not errShimForged, and 0", name, err, state)
		}
	}
}

// TestShimTookReadsTheThreadsOfACgroupBelowTheRuns: shimTook, of a run whose cgroup.threads lists the
// shim's thread and whose folder x, a cgroup below it, lists another, as a thread a program moved into
// a child cgroup is, reads both: nil where both hold what the shim leaves, and where the one in x has a
// capability, a refusal naming it that matches errShimForged. A cgroup.threads below the run's that
// cannot be read is a refusal.
//
// What it costs when this fails: a forger moves a helper that kept its capabilities and the caller's
// mounts into a child cgroup before it writes the byte, the run's own list is the shim's alone, and
// the caller takes the run as held.
func TestShimTookReadsTheThreadsOfACgroupBelowTheRuns(t *testing.T) {
	t.Parallel()
	const zero = "0000000000000000"
	run := tookRun(t)
	below := filepath.Join(run.path, "x")
	if err := os.Mkdir(below, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run.path, "cgroup.threads"), []byte("7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(below, "cgroup.threads"), []byte("9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tookTree(t, run.proc, "7", tookStatus("1", zero))
	tookTree(t, run.proc, "9", tookStatus("1", zero))
	if err := run.shimTook(); err != nil {
		t.Errorf("a thread of the run and one of a cgroup below it, each the shim's: %v, want nil", err)
	}
	tookTree(t, run.proc, "9", tookStatus("1", "0000000000200000"))
	if err := run.shimTook(); !errors.Is(err, errShimForged) || err == nil || !strings.Contains(err.Error(), "thread 9 holds capabilities") {
		t.Errorf("a thread with a capability in a cgroup below the run's: %v, want errShimForged naming thread 9", err)
	}
	if err := os.Remove(filepath.Join(below, "cgroup.threads")); err != nil {
		t.Fatal(err)
	}
	if err := run.shimTook(); !errors.Is(err, errShimForged) {
		t.Errorf("a cgroup.threads below the run's that cannot be read: %v, want errShimForged", err)
	}
}

// TestAShimTookRefusalKillsEachThreadGroupByItsId: shimTook, where it refuses the run, sends SIGKILL to
// each thread-group id the run's cgroup.procs lists, and to those of each cgroup below it, by the
// pid and not by cgroup.kill alone (which signals a group's leader, and a leader that ended with
// the thread-only exit takes no signal): a run it does not refuse sends none. A group that is gone,
// ESRCH, is no error; another error is joined to the refusal.
//
// What it costs when this fails: a forger the check refused, whose leader ended with the thread-only
// exit, outlives the kill and holds the report open, and the run ends at its Timeout, not at the
// refusal.
func TestAShimTookRefusalKillsEachThreadGroupByItsId(t *testing.T) {
	t.Parallel()
	const zero = "0000000000000000"
	run := tookRun(t)
	below := filepath.Join(run.path, "x")
	if err := os.Mkdir(below, 0o700); err != nil {
		t.Fatal(err)
	}
	for file, ids := range map[string]string{
		filepath.Join(run.path, "cgroup.threads"): "7\n9\n", filepath.Join(run.path, "cgroup.procs"): "7\n",
		filepath.Join(below, "cgroup.threads"): "11\n", filepath.Join(below, "cgroup.procs"): "11\n",
	} {
		if err := os.WriteFile(file, []byte(ids), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tookTree(t, run.proc, "7", tookStatus("1", zero))
	tookTree(t, run.proc, "9", tookStatus("1", zero))
	tookTree(t, run.proc, "11", tookStatus("1", zero))
	var sent []string
	run.signal = func(pid int, sig syscall.Signal) error {
		sent = append(sent, strconv.Itoa(pid)+" "+sig.String())
		if pid == 11 {
			return syscall.ESRCH
		}
		return nil
	}
	if err := run.shimTook(); err != nil || len(sent) != 0 {
		t.Errorf("a run held: %v, sent %q, want nil and nothing", err, sent)
	}
	tookTree(t, run.proc, "9", tookStatus("0", zero))
	err := run.shimTook()
	slices.Sort(sent)
	if !errors.Is(err, errShimForged) || !slices.Equal(sent, []string{"11 killed", "7 killed"}) {
		t.Errorf("a run refused: %v, sent %q, want errShimForged and SIGKILL to 7 and 11 (11 gone, no error)", err, sent)
	}
	run.signal = func(int, syscall.Signal) error { return syscall.EPERM }
	if err := run.shimTook(); !errors.Is(err, syscall.EPERM) {
		t.Errorf("a signal that fails: %v, want EPERM joined to the refusal", err)
	}
}

// TestStartedGivesTheGoAheadOnlyOnceTheRunsThreadsWereRead: started, of a run whose report says
// shimReady and whose one thread holds what the shim leaves, writes shimGo to the go-ahead, and the
// go-ahead is empty still when the run's threads are read, which a seam on that read sees; of a run
// whose thread holds a capability, it writes nothing, closes the go-ahead and gives a refusal that
// matches errShimForged. The report ends in each once the go-ahead was read to the byte or to its
// end, as the second stage's exec or exit ends it.
//
// What it costs when this fails: the second stage execs the program while the caller still reads the
// run's threads, so a healthy program that makes a user namespace at once is read holding
// capabilities, refused and killed; or a refused run's stage
// is told to go on, and its program starts.
func TestStartedGivesTheGoAheadOnlyOnceTheRunsThreadsWereRead(t *testing.T) {
	t.Parallel()
	const zero = "0000000000000000"
	for _, c := range []struct {
		name, caps string
		goAhead    string
		forged     bool
	}{
		{"a thread the shim set up", zero, string(shimGo), false},
		{"a thread that holds a capability", "0000000000200000", "", true},
	} {
		run := tookRun(t)
		for _, f := range []string{"cgroup.threads", "cgroup.procs"} {
			if err := os.WriteFile(filepath.Join(run.path, f), []byte("7\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(run.path, "cgroup.kill"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		tookTree(t, run.proc, "7", tookStatus("1", c.caps))
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		goR, goW, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte{shimReady}); err != nil {
			t.Fatal(err)
		}
		// started closes its own copy of the report's write end; w stays open, as the stage holds its own.
		own, err := os.Open(os.DevNull)
		if err != nil {
			t.Fatal(err)
		}
		run.shim, run.goAhead = [4]*os.File{r, own, nil, nil}, goW
		conn, err := goR.SyscallConn()
		if err != nil {
			t.Fatal(err)
		}
		// waiting is how many bytes the go-ahead holds unread, or -1.
		waiting := func() int {
			var n int32 = -1
			if err := conn.Control(func(fd uintptr) {
				if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCINQ, uintptr(unsafe.Pointer(&n))); errno != 0 { //nolint:gosec // G103: the ioctl writes its count to n
					n = -1
				}
			}); err != nil {
				return -1
			}
			return int(n)
		}
		early := false
		run.read = func(name string) ([]byte, error) {
			if name == filepath.Join(run.path, "cgroup.threads") {
				early = early || waiting() != 0
			}
			return os.ReadFile(name)
		}
		got := make(chan []byte)
		go func() {
			b, _ := io.ReadAll(goR) // to its end: started closes the go-ahead whether it wrote or not
			_ = w.Close()
			got <- b
		}()
		err = run.started()
		b := <-got
		_ = goR.Close()
		if early || string(b) != c.goAhead || errors.Is(err, errShimForged) != c.forged || (err != nil) != c.forged {
			t.Errorf("%s: %v, the go-ahead read %q, and written before the threads were read %t, want forged %t, %q and false", c.name, err, b, early, c.forged, c.goAhead)
		}
	}
}

// TestShimTookRefusesADescriptorOnACgroupFileSystemOrAMountNamespace: shimTook, of a run whose threads
// each hold what the shim leaves, reads each thread's own fd folder, and is nil where the descriptors are
// the shim's own (a null file, pipes, a memfd), a user namespace and a file on another file system; and
// is a refusal that matches ErrRefused and errShimForged, names the thread and the descriptor's number
// and says what it is on, where one is on a cgroup v2 file system (a folder or a file), on a cgroup v1
// one, on a mount that no mountinfo lists, as a detached cgroup mount is, or is a mount namespace; and
// where the thread that holds it is not the first of the list, which has a file table of its own. A
// thread that is gone, with no fd folder, is gone; an fd folder that cannot be read is a refusal.
//
// What it costs when this fails: a program that skipped the shim keeps a descriptor on the caller's
// cgroup tree, or on a mount namespace of the caller's, opened before the covers, and the check passes;
// through it, by the "*at" calls and ".." or by setns, it reaches the caller's cgroups outside the tree's
// namespace.
func TestShimTookRefusesADescriptorOnACgroupFileSystemOrAMountNamespace(t *testing.T) {
	t.Parallel()
	const zero = "0000000000000000"
	types := map[string]int64{"/dev/null": unix.TMPFS_MAGIC, "pipe:[7]": unix.TMPFS_MAGIC, "/memfd:go-proc-env (deleted)": unix.TMPFS_MAGIC,
		"user:[4026531837]": unix.TMPFS_MAGIC, "/home/x/file": unix.EXT4_SUPER_MAGIC,
		"/sys/fs/cgroup/caller": unix.CGROUP2_SUPER_MAGIC, "/sys/fs/cgroup/a-file": unix.CGROUP2_SUPER_MAGIC,
		"/old/cgroup/v1": unix.CGROUP_SUPER_MAGIC, "/detached": unix.CGROUP2_SUPER_MAGIC}
	for _, c := range []struct {
		name  string
		fds   map[string][]string // thread, then descriptor links, numbered from 0
		bad   string              // "" for a pass, else "thread descriptor what"
		plain bool                // an fd that is a file, not a folder
	}{
		{name: "the shim's own descriptors", fds: map[string][]string{"7": {"/dev/null", "/dev/null", "/dev/null", "pipe:[7]", "/memfd:go-proc-env (deleted)", "pipe:[7]"}}},
		{name: "a user namespace and a file on another file system", fds: map[string][]string{"7": {"user:[4026531837]", "/home/x/file"}}},
		{name: "a folder on cgroup v2", fds: map[string][]string{"7": {"/dev/null", "/sys/fs/cgroup/caller"}}, bad: "thread 7 holds descriptor 1 on a cgroup file system"},
		{name: "a file on cgroup v2", fds: map[string][]string{"7": {"/sys/fs/cgroup/a-file"}}, bad: "thread 7 holds descriptor 0 on a cgroup file system"},
		{name: "a folder on cgroup v1", fds: map[string][]string{"7": {"/old/cgroup/v1"}}, bad: "thread 7 holds descriptor 0 on a cgroup file system"},
		{name: "a detached cgroup mount", fds: map[string][]string{"7": {"/detached"}}, bad: "thread 7 holds descriptor 0 on a cgroup file system"},
		{name: "a mount namespace", fds: map[string][]string{"7": {"/dev/null", "/dev/null", "mnt:[4026531841]"}}, bad: "thread 7 holds descriptor 2 on a mount namespace"},
		{name: "a thread that is not the first, with a table of its own", fds: map[string][]string{"7": {"/dev/null"}, "9": {"/dev/null", "/sys/fs/cgroup/caller"}}, bad: "thread 9 holds descriptor 1 on a cgroup file system"},
		{name: "an fd folder that cannot be read", fds: map[string][]string{"7": {"/dev/null"}}, plain: true, bad: "thread 7 descriptors could not be read"},
	} {
		run := tookRun(t)
		var ids string
		for _, tid := range []string{"7", "9"} {
			links, ok := c.fds[tid]
			if !ok {
				continue
			}
			ids += tid + "\n"
			tookTree(t, run.proc, tid, tookStatus("1", zero))
			fd := filepath.Join(run.proc, tid, "fd")
			if c.plain {
				if err := os.WriteFile(fd, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				continue
			}
			if err := os.Mkdir(fd, 0o700); err != nil {
				t.Fatal(err)
			}
			for n, link := range links {
				if err := os.Symlink(link, filepath.Join(fd, strconv.Itoa(n))); err != nil {
					t.Fatal(err)
				}
			}
		}
		for _, f := range []string{"cgroup.threads", "cgroup.procs"} {
			if err := os.WriteFile(filepath.Join(run.path, f), []byte(ids), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		run.magic = func(path string) (int64, error) {
			link, err := os.Readlink(path)
			if err != nil {
				return 0, err
			}
			return types[link], nil
		}
		err := run.shimTook()
		switch {
		case c.bad == "" && err != nil:
			t.Errorf("%s: %v, want nil", c.name, err)
		case c.bad != "" && (!errors.Is(err, ErrRefused) || !errors.Is(err, errShimForged) || !strings.Contains(fmt.Sprint(err), c.bad)):
			t.Errorf("%s: %v, want a refusal that is errShimForged and says %q", c.name, err, c.bad)
		}
	}
	// a thread that is gone has no fd folder
	run := tookRun(t)
	for _, f := range []string{"cgroup.threads", "cgroup.procs"} {
		if err := os.WriteFile(filepath.Join(run.path, f), []byte("7\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tookTree(t, run.proc, "7", tookStatus("1", zero))
	if err := run.shimTook(); err != nil {
		t.Errorf("a thread with no fd folder: %v, want nil", err)
	}
}
