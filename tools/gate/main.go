// Command gate is the Go that Taskfile.yml calls in place of shell: the
// members whose commands had pipes, awk, grep, shell loops or a temporary file joined in shell. It
// runs from the repository's top, as
//
//	go -C tools/gate run . -top <the repository's top> <step> [arguments]
//
// and its steps are lint, layers, test-short, namespace, psx and notice-check, each the member of
// that name, or for psx the first command of licenses. A step's exit is 0 when it holds, 1 when it
// does not or could not finish, and 2 for arguments it does not take. It is a module of its own, as
// the tools are, so that go-proc's go.mod, its lint and its layer rows are as they were, and task
// size leaves it out. Its own test is task taskfile's: Taskfile.yml calls Go, by go-testkit's
// taskfile check.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// step is one member: its arguments, and where its lines go.
type step func(ctx context.Context, args []string, stdout, stderr io.Writer) error

// steps are the steps by name.
func steps() map[string]step {
	return map[string]step{
		"lint":         lintStep,
		"layers":       layersStep,
		"test-short":   testShortStep,
		"namespace":    namespaceStep,
		"psx":          psxStep,
		"notice-check": noticeStep,
	}
}

// errRed is a step that did not hold, whose reason it has already said.
var errRed = errors.New("gate: red")

// errUsage is a step given arguments it does not take, which it has already said.
var errUsage = errors.New("gate: usage")

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) >= 2 && args[0] == "-top" {
		if err := os.Chdir(args[1]); err != nil {
			fmt.Fprintf(stderr, "gate: %v\n", err)
			return 1
		}
		args = args[2:]
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, "gate: a step is the first argument: lint, layers, test-short, namespace, psx or notice-check")
		return 2
	}
	s, ok := steps()[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "gate: no step %q\n", args[0])
		return 2
	}
	switch err := s(ctx, args[1:], stdout, stderr); {
	case err == nil:
		return 0
	case errors.Is(err, errUsage):
		return 2
	case errors.Is(err, errRed):
		return 1
	default:
		fmt.Fprintf(stderr, "gate: %s: %v\n", args[0], err)
		return 1
	}
}

// command is name with args, run at the top with env set over the task's own environment. A child
// still running when the gate is told to stop is killed, and its pipes are let go a second after.
func command(ctx context.Context, env []string, name string, args ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, name, args...) //nolint:gosec // the tier's own tools, go and git, with the gate's arguments
	c.Env = append(os.Environ(), env...)
	c.WaitDelay = time.Second
	return c
}

