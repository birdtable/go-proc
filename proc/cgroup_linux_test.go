package proc

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix" //nolint:depguard // arch.ports.23: the fake cgroup fs answers by the same magic and access modes the check asks with
)

// delegated is the delegated parent this process's runs make their cgroups in, and nil; or "" and
// the refusal of the check that found none, which the test then proves in place of the cap, saying
// so in its log. A native run on a delegated host counts none of those lines.
func delegated(t *testing.T) (string, error) {
	t.Helper()
	p, err := cgroupHost{}.parent()
	if err != nil {
		t.Logf("no cgroup is delegated here, so the row proves the refusal: %v", err)
	}
	return p, err
}

// refusedHere fails the test unless c's run was refused before it started, as a TreeMemory is where
// no cgroup is delegated: with ErrRefused, errors.ErrUnsupported, Code -1 and no output.
func refusedHere(t *testing.T, what string, res Result, err error) {
	t.Helper()
	if !errors.Is(err, ErrRefused) || !errors.Is(err, errors.ErrUnsupported) || res.Code != -1 || len(res.Stdout) != 0 {
		t.Errorf("%s with no cgroup delegated: %v, Code %d, stdout %q, want ErrRefused, ErrUnsupported, Code -1 and no output", what, err, res.Code, res.Stdout)
	}
}

// peakHere is whether a run here reads its peak: where a cgroup is delegated and its kernel gives
// memory.peak (Linux 5.19).
func peakHere(t *testing.T) bool {
	t.Helper()
	p, err := delegated(t)
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(p, "memory.peak"))
	return err == nil
}

// fakeCgroups is a cgroupFS of maps: a file it does not hold is ENOENT, a path in denied fails
// access with EACCES, and each write is kept.
type fakeCgroups struct {
	magicOf  int64
	files    map[string]string
	xattrs   map[string]string
	denied   map[string]bool
	writeErr error
	wrote    map[string]string
	reads    []string
}

func (f *fakeCgroups) magic(string) (int64, error) { return f.magicOf, nil }

func (f *fakeCgroups) read(path string) ([]byte, error) {
	f.reads = append(f.reads, path)
	if v, ok := f.files[path]; ok {
		return []byte(v), nil
	}
	return nil, unix.ENOENT
}

func (f *fakeCgroups) xattr(path, name string) ([]byte, error) {
	if v, ok := f.xattrs[path+"\x00"+name]; ok {
		return []byte(v), nil
	}
	return nil, unix.ENODATA
}

func (f *fakeCgroups) access(path string, mode uint32) error {
	if f.denied[path] {
		return unix.EACCES
	}
	if _, ok := f.files[path]; ok || mode != unix.F_OK {
		return nil
	}
	return unix.ENOENT
}

func (f *fakeCgroups) write(path, value string) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.wrote[path] = value
	return nil
}

// fakeParent is the delegated parent of the fake host: the caller runs in its leaf, app.
const fakeParent = "/cg/user.slice/svc.service"

// goodCgroups is a fake host whose every check passes: cgroup v2 at /cg, the caller in
// fakeParent/app, fakeParent marked delegated, listing memory, writable, with cgroup.kill and
// memory.swap.max, and giving its children memory already when given is set.
func goodCgroups(given bool) *fakeCgroups {
	sub := ""
	if given {
		sub = "memory pids\n"
	}
	return &fakeCgroups{
		magicOf: unix.CGROUP2_SUPER_MAGIC,
		files: map[string]string{
			"/self":                                "0::/user.slice/svc.service/app\n",
			fakeParent + "/cgroup.controllers":     "cpu memory pids\n",
			fakeParent + "/cgroup.subtree_control": sub,
			fakeParent + "/cgroup.procs":           "",
			fakeParent + "/cgroup.kill":            "",
			fakeParent + "/memory.swap.max":        "max\n",
		},
		xattrs: map[string]string{fakeParent + "\x00" + delegateMark: delegateValue},
		denied: map[string]bool{},
		wrote:  map[string]string{},
	}
}

// fakeHost is the cgroupHost of f, whose caller is uid 1000 whatever this process's is.
func fakeHost(f *fakeCgroups) cgroupHost {
	return cgroupHost{mount: "/cg", self: "/self", mounts: "/mounts", fs: f, euid: func() int { return 1000 }}
}

// nsdelegateLine is a mountinfo line of cgroup2 at the fake host's /cg, mounted nsdelegate, as
// systemd mounts it.
const nsdelegateLine = "30 23 0:26 / /cg rw,nosuid,nodev,noexec,relatime shared:4 - cgroup2 cgroup2 rw,nsdelegate,memory_recursiveprot\n"

// TestContainedIsRefusedWhereCgroup2HasNoNsdelegate: on a host whose every delegation check passes,
// a Contained is refused with errNoContained, errNoNsDelegate, ErrRefused and
// errors.ErrUnsupported, and nothing is written to the host, unless the last mountinfo line at the
// cgroup v2 mount is of cgroup2 with nsdelegate among its superblock's options: no mountinfo, a
// cgroup2 mount without it, nsdelegate in the mount's own options alone, nsdelegate on another
// mount point, a tmpfs mounted over the cgroup2 one, an option nsdelegatex, and a line cut short
// are each refused. A line with no optional fields, and an nsdelegate mount over a plain one, pass.
//
// What it costs when this fails: a Contained run starts in a cgroup namespace on a host whose
// kernel lets a process of it move out of the run's cgroup and write its memory.max, as the caller
// was told it could not; or a host that holds it is refused.
func TestContainedIsRefusedWhereCgroup2HasNoNsdelegate(t *testing.T) {
	t.Parallel()
	const plain = "30 23 0:26 / /cg rw,nosuid shared:4 - cgroup2 cgroup2 rw,memory_recursiveprot\n"
	for _, r := range []struct {
		name, mounts string
		held         bool
	}{
		{"no mountinfo", "", false},
		{"a cgroup2 mount without nsdelegate", plain, false},
		{"nsdelegate in the mount's own options alone", "30 23 0:26 / /cg rw,nsdelegate shared:4 - cgroup2 cgroup2 rw\n", false},
		{"nsdelegate on another mount point", strings.Replace(nsdelegateLine, " /cg ", " /other ", 1) + plain, false},
		{"a tmpfs over the cgroup2 mount", nsdelegateLine + "41 30 0:50 / /cg rw - tmpfs tmpfs rw\n", false},
		{"an option nsdelegatex", strings.Replace(nsdelegateLine, "nsdelegate", "nsdelegatex", 1), false},
		{"a line cut short", "30 23 0:26 / /cg rw - cgroup2 cgroup2\n", false},
		{"nsdelegate as systemd mounts it", nsdelegateLine, true},
		{"no optional fields", "30 23 0:26 / /cg rw - cgroup2 cgroup2 rw,nsdelegate\n", true},
		{"an nsdelegate mount over a plain one", plain + nsdelegateLine, true},
	} {
		f := goodCgroups(false)
		if r.mounts != "" {
			f.files["/mounts"] = r.mounts
		}
		err := fakeHost(f).nsDelegated()
		refused := errors.Is(err, errNoContained) && errors.Is(err, errNoNsDelegate) && errors.Is(err, ErrRefused) && errors.Is(err, errors.ErrUnsupported)
		if (r.held && err != nil) || (!r.held && !refused) {
			t.Errorf("%s: %v, want held %v", r.name, err, r.held)
		}
	}
	f := goodCgroups(false)
	f.files["/mounts"] = plain
	mark := filepath.Join(t.TempDir(), "started")
	c := helperCmd(t, "touch", mark)
	c.Limits, c.seam = Limits{TreeMemory: treeLimit, Contained: true}, &seams{host: fakeHost(f)}
	res, err := Run(t.Context(), c)
	if !errors.Is(err, errNoNsDelegate) || !errors.Is(err, ErrRefused) || !errors.Is(err, errors.ErrUnsupported) || res.Code != -1 {
		t.Errorf("a Contained run with no nsdelegate: %v, Code %d, want errNoNsDelegate, ErrRefused, ErrUnsupported and Code -1", err, res.Code)
	}
	if _, err := os.Stat(mark); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a Contained run with no nsdelegate: the program ran (%v), want nothing started", err)
	}
	if len(f.wrote) != 0 {
		t.Errorf("a Contained run with no nsdelegate: wrote %q to the host, want nothing, as the check of the mount comes first", f.wrote)
	}
}

// TestContainedIsRefusedToARootCaller: on a host whose every delegation check passes and whose
// cgroup v2 is mounted nsdelegate, a Contained run by a caller whose effective uid is 0, forced
// through the seam, is refused with errNoContained, errRootCaller, ErrRefused and
// errors.ErrUnsupported, and Code -1, before anything is written to the host or started. A caller
// of uid 1 or 65534 passes the check, and this process passes it where it is not root.
//
// What it costs when this fails: a root caller's tree is uid 0 in the initial user namespace, which
// writes kernel.core_pattern with no capability and under no_new_privs, crashes, and has the kernel
// run its helper as root outside every namespace of the tree, to lift the run's memory.max or kill
// the caller; the caller was told its tree held no capability.
func TestContainedIsRefusedToARootCaller(t *testing.T) {
	t.Parallel()
	for _, uid := range []int{0, 1, 65534} {
		h := fakeHost(goodCgroups(false))
		h.euid = func() int { return uid }
		err := h.notRoot()
		refused := errors.Is(err, errNoContained) && errors.Is(err, errRootCaller) && errors.Is(err, ErrRefused) && errors.Is(err, errors.ErrUnsupported)
		if (uid == 0) != refused || (uid != 0 && err != nil) {
			t.Errorf("a caller of euid %d: %v, want refused %v", uid, err, uid == 0)
		}
	}
	if err := (cgroupHost{}).notRoot(); (os.Geteuid() == 0) != (err != nil) {
		t.Errorf("this process, of euid %d: %v", os.Geteuid(), err)
	}
	f := goodCgroups(false)
	f.files["/mounts"] = nsdelegateLine
	h := fakeHost(f)
	h.euid = func() int { return 0 }
	mark := filepath.Join(t.TempDir(), "started")
	c := helperCmd(t, "touch", mark)
	c.Limits, c.seam = Limits{TreeMemory: treeLimit, Contained: true}, &seams{host: h}
	res, err := Run(t.Context(), c)
	if !errors.Is(err, errRootCaller) || !errors.Is(err, errNoContained) || !errors.Is(err, ErrRefused) || !errors.Is(err, errors.ErrUnsupported) || res.Code != -1 {
		t.Errorf("a Contained run by root: %v, Code %d, want errRootCaller, errNoContained, ErrRefused, ErrUnsupported and Code -1", err, res.Code)
	}
	if _, err := os.Stat(mark); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a Contained run by root: the program ran (%v), want nothing started", err)
	}
	if len(f.wrote) != 0 {
		t.Errorf("a Contained run by root: wrote %q to the host, want nothing, as the check of the caller comes first", f.wrote)
	}
}

// TestContainedIsRefusedWithTerminal: a Contained run held to 64 MiB with Terminal is refused with
// errContainedTerminal, wrapping ErrRefused, and Code -1, before anything is written to a host whose
// every check passes or started; a TreeMemory run with Terminal is not refused by it.
//
// What it costs when this fails: the tree shares the caller's controlling terminal, and by TIOCSTI
// types an INTR that signals the caller's group past every namespace of the tree, so its Timeout and
// its kill do not run, or a line the user's shell runs as the user outside the tree once the caller
// exits.
func TestContainedIsRefusedWithTerminal(t *testing.T) {
	t.Parallel()
	f := goodCgroups(false)
	f.files["/mounts"] = nsdelegateLine
	mark := filepath.Join(t.TempDir(), "started")
	c := helperCmd(t, "touch", mark)
	c.Terminal = true
	c.Limits, c.seam = Limits{TreeMemory: treeLimit, Contained: true}, &seams{host: fakeHost(f)}
	res, err := Run(t.Context(), c)
	if !errors.Is(err, errContainedTerminal) || !errors.Is(err, ErrRefused) || res.Code != -1 {
		t.Errorf("a Contained run with Terminal: %v, Code %d, want errContainedTerminal, ErrRefused and Code -1", err, res.Code)
	}
	if _, err := os.Stat(mark); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a Contained run with Terminal: the program ran (%v), want nothing started", err)
	}
	if len(f.wrote) != 0 {
		t.Errorf("a Contained run with Terminal: wrote %q to the host, want nothing", f.wrote)
	}
	c.Limits.Contained = false
	if _, err := Run(t.Context(), c); errors.Is(err, errContainedTerminal) {
		t.Errorf("a TreeMemory run with Terminal: %v, want no errContainedTerminal", err)
	}
}

// TestTheCgroupCheckFindsTheDelegatedParentAndGivesItMemory: on a host whose every check passes, the
// run's cgroup goes in the parent of the caller's cgroup; the parent's children are given memory by
// "+memory" in its cgroup.subtree_control when they are not yet, and nothing is written when they are.
//
// What it costs when this fails: every run on a delegated host is refused; or a run's cgroup is made
// where its memory.max holds nothing, as its parent gives it no memory controller.
func TestTheCgroupCheckFindsTheDelegatedParentAndGivesItMemory(t *testing.T) {
	t.Parallel()
	for _, given := range []bool{false, true} {
		f := goodCgroups(given)
		p, err := fakeHost(f).parent()
		if err != nil || p != fakeParent {
			t.Fatalf("memory given %v: %q, %v, want %q", given, p, err, fakeParent)
		}
		want := map[string]string{}
		if !given {
			want[fakeParent+"/cgroup.subtree_control"] = "+memory"
		}
		if len(f.wrote) != len(want) || f.wrote[fakeParent+"/cgroup.subtree_control"] != want[fakeParent+"/cgroup.subtree_control"] {
			t.Errorf("memory given %v: wrote %q, want %q", given, f.wrote, want)
		}
	}
}

