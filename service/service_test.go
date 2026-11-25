package service

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// system is a fake of one system's tool, which answers as the tool does and records each argv. Its
// systemd is systemd 257's as a native run saw it: a calendar year past 2199 is a bad setting, an
// enable of a unit with one makes the symlink and fails to start it, a stop of an inactive unit
// systemd could not load is refused, and a disable needs a unit systemd knows. Its launchd holds
// labels: a bootstrap takes the Label of the plist it is given, a bootout by a plist's path drops
// that plist's Label, and either by path or by name exits non-zero for a label it does not hold.
type system struct {
	goos    string
	files   []string // the plan's file paths, which a daemon-reload reads
	load    string   // systemd's LoadState since its last reload; empty is not-found
	active  bool     // systemd's ActiveState is active
	held    bool     // Task Scheduler holds the task
	task    []byte   // the XML Task Scheduler read at its last create
	labels  []string // the labels launchd holds
	enabled bool
	fail    map[string]int // a verb that exits with this code and does nothing
	ran     [][]string
}

// hostOf is a host of goos whose folders for a plan's files are dirs.
func hostOf(goos string, dirs ...string) host {
	return host{goos: goos, uid: 501, folders: dirs}
}

// reload is what systemd makes of the plan's files: not-found when one is missing, bad-setting when
// an OnCalendar names a year past 2199, and loaded otherwise.
func (s *system) reload() string {
	if len(s.files) == 0 {
		return ""
	}
	load := "loaded"
	for _, f := range s.files {
		b, err := os.ReadFile(f)
		if err != nil {
			return ""
		}
		for line := range strings.Lines(string(b)) {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "OnCalendar="); ok && len(v) >= 4 {
				if year, err := strconv.Atoi(v[:4]); err == nil && year > 2199 {
					load = "bad-setting"
				}
			}
		}
	}
	return load
}

// label is the Label of the plist at path, or "" when there is none to read.
func label(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	l, err := plistLabel(string(b))
	if err != nil {
		return ""
	}
	return l
}

func (s *system) run(_ context.Context, argv []string) ([]byte, int, error) {
	s.ran = append(s.ran, slices.Clone(argv))
	verb := argv[1]
	if s.goos == "linux" {
		verb = argv[2]
	}
	if code, ok := s.fail[verb]; ok {
		return []byte("refused"), code, nil
	}
	switch verb {
	case "daemon-reload":
		s.load = s.reload()
	case "enable":
		if s.load == "" {
			return []byte("Failed to enable unit: Unit file app.timer does not exist."), 1, nil
		}
		s.enabled = true
		if s.load == "bad-setting" {
			return []byte("Created symlink.\nFailed to start app.timer: Unit app.timer has a bad unit file setting."), 1, nil
		}
		s.active = true
	case "stop":
		if s.load != "loaded" && !s.active {
			return []byte("Failed to stop app.timer: Unit app.timer not loaded."), 5, nil
		}
		s.active = false
	case "disable":
		if s.load == "" {
			return []byte("Failed to disable unit: Unit file app.timer does not exist."), 1, nil
		}
		s.enabled = false
	case "show":
		load, file, active := cmp.Or(s.load, "not-found"), "disabled", "inactive"
		if s.load == "" {
			file = ""
		}
		if s.enabled {
			file = "enabled"
		}
		if s.active {
			active = "active"
		}
		return []byte("LoadState=" + load + "\nUnitFileState=" + file + "\nActiveState=" + active + "\n"), 0, nil

	case "bootstrap":
		l := label(argv[3])
		if l == "" || slices.Contains(s.labels, l) {
			return []byte("Bootstrap failed: 5: Input/output error"), 5, nil
		}
		s.labels = append(s.labels, l)
	case "bootout":
		l, code := strings.TrimPrefix(argv[2], "gui/501/"), 3
		if len(argv) == 4 {
			l, code = label(argv[3]), 5
		}
		i := slices.Index(s.labels, l)
		if l == "" || i < 0 {
			return []byte("Boot-out failed: No such process"), code, nil
		}
		s.labels = slices.Delete(s.labels, i, i+1)
	case "print":
		if !slices.Contains(s.labels, strings.TrimPrefix(argv[2], "gui/501/")) {
			return []byte("Could not find service"), 113, nil
		}
		return []byte("com.example.app = {\n}"), 0, nil
	case "/create", "/delete", "/query":
		return s.schtasks(verb, argv)
	}
	return nil, 0, nil
}

// schtasks is the fake Task Scheduler's answer to verb: /create reads the task's XML at argv[5].
func (s *system) schtasks(verb string, argv []string) ([]byte, int, error) {
	const notFound = "ERROR: The system cannot find the file specified."
	switch verb {
	case "/create":
		b, err := os.ReadFile(argv[5])
		if err != nil {
			return []byte(notFound), 1, nil
		}
		s.task, s.held, s.enabled = b, true, true
	case "/delete":
		if !s.held {
			return []byte(notFound), 1, nil
		}
		s.held = false
	case "/query":
		if !s.held {
			return []byte(notFound), 1, nil
		}
		return []byte(`<?xml version="1.0" encoding="UTF-16"?><Task><Settings><Enabled>true</Enabled></Settings></Task>`), 0, nil
	}
	return nil, 0, nil
}

// plist is a plist whose Label is l.
func plist(l string) string {
	return "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<plist version=\"1.0\"><dict>\n<key>Label</key><string>" + l + "</string>\n<key>ProgramArguments</key><array><string>/usr/bin/true</string></array>\n</dict></plist>\n"
}

// plans is a plan for each system, its files in dir; on Windows the one file has no path.
func plans(dir string) map[string]Plan {
	return map[string]Plan{
		"linux": {Name: "app.timer", Files: []File{
			{Path: filepath.Join(dir, "app.service"), Body: "[Service]\nExecStart=/usr/bin/app\n"},
			{Path: filepath.Join(dir, "app.timer"), Body: "[Timer]\nOnCalendar=daily\n"},
		}},
		"darwin":  {Name: "com.example.app", Files: []File{{Path: filepath.Join(dir, "com.example.app.plist"), Body: plist("com.example.app")}}},
		"windows": {Name: "app-sweep", Files: []File{{Body: "<Task/>\n", UTF16: true}}},
	}
}

// taskXML is the path of the XML a Windows install gives schtasks, <tmp>/task.xml with <tmp> a
// folder of the system's temp folder, and whether s is one. On Windows the path is the one the
// folder's handle names, so the temp folder is matched as a file, not by its spelling.
func taskXML(s string) bool {
	dir, base := filepath.Split(s)
	dir = filepath.Clean(dir)
	parent, err := os.Stat(filepath.Dir(dir))
	root, rerr := os.Stat(os.TempDir())
	return base == "task.xml" && err == nil && rerr == nil && os.SameFile(parent, root) && strings.HasPrefix(filepath.Base(dir), "task-")
}

// liveLinuxPlan is the live test's timer and its service, in dir, which never run: systemd's
// calendar years end at 2199, and it refuses a timer that names a later one.
func liveLinuxPlan(dir, name string) Plan {
	return Plan{Name: name + ".timer", Files: []File{
		{Path: filepath.Join(dir, name+".service"), Body: "[Service]\nType=oneshot\nExecStart=" + Quote("linux", "/bin/true") + "\n"},
		{Path: filepath.Join(dir, name+".timer"), Body: "[Timer]\nOnCalendar=2199-12-31\n\n[Install]\nWantedBy=timers.target\n"},
	}}
}

// hasTool is whether goos is one of the three systems the package has a tool for.
func hasTool(goos string) bool { return goos == "linux" || goos == "darwin" || goos == "windows" }

