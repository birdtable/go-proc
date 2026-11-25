//go:build linux && (amd64 || arm64)

package main

import (
	"context"
	"encoding/binary"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	ll "github.com/landlock-lsm/go-landlock/landlock/syscall"
)

// landlockABI is the Landlock ABI the kernel speaks, as go-landlock asks
// it, 0 when it has none.
func landlockABI() int {
	abi, err := ll.LandlockGetABIVersion()
	if err != nil {
		return 0
	}
	return abi
}

// pathSocketBlock is how a dial to a Unix socket on a path made outside goes
// behind the Linux block: Landlock refuses it with EACCES from ABI 9, and
// below it the filter refuses every connect with EPERM.
func pathSocketBlock(*testing.T) string {
	if landlockABI() >= 9 {
		return "EACCES"
	}
	return "EPERM"
}

// ownSocketBlock is how a program's dial to a Unix socket it made itself
// goes behind the Linux block: Landlock lets it answer from ABI 9, and below
// it the filter refuses every connect.
func ownSocketBlock() string {
	if landlockABI() >= 9 {
		return "ok"
	}
	return "EPERM"
}

// fileBlock is how a read or a write outside the folders goes behind the
// Linux block: Landlock refuses it with EACCES.
func fileBlock() string { return "EACCES" }

// The kernel's numbers the rows call that package syscall does not name.
// Every call from 424 on has one number on every architecture.
const (
	sysIoUringSetup = 425
	sysPidfdOpen    = 434
	sysPidfdGetfd   = 438
)

// osRows are the rows Linux adds, with the listener the gate holds for the
// pidfd_getfd row.
func osRows(t *testing.T, held net.Listener) []rowCase {
	t.Helper()
	f, err := held.(*net.TCPListener).File()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	abstract := "@app-netblock-" + strconv.Itoa(os.Getpid())
	hidden, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", abstract)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hidden.Close() })
	gram := socketPath(t, "g.sock")
	inbox, err := (&net.ListenConfig{}).ListenPacket(t.Context(), "unixgram", gram)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inbox.Close() })
	sock := func(fam, typ int) []string { return []string{"socket", strconv.Itoa(fam), strconv.Itoa(typ)} }
	return []rowCase{
		{"a tcp4 socket", sock(syscall.AF_INET, syscall.SOCK_STREAM), "ok", "EPERM"},
		{"a udp socket", sock(syscall.AF_INET, syscall.SOCK_DGRAM), "ok", "EPERM"},
		{"a tcp6 socket", sock(syscall.AF_INET6, syscall.SOCK_STREAM), "", "EPERM"},
		{"a netlink socket", sock(syscall.AF_NETLINK, syscall.SOCK_RAW), "ok", "EPERM"},
		{"a raw packet socket", sock(syscall.AF_PACKET, syscall.SOCK_RAW), "", "EPERM"},
		{"a Unix socket pair is allowed", sock(syscall.AF_UNIX, syscall.SOCK_STREAM), "ok", "ok"},
		{"an abstract Unix socket outside", []string{"dial", "unix", abstract}, "ok", "EPERM"},
		{"a Unix socket on a path the program makes itself", []string{"own_socket"}, "ok", ownSocketBlock()},
		{"a datagram to a Unix socket on a path outside, by sendto", []string{"send", "sendto", gram}, "ok", pathSocketBlock(t)},
		{"a datagram to a Unix socket on a path outside, by sendmsg", []string{"send", "sendmsg", gram}, "ok", pathSocketBlock(t)},
		{"a datagram to a Unix socket on a path outside, by sendmmsg", []string{"send", "sendmmsg", gram}, "ok", pathSocketBlock(t)},
		{"io_uring", []string{"io_uring"}, "", "EPERM"},
		{"pidfd_getfd of the gate's listener", []string{"pidfd_getfd", strconv.Itoa(os.Getpid()), strconv.Itoa(int(f.Fd()))}, "", "EPERM"},
		{"pidfd_getfd of its own descriptor", []string{"pidfd_getfd", "self", "1"}, "ok", "EPERM"},
		{"ptrace", []string{"ptrace"}, "other no such process", "EPERM"},
		{"process_vm_writev", []string{"vm_writev"}, "ok", "EPERM"},
	}
}

