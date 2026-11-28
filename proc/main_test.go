package proc

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/birdtable/go-testkit"
)

// helperMode is the first argument that makes the test binary a helper a test runs as a child,
// in place of the tests: "proc-helper:<mode>", then the mode's arguments.
const helperMode = "proc-helper:"

// holdFor is how long a holding helper lives unless it is killed, longer than any test.
const holdFor = 10 * time.Minute

// treeLimit is the TreeMemory a row holds a tree to: 64 MiB, and under -race raceMemory more, so a
// helper with the detector keeps the margin it has without it. A row whose point is the exact size
// of a limit, and a unit row of hold, keep their own sizes.
const treeLimit = 64<<20 + raceMemory

// inHelper is whether this process is the test binary started as a helper, not the tests.
func inHelper() bool {
	return len(os.Args) > 1 && strings.HasPrefix(os.Args[1], helperMode)
}

// TestMain is a helper when the binary was started as one, and otherwise starts the tests from the
// environment testkit.Main makes: no GIT_ variable, no setting of the person's, and the home, temp
// and XDG_ folders beneath one removed after the run. After the tests, no goroutine of the package or
// of os/exec may be left.
func TestMain(m *testing.M) {
	if inHelper() {
		code := helper(strings.TrimPrefix(os.Args[1], helperMode), os.Args[2:])
		if exitHeld() {
			syscall.Exit(code)
		}
		os.Exit(code)
	}
	// go-proc's policy: a go a test starts reads no workspace. testkit pins none, so it is set here and kept.
	if err := os.Setenv("GOWORK", "off"); err != nil {
		panic(err)
	}
	code, err := testkit.Main(m, testkit.Keep("GOWORK"))
	if err != nil {
		panic(err)
	}
	if left := settled(); len(left) > 0 {
		fmt.Fprintf(os.Stderr, "proc: %d goroutines of the package or of os/exec outlived the tests:\n\n%s\n", len(left), strings.Join(left, "\n\n"))
		code = 1
	}
	os.Exit(code)
}

// exitHeld is whether os.Exit would hold this helper a second: one started with neither GORACE nor
// GOCOVERDIR, as a test of exactly what a child is handed starts it. os.Exit's runtime hook runs the
// race detector's finish, which holds a process built with -race for atexit_sleep_ms, a second unless
// GORACE sets it to 0 as the tiers do. Such a helper ends by syscall.Exit instead, past every exit
// hook: with no GOCOVERDIR no coverage hook has a folder to write to, and the race reports it finds
// are printed as they are found; what it gives up is exit code 66 for one found.
func exitHeld() bool {
	_, race := os.LookupEnv("GORACE")
	_, cover := os.LookupEnv("GOCOVERDIR")
	return !race && !cover
}

// settled is leaked, once the goroutines on their way out have gone. A Cmd's watchCtx can still
// be runnable after its Wait has returned: its last send was handed to Wait, but its own return
// has not run yet. So settled looks again, as go.uber.org/goleak does, but it waits on no clock: it
// yields the processor between looks, so a runnable goroutine runs its return, and it looks at most
// settleLooks times. A goroutine that is blocked stays blocked through every yield, and is left.
func settled() []string {
	left := leaked()
	for range settleLooks {
		if len(left) == 0 {
			break
		}
		runtime.Gosched()
		left = leaked()
	}
	return left
}

// settleLooks is how many more times settled looks, each after a yield of the processor.
const settleLooks = 100

// leaked is each goroutine still running a frame of this package or of os/exec, but the one
// running TestMain.
func leaked() []string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	var left []string
	for g := range strings.SplitSeq(string(buf), "\n\n") {
		if strings.Contains(g, "proc.TestMain") {
			continue
		}
		if strings.Contains(g, "go-proc/proc.") || strings.Contains(g, "os/exec.") {
			left = append(left, g)
		}
	}
	return left
}

