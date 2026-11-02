package execbridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/runtimeauth"
	runtimehttp "github.com/optimiware/kodama/internal/runtimeauth/http"
)

// PreparationGrantRef is the stable reference of a source-set run's preparation Git
// grant carried in its resource context (git.grantId), as execution.GitGrantRef is
// for an attempt: the context is committed before a grant can exist, so the
// reference is derived from the run, never read from the Git authority. It
// correlates the context with the grant and is not a credential.
func PreparationGrantRef(scope domain.Scope, run domain.RunID) string {
	sum := sha256.Sum256([]byte("kodama/git-grant-ref/preparation/" + string(scope.TenantID) + "/" + string(scope.ProjectID) + "/" + string(run)))
	return "gg_" + hex.EncodeToString(sum[:16])
}

// stateOfV3 maps the committed record of one resource of a source-set run to the
// authority state of its workload, on the v3 wire: the preparation's assignment is
// the whole source set (one checkout per source in path order) and its Git
// delivery is the read-only preparation grant's descriptor (no update right, no
// secret); a service is the unchanged v2 service on the v3 wire. As for v2 the
// context is built only from the committed spec: no request value enters it.
func stateOfV3(a *execution.Aggregate, r *execution.RunResource, kind runtimev2.ResourceKind, key string, phase runtimeauth.ResourcePhase, deadline time.Time) (runtimeauth.ResourceState, error) {
	at := runtimev3.Timestamp(deadline.UTC().Format(time.RFC3339Nano))
	pin := runtimev3.PolicyPin{ID: r.Spec.Policy.ID, Digest: runtimev3.Digest(r.Spec.Policy.Digest)}
	ctx := runtimev3.ResourceContextV3{
		ContextVersion: runtimev3.ContextVersion, ResourceKind: kind,
		TenantID: r.Scope.TenantID, ProjectID: r.Scope.ProjectID, RunID: r.RunID, ResourceKey: key, Policy: pin,
	}
	switch kind {
	case runtimev2.ResourcePreparation:
		p := r.Spec.Preparation
		if p == nil || len(p.Sources) == 0 || a.Run.Sources == nil {
			return runtimeauth.ResourceState{}, fmt.Errorf("%w: the committed spec has no source-set preparation", runtimeauth.ErrUnavailable)
		}
		sources := make([]runtimev3.PreparationSource, len(p.Sources))
		var repos []string
		seen := map[string]bool{}
		for i, s := range p.Sources {
			sources[i] = runtimev3.PreparationSource{RepositoryID: s.RepositoryID, RepositoryURL: s.RepositoryURL, Commit: runtimev3.GitCommit(s.Commit), Path: s.Path}
			if !seen[s.RepositoryID] {
				seen[s.RepositoryID] = true
				repos = append(repos, s.RepositoryID)
			}
		}
		var ca *string
		if a.Run.Sources.CAFile != nil {
			v := *a.Run.Sources.CAFile
			ca = &v
		}
		ctx.Preparation = &runtimev3.PreparationContextV3{
			Sources: sources, MountPath: runtimev3.MountPath, TimeoutSeconds: p.TimeoutSecs, MaxBytes: p.MaxBytes, MaxAttempts: p.MaxAttempts, DeadlineAt: at,
			Git: runtimev3.PreparationGit{
				GrantID: PreparationGrantRef(r.Scope, r.RunID), BaseURL: a.Run.Sources.BaseURL,
				CredentialFile: runtimev3.GitDirectory + "/credential", CAFile: ca, Repositories: repos,
			},
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
			Name: s.Name, Role: runtimev2.ServiceRole(s.Role), DeadlineAt: runtimev2.Timestamp(at), Secrets: secrets,
			Command: runtimev2.ServiceCommand{Image: s.Image, Command: command, Args: args, Env: env},
		}
	}
	return runtimeauth.ResourceState{
		Scope: r.Scope, RunID: r.RunID, Key: key, Kind: kind, Phase: phase,
		Fingerprint: r.Fingerprint, WorkloadName: r.Name, WorkloadUID: r.UID, DeadlineAt: deadline.UTC(), ContextV3: &ctx,
	}, nil
}

// ExecutionV3 is the part of *execution.Service the v3 handlers use.
type ExecutionV3 interface {
	RecordBeginV3(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, claim execution.GrantClaim, req runtimev3.BeginRequestV3) (execution.BeginAck, error)
	RecordPreparationReceiptV3(ctx context.Context, scope domain.Scope, run domain.RunID, claim execution.GrantClaim, req runtimev3.PreparationReceiptRequestV3) (execution.ReceiptAck, error)
}

var _ ExecutionV3 = (*execution.Service)(nil)

// BeginsV3 serves the v3 command-begin fence (runtimehttp.BeginRecorderV3).
type BeginsV3 struct {
	Exec    ExecutionV3
	Recheck Rechecker
}

