//go:build windows

package proc

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows" //nolint:depguard // arch.ports.23: the tests find, wait on and end the processes a killed caller left, by the calls Windows makes and reads them with
)

// heldProcess opens process pid to wait on it and end it: the handle names that process, and no
// later one with its pid, so a test that ends what is left ends nothing else. The test ends the
// process, if it still runs, and closes the handle at its own end.
func heldProcess(t *testing.T, pid int) windows.Handle {
	t.Helper()
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		t.Fatalf("opening process %d: %v", pid, err)
	}
	t.Cleanup(func() { endHeld(h) })
	return h
}

// endHeld ends the process h names, if it still runs, and closes h. A process that has ended
// refuses the end, which is what was wanted.
func endHeld(h windows.Handle) {
	_ = windows.TerminateProcess(h, 1) // one that has ended refuses it
	_ = windows.CloseHandle(h)         // a handle the test opened, closed once
}

// endsIn says whether the process h names ends within d.
func endsIn(t *testing.T, h windows.Handle, d time.Duration) bool {
	t.Helper()
	event, err := windows.WaitForSingleObject(h, uint32(d.Milliseconds()))
	switch {
	case err != nil:
		t.Fatalf("waiting for a process: %v", err)
	case event == windows.WAIT_OBJECT_0:
		return true
	case event != uint32(windows.WAIT_TIMEOUT):
		t.Fatalf("the wait for a process gave %#x", event)
	}
	return false
}

// TestACallerKilledBeforeItsTreeIsWholeLeavesNoLeader: a caller of Start killed at any step after
// its leader is in the guard job, the leader suspended in it, given its handle of the tree's job, or
// resumed in that job, takes the leader with it: the leader has ended within endsWithin. A caller
// killed once the guard is lifted leaves the tree running, as a detached tree outlives its caller:
// the leader writes that it outlived the caller. A caller killed in the guard after it starts a
// process by CreateProcess with every inheritable handle of its own takes the leader with it as well,
// and that process still runs: the guard's handle is the caller's alone, so no process the caller
// starts keeps the guard open.
//
// What it costs when this fails: a caller that dies as it starts a tree, killed by a test's timeout
// or by its user, leaves the leader suspended with no parent, in a job that does not end it when its
// handles close: a process no id names, which holds what it was given until the machine restarts;
// or, when the guard is not lifted, no detached tree outlives the process that started it; or a
// process any code of the caller's starts as the tree starts holds the guard, and the leader with it.
func TestACallerKilledBeforeItsTreeIsWholeLeavesNoLeader(t *testing.T) {
	t.Parallel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		stage      startStage
		outlives   bool
		inheritAll bool // the caller first starts a process with every inheritable handle of its own
	}{
		{stageGuarded, false, false},
		{stageGiven, false, false},
		{stageResumed, false, false},
		{stageFreed, true, false},
		{stageGuarded, false, true},
	} {
		name := string(c.stage)
		if c.inheritAll {
			name += " with every handle inherited"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// pipe, not os.Pipe, whose ends are inheritable here: a process another test starts
			// would take the write end, and the read would never end.
			r, w, err := pipe()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = r.Close() })
			args := []string{helperMode + "detach-at", string(c.stage), strconv.Itoa(os.Getpid())}
			said := 1
			if c.inheritAll {
				args, said = append(args, "inherit-all"), 2
			}
			caller := exec.Command(exe, args...) //nolint:gosec,noctx // the test binary itself, as a helper, which the test kills
			caller.Stdout, caller.Stderr = w, w
			err = startTestProcess(caller)
			_ = w.Close()
			if err != nil {
				t.Fatal(err)
			}
			br := bufio.NewReader(r)
			got := lines(t, br, said)
			pid, err := strconv.Atoi(got["held"])
			if err != nil {
				t.Fatalf("the caller's leader pid: %v", err)
			}
			leader := heldProcess(t, pid)
			var holder windows.Handle
			if c.inheritAll {
				h, err := strconv.Atoi(got["inheriting"])
				if err != nil {
					t.Fatalf("the pid of the process that inherits the caller's handles: %v", err)
				}
				holder = heldProcess(t, h)
			}
			if err := endTestProcess(caller); err != nil {
				t.Fatalf("killing the caller: %v", err)
			}
			_ = caller.Wait() // reaped; its exit is the kill's
			if c.outlives {
				if got := lines(t, br, 1)["outlived"]; got != strconv.Itoa(pid) {
					t.Fatalf("the leader wrote that %q outlived its caller, want its own pid %d", got, pid)
				}
				return
			}
			if !endsIn(t, leader, endsWithin) {
				t.Fatalf("the leader %d still runs %v after its caller was killed at %s, want it ended with the guard job", pid, endsWithin, name)
			}
			if c.inheritAll && endsIn(t, holder, 0) {
				t.Fatal("the process that inherits the caller's handles has ended, so the row shows nothing of what it holds")
			}
		})
	}
}

