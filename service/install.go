package service

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Install writes the plan's files and has the system's tool take the unit, then asks the tool and
// returns an error unless the unit is installed and enabled. Installing again what is installed
// leaves it as one install does: on macOS a label whose plist is unchanged is left alone, and one
// whose plist changed is booted out and bootstrapped again. When that bootout or bootstrap fails, the
// plist that was on disk before is put back, or the new one removed when there was none, so an
// Install again sees the change and runs them again. On Windows the task's XML is written to a new
// folder in the system's temp folder, read there by schtasks, and the folder removed whether or not
// schtasks took the task.
func Install(ctx context.Context, p Plan, run Runner) error {
	return install(ctx, here(), p, run)
}

// Remove has the system's tool drop the unit when it holds it, in whatever state it holds it,
// removes the plan's files, then asks the tool and returns an error if it still holds the unit. On
// Windows there is no file to remove. On Linux the files are removed even when the tool fails to
// drop the unit, and that failure is returned; a unit systemctl did not disable has the enable links its [Install] section
// names removed by the package. On macOS a plist on disk is booted out by its path, so the Label it
// holds is dropped whatever it is, and a plist whose bootout failed is kept, so a removal again can
// boot out what it loaded. Removing what is not installed succeeds.
func Remove(ctx context.Context, p Plan, run Runner) error {
	return remove(ctx, here(), p, run)
}

// Query is what the system's tool answers about the unit, read from its machine answers alone:
// systemctl show's properties, schtasks' exit code and the task's XML, and launchctl print's exit
// code.
func Query(ctx context.Context, p Plan, run Runner) (State, error) {
	return query(ctx, here(), p, run)
}

func install(ctx context.Context, h host, p Plan, run Runner) error {
	t, err := toolOf(h, p, run)
	if err != nil {
		return err
	}
	switch h.goos {
	case "windows":
		if err := percentFree(p.Files[0].Body); err != nil {
			return err
		}
		err = createTask(ctx, h, p, run)
	case "darwin":
		old, err := samePlist(p) // the plist on disk before the write; nil when there was none
		if err != nil {
			return err
		}
		changed, err := write(p.Files)
		if err != nil {
			return err
		}
		if err = bootstrap(ctx, t, changed, run); err != nil && changed {
			err = errors.Join(err, putBack(p.Files[0].Path, old))
		}
		if err != nil {
			return err
		}
	default:
		if _, err := write(p.Files); err != nil {
			return err
		}
		err = mustAll(ctx, run, t.reload, t.install)
	}
	if err != nil {
		return err
	}
	now, err := ask(ctx, h.goos, t, run)
	if err != nil {
		return err
	}
	if !now.Installed || !now.Enabled {
		return fmt.Errorf("%s took %s, and then answers that it is installed: %t, enabled: %t", t.query[0], p.Name, now.Installed, now.Enabled)
	}
	return nil
}

// createTask has schtasks take the task from its XML, written to a folder made for it alone in the
// system's temp folder and removed on every way out, so the package keeps no file of a task. On
// Windows the folder is the user's alone from its making, its descriptor read back, and it is held
// by a handle that shares no delete from then until it is removed by that handle; task.xml is made
// by it with the folder's descriptor, and schtasks is given the path the handle names, through no
// junction. So no other account the temp folder grants may write, remove or make a file in it, or
// rename it away and put another at its path, before schtasks reads it (makeTaskFolder). The file is
// still made new, never one already there: such a file fails the install.
func createTask(ctx context.Context, h host, p Plan, run Runner) (err error) {
	f, err := makeTaskFolder(h)
	if err != nil {
		return fmt.Errorf("a folder for the task's XML could not be made: %w", err)
	}
	defer func() {
		if rerr := f.remove(); rerr != nil {
			err = errors.Join(err, fmt.Errorf("the task's temp folder %s is left: %w", f.dir, rerr))
		}
	}()
	path := filepath.Join(f.dir, "task.xml")
	h.staging(f.dir)
	if err := f.write("task.xml", p.Files[0].Bytes()); err != nil {
		return fmt.Errorf("the task's XML could not be written to %s: %w", path, err)
	}
	return must(ctx, run, createArgv(p.Name, path))
}

