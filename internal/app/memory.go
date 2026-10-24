package app

// The development composition's joins (SD-12, SD-13). Each module owns an
// in-memory adapter of its own port in its `*test` package; this file holds the
// few seams that join two modules and so belong to the composition root: the
// in-process idempotency record, the keyed run and secret writes, the
// transaction-bound peers of the in-memory execution store, the session guard
// of the result path and the observation projection. They carry the same
// semantics as their PostgreSQL counterparts that the outcome of a call is
// visible to the caller (committed together or not at all, replayed, refused),
// and none of the guarantees that need a database: no lock order, no row
// locks, no isolation level and no restart persistence. Every type reports
// DevelopmentOnly, so `production` refuses it at construction (Admit).
//
// Nothing here imports a test package: the composition test injects the memory
// adapters, and the unit gate proves they are the ones the process would serve.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	execmemory "github.com/optimiware/kodama/internal/execution/memory"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/observation"
	rpg "github.com/optimiware/kodama/internal/results/postgres"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/internal/work"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
	"github.com/optimiware/kodama/internal/workspace"
)

// ---- idempotency ------------------------------------------------------------

// memIdempotency is the in-process idempotency record. A key is claimed by one
// request, the business callback runs once and its response is stored under the
// full lookup key (tenant, principal, method, canonical route, key) with the
// request hash beside it. A repeat with the same hash replays the stored
// response after the current authorization check; a repeat with a different
// hash is the stable idempotency conflict. Nothing is stored for a failed
// request: the business effects and the record commit together or not at all.
//
// Two forms share the record format. executeTx joins the record to the
// caller's memdb transaction (the run admission): the mutation, the response
// validation and the record are one Update, so a refused response or a failed
// commit leaves neither. executeUnit serves a store whose own unit of work
// cannot be joined (the secrets memory store opens its Update itself): the
// record is written inside that unit, after the response was validated, and is
// withdrawn when the unit does not commit. The memory kernel reports a failed
// commit only when it discarded everything, so the withdrawal is exact.
type memIdempotency struct {
	mu      sync.Mutex
	records map[string]memRecord
	// unitSlot serializes executeUnit from the first read of a key to the end of
	// the unit's commit or rollback.
	unitSlot chan struct{}
	now      func() time.Time
	ttl      time.Duration
}

type memRecord struct {
	Key     string
	Hash    httpapi.RequestHash
	Resp    httpapi.StoredResponse
	Ref     httpapi.Reference
	Expires time.Time
}

// idempotencyT holds the records of executeTx inside the kernel, so they roll
// back with the business rows of their transaction.
var idempotencyT = memdb.NewTable("app_idempotency", func(r memRecord) string { return r.Key })

func (r memRecord) clone() memRecord {
	r.Resp.Body = bytes.Clone(r.Resp.Body)
	hd := make(map[string]string, len(r.Resp.Header))
	for k, v := range r.Resp.Header {
		hd[k] = v
	}
	r.Resp.Header = hd
	return r
}

func newMemIdempotency(now func() time.Time) *memIdempotency {
	if now == nil {
		now = time.Now
	}
	return &memIdempotency{records: map[string]memRecord{}, unitSlot: make(chan struct{}, 1), now: now, ttl: 24 * time.Hour}
}

func (*memIdempotency) DevelopmentOnly() bool { return true }

func lookupKey(id httpapi.RequestIdentity) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s", id.TenantID, id.Principal.ID, id.Method, id.Route, id.Key)
}

func (m *memIdempotency) check(id httpapi.RequestIdentity, authorizeReplay func(context.Context, httpapi.Reference) error, have bool) error {
	if !have || authorizeReplay == nil {
		return httpapi.InternalServerError(errors.New("app: a replay authorizer and a callback are required"))
	}
	if err := id.Validate(); err != nil {
		return httpapi.InternalServerError(err)
	}
	return nil
}

// finish validates the response of a successful callback and builds its record.
func (m *memIdempotency) finish(id httpapi.RequestIdentity, o httpapi.Outcome) (memRecord, error) {
	if err := o.Response.Validate(httpapi.DefaultMaxStoredBodyBytes); err != nil {
		return memRecord{}, httpapi.InternalServerError(err)
	}
	return memRecord{Key: lookupKey(id), Hash: id.Hash, Resp: o.Response, Ref: o.Target, Expires: m.now().Add(m.ttl)}.clone(), nil
}

