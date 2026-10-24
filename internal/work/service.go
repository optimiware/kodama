package work

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// MaxPageRows bounds the rows one list or history read may request (callers ask
// for page size + 1).
const MaxPageRows = 1001

// Config wires a Service. Repository and Authority are required: there is no
// default that grants access.
type Config struct {
	Repository Repository
	Authority  Authority
	// Refresher is optional; it runs before review-related transitions, outside
	// any transaction.
	Refresher SubjectRefresher
	Clock     func() time.Time
	NewID     func(prefix string) (string, error)
}

// Service holds the work-management use cases. It authorizes the scope before
// persistence and delegates locking, version comparison and the atomic
// work/history/audit/outbox write to the Repository.
type Service struct {
	repo      Repository
	authority Authority
	refresher SubjectRefresher
	now       func() time.Time
	newID     func(string) (string, error)
}

// NewService validates cfg.
func NewService(cfg Config) (*Service, error) {
	if cfg.Repository == nil || cfg.Authority == nil {
		return nil, errors.New("work: a repository and an authority are required")
	}
	s := &Service{repo: cfg.Repository, authority: cfg.Authority, refresher: cfg.Refresher, now: cfg.Clock, newID: cfg.NewID}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.newID == nil {
		s.newID = RandomID
	}
	return s, nil
}

// authorize resolves the caller and checks the permissions, requiring all of
// them. It runs before any persistence.
func (s *Service) authorize(ctx context.Context, scope domain.Scope, caller Caller, perms ...Permission) (Actor, error) {
	if err := scope.Validate(); err != nil {
		return Actor{}, invalid("%v", err)
	}
	if err := caller.validate(); err != nil {
		return Actor{}, err
	}
	actor, err := s.authority.ResolveActor(ctx, scope, caller.PrincipalID)
	if err != nil {
		return Actor{}, authorityError(err, "actor")
	}
	if err := actor.Validate(); err != nil {
		return Actor{}, Unavailablef(err, "the authority returned an unusable actor")
	}
	for _, p := range perms {
		if err := s.authority.Authorize(ctx, actor, scope, p); err != nil {
			return Actor{}, authorityError(err, "permission")
		}
	}
	return actor, nil
}

// authorizeAny requires at least one of the permissions.
func (s *Service) authorizeAny(ctx context.Context, actor Actor, scope domain.Scope, perms []Permission) error {
	var last error = fmt.Errorf("%w: no permission grants this operation", ErrDenied)
	for _, p := range perms {
		err := s.authority.Authorize(ctx, actor, scope, p)
		if err == nil {
			return nil
		}
		err = authorityError(err, "permission")
		if !errors.Is(err, ErrDenied) && !errors.Is(err, ErrHidden) {
			return err
		}
		last = err
	}
	return last
}

func authorityError(err error, what string) error {
	if errors.Is(err, ErrDenied) || errors.Is(err, ErrHidden) {
		return err
	}
	return Unavailablef(err, "%s authority failed", what)
}

func (s *Service) historyID() (string, error) {
	id, err := s.newID("wih_")
	if err != nil {
		return "", Unavailablef(err, "identifier generation failed")
	}
	return id, nil
}

