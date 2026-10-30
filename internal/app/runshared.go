package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/runtimeauth/execbridge"
	runtimehttp "github.com/optimiware/kodama/internal/runtimeauth/http"
	rtpg "github.com/optimiware/kodama/internal/runtimeauth/postgres"
	"github.com/optimiware/kodama/internal/secrets"
	secretsruntime "github.com/optimiware/kodama/internal/secrets/runtime"
)

// This file composes the run_shared model (O-30) over the pieces its owners
// built: the Kubernetes run-resource adapter (T-114), the runtime authority's
// resource subjects and the /runtime/v2 routes (T-113) and execution's
// resource lifecycle (T-112). Nothing here decides anything: each bridge maps one
// owner's consumer-owned port onto another owner's and fails closed.

// SharedRuntime is what a runtime backend contributes to the run_shared model:
// the companion resource port of the SAME backend as the attempt runtime, the
// runtime authority's check that a claimed Pod belongs to a committed workload,
// and the installation's execution policies. A backend that cannot enforce the
// policy contributes none (RuntimeSet.Shared is nil), and run_shared stays
// closed.
type SharedRuntime struct {
	Resources execution.RunResources
	Workloads runtimeauth.ResourceWorkloads
	Policies  execution.Policies
}

// SharedInputs are the collaborators of the run_shared half of the execution
// slice: the persistence and token seams of the resource subjects and the secret
// resolver of service secrets. A runtime with a SharedRuntime and no SharedInputs
// (or the reverse) is refused at assembly: the model is composed completely or
// not at all.
type SharedInputs struct {
	Store    runtimeauth.ResourceStore
	State    runtimeauth.ResourceObserver
	Tokens   runtimeauth.ResourceTokenIssuer
	Verifier runtimeauth.ResourceTokenVerifier
	// Secrets resolves the pinned secrets of run services. Nil leaves the route
	// unwired (recorded as disabled); a service with secret bindings then cannot
	// start, and one without them is unaffected.
	Secrets secrets.VersionSource
}

// admitShared checks that the run_shared model is composed completely or not
// at all, and that its parts belong to the environment and to the runtime's own
// backend: a resource port of another backend, or a development adapter under
// production, is refused before anything starts.
func admitShared(in ExecutionInputs, set RuntimeSet) error {
	switch {
	case set.Shared == nil && in.Shared == nil:
		return nil
	case set.Shared == nil:
		return fmt.Errorf("%w: run_shared inputs were supplied but the %s runtime contributes no resource port", execution.ErrNotConfigured, set.Runtime.Capabilities().Backend)
	case in.Shared == nil:
		return fmt.Errorf("%w: the %s runtime offers run_shared but the resource authority is not supplied", execution.ErrNotConfigured, set.Runtime.Capabilities().Backend)
	case set.Shared.Resources == nil, set.Shared.Workloads == nil, set.Shared.Policies == nil:
		return fmt.Errorf("%w: run_shared needs the run resource port, the workload verifier and the policy source", execution.ErrNotConfigured)
	case in.Shared.Store == nil, in.Shared.State == nil, in.Shared.Tokens == nil, in.Shared.Verifier == nil:
		return fmt.Errorf("%w: run_shared needs the resource grant store, state, token issuer and verifier", execution.ErrNotConfigured)
	}
	for _, adapter := range []any{set.Shared.Resources, set.Shared.Workloads, set.Shared.Policies, in.Shared.Store, in.Shared.State, in.Shared.Secrets} {
		if adapter == nil {
			continue
		}
		if _, err := Admit(in.Env, PortRuntime, adapter); err != nil {
			return err
		}
	}
	if got, want := set.Shared.Resources.Capabilities(context.Background()).Backend, set.Runtime.Capabilities().Backend; got != want {
		return fmt.Errorf("%w: the run resource port is the %q backend's, the runtime is %q's", execution.ErrNotConfigured, got, want)
	}
	return nil
}

// ---- Kubernetes bridges ------------------------------------------------------

// kubeResourceRegistrar bridges the adapter-local registration port (seam S5) to
// runtimeauth.RegisterResource. A different credential for the resource is a
// registration conflict; a resource that is stopping, revoked or past its
// deadline is a revoked registration; everything else is an outage the adapter
// repeats with the same credential.
type kubeResourceRegistrar struct{ late *LateRegistrar }

var _ kube.ResourceRegistrar = kubeResourceRegistrar{}

