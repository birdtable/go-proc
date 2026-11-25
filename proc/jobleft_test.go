package proc

import (
	"errors"
	"slices"
	"testing"
)

// fakeJob is a Windows job as jobView reads it, one read at a time: counts are its counts of
// active processes, in order, and the rest as each read answers. Handles are pids.
type fakeJob struct {
	counts   []int
	countErr error
	pids     []int
	listErr  error
	parents  map[int]int
	snapErr  error
	// images is each pid's image: host for the console host, gone for a pid no process has.
	images  map[int]string
	waitErr error
	waited  [][]int
	opened  []int
	closed  []int
}

const (
	fakeLeader = 100
	fakeCaller = 1
)

func (f *fakeJob) view() jobView[int] {
	return jobView[int]{
		live: func() (int, error) {
			if f.countErr != nil {
				return 0, f.countErr
			}
			n := f.counts[0]
			if len(f.counts) > 1 {
				f.counts = f.counts[1:]
			}
			return n, nil
		},
		listed:  func() ([]int, error) { return f.pids, f.listErr },
		parents: func() (map[int]int, error) { return f.parents, f.snapErr },
		host: func(pid int) (int, error) {
			switch f.images[pid] {
			case "host":
				f.opened = append(f.opened, pid)
				return pid, nil
			case "gone":
				return 0, errGone
			default:
				return 0, errNoConsoleHost
			}
		},
		wait: func(hs []int) error {
			f.waited = append(f.waited, slices.Clone(hs))
			return f.waitErr
		},
		release: func(hs []int) error {
			f.closed = append(f.closed, hs...)
			return nil
		},
	}
}

// TestAJobsLeftReadsEachProcessAsItIsThen: jobView.left over a job whose reads are each their own
// instant. A count of one, then a listing in which the console host has already ended, or a host
// listed that the snapshot of parents no longer has, or that is gone by its opening, or the leader
// itself, waited for and listed still, each read the count again, and Left is what it says: false
// once the job is empty. A host that is there is waited for, and none is waited for with no handle.
// And the guards: a process of the job that is not the console host, or whose parent is outside
// the job, or a count, listing, snapshot or wait that fails, or more processes than one wait takes,
// or a count still above zero after the wait, each read Left true.
//
// What it costs when this fails: under load, where a host ends between two reads, a program run
// without Terminal that left nothing running reads Left true, so a caller that fails closed refuses it;
// or, for a guard, a program whose worker runs on reads Left false.
func TestAJobsLeftReadsEachProcessAsItIsThen(t *testing.T) {
	t.Parallel()
	const host, other, gone = 200, 300, 400
	hostOf := map[int]int{host: fakeLeader}
	images := map[int]string{host: "host", gone: "gone", other: "worker", fakeLeader: "program"}
	for _, row := range []struct {
		name string
		job  fakeJob
		want bool
		// waits is the handles each wait was given.
		waits [][]int
	}{
		{"a job empty at its first count", fakeJob{counts: []int{0}}, false, nil},
		{"a host that ends before the listing", fakeJob{counts: []int{1, 0}, pids: nil}, false, nil},
		{"a host that ends before the snapshot", fakeJob{counts: []int{1, 0}, pids: []int{host}, parents: map[int]int{}, images: images}, false, nil},
		{"a host that ends before its opening", fakeJob{counts: []int{1, 0}, pids: []int{gone}, parents: map[int]int{gone: fakeLeader}, images: images}, false, nil},
		{"the leader listed still", fakeJob{counts: []int{2, 0}, pids: []int{fakeLeader, host}, parents: map[int]int{fakeLeader: fakeCaller, host: fakeLeader}, images: images}, false, [][]int{{host}}},
		{"a host that ends within the wait", fakeJob{counts: []int{1, 0}, pids: []int{host}, parents: hostOf, images: images}, false, [][]int{{host}}},
		{"a host still there after the wait", fakeJob{counts: []int{1, 1}, pids: []int{host}, parents: hostOf, images: images}, true, [][]int{{host}}},
		{"a host gone and one still there", fakeJob{counts: []int{2, 1}, pids: []int{gone, host}, parents: map[int]int{gone: fakeLeader, host: fakeLeader}, images: images}, true, [][]int{{host}}},
		{"a worker of the program", fakeJob{counts: []int{2, 0}, pids: []int{host, other}, parents: map[int]int{host: fakeLeader, other: fakeLeader}, images: images}, true, nil},
		{"a host whose parent is outside the job", fakeJob{counts: []int{1, 0}, pids: []int{host}, parents: map[int]int{host: fakeCaller}, images: images}, true, nil},
		{"a count that fails", fakeJob{countErr: errors.New("no count")}, true, nil},
		{"a listing that fails", fakeJob{counts: []int{1, 0}, listErr: errors.New("no listing")}, true, nil},
		{"a snapshot that fails", fakeJob{counts: []int{1, 0}, pids: []int{host}, snapErr: errors.New("no snapshot")}, true, nil},
		{"a wait that fails", fakeJob{counts: []int{1, 0}, pids: []int{host}, parents: hostOf, images: images, waitErr: errors.New("no wait")}, true, [][]int{{host}}},
		{"more processes than one wait takes", fakeJob{counts: []int{maxWaitObjects + 1, 0}, pids: make([]int, maxWaitObjects+1)}, true, nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			job := row.job
			if got := job.view().left(fakeLeader); got != row.want {
				t.Errorf("Left %v, want %v", got, row.want)
			}
			if !slices.EqualFunc(job.waited, row.waits, slices.Equal) {
				t.Errorf("waited for %v, want %v", job.waited, row.waits)
			}
			slices.Sort(job.opened)
			slices.Sort(job.closed)
			if !slices.Equal(job.opened, job.closed) {
				t.Errorf("opened %v, closed %v, want each opened closed", job.opened, job.closed)
			}
		})
	}
}
