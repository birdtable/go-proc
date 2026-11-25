//go:build (linux && (amd64 || arm64)) || darwin

package main

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/birdtable/go-proc/sandbox"
)

// The block is proven here on the system itself, through the command: the
// test binary is started as netblock, which sets the block and execs the
// test binary again as the row child, the program it runs. The row child
// tries one way to the network, or to another process's socket, and writes
// how it went to a report file.

// asCommand makes the test binary the command, rowWord the row child, loadedWord the command
// with a load running beside it, named by the word after it, and limitedWord a caller started
// with the soft open-file limit the word after it gives, which runs the rest.
const (
	asCommand   = "netblock-command"
	rowWord     = "netblock-row"
	loadedWord  = "netblock-loaded"
	limitedWord = "netblock-limited"
)

// load starts the goroutines of the load named, which a loaded command runs as it sets the block,
// and says whether it knows the name and could start it. Each is 4 goroutines doing one thing over
// and over, as a caller's goroutines may while it calls Run:
//   - pipes: each makes a pipe and closes both ends. Go makes and marks it under syscall.ForkLock.
//   - accepts: each accepts on a listener of its own, at descriptors 3 to 6, and closes the
//     connection: Go lets one goroutine at a time accept on a listener. Go marks an accepted
//     socket in a second call, under no lock. The test dials them, from outside.
//   - rights: each receives a descriptor in an SCM_RIGHTS message, by ReadMsgUnix, and closes it; 4
//     more send them stdin. Go marks a received descriptor in a later call, under no lock.
//   - dups: each dups stdin by a raw call and closes it. Nothing marks it.
func load(name string) bool {
	var work func(i int)
	switch name {
	case "pipes":
		work = func(int) {
			if r, w, err := os.Pipe(); err == nil {
				_, _ = r.Close(), w.Close()
			}
		}
	case "accepts":
		var ls [4]net.Listener
		for i := range ls {
			f := os.NewFile(uintptr(3+i), "listener")
			l, err := net.FileListener(f)
			_ = f.Close()
			if err != nil {
				return false
			}
			ls[i] = l
		}
		work = func(i int) {
			if c, err := ls[i].Accept(); err == nil {
				_ = c.Close()
			}
		}
	case "rights":
		return rightsLoad()
	case "dups":
		work = func(int) {
			if fd, err := syscall.Dup(0); err == nil {
				_ = syscall.Close(fd)
			}
		}
	default:
		return false
	}
	for i := range 4 {
		go func() {
			for {
				work(i)
			}
		}()
	}
	return true
}

// limited sets this process's soft open-file limit to args[0] and execs self with the rest of
// args, so what it execs is a process started with that limit, as a shell's ulimit -n starts
// one: a Go program's runtime then raises the soft limit as it starts.
func limited(self string, args []string) int {
	if len(args) < 2 {
		return 1
	}
	soft, err := strconv.ParseUint(args[0], 10, 64)
	if err != nil {
		return 1
	}
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return 1
	}
	lim.Cur = soft
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return 1
	}
	_ = syscall.Exec(self, append([]string{self}, args[1:]...), os.Environ()) //nolint:forbidigo,gosec // G204: the test binary execs itself, as a shell execs a program after ulimit
	return 1
}

// rightsLoad starts the rights load, and says whether it could: 4 Unix socket pairs, each made and
// marked under syscall.ForkLock as Go makes a socket, with one goroutine that sends stdin over it
// in an SCM_RIGHTS message beside a byte, and one that receives it by ReadMsgUnix and closes what
// came.
func rightsLoad() bool {
	for range 4 {
		syscall.ForkLock.RLock()
		pair, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
		if err == nil {
			syscall.CloseOnExec(pair[0])
			syscall.CloseOnExec(pair[1])
		}
		syscall.ForkLock.RUnlock()
		if err != nil {
			return false
		}
		var ends [2]*net.UnixConn
		for i, fd := range pair {
			f := os.NewFile(uintptr(fd), "pair")
			c, err := net.FileConn(f)
			_ = f.Close()
			u, ok := c.(*net.UnixConn)
			if err != nil || !ok {
				return false
			}
			ends[i] = u
		}
		go func() {
			for {
				_, _, _ = ends[0].WriteMsgUnix([]byte{0}, syscall.UnixRights(0), nil)
			}
		}()
		go func() {
			data, oob := make([]byte, 1), make([]byte, syscall.CmsgSpace(4))
			for {
				_, n, _, _, err := ends[1].ReadMsgUnix(data, oob)
				if err != nil {
					continue
				}
				msgs, err := syscall.ParseSocketControlMessage(oob[:n])
				if err != nil {
					continue
				}
				for i := range msgs {
					fds, _ := syscall.ParseUnixRights(&msgs[i])
					for _, fd := range fds {
						_ = syscall.Close(fd)
					}
				}
			}
		}()
	}
	return true
}

