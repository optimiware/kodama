package artifactstest

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/memdb"
)

type artifactRec struct{ A artifacts.Artifact }

type objectRec struct {
	Project         domain.ProjectID
	ArtifactID      domain.ArtifactID
	Key             string
	Purpose         string // "staging" or "candidate"
	Generation      int64
	State           string // "live", "delete_pending", "deleted"
	Winner          bool
	WritableUntil   time.Time
	CreatedAt       time.Time
	DeleteRequested time.Time
	DeletedAt       time.Time
	DeleteToken     string
	DeleteUntil     time.Time
	DeleteAttempts  int
}

type pinRec struct {
	Project  domain.ProjectID
	Artifact domain.ArtifactID
	Owner    artifacts.PinOwner
}

type auditRec struct {
	ID       string
	Project  domain.ProjectID
	Artifact domain.ArtifactID
	Action   string
	Actor    artifacts.Actor
	Version  domain.Version
}

// The primary keys carry the project, as PostgreSQL's (tenant_id, project_id,
// id): the same artifact ID may exist in two projects of a tenant, and every
// object, pin and audit lookup names the project of its artifact.
func akey(project domain.ProjectID, id domain.ArtifactID) string {
	return string(project) + "/" + string(id)
}

func okey(project domain.ProjectID, id domain.ArtifactID, key string) string {
	return akey(project, id) + "|" + key
}

func pkey(project domain.ProjectID, id domain.ArtifactID, o artifacts.PinOwner) string {
	return akey(project, id) + "|" + o.Kind + "|" + o.ID
}

var (
	artifactsT = memdb.NewTable("artifacts",
		func(r artifactRec) string { return akey(r.A.Scope().ProjectID, r.A.ID()) },
		func(r artifactRec) string {
			return string(r.A.Scope().ProjectID) + "/" + string(r.A.Attempt()) + "/" + r.A.Name()
		})
	objectsT = memdb.NewTable("artifact_objects", func(r objectRec) string { return okey(r.Project, r.ArtifactID, r.Key) })
	pinsT    = memdb.NewTable("artifact_pins", func(r pinRec) string { return pkey(r.Project, r.Artifact, r.Owner) })
	auditT   = memdb.NewTable("artifact_audit", func(r auditRec) string { return r.ID })
)

// MemStore implements artifacts.Repository and artifacts.CleanupStore over
// memdb. Every method is one memdb.Update or View: a reservation, its staging
// object record and its audit entry commit together, a completion commits only
// under the current fenced claim, and no call reaches the object store.
//
// MemStore is development-only. See MEMORY.md for what it does not model.
type MemStore struct {
	store *memdb.Store

	mu      sync.Mutex
	tenants map[domain.TenantID]struct{}
	seq     int
}

var (
	_ artifacts.Repository   = (*MemStore)(nil)
	_ artifacts.CleanupStore = (*MemStore)(nil)
)

// NewMemStore returns a store over the kernel.
func NewMemStore(store *memdb.Store) (*MemStore, error) {
	if store == nil {
		return nil, errors.New("artifactstest: a memdb store is required")
	}
	return &MemStore{store: store, tenants: map[domain.TenantID]struct{}{}}, nil
}

// DevelopmentOnly marks the adapter for the composition root: production
// refuses it (SD-12).
func (*MemStore) DevelopmentOnly() bool { return true }

func (s *MemStore) know(t domain.TenantID) {
	s.mu.Lock()
	s.tenants[t] = struct{}{}
	s.mu.Unlock()
}

func (s *MemStore) auditID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	return fmt.Sprintf("aud_%08d", s.seq)
}

func (s *MemStore) audit(tx memdb.Tx, a artifacts.Artifact, actor artifacts.Actor, action string) error {
	return auditT.Put(tx, auditRec{ID: s.auditID(), Project: a.Scope().ProjectID, Artifact: a.ID(), Action: action, Actor: actor, Version: a.Version()})
}

