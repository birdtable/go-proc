//go:build linux && arm64

package sandbox

import (
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// TestTheArm64FilterHoldsTheNumbersSyscallNames: the filter's numbers are its
// own table's, so each one package syscall names on arm64 is held to it here.
// syscall does not name the audit values or the calls from 424 on; the
// kernel proof in cmd/netblock holds those, since a wrong one lets its row
// through, or kills the program.
//
// What it costs when this fails: a wrong number in the table lets a call through or refuses another, and
// only the kernel proof, on this architecture, says so.
func TestTheArm64FilterHoldsTheNumbersSyscallNames(t *testing.T) {
	t.Parallel()
	a := filterArches()["arm64"]
	n := 0
	for _, c := range []struct {
		name      string
		got, want uint32
	}{
		{"seccomp", a.seccomp, syscall.SYS_SECCOMP},
		{"socket", a.socket, syscall.SYS_SOCKET},
		{"socketpair", a.socketpair, syscall.SYS_SOCKETPAIR},
		{"ioctl", a.ioctl, syscall.SYS_IOCTL},
		{"TIOCSTI", tiocsti, syscall.TIOCSTI},
		{"TIOCLINUX", tioclinux, syscall.TIOCLINUX},
		{"connect", a.sends["connect"], syscall.SYS_CONNECT},
		{"sendto", a.sendto, syscall.SYS_SENDTO},
		{"sendmsg", a.sends["sendmsg"], syscall.SYS_SENDMSG},
		{"sendmmsg", a.sends["sendmmsg"], syscall.SYS_SENDMMSG},
		{"ptrace", a.refused["ptrace"], syscall.SYS_PTRACE},
		{"process_vm_writev", a.refused["process_vm_writev"], syscall.SYS_PROCESS_VM_WRITEV},
		{"EPERM", errnoEPERM, uint32(syscall.EPERM)},
		{"AF_UNIX", afUnix, syscall.AF_UNIX},
		{"PR_SET_NO_NEW_PRIVS", unix.PR_SET_NO_NEW_PRIVS, syscall.PR_SET_NO_NEW_PRIVS},
	} {
		if c.got != c.want {
			t.Errorf("arm64 %s is %d, want syscall's %d", c.name, c.got, c.want)
		}
		n++
	}
	if n == 0 {
		t.Fatal("no number was checked")
	}
}