// bootstrap has launchd take the plist: a label it does not hold is bootstrapped, a held one whose
// plist changed is booted out and bootstrapped again, and a held one whose plist is unchanged is
// left alone.
func bootstrap(ctx context.Context, t tool, changed bool, run Runner) error {
	was, err := ask(ctx, "darwin", t, run)
	switch {
	case err != nil:
		return err
	case was.Installed && !changed:
		return nil
	case was.Installed:
		return mustAll(ctx, run, t.remove, t.install)
	}
	return must(ctx, run, t.install)
}

func remove(ctx context.Context, h host, p Plan, run Runner) error {
	t, err := toolOf(h, p, run)
	if err != nil {
		return err
	}
	var errs []error
	switch h.goos {
	case "darwin":
		if err := dropLaunchd(ctx, t, p, run); err != nil {
			return fmt.Errorf("%w; the plist %s is kept, so a removal again can boot out what it loaded", err, p.Files[0].Path)
		}
	case "linux":
		disabled, err := dropSystemd(ctx, t, run)
		if !disabled {
			gone, lerr := unlink(h.folders[0], p.Files, h.linkReader())
			if err != nil && len(gone) > 0 {
				err = fmt.Errorf("%w; the package removed the enable links %s itself", err, strings.Join(gone, ", "))
			}
			errs = append(errs, err, lerr)
		}
	default:
		errs = append(errs, dropTask(ctx, t, run))
	}
	// The files go and the tool reloads even when the drop fails, so a unit the tool refused to
	// load leaves nothing of the plan's behind. A Windows plan has no file on disk.
	for _, f := range p.Files {
		if h.goos == "windows" {
			break
		}
		if err := os.Remove(f.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("the unit's file %s could not be removed: %w", f.Path, err))
		}
	}
	errs = append(errs, mustAll(ctx, run, t.reload))
	if err := errors.Join(errs...); err != nil {
		return err
	}
	now, err := ask(ctx, h.goos, t, run)
	if err != nil {
		return err
	}
	if now.Installed {
		return fmt.Errorf("%s dropped %s, and then answers that it still holds it", t.remove[0], p.Name)
	}
	return nil
}

func query(ctx context.Context, h host, p Plan, run Runner) (State, error) {
	t, err := toolOf(h, p, run)
	if err != nil {
		return State{}, err
	}
	return ask(ctx, h.goos, t, run)
}

// toolOf is the plan's argv on h once the plan is checked, and ErrAbsent where there is no tool.
func toolOf(h host, p Plan, run Runner) (tool, error) {
	if h.goos != "linux" && h.goos != "darwin" && h.goos != "windows" {
		return tool{}, errAbsent
	}
	if h.err != nil {
		return tool{}, h.err
	}
	if err := p.check(h, run); err != nil {
		return tool{}, err
	}
	return toolFor(h.goos, h.uid, p), nil
}

func must(ctx context.Context, run Runner, argv []string) error {
	_, _, err := ran(ctx, run, argv, 0)
	return err
}

// ran runs argv, and returns what it printed and its exit code when it could be run and exited 0 or
// also, and an error otherwise.
func ran(ctx context.Context, run Runner, argv []string, also int) ([]byte, int, error) {
	out, code, err := run(ctx, argv)
	if err != nil {
		return nil, 0, fmt.Errorf("%s could not be run: %w", strings.Join(argv, " "), err)
	}
	if code != 0 && code != also {
		return nil, 0, fmt.Errorf("%s exited %d: %s", strings.Join(argv, " "), code, bytes.TrimSpace(out))
	}
	return out, code, nil
}

// mustAll is must of each argv in order that is not empty.
func mustAll(ctx context.Context, run Runner, argvs ...[]string) error {
	for _, argv := range argvs {
		if len(argv) == 0 {
			continue
		}
		if err := must(ctx, run, argv); err != nil {
			return err
		}
	}
	return nil
}