// raceRuns is how many callers TestACallerKilledAsItStartsLeavesNoProcessOnceItIsInItsJob kills for
// each of Start and Run, and raceStep how much later than the one before each kill comes after the
// caller says it is about to start: the kills land from 0 to raceRuns-1 steps after the go.
const (
	raceRuns = 100
	raceStep = 100 * time.Microsecond
)

// raceOutcome is what one killed caller left: whether a process it made was left running,
// suspended, with no job to end it, and the time from the caller's go to its leader in the guard,
// where the caller wrote it (zero where not).
type raceOutcome struct {
	orphan  bool
	guarded time.Duration
}

// TestACallerKilledAsItStartsLeavesNoProcessOnceItIsInItsJob: a caller of Start, or of Run, killed
// as close after the start as the test can, at a delay that grows over raceRuns runs, leaves no
// process once the process it started is in its guard job (Start) or its job (Run): a leader its
// caller wrote was guarded, and not yet resumed, has ended within endsWithin. One resumed may
// outlive the kill, as its guard may be lifted, and is not judged. A process a kill left in the
// window before, from CreateProcess inside exec's Start to the assignment (startHeld), is counted,
// ended and reported, with the time from the go to the guard, as proc's Windows docs say: the test
// fails on none of those.
//
// What it costs when this fails: a caller killed as it starts leaves a process suspended with no
// parent and no job to end it, from the guard on, which startHeld says it does not.
func TestACallerKilledAsItStartsLeavesNoProcessOnceItIsInItsJob(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"start", "run"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			var orphaned, guarded []time.Duration
			for i := range raceRuns {
				delay := time.Duration(i) * raceStep
				o := raceOnce(t, kind, delay)
				if o.orphan {
					orphaned = append(orphaned, delay)
				}
				if o.guarded > 0 {
					guarded = append(guarded, o.guarded)
				}
			}
			t.Logf("%s: %d callers killed 0 to %v after the go; %d left a process suspended with no job, killed at %s; go to guard %s",
				kind, raceRuns, (raceRuns-1)*raceStep, len(orphaned), spread(orphaned), spread(guarded))
		})
	}
}

// spread says the least, the median and the most of ds, and how many there are.
func spread(ds []time.Duration) string {
	if len(ds) == 0 {
		return "not written"
	}
	slices.Sort(ds)
	return fmt.Sprintf("least %v, median %v, most %v, of %d", ds[0], ds[len(ds)/2], ds[len(ds)-1], len(ds))
}

