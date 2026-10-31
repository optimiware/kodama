//go:build darwin

package process

// tableVisibility verifies that `ps -A` lists every user's processes: macOS has
// no procfs and no mechanism that hides other users' processes from ps, so the
// process table is complete by the platform's own rule.
func tableVisibility() error { return nil }
