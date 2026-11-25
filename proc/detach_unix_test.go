//go:build linux || darwin

package proc

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"

	"golang.org/x/sys/unix" //nolint:depguard // arch.ports.23: a process's session is read by getsid, which syscall does not give on every Unix
)

// counted is a killer that counts each signal it sends, and sends it.
func counted(calls *atomic.Int32) killer {
	return killer{
		signal: func(pid int, sig syscall.Signal) error {
			calls.Add(1)
			return signalPid(syscall.Kill, pid, sig)
		},
		pause: func() {},
	}
}

// outOfSession fails the test unless the leader id names leads a session of its own, not this
// test's: a terminal's hangup when it closes reaches its session alone.
func outOfSession(t *testing.T, id TreeID) {
	t.Helper()
	pid, _, _, ok := parseTreeID(id)
	if !ok {
		t.Fatalf("Start gave %q, which is not a tree id", id)
	}
	sid, err := unix.Getsid(pid)
	if err != nil {
		t.Fatalf("reading the session of leader %d: %v", pid, err)
	}
	ours, err := unix.Getsid(0)
	if err != nil {
		t.Fatal(err)
	}
	if sid != pid || sid == ours {
		t.Errorf("leader %d is in session %d, this test in %d: want a session the leader leads", pid, sid, ours)
	}
}

// endTestProcess ends c, a process a test started, by SIGKILL to its pid, through signalPid. It
// builds where its callers, the detach rows, build: here on Linux and macOS, and on Windows in
// helper_windows_test.go.
func endTestProcess(c *process) error {
	return signalPid(syscall.Kill, c.Process.Pid, syscall.SIGKILL)
}

// stillThere fails the test unless the stale tree's group still has a process, its grandchild, and
// ends it. The group's pid cannot be another's while the group has a process.
func stillThere(t *testing.T, tree Tree, _ int) {
	t.Helper()
	if err := signalPid(syscall.Kill, -tree.Leader.Pid, syscall.SIGKILL); err != nil {
		t.Errorf("killing the stale tree's group %d, which holds its grandchild: %v, want it there", tree.Leader.Pid, err)
	}
}

// TestALeaderThatLeadsNoGroupOfItsOwnNamesNoTree: named refuses a leader read in another process
// group, and one whose start time was not read, with the read's error; a leader of its own group is
// its pid, a dot and its start time.
//
// What it costs when this fails: Start names a tree by a process that does not lead its group, and
// Stop later kills a group the program never led, its starter's among them.
func TestALeaderThatLeadsNoGroupOfItsOwnNamesNoTree(t *testing.T) {
	t.Parallel()
	if id, err := named(5, 4, 7, "", nil); err == nil || id != "" {
		t.Errorf("named of a leader in another group = %q, %v, want no id and an error", id, err)
	}
	unread := errors.New("unread")
	if id, err := named(5, 5, 7, "", unread); !errors.Is(err, unread) || id != "" {
		t.Errorf("named of a leader not read = %q, %v, want no id and the read's error", id, err)
	}
	if id, err := named(5, 5, 7, "", nil); err != nil || id != "5.7" {
		t.Errorf("named of a leader of its own group = %q, %v, want 5.7", id, err)
	}
	const b = "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0.4026531836.4026531834"
	if id, err := named(5, 5, 7, b, nil); err != nil || id != "5.7."+b {
		t.Errorf("named of a leader of its own group, with a place = %q, %v, want 5.7.%s", id, err, b)
	}
}

// TestStopAndInterruptRefuseAnIDStartDoesNotGive: an id that is not a pid above 1 and a start time,
// each in decimal as Start writes it, is refused by Stop and by Interrupt alike, and nothing is
// signalled.
//
// What it costs when this fails: an id of 0, -1 or 1 kills or interrupts this process's own group,
// every process it can reach, or init's group; one Start never gave may name a group it never
// started.
func TestStopAndInterruptRefuseAnIDStartDoesNotGive(t *testing.T) {
	t.Parallel()
	ids := []TreeID{
		"", "5", "5.", ".5", "0.5", "1.5", "-1.5", "-5.3", "+5.3", "05.3", "5.03", "5.+3", " 5.3", "5.3 ", "5.3\n",
		"5.3.1", "5,3", "0x5.3", "2147483648.1", "5.18446744073709551616", `Local\go-proc-tree-00000000000000000000000000000000`,
		// A place that is not a boot, a pid namespace and a time namespace as the kernel writes them.
		"5.3.", "5.3.0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0", "5.3.0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0.1",
		"5.3.0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f.1.0", "5.3.0F1E2D3C-4B5A-6978-8796-A5B4C3D2E1F0.1.0",
		"5.3.0f1e2d3c4-b5a-6978-8796-a5b4c3d2e1f0.1.0", "5.3.0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1fg.1.0",
		"5.3.0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0.0.0", "5.3.0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0.01.0",
		"5.3.0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0.1.00", "5.3.0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0.-1.0",
		"5.3.0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0.1.", "5.3.0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0..0",
		"5.3.0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0.1.0.7", "5.3.0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0.1.18446744073709551616",
	}
	// An id of the other form: with no place where this system's ids carry one, and with one where
	// they do not.
	if here, err := placeHere(); err != nil {
		t.Fatal(err)
	} else if here != "" {
		ids = append(ids, "5.3")
	} else {
		ids = append(ids, "5.3.0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0.4026531836.0")
	}
	for _, id := range ids {
		for _, g := range guardedActs() {
			var calls atomic.Int32
			if err := g.act(id, counted(&calls)); !errors.Is(err, errTreeID) {
				t.Errorf("%s(%q) = %v, want errTreeID", g.name, id, err)
			}
			if n := calls.Load(); n != 0 {
				t.Errorf("%s(%q) signalled %d times, want none", g.name, id, n)
			}
		}
	}
}

