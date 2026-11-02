package gitpush

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gitauth"
)

// Options tune the use cases. The zero value is usable.
type Options struct {
	// MaxUpdates is the installation's bound on the ref updates of one receive
	// (GR-16); 0 or a value above gitv1.MaxPushUpdates selects the contract ceiling.
	MaxUpdates int
	// ReceiveDeadline is how long a receive may run from its authorization (the
	// worker terminates an overrunning receive); default 15 minutes, at most
	// gitv1.MaxReceiveDeadlineLifetime.
	ReceiveDeadline time.Duration
	// EvidenceTimeout is how long after the receive deadline an unreadable journal
	// is waited for before the entry is settled unknown (journal_unreadable);
	// default 10 minutes.
	EvidenceTimeout time.Duration
	// Clock defaults to time.Now.
	Clock func() time.Time
	// NewID creates identifiers ("push" prefix for push records); default random.
	NewID func(prefix string) string
}

// Dependencies are the collaborators. Store, Repositories, Grants and Attempts
// are required. A nil Reservations means no attempt holds a writer reservation,
// so a workload never updates a task branch; a nil Worker disables Reconcile; a
// nil Reads refuses every read.
type Dependencies struct {
	Store        Store
	Repositories Repositories
	Grants       Grants
	Attempts     Attempts
	Reservations Reservations
	Worker       Worker
	Reads        ReadAuthorizer
	// Admin authorizes the operator resolution of unknown entries; nil refuses it.
	Admin ResolveAuthorizer
}

// Service holds the use cases of push authorization, the journal and the events.
// It implements the control-plane half of the contract's gitpush.Service and
// WriterGate (contracts/git/README.md).
type Service struct {
	d Dependencies
	o Options
}

// NewService validates the configuration.
func NewService(d Dependencies, o Options) (*Service, error) {
	if d.Store == nil || d.Repositories == nil || d.Grants == nil || d.Attempts == nil {
		return nil, fmt.Errorf("%w: a store, repositories, grants and attempts are required", ErrNotConfigured)
	}
	if o.ReceiveDeadline == 0 {
		o.ReceiveDeadline = 15 * time.Minute
	}
	if o.ReceiveDeadline < time.Second || o.ReceiveDeadline > gitv1.MaxReceiveDeadlineLifetime {
		return nil, fmt.Errorf("gitpush: the receive deadline must be between 1s and %s", gitv1.MaxReceiveDeadlineLifetime)
	}
	if o.EvidenceTimeout == 0 {
		o.EvidenceTimeout = 10 * time.Minute
	}
	if o.MaxUpdates < 0 {
		return nil, errors.New("gitpush: the update bound must not be negative")
	}
	if o.Clock == nil {
		o.Clock = time.Now
	}
	if o.NewID == nil {
		o.NewID = randomID
	}
	return &Service{d: d, o: o}, nil
}

func randomID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(b)
}

func (s *Service) now() time.Time { return s.o.Clock().UTC().Truncate(time.Microsecond) }

// depErr maps a dependency failure: a contract error passes through (the
// dependency's own refusal), anything else is a retryable outage, never access.
func depErr(err error, what string) error {
	var ce *gitv1.ContractError
	if errors.As(err, &ce) {
		return err
	}
	return unavailable(err, what)
}

