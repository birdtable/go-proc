//go:build windows

package gitenv_test

// setsExecBit is whether a file written here can have the owner's exec bit as Go reports it: not on
// Windows, where Go reports none.
const setsExecBit = false

// initFileMode is the core.fileMode a fresh git init sets: Git for Windows sets false.
const initFileMode = "false"