// TestStopKillsOnlyTheLeaderTheIDNames: an id with the leader's pid and another start time, as a
// process that took a reaped leader's pid is named, and one with the pid and start time of the
// grandchild, which leads no group, are ErrStale to Stop and to Interrupt and signal nothing; the
// leader's own id kills the group.
//
// What it costs when this fails: a stop or an interrupt reaches a process that took a reaped
// leader's pid, or a process that is in a group but leads none, and signals a group the id never
// named.
func TestStopKillsOnlyTheLeaderTheIDNames(t *testing.T) {
	t.Parallel()
	tree, r := treeHelper(t, "hold")
	t.Cleanup(func() { _ = Stop(tree.ID) })
	got := lines(t, r, 2)
	gc, err := strconv.Atoi(got["grandchild"])
	if err != nil {
		t.Fatalf("the leader wrote %q for its grandchild's pid: %v", got["grandchild"], err)
	}
	pid, born, b, ok := parseTreeID(tree.ID)
	if !ok || pid != tree.Leader.Pid {
		t.Fatalf("Start gave id %q for leader %d", tree.ID, tree.Leader.Pid)
	}
	_, gcBorn, err := leaderHere(gc)
	if err != nil {
		t.Fatalf("reading grandchild %d: %v", gc, err)
	}
	onBoot := "" // the boot the tree's id carries, where the system puts one there
	if b != "" {
		onBoot = "." + b
	}
	for _, id := range []TreeID{
		TreeID(strconv.Itoa(pid) + "." + strconv.FormatUint(born+1, 10) + onBoot),
		TreeID(strconv.Itoa(gc) + "." + strconv.FormatUint(gcBorn, 10) + onBoot),
	} {
		for _, g := range guardedActs() {
			var calls atomic.Int32
			if err := g.act(id, counted(&calls)); !errors.Is(err, ErrStale) {
				t.Errorf("%s(%q) = %v, want ErrStale", g.name, id, err)
			}
			if n := calls.Load(); n != 0 {
				t.Errorf("%s(%q) signalled %d times, want none", g.name, id, n)
			}
		}
	}
	var calls atomic.Int32
	if err := stopTree(tree.ID, counted(&calls)); err != nil || calls.Load() == 0 {
		t.Fatalf("stopTree(%q), the leader's own id: %v after %d signals, want the group killed", tree.ID, err, calls.Load())
	}
	drained(t, "the tree's leader and grandchild, once stopped", r)
	state, err := tree.Leader.Wait()
	if ws, ok := state.Sys().(syscall.WaitStatus); err != nil || !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Errorf("the stopped leader's wait: %v, %v, want it ended by SIGKILL", state, err)
	}
}

