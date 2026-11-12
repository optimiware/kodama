package factory

// Guards for the read-only stored approval evidence.
//
// The protected-ref verifier stores the request assignment it actually used on
// the verification row. The factory must read and decode that exact durable
// value; it must never infer the reviewer role from the committed policy or
// from a caller-supplied string. These tests pin the decoder against the real
// stored JSON shape, including every missing or malformed form.

import (
	"strings"
	"testing"
)

// TestVerificationApprovalDecodesRequestedRole proves the stored
// `role/reviewer` assignment decodes to the exact role and that a missing or
// malformed stored assignment is refused instead of defaulting.
func TestVerificationApprovalDecodesRequestedRole(t *testing.T) {
	t.Run("stored role assignment decodes exactly", func(t *testing.T) {
		assignment, err := decodeRequestedApproval([]byte(`{"kind":"role","role":"reviewer"}`))
		if err != nil {
			t.Fatalf("decode stored role assignment: %v", err)
		}
		if assignment.Kind != "role" || assignment.Role != "reviewer" || assignment.ServiceID != "" {
			t.Fatalf("decoded assignment = %+v; want role/reviewer", assignment)
		}
		if assignment.String() != "role/reviewer" {
			t.Fatalf("assignment string = %q; want role/reviewer", assignment.String())
		}
		if err := assignment.Validate(); err != nil {
			t.Fatalf("decoded assignment is invalid: %v", err)
		}
	})

	t.Run("stored service assignment decodes exactly", func(t *testing.T) {
		assignment, err := decodeRequestedApproval([]byte(`{"kind":"service","service_id":"service-i04-git"}`))
		if err != nil {
			t.Fatalf("decode stored service assignment: %v", err)
		}
		if assignment.String() != "service/service-i04-git" {
			t.Fatalf("assignment string = %q; want service/service-i04-git", assignment.String())
		}
	})

	missing := []struct {
		name string
		raw  string
	}{
		{name: "empty", raw: ""},
		{name: "whitespace", raw: " \n\t "},
		{name: "null", raw: "null"},
		{name: "empty object", raw: "{}"},
	}
	for _, testCase := range missing {
		t.Run("missing "+testCase.name+" is refused", func(t *testing.T) {
			if _, err := decodeRequestedApproval([]byte(testCase.raw)); err == nil {
				t.Fatalf("decode %q succeeded; want a missing-evidence refusal", testCase.raw)
			}
		})
	}

	malformed := []struct {
		name string
		raw  string
	}{
		{name: "not JSON", raw: `{`},
		{name: "unknown field", raw: `{"kind":"role","role":"reviewer","policy":"repo"}`},
		{name: "unknown kind", raw: `{"kind":"magic","role":"reviewer"}`},
		{name: "role with service", raw: `{"kind":"role","role":"reviewer","service_id":"service-x"}`},
		{name: "service with role", raw: `{"kind":"service","role":"reviewer","service_id":"service-x"}`},
		{name: "unknown role", raw: `{"kind":"role","role":"superuser"}`},
		{name: "empty service", raw: `{"kind":"service"}`},
		{name: "trailing data", raw: `{"kind":"role","role":"reviewer"} {}`},
	}
	for _, testCase := range malformed {
		t.Run("malformed "+testCase.name+" is refused", func(t *testing.T) {
			_, err := decodeRequestedApproval([]byte(testCase.raw))
			if err == nil {
				t.Fatalf("decode %q succeeded; want a refusal", testCase.raw)
			}
		})
	}

	// The rendered diagnostic value is the canonical assignment grammar, never
	// a raw JSON blob, so a test failure names the assignment without echoing
	// any stored bytes.
	assignment, err := decodeRequestedApproval([]byte(`{"kind":"role","role":"reviewer"}`))
	if err != nil {
		t.Fatalf("decode canonical role assignment: %v", err)
	}
	if strings.Contains(assignment.String(), "{") {
		t.Fatalf("assignment string %q leaked stored JSON", assignment.String())
	}
}