// osRow is a row only Linux has, under ctx: rest is what it needs.
func osRow(ctx context.Context, row string, rest []string) (string, bool) {
	num := func(i int) uintptr {
		n, err := strconv.Atoi(rest[i])
		if err != nil {
			panic(err)
		}
		return uintptr(n)
	}
	switch row {
	case "own_socket":
		return ownSocket(ctx), true
	case "send":
		return send(rest[0], rest[1]), true
	case "socket":
		fd, err := syscall.Socket(int(num(0)), int(num(1)), 0)
		if err == nil {
			err = syscall.Close(fd)
		}
		return outcome(err), true
	case "io_uring":
		_, _, errno := syscall.Syscall(sysIoUringSetup, 0, 0, 0)
		if errno == syscall.EPERM {
			return "EPERM", true
		}
		return "other " + errno.Error(), true
	case "pidfd_getfd":
		pid := uintptr(os.Getpid())
		if rest[0] != "self" {
			pid = num(0)
		}
		pidfd, _, errno := syscall.Syscall(sysPidfdOpen, pid, 0, 0)
		if errno != 0 {
			return "open " + outcome(errno), true
		}
		fd, _, errno := syscall.Syscall(sysPidfdGetfd, pidfd, num(1), 0)
		var err error
		if errno != 0 {
			err = errno
		} else {
			err = syscall.Close(int(fd))
		}
		return outcome(errors.Join(err, syscall.Close(int(pidfd)))), true
	case "ptrace":
		_, err := syscall.PtracePeekData(os.Getppid(), 0, make([]byte, 8))
		return outcome(err), true
	case "vm_writev":
		_, _, errno := syscall.Syscall6(sysProcessVMWritev, uintptr(os.Getpid()), 0, 0, 0, 0, 0)
		if errno == 0 {
			return "ok", true
		}
		return outcome(errno), true
	}
	return "", false
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
	var unlock int32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, ptm.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); errno != 0 { //nolint:gosec // G103: the ioctl takes its flag by address
		t.Fatal(errno)
	}
	var n uint32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, ptm.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); errno != 0 { //nolint:gosec // G103: the ioctl writes the terminal's number by address
		t.Fatal(errno)
	}
	pts, err := os.OpenFile("/dev/pts/"+strconv.FormatUint(uint64(n), 10), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pts.Close() })
	return ptm, pts
}

// tiocstiOpen is how TIOCSTI goes with the network open: it works unless the
// host has turned it off (dev.tty.legacy_tiocsti, Linux 6.2 on), and then
// there is no control, since the host already refuses it, with EIO, and the
// block's refusal is EPERM.
func tiocstiOpen(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("/proc/sys/dev/tty/legacy_tiocsti")
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "ok"
	case err != nil:
		t.Fatal(err)
	case strings.TrimSpace(string(b)) == "0":
		return ""
	}
	return "ok"
}

// ownSocket listens on a Unix socket on a path in a folder it makes, behind
// whatever block it runs under, dials it, and says how the dial went.
func ownSocket(ctx context.Context) string {
	dir, err := os.MkdirTemp("", "netblock-own-")
	if err != nil {
		return "other " + err.Error()
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := dir + "/s.sock"
	if len(path) > maxSocketPath {
		return "other: the socket path is " + strconv.Itoa(len(path)) + " bytes, over the " + strconv.Itoa(maxSocketPath) + " a Unix socket takes"
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "unix", path)
	if err != nil {
		return "listen " + outcome(err)
	}
	defer func() { _ = ln.Close() }()
	c, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err == nil {
		err = c.Close()
	}
	return outcome(err)
}

// send sends one datagram to the Unix socket on path, from a new socket, by
// the call named, and says how it went.
func send(call, path string) string {
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return "socket " + outcome(err)
	}
	defer func() { _ = syscall.Close(fd) }()
	to := &syscall.SockaddrUnix{Name: path}
	switch call {
	case "sendto":
		err = syscall.Sendto(fd, []byte("x"), 0, to)
	case "sendmsg":
		_, err = syscall.SendmsgN(fd, []byte("x"), nil, to, 0)
	case "sendmmsg":
		err = sendmmsg(fd, path)
	default:
		return "other: no call " + call
	}
	return outcome(err)
}

