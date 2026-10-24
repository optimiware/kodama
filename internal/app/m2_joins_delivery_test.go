package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	wrapperspec "github.com/optimiware/kodama/images/harness-opencode/wrapper/spec"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/execution"
	execadapters "github.com/optimiware/kodama/internal/execution/adapters"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/gates"
	gatesadapters "github.com/optimiware/kodama/internal/gates/adapters"
	"github.com/optimiware/kodama/internal/gates/gatestest/gatescontract"
	gatesmemory "github.com/optimiware/kodama/internal/gates/gatestest/memory"
	gatespg "github.com/optimiware/kodama/internal/gates/postgres"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	"github.com/optimiware/kodama/internal/secrets"
	secretspg "github.com/optimiware/kodama/internal/secrets/postgres"
	"github.com/optimiware/kodama/internal/workspace"
)

// ---- a real execution store over inert peers ------------------------------------

type inertDB struct{}

func (inertDB) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	return nil, errors.New("no database in the unit layer")
}

type inertEvents struct{}

func (inertEvents) Append(context.Context, pgx.Tx, eventsv1.Envelope) (eventspg.AppendResult, error) {
	return eventspg.AppendResult{}, errors.New("no event store in the unit layer")
}

type inertWorkspaces struct{}

func (inertWorkspaces) UpdateInTx(context.Context, pgx.Tx, domain.Scope, workspace.ID, workspace.Load, workspace.Mutation) (*workspace.Aggregate, error) {
	return nil, errors.New("no workspace store in the unit layer")
}

type inertPins struct{}

func (inertPins) RegisterPins(context.Context, pgx.Tx, secretspg.PinRequest) error {
	return errors.New("no secret store in the unit layer")
}
func (inertPins) ReleasePins(context.Context, pgx.Tx, domain.Scope, secrets.ConsumerRef) (int64, error) {
	return 0, errors.New("no secret store in the unit layer")
}

// executionStore builds the real PostgreSQL execution store the way NewCore
// does, through the joins' peers, over inert transaction seams.
func executionStore(t *testing.T, j DeliveryJoins) *execpg.Store {
	t.Helper()
	s, err := execpg.NewRuntimeStore(inertDB{}, inertEvents{}, j.executionPeers(execpg.Peers{
		Authority: execadapters.BindAuthority, Workspaces: inertWorkspaces{}, Secrets: inertPins{},
	}), func(context.Context, pgx.Tx, domain.Scope, domain.AttemptID, time.Time) (int, error) { return 0, nil }, execpg.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// ---- a scripted transaction row reader --------------------------------------------

type depRow struct {
	values []any
	err    error
}

func (r depRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.values) {
		return errors.New("column count mismatch")
	}
	for i, d := range dest {
		switch p := d.(type) {
		case *string:
			*p = r.values[i].(string)
		case **time.Time:
			*p, _ = r.values[i].(*time.Time)
		default:
			return errors.New("unexpected destination")
		}
	}
	return nil
}

// rowTx is the open transaction of a gate decision: it answers the one
// deployment read and records what it was asked.
type rowTx struct {
	pgx.Tx
	row     pgx.Row
	queries []string
}

func (t *rowTx) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	t.queries = append(t.queries, sql)
	return t.row
}

func liveDeployment(scope domain.Scope, id string) depRow {
	return depRow{values: []any{string(scope.TenantID), string(scope.ProjectID), id, "sha256:" + strings.Repeat("c", 64), strings.Repeat("a", 40), (*time.Time)(nil), string(delivery.CleanupNone), (*time.Time)(nil)}}
}

// fullJoins is what NewCore binds from the stores, with the committed reader of
// the test.
func fullJoins(reader gates.Deployments, secrets SecretResolver) DeliveryJoins {
	return DeliveryJoins{
		Environments: execadapters.BindDeliveryEnvironments, Deployments: gatesadapters.BindDeployments, DeploymentReader: reader,
		Secrets: secrets, ConformanceOutput: ConformanceReportOutput,
	}
}

