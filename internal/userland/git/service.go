package git

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
)

// Sentinel errors reported by the independent Git service. Every refusal that
// is part of the contract is distinguishable with errors.Is.
var (
	// ErrInvalidRepository reports a malformed tenant-relative repository name.
	ErrInvalidRepository = errors.New("invalid repository name")
	// ErrRepositoryUnknown reports a repository the tenant config does not name.
	ErrRepositoryUnknown = errors.New("repository is not configured")
	// ErrPolicyDenied reports an attempt the ordered rules refuse.
	ErrPolicyDenied = errors.New("access rules refuse the attempt")
	// ErrApprovalRequired reports an approval-guarded ref without approval.
	ErrApprovalRequired = errors.New("approval is required")
	// ErrStaleBase reports an observed ref that is not the expected base.
	ErrStaleBase = errors.New("observed ref is not the expected base")
	// ErrUncertain reports a ref effect that may or may not have happened.
	ErrUncertain = errors.New("ref effect outcome is uncertain")
	// ErrNoJournalEntry reports a recovery without a durable receive entry.
	ErrNoJournalEntry = errors.New("no journal entry for the source event")
	// ErrSourceEventConflict reports a stable source event reused with
	// different content.
	ErrSourceEventConflict = errors.New("source event id was reused with different content")
	// ErrResourceMismatch reports a repository whose configured static resource
	// identity and committed policy `option resource` declaration disagree.
	ErrResourceMismatch = errors.New("repository resource id disagrees with policy")
)

// PolicySource supplies the current parsed tenant git.conf. The service reads
// it for every attempt and never caches the rules, so an accepted admin push
// changes the next attempt.
type PolicySource interface {
	Current(context.Context) (Config, error)
}

// ServiceConfig is the complete dependency set of one tenant-bound Git service.
// Every boundary is required: a missing seam would silently skip verification,
// journaling or observation.
type ServiceConfig struct {
	TenantID    id.TenantID
	Storage     Storage
	Policies    PolicySource
	Executor    CommandExecutor
	Observer    RefObserver
	Journal     JournalStore
	Verify      VerificationClient
	Retention   RetentionQuery
	Clock       Clock
	ResourceIDs map[string]string
}

// UpdateRequest is one received ref update attempt.
type UpdateRequest struct {
	// ResourceID is the registered resource metadata identity.
	ResourceID string
	// Repository is the tenant-relative repository name.
	Repository string
	// Ref is an anchored ref; it is never a rule prefix.
	Ref string
	// ExpectedBase is the observed base; empty means the ref must not exist.
	ExpectedBase string
	// Candidate is the new commit; a delete has no journal representation.
	Candidate string
	// ActorCredential is the forwarded actor credential. It is used only for
	// verification and is never persisted.
	ActorCredential string
	// SourceEventID is the stable receive-event identity.
	SourceEventID string
}

// UpdateResult is the settled outcome of one serialized receive attempt.
type UpdateResult struct {
	Entry    JournalEntry
	Ref      RefState
	Report   api.ReportResponse
	Reported bool
}

// RecoveryResult is the reconciliation of one durable receive entry.
type RecoveryResult struct {
	Entry    JournalEntry
	Outcome  RecoveryOutcome
	Report   api.ReportResponse
	Reported bool
}

// RetentionPlan is the retained-commit discovery result for one repository.
type RetentionPlan struct {
	Repository string
	Commits    []string
	Pages      int
}

// BackupManifest is one tenant-scoped backup description. It carries no
// credential material.
type BackupManifest struct {
	TenantID   id.TenantID
	Repository string
	Path       string
	Refs       []RefState
	Retained   []string
}

// Service is the tenant-bound independent Git service. It implements the
// smart-HTTP read authorization seam and the atomic ref receive sequence. It
// starts no process itself and never imports Shell or Kernel internals.
type Service struct {
	tenant      id.TenantID
	storage     Storage
	policies    PolicySource
	executor    CommandExecutor
	observer    RefObserver
	journal     JournalStore
	verify      VerificationClient
	retention   RetentionQuery
	clock       Clock
	keepRefs    *KeepRefStore
	resourceIDs map[string]string

	locks lockTable
}