// executeTx runs do inside one update of the tenant's kernel and stores the
// record in the same update. The replay is authorized after the update, outside
// it, as the PostgreSQL adapter does.
func (m *memIdempotency) executeTx(ctx context.Context, db *memdb.Store, id httpapi.RequestIdentity, authorizeReplay func(context.Context, httpapi.Reference) error,
	do func(context.Context, memdb.Tx) (httpapi.Outcome, error)) (httpapi.StoredResponse, bool, error) {
	if err := m.check(id, authorizeReplay, do != nil && db != nil); err != nil {
		return httpapi.StoredResponse{}, false, err
	}
	var (
		replay    *memRecord
		out       httpapi.Outcome
		callback  error
		completed bool
	)
	err := db.Update(ctx, id.TenantID, func(tx memdb.Tx) error {
		if rec, ok := idempotencyT.Get(tx, lookupKey(id)); ok && m.now().Before(rec.Expires) {
			if rec.Hash != id.Hash {
				callback = httpapi.IdempotencyConflictError()
				return callback
			}
			r := rec.clone()
			replay, completed = &r, true
			return nil
		}
		o, err := do(ctx, tx)
		if err != nil {
			callback = err
			return err
		}
		rec, err := m.finish(id, o)
		if err != nil {
			callback = err
			return err
		}
		if err := idempotencyT.Put(tx, rec); err != nil {
			return err
		}
		out, completed = o, true
		return nil
	})
	if err != nil {
		if callback != nil && errors.Is(err, callback) {
			return httpapi.StoredResponse{}, false, callback
		}
		if completed {
			// Only the commit failed: nothing was stored, and the client retries
			// with the same key.
			return httpapi.StoredResponse{}, false, httpapi.UnavailableError(time.Second, fmt.Errorf("commit outcome unknown: %w", err))
		}
		return httpapi.StoredResponse{}, false, httpapi.UnavailableError(time.Second, err)
	}
	if replay != nil {
		if err := authorizeReplay(ctx, replay.Ref); err != nil {
			return httpapi.StoredResponse{}, false, err
		}
		return replay.Resp, true, nil
	}
	return out.Response, false, nil
}

// executeUnit runs a unit of work that cannot be joined to a kernel
// transaction. The unit calls record with its response once the business
// writes are made and before it commits; record validates the response and
// stores the record, and an error from record must abort the unit. If the unit
// returns an error, the record is withdrawn.
//
// The whole unit, through its commit or rollback, holds the unit slot. Records
// are read only under that slot, so a record that is not yet backed by a
// committed unit is never visible: a concurrent request with the same key waits
// for the outcome and then either replays a committed response or runs afresh.
// Nobody is ever answered from state that was rolled back.
func (m *memIdempotency) executeUnit(ctx context.Context, id httpapi.RequestIdentity, authorizeReplay func(context.Context, httpapi.Reference) error,
	unit func(ctx context.Context, record func(httpapi.Outcome) error) error) (httpapi.StoredResponse, bool, error) {
	if err := m.check(id, authorizeReplay, unit != nil); err != nil {
		return httpapi.StoredResponse{}, false, err
	}
	select {
	case m.unitSlot <- struct{}{}:
	case <-ctx.Done():
		return httpapi.StoredResponse{}, false, httpapi.UnavailableError(time.Second, ctx.Err())
	}
	released := false
	release := func() {
		if !released {
			released = true
			<-m.unitSlot
		}
	}
	defer release()

	key := lookupKey(id)
	m.mu.Lock()
	rec, found := m.records[key]
	m.mu.Unlock()
	if found && m.now().Before(rec.Expires) {
		release()
		if rec.Hash != id.Hash {
			return httpapi.StoredResponse{}, false, httpapi.IdempotencyConflictError()
		}
		if err := authorizeReplay(ctx, rec.Ref); err != nil {
			return httpapi.StoredResponse{}, false, err
		}
		return rec.clone().Resp, true, nil
	}

	var (
		out      httpapi.Outcome
		recorded bool
	)
	err := unit(ctx, func(o httpapi.Outcome) error {
		rec, err := m.finish(id, o)
		if err != nil {
			return err
		}
		m.mu.Lock()
		m.records[key] = rec
		m.mu.Unlock()
		out, recorded = o, true
		return nil
	})
	if err != nil {
		m.mu.Lock()
		if recorded {
			delete(m.records, key)
		}
		m.mu.Unlock()
		return httpapi.StoredResponse{}, false, err
	}
	return out.Response, false, nil
}

