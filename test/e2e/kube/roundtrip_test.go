package kube_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/app"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/runtime/podman"
	"github.com/optimiware/kodama/internal/runtimeauth"
	rtpg "github.com/optimiware/kodama/internal/runtimeauth/postgres"
)

// backend is the runtime under test. T-026's three named roundtrips are written
// against a backend table (test/integration/assembly-execution): this suite
// re-runs their obligations with `kube` as the backend. The Kubernetes backend
// is the only entry of this package.
type backend struct{ Name string }

var backends = []backend{{Name: kube.Backend}}

// settle drives the loops until the run succeeded and returns its aggregate.
func (s *stack) settle(run *execution.Run, in *execution.AttemptSpec) *execution.Aggregate {
	s.t.Helper()
	s.until(4*time.Minute, "the run to succeed", func() (bool, string) {
		st := s.e.Get(run).Run.Status
		at, _ := s.attempt(in)
		return st == execution.RunSucceeded, string(st) + "/" + string(at.Status) + "/" + string(at.Class) + "/" + string(at.Reason)
	})
	return s.e.Get(run)
}

// dispatch claims and starts the next attempt of the tenant through the real
// dispatch path (execution.DispatchNext over the Kubernetes runtime).
func (s *stack) dispatch(run *execution.Run) *execution.AttemptSpec {
	s.t.Helper()
	claimed, err := s.svc.DispatchNext(context.Background(), run.Scope.TenantID)
	if err != nil || !claimed {
		s.t.Fatalf("dispatch: %v %v", claimed, err)
	}
	agg := s.e.Get(run)
	at := agg.Attempts[len(agg.Attempts)-1]
	return &execution.AttemptSpec{Scope: run.Scope, RunID: run.ID, StepID: at.StepID, AttemptID: at.ID, WorkloadName: at.WorkloadName}
}

// Test_kube_component_roundtrip repeats the M1 execution roundtrips of T-026
// (execution_assembly_roundtrip and execution_assembly_result_guard, PX-A02,
// PX-A06, IT-A05 in their Kubernetes form) with `kube` as the backend: a run
// admitted through the real execution service is dispatched as a restricted Job,
// the actual entrypoint admits itself, runs the command and commits its result,
// and the result guards hold. PX-03, PX-05, PX-12, IT-03, IT-06, AC-12.
func Test_kube_component_roundtrip(t *testing.T) {
	for _, b := range backends {
		t.Run(b.Name, func(t *testing.T) {
			t.Run("production guard", func(t *testing.T) { productionGuard(t, b) })
			t.Run("roundtrip", func(t *testing.T) { roundtrip(t, b) })
			t.Run("result guard", func(t *testing.T) { resultGuard(t, b) })
		})
	}
}

// productionGuard: the runtime set the production environment composes is the
// one under test. The construction guard of SD-12 admits the real Kubernetes
// runtime, Job observer and planner, and refuses a runtime the `production`
// environment must never run (the Process and Podman backends).
func productionGuard(t *testing.T, b backend) {
	s := newStack(t)
	sel, err := app.Selections(app.Production)
	must(t, err)
	if got := sel[app.PortRuntime]; got.Adapter != b.Name || got.Restricted {
		t.Fatalf("production selects %+v for the runtime, want the unrestricted %q", got, b.Name)
	}
	set := app.RuntimeSet{
		Runtime: s.runtime, Jobs: s.observer, Planner: kube.NewPlanner(s.policy),
		Authority: func(*execpg.Store) (rtpg.AttemptAuthority, error) { return s.auth, nil },
	}
	if _, err := app.AdmitRuntime(app.Production, set); err != nil {
		t.Fatalf("the production environment refuses the Kubernetes runtime: %v", err)
	}
	pod, err := podman.New(podman.Config{Program: podman.DeclaredCommand{}, StateDir: t.TempDir()})
	must(t, err)
	if _, err := app.AdmitRuntime(app.Production, app.RuntimeSet{Runtime: pod, Jobs: pod, Authority: set.Authority}); err == nil {
		t.Fatal("the production environment admitted the Podman runtime")
	}
}