// reexec runs the test binary as the command or as the row child when args say so, and says its exit
// code; when args say neither it says it did not, and the tests run.
func reexec(args []string) (code int, ran bool) {
	if len(args) > 1 {
		switch args[1] {
		case asCommand:
			// The command's own main, with its arguments where it reads them.
			os.Args = append([]string{args[0]}, args[2:]...)
			main()
			return 0, true
		case loadedWord:
			if !load(strings.Join(args[2:3], "")) {
				return 1, true
			}
			os.Args = append([]string{args[0]}, args[3:]...)
			main()
			return 0, true
		case limitedWord:
			return limited(args[0], args[2:]), true
		case heldWord:
			return holding(args[0], args[2:]), true
		case rowWord:
			return runRow(context.Background(), args[2:]), true
		}
	}
	return 0, false
}

// childEnv is the environment the test's children get: the path, and GORACE when the tests have it,
// since each child is this test binary, built with the race detector when the tests are, which
// reads its settings from GORACE (the tiers set atexit_sleep_ms=0, so a child's exit is not held a
// second). GODEBUG=containermaxprocs=0 is there, as proc's shim gives it to its own second stage,
// because each child is a Go program: in a cgroup that has a cpu.max, a container's among them, its
// runtime opens that file for GOMAXPROCS as it starts and keeps it, at descriptor 3 where that is
// free, so a row child that reads its own descriptors found one neither the caller nor the block
// left it. The command hands its environment on, so the row child has the setting too.
func childEnv() []string {
	env := []string{"PATH=/usr/bin:/bin", "GODEBUG=containermaxprocs=0"}
	if v, ok := os.LookupEnv("GORACE"); ok {
		env = append(env, "GORACE="+v)
	}
	return env
}

// outcome is how a row's call went: ok, EPERM, EACCES, or the error.
func outcome(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, syscall.EPERM):
		return "EPERM"
	case errors.Is(err, syscall.EACCES):
		return "EACCES"
	}
	return "other " + err.Error()
}

// runRow is the row child: args are the report file, the row's name and
// what it needs. A grandchild row writes the report itself.
func runRow(ctx context.Context, args []string) int {
	if len(args) < 2 {
		return 1
	}
	report, row, rest := args[0], args[1], args[2:]
	if row == "grandchild" {
		return startChild(append([]string{rowWord, report}, rest...))
	}
	if err := os.WriteFile(report, []byte(rowSays(ctx, row, rest)), 0o600); err != nil {
		return 1
	}
	return 0
}

