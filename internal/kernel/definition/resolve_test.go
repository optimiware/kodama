package definition

import (
	"reflect"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/id"
)

func TestResolveSubjectResolvesLiteralsInputsAndSucceededOutputs(t *testing.T) {
	subject := NewSubject(map[string]SubjectValue{
		"operation": {Kind: SubjectLiteral, Literal: "git.ref.update"},
		"resource":  {Kind: SubjectReference, Reference: Reference{Input: true, Key: "repository"}},
		"candidate": {Kind: SubjectReference, Reference: Reference{Action: "build", Key: "candidate"}},
	})
	inputs := map[string]string{"repository": "example/repo"}
	succeeded := map[id.ActionName]map[id.OutputKey]string{
		"build": {"candidate": "abc123"},
	}

	resolved, err := ResolveSubject(subject, inputs, succeeded)
	if err != nil {
		t.Fatalf("ResolveSubject() error = %v, want nil", err)
	}
	if resolved.Len() != 3 {
		t.Fatalf("resolved.Len() = %d, want 3", resolved.Len())
	}

	want := map[string]SubjectValue{
		"operation": {Kind: SubjectLiteral, Literal: "git.ref.update"},
		"resource":  {Kind: SubjectLiteral, Literal: "example/repo"},
		"candidate": {Kind: SubjectLiteral, Literal: "abc123"},
	}
	if got := resolved.Fields(); !reflect.DeepEqual(got, want) {
		t.Fatalf("resolved.Fields() = %#v, want %#v", got, want)
	}

	original := subject.Fields()
	if original["resource"].Kind != SubjectReference || original["candidate"].Kind != SubjectReference {
		t.Fatalf("ResolveSubject mutated the admitted subject: %#v", original)
	}
}

// TestResolveSubjectDistinguishesEmptyValueFromMissingValue pins the contract
// difference between a supplied empty string and an unavailable value.
func TestResolveSubjectDistinguishesEmptyValueFromMissingValue(t *testing.T) {
	inputSubject := NewSubject(map[string]SubjectValue{
		"resource": {Kind: SubjectReference, Reference: Reference{Input: true, Key: "repository"}},
	})
	resolved, err := ResolveSubject(inputSubject, map[string]string{"repository": ""}, nil)
	if err != nil {
		t.Fatalf("empty input value: ResolveSubject() error = %v, want nil", err)
	}
	if got := resolved.Fields()["resource"]; got.Kind != SubjectLiteral || got.Literal != "" {
		t.Fatalf("empty input value resolved to %#v, want a literal empty string", got)
	}

	outputSubject := NewSubject(map[string]SubjectValue{
		"candidate": {Kind: SubjectReference, Reference: Reference{Action: "build", Key: "candidate"}},
	})
	resolved, err = ResolveSubject(outputSubject, nil, map[id.ActionName]map[id.OutputKey]string{
		"build": {"candidate": ""},
	})
	if err != nil {
		t.Fatalf("empty output value: ResolveSubject() error = %v, want nil", err)
	}
	if got := resolved.Fields()["candidate"]; got.Kind != SubjectLiteral || got.Literal != "" {
		t.Fatalf("empty output value resolved to %#v, want a literal empty string", got)
	}

	if _, err := ResolveSubject(inputSubject, nil, nil); err == nil {
		t.Fatal("missing input value resolved without an error")
	} else {
		assertInvalidInput(t, err, "missing input value")
	}
	if _, err := ResolveSubject(inputSubject, map[string]string{}, nil); err == nil {
		t.Fatal("empty input map resolved a declared reference without an error")
	} else {
		assertInvalidInput(t, err, "empty input map")
	}
	if _, err := ResolveSubject(outputSubject, nil, map[id.ActionName]map[id.OutputKey]string{
		"build": {"artifact": "abc123"},
	}); err == nil {
		t.Fatal("undeclared output key resolved without an error")
	} else {
		assertInvalidInput(t, err, "undeclared output key")
	}
}

// TestResolveSubjectRefusesOutputsAbsentFromSucceededMap proves resolution sees
// only the supplied earlier-succeeded map. A failed, pending or still-staged
// action has no entry, so its declared output is unreachable.
func TestResolveSubjectRefusesOutputsAbsentFromSucceededMap(t *testing.T) {
	subject := NewSubject(map[string]SubjectValue{
		"candidate": {Kind: SubjectReference, Reference: Reference{Action: "test", Key: "candidate"}},
	})
	succeeded := map[id.ActionName]map[id.OutputKey]string{
		"build": {"candidate": "abc123"},
	}

	if _, err := ResolveSubject(subject, nil, succeeded); err == nil {
		t.Fatal("resolved an output of an action absent from the succeeded map")
	} else {
		assertInvalidInput(t, err, "failed or pending action output")
	}

	for name, outputs := range map[string]map[id.ActionName]map[id.OutputKey]string{
		"nil inner map":   {"test": nil},
		"empty inner map": {"test": {}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ResolveSubject(subject, nil, outputs); err == nil {
				t.Fatalf("%s: resolved an unavailable action output", name)
			} else {
				assertInvalidInput(t, err, name)
			}
		})
	}
}

