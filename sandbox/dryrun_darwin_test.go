//go:build darwin

package sandbox

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// errExecKept is the exec of execKept: recorded and not taken, so a test
// reaches the exec without this process becoming the sandbox.
var errExecKept = errors.New("the exec, recorded and not taken")

// execKept is the macOS kernel, with its real dry run, whose exec is
// recorded in ran and refused.
type execKept struct {
	darwinKernel
	ran []string
}

func (k *execKept) exec(prog string, argv, _ []string) error {
	k.ran = append([]string{prog}, argv...)
	return errExecKept
}

// TestTheMacSandboxRefusingAProfileInItsDryRunIsErrNoBlockAndNothingIsRun:
// on this Mac, the system sandbox is given a profile sandbox_init refuses,
// and a profile that reads a parameter it is not given, and in each the dry
// run fails and the run is refused before the exec, matching ErrNoBlock with
// the sandbox's own first line; the program is never run. The block's own
// profile, open and holding a folder, passes its dry run and reaches the
// exec: the control, without which a dry run that always fails would pass
// the refused rows too.
//
// What it costs when this fails: a profile the sandbox refuses on this
// release reads as the program's own exit, 65, and a caller reports that the
// tool failed where it never ran.
func TestTheMacSandboxRefusingAProfileInItsDryRunIsErrNoBlockAndNothingIsRun(t *testing.T) {
	t.Parallel()
	held, heldParams := macosHolding(named{readWrite: []string{t.TempDir()}, readOnly: []string{"/usr"}})
	rows := []struct {
		claim   string
		profile string
		params  []string
		says    string
	}{
		{"a profile sandbox_init refuses", "(version 1)(bogus)", nil, "unbound variable: bogus"},
		{"a parameter the profile reads and is not given", `(version 1)(allow default)(allow file-read* (subpath (param "RW0")))`, nil, "expected pattern"},
		{"the block's own profile, the control", macosProfile, nil, ""},
		{"the block's own profile holding a folder, the control", held, heldParams, ""},
	}
	prog := []string{"/usr/bin/false"}
	for _, r := range rows {
		k := &execKept{}
		err := sandboxed(k, r.profile, r.params, prog[0], prog, nil)
		if r.says == "" {
			want := slices.Concat([]string{sandboxProgram, sandboxProgram}, r.params, []string{"-p", r.profile}, prog)
			if !errors.Is(err, errExecKept) || !slices.Equal(k.ran, want) {
				t.Errorf("%s: %v, and the exec was %q; want the dry run passed and the exec %q", r.claim, err, k.ran, want)
			}
			continue
		}
		if !errors.Is(err, ErrNoBlock) || !strings.Contains(err.Error(), "did not set the profile in its dry run (it exited 65") || !strings.Contains(err.Error(), r.says) {
			t.Errorf("%s: %v; want ErrNoBlock from the dry run, exit 65, saying %q", r.claim, err, r.says)
		}
		if k.ran != nil {
			t.Errorf("%s: the sandbox was run with the program: %q", r.claim, k.ran)
		}
	}
	if len(rows) != 4 {
		t.Fatalf("ran %d rows, want 4", len(rows))
	}
}

// dryRunOf is the macOS kernel whose dry run runs prog in the sandbox's
// place, through the real dryRun, and whose exec is recorded and refused.
type dryRunOf struct {
	execKept
	prog []string
}

func (k *dryRunOf) dryRun(string, []string) (int, []byte, error) {
	return k.darwinKernel.dryRun(k.prog[0], k.prog[1:])
}

