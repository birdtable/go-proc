//go:build linux || darwin || windows

package proc

import (
	"bufio"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// lines reads n lines from r, each a word, a space and a value, as the tree and detach helpers write
// them, and gives each value by its word. A line of any other form fails the test: a helper's error
// says itself there.
func lines(t *testing.T, r *bufio.Reader, n int) map[string]string {
	t.Helper()
	got := map[string]string{}
	within(t, "reading the helpers' lines", func() {
		for range n {
			line, err := r.ReadString('\n')
			word, value, ok := strings.Cut(strings.TrimSuffix(line, "\n"), " ")
			if err != nil || !ok {
				t.Errorf("a helper wrote %q (%v), want a word and a value; the lines so far: %q", line, err, got)
				return
			}
			got[word] = value
		}
	})
	if t.Failed() {
		t.FailNow()
	}
	return got
}

// drained reads r to its end, which comes once every process that holds the pipe's write end has
// ended, and fails the test when it has not with most of the test's time gone.
func drained(t *testing.T, what string, r io.Reader) {
	t.Helper()
	within(t, what, func() {
		if rest, err := io.ReadAll(r); err != nil || len(rest) != 0 {
			t.Errorf("%s: read %q to the end (%v), want nothing more", what, rest, err)
		}
	})
}

// treeHelper starts the tree helper detached, with how it ends, its output to a pipe the test reads,
// and gives the tree and the pipe's read end. The test's copy of the write end is closed, so the
// read end ends once the leader and the grandchild do.
func treeHelper(t *testing.T, how string) (Tree, *bufio.Reader) {
	t.Helper()
	return helperTree(t, "tree", how, strconv.Itoa(os.Getpid()))
}

// helperTree starts the helper mode with args detached, as treeHelper starts the tree helper.
func helperTree(t *testing.T, mode string, args ...string) (Tree, *bufio.Reader) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	tree, err := Start(Detached{Path: exe, Args: append([]string{helperMode + mode}, args...), Output: w})
	_ = w.Close()
	if err != nil {
		t.Fatal(err)
	}
	return tree, bufio.NewReader(r)
}

// interruptTree is Interrupt by k, as stopTree is Stop by k.
func interruptTree(id TreeID, k killer) error { return interrupt(id, procFS{dir: procMount}, k) }

// guardedAct is Stop's act or Interrupt's on a tree id by a killer, by its name.
type guardedAct struct {
	name string
	act  func(TreeID, killer) error
}

// guardedActs are Stop's act and Interrupt's, for a row that wants both to refuse an id alike, under
// the one guard, and signal nothing.
func guardedActs() []guardedAct {
	return []guardedAct{{"stopTree", stopTree}, {"interruptTree", interruptTree}}
}

// TestADetachedTreeOutlivesItsStarterAndIsStoppedByItsID: the detach helper, A, starts a tree
// detached, writes its id and exits; the tree's leader has no console and no controlling terminal
// and is out of A's session, and is still the tree's leader once A has exited and been reaped. The
// test, a second process, stops the tree by its id, and the leader and the grandchild it started
// are gone: the pipe they hold ends.
//
// What it costs when this fails: a job a CLI promised would run on after the window closed dies with
// the window, or a second process told to stop the job leaves the job's children running.
func TestADetachedTreeOutlivesItsStarterAndIsStoppedByItsID(t *testing.T) {
	t.Parallel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	a := &process{Path: exe, Args: []string{exe, helperMode + "detach", strconv.Itoa(os.Getpid())}, Stdout: w, Stderr: w}
	err = startTestProcess(a)
	_ = w.Close()
	if err != nil {
		t.Fatal(err)
	}
	got := lines(t, bufio.NewReader(r), 3)
	id := TreeID(got["id"])
	t.Cleanup(func() { _ = Stop(id) })
	if err := a.Wait(); err != nil {
		t.Fatalf("A, which starts the tree and exits: %v", err)
	}
	if got["console"] != "none" {
		t.Errorf("the detached leader has a console or a controlling terminal (%q), want none", got["console"])
	}
	if _, err := strconv.Atoi(got["grandchild"]); err != nil {
		t.Fatalf("the leader wrote %q for its grandchild's pid: %v", got["grandchild"], err)
	}
	outOfSession(t, id)
	if err := Stop(id); err != nil {
		t.Fatalf("Stop(%q) with A gone: %v", id, err)
	}
	drained(t, "the tree's leader and grandchild, once stopped", r)
}

