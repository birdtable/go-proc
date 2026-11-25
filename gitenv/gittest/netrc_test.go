package gittest

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/birdtable/go-testkit"
)

// TestAGitChildReadsTheNetrcOfItsOwnHomeNotThePersons: a git child of Git(dir) that a host answers
// 401 sends the password the .netrc in dir holds for that host, so libcurl reads the netrc of the
// test's own HOME. With no HOME, as in older versions, libcurl takes the home from the account's
// passwd entry, never reads dir's, and sends the password the person's .netrc holds. The second case
// gives gitenv.Env, under Git, a NETRC that names another .netrc with another password for the same
// host: libcurl 8.16 and later reads that file ahead of HOME's, so the child sends its password unless
// Env drops NETRC. Both planted files are the test's own and name only the listener's host, 127.0.0.1.
//
// What it costs when this fails: a test's git that fetches or pushes over http or https reads the
// .netrc of another home, or the one a NETRC names, the person's among them, and sends their password
// to a host it names.
func TestAGitChildReadsTheNetrcOfItsOwnHomeNotThePersons(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		netrc bool
	}{{"Git", false}, {"Git with a NETRC planted", true}} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir, other := t.TempDir(), filepath.Join(t.TempDir(), "netrc")
			own := netrcEntry(t, filepath.Join(dir, ".netrc"), "row194", "planted-in-the-tests-own-folder")
			elsewhere := netrcEntry(t, other, "row194-netrc", "planted-where-netrc-names")
			env := Git(dir)
			if c.netrc {
				env = gitFrom(append(testkit.Clean(os.Environ()), "NETRC="+other), dir)
			}
			sent, out, err := lsRemote(t, env, dir, `Basic realm="row"`)
			if !slices.Contains(sent, own) || slices.Contains(sent, elsewhere) {
				t.Errorf("git ls-remote in Git(%s) sent %s, want the planted one among them and never NETRC's: the child reads the .netrc of another home, or the file NETRC names (git: %v: %s)", dir, described(sent, own, elsewhere), err, out)
			}
		})
	}
}

// netrcEntry writes a .netrc at path that names the listener's host with login and password, and
// is the Authorization header a child that reads it sends.
func netrcEntry(t *testing.T, path, login, password string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte("machine 127.0.0.1 login "+login+" password "+password+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(login+":"+password))
}

// lsRemote runs the host's git ls-remote in dir with env against a listener that answers every
// request 401, offering each of offers as a WWW-Authenticate challenge, and is each Authorization
// header the listener was sent, with git's output.
func lsRemote(t *testing.T, env []string, dir string, offers ...string) ([]string, []byte, error) {
	t.Helper()
	var mu sync.Mutex
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sent = append(sent, r.Header.Get("Authorization"))
		mu.Unlock()
		for _, o := range offers {
			w.Header().Add("WWW-Authenticate", o)
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	cmd := exec.CommandContext(t.Context(), GitPath(t), "ls-remote", srv.URL+"/r.git") //nolint:gosec // the host's git, against the test's own listener
	cmd.Env, cmd.Dir = env, dir
	out, err := cmd.CombinedOutput() // the listener refuses every request, so git fails
	srv.Close()
	mu.Lock()
	defer mu.Unlock()
	return slices.Clone(sent), out, err
}

// described says how many headers came and what each is, never the header: empty, the planted one,
// NETRC's, or another. A red line that printed them would print the password of whatever .netrc the
// child read, the person's among them.
func described(sent []string, own, elsewhere string) string {
	what := make([]string, 0, len(sent))
	for _, h := range sent {
		switch h {
		case "":
			what = append(what, "empty")
		case own:
			what = append(what, "the planted one")
		case elsewhere:
			what = append(what, "NETRC's")
		default:
			what = append(what, "another")
		}
	}
	return fmt.Sprintf("%d headers [%s]", len(sent), strings.Join(what, ", "))
}