// NewService validates the tenant and every boundary and returns the service.
func NewService(config ServiceConfig) (*Service, error) {
	tenant, err := id.ParseTenantID(string(config.TenantID))
	if err != nil {
		return nil, fmt.Errorf("git service tenant: %w", err)
	}
	switch {
	case config.Storage == nil:
		return nil, fmt.Errorf("git service storage boundary is missing")
	case config.Policies == nil:
		return nil, fmt.Errorf("git service policy source is missing")
	case config.Executor == nil:
		return nil, fmt.Errorf("git service command executor is missing")
	case config.Observer == nil:
		return nil, fmt.Errorf("git service ref observer is missing")
	case config.Journal == nil:
		return nil, fmt.Errorf("git service journal is missing")
	case config.Verify == nil:
		return nil, fmt.Errorf("git service verification client is missing")
	case config.Retention == nil:
		return nil, fmt.Errorf("git service retention query is missing")
	case config.Clock == nil:
		return nil, fmt.Errorf("git service clock is missing")
	}
	keepRefs, err := NewKeepRefStore(config.Storage, config.Executor)
	if err != nil {
		return nil, err
	}
	var resourceIDs map[string]string
	if len(config.ResourceIDs) > 0 {
		resourceIDs = make(map[string]string, len(config.ResourceIDs))
		for repository, resourceID := range config.ResourceIDs {
			repository = strings.TrimSpace(repository)
			resourceID = strings.TrimSpace(resourceID)
			if repository == "" || resourceID == "" {
				continue
			}
			if !validJournalID(resourceID) {
				return nil, fmt.Errorf("git service resource id %q for repository %q is invalid", resourceID, repository)
			}
			resourceIDs[repository] = resourceID
		}
	}
	return &Service{
		tenant:      tenant,
		storage:     config.Storage,
		policies:    config.Policies,
		executor:    config.Executor,
		observer:    config.Observer,
		journal:     config.Journal,
		verify:      config.Verify,
		retention:   config.Retention,
		clock:       config.Clock,
		keepRefs:    keepRefs,
		resourceIDs: resourceIDs,
	}, nil
}

// ResolveResourceID resolves a repository's registered resource metadata
// identity from the static ResourceIDs mapping when configured, or else from
// the committed tenant policy's `option resource` declaration. When both are
// present and disagree, the attempt is refused with fault.Forbidden so a
// policy commit can never spoof a static pin. When neither supplies a resource
// identity, UnregisteredRepositoryError is returned so smart HTTP answers the
// uniform authentication challenge without revealing repository existence.
func (service *Service) ResolveResourceID(ctx context.Context, repository string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := validateRepositoryName(repository); err != nil {
		return "", err
	}
	staticID := service.resourceIDs[repository]
	config, err := service.policies.Current(ctx)
	if err != nil {
		return "", err
	}
	policy, hasPolicy := config.Policy(repository)
	policyID := ""
	if hasPolicy {
		policyID = policy.ResourceID
	}
	if staticID != "" && policyID != "" && staticID != policyID {
		return "", fault.Wrap(fault.Forbidden,
			fmt.Sprintf("repository %q resource id disagrees with policy", repository),
			ErrResourceMismatch)
	}
	if staticID != "" {
		return staticID, nil
	}
	if policyID != "" {
		return policyID, nil
	}
	return "", UnregisteredRepositoryError(repository)
}

// VerifyResourceBinding checks that a supplied resource identity agrees with
// both the static ResourceIDs pin (if configured) and the repository policy's
// `option resource` declaration (if configured).
func (service *Service) VerifyResourceBinding(repository, resourceID string, policy RepositoryPolicy) error {
	staticID := service.resourceIDs[repository]
	if staticID != "" && policy.ResourceID != "" && staticID != policy.ResourceID {
		return fault.Wrap(fault.Forbidden,
			fmt.Sprintf("repository %q resource id disagrees with policy", repository),
			ErrResourceMismatch)
	}
	expected := staticID
	if expected == "" {
		expected = policy.ResourceID
	}
	if expected != "" && resourceID != expected {
		return fault.Wrap(fault.Forbidden,
			fmt.Sprintf("repository %q resource id disagrees with configured binding", repository),
			ErrResourceMismatch)
	}
	return nil
}

// KeepRefs reconciles this tenant's managed retained-commit refs for one
// repository against the authoritative retained set. The set comes from the
// caller's retention source; this service never applies a retention window of
// its own. An unavailable or failed source must never call this with an empty
// set, because that would release every keep ref.
func (service *Service) KeepRefs(ctx context.Context, repository string, retained []string) (KeepRefsResult, error) {
	if _, err := service.currentPolicy(ctx, repository); err != nil {
		return KeepRefsResult{}, err
	}
	return service.keepRefs.KeepRefs(ctx, service.tenant, repository, retained)
}

// AuthorizeRead decides one local smart-HTTP read. Users with scoped RBAC
// roles, service accounts and short-lived pipeline service accounts are all
// evaluated against the repository's current ordered rules at repository
// granularity. The actor is verified earlier through the public
// repository-level `git.read` operation; there is no `git.ref.read` resource
// operation and no per-ref read rule.
func (service *Service) AuthorizeRead(ctx context.Context, repository string, identity Identity) (Decision, error) {
	if err := identity.Validate(); err != nil {
		return DecisionDeny, err
	}
	policy, err := service.currentPolicy(ctx, repository)
	if err != nil {
		return DecisionDeny, err
	}
	return policy.AuthorizeRead(ReadRequest{Identity: identity})
}

// ReadVerifier is the smart-HTTP read actor verifier. It resolves the
// requested repository's registered resource identity and verifies the
// forwarded credential through the public VerificationClient.Verify with the
// repository-level `git.read` resource operation. The request carries an
// EMPTY subject and NO approval. The verified principal and scoped roles are mapped to the local
// rule identity; a service identity is mapped by its service id even when the
// verification response also carries the service's RBAC roles, because those
// roles never grant a human-role match.
type ReadVerifier struct {
	verification      VerificationClient
	resourceID        func(repository string) (string, error)
	contextResourceID func(ctx context.Context, repository string) (string, error)
}

