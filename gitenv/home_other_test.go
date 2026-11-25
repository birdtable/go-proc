//go:build !windows

package gitenv_test

import "testing"

// systemConfigCases is the reds of the configuration row this system adds: none, as git here takes
// the home from HOME alone.
func systemConfigCases(string) []configCase { return nil }

// systemNetrcPlaces is the reds of the .netrc row this system adds: none, as libcurl here reads
// .netrc alone and, with HOME unset, takes the home from the passwd entry, the real account's, which
// no test plants in.
func systemNetrcPlaces(*testing.T) []netrcPlace { return nil }