// Create creates a work item in backlog at version 1, content revision 1.
func (s *Service) Create(ctx context.Context, cmd CreateCommand) (Item, error) {
	perms := []Permission{PermCreate}
	if cmd.AssigneeID != "" {
		perms = append(perms, PermEditTechnical)
	}
	actor, err := s.authorize(ctx, cmd.Scope, cmd.Caller, perms...)
	if err != nil {
		return Item{}, err
	}
	id, err := s.newID("wi_")
	if err != nil {
		return Item{}, Unavailablef(err, "identifier generation failed")
	}
	hid, err := s.historyID()
	if err != nil {
		return Item{}, err
	}
	params := NewItemParams{
		Scope: cmd.Scope, ID: domain.WorkItemID(id), Kind: cmd.Kind, Title: cmd.Title, Spec: cmd.Spec,
		Criteria: cmd.Criteria, EvidenceLinks: cmd.EvidenceLinks, AssigneeID: cmd.AssigneeID, ParentID: cmd.ParentID,
		Actor: actor, Now: s.now(), HistoryID: hid, RequestID: cmd.Caller.RequestID,
	}
	// Validate before opening a transaction.
	if _, _, err := NewItem(params); err != nil {
		return Item{}, err
	}
	return s.repo.Insert(ctx, cmd.Scope, func(ctx context.Context, tx Tx) (Item, HistoryEntry, error) {
		for _, p := range perms {
			if err := tx.Authorize(ctx, actor, cmd.Scope, p); err != nil {
				return Item{}, HistoryEntry{}, err
			}
		}
		if cmd.AssigneeID != "" {
			if err := tx.AssigneeEligible(ctx, cmd.Scope, cmd.AssigneeID); err != nil {
				return Item{}, HistoryEntry{}, err
			}
		}
		if cmd.ParentID != "" {
			if err := tx.ParentEligible(ctx, cmd.Scope, params.ID, cmd.ParentID); err != nil {
				return Item{}, HistoryEntry{}, err
			}
		}
		if len(cmd.EvidenceLinks) > 0 {
			if err := tx.VerifyEvidence(ctx, cmd.Scope, cmd.EvidenceLinks); err != nil {
				return Item{}, HistoryEntry{}, err
			}
		}
		params.Now = s.now()
		return NewItem(params)
	})
}

// Get returns one item.
func (s *Service) Get(ctx context.Context, scope domain.Scope, caller Caller, id domain.WorkItemID) (Item, error) {
	if _, err := s.authorize(ctx, scope, caller, PermRead); err != nil {
		return Item{}, err
	}
	if err := id.Validate(); err != nil {
		return Item{}, invalid("%v", err)
	}
	return s.repo.Get(ctx, scope, id)
}

// List returns one page of items. Authorization applies on every page.
func (s *Service) List(ctx context.Context, scope domain.Scope, caller Caller, q ListQuery) ([]Item, error) {
	if _, err := s.authorize(ctx, scope, caller, PermRead); err != nil {
		return nil, err
	}
	if q.Limit < 1 || q.Limit > MaxPageRows {
		return nil, invalid("limit must be between 1 and %d", MaxPageRows)
	}
	for _, st := range q.States {
		if !st.Valid() {
			return nil, invalid("unknown state filter %q", string(st))
		}
	}
	for _, k := range q.Kinds {
		if !k.Valid() {
			return nil, invalid("unknown kind filter %q", string(k))
		}
	}
	return s.repo.List(ctx, scope, q)
}

// History returns one page of an item's history, ordered by version.
func (s *Service) History(ctx context.Context, scope domain.Scope, caller Caller, id domain.WorkItemID, q HistoryQuery) ([]HistoryEntry, error) {
	if _, err := s.authorize(ctx, scope, caller, PermRead); err != nil {
		return nil, err
	}
	if err := id.Validate(); err != nil {
		return nil, invalid("%v", err)
	}
	if q.Limit < 1 || q.Limit > MaxPageRows {
		return nil, invalid("limit must be between 1 and %d", MaxPageRows)
	}
	return s.repo.History(ctx, scope, id, q)
}

