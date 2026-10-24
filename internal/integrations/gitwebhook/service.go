// Package gitwebhook starts authorized, exact-commit runs from authenticated
// push notifications sent by a server-side receive hook on a Git host the
// installation controls (task T-036, decision O-03).
//
// The package builds no Git server or transport and trusts no payload: a
// notification is only a claim "this ref now points at this commit". Authority
// comes from the trigger record, the hook secret's signature, the project's
// current repository binding, the installer's current run authority, and
// the Git host itself, read with the read-only checkout credential.
package gitwebhook

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/workspace"
)

// Dependencies are the collaborators of a Service. All but Now and NewID are
// required.
type Dependencies struct {
	Store        Store
	Authorizer   Authorizer
	Secrets      Secrets
	Definitions  Definitions
	Repositories workspace.Repositories
	// Git reads the remote with read-only checkout credentials only.
	Git  workspace.RefReader
	Runs Runs
	// Now and NewID default to the wall clock and random identifiers.
	Now   func() time.Time
	NewID func() (string, error)
}

// Service coordinates trigger installation and notification handling.
type Service struct{ d Dependencies }

// New validates the dependencies; a missing one is a construction error, never
// an allow-all default.
func New(d Dependencies) (*Service, error) {
	switch {
	case d.Store == nil, d.Authorizer == nil, d.Secrets == nil, d.Definitions == nil,
		d.Repositories == nil, d.Git == nil, d.Runs == nil:
		return nil, errors.New("gitwebhook: store, authorizer, secrets, definitions, repositories, git and runs are required")
	}
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
	if d.NewID == nil {
		d.NewID = randomID
	}
	return &Service{d: d}, nil
}

func randomID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "ght_" + hex.EncodeToString(b[:]), nil
}

// ---- installation ----

// InstallCommand installs a trigger. The repository is never supplied: it is
// the project's configured repository, resolved here.
type InstallCommand struct {
	Scope        domain.Scope
	Principal    string
	DefinitionID domain.DefinitionID
	WorkItemID   domain.WorkItemID
	Branches     []string
	SecretRef    string
}

func (s *Service) authorize(ctx context.Context, principal string, scope domain.Scope, action Action) error {
	if err := scope.Validate(); err != nil {
		return invalidf("%v", err)
	}
	if principal == "" || len(principal) > maxPrincipalLen {
		return ErrDenied
	}
	err := s.d.Authorizer.Authorize(ctx, principal, scope, action)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrDenied):
		return ErrDenied
	case errors.Is(err, ErrNotFound):
		return ErrNotFound
	default:
		return unavailable("authorization", err)
	}
}

// Install authenticates the installer, binds the trigger to the project's
// repository and registered definition, and checks the hook secret is a
// strong value distinct from every Git credential of that repository.
func (s *Service) Install(ctx context.Context, cmd InstallCommand) (Trigger, error) {
	if err := s.authorize(ctx, cmd.Principal, cmd.Scope, ActionInstall); err != nil {
		return Trigger{}, err
	}
	if err := ValidateBranchPatterns(cmd.Branches); err != nil {
		return Trigger{}, err
	}
	cfg, err := s.d.Repositories.Repository(ctx, cmd.Scope)
	if err != nil {
		return Trigger{}, mapRepositoryError(err)
	}
	if cmd.SecretRef == "" || cmd.SecretRef == cfg.CheckoutSecretRef || cmd.SecretRef == cfg.PublisherSecretRef {
		return Trigger{}, invalidf("the hook secret must be a dedicated secret, not a Git credential")
	}
	if err := s.checkSecret(ctx, cmd.Scope, cmd.SecretRef); err != nil {
		return Trigger{}, err
	}
	if err := s.d.Definitions.Registered(ctx, cmd.Scope, cmd.DefinitionID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return Trigger{}, ErrNotFound
		}
		return Trigger{}, unavailable("definition lookup", err)
	}
	id, err := s.d.NewID()
	if err != nil {
		return Trigger{}, unavailable("identifier", err)
	}
	now := s.d.Now()
	t := Trigger{
		ID: TriggerID(id), Scope: cmd.Scope, RepositoryID: cfg.ID, DefinitionID: cmd.DefinitionID,
		WorkItemID: cmd.WorkItemID, Branches: append([]string(nil), cmd.Branches...), SecretRef: cmd.SecretRef,
		InstalledBy: cmd.Principal, Enabled: true, Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := t.Validate(); err != nil {
		return Trigger{}, err
	}
	audit := AuditRecord{
		Action: "git_trigger.installed", Scope: t.Scope, TriggerID: t.ID, Principal: cmd.Principal, At: now,
		Metadata: map[string]string{"repository": string(t.RepositoryID), "definition": string(t.DefinitionID)},
	}
	if err := s.d.Store.InsertTrigger(ctx, t, audit); err != nil {
		return Trigger{}, mapStoreError(err)
	}
	return t, nil
}