// AuditActions lists the recorded audit actions of one artifact in order.
func (s *MemStore) AuditActions(ctx context.Context, scope domain.Scope, id domain.ArtifactID) ([]string, error) {
	var out []string
	err := s.store.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		auditT.Scan(tx, func(r auditRec) bool {
			if r.Artifact == id && r.Project == scope.ProjectID {
				out = append(out, r.Action)
			}
			return true
		})
		return nil
	})
	return out, err
}

// record rebuilds an artifact through its persisted form with a change applied,
// the memory counterpart of the UPDATE statements that move single columns.
func record(a artifacts.Artifact) artifacts.Record {
	rec := artifacts.Record{
		ID: a.ID(), Reservation: a.Reservation(), State: a.State(), Disposition: a.Disposition(),
		StagingKey: a.StagingKey(), SealedKey: a.SealedKey(), Version: a.Version(), CreatedAt: a.CreatedAt(), UpdatedAt: a.UpdatedAt(),
		TransferExpiresAt: a.TransferExpiresAt(), VerifiedAt: a.VerifiedAt(), Claim: a.Claim(),
	}
	if a.VerifiedAt() != nil {
		if size, ok := a.Size(); ok {
			rec.VerifiedSize = &size
		}
	}
	return rec
}

func rewrite(a artifacts.Artifact, change func(*artifacts.Record)) (artifacts.Artifact, error) {
	rec := record(a)
	change(&rec)
	return artifacts.Restore(rec)
}

func load(tx memdb.Tx, scope domain.Scope, id domain.ArtifactID) (artifacts.Artifact, error) {
	r, ok := artifactsT.Get(tx, akey(scope.ProjectID, id))
	if !ok || r.A.Scope() != scope {
		return artifacts.Artifact{}, artifacts.ErrNotFound
	}
	return r.A, nil
}

func findByName(tx memdb.Tx, scope domain.Scope, attempt domain.AttemptID, name string) (artifacts.Artifact, error) {
	var out artifacts.Artifact
	found := false
	artifactsT.Scan(tx, func(r artifactRec) bool {
		if r.A.Scope() == scope && r.A.Attempt() == attempt && r.A.Name() == name {
			out, found = r.A, true
			return false
		}
		return true
	})
	if !found {
		return artifacts.Artifact{}, artifacts.ErrNotFound
	}
	return out, nil
}

// ---- reads ---------------------------------------------------------------------

// Get implements artifacts.Repository.
func (s *MemStore) Get(ctx context.Context, scope domain.Scope, id domain.ArtifactID) (artifacts.Artifact, error) {
	var a artifacts.Artifact
	err := s.store.View(ctx, scope.TenantID, func(tx memdb.Tx) (err error) {
		a, err = load(tx, scope, id)
		return err
	})
	return a, err
}

// GetForAttempt implements artifacts.Repository. An artifact of another attempt
// is reported exactly as a missing one.
func (s *MemStore) GetForAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, id domain.ArtifactID) (artifacts.Artifact, error) {
	a, err := s.Get(ctx, scope, id)
	if err != nil {
		return artifacts.Artifact{}, err
	}
	if a.Attempt() != attempt {
		return artifacts.Artifact{}, artifacts.ErrNotFound
	}
	return a, nil
}

// FindByName implements artifacts.Repository.
func (s *MemStore) FindByName(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, name string) (artifacts.Artifact, error) {
	var a artifacts.Artifact
	err := s.store.View(ctx, scope.TenantID, func(tx memdb.Tx) (err error) {
		a, err = findByName(tx, scope, attempt, name)
		return err
	})
	return a, err
}

// ReferencesForAttempt implements artifacts.Repository.
func (s *MemStore) ReferencesForAttempt(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, ids []domain.ArtifactID) (map[domain.ArtifactID]artifacts.Reference, error) {
	out := map[domain.ArtifactID]artifacts.Reference{}
	err := s.store.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		for _, id := range ids {
			if a, err := load(tx, scope, id); err == nil && a.Attempt() == attempt {
				out[id] = a.Reference()
			}
		}
		return nil
	})
	return out, err
}

