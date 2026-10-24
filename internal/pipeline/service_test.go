package pipeline_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

func Test_capability_and_reference_policy(t *testing.T) {
	agent := func(profile string) string {
		return "{id: impl, kind: agent, agent: {profile: " + profile + "}, timeoutSeconds: 60}"
	}
	t.Run("disabled kinds are rejected without lookups", func(t *testing.T) {
		m1 := newEnv(t, pipeline.KindContainer)
		_, err := m1.validate(doc(agent("coding-v1")))
		expectCode(t, err, pipeline.CodeKindDisabled)
		if m1.profiles.calls != 0 {
			t.Fatal("a disabled kind must not trigger profile lookups")
		}
		_, err = m1.validate(doc(container("a"), "{id: g, kind: gate, needs: [a]}"))
		expectCode(t, err, pipeline.CodeKindDisabled)
	})
	t.Run("container only wiring accepts container pipelines", func(t *testing.T) {
		newEnv(t, pipeline.KindContainer).mustValidate(t, doc(container("a")))
	})
	t.Run("nothing is enabled by default", func(t *testing.T) {
		none := newEnv(t)
		none.caps = nil
		v, _ := pipeline.NewValidator(pipeline.Config{Capabilities: pipeline.EnabledKinds{}})
		_, err := v.ValidateYAML(t.Context(), scope, actor, []byte(doc(container("a"))))
		expectCode(t, err, pipeline.CodeKindDisabled)
	})
	t.Run("a capability outage fails closed", func(t *testing.T) {
		v, _ := pipeline.NewValidator(pipeline.Config{Capabilities: failingCaps{}})
		_, err := v.ValidateYAML(t.Context(), scope, actor, []byte(doc(container("a"))))
		if !errors.Is(err, pipeline.ErrUnavailable) || errors.Is(err, pipeline.ErrInvalid) {
			t.Fatalf("expected unavailable, got %v", err)
		}
	})

	t.Run("immutable profiles are accepted", func(t *testing.T) {
		newEnv(t).mustValidate(t, doc(agent("coding-v1")))
	})
	t.Run("unknown profiles are rejected", func(t *testing.T) {
		_, err := newEnv(t).validate(doc(agent("ghost")))
		expectCode(t, err, pipeline.CodeUnknownProfile)
	})
	t.Run("mutable profile references are rejected", func(t *testing.T) {
		_, err := newEnv(t).validate(doc(agent("coding-latest")))
		expectCode(t, err, pipeline.CodeMutableProfile)
	})
	t.Run("a profile of another scope is not found", func(t *testing.T) {
		e := newEnv(t)
		other := domain.Scope{TenantID: "t_globex", ProjectID: "p_billing"}
		_, err := e.v.ValidateYAML(t.Context(), other, actor, []byte(doc(agent("coding-v1"))))
		expectCode(t, err, pipeline.CodeUnknownProfile)
	})
	t.Run("profile lookups fail closed", func(t *testing.T) {
		e := newEnv(t)
		e.profiles.err = errors.New("profile service down")
		_, err := e.validate(doc(agent("coding-v1")))
		if !errors.Is(err, pipeline.ErrUnavailable) || errors.Is(err, pipeline.ErrInvalid) {
			t.Fatalf("expected unavailable, got %v", err)
		}
		// Without a configured lookup an agent step is never accepted.
		v, _ := pipeline.NewValidator(pipeline.Config{Capabilities: pipeline.EnabledKinds{pipeline.KindAgent}})
		_, err = v.ValidateYAML(t.Context(), scope, actor, []byte(doc(agent("coding-v1"))))
		if !errors.Is(err, pipeline.ErrUnavailable) {
			t.Fatalf("expected unavailable without a lookup, got %v", err)
		}
	})
	t.Run("profile lookups are memoized per document", func(t *testing.T) {
		e := newEnv(t)
		e.mustValidate(t, doc(
			"{id: a, kind: agent, agent: {profile: coding-v1}, timeoutSeconds: 60}",
			"{id: b, kind: agent, needs: [a], agent: {profile: coding-v1}, timeoutSeconds: 60}"))
		if e.profiles.calls != 1 {
			t.Fatalf("expected one lookup, got %d", e.profiles.calls)
		}
	})

	secretStep := func(extra string) string { return container("a", extra) }
	t.Run("declared secret references are checked against the policy", func(t *testing.T) {
		e := newEnv(t)
		e.mustValidate(t, doc(secretStep("env: {T: {secretRef: 'secret://project/token'}}")))
		e.mustValidate(t, doc(secretStep("secretFiles: {t: 'secret://project/readonly'}")))
	})
	t.Run("an env secret the policy does not permit is rejected", func(t *testing.T) {
		_, err := newEnv(t).validate(doc(secretStep("env: {T: {secretRef: 'secret://project/readonly'}}")))
		expectCode(t, err, pipeline.CodeSecretDenied)
	})
	t.Run("an unknown or foreign secret is indistinguishable from a denied one", func(t *testing.T) {
		e := newEnv(t)
		_, unknown := e.validate(doc(secretStep("secretFiles: {t: 'secret://project/ghost'}")))
		_, denied := e.validate(doc(secretStep("env: {T: {secretRef: 'secret://project/readonly'}}")))
		var a, b *pipeline.ValidationError
		if !errors.As(unknown, &a) || !errors.As(denied, &b) || a.Diagnostics[0].Message != b.Diagnostics[0].Message || a.Diagnostics[0].Code != b.Diagnostics[0].Code {
			t.Fatalf("existence leaks through diagnostics: %v / %v", unknown, denied)
		}
		other := domain.Scope{TenantID: "t_globex", ProjectID: "p_billing"}
		_, err := e.v.ValidateYAML(t.Context(), other, actor, []byte(doc(secretStep("secretFiles: {t: 'secret://project/token'}"))))
		expectCode(t, err, pipeline.CodeSecretDenied)
	})
	t.Run("undeclared secret access is rejected", func(t *testing.T) {
		e := newEnv(t)
		for name, step := range map[string]string{
			"literal in env":     container("a", "env: {T: 'secret://project/token'}"),
			"literal in args":    container("a", "args: ['secret://project/token']"),
			"literal in command": "{id: a, kind: container, image: " + img + ", command: ['secret://project/token'], timeoutSeconds: 5}",
		} {
			t.Run(name, func(t *testing.T) {
				_, err := e.validate(doc(step))
				expectCode(t, err, pipeline.CodeInvalidSecretRef)
			})
		}
		for name, extra := range map[string]string{
			"other scope":     "secretFiles: {t: 'secret://tenant/token'}",
			"path traversal":  "secretFiles: {t: 'secret://project/../token'}",
			"missing name":    "secretFiles: {t: 'secret://project/'}",
			"env other scope": "env: {T: {secretRef: 'secret://other/token'}}",
		} {
			t.Run(name, func(t *testing.T) {
				_, err := e.validate(doc(container("a", extra)))
				if err == nil || !errors.Is(err, pipeline.ErrInvalid) {
					t.Fatalf("expected a rejection, got %v", err)
				}
			})
		}
		if e.refs.calls != 0 {
			t.Fatal("malformed references must never reach the policy")
		}
	})
	t.Run("secret policy outages and a missing policy fail closed", func(t *testing.T) {
		e := newEnv(t)
		e.refs.err = errors.New("policy down")
		_, err := e.validate(doc(secretStep("secretFiles: {t: 'secret://project/token'}")))
		if !errors.Is(err, pipeline.ErrUnavailable) || errors.Is(err, pipeline.ErrInvalid) {
			t.Fatalf("expected unavailable, got %v", err)
		}
		v, _ := pipeline.NewValidator(pipeline.Config{Capabilities: pipeline.EnabledKinds{pipeline.KindContainer}})
		_, err = v.ValidateYAML(t.Context(), scope, actor, []byte(doc(secretStep("secretFiles: {t: 'secret://project/token'}"))))
		if !errors.Is(err, pipeline.ErrUnavailable) {
			t.Fatalf("expected unavailable without a policy, got %v", err)
		}
		// A document without references needs no policy.
		if _, err := v.ValidateYAML(t.Context(), scope, actor, []byte(doc(container("a")))); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("the policy never sees plaintext and is asked once per distinct reference", func(t *testing.T) {
		e := newEnv(t)
		e.mustValidate(t, doc(
			secretStep("secretFiles: {a: 'secret://project/token', b: 'secret://project/token'}"),
			container("b", "needs: [a]", "secretFiles: {c: 'secret://project/token'}")))
		if e.refs.calls != 1 {
			t.Fatalf("expected one policy call, got %d", e.refs.calls)
		}
	})
	t.Run("environments are checked against the policy", func(t *testing.T) {
		e := newEnv(t)
		e.mustValidate(t, doc(container("deploy", "environment: env_staging")))
		_, err := e.validate(doc(container("deploy", "environment: env_prod")))
		expectCode(t, err, pipeline.CodeInvalidEnv)
		_, err = e.validate(doc(container("deploy", "environment: 'bad env'")))
		expectCode(t, err, pipeline.CodeInvalidFormat)
	})
	t.Run("lookups run only after every local rule passed", func(t *testing.T) {
		e := newEnv(t)
		_, err := e.validate(doc(container("a", "needs: [a]", "secretFiles: {t: 'secret://project/token'}")))
		expectCode(t, err, pipeline.CodeCycle)
		if e.refs.calls != 0 || e.profiles.calls != 0 {
			t.Fatal("lookups must not run for an invalid document")
		}
	})
}

type failingCaps struct{}

func (failingCaps) StepKindEnabled(context.Context, pipeline.StepKind) (bool, error) {
	return false, errors.New("capability store down")
}

func newAuthority() *pipelinetest.Authority {
	a := pipelinetest.NewAuthority()
	a.Grant(scope, "u_engineer", pipeline.ActorUser, pipeline.PermList, pipeline.PermRegister, pipeline.PermRead)
	a.Grant(scope, "u_viewer", pipeline.ActorUser, pipeline.PermList, pipeline.PermRead)
	a.Grant(scope, "svc_ci", pipeline.ActorService, pipeline.PermRegister)
	return a
}

type svcEnv struct {
	*env
	repo *pipelinetest.MemRepo
	auth *pipelinetest.Authority
	svc  *pipeline.Service
	now  time.Time
}

func newSvc(t testing.TB, kinds ...pipeline.StepKind) *svcEnv {
	t.Helper()
	e := newEnv(t, kinds...)
	s := &svcEnv{env: e, repo: &pipelinetest.MemRepo{}, auth: newAuthority(), now: time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)}
	n := 0
	var err error
	s.svc, err = pipeline.NewService(pipeline.ServiceConfig{
		Repository: s.repo, Authority: s.auth, Validator: e.v,
		Clock: func() time.Time { s.now = s.now.Add(time.Second); return s.now },
		NewID: func() (string, error) { n++; return fmt.Sprintf("pd_%s_%04d", prefix, n), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *svcEnv) register(principal, src string, format pipeline.Format) (pipeline.RegisterResult, error) {
	return s.svc.Register(context.Background(), pipeline.RegisterCommand{
		Scope: scope, Caller: pipeline.Caller{PrincipalID: principal, RequestID: "req_1"}, Format: format, Body: []byte(src),
	})
}

func Test_service_register_get_list(t *testing.T) {
	s := newSvc(t)
	src := doc(container("a"))

	first, err := s.register("u_engineer", src, pipeline.FormatYAML)
	if err != nil || !first.Created || first.Definition.Version != 1 || first.Definition.Name != "sample" {
		t.Fatalf("first registration: %+v %v", first, err)
	}
	if first.Definition.CreatedBy != (pipeline.Actor{ID: "u_engineer", Type: pipeline.ActorUser}) {
		t.Fatalf("the actor must come from the authority: %+v", first.Definition.CreatedBy)
	}
	if !first.Definition.CreatedAt.Equal(first.Definition.CreatedAt.Truncate(time.Microsecond)) {
		t.Fatal("creation time must be microsecond precision")
	}

	again, err := s.register("u_engineer", src, pipeline.FormatYAML)
	if err != nil || again.Created || again.Definition.ID != first.Definition.ID || again.Definition.Digest != first.Definition.Digest {
		t.Fatalf("identical content must return the existing definition: %+v %v", again, err)
	}
	asJSON, err := s.register("svc_ci", string(first.Definition.Content.Canonical()), pipeline.FormatJSON)
	if err != nil || asJSON.Created || asJSON.Definition.ID != first.Definition.ID {
		t.Fatalf("the canonical JSON form must dedupe with the YAML form: %+v %v", asJSON, err)
	}

	changed, err := s.register("u_engineer", doc(container("a", "args: [changed]")), pipeline.FormatYAML)
	if err != nil || !changed.Created || changed.Definition.Version != 2 || changed.Definition.ID == first.Definition.ID {
		t.Fatalf("changed content must be a new version: %+v %v", changed, err)
	}
	if reread, _ := s.svc.Get(t.Context(), scope, pipeline.Caller{PrincipalID: "u_engineer"}, first.Definition.ID); string(reread.Content.Canonical()) != string(first.Definition.Content.Canonical()) {
		t.Fatal("the earlier version must remain unchanged")
	}
	back, err := s.register("u_engineer", src, pipeline.FormatYAML)
	if err != nil || back.Created || back.Definition.Version != 1 {
		t.Fatalf("returning to earlier content resolves to its version: %+v %v", back, err)
	}

	got, err := s.svc.Get(t.Context(), scope, pipeline.Caller{PrincipalID: "u_viewer"}, changed.Definition.ID)
	if err != nil || got.Version != 2 {
		t.Fatalf("get: %+v %v", got, err)
	}
	list, err := s.svc.List(t.Context(), scope, pipeline.Caller{PrincipalID: "u_viewer"}, pipeline.ListQuery{Limit: 10})
	if err != nil || len(list) != 2 || list[0].Version != 1 || list[1].Version != 2 {
		t.Fatalf("list: %+v %v", list, err)
	}
	named, err := s.svc.List(t.Context(), scope, pipeline.Caller{PrincipalID: "u_viewer"}, pipeline.ListQuery{Name: "other", Limit: 10})
	if err != nil || len(named) != 0 {
		t.Fatalf("name filter: %+v %v", named, err)
	}
	if _, err := s.svc.List(t.Context(), scope, pipeline.Caller{PrincipalID: "u_viewer"}, pipeline.ListQuery{Name: "Bad Name", Limit: 10}); !errors.Is(err, pipeline.ErrInvalid) {
		t.Fatalf("invalid name filter: %v", err)
	}
	if _, err := s.svc.List(t.Context(), scope, pipeline.Caller{PrincipalID: "u_viewer"}, pipeline.ListQuery{Limit: 0}); !errors.Is(err, pipeline.ErrInvalid) {
		t.Fatalf("zero page: %v", err)
	}
	if _, err := s.svc.Get(t.Context(), scope, pipeline.Caller{PrincipalID: "u_viewer"}, "pd_missing"); !errors.Is(err, pipeline.ErrNotFound) {
		t.Fatalf("missing definition: %v", err)
	}
	if _, err := s.svc.Get(t.Context(), scope, pipeline.Caller{PrincipalID: "u_viewer"}, "bad id"); !errors.Is(err, pipeline.ErrNotFound) {
		t.Fatalf("malformed identifier: %v", err)
	}
}

func Test_service_authority(t *testing.T) {
	s := newSvc(t)
	src := doc(container("a"))
	if _, err := s.register("u_engineer", src, pipeline.FormatYAML); err != nil {
		t.Fatal(err)
	}
	calls := s.repo.Calls

	t.Run("a viewer cannot register, even identical content", func(t *testing.T) {
		_, err := s.register("u_viewer", src, pipeline.FormatYAML)
		if !errors.Is(err, pipeline.ErrDenied) {
			t.Fatalf("expected denial, got %v", err)
		}
		if s.repo.Calls != calls {
			t.Fatal("a denied registration must not reach persistence")
		}
	})
	t.Run("denial precedes validation, so invalid content discloses nothing", func(t *testing.T) {
		_, err := s.register("u_viewer", "not: [valid", pipeline.FormatYAML)
		if !errors.Is(err, pipeline.ErrDenied) || errors.Is(err, pipeline.ErrInvalid) {
			t.Fatalf("expected denial before parsing, got %v", err)
		}
		_, err = s.register("u_unknown", "not: [valid", pipeline.FormatYAML)
		if !errors.Is(err, pipeline.ErrHidden) {
			t.Fatalf("expected hidden, got %v", err)
		}
	})
	t.Run("revocation takes effect on the next call", func(t *testing.T) {
		if _, err := s.register("svc_ci", src, pipeline.FormatYAML); err != nil {
			t.Fatal(err)
		}
		s.auth.Revoke(scope, "svc_ci", pipeline.PermRegister)
		if _, err := s.register("svc_ci", src, pipeline.FormatYAML); !errors.Is(err, pipeline.ErrDenied) {
			t.Fatalf("expected denial after revocation, got %v", err)
		}
	})
	t.Run("reads are authorized", func(t *testing.T) {
		list, _ := s.svc.List(t.Context(), scope, pipeline.Caller{PrincipalID: "u_viewer"}, pipeline.ListQuery{Limit: 5})
		s.auth.Revoke(scope, "u_viewer", pipeline.PermRead)
		s.auth.Revoke(scope, "u_viewer", pipeline.PermList)
		if _, err := s.svc.Get(t.Context(), scope, pipeline.Caller{PrincipalID: "u_viewer"}, list[0].ID); !errors.Is(err, pipeline.ErrDenied) {
			t.Fatalf("get after revocation: %v", err)
		}
		if _, err := s.svc.List(t.Context(), scope, pipeline.Caller{PrincipalID: "u_viewer"}, pipeline.ListQuery{Limit: 5}); !errors.Is(err, pipeline.ErrDenied) {
			t.Fatalf("list after revocation: %v", err)
		}
	})
	t.Run("an authority outage is unavailable, never access", func(t *testing.T) {
		s.auth.SetErr(errors.New("identity down"))
		for name, fn := range map[string]func() error{
			"register": func() error { _, err := s.register("u_engineer", src, pipeline.FormatYAML); return err },
			"get": func() error {
				_, err := s.svc.Get(t.Context(), scope, pipeline.Caller{PrincipalID: "u_engineer"}, "pd_x")
				return err
			},
			"list": func() error {
				_, err := s.svc.List(t.Context(), scope, pipeline.Caller{PrincipalID: "u_engineer"}, pipeline.ListQuery{Limit: 1})
				return err
			},
		} {
			if err := fn(); !errors.Is(err, pipeline.ErrUnavailable) {
				t.Fatalf("%s: expected unavailable, got %v", name, err)
			}
		}
		s.auth.SetErr(nil)
	})
	t.Run("scope and caller are validated", func(t *testing.T) {
		_, err := s.svc.Register(t.Context(), pipeline.RegisterCommand{Scope: domain.Scope{}, Caller: pipeline.Caller{PrincipalID: "u_engineer"}, Format: pipeline.FormatYAML, Body: []byte(src)})
		if !errors.Is(err, pipeline.ErrInvalid) {
			t.Fatalf("empty scope: %v", err)
		}
		_, err = s.svc.Register(t.Context(), pipeline.RegisterCommand{Scope: scope, Caller: pipeline.Caller{}, Format: pipeline.FormatYAML, Body: []byte(src)})
		if !errors.Is(err, pipeline.ErrInvalid) {
			t.Fatalf("empty caller: %v", err)
		}
		_, err = s.svc.Register(t.Context(), pipeline.RegisterCommand{Scope: scope, Caller: pipeline.Caller{PrincipalID: "u_engineer"}, Format: "xml", Body: []byte(src)})
		if !errors.Is(err, pipeline.ErrInvalid) {
			t.Fatalf("unknown format: %v", err)
		}
	})
	t.Run("a service without ports cannot exist", func(t *testing.T) {
		if _, err := pipeline.NewService(pipeline.ServiceConfig{}); err == nil {
			t.Fatal("ports are required")
		}
		if _, err := pipeline.NewService(pipeline.ServiceConfig{Repository: s.repo, Authority: s.auth}); err == nil {
			t.Fatal("a validator is required")
		}
	})
}

func Test_service_validation_and_failures(t *testing.T) {
	t.Run("invalid documents never reach persistence", func(t *testing.T) {
		s := newSvc(t)
		_, err := s.register("u_engineer", doc(container("a", "needs: [a]")), pipeline.FormatYAML)
		expectCode(t, err, pipeline.CodeCycle)
		if s.repo.Calls != 0 {
			t.Fatal("persistence must not be called")
		}
	})
	t.Run("M1 capabilities reject gates at registration", func(t *testing.T) {
		s := newSvc(t, pipeline.KindContainer)
		_, err := s.register("u_engineer", doc(container("a"), "{id: g, kind: gate, needs: [a]}"), pipeline.FormatYAML)
		expectCode(t, err, pipeline.CodeKindDisabled)
	})
	t.Run("a repository outage is unavailable and not retried internally", func(t *testing.T) {
		s := newSvc(t)
		s.repo.Err = fmt.Errorf("%w: %w", pipeline.ErrUnavailable, pipeline.ErrOutcomeUnknown)
		_, err := s.register("u_engineer", doc(container("a")), pipeline.FormatYAML)
		if !errors.Is(err, pipeline.ErrUnavailable) || !errors.Is(err, pipeline.ErrOutcomeUnknown) || s.repo.Calls != 1 {
			t.Fatalf("got %v after %d calls", err, s.repo.Calls)
		}
	})
	t.Run("a repository answering for different content is rejected", func(t *testing.T) {
		s := newSvc(t)
		other, _ := s.env.validate(doc(container("zzz")))
		wrong := &wrongRepo{content: other}
		svc, _ := pipeline.NewService(pipeline.ServiceConfig{Repository: wrong, Authority: s.auth, Validator: s.v})
		_, err := svc.Register(t.Context(), pipeline.RegisterCommand{Scope: scope, Caller: pipeline.Caller{PrincipalID: "u_engineer"}, Format: pipeline.FormatYAML, Body: []byte(doc(container("a")))})
		if !errors.Is(err, pipeline.ErrConflict) {
			t.Fatalf("expected a conflict, got %v", err)
		}
	})
	t.Run("a lookup outage during validation is unavailable", func(t *testing.T) {
		s := newSvc(t)
		s.refs.err = errors.New("policy down")
		_, err := s.register("u_engineer", doc(container("a", "secretFiles: {t: 'secret://project/token'}")), pipeline.FormatYAML)
		if !errors.Is(err, pipeline.ErrUnavailable) || s.repo.Calls != 0 {
			t.Fatalf("got %v after %d repository calls", err, s.repo.Calls)
		}
	})
	t.Run("the definition entity rejects inconsistent content", func(t *testing.T) {
		s := newSvc(t)
		v := s.mustValidate(t, doc(container("a")))
		c := pipeline.Candidate{Scope: scope, ID: "pd_1", Content: v, CreatedBy: actor, CreatedAt: time.Now()}
		d, err := pipeline.NewDefinition(c, 1)
		if err != nil || d.Name != "sample" || d.Digest != v.Digest() {
			t.Fatalf("%+v %v", d, err)
		}
		for name, mutate := range map[string]func(*pipeline.Candidate) ([]byte, domain.Version){
			"zero content": func(c *pipeline.Candidate) ([]byte, domain.Version) { c.Content = pipeline.Validated{}; return nil, 1 },
			"bad scope":    func(c *pipeline.Candidate) ([]byte, domain.Version) { c.Scope = domain.Scope{}; return nil, 1 },
			"bad id":       func(c *pipeline.Candidate) ([]byte, domain.Version) { c.ID = "a b"; return nil, 1 },
			"bad actor":    func(c *pipeline.Candidate) ([]byte, domain.Version) { c.CreatedBy = pipeline.Actor{}; return nil, 1 },
			"no time":      func(c *pipeline.Candidate) ([]byte, domain.Version) { c.CreatedAt = time.Time{}; return nil, 1 },
			"zero version": func(c *pipeline.Candidate) ([]byte, domain.Version) { return nil, 0 },
		} {
			cc := c
			_, version := mutate(&cc)
			if _, err := pipeline.NewDefinition(cc, version); err == nil {
				t.Errorf("%s: accepted", name)
			}
		}
	})
}

type wrongRepo struct{ content pipeline.Validated }

func (w *wrongRepo) RegisterOrGet(_ context.Context, c pipeline.Candidate) (pipeline.Definition, bool, error) {
	c.Content = w.content
	d, err := pipeline.NewDefinition(c, 1)
	return d, true, err
}
func (*wrongRepo) Get(context.Context, domain.Scope, domain.DefinitionID) (pipeline.Definition, error) {
	return pipeline.Definition{}, pipeline.ErrNotFound
}
func (*wrongRepo) List(context.Context, domain.Scope, pipeline.ListQuery) ([]pipeline.Definition, error) {
	return nil, nil
}