func (s *Service) checkSecret(ctx context.Context, scope domain.Scope, ref string) error {
	secret, err := s.d.Secrets.Resolve(ctx, scope, ref)
	defer clear(secret)
	switch {
	case errors.Is(err, ErrSecretNotFound):
		return ErrNotFound
	case err != nil:
		return unavailable("secret", err)
	case len(secret) < MinSecretBytes:
		return invalidf("the hook secret must be at least %d bytes", MinSecretBytes)
	}
	return nil
}

// SetEnabled disables or re-enables a trigger the principal may manage. A
// disabled trigger authenticates nothing and starts no run.
func (s *Service) SetEnabled(ctx context.Context, scope domain.Scope, principal string, id TriggerID, enabled bool) (Trigger, error) {
	if err := s.authorize(ctx, principal, scope, ActionInstall); err != nil {
		return Trigger{}, err
	}
	t, err := s.d.Store.Trigger(ctx, id)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Trigger{}, unavailable("trigger lookup", err)
	}
	if err != nil || t.Scope != scope {
		// A foreign trigger is the same answer as a missing one.
		return Trigger{}, ErrNotFound
	}
	now := s.d.Now()
	next, err := t.withEnabled(enabled, now)
	if err != nil {
		return Trigger{}, err
	}
	action := "git_trigger.disabled"
	if enabled {
		action = "git_trigger.enabled"
	}
	audit := AuditRecord{Action: action, Scope: scope, TriggerID: id, Principal: principal, At: now}
	got, err := s.d.Store.UpdateTrigger(ctx, next, t.Version, audit)
	if err != nil {
		return Trigger{}, mapStoreError(err)
	}
	return got, nil
}

func mapStoreError(err error) error {
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrConflict), errors.Is(err, ErrDeliveryConflict):
		return err
	default:
		return unavailable("store", err)
	}
}

func mapRepositoryError(err error) error {
	if errors.Is(err, workspace.ErrNotFound) {
		return fmt.Errorf("%w: the project has no configured repository", ErrNotFound)
	}
	return unavailable("repository configuration", err)
}

// ---- notification handling ----

// Notification is one received hook request: the address, the credential
// headers verbatim and the raw body. Nothing in it is trusted yet.
type Notification struct {
	TriggerID  TriggerID
	DeliveryID string
	Timestamp  string
	Signature  string
	Body       []byte
}

// Receipt is the recorded result of a delivery.
type Receipt struct {
	Outcome Outcome
	Reason  string
	RunID   domain.RunID
	Ref     string
	Commit  workspace.Commit
	// Duplicate is true when the delivery had already finished and this call
	// only reported its recorded result.
	Duplicate bool
}