// dropTask has schtasks delete the task when it holds it.
func dropTask(ctx context.Context, t tool, run Runner) error {
	was, err := ask(ctx, "windows", t, run)
	if err != nil || !was.Installed {
		return err
	}
	return must(ctx, run, t.remove)
}

// dropSystemd stops the unit when it is active and disables it when systemd knows it, going by
// systemd's properties as they are: a unit whose file systemd refused (a LoadState of bad-setting or
// error) can still be enabled, and systemd stops only a unit that is active. It says whether a
// disable ran and exited 0.
func dropSystemd(ctx context.Context, t tool, run Runner) (bool, error) {
	out, _, err := ran(ctx, run, t.query, 0)
	if err != nil {
		return false, err
	}
	props, err := parseSystemd(out)
	if err != nil {
		return false, err
	}
	var stopErr error
	if active := props["ActiveState"]; active != "inactive" && active != "failed" {
		stopErr = must(ctx, run, t.stop)
	}
	if props["LoadState"] == "not-found" {
		return false, stopErr
	}
	if err := must(ctx, run, t.remove); err != nil {
		return false, errors.Join(stopErr, err)
	}
	return true, stopErr
}

// unlink removes each link systemctl enable makes in folder for the plan's units, as each unit's
// [Install] section names them, that is still there: a symbolic link to a file of the unit's name,
// and nothing else, as readlink reads it. It gives the links it removed.
func unlink(folder string, files []File, readlink func(string) (string, error)) ([]string, error) {
	var gone []string
	var errs []error
	for _, f := range files {
		unit := filepath.Base(f.Path)
		for _, link := range links(folder, unit, f.Body) {
			// Readlink fails on a path that is no symbolic link, so a file there is left.
			if to, err := readlink(link); err != nil || filepath.Base(to) != unit {
				continue
			}
			if err := os.Remove(link); err != nil {
				errs = append(errs, fmt.Errorf("the enable link %s could not be removed: %w", link, err))
				continue
			}
			gone = append(gone, link)
		}
	}
	return gone, errors.Join(errs...)
}

// linkSuffix is the folder suffix of an [Install] key whose targets systemctl enable links a unit
// into, and false for any other key.
func linkSuffix(key string) (string, bool) {
	switch key {
	case "WantedBy":
		return ".wants", true
	case "RequiredBy":
		return ".requires", true
	case "UpheldBy":
		return ".upholds", true
	}
	return "", false
}

// links are the paths in folder where systemctl enable links unit, from the [Install] section of its
// body: <target>.wants/<unit> for WantedBy, .requires for RequiredBy, .upholds for UpheldBy, and
// <alias> for Alias. A target or an alias that is not a name the package takes is left out.
func links(folder, unit, body string) []string {
	var out []string
	section := ""
	for line := range strings.Lines(body) {
		line = strings.TrimSpace(line)
		switch {
		case line == "" || line[0] == '#' || line[0] == ';':
			continue
		case line[0] == '[':
			section = line
			continue
		case section != "[Install]":
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		for name := range strings.FieldsSeq(value) {
			if checkName(name) != nil {
				continue
			}
			if suffix, ok := linkSuffix(key); ok {
				out = append(out, filepath.Join(folder, name+suffix, unit))
			} else if key == "Alias" {
				out = append(out, filepath.Join(folder, name))
			}
		}
	}
	return out
}

// dropLaunchd boots out what the plan's plist loaded. With the plist on disk it asks launchd for the
// plist's own Label, and boots a held one out by the file's path, so a Label other than the plan's
// name is not missed; then it asks for the name, when it differs. With no plist it boots out the
// name, when launchd holds it.
func dropLaunchd(ctx context.Context, t tool, p Plan, run Runner) error {
	body, label, err := onDisk(p.Files[0].Path)
	if err != nil {
		return err
	}
	if body == nil {
		return bootout(ctx, t, p.Name, t.remove, run)
	}
	if err := bootout(ctx, t, label, t.removeFile, run); err != nil || label == p.Name {
		return err
	}
	return bootout(ctx, t, p.Name, t.remove, run)
}

// bootout runs argv when launchd holds label.
func bootout(ctx context.Context, t tool, label string, argv []string, run Runner) error {
	was, err := ask(ctx, "darwin", tool{query: []string{"launchctl", "print", t.session + "/" + label}}, run)
	if err != nil || !was.Installed {
		return err
	}
	return must(ctx, run, argv)
}

// samePlist refuses an install over a plist on disk whose Label is not the plan's name, which
// launchd may hold and which the package would then no longer find. It gives the plist on disk, and
// nil when there is none.
func samePlist(p Plan) ([]byte, error) {
	body, label, err := onDisk(p.Files[0].Path)
	if err != nil || body == nil {
		return nil, err
	}
	if label != p.Name {
		return nil, fmt.Errorf("the plist %s on disk holds the Label %q, which launchd may hold; remove it with a plan of that name before installing %s", p.Files[0].Path, label, p.Name)
	}
	return body, nil
}

// putBack puts the plist that was on disk before a write changed it back at path, or removes the
// new one when there was none, after the bootout or bootstrap that should have followed failed: an
// Install again then finds its plist changed and boots out and bootstraps again, rather than take
// the new plist on disk for one launchd already holds.
func putBack(path string, old []byte) error {
	if old == nil {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("the new plist %s could not be removed, so an Install again would take it for one launchd holds: %w", path, err)
		}
		return nil
	}
	if _, err := write([]File{{Path: path, Body: string(old)}}); err != nil {
		return fmt.Errorf("the old plist could not be put back, so an Install again would take the new one for one launchd holds: %w", err)
	}
	return nil
}

