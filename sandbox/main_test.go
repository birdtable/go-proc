package sandbox_test

import (
	"os"
	"slices"
	"testing"

	"github.com/birdtable/go-proc/sandbox"
	"github.com/birdtable/go-testkit"
)

// TestMain is first the caller of RunHolding the load rows start, and then the one the live row
// starts, each of which exits here, as the test binary is that process too. The tests then start from the environment testkit.Main makes: no
// GIT_ variable, no setting of the person's, and the home, temp and XDG_ folders beneath one
// removed after the run.
func TestMain(m *testing.M) {
	if code, ran := sandbox.LoadChild(os.Args); ran {
		os.Exit(code)
	}
	if code, ran := sandbox.HoldChild(os.Args); ran {
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
