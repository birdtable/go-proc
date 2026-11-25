package proc

import (
	"strings"
	"testing"
)

// TestAnNSpidOfMoreThanOneNumberIsAnOuterNamespacesMount: a self/status whose NSpid line holds this
// process's pid alone shows the mount is its own pid namespace's. Two numbers, as an outer
// namespace's mount lists, even with this pid first or last; another pid; no number; no NSpid line,
// as before Linux 4.1; and two NSpid lines each show nothing, and are refused.
//
// What it costs when this fails: through an outer namespace's proc mount whose self/stat names a
// pid equal to this process's by chance, the guard reads another process than the kill reaches,
// and Stop kills a group the id never named.
func TestAnNSpidOfMoreThanOneNumberIsAnOuterNamespacesMount(t *testing.T) {
	t.Parallel()
	const pid = 4242
	rows := []struct {
		claim  string
		status string
		says   string
	}{
		{"one number, this pid", "Name:\tproc.test\nTgid:\t4242\nPid:\t4242\nNSpid:\t4242\nNSpgid:\t4242\n", ""},
		{"one number, tabs and spaces", "NSpid: \t 4242 \t\n", ""},
		{"two numbers, this pid last", "Pid:\t4242\nNSpid:\t9001\t4242\n", "lists 2 ids"},
		{"two numbers, this pid first", "Pid:\t4242\nNSpid:\t4242\t7\n", "lists 2 ids"},
		{"three numbers", "NSpid:\t31\t17\t4242\n", "lists 3 ids"},
		{"another pid", "NSpid:\t4243\n", `names process "4243"`},
		{"no number", "NSpid:\t\n", "lists 0 ids"},
		{"no NSpid line", "Name:\tproc.test\nPid:\t4242\n", "0 NSpid lines"},
		{"two NSpid lines", "NSpid:\t4242\nNSpid:\t4242\n", "2 NSpid lines"},
		{"an NSpgid line alone", "NSpgid:\t4242\n", "0 NSpid lines"},
	}
	for _, r := range rows {
		why := nspidOwn(r.status, pid)
		switch {
		case r.says == "" && why != "":
			t.Errorf("%s: %q, want the mount its own", r.claim, why)
		case r.says != "" && !strings.Contains(why, r.says):
			t.Errorf("%s: %q, want it refused, saying %q", r.claim, why, r.says)
		}
	}
	if len(rows) < 10 {
		t.Fatalf("ran %d rows", len(rows))
	}
}
