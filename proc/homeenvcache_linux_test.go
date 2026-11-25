package proc

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// On Linux, a child run with Bare's environment and HomeEnvCache's after it finds its cache folder,
// by os.UserCacheDir, at the kept folder, and its home, configuration and temp folders beneath the
// fresh root.
//
// What it costs when this fails: a Go program given a kept cache by HomeEnvCache still keeps its
// cache in the fresh home, and starts it empty at every run, or keeps more than its cache there.
func TestOnLinuxAChildGivenHomeEnvCacheFindsItsCacheFolderAtTheKeptOne(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	root, cache := filepath.Join(tmp, "home"), filepath.Join(tmp, "cache")
	add, err := HomeEnvCache(root, cache)
	if err != nil {
		t.Fatal(err)
	}
	c := helperCmd(t, "folders")
	c.Env = append(c.Env, add...)
	r, err := Run(context.Background(), c)
	if err != nil || r.Code != 0 {
		t.Fatalf("the child: %v, code %d, stdout %q, stderr %q", err, r.Code, r.Stdout, r.Stderr)
	}
	read := strings.Split(strings.TrimSuffix(string(r.Stdout), "\x00"), "\x00")
	if len(read) != 4 {
		t.Fatalf("the child printed %q, want four folders", r.Stdout)
	}
	if read[2] != cache {
		t.Errorf("the child's cache folder is %q, want the kept folder %q", read[2], cache)
	}
	for _, i := range []int{0, 1, 3} {
		if !beneath(read[i], root) {
			t.Errorf("the child's folder %d (home, configuration, cache, temp) is %q, not beneath %q", i, read[i], root)
		}
	}
}
