package adapters

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/runtimeauth"
	rtpg "github.com/optimiware/kodama/internal/runtimeauth/postgres"
)

// Authority implements runtimeauth/postgres.AttemptAuthority over execution's
// store and the same ExecutionPolicy the executor uses, so the namespace and the
// action ceiling that runtime admission trusts are exactly the ones the Job was
// built for. ObserveAttempt is advisory and takes no lock; LockAttempt runs in the
// caller's tenant transaction through execution's own cancel-serialized read and
// performs no Kubernetes call and no write.
type Authority struct {
	store  *execpg.Store
	policy kube.ExecutionPolicy
}

var _ rtpg.AttemptAuthority = (*Authority)(nil)

// NewAuthority requires the store and a valid policy.
func NewAuthority(store *execpg.Store, policy kube.ExecutionPolicy) (*Authority, error) {
	if store == nil || !policy.Valid() {
		return nil, errors.New("kube/adapters: an execution store and a valid policy are required")
	}
	return &Authority{store: store, policy: policy}, nil
}

// ObserveAttempt implements runtimeauth.AttemptObserver.
func (a *Authority) ObserveAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (runtimeauth.AttemptState, error) {
	snap, err := a.store.ObserveAttempt(ctx, scope, attempt)
	if err != nil {
		return runtimeauth.AttemptState{}, mapErr(err)
	}
	return a.state(snap)
}

// LockAttempt implements runtimeauth/postgres.AttemptAuthority.
func (a *Authority) LockAttempt(ctx context.Context, tx pgx.Tx, scope domain.Scope, attempt domain.AttemptID) (runtimeauth.AttemptState, error) {
	snap, err := execpg.LockAttempt(ctx, tx, scope, attempt)
	if err != nil {
		return runtimeauth.AttemptState{}, mapErr(err)
	}
	return a.state(snap)
}

func mapErr(err error) error {
	if errors.Is(err, execution.ErrNotFound) {
		return runtimeauth.ErrNotFound
	}
	return err
}

// state maps the authoritative snapshot. An attempt that is not live has no
// authority; policy that cannot resolve the tenant fails closed.
func (a *Authority) state(s execpg.AttemptSnapshot) (runtimeauth.AttemptState, error) {
	tgt, err := a.policy.Tenant(s.Scope.TenantID)
	if err != nil {
		return runtimeauth.AttemptState{}, errors.Join(runtimeauth.ErrUnavailable, err)
	}
	actions, err := a.policy.Actions(s.Access)
	if err != nil {
		return runtimeauth.AttemptState{}, errors.Join(runtimeauth.ErrUnavailable, err)
	}
	digest, err := parseDigest(s.ContextDigest)
	if err != nil {
		return runtimeauth.AttemptState{}, errors.Join(runtimeauth.ErrUnavailable, err)
	}
	phase := runtimeauth.AttemptActive
	switch {
	case s.Live:
	case s.Status == execution.AttemptSucceeded || s.Status == execution.AttemptFailed:
		phase = runtimeauth.AttemptFinished
	default:
		phase = runtimeauth.AttemptCancelled
	}
	return runtimeauth.AttemptState{
		Scope: s.Scope, RunID: s.RunID, AttemptID: s.AttemptID, Phase: phase, Revision: s.Revision,
		Job:           runtimeauth.ExpectedJob{Namespace: tgt.Namespace, Name: s.WorkloadName},
		ContextDigest: digest, Actions: actions,
	}, nil
}

func parseDigest(s string) (runtimeauth.Digest, error) {
	raw, ok := strings.CutPrefix(s, "sha256:")
	if !ok {
		return runtimeauth.Digest{}, errors.New("context digest is not sha256")
	}
	b, err := hex.DecodeString(raw)
	if err != nil {
		return runtimeauth.Digest{}, err
	}
	return runtimeauth.DigestFromBytes(b)
}