// tool is the path of a pinned tool, built from tools/<name>.mod by go tool, by its own name.
func tool(ctx context.Context, name string) (string, error) {
	out, err := command(ctx, []string{"CGO_ENABLED=0"}, "go", "tool", "-modfile=tools/"+name+".mod", "-n", name).Output()
	if err != nil {
		return "", fmt.Errorf("go tool -n %s: %w", name, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// noArgs refuses any argument, for a step that takes none.
func noArgs(name string, args []string, stderr io.Writer) error {
	if len(args) != 0 {
		fmt.Fprintf(stderr, "gate: %s takes no arguments, and was given %q\n", name, args)
		return errUsage
	}
	return nil
}

// result is one child's end: what it printed and whether it failed.
type result struct {
	out []byte
	err error
}

// together runs one golangci-lint per system at once, each with its own args and GOOS, and gives
// each its output and its error, in the order of systems.
func together(ctx context.Context, bin string, systems []string, args ...string) []result {
	res := make([]result, len(systems))
	var wg sync.WaitGroup
	for i, goos := range systems {
		wg.Go(func() {
			out, err := command(ctx, []string{"CGO_ENABLED=0", "GOOS=" + goos}, bin, args...).CombinedOutput()
			res[i] = result{out, err}
		})
	}
	wg.Wait()
	return res
}

// report prints each system's output, led by its name, and is errRed if any of them failed.
func report(stdout io.Writer, name string, systems []string, res []result) error {
	red := false
	for i, r := range res {
		if len(r.out) != 0 || r.err != nil {
			fmt.Fprintf(stdout, "%s: %s\n%s", name, systems[i], r.out)
		}
		if r.err != nil {
			fmt.Fprintf(stdout, "%s: %s: %v\n", name, systems[i], r.err)
			red = true
		}
	}
	if red {
		return errRed
	}
	return nil
}

// lintStep is golangci-lint for each of the three systems, run together, as the lint member was.
// Its lint's timeout is the member's.
func lintStep(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if err := noArgs("lint", args, stderr); err != nil {
		return err
	}
	bin, err := tool(ctx, "golangci-lint")
	if err != nil {
		return err
	}
	systems := []string{"linux", "darwin", "windows"}
	return report(stdout, "lint", systems, together(ctx, bin, systems, "run", "--allow-parallel-runners", "./..."))
}

// goraceTests is GORACE for the tests: atexit_sleep_ms=0 after whatever the caller's GORACE holds,
// since the tests start their race-built binary as their helpers, and each helper's exit would
// otherwise be held a second.
func goraceTests() string {
	if g := os.Getenv("GORACE"); g != "" {
		return g + " atexit_sleep_ms=0"
	}
	return "atexit_sleep_ms=0"
}

// event is the part of one line of go test's JSON a step reads.
type event struct {
	Action  string
	Package string
	Test    string
	Output  string
}

// key is a test by package and name.
type key struct{ pkg, test string }

func (k key) String() string { return k.pkg + " " + k.test }

// events reads go test's JSON, one event a line. A line that is not an event stops it: gotestsum
// wrote the file, so a line it could not have written means the file is not what the step reads.
func events(r io.Reader) ([]event, error) {
	var evs []event
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e event
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("line %d of go test's JSON: %w", n, err)
		}
		evs = append(evs, e)
	}
	return evs, sc.Err()
}

// releaseTier is the line a test prints, through t.Skip, to say it runs in the release tier: its
// file and line as testing writes them, then "release tier: ".
var releaseTier = regexp.MustCompile(`^    [^"]*_test\.go:[0-9]+: release tier: `)

// skipped is each test that skipped, by package and name, sorted. With marked, only the ones whose
// own output holds no line that says it runs in the release tier.
func skipped(evs []event, marked bool) []key {
	skip := map[key]bool{}
	said := map[key]bool{}
	for _, e := range evs {
		if e.Test == "" {
			continue
		}
		k := key{e.Package, e.Test}
		switch e.Action {
		case "skip":
			skip[k] = true
		case "output":
			if releaseTier.MatchString(e.Output) {
				said[k] = true
			}
		}
	}
	var out []key
	for k := range skip {
		if !marked || !said[k] {
			out = append(out, k)
		}
	}
	slices.SortFunc(out, func(a, b key) int { return strings.Compare(a.String(), b.String()) })
	return out
}

// gotestsum runs gotestsum, pinned in tools/gotestsum.mod, with its JSON in a folder of the system's
// temporary folder, never the module, as a file the run writes in the module's tree changes what the
// tests read and go test then runs their packages again. prefix comes before go tool, and args are
// go test's. It gives the JSON's events once the run has passed.
func gotestsum(ctx context.Context, stdout, stderr io.Writer, prefix []string, args ...string) ([]event, error) {
	dir, err := os.MkdirTemp("", "go-proc-gate-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	f := filepath.Join(dir, "tests.json")
	argv := append(slices.Clone(prefix), "go", "tool", "-modfile=tools/gotestsum.mod", "gotestsum", "--format", "testname", "--jsonfile", f, "--")
	argv = append(argv, args...)
	c := command(ctx, []string{"CGO_ENABLED=1", "GORACE=" + goraceTests()}, argv[0], argv[1:]...)
	c.Stdout, c.Stderr = stdout, stderr
	if err := c.Run(); err != nil {
		return nil, errRed
	}
	j, err := os.Open(f)
	if err != nil {
		return nil, err
	}
	defer func() { _ = j.Close() }()
	return events(j)
}

// testShortStep is the push tier's suite, -race and -short, from the test cache where nothing
// changed. A skip passes only where the test's own output says it runs in the release tier.
func testShortStep(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if err := noArgs("test-short", args, stderr); err != nil {
		return err
	}
	evs, err := gotestsum(ctx, stdout, stderr, nil, "-race", "-short", "./...")
	if err != nil {
		return err
	}
	if bad := skipped(evs, true); len(bad) != 0 {
		fmt.Fprintln(stdout, "a test skipped that does not say it runs in the release tier; a skip is a failure here:")
		for _, k := range bad {
			fmt.Fprintln(stdout, k)
		}
		return errRed
	}
	return nil
}

// namespaceStep runs packages' tests in a user and pid namespace of their own, under tini, an init
// that reaps. On any system but Linux it says it has nothing to run and holds. -cached takes their
// result from the test cache, as the push tier alone asks; without it they run with -count=1 and
// -shuffle=on. Any skip fails.
func namespaceStep(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("namespace", flag.ContinueOnError)
	set.SetOutput(stderr)
	cached := set.Bool("cached", false, "take the tests' result from the test cache where nothing changed")
	if err := set.Parse(args); err != nil {
		return errUsage
	}
	pkgs := set.Args()
	if len(pkgs) == 0 {
		fmt.Fprintln(stderr, "gate: namespace takes the packages to run, and was given none")
		return errUsage
	}
	if runtime.GOOS != "linux" {
		fmt.Fprintf(stdout, "namespace: nothing to run on %s, which has no pid namespace\n", runtime.GOOS)
		return nil
	}
	var prefix []string
	for _, t := range []struct{ name, pkg string }{{"unshare", "util-linux"}, {"tini", "tini"}} {
		p, err := exec.LookPath(t.name)
		if err != nil {
			fmt.Fprintf(stderr, "namespace: %s is not installed: install the package named %s\n", t.name, t.pkg)
			return errRed
		}
		if t.name == "unshare" {
			prefix = append(prefix, p, "--user", "--map-current-user", "--pid", "--fork", "--mount-proc", "--kill-child")
		} else {
			prefix = append(prefix, p, "-s", "--")
		}
	}
	var test []string
	if !*cached {
		test = append(test, "-count=1", "-shuffle=on")
	}
	test = append(test, "-race")
	test = append(test, pkgs...)
	evs, err := gotestsum(ctx, stdout, stderr, prefix, test...)
	if err != nil {
		return err
	}
	if bad := skipped(evs, false); len(bad) != 0 {
		fmt.Fprintln(stdout, "a test skipped; a skip is a failure here:")
		for _, k := range bad {
			fmt.Fprintln(stdout, k)
		}
		return errRed
	}
	return nil
}

// outside is each line "<import path> <module path>" of go list whose package lies under mod's path
// but whose module is not mod: a package go-licenses' --ignore of mod, which goes by prefix, would
// pass though it is not of that module.
func outside(list, mod string) []string {
	var out []string
	for line := range strings.Lines(list) {
		f := strings.Fields(line)
		if len(f) == 2 && strings.HasPrefix(f[0], mod) && f[1] != mod {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return out
}

// psxStep fails on any package of the build under the path of the module it is given whose module is
// not that one, so that licenses' --ignore of it reaches that module and no other.
func psxStep(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) != 1 || args[0] == "" {
		fmt.Fprintf(stderr, "gate: psx takes one module path, and was given %q\n", args)
		return errUsage
	}
	mod := args[0]
	c := command(ctx, nil, "go", "list", "-deps", "-f", "{{if .Module}}{{.ImportPath}} {{.Module.Path}}{{end}}", "./...")
	c.Stderr = stderr
	list, err := c.Output()
	if err != nil {
		return fmt.Errorf("go list: %w", err)
	}
	if bad := outside(string(list), mod); len(bad) != 0 {
		fmt.Fprintf(stderr, "licenses: a package under %s's path that is not of that module, which its ignore would pass:\n", mod)
		for _, l := range bad {
			fmt.Fprintln(stderr, l)
		}
		return errRed
	}
	return nil
}

// noticeStep hands measure notice-check the modules of the non-test build on linux, darwin and
// windows, as go list prints them, on its standard input. A go list that fails fails the step, and
// one that prints nothing makes measure refuse.
func noticeStep(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if err := noArgs("notice-check", args, stderr); err != nil {
		return err
	}
	var mods bytes.Buffer
	for _, goos := range []string{"linux", "darwin", "windows"} {
		c := command(ctx, []string{"GOOS=" + goos}, "go", "list", "-deps", "-f", "{{with .Module}}{{.Path}} {{.Version}}{{end}}", "./...")
		c.Stderr = stderr
		out, err := c.Output()
		if err != nil {
			return fmt.Errorf("go list for %s: %w", goos, err)
		}
		mods.Write(bytes.TrimRight(out, "\n"))
		mods.WriteByte('\n')
	}
	c := command(ctx, []string{"CGO_ENABLED=0"}, "go", "run", "./internal/cmd/measure", "notice-check")
	c.Stdin, c.Stdout, c.Stderr = &mods, stdout, stderr
	if err := c.Run(); err != nil {
		return errRed
	}
	return nil
}