// Receive authenticates and handles one notification.
//
// Order, each step a precondition of the next: signature (so a forged request
// touches no state), payload syntax, the durable delivery claim, replay of a
// finished delivery, and only then the decisions that depend on current state:
// repository identity, branch filter, the installer's authority and the exact
// commit on the remote through the read-only credential. Every terminal result
// of an authenticated, well-formed delivery, including ignored and refused
// ones, is persisted with its audit under the claim, so the identifier is
// deduplicated whatever the outcome. Admission is a separate, fenced phase: see
// admit. A dependency outage leaves the delivery pending and returns
// ErrUnavailable; redelivery resumes it.
func (s *Service) Receive(ctx context.Context, n Notification) (Receipt, error) {
	t, err := s.authenticate(ctx, n)
	if err != nil {
		return Receipt{}, err
	}
	p, err := ParsePayload(n.Body)
	if err != nil {
		s.audit(ctx, t, "git_hook.rejected", n.DeliveryID, ReasonInvalidPayload, nil)
		return Receipt{}, err
	}

	key := DeliveryKey{TriggerID: t.ID, DeliveryID: n.DeliveryID}
	digest := BodyDigest(n.Body)
	rec, claimed, err := s.d.Store.ClaimDelivery(ctx, DeliveryRecord{
		Key: key, Scope: t.Scope, BodyDigest: digest, Ref: p.Ref,
		Commit: workspace.Commit(p.After), ReceivedAt: s.d.Now(),
	})
	if err != nil {
		return Receipt{}, mapStoreError(err)
	}
	if !claimed {
		if rec.BodyDigest != digest {
			s.audit(ctx, t, "git_hook.rejected", n.DeliveryID, ReasonDeliveryConflict, nil)
			return Receipt{}, ErrDeliveryConflict
		}
		if rec.Done {
			s.audit(ctx, t, "git_hook.duplicate", n.DeliveryID, ReasonDuplicateDelivery, nil)
			return receiptOf(rec, true)
		}
		if rec.Admitting {
			// A run may already exist: never re-decide, recover it.
			return s.admit(ctx, t, key, p)
		}
	}

	if p.Repository != string(t.RepositoryID) {
		return s.finishEarly(ctx, t, key, p, OutcomeRejected, ReasonForeignRepository)
	}
	if reason := ignoreReason(t, p); reason != "" {
		return s.finishEarly(ctx, t, key, p, OutcomeIgnored, reason)
	}
	cfg, err := s.d.Repositories.Repository(ctx, t.Scope)
	if err != nil {
		return Receipt{}, mapRepositoryError(err)
	}
	if cfg.ID != t.RepositoryID {
		return s.finishEarly(ctx, t, key, p, OutcomeRejected, ReasonRepositoryRebound)
	}
	if err := s.authorize(ctx, t.InstalledBy, t.Scope, ActionRun); err != nil {
		if errors.Is(err, ErrDenied) || errors.Is(err, ErrNotFound) {
			return s.finishEarly(ctx, t, key, p, OutcomeRejected, ReasonInstallerDenied)
		}
		return Receipt{}, err
	}
	reason, err := s.verifyCommit(ctx, cfg, t, p.Ref, workspace.Commit(p.After))
	if err != nil {
		return Receipt{}, err
	}
	if reason != "" {
		return s.finishEarly(ctx, t, key, p, OutcomeRejected, reason)
	}

	// The decision to admit is made. Record that durably before the first
	// admission attempt; from here a run may exist.
	rec, err = s.d.Store.BeginAdmission(ctx, key)
	if err != nil {
		return Receipt{}, mapStoreError(err)
	}
	if rec.Done {
		return receiptOf(rec, true)
	}
	return s.admit(ctx, t, key, p)
}

// Admission lease and call bounds. The lease must outlive the bounded
// StartRun call so a live holder is never fenced out by expiry.
const (
	AdmissionLease   = 2 * time.Minute
	AdmissionTimeout = 30 * time.Second
)

// admit is the admission phase. It first recovers a run already admitted under
// the delivery's idempotency key (a lost response or lost finish), without
// re-checking authority or the branch, so a later revocation or branch movement
// can neither lose that run nor turn it into a rejection.
//
// To create a run that does not yet exist the processor must hold the
// delivery's admission lease. Only the holder calls StartRun or records a
// refusal, so a success and a rejection are mutually exclusive durable
// outcomes: a competing processor gets ErrAdmissionBusy and retries, and after
// a refusal is first decided in admission's own ledger (Runs.DecideRefusal). Under
// the lease the verified repository binding is re-checked first and carried
// into admission, so recovery cannot start a run for a rebound project.
// StartRun is idempotent by key and re-checks the installer itself.
func (s *Service) admit(ctx context.Context, t Trigger, key DeliveryKey, p Payload) (Receipt, error) {
	idem := idempotencyKey(key)
	run, found, err := s.d.Runs.FindRun(ctx, t.Scope, idem)
	if err != nil {
		return Receipt{}, unavailable("run lookup", err)
	}
	if found {
		return s.finish(ctx, t, key, p, Finish{Outcome: OutcomeStarted, RunID: run.ID, Admission: true})
	}
	holder, err := randomID()
	if err != nil {
		return Receipt{}, unavailable("identifier", err)
	}
	rec, acquired, err := s.d.Store.AcquireAdmission(ctx, key, holder, s.d.Now(), AdmissionLease)
	if err != nil {
		return Receipt{}, mapStoreError(err)
	}
	if rec.Done {
		return receiptOf(rec, true)
	}
	if !acquired {
		return Receipt{}, ErrAdmissionBusy
	}
	r, err := s.admitUnderLease(ctx, t, key, p, holder, idem)
	if err != nil {
		// Terminal results end the lease with the delivery; anything else
		// (outage, lost finish) must not hold others off until expiry.
		_ = s.d.Store.ReleaseAdmission(context.WithoutCancel(ctx), key, holder)
	}
	return r, err
}

