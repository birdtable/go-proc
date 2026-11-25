package proc

import (
	"bufio"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix" //nolint:depguard // arch.ports.23: the late-fork row reads its leader's state by the kern.proc.pid sysctl, whose struct is in x/sys alone
)

// TestStopOnMacOSKillsTheGroupNoMoreOnceTheLeaderIsStale: on macOS a stop kills a tree's group again
// until no live process answers. Here the leader, killed by the first kill, is reaped before the
// second, as its parent may reap it at any time, so its pid is free to be taken; the guard, read
// again, no longer holds, and no second kill goes to the group by its number. Stop goes on by
// member, finds the group gone, and answers nil.
//
// What it costs when this fails: Stop, which is not the leader's parent, kills a group by its number
// again after the leader was reaped and its pid taken, and the later kill reaches another process's
// group.
func TestStopOnMacOSKillsTheGroupNoMoreOnceTheLeaderIsStale(t *testing.T) {
	t.Parallel()
	tree, r := treeHelper(t, "hold")
	_ = lines(t, r, 2)
	groupKills := 0
	k := killer{signal: func(pid int, sig syscall.Signal) error {
		if pid < 0 && sig == syscall.SIGKILL {
			groupKills++
		}
		err := signalPid(syscall.Kill, pid, sig)
		if groupKills == 1 && sig == syscall.SIGKILL {
			_, _ = tree.Leader.Wait() // the leader, killed, is reaped: its pid may now be taken
		}
		return err
	}, pause: func() {}}
	if err := stopTree(tree.ID, k); err != nil {
		t.Errorf("stopTree(%q): %v, want the group killed", tree.ID, err)
	}
	if groupKills != 1 {
		t.Errorf("stopTree(%q) killed the group by its number %d times with the leader reaped after the first, want once", tree.ID, groupKills)
	}
	drained(t, "the tree's leader and grandchild, once stopped", r)
}

// TestStopOnMacOSLeavesNoProcessOfATreeWhoseLeaderIsReapedMidFork: a tree Start started runs the
// fork loop of TestACancelMidForkLeavesNoProcessInTheGroup, and its starter waits on its leader, so
// the leader is reaped as soon as the first kill ends it, while a process of the tree is mid-fork. A
// process forked as the kill lands is not reached by it, and the guard no longer holds once the
// leader is reaped, so Stop goes on by member: once Stop returns nil, no process of the group is
// alive, in every one of forkRuns runs. Stop runs as stop with ender's killer, each kill logged, so
// a failure says what each kill answered and lists what the group held as Stop returned and at the
// probe.
//
// What it costs when this fails: Stop answers nil for a tree a process of which runs on, out of reach
// of any later stop, since the id no longer names a leader.
func TestStopOnMacOSLeavesNoProcessOfATreeWhoseLeaderIsReapedMidFork(t *testing.T) {
	// serial: each run's shell forks forkBound sleeps without pause, so beside other tests that fork it slows their forks, and theirs can slow the first fork it waits on to the test's deadline
	ran, midFork := 0, 0
	for run := range forkRuns {
		ran++
		dir := t.TempDir()
		fifo, ended := filepath.Join(dir, "forked"), filepath.Join(dir, "ended")
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Fatal(err)
		}
		tree, err := Start(Detached{Path: "/bin/sh", Args: []string{"-c", forkScript, fifo, strconv.Itoa(forkBound), ended}})
		if err != nil {
			t.Fatal(err)
		}
		pgid := tree.Leader.Pid
		reaped := make(chan struct{})
		go func() {
			_, _ = tree.Leader.Wait() // the starter waits on its leader
			close(reaped)
		}()
		within(t, "the shell's first fork", func() {
			data, err := os.ReadFile(fifo)
			if err != nil || len(data) == 0 {
				t.Errorf("run %d: the fifo read %q, %v, want the shell's line", run+1, data, err)
			}
		})
		var kills killLog
		if err := stop(tree.ID, procFS{dir: procMount}, kills.killer()); err != nil { // Stop, with each kill logged
			t.Errorf("run %d: Stop(%q): %v, want nil", run+1, tree.ID, err)
		}
		left := groupState(pgid)
		within(t, "the leader's reap", func() { <-reaped })
		if _, err := os.Stat(ended); errors.Is(err, os.ErrNotExist) {
			midFork++
		}
		if err := signalPid(syscall.Kill, -pgid, 0); err == nil {
			t.Errorf("run %d: a process of the group -%d is alive after Stop returned\nStop's kills:\n%s\nthe group as Stop returned:\n%s\nthe group at the probe:\n%s",
				run+1, pgid, &kills, left, groupState(pgid))
			killed(t, pgid)
		} else if !errors.Is(err, syscall.ESRCH) && !errors.Is(err, syscall.EPERM) {
			t.Errorf("run %d: the probe of the group -%d: %v, want it gone", run+1, pgid, err)
		}
	}
	if ran == 0 {
		t.Fatal("no run ran")
	}
	if midFork == 0 {
		t.Errorf("each of the %d runs' loops of %d forks ended before its stop, so none was stopped mid-fork: raise forkBound", ran, forkBound)
	}
}

