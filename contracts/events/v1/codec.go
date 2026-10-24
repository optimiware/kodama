package eventsv1

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/optimiware/kodama/internal/domain"
)

// Decode strictly decodes and validates one encoded envelope.
//
// The parser rejects oversized input, invalid UTF-8, duplicate object keys at
// any depth, nesting beyond MaxDepth, trailing data and non-integer numbers.
// Additional members are tolerated (compatible optional additions), but the
// schema version and event type are checked first so an event from a newer
// contract is reported as unsupported and never as a success.
func Decode(data []byte) (Envelope, error) {
	if len(data) > MaxEnvelopeBytes {
		return Envelope{}, fmt.Errorf("%w: %d bytes (limit %d)", ErrTooLarge, len(data), MaxEnvelopeBytes)
	}
	tree, err := parseStrict(data)
	if err != nil {
		return Envelope{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	obj, ok := tree.(map[string]any)
	if !ok {
		return Envelope{}, fmt.Errorf("%w: envelope must be an object", ErrMalformed)
	}
	if v, present := obj["schemaVersion"]; present {
		if n, isNum := v.(json.Number); isNum {
			if i, err := strconv.ParseInt(n.String(), 10, 64); err == nil && i != SchemaVersion {
				return Envelope{}, fmt.Errorf("%w: %d (supported: %d)", ErrUnsupportedVersion, i, SchemaVersion)
			}
		}
	}
	if _, present := obj["causationId"]; !present {
		return Envelope{}, fmt.Errorf("%w: causationId must be present (explicit null when none)", ErrInvalid)
	}
	if v, present := obj["projectId"]; present {
		if s, _ := v.(string); s == "" {
			return Envelope{}, fmt.Errorf("%w: projectId must be omitted for tenant-wide events, never empty", ErrInvalid)
		}
	}
	var env Envelope
	if err := env.UnmarshalJSON(data); err != nil {
		return Envelope{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := env.Validate(); err != nil {
		return Envelope{}, err
	}
	return env, nil
}

// UnmarshalJSON decodes the wire form without validating it; use Decode.
func (e *Envelope) UnmarshalJSON(data []byte) error {
	var w wireEnvelope
	if err := unmarshalExact(data, &w); err != nil {
		return err
	}
	var at time.Time
	if w.OccurredAt != "" {
		if err := matchStamp("occurredAt", w.OccurredAt); err != nil {
			return err
		}
		parsed, err := time.Parse(time.RFC3339Nano, w.OccurredAt)
		if err != nil {
			return err
		}
		at = parsed.UTC()
	}
	*e = Envelope{
		ID: w.ID, SchemaVersion: w.SchemaVersion, Type: w.Type, OccurredAt: at,
		TenantID: domain.TenantID(w.TenantID), ProjectID: domain.ProjectID(w.ProjectID),
		Resource: w.Resource, Actor: w.Actor, CorrelationID: w.CorrelationID,
		CausationID: w.CausationID, Data: w.Data,
	}
	return nil
}

// Encode validates and marshals the envelope.
func Encode(e Envelope) ([]byte, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	out, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	if len(out) > MaxEnvelopeBytes {
		return nil, fmt.Errorf("%w: %d bytes (limit %d)", ErrTooLarge, len(out), MaxEnvelopeBytes)
	}
	return out, nil
}

// DecodeData returns the typed payload of a validated envelope.
func DecodeData(e Envelope) (any, error) {
	var target any
	switch e.Type {
	case TypeWorkItemCreated:
		target = new(WorkItemCreated)
	case TypeWorkItemUpdated:
		target = new(WorkItemUpdated)
	case TypeWorkItemTransitioned:
		target = new(WorkItemTransitioned)
	case TypeWorkspaceStateChanged:
		target = new(WorkspaceStateChanged)
	case TypeWorkspaceCheckpoint:
		target = new(WorkspaceCheckpointCreated)
	case TypeRunCreated:
		target = new(RunCreated)
	case TypeRunStateChanged:
		target = new(RunStateChanged)
	case TypeStepAttemptStateChange:
		target = new(StepAttemptStateChanged)
	case TypeGateRequested:
		target = new(GateRequested)
	case TypeGateDecided:
		target = new(GateDecided)
	case TypeGateInvalidated:
		target = new(GateInvalidated)
	case TypeDeploymentStateChanged:
		target = new(DeploymentStateChanged)
	case TypeMembershipChanged:
		target = new(MembershipChanged)
	case TypeProjectSettingsChanged:
		target = new(ProjectSettingsChanged)
	case TypeConfigurationChanged:
		target = new(ConfigurationChanged)
	case TypeUsageRecorded:
		target = new(UsageRecorded)
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedType, truncate(e.Type, 64))
	}
	if err := unmarshalExact(e.Data, target); err != nil {
		return nil, fmt.Errorf("%w: data: %v", ErrInvalid, err)
	}
	return target, nil
}

// unmarshalExact is json.Unmarshal with exact member names. encoding/json
// matches object members to struct fields case-insensitively, which would let an
// additional member such as "ID" or "Kind" (an ignorable compatible addition
// under the schema) override the real "id" or "kind", or stand in for a missing
// required member. Here only members spelled exactly like a field's wire name
// reach the decoder, at every nesting level; every other member is, as the
// contract says, an ignored addition.
func unmarshalExact(data []byte, v any) error {
	t := reflect.TypeOf(v)
	if t == nil {
		return fmt.Errorf("unmarshalExact: nil target")
	}
	clean, err := exactMembers(data, t)
	if err != nil {
		return err
	}
	return json.Unmarshal(clean, v)
}

// exactMembers returns raw with every object member that does not exactly
// match a json-tagged field of t removed, recursively through structs,
// pointers, slices and arrays. Values of other kinds (including raw messages)
// pass through untouched.
func exactMembers(raw []byte, t reflect.Type) ([]byte, error) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		if !isJSONContainer(raw, '{') {
			return raw, nil
		}
		var members map[string]json.RawMessage
		if err := json.Unmarshal(raw, &members); err != nil {
			return nil, err
		}
		fields := map[string]reflect.Type{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			fields[name] = f.Type
		}
		kept := make(map[string]json.RawMessage, len(members))
		for name, value := range members {
			ft, known := fields[name]
			if !known {
				continue
			}
			cleaned, err := exactMembers(value, ft)
			if err != nil {
				return nil, err
			}
			kept[name] = cleaned
		}
		return json.Marshal(kept)
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 || !isJSONContainer(raw, '[') {
			return raw, nil
		}
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, err
		}
		for i, item := range items {
			cleaned, err := exactMembers(item, t.Elem())
			if err != nil {
				return nil, err
			}
			items[i] = cleaned
		}
		return json.Marshal(items)
	}
	return raw, nil
}

