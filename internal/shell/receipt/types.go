// Package receipt declares scoped receipt identity, idempotency content
// normalization and the durable committed result.
//
// Receipt identity is (tenant, project, principal, operation, caller key).
// Equal normalized content returns the original result; changed content returns
// changed_idempotency_content. Normalization is for operation content identity
// only — never for the exact-YAML execution hash, which is always SHA-256 of
// the admitted bytes.
//
// JSON media types are canonicalized with sorted object keys and literal
// numbers, so object key order and insignificant whitespace do not change the
// fingerprint while an integer literal and a float literal remain distinct.
// Every other media type is compared as exact bytes. Absent and null stay
// distinct because they are different JSON documents.
package receipt

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
)

// Key is the scoped idempotency identity of one operation invocation. ProjectID
// is empty for tenant-scoped operations.
type Key struct {
	TenantID    id.TenantID    `json:"tenant_id"`
	ProjectID   id.ProjectID   `json:"project_id,omitempty"`
	PrincipalID id.PrincipalID `json:"principal_id"`
	Operation   string         `json:"operation"`
	CallerKey   string         `json:"caller_key"`
}

// Validate checks the receipt identity fields.
func (key Key) Validate() error {
	if _, err := id.ParseTenantID(string(key.TenantID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "receipt tenant id is invalid", err)
	}
	if key.ProjectID != "" {
		if _, err := id.ParseProjectID(string(key.ProjectID)); err != nil {
			return fault.Wrap(fault.InvalidInput, "receipt project id is invalid", err)
		}
	}
	if _, err := id.ParsePrincipalID(string(key.PrincipalID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "receipt principal id is invalid", err)
	}
	if key.Operation == "" {
		return fault.New(fault.InvalidInput, "receipt operation is empty")
	}
	if key.CallerKey == "" {
		return fault.New(fault.InvalidInput, "receipt caller key is empty")
	}
	return nil
}

// Content is one request content body for idempotency comparison.
type Content struct {
	MediaType string
	Bytes     []byte
}

// Record is one durable committed receipt. ContentHash pins the normalized
// request content; Result is the durable result body returned to identical
// retries. ExecutionID retains the committed execution when the operation
// created or selected one. SnapshotHash pins the selected definition when the
// operation admitted work.
type Record struct {
	ID           id.ReceiptID   `json:"id"`
	Key          Key            `json:"key"`
	ContentHash  string         `json:"content_hash"`
	Result       []byte         `json:"result,omitempty"`
	ExecutionID  id.ExecutionID `json:"execution_id,omitempty"`
	SnapshotHash *[32]byte      `json:"snapshot_hash,omitempty"`
	At           time.Time      `json:"at"`
}

// Normalize returns the canonical content bytes for the media type: canonical
// JSON for a JSON media type, exact bytes otherwise.
func Normalize(content Content) ([]byte, error) {
	mediaType, err := normalizeMediaType(content.MediaType)
	if err != nil {
		return nil, err
	}
	if isJSONMediaType(mediaType) {
		return canonicalJSON(content.Bytes)
	}
	return append([]byte(nil), content.Bytes...), nil
}

// Fingerprint returns the content-addressed identity of one content body. The
// normalized media type participates, so the same bytes interpreted as JSON and
// as opaque binary content are different request content.
func Fingerprint(content Content) (string, error) {
	mediaType, err := normalizeMediaType(content.MediaType)
	if err != nil {
		return "", err
	}
	normalized, err := Normalize(content)
	if err != nil {
		return "", err
	}
	hasher := sha256.New()
	hasher.Write([]byte(mediaType))
	hasher.Write([]byte{0})
	hasher.Write(normalized)
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), nil
}

func normalizeMediaType(value string) (string, error) {
	base := value
	if index := strings.IndexByte(base, ';'); index >= 0 {
		base = base[:index]
	}
	base = strings.ToLower(strings.TrimSpace(base))
	if base == "" {
		return "", fault.New(fault.InvalidInput, "receipt content media type is empty")
	}
	return base, nil
}

func isJSONMediaType(mediaType string) bool {
	return mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
}

func canonicalJSON(data []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var buffer bytes.Buffer
	if err := writeCanonicalValue(decoder, &buffer); err != nil {
		return nil, fault.Wrap(fault.InvalidInput, "receipt content is not valid JSON", err)
	}
	if token, err := decoder.Token(); err != io.EOF {
		if err != nil {
			return nil, fault.Wrap(fault.InvalidInput, "receipt content has trailing bytes", err)
		}
		_ = token
		return nil, fault.New(fault.InvalidInput, "receipt content has more than one JSON value")
	}
	return buffer.Bytes(), nil
}

func writeCanonicalValue(decoder *json.Decoder, buffer *bytes.Buffer) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	return writeCanonicalToken(decoder, buffer, token)
}

func writeCanonicalToken(decoder *json.Decoder, buffer *bytes.Buffer, token json.Token) error {
	switch value := token.(type) {
	case json.Delim:
		switch value {
		case '{':
			return writeCanonicalObject(decoder, buffer)
		case '[':
			return writeCanonicalArray(decoder, buffer)
		default:
			return fmt.Errorf("unexpected JSON delimiter %q", value)
		}
	case string:
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		buffer.Write(encoded)
		return nil
	case json.Number:
		buffer.WriteString(value.String())
		return nil
	case bool:
		if value {
			buffer.WriteString("true")
		} else {
			buffer.WriteString("false")
		}
		return nil
	case nil:
		buffer.WriteString("null")
		return nil
	default:
		return fmt.Errorf("unexpected JSON token %T", token)
	}
}

func writeCanonicalObject(decoder *json.Decoder, buffer *bytes.Buffer) error {
	values := make(map[string][]byte)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := keyToken.(string)
		if !ok {
			return fmt.Errorf("JSON object key is not a string")
		}
		if _, duplicate := values[key]; duplicate {
			return fmt.Errorf("JSON object key %q is duplicated", key)
		}
		var value bytes.Buffer
		if err := writeCanonicalValue(decoder, &value); err != nil {
			return err
		}
		values[key] = value.Bytes()
	}
	closing, err := decoder.Token()
	if err != nil {
		return err
	}
	if closing != json.Delim('}') {
		return fmt.Errorf("JSON object is not closed")
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	buffer.WriteByte('{')
	for index, key := range keys {
		if index > 0 {
			buffer.WriteByte(',')
		}
		encodedKey, err := json.Marshal(key)
		if err != nil {
			return err
		}
		buffer.Write(encodedKey)
		buffer.WriteByte(':')
		buffer.Write(values[key])
	}
	buffer.WriteByte('}')
	return nil
}

func writeCanonicalArray(decoder *json.Decoder, buffer *bytes.Buffer) error {
	buffer.WriteByte('[')
	first := true
	for decoder.More() {
		if !first {
			buffer.WriteByte(',')
		}
		first = false
		if err := writeCanonicalValue(decoder, buffer); err != nil {
			return err
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return err
	}
	if closing != json.Delim(']') {
		return fmt.Errorf("JSON array is not closed")
	}
	buffer.WriteByte(']')
	return nil
}
