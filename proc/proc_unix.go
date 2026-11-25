//go:build unix

package proc

import (
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"sync"
	"syscall"
	"time"
)

// errDenied is a group's first answer of EPERM: its processes have exited
// unreaped, as macOS answers, which is a group gone, or one of them changed its
// user and no signal from here reaches it. It wraps os.ErrProcessDone.
var errDenied = fmt.Errorf("the process group answered EPERM: %w, or a process of it changed its user", os.ErrProcessDone)

// errSignalPid is a signal to a pid that is not one process or one process group the package may
// have started: below 2 by its size, where 0 is the caller's own group, 1 is init and -1 is every
// process the caller may signal, or wider than a pid_t, of which the kernel reads only the low 32
// bits, any of those among them. Nothing is sent.
var errSignalPid = errors.New("proc: a signal to a pid below 2 by its size, or wider than a pid_t, is refused, since 0 names the caller's own process group, 1 init and -1 every process it may signal; nothing was sent")

// signalPid sends sig by signal to pid, a process when above 0 and the process group -pid when below,
// once pidRefused passes it, and otherwise sends nothing. Every signal the package sends on Unix goes
// by it, and every one its tests send, which TestEverySignalOnUnixGoesBySignalPid holds.
func signalPid(signal func(pid int, sig syscall.Signal) error, pid int, sig syscall.Signal) error {
	if err := pidRefused(pid); err != nil {
		return err
	}
	return signal(pid, sig)
}

// pidRefused is why pid names no single process or process group a signal may go to, or nil.
func pidRefused(pid int) error {
	if pid > -2 && pid < 2 || pid > math.MaxInt32 || pid < -math.MaxInt32 {
		return fmt.Errorf("%w: pid %d", errSignalPid, pid)
	}
	return nil
}

// killTries is how many times a group is killed, where killAgain says so,
// before a live process still in it is an error, and killPause the pause
// between two kills: at least five seconds for the killed to exit.
const (
	killTries = 5000
	killPause = time.Millisecond
)

// A group is a member's process group: its leader, the process run starts, and every process the
// leader starts. The group is killed only while its leader is not yet reaped. Until then the
// leader's pid, and so the group's, is the member's own. Once the leader is reaped, the pid is
// free, may lead another group, and a kill of the old group would reach that one instead.
type group struct {
	cmd *process
	// tree, when not nil, is the run's cgroup on Linux, which says whether the tree passed its limit.
	tree *runCgroup
	// signal is syscall.Kill; a test of the package counts the kills.
	signal func(pid int, sig syscall.Signal) error
	// exited is the wait for the leader's exit that leaves it unreaped; a
	// test of the package gives a group one that refuses.
	exited func(pid int) error
	// exitedNow asks, without waiting, whether the leader has exited, leaving it unreaped: end
	// asks it once bound is past, as exited's answer may not have come by then, and takes that
	// answer when it says so. A test of the package gives a group one that notes the asking.
	exitedNow func(pid int) (bool, error)
	// bound is how long the wait for the leader's exit goes on once a kill finds the group out of
	// its reach: Grace and the run's Drain. A test of the package gives a group a shorter one.
	bound time.Duration
	// gap, in a test of the package, runs between the listing of the group and the reading of each
	// process it lists, on Linux (groupLeft), and between the listing before the stop and the stop
	// on macOS (frozenLeft).
	gap func()
	// unreached is closed once the first signal of a cancel or a kill is answered EPERM: every
	// process of the group has exited unreaped (macOS), or one changed its user, and no signal
	// from here reaches it. told is whether it is closed.
	unreached chan struct{}
	told      bool
	// pause is the wait between two kills of the group, a sleep of killPause;
	// a test of the package gives a group one that counts.
	pause func()
	// grace is how long the group has between the SIGTERM of a cancel and its
	// SIGKILL; zero sends no SIGTERM.
	grace time.Duration
	mu    sync.Mutex
	// ended is set once the group is stopped, before the leader is reaped:
	// a kill after it signals nothing.
	ended bool
	// cut is set once a cancel signals the group.
	cut bool
	// late is the SIGKILL a cancel with a grace leaves for later, fired
	// closed once it has run, and lateErr how it went, read only after fired.
	late    *time.Timer
	fired   chan struct{}
	lateErr error
	// left is whether the group held a process other than its leader once the leader had exited,
	// read by frozenLeft before the group is stopped; true when the wait for the exit failed, and
	// when the group was found out of a signal's reach past bound.
	left bool
	// seen is every process a listing before one of the group's signals held, the leader aside.
	seen []int
}

