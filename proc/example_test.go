package proc_test

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/birdtable/go-proc/proc"
)

// Run a program with a deadline, a cap on what it may print and a bare environment. Run looks
// nothing up in PATH, so the caller finds the program first, here the go command.
func ExampleRun() {
	gocmd, err := exec.LookPath("go")
	if err != nil {
		fmt.Println(err)
		return
	}
	res, err := proc.Run(context.Background(), proc.Cmd{
		Path:      gocmd,
		Args:      []string{"version"},
		Timeout:   30 * time.Second,
		MaxOutput: 64 << 10,
		Drain:     2 * time.Second,
		Env:       proc.Bare(),
	})
	if ended, ok := errors.AsType[proc.Ended](err); ok {
		fmt.Println("the run was ended:", ended.Why)
		return
	}
	if err != nil {
		fmt.Println(err)
		return
	}
	fields := strings.Fields(string(res.Stdout))
	fmt.Println(res.Code, strings.Join(fields[:min(2, len(fields))], " "))
	// Output: 0 go version
}
