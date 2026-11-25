package proc

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// On macOS, a root reached through a link (as the system temp folder is, /var being a link to
// /private/var) and a cache beneath root's resolved form, both resolved the same way first as
// HomeEnvCache's doc says, have the cache refused as beneath root.
//
// What it costs when this fails: a caller that follows the doc's advice still keeps between runs a
// cache that is part of the fresh home, so what one run leaves in its home reaches the next.
func TestOnMacOSACacheBeneathTheResolvedRootIsRefusedOnceBothAreResolved(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	if err := os.Mkdir(filepath.Join(tmp, "real"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(tmp, "link")); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(tmp, "link")
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(resolvedRoot, "cache")
	r, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	c, err := filepath.EvalSymlinks(filepath.Dir(cache))
	if err != nil {
		t.Fatal(err)
	}
	c = filepath.Join(c, filepath.Base(cache))
	if env, err := HomeEnvCache(r, c); !errors.Is(err, errHomeCacheWithin) || env != nil {
		t.Errorf("root %q, resolved %q, with cache %q, resolved %q: HomeEnvCache gives %q, %v; want no list and %v", root, r, cache, c, env, err, errHomeCacheWithin)
	}
}