// rowSays tries the row and says how it went.
func rowSays(ctx context.Context, row string, rest []string) string {
	switch row {
	case "dial":
		c, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, rest[0], rest[1])
		if err == nil {
			err = c.Close()
		}
		return outcome(err)
	case "fd3":
		// Whether descriptor 3 is the socket the parent handed down, by its
		// inode, so a file the runtime opened there is not taken for it.
		var st syscall.Stat_t
		switch err := syscall.Fstat(3, &st); {
		case errors.Is(err, syscall.EBADF):
			return "EBADF"
		case err != nil:
			return "other " + err.Error()
		case strconv.FormatUint(st.Ino, 10) == rest[0]:
			return "held"
		}
		return "another file"
	case "tiocsti":
		// One byte pushed into the input of the terminal on stdin, which a
		// shell sharing it would read as typed.
		c := byte('x')
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, 0, syscall.TIOCSTI, uintptr(unsafe.Pointer(&c))); errno != 0 { //nolint:gosec // G103: the ioctl takes its byte by address
			return outcome(errno)
		}
		return "ok"
	case "read":
		_, err := os.ReadFile(rest[0])
		return outcome(err)
	case "write":
		return outcome(os.WriteFile(rest[0], []byte("written behind the block\n"), 0o600))
	case "signal":
		// Signal 0 to a process outside: the kernel checks the right to
		// signal it and delivers nothing.
		pid, err := strconv.Atoi(rest[0])
		if err != nil {
			return "other " + err.Error()
		}
		return outcome(syscall.Kill(pid, 0))
	case "nothing":
		return "ok"
	case "limit":
		// This process's soft open-file limit, as a Go program sees it once its runtime has started.
		var lim syscall.Rlimit
		if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
			return outcome(err)
		}
		return strconv.FormatUint(lim.Cur, 10)
	case "descriptors":
		// Each descriptor from 3 to 255 open as the row starts, by its number and its kind, before
		// the row opens one; "none" when none is.
		var open []string
		for fd := 3; fd < 256; fd++ {
			var st syscall.Stat_t
			if syscall.Fstat(fd, &st) == nil {
				open = append(open, strconv.Itoa(fd)+" of mode "+strconv.FormatUint(uint64(st.Mode), 8))
			}
		}
		if len(open) == 0 {
			return "none"
		}
		return strings.Join(open, ", ")
	}
	if said, ok := osRow(ctx, row, rest); ok {
		return said
	}
	return "other: no row " + row
}

// marked is whether fd is marked to close at exec, by fcntl's F_GETFD: a raw call, made here since
// this file is built for Linux and macOS alone, and package syscall has no fcntl of its own.
func marked(fd int) (bool, error) {
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0)
	if errno != 0 {
		return false, errno
	}
	return flags&syscall.FD_CLOEXEC != 0, nil
}

// startChild starts this binary with args and its own environment, and
// returns its exit.
func startChild(args []string) int {
	self, err := os.Executable()
	if err != nil {
		return 1
	}
	// A nil environment is this process's own, and the grandchild writes
	// only its report.
	p, err := os.StartProcess(self, append([]string{self}, args...), &os.ProcAttr{}) //nolint:forbidigo,gosec // G702: the proof starts the test binary itself, as the program a block runs
	if err != nil {
		return 1
	}
	st, err := p.Wait()
	if err != nil {
		return 1
	}
	return st.ExitCode()
}

// proof is the test binary, by its absolute path, as the command and as the
// program it runs, and env, the environment the command is started in,
// childEnv's when nil.
type proof struct {
	self string
	env  []string
}

func newProof(t *testing.T) proof {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return proof{self: self}
}

// start runs the test binary with args, stdin (none when nil) and extra from
// descriptor 3, and returns its exit and what it printed.
func (p proof) start(t *testing.T, stdin *os.File, extra []*os.File, args ...string) (int, string) {
	t.Helper()
	return p.startIn(t, nil, stdin, extra, args...)
}

// startIn is start with sys, the session and terminal the child gets.
func (p proof) startIn(t *testing.T, sys *syscall.SysProcAttr, stdin *os.File, extra []*os.File, args ...string) (int, string) {
	t.Helper()
	outPath := filepath.Join(t.TempDir(), "out")
	out, err := os.Create(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if stdin == nil {
		if stdin, err = os.Open(os.DevNull); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = stdin.Close() }()
	}
	files := append([]*os.File{stdin, out, out}, extra...)
	env := p.env
	if env == nil {
		env = childEnv()
	}
	proc, err := os.StartProcess(p.self, append([]string{p.self}, args...), &os.ProcAttr{Env: env, Files: files, Sys: sys}) //nolint:forbidigo // the proof starts the test binary itself, as a runner starts the command
	if err != nil {
		t.Fatal(err)
	}
	st, err := proc.Wait()
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
	said, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	return st.ExitCode(), string(said)
}

// command is args as the row child sees them, behind the command when
// blocked, or run directly with the network open.
func (p proof) command(blocked bool, args ...string) []string {
	if !blocked {
		return args
	}
	return append([]string{asCommand, "--", p.self}, args...)
}

