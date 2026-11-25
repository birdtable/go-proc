package proc

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows" //nolint:depguard // arch.ports.23: the tests start a helper with no console, or one no window shows, by the flags Windows takes
)

// breakGrace is the Grace of a run whose program ignores the break: the run returns once it is gone.
const breakGrace = 2 * time.Second

// breakSlack is how long past its Grace a run whose program ignores the break may take to return,
// and how far within its Grace one whose program catches it returns: a loaded host's start and
// kill, not a clock any row waits out.
const breakSlack = 20 * time.Second

// underHelper starts the helper mode with args with the creation flags given, as a test starts one,
// waits for it, and gives what it wrote, by each line's first word.
func underHelper(t *testing.T, flags uint32, mode string, args ...string) map[string]string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	c := &process{Path: exe, Args: append([]string{exe, helperMode + mode}, args...), Stdout: &out, Stderr: &errs, SysProcAttr: &syscall.SysProcAttr{CreationFlags: flags}}
	if err := startTestProcess(c); err != nil {
		t.Fatal(err)
	}
	within(t, "the "+mode+" helper", func() { err = c.Wait() })
	if err != nil {
		t.Fatalf("the %s helper: %v, wrote %q and %q", mode, err, out.String(), errs.String())
	}
	said := map[string]string{}
	for line := range strings.Lines(out.String()) {
		word, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
		said[word] = rest
	}
	return said
}

// TestAGraceSendsARunWithTerminalABreakThenKillsTheJob: a helper with a console of its own that no
// window shows runs a program under Run with Terminal and a Grace, and ends the run's context once
// the program has said up. A program that catches the break says so and exits, and the run returns
// long before its Grace is gone, ended WhyCancelled with no other error; one that catches the break
// and does not exit is killed with the job once its Grace is gone, and not before. Both are sent the
// break: each wrote broken.
//
// What it costs when this fails: a program given a grace on Windows to save its work is killed
// with no word, or one that ignores the break outlives its Grace, or the run waits out a Grace its
// program ended within.
func TestAGraceSendsARunWithTerminalABreakThenKillsTheJob(t *testing.T) {
	t.Parallel()
	for _, r := range []struct {
		how   string
		grace time.Duration
	}{
		{"catch", time.Hour},
		{"ignore", breakGrace},
	} {
		t.Run(r.how, func(t *testing.T) {
			t.Parallel()
			said := underHelper(t, windows.CREATE_NO_WINDOW, "brk-run", r.how, r.grace.String())
			if said["out"] != fmt.Sprintf("%q", "up\nbroken\n") || said["end"] != "cancelled" {
				t.Fatalf("a run whose program does %s the break: out %s, end %s, want up and broken, and cancelled", r.how, said["out"], said["end"])
			}
			var ms int64
			if _, err := fmt.Sscan(said["took"], &ms); err != nil {
				t.Fatalf("took %q: %v", said["took"], err)
			}
			took := time.Duration(ms) * time.Millisecond
			switch {
			case r.how == "catch" && took > breakSlack:
				t.Errorf("a run whose program caught the break and exited took %v to return, want well within its Grace of %v", took, r.grace)
			case r.how == "ignore" && (took < r.grace || took > r.grace+breakSlack):
				t.Errorf("a run whose program ignored the break took %v to return, want its Grace of %v and at most %v more", took, r.grace, breakSlack)
			}
		})
	}
}

// TestAGraceIsRefusedWithoutTerminalOrAConsole: a run with a Grace and no Terminal is refused with
// errNoGrace, ErrNotHeld, ErrRefused and errors.ErrUnsupported, and Code -1, with nothing started,
// as Holds says first, whether or not this process has a console; and a helper with no console at all
// (DETACHED_PROCESS) is refused the same for a run with Terminal.
//
// What it costs when this fails: a run whose program has a console of its own, which no break from
// here reaches, or a run from a process with no console to send one from, is told it has a Grace and
// is killed at its end with none.
func TestAGraceIsRefusedWithoutTerminalOrAConsole(t *testing.T) {
	t.Parallel()
	mark := filepath.Join(t.TempDir(), "made")
	c := helperCmd(t, "touch", mark)
	c.Grace = time.Second
	_, held := Holds(c)
	res, err := Run(t.Context(), c)
	for name, err := range map[string]error{"Run": err, "Holds": held} {
		if !errors.Is(err, errNoGrace) || !errors.Is(err, ErrNotHeld) || !errors.Is(err, ErrRefused) || !errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("a run with a Grace and no Terminal: %s gave %v, want errNoGrace, ErrNotHeld, ErrRefused and errors.ErrUnsupported", name, err)
		}
	}
	if _, statErr := os.Stat(mark); res.Code != -1 || !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("a run with a Grace and no Terminal: Code %d, file %v, want Code -1 and nothing started", res.Code, statErr)
	}
	said := underHelper(t, windows.DETACHED_PROCESS, "brk-refused", t.TempDir())
	if said["console"] != "none" {
		t.Fatalf("the helper started with no console has one: console %s", said["console"])
	}
	if said["run"] != "refused" {
		t.Errorf("a run with Terminal and a Grace from a process with no console: %s, want refused, with nothing started", said["run"])
	}
}

// TestABreakToAProgramWithAConsoleOfItsOwn says, in the log, whether a CTRL_BREAK_EVENT sent from a
// process with a console reaches a program started with a console of its own that no window shows,
// as a run without Terminal is: a Grace is widened to such runs only given this answer. It fails only when the probe does not run.
func TestABreakToAProgramWithAConsoleOfItsOwn(t *testing.T) {
	t.Parallel()
	said := underHelper(t, windows.CREATE_NO_WINDOW, "brk-nowindow")
	for word := range said {
		t.Logf("a break to a CREATE_NO_WINDOW program: %s", strings.TrimSpace(word+" "+said[word]))
	}
	if len(said) != 1 {
		t.Errorf("the probe wrote %v, want one line", said)
	}
}
