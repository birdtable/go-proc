package proc

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix" //nolint:depguard // arch.ports.23: the row makes its FIFO by mkfifo, which os does not give
)

// TestAContainedRunWhoseFolderCannotBeFoundAgainIsRefused: a Contained run, held to 64 MiB, whose
// Dir is the delegated cgroup's folder, which the shim's covers hide, and one with no Dir from this
// process in a folder of the test's it then removed, are each refused with errContainedFolder,
// inside ErrRefused but not errors.ErrUnsupported, with ENOENT, Code -1 and no output: the program
// never ran. Where no cgroup is delegated, or cgroup v2 has no nsdelegate, the run is refused
// before its start.
//
// What it costs when this fails: the tree starts in a folder of the host's cgroup mount, under the
// covers, and names the caller's cgroup by "..", so freezes or kills the caller, then lifts its own
// limit; or a folder this process cannot find again is refused as if
// this system could not hold Contained, and a caller gives Contained up.
func TestAContainedRunWhoseFolderCannotBeFoundAgainIsRefused(t *testing.T) {
	// serial: the second row changes this process's folder
	here, err := selfPath()
	if held, _ := containedHere(t); err != nil && held {
		t.Fatal(err)
	}
	for _, what := range []string{"a Contained run whose Dir is the delegated cgroup's folder", "a Contained run with no Dir from a folder since removed"} {
		cmd := containedCmd(t, what, "none", "16", "exit", "")
		if strings.HasSuffix(what, "removed") {
			gone := filepath.Join(t.TempDir(), "gone")
			if err := os.Mkdir(gone, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Chdir(gone)
			if err := os.Remove(gone); err != nil {
				t.Fatal(err)
			}
			cmd.Dir = ""
		} else {
			cmd.Dir = filepath.Join(cgroupMount, filepath.Dir(here))
		}
		res, held, err := runContained(t, what, cmd)
		if held && (!errors.Is(err, errContainedFolder) || !errors.Is(err, ErrRefused) || errors.Is(err, errors.ErrUnsupported) ||
			!errors.Is(err, fs.ErrNotExist) || res.Code != -1 || len(res.Stdout) != 0) {
			t.Errorf("%s: %v, Code %d, stdout %q, want errContainedFolder and ErrRefused, not ErrUnsupported, with ENOENT, Code -1 and no output", what, err, res.Code, res.Stdout)
		}
	}
}

// TestAContainedRunGetsNoDescriptorItWasNotHanded: a Contained run, held to 64 MiB, with no
// ExtraFiles, from this process holding a folder of the test's at a descriptor from 40 up, the lowest
// that is free, without close-on-exec, as one a caller was started with, finds it not open (EBADF)
// and runs to its end, Code 0. The descriptor is the first free from 40, not 40: Go holds a pidfd for
// each process it started and has not released, so a process left running by an earlier row may
// hold 40 itself. Where no cgroup is delegated, or cgroup v2 has no nsdelegate, the
// run is refused before its start.
//
// What it costs when this fails: Go's start leaves such a descriptor open through the shim into the
// program, unchecked: a folder in the host's cgroup mount names the caller's cgroup, and any other
// one walks to it by "..", so the tree freezes or kills the caller, then lifts its own limit.
func TestAContainedRunGetsNoDescriptorItWasNotHanded(t *testing.T) {
	// serial: the descriptor is this process's, which a parallel row's run would inherit
	const what, floor = "a Contained run from a process holding a folder at a descriptor from 40 up", 40
	dir, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	// F_DUPFD gives the lowest descriptor from floor that is free, without close-on-exec.
	high, err := unix.FcntlInt(dir.Fd(), unix.F_DUPFD, floor)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(high)
	res, held, err := runContained(t, what, containedCmd(t, what, "fd", "16", "exit", strconv.Itoa(high)))
	if held && (err != nil || res.Code != 0 || !strings.Contains(string(res.Stdout), "errno 9\n") || !strings.HasSuffix(string(res.Stdout), "survived\n")) {
		t.Errorf("%s: %v, Code %d, stdout %q, want descriptor %d not open, EBADF (errno 9), and the run to its end, Code 0", what, err, res.Code, res.Stdout, high)
	}
}

// TestAContainedRunWhoseDirIsAProcLinkIsRefused: a Contained run, held to 64 MiB, whose Dir is
// /proc/self/fd/N, N a folder of the test's this process holds from 40 up without close-on-exec, as
// in TestAContainedRunGetsNoDescriptorItWasNotHanded; and, from this process in the delegated
// cgroup's folder, one whose Dir is /proc/self/cwd and one whose Dir is a relative path to a link
// to /proc/self/cwd, are each refused as TestAContainedRunWhoseFolderCannotBeFoundAgainIsRefused
// wants: errContainedFolder, inside ErrRefused but not errors.ErrUnsupported, with ENOENT, Code -1
// and no output, the program never ran. The shim enters each by the folder the link holds, then
// takes it again by its path: the delegated cgroup's lies under the covers, and the held folder on
// no mount of the tree's. Where no cgroup is delegated,
// or cgroup v2 has no nsdelegate, the run is refused before its start.
//
// What it costs when this fails: the tree starts in a folder of the host's cgroup mount, under the
// covers, or in one on the caller's mounts from which ".." walks to it, and names the caller's
// cgroup, so freezes or kills the caller, then lifts its own limit.
func TestAContainedRunWhoseDirIsAProcLinkIsRefused(t *testing.T) {
	// serial: the descriptor is this process's, which a parallel row's run would inherit, and two rows change this process's folder
	held, _ := containedHere(t)
	here, err := selfPath()
	if err != nil && held {
		t.Fatal(err)
	}
	refused := func(what, dir string) {
		t.Helper()
		cmd := containedCmd(t, what, "none", "16", "exit", "")
		cmd.Dir = dir
		res, held, err := runContained(t, what, cmd)
		if held && (!errors.Is(err, errContainedFolder) || !errors.Is(err, ErrRefused) || errors.Is(err, errors.ErrUnsupported) ||
			!errors.Is(err, fs.ErrNotExist) || res.Code != -1 || len(res.Stdout) != 0) {
			t.Errorf("%s: %v, Code %d, stdout %q, want errContainedFolder and ErrRefused, not ErrUnsupported, with ENOENT, Code -1 and no output", what, err, res.Code, res.Stdout)
		}
	}
	folder, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer folder.Close()
	// F_DUPFD gives the lowest descriptor from 40 that is free, without close-on-exec.
	high, err := unix.FcntlInt(folder.Fd(), unix.F_DUPFD, 40)
	if err != nil {
		t.Fatal(err)
	}
	refused("a Contained run whose Dir is /proc/self/fd/N for a folder this process holds", "/proc/self/fd/"+strconv.Itoa(high))
	if err := unix.Close(high); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "cwd")
	if err := os.Symlink("/proc/self/cwd", link); err != nil {
		t.Fatal(err)
	}
	rel := link
	if held {
		parent := filepath.Join(cgroupMount, filepath.Dir(here))
		t.Chdir(parent)
		if rel, err = filepath.Rel(parent, link); err != nil {
			t.Fatal(err)
		}
	}
	refused("a Contained run from the delegated cgroup's folder whose Dir is /proc/self/cwd", "/proc/self/cwd")
	refused("a Contained run from the delegated cgroup's folder whose Dir is a relative path to a link to /proc/self/cwd, "+rel, rel)
}

