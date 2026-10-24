package runtimeauth_test

import (
	"context"
	"errors"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

type profiles struct {
	caps  map[string]runtimeauth.ProfileCapabilities
	err   error
	calls int
}

func (p *profiles) ProfileCapabilities(_ context.Context, _ domain.Scope, pin runtimev1.Profile) (runtimeauth.ProfileCapabilities, error) {
	p.calls++
	if p.err != nil {
		return runtimeauth.ProfileCapabilities{}, p.err
	}
	c, ok := p.caps[pin.Name]
	if !ok {
		return runtimeauth.ProfileCapabilities{}, errors.New("no such profile")
	}
	return c, nil
}

type failingSource struct{}

func (failingSource) Ceiling(context.Context) (runtimeauth.CeilingConfig, error) {
	return runtimeauth.CeilingConfig{}, errors.New("policy store unreachable")
}

type sourceFunc func() (runtimeauth.CeilingConfig, error)

func (f sourceFunc) Ceiling(context.Context) (runtimeauth.CeilingConfig, error) { return f() }

var scope = domain.Scope{TenantID: "t096_acme", ProjectID: "p096_shop"}

func fullConfig() runtimeauth.CeilingConfig {
	return runtimeauth.CeilingConfig{Kinds: map[runtimeauth.StepKind]runtimeauth.KindCeiling{
		runtimeauth.StepAgent:     {Grants: []runtimeauth.Action{runtimeauth.ActionModelInvoke, runtimeauth.ActionToolInvoke}},
		runtimeauth.StepContainer: {Grants: []runtimeauth.Action{runtimeauth.ActionDeploymentCall}},
	}}
}

func fullProfiles() *profiles {
	return &profiles{caps: map[string]runtimeauth.ProfileCapabilities{
		"both":      {Model: true, Tools: true},
		"modelonly": {Model: true},
		"toolsonly": {Tools: true},
		"neither":   {},
	}}
}

func policyOf(t *testing.T, src runtimeauth.CeilingSource, p runtimeauth.ProfileResolver) runtimeauth.CeilingPolicy {
	t.Helper()
	pol, err := runtimeauth.NewCeilingPolicy(src, p)
	if err != nil {
		t.Fatal(err)
	}
	return pol
}

func pin(name string) *runtimev1.Profile {
	return &runtimev1.Profile{Name: name, Version: 1, WrapperVersion: "1.0.0"}
}

func defaults(t *testing.T, writer bool) runtimeauth.ActionSet {
	t.Helper()
	return actions(t, runtimeauth.DefaultActions(writer)...)
}

func plus(t *testing.T, writer bool, extra ...runtimeauth.Action) runtimeauth.ActionSet {
	t.Helper()
	return actions(t, append(runtimeauth.DefaultActions(writer), extra...)...)
}

// Test_runtime_ceiling_agent_actions: an agent attempt whose pinned profile
// names a model and a tool holds exactly model.invoke and tool.invoke beyond
// the defaults; each one only when its own fact holds.
func Test_runtime_ceiling_agent_actions(t *testing.T) {
	pol := policyOf(t, runtimeauth.StaticCeiling(fullConfig()), fullProfiles())
	for name, tc := range map[string]struct {
		profile string
		writer  bool
		want    runtimeauth.ActionSet
	}{
		"model and tool":     {"both", false, plus(t, false, runtimeauth.ActionModelInvoke, runtimeauth.ActionToolInvoke)},
		"model and tool, rw": {"both", true, plus(t, true, runtimeauth.ActionModelInvoke, runtimeauth.ActionToolInvoke)},
		"model only":         {"modelonly", false, plus(t, false, runtimeauth.ActionModelInvoke)},
		"tools only":         {"toolsonly", false, plus(t, false, runtimeauth.ActionToolInvoke)},
		"neither":            {"neither", false, defaults(t, false)},
	} {
		t.Run(name, func(t *testing.T) {
			got := pol.Actions(context.Background(), runtimeauth.CeilingInput{Scope: scope, Kind: runtimeauth.StepAgent, Writer: tc.writer, Profile: pin(tc.profile)})
			if !got.Equal(tc.want) {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
			if got.Contains(runtimeauth.ActionDeploymentCall) {
				t.Fatal("an agent without a deployment holds deployment.call")
			}
		})
	}

	t.Run("the ceiling configured is the bound", func(t *testing.T) {
		cfg := runtimeauth.CeilingConfig{Kinds: map[runtimeauth.StepKind]runtimeauth.KindCeiling{
			runtimeauth.StepAgent: {Grants: []runtimeauth.Action{runtimeauth.ActionModelInvoke}},
		}}
		got := policyOf(t, runtimeauth.StaticCeiling(cfg), fullProfiles()).Actions(context.Background(),
			runtimeauth.CeilingInput{Scope: scope, Kind: runtimeauth.StepAgent, Profile: pin("both")})
		if !got.Equal(plus(t, false, runtimeauth.ActionModelInvoke)) {
			t.Fatalf("an unconfigured action was granted: %s", got)
		}
	})
}

// Test_runtime_ceiling_deployment_actions: a deployment-declaring step holds
// deployment.call and no other step does.
func Test_runtime_ceiling_deployment_actions(t *testing.T) {
	pol := policyOf(t, runtimeauth.StaticCeiling(fullConfig()), fullProfiles())
	ctx := context.Background()
	got := pol.Actions(ctx, runtimeauth.CeilingInput{Scope: scope, Kind: runtimeauth.StepContainer, Deployment: true})
	if !got.Equal(plus(t, false, runtimeauth.ActionDeploymentCall)) {
		t.Fatalf("deployment step: %s", got)
	}
	got = pol.Actions(ctx, runtimeauth.CeilingInput{Scope: scope, Kind: runtimeauth.StepContainer, Writer: true, Deployment: true})
	if !got.Equal(plus(t, true, runtimeauth.ActionDeploymentCall)) {
		t.Fatalf("writing deployment step: %s", got)
	}
	for name, in := range map[string]runtimeauth.CeilingInput{
		"container without deployment": {Scope: scope, Kind: runtimeauth.StepContainer},
		"agent without deployment":     {Scope: scope, Kind: runtimeauth.StepAgent, Profile: pin("both")},
		"agent declaring deployment":   {Scope: scope, Kind: runtimeauth.StepAgent, Profile: pin("both"), Deployment: true},
		"unknown kind":                 {Scope: scope, Kind: "gate", Deployment: true},
	} {
		if got := pol.Actions(ctx, in); got.Contains(runtimeauth.ActionDeploymentCall) {
			t.Errorf("%s holds deployment.call: %s", name, got)
		}
	}

	// An operator who grants deployment.call to agents grants it to a
	// deployment-declaring agent that carries a profile pin, and without a
	// profile lookup.
	cfg := runtimeauth.CeilingConfig{Kinds: map[runtimeauth.StepKind]runtimeauth.KindCeiling{
		runtimeauth.StepAgent: {Grants: []runtimeauth.Action{runtimeauth.ActionDeploymentCall}},
	}}
	pr := fullProfiles()
	agent := policyOf(t, runtimeauth.StaticCeiling(cfg), pr)
	if got := agent.Actions(ctx, runtimeauth.CeilingInput{Scope: scope, Kind: runtimeauth.StepAgent, Profile: pin("both"), Deployment: true}); !got.Equal(plus(t, false, runtimeauth.ActionDeploymentCall)) {
		t.Fatalf("agent deployment: %s", got)
	}
	if got := agent.Actions(ctx, runtimeauth.CeilingInput{Scope: scope, Kind: runtimeauth.StepAgent, Deployment: true}); !got.Equal(defaults(t, false)) {
		t.Fatalf("agent deployment without a pin: %s", got)
	}
	if got := agent.Actions(ctx, runtimeauth.CeilingInput{Scope: scope, Kind: runtimeauth.StepAgent, Profile: pin("both")}); !got.Equal(defaults(t, false)) || pr.calls != 0 {
		t.Fatalf("agent without deployment: %s (%d lookups)", got, pr.calls)
	}
}

// fakeTx serves CheckSession from one consumed grant and one attempt state.
type fakeTx struct {
	runtimeauth.GrantTx
	grant runtimeauth.AdmissionGrant
	state runtimeauth.AttemptState
}

func (f fakeTx) LockAttempt(context.Context, domain.Scope, domain.AttemptID) (runtimeauth.AttemptState, error) {
	return f.state, nil
}

func (f fakeTx) GrantBySession(context.Context, runtimeauth.SessionID, runtimeauth.LockMode) (runtimeauth.AdmissionGrant, error) {
	return f.grant, nil
}

// Test_runtime_ceiling_default_closed: an unknown kind, a missing pin, a failing
// source or profile lookup and an invalid configuration yield only
// DefaultActions, and a per-attempt revocation still removes a granted action.
func Test_runtime_ceiling_default_closed(t *testing.T) {
	ctx := context.Background()
	agent := runtimeauth.CeilingInput{Scope: scope, Kind: runtimeauth.StepAgent, Profile: pin("both"), Deployment: true}
	want := defaults(t, false)

	invalid := runtimeauth.CeilingConfig{Kinds: map[runtimeauth.StepKind]runtimeauth.KindCeiling{
		runtimeauth.StepAgent: {Grants: []runtimeauth.Action{runtimeauth.ActionModelInvoke, runtimeauth.ActionSecretResolve}},
	}}
	for name, tc := range map[string]struct {
		pol runtimeauth.CeilingPolicy
		in  runtimeauth.CeilingInput
	}{
		"failing source":          {policyOf(t, failingSource{}, fullProfiles()), agent},
		"invalid configuration":   {policyOf(t, sourceFunc(func() (runtimeauth.CeilingConfig, error) { return invalid, nil }), fullProfiles()), agent},
		"unknown kind":            {policyOf(t, runtimeauth.StaticCeiling(fullConfig()), fullProfiles()), withKind(agent, "paused")},
		"empty kind":              {policyOf(t, runtimeauth.StaticCeiling(fullConfig()), fullProfiles()), withKind(agent, "")},
		"missing profile pin":     {policyOf(t, runtimeauth.StaticCeiling(fullConfig()), fullProfiles()), withProfile(agent, nil)},
		"unknown profile version": {policyOf(t, runtimeauth.StaticCeiling(fullConfig()), fullProfiles()), withProfile(agent, pin("ghost"))},
		"failing profile source":  {policyOf(t, runtimeauth.StaticCeiling(fullConfig()), &profiles{err: errors.New("down")}), agent},
		"no profile resolver":     {policyOf(t, runtimeauth.StaticCeiling(fullConfig()), nil), agent},
		"deployment-only ceiling, no pin": {policyOf(t, runtimeauth.StaticCeiling(runtimeauth.CeilingConfig{Kinds: map[runtimeauth.StepKind]runtimeauth.KindCeiling{
			runtimeauth.StepAgent: {Grants: []runtimeauth.Action{runtimeauth.ActionDeploymentCall}},
		}}), fullProfiles()), withProfile(agent, nil)},
		"no kinds configured": {policyOf(t, runtimeauth.StaticCeiling(runtimeauth.CeilingConfig{}), fullProfiles()), agent},
		"zero policy":         {runtimeauth.CeilingPolicy{}, agent},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.pol.Actions(ctx, tc.in); !got.Equal(want) {
				t.Fatalf("got %s, want only %s", got, want)
			}
		})
	}

	t.Run("a source that recovers applies to the next decision", func(t *testing.T) {
		fail := true
		pol := policyOf(t, sourceFunc(func() (runtimeauth.CeilingConfig, error) {
			if fail {
				return runtimeauth.CeilingConfig{}, errors.New("down")
			}
			return fullConfig(), nil
		}), fullProfiles())
		if got := pol.Actions(ctx, agent); !got.Equal(want) {
			t.Fatalf("during the outage: %s", got)
		}
		fail = false
		if got := pol.Actions(ctx, agent); !got.Contains(runtimeauth.ActionModelInvoke) {
			t.Fatalf("after recovery: %s", got)
		}
	})

	t.Run("invalid configuration is rejected at construction", func(t *testing.T) {
		for name, cfg := range map[string]runtimeauth.CeilingConfig{
			"unknown kind":          {Kinds: map[runtimeauth.StepKind]runtimeauth.KindCeiling{"gate": {Grants: []runtimeauth.Action{runtimeauth.ActionDeploymentCall}}}},
			"default action":        {Kinds: map[runtimeauth.StepKind]runtimeauth.KindCeiling{runtimeauth.StepAgent: {Grants: []runtimeauth.Action{runtimeauth.ActionSourcePropose}}}},
			"unknown action":        {Kinds: map[runtimeauth.StepKind]runtimeauth.KindCeiling{runtimeauth.StepAgent: {Grants: []runtimeauth.Action{"*"}}}},
			"duplicate":             {Kinds: map[runtimeauth.StepKind]runtimeauth.KindCeiling{runtimeauth.StepAgent: {Grants: []runtimeauth.Action{runtimeauth.ActionModelInvoke, runtimeauth.ActionModelInvoke}}}},
			"model for a container": {Kinds: map[runtimeauth.StepKind]runtimeauth.KindCeiling{runtimeauth.StepContainer: {Grants: []runtimeauth.Action{runtimeauth.ActionModelInvoke}}}},
		} {
			if _, err := runtimeauth.NewCeilingPolicy(runtimeauth.StaticCeiling(cfg), nil); err == nil {
				t.Errorf("%s accepted", name)
			}
		}
		if _, err := runtimeauth.NewCeilingPolicy(nil, nil); err == nil {
			t.Error("a nil source was accepted")
		}
	})

	t.Run("per-attempt revocation removes a granted action", func(t *testing.T) {
		pol := policyOf(t, runtimeauth.StaticCeiling(fullConfig()), fullProfiles())
		ceiling := pol.Actions(ctx, agent)
		if !ceiling.Contains(runtimeauth.ActionModelInvoke) || !ceiling.Contains(runtimeauth.ActionToolInvoke) {
			t.Fatalf("setup: %s", ceiling)
		}
		prov := provision(t)
		prov.Actions = ceiling
		g, err := runtimeauth.NewAdmissionGrant(prov, t0, policy)
		if err != nil {
			t.Fatal(err)
		}
		ses := session(t)
		if _, err := g.Consume(runtimeauth.NonceDigest("nnnnnnnnnnnnnnnnnnnnnn"), podA, ses, t0, policy); err != nil {
			t.Fatal(err)
		}
		state := func(a runtimeauth.ActionSet) runtimeauth.AttemptState {
			return runtimeauth.AttemptState{
				Scope: g.Scope, RunID: g.RunID, AttemptID: g.AttemptID, Phase: runtimeauth.AttemptActive, Revision: g.AuthorityRevision,
				Job: g.Job, ContextDigest: g.ContextDigest, Actions: a,
			}
		}
		check := func(tx fakeTx, a runtimeauth.Action) error {
			_, _, err := runtimeauth.CheckSession(ctx, tx, runtimeauth.SessionCheck{
				Route: g.AttemptID, Session: ses, TokenGeneration: g.Generation, CurrentGeneration: g.Generation, Action: a, Now: t0.Add(time.Minute),
			})
			return err
		}
		if err := check(fakeTx{grant: g, state: state(ceiling)}, runtimeauth.ActionModelInvoke); err != nil {
			t.Fatalf("granted action: %v", err)
		}
		narrowed := plus(t, false, runtimeauth.ActionToolInvoke) // the attempt revoked model.invoke
		tx := fakeTx{grant: g, state: state(narrowed)}
		mustIs(t, check(tx, runtimeauth.ActionModelInvoke), runtimeauth.ErrForbidden)
		if err := check(tx, runtimeauth.ActionToolInvoke); err != nil {
			t.Fatalf("an action the attempt still allows: %v", err)
		}
		// The attempt allowing more than the ceiling never widens the grant.
		wide := plus(t, false, runtimeauth.ActionToolInvoke, runtimeauth.ActionModelInvoke, runtimeauth.ActionDeploymentCall)
		mustIs(t, check(fakeTx{grant: g, state: state(wide)}, runtimeauth.ActionDeploymentCall), runtimeauth.ErrForbidden)
	})
}

func withKind(in runtimeauth.CeilingInput, k runtimeauth.StepKind) runtimeauth.CeilingInput {
	in.Kind = k
	return in
}

func withProfile(in runtimeauth.CeilingInput, p *runtimev1.Profile) runtimeauth.CeilingInput {
	in.Profile = p
	return in
}
