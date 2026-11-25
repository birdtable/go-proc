package sandbox

import (
	"errors"
	"fmt"
	"math"
	"syscall"
)

// sockFilter is one classic BPF instruction, laid out as the kernel's
// struct sock_filter.
type sockFilter struct {
	Code uint16
	Jt   uint8
	Jf   uint8
	K    uint32
}

// The classic BPF opcodes the filter uses: a 32-bit load from the seccomp
// data, a jump on equal or on greater or equal to a constant, a return.
const (
	bpfLdWAbs = 0x20 // BPF_LD | BPF_W | BPF_ABS
	bpfJeqK   = 0x15 // BPF_JMP | BPF_JEQ | BPF_K
	bpfJgeK   = 0x35 // BPF_JMP | BPF_JGE | BPF_K
	bpfRetK   = 0x06 // BPF_RET | BPF_K
)

// The offsets in struct seccomp_data: the call's number, the architecture's
// audit value, the low halves of the first and second arguments, and both
// halves of the fifth, on a little-endian machine.
const (
	seccompNr       = 0
	seccompArch     = 4
	seccompArg0Low  = 16
	seccompArg1Low  = 24
	seccompArg4Low  = 48
	seccompArg4High = 52
	seccompRetAllow = 0x7fff0000
	seccompRetKill  = 0x80000000 // SECCOMP_RET_KILL_PROCESS
	seccompRetErrno = 0x00050000
	errnoEPERM      = 1
	afUnix          = 1
	x32SyscallBit   = 0x40000000
)

// The seccomp call that loads the filter, of linux/seccomp.h: the filter mode, and its flags.
// TSYNC sets the filter on every thread of the process, or on none. Without TSYNC_ESRCH a thread
// it cannot synchronize, one that holds a filter of its own this thread's is not an ancestor of,
// fails the call with that thread's id as its result and no errno; with it, the call fails with
// ESRCH (Linux 5.7, below the block's 6.12).
const (
	seccompSetModeFilter        = 1
	seccompFilterFlagTsync      = 1
	seccompFilterFlagTsyncESRCH = 1 << 4
	seccompFilterFlags          = seccompFilterFlagTsync | seccompFilterFlagTsyncESRCH
)

// errFilterUnsynced is a seccomp load that answered with a thread's id: the kernel attached the
// filter to no thread.
var errFilterUnsynced = errors.New("the kernel set the filter on no thread, since one thread of this process could not be synchronized to it")

// filterLoaded is how the seccomp load went, from its first result r1 and its errno: an errno, or
// any result but 0, is a filter not set. TSYNC_ESRCH makes the thread's id an errno, and the
// result is read all the same, so a kernel that answers either way is a refusal, never a pass.
func filterLoaded(r1 uintptr, errno syscall.Errno) error {
	switch {
	case errno != 0:
		return errno
	case r1 != 0:
		return fmt.Errorf("%w: the call answered %d", errFilterUnsynced, r1)
	}
	return nil
}

// The ioctl requests the filter refuses, of asm-generic/ioctls.h: TIOCSTI
// pushes a byte into a terminal's input, so a shell outside the block that
// shares the terminal runs a line the program wrote, and TIOCLINUX does the
// same through a virtual console's selection. A request is an unsigned int,
// so its low half is all of it.
const (
	tiocsti   = 0x5412
	tioclinux = 0x541c
)

// The calls the filter refuses whatever their arguments, by the names the
// kernel gives them.
const (
	callIoUringSetup    = "io_uring_setup"
	callPidfdGetfd      = "pidfd_getfd"
	callPtrace          = "ptrace"
	callProcessVMWritev = "process_vm_writev"
)

// The calls that reach a socket by its address, refused whatever their
// arguments when the filter closes Unix sockets on a path, by the names the
// kernel gives them.
const (
	callConnect  = "connect"
	callSendmsg  = "sendmsg"
	callSendmmsg = "sendmmsg"
)

// The labels the filter's jumps name: refuse with EPERM, judge a socket's
// family, judge an ioctl's request, judge whether sendto names an address,
// and kill.
const (
	labelDeny   = "deny"
	labelFamily = "family"
	labelIoctl  = "ioctl"
	labelSendto = "sendto"
	labelKill   = "kill"
)

// filterArch is the calling convention of one architecture the filter holds:
// its audit value, whether it has the x32 calls, and its numbers for the
// calls the filter judges.
type filterArch struct {
	audit uint32
	x32   bool
	// seccomp is the call that loads the filter.
	seccomp uint32
	// socket and socketpair are allowed for AF_UNIX alone.
	socket, socketpair uint32
	// ioctl is allowed but for the requests tiocsti and tioclinux.
	ioctl uint32
	// sends are the calls that reach a socket by its address, refused when
	// the kernel cannot keep a Unix socket on a path outside the block from
	// answering: connect, sendmsg and sendmmsg whatever their arguments, and
	// sendto when it names an address.
	sends map[string]uint32
	// sendto is refused with an address, in that mode, and allowed without
	// one, on a socket already connected.
	sendto uint32
	// refused are refused whatever their arguments.
	refused map[string]uint32
}