// Test_app_delivery_joins_bound: the T-097 joins reach the composition. An
// environment-declaring step is admitted by a store that binds delivery's
// revalidation, a deployment-bearing gate is decidable through the gate store's
// binder, the runtime secret route is mounted, and each of them fails closed
// and is reported disabled without its configuration.
func Test_app_delivery_joins_bound(t *testing.T) {
	ctx := context.Background()

	t.Run("the execution store binds environments in its admission transaction", func(t *testing.T) {
		bound := executionStore(t, fullJoins(nil, nil))
		if !bound.AdmitsEnvironments() || !admitsEnvironments(bound) || !admitsEnvironments(Decorated(&fxStore{Store: bound}, bound)) {
			t.Fatal("the store does not revalidate declared environments")
		}
		unbound := executionStore(t, DeliveryJoins{})
		if unbound.AdmitsEnvironments() || admitsEnvironments(Decorated(&fxStore{Store: unbound}, unbound)) {
			t.Fatal("a store without the join reports that it binds environments")
		}
		// The bound reader is the production one: through a transaction that cannot
		// be read, an admission is an outage and never a pass.
		tx := &rowTx{row: depRow{err: errors.New("connection reset")}}
		bind := fullJoins(nil, nil).Environments(tx)
		err := bind.BindEnvironment(ctx, execution.EnvironmentBinding{
			Scope: ceilingScope, Actor: execution.Actor{ID: "u_engineer", Type: execution.ActorUser}, Pin: runtimev1.EnvironmentPin{ID: "env_staging", ConfigDigest: runtimev1.Digest("sha256:" + strings.Repeat("3", 64))},
		})
		if !errors.Is(err, execution.ErrUnavailable) || len(tx.queries) == 0 {
			t.Fatalf("an unreadable admission transaction: %v (%d reads)", err, len(tx.queries))
		}
	})

	t.Run("the environment ports follow the store's own statement", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			joins DeliveryJoins
			bound bool
		}{{"joined", fullJoins(nil, nil), true}, {"not joined", DeliveryJoins{}, false}} {
			store := executionStore(t, tc.joins)
			ports, err := BindDelivery(DeliveryPortInputs{
				Env: Test, Delivery: dvBoundary{newDVRecords(func() time.Time { return dvNow })}, Records: newDVStore(), Runs: &dvRuns{}, Bindings: &dvBindings{},
				AdmitsEnvironments: store.AdmitsEnvironments(),
			})
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if (ports.Environments != nil) != tc.bound || (ports.CleanupEnvironments != nil) != tc.bound {
				t.Errorf("%s: environment ports %+v", tc.name, ports)
			}
			names, reasons := tc.joins.Disabled(store.AdmitsEnvironments())
			if contains(names, capStepEnvironments) == tc.bound || (reasons[capStepEnvironments] == "") == !tc.bound {
				t.Errorf("%s: reported %v %v", tc.name, names, reasons)
			}
		}
	})

	t.Run("an environment-declaring step is admitted through the assembled service when joined and refused when not", func(t *testing.T) {
		checkEnvironmentAdmission(t)
	})

	t.Run("a deployment-bearing gate is decidable with the joins and not created without them", func(t *testing.T) {
		// build composes the gate service of NewCore over the memory gate module:
		// the baseline has neither the deployment reader nor the conformance
		// mapping, exactly what NewCore had before the joins.
		build := func(t *testing.T, joins func(h *gatesmemory.Harness) DeliveryJoins) (*gatesmemory.Harness, *gates.Service, gatescontract.Case) {
			t.Helper()
			h := gatesmemory.NewHarness(t)
			c := h.NewCase(t, true)
			j := joins(h)
			d := h.E.Dependencies()
			d.Deployments, d.Conformance = nil, nil
			svc, err := gates.NewService(j.gateDependencies(d, gatesadapters.ConformanceMapping{ReportOutput: j.ConformanceOutput}), h.E.ServiceOptions())
			if err != nil {
				t.Fatal(err)
			}
			return h, svc, c
		}
		joined := func(h *gatesmemory.Harness) DeliveryJoins { return fullJoins(h.E.Deployments, nil) }

		_, svc, c := build(t, joined)
		g, created, err := svc.RequestForStep(ctx, c.Scope, c.Run, c.Step)
		if err != nil || !created {
			t.Fatalf("a deployment gate: %v created=%v", err, created)
		}
		if id, cfg := g.Subject.Deployment(); id != c.Deployment || cfg == "" {
			t.Fatalf("the subject binds %q %q", id, cfg)
		}
		decided, err := svc.Decide(ctx, gates.Caller{PrincipalID: gatesmemory.Reviewer, RequestID: "req_t101"}, g.Scope, g.ID, gates.DecideCommand{
			Decision: gates.DecisionApprove, Reason: "Reviewed the identified source and deployment.", ExpectedVersion: g.Version, SubjectDigest: g.SubjectDigest(),
		})
		if err != nil || decided.Status != gates.StatusApproved {
			t.Fatalf("the deployment gate was not decided: %v", err)
		}

		for name, joins := range map[string]func(h *gatesmemory.Harness) DeliveryJoins{
			"no join": func(*gatesmemory.Harness) DeliveryJoins { return DeliveryJoins{} },
			"the binder alone": func(*gatesmemory.Harness) DeliveryJoins {
				return DeliveryJoins{Deployments: gatesadapters.BindDeployments, ConformanceOutput: ConformanceReportOutput}
			},
			"the reader alone": func(h *gatesmemory.Harness) DeliveryJoins {
				return DeliveryJoins{DeploymentReader: h.E.Deployments, ConformanceOutput: ConformanceReportOutput}
			},
		} {
			h, svc, c := build(t, joins)
			if _, _, err := svc.RequestForStep(ctx, c.Scope, c.Run, c.Step); !errors.Is(err, gates.ErrNotConfigured) {
				t.Fatalf("%s: a deployment gate was requested: %v", name, err)
			}
			if _, err := h.E.Store.ByStep(ctx, c.Scope, c.Run, c.Step); !errors.Is(err, gates.ErrNotFound) {
				t.Fatalf("%s: a gate exists for a deployment that cannot be verified: %v", name, err)
			}
			names, reasons := joins(h).Disabled(true)
			if !contains(names, capDeploymentGates) || reasons[capDeploymentGates] == "" {
				t.Fatalf("%s: not reported: %v %v", name, names, reasons)
			}
		}
	})

	t.Run("the gate store's binder reads the deployment in the deciding transaction", func(t *testing.T) {
		scope := gatesmemory.ScopeA
		join := fullJoins(nil, nil)
		if join.gateStore(gatespg.Config{}).Deployments == nil || (DeliveryJoins{}).gateStore(gatespg.Config{}).Deployments != nil {
			t.Fatal("the gate store configuration does not follow the join")
		}
		read := func(row depRow) (gates.DeploymentSnapshot, error, *rowTx) {
			tx := &rowTx{row: row}
			snap, err := join.Deployments(tx).Snapshot(ctx, scope, "dep_t101")
			return snap, err, tx
		}
		snap, err, tx := read(liveDeployment(scope, "dep_t101"))
		if err != nil || snap.ID != "dep_t101" || snap.Scope != scope || snap.Candidate != strings.Repeat("a", 40) || len(tx.queries) != 1 || !strings.Contains(tx.queries[0], "tenant_id = $1 AND project_id = $2") {
			t.Fatalf("a live deployment: %+v %v (%d reads)", snap, err, len(tx.queries))
		}
		cleaned := liveDeployment(scope, "dep_t101")
		at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
		cleaned.values[5] = &at
		if _, err, _ := read(cleaned); !errors.Is(err, gates.ErrNotFound) {
			t.Fatalf("a deployment whose cleanup was requested: %v", err)
		}
		foreign := liveDeployment(domain.Scope{TenantID: scope.TenantID, ProjectID: "p_other"}, "dep_t101")
		if _, err, _ := read(foreign); !errors.Is(err, gates.ErrNotFound) {
			t.Fatalf("a deployment of another project: %v", err)
		}
		if _, err, _ := read(depRow{err: pgx.ErrNoRows}); !errors.Is(err, gates.ErrNotFound) {
			t.Fatalf("an absent deployment: %v", err)
		}
		if _, err, _ := read(depRow{err: errors.New("connection reset")}); !errors.Is(err, gates.ErrUnavailable) {
			t.Fatalf("an outage must deny the progression: %v", err)
		}
	})

	t.Run("the agent-candidate gate reads the report the wrapper declares", func(t *testing.T) {
		if ConformanceReportOutput != wrapperspec.ConformanceOutput {
			t.Fatalf("the composition names %q, the OpenCode wrapper declares %q", ConformanceReportOutput, wrapperspec.ConformanceOutput)
		}
		if ConformanceReportOutput != gatesmemory.ReportOutput {
			t.Fatalf("the gate module's agent-candidate case declares %q", gatesmemory.ReportOutput)
		}
		reader := gatesmemory.New().Deployments
		if got := deliveryUnavailableM4(fullJoins(reader, nil), true, true, true); contains(got, capCandidateGates) || contains(got, capDeploymentGates) || contains(got, capStepEnvironments) {
			t.Fatalf("a joined composition still lists %v", got)
		}
		got := deliveryUnavailableM4(DeliveryJoins{}, false, true, true)
		for _, want := range []string{capCandidateGates, capDeploymentGates, capStepEnvironments} {
			if !contains(got, want) {
				t.Errorf("the unjoined composition does not list %q: %v", want, got)
			}
		}
		// Each join leaves the others' reports unchanged.
		only := fullJoins(reader, nil)
		only.ConformanceOutput = ""
		if got := deliveryUnavailableM4(only, true, true, true); !contains(got, capCandidateGates) || contains(got, capDeploymentGates) {
			t.Fatalf("without the report name: %v", got)
		}
		// The M1/M2 refusal list itself is unchanged.
		if !contains(deliveryUnavailableM2(true, true, true), capDeploymentGates) || !contains(deliveryUnavailableM2(true, true, true), capCandidateGates) {
			t.Fatal("the earlier refusal list changed")
		}
	})

	t.Run("the secret route is mounted on the runtime API and not without it", func(t *testing.T) {
		var route recordingSecrets
		f := fxBuild(t, Test, func(f *fixture) { f.in.Secrets = &route })
		slice, err := AssembleExecution(f.in)
		if err != nil {
			t.Fatal(err)
		}
		f.slice = slice
		if !contains(slice.Routes, capSecretResolution) || contains(slice.Disabled, capSecretResolution) {
			t.Fatalf("routes %v, disabled %v", slice.Routes, slice.Disabled)
		}
		if got := DisabledCapabilities(slice, nil); contains(got, capSecretResolution) {
			t.Fatalf("a mounted route is reported disabled: %v", got)
		}
		const path = "/runtime/v1/attempts/att_secret1/secrets/resolve"
		body := runtimev1.SecretResolveRequest{Ref: "secret://project/db", Version: 3}

		rec := f.do(http.MethodPost, path, "runtime-token", body)
		if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" || !strings.Contains(rec.Body.String(), `"valueBase64"`) {
			t.Fatalf("a resolution: %d %v %s", rec.Code, rec.Header(), rec.Body.String())
		}
		got := route.last()
		if got.attempt != "att_secret1" || got.bearer != "runtime-token" || got.req != body || !strings.HasPrefix(got.requestID, "req_") {
			t.Fatalf("the route saw %+v", got)
		}

		// Transport: no credential, a malformed or unknown request, another media
		// type and the wrong method never reach the route.
		before := route.count()
		for name, rec := range map[string]*httptest.ResponseRecorder{
			"no bearer":        f.do(http.MethodPost, path, "", body),
			"an unknown field": f.doRaw(http.MethodPost, path, "runtime-token", "application/json", `{"ref":"secret://project/db","version":3,"extra":true}`),
			"a text body":      f.doRaw(http.MethodPost, path, "runtime-token", "text/plain", `{}`),
			"not json":         f.doRaw(http.MethodPost, path, "runtime-token", "application/json", `ref=1`),
			"a bad attempt":    f.do(http.MethodPost, "/runtime/v1/attempts/not%20an%20attempt/secrets/resolve", "runtime-token", body),
			"a GET":            f.do(http.MethodGet, path, "runtime-token", nil),
		} {
			if rec.Code < 400 || rec.Code == http.StatusServiceUnavailable && name != "a GET" {
				t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "valueBase64") {
				t.Errorf("%s: a value was released", name)
			}
		}
		if route.count() != before {
			t.Fatalf("%d refused requests reached the route", route.count()-before)
		}

		// The route's own contract errors keep their code, restricted to the
		// operation's frozen inventory; nothing else leaks.
		for name, tc := range map[string]struct {
			err    error
			status int
			code   string
		}{
			"not declared":  {&runtimev1.ContractError{Code: domain.CodeForbidden, Message: "SECRET-DETAIL"}, http.StatusForbidden, "forbidden"},
			"not found":     {&runtimev1.ContractError{Code: domain.CodeNotFound, Message: "SECRET-DETAIL"}, http.StatusNotFound, "not_found"},
			"revoked":       {&runtimev1.ContractError{Code: runtimev1.CodeAttemptRevoked, Message: "SECRET-DETAIL"}, http.StatusForbidden, "attempt_revoked"},
			"invalid token": {&runtimev1.ContractError{Code: domain.CodeInvalidCredential, Message: "SECRET-DETAIL"}, http.StatusUnauthorized, "invalid_credential"},
			"an outage":     {errors.New("db password=hunter2"), http.StatusServiceUnavailable, "unavailable"},
			"a stray code":  {&runtimev1.ContractError{Code: runtimev1.CodeResultConflict, Message: "SECRET-DETAIL"}, http.StatusServiceUnavailable, "unavailable"},
		} {
			route.fail(tc.err)
			rec := f.do(http.MethodPost, path, "runtime-token", body)
			if rec.Code != tc.status || errorCode(rec.Body.Bytes()) != tc.code || strings.Contains(rec.Body.String(), "SECRET-DETAIL") || strings.Contains(rec.Body.String(), "hunter2") {
				t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
			}
		}

		// Without the route the operation is answered by the contract's not_found
		// envelope and reported disabled.
		bare := newFixture(t, Test)
		if rec := bare.do(http.MethodPost, path, "runtime-token", body); rec.Code != http.StatusNotFound || errorCode(rec.Body.Bytes()) != "not_found" {
			t.Fatalf("an unmounted route answered %d %s", rec.Code, rec.Body.String())
		}
		if contains(bare.slice.Routes, capSecretResolution) || !contains(bare.slice.Disabled, capSecretResolution) {
			t.Fatalf("routes %v, disabled %v", bare.slice.Routes, bare.slice.Disabled)
		}
		names, reasons := DeliveryJoins{}.Disabled(true)
		if !contains(names, capSecretResolution) || reasons[capSecretResolution] == "" {
			t.Fatalf("not reported: %v %v", names, reasons)
		}
		if n, _ := fullJoins(nil, &route).Disabled(true); contains(n, capSecretResolution) {
			t.Fatalf("a mounted route is reported disabled: %v", n)
		}
		if _, err := newSecretHandler(nil, nil); err == nil {
			t.Fatal("a secret handler without a route was built")
		}
	})
}