// memRunCreator is the keyed run creation over the in-memory execution store:
// the admission with its history, audit and outbox rows, the response render
// and the idempotency record are one memdb update.
type memRunCreator struct {
	db    *memdb.Store
	store *execmemory.Store
	idem  *memIdempotency
}

var _ interface {
	CreateKeyed(context.Context, httpapi.RequestIdentity, func(context.Context, httpapi.Reference) error,
		*execution.AdmissionPlan, func(*execution.Run) (httpapi.Outcome, error)) (httpapi.StoredResponse, bool, *execution.Run, error)
} = (*memRunCreator)(nil)

func (*memRunCreator) DevelopmentOnly() bool { return true }

func (c *memRunCreator) CreateKeyed(ctx context.Context, id httpapi.RequestIdentity, authorizeReplay func(context.Context, httpapi.Reference) error,
	plan *execution.AdmissionPlan, render func(*execution.Run) (httpapi.Outcome, error)) (httpapi.StoredResponse, bool, *execution.Run, error) {
	var created *execution.Run
	resp, replayed, err := c.idem.executeTx(ctx, c.db, id, authorizeReplay, func(ctx context.Context, tx memdb.Tx) (httpapi.Outcome, error) {
		run, _, err := c.store.AdmitInTx(ctx, tx, plan)
		if err != nil {
			return httpapi.Outcome{}, err
		}
		o, err := render(run)
		if err != nil {
			return httpapi.Outcome{}, err
		}
		created = run
		return o, nil
	})
	if err != nil {
		return httpapi.StoredResponse{}, false, nil, err
	}
	return resp, replayed, created, nil
}

// memSecretsKeyed is the keyed secret and variable write over a secrets.Store.
// The write is one Atomically unit; the response is validated and recorded
// inside it, so a response that cannot be stored aborts the write, and a unit
// that fails to commit withdraws its record.
type memSecretsKeyed struct {
	store interface {
		Atomically(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, w secrets.Writer) error) error
	}
	idem *memIdempotency
}

func (*memSecretsKeyed) DevelopmentOnly() bool { return true }

func (k *memSecretsKeyed) Execute(ctx context.Context, id httpapi.RequestIdentity, authorizeReplay func(context.Context, httpapi.Reference) error,
	do func(context.Context, secrets.Writer) (httpapi.Outcome, error)) (httpapi.StoredResponse, bool, error) {
	return k.idem.executeUnit(ctx, id, authorizeReplay, func(ctx context.Context, record func(httpapi.Outcome) error) error {
		return k.store.Atomically(ctx, id.TenantID, func(ctx context.Context, w secrets.Writer) error {
			o, err := do(ctx, w)
			if err != nil {
				return err
			}
			return record(o)
		})
	})
}

// ---- the transaction-bound peers of the in-memory execution store ----------

// memTxAuthority is the run permission check of a transaction: the identity
// authority bound to the open memdb transaction. Unknown permissions deny.
type memTxAuthority struct{ authority identity.Authority }

var runPermissionMap = map[execution.Permission]identity.Permission{
	execution.PermRunRead:   identity.PermRunRead,
	execution.PermRunManage: identity.PermRunManage,
}

func (a memTxAuthority) Authorize(ctx context.Context, actor execution.Actor, scope domain.Scope, perm execution.Permission) error {
	p, ok := runPermissionMap[perm]
	if !ok {
		return execution.ErrDenied
	}
	err := a.authority.Authorize(ctx, identity.Actor{ID: actor.ID, Type: identity.ActorType(actor.Type)},
		identity.Target{TenantID: scope.TenantID, ProjectID: scope.ProjectID}, p)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, identity.ErrHidden):
		return execution.ErrHidden
	case errors.Is(err, identity.ErrDenied):
		return execution.ErrDenied
	}
	return execution.Unavailablef(err, "the identity authority is unavailable")
}

// MemoryAuthorityBinder binds the identity authority of a transaction to the
// run permission check the execution store takes.
func MemoryAuthorityBinder(in func(memdb.Tx) identity.Authority) execmemory.AuthorityBinder {
	return func(tx memdb.Tx) execution.TxAuthority { return memTxAuthority{authority: in(tx)} }
}

