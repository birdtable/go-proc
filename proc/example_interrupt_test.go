package proc_test

import (
	"errors"
	"fmt"
	"os/exec"
	"time"

	"github.com/birdtable/go-proc/proc"
)

// Start a program in a tree that outlives this process, ask it to stop, and kill it once the
// caller's own wait is over. Interrupt sends nothing on Windows and says so with
// errors.ErrUnsupported, so the caller goes straight to Stop there; on Linux and macOS it sends
// SIGINT, and a program that ignores it runs on until Stop. Stop of a tree already ended and
// reaped is ErrStale.
func ExampleInterrupt() {
	gocmd, err := exec.LookPath("go")
	if err != nil {
		fmt.Println(err)
		return
	}
	tree, err := proc.Start(proc.Detached{Path: gocmd, Args: []string{"version"}})
	if err != nil {
		fmt.Println(err)
		return
	}
	ended := make(chan struct{})
	go func() {
		_, _ = tree.Leader.Wait()
		close(ended)
	}()
	if err := proc.Interrupt(tree.ID); !errors.Is(err, errors.ErrUnsupported) {
		select {
		case <-ended:
		case <-time.After(10 * time.Second):
		}
	}
	if err := proc.Stop(tree.ID); err != nil && !errors.Is(err, proc.ErrStale) {
		fmt.Println(err)
	}
	<-ended
	fmt.Println("the tree has ended")
	// Output: the tree has ended
}
