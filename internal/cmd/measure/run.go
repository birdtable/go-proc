package main

import (
	"fmt"
	"io"
	"os"
)

// run does what the one word of args names in the module at dir, reading in where the word reads
// a program's output, writes what it has to say to out, and gives the exit code: 0 done, 1
// refused, 2 not run.
func run(args []string, dir string, in io.Reader, out io.Writer) int {
	said, code := do(args, dir, in)
	if _, err := io.WriteString(out, said); err != nil {
		return 2
	}
	return code
}

// do does what run does, and gives what it has to say.
func do(args []string, dir string, in io.Reader) (string, int) {
	if len(args) != 1 {
		return "measure takes one word: notice-check or size\n", 2
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err.Error() + "\n", 2
	}
	said, code := word(root, args[0], in)
	if err := root.Close(); err != nil && code == 0 {
		return said + err.Error() + "\n", 2
	}
	return said, code
}

// word does what the word w names in root, reading in for notice-check.
func word(root *os.Root, w string, in io.Reader) (string, int) {
	switch w {
	case "notice-check":
		return notice(root, in)
	case "size":
		return size(root)
	default:
		return fmt.Sprintf("measure takes notice-check or size, not %q\n", w), 2
	}
}

// size says the module's size, its runtime packages and testenv and then the total, and refuses
// the total above Ceiling.
func size(root *os.Root) (string, int) {
	c, err := Size(root.FS())
	if err != nil {
		return err.Error() + "\n", 2
	}
	n := c.Total()
	said := fmt.Sprintf("runtime packages (all but testenv): %d\n", c.Runtime) +
		fmt.Sprintf("testenv: %d\n", c.Testenv) +
		fmt.Sprintf("non-test Go lines outside internal/: %d (the ceiling is %d)\n", n, Ceiling)
	if n > Ceiling {
		return said + fmt.Sprintf("%d lines is %d above the ceiling\n", n, n-Ceiling), 1
	}
	return said, 0
}
