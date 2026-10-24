package httpapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"sort"
	"strings"

	"github.com/optimiware/kodama/internal/domain"
)

const (
	// MinCursorKeyBytes is the shortest accepted signing key.
	MinCursorKeyBytes = 32
	// MaxCursorPositionBytes bounds the module-supplied continuation position.
	MaxCursorPositionBytes = 1024
	// MaxCursorBytes bounds the encoded token accepted from a client.
	MaxCursorBytes = 2048

	cursorVersion  = 1
	cursorMacLabel = "kodama-cursor-v1"
)

var cursorEncoding = base64.RawURLEncoding

// CursorScope is everything a cursor is bound to. A token issued for one scope
// fails verification under any other: the scope is not stored in the token, it
// is mixed into the MAC, so a copied cursor cannot be pointed at another
// tenant, project, endpoint, filter set or ordering.
type CursorScope struct {
	TenantID  domain.TenantID
	ProjectID domain.ProjectID
	// Endpoint is the registered route pattern (Call.Pattern).
	Endpoint string
	// PathValues are the request's values for the endpoint's wildcards, in
	// pattern order (Call.CursorScope fills them), so a cursor for one parent
	// resource cannot continue a listing of another under the same template.
	PathValues []string
	// Filters are the normalized, validated filter values the module applied,
	// excluding limit and cursor. Value order is significant.
	Filters map[string][]string
	// Sort names the deterministic ordering, which must end in a unique
	// tie-breaker so the continuation position identifies exactly one row.
	Sort string
}

// canonical frames the scope unambiguously (length-prefixed fields, filters in
// key order).
func (s CursorScope) canonical() []byte {
	var b []byte
	b = appendFrame(b, string(s.TenantID))
	b = appendFrame(b, string(s.ProjectID))
	b = appendFrame(b, s.Endpoint)
	b = appendFrame(b, s.Sort)
	b = binary.BigEndian.AppendUint32(b, uint32(len(s.PathValues)))
	for _, v := range s.PathValues {
		b = appendFrame(b, v)
	}
	keys := make([]string, 0, len(s.Filters))
	for k := range s.Filters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b = binary.BigEndian.AppendUint32(b, uint32(len(keys)))
	for _, k := range keys {
		b = appendFrame(b, k)
		vs := s.Filters[k]
		b = binary.BigEndian.AppendUint32(b, uint32(len(vs)))
		for _, v := range vs {
			b = appendFrame(b, v)
		}
	}
	return b
}

func appendFrame(b []byte, s string) []byte {
	b = binary.BigEndian.AppendUint32(b, uint32(len(s)))
	return append(b, s...)
}

// ErrInvalidCursor is the cause of every cursor rejection. The response never
// says why, so a probe learns nothing about which check failed.
var ErrInvalidCursor = errors.New("httpapi: invalid cursor")

func invalidCursorError() *APIError {
	e := NewError(400, CodeInvalidCursor, "the cursor is not valid for this request")
	e.Cause = ErrInvalidCursor
	return e
}

// CursorCodec issues and verifies authenticated opaque cursors. It is
// stateless and safe for concurrent use.
type CursorCodec struct {
	keys [][]byte
}

// NewCursorCodec returns a codec that signs with signingKey and verifies with
// it and any previous keys, which allows key rotation without invalidating
// in-flight cursors. Keys are injected by the composition root; each must be at
// least MinCursorKeyBytes long.
func NewCursorCodec(signingKey []byte, previous ...[]byte) (*CursorCodec, error) {
	all := append([][]byte{signingKey}, previous...)
	c := &CursorCodec{}
	for _, k := range all {
		if len(k) < MinCursorKeyBytes {
			return nil, errors.New("httpapi: cursor signing keys must be at least 32 bytes")
		}
		c.keys = append(c.keys, append([]byte(nil), k...))
	}
	return c, nil
}

func (c *CursorCodec) mac(key []byte, scope CursorScope, payload []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(cursorMacLabel))
	m.Write(scope.canonical())
	m.Write(payload)
	return m.Sum(nil)
}

// Encode returns a token binding position to scope. The position is the
// module's continuation key (typically the last returned row's sort-key
// values); it is readable by the client but cannot be altered or moved to
// another scope.
func (c *CursorCodec) Encode(scope CursorScope, position []byte) (string, error) {
	if c == nil || len(c.keys) == 0 {
		return "", errors.New("httpapi: cursor codec is not configured")
	}
	if len(position) == 0 || len(position) > MaxCursorPositionBytes {
		return "", errors.New("httpapi: cursor position must be 1..1024 bytes")
	}
	payload := append([]byte{cursorVersion}, position...)
	return cursorEncoding.EncodeToString(payload) + "." + cursorEncoding.EncodeToString(c.mac(c.keys[0], scope, payload)), nil
}

// Decode verifies token under scope and returns its position. Any failure is a
// 400 invalid_cursor. Verification proves provenance and scope only: the caller
// still authorizes the request.
func (c *CursorCodec) Decode(scope CursorScope, token string) ([]byte, error) {
	if c == nil || len(c.keys) == 0 {
		return nil, UnavailableError(0, errors.New("httpapi: cursor codec is not configured"))
	}
	if token == "" || len(token) > MaxCursorBytes {
		return nil, invalidCursorError()
	}
	body, sig, ok := strings.Cut(token, ".")
	if !ok {
		return nil, invalidCursorError()
	}
	payload, err := cursorEncoding.DecodeString(body)
	if err != nil || len(payload) < 2 || payload[0] != cursorVersion || len(payload)-1 > MaxCursorPositionBytes {
		return nil, invalidCursorError()
	}
	got, err := cursorEncoding.DecodeString(sig)
	if err != nil {
		return nil, invalidCursorError()
	}
	for _, k := range c.keys {
		if hmac.Equal(got, c.mac(k, scope, payload)) {
			return payload[1:], nil
		}
	}
	return nil, invalidCursorError()
}