// TestEachStepRunsTheSystemsToolWithTheseArgumentsInThisOrder: on a fake of each system's tool,
// install, install again, query, remove and remove again run exactly these argv, in order, write
// each file as Bytes gives it and remove it after (on Windows, give schtasks the task's Bytes from a
// temp folder); installing twice leaves what once does, and removing twice succeeds both times.
//
// What it costs when this fails: a unit is enabled before systemd has read it, a macOS label
// already held is bootstrapped again and the install fails, a removal of what is gone fails, or a
// unit's file is left behind after it is removed.
func TestEachStepRunsTheSystemsToolWithTheseArgumentsInThisOrder(t *testing.T) {
	t.Parallel()
	show := []string{"systemctl", "--user", "show", "-p", "LoadState,UnitFileState,ActiveState", "app.timer"}
	reload := []string{"systemctl", "--user", "daemon-reload"}
	lprint := []string{"launchctl", "print", "gui/501/com.example.app"}
	task := []string{"schtasks", "/query", "/tn", "app-sweep", "/xml"}
	want := map[string]map[string][][]string{
		"linux": {
			"install":       {reload, {"systemctl", "--user", "enable", "--now", "app.timer"}, show},
			"install again": {reload, {"systemctl", "--user", "enable", "--now", "app.timer"}, show},
			"query":         {show},
			"remove":        {show, {"systemctl", "--user", "stop", "app.timer"}, {"systemctl", "--user", "disable", "app.timer"}, reload, show},
			"remove again":  {show, reload, show},
		},
		"darwin": {
			"install":       {lprint, {"launchctl", "bootstrap", "gui/501", "<dir>/com.example.app.plist"}, lprint},
			"install again": {lprint, lprint},
			"query":         {lprint},
			"remove":        {lprint, {"launchctl", "bootout", "gui/501", "<dir>/com.example.app.plist"}, lprint},
			"remove again":  {lprint, lprint},
		},
		"windows": {
			"install":       {{"schtasks", "/create", "/tn", "app-sweep", "/xml", "<tmp>/task.xml", "/f"}, task},
			"install again": {{"schtasks", "/create", "/tn", "app-sweep", "/xml", "<tmp>/task.xml", "/f"}, task},
			"query":         {task},
			"remove":        {task, {"schtasks", "/delete", "/tn", "app-sweep", "/f"}, task},
			"remove again":  {task, task},
		},
	}
	steps := []string{"install", "install again", "query", "remove", "remove again"}
	checked := 0
	for goos, wants := range want {
		dir := t.TempDir()
		p := plans(dir)[goos]
		sys := &system{goos: goos}
		for _, f := range p.Files {
			sys.files = append(sys.files, f.Path)
		}
		for _, step := range steps {
			sys.ran = nil
			var err error
			var st State
			switch step {
			case "install", "install again":
				err = install(t.Context(), hostOf(goos, dir), p, sys.run)
			case "query":
				st, err = query(t.Context(), hostOf(goos, dir), p, sys.run)
				if !st.Installed || !st.Enabled {
					t.Errorf("%s: query after the install says %+v, want installed and enabled", goos, st)
				}
			default:
				err = remove(t.Context(), hostOf(goos, dir), p, sys.run)
			}
			if err != nil {
				t.Errorf("%s, %s: %v", goos, step, err)
			}
			got := slices.Clone(sys.ran)
			for i := range got {
				got[i] = slices.Clone(got[i])
				for j := range got[i] {
					got[i][j] = strings.Replace(got[i][j], dir+string(filepath.Separator), "<dir>/", 1)
					if taskXML(got[i][j]) {
						got[i][j] = "<tmp>/task.xml"
					}
				}
			}
			if !slices.EqualFunc(got, wants[step], slices.Equal) {
				t.Errorf("%s, %s ran\n%q\nwant\n%q", goos, step, got, wants[step])
			}
			if goos == "windows" && strings.HasPrefix(step, "install") && !bytes.Equal(sys.task, p.Files[0].Bytes()) {
				t.Errorf("windows, %s: schtasks read %q, want %q", step, sys.task, p.Files[0].Bytes())
			}
			for _, f := range p.Files {
				if goos == "windows" {
					break
				}
				b, err := os.ReadFile(f.Path)
				gone := step == "remove" || step == "remove again"
				if gone && !errors.Is(err, os.ErrNotExist) {
					t.Errorf("%s, %s: %s is still there (%v)", goos, step, f.Path, err)
				}
				if !gone && !bytes.Equal(b, f.Bytes()) {
					t.Errorf("%s, %s: %s holds %q (%v), want %q", goos, step, f.Path, b, err, f.Bytes())
				}
			}
			checked++
		}
	}
	if checked != 15 {
		t.Fatalf("checked %d steps, want 15", checked)
	}
}

// TestAMacOSLabelWhosePlistChangedIsBootedOutThenBootstrappedAgain: installing a held label with a
// changed plist boots it out before it bootstraps it, and with the same plist runs neither.
//
// What it costs when this fails: a changed plist never reaches launchd, which keeps running the old
// one, or a second install fails because launchctl bootstrap refuses a label it holds.
func TestAMacOSLabelWhosePlistChangedIsBootedOutThenBootstrappedAgain(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := plans(dir)["darwin"]
	sys := &system{goos: "darwin", files: []string{p.Files[0].Path}}
	if err := install(t.Context(), hostOf("darwin", dir), p, sys.run); err != nil {
		t.Fatal(err)
	}
	p.Files[0].Body = strings.Replace(plist("com.example.app"), "/usr/bin/true", "/usr/bin/false", 1)
	sys.ran = nil
	if err := install(t.Context(), hostOf("darwin", dir), p, sys.run); err != nil {
		t.Fatal(err)
	}
	verbs := []string{}
	for _, argv := range sys.ran {
		verbs = append(verbs, argv[1])
	}
	if want := []string{"print", "bootout", "bootstrap", "print"}; !slices.Equal(verbs, want) {
		t.Errorf("the second install with a changed plist ran %q, want %q", verbs, want)
	}
	if b, err := os.ReadFile(p.Files[0].Path); err != nil || string(b) != p.Files[0].Body {
		t.Errorf("the plist holds %q (%v), want the changed body", b, err)
	}
}

// TestAMacOSInstallWhoseBootoutOrBootstrapFailsPutsTheOldPlistBack: an install with a changed plist
// whose bootout, or whose bootstrap after it, fails returns the failure and leaves the old plist on
// disk, and an install of a new plist whose bootstrap fails leaves none; so the same install again
// boots out and bootstraps, or bootstraps, and launchd holds the new plist.
//
// What it costs when this fails: a second install finds the new plist already on disk, takes it for
// the one launchd holds, and returns nil, while launchd runs the old plist until the next login.
func TestAMacOSInstallWhoseBootoutOrBootstrapFailsPutsTheOldPlistBack(t *testing.T) {
	t.Parallel()
	changed := strings.Replace(plist("com.example.app"), "/usr/bin/true", "/usr/bin/false", 1)
	for _, r := range []struct {
		claim, fails string
		installed    bool     // the old plist was installed before
		again        []string // the verbs of the install again
	}{
		{"a bootout that fails", "bootout", true, []string{"print", "bootout", "bootstrap", "print"}},
		{"a bootstrap after the bootout that fails", "bootstrap", true, []string{"print", "bootstrap", "print"}},
		{"a first bootstrap that fails", "bootstrap", false, []string{"print", "bootstrap", "print"}},
	} {
		dir := t.TempDir()
		p := plans(dir)["darwin"]
		sys := &system{goos: "darwin"}
		var old []byte
		if r.installed {
			if err := install(t.Context(), hostOf("darwin", dir), p, sys.run); err != nil {
				t.Fatal(err)
			}
			old = []byte(p.Files[0].Body)
		}
		p.Files[0].Body = changed
		sys.fail = map[string]int{r.fails: 36}
		if err := install(t.Context(), hostOf("darwin", dir), p, sys.run); err == nil || !strings.Contains(err.Error(), "exited 36") {
			t.Errorf("%s: %v, want the exit", r.claim, err)
		}
		if b, err := os.ReadFile(p.Files[0].Path); (old == nil) != errors.Is(err, os.ErrNotExist) || !bytes.Equal(b, old) {
			t.Errorf("%s: the plist on disk is %q (%v), want %q", r.claim, b, err, old)
		}
		sys.fail, sys.ran = nil, nil
		if err := install(t.Context(), hostOf("darwin", dir), p, sys.run); err != nil {
			t.Errorf("%s: the install again: %v", r.claim, err)
		}
		verbs := []string{}
		for _, argv := range sys.ran {
			verbs = append(verbs, argv[1])
		}
		if !slices.Equal(verbs, r.again) {
			t.Errorf("%s: the install again ran %q, want %q", r.claim, verbs, r.again)
		}
		if b, err := os.ReadFile(p.Files[0].Path); err != nil || string(b) != changed {
			t.Errorf("%s: after the install again the plist holds %q (%v), want the changed body", r.claim, b, err)
		}
	}
}

