//go:build unix

package proc

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// answerModes runs the helper mode of a tree whose processes answer SIGINT, and says whether mode
// and args are it:
//
//   - answer <how> <role> <binary>: answers SIGINT as how says, then writes role, a space and
//     "ready" and a newline. With how "catch" it writes role and "interrupted" at each SIGINT and
//     runs on; with how "ignore" SIGINT is ignored, and it writes role and "alive" at each SIGUSR1,
//     so a test can ask whether it runs on. A role of "leader" first starts answer <how> child
//     <binary> with this helper's stdout and stderr, in its group, which writes its own lines. Each
//     holds while process <binary>, the test binary that started its tree, lives.
func answerModes(mode string, args []string) (int, bool) {
	if mode != "answer" || len(args) != 3 {
		return 0, false
	}
	how, role, binary := args[0], args[1], args[2]
	heard := make(chan os.Signal, 1)
	word := "interrupted"
	switch how {
	case "catch":
		signal.Notify(heard, os.Interrupt)
	case "ignore":
		signal.Ignore(os.Interrupt)
		signal.Notify(heard, syscall.SIGUSR1)
		word = "alive"
	default:
		return wrote(0, fmt.Errorf("answer: how %q is neither catch nor ignore", how)), true
	}
	if role == "leader" {
		exe, err := os.Executable()
		if err != nil {
			return wrote(0, err), true
		}
		child := &process{Path: exe, Args: []string{exe, helperMode + "answer", how, "child", binary}, Stdout: os.Stdout, Stderr: os.Stderr}
		if err := startTestProcess(child); err != nil {
			return wrote(0, err), true
		}
	}
	go func() {
		for range heard {
			_, _ = fmt.Fprintf(os.Stdout, "%s %s\n", role, word)
		}
	}()
	if code := wrote(fmt.Fprintf(os.Stdout, "%s ready\n", role)); code != 0 {
		return code, true
	}
	return holdWhile(binary), true
}
