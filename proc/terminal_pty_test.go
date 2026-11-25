//go:build linux || darwin

package proc

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// openPty opens a pseudo-terminal of the test's own, as posix_openpt, grantpt, unlockpt and ptsname
// do: its master, and its terminal, which a child takes as its controlling terminal. Neither becomes
// this process's. Both are closed when the test ends.
func openPty(t *testing.T) (master, term *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = master.Close() })
	name, err := ptyTerm(master)
	if err != nil {
		t.Fatalf("unlocking and naming the pseudo-terminal: %v", err)
	}
	term, err = os.OpenFile(name, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = term.Close() })
	return master, term
}

// underPty starts a tty-run helper in a session of its own whose controlling terminal is a
// pseudo-terminal the test opened, never the terminal of whoever ran go test, and gives what it
// wrote, by each line's first word, and its pid.
func underPty(t *testing.T, noTerminal bool) (said map[string][]string, pid int) {
	t.Helper()
	with := "0"
	if noTerminal {
		with = "1"
	}
	return underPtyWith(t, with)
}

// underPtyWith is underPty with tty-run's with: "0" for Terminal, "1" for none, or "peak" for
// none with ReadPeak.
func underPtyWith(t *testing.T, with string) (said map[string][]string, pid int) {
	t.Helper()
	_, term := openPty(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	c := &process{
		Path: exe, Args: []string{exe, helperMode + "tty-run", with}, Stdin: term, Stdout: &out, Stderr: &errs,
		SysProcAttr: &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0},
	}
	if err := startTestProcess(c); err != nil {
		t.Fatal(err)
	}
	_ = term.Close()
	within(t, "the tty-run helper", func() { err = c.Wait() })
	if err != nil {
		t.Fatalf("the tty-run helper: %v, wrote %q and %q", err, out.String(), errs.String())
	}
	said = map[string][]string{}
	for line := range strings.Lines(out.String()) {
		word, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
		said[word] = append(said[word], rest)
	}
	for _, word := range []string{"ctty", "tty", "self", "grandchild", "end"} {
		if len(said[word]) != 1 {
			t.Fatalf("the tty-run helper wrote %q, want one %s line", out.String(), word)
		}
	}
	if g, err := strconv.Atoi(strings.Fields(said["grandchild"][0] + " -")[0]); err == nil && said["end"][0] != "cancelled" {
		// The run did not end the grandchild, which holds for holdFor unless it is killed.
		t.Cleanup(func() { _ = signalPid(syscall.Kill, g, syscall.SIGKILL) })
	}
	if said["ctty"][0] != "some" {
		t.Fatalf("the tty-run helper has no controlling terminal (%q), so the probe could reach none", out.String())
	}
	return said, c.Process.Pid
}

// idsOf reads an ids line, "<pid> <pgid> <sid>".
func idsOf(t *testing.T, what, line string) (pid, pgid, sid int) {
	t.Helper()
	f := strings.Fields(line)
	if len(f) != 3 {
		t.Fatalf("%s: %q is not a pid, a group and a session", what, line)
	}
	n := make([]int, 3)
	for i, s := range f {
		v, err := strconv.Atoi(s)
		if err != nil || v <= 0 {
			t.Fatalf("%s: %q is not a pid, a group and a session", what, line)
		}
		n[i] = v
	}
	return n[0], n[1], n[2]
}