// filterArches are the architectures the block runs on in Linux, by GOARCH,
// with the numbers of their kernel's unistd tables.
func filterArches() map[string]filterArch {
	return map[string]filterArch{
		"amd64": {
			audit: 0xc000003e, x32: true, seccomp: 317, socket: 41, socketpair: 53, ioctl: 16,
			refused: map[string]uint32{callIoUringSetup: 425, callPidfdGetfd: 438, callPtrace: 101, callProcessVMWritev: 311},
			sends:   map[string]uint32{callConnect: 42, callSendmsg: 46, callSendmmsg: 307},
			sendto:  44,
		},
		"arm64": {
			audit: 0xc00000b7, seccomp: 277, socket: 198, socketpair: 199, ioctl: 29,
			refused: map[string]uint32{callIoUringSetup: 425, callPidfdGetfd: 438, callPtrace: 117, callProcessVMWritev: 271},
			sends:   map[string]uint32{callConnect: 203, callSendmsg: 211, callSendmmsg: 269},
			sendto:  206,
		},
	}
}

// insn is an instruction whose jumps name labels, before they are resolved.
type insn struct {
	code   uint16
	k      uint32
	jt, jf string
	label  string
}

// program is the filter for a: the architecture first, killing on any other,
// then the refused calls, then socket and socket pair held to AF_UNIX, then
// ioctl refused for the two requests that write into a terminal, and
// everything else allowed. With closeSends, the calls that reach a socket by
// its address are refused too: connect, sendmsg and sendmmsg, and sendto when
// its address is not NULL.
func (a filterArch) program(closeSends bool) ([]sockFilter, error) {
	ins := []insn{
		{code: bpfLdWAbs, k: seccompArch},
		{code: bpfJeqK, k: a.audit, jf: labelKill},
		{code: bpfLdWAbs, k: seccompNr},
	}
	if a.x32 {
		ins = append(ins, insn{code: bpfJgeK, k: x32SyscallBit, jt: labelDeny})
	}
	// In one order, so the program is the same on every build.
	for _, name := range []string{callIoUringSetup, callPidfdGetfd, callPtrace, callProcessVMWritev} {
		ins = append(ins, insn{code: bpfJeqK, k: a.refused[name], jt: labelDeny})
	}
	if closeSends {
		for _, name := range []string{callConnect, callSendmsg, callSendmmsg} {
			ins = append(ins, insn{code: bpfJeqK, k: a.sends[name], jt: labelDeny})
		}
		ins = append(ins, insn{code: bpfJeqK, k: a.sendto, jt: labelSendto})
	}
	ins = append(ins,
		insn{code: bpfJeqK, k: a.socket, jt: labelFamily},
		insn{code: bpfJeqK, k: a.socketpair, jt: labelFamily},
		insn{code: bpfJeqK, k: a.ioctl, jt: labelIoctl},
		insn{code: bpfRetK, k: seccompRetAllow},
		insn{code: bpfLdWAbs, k: seccompArg0Low, label: labelFamily},
		insn{code: bpfJeqK, k: afUnix, jf: labelDeny},
		insn{code: bpfRetK, k: seccompRetAllow},
		insn{code: bpfLdWAbs, k: seccompArg1Low, label: labelIoctl},
		insn{code: bpfJeqK, k: tiocsti, jt: labelDeny},
		insn{code: bpfJeqK, k: tioclinux, jt: labelDeny},
		insn{code: bpfRetK, k: seccompRetAllow},
	)
	if closeSends {
		// A pointer is NULL when both of its halves are.
		ins = append(ins,
			insn{code: bpfLdWAbs, k: seccompArg4Low, label: labelSendto},
			insn{code: bpfJeqK, k: 0, jf: labelDeny},
			insn{code: bpfLdWAbs, k: seccompArg4High},
			insn{code: bpfJeqK, k: 0, jf: labelDeny},
			insn{code: bpfRetK, k: seccompRetAllow},
		)
	}
	ins = append(ins,
		insn{code: bpfRetK, k: seccompRetErrno | errnoEPERM, label: labelDeny},
		insn{code: bpfRetK, k: seccompRetKill, label: labelKill},
	)
	return assemble(ins)
}

// assemble resolves each jump's label to the forward offset classic BPF
// counts from the next instruction. An empty label is the next instruction.
// A label that is missing or behind the jump, or too far, is a fault of this
// file, and the block refuses to run: TestTheFilterIsWellFormed holds every
// program.
func assemble(ins []insn) ([]sockFilter, error) {
	at := map[string]int{}
	for i, in := range ins {
		if in.label != "" {
			at[in.label] = i
		}
	}
	var bad error
	to := func(i int, label string) uint8 {
		if label == "" {
			return 0
		}
		// The offset is what a jump field holds: one byte, counted from the
		// instruction after the jump, so ahead of it and under 256.
		j, ok := at[label]
		off := j - i - 1
		if !ok || off < 0 || off > math.MaxUint8 {
			bad = fmt.Errorf("the seccomp filter jumps from %d to %q, which is not ahead of it", i, label)
			return 0
		}
		return uint8(off)
	}
	out := make([]sockFilter, len(ins))
	for i, in := range ins {
		out[i] = sockFilter{Code: in.code, K: in.k, Jt: to(i, in.jt), Jf: to(i, in.jf)}
	}
	if bad != nil {
		return nil, bad
	}
	return out, nil
}
