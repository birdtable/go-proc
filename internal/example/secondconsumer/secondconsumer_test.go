package secondconsumer_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/birdtable/go-proc/gitenv"
	"github.com/birdtable/go-proc/gitenv/gittest"
	"github.com/birdtable/go-proc/load"
	"github.com/birdtable/go-proc/proc"
	"github.com/birdtable/go-proc/sandbox"
	"github.com/birdtable/go-proc/service"
	"github.com/birdtable/go-testkit"
)

// The second consumer's own values, each unlike what go-proc's own tests and Taskfile use.
const (
	helperArg   = "secondconsumer-helper" // the first argument that makes the test binary a helper
	region      = "SC_REGION"             // a variable of the consumer's, which its allow-list keeps
	regionValue = "eu-north-7"
	dropped     = "GOTRACEBACK" // a variable testkit's fixed list keeps, which the consumer drops
	runTimeout  = 7 * time.Second
	runCap      = 1500 // bytes of each stream a run keeps
	runGrace    = 150 * time.Millisecond
	runDrain    = 900 * time.Millisecond
	rerunWithin = 40 * time.Second // a Rerun child's deadline, by the consumer's ctx
	rerunCap    = 3000             // bytes of each stream a Rerun child keeps
	authorName  = "Second Consumer Builds"
	authorEmail = "builds@second-consumer.invalid"
	unitBase    = "second-consumer-sweep"
)

// workspace is the go.work the consumer's builds would read, a path of its own beside its tests,
// which nothing here opens: GOWORK is the caller's, and go-proc's own is off.
func workspace() string {
	wd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	return filepath.Join(wd, "testdata", "second-consumer.work")
}

// system is the system the example runs on, which decides what the consumer asks each package for.
func system() string {
	return runtime.GOOS //nolint:forbidigo // arch.ports.1: the consumer asks each package for what its system has, as a host layer does
}

// environ is the environment this process has: in a test, what testkit.Main made of the
// consumer's allow-list; in a helper, what its run gave it.
func environ() []string {
	return os.Environ()
}

// getenv is environ's value for name, empty when it is unset.
func getenv(name string) string {
	for _, kv := range environ() {
		if k, v, _ := strings.Cut(kv, "="); k == name {
			return v
		}
	}
	return ""
}

// say writes s to a helper's stdout, which the run that started it reads.
func say(s string) {
	fmt.Fprint(os.Stdout, s) //nolint:forbidigo // cli.errors.3: a helper's stdout is the run's output, which the test reads
}

// TestMain is a helper when the binary was started as one, and otherwise starts the tests under the
// consumer's own allow-list: its workspace and region kept, GOTRACEBACK dropped. A Rerun child sets
// nothing, so what it sees is what Rerun handed it.
func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == helperArg {
		os.Exit(helper(os.Args[2], os.Args[3:]))
	}
	if _, child := testkit.Helper(); !child {
		for k, v := range map[string]string{"GOWORK": workspace(), region: regionValue, dropped: "system"} {
			if err := os.Setenv(k, v); err != nil {
				panic(err)
			}
		}
	}
	code, err := testkit.Main(m, testkit.Keep("GOWORK", region), testkit.Drop(dropped))
	if err != nil {
		panic(err)
	}
	os.Exit(code)
}