// TestALeaderIsReadByItsPid: this process's group and start time are read, the same each time,
// and a child once reaped is read as no process, or as another with another start time.
//
// What it costs when this fails: Stop's guard reads a group or a start time that is not the
// process's, and refuses every live tree, or takes a new process for a reaped leader.
func TestALeaderIsReadByItsPid(t *testing.T) {
	t.Parallel()
	pgid, born, err := leaderHere(os.Getpid())
	if err != nil || pgid != syscall.Getpgrp() {
		t.Fatalf("leader(this process) = %d, %d, %v, want group %d", pgid, born, err, syscall.Getpgrp())
	}
	if _, again, err := leaderHere(os.Getpid()); err != nil || again != born {
		t.Errorf("leader(this process) read again gives start time %d (%v), want %d", again, err, born)
	}
	child, err := helping("hold")
	if err != nil {
		t.Fatal(err)
	}
	pid := child.Process.Pid
	_, childBorn, err := leaderHere(pid)
	if err != nil {
		t.Fatalf("leader(a live child) = %v", err)
	}
	if err := signalPid(syscall.Kill, pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	if _, now, err := leaderHere(pid); !errors.Is(err, errNoProcess) && (err != nil || now == childBorn) {
		t.Errorf("leader(a reaped child's pid) = start time %d, %v, want errNoProcess or a start time other than %d", now, err, childBorn)
	}
}

// TestAStopTheKillDoesNotReachSaysWhatTheKillAnswered: with the guard holding for a live tree, a
// first kill answered with EPERM gives an error wrapping syscall.EPERM, not os.ErrProcessDone and
// not ErrStale; one answered with ESRCH, the leader reaped between the guard and the kill, is
// ErrStale. Each sends that one kill alone. Interrupt's signal, answered the same, gives the same.
// The tree is then stopped.
//
// What it costs when this fails: a live tree the caller may not signal reads as one that ended,
// and runs on unseen; or a tree gone since the guard reads as an error, not as the stale id the Stop
// and Interrupt docs say it is.
func TestAStopTheKillDoesNotReachSaysWhatTheKillAnswered(t *testing.T) {
	t.Parallel()
	tree, r := treeHelper(t, "hold")
	t.Cleanup(func() { _ = Stop(tree.ID) })
	lines(t, r, 2)
	answering := func(e syscall.Errno, calls *atomic.Int32) killer {
		return killer{signal: func(int, syscall.Signal) error { calls.Add(1); return e }, pause: func() {}}
	}
	var calls atomic.Int32
	err := stopTree(tree.ID, answering(syscall.EPERM, &calls))
	if !errors.Is(err, syscall.EPERM) || errors.Is(err, os.ErrProcessDone) || errors.Is(err, ErrStale) || calls.Load() != 1 {
		t.Errorf("stopTree answered EPERM: %v after %d kills, want one kill and syscall.EPERM, not os.ErrProcessDone or ErrStale", err, calls.Load())
	}
	calls.Store(0)
	if err := stopTree(tree.ID, answering(syscall.ESRCH, &calls)); !errors.Is(err, ErrStale) || calls.Load() != 1 {
		t.Errorf("stopTree answered ESRCH: %v after %d kills, want one kill and ErrStale", err, calls.Load())
	}
	calls.Store(0)
	err = interruptTree(tree.ID, answering(syscall.EPERM, &calls))
	if !errors.Is(err, syscall.EPERM) || errors.Is(err, os.ErrProcessDone) || errors.Is(err, ErrStale) || calls.Load() != 1 {
		t.Errorf("interruptTree answered EPERM: %v after %d signals, want one and syscall.EPERM, not os.ErrProcessDone or ErrStale", err, calls.Load())
	}
	calls.Store(0)
	if err := interruptTree(tree.ID, answering(syscall.ESRCH, &calls)); !errors.Is(err, ErrStale) || calls.Load() != 1 {
		t.Errorf("interruptTree answered ESRCH: %v after %d signals, want one and ErrStale", err, calls.Load())
	}
	if err := Stop(tree.ID); err != nil {
		t.Fatalf("Stop(%q) of the live tree: %v", tree.ID, err)
	}
	drained(t, "the tree's leader and grandchild, once stopped", r)
	_, _ = tree.Leader.Wait()
}

// TestAClosedFileOutputGivesTheProgramTheNullDevice: a tree Start starts with an Output that is an
// *os.File already closed has the null device on fds 1 and 2, as one with a nil Output does: the
// program makes its mark only when both are the null device.
//
// What it costs when this fails: a closed file has the descriptor of a nil one, none, which exec
// hands on, so the program starts with fds 1 and 2 closed, and the first files it opens, a key or a
// socket, take what it writes as its output.
func TestAClosedFileOutputGivesTheProgramTheNullDevice(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	f, err := os.CreateTemp(dir, "output")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	mark := filepath.Join(dir, "null")
	script := `if [ /dev/fd/1 -ef /dev/null ] && [ /dev/fd/2 -ef /dev/null ]; then : > "$1"; fi`
	tree, err := Start(Detached{Path: "/bin/sh", Args: []string{"-c", script, "sh", mark}, Dir: dir, Output: f})
	if err != nil {
		t.Fatal(err)
	}
	if state, err := tree.Leader.Wait(); err != nil || !state.Success() {
		t.Fatalf("the leader: %v, %v, want it to exit 0", state, err)
	}
	if _, err := os.Stat(mark); err != nil {
		t.Errorf("a tree started with a closed *os.File Output made no mark (%v), want fds 1 and 2 on the null device", err)
	}
}

// leaderHere is leader through procMount, opened and checked by procOwn, as Stop reads it.
func leaderHere(pid int) (pgid int, born uint64, err error) {
	r, err := procOwn(procFS{dir: procMount})
	if err != nil {
		return 0, 0, err
	}
	defer r.close()
	return leader(r, pid)
}

// placeHere is place through procMount, opened and checked by procOwn, as Start reads it.
func placeHere() (string, error) {
	r, err := procOwn(procFS{dir: procMount})
	if err != nil {
		return "", err
	}
	defer r.close()
	return place(r)
}

// recording is a killer that records each signal it sends, to which pid, and sends it.
func recording(got *[]string) killer {
	return killer{
		signal: func(pid int, sig syscall.Signal) error {
			*got = append(*got, strconv.Itoa(pid)+" "+sig.String())
			return signalPid(syscall.Kill, pid, sig)
		},
		pause: func() {},
	}
}

// answerTree starts the answer helper, how, detached, and waits until its leader and the child it
// starts each say they answer SIGINT as how says.
func answerTree(t *testing.T, how string) (Tree, *bufio.Reader) {
	t.Helper()
	tree, r := helperTree(t, "answer", how, "leader", strconv.Itoa(os.Getpid()))
	t.Cleanup(func() { _ = Stop(tree.ID) })
	if got := lines(t, r, 2); got["leader"] != "ready" || got["child"] != "ready" {
		t.Fatalf("the answer helpers wrote %q, want the leader and the child ready", got)
	}
	return tree, r
}

// killedBy fails the test unless the tree's leader, waited for, was ended by SIGKILL: by Stop, and
// not by a signal before it.
func killedBy(t *testing.T, tree Tree) {
	t.Helper()
	state, err := tree.Leader.Wait()
	if ws, ok := state.Sys().(syscall.WaitStatus); err != nil || !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Errorf("the leader's wait: %v, %v, want it ended by Stop's SIGKILL", state, err)
	}
}