var _ runtimehttp.BeginRecorderV3 = BeginsV3{}

// RecordBeginV3 implements runtimehttp.BeginRecorderV3 with the semantics of
// Begins.RecordBegin: rechecked here, then atomically by execution, which checks
// the verified session as a grant claim in the transaction that records the begin.
func (b BeginsV3) RecordBeginV3(ctx context.Context, auth runtimeauth.Authorization, req runtimev3.BeginRequestV3) (runtimehttp.BeginResult, error) {
	if b.Exec == nil || b.Recheck == nil {
		return runtimehttp.BeginResult{}, fmt.Errorf("%w: execution and the session recheck are required", runtimeauth.ErrUnavailable)
	}
	if err := b.Recheck.Reauthorize(ctx, auth); err != nil {
		return runtimehttp.BeginResult{}, err
	}
	ack, err := b.Exec.RecordBeginV3(ctx, auth.Scope, auth.AttemptID, execution.GrantClaim{Session: string(auth.Session), Generation: auth.TokenGeneration}, req)
	if err != nil {
		return runtimehttp.BeginResult{}, Classify(err)
	}
	return runtimehttp.BeginResult{AttemptID: ack.AttemptID, BeganAt: ack.BeganAt, WorkspaceState: ack.WorkspaceState, IgnoredPresent: ack.IgnoredPresent}, nil
}

// ReceiptsV3 serves the v3 preparation receipt (runtimehttp.ReceiptRecorderV3).
type ReceiptsV3 struct {
	Exec    ExecutionV3
	Recheck Rechecker
}

var _ runtimehttp.ReceiptRecorderV3 = ReceiptsV3{}

// RecordPreparationReceiptV3 implements runtimehttp.ReceiptRecorderV3. Like a
// begin it is rechecked here and then atomically by execution.
func (r ReceiptsV3) RecordPreparationReceiptV3(ctx context.Context, auth runtimeauth.ResourceAuthorization, req runtimev3.PreparationReceiptRequestV3) (runtimehttp.ReceiptResult, error) {
	if r.Exec == nil || r.Recheck == nil {
		return runtimehttp.ReceiptResult{}, fmt.Errorf("%w: execution and the session recheck are required", runtimeauth.ErrUnavailable)
	}
	if err := r.Recheck.ReauthorizeResource(ctx, auth); err != nil {
		return runtimehttp.ReceiptResult{}, err
	}
	ack, err := r.Exec.RecordPreparationReceiptV3(ctx, auth.Ref.Scope, auth.Ref.RunID, execution.GrantClaim{Session: string(auth.Session), Generation: auth.TokenGeneration}, req)
	if err != nil {
		return runtimehttp.ReceiptResult{}, Classify(err)
	}
	return runtimehttp.ReceiptResult{Digest: ack.Digest, RecordedAt: ack.RecordedAt}, nil
}

// ---- attempt lease: the liveness of an attempt for the Git authority -------------

// AttemptLease is what the Git services need to know of an attempt: whether it is
// live and when its authority ends at the latest.
type AttemptLease struct {
	// Live is true while the attempt is running, not revoked, and its run is
	// neither draining, terminal nor past its deadline.
	Live bool
	// Deadline is the run's pinned deadline (zero when the run has none). The
	// Git grants of the attempt never outlive it.
	Deadline time.Time
}

// LeaseOf derives the lease of an attempt from the aggregate of its run (read by
// the caller; a lease is a decision at a moment, enforced again at every push by
// the push authorizer). It is false for an attempt that is not running, whose
// authority was revoked (cancellation, failure drain, timeout, supersession or an
// accepted result), whose run is draining or terminal, or whose run deadline has
// passed. The deadline is the run's, which is also the latest the attempt could
// have been admitted to run, so a grant bounded by it never outlives the lease.
func LeaseOf(a *execution.Aggregate, attempt domain.AttemptID, now time.Time) (AttemptLease, error) {
	if a == nil || a.Run == nil {
		return AttemptLease{}, fmt.Errorf("%w: no run", runtimeauth.ErrNotFound)
	}
	at := a.Attempt(attempt)
	if at == nil || at.Scope != a.Run.Scope {
		return AttemptLease{}, fmt.Errorf("%w: no such attempt", runtimeauth.ErrNotFound)
	}
	var lease AttemptLease
	if a.Run.DeadlineAt != nil {
		lease.Deadline = a.Run.DeadlineAt.UTC()
	}
	c := a.Control(attempt)
	lease.Live = at.Status == execution.AttemptRunning && c != nil && c.Live() &&
		!a.Run.Draining() && !a.Run.Status.Terminal() && (lease.Deadline.IsZero() || now.Before(lease.Deadline))
	return lease, nil
}