// TestAStaleTreeIDIsRefusedAndSignalsNothing: a tree whose leader exited and was reaped is refused
// with ErrStale by Stop and by Interrupt, and neither signals anything: its grandchild still runs,
// and is ended by the test.
//
// What it costs when this fails: a stop or an interrupt by an old id reaches whatever took the
// reaped leader's pid, and kills or interrupts a process group or a job the caller never started.
func TestAStaleTreeIDIsRefusedAndSignalsNothing(t *testing.T) {
	t.Parallel()
	tree, r := treeHelper(t, "exit")
	staleRefused(t, tree, r)
}

// staleRefused is the stale row's check of tree, the tree helper's of "exit", whose lines are read
// from r: its leader is reaped, Stop and Interrupt of its id are ErrStale and signal nothing, and its
// grandchild still runs, ended here.
func staleRefused(t *testing.T, tree Tree, r *bufio.Reader) {
	t.Helper()
	got := lines(t, r, 2)
	gc, err := strconv.Atoi(got["grandchild"])
	if err != nil {
		t.Fatalf("the leader wrote %q for its grandchild's pid: %v", got["grandchild"], err)
	}
	if state, err := tree.Leader.Wait(); err != nil || !state.Success() {
		t.Fatalf("the leader, which exits 0: %v, %v", state, err)
	}
	for _, g := range guardedActs() {
		var calls atomic.Int32
		if err := g.act(tree.ID, counted(&calls)); !errors.Is(err, ErrStale) {
			t.Errorf("%s(%q) with its leader reaped: %v, want ErrStale", g.name, tree.ID, err)
		}
		if n := calls.Load(); n != 0 {
			t.Errorf("%s(%q) with its leader reaped signalled %d times, want none", g.name, tree.ID, n)
		}
	}
	stillThere(t, tree, gc)
	drained(t, "the grandchild, once ended", r)
}

// startedHelper starts d, the helper's touch of mark, detached, and wants it run: a tree, its leader
// exiting 0 once reaped, and mark made.
func startedHelper(t *testing.T, name string, d Detached, mark string) {
	t.Helper()
	tree, err := Start(d)
	if err != nil {
		t.Errorf("%s: Start of Path %q in Dir %q: %v, want it started", name, d.Path, d.Dir, err)
		return
	}
	if state, err := tree.Leader.Wait(); err != nil || !state.Success() {
		t.Errorf("%s: the leader at Path %q in Dir %q: %v, %v, want it to exit 0", name, d.Path, d.Dir, state, err)
		return
	}
	if _, err := os.Stat(mark); err != nil {
		t.Errorf("%s: Path %q in Dir %q started and made no file: %v", name, d.Path, d.Dir, err)
	}
}

// refusedStart starts d, the helper's touch of mark, detached, and wants it refused with want,
// wrapping ErrRefused, with no tree and mark not made.
func refusedStart(t *testing.T, name string, d Detached, mark string, want error) {
	t.Helper()
	tree, err := Start(d)
	if tree.Leader != nil {
		_, _ = tree.Leader.Wait()
	}
	if !errors.Is(err, want) || !errors.Is(err, ErrRefused) || tree != (Tree{}) {
		t.Errorf("%s: Start of Path %q in Dir %q gave %+v and %v, want no tree and %v", name, d.Path, d.Dir, tree, err, want)
	}
	if _, err := os.Stat(mark); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s: Start of Path %q in Dir %q started it (%v), want nothing started", name, d.Path, d.Dir, err)
	}
}

