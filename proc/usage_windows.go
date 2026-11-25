//go:build windows

package proc

import (
	"errors"
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/windows" //nolint:depguard // arch.ports.23: the Job Object calls are in x/sys alone
)

// pidRoom is how many more ids than the job said it holds a read of its processes makes room for,
// so a process or two that starts between the reads does not need a third.
const pidRoom = 8

// pidReads is how many times Pids reads a job whose processes outgrow each read's room, before it
// says so.
const pidReads = 4

// errPidsGrew is a job whose processes outgrew the room of every read Pids made.
var errPidsGrew = errors.New("proc: the tree's processes grew faster than they were read")

// usageOf reads the job id names for its CPU time and its live processes.
func usageOf(id TreeID) (TreeUsage, error) {
	return inJob(id, jobQuery, "reading the usage of", accounted)
}

// accounted reads job's accounting: the user and kernel time of every process it held, in
// 100-nanosecond units, and its count of live processes.
func accounted(job windows.Handle) (TreeUsage, error) {
	var info jobAccounting
	if err := queryJob(job, windows.JobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil { //nolint:gosec // G103: the job call reads the struct Windows defines, by its address and size
		return TreeUsage{}, err
	}
	return TreeUsage{
		CPU:  time.Duration(info.TotalUserTime+info.TotalKernelTime) * 100,
		Live: int(info.ActiveProcesses),
	}, nil
}

// pidsOf reads the ids of the processes in the job id names.
func pidsOf(id TreeID) ([]int, error) { return inJob(id, jobQuery, "reading the processes of", listed) }

// listed reads the ids of job's processes into a buffer of uintptrs, the list's header first, then
// room for the ids. A buffer too small for them is ERROR_MORE_DATA, with the count the job holds in
// the header; the next read makes room for that count and pidRoom more, up to pidReads reads.
func listed(job windows.Handle) ([]int, error) {
	head := int(unsafe.Offsetof(jobPidList{}.ProcessIDList) / unsafe.Sizeof(uintptr(0)))
	room := pidRoom
	for range pidReads {
		buf := make([]uintptr, head+room)
		err := queryJob(job, windows.JobObjectBasicProcessIdList,
			uintptr(unsafe.Pointer(&buf[0])), uint32(len(buf))*uint32(unsafe.Sizeof(buf[0])), nil) //nolint:gosec // G103: the job call fills the list Windows defines, by its address and size; G115: a few ids, far below 2^32 bytes
		list := (*jobPidList)(unsafe.Pointer(&buf[0])) //nolint:gosec // G103: the buffer's head is the list's header, as Windows writes it
		if errors.Is(err, windows.ERROR_MORE_DATA) {
			room = int(list.NumberOfAssignedProcesses) + pidRoom
			continue
		}
		if err != nil {
			return nil, err
		}
		// The count is Windows' own, below the room it was given, and read no further than the room.
		ids := buf[head : head+min(int(list.NumberOfProcessIDsInList), room)]
		pids := make([]int, len(ids))
		for i, id := range ids {
			pids[i] = int(id)
		}
		return pids, nil
	}
	return nil, errPidsGrew
}

// joinTree puts this process in a job made for it under a name drawn at random, as Start makes one
// for a detached tree. The job does not end its processes when its handles close. This process
// keeps its handle of the job, never closed and not inherited, which keeps the name while this
// process lives, since Windows drops an object's name once its last handle closes.
func joinTree() (TreeID, error) {
	name := jobName()
	job, err := createJob(name)
	if err != nil {
		return "", err
	}
	if err := windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
		return "", errors.Join(fmt.Errorf("proc: putting this process in its tree's job: %w", err), windows.CloseHandle(job))
	}
	return TreeID(name), nil
}
