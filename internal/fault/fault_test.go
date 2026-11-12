package fault

import (
	"errors"
	"strings"
	"testing"
)

func expectedCodes() map[Class]Code {
	return map[Class]Code{
		InvalidInput:              CodeInvalidInput,
		Unauthenticated:           CodeUnauthenticated,
		Forbidden:                 CodeForbidden,
		NotFound:                  CodeNotFound,
		VersionConflict:           CodeVersionConflict,
		ChangedIdempotencyContent: CodeChangedIdempotencyContent,
		DeadlineExpired:           CodeDeadlineExpired,
		UnsupportedCapability:     CodeUnsupportedCapability,
		DependencyUnavailable:     CodeDependencyUnavailable,
		UncertainExternalEffect:   CodeUncertainExternalEffect,
		InternalFailure:           CodeInternalFailure,
	}
}

func TestClassesAndCodesAreStable(t *testing.T) {
	expected := expectedCodes()
	if got := len(Classes()); got != len(expected) {
		t.Fatalf("Classes() length = %d, want %d", got, len(expected))
	}
	if got := len(Codes()); got != len(expected) {
		t.Fatalf("Codes() length = %d, want %d", got, len(expected))
	}

	for class, code := range expected {
		if !class.Valid() {
			t.Fatalf("class %q reports invalid", class)
		}
		if string(class) == "" {
			t.Fatalf("class %q is empty", class)
		}
		gotCode, ok := CodeFor(class)
		if !ok {
			t.Fatalf("CodeFor(%q) not found", class)
		}
		if gotCode != code {
			t.Fatalf("CodeFor(%q) = %q, want %q", class, gotCode, code)
		}
		if !gotCode.Valid() {
			t.Fatalf("code %q reports invalid", gotCode)
		}
		gotClass, ok := ClassFor(code)
		if !ok {
			t.Fatalf("ClassFor(%q) not found", code)
		}
		if gotClass != class {
			t.Fatalf("ClassFor(%q) = %q, want %q", code, gotClass, class)
		}
	}

	if Class("bogus").Valid() {
		t.Fatal("Class(bogus).Valid() = true, want false")
	}
	if Code("bogus").Valid() {
		t.Fatal("Code(bogus).Valid() = true, want false")
	}
	if _, ok := CodeFor(Class("bogus")); ok {
		t.Fatal("CodeFor(bogus) found, want not found")
	}
	if _, ok := ClassFor(Code("bogus")); ok {
		t.Fatal("ClassFor(bogus) found, want not found")
	}
}

func TestClassCodesAreOneToOne(t *testing.T) {
	seen := make(map[Code]Class)
	for _, class := range Classes() {
		code, ok := CodeFor(class)
		if !ok {
			t.Fatalf("CodeFor(%q) not found", class)
		}
		if previous, ok := seen[code]; ok {
			t.Fatalf("code %q maps to both %q and %q", code, previous, class)
		}
		seen[code] = class
	}
	if len(seen) != len(expectedCodes()) {
		t.Fatalf("distinct codes = %d, want %d", len(seen), len(expectedCodes()))
	}
}

func TestErrorKeepsPrivateCauseOutOfPublicText(t *testing.T) {
	cause := errors.New("dial tcp 10.0.0.1:443: token=super-secret-value")
	err := Wrap(DependencyUnavailable, "dependency unavailable", cause)

	if err.Class() != DependencyUnavailable {
		t.Fatalf("Class() = %q, want %q", err.Class(), DependencyUnavailable)
	}
	if err.Code() != CodeDependencyUnavailable {
		t.Fatalf("Code() = %q, want %q", err.Code(), CodeDependencyUnavailable)
	}
	if err.Message() != "dependency unavailable" {
		t.Fatalf("Message() = %q, want public message", err.Message())
	}
	if strings.Contains(err.Error(), "super-secret-value") {
		t.Fatalf("Error() leaked private cause: %q", err.Error())
	}
	if strings.Contains(err.Message(), "super-secret-value") {
		t.Fatalf("Message() leaked private cause: %q", err.Message())
	}
	if !errors.Is(err, cause) {
		t.Fatal("errors.Is(err, cause) = false, want true; cause must stay reachable internally")
	}
	if errors.Unwrap(err) != cause {
		t.Fatal("errors.Unwrap(err) != cause")
	}

	var target *Error
	if !errors.As(err, &target) {
		t.Fatal("errors.As(*fault.Error) = false")
	}
	if target != err {
		t.Fatal("errors.As resolved a different *fault.Error")
	}
}

func TestNewErrorHasNoCause(t *testing.T) {
	err := New(InvalidInput, "action name is invalid")
	if err.Error() == "" {
		t.Fatal("Error() is empty")
	}
	if !strings.Contains(err.Error(), "action name is invalid") {
		t.Fatalf("Error() = %q, want it to include the public message", err.Error())
	}
	if err.Unwrap() != nil {
		t.Fatalf("Unwrap() = %v, want nil", err.Unwrap())
	}
}

func TestClassOfAndCodeOf(t *testing.T) {
	err := Wrap(VersionConflict, "execution version changed", errors.New("stale"))
	class, ok := ClassOf(err)
	if !ok || class != VersionConflict {
		t.Fatalf("ClassOf(err) = (%q, %v), want (%q, true)", class, ok, VersionConflict)
	}
	code, ok := CodeOf(err)
	if !ok || code != CodeVersionConflict {
		t.Fatalf("CodeOf(err) = (%q, %v), want (%q, true)", code, ok, CodeVersionConflict)
	}

	if _, ok := ClassOf(errors.New("plain")); ok {
		t.Fatal("ClassOf(plain error) = true, want false")
	}
	if _, ok := CodeOf(errors.New("plain")); ok {
		t.Fatal("CodeOf(plain error) = true, want false")
	}
}
