//go:build !linux

package proc

import "errors"

// procRoot is nothing here: no proc mount is read.
type procRoot struct{}

// close closes nothing here.
func (procRoot) close() error { return nil }

// procOwn checks nothing here: no proc mount is read, and the guard, where there is one, reads the
// system's own process table. A stand-in for a proc mount, or a kernel taken for one with no
// openat2, means nothing here, and is refused.
func procOwn(p procFS) (procRoot, error) {
	if p.standIn || p.noOpenat2 {
		return procRoot{}, errors.New("proc: a stand-in for a proc mount is read on Linux alone")
	}
	return procRoot{}, nil
}
