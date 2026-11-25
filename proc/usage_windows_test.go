//go:build windows

package proc

import (
	"bufio"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows" //nolint:depguard // arch.ports.23: the tests hold a tree's job and its processes by the calls the package reads them with
)

// pidOf is the pid a helper wrote for word, or the test fails.
func pidOf(t *testing.T, got map[string]string, word string) int {
	t.Helper()
	pid, err := strconv.Atoi(got[word])
	if err != nil {
		t.Fatalf("a helper wrote %q for %s: %v", got[word], word, err)
	}
	return pid
}

// opened opens process pid to wait for its end, for the rest of the test.
func opened(t *testing.T, pid int) windows.Handle {
	t.Helper()
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		t.Fatalf("opening process %d: %v", pid, err)
	}
	t.Cleanup(func() { _ = windows.CloseHandle(h) })
	return h
}

// passed reads the leader's cpu lines from r until one says it has used more CPU time than cpu,
// and fails the test when a line of another form comes first.
func passed(t *testing.T, r *bufio.Reader, cpu time.Duration) {
	t.Helper()
	within(t, "reading the leader's CPU time", func() {
		for {
			line, err := r.ReadString('\n')
			ns, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), "cpu ")
			n, errN := strconv.ParseInt(ns, 10, 64)
			if err != nil || !ok || errN != nil {
				t.Errorf("the leader wrote %q (%v), want cpu and a count of nanoseconds", line, err)
				return
			}
			if time.Duration(n) > cpu {
				return
			}
		}
	})
	if t.Failed() {
		t.FailNow()
	}
}

// TestUsageAndPidsReadADetachedTreeWhileItRunsAndOnceItIsStopped: a detached tree whose leader
// starts two busy children has 3 live processes, and Pids lists the three, the leader's pid and each
// child's. Its CPU time grows between two reads: the second read comes once the leader has itself
// used more CPU time than the whole tree had at the first, so no clock decides it. Once Stop ended
// the tree, and the leader and each child are seen to have ended, the tree has no live process, Pids
// lists none, and its CPU time is no less than before: the test holds its own handle of the job,
// which keeps the job's name once the leader, which held it, is gone.
//
// What it costs when this fails: a caller that watches a tree's CPU time, to bound it or report it,
// reads a figure that does not move, or counts a tree's processes wrong and stops it too soon or
// never; or Pids names a process that is not the tree's, which a caller then ends.
func TestUsageAndPidsReadADetachedTreeWhileItRunsAndOnceItIsStopped(t *testing.T) {
	t.Parallel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	tree, err := Start(Detached{Path: exe, Args: []string{helperMode + "busytree"}, Output: w})
	_ = w.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Stop(tree.ID) })
	br := bufio.NewReader(r)
	got := lines(t, br, 2)
	c1, c2 := pidOf(t, got, "child1"), pidOf(t, got, "child2")
	held, err := openJob(jobQuery, string(tree.ID))
	if err != nil {
		t.Fatalf("holding the job of tree %s: %v", tree.ID, err)
	}
	t.Cleanup(func() { _ = windows.CloseHandle(held) })
	kids := []windows.Handle{opened(t, c1), opened(t, c2)}
	first, err := Usage(tree.ID)
	if err != nil || first.Live != 3 {
		t.Fatalf("Usage(%q) with the leader and two children running: %+v, %v, want Live 3", tree.ID, first, err)
	}
	pids, err := Pids(tree.ID)
	want := []int{tree.Leader.Pid, c1, c2}
	slices.Sort(pids)
	slices.Sort(want)
	if err != nil || !slices.Equal(pids, want) {
		t.Errorf("Pids(%q) with the leader and two children running: %v, %v, want %v", tree.ID, pids, err, want)
	}
	passed(t, br, first.CPU)
	second, err := Usage(tree.ID)
	if err != nil || second.CPU <= first.CPU {
		t.Errorf("Usage(%q) once the leader alone used more than the tree had: CPU %v (%v), want more than %v", tree.ID, second.CPU, err, first.CPU)
	}
	if err := Stop(tree.ID); err != nil {
		t.Fatalf("Stop(%q): %v", tree.ID, err)
	}
	if _, err := tree.Leader.Wait(); err != nil {
		t.Fatalf("the stopped leader: %v", err)
	}
	within(t, "the stopped tree's children ending", func() {
		for _, k := range kids {
			if ev, err := windows.WaitForSingleObject(k, windows.INFINITE); ev != windows.WAIT_OBJECT_0 {
				t.Errorf("waiting for a stopped child: %d, %v", ev, err)
			}
		}
	})
	last, err := Usage(tree.ID)
	if err != nil || last.Live != 0 || last.CPU < second.CPU {
		t.Errorf("Usage(%q) once stopped: %+v, %v, want Live 0 and CPU no less than %v", tree.ID, last, err, second.CPU)
	}
	if pids, err := Pids(tree.ID); err != nil || len(pids) != 0 {
		t.Errorf("Pids(%q) once stopped: %v, %v, want none", tree.ID, pids, err)
	}
}

