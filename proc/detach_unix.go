//go:build unix

package proc

import (
	"errors"
	"fmt"
	"os"
	"os/exec" //nolint:depguard // proc is the runner: the leader Start starts is an exec.Cmd
	"strconv"
	"strings"
	"syscall"
	"time"
)

// errNoProcess is a leader read of a pid no process has.
var errNoProcess = errors.New("no process has the pid")

// detachFromTerminal puts the leader in a session of its own, and so a process group of its own.
//
// Without it the child stays in the terminal's process group, and closing the terminal sends
// SIGHUP to the whole group. The parent has already exited by then, so nothing catches it: the job
// the caller had just promised would keep going dies with the window. Setsid is what "or close this
// window" costs. The new session has no controlling terminal until its leader opens one.
func detachFromTerminal(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// detach starts cmd as the leader of a session of its own, and names its tree by the leader's pid,
// which is its group's, its start time, as r gives it, and here, where the id is good, which start
// read before anything started. A leader whose start time cannot be read, or that leads no group of its own,
// is killed, with the group its pid names, and reaped: the leader by itself too, since a leader in
// another group is not reached by its pid's group.
func detach(cmd *process, r procRoot, here string) (Tree, error) {
	detachFromTerminal(cmd)
	if err := cmd.Start(); err != nil {
		return Tree{}, err
	}
	pid := cmd.Process.Pid
	pgid, born, err := leader(r, pid)
	id, err := named(pid, pgid, born, here, err)
	if err != nil {
		// This process is the leader's parent and has not reaped it, so its pid stays the leader's.
		return Tree{}, errors.Join(err, gone(signalGroup(syscall.Kill, func() {}, pid, syscall.SIGKILL, killAgain, nil)), killedAlone(pid), cmd.Wait())
	}
	return Tree{ID: id, Leader: cmd.Process}, nil
}

// killedAlone kills the started leader pid by itself, not yet reaped, so the pid is still its own: a
// leader in another group is not reached by its pid's group. One that exited already may answer
// ESRCH, which is no error here.
func killedAlone(pid int) error {
	if err := signalPid(syscall.Kill, pid, syscall.SIGKILL); !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// named is the id of the tree whose leader is pid, as leader read it: its group pgid and start time
// born, and b, place's answer where the system needs one in the id, or err. A leader whose start
// time was not read, or that leads no group of its own, names no tree.
func named(pid, pgid int, born uint64, b string, err error) (TreeID, error) {
	if err == nil && pgid != pid {
		err = fmt.Errorf("proc: the started leader %d is in process group %d, not one of its own", pid, pgid)
	}
	if err != nil {
		return "", fmt.Errorf("proc: naming the started tree: %w", err)
	}
	id := strconv.Itoa(pid) + "." + strconv.FormatUint(born, 10)
	if b != "" {
		id += "." + b
	}
	return TreeID(id), nil
}

// killer is how Stop kills a group: signal is syscall.Kill, and pause the wait between two kills;
// a test of the package gives one that counts.
type killer struct {
	signal func(pid int, sig syscall.Signal) error
	pause  func()
}

// ender is the killer Stop uses.
func ender() killer {
	return killer{signal: syscall.Kill, pause: func() { time.Sleep(killPause) }}
}

// stopTree kills the group id names by k, once the guard holds: the process with the group's pid
// leads that group and started when the id says, so it is the leader Start started and is not
// reaped, and while it is not reaped no other process can take its pid. Between the guard and the
// kill the leader could be reaped and its pid taken again, which takes the system's whole run of
// pids in that time. A group the first kill finds gone, its leader reaped since the guard, is
// ErrStale; one that answers the first kill with EPERM is errStopDenied, never a tree done: the
// tree may run on, out of this process's reach. Where the id carries a place, its boot, pid
// namespace and time namespace, an id of another boot or of another namespace is ErrStale before
// the leader is read: a pid and a start time in ticks name a process within one boot, one pid
// namespace and one time namespace alone. Stop is not the leader's parent, so where a group is
// killed again (killAgain), the guard is asked again before each kill after the first; once it no
// longer holds, the group is killed by member (killMembers), and the answer is never a nil unseen.
// On macOS a process the kill missed mid-exec is asked after as the run's end asks:
// while one a listing before the kill held is live in the group past its exec, the group is
// killed again, as the guard allows, and past killTries kills the answer is errTreeRunsOn.
//
// On macOS the group is first stopped, the guard asked as before each kill:
// a process forked as the kill lands, mid-exec as its parent dies, was missed by that kill and by
// every listing liveIn asks, and Stop could answer nil while it ran on. The cause was found by
// reading, not reproduced. The first SIGSTOP is answered as the first kill was: EPERM is
// errStopDenied, ESRCH ErrStale, and nothing more is sent. The first SIGKILL after it, the group
// stopped, is answered so too: EPERM is errStopDenied, the tree perhaps left stopped and out of
// reach; ESRCH is ErrStale, the group gone. Stop sends no SIGCONT: a tree is left stopped if Stop's
// own process dies between its stop and its kill, and a second Stop of the id, whose guard still
// holds, kills it. The hole left: a process forked after the freeze's last listing before a stop,
// mid-exec as that stop lands, and in no listing until the kill has gone; how often that happens
// was not measured.
// It is stop through /proc, procOwn's checks first.
func stopTree(id TreeID, k killer) error { return stop(id, procFS{dir: procMount}, k) }

// stopAt is stopTree with r, the mount stop checked and holds, and here, where place says an id is
// good, read from it.
func stopAt(id TreeID, r procRoot, here string, k killer) error {
	pid, still, err := guarded(id, r, here)
	if err != nil {
		return err
	}
	seen, err := freeze(k, pid, still, seenIn(nil, pid))
	if err == nil {
		err = signalGroup(k.signal, k.pause, pid, syscall.SIGKILL, killAgain, still)
	}
	if errors.Is(err, errGuardGone) {
		err = killMembers(r, pid, k)
	}
	switch {
	case errors.Is(err, errDenied):
		return fmt.Errorf("proc: stopping tree %s: %w", id, errStopDenied)
	case errors.Is(err, os.ErrProcessDone):
		return errStale
	case err != nil:
		return fmt.Errorf("proc: stopping tree %s: %w", id, err)
	}
	for pass := 0; liveIn(k.signal, pid, seen); pass++ {
		err = errGuardGone
		if pass == killTries {
			err = errTreeRunsOn
		} else if still() {
			err = signalGroup(k.signal, k.pause, pid, syscall.SIGKILL, killAgain, still)
		}
		if errors.Is(err, errGuardGone) {
			err = killMembers(r, pid, k)
		}
		if gone(err) != nil {
			return fmt.Errorf("proc: stopping tree %s: %w", id, err)
		}
	}
	return nil
}

// guarded is the guard Stop and Interrupt signal under, one code for both: the pid of the group id
// names, once id has the form Start gives here (errTreeID otherwise), is of this place (placeStale)
// and names a process that is there, started when the id says and leads its own group (ErrStale
// otherwise), asked in that order. Nothing is signalled here. still asks the last again, for a
// caller that signals more than once.
func guarded(id TreeID, r procRoot, here string) (pid int, still func() bool, err error) {
	pid, born, at, ok := parseTreeID(id)
	if !ok {
		return 0, nil, errTreeID
	}
	if err := placeStale(at, here); err != nil {
		return 0, nil, err
	}
	pgid, now, err := leader(r, pid)
	switch {
	case errors.Is(err, errNoProcess):
		return 0, nil, errStale
	case err != nil:
		return 0, nil, fmt.Errorf("proc: reading the leader of tree %s: %w", id, err)
	case now != born || pgid != pid:
		return 0, nil, errStale
	}
	return pid, func() bool {
		pgid, now, err := leader(r, pid)
		return err == nil && now == born && pgid == pid
	}, nil
}

// interruptAt sends the group id names one SIGINT by k, once guarded holds, and nothing else: no
// SIGSTOP before it as Stop sends on macOS, no second signal and no pass by member, which Stop
// sends to leave no process of the tree alive, and Interrupt kills none. The window between the
// guard and the signal is stopTree's: the leader reaped and its pid taken again in that time. A
// group the signal finds gone is ErrStale, and one that answers EPERM is errStopDenied, as Stop's
// first kill is answered.
func interruptAt(id TreeID, r procRoot, here string, k killer) error {
	pid, _, err := guarded(id, r, here)
	if err != nil {
		return err
	}
	err = signalGroup(k.signal, k.pause, pid, syscall.SIGINT, false, nil)
	switch {
	case errors.Is(err, errDenied):
		return fmt.Errorf("proc: interrupting tree %s: %w", id, errStopDenied)
	case errors.Is(err, os.ErrProcessDone):
		return errStale
	case err != nil:
		return fmt.Errorf("proc: interrupting tree %s: %w", id, err)
	}
	return nil
}

// errStopDenied is a tree's group that answered Stop's first kill, or Interrupt's signal, with
// EPERM: a process of it may run as another user, or, on macOS, its processes have exited
// unreaped. Neither can tell which, so each says what the kill answered. It wraps syscall.EPERM, not
// os.ErrProcessDone, so a live tree out of this process's reach never reads as one that ended.
var errStopDenied = fmt.Errorf("the process group answered the kill with %w, so a process of it may run on out of this process's reach", syscall.EPERM)

// parseTreeID reads an id Start gives on a Unix: the leader's pid, more than 1 and at most the
// largest pid a kill takes, a dot, and its start time, each in decimal as Start writes it, so no
// sign, no leading zero and no space; then, where the system puts one there, a dot and its place:
// the boot's id, a UUID as the kernel writes it, a dot, the pid namespace's number, above 0, a dot,
// and the time namespace's number, 0 for a kernel with none. A pid of 0, 1 or below would name this
// process's own group, init's, or every process a kill reaches. Whether this system's ids carry a
// place is placeStale's to check; at is the place, or "".
func parseTreeID(id TreeID) (pid int, born uint64, at string, ok bool) {
	parts := strings.Split(string(id), ".")
	switch {
	case len(parts) == 5 && bootForm(parts[2]) && decimal(parts[3]) && parts[3] != "0" && decimal(parts[4]):
		at = strings.Join(parts[2:], ".")
	case len(parts) != 2:
		return 0, 0, "", false
	}
	p, t := parts[0], parts[1]
	n, errP := strconv.ParseUint(p, 10, 31) // at most math.MaxInt32
	born, errB := strconv.ParseUint(t, 10, 64)
	if errP != nil || errB != nil || n < 2 || strconv.FormatUint(n, 10) != p || strconv.FormatUint(born, 10) != t {
		return 0, 0, "", false
	}
	return int(n), born, at, true
}

// decimal is whether s is a count in decimal as the kernel writes one: digits alone, at most 19 of
// them, with no leading zero but in 0 itself.
func decimal(s string) bool {
	n, err := strconv.ParseUint(s, 10, 64)
	return err == nil && strconv.FormatUint(n, 10) == s
}

// placeStale is why an id whose place is at is not good here, where place answered here, or nil:
// an id of the other form, a place where this system gives none or none where it does, is
// errTreeID; one of another boot, another pid namespace or another time namespace is ErrStale.
// Each is compared on its own.
func placeStale(at, here string) error {
	if (at == "") != (here == "") {
		return errTreeID
	}
	if at == "" {
		return nil
	}
	id, ours := strings.Split(at, "."), strings.Split(here, ".")
	switch {
	case len(id) != 3 || len(ours) != 3:
		return errTreeID
	case id[0] != ours[0] || id[1] != ours[1] || id[2] != ours[2]: // the boot, pid or time namespace
		return errStale
	}
	return nil
}

// errTreeRunsOn is a tree whose group still had a live member after killMembers's last pass.
var errTreeRunsOn = errors.New("a process of the tree still answered after the last pass by member, so it may run on")

// killMembers kills the group pgid by member, once the guard no longer holds after the first kill:
// the leader was reaped, so its pid may no longer be the group's to kill by. A group that still has
// a member keeps its number, so a member is the tree's. Each pass asks kill(-pgid, 0), which sends
// nothing: ESRCH, or EPERM, the unreaped exits macOS answers for, is the group gone, nil. Otherwise
// the pass lists the group's members (groupMembers) and kills each by its pid once it is read again
// in the group with the start time it was listed with, the guard's own read and kill; a process that
// joins after the list is the next pass's. After killTries passes a member still answering is
// errTreeRunsOn, never nil.
func killMembers(r procRoot, pgid int, k killer) error {
	for range killTries {
		err := signalPid(k.signal, -pgid, 0)
		switch {
		case errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM):
			return nil
		case err != nil:
			return err
		}
		members, err := groupMembers(pgid)
		if err != nil {
			return fmt.Errorf("%w: listing the group: %w", errTreeRunsOn, err)
		}
		for _, m := range members {
			if g, born, err := leader(r, m.pid); err != nil || g != pgid || born != m.born {
				continue
			}
			if err := signalPid(k.signal, m.pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				return err
			}
		}
		k.pause()
	}
	return errTreeRunsOn
}

// member is a process of a group, as groupMembers lists it: its pid and its start time.
type member struct {
	pid  int
	born uint64
}

// bootForm is whether s is a boot's id as Linux writes /proc/sys/kernel/random/boot_id: a UUID in
// lower-case hex, 36 characters with a dash at 8, 13, 18 and 23.
func bootForm(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
	}
	return true
}
