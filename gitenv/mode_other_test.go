//go:build !windows

package gitenv_test

// setsExecBit is whether a file written here can have the owner's exec bit as Go reports it: yes,
// on every system but Windows.
const setsExecBit = true

// initFileMode is the core.fileMode a fresh git init sets where the folder keeps the exec bit.
const initFileMode = "true"
