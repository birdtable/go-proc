package proc

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// longEnough is a Timeout no test waits out: the run ends before it, by its own exit or the test's
// doing.
const longEnough = 5 * time.Minute

// drainWait is the Drain the package's tests give a run that is not about its drain: long enough
// for a member the kill ended to exit, which a live row has seen take a quarter second.
const drainWait = 10 * time.Second

// exitsWithin is a Timeout that a helper which exits at once beats: one built with -race, on a
// machine at a load of 50, does not start and exit within a second on Linux, macOS or Windows.
const exitsWithin = 30 * time.Second

// pastStart is the Timeout of a run that is to reach it once its program has started: it outlasts
// only Run's own work between setting the deadline and starting the program, done in this process
// with no wait of its own, so a quarter second leaves that work a wide margin on a loaded host.
const pastStart = 250 * time.Millisecond

// helperCmd is a Cmd that runs the test binary as the helper of mode with args, in a folder of the
// test's own, bounded by longEnough and 1 MiB a stream. Its Env is Bare's with GORACE, which the
// helper, this test binary, built with the race detector when the tests are, reads its settings
// from: the tiers set atexit_sleep_ms=0, so a helper's exit is not held a second. A test of what a
// nil Env gives takes nilEnvHelperCmd.
func helperCmd(t *testing.T, mode string, args ...string) Cmd {
	t.Helper()
	c := nilEnvHelperCmd(t, mode, args...)
	c.Env = Bare("GORACE")
	return c
}

// nilEnvHelperCmd is helperCmd with a nil Env, so the run gives the helper what a nil Env gives.
func nilEnvHelperCmd(t *testing.T, mode string, args ...string) Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Cmd{Path: exe, Args: slices.Concat([]string{helperMode + mode}, args), Dir: t.TempDir(), Timeout: longEnough, MaxOutput: 1 << 20, Drain: drainWait}
}

// endedWith is whether err holds an Ended of why.
func endedWith(err error, why string) bool {
	var e Ended
	return errors.As(err, &e) && e.Why == why
}

// TestRunRefusesWhatItCannotBoundAndStartsNothing: an empty Path, a lone -, a Path that holds =, a
// Path that is a bare name, a zero or negative Timeout or MaxOutput, a negative Grace, a negative
// Drain, and a Drain that with Grace passes the longest time.Duration are each
// refused with their own error, wrapping ErrRefused, and Code -1, before anything starts: the
// helper that would make a file makes none. Only a Grace above zero can wrap, so where the system
// refuses one for a run without Terminal (graceHeld false: Windows), the two that wrap are refused for their Grace (errNoGrace). The control, the same Cmd with nothing wrong, makes it.
//
// What it costs when this fails: a run with no deadline or no cap holds the caller, or its memory,
// for as long as the program likes; a Path that is a word runs whatever this process's PATH finds
// first; or a launcher reads a lone - or a setting as an empty environment and runs the next word.
func TestRunRefusesWhatItCannotBoundAndStartsNothing(t *testing.T) {
	t.Parallel()
	wraps := errDrainWide
	if !graceHeld(false) {
		wraps = errNoGrace
	}
	rows := []struct {
		name string
		edit func(*Cmd)
		want error
	}{
		{"an empty Path", func(c *Cmd) { c.Path = "" }, errEmptyPath},
		{"a lone -", func(c *Cmd) { c.Path = "-" }, errDashPath},
		{"a Path that holds =", func(c *Cmd) { c.Path = filepath.Join(filepath.Dir(c.Path), "A=B") }, errEqualsPath},
		{"a Path that is a bare name", func(c *Cmd) { c.Path = filepath.Base(c.Path) }, errRelativePath},
		{"a zero Timeout", func(c *Cmd) { c.Timeout = 0 }, errTimeout},
		{"a negative Timeout", func(c *Cmd) { c.Timeout = -time.Second }, errTimeout},
		{"a zero MaxOutput", func(c *Cmd) { c.MaxOutput = 0 }, errMaxOutput},
		{"a negative MaxOutput", func(c *Cmd) { c.MaxOutput = -1 }, errMaxOutput},
		{"a negative Grace", func(c *Cmd) { c.Grace = -time.Nanosecond }, errGrace},
		{"a negative Drain", func(c *Cmd) { c.Drain = -time.Nanosecond }, errDrain},
		{"a Drain that with Grace wraps", func(c *Cmd) { c.Grace, c.Drain = time.Second, math.MaxInt64-time.Second+1 }, wraps},
		{"a zero Drain, DefaultDrain, that with Grace wraps", func(c *Cmd) { c.Grace = math.MaxInt64 - DefaultDrain + 1 }, wraps},
	}
	for _, r := range rows {
		mark := filepath.Join(t.TempDir(), "started")
		c := helperCmd(t, "touch", mark)
		r.edit(&c)
		res, err := Run(t.Context(), c)
		if !errors.Is(err, r.want) || !errors.Is(err, ErrRefused) || res.Code != -1 {
			t.Errorf("%s: %v, Code %d, want %v and Code -1", r.name, err, res.Code, r.want)
		}
		if _, err := os.Stat(mark); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: the helper started (%v), want nothing started", r.name, err)
		}
	}
	mark := filepath.Join(t.TempDir(), "started")
	if res, err := Run(t.Context(), helperCmd(t, "touch", mark)); err != nil || res.Code != 0 {
		t.Fatalf("the control: %v, Code %d", err, res.Code)
	}
	if _, err := os.Stat(mark); err != nil {
		t.Errorf("the control made no file: %v", err)
	}
	if len(rows) != 12 {
		t.Errorf("%d rows", len(rows))
	}
}