// TestAnInstallTheToolDoesNotHoldAfterwardIsAnError: an install whose every command exits 0 and
// whose query then says not installed, or installed and not enabled, returns an error; so does a
// removal after which the tool still holds the unit, and a step that exits non-zero.
//
// What it costs when this fails: a program reports a unit as running because the tool said yes,
// on a machine where nothing will ever run it.
func TestAnInstallTheToolDoesNotHoldAfterwardIsAnError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := plans(dir)["windows"]
	answers := map[string]struct {
		out  string
		code int
	}{
		"absent":   {"", 1},
		"disabled": {"<Task><Settings><Enabled>false</Enabled></Settings></Task>", 0},
	}
	for claim, a := range answers {
		run := func(_ context.Context, argv []string) ([]byte, int, error) {
			if argv[1] == "/query" {
				return []byte(a.out), a.code, nil
			}
			return nil, 0, nil
		}
		if err := install(t.Context(), hostOf("windows", dir), p, run); err == nil || !strings.Contains(err.Error(), "installed: ") {
			t.Errorf("an install the tool then answers %s for: %v, want an error saying what it answers", claim, err)
		}
	}
	held := func(_ context.Context, argv []string) ([]byte, int, error) {
		if argv[1] == "/query" {
			return []byte("<Task/>"), 0, nil
		}
		return nil, 0, nil
	}
	if err := remove(t.Context(), hostOf("windows", dir), p, held); err == nil || !strings.Contains(err.Error(), "still holds") {
		t.Errorf("a removal the tool still holds after: %v, want an error saying so", err)
	}
	refused := func(_ context.Context, argv []string) ([]byte, int, error) {
		if argv[1] == "/create" {
			return []byte("ERROR: Access is denied."), 5, nil
		}
		return nil, 1, nil
	}
	if err := install(t.Context(), hostOf("windows", dir), p, refused); err == nil || !strings.Contains(err.Error(), "exited 5") {
		t.Errorf("an install whose create exits 5: %v, want an error naming the exit", err)
	}
	if len(answers) != 2 {
		t.Fatalf("%d answers", len(answers))
	}
}

// TestQueryReadsOnlyTheToolsMachineAnswers: systemctl show's properties, launchctl print's exit code,
// and schtasks' exit code and the task's Settings/Enabled, in UTF-8 or UTF-16, give these states;
// any other exit code, a missing property, a LoadState of error and XML that does not parse give an
// error, and the words a tool prints around the answer decide nothing.
//
// What it costs when this fails: a query on a localised Windows or a newer systemd says a unit is
// absent that runs, or installed when its file did not load.
func TestQueryReadsOnlyTheToolsMachineAnswers(t *testing.T) {
	t.Parallel()
	utf16Task, err := os.ReadFile(filepath.Join("testdata", "task.xml.utf16"))
	if err != nil {
		t.Fatal(err)
	}
	none, on, off := State{}, State{Installed: true, Enabled: true}, State{Installed: true}
	known := func(s State) State { s.ActiveKnown = true; return s }
	rows := []struct {
		claim, goos, out string
		code             int
		want             State
		fails            bool
	}{
		{"systemd: no such unit, known inactive", "linux", "LoadState=not-found\nUnitFileState=\nActiveState=inactive\n", 0, known(none), false},
		{"systemd: enabled and active", "linux", "ActiveState=active\nLoadState=loaded\nUnitFileState=enabled\n", 0, State{Installed: true, Enabled: true, Active: true, ActiveKnown: true}, false},
		{"systemd: loaded and disabled", "linux", "LoadState=loaded\r\nUnitFileState=disabled\r\nActiveState=inactive\r\n", 0, known(off), false},
		{"systemd: masked", "linux", "LoadState=masked\nUnitFileState=masked\nActiveState=inactive\n", 0, known(off), false},
		{"systemd: enabled, activating is not active", "linux", "LoadState=loaded\nUnitFileState=enabled\nActiveState=activating\n", 0, known(on), false},
		{"launchd: words that say running are no machine answer, so Active is not known", "darwin", "gui/501/com.x = {\n\tstate = running\n\tpid = 42\n}", 0, on, false},
		{"schtasks: a task's XML says nothing of a run, so Active is not known", "windows", "<Task><Settings><Enabled>true</Enabled></Settings><Actions><Exec><Command>running</Command></Exec></Actions></Task>", 0, on, false},
		{"systemd: the file did not load", "linux", "LoadState=error\nUnitFileState=\nActiveState=inactive\n", 0, none, true},
		{"systemd: a property missing", "linux", "LoadState=loaded\nActiveState=active\n", 0, none, true},
		{"systemd: no user manager", "linux", "Failed to connect to bus: No medium found", 1, none, true},
		{"systemd: properties with a non-zero exit", "linux", "LoadState=loaded\nUnitFileState=enabled\nActiveState=active\n", 1, none, true},
		{"launchd: held", "darwin", "", 0, on, false},
		{"launchd: not held", "darwin", "Could not find service", 113, none, false},
		{"launchd: no such domain", "darwin", "", 112, none, true},
		{"launchd: words that say not found, with exit 0", "darwin", "Could not find service", 0, on, false},
		{"schtasks: no such task", "windows", "ERROR: Die angegebene Datei wurde nicht gefunden.", 1, none, false},
		{"schtasks: enabled", "windows", `<?xml version="1.0" encoding="UTF-16"?><Task xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task"><Triggers><TimeTrigger><Enabled>false</Enabled></TimeTrigger></Triggers><Settings><Enabled>true</Enabled></Settings></Task>`, 0, on, false},
		{"schtasks: disabled", "windows", "<Task><Settings><Enabled>false</Enabled></Settings></Task>", 0, off, false},
		{"schtasks: disabled as 0", "windows", "<Task><Settings><Enabled> 0 </Enabled></Settings></Task>", 0, off, false},
		{"schtasks: no Enabled is enabled", "windows", "<Task><Settings></Settings></Task>", 0, on, false},
		{"schtasks: UTF-16 with its byte order mark", "windows", string(utf16Task), 0, on, false},
		{"schtasks: a description in another code page", "windows", "<Task><RegistrationInfo><Description>\xef\xf0\xe8</Description></RegistrationInfo><Settings><Enabled>false</Enabled></Settings></Task>", 0, off, false},
		{"schtasks: Enabled that is no boolean", "windows", "<Task><Settings><Enabled>yes</Enabled></Settings></Task>", 0, none, true},
		{"schtasks: not XML", "windows", "SUCCESS", 0, none, true},
		{"schtasks: access denied", "windows", "ERROR: Access is denied.", 5, none, true},
	}
	dir := t.TempDir()
	p := plans(dir)
	for _, r := range rows {
		run := func(context.Context, []string) ([]byte, int, error) { return []byte(r.out), r.code, nil }
		got, err := query(t.Context(), hostOf(r.goos, dir), p[r.goos], run)
		if (err != nil) != r.fails || got != r.want {
			t.Errorf("%s: %+v, %v; want %+v and an error: %t", r.claim, got, err, r.want, r.fails)
		}
	}
	unrun := func(context.Context, []string) ([]byte, int, error) { return nil, 0, errors.New("exec: not found") }
	if _, err := query(t.Context(), hostOf("linux", dir), p["linux"], unrun); err == nil || !strings.Contains(err.Error(), "could not be run") {
		t.Errorf("a query whose tool could not be run: %v, want an error saying so", err)
	}
	if len(rows) < 20 {
		t.Fatalf("%d rows", len(rows))
	}
}

