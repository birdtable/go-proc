package gitenv

// Names is names, for the tests of package gitenv_test, which hold git's own list to it. It is in a
// test file, so no build but a test's has it.
func Names() []string { return names() }

// ErrNoFolder, ErrRelative and ErrUnclean are the refusals of Home and NoHome, for the tests of
// package gitenv_test, which tell each refusal from the others.
var (
	ErrNoFolder = errNoFolder
	ErrRelative = errRelative
	ErrUnclean  = errUnclean
)

// The errors CommitMode refuses with, for the rows that match each refusal.
var (
	ErrNotRegular = errNotRegular
	ErrHeadMode   = errHeadMode
)
