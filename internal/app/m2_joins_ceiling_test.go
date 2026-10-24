package app

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/agents/agentstest"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
)

var ceilingScope = domain.Scope{TenantID: "t101_ceiling", ProjectID: "p101_ceiling"}

// profileStore serves stored profile versions by their immutable reference, in
// the scope that owns them only (as agents.Service.Stored does).
type profileStore struct {
	mu       sync.Mutex
	versions map[string]agents.ProfileVersion
	err      error
	reads    int
}

func (p *profileStore) Stored(_ context.Context, scope domain.Scope, ref string) (agents.ProfileVersion, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reads++
	if p.err != nil {
		return agents.ProfileVersion{}, p.err
	}
	if v, ok := p.versions[ref]; ok && v.Scope == scope {
		return v, nil
	}
	return agents.ProfileVersion{}, agents.ErrNotFound
}

func (p *profileStore) add(t *testing.T, scope domain.Scope, name string, mutate func(*agents.Draft)) {
	t.Helper()
	d := agentstest.Draft(t, name)
	if mutate != nil {
		mutate(&d)
	}
	v, err := agents.NewProfileVersion(agents.Candidate{
		Scope: scope, ID: agents.ProfileID("ap_" + name), Content: agentstest.Content(t, d),
		CreatedBy: agents.Actor{ID: "u_author", Type: agents.ActorUser}, CreatedAt: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC),
	}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if p.versions == nil {
		p.versions = map[string]agents.ProfileVersion{}
	}
	p.versions[v.Ref()] = v
}

// ceilingCompatible: the real agent service is what the composition hands the
// resolver.
var _ StoredProfiles = (*agents.Service)(nil)

func ceilingFixture(t *testing.T) *profileStore {
	t.Helper()
	ps := &profileStore{}
	ps.add(t, ceilingScope, "coding", nil)                                            // a model and tools
	ps.add(t, ceilingScope, "reader", func(d *agents.Draft) { d.Tools = []string{} }) // a model, no tool
	// A profile of another project with the same name must never answer.
	ps.add(t, domain.Scope{TenantID: "t101_other", ProjectID: "p101_other"}, "foreign", nil)
	return ps
}

// ceilingStatement is the operator's statement: agents may hold the model and
// tool actions, plain steps the deployment one.
const ceilingStatement = `{"kinds": {"agent": {"grants": ["model.invoke", "tool.invoke"]}, "container": {"grants": ["deployment.call"]}}}`

