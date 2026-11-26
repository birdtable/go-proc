package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// What it costs when this fails: a budget step given a tier, a step or a run it does not know times
// nothing, or times a run by a start another run wrote.
func TestBudgetRefusesATierStepOrRunItDoesNotKnow(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"budget"}, {"budget", "pre-commit"}, {"budget", "nope", "start", "1"}, {"budget", "", "cpu"},
		{"budget", "pre-commit", "stop", "1"}, {"budget", "pre-commit", "start"}, {"budget", "release", "end"},
		{"budget", "pre-commit", "start", "0"}, {"budget", "pre-commit", "start", "012"}, {"budget", "pre-commit", "start", "x1"},
		{"budget", "pre-commit", "end", "-1"}, {"budget", "pre-commit", "start", "1", "2"}, {"budget", "pre-push", "cpu", "1"},
	} {
		var out, errs bytes.Buffer
		if code := run(context.Background(), args, &out, &errs); code != 2 {
			t.Errorf("run(%q) exited %d, and want 2: %s%s", args, code, &out, &errs)
		}
	}
}

// What it costs when this fails: a tier is held to another kind of host's budget, a release session
// goes unheld, or a hosted runner, whose job's timeout holds it, fails on a session's budget.
func TestEndRowIsTheHostsOwn(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		tier, host string
		hosted     bool
		want       string
	}{
		{"pre-commit", "darwin-arm64", false, "pre-commit-budget"},
		{"release", "darwin-arm64", false, "release-budget"},
		{"release", "darwin-arm64", true, "release-budget"},
		{"release", "linux-amd64", false, "release-budget-session"},
		{"release", "windows-amd64", false, "release-budget-session"},
		{"release", "linux-amd64", true, ""},
		{"release", "windows-amd64", true, ""},
		{"release", "linux-arm64", false, ""},
		{"release", "windows-arm64", false, ""},
		{"release", "darwin-amd64", false, ""},
		{"pre-commit", "linux-amd64", false, ""},
		{"pre-commit", "windows-amd64", false, ""},
	} {
		if got := endRow(c.tier, c.host, "darwin-arm64", c.hosted); got != c.want {
			t.Errorf("endRow(%s, %s, hosted %v) is %q, and want %q", c.tier, c.host, c.hosted, got, c.want)
		}
	}
}

// What it costs when this fails: a start file edited or cut short times the tier from a start it
// never had, or excuses it by a load it never read.
func TestReadStartTakesAStartAndALoadAndNothingMore(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		data, load string
		start      int64
	}{
		{"1764000000 3.21\n", "3.21", 1764000000},
		{"1764000000 16\n", "16", 1764000000},
		{"1764000000 \n", "", 1764000000},
		{"1764000000", "", 1764000000},
	} {
		start, load, err := readStart("f", []byte(c.data))
		if err != nil || start != c.start || load != c.load {
			t.Errorf("readStart(%q) is %d, %q, %v, and want %d, %q", c.data, start, load, err, c.start, c.load)
		}
	}
	for _, data := range []string{"", "\n", "0 1.0", "01764000000 1.0", "x 1.0", "-5 1.0", "1764000000 1e9", "1764000000 0x20",
		"1764000000 .5", "1764000000 5.", "1764000000 1.2.3", "1764000000 1.0 more", "1764000000 1.0\nmore", "99999999999999999999 1.0"} {
		if _, _, err := readStart("f", []byte(data)); err == nil {
			t.Errorf("readStart(%q) took it, and want an error", data)
		}
	}
}