// TestWhatThisSystemCannotHoldIsRefused: a Grace above zero where the system sends the tree of a
// run without Terminal no signal it can catch (Windows) is refused with its own error, wrapping
// ErrRefused and errors.ErrUnsupported, and Code -1, before anything starts; where the system holds
// it (Unix), the same Cmd runs. A zero Drain, DefaultDrain, runs on every system. The
// control, a zero Grace and the Drain helperCmd gives, runs on both.
//
// What it costs when this fails: on Windows a caller asks its program for time to end cleanly and
// gets none, with nothing said, so what the program would have written in that time is lost there
// alone; or a zero Drain, the field's default, is refused on one system and runs on another.
func TestWhatThisSystemCannotHoldIsRefused(t *testing.T) {
	t.Parallel()
	// Each row holds by what the system does, not by the constant Run reads.
	if graceHeld(false) != sendsTerm || unheldBounded != boundsEveryRead {
		t.Errorf("graceHeld without Terminal is %t and unheldBounded %t, want %t and %t on this system", graceHeld(false), unheldBounded, sendsTerm, boundsEveryRead)
	}
	for _, r := range []struct {
		name  string
		edit  func(*Cmd)
		holds bool
		want  error
	}{
		{"a Grace of 1 ns", func(c *Cmd) { c.Grace = time.Nanosecond }, sendsTerm, errNoGrace},
		{"a Grace of 1 s", func(c *Cmd) { c.Grace = time.Second }, sendsTerm, errNoGrace},
		{"a zero Drain", func(c *Cmd) { c.Drain = 0 }, true, nil},
	} {
		mark := filepath.Join(t.TempDir(), "started")
		c := helperCmd(t, "touch", mark)
		r.edit(&c)
		res, err := Run(t.Context(), c)
		_, statErr := os.Stat(mark)
		if r.holds {
			if err != nil || res.Code != 0 || statErr != nil {
				t.Errorf("%s where the system holds it: %v, Code %d, file %v, want it run", r.name, err, res.Code, statErr)
			}
			continue
		}
		if !errors.Is(err, r.want) || !errors.Is(err, ErrRefused) || !errors.Is(err, errors.ErrUnsupported) || res.Code != -1 {
			t.Errorf("%s where the system cannot hold it: %v, Code %d, want %v, ErrRefused, errors.ErrUnsupported and Code -1", r.name, err, res.Code, r.want)
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("%s where the system cannot hold it: the helper started (%v), want nothing started", r.name, statErr)
		}
	}
	mark := filepath.Join(t.TempDir(), "started")
	if res, err := Run(t.Context(), helperCmd(t, "touch", mark)); err != nil || res.Code != 0 {
		t.Fatalf("the control: %v, Code %d", err, res.Code)
	}
	if _, err := os.Stat(mark); err != nil {
		t.Errorf("the control made no file: %v", err)
	}
}

// TestACmdsDrainIsTheReadAfterTheKillAsGiven: a Drain above zero is the run's drain as the caller
// gave it, a nanosecond included: no floor. A zero one is DefaultDrain, on every system. exec's WaitDelay is the Grace and that drain, so the leader exec kills past ctx's
// end is killed no later.
//
// What it costs when this fails: a run waits on a process that left its tree for longer than its
// caller asked, so Run returns past Timeout, Grace and Drain; or a zero Drain waits none and cuts
// output a process this one started beside the run still held from its fork to its exec.
func TestACmdsDrainIsTheReadAfterTheKillAsGiven(t *testing.T) {
	t.Parallel()
	for _, r := range []struct{ drain, want time.Duration }{
		{0, DefaultDrain}, {time.Nanosecond, time.Nanosecond}, {time.Second, time.Second}, {time.Minute, time.Minute},
	} {
		c := Cmd{Path: "/bin/true", Grace: 3 * time.Second, Drain: r.drain}
		if got := prepare(t.Context(), c, nil).WaitDelay; got != c.Grace+r.want {
			t.Errorf("a Drain of %v: WaitDelay %v, want the Grace and %v, %v", r.drain, got, r.want, c.Grace+r.want)
		}
	}
	if DefaultDrain != time.Second {
		t.Errorf("DefaultDrain %v, want the second", DefaultDrain)
	}
}

// TestATreeThatLeftNothingGivesItsWholeOutputWhateverItsDrain: on Unix, a run with a nanosecond's Drain
// whose helper writes 4 KiB to stdout and 4 KiB to stderr and exits 0, leaving no process, ends with
// no error, Code 0 and both streams whole, in each of 100 runs. The drain begins once the helper has
// exited, when what it wrote may still be in the pipes: past Drain a pipe no process holds is read
// for what it held then, and no process holds these. The Drain is a nanosecond, the shortest one
// that is not DefaultDrain; on Windows, which cuts every pipe still read past Drain, it is
// DefaultDrain. The test is not parallel, and its runs go one at a time: a process another run
// starts holds a copy of every pipe end open here from its fork to its exec, which a nanosecond's
// Drain counts as held, as Cmd.Drain says.
//
// What it costs when this fails: a short Drain, set so an escaped run returns soon, cuts the output
// of a run that left nothing running and ends it with WhyEscaped, so the caller reads a program that
// passed as one that left a process holding its output, and its output as cut.
func TestATreeThatLeftNothingGivesItsWholeOutputWhateverItsDrain(t *testing.T) { //nolint:paralleltest // a run another test starts holds a copy of this one's pipes from its fork to its exec
	const runs, size = 100, 4096
	c := helperCmd(t, "write", strconv.Itoa(size), strconv.Itoa(size), "exit")
	c.Drain = time.Nanosecond
	if boundsEveryRead {
		c.Drain = 0
	}
	c.Timeout = exitsWithin
	for i := range runs {
		// Stdout and Stderr's writers are given what the drain reads after the kill, tail's bytes
		// on Unix, as Result is.
		var out, errs bytes.Buffer
		c.Stdout, c.Stderr = &out, &errs
		res, err := Run(t.Context(), c)
		if err != nil || res.Code != 0 || len(res.Stdout) != size || len(res.Stderr) != size {
			t.Errorf("run %d of a helper that wrote %d bytes to each stream and exited, with a Drain of %v: %v, Code %d, %d and %d bytes, want no error, Code 0 and both whole", i, size, c.Drain, err, res.Code, len(res.Stdout), len(res.Stderr))
		}
		if !bytes.Equal(out.Bytes(), res.Stdout) || !bytes.Equal(errs.Bytes(), res.Stderr) {
			t.Errorf("run %d: the writers were given %d and %d bytes, Result holds %d and %d, want the same", i, out.Len(), errs.Len(), len(res.Stdout), len(res.Stderr))
		}
	}
}

// TestAFileIsKnownByItsExtensionAsWindowsReadsIt: a path whose last name ends in .bat or .cmd is a
// batch file, and one ending in .exe or .com a program file, in any case, after any trailing dots
// and spaces, under either separator and once cleaned; one whose extension only begins or ends so,
// a folder named so, and no extension are neither.
//
// What it costs when this fails: on Windows Run starts a batch file or a script, whose program
// reads Args by its own rules, so an Arg such as &calc runs a second command (CVE-2024-24576's
// class), or refuses a program it should start.
func TestAFileIsKnownByItsExtensionAsWindowsReadsIt(t *testing.T) {
	t.Parallel()
	const isBatch, isProgram, neither = "batch", "program", "neither"
	rows := []struct{ path, want string }{
		{`C:\tools\build.bat`, isBatch},
		{`C:\tools\BUILD.CMD`, isBatch},
		{`C:\tools\build.Bat. . `, isBatch},
		{`C:/tools/build.cmd`, isBatch},
		{`\\host\share\build.bat`, isBatch},
		{`C:\tools\build.exe.bat`, isBatch},
		{`C:/tools/build.bat/.`, isBatch},
		{`C:\tools\build.exe`, isProgram},
		{`C:\tools\BUILD.EXE`, isProgram},
		{`C:\tools\build.Com`, isProgram},
		{`C:\tools\build.exe. . `, isProgram},
		{`C:/tools/build.exe`, isProgram},
		{`C:\tools\build.bat.exe`, isProgram},
		{`C:/tools/x.bat/../build.exe`, isProgram},
		{`C:\tools\build.batch`, neither},
		{`C:\tools\build.batx`, neither},
		{`C:\tools\build.exex`, neither},
		{`C:\tools\build.ps1`, neither},
		{`C:\tools\build.vbs`, neither},
		{`C:\tools.bat\build`, neither},
		{`C:\tools.exe\build`, neither},
		{`C:\tools\build`, neither},
		{`C:\tools\buildexe`, neither},
	}
	for _, r := range rows {
		got := neither
		switch {
		case batch(r.path) && program(r.path):
			got = "both"
		case batch(r.path):
			got = isBatch
		case program(r.path):
			got = isProgram
		}
		if got != r.want {
			t.Errorf("%q is read as %s, want %s", r.path, got, r.want)
		}
	}
	if len(rows) != 23 {
		t.Errorf("%d rows", len(rows))
	}
}