// TestStopOnMacOSStopsTheTreeBeforeItsKill: Stop listed the group once before its kill, and a process
// the tree forked after that listing, whose exec finished as the kill ended its parent, was missed by
// that kill and by every later one, and was in no listing in that moment: the kills after the first
// answer EPERM, as a group of zombies does, and no listing named it to liveIn. The row's
// hidden grandchild is a model of the moment measured, not the kernel's own escape: the
// late-fork helper's leader forks a grandchild on SIGUSR1. At the group's first SIGKILL the row's
// killer reads the leader's state. Stopped, it sends nothing more; running, it sends SIGUSR1 and waits
// for the grandchild's line, bounded only by the test's deadline. That kill then reaches every member
// of the group but the new one, by pid, and each later kill of the group answers EPERM. A SIGSTOP of
// the group must come before that first SIGKILL. So the row shows that Stop kills a stopped tree,
// whose leader forks nothing as the kill lands; it does not show the kernel's escape closed.
//
// What it costs when this fails: Stop answers nil for a tree a process of which runs on, out of reach
// of any later stop, since the id no longer names a live leader.
func TestStopOnMacOSStopsTheTreeBeforeItsKill(t *testing.T) {
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
	tree, err := Start(Detached{Path: exe, Args: []string{helperMode + "late-fork", strconv.Itoa(os.Getpid())}, Output: w})
	_ = w.Close()
	if err != nil {
		t.Fatal(err)
	}
	pgid, late := tree.Leader.Pid, 0
	t.Cleanup(func() {
		if late != 0 {
			_ = signalPid(syscall.Kill, late, syscall.SIGKILL)
		}
		_ = Stop(tree.ID)
		_, _ = tree.Leader.Wait()
	})
	br := bufio.NewReader(r)
	_ = lines(t, br, 1)
	groupKills, stoppedFirst := 0, false
	k := killer{signal: func(pid int, sig syscall.Signal) error {
		if pid == -pgid && sig == syscall.SIGSTOP && groupKills == 0 {
			stoppedFirst = true
		}
		if pid != -pgid || sig != syscall.SIGKILL {
			return signalPid(syscall.Kill, pid, sig)
		}
		if groupKills++; groupKills > 1 {
			return syscall.EPERM
		}
		kp, err := unix.SysctlKinfoProc("kern.proc.pid", pgid)
		if err != nil {
			t.Fatalf("reading the leader's state at the first kill: %v", err)
		}
		if kp.Proc.P_stat != 4 {
			if err := signalPid(syscall.Kill, pgid, syscall.SIGUSR1); err != nil {
				t.Fatalf("the SIGUSR1 to the running leader: %v", err)
			}
			n, err := strconv.Atoi(lines(t, br, 1)["late"])
			if err != nil {
				t.Fatalf("the leader's late line: %v", err)
			}
			late = n
		}
		members, err := groupMembers(pgid)
		if err != nil {
			t.Fatalf("listing the group: %v", err)
		}
		for _, m := range members {
			if m.pid != late {
				_ = signalPid(syscall.Kill, m.pid, syscall.SIGKILL)
			}
		}
		return nil
	}, pause: func() {}}
	if err := stopTree(tree.ID, k); err != nil {
		t.Errorf("stopTree(%q): %v, want the tree killed", tree.ID, err)
	}
	if !stoppedFirst || groupKills == 0 {
		t.Errorf("stopTree(%q) sent %d group kills, a SIGSTOP of the group before the first %v; want a SIGSTOP, then a kill", tree.ID, groupKills, stoppedFirst)
	}
	if late != 0 && signalPid(syscall.Kill, late, 0) == nil {
		t.Errorf("stopTree(%q) returned with process %d, forked after its listing and missed by its kill, running on: the leader was not stopped before the kill (%d group kills)", tree.ID, late, groupKills)
	}
}

