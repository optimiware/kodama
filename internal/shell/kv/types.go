// Package kv implements the project-scoped, content-addressed immutable KV
// store capability.
//
// Keys are always derived server-side from SHA-256(bytes) in the canonical
// form "kv:sha256:<64-lowercase-hex>". Callers never choose arbitrary keys.
// Once written under (tenant_id, project_id, key), bytes are strictly
// write-once and immutable: putting identical bytes in the same project
// converges idempotently to the existing record without mutation, while
// identical bytes in another project or tenant remain completely isolated.
package kv

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"mime"
	"strings"
	"time"

	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/shell/identity"
	"github.com/optimiware/kodema/internal/shell/tenancy"
)

const (
	// KeyPrefix is the canonical prefix of every content-addressed KV key.
	KeyPrefix = "kv:sha256:"
	// KeyLength is the exact byte length of a canonical KV key ("kv:sha256:" + 64 hex chars).
	KeyLength = len(KeyPrefix) + 64
	// MaxValueBytes bounds one KV object payload (16 MiB).
	MaxValueBytes = 16 << 20
	// MaxContentTypeBytes bounds the optional MIME content_type string.
	MaxContentTypeBytes = 256
	// DefaultContentType is used for binary/base64 puts when content_type is omitted.
	DefaultContentType = "application/octet-stream"
	// DefaultTextContentType is used for UTF-8 text puts when content_type is omitted.
	DefaultTextContentType = "text/plain; charset=utf-8"
)

// Key is one canonical server-derived content-addressed KV key.
type Key string

// KeyFor derives the canonical content-addressed key from raw payload bytes.
func KeyFor(data []byte) Key {
	sum := sha256.Sum256(data)
	return Key(KeyPrefix + hex.EncodeToString(sum[:]))
}

// ParseKey validates and parses a canonical KV key ("kv:sha256:<64-lowercase-hex>").
// Uppercase hex, wrong prefixes, wrong lengths, whitespace, control characters
// and path-traversal tokens are refused with invalid_input.
func ParseKey(raw string) (Key, error) {
	if len(raw) != KeyLength || !strings.HasPrefix(raw, KeyPrefix) {
		return "", fault.New(fault.InvalidInput, fmt.Sprintf("kv key must match %s<64-lowercase-hex>", KeyPrefix))
	}
	digest := raw[len(KeyPrefix):]
	for index := 0; index < len(digest); index++ {
		ch := digest[index]
		if (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') {
			continue
		}
		return "", fault.New(fault.InvalidInput, fmt.Sprintf("kv key must match %s<64-lowercase-hex>", KeyPrefix))
	}
	return Key(raw), nil
}

// RowID derives the deterministic, store-valid row identity for one
// (tenant_id, project_id, key) tuple. Two projects storing identical bytes
// receive distinct row identities so primary keys never collide across scopes.
func RowID(scope tenancy.Scope, key Key) (string, error) {
	if err := scope.Validate(); err != nil {
		return "", err
	}
	if !scope.ProjectScoped() {
		return "", fault.New(fault.InvalidInput, "kv objects require a project scope")
	}
	canonical, err := ParseKey(string(key))
	if err != nil {
		return "", err
	}
	hasher := sha256.New()
	hasher.Write([]byte(scope.TenantID))
	hasher.Write([]byte{0})
	hasher.Write([]byte(scope.ProjectID))
	hasher.Write([]byte{0})
	hasher.Write([]byte(canonical))
	return "kvo-" + hex.EncodeToString(hasher.Sum(nil)), nil
}

// NormalizeContentType validates an optional MIME content_type string or
// returns fallback when raw is empty.
func NormalizeContentType(raw, fallback string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		if fallback == "" {
			return DefaultContentType, nil
		}
		return fallback, nil
	}
	if len(trimmed) > MaxContentTypeBytes {
		return "", fault.New(fault.InvalidInput, fmt.Sprintf("content_type exceeds %d bytes", MaxContentTypeBytes))
	}
	for index := 0; index < len(trimmed); index++ {
		ch := trimmed[index]
		if ch < 0x20 || ch >= 0x7f {
			return "", fault.New(fault.InvalidInput, "content_type must contain only printable ASCII characters")
		}
	}
	mediaType, params, err := mime.ParseMediaType(trimmed)
	if err != nil || mediaType == "" {
		return "", fault.New(fault.InvalidInput, "content_type is not a valid MIME media type")
	}
	formatted := mime.FormatMediaType(mediaType, params)
	if formatted == "" {
		return "", fault.New(fault.InvalidInput, "content_type is not a valid MIME media type")
	}
	if len(formatted) > MaxContentTypeBytes {
		return "", fault.New(fault.InvalidInput, fmt.Sprintf("content_type exceeds %d bytes", MaxContentTypeBytes))
	}
	return formatted, nil
}

// Metadata describes one stored immutable KV object without its payload bytes.
type Metadata struct {
	Key         Key
	Scope       tenancy.Scope
	ContentType string
	SizeBytes   int64
	CreatedBy   id.PrincipalID
	CreatedAt   time.Time
}

// Object is one immutable KV object together with its exact stored bytes.
type Object struct {
	Metadata Metadata
	Data     []byte
}

// PutCommand stores bounded immutable bytes under a project scope.
type PutCommand struct {
	Scope       tenancy.Scope
	Actor       identity.Principal
	Data        []byte
	ContentType string
}

// PutResult reports the content-addressed metadata and whether an identical
// object already existed in the project scope.
type PutResult struct {
	Metadata Metadata
	Existed  bool
}

// GetCommand reads one immutable KV object by its canonical content key.
type GetCommand struct {
	Scope tenancy.Scope
	Actor identity.Principal
	Key   Key
}
