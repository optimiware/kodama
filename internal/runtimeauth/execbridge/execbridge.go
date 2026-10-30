// Package execbridge is the adapter between the runtime authority's consumer-owned
// ports (runtimeauth, runtimeauth/http) and the execution module, which stays the
// authority for the run_shared model: it turns execution's committed run-resource
// record and spec into the ResourceState the resource subjects are authorized
// against, and it serves the command-begin fence and the preparation receipt
// through execution's use cases with execution's failures classified as the
// runtime authority's.
//
// It holds no state and no policy, and it does not compose anything: the assembly
// (T-116) builds the store-side lock and read around StateOf, which is the pure
// part, and wires Begins and Receipts into runtimehttp.V2Config.
package execbridge

import (
	"context"
	"errors"
	"fmt"
	"time"

	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/runtimeauth"
	runtimehttp "github.com/optimiware/kodama/internal/runtimeauth/http"
)

// StateOf maps the committed record of one resource of an aggregate (read under
// the run's lock by the caller) to the authority state of its workload. The phase
// is ended as soon as anything ends the resource's authority: its own stop intent,
// stop or removal, a failed, cancelled or terminal run, or a passed run deadline.
// The context is built only from the committed spec; no request value enters it.
// A resource with no runtime identity (the workspace and the network) is refused.
func StateOf(a *execution.Aggregate, key string, now time.Time) (runtimeauth.ResourceState, error) {
	if a == nil || a.Run == nil {
		return runtimeauth.ResourceState{}, fmt.Errorf("%w: no run", runtimeauth.ErrNotFound)
	}
	kind, err := runtimeauth.ResourceKindOf(key)
	if err != nil {
		return runtimeauth.ResourceState{}, err
	}
	r := a.Resource(key)
	if r == nil || !r.Kind.HasIdentity() || r.Scope != a.Run.Scope || r.RunID != a.Run.ID {
		return runtimeauth.ResourceState{}, fmt.Errorf("%w: no such resource", runtimeauth.ErrNotFound)
	}
	deadline, err := time.Parse(time.RFC3339, r.Spec.DeadlineAt)
	if err != nil {
		return runtimeauth.ResourceState{}, fmt.Errorf("%w: the committed deadline is unreadable", runtimeauth.ErrUnavailable)
	}
	phase := runtimeauth.ResourceActive
	if r.Stopping() || r.Stopped() || r.Removed() || a.Run.Status.Terminal() || a.Run.Draining() ||
		(a.Run.DeadlineAt != nil && !now.Before(*a.Run.DeadlineAt)) || !now.Before(deadline) {
		phase = runtimeauth.ResourceEnded
	}
	if a.Run.SourceRun() {
		// A source-set run is served on the v3 wire only (ContextV3).
		return stateOfV3(a, r, kind, key, phase, deadline)
	}
	pin := runtimev2.PolicyPin{ID: r.Spec.Policy.ID, Digest: runtimev2.Digest(r.Spec.Policy.Digest)}
	ctx := runtimev2.ResourceContext{
		ContextVersion: runtimev2.ContextVersion, ResourceKind: kind,
		TenantID: r.Scope.TenantID, ProjectID: r.Scope.ProjectID, RunID: r.RunID, ResourceKey: key, Policy: pin,
	}
	at := runtimev2.Timestamp(deadline.UTC().Format(time.RFC3339Nano))
	switch kind {
	case runtimev2.ResourcePreparation:
		p := r.Spec.Preparation
		if p == nil {
			return runtimeauth.ResourceState{}, fmt.Errorf("%w: the committed spec has no preparation", runtimeauth.ErrUnavailable)
		}
		ctx.Preparation = &runtimev2.PreparationContext{
			RepositoryURL: p.RepositoryURL, Commit: runtimev2.GitCommit(p.Commit), MountPath: runtimev2.MountPath,
			TimeoutSeconds: p.TimeoutSecs, MaxBytes: p.MaxBytes, MaxAttempts: p.MaxAttempts, DeadlineAt: at,
		}
	case runtimev2.ResourceService:
		s := r.Spec.Service
		if s == nil {
			return runtimeauth.ResourceState{}, fmt.Errorf("%w: the committed spec has no service", runtimeauth.ErrUnavailable)
		}
		secrets := make([]runtimev2.SecretBinding, 0, len(s.Secrets))
		for _, p := range s.Secrets {
			secrets = append(secrets, runtimev2.SecretBinding{Ref: p.Ref, Version: p.Version, Name: p.Name, Delivery: p.Delivery})
		}
		env := map[string]string{}
		for k, v := range s.Env {
			env[k] = v
		}
		args := append([]string{}, s.Args...)
		var command []string
		if s.Command != nil {
			command = append([]string{}, s.Command...)
		}
		ctx.Service = &runtimev2.ServiceContext{
			Name: s.Name, Role: runtimev2.ServiceRole(s.Role), DeadlineAt: at, Secrets: secrets,
			Command: runtimev2.ServiceCommand{Image: s.Image, Command: command, Args: args, Env: env},
		}
	}
	return runtimeauth.ResourceState{
		Scope: r.Scope, RunID: r.RunID, Key: key, Kind: kind, Phase: phase,
		Fingerprint: r.Fingerprint, WorkloadName: r.Name, WorkloadUID: r.UID, DeadlineAt: deadline.UTC(), Context: ctx,
	}, nil
}

