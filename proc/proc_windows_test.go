//go:build windows

package proc

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows" //nolint:depguard // arch.ports.23: the tests read the Job Object the package makes, by the calls it makes it with
)

// watcher is a named pipe of the test's that a pipe-nested helper writes its pid and its holder's
// to, and then waits on for a byte back. A process the test opens while it waits is open while it
// runs, so its handle is its own and no pid reused after it can stand in for it.
type watcher struct {
	name string
	f    *os.File
}

// watching makes the test's watcher, named for the test and this process, closed when the test ends.
func watching(t *testing.T) *watcher {
	t.Helper()
	name := fmt.Sprintf(`\\.\pipe\go-proc-test-%d-%s`, os.Getpid(), t.Name())
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateNamedPipe(p, windows.PIPE_ACCESS_DUPLEX|windows.FILE_FLAG_FIRST_PIPE_INSTANCE,
		windows.PIPE_TYPE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS, 1, 512, 512, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	w := &watcher{name: name, f: os.NewFile(uintptr(h), name)}
	t.Cleanup(func() { _ = w.f.Close() })
	return w
}

// opened waits for the helper to write its two pids, opens each process while it waits, and lets
// the helper go on.
func (w *watcher) opened(t *testing.T) []windows.Handle {
	t.Helper()
	if err := windows.ConnectNamedPipe(windows.Handle(w.f.Fd()), nil); err != nil && !errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(w.f).ReadString('\n')
	if err != nil {
		t.Fatalf("the helper wrote %q: %v", line, err)
	}
	var hs []windows.Handle
	for field := range strings.FieldsSeq(line) {
		pid, err := strconv.ParseUint(field, 10, 32)
		if err != nil {
			t.Fatalf("the helper wrote %q: %v", line, err)
		}
		h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
		if err != nil {
			t.Fatalf("opening process %d, which waits for the test: %v", pid, err)
		}
		t.Cleanup(func() { _ = windows.CloseHandle(h) })
		hs = append(hs, h)
	}
	if len(hs) != 2 {
		t.Fatalf("the helper wrote %q, want its pid and its holder's", line)
	}
	if _, err := w.f.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	return hs
}

// ended waits for each process to end, and fails the test when one has not with most of the
// test's time gone, rather than leave go test's own deadline to stop it.
func ended(t *testing.T, what string, hs []windows.Handle) {
	t.Helper()
	wait := uint32(windows.INFINITE)
	if deadline, ok := t.Deadline(); ok {
		wait = uint32(min(max(time.Until(deadline)*9/10/time.Millisecond, 0), windows.INFINITE-1))
	}
	for i, h := range hs {
		event, err := windows.WaitForSingleObject(h, wait)
		if err != nil || event != windows.WAIT_OBJECT_0 {
			t.Fatalf("%s: process %d of %d still runs with the test's deadline near (%d, %v)", what, i+1, len(hs), event, err)
		}
	}
}

// ran is what a run returned.
type ran struct {
	res Result
	err error
}

// running runs c in a goroutine, and gives what Run returns once it does.
func running(t *testing.T, c Cmd) <-chan ran {
	t.Helper()
	done := make(chan ran, 1)
	go func() {
		res, err := Run(t.Context(), c)
		done <- ran{res, err}
	}()
	return done
}

// TestATimedOutRunEndsEveryProcessOfItsJob: the helper starts a holder and both hold; the run
// reaches its Timeout, ends with Ended{WhyDeadline}, and the job's end ends the holder too, which
// the helper started and the run never saw.
//
// What it costs when this fails: a timed-out run on Windows leaves the program's children
// running, holding CPU and files after the caller was told the run ended.
func TestATimedOutRunEndsEveryProcessOfItsJob(t *testing.T) {
	t.Parallel()
	w := watching(t)
	c := helperCmd(t, "pipe-nested", w.name, "hold")
	c.Timeout = 5 * time.Second
	done := running(t, c)
	hs := w.opened(t)
	if r := <-done; !endedWith(r.err, WhyDeadline) || r.res.Code != -1 {
		t.Errorf("a run that reached its Timeout: %v, Code %d, want Ended{%s}", r.err, r.res.Code, WhyDeadline)
	}
	ended(t, "a run that reached its Timeout", hs)
}

// TestAProcessAProgramLeftRunningEndsWithItsJob: the helper starts a holder and exits 0; the
// run's end ends the job, and the holder with it, and the run is the helper's own exit.
//
// What it costs when this fails: a program that leaves a process in the background on Windows
// leaves it running for as long as it likes, and the run waits on its hold of the output.
func TestAProcessAProgramLeftRunningEndsWithItsJob(t *testing.T) {
	t.Parallel()
	w := watching(t)
	done := running(t, helperCmd(t, "pipe-nested", w.name, "exit"))
	hs := w.opened(t)
	if r := <-done; r.err != nil || r.res.Code != 0 {
		t.Errorf("a program that exited 0: %v, Code %d, stderr %q", r.err, r.res.Code, r.res.Stderr)
	}
	ended(t, "a program that exited 0", hs)
}

// TestTheJobAllowsNoBreakaway: the job is set to kill its processes when it closes, and allows no
// breakaway, silent or asked for; the child is not started asking to leave it, but suspended, to be
// put in the job before it runs; and a process in the job that asks to start a child out of it is
// refused.
//
// What it costs when this fails: a program on Windows starts a process outside its job, and that
// process outlives the run, the timeout and this process.
func TestTheJobAllowsNoBreakaway(t *testing.T) {
	t.Parallel()
	cmd := prepare(t.Context(), helperCmd(t, "hold"), Bare())
	g, err := grouped(cmd, 0, drainWait, Limits{}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.stop() })
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if err := queryJob(g.job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil { //nolint:gosec // G103: the job call reads the struct Windows defines, by its address and size
		t.Fatal(err)
	}
	flags := info.BasicLimitInformation.LimitFlags
	if flags&windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE == 0 || flags&(windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK|windows.JOB_OBJECT_LIMIT_SILENT_BREAKAWAY_OK) != 0 {
		t.Errorf("the job's limits are %#x, want kill on close and no breakaway", flags)
	}
	if f := cmd.SysProcAttr.CreationFlags; f&windows.CREATE_BREAKAWAY_FROM_JOB != 0 || f&windows.CREATE_SUSPENDED == 0 {
		t.Errorf("the child is created with %#x, want suspended and not leaving its job", f)
	}
	res, err := Run(t.Context(), helperCmd(t, "breakaway"))
	if err != nil || res.Code != 0 {
		t.Errorf("a process in the job asked to start a child out of it: %v, Code %d, stderr %q, want it refused", err, res.Code, res.Stderr)
	}
}

// moveStack grows the goroutine's stack until it is copied elsewhere, as a call deep enough does
// at any time, and so moves every value on it.
func moveStack(t *testing.T) {
	t.Helper()
	var mark byte
	before := uintptr(unsafe.Pointer(&mark)) //nolint:gosec // G103: the address is only compared, to see the stack move
	for depth := 1; depth <= 1<<16; depth *= 2 {
		_ = deep(depth)
		if uintptr(unsafe.Pointer(&mark)) != before { //nolint:gosec // G103: the address is only compared, to see the stack move
			return
		}
	}
	t.Fatal("the stack did not move")
}

// deep calls itself n deep, each call holding a kilobyte on the stack.
//
//go:noinline
func deep(n int) byte {
	var pad [1024]byte
	pad[n%len(pad)] = byte(n)
	if n == 0 {
		return pad[0]
	}
	return deep(n-1) + pad[n%len(pad)]
}

// TestAJobStructSurvivesAStackMovedInTheCall: the job's limits are set by setJob with the stack
// moved after the struct's address is taken and, after the move, a second flag written to the
// struct; they are then read by queryJob with the stack moved again. Both flags are read back:
// Windows read the struct where the code last wrote it, and wrote it where the test reads it.
//
// What it costs when this fails: a job's limits are set from a stack already freed, so a job may
// have none, kill on close among them, and the child's tree outlives the run; and a read of them
// reads nothing, as an earlier run did, read 0x0 under load.
func TestAJobStructSurvivesAStackMovedInTheCall(t *testing.T) {
	t.Parallel()
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = windows.CloseHandle(job) })
	want := uint32(windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_DIE_ON_UNHANDLED_EXCEPTION)
	var set windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	set.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if err := setJob(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&set)), uint32(unsafe.Sizeof(set)), func() { //nolint:gosec // G103: the job call takes the struct Windows defines, by its address and size
		moveStack(t)
		set.BasicLimitInformation.LimitFlags = want
	}); err != nil {
		t.Fatal(err)
	}
	var got windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if err := queryJob(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&got)), uint32(unsafe.Sizeof(got)), func() { moveStack(t) }); err != nil { //nolint:gosec // G103: the job call reads the struct Windows defines, by its address and size
		t.Fatal(err)
	}
	if flags := got.BasicLimitInformation.LimitFlags; flags != want {
		t.Errorf("the job's limits, set and read with the stack moved in each call, are %#x, want %#x", flags, want)
	}
}

