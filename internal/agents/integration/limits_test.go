package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/agents/adapters"
	"github.com/optimiware/kodama/internal/agents/agentstest"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

func twoAgents(refA, refB string) string {
	return pipelinetest.DocNamed("two", "{}",
		"{id: first, kind: agent, agent: {profile: "+refA+"}, timeoutSeconds: 600, workspace: {access: read, from: run}}",
		"{id: second, kind: agent, needs: [first], agent: {profile: "+refB+"}, timeoutSeconds: 600, workspace: {access: read, from: run}}")
}

// requestGateway is an explicit test double of the AI gateway (T-032), not an
// implementation of it. It consumes the pinned policy exactly as the gateway
// will, admits requests up to the run allowance and then refuses new admission.
// Cross-replica atomic enforcement remains T-032's acceptance.
type requestGateway struct {
	policy   adapters.PolicyReader
	admitted int
}

func (g *requestGateway) admit(ctx context.Context, scope domain.Scope, run domain.RunID, step string, attempt domain.AttemptID) error {
	pol, err := g.policy.PolicyOf(ctx, scope, run, step, attempt)
	if err != nil {
		return err
	}
	if pol.Limits.MaxRequests != nil && int64(g.admitted) >= *pol.Limits.MaxRequests {
		return errors.New("request allowance exhausted")
	}
	g.admitted++
	return nil
}