func requestDigest(req gitv1.PushAuthorizeRequest) (string, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func (s *Service) repository(ctx context.Context, tenant domain.TenantID, id string) (RepositoryFacts, error) {
	repo, err := s.d.Repositories.Repository(ctx, tenant, id)
	switch {
	case errors.Is(err, ErrNotFound):
		return RepositoryFacts{}, notFound()
	case err != nil:
		return RepositoryFacts{}, depErr(err, "repository")
	case repo.Scope.TenantID != tenant:
		return RepositoryFacts{}, notFound()
	}
	return repo, nil
}

// replay answers a repeated receive: the identical request returns the stored
// response, any other request is idempotency_conflict. found is false when the
// receive has no record yet.
func (s *Service) replay(ctx context.Context, tenant domain.TenantID, repositoryID, receiveID, digest string) (resp gitv1.PushAuthorizeResponse, found bool, err error) {
	var rec *Record
	err = s.d.Store.View(ctx, tenant, func(ctx context.Context, tx Tx) error {
		r, err := tx.ByReceive(ctx, repositoryID, receiveID)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		rec = r
		return err
	})
	if err != nil || rec == nil {
		return gitv1.PushAuthorizeResponse{}, false, err
	}
	if rec.RequestDigest != digest {
		return gitv1.PushAuthorizeResponse{}, true, refuse(domain.CodeIdempotencyConflict, "the receive %s was already authorized with a different request", truncate(receiveID, 64))
	}
	return rec.AuthorizeResponse(), true, nil
}

// Authorize implements the authorization step of the push journal: it checks
// each update against the grant, the ref policy matrix, the repository's policy
// and archive state and current authority, creates the push record (one entry
// per ref, the kind persisted from the git facts) and returns the verdicts. The
// record is durable before the response. The seed grant of a seed receive is
// consumed in the same transaction, before the record is inserted. Nothing here
// calls Git or a worker, and no network call is made while a SQL lock is held:
// claims, liveness and writer reservations are read before the transaction, and
// the writer reservation is read again after the record committed and before the
// verdicts are returned (recheckWriter).
func (s *Service) Authorize(ctx context.Context, tenant domain.TenantID, req gitv1.PushAuthorizeRequest) (gitv1.PushAuthorizeResponse, error) {
	if err := tenant.Validate(); err != nil {
		return gitv1.PushAuthorizeResponse{}, notFound()
	}
	if err := gitv1.CheckUpdateBound(len(req.Updates), s.o.MaxUpdates); err != nil {
		return gitv1.PushAuthorizeResponse{}, err
	}
	if err := req.Validate(); err != nil {
		return gitv1.PushAuthorizeResponse{}, refuse(domain.CodeInvalidRequest, "%v", err)
	}
	digest, err := requestDigest(req)
	if err != nil {
		return gitv1.PushAuthorizeResponse{}, err
	}
	// A foreign or unknown repository is not_found, before anything else.
	repo, err := s.repository(ctx, tenant, req.RepositoryID)
	if err != nil {
		return gitv1.PushAuthorizeResponse{}, err
	}
	if resp, found, err := s.replay(ctx, tenant, req.RepositoryID, req.ReceiveID, digest); found || err != nil {
		return resp, err
	}
	claims, err := s.d.Grants.Claims(ctx, tenant, req.GrantID)
	if err != nil {
		return gitv1.PushAuthorizeResponse{}, depErr(err, "grant")
	}
	if err := claims.CheckScope(string(tenant), string(repo.Scope.ProjectID), repo.ID); err != nil {
		return gitv1.PushAuthorizeResponse{}, err
	}
	switch {
	case repo.Hosting != gitv1.HostingInternal:
		return gitv1.PushAuthorizeResponse{}, refuse(gitv1.CodeHostingMismatch, "only an internally hosted repository accepts pushes")
	case repo.Status == gitv1.StatusProvisioning:
		return gitv1.PushAuthorizeResponse{}, refuse(gitv1.CodeRepositoryNotReady, "the repository is not ready")
	}
	live, held, err := s.authority(ctx, repo.Scope, claims, req, repo.DefaultBranch)
	if err != nil {
		return gitv1.PushAuthorizeResponse{}, err
	}
	policy, err := Evaluate(PolicyInput{
		Repo:         repo.policy(),
		RepositoryID: repo.ID, Claims: claims, Request: req, AttemptLive: live, WriterHeld: held,
	})
	if err != nil {
		return gitv1.PushAuthorizeResponse{}, refuse(domain.CodeInvalidRequest, "%v", err)
	}
	entries, err := gitv1.NewPushEntries(req)
	if err != nil {
		return gitv1.PushAuthorizeResponse{}, refuse(domain.CodeInvalidRequest, "%v", err)
	}

	now := s.now()
	var created *Record
	err = s.d.Store.Do(ctx, tenant, func(ctx context.Context, tx Tx) error {
		// The facts the policy was evaluated under must still hold when the record
		// commits: the repository row is re-read and held in this transaction.
		if err := s.guardRepository(ctx, tx, repo); err != nil {
			return err
		}
		denials := append([]string(nil), policy.Denials...)
		if policy.SeedReceive {
			reason, err := s.consumeSeed(ctx, tx, repo, req, now)
			if err != nil {
				return err
			}
			if reason != "" {
				for i := range denials {
					denials[i] = reason
				}
			}
		}
		rec := &Record{
			ID: s.o.NewID("push"), Scope: repo.Scope, RepositoryID: repo.ID, ReceiveID: req.ReceiveID, GrantID: req.GrantID, GrantKind: claims.Kind,
			Actor: claims.Actor, PushOptions: append([]string{}, req.PushOptions...), RepositoryEmpty: req.GitFacts[0].RepositoryEmpty,
			RequestDigest: digest, Deadline: now.Add(s.o.ReceiveDeadline), Version: 1, CreatedAt: now, UpdatedAt: now,
		}
		denied := false
		for _, d := range denials {
			denied = denied || d != ""
		}
		for i, e := range entries {
			entry := Entry{Ref: e.Ref, Old: e.Old, New: e.New, Kind: e.Kind, State: gitv1.EntryAuthorized}
			switch {
			case denials[i] != "":
				entry.State, entry.Reason = gitv1.EntryRefused, denials[i]
			case denied:
				// The receive fails as a whole: an entry allowed on its own never
				// commits, so it is settled now and cannot hold a seed open.
				entry.State, entry.Reason = gitv1.EntryRefused, gitv1.ReasonNoCommitIntent
			}
			rec.Entries = append(rec.Entries, entry)
		}
		if err := tx.Insert(ctx, rec, authorizedEffects(rec)); err != nil {
			return err
		}
		created = rec
		return nil
	})
	if errors.Is(err, ErrConflict) {
		// A concurrent authorization of the same receive won: it is a replay.
		resp, found, rerr := s.replay(ctx, tenant, req.RepositoryID, req.ReceiveID, digest)
		if rerr != nil {
			return gitv1.PushAuthorizeResponse{}, rerr
		}
		if found {
			return resp, nil
		}
	}
	if err != nil {
		return gitv1.PushAuthorizeResponse{}, depErr(err, "push record")
	}
	// The writer reservation was read before the transaction; it can be released
	// (or the workspace archived) between that read and the commit of the record,
	// and the workspace cannot see an uncommitted record. Now that the record is
	// durable it is read again, and a reservation lost since refuses the record.
	created, err = s.recheckWriter(ctx, tenant, created, repo.DefaultBranch)
	if err != nil {
		return gitv1.PushAuthorizeResponse{}, err
	}
	return created.AuthorizeResponse(), nil
}

// reasonWriterLost is the reason recorded when a reservation was lost after the
// authorization was decided.
const reasonWriterLost = "the attempt no longer holds the writer reservation of the task branch"

// taskWorkItems returns the work items of the task branches among refs.
func taskWorkItems(defaultBranch string, refs []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, ref := range refs {
		c, err := gitv1.ClassifyRef(defaultBranch, ref)
		if err != nil || c.Class != gitv1.ClassTask || seen[c.WorkItemID] {
			continue
		}
		seen[c.WorkItemID] = true
		out = append(out, c.WorkItemID)
	}
	return out
}

// lostWriterRefs reads the writer reservation of an attempt's record again for
// the task-branch refs named, and returns those whose work item the attempt no
// longer holds. A read failure is a retryable outage (nothing is decided).
func (s *Service) lostWriterRefs(ctx context.Context, rec *Record, defaultBranch string, refs []string) (map[string]bool, error) {
	if rec.GrantKind != gitv1.GrantAttempt || rec.Actor.AttemptID == nil || s.d.Reservations == nil {
		return nil, nil
	}
	lost := map[string]bool{}
	for _, wi := range taskWorkItems(defaultBranch, refs) {
		ok, err := s.d.Reservations.HoldsWriter(ctx, rec.Scope, wi, *rec.Actor.AttemptID)
		if err != nil {
			return nil, depErr(err, "writer reservation")
		}
		if ok {
			continue
		}
		for _, ref := range refs {
			if c, err := gitv1.ClassifyRef(defaultBranch, ref); err == nil && c.Class == gitv1.ClassTask && c.WorkItemID == wi {
				lost[ref] = true
			}
		}
	}
	return lost, nil
}

// recheckWriter runs after the authorized record committed and before the
// verdicts reach the worker. For an attempt's task-branch updates that were
// allowed it reads the reservation again; a work item whose reservation was lost
// settles the record refused in its own transaction (the denied entries
// ref_protected, the others no_commit_intent, audited, no event) and the denial
// is what the worker is told. A read failure leaves the record authorized and is
// retryable; CommitIntent checks the reservation again, so a replay of the
// authorization cannot slip past it.
func (s *Service) recheckWriter(ctx context.Context, tenant domain.TenantID, rec *Record, defaultBranch string) (*Record, error) {
	var refs []string
	for _, e := range rec.Entries {
		if e.State == gitv1.EntryAuthorized {
			refs = append(refs, e.Ref)
		}
	}
	if len(refs) == 0 {
		return rec, nil
	}
	lost, err := s.lostWriterRefs(ctx, rec, defaultBranch, refs)
	if err != nil || len(lost) == 0 {
		return rec, err
	}
	var settled *Record
	err = s.d.Store.Do(ctx, tenant, func(ctx context.Context, tx Tx) error {
		cur, err := tx.ByID(ctx, rec.ID, true)
		if err != nil {
			return err
		}
		before := cur.Status()
		changed, err := cur.RefuseLostWriter(lost, s.now())
		if err != nil {
			return err
		}
		settled = cur
		if !changed {
			return nil
		}
		fx := settlementEffects(before, cur, SystemActor, nil)
		for i := range fx.Audit {
			fx.Audit[i].Reason = reasonWriterLost
		}
		return tx.Save(ctx, cur, fx)
	})
	if err != nil {
		return nil, mapStoreErr(err)
	}
	return settled, nil
}

// guardRepository compares the repository row, read inside the transaction with
// the row held, to the snapshot the policy used. A repository that was archived,
// given another default branch or another push policy in between is refused as
// retryable (unavailable): the worker retries the receive and the next
// authorization evaluates the fresh facts.
func (s *Service) guardRepository(ctx context.Context, tx Tx, repo RepositoryFacts) error {
	state, err := tx.RepositoryState(ctx, repo.ID)
	switch {
	case errors.Is(err, ErrNotFound):
		return notFound()
	case err != nil:
		return err
	case state != repo.State():
		return unavailable(errors.New("the repository's status or policy changed while the push was authorized"), "repository")
	}
	return nil
}

// policy projects the repository module's answer onto the policy's facts.
func (repo RepositoryFacts) policy() PolicyRepository {
	return PolicyRepository{DefaultBranch: repo.DefaultBranch, DirectPushToDefault: repo.DirectPushToDefault, Archived: repo.Status == gitv1.StatusArchived}
}

// consumeSeed runs inside the authorization transaction, before the record is
// inserted. It takes the repository's seed lock first, refuses (seed_consumed)
// when any other push record of the repository is unsettled, and then consumes
// the grant for this receive through gitauth.ConsumeSeedIn (idempotent for the
// same receive, refused for another). It returns the deny reason, or "" when
// the seed is consumed.
func (s *Service) consumeSeed(ctx context.Context, tx Tx, repo RepositoryFacts, req gitv1.PushAuthorizeRequest, now time.Time) (string, error) {
	if err := tx.Grants().LockSeed(ctx, repo.ID); err != nil {
		return "", err
	}
	busy, err := tx.HasUnsettled(ctx, repo.Scope, repo.ID, req.ReceiveID)
	if err != nil {
		return "", err
	}
	if busy {
		return gitv1.ReasonSeedConsumed, nil
	}
	switch err := gitauth.ConsumeSeedIn(ctx, tx.Grants(), req.GrantID, req.ReceiveID, now); {
	case err == nil:
		return "", nil
	case errors.Is(err, gitauth.ErrSeedConsumed):
		return gitv1.ReasonSeedConsumed, nil
	case errors.Is(err, gitauth.ErrGrantInactive):
		return "", refuse(domain.CodeInvalidCredential, "the credential is not valid")
	default:
		return "", err
	}
}

// authority reads, before the transaction, the current authority the policy
// needs beyond the grant: the grant's attempt liveness and, for an attempt's
// updates of task branches, whether it holds the work item's writer reservation.
func (s *Service) authority(ctx context.Context, scope domain.Scope, claims gitv1.GrantClaims, req gitv1.PushAuthorizeRequest, defaultBranch string) (bool, map[string]bool, error) {
	live := true
	if id := claims.Actor.AttemptID; id != nil {
		ok, err := s.d.Attempts.Live(ctx, scope, *id)
		switch {
		case errors.Is(err, ErrNotFound):
			ok = false
		case err != nil:
			return false, nil, depErr(err, "attempt")
		}
		live = ok
	}
	held := map[string]bool{}
	if !live || claims.Kind != gitv1.GrantAttempt || claims.Actor.AttemptID == nil || s.d.Reservations == nil {
		return live, held, nil
	}
	asked := map[string]bool{}
	for _, u := range req.Updates {
		ref, err := gitv1.ClassifyRef(defaultBranch, u.Ref)
		if err != nil || ref.Class != gitv1.ClassTask || asked[ref.WorkItemID] {
			continue
		}
		asked[ref.WorkItemID] = true
		ok, err := s.d.Reservations.HoldsWriter(ctx, scope, ref.WorkItemID, *claims.Actor.AttemptID)
		if err != nil {
			return false, nil, depErr(err, "writer reservation")
		}
		held[ref.WorkItemID] = ok
	}
	return live, held, nil
}

func (s *Service) load(ctx context.Context, tenant domain.TenantID, pushID, receiveID string) (*Record, error) {
	if err := domain.TenantID(tenant).Validate(); err != nil {
		return nil, notFound()
	}
	var rec *Record
	err := s.d.Store.View(ctx, tenant, func(ctx context.Context, tx Tx) (err error) {
		rec, err = tx.ByID(ctx, pushID, false)
		return err
	})
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, notFound()
	case err != nil:
		return nil, depErr(err, "push record")
	case rec.ReceiveID != receiveID:
		return nil, notFound()
	}
	return rec, nil
}