// TestTheCgroupCheckRefusesEachHostItCannotHoldATreeOn: a host that fails one check of the delegation
// check is refused with that check's reason, with ErrRefused and errors.ErrUnsupported: no cgroup v2
// mount, no one clean cgroup v2 line, the root cgroup, no delegation mark or one of another value, no
// memory controller, the parent, its cgroup.procs or its cgroup.subtree_control not writable, no
// cgroup.kill, no memory.swap.max, and a subtree_control that will not take memory.
//
// What it costs when this fails: a run held to TreeMemory starts on a host that cannot hold it: a
// cgroup that is not the caller's to change, one whose memory.max holds nothing, one whose tree
// passes its limit into swap, or one left behind with processes in it.
func TestTheCgroupCheckRefusesEachHostItCannotHoldATreeOn(t *testing.T) {
	t.Parallel()
	for _, r := range hostRefusals() {
		f := goodCgroups(false)
		r.plant(f)
		p, err := fakeHost(f).parent()
		if !errors.Is(err, r.want) || !errors.Is(err, ErrRefused) || !errors.Is(err, errors.ErrUnsupported) || !errors.Is(err, ErrNotHeld) || p != "" {
			t.Errorf("%s: %q, %v, want %v, ErrRefused, ErrUnsupported and ErrNotHeld", r.name, p, err, r.want)
		}
	}
}

// hostRefusal is a host the cgroup check refuses: goodCgroups with plant's one check failed, and the
// reason.
type hostRefusal struct {
	name  string
	plant func(*fakeCgroups)
	want  error
}

// hostRefusals are the hosts the cgroup check refuses, one check failed in each.
func hostRefusals() []hostRefusal {
	return []hostRefusal{
		{"a cgroup v1 mount", func(f *fakeCgroups) { f.magicOf = unix.TMPFS_MAGIC }, errNoCgroup2},
		{"no file of this process's cgroup", func(f *fakeCgroups) { delete(f.files, "/self") }, errNoCgroupLine},
		{"only cgroup v1 lines", func(f *fakeCgroups) { f.files["/self"] = "4:memory:/app\n" }, errNoCgroupLine},
		{"two cgroup v2 lines", func(f *fakeCgroups) { f.files["/self"] += "0::/other\n" }, errNoCgroupLine},
		{"a relative path", func(f *fakeCgroups) { f.files["/self"] = "0::user.slice/app\n" }, errNoCgroupLine},
		{"a path with ..", func(f *fakeCgroups) { f.files["/self"] = "0::/user.slice/svc.service/x/../app\n" }, errNoCgroupLine},
		{"the root cgroup", func(f *fakeCgroups) { f.files["/self"] = "0::/\n" }, errCgroupRoot},
		{"no delegation mark", func(f *fakeCgroups) { clear(f.xattrs) }, errNotDelegated},
		{"a delegation mark of 0", func(f *fakeCgroups) { f.xattrs[fakeParent+"\x00"+delegateMark] = "0" }, errNotDelegated},
		{"a delegation mark of 1 and a newline", func(f *fakeCgroups) { f.xattrs[fakeParent+"\x00"+delegateMark] = "1\n" }, errNotDelegated},
		{"no memory controller", func(f *fakeCgroups) { f.files[fakeParent+"/cgroup.controllers"] = "cpu pids\n" }, errNoMemory},
		{"a controller named memoryx", func(f *fakeCgroups) { f.files[fakeParent+"/cgroup.controllers"] = "memoryx\n" }, errNoMemory},
		{"the parent not writable", func(f *fakeCgroups) { f.denied[fakeParent] = true }, errNotWritable},
		{"cgroup.procs not writable", func(f *fakeCgroups) { f.denied[fakeParent+"/cgroup.procs"] = true }, errNotWritable},
		{"cgroup.subtree_control not writable", func(f *fakeCgroups) { f.denied[fakeParent+"/cgroup.subtree_control"] = true }, errNotWritable},
		{"no cgroup.kill", func(f *fakeCgroups) { delete(f.files, fakeParent+"/cgroup.kill") }, errNoCgroupKill},
		{"no memory.swap.max", func(f *fakeCgroups) { delete(f.files, fakeParent+"/memory.swap.max") }, errNoSwapMax},
		{"a subtree_control that will not take memory", func(f *fakeCgroups) { f.writeErr = unix.EBUSY }, errSubtreeMemory},
	}
}

// TestHoldsChecksTheCgroupAsRunDoesAndWritesNothing: on the fake host, Holds of a run held to
// TreeMemory that reads its peak gives PeakTreeCharged and no refusal where every check passes, its
// children given memory or not, and each host refusal's reason with ErrNotHeld where one fails; it
// writes nothing to the host and starts nothing. Its one difference from Run: a subtree_control that
// would not take the write passes, as Holds makes none, and a parent that holds a process of its own,
// which keeps memory from its children, is refused with errSubtreeMemory. A run that only reads its
// peak gets PeakNone and no refusal where no cgroup is delegated; from within a run's cgroup it, and a
// TreeMemory, are refused with errInRun, ErrNotHeld; a Contained one by a root caller with
// errRootCaller and ErrNotHeld.
//
// What it costs when this fails: a caller told by Holds that a limit holds learns otherwise only after
// a run; or a probe changes the caller's delegated cgroup, which the caller asked only to read.
func TestHoldsChecksTheCgroupAsRunDoesAndWritesNothing(t *testing.T) {
	t.Parallel()
	held := func(f *fakeCgroups, h cgroupHost, set func(*Cmd)) (PeakKind, error, bool) {
		mark := filepath.Join(t.TempDir(), "started")
		c := helperCmd(t, "touch", mark)
		c.Limits, c.ReadPeak, c.seam = Limits{TreeMemory: treeLimit}, true, &seams{host: h}
		if set != nil {
			set(&c)
		}
		kind, err := Holds(c)
		if len(f.wrote) != 0 {
			t.Errorf("Holds wrote %q to the host, want nothing", f.wrote)
		}
		_, statErr := os.Stat(mark)
		return kind, err, statErr == nil
	}
	for _, given := range []bool{false, true} {
		f := goodCgroups(given)
		if kind, err, ran := held(f, fakeHost(f), nil); kind != PeakTreeCharged || err != nil || ran {
			t.Errorf("memory given %v: %q, %v, ran %v, want PeakTreeCharged, no refusal and nothing run", given, kind, err, ran)
		}
	}
	for _, r := range hostRefusals() {
		f := goodCgroups(false)
		r.plant(f)
		kind, err, ran := held(f, fakeHost(f), nil)
		if f.writeErr != nil {
			if err != nil || kind != PeakTreeCharged {
				t.Errorf("%s: %q, %v, want PeakTreeCharged and no refusal, as Holds writes nothing", r.name, kind, err)
			}
			continue
		}
		if !errors.Is(err, r.want) || !errors.Is(err, ErrNotHeld) || !errors.Is(err, ErrRefused) || kind != PeakNone || ran {
			t.Errorf("%s: %q, %v, ran %v, want %v, ErrNotHeld, ErrRefused, PeakNone and nothing run", r.name, kind, err, ran, r.want)
		}
	}
	f := goodCgroups(false)
	f.files[fakeParent+"/cgroup.procs"] = "4242\n"
	if kind, err, _ := held(f, fakeHost(f), nil); !errors.Is(err, errSubtreeMemory) || !errors.Is(err, errHeldProcess) || !errors.Is(err, ErrNotHeld) || kind != PeakNone {
		t.Errorf("a parent that holds a process: %q, %v, want errSubtreeMemory, errHeldProcess and ErrNotHeld", kind, err)
	}
	f = goodCgroups(false)
	clear(f.xattrs)
	if kind, err, _ := held(f, fakeHost(f), func(c *Cmd) { c.Limits = Limits{} }); kind != PeakNone || err != nil {
		t.Errorf("a ReadPeak run where no cgroup is delegated: %q, %v, want PeakNone and no refusal", kind, err)
	}
	for _, r := range []struct {
		name string
		set  func(*Cmd)
	}{{"TreeMemory", nil}, {"ReadPeak", func(c *Cmd) { c.Limits = Limits{} }}} {
		f := goodCgroups(false)
		f.files["/self"] = "0::/user.slice/svc.service/proc-0123456789abcdef\n"
		if kind, err, _ := held(f, fakeHost(f), r.set); !errors.Is(err, errInRun) || !errors.Is(err, ErrNotHeld) || !errors.Is(err, errors.ErrUnsupported) || kind != PeakNone {
			t.Errorf("%s in a run's cgroup: %q, %v, want errInRun, ErrNotHeld and ErrUnsupported", r.name, kind, err)
		}
	}
	f = goodCgroups(false)
	f.files["/mounts"] = nsdelegateLine
	h := fakeHost(f)
	h.euid = func() int { return 0 }
	if _, err, _ := held(f, h, func(c *Cmd) { c.Limits.Contained = true }); !errors.Is(err, errRootCaller) || !errors.Is(err, ErrNotHeld) {
		t.Errorf("a Contained run by root: %v, want errRootCaller and ErrNotHeld", err)
	}
}

// TestACgroupNamedBelowAProcThatIsNotAProcMountIsRefused: on a host whose every other check passes,
// a /proc that is a plain folder, or a link to the kernel's proc mount, refuses the check with
// errNoProcCgroup, ErrRefused and errors.ErrUnsupported, before the file that names this process's
// cgroup is read; a run held to TreeMemory there is refused, starts nothing and writes nothing to the
// host, and a run that only asks for its peak runs and reads none. The kernel's /proc passes.
//
// What it costs when this fails: in a root whose /proc is not the kernel's, a planted self/cgroup
// places the run's cgroup, and with it what holds the tree's TreeMemory.
func TestACgroupNamedBelowAProcThatIsNotAProcMountIsRefused(t *testing.T) {
	t.Parallel()
	link := filepath.Join(t.TempDir(), "proc")
	if err := os.Symlink(procMount, link); err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct {
		name, proc string
		refused    bool
	}{
		{"a plain folder", t.TempDir(), true},
		{"a link to the kernel's proc mount", link, true},
		{"the kernel's proc mount", procMount, false},
	} {
		f := goodCgroups(true)
		h := fakeHost(f)
		h.proc = r.proc
		p, err := h.parent()
		switch {
		case r.refused && (!errors.Is(err, errNoProcCgroup) || !errors.Is(err, ErrRefused) || !errors.Is(err, errors.ErrUnsupported) || p != ""):
			t.Errorf("%s: %q, %v, want errNoProcCgroup, ErrRefused and ErrUnsupported", r.name, p, err)
		case !r.refused && (err != nil || p != fakeParent):
			t.Errorf("%s: %q, %v, want %s and no error", r.name, p, err, fakeParent)
		}
		if !r.refused {
			continue
		}
		if slices.Contains(f.reads, "/self") {
			t.Errorf("%s: the check read the file that names this process's cgroup (%q), want it refused first", r.name, f.reads)
		}
		mark := filepath.Join(t.TempDir(), "started")
		c := helperCmd(t, "touch", mark)
		c.Limits, c.seam = Limits{TreeMemory: treeLimit}, &seams{host: h}
		res, err := Run(t.Context(), c)
		if !errors.Is(err, errNoProcCgroup) || !errors.Is(err, ErrRefused) || !errors.Is(err, errors.ErrUnsupported) || res.Code != -1 {
			t.Errorf("%s: TreeMemory: %v, Code %d, want errNoProcCgroup, ErrRefused, ErrUnsupported and Code -1", r.name, err, res.Code)
		}
		if _, err := os.Stat(mark); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: TreeMemory: the program ran (%v), want nothing started", r.name, err)
		}
		if len(f.wrote) != 0 {
			t.Errorf("%s: TreeMemory: wrote %q to the host, want nothing", r.name, f.wrote)
		}
		c = helperCmd(t, "touch", mark)
		c.ReadPeak, c.seam = true, &seams{host: h}
		res, err = Run(t.Context(), c)
		if err != nil || res.Code != 0 || res.PeakRead || res.Peak != 0 {
			t.Errorf("%s: ReadPeak alone: %v, Code %d, Peak %d, PeakRead %v, want exit 0, 0 and false", r.name, err, res.Code, res.Peak, res.PeakRead)
		}
	}
}

// TestTreeMemoryWithNoDelegationIsRefusedAndStartsNothing: a run held to TreeMemory on a host whose
// parent cgroup is not delegated is refused with errNotDelegated, ErrRefused and
// errors.ErrUnsupported, Code -1, and its program, which would make a file if run, makes none; and
// nothing is written to the host. A run on that host that only asks for its peak runs, and reads none.
//
// What it costs when this fails: a run asked to be bounded starts unbounded where nothing is
// delegated, or a run that asked only for a reading is refused for it.
func TestTreeMemoryWithNoDelegationIsRefusedAndStartsNothing(t *testing.T) {
	t.Parallel()
	f := goodCgroups(false)
	clear(f.xattrs)
	mark := filepath.Join(t.TempDir(), "started")
	c := helperCmd(t, "touch", mark)
	c.Limits, c.seam = Limits{TreeMemory: treeLimit}, &seams{host: fakeHost(f)}
	res, err := Run(t.Context(), c)
	if !errors.Is(err, errNotDelegated) || !errors.Is(err, ErrRefused) || !errors.Is(err, errors.ErrUnsupported) || res.Code != -1 {
		t.Errorf("TreeMemory with no delegation: %v, Code %d, want errNotDelegated, ErrRefused, ErrUnsupported and Code -1", err, res.Code)
	}
	if _, err := os.Stat(mark); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("TreeMemory with no delegation: the program ran (%v), want nothing started", err)
	}
	if len(f.wrote) != 0 {
		t.Errorf("TreeMemory with no delegation: wrote %q to the host, want nothing", f.wrote)
	}
	c = helperCmd(t, "touch", mark)
	c.ReadPeak, c.seam = true, &seams{host: fakeHost(f)}
	res, err = Run(t.Context(), c)
	if err != nil || res.Code != 0 || res.PeakRead || res.Peak != 0 {
		t.Errorf("ReadPeak alone with no delegation: %v, Code %d, Peak %d, PeakRead %v, want exit 0, 0 and false", err, res.Code, res.Peak, res.PeakRead)
	}
}