// TestARunWithoutTerminalReachesNoTerminalOfItsCallersAndStaysInItsTree: the test opens a
// pseudo-terminal of its own and starts a helper in a session whose controlling terminal it is. The
// helper runs a probe under Run, which opens /dev/tty, reads its ids, starts a holding grandchild
// that holds the run's output, says it is ready, and holds; the run's context ends then. Without
// Terminal, the probe's open of /dev/tty is ENXIO; the probe leads its own session and its own
// group, and the grandchild is in both, so the kill of the group as the context ends ends it, and
// the run ends with Ended{WhyCancelled}, not WhyEscaped. With it, the probe opens the helper's
// terminal, the test's pseudo-terminal, from the helper's session, in a group of its own that the
// same kill ends as well.
//
// What it costs when this fails: a program that asks at the terminal, for a passphrase or a yes,
// asks the person who ran the caller, or waits on them until its Timeout; or a program started with
// no terminal is out of the tree the run kills, and outlives the run.
func TestARunWithoutTerminalReachesNoTerminalOfItsCallersAndStaysInItsTree(t *testing.T) {
	t.Parallel()
	t.Run("without Terminal", func(t *testing.T) {
		t.Parallel()
		said, _ := underPty(t, true)
		if got := said["tty"][0]; got != "ENXIO" {
			t.Errorf("a probe without Terminal opening /dev/tty: %s, want ENXIO", got)
		}
		pid, pgid, sid := idsOf(t, "the probe", said["self"][0])
		if pgid != pid || sid != pid {
			t.Errorf("a probe without Terminal, pid %d: group %d and session %d, want both its pid", pid, pgid, sid)
		}
		g, gpgid, gsid := idsOf(t, "the grandchild", said["grandchild"][0])
		if gpgid != pid || gsid != pid {
			t.Errorf("the probe's grandchild %d: group %d and session %d, want the probe's %d for both", g, gpgid, gsid, pid)
		}
		if got := said["end"][0]; got != "cancelled" {
			t.Errorf("the run of a probe without Terminal whose grandchild holds its output: %s, want Ended{%s} alone", got, WhyCancelled)
		}
	})
	t.Run("with Terminal", func(t *testing.T) {
		t.Parallel()
		said, helper := underPty(t, false)
		if got := said["tty"][0]; got != "ok" {
			t.Errorf("a probe with Terminal opening /dev/tty under the test's pseudo-terminal: %s, want ok", got)
		}
		pid, pgid, sid := idsOf(t, "the probe", said["self"][0])
		if pgid != pid || sid != helper {
			t.Errorf("a probe with Terminal, pid %d: group %d and session %d, want its pid and the helper's %d", pid, pgid, sid, helper)
		}
		g, gpgid, _ := idsOf(t, "the grandchild", said["grandchild"][0])
		if gpgid != pid {
			t.Errorf("the probe's grandchild %d: group %d, want the probe's %d", g, gpgid, pid)
		}
		if got := said["end"][0]; got != "cancelled" {
			t.Errorf("the run of a probe with Terminal whose grandchild holds its output: %s, want Ended{%s} alone", got, WhyCancelled)
		}
	})
}

// TestARunWithoutTerminalRefusesATerminalStdin: a run without Terminal whose Stdin is a terminal of a
// pseudo-terminal the test opened is refused, with errTerminalStdin, which wraps ErrRefused, and
// Code -1, and the helper that would make a file makes none. The same terminal with Terminal
// runs; a pipe and a regular file without Terminal run.
//
// What it costs when this fails: a program the caller asked to run with no terminal reads its
// prompt's answer from the caller's terminal, as ssh-keygen falls back to stdin; or a run with
// no Terminal and a Stdin that is no terminal is refused.
func TestARunWithoutTerminalRefusesATerminalStdin(t *testing.T) {
	t.Parallel()
	_, term := openPty(t)
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
	for _, row := range []struct {
		name       string
		stdin      *os.File
		noTerminal bool
		refused    bool
	}{
		{"a terminal, without Terminal", term, true, true},
		{"a terminal, with Terminal", term, false, false},
		{"a pipe, without Terminal", r, true, false},
		{"a regular file, without Terminal", plain, true, false},
	} {
		path := filepath.Join(t.TempDir(), "made")
		c := helperCmd(t, "touch", path)
		c.Stdin, c.Terminal = row.stdin, !row.noTerminal
		res, err := Run(t.Context(), c)
		_, statErr := os.Stat(path)
		switch {
		case row.refused && (!errors.Is(err, errTerminalStdin) || !errors.Is(err, ErrRefused) || res.Code != -1):
			t.Errorf("%s: %v, Code %d, want errTerminalStdin, ErrRefused and Code -1", row.name, err, res.Code)
		case row.refused && !errors.Is(statErr, os.ErrNotExist):
			t.Errorf("%s: the program made its file (%v), want nothing started", row.name, statErr)
		case !row.refused && (err != nil || res.Code != 0 || statErr != nil):
			t.Errorf("%s: %v, Code %d, file %v, stderr %q, want a run that made its file", row.name, err, res.Code, statErr, res.Stderr)
		}
	}
}
