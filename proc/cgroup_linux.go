//go:build linux

package proc

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix" //nolint:depguard // arch.ports.23: a cgroup's mount is told by statfs's magic, its delegation by an xattr, and its writers by faccessat with AT_EACCESS, which syscall does not give
)

// peakKind is what this system's Result.Peak counts.
const peakKind = PeakTreeCharged

// limitsHere is which limits this system holds: TreeMemory, on a cgroup of the run's own.
const limitsHere = limCgroup

// Where this system keeps its cgroups, this process's own, and the mounts this process sees.
const (
	cgroupMount = "/sys/fs/cgroup"
	selfCgroup  = "/proc/self/cgroup"
	selfMounts  = "/proc/self/mountinfo"
)

// delegateMark is the xattr systemd sets, to delegateValue, on a cgroup it delegates (systemd 251
// on, on Linux 5.7 on): the mark that the cgroup's subtree is its user's to manage.
const (
	delegateMark  = "user.delegate"
	delegateValue = "1"
)

// maxCgroupFile is the most of a file the check reads: a cgroup's file is a line or a few, and the
// mountinfo a line a mount.
const maxCgroupFile = 1 << 20

// The reasons a run cannot have a cgroup of its own here, each a refusal of TreeMemory by
// errNoTreeMemory, which wraps ErrRefused and errors.ErrUnsupported.
var (
	errNoCgroup2     = errors.New("there is no cgroup v2 mounted at " + cgroupMount)
	errNoProcCgroup  = errors.New(procMount + ", where " + selfCgroup + " names this process's cgroup, is not a proc mount but another folder or a link, so a file by that name there would place the run's cgroup in the kernel's stead")
	errNoCgroupLine  = errors.New("this process's cgroup is not named in " + selfCgroup + " by one cgroup v2 line with an absolute, clean path")
	errCgroupRoot    = errors.New("this process is in the root cgroup, which has no parent to make a run's cgroup beside it in")
	errNotDelegated  = errors.New("the parent of this process's cgroup carries no " + delegateMark + " mark of " + delegateValue + ", which systemd sets on a cgroup it delegates (Delegate=yes)")
	errNoMemory      = errors.New("the parent of this process's cgroup does not list the memory controller in its cgroup.controllers")
	errNotWritable   = errors.New("this process cannot write the parent of its cgroup, that cgroup's cgroup.procs or its cgroup.subtree_control")
	errNoCgroupKill  = errors.New("the kernel gives no cgroup.kill, which ends a run's cgroup whole (Linux 5.14)")
	errNoSwapMax     = errors.New("the kernel gives no memory.swap.max, so a run's tree could pass its memory limit into swap")
	errSubtreeMemory = errors.New("the memory controller could not be given to the children of the parent of this process's cgroup, by its cgroup.subtree_control: a cgroup that holds a process gives its children none, so the caller runs in a leaf of the delegated cgroup (DelegateSubgroup=)")
)

// errRootCaller, errNoNsDelegate, errNoNsLimit, errNoProcMount and errNoCgroupMount are why
// errNoContained refuses a Contained.
var (
	errRootCaller   = errors.New("the caller's effective uid is 0, which the tree would keep: the kernel lets uid 0 write its global sysctls by their owner bits, with no capability, kernel.core_pattern among them, and so run a helper of the tree's choosing as root with every capability, outside every namespace of the tree; a caller that would contain a run takes another uid first")
	errNoNsDelegate = errors.New("the cgroup v2 mount at " + cgroupMount + " has no nsdelegate, without which a cgroup namespace keeps no process of it in its cgroup")
	errNoNsLimit    = errors.New("the run's shim could not write 0 to user.max_cgroup_namespaces in the tree's user namespace, without which a process of the tree may make a cgroup namespace rooted below the run's cgroup and lift its limit")
	errNoProcMount  = errors.New("the run's shim could not mount a proc of the tree's pid namespace on its /proc, without which the tree's /proc lists, and opens, processes outside it")
	// errNoCgroupMount: the tree's cgroup mounts were not covered.
	errNoCgroupMount = errors.New("the run's shim could not make the tree's mounts private, or mount a cgroup v2 of the tree's cgroup namespace over each cgroup mount in them, without which the tree writes the caller user's other cgroups, the caller's own and its parent's among them, so may freeze or kill the caller and then lift its own limit unkilled")
)

// errContainedFolder, errContainedHanded and errContainedUnchecked refuse a Contained for what the
// run gives the tree, not for this system, so they wrap ErrRefused alone, not
// errors.ErrUnsupported; errContainedExec too, as by its exec the shim took every step.
var (
	errContainedExec      = fmt.Errorf("%w: Limits.Contained: the shim took every step of containment, then could not exec the program, as a Path that names no file or no program, so the Cmd is the run's own", ErrRefused)
	errContainedFolder    = fmt.Errorf("%w: Limits.Contained: the run's folder, its Dir or this process's own, could not be entered by its path once the shim covered the cgroup mounts, as one in a cgroup mount, one through a proc link to a folder there or on no mount of the tree's, one removed, one this user cannot enter, or one below a folder this user cannot search, without which the tree keeps a folder from which \"..\" may reach this process's cgroup", ErrRefused)
	errContainedHanded    = fmt.Errorf("%w: Limits.Contained: the run hands the tree a directory, as its Stdin or among its ExtraFiles, which stays on this process's mounts, under no cover, where \"..\" reaches this process's cgroup", ErrRefused)
	errContainedUnchecked = fmt.Errorf("%w: Limits.Contained: a descriptor the run hands the tree could not be checked, as its Stdin, Stdout, Stderr, File or among its ExtraFiles, so the shim cannot tell it is no directory, from which \"..\" reaches this process's cgroup", ErrRefused)
)

// errShimEnded is why ErrRefused refuses a Contained run whose shim never said it was ready to exec:
// the process ended before it took its steps, or the start skipped it, and what ran was not held.
var errShimEnded = errors.New("the run's shim ended, or was skipped, before it took its steps: an init function of the caller's binary that runs before go-proc's, with the shim's empty environment, may end it")

// errShimForged is why ErrRefused refuses a Contained run whose report said shimReady while a process of
// the run's cgroup held a capability or had not set no_new_privs: a program that skipped the shim and wrote the byte itself.
var errShimForged = errors.New("the run's report said the shim was ready, but a process of the run's cgroup holds a capability or has not set no_new_privs, as one that skipped the shim does: a start that skipped it, and a program that writes the ready byte itself")

// errLimitLost ends a Contained run whose cgroup lost its limit while the tree ran (as memory leaves
// it at the tree's write of -memory to its parent's cgroup.subtree_control), never WhyLimit.
var errLimitLost = errors.New("proc: the run's cgroup lost the TreeMemory limit the run set, and the run killed its tree")

// errInRun refuses a cgroup, for a TreeMemory or a ReadPeak, from within a run's (whose limit, kill
// and peak it would escape), never running without it, so the nested caller decides.
var errInRun = notHeld{fmt.Errorf("%w: this process is in a run's cgroup that proc made, and a run's cgroup made from it would be out of that run's limit, its kill and its peak; a run is not nested in a run here: %w", ErrRefused, errors.ErrUnsupported)}

