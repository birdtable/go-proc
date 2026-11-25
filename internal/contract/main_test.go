package contract

import (
	"os"
	"testing"

	"github.com/birdtable/go-testkit"
)

// TestMain starts the tests from the environment testkit.Main makes: no GIT_ variable, no setting of
// the person's, and the home, temp and XDG_ folders beneath one removed after the run. The tests
// start git, to read the last tag.
func TestMain(m *testing.M) {
	code, err := testkit.Main(m)
	if err != nil {
		panic(err)
	}
	os.Exit(code)
}