// TestEveryCallRefusesAPlanTheToolWouldMisreadAndRunsNothing: on each system, a name that is not
// lower-case ASCII letters, digits, '.', '-' and '_' (a capital, as in App and aPP, a pattern of *,
// ? or [, a space, a slash, a backslash, a control character, a letter outside ASCII), that starts
// with '-' or '.', that is empty or longer than nameMax, 230 bytes, that ends in '.', or whose part
// before its first '.' is a Windows device name (con, prn, aux, nul, com0 to com9, lpt0 to lpt9;
// with capitals too, each refused by the capital); a nil runner; a file that is no
// absolute path, not in its clean form, or outside
// the host's folders; on Windows a task's file with any path; on macOS and Windows a plan with
// other than one file; and on macOS a plist
// whose Label is not the plan's name or cannot be read, are each refused by install, remove and
// query with ErrUsage, before any file is written or any tool run. The same plans with a name the
// package takes, app and nameMax bytes long among them, and names that only resemble a device
// (app.v2, console, com10, com, a.nul), pass the check.
//
// What it costs when this fails: App reaches schtasks /f or a plist's path as another program's
// app, and replaces or removes it, a name of * reaches schtasks /delete and deletes every task on the
// computer, app-* reaches systemctl as a pattern of many units, --global or gui/0/x reaches the tool
// as an option or another domain, a name Win32's file naming rules read as another file or a device
// reaches Task Scheduler, which keeps each task as a file of its name, a removal deletes a file
// outside the unit folders, or a macOS label
// stays loaded after its plist is gone.
func TestEveryCallRefusesAPlanTheToolWouldMisreadAndRunsNothing(t *testing.T) {
	t.Parallel()
	dir, other := t.TempDir(), t.TempDir()
	systems := []string{"linux", "darwin", "windows"}
	// named is the system's plan of plans(dir) under another name, its plist's Label that name too,
	// so that only the name can refuse it.
	named := func(goos, name string) Plan {
		p := plans(dir)[goos]
		p.Name = name
		if goos == "darwin" {
			p.Files = []File{{Path: p.Files[0].Path, Body: plist(Quote("darwin", name))}}
		}
		return p
	}
	type row struct {
		claim, goos string
		p           Plan
		nilRun      bool
		folders     []string // the host's folders, when not dir alone
	}
	var rows []row
	names := []string{"*", "app-*", "?", "app?", "[", "app[0]", "app timer", "a/b", "gui/0/com.example.app", `a\b`, `\app`,
		"-app", "--global", ".app", "app\nb", "app\x00", "appé", "", strings.Repeat("a", nameMax+1),
		"app.", "app..", "NUL", "con.app", "COM1", "lpt9.x", "com0", "LPT0.x", "Aux",
		"App", "aPP", "nul", "aux.x", "prn", "com9", "lpt0.x"}
	for _, name := range names {
		for _, goos := range systems {
			rows = append(rows, row{"the name " + strconv.Quote(name), goos, named(goos, name), false, nil})
		}
	}
	darwin := plans(dir)["darwin"]
	with := func(p Plan, files ...File) Plan { p.Files = files; return p }
	rows = append(rows,
		row{"a nil runner", "linux", plans(dir)["linux"], true, nil},
		// The host holds the relative folder ".", so only the absolute-path check refuses the file.
		row{"a relative file", "linux", with(plans(dir)["linux"], File{Path: "app.timer"}), false, []string{dir, "."}},
		row{"a file not in its clean form", "linux", with(plans(dir)["linux"], File{Path: dir + string(filepath.Separator) + "x" + string(filepath.Separator) + ".." + string(filepath.Separator) + "app.timer"}), false, nil},
		row{"a file outside the folders", "linux", with(plans(dir)["linux"], File{Path: filepath.Join(other, "app.timer")}), false, nil},
		// The host holds dir, so only the Windows rule refuses a task's file with a path in it.
		row{"a task's file with a path, in the host's folder", "windows", with(plans(dir)["windows"], File{Path: filepath.Join(dir, "app.xml"), Body: "<Task/>\n", UTF16: true}), false, nil},
		row{"a file outside the folders, on macOS", "darwin", with(darwin, File{Path: filepath.Join(other, "com.example.app.plist"), Body: darwin.Files[0].Body}), false, nil},
		row{"no file on macOS", "darwin", with(darwin), false, nil},
		row{"two files on Windows", "windows", with(plans(dir)["windows"], plans(dir)["windows"].Files[0], plans(dir)["windows"].Files[0]), false, nil},
		row{"a plist whose Label is another", "darwin", with(darwin, File{Path: darwin.Files[0].Path, Body: plist("com.example.other")}), false, nil},
		row{"a plist with no Label", "darwin", with(darwin, File{Path: darwin.Files[0].Path, Body: "<plist><dict><key>Program</key><string>/usr/bin/true</string></dict></plist>"}), false, nil},
		row{"a plist with two Labels", "darwin", with(darwin, File{Path: darwin.Files[0].Path, Body: "<plist><dict><key>Label</key><string>com.example.app</string><key>Label</key><string>com.example.app</string></dict></plist>"}), false, nil},
		row{"a plist whose Label is no string", "darwin", with(darwin, File{Path: darwin.Files[0].Path, Body: "<plist><dict><key>Label</key><array><string>com.example.app</string></array></dict></plist>"}), false, nil},
		row{"a plist whose Label is in a dict below the top", "darwin", with(darwin, File{Path: darwin.Files[0].Path, Body: "<plist><dict><key>Sockets</key><dict><key>Label</key><string>com.example.app</string></dict></dict></plist>"}), false, nil},
		row{"a plist that is not XML", "darwin", with(darwin, File{Path: darwin.Files[0].Path, Body: "<plist><dict>"}), false, nil},
		row{"a plist marked UTF16", "darwin", with(darwin, File{Path: darwin.Files[0].Path, Body: darwin.Files[0].Body, UTF16: true}), false, nil},
		row{"a plist with a DOCTYPE internal subset", "darwin", with(darwin, File{Path: darwin.Files[0].Path, Body: strings.Replace(darwin.Files[0].Body, "<plist ", `<!DOCTYPE plist [ <!ENTITY x "y"> ]><plist `, 1)}), false, nil},
		row{"a unit file holding a NUL", "linux", with(plans(dir)["linux"], File{Path: filepath.Join(dir, "app.service"), Body: "[Service]\nExecStart=/usr/bin/app\x00ExecStart=/bin/evil\n"}), false, nil},
		row{"a unit file holding a carriage return with no newline after it", "linux", with(plans(dir)["linux"], File{Path: filepath.Join(dir, "app.service"), Body: "[Service]\nExecStart=/usr/bin/app\rExecStart=/bin/evil\r\n"}), false, nil},
		row{"a unit file marked UTF16, whose bytes hold NULs", "linux", with(plans(dir)["linux"], File{Path: filepath.Join(dir, "app.service"), Body: "[Service]\nExecStart=/usr/bin/app\n", UTF16: true}), false, nil},
	)
	for _, r := range rows {
		ran := 0
		var run Runner = func(context.Context, []string) ([]byte, int, error) { ran++; return nil, 0, nil }
		if r.nilRun {
			run = nil
		}
		h := hostOf(r.goos, dir)
		if r.folders != nil {
			h.folders = r.folders
		}
		errs := []error{install(t.Context(), h, r.p, run), remove(t.Context(), h, r.p, run)}
		_, err := query(t.Context(), h, r.p, run)
		errs = append(errs, err)
		for i, err := range errs {
			if !errors.Is(err, ErrUsage) {
				t.Errorf("%s on %s: call %d gave %v, want ErrUsage", r.claim, r.goos, i, err)
			}
		}
		if ran != 0 {
			t.Errorf("%s on %s: the tool ran %d times", r.claim, r.goos, ran)
		}
	}
	for _, d := range []string{dir, other} {
		if left, err := os.ReadDir(d); err != nil || len(left) != 0 {
			t.Errorf("a refused plan wrote into %s: %v (%v)", d, left, err)
		}
	}
	taken := []string{"a", "app", "app-sweep", "com.example.app", "a.b_c-9", strings.Repeat("a", nameMax),
		"app.v2", "console", "com10", "com", "a.nul"}
	for _, name := range taken {
		for _, goos := range systems {
			if err := named(goos, name).check(hostOf(goos, dir), func(context.Context, []string) ([]byte, int, error) { return nil, 0, nil }); err != nil {
				t.Errorf("the name %q on %s: %v, want it taken", name, goos, err)
			}
		}
	}
	// A unit file with CRLF line ends, and a plist with Apple's own DOCTYPE, which has no internal
	// subset, are taken.
	crlf := with(plans(dir)["linux"], File{Path: filepath.Join(dir, "app.service"), Body: "[Service]\r\nExecStart=/usr/bin/app\r\n"})
	apple := with(darwin, File{Path: darwin.Files[0].Path, Body: strings.Replace(darwin.Files[0].Body, "<plist ", `<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist `, 1)})
	for goos, p := range map[string]Plan{"linux": crlf, "darwin": apple} {
		if err := p.check(hostOf(goos, dir), func(context.Context, []string) ([]byte, int, error) { return nil, 0, nil }); err != nil {
			t.Errorf("the %s plan %s: %v, want it taken", goos, p.Files[0].Body, err)
		}
	}
	if len(rows) != 3*len(names)+19 || len(names) != 35 || len(taken) != 11 {
		t.Fatalf("%d rows of %d names, and %d names taken", len(rows), len(names), len(taken))
	}
}

