package gittest

import (
	"bytes"
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"
)

// TestAGitChildSendsAHostNoTicketOfThePersons: a git child of Git(dir) that a host answers 401,
// offering Negotiate, NTLM and Basic, authenticates as nobody but the test. The person's Kerberos
// ticket, and on Windows their logon session through SSPI, is found with no file in HOME and no
// variable, so HOME=dir does not keep libcurl from them, and git from 2.55 on retries a Negotiate
// host with an empty login so that libcurl sends one. With no login of the test's,
// the child sends no Negotiate or NTLM header at all: whatever it sent would be the person's. With
// a .netrc of the test's own for the host, git has a login, and libcurl spends it on Negotiate,
// where GSS-API takes the ticket cache and leaves the login unread; SSPI builds its token from the
// login, which is the test's. So the case holds that no header carries a Kerberos AP-REQ, as the
// test plants no ticket. The row prints the scheme of each header and never the token.
//
// The headers prove something only where the person has a ticket or a session to send: a host with
// no Kerberos ticket cache, other than Windows, passes those checks whatever Git sets. Their red,
// unfixed, and their green were shown on Linux with a ticket from a local KDC, on git 2.47.3 and
// 2.56.0. So the row also counts git's requests, which needs no ticket: 1 with no
// login, the request the host answers 401, and 2 with the test's netrc, that one and one with the
// login. With http.emptyAuth not false, git 2.55 and later add the empty-login retry, and both cases
// make 3 (macOS, git 2.56.0; Windows, git 2.55.0.windows.3). A git before 2.55 makes no such retry,
// so there the count is the same with the setting or without it.
//
// What it costs when this fails: a test's git that fetches or pushes over http or https
// authenticates to the host as the person, with their Kerberos ticket or their Windows session.
func TestAGitChildSendsAHostNoTicketOfThePersons(t *testing.T) {
	t.Parallel()
	offers := []string{"Negotiate", "NTLM", `Basic realm="row"`}
	for _, c := range []struct {
		name  string
		netrc bool
	}{{"Git", false}, {"Git with a netrc of the test's own", true}} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if c.netrc {
				netrcEntry(t, filepath.Join(dir, ".netrc"), "row208", "planted-in-the-tests-own-folder")
			}
			sent, out, err := lsRemote(t, Git(dir), dir, offers...)
			schemes := make([]string, 0, len(sent))
			for _, h := range sent {
				s, _, _ := strings.Cut(h, " ")
				schemes = append(schemes, s)
				switch {
				case kerberos(h):
					t.Errorf("git ls-remote in Git(%s) sent a %s header that carries a Kerberos ticket: the child authenticates with the person's", dir, s)
				case !c.netrc && (strings.EqualFold(s, "Negotiate") || strings.EqualFold(s, "NTLM")):
					t.Errorf("git ls-remote in Git(%s), with no login of the test's, sent a %s header: the child authenticates with the person's ticket or session", dir, s)
				}
			}
			want := 1
			if c.netrc {
				want = 2
			}
			if len(sent) != want {
				t.Errorf("git ls-remote in Git(%s) sent %d requests, want %d: git made its empty-login retry to a Negotiate host, which http.emptyAuth=false stops, and that retry spends the person's ticket or session where they hold one", dir, len(sent), want)
			}
			if t.Failed() {
				t.Logf("offered %q, sent %d requests with schemes %q (git: %v: %s)", offers, len(sent), schemes, err, out)
			}
		})
	}
}

// kerberos is whether the Authorization header h carries a Kerberos AP-REQ: a GSS-API token whose
// mechanism is Kerberos (RFC 1964's OID, or Microsoft's), followed by the AP-REQ token id 01 00. A
// SPNEGO header lists the Kerberos OIDs among its mechanisms whether or not it sends a ticket, but
// only a ticket follows one with that id.
func kerberos(h string) bool {
	_, token, _ := strings.Cut(h, " ")
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(token))
	if err != nil {
		return false
	}
	for _, oid := range [][]byte{
		{0x06, 0x09, 0x2a, 0x86, 0x48, 0x86, 0xf7, 0x12, 0x01, 0x02, 0x02, 0x01, 0x00},
		{0x06, 0x09, 0x2a, 0x86, 0x48, 0x82, 0xf7, 0x12, 0x01, 0x02, 0x02, 0x01, 0x00},
	} {
		if bytes.Contains(raw, oid) {
			return true
		}
	}
	return false
}
