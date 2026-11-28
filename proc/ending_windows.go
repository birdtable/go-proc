//go:build windows

package proc

import (
	"os"
	"syscall"
)

// ntError is the least NTSTATUS error value, severity 3 of ntdef.h.
const ntError = 0xC0000000

// endOf is how the reaped leader ended, as its exit code says: with an NTSTATUS error value, or by
// its exit.
func endOf(state *os.ProcessState) (EndKind, os.Signal, uint32) {
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.ExitCode >= ntError {
		return EndAbnormal, nil, ws.ExitCode
	}
	return EndExited, nil, 0
}
