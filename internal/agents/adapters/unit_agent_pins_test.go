package adapters_test

// Agent pins in shared (v2) and source-set (v3) contexts (T-163). A run admitted
// through the real agent admission chain freezes the expanded wrapper payload into
// its step; the execution module builds the attempt's context with the pins the
// chain's Pinner derives from that payload, so the policy and metadata readers
// and the wrapper's context check accept it. Nothing here is a provider, gateway
// or live-harness claim.

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/agents/adapters"
	"github.com/optimiware/kodama/internal/agents/agentstest"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

// pinWorld is one agent profile registry in the execution fixtures' scope, the
// real admission chain over it, and a validator that knows its profiles.
type pinWorld struct {
	svc     *agents.Service
	profile agents.ProfileVersion
	resolve adapters.Resolver
	chain   adapters.Chain
	valid   *pipeline.Validator
}

func newPinWorld(t *testing.T) *pinWorld {
	t.Helper()
	sc := executiontest.SharedScope
	draft := agentstest.Draft(t, "coding")
	auth := agentstest.NewAuthority()
	auth.Grant(sc, engineerID, agents.ActorUser, agents.PermRegister, agents.PermList)
	svc, err := agents.NewService(agents.ServiceConfig{
		Repository: &agentstest.MemRepo{}, Authority: auth, Compatibility: agentstest.Catalogue(t, draft), Approvals: agentstest.NewApprovals(),
		Clock: func() time.Time { return time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.Register(context.Background(), agents.RegisterCommand{Scope: sc, Caller: agents.Caller{PrincipalID: engineerID}, Draft: draft})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := adapters.NewResolver(svc)
	if err != nil {
		t.Fatal(err)
	}
	chain, err := adapters.NewChain(adapters.ChainConfig{Resolver: resolver, Store: &spyStore{}, Creator: &spyCreator{}})
	if err != nil {
		t.Fatal(err)
	}
	lookup, err := adapters.NewLookup(svc)
	if err != nil {
		t.Fatal(err)
	}
	v, err := pipeline.NewValidator(pipeline.Config{
		Capabilities: chain.Capabilities(pipeline.KindGate), Profiles: lookup, References: pipelinetest.References{},
		EnableV1Alpha2: true, EnableV1Alpha3: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &pinWorld{svc: svc, profile: res.Profile, resolve: resolver, chain: chain, valid: v}
}

// wrap decorates the memory store the way the chain is wired in a composition.
func (w *pinWorld) wrap(t *testing.T) func(execution.Store) execution.Store {
	return func(s execution.Store) execution.Store {
		c, err := adapters.NewChain(adapters.ChainConfig{Resolver: w.resolve, Store: s, Creator: &spyCreator{}})
		if err != nil {
			t.Fatal(err)
		}
		return c.Store
	}
}

// agentStepYAML is an agent step of the world's profile.
func (w *pinWorld) agentStepYAML(id string, extra ...string) string {
	parts := append([]string{"id: " + id, "kind: agent", "agent: {profile: " + w.profile.Ref() + "}", "timeoutSeconds: 600"}, extra...)
	return "{" + strings.Join(parts, ", ") + "}"
}

// wantPins are the pins the world's profile payload implies.
func wantPins(t *testing.T, cmd runtimev1.Command) (agents.WrapperConfig, runtimev1.Profile, runtimev1.DesignBaseline) {
	t.Helper()
	cfg, present, err := agents.DecodeArgs(cmd.Args)
	if err != nil || !present {
		t.Fatalf("the frozen command carries no payload: %v %v", present, err)
	}
	if err := cfg.Verify(cmd.Image, cmd.Command); err != nil {
		t.Fatal(err)
	}
	p, b := cfg.Pins()
	return cfg, p, b
}

// wrapperAccepts mirrors the wrapper's pinned-context check (wrapper/run.go): the
// attempt is the one it was started for, and the context carries the profile and
// the design baseline digest the payload names. It cannot import the wrapper.
func wrapperAccepts(attempt domain.AttemptID, got *runtimev1.Profile, gotBase *runtimev1.DesignBaseline, id domain.AttemptID, profile runtimev1.Profile, base runtimev1.DesignBaseline) bool {
	return attempt == id && gotBase != nil && gotBase.Digest == base.Digest && got != nil && *got == profile
}

func Test_unit_v3_agent_attempt_policy_end_to_end(t *testing.T) {
	ctx := context.Background()
	sc := executiontest.SharedScope
	w := newPinWorld(t)

	m, s := executiontest.NewSourceMem(t, executiontest.SourceOptions{Hooks: w.chain.Hooks, WrapStore: w.wrap(t), AgentPins: w.chain.Pins})
	m.Validator = w.valid
	run := executiontest.NewSourceRunSteps(t, s, w.agentStepYAML("generate", "workspace: {access: write}"))
	in := run.Claim()

	c, err := in.DecodeContextV3()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("the context violates the runtime v3 contract: %v", err)
	}
	cfg, profile, baseline := wantPins(t, c.Command)
	if c.Profile == nil || c.DesignBaseline == nil {
		t.Fatal("the attempt context lacks the agent pins")
	}
	if !wrapperAccepts(in.AttemptID, c.Profile, c.DesignBaseline, c.AttemptID, profile, baseline) || !reflect.DeepEqual(*c.DesignBaseline, baseline) {
		t.Fatalf("the wrapper's context check refuses: %+v %+v", c.Profile, c.DesignBaseline)
	}

	attempts, err := adapters.NewAttemptSource(m.Mem)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := adapters.NewPolicyReader(attempts)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := adapters.NewMetadataReader(attempts)
	if err != nil {
		t.Fatal(err)
	}
	got, err := policy.PolicyOf(ctx, sc, run.Run.ID, "generate", in.AttemptID)
	if err != nil {
		t.Fatalf("the policy of a source-set agent attempt: %v", err)
	}
	if !reflect.DeepEqual(got, cfg.Policy()) {
		t.Fatalf("policy:\n%+v\n%+v", got, cfg.Policy())
	}
	md, err := meta.MetadataOf(ctx, sc, run.Run.ID, "generate", in.AttemptID)
	if err != nil {
		t.Fatalf("the metadata of a source-set agent attempt: %v", err)
	}
	if md.ContextVersion != 3 || md.Profile != cfg.Profile || md.DesignBaselineDigest != baseline.Digest ||
		md.SourceSetDigest != *c.Workspace.SourceSetDigest || len(md.Checkouts) == 0 {
		t.Fatalf("attribution: %+v", md)
	}

	t.Run("the recorded attempt carries exactly the claimed context and digest", func(t *testing.T) {
		at, err := attempts.Attempt(ctx, sc, run.Run.ID, "generate", in.AttemptID)
		if err != nil {
			t.Fatal(err)
		}
		if string(at.Context) != string(in.Context) || at.ContextDigest != in.ContextDigest {
			t.Fatal("the stored attempt context differs from the dispatched one")
		}
	})

	t.Run("a redrive and a restarted controller see the same frozen bytes", func(t *testing.T) {
		restarted := s.RestartSource()
		spec, err := restarted.ClaimRunnable(ctx, sc.TenantID)
		if err == nil && spec != nil && spec.AttemptID != in.AttemptID {
			t.Fatalf("a second attempt was planned for the one-attempt run: %+v", spec)
		}
		at, err := attempts.Attempt(ctx, sc, run.Run.ID, "generate", in.AttemptID)
		if err != nil || string(at.Context) != string(in.Context) {
			t.Fatalf("the frozen context changed: %v", err)
		}
		again, err := policy.PolicyOf(ctx, sc, run.Run.ID, "generate", in.AttemptID)
		if err != nil || !reflect.DeepEqual(again, got) {
			t.Fatalf("%+v %v", again, err)
		}
	})

	t.Run("the pins are the payload's: a context without them still fails closed", func(t *testing.T) {
		at, err := attempts.Attempt(ctx, sc, run.Run.ID, "generate", in.AttemptID)
		if err != nil {
			t.Fatal(err)
		}
		stripped := c
		stripped.Profile, stripped.DesignBaseline = nil, nil
		raw, err := json.Marshal(stripped)
		if err != nil {
			t.Fatal(err)
		}
		at.Context = raw
		r, err := adapters.NewPolicyReader(oneAttempt(at))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.PolicyOf(ctx, sc, at.RunID, at.StepID, at.ID); !errors.Is(err, agents.ErrUnavailable) {
			t.Fatalf("a pinless context is unavailable: %v", err)
		}
	})
}

func Test_unit_v2_agent_attempt_policy_end_to_end(t *testing.T) {
	ctx := context.Background()
	sc := executiontest.SharedScope
	w := newPinWorld(t)

	m, s := executiontest.NewSharedMem(t, func(mo *executiontest.MemOptions) {
		mo.Hooks, mo.WrapStore, mo.AgentPins = w.chain.Hooks, w.wrap(t), w.chain.Pins
	})
	m.Validator = w.valid
	r := executiontest.NewSharedRun(t, s, executiontest.SharedSpecDoc(false, w.agentStepYAML("generate", "workspace: {access: read}")))
	r.Up()
	in, err := r.Claim()
	if err != nil {
		t.Fatal(err)
	}
	c, err := runtimev2.DecodeRequest[runtimev2.ContextV2](in.Context)
	if err != nil {
		t.Fatal(err)
	}
	cfg, profile, baseline := wantPins(t, c.Command)
	if !wrapperAccepts(in.AttemptID, c.Profile, c.DesignBaseline, c.AttemptID, profile, baseline) {
		t.Fatalf("the wrapper's context check refuses: %+v %+v", c.Profile, c.DesignBaseline)
	}
	attempts, err := adapters.NewAttemptSource(m.Mem)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := adapters.NewPolicyReader(attempts)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := adapters.NewMetadataReader(attempts)
	if err != nil {
		t.Fatal(err)
	}
	got, err := policy.PolicyOf(ctx, sc, r.Run().ID, "generate", in.AttemptID)
	if err != nil || !reflect.DeepEqual(got, cfg.Policy()) {
		t.Fatalf("%+v %v", got, err)
	}
	if md, err := meta.MetadataOf(ctx, sc, r.Run().ID, "generate", in.AttemptID); err != nil || md.ContextVersion != 2 || md.Profile != cfg.Profile {
		t.Fatalf("%+v %v", md, err)
	}
}

// Without the pin port a shared run of an agent step is refused at dispatch with
// a classified failure instead of running a wrapper that no policy would accept;
// the step fails and the run does not start the agent.
func Test_unit_v3_agent_step_without_pin_port_fails_closed(t *testing.T) {
	w := newPinWorld(t)
	m, s := executiontest.NewSourceMem(t, executiontest.SourceOptions{Hooks: w.chain.Hooks, WrapStore: w.wrap(t)})
	m.Validator = w.valid
	run := executiontest.NewSourceRunSteps(t, s, w.agentStepYAML("generate", "workspace: {access: write}"))
	if in, err := run.TryClaim(); in != nil {
		t.Fatalf("an agent step without pins must not be dispatched: %+v %v", in, err)
	}
	agg := run.Get()
	if len(agg.Attempts) != 0 {
		t.Fatalf("an attempt was created: %d", len(agg.Attempts))
	}
	for _, st := range agg.Steps {
		if st.ID == "generate" && st.Status != execution.StepFailed {
			t.Fatalf("the step is %s", st.Status)
		}
	}
}

func Test_unit_pinner_is_pure_and_shares_the_planner_checks(t *testing.T) {
	w := newPinWorld(t)
	exp, err := w.resolve.ResolveAgent(context.Background(), executiontest.SharedScope, pipeline.Step{Kind: pipeline.KindAgent, Agent: &pipeline.AgentSpec{Profile: w.profile.Ref()}})
	if err != nil {
		t.Fatal(err)
	}
	cmd := runtimev1.Command{Image: exp.Image, Command: exp.Command, Args: exp.Args, Env: map[string]string{}}
	pin := adapters.Pinner{}
	a, ok, err := pin.PinAgent(executiontest.SharedScope.TenantID, cmd)
	if err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	b, _, _ := pin.PinAgent("t_other", cmd)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("the pins depend on more than the frozen command")
	}
	if _, ok, err := pin.PinAgent(executiontest.SharedScope.TenantID, runtimev1.Command{Image: exp.Image, Command: []string{"/bin/true"}, Args: []string{}}); ok || err != nil {
		t.Fatalf("an ordinary command has no pin: %v %v", ok, err)
	}
	bad := cmd
	bad.Image = "registry.example.invalid/other@sha256:" + strings.Repeat("9", 64)
	if _, _, err := pin.PinAgent(executiontest.SharedScope.TenantID, bad); !errors.Is(err, execution.ErrInvalid) {
		t.Fatalf("a payload that does not match its command: %v", err)
	}
	bad = cmd
	bad.Args = append([]string{}, cmd.Args...)
	bad.Args[len(bad.Args)-1] = "!"
	if _, _, err := pin.PinAgent(executiontest.SharedScope.TenantID, bad); !errors.Is(err, execution.ErrInvalid) {
		t.Fatalf("a corrupt payload: %v", err)
	}
	// The v1 planner pins what the pinner pins.
	c := contextOf(cmd.Image, cmd.Command, cmd.Args)
	wp, err := w.chain.Planner.Plan(executiontest.SharedScope.TenantID, c)
	if err != nil {
		t.Fatal(err)
	}
	v1, err := runtimev1.DecodeRequest[runtimev1.Context](wp.Context)
	if err != nil || v1.Profile == nil || *v1.Profile != a.Profile || v1.DesignBaseline.Digest != a.DesignBaseline.Digest {
		t.Fatalf("%+v %v", v1, err)
	}
}
