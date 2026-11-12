package definition

// These tests pin the behavior of validate.go:
//
//	func Validate(p Pipeline) error
//	func ParseReference(from string) (Reference, error)
//	func ResolveSubject(subject Subject, inputs map[string]string, succeeded map[id.ActionName]map[id.OutputKey]string) (Subject, error)
//
// Validate is pure: it reads no clock, filesystem or external service, never
// mutates its argument, and refuses with a typed fault.InvalidInput fault that
// carries the stable invalid_input code. ParseReference accepts exactly the two
// direct lookup forms of S03 §2 and nothing else. ResolveSubject reads only the
// supplied input values and the supplied map of earlier succeeded action
// outputs, so failed, pending and staged outputs are structurally unreachable.
//
// These tests exercise only exported declarations from types.go plus the three
// entry points above. They construct typed values directly; no YAML parser
// or I/O is involved.

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
)

const k01PinnedImage = "registry.example.com/tool@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// k01ContainerAction builds a structurally valid container declaration so each
// test can change exactly one field.
func k01ContainerAction(name string, outputs ...id.OutputKey) Action {
	return Action{
		Name:    id.ActionName(name),
		Timeout: time.Minute,
		Container: &Container{
			Image:    k01PinnedImage,
			Commands: []string{"run"},
			Outputs:  outputs,
		},
	}
}

// k01RequestAction builds a structurally valid request declaration.
func k01RequestAction(name string, subject map[string]SubjectValue, outputs ...id.OutputKey) Action {
	return Action{
		Name:    id.ActionName(name),
		Timeout: time.Minute,
		Request: &Request{
			Assignment: "role/reviewer",
			Subject:    NewSubject(subject),
			Outputs:    outputs,
		},
	}
}

// assertInvalidInput requires a typed invalid_input fault with a non-empty
// public message. A plain error, a different class or a nil error all fail.
func assertInvalidInput(t *testing.T, err error, label string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: error = nil, want invalid_input refusal", label)
	}
	class, ok := fault.ClassOf(err)
	if !ok {
		t.Fatalf("%s: error %v is not a typed fault", label, err)
	}
	if class != fault.InvalidInput {
		t.Fatalf("%s: class = %q, want %q (%v)", label, class, fault.InvalidInput, err)
	}
	code, ok := fault.CodeOf(err)
	if !ok || code != fault.CodeInvalidInput {
		t.Fatalf("%s: code = %q, want %q (%v)", label, code, fault.CodeInvalidInput, err)
	}
	if message, ok := fault.MessageOf(err); !ok || strings.TrimSpace(message) == "" {
		t.Fatalf("%s: public fault message = %q, want a non-empty diagnostic", label, message)
	}
}

func assertValidateOK(t *testing.T, pipeline Pipeline, label string) {
	t.Helper()
	if err := Validate(pipeline); err != nil {
		t.Fatalf("%s: Validate() error = %v, want nil", label, err)
	}
}

