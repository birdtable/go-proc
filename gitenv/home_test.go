package gitenv_test

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/birdtable/go-proc/gitenv"
)

// Home and NoHome give their add for an absolute, clean folder, and refuse an empty, a relative and
// an unclean one, each with its own error, which names the folder; a refusal gives no add.
//
// What it costs when this fails: a caller's empty or relative folder names its child's working
// directory as the home, a folder the child's own repository may hold, and git reads the global
// configuration and libcurl the .netrc a folder's author put there.
func TestHomeAndNoHomeRefuseAnEmptyARelativeAndAnUncleanFolder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sep := string(filepath.Separator)
	if got, err := gitenv.Home(dir); err != nil || !slices.Equal(got, []string{"HOME=" + dir}) {
		t.Errorf("Home(%q) = %q, %v", dir, got, err)
	}
	if got, err := gitenv.NoHome(dir); err != nil || !slices.Equal(got, []string{"HOME=" + dir, "GIT_CONFIG_GLOBAL=/dev/null"}) {
		t.Errorf("NoHome(%q) = %q, %v", dir, got, err)
	}
	refused := 0
	for name, tc := range map[string]struct {
		dir  string
		want error
	}{
		"empty":                        {"", gitenv.ErrNoFolder},
		"relative":                     {"home", gitenv.ErrRelative},
		"relative by dot":              {"." + sep + "home", gitenv.ErrRelative},
		"unclean by a trailing slash":  {dir + sep, gitenv.ErrUnclean},
		"unclean by a parent":          {dir + sep + ".." + sep + filepath.Base(dir), gitenv.ErrUnclean},
		"unclean by a doubled slash":   {dir + sep + sep + "home", gitenv.ErrUnclean},
		"unclean by a dot in the path": {dir + sep + "." + sep + "home", gitenv.ErrUnclean},
	} {
		for fn, call := range map[string]func(string) ([]string, error){"Home": gitenv.Home, "NoHome": gitenv.NoHome} {
			refused++
			got, err := call(tc.dir)
			if !errors.Is(err, tc.want) || got != nil {
				t.Errorf("%s, %s: %s(%q) = %q, %v, want the refusal %v", name, fn, fn, tc.dir, got, err, tc.want)
			}
			// The refusal names the folder quoted, as %q writes it: on Windows that doubles each backslash.
			if err != nil && tc.dir != "" && !strings.Contains(err.Error(), strconv.Quote(tc.dir)) {
				t.Errorf("%s, %s: the refusal %q does not name %s", name, fn, err, strconv.Quote(tc.dir))
			}
		}
	}
	if refused != 14 {
		t.Fatalf("checked %d of 14 refusals", refused)
	}
}

