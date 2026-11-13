//go:build !sane

package scanner

import "errors"

// Default is the real scanner driver. This build was made without SANE, so there is none.
func Default() Driver { return unsupported{} }

type unsupported struct{}

func (unsupported) Open() (Device, error) {
	return nil, errors.New("this build has no scanner support (build with -tags sane)")
}