var _ ReadActorVerifier = (*ReadVerifier)(nil)

// NewReadVerifier validates the read-verification boundaries.
func NewReadVerifier(verification VerificationClient, resourceID func(repository string) (string, error)) (*ReadVerifier, error) {
	if verification == nil {
		return nil, fmt.Errorf("read verifier requires a verification client")
	}
	if resourceID == nil {
		return nil, fmt.Errorf("read verifier requires a resource identity resolver")
	}
	return &ReadVerifier{verification: verification, resourceID: resourceID}, nil
}

// WithResourceResolver returns a copy of the verifier that resolves repository
// resource identities with ctx. The original verifier is unchanged.
func (verifier *ReadVerifier) WithResourceResolver(resolve func(ctx context.Context, repository string) (string, error)) (*ReadVerifier, error) {
	if verifier == nil || verifier.verification == nil {
		return nil, fmt.Errorf("read verifier requires a verification client")
	}
	if resolve == nil {
		return nil, fmt.Errorf("read verifier requires a resource identity resolver")
	}
	return &ReadVerifier{
		verification:      verifier.verification,
		contextResourceID: resolve,
	}, nil
}

// VerifyReadActor verifies one smart-HTTP read actor.
func (verifier *ReadVerifier) VerifyReadActor(ctx context.Context, repository, credential string) (Identity, error) {
	var (
		resourceID string
		err        error
	)
	if verifier.contextResourceID != nil {
		resourceID, err = verifier.contextResourceID(ctx, repository)
	} else {
		resourceID, err = verifier.resourceID(repository)
	}
	if err != nil {
		return Identity{}, err
	}
	request := api.VerifyRequest{
		ActorCredential: credential,
		ResourceID:      resourceID,
		Operation:       string(api.ResourceOperationGitRead),
	}
	if err := request.Validate(); err != nil {
		return Identity{}, err
	}
	response, err := verifier.verification.Verify(ctx, request)
	if err != nil {
		return Identity{}, err
	}
	if err := response.Validate(); err != nil {
		return Identity{}, err
	}
	return identityFromVerified(response.Principal, response.Roles)
}

// Provision creates the tenant-scoped repository directory. Provisioning is
// idempotent and does not require rules: a repository may exist before its
// first rule line is accepted.
func (service *Service) Provision(ctx context.Context, repository string) (RepositoryLocation, error) {
	if err := ctx.Err(); err != nil {
		return RepositoryLocation{}, err
	}
	if err := validateRepositoryName(repository); err != nil {
		return RepositoryLocation{}, err
	}
	location, err := service.storage.EnsureRepository(service.tenant, repository)
	if err != nil {
		return RepositoryLocation{}, err
	}
	return location, nil
}

// Prepare verifies the actor and the exact canonical `git.ref.update` subject,
// evaluates the current rules, enforces approval and persists an `intended`
// entry. It performs no ref effect. A permanent refusal after verification is
// recorded as a `refused` entry and returned with its sentinel error.
func (service *Service) Prepare(ctx context.Context, request UpdateRequest) (JournalEntry, error) {
	if err := validateUpdateRequest(request); err != nil {
		return JournalEntry{}, err
	}
	unlock := service.locks.lock(eventLockKey(request.SourceEventID))
	defer unlock()
	return service.prepareLocked(ctx, request)
}

// prepareLocked is Prepare with the stable-source-event lock already held.
func (service *Service) prepareLocked(ctx context.Context, request UpdateRequest) (JournalEntry, error) {
	if entry, ok, err := service.journal.Load(ctx, request.SourceEventID); err != nil {
		return JournalEntry{}, err
	} else if ok {
		if !sameUpdateContent(entry, request) {
			return entry, ErrSourceEventConflict
		}
		return entry, nil
	}

	policy, err := service.currentPolicy(ctx, request.Repository)
	if err != nil {
		return JournalEntry{}, err
	}
	if err := service.VerifyResourceBinding(request.Repository, request.ResourceID, policy); err != nil {
		return JournalEntry{}, err
	}
	approvalRole, guarded := policy.ApprovalFor(request.Ref)
	identity, verificationID, approved, err := service.verifyActor(ctx, request, approvalRole, guarded)
	if err != nil {
		return JournalEntry{}, err
	}

	// Preliminary write permission check before any repository command: a
	// principal with no write-covering rule for this ref cannot be allowed by
	// any later update-kind evaluation.
	preliminary := AccessRequest{Identity: identity, Ref: request.Ref, Update: UpdateCreate}
	if decision, err := policy.Authorize(preliminary); err != nil {
		return JournalEntry{}, err
	} else if decision != DecisionAllow {
		return service.refuse(ctx, request, verificationID, ErrPolicyDenied)
	}

	path, err := service.repositoryPath(request.Repository)
	if err != nil {
		return JournalEntry{}, err
	}
	kind, err := service.updateKind(ctx, path, request)
	if err != nil {
		return JournalEntry{}, err
	}
	exact := AccessRequest{Identity: identity, Ref: request.Ref, Update: kind}
	if decision, err := policy.Authorize(exact); err != nil {
		return JournalEntry{}, err
	} else if decision != DecisionAllow {
		return service.refuse(ctx, request, verificationID, ErrPolicyDenied)
	}
	if guarded && (!identity.Execution || !approved) {
		return service.refuse(ctx, request, verificationID, ErrApprovalRequired)
	}

	entry := service.entryFor(request, verificationID, JournalIntended)
	if err := entry.Validate(); err != nil {
		return entry, err
	}
	if err := service.journal.Record(ctx, entry); err != nil {
		return entry, err
	}
	return entry, nil
}

