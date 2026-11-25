// Package asbuilt holds the gates of go-proc's libraries to the value each exported error var was
// built with: a gate returns, wraps or matches an unexported copy taken at init, so a value a caller
// or a dependency stores in the exported var later, nil included, never turns a refusal into a go.
// It has no code of its own; its tests read the module's source.
package asbuilt
