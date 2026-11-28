//go:build windows

package proc

import (
	"strconv"
	"testing"

	"golang.org/x/sys/windows" //nolint:depguard // arch.ports.23: a process is ended by TerminateProcess on a handle, which the calls of x/sys alone open
)

// TestOnWindowsAnNTStatusExitReadsEndAbnormalWithItsValue: a helper that exits with 0xC0000005, the
// code an access violation ends a program with, reads EndAbnormal with that Status, no Signal, its
// Code the same 32 bits, and no error. One that exits with 0xBFFFFFFF, the largest code below the
// NTSTATUS error values, reads EndExited with no Status.
//
// What it costs when this fails: a caller cannot tell a program that crashed from one that exited
// with a large code.
func TestOnWindowsAnNTStatusExitReadsEndAbnormalWithItsValue(t *testing.T) {
	t.Parallel()
	rows := []struct {
		code   uint32
		end    EndKind
		status uint32
	}{{0xC0000005, EndAbnormal, 0xC0000005}, {ntError - 1, EndExited, 0}}
	for _, r := range rows {
		res, err := Run(t.Context(), helperCmd(t, "exit-code", strconv.FormatUint(uint64(r.code), 10)))
		if err != nil || uint32(res.Code) != r.code || res.End != r.end || res.Signal != nil || res.Status != r.status {
			t.Errorf("a program that exits %#x: %v, Code %#x, End %q, Signal %v, Status %#x, want no error, Code %#x, %q and Status %#x", r.code, err, uint32(res.Code), res.End, res.Signal, res.Status, r.code, r.end, r.status)
		}
	}
}

// TestOnWindowsTerminateProcessWithCodeOneReadsAsAnExitWithCodeOne: a helper that writes its pid and
// holds, ended by the test's TerminateProcess with code 1, reads EndExited with Code 1, no Status and
// no error, as End's doc says: Windows keeps no mark of who chose the code.
//
// What it costs when this fails: End claims to tell apart what no library can, and the doc's limit is
// wrong.
func TestOnWindowsTerminateProcessWithCodeOneReadsAsAnExitWithCodeOne(t *testing.T) {
	t.Parallel()
	res, err, ended := onPid(t, helperCmd(t, "pid-hold"), func(pid int) error {
		h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
		if err != nil {
			return err
		}
		defer windows.CloseHandle(h)
		return windows.TerminateProcess(h, 1)
	})
	if ended != nil || err != nil || res.Code != 1 || res.End != EndExited || res.Signal != nil || res.Status != 0 {
		t.Errorf("a program ended by TerminateProcess with code 1 (%v): %v, Code %d, End %q, Signal %v, Status %#x, want no error, Code 1 and EndExited alone", ended, err, res.Code, res.End, res.Signal, res.Status)
	}
}