// row runs the row child with args, blocked or with the network open, with
// extra from descriptor 3, and returns what it reported.
func (p proof) row(t *testing.T, blocked bool, extra []*os.File, args ...string) string {
	t.Helper()
	report := filepath.Join(t.TempDir(), "row")
	code, out := p.start(t, nil, extra, p.command(blocked, append([]string{rowWord, report}, args...)...)...)
	said, err := os.ReadFile(report)
	if code != 0 || err != nil {
		t.Fatalf("%q (blocked %v) did not run: exit %d, %v: %s", args, blocked, code, err, out)
	}
	return string(said)
}

// rowCase is one way to the network and how it goes with the network open
// ("" when it is not tried open) and behind the block.
type rowCase struct {
	claim       string
	args        []string
	open, block string
}

// TestTheBlockRefusesEveryWayToTheNetwork runs each row through the command,
// and where a row's call works with the network open, also open as the
// control, so a refusal is the block's and not the host's.
//
// What it costs when this fails: a way to the network the block leaves open is not found, and a program
// behind the block reaches a host.
func TestTheBlockRefusesEveryWayToTheNetwork(t *testing.T) {
	t.Parallel()
	p := newProof(t)
	lc := &net.ListenConfig{}
	tcp, err := lc.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tcp.Close() })
	path := socketPath(t, "s.sock")
	pathed, err := lc.Listen(t.Context(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pathed.Close() })
	udp, err := lc.ListenPacket(t.Context(), "udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = udp.Close() })
	dialTo := tcp.Addr().String()
	rows := append([]rowCase{
		{"a dial to the loopback", []string{"dial", "tcp4", dialTo}, "ok", "EPERM"},
		{"a udp dial to the loopback", []string{"dial", "udp4", udp.LocalAddr().String()}, "ok", "EPERM"},
		{"a grandchild's dial, after a fork and an exec", []string{"grandchild", "dial", "tcp4", dialTo}, "ok", "EPERM"},
		{"a Unix socket on a path, made outside", []string{"dial", "unix", path}, "ok", pathSocketBlock(t)},
	}, osRows(t, tcp)...)
	ran := 0
	for _, r := range rows {
		ran++
		if got := p.row(t, true, nil, r.args...); got != r.block {
			t.Errorf("%s, blocked: %s, want %s", r.claim, got, r.block)
		}
		if r.open == "" {
			continue
		}
		if got := p.row(t, false, nil, r.args...); got != r.open {
			t.Errorf("%s, the open control: %s, want %s", r.claim, got, r.open)
		}
	}
	if ran < 4 {
		t.Fatalf("ran %d rows", ran)
	}
}

// TestTheBlockClosesWhatCheckSaysAndLeavesOpenWhatItSaysIsOpen: Check's Block is held to the block
// itself. A signal to the gate, a process outside, is refused behind the block where Check says
// Signals, with EPERM, and delivered where it says not, as on macOS; where the system has a row for
// its service manager (macOS's launchd), the manager answers behind the block exactly where Check
// says ServiceManager is open. The open control delivers the signal.
//
// What it costs when this fails: Check tells a caller the block closes what a program behind it still
// reaches, or the typed answer and the block drift apart unseen.
func TestTheBlockClosesWhatCheckSaysAndLeavesOpenWhatItSaysIsOpen(t *testing.T) {
	t.Parallel()
	p := newProof(t)
	block, err := sandbox.Check(sandbox.Folders{}, "")
	if err != nil {
		t.Fatalf("Check where the proof runs: %v", err)
	}
	gate := strconv.Itoa(os.Getpid())
	signalled := map[bool]string{true: "EPERM", false: "ok"}[block.Signals]
	if got := p.row(t, true, nil, "signal", gate); got != signalled {
		t.Errorf("a signal to a process outside, blocked, with Signals %t: %s, want %s", block.Signals, got, signalled)
	}
	if got := p.row(t, false, nil, "signal", gate); got != "ok" {
		t.Errorf("a signal to a process outside, the open control: %s, want ok", got)
	}
	if args, answers := serviceManagerRow(); args != nil {
		if got := p.row(t, true, nil, args...); (got == answers) == block.ServiceManager {
			t.Errorf("the service manager, blocked, with ServiceManager %t: %s (it answers with %s)", block.ServiceManager, got, answers)
		}
	}
}