// touching is the helper's touch of mark, as a Detached at path in dir, its Env Bare's with GORACE,
// as helperCmd's is.
func touching(t *testing.T, path, dir, mark string) Detached {
	t.Helper()
	return Detached{Path: path, Dir: dir, Args: []string{helperMode + "touch", mark}, Env: Bare("GORACE")}
}

// TestStartRefusesAPathRunRefuses: Start refuses each Path Run refuses, before it starts anything,
// with Run's error, wrapping ErrRefused, and no tree: an empty Path, a lone -, a Path that holds =,
// and a bare name; on Windows a batch file or a script, and a drive with no root after it or a root
// with no drive before it.
//
// What it costs when this fails: a detached start of a name a search resolves, or of a script a shell
// reads, starts a program its caller did not name, out of reach of any later stop.
func TestStartRefusesAPathRunRefuses(t *testing.T) {
	t.Parallel()
	rows := []struct {
		path string
		want error
	}{
		{"", errEmptyPath},
		{"-", errDashPath},
		{"=", errEqualsPath},
		{"prog", errRelativePath},
	}
	if programsOnly {
		rows = append(rows, []struct {
			path string
			want error
		}{
			{`C:\tools\a.bat`, ErrBatch},
			{`C:\tools\a.cmd`, ErrBatch},
			{`C:\tools\a.ps1`, ErrNotProgram},
			{`C:prog.exe`, errNotUnderDir},
			{`\prog.exe`, errNotUnderDir},
		}...)
	}
	for _, r := range rows {
		tree, err := Start(Detached{Path: r.path})
		if !errors.Is(err, r.want) || !errors.Is(err, ErrRefused) || tree != (Tree{}) {
			t.Errorf("Start(%q) gave %+v and %v, want no tree and %v", r.path, tree, err, r.want)
		}
	}
	if len(rows) != 4 && len(rows) != 9 {
		t.Errorf("%d rows", len(rows))
	}
}

// TestStartTakesARelativePathWithASeparatorResolvedAgainstDir: Start takes a relative Path with a
// separator as Run takes one, resolved against Dir: bin/helper, ./bin/helper, ./bin/helper written
// with / on every system, and ../bin/helper from a folder below each start the helper, which makes
// its mark. The helper is the test binary, placed under a folder of the test's own, and nothing is
// at bin/helper in this process's folder, so a start that ran proves Dir was read.
//
// What it costs when this fails: a caller that names a detached program as os/exec and Run allow,
// by a path relative to the folder it runs in, is refused, or starts a program
// found somewhere else.
func TestStartTakesARelativePathWithASeparatorResolvedAgainstDir(t *testing.T) {
	t.Parallel()
	sep := string(filepath.Separator)
	root := t.TempDir()
	helper := placed(t, root, filepath.Join("bin", "helper"))
	if _, err := os.Stat(helper); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("this process's folder holds %s (%v), so a start would not prove Dir is read", helper, err)
	}
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
		startedHelper(t, r.name, touching(t, r.path, r.dir, mark), mark)
	}
	if len(rows) != 4 {
		t.Errorf("%d rows", len(rows))
	}
}

// TestStartWithNoDirTakesARelativePathFromThisFolderAndRefusesABareName: with Dir empty, Start
// resolves a relative Path with a separator against this process's folder, and starts it. A bare
// name is refused with errRelativePath, and starts nothing, though a program of that name is in
// this process's folder: with Dir empty, and with Dir set to this folder.
//
// What it costs when this fails: a caller whose Dir is empty is refused a path Run takes; or a word
// starts the first program PATH, Dir or this folder holds by that name (exec.ErrDot's class).
func TestStartWithNoDirTakesARelativePathFromThisFolderAndRefusesABareName(t *testing.T) {
	// serial: t.Chdir changes the folder of the whole process.
	sep := string(filepath.Separator)
	root := t.TempDir()
	helper := placed(t, root, filepath.Join("bin", "helper"))
	name := placed(t, root, "helper")
	t.Chdir(root)
	mark := filepath.Join(t.TempDir(), "started")
	startedHelper(t, "a relative Path with Dir empty", touching(t, "."+sep+helper, "", mark), mark)
	rows := []struct{ name, dir string }{
		{"a bare name in this folder, with Dir empty", ""},
		{"a bare name in Dir, which is this folder", root},
	}
	for _, r := range rows {
		mark := filepath.Join(t.TempDir(), "started")
		refusedStart(t, r.name, touching(t, name, r.dir, mark), mark, errRelativePath)
	}
	if len(rows) != 2 {
		t.Errorf("%d rows", len(rows))
	}
}