// TestInterruptSendsTheTreeOneSIGINTAndKillsNothing: Interrupt of a live tree sends one signal,
// SIGINT, to the tree's process group, and returns nil; the leader and the child it started each
// catch it, and both run on, so a Stop after it finds the leader still the tree's and kills the
// group with SIGKILL.
//
// What it costs when this fails: a caller that asks a tree to stop and waits for it to save its
// work has the tree killed instead, or only its leader asked, its children left to be killed
// unwarned; or a second signal, a SIGSTOP on macOS, reaches a tree that was only to be asked.
func TestInterruptSendsTheTreeOneSIGINTAndKillsNothing(t *testing.T) {
	t.Parallel()
	tree, r := answerTree(t, "catch")
	var got []string
	if err := interruptTree(tree.ID, recording(&got)); err != nil {
		t.Fatalf("interruptTree(%q) of the live tree: %v", tree.ID, err)
	}
	if want := []string{strconv.Itoa(-tree.Leader.Pid) + " " + syscall.SIGINT.String()}; !slices.Equal(got, want) {
		t.Fatalf("interruptTree(%q) sent %q, want %q alone", tree.ID, got, want)
	}
	if got := lines(t, r, 2); got["leader"] != "interrupted" || got["child"] != "interrupted" {
		t.Errorf("after Interrupt the tree wrote %q, want the leader and the child interrupted", got)
	}
	if err := Stop(tree.ID); err != nil {
		t.Fatalf("Stop(%q) after Interrupt: %v, want the tree still there and killed", tree.ID, err)
	}
	drained(t, "the tree's leader and child, once stopped", r)
	killedBy(t, tree)
}

// TestATreeThatIgnoresSIGINTRunsOnAfterInterruptAndStopEndsIt: Interrupt of a tree whose leader
// and child ignore SIGINT returns nil, and both run on: each answers a SIGUSR1 sent after it. A
// Stop then kills the group.
//
// What it costs when this fails: Interrupt, told that a program may ignore it, kills that program
// in its place, or a caller's Stop after its own wait finds no tree, and a tree the doc says runs
// on ends unasked.
func TestATreeThatIgnoresSIGINTRunsOnAfterInterruptAndStopEndsIt(t *testing.T) {
	t.Parallel()
	tree, r := answerTree(t, "ignore")
	if err := Interrupt(tree.ID); err != nil {
		t.Fatalf("Interrupt(%q) of the live tree: %v", tree.ID, err)
	}
	if err := signalPid(syscall.Kill, -tree.Leader.Pid, syscall.SIGUSR1); err != nil {
		t.Fatalf("asking the tree's group %d whether it runs on: %v", tree.Leader.Pid, err)
	}
	if got := lines(t, r, 2); got["leader"] != "alive" || got["child"] != "alive" {
		t.Errorf("after Interrupt the tree wrote %q, want the leader and the child alive", got)
	}
	if err := Stop(tree.ID); err != nil {
		t.Fatalf("Stop(%q) after Interrupt: %v, want the tree still there and killed", tree.ID, err)
	}
	drained(t, "the tree's leader and child, once stopped", r)
	killedBy(t, tree)
}
