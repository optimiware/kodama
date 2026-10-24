package composition_test

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	rpg "github.com/optimiware/kodama/internal/results/postgres"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/storage"
)

const (
	labelJob  = "outputs: {label: {type: string}}"
	secretDoc = `env: {TOKEN: {secretRef: "secret://project/token"}}`
)

func outputsOf(label string) map[string]runtimev1.Value {
	return map[string]runtimev1.Value{"label": runtimev1.StringValue(label)}
}

func decodeAck(t testing.TB, body []byte) runtimev1.ResultResponse {
	t.Helper()
	var r runtimev1.ResultResponse
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return r
}

func errCode(t testing.TB, body []byte) domain.ErrorCode {
	t.Helper()
	var r runtimev1.ErrorResponse
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return r.Error.Code
}

// Snapshot is every durable effect an execution transaction can leave in its
// peers, counted as the owner role: equal snapshots before and after a failed
// mutation prove it rolled back as a whole.
type Snapshot struct {
	Runs, Attempts, History, Audit, Outbox        int
	HeldReservations, ReleasedReservations        int
	GrantsRevoked, ControlsRevoked, Terminated    int
	PinsLive, PinsReleased, WritersHeld, Retained int
	CancelIntents, Evidence, RunsCancelRequested  int
	PublicationRows                               int
}

// Snap counts the effects across the whole task database (one per test).
func (e *Env) Snap(tenant domain.TenantID) Snapshot {
	e.T.Helper()
	c := e.Count
	return Snapshot{
		Runs: c(`SELECT count(*) FROM runs`), Attempts: c(`SELECT count(*) FROM step_attempts`),
		History: c(`SELECT count(*) FROM run_history`), Audit: c(`SELECT count(*) FROM audit`),
		Outbox:               e.X.CountTenant(tenant, `SELECT count(*) FROM outbox`),
		HeldReservations:     c(`SELECT count(*) FROM execution_reservations WHERE released_at IS NULL`),
		ReleasedReservations: c(`SELECT count(*) FROM execution_reservations WHERE released_at IS NOT NULL`),
		GrantsRevoked:        c(`SELECT count(*) FROM runtime_grants WHERE revoked_at IS NOT NULL`),
		ControlsRevoked:      c(`SELECT count(*) FROM attempt_controls WHERE revoked_at IS NOT NULL`),
		Terminated:           c(`SELECT count(*) FROM attempt_controls WHERE termination_confirmed_at IS NOT NULL`),
		PinsLive:             c(`SELECT count(*) FROM secret_pins WHERE released_at IS NULL`),
		PinsReleased:         c(`SELECT count(*) FROM secret_pins WHERE released_at IS NOT NULL`),
		WritersHeld:          c(`SELECT count(*) FROM workspaces WHERE writer_attempt_id IS NOT NULL`),
		Retained:             c(`SELECT count(*) FROM workspace_retained_refs`),
		CancelIntents:        c(`SELECT count(*) FROM work_cancellation_requests`),
		Evidence:             c(`SELECT count(*) FROM attempt_evidence`),
		RunsCancelRequested:  c(`SELECT count(*) FROM runs WHERE cancel_requested_at IS NOT NULL`),
		PublicationRows:      c(`SELECT count(*) FROM workspace_publications`),
	}
}

// GrantRevoked reports whether the attempt's runtime grant is revoked.
func (e *Env) GrantRevoked(a *Attempt) bool {
	e.T.Helper()
	return e.Count(`SELECT count(*) FROM runtime_grants WHERE attempt_id = $1 AND revoked_at IS NOT NULL`, string(a.ID())) == 1
}

// Define registers a definition of the steps.
func (e *Env) Define(scope domain.Scope, steps ...string) pipeline.Definition {
	e.T.Helper()
	return e.X.Define(scope, pipelinetest.Doc(steps...))
}

// AdmitRun admits a run of the project without claiming it.
func (e *Env) AdmitRun(p Project, def pipeline.Definition) (*execution.Run, error) {
	return e.ExecPlain.Admit(context.Background(), execution.Caller{PrincipalID: executiontest.Engineer, RequestID: "req_t062"}, execution.AdmitCommand{
		Scope: p.Scope, WorkItemID: p.Item, DefinitionID: def.ID, ContentRevision: e.X.ContentRevision(executiontest.Project{Scope: p.Scope, Item: p.Item}),
		SourceCommit: e.Source(),
	})
}

// Unadmitted admits, claims and starts a run whose attempt has no runtime grant
// yet; e.Register registers it afterwards.
func (e *Env) Unadmitted(p Project, steps ...string) *Attempt {
	e.T.Helper()
	def := e.Define(p.Scope, steps...)
	run, err := e.AdmitRun(p, def)
	must(e.T, err)
	return e.startUnadmitted(p, run, true)
}