// cgroupLines is the "word value" lines of a cgroup helper's stdout, by word.
func cgroupLines(out []byte) map[string]string {
	got := map[string]string{}
	for line := range strings.Lines(string(out)) {
		if k, v, ok := strings.Cut(strings.TrimSuffix(line, "\n"), " "); ok {
			got[k] = v
		}
	}
	return got
}

// cgroupGone fails the test unless the run's cgroup, named in its helper's stdout, is gone.
func cgroupGone(t *testing.T, what string, out []byte) {
	t.Helper()
	rel := cgroupLines(out)["cgroup"]
	if rel == "" {
		t.Errorf("%s: the helper named no cgroup in %q", what, out)
		return
	}
	if _, err := os.Stat(filepath.Join(cgroupMount, rel)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s: the run's cgroup %s is still there (%v), want it removed", what, rel, err)
	}
}

// TestATreeMemoryRunsInACgroupOfItsOwnHeldToIt: a helper run with a TreeMemory of 96 MiB finds
// itself in a cgroup of its own, beside the caller's in the delegated parent, whose memory.max is
// those 96 MiB in bytes, memory.swap.max 0 and memory.oom.group 1; and the cgroup is gone once the
// run returns. Where no cgroup is delegated, the run is refused and starts nothing.
//
// What it costs when this fails: a run held to TreeMemory runs unbounded, or bounded to some other
// figure, or passes its bound into swap, or is ended in part; or every run leaves a cgroup behind.
func TestATreeMemoryRunsInACgroupOfItsOwnHeldToIt(t *testing.T) {
	t.Parallel()
	const what = "a helper held to 96 MiB"
	p, why := delegated(t)
	c := helperCmd(t, "cgroup-self", "exit")
	c.Limits = Limits{TreeMemory: 96 << 20}
	res, err := Run(t.Context(), c)
	if why != nil {
		refusedHere(t, what, res, err)
		return
	}
	if err != nil || res.Code != 0 {
		t.Fatalf("%s: %v, Code %d, stderr %q", what, err, res.Code, res.Stderr)
	}
	got := cgroupLines(res.Stdout)
	rel := got["cgroup"]
	if filepath.Join(cgroupMount, filepath.Dir(rel)) != p || !strings.HasPrefix(filepath.Base(rel), "proc-") {
		t.Errorf("%s ran in %q, want a proc- cgroup in %s", what, rel, p)
	}
	for k, v := range map[string]string{"max": strconv.Itoa(96 << 20), "swap": "0", "group": "1"} {
		if got[k] != v {
			t.Errorf("%s: its cgroup's %s is %q, want %q", what, k, got[k], v)
		}
	}
	cgroupGone(t, what, res.Stdout)
}

// TestATreeThatPassesTreeMemoryIsKilledWithWhyLimit: a helper that holds little itself and starts a
// child that allocates 256 MiB, held to 96 MiB, ends with WhyLimit and Code -1, its child never
// writing done; the same helper whose child allocates 16 MiB exits 0 with done and no WhyLimit. The
// run's cgroup is gone after each. Where no cgroup is delegated, each run is refused and starts
// nothing.
//
// What it costs when this fails: a tree held to TreeMemory passes it in a child, as only its program
// was held; or a tree killed for its limit reads as a program that crashed by itself; or a cgroup the
// kernel killed is left behind.
func TestATreeThatPassesTreeMemoryIsKilledWithWhyLimit(t *testing.T) {
	t.Parallel()
	_, why := delegated(t)
	for _, r := range []struct {
		mib  string
		over bool
	}{{"256", true}, {"16", false}} {
		what := "a child of " + r.mib + " MiB held to 96 MiB"
		c := helperCmd(t, "spawn-hog", r.mib)
		c.Limits = Limits{TreeMemory: 96 << 20}
		res, err := Run(t.Context(), c)
		done := bytes.Contains(res.Stdout, []byte("done"))
		switch {
		case why != nil:
			refusedHere(t, what, res, err)
			continue
		case r.over && (!endedWith(err, WhyLimit) || res.Code != -1 || done):
			t.Errorf("%s: %v, Code %d, stdout %q, stderr %q, want WhyLimit, Code -1 and no done", what, err, res.Code, res.Stdout, res.Stderr)
		case !r.over && (err != nil || res.Code != 0 || !done):
			t.Errorf("%s: %v, Code %d, stdout %q, stderr %q, want exit 0 and done", what, err, res.Code, res.Stdout, res.Stderr)
		}
		cgroupGone(t, what, res.Stdout)
	}
}

// TestARunsCgroupIsGoneAfterItsDeadline: a helper held to TreeMemory that holds past its Timeout of a
// second ends with WhyDeadline and Code -1, and its cgroup is gone once the run returns. Where no
// cgroup is delegated, the run is refused and starts nothing.
//
// What it costs when this fails: each run the deadline ends leaves a cgroup behind in the caller's
// delegated parent, until the parent holds no more.
func TestARunsCgroupIsGoneAfterItsDeadline(t *testing.T) {
	t.Parallel()
	const what = "a held helper past its Timeout"
	_, why := delegated(t)
	c := helperCmd(t, "cgroup-self", "hold")
	c.Limits, c.Timeout = Limits{TreeMemory: treeLimit}, time.Second
	res, err := Run(t.Context(), c)
	if why != nil {
		refusedHere(t, what, res, err)
		return
	}
	if !endedWith(err, WhyDeadline) || res.Code != -1 {
		t.Errorf("%s: %v, Code %d, stderr %q, want WhyDeadline and Code -1", what, err, res.Code, res.Stderr)
	}
	cgroupGone(t, what, res.Stdout)
}

// TestAPeakCountsAGrandchild: a helper whose child starts a hog of 64 MiB, each page written, and
// waits for it, run with ReadPeak, reads a Peak of 64 MiB or more with PeakRead true, where a cgroup
// is delegated and its kernel gives memory.peak; elsewhere it runs, and reads none.
//
// What it costs when this fails: a program that does its work two processes down, as a shell under a
// build driver does, reads a peak short of what its tree held, and the caller sizes by it.
func TestAPeakCountsAGrandchild(t *testing.T) {
	t.Parallel()
	const what = "a grandchild of 64 MiB"
	reads := peakHere(t)
	c := helperCmd(t, "deep-hog", "64")
	c.ReadPeak = true
	res, err := Run(t.Context(), c)
	if err != nil || res.Code != 0 {
		t.Fatalf("%s: %v, Code %d, stderr %q", what, err, res.Code, res.Stderr)
	}
	switch {
	case !reads && (res.PeakRead || res.Peak != 0):
		t.Errorf("%s where no peak is read: Peak %d, PeakRead %v, want 0 and false", what, res.Peak, res.PeakRead)
	case reads && (!res.PeakRead || res.Peak < 64<<20):
		t.Errorf("%s: Peak %d bytes, PeakRead %v, want %d or more and true", what, res.Peak, res.PeakRead, 64<<20)
	}
}

// fallen is the map, in MiB, of TestAPeakIsWhatTheTreeHeldAtMostNotAtItsEnd: well above what the
// helper and its cached pages hold once the map is gone.
const fallen = 64

// TestAPeakIsWhatTheTreeHeldAtMostNotAtItsEnd: a helper that maps fallen MiB, writes each page, and
// unmaps it, and then reads its own cgroup's memory.current, run with ReadPeak, reads a Peak of
// fallen MiB or more with PeakRead true, where a cgroup is delegated and its kernel gives
// memory.peak, while the memory.current the helper read is less; elsewhere it runs, and reads
// none.
//
// What it costs when this fails: a peak read as what the tree holds at its end, its memory.current,
// passes for one when the tree's memory never falls, and reads short of the most it held when it
// does: the caller sizes the next run below what this one needed.
func TestAPeakIsWhatTheTreeHeldAtMostNotAtItsEnd(t *testing.T) {
	t.Parallel()
	what := fmt.Sprintf("a helper that held %d MiB and gave it back", fallen)
	reads := peakHere(t)
	c := helperCmd(t, "peak-then-fall", strconv.Itoa(fallen))
	c.ReadPeak = true
	res, err := Run(t.Context(), c)
	if err != nil || res.Code != 0 {
		t.Fatalf("%s: %v, Code %d, stderr %q", what, err, res.Code, res.Stderr)
	}
	if !reads {
		if res.PeakRead || res.Peak != 0 {
			t.Errorf("%s where no peak is read: Peak %d, PeakRead %v, want 0 and false", what, res.Peak, res.PeakRead)
		}
		return
	}
	current, cerr := strconv.ParseInt(cgroupLines(res.Stdout)["current"], 10, 64)
	if cerr != nil {
		t.Fatalf("%s: the helper read no memory.current in %q: %v", what, res.Stdout, cerr)
	}
	if current >= fallen<<20 {
		t.Fatalf("%s: memory.current was %d bytes once it gave its map back, not below its %d, so the row cannot tell a peak from it", what, current, fallen<<20)
	}
	if !res.PeakRead || res.Peak < fallen<<20 {
		t.Errorf("%s: Peak %d bytes, PeakRead %v, want %d or more and true, above the %d bytes memory.current held at the end", what, res.Peak, res.PeakRead, fallen<<20, current)
	}
}

// TestAProcessThatLeftTheTreeIsEndedWithTheRunsCgroup: a helper held to TreeMemory that starts a
// process in a session of its own, with none of its output, and exits 0, returns no error and Code 0;
// once the run returns, that process runs no more and the run's cgroup is gone. A signal to the
// tree's group does not reach a process in another session; cgroup.kill does. Where no cgroup is
// delegated, the run is refused and starts nothing.
//
// What it costs when this fails: a process that left a run held to TreeMemory runs on past it,
// outside its limit's watch once its cgroup is gone, or holds the cgroup so it is never removed.
func TestAProcessThatLeftTheTreeIsEndedWithTheRunsCgroup(t *testing.T) {
	t.Parallel()
	const what = "a helper held to 64 MiB whose child left its session"
	_, why := delegated(t)
	c := helperCmd(t, "cgroup-escape")
	c.Limits = Limits{TreeMemory: treeLimit}
	res, err := Run(t.Context(), c)
	if why != nil {
		refusedHere(t, what, res, err)
		return
	}
	if err != nil || res.Code != 0 {
		t.Errorf("%s: %v, Code %d, stderr %q, want exit 0", what, err, res.Code, res.Stderr)
	}
	pid, perr := strconv.Atoi(cgroupLines(res.Stdout)["escaped"])
	if perr != nil {
		t.Fatalf("%s: the helper named no process in %q: %v", what, res.Stdout, perr)
	}
	// cgroup.kill has sent its SIGKILL once the cgroup is removed, but a process leaves its cgroup
	// on its way out, before it is a zombie, so its end is waited for, up to leftAlone.
	end, eerr := processEnd(pid)
	if eerr != nil {
		t.Fatalf("%s: %v", what, eerr)
	}
	if runsOn(end) {
		t.Errorf("%s: process %d still runs %v after the run returned, want it ended with the run", what, pid, leftAlone)
	}
	cgroupGone(t, what, res.Stdout)
}

// TestAProcessThatMovesItselfToAnotherCgroupIsOutOfTreeMemory: a helper held to a TreeMemory of 64
// MiB that writes its pid to the cgroup.procs of this process's cgroup, beside the run's under the
// delegated one, and then holds 128 MiB, each page written, runs to its end: the run returns no
// error and Code 0, not `limit`, and the helper's cgroup after the move is this process's. This is
// the limit limits.go and the README state for Linux without Contained: TreeMemory holds a program
// that does not move itself to another cgroup (TestAContainedTreeCannotLeaveOrLiftTreeMemory holds
// one that does). Where no cgroup is delegated, the run is refused and starts nothing.
//
// What it costs when this fails: the docs say less than the code does, or more. Ended `limit`, the
// move is held without Contained, and the docs and this row are out of date; a helper that could not
// move, the delegated cgroup is not as the docs say a caller's is.
func TestAProcessThatMovesItselfToAnotherCgroupIsOutOfTreeMemory(t *testing.T) {
	t.Parallel()
	const what = "a helper held to 64 MiB that moved itself to this process's cgroup and held 128 MiB"
	_, why := delegated(t)
	here, serr := selfPath()
	if serr != nil && why == nil {
		t.Fatalf("%s: %v", what, serr)
	}
	c := helperCmd(t, "cgroup-move", here, "128")
	c.Limits = Limits{TreeMemory: treeLimit}
	res, err := Run(t.Context(), c)
	if why != nil {
		refusedHere(t, what, res, err)
		return
	}
	if err != nil || res.Code != 0 {
		t.Errorf("%s: %v, Code %d, stderr %q, want it run to its end, Code 0, as the docs say a moved process is out of TreeMemory", what, err, res.Code, res.Stderr)
	}
	got := cgroupLines(res.Stdout)
	if got["cgroup"] != here {
		t.Errorf("%s: the helper's cgroup after the move is %q, want this process's, %q", what, got["cgroup"], here)
	}
	if !strings.Contains(string(res.Stdout), "\ndone\n") {
		t.Errorf("%s: the helper did not write done, so it did not hold 128 MiB: stdout %q", what, res.Stdout)
	}
}

