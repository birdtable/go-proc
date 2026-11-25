// Package main is the netblock command, which runs a program with the
// network closed, and with its files held to folders when it is given any:
//
//	netblock [--read-write <folder>]... [--read-only <folder>]... -- <program by its absolute path> [arguments]
//
// Each folder is named by its absolute path. With none, every file is open
// to the program, as sandbox.Run leaves them; with any, every file outside
// them is denied, reads included, as sandbox.RunHolding holds them.
//
// It sets the block on itself and execs the program, so the program's exit is
// its own. When the program is not run it says why on stderr and exits 2 for
// a command line it does not take, 5 when the block could not be set, which
// it names ("no block"), on a system that gives none as on one whose kernel
// refused a step, and 1 when the program could not be started behind a block
// that was set.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/birdtable/go-proc/sandbox"
)

// The exits of a program that was not run.
const (
	codeFailed  = 1
	codeUsage   = 2
	codeRefused = 5
)

func main() { os.Exit(blocked(os.Args[1:], os.Environ(), os.Stderr)) }

// blocked runs args behind the block with environ, and returns the exit
// when the program was not run, having said why on stderr.
func blocked(args, environ []string, stderr io.Writer) int {
	code, says := exitFor(run(args, environ))
	if _, werr := io.WriteString(stderr, "netblock: "+says+"\n"); werr != nil {
		return codeFailed
	}
	return code
}

// exitFor is the exit for err, why the program was not run, and what the
// command says of it: the usage, the block it could not set, named, or the
// program it could not start.
func exitFor(err error) (int, string) {
	switch {
	case errors.Is(err, errLine), errors.Is(err, sandbox.ErrUsage):
		return codeUsage, err.Error()
	case errors.Is(err, sandbox.ErrNoBlock):
		return codeRefused, "no block: " + err.Error()
	}
	return codeFailed, err.Error()
}

// errLine is a command line the command does not take: no --, no program
// after it, or a program not named by its absolute path. Its exit is 2, as
// sandbox.ErrUsage's.
var errLine = errors.New("the blocked command takes --, then the program by its absolute path, then its arguments")

// run takes the folders from the front of args, each flag with the folder
// after it, then --, the program by its absolute path and its arguments, and
// runs the program behind the block: sandbox.Run with no folder, and
// sandbox.RunHolding with any.
func run(args, environ []string) error {
	var folders sandbox.Folders
	for len(args) > 0 {
		var into *[]string
		switch args[0] {
		case "--read-write":
			into = &folders.ReadWrite
		case "--read-only":
			into = &folders.ReadOnly
		}
		if into == nil {
			break
		}
		if len(args) < 2 {
			return fmt.Errorf("%s takes a folder by its absolute path: %w", args[0], sandbox.ErrUsage)
		}
		*into = append(*into, args[1])
		args = args[2:]
	}
	if len(args) < 2 || args[0] != "--" || !filepath.IsAbs(args[1]) {
		return errLine
	}
	if len(folders.ReadWrite)+len(folders.ReadOnly) == 0 {
		return sandbox.Run(args[1], args[2:], environ)
	}
	return sandbox.RunHolding(folders, args[1], args[2:], environ)
}
