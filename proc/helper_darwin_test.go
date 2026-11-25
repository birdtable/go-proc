package proc

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// nsModes runs the helper mode of macOS alone, as a tree id here carries no namespace:
//
//   - late-fork <binary>: starts a hold-while helper, writes "grandchild " and its pid, and then,
//     on each SIGUSR1, starts another and writes "late " and its pid, each line ended by a newline;
//     it holds as hold-while does. binary is the pid of the test binary.
func nsModes(mode string, args []string) (int, bool) {
	if mode != "late-fork" || len(args) != 1 {
		return 0, false
	}
	exe, err := os.Executable()
	if err != nil {
		return wrote(0, err), true
	}
	start := func(word string) int {
		child := &process{Path: exe, Args: []string{exe, helperMode + "hold-while", args[0]}, Stdout: os.Stdout, Stderr: os.Stderr}
		if err := startTestProcess(child); err != nil {
			return wrote(0, err)
		}
		return wrote(fmt.Fprintf(os.Stdout, "%s %d\n", word, child.Process.Pid))
	}
	usr1 := make(chan os.Signal, 1)
	signal.Notify(usr1, syscall.SIGUSR1)
	if code := start("grandchild"); code != 0 {
		return code, true
	}
	go func() {
		for range usr1 {
			_ = start("late")
		}
	}()
	return holdWhile(args[0]), true
}