// TestEveryCallOnASystemWithNoToolIsErrAbsent: on FreeBSD, OpenBSD, plan 9 and an empty system,
// install, remove and query return ErrAbsent, which is errors.ErrUnsupported, write nothing and run
// nothing; on such a system the exported calls do too.
//
// What it costs when this fails: a program on FreeBSD reports a unit as installed that nothing
// holds, or cannot tell "not here" from a failure.
func TestEveryCallOnASystemWithNoToolIsErrAbsent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := Plan{Name: "app", Files: []File{{Path: filepath.Join(dir, "app"), Body: "x"}}}
	ran := 0
	run := func(context.Context, []string) ([]byte, int, error) { ran++; return nil, 0, nil }
	systems := []string{"freebsd", "openbsd", "plan9", ""}
	for _, goos := range systems {
		_, qerr := query(t.Context(), host{goos: goos}, p, run)
		for i, err := range []error{install(t.Context(), host{goos: goos}, p, run), remove(t.Context(), host{goos: goos}, p, run), qerr} {
			if !errors.Is(err, ErrAbsent) || !errors.Is(err, errors.ErrUnsupported) {
				t.Errorf("%q: call %d gave %v, want ErrAbsent", goos, i, err)
			}
		}
	}
	if _, err := os.Stat(p.Files[0].Path); ran != 0 || !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the tool ran %d times, or the file was written (%v)", ran, err)
	}
	if _, err := Query(t.Context(), p, run); hasTool(here().goos) != !errors.Is(err, ErrAbsent) {
		t.Errorf("Query on %s gave %v", here().goos, err)
	}
	if len(systems) != 4 {
		t.Fatalf("%d systems", len(systems))
	}
}

// TestEveryCallWhoseFoldersCouldNotBeFoundReturnsWhyAndRunsNothing: on Linux, macOS and Windows, a
// host whose folders could not be found makes install, remove and query return that error, write
// nothing and run nothing; and the folders of each system are the ones Folders names, which on
// Windows are none, an empty list and no error.
//
// What it costs when this fails: a program with no home folder gets "not in a folder the package
// writes units to" for every plan, and never learns that its home could not be found.
func TestEveryCallWhoseFoldersCouldNotBeFoundReturnsWhyAndRunsNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	lost := errors.New("no home")
	ran := 0
	run := func(context.Context, []string) ([]byte, int, error) { ran++; return nil, 0, nil }
	systems := []string{"linux", "darwin", "windows"}
	for _, goos := range systems {
		h := host{goos: goos, uid: 501, err: lost}
		p := plans(dir)[goos]
		_, qerr := query(t.Context(), h, p, run)
		for i, err := range []error{install(t.Context(), h, p, run), remove(t.Context(), h, p, run), qerr} {
			if !errors.Is(err, lost) {
				t.Errorf("%s: call %d gave %v, want the folders' error", goos, i, err)
			}
		}
	}
	if left, err := os.ReadDir(dir); ran != 0 || err != nil || len(left) != 0 {
		t.Errorf("the tool ran %d times, or %s holds %v (%v)", ran, dir, left, err)
	}
	want := map[string][]string{
		"linux":   {filepath.Join("systemd", "user"), filepath.Join("/run/user", "1000", "systemd", "user")},
		"darwin":  {filepath.Join("Library", "LaunchAgents")},
		"windows": {},
	}
	for _, goos := range systems {
		got, err := foldersOf(goos, 1000)
		if len(want[goos]) == 0 {
			if err != nil || got == nil || len(got) != 0 {
				t.Errorf("the folders of %s are %q (nil: %t, %v), want an empty list and no error", goos, got, got == nil, err)
			}
			continue
		}
		if err != nil || len(got) != len(want[goos]) || !strings.HasSuffix(got[0], string(filepath.Separator)+want[goos][0]) || !filepath.IsAbs(got[0]) {
			t.Errorf("the folders of %s are %q (%v), want an absolute folder ending in %s", goos, got, err, want[goos][0])
			continue
		}
		if len(got) == 2 && got[1] != want[goos][1] {
			t.Errorf("the second folder of %s is %s, want %s", goos, got[1], want[goos][1])
		}
	}
	if _, err := foldersOf("freebsd", 1000); !errors.Is(err, ErrAbsent) {
		t.Errorf("the folders of freebsd gave %v, want ErrAbsent", err)
	}
	if len(systems) != 3 || len(want) != 3 {
		t.Fatalf("%d systems", len(systems))
	}
}

// TestABodyMarkedUTF16IsWrittenAsTheCheckedInFile: File.Bytes of the checked-in task text, marked
// UTF16, is the checked-in UTF-16 file byte for byte, its byte order mark and a surrogate pair among
// them; unmarked, it is the body's own bytes.
//
// What it costs when this fails: Task Scheduler refuses the task, since its declaration says UTF-16
// and its bytes are not.
func TestABodyMarkedUTF16IsWrittenAsTheCheckedInFile(t *testing.T) {
	t.Parallel()
	text, err := os.ReadFile(filepath.Join("testdata", "task.xml"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "task.xml.utf16"))
	if err != nil {
		t.Fatal(err)
	}
	if got := (File{Body: string(text), UTF16: true}).Bytes(); !bytes.Equal(got, want) {
		t.Errorf("Bytes gave %d bytes starting % x, want the %d of task.xml.utf16 starting % x", len(got), got[:min(8, len(got))], len(want), want[:8])
	}
	if got := (File{Body: string(text)}).Bytes(); !bytes.Equal(got, text) {
		t.Errorf("Bytes of an unmarked body changed it")
	}
}

