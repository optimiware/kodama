package integration_test

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/runtime/runtimetest"
)

// TestAgentAdmissionRecovery verifies the durable admission and recovery
// semantics of agent steps against real PostgreSQL: keyed replay returns the
// original run and its original pins, an expansion failure writes nothing on
// either path, both paths expand identically, and a lost dispatch is redriven
// with the same attempt and context identity.
func TestAgentAdmissionRecovery(t *testing.T) {
	scope := executiontest.ScopeA

	t.Run("keyed replay preserves the original run and its pins", func(t *testing.T) {
		e := newEnv(t)
		p1 := e.register(scope, e.Draft)
		p := e.NewProject(scope)
		def := e.define(scope, string(agentDoc("replay", p1.Ref(), "")))

		first := e.httpCreate(p, def, "key-replay")
		if first.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", first.Code, first.Body.String())
		}
		run := e.runRef(scope, runID(t, first))
		before := e.Get(run).Step("implement").Spec

		// The profile changes after the run was admitted; the client replays.
		e.register(scope, e.variant(2))
		again := e.httpCreate(p, def, "key-replay")
		if again.Code != http.StatusCreated || again.Header().Get("Idempotent-Replayed") != "true" || again.Body.String() != first.Body.String() {
			t.Fatalf("replay: %d replayed=%q", again.Code, again.Header().Get("Idempotent-Replayed"))
		}
		if n := e.Count(`SELECT count(*) FROM runs`); n != 1 {
			t.Fatalf("the replay created a run: %d", n)
		}
		if after := e.Get(run).Step("implement").Spec; !reflect.DeepEqual(before, after) {
			t.Fatal("the original run's frozen step must not change")
		}
		// A new key is a rerun and pins the profile of its own admission: the
		// reference in the definition is the exact version, so it is the same one.
		rerun := e.httpCreate(p, def, "key-rerun")
		if rerun.Code != http.StatusCreated || runID(t, rerun) == run.ID {
			t.Fatalf("rerun: %d", rerun.Code)
		}
		if !reflect.DeepEqual(e.Get(e.runRef(scope, runID(t, rerun))).Step("implement").Spec, before) {
			t.Fatal("an exact reference always resolves to the same pins")
		}
	})

	t.Run("an expansion failure writes nothing on either path", func(t *testing.T) {
		e := newEnv(t)
		p1 := e.register(scope, e.Draft)
		p := e.NewProject(scope)
		def := e.define(scope, string(agentDoc("fail", p1.Ref(), "")))
		tables := []string{"runs", "step_runs", "run_history", "workspace_retained_refs", "secret_pins", "outbox", "step_attempts"}
		snapshot := func() map[string]int {
			out := map[string]int{"audit": e.Count(`SELECT count(*) FROM audit`)}
			for _, tb := range tables {
				out[tb] = e.Count(`SELECT count(*) FROM ` + tb)
			}
			return out
		}
		before := snapshot()

		e.Approvals.WithdrawModel("t030-model") // the profile no longer resolves for new runs
		if _, err := e.admit(p, def); !errors.Is(err, execution.ErrAdmissionRefused) {
			t.Fatalf("ordinary path: %v", err)
		}
		rec := e.httpCreate(p, def, "key-fail")
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("keyed path: %d %s", rec.Code, rec.Body.String())
		}
		if after := snapshot(); !reflect.DeepEqual(before, after) {
			t.Fatalf("a failed expansion changed state:\n%v\n%v", before, after)
		}
		// Nothing is stuck: once the approval is back, the same key succeeds.
		e.Approvals.AllowModel("t030-model")
		ok := e.httpCreate(p, def, "key-fail")
		if ok.Code != http.StatusCreated {
			t.Fatalf("retry after recovery: %d %s", ok.Code, ok.Body.String())
		}
		if n := e.Count(`SELECT count(*) FROM runs`); n != 1 {
			t.Fatalf("%d runs", n)
		}
	})

	t.Run("a stored definition naming a moving profile alias is refused at admission", func(t *testing.T) {
		e := newEnv(t)
		e.register(scope, e.Draft)
		p := e.NewProject(scope)
		// A definition registered through the repository directly (the validator
		// would have refused the unknown version) simulates drift between
		// registration and admission.
		def := e.Define(scope, agentDocDirect("coding-v1"))
		if _, err := e.admit(p, def); !errors.Is(err, execution.ErrAdmissionRefused) {
			t.Fatalf("an unknown profile version must refuse admission: %v", err)
		}
		if n := e.Count(`SELECT count(*) FROM runs`); n != 0 {
			t.Fatalf("%d runs", n)
		}
	})

	t.Run("dispatch redrive preserves attempt, job and context identity", func(t *testing.T) {
		e := newEnv(t)
		p1 := e.register(scope, e.Draft)
		p := e.NewProject(scope)
		run := e.mustAdmit(p, e.define(scope, string(agentDoc("redrive", p1.Ref(), ""))))

		calls := 0
		e.Workloads.After(runtimetest.OpStart, func(context.Context, runtimetest.Call) error {
			calls++
			if calls == 1 {
				return errors.New("connection lost after the workload create")
			}
			return nil
		})
		ctx := context.Background()
		claimed, err := e.Svc.DispatchNext(ctx, scope.TenantID)
		if err != nil || !claimed {
			t.Fatalf("dispatch: %v %v", claimed, err)
		}
		first := e.Workloads.Starts()[0]
		var storedBefore string
		e.Scalar(&storedBefore, `SELECT context FROM step_attempts WHERE tenant_id = $1 AND id = $2`, string(scope.TenantID), string(first.AttemptID))

		// A newer profile version and a restarted controller do not matter.
		e.register(scope, e.variant(9))
		e.Clock.Advance(3 * time.Second)
		rep, err := e.newService(t).ReconcileTenant(ctx, scope.TenantID)
		if err != nil || rep.Redriven != 1 {
			t.Fatalf("redrive: %+v %v", rep, err)
		}
		second := e.Workloads.Starts()[1]
		if !second.Redrive || second.AttemptID != first.AttemptID || second.WorkloadName != first.WorkloadName ||
			second.SpecFingerprint != first.SpecFingerprint || second.ContextDigest != first.ContextDigest {
			t.Fatalf("redrive changed identity:\n%+v\n%+v", second, first)
		}
		var storedAfter string
		e.Scalar(&storedAfter, `SELECT context FROM step_attempts WHERE tenant_id = $1 AND id = $2`, string(scope.TenantID), string(first.AttemptID))
		if storedAfter != storedBefore {
			t.Fatal("the pinned context of an attempt is immutable")
		}
		if n := e.Count(`SELECT count(*) FROM step_attempts WHERE run_id = $1`, string(run.ID)); n != 1 {
			t.Fatalf("a redrive must not create another attempt: %d", n)
		}
	})
}

// agentDocDirect is a pipeline document with an agent step referencing ref, for
// registration through the repository without profile validation (the fixture's
// executiontest validator accepts any agent profile name).
func agentDocDirect(ref string) string {
	return "apiVersion: kodama/v1alpha1\nkind: Pipeline\nmetadata: {name: drift}\nspec:\n  inputs: {}\n  steps:\n" +
		"    - {id: implement, kind: agent, agent: {profile: " + ref + "}, timeoutSeconds: 600, workspace: {access: read, from: run}}\n"
}
