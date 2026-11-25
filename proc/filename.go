package proc

import "github.com/birdtable/go-proc/internal/homevars"

// FileName is the file name the program id has on the system goos, named as runtime.GOOS names
// one: id with .exe after it on windows, and id as given on every other system. It is for code that
// writes, copies, links or looks for a program's file by its name; Run, and exec.LookPath, complete
// .exe by PATHEXT themselves when they find a program. id is the name without its extension, so one
// that already ends in .exe is given a second. goos is an argument, so a caller tests every
// system's name on any host.
func FileName(goos, id string) string {
	if goos == "windows" {
		return id + ".exe"
	}
	return id
}

// LocalFileName is the file name the program id has on the system this process runs on, as
// FileName gives it for that system: id with .exe after it on Windows, and id as given elsewhere.
// Code that names every system's name, or tests one system's on another, calls FileName. id is the
// name without its extension, so one that already ends in .exe is given a second on Windows.
func LocalFileName(id string) string {
	return FileName(homevars.Here, id)
}
