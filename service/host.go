package service

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

// host is the system the calls act on: its name as runtime.GOOS gives it, the account launchctl
// addresses its login session by, and the folders a plan's files may be in, or why they could not
// be found. readlink is nil but in a test of the package, which plants the enable links systemctl
// makes without the privilege Windows wants for a symbolic link; linkReader takes it only under
// testing.Testing(). staged is nil but in a test of the package too, which makes a file in a
// Windows task's folder before the task's XML is written there, as another account could; staging
// runs it only under testing.Testing(). task is what such a test plants for a Windows task's folder
// as it is read back and removed (taskSeams).
type host struct {
	goos     string
	uid      int
	folders  []string
	err      error
	readlink func(string) (string, error)
	staged   func(dir string)
	task     taskSeams
}

// linkReader reads a symbolic link: os.Readlink, or under testing.Testing() the reader a test of
// the package planted in the host.
func (h host) linkReader() func(string) (string, error) {
	if testing.Testing() && h.readlink != nil {
		return h.readlink
	}
	return os.Readlink
}

// staging runs, under testing.Testing() alone, what a test of the package planted in the host for a
// Windows task's folder dir, once it is made and before the task's XML is written in it.
func (h host) staging(dir string) {
	if testing.Testing() && h.staged != nil {
		h.staged(dir)
	}
}

// here is this system as a host.
func here() host {
	h := host{goos: runtime.GOOS, uid: os.Getuid()}
	h.folders, h.err = foldersOf(h.goos, h.uid)
	return h
}

// Folders are the folders a plan's files may be in on this system, and every call refuses a file in
// any other: on Linux the user's systemd folder in the configuration home, where systemctl enable
// makes its links, and /run/user/<uid>/systemd/user, which systemd's user manager reads and which is
// emptied at logout; on macOS ~/Library/LaunchAgents. Each is a folder the tool itself reads from,
// and the package names none of its own. On Windows there are none: schtasks reads a task's XML
// once, so the package keeps no file of the task, and a Windows plan's file has an empty Path. It is
// ErrAbsent on a system with no tool.
func Folders() ([]string, error) {
	h := here()
	return h.folders, h.err
}

func foldersOf(goos string, uid int) ([]string, error) {
	switch goos {
	case "linux":
		config, err := os.UserConfigDir()
		if err != nil {
			return nil, fmt.Errorf("the user's systemd folder could not be found: %w", err)
		}
		return []string{filepath.Join(config, "systemd", "user"), filepath.Join("/run/user", strconv.Itoa(uid), "systemd", "user")}, nil
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("the user's LaunchAgents folder could not be found: %w", err)
		}
		return []string{filepath.Join(home, "Library", "LaunchAgents")}, nil
	case "windows":
		return []string{}, nil
	}
	return nil, errAbsent
}