// Claimed admits and claims a run's first step without observing its command
// start, and registers its grant: the attempt whose launch is still unknown.
func (e *Env) Claimed(p Project, steps ...string) *Attempt {
	e.T.Helper()
	run, err := e.AdmitRun(p, e.Define(p.Scope, steps...))
	must(e.T, err)
	a := e.startUnadmitted(p, run, false)
	e.admit(a)
	return a
}

// Register registers the attempt's runtime grant (the executor's step before the
// workload's admission) and returns the error.
func (e *Env) Register(a *Attempt) error {
	e.T.Helper()
	cred, err := runtimeauth.GenerateCredential(nil)
	must(e.T, err)
	actions, err := rpg.DefaultActions(a.Intent.Access)
	must(e.T, err)
	_, err = e.Auth.Register(context.Background(), runtimeauth.RegisterCommand{Scope: a.Scope, AttemptID: a.ID(), Credential: cred, Actions: actions, Context: a.Context})
	return err
}

// Commit submits a result through execution's trusted (unguarded) path.
func (e *Env) CommitPlain(a *Attempt, res runtimev1.Result) (execution.ResultAck, error) {
	h, err := runtimev1.ResultHash(res)
	must(e.T, err)
	return e.ExecPlain.CommitResult(context.Background(), execution.ResultCommit{
		Scope: a.Scope, AttemptID: a.ID(), Request: runtimev1.ResultRequest{ResultHash: h, Result: res},
	})
}

// Failed is a failed command result.
func Failed() runtimev1.Result {
	code, reason := int64(1), "command_failed"
	return runtimev1.Result{Outcome: runtimev1.OutcomeFailed, ExitCode: &code, Reason: &reason, Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}}
}

// NeedsInput is a needs-input result.
func NeedsInput() runtimev1.Result {
	r := Failed()
	r.Outcome = runtimev1.OutcomeNeedsInput
	r.Reason = nil
	question := "Which environment should this deploy to?"
	r.Question = &question
	return r
}

// CancelErr cancels the attempt's run through the trusted path and returns the error.
func (e *Env) CancelErr(a *Attempt) error {
	_, err := e.ExecPlain.Cancel(context.Background(), execution.Caller{PrincipalID: executiontest.Engineer, RequestID: "req_t062"}, a.Scope, a.Run.ID)
	return err
}

// byXid groups a trace by transaction identity, each group in call order.
func byXid(hits []Hit) map[int64][]Hit {
	out := map[int64][]Hit{}
	for _, h := range hits {
		out[h.Xid] = append(out[h.Xid], h)
	}
	return out
}

// assertOrdered fails unless, inside every transaction of the trace, the
// listed points occur in the listed order (a point absent from a transaction
// is skipped; the first occurrence of each is compared).
func assertOrdered(t testing.TB, hits []Hit, points ...string) {
	t.Helper()
	for xid, group := range byXid(hits) {
		last, lastPoint := -1, ""
		for _, p := range points {
			for _, h := range group {
				if h.Point == p || strings.HasPrefix(h.Point, p+":") {
					if h.Seq < last {
						t.Fatalf("transaction %d: %s ran before %s (lock order inversion)", xid, p, lastPoint)
					}
					last, lastPoint = h.Seq, p
					break
				}
			}
		}
	}
}

// onePositiveXid returns the single transaction identity of the hits, failing
// when they span several transactions or the identity was not readable.
func onePositiveXid(t testing.TB, hits []Hit) int64 {
	t.Helper()
	if len(hits) == 0 {
		t.Fatal("no peer call was observed")
	}
	xid := hits[0].Xid
	for _, h := range hits {
		if h.Xid != xid || h.Xid <= 0 {
			t.Fatalf("peer calls span transactions or lost identity: %+v", hits)
		}
	}
	return xid
}

func points(hits []Hit) []string {
	var out []string
	for _, h := range hits {
		out = append(out, h.Point)
	}
	sort.Strings(out)
	return out
}

func (e *Env) inTenant(tenant domain.TenantID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return storage.WithTenant(context.Background(), e.Pool, tenant, fn)
}

func outbox(e *Env, tenant domain.TenantID) int {
	return e.X.CountTenant(tenant, `SELECT count(*) FROM outbox`)
}

// executiontestOptions are the service options of a reconstructed execution
// service over the runtime-enabled store, with or without the effect adapter.
func executiontestOptions(e *Env, noEffects bool) executiontest.Options {
	return executiontest.Options{Store: e.RT, Sources: e.X.Sources, NoEffects: noEffects}
}
