package proc

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix" //nolint:depguard // arch.ports.23: the test takes a pidfd of its own process, which syscall does not give
)

// endsBeforeShim is the helper mode whose shim endsBeforeTheShim ends with code 3.
const endsBeforeShim = "ends-before-shim"

// sleepsBeforeShim is the helper mode whose shim endsBeforeTheShim holds for shimSleep.
const sleepsBeforeShim = "sleeps-before-shim"

// shimSleep is how long the shim of a run of sleepsBeforeShim sleeps before the shim's init, well
// past the Timeout its row gives the run.
const shimSleep = 30 * time.Second

// The test binary started as the shim of a run of endsBeforeShim ends with code 3 before the shim's
// init, and as the shim of a run of sleepsBeforeShim sleeps there for shimSleep: a package
// variable's initializer runs before every init function of its package, as an init of a package
// the caller's binary imports does before go-proc's, and the first ends as such an init would that
// exits on a variable the shim's empty environment does not hold, the second takes as
// long as a slow one does.
var _ = endsBeforeTheShim()

// endsBeforeTheShim ends this process with code 3 where it is the shim of a run of endsBeforeShim,
// sleeps for shimSleep where it is the shim of a run of sleepsBeforeShim, and returns true.
func endsBeforeTheShim() bool {
	if len(os.Args) == 0 || os.Args[0] != shimMark {
		return true
	}
	if slices.Contains(os.Args, helperMode+endsBeforeShim) {
		os.Exit(3) //nolint:revive // the init this stands for ends the process
	}
	if slices.Contains(os.Args, helperMode+sleepsBeforeShim) {
		time.Sleep(shimSleep) //nolint:forbidigo // the init this stands for takes this long, and the row's Timeout, far shorter, ends it
	}
	return true
}

// TestShimFailedReadsTheReport: a shim's report of shimReady alone is no refusal; a step and its
// errno, after shimReady or alone, is errNoContained with the step's reason and the errno, but the
// exec's, which is errContainedExec, ErrRefused and not ErrNotHeld, as by then every step was taken; and any other report, the empty one among them, is ErrRefused with
// errShimEnded, not errNoContained. ErrNotHeld is matched where errNoContained is, and nowhere else.
//
// What it costs when this fails: a shim that ended before its steps, or a start that skipped it,
// reads as a run that started held, and the caller takes the program's exit, or a program that ran
// with no namespace limit, own proc or cleared capabilities, for a Contained one.
func TestShimFailedReadsTheReport(t *testing.T) {
	t.Parallel()
	enoent, eperm := byte(syscall.ENOENT), byte(syscall.EPERM)
	for _, c := range []struct {
		report []byte
		want   []error
	}{
		{[]byte{shimReady}, nil},
		{[]byte{shimReady, shimExec, enoent}, []error{errContainedExec, ErrRefused, syscall.ENOENT}},
		{[]byte{shimReady, shimExec, byte(syscall.ENOEXEC)}, []error{errContainedExec, ErrRefused, syscall.ENOEXEC}},
		{[]byte{shimLimit, eperm}, []error{errNoContained, errNoNsLimit, syscall.EPERM}},
		{[]byte{shimProc, enoent}, []error{errNoContained, errNoProcMount, syscall.ENOENT}},
		{[]byte{shimPrivs, eperm}, []error{errNoContained, syscall.EPERM}},
		{nil, []error{ErrRefused, errShimEnded}},
		{[]byte{shimReady, shimExec}, []error{ErrRefused, errShimEnded}},
		{[]byte{shimExec}, []error{ErrRefused, errShimEnded}},
		{[]byte{'z', eperm}, []error{ErrRefused, errShimEnded}},
		{[]byte{shimReady, 'z', eperm}, []error{ErrRefused, errShimEnded}},
	} {
		err := shimFailed(c.report)
		ended := errors.Is(err, errShimEnded)
		contained := errors.Is(err, errNoContained)
		if (err == nil) != (c.want == nil) || slices.ContainsFunc(c.want, func(w error) bool { return !errors.Is(err, w) }) || ended && contained ||
			errors.Is(err, ErrNotHeld) != contained || errors.Is(err, errContainedExec) && (ended || contained) {
			t.Errorf("report %q: %v, want %v, errShimEnded, errNoContained or errContainedExec, one at most, and ErrNotHeld only with errNoContained", c.report, err, c.want)
		}
	}
}