// cgroupFS is the calls the delegation check reads and writes cgroups by: statfs's type, a small
// file whole (at most maxCgroupFile bytes), an xattr, faccessat by the effective ids, and a write to
// an existing file. It is this system's own outside a test.
type cgroupFS interface {
	magic(path string) (int64, error)
	read(path string) ([]byte, error)
	xattr(path, name string) ([]byte, error)
	access(path string, mode uint32) error
	write(path, value string) error
}

// cgroupHost is where the delegation check reads: the cgroup v2 mount, the file that names this
// process's cgroup, the proc mount that file is below, the file of the mounts it sees, the file a
// Contained run's shim writes 0 to, where it mounts the tree's proc and the tree's file of mounts it
// reads the cgroup mounts from, the binary it runs as (its zero is the proc mount's self/exe), the
// shim's argv[0] (shimMark, or shimEINTR), the calls it reads them by, and the caller's effective
// uid. The zero one is this system's own; a test of the package gives a run another by Cmd.seam.
type cgroupHost struct {
	mount, self, proc, mounts, limit, tree, cgroups, exe, shim string
	fs                                                         cgroupFS
	euid                                                       func() int
}

// files is h with this system's own in place of each field left zero.
func (h cgroupHost) files() cgroupHost {
	h.mount, h.self = cmp.Or(h.mount, cgroupMount), cmp.Or(h.self, selfCgroup)
	h.proc, h.mounts = cmp.Or(h.proc, procMount), cmp.Or(h.mounts, selfMounts)
	h.limit, h.tree = cmp.Or(h.limit, h.proc+"/sys/user/max_cgroup_namespaces"), cmp.Or(h.tree, procMount)
	h.cgroups, h.shim = cmp.Or(h.cgroups, h.tree+"/self/mountinfo"), cmp.Or(h.shim, shimMark)
	if h.fs == nil {
		h.fs = sysCgroups{}
	}
	if h.euid == nil {
		h.euid = os.Geteuid
	}
	return h
}

// sysCgroups is this system's cgroupFS.
type sysCgroups struct{}

func (sysCgroups) magic(path string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Type), nil //nolint:unconvert // Statfs_t.Type is int32 or uint32 on some architectures
}

func (sysCgroups) read(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // G304: a cgroup file beneath the cgroup mount, or this process's own cgroup file
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, maxCgroupFile+1))
	if err = errors.Join(err, f.Close()); err != nil {
		return nil, err
	}
	if len(b) > maxCgroupFile {
		return nil, fmt.Errorf("%s holds more than %d bytes", path, maxCgroupFile)
	}
	return b, nil
}

func (sysCgroups) xattr(path, name string) ([]byte, error) {
	buf := make([]byte, 64)
	n, err := unix.Getxattr(path, name, buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func (sysCgroups) access(path string, mode uint32) error {
	return unix.Faccessat(unix.AT_FDCWD, path, mode, unix.AT_EACCESS)
}

func (sysCgroups) write(path, value string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0) //nolint:gosec // G304: a cgroup file beneath the delegated cgroup the check found
	if err != nil {
		return err
	}
	_, err = f.WriteString(value)
	return errors.Join(err, f.Close())
}

// notHere is why, as TreeMemory's refusal, with what was met, when anything was.
func notHere(why, met error) error {
	if met != nil {
		return fmt.Errorf("%w: %w (%w)", errNoTreeMemory, why, met)
	}
	return fmt.Errorf("%w: %w", errNoTreeMemory, why)
}

// ownCgroup is this process's cgroup v2 path, read from b, the file that names it: the path of its one
// line of hierarchy 0 with no controllers, absolute and clean. A cgroup v1 line beside it is left alone.
func ownCgroup(b []byte) (string, bool) {
	var found []string
	for line := range strings.Lines(string(b)) {
		if p, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), "0::"); ok {
			found = append(found, p)
		}
	}
	if len(found) != 1 {
		return "", false
	}
	p := found[0]
	return p, strings.HasPrefix(p, "/") && filepath.Clean(p) == p
}

// inRun is whether own, a cgroup v2 path, is a run's cgroup or beneath one: whether any of its names
// is one runName gives. cgroup v2 renames no cgroup, so a run's keeps its name.
func inRun(own string) bool {
	for name := range strings.SplitSeq(own, "/") {
		hexed, ok := strings.CutPrefix(name, runPrefix)
		if ok && len(hexed) == 2*runBytes && strings.Trim(hexed, "0123456789abcdef") == "" {
			return true
		}
	}
	return false
}

// fields is whether b, a cgroup file of words split by spaces, holds word.
func fields(b []byte, word string) bool {
	return slices.Contains(strings.Fields(string(b)), word)
}

// parent is the cgroup a run's own cgroup is made in: the parent P of this process's cgroup, once P
// is found on a cgroup v2 mount, named by the kernel's own file below a proc mount, carrying
// systemd's delegation mark, listing the memory controller, writable by this process with its
// cgroup.procs and cgroup.subtree_control, and giving cgroup.kill and memory.swap.max; memory is
// then given to P's children. The run's goes beside the caller's cgroup, as a cgroup that holds a
// process gives its children no controller. Each check that fails is a refusal that names it.
func (h cgroupHost) parent() (string, error) {
	p, err := h.delegatedParent()
	if err != nil {
		return "", err
	}
	return h.files().giveMemory(p)
}

// delegatedParent is parent with no write: P once each check but the giving of memory passes.
func (h cgroupHost) delegatedParent() (string, error) {
	h = h.files()
	if m, err := h.fs.magic(h.mount); err != nil || m != unix.CGROUP2_SUPER_MAGIC {
		return "", notHere(errNoCgroup2, err)
	}
	// The file that names this process's cgroup is the kernel's only below a proc mount (procMounted);
	// a planted one would place the run. The check and the read go by path, not one descriptor as
	// procOwn's do, so three routes are not guarded here: a file bound below /proc, a change of what
	// /proc names between them (a mount, or a rename from another mount namespace), and a file
	// system whose server answers statfs as proc.
	if !procMounted(h.proc) {
		return "", notHere(errNoProcCgroup, nil)
	}
	b, err := h.fs.read(h.self)
	if err != nil {
		return "", notHere(errNoCgroupLine, err)
	}
	own, ok := ownCgroup(b)
	switch {
	case !ok:
		return "", notHere(errNoCgroupLine, nil)
	case own == "/":
		return "", notHere(errCgroupRoot, nil)
	case inRun(own):
		return "", errInRun
	}
	p := filepath.Join(h.mount, filepath.Dir(own))
	if v, err := h.fs.xattr(p, delegateMark); err != nil || string(v) != delegateValue {
		return "", notHere(fmt.Errorf("%w: %s", errNotDelegated, p), err)
	}
	if ctl, err := h.fs.read(filepath.Join(p, "cgroup.controllers")); err != nil || !fields(ctl, "memory") {
		return "", notHere(fmt.Errorf("%w: %s", errNoMemory, p), err)
	}
	for _, w := range []struct {
		path string
		mode uint32
	}{{p, unix.W_OK | unix.X_OK}, {filepath.Join(p, "cgroup.procs"), unix.W_OK}, {filepath.Join(p, "cgroup.subtree_control"), unix.W_OK}} {
		if err := h.fs.access(w.path, w.mode); err != nil {
			return "", notHere(fmt.Errorf("%w: %s", errNotWritable, w.path), err)
		}
	}
	if err := h.fs.access(filepath.Join(p, "cgroup.kill"), unix.F_OK); err != nil {
		return "", notHere(errNoCgroupKill, err)
	}
	if err := h.fs.access(filepath.Join(p, "memory.swap.max"), unix.F_OK); err != nil {
		return "", notHere(errNoSwapMax, err)
	}
	return p, nil
}