// TestAProcessThatJoinsItsOwnTreeSeesItsChildInPids: the join helper joins a tree of its own, then
// starts a child; Pids of the id it wrote lists the child and the helper both, and Usage counts both
// live. Stop of the id ends the helper and its child: the pipe they hold ends.
//
// What it costs when this fails: a program that put itself in a tree to account for what it starts
// reads a tree without its own children, or one it cannot stop.
func TestAProcessThatJoinsItsOwnTreeSeesItsChildInPids(t *testing.T) {
	t.Parallel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	joiner := &process{Path: exe, Args: []string{exe, helperMode + "join"}, Stdout: w, Stderr: w}
	err = startTestProcess(joiner)
	_ = w.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = endTestProcess(joiner) })
	got := lines(t, bufio.NewReader(r), 2)
	id := TreeID(got["id"])
	t.Cleanup(func() { _ = Stop(id) })
	child := pidOf(t, got, "child")
	pids, err := Pids(id)
	if err != nil || !slices.Contains(pids, joiner.Process.Pid) || !slices.Contains(pids, child) {
		t.Errorf("Pids(%q) of the joined tree: %v, %v, want the joiner %d and its child %d among them", id, pids, err, joiner.Process.Pid, child)
	}
	if u, err := Usage(id); err != nil || u.Live < 2 {
		t.Errorf("Usage(%q) of the joined tree: %+v, %v, want Live 2 or more", id, u, err)
	}
	if err := Stop(id); err != nil {
		t.Fatalf("Stop(%q) of the joined tree: %v", id, err)
	}
	_ = joiner.Wait() // ended by the stop, so its exit is the stop's
	drained(t, "the joiner and its child, once stopped", r)
}

// TestUsageAndPidsRefuseANameStartDoesNotGiveAndAGoneTreeIsStale: a name that is not
// Local\go-proc-tree- and 32 lowercase hex digits is refused with errTreeID, and one of that form
// that no job has is ErrStale, by Usage and by Pids, which open no job for either.
//
// What it costs when this fails: a read by a caller's text opens a job Start never made, another
// program's or the system's, and reports its processes as the caller's tree; or a gone tree reads as
// a tree with nothing in it.
func TestUsageAndPidsRefuseANameStartDoesNotGiveAndAGoneTreeIsStale(t *testing.T) {
	t.Parallel()
	hex32 := "0123456789abcdef0123456789abcdef"
	rows := []struct {
		id   TreeID
		want error
	}{
		{"", errTreeID},
		{TreeID(jobPrefix + hex32[:31]), errTreeID},
		{TreeID(jobPrefix + "0123456789ABCDEF0123456789abcdef"), errTreeID},
		{TreeID(`Global\go-proc-tree-` + hex32), errTreeID},
		{TreeID(`go-proc-tree-` + hex32), errTreeID},
		{"4242.17", errTreeID},
		{TreeID(randomName()), ErrStale},
	}
	for _, r := range rows {
		if u, err := Usage(r.id); !errors.Is(err, r.want) || u != (TreeUsage{}) {
			t.Errorf("Usage(%q) = %+v, %v, want nothing and %v", r.id, u, err, r.want)
		}
		if pids, err := Pids(r.id); !errors.Is(err, r.want) || pids != nil {
			t.Errorf("Pids(%q) = %v, %v, want nothing and %v", r.id, pids, err, r.want)
		}
	}
	if len(rows) != 7 {
		t.Errorf("%d rows", len(rows))
	}
}