func ceilingConfig(t *testing.T) ActionCeilingConfig {
	t.Helper()
	var c ActionCeilingConfig
	if err := readStrictJSON(writeTemp(t, "action-ceiling.json", ceilingStatement), "action ceiling file", &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	p := t.TempDir() + "/" + name
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// attemptContext is a pinned context the way execution freezes it.
func attemptContext(t *testing.T, attempt string, mutate func(*runtimev1.Context)) runtimev1.Context {
	t.Helper()
	c := runtimev1.Context{
		TenantID: ceilingScope.TenantID, ProjectID: ceilingScope.ProjectID, RunID: "run_ceiling", StepID: "step", AttemptID: domain.AttemptID(attempt),
		ActionKey:  execution.ActionKey("run_ceiling", "step"),
		Definition: runtimev1.Definition{ID: "pd_1", Digest: runtimev1.Digest("sha256:" + strings.Repeat("2", 64))},
		Command:    runtimev1.Command{Image: "registry.invalid/x@sha256:" + strings.Repeat("0", 64), Command: []string{"true"}, Args: []string{}, Env: map[string]string{}},
		Workspace:  runtimev1.Workspace{Access: runtimev1.AccessNone},
		Inputs:     map[string]runtimev1.Value{}, Outputs: []runtimev1.OutputDecl{}, Secrets: []runtimev1.SecretBinding{},
		Limits: runtimev1.Limits{TimeoutSeconds: 60, MaxOutputBytes: 1 << 20, MaxOutputFiles: 16, MaxDiagnosticBytes: 1 << 20},
	}
	if mutate != nil {
		mutate(&c)
	}
	return c
}

func agentContext(t *testing.T, profile string) func(*runtimev1.Context) {
	return func(c *runtimev1.Context) {
		b := agentstest.Baseline(t)
		c.DesignBaseline = &b
		c.Profile = &runtimev1.Profile{Name: profile, Version: 1, WrapperVersion: "1.4.2"}
	}
}

func deploymentContext(c *runtimev1.Context) {
	c.Environment = &runtimev1.EnvironmentPin{ID: "env_staging", ConfigDigest: runtimev1.Digest("sha256:" + strings.Repeat("3", 64))}
}

func attemptSpec(t *testing.T, c runtimev1.Context) execution.AttemptSpec {
	t.Helper()
	plan, err := execution.DefaultPlanner{}.Plan(c.TenantID, c)
	if err != nil {
		t.Fatal(err)
	}
	return execution.AttemptSpec{
		Scope: c.Scope(), RunID: c.RunID, StepID: c.StepID, AttemptID: c.AttemptID, ActionKey: c.ActionKey,
		WorkloadName: plan.WorkloadName, SpecFingerprint: plan.SpecFingerprint, ContextDigest: plan.ContextDigest, Context: plan.Context,
		Access: execution.AccessNone,
	}
}

// recordingRegistrar delegates to the real runtime-authority service and keeps
// the command: what the runtime registered and whether runtime admission
// accepted it against the attempt authority's own action list.
type recordingRegistrar struct {
	svc  *runtimeauth.Service
	mu   sync.Mutex
	cmds []runtimeauth.RegisterCommand
}

func (r *recordingRegistrar) Register(ctx context.Context, cmd runtimeauth.RegisterCommand) (runtimeauth.Registration, error) {
	r.mu.Lock()
	r.cmds = append(r.cmds, cmd)
	r.mu.Unlock()
	return r.svc.Register(ctx, cmd)
}

// issue drives the issuer the runtime settings build, against the real
// runtime-authority service whose attempt authority allows exactly what
// allowed says (the production attempt authority's action policy).
func issue(t *testing.T, s RuntimeSettings, c runtimev1.Context, allowed func(execution.Access) (runtimeauth.ActionSet, error)) ([]string, error) {
	t.Helper()
	stack := rtest.NewMemStack(t)
	if allowed == nil {
		allowed = func(a execution.Access) (runtimeauth.ActionSet, error) {
			return runtimeauth.NewActionSet(runtimeauth.DefaultActions(a == execution.AccessWrite)...)
		}
	}
	set, err := allowed(execution.AccessNone)
	if err != nil {
		t.Fatal(err)
	}
	stack.SeedAttempt(c, set)
	rec := &recordingRegistrar{svc: stack.Service}
	s.Registrar = rec
	if err := s.issuer().Issue(context.Background(), attemptSpec(t, c), t.TempDir()); err != nil {
		return nil, err
	}
	if len(rec.cmds) != 1 {
		t.Fatalf("%d registrations", len(rec.cmds))
	}
	return rec.cmds[0].Actions.Strings(), nil
}

func actionsWith(extra ...runtimeauth.Action) []string {
	set, err := runtimeauth.NewActionSet(append(runtimeauth.DefaultActions(false), extra...)...)
	if err != nil {
		panic(err)
	}
	return set.Strings()
}

// Test_app_action_ceiling_bound: the core issues an agent attempt the
// operator's ceiling actions (as far as its pinned profile allows them) and a
// plain step only the defaults; the attempt authority allows exactly the
// registration; and every missing or degraded input leaves the defaults.
func Test_app_action_ceiling_bound(t *testing.T) {
	profiles := ceilingFixture(t)
	resolver, err := NewProfileCapabilities(profiles)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := NewCeilingBinding(ceilingConfig(t), resolver)
	if err != nil || binding == nil {
		t.Fatalf("binding %v, %v", binding, err)
	}
	settings := RuntimeSettings{Ceiling: binding}

	t.Run("an agent attempt receives the ceiling's actions", func(t *testing.T) {
		got, err := issue(t, settings, attemptContext(t, "att_agent", agentContext(t, "coding")), binding.Allowed)
		if err != nil {
			t.Fatal(err)
		}
		if want := actionsWith(runtimeauth.ActionModelInvoke, runtimeauth.ActionToolInvoke); !slices.Equal(got, want) {
			t.Fatalf("registered %v, want %v", got, want)
		}
	})

	t.Run("an agent receives only what its own pinned profile names", func(t *testing.T) {
		got, err := issue(t, settings, attemptContext(t, "att_reader", agentContext(t, "reader")), binding.Allowed)
		if err != nil {
			t.Fatal(err)
		}
		if want := actionsWith(runtimeauth.ActionModelInvoke); !slices.Equal(got, want) {
			t.Fatalf("a profile without a tool registered %v, want %v", got, want)
		}
	})

	t.Run("a plain step receives only the defaults", func(t *testing.T) {
		got, err := issue(t, settings, attemptContext(t, "att_plain", nil), binding.Allowed)
		if err != nil {
			t.Fatal(err)
		}
		if want := actionsWith(); !slices.Equal(got, want) {
			t.Fatalf("a plain step registered %v, want %v", got, want)
		}
	})

	t.Run("a deployment-declaring step may call the deployment interface", func(t *testing.T) {
		got, err := issue(t, settings, attemptContext(t, "att_deploy", deploymentContext), binding.Allowed)
		if err != nil {
			t.Fatal(err)
		}
		if want := actionsWith(runtimeauth.ActionDeploymentCall); !slices.Equal(got, want) {
			t.Fatalf("registered %v, want %v", got, want)
		}
	})

	t.Run("an unknown, foreign or unreadable profile keeps the defaults", func(t *testing.T) {
		for name, profile := range map[string]string{"unknown": "ghost", "other project's": "foreign"} {
			got, err := issue(t, settings, attemptContext(t, "att_"+strings.ReplaceAll(profile, " ", ""), agentContext(t, profile)), binding.Allowed)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if !slices.Equal(got, actionsWith()) {
				t.Errorf("%s profile registered %v", name, got)
			}
		}
		down := &profileStore{err: errors.New("database down")}
		r, _ := NewProfileCapabilities(down)
		b, err := NewCeilingBinding(ceilingConfig(t), r)
		if err != nil {
			t.Fatal(err)
		}
		got, err := issue(t, RuntimeSettings{Ceiling: b}, attemptContext(t, "att_down", agentContext(t, "coding")), b.Allowed)
		if err != nil || !slices.Equal(got, actionsWith()) {
			t.Fatalf("an unreadable profile registered %v, %v", got, err)
		}
	})

	t.Run("without a statement every attempt holds the defaults", func(t *testing.T) {
		b, err := NewCeilingBinding(ActionCeilingConfig{}, resolver)
		if err != nil || b != nil {
			t.Fatalf("an empty statement bound a ceiling: %v %v", b, err)
		}
		b, err = NewCeilingBinding(ActionCeilingConfig{Kinds: map[string]KindCeilingConfig{"agent": {}}}, resolver)
		if err != nil || b != nil {
			t.Fatalf("a statement without grants bound a ceiling: %v %v", b, err)
		}
		none := RuntimeSettings{}
		if none.attemptActions() != nil {
			t.Fatal("the attempt authority has an action policy without a ceiling")
		}
		got, err := issue(t, none, attemptContext(t, "att_nostatement", agentContext(t, "coding")), nil)
		if err != nil || !slices.Equal(got, actionsWith()) {
			t.Fatalf("registered %v, %v", got, err)
		}
	})

	t.Run("the attempt authority must allow the ceiling", func(t *testing.T) {
		// The default-only authority refuses the registration of a ceiling above
		// it: both halves must be bound from the one statement.
		_, err := issue(t, settings, attemptContext(t, "att_split", agentContext(t, "coding")), nil)
		if !errors.Is(err, runtimeauth.ErrForbidden) {
			t.Fatalf("a ceiling above the attempt authority registered: %v", err)
		}
	})

	t.Run("the authority's list is the defaults plus the granted actions", func(t *testing.T) {
		for _, access := range []execution.Access{execution.AccessNone, execution.AccessRead, execution.AccessWrite} {
			set, err := binding.Allowed(access)
			if err != nil {
				t.Fatal(err)
			}
			want, _ := runtimeauth.NewActionSet(append(runtimeauth.DefaultActions(access == execution.AccessWrite),
				runtimeauth.ActionModelInvoke, runtimeauth.ActionToolInvoke, runtimeauth.ActionDeploymentCall)...)
			if !set.Equal(want) {
				t.Errorf("%s: %v, want %v", access, set, want)
			}
		}
		// The effective authority of an attempt is its registered ceiling: a plain
		// step's grant does not gain the authority's wider list.
		def := runtimeauth.DefaultActions(false)
		set, _ := binding.Allowed(execution.AccessNone)
		if set.Len() <= len(def) {
			t.Fatal("the authority list is not wider than the defaults")
		}
	})
}

// Test_app_action_ceiling_statement: the statement is strict and fails closed.
func Test_app_action_ceiling_statement(t *testing.T) {
	for name, tc := range map[string]struct {
		doc  string
		env  Env
		fail bool
	}{
		"valid, test":                 {ceilingStatement, Test, false},
		"empty":                       {`{}`, Test, false},
		"an unknown kind":             {`{"kinds": {"review": {"grants": ["model.invoke"]}}}`, Test, true},
		"a default action":            {`{"kinds": {"agent": {"grants": ["secret.resolve"]}}}`, Test, true},
		"an unknown action":           {`{"kinds": {"agent": {"grants": ["admin.all"]}}}`, Test, true},
		"a duplicate":                 {`{"kinds": {"agent": {"grants": ["model.invoke", "model.invoke"]}}}`, Test, true},
		"a model action on container": {`{"kinds": {"container": {"grants": ["model.invoke"]}}}`, Test, true},
		"production cannot honour it": {ceilingStatement, Production, true},
	} {
		t.Run(name, func(t *testing.T) {
			var c ActionCeilingConfig
			err := readStrictJSON(writeTemp(t, "c.json", tc.doc), "action ceiling file", &c)
			if err == nil {
				err = c.Validate(tc.env)
			}
			if (err != nil) != tc.fail {
				t.Fatalf("%v", err)
			}
		})
	}
	var c ActionCeilingConfig
	if err := readStrictJSON(writeTemp(t, "c.json", `{"kinds": {}, "extra": 1}`), "action ceiling file", &c); err == nil {
		t.Fatal("an unknown member was accepted")
	}

	t.Run("the Kubernetes runtime refuses a ceiling it cannot register", func(t *testing.T) {
		b, err := NewCeilingBinding(ceilingConfig(t), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := newKubeRuntime(RuntimeSettings{Ceiling: b}); err == nil || !strings.Contains(err.Error(), "T-089") {
			t.Fatalf("the Kubernetes runtime accepted a ceiling: %v", err)
		}
	})
}
