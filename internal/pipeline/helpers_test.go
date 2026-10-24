package pipeline_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/pipeline"
)

// Resource prefix of this task's test fixtures.
const prefix = "t014"

const digest1 = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

var (
	img   = "registry.example.invalid/go-tools@" + digest1
	scope = domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	actor = pipeline.Actor{ID: "u_engineer", Type: pipeline.ActorUser}
)

// container builds one flow-style container step.
func container(id string, extra ...string) string {
	return "{id: " + id + ", kind: container, image: " + img + ", command: [/bin/sh, -ec], timeoutSeconds: 60" + joinExtra(extra) + "}"
}

func joinExtra(extra []string) string {
	if len(extra) == 0 {
		return ""
	}
	return ", " + strings.Join(extra, ", ")
}

// doc assembles a document from step flow mappings.
func doc(steps ...string) string { return docWith("{}", steps...) }

func docWith(inputs string, steps ...string) string {
	var b strings.Builder
	b.WriteString("apiVersion: kodama/v1alpha1\nkind: Pipeline\nmetadata: {name: sample}\nspec:\n  inputs: " + inputs + "\n  steps:\n")
	for _, s := range steps {
		b.WriteString("    - " + s + "\n")
	}
	return b.String()
}

type fakeProfiles struct {
	mu    sync.Mutex
	known map[string]bool // name -> immutable
	err   error
	calls int
}

func (f *fakeProfiles) LookupProfile(_ context.Context, s domain.Scope, name string) (pipeline.ProfileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return pipeline.ProfileInfo{}, f.err
	}
	if s != scope {
		return pipeline.ProfileInfo{}, pipeline.ErrReferenceNotFound
	}
	immutable, ok := f.known[name]
	if !ok {
		return pipeline.ProfileInfo{}, pipeline.ErrReferenceNotFound
	}
	return pipeline.ProfileInfo{Immutable: immutable}, nil
}

type fakeRefs struct {
	secrets      map[string]bool // name -> env use permitted
	environments map[string]bool
	err          error
	calls        int
}

func (f *fakeRefs) CheckSecret(_ context.Context, s domain.Scope, _ pipeline.Actor, name string, use pipeline.SecretUse) error {
	f.calls++
	if f.err != nil {
		return f.err
	}
	envOK, ok := f.secrets[name]
	if s != scope || !ok {
		return pipeline.ErrReferenceNotFound
	}
	if use == pipeline.SecretUseEnv && !envOK {
		return pipeline.ErrReferenceDenied
	}
	return nil
}

func (f *fakeRefs) CheckEnvironment(_ context.Context, s domain.Scope, _ pipeline.Actor, id domain.EnvironmentID) error {
	f.calls++
	if f.err != nil {
		return f.err
	}
	if s != scope || !f.environments[string(id)] {
		return pipeline.ErrReferenceNotFound
	}
	return nil
}

type env struct {
	profiles *fakeProfiles
	refs     *fakeRefs
	caps     pipeline.EnabledKinds
	v        *pipeline.Validator
}

// newEnv builds a validator with every kind enabled and permissive fakes.
func newEnv(t testing.TB, kinds ...pipeline.StepKind) *env {
	t.Helper()
	if len(kinds) == 0 {
		kinds = []pipeline.StepKind{pipeline.KindContainer, pipeline.KindAgent, pipeline.KindGate}
	}
	e := &env{
		profiles: &fakeProfiles{known: map[string]bool{"coding-v1": true, "coding-latest": false}},
		refs:     &fakeRefs{secrets: map[string]bool{"token": true, "readonly": false, "registry-token": true, "netrc": true}, environments: map[string]bool{"env_staging": true}},
		caps:     pipeline.EnabledKinds(kinds),
	}
	v, err := pipeline.NewValidator(pipeline.Config{Capabilities: e.caps, Profiles: e.profiles, References: e.refs})
	if err != nil {
		t.Fatal(err)
	}
	e.v = v
	return e
}

func (e *env) validate(src string) (pipeline.Validated, error) {
	return e.v.ValidateYAML(context.Background(), scope, actor, []byte(src))
}

func (e *env) mustValidate(t testing.TB, src string) pipeline.Validated {
	t.Helper()
	v, err := e.validate(src)
	if err != nil {
		t.Fatalf("expected a valid document: %v\n%s", err, src)
	}
	return v
}

// codes returns the diagnostic codes of a validation error.
func codes(err error) []string {
	var ve *pipeline.ValidationError
	if !errors.As(err, &ve) {
		return nil
	}
	out := make([]string, len(ve.Diagnostics))
	for i, d := range ve.Diagnostics {
		out[i] = d.Code
	}
	return out
}

func hasCode(err error, code string) bool {
	for _, c := range codes(err) {
		if c == code {
			return true
		}
	}
	return false
}

func hasDiag(err error, code, pathPart string) bool {
	var ve *pipeline.ValidationError
	if !errors.As(err, &ve) {
		return false
	}
	for _, d := range ve.Diagnostics {
		if d.Code == code && strings.Contains(d.Path, pathPart) {
			return true
		}
	}
	return false
}

func expectCode(t testing.TB, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected diagnostic %q, got a valid document", code)
	}
	if !errors.Is(err, pipeline.ErrInvalid) || !hasCode(err, code) {
		t.Fatalf("expected diagnostic %q, got %v (codes %v)", code, err, codes(err))
	}
}