// giveMemory gives p's children the memory controller by "+memory" in its cgroup.subtree_control,
// where they have it not yet, and is p; or is "" and errSubtreeMemory's refusal, as parent's are.
func (h cgroupHost) giveMemory(p string) (string, error) {
	sub := filepath.Join(p, "cgroup.subtree_control")
	given, err := h.fs.read(sub)
	if err == nil && !fields(given, "memory") {
		err = h.fs.write(sub, "+memory")
	}
	if err != nil {
		return "", notHere(fmt.Errorf("%w: %s", errSubtreeMemory, p), err)
	}
	return p, nil
}

// memoryGivable is giveMemory with no write: nil where p's children have the memory controller, or
// where p holds no process of its own, which would keep it from them; else errSubtreeMemory's
// refusal. Whether the kernel would take the write for another reason is not asked.
func (h cgroupHost) memoryGivable(p string) error {
	given, err := h.fs.read(filepath.Join(p, "cgroup.subtree_control"))
	if err == nil && fields(given, "memory") {
		return nil
	}
	var procs []byte
	if err == nil {
		procs, err = h.fs.read(filepath.Join(p, "cgroup.procs"))
	}
	if err == nil && len(strings.TrimSpace(string(procs))) != 0 {
		err = errHeldProcess
	}
	if err != nil {
		return notHere(fmt.Errorf("%w: %s", errSubtreeMemory, p), err)
	}
	return nil
}

// errHeldProcess is why memoryGivable finds the controller kept from p's children.
var errHeldProcess = errors.New("the parent holds a process of its own")

// notRoot is nil where the caller's effective uid is other than 0, or Contained's refusal. The tree's
// user namespace maps the caller's uid to itself, so a root caller's tree is uid 0 in the initial
// namespace, which the kernel checks no capability of to write a global sysctl owned by root: with
// no capability and with no_new_privs, the tree would write kernel.core_pattern, crash, and have the
// kernel run its helper as root outside the tree, which may then lift the run's memory.max.
func (h cgroupHost) notRoot() error {
	if h.files().euid() == 0 {
		return fmt.Errorf("%w: %w", errNoContained, errRootCaller)
	}
	return nil
}

// nsDelegated is nil where the last mountinfo line at h's cgroup v2 mount is cgroup2 with nsdelegate,
// or Contained's refusal. With nsdelegate, set by the init namespace's mount, a process in a cgroup
// namespace moves no process across its root, and writes no file of the root's but those delegated.
func (h cgroupHost) nsDelegated() error {
	h = h.files()
	b, err := h.fs.read(h.mounts)
	if err != nil {
		return fmt.Errorf("%w: %w (%w)", errNoContained, errNoNsDelegate, err)
	}
	seen := false
	for line := range strings.Lines(string(b)) {
		// id, parent, device, root, mount point, options, optional fields, -, type, source, superblock options
		f := strings.Fields(line)
		if len(f) < 10 || f[4] != h.mount {
			continue
		}
		i := 6 + slices.Index(f[6:], "-")
		seen = i >= 6 && len(f) > i+3 && f[i+1] == "cgroup2" && slices.Contains(strings.Split(f[i+3], ","), "nsdelegate")
	}
	if !seen {
		return fmt.Errorf("%w: %w", errNoContained, errNoNsDelegate)
	}
	return nil
}

// A runCgroup is the cgroup of one run's own, made for it beside this process's cgroup and removed
// once its tree is gone. The program is started into it, so every process the tree starts is in it
// from its first instruction.
type runCgroup struct {
	// path is the cgroup's folder.
	path string
	// dir is the cgroup's folder, open for the start (CgroupFD), closed once the start is done.
	dir *os.File
	// limited is whether the run set memory.max; passed is whether that limit ended the tree (end).
	limited, passed bool
	// max is memory.max as read once set, rounded to pages, which a contained t's check reads back.
	max string
	// peak is memory.peak, read at end when the run asked, and peakRead whether it was.
	peak     int64
	peakRead bool
	// pause is the wait between two tries at removing the cgroup, a sleep of killPause.
	pause func()
	// contained is whether the program starts in namespaces of its own and the run kills the tree on
	// its oom or a loss of its limit: watch is the shared inotify, wds t's two watches there, and
	// watchErr what ended the check, and oomKilled whether the check wrote cgroup.kill on an oom,
	// both under watchesMu. shim is the pipe its shim reports on, the memfd that holds the program's
	// environment and the end of the go-ahead's pipe the shim reads, and goAhead the end the caller
	// writes shimGo to, open from made to started; exe is the binary the shim runs as, limit
	// the file it writes 0 to, tree where it mounts proc, cgroups the file of mounts it reads there,
	// mark its argv[0].
	contained                       bool
	watch                           *watch
	wds                             [2]int
	watchErr                        error
	oomKilled                       bool
	shim                            [4]*os.File
	goAhead                         *os.File
	exe, limit, tree, cgroups, mark string
	// proc is the host's proc mount, where started checks the run's processes.
	proc string
	// read reads the files of proc that coversOf does; nil is os.ReadFile (a test's seam).
	read func(string) ([]byte, error)
	// magic is the file system type of a path, nil is statfs (a test's seam).
	magic func(string) (int64, error)
	// signal sends a signal to the pid shimTook's refusal kills, nil is syscall.Kill (a test's seam).
	signal func(pid int, sig syscall.Signal) error
	// ctx is the run's own context, which bounds the wait of freeze for the frozen state.
	ctx context.Context
}

// cgroupFor is the cgroup c's run takes, or nil: one held to c's TreeMemory with no swap, contained
// when c asks, or one that only reads c's peak. A limit h cannot hold is refused by name; a run that
// only asks for its peak runs without a cgroup instead, but not from a run's (errInRun).
func cgroupFor(c Cmd, h cgroupHost) (*runCgroup, error) {
	memory := c.Limits.TreeMemory
	if memory == 0 && !c.ReadPeak {
		return nil, nil
	}
	tree, err := h.made(memory, c.Limits.Contained)
	if err != nil && memory == 0 && !errors.Is(err, errInRun) {
		// A reading is not a limit: the run goes without it.
		return nil, nil
	}
	return tree, err
}

// heldHere is whether a run of c reads its peak by a cgroup of its own here, and the refusal
// cgroupFor would give c, found as cgroupFor finds them but with nothing made or written.
func heldHere(c Cmd, h cgroupHost) (bool, error) {
	memory := c.Limits.TreeMemory
	if memory == 0 && !c.ReadPeak {
		return false, nil
	}
	err := h.containable(c.Limits.Contained)
	if err == nil {
		var p string
		if p, err = h.delegatedParent(); err == nil {
			err = h.files().memoryGivable(p)
		}
	}
	if err != nil && memory == 0 && !errors.Is(err, errInRun) {
		return false, nil
	}
	return err == nil, err
}

