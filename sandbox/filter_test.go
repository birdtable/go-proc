package sandbox

import (
	"encoding/binary"
	"errors"
	"fmt"
	"syscall"
	"testing"
)

// The audit values of the architectures a process can call the kernel in on
// an x86_64 or arm64 machine beside its own, from linux/audit.h.
const (
	auditI386 = 0x40000003
	auditARM  = 0x40000028
)

// The address families the rows name, from linux/socket.h.
const (
	afInet    = 2
	afInet6   = 10
	afNetlink = 16
	afPacket  = 17
	afVsock   = 40
)

// tiocgwinsz is TIOCGWINSZ of asm-generic/ioctls.h: it reads a terminal's
// size, and changes nothing.
const tiocgwinsz = 0x5413

// runFilter runs prog, classic BPF as the kernel's seccomp runs it, over the
// seccomp data of one call: its number, its architecture's audit value and
// its arguments, from the first, the rest 0. It knows the instructions the
// filter uses and fails the test on any other, and on a jump out of the
// program.
func runFilter(t *testing.T, prog []sockFilter, nr, arch uint32, args ...uint64) uint32 {
	t.Helper()
	data := make([]byte, 64)
	binary.LittleEndian.PutUint32(data[seccompNr:], nr)
	binary.LittleEndian.PutUint32(data[seccompArch:], arch)
	if len(args) > 6 {
		t.Fatalf("a call has six arguments, not %d", len(args))
	}
	for i, arg := range args {
		binary.LittleEndian.PutUint64(data[seccompArg0Low+8*i:], arg)
	}
	var acc uint32
	for pc := 0; pc < len(prog); pc++ {
		in := prog[pc]
		switch in.Code {
		case bpfLdWAbs:
			if int(in.K)+4 > len(data) {
				t.Fatalf("instruction %d loads at %d, past the seccomp data", pc, in.K)
			}
			acc = binary.LittleEndian.Uint32(data[in.K:])
		case bpfJeqK, bpfJgeK:
			hit := acc == in.K
			if in.Code == bpfJgeK {
				hit = acc >= in.K
			}
			if hit {
				pc += int(in.Jt)
			} else {
				pc += int(in.Jf)
			}
		case bpfRetK:
			return in.K
		default:
			t.Fatalf("instruction %d has the opcode %#x, which the filter does not use", pc, in.Code)
		}
	}
	t.Fatalf("the program ran off its end")
	return 0
}

// verdict names a filter's return value as the rows say it.
func verdict(ret uint32) string {
	switch ret {
	case seccompRetAllow:
		return "allow"
	case seccompRetKill:
		return "kill"
	case seccompRetErrno | 1: // EPERM, from asm-generic/errno-base.h
		return "EPERM"
	}
	return fmt.Sprintf("%#x", ret)
}

// TestTheFilterLoadIsTSYNCWithESRCH: the seccomp call asks for TSYNC and for
// TSYNC_ESRCH, as linux/seccomp.h numbers them, and its answer is a refusal
// on an errno and on any first result but 0, a thread's id, with no errno.
//
// What it costs when this fails: a thread that could not be synchronized
// leaves the filter set on no thread, the call answers that thread's id with
// no errno, and the block goes on as if the filter held.
func TestTheFilterLoadIsTSYNCWithESRCH(t *testing.T) {
	t.Parallel()
	if seccompFilterFlagTsync != 1 || seccompFilterFlagTsyncESRCH != 1<<4 {
		t.Errorf("the flags are %#x and %#x, want linux/seccomp.h's 0x1 and 0x10", seccompFilterFlagTsync, seccompFilterFlagTsyncESRCH)
	}
	if seccompFilterFlags != seccompFilterFlagTsync|seccompFilterFlagTsyncESRCH {
		t.Errorf("the load asks for %#x, want TSYNC and TSYNC_ESRCH, %#x", seccompFilterFlags, seccompFilterFlagTsync|seccompFilterFlagTsyncESRCH)
	}
	rows := []struct {
		claim string
		r1    uintptr
		errno syscall.Errno
		want  error
	}{
		{"set on every thread", 0, 0, nil},
		{"ESRCH", 0, syscall.ESRCH, syscall.ESRCH},
		{"a thread's id and no errno", 4242, 0, errFilterUnsynced},
		{"a thread's id and an errno", 4242, syscall.EINVAL, syscall.EINVAL},
	}
	for _, r := range rows {
		err := filterLoaded(r.r1, r.errno)
		if (err == nil) != (r.want == nil) || !errors.Is(err, r.want) {
			t.Errorf("%s: %v, want %v", r.claim, err, r.want)
		}
	}
}

