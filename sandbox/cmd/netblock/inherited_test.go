//go:build (linux && (amd64 || arm64)) || darwin

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// heldWord makes the test binary a caller that holds a file open and inheritable, as a package init
// can, and then runs the rest of its arguments: the command, which calls Run or RunHolding, or, as
// the open control, the row child by an exec of its own.
const heldWord = "netblock-holding"

// holding opens the file at args[1] to read with syscall.Open and no O_CLOEXEC, as a package init
// can, writes to args[0], the control, the descriptor and whether it is marked to close at exec,
// read just before the rest of args runs, and runs it: the command with its arguments, or the row
// child, execed by self with no block.
func holding(self string, args []string) int {
	if len(args) < 3 {
		return 1
	}
	control, planted, rest := args[0], args[1], args[2:]
	fd, err := syscall.Open(planted, syscall.O_RDONLY, 0)
	if err != nil {
		return 1
	}
	cloexec, err := marked(fd)
	if err != nil {
		return 1
	}
	said := "descriptor " + strconv.Itoa(fd) + ", close-on-exec " + strconv.FormatBool(cloexec)
	if err := os.WriteFile(control, []byte(said), 0o600); err != nil {
		return 1
	}
	if rest[0] == asCommand {
		os.Args = append([]string{self}, rest[1:]...)
		main()
		return 0
	}
	_ = syscall.Exec(self, append([]string{self}, rest...), os.Environ()) //nolint:forbidigo,gosec // G204: the open control execs the test binary itself, as a caller with no block would run the program
	return 1
}

// TestWhatTheCallerHoldsInheritableAtRunNeverReachesTheProgram: on Linux and macOS a caller opens
// a file with syscall.Open and no O_CLOEXEC, as a package init can, and then calls Run, and then
// RunHolding; the control, written just before, says the descriptor is open and not marked to
// close at exec. The program, the row child, finds no descriptor open above stderr behind either.
// The open control is the same caller execing the row child itself, with no block, and the row
// child finds the file there, by its descriptor and mode, so the row would see it. The row child
// opens no file of its own before it reads its descriptors: childEnv keeps its runtime from the
// cpu.max of its cgroup, so "none" stays the whole of what the row wants.
//
// What it costs when this fails: a file or a socket a package init of the caller opened without
// close-on-exec reaches the program behind the block, and a connected socket is its way to the
// network.
func TestWhatTheCallerHoldsInheritableAtRunNeverReachesTheProgram(t *testing.T) {
	t.Parallel()
	p := newProof(t)
	self, err := filepath.EvalSymlinks(p.self)
	if err != nil {
		t.Fatal(err)
	}
	rw := realDir(t)
	planted := plant(t, filepath.Join(realDir(t), "held"))
	var st syscall.Stat_t
	if err := syscall.Stat(planted, &st); err != nil {
		t.Fatal(err)
	}
	mode := strconv.FormatUint(uint64(st.Mode), 8)
	holdingFolders := []string{asCommand, "--read-write", rw, "--read-only", filepath.Dir(self)}
	rows := []struct {
		claim   string
		command []string // nil for the open control
	}{
		{"the open control, the row child execed by the caller", nil},
		{"Run", []string{asCommand}},
		{"RunHolding", holdingFolders},
	}
	for i, r := range rows {
		control, report := filepath.Join(rw, "control"+strconv.Itoa(i)), filepath.Join(rw, "report"+strconv.Itoa(i))
		line := []string{rowWord, report, "descriptors"}
		if r.command != nil {
			line = append(append(append([]string{}, r.command...), "--", self), line...)
		}
		code, out := p.start(t, nil, nil, append([]string{heldWord, control, planted}, line...)...)
		held, cerr := os.ReadFile(control)
		said, rerr := os.ReadFile(report)
		if code != 0 || cerr != nil || rerr != nil {
			t.Fatalf("%s: the caller did not run the program: exit %d, %v, %v: %s", r.claim, code, cerr, rerr, out)
		}
		fd, ok := strings.CutSuffix(strings.TrimPrefix(string(held), "descriptor "), ", close-on-exec false")
		if _, err := strconv.Atoi(fd); err != nil || !ok {
			t.Fatalf("%s: the control says %q, want the file open and not marked to close at exec just before the call", r.claim, held)
		}
		switch want := fd + " of mode " + mode; {
		case r.command == nil && !strings.Contains(", "+string(said)+", ", ", "+want+", "):
			t.Errorf("%s: the program held, above stderr: %s, want %s among them", r.claim, said, want)
		case r.command != nil && string(said) != "none":
			t.Errorf("%s: the program held, above stderr: %s, want none (the caller held %s)", r.claim, said, want)
		}
	}
	if len(rows) < 3 {
		t.Fatalf("ran %d rows", len(rows))
	}
}