// endsWithin is how soon a detached tree's holders end once the test binary that started the tree
// has: a few seconds, far under holdFor.
const endsWithin = 10 * time.Second

// TestADetachedTreeEndsWithTheTestBinaryThatStartedIt: a stand-in for a test binary starts a tree
// detached, whose leader and grandchild hold, each given the stand-in's pid. The stand-in is killed
// while both hold. The tree is out of its process group and job, as Detached means, yet both end
// within endsWithin, long before holdFor, since each holder watches the binary that started its tree.
//
// What it costs when this fails: a test binary killed by a timeout, a session or a person leaves its
// detached trees holding the machine for holdFor.
func TestADetachedTreeEndsWithTheTestBinaryThatStartedIt(t *testing.T) {
	t.Parallel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	standIn := &process{Path: exe, Args: []string{exe, helperMode + "detach-hold", strconv.Itoa(os.Getpid())}, Stdout: w, Stderr: w}
	err = startTestProcess(standIn)
	_ = w.Close()
	if err != nil {
		t.Fatal(err)
	}
	reaped := false
	t.Cleanup(func() {
		// Only while it is unreaped: its pid then names it and no later process.
		if !reaped {
			_ = endTestProcess(standIn)
			_ = standIn.Wait()
		}
	})
	got := lines(t, bufio.NewReader(r), 4)
	id := TreeID(got["id"])
	t.Cleanup(func() { _ = Stop(id) }) // a row that fails leaves nothing holding
	holders := []string{"leader", "grandchild"}
	ends := make([]<-chan error, 0, len(holders))
	for _, word := range holders {
		pid, err := strconv.Atoi(got[word])
		if err != nil {
			t.Fatalf("the %s's pid %q: %v", word, got[word], err)
		}
		end, err := processEnd(pid)
		if err != nil {
			t.Fatal(err)
		}
		ends = append(ends, end)
	}
	if err := endTestProcess(standIn); err != nil {
		t.Fatalf("killing the stand-in: %v", err)
	}
	_ = standIn.Wait() // its end is the kill's
	reaped = true
	within := time.NewTimer(endsWithin)
	defer within.Stop()
	for i, word := range holders {
		select {
		case err := <-ends[i]:
			if err != nil {
				t.Errorf("watching the detached tree's %s: %v", word, err)
			}
		case <-within.C:
			t.Fatalf("the detached tree's %s still holds %v after the stand-in that started it was killed, want it ended", word, endsWithin)
		}
	}
}

// TestStartRefusesAPathThatBeginsWithADashAndStartsItAfterDotSlash: Start refuses a relative Path
// that begins with -, -x/tool, with errDashPath and no tree, though the file is there under Dir; the
// same file written after ./, ./-x/tool, starts.
//
// What it costs when this fails: a caller that places Path after an env-style launcher has it read as
// an option, and the launcher starts the next word in its place, out of reach of any later stop.
func TestStartRefusesAPathThatBeginsWithADashAndStartsItAfterDotSlash(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	tool := placed(t, root, filepath.Join("-x", "tool"))
	mark := filepath.Join(t.TempDir(), "started")
	refusedStart(t, "a Path that begins with -", touching(t, tool, root, mark), mark, errDashPath)
	startedHelper(t, "the same file after ./", touching(t, "."+string(filepath.Separator)+tool, root, mark), mark)
}