// ---- reservation ---------------------------------------------------------------

// Reserve implements artifacts.Repository: an existing artifact of the same
// (attempt, name) is returned untouched; otherwise the aggregate limits are
// checked, and the artifact, its staging object record and its audit entry are
// written together.
func (s *MemStore) Reserve(ctx context.Context, cmd artifacts.ReserveCommand) (artifacts.ReserveResult, error) {
	a := cmd.Artifact
	res := a.Reservation()
	var out artifacts.ReserveResult
	err := s.store.Update(ctx, res.Scope.TenantID, func(tx memdb.Tx) error {
		if existing, err := findByName(tx, res.Scope, res.Attempt, res.Name); err == nil {
			out = artifacts.ReserveResult{Artifact: existing}
			return nil
		}
		if _, taken := artifactsT.Get(tx, akey(res.Scope.ProjectID, a.ID())); taken {
			// Put replaces a row with the same key: refuse before it can.
			return fmt.Errorf("%w: artifact identity %s is already in use in the project", domain.ErrConflict, a.ID())
		}
		if res.Kind == artifacts.KindFile {
			if err := checkLimits(tx, res, cmd.Limits); err != nil {
				return err
			}
		}
		if err := artifactsT.Put(tx, artifactRec{A: a}); err != nil {
			return err
		}
		if a.Kind() == artifacts.KindFile {
			if err := objectsT.Put(tx, objectRec{Project: res.Scope.ProjectID, ArtifactID: a.ID(), Key: a.StagingKey(), Purpose: "staging",
				State: "live", WritableUntil: *a.TransferExpiresAt(), CreatedAt: cmd.Now}); err != nil {
				return err
			}
		}
		action := "artifact.reserved"
		if a.Kind() == artifacts.KindOCI {
			action = "artifact.registered"
		}
		if err := s.audit(tx, a, cmd.Actor, action); err != nil {
			return err
		}
		out = artifacts.ReserveResult{Artifact: a, Created: true}
		return nil
	})
	if err == nil {
		s.know(res.Scope.TenantID)
	}
	return out, err
}

// checkLimits enforces the aggregate bounds of the producing attempt: the
// active files of the same role, counted with the new one.
func checkLimits(tx memdb.Tx, r artifacts.Reservation, l artifacts.Limits) error {
	var count, bytes int64
	artifactsT.Scan(tx, func(x artifactRec) bool {
		if x.A.Scope() == r.Scope && x.A.Attempt() == r.Attempt && x.A.Kind() == artifacts.KindFile &&
			x.A.Disposition() == artifacts.DispositionActive && x.A.Role() == r.Role {
			count++
			if size, ok := x.A.Size(); ok {
				bytes += size
			}
		}
		return true
	})
	var maxFiles, maxBytes int64
	switch r.Role {
	case artifacts.RoleOutput:
		maxFiles, maxBytes = l.OutputFiles, l.OutputBytes
	case artifacts.RoleDiagnostic:
		maxFiles, maxBytes = l.DiagnosticFiles, l.DiagnosticBytes
	case artifacts.RoleBundle:
		maxFiles, maxBytes = l.BundleFiles, l.BundleBytes
	}
	if count+1 > maxFiles || bytes+*r.Size > maxBytes {
		return fmt.Errorf("%w: the attempt's %s artifacts would exceed their limit", artifacts.ErrLimitExceeded, r.Role)
	}
	return nil
}

