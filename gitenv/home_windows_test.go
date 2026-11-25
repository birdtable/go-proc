//go:build windows

package gitenv_test

import "testing"

// systemConfigCases is Windows' own red of the configuration row: git for Windows takes the home
// from USERPROFILE where HOME is unset, so a .gitconfig in profile is read.
func systemConfigCases(profile string) []configCase {
	return []configCase{{"USERPROFILE", []string{"USERPROFILE=" + profile}, "probe.profile=planted"}}
}

// systemNetrcPlaces is Windows' own reds of the .netrc row: libcurl reads _netrc where the home holds
// no .netrc, and takes the home from USERPROFILE where HOME is unset.
func systemNetrcPlaces(t *testing.T) []netrcPlace {
	t.Helper()
	return []netrcPlace{{"HOME", t.TempDir(), []string{"_netrc"}}, {"USERPROFILE", t.TempDir(), []string{".netrc", "_netrc"}}}
}