// TestAContainedRunWhoseShimEndsBeforeItsStepsIsRefused: a Contained run, held to 64 MiB, of a
// helper whose shim ends with code 3 before go-proc's init, as endsBeforeTheShim makes it, ends with
// ErrRefused and errShimEnded, Code -1 and no output, not with the shim's code. Where the caller is
// root, no cgroup is delegated, or cgroup v2 has no nsdelegate, the run is refused before its start.
//
// What it costs when this fails: a caller's binary with an init that ends the process, as one that
// exits on a variable the shim's empty environment lacks, gets that exit as its program's, with no
// error, and takes a program that never ran for one that ran held.
func TestAContainedRunWhoseShimEndsBeforeItsStepsIsRefused(t *testing.T) {
	t.Parallel()
	const what = "a Contained run whose shim ends with code 3 before its steps"
	c := helperCmd(t, endsBeforeShim)
	c.Limits = Limits{TreeMemory: treeLimit, Contained: true}
	res, held, err := runContained(t, what, c)
	if held && (!errors.Is(err, ErrRefused) || !errors.Is(err, errShimEnded) || errors.Is(err, errNoContained) || res.Code != -1 || len(res.Stdout) != 0) {
		t.Errorf("%s: %v, Code %d, stdout %q, want ErrRefused with errShimEnded, not errNoContained, Code -1 and no output", what, err, res.Code, res.Stdout)
	}
}

// TestAContainedRunHeldToOnePageEndsLimit: a Contained run of the argv helper held to one page, below
// the shim's own memory, which the kernel kills before it writes its report, ends with WhyLimit and
// Code -1, not ErrRefused with errShimEnded. Where the caller is root, no cgroup is delegated, or
// cgroup v2 has no nsdelegate, the run is refused before its start.
//
// What it costs when this fails: a caller that sets a TreeMemory too low for its own binary is told
// its init functions may have ended the shim, and one that reads WhyLimit to learn its limit was
// passed does not get it, as Limits.Contained says it does.
func TestAContainedRunHeldToOnePageEndsLimit(t *testing.T) {
	t.Parallel()
	const what = "a Contained run held to one page"
	c := helperCmd(t, "argv")
	c.Limits = Limits{TreeMemory: uint64(os.Getpagesize()), Contained: true}
	res, held, err := runContained(t, what, c)
	if held && (!endedWith(err, WhyLimit) || errors.Is(err, ErrRefused) || res.Code != -1) {
		t.Errorf("%s: %v, Code %d, want WhyLimit, not ErrRefused, and Code -1", what, err, res.Code)
	}
}

// TestAContainedRunWhoseShimPassesItsTimeoutEndsDeadline: a Contained run, held to 64 MiB, of a
// helper whose shim sleeps for shimSleep before go-proc's init, as endsBeforeTheShim makes it, under
// a Timeout of a second, ends with WhyDeadline and Code -1, not ErrRefused with errShimEnded. Where
// the caller is root, no cgroup is delegated, or cgroup v2 has no nsdelegate, the run is refused
// before its start.
//
// What it costs when this fails: a caller that tells a deadline from a refusal by Ended gets none
// for a run cut while its shim starts, and is told an init of its binary may have ended the shim;
// one that cancels many runs at once meets this on every run then starting.
func TestAContainedRunWhoseShimPassesItsTimeoutEndsDeadline(t *testing.T) {
	t.Parallel()
	const what = "a Contained run whose shim sleeps past its Timeout"
	c := helperCmd(t, sleepsBeforeShim)
	c.Timeout = time.Second
	c.Limits = Limits{TreeMemory: treeLimit, Contained: true}
	res, held, err := runContained(t, what, c)
	if held && (!endedWith(err, WhyDeadline) || errors.Is(err, ErrRefused) || res.Code != -1) {
		t.Errorf("%s: %v, Code %d, want WhyDeadline, not ErrRefused, and Code -1", what, err, res.Code)
	}
}