// helper is what the test binary does when a test starts it as a helper: env prints its environment,
// sorted, a line each; flood writes past any cap of the consumer's; hold waits to be ended; write
// writes a file in each of the folders it is given and says which writes the system refused.
func helper(mode string, args []string) int {
	switch mode {
	case "env":
		env := environ()
		slices.Sort(env)
		say(strings.Join(env, "\n") + "\n")
	case "flood":
		say(strings.Repeat("x", 64<<10))
	case "hold":
		<-time.After(time.Minute) // the run's kill ends it first; the minute ends one nothing killed
	case "write":
		for _, dir := range args {
			err := os.WriteFile(filepath.Join(dir, "written"), []byte("second\n"), 0o600)
			say(fmt.Sprintf("%s %t\n", filepath.Base(dir), err == nil))
		}
	case "sandbox":
		exe, err := os.Executable()
		if err != nil {
			say(fmt.Sprintln(err))
			return 3
		}
		// The system sandbox matches a file by its real path, so each folder goes by its own.
		real := func(p string) string { r, _ := filepath.EvalSymlinks(p); return r }
		// The folders its loader and libraries are in, which a test binary built with cgo reads to
		// start, as the race detector's is, are RunHolding's to add: the consumer names none.
		f := sandbox.Folders{ReadWrite: []string{real(args[0])}, ReadOnly: []string{real(filepath.Dir(exe))}}
		err = sandbox.RunHolding(f, real(exe), []string{helperArg, "write", real(args[0]), real(args[1])}, consumerEnv())
		say(fmt.Sprintln(err)) // RunHolding returns only when the program did not run
		return 3
	default:
		return 2
	}
	return 0
}

// consumerEnv is the consumer's whole environment for a program it runs: two variables of its own,
// and on Windows the system root, which a Windows program needs to start.
func consumerEnv() []string {
	env := []string{region + "=" + regionValue, "SC_TIER=batch"}
	if system() == "windows" {
		env = append(env, "SYSTEMROOT="+getenv("SYSTEMROOT"))
	}
	slices.Sort(env)
	return env
}