// containable is nil where a run is not contained, or its caller and mount pass Contained's checks
// that come before the cgroup is made; else the first refusal.
func (h cgroupHost) containable(contained bool) error {
	if !contained {
		return nil
	}
	if err := h.notRoot(); err != nil {
		return err
	}
	return h.nsDelegated()
}

// made is a new cgroup of a run's own in h's delegated parent, held to memory bytes (0 is none),
// contained when asked, and open for the start; a Contained is asked of the caller's uid and the
// mount first.
func (h cgroupHost) made(memory uint64, contained bool) (*runCgroup, error) {
	// The mountinfo is the kernel's only below a proc mount, as the file parent reads is; parent
	// refuses a /proc that is not one, so a planted mountinfo that claims nsdelegate starts nothing.
	if err := h.containable(contained); err != nil {
		return nil, err
	}
	p, err := h.parent()
	if err != nil {
		return nil, err
	}
	h = h.files()
	path := filepath.Join(p, runName())
	if err := os.Mkdir(path, 0o700); err != nil {
		return nil, fmt.Errorf("%w: making the run's cgroup: %w", errRefused, err)
	}
	t := &runCgroup{path: path, limited: memory != 0, pause: killPaused, contained: contained}
	if err := t.hold(memory); err != nil {
		return nil, errors.Join(fmt.Errorf("%w: setting the run's cgroup: %w", errRefused, err), t.remove())
	}
	if contained {
		if err := t.watchOom(); err != nil {
			return nil, errors.Join(fmt.Errorf("%w: watching the run's cgroup's memory.events: %w", errRefused, err), t.remove())
		}
		t.exe, t.limit, t.tree, t.cgroups, t.mark = cmp.Or(h.exe, h.proc+"/self/exe"), h.limit, h.tree, h.cgroups, h.shim
		t.proc = h.proc
		t.shim[0], t.shim[1], err = os.Pipe()
		if err == nil {
			t.shim[2], err = memfd()
		}
		if err == nil {
			t.shim[3], t.goAhead, err = os.Pipe()
		}
		if err != nil {
			return nil, errors.Join(fmt.Errorf("%w: making the shim's report, environment or go-ahead: %w", errRefused, err), t.started(), t.unwatch(), t.remove())
		}
	}
	dir, err := os.OpenFile(path, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0) //nolint:gosec // G304: the run's own cgroup, made above
	if err != nil {
		return nil, errors.Join(fmt.Errorf("%w: opening the run's cgroup: %w", errRefused, err), t.started(), t.unwatch(), t.remove())
	}
	t.dir = dir
	return t, nil
}

// A run's cgroup is named runPrefix and runBytes random bytes in lowercase hex.
const (
	runPrefix = "proc-"
	runBytes  = 8
)

// killPaused waits killPause, between two tries at removing a cgroup.
func killPaused() { time.Sleep(killPause) }

// runName is a name no other run's cgroup has: proc- and 16 random hex digits.
func runName() string {
	var b [runBytes]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error: it ends the program when it cannot read
	return runPrefix + hex.EncodeToString(b[:])
}

// hold sets t's memory.max to memory, its memory.swap.max to 0 and its memory.oom.group to 1, so a
// tree that passes memory is killed whole, as Windows ends the job's. memory 0 sets nothing.
func (t *runCgroup) hold(memory uint64) error {
	if memory == 0 {
		return nil
	}
	// The cgroup is empty, so no order is owed among the three; a file that is not there is an error,
	// as a kernel without the memory controller's files holds no limit.
	for _, w := range [...][2]string{{"memory.swap.max", "0"}, {"memory.max", strconv.FormatUint(memory, 10)}, {"memory.oom.group", "1"}} {
		if err := (sysCgroups{}).write(filepath.Join(t.path, w[0]), w[1]); err != nil {
			return err
		}
	}
	b, err := sysCgroups{}.read(filepath.Join(t.path, "memory.max"))
	t.max = string(b)
	return err
}

// watching is the one inotify instance all of this process's Contained runs share, under
// watchesMu, not one each against the user's fs.inotify.max_user_instances.
var (
	watchesMu sync.Mutex //nolint:gochecknoglobals // one instance for the process, as Run is a function, not a value one holds
	watching  *watch     //nolint:gochecknoglobals // the instance watchesMu guards
)

// watch is an inotify instance, under watchesMu: runs those it checks, n those not unwatched,
// parents the runs that share each parent's watch; done closes as its goroutine returns.
type watch struct {
	f       *os.File
	fd      int
	runs    map[*runCgroup]bool
	parents map[int]int
	n       int
	done    chan struct{}
}

// watchOom has the run kill t's tree by cgroup.kill once memory.events counts an oom, as a tree may
// write 0 to memory.oom.group, or once t loses its limit (held). It checks at each inotify write of
// t's memory.events or of its parent's cgroup.subtree_control, by which alone memory leaves t, and
// of which memory.events tells nothing. A check or read that fails kills the tree too: it fails
// closed. unwatch ends t's check.
func (t *runCgroup) watchOom() error {
	watchesMu.Lock()
	defer watchesMu.Unlock()
	w := watching
	if w == nil {
		fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
		if err != nil {
			return err
		}
		w = &watch{f: os.NewFile(uintptr(fd), "inotify"), fd: fd, runs: map[*runCgroup]bool{}, parents: map[int]int{}, done: make(chan struct{})}
		watching = w
		go w.wake()
	}
	t.watch, t.wds = w, [2]int{-1, -1}
	w.n++
	for i, f := range []string{filepath.Join(t.path, "memory.events"), filepath.Join(filepath.Dir(t.path), "cgroup.subtree_control")} {
		wd, err := unix.InotifyAddWatch(w.fd, f, unix.IN_MODIFY)
		if err != nil {
			return errors.Join(err, w.leave(t))
		}
		t.wds[i] = wd
	}
	w.parents[t.wds[1]]++
	w.runs[t] = true
	return nil
}

// wake checks w's runs at each read of w until it is closed; a read that fails else kills each tree.
func (w *watch) wake() {
	defer close(w.done)
	buf := make([]byte, 4096)
	for {
		_, err := w.f.Read(buf)
		watchesMu.Lock()
		for t := range w.runs {
			ended, why := true, error(nil)
			if err == nil {
				ended, why = t.check()
			} else if !errors.Is(err, os.ErrClosed) {
				why = errors.Join(fmt.Errorf("%w: watching the run's cgroup: %w", errLimitLost, err), t.kill())
			}
			if ended {
				t.watchErr = why
				delete(w.runs, t)
			}
		}
		if err != nil && watching == w {
			watching = nil
		}
		watchesMu.Unlock()
		if err != nil {
			return
		}
	}
}