// TestARunInsideAJobRunsInAJobOfItsOwn: Run, run by a program that is itself in a run's job, puts
// its child in a job of its own, nested in that one, and the child runs to its own exit.
//
// What it costs when this fails: a program run under a CI runner's job, or under proc itself,
// cannot run anything with proc.
func TestARunInsideAJobRunsInAJobOfItsOwn(t *testing.T) {
	t.Parallel()
	res, err := Run(t.Context(), helperCmd(t, "runner-ok"))
	if err != nil || res.Code != 0 {
		t.Errorf("a run inside a job: %v, Code %d, stderr %q", err, res.Code, res.Stderr)
	}
}

// TestARunnerKilledLeavesNoProcessOfItsJob: a runner, started outside any job of the test's, runs
// a helper that starts a holder; the runner is killed by taskkill /F, which ends it by
// TerminateProcess, so it runs nothing more. Its handle to the job closes with it, and the job kills the helper and
// the holder.
//
// What it costs when this fails: a runner that is killed, or crashes, on Windows leaves every
// program it ran, and their children, running with no one to end them.
func TestARunnerKilledLeavesNoProcessOfItsJob(t *testing.T) {
	t.Parallel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	w := watching(t)
	runner := &process{Path: exe, Args: []string{exe, helperMode + "runner-hold", w.name}, Env: Bare()}
	if err := startTestProcess(runner); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = runner.Process.Kill()
		_ = runner.Wait()
	})
	hs := w.opened(t)
	system, err := windows.GetSystemDirectory()
	if err != nil {
		t.Fatal(err)
	}
	taskkill := &process{Path: filepath.Join(system, "taskkill.exe"), Args: []string{"taskkill.exe", "/F", "/PID", strconv.Itoa(runner.Process.Pid)}, Env: Bare()}
	if out, err := combinedOutput(taskkill); err != nil {
		t.Fatalf("taskkill /F of the runner: %v: %s", err, out)
	}
	ended(t, "a runner killed", hs)
}

