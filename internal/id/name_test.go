package id

import (
	"errors"
	"strings"
	"testing"
)

type nameParser struct {
	name  string
	parse func(string) (string, error)
}

func TestActionNames(t *testing.T) {
	valid := []string{
		"a",
		"implement",
		"build-2",
		"a-b-c",
		strings.Repeat("a", MaxNameLength),
	}
	for _, input := range valid {
		got, err := ParseActionName(input)
		if err != nil {
			t.Fatalf("ParseActionName(%q) error = %v, want nil", input, err)
		}
		if string(got) != input {
			t.Fatalf("ParseActionName(%q) = %q, want round-trip", input, got)
		}
	}

	invalid := []string{
		"",
		"A",
		"1abc",
		"-abc",
		"abc-",
		"a_b",
		"a.b",
		"a/b",
		"../x",
		"a b",
		"a\nb",
		strings.Repeat("a", MaxNameLength+1),
	}
	for _, input := range invalid {
		got, err := ParseActionName(input)
		if err == nil {
			t.Fatalf("ParseActionName(%q) = %q, want error", input, got)
		}
		if !errors.Is(err, ErrInvalidName) {
			t.Fatalf("ParseActionName(%q) error = %v, want ErrInvalidName", input, err)
		}
	}
}

func TestEnvNamesRejectReservedContext(t *testing.T) {
	valid := []string{"A", "LOG_LEVEL", "KODAMA_X", strings.Repeat("A", MaxKeyLength)}
	for _, input := range valid {
		got, err := ParseEnvName(input)
		if err != nil {
			t.Fatalf("ParseEnvName(%q) error = %v, want nil", input, err)
		}
		if string(got) != input {
			t.Fatalf("ParseEnvName(%q) = %q, want round-trip", input, got)
		}
	}

	reserved := []string{"KODAMA_URL", "KODAMA_EXECUTION", "KODAMA_ACTION", "KODAMA_TASK"}
	for _, input := range reserved {
		if _, err := ParseEnvName(input); !errors.Is(err, ErrInvalidName) {
			t.Fatalf("ParseEnvName(%q) error = %v, want ErrInvalidName for reserved context", input, err)
		}
	}

	invalid := []string{
		"",
		"a",
		"_A",
		"A-B",
		"A B",
		"A/B",
		strings.Repeat("A", MaxKeyLength+1),
	}
	for _, input := range invalid {
		if _, err := ParseEnvName(input); !errors.Is(err, ErrInvalidName) {
			t.Fatalf("ParseEnvName(%q) error = %v, want ErrInvalidName", input, err)
		}
	}
}

func TestSecretNamesRejectReservedToken(t *testing.T) {
	valid := []string{"TOKEN", "TEST_DB_PASSWORD", strings.Repeat("A", MaxKeyLength)}
	for _, input := range valid {
		got, err := ParseSecretName(input)
		if err != nil {
			t.Fatalf("ParseSecretName(%q) error = %v, want nil", input, err)
		}
		if string(got) != input {
			t.Fatalf("ParseSecretName(%q) = %q, want round-trip", input, got)
		}
	}

	if _, err := ParseSecretName("KODAMA_TOKEN"); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("ParseSecretName(KODAMA_TOKEN) error = %v, want ErrInvalidName", err)
	}

	invalid := []string{
		"",
		"token",
		"_TOKEN",
		"TOKEN-1",
		"TOKEN 1",
		"../TOKEN",
		strings.Repeat("A", MaxKeyLength+1),
	}
	for _, input := range invalid {
		if _, err := ParseSecretName(input); !errors.Is(err, ErrInvalidName) {
			t.Fatalf("ParseSecretName(%q) error = %v, want ErrInvalidName", input, err)
		}
	}
}

func TestInputAndOutputKeys(t *testing.T) {
	parsers := []nameParser{
		{"input", func(s string) (string, error) { v, err := ParseInputKey(s); return string(v), err }},
		{"output", func(s string) (string, error) { v, err := ParseOutputKey(s); return string(v), err }},
	}

	valid := []string{
		"a",
		"repository",
		"target_ref",
		"target-ref",
		"k0",
		strings.Repeat("a", MaxKeyLength),
	}
	for _, parser := range parsers {
		for _, input := range valid {
			got, err := parser.parse(input)
			if err != nil {
				t.Fatalf("%s key Parse(%q) error = %v, want nil", parser.name, input, err)
			}
			if got != input {
				t.Fatalf("%s key Parse(%q) = %q, want round-trip", parser.name, input, got)
			}
		}
	}

	invalid := []string{
		"",
		"A",
		"1a",
		"_a",
		"-a",
		"a.b",
		"a/b",
		"../x",
		"a b",
		"a\nb",
		strings.Repeat("a", MaxKeyLength+1),
	}
	for _, parser := range parsers {
		for _, input := range invalid {
			got, err := parser.parse(input)
			if err == nil {
				t.Fatalf("%s key Parse(%q) = %q, want error", parser.name, input, got)
			}
			if !errors.Is(err, ErrInvalidName) {
				t.Fatalf("%s key Parse(%q) error = %v, want ErrInvalidName", parser.name, input, err)
			}
		}
	}
}