// leaderPid is the pid of the group's leader, and whether there is one: the process run started,
// once it has.
func (g *group) leaderPid() (int, bool) {
	if g.cmd.Process == nil {
		return 0, false
	}
	return g.cmd.Process.Pid, true
}

// grouped starts cmd as the leader of a process group of its own, and makes the context's end end
// the whole group, not the leader alone: SIGTERM, and SIGKILL grace later, or at once with no
// grace. A group holds no limit: Run refused one this system does not hold, and Linux's TreeMemory
// is the run's cgroup's (inCgroup). drain is the run's Drain, which with grace bounds the wait for
// a leader no signal reaches. With noTerminal, a run without Terminal, cmd leads a session of its
// own, with no controlling terminal, and so a group of its own too, whose id is its pid as with
// Setpgid: setsid makes both. Setpgid is not set with it, as setsid fails in a group's leader.
func grouped(cmd *process, grace, drain time.Duration, _ Limits, _ func(), noTerminal bool) (*group, error) {
	g := &group{
		cmd: cmd, signal: syscall.Kill, exited: exited, exitedNow: exitedNow, pause: func() { time.Sleep(killPause) }, grace: grace,
		bound: grace + drain, unreached: make(chan struct{}),
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: !noTerminal, Setsid: noTerminal}
	cmd.Cancel = g.cancel
	return g, nil
}

func (g *group) start() error { return g.cmd.Start() }

// cancel is the context's end: unless the group is stopped, it sends the group
// SIGKILL, or with a grace SIGTERM, and SIGKILL once the grace is gone. It can
// come at any time, even as Wait reaps the leader.
func (g *group) cancel() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ended {
		return os.ErrProcessDone
	}
	g.cut = true
	if g.grace == 0 || g.late != nil {
		return g.reached(g.signalled(syscall.SIGKILL))
	}
	err := g.reached(g.signalled(syscall.SIGTERM))
	if err != nil {
		return err
	}
	fired := make(chan struct{})
	g.fired = fired
	g.late = time.AfterFunc(g.grace, func() {
		defer close(fired)
		g.lateErr = g.kill()
	})
	return nil
}

// inCgroup starts the group's leader into t, the run's cgroup, which then says whether the tree passed
// its limit. No t, as every system's but Linux's, places it nowhere.
func (g *group) inCgroup(t *runCgroup) error {
	g.tree = t
	return t.place(g.cmd)
}

// limited is whether the tree passed a limit: whether the run's cgroup's own memory.max ended the
// tree, by the kernel's kill or the run's, read once the cgroup's tree had ended. A group with no
// cgroup is told nothing.
func (g *group) limited() bool { return g.tree.passedLimit() }

func (g *group) wasCut() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cut
}

// kill kills every process of the group, unless the group is stopped. It is the context's kill,
// which can come at any time, even as Wait reaps the leader.
func (g *group) kill() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ended {
		return os.ErrProcessDone
	}
	return g.reached(g.signalled(syscall.SIGKILL))
}

// stop kills every process of the group and ends it, so no kill after it
// reaches the pid. The caller reaps the leader only after stop.
func (g *group) stop() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	err := g.signalled(syscall.SIGKILL)
	g.ended = true
	return err
}

// wait ends the group, then reaps the leader: the one reap of it, and so after every kill that
// reaches its pid. A SIGKILL a cancel left for later is stopped, or waited for when it has begun.
// killed is how the group's kills went, the late SIGKILL's with them, and waited is cmd.Wait's. A
// group the end gave up on is not reaped here: killed is errUnkillable, and the leader is reaped
// when it exits by itself.
func (g *group) wait() (killed, waited error) {
	killed = g.end()
	if errors.Is(killed, errUnkillable) {
		return killed, nil
	}
	g.mu.Lock()
	late, fired := g.late, g.fired
	g.mu.Unlock()
	if late != nil && !late.Stop() {
		<-fired
		killed = errors.Join(killed, gone(g.lateErr))
	}
	return killed, g.cmd.Wait()
}