// extraRun runs the extra helper, with ReadPeak peak, and the writing end of a pipe as its one
// extra file, and gives the run's result and error and what the pipe carried once the run returned.
func extraRun(t *testing.T, peak bool) (Result, string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	c := helperCmd(t, "extra", inherited(t, w), "through")
	c.ReadPeak = peak
	c.ExtraFiles = []*os.File{w}
	res, runErr := Run(t.Context(), c)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return res, string(got), runErr
}

// TestTheProgramWritesTheFileExtraFilesHandsIt: a program run with a pipe's writing end in
// ExtraFiles writes to it, as its descriptor 3 on Unix and by its handle's value on Windows, and
// what it wrote reaches the reading end; with ReadPeak too, on every system, as no run starts
// through a wrapper of its own.
//
// What it costs when this fails: a caller's wrapper that tells its parent by a channel the
// program it execs cannot write has no such channel, and reads the program's own exit in its
// place.
func TestTheProgramWritesTheFileExtraFilesHandsIt(t *testing.T) {
	t.Parallel()
	for _, peak := range []bool{false, true} {
		res, got, err := extraRun(t, peak)
		if err != nil || res.Code != 0 || got != "through" {
			t.Errorf("ReadPeak %v: %v, Code %d, stderr %q, the pipe carried %q, want exit 0 and %q", peak, err, res.Code, res.Stderr, got, "through")
		}
	}
}

// TestAnExtraFilesEntryThatIsNoFileIsRefused: an ExtraFiles entry that is nil, or a file closed
// before Run, is refused with errExtraNoFile, Code -1, on every system, and the program never
// starts. A pipe's writing end is the next entry, and on Windows the program is told its handle,
// so a start that went ahead there would write into the pipe; on Unix the program is told
// descriptor 3, the slot of the entry that is no file. The pipe is left empty.
//
// What it costs when this fails: on Unix the program starts with that descriptor closed, the first
// file it opens takes its number, and what it was told to write to the caller's file goes into a
// file of its own, with Run saying nothing; on Windows an invalid handle reaches the start.
func TestAnExtraFilesEntryThatIsNoFileIsRefused(t *testing.T) {
	t.Parallel()
	closed, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct {
		name string
		file *os.File
	}{
		{"nil", nil},
		{"closed", closed},
	} {
		pr, pw, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		c := helperCmd(t, "extra", inherited(t, pw), "through")
		c.ExtraFiles = []*os.File{r.file, pw}
		res, runErr := Run(t.Context(), c)
		if err := pw.Close(); err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(pr)
		if err != nil {
			t.Fatal(err)
		}
		_ = pr.Close()
		if !errors.Is(runErr, errExtraNoFile) || !errors.Is(runErr, ErrRefused) || res.Code != -1 || len(got) != 0 {
			t.Errorf("%s entry: %v, Code %d, the pipe carried %q, want errExtraNoFile, Code -1 and nothing", r.name, runErr, res.Code, got)
		}
	}
}

// TestTheProgramFileRefusalsAreExportedAndToldApart: ErrBatch and ErrNotProgram each wrap
// ErrRefused and neither matches the other, on every system. On Windows Run refuses a .bat by
// ErrBatch alone and a .ps1 by ErrNotProgram alone, each with Code -1, though neither file is
// there; on any other system neither run is refused, and neither error comes back.
//
// What it costs when this fails: a caller that words each refusal its own way, by errors.Is,
// gives a batch file the words of a script, or of a run that ran, or has to repeat Run's Windows
// check in code of its own to tell them apart.
func TestTheProgramFileRefusalsAreExportedAndToldApart(t *testing.T) {
	t.Parallel()
	for _, err := range []error{ErrBatch, ErrNotProgram} {
		if !errors.Is(err, ErrRefused) {
			t.Errorf("%v does not wrap ErrRefused", err)
		}
	}
	if errors.Is(ErrBatch, ErrNotProgram) || errors.Is(ErrNotProgram, ErrBatch) {
		t.Error("ErrBatch and ErrNotProgram match each other")
	}
	dir := t.TempDir()
	rows := []struct {
		name      string
		want, not error
	}{
		{"absent.bat", ErrBatch, ErrNotProgram},
		{"absent.ps1", ErrNotProgram, ErrBatch},
	}
	for _, r := range rows {
		// On every system the gate Windows runs words each refusal by its own name alone, so a
		// crossed or dropped wrap is red off Windows too.
		if err := unstartable(filepath.Join(dir, r.name), ""); !errors.Is(err, r.want) || errors.Is(err, r.not) {
			t.Errorf("unstartable(%s) gave %v, want %v alone", r.name, err, r.want)
		}
		res, err := Run(t.Context(), Cmd{Path: filepath.Join(dir, r.name), Timeout: exitsWithin, MaxOutput: 1 << 10, Drain: drainWait})
		switch {
		case programsOnly && (!errors.Is(err, r.want) || errors.Is(err, r.not) || res.Code != -1):
			t.Errorf("Run(%s) gave code %d and %v, want code -1 and %v alone", r.name, res.Code, err, r.want)
		case !programsOnly && (errors.Is(err, ErrRefused) || errors.Is(err, ErrBatch) || errors.Is(err, ErrNotProgram)):
			t.Errorf("Run(%s) gave %v, want no refusal off Windows", r.name, err)
		}
	}
}

// TestAValueStoredInTheExportedRefusalsStartsNoBatchFile: the refusal of a batch file, or of a
// file that is no program file, stands when a caller or a dependency stores nil in ErrBatch and
// ErrNotProgram, as any package of the binary can. A helper stores it, in a process of its own so
// no other test reads the change, and asks the gate of both, which builds on every system.
//
// What it costs when this fails: `proc.ErrBatch = nil` anywhere in the binary makes Run and Start
// start a .bat through cmd.exe, whose rules let an argument run a second command
// (CVE-2024-24576's class).
func TestAValueStoredInTheExportedRefusalsStartsNoBatchFile(t *testing.T) {
	t.Parallel()
	c := helperCmd(t, "refused-nil", t.TempDir())
	res, err := Run(t.Context(), c)
	if err != nil || res.Code != 0 {
		t.Errorf("the gate after nil was stored: %v, Code %d, stderr %q, want exit 0", err, res.Code, res.Stderr)
	}
}

