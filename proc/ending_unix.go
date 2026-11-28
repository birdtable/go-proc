//go:build unix

package proc

import (
	"os"
	"syscall"
)

// endOf is how the reaped leader ended, as its wait status says: by a signal, or by its exit.
func endOf(state *os.ProcessState) (EndKind, os.Signal, uint32) {
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return EndSignal, ws.Signal(), 0
	}
	return EndExited, nil, 0
}
