//go:build unix && !linux && !darwin

package proc

// nsModes runs no helper mode here: a tree id here carries no namespace.
func nsModes(string, []string) (int, bool) { return 0, false }