// TestTheChildSeesPathThenEachArgAsOneArgumentInOrder: the child's own argv is Path, then each of
// Args as one argument, in order: an argument with a space, an empty one, one with =, a lone -, and
// what a shell would expand, each as written.
//
// What it costs when this fails: an argument is split, dropped or read by a shell, and the program
// runs on words its caller never gave it.
func TestTheChildSeesPathThenEachArgAsOneArgumentInOrder(t *testing.T) {
	t.Parallel()
	c := helperCmd(t, "argv", "two words", "", "a=b", "-", "*", "$HOME", "`id`")
	res, err := Run(t.Context(), c)
	if err != nil || res.Code != 0 {
		t.Fatalf("the argv helper: %v, Code %d, stderr %q", err, res.Code, res.Stderr)
	}
	got := strings.Split(strings.TrimSuffix(string(res.Stdout), "\x00"), "\x00")
	if want := slices.Concat([]string{c.Path}, c.Args); !slices.Equal(got, want) {
		t.Errorf("the child saw %q, want %q", got, want)
	}
}

// environment is what the env helper says its environment is, under Cmd c.
func environment(t *testing.T, c Cmd) []string {
	t.Helper()
	res, err := Run(t.Context(), c)
	if err != nil || res.Code != 0 {
		t.Fatalf("the env helper: %v, Code %d, stderr %q", err, res.Code, res.Stderr)
	}
	return strings.Split(strings.TrimSuffix(string(res.Stdout), "\x00"), "\x00")
}

// TestANilEnvGivesTheChildExactlyBare: a nil Env gives the child Bare() and nothing else; a given
// Env is the child's, with nothing of Bare's added. Bare gives each name once, the bare names first,
// and drops a kept name that is empty or holds =.
//
// What it costs when this fails: a child is handed this process's whole environment, its tokens
// with it, by a caller who passed none.
func TestANilEnvGivesTheChildExactlyBare(t *testing.T) {
	t.Parallel()
	if got, want := environment(t, nilEnvHelperCmd(t, "env")), Bare(); !slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want))) {
		t.Errorf("a nil Env gave the child %q, want Bare(), %q", got, want)
	}
	c := helperCmd(t, "env")
	c.Env = []string{"PROC_GIVEN=1"}
	got := environment(t, c)
	if !slices.Contains(got, "PROC_GIVEN=1") || slices.ContainsFunc(got, func(kv string) bool { return strings.HasPrefix(kv, "PATH=") }) {
		t.Errorf("a given Env gave the child %q, want it, with no PATH", got)
	}
	bare := Bare("PATH", "", "A=B", bareNames()[0])
	if !slices.Equal(bare, Bare()) {
		t.Errorf("Bare with a bare name again, an empty name and one with = is %q, want Bare(), %q", bare, Bare())
	}
	if len(Bare()) == 0 || !strings.HasPrefix(Bare()[0], "PATH=") {
		t.Errorf("Bare() is %q, want PATH first, as the test's own environment has it", Bare())
	}
}

// TestARunWithNoBareNameSetGivesTheChildNothingOfThisProcess: in a process where none of the bare
// names is set, as one started with only its own secrets, Bare() is empty and not nil, prepare
// hands exec no nil Env, and a run with a nil Env gives the child exactly the list emptyEnvChild
// says, empty but on Windows, and nothing of this process's. The list is compared whole. Each check
// is one guard: Bare's empty list, prepare's, and the two together.
//
// What it costs when this fails: a runner started with only a token in its environment hands that
// token, and all else it holds, to every child it runs with a nil Env, as exec does for a nil Env.
func TestARunWithNoBareNameSetGivesTheChildNothingOfThisProcess(t *testing.T) {
	// serial: t.Setenv forbids t.Parallel, and the names it unsets every other test's child reads.
	for _, name := range bareNames() {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PROC_PARENT_ONLY", "marker")
	if bare := Bare(); bare == nil || len(bare) != 0 {
		t.Errorf("Bare() with no bare name set is %#v, want an empty list that is not nil", bare)
	}
	if env := prepare(t.Context(), nilEnvHelperCmd(t, "env"), nil).Env; env == nil || len(env) != 0 {
		t.Errorf("prepare with a nil env set exec's Env to %#v, want an empty list that is not nil", env)
	}
	res, err := Run(t.Context(), nilEnvHelperCmd(t, "env"))
	if err != nil || res.Code != 0 {
		t.Fatalf("the env helper: %v, Code %d, stderr %q", err, res.Code, res.Stderr)
	}
	out, ok := strings.CutSuffix(string(res.Stdout), "\x00")
	if !ok {
		t.Fatalf("the env helper printed %q, want its list ended by a NUL", res.Stdout)
	}
	got := []string{}
	if out != "" {
		got = strings.Split(out, "\x00")
	}
	// The whole list, never filtered, so no entry passes unseen.
	if want := emptyEnvChild(); !slices.Equal(got, want) {
		t.Errorf("a nil Env with no bare name set gave the child %q, want exactly %q", got, want)
	}
	if slices.ContainsFunc(got, func(kv string) bool { return strings.HasPrefix(kv, "PROC_PARENT_ONLY=") }) {
		t.Errorf("the child got this process's marker: %q", got)
	}
}

// TestStdinReachesTheChild: what Stdin holds is what the child reads, to its end.
//
// What it costs when this fails: a program fed its input by the caller reads nothing, or hangs on a
// stdin that never ends.
func TestStdinReachesTheChild(t *testing.T) {
	t.Parallel()
	c := helperCmd(t, "stdin")
	c.Stdin = strings.NewReader("line one\nline two\n")
	res, err := Run(t.Context(), c)
	if err != nil || string(res.Stdout) != "line one\nline two\n" {
		t.Errorf("the child read %q (%v), want what Stdin held", res.Stdout, err)
	}
}

// TestARunEndsThoughItsStdinsReadNeverReturns: a Stdin that is no file and whose Read never
// returns, an io.Pipe reader nothing writes to, holds no run past its end. A program that exits at
// once returns Code 0 and no error before its Timeout, exitsWithin; a program that reads its stdin,
// and so holds to its Timeout, pastStart, ends with Ended{WhyDeadline} and Code -1, within the
// Timeout, the grace and the drain.
// Each is bounded by the test, so a run that never returns fails the row and does not hang it.
// Each row runs again with ReadPeak, which on Linux starts the program in a cgroup of its own where
// one is delegated.
//
// What it costs when this fails: a caller that hands Run a reader of its own, a network stream or
// a pipe of its own making, waits on that reader past the Timeout, for as long as nothing writes
// to it, and Timeout no longer ends a run.
func TestARunEndsThoughItsStdinsReadNeverReturns(t *testing.T) {
	t.Parallel()
	const drain = 2 * time.Second
	for _, r := range []stdinCase{
		{"a program that exits at once", []string{"write", "0", "0", "exit"}, exitsWithin, 0, ""},
		{"a program that reads its stdin", []string{"stdin"}, pastStart, -1, WhyDeadline},
	} {
		for _, peak := range []bool{false, true} {
			name := r.name
			if peak {
				// On Linux, in the cgroup whose memory.peak it reads.
				name += ", reading its peak"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				stdinRow(t, r, peak, drain)
			})
		}
	}
}

