package proc

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

// TestAProcessListedBeforeTheStopIsLeft: a worker that is in the program's group when the program
// exits, and is gone by the listing after the stop, reads Left true. The worker is the test's own
// child, put in the group by setpgid, and the row kills and reaps it in gap, between the listing
// before the stop and the stop, so the frozen listing holds only the program, unreaped. The row
// stands in for this case: on macOS a process whose exec is finishing as its parent exits is, for
// a moment after the stop lands, in no listing of the group (kern.proc.pgrp, proc_listpids, a kill
// of the group) though its pid answers a kill, and the frozen listing alone reads Left false. The
// moment comes about once in 2,000 runs of a program that starts a worker and exits, under load,
// too seldom for a row to wait on it; this row takes the frozen listing's miss as given and reads
// what the listing before the stop adds.
//
// What it costs when this fails: a caller that fails closed on a program that leaves work running
// is told nothing was left, once in some thousands of runs, while a worker runs on.
func TestAProcessListedBeforeTheStopIsLeft(t *testing.T) {
	t.Parallel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := prepare(t.Context(), Cmd{Path: "/bin/sh", Args: []string{"-c", "read x || :"}}, Bare())
	g, err := grouped(cmd, 0, drainWait, Limits{}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	cmd.Stdin = r
	if err := cmd.Start(); err != nil {
		_ = r.Close()
		t.Fatal(err)
	}
	_ = r.Close()
	worker := &process{Path: exe, Args: []string{exe, helperMode + "hold"}, SysProcAttr: &syscall.SysProcAttr{Setpgid: true, Pgid: cmd.Process.Pid}}
	if err := startTestProcess(worker); err != nil {
		_ = signalPid(syscall.Kill, -cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
		t.Fatal(err)
	}
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			_ = signalPid(syscall.Kill, worker.Process.Pid, syscall.SIGKILL)
			_ = worker.Wait()
		}
	})
	gapRan := 0
	g.gap = func() {
		gapRan++
		_ = signalPid(syscall.Kill, worker.Process.Pid, syscall.SIGKILL)
		_ = worker.Wait()
		reaped = true
	}
	_ = w.Close()
	var killed, waited error
	within(t, "the group's wait", func() { killed, waited = g.wait() })
	if killed != nil && !errors.Is(killed, os.ErrProcessDone) {
		t.Errorf("the group's kill: %v, want it killed or found gone", killed)
	}
	if waited != nil || !cmd.ProcessState.Success() {
		t.Errorf("a program that reads its input to the end: %v, %v, want its exit 0", waited, cmd.ProcessState)
	}
	if gapRan != 1 || !reaped {
		t.Fatalf("gap ran %d times and reaped the worker %v, want once and reaped", gapRan, reaped)
	}
	if !g.leftOver() {
		t.Errorf("a worker in the group at the program's exit, gone by the frozen listing: Left false, want true")
	}
}