// TestTheBlockClosesASocketHandedDown: a listener handed down at descriptor
// 3 is held by the program with the network open, and closed at its exec
// behind the block.
//
// What it costs when this fails: a listener handed down at descriptor 3 is held by the program behind the
// block, which then answers the network the block was to close.
func TestTheBlockClosesASocketHandedDown(t *testing.T) {
	t.Parallel()
	p := newProof(t)
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	held, err := ln.(*net.TCPListener).File()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	var st syscall.Stat_t
	if err := syscall.Fstat(int(held.Fd()), &st); err != nil {
		t.Fatal(err)
	}
	ino := strconv.FormatUint(st.Ino, 10)
	if got := p.row(t, false, []*os.File{held}, "fd3", ino); got != "held" {
		t.Errorf("a socket handed down at descriptor 3, the open control: %s, want held", got)
	}
	if got := p.row(t, true, []*os.File{held}, "fd3", ino); got == "held" {
		t.Errorf("a socket handed down at descriptor 3, blocked: %s, want it closed at exec", got)
	}
}

// member runs the row child that does nothing, blocked or open, with stdin,
// and returns whether it ran, its exit and what it said.
func (p proof) member(t *testing.T, blocked bool, stdin *os.File) (bool, int, string) {
	t.Helper()
	report := filepath.Join(t.TempDir(), "row")
	code, out := p.start(t, stdin, nil, p.command(blocked, rowWord, report, "nothing")...)
	_, err := os.Stat(report)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil, code, out
}

// TestTheBlockRefusesAStandardStreamThatIsAnInetSocket: a program whose stdin
// is a Unix socket runs, blocked or open; one whose stdin is a TCP connection
// is not run behind the block, which exits 1 and names the descriptor, and
// with the network open it runs.
//
// What it costs when this fails: a program whose stdin is a network connection runs behind the block, and
// the connection is its way out.
func TestTheBlockRefusesAStandardStreamThatIsAnInetSocket(t *testing.T) {
	t.Parallel()
	p := newProof(t)
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	ours, theirs := os.NewFile(uintptr(fds[0]), "ours"), os.NewFile(uintptr(fds[1]), "theirs")
	t.Cleanup(func() { _, _ = ours.Close(), theirs.Close() })
	for _, blocked := range []bool{false, true} {
		if ran, code, said := p.member(t, blocked, theirs); !ran || code != 0 {
			t.Errorf("a Unix socket as stdin (blocked %v): the program did not run: exit %d: %s", blocked, code, said)
		}
	}
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	c, err := (&net.Dialer{}).DialContext(t.Context(), "tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	conn, ok := c.(*net.TCPConn)
	if !ok {
		t.Fatalf("the dial gave a %T", c)
	}
	f, err := conn.File()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if ran, code, said := p.member(t, false, f); !ran || code != 0 {
		t.Errorf("the open control did not run: exit %d: %s", code, said)
	}
	ran, code, said := p.member(t, true, f)
	switch {
	case ran || code != codeRefused:
		t.Errorf("blocked, the program ran with an inet socket as stdin, or the exit was %d, not %d: %s", code, codeRefused, said)
	case !strings.Contains(said, "netblock: no block: ") || !strings.Contains(said, "descriptor 0 is of family 2"):
		t.Errorf("blocked, it said %q, want no block, and descriptor 0 named", said)
	}
}

// shortDir is a folder of the test's own under /tmp, short enough for a
// Unix socket's path, which macOS holds to 104 bytes.
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "netblock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(d); err != nil {
			t.Error(err)
		}
	})
	return d
}

// maxSocketPath is the longest path a Unix socket takes on any system the tests run on: macOS holds it to
// 104 bytes with its NUL, and the tests keep 4 under that.
const maxSocketPath = 100

// socketPath is name in a folder of the test's own, and fails the test if the path is over maxSocketPath
// bytes, which a Unix socket would refuse on macOS as a bind that fails for no reason the row names.
func socketPath(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(shortDir(t), name)
	if len(p) > maxSocketPath {
		t.Fatalf("the socket path %q is %d bytes, over the %d a Unix socket takes", p, len(p), maxSocketPath)
	}
	return p
}

