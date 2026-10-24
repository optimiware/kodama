//go:build !unix

package devenv

// LockScope is a no-op on platforms without flock. The development
// environment targets macOS and Linux.
func LockScope(scope string) (func(), error) { return func() {}, nil }