// MemoryWorkLock adapts the work module's in-memory admission lock to the
// execution store's WorkLock, with the same error mapping the PostgreSQL store
// applies.
func MemoryWorkLock(lock func(memdb.Tx, domain.Scope, domain.WorkItemID) (workpg.Admission, error)) execmemory.WorkLock {
	return func(tx memdb.Tx, scope domain.Scope, id domain.WorkItemID) (execution.WorkAdmission, error) {
		adm, err := lock(tx, scope, id)
		switch {
		case err == nil:
			return execution.WorkAdmission{ContentRevision: adm.ContentRevision}, nil
		case errors.Is(err, work.ErrCancelled):
			return execution.WorkAdmission{}, execution.ErrWorkCancelled
		case errors.Is(err, work.ErrNotActionable):
			return execution.WorkAdmission{}, execution.ErrNotActionable
		case errors.Is(err, work.ErrNotFound):
			return execution.WorkAdmission{}, execution.ErrNotFound
		}
		return execution.WorkAdmission{}, err
	}
}

// memSecretPins adapts register and release functions to execution's
// SecretPins, mapping a missing, deleted or revoked version to the refusal the
// PostgreSQL store reports.
type memSecretPins struct {
	register func(memdb.Tx, domain.Scope, secrets.ConsumerRef, []runtimev1.SecretBinding) error
	release  func(memdb.Tx, domain.Scope, secrets.ConsumerRef) (int64, error)
}

// MemorySecretPinsFrom binds a secrets memory store's pin functions.
func MemorySecretPinsFrom(
	register func(memdb.Tx, domain.Scope, secrets.ConsumerRef, []runtimev1.SecretBinding) error,
	release func(memdb.Tx, domain.Scope, secrets.ConsumerRef) (int64, error),
) execmemory.SecretPins {
	return memSecretPins{register: register, release: release}
}

func (p memSecretPins) RegisterPins(tx memdb.Tx, scope domain.Scope, run domain.RunID, bindings []runtimev1.SecretBinding) error {
	err := p.register(tx, scope, secrets.ConsumerRef{Kind: secrets.ConsumerKindRun, ID: string(run)}, bindings)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, secrets.ErrNotFound), errors.Is(err, secrets.ErrDeleted), errors.Is(err, secrets.ErrRevoked):
		return execution.Refusedf("a pinned secret is no longer usable")
	}
	return err
}

func (p memSecretPins) ReleasePins(tx memdb.Tx, scope domain.Scope, run domain.RunID) error {
	_, err := p.release(tx, scope, secrets.ConsumerRef{Kind: secrets.ConsumerKindRun, ID: string(run)})
	return err
}

// MemoryEvents adapts the event outbox append of the memory event bus.
func MemoryEvents(appendFn func(memdb.Tx, eventsv1.Envelope) error) execmemory.EventAppender {
	return eventAppend(appendFn)
}

type eventAppend func(memdb.Tx, eventsv1.Envelope) error

func (f eventAppend) Append(tx memdb.Tx, env eventsv1.Envelope) error { return f(tx, env) }

// WorkspaceTxStore is the workspace store's transaction-joining entry point in
// memory (workspacetest.MemStore.UpdateInTx).
type WorkspaceTxStore interface {
	UpdateInTx(ctx context.Context, tx memdb.Tx, scope domain.Scope, id workspace.ID, load workspace.Load, fn workspace.Mutation) (*workspace.Aggregate, error)
}

// memWorkspaces joins the execution store to the workspace module's own
// entities (the writer slot and the retained-source intent) through the caller's
// memdb transaction, as execution/postgres does through pgx.Tx.
type memWorkspaces struct {
	store  WorkspaceTxStore
	prefix string
	now    func() time.Time
}

