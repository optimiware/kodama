package domain

import "fmt"

// MaxSafeInteger is the largest integer carried by any Kodama JSON contract.
// It is the largest integer every common JSON implementation represents
// exactly, so contracts never depend on floating-point behaviour.
const MaxSafeInteger = 1<<53 - 1

// Version is the integer resource version behind an ETag and optimistic checks.
type Version int64

// ContentRevision counts changes to editable work content.
type ContentRevision int64

func (v Version) Validate() error         { return positive("version", int64(v)) }
func (r ContentRevision) Validate() error { return positive("content revision", int64(r)) }

func positive(name string, v int64) error {
	if v < 1 || v > MaxSafeInteger {
		return fmt.Errorf("%w: %s must be between 1 and %d", ErrInvalidValue, name, int64(MaxSafeInteger))
	}
	return nil
}