// TestQuoteGivesEachSystemsFormOfOneArgument: Quote on each system, for a plain word, an empty one,
// one with a space, a quote and a percent sign, and the characters each system reads.
//
// What it costs when this fails: a path with a space becomes two arguments, a % in a unit is read as
// a specifier, or a & breaks the plist or the task's XML.
func TestQuoteGivesEachSystemsFormOfOneArgument(t *testing.T) {
	t.Parallel()
	rows := map[string][][2]string{
		"linux": {
			{"app", "app"},
			{"", `""`},
			{`a b"c%d`, `"a b\"c%%d"`},
			{"100%", "100%%"},
			{"$HOME", "$$HOME"},
			{`back\slash`, `"back\\slash"`},
			{"it's", `"it's"`},
			{"two\nlines", `"two\nlines"`},
			{"a\rExecStart=/bin/evil", `"a\rExecStart=/bin/evil"`},
			{"x y\rExecStart=/bin/evil\r", `"x y\rExecStart=/bin/evil\r"`},
			{";", `";"`},
		},
		"darwin": {
			{"app", "app"},
			{"", ""},
			{`a b"c%d`, "a b&#34;c%d"},
			{"<&>", "&lt;&amp;&gt;"},
		},
		"windows": {
			{"app", "app"},
			{"", "&#34;&#34;"},
			{`a b"c%d`, `&#34;a b\&#34;c%d&#34;`},
			{`C:\Program Files\app\`, `&#34;C:\Program Files\app\\&#34;`},
			{`a\"b`, `&#34;a\\\&#34;b&#34;`},
			{`C:\app\x`, `C:\app\x`},
			{"&", "&amp;"},
		},
		"freebsd": {
			{`a b"c%d`, `a b"c%d`},
		},
	}
	checked := 0
	for goos, table := range rows {
		for _, r := range table {
			checked++
			if got := Quote(goos, r[0]); got != r[1] {
				t.Errorf("Quote(%s, %q) = %s, want %s", goos, r[0], got, r[1])
			}
		}
	}
	if checked != 23 {
		t.Fatalf("checked %d rows, want 23", checked)
	}
}

// TestTheLiveLinuxPlanLoadsOnTheFakeSystemd: the live test's Linux timer and service, installed on
// the fake systemd, which refuses a calendar year past 2199 as systemd 257 does, are installed and
// enabled, and removed after.
//
// What it costs when this fails: the live test's timer is one systemd refuses, so the Linux run is
// red for its fixture and not for the package, and only a native run finds it.
func TestTheLiveLinuxPlanLoadsOnTheFakeSystemd(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := liveLinuxPlan(dir, "app-0123456789ab")
	sys := &system{goos: "linux"}
	for _, f := range p.Files {
		sys.files = append(sys.files, f.Path)
	}
	if err := install(t.Context(), hostOf("linux", dir), p, sys.run); err != nil {
		t.Fatalf("the live plan's install on the fake systemd: %v", err)
	}
	if st, err := query(t.Context(), hostOf("linux", dir), p, sys.run); err != nil || !st.Installed || !st.Enabled {
		t.Errorf("after the install the fake systemd answers %+v, %v; want installed and enabled", st, err)
	}
	if err := remove(t.Context(), hostOf("linux", dir), p, sys.run); err != nil {
		t.Errorf("the live plan's removal: %v", err)
	}
	if len(sys.files) != 2 {
		t.Fatalf("the live plan has %d files, want its timer and service", len(sys.files))
	}
}

// TestARemovalDropsAUnitTheToolRefusedAndLeavesNoneOfItsFiles: a timer systemd refuses (a calendar
// year past 2199), whose install failed after the enable made its symlink, is disabled and not
// stopped by Remove, its files are removed, and Remove succeeds with systemd answering not-found;
// and a Windows removal whose delete exits 5 returns the exit.
//
// What it costs when this fails: a unit systemd refused can be neither started nor removed through
// the package, so its files and its enable symlink, which outlives a logout, stay behind.
func TestARemovalDropsAUnitTheToolRefusedAndLeavesNoneOfItsFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := Plan{Name: "app.timer", Files: []File{
		{Path: filepath.Join(dir, "app.service"), Body: "[Service]\nExecStart=/usr/bin/app\n"},
		{Path: filepath.Join(dir, "app.timer"), Body: "[Timer]\nOnCalendar=2999-01-01\n"},
	}}
	sys := &system{goos: "linux", files: []string{p.Files[0].Path, p.Files[1].Path}}
	if err := install(t.Context(), hostOf("linux", dir), p, sys.run); err == nil || !strings.Contains(err.Error(), "exited 1") {
		t.Fatalf("the install of a timer systemd refuses: %v, want the enable's exit 1", err)
	}
	if !sys.enabled || sys.load != "bad-setting" {
		t.Fatalf("the fake systemd is enabled: %t, LoadState %q; want the symlink made and bad-setting", sys.enabled, sys.load)
	}
	sys.ran = nil
	if err := remove(t.Context(), hostOf("linux", dir), p, sys.run); err != nil {
		t.Errorf("the removal of a timer systemd refused: %v", err)
	}
	want := [][]string{
		{"systemctl", "--user", "show", "-p", "LoadState,UnitFileState,ActiveState", "app.timer"},
		{"systemctl", "--user", "disable", "app.timer"},
		{"systemctl", "--user", "daemon-reload"},
		{"systemctl", "--user", "show", "-p", "LoadState,UnitFileState,ActiveState", "app.timer"},
	}
	if !slices.EqualFunc(sys.ran, want, slices.Equal) {
		t.Errorf("the removal ran\n%q\nwant\n%q", sys.ran, want)
	}
	if sys.enabled {
		t.Errorf("the enable symlink of the refused timer is left")
	}
	for _, f := range p.Files {
		if _, err := os.Stat(f.Path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is left after the removal (%v)", f.Path, err)
		}
	}

	task := plans(t.TempDir())["windows"]
	refused := func(_ context.Context, argv []string) ([]byte, int, error) {
		if argv[1] == "/delete" {
			return []byte("ERROR: Access is denied."), 5, nil
		}
		return []byte("<Task/>"), 0, nil
	}
	if err := remove(t.Context(), hostOf("windows"), task, refused); err == nil || !strings.Contains(err.Error(), "exited 5") {
		t.Errorf("a removal whose delete exits 5: %v, want the exit", err)
	}
}

// TestAWindowsInstallKeepsNoFileOfTheTask: on Windows an install gives schtasks the task's bytes in
// task.xml, in a folder of its own in the system's temp folder, and that folder is gone after the
// install when schtasks takes the task, when it exits 5, and when it could not be run; a task.xml
// made in the folder before the install writes its own fails the install with an error that names
// it, before schtasks runs, and the folder is gone then too; and no install makes a go-proc folder
// in the user's cache or configuration folder.
//
// What it costs when this fails: a task's XML, its command line and arguments among them, is left
// on disk after the install for any program of the user's to read or change; or the install writes
// into a task.xml another account made in the folder, which that account can rewrite before
// schtasks reads it, so schtasks takes a task no check of the package read; or a folder the package
// names of its own, which no tool reads, comes back.
func TestAWindowsInstallKeepsNoFileOfTheTask(t *testing.T) {
	t.Parallel()
	p := plans(t.TempDir())["windows"]
	rows := []struct {
		claim   string
		answer  func() ([]byte, int, error) // what /create answers in place of the fake's own; nil for the fake's
		planted bool                        // a task.xml is made in the folder before the install writes its own
		fails   bool
	}{
		{"schtasks takes the task", nil, false, false},
		{"schtasks exits 5", func() ([]byte, int, error) { return []byte("ERROR: Access is denied."), 5, nil }, false, true},
		{"schtasks could not be run", func() ([]byte, int, error) { return nil, 0, errors.New("schtasks is not on the path") }, false, true},
		{"a task.xml is in the folder before the write", nil, true, true},
	}
	for _, r := range rows {
		sys := &system{goos: "windows"}
		var given string
		var read []byte
		ran := 0
		run := func(ctx context.Context, argv []string) ([]byte, int, error) {
			ran++
			if argv[1] == "/create" {
				given = argv[5]
				read, _ = os.ReadFile(given)
				if r.answer != nil {
					return r.answer()
				}
			}
			return sys.run(ctx, argv)
		}
		h := hostOf("windows")
		var made string
		if r.planted {
			h.staged = func(dir string) {
				made = filepath.Join(dir, "task.xml")
				if err := os.WriteFile(made, []byte("<Task/>"), 0o600); err != nil {
					t.Errorf("%s: %v", r.claim, err)
				}
			}
		}
		err := install(t.Context(), h, p, run)
		if (err != nil) != r.fails {
			t.Errorf("%s: the install gave %v, want an error: %t", r.claim, err, r.fails)
		}
		if r.planted {
			if made == "" || ran != 0 || !errors.Is(err, fs.ErrExist) || !strings.Contains(err.Error(), made) {
				t.Errorf("%s: the install ran %d commands and gave %v, want none run and an error that is fs.ErrExist naming the planted %q", r.claim, ran, err, made)
				continue
			}
			given = made
		} else if !taskXML(given) || !bytes.Equal(read, p.Files[0].Bytes()) {
			t.Errorf("%s: schtasks was given %q holding %q, want task.xml in a task- folder of %s holding %q", r.claim, given, read, os.TempDir(), p.Files[0].Bytes())
			continue
		}
		if _, err := os.Stat(filepath.Dir(given)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: the task's folder %s is left after the install (%v)", r.claim, filepath.Dir(given), err)
		}
	}
	// The user's cache and configuration folders as each system's os reads them, beneath the
	// folders testkit.Main gave the run: v0.3.0 named go-proc\service under LOCALAPPDATA.
	var users []string
	for _, name := range []string{"LOCALAPPDATA", "APPDATA", "XDG_CACHE_HOME", "XDG_CONFIG_HOME"} {
		if v, ok := os.LookupEnv(name); ok && v != "" {
			users = append(users, v)
		}
	}
	if home, ok := os.LookupEnv("HOME"); ok && home != "" {
		users = append(users, filepath.Join(home, "Library", "Caches"), filepath.Join(home, "Library", "Application Support"), filepath.Join(home, ".cache"), filepath.Join(home, ".config"))
	}
	if len(users) < 2 {
		t.Fatalf("testkit.Main gave the run the user folders %q, want the cache and configuration folders", users)
	}
	for _, d := range users {
		if _, err := os.Stat(filepath.Join(d, "go-proc")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is made (%v), and the package names no folder of its own", filepath.Join(d, "go-proc"), err)
		}
	}
	if len(rows) != 4 {
		t.Fatalf("%d rows", len(rows))
	}
}

