package runtimev1

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// Patterns are the string syntaxes shared by the Go validators and
// schema.json. A test requires the schema's patterns to equal these.
var Patterns = map[string]string{
	"ID":                  domain.IDPattern,
	"Digest":              `^sha256:[0-9a-f]{64}$`,
	"GitCommit":           `^([0-9a-f]{40}|[0-9a-f]{64})$`,
	"Timestamp":           `^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?Z$`,
	"Name":                `^[a-z][a-z0-9_-]{0,62}$`,
	"StepName":            `^[a-z][a-z0-9-]{0,62}$`,
	"SafeName":            `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`,
	"ActionKey":           `^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`,
	"ReasonCode":          `^[a-z][a-z0-9_]{0,63}$`,
	"ImageRef":            `^[^\s@]+@sha256:[0-9a-f]{64}$`,
	"RepositoryURL":       `^https://[^\s@/?#]+(/[^\s@?#]*)?$`,
	"BranchRef":           `^refs/heads/[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`,
	"SecretRef":           `^secret://[a-z][a-z0-9-]{0,62}/[a-z][a-z0-9-]{0,62}$`,
	"Nonce":               `^[A-Za-z0-9_-]{22,64}$`,
	"EnvName":             `^[A-Za-z_][A-Za-z0-9_]{0,127}$`,
	"KubernetesName":      `^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`,
	"UID":                 `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`,
	"OutputPath":          `^/kodama/outputs(/[A-Za-z0-9_-][A-Za-z0-9._-]*)+$`,
	"Base64":              `^[A-Za-z0-9+/]*={0,2}$`,
	"AdmissionCredential": `^[A-Za-z0-9._~+/=-]{16,512}$`,
	"TokenValue":          `^[A-Za-z0-9._~+/=-]{16,1000}$`,
	"ClientVersion":       `^[0-9A-Za-z.+-]{1,64}$`,
	"MediaType":           `^[A-Za-z0-9][A-Za-z0-9!#$&^_.+-]{0,126}/[A-Za-z0-9][A-Za-z0-9!#$&^_.+-]{0,126}$`,
	"RegistryRef":         `^[^\s@]+$`,
	"UploadURL":           `^https?://[^\s]+$`,
	"RequirementID":       `^[A-Z]{2,4}-A?[0-9]{2,3}$`,
	"DecisionID":          `^[COP]-[0-9]{2}$`,
	"CheckID":             `^[a-z][a-z0-9-]{0,62}$`,
	"SourcePath":          `^(\.?[A-Za-z0-9_-][A-Za-z0-9._-]*)(/\.?[A-Za-z0-9_-][A-Za-z0-9._-]*)*$`,
}

var compiled = func() map[string]*regexp.Regexp {
	m := make(map[string]*regexp.Regexp, len(Patterns))
	for name, p := range Patterns {
		m[name] = regexp.MustCompile(p)
	}
	return m
}()

// ValidationError reports the first structural or semantic problem found.
type ValidationError struct {
	Path    string
	Message string
}

func (e *ValidationError) Error() string { return e.Path + ": " + e.Message }

func fail(path, format string, args ...any) error {
	return &ValidationError{Path: path, Message: fmt.Sprintf(format, args...)}
}

func matches(path, pattern, value string) error {
	if !compiled[pattern].MatchString(value) {
		return fail(path, "must match %s syntax", pattern)
	}
	return nil
}

func wrap(path string, err error) error {
	if err == nil {
		return nil
	}
	return fail(path, "%v", err)
}

// Digest is "sha256:" followed by 64 lowercase hexadecimal digits.
type Digest string

func (d Digest) Validate() error { return matches("digest", "Digest", string(d)) }

// GitCommit is a lowercase hexadecimal Git object ID (SHA-1 or SHA-256).
type GitCommit string

func (c GitCommit) Validate() error { return matches("commit", "GitCommit", string(c)) }

// Timestamp is a UTC RFC 3339 time with a literal "Z" suffix.
type Timestamp string

func (t Timestamp) Validate() error {
	if err := matches("timestamp", "Timestamp", string(t)); err != nil {
		return err
	}
	if _, err := time.Parse(time.RFC3339Nano, string(t)); err != nil {
		return fail("timestamp", "not a valid time: %v", err)
	}
	return nil
}

// Time parses a validated timestamp.
func (t Timestamp) Time() (time.Time, error) { return time.Parse(time.RFC3339Nano, string(t)) }

type valueKind uint8

const (
	kindInvalid valueKind = iota
	kindString
	kindBool
	kindInteger
)

// Value is a scalar run input or output: a string, a boolean or an integer.
// An artifact or OCI output is carried as its artifact ID string; the
// declaration, not the JSON type, says which. Integers never pass through
// floating point.
type Value struct {
	kind valueKind
	s    string
	b    bool
	i    int64
}

func StringValue(s string) Value { return Value{kind: kindString, s: s} }
func BoolValue(b bool) Value     { return Value{kind: kindBool, b: b} }
func IntValue(i int64) Value     { return Value{kind: kindInteger, i: i} }

func (v Value) IsString() bool  { return v.kind == kindString }
func (v Value) IsBool() bool    { return v.kind == kindBool }
func (v Value) IsInteger() bool { return v.kind == kindInteger }

// String returns the string value, or "" for a boolean or integer.
func (v Value) String() string { return v.s }

func (v Value) Validate() error {
	switch v.kind {
	case kindString:
		if len(v.s) > 4096 {
			return fail("value", "string longer than 4096 bytes")
		}
	case kindInteger:
		if v.i > domain.MaxSafeInteger || v.i < -domain.MaxSafeInteger {
			return fail("value", "integer outside +/-(2^53-1)")
		}
	case kindBool:
	default:
		return fail("value", "missing scalar value")
	}
	return nil
}

func (v Value) MarshalJSON() ([]byte, error) {
	switch v.kind {
	case kindString:
		return json.Marshal(v.s)
	case kindBool:
		return json.Marshal(v.b)
	case kindInteger:
		return []byte(strconv.FormatInt(v.i, 10)), nil
	}
	return nil, fmt.Errorf("runtimev1: empty Value")
}

func (v *Value) UnmarshalJSON(data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("runtimev1: empty scalar")
	}
	switch c := data[0]; {
	case c == '"':
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*v = StringValue(s)
	case string(data) == "true" || string(data) == "false":
		*v = BoolValue(string(data) == "true")
	case c == '-' || (c >= '0' && c <= '9'):
		n, err := strconv.ParseInt(string(data), 10, 64)
		if err != nil {
			return fmt.Errorf("runtimev1: scalar must be an integer: %v", err)
		}
		*v = IntValue(n)
	default:
		return fmt.Errorf("runtimev1: scalar must be a string, boolean or integer")
	}
	return nil
}