// helper runs the helper mode with its arguments and gives its exit code:
//
//   - argv: writes its own os.Args to stdout, each ended by a NUL.
//   - env: writes its environment to stdout, each ended by a NUL.
//   - path-split: writes each folder filepath.SplitList reads from its PATH to stdout, each ended by
//     a NUL.
//   - pwd: writes the folder it runs in to stdout.
//   - stdin: copies stdin to stdout.
//   - extra <n> <word>: writes word to the file this helper inherited as n, a descriptor on Unix
//     and a handle on Windows (Cmd.ExtraFiles).
//   - write <n> <m> <then>: writes n bytes to stdout and m to stderr, then exits 0, or holds.
//   - touch <path>: makes the file path.
//   - hold: lives holdFor, unless it is killed.
//   - refused-nil <dir>: stores nil in ErrBatch and ErrNotProgram, as a caller could, and exits 0
//     when the gate still refuses a .bat and a .ps1 under dir with an error that wraps ErrRefused.
//   - hog <mib>: allocates mib MiB, a MiB at a time, writing each page and keeping all of it, then
//     writes "done" and a newline.
//   - hog-child <mib>: starts hog <mib> with this helper's stdout and stderr, waits for it, and
//     exits as it did, holding little of its own.
//   - procs <n>: starts n holding helpers, one after another, writing "started " and the count of
//     each that starts, and "refused " and its count for the first that does not; then exits 0.
//   - nested <then>: starts a holding helper with its own stdout and stderr, writes its pid and a
//     newline to stdout, then exits 0, or holds.
//   - leave: starts a holding helper with none of its stdin, stdout or stderr, the system's null
//     device in their place, and exits 0 without waiting for it.
//   - tree <then> <binary>: a detached tree's leader: writes "console " and whether it has a console
//     or a controlling terminal, "none" or "some", then starts hold-while <binary> with its own
//     stdout and stderr, quietly (no console of its own), and writes "grandchild " and its pid, each
//     line ended by a newline; then exits 0, or holds while <binary> lives.
//   - detach <binary>: starts tree hold <binary> detached, with this helper's stdout as its output,
//     writes "id " and its tree id and a newline to stdout, and exits 0 without waiting for it.
//   - hold-while <binary>: lives while process <binary>, the test binary that started its tree,
//     lives, and holdFor at most.
//   - detach-hold <binary>: stands in for a test binary: starts tree hold <its own pid> detached,
//     with this helper's stdout as its output, writes "id " and its tree id and "leader " and the
//     leader's pid, then holds while <binary> lives.
//   - exit-code <n>: exits with code n, which may be above the largest int32 where the system's
//     exit codes are 32 bits wide, as an NTSTATUS value is.
//   - pid-hold: writes its own pid and a newline to stdout, then holds.
//
// A mode of one system is in that system's helpers.
func helper(mode string, args []string) int {
	switch {
	case mode == "argv":
		return wrote(os.Stdout.WriteString(strings.Join(os.Args, "\x00") + "\x00"))
	case mode == "env":
		return wrote(os.Stdout.WriteString(strings.Join(os.Environ(), "\x00") + "\x00"))
	case mode == "folders":
		return folders()
	case mode == "stdin":
		_, err := io.Copy(os.Stdout, os.Stdin)
		return wrote(0, err)
	case mode == "write" && len(args) == 3:
		n, errN := strconv.Atoi(args[0])
		m, errM := strconv.Atoi(args[1])
		if errN != nil || errM != nil {
			return 2
		}
		if code := wrote(os.Stdout.Write(bytes.Repeat([]byte("o"), n))); code != 0 {
			return code
		}
		if code := wrote(os.Stderr.Write(bytes.Repeat([]byte("e"), m))); code != 0 {
			return code
		}
		return then(args[2])
	case mode == "touch" && len(args) == 1:
		return wrote(0, os.WriteFile(args[0], nil, 0o600))
	case mode == "hold":
		return then("hold")
	case mode == "hog" && len(args) == 1:
		mib, err := strconv.Atoi(args[0])
		if err != nil {
			return 2
		}
		held := hog(mib)
		code := wrote(os.Stdout.WriteString("done\n"))
		runtime.KeepAlive(held)
		return code
	case mode == "hog-child" && len(args) == 1:
		child, err := helping("hog", args[0])
		if err != nil {
			return wrote(0, err)
		}
		if err := child.Wait(); err != nil {
			return wrote(0, err)
		}
		return 0
	case mode == "procs" && len(args) == 1:
		n, err := strconv.Atoi(args[0])
		if err != nil {
			return 2
		}
		return procs(n)
	case mode == "nested" && len(args) == 1:
		child, err := helping("hold")
		if err != nil {
			return wrote(0, err)
		}
		if code := wrote(fmt.Fprintf(os.Stdout, "%d\n", child.Process.Pid)); code != 0 {
			return code
		}
		return then(args[0])
	}
	// Each further mode is a function that says whether mode and args are its own, so a mode added
	// to this list costs helper no branch.
	for _, run := range []func(string, []string) (int, bool){refusedNil, treeModes, extraMode, endModes, pwdMode, pathSplitMode, systemEnvMode, systemHelper} {
		if code, ok := run(mode, args); ok {
			return code
		}
	}
	fmt.Fprintf(os.Stderr, "proc: no helper mode %q with %d arguments\n", mode, len(args))
	return 2
}

