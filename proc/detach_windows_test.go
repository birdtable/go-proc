//go:build windows

package proc

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"golang.org/x/sys/windows" //nolint:depguard // arch.ports.23: the tests end a Job Object by the call Stop ends it with
)

// counted is a killer that counts each job it ends, and ends it.
func counted(calls *atomic.Int32) killer {
	return killer{terminate: func(job windows.Handle, code uint32) error {
		calls.Add(1)
		return windows.TerminateJobObject(job, code)
	}}
}

// outOfSession checks nothing more on Windows: a tree's leader has no console, which the leader
// says itself, and so no window whose close reaches it.
func outOfSession(*testing.T, TreeID) {}

// stillThere fails the test unless the stale tree's grandchild still runs, and ends it.
func stillThere(t *testing.T, _ Tree, gc int) {
	t.Helper()
	p, err := os.FindProcess(gc)
	if err != nil {
		t.Errorf("opening the stale tree's grandchild %d: %v, want it running", gc, err)
		return
	}
	if err := p.Kill(); err != nil {
		t.Errorf("ending the stale tree's grandchild %d: %v, want it running", gc, err)
	}
	_ = p.Release()
}

// randomName is a job name of the form Start gives, drawn at random.
func randomName() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error: it ends the program when it cannot read
	return jobPrefix + hex.EncodeToString(b[:])
}

