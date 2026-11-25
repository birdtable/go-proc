package proc

import (
	"errors"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestAProcessMidExecAtTheKillIsKilledOnceItsExecIsThrough: a worker in the program's group that
// the group's kill misses, as it misses a process mid-exec, is killed once a kill of 0 to it has
// waited out the exec, and a worker no kill of the group reaches is an error of the kill, not its
// success. On macOS a process whose exec is finishing as its parent exits is, for a moment, in no
// listing of its group, and a SIGKILL of the group that lands on it before the exec switch is
// dropped with the old process; the kills after answer EPERM, only the unreaped leader being left,
// and the group's end took that as the tree gone while the worker ran on: 3 times in 120,000 tries
// under load, with the worker ignoring SIGHUP. The rows plant that moment in the
// group's signal: while the worker is hidden, the group's first SIGKILL is answered as reached and
// sends nothing, and the rest EPERM. In the first row the worker's own kill of 0, which in the
// kernel waits out the exec, ends the hiding; in the second nothing does.
//
// What it costs when this fails: a program that starts a worker and exits as the worker execs
// leaves it running past Run's end, and Run reports the tree killed.
func TestAProcessMidExecAtTheKillIsKilledOnceItsExecIsThrough(t *testing.T) {
	t.Parallel()
	for _, hidden := range []bool{false, true} {
		t.Run(map[bool]string{false: "its exec ends", true: "it stays hidden"}[hidden], func(t *testing.T) {
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
			ended := make(chan *os.ProcessState, 1)
			go func() {
				state, _ := worker.Process.Wait()
				ended <- state
			}()
			t.Cleanup(func() {
				_ = signalPid(syscall.Kill, worker.Process.Pid, syscall.SIGKILL)
				<-ended
			})
			var mu sync.Mutex
			midExec, groupKills := true, 0
			g.signal = func(pid int, sig syscall.Signal) error {
				mu.Lock()
				defer mu.Unlock()
				switch {
				case midExec && !hidden && pid == worker.Process.Pid && sig == 0:
					midExec = false
				case midExec && pid == -cmd.Process.Pid && sig == syscall.SIGKILL:
					groupKills++
					if groupKills == 1 {
						return nil
					}
					return syscall.EPERM
				}
				return signalPid(syscall.Kill, pid, sig)
			}
			_ = w.Close()
			var killed, waited error
			within(t, "the group's wait", func() { killed, waited = g.wait() })
			if waited != nil || !cmd.ProcessState.Success() {
				t.Errorf("a program that reads its input to the end: %v, %v, want its exit 0", waited, cmd.ProcessState)
			}
			if hidden {
				if killed == nil || errors.Is(killed, os.ErrProcessDone) || !strings.Contains(killed.Error(), "still held a live process") {
					t.Errorf("the group's kill, its worker live past every kill: %v, want the error that the group still held a live process", killed)
				}
				return
			}
			if killed != nil && !errors.Is(killed, os.ErrProcessDone) {
				t.Errorf("the group's kill: %v, want it killed or found gone", killed)
			}
			select {
			case state := <-ended:
				ended <- state
				if status, ok := state.Sys().(syscall.WaitStatus); !ok || status.Signal() != syscall.SIGKILL {
					t.Errorf("the worker ended %v, want by the group's SIGKILL", state)
				}
			case <-time.After(10 * time.Second):
				t.Errorf("a worker the group's kill missed mid-exec runs on 10s past the group's end, want it killed once its exec is through")
			}
		})
	}
}
