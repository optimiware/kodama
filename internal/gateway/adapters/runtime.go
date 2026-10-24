package adapters

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/gateway"
	gatewaypg "github.com/optimiware/kodama/internal/gateway/postgres"
	"github.com/optimiware/kodama/internal/runtimeauth"
	runtimepg "github.com/optimiware/kodama/internal/runtimeauth/postgres"
)

func runtimeError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, runtimeauth.ErrInvalidCredential), errors.Is(err, runtimeauth.ErrNotFound), errors.Is(err, runtimeauth.ErrInvalidRequest):
		return gateway.FailWith(gateway.ErrUnauthenticated, err, "the workload credential is not valid")
	case errors.Is(err, runtimeauth.ErrForbidden), errors.Is(err, runtimeauth.ErrAttemptRevoked):
		return gateway.FailWith(gateway.ErrForbidden, err, "the attempt may not use models")
	}
	return gateway.FailWith(gateway.ErrUnavailable, err, "the workload authority could not answer")
}

// Verifier is the part of *runtimeauth.Service the gateway uses.
type Verifier interface {
	Authorize(ctx context.Context, req runtimeauth.AuthorizeRequest) (runtimeauth.Authorization, error)
}

var _ Verifier = (*runtimeauth.Service)(nil)

// RuntimeWorkloads implements gateway.Workloads: it verifies the bearer for the
// route's attempt and the model.invoke action and derives the identity from the
// durable grant, never from the request.
type RuntimeWorkloads struct{ v Verifier }

var _ gateway.Workloads = (*RuntimeWorkloads)(nil)

// NewRuntimeWorkloads requires the runtime authority.
func NewRuntimeWorkloads(v Verifier) (*RuntimeWorkloads, error) {
	if v == nil {
		return nil, errors.New("gateway/adapters: a runtime authority is required")
	}
	return &RuntimeWorkloads{v: v}, nil
}

// Authorize implements gateway.Workloads.
func (r *RuntimeWorkloads) Authorize(ctx context.Context, attempt domain.AttemptID, bearer string) (gateway.Workload, error) {
	a, err := r.v.Authorize(ctx, runtimeauth.AuthorizeRequest{AttemptID: attempt, Bearer: bearer, Action: runtimeauth.ActionModelInvoke})
	if err != nil {
		return gateway.Workload{}, runtimeError(err)
	}
	w := gateway.Workload{Scope: a.Scope, RunID: a.RunID, AttemptID: a.AttemptID, Session: string(a.Session)}
	if a.AttemptID != attempt || w.Validate() != nil {
		return gateway.Workload{}, gateway.Failf(gateway.ErrUnauthenticated, "the workload credential is not valid")
	}
	return w, nil
}

// TxAuthority implements gatewaypg.WorkloadAuthority: inside the gateway's own
// transaction it re-runs runtimeauth.CheckSession through runtimeauth/postgres
// Bind, so the execution owner's attempt lock, the grant lock and every check
// (session, route attempt, signing generation, attempt phase and authority
// revision, allowed actions, grant ceiling, session lifetime) hold for the
// duration of the write. A check made earlier by the route's middleware is not
// a permit.
type TxAuthority struct {
	attempts   runtimepg.AttemptAuthority
	generation func() int64
}

var _ gatewaypg.WorkloadAuthority = (*TxAuthority)(nil)

// NewTxAuthority needs the execution owner's attempt authority and the current
// signing generation (jwt.KeyRing.Generation).
func NewTxAuthority(attempts runtimepg.AttemptAuthority, generation func() int64) (*TxAuthority, error) {
	if attempts == nil || generation == nil {
		return nil, errors.New("gateway/adapters: an attempt authority and a signing generation source are required")
	}
	return &TxAuthority{attempts: attempts, generation: generation}, nil
}