// Classify maps an execution failure to the runtime authority's classes the v2
// handlers render. Anything unclassified is unavailable (fail closed).
func Classify(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, execution.ErrContextVersionMismatch):
		return fmt.Errorf("%w: %w", runtimeauth.ErrContextVersionMismatch, err)
	case errors.Is(err, execution.ErrBeginConflict):
		return fmt.Errorf("%w: %w", runtimeauth.ErrIdempotencyConflict, err)
	case errors.Is(err, execution.ErrReceiptConflict):
		return fmt.Errorf("%w: %w", runtimeauth.ErrReceiptConflict, err)
	case errors.Is(err, execution.ErrResourceUnbound):
		return fmt.Errorf("%w: %w", runtimeauth.ErrResourceUnbound, err)
	case errors.Is(err, execution.ErrResourceRevoked):
		return fmt.Errorf("%w: %w", runtimeauth.ErrResourceRevoked, err)
	case errors.Is(err, execution.ErrAttemptRevoked):
		return fmt.Errorf("%w: %w", runtimeauth.ErrAttemptRevoked, err)
	case errors.Is(err, execution.ErrNotFound), errors.Is(err, execution.ErrHidden):
		return fmt.Errorf("%w: %w", runtimeauth.ErrNotFound, err)
	case errors.Is(err, execution.ErrInvalid):
		return fmt.Errorf("%w: %w", runtimeauth.ErrInvalidRequest, err)
	case errors.Is(err, execution.ErrDenied):
		return fmt.Errorf("%w: %w", runtimeauth.ErrForbidden, err)
	}
	return fmt.Errorf("%w: %w", runtimeauth.ErrUnavailable, err)
}

// Execution is the part of *execution.Service the handlers use.
type Execution interface {
	RecordBegin(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, claim execution.GrantClaim, req runtimev2.BeginRequest) (execution.BeginAck, error)
	RecordPreparationReceipt(ctx context.Context, scope domain.Scope, run domain.RunID, claim execution.GrantClaim, req runtimev2.PreparationReceiptRequest) (execution.ReceiptAck, error)
}

var _ Execution = (*execution.Service)(nil)

// Rechecker re-verifies a verified session immediately before a durable mutation;
// *runtimeauth.Service implements both methods. Begins and Receipts require it:
// without one they refuse (fail closed), so the bridge can never record on a
// stale authorization even if a caller skipped the check.
type Rechecker interface {
	Reauthorize(ctx context.Context, a runtimeauth.Authorization) error
	ReauthorizeResource(ctx context.Context, a runtimeauth.ResourceAuthorization) error
}

var _ Rechecker = (*runtimeauth.Service)(nil)

// Begins serves the command-begin fence (runtimehttp.BeginRecorder).
type Begins struct {
	Exec    Execution
	Recheck Rechecker
}

var _ runtimehttp.BeginRecorder = Begins{}

// RecordBegin implements runtimehttp.BeginRecorder.
//
// The grant is re-verified here, after the caller's body was read and before
// execution's use case runs (an early refusal), and then once more, atomically,
// by execution itself: the verified session and signing generation travel to
// execution as a grant claim, and RecordBegin checks the grant row in the
// transaction that records the begin, after the run lock (execution.GrantFence).
// A revocation committed between the two checks therefore records nothing.
func (b Begins) RecordBegin(ctx context.Context, auth runtimeauth.Authorization, req runtimev2.BeginRequest) (runtimehttp.BeginResult, error) {
	if b.Exec == nil || b.Recheck == nil {
		return runtimehttp.BeginResult{}, fmt.Errorf("%w: execution and the session recheck are required", runtimeauth.ErrUnavailable)
	}
	if err := b.Recheck.Reauthorize(ctx, auth); err != nil {
		return runtimehttp.BeginResult{}, err
	}
	ack, err := b.Exec.RecordBegin(ctx, auth.Scope, auth.AttemptID, execution.GrantClaim{Session: string(auth.Session), Generation: auth.TokenGeneration}, req)
	if err != nil {
		return runtimehttp.BeginResult{}, Classify(err)
	}
	return runtimehttp.BeginResult{AttemptID: ack.AttemptID, BeganAt: ack.BeganAt, WorkspaceState: ack.WorkspaceState, IgnoredPresent: ack.IgnoredPresent}, nil
}

// Receipts serves the preparation receipt (runtimehttp.ReceiptRecorder).
type Receipts struct {
	Exec    Execution
	Recheck Rechecker
}

var _ runtimehttp.ReceiptRecorder = Receipts{}

// RecordPreparationReceipt implements runtimehttp.ReceiptRecorder. Like a begin it
// is rechecked here and then atomically by execution (see Begins.RecordBegin).
func (r Receipts) RecordPreparationReceipt(ctx context.Context, auth runtimeauth.ResourceAuthorization, req runtimev2.PreparationReceiptRequest) (runtimehttp.ReceiptResult, error) {
	if r.Exec == nil || r.Recheck == nil {
		return runtimehttp.ReceiptResult{}, fmt.Errorf("%w: execution and the session recheck are required", runtimeauth.ErrUnavailable)
	}
	if err := r.Recheck.ReauthorizeResource(ctx, auth); err != nil {
		return runtimehttp.ReceiptResult{}, err
	}
	ack, err := r.Exec.RecordPreparationReceipt(ctx, auth.Ref.Scope, auth.Ref.RunID, execution.GrantClaim{Session: string(auth.Session), Generation: auth.TokenGeneration}, req)
	if err != nil {
		return runtimehttp.ReceiptResult{}, Classify(err)
	}
	return runtimehttp.ReceiptResult{Digest: ack.Digest, RecordedAt: ack.RecordedAt}, nil
}