// TestAWindowsInstallWithAPercentWhereTaskSchedulerExpandsOneIsRefused: an install on Windows whose
// task's Command, Arguments or WorkingDirectory hold a %, written as it is or as a character
// reference, is ErrPercent, before schtasks runs; a % elsewhere in the
// task is taken; a body that is not XML, whose root is not a Task, that has a DOCTYPE internal subset,
// which Go reads past to its last >, or that declares an encoding other than UTF-8 or UTF-16 (UTF-7
// among them, whose +ACU- is a %), is refused, and not as ErrPercent; and Remove and Query of
// such a plan still run.
//
// What it costs when this fails: Task Scheduler expands %TEMP% in a task's arguments, as a native
// run saw, so the program gets another argument than the caller wrote, and nothing says so; or a
// body in another encoding hides a % from the check that Task Scheduler then reads.
func TestAWindowsInstallWithAPercentWhereTaskSchedulerExpandsOneIsRefused(t *testing.T) {
	t.Parallel()
	task := func(args string) string {
		return `<?xml version="1.0" encoding="UTF-16"?><Task xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task"><RegistrationInfo><Description>100% sure</Description></RegistrationInfo><Actions><Exec><Command>app.exe</Command><Arguments>` + args + `</Arguments></Exec></Actions></Task>`
	}
	rows := []struct {
		claim, body string
		percent     bool
		fails       bool
	}{
		{"a variable", task("--dir " + Quote("windows", "%TEMP%")), true, true},
		{"a percent sign", task(Quote("windows", `a b"c%d`)), true, true},
		{"a character reference", task("--x &#37;TEMP&#37;"), true, true},
		{"a percent in the description alone", task("--dir " + Quote("windows", `C:\app data`)), false, false},
		{"no XML", "<Task><Arguments>x</Task>", false, true},
		{"a variable in the command", strings.Replace(task("/c exit 0"), "<Command>app.exe", `<Command>%SystemRoot%\system32\cmd.exe`, 1), true, true},
		{"a variable in the working folder", strings.Replace(task("x"), "</Exec>", "<WorkingDirectory>%TEMP%</WorkingDirectory></Exec>", 1), true, true},
		{"a UTF-7 declaration", strings.Replace(task("+ACU-TEMP+ACU-"), "UTF-16", "UTF-7", 1), false, true},
		{"a Latin-1 declaration", strings.Replace(task("x"), "UTF-16", "ISO-8859-1", 1), false, true},
		{"a UTF-8 declaration", strings.Replace(task("x"), "UTF-16", "UTF-8", 1), false, false},
		{"no declaration", strings.Replace(task("x"), `<?xml version="1.0" encoding="UTF-16"?>`, "", 1), false, false},
		// Go reads from <!DOCTYPE to the last '>' as one directive, by its quotes alone, and so sees no
		// element and no % in this body.
		{"a DOCTYPE internal subset hiding the task", strings.Replace(task(Quote("windows", "/c echo %TEMP%")), "?>", "?><!DOCTYPE Task [ <?p ' ?> ]>", 1) + "<!-- '>> -->", false, true},
		{"a DOCTYPE internal subset before a Task", strings.Replace(task("x"), "?>", `?><!DOCTYPE Task [ <!ENTITY x "y"> ]>`, 1), false, true},
		{"a DOCTYPE with no internal subset", strings.Replace(task("x"), "?>", "?><!DOCTYPE Task>", 1), false, false},
		{"a root that is not a Task", `<?xml version="1.0"?><Actions><Exec><Command>app.exe</Command></Exec></Actions>`, false, true},
		{"no element", `<?xml version="1.0"?>`, false, true},
	}
	for _, r := range rows {
		p := Plan{Name: "app", Files: []File{{Body: r.body, UTF16: true}}}
		sys := &system{goos: "windows"}
		err := install(t.Context(), hostOf("windows"), p, sys.run)
		if (err != nil) != r.fails || errors.Is(err, ErrPercent) != r.percent {
			t.Errorf("%s: %v; want an error: %t, ErrPercent: %t", r.claim, err, r.fails, r.percent)
		}
		if r.fails && (len(sys.ran) != 0 || sys.task != nil) {
			t.Errorf("%s: schtasks ran %q, and read %q", r.claim, sys.ran, sys.task)
		}
		if !r.percent {
			continue
		}
		if _, err := query(t.Context(), hostOf("windows"), p, sys.run); err != nil {
			t.Errorf("%s: the query of the refused plan: %v", r.claim, err)
		}
		if err := remove(t.Context(), hostOf("windows"), p, sys.run); err != nil {
			t.Errorf("%s: the removal of the refused plan: %v", r.claim, err)
		}
	}
	if len(rows) != 16 {
		t.Fatalf("%d rows", len(rows))
	}
}

// enableLinks gives a test the means to make the enable links systemctl makes, and the Linux host
// that reads them, for the folder a test names.
//
// The links are symbolic links, read by the host's own reader, os.Readlink, wherever the system
// makes one. Where it refuses for want of the privilege, as Windows does a run without it
// (ERROR_PRIVILEGE_NOT_HELD, 1314, which no other system's errno takes), they are planted as files
// and read through the host's test reader: a planted path reads as its target while it is on disk,
// and any other path fails as os.Readlink fails on a file that is no link.
func enableLinks(t *testing.T) (link func(to, at string) string, linux func(dir string) host) {
	t.Helper()
	probe := os.Symlink("app.timer", filepath.Join(t.TempDir(), "probe"))
	planted := errors.Is(probe, syscall.Errno(1314))
	if probe != nil && !planted {
		t.Fatal(probe)
	}
	targets := map[string]string{}
	readlink := func(at string) (string, error) {
		if _, err := os.Lstat(at); err != nil {
			return "", err
		}
		if to, ok := targets[at]; ok {
			return to, nil
		}
		return "", &fs.PathError{Op: "readlink", Path: at, Err: fs.ErrInvalid}
	}
	linux = func(dir string) host {
		h := hostOf("linux", dir)
		if planted {
			h.readlink = readlink
		}
		return h
	}
	link = func(to, at string) string {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(at), 0o700); err != nil {
			t.Fatal(err)
		}
		if !planted {
			if err := os.Symlink(to, at); err != nil {
				t.Fatal(err)
			}
			return at
		}
		if err := os.WriteFile(at, []byte(to), 0o600); err != nil {
			t.Fatal(err)
		}
		targets[at] = to
		return at
	}
	return link, linux
}

