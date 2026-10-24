package adapters

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/qa"
	"github.com/optimiware/kodama/internal/work"
	workpg "github.com/optimiware/kodama/internal/work/postgres"
)

// qaPermissions is everything the QA mapping principal may do in the work
// module: read the item under test, create a defect and link evidence to it.
// There is deliberately no transition, completion, reopen, cancellation,
// refinement, assignment or blocking permission: QA produces evidence and
// defects and never decides (DQ-06, AR-12, WM-07).
var qaPermissions = map[work.Permission]bool{
	work.PermRead:     true,
	work.PermCreate:   true,
	work.PermEditSpec: true,
}

// narrowed restricts a work.Authority to qaPermissions. The inner authority is
// the work module's real authority (an identity-backed bridge in production):
// narrowing never widens it, so a permission is held only when both the QA
// ceiling and the current owner decision allow it.
type narrowed struct{ inner work.Authority }

func (n narrowed) ResolveActor(ctx context.Context, scope domain.Scope, principal string) (work.Actor, error) {
	return n.inner.ResolveActor(ctx, scope, principal)
}

func (n narrowed) Authorize(ctx context.Context, actor work.Actor, scope domain.Scope, perm work.Permission) error {
	if !qaPermissions[perm] {
		return fmt.Errorf("%w: the QA mapping principal does not hold %s", work.ErrDenied, perm)
	}
	return n.inner.Authorize(ctx, actor, scope, perm)
}

// txPermissions is the transaction-bound permission check of the QA work store.
// The QA store itself refuses everything but read, create and evidence-link
// permissions, and refuses a workload actor outright (mapping is a control-plane
// operation, never a runtime action); the remaining decision is the owner's
// current one, read inside the mutating transaction by the injected checker, so
// a mapping authority revoked while the mutation waited is seen. Without an
// injected checker the store has no permission guard and every mutation fails
// closed.
type txPermissions struct{ inner work.PermissionChecker }

func (t txPermissions) Authorize(ctx context.Context, actor work.Actor, scope domain.Scope, perm work.Permission) error {
	if actor.Type == work.ActorWorkload || !qaPermissions[perm] {
		return fmt.Errorf("%w: the QA work store admits only the mapping principal's defect filing", work.ErrDenied)
	}
	return t.inner.Authorize(ctx, actor, scope, perm)
}

// evidenceGuard is the work.SubjectGuard of the QA work store: evidence links
// are artifacts of the scope that are usable now; commits and review subjects
// are not QA's to link or verify, so they fail closed.
type evidenceGuard struct{ repo artifacts.Repository }

func (g evidenceGuard) VerifySubject(context.Context, domain.Scope, domain.WorkItemID, work.ReviewClaim) (work.VerifiedSubject, error) {
	return work.VerifiedSubject{}, fmt.Errorf("%w: the QA work store verifies no review subject", work.ErrDenied)
}

func (g evidenceGuard) VerifyEvidence(ctx context.Context, scope domain.Scope, refs []work.EvidenceRef) error {
	for _, r := range refs {
		if r.Type != work.EvidenceArtifact {
			return work.Invalidf("QA defects link artifacts only")
		}
		a, err := g.repo.Get(ctx, scope, domain.ArtifactID(r.ID))
		if err != nil {
			if errors.Is(err, artifacts.ErrNotFound) {
				return fmt.Errorf("%w: an evidence artifact was not found", work.ErrNotFound)
			}
			return work.Unavailablef(err, "the artifact store is unavailable")
		}
		if !a.Usable() {
			return fmt.Errorf("%w: an evidence artifact is not usable", work.ErrNotFound)
		}
	}
	return nil
}

// WorkGuards is the guard binder of the QA work store (workpg.NewStore): the
// narrowed, transaction-bound permission check and the artifact-backed evidence
// guard. permissions binds the work module's owner authority to the mutating
// transaction (identity's TxPermissions in production); nil leaves the store
// without a permission guard, so every mutation fails closed. The writer and
// dependency guards are left unset, so every transition fails closed.
func WorkGuards(repo artifacts.Repository, permissions func(pgx.Tx) work.PermissionChecker) workpg.GuardBinder {
	return func(tx pgx.Tx) work.TxGuards {
		g := work.TxGuards{Subjects: evidenceGuard{repo: repo}}
		if permissions != nil {
			if inner := permissions(tx); inner != nil {
				g.Permissions = txPermissions{inner: inner}
			}
		}
		return g
	}
}

