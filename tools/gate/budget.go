package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The budget step of each tier, which was gate/budget.sh. Taskfile.yml calls it with the tier and
// the step in literal words:
//
//	budget pre-commit start $$
//	budget pre-commit end $$
//	budget pre-push cpu
//
// pre-commit and release start and end with it. start reads the 1-minute load with hatch, then
// writes the tier's start in seconds and that load to .task/budget/<tier>.<run>. The run is go-task's
// own pid, $$ in its interpreter: every command of one run has it, no run beside it does, and no
// caller's environment sets it, so two runs of one tier in one tree each time themselves. end reads
// that file, removes it, and has `hatch budget` (tools/hatch.mod) hold the elapsed seconds to the
// tier's budget in budgets.yaml. hatch holds no policy, so this file holds go-proc's: which row
// applies on this kind of host, and that a pre-commit whose load at its start was above 15 passes
// past its budget, saying so. A release past its budget fails at any load.
//
// pre-push is one step, cpu: `hatch budget --cpu` runs the tier's members, `task <tier>-members`, in
// a tree of its own, and holds their CPU time, user plus system, to the tier's <tier>-cpu_limit row.
// CPU time is the same on a quiet machine and a loaded one, so no load excuses anything: an overrun
// fails at any load, and one under its CPU row passes over its wall row. The wall row, <tier>-budget,
// is the hang guard only: hatch kills a tree still running at ten times it, and fails it as hung.
// hatch prints the CPU time, the wall time and the load. On a kind of host with no row the members
// run alone and the time is printed. No tier skips, and none is tried again.
//
// Nothing here is taken from a Task var or the caller's environment: a start or load kept in a var
// would be whatever the caller's environment said, as `LOAD=16 git push`. The tier, the step and the
// run are the Taskfile's words and go-task's pid. GITHUB_ACTIONS is read, as the one name from the
// environment that decides anything: it marks a hosted runner, whose job's timeout holds it, so a
// release session's budget is not applied there.

// budgetTiers are the tiers the step times.
var budgetTiers = []string{"pre-commit", "pre-push", "release"}

// sessionHosts are the kinds of host a release session runs on, where a release off a hosted runner
// is held to release-budget-session.
var sessionHosts = []string{"linux-amd64", "windows-amd64"}

// coldNote follows a red of the CPU step. hatch adds no --advice under --cpu, so the note is the
// step's own. Every red of hatch exits 1, a member's failure as much as an overrun, so the note says
// which red it is about. The row is measured warm: a cold build cache, after a module or toolchain
// bump or a cleared cache, builds what a warm push reuses (budgets.yaml has both figures). Its push
// fails all the same, and the next push is judged in full: nothing is waived and no hook is skipped.
const coldNote = "budget: if %[1]s is over its CPU budget above, a cold build cache can explain it: the first push after a module or toolchain bump, or a cleared cache, builds what a warm push reuses, and %[1]s measured 434 s of CPU on an empty build cache against 50 s warm. Push again: the second push reads the cache warm and is judged in full. Past the row warm, it is a speed bug to find; budgets.yaml has the row\n"

// The rows hatch reads in budgets.yaml: a key, a colon, a space and whole seconds. A wall row's id is
// its key; a CPU row's id is the tier, before -cpu_limit. The CPU step's wall rows are <tier>-budget,
// whose id is the tier.
const (
	wallRow    = `^(?P<id>[a-z-]+): (?P<limit>[0-9]+)$`
	cpuWallRow = `^(?P<id>[a-z-]+)-budget: (?P<limit>[0-9]+)$`
	cpuRow     = `^(?P<id>[a-z-]+)-cpu_limit: (?P<limit>[0-9]+)$`
	advice     = "a tier over its budget is a speed bug to find; budgets.yaml has the budgets"
)

// budgeter is one budget step's world: where it reads and writes, the kind of host it runs on, and
// the command that runs a tier's members. budgetStep fills it from the machine; a test, from its own.
type budgeter struct {
	top     string                     // the repository's top: hatch and the members run there
	budgets string                     // budgets.yaml, under top or absolute
	starts  string                     // the folder of the start files, under top or absolute
	host    string                     // the kind of host, <os>-<arch> as Go names them
	hosted  bool                       // GITHUB_ACTIONS is set: a hosted runner
	members func(tier string) []string // the command that runs the tier's members
	now     func() time.Time
	stdout  io.Writer
	stderr  io.Writer
}

// budgetStep is the budget step: budget <tier> start|end <run>, or budget <tier> cpu.
func budgetStep(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	b := &budgeter{
		top:     ".",
		budgets: "budgets.yaml",
		starts:  filepath.Join(".task", "budget"),
		host:    runtime.GOOS + "-" + runtime.GOARCH,
		hosted:  os.Getenv("GITHUB_ACTIONS") != "",
		members: func(tier string) []string {
			return []string{"go", "tool", "-modfile=tools/task.mod", "task", tier + "-members"}
		},
		now:    time.Now,
		stdout: stdout,
		stderr: stderr,
	}
	return b.run(ctx, args)
}

