// Package chatopstest is the shared fixture of the ChatOps tests and the
// published component evidence for its consumers: one isolated, migrated
// PostgreSQL database carrying the event bus, identity and ChatOps schemas, with
// the real stores wired together, plus an isolated HTTP/TLS receiver and event
// builders for the supported catalog types. The only controlled doubles are the
// receivers and the clock.
//
// It is component evidence, not production wiring: nothing here activates
// ChatOps in the composition root (T-075) or qualifies a real gate target
// (T-046). Resource prefix t041: tenants, projects, users and identifiers carry
// it.
package chatopstest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events/eventstest"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/identity"
	identitypg "github.com/optimiware/kodama/internal/identity/postgres"
	"github.com/optimiware/kodama/internal/integrations/chatops"
	chatopspg "github.com/optimiware/kodama/internal/integrations/chatops/postgres"
	"github.com/optimiware/kodama/internal/integrations/chatops/webhook"
	"github.com/optimiware/kodama/internal/storage"
	pgtest "github.com/optimiware/kodama/test/support/postgres"
)

// Prefix is the task's resource prefix.
const Prefix = "t041"

// Fixed scopes of the tests.
var (
	ScopeA = domain.Scope{TenantID: "t041_acme", ProjectID: "p041_inventory"}
	// ScopeA2 is a second project of ScopeA's tenant.
	ScopeA2 = domain.Scope{TenantID: "t041_acme", ProjectID: "p041_billing"}
	ScopeB  = domain.Scope{TenantID: "t041_globex", ProjectID: "p041_ops"}
)

// Origin is the canonical API origin of the fixture's review links.
const Origin = "https://api.t041.example"

// Contributions is the migration manifest the tests apply: the squashed
// baseline, which already holds the identity tables ChatOps authorizes against
// and the ChatOps tables themselves.
func Contributions() []storage.Contribution {
	return []storage.Contribution{storage.BaseContribution()}
}

// Options select fault injection and policies.
type Options struct {
	// Retry is the delivery retry policy used by Worker; the zero value selects
	// a fast test schedule.
	Retry chatops.RetryPolicy
	// MaxDestinations bounds destinations per project (default 5).
	MaxDestinations int
	// Authorizer replaces the identity authorizer (fault injection).
	Authorizer chatopspg.TxAuthorizer
	// Credentials is the credential source of the service and clients.
	Credentials chatops.CredentialSource
	// WrapDB inserts fault injection into the ChatOps store's transactions.
	WrapDB func(storage.TxBeginner) storage.TxBeginner
	// Events mutates the event store options.
	Events func(*eventspg.Options)
}

// Env is one wired ChatOps module over real PostgreSQL.
type Env struct {
	T       testing.TB
	DB      *pgtest.Database
	Owner   *pgxpool.Pool
	Runtime *pgxpool.Pool
	Clock   *eventstest.Clock

	Events   *eventspg.Store
	Identity *identitypg.Store
	Store    *chatopspg.Store
	Svc      *chatops.Service
	Planner  chatops.Planner
	Links    chatops.LinkBuilder

	opts Options
	// policy is rebuilt by Client; Service approves with a permissive-by-origin
	// policy that accepts the receivers registered through Allow.
	allowed []webhook.PrivateEndpoint
}