// TestALinuxRemovalWhoseDisableDidNotRunRemovesTheEnableLinksOfItsUnitAlone: when systemctl disable
// fails, Remove removes the links its [Install] section names in the user's systemd folder (a
// WantedBy, RequiredBy and Alias link to a file of the unit's name), returns the disable's failure and
// names the links; it leaves a file that is no link, a link to another unit, a link no key names, and
// a link a target that is not a name would reach outside the folder. A unit systemd no longer knows
// has its leftover link removed and Remove succeeds; and after a disable that succeeded the package
// removes no link, since systemctl has.
//
// What it costs when this fails: a unit whose disable failed loses its file and keeps its enable link,
// which outlives a logout, and no later Remove can reach it, since systemd then answers not-found and
// the disable is skipped; or Remove deletes a link or a file that is not the plan's.
func TestALinuxRemovalWhoseDisableDidNotRunRemovesTheEnableLinksOfItsUnitAlone(t *testing.T) {
	t.Parallel()
	timer := "[Timer]\nOnCalendar=daily\n\n[Install]\nWantedBy=timers.target default.target\nRequiredBy=x.target\nAlias=app-alias.timer\nWantedBy=../outside\n"
	plan := func(dir string) Plan {
		return Plan{Name: "app.timer", Files: []File{
			{Path: filepath.Join(dir, "app.service"), Body: "[Service]\nExecStart=/usr/bin/app\n"},
			{Path: filepath.Join(dir, "app.timer"), Body: timer},
		}}
	}
	link, linux := enableLinks(t)
	there := func(path string) bool { _, err := os.Lstat(path); return err == nil }

	dir := t.TempDir()
	p := plan(dir)
	sys := &system{goos: "linux", files: []string{p.Files[0].Path, p.Files[1].Path}}
	if err := install(t.Context(), linux(dir), p, sys.run); err != nil {
		t.Fatal(err)
	}
	gone := []string{
		link("/run/user/1000/systemd/user/app.timer", filepath.Join(dir, "timers.target.wants", "app.timer")),
		link("app.timer", filepath.Join(dir, "app-alias.timer")),
	}
	notLink := filepath.Join(dir, "default.target.wants", "app.timer")
	if err := os.MkdirAll(filepath.Dir(notLink), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(notLink, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	kept := []string{
		notLink,
		link("/elsewhere/other.timer", filepath.Join(dir, "x.target.requires", "app.timer")),
		link("other.timer", filepath.Join(dir, "timers.target.wants", "other.timer")),
		link(p.Files[1].Path, filepath.Join(filepath.Dir(dir), "outside.wants", "app.timer")),
	}
	sys.fail = map[string]int{"disable": 1}
	err := remove(t.Context(), linux(dir), p, sys.run)
	if err == nil || !strings.Contains(err.Error(), "exited 1") || !strings.Contains(err.Error(), "removed the enable links") {
		t.Errorf("a removal whose disable exits 1: %v, want the exit and the links the package removed", err)
	}
	for _, l := range gone {
		if there(l) {
			t.Errorf("the enable link %s is left after a disable that failed", l)
		}
	}
	for _, l := range kept {
		if !there(l) {
			t.Errorf("%s, which is not a link of the plan's unit in the folder, was removed", l)
		}
	}
	for _, f := range p.Files {
		if there(f.Path) {
			t.Errorf("%s is left after the removal", f.Path)
		}
	}

	stale := t.TempDir()
	sp := plan(stale)
	left := link(sp.Files[1].Path, filepath.Join(stale, "timers.target.wants", "app.timer"))
	ssys := &system{goos: "linux", files: []string{sp.Files[0].Path, sp.Files[1].Path}}
	if err := remove(t.Context(), linux(stale), sp, ssys.run); err != nil {
		t.Errorf("the removal of a unit systemd does not know, with a link left: %v", err)
	}
	if there(left) {
		t.Errorf("the link %s of a unit systemd does not know is left", left)
	}
	for _, argv := range ssys.ran {
		if argv[2] == "disable" || argv[2] == "stop" {
			t.Errorf("the removal of a unit systemd does not know ran %q", argv)
		}
	}

	done := t.TempDir()
	dp := plan(done)
	dsys := &system{goos: "linux", files: []string{dp.Files[0].Path, dp.Files[1].Path}}
	if err := install(t.Context(), linux(done), dp, dsys.run); err != nil {
		t.Fatal(err)
	}
	own := link(dp.Files[1].Path, filepath.Join(done, "timers.target.wants", "app.timer"))
	if err := remove(t.Context(), linux(done), dp, dsys.run); err != nil {
		t.Errorf("a removal whose disable succeeds: %v", err)
	}
	if !there(own) {
		t.Errorf("after a disable that succeeded, the package removed the link %s itself", own)
	}
	if len(gone) != 2 || len(kept) != 4 {
		t.Fatalf("%d links gone and %d kept", len(gone), len(kept))
	}
}

// TestAMacOSPlistOnDiskWithAnotherLabelIsBootedOutByItsPath: a plist at the plan's path whose Label is
// not the plan's name, left by another install, makes Install fail before it writes or runs anything;
// Remove boots that Label out by the plist's path and then the plan's name, and removes the plist; a
// bootout by path that fails is returned and the plist kept; and a plist on disk whose Label is no name
// the package takes, or cannot be read, makes Remove fail before it runs anything, the plist kept.
//
// What it costs when this fails: launchctl bootstraps a plist's own Label, so a stale plist's job
// stays loaded after Remove deleted its plist, where neither the package nor the person can find it by
// name; or an install overwrites the plist of a loaded job and loses it the same way.
func TestAMacOSPlistOnDiskWithAnotherLabelIsBootedOutByItsPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := plans(dir)["darwin"]
	path := p.Files[0].Path
	plant := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h := hostOf("darwin", dir)

	plant(plist("com.example.old"))
	sys := &system{goos: "darwin", labels: []string{"com.example.old"}}
	if err := install(t.Context(), h, p, sys.run); err == nil {
		t.Errorf("an install over a plist on disk whose Label is com.example.old was taken")
	}
	if len(sys.ran) != 0 || label(path) != "com.example.old" {
		t.Errorf("the refused install ran %q, or rewrote the plist to the Label %q", sys.ran, label(path))
	}

	sys = &system{goos: "darwin", labels: []string{"com.example.old"}, fail: map[string]int{"bootout": 5}}
	if err := remove(t.Context(), h, p, sys.run); err == nil || !strings.Contains(err.Error(), "exited 5") {
		t.Errorf("a removal whose bootout by path exits 5: %v, want the exit", err)
	}
	if label(path) != "com.example.old" {
		t.Errorf("the plist whose bootout failed is not kept")
	}

	sys = &system{goos: "darwin", labels: []string{"com.example.old", "com.example.app"}}
	if err := remove(t.Context(), h, p, sys.run); err != nil {
		t.Errorf("the removal over a plist whose Label is com.example.old: %v", err)
	}
	want := [][]string{
		{"launchctl", "print", "gui/501/com.example.old"},
		{"launchctl", "bootout", "gui/501", path},
		{"launchctl", "print", "gui/501/com.example.app"},
		{"launchctl", "bootout", "gui/501/com.example.app"},
		{"launchctl", "print", "gui/501/com.example.app"},
	}
	if !slices.EqualFunc(sys.ran, want, slices.Equal) {
		t.Errorf("the removal ran\n%q\nwant\n%q", sys.ran, want)
	}
	if _, err := os.Stat(path); len(sys.labels) != 0 || !errors.Is(err, os.ErrNotExist) {
		t.Errorf("after the removal launchd holds %q and the plist is there: %v", sys.labels, err)
	}

	for _, body := range []string{plist("gui/0/com.example.app"), plist("-x"), "<plist><dict>"} {
		plant(body)
		sys = &system{goos: "darwin"}
		if err := remove(t.Context(), h, p, sys.run); err == nil || len(sys.ran) != 0 {
			t.Errorf("a removal over the plist %q: %v, and ran %q; want an error and nothing run", body, err, sys.ran)
		}
		if b, err := os.ReadFile(path); err != nil || string(b) != body {
			t.Errorf("the plist %q is not kept (%v)", body, err)
		}
	}
}