// TestTheFilterIsWellFormed: each architecture's program, with the sends
// closed and open, is short enough for the kernel, every jump lands inside
// it, and its last instruction returns.
//
// What it costs when this fails: a program too long for the kernel, or with a jump that lands outside it, does
// not load, and the block either refuses every run or runs with no filter.
func TestTheFilterIsWellFormed(t *testing.T) {
	t.Parallel()
	ran, planted := 0, 0
	for arch, a := range filterArches() {
		for _, closeSends := range []bool{false, true} {
			wellFormed(t, fmt.Sprintf("%s, sends closed %t", arch, closeSends), a, closeSends)
			ran++
		}
	}
	for _, ins := range [][]insn{
		{{code: bpfRetK, label: "back"}, {code: bpfJeqK, jt: "back"}},
		{{code: bpfJeqK, jf: "self", label: "self"}, {code: bpfRetK}},
		{{code: bpfJeqK, jt: "nowhere"}, {code: bpfRetK}},
	} {
		if prog, err := assemble(ins); err == nil || prog != nil {
			t.Errorf("a jump to a label behind it, at it or missing was assembled: %v", prog)
		}
		planted++
	}
	if ran == 0 || planted != 3 {
		t.Fatalf("%d programs and %d planted jumps were checked", ran, planted)
	}
}

// wellFormed holds one program to the kernel's length, its jumps inside it,
// and a return last.
func wellFormed(t *testing.T, arch string, a filterArch, closeSends bool) {
	t.Helper()
	prog, err := a.program(closeSends)
	if err != nil {
		t.Fatalf("%s: %v", arch, err)
	}
	if len(prog) == 0 || len(prog) > 4096 {
		t.Fatalf("%s: %d instructions", arch, len(prog))
	}
	for i, in := range prog {
		if in.Code == bpfJeqK || in.Code == bpfJgeK {
			if i+1+int(in.Jt) >= len(prog) || i+1+int(in.Jf) >= len(prog) {
				t.Errorf("%s: instruction %d jumps out of the program", arch, i)
			}
		}
	}
	if prog[len(prog)-1].Code != bpfRetK {
		t.Errorf("%s: the last instruction is %#x, not a return", arch, prog[len(prog)-1].Code)
	}
}

// filterRow is one call the filter is decoded over, and its verdict.
type filterRow struct {
	claim   string
	nr, aud uint32
	arg0    uint64
	want    string
}

// TestTheFilterHoldsEachCall decodes each architecture's program, with the
// sends closed and open, over the calls it judges: another architecture's
// convention is killed before any call is read, an x32 call is refused, a
// socket or a socket pair passes for AF_UNIX alone, io_uring and the three
// calls that take another process's descriptor or memory are refused, an
// ioctl is refused for TIOCSTI and TIOCLINUX alone, and an ordinary call
// passes. With the sends closed, connect, sendmsg and sendmmsg are refused,
// and sendto is refused when either half of its address is not 0; with them
// open, each passes.
//
// What it costs when this fails: a call that reaches the network or another process passes the filter, or an
// ordinary call is refused, and the block lets a program out or breaks it.
func TestTheFilterHoldsEachCall(t *testing.T) {
	t.Parallel()
	ran := 0
	for arch, a := range filterArches() {
		for _, closeSends := range []bool{false, true} {
			holdsEachCall(t, arch, a, closeSends)
			ran++
		}
	}
	for _, arch := range []string{"amd64", "arm64"} {
		if _, ok := filterArches()[arch]; !ok {
			t.Errorf("no filter for %s, which the block runs on in Linux", arch)
		}
	}
	if ran == 0 {
		t.Fatal("no program was decoded")
	}
}