// New provisions the database, migrations and services.
func New(t testing.TB, o Options) *Env {
	t.Helper()
	db := pgtest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	e := &Env{T: t, DB: db, Owner: db.MigrationPool(ctx, 4), Runtime: db.RuntimePool(ctx, 32), Clock: &eventstest.Clock{}, opts: o}
	manifest, err := storage.NewManifest(Contributions()...)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if _, err := storage.Migrate(ctx, e.Owner, manifest, storage.MigrateOptions{RuntimeRole: db.RuntimeRole}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if e.Links, err = chatops.NewLinkBuilder(Origin); err != nil {
		t.Fatal(err)
	}
	if e.Planner, err = chatops.NewPlanner(e.Links, e.Clock.Now); err != nil {
		t.Fatal(err)
	}
	binding, err := chatopspg.NewBinding(e.Planner)
	if err != nil {
		t.Fatal(err)
	}
	eopts := eventstest.DefaultOptions()
	eopts.Clock = e.Clock.Now
	eopts.Authorizer = eventstest.Operators{"ops_1"}
	if o.Events != nil {
		o.Events(&eopts)
	}
	if e.Events, err = eventspg.NewStore(e.Runtime, eopts, binding); err != nil {
		t.Fatal(err)
	}
	if e.Identity, err = identitypg.NewStore(e.Runtime, e.Events); err != nil {
		t.Fatal(err)
	}
	e.Store = e.NewStore(o.WrapDB, o.Authorizer)
	e.Svc = e.NewService(o.Credentials)
	return e
}

// NewStore builds another ChatOps store over the runtime pool.
func (e *Env) NewStore(wrap func(storage.TxBeginner) storage.TxBeginner, auth chatopspg.TxAuthorizer) *chatopspg.Store {
	e.T.Helper()
	var db storage.TxBeginner = e.Runtime
	if wrap != nil {
		db = wrap(db)
	}
	if auth == nil {
		auth = chatopspg.IdentityAuthorizer{}
	}
	s, err := chatopspg.NewStore(chatopspg.Config{DB: db, Authorizer: auth, Clock: e.Clock.Now})
	if err != nil {
		e.T.Fatal(err)
	}
	return s
}

// Allow registers an exact private receiver address for both fixture tenants:
// the explicit, scoped installation policy the tests need because every
// receiver listens on loopback.
func (e *Env) Allow(addr netip.AddrPort) {
	for _, tenant := range []domain.TenantID{ScopeA.TenantID, ScopeB.TenantID} {
		e.allowed = append(e.allowed, webhook.PrivateEndpoint{Scope: domain.Scope{TenantID: tenant}, Addr: addr})
	}
}

// Policy builds the destination policy including everything registered with Allow.
func (e *Env) Policy(extra ...webhook.PrivateEndpoint) *webhook.Policy {
	e.T.Helper()
	p, err := webhook.NewPolicy(webhook.PolicyConfig{Ports: portsOf(e.allowed, extra), PrivateEndpoints: append(append([]webhook.PrivateEndpoint(nil), e.allowed...), extra...)})
	if err != nil {
		e.T.Fatal(err)
	}
	return p
}

func portsOf(lists ...[]webhook.PrivateEndpoint) []int {
	seen := map[int]bool{443: true}
	out := []int{443}
	for _, l := range lists {
		for _, e := range l {
			if p := int(e.Addr.Port()); !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// Client builds the real outbound client under Policy, trusting the receivers' certificates.
func (e *Env) Client(rc *Receiver, extra ...webhook.PrivateEndpoint) *webhook.Client {
	e.T.Helper()
	cfg := webhook.Config{Policy: e.Policy(extra...), Credentials: e.opts.Credentials}
	if rc != nil {
		cfg.RootCAs = rc.Pool()
	}
	c, err := webhook.NewClient(cfg)
	if err != nil {
		e.T.Fatal(err)
	}
	return c
}

// NewService builds a Service over the fixture store and the current policy.
func (e *Env) NewService(creds chatops.CredentialSource) *chatops.Service {
	e.T.Helper()
	max := e.opts.MaxDestinations
	if max == 0 {
		max = 5
	}
	svc, err := chatops.NewService(chatops.Config{Store: e.Store, Policy: livePolicy{e}, Credentials: creds, Fences: e.Store,
		Operators: operators{"ops_1": true}, MaxDestinationsPerProject: max, Clock: e.Clock.Now})
	if err != nil {
		e.T.Fatal(err)
	}
	return svc
}

// livePolicy approves through the environment's current policy, so receivers
// registered after construction are accepted.
type livePolicy struct{ e *Env }

func (p livePolicy) Approve(scope domain.Scope, endpoint string) error {
	return p.e.Policy().Approve(scope, endpoint)
}

type operators map[string]bool

func (o operators) AuthorizeOperator(_ context.Context, op chatops.Operator, _ domain.TenantID, _ string) error {
	if o[op.ID] {
		return nil
	}
	return fmt.Errorf("operator %q is not authorized", op.ID)
}

// FastRetry is the short retry schedule of the tests (not production values).
func FastRetry() chatops.RetryPolicy {
	return chatops.RetryPolicy{Initial: 20 * time.Millisecond, Max: 100 * time.Millisecond, MaxAttempts: 4}
}

// WorkerConfig returns a fast worker configuration over the given sender.
func (e *Env) WorkerConfig(sender chatops.Sender) chatops.WorkerConfig {
	retry := e.opts.Retry
	if retry == (chatops.RetryPolicy{}) {
		retry = FastRetry()
	}
	return chatops.WorkerConfig{Work: e.Store, Tenants: e.Events, Sender: sender, Retry: retry, Interval: 20 * time.Millisecond, Batch: 20,
		Lease: 30 * time.Second, AttemptTimeout: 5 * time.Second, Concurrency: 4, Clock: e.Clock.Now}
}

// Worker builds a worker over the sender.
func (e *Env) Worker(sender chatops.Sender) *chatops.Worker {
	e.T.Helper()
	w, err := chatops.NewWorker(e.WorkerConfig(sender))
	if err != nil {
		e.T.Fatal(err)
	}
	return w
}

// ---- seeding -----------------------------------------------------------------

// OwnerExec runs SQL as the migration (owner) role.
func (e *Env) OwnerExec(sql string, args ...any) {
	e.T.Helper()
	if _, err := e.Owner.Exec(context.Background(), sql, args...); err != nil {
		e.T.Fatalf("owner exec: %v\n%s", err, sql)
	}
}

// SeedProject creates a project and its tenant.
func (e *Env) SeedProject(scope domain.Scope) {
	e.T.Helper()
	e.OwnerExec(`INSERT INTO tenants (id, key) VALUES ($1::text, $1::text) ON CONFLICT DO NOTHING`, string(scope.TenantID))
	e.OwnerExec(`INSERT INTO projects (tenant_id, id, key, active_wip_limit) VALUES ($1::text, $2::text, $2::text, 10)`, string(scope.TenantID), string(scope.ProjectID))
}

// User maps a verified principal the way a first login does.
func (e *Env) User(subject string) identity.UserID {
	e.T.Helper()
	id, err := e.Identity.MapPrincipal(context.Background(), identity.Principal{Issuer: "https://idp.t041.invalid", Subject: subject})
	if err != nil {
		e.T.Fatalf("map principal: %v", err)
	}
	return id
}

// Bind grants a role in a project (tenant-wide when project is empty), with its
// authorization revision, as an earlier committed grant would have left them.
func (e *Env) Bind(tenant domain.TenantID, user identity.UserID, project domain.ProjectID, role identity.Role) string {
	e.T.Helper()
	id := "mb_" + RandomHex(e.T, 8)
	var p *string
	if project != "" {
		v := string(project)
		p = &v
	}
	e.OwnerExec(`INSERT INTO memberships (tenant_id, id, user_id, project_id, role) VALUES ($1, $2, $3, $4, $5)`, string(tenant), id, string(user), p, string(role))
	e.Revise(tenant, user)
	return id
}

// Unbind removes a membership (revocation) and advances the revision.
func (e *Env) Unbind(tenant domain.TenantID, bindingID string, user identity.UserID) {
	e.T.Helper()
	e.OwnerExec(`DELETE FROM memberships WHERE tenant_id = $1 AND id = $2`, string(tenant), bindingID)
	e.Revise(tenant, user)
}

// Revise advances the user's authorization revision.
func (e *Env) Revise(tenant domain.TenantID, user identity.UserID) {
	e.T.Helper()
	e.OwnerExec(`INSERT INTO identity_authz_revisions (tenant_id, user_id, revision) VALUES ($1, $2, 1)
		ON CONFLICT (tenant_id, user_id) DO UPDATE SET revision = identity_authz_revisions.revision + 1`, string(tenant), string(user))
}

// Callers holds the standard actors of a scope.
type Callers struct{ Admin, Viewer chatops.Caller }

// Actors seeds an administrator (project:settings) and a viewer (project:read
// only) of the project and returns them as callers.
func (e *Env) Actors(scope domain.Scope, label string) Callers {
	e.T.Helper()
	admin, viewer := e.User("admin-"+label), e.User("viewer-"+label)
	e.Bind(scope.TenantID, admin, scope.ProjectID, identity.RoleAdmin)
	e.Bind(scope.TenantID, viewer, scope.ProjectID, identity.RoleViewer)
	return Callers{Admin: CallerOf(admin), Viewer: CallerOf(viewer)}
}

// CallerOf builds a human caller.
func CallerOf(u identity.UserID) chatops.Caller {
	return chatops.Caller{Actor: chatops.Actor{Type: chatops.ActorUser, ID: string(u)}, RequestID: "req_" + RandomHex(nil, 4)}
}

// RandomHex returns n random bytes in hex.
func RandomHex(t testing.TB, n int) string {
	if t != nil {
		t.Helper()
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// ---- destinations, events, assertions -------------------------------------------

// AllEvents is every forwardable event type.
func AllEvents() []string { return append([]string(nil), chatops.SupportedEvents...) }

// Destination creates an enabled destination through the service as the admin.
func (e *Env) Destination(admin chatops.Caller, scope domain.Scope, name, endpoint string, events ...string) chatops.Destination {
	e.T.Helper()
	if len(events) == 0 {
		events = AllEvents()
	}
	d, err := e.Svc.CreateDestination(context.Background(), admin, scope, chatops.DestinationInput{Name: name, Endpoint: endpoint, Events: events, Enabled: true})
	if err != nil {
		e.T.Fatalf("create destination %s: %v", name, err)
	}
	return d
}

// Append commits one event through the real append path.
func (e *Env) Append(env eventsv1.Envelope) {
	e.T.Helper()
	err := storage.WithTenant(context.Background(), e.Runtime, env.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := e.Events.Append(ctx, tx, env)
		return err
	})
	if err != nil {
		e.T.Fatalf("append %s: %v", env.ID, err)
	}
}

// Count runs a scalar query as the owner role (bypasses RLS by ownership
// policy), for assertions about stored rows.
func (e *Env) Count(sql string, args ...any) int {
	e.T.Helper()
	var n int
	if err := e.Owner.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.T.Fatalf("count: %v\n%s", err, sql)
	}
	return n
}

// CountTenant runs a scalar query as the runtime role inside the tenant's
// context, so tables with tenant-only policies (the outbox) are visible.
func (e *Env) CountTenant(tenant domain.TenantID, sql string, args ...any) int {
	e.T.Helper()
	var n int
	err := storage.WithTenant(context.Background(), e.Runtime, tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(&n)
	})
	if err != nil {
		e.T.Fatalf("count in %s: %v\n%s", tenant, err, sql)
	}
	return n
}

// Deliveries counts the stored deliveries of a destination (every state).
func (e *Env) Deliveries(dest chatops.DestinationID) int {
	return e.Count(`SELECT count(*) FROM chatops_deliveries WHERE destination_id = $1`, string(dest))
}

// DeliveryOf returns the stored delivery of (destination, event) through the
// worker-visible columns, as the owner.
func (e *Env) DeliveryOf(dest chatops.DestinationID, eventID string) chatops.Delivery {
	e.T.Helper()
	var scope domain.Scope
	var id string
	if err := e.Owner.QueryRow(context.Background(), `SELECT tenant_id, project_id, id FROM chatops_deliveries WHERE destination_id = $1 AND event_id = $2`,
		string(dest), eventID).Scan(&scope.TenantID, &scope.ProjectID, &id); err != nil {
		e.T.Fatalf("delivery of %s/%s: %v", dest, eventID, err)
	}
	return e.Delivery(scope, chatops.DeliveryID(id))
}

// Delivery reads a delivery as the owner (no authorization).
func (e *Env) Delivery(scope domain.Scope, id chatops.DeliveryID) chatops.Delivery {
	e.T.Helper()
	var d chatops.Delivery
	var state, class, reason string
	var lease, delivered *time.Time
	err := e.Owner.QueryRow(context.Background(), `SELECT destination_id, event_id, event_type, binding_revision, payload, state, attempts, cycle_attempts, replays,
next_attempt_at, claim_token, lease_expires_at, last_class, last_uncertain, last_status, receipt, delivered_at, failed_reason, version
FROM chatops_deliveries WHERE tenant_id = $1 AND project_id = $2 AND id = $3`, string(scope.TenantID), string(scope.ProjectID), string(id)).
		Scan(&d.DestinationID, &d.EventID, &d.EventType, &d.BindingRevision, &d.Payload, &state, &d.Attempts, &d.CycleAttempts, &d.Replays, &d.NextAttemptAt,
			&d.ClaimToken, &lease, &class, &d.LastUncertain, &d.LastStatus, &d.Receipt, &delivered, &reason, &d.Version)
	if err != nil {
		e.T.Fatalf("read delivery %s: %v", id, err)
	}
	d.Scope, d.ID, d.State, d.LastClass, d.FailedReason = scope, id, chatops.DeliveryState(state), chatops.FailureClass(class), chatops.FailedReason(reason)
	if lease != nil {
		d.LeaseExpiresAt = *lease
	}
	if delivered != nil {
		d.DeliveredAt = *delivered
	}
	return d
}

// Eventually polls cond until it holds or the timeout elapses.
func Eventually(t testing.TB, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	eventstest.Eventually(t, timeout, what, cond)
}

// ---- event builders ----------------------------------------------------------------

var fixedTime = time.Date(2026, 10, 19, 10, 5, 0, 0, time.UTC)

func envelope(scope domain.Scope, id, eventType string, res eventsv1.Resource, actor eventsv1.Actor, data any) eventsv1.Envelope {
	raw, err := json.Marshal(data)
	if err != nil {
		panic(err)
	}
	return eventsv1.Envelope{ID: id, SchemaVersion: eventsv1.SchemaVersion, Type: eventType, OccurredAt: fixedTime, TenantID: scope.TenantID, ProjectID: scope.ProjectID,
		Resource: res, Actor: actor, CorrelationID: "req_" + id, Data: raw}
}

var engineer = eventsv1.Actor{Type: eventsv1.ActorUser, ID: "usr_engineer"}

// Digest returns a deterministic sha256 digest string.
func Digest(c byte) string { return "sha256:" + strings.Repeat(string(c), 64) }

// WorkItem builds a work_item.transitioned event.
func WorkItem(scope domain.Scope, id, workItem string, version int64) eventsv1.Envelope {
	return envelope(scope, id, eventsv1.TypeWorkItemTransitioned, eventsv1.Resource{Type: "work_item", ID: workItem, Version: version}, engineer,
		eventsv1.WorkItemTransitioned{From: "ready", To: "active", TransitionID: "tr_" + id})
}

// GateRequested builds a gate.requested event (the review fixture of AC-08).
func GateRequested(scope domain.Scope, id, gate string, version int64, subject byte) eventsv1.Envelope {
	return envelope(scope, id, eventsv1.TypeGateRequested, eventsv1.Resource{Type: "gate", ID: gate, Version: version}, engineer,
		eventsv1.GateRequested{SubjectDigest: Digest(subject), ContentRevision: 3, ReviewerScope: "project", Deadline: "2027-01-01T00:00:00Z"})
}

// GateDecided builds a gate.decided event.
func GateDecided(scope domain.Scope, id, gate string, version int64, subject byte) eventsv1.Envelope {
	return envelope(scope, id, eventsv1.TypeGateDecided, eventsv1.Resource{Type: "gate", ID: gate, Version: version}, eventsv1.Actor{Type: eventsv1.ActorUser, ID: "usr_reviewer"},
		eventsv1.GateDecided{DecidedBy: eventsv1.Actor{Type: eventsv1.ActorUser, ID: "usr_reviewer"}, Decision: "approved", State: "approved", SubjectDigest: Digest(subject)})
}

// GateInvalidated builds a gate.invalidated event.
func GateInvalidated(scope domain.Scope, id, gate string, version int64, subject byte) eventsv1.Envelope {
	rev := int64(4)
	return envelope(scope, id, eventsv1.TypeGateInvalidated, eventsv1.Resource{Type: "gate", ID: gate, Version: version}, engineer,
		eventsv1.GateInvalidated{SubjectDigest: Digest(subject), Reason: "content_changed", SupersedingRevision: &rev})
}

// RunState builds a run.state_changed event.
func RunState(scope domain.Scope, id, run string, version int64) eventsv1.Envelope {
	return envelope(scope, id, eventsv1.TypeRunStateChanged, eventsv1.Resource{Type: "run", ID: run, Version: version}, engineer,
		eventsv1.RunStateChanged{From: "running", To: "succeeded", Reason: "completed"})
}

// Deployment builds a deployment.state_changed event.
func Deployment(scope domain.Scope, id, deployment string, version int64) eventsv1.Envelope {
	commit := strings.Repeat("1", 40)
	return envelope(scope, id, eventsv1.TypeDeploymentStateChanged, eventsv1.Resource{Type: "deployment", ID: deployment, Version: version}, engineer,
		eventsv1.DeploymentStateChanged{EnvironmentID: "env_1", From: "running", To: "succeeded", SourceCommit: &commit})
}

// UsageEvent builds a usage.recorded event, which no destination can select.
func UsageEvent(scope domain.Scope, id string) eventsv1.Envelope {
	return envelope(scope, id, eventsv1.TypeUsageRecorded, eventsv1.Resource{Type: "usage_record", ID: "usg_1", Version: 1}, engineer,
		eventsv1.UsageRecorded{RunID: "run_1", AttemptID: "att_1", Model: "m", Basis: "observed", Uncertainty: "none"})
}

// MembershipEvent builds a tenant-wide event (no project).
func MembershipEvent(tenant domain.TenantID, id string) eventsv1.Envelope {
	return eventstest.MembershipChange(string(tenant), id, 1)
}
