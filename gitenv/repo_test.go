package gitenv_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/birdtable/go-proc/gitenv"
)

// What it costs when this fails: a variable git adds to the ones it reads to find a repository is
// left in the child's environment by Repo and Env, and a hook's GIT_DIR sends the child's commits
// to the hook's repository.
//
// The oracle: git itself, run as rev-parse --local-env-vars, names the variables local to a
// repository, and the package's list holds every one. It fails if git names a variable the list
// lacks, and runs on every system the package builds on.
func TestEveryVariableGitNamesAsLocalToARepositoryIsInTheList(t *testing.T) {
	t.Parallel()
	out, err := gitIn(t, gitenv.Env(os.Environ(), who()...), "rev-parse", "--local-env-vars")
	if err != nil {
		t.Fatalf("git rev-parse --local-env-vars: %v\n%s", err, out)
	}
	listed := 0
	for name := range strings.FieldsSeq(out) {
		listed++
		if !slices.Contains(gitenv.Names(), name) {
			t.Errorf("git names %s as a variable local to a repository, and the package's list lacks it", name)
		}
	}
	if listed < 10 {
		t.Fatalf("git named %d variables, so the oracle read nothing: %q", listed, out)
	}
}

// Repo drops the names of the list in any case and keeps the author, the committer and the rest; it
// reads only what it is given; add comes last.
//
// What it costs when this fails: a child that writes takes a repository the caller never named, or
// loses the author a release is made as.
func TestRepoDropsTheRepositoryNamesAndKeepsTheAuthorAndTheRest(t *testing.T) {
	t.Setenv("GIT_DIR", "/from/the/process")
	in := []string{"GIT_DIR=/x", "git_work_tree=/y", "GIT_AUTHOR_NAME=a", "MY_GIT_DIR=/kept", "PATH=/bin", "GIT_CONFIG_COUNT=1"}
	got := gitenv.Repo(in, "GIT_DIR=/mine")
	for _, want := range []string{"GIT_AUTHOR_NAME=a", "MY_GIT_DIR=/kept", "PATH=/bin", "GIT_DIR=/mine"} {
		if !slices.Contains(got, want) {
			t.Errorf("Repo lost %s: %q", want, got)
		}
	}
	for _, gone := range []string{"GIT_DIR=/x", "git_work_tree=/y", "GIT_CONFIG_COUNT=1", "GIT_DIR=/from/the/process"} {
		if slices.Contains(got, gone) {
			t.Errorf("Repo kept %s: %q", gone, got)
		}
	}
	if len(got) != 4 || got[len(got)-1] != "GIT_DIR=/mine" {
		t.Errorf("Repo gave %q: want the four kept, the caller's add last", got)
	}
}

// Env and Repo leave the slice they are given as it was, the room past its length too: each builds
// a slice of its own, and a write to what it gave reaches nothing of the caller's. The slice here
// holds names each of them drops, in both cases, and two entries past its length that an append in
// place would write over.
//
// What it costs when this fails: a caller's own environment loses its entries, or gains the add of
// one child, by a call that reads as a copy, and the next child it starts runs with what is left.
func TestEnvAndRepoLeaveTheSliceTheyAreGivenAsItWas(t *testing.T) {
	t.Parallel()
	ran := 0
	for name, f := range map[string]func([]string, ...string) []string{"Env": gitenv.Env, "Repo": gitenv.Repo} {
		ran++
		whole := []string{"GIT_DIR=/x", "PATH=/bin", "git_work_tree=/y", "HOME=/h", "GIT_AUTHOR_NAME=a", "past=1", "past=2"}
		before := slices.Clone(whole)
		got := f(whole[:5], "ADDED=1")
		if len(got) < 2 || got[len(got)-1] != "ADDED=1" || slices.Contains(got, "GIT_DIR=/x") || !slices.Contains(got, "PATH=/bin") {
			t.Fatalf("%s gave %q: want GIT_DIR dropped, PATH kept and the add last, so the call did its work", name, got)
		}
		for i := range got {
			got[i] = "written by the caller"
		}
		if !slices.Equal(whole, before) {
			t.Errorf("%s, or a write to what it gave, changed the slice it was given: %q, want %q", name, whole, before)
		}
	}
	if ran != 2 {
		t.Fatalf("ran %d of 2 functions", ran)
	}
}