// holdsEachCall decodes one program over the calls TestTheFilterHoldsEachCall
// names.
func holdsEachCall(t *testing.T, arch string, a filterArch, closeSends bool) {
	t.Helper()
	prog, err := a.program(closeSends)
	if err != nil {
		t.Fatalf("%s: %v", arch, err)
	}
	arch = fmt.Sprintf("%s, sends closed %t", arch, closeSends)
	other := filterArches()["arm64"].audit
	if a.audit == other {
		other = filterArches()["amd64"].audit
	}
	rows := []filterRow{
		{"an ordinary call passes", 0, a.audit, 0, "allow"},
		{"a socket of AF_UNIX passes", a.socket, a.audit, afUnix, "allow"},
		{"a socket pair of AF_UNIX passes", a.socketpair, a.audit, afUnix, "allow"},
		{"a socket of AF_INET is refused", a.socket, a.audit, afInet, "EPERM"},
		{"a socket of AF_INET6 is refused", a.socket, a.audit, afInet6, "EPERM"},
		{"a socket of AF_NETLINK is refused", a.socket, a.audit, afNetlink, "EPERM"},
		{"a socket of AF_PACKET is refused", a.socket, a.audit, afPacket, "EPERM"},
		{"a socket of AF_VSOCK is refused", a.socket, a.audit, afVsock, "EPERM"},
		{"a socket of family 0 is refused", a.socket, a.audit, 0, "EPERM"},
		{"a socket pair of AF_INET is refused", a.socketpair, a.audit, afInet, "EPERM"},
		{"io_uring_setup is refused", a.refused["io_uring_setup"], a.audit, 0, "EPERM"},
		{"pidfd_getfd is refused", a.refused["pidfd_getfd"], a.audit, 0, "EPERM"},
		{"ptrace is refused", a.refused["ptrace"], a.audit, 0, "EPERM"},
		{"process_vm_writev is refused", a.refused["process_vm_writev"], a.audit, 0, "EPERM"},
		{"the i386 convention is killed", a.socket, auditI386, afUnix, "kill"},
		{"the AArch32 convention is killed", a.socket, auditARM, afUnix, "kill"},
		{"the other 64-bit convention is killed", 0, other, 0, "kill"},
	}
	if a.x32 {
		rows = append(rows,
			filterRow{"an x32 socket of AF_UNIX is refused", x32SyscallBit | a.socket, a.audit, afUnix, "EPERM"},
			filterRow{"an x32 ordinary call is refused", x32SyscallBit, a.audit, 0, "EPERM"})
	}
	for _, r := range rows {
		if got := verdict(runFilter(t, prog, r.nr, r.aud, r.arg0)); got != r.want {
			t.Errorf("%s: %s: %s, want %s", arch, r.claim, got, r.want)
		}
	}
	ioctls := []struct {
		claim string
		nr    uint32
		req   uint64
		want  string
	}{
		{"an ioctl that reads a terminal's size passes", a.ioctl, tiocgwinsz, "allow"},
		{"TIOCSTI is refused", a.ioctl, tiocsti, "EPERM"},
		{"TIOCLINUX is refused", a.ioctl, tioclinux, "EPERM"},
		{"TIOCSTI with a high half of junk is refused", a.ioctl, 0xdeadbeef<<32 | tiocsti, "EPERM"},
		{"TIOCSTI's number as another call's second argument passes", 0, tiocsti, "allow"},
	}
	for _, r := range ioctls {
		if got := verdict(runFilter(t, prog, r.nr, a.audit, 0, r.req)); got != r.want {
			t.Errorf("%s: %s: %s, want %s", arch, r.claim, got, r.want)
		}
	}
	// What a call that reaches a socket by its address meets in this mode.
	closed := "allow"
	if closeSends {
		closed = "EPERM"
	}
	sends := []struct {
		claim string
		nr    uint32
		addr  uint64
		want  string
	}{
		{"connect", a.sends["connect"], 0, closed},
		{"sendmsg", a.sends["sendmsg"], 0, closed},
		{"sendmmsg", a.sends["sendmmsg"], 0, closed},
		{"sendto with no address passes", a.sendto, 0, "allow"},
		{"sendto with an address", a.sendto, 0x00007ffd_e0001000, closed},
		{"sendto with an address in its low half alone", a.sendto, 0x1000, closed},
		{"sendto with an address in its high half alone", a.sendto, 0x00007ffd_00000000, closed},
		{"another call with sendto's fifth argument passes", 0, 0x1000, "allow"},
	}
	for _, r := range sends {
		if got := verdict(runFilter(t, prog, r.nr, a.audit, afUnix, 0, 0, 0, r.addr)); got != r.want {
			t.Errorf("%s: %s: %s, want %s", arch, r.claim, got, r.want)
		}
	}
	if len(rows) == 0 || len(ioctls) == 0 || len(sends) == 0 {
		t.Fatalf("%s: no rows ran", arch)
	}
}
