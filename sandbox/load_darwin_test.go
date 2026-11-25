//go:build darwin

package sandbox

import (
	"strings"
	"testing"
)

// TestAProgramLinkedAgainstLibSystemStartsBehindTheBlockWithItsOwnFolderAlone: on macOS
// /usr/bin/true, which is linked against libSystem, runs behind RunHolding with /usr/bin alone and
// exits 0, /usr/bin/false beside it exits 1, so the program itself is what ran, and the load
// folders this system answers are none: the measure RunHolding's doc gives for adding no folder
// here.
//
// What it costs when this fails: macOS has moved a library a program reads to start out of the
// shared cache, and every program a caller holds to its folders no longer starts.
func TestAProgramLinkedAgainstLibSystemStartsBehindTheBlockWithItsOwnFolderAlone(t *testing.T) {
	t.Parallel()
	if dirs, err := loadHere("/usr/bin/true"); err != nil || len(dirs) != 0 {
		t.Errorf("the load folders of /usr/bin/true on macOS: %q, %v, want none", dirs, err)
	}
	for prog, want := range map[string]int{"/usr/bin/true": 0, "/usr/bin/false": 1} {
		if code, said := behindTheBlock(t, prog); code != want {
			t.Errorf("%s behind RunHolding with /usr/bin alone: exit %d, want %d: %s", prog, code, want, strings.TrimSpace(said))
		}
	}
}
