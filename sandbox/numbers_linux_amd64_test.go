//go:build linux && amd64

package sandbox

import (
	"syscall"
	"testing"
)

// TestTheAmd64FilterHoldsTheNumbersSyscallNames: the filter's numbers are its
// own table's, so each one package syscall names on amd64 is held to it here.
// syscall does not name seccomp, process_vm_writev, sendmmsg, the audit
// values or the calls from 424 on for amd64; the kernel proof in cmd/netblock holds those,
// since a wrong one lets its row through, or the filter not load.
//
// What it costs when this fails: a wrong number in the table lets a call through or refuses another, and
// only the kernel proof, on this architecture, says so.
func TestTheAmd64FilterHoldsTheNumbersSyscallNames(t *testing.T) {
	t.Parallel()
	a := filterArches()["amd64"]
	n := 0
	for _, c := range []struct {
		name      string
		got, want uint32
	}{
		{"socket", a.socket, syscall.SYS_SOCKET},
		{"socketpair", a.socketpair, syscall.SYS_SOCKETPAIR},
		{"ioctl", a.ioctl, syscall.SYS_IOCTL},
		{"TIOCSTI", tiocsti, syscall.TIOCSTI},
		{"TIOCLINUX", tioclinux, syscall.TIOCLINUX},
		{"connect", a.sends["connect"], syscall.SYS_CONNECT},
		{"sendto", a.sendto, syscall.SYS_SENDTO},
		{"sendmsg", a.sends["sendmsg"], syscall.SYS_SENDMSG},
		{"ptrace", a.refused["ptrace"], syscall.SYS_PTRACE},
		{"EPERM", errnoEPERM, uint32(syscall.EPERM)},
		{"AF_UNIX", afUnix, syscall.AF_UNIX},
	} {
		if c.got != c.want {
			t.Errorf("amd64 %s is %d, want syscall's %d", c.name, c.got, c.want)
		}
		n++
	}
	if n == 0 {
		t.Fatal("no number was checked")
	}
}