// CommitIntent moves the transaction's entries from authorized to committing
// when every listed entry is in flight, the receive deadline has not passed and
// the grant's attempt is live; otherwise push_settled, receive_expired or
// attempt_inactive (409) and the hook aborts; a new intent is also refused on an
// archived repository (repository_archived). Repeating it for entries already
// committing succeeds while those conditions hold.
//
// The conditions are decided in the intent's transaction, with every lock taken:
// the record, the attempt's grant row (a cancellation revokes the attempt's
// grants in its own transaction, so it serializes with this intent) and the
// repository row; the deadline is checked again after the last lock was
// acquired, immediately before the intent is saved, because a wait for a lock
// may have crossed it. The attempt's liveness and the record are also read
// before the transaction as a fast path that refuses a plainly doomed intent
// without taking any lock; no network call is made under a lock.
func (s *Service) CommitIntent(ctx context.Context, tenant domain.TenantID, pushID string, req gitv1.CommitIntentRequest) (gitv1.CommitIntentResponse, error) {
	if err := req.Validate(); err != nil {
		return gitv1.CommitIntentResponse{}, refuse(domain.CodeInvalidRequest, "%v", err)
	}
	rec, err := s.load(ctx, tenant, pushID, req.ReceiveID)
	if err != nil {
		return gitv1.CommitIntentResponse{}, err
	}
	if _, err := rec.Clone().CommitIntent(req.Updates, s.now()); err != nil {
		return gitv1.CommitIntentResponse{}, err
	}
	if id := rec.Actor.AttemptID; id != nil {
		live, err := s.d.Attempts.Live(ctx, rec.Scope, *id)
		switch {
		case errors.Is(err, ErrNotFound):
			live = false
		case err != nil:
			return gitv1.CommitIntentResponse{}, depErr(err, "attempt")
		}
		if !live {
			return gitv1.CommitIntentResponse{}, refuse(gitv1.CodeAttemptInactive, "the attempt of this push is no longer live")
		}
	}
	// The writer reservation is checked again before intent is given, outside any
	// lock like the liveness: a reservation released since the authorization
	// refuses the intent of a task-branch update (the authorization's own recheck
	// covers the window before the verdict; this covers the time after it).
	if err := s.checkWriter(ctx, tenant, rec, req.Updates); err != nil {
		return gitv1.CommitIntentResponse{}, err
	}
	err = s.d.Store.Do(ctx, tenant, func(ctx context.Context, tx Tx) error {
		cur, err := tx.ByID(ctx, pushID, true)
		if err != nil {
			return err
		}
		changed, err := cur.CommitIntent(req.Updates, s.now())
		if err != nil {
			return err
		}
		// The authoritative checks run in this transaction, after the record is
		// locked: a cancellation revokes the attempt's grants in its own
		// transaction (gitauth.RevokeAttemptIn), so locking the grant row here
		// serializes it with this intent: either the revocation committed first
		// and the intent is refused, or the intent commits first and the
		// transaction completes, as the specification allows.
		if err := s.checkAttempt(ctx, tx, cur); err != nil {
			return err
		}
		if !changed {
			return s.checkDeadline(cur)
		}
		// New commit intent is not given on an archived repository; a repeat for
		// entries that already hold it is not a new intent.
		state, err := tx.RepositoryState(ctx, cur.RepositoryID)
		switch {
		case errors.Is(err, ErrNotFound):
			return notFound()
		case err != nil:
			return err
		case state.Status == gitv1.StatusArchived:
			return refuse(gitv1.CodeRepositoryArchived, "the repository is archived")
		}
		// Waiting for the grant or the repository row may have crossed the
		// deadline: it is decided last, with every lock held.
		if err := s.checkDeadline(cur); err != nil {
			return err
		}
		return tx.Save(ctx, cur, Effects{})
	})
	if err != nil {
		return gitv1.CommitIntentResponse{}, mapStoreErr(err)
	}
	return gitv1.CommitIntentResponse{Committing: true}, nil
}