// onDisk is the plist at path and its Label, when the Label is a name the package takes; the plist
// is nil when there is none.
func onDisk(path string) ([]byte, string, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("the plist %s could not be read: %w", path, err)
	}
	label, err := plistLabel(string(body))
	if err == nil {
		err = checkName(label)
	}
	if err != nil {
		return nil, "", fmt.Errorf("the plist %s on disk: %w", path, err)
	}
	return body, label, nil
}

// plistLabel is the Label of a plist's top dict, and an error when the body is not XML, has no
// Label, has two, gives one that is not a string, or declares a DOCTYPE internal subset (noSubset).
func plistLabel(body string) (string, error) {
	var r plistReader
	err := walkXML(body, "it could not be read as XML", asUTF8, func(d *xml.Decoder, tok xml.Token) error {
		switch tok := tok.(type) {
		case xml.StartElement:
			return r.start(d, tok)
		case xml.EndElement:
			r.open = r.open[:len(r.open)-1]
		case xml.Directive:
			return noSubset(tok)
		}
		return nil
	})
	switch {
	case err != nil:
		return "", err
	case r.value:
		return "", errors.New("its Label has no value")
	case len(r.label) != 1:
		return "", fmt.Errorf("its top dict has %d Labels, and launchd takes one", len(r.label))
	}
	return r.label[0], nil
}

// walkXML reads body by a decoder whose CharsetReader is charset, and gives each token to each. It
// returns the first error each gives, or one that says notXML for a body it could not read.
func walkXML(body, notXML string, charset func(string, io.Reader) (io.Reader, error), each func(*xml.Decoder, xml.Token) error) error {
	d := xml.NewDecoder(strings.NewReader(body))
	d.CharsetReader = charset
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", notXML, err)
		}
		if err := each(d, tok); err != nil {
			return err
		}
	}
}

// asUTF8 is the CharsetReader of a body that is UTF-8 whatever its declaration says: Go text, or a
// task readTask has decoded.
func asUTF8(_ string, in io.Reader) (io.Reader, error) { return in, nil }

// plistReader is what plistLabel has read so far.
type plistReader struct {
	open  []string // the names of the elements open, below the top dict's
	label []string // each Label of the top dict
	value bool     // the last key of the top dict is Label, and its value is next
}

// start reads an element that begins: one in the top dict whole, as a key or a value, and any other
// as one more open.
func (r *plistReader) start(d *xml.Decoder, tok xml.StartElement) error {
	if len(r.open) != 2 || r.open[0] != "plist" || r.open[1] != "dict" {
		r.open = append(r.open, tok.Name.Local)
		return nil
	}
	var text string
	if err := d.DecodeElement(&text, &tok); err != nil {
		return fmt.Errorf("it could not be read as XML: %w", err)
	}
	switch {
	case r.value && tok.Name.Local != "string":
		return fmt.Errorf("its Label is a %s, not a string", tok.Name.Local)
	case r.value:
		r.label = append(r.label, text)
		r.value = false
	case tok.Name.Local == "key":
		r.value = text == "Label"
	}
	return nil
}