// TestAProcessThatWritesItsOwnCgroupsMemoryMaxIsOutOfTreeMemory: a helper held to a TreeMemory of 64
// MiB that writes max to its own cgroup's memory.max, the run's, and then holds 128 MiB, each page
// written, runs to its end: the run returns no error and Code 0, not `limit`. This is the limit
// limits.go and the README state for Linux without Contained: TreeMemory holds a program that does
// not change its own cgroup, by a move or by a write to the cgroup's files, which are its user's as
// the run's are (TestAContainedTreeCannotLeaveOrLiftTreeMemory holds one that does). Where no cgroup
// is delegated, the run is refused and starts nothing.
//
// What it costs when this fails: the docs say less than the code does, or more. Ended `limit`, the
// write is held without Contained, and the docs and this row are out of date.
func TestAProcessThatWritesItsOwnCgroupsMemoryMaxIsOutOfTreeMemory(t *testing.T) {
	t.Parallel()
	const what = "a helper held to 64 MiB that wrote max to its cgroup's memory.max and held 128 MiB"
	_, why := delegated(t)
	c := helperCmd(t, "cgroup-unlimit", "128")
	c.Limits = Limits{TreeMemory: treeLimit}
	res, err := Run(t.Context(), c)
	if why != nil {
		refusedHere(t, what, res, err)
		return
	}
	if err != nil || res.Code != 0 {
		t.Errorf("%s: %v, Code %d, stderr %q, want it run to its end, Code 0, as the docs say a program that writes its cgroup's files is out of TreeMemory", what, err, res.Code, res.Stderr)
	}
	if !strings.Contains(string(res.Stdout), "\ndone\n") {
		t.Errorf("%s: the helper did not write done, so it did not hold 128 MiB: stdout %q", what, res.Stdout)
	}
	cgroupGone(t, what, res.Stdout)
}

// containedHere is whether a Contained run here is held: the caller is not root, a cgroup is
// delegated, and cgroup v2 is mounted nsdelegate. Where it is not, the test proves the refusal in
// place of the hold, saying so in its log, and what proves it: the reason a refusal of Contained
// names, or nil for a refusal of TreeMemory, which any refusal by ErrRefused and ErrUnsupported is.
func containedHere(t *testing.T) (held bool, want error) {
	t.Helper()
	if err := (cgroupHost{}).notRoot(); err != nil {
		t.Logf("this process runs as root, so the row proves the refusal: %v", err)
		return false, errRootCaller
	}
	if _, err := delegated(t); err != nil {
		return false, nil
	}
	if err := (cgroupHost{}).nsDelegated(); err != nil {
		t.Logf("cgroup v2 has no nsdelegate here, so the row proves the refusal: %v", err)
		return false, errNoNsDelegate
	}
	return true, nil
}

// containedRun runs the contained helper with try, held to a TreeMemory of 64 MiB and Contained, as
// containedCmd makes it, by runContained.
func containedRun(t *testing.T, what, try, mib, how, to string) (Result, bool, error) {
	t.Helper()
	return runContained(t, what, containedCmd(t, what, try, mib, how, to))
}

// copyLimit is treeLimit for a tree that holds a copy of the test binary beside its helper, as the
// caps helper's does by its exec of itself: under -race each is a race binary with its own footprint, so
// it holds raceMemory once more, 64 MiB and two of them.
const copyLimit = treeLimit + raceMemory

// containedCmd is the contained helper with try, held to a TreeMemory of 64 MiB (copyLimit for caps,
// which runs a copy of the test binary beside it) and Contained, its
// hog of mib MiB, then exiting or holding by how; its move goes to this process's cgroup, and its
// write to a sibling to cgroup v2 path to. A helper that holds has a Timeout of exitsWithin, which a
// kill of its tree on its limit beats.
func containedCmd(t *testing.T, what, try, mib, how, to string) Cmd {
	t.Helper()
	held, _ := containedHere(t)
	here, err := selfPath()
	if err != nil && held {
		t.Fatalf("%s: %v", what, err)
	}
	if to == "" {
		to = here
	}
	c := helperCmd(t, "contained", filepath.Dir(here), to, try, mib, how)
	c.Limits = Limits{TreeMemory: treeLimit, Contained: true}
	if try == "caps" {
		c.Limits.TreeMemory = copyLimit
	}
	if how == "hold" {
		c.Timeout = exitsWithin
	}
	return c
}

// runContained runs c, a Contained run. It fails the test where the run is not held here and was not
// refused as containedHere says, and gives whether it was held.
func runContained(t *testing.T, what string, c Cmd) (Result, bool, error) {
	t.Helper()
	held, want := containedHere(t)
	res, err := Run(t.Context(), c)
	switch {
	case held:
	case want != nil && (!errors.Is(err, want) || res.Code != -1 || len(res.Stdout) != 0):
		t.Errorf("%s, refused here: %v, Code %d, stdout %q, want %v, Code -1 and no output", what, err, res.Code, res.Stdout, want)
	case want != nil:
	default:
		refusedHere(t, what, res, err)
	}
	return res, held, err
}

// TestAContainedProgramRunsAsTheCallersUidAndGid: a helper run Contained, held to 64 MiB, that
// tries nothing and starts a hog of 16 MiB, sees its own cgroup as /, its uid and gid as this
// process's effective ones, and runs to its end, survived, Code 0; and its run's cgroup, found
// beside this process's by its cgroup.procs, is gone once the run returns. Where no cgroup is
// delegated, or cgroup v2 has no nsdelegate, the run is refused and starts nothing.
//
// What it costs when this fails: a Contained run does not start in the namespaces the docs say, and
// the move and writes of the other Contained rows pass for some other reason; or its program runs
// as another user than the caller's, owning other files; or its cgroup is left behind.
func TestAContainedProgramRunsAsTheCallersUidAndGid(t *testing.T) {
	t.Parallel()
	const what = "a Contained helper held to 64 MiB with a hog of 16 MiB"
	res, held, err := containedRun(t, what, "none", "16", "exit", "")
	if !held {
		return
	}
	if err != nil || res.Code != 0 || !bytes.Contains(res.Stdout, []byte("survived\n")) {
		t.Errorf("%s: %v, Code %d, stdout %q, stderr %q, want exit 0 and survived", what, err, res.Code, res.Stdout, res.Stderr)
	}
	got := cgroupLines(res.Stdout)
	for k, v := range map[string]string{"cgroup": "/", "uid": strconv.Itoa(os.Geteuid()), "gid": strconv.Itoa(os.Getegid()), "tried": "<nil>"} {
		if got[k] != v {
			t.Errorf("%s: its %s is %q, want %q", what, k, got[k], v)
		}
	}
	runGone(t, what, got["run"])
}

// runGone fails the test unless the run's cgroup, the folder of inode ino a contained helper names,
// is gone from beside this process's cgroup, as runGoneFrom finds.
func runGone(t *testing.T, what, ino string) {
	t.Helper()
	here, err := selfPath()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	runGoneFrom(t, what, filepath.Dir(here), ino)
}

// runGoneFrom fails the test unless ino, the inode a contained helper names as its run's cgroup's,
// is a number, and no proc- cgroup in cgroup v2 path parent has it: kernfs does not use one again.
func runGoneFrom(t *testing.T, what, parent, ino string) {
	t.Helper()
	n, err := strconv.ParseUint(ino, 10, 64)
	if err != nil || n == 0 {
		t.Errorf("%s: the helper named %q as its run's cgroup's inode, want a number", what, ino)
		return
	}
	entries, err := os.ReadDir(filepath.Join(cgroupMount, parent))
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	for _, e := range entries {
		var st syscall.Stat_t
		if strings.HasPrefix(e.Name(), runPrefix) && syscall.Stat(filepath.Join(cgroupMount, parent, e.Name()), &st) == nil && st.Ino == n {
			t.Errorf("%s: the run's cgroup %s is still there, want it removed", what, filepath.Join(parent, e.Name()))
		}
	}
}

// TestAContainedTreeCannotLeaveOrLiftTreeMemory: a helper run Contained, held to 64 MiB, that tries
// to leave or lift the limit, then starts a hog of 128 MiB and, once the hog is gone, holds, ends
// with WhyLimit, not WhyDeadline, and Code -1, the hog never writing done, and its run's cgroup is
// gone after. Its write of its pid to this process's cgroup's cgroup.procs, the move
// TestAProcessThatMovesItselfToAnotherCgroupIsOutOfTreeMemory makes, and its write there of the pid
// of another process of the tree, are ENOENT: the cgroup v2 the shim mounted is rooted at the run's
// cgroup, so no path of the tree's names this process's. The kernel refuses, by EPERM,
// its writes of max to its run's memory.max, the write
// TestAProcessThatWritesItsOwnCgroupsMemoryMaxIsOutOfTreeMemory makes, and to its memory.swap.max,
// and of 1 to its run's cgroup.kill and cgroup.freeze, by nsdelegate, as the run's is the root of
// the tree's cgroup namespace; the kernel
// kills the tree whole, so the helper never writes survived. It lets the write of 0 to
// memory.oom.group through, which the run reads back: the run's check, woken by the
// hog's oom, ends the tree with errLimitLost naming memory.oom.group and the 0 it reads, not
// WhyLimit and not WhyDeadline, Code -1, before the Timeout, and the hog never writes done. Whether
// the helper writes survived first is not the row's: the kernel's kill of the hog alone and the
// run's check race. Where no cgroup is delegated, or cgroup v2 has no nsdelegate, each run is
// refused and starts nothing.
//
// What it costs when this fails: a Contained tree leaves its limit by a move, lifts it by a write,
// freezes or kills its own cgroup, or has the kernel kill its hog alone and runs on past its limit,
// as a run that is not Contained may; the caller was told it could not. Or a write to the run's own
// files is refused for a reason not the kernel's, a wrong path or a file's mode, and the row passes
// for it.
func TestAContainedTreeCannotLeaveOrLiftTreeMemory(t *testing.T) {
	t.Parallel()
	for _, r := range []struct {
		try   string
		errno syscall.Errno
	}{{"move", syscall.ENOENT}, {"other", syscall.ENOENT}, {"max", syscall.EPERM}, {"swap", syscall.EPERM}, {"kill", syscall.EPERM}, {"freeze", syscall.EPERM}, {"group", 0}} {
		what := "a Contained helper held to 64 MiB that tried " + r.try + " and started a hog of 128 MiB"
		res, held, err := containedRun(t, what, r.try, "128", "hold", "")
		if !held {
			continue
		}
		got := cgroupLines(res.Stdout)
		if tried := got["tried"]; tried == "" || (tried == "<nil>") != (r.errno == 0) || got["errno"] != strconv.Itoa(int(r.errno)) {
			t.Errorf("%s: its try gave %q, errno %q, want errno %d (%v): stdout %q, stderr %q", what, tried, got["errno"], int(r.errno), r.errno, res.Stdout, res.Stderr)
		}
		survived := bytes.Contains(res.Stdout, []byte("survived\n"))
		ended := endedWith(err, WhyLimit)
		if r.try == "group" {
			// a limit the tree lifted by its write, found by the read-back: never WhyLimit, which says the limit killed it
			ended = errors.Is(err, errLimitLost) && !endedWith(err, WhyLimit) && strings.Contains(fmt.Sprint(err), `memory.oom.group reads "0"`)
		}
		if !ended || errors.Is(err, errDeadline) || res.Code != -1 || bytes.Contains(res.Stdout, []byte("done\n")) || (r.errno != 0 && survived) {
			t.Errorf("%s: %v, Code %d, stdout %q, stderr %q, want WhyLimit (errLimitLost naming memory.oom.group for group) and no WhyDeadline, Code -1, no done, and survived only after a try of group", what, err, res.Code, res.Stdout, res.Stderr)
		}
		runGone(t, what, got["run"])
	}
}

// TestAContainedTreeCannotStopItsCallerThenLiftTreeMemory: a caller, this test binary started in a
// cgroup of its own beneath a delegated cgroup this test makes, runs a helper Contained, held to
// 64 MiB, that writes 1 to its caller's cgroup.freeze (freeze) or cgroup.kill (kill), then -memory
// to its run's parent's cgroup.subtree_control, then starts a hog of 128 MiB and holds. Each write
// is ENOENT, as the cgroup v2 the shim mounted is rooted at the run's cgroup, so no path of the
// tree's names the caller's or the parent's; the caller is never frozen and exits 0,
// and its run ends with WhyLimit, Code -1, the hog never writing done, and its run's cgroup gone
// after. The delegated cgroup is the test's own, so the row writes -memory to no cgroup another row
// runs in. Where no cgroup is delegated, or cgroup v2 has no nsdelegate, each run is refused and
// starts nothing.
//
// What it costs when this fails: a Contained tree freezes or kills its caller, whose check and kill
// on a lost limit then never run, and takes the memory controller from its own cgroup: it runs on
// with no limit, and nothing kills it.
func TestAContainedTreeCannotStopItsCallerThenLiftTreeMemory(t *testing.T) {
	t.Parallel()
	for _, try := range []string{"freeze", "kill"} {
		what := "a Contained helper held to 64 MiB that tried to " + try + " its caller, then wrote -memory to its parent, and started a hog of 128 MiB"
		if held, _ := containedHere(t); !held {
			_, _, _ = containedRun(t, what, try+"-caller", "128", "hold", "")
			continue
		}
		parent, caller := callerCgroup(t, what, try)
		dir, err := os.OpenFile(filepath.Join(cgroupMount, caller), unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		c := &process{
			Path: exe, Args: []string{exe, helperMode + "contained-caller", parent, caller, try + "-caller"}, Stdout: &out, Stderr: &out,
			SysProcAttr: &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(dir.Fd())},
		}
		err = c.Start()
		dir.Close()
		if err != nil {
			t.Fatalf("%s: starting the caller: %v", what, err)
		}
		exited := make(chan error, 1)
		go func() { exited <- c.Wait() }()
		select {
		case err = <-exited:
		case <-time.After(2 * exitsWithin):
			events, _ := os.ReadFile(filepath.Join(cgroupMount, caller, "cgroup.events"))
			t.Errorf("%s: the caller has not exited after %v, its cgroup.events %q: the tree froze it", what, 2*exitsWithin, events)
			_ = os.WriteFile(filepath.Join(cgroupMount, caller, "cgroup.freeze"), []byte("0"), 0)
			err = <-exited
		}
		got := cgroupLines(out.Bytes())
		enoent := strconv.Itoa(int(syscall.ENOENT))
		if err != nil || got["errno"] != enoent || got["lifterrno"] != enoent || got["limit"] != "true" || got["code"] != "-1" || bytes.Contains(out.Bytes(), []byte("done\n")) {
			t.Errorf("%s: the caller ended %v, output %q, want it exit 0, both writes ENOENT, WhyLimit, Code -1 and no done", what, err, out.Bytes())
		}
		runGoneFrom(t, what, parent, got["run"])
	}
}