// sendmmsg sends one datagram to the Unix socket on path by sendmmsg, which
// package syscall has no wrapper for: one struct mmsghdr, its message's
// address the socket's. The address is a struct sockaddr_un written out as
// its bytes: the family, two bytes in the machine's order, then the path and
// its NUL.
func sendmmsg(fd int, path string) error {
	var sa [2 + len(syscall.RawSockaddrUnix{}.Path)]byte
	if 2+len(path) >= len(sa) {
		return syscall.ENAMETOOLONG
	}
	binary.NativeEndian.PutUint16(sa[:2], syscall.AF_UNIX)
	copy(sa[2:], path)
	b := []byte("x")
	iov := syscall.Iovec{Base: &b[0]}
	iov.SetLen(len(b))
	var m struct {
		hdr syscall.Msghdr
		n   uint32
		_   [4]byte
	}
	m.hdr.Name = &sa[0]
	m.hdr.Namelen = uint32(2 + len(path) + 1)
	m.hdr.Iov = &iov
	m.hdr.Iovlen = 1
	if _, _, errno := syscall.Syscall6(sysSendmmsg, uintptr(fd), uintptr(unsafe.Pointer(&m)), 1, 0, 0, 0); errno != 0 { //nolint:gosec // G103: the call takes its messages by address
		return errno
	}
	return nil
}

// TestTheProbesFolderIsGoneAfterAHeldRun runs a row through the command held
// to its folders, with TMPDIR at a folder of the test's own outside them, so
// the probe's folder is made there. Once the row has run, nothing is left
// in that folder: the ruleset denies the probe's folder to the thread it
// restricts, so it was removed by another thread, which the ruleset did not
// restrict.
//
// What it costs when this fails: the ruleset restricts every thread of the command, not the one that
// execs, or the probe's folder of sockets is left in the temporary folder after every run.
func TestTheProbesFolderIsGoneAfterAHeldRun(t *testing.T) {
	t.Parallel()
	p := newProof(t)
	// A short folder, so the probe's socket paths fit a Unix socket's address.
	tmp, err := filepath.EvalSymlinks(shortDir(t))
	if err != nil {
		t.Fatal(err)
	}
	rw := realDir(t)
	p.env = append(childEnv(), "TMPDIR="+tmp)
	prog, err := filepath.EvalSymlinks(filepath.Dir(p.self))
	if err != nil {
		t.Fatal(err)
	}
	folders := []string{"--read-write", rw, "--read-only", prog}
	if got := p.heldRow(t, filepath.Join(rw, "report"), folders, "read", plant(t, filepath.Join(rw, "in"))); got != "ok" {
		t.Fatalf("the held row: %s, want ok", got)
	}
	left, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range left {
		t.Errorf("%s is left in TMPDIR after the run", e.Name())
	}
}

// TestTheCommandRefusesAProgramWhoseLoadFoldersItCannotTell: held to folders, a #! script and a
// file that is no ELF file each exit 2 with the reason, naming the file, and nothing is run: the
// script, whose folder is granted, writes no line.
//
// What it costs when this fails: the command runs a program behind folders it guessed, or calls a
// program it refused a block it could not set, exit 5.
func TestTheCommandRefusesAProgramWhoseLoadFoldersItCannotTell(t *testing.T) {
	t.Parallel()
	p := newProof(t)
	rw, bin := realDir(t), realDir(t)
	for name, r := range map[string]struct{ holds, says string }{
		"script": {"#!/bin/sh\necho the script ran\n", "it is a #! script, and its interpreter is the program to run"},
		"text":   {"no program\n", "it is not an ELF file"},
	} {
		prog := filepath.Join(bin, name)
		if err := os.WriteFile(prog, []byte(r.holds), 0o700); err != nil {
			t.Fatal(err)
		}
		code, said := p.start(t, nil, nil, asCommand, "--read-write", rw, "--read-only", bin, "--", prog)
		if code != codeUsage || !strings.Contains(said, "netblock: the program \""+prog+"\"") || !strings.Contains(said, r.says) || strings.Contains(said, "the script ran") {
			t.Errorf("a %s as the program: exit %d, said %q; want exit %d, the file named and %q", name, code, said, codeUsage, r.says)
		}
	}
}

// serviceManagerRow is none on Linux: its service manager is reached by a Unix socket, which the
// socket rows hold closed, and a host the proof runs on need not run one.
func serviceManagerRow() ([]string, string) { return nil, "" }