// prepareBatch verifies and journals every request in a pre-receive batch. It
// performs no ref effect and sends no report for a fresh intent: the native
// receive-pack ref transaction is the atomic all-or-nothing boundary. A
// duplicate delivery of the same source event is settled from observation only,
// so a stored intent is never applied from its stored compare-and-swap. A
// permanent refusal or an unobservable duplicate rejects the whole batch before
// any effect.
func (service *Service) prepareBatch(ctx context.Context, requests []UpdateRequest) ([]JournalEntry, []JournalEntry, error) {
	seen := make(map[string]bool, len(requests))
	for _, request := range requests {
		if err := validateUpdateRequest(request); err != nil {
			return nil, nil, err
		}
		if seen[request.SourceEventID] {
			return nil, nil, fmt.Errorf("batch repeats source event id %q", request.SourceEventID)
		}
		seen[request.SourceEventID] = true
	}
	entries := make([]JournalEntry, 0, len(requests))
	fresh := make([]JournalEntry, 0, len(requests))
	for _, request := range requests {
		entry, createdIntended, err := service.prepareBatchEntry(ctx, request)
		if err != nil {
			service.refuseBatchEntries(ctx, fresh)
			return nil, nil, err
		}
		if createdIntended {
			fresh = append(fresh, entry)
		}
		entries = append(entries, entry)
	}
	return entries, fresh, nil
}

// refuseBatchEntries settles freshly created pre-receive intents to refused
// when a later batch ref or precheck rejects the push before receive-pack runs.
func (service *Service) refuseBatchEntries(ctx context.Context, entries []JournalEntry) {
	if ctx == nil {
		ctx = context.Background()
	}
	settleCtx := context.WithoutCancel(ctx)
	for _, entry := range entries {
		unlock := service.locks.lock(eventLockKey(entry.SourceEventID))
		latest, ok, err := service.journal.Load(settleCtx, entry.SourceEventID)
		if err == nil && ok && latest.State == JournalIntended {
			_, _ = service.recordRefused(settleCtx, latest, nil)
		}
		unlock()
	}
}

// prepareBatchEntry prepares one batch ref under its stable-source-event lock.
// A missing entry is verified and journaled as an intent. An existing entry is
// a duplicate delivery: a reported entry is a no-op, an accepted entry retries
// its report, and an intended or uncertain entry is reconciled from observed
// ref state only. The boolean reports whether this call freshly created a
// JournalIntended entry.
func (service *Service) prepareBatchEntry(ctx context.Context, request UpdateRequest) (JournalEntry, bool, error) {
	unlock := service.locks.lock(eventLockKey(request.SourceEventID))
	defer unlock()

	entry, ok, err := service.journal.Load(ctx, request.SourceEventID)
	if err != nil {
		return JournalEntry{}, false, err
	}
	if !ok {
		prepared, err := service.prepareLocked(ctx, request)
		if err != nil {
			return prepared, false, err
		}
		return prepared, prepared.State == JournalIntended, nil
	}
	if !sameUpdateContent(entry, request) {
		return entry, false, ErrSourceEventConflict
	}
	switch entry.State {
	case JournalReported:
		return entry, false, nil
	case JournalRefused:
		return entry, false, ErrPolicyDenied
	case JournalAccepted:
		reported, _, err := service.reportLocked(ctx, entry)
		return reported, false, err
	case JournalIntended, JournalUncertain:
		recovery, err := service.recoverLocked(ctx, request.SourceEventID)
		if err != nil {
			return recovery.Entry, false, err
		}
		switch recovery.Entry.State {
		case JournalReported:
			return recovery.Entry, false, nil
		case JournalRefused:
			return recovery.Entry, false, ErrPolicyDenied
		case JournalUncertain:
			return recovery.Entry, false, ErrUncertain
		default:
			return recovery.Entry, false, fmt.Errorf("journal state %q cannot settle a batch", recovery.Entry.State)
		}
	default:
		return entry, false, fmt.Errorf("journal state %q is unknown", entry.State)
	}
}

// Apply loads the latest durable entry, observes the ref and performs the
// compare-and-swap only when the ref still equals the expected base. It never
// re-verifies or re-evaluates policy. A stale base, an executor error, an
// unobservable ref or a lost acceptance write settles the entry as uncertain or
// refused instead of blindly retrying.
func (service *Service) Apply(ctx context.Context, entry JournalEntry) (JournalEntry, error) {
	unlock := service.locks.lock(eventLockKey(entry.SourceEventID))
	defer unlock()
	return service.applyLocked(ctx, entry)
}

