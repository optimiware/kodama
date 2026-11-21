package api

import (
	"encoding/base64"
	"mime"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// KVKeyPrefix is the canonical prefix of every content-addressed KV key.
	KVKeyPrefix = "kv:sha256:"
	// KVKeyLength is the exact byte length of a canonical KV key ("kv:sha256:" + 64 lowercase hex).
	KVKeyLength = len(KVKeyPrefix) + 64
)

// ValidKVKey reports whether value is a canonical server-derived KV key
// matching "kv:sha256:<64-lowercase-hex>".
func ValidKVKey(value string) bool {
	if len(value) != KVKeyLength || !strings.HasPrefix(value, KVKeyPrefix) {
		return false
	}
	digest := value[len(KVKeyPrefix):]
	for index := 0; index < len(digest); index++ {
		ch := digest[index]
		if (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') {
			continue
		}
		return false
	}
	return true
}

func validKVContentType(value string) bool {
	if value == "" || len(value) > MaxKVContentTypeBytes {
		return false
	}
	for index := 0; index < len(value); index++ {
		ch := value[index]
		if ch < 0x20 || ch >= 0x7f {
			return false
		}
	}
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && mediaType != ""
}

// KVPutRequest is the typed input of kv.put. Exactly one of Text (UTF-8) or
// DataBase64 (standard base64) must be supplied; empty content is permitted.
type KVPutRequest struct {
	ProjectID   string  `json:"project_id,omitempty" schema:"id"`
	Text        *string `json:"text,omitempty"`
	DataBase64  *string `json:"data_base64,omitempty"`
	ContentType string  `json:"content_type,omitempty"`
}

// Validate checks the optional project identity, exclusive payload selection,
// decoded byte bound (16 MiB) and optional MIME content_type.
func (request KVPutRequest) Validate() error {
	if request.ProjectID != "" && !validID(request.ProjectID) {
		return invalid("project_id %q is not a valid identity", request.ProjectID)
	}
	if _, _, err := request.DecodeBytes(); err != nil {
		return err
	}
	if request.ContentType != "" && !validKVContentType(request.ContentType) {
		return invalid("content_type %q is not a valid MIME media type", request.ContentType)
	}
	return nil
}

// DecodeBytes extracts the exact payload bytes and reports whether the caller
// submitted the UTF-8 text form (true) or the base64 form (false).
func (request KVPutRequest) DecodeBytes() ([]byte, bool, error) {
	hasText := request.Text != nil
	hasBase64 := request.DataBase64 != nil
	switch {
	case !hasText && !hasBase64:
		return nil, false, invalid("kv.put requires exactly one of text or data_base64")
	case hasText && hasBase64:
		return nil, false, invalid("kv.put accepts only one of text or data_base64")
	case hasText:
		text := *request.Text
		if !utf8.ValidString(text) {
			return nil, true, invalid("kv.put text must be valid UTF-8")
		}
		if len(text) > MaxKVValueBytes {
			return nil, true, invalid("kv.put payload is %d bytes; at most %d are allowed", len(text), MaxKVValueBytes)
		}
		return []byte(text), true, nil
	default:
		decoded, err := base64.StdEncoding.DecodeString(*request.DataBase64)
		if err != nil {
			return nil, false, invalid("kv.put data_base64 is not valid standard base64")
		}
		if len(decoded) > MaxKVValueBytes {
			return nil, false, invalid("kv.put payload is %d bytes; at most %d are allowed", len(decoded), MaxKVValueBytes)
		}
		return decoded, false, nil
	}
}

// KVPutResult is the typed result of kv.put.
type KVPutResult struct {
	Key         string    `json:"key"`
	ProjectID   string    `json:"project_id"`
	SizeBytes   int64     `json:"size_bytes"`
	ContentType string    `json:"content_type"`
	CreatedAt   time.Time `json:"created_at"`
	Existed     bool      `json:"existed"`
}

// Validate checks the server-derived KV key, project identity, size bound,
// content type and creation timestamp.
func (result KVPutResult) Validate() error {
	if !ValidKVKey(result.Key) {
		return invalid("kv key %q is invalid", result.Key)
	}
	if !validID(result.ProjectID) {
		return invalid("project_id %q is not a valid identity", result.ProjectID)
	}
	if result.SizeBytes < 0 || result.SizeBytes > MaxKVValueBytes {
		return invalid("size_bytes %d is outside 0..%d", result.SizeBytes, MaxKVValueBytes)
	}
	if !validKVContentType(result.ContentType) {
		return invalid("content_type %q is not a valid MIME media type", result.ContentType)
	}
	if result.CreatedAt.IsZero() {
		return invalid("created_at is zero")
	}
	return nil
}

// KVGetRequest is the typed input of kv.get.
type KVGetRequest struct {
	ProjectID string `json:"project_id,omitempty" schema:"id"`
	Key       string `json:"key" schema:"kv_key"`
}

// Validate checks the optional project identity and required canonical KV key.
func (request KVGetRequest) Validate() error {
	if request.ProjectID != "" && !validID(request.ProjectID) {
		return invalid("project_id %q is not a valid identity", request.ProjectID)
	}
	if !ValidKVKey(request.Key) {
		return invalid("kv key %q must match %s<64-lowercase-hex>", request.Key, KVKeyPrefix)
	}
	return nil
}

// KVGetResult is the typed result of kv.get. DataBase64 always carries the
// exact stored bytes; Text is additionally populated for valid UTF-8 payloads
// up to MaxKVInlineTextBytes without NUL bytes.
type KVGetResult struct {
	Key         string    `json:"key"`
	ProjectID   string    `json:"project_id"`
	SizeBytes   int64     `json:"size_bytes"`
	ContentType string    `json:"content_type"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
	DataBase64  string    `json:"data_base64"`
	Text        *string   `json:"text,omitempty"`
}

// Validate checks all KV metadata fields, base64 payload integrity, and
// consistency between Text and DataBase64 when Text is present.
func (result KVGetResult) Validate() error {
	if !ValidKVKey(result.Key) {
		return invalid("kv key %q is invalid", result.Key)
	}
	if !validID(result.ProjectID) {
		return invalid("project_id %q is not a valid identity", result.ProjectID)
	}
	if result.SizeBytes < 0 || result.SizeBytes > MaxKVValueBytes {
		return invalid("size_bytes %d is outside 0..%d", result.SizeBytes, MaxKVValueBytes)
	}
	if !validKVContentType(result.ContentType) {
		return invalid("content_type %q is not a valid MIME media type", result.ContentType)
	}
	if !validID(result.CreatedBy) {
		return invalid("created_by %q is not a valid identity", result.CreatedBy)
	}
	if result.CreatedAt.IsZero() {
		return invalid("created_at is zero")
	}
	decoded, err := base64.StdEncoding.DecodeString(result.DataBase64)
	if err != nil {
		return invalid("data_base64 is not valid standard base64")
	}
	if int64(len(decoded)) != result.SizeBytes {
		return invalid("data_base64 decodes to %d bytes; size_bytes is %d", len(decoded), result.SizeBytes)
	}
	if result.Text != nil && *result.Text != string(decoded) {
		return invalid("text does not match data_base64")
	}
	return nil
}
