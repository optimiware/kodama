package secrets

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// RandomIDs generates opaque identifiers from 128 random bits, for example
// "sec_9f86d081884c7d659a2feaa0c55ad015". The prefix is a readability aid:
// nothing may infer ownership, kind or order from an identifier.
type RandomIDs struct{}

// NewID implements IDGenerator.
func (RandomIDs) NewID(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("secrets: generate ID: %w", err)
	}
	return prefix + "_" + hex.EncodeToString(b[:]), nil
}