func (s *Service) admitUnderLease(ctx context.Context, t Trigger, key DeliveryKey, p Payload, holder, idem string) (Receipt, error) {
	started := func(run RunRef) (Receipt, error) {
		return s.finish(ctx, t, key, p, Finish{Outcome: OutcomeStarted, RunID: run.ID, Admission: true})
	}
	// reject makes the refusal durable in admission's own ledger first, atomic
	// with run creation: a run that exists wins, and a stalled holder that
	// resumes later can no longer create one. Only then is it recorded on the
	// delivery, with the ledger's (first) reason.
	reject := func(reason string) (Receipt, error) {
		d, err := s.d.Runs.DecideRefusal(ctx, t.Scope, idem, reason)
		if err != nil {
			return Receipt{}, unavailable("admission decision", err)
		}
		if d.Found {
			return started(d.Run)
		}
		r, err := s.finish(ctx, t, key, p, Finish{Outcome: OutcomeRejected, Reason: d.Reason, Admission: true, Holder: holder})
		if errors.Is(err, ErrConflict) {
			return Receipt{}, ErrAdmissionBusy // the lease was lost: another processor owns the outcome
		}
		return r, err
	}
	// Another holder may have admitted between the first lookup and the lease.
	run, found, err := s.d.Runs.FindRun(ctx, t.Scope, idem)
	if err != nil {
		return Receipt{}, unavailable("run lookup", err)
	}
	if found {
		return started(run)
	}
	cfg, err := s.d.Repositories.Repository(ctx, t.Scope)
	if err != nil {
		return Receipt{}, mapRepositoryError(err)
	}
	if cfg.ID != t.RepositoryID {
		return reject(ReasonRepositoryRebound)
	}
	callCtx, cancel := context.WithTimeout(ctx, AdmissionTimeout)
	defer cancel()
	run, err = s.d.Runs.StartRun(callCtx, RunRequest{
		Scope: t.Scope, DefinitionID: t.DefinitionID, WorkItemID: t.WorkItemID, Commit: workspace.Commit(p.After), Ref: p.Ref,
		TriggerID: t.ID, DeliveryID: key.DeliveryID, IdempotencyKey: idem, RequestedBy: t.InstalledBy,
		RepositoryID: t.RepositoryID,
	})
	switch {
	case errors.Is(err, ErrRunRefused):
		return reject(ReasonAdmissionRefused)
	case err != nil:
		return Receipt{}, unavailable("run admission", err)
	}
	return started(run)
}

func idempotencyKey(key DeliveryKey) string {
	return "git-hook:" + string(key.TriggerID) + ":" + key.DeliveryID
}

// finishEarly records a pre-admission result. If another processor has already
// begun admission for this delivery, the fence refuses the result and this call
// joins the admission instead, so a rejection cannot win against a run.
func (s *Service) finishEarly(ctx context.Context, t Trigger, key DeliveryKey, p Payload, outcome Outcome, reason string) (Receipt, error) {
	r, err := s.finish(ctx, t, key, p, Finish{Outcome: outcome, Reason: reason})
	if errors.Is(err, ErrConflict) {
		return s.admit(ctx, t, key, p)
	}
	return r, err
}

// authenticate resolves the trigger and verifies the signature under its
// secret. Every failure is ErrUnauthenticated except an outage.
func (s *Service) authenticate(ctx context.Context, n Notification) (Trigger, error) {
	if n.TriggerID.Validate() != nil {
		return Trigger{}, ErrUnauthenticated
	}
	t, err := s.d.Store.Trigger(ctx, n.TriggerID)
	switch {
	case errors.Is(err, ErrNotFound):
		// Unknown triggers have no scope to attribute an audit record to.
		return Trigger{}, ErrUnauthenticated
	case err != nil:
		return Trigger{}, unavailable("trigger lookup", err)
	}
	fail := func(reason string) (Trigger, error) {
		s.audit(ctx, t, "git_hook.authentication_failed", n.DeliveryID, reason, nil)
		return Trigger{}, ErrUnauthenticated
	}
	if !t.Enabled {
		return fail(ReasonTriggerDisabled)
	}
	ts, err := strconv.ParseInt(n.Timestamp, 10, 64)
	if err != nil || ValidateDeliveryID(n.DeliveryID) != nil || n.Signature == "" || len(n.Body) > MaxBodyBytes {
		return fail(ReasonMissingCredentials)
	}
	skew := s.d.Now().Sub(time.Unix(ts, 0))
	if skew > MaxClockSkew || skew < -MaxClockSkew {
		return fail(ReasonStaleTimestamp)
	}
	secret, err := s.d.Secrets.Resolve(ctx, t.Scope, t.SecretRef)
	defer clear(secret)
	switch {
	case errors.Is(err, ErrSecretNotFound):
		return fail(ReasonSecretUnavailable)
	case err != nil:
		return Trigger{}, unavailable("secret", err)
	case len(secret) < MinSecretBytes:
		return fail(ReasonSecretUnavailable)
	}
	if !verifySignature(secret, t.ID, n.DeliveryID, ts, n.Body, n.Signature) {
		return fail(ReasonBadSignature)
	}
	return t, nil
}