// pwdMode runs the pwd mode, which writes the folder it runs in, and says whether mode and args are
// it.
func pwdMode(mode string, args []string) (int, bool) {
	if mode != "pwd" || len(args) != 0 {
		return 0, false
	}
	dir, err := os.Getwd()
	if err != nil {
		return wrote(0, err), true
	}
	return wrote(os.Stdout.WriteString(dir)), true
}

// pathSplitMode runs the path-split mode, which writes each folder filepath.SplitList reads from its
// PATH, and says whether mode and args are it.
func pathSplitMode(mode string, args []string) (int, bool) {
	if mode != "path-split" || len(args) != 0 {
		return 0, false
	}
	var b strings.Builder
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		b.WriteString(dir + "\x00")
	}
	return wrote(os.Stdout.WriteString(b.String())), true
}

// endModes runs the exit-code and pid-hold modes, and says whether mode and args are one of them.
func endModes(mode string, args []string) (int, bool) {
	switch {
	case mode == "exit-code" && len(args) == 1:
		code, err := strconv.ParseUint(args[0], 10, 32)
		if err != nil {
			return 2, true
		}
		return int(code), true
	case mode == "pid-hold" && len(args) == 0:
		if code := wrote(fmt.Fprintf(os.Stdout, "%d\n", os.Getpid())); code != 0 {
			return code, true
		}
		return then("hold"), true
	}
	return 0, false
}

// extraMode runs the extra mode, and says whether mode and args are it.
func extraMode(mode string, args []string) (int, bool) {
	if mode != "extra" || len(args) != 2 {
		return 0, false
	}
	n, err := strconv.ParseUint(args[0], 10, 64)
	if err != nil {
		return 2, true
	}
	return wrote(os.NewFile(uintptr(n), "extra").WriteString(args[1])), true
}

// helping starts the test binary as a helper of mode, with this helper's stdout and stderr and no
// stdin, and leaves it running.
func helping(mode string, args ...string) (*process, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	child := &process{Path: exe, Args: slices.Concat([]string{exe, helperMode + mode}, args), Stdout: os.Stdout, Stderr: os.Stderr}
	return child, startTestProcess(child)
}

// hog allocates mib MiB, a MiB at a time, writes each page of it, and gives all of it, so none is
// freed before the hog is done.
func hog(mib int) [][]byte {
	var hogged [][]byte
	for range mib {
		b := make([]byte, 1<<20)
		for i := 0; i < len(b); i += 4096 {
			b[i] = 1
		}
		hogged = append(hogged, b)
	}
	return hogged
}

// procs starts n holding helpers, as the procs mode says.
func procs(n int) int {
	for i := 1; i <= n; i++ {
		if _, err := helping("hold"); err != nil {
			return wrote(fmt.Fprintf(os.Stdout, "refused %d\n", i))
		}
		if code := wrote(fmt.Fprintf(os.Stdout, "started %d\n", i)); code != 0 {
			return code
		}
	}
	return 0
}