// MemoryWorkspaces binds the workspace memory store to the execution store.
func MemoryWorkspaces(store WorkspaceTxStore, now func() time.Time) (execmemory.Workspaces, error) {
	if store == nil {
		return nil, errors.New("app: a workspace store is required")
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &memWorkspaces{store: store, prefix: workspace.DefaultRetainedRefPrefix, now: now}, nil
}

func (*memWorkspaces) DevelopmentOnly() bool { return true }

var executionActor = workspace.Actor{Type: workspace.ActorService, ID: "execution"}

func (w *memWorkspaces) ReserveWriter(tx memdb.Tx, at *execution.Attempt) error {
	in := workspace.ReserveWriterInput{
		Scope: at.Scope, WorkspaceID: workspace.ID(at.WorkspaceID), AttemptID: at.ID,
		InputCommit: workspace.Commit(at.InputCommit), ExpectedTip: workspace.Commit(at.ExpectedTip), Actor: executionActor,
	}
	_, err := w.store.UpdateInTx(context.Background(), tx, at.Scope, in.WorkspaceID, workspace.Load{}, workspace.ReserveWriterMutation(in, w.now))
	switch {
	case err == nil:
		return nil
	case errors.Is(err, workspace.ErrWriterBusy), errors.Is(err, workspace.ErrFenced), errors.Is(err, workspace.ErrAdmissionClosed):
		return fmt.Errorf("%w: the workspace writer slot is not available", execution.ErrBusy)
	case errors.Is(err, workspace.ErrStale):
		return fmt.Errorf("%w: the workspace tip moved", execution.ErrStale)
	case errors.Is(err, workspace.ErrNotFound):
		return execution.ErrNotFound
	}
	return err
}

func (w *memWorkspaces) SettleWriter(tx memdb.Tx, at *execution.Attempt, outcome execution.WriterOutcome, source string) error {
	in := workspace.SettleWriterInput{
		Scope: at.Scope, WorkspaceID: workspace.ID(at.WorkspaceID), AttemptID: at.ID,
		Outcome: workspace.WriterOutcome(outcome), SourceCommit: workspace.Commit(source), Actor: executionActor,
	}
	_, err := w.store.UpdateInTx(context.Background(), tx, at.Scope, in.WorkspaceID, workspace.Load{Receipt: at.ID}, workspace.SettleWriterMutation(in, w.now))
	switch {
	case err == nil, errors.Is(err, workspace.ErrNotWriter):
		return nil
	case errors.Is(err, workspace.ErrPublicationPending):
		return fmt.Errorf("%w: the writer's publication is unsettled", execution.ErrBusy)
	case errors.Is(err, workspace.ErrOutputNotSelectable):
		return fmt.Errorf("%w: the writer's output is not published", execution.ErrStale)
	}
	return err
}

func (w *memWorkspaces) RetainSource(tx memdb.Tx, run *execution.Run, pinID string) error {
	commit := workspace.Commit(run.SourceCommit)
	_, err := w.store.UpdateInTx(context.Background(), tx, run.Scope, workspace.ID(run.WorkspaceID), workspace.Load{Pins: true},
		func(a *workspace.Aggregate) (workspace.Effects, error) {
			if a.Workspace.State == workspace.StateProvisioning {
				return workspace.Effects{}, fmt.Errorf("%w: the workspace is provisioning", workspace.ErrInvalidTransition)
			}
			if err := workspace.CheckPinAdmission(commit, a.Pins); err != nil {
				return workspace.Effects{}, err
			}
			pin, err := workspace.NewPin(workspace.PinID(pinID), a.Workspace, commit, workspace.PinReader, run.ID, "", w.prefix, w.now())
			if err != nil {
				return workspace.Effects{}, err
			}
			return workspace.Effects{Pins: []*workspace.RetainedRef{pin}, Audit: []workspace.Audit{{
				Action: "workspace.retained_ref.requested", Actor: executionActor,
				Metadata: map[string]any{"pinId": pinID, "commit": run.SourceCommit, "ref": pin.RefName, "purpose": string(workspace.PinReader), "runId": string(run.ID)},
			}}}, nil
		})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, workspace.ErrNotFound):
		return fmt.Errorf("%w: the workspace was not found", execution.ErrNotFound)
	case errors.Is(err, workspace.ErrInvalidTransition), errors.Is(err, workspace.ErrRetentionBusy), errors.Is(err, workspace.ErrAdmissionClosed):
		return execution.Refusedf("the workspace cannot retain the source right now")
	}
	return err
}

// ---- workspace seams over the in-memory execution store ---------------------

// memAttempts is workspace.Attempts over the execution store's scoped reads: the
// workspace and access of the attempt and whether it is still live.
type memAttempts struct{ runs needleRuns }

// MemoryAttempts binds the workspace module's attempt authority to a run reader.
func MemoryAttempts(runs needleRuns) workspace.Attempts { return memAttempts{runs: runs} }

func (memAttempts) DevelopmentOnly() bool { return true }

