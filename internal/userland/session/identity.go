package session

import "fmt"

// SessionID is one bounded durable session identity.
//
// A session identity names the session stream whose revisions a checkpoint
// belongs to. It is never a process ID, a scratch path or a worktree path.
type SessionID string

// maxSessionIDBytes bounds one session identity.
const maxSessionIDBytes = 128

// ParseSessionID validates and converts a session identity.
func ParseSessionID(value string) (SessionID, error) {
	if !validSessionID(value) {
		return "", fmt.Errorf("invalid session id %q", value)
	}
	return SessionID(value), nil
}

// validSessionID reports whether value is a bounded ASCII identity. Dots are
// refused so an identity can never be confused with a path component.
func validSessionID(value string) bool {
	if value == "" || len(value) > maxSessionIDBytes {
		return false
	}
	if value == "." || value == ".." {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '-', character == '_':
		default:
			return false
		}
	}
	return true
}