func TestValidateAcceptsSupportedDeclarations(t *testing.T) {
	longName := strings.Repeat("a", id.MaxNameLength)
	longKey := strings.Repeat("a", id.MaxKeyLength)
	longEnv := strings.Repeat("A", id.MaxKeyLength)

	override := k01ContainerAction("build")
	override.Container.Env = []EnvVar{{Name: "LOG_LEVEL", Value: "debug"}}

	opaqueAssignment := k01RequestAction("review", nil)
	opaqueAssignment.Request.Assignment = "service/opaque-assignment"

	lowerBound := k01ContainerAction("build")
	lowerBound.Timeout = time.Minute
	upperBound := k01RequestAction("review", nil)
	upperBound.Timeout = 30 * 24 * time.Hour
	betweenBounds := k01ContainerAction("build")
	betweenBounds.Timeout = 90 * time.Second

	cases := []struct {
		name     string
		pipeline Pipeline
	}{
		{"single container", Pipeline{Actions: []Action{k01ContainerAction("build")}}},
		{"single request", Pipeline{Actions: []Action{k01RequestAction("review", nil)}}},
		{
			"pipeline env overridden by action env",
			Pipeline{
				Env:     []EnvVar{{Name: "LOG_LEVEL", Value: "info"}},
				Actions: []Action{override},
			},
		},
		{
			"container then request with direct references",
			Pipeline{
				Inputs: []id.InputKey{"repository"},
				Actions: []Action{
					k01ContainerAction("build", "candidate"),
					k01RequestAction("review", map[string]SubjectValue{
						"operation": {Kind: SubjectLiteral, Literal: "git.ref.update"},
						"resource":  {Kind: SubjectReference, Reference: Reference{Input: true, Key: "repository"}},
						"candidate": {Kind: SubjectReference, Reference: Reference{Action: "build", Key: "candidate"}},
					}),
				},
			},
		},
		{
			"request references earlier request output",
			Pipeline{Actions: []Action{
				k01RequestAction("first-review", nil, "decision"),
				k01RequestAction("second-review", map[string]SubjectValue{
					"previous": {Kind: SubjectReference, Reference: Reference{Action: "first-review", Key: "decision"}},
				}),
			}},
		},
		{"assignment stays opaque", Pipeline{Actions: []Action{opaqueAssignment}}},
		{"timeout lower bound", Pipeline{Actions: []Action{lowerBound}}},
		{"timeout upper bound", Pipeline{Actions: []Action{upperBound}}},
		{"timeout between bounds", Pipeline{Actions: []Action{betweenBounds}}},
		{
			"maximum name and key bounds",
			Pipeline{
				Inputs: []id.InputKey{id.InputKey(longKey)},
				Env:    []EnvVar{{Name: id.EnvName(longEnv), Value: "value"}},
				Actions: []Action{{
					Name:    id.ActionName(longName),
					Timeout: 30 * 24 * time.Hour,
					Container: &Container{
						Image:    k01PinnedImage,
						Commands: []string{"run"},
						Env:      []EnvVar{{Name: id.EnvName(longEnv), Value: "value"}},
						Secrets:  []id.SecretName{id.SecretName(longEnv)},
						Outputs:  []id.OutputKey{id.OutputKey(longKey)},
					},
				}},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertValidateOK(t, tc.pipeline, tc.name)
		})
	}
}

func TestValidateRefusesEmptyAndMixedActionKinds(t *testing.T) {
	noKind := Action{Name: "build", Timeout: time.Minute}
	mixedKind := k01ContainerAction("build")
	mixedKind.Request = &Request{Assignment: "role/reviewer"}

	cases := []struct {
		name     string
		pipeline Pipeline
	}{
		{"no actions", Pipeline{}},
		{"inputs without actions", Pipeline{Inputs: []id.InputKey{"repository"}}},
		{"action without a kind", Pipeline{Actions: []Action{noKind}}},
		{"action with both kinds", Pipeline{Actions: []Action{mixedKind}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertInvalidInput(t, Validate(tc.pipeline), tc.name)
		})
	}
}

func TestValidateRefusesDuplicateActionNames(t *testing.T) {
	cases := []struct {
		name     string
		pipeline Pipeline
	}{
		{
			"same container name twice",
			Pipeline{Actions: []Action{k01ContainerAction("build"), k01ContainerAction("build")}},
		},
		{
			"same request name twice",
			Pipeline{Actions: []Action{k01RequestAction("review", nil), k01RequestAction("review", nil)}},
		},
		{
			"same name across kinds",
			Pipeline{Actions: []Action{k01ContainerAction("build"), k01RequestAction("build", nil)}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertInvalidInput(t, Validate(tc.pipeline), tc.name)
		})
	}
}

func TestValidateRefusesUnsafeActionNames(t *testing.T) {
	names := []string{
		"",
		"Build",
		"1build",
		"-build",
		"build-",
		"build--x",
		"build_x",
		"build.x",
		"build/x",
		"../build",
		"build x",
		"build\nx",
		"build\x00x",
		strings.Repeat("a", id.MaxNameLength+1),
	}
	for _, name := range names {
		t.Run("action name "+strings.ReplaceAll(name, "\x00", "NUL"), func(t *testing.T) {
			pipeline := Pipeline{Actions: []Action{k01ContainerAction(name)}}
			assertInvalidInput(t, Validate(pipeline), "action name "+name)
		})
	}
}

