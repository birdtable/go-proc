//go:build windows

package proc

import (
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/windows" //nolint:depguard // arch.ports.23: a pipe's name is read by a call of kernel32 alone
)

// pipeNameWithin is how long a run without Terminal waits for its Stdin's pipe to give its name.
// Windows answers at once, but not while another thread's synchronous Read is pending on the same
// handle: the query waits behind it for as long as the Read does, which may be for ever, and no call
// frees it before then (CancelSynchronousIo does not, on Windows 10.0.26200).
const pipeNameWithin = time.Second

// errPipeNameUnanswered is a Stdin pipe whose name did not come within pipeNameWithin.
var errPipeNameUnanswered = fmt.Errorf("%w: Terminal is not set and Stdin is a pipe whose name query timed out after %v, as it does behind a Read pending on the pipe, so the pipe cannot be told from an MSYS or Cygwin pty's, where a program that finds no terminal would ask this process's user", ErrRefused, pipeNameWithin)

// nameAnswer is the name query's answer.
type nameAnswer struct {
	info fileNameInfo
	err  error
}

// pipeName is the name of pipe h, as FileNameInfo gives it, asked on a goroutine of its own and
// waited for up to within. One not given by then is errPipeNameUnanswered, given at once; the query
// is left to end when Windows answers it, as it does once the Read pending on h ends, so one thread
// is held for as long as that Read is.
func pipeName(h windows.Handle, within time.Duration) (fileNameInfo, error) {
	info, _, err := askPipeName(h, within)
	return info, err
}

// askPipeName is pipeName with a channel closed once the query's goroutine has ended: by the time
// it returns when the name came within the bound, else once Windows answers the query. The answer
// goes to a buffered channel, so a query answered after the caller has gone ends, and does not wait
// for a receive that never comes.
func askPipeName(h windows.Handle, within time.Duration) (fileNameInfo, <-chan struct{}, error) {
	answer := make(chan nameAnswer, 1)
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		var a nameAnswer
		a.err = windows.GetFileInformationByHandleEx(h, windows.FileNameInfo,
			(*byte)(unsafe.Pointer(&a.info)), uint32(unsafe.Sizeof(a.info))) //nolint:gosec // G103: the call fills the struct Windows defines, by its address and size
		answer <- a
	}()
	select {
	case a := <-answer:
		return a.info, ended, a.err
	case <-time.After(within):
		return fileNameInfo{}, ended, errPipeNameUnanswered
	}
}
