//go:build windows

package proc

import (
	"errors"
	"fmt"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/windows" //nolint:depguard // arch.ports.23: a Job Object's limits and its completion port are in x/sys alone
)

// limitsHere is which limits this system holds: both, on the tree's Job Object.
const limitsHere = limJob

// The messages a Job Object posts to its completion port when its tree passes a limit, as winnt.h
// names them; x/sys does not.
const (
	jobMsgProcesses = 3  // JOB_OBJECT_MSG_ACTIVE_PROCESS_LIMIT
	jobMsgMemory    = 10 // JOB_OBJECT_MSG_JOB_MEMORY_LIMIT
)

// The completion keys on a watch's port: the job's messages, and the watch's own stop.
const (
	jobKey  = 1
	stopKey = 2
)

// jobPort is JOBOBJECT_ASSOCIATE_COMPLETION_PORT, which x/sys does not define: the key the job's
// messages carry, and the port they go to.
type jobPort struct {
	key  uintptr
	port windows.Handle
}

// limit sets l on info, the job's limits.
func limit(info *windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION, l Limits) {
	if l.TreeMemory > 0 {
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_JOB_MEMORY
		info.JobMemoryLimit = uintptr(l.TreeMemory)
	}
	if l.Processes > 0 {
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS
		info.BasicLimitInformation.ActiveProcessLimit = uint32(l.Processes) //nolint:gosec // G115: jobRefused refused a Processes wider than a DWORD
	}
}

// A jobWatch reads a job's completion port and calls hit, and keeps that it did, each time the job
// says its tree passed a limit: a process that would pass Processes did not start, or an allocation
// that would pass TreeMemory failed. Windows posts each such message before the call that failed returns
// to the process that made it, and a port gives its messages in order, so once the watch reads its
// stop every message of the run is read. Windows says the delivery of these messages is not
// guaranteed: a message that does not come leaves the program to answer the failed call itself, and
// the run to end as the program ends it.
type jobWatch struct {
	port windows.Handle
	hit  func()
	// passed is set once a message said the tree passed a limit.
	passed atomic.Bool
	done   chan struct{}
	err    error
}

// watch makes a completion port for job, its messages to go to it, and starts reading it.
func watch(job windows.Handle, hit func()) (*jobWatch, error) {
	port, err := windows.CreateIoCompletionPort(windows.InvalidHandle, 0, 0, 1)
	if err != nil {
		return nil, fmt.Errorf("proc: making the port the child's job says its limits on: %w", err)
	}
	assoc := jobPort{key: jobKey, port: port}
	if err := setJob(job, windows.JobObjectAssociateCompletionPortInformation,
		uintptr(unsafe.Pointer(&assoc)), uint32(unsafe.Sizeof(assoc)), nil); err != nil { //nolint:gosec // G103: the job call takes the struct Windows defines, by its address and size
		return nil, errors.Join(fmt.Errorf("proc: setting the child's job to say its limits on a port: %w", err), windows.CloseHandle(port))
	}
	w := &jobWatch{port: port, hit: hit, done: make(chan struct{})}
	go w.read()
	return w, nil
}

// read reads the port until its stop.
func (w *jobWatch) read() {
	defer close(w.done)
	for {
		var msg uint32
		var key uintptr
		var ov *windows.Overlapped
		if err := windows.GetQueuedCompletionStatus(w.port, &msg, &key, &ov, windows.INFINITE); err != nil {
			w.err = fmt.Errorf("proc: reading the port the child's job says its limits on: %w", err)
			return
		}
		switch {
		case key == stopKey:
			return
		case key == jobKey && (msg == jobMsgProcesses || msg == jobMsgMemory):
			w.passed.Store(true)
			w.hit()
		}
	}
}

// stop reads the port to the end of what the job posted, and closes it. A nil watch, of a job held
// to no limit, does nothing.
func (w *jobWatch) stop() error {
	if w == nil {
		return nil
	}
	if err := windows.PostQueuedCompletionStatus(w.port, 0, stopKey, nil); err != nil {
		// With no stop to read, the reader is ended by the port's close.
		return errors.Join(fmt.Errorf("proc: stopping the read of the child's job's port: %w", err), windows.CloseHandle(w.port), w.wait())
	}
	return errors.Join(w.wait(), windows.CloseHandle(w.port))
}

// wait waits for the reader to end, and is how it ended.
func (w *jobWatch) wait() error {
	<-w.done
	return w.err
}

// limited is whether the job said its tree passed a limit; it is read once the watch stopped.
func (w *jobWatch) limited() bool { return w != nil && w.passed.Load() }