type secretCall struct {
	attempt   domain.AttemptID
	bearer    string
	req       runtimev1.SecretResolveRequest
	requestID string
}

// recordingSecrets is the secret route behind the handler: it records what it
// is asked and answers a fixed declared secret, or fails.
type recordingSecrets struct {
	mu    sync.Mutex
	calls []secretCall
	err   error
}

func (r *recordingSecrets) DevelopmentOnly() bool { return true }

func (r *recordingSecrets) Resolve(_ context.Context, attempt domain.AttemptID, bearer string, req runtimev1.SecretResolveRequest, requestID string) (runtimev1.SecretResolveResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, secretCall{attempt, bearer, req, requestID})
	if r.err != nil {
		return runtimev1.SecretResolveResponse{}, r.err
	}
	return runtimev1.SecretResolveResponse{Ref: req.Ref, Version: req.Version, Name: "DB", Delivery: runtimev1.DeliveryFile, ValueBase64: "c2VjcmV0"}, nil
}

func (r *recordingSecrets) last() secretCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return secretCall{}
	}
	return r.calls[len(r.calls)-1]
}

func (r *recordingSecrets) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *recordingSecrets) fail(err error) {
	r.mu.Lock()
	r.err = err
	r.mu.Unlock()
}

// doRaw sends a raw body with a content type to the runtime API.
func (f *fixture) doRaw(method, path, bearer, contentType, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", contentType)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	f.slice.Runtime.ServeHTTP(rec, req)
	return rec
}