// TestAContainedRunOnAnEndedContextIsCancelled: a Contained run, held to 64 MiB, whose parent
// context has already ended, ends with Ended{WhyCancelled} joined to the parent's cause and Code -1,
// not errNoContained. Where the caller is root, no cgroup is delegated, or cgroup v2 has no
// nsdelegate, the run is refused before its start.
//
// What it costs when this fails: exec's Start gives the context's error, which a Contained start
// reads as a kernel or security module that refuses an unprivileged user namespace, and a cancelled
// caller is told this system cannot hold Contained.
func TestAContainedRunOnAnEndedContextIsCancelled(t *testing.T) {
	t.Parallel()
	parent, cancel := context.WithCancelCause(t.Context())
	cancel(errTheTestEndedIt)
	c := helperCmd(t, "argv")
	c.Limits = Limits{TreeMemory: treeLimit, Contained: true}
	held, _ := containedHere(t)
	res, err := Run(parent, c)
	if held && (!endedWith(err, WhyCancelled) || !errors.Is(err, errTheTestEndedIt) || errors.Is(err, errNoContained) || res.Code != -1) {
		t.Errorf("a Contained run on an ended context: %v, Code %d, want Ended{%s} and the parent's cause, not errNoContained, and Code -1", err, res.Code, WhyCancelled)
	}
}

// skipShimScript is a shell script that takes the shim's argv (its report's descriptor, the three
// files of at, the program's path and its Dir, then the program's argv) and execs the program at
// once, as a start that skips the shim would: it reports nothing, writes no limit, mounts nothing
// and stays in the caller's folder.
const skipShimScript = "#!/bin/sh\np=$5\nshift 7\nexec \"$p\" \"$@\"\n"

// TestShimEnterTakesTheFolderAgainByWhatTheKernelOpened: the shim enters Dir, or stays in the caller's
// folder where Dir is empty, then asks that folder's path, opens it by open and enters it by fchdir,
// never by a second chdir to the path; a chdir to Dir that fails, a getwd that fails, as ENOENT for a
// folder on no mount of the tree's, /proc/self/cwd among them, or an open that fails, as ELOOP for a
// link, or ENOSYS or EPERM where openat2 is absent or filtered, ends it with that error, before any
// fchdir and with no chdir by the path in its place; an fchdir that fails ends it with that error.
//
// What it costs when this fails: a Dir through a proc link, /proc/self/cwd or /proc/self/fd/N, is
// entered by the folder the link holds, under the covers: the tree starts in the host's cgroup mount
// and names the caller's cgroup by "..", so freezes or kills the caller, then lifts its own limit; and
// where openat2 is filtered, the shim falls back to the chdir by a path that a link swapped in beside
// the start can take to the caller's mounts.
func TestShimEnterTakesTheFolderAgainByWhatTheKernelOpened(t *testing.T) {
	t.Parallel()
	refused := errors.New("refused")
	for _, tc := range []struct {
		dir, wd                                  string
		chdirErr, wdErr, openErr, fchdirErr, err error
		want                                     []string
	}{
		{"/proc/self/cwd", "/sys/fs/cgroup/run", nil, nil, nil, nil, nil, []string{"chdir /proc/self/cwd", "getwd", "open /sys/fs/cgroup/run", "fchdir"}},
		{"rel/../dir", "/home/someone/dir", nil, nil, nil, nil, nil, []string{"chdir rel/../dir", "getwd", "open /home/someone/dir", "fchdir"}},
		{"", "/home/someone", nil, nil, nil, nil, nil, []string{"getwd", "open /home/someone", "fchdir"}},
		{"/locked", "", refused, nil, nil, nil, refused, []string{"chdir /locked"}},
		{"/proc/self/fd/40", "", nil, unix.ENOENT, nil, nil, unix.ENOENT, []string{"chdir /proc/self/fd/40", "getwd"}},
		{"/proc/self/cwd", "", nil, unix.ENOENT, nil, nil, unix.ENOENT, []string{"chdir /proc/self/cwd", "getwd"}},
		{"/home/someone/dir", "/home/someone/dir", nil, nil, unix.ELOOP, nil, unix.ELOOP, []string{"chdir /home/someone/dir", "getwd", "open /home/someone/dir"}},
		{"/home/someone/dir", "/home/someone/dir", nil, nil, unix.ENOSYS, nil, unix.ENOSYS, []string{"chdir /home/someone/dir", "getwd", "open /home/someone/dir"}},
		{"/home/someone/dir", "/home/someone/dir", nil, nil, unix.EPERM, nil, unix.EPERM, []string{"chdir /home/someone/dir", "getwd", "open /home/someone/dir"}},
		{"", "/home/someone", nil, nil, nil, refused, refused, []string{"getwd", "open /home/someone", "fchdir"}},
	} {
		var calls []string
		err := shimEnter(tc.dir, func(path string) error {
			calls = append(calls, "chdir "+path)
			if path == tc.dir {
				return tc.chdirErr
			}
			return nil
		}, func() (string, error) {
			calls = append(calls, "getwd")
			return tc.wd, tc.wdErr
		}, func(path string) (int, error) {
			calls = append(calls, "open "+path)
			return 7, tc.openErr
		}, func(int) error {
			calls = append(calls, "fchdir")
			return tc.fchdirErr
		})
		if !errors.Is(err, tc.err) || !slices.Equal(calls, tc.want) {
			t.Errorf("the shim entering Dir %q (folder %q): %v, calls %q, want %v, calls %q", tc.dir, tc.wd, err, calls, tc.err, tc.want)
		}
	}
}

