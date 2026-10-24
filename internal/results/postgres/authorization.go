package postgres

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/runtimeauth"
	rtpg "github.com/optimiware/kodama/internal/runtimeauth/postgres"
)

// Namespaces resolves the Kubernetes namespace of a tenant's Jobs. It is trusted
// installation configuration (the executor's), never request data.
type Namespaces func(scope domain.Scope) (string, error)

// ActionPolicy resolves the runtime actions an attempt currently allows from its
// declared workspace access. It narrows, never widens, a grant's ceiling.
type ActionPolicy func(access execution.Access) (runtimeauth.ActionSet, error)

// DefaultActions allows the actions the entrypoint uses: context, token
// refresh, secrets, artifacts and the result; source proposals only for a
// writer. Model, tool and deployment calls are granted on their own
// interfaces, not here.
func DefaultActions(access execution.Access) (runtimeauth.ActionSet, error) {
	return runtimeauth.NewActionSet(runtimeauth.DefaultActions(access == execution.AccessWrite)...)
}

// AttemptAuthority implements runtimeauth/postgres.AttemptAuthority over
// execution's attempt rows: the authoritative, lockable view of an attempt that
// runtime grants are checked against.
type AttemptAuthority struct {
	store     *execpg.Store
	namespace Namespaces
	actions   ActionPolicy
}

var _ rtpg.AttemptAuthority = (*AttemptAuthority)(nil)

// NewAttemptAuthority requires the execution store and the trusted namespace
// resolution; a nil ActionPolicy selects DefaultActions.
func NewAttemptAuthority(store *execpg.Store, namespace Namespaces, actions ActionPolicy) (*AttemptAuthority, error) {
	if store == nil || namespace == nil {
		return nil, errors.New("results/postgres: an execution store and a namespace resolver are required")
	}
	if actions == nil {
		actions = DefaultActions
	}
	return &AttemptAuthority{store: store, namespace: namespace, actions: actions}, nil
}

// ObserveAttempt implements runtimeauth.AttemptObserver without locks.
func (a *AttemptAuthority) ObserveAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (runtimeauth.AttemptState, error) {
	snap, err := a.store.ObserveAttempt(ctx, scope, attempt)
	if err != nil {
		return runtimeauth.AttemptState{}, mapSnapshotError(err)
	}
	return a.state(snap)
}

// LockAttempt re-reads the attempt in tx and locks its run row FOR SHARE, which
// conflicts with execution's cancellation (FOR UPDATE).
func (a *AttemptAuthority) LockAttempt(ctx context.Context, tx pgx.Tx, scope domain.Scope, attempt domain.AttemptID) (runtimeauth.AttemptState, error) {
	snap, err := execpg.LockAttempt(ctx, tx, scope, attempt)
	if err != nil {
		return runtimeauth.AttemptState{}, mapSnapshotError(err)
	}
	return a.state(snap)
}

func mapSnapshotError(err error) error {
	if errors.Is(err, execution.ErrNotFound) {
		return errors.Join(runtimeauth.ErrNotFound, errors.New("no such attempt"))
	}
	return err
}

// state maps execution's authoritative snapshot. The phase preserves what
// really happened: a running attempt whose authority was revoked and a
// cancelled attempt are cancelled; any other terminal attempt is finished.
func (a *AttemptAuthority) state(snap execpg.AttemptSnapshot) (runtimeauth.AttemptState, error) {
	ns, err := a.namespace(snap.Scope)
	if err != nil {
		return runtimeauth.AttemptState{}, errors.Join(runtimeauth.ErrUnavailable, fmt.Errorf("namespace: %w", err))
	}
	digest, err := parseDigest(snap.ContextDigest)
	if err != nil {
		return runtimeauth.AttemptState{}, errors.Join(runtimeauth.ErrUnavailable, err)
	}
	actions, err := a.actions(snap.Access)
	if err != nil {
		return runtimeauth.AttemptState{}, errors.Join(runtimeauth.ErrUnavailable, err)
	}
	phase := runtimeauth.AttemptFinished
	switch {
	case snap.Status == execution.AttemptRunning && snap.Live:
		phase = runtimeauth.AttemptActive
	case snap.Status == execution.AttemptRunning, snap.Status == execution.AttemptCancelled:
		phase = runtimeauth.AttemptCancelled
	}
	return runtimeauth.AttemptState{
		Scope: snap.Scope, RunID: snap.RunID, AttemptID: snap.AttemptID, Phase: phase, Revision: snap.Revision,
		Job: runtimeauth.ExpectedJob{Namespace: ns, Name: snap.WorkloadName}, ContextDigest: digest, Actions: actions,
	}, nil
}

func parseDigest(s string) (runtimeauth.Digest, error) {
	raw, err := hex.DecodeString(strings.TrimPrefix(s, "sha256:"))
	if err != nil {
		return runtimeauth.Digest{}, errors.New("the attempt's context digest is malformed")
	}
	return runtimeauth.DigestFromBytes(raw)
}