func roundtrip(t *testing.T, b backend) {
	s := newStack(t)
	ctx := context.Background()
	if got := s.runtime.Capabilities().Backend; got != b.Name {
		t.Fatalf("backend %q, want %q", got, b.Name)
	}
	run, _ := s.submit(executiontest.ScopeA, stepSpec{Script: "echo kube-component-log-marker\n" + quick, Outputs: []string{"v"}})
	in := s.dispatch(run)

	// The attempt's identity is the Job's: the deterministic name and the UID
	// the runtime bound at dispatch (PX-03).
	job, ok := s.jobOf(in)
	if !ok {
		t.Fatal("dispatch created no Job")
	}
	if job.Metadata.Name != in.WorkloadName {
		t.Fatalf("Job %q, attempt workload %q", job.Metadata.Name, in.WorkloadName)
	}
	at, _ := s.attempt(in)
	if at.WorkloadUID != job.Metadata.UID || at.WorkloadName != in.WorkloadName {
		t.Fatalf("bound identity %q/%q, Job %q/%q", at.WorkloadName, at.WorkloadUID, in.WorkloadName, job.Metadata.UID)
	}
	if at.Ordinal != 1 {
		t.Fatalf("ordinal %d", at.Ordinal)
	}

	// The command's output is observable through the runtime while the workload
	// exists (the log read the log API will be bound to).
	handle := execution.Workload{Scope: in.Scope, AttemptID: in.AttemptID, Name: in.WorkloadName, UID: job.Metadata.UID}
	eventually(t, 3*time.Minute, "the workload's log to carry the command's output", func() (bool, string) {
		var buf bytes.Buffer
		err := s.runtime.Logs(ctx, handle, &buf)
		return err == nil && strings.Contains(buf.String(), "kube-component-log-marker"), fmt.Sprintf("%v %q", err, buf.String())
	})

	agg := s.settle(run, in)
	if got := strOutput(t, agg, "v"); got != "ok" {
		t.Fatalf("output %q", got)
	}
	at, ctl := s.attempt(in)
	if at.Status != execution.AttemptSucceeded || !ctl.Settled() {
		t.Fatalf("attempt %s settled=%v", at.Status, ctl.Settled())
	}
	if at.WorkloadUID != job.Metadata.UID {
		t.Fatal("the bound workload instance changed")
	}

	// Admission was one-time: exactly one successful admit of the entrypoint, and
	// its token authorizes nothing once the attempt settled (IT-03, IT-06).
	ok200 := 0
	for _, e := range s.srv.admitEvents(in.AttemptID) {
		if e.Status == 200 {
			ok200++
		}
	}
	if ok200 != 1 {
		t.Fatalf("%d successful admissions: %+v", ok200, s.srv.admitEvents(in.AttemptID))
	}
	tok := s.srv.token(in.AttemptID)
	if tok == "" {
		t.Fatal("no token was issued")
	}
	if _, err := s.registry.Authorize(ctx, runtimeauth.AuthorizeRequest{AttemptID: in.AttemptID, Bearer: tok, Action: runtimeauth.ActionContextRead}); !errors.Is(err, runtimeauth.ErrAttemptRevoked) {
		t.Fatalf("the settled attempt's token still reads its context: %v", err)
	}

	// Transitions are recorded durably and in order (PX-05): the attempt is
	// dispatched, then running, then succeeded, and the run ends after it.
	hist := s.history(run)
	last := -1
	for _, want := range []string{"attempt:" + string(execution.AttemptSucceeded), "run:" + string(execution.RunSucceeded)} {
		i := indexOf(hist, want)
		if i < 0 || i < last {
			t.Fatalf("history out of order (want %s after index %d): %v", want, last, hist)
		}
		last = i
	}

	// The settled attempt's workload is removed by the reconciler.
	s.until(2*time.Minute, "the settled Job to be removed", func() (bool, string) {
		_, still := s.jobOf(in)
		return !still, "the Job still exists"
	})
	s.until(2*time.Minute, "the admission Secret to be removed with the workload", func() (bool, string) {
		return !s.secretExists(in), "the admission Secret still exists"
	})
	if s.held() != 0 {
		t.Fatalf("%d reservations still held", s.held())
	}

	// Stored responses replay: the same result commits to the same ack, and a
	// different one never rewrites it (AC-12, PX-12).
	res := runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: ptrTo(int64(0)), Outputs: map[string]runtimev1.Value{"v": runtimev1.StringValue("ok")}, DiagnosticArtifactIDs: []domain.ArtifactID{}}
	hash, err := runtimev1.ResultHash(res)
	must(t, err)
	if _, err := s.svc.CommitResult(ctx, execution.ResultCommit{Scope: in.Scope, AttemptID: in.AttemptID, Request: runtimev1.ResultRequest{ResultHash: hash, Result: res}}); !errors.Is(err, execution.ErrAttemptRevoked) && err != nil && !errors.Is(err, execution.ErrResultConflict) {
		t.Fatalf("a result after settlement: %v", err)
	}
	if got := strOutput(t, s.e.Get(run), "v"); got != "ok" {
		t.Fatalf("the settled output changed: %q", got)
	}
}

