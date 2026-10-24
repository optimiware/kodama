package secretkeys

import "errors"

var (
	ErrInvalid     = errors.New("secretkeys: invalid")
	ErrUnavailable = errors.New("secretkeys: unavailable")
	ErrCiphertext  = errors.New("secretkeys: ciphertext rejected")
	ErrDenied      = errors.New("secretkeys: denied")
	ErrNotFound    = errors.New("secretkeys: key not found")
	ErrConflict    = errors.New("secretkeys: conflict")
)