// RecordTransfer implements artifacts.Repository. A renewed instruction can land
// a late write after cleanup, so the staging record is reopened.
func (s *MemStore) RecordTransfer(ctx context.Context, scope domain.Scope, id domain.ArtifactID, expires, now time.Time) error {
	return s.store.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		a, err := load(tx, scope, id)
		if err != nil || a.Kind() != artifacts.KindFile || a.Disposition() != artifacts.DispositionActive {
			return artifacts.ErrNotFound
		}
		next, err := rewrite(a, func(r *artifacts.Record) {
			if r.TransferExpiresAt == nil || expires.After(*r.TransferExpiresAt) {
				e := expires
				r.TransferExpiresAt = &e
			}
			r.UpdatedAt = now
		})
		if err != nil {
			return err
		}
		if err := artifactsT.Put(tx, artifactRec{A: next}); err != nil {
			return err
		}
		objectsT.Scan(tx, func(o objectRec) bool {
			if o.ArtifactID == id && o.Project == scope.ProjectID && o.Purpose == "staging" {
				if expires.After(o.WritableUntil) {
					o.WritableUntil = expires
				}
				o.State, o.DeletedAt, o.DeleteToken, o.DeleteUntil = "live", time.Time{}, "", time.Time{}
				_ = objectsT.Put(tx, o)
			}
			return true
		})
		return nil
	})
}

// ---- completion ----------------------------------------------------------------

// BeginCompletion implements artifacts.Repository: the fenced completion lease,
// with the candidate destination recorded before any object is written to it.
func (s *MemStore) BeginCompletion(ctx context.Context, cmd artifacts.BeginCompletion) (artifacts.ClaimResult, error) {
	var out artifacts.ClaimResult
	err := s.store.Update(ctx, cmd.Scope.TenantID, func(tx memdb.Tx) error {
		a, err := load(tx, cmd.Scope, cmd.ID)
		if err != nil {
			return err
		}
		if a.Attempt() != cmd.Attempt || a.Disposition() != artifacts.DispositionActive {
			return artifacts.ErrNotFound
		}
		if a.Kind() == artifacts.KindOCI || a.State() == artifacts.StateUsable {
			out = artifacts.ClaimResult{Artifact: a}
			return nil
		}
		if a.Claim().Active(cmd.Now) {
			return artifacts.ErrInProgress
		}
		gen := a.Claim().Generation + 1
		until := cmd.Now.Add(cmd.Lease).Truncate(time.Microsecond)
		next, err := rewrite(a, func(r *artifacts.Record) {
			r.Claim = artifacts.Claim{Token: cmd.Token, Generation: gen, Until: until}
			r.UpdatedAt = cmd.Now
		})
		if err != nil {
			return err
		}
		if err := artifactsT.Put(tx, artifactRec{A: next}); err != nil {
			return err
		}
		if _, dup := objectsT.Get(tx, okey(cmd.Scope.ProjectID, cmd.ID, cmd.Destination)); dup {
			return fmt.Errorf("%w: the candidate destination is already recorded", domain.ErrConflict)
		}
		if err := objectsT.Put(tx, objectRec{Project: cmd.Scope.ProjectID, ArtifactID: cmd.ID, Key: cmd.Destination, Purpose: "candidate",
			Generation: gen, State: "live", WritableUntil: until, CreatedAt: cmd.Now}); err != nil {
			return err
		}
		c := next.Claim()
		out = artifacts.ClaimResult{Artifact: next, Claim: &c}
		return nil
	})
	return out, err
}

// ReleaseClaim implements artifacts.Repository.
func (s *MemStore) ReleaseClaim(ctx context.Context, scope domain.Scope, id domain.ArtifactID, token string) error {
	return s.store.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		a, err := load(tx, scope, id)
		if err != nil || a.Claim().Token == "" || a.Claim().Token != token || a.State() != artifacts.StateReserved {
			return nil
		}
		next, err := rewrite(a, func(r *artifacts.Record) { r.Claim = artifacts.Claim{Generation: r.Claim.Generation} })
		if err != nil {
			return err
		}
		return artifactsT.Put(tx, artifactRec{A: next})
	})
}