// checkWriter refuses a commit intent (ref_protected) for task-branch updates of
// an attempt that no longer holds their work item's writer reservation. An
// entry that already holds intent is not a new one and is not rechecked.
func (s *Service) checkWriter(ctx context.Context, tenant domain.TenantID, rec *Record, updates []gitv1.RefUpdate) error {
	if rec.Actor.AttemptID == nil || s.d.Reservations == nil {
		return nil
	}
	var refs []string
	for _, u := range updates {
		if i := rec.find(u.Ref); i >= 0 && rec.Entries[i].State == gitv1.EntryAuthorized {
			refs = append(refs, u.Ref)
		}
	}
	if len(refs) == 0 {
		return nil
	}
	repo, err := s.repository(ctx, tenant, rec.RepositoryID)
	if err != nil {
		return err
	}
	lost, err := s.lostWriterRefs(ctx, rec, repo.DefaultBranch, refs)
	if err != nil {
		return err
	}
	if len(lost) > 0 {
		return refuse(domain.CodeForbidden, "ref_protected: "+reasonWriterLost)
	}
	return nil
}

// checkDeadline refuses (receive_expired) once the receive deadline passed, by
// the service's clock.
func (s *Service) checkDeadline(rec *Record) error {
	if !s.now().Before(rec.Deadline) {
		return refuse(gitv1.CodeReceiveExpired, "the receive deadline passed")
	}
	return nil
}