// applyLocked is Apply with the stable-source-event lock already held.
func (service *Service) applyLocked(ctx context.Context, entry JournalEntry) (JournalEntry, error) {
	latest, ok, err := service.journal.Load(ctx, entry.SourceEventID)
	if err != nil {
		return entry, err
	}
	if !ok {
		return entry, ErrNoJournalEntry
	}
	entry = latest
	switch entry.State {
	case JournalAccepted, JournalReported, JournalRefused:
		return entry, nil
	case JournalUncertain:
		// An uncertain effect is never replayed blindly.
		return entry, ErrUncertain
	case JournalIntended:
	default:
		return entry, fmt.Errorf("journal state %q cannot be applied", entry.State)
	}

	observed, err := service.observer.Observe(ctx, entry.Repository, entry.Ref)
	if err != nil {
		return service.recordUncertain(ctx, entry)
	}
	if observed.Ref != entry.Ref {
		return service.recordUncertain(ctx, entry)
	}
	if observed.Commit == entry.Candidate {
		return service.recordAccepted(ctx, entry)
	}
	if observed.Commit != entry.ExpectedBase {
		return service.recordRefused(ctx, entry, ErrStaleBase)
	}

	path, err := service.repositoryPath(entry.Repository)
	if err != nil {
		return service.recordUncertain(ctx, entry)
	}
	result, err := service.executor.Run(ctx, Command{
		Name: "git",
		Args: []string{"-C", path, "update-ref", entry.Ref, entry.Candidate, entry.ExpectedBase},
	})
	if err != nil {
		return service.recordUncertain(ctx, entry)
	}
	if result.ExitCode != 0 {
		return service.recordRefused(ctx, entry, ErrStaleBase)
	}
	return service.recordAccepted(ctx, entry)
}

// Report reports only a durably accepted entry and records `reported` after a
// successful report. It never performs a ref update; a retry repeats the same
// report identity and content.
func (service *Service) Report(ctx context.Context, entry JournalEntry) (api.ReportResponse, error) {
	unlock := service.locks.lock(eventLockKey(entry.SourceEventID))
	defer unlock()
	latest, ok, err := service.journal.Load(ctx, entry.SourceEventID)
	if err != nil {
		return api.ReportResponse{}, err
	}
	if !ok {
		return api.ReportResponse{}, ErrNoJournalEntry
	}
	_, response, err := service.reportLocked(ctx, latest)
	return response, err
}

// reportLocked reports an entry that is already the latest durable state.
func (service *Service) reportLocked(ctx context.Context, entry JournalEntry) (JournalEntry, api.ReportResponse, error) {
	switch entry.State {
	case JournalReported:
		// The journal carries no report event id. A replay therefore
		// repeats the stable receive identity rather than inventing a new one,
		// and no client call is made.
		return entry, api.ReportResponse{
			VerificationID: entry.VerificationID,
			EventID:        entry.SourceEventID,
			Replayed:       true,
		}, nil
	case JournalAccepted:
	default:
		return entry, api.ReportResponse{}, fmt.Errorf("journal entry in state %q is not reportable", entry.State)
	}

	request, err := entry.ReportRequest()
	if err != nil {
		return entry, api.ReportResponse{}, err
	}
	response, err := service.verify.Report(ctx, request)
	if err != nil {
		return entry, api.ReportResponse{}, err
	}
	if err := response.Validate(); err != nil {
		return entry, api.ReportResponse{}, err
	}
	reported := entry
	reported.State = JournalReported
	reported.At = service.clock.Now()
	if err := reported.Validate(); err != nil {
		return entry, api.ReportResponse{}, err
	}
	if err := service.journal.Record(ctx, reported); err != nil {
		return entry, api.ReportResponse{}, err
	}
	return reported, response, nil
}

// Update is the serialized receive entry point. Per repository/ref and per
// stable source event id exactly one attempt runs at a time. An identical
// duplicate delivery settles the existing entry without verifying or reporting
// twice; changed content under the same source event id refuses. A duplicate
// that finds a pre-effect `intended` entry reconciles it from observed ref state
// only: the stored verification may be stale (revoked actor or changed rules)
// and the supplied credential was never checked, so the stored compare-and-swap
// is never applied.
func (service *Service) Update(ctx context.Context, request UpdateRequest) (UpdateResult, error) {
	if err := validateUpdateRequest(request); err != nil {
		return UpdateResult{}, err
	}
	eventUnlock := service.locks.lock(eventLockKey(request.SourceEventID))
	defer eventUnlock()
	refUnlock := service.locks.lock(refLockKey(request.Repository, request.Ref))
	defer refUnlock()

	entry, ok, err := service.journal.Load(ctx, request.SourceEventID)
	if err != nil {
		return UpdateResult{}, err
	}
	if ok {
		if !sameUpdateContent(entry, request) {
			return UpdateResult{Entry: entry}, ErrSourceEventConflict
		}
		if entry.State == JournalIntended {
			// A duplicate delivery must not act on the stored intent: the
			// verification behind it may be stale and the supplied credential
			// was never checked. Reconcile from observation only.
			return service.reconcileLocked(ctx, entry)
		}
		return service.settleLocked(ctx, entry)
	}
	entry, err = service.prepareLocked(ctx, request)
	if err != nil {
		return UpdateResult{Entry: entry}, err
	}
	return service.settleLocked(ctx, entry)
}