func TestValidateRefusesReservedEnvironmentNames(t *testing.T) {
	for _, reserved := range id.ReservedEnvNames() {
		t.Run("pipeline "+reserved, func(t *testing.T) {
			pipeline := Pipeline{
				Env:     []EnvVar{{Name: id.EnvName(reserved), Value: "value"}},
				Actions: []Action{k01ContainerAction("build")},
			}
			assertInvalidInput(t, Validate(pipeline), "pipeline env "+reserved)
		})
		t.Run("action "+reserved, func(t *testing.T) {
			action := k01ContainerAction("build")
			action.Container.Env = []EnvVar{{Name: id.EnvName(reserved), Value: "value"}}
			assertInvalidInput(t, Validate(Pipeline{Actions: []Action{action}}), "action env "+reserved)
		})
	}
}

func TestValidateRefusesInvalidEnvironmentNames(t *testing.T) {
	names := []string{
		"",
		"log_level",
		"_LOG",
		"LOG-LEVEL",
		"LOG/LEVEL",
		"LOG.LEVEL",
		"../LOG",
		"LOG\nLEVEL",
		"LOG\x00LEVEL",
		strings.Repeat("A", id.MaxKeyLength+1),
	}
	for _, name := range names {
		t.Run("env name "+strings.ReplaceAll(name, "\x00", "NUL"), func(t *testing.T) {
			pipeline := Pipeline{
				Env:     []EnvVar{{Name: id.EnvName(name), Value: "value"}},
				Actions: []Action{k01ContainerAction("build")},
			}
			assertInvalidInput(t, Validate(pipeline), "pipeline env "+name)

			action := k01ContainerAction("build")
			action.Container.Env = []EnvVar{{Name: id.EnvName(name), Value: "value"}}
			assertInvalidInput(t, Validate(Pipeline{Actions: []Action{action}}), "action env "+name)
		})
	}
}

func TestValidateRefusesReservedSecretName(t *testing.T) {
	action := k01ContainerAction("build")
	action.Container.Secrets = []id.SecretName{id.SecretTokenName}
	assertInvalidInput(t, Validate(Pipeline{Actions: []Action{action}}), "reserved secret "+id.SecretTokenName)
}

func TestValidateRefusesInvalidSecretNames(t *testing.T) {
	names := []string{
		"",
		"db_password",
		"DB-PASSWORD",
		"DB/PASSWORD",
		"DB.PASSWORD",
		"../DB_PASSWORD",
		"DB PASSWORD",
		"DB\nPASSWORD",
		"DB\x00PASSWORD",
		strings.Repeat("A", id.MaxKeyLength+1),
	}
	for _, name := range names {
		t.Run("secret name "+strings.ReplaceAll(name, "\x00", "NUL"), func(t *testing.T) {
			action := k01ContainerAction("build")
			action.Container.Secrets = []id.SecretName{id.SecretName(name)}
			assertInvalidInput(t, Validate(Pipeline{Actions: []Action{action}}), "secret name "+name)
		})
	}
}

func TestValidateRefusesInvalidInputKeys(t *testing.T) {
	keys := []string{
		"",
		"Repository",
		"1repository",
		"_repository",
		"repository/../x",
		"repository key",
		"repository\nkey",
		"repository\x00key",
		strings.Repeat("a", id.MaxKeyLength+1),
	}
	for _, key := range keys {
		t.Run("input key "+strings.ReplaceAll(key, "\x00", "NUL"), func(t *testing.T) {
			pipeline := Pipeline{
				Inputs:  []id.InputKey{id.InputKey(key)},
				Actions: []Action{k01ContainerAction("build")},
			}
			assertInvalidInput(t, Validate(pipeline), "input key "+key)
		})
	}
}

func TestValidateRefusesInvalidOutputKeys(t *testing.T) {
	keys := []string{
		"",
		"Candidate",
		"1candidate",
		"_candidate",
		"candidate/../x",
		"candidate key",
		"candidate\nkey",
		"candidate\x00key",
		strings.Repeat("a", id.MaxKeyLength+1),
	}
	for _, key := range keys {
		t.Run("output key "+strings.ReplaceAll(key, "\x00", "NUL"), func(t *testing.T) {
			action := k01ContainerAction("build")
			action.Container.Outputs = []id.OutputKey{id.OutputKey(key)}
			assertInvalidInput(t, Validate(Pipeline{Actions: []Action{action}}), "output key "+key)
		})
	}
}

