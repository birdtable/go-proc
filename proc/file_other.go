//go:build !linux

package proc

// fileRuns is whether Run starts a Cmd.File here: no system but Linux does.
const fileRuns = false

// procMounted is whether dir is on a proc mount: never here, where no File runs.
func procMounted(string) bool { return false }