// CommitUsable implements artifacts.Repository: the claim token and generation,
// the live lease and the winner flag on the exact candidate are checked in one
// Update, so a fenced-out or expired completion commits nothing.
func (s *MemStore) CommitUsable(ctx context.Context, cmd artifacts.CommitCommand) (artifacts.Artifact, error) {
	var out artifacts.Artifact
	err := s.store.Update(ctx, cmd.Scope.TenantID, func(tx memdb.Tx) error {
		a, err := load(tx, cmd.Scope, cmd.ID)
		if err != nil {
			return err
		}
		if a.Attempt() != cmd.Attempt {
			return artifacts.ErrNotFound
		}
		if a.Usable() {
			out = a // a concurrent completion of identical verified content won
			return nil
		}
		if !a.Open() {
			return artifacts.ErrNotFound
		}
		c := a.Claim()
		if c.Token == "" || c.Token != cmd.Claim.Token || c.Generation != cmd.Claim.Generation || !c.Active(cmd.Now) {
			return artifacts.ErrClaimLost
		}
		n, err := a.Complete(cmd.Verification, cmd.Destination, cmd.Now)
		if err != nil {
			return err
		}
		o, ok := objectsT.Get(tx, okey(cmd.Scope.ProjectID, cmd.ID, cmd.Destination))
		if !ok || o.Purpose != "candidate" || o.Generation != cmd.Claim.Generation || o.State != "live" {
			return artifacts.ErrClaimLost
		}
		o.Winner = true
		if err := objectsT.Put(tx, o); err != nil {
			return err
		}
		if err := artifactsT.Put(tx, artifactRec{A: n}); err != nil {
			return err
		}
		if err := s.audit(tx, n, cmd.Actor, "artifact.completed"); err != nil {
			return err
		}
		out = n
		return nil
	})
	return out, err
}

// ---- pins ----------------------------------------------------------------------

// Pin implements artifacts.Repository. Serialized writers order a pin and a
// retention decision totally.
func (s *MemStore) Pin(ctx context.Context, scope domain.Scope, id domain.ArtifactID, owner artifacts.PinOwner, now time.Time) error {
	return s.store.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		a, err := load(tx, scope, id)
		if err != nil {
			return err
		}
		if a.Disposition() == artifacts.DispositionRemoved {
			return artifacts.ErrNotFound
		}
		if !a.Usable() {
			return fmt.Errorf("%w: only usable content can be pinned", artifacts.ErrIncomplete)
		}
		return pinsT.Put(tx, pinRec{Project: scope.ProjectID, Artifact: id, Owner: owner})
	})
}

// Unpin implements artifacts.Repository.
func (s *MemStore) Unpin(ctx context.Context, scope domain.Scope, id domain.ArtifactID, owner artifacts.PinOwner) error {
	return s.store.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		if _, err := load(tx, scope, id); err != nil {
			return nil // an unknown artifact has no pin to release
		}
		pinsT.Delete(tx, pkey(scope.ProjectID, id, owner))
		return nil
	})
}

func pinned(tx memdb.Tx, project domain.ProjectID, id domain.ArtifactID) bool {
	found := false
	pinsT.Scan(tx, func(p pinRec) bool {
		if p.Artifact == id && p.Project == project {
			found = true
		}
		return !found
	})
	return found
}

// ---- cleanup -------------------------------------------------------------------