func (a memAttempts) Authority(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (workspace.WriterAuthority, error) {
	run, err := a.runs.RunOf(ctx, scope, attempt)
	if err != nil {
		return workspace.WriterAuthority{}, workspace.ErrNotFound
	}
	agg, err := a.runs.Get(ctx, scope, run)
	if err != nil {
		return workspace.WriterAuthority{}, workspace.ErrNotFound
	}
	at := agg.Attempt(attempt)
	if at == nil {
		return workspace.WriterAuthority{}, workspace.ErrNotFound
	}
	live := at.Status == execution.AttemptRunning
	if c := agg.Control(attempt); c != nil {
		live = live && c.Live()
	}
	return workspace.WriterAuthority{WorkspaceID: workspace.ID(at.WorkspaceID), Access: workspace.Access(at.Access), Live: live}, nil
}

// memRepositories is the project's repository configuration for the workspace
// module in development: one repository per project, set by the composition
// (the PostgreSQL composition reads it from the project settings).
type memRepositories struct {
	mu    sync.Mutex
	repos map[domain.Scope]workspace.RepositoryConfig
}

func newMemRepositories() *memRepositories {
	return &memRepositories{repos: map[domain.Scope]workspace.RepositoryConfig{}}
}

func (*memRepositories) DevelopmentOnly() bool { return true }

// Set configures the repository of a project.
func (r *memRepositories) Set(scope domain.Scope, cfg workspace.RepositoryConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.repos[scope] = cfg
}

func (r *memRepositories) Repository(_ context.Context, scope domain.Scope) (workspace.RepositoryConfig, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cfg, ok := r.repos[scope]
	if !ok {
		return workspace.RepositoryConfig{}, fmt.Errorf("%w: no repository is configured for the project", workspace.ErrNotFound)
	}
	return cfg, nil
}

// ---- the session guard of the result path -----------------------------------

// RunTxStore is the execution store's transaction-joining update
// (execmemory.Store.UpdateInTx).
type RunTxStore interface {
	UpdateInTx(ctx context.Context, tx memdb.Tx, scope domain.Scope, id domain.RunID, opts execution.UpdateOptions, fn execution.Mutation) (*execution.Aggregate, any, error)
}

// memGuardedStore is the execution store of the runtime result path in memory.
// A mutation made on behalf of a runtime attempt must carry its verified
// session, and the session is checked in the same memdb transaction as the
// mutation, inside the run's update, as results/postgres.ExecutionStore does
// inside the run's database transaction: a revocation commits entirely before
// or entirely after the mutation, never between the check and the write. The
// grant the check returns must belong to the scope and the run being mutated,
// so a valid session of one run authorizes nothing on another run or project,
// including another run of the same tenant. A mutation without a session is
// refused.
type memGuardedStore struct {
	execution.Store
	db         *memdb.Store
	inner      RunTxStore
	bind       func(memdb.Tx) runtimeauth.GrantTx
	generation func() int64
	now        func() time.Time
}

// NewMemoryGuardedStore guards the execution store of the result path. bind
// opens the runtime-authority view of a transaction (rtest.BindMem), generation
// reports the signing generation in force.
func NewMemoryGuardedStore(store execution.Store, db *memdb.Store, inner RunTxStore, bind func(memdb.Tx) runtimeauth.GrantTx, generation func() int64) (execution.Store, error) {
	if store == nil || db == nil || inner == nil || bind == nil || generation == nil {
		return nil, errors.New("app: a store, its kernel, a transaction-joined update, a grant binding and a signing generation are required")
	}
	return &memGuardedStore{Store: store, db: db, inner: inner, bind: bind, generation: generation, now: time.Now}, nil
}

func (*memGuardedStore) DevelopmentOnly() bool { return true }

func (s *memGuardedStore) Update(ctx context.Context, scope domain.Scope, id domain.RunID, opts execution.UpdateOptions, fn execution.Mutation) (*execution.Aggregate, any, error) {
	sess, ok := rpg.SessionFrom(ctx)
	if !ok {
		return nil, nil, errors.Join(runtimeauth.ErrInvalidCredential, errors.New("no runtime session"))
	}
	if sess.Tenant != scope.TenantID {
		return nil, nil, errors.Join(runtimeauth.ErrInvalidCredential, errors.New("session of another tenant"))
	}
	var (
		agg       *execution.Aggregate
		res       any
		completed bool
	)
	err := s.db.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		var err error
		agg, res, err = s.inner.UpdateInTx(ctx, tx, scope, id, opts, func(ctx context.Context, env execution.Env, a *execution.Aggregate) (execution.Effects, error) {
			grant, _, err := runtimeauth.CheckSession(ctx, s.bind(tx), runtimeauth.SessionCheck{
				Route: sess.Route, Session: sess.ID, TokenGeneration: sess.Generation, CurrentGeneration: s.generation(), Action: sess.Action, Now: s.now(),
			})
			if err != nil {
				return execution.Effects{}, err
			}
			if grant.Scope != scope {
				return execution.Effects{}, errors.Join(runtimeauth.ErrInvalidCredential, errors.New("session of another project"))
			}
			if grant.RunID != id {
				return execution.Effects{}, errors.Join(runtimeauth.ErrInvalidCredential, errors.New("session of another run"))
			}
			return fn(ctx, env, a)
		})
		if err != nil {
			return err
		}
		completed = true
		return nil
	})
	if err != nil {
		if completed {
			return nil, nil, execution.Unavailablef(err, "the commit outcome is unknown")
		}
		return nil, nil, err
	}
	return agg, res, nil
}

