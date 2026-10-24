package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Errors of the contract. They are classes: messages carry no content.
var (
	// ErrInvalid: the document is malformed or violates its invariants.
	ErrInvalid = errors.New("logs/contract: invalid metadata")
	// ErrBinding: the document describes a different artifact than the one it
	// accompanies.
	ErrBinding = errors.New("logs/contract: metadata does not bind this artifact")
)

// Encode validates and serializes m deterministically.
func Encode(m Metadata) ([]byte, error) {
	if m.Reasons == nil {
		m.Reasons = []Reason{}
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if len(b) > MaxMetadataBytes {
		return nil, fmt.Errorf("%w: document exceeds %d bytes", ErrInvalid, MaxMetadataBytes)
	}
	return b, nil
}

// Decode strictly parses a sidecar: bounded size, unknown fields rejected,
// exactly one JSON value, then validated.
func Decode(data []byte) (Metadata, error) {
	if len(data) > MaxMetadataBytes {
		return Metadata{}, fmt.Errorf("%w: document exceeds %d bytes", ErrInvalid, MaxMetadataBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m Metadata
	if err := dec.Decode(&m); err != nil {
		return Metadata{}, fmt.Errorf("%w: not a metadata document", ErrInvalid)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Metadata{}, fmt.Errorf("%w: trailing content", ErrInvalid)
	}
	if err := m.Validate(); err != nil {
		return Metadata{}, err
	}
	return m, nil
}

// Unverified is the statement about a log artifact that has no sidecar: its
// completeness is unknown, never complete.
func Unverified() (Completeness, []Reason) { return Unknown, []Reason{ReasonUnverified} }