// A stdinCase is a row of TestARunEndsThoughItsStdinsReadNeverReturns: the helper of args, run with
// a Timeout of timeout, ends by code and, when why is set, Ended{why}.
type stdinCase struct {
	name    string
	args    []string
	timeout time.Duration
	code    int
	why     string
}

// stdinRow runs the row r, asking for its peak when peak is set, with a Stdin whose Read never
// returns and a drain of drain.
func stdinRow(t *testing.T, r stdinCase, peak bool, drain time.Duration) {
	t.Helper()
	stdin, never := io.Pipe()
	// Closing the writer ends the Read the run left behind, once the row is done with it.
	t.Cleanup(func() { _ = never.Close() })
	code, why := r.code, r.why
	c := helperCmd(t, r.args[0], r.args[1:]...)
	c.Stdin, c.Timeout, c.Drain = stdin, r.timeout, drain
	c.ReadPeak = peak
	type ran struct {
		res Result
		err error
	}
	done := make(chan ran, 1)
	go func() {
		res, err := Run(t.Context(), c)
		done <- ran{res, err}
	}()
	// The bound is the run's own: its Timeout, its grace and its drain, and as long again for a
	// loaded machine.
	bound := 2 * (c.Timeout + c.Grace + drain)
	select {
	case got := <-done:
		switch {
		case why == "" && (got.err != nil || got.res.Code != code):
			t.Errorf("%s with a Stdin that never returns: %v, Code %d, want no error and Code %d", t.Name(), got.err, got.res.Code, code)
		case why != "" && (!endedWith(got.err, why) || got.res.Code != code):
			t.Errorf("%s with a Stdin that never returns: %v, Code %d, want Ended{%s} and Code %d", t.Name(), got.err, got.res.Code, why, code)
		}
	case <-time.After(bound):
		t.Errorf("%s with a Stdin that never returns: Run had not returned %v after it began, past its Timeout %v, grace %v and drain %v", t.Name(), bound, c.Timeout, c.Grace, drain)
	}
}

// failing is a Stdin that gives what it holds, then errStdinFailed.
type failing struct{ r io.Reader }

var errStdinFailed = errors.New("the test's Stdin failed")

func (f failing) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if errors.Is(err, io.EOF) {
		return n, errStdinFailed
	}
	return n, err
}

// TestAStdinThatIsNoFileReachesTheChildWhole: a Stdin that is no file and ends reaches the child
// whole, however it arrives: a reader of 512 KiB, more than a pipe holds at once, and an io.Pipe
// a writer fills in pieces as the child reads. A Stdin whose Read fails is the run's error, after
// what it gave, when the child's exit is none, as it is from exec. A child that exits before it
// reads its stdin leaves the copy a pipe no one reads, which is no error.
//
// What it costs when this fails: a program fed its input by a reader of the caller's reads only
// part of it, a caller is not told its reader failed and the program read a part as the whole,
// or a run whose program did not need all its input reads as a failed one.
func TestAStdinThatIsNoFileReachesTheChildWhole(t *testing.T) {
	t.Parallel()
	whole := bytes.Repeat([]byte("0123456789abcdef"), 32<<10)
	c := helperCmd(t, "stdin")
	c.Stdin = bytes.NewReader(whole)
	res, err := Run(t.Context(), c)
	if err != nil || res.Code != 0 || !bytes.Equal(res.Stdout, whole) {
		t.Errorf("a reader of %d bytes: %v, Code %d, the child read %d bytes, want all of them", len(whole), err, res.Code, len(res.Stdout))
	}
	stdin, w := io.Pipe()
	go func() {
		for piece := range slices.Chunk(whole, 4<<10) {
			if _, err := w.Write(piece); err != nil {
				_ = w.CloseWithError(err)
				return
			}
		}
		_ = w.Close()
	}()
	c = helperCmd(t, "stdin")
	c.Stdin = stdin
	res, err = Run(t.Context(), c)
	if err != nil || res.Code != 0 || !bytes.Equal(res.Stdout, whole) {
		t.Errorf("an io.Pipe written in pieces: %v, Code %d, the child read %d bytes, want all %d", err, res.Code, len(res.Stdout), len(whole))
	}
	c = helperCmd(t, "stdin")
	c.Stdin = failing{strings.NewReader("before it failed\n")}
	res, err = Run(t.Context(), c)
	if !errors.Is(err, errStdinFailed) || string(res.Stdout) != "before it failed\n" {
		t.Errorf("a Stdin that failed: %v, the child read %q, want errStdinFailed and what it gave first", err, res.Stdout)
	}
	c = helperCmd(t, "write", "0", "0", "exit")
	c.Stdin = bytes.NewReader(bytes.Repeat(whole, 8))
	res, err = Run(t.Context(), c)
	if err != nil || res.Code != 0 {
		t.Errorf("a child that exited before it read its stdin: %v, Code %d, want no error and Code 0", err, res.Code)
	}
}

// TestAProgramThatWritesPastTheCapIsEndedWithWhyCap: a program that writes MaxOutput + 1 bytes to
// one stream, and would then live on, is killed and ends with Ended{WhyCap} and Code -1, the stream
// holding its first MaxOutput bytes; for each stream. The control: MaxOutput bytes to each, and an
// exit, is no error and both streams whole.
//
// What it costs when this fails: a program that prints without end fills the caller's memory, or a
// run cut at its cap reads as one that passed.
func TestAProgramThatWritesPastTheCapIsEndedWithWhyCap(t *testing.T) {
	t.Parallel()
	const maxOutput = 64 << 10
	for _, r := range []struct{ n, m string }{{"65537", "0"}, {"0", "65537"}} {
		c := helperCmd(t, "write", r.n, r.m, "hold")
		c.MaxOutput = maxOutput
		res, err := Run(t.Context(), c)
		if !endedWith(err, WhyCap) || res.Code != -1 || res.End != EndNone || res.Signal != nil {
			t.Errorf("%s and %s bytes over a cap of %d: %v, Code %d, End %q, Signal %v, want Ended{%s}, Code -1 and EndNone", r.n, r.m, maxOutput, err, res.Code, res.End, res.Signal, WhyCap)
		}
		if got := len(res.Stdout) + len(res.Stderr); got != maxOutput {
			t.Errorf("%s and %s bytes over a cap of %d: kept %d and %d, want the cap's worth of the one stream", r.n, r.m, maxOutput, len(res.Stdout), len(res.Stderr))
		}
	}
	c := helperCmd(t, "write", "65536", "65536", "exit")
	c.MaxOutput = maxOutput
	res, err := Run(t.Context(), c)
	if err != nil || res.Code != 0 || len(res.Stdout) != maxOutput || len(res.Stderr) != maxOutput {
		t.Errorf("the cap's worth on each stream: %v, Code %d, %d and %d bytes, want no error and both whole", err, res.Code, len(res.Stdout), len(res.Stderr))
	}
}