// checkAttempt refuses (attempt_inactive) when the grant of an attempt's push is
// revoked or gone, reading and locking the grant row in the transaction.
func (s *Service) checkAttempt(ctx context.Context, tx Tx, rec *Record) error {
	if rec.Actor.AttemptID == nil {
		return nil
	}
	g, err := tx.Grants().GrantByID(ctx, rec.GrantID, true)
	switch {
	case errors.Is(err, gitauth.ErrNotFound):
		return refuse(gitv1.CodeAttemptInactive, "the attempt of this push is no longer live")
	case err != nil:
		return err
	case g.Revoked():
		return refuse(gitv1.CodeAttemptInactive, "the attempt of this push is no longer live")
	}
	return nil
}

// mapStoreErr keeps contract errors, maps a missing record to not_found and
// everything else to a retryable outage.
func mapStoreErr(err error) error {
	switch {
	case errors.Is(err, ErrNotFound):
		return notFound()
	case errors.Is(err, ErrUnavailable):
		return err
	}
	return depErr(err, "push record")
}

// Outcome records the post-receive report: the reported updates that reached
// committing become applied, entries that never reached committing become
// refused, and the record's status is derived. Audit rows and, when the record
// becomes applied, the one reference-only git.refs_updated event commit with the
// entries in the same transaction. It is idempotent: a repeated report changes
// nothing, and the event identity is fixed per record, so it is emitted once.
// An empty repository that this push made non-empty is reported to the
// repository module after the commit (and again by Reconcile if that fails).
func (s *Service) Outcome(ctx context.Context, tenant domain.TenantID, pushID string, req gitv1.OutcomeRequest) (gitv1.OutcomeResponse, error) {
	if err := req.Validate(); err != nil {
		return gitv1.OutcomeResponse{}, refuse(domain.CodeInvalidRequest, "%v", err)
	}
	if _, err := s.load(ctx, tenant, pushID, req.ReceiveID); err != nil {
		return gitv1.OutcomeResponse{}, err
	}
	var rec *Record
	err := s.d.Store.Do(ctx, tenant, func(ctx context.Context, tx Tx) error {
		cur, err := tx.ByID(ctx, pushID, true)
		if err != nil {
			return err
		}
		before := cur.Status()
		res, err := cur.ApplyOutcome(req.Applied, s.now())
		if err != nil {
			return err
		}
		rec = cur
		if !res.Changed {
			return nil
		}
		return tx.Save(ctx, cur, settlementEffects(before, cur, cur.pushActor(), res.Contradicted))
	})
	if err != nil {
		return gitv1.OutcomeResponse{}, mapStoreErr(err)
	}
	_ = s.flushEmptyReport(ctx, tenant, rec) // best effort: Reconcile repeats it from the record
	return gitv1.OutcomeResponse{PushID: rec.ID, Status: rec.Status()}, nil
}