// self is the test binary, which a run starts as a helper.
func self(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// grace is the consumer's Grace on the system it runs on: Windows sends no TERM, so the consumer
// asks for none there, where proc refuses one.
func grace() time.Duration {
	if system() == "windows" {
		return 0
	}
	return runGrace
}

// cmd is a Cmd of the consumer's for the helper mode and its arguments: every bound its own.
func cmd(t *testing.T, mode string, args ...string) proc.Cmd {
	t.Helper()
	return proc.Cmd{
		Path: self(t), Args: append([]string{helperArg, mode}, args...), Dir: t.TempDir(), Env: consumerEnv(),
		Timeout: runTimeout, MaxOutput: runCap, Grace: grace(), Drain: runDrain, Terminal: true,
	}
}

// ended is whether err is a run ended for why.
func ended(err error, why string) bool {
	var e proc.Ended
	return errors.As(err, &e) && e.Why == why
}

// lines is out's lines, a trailing line end aside.
func lines(out []byte) []string {
	return strings.Split(strings.TrimRight(string(out), "\r\n"), "\n")
}

// A run's program gets the consumer's environment whole, and nothing of Bare's or of this process's.
//
// What it costs when this fails: proc fills in variables of its own choosing, so a consumer whose
// allow-list differs hands its programs names it never chose.
func TestARunGivesTheProgramTheConsumersEnvironmentAlone(t *testing.T) {
	t.Parallel()
	res, err := proc.Run(t.Context(), cmd(t, "env"))
	if err != nil || res.Code != 0 {
		t.Fatalf("a run of the consumer's: %v, Code %d, stderr %q", err, res.Code, res.Stderr)
	}
	if got := lines(res.Stdout); !slices.Equal(got, consumerEnv()) {
		t.Errorf("the program's environment is %q, want the consumer's %q", got, consumerEnv())
	}
}

// A run is held to the consumer's own bounds, and a bound it left out is refused, never filled in:
// that refusal is the consumer's to handle, as a configuration of its own that is wrong.
//
// What it costs when this fails: proc runs on a timeout, a cap or a drain of ours, so a consumer
// that forgot one never learns it, and its runs take our bounds.
func TestARunTakesTheConsumersBoundsAndRefusesOneLeftOut(t *testing.T) {
	t.Parallel()
	for _, r := range []struct {
		name string
		edit func(*proc.Cmd)
	}{
		{"no timeout", func(c *proc.Cmd) { c.Timeout = 0 }},
		{"no output cap", func(c *proc.Cmd) { c.MaxOutput = 0 }},
		{"a negative drain", func(c *proc.Cmd) { c.Drain = -time.Second }},
	} {
		c := cmd(t, "env")
		r.edit(&c)
		if _, err := proc.Run(t.Context(), c); !errors.Is(err, proc.ErrRefused) {
			t.Errorf("a run with %s: %v, want the consumer told by ErrRefused", r.name, err)
		}
	}
	res, err := proc.Run(t.Context(), cmd(t, "flood"))
	if !ended(err, proc.WhyCap) || len(res.Stdout) != runCap {
		t.Errorf("a run that writes past the consumer's cap of %d: %v, kept %d bytes, want WhyCap and %d", runCap, err, len(res.Stdout), runCap)
	}
	c := cmd(t, "hold")
	c.Timeout = 300 * time.Millisecond
	start := time.Now()
	_, err = proc.Run(t.Context(), c)
	if took := time.Since(start); !ended(err, proc.WhyDeadline) || took > c.Timeout+grace()+runDrain+5*time.Second {
		t.Errorf("a run past the consumer's timeout of %v: %v after %v, want WhyDeadline within its own bounds", c.Timeout, err, took)
	}
}

// A Grace and a zero Drain are held on every system the consumer runs on, Windows among them: with a
// terminal and a console, as cmd gives, Grace sends the break before the kill on Windows
// and a zero Drain waits for no pipe, as off Windows, so each run gives the consumer's environment
// whole. A zero Drain's own cost on Unix, a run escaped from a pipe another start still holds, is the
// consumer's to take.
//
// What it costs when this fails: proc puts a floor of its own under the consumer's Drain, or refuses
// a Grace a terminal and console allow, or a zero Drain cuts the consumer's output.
func TestAGraceAndAZeroDrainAreTheConsumersWhereTheSystemHoldsThem(t *testing.T) {
	t.Parallel()
	for _, r := range []struct {
		name string
		edit func(*proc.Cmd)
	}{
		{"the consumer's Grace", func(c *proc.Cmd) { c.Grace = runGrace }},
		{"a zero Drain", func(c *proc.Cmd) { c.Drain = 0 }},
	} {
		c := cmd(t, "env")
		r.edit(&c)
		res, err := proc.Run(t.Context(), c)
		// A zero Drain's cost is the consumer's to take: on Unix a copy of the run's pipes another start
		// holds from its fork to its exec ends the run escaped, never refused; Windows forks none.
		if c.Drain == 0 && ended(err, proc.WhyEscaped) {
			continue
		}
		if err != nil || res.Code != 0 || !slices.Equal(lines(res.Stdout), consumerEnv()) {
			t.Errorf("a run with %s on %s: %v, Code %d, wrote %q, want the consumer's environment whole", r.name, system(), err, res.Code, res.Stdout)
		}
	}
}

// The consumer sizes its machines by a run's peak, kept by what the peak counts: a reading names its
// kind, one of the closed list and the same for every run on one system, and no reading names none.
//
// What it costs when this fails: a consumer that keeps peaks from several systems compares one
// process's resident set with a whole tree's charge, and sizes by figures that do not compare.
func TestAPeakNamesWhatItCountsForTheConsumersSizing(t *testing.T) {
	t.Parallel()
	known := []proc.PeakKind{proc.PeakTreeCharged, proc.PeakProcessResident, proc.PeakProcessCommitted}
	sizes := map[proc.PeakKind]int64{}
	for range 2 {
		c := cmd(t, "env")
		c.ReadPeak = true
		res, err := proc.Run(t.Context(), c)
		switch {
		case err != nil || res.Code != 0:
			t.Fatalf("a run of the consumer's that reads its peak: %v, Code %d, stderr %q", err, res.Code, res.Stderr)
		case !res.PeakRead && res.PeakKind != proc.PeakNone:
			t.Errorf("a run that read no peak names its kind %q, want none", res.PeakKind)
		case res.PeakRead && !slices.Contains(known, res.PeakKind):
			t.Errorf("a peak of %d bytes names its kind %q, want one of %q", res.Peak, res.PeakKind, known)
		case res.PeakRead:
			sizes[res.PeakKind] = max(sizes[res.PeakKind], res.Peak)
		}
	}
	if len(sizes) > 1 {
		t.Errorf("two runs on one system read peaks of kinds %v, want one kind", sizes)
	}
}

// A detached program gets the consumer's environment whole and writes to the consumer's file; once it
// has ended, Stop is ErrStale, which the consumer handles as a tree already gone.
//
// What it costs when this fails: Start fills in an environment or an output of ours.
func TestADetachedProgramTakesTheConsumersEnvironmentAndFile(t *testing.T) {
	t.Parallel()
	out, err := os.Create(filepath.Join(t.TempDir(), "sweep.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	tree, err := proc.Start(proc.Detached{Path: self(t), Args: []string{helperArg, "env"}, Dir: t.TempDir(), Env: consumerEnv(), Output: out})
	if errors.Is(err, errors.ErrUnsupported) {
		t.Logf("no tree to name on %s: the consumer runs no detached step here", system())
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tree.Leader.Wait(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatal(err)
	}
	if got := lines(b); !slices.Equal(got, consumerEnv()) {
		t.Errorf("the detached program's environment is %q, want the consumer's %q", got, consumerEnv())
	}
	if err := proc.Stop(tree.ID); !errors.Is(err, proc.ErrStale) {
		t.Errorf("Stop of a tree whose leader ended: %v, want ErrStale", err)
	}
}

// The test process and a Rerun child see the consumer's allow-list: its workspace and region kept,
// GOTRACEBACK dropped; Rerun runs on the consumer's deadline and cap, and refuses to run without them.
//
// What it costs when this fails: testkit pins a workspace of ours, keeps a name the consumer dropped,
// or reruns on a limit of ours.
func TestRerunAndTheTestsTakeTheConsumersAllowListDeadlineAndCap(t *testing.T) {
	t.Parallel()
	want := []string{"GOWORK=" + workspace(), region + "=" + regionValue, dropped + "="}
	if got := seen(); !slices.Equal(got, want) {
		t.Errorf("the test process sees %q, want %q", got, want)
	}
	ctx, cancel := context.WithTimeout(t.Context(), rerunWithin)
	defer cancel()
	if _, _, err := testkit.Rerun(t.Context(), rerunCap, "seen"); !errors.Is(err, testkit.ErrRefused) {
		t.Errorf("a Rerun whose ctx has no deadline: %v, want ErrRefused", err)
	}
	if _, _, err := testkit.Rerun(ctx, 0, "seen"); !errors.Is(err, testkit.ErrRefused) {
		t.Errorf("a Rerun with no output cap: %v, want ErrRefused", err)
	}
	code, out, err := testkit.Rerun(ctx, rerunCap, "seen")
	if err != nil || code != 0 || !bytes.Contains(out, []byte("seen: "+strings.Join(want, " ")+"\n")) {
		t.Errorf("a Rerun child: %v, code %d, printed %q, want it to see %q", err, code, out, want)
	}
	if _, out, err := testkit.Rerun(ctx, rerunCap, "flood"); !errors.Is(err, testkit.ErrOutputCap) || len(out) > 2*rerunCap {
		t.Errorf("a Rerun child that writes past the consumer's cap: %v, %d bytes, want ErrOutputCap and at most %d", err, len(out), 2*rerunCap)
	}
}

// seen is each variable of the allow-list rows as this process has it, NAME=value, empty when unset.
func seen() []string {
	var out []string
	for _, k := range []string{"GOWORK", region, dropped} {
		out = append(out, k+"="+getenv(k))
	}
	return out
}

// TestTheHelperRunsOnlyWhenRerun is what a Rerun child of this package runs: seen prints what it was
// handed of the allow-list rows, and flood writes past the consumer's cap.
func TestTheHelperRunsOnlyWhenRerun(t *testing.T) {
	t.Parallel()
	args, child := testkit.Helper()
	if !child {
		if len(args) > 0 {
			t.Fatalf("a run go test started was given %q", args)
		}
		return
	}
	switch args[0] {
	case "seen":
		say("seen: " + strings.Join(seen(), " ") + "\n")
	case "flood":
		say(strings.Repeat("y", 4*rerunCap))
	}
}

// A repository gittest makes is on git's own default branch, the one a plain git init gives under
// the same environment, so the consumer asks git for its name and never assumes one.
//
// What it costs when this fails: gittest picks a branch name, main, that a consumer's code then
// assumes and a consumer's own git would not have given.
func TestARepoIsOnGitsOwnDefaultBranch(t *testing.T) {
	t.Parallel()
	git := func(args ...string) string {
		t.Helper()
		res, err := proc.Run(t.Context(), proc.Cmd{Path: gittest.GitPath(t), Args: args, Env: gittest.Git(t.TempDir()), Timeout: runTimeout, MaxOutput: runCap, Grace: grace(), Drain: runDrain})
		if err != nil || res.Code != 0 {
			t.Fatalf("git %q: %v, Code %d, %s", args, err, res.Code, res.Stderr)
		}
		return strings.TrimSpace(string(res.Stdout))
	}
	plain := t.TempDir()
	git("init", "-q", plain)
	want := git("-C", plain, "symbolic-ref", "--short", "HEAD")
	repo := gittest.Repo(t, map[string]string{"sweep.toml": "region = \"" + regionValue + "\"\n"}, nil)
	if got := git("-C", repo, "symbolic-ref", "--short", "HEAD"); got != want {
		t.Errorf("Repo's branch is %q, want git's own default, %q, as a plain git init gives it", got, want)
	}
}

// A commit a git child makes is under the consumer's identity, appended after gittest's, and a git
// that only reads, by gitenv, runs in the consumer's repository with the consumer's home.
//
// What it costs when this fails: a consumer's commits carry gittest's name, or a read takes a
// configuration of ours.
func TestAGitChildCommitsUnderTheConsumersIdentity(t *testing.T) {
	t.Parallel()
	repo := gittest.Repo(t, map[string]string{"sweep.toml": "region = \"" + regionValue + "\"\n"}, nil)
	git := gittest.GitPath(t)
	id := []string{"GIT_AUTHOR_NAME=" + authorName, "GIT_AUTHOR_EMAIL=" + authorEmail, "GIT_COMMITTER_NAME=" + authorName, "GIT_COMMITTER_EMAIL=" + authorEmail}
	commit := proc.Cmd{
		Path: git, Args: []string{"-C", repo, "commit", "--allow-empty", "--no-verify", "-m", "sweep"}, Env: append(gittest.Git(t.TempDir()), id...),
		Timeout: runTimeout, MaxOutput: runCap, Grace: grace(), Drain: runDrain,
	}
	if res, err := proc.Run(t.Context(), commit); err != nil || res.Code != 0 {
		t.Fatalf("the commit: %v, Code %d, %s", err, res.Code, res.Stderr)
	}
	read := commit
	read.Args = gitenv.ReadArgs(repo, "log", "-1", "--format=%an <%ae>, %cn <%ce>")
	read.Env = gitenv.Env(testkit.Clean(environ()), "HOME="+t.TempDir())
	res, err := proc.Run(t.Context(), read)
	if err != nil || res.Code != 0 {
		t.Fatalf("the read: %v, Code %d, %s", err, res.Code, res.Stderr)
	}
	who := authorName + " <" + authorEmail + ">"
	if got, want := strings.TrimSpace(string(res.Stdout)), who+", "+who; got != want {
		t.Errorf("the commit is by %q, want the consumer's %q", got, want)
	}
}

// A service is the consumer's unit, by its own name and text, in a folder the system's tool reads:
// the folders are the tool's alone, a file in a folder of the consumer's own is refused, and the
// tool is asked for the consumer's name, verbatim.
//
// What it costs when this fails: service writes to a folder of ours, or one a consumer never chose,
// as v0.3.0 did on Windows; the plant for that case is such a folder, written into service.
func TestAServiceIsTheConsumersUnitInAFolderTheToolReads(t *testing.T) {
	t.Parallel()
	folders, err := service.Folders()
	want, wantErr := toolFolders()
	if !errors.Is(err, wantErr) || !slices.Equal(folders, want) {
		t.Fatalf("service.Folders() is %q, %v; want the tool's own %q, %v", folders, err, want, wantErr)
	}
	if wantErr != nil {
		t.Logf("no service tool on %s: the consumer installs nothing here", system())
		return
	}
	p := unit(folders)
	tool := &fakeTool{}
	if err := service.Install(t.Context(), p, tool.run); err != nil {
		t.Fatalf("the install: %v; the tool was asked %q", err, tool.ran)
	}
	for _, f := range p.Files {
		got := tool.task
		if f.Path != "" {
			if got, err = os.ReadFile(f.Path); err != nil {
				t.Fatal(err)
			}
		}
		if !bytes.Equal(got, f.Bytes()) {
			t.Errorf("the tool read %q for the file at %q, want the consumer's %q", got, f.Path, f.Bytes())
		}
	}
	named := func(arg string) bool { return arg == p.Name || strings.HasSuffix(arg, "/"+p.Name) } // launchctl's gui/<uid>/<label>
	if !slices.ContainsFunc(tool.ran, func(argv []string) bool { return slices.ContainsFunc(argv, named) }) {
		t.Errorf("the tool was asked %q, none of it for the consumer's %q", tool.ran, p.Name)
	}
	st, err := service.Query(t.Context(), p, tool.run)
	if err != nil || !st.Installed || !st.Enabled {
		t.Errorf("the query after the install: %+v, %v, want installed and enabled", st, err)
	}
	// The consumer shows "running" only on the tool's own answer, and "not known" where there is
	// none: systemd answers, and its fake says active; launchctl and schtasks do not.
	if want := system() == "linux"; st.ActiveKnown != want || st.Active != want {
		t.Errorf("the query after the install: %+v; want Active and ActiveKnown both %t on %s", st, want, system())
	}
	if err := service.Remove(t.Context(), p, tool.run); err != nil {
		t.Fatalf("the removal: %v", err)
	}
	for _, f := range p.Files {
		if _, err := os.Stat(f.Path); f.Path != "" && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("after the removal the file at %q: %v, want none", f.Path, err)
		}
	}
	own := unit([]string{t.TempDir()})
	ran := len(tool.ran)
	if err := service.Install(t.Context(), own, tool.run); !errors.Is(err, service.ErrUsage) || len(tool.ran) != ran {
		t.Errorf("an install in a folder of the consumer's own: %v, ran %q, want ErrUsage and nothing run", err, tool.ran[ran:])
	}
}

// toolFolders is the folders each system's tool reads a user's unit from, as the consumer knows
// them from the tools' own documents (systemd.unit(5), launchd.plist(5)), and the error of a
// system with none.
func toolFolders() ([]string, error) {
	switch system() {
	case "linux":
		config := getenv("XDG_CONFIG_HOME")
		if config == "" {
			config = filepath.Join(getenv("HOME"), ".config")
		}
		return []string{filepath.Join(config, "systemd", "user"), filepath.Join("/run/user", strconv.Itoa(os.Getuid()), "systemd", "user")}, nil
	case "darwin":
		return []string{filepath.Join(getenv("HOME"), "Library", "LaunchAgents")}, nil
	case "windows":
		return []string{}, nil
	}
	return nil, service.ErrAbsent
}

// unit is the consumer's own unit for this system, its file in the first of folders, or on Windows
// in none: a sweep at 03:17 each day, by the consumer's name and text.
func unit(folders []string) service.Plan {
	in := func(name string) string {
		if len(folders) == 0 {
			return ""
		}
		return filepath.Join(folders[0], name)
	}
	switch system() {
	case "linux":
		return service.Plan{Name: unitBase + ".timer", Files: []service.File{
			{Path: in(unitBase + ".service"), Body: "[Unit]\nDescription=the second consumer's sweep\n\n[Service]\nType=oneshot\nExecStart=/opt/second-consumer/bin/sweep --region " + regionValue + "\n"},
			{Path: in(unitBase + ".timer"), Body: "[Timer]\nOnCalendar=*-*-* 03:17:00\n\n[Install]\nWantedBy=timers.target\n"},
		}}
	case "darwin":
		label := "com.second-consumer.sweep"
		return service.Plan{Name: label, Files: []service.File{{Path: in(label + ".plist"), Body: "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<plist version=\"1.0\"><dict>\n" +
			"<key>Label</key><string>" + label + "</string>\n<key>ProgramArguments</key><array><string>/opt/second-consumer/bin/sweep</string><string>--region</string><string>" + regionValue + "</string></array>\n" +
			"<key>StartCalendarInterval</key><dict><key>Hour</key><integer>3</integer><key>Minute</key><integer>17</integer></dict>\n</dict></plist>\n"}}}
	}
	return service.Plan{Name: unitBase, Files: []service.File{{Path: in(unitBase + ".xml"), UTF16: true, Body: "<?xml version=\"1.0\" encoding=\"UTF-16\"?>\n" +
		"<Task version=\"1.2\" xmlns=\"http://schemas.microsoft.com/windows/2004/02/mit/task\"><Triggers><CalendarTrigger><StartBoundary>2026-11-18T03:17:00</StartBoundary><ScheduleByDay><DaysInterval>1</DaysInterval></ScheduleByDay></CalendarTrigger></Triggers>" +
		"<Actions><Exec><Command>C:\\second-consumer\\sweep.exe</Command><Arguments>--region " + regionValue + "</Arguments></Exec></Actions></Task>\n"}}}
}

// fakeTool answers as each system's tool does for one unit the consumer installs, and records each
// argv: systemctl, launchctl and schtasks, each only as far as service asks it.
type fakeTool struct {
	held bool
	task []byte // the XML schtasks read at its last create
	ran  [][]string
}

func (f *fakeTool) run(_ context.Context, argv []string) ([]byte, int, error) {
	f.ran = append(f.ran, slices.Clone(argv))
	verb := argv[1]
	if argv[0] == "systemctl" {
		verb = argv[2]
	}
	switch verb {
	case "enable", "bootstrap":
		f.held = true
	case "disable":
		f.held = false
	case "show":
		if f.held {
			return []byte("LoadState=loaded\nUnitFileState=enabled\nActiveState=active\n"), 0, nil
		}
		return []byte("LoadState=not-found\nUnitFileState=\nActiveState=inactive\n"), 0, nil
	case "bootout":
		if !f.held {
			return []byte("Boot-out failed: 3: No such process"), 3, nil
		}
		f.held = false
	case "print":
		if !f.held {
			return []byte("Could not find service"), 113, nil
		}
		return []byte(argv[2] + " = {\n}"), 0, nil
	case "/create":
		b, err := os.ReadFile(argv[5])
		if err != nil {
			return []byte("ERROR: The system cannot find the file specified."), 1, nil
		}
		f.task, f.held = b, true
	case "/delete", "/query":
		if !f.held {
			return []byte("ERROR: The system cannot find the file specified."), 1, nil
		}
		if verb == "/delete" {
			f.held = false
			return nil, 0, nil
		}
		return []byte(`<?xml version="1.0" encoding="UTF-16"?><Task><Settings><Enabled>true</Enabled></Settings></Task>`), 0, nil
	}
	return nil, 0, nil
}

// The load average is the consumer's to judge: Read gives the kernel's figures and no threshold, and
// where there is none it is ErrNoAverage, which the consumer handles by running unthrottled.
//
// What it costs when this fails: load decides what a figure means, or makes one up where the system
// gives none.
func TestTheLoadIsTheConsumersToJudge(t *testing.T) {
	t.Parallel()
	r, err := load.Read()
	switch {
	case system() != "linux" && system() != "darwin":
		if !errors.Is(err, load.ErrNoAverage) || !errors.Is(err, errors.ErrUnsupported) || r != (load.Reading{}) {
			t.Errorf("load.Read() on %s: %+v, %v; want the zero Reading and ErrNoAverage", system(), r, err)
		}
		t.Logf("no load average on %s: the consumer runs unthrottled", system())
	case err != nil || r.CPUs != runtime.NumCPU():
		t.Errorf("load.Read() on %s: %+v, %v; want a reading of %d CPUs", system(), r, err, runtime.NumCPU())
	default:
		t.Logf("the consumer's own threshold, 1.5 runnable threads a CPU: busy %t at %.2f", r.One > 1.5*float64(r.CPUs), r.One)
	}
}

// The sandbox holds a program to the consumer's folders: it writes in the one the consumer gave it
// and is refused the other. The consumer names its program by a path and its arguments, as a library
// caller does, and a program not named by its absolute path is ErrUsage naming it, in words of no
// command line. Check says first which block the system gives, typed, and the consumer decides by
// its fields, never by the system's name: it runs the program only behind a block that closes the
// network and the terminal, and says what the block leaves open. Where the system gives no block,
// Check says so first, and the consumer handles ErrNoBlock by running nothing it would have held.
//
// What it costs when this fails: the block takes a folder of ours, or a program writes where the
// consumer did not let it, or a caller is told of a -- it never wrote, or cannot tell what a yes
// leaves open.
func TestTheSandboxHoldsTheProgramToTheConsumersFolders(t *testing.T) {
	t.Parallel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sandbox.Check(sandbox.Folders{ReadWrite: []string{"second-consumer"}}, exe); !errors.Is(err, sandbox.ErrUsage) {
		t.Errorf("Check of a relative folder of the consumer's: %v, want ErrUsage", err)
	}
	err = sandbox.Run("second-consumer-tool", []string{"--check"}, consumerEnv())
	if says := fmt.Sprint(err); !errors.Is(err, sandbox.ErrUsage) || !strings.Contains(says, `"second-consumer-tool"`) || strings.Contains(says, "--") || strings.Contains(says, "command") {
		t.Errorf("Run of a program by a relative path: %v, want ErrUsage naming it, with no -- and no command", err)
	}
	in, out := t.TempDir(), t.TempDir()
	// The program is a file like any other, so the consumer names its folder, by its real path.
	realExe, err := filepath.EvalSymlinks(exe)
	if err != nil {
		t.Fatal(err)
	}
	block, err := sandbox.Check(sandbox.Folders{ReadWrite: []string{in}, ReadOnly: []string{filepath.Dir(realExe)}}, realExe)
	if errors.Is(err, sandbox.ErrNoBlock) {
		if block != (sandbox.Block{}) {
			t.Errorf("no block, yet Check says it closes %+v", block)
		}
		t.Logf("no block on this system (%v): the consumer runs nothing it would have held", err)
		return
	} else if err != nil {
		t.Fatal(err)
	}
	if !block.Network || !block.Terminal {
		t.Fatalf("a yes from Check with %+v: the consumer's program needs the network and the terminal closed", block)
	}
	if !block.Signals || !block.ServiceManager {
		t.Logf("the block leaves open: signals to processes outside %t, the service manager %t", !block.Signals, !block.ServiceManager)
	}
	res, err := proc.Run(t.Context(), cmd(t, "sandbox", in, out))
	if want := filepath.Base(in) + " true\n" + filepath.Base(out) + " false\n"; err != nil || res.Code != 0 || string(res.Stdout) != want {
		t.Errorf("a held run: %v, Code %d, printed %q and %q, want %q", err, res.Code, res.Stdout, res.Stderr, want)
	}
}