// ignoreReason returns why an authenticated update is not a trigger event.
func ignoreReason(t Trigger, p Payload) string {
	switch {
	case !strings.HasPrefix(p.Ref, "refs/heads/"):
		return ReasonNotBranch
	case p.IsDeletion():
		return ReasonBranchDeleted
	case !t.MatchesRef(p.Ref):
		return ReasonBranchFiltered
	}
	return ""
}

// verifyCommit proves, through the read-only checkout credential only, that the
// remote branch exists and its history contains the exact announced commit. A
// non-empty reason is a permanent refusal; an error is an outage. The commit to
// run is never taken from the remote: the branch tip may have moved on.
func (s *Service) verifyCommit(ctx context.Context, cfg workspace.RepositoryConfig, t Trigger, ref string, commit workspace.Commit) (string, error) {
	info, err := s.d.Git.BranchContains(ctx, workspace.RepoRef{
		Scope: t.Scope, URL: cfg.URL, CheckoutSecretRef: cfg.CheckoutSecretRef,
	}, ref, commit)
	switch {
	case err != nil:
		return "", unavailable("git", err)
	case !info.Exists:
		return ReasonBranchNotFound, nil
	case !info.Contains:
		return ReasonCommitNotOnBranch, nil
	}
	return "", nil
}

func (s *Service) finish(ctx context.Context, t Trigger, key DeliveryKey, p Payload, fin Finish) (Receipt, error) {
	action := "git_hook.run_started"
	switch fin.Outcome {
	case OutcomeRejected:
		action = "git_hook.rejected"
	case OutcomeIgnored:
		action = "git_hook.ignored"
	}
	audit := AuditRecord{
		Action: action, Scope: t.Scope, TriggerID: t.ID, DeliveryID: key.DeliveryID, Reason: fin.Reason, At: s.d.Now(),
		Metadata: map[string]string{"ref": truncate(p.Ref), "commit": p.After},
	}
	if fin.RunID != "" {
		audit.Metadata["run"] = string(fin.RunID)
	}
	rec, err := s.d.Store.FinishDelivery(ctx, key, fin, audit)
	if err != nil {
		return Receipt{}, mapStoreError(err)
	}
	return receiptOf(rec, false)
}

func receiptOf(rec DeliveryRecord, duplicate bool) (Receipt, error) {
	r := Receipt{Outcome: rec.Outcome, Reason: rec.Reason, RunID: rec.RunID, Ref: rec.Ref, Commit: rec.Commit, Duplicate: duplicate}
	if rec.Outcome != OutcomeRejected {
		return r, nil
	}
	switch rec.Reason {
	case ReasonForeignRepository, ReasonRepositoryRebound:
		return r, fmt.Errorf("%w: %w: %s", ErrForeignRepository, ErrRejected, rec.Reason)
	}
	return r, fmt.Errorf("%w: %s", ErrRejected, rec.Reason)
}

// audit records a refusal that changes no state. It is best effort: failing to
// write it never turns a refusal into an acceptance or into an outage.
func (s *Service) audit(ctx context.Context, t Trigger, action, deliveryID, reason string, meta map[string]string) {
	if deliveryID != "" && ValidateDeliveryID(deliveryID) != nil {
		deliveryID = ""
	}
	_ = s.d.Store.RecordAudit(ctx, AuditRecord{
		Action: action, Scope: t.Scope, TriggerID: t.ID, DeliveryID: deliveryID, Reason: reason, Metadata: meta, At: s.d.Now(),
	})
}

func truncate(s string) string {
	if len(s) > 255 {
		return s[:255]
	}
	return s
}