// Tenants implements artifacts.CleanupStore: the tenants that hold artifacts.
func (s *MemStore) Tenants(context.Context) ([]domain.TenantID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]domain.TenantID, 0, len(s.tenants))
	for t := range s.tenants {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// AbandonExpired implements artifacts.CleanupStore.
func (s *MemStore) AbandonExpired(ctx context.Context, tenant domain.TenantID, now time.Time, p artifacts.CleanupPolicy) (int, error) {
	n := 0
	err := s.store.Update(ctx, tenant, func(tx memdb.Tx) error {
		var due []artifacts.Artifact
		artifactsT.Scan(tx, func(r artifactRec) bool {
			a := r.A
			if a.State() != artifacts.StateReserved || a.Disposition() != artifacts.DispositionActive || a.Kind() != artifacts.KindFile {
				return true
			}
			if a.TransferExpiresAt().Add(p.AbandonAfter).After(now) {
				return true
			}
			if c := a.Claim(); c.Token != "" && c.Until.Add(p.AbandonAfter).After(now) {
				return true
			}
			due = append(due, a)
			return true
		})
		sort.Slice(due, func(i, j int) bool {
			ti, tj := *due[i].TransferExpiresAt(), *due[j].TransferExpiresAt()
			if !ti.Equal(tj) {
				return ti.Before(tj)
			}
			if due[i].ID() != due[j].ID() {
				return due[i].ID() < due[j].ID()
			}
			return due[i].Scope().ProjectID < due[j].Scope().ProjectID
		})
		if p.Batch > 0 && len(due) > p.Batch {
			due = due[:p.Batch]
		}
		for _, a := range due {
			ab, err := a.Abandon(now)
			if err != nil {
				return err
			}
			if err := artifactsT.Put(tx, artifactRec{A: ab}); err != nil {
				return err
			}
			if err := s.audit(tx, ab, artifacts.Actor{Type: "system", ID: "artifact-reconciler"}, "artifact.abandoned"); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// ClaimDeletions implements artifacts.CleanupStore: promote eligible live
// objects to durable deletion intent, then lease a batch of pending deletions
// under the token.
func (s *MemStore) ClaimDeletions(ctx context.Context, tenant domain.TenantID, now time.Time, p artifacts.CleanupPolicy, token string) ([]artifacts.DeletionClaim, error) {
	var out []artifacts.DeletionClaim
	err := s.store.Update(ctx, tenant, func(tx memdb.Tx) error {
		var live []objectRec
		objectsT.Scan(tx, func(o objectRec) bool {
			if o.State != "live" || o.Winner || o.WritableUntil.Add(p.StagingGrace).After(now) {
				return true
			}
			r, ok := artifactsT.Get(tx, akey(o.Project, o.ArtifactID))
			if !ok {
				return true
			}
			a := r.A
			c := a.Claim()
			held := c.Generation == o.Generation && !c.Until.IsZero() && c.Token != "" && !c.Until.Before(now)
			if a.State() == artifacts.StateUsable || a.Disposition() != artifacts.DispositionActive || (o.Purpose == "candidate" && !held) {
				live = append(live, o)
			}
			return true
		})
		sort.Slice(live, func(i, j int) bool {
			if !live[i].WritableUntil.Equal(live[j].WritableUntil) {
				return live[i].WritableUntil.Before(live[j].WritableUntil)
			}
			return live[i].Key < live[j].Key
		})
		if p.Batch > 0 && len(live) > p.Batch {
			live = live[:p.Batch]
		}
		for _, o := range live {
			o.State, o.DeleteRequested = "delete_pending", now
			if err := objectsT.Put(tx, o); err != nil {
				return err
			}
		}
		var pending []objectRec
		objectsT.Scan(tx, func(o objectRec) bool {
			if o.State == "delete_pending" && (o.DeleteUntil.IsZero() || !o.DeleteUntil.After(now)) {
				pending = append(pending, o)
			}
			return true
		})
		sort.Slice(pending, func(i, j int) bool {
			if !pending[i].DeleteRequested.Equal(pending[j].DeleteRequested) {
				return pending[i].DeleteRequested.Before(pending[j].DeleteRequested)
			}
			return pending[i].Key < pending[j].Key
		})
		if p.Batch > 0 && len(pending) > p.Batch {
			pending = pending[:p.Batch]
		}
		for _, o := range pending {
			o.DeleteToken, o.DeleteUntil, o.DeleteAttempts = token, now.Add(p.DeletionLease), o.DeleteAttempts+1
			if err := objectsT.Put(tx, o); err != nil {
				return err
			}
			out = append(out, artifacts.DeletionClaim{Scope: domain.Scope{TenantID: tenant, ProjectID: o.Project}, ArtifactID: o.ArtifactID, Key: o.Key, Token: token})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// AckDeletion implements artifacts.CleanupStore: it records deletion only while
// the lease is held and no renewed transfer reopened a staging object's write
// horizon.
func (s *MemStore) AckDeletion(ctx context.Context, c artifacts.DeletionClaim, now time.Time, p artifacts.CleanupPolicy) error {
	return s.store.Update(ctx, c.Scope.TenantID, func(tx memdb.Tx) error {
		o, ok := objectsT.Get(tx, okey(c.Scope.ProjectID, c.ArtifactID, c.Key))
		if !ok || o.State != "delete_pending" || o.DeleteToken != c.Token ||
			(o.Purpose == "staging" && o.WritableUntil.Add(p.StagingGrace).After(now)) {
			return artifacts.ErrClaimLost
		}
		o.State, o.DeletedAt, o.DeleteToken, o.DeleteUntil = "deleted", now, "", time.Time{}
		return objectsT.Put(tx, o)
	})
}

// ObjectStates lists the state of the artifact's object records by key, with
// ":winner" appended for the sealed candidate.
func (s *MemStore) ObjectStates(ctx context.Context, scope domain.Scope, id domain.ArtifactID) (map[string]string, error) {
	out := map[string]string{}
	err := s.store.View(ctx, scope.TenantID, func(tx memdb.Tx) error {
		objectsT.Scan(tx, func(o objectRec) bool {
			if o.ArtifactID == id && o.Project == scope.ProjectID {
				st := o.State
				if o.Winner {
					st += ":winner"
				}
				out[o.Key] = st
			}
			return true
		})
		return nil
	})
	return out, err
}

// RetentionCandidates implements artifacts.CleanupStore.
func (s *MemStore) RetentionCandidates(ctx context.Context, tenant domain.TenantID, after domain.ArtifactID, limit int) ([]artifacts.RetentionSubject, error) {
	var out []artifacts.RetentionSubject
	err := s.store.View(ctx, tenant, func(tx memdb.Tx) error {
		artifactsT.Scan(tx, func(r artifactRec) bool {
			a := r.A
			if a.ID() <= after || a.Disposition() != artifacts.DispositionActive || !a.Usable() || pinned(tx, a.Scope().ProjectID, a.ID()) {
				return true
			}
			var verified time.Time
			if a.VerifiedAt() != nil {
				verified = *a.VerifiedAt()
			}
			out = append(out, artifacts.RetentionSubject{ID: a.ID(), Scope: a.Scope(), Attempt: a.Attempt(), Kind: a.Kind(), Role: a.Role(),
				CreatedAt: a.CreatedAt(), VerifiedAt: verified})
			return true
		})
		// PostgreSQL orders by ID: the project only breaks a tie.
		sort.Slice(out, func(i, j int) bool {
			if out[i].ID != out[j].ID {
				return out[i].ID < out[j].ID
			}
			return out[i].Scope.ProjectID < out[j].Scope.ProjectID
		})
		if limit >= 0 && len(out) > limit {
			out = out[:limit]
		}
		return nil
	})
	return out, err
}

// BeginRetentionDeletion implements artifacts.CleanupStore: a pin committed
// first is seen here, and a pin arriving later finds the artifact removed.
func (s *MemStore) BeginRetentionDeletion(ctx context.Context, scope domain.Scope, id domain.ArtifactID, now time.Time, actor artifacts.Actor) error {
	return s.store.Update(ctx, scope.TenantID, func(tx memdb.Tx) error {
		a, err := load(tx, scope, id)
		if err != nil {
			return err
		}
		if !a.Usable() {
			return artifacts.ErrNotFound
		}
		if pinned(tx, scope.ProjectID, id) {
			return artifacts.ErrPinned
		}
		n, err := a.Remove(now)
		if err != nil {
			return err
		}
		if err := artifactsT.Put(tx, artifactRec{A: n}); err != nil {
			return err
		}
		objectsT.Scan(tx, func(o objectRec) bool {
			if o.ArtifactID == id && o.Project == scope.ProjectID && o.Winner && o.State == "live" {
				o.State, o.DeleteRequested = "delete_pending", now
				_ = objectsT.Put(tx, o)
			}
			return true
		})
		return s.audit(tx, n, actor, "artifact.removed")
	})
}