// ask runs the query and reads its answer on goos.
func ask(ctx context.Context, goos string, t tool, run Runner) (State, error) {
	absent := 1 // schtasks /query exits 1 for a task it does not hold, whatever the display language
	switch goos {
	case "linux":
		absent = 0 // systemctl show's properties say a unit is missing, and an exit but 0 is an error
	case "darwin":
		absent = 113 // launchctl print exits 113 for a label its domain does not hold
	}
	out, code, err := ran(ctx, run, t.query, absent)
	switch {
	case err != nil:
		return State{}, err
	case code != 0:
		return State{}, nil
	case goos == "linux":
		return readSystemd(out)
	case goos == "darwin":
		return State{Installed: true, Enabled: true}, nil
	}
	return readTask(out)
}

// parseSystemd is systemctl show's three properties, one Name=value a line, and an error when one
// is missing.
func parseSystemd(out []byte) (map[string]string, error) {
	props := map[string]string{}
	for line := range strings.Lines(string(out)) {
		if name, value, ok := strings.Cut(strings.TrimRight(line, "\r\n"), "="); ok {
			props[name] = value
		}
	}
	for _, name := range []string{"LoadState", "UnitFileState", "ActiveState"} {
		if _, ok := props[name]; !ok {
			return nil, fmt.Errorf("systemctl show gave no %s for the unit: %q", name, out)
		}
	}
	return props, nil
}

// readSystemd is the state systemctl show's properties give, and an error for a unit systemd could
// not load. systemd gives ActiveState for a unit it does not hold too, so Active is known either way.
func readSystemd(out []byte) (State, error) {
	props, err := parseSystemd(out)
	if err != nil {
		return State{}, err
	}
	st := State{Active: props["ActiveState"] == "active", ActiveKnown: true}
	switch load := props["LoadState"]; load {
	case "not-found":
		return st, nil
	case "loaded", "masked":
	default:
		return State{}, fmt.Errorf("systemd could not load the unit: its LoadState is %s", load)
	}
	st.Installed, st.Enabled = true, props["UnitFileState"] == "enabled"
	return st, nil
}

// readTask reads the Enabled of a task's Settings from schtasks /query /xml, in UTF-16 or in
// UTF-8; a task whose Settings name no Enabled is enabled, as the task schema says.
func readTask(out []byte) (State, error) {
	text := string(bytes.TrimPrefix(out, []byte{0xEF, 0xBB, 0xBF}))
	if rest, ok := bytes.CutPrefix(out, []byte{0xFF, 0xFE}); ok {
		units := make([]uint16, len(rest)/2)
		for i := range units {
			units[i] = uint16(rest[2*i]) | uint16(rest[2*i+1])<<8
		}
		text = string(utf16.Decode(units))
	}
	var task struct {
		XMLName  xml.Name `xml:"Task"`
		Settings struct {
			Enabled *string `xml:"Enabled"`
		} `xml:"Settings"`
	}
	d := xml.NewDecoder(strings.NewReader(strings.ToValidUTF8(text, "�")))
	d.CharsetReader = asUTF8
	if err := d.Decode(&task); err != nil {
		return State{}, fmt.Errorf("schtasks gave a task that is not XML: %w", err)
	}
	enabled := true
	if e := task.Settings.Enabled; e != nil {
		v, err := strconv.ParseBool(strings.TrimSpace(*e))
		if err != nil {
			return State{}, fmt.Errorf("the task's Settings say Enabled %q, which is not a boolean", *e)
		}
		enabled = v
	}
	return State{Installed: true, Enabled: enabled}, nil
}

// expands is whether Task Scheduler expands %name% in a task's element of this name.
func expands(name string) bool {
	return name == "Command" || name == "Arguments" || name == "WorkingDirectory"
}

