package integration_test

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
)

// TestProfilePinning verifies AR-01 / AR-03 / AR-A08's pinned-context rule: both
// admission paths freeze the profile's image, wrapper, instructions, model,
// tools, limits and design baseline into the run before it becomes durable, and
// neither registering another version, restarting a controller nor retrying an
// attempt can change what the run executes. The run keeps the original pipeline
// definition and the ordinary source/publication identities. Real PostgreSQL.
func TestProfilePinning(t *testing.T) {
	e := newEnv(t)
	scope := executiontest.ScopeA
	p1 := e.register(scope, e.Draft)
	p := e.NewProject(scope)
	def := e.define(scope, string(agentDoc("pinned", p1.Ref(), ", retry: {maxAttempts: 2, backoffSeconds: 30, on: [infrastructure]}")))

	// The registered definition is the author's document: no image, no payload.
	stored := def.Content.Canonical()
	for _, needle := range []string{agentsImageFragment, "kodama.agent-config"} {
		if contains(stored, needle) {
			t.Fatalf("the pipeline definition must not be rewritten with %q", needle)
		}
	}

	ordinary := e.mustAdmit(p, def)

	p2 := e.NewProject(scope)
	rec := e.httpCreate(p2, def, "key-pin-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("keyed create: %d %s", rec.Code, rec.Body.String())
	}
	keyed := e.runRef(scope, runID(t, rec))

	t.Run("both admission paths freeze identical steps and keep the original definition", func(t *testing.T) {
		a, b := e.Get(ordinary), e.Get(keyed)
		for name, agg := range map[string]*execution.Aggregate{"ordinary": a, "keyed": b} {
			if agg.Run.DefinitionID != def.ID || agg.Run.DefinitionDigest != string(def.Digest) {
				t.Fatalf("%s: the run must pin the original definition ID and digest: %s %s", name, agg.Run.DefinitionID, agg.Run.DefinitionDigest)
			}
			if agg.Run.SourceCommit != executiontest.CommitA {
				t.Fatalf("%s: source identity changed: %s", name, agg.Run.SourceCommit)
			}
		}
		sa, sb := a.Step("implement"), b.Step("implement")
		if !reflect.DeepEqual(sa.Spec, sb.Spec) {
			t.Fatalf("the two admission paths froze different steps:\n%+v\n%+v", sa.Spec, sb.Spec)
		}
		spec := sa.Spec
		if spec.Kind != "agent" || spec.Agent == nil || spec.Agent.Profile != p1.Ref() {
			t.Fatalf("agent attribution must be retained: %+v", spec.Agent)
		}
		if spec.Image != e.Draft.Image || !reflect.DeepEqual(spec.Command, e.Draft.Wrapper.Command) || spec.TimeoutSeconds != 600 {
			t.Fatalf("frozen command: image %q command %v timeout %d", spec.Image, spec.Command, spec.TimeoutSeconds)
		}
		cfg, present, err := agents.DecodeArgs(spec.Args)
		if err != nil || !present {
			t.Fatalf("the frozen payload: %v %v", present, err)
		}
		if cfg.Profile.Digest != p1.Digest || cfg.Profile.Version != 1 || cfg.Content.Instructions != e.Draft.Instructions ||
			cfg.Content.Model != e.Draft.Model || !reflect.DeepEqual(cfg.Content.Tools, []string{"repo.read", "repo.search"}) ||
			cfg.Content.Limits.WallSeconds != 1800 || cfg.Content.DesignBaseline.Digest != e.Draft.DesignBaseline.Digest {
			t.Fatalf("the payload must pin the complete profile: %+v", cfg.Profile)
		}
		// The frozen step is what the database holds, immutably.
		var raw string
		e.Scalar(&raw, `SELECT spec::text FROM step_runs WHERE tenant_id = $1 AND run_id = $2 AND step_id = 'implement'`, string(scope.TenantID), string(ordinary.ID))
		if !contains([]byte(raw), "kodama.agent-config") {
			t.Fatal("the stored step spec must carry the payload")
		}
		if err := e.OwnerExecErr(`UPDATE step_runs SET spec = '{}'::jsonb WHERE run_id = $1`, string(ordinary.ID)); err == nil {
			t.Fatal("a run's frozen steps must be immutable in the database")
		}
	})

	// A newer version appears after both runs were admitted.
	newer := e.Draft
	newer.Instructions = "Completely different instructions."
	newer.Model.Name = "t030-model"
	newer.Limits.WallSeconds = 60
	p2v := e.register(scope, newer)
	if p2v.Version != 2 {
		t.Fatalf("the second version: %+v", p2v)
	}

	t.Run("registering another version, restarting and retrying change nothing", func(t *testing.T) {
		for _, c := range []struct {
			name string
			svc  *execution.Service
			run  *execution.Run
			proj executiontest.Project
		}{
			{"ordinary run, original controller", e.Svc, ordinary, p},
			{"keyed run, restarted controller", e.newService(t), keyed, p2},
		} {
			in := e.claim(c.svc, c.proj.Scope.TenantID)
			if in.RunID != c.run.ID {
				t.Fatalf("%s: claimed run %s", c.name, in.RunID)
			}
			ctx := e.attemptContext(in)
			if ctx.Profile == nil || ctx.Profile.Name != "coding" || ctx.Profile.Version != 1 || ctx.Profile.WrapperVersion != e.Draft.Wrapper.Version {
				t.Fatalf("%s: profile pin %+v", c.name, ctx.Profile)
			}
			if ctx.DesignBaseline == nil || ctx.DesignBaseline.Digest != e.Draft.DesignBaseline.Digest {
				t.Fatalf("%s: baseline pin %+v", c.name, ctx.DesignBaseline)
			}
			if ctx.Command.Image != e.Draft.Image || ctx.Limits.TimeoutSeconds != 600 {
				t.Fatalf("%s: command %q timeout %d", c.name, ctx.Command.Image, ctx.Limits.TimeoutSeconds)
			}
			cfg := e.payload(ctx)
			if cfg.Content.Instructions != e.Draft.Instructions || cfg.Profile.Digest != p1.Digest || cfg.Content.Limits.WallSeconds != 1800 {
				t.Fatalf("%s: the attempt saw a newer profile: %+v", c.name, cfg.Profile)
			}
			if ctx.Workspace.Access != "read" || ctx.Workspace.Source == nil || string(ctx.Workspace.Source.Commit) != executiontest.CommitA {
				t.Fatalf("%s: source identity %+v", c.name, ctx.Workspace)
			}
			if ctx.Definition.ID != def.ID || string(ctx.Definition.Digest) != string(def.Digest) {
				t.Fatalf("%s: definition identity %+v", c.name, ctx.Definition)
			}
		}
	})

	t.Run("a retried attempt pins exactly what the first attempt did", func(t *testing.T) {
		pr := e.NewProject(scope)
		run := e.mustAdmit(pr, def)
		first := e.claim(e.Svc, scope.TenantID)
		if first.RunID != run.ID {
			t.Fatalf("claimed %s", first.RunID)
		}
		firstCtx := e.attemptContext(first)
		ctx := context.Background()
		must(t, e.Svc.ObserveAttempt(ctx, first.Scope, first.AttemptID, execution.Observation{
			Kind: execution.ObservedFailed, WorkloadName: first.WorkloadName, Class: execution.ClassInfrastructure, Reason: execution.ReasonInfrastructure, Launch: execution.LaunchNotStarted}))
		must(t, e.Svc.ObserveAttempt(ctx, first.Scope, first.AttemptID, execution.Observation{
			Kind: execution.ObservedTerminated, WorkloadName: first.WorkloadName, Launch: execution.LaunchNotStarted}))
		// Another profile version exists by now; the retry runs on a restarted controller.
		e.register(scope, e.variant(77))
		e.Clock.Advance(time.Minute)
		second := e.claim(e.newService(t), scope.TenantID)
		if second.RunID != run.ID || second.Ordinal != 2 || second.ActionKey != first.ActionKey {
			t.Fatalf("the retry: %+v", second)
		}
		secondCtx := e.attemptContext(second)
		if !reflect.DeepEqual(secondCtx.Command, firstCtx.Command) || !reflect.DeepEqual(secondCtx.Profile, firstCtx.Profile) ||
			!reflect.DeepEqual(secondCtx.DesignBaseline, firstCtx.DesignBaseline) || secondCtx.Limits != firstCtx.Limits {
			t.Fatal("a retry must run the pinned profile of the first attempt")
		}
		if secondCtx.AttemptID == firstCtx.AttemptID || secondCtx.ActionKey != firstCtx.ActionKey {
			t.Fatal("a retry is another attempt of the same logical step")
		}
	})

	t.Run("a writer agent step keeps the workspace publication identity", func(t *testing.T) {
		pw := e.NewProject(scope)
		wdef := e.define(scope, agentDoc("writer", p1.Ref(), "").replaceAccess())
		run := e.mustAdmit(pw, wdef)
		in := e.claim(e.Svc, scope.TenantID)
		if in.RunID != run.ID || in.Access != execution.AccessWrite {
			t.Fatalf("claim: %+v", in)
		}
		ctx := e.attemptContext(in)
		src := ctx.Workspace.Source
		if ctx.Workspace.Access != "write" || src == nil || src.Branch == nil || src.ExpectedTip == nil || string(*src.ExpectedTip) != executiontest.CommitA {
			t.Fatalf("a writer keeps its branch and expected tip: %+v", ctx.Workspace)
		}
		if *src.Branch == "" || string(src.Commit) != executiontest.CommitA {
			t.Fatalf("writer source: %+v", src)
		}
		if e.Count(`SELECT count(*) FROM workspaces WHERE writer_attempt_id = $1`, string(in.AttemptID)) != 1 {
			t.Fatal("the agent attempt must hold the workspace's single writer slot like any command")
		}
	})

	t.Run("container steps in the same plan are unchanged", func(t *testing.T) {
		pc := e.NewProject(scope)
		cdef := e.define(scope, pipelineWithContainer(p1.Ref()))
		run := e.mustAdmit(pc, cdef)
		agg := e.Get(run)
		build := agg.Step("build")
		if build.Spec.Image == e.Draft.Image || len(build.Spec.Args) != 1 || build.Spec.Args[0] != "make" {
			t.Fatalf("the container step was altered: %+v", build.Spec)
		}
		in := e.claim(e.Svc, scope.TenantID)
		if in.StepID != "build" {
			t.Fatalf("claimed %s", in.StepID)
		}
		ctx := e.attemptContext(in)
		if ctx.Profile != nil || ctx.DesignBaseline != nil {
			t.Fatal("a container attempt must carry no agent pins")
		}
	})

	_ = domain.RunID("")
}