// cancelling is a Stdin that ends its run's parent context by cause when exec first reads it, which
// is after the program has started.
type cancelling struct {
	cancel context.CancelCauseFunc
	cause  error
}

var errTheTestEndedIt = errors.New("the test ended the run's parent context")

func (c cancelling) Read([]byte) (int, error) {
	c.cancel(c.cause)
	return 0, io.EOF
}

// TestEachWayARunIsEndedIsAnEnded: a run that reaches its Timeout ends with Ended{WhyDeadline}, a
// run whose parent context ends with Ended{WhyCancelled} joined to the parent's cause, even a cause
// that is itself an Ended{WhyCap} as another run's error is, and a
// program's own exit, whatever its code, with no error and its Code. WhyCap has its own test,
// WhyEscaped one on Unix, where a process can leave the tree, and WhyUnkillable a group test on Unix,
// where a process can change its user. Each word says its own words.
//
// What it costs when this fails: a caller cannot tell a run that timed out from one it cancelled,
// or a program that failed from one the run killed.
func TestEachWayARunIsEndedIsAnEnded(t *testing.T) {
	t.Parallel()
	c := helperCmd(t, "hold")
	c.Timeout = pastStart
	res, err := Run(t.Context(), c)
	if !endedWith(err, WhyDeadline) || res.Code != -1 {
		t.Errorf("a run that reached its Timeout: %v, Code %d, want Ended{%s}", err, res.Code, WhyDeadline)
	}
	for _, cause := range []error{errTheTestEndedIt, errCap} {
		parent, cancel := context.WithCancelCause(t.Context())
		c = helperCmd(t, "hold")
		c.Stdin = cancelling{cancel, cause}
		res, err = Run(parent, c)
		cancel(nil)
		if !endedWith(err, WhyCancelled) || !errors.Is(err, cause) || res.Code != -1 || res.End != EndNone || res.Signal != nil {
			t.Errorf("a run whose parent ended by %v: %v, Code %d, End %q, Signal %v, want Ended{%s}, the parent's cause and EndNone", cause, err, res.Code, res.End, res.Signal, WhyCancelled)
		}
	}
	res, err = Run(t.Context(), helperCmd(t, "write", "0", "0", "exit"))
	if err != nil || res.Code != 0 {
		t.Errorf("a program that exited 0: %v, Code %d", err, res.Code)
	}
	res, err = Run(t.Context(), helperCmd(t, "no such mode"))
	if err != nil || res.Code != 2 {
		t.Errorf("a program that exited 2: %v, Code %d, want no error and Code 2", err, res.Code)
	}
	for _, why := range []string{WhyDeadline, WhyCap, WhyCancelled, WhyEscaped, WhyUnkillable} {
		if msg := (Ended{why}).Error(); !strings.HasPrefix(msg, "proc: ") || strings.HasSuffix(msg, why) {
			t.Errorf("Ended{%q} says %q, want its own words", why, msg)
		}
	}
}

// TestEndIsTheProgramsOwnExitAndNoneForARunGoProcEndedOrRefused: a program that exits 0 or 3
// reads EndExited with that Code, no Signal and no Status, and no error. A run its Timeout ends and
// a run refused before it starts read EndNone, no Signal and no Status, with Code -1 and their
// errors saying why: on Unix the Timeout's kill is a signal, which End does not report as the
// program's own end.
//
// What it costs when this fails: a caller reads a run go-proc ended, or never started, as the
// program's own end, or a program's exit as something else.
func TestEndIsTheProgramsOwnExitAndNoneForARunGoProcEndedOrRefused(t *testing.T) {
	t.Parallel()
	for _, code := range []int{0, 3} {
		res, err := Run(t.Context(), helperCmd(t, "exit-code", strconv.Itoa(code)))
		if err != nil || res.Code != code || res.End != EndExited || res.Signal != nil || res.Status != 0 {
			t.Errorf("a program that exits %d: %v, Code %d, End %q, Signal %v, Status %#x, want no error, Code %d and EndExited alone", code, err, res.Code, res.End, res.Signal, res.Status, code)
		}
	}
	timed := helperCmd(t, "hold")
	timed.Timeout = pastStart
	res, err := Run(t.Context(), timed)
	if !endedWith(err, WhyDeadline) || res.Code != -1 || res.End != EndNone || res.Signal != nil || res.Status != 0 {
		t.Errorf("a run that reached its Timeout: %v, Code %d, End %q, Signal %v, Status %#x, want Ended{%s}, Code -1 and EndNone alone", err, res.Code, res.End, res.Signal, res.Status, WhyDeadline)
	}
	refused := helperCmd(t, "exit-code", "0")
	refused.Timeout = 0
	res, err = Run(t.Context(), refused)
	if !errors.Is(err, ErrRefused) || res.Code != -1 || res.End != EndNone || res.Signal != nil || res.Status != 0 {
		t.Errorf("a run refused for its Timeout: %v, Code %d, End %q, Signal %v, Status %#x, want ErrRefused, Code -1 and EndNone alone", err, res.Code, res.End, res.Signal, res.Status)
	}
}

// onPid runs c, a pid-hold helper's, with its stdout to a pipe whose first line, the helper's pid,
// is given to act while the run waits; the rest is read and dropped. It gives the run's Result and
// error, and act's error, or why no pid was read.
func onPid(t *testing.T, c Cmd, act func(pid int) error) (Result, error, error) {
	t.Helper()
	r, w := io.Pipe()
	c.Stdout = w
	acted := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(r).ReadString('\n')
		pid, perr := strconv.Atoi(strings.TrimSpace(line))
		if err = cmp.Or(err, perr); err == nil {
			err = act(pid)
		}
		acted <- err
		_, _ = io.Copy(io.Discard, r)
	}()
	res, err := Run(t.Context(), c)
	_ = w.Close()
	return res, err, <-acted
}

