package main

import (
	"os"
	"slices"
	"testing"

	"github.com/birdtable/go-testkit"
)

// TestMain re-execs first, since the test binary is also the command and the row child, which exit
// here with the environment childEnv gave them. The tests then run from the environment
// testkit.Main makes: no GIT_ variable, no setting of the person's, and the home, temp and XDG_
// folders beneath one removed after the run.
func TestMain(m *testing.M) {
	if code, ran := reexec(os.Args); ran {
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
	os.Exit(code)
}

// TestTheTestsRunUnderTheIsolateOfTestkitMain: the process's HOME passes testkit.Clean, which keeps
// it only while an Isolate is in force, so TestMain calls testkit.Main after the re-exec; and each
// variable the process has passes it too, so none of the run's own, a planted GIT_ variable or a
// token among them, reached the tests.
//
// What it costs when this fails: TestMain no longer calls testkit.Main, and the tests run with the
// environment of whoever ran go test: its tokens, its GIT_ variables and its home.
func TestTheTestsRunUnderTheIsolateOfTestkitMain(t *testing.T) {
	t.Parallel()
	if home := []string{"HOME=" + os.Getenv("HOME")}; !slices.Equal(testkit.Clean(home), home) {
		t.Fatalf("testkit.Clean drops %q, so no Isolate is in force: TestMain does not call testkit.Main", home)
	}
	env := os.Environ()
	kept := testkit.Clean(env)
	for _, kv := range env {
		if !slices.Contains(kept, kv) {
			t.Errorf("%q reached the tests, and testkit.Clean drops it", kv)
		}
	}
	if len(env) == 0 {
		t.Fatal("the tests' environment is empty, and testkit.Main sets HOME")
	}
}