// check kills t's tree and ends once t loses its limit, counts an oom, or cannot be read; why is why.
func (t *runCgroup) check() (ended bool, why error) {
	if err := t.held(); err != nil {
		return true, errors.Join(err, t.kill())
	}
	oomed, err := t.oomed()
	if err != nil {
		return true, errors.Join(fmt.Errorf("%w: reading the run's cgroup's memory.events: %w", errLimitLost, err), t.kill())
	}
	if oomed {
		err := t.kill()
		t.oomKilled = err == nil
		return true, err
	}
	return false, nil
}

// kill kills every process in t and beneath it, by cgroup.kill, which no controller takes away.
func (t *runCgroup) kill() error {
	return sysCgroups{}.write(filepath.Join(t.path, "cgroup.kill"), "1")
}

// held is nil while memory is in t's cgroup.controllers and each file hold wrote reads what it wrote:
// t's memory.max the run's limit, its memory.swap.max 0 and its memory.oom.group 1; else errLimitLost,
// naming what t lost and, of a file, what it reads.
func (t *runCgroup) held() error {
	b, err := sysCgroups{}.read(filepath.Join(t.path, "cgroup.controllers"))
	m, merr := sysCgroups{}.read(filepath.Join(t.path, "memory.max"))
	switch {
	case err != nil:
		return fmt.Errorf("%w: reading the run's cgroup's controllers: %w", errLimitLost, err)
	case !fields(b, "memory"):
		return fmt.Errorf("%w: the memory controller left the run's cgroup, as a write of -memory to %s takes it", errLimitLost, filepath.Join(filepath.Dir(t.path), "cgroup.subtree_control"))
	case merr != nil:
		return fmt.Errorf("%w: reading the run's cgroup's memory.max: %w", errLimitLost, merr)
	case string(m) != t.max:
		return fmt.Errorf("%w: the run's cgroup's memory.max reads %q, not the %q the run set", errLimitLost, strings.TrimSpace(string(m)), strings.TrimSpace(t.max))
	}
	for _, f := range [...][2]string{{"memory.swap.max", "0"}, {"memory.oom.group", "1"}} {
		b, err := sysCgroups{}.read(filepath.Join(t.path, f[0]))
		if err != nil {
			return fmt.Errorf("%w: reading the run's cgroup's %s: %w", errLimitLost, f[0], err)
		}
		if strings.TrimSpace(string(b)) != f[1] {
			return fmt.Errorf("%w: the run's cgroup's %s reads %q, not the %q the run set", errLimitLost, f[0], strings.TrimSpace(string(b)), f[1])
		}
	}
	return nil
}

// leave takes t off w, under watchesMu, removing its watches by number (the file may be gone), a
// parent's once no run shares it, and closing w once t was its last run.
func (w *watch) leave(t *runCgroup) error {
	delete(w.runs, t)
	if t.wds[0] >= 0 {
		_, _ = unix.InotifyRmWatch(w.fd, uint32(t.wds[0])) //nolint:errcheck,gosec // the wd may be gone with its file; G115: a wd inotify gave is not negative
	}
	if t.wds[1] >= 0 {
		if w.parents[t.wds[1]]--; w.parents[t.wds[1]] <= 0 {
			delete(w.parents, t.wds[1])
			_, _ = unix.InotifyRmWatch(w.fd, uint32(t.wds[1])) //nolint:errcheck,gosec // as above
		}
	}
	t.watch = nil
	if w.n--; w.n > 0 {
		return nil
	}
	if watching == w {
		watching = nil
	}
	return w.f.Close()
}

// unwatch ends t's check, waiting for the goroutine after a last run: what else ended the check.
func (t *runCgroup) unwatch() error {
	watchesMu.Lock()
	w := t.watch
	if w == nil {
		watchesMu.Unlock()
		return nil
	}
	err := errors.Join(w.leave(t), t.watchErr)
	last := w.n == 0
	watchesMu.Unlock()
	if last {
		<-w.done
	}
	return err
}

// oomed is whether t's memory.events counts an oom (a limit at or beneath t reached), not oom_kill,
// which counts kills for limits above t too. A memory.events gone with memory is an error (it matches
// os.ErrNotExist), and so is one with no oom line, an empty one, and a count that is no number: a
// file that does not say whether the limit was reached is no "no". The check kills on any error;
// what the run reports is limitEnded's.
func (t *runCgroup) oomed() (bool, error) {
	counts, err := t.events("memory.events")
	if err != nil {
		return false, err
	}
	n, ok := counts["oom"]
	if !ok {
		return false, fmt.Errorf("the run's cgroup %s has no oom line in its memory.events", t.path)
	}
	return n > 0, nil
}

// place starts cmd into t by clone3's CLONE_INTO_CGROUP. A contained t starts it in a user namespace
// mapping the caller's effective uid and gid each to itself, without setgroups, and cgroup (rooted
// at t), pid and mount namespaces of its own; through the shim, with CAP_SYS_RESOURCE and
// CAP_SYS_ADMIN ambient, which reports on the descriptor after cmd's others, and covers each cgroup
// mount t.cgroups lists with a cgroup v2 of the tree's namespace, and refuses a folder
// it cannot find again through them and a directory, or one it cannot check, among cmd's
// descriptors. The shim takes cmd's Dir, which the start then leaves empty, so the start enters
// no folder and execs only the shim. The shim starts with
// no environment, as a loader reads one before the shim's first step, LD_PRELOAD among it;
// cmd's is written to the memfd after the report's, which the
// shim's second stage reads and execs the program with, once the go-ahead on the descriptor after it
// says shimGo.
func (t *runCgroup) place(cmd *process) error {
	if t == nil {
		return nil
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	a := cmd.SysProcAttr
	a.UseCgroupFD, a.CgroupFD = true, int(t.dir.Fd())
	if t.contained {
		uid, gid := os.Geteuid(), os.Getegid()
		a.Cloneflags |= syscall.CLONE_NEWUSER | syscall.CLONE_NEWCGROUP | syscall.CLONE_NEWPID | syscall.CLONE_NEWNS
		a.UidMappings = []syscall.SysProcIDMap{{ContainerID: uid, HostID: uid, Size: 1}}
		a.GidMappings = []syscall.SysProcIDMap{{ContainerID: gid, HostID: gid, Size: 1}}
		a.GidMappingsEnableSetgroups = false
		a.AmbientCaps = []uintptr{unix.CAP_SYS_RESOURCE, unix.CAP_SYS_ADMIN}
		var env []byte // Run refused a NUL in Env before (nulRefused)
		for _, v := range cmd.Environ() {
			env = append(append(env, v...), 0)
		}
		if _, err := t.shim[2].WriteAt(env, 0); err != nil {
			return fmt.Errorf("%w: writing the program's environment for the shim: %w", errRefused, err)
		}
		cmd.Env = []string{}
		fd := strconv.Itoa(3 + len(cmd.ExtraFiles)) // the report's, after the program's own
		cmd.ExtraFiles = slices.Concat(cmd.ExtraFiles, t.shim[1:])
		// The shim enters Dir, so the start enters no folder: its errno is never the Cmd's own, and an
		// EACCES is the host's.
		cmd.Args = slices.Concat([]string{t.mark, fd, t.limit, t.tree, t.cgroups, cmd.Path, cmd.Dir}, cmd.Args)
		cmd.Path, cmd.Dir = t.exe, ""
	}
	return nil
}

// memfd is a file in memory, closed on exec, for the program's environment.
func memfd() (*os.File, error) {
	fd, err := unix.MemfdCreate("go-proc-env", unix.MFD_CLOEXEC)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), "go-proc-env"), nil
}