// ---- the writer guard of work transitions -----------------------------------

// WriterRuns is the part of the execution store the writer guard reads.
type WriterRuns interface {
	List(ctx context.Context, scope domain.Scope, q execution.ListQuery) ([]*execution.Run, error)
	Get(ctx context.Context, scope domain.Scope, id domain.RunID) (*execution.Aggregate, error)
}

// memWriterGuard is work.WriterGuard over the composed execution state, the
// counterpart of execution/postgres.WriterGuard: an item's writer attempts must
// all be settled (terminal, workload gone, effects accounted for) before a
// business transition may leave active. A writer attempt with no controller
// record counts as unsettled. The execution state is read from the published
// kernel state; the work transition holds the kernel's single writer slot, so
// no execution update can interleave with the decision. An unavailable run
// store fails closed.
type memWriterGuard struct{ runs func() WriterRuns }

// NewMemoryWriterGuard binds the guard to the execution store, which is built
// after the work store and therefore supplied late.
func NewMemoryWriterGuard(runs func() WriterRuns) work.WriterGuard { return memWriterGuard{runs: runs} }

func (memWriterGuard) DevelopmentOnly() bool { return true }

func (g memWriterGuard) WriterSettled(ctx context.Context, scope domain.Scope, item domain.WorkItemID) (bool, error) {
	var runs WriterRuns
	if g.runs != nil {
		runs = g.runs()
	}
	if runs == nil {
		return false, work.Unavailablef(nil, "the execution state is not available")
	}
	var after *execution.Position
	for {
		page, err := runs.List(ctx, scope, execution.ListQuery{WorkItemID: item, After: after, Limit: 100})
		if err != nil {
			return false, work.Unavailablef(err, "the execution state is not available")
		}
		for _, r := range page {
			agg, err := runs.Get(ctx, scope, r.ID)
			if err != nil {
				return false, work.Unavailablef(err, "the execution state is not available")
			}
			for _, at := range agg.Attempts {
				if at.Writer() && !agg.Control(at.ID).Settled() {
					return false, nil
				}
			}
		}
		if len(page) < 100 {
			return true, nil
		}
		last := page[len(page)-1]
		after = &execution.Position{CreatedAt: last.CreatedAt, ID: last.ID}
	}
}

// ---- observation ------------------------------------------------------------

// MemoryObservationSource is what the observation projection reads: the event
// log of a scope and the current state of the projected resources.
type MemoryObservationSource interface {
	Watermark(ctx context.Context, scope observation.Scope) (observation.Watermark, error)
	Read(ctx context.Context, scope observation.Scope, after int64, limit int) ([]observation.Record, observation.Watermark, error)
	Resources(ctx context.Context, scope observation.Scope, max int) ([]observation.Resource, error)
	Exists(ctx context.Context, scope observation.Scope) (bool, error)
}

// memObservationState is observation.State over a MemoryObservationSource.
type memObservationState struct{ src MemoryObservationSource }

// NewMemoryObservation returns the log and the state of the observation module
// over one in-memory source. The snapshot position and resources are read
// together; the watermark is read first and the state after, so a concurrent
// commit can only make the state newer than the position, which the stream
// replays idempotently (events carry resource versions).
func NewMemoryObservation(src MemoryObservationSource) (observation.Log, observation.State, error) {
	if src == nil {
		return nil, nil, errors.New("app: an observation source is required")
	}
	return memObservationLog{src: src}, memObservationState{src: src}, nil
}

type memObservationLog struct{ src MemoryObservationSource }

func (memObservationLog) DevelopmentOnly() bool { return true }
func (l memObservationLog) Watermark(ctx context.Context, scope observation.Scope) (observation.Watermark, error) {
	return l.src.Watermark(ctx, scope)
}
func (l memObservationLog) Read(ctx context.Context, scope observation.Scope, after int64, limit int) ([]observation.Record, observation.Watermark, error) {
	return l.src.Read(ctx, scope, after, limit)
}

func (memObservationState) DevelopmentOnly() bool { return true }

func (s memObservationState) Verify(ctx context.Context, scope observation.Scope) error {
	ok, err := s.src.Exists(ctx, scope)
	if err != nil {
		return err
	}
	if !ok {
		return observation.ErrNotFound
	}
	return nil
}

