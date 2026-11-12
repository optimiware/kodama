//go:build !linux && !darwin

package sessionlock

import (
	"errors"
	"testing"
)

// TestNewSessionLockRefusesUnsupportedPlatform pins that a platform without a
// supported exclusive file lock reports a clear refusal instead of silently
// providing a process-local lock.
func TestNewSessionLockRefusesUnsupportedPlatform(t *testing.T) {
	_, err := NewSessionLock("/home/engineer/.config/kodama/session.json")
	if !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("NewSessionLock error = %v; want ErrUnsupportedPlatform", err)
	}
}