// started closes t's folder, its end of the shim's report and the environment's memfd, then reads
// the report to its end, which the shim's exec closes: a step the shim could not take is a refusal
// of Contained, and a report without shimReady a refusal of the run.
func (t *runCgroup) started() error {
	if t == nil {
		return nil
	}
	var errs []error
	if t.dir != nil {
		errs = append(errs, t.dir.Close())
		t.dir = nil
	}
	if r, w, goAhead := t.shim[0], t.shim[1], t.goAhead; r != nil {
		errs = append(errs, w.Close())
		for _, f := range t.shim[2:] {
			if f != nil {
				errs = append(errs, f.Close())
			}
		}
		t.shim, t.goAhead = [4]*os.File{}, nil
		b, err := io.ReadAll(io.LimitReader(r, 1))
		// A report that says shimReady is a shim's second stage, which waits for the go-ahead, or a
		// program's that skipped the shim and wrote the byte itself, which keeps the report open: the
		// run's cgroup is checked now, and the tree killed if a process of it has not had the shim's
		// steps. The stage execs the program only on the go-ahead, written once the check
		// passed, so the check never reads a step of the program's.
		if err == nil && len(b) == 1 && b[0] == shimReady {
			if took := t.shimTook(); took != nil {
				errs = append(errs, took, t.kill())
			} else if goAhead != nil {
				_, _ = goAhead.Write([]byte{shimGo}) //nolint:errcheck // a stage that is gone reads none, and the run ends as its tree did
			}
		}
		if goAhead != nil {
			errs = append(errs, goAhead.Close())
		}
		rest, restErr := io.ReadAll(io.LimitReader(r, 2))
		b = append(b, rest...)
		errs = append(errs, err, restErr, r.Close(), shimFailed(b))
	}
	return errors.Join(errs...)
}

// shimTook is nil where each thread of t's cgroup, read as its report says shimReady, holds what the
// shim leaves of its steps that the caller can read: no_new_privs set and no capability
// (shimSteps), and the shim's cover over each cgroup mount (coversOf). It reads cgroup.threads of the
// run's cgroup and of each folder below it, a cgroup that file lists the threads of alone, each
// thread by its own id: a thread-group leader that ended with the thread-only exit is a zombie whose
// other threads run on, and capability sets, no_new_privs and mounts are each thread's.
// A program that skipped the shim and wrote the byte itself has CAP_SYS_ADMIN and CAP_SYS_RESOURCE
// ambient, and a helper it forked before it dropped them is a thread of the cgroup too. This
// refuses a program that left a cgroup mount open, or dropped no capability. It does not see a user
// namespace of the program's that rebuilds the cover's shape and root from the caller's mount, a descriptor on the caller's cgroup tree that a process outside the tree holds and
// hands back after ready, a cgroup write made before the byte to a cgroup other than the run's,
// which the tree's kill does not undo, or a thread moved out of the run's cgroup into another of
// the user's cgroups before the byte. It reads each thread's own descriptors, and refuses one on
// a cgroup file system or a mount namespace (fdsOf). A cgroup that
// cannot be read is no pass. The run's cgroup is frozen across the read of its threads, its folders and
// its mounts, and thawed after, so no thread is forked between the list and the check: a forger's
// child that kept its capabilities is a thread of the list, or it never ran. A cgroup that cannot be
// frozen, or thawed, is a refusal of the run. A run refused is killed there, while frozen, by each
// thread-group's id (killGroups).
func (t *runCgroup) shimTook() error {
	if err := t.freeze("1"); err != nil {
		return errors.Join(err, t.freeze("0"))
	}
	err := t.shimTookFrozen()
	if err != nil {
		err = errors.Join(err, t.killGroups())
	}
	if thawed := t.freeze("0"); thawed != nil {
		err = errors.Join(err, thawed)
	}
	return err
}

// bound sets the run's own context, which freeze's wait for the frozen state ends with.
func (t *runCgroup) bound(ctx context.Context) {
	if t != nil {
		t.ctx = ctx
	}
}

// freeze writes state, "1" or "0", to the run's cgroup.freeze, and for "1" waits, a millisecond apart,
// until cgroup.events says frozen 1, which the kernel says once each task of the cgroup is frozen, or
// the run's own context ends: a loaded host, or a task slow to freeze, waits as long as the run does,
// and a run that ended is refused. Its refusals wrap errRefused alone: a cgroup that cannot be frozen
// is no forgery.
func (t *runCgroup) freeze(state string) error {
	if err := t.writeFile(filepath.Join(t.path, "cgroup.freeze"), state); err != nil {
		return fmt.Errorf("%w: the run's cgroup could not be %s to read its threads: %w", errRefused, map[string]string{"1": "frozen", "0": "thawed"}[state], err)
	}
	for state == "1" {
		events, err := t.readFile(filepath.Join(t.path, "cgroup.events"))
		if err != nil {
			return fmt.Errorf("%w: reading the frozen state of the run's cgroup: %w", errRefused, err)
		}
		if slices.Contains(strings.Split(string(events), "\n"), "frozen 1") {
			return nil
		}
		select {
		case <-t.ctx.Done():
			return fmt.Errorf("%w: the run's cgroup was not frozen before the run's context ended: %w", errRefused, context.Cause(t.ctx))
		case <-time.After(time.Millisecond):
		}
	}
	return nil
}

// listed is the ids that file, cgroup.threads or cgroup.procs, lists in the run's cgroup and in each
// folder below it: a cgroup's file lists its own alone.
func (t *runCgroup) listed(file string) ([]string, error) {
	var ids []string
	err := filepath.WalkDir(t.path, func(dir string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		b, err := t.readFile(filepath.Join(dir, file))
		ids = append(ids, strings.Fields(string(b))...)
		return err
	})
	return ids, err
}

// killGroups sends SIGKILL to each thread-group the run's cgroups list in cgroup.procs, by its id and
// not by cgroup.kill, which signals each group's leader alone: a leader that ended with the thread-only
// exit is a zombie that takes no signal, so its other threads outlive the write, and the report stays
// open until the run's Timeout. A group that is gone is no error.
func (t *runCgroup) killGroups() error {
	send := t.signal
	if send == nil {
		send = syscall.Kill
	}
	groups, err := t.listed("cgroup.procs")
	for _, g := range groups {
		if pid, convErr := strconv.Atoi(g); convErr != nil {
			err = errors.Join(err, convErr)
		} else if sendErr := signalPid(send, pid, syscall.SIGKILL); sendErr != nil && !errors.Is(sendErr, syscall.ESRCH) {
			err = errors.Join(err, sendErr)
		}
	}
	return err
}