// Work implements qa.Defects over a work.Repository that is the QA work store
// (built with WorkGuards). It creates defects through an ordinary work.Service
// per call, acting as one configured mapping principal whose authority is the
// work module's own current decision (never a permission built here from the
// attempt or the report), narrowed to read, create and evidence links.
type Work struct {
	repo      work.Repository
	authority work.Authority
	principal string
	clock     func() time.Time
}

var _ qa.Defects = (*Work)(nil)

// NewWork requires the QA work store, the work module's authority and the
// verified principal ID the mapping runs as (a service principal bound in the
// project with exactly the rights to read and file defects; revoking that
// binding stops all mapping at the next call and the next mutation). Nothing
// defaults: a missing collaborator or principal fails construction. A nil clock
// selects UTC wall time.
func NewWork(repo work.Repository, authority work.Authority, principal string, clock func() time.Time) (*Work, error) {
	switch {
	case repo == nil:
		return nil, errors.New("qa/adapters: a work repository is required")
	case authority == nil:
		return nil, errors.New("qa/adapters: the work authority of the mapping principal is required")
	case principal == "" || len(principal) > 256:
		return nil, errors.New("qa/adapters: the mapping principal is required")
	}
	if clock == nil {
		clock = nowUTC
	}
	return &Work{repo: repo, authority: narrowed{inner: authority}, principal: principal, clock: clock}, nil
}

// authorize verifies the mapping principal's current authority for everything a
// mapping does in the scope. It runs before any read of defect data or write, so
// a missing or revoked authority is a denial that touches nothing.
func (w *Work) authorize(ctx context.Context, scope domain.Scope) error {
	actor, err := w.authority.ResolveActor(ctx, scope, w.principal)
	if err != nil {
		return mapWork(err)
	}
	for _, p := range []work.Permission{work.PermRead, work.PermCreate, work.PermEditSpec} {
		if err := w.authority.Authorize(ctx, actor, scope, p); err != nil {
			return mapWork(err)
		}
	}
	return nil
}

func (w *Work) service(fixed domain.WorkItemID) (*work.Service, error) {
	return work.NewService(work.Config{
		Repository: w.repo, Authority: w.authority, Clock: w.clock,
		NewID: func(prefix string) (string, error) {
			if prefix == "wi_" && fixed != "" {
				return string(fixed), nil
			}
			return work.RandomID(prefix)
		},
	})
}

// mapWork converts the work module's classes. Business refusals keep their
// meaning; an unknown failure and an unknown commit outcome are unavailable.
func mapWork(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, work.ErrNotFound):
		return qa.Failf(qa.ErrNotFound, "work item not found")
	case errors.Is(err, work.ErrInvalid):
		return qa.Failf(qa.ErrInvalid, "the defect is invalid")
	case errors.Is(err, work.ErrDenied), errors.Is(err, work.ErrHidden), errors.Is(err, work.ErrNotHuman):
		return qa.Failf(qa.ErrDenied, "the mapping authority does not allow that")
	}
	return qa.Unavailablef(err, "the work store could not complete the request")
}

// Subject implements qa.Defects.
func (w *Work) Subject(ctx context.Context, scope domain.Scope, id domain.WorkItemID) (qa.SubjectFacts, error) {
	if err := w.authorize(ctx, scope); err != nil {
		return qa.SubjectFacts{}, err
	}
	it, err := w.repo.Get(ctx, scope, id)
	if err != nil {
		return qa.SubjectFacts{}, mapWork(err)
	}
	return qa.SubjectFacts{ID: it.ID, ContentRevision: it.ContentRevision, Cancelled: it.State == work.StateCancelled}, nil
}