// end waits until the leader has exited, leaves it unreaped, and stops the group. A wait that fails
// stops the group at once, the leader with it, and its refusal stands even when the group is found
// gone. Once a kill finds the group out of its reach, the wait goes on for bound at most: a group
// of exits not yet reaped answers at once. Past bound the leader is asked once more, without
// waiting, as bound may be zero and the wait's answer not yet come: for a leader that has exited,
// the wait's answer is taken, as it comes at once, and EPERM alone is never errUnkillable. One
// goroutine waits on the leader, and nothing reaps it before that wait has answered, so no wait
// asks of a pid another process may hold by then. One still running is left to run, to be reaped
// when it exits, and end is errUnkillable.
func (g *group) end() error {
	if g.cmd.Process == nil {
		return g.stop()
	}
	done := make(chan error, 1)
	go func() { done <- g.exited(g.cmd.Process.Pid) }()
	select {
	case err := <-done:
		return g.exitedThen(err)
	case <-g.unreached:
	}
	bound := time.NewTimer(g.bound)
	defer bound.Stop()
	select {
	case err := <-done:
		return g.exitedThen(err)
	case <-bound.C:
		now, err := g.exitedNow(g.cmd.Process.Pid)
		if now && err == nil {
			// done's goroutine owns the wait: its answer for a leader that has exited comes at once,
			// and the leader is reaped only after it, while its pid is still the leader's.
			return g.exitedThen(<-done)
		}
		// A group no signal reaches holds its leader at least, which runs on: a process left.
		g.left = true
		go g.reapLater(done)
		return errors.Join(errUnkillable, err)
	}
}

// exitedThen stops the group once the wait for the leader's exit is done, err
// being how it went. First it reads whether the group holds a process other
// than the leader, left unreaped, whose pid is the group's: frozenLeft.
func (g *group) exitedThen(err error) error {
	if err != nil {
		g.left = true
		return errors.Join(err, gone(g.stop()))
	}
	g.left = g.frozenLeft()
	return g.stop()
}

// frozenLeft stops every process of the group by SIGSTOP, then lists the group, the leader aside
// (groupLeft). A listing is not one instant: on Linux /proc is listed, then each process's stat is
// read, and a process that forks and exits in between is gone at its stat, its child not listed, so
// a chain of such processes would never be seen. A stopped process neither forks nor exits until
// the kill that follows, and a fork the stop lands on is stopped with it, so the listing sees the
// group as it was frozen. The leader has exited and is not stopped. A group that answers the stop
// as gone (ESRCH), or as one no signal reaches (errDenied, as macOS answers a group of exits not
// yet reaped), is listed as it is; a stop that fails otherwise is a process left. A group with no
// leader, one never started, holds nothing.
//
// Where listsBeforeStop, on macOS, the group is listed before the stop too, and a process either
// listing holds is left: it was there once the leader had exited. A process whose exec is finishing
// as its parent exits is, for a moment after the stop lands, in no listing of the group or of every
// process, though its pid answers a kill; the listing before the stop holds it. gap,
// in a test of the package, runs between that listing and the stop.
func (g *group) frozenLeft() bool {
	pid, ok := g.leaderPid()
	before := ok && listsBeforeStop && groupLeft(pid, pid, nil)
	if listsBeforeStop && g.gap != nil {
		g.gap()
	}
	if err := g.freeze(); err != nil {
		return true
	}
	return ok && (before || groupLeft(pid, pid, g.gap))
}

// freeze sends the group SIGSTOP once, unless it is stopped: a stop that finds it gone, or answered
// EPERM, is no error. It does not close unreached: a cancel's or a kill's answer does that.
func (g *group) freeze() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ended {
		return nil
	}
	err := g.signalled(syscall.SIGSTOP)
	if errors.Is(err, os.ErrProcessDone) || errors.Is(err, errDenied) {
		return nil
	}
	return err
}

func (g *group) leftOver() bool { return g.left }

// reapLater reaps the leader of a group end gave up on, once done says it
// exited: the group is stopped first, while the pid is still the leader's.
// Run has returned by then, and nothing reads how it went.
func (g *group) reapLater(done <-chan error) {
	_ = errors.Join(<-done, gone(g.stop()), g.cmd.Wait()) //nolint:errcheck // Run returned errUnkillable already, and has no caller left to hand this to
}

// reached is err, the answer of a cancel's or a kill's signal, with g.mu held.
// An answer of errDenied closes unreached, once.
func (g *group) reached(err error) error {
	if errors.Is(err, errDenied) && !g.told {
		g.told = true
		close(g.unreached)
	}
	return err
}