// TestATestBinaryKilledLeavesNoProcessItsTestsStarted: a helper stands in for a test binary. It
// starts a helper through startInTestsJob, as the test binary starts what its tests start, and that
// helper starts a holder. The stand-in is ended by TerminateProcess while both hold, so it runs
// nothing more; its handle to its tests' job closes with it, and the job ends the helper and the
// holder.
//
// What it costs when this fails: a test binary killed on Windows, by a timeout, a session or a
// person, leaves what its tests started running, and their children, holding the machine.
func TestATestBinaryKilledLeavesNoProcessItsTestsStarted(t *testing.T) {
	t.Parallel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	w := watching(t)
	standIn := &process{Path: exe, Args: []string{exe, helperMode + "tests-job", w.name}, Env: Bare()}
	if err := startTestProcess(standIn); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = standIn.Process.Kill()
		_ = standIn.Wait()
	})
	hs := w.opened(t)
	t.Cleanup(func() { endLeft(t, hs) })
	var killed error
	if err := standIn.Process.WithHandle(func(h uintptr) {
		killed = windows.TerminateProcess(windows.Handle(h), 1)
	}); err != nil {
		killed = err
	}
	if killed != nil {
		t.Fatalf("TerminateProcess of the stand-in: %v", killed)
	}
	ended(t, "a test binary killed", hs)
}

