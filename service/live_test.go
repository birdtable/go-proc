//go:build live

package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// realTool runs argv as a child of the test with env added to its environment, and gives a non-zero
// exit as its code with no error.
func realTool(env ...string) Runner {
	return func(ctx context.Context, argv []string) ([]byte, int, error) {
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		if exit := (*exec.ExitError)(nil); errors.As(err, &exit) && exit.Exited() {
			return out, exit.ExitCode(), nil
		}
		return out, 0, err
	}
}

// livePlan is a plan for a made-up unit on this system that runs nothing on its own, in one of the
// package's folders (on Windows with no path, as the package keeps no file of a task), and the
// runner for its tool.
func livePlan(t *testing.T) (Plan, Runner) {
	t.Helper()
	return livePlanOf(t, 0)
}

// livePlanOf is livePlan with a name of n bytes, its made-up part padded with a's, or as made when
// n is 0.
func livePlanOf(t *testing.T, n int) (Plan, Runner) {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	id := hex.EncodeToString(b)
	pad := func(name, suffix string) string {
		if n == 0 {
			return name
		}
		return name + strings.Repeat("a", n-len(name)-len(suffix))
	}
	folders, err := Folders()
	if err != nil {
		return Plan{Name: pad("app-"+id, ""), Files: []File{{Path: filepath.Join(t.TempDir(), "app")}}}, realTool()
	}
	switch here().goos {
	case "linux":
		// The runtime folder of the user's manager is on its search path and is emptied at logout;
		// testkit has pointed XDG_RUNTIME_DIR at a folder of its own, so the runner names the real one.
		return liveLinuxPlan(folders[1], pad("app-"+id, ".timer")), realTool("XDG_RUNTIME_DIR=/run/user/" + strconv.Itoa(os.Getuid()))
	case "darwin":
		label := pad("com.example.app."+id, "")
		return Plan{Name: label, Files: []File{{Path: filepath.Join(folders[0], label+".plist"), Body: `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>` + Quote("darwin", label) + `</string>
<key>ProgramArguments</key><array><string>/usr/bin/true</string></array>
</dict></plist>
`}}}, realTool()
	default:
		return Plan{Name: pad("app-"+id, ""), Files: []File{{UTF16: true, Body: `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
<Settings><Enabled>true</Enabled></Settings>
<Actions><Exec><Command>cmd.exe</Command><Arguments>/c exit 0</Arguments></Exec></Actions>
</Task>
`}}}, realTool()
	}
}

// TestAMadeUpUnitIsInstalledQueriedAndRemovedByTheSystemsOwnTool: built with -tags live, on this
// system's own tool in the user's scope, a made-up unit is not installed before; installed and
// enabled after one install and after a second; and not installed after one removal and after a
// second, both of which succeed. It is removed after the test whatever the test found. On a system
// with no tool every call is ErrAbsent.
//
// What it costs when this fails: the fake tool of the other tests answers as the real one does not,
// so a program reports a unit installed that the system does not hold, or fails on a second install.
func TestAMadeUpUnitIsInstalledQueriedAndRemovedByTheSystemsOwnTool(t *testing.T) {
	t.Parallel()
	p, run := livePlan(t)
	if !hasTool(here().goos) {
		_, qerr := Query(t.Context(), p, run)
		for i, err := range []error{Install(t.Context(), p, run), Remove(t.Context(), p, run), qerr} {
			if !errors.Is(err, errors.ErrUnsupported) {
				t.Errorf("call %d on %s gave %v, want ErrAbsent", i, here().goos, err)
			}
		}
		return
	}
	t.Cleanup(func() {
		if err := Remove(context.WithoutCancel(t.Context()), p, run); err != nil {
			t.Errorf("the made-up unit %s could not be removed after the test: %v", p.Name, err)
		}
	})
	steps := 0
	want := func(step string, installed bool) {
		t.Helper()
		steps++
		st, err := Query(t.Context(), p, run)
		if err != nil || st.Installed != installed || st.Enabled != installed {
			t.Fatalf("%s: the tool answers %+v, %v; want installed and enabled: %t", step, st, err, installed)
		}
	}
	want("before the install", false)
	for _, step := range []string{"the install", "a second install"} {
		if err := Install(t.Context(), p, run); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		want("after "+step, true)
	}
	for _, step := range []string{"the removal", "a second removal"} {
		if err := Remove(t.Context(), p, run); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		want("after "+step, false)
	}
	if steps != 5 {
		t.Fatalf("asked the tool %d times, want 5", steps)
	}
}

