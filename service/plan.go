// Package service installs, removes and queries one login service or timer, built on the system's
// own tool: systemctl --user on Linux, launchctl in the login session on macOS, schtasks on
// Windows. It adds a refusal, before anything is written, of what a tool would misread, and reads
// only machine answers. The app gives each unit, plist and task body as text, and the Runner. What
// each call runs, refuses and does not hold is in https://github.com/birdtable/go-proc/blob/main/docs/service.md.
package service

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf16"
)

// A Plan is one unit as the system's tool names it, and the files that make it.
type Plan struct {
	// Name is the unit as the tool addresses it: a systemd unit with its suffix (app.timer), a
	// launchd label (com.example.app), which is the Label of its plist, or a Task Scheduler task in
	// its root folder (app). It is lower-case ASCII letters, digits, '.', '-' and '_', at most 230
	// bytes, starts with neither '-' nor '.' and does not end in '.', and its part before its first
	// '.' is no Windows device name (con, prn, aux, nul, com0 to com9, lpt0 to lpt9), on every
	// system, as names that differ by case alone are one task on Windows and one plist on macOS.
	Name string
	// Files are written, in order, before the tool is run, each in one of Folders(). On macOS there
	// is one, the plist that launchctl bootstraps. On Windows there is one, the task's XML, with an
	// empty Path: Install writes it to a folder of its own in the system's temp folder, has schtasks
	// read it there and removes the folder, so the package keeps no file of a task.
	Files []File
}

// A File is one file a plan writes: its absolute path and its text. On Windows the path is empty.
type File struct {
	Path string
	Body string
	// UTF16 writes the body as UTF-16 little-endian with a byte order mark, as Task Scheduler
	// reads a task's XML.
	UTF16 bool
}

// Bytes is the file as it is written: the body, or with UTF16 the body in UTF-16 little-endian
// after the byte order mark FF FE.
func (f File) Bytes() []byte {
	if !f.UTF16 {
		return []byte(f.Body)
	}
	units := utf16.Encode([]rune(f.Body))
	out := make([]byte, 0, 2+2*len(units))
	out = append(out, 0xFF, 0xFE)
	for _, u := range units {
		out = binary.LittleEndian.AppendUint16(out, u)
	}
	return out
}

// State is what the system's tool answers about a unit.
type State struct {
	// Installed: the system holds the unit (systemd's LoadState is loaded or masked, launchctl
	// print finds the label, schtasks finds the task).
	Installed bool
	// Enabled: it will run when its time comes (systemd's UnitFileState is enabled, the task's
	// Settings say Enabled; on macOS a label launchd holds is enabled).
	Enabled bool
	// Active: systemd's ActiveState is active. It means something only with ActiveKnown.
	Active bool
	// ActiveKnown: the tool answered whether the unit runs now, as systemd does. launchctl and
	// schtasks give no machine answer for it, so on macOS and Windows it is false, and so is Active.
	ActiveKnown bool
}

// A Runner runs argv and returns what it printed, its exit code, and an error only when it could
// not be run or did not end by itself.
type Runner func(ctx context.Context, argv []string) ([]byte, int, error)

// ErrAbsent is every call on a system this package has no tool for.
var ErrAbsent = fmt.Errorf("this system has no service tool that this package drives: %w", errors.ErrUnsupported)

// ErrPercent is an install on Windows whose task holds a % in its command, its arguments or its
// working folder: Task Scheduler reads %name% there as a variable and gives the program its value,
// and gives no way to keep a % as it is.
var ErrPercent = errors.New("a % in the task's command, arguments or working folder, which Task Scheduler would read as the start of a variable")

// ErrUsage is matched by every refusal of a plan the tool would misread or that reaches past the
// package's folders, made before any file is written or any tool run. The error's text is the
// refusal's own.
var ErrUsage = errors.New("the plan is refused before anything is written or run")

// errAbsent, errPercent and errUsage are what each call returns and matches for ErrAbsent,
// ErrPercent and ErrUsage: the values those were given here, so a value a caller or a dependency
// stores in one later, nil included, changes what a match finds and never what is refused.
var errAbsent, errPercent, errUsage = ErrAbsent, ErrPercent, ErrUsage

// usage is a refusal of a plan, which matches ErrUsage.
type usage struct{ msg string }

func (u usage) Error() string        { return u.msg }
func (u usage) Is(target error) bool { return target == errUsage }

func refuse(format string, args ...any) error { return usage{fmt.Sprintf(format, args...)} }

// nameMax is the longest name taken, a fact of the tools: the longest Task Scheduler creates, as
// measured on Windows 10, inside its documented 238 and the file systems' 255.
const nameMax = 230