// TestARunOnAnEndedContextIsCancelledAndStartsNothing: a run whose parent context has already ended
// ends with Ended{WhyCancelled} joined to the parent's cause, Code -1, and starts nothing: the helper
// that would make a file makes none. A cause that is itself an Ended{WhyCap} or Ended{WhyLimit}, as
// errgroup gives its context from another run's error, is the parent's, not this run's own cut.
//
// What it costs when this fails: a caller that cancels its runs reads exec's bare context error, and
// on Linux a Contained run reads it as errNoContained, a kernel that refuses an unprivileged user
// namespace; or, with an Ended as the cause, a run
// that started nothing returns no error (third review on 6415d03, 1).
func TestARunOnAnEndedContextIsCancelledAndStartsNothing(t *testing.T) {
	t.Parallel()
	for _, cause := range []error{errTheTestEndedIt, errCap, errLimit} {
		parent, cancel := context.WithCancelCause(t.Context())
		cancel(cause)
		mark := filepath.Join(t.TempDir(), "started")
		res, err := Run(parent, helperCmd(t, "touch", mark))
		if !endedWith(err, WhyCancelled) || !errors.Is(err, cause) || res.Code != -1 {
			t.Errorf("a run on a context ended by %v: %v, Code %d, want Ended{%s}, the parent's cause and Code -1", cause, err, res.Code, WhyCancelled)
		}
		if _, err := os.Stat(mark); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a run on a context ended by %v: the helper started (%v), want nothing started", cause, err)
		}
	}
}

// placed puts the test binary at name under dir, with .exe after it where the binary has one (on
// Windows), by a hard link where the system makes one and by a copy where not, and returns name as
// placed.
func placed(t *testing.T, dir, name string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if ext := filepath.Ext(exe); strings.EqualFold(ext, ".exe") {
		name += ext
	}
	to := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		t.Fatal(err)
	}
	if os.Link(exe, to) == nil {
		return name
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeExe(to, data); err != nil {
		t.Fatal(err)
	}
	return name
}

// writeExe writes data to path, mode 0700, as a program for a test to start, by writeHeld.
func writeExe(path string, data []byte) error {
	return writeHeld(path, data, 0o700)
}

// writeHeld writes data to path with mode, as a file a test starts or tries to start. It holds
// syscall.ForkLock for reading from the file's open to its close, which no fork of this process
// starts within. A child another test forked while the file was open would hold it open for writing
// until its own exec, and a start of path in that time fails `text file busy` on Linux.
func writeHeld(path string, data []byte, mode os.FileMode) error {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	return os.WriteFile(path, data, mode)
}

// ranHelper runs c, the helper's touch of mark, and wants it run: no error, Code 0 and mark made.
func ranHelper(t *testing.T, name string, c Cmd, mark string) {
	t.Helper()
	if res, err := Run(t.Context(), c); err != nil || res.Code != 0 {
		t.Errorf("%s: Path %q in Dir %q: %v, Code %d, stderr %q, want it run", name, c.Path, c.Dir, err, res.Code, res.Stderr)
		return
	}
	if _, err := os.Stat(mark); err != nil {
		t.Errorf("%s: Path %q in Dir %q ran and made no file: %v", name, c.Path, c.Dir, err)
	}
}

// refusedHelper runs c, the helper's touch of mark, and wants it refused with want, wrapping
// ErrRefused, with Code -1 and mark not made.
func refusedHelper(t *testing.T, name string, c Cmd, mark string, want error) {
	t.Helper()
	res, err := Run(t.Context(), c)
	if !errors.Is(err, want) || !errors.Is(err, ErrRefused) || res.Code != -1 {
		t.Errorf("%s: Path %q in Dir %q: %v, Code %d, want %v and Code -1", name, c.Path, c.Dir, err, res.Code, want)
	}
	if _, err := os.Stat(mark); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s: Path %q in Dir %q started (%v), want nothing started", name, c.Path, c.Dir, err)
	}
}

// TestRunTakesARelativePathWithASeparatorResolvedAgainstDir: a relative Path with a separator runs,
// resolved against Dir as exec.Cmd resolves it: bin/helper, ./bin/helper, ./bin/helper written
// with / on every system, and ../bin/helper from a folder below. The helper is the test binary,
// placed under a folder of the test's own, and Dir is never this process's folder.
//
// What it costs when this fails: a caller that names its program as os/exec allows, by a path
// relative to the folder it runs in, is refused, or runs a program found somewhere else.
func TestRunTakesARelativePathWithASeparatorResolvedAgainstDir(t *testing.T) {
	t.Parallel()
	sep := string(filepath.Separator)
	root := t.TempDir()
	helper := placed(t, root, filepath.Join("bin", "helper"))
	below := filepath.Join(root, "below")
	if err := os.Mkdir(below, 0o700); err != nil {
		t.Fatal(err)
	}
	rows := []struct{ name, dir, path string }{
		{"a folder before it", root, helper},
		{"./ before it", root, "." + sep + helper},
		{"./ before it, with / on every system", root, "./" + filepath.ToSlash(helper)},
		{"../ before it, from a folder below", below, ".." + sep + helper},
	}
	for _, r := range rows {
		mark := filepath.Join(t.TempDir(), "started")
		c := helperCmd(t, "touch", mark)
		c.Path, c.Dir = r.path, r.dir
		ranHelper(t, r.name, c, mark)
	}
	if len(rows) != 4 {
		t.Errorf("%d rows", len(rows))
	}
}

// TestARelativePathWithNoDirIsThisFoldersAndABareNameIsRefusedWhereverItIs: with Dir empty, a
// relative Path with a separator is resolved against this process's folder, and runs. A bare name
// is refused with errRelativePath, and starts nothing, though a program of that name is in Dir and
// in this process's folder both: with Dir set to this folder, with Dir empty, and with Dir set to a
// folder that holds it and this folder holding another.
//
// What it costs when this fails: a caller whose Dir is empty is refused a path os/exec runs; or a
// word runs the first program PATH, Dir or this folder holds by that name (exec.ErrDot's class).
func TestARelativePathWithNoDirIsThisFoldersAndABareNameIsRefusedWhereverItIs(t *testing.T) {
	// serial: t.Chdir changes the folder of the whole process.
	sep := string(filepath.Separator)
	root := t.TempDir()
	helper := placed(t, root, filepath.Join("bin", "helper"))
	name := placed(t, root, "helper")
	t.Chdir(root)
	mark := filepath.Join(t.TempDir(), "started")
	c := helperCmd(t, "touch", mark)
	c.Path, c.Dir = "."+sep+helper, ""
	ranHelper(t, "a relative Path with Dir empty", c, mark)
	rows := []struct{ name, dir string }{
		{"a bare name in Dir, which is this folder", root},
		{"a bare name in this folder, with Dir empty", ""},
		{"a bare name in Dir and, by another file, in this folder", filepath.Join(root, "bin")},
	}
	for _, r := range rows {
		mark := filepath.Join(t.TempDir(), "started")
		c := helperCmd(t, "touch", mark)
		c.Path, c.Dir = name, r.dir
		refusedHelper(t, r.name, c, mark, errRelativePath)
	}
	if len(rows) != 3 {
		t.Errorf("%d rows", len(rows))
	}
}