// callerCgroup makes, beside this process's cgroup, a cgroup of the test's own, marked delegated as
// systemd marks one and given the memory controller, and a caller cgroup in it, and gives their cgroup v2 paths; it removes both at
// the test's end, killing first what either holds, a tree a failed row left among it.
func callerCgroup(t *testing.T, what, try string) (parent, caller string) {
	t.Helper()
	here, err := selfPath()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	parent = filepath.Join(filepath.Dir(here), "contained-caller-"+strconv.Itoa(os.Getpid())+"-"+try)
	caller = filepath.Join(parent, "caller")
	p := filepath.Join(cgroupMount, parent)
	// The delegated cgroup gives the memory controller to its children, as a run's start has it do.
	if err := os.WriteFile(filepath.Join(cgroupMount, filepath.Dir(here), "cgroup.subtree_control"), []byte("+memory"), 0); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	t.Cleanup(func() {
		// remove is the run's own: it kills by cgroup.kill, then removes the deepest first.
		if err := (&runCgroup{path: p, pause: killPaused}).remove(); err != nil {
			t.Errorf("%s: %v", what, err)
		}
	})
	if err := unix.Setxattr(p, delegateMark, []byte(delegateValue), 0); err != nil {
		t.Fatalf("%s: marking %s delegated: %v", what, parent, err)
	}
	if err := os.Mkdir(filepath.Join(cgroupMount, caller), 0o700); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	return parent, caller
}

// TestAContainedTreeThatNestsACgroupNamespaceCannotLiftTreeMemory: a helper run Contained, held to
// 64 MiB, that moves into a child cgroup x of its run's and, from a cgroup namespace rooted at x,
// writes max to its run's memory.max and memory.swap.max, then starts a hog of 128 MiB, is refused
// the namespace with ENOSPC, by the 0 the run's shim wrote to user.max_cgroup_namespaces in the
// tree's user namespace, and ends with WhyLimit, its run's cgroup gone after: nest makes the
// namespace at its clone, with a user namespace of its own (unshare -U -C), and nestroot by unshare
// in a user namespace of its own that maps root (unshare -U -r, then -C). Where no cgroup is
// delegated, or cgroup v2 has no nsdelegate, the run is refused and starts nothing.
//
// What it costs when this fails: a Contained tree lifts its own limit with two writes and one
// unshare, unprivileged, whatever Yama says, and runs on past it.
func TestAContainedTreeThatNestsACgroupNamespaceCannotLiftTreeMemory(t *testing.T) {
	t.Parallel()
	for _, try := range []string{"nest", "nestroot"} {
		what := "a Contained helper held to 64 MiB that tried " + try + " and started a hog of 128 MiB"
		res, held, err := containedRun(t, what, try, "128", "exit", "")
		if !held {
			continue
		}
		got := cgroupLines(res.Stdout)
		if got["errno"] != strconv.Itoa(int(syscall.ENOSPC)) || !endedWith(err, WhyLimit) || bytes.Contains(res.Stdout, []byte("done\n")) {
			t.Errorf("%s: tried %q, errno %q, %v, Code %d, stdout %q, stderr %q, want the namespace refused with ENOSPC, WhyLimit and no done", what, got["tried"], got["errno"], err, res.Code, res.Stdout, res.Stderr)
		}
		runGone(t, what, got["run"])
	}
}

// capsFile is a copy of this test binary given cap_sys_admin as a file's capability, permitted and
// effective, by its security.capability (VFS_CAP_REVISION_2), and true: the one beside this binary
// named as it is with .capped, where root left one so, or one this process makes in a folder of the
// test's. Where it may not set one, which takes CAP_SETFCAP, it is the binary itself and false.
func capsFile(t *testing.T) (string, bool) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unix.Getxattr(exe+".capped", "security.capability", nil); err == nil {
		return exe + ".capped", true
	}
	b, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "capped")
	if err := writeExe(path, b); err != nil {
		t.Fatal(err)
	}
	// magic_etc, then permitted and inheritable for caps 0 to 31 and 32 to 63, each little-endian.
	vfs := []byte{1, 0, 0, 2, 0, 0, 1 << (unix.CAP_SYS_ADMIN - 16), 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	if err := unix.Setxattr(path, "security.capability", vfs, 0); err != nil {
		t.Logf("this process may not set a file's capabilities, so the row proves no_new_privs and no CapEff of the helper itself: %v", err)
		return exe, false
	}
	return path, true
}

// TestAContainedTreeGainsNoCapabilityByAFile: a helper run Contained, held to 64 MiB, runs with
// no_new_privs set and a CapEff of 0, and so does a copy of itself it runs that is given
// cap_sys_admin as a file's capability, permitted and effective, as the run's shim set no_new_privs
// and cleared its capabilities before its exec of the program: the kernel gives a file's
// capabilities written in the initial user namespace in every user namespace below it. Where this
// process may not set a file's capabilities and finds no copy that holds them, the helper runs
// itself, with the same want. Where the caller is root, no cgroup is delegated, or cgroup v2 has no
// nsdelegate, the run is refused and starts nothing.
//
// What it costs when this fails: a process of a Contained tree holds a capability in the tree's user
// namespace, the shim's CAP_SYS_RESOURCE kept, or gains one by an exec, cap_sys_admin by a file that
// carries it, and with CAP_SYS_RESOURCE may lift user.max_cgroup_namespaces and nest a cgroup
// namespace. Root's uid is not such a route here, as no_new_privs clamps root's exec too: what a
// root caller's tree holds without a capability, the global sysctls, is why a root caller is
// refused (TestContainedIsRefusedToARootCaller).
func TestAContainedTreeGainsNoCapabilityByAFile(t *testing.T) {
	t.Parallel()
	path, _ := capsFile(t)
	const what = "a Contained helper that ran a copy of itself with cap_sys_admin as a file's capability"
	res, held, err := containedRun(t, what, "caps", "16", "exit", path)
	if !held {
		return
	}
	got := cgroupLines(res.Stdout)
	none := "0000000000000000"
	if err != nil || got["tried"] != "<nil>" || got["owncapeff"] != none || got["ownnnp"] != "1" || got["capeff"] != none || got["nnp"] != "1" {
		t.Errorf("%s: %v, tried %q, its own CapEff %q and NoNewPrivs %q, the copy's %q and %q, stderr %q, want CapEff %s and NoNewPrivs 1 for each", what, err, got["tried"], got["owncapeff"], got["ownnnp"], got["capeff"], got["nnp"], res.Stderr, none)
	}
}

// TestAContainedProgramHasTheEnvironmentARunWithoutContainedGives: the env helper run Contained, held
// to 64 MiB, with an Env that names a variable twice, is given the same environment as the same run
// held to nothing, which its shim, started with none, reads from a descriptor, or where no cgroup is
// delegated, or cgroup v2 has no nsdelegate, is refused before its start; and a Contained run whose
// Env holds a NUL is refused with errNUL, ErrRefused alone, Code -1 and no output on every host, as
// Run refuses a NUL in any Cmd before its limits.
//
// What it costs when this fails: a Contained program runs with an environment its caller did not
// give, a variable lost, doubled or cut at a NUL, or none.
func TestAContainedProgramHasTheEnvironmentARunWithoutContainedGives(t *testing.T) {
	t.Parallel()
	c := helperCmd(t, "env")
	c.Env = append(c.Env, "PROC_GIVEN=1", "PROC_TWICE=a", "PROC_TWICE=b")
	want := environment(t, c)
	c.Limits = Limits{TreeMemory: treeLimit, Contained: true}
	const what = "the env helper run Contained"
	res, held, err := runContained(t, what, c)
	if held {
		got := strings.Split(strings.TrimSuffix(string(res.Stdout), "\x00"), "\x00")
		if err != nil || res.Code != 0 || !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want))) {
			t.Errorf("%s: %v, Code %d, environment %q, stderr %q, want %q, as a run without Contained gives", what, err, res.Code, got, res.Stderr, want)
		}
	}
	c.Env = append(c.Env, "PROC_NUL=a\x00b")
	if res, err := Run(t.Context(), c); !errors.Is(err, errNUL) || errors.Is(err, ErrNotHeld) || res.Code != -1 || len(res.Stdout) != 0 {
		t.Errorf("a Contained run whose Env holds a NUL: %v, Code %d, stdout %q, want errNUL, ErrRefused alone, Code -1 and no output, on any host", err, res.Code, res.Stdout)
	}
}

// TestAContainedRunWhoseShimCannotTakeAStepIsRefused: a Contained run, held to 64 MiB, whose shim
// is given a file to write 0 to that is not there, by the seam that stands in for
// user.max_cgroup_namespaces, is refused with errNoNsLimit; one given a folder to mount the tree's
// proc on that is not there, by the seam that stands in for /proc, with errNoProcMount; one given a
// file of mounts to read the cgroup mounts from that is not there, by the seam that stands in for
// the tree's mountinfo, with errNoCgroupMount; each inside
// errNoContained, ErrRefused and errors.ErrUnsupported, with ENOENT, Code -1 and no output: the
// program never ran. Where no cgroup is delegated, or cgroup v2 has no nsdelegate, the run is
// refused before its start.
//
// What it costs when this fails: a Contained tree runs where its shim could not lower
// user.max_cgroup_namespaces, as on a host whose /proc/sys is read-only, and may nest a cgroup
// namespace and lift its limit; or runs where it could not mount its own proc, as in a container
// that masks paths of /proc, and opens the caller's /proc files by pid; or where it could not cover
// the host's cgroup mounts, and freezes the caller, then lifts its limit; the caller told it was
// held.
func TestAContainedRunWhoseShimCannotTakeAStepIsRefused(t *testing.T) {
	t.Parallel()
	absent := filepath.Join(t.TempDir(), "absent")
	for _, c := range []struct {
		what string
		host cgroupHost
		want error
	}{
		{"a Contained run whose shim's file is not there", cgroupHost{limit: absent}, errNoNsLimit},
		{"a Contained run whose shim's proc folder is not there", cgroupHost{tree: absent}, errNoProcMount},
		{"a Contained run whose shim's file of mounts is not there", cgroupHost{cgroups: absent}, errNoCgroupMount},
	} {
		cmd := containedCmd(t, c.what, "none", "16", "exit", "")
		cmd.seam = &seams{host: c.host}
		res, held, err := runContained(t, c.what, cmd)
		if held && (!errors.Is(err, c.want) || !errors.Is(err, errNoContained) || !errors.Is(err, ErrRefused) ||
			!errors.Is(err, errors.ErrUnsupported) || !errors.Is(err, fs.ErrNotExist) || res.Code != -1 || len(res.Stdout) != 0) {
			t.Errorf("%s: %v, Code %d, stdout %q, want %v, errNoContained, ErrRefused and ErrUnsupported with ENOENT, Code -1 and no output", c.what, err, res.Code, res.Stdout, c.want)
		}
	}
}

// TestAContainedShimTriesACallAgainThatASignalInterrupts: a Contained run, held to 64 MiB, whose
// shim fails the first try of each fstat, getcwd and chdir with EINTR, by the seam shimEINTR, starts
// its program, which runs to its end, survived, Code 0. Where no cgroup is delegated, or cgroup v2
// has no nsdelegate, the run is refused before its start.
//
// What it costs when this fails: a signal to the shim on a slow FUSE mount, the Go runtime's
// preemption among them, refuses a run the shim would have started, as a folder it cannot find again
// or a descriptor it cannot check.
func TestAContainedShimTriesACallAgainThatASignalInterrupts(t *testing.T) {
	t.Parallel()
	const what = "a Contained run whose shim's fstat, getcwd and chdir each fail EINTR once"
	cmd := containedCmd(t, what, "none", "16", "exit", "")
	cmd.seam = &seams{host: cgroupHost{shim: shimEINTR}}
	res, held, err := runContained(t, what, cmd)
	if held && (err != nil || res.Code != 0 || !strings.HasSuffix(string(res.Stdout), "survived\n")) {
		t.Errorf("%s: %v, Code %d, stdout %q, want no error, Code 0 and survived", what, err, res.Code, res.Stdout)
	}
}

// TestAContainedRunHandedADirectoryIsRefused: a Contained run, held to 64 MiB, handed a folder of
// the test's as its Stdin, or among its ExtraFiles, is refused with errContainedHanded, inside
// ErrRefused but not errors.ErrUnsupported, with EISDIR, Code -1 and no output: the program never
// ran. Where no cgroup is delegated, or cgroup v2 has no nsdelegate, the run is refused before its
// start.
//
// What it costs when this fails: the tree holds a directory on the caller's mounts, under no cover,
// from which ".." walks past its root to the caller's cgroup mount, and freezes or kills the caller,
// then lifts its own limit; the caller told it was held.
func TestAContainedRunHandedADirectoryIsRefused(t *testing.T) {
	t.Parallel()
	for _, how := range []string{"Stdin", "ExtraFiles"} {
		what := "a Contained run handed a folder as its " + how
		dir, err := os.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = dir.Close() })
		cmd := containedCmd(t, what, "none", "16", "exit", "")
		if how == "Stdin" {
			cmd.Stdin = dir
		} else {
			cmd.ExtraFiles = []*os.File{dir}
		}
		res, held, err := runContained(t, what, cmd)
		if held && (!errors.Is(err, errContainedHanded) || !errors.Is(err, ErrRefused) || errors.Is(err, errors.ErrUnsupported) ||
			!errors.Is(err, syscall.EISDIR) || res.Code != -1 || len(res.Stdout) != 0) {
			t.Errorf("%s: %v, Code %d, stdout %q, want errContainedHanded and ErrRefused, not ErrUnsupported, with EISDIR, Code -1 and no output", what, err, res.Code, res.Stdout)
		}
	}
}

