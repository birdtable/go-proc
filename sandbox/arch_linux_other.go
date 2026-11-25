//go:build linux && !amd64 && !arm64

package sandbox

// builtArch is the architecture this program was built for: one with no
// seccomp filter, so the Linux block refuses to run a program.
const builtArch = ""
