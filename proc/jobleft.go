package proc

import (
	"errors"
)

// maxWaitObjects is MAXIMUM_WAIT_OBJECTS of winnt.h: the most handles WaitForMultipleObjects takes.
const maxWaitObjects = 64

// errNoConsoleHost is a process of the job whose image is not the system's console host.
var errNoConsoleHost = errors.New("proc: the process is not the system's console host")

// errGone is a process that ended between the job's listing and its opening.
var errGone = errors.New("proc: the process has ended")

// jobView is what jobLeft reads of a Windows job, by calls a test of the package stands in for, H
// being a handle to a process. Each read is its own instant: a process the count saw may be gone by
// the listing, the snapshot of parents or the opening, which ends no wait and counts as nothing,
// as the count read again after the wait decides.
type jobView[H any] struct {
	// live is the job's count of active processes.
	live func() (int, error)
	// listed is the pids of the job's processes.
	listed func() ([]int, error)
	// parents is the parent of each process of the system, by its pid, as a snapshot gives it.
	parents func() (map[int]int, error)
	// host opens process pid for its end when its image is the system's console host, by full path;
	// errNoConsoleHost when it is another, errGone when no process has the pid or it has ended. A
	// process that has begun to exit, whose image cannot be read then, is opened for its end too.
	host func(pid int) (H, error)
	// wait waits for each of hs to end, up to consoleHostWithin; a wait that runs out is no error.
	wait func(hs []H) error
	// release closes each of hs.
	release func(hs []H) error
}

// left is whether the job holds a live process once its leader, pid leader, has been waited for:
// its count of active processes is above zero. A console host Windows started for the tree, its
// image conhost.exe of the system directory by full path and its parent the leader or a process of
// the job, ends only once the last process on its console has, which may be after the leader's wait
// returned. When such hosts alone are left, they are waited for, up to consoleHostWithin, and the
// count is read again; anything in the job then, a host included, is left. A count that cannot be
// read is a live process, as is a job one of whose processes is another or cannot be named, unless
// it has begun to exit, when it is waited for as a host is.
func (j jobView[H]) left(leader int) bool {
	n, err := j.live()
	if err != nil {
		return true
	}
	if n == 0 {
		return false
	}
	hosts, err := j.hostsAlone(leader)
	if err != nil {
		return true
	}
	if len(hosts) > 0 {
		err = errors.Join(j.wait(hosts), j.release(hosts))
	}
	n, errAgain := j.live()
	return errors.Join(err, errAgain) != nil || n > 0
}

// hostsAlone opens each process of the job, for its end, when every one is a console host Windows
// started for the tree: none more than WaitForMultipleObjects takes, its parent the leader or a
// process of the job, and its image conhost.exe of the system directory by full path. A name alone
// is not enough, as a program may give its worker that name. One that is not, or cannot be read, is
// an error, and nothing is held open. The leader, which has been waited for, and a process gone
// since the listing, are not waited for: the count read again sees either if it is still live.
func (j jobView[H]) hostsAlone(leader int) ([]H, error) {
	pids, err := j.listed()
	if err != nil {
		return nil, err
	}
	if len(pids) > maxWaitObjects {
		return nil, errNoConsoleHost
	}
	if len(pids) == 0 {
		return nil, nil
	}
	parents, err := j.parents()
	if err != nil {
		return nil, err
	}
	inJob := map[int]bool{leader: true}
	for _, pid := range pids {
		inJob[pid] = true
	}
	var hosts []H
	for _, pid := range pids {
		ppid, ok := parents[pid]
		if pid == leader || !ok {
			continue
		}
		if ppid == pid || !inJob[ppid] {
			return nil, errors.Join(errNoConsoleHost, j.release(hosts))
		}
		h, err := j.host(pid)
		if errors.Is(err, errGone) {
			continue
		}
		if err != nil {
			return nil, errors.Join(err, j.release(hosts))
		}
		hosts = append(hosts, h)
	}
	return hosts, nil
}