// What it costs when this fails: budgets.yaml with no host line, or two, holds every host to no
// budget, or to the first line's.
func TestFileHostWantsOneHostLine(t *testing.T) {
	t.Parallel()
	for data, want := range map[string]string{
		"# host: no\nhost: darwin-arm64\npre-commit-budget: 2\n": "darwin-arm64",
		"host: darwin-arm64\r\npre-commit-budget: 2\r\n":         "darwin-arm64",
		"host:linux-amd64": "linux-amd64",
	} {
		if got, err := fileHost("b", []byte(data)); err != nil || got != want {
			t.Errorf("fileHost(%q) is %q, %v, and want %q", data, got, err, want)
		}
	}
	for _, data := range []string{"", "pre-commit-budget: 2\n", "host: a\nhost: b\n", "host: \n", " host: a\n"} {
		if got, err := fileHost("b", []byte(data)); err == nil {
			t.Errorf("fileHost(%q) is %q, and want an error", data, got)
		}
	}
}

// budgets writes a budgets.yaml for the darwin-arm64 host with every wall row at wall seconds and the
// push tier's CPU row at cpu seconds, and gives its path.
func budgets(t *testing.T, wall, cpu int) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "budgets.yaml")
	data := fmt.Sprintf("# a test's\nhost: darwin-arm64\npre-commit-budget: %[1]d\npre-push-budget: %[1]d\nrelease-budget: %[1]d\nrelease-budget-session: %[1]d\npre-push-cpu_limit: %[2]d\n", wall, cpu)
	if err := os.WriteFile(f, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// testBudgeter is a budgeter on host at the repository's top, with budgets as its budgets.yaml,
// starts in a folder of its own, and `go env GOOS` as every tier's members.
func testBudgeter(t *testing.T, host string, hosted bool, budgets string) (*budgeter, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var out, errs bytes.Buffer
	return &budgeter{
		top:     filepath.Join("..", ".."),
		budgets: budgets,
		starts:  t.TempDir(),
		host:    host,
		hosted:  hosted,
		members: func(string) []string { return []string{"go", "env", "GOOS"} },
		now:     time.Now,
		stdout:  &out,
		stderr:  &errs,
	}, &out, &errs
}

// What it costs when this fails: a tier past its budget passes, or one inside it fails, on a kind of
// host the budget step holds; or a kind of host with no budget is held to one. Each row is a real
// hatch budget, the elapsed seconds 5 against a budget of 1 and of 100, run for the three systems'
// hosts: the host is the budgeter's, so every row runs on every system the test does.
func TestBudgetEndIsRedOverTheBudgetAndGreenInside(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		tier, host string
		hosted     bool
		row        string // the row hatch names, or "" for a host with none
	}{
		{"pre-commit", "darwin-arm64", false, "pre-commit-budget"},
		{"release", "darwin-arm64", false, "release-budget"},
		{"release", "linux-amd64", false, "release-budget-session"},
		{"release", "windows-amd64", false, "release-budget-session"},
		{"release", "linux-amd64", true, ""},
		{"pre-commit", "windows-amd64", false, ""},
	} {
		for _, limit := range []int{1, 100} {
			t.Run(fmt.Sprintf("%s-%s-hosted-%v-%d", c.tier, c.host, c.hosted, limit), func(t *testing.T) {
				t.Parallel()
				b, out, errs := testBudgeter(t, c.host, c.hosted, budgets(t, limit, 100))
				f := filepath.Join(b.starts, c.tier+".7")
				if err := os.WriteFile(f, fmt.Appendf(nil, "%d\n", time.Now().Unix()-5), 0o600); err != nil {
					t.Fatal(err)
				}
				err := b.run(context.Background(), []string{c.tier, "end", "7"})
				red := c.row != "" && limit == 1
				if red != errors.Is(err, errRed) || (!red && err != nil) {
					t.Errorf("end is %v, and want red %v: %s%s", err, red, out, errs)
				}
				if c.row == "" && !strings.Contains(out.String(), "no budget for this kind of host ("+c.host+")") {
					t.Errorf("end on %s printed %q, and want the time with no budget", c.host, out)
				}
				if c.row != "" && !strings.Contains(out.String()+errs.String(), c.row) {
					t.Errorf("end on %s printed %s%s, and want row %s", c.host, out, errs, c.row)
				}
				if red && !strings.Contains(errs.String(), advice) {
					t.Errorf("a red end printed %q, and want the advice", errs)
				}
				if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("end left its start file: %v", err)
				}
			})
		}
	}
}