// plant writes body to the file name under dir, making the folders it needs.
func plant(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// noHome is NoHome's add for a fresh empty folder of the test's own.
func noHome(t *testing.T) []string {
	t.Helper()
	add, err := gitenv.NoHome(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return add
}

// Under NoHome, `git config --global --list` reads nothing, with a .gitconfig planted in HOME, a
// git/config in XDG_CONFIG_HOME and, on Windows, a .gitconfig in USERPROFILE, each holding a key of
// its own. Each red is Env with one of those names handed on, as a caller that sets its child's home
// without NoHome does, and git prints that plant's key; git reads XDG_CONFIG_HOME's file only where
// HOME's folder holds no .gitconfig, so that red's HOME is an empty folder. Each green is the same
// with NoHome's add last, and git prints no key.
//
// What it costs when this fails: a child the caller meant to have no home reads the person's global
// configuration, and with it an alias, a credential helper or a core.sshCommand that runs a program.
func TestUnderNoHomeGitReadsNoGlobalConfigurationPlantedInTheHomes(t *testing.T) {
	t.Parallel()
	cases := plantedConfigs(t)
	checked := 0
	for _, tc := range cases {
		checked++
		red, err := gitIn(t, gitenv.Env(os.Environ(), tc.add...), "config", "--global", "--list")
		if !strings.Contains(red, tc.key) {
			t.Errorf("%s without NoHome: git does not print %s, so the plant is not where git looks (%v)\n%s", tc.name, tc.key, err, red)
		}
		green, err := gitIn(t, gitenv.Env(os.Environ(), slices.Concat(tc.add, noHome(t))...), "config", "--global", "--list")
		if err != nil || strings.Contains(green, "planted") {
			t.Errorf("%s with NoHome: git read a planted configuration (%v)\n%s", tc.name, err, green)
		}
	}
	if checked != len(cases) || checked < 2 {
		t.Fatalf("checked %d of %d home names", checked, len(cases))
	}
}

// plantedConfigs plants a .gitconfig in a HOME, a git/config in an XDG_CONFIG_HOME and, on Windows,
// a .gitconfig in a USERPROFILE, each folder the test's own and each file holding a key of its own,
// and gives the add that names each home, with the key git prints when it reads that plant. git reads
// XDG_CONFIG_HOME's file only where HOME's folder holds no .gitconfig, so that add's HOME is empty.
func plantedConfigs(t *testing.T) []configCase {
	t.Helper()
	home, xdg, profile := t.TempDir(), t.TempDir(), t.TempDir()
	plant(t, home, ".gitconfig", "[probe]\n\thome = planted\n")
	plant(t, xdg, filepath.Join("git", "config"), "[probe]\n\txdg = planted\n")
	plant(t, profile, ".gitconfig", "[probe]\n\tprofile = planted\n")
	return slices.Concat([]configCase{
		{"HOME", []string{"HOME=" + home}, "probe.home=planted"},
		{"XDG_CONFIG_HOME", []string{"HOME=" + t.TempDir(), "XDG_CONFIG_HOME=" + xdg}, "probe.xdg=planted"},
	}, systemConfigCases(profile))
}

// NoGlobalConfig gives GIT_CONFIG_GLOBAL=/dev/null alone, NoHome's GIT_CONFIG_GLOBAL element byte for
// byte, and a new slice each call, so a caller that writes into one changes neither the next nor
// NoHome's. Under Env with NoGlobalConfig's add last, `git config --global --list` reads nothing, the
// child's home left as the caller's add names it: HOME holding a .gitconfig, an XDG_CONFIG_HOME
// holding a git/config and, on Windows, a USERPROFILE holding a .gitconfig, each with a key of its
// own. Each red is Env with that home's add alone, and git prints the plant's key; each green is the
// same add with NoGlobalConfig's after it, and git prints no key.
//
// What it costs when this fails: a child that keeps a home of the caller's reads the global
// configuration there, an alias, a credential helper or a core.sshCommand among it; or a caller that
// takes go-proc's spelling of the value compares against a string NoHome does not set.
func TestNoGlobalConfigIsNoHomesValueAndGitReadsNoGlobalConfigurationBesideAHome(t *testing.T) {
	t.Parallel()
	want := []string{"GIT_CONFIG_GLOBAL=/dev/null"}
	add := gitenv.NoGlobalConfig()
	if !slices.Equal(add, want) {
		t.Fatalf("NoGlobalConfig() = %q, want %q", add, want)
	}
	nohome := noHome(t)
	i := slices.IndexFunc(nohome, func(kv string) bool { return strings.HasPrefix(kv, "GIT_CONFIG_GLOBAL=") })
	if i < 0 || nohome[i] != add[0] {
		t.Errorf("NoHome's add %q holds no element %q", nohome, add[0])
	}
	add[0] = "GIT_CONFIG_GLOBAL=/written"
	if again := gitenv.NoGlobalConfig(); !slices.Equal(again, want) {
		t.Errorf("after a caller wrote into its slice, NoGlobalConfig() = %q, want %q", again, want)
	}
	if again := noHome(t); !slices.Contains(again, want[0]) {
		t.Errorf("after a caller wrote into NoGlobalConfig's slice, NoHome's add is %q", again)
	}
	cases := plantedConfigs(t)
	checked := 0
	for _, tc := range cases {
		checked++
		red, err := gitIn(t, gitenv.Env(os.Environ(), tc.add...), "config", "--global", "--list")
		if !strings.Contains(red, tc.key) {
			t.Errorf("%s without NoGlobalConfig: git does not print %s, so the plant is not where git looks (%v)\n%s", tc.name, tc.key, err, red)
		}
		green, err := gitIn(t, gitenv.Env(os.Environ(), slices.Concat(tc.add, gitenv.NoGlobalConfig())...), "config", "--global", "--list")
		if err != nil || strings.Contains(green, "planted") {
			t.Errorf("%s with NoGlobalConfig: git read a planted configuration (%v)\n%s", tc.name, err, green)
		}
	}
	if checked != len(cases) || checked < 2 {
		t.Fatalf("checked %d of %d home names", checked, len(cases))
	}
}

// basicAuth is a server that asks every request for basic authentication, and the Authorization
// headers it was sent.
type basicAuth struct {
	mu   sync.Mutex
	sent []string
}

// ServeHTTP records the request's Authorization header, if it has one, and answers 401.
func (b *basicAuth) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if got := r.Header.Get("Authorization"); got != "" {
		b.mu.Lock()
		b.sent = append(b.sent, got)
		b.mu.Unlock()
	}
	w.Header().Set("WWW-Authenticate", `Basic realm="probe"`)
	w.WriteHeader(http.StatusUnauthorized)
}

// take is the headers sent since the last take.
func (b *basicAuth) take() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.sent
	b.sent = nil
	return out
}

