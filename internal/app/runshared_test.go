package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/kube/kubetest"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	"github.com/optimiware/kodama/internal/runtimeauth"
	rtjwt "github.com/optimiware/kodama/internal/runtimeauth/jwt"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
	"github.com/optimiware/kodama/internal/secrets"
)

// ---- the run_shared composition fixture -------------------------------------------

// sharedFx is the container execution slice composed with the run_shared model
// from test doubles: the companion port of the SAME backend as the runtime, the
// installation's policy list and the runtime authority's resource subjects.
type sharedFx struct {
	*fixture
	list *PolicyList
	res  *executiontest.FakeResources
	rr   *rtest.MemResources
}

func sharedConfig(policies ...execution.ExecutionPolicy) RunSharedConfig {
	return RunSharedConfig{Policies: policies}
}

func sharedValidator(t *testing.T, enabled bool) *pipeline.Validator {
	t.Helper()
	v, err := pipeline.NewValidator(pipeline.Config{
		Capabilities:   pipeline.EnabledKinds{pipeline.KindContainer},
		Profiles:       pipelinetest.Profiles{},
		References:     pipelinetest.References{},
		EnableV1Alpha2: enabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// newSharedFx assembles the slice with run_shared composed. The mutations run
// before assembly, over the inputs.
func newSharedFx(t *testing.T, env Env, cfg RunSharedConfig, mutate ...func(*sharedFx)) *sharedFx {
	t.Helper()
	s := buildSharedFx(t, env, cfg, mutate...)
	slice, err := AssembleExecution(s.in)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	s.slice = slice
	return s
}

func buildSharedFx(t *testing.T, env Env, cfg RunSharedConfig, mutate ...func(*sharedFx)) *sharedFx {
	t.Helper()
	list, err := NewPolicyList(cfg)
	if err != nil {
		t.Fatal(err)
	}
	lim := executiontest.SharedLimit()
	s := &sharedFx{list: list, res: executiontest.NewFakeResources(executiontest.SharedBackend), rr: rtest.NewMemResources()}
	s.fixture = fxBuildOpts(t, env, executiontest.MemOptions{Limits: &lim, Cleanup: executiontest.AllowCleanup{}}, func(f *fixture) {
		f.m.Manual.SetCapabilities(executiontest.SharedCaps())
		f.m.Validator = sharedValidator(t, true)
		f.in.Runtime.Shared = &SharedRuntime{Resources: s.res, Workloads: s.rr, Policies: s.list}
		f.in.Shared = &SharedInputs{
			Store: s.rr, State: s.rr, Tokens: f.in.Auth.Tokens.(*rtjwt.Signer), Verifier: f.in.Auth.Verifier.(*rtjwt.Verifier),
		}
	})
	for _, m := range mutate {
		m(s)
	}
	return s
}

func (s *sharedFx) defineShared() pipeline.Definition {
	return s.m.Define(fxScope, executiontest.SharedSpecDoc(false, executiontest.SharedStep("build")))
}

func (s *sharedFx) admitDef(def pipeline.Definition) (*execution.Run, error) {
	p := s.m.NewProject(fxScope)
	return s.slice.Trusted.Admit(context.Background(), execution.Caller{PrincipalID: executiontest.Engineer, RequestID: "req_t116"}, execution.AdmitCommand{
		Scope: p.Scope, WorkItemID: p.Item, DefinitionID: def.ID, ContentRevision: s.m.ContentRevision(p), SourceCommit: executiontest.CommitA,
	})
}

func policyWith(id string, mutate func(*execution.ExecutionPolicy)) execution.ExecutionPolicy {
	p := executiontest.TenantPolicy()
	p.ID = id
	if mutate != nil {
		mutate(&p)
	}
	return p
}

// Test_unit_app_run_shared_composition: the execution slice composes the
// run_shared model completely or not at all. Composed, the service of the same
// backend admits kodama/v1alpha2 under the installation's current policy and pins
// it, the resource port of that backend receives the run's resources, the
// /runtime/v2 routes are served and the credential registration of a resource
// reaches the runtime authority with its conflicts and revocations classified.
// Unconfigured, kodama/v1alpha2 admission is refused exactly as before and no
// v2 route exists. The legacy format follows the policy's `legacyFormat`.
func Test_unit_app_run_shared_composition(t *testing.T) {
	open := policyWith("run-open", nil)

	t.Run("composed: admission pins the current policy and the port of the runtime's backend receives the resources", func(t *testing.T) {
		s := newSharedFx(t, Test, sharedConfig(open))
		if s.slice.RuntimeV2 == nil || s.slice.Policies == nil || !hasRoute(s.slice.Routes, "runtime-v2") {
			t.Fatalf("run_shared is not composed: routes %v", s.slice.Routes)
		}
		if !hasRoute(s.slice.Disabled, "service-secret-resolution") {
			t.Fatal("a missing secret resolver is not recorded as disabled")
		}
		run, err := s.admitDef(s.defineShared())
		if err != nil {
			t.Fatalf("admit: %v", err)
		}
		pin, _ := open.Pin()
		if run.Shared == nil || run.Shared.RunModel != execution.RunModelShared || run.Shared.Policy != pin {
			t.Fatalf("the run did not pin the current policy: %+v", run.Shared)
		}
		// The first claim starts the run; the reconciliation then provisions its
		// resources through the port of the runtime's backend (the network first).
		if _, err := s.slice.Trusted.DispatchNext(context.Background(), fxScope.TenantID); err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		if _, err := s.slice.Trusted.ReconcileTenant(context.Background(), fxScope.TenantID); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if calls := strings.Join(s.res.Calls(), ","); !strings.HasPrefix(calls, "ensure:network") {
			t.Fatalf("the resource port received %q: the service is bound to another port", calls)
		}
	})

	t.Run("the newest policy is the one admission pins and an older one stays resolvable", func(t *testing.T) {
		older := policyWith("run-old", nil)
		newest := policyWith("run-new", func(p *execution.ExecutionPolicy) { p.Limits.MaxServices = 4 })
		s := newSharedFx(t, Test, sharedConfig(older, newest))
		run, err := s.admitDef(s.defineShared())
		if err != nil {
			t.Fatal(err)
		}
		pin, _ := newest.Pin()
		if run.Shared.Policy != pin {
			t.Fatalf("pinned %+v, want the newest %+v", run.Shared.Policy, pin)
		}
		oldPin, _ := older.Pin()
		if body, err := s.list.Policy(context.Background(), oldPin); err != nil || body.ID != "run-old" {
			t.Fatalf("an older retained policy is not resolvable: %v", err)
		}
	})

	t.Run("a closed policy refuses new-format admission and nothing is created", func(t *testing.T) {
		closed := policyWith("run-closed", func(p *execution.ExecutionPolicy) { p.Admission.RunShared = execution.PolicyClosed })
		s := newSharedFx(t, Test, sharedConfig(closed))
		if _, err := s.admitDef(s.defineShared()); !errors.Is(err, execution.ErrAdmissionRefused) {
			t.Fatalf("admit under a closed policy: %v", err)
		}
		if len(s.res.Calls()) != 0 {
			t.Fatal("a refused admission touched the resource port")
		}
	})

	t.Run("unconfigured: kodama/v1alpha2 is refused exactly as before and no v2 route exists", func(t *testing.T) {
		f := newFixture(t, Test, func(f *fixture) {
			f.m.Manual.SetCapabilities(executiontest.SharedCaps())
			f.m.Validator = sharedValidator(t, true)
		})
		if f.slice.RuntimeV2 != nil || f.slice.Policies != nil || hasRoute(f.slice.Routes, "runtime-v2") {
			t.Fatal("run_shared is composed without being configured")
		}
		def := f.m.Define(fxScope, executiontest.SharedSpecDoc(false, executiontest.SharedStep("build")))
		p := f.m.NewProject(fxScope)
		_, err := f.slice.Trusted.Admit(context.Background(), execution.Caller{PrincipalID: executiontest.Engineer, RequestID: "req_t116"}, execution.AdmitCommand{
			Scope: p.Scope, WorkItemID: p.Item, DefinitionID: def.ID, ContentRevision: f.m.ContentRevision(p), SourceCommit: executiontest.CommitA,
		})
		if !errors.Is(err, execution.ErrAdmissionRefused) {
			t.Fatalf("an unconfigured composition admitted kodama/v1alpha2: %v", err)
		}
	})

	t.Run("the legacy format follows the policy", func(t *testing.T) {
		legacy := func(s *sharedFx) pipeline.Definition { return s.define(pipelinetest.Container("build")) }
		for _, mode := range []string{execution.PolicyDrain, execution.PolicyClosed} {
			p := policyWith("run-"+mode, func(p *execution.ExecutionPolicy) { p.Admission.LegacyFormat = mode })
			s := newSharedFx(t, Test, sharedConfig(p))
			_, err := s.admitDef(legacy(s))
			if !errors.Is(err, execution.ErrAdmissionRefused) || !strings.Contains(err.Error(), pipeline.AdmissionFormatRetired) {
				t.Fatalf("legacyFormat %s: v1alpha1 admission: %v", mode, err)
			}
			if _, err := s.admitDef(s.defineShared()); err != nil {
				t.Fatalf("legacyFormat %s refused kodama/v1alpha2: %v", mode, err)
			}
		}
		s := newSharedFx(t, Test, sharedConfig(policyWith("run-open-legacy", nil)))
		if _, err := s.admitDef(legacy(s)); err != nil {
			t.Fatalf("legacyFormat open refused kodama/v1alpha1: %v", err)
		}
	})

	t.Run("credential registration reaches the runtime authority with its failures classified", func(t *testing.T) {
		s := newSharedFx(t, Test, sharedConfig(open))
		ref := runtimeauth.ResourceRef{Scope: fxScope, RunID: "run_t116", Key: "preparation"}
		f := rtest.ResourceFixture{Scope: ref.Scope, RunID: ref.RunID, WorkloadUID: "8b3e3f40-5c6d-4e7f-8a01-2c3d4e5f6071", DeadlineAt: time.Now().Add(time.Hour)}
		s.rr.Put(rtest.PreparationState(f))
		st, _ := s.rr.ObserveResource(context.Background(), ref)
		reg := kubeResourceRegistrar{late: s.late}
		cred, _ := runtimeauth.GenerateCredential(nil)
		cmd := kube.ResourceRegisterCommand{Scope: ref.Scope, RunID: ref.RunID, Key: ref.Key, Credential: cred, SpecFingerprint: st.Fingerprint, ExpiresAt: st.DeadlineAt}
		if err := reg.RegisterResource(context.Background(), cmd); err != nil {
			t.Fatalf("register: %v", err)
		}
		if err := reg.RegisterResource(context.Background(), cmd); err != nil {
			t.Fatalf("the same credential is not idempotent: %v", err)
		}
		other, _ := runtimeauth.GenerateCredential(nil)
		cmd.Credential = other
		if err := reg.RegisterResource(context.Background(), cmd); !errors.Is(err, kube.ErrRegistrationConflict) {
			t.Fatalf("a different credential: %v", err)
		}
		s.rr.Stop(ref, true, time.Now())
		if err := reg.RegisterResource(context.Background(), cmd); !errors.Is(err, kube.ErrRegistrationRevoked) {
			t.Fatalf("an ended resource: %v", err)
		}
		// Before the authority is bound nothing registers.
		if err := (kubeResourceRegistrar{late: &LateRegistrar{}}).RegisterResource(context.Background(), cmd); !errors.Is(err, runtimeauth.ErrUnavailable) {
			t.Fatalf("an unbound registrar accepted a registration: %v", err)
		}
	})
}

func hasRoute(list []string, name string) bool {
	for _, v := range list {
		if v == name {
			return true
		}
	}
	return false
}

// Test_unit_app_run_shared_refuses_unsupported_backends: run_shared is composed on
// Kubernetes only and never degrades. The Process and Podman environments refuse
// the configuration at startup; the slice refuses a half-composed model, a
// resource port that is not the runtime's backend, a pre-bound resource port and
// every development adapter under production (the Process and Podman backends
// included, behind any wrapper); a runtime that offers no resource port leaves
// the model closed.
func Test_unit_app_run_shared_refuses_unsupported_backends(t *testing.T) {
	cfg := sharedConfig(policyWith("run-open", nil))

	t.Run("the configuration is refused outside production", func(t *testing.T) {
		for _, env := range []Env{Development, Test} {
			c := CoreConfig{Env: env}
			c.Runtime.Kubernetes.RunShared = &cfg
			refused := false
			for _, e := range c.validateRuntime() {
				refused = refused || strings.Contains(e.Error(), "run_shared is configured")
			}
			if !refused {
				t.Errorf("%s accepted run_shared", env)
			}
		}
		c := CoreConfig{Env: Production}
		c.Runtime.Kubernetes.RunShared = &RunSharedConfig{}
		found := false
		for _, e := range c.validateRuntime() {
			found = found || strings.Contains(e.Error(), "at least one execution policy")
		}
		if !found {
			t.Error("production accepted an empty policy list")
		}
	})

	t.Run("a half-composed model is refused", func(t *testing.T) {
		if _, err := AssembleExecution(buildSharedFx(t, Test, cfg, func(s *sharedFx) { s.in.Shared = nil }).in); !errors.Is(err, execution.ErrNotConfigured) {
			t.Fatalf("a runtime with a resource port and no resource authority: %v", err)
		}
		if _, err := AssembleExecution(buildSharedFx(t, Test, cfg, func(s *sharedFx) { s.in.Runtime.Shared = nil }).in); !errors.Is(err, execution.ErrNotConfigured) {
			t.Fatalf("a resource authority and no resource port: %v", err)
		}
		for name, mutate := range map[string]func(*sharedFx){
			"no workload verifier": func(s *sharedFx) { s.in.Runtime.Shared.Workloads = nil },
			"no policy source":     func(s *sharedFx) { s.in.Runtime.Shared.Policies = nil },
			"no resource port":     func(s *sharedFx) { s.in.Runtime.Shared.Resources = nil },
			"no grant store":       func(s *sharedFx) { s.in.Shared.Store = nil },
			"no token issuer":      func(s *sharedFx) { s.in.Shared.Tokens = nil },
		} {
			if _, err := AssembleExecution(buildSharedFx(t, Test, cfg, mutate).in); !errors.Is(err, execution.ErrNotConfigured) {
				t.Errorf("%s: %v", name, err)
			}
		}
	})

	t.Run("the resource port must be the runtime's backend and cannot be pre-bound", func(t *testing.T) {
		other := func(s *sharedFx) { s.res.SetBackend("elsewhere") }
		if _, err := AssembleExecution(buildSharedFx(t, Test, cfg, other).in); !errors.Is(err, execution.ErrNotConfigured) {
			t.Fatalf("a resource port of another backend: %v", err)
		}
		pre := func(s *sharedFx) {
			s.in.Deps.Shared = &execution.SharedConfig{Resources: s.res, Policies: s.list}
		}
		if _, err := AssembleExecution(buildSharedFx(t, Test, cfg, pre).in); !errors.Is(err, execution.ErrNotConfigured) {
			t.Fatalf("a pre-bound resource port: %v", err)
		}
		bound := func(s *sharedFx) { s.in.Auth.Resources = &runtimeauth.ResourceConfig{} }
		if _, err := AssembleExecution(buildSharedFx(t, Test, cfg, bound).in); !errors.Is(err, execution.ErrNotConfigured) {
			t.Fatalf("pre-bound resource subjects: %v", err)
		}
	})

	t.Run("production refuses the Process and Podman backends and every development adapter", func(t *testing.T) {
		// Everything but the resource port is a production-grade stand-in, so the
		// refusal below is the port's own.
		prod := func(backend string) (ExecutionInputs, RuntimeSet) {
			s := buildSharedFx(t, Production, cfg)
			s.res.SetBackend(backend)
			in := s.in
			in.Shared = &SharedInputs{
				Store: struct{ runtimeauth.ResourceStore }{}, State: struct{ runtimeauth.ResourceObserver }{},
				Tokens: s.in.Shared.Tokens, Verifier: s.in.Shared.Verifier,
			}
			set := RuntimeSet{Runtime: s.in.Runtime.Runtime, Shared: &SharedRuntime{Resources: s.res, Workloads: struct{ runtimeauth.ResourceWorkloads }{}, Policies: s.list}}
			return in, set
		}
		for _, backend := range []string{"process", "podman"} {
			if !IsDevelopmentAdapter(executiontest.NewFakeResources(backend)) {
				t.Errorf("the %s resource port is not recognized as a development adapter", backend)
			}
			in, set := prod(backend)
			if err := admitShared(in, set); !errors.Is(err, ErrDevelopmentAdapter) {
				t.Errorf("production admitted a %s resource port: %v", backend, err)
			}
		}
		// The same stand-ins over the Kubernetes backend's name pass this guard (the
		// backend equality with the runtime is the next check and fails here only
		// because the runtime double is the manual backend).
		in, set := prod("kubernetes")
		if err := admitShared(in, set); errors.Is(err, ErrDevelopmentAdapter) {
			t.Errorf("production refused the Kubernetes backend's resource port: %v", err)
		}
		// The in-memory grant store and resource authority are development-only.
		if _, err := AssembleExecution(buildSharedFx(t, Production, cfg).in); !errors.Is(err, ErrDevelopmentAdapter) {
			t.Errorf("production composed the in-memory resource authority: %v", err)
		}
	})

	t.Run("a runtime without a resource port leaves the model closed", func(t *testing.T) {
		f := newFixture(t, Test)
		if f.slice.RuntimeV2 != nil {
			t.Fatal("a v2 surface exists without a resource port")
		}
	})
}

// Test_unit_app_execution_policy_settings: the installation's execution policies
// come from the rendered settings document. The list is append-only (the newest is
// Current; a repeated id or digest is refused), a retained body is resolved by its
// pin and an unretained pin is unavailable (never the current policy), the body is
// decoded strictly and validated, and the section is absent unless configured.
func Test_unit_app_execution_policy_settings(t *testing.T) {
	asMap := func(p execution.ExecutionPolicy) map[string]any {
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	doc := func(policies ...map[string]any) ChartConfig {
		return ChartConfig{RunSharedSection: &ChartRunShared{Policies: policies, ClusterCIDRs: []string{"10.32.0.0/12"}}}
	}
	one, two := policyWith("run-v1", nil), policyWith("run-v2", func(p *execution.ExecutionPolicy) { p.Limits.MaxRunSeconds = 3600 })

	if got, err := (ChartConfig{}).RunShared(); got != nil || err != nil {
		t.Fatalf("an unconfigured document yielded %v %v", got, err)
	}
	got, err := doc(asMap(one), asMap(two)).RunShared()
	if err != nil || got == nil {
		t.Fatalf("two policies: %v", err)
	}
	if got.Current().ID != "run-v2" || len(got.Policies) != 2 || len(got.ClusterCIDRs) != 1 {
		t.Fatalf("current %q of %d", got.Current().ID, len(got.Policies))
	}
	if d1, _ := got.Policies[0].Digest(); func() string { d, _ := one.Digest(); return d }() != d1 {
		t.Fatal("decoding changed a policy's digest")
	}
	list, err := NewPolicyList(*got)
	if err != nil {
		t.Fatal(err)
	}
	if cur, err := list.Current(context.Background()); err != nil || cur.ID != "run-v2" {
		t.Fatalf("Current: %v %v", cur.ID, err)
	}
	pin1, _ := one.Pin()
	if body, err := list.Policy(context.Background(), pin1); err != nil || body.ID != "run-v1" {
		t.Fatalf("the retained body of an older pin: %v", err)
	}
	for name, pin := range map[string]execution.PolicyPin{
		"an unknown id":      {ID: "run-v9", Digest: pin1.Digest},
		"a different digest": {ID: "run-v1", Digest: "sha256:" + strings.Repeat("a", 64)},
		"the zero pin":       {},
	} {
		if _, err := list.Policy(context.Background(), pin); !errors.Is(err, kube.ErrPolicyUnavailable) {
			t.Errorf("%s resolved: %v", name, err)
		}
	}
	if len(list.Pins()) != 2 {
		t.Fatalf("pins %v", list.Pins())
	}

	for name, bad := range map[string]ChartConfig{
		"no policy":                doc(),
		"an id listed twice":       doc(asMap(one), asMap(policyWith("run-v1", func(p *execution.ExecutionPolicy) { p.Limits.MaxServices = 2 }))),
		"the same body twice":      doc(asMap(one), asMap(one)),
		"a policy without a role":  doc(func() map[string]any { m := asMap(one); delete(m["roles"].(map[string]any), "step"); return m }()),
		"an unknown member":        doc(func() map[string]any { m := asMap(one); m["privileged"] = true; return m }()),
		"an unknown nested member": doc(func() map[string]any { m := asMap(one); m["admission"].(map[string]any)["mode"] = "x"; return m }()),
		"an extra capability": doc(func() map[string]any {
			m := asMap(one)
			m["roles"].(map[string]any)["step"].(map[string]any)["capabilitiesAdd"] = []any{"SYS_ADMIN"}
			return m
		}()),
		"an unknown legacy format": doc(func() map[string]any {
			m := asMap(one)
			m["admission"].(map[string]any)["legacyFormat"] = "retired"
			return m
		}()),
		"a mutable preparation image": doc(func() map[string]any {
			m := asMap(one)
			m["roles"].(map[string]any)["preparation"].(map[string]any)["image"] = "registry.example.invalid/prep:latest"
			return m
		}()),
		"a non-RWOP workspace": doc(func() map[string]any {
			m := asMap(one)
			m["workspace"].(map[string]any)["access"] = "ReadWriteOnce"
			return m
		}()),
		"a non-IPv4 cluster CIDR":      {RunSharedSection: &ChartRunShared{Policies: []map[string]any{asMap(one)}, ClusterCIDRs: []string{"fd00::/8"}}},
		"a catch-all cluster CIDR":     {RunSharedSection: &ChartRunShared{Policies: []map[string]any{asMap(one)}, ClusterCIDRs: []string{"0.0.0.0/0"}}},
		"a malformed cluster CIDR":     {RunSharedSection: &ChartRunShared{Policies: []map[string]any{asMap(one)}, ClusterCIDRs: []string{"nonsense"}}},
		"a numeric policy where a map": {RunSharedSection: &ChartRunShared{Policies: []map[string]any{{"id": 7}}}},
	} {
		if cfg, err := bad.RunShared(); err == nil {
			t.Errorf("%s was accepted: %+v", name, cfg)
		}
	}
	if _, err := NewPolicyList(RunSharedConfig{}); err == nil {
		t.Error("an empty list built a policy source")
	}
}

// Test_unit_app_runtime_v2_routes_served: the /runtime/v2 routes are served by
// the composed runtime authority and answer the contract's envelopes. Every
// operation of the v2 contract that has an owner is registered and demands a
// credential; an operation without an owner (the service secret route without a
// resolver) answers not_found; and a resource completes the one-time admission
// exchange over HTTP with the token, context and renewal the contract defines. A
// composition that did not configure run_shared has no v2 handler.
func Test_unit_app_runtime_v2_routes_served(t *testing.T) {
	s := newSharedFx(t, Test, sharedConfig(policyWith("run-open", nil)))
	h := s.slice.RuntimeV2
	do := func(method, path string, body []byte, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	fill := func(p string) string {
		return strings.NewReplacer("{attemptId}", "att_t116", "{runId}", "run_t116", "{resourceKey}", "service.db", "{id}", "art_t116").Replace(p)
	}
	wired := map[string]bool{
		"admit": true, "renewToken": true, "getContext": true, "begin": true, "admitResource": true, "renewResourceToken": true,
		"getResourceContext": true, "completePreparation": true, "proposeSource": true, "submitResult": true, "reserveArtifact": true, "completeArtifact": true,
		// resolveSecret (attempts) is the v1 secret route; neither it nor the service route is wired without a resolver.
		"resolveSecret": false, "resolveResourceSecret": false,
	}
	for _, op := range runtimev2.Operations {
		path := runtimeV2Prefix + strings.TrimPrefix(fill(pathOf(op)), "/runtime/v2")
		rec := do(op.Method, path, []byte(`{}`), "")
		unwired := rec.Code == http.StatusNotFound
		want, known := wired[op.ID]
		if !known {
			t.Errorf("operation %s has no expectation", op.ID)
			continue
		}
		if unwired == want {
			t.Errorf("%s %s: status %d, wired=%v", op.Method, path, rec.Code, want)
		}
		if want && rec.Code != http.StatusUnauthorized && rec.Code != http.StatusBadRequest {
			t.Errorf("%s %s without a credential: %d %s", op.Method, path, rec.Code, rec.Body)
		}
	}
	if rec := do("GET", "/runtime/v2/nonsense", nil, ""); rec.Code != http.StatusNotFound {
		t.Errorf("an unknown path: %d", rec.Code)
	}

	// A preparation completes admission over HTTP: the committed state, the
	// registered credential, the verified Pod and the signed token meet.
	ref := runtimeauth.ResourceRef{Scope: fxScope, RunID: "run_t116", Key: "service.db"}
	f := rtest.ResourceFixture{Scope: ref.Scope, RunID: ref.RunID, WorkloadUID: "8b3e3f40-5c6d-4e7f-8a01-2c3d4e5f6071", DeadlineAt: time.Now().Add(time.Hour)}
	s.rr.Put(rtest.ServiceState(f, "db", nil))
	st, _ := s.rr.ObserveResource(context.Background(), ref)
	cred, _ := runtimeauth.GenerateCredential(nil)
	if err := s.slice.Auth.RegisterResource(context.Background(), runtimeauth.ResourceRegisterCommand{
		Scope: ref.Scope, RunID: ref.RunID, Key: ref.Key, Credential: cred, SpecFingerprint: st.Fingerprint, ExpiresAt: st.DeadlineAt,
	}); err != nil {
		t.Fatal(err)
	}
	base := "/runtime/v2/runs/run_t116/resources/service.db"
	areq, _ := json.Marshal(runtimev2.ResourceAdmitRequest{
		AdmissionCredential: cred.Reveal(), Nonce: "nnnnnnnnnnnnnnnnnnnnnn", ClientVersion: "1.0.0",
		Identity: runtimev2.ResourceIdentity{WorkloadName: st.WorkloadName, WorkloadUID: st.WorkloadUID},
	})
	rec := do("POST", base+"/admit", areq, "")
	if rec.Code != 200 {
		t.Fatalf("resource admit: %d %s", rec.Code, rec.Body)
	}
	resp, err := runtimev2.DecodeResponse[runtimev2.ResourceAdmitResponse](rec.Body.Bytes())
	if err != nil || resp.Token.Audience != runtimev2.ResourceAudience {
		t.Fatalf("admit response: %v %+v", err, resp.Token)
	}
	if rec := do("GET", base+"/context", nil, resp.Token.Token); rec.Code != 200 {
		t.Fatalf("resource context: %d %s", rec.Code, rec.Body)
	}
	if rec := do("POST", base+"/token", []byte(`{}`), resp.Token.Token); rec.Code != 200 {
		t.Fatalf("resource renew: %d %s", rec.Code, rec.Body)
	}
	// A resource token is never an attempt token, and the reverse.
	if rec := do("GET", "/runtime/v2/attempts/att_t116/context", nil, resp.Token.Token); rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized && rec.Code != http.StatusNotFound {
		t.Fatalf("a resource token on an attempt route: %d %s", rec.Code, rec.Body)
	}
	// After the stop the same token reaches nothing.
	s.rr.Stop(ref, true, time.Now())
	if rec := do("GET", base+"/context", nil, resp.Token.Token); rec.Code != http.StatusForbidden {
		t.Fatalf("a stopped resource kept its authority: %d %s", rec.Code, rec.Body)
	}

	// The v1 surface still answers, and nothing is composed without the opt-in.
	if rec := httptest.NewRecorder(); true {
		s.slice.Runtime.ServeHTTP(rec, httptest.NewRequest("POST", "/runtime/v1/attempts/att_t116/result", strings.NewReader(`{}`)))
		if rec.Code == http.StatusNotFound {
			t.Fatal("the v1 result route disappeared")
		}
	}
	if f := newFixture(t, Test); f.slice.RuntimeV2 != nil {
		t.Fatal("a v2 handler exists without run_shared")
	}

	// The Core mounts /runtime/v2 only when composed.
	mounted := (&Core{runtime: http.NotFoundHandler(), runtimeV2: h, ready: &readiness{}}).Handler()
	rr := httptest.NewRecorder()
	mounted.ServeHTTP(rr, httptest.NewRequest("GET", "/runtime/v2/nonsense", nil))
	if rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), "not_found") {
		t.Fatalf("the core does not mount the v2 handler: %d %s", rr.Code, rr.Body)
	}
}

// pathOf is the path of a v2 operation under the runtime prefix.
func pathOf(op runtimev2.Operation) string {
	if op.Subject == runtimev2.SubjectResource {
		return runtimev2.ResourceBasePath + op.Path
	}
	return runtimev2.AttemptBasePath + op.Path
}

// ---- prepare and service-run -----------------------------------------------------------

// Test_unit_app_prepare_and_service_run_commands: the binary dispatches the
// in-workload commands of the run_shared model beside `step`, with no server
// configuration: they read no environment selection, ask for no database or
// secret, and answer usage for what they were not given.
func Test_unit_app_prepare_and_service_run_commands(t *testing.T) {
	getenv := func(string) string { return "" }
	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := Main(context.Background(), args, getenv, &out, &errb)
		return code, out.String(), errb.String()
	}
	if code, out, _ := run("help"); code != 0 || !strings.Contains(out, "prepare") || !strings.Contains(out, "service-run") || !strings.Contains(out, "step") {
		t.Fatalf("help: %d %q", code, out)
	}
	names := map[string]bool{}
	for _, c := range workloadCommands() {
		names[c.Name] = true
		if c.Run == nil || c.Summary == "" {
			t.Errorf("command %q is incomplete", c.Name)
		}
	}
	if !names["step"] || !names["prepare"] || !names["service-run"] || len(names) != 3 {
		t.Fatalf("workload commands: %v", names)
	}
	for _, name := range []string{"prepare", "service-run"} {
		// --help is served by the command itself, not by the server's usage.
		code, out, errb := run(name, "--help")
		if code != 0 || !strings.Contains(out+errb, "Usage") || strings.Contains(out+errb, "kodama server") {
			t.Errorf("%s --help: %d %q %q", name, code, out, errb)
		}
		// Without its environment the command answers a usage error and starts nothing:
		// the server configuration (KODAMA_ENV, a database URL) is not what it asked for.
		code, _, errb = run(name)
		if code == 0 || strings.Contains(errb, "KODAMA_ENV") {
			t.Errorf("%s without configuration: %d %q", name, code, errb)
		}
	}
	// service-run needs a command after `--`.
	if code, _, errb := run("service-run", "--run-id", "run_1", "--resource-key", "service.db"); code == 0 || errb == "" {
		t.Errorf("service-run without a command: %d %q", code, errb)
	}
	// An unknown command is still the server's usage error.
	if code, _, errb := run("prepared"); code != 2 || !strings.Contains(errb, "Usage") {
		t.Errorf("an unknown command: %d %q", code, errb)
	}
}

// ---- the disclosure plumbing -------------------------------------------------------------

// Test_unit_app_run_shared_disclosures: the bindings a run disclosed to its other
// attempts and to every service are enumerated from committed state under either
// runtime wire, and a context of neither wire is unreadable, never an empty set.
func Test_unit_app_run_shared_disclosures(t *testing.T) {
	scope := fxScope
	secretA := runtimev1.SecretBinding{Ref: "secret://project/a", Version: 1, Name: "A", Delivery: runtimev1.DeliveryFile}
	secretB := runtimev1.SecretBinding{Ref: "secret://project/b", Version: 2, Name: "B", Delivery: runtimev1.DeliveryFile}
	ctxOf := func(attempt domain.AttemptID, secrets ...runtimev1.SecretBinding) []byte {
		c := kubetest.Context(t, kubetest.ContextSpec{Tenant: scope.TenantID, Project: scope.ProjectID, Run: "run_t116", Step: "build", Attempt: attempt, Image: kubetest.WorkloadImage})
		c.Secrets = append([]runtimev1.SecretBinding{}, secrets...)
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	agg := &execution.Aggregate{
		Run: &execution.Run{Scope: scope, ID: "run_t116", Shared: &execution.RunShared{}},
		Attempts: []*execution.Attempt{
			{Scope: scope, RunID: "run_t116", ID: "att_one", Context: ctxOf("att_one", secretA)},
			{Scope: scope, RunID: "run_t116", ID: "att_two", Context: ctxOf("att_two", secretB)},
			{Scope: scope, RunID: "run_t116", ID: "att_unbegun", Context: ctxOf("att_unbegun", secretB)},
		},
		Begins: []*execution.BeginRecord{{AttemptID: "att_one"}, {AttemptID: "att_two"}},
		Resources: []*execution.RunResource{
			{Kind: execution.KindService, Key: "service.db", Spec: execution.ResourceSpec{Service: &execution.ServiceSpec{Secrets: []execution.SecretPin{{Ref: "secret://project/db", Version: 7, Name: "PW", Delivery: runtimev1.DeliveryEnv}}}}},
			{Kind: execution.KindWorkspace, Key: "workspace"},
		},
	}
	refs := func(b []runtimev1.SecretBinding) string {
		var out []string
		for _, x := range b {
			out = append(out, x.Ref)
		}
		return strings.Join(out, ",")
	}
	// For attempt one: attempt two (begun) and the service; never itself and never an unbegun attempt.
	got, err := runDisclosed(agg, "att_one")
	if err != nil || refs(got) != "secret://project/b,secret://project/db" {
		t.Fatalf("disclosed to everything but att_one: %v %v", refs(got), err)
	}
	if all, err := runDisclosed(agg, ""); err != nil || refs(all) != "secret://project/a,secret://project/b,secret://project/db" {
		t.Fatalf("every binding of the run: %v %v", refs(all), err)
	}
	agg.Attempts[0].Context = []byte(`{"not":"a context"}`)
	if _, err := runDisclosed(agg, "att_two"); err == nil {
		t.Fatal("an unreadable pinned context yielded an answer")
	}

	// The pinned context of a v2 attempt is read the same way.
	sc := rtest.LoadV2Scenario(t, "begin_clean_then_result_hash_is_the_v1_hash")
	var v2raw []byte
	for _, st := range sc.Steps {
		if st.Op == "admit" && st.ResourceKind == "" {
			var resp struct {
				Context json.RawMessage `json:"context"`
			}
			if err := json.Unmarshal(st.Body, &resp); err == nil && len(resp.Context) > 0 {
				v2raw = resp.Context
				break
			}
		}
	}
	if v2raw == nil {
		t.Fatal("no v2 context in the scenario")
	}
	p, err := decodePinned(v2raw)
	if err != nil || !p.Shared || p.RunID == "" || p.SourceCommit == "" {
		t.Fatalf("v2 context: %+v %v", p, err)
	}
	if v1, err := decodePinned(ctxOf("att_three")); err != nil || v1.Shared {
		t.Fatalf("v1 context: %+v %v", v1, err)
	}
	if _, err := decodePinned([]byte(`{}`)); err == nil {
		t.Fatal("a context of neither wire was readable")
	}

	// A resource authority reads absence and outages as such.
	if !errors.Is(classifyResourceRead(execution.ErrNotFound), runtimeauth.ErrNotFound) || !errors.Is(classifyResourceRead(errors.New("db down")), runtimeauth.ErrUnavailable) {
		t.Fatal("resource reads are not classified")
	}
}

// ---- the bridges ----------------------------------------------------------------------------

type stubResourceStore struct {
	agg *execution.Aggregate
	err error
	// locked counts the transaction-bound reads.
	locked int
}

func (s *stubResourceStore) Get(context.Context, domain.Scope, domain.RunID) (*execution.Aggregate, error) {
	return s.agg, s.err
}

func (s *stubResourceStore) UpdateInTx(ctx context.Context, _ pgx.Tx, _ domain.Scope, _ domain.RunID, opts execution.UpdateOptions, fn execution.Mutation) (*execution.Aggregate, any, error) {
	s.locked++
	if opts != (execution.UpdateOptions{}) {
		panic("the locked read takes no quota or work locks")
	}
	if s.err != nil {
		return nil, nil, s.err
	}
	_, err := fn(ctx, nil, s.agg)
	return s.agg, nil, err
}

// Test_unit_app_run_shared_bridges: the resource authority reads execution's
// committed state under the run's lock and classifies what it cannot read (a
// missing resource is absence, an outage is never absence); the Pod verifier
// maps the adapter's definite "not ours" to runtimeauth's not-observed and an
// unreadable cluster to nothing conclusive; and a service's secrets resolve only
// for its own pins and only while it holds current authority.
func Test_unit_app_run_shared_bridges(t *testing.T) {
	ctx := context.Background()
	ref := runtimeauth.ResourceRef{Scope: fxScope, RunID: "run_t116", Key: "preparation"}
	agg := &execution.Aggregate{Run: &execution.Run{Scope: fxScope, ID: "run_t116", Shared: &execution.RunShared{}}}

	store := &stubResourceStore{agg: agg}
	auth, err := newResourceAuthority(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.LockResource(ctx, nil, ref); !errors.Is(err, runtimeauth.ErrNotFound) || store.locked != 1 {
		t.Fatalf("an absent resource: %v (locked reads %d)", err, store.locked)
	}
	if _, err := auth.ObserveResource(ctx, ref); !errors.Is(err, runtimeauth.ErrNotFound) {
		t.Fatalf("observe an absent resource: %v", err)
	}
	if _, err := auth.LockResource(ctx, nil, runtimeauth.ResourceRef{}); !errors.Is(err, runtimeauth.ErrInvalidRequest) && err == nil {
		t.Fatalf("a malformed ref: %v", err)
	}
	store.err = errors.New("database down")
	if _, err := auth.LockResource(ctx, nil, ref); !errors.Is(err, runtimeauth.ErrUnavailable) || errors.Is(err, runtimeauth.ErrNotFound) {
		t.Fatalf("an outage must not read as absence: %v", err)
	}
	store.err = execution.ErrHidden
	if _, err := auth.ObserveResource(ctx, ref); !errors.Is(err, runtimeauth.ErrNotFound) {
		t.Fatalf("a hidden run: %v", err)
	}
	if _, err := newResourceAuthority(nil, nil); err == nil {
		t.Fatal("an authority without a store was built")
	}

	// Pod verification.
	notOwned := kubeWorkloads{rr: &podVerifier{err: kube.ErrPodNotOwned}}
	w := runtimeauth.ResourceWorkload{Ref: ref, Name: "kr-0123456789abcdef01234567", UID: "uid"}
	if err := notOwned.VerifyPod(ctx, w, "pod", "puid"); !errors.Is(err, runtimeauth.ErrNotObserved) {
		t.Fatalf("a Pod that is not the workload's: %v", err)
	}
	outage := kubeWorkloads{rr: &podVerifier{err: kube.ErrUnavailable}}
	if err := outage.VerifyPod(ctx, w, "pod", "puid"); err == nil || errors.Is(err, runtimeauth.ErrNotObserved) {
		t.Fatalf("an unreadable cluster concluded %v", err)
	}
	ok := &podVerifier{}
	if err := (kubeWorkloads{rr: ok}).VerifyPod(ctx, w, "pod", "puid"); err != nil || ok.got.Key != "preparation" || ok.got.Kind != execution.KindPreparation || ok.got.UID != "uid" || ok.got.Name != w.Name {
		t.Fatalf("handle %+v: %v", ok.got, err)
	}
	if err := (kubeWorkloads{rr: ok}).VerifyPod(ctx, runtimeauth.ResourceWorkload{Ref: runtimeauth.ResourceRef{Key: "volume"}}, "p", "u"); err == nil {
		t.Fatal("a key that is no workload was verified")
	}
}

type podVerifier struct {
	err error
	got execution.ResourceHandle
}

func (p *podVerifier) VerifyResourcePod(_ context.Context, h execution.ResourceHandle, _, _ string) error {
	p.got = h
	return p.err
}

type reauth struct{ err error }

func (r reauth) ReauthorizeResource(context.Context, runtimeauth.ResourceAuthorization) error {
	return r.err
}

// Test_unit_app_run_shared_service_secrets: a service resolves exactly the
// bindings its committed context pins, with the runtime contract's error codes and
// no detail, and loses the ability the moment its authority ends.
func Test_unit_app_run_shared_service_secrets(t *testing.T) {
	ctx := context.Background()
	src := pubSecrets{values: map[string]string{"db-password": "hunter2"}}
	resolver, err := secrets.NewResolver(denyAttempts{}, src)
	if err != nil {
		t.Fatal(err)
	}
	pin := runtimev1.SecretBinding{Ref: "secret://project/db-password", Version: 1, Name: "PW", Delivery: runtimev1.DeliveryEnv}
	authz := runtimeauth.ResourceAuthorization{
		Ref:     runtimeauth.ResourceRef{Scope: fxScope, RunID: "run_t116", Key: "service.db"},
		Context: runtimev2.ResourceContext{Service: &runtimev2.ServiceContext{Name: "db", Secrets: []runtimev2.SecretBinding{pin}}},
	}
	resolve := func(a error, ref string, version int64) (runtimev1.SecretResolveResponse, error) {
		return resourceSecrets{resolver: resolver, auth: reauth{err: a}}.ResolveResourceSecret(ctx, authz, runtimev1.SecretResolveRequest{Ref: ref, Version: version}, "req_t116")
	}
	resp, err := resolve(nil, pin.Ref, 1)
	if err != nil || resp.Name != "PW" || resp.Ref != pin.Ref || resp.ValueBase64 == "" {
		t.Fatalf("a pinned binding: %+v %v", resp, err)
	}
	code := func(err error) domain.ErrorCode {
		var ce *runtimev1.ContractError
		if !errors.As(err, &ce) {
			t.Fatalf("not a contract error: %v", err)
		}
		if strings.Contains(ce.Message, "hunter2") || strings.Contains(ce.Message, pin.Ref) {
			t.Fatalf("the message names the secret: %q", ce.Message)
		}
		return ce.Code
	}
	if _, err := resolve(nil, "secret://project/other", 1); code(err) != domain.CodeForbidden {
		t.Fatalf("an unpinned reference: %v", err)
	}
	if _, err := resolve(nil, pin.Ref, 2); code(err) != domain.CodeForbidden {
		t.Fatalf("another version of a pinned reference: %v", err)
	}
	if _, err := resolve(runtimeauth.ErrResourceRevoked, pin.Ref, 1); code(err) != runtimev1.CodeAttemptRevoked {
		t.Fatalf("a stopped service: %v", err)
	}
	if _, err := resolve(runtimeauth.ErrInvalidCredential, pin.Ref, 1); code(err) != domain.CodeInvalidCredential {
		t.Fatalf("a lost credential: %v", err)
	}
	// An authority outage fails closed. (The secrets module reports it as an
	// unauthenticated caller, which it does for every authority failure.)
	if out, err := resolve(runtimeauth.ErrUnavailable, pin.Ref, 1); err == nil || out.ValueBase64 != "" || code(err) == "" {
		t.Fatalf("an authority outage released %+v: %v", out, err)
	}
	// A preparation has no bindings at all.
	prep := authz
	prep.Context = runtimev2.ResourceContext{}
	prep.Ref.Key = "preparation"
	if _, err := (resourceSecrets{resolver: resolver, auth: reauth{}}).ResolveResourceSecret(ctx, prep, runtimev1.SecretResolveRequest{Ref: pin.Ref, Version: 1}, "req"); err == nil {
		t.Fatal("a preparation resolved a secret")
	}
	// The attempt path of this resolver has no authority.
	if _, err := resolver.Resolve(ctx, "att_1", runtimev1.SecretResolveRequest{Ref: pin.Ref, Version: 1}, "req"); !errors.Is(err, secrets.ErrAuthorityMissing) {
		t.Fatalf("the attempt path: %v", err)
	}
}

// Test_unit_app_kube_backend_declares_run_resources: the Kubernetes attempt runtime
// advertises the original six guarantees only, and every run_shared run requires
// the run-resources marker too, so the composed runtime declares it (and never
// more than the marker); the late registrar carries the v2 wire.
func Test_unit_app_kube_backend_declares_run_resources(t *testing.T) {
	rt := (&kube.Runtime{}).Capabilities()
	if rt.Guarantees&execution.RunResourcesMarker != 0 {
		t.Skip("the Kubernetes runtime already declares the marker")
	}
	got := withRunResources{Runtime: &kube.Runtime{}}.Capabilities()
	if got.Backend != rt.Backend || got.Guarantees != rt.Guarantees.WithRunResources() || got.ProvesNeverStarted != rt.ProvesNeverStarted {
		t.Fatalf("capabilities %+v from %+v", got, rt)
	}
	if _, err := (&LateRegistrar{}).RegisterV2(context.Background(), runtimeauth.RegisterCommandV2{}); !errors.Is(err, runtimeauth.ErrUnavailable) {
		t.Fatal("an unbound registrar accepted a run_shared registration")
	}
}

// Test_unit_app_policy_list_owns_its_bodies: a retained body is immutable under
// its pin. The list copies deeply (maps, slices, nested egress destinations and
// their ports) when it is built and on every read, so neither the configuration the
// caller kept, nor a body one reader received, reaches what the next resolves; an
// empty collection stays empty and an absent one stays absent (the digest tells
// them apart).
func Test_unit_app_policy_list_owns_its_bodies(t *testing.T) {
	ctx := context.Background()
	build := func(id string) execution.ExecutionPolicy {
		return policyWith(id, func(p *execution.ExecutionPolicy) {
			step := p.Roles[execution.RoleStep]
			step.CapabilitiesAdd = []string{"SETUID"}
			step.Devices = []string{}
			p.Roles[execution.RoleStep] = step
			p.EgressProfiles = map[string]execution.EgressProfile{
				"registry": {Destinations: []execution.EgressDestination{{Host: "registry.example.test", Ports: []int64{443}}, {Host: "git.example.test", Ports: []int64{}}}},
			}
		})
	}
	// The caller's configuration shares nothing with the reference bodies.
	one, two := build("run-v1"), build("run-v2")
	cfg := RunSharedConfig{Policies: []execution.ExecutionPolicy{build("run-v1"), build("run-v2")}, ClusterCIDRs: []string{"10.32.0.0/12"}}
	list, err := NewPolicyList(cfg)
	if err != nil {
		t.Fatal(err)
	}
	pin1, _ := one.Pin()
	pin2, _ := two.Pin()
	want1, _ := one.Canonical()

	vandalize := func(p execution.ExecutionPolicy) {
		for k, r := range p.Roles {
			if len(r.CapabilitiesAdd) > 0 {
				r.CapabilitiesAdd[0] = "SYS_ADMIN"
			}
			r.Image = "registry.example.invalid/evil:latest"
			p.Roles[k] = r
		}
		p.Roles["extra"] = execution.RoleShape{}
		for _, prof := range p.EgressProfiles {
			prof.Destinations[0].Host = "evil.example.test"
			prof.Destinations[0].Ports[0] = 22
			prof.Destinations = append(prof.Destinations[:0], execution.EgressDestination{Host: "evil.example.test"})
		}
		p.EgressProfiles["evil"] = execution.EgressProfile{}
		delete(p.EgressProfiles, "registry")
	}
	check := func(when string) {
		t.Helper()
		for _, pin := range []execution.PolicyPin{pin1, pin2} {
			got, err := list.Policy(ctx, pin)
			if err != nil {
				t.Fatalf("%s: %v", when, err)
			}
			if gp, err := got.Pin(); err != nil || gp != pin {
				t.Fatalf("%s: the body of %s changed under its pin (%v)", when, pin.ID, err)
			}
		}
		cur, _ := list.Current(ctx)
		if cp, _ := cur.Pin(); cp != pin2 {
			t.Fatalf("%s: current changed", when)
		}
		if raw, _ := mustPolicy(t, list, pin1).Canonical(); !bytes.Equal(raw, want1) {
			t.Fatalf("%s: the canonical body changed", when)
		}
	}

	// The configuration the caller kept.
	vandalize(cfg.Policies[0])
	vandalize(cfg.Policies[1])
	check("after mutating the configuration")
	// A body one reader received.
	got, _ := list.Policy(ctx, pin1)
	vandalize(got)
	cur, _ := list.Current(ctx)
	vandalize(cur)
	check("after mutating the bodies read")
	// RunSharedConfig.Current is a copy too.
	fresh := RunSharedConfig{Policies: []execution.ExecutionPolicy{build("run-v1"), build("run-v2")}}
	vandalize(fresh.Current())
	if cp, _ := fresh.Current().Pin(); cp != pin2 {
		t.Fatal("the configuration's current policy was reached through its copy")
	}

	// Shape is preserved exactly: nil stays nil and empty stays empty.
	c := mustPolicy(t, list, pin1)
	if c.Roles[execution.RoleStep].Devices == nil || len(c.Roles[execution.RoleStep].Devices) != 0 {
		t.Fatal("an empty slice did not stay empty")
	}
	if !reflect.DeepEqual(c, one) {
		t.Fatalf("the copy is not equal to the body:\n%+v\n%+v", c, one)
	}
	nilp := policyWith("run-nil", func(p *execution.ExecutionPolicy) { p.EgressProfiles = nil })
	nl, err := NewPolicyList(RunSharedConfig{Policies: []execution.ExecutionPolicy{nilp}})
	if err != nil {
		t.Fatal(err)
	}
	np, _ := nilp.Pin()
	if body := mustPolicy(t, nl, np); body.EgressProfiles != nil {
		t.Fatal("an absent map became present")
	}
}

func mustPolicy(t *testing.T, l *PolicyList, pin execution.PolicyPin) execution.ExecutionPolicy {
	t.Helper()
	p, err := l.Policy(context.Background(), pin)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
