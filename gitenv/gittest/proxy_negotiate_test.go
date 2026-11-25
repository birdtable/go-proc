package gittest

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/birdtable/go-testkit"
)

// TestAGitChildSendsAProxyNoTicketOfThePersons: a git child of Git(dir) that a proxy the run names
// answers 407, offering Negotiate, NTLM and Basic, sends the proxy no Kerberos ticket of the
// person's. libcurl answers a proxy that offers Negotiate with the GSS-API's default ticket cache
// whenever git has any proxy login at all, an empty one included, and with no login when
// http.proxyAuthMethod is negotiate. git's emptyAuth is the host's retry, never the proxy's, so here Git's KRB5CCNAME
// alone is the fix, and a count of requests is the same with it or without it.
//
// So the row plants a ticket cache of its own and names it in KRB5CCNAME where the person's
// environment would, and hands that environment to gitFrom, as Git hands it the process's. The
// cache is synthetic: a made-up realm and principals, a random session key, and a service ticket
// for HTTP/127.0.0.1 whose cipher is random bytes. The GSS-API builds an AP-REQ from a cached service
// ticket with no KDC, so on Linux and macOS a git that reads this cache sends the proxy a Kerberos
// AP-REQ, and the row is red without Git's KRB5CCNAME on any host. The control runs the same git in
// the environment without Git's settings and must see that AP-REQ, so a git that never reads the
// cache cannot pass the row unseen. On Windows the logon session through SSPI is what libcurl
// sends, never a file cache, so there the control is not held, and a Negotiate or NTLM header with
// no login of the test's fails the row as the person's session. The row prints the scheme of each
// header and never the token.
//
// What it costs when this fails: a test's git that fetches through a proxy the run names
// authenticates to that proxy as the person, with their Kerberos ticket.
func TestAGitChildSendsAProxyNoTicketOfThePersons(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, login, method, target string
		byVariable, theTests        bool
	}{
		{name: "http.proxy, an empty login", login: ":@"},
		{name: "http.proxy, the test's login", login: "row226:planted-in-the-tests-own-folder@", theTests: true},
		{name: "http.proxy, no login, proxyAuthMethod negotiate", method: "negotiate"},
		{name: "HTTPS_PROXY, an empty login, CONNECT", login: ":@", byVariable: true, target: "https"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			persons := append(testkit.Clean(os.Environ()), plantTicket(t, t.TempDir())...)
			p := newProxy(t)
			url := "http://" + c.login + p.addr
			args := []string{"-c", "credential.helper=", "-c", "core.askPass="}
			if c.byVariable {
				persons = append(persons, "HTTPS_PROXY="+url)
			} else {
				args = append(args, "-c", "http.proxy="+url)
			}
			if c.method != "" {
				args = append(args, "-c", "http.proxyAuthMethod="+c.method)
			}
			target := "http://row226.invalid/r.git"
			if c.target == "https" {
				target = "https://row226.invalid/r.git"
			}
			args = append(args, "ls-remote", target)

			if fileTicketCache {
				sent, out, err := p.run(t, slices.Concat(persons, []string{"HOME=" + dir}), dir, args)
				if !slices.ContainsFunc(sent, kerberos) {
					t.Fatalf("the control, git ls-remote with the planted cache and none of Git's settings, sent the proxy %d requests with schemes %q and no Kerberos AP-REQ: this git does not read the cache, so the row would pass whatever Git sets (git: %v: %s)", len(sent), schemes(sent), err, out)
				}
			}

			sent, out, err := p.run(t, gitFrom(persons, dir), dir, args)
			if len(sent) == 0 {
				t.Errorf("git ls-remote in Git(%s) sent the proxy no request: the row tests nothing", dir)
			}
			for _, h := range sent {
				s, _, _ := strings.Cut(h, " ")
				switch {
				case kerberos(h):
					t.Errorf("git ls-remote in Git(%s) sent the proxy a %s header that carries a Kerberos ticket: the child authenticates to the proxy with the person's", dir, s)
				case !c.theTests && (strings.EqualFold(s, "Negotiate") || strings.EqualFold(s, "NTLM")):
					t.Errorf("git ls-remote in Git(%s), with no login of the test's, sent the proxy a %s header: the child authenticates to it with the person's ticket or session", dir, s)
				}
			}
			if t.Failed() {
				t.Logf("sent the proxy %d requests with schemes %q (git: %v: %s)", len(sent), schemes(sent), err, out)
			}
		})
	}
}

// rowProxy is a proxy on 127.0.0.1 that answers every request, a GET or a CONNECT, 407, offering
// Negotiate, NTLM and Basic, and keeps each Proxy-Authorization header it was sent.
type rowProxy struct {
	addr string
	mu   sync.Mutex
	sent []string
}

