package proc

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/birdtable/go-proc/internal/homevars"
)

// HomeEnvCache is HomeEnv's list, entry for entry and in its order, but for XDG_CACHE_HOME, which
// names the cache folder itself, cleaned, and it gives no name twice.
//
// What it costs when this fails: a child given HomeEnvCache reads a folder of its fresh home in
// the kept cache, or the kept cache beneath its fresh home, or holds a name twice, and which value
// it reads then depends on the order its runtime takes.
func TestHomeEnvCacheIsHomeEnvWithTheCacheFolderAlonePointedAtCache(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	root, cache := filepath.Join(tmp, "home"), filepath.Join(tmp, "cache")
	env, err := HomeEnvCache(root, cache+string(filepath.Separator))
	if err != nil {
		t.Fatalf("HomeEnvCache(%q, %q): %v", root, cache, err)
	}
	home := HomeEnv(root)
	if len(env) != len(home) {
		t.Fatalf("HomeEnvCache gives %d entries, HomeEnv %d: %q", len(env), len(home), env)
	}
	seen := map[string]bool{}
	for i, kv := range env {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || seen[strings.ToUpper(k)] {
			t.Fatalf("HomeEnvCache gives %q, not one NAME=value of a name given once", kv)
		}
		seen[strings.ToUpper(k)] = true
		want := home[i]
		if k == homevars.Cache {
			want = k + "=" + cache
		}
		if kv != want {
			t.Errorf("entry %d is %q, want %q", i, kv, want)
		}
	}
	if !seen[homevars.Cache] {
		t.Errorf("HomeEnvCache gives no %s: %q", homevars.Cache, env)
	}
}

// HomeEnvCache refuses an empty or relative root or cache, a cache that is root or beneath it, and
// a cache that holds root, each with its own error, wrapping ErrHomeCache and naming the folder,
// and gives no list; folders that differ only in case are one folder. A cache beside root whose
// name begins with root's passes.
//
// What it costs when this fails: a caller keeps between runs a cache that is part of the fresh
// home, or a fresh home inside the kept cache, so what one run leaves in its home reaches the next;
// or a relative folder is read against the child's own Dir.
func TestHomeEnvCacheRefusesAFolderThatIsNoKeptCacheApartFromRoot(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "home")
	// kroot's k folds with U+212A KELVIN SIGN, three bytes in UTF-8 to k's one.
	kroot := filepath.Join(tmp, "kroot")
	sep := string(filepath.Separator)
	for _, tc := range []struct {
		name, root, cache string
		want              error
	}{
		{"an empty root", "", filepath.Join(tmp, "cache"), errHomeCacheRoot},
		{"a relative root", "home", filepath.Join(tmp, "cache"), errHomeCacheRoot},
		{"an empty cache", root, "", errHomeCacheCache},
		{"a relative cache", root, "cache", errHomeCacheCache},
		{"a cache that is root", root, root, errHomeCacheWithin},
		{"a cache that is root with a separator after it", root, root + sep, errHomeCacheWithin},
		{"a cache beneath root", root, filepath.Join(root, "cache"), errHomeCacheWithin},
		{"a cache beneath root by a .. that stays in it", root, root + sep + "a" + sep + ".." + sep + "cache", errHomeCacheWithin},
		{"a cache beneath root in another case", root, filepath.Join(strings.ToUpper(root), "cache"), errHomeCacheWithin},
		{"a cache beneath root spelled with a case pair of another length", kroot, filepath.Join(tmp, "Kroot", "cache"), errHomeCacheWithin},
		{"a cache that holds root", root, tmp, errHomeCacheHolds},
		{"a cache that is the volume's root", root, filepath.VolumeName(root) + sep, errHomeCacheHolds},
		{"a cache that holds root in another case", root, strings.ToUpper(tmp), errHomeCacheHolds},
	} {
		env, err := HomeEnvCache(tc.root, tc.cache)
		if !errors.Is(err, tc.want) || !errors.Is(err, ErrHomeCache) || env != nil {
			t.Errorf("%s: HomeEnvCache(%q, %q) = %q, %v; want no list and %v", tc.name, tc.root, tc.cache, env, err, tc.want)
			continue
		}
		named := tc.cache
		if errors.Is(tc.want, errHomeCacheRoot) {
			named = tc.root
		}
		if !strings.Contains(err.Error(), strconv.Quote(named)) {
			t.Errorf("%s: the error %q does not name %q", tc.name, err, named)
		}
	}
	beside := root + "-cache"
	if env, err := HomeEnvCache(root, beside); err != nil || !slices.Contains(env, homevars.Cache+"="+beside) {
		t.Errorf("a cache beside root, %q: HomeEnvCache gives %q, %v; want %s at it", beside, env, err, homevars.Cache)
	}
}

// A child run with Bare's environment and HomeEnvCache's after it sees XDG_CACHE_HOME at the kept
// folder, and every other name of homevars' list for this system beneath the fresh root.
//
// What it costs when this fails: a name of Bare's wins over HomeEnvCache's, so the child keeps its
// cache in the person's own, or reads a folder of the person's where a fresh one was meant.
func TestAChildGivenHomeEnvCacheSeesItsCacheAtTheKeptFolderAndTheRestBeneathRoot(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	root, cache := filepath.Join(tmp, "home"), filepath.Join(tmp, "cache")
	add, err := HomeEnvCache(root, cache)
	if err != nil {
		t.Fatal(err)
	}
	c := helperCmd(t, "env")
	c.Env = append(c.Env, add...)
	r, err := Run(context.Background(), c)
	if err != nil || r.Code != 0 {
		t.Fatalf("the child: %v, code %d, stderr %q", err, r.Code, r.Stderr)
	}
	got := map[string]string{}
	for kv := range strings.SplitSeq(strings.TrimSuffix(string(r.Stdout), "\x00"), "\x00") {
		k, v, _ := strings.Cut(kv, "=")
		got[strings.ToUpper(k)] = v
	}
	for _, k := range homevars.Folders(homevars.Here) {
		v, ok := got[strings.ToUpper(k)]
		switch {
		case !ok:
			t.Errorf("the child has no %s", k)
		case k == homevars.Cache && v != cache:
			t.Errorf("the child's %s is %q, want the kept folder %q", k, v, cache)
		case k != homevars.Cache && !beneath(v, root):
			t.Errorf("the child's %s is %q, not beneath %q", k, v, root)
		}
	}
}