func (k kubeResourceRegistrar) RegisterResource(ctx context.Context, cmd kube.ResourceRegisterCommand) error {
	err := k.late.RegisterResource(ctx, runtimeauth.ResourceRegisterCommand{
		Scope: cmd.Scope, RunID: cmd.RunID, Key: cmd.Key, Credential: cmd.Credential, SpecFingerprint: cmd.SpecFingerprint, ExpiresAt: cmd.ExpiresAt,
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, runtimeauth.ErrConflict):
		return fmt.Errorf("%w: %w", kube.ErrRegistrationConflict, err)
	case errors.Is(err, runtimeauth.ErrResourceRevoked):
		return fmt.Errorf("%w: %w", kube.ErrRegistrationRevoked, err)
	}
	return err
}

// kubeWorkloads bridges runtimeauth's ResourceWorkloads (seam S6) to the
// adapter's VerifyResourcePod. A Pod that definitely is not the committed
// workload's is runtimeauth.ErrNotObserved; an unreadable cluster is not.
type kubeWorkloads struct {
	rr interface {
		VerifyResourcePod(ctx context.Context, h execution.ResourceHandle, podName, podUID string) error
	}
}

var _ runtimeauth.ResourceWorkloads = kubeWorkloads{}

func (k kubeWorkloads) VerifyPod(ctx context.Context, w runtimeauth.ResourceWorkload, podName, podUID string) error {
	kind, err := runtimeauth.ResourceKindOf(w.Ref.Key)
	if err != nil {
		return err
	}
	h := execution.ResourceHandle{
		Scope: w.Ref.Scope, RunID: w.Ref.RunID, Kind: execution.ResourceKind(kind), Key: w.Ref.Key, Name: w.Name, UID: w.UID,
	}
	err = k.rr.VerifyResourcePod(ctx, h, podName, podUID)
	if errors.Is(err, kube.ErrPodNotOwned) {
		return fmt.Errorf("%w: %w", runtimeauth.ErrNotObserved, err)
	}
	return err
}

// ---- the resource authority --------------------------------------------------

// resourceStore is the part of the execution PostgreSQL store the authority uses.
type resourceStore interface {
	Get(ctx context.Context, scope domain.Scope, id domain.RunID) (*execution.Aggregate, error)
	UpdateInTx(ctx context.Context, tx pgx.Tx, scope domain.Scope, id domain.RunID, opts execution.UpdateOptions, fn execution.Mutation) (*execution.Aggregate, any, error)
}

var _ resourceStore = (*execpg.Store)(nil)

// resourceAuthority is the runtime authority's view of execution's committed
// run resources (rtpg.ResourceAuthority). The state is execbridge.StateOf over
// the committed aggregate; nothing comes from a request.
//
// LockResource reads the aggregate inside the caller's transaction under the
// run row's lock, so a stop or the run's cancellation (which lock the run for
// update) either committed first and is seen or waits for the caller. It is the
// execution store's own locked read with a mutation that changes nothing: the
// aggregate stays clean, so nothing is written, no grant is revoked and no
// event is recorded.
type resourceAuthority struct {
	store resourceStore
	now   func() time.Time
}

var _ rtpg.ResourceAuthority = (*resourceAuthority)(nil)

func newResourceAuthority(store resourceStore, now func() time.Time) (*resourceAuthority, error) {
	if store == nil {
		return nil, errors.New("app: an execution store is required for the resource authority")
	}
	if now == nil {
		now = time.Now
	}
	return &resourceAuthority{store: store, now: now}, nil
}

// ObserveResource implements runtimeauth.ResourceObserver (advisory, no lock).
func (a *resourceAuthority) ObserveResource(ctx context.Context, ref runtimeauth.ResourceRef) (runtimeauth.ResourceState, error) {
	if err := ref.Validate(); err != nil {
		return runtimeauth.ResourceState{}, err
	}
	agg, err := a.store.Get(ctx, ref.Scope, ref.RunID)
	if err != nil {
		return runtimeauth.ResourceState{}, classifyResourceRead(err)
	}
	return execbridge.StateOf(agg, ref.Key, a.now())
}

