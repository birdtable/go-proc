package proc

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// underConsole starts a con-run helper with a console of its own that no window shows, never the
// console of whoever ran go test, and gives what it wrote, by each line's first word, and its pid.
func underConsole(t *testing.T, noTerminal bool) (said map[string]string, pid int) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	with := "0"
	if noTerminal {
		with = "1"
	}
	var out, errs bytes.Buffer
	c := &process{Path: exe, Args: []string{exe, helperMode + "con-run", with}, Stdout: &out, Stderr: &errs}
	quiet(c)
	if err := startTestProcess(c); err != nil {
		t.Fatal(err)
	}
	within(t, "the con-run helper", func() { err = c.Wait() })
	if err != nil {
		t.Fatalf("the con-run helper: %v, wrote %q and %q", err, out.String(), errs.String())
	}
	said = map[string]string{}
	for line := range strings.Lines(out.String()) {
		word, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
		if _, twice := said[word]; twice {
			t.Fatalf("the con-run helper wrote %q, with two %s lines", out.String(), word)
		}
		said[word] = rest
	}
	for _, word := range []string{"caller", "conin", "self", "console", "window", "grandchild", "gconsole", "gwindow", "end"} {
		if _, ok := said[word]; !ok {
			t.Fatalf("the con-run helper wrote %q, with no %s line", out.String(), word)
		}
	}
	if said["caller"] != strconv.Itoa(c.Process.Pid) {
		t.Fatalf("the con-run helper %d wrote caller %s", c.Process.Pid, said["caller"])
	}
	return said, c.Process.Pid
}

// TestARunWithoutTerminalReadsNoConsoleOfItsCallersAndStaysInItsJob: the test starts a helper with a
// console of its own that no window shows. The helper runs a probe under Run, which opens CONIN$,
// reads which processes share its console and whether the console has a window, starts a console
// grandchild plainly, as a program starts git or ssh, which reads the same of its console and holds
// the run's output, and holds until the run's Timeout. Without Terminal, the probe's console is not
// the helper's: the helper is not attached to it, and the probe is; it has no window; the grandchild
// shares the probe's console, so no console and no window opens for it; and the job's end at the
// deadline ends the grandchild, so the run ends with Ended{WhyDeadline}, not WhyEscaped. Without it,
// the probe shares the helper's console, its caller's.
//
// What it costs when this fails: a program that asks at the console, for a passphrase or a yes,
// reads the console of the person who ran the caller; a console program it starts opens a window on
// their screen; or a program started with no console of its caller's outlives the run.
func TestARunWithoutTerminalReadsNoConsoleOfItsCallersAndStaysInItsJob(t *testing.T) {
	t.Parallel()
	t.Run("without Terminal", func(t *testing.T) {
		t.Parallel()
		said, caller := underConsole(t, true)
		self := said["self"]
		console := strings.Fields(said["console"])
		if slices.Contains(console, strconv.Itoa(caller)) || !slices.Contains(console, self) {
			t.Errorf("a probe %s without Terminal shares its console with %v, want itself and not its caller %d", self, console, caller)
		}
		if said["conin"] != "ok" || said["window"] != "none" {
			t.Errorf("a probe without Terminal: CONIN$ %s, window %s, want ok, a console of its own, and none", said["conin"], said["window"])
		}
		gconsole := strings.Fields(said["gconsole"])
		if slices.Contains(gconsole, strconv.Itoa(caller)) || !slices.Contains(gconsole, self) {
			t.Errorf("the probe's console grandchild %s shares its console with %v, want the probe %s and not the caller %d", said["grandchild"], gconsole, self, caller)
		}
		if said["gwindow"] != "none" {
			t.Errorf("the probe's console grandchild %s: window %s, want none", said["grandchild"], said["gwindow"])
		}
		if said["end"] != "deadline" {
			t.Errorf("the run of a probe without Terminal whose grandchild holds its output: %s, want Ended{%s} alone", said["end"], WhyDeadline)
		}
	})
	t.Run("with Terminal", func(t *testing.T) {
		t.Parallel()
		said, caller := underConsole(t, false)
		if console := strings.Fields(said["console"]); !slices.Contains(console, strconv.Itoa(caller)) || !slices.Contains(console, said["self"]) {
			t.Errorf("a probe %s with Terminal shares its console with %v, want itself and its caller %d", said["self"], console, caller)
		}
		if said["end"] != "deadline" {
			t.Errorf("the run of a probe with Terminal whose grandchild holds its output: %s, want Ended{%s} alone", said["end"], WhyDeadline)
		}
	})
}

// TestARunWithoutTerminalRefusesAConsoleStdin: a helper started with a console of its own that no
// window shows opens that console's input, CONIN$, and runs a helper with it as Stdin: without
// Terminal the run is refused, with errTerminalStdin, which wraps ErrRefused, and Code -1, and
// the helper that would make a file makes none; with it the helper runs. The helper always has
// a console, so the row does not hang on whether whoever ran go test had one. A pipe and a regular
// file without Terminal run.
//
// What it costs when this fails: a program the caller asked to run with no console of its caller's
// reads its prompt's answer from the caller's console; or a run without Terminal and a Stdin that is
// no console is refused.
func TestARunWithoutTerminalRefusesAConsoleStdin(t *testing.T) {
	t.Parallel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	c := &process{Path: exe, Args: []string{exe, helperMode + "con-refused", t.TempDir()}, Stdout: &out, Stderr: &errs}
	quiet(c)
	if err := startTestProcess(c); err != nil {
		t.Fatal(err)
	}
	within(t, "the con-refused helper", func() { err = c.Wait() })
	if err != nil {
		t.Fatalf("the con-refused helper: %v, wrote %q and %q", err, out.String(), errs.String())
	}
	if out.String() != "with refused\nwithout ran\n" {
		t.Errorf("a run with its helper's console input as Stdin wrote %q, want with refused and without ran", out.String())
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	_ = w.Close()
	plain, err := os.Create(filepath.Join(t.TempDir(), "stdin"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plain.Close() })
	for name, stdin := range map[string]*os.File{"a pipe": r, "a regular file": plain} {
		path := filepath.Join(t.TempDir(), "made")
		c := helperCmd(t, "touch", path)
		c.Stdin = stdin
		res, err := Run(t.Context(), c)
		if _, statErr := os.Stat(path); err != nil || res.Code != 0 || statErr != nil {
			t.Errorf("%s, without Terminal: %v, Code %d, file %v, stderr %q, want a run that made its file", name, err, res.Code, statErr, res.Stderr)
		}
	}
}