// signalled sends the group sig, with g.mu held. A group with no process left is done, not an
// error: macOS answers a group of exited members not yet reaped with EPERM. That first EPERM is
// errDenied, which is done too, since a process that changed its user answers so as well: end tells
// the two apart by the leader's exit. Where killAgain says so, a SIGKILL is sent again, a pause
// apart, until no live process answers, which reaches a process forked as an earlier kill landed; a
// group still answering after killTries kills is an error. A SIGTERM is sent once. Otherwise the
// first kill's answer is the answer, so a group the first kill found live was killed, not gone.
func (g *group) signalled(sig syscall.Signal) error {
	pid, ok := g.leaderPid()
	if !ok {
		return nil
	}
	// Run is the leader's parent, and the leader is reaped only after the group is stopped, so its
	// pid stays the leader's through every kill, and no kill needs the guard asked again. On macOS a
	// process mid-exec is for a moment in no listing of the group and no kill of it reaches it: while a process a listing before one of the signals held is live in the group
	// once its exec is through, the group (never the pid) is killed again, and past killTries kills
	// the end says so, whatever the group answered.
	g.seen = seenIn(g.seen, pid)
	err := signalGroup(g.signal, g.pause, pid, sig, killAgain, nil)
	for pass := 0; sig == syscall.SIGKILL && gone(err) == nil && liveIn(g.signal, pid, g.seen); pass++ {
		if pass == killTries {
			return fmt.Errorf("the process group of member process %d still held a live process past an exec after %d kills", pid, killTries)
		}
		err = signalGroup(g.signal, g.pause, pid, sig, killAgain, nil)
	}
	return err
}

// seenIn is seen with each process group pgid lists, its leader aside, added once.
func seenIn(seen []int, pgid int) []int {
	members, _ := groupMembers(pgid) //nolint:errcheck // a listing that fails adds nothing
	for _, m := range members {
		if m.pid != pgid && !slices.Contains(seen, m.pid) {
			seen = append(seen, m.pid)
		}
	}
	return seen
}

// errGuardGone is signalGroup's answer once still says the guard no longer holds after the first
// kill: the group was killed once, and may still have a member, which the caller kills by member.
var errGuardGone = errors.New("the guard no longer holds after the first kill")

// signalGroup sends the process group pgid sig by signal, as signalled says, pause being the wait
// between two kills and again whether a SIGKILL is sent again, killAgain outside a test. still,
// when not nil, is asked before each kill after the first: a caller that is not the leader's parent
// cannot keep the leader from being reaped and its pid taken between two kills, so once still says
// the guard no longer holds, no further kill is sent by the group, and the answer is errGuardGone,
// for the caller to go on by member. A pgid below 2 names no group the package started: it is
// refused before any signal, so nothing is sent, and below 0 it would name a process, not a group.
func signalGroup(signal func(pid int, sig syscall.Signal) error, pause func(), pgid int, sig syscall.Signal, again bool, still func() bool) error {
	if pgid < 2 {
		return fmt.Errorf("%w: process group %d", errSignalPid, pgid)
	}
	for try := 1; ; try++ {
		if try > 1 && still != nil && !still() {
			return errGuardGone
		}
		err := signalPid(signal, -pgid, sig)
		switch {
		case errors.Is(err, syscall.EPERM) && try == 1:
			return errDenied
		case errors.Is(err, syscall.ESRCH) && try == 1:
			return os.ErrProcessDone
		case errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM):
			return nil
		case err != nil || !again || sig != syscall.SIGKILL:
			return err
		case try == killTries:
			return fmt.Errorf("the process group of member process %d still had a live process after %d kills", pgid, killTries)
		}
		pause()
	}
}

// bareNames are the names Bare gives a child here: PATH, LANG and HOME.
func bareNames() []string { return []string{"PATH", "LANG", "HOME"} }

// systemNames are the names a child here needs to start, or to find the system's own files, beyond
// PATH, LANG and the home names: none.
func systemNames() []string { return nil }

// envKey is an environment name as this system compares it: as written.
func envKey(name string) string { return name }

// programsOnly is whether Run starts only a program file, by its name: only on Windows.
const programsOnly = false

// graceHeld is whether a run's tree is sent a signal it can catch before the kill, so a Grace
// above zero is held, not refused (errNoGrace): on Unix, every run's, with Terminal or without.
func graceHeld(bool) bool { return true }

// brokenPipe is whether err is a write to a pipe no process reads any more, as a program that
// exited before it read all its stdin leaves it.
func brokenPipe(err error) bool { return errors.Is(err, syscall.EPIPE) }