// LockResource implements rtpg.ResourceAuthority.
func (a *resourceAuthority) LockResource(ctx context.Context, tx pgx.Tx, ref runtimeauth.ResourceRef) (runtimeauth.ResourceState, error) {
	if err := ref.Validate(); err != nil {
		return runtimeauth.ResourceState{}, err
	}
	var state runtimeauth.ResourceState
	_, _, err := a.store.UpdateInTx(ctx, tx, ref.Scope, ref.RunID, execution.UpdateOptions{}, func(_ context.Context, _ execution.Env, agg *execution.Aggregate) (execution.Effects, error) {
		var err error
		state, err = execbridge.StateOf(agg, ref.Key, a.now())
		return execution.Effects{}, err
	})
	if err != nil {
		return runtimeauth.ResourceState{}, classifyResourceRead(err)
	}
	return state, nil
}

// classifyResourceRead keeps a runtimeauth class and maps execution's absence to
// runtimeauth.ErrNotFound; anything else is an outage (never absence).
func classifyResourceRead(err error) error {
	switch {
	case errors.Is(err, runtimeauth.ErrNotFound), errors.Is(err, runtimeauth.ErrUnavailable), errors.Is(err, runtimeauth.ErrInvalidRequest):
		return err
	case errors.Is(err, execution.ErrNotFound), errors.Is(err, execution.ErrHidden):
		return fmt.Errorf("%w: %w", runtimeauth.ErrNotFound, err)
	}
	return fmt.Errorf("%w: %w", runtimeauth.ErrUnavailable, err)
}

// ---- the secrets of a service --------------------------------------------------

// denyAttempts is the attempt authority of the resolver that serves services: a
// service resolves through ResolveResource only, so the attempt path of this
// resolver has no authority at all.
type denyAttempts struct{}

func (denyAttempts) Grant(context.Context, domain.AttemptID) (secrets.AttemptGrant, error) {
	return secrets.AttemptGrant{}, secrets.ErrAuthorityMissing
}

var _ secrets.AttemptAuthority = denyAttempts{}

// resourceSecrets is runtimehttp.ResourceSecretResolver: a service resolves only
// the bindings its committed context pins (ResourceAuthorization.Bindings), and
// the grant is rechecked against current authority every time the resolver asks
// (before and after the provider call), so a stopped or revoked service releases
// nothing. Errors are the runtime contract's, with fixed texts.
type resourceSecrets struct {
	resolver *secrets.Resolver
	auth     interface {
		ReauthorizeResource(ctx context.Context, a runtimeauth.ResourceAuthorization) error
	}
}

var _ runtimehttp.ResourceSecretResolver = resourceSecrets{}

func newResourceSecrets(source secrets.VersionSource, svc *runtimeauth.Service) (resourceSecrets, error) {
	r, err := secrets.NewResolver(denyAttempts{}, source)
	if err != nil {
		return resourceSecrets{}, err
	}
	return resourceSecrets{resolver: r, auth: svc}, nil
}

func (r resourceSecrets) ResolveResourceSecret(ctx context.Context, auth runtimeauth.ResourceAuthorization, req runtimev1.SecretResolveRequest, requestID string) (runtimev1.SecretResolveResponse, error) {
	grant := secrets.ResourceGrant{Scope: auth.Ref.Scope, RunID: auth.Ref.RunID, Key: auth.Ref.Key, Bindings: auth.Bindings()}
	current := func(ctx context.Context) (secrets.ResourceGrant, error) {
		switch err := r.auth.ReauthorizeResource(ctx, auth); {
		case err == nil:
			return grant, nil
		case errors.Is(err, runtimeauth.ErrResourceRevoked), errors.Is(err, runtimeauth.ErrForbidden), errors.Is(err, runtimeauth.ErrAttemptRevoked):
			return secrets.ResourceGrant{}, secrets.ErrAttemptInactive
		case errors.Is(err, runtimeauth.ErrInvalidCredential), errors.Is(err, runtimeauth.ErrNotFound), errors.Is(err, runtimeauth.ErrInvalidRequest):
			return secrets.ResourceGrant{}, secrets.ErrAuthorityMissing
		default:
			return secrets.ResourceGrant{}, err
		}
	}
	resp, err := r.resolver.ResolveResource(ctx, current, req, requestID)
	if err != nil {
		return runtimev1.SecretResolveResponse{}, secretsruntime.Translate(err)
	}
	return resp, nil
}

// ---- prior disclosures ---------------------------------------------------------

// runBindings is secrets.RunBindings over the execution store: the secret
// bindings a run disclosed to its OTHER attempts and to every service. In a
// run_shared run the scratch survives from step to step and a service can answer
// a later step, so a value given to one can reach source, logs or outputs of
// another that was never given it (VS-14). The answer is the pinned bindings of
// every other attempt that began its command and of every declared service: the
// committed state, never anything a workload said. A run that is not run_shared
// has none.
type runBindings struct{ runs needleRuns }

