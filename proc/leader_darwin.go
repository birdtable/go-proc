//go:build darwin

package proc

import (
	"errors"
	"slices"
	"syscall"

	"golang.org/x/sys/unix" //nolint:depguard // arch.ports.23: a process's start time and group are read by the kern.proc.pid sysctl, whose struct is in x/sys alone
)

// treeIDHere is how this system names a tree: by its process group and its leader's start time.
const treeIDHere = idGroup

// listsBeforeStop is true: frozenLeft lists the group before the stop as well as after it. A process
// whose exec is finishing as its parent exits is, for a moment after the stop lands, in no listing, and was in the one before.
const listsBeforeStop = true

// leader reads the process group and start time of the process pid by the kern.proc.pid sysctl,
// the start time in microseconds. A pid no process has is errNoProcess: the sysctl gives no entry.
func leader(_ procRoot, pid int) (pgid int, born uint64, err error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil {
		return 0, 0, err
	}
	if len(procs) == 0 || int(procs[0].Proc.P_pid) != pid {
		return 0, 0, errNoProcess
	}
	return int(procs[0].Eproc.Pgid), startedAt(&procs[0]), nil
}

// startedAt is p's start time in microseconds from 1970.
func startedAt(p *unix.KinfoProc) uint64 {
	start := p.Proc.P_starttime
	return uint64(start.Sec)*1_000_000 + uint64(start.Usec) //nolint:gosec // G115: a start time is after 1970, so neither part is negative
}

// place is "": a tree id here needs no place after its start time, which counts microseconds from
// 1970 and so differs from one boot to the next by itself.
func place(procRoot) (string, error) { return "", nil }

// groupMembers lists the processes of group pgid by the kern.proc.pgrp sysctl, each with its pid and
// its start time as leader reads it.
func groupMembers(pgid int) ([]member, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pgid)
	if err != nil {
		return nil, err
	}
	members := make([]member, 0, len(procs))
	for i := range procs {
		members = append(members, member{pid: int(procs[i].Proc.P_pid), born: startedAt(&procs[i])})
	}
	return members, nil
}

// groupLeft is whether group pgid holds a process other than except, as groupMembers lists the
// group: any, in any state, a zombie or one exiting among them. A listing that fails is true. The
// listing is one call, which the kernel makes from one list of pids taken under its lock, but it is
// not every process of the group: one whose exec is finishing as its parent exits is left out for a
// moment, though its pid answers a kill, so frozenLeft lists the group before the stop
// too. gap, a test's stand-in for a slow listing on Linux, has no place in it here and is not run.
func groupLeft(pgid, except int, _ func()) bool {
	members, err := groupMembers(pgid)
	return err != nil || slices.ContainsFunc(members, func(m member) bool { return m.pid != except })
}

// liveIn is whether a process of seen is live in group pgid once its exec is through: a kill of 0,
// which sends nothing, waits out an exec, and the kern.proc.pid sysctl then lists it in the group
// neither a zombie (5) nor exiting (P_WEXIT), or not at all though it answers. A kill
// answered EPERM is a process that exists, one whose uid changed, as a command under sudo, so it is
// asked after as one that answered, never read as gone.
func liveIn(signal func(pid int, sig syscall.Signal) error, pgid int, seen []int) bool {
	return slices.ContainsFunc(seen, func(pid int) bool {
		if err := signalPid(signal, pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
			return false
		}
		k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
		return err != nil || int(k.Eproc.Pgid) == pgid && k.Proc.P_stat != 5 && k.Proc.P_flag&0x2000 == 0
	})
}

// freeze stops the group pgid before Stop's kill, with k's signal and pause, as a run's
// end stops its group before its kill (frozenLeft), and gives seen with the processes each of its
// listings held added, the leader aside: the union liveIn asks after the kill. A process mid-exec as
// its parent exits is, for a moment, in no listing and no signal of its group, and
// a kill landing as a process of the group forks makes that moment; a stopped parent is not exiting,
// so a child mid-exec as the stop lands comes out of it a member a later listing holds. The first
// SIGSTOP is answered as signalGroup answers a first kill: EPERM is errDenied, ESRCH os.ErrProcessDone.
// Then, a pause apart, each pass lists the group into seen and asks the leader and each of seen by its
// pid: a signal 0, which waits out an exec, then its state (frozen). The freeze ends at a pass whose
// listing added no one and which found each frozen; else the guard is asked and the group stopped
// again. A later stop answered EPERM or ESRCH, or killTries passes, leave the group to the kill as it
// is, never to a nil. still is asked last, so a guard gone by then is errGuardGone, for the caller to
// go on by member.
func freeze(k killer, pgid int, still func() bool, seen []int) ([]int, error) {
	if err := signalGroup(k.signal, k.pause, pgid, syscall.SIGSTOP, false, nil); err != nil {
		return seen, err
	}
	for range killTries {
		k.pause()
		had := len(seen)
		seen = seenIn(seen, pgid)
		if len(seen) == had && frozen(k.signal, pgid, pgid) && !slices.ContainsFunc(seen, func(pid int) bool { return !frozen(k.signal, pgid, pid) }) {
			break
		}
		if !still() {
			return seen, errGuardGone
		}
		if err := signalPid(k.signal, -pgid, syscall.SIGSTOP); errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM) {
			break
		} else if err != nil {
			return seen, err
		}
	}
	if !still() {
		return seen, errGuardGone
	}
	return seen, nil
}

// frozen is whether pid, asked by a signal 0 that waits out an exec first, as liveIn asks, can fork
// nothing more in group pgid: it is gone, out of the group, stopped (4), a zombie (5) or exiting
// (P_WEXIT). One that answers the signal but whose state cannot be read is not frozen, and one that
// answers EPERM, a process that exists, is asked after as one that answered, as liveIn asks.
func frozen(signal func(pid int, sig syscall.Signal) error, pgid, pid int) bool {
	if err := signalPid(signal, pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return true
	}
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	return err == nil && (int(k.Eproc.Pgid) != pgid || k.Proc.P_stat == 4 || k.Proc.P_stat == 5 || k.Proc.P_flag&0x2000 != 0)
}