func (b *budgeter) run(ctx context.Context, args []string) error {
	if len(args) < 2 || !slices.Contains(budgetTiers, args[0]) {
		fmt.Fprintf(b.stderr, "gate: budget takes a tier, pre-commit, pre-push or release, then start or end with the run, or cpu, and was given %q\n", args)
		return errUsage
	}
	tier, which := args[0], args[1]
	switch which {
	case "start", "end":
		if len(args) != 3 || !wholeNumber(args[2]) {
			fmt.Fprintf(b.stderr, "gate: budget %s %s takes the run, go-task's pid ($$), and was given %q\n", tier, which, args[2:])
			return errUsage
		}
		f := filepath.Join(b.starts, tier+"."+args[2])
		if which == "start" {
			return b.start(ctx, f)
		}
		return b.end(ctx, tier, f)
	case "cpu":
		if len(args) != 2 {
			fmt.Fprintf(b.stderr, "gate: budget %s cpu takes nothing more, and was given %q\n", tier, args[2:])
			return errUsage
		}
		return b.cpu(ctx, tier)
	}
	fmt.Fprintf(b.stderr, "gate: budget's step is %q, and wants start, end or cpu\n", which)
	return errUsage
}

// wholeNumber is whether s is whole digits with no leading 0, which a shell's $(( )) and hatch's
// --since would read as octal.
func wholeNumber(s string) bool {
	if s == "" || s[0] == '0' {
		return false
	}
	return strings.Trim(s, "0123456789") == ""
}

// loadShape is a load as hatch prints one, digits with one dot inside, or nothing, so no spelling a
// number parser reads (1e9, 0x20) passes for a load.
var loadShape = regexp.MustCompile(`^([0-9]+(\.[0-9]+)?)?$`)

// path is p under the top, or p when it is absolute.
func (b *budgeter) path(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(b.top, p)
}

// hatch runs hatch budget with args at the top, its lines going to the step's.
func (b *budgeter) hatch(ctx context.Context, stdout io.Writer, args ...string) error {
	c := command(ctx, nil, "go", append([]string{"tool", "-modfile=tools/hatch.mod", "hatch", "budget"}, args...)...)
	c.Dir = b.top
	c.Stdout, c.Stderr = stdout, b.stderr
	return exited(c.Run())
}

// exited is errRed for a child that ran and exited other than 0, whose lines have said why, and err
// itself for one that could not start, which the gate prints.
func exited(err error) error {
	if _, ok := errors.AsType[*exec.ExitError](err); ok {
		return errRed
	}
	return err
}

// load is the 1-minute load as hatch reads it, or "" where there is none (Windows) or it fails. Its
// lines of failure are dropped, as the shell's were.
func (b *budgeter) load(ctx context.Context) string {
	var out bytes.Buffer
	c := command(ctx, nil, "go", "tool", "-modfile=tools/hatch.mod", "hatch", "budget", "--load", "--window", "1")
	c.Dir = b.top
	c.Stdout = &out
	if c.Run() != nil {
		return ""
	}
	return strings.TrimSpace(out.String())
}

// start writes the tier's start in seconds and the load at it to f. The load is read before the
// start, so hatch's first build is not timed.
func (b *budgeter) start(ctx context.Context, f string) error {
	load := b.load(ctx)
	if err := os.MkdirAll(b.path(filepath.Dir(f)), 0o750); err != nil {
		return err
	}
	return os.WriteFile(b.path(f), fmt.Appendf(nil, "%d %s\n", b.now().Unix(), load), 0o600)
}

// readStart reads a start file: the start in whole seconds, then the load or nothing, and nothing
// more. Each field is checked before it is used.
func readStart(f string, data []byte) (int64, string, error) {
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, "", fmt.Errorf("%s gives no start, and wants whole seconds", f)
	}
	if !wholeNumber(fields[0]) {
		return 0, "", fmt.Errorf("%s gives the start as '%s', and wants whole seconds", f, fields[0])
	}
	start, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("%s gives the start as '%s', and wants whole seconds", f, fields[0])
	}
	load := ""
	if len(fields) > 1 {
		load = fields[1]
	}
	if !loadShape.MatchString(load) {
		return 0, "", fmt.Errorf("%s gives the load as '%s', and wants a load such as 3.21, or none", f, load)
	}
	if len(fields) > 2 {
		return 0, "", fmt.Errorf("%s has '%s' after the start and the load, and wants nothing more", f, strings.Join(fields[2:], " "))
	}
	return start, load, nil
}

