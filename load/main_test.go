package load_test

import (
	"os"
	"testing"

	"github.com/birdtable/go-testkit"
)

// TestMain starts the tests from the environment testkit.Main makes, as every package's here does:
// the go and sysctl children the tests start get no setting or token of the person's.
func TestMain(m *testing.M) {
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