func TestValidateRefusesDuplicateDeclaredKeys(t *testing.T) {
	duplicatePipelineEnv := Pipeline{
		Env:     []EnvVar{{Name: "LOG_LEVEL", Value: "info"}, {Name: "LOG_LEVEL", Value: "debug"}},
		Actions: []Action{k01ContainerAction("build")},
	}
	duplicateActionEnv := k01ContainerAction("build")
	duplicateActionEnv.Container.Env = []EnvVar{{Name: "APP_ENV", Value: "a"}, {Name: "APP_ENV", Value: "b"}}
	duplicateSecrets := k01ContainerAction("build")
	duplicateSecrets.Container.Secrets = []id.SecretName{"DB_PASSWORD", "DB_PASSWORD"}
	duplicateOutputs := k01ContainerAction("build", "candidate", "candidate")
	duplicateRequestOutputs := k01RequestAction("review", nil, "decision", "decision")

	cases := []struct {
		name     string
		pipeline Pipeline
	}{
		{"duplicate pipeline env", duplicatePipelineEnv},
		{"duplicate action env", Pipeline{Actions: []Action{duplicateActionEnv}}},
		{"duplicate inputs", Pipeline{Inputs: []id.InputKey{"repository", "repository"}, Actions: []Action{k01ContainerAction("build")}}},
		{"duplicate secrets", Pipeline{Actions: []Action{duplicateSecrets}}},
		{"duplicate container outputs", Pipeline{Actions: []Action{duplicateOutputs}}},
		{"duplicate request outputs", Pipeline{Actions: []Action{duplicateRequestOutputs}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertInvalidInput(t, Validate(tc.pipeline), tc.name)
		})
	}
}

func TestValidateTimeoutBounds(t *testing.T) {
	accepted := []time.Duration{
		time.Minute,
		90 * time.Second,
		30 * 24 * time.Hour,
	}
	for _, timeout := range accepted {
		t.Run("accepted "+timeout.String(), func(t *testing.T) {
			action := k01ContainerAction("build")
			action.Timeout = timeout
			assertValidateOK(t, Pipeline{Actions: []Action{action}}, "timeout "+timeout.String())

			request := k01RequestAction("review", nil)
			request.Timeout = timeout
			assertValidateOK(t, Pipeline{Actions: []Action{request}}, "request timeout "+timeout.String())
		})
	}

	refused := []time.Duration{
		0,
		-time.Second,
		time.Minute - time.Nanosecond,
		30*24*time.Hour + time.Nanosecond,
	}
	for _, timeout := range refused {
		t.Run("refused "+timeout.String(), func(t *testing.T) {
			action := k01ContainerAction("build")
			action.Timeout = timeout
			assertInvalidInput(t, Validate(Pipeline{Actions: []Action{action}}), "container timeout "+timeout.String())

			request := k01RequestAction("review", nil)
			request.Timeout = timeout
			assertInvalidInput(t, Validate(Pipeline{Actions: []Action{request}}), "request timeout "+timeout.String())
		})
	}
}

func TestValidateRefusesUnknownSelfAndForwardReferences(t *testing.T) {
	cases := []struct {
		name     string
		pipeline Pipeline
	}{
		{
			"unknown action",
			Pipeline{Actions: []Action{
				k01RequestAction("review", map[string]SubjectValue{
					"candidate": {Kind: SubjectReference, Reference: Reference{Action: "missing", Key: "candidate"}},
				}),
			}},
		},
		{
			"self reference",
			Pipeline{Actions: []Action{
				k01RequestAction("review", map[string]SubjectValue{
					"candidate": {Kind: SubjectReference, Reference: Reference{Action: "review", Key: "candidate"}},
				}, "candidate"),
			}},
		},
		{
			"forward reference",
			Pipeline{Actions: []Action{
				k01RequestAction("first", map[string]SubjectValue{
					"candidate": {Kind: SubjectReference, Reference: Reference{Action: "second", Key: "candidate"}},
				}),
				k01ContainerAction("second", "candidate"),
			}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertInvalidInput(t, Validate(tc.pipeline), tc.name)
		})
	}
}

