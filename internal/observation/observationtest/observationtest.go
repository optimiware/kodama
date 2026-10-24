// Package observationtest is the shared fixture of the observation tests: one
// isolated, migrated PostgreSQL database with the owner and runtime-role pools,
// the real event store, the real identity authority and the work tables, plus
// explicitly test-only persisted run and step-attempt tables behind the
// execution seam, event builders, and an HTTP server wired through the shared
// router. It is imported only by tests. Resource identifiers carry the task
// resource prefix t024. Only PostgreSQL is needed: no NATS, S3 or Git.
package observationtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/identity"
	identityhttp "github.com/optimiware/kodama/internal/identity/http"
	"github.com/optimiware/kodama/internal/identity/idtest"
	"github.com/optimiware/kodama/internal/observation"
	obsauth "github.com/optimiware/kodama/internal/observation/authorization"
	obshttp "github.com/optimiware/kodama/internal/observation/http"
	obspg "github.com/optimiware/kodama/internal/observation/postgres"
	"github.com/optimiware/kodama/internal/storage"
)

// Prefix is this task's resource prefix.
const Prefix = "t024"

// Fixture identifiers.
const (
	TenantA   domain.TenantID  = "t024_acme"
	ProjectA  domain.ProjectID = "p024_inventory"
	ProjectA2 domain.ProjectID = "p024_billing"
	TenantB   domain.TenantID  = "t024_globex"
	ProjectB  domain.ProjectID = "p024_ledger"
)

// TestOptions are short, explicit test bounds (not production values).
func TestOptions() observation.Options {
	return observation.Options{
		PageSize: 5, MaxPageBytes: 1 << 20,
		PollInterval: 10 * time.Millisecond, HeartbeatInterval: 60 * time.Millisecond, AccessInterval: 20 * time.Millisecond,
		MaxSnapshotResources: 200,
	}
}

// Env is one migrated database wired for observation tests.
type Env struct {
	*idtest.Stack
	Runs *RunTables
	Obs  *obspg.Store
}

// Options tune the environment.
type Options struct {
	// Events mutates the event store options (for example retention).
	Events func(*eventspg.Options)
	// Conns is the runtime pool size (idtest's default when zero).
	Conns int32
}

const runDDL = `
CREATE TABLE t024_runs (
    tenant_id  kodama_id NOT NULL REFERENCES tenants (id),
    project_id kodama_id NOT NULL,
    id         kodama_id NOT NULL,
    state      text NOT NULL,
    version    bigint NOT NULL,
    PRIMARY KEY (tenant_id, id),
    FOREIGN KEY (tenant_id, project_id) REFERENCES projects (tenant_id, id)
);
CREATE TABLE t024_step_attempts (
    tenant_id  kodama_id NOT NULL REFERENCES tenants (id),
    project_id kodama_id NOT NULL,
    id         kodama_id NOT NULL,
    run_id     kodama_id NOT NULL,
    step_id    kodama_id NOT NULL,
    state      text NOT NULL,
    version    bigint NOT NULL,
    PRIMARY KEY (tenant_id, id)
);
ALTER TABLE t024_runs ENABLE ROW LEVEL SECURITY; ALTER TABLE t024_runs FORCE ROW LEVEL SECURITY;
ALTER TABLE t024_step_attempts ENABLE ROW LEVEL SECURITY; ALTER TABLE t024_step_attempts FORCE ROW LEVEL SECURITY;
CREATE POLICY owner_all ON t024_runs TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY tenant_isolation ON t024_runs USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
CREATE POLICY owner_all ON t024_step_attempts TO CURRENT_USER USING (true) WITH CHECK (true);
CREATE POLICY tenant_isolation ON t024_step_attempts USING (tenant_id = kodama_tenant_id()) WITH CHECK (tenant_id = kodama_tenant_id());
GRANT SELECT, INSERT, UPDATE ON t024_runs, t024_step_attempts TO "%s";
`

// New provisions the database, the seeded tenants and the observation store.
func New(t testing.TB, o Options) *Env {
	t.Helper()
	s := idtest.New(t, idtest.Options{Events: o.Events, Conns: o.Conns})
	s.OwnerExec(fmt.Sprintf(runDDL, s.DB.RuntimeRole))
	s.SeedProject(TenantA, ProjectA, 50)
	s.SeedProject(TenantA, ProjectA2, 50)
	s.SeedProject(TenantB, ProjectB, 50)
	e := &Env{Stack: s, Runs: &RunTables{}}
	obs, err := obspg.NewStore(s.Runtime, s.Events, obspg.Bindings{Work: obspg.WorkResources{}, Runs: e.Runs})
	if err != nil {
		t.Fatal(err)
	}
	e.Obs = obs
	return e
}