// TestShimEnterRefusesALinkSwappedInAfterGetwd: the folder the shim asks by getwd is replaced, after
// getwd answers and before the shim takes it, by a link to another folder, as a writer to its parent
// does by a rename and a symlink; the shim, opening it by openFolder, ends with ELOOP, and enters no
// folder by fchdir or by a chdir to the path.
//
// What it costs when this fails: a caller that holds a directory open without close-on-exec, and a
// writer at work beside the start, send the shim by a link to /proc/self/fd/N into the caller's mounts
// under the covers, which the second step by the folder's path was to keep it from.
func TestShimEnterRefusesALinkSwappedInAfterGetwd(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	wd, other := filepath.Join(root, "wd"), filepath.Join(root, "other")
	for _, d := range []string{wd, other} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var calls []string
	err := shimEnter("", func(path string) error {
		calls = append(calls, "chdir "+path)
		return nil
	}, func() (string, error) {
		calls = append(calls, "getwd")
		if err := os.Rename(wd, wd+".moved"); err != nil {
			t.Error(err)
		}
		if err := os.Symlink(other, wd); err != nil {
			t.Error(err)
		}
		return wd, nil
	}, openFolder, func(fd int) error {
		calls = append(calls, "fchdir")
		return unix.Close(fd)
	})
	if want := []string{"getwd"}; !errors.Is(err, unix.ELOOP) || !slices.Equal(calls, want) {
		t.Errorf("the shim entering a folder replaced by a link after getwd: %v, calls %q, want ELOOP, calls %q", err, calls, want)
	}
}