// flushEmptyReport tells the repository module that an empty repository now has
// refs. It runs after the settlement committed and is best effort: a failure
// leaves the obligation recorded on the push record for Reconcile.
func (s *Service) flushEmptyReport(ctx context.Context, tenant domain.TenantID, rec *Record) error {
	if rec == nil || !rec.EmptyReportPending {
		return nil
	}
	if err := s.d.Repositories.ReportEmpty(context.WithoutCancel(ctx), tenant, rec.RepositoryID, false, rec.UpdatedAt); err != nil {
		return depErr(err, "repository emptiness report")
	}
	return s.d.Store.Do(ctx, tenant, func(ctx context.Context, tx Tx) error { return tx.ClearEmptyReport(ctx, rec.ID) })
}

// ---- reads and the ports other modules consume ----

func (s *Service) authorizeRead(ctx context.Context, principal string, scope domain.Scope, repositoryID string) error {
	if s.d.Reads == nil {
		return unavailable(ErrNotConfigured, "read authority")
	}
	err := s.d.Reads.AuthorizeRead(ctx, principal, scope, repositoryID)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrNotFound):
		return notFound()
	case errors.Is(err, ErrDenied):
		return refuse(domain.CodeForbidden, "the caller may not read push records")
	}
	return unavailable(err, "read authority")
}