func defectOf(it work.Item) qa.Defect {
	d := qa.Defect{ID: it.ID, Version: it.Version, State: string(it.State), Terminal: !it.State.Unfinished()}
	for _, l := range it.EvidenceLinks {
		if l.Type == work.EvidenceArtifact {
			d.Links = append(d.Links, domain.ArtifactID(l.ID))
		}
	}
	return d
}

// existing reads the item under the draft's identity and requires it to be a
// defect: another kind of record under this identity is a conflict. Identities
// are server-generated (a human cannot choose one) and this one is derived from
// a cryptographic key, so the kind check is the whole collision guard; the
// specification is deliberately not consulted, because a person may edit it
// without breaking deduplication.
func (w *Work) existing(ctx context.Context, scope domain.Scope, id domain.WorkItemID) (work.Item, bool, error) {
	it, err := w.repo.Get(ctx, scope, id)
	if err != nil {
		if errors.Is(err, work.ErrNotFound) {
			return work.Item{}, false, nil
		}
		return work.Item{}, false, mapWork(err)
	}
	if it.Kind != work.KindDefect {
		return work.Item{}, false, qa.Failf(qa.ErrConflict, "the defect identity is held by another record")
	}
	return it, true, nil
}

func evidenceRefs(links []qa.EvidenceLink) []work.EvidenceRef {
	out := make([]work.EvidenceRef, 0, len(links))
	for _, l := range links {
		out = append(out, work.EvidenceRef{Type: work.EvidenceArtifact, ID: string(l.ArtifactID), Note: l.Note})
	}
	return out
}

// samePayload compares the finding payload recorded in the defect's immutable
// creation record (the first work history entry) with the draft's. The current
// specification is deliberately not consulted: a person may edit it, but the
// creation record cannot be edited. A defect whose creation record carries no
// payload marker, or another one, is a conflict: the identity is held by
// something that was not created from this claim.
func (w *Work) samePayload(ctx context.Context, scope domain.Scope, id domain.WorkItemID, payload string) error {
	conflict := func() error {
		return qa.Failf(qa.ErrConflict, "the defect identity is held by a different finding payload")
	}
	hist, err := w.repo.History(ctx, scope, id, work.HistoryQuery{Limit: 1})
	if err != nil {
		if errors.Is(err, work.ErrNotFound) {
			return qa.Failf(qa.ErrNotFound, "defect not found")
		}
		return mapWork(err)
	}
	if len(hist) == 0 || hist[0].Kind != work.ChangeCreated || hist[0].Version != 1 {
		return conflict()
	}
	spec, _ := hist[0].After[work.FieldSpec].(string)
	got, ok := qa.ParsePayloadMarker(spec)
	if !ok || payload == "" || got != payload {
		return conflict()
	}
	return nil
}

// Verify implements qa.Defects: an absent identity is free; an existing one must
// be a defect created from the same finding payload.
func (w *Work) Verify(ctx context.Context, d qa.DefectDraft) error {
	if err := w.authorize(ctx, d.Scope); err != nil {
		return err
	}
	_, ok, err := w.existing(ctx, d.Scope, d.ID)
	if err != nil || !ok {
		return err
	}
	return w.samePayload(ctx, d.Scope, d.ID, d.Payload)
}

// observe returns the existing defect under the draft's identity after the
// payload comparison.
func (w *Work) observe(ctx context.Context, d qa.DefectDraft) (qa.Defect, bool, error) {
	it, ok, err := w.existing(ctx, d.Scope, d.ID)
	if err != nil || !ok {
		return qa.Defect{}, false, err
	}
	if err := w.samePayload(ctx, d.Scope, d.ID, d.Payload); err != nil {
		return qa.Defect{}, false, err
	}
	return defectOf(it), true, nil
}