func isJSONContainer(raw []byte, open byte) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == open
}

// Canonical returns the canonical bytes of the encoded envelope: object keys
// sorted, no insignificant whitespace, strings escaped without HTML escaping.
// Equal events always have equal canonical bytes regardless of the member
// order or spacing of the original encoding.
func Canonical(e Envelope) ([]byte, error) {
	raw, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	tree, err := parseStrict(raw)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := writeCanonical(&buf, tree); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Digest is "sha256:" and the lowercase hex SHA-256 of Canonical. A repeated
// append of the same event ID is idempotent only when the digest is equal.
func Digest(e Envelope) (string, error) {
	c, err := Canonical(e)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(c)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func writeCanonical(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		buf.WriteString(strconv.FormatBool(t))
	case json.Number:
		buf.WriteString(t.String())
	case string:
		var s bytes.Buffer
		enc := json.NewEncoder(&s)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(t); err != nil {
			return err
		}
		buf.Write(bytes.TrimRight(s.Bytes(), "\n"))
	case []any:
		buf.WriteByte('[')
		for i, item := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, k); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := writeCanonical(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("unsupported value %T", v)
	}
	return nil
}

// parseStrict parses one JSON document into generic values with the bounded,
// unambiguous rules of Decode.
func parseStrict(data []byte) (any, error) {
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("invalid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := parseValue(dec, 1)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing data after the document")
	}
	return v, nil
}

func parseValue(dec *json.Decoder, depth int) (any, error) {
	if depth > MaxDepth {
		return nil, fmt.Errorf("nesting deeper than %d", MaxDepth)
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := map[string]any{}
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, _ := keyTok.(string)
				if _, dup := obj[key]; dup {
					return nil, fmt.Errorf("duplicate key %q", truncate(key, 64))
				}
				val, err := parseValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				obj[key] = val
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return obj, nil
		case '[':
			arr := []any{}
			for dec.More() {
				val, err := parseValue(dec, depth+1)
				if err != nil {
					return nil, err
				}
				arr = append(arr, val)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return arr, nil
		}
		return nil, fmt.Errorf("unexpected delimiter %v", t)
	case json.Number:
		s := t.String()
		if strings.ContainsAny(s, ".eE") {
			return nil, fmt.Errorf("number %q is not an integer", truncate(s, 32))
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n > MaxSafeInteger || n < -MaxSafeInteger || s == "-0" {
			return nil, fmt.Errorf("number %q is outside the safe integer range", truncate(s, 32))
		}
		return t, nil
	default:
		return tok, nil
	}
}
