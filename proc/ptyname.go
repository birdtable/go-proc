package proc

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"
)

// errStdinUntold is a Stdin, without Terminal, that Windows does not say enough of to tell it from
// an MSYS or Cygwin pty's pipe: its file type, or the name of its pipe.
var errStdinUntold = fmt.Errorf("%w: Terminal is not set and Windows does not say what Stdin is, so it cannot be told from an MSYS or Cygwin pty's pipe, where a program that finds no terminal would ask this process's user", ErrRefused)

// ptyAnswer is whether a Stdin is an MSYS or Cygwin pty's pipe, from what Windows answers of it:
// kindErr is GetFileType's error, pipe whether its type is FILE_TYPE_PIPE, and name the query of
// its name, asked of a pipe alone, which gives the name's length in bytes, the room it came in,
// and the query's error.
//
// A Stdin that cannot be told from a pty's pipe is refused: a type Windows does not give, which
// FILE_TYPE_UNKNOWN is too, as x/sys gives it; a name query's error, given as it is when it wraps
// ErrRefused already, as the bound's does, and otherwise errStdinUntold with it; and a length that
// is odd or past the room. A name of 0 bytes is no pty's: it is what an unnamed pipe, such as an
// os.Pipe end, answers, and a pty's pipes are named so another process can open them.
func ptyAnswer(kindErr error, pipe bool, name func() (uint32, []uint16, error)) (bool, error) {
	if kindErr != nil {
		return false, fmt.Errorf("%w: its file type: %w", errStdinUntold, kindErr)
	}
	if !pipe {
		return false, nil
	}
	length, room, err := name()
	if errors.Is(err, errRefused) {
		return false, err
	}
	if err != nil {
		return false, fmt.Errorf("%w: its pipe's name: %w", errStdinUntold, err)
	}
	n := int(length / 2)
	if length%2 != 0 || n > len(room) {
		return false, fmt.Errorf("%w: its pipe's name came with a length of %d bytes, odd or past the %d of its room", errStdinUntold, length, 2*len(room))
	}
	return msysPty(string(utf16.Decode(room[:n]))), nil
}

// msysPty is whether name, a named pipe's name as Windows' FileNameInfo gives it, is the name an
// MSYS or Cygwin pty gives the pipe a native program reads as its terminal: \msys-<id>-pty<n>-from-master
// or -to-master, \cygwin-... the same, or either under \Device\NamedPipe. Under mintty with pseudo
// consoles off, a program's stdin is such a pipe and no console, and an MSYS program takes it as its
// terminal, so Windows' isTerminal takes it as one too. The name is read word by word, split at
// each dash; a word past the fifth is let be, as MSYS may add one.
func msysPty(name string) bool {
	words := strings.Split(name, "-")
	if len(words) < 5 {
		return false
	}
	switch strings.ToLower(words[0]) {
	case `\msys`, `\cygwin`, `\device\namedpipe\msys`, `\device\namedpipe\cygwin`:
	default:
		return false
	}
	n, pty := strings.CutPrefix(words[2], "pty")
	return words[1] != "" && pty && n != "" && strings.Trim(n, "0123456789") == "" &&
		(words[3] == "from" || words[3] == "to") && words[4] == "master"
}