// Authority is the standalone authority over the persisted bindings, selected the
// way an installation selects it.
func (e *Env) Authority() identity.Authority {
	sa, err := identity.NewStandaloneAuthority(e.Stack.Store)
	if err != nil {
		e.T.Fatal(err)
	}
	a, err := identity.SelectAuthority(identity.AuthorityConfig{Profile: identity.ProfileStandalone, Standalone: sa})
	if err != nil {
		e.T.Fatal(err)
	}
	return a
}

// Access is the observation access adapter over Authority.
func (e *Env) Access() observation.Access { return e.AccessOver(e.Authority()) }

// AccessOver adapts any authority.
func (e *Env) AccessOver(a identity.Authority) observation.Access {
	ad, err := obsauth.NewAdapter(a)
	if err != nil {
		e.T.Fatal(err)
	}
	return ad
}

// Service builds the observation service over the real store and authority.
func (e *Env) Service(mutate ...func(*observation.Config)) *observation.Service {
	cfg := observation.Config{Log: e.Obs, State: e.Obs, Access: e.Access(), Options: TestOptions()}
	for _, m := range mutate {
		m(&cfg)
	}
	svc, err := observation.NewService(cfg)
	if err != nil {
		e.T.Fatal(err)
	}
	return svc
}

// Commit runs apply and appends envs in one tenant transaction, as a resource
// owner's use case does, and returns the assigned sequences.
func (e *Env) Commit(tenant domain.TenantID, apply func(ctx context.Context, tx pgx.Tx) error, envs ...eventsv1.Envelope) []int64 {
	e.T.Helper()
	seqs, err := e.TryCommit(tenant, apply, envs...)
	if err != nil {
		e.T.Fatalf("commit: %v", err)
	}
	return seqs
}

// TryCommit is Commit returning the error (a failing apply rolls back everything).
func (e *Env) TryCommit(tenant domain.TenantID, apply func(ctx context.Context, tx pgx.Tx) error, envs ...eventsv1.Envelope) ([]int64, error) {
	var seqs []int64
	err := storage.WithTenant(context.Background(), e.Runtime, tenant, func(ctx context.Context, tx pgx.Tx) error {
		seqs = nil
		if apply != nil {
			if err := apply(ctx, tx); err != nil {
				return err
			}
		}
		for _, env := range envs {
			res, err := e.Events.Append(ctx, tx, env)
			if err != nil {
				return err
			}
			seqs = append(seqs, res.Sequence)
		}
		return nil
	})
	return seqs, err
}

// InsertWork inserts a work item as the runtime role would.
func InsertWork(project domain.ProjectID, id, state string) func(ctx context.Context, tx pgx.Tx) error {
	return func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO work_items (tenant_id, project_id, id, kind, title, state, created_by_type, created_by_id, created_at, updated_at)
			VALUES (kodama_tenant_id(), $1::text, $2::text, 'story', $2::text, $3::text, 'user', 'usr_t024', now(), now())`, string(project), id, state)
		return err
	}
}

// UpdateWork changes a work item's state and bumps its version.
func UpdateWork(project domain.ProjectID, id, state string) func(ctx context.Context, tx pgx.Tx) error {
	return func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE work_items SET state = $3, version = version + 1, updated_at = now()
			WHERE tenant_id = kodama_tenant_id() AND project_id = $1 AND id = $2`, string(project), id, state)
		if err == nil && tag.RowsAffected() != 1 {
			err = errors.New("observationtest: no such work item")
		}
		return err
	}
}

// RunTables is the test-only persisted binding of the execution seam. It is
// not a production run schema: it exists to prove the seam against real rows.
type RunTables struct {
	// Down, when set, makes every read fail like an unavailable binding.
	Down atomic.Pointer[error]
}

var _ obspg.RunBinding = (*RunTables)(nil)

// RunExists implements obspg.RunBinding.
func (r *RunTables) RunExists(ctx context.Context, tx pgx.Tx, scope domain.Scope, run domain.RunID) (bool, error) {
	if err := r.Down.Load(); err != nil {
		return false, *err
	}
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM t024_runs WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
		string(scope.TenantID), string(scope.ProjectID), string(run)).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// ReadResources implements obspg.ResourceBinding.