// TestStopAndInterruptRefuseANameStartDoesNotGive: a name that is not Local\go-proc-tree- and 32
// lowercase hex digits is refused by Stop and by Interrupt alike, and no job is opened or ended.
//
// What it costs when this fails: a stop by a caller's text opens and ends a job Start never made,
// another program's or the system's, by any name the caller passes; or an interrupt opens one.
func TestStopAndInterruptRefuseANameStartDoesNotGive(t *testing.T) {
	t.Parallel()
	hex32 := "0123456789abcdef0123456789abcdef"
	ids := []TreeID{
		"", TreeID(jobPrefix), TreeID(jobPrefix + hex32[:31]), TreeID(jobPrefix + hex32 + "0"),
		TreeID(jobPrefix + "0123456789ABCDEF0123456789abcdef"), TreeID(jobPrefix + "0123456789abcdeg0123456789abcdef"),
		TreeID(`Global\go-proc-tree-` + hex32), TreeID(`go-proc-tree-` + hex32), TreeID(`local\go-proc-tree-` + hex32),
		TreeID(jobPrefix + hex32[:16] + `\` + hex32[17:]), "4242.17",
	}
	for _, id := range ids {
		for _, g := range guardedActs() {
			var calls atomic.Int32
			if err := g.act(id, counted(&calls)); !errors.Is(err, errTreeID) {
				t.Errorf("%s(%q) = %v, want errTreeID", g.name, id, err)
			}
			if n := calls.Load(); n != 0 {
				t.Errorf("%s(%q) ended %d jobs, want none", g.name, id, n)
			}
		}
	}
}

// TestStopOfANameNoJobHasIsStale: a name of Start's form that no job has is ErrStale to Stop and to
// Interrupt, not Interrupt's errors.ErrUnsupported, and nothing is ended.
//
// What it costs when this fails: a stop of a tree that is gone reads as done, or fails in a way the
// caller cannot tell from a stop that could not reach a live tree; or an interrupt of it reads as
// one of a live tree, and the caller waits for a tree that is not there.
func TestStopOfANameNoJobHasIsStale(t *testing.T) {
	t.Parallel()
	id := TreeID(randomName())
	for _, g := range guardedActs() {
		var calls atomic.Int32
		if err := g.act(id, counted(&calls)); !errors.Is(err, ErrStale) || errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("%s(%q) = %v, want ErrStale alone", g.name, id, err)
		}
		if n := calls.Load(); n != 0 {
			t.Errorf("%s(%q) ended %d jobs, want none", g.name, id, n)
		}
	}
}

// TestInterruptOfALiveTreeIsRefusedAsUnsupportedAndLeavesItRunning: Interrupt of a live tree here
// sends nothing and gives an error that wraps errors.ErrUnsupported, not ErrStale; the leader still
// runs, so a Stop after it ends the job, and the pipe the leader and its grandchild hold ends.
//
// What it costs when this fails: a caller told the tree was asked waits for a tree that heard
// nothing; or Interrupt ends the tree a caller only meant to ask, and its work is lost unsaved.
func TestInterruptOfALiveTreeIsRefusedAsUnsupportedAndLeavesItRunning(t *testing.T) {
	t.Parallel()
	tree, r := treeHelper(t, "hold")
	t.Cleanup(func() { _ = Stop(tree.ID) })
	lines(t, r, 2)
	var calls atomic.Int32
	if err := interruptTree(tree.ID, counted(&calls)); !errors.Is(err, errors.ErrUnsupported) || errors.Is(err, ErrStale) || calls.Load() != 0 {
		t.Errorf("interruptTree(%q) of a live tree: %v after %d ends, want errors.ErrUnsupported, not ErrStale, and none", tree.ID, err, calls.Load())
	}
	if err := Interrupt(tree.ID); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("Interrupt(%q) of a live tree: %v, want errors.ErrUnsupported", tree.ID, err)
	}
	if err := Stop(tree.ID); err != nil {
		t.Fatalf("Stop(%q) after Interrupt: %v, want the tree still there and ended", tree.ID, err)
	}
	drained(t, "the tree's leader and grandchild, once stopped", r)
	if state, err := tree.Leader.Wait(); err != nil || state.ExitCode() != 1 {
		t.Errorf("the leader's wait: %v, %v, want it ended by Stop, with code 1", state, err)
	}
}

// TestAJobNameAJobHasIsNotTaken: a second job made under a name a job has is refused, and is not the
// first opened in its place.
//
// What it costs when this fails: a program that made a job under a name Start draws gets the tree
// put in its job, and holds a handle that ends it or lets it break away.
func TestAJobNameAJobHasIsNotTaken(t *testing.T) {
	t.Parallel()
	name := randomName()
	first, err := createJob(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = windows.CloseHandle(first) })
	if second, err := createJob(name); err == nil || second != 0 {
		if second != 0 {
			_ = windows.CloseHandle(second)
		}
		t.Errorf("createJob(%q) with a job of that name gave handle %v and %v, want a refusal", name, second, err)
	}
}

// TestAProcessStartedWithEveryInheritableHandleAsATreeStartsLeavesItsIDStale: once Start has given
// a tree's leader its handle of the job, and before it closes its own, this test binary starts a
// holding helper as a process is started outside exec, by CreateProcess with every inheritable handle
// of this process handed on, and the helper outlives the leader. It holds no handle of the job: once
// the leader has exited and been reaped, Stop and Interrupt of the tree's id are ErrStale and signal
// nothing, and the grandchild the leader left still runs, as the stale row wants with no helper.
//
// What it costs when this fails: a process any code of the caller's starts while a tree starts keeps
// the tree's job named after its leader is reaped, so a Stop by the old id ends the processes the
// tree left running, and an Interrupt answers the tree as live, not stale.
func TestAProcessStartedWithEveryInheritableHandleAsATreeStartsLeavesItsIDStale(t *testing.T) {
	t.Parallel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// pipe, not os.Pipe, whose ends are inheritable here: the holding helper would take the write end,
	// and the grandchild's pipe would never end however the tree did.
	r, w, err := pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	cmd := exec.Command(exe, helperMode+"tree", "exit", strconv.Itoa(os.Getpid())) //nolint:gosec,noctx // the test binary itself, as a helper; the tree outlives every context
	cmd.Env, cmd.Stdout, cmd.Stderr = Bare(), w, w
	var started error
	tree, err := detachWith(cmd, func(s startStage, _ int) {
		if s == stageFreed {
			started = inheritingAll(t)
		}
	})
	_ = w.Close()
	if err != nil {
		t.Fatal(err)
	}
	if started != nil {
		t.Fatalf("the holding helper did not start: %v", started)
	}
	staleRefused(t, tree, bufio.NewReader(r))
}

// TestAClosedFileOutputGivesTheProgramTheNullDevice: a tree Start starts with an Output that is an
// *os.File already closed has the null device for its stdout, as one with a nil Output does: a
// handle of the character type that is no console and no process.
//
// What it costs when this fails: a closed file has the handle of a nil one, InvalidHandle, which
// exec hands on and which is the caller's own pseudo-handle, so the program's stdout and stderr are
// a handle to the caller's process, with which it can read and write the caller's memory.
func TestAClosedFileOutputGivesTheProgramTheNullDevice(t *testing.T) {
	t.Parallel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	f, err := os.CreateTemp(dir, "output")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	kind := filepath.Join(dir, "kind")
	tree, err := Start(Detached{Path: exe, Args: []string{helperMode + "out-kind", kind}, Dir: dir, Env: Bare("GORACE"), Output: f})
	if err != nil {
		t.Fatal(err)
	}
	if state, err := tree.Leader.Wait(); err != nil || !state.Success() {
		t.Fatalf("the leader: %v, %v, want it to exit 0", state, err)
	}
	got, err := os.ReadFile(kind)
	if want := fmt.Sprintf("type %d console no process no\n", windows.FILE_TYPE_CHAR); err != nil || string(got) != want {
		t.Errorf("a tree started with a closed *os.File Output: its stdout %q (%v), want %q", got, err, want)
	}
}