// resultGuard: the result commit refuses what the contract refuses, and nothing
// the entrypoint or a stranger sends changes a settled attempt (T-026
// execution_assembly_result_guard on `kube`).
func resultGuard(t *testing.T, b backend) {
	s := newStack(t)
	ctx := context.Background()

	// A missing declared output fails the attempt instead of succeeding it.
	run, _ := s.submit(executiontest.ScopeA, stepSpec{Script: `printf '{"outcome":"completed","outputs":{}}' > "$KODAMA_RESULTS_FILE"`, Outputs: []string{"v"}})
	in := s.dispatch(run)
	s.until(4*time.Minute, "the run to settle", func() (bool, string) {
		st := s.e.Get(run).Run.Status
		return st.Terminal(), string(st)
	})
	if got := s.e.Get(run).Run.Status; got == execution.RunSucceeded {
		t.Fatal("a missing declared output succeeded the run")
	}
	at, _ := s.attempt(in)
	if at.Status == execution.AttemptSucceeded {
		t.Fatal("a missing declared output succeeded the attempt")
	}
	if _, ok := s.e.Get(run).Step("work").Outputs["v"]; ok {
		t.Fatal("outputs were exposed for a failed attempt")
	}

	// A result for another attempt's token is refused and changes nothing: the
	// first run's attempt has no authority over the second's.
	run2, _ := s.submit(executiontest.ScopeA, stepSpec{Script: quick, Outputs: []string{"v"}})
	in2 := s.dispatch(run2)
	s.settle(run2, in2)
	res := runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: ptrTo(int64(0)), Outputs: map[string]runtimev1.Value{"v": runtimev1.StringValue("forged")}, DiagnosticArtifactIDs: []domain.ArtifactID{}}
	hash, err := runtimev1.ResultHash(res)
	must(t, err)
	if _, err := s.registry.Authorize(ctx, runtimeauth.AuthorizeRequest{AttemptID: in2.AttemptID, Bearer: s.srv.token(in.AttemptID), Action: runtimeauth.ActionResultSubmit}); err == nil {
		t.Fatal("another attempt's token authorizes this attempt")
	}
	if _, err := s.svc.CommitResult(ctx, execution.ResultCommit{Scope: in2.Scope, AttemptID: in2.AttemptID, Request: runtimev1.ResultRequest{ResultHash: hash, Result: res}}); err == nil {
		t.Fatal("a different result rewrote a settled attempt")
	}
	if got := strOutput(t, s.e.Get(run2), "v"); got != "ok" {
		t.Fatalf("the settled output changed: %q", got)
	}

	// A malformed result (a hash that is not the result's) is invalid.
	bad := runtimev1.ResultRequest{ResultHash: runtimev1.Digest("sha256:" + strings.Repeat("0", 64)), Result: res}
	if _, err := s.svc.CommitResult(ctx, execution.ResultCommit{Scope: in2.Scope, AttemptID: in2.AttemptID, Request: bad}); err == nil {
		t.Fatal("a result with a wrong hash was accepted")
	}

	// A database outage during the callback rolls the commit back and the same
	// result commits once afterwards is covered by the PostgreSQL contract
	// suites of T-026; a refused result route leaves the Job's result to be
	// retried by the entrypoint.
	s.srv.RefuseResults.Store(true)
	run3, _ := s.submit(executiontest.ScopeA, stepSpec{Script: quick, Outputs: []string{"v"}})
	in3 := s.dispatch(run3)
	eventually(t, 3*time.Minute, "the entrypoint to call the refused result route", func() (bool, string) {
		return s.srv.results.Load() > 1, "calls"
	})
	if st := s.attemptStatus(in3); st == execution.AttemptSucceeded {
		t.Fatal("an attempt succeeded while its result was refused")
	}
	s.srv.RefuseResults.Store(false)
	s.settle(run3, in3)
	if got := strOutput(t, s.e.Get(run3), "v"); got != "ok" {
		t.Fatalf("output %q", got)
	}
}

func indexOf(list []string, want string) int {
	for i, v := range list {
		if v == want {
			return i
		}
	}
	return -1
}

// history lists the run's recorded transitions ("<subject>:<new status>") in
// commit order.
func (s *stack) history(run *execution.Run) []string {
	s.t.Helper()
	rows, err := s.e.Migration.Query(context.Background(), `SELECT subject_kind || ':' || to_status FROM run_history WHERE tenant_id = $1 AND run_id = $2 ORDER BY seq`, string(run.Scope.TenantID), string(run.ID))
	must(s.t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		must(s.t, rows.Scan(&v))
		out = append(out, v)
	}
	must(s.t, rows.Err())
	return out
}