// TestAContainedRunWhoseHandedDescriptorCannotBeCheckedIsRefused: a helper that installs a seccomp
// filter failing an fstat of descriptor 0 with EIO, then makes a Contained run, held to 64 MiB,
// whose Stdin is the null device, has it refused with errContainedUnchecked and ErrRefused, not
// errors.ErrUnsupported, with EIO, Code -1 and no output: the program never ran. Where no cgroup
// is delegated, or cgroup v2 has no nsdelegate, the run is refused before its start.
//
// What it costs when this fails: a directory the run hands the tree, whose fstat fails as on a FUSE
// mount whose server died, passes the shim's check; the program starts with it, and ".." from it
// walks the caller's mounts to its cgroups, so the tree freezes or kills the caller, then lifts its
// own limit.
func TestAContainedRunWhoseHandedDescriptorCannotBeCheckedIsRefused(t *testing.T) {
	t.Parallel()
	const what = "a Contained run whose Stdin's fstat fails EIO, by a seccomp filter its caller installed"
	held, _ := containedHere(t)
	res, err := Run(t.Context(), helperCmd(t, "contained-unchecked"))
	if err != nil || res.Code != 0 {
		t.Fatalf("%s: the helper: %v, Code %d, stdout %q, stderr %q", what, err, res.Code, res.Stdout, res.Stderr)
	}
	want := "out 0\nunchecked true\nrefused true\nunsupported false\neio true\ncode -1\n"
	if !held {
		want = "out 0\nunchecked false\nrefused true\n"
	}
	if !strings.HasPrefix(string(res.Stdout), want) {
		t.Errorf("%s: %q, want it to start %q", what, res.Stdout, want)
	}
}

// TestAShimThatHoldsNoAmbientCapabilityWritesNothing: this test binary started as the shim, with no
// ambient capability, as a setuid or file-capability exec of a binary that imports proc starts it,
// given a file of the test's to write 0 to, the argv helper to exec and a pipe at the descriptor its
// argv names for its report, writes nothing to that pipe, leaves the file as it was and execs
// nothing, and exits 127, whatever this process's own capabilities.
//
// What it costs when this fails: anyone may start a setuid binary that imports proc as the shim, have
// it write 0 to the initial user namespace's user.max_cgroup_namespaces, which no cgroup namespace on
// the host may then pass, and exec a program of theirs as the binary's owner; or it writes two bytes
// to a descriptor an earlier init of that binary opened, which its argv names.
func TestAShimThatHoldsNoAmbientCapabilityWritesNothing(t *testing.T) {
	t.Parallel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	limit := filepath.Join(t.TempDir(), "limit")
	if err := os.WriteFile(limit, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var out bytes.Buffer
	shim := &process{Path: exe, Args: []string{shimMark, "3", limit, t.TempDir(), limit, exe, "", exe, helperMode + "argv"}, Stdout: &out, ExtraFiles: []*os.File{w}}
	ran := shim.Run()
	w.Close()
	report, rerr := io.ReadAll(r)
	b, lerr := os.ReadFile(limit)
	exited, isExit := errors.AsType[*exec.ExitError](ran)
	if rerr != nil || lerr != nil || len(report) != 0 || string(b) != "1" || out.Len() != 0 || !isExit || exited.ExitCode() != 127 {
		t.Errorf("a shim with no ambient capability: %v, report %q (%v), file %q (%v), stdout %q, want exit 127, no report, the file 1 and no output", ran, report, rerr, b, lerr, out.String())
	}
}

// TestAContainedTreeCannotNameTheCallerByPid: a helper run Contained, held to 64 MiB, sees its parent's
// pid as 0, as pid 1 of a pid namespace of its own, and its signal 0 to this process's pid is ESRCH,
// as no process of the namespace has that pid; then holds, and its Timeout still ends the run, with
// WhyDeadline, and its run's cgroup is gone after. Where no cgroup is delegated, or cgroup v2 has no
// nsdelegate, the run is refused and starts nothing.
//
// What it costs when this fails: a Contained tree stops or kills the caller, or lowers its limits by
// prlimit, by its pid, whatever Yama says, and so defeats the Timeout and the end's kill.
func TestAContainedTreeCannotNameTheCallerByPid(t *testing.T) {
	t.Parallel()
	const what = "a Contained helper that signalled this process by its pid, then held"
	res, held, err := containedRun(t, what, "signal", "16", "hold", strconv.Itoa(os.Getpid()))
	if !held {
		return
	}
	got := cgroupLines(res.Stdout)
	if got["ppid"] != "0" || got["pid"] != "1" || got["self"] != "1" || got["proc"] != strconv.Itoa(int(syscall.ENOENT)) ||
		got["errno"] != strconv.Itoa(int(syscall.ESRCH)) || !endedWith(err, WhyDeadline) {
		t.Errorf("%s: ppid %q, pid %q, /proc/self %q, stat errno %q, signal errno %q, %v, stdout %q, stderr %q, want ppid 0, pid and /proc/self 1, ENOENT, ESRCH and WhyDeadline", what, got["ppid"], got["pid"], got["self"], got["proc"], got["errno"], err, res.Stdout, res.Stderr)
	}
	runGone(t, what, got["run"])
}

// TestAContainedRunWhoseOwnCmdCannotStartIsRefusedWithoutErrNotHeld: a Contained run, held to 64
// MiB, of a Path that names no file (ENOENT) or a file that is no program (ENOEXEC) ends with
// errContainedExec; one with an argument past the exec limit (E2BIG) with a failed start inside
// ErrRefused; one whose Dir names no folder, or names a file, with errContainedDir before any
// check of the host; and one whose Dir is a folder the caller cannot enter, of mode 0 and the
// caller's own, or of a uid the tree does not map (othersLocked), with the shim's
// errContainedFolder and EACCES, as the shim, not the start, enters Dir. Each matches
// ErrRefused, not ErrNotHeld or errors.ErrUnsupported, has its cause inside, and Code -1. Where the caller is root, no cgroup is delegated, or
// cgroup v2 has no nsdelegate, all but the rows of a Dir that names no folder are refused before
// the start. Where the run is held and no folder of another uid is found that the caller cannot
// enter, the test fails: without that row a start that enters Dir again passes it.
//
// What it costs when this fails: a caller told ErrNotHeld for a mistyped path, or for a file put
// there by the party the run contains, reads it as a host that cannot contain the run, and runs the
// program again without Contained; and with EACCES among hostStartErrnos, a Dir the caller cannot
// enter, entered by the start, is ErrNotHeld so. Only the folder of another uid shows the start's
// entry: the start's child holds every capability in the tree's user namespace, where the caller's
// uid is mapped, so CAP_DAC_OVERRIDE takes it into the caller's own folder of mode 0, and only the
// shim, which holds two capabilities, fails there.
func TestAContainedRunWhoseOwnCmdCannotStartIsRefusedWithoutErrNotHeld(t *testing.T) {
	t.Parallel()
	held, want := containedHere(t)
	file := filepath.Join(t.TempDir(), "not-a-program")
	if err := writeExe(file, []byte("not a program\n")); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	others := othersLocked()
	if held && others == "" {
		t.Errorf("no folder of another uid that this process cannot enter, of %q, so a start that enters Dir is not shown", othersTried())
	}
	for _, tc := range []struct {
		what, path, dir, arg string
		ofCmd                bool // refused by Run's own checks, on any host
		want                 []error
	}{
		{"a Dir of the caller's own it cannot enter", "", locked, "", false, []error{errContainedFolder, syscall.EACCES}},
		{"a Dir of another uid the caller cannot enter, " + others, "", cmp.Or(others, locked), "", false, []error{errContainedFolder, syscall.EACCES}},
		{"a Path that names no file", filepath.Join(t.TempDir(), "absent"), "", "", false, []error{errContainedExec, syscall.ENOENT}},
		{"a Path that names no program", file, "", "", false, []error{errContainedExec, syscall.ENOEXEC}},
		{"an argument past the exec limit", "", "", strings.Repeat("a", 256<<10), false, []error{syscall.E2BIG}},
		{"a Dir that names no folder", "", filepath.Join(t.TempDir(), "absent"), "", true, []error{errContainedDir, fs.ErrNotExist}},
		{"a Dir that names a file", "", file, "", true, []error{errContainedDir}},
	} {
		c := helperCmd(t, "none")
		c.Path, c.Dir = cmp.Or(tc.path, c.Path), tc.dir
		if tc.arg != "" {
			c.Args = append(c.Args, tc.arg)
		}
		c.Limits = Limits{TreeMemory: treeLimit, Contained: true}
		res, err := Run(t.Context(), c)
		switch {
		case held || tc.ofCmd:
			if slices.ContainsFunc(tc.want, func(w error) bool { return !errors.Is(err, w) }) || !errors.Is(err, ErrRefused) ||
				errors.Is(err, ErrNotHeld) || errors.Is(err, errors.ErrUnsupported) || res.Code != -1 {
				t.Errorf("a Contained run of %s: %v, Code %d, want %v and ErrRefused, not ErrNotHeld or ErrUnsupported, and Code -1", tc.what, err, res.Code, tc.want)
			}
		case want != nil && !errors.Is(err, want):
			t.Errorf("a Contained run of %s, refused here: %v, want %v", tc.what, err, want)
		case want == nil:
			refusedHere(t, "a Contained run of "+tc.what, res, err)
		}
	}
}

// othersTried are the folders othersLocked tries, in order: root's home first, mode 0700 or 0550
// and root's on every Linux tried.
func othersTried() []string {
	return []string{"/root", "/etc/ssl/private", "/var/lib/private", "/lost+found"}
}

// othersLocked is the first of othersTried that is a folder owned by a uid other than this
// process's effective one, which a Contained tree does not map, and that this process cannot
// enter (EACCES by its effective ids), or "" where none is.
func othersLocked() string {
	for _, p := range othersTried() {
		var st unix.Stat_t
		if unix.Stat(p, &st) == nil && st.Mode&unix.S_IFMT == unix.S_IFDIR && int(st.Uid) != os.Geteuid() &&
			errors.Is(unix.Faccessat(unix.AT_FDCWD, p, unix.X_OK, unix.AT_EACCESS), unix.EACCES) {
			return p
		}
	}
	return ""
}

// TestAContainedStartIsErrNotHeldOnlyForTheHostsErrnos: startFailed gives a contained tree's failed
// start errNoContained, so ErrNotHeld, for each errno of hostStartErrnos, and ErrRefused alone,
// neither ErrNotHeld nor errors.ErrUnsupported, for any other: arguments the exec refuses, and an
// error with no errno. EACCES is the host's, as a Contained start enters no Dir. The
// error is inside each; a tree not contained, or none, gives the error as it is.
//
// What it costs when this fails: a start the caller's own Cmd failed reads as a host that cannot
// contain the run, and the caller runs it again without Contained; or a host that refuses the
// namespaces reads as the run's own.
func TestAContainedStartIsErrNotHeldOnlyForTheHostsErrnos(t *testing.T) {
	t.Parallel()
	start := func(errno error) error { return &fs.PathError{Op: "fork/exec", Path: "/x", Err: errno} }
	contained := &runCgroup{contained: true}
	for _, errno := range []error{syscall.EPERM, syscall.EINVAL, syscall.ENOSPC, syscall.ENOSYS, syscall.ENOENT, syscall.ENOTDIR, syscall.EACCES, syscall.E2BIG, syscall.ENOEXEC, errors.New("no errno")} {
		err := contained.startFailed(start(errno))
		var e syscall.Errno
		host := errors.As(errno, &e) && slices.Contains(hostStartErrnos(), e)
		if !errors.Is(err, errno) || !errors.Is(err, ErrRefused) || errors.Is(err, ErrNotHeld) != host || errors.Is(err, errNoContained) != host {
			t.Errorf("a contained start that failed with %v: %v, want it inside ErrRefused, and errNoContained and ErrNotHeld %v", errno, err, host)
		}
		for _, tree := range []*runCgroup{nil, {}} {
			if got := tree.startFailed(start(errno)); got.Error() != start(errno).Error() || errors.Is(got, ErrRefused) {
				t.Errorf("a start not contained that failed with %v: %v, want it as it is", errno, got)
			}
		}
	}
	if len(hostStartErrnos()) != 5 {
		t.Errorf("hostStartErrnos: %v, want EPERM, EINVAL, ENOSPC, ENOSYS and EACCES, and a new one ruled", hostStartErrnos())
	}
}

// TestAContainedStartGivesTheShimItsDir: place gives a contained tree's start the shim's argv, the
// program's path, then its Dir, as given, absolute, relative or empty, then the program's argv, as
// init reads them (at[3], at[4], then argv from os.Args[7]), and leaves the start's own Dir empty, so the start enters no
// folder and a Dir the caller cannot enter fails in the shim.
//
// What it costs when this fails: the start enters Dir again, its EACCES reads as the host's, and a
// caller told ErrNotHeld for a Dir it cannot enter runs the program without Contained; or the shim
// runs the program in another folder than Dir.
func TestAContainedStartGivesTheShimItsDir(t *testing.T) {
	t.Parallel()
	for _, dir := range []string{"/abs/dir", "rel/../dir", ""} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		env, err := memfd()
		if err != nil {
			t.Fatal(err)
		}
		tree := &runCgroup{contained: true, dir: r, shim: [4]*os.File{r, w, env, r}, exe: "/the/shim", mark: shimMark, limit: "limit", tree: "tree", cgroups: "cgroups"}
		cmd := &process{Path: "/the/program", Args: []string{"program", "an argument"}, Dir: dir}
		err = tree.place(cmd)
		args := []string{shimMark, strconv.Itoa(3 + len(cmd.ExtraFiles) - shimFiles), "limit", "tree", "cgroups", "/the/program", dir, "program", "an argument"}
		if err != nil || cmd.Path != "/the/shim" || cmd.Dir != "" || !slices.Equal(cmd.Args, args) {
			t.Errorf("a contained start of Dir %q: %v, path %q, Dir %q, argv %q, want path /the/shim, no Dir and argv %q", dir, err, cmd.Path, cmd.Dir, cmd.Args, args)
		}
		for _, f := range []*os.File{r, w, env} {
			_ = f.Close()
		}
	}
}