// shimTookFrozen is shimTook's read of a cgroup that is frozen.
func (t *runCgroup) shimTookFrozen() error {
	threads, err := t.listed("cgroup.threads")
	if err != nil {
		return fmt.Errorf("%w: %w: reading the run's threads: %w", errRefused, errShimForged, err)
	}
	info, err := t.readFile(filepath.Join(t.proc, "self", "mountinfo"))
	var cover string
	if err == nil {
		cover, err = coverRoot(string(info), t.path)
	}
	if err != nil {
		return fmt.Errorf("%w: %w: reading the caller's cgroup mount: %w", errRefused, errShimForged, err)
	}
	for _, tid := range threads {
		why := t.shimTookOf(tid)
		if why == "" {
			why = t.coversOf(tid, cover)
		}
		if why == "" {
			why = t.fdsOf(tid)
		}
		if why != "" {
			return fmt.Errorf("%w: %w: thread %s %s", errRefused, errShimForged, tid, why)
		}
	}
	return nil
}

// fdsOf is "" where no descriptor of the thread tid is on a cgroup file system, of either version,
// wherever its mount is (a detached mount, made by open_tree with OPEN_TREE_CLONE, is in no mountinfo),
// or is a mount namespace, and else which one is and what it is on. It reads the thread's
// own fd folder, as a thread may have a file table of its own. The descriptors of the shim's second stage
// are the report, the environment and the go-ahead, a pipe and a memfd, which are neither, and none of
// the Go runtime's: the runtime of a stage in a cgroup namespace opens the cpu.max it finds under the
// cover and keeps it, a descriptor on a cgroup file system, unless the stage is started with GODEBUG
// containermaxprocs=0 (shimRunEnv); those of the caller's that were not close-on-exec went at its exec. A thread or a descriptor that is gone is
// gone; one that cannot be read is no pass.
func (t *runCgroup) fdsOf(tid string) string {
	dir := filepath.Join(t.proc, tid, "fd")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		return "descriptors could not be read: " + err.Error()
	}
	magic := t.magic
	if magic == nil {
		magic = sysCgroups{}.magic
	}
	for _, e := range entries {
		fd := filepath.Join(dir, e.Name())
		link, err := os.Readlink(fd)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "descriptor " + e.Name() + " could not be read: " + err.Error()
		}
		if strings.HasPrefix(link, "mnt:[") {
			return "holds descriptor " + e.Name() + " on a mount namespace"
		}
		fs, err := magic(fd)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "descriptor " + e.Name() + " could not be checked: " + err.Error()
		}
		if fs == unix.CGROUP2_SUPER_MAGIC || fs == unix.CGROUP_SUPER_MAGIC {
			return "holds descriptor " + e.Name() + " on a cgroup file system"
		}
	}
	return ""
}

// coversTries is how many times coversOf reads a mountinfo that answers EINVAL, a millisecond apart.
const coversTries = 20

// coversOf is "" where the mounts of the process pid hold the shim's cover over each cgroup mount, read
// back from its mountinfo by mount id and parent id (uncoveredCgroups), or where it is gone, and else
// which mount points are open, or why its mountinfo could not be read. A task that is
// ending answers EINVAL: that is gone where its status says zombie or dead, or is no file, and else is
// read again up to coversTries times; one that still answers it, or any other error, is no pass.
func (t *runCgroup) coversOf(pid, cover string) string {
	read := t.readFile
	var b []byte
	var err error
	for try := 1; ; try++ {
		b, err = read(filepath.Join(t.proc, pid, "mountinfo"))
		if !errors.Is(err, syscall.EINVAL) || try == coversTries {
			break
		}
		if status, statusErr := read(filepath.Join(t.proc, pid, "status")); errors.Is(statusErr, os.ErrNotExist) || (statusErr == nil && taskEnded(status)) {
			return ""
		}
		time.Sleep(time.Millisecond)
	}
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		return "mounts could not be read: " + err.Error()
	}
	open, err := uncoveredCgroups(string(b), cover)
	if err != nil {
		return "mounts could not be read: " + err.Error()
	}
	if len(open) > 0 {
		return "keeps a cgroup mount uncovered at " + strings.Join(open, ", ")
	}
	return ""
}

// readFile reads name by t's read seam, or os.ReadFile where there is none.
func (t *runCgroup) readFile(name string) ([]byte, error) {
	if t.read != nil {
		return t.read(name)
	}
	return os.ReadFile(name) //nolint:gosec // G304: a file of the run's cgroup or of the proc mount
}

// writeFile writes data to name, a file of the run's cgroup, by os.WriteFile.
func (t *runCgroup) writeFile(name, data string) error {
	return os.WriteFile(name, []byte(data), 0o600) //nolint:gosec,nolintlint // G703: a file of the run's own cgroup, made by the run; gosec's taint analysis finds it or not by what else the package holds, so the directive is held unused where it does not
}

// taskEnded is true where status, a process's status file, has a State of zombie (Z) or dead (X).
func taskEnded(status []byte) bool {
	for line := range strings.Lines(string(status)) {
		if v, ok := strings.CutPrefix(line, "State:"); ok {
			v = strings.TrimSpace(v)
			return strings.HasPrefix(v, "Z") || strings.HasPrefix(v, "X")
		}
	}
	return false
}

// shimTookOf is what the process pid lacks of what the shim leaves, or "" where it holds it all, or
// is gone.
func (t *runCgroup) shimTookOf(pid string) string {
	status, err := os.ReadFile(filepath.Join(t.proc, pid, "status")) //nolint:gosec // G304: the proc mount's file of a process of the run
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		return "could not be read: " + err.Error()
	}
	noNewPrivs := false
	for line := range strings.Lines(string(status)) {
		k, v, _ := strings.Cut(line, ":")
		switch v = strings.TrimSpace(v); k {
		case "NoNewPrivs":
			noNewPrivs = v == "1"
		case "CapInh", "CapPrm", "CapEff", "CapAmb":
			if strings.Trim(v, "0") != "" {
				return "holds capabilities (" + k + " " + v + ")"
			}
		}
	}
	if !noNewPrivs {
		return "has not set no_new_privs"
	}
	return ""
}

// shimFailed is the refusal a shim's report b gives: nil for shimReady alone, which the shim writes
// just before its exec; a step and its errno, after shimReady or alone, for a step it could not
// take; and for any other report, none among them, a shim that ended before its steps were done,
// as it does where an init of the caller's binary that runs before go-proc's ends the process, or a
// start that skipped the shim.
func shimFailed(b []byte) error {
	if len(b) > 0 && b[0] == shimReady {
		if len(b) == 1 {
			return nil
		}
		b = b[1:]
	}
	if len(b) == 2 {
		errno := syscall.Errno(b[1])
		switch b[0] {
		case shimLimit:
			return fmt.Errorf("%w: %w: %w", errNoContained, errNoNsLimit, errno)
		case shimProc:
			return fmt.Errorf("%w: %w: %w", errNoContained, errNoProcMount, errno)
		case shimCgroup:
			return fmt.Errorf("%w: %w: %w", errNoContained, errNoCgroupMount, errno)
		case shimFolder:
			return fmt.Errorf("%w: %w", errContainedFolder, errno)
		case shimHanded:
			return fmt.Errorf("%w: %w", errContainedHanded, errno)
		case shimUnchecked:
			return fmt.Errorf("%w: %w", errContainedUnchecked, errno)
		case shimPrivs:
			return fmt.Errorf("%w: the shim could not close its other descriptors at the exec, set no_new_privs or clear its capabilities: %w", errNoContained, errno)
		case shimExec:
			return fmt.Errorf("%w: %w", errContainedExec, errno)
		}
	}
	return fmt.Errorf("%w: %w, its report %q", errRefused, errShimEnded, b)
}

