//go:build unix && !linux && !darwin

package proc

// treeIDHere is how this system names a tree: it cannot, since x/sys does not read this Unix's
// process table for a start time.
const treeIDHere = idUnread

// listsBeforeStop is false: groupLeft is true here, so a second listing would add nothing.
const listsBeforeStop = false

// leader reads nothing here.
func leader(procRoot, int) (pgid int, born uint64, err error) { return 0, 0, errNoTreeID }

// place reads nothing here.
func place(procRoot) (string, error) { return "", errNoTreeID }

// groupMembers lists nothing here: no tree is stopped by id.
func groupMembers(int) ([]member, error) { return nil, errNoTreeID }

// groupLeft is true here: x/sys lists no group's processes on this Unix, so none is ruled out.
func groupLeft(int, int, func()) bool { return true }