// TestAMacDryRunItsBoundItsCapOrASignalEndedSaysSoWithItsFirstLine: on this
// Mac a dry run that ran and was ended, by a signal, by passing its output
// cap, or by its bound, refuses the run before the exec as a dry run that
// did not exit 0: it matches ErrNoBlock, says it exited -1 and gives the
// first line the dry run wrote, and the program is never run. A dry run
// whose program does not exist is the control: it could not be started, and
// says so.
//
// What it costs when this fails: a sandbox that was started and then ended is
// said to have not been started, and the line it wrote, which says why, is
// dropped.
func TestAMacDryRunItsBoundItsCapOrASignalEndedSaysSoWithItsFirstLine(t *testing.T) {
	t.Parallel()
	rows := []struct {
		claim string
		bound time.Duration
		prog  []string
		says  string
	}{
		{"a dry run a signal ended", 0, []string{"/bin/sh", "-c", "echo refused >&2; kill -9 $$"}, `did not set the profile in its dry run (it exited -1: "refused")`},
		{"a dry run that passed its output cap", 0, []string{"/bin/sh", "-c", "echo refused >&2; /usr/bin/head -c 5000 /dev/zero >&2; exit 65"}, `did not set the profile in its dry run (it exited -1: "refused")`},
		{"a dry run its bound ended", 100 * time.Millisecond, []string{"/bin/sleep", "30"}, `did not set the profile in its dry run (it exited -1: "")`},
		{"a dry run that cannot be started, the control", 0, []string{filepath.Join(t.TempDir(), "absent")}, "could not be started for its dry run of the profile"},
	}
	prog := []string{"/usr/bin/false"}
	for _, r := range rows {
		k := &dryRunOf{bound: r.bound, prog: r.prog}
		err := sandboxed(k, macosProfile, nil, prog[0], prog, nil)
		if !errors.Is(err, ErrNoBlock) || !strings.Contains(err.Error(), r.says) {
			t.Errorf("%s: %v; want ErrNoBlock saying %q", r.claim, err, r.says)
		}
		if k.ran != nil {
			t.Errorf("%s: the sandbox was run with the program: %q", r.claim, k.ran)
		}
	}
	if len(rows) != 4 {
		t.Fatalf("ran %d rows, want 4", len(rows))
	}
}

// opensDuring is the macOS kernel whose real dry run runs while a descriptor
// is opened without close-on-exec, as another goroutine of a caller might,
// and whose exec, recorded and refused, reads whether that descriptor is
// marked to close.
type opensDuring struct {
	execKept
	fd       int
	atExec   int
	readFlag error
}

func (k *opensDuring) dryRun(path string, args []string) (int, []byte, error) {
	fd, err := syscall.Dup(2)
	if err != nil {
		return 0, nil, err
	}
	k.fd = fd
	return k.darwinKernel.dryRun(path, args)
}

func (k *opensDuring) exec(prog string, argv, environ []string) error {
	k.atExec, k.readFlag = unix.FcntlInt(uintptr(k.fd), unix.F_GETFD, 0)
	return k.execKept.exec(prog, argv, environ)
}

// TestADescriptorOpenedDuringTheMacDryRunIsMarkedToCloseAtTheExec: on this
// Mac a descriptor opened without close-on-exec while the dry run runs, after
// streams marked the descriptors, is marked to close by the time the sandbox
// is exec'd, so it does not reach the program.
//
// What it costs when this fails: a socket another goroutine opened during a
// dry run of up to 10 s is open in the program, read and written behind the
// block.
func TestADescriptorOpenedDuringTheMacDryRunIsMarkedToCloseAtTheExec(t *testing.T) {
	t.Parallel()
	k := &opensDuring{fd: -1}
	t.Cleanup(func() {
		if k.fd >= 0 {
			_ = syscall.Close(k.fd)
		}
	})
	if err := runMac(k, named{}, "/usr/bin/false", []string{"/usr/bin/false"}, nil); !errors.Is(err, errExecKept) {
		t.Fatalf("runMac: %v; want the dry run passed and the exec reached", err)
	}
	if k.readFlag != nil {
		t.Fatalf("reading descriptor %d's flags at the exec: %v", k.fd, k.readFlag)
	}
	if k.atExec&unix.FD_CLOEXEC == 0 {
		t.Errorf("descriptor %d, opened during the dry run, was not marked to close at the exec (flags %#x)", k.fd, k.atExec)
	}
}
