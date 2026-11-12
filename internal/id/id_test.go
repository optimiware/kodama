package id

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// Every identity the Kernel vocabulary needs is a distinct named string type.
// Aliases would collapse scopes (for example an action id silently accepted as
// an execution id), so the contract asserts distinct reflect types and names.
func TestIDsAreDistinctNamedTypes(t *testing.T) {
	values := []any{
		ExecutionID(""),
		ActionID(""),
		TaskID(""),
		TenantID(""),
		ProjectID(""),
		PrincipalID(""),
		EventID(""),
		ReceiptID(""),
		OperationID(""),
		WorkloadID(""),
		WorkspaceID(""),
	}

	seen := make(map[reflect.Type]string, len(values))
	for _, value := range values {
		typ := reflect.TypeOf(value)
		if typ.Kind() != reflect.String {
			t.Fatalf("%T: kind = %v, want string", value, typ.Kind())
		}
		if typ.PkgPath() != "github.com/optimiware/kodama/internal/id" {
			t.Fatalf("%T: package = %q, want github.com/optimiware/kodama/internal/id", value, typ.PkgPath())
		}
		if typ.Name() == "" {
			t.Fatalf("%T: unnamed type; identities must be named types", value)
		}
		if previous, ok := seen[typ]; ok {
			t.Fatalf("identity type %q is shared by two values (previous %q)", typ.Name(), previous)
		}
		seen[typ] = typ.Name()
	}
	if len(seen) != len(values) {
		t.Fatalf("distinct identity types = %d, want %d", len(seen), len(values))
	}

	// Conversion is explicit; the string payload is preserved verbatim.
	if got := string(ExecutionID("exec-1")); got != "exec-1" {
		t.Fatalf("ExecutionID payload = %q, want %q", got, "exec-1")
	}
}

type idParser struct {
	name  string
	parse func(string) (string, error)
}

func idParsers() []idParser {
	return []idParser{
		{"execution", func(s string) (string, error) { v, err := ParseExecutionID(s); return string(v), err }},
		{"action", func(s string) (string, error) { v, err := ParseActionID(s); return string(v), err }},
		{"task", func(s string) (string, error) { v, err := ParseTaskID(s); return string(v), err }},
		{"tenant", func(s string) (string, error) { v, err := ParseTenantID(s); return string(v), err }},
		{"project", func(s string) (string, error) { v, err := ParseProjectID(s); return string(v), err }},
		{"principal", func(s string) (string, error) { v, err := ParsePrincipalID(s); return string(v), err }},
		{"event", func(s string) (string, error) { v, err := ParseEventID(s); return string(v), err }},
		{"receipt", func(s string) (string, error) { v, err := ParseReceiptID(s); return string(v), err }},
		{"operation", func(s string) (string, error) { v, err := ParseOperationID(s); return string(v), err }},
		{"workload", func(s string) (string, error) { v, err := ParseWorkloadID(s); return string(v), err }},
		{"workspace", func(s string) (string, error) { v, err := ParseWorkspaceID(s); return string(v), err }},
	}
}

func TestParseIDsRoundTripValidValues(t *testing.T) {
	valid := []string{
		"a",
		"A",
		"0",
		"exec-1",
		"ws_2",
		"0f8fad5b-d9cb-469f-a165-70867728950e",
		strings.Repeat("a", MaxIDLength),
	}
	for _, parser := range idParsers() {
		for _, input := range valid {
			got, err := parser.parse(input)
			if err != nil {
				t.Fatalf("%s.Parse(%q) error = %v, want nil", parser.name, input, err)
			}
			if got != input {
				t.Fatalf("%s.Parse(%q) = %q, want round-trip", parser.name, input, got)
			}
		}
	}
}

func TestParseIDsRejectUnsafeValues(t *testing.T) {
	invalid := []string{
		"",
		" ",
		"a b",
		"a/b",
		"a\\b",
		"../x",
		"..",
		".",
		"a\nb",
		"a\tb",
		"a\x00b",
		"a:b",
		strings.Repeat("a", MaxIDLength+1),
	}
	for _, parser := range idParsers() {
		for _, input := range invalid {
			got, err := parser.parse(input)
			if err == nil {
				t.Fatalf("%s.Parse(%q) = %q, want error", parser.name, input, got)
			}
			if !errors.Is(err, ErrInvalidID) {
				t.Fatalf("%s.Parse(%q) error = %v, want ErrInvalidID", parser.name, input, err)
			}
		}
	}
}

func TestIDLengthBoundary(t *testing.T) {
	atLimit := strings.Repeat("a", MaxIDLength)
	if _, err := ParseExecutionID(atLimit); err != nil {
		t.Fatalf("ParseExecutionID(%d bytes) error = %v, want nil", len(atLimit), err)
	}
	overLimit := strings.Repeat("a", MaxIDLength+1)
	if _, err := ParseExecutionID(overLimit); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("ParseExecutionID(%d bytes) error = %v, want ErrInvalidID", len(overLimit), err)
	}
}
