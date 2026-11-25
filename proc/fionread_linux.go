package proc

import "golang.org/x/sys/unix" //nolint:depguard // arch.ports.23: FIONREAD is TIOCINQ by value on Linux, which x/sys alone names for each architecture

// fionread is the ioctl that gives how many bytes a pipe holds: Linux's TIOCINQ, its FIONREAD.
const fionread = unix.TIOCINQ
