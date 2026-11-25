//go:build darwin

package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"github.com/birdtable/go-testkit"
)

// pathSocketBlock is how a dial to a Unix socket on a path made outside goes
// behind the macOS block: the sandbox's network operations take in a Unix
// socket's connect.
func pathSocketBlock(*testing.T) string { return "EPERM" }

// fileBlock is how a read or a write outside the folders goes behind the
// macOS block: the sandbox refuses it with EPERM.
func fileBlock() string { return "EPERM" }

// notifyKey is the notification the mach row reads the state of, and the
// service the launchd row asks for: a name of the test's own, which
// nothing posts and no service has.
const notifyKey = "box.netblock.row"

// osRows are the rows macOS adds: a daemon reached by mach-lookup, which the
// profile refuses, and launchd, which it does not. notifyd is the daemon
// asked, since reading a notification's state changes nothing and its client
// says when the lookup failed. Last, a debugger's attach and the name port of
// the same target, which the profile refuses.
func osRows(t *testing.T, _ net.Listener) []rowCase {
	t.Helper()
	return append([]rowCase{
		{"a daemon reached by mach-lookup, notifyd", []string{"notify"}, "ok", "refused"},
		{"launchd, by its bootstrap port, still answers", []string{"launchd"}, "answers", "answers"},
	}, taskportRows(t)...)
}

// taskportRows are the rows of another process's task ports: a caller signed with no
// entitlement asks for the control port of a target signed with get-task-allow, which a debugger
// may take, and for the same target's name port. Both are the helper in testdata/taskport. The
// target is signed and started once, so every column asks the same live target, and it is checked
// still running once they have. A build, a signing or a start that fails fails the test, and so
// does a target whose signature, read back, does not name get-task-allow.
//
// With no person to approve it, the system would hold the open control's ask for the control port
// behind a dialog, so that row then has no control, only the blocked column, as the TIOCSTI row
// has on a host that refuses TIOCSTI. The name port asks no person: with the network open it is
// given, and behind the block only the profile's (deny mach-priv-task-port) refuses it, so its
// row shows on every Mac that the clause is in force.
func taskportRows(t *testing.T) []rowCase {
	t.Helper()
	dir := t.TempDir()
	helper := testkit.Build("sandbox/cmd/netblock/testdata/taskport")
	caller := helper.Install(t, dir, "caller")
	target := helper.Install(t, dir, "target")
	plist := filepath.Join(dir, "get-task-allow.plist")
	if err := os.WriteFile(plist, []byte(getTaskAllow), 0o600); err != nil {
		t.Fatal(err)
	}
	signGetTaskAllow(t, target, plist)
	pid := strconv.Itoa(holdTarget(t, target))
	open := "ok"
	switch got := said([]string{caller, "asks"}, strings.TrimSpace); got {
	case "allowed":
	case "needs a person", "denied":
		t.Logf("the system gives no task port to this session without a person (%s), so the task-port row has no open-network control, only the blocked row", got)
		open = ""
	default:
		t.Fatalf("the task-port helper's asks said %q, want allowed, needs a person or denied", got)
	}
	return []rowCase{
		{
			"a debugger's attach: a caller with no entitlement takes the control port of a target signed with get-task-allow",
			[]string{"taskport", caller, "take", pid}, open, "refused",
		},
		{
			"a caller with no entitlement takes the name port of the same target, which no person approves",
			[]string{"taskport", caller, "name", pid}, "ok", "refused",
		},
	}
}

// signGetTaskAllow signs target ad hoc with the entitlements in plist, and fails the test unless
// codesign exits 0 and the signature it reads back names get-task-allow.
func signGetTaskAllow(t *testing.T, target, plist string) {
	t.Helper()
	sign := []string{"/usr/bin/codesign", "--sign", "-", "--force", "--entitlements", plist, target}
	if out, code, err := ran(sign); err != nil || code != 0 {
		t.Fatalf("signing the row's target with get-task-allow: exit %d, %v: %s", code, err, out)
	}
	read := []string{"/usr/bin/codesign", "--display", "--entitlements", "-", "--xml", target}
	out, code, err := ran(read)
	if err != nil || code != 0 || !strings.Contains(out, "<key>com.apple.security.get-task-allow</key><true/>") {
		t.Fatalf("the row's target, read back, is not signed with get-task-allow: exit %d, %v: %s", code, err, out)
	}
}