// With GIT_DIR and GIT_WORK_TREE set to another repository, a git run built from Repo and ReadArgs
// answers for the folder it was given; the control is the same git with the variables kept and no
// ReadArgs folder, which answers for the other repository. On every system.
//
// What it costs when this fails: a hook's GIT_DIR makes every folder a program reads answer as the
// hook's repository, and a build or a release is read from the wrong tree.
func TestARunBuiltFromRepoAndReadArgsAnswersForTheFolderItWasGiven(t *testing.T) {
	t.Parallel()
	a, b := repo(t, "a.txt"), repo(t, "b.txt")
	moved := []string{"GIT_DIR=" + filepath.Join(b, ".git"), "GIT_WORK_TREE=" + b}
	probe := []string{"log", "-1", "--format=%s"}
	got, err := gitIn(t, gitenv.Repo(slices.Concat(os.Environ(), moved), who()...), gitenv.ReadArgs(a, probe...)...)
	if err != nil || got != "one a.txt" {
		t.Errorf("under Repo and ReadArgs git answered %q (%v), want the subject of the given folder's commit", got, err)
	}
	control, _ := gitIn(t, slices.Concat(os.Environ(), moved, who()), probe...)
	if control != "one b.txt" {
		t.Errorf("the control answered %q, so the case proves nothing: want the other repository's commit", control)
	}
}

// A hook the folder's own configuration names does not run under ReadArgs, on every system; the
// control is the same commit without ReadArgs, where it runs and leaves its marker.
//
// What it costs when this fails: a read of a folder that is not the caller's runs a program that
// folder's configuration names, with the caller's own rights.
func TestAPlantedHookDoesNotRunUnderReadArgs(t *testing.T) {
	t.Parallel()
	prog, marker := program(t)
	hooks := filepath.Dir(prog)
	if err := os.Rename(prog, filepath.Join(hooks, "pre-commit")); err != nil {
		t.Fatal(err)
	}
	dir := repo(t, "a.txt")
	setup(t, "-C", dir, "config", "core.hooksPath", hooks)
	ran := 0
	for _, tc := range []struct {
		name string
		args []string
		runs bool
	}{
		{"without ReadArgs", []string{"-C", dir, "commit", "-q", "--allow-empty", "-m", "x"}, true},
		{"under ReadArgs", gitenv.ReadArgs(dir, "commit", "-q", "--allow-empty", "-m", "y"), false},
	} {
		ran++
		if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		out, err := gitIn(t, gitenv.Env(os.Environ(), who()...), tc.args...)
		if err != nil {
			t.Errorf("%s: git failed: %v\n%s", tc.name, err, out)
		}
		if _, statErr := os.Stat(marker); (statErr == nil) != tc.runs {
			t.Errorf("%s: the hook ran %v, want %v\n%s", tc.name, statErr == nil, tc.runs, out)
		}
	}
	if ran != 2 {
		t.Fatalf("ran %d of 2 cases", ran)
	}
}

// What it costs when this fails: a type or a second spelling of the entry points grows beside the
// seven functions, and each caller has to learn which one to take.
//
// The package exports Env, Repo, ReadArgs, Home, NoHome, NoGlobalConfig and CommitMode and nothing
// else: no Names, no type, and no Without, which slices.DeleteFunc over
// slices.Clone replaces.
func TestThePackageExportsTheSevenFunctionsAndNothingElse(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("gitenv.go")
	if err != nil {
		t.Fatal(err)
	}
	if got := exported(t, string(src)); !slices.Equal(got, []string{"CommitMode", "Env", "Home", "NoGlobalConfig", "NoHome", "ReadArgs", "Repo"}) {
		t.Errorf("gitenv.go exports %q, want CommitMode, Env, Home, NoGlobalConfig, NoHome, ReadArgs and Repo", got)
	}
	// The plant: a second function and a type are found by the same reader.
	plant := "package p\n\nfunc Env() {}\nfunc Names() {}\ntype Policy struct{}\nvar Default = 1\n"
	if got := exported(t, plant); !slices.Equal(got, []string{"Default", "Env", "Names", "Policy"}) {
		t.Errorf("the reader found %q in the planted file, want Default, Env, Names and Policy", got)
	}
}

// exported is the sorted exported top-level names of the Go source src: functions, types, variables
// and constants.
func exported(t *testing.T, src string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil && d.Name.IsExported() {
				out = append(out, d.Name.Name)
			}
		case *ast.GenDecl:
			for _, s := range d.Specs {
				switch s := s.(type) {
				case *ast.TypeSpec:
					if s.Name.IsExported() {
						out = append(out, s.Name.Name)
					}
				case *ast.ValueSpec:
					for _, n := range s.Names {
						if n.IsExported() {
							out = append(out, n.Name)
						}
					}
				}
			}
		}
	}
	slices.Sort(out)
	return out
}