// TestAContainedRunWhoseStartIsRefusedEACCESIsNotHeld: a Contained run, held to 64 MiB, whose shim's
// binary is a file no one may exec, by the seam, fails at its start with EACCES, as a security
// module that refuses the user namespace fails it, and is refused with errNoContained, so
// ErrNotHeld, ErrRefused and errors.ErrUnsupported, with EACCES inside, Code -1 and no output: the
// start enters no folder of the Cmd's, so its EACCES is the host's. Where the caller is
// root, no cgroup is delegated, or cgroup v2 has no nsdelegate, the run is refused before its start.
//
// What it costs when this fails: on a host whose SELinux or AppArmor refuses the user namespace with
// EACCES, a caller that drops Contained where the host cannot hold it meets a hard refusal instead,
// and never runs.
func TestAContainedRunWhoseStartIsRefusedEACCESIsNotHeld(t *testing.T) {
	t.Parallel()
	const what = "a Contained run whose start fails EACCES"
	exe := filepath.Join(t.TempDir(), "no-exec")
	if err := os.WriteFile(exe, []byte(skipShimScript), 0o600); err != nil {
		t.Fatal(err)
	}
	c := helperCmd(t, "argv")
	c.Limits = Limits{TreeMemory: treeLimit, Contained: true}
	c.seam = &seams{host: cgroupHost{exe: exe}}
	res, held, err := runContained(t, what, c)
	if held && (!errors.Is(err, syscall.EACCES) || !errors.Is(err, errNoContained) || !errors.Is(err, ErrNotHeld) || !errors.Is(err, ErrRefused) ||
		!errors.Is(err, errors.ErrUnsupported) || res.Code != -1 || len(res.Stdout) != 0) {
		t.Errorf("%s: %v, Code %d, stdout %q, want EACCES inside errNoContained, ErrNotHeld, ErrRefused and ErrUnsupported, Code -1 and no output", what, err, res.Code, res.Stdout)
	}
}

// TestAContainedRunReadsItsPeak: a helper run Contained, held to 64 MiB, with ReadPeak, that starts
// a hog of 16 MiB, each page written, runs to its end, Code 0, and reads a Peak of 16 MiB or more
// with PeakRead true, where Contained is held and the kernel gives memory.peak.
//
// What it costs when this fails: a Contained run's namespaces, or its own watch, cost the caller the
// peak it asked for, and it sizes the next run by none.
func TestAContainedRunReadsItsPeak(t *testing.T) {
	t.Parallel()
	const what = "a Contained helper with ReadPeak and a hog of 16 MiB"
	reads := peakHere(t)
	c := containedCmd(t, what, "none", "16", "exit", "")
	c.ReadPeak = true
	res, held, err := runContained(t, what, c)
	if !held {
		return
	}
	if err != nil || res.Code != 0 || (reads && (!res.PeakRead || res.Peak < 16<<20)) {
		t.Errorf("%s: %v, Code %d, Peak %d, PeakRead %v, want Code 0 and, where memory.peak is read, a Peak of %d or more", what, err, res.Code, res.Peak, res.PeakRead, 16<<20)
	}
}

// TestTheWatchKillsTheTreeOnALossOrAFileItCannotRead: a contained run's check, on a cgroup's files
// in a folder, writes 1 to its cgroup.kill and ends the check with errLimitLost when memory is not
// among its cgroup.controllers, when its memory.max reads other than the run set, when its
// memory.swap.max reads other than 0 or its memory.oom.group other than 1, or either is gone,
// or when its memory.events is gone, has no oom line, is empty or holds a count that is no number; writes 1 and ends with no error on an oom; and writes nothing and goes on
// while the cgroup holds its limit with no oom.
//
// What it costs when this fails: the watch of a Contained run fails open, on a loss of the memory
// controller, a lifted memory.max, or a memory.events it cannot read, and the tree runs on past a
// limit it no longer has.
func TestTheWatchKillsTheTreeOnALossOrAFileItCannotRead(t *testing.T) {
	t.Parallel()
	const set = "67108864\n"
	for _, r := range []struct {
		name, controllers, max, events string
		lost, ended                    bool
		override                       map[string]string
	}{
		{"memory gone from cgroup.controllers", "cpu pids\n", set, "oom 0\n", true, true, nil},
		{"memory.max lifted", "cpu memory pids\n", "max\n", "oom 0\n", true, true, nil},
		{"memory.events gone", "cpu memory pids\n", set, "", true, true, nil},
		{"memory.events with no oom line", "cpu memory pids\n", set, "low 0\nhigh 0\n", true, true, nil},
		{"an empty memory.events", "cpu memory pids\n", set, "(empty)", true, true, nil},
		{"an oom count that is no number", "cpu memory pids\n", set, "oom many\n", true, true, nil},
		{"an oom", "cpu memory pids\n", set, "oom 1\n", false, true, nil},
		{"memory.swap.max lifted", "cpu memory pids\n", set, "oom 0\n", true, true, map[string]string{"memory.swap.max": "max\n"}},
		{"memory.swap.max gone", "cpu memory pids\n", set, "oom 0\n", true, true, map[string]string{"memory.swap.max": ""}},
		{"memory.oom.group written 0", "cpu memory pids\n", set, "oom 0\n", true, true, map[string]string{"memory.oom.group": "0\n"}},
		{"memory.oom.group gone", "cpu memory pids\n", set, "oom 0\n", true, true, map[string]string{"memory.oom.group": ""}},
		{"the limit held", "cpu memory pids\n", set, "oom 0\n", false, false, nil},
	} {
		mount := t.TempDir()
		dir := filepath.Join(mount, "proc-x")
		files := map[string]string{"cgroup.controllers": r.controllers, "memory.max": r.max, "cgroup.kill": ""}
		switch r.events {
		case "":
		case "(empty)":
			files["memory.events"] = ""
		default:
			files["memory.events"] = r.events
		}
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		for name, body := range map[string]string{"memory.swap.max": "0\n", "memory.oom.group": "1\n"} {
			if _, set := r.override[name]; !set {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		for name, body := range r.override {
			if body == "" {
				continue // a file that is not there
			}
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		c := &runCgroup{path: dir, max: set}
		ended, why := c.check()
		kill, err := os.ReadFile(filepath.Join(dir, "cgroup.kill"))
		if err != nil {
			t.Fatal(err)
		}
		if ended != r.ended || errors.Is(why, errLimitLost) != r.lost || (!r.lost && why != nil) || (string(kill) == "1") != r.ended {
			t.Errorf("%s: ended %v, %v, cgroup.kill %q, want ended %v, errLimitLost %v and a kill %v", r.name, ended, why, kill, r.ended, r.lost, r.ended)
		}
	}
}

// TestARunInARunsCgroupIsRefusedBeforeAnythingIsMade: on a host whose every check passes but whose
// process is in a run's cgroup, or in a cgroup beneath one, a run held to TreeMemory, and one that
// only asks for its peak, are each refused with errInRun, ErrRefused and errors.ErrUnsupported, Code
// -1; the program, which would make a file if run, makes none, and nothing is written to the host. A
// cgroup whose name is near a run's but is not one, proc- and fewer digits or a capital, is no run's.
//
// What it costs when this fails: a caller built on proc, run by proc, makes its runs' cgroups beside
// its own, out of the outer run's limit, its kill and its peak; or a ReadPeak run there goes without
// a cgroup, held to nothing, where the caller should decide.
func TestARunInARunsCgroupIsRefusedBeforeAnythingIsMade(t *testing.T) {
	t.Parallel()
	for _, own := range []string{"/user.slice/svc.service/proc-0123456789abcdef", "/user.slice/svc.service/proc-0123456789abcdef/x"} {
		for _, r := range []struct {
			name string
			set  func(*Cmd)
		}{{"TreeMemory", func(c *Cmd) { c.Limits = Limits{TreeMemory: treeLimit} }}, {"ReadPeak", func(c *Cmd) { c.ReadPeak = true }}} {
			what := r.name + " in " + own
			f := goodCgroups(false)
			f.files["/self"] = "0::" + own + "\n"
			mark := filepath.Join(t.TempDir(), "started")
			c := helperCmd(t, "touch", mark)
			r.set(&c)
			c.seam = &seams{host: fakeHost(f)}
			res, err := Run(t.Context(), c)
			if !errors.Is(err, errInRun) || !errors.Is(err, ErrRefused) || !errors.Is(err, errors.ErrUnsupported) || res.Code != -1 {
				t.Errorf("%s: %v, Code %d, want errInRun, ErrRefused, ErrUnsupported and Code -1", what, err, res.Code)
			}
			if _, err := os.Stat(mark); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s: the program ran (%v), want nothing started", what, err)
			}
			if len(f.wrote) != 0 {
				t.Errorf("%s: wrote %q to the host, want nothing", what, f.wrote)
			}
		}
	}
	for _, name := range []string{"proc-0123", "proc-0123456789abcdeF", "proc-0123456789abcdef0"} {
		f := goodCgroups(true)
		f.files["/self"] = "0::/user.slice/svc.service/" + name + "\n"
		if p, err := fakeHost(f).parent(); err != nil || p != fakeParent {
			t.Errorf("a cgroup named %s: %q, %v, want %q, as it is no run's", name, p, err, fakeParent)
		}
	}
}

// TestARunFromARunsCgroupIsRefused: a helper held to a TreeMemory of 64 MiB, in its run's cgroup,
// runs a helper of its own with ReadPeak: that run is refused with errInRun, ErrRefused and
// errors.ErrUnsupported, Code -1, and its program never runs, so names no cgroup beside the outer
// run's. Where no cgroup is delegated, the outer run is refused and starts nothing.
//
// What it costs when this fails: a runner built on proc that runs a tool built on proc makes the
// tool's cgroup beside its own, out of its limit, its kill and its peak.
func TestARunFromARunsCgroupIsRefused(t *testing.T) {
	t.Parallel()
	const what = "a ReadPeak run from a helper held to 64 MiB"
	_, why := delegated(t)
	c := helperCmd(t, "nested-peak")
	c.Limits = Limits{TreeMemory: treeLimit}
	res, err := Run(t.Context(), c)
	if why != nil {
		refusedHere(t, what, res, err)
		return
	}
	got := cgroupLines(res.Stdout)
	if err != nil || res.Code != 0 || got["refused"] != "true" || got["inrun"] != "true" || got["code"] != "-1" || got["inner"] != "none" {
		t.Errorf("%s: %v, Code %d, stdout %q, stderr %q, want the inner run refused by errInRun, Code -1, its program never run", what, err, res.Code, res.Stdout, res.Stderr)
	}
	cgroupGone(t, what, res.Stdout)
}

// TestARunsCgroupIsGoneWithACgroupItsProgramMade: a helper held to TreeMemory that makes a cgroup in
// its own, the run's, and exits 0 returns no error and Code 0, and once the run returns its cgroup is
// gone, the one the helper made with it. The kernel will not remove a cgroup that holds another, so
// the run removes those beneath its own first. Where no cgroup is delegated, the run is refused and
// starts nothing.
//
// What it costs when this fails: a program that makes a cgroup in its run's holds the run 5 s past
// its end, with an error for a program that exited 0, and leaves both cgroups in the caller's
// delegated parent for good.
func TestARunsCgroupIsGoneWithACgroupItsProgramMade(t *testing.T) {
	t.Parallel()
	const what = "a helper held to 64 MiB that made a cgroup in its own"
	_, why := delegated(t)
	c := helperCmd(t, "cgroup-child")
	c.Limits = Limits{TreeMemory: treeLimit}
	res, err := Run(t.Context(), c)
	if why != nil {
		refusedHere(t, what, res, err)
		return
	}
	if err != nil || res.Code != 0 || !strings.Contains(string(res.Stdout), "\nmade x\n") {
		t.Errorf("%s: %v, Code %d, stdout %q, stderr %q, want exit 0, made x and no error", what, err, res.Code, res.Stdout, res.Stderr)
	}
	cgroupGone(t, what, res.Stdout)
}

// TestAReadPeakRunUnderACappedParentDoesNotEndLimit: a ReadPeak run of a hog of 128 MiB, with no
// Limits, under a parent capped at 64 MiB, is killed by the cap and ends Code -1, but not `limit`:
// the run set no limit, and a kill for a limit above its cgroup is not one of its. Where no cgroup
// is delegated, there is no parent to cap, and the same ReadPeak run goes, reads no peak and ends
// Code 0: that half runs in every build. The capped half is live (cappedHalf, in
// peak_live_linux_test.go), as its helper must be one no oom may kill; outside a live
// run, where a cgroup is delegated, the row says so in its log and proves nothing more.
//
// What it costs when this fails: a run with zero Limits ends `limit`, or a TreeMemory run ends
// `limit` for a limit not its own, where WhyLimit says the cgroup's own memory.max; or a run that
// asks for its peak where no cgroup is delegated is refused or ends other than its tree did.
func TestAReadPeakRunUnderACappedParentDoesNotEndLimit(t *testing.T) {
	t.Parallel()
	p, why := delegated(t)
	if why == nil {
		cappedHalf(t, p)
		return
	}
	c := helperCmd(t, "hog", "128")
	c.ReadPeak = true
	res, err := Run(t.Context(), c)
	if err != nil || res.Code != 0 || res.PeakRead {
		t.Errorf("a ReadPeak run of a hog of 128 MiB, with no cgroup delegated: %v, Code %d, PeakRead %v, want exit 0 and no peak", err, res.Code, res.PeakRead)
	}
}

// TestAContainedTreeCannotNameASiblingCgroup: a helper run Contained writes max to the memory.max of
// a cgroup beside its run's, made by this process and held to 64 MiB, by the path this process
// names it by, and the write is ENOENT: the cgroup v2 the shim mounted over the tree's cgroup mount
// is rooted at the run's cgroup, so the sibling's memory.max reads 64 MiB after, and the helper runs
// to its end, Code 0. Where no cgroup is delegated, or cgroup v2 has no nsdelegate,
// the run is refused and starts nothing.
//
// What it costs when this fails: a Contained tree lifts the limit of every cgroup of the caller's
// user, another run's among them, as nsdelegate does not refuse a write outside the tree's cgroup
// namespace; or, with EPERM, the write named a file of the run's own, and the row passes for a
// reason that is not the mount's.
func TestAContainedTreeCannotNameASiblingCgroup(t *testing.T) {
	t.Parallel()
	const what = "a Contained helper that wrote max to a sibling cgroup's memory.max by its caller's path"
	var sib string
	if held, _ := containedHere(t); held {
		here, err := selfPath()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		parent := filepath.Join(cgroupMount, filepath.Dir(here))
		sib = filepath.Join(filepath.Dir(here), "contained-sibling-"+strconv.Itoa(os.Getpid()))
		if err := os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), []byte("+memory"), 0); err != nil {
			t.Fatalf("%s: giving the parent's children memory: %v", what, err)
		}
		if err := os.Mkdir(filepath.Join(cgroupMount, sib), 0o700); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		t.Cleanup(func() {
			if err := os.Remove(filepath.Join(cgroupMount, sib)); err != nil {
				t.Errorf("%s: removing the sibling: %v", what, err)
			}
		})
		if err := os.WriteFile(filepath.Join(cgroupMount, sib, "memory.max"), []byte("67108864"), 0); err != nil {
			t.Fatalf("%s: holding the sibling to 64 MiB: %v", what, err)
		}
	}
	res, held, err := containedRun(t, what, "sibling", "16", "exit", sib)
	if !held {
		return
	}
	got := cgroupLines(res.Stdout)
	if err != nil || res.Code != 0 || got["errno"] != strconv.Itoa(int(syscall.ENOENT)) {
		t.Errorf("%s: %v, Code %d, stdout %q, stderr %q, want its try ENOENT and exit 0", what, err, res.Code, res.Stdout, res.Stderr)
	}
	if b, err := os.ReadFile(filepath.Join(cgroupMount, sib, "memory.max")); err != nil || string(b) != "67108864\n" {
		t.Errorf("%s: the sibling's memory.max reads %q (%v), want 67108864", what, b, err)
	}
	runGone(t, what, got["run"])
}