// Get returns one push record of a repository of the scope (the reload of a
// git.refs_updated consumer, and GET .../pushes/{pushId}). A record of another
// repository, project or tenant is not_found.
func (s *Service) Get(ctx context.Context, principal string, scope domain.Scope, repositoryID, pushID string) (gitv1.PushRecord, error) {
	if err := s.authorizeRead(ctx, principal, scope, repositoryID); err != nil {
		return gitv1.PushRecord{}, err
	}
	return s.Reload(ctx, scope, repositoryID, pushID)
}

// Reload is Get for a consumer that already holds the scope's authority (a
// trigger admission reloading the record of an event): it applies the scope
// comparison only.
func (s *Service) Reload(ctx context.Context, scope domain.Scope, repositoryID, pushID string) (gitv1.PushRecord, error) {
	var rec *Record
	err := s.d.Store.View(ctx, scope.TenantID, func(ctx context.Context, tx Tx) (err error) {
		rec, err = tx.ByID(ctx, pushID, false)
		return err
	})
	switch {
	case errors.Is(err, ErrNotFound):
		return gitv1.PushRecord{}, notFound()
	case err != nil:
		return gitv1.PushRecord{}, depErr(err, "push record")
	case rec.Scope != scope || rec.RepositoryID != repositoryID:
		return gitv1.PushRecord{}, notFound()
	}
	return rec.DTO()
}

// ReloadByReceive is Reload for the record of a receive of the repository: the
// identity a git.refs_updated event carries (its resource ID and receiveId).
func (s *Service) ReloadByReceive(ctx context.Context, scope domain.Scope, repositoryID, receiveID string) (gitv1.PushRecord, error) {
	var rec *Record
	err := s.d.Store.View(ctx, scope.TenantID, func(ctx context.Context, tx Tx) (err error) {
		rec, err = tx.ByReceive(ctx, repositoryID, receiveID)
		return err
	})
	switch {
	case errors.Is(err, ErrNotFound):
		return gitv1.PushRecord{}, notFound()
	case err != nil:
		return gitv1.PushRecord{}, depErr(err, "push record")
	case rec.Scope != scope || rec.RepositoryID != repositoryID:
		return gitv1.PushRecord{}, notFound()
	}
	return rec.DTO()
}

// List returns one page of the repository's push records, newest first. The
// repository must belong to the scope.
func (s *Service) List(ctx context.Context, principal string, scope domain.Scope, repositoryID string, q ListQuery) ([]Page, error) {
	if err := s.authorizeRead(ctx, principal, scope, repositoryID); err != nil {
		return nil, err
	}
	repo, err := s.repository(ctx, scope.TenantID, repositoryID)
	if err != nil {
		return nil, err
	}
	if repo.Scope != scope {
		return nil, notFound()
	}
	var recs []*Record
	err = s.d.Store.View(ctx, scope.TenantID, func(ctx context.Context, tx Tx) (err error) {
		recs, err = tx.List(ctx, scope, repositoryID, q)
		return err
	})
	if err != nil {
		return nil, depErr(err, "push records")
	}
	out := make([]Page, 0, len(recs))
	for _, r := range recs {
		dto, err := r.DTO()
		if err != nil {
			return nil, err
		}
		out = append(out, Page{Record: dto, CreatedAt: r.CreatedAt})
	}
	return out, nil
}

