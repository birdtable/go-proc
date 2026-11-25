package proc

// killAgain is whether a group is killed again until no live process of it answers. macOS needs
// it: a process forked while the group's SIGKILL lands is not reached by it, and it answers a group
// of exited processes not yet reaped with EPERM, so a kill that finds no live process says so.
const killAgain = true