// Under NoHome, a fetch over http from a local server that asks for basic authentication sends no
// Authorization header, with a .netrc and a _netrc planted in HOME and, on Windows, in USERPROFILE,
// and on Windows a _netrc alone in a second HOME, each with a password of its own for the server.
// Each red is Env with one of those names handed on, and the server logs the password of the file
// libcurl reads first there; each green is the same with NoHome's add last, and it logs none.
//
// What it costs when this fails: a child the caller meant to have no home sends the password the
// person's .netrc holds for a host to whatever answers there with a 401.
func TestUnderNoHomeAFetchOverHTTPSendsNoPasswordPlantedForCurl(t *testing.T) {
	t.Parallel()
	server := &basicAuth{}
	srv := httptest.NewServer(server)
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	host, _, _ = strings.Cut(host, ":")
	places := slices.Concat([]netrcPlace{{"HOME", t.TempDir(), []string{".netrc", "_netrc"}}}, systemNetrcPlaces(t))
	proxyless := []string{"NO_PROXY=" + host, "no_proxy=" + host}
	checked := 0
	for _, p := range places {
		for _, file := range p.files {
			plant(t, p.dir, file, "machine "+host+" login "+p.name+" password "+p.name+file+"\n")
		}
		checked++
		want := p.name + ":" + p.name + p.files[0]
		add := []string{p.name + "=" + p.dir}
		server.take()
		out, _ := gitIn(t, gitenv.Env(os.Environ(), slices.Concat(proxyless, add)...), "ls-remote", srv.URL+"/probe.git")
		if red := server.take(); !slices.ContainsFunc(red, func(header string) bool { return decoded(header) == want }) {
			t.Errorf("%s holding %q, without NoHome: the server logged %q, not %s, so the plant is not where libcurl looks\n%s", p.name, p.files, red, want, out)
		}
		out, _ = gitIn(t, gitenv.Env(os.Environ(), slices.Concat(proxyless, add, noHome(t))...), "ls-remote", srv.URL+"/probe.git")
		if green := server.take(); len(green) != 0 {
			t.Errorf("%s holding %q, with NoHome: the server logged %q\n%s", p.name, p.files, green, out)
		}
	}
	if checked != len(places) || checked < 1 {
		t.Fatalf("checked %d of %d planted homes", checked, len(places))
	}
}

// decoded is the user:password of a Basic Authorization header, or the header as it is.
func decoded(header string) string {
	b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(header, "Basic "))
	if err != nil {
		return header
	}
	return string(b)
}

// configCase is one red of the configuration row: a home name's add, with the key git prints when it
// reads the configuration planted there.
type configCase struct {
	name string
	add  []string
	key  string
}

// netrcPlace is one red of the .netrc row: the home name, a folder of the test's own, and the files
// planted there, each with the password <name>:<name><file>. libcurl reads the first file it finds.
type netrcPlace struct {
	name, dir string
	files     []string
}