// Check implements gatewaypg.WorkloadAuthority.
func (t *TxAuthority) Check(ctx context.Context, tx pgx.Tx, w gateway.Workload, now time.Time) error {
	if err := w.Validate(); err != nil {
		return err
	}
	bound := runtimepg.Bind(tx, w.Scope.TenantID, t.attempts)
	session := runtimeauth.SessionID(w.Session)
	// The signing generation of the session is read from the durable grant; the
	// check still requires it to equal the generation in force now.
	first, err := bound.GrantBySession(ctx, session, runtimeauth.LockNone)
	if err != nil {
		return runtimeError(err)
	}
	g, _, err := runtimeauth.CheckSession(ctx, bound, runtimeauth.SessionCheck{
		Route: w.AttemptID, Session: session, TokenGeneration: first.Generation, CurrentGeneration: t.generation(),
		Action: runtimeauth.ActionModelInvoke, Now: now,
	})
	if err != nil {
		return runtimeError(err)
	}
	if g.Scope != w.Scope || g.RunID != w.RunID || g.AttemptID != w.AttemptID {
		return gateway.Failf(gateway.ErrForbidden, "the session belongs to another attempt")
	}
	return nil
}

// ExecutionAttempts implements runtimeauth/postgres.AttemptAuthority over the
// execution module's attempt records: the authoritative state the runtime
// authority decides on. Execution does not expose the Job namespace or a
// per-attempt action set yet, so both are installation inputs here and the
// integration owner replaces them when it does (see the T-032 handoff).
type ExecutionAttempts struct {
	store     *execpg.Store
	namespace string
	actions   runtimeauth.ActionSet
}

var _ runtimepg.AttemptAuthority = (*ExecutionAttempts)(nil)

// NewExecutionAttempts wires the adapter. actions is the set every attempt
// currently allows (the grant's own ceiling still narrows it).
func NewExecutionAttempts(store *execpg.Store, jobNamespace string, actions runtimeauth.ActionSet) (*ExecutionAttempts, error) {
	if store == nil || jobNamespace == "" || actions.IsZero() {
		return nil, errors.New("gateway/adapters: an execution store, the Job namespace and the allowed actions are required")
	}
	return &ExecutionAttempts{store: store, namespace: jobNamespace, actions: actions}, nil
}

func (e *ExecutionAttempts) state(s execpg.AttemptSnapshot) (runtimeauth.AttemptState, error) {
	digest, err := hex.DecodeString(strings.TrimPrefix(s.ContextDigest, "sha256:"))
	if err != nil {
		return runtimeauth.AttemptState{}, errors.Join(runtimeauth.ErrUnavailable, errors.New("the execution context digest is malformed"))
	}
	d, err := runtimeauth.DigestFromBytes(digest)
	if err != nil {
		return runtimeauth.AttemptState{}, errors.Join(runtimeauth.ErrUnavailable, err)
	}
	phase := runtimeauth.AttemptActive
	switch {
	case s.Live:
	case s.Status == execution.AttemptRunning, s.Status == execution.AttemptCancelled:
		phase = runtimeauth.AttemptCancelled
	default:
		phase = runtimeauth.AttemptFinished
	}
	return runtimeauth.AttemptState{
		Scope: s.Scope, RunID: s.RunID, AttemptID: s.AttemptID, Phase: phase, Revision: s.Revision,
		Job: runtimeauth.ExpectedJob{Namespace: e.namespace, Name: s.WorkloadName}, ContextDigest: d, Actions: e.actions,
	}, nil
}

func executionError(err error) error {
	if errors.Is(err, execution.ErrNotFound) {
		return errors.Join(runtimeauth.ErrNotFound, err)
	}
	return err
}

// ObserveAttempt implements runtimeauth.AttemptObserver.
func (e *ExecutionAttempts) ObserveAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (runtimeauth.AttemptState, error) {
	s, err := e.store.ObserveAttempt(ctx, scope, attempt)
	if err != nil {
		return runtimeauth.AttemptState{}, executionError(err)
	}
	return e.state(s)
}

// LockAttempt implements runtimeauth/postgres.AttemptAuthority: it re-reads the
// attempt through execpg.LockAttempt, which locks the run row FOR SHARE so a
// concurrent cancellation (FOR UPDATE) is ordered with the caller's transaction.
func (e *ExecutionAttempts) LockAttempt(ctx context.Context, tx pgx.Tx, scope domain.Scope, attempt domain.AttemptID) (runtimeauth.AttemptState, error) {
	s, err := execpg.LockAttempt(ctx, tx, scope, attempt)
	if err != nil {
		return runtimeauth.AttemptState{}, executionError(err)
	}
	return e.state(s)
}