// checkName refuses a name that is not lower-case ASCII letters, digits, '.', '-' and '_', that
// starts with '-' or '.', that is longer than nameMax, that ends in '.', or whose part before its
// first '.' is a Windows device name: a tool reads *, ? and [ as a pattern of many units, - as an
// option, / as a step of launchd's domain, \ as a folder of Task Scheduler, and Task Scheduler and
// APFS read App as app, so a name is lower case, the practice of RFC 1123's DNS labels.
// Task Scheduler keeps a task as a file of its name, and the practice followed is
// Win32's file naming rules, which drop a last '.' from a file name and read a device name, with or
// without an extension, as the device. A native run on Windows 10.0.26200 found schtasks keeps app.
// and app as two tasks, so the last '.' is refused by that rule, not for a collision
// seen. A name is the same on every system, so each system refuses them.
func checkName(name string) error {
	switch {
	case name == "":
		return refuse("the unit has no name")
	case len(name) > nameMax:
		return refuse("the unit name %q is %d bytes, and a name is %d or fewer", name, len(name), nameMax)
	case name[0] == '-' || name[0] == '.':
		return refuse("the unit name %q starts with %q, which the tool would read as an option or a hidden file", name, name[0])
	}
	for _, r := range name {
		if !nameRune(r) {
			return refuse("the unit name %q holds %q; a name is lower-case ASCII letters, digits, '.', '-' and '_', so no tool reads it as a pattern, an option, a path or, by case, another name", name, r)
		}
	}
	switch stem, _, _ := strings.Cut(name, "."); {
	case name[len(name)-1] == '.':
		return refuse("the unit name %q ends in '.', which Win32's file naming rules drop from a file name, and Task Scheduler keeps a task as a file of its name", name)
	case isDevice(stem):
		return refuse("the unit name %q is the Windows device %s, with or without an extension, which Win32's file naming rules read as the device and not as a file, and Task Scheduler keeps a task as a file of its name", name, strings.ToUpper(stem))
	}
	return nil
}

// isDevice reports whether stem is one of the names Win32's file naming rules reserve for a
// device, in any case: CON, PRN, AUX, NUL, and COM and LPT each with a digit, 0 to 9.
func isDevice(stem string) bool {
	switch strings.ToUpper(stem) {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if len(stem) != 4 || stem[3] < '0' || stem[3] > '9' {
		return false
	}
	head := strings.ToUpper(stem[:3])
	return head == "COM" || head == "LPT"
}

func nameRune(r rune) bool {
	return 'a' <= r && r <= 'z' || '0' <= r && r <= '9' || r == '.' || r == '-' || r == '_'
}

// check refuses a plan the tool would misread, or whose files are outside h's folders, before
// anything is written or run.
func (p Plan) check(h host, run Runner) error {
	if run == nil {
		return refuse("the runner is nil, so no tool can be run")
	}
	if err := checkName(p.Name); err != nil {
		return err
	}
	if h.goos != "linux" && len(p.Files) != 1 {
		return refuse("the plan has %d files, and on %s it is the one file the tool reads", len(p.Files), h.goos)
	}
	for _, f := range p.Files {
		switch {
		case h.goos == "windows" && f.Path != "":
			return refuse("the task's file is %q, and on Windows its Path is empty: the package keeps no file of a task, and gives schtasks the XML from a folder it makes and removes", f.Path)
		case h.goos == "windows":
			continue
		case !filepath.IsAbs(f.Path):
			return refuse("the file %q is not an absolute path", f.Path)
		case filepath.Clean(f.Path) != f.Path:
			return refuse("the file %q is not in its clean form", f.Path)
		case !slices.Contains(h.folders, filepath.Dir(f.Path)):
			return refuse("the file %s is not in a folder the package writes units to, which on %s are %q", f.Path, h.goos, h.folders)
		}
		if h.goos == "linux" {
			if err := oneLineEach(f); err != nil {
				return err
			}
		}
	}
	if h.goos == "darwin" {
		if p.Files[0].UTF16 {
			return refuse("the plist of %s is marked UTF16: the package reads a plist on disk as UTF-8, so it could neither install it again nor remove it", p.Name)
		}
		label, err := plistLabel(p.Files[0].Body)
		if err != nil {
			return refuse("the plist of %s: %v", p.Name, err)
		}
		if label != p.Name {
			return refuse("the plist's Label is %q and the plan's name %q: launchctl bootstraps the Label, and the package asks for and boots out the name", label, p.Name)
		}
	}
	return nil
}

// oneLineEach refuses a unit file whose bytes, as written, hold a NUL or a carriage return that does
// not end a line with the newline after it: systemd ends a line at each, so a line of the caller's
// would be read as two, and the second as a setting of its own.
func oneLineEach(f File) error {
	b := f.Bytes()
	if i := bytes.IndexByte(b, 0); i >= 0 {
		return refuse("the unit file %s holds a NUL at byte %d, where systemd ends the line", f.Path, i)
	}
	for i, c := range b {
		if c == '\r' && (i+1 == len(b) || b[i+1] != '\n') {
			return refuse("the unit file %s holds a carriage return at byte %d with no newline after it, where systemd ends the line", f.Path, i)
		}
	}
	return nil
}