// settleLocked drives one durable entry to its settled state. It is the shared
// continuation for a fresh attempt and a duplicate delivery.
func (service *Service) settleLocked(ctx context.Context, entry JournalEntry) (UpdateResult, error) {
	// Only a durably accepted effect proves the observed ref; a refusal or an
	// unknown outcome must not claim the candidate is present.
	ref := RefState{Ref: entry.Ref, Commit: entry.Candidate}
	switch entry.State {
	case JournalReported:
		return UpdateResult{Entry: entry, Ref: ref, Reported: true}, nil
	case JournalRefused:
		return UpdateResult{Entry: entry}, nil
	case JournalAccepted:
		reported, response, err := service.reportLocked(ctx, entry)
		if err != nil {
			return UpdateResult{Entry: reported}, err
		}
		return UpdateResult{Entry: reported, Ref: ref, Report: response, Reported: true}, nil
	case JournalIntended:
		applied, err := service.applyLocked(ctx, entry)
		if err != nil {
			return UpdateResult{Entry: applied}, err
		}
		return service.settleLocked(ctx, applied)
	case JournalUncertain:
		return service.reconcileLocked(ctx, entry)
	default:
		return UpdateResult{Entry: entry}, fmt.Errorf("journal state %q is unknown", entry.State)
	}
}

// reconcileLocked settles an entry from observed ref state only. It never
// verifies an actor, re-evaluates policy or performs a ref effect, so a
// duplicate delivery can never push a ref from stale authority. An observed
// candidate records acceptance and reports; an observed base records a refusal
// so a new delivery must verify afresh; anything else stays uncertain.
func (service *Service) reconcileLocked(ctx context.Context, entry JournalEntry) (UpdateResult, error) {
	recovery, err := service.recoverLocked(ctx, entry.SourceEventID)
	if err != nil {
		return UpdateResult{Entry: recovery.Entry}, err
	}
	result := UpdateResult{Entry: recovery.Entry, Report: recovery.Report, Reported: recovery.Reported}
	if recovery.Outcome == RecoveryApplied {
		result.Ref = RefState{Ref: recovery.Entry.Ref, Commit: recovery.Entry.Candidate}
	}
	if recovery.Outcome == RecoveryUnknown {
		return result, ErrUncertain
	}
	return result, nil
}

// Recover reconciles an intended or uncertain entry from observed ref state.
// An accepted entry is report-only; reported and refused entries are settled
// no-ops. Recovery never replays an unknown effect and never re-pushes.
func (service *Service) Recover(ctx context.Context, sourceEventID string) (RecoveryResult, error) {
	unlock := service.locks.lock(eventLockKey(sourceEventID))
	defer unlock()
	return service.recoverLocked(ctx, sourceEventID)
}

// recoverLocked is Recover with the stable-source-event lock already held.
func (service *Service) recoverLocked(ctx context.Context, sourceEventID string) (RecoveryResult, error) {
	entry, ok, err := service.journal.Load(ctx, sourceEventID)
	if err != nil {
		return RecoveryResult{}, err
	}
	if !ok {
		return RecoveryResult{}, ErrNoJournalEntry
	}
	switch entry.State {
	case JournalReported:
		return RecoveryResult{Entry: entry, Reported: true}, nil
	case JournalRefused:
		return RecoveryResult{Entry: entry}, nil
	case JournalAccepted:
		reported, response, err := service.reportLocked(ctx, entry)
		if err != nil {
			return RecoveryResult{Entry: reported}, err
		}
		return RecoveryResult{Entry: reported, Outcome: RecoveryApplied, Report: response, Reported: true}, nil
	case JournalIntended, JournalUncertain:
	default:
		return RecoveryResult{Entry: entry}, fmt.Errorf("journal state %q cannot be recovered", entry.State)
	}

	observed, err := service.observer.Observe(ctx, entry.Repository, entry.Ref)
	if err != nil {
		// Without repository evidence neither outcome is proven; leave the
		// durable state untouched rather than guessing.
		return RecoveryResult{Entry: entry}, err
	}
	outcome, err := entry.Reconcile(observed)
	if err != nil {
		return RecoveryResult{Entry: entry}, err
	}
	switch outcome {
	case RecoveryApplied:
		accepted, err := service.recordAccepted(ctx, entry)
		if err != nil {
			return RecoveryResult{Entry: accepted}, err
		}
		reported, response, err := service.reportLocked(ctx, accepted)
		if err != nil {
			return RecoveryResult{Entry: reported}, err
		}
		return RecoveryResult{Entry: reported, Outcome: RecoveryApplied, Report: response, Reported: true}, nil
	case RecoveryNotApplied:
		refused, err := service.recordRefused(ctx, entry, nil)
		if err != nil {
			return RecoveryResult{Entry: refused}, err
		}
		return RecoveryResult{Entry: refused, Outcome: RecoveryNotApplied}, nil
	default:
		uncertain, err := service.recordUncertain(ctx, entry)
		if err != nil && !errors.Is(err, ErrUncertain) {
			return RecoveryResult{Entry: uncertain}, err
		}
		return RecoveryResult{Entry: uncertain, Outcome: RecoveryUnknown}, nil
	}
}

