//go:build windows

package proc

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows" //nolint:depguard // arch.ports.23: the tests put what they start in a Job Object, by the calls Run makes its own with
)

// testsJob is the Job Object this test binary holds for the processes its tests start themselves,
// made the first time one is started: set to kill every process in it when its last handle closes.
// That handle is this process's own and is never closed here, so Windows closes it however the
// binary ends, by TerminateProcess too, and the job ends what the tests started, and their children
// with it.
var testsJob = sync.OnceValues(func() (windows.Handle, error) { //nolint:gochecknoglobals // one job for the test binary's life, made once and closed by the process's own end
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("making the tests' job: %w", err)
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if err := setJob(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil { //nolint:gosec // G103: the job call takes the struct Windows defines, by its address and size
		return 0, errors.Join(fmt.Errorf("setting the tests' job to kill on close: %w", err), windows.CloseHandle(job))
	}
	return job, nil
})

// startTestProcess starts c, a process a test starts itself, and is the one way the tests start
// one on Windows. In the test binary proper it goes through startInTestsJob. In a helper, the test
// binary run as a child program, it starts c plainly: the helper is already in the job of the
// process that started it, a run's job or the tests' job, and c inherits that job, so a killed test
// binary still takes it. A job of the helper's own would end c as soon as the helper exits, and a
// test that wants a run's job to end what its helper left would pass without seeing it.
func startTestProcess(c *process) error {
	if inHelper() {
		return c.Start()
	}
	return startInTestsJob(c)
}

// startInTestsJob starts c suspended, puts it in the tests' job, and only then resumes it, as Run
// does with a run's own job: c runs no instruction outside the job. A c that cannot be put in the
// job, or resumed, is killed and reaped before the error is given. Windows 8 and later nest jobs,
// so a run c makes inside it makes its own job, nested in this one.
func startInTestsJob(c *process) error {
	job, err := testsJob()
	if err != nil {
		return err
	}
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	c.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	if err := c.Start(); err != nil {
		return err
	}
	var held error
	if err := c.Process.WithHandle(func(h uintptr) {
		held = windows.AssignProcessToJobObject(job, windows.Handle(h))
	}); err != nil {
		held = err
	}
	if held == nil {
		held = resume(uint32(c.Process.Pid))
	}
	if held != nil {
		killed := c.Process.Kill()
		_ = c.Wait() // reaped; its exit is the kill's
		return errors.Join(fmt.Errorf("putting the test's process %d in the tests' job: %w", c.Process.Pid, held), killed)
	}
	return nil
}

// combinedOutput is c's CombinedOutput, with c started by startTestProcess. One buffer takes both
// streams, as CombinedOutput's does: os/exec writes to a Stdout and Stderr that are the same writer
// from one goroutine at a time.
func combinedOutput(c *process) ([]byte, error) {
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	if err := startTestProcess(c); err != nil {
		return nil, err
	}
	err := c.Wait()
	return out.Bytes(), err
}