var _ = json.Marshal

// ---- environment admission through the assembled execution service ----------------

const envStepYAML = `{id: deploy, kind: container, workspace: {access: none}, environment: env_staging, image: "` + pipelinetest.Image + `", command: ["true"], timeoutSeconds: 60}`

// admissionTx is the open admission transaction of the environment join,
// scripted at the SQL boundary: the identity authority reads the actor's
// revision and memberships, and delivery reads the retained snapshot. It
// records every statement. It is not evidence of SQL or locking (T-038/T-067).
type admissionTx struct {
	pgx.Tx
	members  bool
	snapshot depRow
	mu       sync.Mutex
	queries  []string
}

func (t *admissionTx) record(sql string) {
	t.mu.Lock()
	t.queries = append(t.queries, sql)
	t.mu.Unlock()
}

func (t *admissionTx) reads(table string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, q := range t.queries {
		if strings.Contains(q, table) {
			n++
		}
	}
	return n
}

func scanInto(dest []any, values []any) error {
	if len(dest) != len(values) {
		return errors.New("column count mismatch")
	}
	for i, d := range dest {
		reflect.ValueOf(d).Elem().Set(reflect.ValueOf(values[i]))
	}
	return nil
}

type valuesRow struct {
	values []any
	err    error
}

func (r valuesRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	return scanInto(dest, r.values)
}