// TestAContainedRunFromTheCgroupMountItselfIsEnteredIntoTheRunsRoot: a Contained run, held to 64 MiB,
// with no Dir, from this process in the cgroup mount itself, finds by its helper no folder at the
// delegated parent's path from its own folder, without the leading slash: ENOENT (errno 2), then runs
// to its end, Code 0. The shim's getwd names that folder /sys/fs/cgroup, which its covers hold the run's
// root at, and its fchdir enters it there, so a path the host's mount holds is not found; a call that
// closes the descriptor without entering it leaves the shim in the host's mount under the covers, where
// the helper finds the parent and the caller's cgroup by "..". Where no cgroup is delegated, or cgroup v2
// has no nsdelegate, the run is refused before its start, and the row proves that refusal only.
//
// What it costs when this fails: the shim's call to shimFchdir at proc/shim_linux.go has no row, and a
// call site that closes the descriptor without entering the folder, or drops its error, passes every
// other row, the held ones among them: a caller whose own folder is the cgroup mount's root starts
// the tree in the host's mount, from which the caller's cgroup is a relative path.
func TestAContainedRunFromTheCgroupMountItselfIsEnteredIntoTheRunsRoot(t *testing.T) {
	// serial: the row changes this process's folder
	held, _ := containedHere(t)
	here, err := selfPath()
	if err != nil && held {
		t.Fatal(err)
	}
	const what = "a Contained run with no Dir from this process in the cgroup mount itself"
	if held {
		if filepath.Dir(here) == "/" {
			t.Fatalf("this process's cgroup %s has no delegated parent below the root", here)
		}
		t.Chdir(cgroupMount)
	}
	cmd := containedCmd(t, what, "parent", "16", "exit", "")
	cmd.Dir = ""
	res, held, err := runContained(t, what, cmd)
	if held && (err != nil || res.Code != 0 || !strings.Contains(string(res.Stdout), "errno 2\n") || !strings.HasSuffix(string(res.Stdout), "survived\n")) {
		t.Errorf("%s: %v, Code %d, stdout %q, want the parent not found (ENOENT, errno 2) and the run to its end, Code 0", what, err, res.Code, res.Stdout)
	}
}