// verifyActor verifies the exact canonical `git.ref.update` subject and turns
// the verified principal into the local rule identity. The forwarded actor
// credential is the only credential used and is never retained.
func (service *Service) verifyActor(ctx context.Context, request UpdateRequest, approvalRole string, guarded bool) (Identity, string, bool, error) {
	verify := api.VerifyRequest{
		ActorCredential: request.ActorCredential,
		ResourceID:      request.ResourceID,
		Operation:       string(api.ResourceOperationGitRefUpdate),
		Subject: api.Subject{Fields: []api.SubjectField{
			{Name: "operation", Kind: "string", Value: string(api.ResourceOperationGitRefUpdate)},
			{Name: "resource", Kind: "string", Value: request.ResourceID},
			{Name: "ref", Kind: "string", Value: request.Ref},
			{Name: "expected_base", Kind: "string", Value: request.ExpectedBase},
			{Name: "candidate", Kind: "string", Value: request.Candidate},
		}},
	}
	if guarded {
		verify.Approval = "role/" + approvalRole
	}
	if err := verify.Validate(); err != nil {
		return Identity{}, "", false, err
	}
	response, err := service.verify.Verify(ctx, verify)
	if err != nil {
		return Identity{}, "", false, err
	}
	if err := response.Validate(); err != nil {
		return Identity{}, "", false, err
	}
	identity, err := identityFromVerified(response.Principal, response.Roles)
	if err != nil {
		return Identity{}, "", false, err
	}
	return identity, response.VerificationID, response.Approval, nil
}

// identityFromVerified maps one verified principal to the local rule identity.
// A verified role list belongs to a human; execution and service identities
// must not carry one. A service response may still carry its scoped RBAC roles
// (the Shell verification fact includes them), so the service is mapped by its
// service id and those roles are deliberately dropped: they never grant a
// human-role rule match.
func identityFromVerified(principal api.VerifiedPrincipal, roles []string) (Identity, error) {
	var identity Identity
	switch principal.Kind {
	case "human":
		identity = Identity{Roles: append([]string(nil), roles...)}
	case "execution":
		if len(roles) > 0 {
			return Identity{}, fmt.Errorf("verified execution identity must not carry roles")
		}
		identity = Identity{Execution: true}
	case "service":
		identity = Identity{ServiceID: principal.ID}
	default:
		return Identity{}, fmt.Errorf("verified principal kind %q is unknown", principal.Kind)
	}
	if err := identity.Validate(); err != nil {
		return Identity{}, err
	}
	return identity, nil
}

// updateKind determines the attempted update from the expected base and the
// repository ancestry. A create needs no repository command.
func (service *Service) updateKind(ctx context.Context, path string, request UpdateRequest) (UpdateKind, error) {
	if request.ExpectedBase == "" {
		return UpdateCreate, nil
	}
	result, err := service.executor.Run(ctx, Command{
		Name: "git",
		Args: []string{"-C", path, "merge-base", "--is-ancestor", request.ExpectedBase, request.Candidate},
	})
	if err != nil {
		return "", err
	}
	switch result.ExitCode {
	case 0:
		return UpdateFastForward, nil
	case 1:
		return UpdateRewind, nil
	default:
		return "", fmt.Errorf("merge-base exited %d; the update kind is unknown", result.ExitCode)
	}
}

// currentPolicy validates the repository name and reads the current tenant
// rules for every attempt.
func (service *Service) currentPolicy(ctx context.Context, repository string) (RepositoryPolicy, error) {
	if err := validateRepositoryName(repository); err != nil {
		return RepositoryPolicy{}, err
	}
	config, err := service.policies.Current(ctx)
	if err != nil {
		return RepositoryPolicy{}, err
	}
	policy, ok := config.Policy(repository)
	if !ok {
		return RepositoryPolicy{}, fmt.Errorf("%w: repository %q", ErrRepositoryUnknown, repository)
	}
	return policy, nil
}

// repositoryPath resolves the tenant-scoped repository path through the storage
// boundary.
func (service *Service) repositoryPath(repository string) (string, error) {
	if err := validateRepositoryName(repository); err != nil {
		return "", err
	}
	path, err := service.storage.RepositoryPath(service.tenant, repository)
	if err != nil {
		if errors.Is(err, ErrInvalidRepository) {
			return "", err
		}
		return "", fmt.Errorf("repository %q: %w", repository, err)
	}
	return path, nil
}

// entryFor builds one journal entry for the request.
func (service *Service) entryFor(request UpdateRequest, verificationID string, state JournalState) JournalEntry {
	return JournalEntry{
		Version:        JournalVersion,
		ResourceID:     request.ResourceID,
		Repository:     request.Repository,
		Ref:            request.Ref,
		ExpectedBase:   request.ExpectedBase,
		Candidate:      request.Candidate,
		SourceEventID:  request.SourceEventID,
		VerificationID: verificationID,
		State:          state,
		At:             service.clock.Now(),
	}
}