// fileHost is the kind of host budgets.yaml names on its one host line.
func fileHost(budgets string, data []byte) (string, error) {
	var hosts []string
	for line := range strings.Lines(strings.ReplaceAll(string(data), "\r\n", "\n")) {
		if k, v, ok := strings.Cut(strings.TrimSuffix(line, "\n"), ":"); ok && k == "host" {
			hosts = append(hosts, strings.TrimSpace(v))
		}
	}
	if len(hosts) != 1 || hosts[0] == "" {
		return "", fmt.Errorf("%s names the kind of host on %d lines, and wants one host line", budgets, len(hosts))
	}
	return hosts[0], nil
}

// endRow is the row a tier's end is held to: the tier's own on the kind of host budgets.yaml names; a
// release session's on Linux or Windows amd64 off a hosted runner; and none elsewhere, "".
func endRow(tier, host, named string, hosted bool) string {
	switch {
	case host == named:
		return tier + "-budget"
	case tier == "release" && !hosted && slices.Contains(sessionHosts, host):
		return "release-budget-session"
	}
	return ""
}

// endArgs are hatch budget's arguments for a tier's end held to row.
func endArgs(budgets, tier, row string, start int64, load string) []string {
	args := []string{"--budgets", budgets, "--row", wallRow, "--tier", row, "--since", strconv.FormatInt(start, 10), "--window", "1", "--noun", tier}
	if load != "" {
		args = append(args, "--load-before", load)
	}
	if tier == "pre-commit" {
		args = append(args, "--limit", "15", "--loaded-when", "before", "--excused", "passed at the load it started at")
	}
	return append(args, "--advice", advice)
}

// end reads the start file f, removes it, and holds the tier's elapsed seconds to its row.
func (b *budgeter) end(ctx context.Context, tier, f string) error {
	data, err := os.ReadFile(b.path(f))
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(b.stderr, "budget: %s is missing, so %s has no start to time from\n", f, tier)
		return errRed
	}
	if err != nil {
		return err
	}
	if err := os.Remove(b.path(f)); err != nil {
		return err
	}
	start, load, err := readStart(f, data)
	if err != nil {
		fmt.Fprintf(b.stderr, "budget: %v\n", err)
		return errRed
	}
	elapsed := b.now().Unix() - start
	if elapsed < 0 {
		fmt.Fprintf(b.stderr, "budget: %s started at %d, after now, so its time would be %ds\n", tier, start, elapsed)
		return errRed
	}
	named, err := b.named()
	if err != nil {
		return err
	}
	row := endRow(tier, b.host, named, b.hosted)
	if row == "" {
		at := ""
		if load != "" {
			at = " at load " + load
		}
		fmt.Fprintf(b.stdout, "%s took %ds%s; no budget for this kind of host (%s), so none applies\n", tier, elapsed, at, b.host)
		return nil
	}
	return b.hatch(ctx, b.stdout, endArgs(b.budgets, tier, row, start, load)...)
}

// named is the kind of host budgets.yaml names.
func (b *budgeter) named() (string, error) {
	data, err := os.ReadFile(b.path(b.budgets))
	if err != nil {
		return "", err
	}
	return fileHost(b.budgets, data)
}

// cpuArgs are hatch budget's arguments for the CPU step of tier, before the "--" and the members.
func cpuArgs(budgets, tier string, start int64) []string {
	return []string{"--cpu", "--cpu-row", cpuRow, "--budgets", budgets, "--row", cpuWallRow, "--tier", tier, "--since", strconv.FormatInt(start, 10), "--window", "1", "--noun", tier}
}

// cpu runs the tier's members under hatch budget --cpu, and on a red names a cold build cache. The row
// is the tier's own on the kind of host budgets.yaml names; elsewhere there is none, the members run
// alone, and the time is printed. hatch is built before the start, so its first build is not timed. A
// tier with no CPU row is refused by hatch, by name.
func (b *budgeter) cpu(ctx context.Context, tier string) error {
	b.load(ctx)
	start := b.now().Unix()
	named, err := b.named()
	if err != nil {
		return err
	}
	members := b.members(tier)
	if b.host != named {
		c := command(ctx, nil, members[0], members[1:]...)
		c.Dir = b.top
		c.Stdout, c.Stderr = b.stdout, b.stderr
		if err := exited(c.Run()); err != nil {
			return err
		}
		fmt.Fprintf(b.stdout, "%s took %ds; no budget for this kind of host (%s), so none applies\n", tier, b.now().Unix()-start, b.host)
		return nil
	}
	err = b.hatch(ctx, b.stdout, append(append(cpuArgs(b.budgets, tier, start), "--"), members...)...)
	if errors.Is(err, errRed) {
		fmt.Fprintf(b.stderr, coldNote, tier)
	}
	return err
}
