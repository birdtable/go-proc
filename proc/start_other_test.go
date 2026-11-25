//go:build !windows

package proc

// startTestProcess starts c, a process a test starts itself. Off Windows it is a plain start:
// the rule is Windows's, where a process a test started, in no job of the test binary's,
// outlives the binary when it is killed.
func startTestProcess(c *process) error {
	return c.Start()
}