func TestResolveSubjectRefusesMalformedReferences(t *testing.T) {
	cases := []struct {
		name  string
		value SubjectValue
	}{
		{
			"literal carries a reference",
			SubjectValue{
				Kind:      SubjectLiteral,
				Literal:   "git.ref.update",
				Reference: Reference{Input: true, Key: "repository"},
			},
		},
		{
			"reference carries a literal",
			SubjectValue{
				Kind:      SubjectReference,
				Literal:   "git.ref.update",
				Reference: Reference{Input: true, Key: "repository"},
			},
		},
		{
			"unknown kind",
			SubjectValue{Kind: SubjectKind("template"), Literal: "git.ref.update"},
		},
		{
			"reference with two sources",
			SubjectValue{
				Kind:      SubjectReference,
				Reference: Reference{Input: true, Action: "build", Key: "candidate"},
			},
		},
		{
			"reference with no source",
			SubjectValue{Kind: SubjectReference, Reference: Reference{Key: "candidate"}},
		},
		{
			"reference with empty key",
			SubjectValue{Kind: SubjectReference, Reference: Reference{Input: true}},
		},
		{
			"reference with unsafe input key",
			SubjectValue{Kind: SubjectReference, Reference: Reference{Input: true, Key: "repository/../x"}},
		},
		{
			"reference with unsafe action name",
			SubjectValue{Kind: SubjectReference, Reference: Reference{Action: "../build", Key: "candidate"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			subject := NewSubject(map[string]SubjectValue{"field": tc.value})
			if _, err := ResolveSubject(subject, map[string]string{"repository": "example/repo"}, map[id.ActionName]map[id.OutputKey]string{
				"build": {"candidate": "abc123"},
			}); err == nil {
				t.Fatalf("%s: ResolveSubject accepted a malformed subject value", tc.name)
			} else {
				assertInvalidInput(t, err, tc.name)
			}
		})
	}
}

// TestResolveSubjectIsImmutable mutates every supplied source after resolution
// and through the returned accessor; the resolved subject must not change.
func TestResolveSubjectIsImmutable(t *testing.T) {
	sourceFields := map[string]SubjectValue{
		"resource":  {Kind: SubjectReference, Reference: Reference{Input: true, Key: "repository"}},
		"candidate": {Kind: SubjectReference, Reference: Reference{Action: "build", Key: "candidate"}},
	}
	subject := NewSubject(sourceFields)
	inputs := map[string]string{"repository": "example/repo"}
	succeeded := map[id.ActionName]map[id.OutputKey]string{
		"build": {"candidate": "abc123"},
	}

	resolved, err := ResolveSubject(subject, inputs, succeeded)
	if err != nil {
		t.Fatalf("ResolveSubject() error = %v, want nil", err)
	}

	sourceFields["resource"] = SubjectValue{Kind: SubjectLiteral, Literal: "mutated"}
	inputs["repository"] = "mutated"
	delete(inputs, "repository")
	succeeded["build"]["candidate"] = "mutated"
	delete(succeeded, "build")

	fields := resolved.Fields()
	if got := fields["resource"]; got.Kind != SubjectLiteral || got.Literal != "example/repo" {
		t.Fatalf("resolved input changed after source mutation: %#v", got)
	}
	if got := fields["candidate"]; got.Kind != SubjectLiteral || got.Literal != "abc123" {
		t.Fatalf("resolved output changed after source mutation: %#v", got)
	}

	fields["resource"] = SubjectValue{Kind: SubjectLiteral, Literal: "mutated-through-accessor"}
	if got := resolved.Fields()["resource"]; got.Literal != "example/repo" {
		t.Fatalf("resolved subject mutated through Fields(): %#v", got)
	}

	if got := subject.Fields()["resource"]; got.Kind != SubjectReference || !got.Reference.Input {
		t.Fatalf("original admitted subject mutated by resolution: %#v", got)
	}
}

// TestResolveSubjectIsDeterministicUnderConcurrency resolves the same immutable
// subject from the same supplied maps in parallel. Results must be identical and
// the read-only inputs must stay untouched.
func TestResolveSubjectIsDeterministicUnderConcurrency(t *testing.T) {
	subject := NewSubject(map[string]SubjectValue{
		"resource":  {Kind: SubjectReference, Reference: Reference{Input: true, Key: "repository"}},
		"candidate": {Kind: SubjectReference, Reference: Reference{Action: "build", Key: "candidate"}},
	})
	inputs := map[string]string{"repository": "example/repo"}
	succeeded := map[id.ActionName]map[id.OutputKey]string{
		"build": {"candidate": "abc123"},
	}

	const workers = 16
	results := make([]map[string]SubjectValue, workers)
	errs := make([]error, workers)

	var wait sync.WaitGroup
	for index := range workers {
		wait.Go(func() {
			resolved, err := ResolveSubject(subject, inputs, succeeded)
			errs[index] = err
			if err == nil {
				results[index] = resolved.Fields()
			}
		})
	}
	wait.Wait()

	for index := range workers {
		if errs[index] != nil {
			t.Fatalf("worker %d: ResolveSubject() error = %v", index, errs[index])
		}
		if !reflect.DeepEqual(results[index], results[0]) {
			t.Fatalf("worker %d resolved %#v, want %#v", index, results[index], results[0])
		}
	}

	if got := inputs["repository"]; got != "example/repo" {
		t.Fatalf("resolution mutated supplied inputs: %q", got)
	}
	if got := succeeded["build"]["candidate"]; got != "abc123" {
		t.Fatalf("resolution mutated supplied outputs: %q", got)
	}
}