func (t *admissionTx) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	t.record(sql)
	switch {
	case strings.Contains(sql, "FROM environment_snapshots"):
		return valuesRow{values: t.snapshot.values, err: t.snapshot.err}
	}
	return valuesRow{err: pgx.ErrNoRows} // no authorization revision row: none to lock
}

type memberRows struct {
	pgx.Rows
	rows [][]any
	i    int
}

func (r *memberRows) Next() bool { r.i++; return r.i <= len(r.rows) }
func (r *memberRows) Scan(dest ...any) error {
	return scanInto(dest, r.rows[r.i-1])
}
func (r *memberRows) Close()     {}
func (r *memberRows) Err() error { return nil }

func (t *admissionTx) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	t.record(sql)
	rows := &memberRows{}
	if t.members {
		rows.rows = [][]any{{"mb_engineer", executiontest.Engineer, (*string)(nil), string(identity.RoleEngineer), int64(1), time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)}}
	}
	return rows, nil
}

// stagingSnapshot is the environment's retained configuration, as delivery
// pinned it.
func stagingSnapshot(t *testing.T) delivery.ConfigurationSnapshot {
	t.Helper()
	const kube, token = "secret://project/kubeconfig", "secret://project/token"
	env, err := delivery.NewEnvironment(delivery.EnvironmentRecord{
		Scope: fxScope, ID: "env_staging", Name: "staging", Kind: delivery.KindStage, Target: "kind", Namespace: "staging",
		Kubeconfig: delivery.Credential{Name: "KUBECONFIG", Ref: kube, Delivery: runtimev1.DeliveryFile},
		Secrets:    []delivery.Credential{{Name: "TOKEN", Ref: token, Delivery: runtimev1.DeliveryEnv}},
		Config:     map[string]string{"HOST": "staging.example.test"},
	}, dvNow)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := delivery.NewSnapshot(env, map[string]int64{kube: 1, token: 1}, dvNow)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func snapshotValues(t *testing.T, s delivery.ConfigurationSnapshot) depRow {
	t.Helper()
	config, err := json.Marshal(s.Config)
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := json.Marshal(s.Secrets)
	if err != nil {
		t.Fatal(err)
	}
	return depRow{values: []any{string(s.Scope.TenantID), string(s.Scope.ProjectID), string(s.Environment), string(s.Digest), string(s.Kind),
		s.Target, s.Namespace, config, int64(s.PreviewTTL / time.Second), s.Kubeconfig, secrets, s.CreatedAt}}
}

// snapshotDelivery is delivery's snapshot resolution over one retained snapshot.
type snapshotDelivery struct {
	dvBoundary
	snap delivery.ConfigurationSnapshot
}

func (d snapshotDelivery) ResolveSnapshot(_ context.Context, scope domain.Scope, a delivery.Actor, id domain.EnvironmentID) (delivery.ConfigurationSnapshot, error) {
	if scope != d.snap.Scope || id != d.snap.Environment || a.ID != executiontest.Engineer {
		return delivery.ConfigurationSnapshot{}, delivery.Fail(delivery.ErrNotFound, nil, "not found")
	}
	return d.snap, nil
}

// admittingStore is the admission transaction of the PostgreSQL store over the
// memory one: it binds environments exactly when the join is configured, runs
// execution.BindEnvironments through the join's own binder over the scripted
// transaction, and only then creates the run (as execpg.Store.AdmitInTx
// orders them). AdmitsEnvironments reports what the real store built from the
// same joins reports.
type admittingStore struct {
	execution.Store
	binder execpg.EnvironmentBinder
	tx     pgx.Tx
	admits bool
}

func (s *admittingStore) AdmitsEnvironments() bool { return s.admits }
func (s *admittingStore) DevelopmentOnly() bool    { return true }

func (s *admittingStore) Admit(ctx context.Context, p *execution.AdmissionPlan) (*execution.Run, bool, error) {
	var bound execution.TxEnvironments
	if s.binder != nil {
		bound = s.binder(s.tx)
	}
	if err := execution.BindEnvironments(ctx, bound, p); err != nil {
		return nil, false, err
	}
	return s.Store.Admit(ctx, p)
}

type envWorld struct {
	f     *fixture
	tx    *admissionTx
	snap  delivery.ConfigurationSnapshot
	joins DeliveryJoins
	ports DeliveryPorts
}

// assembleEnvironments composes the execution slice the way NewCore does: the
// store's own statement drives BindDelivery, whose environment resolver is what
// the service admits through.
func assembleEnvironments(t *testing.T, joins DeliveryJoins, members bool) *envWorld {
	t.Helper()
	snap := stagingSnapshot(t)
	w := &envWorld{snap: snap, joins: joins, tx: &admissionTx{members: members, snapshot: snapshotValues(t, snap)}}
	// What the PostgreSQL store reports for these joins.
	admits := executionStore(t, joins).AdmitsEnvironments()
	w.f = fxBuild(t, Test, func(f *fixture) {
		// The credentials the retained snapshot pins exist in the secrets world.
		f.m.World.CreateSecret(fxScope, "kubeconfig")
		f.m.World.CreateSecret(fxScope, "token")
		store := &admittingStore{Store: f.m.Mem, binder: joins.Environments, tx: w.tx, admits: admits}
		ports, err := BindDelivery(DeliveryPortInputs{
			Env: Test, Delivery: snapshotDelivery{dvBoundary{newDVRecords(func() time.Time { return dvNow })}, snap}, Records: newDVStore(),
			Runs: &dvRuns{}, Bindings: &dvBindings{}, AdmitsEnvironments: admitsEnvironments(store),
		})
		if err != nil {
			t.Fatal(err)
		}
		w.ports = ports
		f.in.Plain, f.in.Environments, f.in.Deps.CleanupEnvironments = store, ports.Environments, ports.CleanupEnvironments
	})
	slice, err := AssembleExecution(w.f.in)
	if err != nil {
		t.Fatal(err)
	}
	w.f.slice = slice
	return w
}

func (w *envWorld) runs(t *testing.T) []*execution.Run {
	t.Helper()
	runs, err := w.f.m.Mem.List(context.Background(), fxScope, execution.ListQuery{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

func (w *envWorld) admit(t *testing.T) (*execution.Run, error) {
	t.Helper()
	def := w.f.define(envStepYAML)
	p := w.f.m.NewProject(fxScope)
	return w.f.slice.Trusted.Admit(context.Background(), execution.Caller{PrincipalID: executiontest.Engineer, RequestID: "req_t101"}, execution.AdmitCommand{
		Scope: p.Scope, WorkItemID: p.Item, DefinitionID: def.ID, ContentRevision: w.f.m.ContentRevision(p), SourceCommit: executiontest.CommitA,
	})
}

func checkEnvironmentAdmission(t *testing.T) {
	ctx := context.Background()

	t.Run("joined: the step is admitted and the environment pin is persisted", func(t *testing.T) {
		w := assembleEnvironments(t, fullJoins(nil, nil), true)
		if w.ports.Environments == nil || w.ports.CleanupEnvironments == nil {
			t.Fatalf("a binding store got no environment ports: %+v", w.ports)
		}
		run, err := w.admit(t)
		if err != nil {
			t.Fatalf("an environment-declaring step was not admitted: %v", err)
		}
		agg, err := w.f.m.Mem.Get(ctx, fxScope, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		pin := w.snap.Pin()
		if len(agg.Run.Snapshot.Environments) != 1 || agg.Run.Snapshot.Environments[0].Pin != pin {
			t.Fatalf("the run pinned %+v, want %+v", agg.Run.Snapshot.Environments, pin)
		}
		if st := agg.Step("deploy"); st == nil || st.Spec.Environment != "env_staging" {
			t.Fatalf("the persisted step %+v", st)
		}
		var names []string
		for _, sec := range agg.Run.Snapshot.Secrets {
			names = append(names, sec.Binding.Name)
		}
		slices.Sort(names)
		if !slices.Equal(names, []string{"KUBECONFIG", "TOKEN"}) {
			t.Fatalf("the retained credentials pinned %v", names)
		}
		// The production binder revalidated in the admission transaction: the
		// actor's current bindings and the retained snapshot were both read there.
		if w.tx.reads("FROM memberships") != 1 || w.tx.reads("FROM environment_snapshots") != 1 {
			t.Fatalf("transaction reads: %v", w.tx.queries)
		}
		names, reasons := w.joins.Disabled(admitsEnvironments(w.f.in.Plain))
		if contains(names, capStepEnvironments) || reasons[capStepEnvironments] != "" {
			t.Fatalf("a joined composition reports step environments disabled: %v", names)
		}
	})

	t.Run("revalidation in the admission transaction refuses and persists nothing", func(t *testing.T) {
		for name, tc := range map[string]struct {
			mutate func(*envWorld)
			want   error
		}{
			"the actor's authority is gone": {func(w *envWorld) { w.tx.members = false }, execution.ErrAdmissionRefused},
			"the retained snapshot is gone": {func(w *envWorld) { w.tx.snapshot = depRow{err: pgx.ErrNoRows} }, execution.ErrAdmissionRefused},
			"the snapshot read is down":     {func(w *envWorld) { w.tx.snapshot = depRow{err: errors.New("connection reset")} }, execution.ErrUnavailable},
		} {
			w := assembleEnvironments(t, fullJoins(nil, nil), true)
			tc.mutate(w)
			run, err := w.admit(t)
			if run != nil || !(errors.Is(err, tc.want) || (tc.want == execution.ErrAdmissionRefused && errors.Is(err, execution.ErrDenied))) {
				t.Fatalf("%s: the run was admitted: %v", name, err)
			}
			if n := len(w.runs(t)); n != 0 {
				t.Fatalf("%s: %d runs persisted by a refused admission", name, n)
			}
		}
	})

	t.Run("not joined: admission is refused, nothing is persisted and the capability is reported disabled", func(t *testing.T) {
		w := assembleEnvironments(t, DeliveryJoins{}, true)
		if w.ports.Environments != nil || w.ports.CleanupEnvironments != nil || w.f.in.Environments != nil {
			t.Fatalf("a store that cannot bind environments got environment ports: %+v", w.ports)
		}
		run, err := w.admit(t)
		if !errors.Is(err, execution.ErrNotConfigured) || run != nil {
			t.Fatalf("an environment-declaring step was admitted without the join: %v", err)
		}
		if n := len(w.runs(t)); n != 0 {
			t.Fatalf("%d runs persisted", n)
		}
		if w.tx.reads("FROM environment_snapshots") != 0 {
			t.Fatal("a snapshot was read without the join")
		}
		names, reasons := w.joins.Disabled(admitsEnvironments(w.f.in.Plain))
		if !contains(names, capStepEnvironments) || reasons[capStepEnvironments] == "" {
			t.Fatalf("not reported disabled: %v %v", names, reasons)
		}
		if got, _ := m2JoinReport(nil, JoinState{Joins: w.joins, AdmitsEnvironments: admitsEnvironments(w.f.in.Plain)}); !contains(got, capStepEnvironments) {
			t.Fatalf("the composition report lacks it: %v", got)
		}
		if !contains(deliveryUnavailableM4(w.joins, admitsEnvironments(w.f.in.Plain), true, true), capStepEnvironments) {
			t.Fatal("the delivery slice does not record it")
		}
		// An ordinary step still admits: only environments fail closed.
		def := w.f.define(`{id: build, kind: container, workspace: {access: none}, image: "` + pipelinetest.Image + `", command: ["true"], timeoutSeconds: 60}`)
		p := w.f.m.NewProject(fxScope)
		if _, err := w.f.slice.Trusted.Admit(ctx, execution.Caller{PrincipalID: executiontest.Engineer, RequestID: "req_t101"}, execution.AdmitCommand{
			Scope: p.Scope, WorkItemID: p.Item, DefinitionID: def.ID, ContentRevision: w.f.m.ContentRevision(p), SourceCommit: executiontest.CommitA,
		}); err != nil {
			t.Fatalf("a step without an environment: %v", err)
		}
	})

	t.Run("a resolver over a store that cannot bind environments is not assembled", func(t *testing.T) {
		w := assembleEnvironments(t, fullJoins(nil, nil), true)
		in := w.f.in
		in.Plain = &admittingStore{Store: w.f.m.Mem, admits: false}
		if _, err := AssembleExecution(in); !errors.Is(err, execution.ErrNotConfigured) {
			t.Fatalf("an unbound store with a resolver: %v", err)
		}
	})
}