func TestValidateRefusesUndeclaredSubjectKeys(t *testing.T) {
	cases := []struct {
		name     string
		pipeline Pipeline
	}{
		{
			"undeclared input key",
			Pipeline{
				Inputs: []id.InputKey{"repository"},
				Actions: []Action{
					k01RequestAction("review", map[string]SubjectValue{
						"resource": {Kind: SubjectReference, Reference: Reference{Input: true, Key: "target_ref"}},
					}),
				},
			},
		},
		{
			"undeclared output key of an earlier action",
			Pipeline{Actions: []Action{
				k01ContainerAction("build", "candidate"),
				k01RequestAction("review", map[string]SubjectValue{
					"candidate": {Kind: SubjectReference, Reference: Reference{Action: "build", Key: "artifact"}},
				}),
			}},
		},
		{
			"earlier action declares no outputs",
			Pipeline{Actions: []Action{
				k01ContainerAction("build"),
				k01RequestAction("review", map[string]SubjectValue{
					"candidate": {Kind: SubjectReference, Reference: Reference{Action: "build", Key: "candidate"}},
				}),
			}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertInvalidInput(t, Validate(tc.pipeline), tc.name)
		})
	}
}

func TestValidateRefusesMalformedSubjectValues(t *testing.T) {
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
			pipeline := Pipeline{
				Inputs: []id.InputKey{"repository"},
				Actions: []Action{
					k01ContainerAction("build", "candidate"),
					k01RequestAction("review", map[string]SubjectValue{"field": tc.value}),
				},
			}
			assertInvalidInput(t, Validate(pipeline), tc.name)
		})
	}
}

func TestValidateRefusesEmptyImageAndAssignment(t *testing.T) {
	emptyImage := k01ContainerAction("build")
	emptyImage.Container.Image = ""
	assertInvalidInput(t, Validate(Pipeline{Actions: []Action{emptyImage}}), "empty image")

	emptyAssignment := k01RequestAction("review", nil)
	emptyAssignment.Request.Assignment = ""
	assertInvalidInput(t, Validate(Pipeline{Actions: []Action{emptyAssignment}}), "empty assignment")
}

func TestValidateRefusesEmptyAndNULContainerCommands(t *testing.T) {
	nilCommands := k01ContainerAction("build")
	nilCommands.Container.Commands = nil
	err := Validate(Pipeline{Actions: []Action{nilCommands}})
	assertInvalidInput(t, err, "nil container commands")
	if message, _ := fault.MessageOf(err); !strings.Contains(message, `action "build": container declares no commands`) {
		t.Fatalf("nil commands message = %q, want %q", message, `action "build": container declares no commands`)
	}

	emptyCommands := k01ContainerAction("build")
	emptyCommands.Container.Commands = []string{}
	err = Validate(Pipeline{Actions: []Action{emptyCommands}})
	assertInvalidInput(t, err, "empty container commands")
	if message, _ := fault.MessageOf(err); !strings.Contains(message, `action "build": container declares no commands`) {
		t.Fatalf("empty commands message = %q, want %q", message, `action "build": container declares no commands`)
	}

	nulFirst := k01ContainerAction("build")
	nulFirst.Container.Commands = []string{"echo\x00bad"}
	err = Validate(Pipeline{Actions: []Action{nulFirst}})
	assertInvalidInput(t, err, "NUL byte in command 0")
	if message, _ := fault.MessageOf(err); !strings.Contains(message, `action "build": command 0 contains a NUL byte`) {
		t.Fatalf("NUL command 0 message = %q, want %q", message, `action "build": command 0 contains a NUL byte`)
	}

	nulSecond := k01ContainerAction("build")
	nulSecond.Container.Commands = []string{"echo ok", "\x00"}
	err = Validate(Pipeline{Actions: []Action{nulSecond}})
	assertInvalidInput(t, err, "NUL byte in command 1")
	if message, _ := fault.MessageOf(err); !strings.Contains(message, `action "build": command 1 contains a NUL byte`) {
		t.Fatalf("NUL command 1 message = %q, want %q", message, `action "build": command 1 contains a NUL byte`)
	}

	validMulti := k01ContainerAction("build")
	validMulti.Container.Commands = []string{"", "echo hello", "test -f out"}
	assertValidateOK(t, Pipeline{Actions: []Action{validMulti}}, "non-empty commands without NUL bytes")
}