// then ends a helper: "exit" exits 0, and "hold" lives holdFor.
func then(how string) int {
	if how == "hold" {
		<-time.NewTimer(holdFor).C
	}
	return 0
}

// wrote is a helper's exit code after a write: 0, or 1 with the error said.
func wrote(_ int, err error) int {
	if err != nil {
		fmt.Fprintln(os.Stderr, "proc helper:", err)
		return 1
	}
	return 0
}

// folders is the helper mode that prints, each ended by a NUL, the folders os reads from the
// environment: the home, the configuration folder, the cache folder and the temp folder.
func folders() int {
	home, errHome := os.UserHomeDir()
	config, errConfig := os.UserConfigDir()
	cache, errCache := os.UserCacheDir()
	for _, err := range []error{errHome, errConfig, errCache} {
		if err != nil {
			return wrote(os.Stderr.WriteString(err.Error()))
		}
	}
	return wrote(os.Stdout.WriteString(strings.Join([]string{home, config, cache, os.TempDir()}, "\x00") + "\x00"))
}

// systemEnvMode runs the system-env mode, and says whether mode and args are it: it prints
// SystemEnv as the helper was started, then again once it has unset each system name, a line each:
// "nil" for a nil list, or "list:" and its entries, each ended by a NUL.
func systemEnvMode(mode string, args []string) (int, bool) {
	if mode != "system-env" || len(args) != 0 {
		return 0, false
	}
	shown := func(env []string) string {
		if env == nil {
			return "nil\n"
		}
		var b strings.Builder
		b.WriteString("list:")
		for _, kv := range env {
			b.WriteString(kv + "\x00")
		}
		return b.String() + "\n"
	}
	before := shown(SystemEnv())
	for _, name := range systemNames() {
		if err := os.Unsetenv(name); err != nil {
			return wrote(0, err), true
		}
	}
	return wrote(os.Stdout.WriteString(before + shown(SystemEnv()))), true
}

// TestTheTestsRunUnderTheIsolateOfTestkitMain: the process's HOME passes testkit.Clean, which keeps
// it only while an Isolate is in force, so TestMain calls testkit.Main.
//
// What it costs when this fails: TestMain no longer calls testkit.Main, and each child a test starts
// inherits the environment of whoever ran go test: its tokens, its GIT_ variables and its home.
func TestTheTestsRunUnderTheIsolateOfTestkitMain(t *testing.T) {
	t.Parallel()
	if home := []string{"HOME=" + os.Getenv("HOME")}; !slices.Equal(testkit.Clean(home), home) {
		t.Fatalf("testkit.Clean drops %q, so no Isolate is in force: TestMain does not call testkit.Main", home)
	}
}

// within runs f, and fails the test when f has not returned with most of the
// test's time gone, rather than leave go test's own deadline to stop it.
func within(t *testing.T, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	var late <-chan time.Time
	if deadline, ok := t.Deadline(); ok {
		late = time.After(time.Until(deadline) * 9 / 10)
	}
	select {
	case <-done:
	case <-late:
		t.Fatalf("%s: not done with the test's deadline near", what)
	}
}

// refusedNil runs the refused-nil mode, and says whether mode and args are it: 0 when the gate
// refuses a .bat and a .ps1 under the folder args names, each with an error that wraps ErrRefused,
// after nil is stored in ErrBatch and ErrNotProgram.
func refusedNil(mode string, args []string) (int, bool) {
	if mode != "refused-nil" || len(args) != 1 {
		return 0, false
	}
	ErrBatch, ErrNotProgram = nil, nil
	code := 0
	for _, name := range []string{"absent.bat", "absent.ps1"} {
		if err := unstartable(filepath.Join(args[0], name), ""); !errors.Is(err, ErrRefused) {
			fmt.Fprintf(os.Stderr, "%s: %v, want a refusal\n", name, err)
			code = 1
		}
	}
	return code, true
}

