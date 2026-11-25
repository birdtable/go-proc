package proc

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"unicode/utf16"
)

// TestMsysPtyTakesOnlyTheNamesOfAnMSYSOrCygwinPtysPipes: the names an MSYS or Cygwin pty gives its
// pipes, with and without \Device\NamedPipe and with a word past the fifth, are a pty's; a name
// that differs in any one word, or lacks one, is not.
//
// What it costs when this fails: on Windows, a run without Terminal whose Stdin is the caller's
// mintty, with pseudo consoles off, goes ahead, and a program that asks at its terminal asks the
// caller's user there; or a pipe that is no pty is refused without Terminal.
func TestMsysPtyTakesOnlyTheNamesOfAnMSYSOrCygwinPtysPipes(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		`\msys-dd50a72ab4668b33-pty0-from-master`:                  true,
		`\msys-dd50a72ab4668b33-pty12-to-master`:                   true,
		`\cygwin-e022582115c10879-pty4-from-master`:                true,
		`\Device\NamedPipe\msys-dd50a72ab4668b33-pty0-from-master`: true,
		`\Device\NamedPipe\cygwin-e022582115c10879-pty4-to-master`: true,
		`\msys-dd50a72ab4668b33-pty0-from-master-nat`:              true,
		`\MSYS-dd50a72ab4668b33-pty0-from-master`:                  true,
		``:                                       false,
		`\msys`:                                  false,
		`\msys-dd50a72ab4668b33-pty0-from`:       false,
		`\msys--pty0-from-master`:                false,
		`\msys-dd50a72ab4668b33-pty-from-master`: false,
		`\msys-dd50a72ab4668b33-ptyx-from-master`:   false,
		`\msys-dd50a72ab4668b33-tty0-from-master`:   false,
		`\msys-dd50a72ab4668b33-pty0-into-master`:   false,
		`\msys-dd50a72ab4668b33-pty0-from-slave`:    false,
		`\gotest-dd50a72ab4668b33-pty0-from-master`: false,
		`msys-dd50a72ab4668b33-pty0-from-master`:    false,
		`\msys-1888ae32e00d56aa-pipe-0x1`:           false,
	} {
		if got := msysPty(name); got != want {
			t.Errorf("msysPty(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestPtyAnswerRefusesAStdinItCannotTellFromAPtysPipe: ptyAnswer, given what Windows answers of a
// Stdin, refuses with errStdinUntold, which wraps ErrRefused and the cause, a file type Windows did
// not give, a name query's error, and a name's length that is odd or past its room; it gives a
// query's error that wraps ErrRefused already, as the bound's does, as it is. A type that is no
// pipe is no pty's and asks no name; a name of 0 bytes is no pty's; a pty's name, up to the room's
// last word, is one; and a name of another kind is not.
//
// What it costs when this fails: on Windows, a run without Terminal whose Stdin Windows says too
// little of to tell from an MSYS or Cygwin pty's pipe goes ahead, and a program that asks at its
// terminal may ask the caller's user there; or an os.Pipe end, or a file, is refused.
func TestPtyAnswerRefusesAStdinItCannotTellFromAPtysPipe(t *testing.T) {
	t.Parallel()
	pty := utf16.Encode([]rune(`\msys-dd50a72ab4668b33-pty0-from-master`))
	other := utf16.Encode([]rune(`\msys-1888ae32e00d56aa-pipe-0x1`))
	fileErr := errors.New("the handle is invalid")
	queryErr := errors.New("the pipe is being closed")
	bound := fmt.Errorf("%w: the name did not come within its bound", ErrRefused)
	named := func(length int, room []uint16, err error) func() (uint32, []uint16, error) {
		return func() (uint32, []uint16, error) { return uint32(length), room, err }
	}
	for _, row := range []struct {
		name    string
		kindErr error
		pipe    bool
		query   func() (uint32, []uint16, error)
		pty     bool
		want    []error // each wrapped by the error; none is no error
		as      error   // the error itself, given as it is
	}{
		{"a file type Windows does not give", fileErr, false, nil, false, []error{errStdinUntold, ErrRefused, fileErr}, nil},
		{"a file type Windows does not give, said to be a pipe", fileErr, true, nil, false, []error{errStdinUntold, ErrRefused, fileErr}, nil},
		{"no pipe", nil, false, nil, false, nil, nil},
		{"a name query's error", nil, true, named(0, nil, queryErr), false, []error{errStdinUntold, ErrRefused, queryErr}, nil},
		{"a name query past its bound", nil, true, named(0, nil, bound), false, []error{ErrRefused}, bound},
		{"an odd length", nil, true, named(2*len(pty)-1, pty, nil), false, []error{errStdinUntold, ErrRefused}, nil},
		{"a length past the room", nil, true, named(2*len(pty)+2, pty, nil), false, []error{errStdinUntold, ErrRefused}, nil},
		{"a name of 0 bytes", nil, true, named(0, pty, nil), false, nil, nil},
		{"a pty's name, filling the room", nil, true, named(2*len(pty), pty, nil), true, nil, nil},
		{"a pty's name, in a room with words to spare", nil, true, named(2*len(pty), append(slices.Clone(pty), 'x', 'y'), nil), true, nil, nil},
		{"another pipe's name", nil, true, named(2*len(other), other, nil), false, nil, nil},
	} {
		query := row.query
		if query == nil {
			query = func() (uint32, []uint16, error) {
				t.Errorf("%s: the name was asked", row.name)
				return 0, nil, nil
			}
		}
		got, err := ptyAnswer(row.kindErr, row.pipe, query)
		if got != row.pty || (err == nil) != (row.want == nil) {
			t.Errorf("%s: %v, %v, want %v and an error wrapping %v", row.name, got, err, row.pty, row.want)
			continue
		}
		if row.as != nil && (!errors.Is(err, row.as) || errors.Is(err, errStdinUntold)) {
			t.Errorf("%s: %v, want the query's error as it is", row.name, err)
		}
		for _, want := range row.want {
			if !errors.Is(err, want) {
				t.Errorf("%s: %v, want it to wrap %v", row.name, err, want)
			}
		}
	}
}