// TestRunRefusesAPathThatBeginsWithADashAndRunsItAfterDotSlash: a relative Path that begins with -,
// -x/tool, is refused with errDashPath before anything starts, though the file is there under Dir;
// the same file written after ./, ./-x/tool, runs.
//
// What it costs when this fails: a caller that places Path after an env-style launcher has it read as
// an option, and the launcher runs the next word in its place.
func TestRunRefusesAPathThatBeginsWithADashAndRunsItAfterDotSlash(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	tool := placed(t, root, filepath.Join("-x", "tool"))
	mark := filepath.Join(t.TempDir(), "started")
	c := helperCmd(t, "touch", mark)
	c.Path, c.Dir = tool, root
	refusedHelper(t, "a Path that begins with -", c, mark, errDashPath)
	c.Path = "." + string(filepath.Separator) + tool
	ranHelper(t, "the same file after ./", c, mark)
}

// TestARunOfFileStartsTheOpenFileNotWhatItsNameNamesLater: on Linux a run with File starts the
// file that was opened, the helper, after another file was renamed over the name it was opened by,
// with that name as its argv[0], and with ExtraFiles where a run without File puts them; the
// control, a run with Path at that name, starts the file renamed over it and fails. On every other
// system a run with File is refused, wrapping ErrRefused and errors.ErrUnsupported, Code -1.
//
// What it costs when this fails: a caller who checked a program and opened it runs whatever was
// put at its name since, the program finds its file where ExtraFiles were to be, or
// another system starts what its name names after all.
func TestARunOfFileStartsTheOpenFileNotWhatItsNameNamesLater(t *testing.T) {
	t.Parallel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	opened := exe
	if fileRuns {
		opened = filepath.Join(t.TempDir(), "program")
		if err := os.Symlink(exe, opened); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.Open(opened)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	run := func(mode string, args ...string) Cmd {
		c := helperCmd(t, mode, args...)
		c.Path, c.File = "", f
		return c
	}
	if !fileRuns {
		res, err := Run(t.Context(), run("argv"))
		if !errors.Is(err, errNoFileRun) || !errors.Is(err, ErrRefused) || !errors.Is(err, errors.ErrUnsupported) || res.Code != -1 {
			t.Errorf("Run with File: %v, Code %d, want errNoFileRun, ErrRefused, errors.ErrUnsupported and Code -1", err, res.Code)
		}
		return
	}
	swap := filepath.Join(filepath.Dir(opened), "swap")
	if err := writeExe(swap, []byte("no program\n")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(swap, opened); err != nil {
		t.Fatal(err)
	}
	res, err := Run(t.Context(), run("argv"))
	if argv := strings.Split(string(res.Stdout), "\x00"); err != nil || res.Code != 0 || argv[0] != opened {
		t.Errorf("Run with File after the swap: %v, Code %d, stderr %q, argv %q, want exit 0 and argv[0] %q", err, res.Code, res.Stderr, argv, opened)
	}
	control := helperCmd(t, "argv")
	control.Path = opened
	if res, err := Run(t.Context(), control); err == nil || errors.Is(err, ErrRefused) {
		t.Errorf("Run with Path at the swapped name: %v, Code %d, want it to start the swapped file and fail", err, res.Code)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	c := run("extra", inherited(t, w), "through")
	c.ExtraFiles = []*os.File{w}
	res, err = Run(t.Context(), c)
	if closeErr := w.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	got, readErr := io.ReadAll(r)
	if err != nil || res.Code != 0 || readErr != nil || string(got) != "through" {
		t.Errorf("Run with File and ExtraFiles: %v, Code %d, stderr %q, the pipe carried %q (%v), want exit 0 and %q", err, res.Code, res.Stderr, got, readErr, "through")
	}
}

// TestFileIsRefusedWithPathAsAScriptOrUnread: a run with File is refused, Code -1, with Path set
// too on every system, and on Linux when File begins with #!, when its start cannot be read (a
// write-only file, or one closed before Run), and when /proc/self/fd is no proc mount (a folder in
// its place, by the seam, that holds a script by the name the child would exec); on another system
// those are refused as any File is (errNoFileRun).
//
// What it costs when this fails: a run starts the interpreter a script names by a path, a file it
// could not check, or a file planted where /proc/self/fd would be, each a program File does not
// hold open; or Path and File disagree on the program and one wins unseen.
func TestFileIsRefusedWithPathAsAScriptOrUnread(t *testing.T) {
	t.Parallel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "script")
	if err := writeExe(script, []byte("#!/bin/sh\nexit 0\n")); err != nil {
		t.Fatal(err)
	}
	open := func(path string, flag int) *os.File {
		f, err := os.OpenFile(path, flag, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		return f
	}
	// planted is a folder that is no proc mount, holding by the name a File run's child execs below
	// it, self/fd/3, a script that exits 7 if it is what runs; linked is a link to the real /proc.
	planted := t.TempDir()
	if err := os.MkdirAll(filepath.Join(planted, "self", "fd"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeExe(filepath.Join(planted, "self", "fd", "3"), []byte("#!/bin/sh\nexit 7\n")); err != nil {
		t.Fatal(err)
	}
	// Off Linux, where any File is refused first, the row needs no link, and Windows makes one
	// only with a privilege.
	linked := planted
	if fileRuns {
		linked = filepath.Join(t.TempDir(), "proc")
		if err := os.Symlink("/proc", linked); err != nil {
			t.Fatal(err)
		}
	}
	closed := open(exe, os.O_RDONLY)
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	rows := []struct {
		name    string
		file    *os.File
		path    string
		procDir string
		linux   error
	}{
		{"with Path", open(exe, os.O_RDONLY), exe, "", errFilePath},
		{"a script", open(script, os.O_RDONLY), "", "", errFileScript},
		{"unread", open(script, os.O_WRONLY), "", "", errFileUnread},
		{"closed", closed, "", "", errFileUnread},
		// A folder of the test's own filesystem, not a proc mount, stands where /proc is, as in a
		// root with no proc mount, holding a script by the name the child would exec; and a link to
		// a proc mount stands there, as one planted where /proc is no mount could be.
		{"no proc mount", open(exe, os.O_RDONLY), "", planted, errFileNoProc},
		{"a link to proc", open(exe, os.O_RDONLY), "", linked, errFileNoProc},
	}
	for _, r := range rows {
		c := helperCmd(t, "argv")
		c.Path, c.File = r.path, r.file
		if r.procDir != "" {
			c.seam = &seams{procDir: r.procDir}
		}
		want := r.linux
		if !fileRuns && !errors.Is(r.linux, errFilePath) {
			want = errNoFileRun
		}
		if res, err := Run(t.Context(), c); !errors.Is(err, want) || !errors.Is(err, ErrRefused) || res.Code != -1 {
			t.Errorf("%s: %v, Code %d, want %v and Code -1", r.name, err, res.Code, want)
		}
	}
}