// TestAContainedRunWhoseCgroupLosesMemoryIsKilledWithErrLimitLost: a helper run Contained, held to
// 64 MiB, that writes ready to a FIFO of the test's, then starts a hog of 16 MiB and holds, has its
// tree killed by the run's watch once this process, outside the tree, writes -memory to the run's
// parent's cgroup.subtree_control, which the write wakes; the run ends with errLimitLost, not
// WhyLimit and not WhyDeadline, its run's cgroup gone after. The row moves this process into a cgroup
// of its own, below a delegated cgroup of its own, so the run's parent is its own and the write takes
// the memory controller from no other run's cgroup: not another row's, and not those of the other
// binary of the release tier, which runs beside this one under the same delegated parent;
// it moves this process back and removes both cgroups after. The run reads EndNone, with no Signal
// and no Status: the run's own kill is no end of the program's. A tree names no such file now
// (TestAContainedTreeCannotStopItsCallerThenLiftTreeMemory), so the write is made from
// outside, as another process of the caller's user may make it. Where no cgroup is delegated, or
// cgroup v2 has no nsdelegate, the run is refused and starts nothing.
//
// What it costs when this fails: the run's cgroup loses the memory controller, with no limit left on
// it, and its tree runs on, or ends as if its program exited, or as WhyLimit, or reads as ended by a
// SIGKILL go-proc did not send; the caller was told it could not leave its limit.
func TestAContainedRunWhoseCgroupLosesMemoryIsKilledWithErrLimitLost(t *testing.T) {
	// serial: this process moves to a cgroup of its own, as every row of the binary would
	const what = "a Contained helper held to 64 MiB whose parent's cgroup.subtree_control this process wrote -memory to"
	fifo := filepath.Join(t.TempDir(), "ready")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	held, _ := containedHere(t)
	if !held {
		_, _, _ = containedRun(t, what, "ready", "16", "hold", fifo)
		return
	}
	here, err := selfPath()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	// The run's parent is the cgroup above this process's, so a parent of the row's own, marked delegated, with
	// a cgroup in it for this process: the write below takes the controller from no run but this row's.
	parent, caller := callerCgroup(t, what, "lostmem")
	control := filepath.Join(cgroupMount, parent, "cgroup.subtree_control")
	pid := []byte(strconv.Itoa(os.Getpid()))
	t.Cleanup(func() { // before callerCgroup's, which removes both: this process must be out of them
		if err := os.WriteFile(filepath.Join(cgroupMount, here, "cgroup.procs"), pid, 0); err != nil {
			t.Errorf("%s: moving this process back to %s: %v", what, here, err)
		}
	})
	if err := os.WriteFile(filepath.Join(cgroupMount, caller, "cgroup.procs"), pid, 0); err != nil {
		t.Fatalf("%s: moving this process into %s: %v", what, caller, err)
	}
	// O_RDWR opens a FIFO at once on Linux, so a run that never writes does not hold the open.
	r, err := os.OpenFile(fifo, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	type ran struct {
		res Result
		err error
	}
	done, ready := make(chan ran, 1), make(chan error, 1)
	go func() {
		res, _, err := containedRun(t, what, "ready", "16", "hold", fifo)
		done <- ran{res, err}
	}()
	go func() {
		_, err := r.Read(make([]byte, 16))
		ready <- err
	}()
	var end ran
	select {
	case end = <-done:
		t.Fatalf("%s: the run ended before its helper wrote ready: %v, Code %d, stdout %q, stderr %q", what, end.err, end.res.Code, end.res.Stdout, end.res.Stderr)
	case err := <-ready:
		if err != nil {
			t.Fatalf("%s: reading the FIFO: %v", what, err)
		}
	}
	if err := os.WriteFile(control, []byte("-memory"), 0); err != nil {
		t.Errorf("%s: writing -memory to %s: %v", what, control, err)
	}
	end = <-done
	if !errors.Is(end.err, errLimitLost) || endedWith(end.err, WhyLimit) || errors.Is(end.err, errDeadline) || end.res.Code != -1 ||
		end.res.End != EndNone || end.res.Signal != nil || end.res.Status != 0 {
		t.Errorf("%s: %v, Code %d, End %q, Signal %v, Status %#x, stdout %q, stderr %q, want errLimitLost, no WhyLimit and no WhyDeadline, Code -1 and EndNone alone",
			what, end.err, end.res.Code, end.res.End, end.res.Signal, end.res.Status, end.res.Stdout, end.res.Stderr)
	}
	runGone(t, what, cgroupLines(end.res.Stdout)["run"])
}

// TestAContainedRunIsNotRefusedWhereTheParentGivesItsChildrenCpu: a helper run Contained, held to 64
// MiB, with the parent's cgroup.subtree_control holding +cpu beside +memory, as a host that delegates
// cpu does, so the run's cgroup has a cpu.max, tries nothing, starts a hog of 16 MiB and exits 0: the
// run returns no error and Code 0, and is not refused as a forgery. The row is serial, and takes the
// +cpu off again after it. Where the parent cannot give cpu, as on a host whose delegation lacks it, the
// row says so and proves no more; where no cgroup is delegated, or cgroup v2 has no nsdelegate, the run
// is refused and starts nothing.
//
// What it costs when this fails: since Go 1.25 the Go runtime of the shim's second stage opens the
// cpu.max it finds under the shim's cgroup mount and keeps that descriptor, a descriptor on a cgroup
// file system, so the check of the run's threads for one refuses every healthy Contained run on a host
// that delegates cpu, as a forgery, and kills its tree.
func TestAContainedRunIsNotRefusedWhereTheParentGivesItsChildrenCpu(t *testing.T) {
	// serial: +cpu on the parent is every other row's too while it is on, and parallel rows start after
	const what = "a Contained helper held to 64 MiB, its parent giving its children cpu"
	if held, _ := containedHere(t); held {
		here, err := selfPath()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		parent := filepath.Join(cgroupMount, filepath.Dir(here))
		file := filepath.Join(parent, "cgroup.subtree_control")
		if err := os.WriteFile(file, []byte("+cpu"), 0); err != nil {
			t.Logf("%s: the parent cannot give cpu here (%v), so the row proves no more than a run without it", what, err)
		} else {
			t.Cleanup(func() {
				if err := os.WriteFile(file, []byte("-cpu"), 0); err != nil {
					t.Errorf("%s: taking +cpu off the parent: %v", what, err)
				}
			})
		}
	}
	res, held, err := containedRun(t, what, "none", "16", "exit", "")
	if !held {
		return
	}
	if err != nil || res.Code != 0 || !bytes.Contains(res.Stdout, []byte("survived\n")) {
		t.Errorf("%s: %v, Code %d, stdout %q, stderr %q, want exit 0 and survived, not refused", what, err, res.Code, res.Stdout, res.Stderr)
	}
}
