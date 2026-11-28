//go:build unix

package proc

import (
	"context"
	"errors"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// kills is a group's signal that counts the kills it passes on, and notes of
// each its signal and whether the leader was reaped by then. A SIGSTOP, the
// freeze before the group is listed, is no kill: it is noted in stops, as the
// number of kills before it.
type kills struct {
	cmd    *process
	mu     sync.Mutex
	pids   []int
	sigs   []syscall.Signal
	reaped []bool
	stops  []int
}

func (k *kills) send(pid int, sig syscall.Signal) error {
	k.mu.Lock()
	if sig == syscall.SIGSTOP {
		k.stops = append(k.stops, len(k.pids))
		k.mu.Unlock()
		return signalPid(syscall.Kill, pid, sig)
	}
	k.pids = append(k.pids, pid)
	k.sigs = append(k.sigs, sig)
	k.reaped = append(k.reaped, k.cmd.ProcessState != nil)
	k.mu.Unlock()
	return signalPid(syscall.Kill, pid, sig)
}

func (k *kills) count() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.pids)
}

// started is argv started in ctx as a group's leader with no grace, as Run
// starts a program, with Bare() for its environment and the group's kills
// counted.
func started(t *testing.T, ctx context.Context, argv ...string) (*process, *group, *kills) {
	t.Helper()
	cmd := prepare(ctx, Cmd{Path: argv[0], Args: argv[1:]}, Bare())
	g, err := grouped(cmd, 0, drainWait, Limits{}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	k := &kills{cmd: cmd}
	g.signal = k.send
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd, g, k
}

// TestAGroupIsKilledAfterItsLeaderExitsAndBeforeItIsReaped: the group's wait
// kills the group once, after the leader has exited and before it is reaped,
// stopping it once before the kill to list it, and then reaps the leader with
// its own exit. A kill before the exit would
// end the leader with SIGKILL, and one after the reap could reach a group
// the pid now leads.
//
// What it costs when this fails: a member's exit code is lost to
// a SIGKILL the runner sent itself, so a passing member reads red, or a kill after the reap reaches a
// group another program now leads.
func TestAGroupIsKilledAfterItsLeaderExitsAndBeforeItIsReaped(t *testing.T) {
	t.Parallel()
	cmd, g, k := started(t, t.Context(), "/bin/sh", "-c", "exit 3")
	var killed, waited error
	within(t, "the group's wait", func() { killed, waited = g.wait() })
	if killed != nil && !errors.Is(killed, os.ErrProcessDone) {
		t.Errorf("the group's kill: %v, want it killed or found gone", killed)
	}
	if len(k.pids) != 1 || k.pids[0] != -cmd.Process.Pid {
		t.Errorf("the wait killed %v, want the group -%d once", k.pids, cmd.Process.Pid)
	}
	if len(k.stops) != 1 || k.stops[0] != 0 {
		t.Errorf("the wait stopped the group after %v kills, want once, before the kill: the freeze before the listing", k.stops)
	}
	noted := 0
	for i, reaped := range k.reaped {
		noted++
		if reaped {
			t.Errorf("kill %d came after the leader was reaped", i+1)
		}
	}
	if noted != len(k.pids) || noted == 0 {
		t.Errorf("the wait noted %d kills of %d as reaped or not, want each, and at least one", noted, len(k.pids))
	}
	// The reap's error is the exit itself, an *exec.ExitError, read by its method.
	var exit interface{ ExitCode() int }
	if !errors.As(waited, &exit) || exit.ExitCode() != 3 {
		t.Errorf("the leader's reap: %v, want its own exit 3, so no kill came before it exited and nothing reaped it before the wait", waited)
	}
}

// TestNoKillReachesAGroupOnceItIsStopped: the context's kill, which can come
// as Wait reaps the leader, signals nothing once the group is stopped. Before
// it, the same kill reaches the group: the control.
//
// What it costs when this fails: a cancel that comes as a member
// ends kills whatever group took the member's pid, a program of the person's that the runner never
// started.
func TestNoKillReachesAGroupOnceItIsStopped(t *testing.T) {
	t.Parallel()
	cmd, g, k := started(t, t.Context(), "/bin/sh", "-c", "exec /bin/sleep 300")
	if err := g.kill(); err != nil {
		t.Errorf("a kill of a running group: %v", err)
	}
	if n := k.count(); n != 1 && !killAgain || n < 2 && killAgain {
		t.Fatalf("a kill of a running group passed %d kills on, want 1, or one more until none answers where killAgain is %t", n, killAgain)
	}
	if err := g.stop(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("stop: %v", err)
	}
	within(t, "the reap after stop", func() { _ = cmd.Wait() })
	before := k.count()
	if err := g.kill(); !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("a kill after stop: %v, want os.ErrProcessDone", err)
	}
	if n := k.count(); n != before {
		t.Errorf("a kill after stop and the reap passed %d more kills on, want none", n-before)
	}
}

