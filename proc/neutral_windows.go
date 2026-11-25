package proc

import (
	"path/filepath"

	"golang.org/x/sys/windows" //nolint:depguard // arch.ports.23: the system folder, whose drive is the system drive, is read by GetSystemDirectory, in x/sys alone
)

// NeutralDir is a folder that is no project's, for a Cmd's Dir: the root of the system drive here,
// C:\ on most hosts, read from the system folder's drive and not from a variable the caller's
// environment could set. If that read fails it is \, the root of this process's drive.
func NeutralDir() string {
	sys, err := windows.GetSystemDirectory()
	if vol := filepath.VolumeName(sys); err == nil && vol != "" {
		return vol + `\`
	}
	return `\`
}