// TestTheBlockRefusesToWriteIntoItsTerminalsInput: a program whose
// controlling terminal is a pseudo-terminal, as a shell outside the block
// would share it, pushes a byte into the terminal's input with TIOCSTI with
// the network open, where the host allows it, and is refused behind the
// block.
//
// What it costs when this fails: a program behind the block types into the terminal it shares with a shell,
// and the shell runs what it typed with the network open.
func TestTheBlockRefusesToWriteIntoItsTerminalsInput(t *testing.T) {
	t.Parallel()
	p := newProof(t)
	ran := 0
	for _, c := range []struct {
		blocked bool
		want    string
	}{{false, tiocstiOpen(t)}, {true, "EPERM"}} {
		if c.want == "" {
			t.Log("the host refuses TIOCSTI itself (dev.tty.legacy_tiocsti is 0), so this run has no open-network control, only the blocked row")
			continue
		}
		ran++
		ptm, pts := openPty(t)
		// The pushed byte is echoed to the terminal's output, which the
		// session's exit waits to drain, so the master is read throughout.
		drained := make(chan struct{})
		go func() { _, _ = io.Copy(io.Discard, ptm); close(drained) }()
		report := filepath.Join(t.TempDir(), "row")
		// The terminal is the child's stdin, and its controlling terminal
		// in a session of its own.
		sys := &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
		code, out := p.startIn(t, sys, pts, nil, p.command(c.blocked, rowWord, report, "tiocsti")...)
		// With no terminal open, the master's read ends.
		_ = pts.Close()
		<-drained
		said, err := os.ReadFile(report)
		if code != 0 || err != nil {
			t.Fatalf("the terminal row (blocked %v) did not run: exit %d, %v: %s", c.blocked, code, err, out)
		}
		if string(said) != c.want {
			t.Errorf("TIOCSTI into the terminal's input (blocked %v): %s, want %s", c.blocked, said, c.want)
		}
	}
	if ran == 0 {
		t.Fatal("ran no row")
	}
}