// TestALeaderThatExitedBeforeTheWaitBeganIsSeenExited: a second wait, which
// begins on a leader the first saw exit and nothing reaped, sees it exited
// too, and does not hang on an exit it was not there for. The leader is still
// there to reap after both.
//
// What it costs when this fails: a run whose member ended before
// the runner began to wait for it hangs until go test's deadline or the person's patience ends it.
func TestALeaderThatExitedBeforeTheWaitBeganIsSeenExited(t *testing.T) {
	t.Parallel()
	cmd, _, _ := started(t, t.Context(), "/bin/sh", "-c", "exit 0")
	for _, which := range []string{"the first wait", "the second wait"} {
		var err error
		within(t, which, func() { err = exited(cmd.Process.Pid) })
		if err != nil {
			t.Errorf("%s: %v", which, err)
		}
	}
	if err := cmd.Wait(); err != nil {
		t.Errorf("the leader reaped after both waits: %v, want it there to reap, with exit 0", err)
	}
}

// TestTheContextsEndKillsTheWholeGroup: the context's end kills the group
// through the group's own kill, which a stopped group refuses, and not
// through exec's default, which kills the leader alone and can come as Wait
// reaps it. The leader, which would sleep for five minutes, ends by SIGKILL,
// and the recorder sees the kills of the group before the reap: the
// context's, and the one the wait makes once the leader has exited; where
// killAgain is set, the context's kill is repeated until none answers, so
// there are at least three.
//
// What it costs when this fails: an interrupted run kills only each
// member's first process, and the compilers and tests it started keep the machine busy after
// the runner has said it stopped.
func TestTheContextsEndKillsTheWholeGroup(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cmd, g, k := started(t, ctx, "/bin/sh", "-c", "exec /bin/sleep 300")
	cancel()
	var waited error
	within(t, "the group's wait after the context ended", func() { _, waited = g.wait() })
	k.mu.Lock()
	pids, reaped := k.pids, k.reaped
	k.mu.Unlock()
	if len(pids) != 2 && !killAgain || len(pids) < 3 && killAgain {
		t.Errorf("the group saw the kills %v, want the context's and the wait's, the context's repeated where killAgain is %t", pids, killAgain)
	}
	aimed, noted := 0, 0
	for i, pid := range pids {
		aimed++
		if pid != -cmd.Process.Pid {
			t.Errorf("kill %d reached %d, want the group -%d", i+1, pid, cmd.Process.Pid)
		}
	}
	for i, r := range reaped {
		noted++
		if r {
			t.Errorf("kill %d came after the leader was reaped", i+1)
		}
	}
	if aimed != len(pids) || noted != len(pids) || aimed == 0 {
		t.Errorf("read %d kills' aims and %d reaps of %d kills, want each, and at least one", aimed, noted, len(pids))
	}
	// The reap's error is the exit itself, an *exec.ExitError, read by its method.
	var exit interface{ Sys() any }
	if !errors.As(waited, &exit) {
		t.Fatalf("the leader's reap: %v, want it ended by SIGKILL", waited)
	}
	if status, ok := exit.Sys().(syscall.WaitStatus); !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Errorf("the leader's reap: %v, want it ended by SIGKILL", waited)
	}
}