// endLeft ends each process of hs that still runs, so a row that fails leaves nothing holding. Each
// handle is still open, so its pid names no other process.
func endLeft(t *testing.T, hs []windows.Handle) {
	t.Helper()
	for _, h := range hs {
		if event, err := windows.WaitForSingleObject(h, 0); err == nil && event == windows.WAIT_OBJECT_0 {
			continue
		}
		pid, err := windows.GetProcessId(h)
		if err != nil {
			t.Errorf("the pid of a process left running: %v", err)
			continue
		}
		k, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pid)
		if err != nil {
			t.Errorf("opening process %d, left running, to end it: %v", pid, err)
			continue
		}
		if err := errors.Join(windows.TerminateProcess(k, 1), windows.CloseHandle(k)); err != nil {
			t.Errorf("ending process %d, left running: %v", pid, err)
		}
	}
}

// TestANilEnvGivesAWindowsChildSystemRoot: Bare() holds SystemRoot, which the test process keeps,
// and a child given a nil Env sees it, so a Windows program that needs it to load does load.
//
// What it costs when this fails: every program run with a nil Env on Windows fails to start its
// sockets or its crypto, far from the cause.
func TestANilEnvGivesAWindowsChildSystemRoot(t *testing.T) {
	t.Parallel()
	systemRoot := func(kv string) bool { return strings.HasPrefix(strings.ToUpper(kv), "SYSTEMROOT=") }
	if !slices.ContainsFunc(Bare(), systemRoot) {
		t.Errorf("Bare() is %q, want SystemRoot in it", Bare())
	}
	if got := environment(t, nilEnvHelperCmd(t, "env")); !slices.ContainsFunc(got, systemRoot) {
		t.Errorf("a nil Env gave the child %q, want SystemRoot in it", got)
	}
}

// TestSystem32sOpenSSHStartsUnderBare: System32's own ssh.exe, run with Bare and nothing more, starts
// and prints its version, as it does with ProgramData given back alone and not without it: Bare holds ProgramData, which the test process keeps.
//
// What it costs when this fails: a caller that starts Windows' own OpenSSH with Bare or SystemEnv
// gets exit 255 and not a byte of output, with nothing that names the variable it lacked.
func TestSystem32sOpenSSHStartsUnderBare(t *testing.T) {
	t.Parallel()
	ssh := filepath.Join(os.Getenv("SystemRoot"), "System32", "OpenSSH", "ssh.exe")
	if _, err := os.Stat(ssh); err != nil {
		t.Fatalf("this Windows host has no OpenSSH client at %s to start: %v", ssh, err)
	}
	r, err := Run(t.Context(), Cmd{Path: ssh, Args: []string{"-V"}, Env: Bare(), Dir: t.TempDir(), Timeout: longEnough, MaxOutput: 1 << 16})
	if err != nil || r.Code != 0 {
		t.Fatalf("ssh -V under Bare %q: %v, code %d, stdout %q, stderr %q", Bare(), err, r.Code, r.Stdout, r.Stderr)
	}
	if out := string(r.Stdout) + string(r.Stderr); !strings.HasPrefix(out, "OpenSSH") {
		t.Errorf("ssh -V under Bare printed %q, want its version, which starts OpenSSH", out)
	}
}

