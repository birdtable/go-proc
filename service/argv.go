package service

import (
	"encoding/xml"
	"strconv"
	"strings"
)

// tool is the argv of each step on one system. reload is empty where the tool reads its files
// when it is told to install, and stop where removing a unit also stops it. On macOS session is the
// login session launchctl addresses, and removeFile boots out the Label the plist on disk holds.
type tool struct {
	reload, install, stop, remove, removeFile, query []string
	session                                          string
}

// toolFor is the argv of each step for a checked plan on linux, darwin or windows, with uid the
// account launchctl addresses its login session by.
func toolFor(goos string, uid int, p Plan) tool {
	switch goos {
	case "linux":
		return tool{
			reload:  []string{"systemctl", "--user", "daemon-reload"},
			install: []string{"systemctl", "--user", "enable", "--now", p.Name},
			stop:    []string{"systemctl", "--user", "stop", p.Name},
			remove:  []string{"systemctl", "--user", "disable", p.Name},
			query:   []string{"systemctl", "--user", "show", "-p", "LoadState,UnitFileState,ActiveState", p.Name},
		}
	case "darwin":
		session := "gui/" + strconv.Itoa(uid)
		return tool{
			install:    []string{"launchctl", "bootstrap", session, p.Files[0].Path},
			remove:     []string{"launchctl", "bootout", session + "/" + p.Name},
			removeFile: []string{"launchctl", "bootout", session, p.Files[0].Path},
			query:      []string{"launchctl", "print", session + "/" + p.Name},
			session:    session,
		}
	case "windows":
		// install is createArgv, with the path of the XML Install writes for it.
		return tool{
			remove: []string{"schtasks", "/delete", "/tn", p.Name, "/f"},
			query:  []string{"schtasks", "/query", "/tn", p.Name, "/xml"},
		}
	}
	return tool{}
}

// createArgv is schtasks' argv to take the task name from the XML at path, in place of one it holds.
func createArgv(name, path string) []string {
	return []string{"schtasks", "/create", "/tn", name, "/xml", path, "/f"}
}

// Quote is s as one argument in a body for goos: in a systemd unit's command line on Linux, in a
// plist's <string> on macOS, and in a task's <Arguments> on Windows, as a program reads its command
// line there. On another system s is given back as it is.
//
// On Linux % and $ are doubled, since systemd reads them as a specifier and a variable inside
// quotes too; a line end is written as \n or \r, since systemd ends a unit's line at either; and a
// NUL has no form there, so Install refuses a Linux file that holds one. On Windows a % is left as it
// is, and Install refuses a task whose arguments hold one (ErrPercent): Task Scheduler expands it,
// and gives no way to keep it as it is.
func Quote(goos, s string) string {
	switch goos {
	case "linux":
		s = strings.NewReplacer("%", "%%", "$", "$$").Replace(s)
		if s != "" && !strings.ContainsAny(s, " \t\n\r\"'\\;") {
			return s
		}
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`).Replace(s) + `"`
	case "darwin":
		return xmlText(s)
	case "windows":
		return xmlText(windowsArg(s))
	}
	return s
}

// windowsArg is s as one argument of a command line that CommandLineToArgvW and the C runtime
// read back as s: quoted when it is empty or holds a space, a tab or a quote, each quote after
// a backslash, and the backslashes before a quote or the closing quote doubled.
func windowsArg(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n\v\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for _, r := range s {
		switch r {
		case '\\':
			slashes++
		case '"':
			b.WriteString(strings.Repeat(`\`, slashes+1))
			slashes = 0
		default:
			slashes = 0
		}
		b.WriteRune(r)
	}
	b.WriteString(strings.Repeat(`\`, slashes))
	b.WriteByte('"')
	return b.String()
}

// xmlText is s as XML character data.
func xmlText(s string) string {
	var b strings.Builder
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		return "" // a strings.Builder's Write returns no error, so EscapeText returns none
	}
	return b.String()
}