// holdTarget starts target, the row's helper, as the target, which holds until its stdin ends, and
// is its pid. At the test's end it fails the test if the target had ended already, then ends it.
func holdTarget(t *testing.T, target string) int {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	p, err := os.StartProcess(target, []string{target, "hold"}, &os.ProcAttr{Files: []*os.File{r, nil, nil}}) //nolint:forbidigo // the row's target is its own helper, started once and held by its stdin
	_ = r.Close()
	if err != nil {
		_ = w.Close()
		t.Fatal(err)
	}
	ended := make(chan struct{})
	go func() { _, _ = p.Wait(); close(ended) }()
	t.Cleanup(func() {
		select {
		case <-ended:
			t.Error("the task-port row's target ended before its columns had run, so what they said was not of a live target")
		default:
		}
		_ = w.Close()
		<-ended
	})
	return p.Pid
}

// getTaskAllow is the entitlements the row's target is signed with: get-task-allow, which lets a
// debugger take its control port.
const getTaskAllow = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>com.apple.security.get-task-allow</key><true/></dict></plist>
`

// osRow is a row only macOS has. notify runs notifyutil, which reads
// notifyKey's state from notifyd, and says whether notifyd answered.
// launchd asks launchd to start a service of the account that does not
// exist, which starts nothing, and says whether launchd answered that it
// found none: a process reaches launchd by its bootstrap port, with no
// lookup, so the profile does not close it, and the row holds that.
// taskport runs the caller rest[0] has, which says whether it took the
// port rest[1] asks for, take the control port or name the name port, of
// the target rest[2] names.
func osRow(_ context.Context, row string, rest []string) (string, bool) {
	switch row {
	case "taskport":
		if len(rest) != 3 || (rest[1] != "take" && rest[1] != "name") {
			return "other usage", true
		}
		return said([]string{rest[0], rest[1], rest[2]}, func(out string) string {
			if out = strings.TrimSpace(out); out == "ok" || out == "refused" || strings.HasPrefix(out, "other ") {
				return out
			}
			return "other " + out
		}), true
	case "notify":
		return said([]string{"/usr/bin/notifyutil", "-g", notifyKey}, func(out string) string {
			switch {
			case strings.Contains(out, "Failed with code"):
				return "refused"
			case strings.HasPrefix(out, notifyKey+" "):
				return "ok"
			}
			return "other " + out
		}), true
	case "launchd":
		service := "gui/" + strconv.Itoa(os.Getuid()) + "/" + notifyKey
		return said([]string{"/bin/launchctl", "kickstart", service}, func(out string) string {
			if strings.Contains(out, "Could not find service") {
				return "answers"
			}
			return "other " + out
		}), true
	}
	return "", false
}

// said runs argv, a system tool or the row's own helper, and judges what it
// wrote to stdout and stderr, whatever its exit: the system's clients say a
// refusal with an exit that is not 0. A tool that could not be run is other.
func said(argv []string, judge func(string) string) string {
	out, _, err := ran(argv)
	if err != nil {
		return "other " + err.Error()
	}
	return judge(out)
}

// ran runs argv and is what it wrote to stdout and stderr, and its exit code;
// the error is one of starting it, reading what it wrote, or waiting for it.
func ran(argv []string) (string, int, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return "", -1, err
	}
	defer func() { _ = r.Close() }()
	p, err := os.StartProcess(argv[0], argv, &os.ProcAttr{Files: []*os.File{nil, w, w}}) //nolint:forbidigo,gosec // the row runs the system's own client of a daemon it reaches over mach, its own helper the test placed, or codesign on that helper
	_ = w.Close()
	if err != nil {
		return "", -1, err
	}
	out, err := io.ReadAll(r)
	st, werr := p.Wait()
	if err == nil {
		err = werr
	}
	if err != nil {
		return string(out), -1, err
	}
	return string(out), st.ExitCode(), nil
}

// openPty opens a pseudo-terminal, its master and its terminal, neither the
// test's controlling terminal.
func openPty(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	ptm, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ptm.Close() })
	for _, req := range []uintptr{syscall.TIOCPTYGRANT, syscall.TIOCPTYUNLK} {
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, ptm.Fd(), req, 0); errno != 0 {
			t.Fatal(errno)
		}
	}
	var name [128]byte
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, ptm.Fd(), syscall.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name))); errno != 0 { //nolint:gosec // G103: the ioctl writes the terminal's name by address
		t.Fatal(errno)
	}
	pts, err := os.OpenFile(string(bytes.TrimRight(name[:], "\x00")), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pts.Close() })
	return ptm, pts
}

// tiocstiOpen is how TIOCSTI goes with the network open: macOS allows it.
func tiocstiOpen(*testing.T) string { return "ok" }

// serviceManagerRow is the row that reaches macOS's service manager, launchd, and what it says when
// launchd answers.
func serviceManagerRow() ([]string, string) { return []string{"launchd"}, "answers" }