// TestARefusedWaitStandsWhenTheGroupIsFoundGone: a wait for the leader's exit
// that refuses is reported, though the kill that follows finds the group gone,
// as macOS answers a group whose members have exited and are not yet reaped.
// start drops a kill that found the group gone, so a refusal joined to one
// would be dropped with it and the member would pass with its own exit. The
// control: the wait's real exit, with the same kill, is no refusal.
//
// What it costs when this fails: a member whose wait went wrong passes
// on its own exit, a green that no run showed to be one.
func TestARefusedWaitStandsWhenTheGroupIsFoundGone(t *testing.T) {
	t.Parallel()
	refusal := errors.New("the test's wait refuses")
	for _, refuses := range []bool{true, false} {
		_, g, k := started(t, t.Context(), "/bin/sh", "-c", "exit 0")
		g.signal = func(pid int, sig syscall.Signal) error {
			_ = k.send(pid, sig)
			return syscall.EPERM
		}
		g.exited = func(pid int) error {
			if err := exited(pid); err != nil || !refuses {
				return err
			}
			return refusal
		}
		var killed, waited error
		within(t, "the group's wait", func() { killed, waited = g.wait() })
		if n := k.count(); n != 1 {
			t.Errorf("refuses %t: the wait passed %d kills on, want 1", refuses, n)
		}
		if refuses && (!errors.Is(killed, refusal) || gone(killed) == nil) {
			t.Errorf("the wait refused, and the group's kill is %v as start reads it, want the refusal", gone(killed))
		}
		if !refuses && gone(killed) != nil {
			t.Errorf("the wait saw the exit, and the group's kill is %v as start reads it, want none", gone(killed))
		}
		if waited != nil {
			t.Errorf("refuses %t: the leader's reap: %v, want its own exit 0", refuses, waited)
		}
	}
}

// held is a group's signal and wait for the leader's exit, as for a leader that changed its user
// when denies is set. The signal is noted, and answers EPERM where denies is set, or is sent, which
// reaches the group. The wait is the real one, held until the first signal is answered, in both
// cases, so the answer alone decides how the wait ends. later is closed once a second signal is
// answered: the stop of a late reap sends it, just before that reap's cmd.Wait.
func held(k *kills, denies bool) (signal func(int, syscall.Signal) error, wait func(int) error, later <-chan struct{}) {
	answered, second := make(chan struct{}), make(chan struct{})
	signal = func(pid int, sig syscall.Signal) error {
		var err error = syscall.EPERM
		if !denies {
			err = signalPid(syscall.Kill, pid, sig)
		}
		k.mu.Lock()
		k.pids = append(k.pids, pid)
		n := len(k.pids)
		k.mu.Unlock()
		switch n {
		case 1:
			close(answered)
		case 2:
			close(second)
		}
		return err
	}
	wait = func(pid int) error {
		<-answered
		return exited(pid)
	}
	return signal, wait, second
}

// TestAGroupNoSignalReachesEndsTheWaitWithWhyUnkillable: a leader that changed
// its user answers the group's signal with EPERM while it runs on. Once the
// context's end finds it so, the wait goes on for the group's bound and no
// longer, and gives errUnkillable with the leader not reaped, instead of
// waiting for the program to end, and the group reads Left true, as its leader
// runs on. The signal here answers EPERM, as for such a
// leader; the bound is cut to nothing, so the test reads no clock. The control
// is the same wait, held the same way, with a signal that reaches the leader:
// it ends with the leader's kill and its reap. The two rows differ only in the
// signal's answer.
//
// What it costs when this fails: Run of a program that becomes another user,
// as sudo does, waits past its Timeout for as long as the program runs, so a
// run never ends at the deadline it promises; or a caller that reads Left alone
// takes a run whose program the kill never reached for one that left nothing.
func TestAGroupNoSignalReachesEndsTheWaitWithWhyUnkillable(t *testing.T) {
	t.Parallel()
	for name, denies := range map[string]bool{"denied": true, "reached": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cmd, g, k := started(t, ctx, "/bin/sleep", "300")
			g.bound = 0
			g.signal, g.exited, _ = held(k, denies)
			cancel()
			var killed, waited error
			within(t, "the group's wait", func() { killed, waited = g.wait() })
			if !denies {
				if errors.Is(killed, errUnkillable) || cmd.ProcessState == nil {
					t.Errorf("a group the signal reaches: killed %v, reaped %t, want its kill and the reap", killed, cmd.ProcessState != nil)
				}
				return
			}
			if !errors.Is(killed, errUnkillable) || waited != nil {
				t.Errorf("a group no signal reaches: killed %v, waited %v, want %v and no reap", killed, waited, errUnkillable)
			}
			if !g.leftOver() {
				t.Error("a group no signal reaches: Left false, want true: its leader runs on")
			}
			// Read with g.mu held: the late reap's stop takes it before that reap.
			g.mu.Lock()
			reaped := cmd.ProcessState != nil
			g.mu.Unlock()
			if reaped {
				t.Error("a group no signal reaches: the wait reaped the leader, which runs on")
			}
			if n := k.count(); n != 1 {
				t.Errorf("a group no signal reaches: %d signals, want the cancel's one", n)
			}
			// The test's own kill lets the late reap through.
			if err := signalPid(syscall.Kill, cmd.Process.Pid, syscall.SIGKILL); err != nil {
				t.Error(err)
			}
		})
	}
}