// raceOnce starts a race helper of kind, kills it delay after it says go, and says what the kill
// left. A leader the helper wrote was guarded, and not resumed, that still runs endsWithin after the
// kill fails the test; one it wrote was resumed is not judged. Any other process the helper made that
// still runs orphanGrace after the kill is an orphan. Each is ended at the test's end.
func raceOnce(t *testing.T, kind string, delay time.Duration) raceOutcome {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r, w, err := pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	caller := exec.Command(exe, helperMode+"race", kind, strconv.Itoa(os.Getpid())) //nolint:gosec,noctx // the test binary itself, as a helper, which the test kills
	caller.Stdout, caller.Stderr = w, w
	err = startTestProcess(caller)
	_ = w.Close()
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(r)
	if line, err := br.ReadString('\n'); err != nil || line != "go\n" {
		t.Fatalf("the race helper wrote %q (%v), want go", line, err)
	}
	for begun := time.Now(); time.Since(begun) < delay; {
		runtime.Gosched() // a wait finer than a timer's, which lands each kill raceStep later than the one before
	}
	if err := endTestProcess(caller); err != nil {
		t.Fatalf("killing the caller: %v", err)
	}
	born, ended := lifeOf(t, caller)
	_ = caller.Wait() // reaped; its exit is the kill's
	rest, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("reading what the race helper wrote: %v", err)
	}
	var o raceOutcome
	guardedPid, resumed := 0, false
	for line := range strings.Lines(string(rest)) {
		word, values, _ := strings.Cut(strings.TrimSuffix(line, "\n"), " ")
		switch {
		case word == "guarded" && guardedPid == 0 && !resumed:
			pid, took, _ := strings.Cut(values, " ")
			guardedPid, _ = strconv.Atoi(pid)
			o.guarded, _ = time.ParseDuration(took)
		case word == "resumed" && guardedPid != 0 && !resumed:
			resumed = true
		default:
			t.Fatalf("the race helper wrote %q after go, want a guarded line, then a resumed line, or less", rest)
		}
	}
	for pid, h := range childrenOf(t, caller.Process.Pid, born, ended, filepath.Base(exe)) {
		switch {
		case pid == guardedPid && resumed:
			// may outlive the kill once the guard is lifted, as a detached tree does; ended at the test's end
		case pid == guardedPid:
			if !endsIn(t, h, endsWithin) {
				t.Errorf("%s: the process %d its caller had put in its guard still runs %v after the caller was killed, want it ended", kind, pid, endsWithin)
			}
		case !endsIn(t, h, orphanGrace):
			o.orphan = true
		}
	}
	return o
}

// orphanGrace is how long after its caller's end a process the caller made, and wrote no guard for,
// has to end before raceOnce counts it as left suspended with no job. One in its job is ended by the
// job as the caller's handles close, before the caller's own end, so it ends within milliseconds; one
// left in the window never ends, and a wait of endsWithin for each would cost the run minutes.
const orphanGrace = 2 * time.Second

// lifeOf waits for the killed process c to end and gives when it was made and when it ended.
func lifeOf(t *testing.T, c *process) (born, ended windows.Filetime) {
	t.Helper()
	var creation, exit, kernel, user windows.Filetime
	var timed error
	err := c.Process.WithHandle(func(h uintptr) {
		if _, timed = windows.WaitForSingleObject(windows.Handle(h), windows.INFINITE); timed == nil {
			timed = windows.GetProcessTimes(windows.Handle(h), &creation, &exit, &kernel, &user)
		}
	})
	if err = errors.Join(err, timed); err != nil {
		t.Fatalf("reading when the caller ended: %v", err)
	}
	return creation, exit
}

// childrenOf gives a handle of each process whose parent pid is parent, made from born, parent's
// making, to ended, its end, and whose program file is named image, by its pid: the processes parent
// made, and not those of an earlier or a later process that had its pid, as an earlier caller whose
// orphan still waits for the test's end may have. Each is ended, if it still runs, and closed at the
// test's end.
func childrenOf(t *testing.T, parent int, born, ended windows.Filetime, image string) map[int]windows.Handle {
	t.Helper()
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		t.Fatalf("listing the processes: %v", err)
	}
	defer func() { _ = windows.CloseHandle(snap) }()
	children := map[int]windows.Handle{}
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		if int(pe.ParentProcessID) != parent || !strings.EqualFold(windows.UTF16ToString(pe.ExeFile[:]), image) {
			continue
		}
		h, opened := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pe.ProcessID)
		if opened != nil {
			continue // gone since the list was taken
		}
		var creation, exit, kernel, user windows.Filetime
		if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil || creation.Nanoseconds() < born.Nanoseconds() || creation.Nanoseconds() > ended.Nanoseconds() {
			_ = windows.CloseHandle(h) // not parent's: made before it or after it ended, or not read
			continue
		}
		t.Cleanup(func() { endHeld(h) })
		children[int(pe.ProcessID)] = h
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		t.Fatalf("listing the processes: %v", err)
	}
	return children
}