// Page is one listed record with the creation time its continuation position
// uses.
type Page struct {
	Record    gitv1.PushRecord
	CreatedAt time.Time
}

// UnsettledPush reports whether the repository has any unsettled push record of
// any kind: a receive in flight or an unknown outcome (gitv1.UnsettledPush). It
// implements gitauth.Pushes, so no seed grant is issued while one exists.
func (s *Service) UnsettledPush(ctx context.Context, scope domain.Scope, repositoryID string) (bool, error) {
	var busy bool
	err := s.d.Store.View(ctx, scope.TenantID, func(ctx context.Context, tx Tx) (err error) {
		busy, err = tx.HasUnsettled(ctx, scope, repositoryID, "")
		return err
	})
	if err != nil {
		return false, depErr(err, "push records")
	}
	return busy, nil
}

// UnknownBlocksWriter implements the WriterGate of the contract: it reports
// whether an entry for branchRef (the work item's task branch, or the run's
// branch) is unknown, in which case the writer attempt must not be reassigned
// until an operator resolves it (WS-11).
func (s *Service) UnknownBlocksWriter(ctx context.Context, scope domain.Scope, repositoryID, branchRef string) (bool, error) {
	if err := gitv1.ValidateRefName(branchRef); err != nil {
		return false, refuse(domain.CodeInvalidRequest, "%v", err)
	}
	var blocked bool
	err := s.d.Store.View(ctx, scope.TenantID, func(ctx context.Context, tx Tx) (err error) {
		blocked, err = tx.HasUnknownEntry(ctx, scope, repositoryID, branchRef)
		return err
	})
	if err != nil {
		return false, depErr(err, "push records")
	}
	return blocked, nil
}

// MaxAppliedOnRef bounds AppliedOnRef's limit.
const MaxAppliedOnRef = 100

// UnsettledOnRef reports whether any push record of the repository has an entry
// for ref that is authorized, committing or unknown: a push to the ref is in
// flight or its outcome is not known (the workspace's RefPushes). A foreign or
// unknown repository is not_found.
func (s *Service) UnsettledOnRef(ctx context.Context, scope domain.Scope, repositoryID, ref string) (bool, error) {
	if err := s.refQuery(ctx, scope, repositoryID, ref); err != nil {
		return false, err
	}
	var busy bool
	err := s.d.Store.View(ctx, scope.TenantID, func(ctx context.Context, tx Tx) (err error) {
		busy, err = tx.UnsettledOnRef(ctx, scope, repositoryID, ref)
		return err
	})
	if err != nil {
		return false, depErr(err, "push records")
	}
	return busy, nil
}

// AppliedOnRef returns up to limit push records of the repository that have an
// applied entry for ref, newest first by the order in which the entry for ref
// applied (persisted per entry; the workspace's
// RefPushes). The limit is 1 to MaxAppliedOnRef. A foreign or unknown
// repository is not_found.
func (s *Service) AppliedOnRef(ctx context.Context, scope domain.Scope, repositoryID, ref string, limit int) ([]gitv1.PushRecord, error) {
	if limit < 1 || limit > MaxAppliedOnRef {
		return nil, refuse(domain.CodeInvalidRequest, "the limit is 1 to %d", MaxAppliedOnRef)
	}
	if err := s.refQuery(ctx, scope, repositoryID, ref); err != nil {
		return nil, err
	}
	var recs []*Record
	err := s.d.Store.View(ctx, scope.TenantID, func(ctx context.Context, tx Tx) (err error) {
		recs, err = tx.AppliedOnRef(ctx, scope, repositoryID, ref, limit)
		return err
	})
	if err != nil {
		return nil, depErr(err, "push records")
	}
	out := make([]gitv1.PushRecord, 0, len(recs))
	for _, r := range recs {
		dto, err := r.DTO()
		if err != nil {
			return nil, err
		}
		out = append(out, dto)
	}
	return out, nil
}

func (s *Service) refQuery(ctx context.Context, scope domain.Scope, repositoryID, ref string) error {
	if err := gitv1.ValidateRefName(ref); err != nil {
		return refuse(domain.CodeInvalidRequest, "%v", err)
	}
	repo, err := s.repository(ctx, scope.TenantID, repositoryID)
	if err != nil {
		return err
	}
	if repo.Scope != scope {
		return notFound()
	}
	return nil
}