// TestARunEndsLimitOnlyWhenItsOwnLimitKilledTheTree: limitEnded is true for the run's own kill on
// an oom; for an oom of the run's own cgroup's memory.max with the kernel's group kill; and, on a
// kernel whose memory.events.local has no oom_group_kill (5.14 to 5.16), for that oom with a kill of
// its own (memory.events.local's oom_kill). It is false for no oom of its own, an oom of a cgroup
// beneath (counted in memory.events alone), an oom with no group kill, and, on that older kernel, an
// oom of its own with no kill of its own, whatever memory.events counts beneath. events reads a
// cgroup's counts, and refuses a line that is no name and count.
//
// What it costs when this fails: a tree that handled an ENOMEM, or one whose own cgroup's limit
// killed a process of it, ends `limit`, which says the run's limit killed it whole; or one the run's
// limit did kill ends as if its program had crashed.
func TestARunEndsLimitOnlyWhenItsOwnLimitKilledTheTree(t *testing.T) {
	t.Parallel()
	for _, r := range []struct {
		what   string
		killed bool
		local  map[string]uint64
		want   bool
	}{
		{"the run's own kill", true, map[string]uint64{"oom": 0, "oom_group_kill": 0}, true},
		{"an oom of its own with a group kill", false, map[string]uint64{"oom": 1, "oom_kill": 2, "oom_group_kill": 1}, true},
		{"no oom", false, map[string]uint64{"oom": 0, "oom_kill": 0, "oom_group_kill": 0}, false},
		{"an oom beneath, with its kill", false, map[string]uint64{"oom": 0, "oom_kill": 0, "oom_group_kill": 0}, false},
		{"an oom of its own that killed nothing", false, map[string]uint64{"oom": 1, "oom_kill": 0, "oom_group_kill": 0}, false},
		{"an oom of its own with a kill of one process alone", false, map[string]uint64{"oom": 1, "oom_kill": 1, "oom_group_kill": 0}, false},
		// A kernel of 5.14 to 5.16: memory.events.local has oom_kill (since 5.2) and no oom_group_kill.
		{"an older kernel: an oom of its own with a kill", false, map[string]uint64{"oom": 1, "oom_kill": 1}, true},
		{"an older kernel: an oom of its own that killed nothing", false, map[string]uint64{"oom": 1, "oom_kill": 0}, false},
	} {
		if got := limitEnded(r.killed, r.local); got != r.want {
			t.Errorf("%s: limitEnded %t, want %t", r.what, got, r.want)
		}
	}
	// A kernel of 5.14 to 5.16, whose run's own limit was reached and killed nothing, where a cgroup
	// beneath killed a process of its own: memory.events counts that kill, and is not read.
	older := &runCgroup{path: t.TempDir()}
	for name, b := range map[string]string{"memory.events.local": "oom 1\noom_kill 0\n", "memory.events": "oom 2\noom_kill 1\n"} {
		if err := os.WriteFile(filepath.Join(older.path, name), []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := older.limitPassed(false); got || err != nil {
		t.Errorf("an older kernel's oom of its own that killed nothing, with a kill beneath: limitPassed %t, %v, want false", got, err)
	}
	dir := t.TempDir()
	cg := &runCgroup{path: dir}
	if err := os.WriteFile(filepath.Join(dir, "memory.events.local"), []byte("low 0\noom 3\noom_group_kill 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := cg.events("memory.events.local"); err != nil || got["oom"] != 3 || got["oom_group_kill"] != 1 || len(got) != 3 {
		t.Errorf("events of three counts: %v, %v, want low 0, oom 3 and oom_group_kill 1", got, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.events.local"), []byte("oom 3\noom_kill\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := cg.events("memory.events.local"); err == nil {
		t.Errorf("events with a line of no count: %v, want an error", got)
	}
}

// TestALimitOfACgroupBeneathTheRunsDoesNotEndLimit: a helper held to 96 MiB holds a cgroup it made
// beneath its run's to 16 MiB and starts a hog of 64 MiB into it. That cgroup's limit kills the hog,
// and the helper exits 0, so the run ends Code 0 and not `limit`: the run's own limit killed nothing.
// The run's cgroup, the two the helper made with it, is gone. Where no cgroup is delegated, the run
// is refused and starts nothing.
//
// What it costs when this fails: a tree that sets a limit of its own on a part of it ends `limit`,
// which says the run's limit killed it whole, where the tree went on and exited 0.
func TestALimitOfACgroupBeneathTheRunsDoesNotEndLimit(t *testing.T) {
	t.Parallel()
	const what = "a helper held to 96 MiB whose hog of 64 MiB a cgroup beneath held to 16 MiB"
	_, why := delegated(t)
	c := helperCmd(t, "nested-limit", "64")
	c.Limits = Limits{TreeMemory: 96 << 20}
	res, err := Run(t.Context(), c)
	if why != nil {
		refusedHere(t, what, res, err)
		return
	}
	if got := cgroupLines(res.Stdout)["hog"]; err != nil || res.Code != 0 || got != "signal: killed" {
		t.Errorf("%s: %v, Code %d, stdout %q, stderr %q, want exit 0, no error and its hog killed", what, err, res.Code, res.Stdout, res.Stderr)
	}
	cgroupGone(t, what, res.Stdout)
}

// TestAPeakOfZeroOrMaxOrNoCountReadsNone: end, on a planted folder that stands for a run's
// cgroup, reads memory.peak's count as the peak, and reads no peak where the file is missing, is 0,
// is "max", holds no count, holds one past an int64, or cannot be read, with no error.
//
// What it costs when this fails: a run whose kernel gives no memory.peak reads a peak of 0 bytes, or
// one of 16 EiB, with PeakRead true, and the caller sizes its next run by it.
func TestAPeakOfZeroOrMaxOrNoCountReadsNone(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		peak string // "" is no memory.peak
		want int64
		read bool
	}{
		{"12345\n", 12345, true},
		{"", 0, false},
		{"0\n", 0, false},
		{"max\n", 0, false},
		{"lots\n", 0, false},
		{"9223372036854775808\n", 0, false},
		{"(unreadable)", 0, false},
	} {
		mount := t.TempDir()
		path := filepath.Join(mount, "run")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		files := map[string]string{"memory.stat": "anon 4096\n"}
		switch c.peak {
		case "":
		case "(unreadable)": // a folder named memory.peak: opening it works, and reading it fails
			if err := os.Mkdir(filepath.Join(path, "memory.peak"), 0o700); err != nil {
				t.Fatal(err)
			}
		default:
			files["memory.peak"] = c.peak
		}
		for name, body := range files {
			if err := os.WriteFile(filepath.Join(path, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		tree := &runCgroup{path: path, pause: func() {}}
		_ = tree.end(true) // the planted folder is no cgroup: its kill and removal fail, and the peak is read first
		got, read := peakOf(nil, nil, tree, true)
		if got != c.want || read != c.read {
			t.Errorf("memory.peak %q: Peak %d, PeakRead %v, want %d and %v", c.peak, got, read, c.want, c.read)
		}
	}
}

// nulNamedFile is this test's program opened by a descriptor of its own under a name that holds a
// NUL, as os.NewFile keeps any name it is given, closed when t ends.
func nulNamedFile(t *testing.T) *os.File {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fd, err := syscall.Open(exe, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	f := os.NewFile(uintptr(fd), exe+"\x00x")
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// TestHoldWritesTheThreeFilesOfTheLimitAndReadsMemoryMaxBack: hold, on a planted folder that stands
// for a run's cgroup with the three files, writes the limit to memory.max, 0 to memory.swap.max and 1
// to memory.oom.group and keeps memory.max as read back; with 0 it writes nothing; and with one of
// the three files missing it is an error that matches os.ErrNotExist and creates no file, as a
// cgroup without the memory controller's files holds no limit.
//
// What it costs when this fails: a run asked to hold a tree to TreeMemory writes another limit than
// the one asked, leaves swap or the group kill off, or goes on without a limit where a file is gone.
func TestHoldWritesTheThreeFilesOfTheLimitAndReadsMemoryMaxBack(t *testing.T) {
	t.Parallel()
	files := []string{"memory.max", "memory.swap.max", "memory.oom.group"}
	plant := func(skip string) string {
		dir := t.TempDir()
		for _, name := range files {
			if name == skip {
				continue
			}
			if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	read := func(dir, name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "(" + err.Error() + ")"
		}
		return string(b)
	}
	dir := plant("")
	c := &runCgroup{path: dir}
	if err := c.hold(64 << 20); err != nil {
		t.Fatalf("hold of 64 MiB: %v", err)
	}
	if got := [3]string{read(dir, "memory.max"), read(dir, "memory.swap.max"), read(dir, "memory.oom.group")}; got != [3]string{"67108864", "0", "1"} || c.max != "67108864" {
		t.Errorf("hold of 64 MiB wrote memory.max, memory.swap.max and memory.oom.group as %q, and kept max %q, want 67108864, 0, 1 and 67108864", got, c.max)
	}
	untouched := plant("")
	if err := (&runCgroup{path: untouched}).hold(0); err != nil || read(untouched, "memory.max") != "" {
		t.Errorf("hold of 0: %v, memory.max %q, want no error and nothing written", err, read(untouched, "memory.max"))
	}
	for _, gone := range files {
		d := plant(gone)
		if err := (&runCgroup{path: d}).hold(64 << 20); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("hold with %s missing: %v, want an error that matches os.ErrNotExist", gone, err)
		}
		if _, err := os.Stat(filepath.Join(d, gone)); err == nil {
			t.Errorf("hold with %s missing created it", gone)
		}
	}
}

// TestAContainedRunWhoseTreeEndedBeforeItsCheckReadAnOomEndsLimitWhenTheOomKilled: limitPassed, on a
// planted memory.events.local, is true for a Contained t whose own cgroup counts an oom and a kill of
// a process of its own, though the group kill count is 0, as where the tree wrote 0 to
// memory.oom.group and the kernel killed its pid 1, ending the tree before the watch read
// memory.events and made its own kill; it is false for such a t that is not Contained, for an oom
// that killed nothing (an allocation that got ENOMEM), and for a kill with no oom of its own, as one
// by a cap above t.
//
// What it costs when this fails: a Contained tree whose pid 1 the kernel's oom kill takes ends as if
// its program had crashed, a signal and no `limit`, on the runs where the tree ends before the check
// reads memory.events, or a tree that handled an ENOMEM, or that a cap
// above it killed, ends `limit`.
func TestAContainedRunWhoseTreeEndedBeforeItsCheckReadAnOomEndsLimitWhenTheOomKilled(t *testing.T) {
	t.Parallel()
	for _, r := range []struct {
		what      string
		contained bool
		local     string
		want      bool
	}{
		{"a Contained tree whose own oom killed a process of its own", true, "oom 1\noom_kill 1\noom_group_kill 0\n", true},
		{"a tree that is not Contained, with the same counts", false, "oom 1\noom_kill 1\noom_group_kill 0\n", false},
		{"a Contained tree whose oom killed nothing", true, "oom 1\noom_kill 0\noom_group_kill 0\n", false},
		{"a Contained tree with a kill and no oom of its own", true, "oom 0\noom_kill 1\noom_group_kill 0\n", false},
		{"a Contained tree with no oom and no kill", true, "oom 0\noom_kill 0\noom_group_kill 0\n", false},
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "memory.events.local"), []byte(r.local), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := (&runCgroup{path: dir, contained: r.contained}).limitPassed(false); err != nil || got != r.want {
			t.Errorf("%s: limitPassed %t, %v, want %t", r.what, got, err, r.want)
		}
	}
}