// TestStopOnMacOSSaysWhatTheKillAfterItsStopAnswered: once Stop has stopped a live tree, its first
// SIGKILL answered with EPERM is an error wrapping syscall.EPERM, not os.ErrProcessDone and not
// ErrStale, and one answered with ESRCH is ErrStale. Each comes after a SIGSTOP of the group, is the
// one kill sent, and no SIGCONT is: the tree is left stopped, and a second Stop of its id kills it.
//
// What it costs when this fails: a stopped tree the kill did not reach reads as one that ended, and
// stays stopped unseen; or Stop wakes a tree it could not kill.
func TestStopOnMacOSSaysWhatTheKillAfterItsStopAnswered(t *testing.T) {
	t.Parallel()
	for _, e := range []syscall.Errno{syscall.EPERM, syscall.ESRCH} {
		tree, r := treeHelper(t, "hold")
		t.Cleanup(func() { _ = Stop(tree.ID) })
		lines(t, r, 2)
		pgid := tree.Leader.Pid
		var sent []syscall.Signal
		k := killer{signal: func(pid int, sig syscall.Signal) error {
			if pid == -pgid {
				sent = append(sent, sig)
			}
			if pid == -pgid && sig == syscall.SIGKILL {
				return e
			}
			return signalPid(syscall.Kill, pid, sig)
		}, pause: func() {}}
		err := stopTree(tree.ID, k)
		switch {
		case e == syscall.EPERM && (!errors.Is(err, syscall.EPERM) || errors.Is(err, os.ErrProcessDone) || errors.Is(err, ErrStale)):
			t.Errorf("stopTree's kill after its stop answered EPERM: %v, want syscall.EPERM, not os.ErrProcessDone or ErrStale", err)
		case e == syscall.ESRCH && !errors.Is(err, ErrStale):
			t.Errorf("stopTree's kill after its stop answered ESRCH: %v, want ErrStale", err)
		}
		if len(sent) == 0 || sent[0] != syscall.SIGSTOP || sent[len(sent)-1] != syscall.SIGKILL || slices.Contains(sent, syscall.SIGCONT) || slices.Index(sent, syscall.SIGKILL) != len(sent)-1 {
			t.Errorf("stopTree, its kill answered %v, sent the group %v; want SIGSTOPs, then one SIGKILL, and no SIGCONT", e, sent)
		}
		if kp, err := unix.SysctlKinfoProc("kern.proc.pid", pgid); err != nil {
			t.Errorf("reading the leader after stopTree's kill answered %v: %v", e, err)
		} else if kp.Proc.P_stat != 4 {
			t.Errorf("the leader after stopTree's kill answered %v: state %d; want it left stopped (4)", e, kp.Proc.P_stat)
		}
		if err := Stop(tree.ID); err != nil {
			t.Fatalf("a second Stop(%q) of the tree left stopped: %v", tree.ID, err)
		}
		drained(t, "the stopped tree's leader and grandchild, once stopped again", r)
		_, _ = tree.Leader.Wait()
	}
}