// treeModes runs the helper modes of a detached tree, tree, detach, hold-while and detach-hold, as
// helper's list says, and says whether mode is one.
func treeModes(mode string, args []string) (int, bool) {
	switch {
	case mode == "tree" && len(args) == 2:
		return treeLeader(args[0], args[1]), true
	case mode == "detach" && len(args) == 1:
		return detachTree(args[0]), true
	case mode == "hold-while" && len(args) == 1:
		return holdWhile(args[0]), true
	case mode == "detach-hold" && len(args) == 1:
		return detachHold(args[0]), true
	case mode == "leave" && len(args) == 0:
		return leave(), true
	}
	return 0, false
}

// leave is the leave helper: it starts a holding helper with none of its stdin, stdout or stderr,
// and exits 0 without waiting for it.
func leave() int {
	exe, err := os.Executable()
	if err != nil {
		return wrote(0, err)
	}
	return wrote(0, startTestProcess(&process{Path: exe, Args: []string{exe, helperMode + "hold"}}))
}

// treeLeader is the tree helper. binary is the pid of the test binary that started the tree.
func treeLeader(how, binary string) int {
	word := "none"
	if console() {
		word = "some"
	}
	if code := wrote(fmt.Fprintf(os.Stdout, "console %s\n", word)); code != 0 {
		return code
	}
	exe, err := os.Executable()
	if err != nil {
		return wrote(0, err)
	}
	child := &process{Path: exe, Args: []string{exe, helperMode + "hold-while", binary}, Stdout: os.Stdout, Stderr: os.Stderr}
	quiet(child)
	if err := startTestProcess(child); err != nil {
		return wrote(0, err)
	}
	if code := wrote(fmt.Fprintf(os.Stdout, "grandchild %d\n", child.Process.Pid)); code != 0 {
		return code
	}
	if how == "hold" {
		return holdWhile(binary)
	}
	return then(how)
}

// detachTree is the detach helper. binary is the pid of the test binary that started this helper.
func detachTree(binary string) int {
	exe, err := os.Executable()
	if err != nil {
		return wrote(0, err)
	}
	tree, err := Start(Detached{Path: exe, Args: []string{helperMode + "tree", "hold", binary}, Output: os.Stdout})
	if err != nil {
		return wrote(0, err)
	}
	return wrote(fmt.Fprintf(os.Stdout, "id %s\n", tree.ID))
}

// holdWhile is a detached tree's holder: it lives as then("hold") does, but ends within moments of
// the end of process binary, the test binary that started its tree, rather than at holdFor. A tree
// Start detaches is out of that binary's process group and job by design, which is what Detached is
// for, so its holders stop themselves (a load stops
// itself). A binary that cannot be watched ends the holder at once, rather than leave it running.
func holdWhile(binary string) int {
	pid, err := strconv.Atoi(binary)
	if err != nil {
		return wrote(0, fmt.Errorf("the test binary's pid %q: %w", binary, err))
	}
	end, err := processEnd(pid)
	if err != nil {
		return wrote(0, err)
	}
	upper := time.NewTimer(holdFor)
	defer upper.Stop()
	select {
	case err := <-end:
		return wrote(0, err)
	case <-upper.C:
		return 0
	}
}

// detachHold is the detach-hold helper: a stand-in for a test binary, which the test kills while the
// tree it detached holds. binary is the pid of the real test binary, so a stand-in the test does not
// kill ends with it.
func detachHold(binary string) int {
	exe, err := os.Executable()
	if err != nil {
		return wrote(0, err)
	}
	tree, err := Start(Detached{Path: exe, Args: []string{helperMode + "tree", "hold", strconv.Itoa(os.Getpid())}, Output: os.Stdout})
	if err != nil {
		return wrote(0, err)
	}
	if code := wrote(fmt.Fprintf(os.Stdout, "id %s\nleader %d\n", tree.ID, tree.Leader.Pid)); code != 0 {
		return code
	}
	return holdWhile(binary)
}