// realDir is a new folder of the test's own, by its real path, as the
// command takes its folders and the macOS sandbox matches them.
func realDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// plant writes a file the rows read and write, and fails the test if it
// cannot.
func plant(t *testing.T, path string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte("planted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// heldRow runs the row child with args behind the command with folders, its
// flags, writing its report to report, which must be beneath a read-write
// folder, and returns what it reported. The program is named by its real
// path, as on macOS a link on the way to it outside the folders, as /var
// is, cannot be followed.
func (p proof) heldRow(t *testing.T, report string, folders []string, args ...string) string {
	t.Helper()
	self, err := filepath.EvalSymlinks(p.self)
	if err != nil {
		t.Fatal(err)
	}
	line := append(append(append([]string{asCommand}, folders...), "--", self, rowWord, report), args...)
	code, out := p.start(t, nil, nil, line...)
	said, err := os.ReadFile(report)
	if code != 0 || err != nil {
		t.Fatalf("%q (held to %q) did not run: exit %d, %v: %s", args, folders, code, err, out)
	}
	return string(said)
}

// TestTheBlockHoldsTheFilesToTheFolders runs each row through the command
// held to its folders: a read-write folder that holds the report, another
// whose name is a clause of a profile, with a quote, parens and a newline,
// and read-only a folder of planted files and the program's own folder,
// with no folder named for its loader or its libraries, which RunHolding
// adds for a test binary that has them. A read and a write outside, and a write in the
// read-only folder, are refused with the block's own error, and each goes
// through with the network open, the control; a read and a write in the
// read-write folders and a read in the read-only one go through, and the
// network stays closed. With no folder, the same read and write outside go
// through behind the block, as before folders were taken.
//
// What it costs when this fails: a program behind the block reads a secret or writes a file outside the
// folders its caller named, or a folder's name opens the files, and nobody is told.
func TestTheBlockHoldsTheFilesToTheFolders(t *testing.T) {
	t.Parallel()
	p := newProof(t)
	rw, ro, outside := realDir(t), realDir(t), realDir(t)
	odd := filepath.Join(realDir(t), "a\"b)(allow file-write*)(c\nd")
	if err := os.Mkdir(odd, 0o700); err != nil {
		t.Fatal(err)
	}
	prog, err := filepath.EvalSymlinks(filepath.Dir(p.self))
	if err != nil {
		t.Fatal(err)
	}
	folders := []string{"--read-write", rw, "--read-write", odd, "--read-only", ro, "--read-only", prog}
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	secret, kept := plant(t, filepath.Join(outside, "secret")), plant(t, filepath.Join(ro, "kept"))
	rows := []rowCase{
		{"a read of a file outside the folders", []string{"read", secret}, "ok", fileBlock()},
		{"a write of a file outside the folders", []string{"write", secret}, "ok", fileBlock()},
		{"a write of a new file outside the folders", []string{"write", filepath.Join(outside, "new")}, "ok", fileBlock()},
		{"a write of a file in the read-only folder", []string{"write", kept}, "ok", fileBlock()},
		{"a read of a file in the read-only folder", []string{"read", kept}, "ok", "ok"},
		{"a read of a file in the read-write folder", []string{"read", plant(t, filepath.Join(rw, "in"))}, "ok", "ok"},
		{"a write of a new file in the read-write folder", []string{"write", filepath.Join(rw, "new")}, "ok", "ok"},
		{"a write in the folder whose name is a clause", []string{"write", filepath.Join(odd, "new")}, "ok", "ok"},
		{"a dial to the loopback, held to folders", []string{"dial", "tcp4", ln.Addr().String()}, "ok", "EPERM"},
	}
	ran := 0
	for i, r := range rows {
		ran++
		report := filepath.Join(rw, "report"+strconv.Itoa(i))
		if got := p.heldRow(t, report, folders, r.args...); got != r.block {
			t.Errorf("%s, held: %s, want %s", r.claim, got, r.block)
		}
		if got := p.row(t, false, nil, r.args...); got != r.open {
			t.Errorf("%s, the open control: %s, want %s", r.claim, got, r.open)
		}
	}
	if ran < 9 {
		t.Fatalf("ran %d rows", ran)
	}
	for _, args := range [][]string{{"read", secret}, {"write", secret}} {
		if got := p.row(t, true, nil, args...); got != "ok" {
			t.Errorf("%q behind the block with no folder: %s, want ok, the files open", args, got)
		}
	}
}

// TestTheCommandRefusesAFolderItCannotHold: a folder flag with no folder, a
// relative folder, one that does not exist, a file, a folder named twice
// under either name and a read-only folder beneath a read-write one each
// exit 2 with the reason, and the program is not run.
//
// What it costs when this fails: a folder the command cannot hold as named is dropped or widened, and
// the program runs with files open its caller meant closed.
func TestTheCommandRefusesAFolderItCannotHold(t *testing.T) {
	t.Parallel()
	p := newProof(t)
	dir := realDir(t)
	inner := filepath.Join(dir, "inner")
	if err := os.Mkdir(inner, 0o700); err != nil {
		t.Fatal(err)
	}
	file := plant(t, filepath.Join(dir, "file"))
	rows := []struct {
		claim   string
		folders []string
		says    string
	}{
		{"a flag with no folder", []string{"--read-only"}, "--read-only takes a folder"},
		{"a relative folder", []string{"--read-write", "box"}, "not an absolute path"},
		{"a folder that does not exist", []string{"--read-only", filepath.Join(dir, "gone")}, "cannot be held"},
		{"a file", []string{"--read-write", file}, "it is not a folder"},
		{"a folder named twice", []string{"--read-write", dir, "--read-write", dir}, "named twice"},
		{"a folder named twice, once with a slash", []string{"--read-write", dir, "--read-only", dir + "/"}, "named twice"},
		{"a read-only folder beneath a read-write one", []string{"--read-write", dir, "--read-only", inner}, "beneath the read-write folder"},
	}
	for _, r := range rows {
		report := filepath.Join(t.TempDir(), "row")
		line := append([]string{asCommand}, r.folders...)
		if r.claim != "a flag with no folder" {
			line = append(line, "--", p.self, rowWord, report, "nothing")
		}
		code, said := p.start(t, nil, nil, line...)
		if code != codeUsage || !strings.Contains(said, "netblock: ") || !strings.Contains(said, r.says) {
			t.Errorf("%s: exit %d, said %q; want exit %d, saying %q", r.claim, code, said, codeUsage, r.says)
		}
		if _, err := os.Stat(report); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: the program ran: %v", r.claim, err)
		}
	}
	if len(rows) == 0 {
		t.Fatal("no rows ran")
	}
}