// refuse records a permanent pre-effect refusal. The intent never reached the
// repository.
func (service *Service) refuse(ctx context.Context, request UpdateRequest, verificationID string, cause error) (JournalEntry, error) {
	entry := service.entryFor(request, verificationID, JournalRefused)
	if err := entry.Validate(); err != nil {
		return entry, err
	}
	if err := service.journal.Record(ctx, entry); err != nil {
		return entry, err
	}
	return entry, cause
}

// recordAccepted durably records a proven effect. A lost acceptance write is
// uncertainty, not a success and not permission to replay.
func (service *Service) recordAccepted(ctx context.Context, entry JournalEntry) (JournalEntry, error) {
	accepted := entry
	accepted.State = JournalAccepted
	accepted.At = service.clock.Now()
	if err := accepted.Validate(); err != nil {
		return service.recordUncertain(ctx, entry)
	}
	if err := service.journal.Record(ctx, accepted); err != nil {
		return service.recordUncertain(ctx, entry)
	}
	return accepted, nil
}

// recordUncertain durably records an unknown outcome when possible. The
// returned error is always ErrUncertain: an unknown effect is never proof of
// execution or non-execution.
func (service *Service) recordUncertain(ctx context.Context, entry JournalEntry) (JournalEntry, error) {
	uncertain := entry
	uncertain.State = JournalUncertain
	uncertain.At = service.clock.Now()
	if err := uncertain.Validate(); err == nil {
		_ = service.journal.Record(ctx, uncertain)
	}
	return uncertain, ErrUncertain
}

// recordRefused durably records a proven non-effect.
func (service *Service) recordRefused(ctx context.Context, entry JournalEntry, cause error) (JournalEntry, error) {
	refused := entry
	refused.State = JournalRefused
	refused.At = service.clock.Now()
	if err := refused.Validate(); err != nil {
		return refused, err
	}
	if err := service.journal.Record(ctx, refused); err != nil {
		return refused, err
	}
	return refused, cause
}

// validateUpdateRequest checks the bounded request before any verification or
// repository command. A delete (empty candidate) is not representable in the
// journal and refuses here.
func validateUpdateRequest(request UpdateRequest) error {
	if err := validateRepositoryName(request.Repository); err != nil {
		return err
	}
	if !validRef(request.Ref) || strings.HasSuffix(request.Ref, "/") {
		return fmt.Errorf("ref %q is not an anchored attempt ref", request.Ref)
	}
	if request.ExpectedBase != "" && !validJournalID(request.ExpectedBase) {
		return fmt.Errorf("expected base %q is invalid", request.ExpectedBase)
	}
	if !validJournalID(request.Candidate) {
		return fmt.Errorf("candidate %q is invalid", request.Candidate)
	}
	if !validJournalID(request.SourceEventID) {
		return fmt.Errorf("source event id %q is invalid", request.SourceEventID)
	}
	if !validJournalID(request.ResourceID) {
		return fmt.Errorf("resource id %q is invalid", request.ResourceID)
	}
	if request.ActorCredential == "" {
		return fmt.Errorf("actor credential is empty")
	}
	if len(request.ActorCredential) > api.MaxCredentialBytes {
		return fmt.Errorf("actor credential is %d bytes; at most %d are allowed", len(request.ActorCredential), api.MaxCredentialBytes)
	}
	return nil
}

// sameUpdateContent reports whether a durable entry describes the same attempt
// as the delivered request. The forwarded credential is intentionally not part
// of the comparison or the durable entry.
func sameUpdateContent(entry JournalEntry, request UpdateRequest) bool {
	return entry.ResourceID == request.ResourceID &&
		entry.Repository == request.Repository &&
		entry.Ref == request.Ref &&
		entry.ExpectedBase == request.ExpectedBase &&
		entry.Candidate == request.Candidate
}

func eventLockKey(sourceEventID string) string {
	return "event\x00" + sourceEventID
}

func refLockKey(repository, ref string) string {
	return "ref\x00" + repository + "\x00" + ref
}

// lockTable is a keyed mutex. It serializes one repository/ref or one stable
// source event at a time without blocking independent keys.
type lockTable struct {
	mutex   sync.Mutex
	entries map[string]*lockEntry
}

type lockEntry struct {
	mutex sync.Mutex
	refs  int
}

// lock acquires key and returns an idempotent release function.
func (table *lockTable) lock(key string) func() {
	table.mutex.Lock()
	if table.entries == nil {
		table.entries = make(map[string]*lockEntry)
	}
	entry, ok := table.entries[key]
	if !ok {
		entry = &lockEntry{}
		table.entries[key] = entry
	}
	entry.refs++
	table.mutex.Unlock()

	entry.mutex.Lock()
	var once sync.Once
	return func() {
		once.Do(func() {
			entry.mutex.Unlock()
			table.mutex.Lock()
			entry.refs--
			if entry.refs == 0 {
				delete(table.entries, key)
			}
			table.mutex.Unlock()
		})
	}
}
