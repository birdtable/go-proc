package proc

import (
	"fmt"
	"strconv"
	"strings"
)

// nspidOwn is why status, the text of a proc mount's self/status read by the process pid, does not
// show the mount to be that process's own pid namespace's, or "". Its NSpid line lists the
// process's id in each pid namespace from the mount's own down to the innermost, the process's, so
// the mount is the process's own when the one line holds one number, and that number is pid. A
// status with no NSpid line, as before Linux 4.1, or with more than one, shows nothing, and is
// refused too. Only Linux reads a proc mount; the text is read the same on every system.
func nspidOwn(status string, pid int) string {
	var lines []string
	for line := range strings.SplitSeq(status, "\n") {
		if rest, ok := strings.CutPrefix(line, "NSpid:"); ok {
			lines = append(lines, rest)
		}
	}
	if len(lines) != 1 {
		return fmt.Sprintf("it has %d NSpid lines, not one", len(lines))
	}
	ids := strings.Fields(lines[0])
	switch {
	case len(ids) != 1:
		return fmt.Sprintf("its NSpid line lists %d ids, %q, not one, so the mount is not the innermost pid namespace's", len(ids), ids)
	case ids[0] != strconv.Itoa(pid):
		return fmt.Sprintf("its NSpid line names process %q, not this one, %d", ids[0], pid)
	}
	return ""
}