// startFailed is err, a failed start of t's tree, for a contained t inside errNoContained where its
// errno is one the clone and its namespaces give a host that refuses them (hostStartErrnos), and
// else inside ErrRefused alone: an errno not known to be the host's is read as the run's own, so a
// caller keeps its limits.
func (t *runCgroup) startFailed(err error) error {
	var errno syscall.Errno
	switch {
	case t == nil || !t.contained:
		return err
	case errors.As(err, &errno) && slices.Contains(hostStartErrnos(), errno):
		return fmt.Errorf("%w: the start failed, as it does where the kernel or a security module refuses an unprivileged user namespace: %w", errNoContained, err)
	}
	return fmt.Errorf("%w: a Contained run's start failed: %w", errRefused, err)
}

// hostStartErrnos are the errnos of a clone, or of its user namespace's maps, on a host that
// refuses the run's namespaces: EPERM (unprivileged user namespaces off, or a security module),
// EINVAL (a flag the kernel lacks), ENOSPC (user.max_user_namespaces reached), ENOSYS, and EACCES
// (a security module, SELinux or AppArmor, that refuses the user namespace; or a cgroup the caller
// may no longer start into, or the caller's own binary, which the shim runs as, on a mount it may
// no longer exec from). A Contained start enters no folder and execs no program of the Cmd's: the
// shim takes Dir and execs Path after the start (place), so none of these is the Cmd's own, and a
// Dir the caller cannot enter is the shim's errContainedFolder, ErrRefused alone.
func hostStartErrnos() []syscall.Errno {
	return []syscall.Errno{syscall.EPERM, syscall.EINVAL, syscall.ENOSPC, syscall.ENOSYS, syscall.EACCES}
}

// passedLimit is whether the memory.max the run set ended t's tree.
func (t *runCgroup) passedLimit() bool { return t != nil && t.passed }

// limitEnded is whether the memory.max the run set ended t's tree: the run's own check killed it,
// or t's memory.events.local (its own counts, none beneath it) has an oom and a kill for it:
// oom_group_kill, the kernel's kill of the tree whole by memory.oom.group, or on a kernel without
// that line (before 5.17, commit b6bf9abb) oom_kill, a process of t's own killed, which there also
// counts a kill of the system's own it does not tell apart. An oom with no kill is an allocation
// that got ENOMEM, or a victim already exiting, and the tree ended as its exit says.
func limitEnded(killed bool, local map[string]uint64) bool {
	if killed {
		return true
	}
	if local["oom"] == 0 {
		return false
	}
	if group, ok := local["oom_group_kill"]; ok {
		return group > 0
	}
	return local["oom_kill"] > 0
}

// limitPassed is limitEnded of t's memory.events.local, read once the tree was waited for. A
// Contained t's own kill of its tree on an oom may not have run when the tree ended: the tree wrote 0
// to memory.oom.group, so the kernel killed one process, and where it was the tree's pid 1 the tree
// ended with it, before the watch read memory.events. A kill of a process of t's own by its own oom,
// an oom and an oom_kill in its memory.events.local, is then the limit's, as the watch's would have
// made it.
func (t *runCgroup) limitPassed(killed bool) (bool, error) {
	local, err := t.events("memory.events.local")
	killed = killed || (t.contained && local["oom"] > 0 && local["oom_kill"] > 0)
	return err == nil && limitEnded(killed, local), err
}

// events is t's memory.events file name, read as its "<name> <count>" lines.
func (t *runCgroup) events(name string) (map[string]uint64, error) {
	b, err := sysCgroups{}.read(filepath.Join(t.path, name))
	if err != nil {
		return nil, err
	}
	counts := map[string]uint64{}
	for line := range strings.Lines(string(b)) {
		k, v, ok := strings.Cut(strings.TrimSuffix(line, "\n"), " ")
		n, err := strconv.ParseUint(v, 10, 64)
		if !ok || err != nil {
			return nil, fmt.Errorf("%s: a line that is no name and count: %q", name, line)
		}
		counts[k] = n
	}
	return counts, nil
}

// end reads, once the tree was waited for, whether the limit the run set ended it (limitEnded) and,
// with readPeak, its peak; then kills what is left of it and removes t. A contained t's watch is
// closed first.
func (t *runCgroup) end(readPeak bool) error {
	if t == nil {
		return nil
	}
	errs := []error{t.started(), t.unwatch()}
	lost := errors.Is(errs[1], errLimitLost)
	if t.contained && !lost {
		// A loss the watch had not yet read: remove, below, kills what is left of the tree.
		errs = append(errs, t.held())
		lost = errors.Is(errs[2], errLimitLost)
	}
	if t.limited && !lost {
		watchesMu.Lock()
		killed := t.oomKilled
		watchesMu.Unlock()
		var err error
		t.passed, err = t.limitPassed(killed)
		errs = append(errs, err)
	}
	if readPeak {
		t.peakFile()
	}
	return errors.Join(append(errs, t.remove())...)
}

// peakFile reads t's memory.peak into peak and peakRead. A tree that ran held some memory, so a
// file that is missing (before Linux 5.19) or unreadable, that holds "max", 0, a number past
// math.MaxInt64 or no number is a peak not read: peakRead stays false, with no error.
func (t *runCgroup) peakFile() {
	b, err := sysCgroups{}.read(filepath.Join(t.path, "memory.peak"))
	if err != nil {
		return
	}
	if v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64); err == nil && v > 0 && v <= math.MaxInt64 {
		t.peak, t.peakRead = int64(v), true
	}
}

// remove kills what is left in t and beneath it, by cgroup.kill, and removes t, deepest first; a
// killed process leaves its cgroup as it exits, so a busy one is tried again, killPause apart,
// killTries times at most.
func (t *runCgroup) remove() error {
	kill := t.kill()
	for range killTries {
		switch err := rmdirTree(t.path); {
		case err == nil:
			return nil
		case !errors.Is(err, unix.EBUSY):
			return errors.Join(fmt.Errorf("proc: removing the run's cgroup %s: %w", t.path, err), kill)
		}
		t.pause()
	}
	return errors.Join(fmt.Errorf("proc: the run's cgroup %s still held a process after %d tries at removing it", t.path, killTries), kill)
}

// rmdirTree removes the cgroup at path, deepest first: a cgroup's folder holds only files and child
// cgroups, and cgroup v2 makes no link.
func rmdirTree(path string) error {
	entries, err := os.ReadDir(path)
	if err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if err := rmdirTree(filepath.Join(path, e.Name())); err != nil {
				return err
			}
		}
	}
	// A folder that would not list is removed all the same, where it is empty.
	return unix.Rmdir(path)
}

// peakOf is the run's peak: tree's memory.peak, read once the tree had ended, when the program ran.
func peakOf(_ *process, _ *group, tree *runCgroup, ran bool) (int64, bool) {
	if !ran || tree == nil || !tree.peakRead {
		return 0, false
	}
	return tree.peak, true
}