func TestValidateDoesNotMutateDeclaration(t *testing.T) {
	subject := NewSubject(map[string]SubjectValue{
		"resource": {Kind: SubjectReference, Reference: Reference{Input: true, Key: "repository"}},
	})
	action := k01RequestAction("review", nil)
	action.Request.Subject = subject
	action.Container = nil

	container := k01ContainerAction("build")
	container.Container.Env = []EnvVar{{Name: "LOG_LEVEL", Value: "info"}}
	container.Container.Secrets = []id.SecretName{"DB_PASSWORD"}

	pipeline := Pipeline{
		Inputs:  []id.InputKey{"repository"},
		Env:     []EnvVar{{Name: "APP_ENV", Value: "test"}},
		Actions: []Action{container, action},
	}

	inputsBefore := append([]id.InputKey(nil), pipeline.Inputs...)
	envBefore := append([]EnvVar(nil), pipeline.Env...)
	actionEnvBefore := append([]EnvVar(nil), pipeline.Actions[0].Container.Env...)
	fieldsBefore := pipeline.Actions[1].Request.Subject.Fields()

	assertValidateOK(t, pipeline, "declaration before mutation check")

	if !reflect.DeepEqual(inputsBefore, pipeline.Inputs) {
		t.Fatalf("Validate mutated pipeline inputs: %v, want %v", pipeline.Inputs, inputsBefore)
	}
	if !reflect.DeepEqual(envBefore, pipeline.Env) {
		t.Fatalf("Validate mutated pipeline env: %v, want %v", pipeline.Env, envBefore)
	}
	if !reflect.DeepEqual(actionEnvBefore, pipeline.Actions[0].Container.Env) {
		t.Fatalf("Validate mutated action env: %v, want %v", pipeline.Actions[0].Container.Env, actionEnvBefore)
	}
	if !reflect.DeepEqual(fieldsBefore, pipeline.Actions[1].Request.Subject.Fields()) {
		t.Fatalf("Validate mutated subject fields: %v, want %v", pipeline.Actions[1].Request.Subject.Fields(), fieldsBefore)
	}
}

// FuzzValidateActionName asserts the action-name grammar has no bypass: a
// declaration is accepted exactly when id.ParseActionName accepts the name.
func FuzzValidateActionName(f *testing.F) {
	for _, seed := range []string{
		"build",
		"a",
		"",
		"Build",
		"../build",
		"build--x",
		strings.Repeat("a", id.MaxNameLength+1),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, name string) {
		pipeline := Pipeline{Actions: []Action{{
			Name:    id.ActionName(name),
			Timeout: time.Minute,
			Container: &Container{
				Image:    k01PinnedImage,
				Commands: []string{"run"},
			},
		}}}
		err := Validate(pipeline)
		_, parseErr := id.ParseActionName(name)
		if (err == nil) != (parseErr == nil) {
			t.Fatalf("Validate(name=%q) error = %v, id.ParseActionName error = %v; grammar must agree", name, err, parseErr)
		}
		if err != nil {
			assertInvalidInput(t, err, "fuzz action name "+name)
		}
	})
}

// FuzzValidateUndeclaredInputReference asserts membership, not just syntax:
// with no admitted inputs, no inputs.<key> reference may validate for any key.
func FuzzValidateUndeclaredInputReference(f *testing.F) {
	for _, seed := range []string{
		"repository",
		"",
		"Repository",
		"../repository",
		strings.Repeat("a", id.MaxKeyLength+1),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, key string) {
		pipeline := Pipeline{Actions: []Action{k01RequestAction("review", map[string]SubjectValue{
			"resource": {Kind: SubjectReference, Reference: Reference{Input: true, Key: key}},
		})}}
		err := Validate(pipeline)
		if err == nil {
			t.Fatalf("Validate accepted a reference to undeclared input %q", key)
		}
		assertInvalidInput(t, err, "fuzz undeclared input "+key)
	})
}