// Edit applies a patch. Field-sensitive permissions are required: analysts edit
// intent, engineers technical fields.
func (s *Service) Edit(ctx context.Context, cmd EditCommand) (Item, error) {
	perms := cmd.Patch.RequiredPermissions()
	if len(perms) == 0 {
		return Item{}, invalid("the patch names no field")
	}
	actor, err := s.authorize(ctx, cmd.Scope, cmd.Caller, perms...)
	if err != nil {
		return Item{}, err
	}
	if err := cmd.ID.Validate(); err != nil {
		return Item{}, invalid("%v", err)
	}
	if err := cmd.Expected.Validate(); err != nil {
		return Item{}, invalid("%v", err)
	}
	hid, err := s.historyID()
	if err != nil {
		return Item{}, err
	}
	return s.repo.Update(ctx, cmd.Scope, cmd.ID, cmd.Expected, UpdateOptions{LockProject: cmd.Patch.Parent.Set},
		func(ctx context.Context, tx Tx, cur Item) (Item, HistoryEntry, error) {
			for _, p := range perms {
				if err := tx.Authorize(ctx, actor, cmd.Scope, p); err != nil {
					return Item{}, HistoryEntry{}, err
				}
			}
			// Cheap entity validation first, so a malformed patch never touches guards.
			next, h, err := cur.ApplyPatch(cmd.Patch, actor, s.now(), hid, cmd.Caller.RequestID)
			if err != nil {
				return Item{}, HistoryEntry{}, err
			}
			if cmd.Patch.Assignee.Set && cmd.Patch.Assignee.Value != nil && *cmd.Patch.Assignee.Value != cur.AssigneeID {
				if err := tx.AssigneeEligible(ctx, cmd.Scope, *cmd.Patch.Assignee.Value); err != nil {
					return Item{}, HistoryEntry{}, err
				}
			}
			if cmd.Patch.Parent.Set && cmd.Patch.Parent.Value != nil && *cmd.Patch.Parent.Value != cur.ParentID {
				if err := tx.ParentEligible(ctx, cmd.Scope, cur.ID, *cmd.Patch.Parent.Value); err != nil {
					return Item{}, HistoryEntry{}, err
				}
			}
			if cmd.Patch.EvidenceLinks != nil && len(*cmd.Patch.EvidenceLinks) > 0 {
				if err := tx.VerifyEvidence(ctx, cmd.Scope, *cmd.Patch.EvidenceLinks); err != nil {
					return Item{}, HistoryEntry{}, err
				}
			}
			return next, h, nil
		})
}

// Transition applies an explicit business transition. The edge, the exact
// permission, the expected version and every prerequisite are evaluated inside
// the mutating transaction; this method only performs the early authorization
// and the pre-transaction refresh.
func (s *Service) Transition(ctx context.Context, cmd TransitionCommand) (Item, error) {
	if !cmd.Request.To.Valid() {
		return Item{}, invalid("unknown target state %q", string(cmd.Request.To))
	}
	actor, err := s.authorize(ctx, cmd.Scope, cmd.Caller)
	if err != nil {
		return Item{}, err
	}
	if err := s.authorizeAny(ctx, actor, cmd.Scope, PermissionsForTarget(cmd.Request.To)); err != nil {
		return Item{}, err
	}
	if cmd.Request.To == StateDone && !actor.IsHuman() {
		return Item{}, fmt.Errorf("%w: completion is recorded by a human with %s", ErrNotHuman, PermComplete)
	}
	if err := cmd.ID.Validate(); err != nil {
		return Item{}, invalid("%v", err)
	}
	if err := cmd.Expected.Validate(); err != nil {
		return Item{}, invalid("%v", err)
	}
	if err := validateReason(cmd.Request.Reason); err != nil {
		return Item{}, err
	}
	hid, err := s.historyID()
	if err != nil {
		return Item{}, err
	}
	if s.refresher != nil && (cmd.Request.To == StateReview || cmd.Request.To == StateDone) {
		// Network work happens before, never inside, the SQL transaction.
		if err := s.refresher.RefreshSubject(ctx, cmd.Scope, cmd.ID); err != nil {
			return Item{}, Unavailablef(err, "the review subject could not be refreshed")
		}
	}
	return s.repo.Update(ctx, cmd.Scope, cmd.ID, cmd.Expected, UpdateOptions{LockProject: true},
		func(ctx context.Context, tx Tx, cur Item) (Item, HistoryEntry, error) {
			return cur.Transition(ctx, tx, cmd.Request, actor, s.now(), hid, cmd.Caller.RequestID)
		})
}