// TestOnWindowsOnlyAProgramFileIsStarted:Run starts a Path only when it, and the file PATHEXT finds
// for it, name a program file (.exe or .com). A batch file, named, not there, behind a trailing dot
// or a \., or found by PATHEXT for no extension, for one PATHEXT lacks or for an .exe not there, is
// refused with ErrBatch; a file with no extension, a .ps1, a .vbs, a .batx, an .exe not there that
// PATHEXT makes a .vbs, and the helper named without its .exe are refused with ErrNotProgram. Each
// wraps ErrRefused, with Code -1, and the file, which would make a file if run, makes none. The
// controls: the batch file run by cmd.exe, and the helper named with an upper-case .EXE, each run.
// PATHEXT is set to the default four and .VBS and .PS1, so each row resolves as it means to.
//
// What it costs when this fails: Windows runs a batch file through cmd.exe, and a script through a
// program of its own, each reading Args by its own rules, so an Arg such as &calc runs a second
// command (CVE-2024-24576's class); or a program Run should start is refused.
func TestOnWindowsOnlyAProgramFileIsStarted(t *testing.T) {
	// serial: t.Setenv forbids t.Parallel, and PATHEXT, which it sets, is read by every lookup in the
	// process. testkit leaves it unset, so the rows name the extensions they resolve by.
	t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD;.VBS;.PS1")
	// script writes a file named name holding a batch file's text, in a folder of its own, that
	// makes the file mark when cmd.exe runs it.
	script := func(name string) (dir, mark string) {
		dir = t.TempDir()
		mark = filepath.Join(dir, "started")
		if err := os.WriteFile(filepath.Join(dir, name), []byte("@echo started> \""+mark+"\"\r\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir, mark
	}
	// refused runs c and wants it refused with want, and mark not made.
	refused := func(name string, c Cmd, mark string, want error) {
		t.Helper()
		res, err := Run(t.Context(), c)
		if !errors.Is(err, want) || !errors.Is(err, ErrRefused) || res.Code != -1 {
			t.Errorf("%s: %v, Code %d, want %v and Code -1", name, err, res.Code, want)
		}
		if _, err := os.Stat(mark); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: the file ran (%v), want nothing started", name, err)
		}
	}
	rows := []struct {
		name, file, path string
		want             error
	}{
		{"a .bat", "x.bat", "x.bat", ErrBatch},
		{"a .CMD", "X.CMD", "X.CMD", ErrBatch},
		{"a .bat with a trailing dot", "x.bat", "x.bat.", ErrBatch},
		{"a .bat with a \\. after it", "x.bat", `x.bat\.`, ErrBatch},
		{"a .bat that is not there", "x.bat", "absent.bat", ErrBatch},
		{"no extension, which PATHEXT makes a .bat", "build.bat", "build", ErrBatch},
		{"no extension, which PATHEXT makes a .cmd", "tool.cmd", "tool", ErrBatch},
		{"an extension PATHEXT lacks, which it makes a .bat", "build.txt.bat", "build.txt", ErrBatch},
		{"an .exe not there, which PATHEXT makes a .bat", "a.exe.bat", "a.exe", ErrBatch},
		{"a file with no extension", "prog", "prog", ErrNotProgram},
		{"a .ps1", "x.ps1", "x.ps1", ErrNotProgram},
		{"a .vbs", "x.vbs", "x.vbs", ErrNotProgram},
		{"a .batx", "x.batx", "x.batx", ErrNotProgram},
		{"an .exe not there, which PATHEXT makes a .vbs", "a.exe.vbs", "a.exe", ErrNotProgram},
	}
	for _, r := range rows {
		dir, mark := script(r.file)
		c := helperCmd(t, "touch", mark)
		c.Path, c.Args = filepath.Join(dir, r.path), []string{"&echo", "second"}
		refused(r.name, c, mark, r.want)
	}
	helper := func() (Cmd, string, string) {
		mark := filepath.Join(t.TempDir(), "started")
		c := helperCmd(t, "touch", mark)
		bare, ok := strings.CutSuffix(c.Path, filepath.Ext(c.Path))
		if !ok || !strings.EqualFold(filepath.Ext(c.Path), ".exe") {
			t.Fatalf("the helper is %s, want an .exe", c.Path)
		}
		return c, bare, mark
	}
	c, bare, mark := helper()
	c.Path = bare
	refused("the helper named without its .exe", c, mark, ErrNotProgram)

	c, bare, mark = helper()
	c.Path = bare + ".EXE"
	if res, err := Run(t.Context(), c); err != nil || res.Code != 0 {
		t.Fatalf("the control, the helper named with .EXE: %v, Code %d, stderr %q", err, res.Code, res.Stderr)
	}
	if _, err := os.Stat(mark); err != nil {
		t.Errorf("the helper named with .EXE made no file: %v", err)
	}
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		t.Fatal(err)
	}
	dir, mark := script("x.bat")
	c = helperCmd(t, "touch", mark)
	c.Path, c.Args = filepath.Join(sys, "cmd.exe"), []string{"/d", "/c", filepath.Join(dir, "x.bat")}
	if res, err := Run(t.Context(), c); err != nil || res.Code != 0 {
		t.Fatalf("the control, cmd.exe running the batch file: %v, Code %d, stderr %q", err, res.Code, res.Stderr)
	}
	if _, err := os.Stat(mark); err != nil {
		t.Errorf("the control made no file: %v", err)
	}
	if len(rows) != 14 {
		t.Errorf("%d rows", len(rows))
	}
}

