//go:build unix && !linux

package proc

// fionread is the ioctl that gives how many bytes a pipe holds: FIONREAD, _IOR('f', 127, int) in
// sys/filio.h, which x/sys does not name here.
const fionread = 0x4004667f