// TestOpenFolderRefusesEveryLink: openFolder opens a folder by its path, and refuses with ELOOP a
// path whose last part is a link to a folder, one whose middle part is, and a proc link,
// /proc/self/cwd and /proc/<this pid>/cwd, which a plain chdir would follow to the folder it holds.
// The first is refused at "self", an ordinary link, before "cwd" is read; the second, which holds no
// ordinary link, shows a proc link refused on its own.
//
// What it costs when this fails: the shim's second step follows a link, so a swapped one takes it to
// the folder the link names, the caller's mounts among them.
func TestOpenFolderRefusesEveryLink(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(filepath.Join(real, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	last, middle := filepath.Join(root, "last"), filepath.Join(root, "middle")
	for link, to := range map[string]string{last: real, middle: real} {
		if err := os.Symlink(to, link); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		path string
		err  error
	}{
		{real, nil},
		{filepath.Join(real, "sub"), nil},
		{last, unix.ELOOP},
		{filepath.Join(middle, "sub"), unix.ELOOP},
		{"/proc/self/cwd", unix.ELOOP},
		{"/proc/" + strconv.Itoa(os.Getpid()) + "/cwd", unix.ELOOP},
	} {
		fd, err := openFolder(tc.path)
		if err == nil {
			_ = unix.Close(fd)
		}
		if !errors.Is(err, tc.err) {
			t.Errorf("openFolder(%q): %v, want %v", tc.path, err, tc.err)
		}
	}
}

// TestShimFchdirEntersTheFolderAndClosesItsDescriptor: shimFchdir, the shim's closure over Fchdir,
// answers the error of an fchdir to a descriptor that is no folder, ENOTDIR for a plain file's; and a
// helper that opens a folder by openFolder and gives it to shimFchdir is in that folder after, by its
// own getcwd, with the descriptor closed (a helper, whose descriptor no parallel test can be given).
//
// What it costs when this fails: a closure that closes the descriptor without entering it, or drops
// Fchdir's error, passes every other row, and the step that takes the shim through the cover is
// never taken or its refusal never seen: a caller whose own folder is the cgroup mount's root, with
// no Dir, keeps the shim on the host's root under it.
func TestShimFchdirEntersTheFolderAndClosesItsDescriptor(t *testing.T) {
	t.Parallel()
	direct := func(f func() error) error { return f() }
	file, err := os.CreateTemp(t.TempDir(), "file")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	fd, err := unix.Dup(int(file.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if err := shimFchdir(fd, direct); !errors.Is(err, unix.ENOTDIR) {
		t.Errorf("shimFchdir of a plain file's descriptor: %v, want ENOTDIR", err)
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	res, err := Run(t.Context(), helperCmd(t, "fchdir-folder", dir))
	got := cgroupLines(res.Stdout)
	if err != nil || got["wd"] != dir || got["closed"] != "true" {
		t.Errorf("a helper that gave the folder %q to shimFchdir: %v, in %q, descriptor closed %q, stderr %q, want it in the folder, closed", dir, err, got["wd"], got["closed"], res.Stderr)
	}
}

// TestAContainedRunThatSkipsItsShimIsRefused: a Contained run, held to 64 MiB, of the argv helper,
// whose shim's binary is skipShimScript in place of this test binary, ends with ErrRefused and
// errShimEnded, Code -1 and no output, though the helper ran, as a start that skips the shim does. Where the caller is root, no cgroup is delegated, or cgroup v2
// has no nsdelegate, the run is refused before its start.
//
// What it costs when this fails: a start that never ran the shim, as by a library the caller's
// loader preloads into it that execs the program, gives a program with CAP_SYS_RESOURCE and
// CAP_SYS_ADMIN in the tree's user namespace, user.max_cgroup_namespaces unwritten and the caller's
// /proc, which may nest a cgroup namespace and lift its limit, and the caller takes it as held.
func TestAContainedRunThatSkipsItsShimIsRefused(t *testing.T) {
	t.Parallel()
	const what = "a Contained run whose start skips its shim"
	exe := filepath.Join(t.TempDir(), "skip")
	if err := writeExe(exe, []byte(skipShimScript)); err != nil {
		t.Fatal(err)
	}
	c := helperCmd(t, "argv")
	c.Limits = Limits{TreeMemory: treeLimit, Contained: true}
	c.seam = &seams{host: cgroupHost{exe: exe}}
	res, held, err := runContained(t, what, c)
	if held && (!errors.Is(err, ErrRefused) || !errors.Is(err, errShimEnded) || errors.Is(err, errNoContained) || res.Code != -1 || len(res.Stdout) != 0) {
		t.Errorf("%s: %v, Code %d, stdout %q, want ErrRefused with errShimEnded, not errNoContained, Code -1 and no output", what, err, res.Code, res.Stdout)
	}
}

// TestAContainedRunWhoseProgramForgesTheReadyByteIsRefused: a Contained run, held to 64 MiB, of the
// forge-ready helper, whose shim's binary is skipShimScript, so the helper starts with no step taken
// and writes the shim's ready byte to each descriptor it inherited, the report's among them, and
// holds for holdFor, ends with ErrRefused and errShimForged, Code -1 and no output, ended by the
// refusal's kill of the tree and not by its Timeout. Where the caller is root, no cgroup is
// delegated, or cgroup v2 has no nsdelegate, the run is refused before its start.
//
// The row waits on the tree's kill, not a clock: the report stays open while the helper holds, so
// Run returns once the tree is gone, and a refusal that leaves the tree running returns only when
// the Timeout kills it, as WhyDeadline, which fails the row however long either took.
//
// What it costs when this fails: a program started with no step of the shim taken, which has
// CAP_SYS_ADMIN in the tree's user namespace and sees the caller's cgroup mounts and proc, writes
// the ready byte itself and the caller takes the run as held, and waits for the program.
func TestAContainedRunWhoseProgramForgesTheReadyByteIsRefused(t *testing.T) {
	t.Parallel()
	const what = "a Contained run whose program forges the ready byte"
	exe := filepath.Join(t.TempDir(), "skip")
	if err := writeExe(exe, []byte(skipShimScript)); err != nil {
		t.Fatal(err)
	}
	c := helperCmd(t, "forge-ready")
	c.Limits = Limits{TreeMemory: treeLimit, Contained: true}
	c.Timeout = 30 * time.Second
	c.seam = &seams{host: cgroupHost{exe: exe}}
	res, held, err := runContained(t, what, c)
	if held && (!errors.Is(err, ErrRefused) || !errors.Is(err, errShimForged) || errors.Is(err, errNoContained) || errors.Is(err, errDeadline) || res.Code != -1 || len(res.Stdout) != 0) {
		t.Errorf("%s: %v, Code %d, stdout %q, want ErrRefused with errShimForged, not errNoContained, not WhyDeadline, Code -1 and no output: the tree killed by the refusal, not by its Timeout", what, err, res.Code, res.Stdout)
	}
}

// TestAContainedRunWhoseProgramDroppedItsCapabilitiesBeforeForgingTheByteIsRefused: as the row
// before, of the forge-dropped helper, which sets no_new_privs and clears its capabilities itself
// before it writes the byte, so it passes the check of each process's no_new_privs and capabilities and keeps the caller's cgroup mounts
// uncovered: the run ends at once with ErrRefused and errShimForged, Code -1 and no output, and the
// helper is killed. Where the caller is root, no cgroup is delegated, or cgroup v2 has no
// nsdelegate, the run is refused before its start.
//
// What it costs when this fails: a program of the caller's own binary's init that skipped the shim
// and dropped what that check reads keeps the caller's cgroup mounts, and through them the limit cgroup's
// memory.max and the caller's other cgroups, and the caller takes the run as held.
func TestAContainedRunWhoseProgramDroppedItsCapabilitiesBeforeForgingTheByteIsRefused(t *testing.T) {
	t.Parallel()
	const what = "a Contained run whose program dropped its capabilities and forges the ready byte"
	exe := filepath.Join(t.TempDir(), "skip")
	if err := os.WriteFile(exe, []byte(skipShimScript), 0o700); err != nil {
		t.Fatal(err)
	}
	c := helperCmd(t, "forge-dropped")
	c.Limits = Limits{TreeMemory: treeLimit, Contained: true}
	c.Timeout = 30 * time.Second
	c.seam = &seams{host: cgroupHost{exe: exe}}
	res, held, err := runContained(t, what, c)
	if held && (!errors.Is(err, ErrRefused) || !errors.Is(err, errShimForged) || !strings.Contains(err.Error(), "cgroup mount uncovered") || errors.Is(err, errNoContained) || errors.Is(err, errDeadline) || res.Code != -1 || len(res.Stdout) != 0) {
		t.Errorf("%s: %v, Code %d, stdout %q, want ErrRefused with errShimForged naming an uncovered cgroup mount, not errNoContained, not errDeadline (the helper killed by the refusal, not by the run's Timeout), Code -1 and no output", what, err, res.Code, res.Stdout)
	}
}

// TestAContainedRunWhoseLeaderEndedWithTheThreadOnlyExitIsRefused: as the row before, of the
// forge-leader-exit helper, whose leader sets no_new_privs and clears its capabilities, forges the byte
// and ends with the thread-only exit, so the leader is a zombie that passes for the shim's and the
// threads that run on keep the caller's cgroup mounts and their capabilities: the run ends at once
// with ErrRefused and errShimForged, Code -1 and no output, and the helper is killed. Where the caller is root, no cgroup is delegated, or cgroup v2 has no
// nsdelegate, the run is refused before its start.
//
// What it costs when this fails: a program of the caller's own binary's init that skipped the shim
// and ended its leader keeps the caller's cgroup mounts in a thread the check read as gone, and the
// caller takes the run as held.
func TestAContainedRunWhoseLeaderEndedWithTheThreadOnlyExitIsRefused(t *testing.T) {
	t.Parallel()
	const what = "a Contained run whose program ended its leader and forges the ready byte"
	exe := filepath.Join(t.TempDir(), "skip")
	if err := os.WriteFile(exe, []byte(skipShimScript), 0o700); err != nil {
		t.Fatal(err)
	}
	c := helperCmd(t, "forge-leader-exit")
	c.Limits = Limits{TreeMemory: treeLimit, Contained: true}
	c.Timeout = 30 * time.Second
	c.seam = &seams{host: cgroupHost{exe: exe}}
	res, held, err := runContained(t, what, c)
	if held && (!errors.Is(err, ErrRefused) || !errors.Is(err, errShimForged) || errors.Is(err, errNoContained) || errors.Is(err, errDeadline) || res.Code != -1 || len(res.Stdout) != 0) {
		t.Errorf("%s: %v, Code %d, stdout %q, want ErrRefused with errShimForged, not errNoContained, not errDeadline, Code -1 and no output", what, err, res.Code, res.Stdout)
	}
}

// TestAContainedTreeCannotTakeTheCallersDescriptorsByPidfd: a helper given a pidfd of this process
// as its descriptor 3 asks pidfd_getfd for this process's descriptor of that pidfd. Run without
// Contained it gets it where Yama is absent or its ptrace_scope is 0, and EPERM where Yama refuses
// it; run Contained, held to 64 MiB, it gets EPERM at every ptrace_scope, as the kernel's
// cap_ptrace_access_check refuses a process access to one in an outer user namespace it holds no
// CAP_SYS_PTRACE over. Where the caller is root, no cgroup is delegated, or cgroup v2 has no
// nsdelegate, the Contained run is refused before its start.
//
// What it costs when this fails: a Contained tree that gets a pidfd for a process of the caller's
// uid, as by SO_PEERPIDFD over a unix socket it connects to, takes that process's descriptors, and
// what they open, out of the tree; or the plain run's EPERM is the reason, and the row proves
// nothing of Contained. Limits.Contained's doc and the README say what this shows.
func TestAContainedTreeCannotTakeTheCallersDescriptorsByPidfd(t *testing.T) {
	t.Parallel()
	pidfd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		t.Fatal(err)
	}
	f := os.NewFile(uintptr(pidfd), "pidfd")
	defer func() { _ = f.Close() }()
	scope := ptraceScope(t)
	eperm := strconv.Itoa(int(syscall.EPERM))
	want := map[bool]string{true: "0", false: eperm}[scope == "0"]
	plain := helperCmd(t, "pidfd-getfd", strconv.Itoa(pidfd))
	plain.ExtraFiles = []*os.File{f}
	res, err := Run(t.Context(), plain)
	if got := cgroupLines(res.Stdout)["getfd"]; err != nil || got != want {
		t.Errorf("a helper run without Contained at ptrace_scope %s: %v, getfd errno %q, stderr %q, want %s", scope, err, got, res.Stderr, want)
	}
	const what = "a Contained helper that asked pidfd_getfd for this process's descriptor"
	c := helperCmd(t, "pidfd-getfd", strconv.Itoa(pidfd))
	c.ExtraFiles = []*os.File{f}
	c.Limits = Limits{TreeMemory: treeLimit, Contained: true}
	res, held, err := runContained(t, what, c)
	if got := cgroupLines(res.Stdout)["getfd"]; held && (err != nil || got != eperm) {
		t.Errorf("%s at ptrace_scope %s: %v, getfd errno %q, stderr %q, want %s", what, scope, err, got, res.Stderr, eperm)
	}
}

// TestAContainedTreeCannotReadTheCallersMemoryByAnOuterProc: a helper opens this process's
// /proc/<pid>/mem and reads a byte of it, and opens its /proc/<pid>/root/, by a /proc of this
// process's pid namespace. Run without Contained, mem opens and reads where Yama is absent or its
// ptrace_scope is 0, and is EACCES where Yama refuses it; root opens at every ptrace_scope, as Yama
// refuses only an attach. Run Contained, held to 64 MiB, with the seam putting the tree's proc
// elsewhere than /proc, so its /proc is the caller's, as a host that shows the tree an outer proc
// mount does: mem and root are each EACCES at every ptrace_scope, as the kernel's
// cap_ptrace_access_check refuses a process access to one in an outer user namespace it holds no
// CAP_SYS_PTRACE over. Where the caller is root, no cgroup is delegated, or cgroup v2 has no
// nsdelegate, the Contained run is refused before its start.
//
// What it costs when this fails: a Contained tree that a host shows an outer proc mount reads and
// writes the memory of the caller's other processes, and walks their root links to the host's
// cgroup mount; or the plain run's EACCES is the reason, and the row proves nothing of Contained.
// Limits.Contained's doc and the README say what this shows.
func TestAContainedTreeCannotReadTheCallersMemoryByAnOuterProc(t *testing.T) {
	t.Parallel()
	pid := strconv.Itoa(os.Getpid())
	scope := ptraceScope(t)
	eacces, enoent := strconv.Itoa(int(syscall.EACCES)), strconv.Itoa(int(syscall.ENOENT))
	wantMem, wantRead := "0", "0"
	if scope != "0" {
		wantMem, wantRead = eacces, enoent
	}
	res, err := Run(t.Context(), helperCmd(t, "outer-proc", pid))
	if got := cgroupLines(res.Stdout); err != nil || got["mem"] != wantMem || got["memread"] != wantRead || got["root"] != "0" {
		t.Errorf("a helper run without Contained at ptrace_scope %s: %v, mem %q, memread %q, root %q, stderr %q, want %s, %s and 0", scope, err, got["mem"], got["memread"], got["root"], res.Stderr, wantMem, wantRead)
	}
	const what = "a Contained helper that opened this process's mem and root by the caller's /proc"
	c := helperCmd(t, "outer-proc", pid)
	c.Limits = Limits{TreeMemory: treeLimit, Contained: true}
	c.seam = &seams{host: cgroupHost{tree: t.TempDir()}}
	res, held, err := runContained(t, what, c)
	if got := cgroupLines(res.Stdout); held && (err != nil || got["mem"] != eacces || got["memread"] != enoent || got["root"] != eacces) {
		t.Errorf("%s at ptrace_scope %s: %v, mem %q, memread %q, root %q, stderr %q, want %s, %s and %s", what, scope, err, got["mem"], got["memread"], got["root"], res.Stderr, eacces, enoent, eacces)
	}
}

// ptraceScope is Yama's ptrace_scope here, logged, or 0 where Yama is absent, which allows as 0 does.
func ptraceScope(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("/proc/sys/kernel/yama/ptrace_scope")
	switch {
	case errors.Is(err, fs.ErrNotExist):
		t.Log("Yama is absent here, which allows as ptrace_scope 0 does")
		return "0"
	case err != nil:
		t.Fatal(err)
	}
	scope := strings.TrimSpace(string(b))
	t.Logf("Yama's ptrace_scope is %s here", scope)
	return scope
}

// TestAContainedProgramStartsWithTheShimsMemoryGone: sh, run Contained and held to 64 MiB, prints
// its own cgroup's memory.current and memory.stat, the run's, through the shim's cover, and then, for
// the log, its threads and processes and the figure a second later: what the run
// holds as its program starts is under 16 MiB, a quarter of the limit, and, in a build with the race
// detector, whose shim image is the caller's race-built binary and is charged for a moment after the
// exec (45 to 48 MiB on arm64), under 16 MiB a second after it starts, the start figure only logged.
// The row logs both figures, the stat lines and the run's peak. Where no cgroup is delegated, or
// cgroup v2 has no nsdelegate, the run is refused and starts nothing.
//
// What it costs when this fails: the memory of the shim's stages is still counted against the run's
// TreeMemory long after the program starts, or, plain, at its start, so a program that fits its limit
// is killed at it.
func TestAContainedProgramStartsWithTheShimsMemoryGone(t *testing.T) {
	t.Parallel()
	const what, most = "sh of its own cgroup's memory.current and memory.stat, run Contained", 16 << 20
	// The program reads memory.current first, and then, for the log alone, what else the run's cgroup
	// holds then and a second later: its threads and processes, and whether the figure falls with time
	// (a stage's memory not yet uncharged) or stays (a stage not gone).
	const script = `cd ` + cgroupMount + `; cat memory.current; cat memory.stat; echo threads; cat cgroup.threads; ` +
		`for p in /proc/[0-9]*; do echo "proc $p"; grep -E '^(Name|State|Threads|VmRSS)' $p/status; done; ` +
		`echo after-1s; sleep 1; cat memory.current; grep -E '^(anon|file|slab|pagetables) ' memory.stat`
	c := helperCmd(t, "")
	c.Path, c.Args, c.ReadPeak = "/bin/sh", []string{"-c", script}, true
	c.Env = append(c.Env, "PATH=/usr/bin:/bin")
	c.Limits = Limits{TreeMemory: treeLimit, Contained: true}
	res, held, err := runContained(t, what, c)
	if !held {
		return
	}
	first, stat, _ := strings.Cut(string(res.Stdout), "\n")
	current, convErr := strconv.ParseUint(first, 10, 64)
	_, afterOut, _ := strings.Cut(stat, "after-1s\n")
	later, _, _ := strings.Cut(afterOut, "\n")
	after, afterErr := strconv.ParseUint(later, 10, 64)
	t.Logf("%s: memory.current %d at the start and %d a second later, the run's peak %d (read %t), memory.stat:\n%s", what, current, after, res.Peak, res.PeakRead, stat)
	// The shim's last image is charged, briefly, until the kernel has freed it after the program's exec;
	// under -race it is the caller's race-built binary, 45 to 48 MiB on arm64, and gone a second later. The
	// figure that must hold at the start is the plain one; under -race the one a second later.
	figure, got, gotErr := "at the start", current, convErr
	if raceMemory != 0 {
		figure, got, gotErr = "a second later", after, afterErr
	}
	if err != nil || res.Code != 0 || convErr != nil || gotErr != nil || got >= most {
		t.Errorf("%s: %v, Code %d, memory.current %q at the start (%v) and %q a second later (%v), stderr %q, want exit 0 and under %d bytes %s", what, err, res.Code, first, convErr, later, afterErr, res.Stderr, most, figure)
	}
}