// TestClaimedLimitsFailClosed verifies AR-04 / AR-A03 at the seams this module
// owns: the profile's wall time reaches the ordinary execution deadlines and
// the ordinary execution policy still applies; unsupported enforcement and
// incompatible run-wide allowances are refused; the pinned policy is readable
// by consumers only for the exact attempt; and a denied budget admission is
// never success and leaves attributable diagnostics. The model and tool
// enforcers are test doubles identified as such.
func TestClaimedLimitsFailClosed(t *testing.T) {
	scope := executiontest.ScopeA
	ctx := context.Background()

	t.Run("wall time reaches the execution deadlines, narrowed but never widened by the step", func(t *testing.T) {
		e := newEnv(t)
		p1 := e.register(scope, e.Draft) // wallSeconds 1800
		long := e.define(scope, pipelinetest.DocNamed("long", "{}",
			"{id: implement, kind: agent, agent: {profile: "+p1.Ref()+"}, timeoutSeconds: 7200, workspace: {access: read, from: run}}"))
		p := e.NewProject(scope)
		run := e.mustAdmit(p, long)
		in := e.claim(e.Svc, scope.TenantID)
		if c := e.attemptContext(in); c.Limits.TimeoutSeconds != 1800 {
			t.Fatalf("a longer step timeout must not widen the profile limit: %d", c.Limits.TimeoutSeconds)
		}
		e.start(in)
		at := e.Get(run).Attempt(in.AttemptID)
		if at.TimeoutSeconds != 1800 || at.ExecutionDeadline == nil || at.ExecutionDeadline.Sub(*at.StartedAt) != 1800*time.Second {
			t.Fatalf("the profile wall time must become the execution deadline: %d %v", at.TimeoutSeconds, at.ExecutionDeadline)
		}
		e.Clock.Advance(1801 * time.Second)
		if _, err := e.Svc.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		at = e.Get(run).Attempt(in.AttemptID)
		if at.Status != execution.AttemptFailed || at.Reason != execution.ReasonTimeout || at.Class != execution.ClassTimeout {
			t.Fatalf("the deadline must fail the attempt: %s/%s/%s", at.Status, at.Reason, at.Class)
		}
		// A late success after the deadline cannot become the outcome.
		ok := runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: ptr(int64(0)), Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: diagnostics()}
		if _, err := e.commit(in, ok, nil); !errors.Is(err, execution.ErrAttemptRevoked) {
			t.Fatalf("a late result: %v", err)
		}

		short := e.define(scope, pipelinetest.DocNamed("short", "{}",
			"{id: implement, kind: agent, agent: {profile: "+p1.Ref()+"}, timeoutSeconds: 300, workspace: {access: read, from: run}}"))
		e.mustAdmit(e.NewProject(scope), short)
		in2 := e.claim(e.Svc, scope.TenantID)
		if c := e.attemptContext(in2); c.Limits.TimeoutSeconds != 300 {
			t.Fatalf("an explicit step restriction narrows the profile limit: %d", c.Limits.TimeoutSeconds)
		}
	})

	t.Run("the ordinary execution limit still applies to the profile's wall time", func(t *testing.T) {
		e := newEnv(t)
		lim := executiontest.DefaultLimit()
		lim.MaxTimeout = 10 * time.Minute // below the profile's 30 minutes
		e.Limits.Set(lim)
		p1 := e.register(scope, e.Draft)
		def := e.define(scope, pipelinetest.DocNamed("capped", "{}",
			"{id: implement, kind: agent, agent: {profile: "+p1.Ref()+"}, timeoutSeconds: 7200, workspace: {access: read, from: run}}"))
		run := e.mustAdmit(e.NewProject(scope), def)
		if _, err := e.Svc.ClaimRunnable(ctx, scope.TenantID); !errors.Is(err, execution.ErrNothingRunnable) {
			t.Fatalf("an over-limit timeout must not dispatch: %v", err)
		}
		agg := e.Get(run)
		if agg.Step("implement").Status != execution.StepFailed || agg.Run.Status != execution.RunFailed {
			t.Fatalf("the step must fail closed: %s / %s", agg.Step("implement").Status, agg.Run.Status)
		}
		if n := e.Count(`SELECT count(*) FROM step_attempts WHERE run_id = $1`, string(run.ID)); n != 0 {
			t.Fatalf("no attempt may start: %d", n)
		}
	})

	t.Run("enforcement that is not installed is refused, not assumed", func(t *testing.T) {
		e := newEnv(t)
		p1 := e.register(scope, e.Draft) // claims requests, tokens and tool calls
		// The same repository, with a catalogue that no longer enforces them.
		entry := agentstest.Entry(e.Draft)
		entry.Controls = []agents.Control{agents.ControlModel, agents.ControlToolAllowlist, agents.ControlTranscript}
		cat, err := agents.NewCatalogue(entry)
		must(t, err)
		narrow, err := agents.NewService(agents.ServiceConfig{Repository: e.AgentRepo, Authority: nopAuthority{}, Compatibility: cat, Approvals: e.Approvals})
		must(t, err)
		resolver, err := adapters.NewResolver(narrow)
		must(t, err)
		chain, err := adapters.NewChain(adapters.ChainConfig{Resolver: resolver, Store: e.Store, Creator: e.Keyed})
		must(t, err)
		svc := e.newServiceFor(t, chain)
		def := e.define(scope, string(agentDoc("narrow", p1.Ref(), "")))
		p := e.NewProject(scope)
		_, err = svc.Admit(ctx, e.caller(), execution.AdmitCommand{Scope: scope, WorkItemID: p.Item, DefinitionID: def.ID, ContentRevision: e.ContentRevision(p), SourceCommit: executiontest.CommitA})
		if !errors.Is(err, execution.ErrAdmissionRefused) {
			t.Fatalf("a claimed control without installed enforcement must refuse admission: %v", err)
		}
		if n := e.Count(`SELECT count(*) FROM runs`); n != 0 {
			t.Fatalf("%d runs", n)
		}
	})

	t.Run("incompatible run-wide allowances are refused, equal ones admitted", func(t *testing.T) {
		e := newEnv(t)
		base := e.register(scope, e.Draft) // coding@1: 100 requests, 200000 tokens
		other := e.Draft
		other.Name = "review"
		tokens := int64(5)
		other.Limits.MaxTokens = &tokens
		different := e.register(scope, other)
		same := e.Draft
		same.Name = "review-same"
		same.Instructions += "\nSame allowances, different words."
		equal := e.register(scope, same)

		p := e.NewProject(scope)
		bad := e.define(scope, twoAgents(base.Ref(), different.Ref()))
		if _, err := e.admit(p, bad); !errors.Is(err, execution.ErrAdmissionRefused) {
			t.Fatalf("different run-wide token allowances must be refused: %v", err)
		}
		if n := e.Count(`SELECT count(*) FROM runs`); n != 0 {
			t.Fatalf("a refused admission wrote %d runs", n)
		}
		good := e.define(scope, twoAgents(base.Ref(), equal.Ref()))
		if _, err := e.admit(p, good); err != nil {
			t.Fatalf("equal allowances are compatible: %v", err)
		}
	})

	t.Run("the pinned policy is readable only for the exact attempt", func(t *testing.T) {
		e := newEnv(t)
		p1 := e.register(scope, e.Draft)
		def := e.define(scope, string(agentDoc("policy", p1.Ref(), "")))
		run := e.mustAdmit(e.NewProject(scope), def)
		in := e.claim(e.Svc, scope.TenantID)
		reader, err := adapters.NewPolicyReader(e.Attempts)
		must(t, err)

		pol, err := reader.PolicyOf(ctx, scope, run.ID, "implement", in.AttemptID)
		if err != nil {
			t.Fatal(err)
		}
		if pol.Profile.Digest != p1.Digest || pol.Model != e.Draft.Model || pol.Limits.WallSeconds != 1800 || *pol.Limits.MaxRequests != 100 ||
			len(pol.Tools) != 2 || !pol.Transcript.Record || pol.WrapperVersion != e.Draft.Wrapper.Version {
			t.Fatalf("policy: %+v", pol)
		}
		// Registering a different profile later does not change the pinned policy.
		e.register(scope, e.variant(4))
		again, err := reader.PolicyOf(ctx, scope, run.ID, "implement", in.AttemptID)
		if err != nil || again.Profile.Digest != pol.Profile.Digest {
			t.Fatalf("pinned policy changed: %v", err)
		}
		for name, tc := range map[string]struct {
			scope   domain.Scope
			run     domain.RunID
			step    string
			attempt domain.AttemptID
		}{
			"another tenant":  {executiontest.ScopeB, run.ID, "implement", in.AttemptID},
			"another project": {executiontest.ScopeA2, run.ID, "implement", in.AttemptID},
			"another run":     {scope, "run_t030_other", "implement", in.AttemptID},
			"another step":    {scope, run.ID, "other", in.AttemptID},
			"another attempt": {scope, run.ID, "implement", "att_t030_other"},
			"an invalid id":   {scope, "", "implement", in.AttemptID},
		} {
			if _, err := reader.PolicyOf(ctx, tc.scope, tc.run, tc.step, tc.attempt); !errors.Is(err, agents.ErrNotFound) {
				t.Errorf("%s must find nothing: %v", name, err)
			}
		}

		// A container attempt has no agent policy.
		cdef := e.define(scope, pipelineWithContainer(p1.Ref()))
		crun := e.mustAdmit(e.NewProject(scope), cdef)
		cin := e.claim(e.Svc, scope.TenantID)
		if _, err := reader.PolicyOf(ctx, scope, crun.ID, "build", cin.AttemptID); !errors.Is(err, agents.ErrNotFound) {
			t.Fatalf("a container attempt has no agent policy: %v", err)
		}
		meta := mustMetadata(t, e)
		if _, err := meta.MetadataOf(ctx, scope, crun.ID, "build", cin.AttemptID); !errors.Is(err, agents.ErrNotFound) {
			t.Fatalf("a container attempt has no agent metadata: %v", err)
		}
		if _, err := meta.MetadataOf(ctx, executiontest.ScopeB, run.ID, "implement", in.AttemptID); !errors.Is(err, agents.ErrNotFound) {
			t.Fatalf("foreign metadata must not be readable: %v", err)
		}
	})

	t.Run("a denied budget admission is a failure with attributable diagnostics", func(t *testing.T) {
		e := newEnv(t)
		small := e.Draft
		small.Limits.MaxRequests = ptr(int64(3))
		p1 := e.register(scope, small)
		def := e.define(scope, pipelinetest.DocNamed("budget", "{}",
			"{id: implement, kind: agent, agent: {profile: "+p1.Ref()+"}, timeoutSeconds: 600, workspace: {access: read, from: run}, outputs: {summary: {type: string}}}"))
		run := e.mustAdmit(e.NewProject(scope), def)
		in := e.claim(e.Svc, scope.TenantID)
		e.start(in)

		reader, err := adapters.NewPolicyReader(e.Attempts)
		must(t, err)
		gw := &requestGateway{policy: reader}
		denied := 0
		for i := 0; i < 5; i++ {
			if err := gw.admit(ctx, scope, run.ID, "implement", in.AttemptID); err != nil {
				denied++
			}
		}
		if gw.admitted != 3 || denied != 2 {
			t.Fatalf("the double must stop new admission at the pinned allowance: admitted %d denied %d", gw.admitted, denied)
		}
		// The wrapper reports the exhaustion honestly; prose success is not enough.
		const art = domain.ArtifactID("art_t030_budget")
		reason := "budget_exhausted"
		res := runtimev1.Result{Outcome: runtimev1.OutcomeFailed, ExitCode: ptr(int64(1)), Reason: &reason, Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: diagnostics(art)}
		ack, err := e.commit(in, res, map[domain.ArtifactID]runtimev1.ArtifactRef{art: artifactRef(scope, art, in, true)})
		if err != nil || ack.AttemptStatus != execution.AttemptFailed {
			t.Fatalf("%+v %v", ack, err)
		}
		must(t, e.Svc.ObserveAttempt(ctx, scope, in.AttemptID, execution.Observation{Kind: execution.ObservedTerminated, WorkloadName: in.WorkloadName, Launch: execution.LaunchStarted}))
		agg := e.Get(run)
		if agg.Run.Status != execution.RunFailed || agg.Step("implement").Status != execution.StepFailed || len(agg.Step("implement").Outputs) != 0 {
			t.Fatalf("exhaustion must not read as success: %s %s", agg.Run.Status, agg.Step("implement").Status)
		}
		meta, err := mustMetadata(t, e).MetadataOf(ctx, scope, run.ID, "implement", in.AttemptID)
		if err != nil {
			t.Fatal(err)
		}
		if meta.Profile.Digest != p1.Digest || meta.Outcome != runtimev1.OutcomeFailed || len(meta.DiagnosticArtifactIDs) != 1 || meta.DiagnosticArtifactIDs[0] != art {
			t.Fatalf("the failure must stay attributable to the pinned profile: %+v", meta)
		}
	})
}

// nopAuthority is never consulted: the narrowed service is used only to resolve.
type nopAuthority struct{}

func (nopAuthority) ResolveActor(context.Context, domain.Scope, string) (agents.Actor, error) {
	return agents.Actor{}, agents.ErrHidden
}

func (nopAuthority) Authorize(context.Context, agents.Actor, domain.Scope, agents.Permission) error {
	return agents.ErrDenied
}
