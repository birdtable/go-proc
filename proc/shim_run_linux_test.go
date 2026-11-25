package proc

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// TestTheShimsSecondStageChecksItselfThenWaitsForTheGoAhead: this test binary started as the shim's
// second stage, by shimRun as its argv[0], the report's descriptor, the program's path and its argv,
// with the report at 3, the program's environment at 4 and the go-ahead at 5, as the stage helper
// execs it. Started with no_new_privs set and each capability set empty, it writes shimReady to the report and
// runs nothing until the test, which has read that byte, writes shimGo; then it execs the program
// with the environment of descriptor 4 (STAGE=1, which the program prints) and not its own (STAGE=0),
// huge pages on again where the stage was started with them off, as the first stage starts it
// (THP_enabled in the program's status, which it prints, where the kernel has the line), and none of
// the three descriptors is open in the program, which says so. The stage is started with GODEBUG
// containermaxprocs=0, as the first stage starts it, and the program holds no GODEBUG it was not
// given by the memfd: none in the first case, the one it was given in the second. A program path that cannot
// be run is reported as shimExec and its errno after shimReady, and the stage exits 127. A go-ahead
// closed with no byte, as a caller that refuses the run closes it, and one that says another byte,
// end the stage with 127 and no program. A report that is no number, a stage started without
// no_new_privs, and one started with it in a user namespace holding CAP_SYS_ADMIN ambient, each exit
// 127 having written nothing to the report and run nothing, with no go-ahead asked of the test.
//
// What it costs when this fails: any local user starts a setuid or file-capability binary that holds
// this package by shimRun as its argv[0], and it runs the program they name with what the exec gave it; or the stage execs the program before the caller has read the run's
// threads, and a healthy program that makes a user namespace at once is refused as a forger and
// killed; or the report stays open in the program, and the caller waits for a report that does not
// end.
func TestTheShimsSecondStageChecksItselfThenWaitsForTheGoAhead(t *testing.T) {
	t.Parallel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	const capSysAdmin = 21 // CAP_SYS_ADMIN, by its number
	// THP_enabled is a line of status from Linux 5.0; the program prints it, and the row wants 1: the
	// stage turns huge pages off for itself and on again before the program.
	thp := " thp="
	if status, err := os.ReadFile("/proc/self/status"); err == nil && strings.Contains(string(status), "THP_enabled:") {
		thp = " thp=1"
	}
	const sh, script = "/bin/sh", `printf %s "$STAGE"; printf ' thp=%s' "$(sed -n 's/^THP_enabled:[[:space:]]*//p' /proc/$$/status)"; printf ' godebug=%s' "$GODEBUG"; for f in 3 4 5; do [ -e /proc/$$/fd/$f ] && printf ' open%s' $f; done; exit 0`
	userns := &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: os.Geteuid(), HostID: os.Geteuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: os.Getegid(), HostID: os.Getegid(), Size: 1}},
		AmbientCaps: []uintptr{capSysAdmin},
	}
	for _, c := range []struct {
		name, nnp, report, path string
		attr                    *syscall.SysProcAttr
		goAhead                 []byte
		out                     string
		code                    int
		report1                 []byte
	}{
		{"a program", "nct", "3", sh, nil, []byte{shimGo}, "1" + thp + " godebug=", 0, []byte{shimReady}},
		{"a program given a GODEBUG", "nct", "3", sh, nil, []byte{shimGo}, "1" + thp + " godebug=asynctimerchan=1", 0, []byte{shimReady}},
		{"a path that cannot be run", "nct", "3", "/nonexistent/program", nil, []byte{shimGo}, "", 127, []byte{shimReady, shimExec, byte(syscall.ENOENT)}},
		{"a go-ahead closed with no byte", "nct", "3", sh, nil, nil, "", 127, []byte{shimReady}},
		{"a go-ahead that says another byte", "nct", "3", sh, nil, []byte{shimReady}, "", 127, []byte{shimReady}},
		{"a report that is no number", "nc", "x", sh, nil, nil, "", 127, nil},
		{"a stage without no_new_privs", "c", "3", sh, nil, nil, "", 127, nil},
		{"a stage that holds a capability", "n", "3", sh, userns, nil, "", 127, nil},
	} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		goR, goW, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		env, err := memfd()
		if err != nil {
			t.Fatal(err)
		}
		// the program's own environment, which the caller wrote to the memfd: it holds a GODEBUG only where it was given one
		given := "STAGE=1\x00"
		if strings.Contains(c.name, "given a GODEBUG") {
			given += "GODEBUG=asynctimerchan=1\x00"
		}
		if _, err := env.WriteAt([]byte(given), 0); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		cmd := exec.CommandContext(t.Context(), exe, slices.Concat([]string{helperMode + "stage", c.nnp, shimRun, c.report, c.path}, []string{"sh", "-c", script})...) //nolint:gosec // G204: this test binary, started as the shim's second stage
		cmd.Env = []string{"STAGE=0", "GODEBUG=containermaxprocs=0"}                                                                                                   // what the first stage gives the second: shimRunEnv
		cmd.Stdout = &out
		cmd.ExtraFiles = []*os.File{w, env, goR}
		cmd.SysProcAttr = c.attr
		err = cmd.Start()
		_, _, _ = w.Close(), env.Close(), goR.Close()
		if err != nil {
			_, _ = r.Close(), goW.Close()
			if errno, ok := errors.AsType[syscall.Errno](err); ok && c.attr != nil && slices.Contains(hostStartErrnos(), errno) {
				t.Logf("%s: this host refuses the user namespace the row holds its capability in, so the row is not run: %v", c.name, err)
				continue
			}
			t.Fatal(err)
		}
		// The first byte is read before the go-ahead is given: a stage that ran its program without
		// waiting has printed by then, which the row for a go-ahead closed with no byte sees.
		got, readErr := io.ReadAll(io.LimitReader(r, 1))
		if len(c.goAhead) > 0 {
			_, _ = goW.Write(c.goAhead) // a stage that is gone reads none, and its report says so below
		}
		_ = goW.Close()
		rest, restErr := io.ReadAll(r) // ends at the program's exec, which closes the report, or at the stage's exit
		got = append(got, rest...)
		waitErr := cmd.Wait()
		_ = r.Close()
		code := 0
		if exited, ok := errors.AsType[*exec.ExitError](waitErr); ok {
			code = exited.ExitCode()
		} else if waitErr != nil {
			t.Fatal(waitErr)
		}
		if readErr != nil || restErr != nil || !bytes.Equal(got, c.report1) || code != c.code || out.String() != c.out {
			t.Errorf("%s: report %q (%v, %v), exit %d, stdout %q, want report %q, exit %d, stdout %q", c.name, got, readErr, restErr, code, out.String(), c.report1, c.code, c.out)
		}
	}
}