func (r *RunTables) ReadResources(ctx context.Context, tx pgx.Tx, q obspg.ResourceQuery) ([]observation.Resource, error) {
	if err := r.Down.Load(); err != nil {
		return nil, *err
	}
	var out []observation.Resource
	for _, f := range q.Families {
		var sql string
		switch f {
		case observation.FamilyRun:
			sql = `SELECT id, version, state, '', '' FROM t024_runs WHERE tenant_id = $1 AND project_id = $2 AND ($3 = '' OR id = $3) ORDER BY id LIMIT $4`
		case observation.FamilyStepAttempt:
			sql = `SELECT id, version, state, run_id, step_id FROM t024_step_attempts WHERE tenant_id = $1 AND project_id = $2 AND ($3 = '' OR run_id = $3) ORDER BY id LIMIT $4`
		default:
			continue
		}
		rows, err := tx.Query(ctx, sql, string(q.Scope.Tenant), string(q.Scope.Project), string(q.Scope.Run), q.Limit)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			res := observation.Resource{Type: f}
			if err := rows.Scan(&res.ID, &res.Version, &res.State, &res.RunID, &res.StepID); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, res)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// SeedRun inserts or replaces a run as owner.
func (e *Env) SeedRun(tenant domain.TenantID, project domain.ProjectID, id, state string, version int64) {
	e.T.Helper()
	e.OwnerExec(`INSERT INTO t024_runs (tenant_id, project_id, id, state, version) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id, id) DO UPDATE SET state = $4, version = $5`, string(tenant), string(project), id, state, version)
}

// SeedAttempt inserts or replaces a step attempt as owner.
func (e *Env) SeedAttempt(tenant domain.TenantID, project domain.ProjectID, id, run, step, state string, version int64) {
	e.T.Helper()
	e.OwnerExec(`INSERT INTO t024_step_attempts (tenant_id, project_id, id, run_id, step_id, state, version) VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (tenant_id, id) DO UPDATE SET state = $6, version = $7`, string(tenant), string(project), id, run, step, state, version)
}

// RunState updates a run's state in the caller's transaction.
func RunState(project domain.ProjectID, id, state string, version int64) func(ctx context.Context, tx pgx.Tx) error {
	return func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE t024_runs SET state = $3, version = $4 WHERE tenant_id = kodama_tenant_id() AND project_id = $1 AND id = $2`,
			string(project), id, state, version)
		return err
	}
}

func envelope(id, typ string, tenant domain.TenantID, project domain.ProjectID, res eventsv1.Resource, data any) eventsv1.Envelope {
	raw, err := json.Marshal(data)
	if err != nil {
		panic(err)
	}
	return eventsv1.Envelope{
		ID: id, SchemaVersion: eventsv1.SchemaVersion, Type: typ, OccurredAt: time.Now().UTC().Truncate(time.Microsecond),
		TenantID: tenant, ProjectID: project, Resource: res,
		Actor:         eventsv1.Actor{Type: eventsv1.ActorUser, ID: "usr_t024"},
		CorrelationID: "req_" + id, Data: raw,
	}
}

// WorkEvent builds a valid work_item.transitioned event stamped now.
func WorkEvent(tenant domain.TenantID, project domain.ProjectID, id, workItem string, version int64) eventsv1.Envelope {
	return envelope(id, eventsv1.TypeWorkItemTransitioned, tenant, project,
		eventsv1.Resource{Type: "work_item", ID: workItem, Version: version},
		eventsv1.WorkItemTransitioned{From: "ready", To: "active", TransitionID: "tr_" + id})
}

// WorkCreated builds a valid work_item.created event.
func WorkCreated(tenant domain.TenantID, project domain.ProjectID, id, workItem string) eventsv1.Envelope {
	return envelope(id, eventsv1.TypeWorkItemCreated, tenant, project,
		eventsv1.Resource{Type: "work_item", ID: workItem, Version: 1},
		eventsv1.WorkItemCreated{Kind: "story", State: "backlog"})
}

// RunEvent builds a valid run.state_changed event.
func RunEvent(tenant domain.TenantID, project domain.ProjectID, id, run string, version int64) eventsv1.Envelope {
	return envelope(id, eventsv1.TypeRunStateChanged, tenant, project,
		eventsv1.Resource{Type: "run", ID: run, Version: version},
		eventsv1.RunStateChanged{From: "pending", To: "running", Reason: "started"})
}

// StepEvent builds a valid step_attempt.state_changed event of a run.
func StepEvent(tenant domain.TenantID, project domain.ProjectID, id, attempt, run string, version int64) eventsv1.Envelope {
	return envelope(id, eventsv1.TypeStepAttemptStateChange, tenant, project,
		eventsv1.Resource{Type: "step_attempt", ID: attempt, Version: version},
		eventsv1.StepAttemptStateChanged{RunID: run, StepID: "build", From: "pending", To: "running", Reason: "started"})
}

// GateEvent builds a gate.requested event (not part of the observable projection).
func GateEvent(tenant domain.TenantID, project domain.ProjectID, id string) eventsv1.Envelope {
	return envelope(id, eventsv1.TypeGateRequested, tenant, project,
		eventsv1.Resource{Type: "gate", ID: "gate_1", Version: 1},
		eventsv1.GateRequested{SubjectDigest: "sha256:" + strings.Repeat("a", 64), ContentRevision: 1, ReviewerScope: "project", Deadline: "2027-01-01T00:00:00Z"})
}

// Server is an HTTP server serving the observation routes through the shared
// router with a bearer authenticator that treats the token as the user ID.
type Server struct {
	*httptest.Server
	Codec   *httpapi.CursorCodec
	Handler *obshttp.Handler
}

// bearerAuth maps "Authorization: Bearer <userId>" to that user. It is a test
// stand-in for the OIDC authenticator: the identity module's tests own that.
type bearerAuth struct{}

func (bearerAuth) Authenticate(_ context.Context, r *http.Request) (httpapi.Principal, error) {
	scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || tok == "" {
		return httpapi.Principal{}, httpapi.ErrUnauthenticated
	}
	return httpapi.Principal{ID: tok}, nil
}

// CursorKey is the signing key of the test codec.
var CursorKey = []byte("t024-observation-cursor-key-000000000000")

// HandlerOptions are short test bounds.
func HandlerOptions() obshttp.HandlerOptions {
	return obshttp.HandlerOptions{WriteTimeout: 500 * time.Millisecond, MaxBufferedBytes: 16 << 10}
}

// ServerOption customizes NewServer.
type ServerOption func(*serverConfig)

type serverConfig struct {
	wrapListener func(net.Listener) net.Listener
}

// WithListener wraps the server's listener (for example to shrink socket buffers).
func WithListener(wrap func(net.Listener) net.Listener) ServerOption {
	return func(c *serverConfig) { c.wrapListener = wrap }
}

// NewServer wires the routes. authority is the one the route authorizer asks.
func (e *Env) NewServer(svc *observation.Service, authority identity.Authority, ho obshttp.HandlerOptions, opts ...ServerOption) *Server {
	e.T.Helper()
	var sc serverConfig
	for _, o := range opts {
		o(&sc)
	}
	codec, err := httpapi.NewCursorCodec(CursorKey)
	if err != nil {
		e.T.Fatal(err)
	}
	authz, err := identityhttp.NewAuthorizer(authority, obshttp.Rules())
	if err != nil {
		e.T.Fatal(err)
	}
	rt, err := httpapi.NewRouter(httpapi.RouterConfig{Authenticator: bearerAuth{}, Authorizer: authz, RateLimiter: httpapi.Unlimited(), Prefix: "/api/v1"})
	if err != nil {
		e.T.Fatal(err)
	}
	h, err := obshttp.NewHandler(svc, codec, ho)
	if err != nil {
		e.T.Fatal(err)
	}
	if err := h.RegisterRoutes(rt); err != nil {
		e.T.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(rt)
	if sc.wrapListener != nil {
		srv.Listener = sc.wrapListener(srv.Listener)
	}
	srv.Start()
	e.T.Cleanup(func() { srv.CloseClientConnections(); srv.Close() })
	return &Server{Server: srv, Codec: codec, Handler: h}
}

// ProjectURL is the project events URL.
func ProjectURL(base string, tenant domain.TenantID, project domain.ProjectID) string {
	return fmt.Sprintf("%s/api/v1/tenants/%s/projects/%s/events", base, tenant, project)
}

// RunURL is the run events URL.
func RunURL(base string, tenant domain.TenantID, project domain.ProjectID, run string) string {
	return fmt.Sprintf("%s/api/v1/tenants/%s/projects/%s/runs/%s/events", base, tenant, project, run)
}

// BigWorkEvent builds a valid work_item.updated event carrying the most the
// catalog allows in its changed-field list (about 4 KB), to fill socket buffers.
func BigWorkEvent(tenant domain.TenantID, project domain.ProjectID, id, workItem string, version int64) eventsv1.Envelope {
	fields := make([]string, 64)
	for n := range fields {
		fields[n] = fmt.Sprintf("field_%02d_%s", n, strings.Repeat("x", 50))
	}
	return envelope(id, eventsv1.TypeWorkItemUpdated, tenant, project,
		eventsv1.Resource{Type: "work_item", ID: workItem, Version: version},
		eventsv1.WorkItemUpdated{ChangedFields: fields})
}