// TestAnUnkillableRunsCodeIsMinusOneAndTheLateReapIsNotRead: the Code of a run
// whose group no signal reaches is -1, and comes without a read of the leader's
// cmd.ProcessState, which the late reap writes once the leader exits, at a
// time nothing orders against Run's own return. Here the leader exits once the
// wait has given errUnkillable, and the Code is taken as the late reap goes on.
// The control: a leader the wait reaped gives its own exit code.
//
// What it costs when this fails: a caller's -race run of a program that
// becomes another user, as sudo does, reports a data race inside Run, and the
// Code is a read of a state written as it is read.
func TestAnUnkillableRunsCodeIsMinusOneAndTheLateReapIsNotRead(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cmd, g, k := started(t, ctx, "/bin/sleep", "300")
	g.bound = 0
	var later <-chan struct{}
	g.signal, g.exited, later = held(k, true)
	cancel()
	var killed error
	within(t, "the group's wait", func() { killed, _ = g.wait() })
	if !errors.Is(killed, errUnkillable) {
		t.Fatalf("a group no signal reaches: killed %v, want %v", killed, errUnkillable)
	}
	if err := signalPid(syscall.Kill, cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	within(t, "the late reap's stop", func() { <-later })
	if code := exitCode(cmd, true); code != -1 {
		t.Errorf("an unkillable run's Code is %d, want -1", code)
	}
	if end, sig, status := endingOf(cmd, true, nil); end != EndNone || sig != nil || status != 0 {
		t.Errorf("an unkillable run's End is %q, Signal %v, Status %#x, want EndNone alone", end, sig, status)
	}

	own, reaps, _ := started(t, t.Context(), "/bin/sh", "-c", "exit 3")
	var waited error
	within(t, "the group's wait", func() { _, waited = reaps.wait() })
	if code := exitCode(own, false); code != 3 {
		t.Errorf("a reaped leader's Code is %d (reap %v), want its own exit 3", code, waited)
	}
}

// TestAGroupIsKilledAgainUntilNoLiveProcessAnswers is the kill-again loop, on
// a scripted signal: where killAgain is set, the group is killed again, a
// pause apart, until the system answers that no live process is left, and
// a group that answers killTries kills is an error; elsewhere the one kill
// stands. Either way the first kill's answer decides whether the group was
// killed or found gone, and a refusal ends the loop.
//
// What it costs when this fails: a process forked as the kill landed outlives its member, or a
// member killed mid-run reads as one whose group was already gone.
func TestAGroupIsKilledAgainUntilNoLiveProcessAnswers(t *testing.T) {
	t.Parallel()
	refused := syscall.EINVAL
	ran := 0
	for name, c := range map[string]struct {
		answers         []error // the answer to each kill; the last repeats
		again, once     int     // the kills where killAgain is set, and where not
		quiet, fallback error   // signalled's answer where killAgain is set, and where not
	}{
		"found gone":           {[]error{syscall.EPERM}, 1, 1, os.ErrProcessDone, os.ErrProcessDone},
		"killed, then EPERM":   {[]error{nil, nil, syscall.EPERM}, 3, 1, nil, nil},
		"killed, then ESRCH":   {[]error{nil, syscall.ESRCH}, 2, 1, nil, nil},
		"a refusal":            {[]error{refused}, 1, 1, refused, refused},
		"killed, then refused": {[]error{nil, refused}, 2, 1, refused, nil},
		"never quiet":          {[]error{nil}, killTries, 1, errors.New("after 5000 kills"), nil},
	} {
		ran++
		cmd, g, _ := started(t, t.Context(), "/bin/sh", "-c", "exec /bin/sleep 300")
		var kills, pauses int
		g.signal = func(pid int, _ syscall.Signal) error {
			kills++
			if pid != -cmd.Process.Pid {
				t.Errorf("%s: kill %d reached %d, want the group -%d", name, kills, pid, cmd.Process.Pid)
			}
			return c.answers[min(kills, len(c.answers))-1]
		}
		g.pause = func() { pauses++ }
		var err error
		within(t, name, func() { err = g.signalled(syscall.SIGKILL) })
		want, wantErr := c.once, c.fallback
		if killAgain {
			want, wantErr = c.again, c.quiet
		}
		if kills != want || pauses != want-1 {
			t.Errorf("%s: %d kills and %d pauses, want %d and %d (killAgain %t)", name, kills, pauses, want, want-1, killAgain)
		}
		switch {
		case wantErr == nil && err != nil, wantErr != nil && err == nil:
			t.Errorf("%s: %v, want %v", name, err, wantErr)
		case errors.Is(wantErr, os.ErrProcessDone) || errors.Is(wantErr, refused):
			if !errors.Is(err, wantErr) {
				t.Errorf("%s: %v, want %v", name, err, wantErr)
			}
		case wantErr != nil && !strings.Contains(err.Error(), wantErr.Error()):
			t.Errorf("%s: %v, want it to say %q", name, err, wantErr)
		}
		_ = signalPid(syscall.Kill, -cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	}
	if ran == 0 {
		t.Fatal("no case ran")
	}
}

// TestASignalToAPidBelow2IsRefusedAndSendsNothing: signalPid, signalGroup, a group's signalled and
// stopTree each refuse a pid of 0, 1 and -1, and one wider than a pid_t, whose low 32 bits the kernel
// would read as 1 or -1, with an error, and the killer that counts is never called. A pid of 2 or
// more by its size, and no wider than a pid_t, is passed on, once for each.
//
// What it costs when this fails: a kill(0) ends the caller's own process group, and a kill(-1) every
// process the caller may signal, as one did to every session on a Linux machine.
func TestASignalToAPidBelow2IsRefusedAndSendsNothing(t *testing.T) {
	t.Parallel()
	sent := 0
	count := func(int, syscall.Signal) error {
		sent++
		return nil
	}
	low := int64(1) << 32
	for _, pid := range []int{0, 1, -1, int(low + 1), int(low - 1), -int(low + 1), math.MinInt32} {
		refused := func(how string, err error) {
			t.Helper()
			if !errors.Is(err, errSignalPid) {
				t.Errorf("%s of pid %d: %v, want %v", how, pid, err, errSignalPid)
			}
		}
		refused("signalPid", signalPid(count, pid, syscall.SIGKILL))
		refused("signalPid of the group", signalPid(count, -pid, syscall.SIGKILL))
		refused("signalGroup", signalGroup(count, func() {}, pid, syscall.SIGKILL, killAgain, nil))
		g := &group{cmd: &process{Process: &os.Process{Pid: pid}}, signal: count, pause: func() {}}
		refused("a group's signalled", g.signalled(syscall.SIGKILL))
		if err := stopTree(TreeID(strconv.Itoa(pid)+".1"), killer{signal: count, pause: func() {}}); err == nil {
			t.Errorf("stopTree of pid %d: nil, want it refused", pid)
		}
	}
	// A group below 0 would be signalled as the process its size names, by -pgid.
	for _, pgid := range []int{-2, -5} {
		if err := signalGroup(count, func() {}, pgid, syscall.SIGKILL, killAgain, nil); !errors.Is(err, errSignalPid) {
			t.Errorf("signalGroup of group %d: %v, want %v", pgid, err, errSignalPid)
		}
		g := &group{cmd: &process{Process: &os.Process{Pid: pgid}}, signal: count, pause: func() {}}
		if err := g.signalled(syscall.SIGKILL); !errors.Is(err, errSignalPid) {
			t.Errorf("a group's signalled of group %d: %v, want %v", pgid, err, errSignalPid)
		}
	}
	if sent != 0 {
		t.Fatalf("the killer was called %d times for pids below 2 or wider than a pid_t, want none", sent)
	}
	for _, pid := range []int{2, -2, math.MaxInt32, -math.MaxInt32} {
		if err := signalPid(count, pid, 0); err != nil {
			t.Errorf("signalPid of pid %d: %v, want it passed on", pid, err)
		}
	}
	if sent != 4 {
		t.Errorf("the killer was called %d times for 4 pids it may signal, want 4", sent)
	}
}

// TestAKillAgainEndsOnceTheGuardNoLongerHolds: signalGroup told to kill again asks still before each
// kill after the first and sends no kill once it says the guard no longer holds; a group that
// answers every kill is killed once more for each time the guard still holds. The signal here counts
// and sends nothing, so the group is a number alone.
//
// What it costs when this fails: a stop that is not the leader's parent kills the group again after
// the leader was reaped and its pid taken, and the later kill reaches another process's group.
func TestAKillAgainEndsOnceTheGuardNoLongerHolds(t *testing.T) {
	t.Parallel()
	const pgid = 4242
	ran := 0
	for _, r := range []struct{ holds, kills int }{{0, 1}, {1, 2}, {3, 4}} {
		ran++
		kills, asks := 0, 0
		count := func(int, syscall.Signal) error { kills++; return nil }
		still := func() bool { asks++; return asks <= r.holds }
		if err := signalGroup(count, func() {}, pgid, syscall.SIGKILL, true, still); !errors.Is(err, errGuardGone) {
			t.Errorf("a guard that holds %d times: %v, want errGuardGone, for the caller to go on by member", r.holds, err)
		}
		if kills != r.kills || asks != r.holds+1 {
			t.Errorf("a guard that holds %d times: %d kills and %d asks, want %d and %d", r.holds, kills, asks, r.kills, r.holds+1)
		}
	}
	if ran != 3 {
		t.Fatalf("ran %d rows, want 3", ran)
	}
}

// TestExitedNowAnswersAtOnceAndReapsNothing: exitedNow is false for a leader still running, true
// once it has exited, and leaves it unreaped, so its one reap still gives its own exit.
//
// What it costs when this fails: a group's wait past a bound of zero blocks on a leader that runs
// on, or calls one that runs on exited; or the ask reaps the leader, its pid free for another's
// group before the kill.
func TestExitedNowAnswersAtOnceAndReapsNothing(t *testing.T) {
	t.Parallel()
	cmd, _, _ := started(t, t.Context(), "/bin/sleep", "300")
	pid := cmd.Process.Pid
	var now bool
	var err error
	within(t, "the ask of a running leader", func() { now, err = exitedNow(pid) })
	if now || err != nil {
		t.Errorf("a running leader: exitedNow %t, %v, want false", now, err)
	}
	if err := signalPid(syscall.Kill, pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := exited(pid); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if now, err := exitedNow(pid); !now || err != nil {
			t.Errorf("a leader that has exited: exitedNow %t, %v, want true", now, err)
		}
	}
	_ = cmd.Wait()
	if cmd.ProcessState == nil || cmd.ProcessState.String() != "signal: terminated" {
		t.Errorf("the leader's one reap: %v, want signal: terminated", cmd.ProcessState)
	}
}

// TestALeaderThatExitedPastTheBoundEndsTheWaitWithItsOwnersAnswer: one goroutine owns the wait for
// the leader's exit. Here the leader has exited, the cancel's kill is answered EPERM, as macOS
// answers a group of exits, and the bound is zero; the owner's wait answers only once the ask past
// the bound has, and its answer is the test's own. The group's kill is that answer, not
// errUnkillable, and the leader is reaped after it, with its code, 3.
//
// What it costs when this fails: the leader is reaped before its owner has asked, and the owner's
// wait then asks of a pid another process may hold: on Linux a waitid on another child of this
// process, a Cmd's of its own, which it waits on until that one exits.
func TestALeaderThatExitedPastTheBoundEndsTheWaitWithItsOwnersAnswer(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cmd, g, k := started(t, ctx, "/bin/sh", "-c", "exit 3")
	g.bound = 0
	if err := exited(cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	g.signal = func(pid int, sig syscall.Signal) error {
		_ = k.send(pid, sig)
		return syscall.EPERM
	}
	asked, owners := make(chan struct{}), errors.New("the owner's answer")
	g.exitedNow = func(pid int) (bool, error) {
		defer close(asked)
		return exitedNow(pid)
	}
	g.exited = func(int) error {
		<-asked
		return owners
	}
	cancel()
	var killed error
	within(t, "the group's wait", func() { killed, _ = g.wait() })
	if !errors.Is(killed, owners) || errors.Is(killed, errUnkillable) {
		t.Errorf("a leader that exited, asked past a bound of zero: killed %v, want the owner's answer", killed)
	}
	if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 3 {
		t.Errorf("a leader that exited, asked past a bound of zero: state %v, want it reaped with code 3", cmd.ProcessState)
	}
}