var _ secrets.RunBindings = runBindings{}

// PriorBindings implements secrets.RunBindings.
func (b runBindings) PriorBindings(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (domain.RunID, []runtimev1.SecretBinding, error) {
	if b.runs == nil {
		return "", nil, errors.New("no execution store backs the run bindings")
	}
	if scope.Validate() != nil || attempt.Validate() != nil {
		return "", nil, execution.ErrNotFound
	}
	run, err := b.runs.RunOf(ctx, scope, attempt)
	if err != nil {
		return "", nil, err
	}
	agg, err := b.runs.Get(ctx, scope, run)
	if err != nil {
		return "", nil, err
	}
	if agg == nil || agg.Run == nil || agg.Run.Scope != scope || agg.Run.ID != run || agg.Attempt(attempt) == nil {
		return "", nil, execution.ErrNotFound
	}
	if agg.Run.Shared == nil {
		return "", nil, nil
	}
	out, err := runDisclosed(agg, attempt)
	return run, out, err
}

// runDisclosed lists the bindings disclosed to everything of the run except the
// given attempt (the empty attempt excludes none).
func runDisclosed(agg *execution.Aggregate, except domain.AttemptID) ([]runtimev1.SecretBinding, error) {
	var out []runtimev1.SecretBinding
	for _, begin := range agg.Begins {
		if begin.AttemptID == except {
			continue
		}
		at := agg.Attempt(begin.AttemptID)
		if at == nil {
			return nil, fmt.Errorf("%w: a recorded begin has no attempt", execution.ErrInvalid)
		}
		pinned, err := decodePinned(at.Context)
		if err != nil || pinned.RunID != agg.Run.ID || pinned.AttemptID != at.ID {
			return nil, fmt.Errorf("%w: an attempt's pinned context is not readable", execution.ErrInvalid)
		}
		out = append(out, pinned.Secrets...)
	}
	for _, r := range agg.Resources {
		if r.Kind != execution.KindService || r.Spec.Service == nil {
			continue
		}
		for _, p := range r.Spec.Service.Secrets {
			out = append(out, runtimev1.SecretBinding{Ref: p.Ref, Version: p.Version, Name: p.Name, Delivery: p.Delivery})
		}
	}
	return out, nil
}

// noOwnRedactions is the own-values half of the log redaction of a run_shared
// run: no value is injected into an attempt through a channel this assembly can
// enumerate (the runtime secret route resolves only a pinned environment's
// credentials, and direct delivery is T-107), so the values to remove are the
// disclosed ones. It is explicit so a later delivery binds its own set here
// rather than the run silently narrowing the redaction.
type noOwnRedactions struct{}

func (noOwnRedactions) Values(context.Context, domain.Scope, domain.AttemptID) ([][]byte, error) {
	return [][]byte{}, nil
}

// ---- the legacy format -----------------------------------------------------------

// retireLegacy enforces the policy's `legacyFormat` at admission: with `drain` or
// `closed` a new run of a kodama/v1alpha1 definition is refused with the stable
// format_retired reason, while `open` (the default of every installation) admits
// both formats. It decorates the definition lookup of admission only; runs that
// already exist keep their pinned snapshot and finish under their original
// semantics, and historical definitions stay readable through the pipeline module.
type retireLegacy struct {
	execution.Definitions
	policies execution.Policies
}

func (r retireLegacy) Get(ctx context.Context, scope domain.Scope, id domain.DefinitionID) (pipeline.Definition, error) {
	def, err := r.Definitions.Get(ctx, scope, id)
	if err != nil {
		return def, err
	}
	doc, derr := def.Content.Document()
	if derr != nil || doc.APIVersion != legacyAPIVersion {
		return def, nil // other formats and unreadable content are the execution guard's to judge
	}
	pol, perr := r.policies.Current(ctx)
	if perr != nil {
		return pipeline.Definition{}, execution.Unavailablef(perr, "the execution policy is unavailable")
	}
	if pol.Admission.LegacyFormat != execution.PolicyOpen {
		return pipeline.Definition{}, execution.Refusedf("%s: new kodama/v1alpha1 runs are no longer admitted; register a kodama/v1alpha2 definition", pipeline.AdmissionFormatRetired)
	}
	return def, nil
}

// legacyAPIVersion is the original executor's format.
const legacyAPIVersion = "kodama/v1alpha1"