// TestOnWindowsARelativePathIsCheckedOnTheFileItResolvesToUnderDir: a relative Path, of Run and of
// Start alike, is checked against the allow list on the file it resolves to under Dir, PATHEXT
// included, as exec starts it. Each row and each control below is run by Run, then started by Start.
// A .bat or a .cmd so named, given or found by PATHEXT, from Dir, from a folder below Dir by ..\,
// and from this process's folder with Dir empty, is refused with ErrBatch; where PATHEXT finds it
// under Dir, this process's folder holds a program file by the same name. A drive with no root
// after it (C:x.exe) and a root with no drive before it (\x.exe, /x.exe) are refused with
// errNotUnderDir, since Windows resolves each against a current folder of this process's; and the
// helper under Dir named without its .exe is refused with ErrNotProgram, as an absolute Path is,
// though PATHEXT finds it. Each wraps ErrRefused, with Code -1 from Run and no tree from Start, and
// starts nothing. The controls:
// the helper, placed under Dir and named by a relative path with its .exe, from Dir and from a
// folder below by ..\, runs, and so does the helper in this process's folder with Dir empty.
//
// What it costs when this fails: a relative Path reaches cmd.exe, which reads Args by its own rules
// (CVE-2024-24576's class), because the check read a file of this folder's and exec started Dir's;
// or a program file under Dir is refused for a batch file elsewhere; or a detached start, which
// outlives every later check, reaches cmd.exe where Run is refused.
func TestOnWindowsARelativePathIsCheckedOnTheFileItResolvesToUnderDir(t *testing.T) {
	// serial: t.Setenv forbids t.Parallel, PATHEXT is read by every lookup in the process, and
	// t.Chdir changes the folder of the whole process.
	t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD;.VBS;.PS1")
	// batchAt writes a batch file at name under dir that makes the file mark when cmd.exe runs it.
	batchAt := func(dir, name, mark string) {
		to := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(to, []byte("@echo started> \""+mark+"\"\r\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// here is this process's folder for the test. It holds sub\build.exe and sub\tool.exe, program
	// files a check that read this folder, not Dir, would pass.
	here := t.TempDir()
	placed(t, here, filepath.Join("sub", "build"))
	placed(t, here, filepath.Join("sub", "tool"))
	t.Chdir(here)
	rows := []struct {
		name, dir, file, path string
		want                  error
	}{
		{"a .bat under Dir", "root", `sub\x.bat`, `sub\x.bat`, ErrBatch},
		{"a .cmd under Dir, with .\\ before it", "root", `sub\x.cmd`, `.\sub\x.cmd`, ErrBatch},
		{"a .bat under Dir, with / for the separator", "root", `sub\x.bat`, "sub/x.bat", ErrBatch},
		{"no extension, which PATHEXT makes a .bat under Dir", "root", `sub\build.bat`, `sub\build`, ErrBatch},
		{"no extension, which PATHEXT makes a .cmd under Dir", "root", `sub\tool.cmd`, `sub\tool`, ErrBatch},
		{"..\\ from a folder below Dir, to a .bat", "below", `sub\x.bat`, `..\sub\x.bat`, ErrBatch},
		{"..\\ from a folder below Dir, which PATHEXT makes a .cmd", "below", `sub\tool.cmd`, `..\sub\tool`, ErrBatch},
		{"a .bat in this folder, with Dir empty", "", `sub\here.bat`, `sub\here.bat`, ErrBatch},
		{"no extension, which PATHEXT makes a .cmd in this folder, with Dir empty", "", `sub\cwd.cmd`, `sub\cwd`, ErrBatch},
		{"a drive with no root after it", "root", `x.exe`, `C:x.exe`, errNotUnderDir},
		{"a drive with no root after it, and a separator", "root", `sub\x.exe`, `C:sub\x.exe`, errNotUnderDir},
		{"a root with no drive before it", "root", `x.exe`, `\x.exe`, errNotUnderDir},
		{"a root with no drive before it, by /", "root", `x.exe`, `/x.exe`, errNotUnderDir},
		{"the helper under Dir, named without its .exe", "root", "", `sub\helper`, ErrNotProgram},
	}
	for _, r := range rows {
		mark := filepath.Join(t.TempDir(), "started")
		root := t.TempDir()
		c := helperCmd(t, "touch", mark)
		c.Path, c.Args = r.path, []string{"&echo", "second"}
		switch r.dir {
		case "root":
			c.Dir = root
		case "below":
			c.Dir = filepath.Join(root, "below")
			if err := os.Mkdir(c.Dir, 0o700); err != nil {
				t.Fatal(err)
			}
		default:
			c.Dir, root = "", here
		}
		if r.file == "" {
			placed(t, root, filepath.Join("sub", "helper"))
		} else {
			batchAt(root, r.file, mark)
		}
		refusedHelper(t, r.name, c, mark, r.want)
		refusedStart(t, r.name, Detached{Path: c.Path, Dir: c.Dir, Args: c.Args}, mark, r.want)
	}
	controls := []struct{ name, dir, path string }{
		{"the helper under Dir", "", `sub\helper.exe`},
		{"the helper under Dir, with .\\ before it", "", `.\sub\helper.exe`},
		{"the helper under Dir, with / for the separator", "", "./sub/helper.exe"},
		{"the helper by ..\\ from a folder below Dir", "below", `..\sub\helper.exe`},
	}
	for _, r := range controls {
		mark := filepath.Join(t.TempDir(), "started")
		root := t.TempDir()
		placed(t, root, filepath.Join("sub", "helper"))
		c := helperCmd(t, "touch", mark)
		c.Path, c.Dir = r.path, filepath.Join(root, r.dir)
		if err := os.MkdirAll(c.Dir, 0o700); err != nil {
			t.Fatal(err)
		}
		ranHelper(t, r.name, c, mark)
		started := filepath.Join(t.TempDir(), "started")
		startedHelper(t, r.name, touching(t, c.Path, c.Dir, started), started)
	}
	mark := filepath.Join(t.TempDir(), "started")
	placed(t, here, filepath.Join("sub", "own"))
	c := helperCmd(t, "touch", mark)
	c.Path, c.Dir = `sub\own.exe`, ""
	ranHelper(t, "the helper in this folder, with Dir empty", c, mark)
	started := filepath.Join(t.TempDir(), "started")
	startedHelper(t, "the helper in this folder, with Dir empty", touching(t, c.Path, "", started), started)
	if len(rows) != 14 || len(controls) != 4 {
		t.Errorf("%d rows, %d controls", len(rows), len(controls))
	}
}

// TestAConsoleHostThatHasEndedWhileHeldIsGone: consoleHost takes a process that has ended, while
// another handle to it is held, for errGone, as it takes a pid no process has: its image cannot be
// read then, and jobLeft waits for no process gone and reads the count again. The control: a
// process that runs and is not the console host is errNoConsoleHost still, never errGone, and
// hasEnded says it has not ended until it has. exiting, which lets a process whose image cannot be
// read be waited for, says the same of the running helper's exit code: not set while it runs, set
// once it has ended.
//
// What it costs when this fails: a run whose program starts nothing reads Left true when its
// console host ends between the job's listing of it and its opening, as a loaded machine has it; or, for the control, a console host that runs is taken for gone and not waited
// for, so Left reads true at once for a tree whose host would have ended within the wait. When
// exiting says a running process has begun to exit, a live process whose image cannot be read is
// waited for, not counted as left at once; when it never says so, a process mid-exit is left.
func TestAConsoleHostThatHasEndedWhileHeldIsGone(t *testing.T) {
	t.Parallel()
	start := func(mode string, args ...string) *process {
		c := helperCmd(t, mode, args...)
		p := &process{Path: c.Path, Args: append([]string{c.Path}, c.Args...), Env: c.Env}
		if err := startTestProcess(p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	hosted := func(pid int) error {
		h, err := consoleHost(pid)
		if err == nil {
			err = windows.CloseHandle(h)
		}
		return err
	}
	running := start("hold")
	watch, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(running.Process.Pid))
	if err != nil {
		_ = running.Process.Kill()
		_ = running.Wait()
		t.Fatal(err)
	}
	err = hosted(running.Process.Pid)
	endedWhileRunning, exitingWhileRunning := hasEnded(watch), exiting(watch)
	if killErr := running.Process.Kill(); killErr != nil {
		t.Error(killErr)
	}
	_ = running.Wait()
	endedOnceReaped, exitingOnceReaped := hasEnded(watch), exiting(watch)
	if closeErr := windows.CloseHandle(watch); closeErr != nil {
		t.Error(closeErr)
	}
	if !errors.Is(err, errNoConsoleHost) || errors.Is(err, errGone) {
		t.Errorf("consoleHost of a running helper: %v, want errNoConsoleHost alone", err)
	}
	if endedWhileRunning || !endedOnceReaped {
		t.Errorf("hasEnded of a helper: %v while it ran, %v once reaped, want false then true", endedWhileRunning, endedOnceReaped)
	}
	if exitingWhileRunning || !exitingOnceReaped {
		t.Errorf("exiting of a helper: %v while it ran, %v once reaped, want false then true", exitingWhileRunning, exitingOnceReaped)
	}
	p := start("write", "0", "0", "exit")
	held, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(p.Process.Pid))
	if err != nil {
		_ = p.Wait()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := windows.CloseHandle(held); err != nil {
			t.Error(err)
		}
	})
	if err := p.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := hosted(p.Process.Pid); !errors.Is(err, errGone) {
		t.Errorf("consoleHost of a helper that has ended, a handle to it held: %v, want errGone", err)
	}
}