// percentFree refuses a task whose Command, Arguments or WorkingDirectory hold a %, read from the
// task's XML as Task Scheduler reads it, character references undone; a body that is not XML; and
// a body that declares an encoding other than UTF-8 or UTF-16, which this reading would misread;
// a body whose root element is not a Task; and a body with a DOCTYPE internal subset (noSubset).
func percentFree(body string) error {
	// encoding/xml asks only of an encoding other than UTF-8; the body is Go text, so UTF-8, and
	// with File.UTF16 it is written as the UTF-16 it declares.
	charset := func(label string, in io.Reader) (io.Reader, error) {
		if !strings.EqualFold(label, "utf-16") {
			return nil, fmt.Errorf("the task declares the encoding %q, and is taken only in UTF-8 or UTF-16", label)
		}
		return in, nil
	}
	var r taskReader
	if err := walkXML(body, "the task's XML could not be read", charset, r.token); err != nil {
		return err
	}
	return r.end()
}

// taskReader is what percentFree has read so far.
type taskReader struct {
	in   int  // how many elements whose text Task Scheduler expands are open
	task bool // the root element, a Task, has begun
}

// token refuses a DOCTYPE internal subset, a root that is not a Task, and a % in the text of an
// element whose text Task Scheduler expands.
func (r *taskReader) token(_ *xml.Decoder, tok xml.Token) error {
	switch tok := tok.(type) {
	case xml.Directive:
		if err := noSubset(tok); err != nil {
			return fmt.Errorf("the task's XML: %w", err)
		}
	case xml.StartElement:
		if !r.task && tok.Name.Local != "Task" {
			return fmt.Errorf("the task's XML begins with a %s element, not a Task", tok.Name.Local)
		}
		r.task = true
		if expands(tok.Name.Local) {
			r.in++
		}
	case xml.EndElement:
		if expands(tok.Name.Local) {
			r.in--
		}
	case xml.CharData:
		if r.in > 0 && bytes.ContainsRune(tok, '%') {
			return fmt.Errorf("the task's command, arguments or working folder %q hold a %%: %w", bytes.TrimSpace(tok), errPercent)
		}
	}
	return nil
}

// end refuses a body that ended with no Task element.
func (r *taskReader) end() error {
	if !r.task {
		return errors.New("the task's XML has no Task element")
	}
	return nil
}

// write writes each file whose bytes on disk differ, through a file beside it renamed over it, and
// says whether it wrote any.
func write(files []File) (bool, error) {
	changed := false
	for _, f := range files {
		want := f.Bytes()
		if have, err := os.ReadFile(f.Path); err == nil && bytes.Equal(have, want) {
			continue
		}
		changed = true
		dir := filepath.Dir(f.Path)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return changed, fmt.Errorf("the folder of %s could not be made: %w", f.Path, err)
		}
		tmp, err := os.CreateTemp(dir, "."+filepath.Base(f.Path)+".*")
		if err != nil {
			return changed, fmt.Errorf("%s could not be written: %w", f.Path, err)
		}
		_, err = tmp.Write(want)
		if err == nil {
			err = tmp.Sync()
		}
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Rename(tmp.Name(), f.Path)
		}
		if err != nil {
			err = fmt.Errorf("%s could not be written: %w", f.Path, err)
			if rerr := os.Remove(tmp.Name()); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
				err = errors.Join(err, fmt.Errorf("its temp file %s is left: %w", tmp.Name(), rerr))
			}
			return changed, err
		}
	}
	return changed, nil
}

// noSubset refuses a directive that holds a [, as a DOCTYPE's internal subset does: encoding/xml
// reads a directive to its end by its quotes, not by its brackets, comments or processing
// instructions, so with one, what it takes for the end can be another place than launchd's or Task
// Scheduler's reader takes, and an element either reads can lie inside what Go passes over.
func noSubset(d xml.Directive) error {
	if bytes.ContainsRune(d, '[') {
		return fmt.Errorf("it declares a DOCTYPE with an internal subset (%q), which the package does not read", bytes.TrimSpace(d))
	}
	return nil
}