// TestStopOnMacOSKillsAProcessMidExecAtTheKillOnceItsExecIsThrough: a process of the tree that
// Stop's kill misses, as it misses a process mid-exec, is killed once a kill of 0 to it has waited
// out the exec, and one no kill of the group reaches is an error of Stop, never its success. It is
// The moment on Stop's path: a process whose exec is finishing as its parent
// exits is, for a moment, in no listing of its group, a SIGKILL of the group misses it, and the kills
// after answer EPERM. The rows plant that moment in Stop's signal, for the tree's grandchild: while
// it is hidden, the group's first SIGKILL reaches the leader alone, and the rest answer EPERM. In the
// first row the grandchild's own kill of 0, which in the kernel waits out the exec, ends the hiding;
// in the second nothing does.
//
// What it costs when this fails: Stop answers nil for a tree a process of which runs on, out of reach
// of any later stop, since the id no longer names a live leader.
func TestStopOnMacOSKillsAProcessMidExecAtTheKillOnceItsExecIsThrough(t *testing.T) {
	t.Parallel()
	for _, hidden := range []bool{false, true} {
		t.Run(map[bool]string{false: "its exec ends", true: "it stays hidden"}[hidden], func(t *testing.T) {
			t.Parallel()
			tree, r := treeHelper(t, "hold")
			t.Cleanup(func() {
				_ = Stop(tree.ID)
				_, _ = tree.Leader.Wait()
			})
			fields := strings.Fields(lines(t, r, 2)["grandchild"])
			grandchild, err := strconv.Atoi(append(fields, "")[0])
			if err != nil {
				t.Fatalf("the tree helper's grandchild line %q: %v", fields, err)
			}
			ended := make(chan struct{})
			go func() {
				_, _ = io.Copy(io.Discard, r) // the pipe ends once the leader and the grandchild are gone
				close(ended)
			}()
			var mu sync.Mutex
			midExec, groupKills := true, 0
			k := killer{signal: func(pid int, sig syscall.Signal) error {
				mu.Lock()
				defer mu.Unlock()
				switch {
				case midExec && !hidden && pid == grandchild && sig == 0:
					midExec = false
				case midExec && pid == -tree.Leader.Pid && sig == syscall.SIGKILL:
					groupKills++
					if groupKills == 1 {
						return signalPid(syscall.Kill, tree.Leader.Pid, syscall.SIGKILL)
					}
					return syscall.EPERM
				}
				return signalPid(syscall.Kill, pid, sig)
			}, pause: func() {}}
			err = stopTree(tree.ID, k)
			if hidden {
				if !errors.Is(err, errTreeRunsOn) {
					t.Errorf("stopTree(%q), its grandchild live past every kill: %v, want errTreeRunsOn", tree.ID, err)
				}
				return
			}
			if err != nil {
				t.Errorf("stopTree(%q): %v, want the tree killed", tree.ID, err)
			}
			select {
			case <-ended:
			case <-time.After(10 * time.Second):
				t.Errorf("a grandchild Stop's kill missed mid-exec runs on 10s past Stop's end, want it killed once its exec is through")
			}
		})
	}
}

// TestAMemberThatAnswersEPERMIsLiveAndNotFrozen: liveIn and frozen, asked by a signal that answers
// EPERM to a signal 0 of this test's own process, which runs in its own group, neither stopped, a
// zombie nor exiting, read it live and not frozen, as a member of the tree whose uid changed (a
// command under sudo) answers; one that answers ESRCH they read gone. A native row with a real such
// member needs a second account or root, which this test cannot make: it is left to the Tag check.
//
// What it costs when this fails: a member of the group whose uid changed is not stopped, not killed
// and asked after by nobody, and Stop answers nil while it runs on.
func TestAMemberThatAnswersEPERMIsLiveAndNotFrozen(t *testing.T) {
	t.Parallel()
	pid, pgid := os.Getpid(), unix.Getpgrp()
	for _, tc := range []struct {
		answer error
		live   bool
	}{
		{syscall.EPERM, true},
		{syscall.ESRCH, false},
	} {
		signal := func(p int, sig syscall.Signal) error {
			if p != pid || sig != 0 {
				t.Errorf("a signal %d to %d, want only a signal 0 to %d", sig, p, pid)
			}
			return tc.answer
		}
		if live, froze := liveIn(signal, pgid, []int{pid}), frozen(signal, pgid, pid); live != tc.live || froze == tc.live {
			t.Errorf("a running member that answers %v: live %t and frozen %t, want live %t and frozen %t", tc.answer, live, froze, tc.live, !tc.live)
		}
	}
}