// TestTheLongestNameTakenInstalls: built with -tags live, on this system's own tool, a made-up unit
// whose name is nameMax bytes, the longest the package takes, is installed and enabled, and then
// removed. On Windows nameMax is Task Scheduler's own limit.
//
// What it costs when this fails: the tool refuses a name the package takes, so an install the
// check passed fails at the tool, and the limit nameMax's doc cites is not the tool's.
func TestTheLongestNameTakenInstalls(t *testing.T) {
	t.Parallel()
	if !hasTool(here().goos) {
		t.Skipf("%s has no tool; TestAMadeUpUnitIsInstalledQueriedAndRemovedByTheSystemsOwnTool holds its refusal", here().goos)
	}
	p, run := livePlanOf(t, nameMax)
	if len(p.Name) != nameMax {
		t.Fatalf("the plan's name is %d bytes, want %d", len(p.Name), nameMax)
	}
	t.Cleanup(func() {
		if err := Remove(context.WithoutCancel(t.Context()), p, run); err != nil {
			t.Errorf("the made-up unit %s could not be removed after the test: %v", p.Name, err)
		}
	})
	if err := Install(t.Context(), p, run); err != nil {
		t.Fatalf("an install of a %d-byte name: %v", len(p.Name), err)
	}
	if st, err := Query(t.Context(), p, run); err != nil || !st.Installed || !st.Enabled {
		t.Fatalf("after the install of a %d-byte name the tool answers %+v, %v; want installed and enabled", len(p.Name), st, err)
	}
	if err := Remove(t.Context(), p, run); err != nil {
		t.Fatalf("the removal of a %d-byte name: %v", len(p.Name), err)
	}
}

// TestAStalePlistIsBootedOutByItsPathOnMacOS: built with -tags live, on macOS, a plist at the plan's
// path whose Label is not the plan's name, bootstrapped by hand as another install would, is booted
// out by Remove, which succeeds; launchd then holds neither that Label nor the name, and the plist is
// gone. The stale Label is booted out after the test whatever the test found. On another system it
// checks nothing, since only launchd loads a file's own label.
//
// What it costs when this fails: a job stays loaded in the login session after Remove deleted its
// plist, where neither the package nor the person finds it by the plan's name.
func TestAStalePlistIsBootedOutByItsPathOnMacOS(t *testing.T) {
	t.Parallel()
	if here().goos != "darwin" {
		return
	}
	p, run := livePlan(t)
	stale := p.Name + ".stale"
	session := "gui/" + strconv.Itoa(os.Getuid())
	held := func(label string) bool {
		t.Helper()
		_, code, err := run(context.WithoutCancel(t.Context()), []string{"launchctl", "print", session + "/" + label})
		if err != nil || (code != 0 && code != 113) {
			t.Fatalf("launchctl print %s: exit %d, %v", label, code, err)
		}
		return code == 0
	}
	t.Cleanup(func() {
		for _, label := range []string{stale, p.Name} {
			if held(label) {
				if _, code, err := run(context.WithoutCancel(t.Context()), []string{"launchctl", "bootout", session + "/" + label}); code != 0 || err != nil {
					t.Errorf("the label %s could not be booted out after the test: exit %d, %v", label, code, err)
				}
			}
		}
		if err := os.Remove(p.Files[0].Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the plist %s could not be removed after the test: %v", p.Files[0].Path, err)
		}
	})
	body := strings.Replace(p.Files[0].Body, "<string>"+p.Name+"</string>", "<string>"+stale+"</string>", 1)
	if body == p.Files[0].Body {
		t.Fatal("the live plist does not hold its Label as a string")
	}
	if err := os.MkdirAll(filepath.Dir(p.Files[0].Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Files[0].Path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, code, err := run(t.Context(), []string{"launchctl", "bootstrap", session, p.Files[0].Path}); code != 0 || err != nil {
		t.Fatalf("the stale plist's bootstrap: exit %d, %v: %s", code, err, out)
	}
	if !held(stale) || held(p.Name) {
		t.Fatalf("after the bootstrap launchd holds %s: %t and %s: %t; want the stale Label alone", stale, held(stale), p.Name, held(p.Name))
	}
	if err := Remove(t.Context(), p, run); err != nil {
		t.Fatalf("the removal over the stale plist: %v", err)
	}
	if held(stale) || held(p.Name) {
		t.Errorf("after the removal launchd holds %s: %t and %s: %t", stale, held(stale), p.Name, held(p.Name))
	}
	if _, err := os.Stat(p.Files[0].Path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the plist is there after the removal: %v", err)
	}
}