// What it costs when this fails: a tier timed from no start, or from a start a second end already
// read, passes on no time at all.
func TestBudgetEndWithNoStartIsRed(t *testing.T) {
	t.Parallel()
	b, out, errs := testBudgeter(t, "darwin-arm64", false, budgets(t, 100, 100))
	if err := b.run(context.Background(), []string{"pre-commit", "end", "7"}); !errors.Is(err, errRed) || !strings.Contains(errs.String(), "has no start to time from") {
		t.Errorf("end with no start is %v, and want red: %s%s", err, out, errs)
	}
	f := filepath.Join(b.starts, "pre-commit.7")
	if err := os.WriteFile(f, fmt.Appendf(nil, "%d\n", time.Now().Unix()+3600), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.run(context.Background(), []string{"pre-commit", "end", "7"}); !errors.Is(err, errRed) || !strings.Contains(errs.String(), "after now") {
		t.Errorf("end from a start after now is %v, and want red: %s%s", err, out, errs)
	}
}

// What it costs when this fails: start writes a start end cannot read, or one in another run's file.
func TestBudgetStartWritesTheRunsOwnStart(t *testing.T) {
	t.Parallel()
	b, out, errs := testBudgeter(t, "darwin-arm64", false, budgets(t, 100, 100))
	b.starts = filepath.Join(b.starts, "budget")
	before := time.Now().Unix()
	if err := b.run(context.Background(), []string{"release", "start", "42"}); err != nil {
		t.Fatalf("start is %v: %s%s", err, out, errs)
	}
	data, err := os.ReadFile(filepath.Join(b.starts, "release.42"))
	if err != nil {
		t.Fatal(err)
	}
	start, _, err := readStart("release.42", data)
	if err != nil || start < before || start > time.Now().Unix() {
		t.Errorf("start wrote %q, and want now and a load: %v", data, err)
	}
}

// What it costs when this fails: a push over its CPU row passes, one under it fails, or a red says
// nothing of a cold build cache. Each row is a real hatch budget --cpu over `go env GOOS`, against a
// CPU row of 0 and of 100; on a kind of host with no row the members run alone.
func TestBudgetCPUIsRedOverItsRowAndNamesAColdCache(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		host  string
		limit int
		red   bool
	}{
		{"darwin-arm64", 0, true},
		{"darwin-arm64", 100, false},
		{"linux-amd64", 0, false},
		{"windows-amd64", 0, false},
	} {
		t.Run(c.host+"-"+strconv.Itoa(c.limit), func(t *testing.T) {
			t.Parallel()
			b, out, errs := testBudgeter(t, c.host, false, budgets(t, 10, c.limit))
			err := b.run(context.Background(), []string{"pre-push", "cpu"})
			if c.red != errors.Is(err, errRed) || (!c.red && err != nil) {
				t.Errorf("cpu is %v, and want red %v: %s%s", err, c.red, out, errs)
			}
			if got := strings.Contains(errs.String(), "a cold build cache can explain it"); got != c.red {
				t.Errorf("cpu named a cold cache %v, and want %v: %s", got, c.red, errs)
			}
			if c.host != "darwin-arm64" && !strings.Contains(out.String(), "no budget for this kind of host ("+c.host+")") {
				t.Errorf("cpu on %s printed %q, and want the time with no budget", c.host, out)
			}
		})
	}
	b, out, errs := testBudgeter(t, "linux-amd64", false, budgets(t, 10, 100))
	b.members = func(string) []string { return []string{"go", "nope"} }
	if err := b.run(context.Background(), []string{"pre-push", "cpu"}); !errors.Is(err, errRed) {
		t.Errorf("cpu with failing members on a host with no budget is %v, and want red: %s%s", err, out, errs)
	}
}