func (s memObservationState) Snapshot(ctx context.Context, scope observation.Scope, max int) (observation.Snapshot, error) {
	if err := s.Verify(ctx, scope); err != nil {
		return observation.Snapshot{}, err
	}
	wm, err := s.src.Watermark(ctx, scope)
	if err != nil {
		return observation.Snapshot{}, err
	}
	res, err := s.src.Resources(ctx, scope, max)
	if err != nil {
		return observation.Snapshot{}, err
	}
	if len(res) > max {
		return observation.Snapshot{}, observation.ErrSnapshotTooLarge
	}
	return observation.Snapshot{Position: wm.HighWater, Resources: res}, nil
}

// ---- Git in development -----------------------------------------------------

// memGit presents the development Git (temporary bare repositories addressed by
// file path) behind the repository URL the rest of the slice requires: the
// pinned runtime context carries an https repository URL, so the project's
// repository is configured with one and the adapter maps it to its local
// repository before every Git call. An unmapped URL is refused, never passed
// through, so the development Git cannot be pointed at another path.
type memGit struct {
	reader    workspace.RefReader
	preparer  workspace.SourcePreparer
	publisher workspace.RefPublisher
	paths     map[string]string
}

// NewMemoryGit wraps the three Git ports with the URL → local path mapping.
func NewMemoryGit(reader workspace.RefReader, preparer workspace.SourcePreparer, publisher workspace.RefPublisher, paths map[string]string) (workspace.RefReader, workspace.SourcePreparer, workspace.RefPublisher, error) {
	if reader == nil || preparer == nil || publisher == nil || len(paths) == 0 {
		return nil, nil, nil, errors.New("app: the Git ports and a repository mapping are required")
	}
	cp := make(map[string]string, len(paths))
	for k, v := range paths {
		cp[k] = v
	}
	g := &memGit{reader: reader, preparer: preparer, publisher: publisher, paths: cp}
	return memGitReader{g}, memGitPreparer{g}, memGitPublisher{g}, nil
}

func (g *memGit) local(repo workspace.RepoRef) (workspace.RepoRef, error) {
	path, ok := g.paths[repo.URL]
	if !ok {
		return workspace.RepoRef{}, fmt.Errorf("%w: the repository is not served by the development Git", workspace.ErrNotFound)
	}
	repo.URL = path
	return repo, nil
}

type memGitReader struct{ g *memGit }

func (memGitReader) DevelopmentOnly() bool { return true }

func (r memGitReader) ResolveRef(ctx context.Context, repo workspace.RepoRef, ref string) (workspace.Commit, bool, error) {
	local, err := r.g.local(repo)
	if err != nil {
		return "", false, err
	}
	return r.g.reader.ResolveRef(ctx, local, ref)
}

func (r memGitReader) BranchContains(ctx context.Context, repo workspace.RepoRef, branch string, commit workspace.Commit) (workspace.BranchInfo, error) {
	local, err := r.g.local(repo)
	if err != nil {
		return workspace.BranchInfo{}, err
	}
	return r.g.reader.BranchContains(ctx, local, branch, commit)
}

type memGitPreparer struct{ g *memGit }

func (memGitPreparer) DevelopmentOnly() bool { return true }

func (p memGitPreparer) Prepare(ctx context.Context, req workspace.PrepareRequest) (workspace.Prepared, error) {
	local, err := p.g.local(req.Repo)
	if err != nil {
		return nil, err
	}
	req.Repo = local
	return p.g.preparer.Prepare(ctx, req)
}

type memGitPublisher struct{ g *memGit }

func (memGitPublisher) DevelopmentOnly() bool { return true }

func (p memGitPublisher) Publish(ctx context.Context, repo workspace.RepoRef, req workspace.PublishRequest) error {
	local, err := p.g.local(repo)
	if err != nil {
		return err
	}
	return p.g.publisher.Publish(ctx, local, req)
}

func (p memGitPublisher) CreateRef(ctx context.Context, repo workspace.RepoRef, ref string, commit workspace.Commit, from string) error {
	local, err := p.g.local(repo)
	if err != nil {
		return err
	}
	return p.g.publisher.CreateRef(ctx, local, ref, commit, from)
}

func (p memGitPublisher) DeleteRef(ctx context.Context, repo workspace.RepoRef, ref string, expected workspace.Commit) error {
	local, err := p.g.local(repo)
	if err != nil {
		return err
	}
	return p.g.publisher.DeleteRef(ctx, local, ref, expected)
}
