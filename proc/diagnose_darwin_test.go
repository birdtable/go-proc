package proc

import (
	"fmt"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix" //nolint:depguard // arch.ports.23: a failed row lists its group's members with their state, parent and flags by the kern.proc.pgrp sysctl, whose struct is in x/sys alone
)

// killLog records each signal a stop sends, so a row that fails can say what Stop saw.
// Its killer is ender's: syscall.Kill and the same pause, so a stop by it is Stop.
type killLog struct {
	start time.Time
	lines []string
}

// killer is ender's killer, with each signal and its answer written to the log, timed from the
// first.
func (l *killLog) killer() killer {
	k := ender()
	signal := k.signal
	k.signal = func(pid int, sig syscall.Signal) error {
		if l.start.IsZero() {
			l.start = time.Now()
		}
		err := signalPid(signal, pid, sig) // the pid passed signalPid once already; it goes by it again, as every signal here does
		l.lines = append(l.lines, fmt.Sprintf("  +%v kill(%d, %d): %v", time.Since(l.start), pid, int(sig), err))
		return err
	}
	return k
}

// String is the log, a line a signal.
func (l *killLog) String() string { return strings.Join(l.lines, "\n") }

// groupState lists the group pgid's members as the kern.proc.pgrp sysctl gives them, each with its
// pid, parent, state (p_stat: 1 SIDL, 2 SRUN, 3 SSLEEP, 4 SSTOP, 5 SZOMB), flags, command and start
// time, a line a member: what a failed row shows of the processes Stop left.
func groupState(pgid int) string {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pgid)
	if err != nil {
		return "  the listing failed: " + err.Error()
	}
	if len(procs) == 0 {
		return "  no member"
	}
	lines := make([]string, 0, len(procs))
	for _, p := range procs {
		start := p.Proc.P_starttime
		lines = append(lines, fmt.Sprintf("  pid %d ppid %d stat %d flag %#x comm %q born %d.%06d",
			p.Proc.P_pid, p.Eproc.Ppid, p.Proc.P_stat, p.Proc.P_flag, unix.ByteSliceToString(p.Proc.P_comm[:]), start.Sec, start.Usec))
	}
	return strings.Join(lines, "\n")
}