// Open implements qa.Defects. It creates the defect under the deterministic
// identity or observes the existing one, whose creation record must carry the
// same finding payload (a changed payload is ErrConflict and writes nothing).
// Any creation failure is followed by a reread: a concurrent creator's commit,
// and a commit whose acknowledgment was lost, both end as "exists" instead of a
// duplicate or a false failure; a rolled-back attempt leaves nothing and
// reports its failure.
func (w *Work) Open(ctx context.Context, d qa.DefectDraft) (qa.Defect, bool, error) {
	if err := w.authorize(ctx, d.Scope); err != nil {
		return qa.Defect{}, false, err
	}
	if cur, ok, err := w.observe(ctx, d); err != nil || ok {
		return cur, false, err
	}
	svc, err := w.service(d.ID)
	if err != nil {
		return qa.Defect{}, false, qa.Unavailablef(err, "the work service could not be built")
	}
	criteria := make([]work.Criterion, 0, len(d.Criteria))
	for _, c := range d.Criteria {
		criteria = append(criteria, work.Criterion{ID: c.ID, Text: c.Text})
	}
	it, createErr := svc.Create(ctx, work.CreateCommand{
		Scope: d.Scope, Caller: work.Caller{PrincipalID: w.principal, RequestID: d.RequestID},
		Kind: work.KindDefect, Title: d.Title, Spec: d.Spec, Criteria: criteria, EvidenceLinks: evidenceRefs(d.Evidence),
	})
	if createErr == nil {
		return defectOf(it), true, nil
	}
	if cur, ok, err := w.observe(ctx, d); err != nil {
		return qa.Defect{}, false, err
	} else if ok {
		return cur, false, nil
	}
	if errors.Is(createErr, work.ErrNotFound) {
		return qa.Defect{}, false, qa.Failf(qa.ErrEvidence, "an evidence artifact is not available")
	}
	return qa.Defect{}, false, mapWork(createErr)
}

// maxAttachTries bounds the optimistic retries of an evidence merge.
const maxAttachTries = 3

// Attach implements qa.Defects: it appends the links the defect lacks, up to the
// work item's link limit, under the item's expected version; a concurrent edit
// is reread and merged. A terminal defect is returned unchanged.
func (w *Work) Attach(ctx context.Context, scope domain.Scope, _ domain.AttemptID, requestID string, id domain.WorkItemID, links []qa.EvidenceLink) (qa.Defect, int, error) {
	if err := w.authorize(ctx, scope); err != nil {
		return qa.Defect{}, 0, err
	}
	svc, err := w.service("")
	if err != nil {
		return qa.Defect{}, 0, qa.Unavailablef(err, "the work service could not be built")
	}
	for try := 0; try < maxAttachTries; try++ {
		it, ok, err := w.existing(ctx, scope, id)
		if err != nil {
			return qa.Defect{}, 0, err
		}
		if !ok {
			return qa.Defect{}, 0, qa.Failf(qa.ErrNotFound, "defect not found")
		}
		if !it.State.Unfinished() {
			return defectOf(it), 0, nil
		}
		have := make(map[string]struct{}, len(it.EvidenceLinks))
		for _, l := range it.EvidenceLinks {
			have[string(l.Type)+"|"+l.ID] = struct{}{}
		}
		merged := append([]work.EvidenceRef{}, it.EvidenceLinks...)
		added := 0
		for _, l := range evidenceRefs(links) {
			if _, dup := have[string(l.Type)+"|"+l.ID]; dup || len(merged) >= work.MaxEvidenceLinks {
				continue
			}
			merged = append(merged, l)
			added++
		}
		if added == 0 {
			return defectOf(it), 0, nil
		}
		next, err := svc.Edit(ctx, work.EditCommand{
			Scope: scope, Caller: work.Caller{PrincipalID: w.principal, RequestID: requestID},
			ID: id, Expected: it.Version, Patch: work.Patch{EvidenceLinks: &merged},
		})
		switch {
		case err == nil:
			return defectOf(next), added, nil
		case errors.Is(err, work.ErrVersionConflict):
			continue
		case errors.Is(err, work.ErrInvalidTransition):
			// The defect reached a terminal state between the read and the write.
			cur, _, rerr := w.existing(ctx, scope, id)
			if rerr != nil {
				return qa.Defect{}, 0, rerr
			}
			return defectOf(cur), 0, nil
		case errors.Is(err, work.ErrNotFound):
			return qa.Defect{}, 0, qa.Failf(qa.ErrEvidence, "an evidence artifact is not available")
		}
		return qa.Defect{}, 0, mapWork(err)
	}
	return qa.Defect{}, 0, qa.Unavailablef(nil, "the defect kept changing; repeat the request")
}
