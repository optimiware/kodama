//go:build !linux && !darwin

package process

import "errors"

// tableVisibility has no verification rule for this platform, so an empty group
// is never proven absent after a permission-denied probe.
func tableVisibility() error {
	return errors.New("no rule verifies process table visibility on this platform")
}