func newProxy(t *testing.T) *rowProxy {
	t.Helper()
	p := &rowProxy{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.sent = append(p.sent, r.Header.Get("Proxy-Authorization"))
		p.mu.Unlock()
		for _, o := range []string{"Negotiate", "NTLM", `Basic realm="row226"`} {
			w.Header().Add("Proxy-Authenticate", o)
		}
		w.WriteHeader(http.StatusProxyAuthRequired)
	}))
	t.Cleanup(srv.Close)
	p.addr = srv.Listener.Addr().String()
	return p
}

// run runs the host's git with args in dir and env, and is each Proxy-Authorization header the
// proxy was sent in that run, with git's output.
func (p *rowProxy) run(t *testing.T, env []string, dir string, args []string) ([]string, []byte, error) {
	t.Helper()
	p.mu.Lock()
	p.sent = nil
	p.mu.Unlock()
	cmd := exec.CommandContext(t.Context(), GitPath(t), args...) //nolint:gosec // the host's git, against the test's own proxy
	cmd.Env, cmd.Dir = env, dir
	out, err := cmd.CombinedOutput() // the proxy refuses every request, so git fails
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.sent), out, err
}

// schemes is the scheme of each header in sent, "" where none was sent.
func schemes(sent []string) []string {
	out := make([]string, 0, len(sent))
	for _, h := range sent {
		s, _, _ := strings.Cut(h, " ")
		out = append(out, s)
	}
	return out
}

// plantTicket writes into dir a krb5.conf that maps 127.0.0.1 to a made-up realm, with no host
// canonicalized and no KDC looked up, and a FILE ticket cache (MIT's format 4, which Heimdal reads
// too) holding a service ticket for HTTP/127.0.0.1 in that realm, and is the two variables that
// name them. Nothing in it is of a real realm: the key and the ticket's cipher are random.
func plantTicket(t *testing.T, dir string) []string {
	t.Helper()
	const realm, aes256 = "ROW226.INVALID", 18
	conf := filepath.Join(dir, "krb5.conf")
	if err := os.WriteFile(conf, []byte("[libdefaults]\n default_realm = "+realm+
		"\n dns_canonicalize_hostname = false\n rdns = false\n dns_lookup_kdc = false\n dns_lookup_realm = false\n"+
		"[domain_realm]\n 127.0.0.1 = "+realm+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ticket := der(0x61, der(0x30, slices.Concat(
		der(0xa0, der(0x02, []byte{5})),
		der(0xa1, der(0x1b, []byte(realm))),
		der(0xa2, der(0x30, slices.Concat(der(0xa0, der(0x02, []byte{2})),
			der(0xa1, der(0x30, slices.Concat(der(0x1b, []byte("HTTP")), der(0x1b, []byte("127.0.0.1")))))))),
		der(0xa3, der(0x30, slices.Concat(der(0xa0, der(0x02, []byte{aes256})), der(0xa1, der(0x02, []byte{1})),
			der(0xa2, der(0x04, random(t, 128)))))),
	)))
	var b bytes.Buffer
	put := func(vs ...any) {
		for _, v := range vs {
			_ = binary.Write(&b, binary.BigEndian, v) // a bytes.Buffer takes every write
		}
	}
	data := func(p []byte) { put(uint32(len(p))); b.Write(p) }
	principal := func(comps ...string) {
		put(uint32(min(len(comps), 2)), uint32(len(comps))) // KRB5_NT_PRINCIPAL, or KRB5_NT_SRV_INST
		data([]byte(realm))
		for _, c := range comps {
			data([]byte(c))
		}
	}
	now := uint32(time.Now().Unix())
	put(uint16(0x0504), uint16(0))
	principal("row226")
	principal("row226")
	principal("HTTP", "127.0.0.1")
	put(uint16(aes256))
	data(random(t, 32))
	put(now-60, now-60, now+3600, uint32(0), uint8(0), uint32(0x40400000), uint32(0), uint32(0))
	data(ticket)
	data(nil)
	cache := filepath.Join(dir, "krb5cc_planted")
	if err := os.WriteFile(cache, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{"KRB5_CONFIG=" + conf, "KRB5CCNAME=FILE:" + cache}
}

// der is the DER encoding of body under tag, for a body shorter than 64 KiB.
func der(tag byte, body []byte) []byte {
	switch n := len(body); {
	case n < 0x80:
		return append([]byte{tag, byte(n)}, body...)
	case n < 0x100:
		return append([]byte{tag, 0x81, byte(n)}, body...)
	default:
		return append([]byte{tag, 0x82, byte(n >> 8), byte(n)}, body...)
	}
}

func random(t *testing.T, n int) []byte {
	t.Helper()
	p := make([]byte, n)
	if _, err := rand.Read(p); err != nil {
		t.Fatal(err)
	}
	return p
}
