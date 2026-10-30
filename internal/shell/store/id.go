package store

import (
	"crypto/rand"
	"encoding/hex"
)

func newID(prefix string) (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + "_" + hex.EncodeToString(b[:]), nil
}

// NewID exposes identifier generation to the API layer.
func NewID(prefix string) (string, error) { return newID(prefix) }
