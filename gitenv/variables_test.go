package gitenv_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/birdtable/go-proc/gitenv"
)

// nineNames is a list of nine names a project drops that leaves GIT_CONFIG_COUNT, GIT_CONFIG and
// GIT_CONFIG_PARAMETERS in, the shape a study found in two projects: the planted
// list the tests below are read against.
func nineNames() []string {
	return []string{
		"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
		"GIT_COMMON_DIR", "GIT_NAMESPACE", "GIT_PREFIX", "GIT_IMPLICIT_WORK_TREE",
	}
}

// without is environ less the variables named in names.
func without(environ, names []string) []string {
	var out []string
	for _, kv := range environ {
		if k, _, _ := strings.Cut(kv, "="); !slices.Contains(names, k) {
			out = append(out, kv)
		}
	}
	return out
}

// A program that the environment alone names, as core.hooksPath or core.fsmonitor through
// GIT_CONFIG_COUNT, runs under the environment as it is and under a list that leaves the config
// names in, and does not run under Env, with or without ReadArgs: the first two are the control and
// the planted list, red against the test, and the others are the block. The hook is a commit's
// pre-commit, the monitor a status's fsmonitor. Every row starts from the test process's own
// environment, so PATH, and on Windows SystemRoot, are there: a program that does not run because
// git found no shell to run it with is no block. On Windows the program is a #! script that Git for
// Windows runs with its own sh, and the value is written with forward slashes, as git may hand
// core.fsmonitor's value to that shell, which reads a backslash as an escape.
//
// What it costs when this fails: a git child of a caller that took the environment it was started in
// runs a program a hook, a harness or a person named in a variable, with the caller's own rights.
func TestAProgramTheEnvironmentNamesRunsUnlessEnvDropsItsVariables(t *testing.T) {
	t.Parallel()
	ran := 0
	for _, kind := range []struct {
		name, key string
		args      []string
		folder    bool
	}{
		{"hook", "core.hooksPath", []string{"commit", "-q", "--allow-empty", "-m", "x"}, true},
		{"monitor", "core.fsmonitor", []string{"status", "--porcelain"}, false},
	} {
		prog, marker := program(t)
		value := prog
		if kind.folder {
			value = filepath.Dir(prog)
			if err := os.Rename(prog, filepath.Join(value, "pre-commit")); err != nil {
				t.Fatal(err)
			}
		}
		dir := repo(t, "a.txt")
		environ := slices.Concat(os.Environ(), []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=" + kind.key, "GIT_CONFIG_VALUE_0=" + filepath.ToSlash(value)})
		naive := slices.Concat(environ, who())
		for name, tc := range map[string]struct {
			env  []string
			args []string
			runs bool
		}{
			"the environment as it is":               {naive, slices.Concat([]string{"-C", dir}, kind.args), true},
			"the nine names":                         {without(naive, nineNames()), slices.Concat([]string{"-C", dir}, kind.args), true},
			"the nine names and ReadArgs":            {without(naive, nineNames()), gitenv.ReadArgs(dir, kind.args...), false},
			"Env alone":                              {gitenv.Env(environ, who()...), slices.Concat([]string{"-C", dir}, kind.args), false},
			"Env and ReadArgs":                       {gitenv.Env(environ, who()...), gitenv.ReadArgs(dir, kind.args...), false},
			"ReadArgs with the environment as it is": {naive, gitenv.ReadArgs(dir, kind.args...), false},
		} {
			ran++
			if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			out, err := gitIn(t, tc.env, tc.args...)
			if err != nil && kind.name == "hook" {
				t.Errorf("%s, %s: git failed: %v\n%s", kind.name, name, err, out)
			}
			_, statErr := os.Stat(marker)
			if got := statErr == nil; got != tc.runs {
				t.Errorf("%s, %s: the program ran %v, want %v\n%s", kind.name, name, got, tc.runs, out)
			}
		}
	}
	if ran != 12 {
		t.Fatalf("ran %d of 12 cases", ran)
	}
}

// Each variable that points git at another repository, work tree, index or configuration, set,
// changes what git reads from a folder, and under Env it changes nothing: git answers as it does with
// no variable set. The control is git given the variable as it is, which answers differently. Each
// run starts from the test process's own environment, and a git under Env with no variable that fails
// and prints stops the test, as two failures alike would prove nothing.
//
// What it costs when this fails: a hook's GIT_DIR makes every folder a child reads answer as the
// hook's repository, and a variable a harness left sets the configuration a child reads.
func TestEachVariableThatMovesGitChangesNothingUnderEnv(t *testing.T) {
	t.Parallel()
	a, b := repo(t, "a.txt"), repo(t, "b.txt")
	config := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(config, []byte("[probe]\n\tkey = leaked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	checked := 0
	for name, tc := range map[string]struct {
		variable string
		probe    []string
	}{
		"GIT_DIR":               {"GIT_DIR=" + filepath.Join(b, ".git"), []string{"rev-parse", "HEAD"}},
		"GIT_WORK_TREE":         {"GIT_WORK_TREE=" + b, []string{"rev-parse", "--show-toplevel"}},
		"GIT_INDEX_FILE":        {"GIT_INDEX_FILE=" + filepath.Join(b, ".git", "index"), []string{"ls-files"}},
		"GIT_CONFIG":            {"GIT_CONFIG=" + config, []string{"config", "--get", "probe.key"}},
		"GIT_CONFIG_PARAMETERS": {"GIT_CONFIG_PARAMETERS='probe.key'='leaked'", []string{"config", "--get", "probe.key"}},
		"GIT_CONFIG_COUNT":      {"GIT_CONFIG_COUNT=1", []string{"config", "--get", "probe.key"}},
	} {
		checked++
		args := gitenv.ReadArgs(a, tc.probe...)
		clean, err := gitIn(t, gitenv.Env(os.Environ(), who()...), args...)
		if err != nil && clean != "" {
			t.Fatalf("%s: git failed under Env with no variable, so the case proves nothing: %v\n%s", name, err, clean)
		}
		held, _ := gitIn(t, gitenv.Env(slices.Concat(os.Environ(), []string{tc.variable, "GIT_CONFIG_KEY_0=probe.key", "GIT_CONFIG_VALUE_0=leaked"}), who()...), args...)
		if held != clean {
			t.Errorf("%s: under Env git answers %q, and %q with no variable", name, held, clean)
		}
		if name == "GIT_CONFIG_COUNT" {
			continue
		}
		as, _ := gitIn(t, slices.Concat(os.Environ(), who(), []string{tc.variable}), append([]string{"-C", a}, tc.probe...)...)
		if as == clean {
			t.Errorf("%s: git answers %q as it is too, so the case proves nothing", name, as)
		}
	}
	if checked != 6 {
		t.Fatalf("checked %d of 6 variables", checked)
	}
}
