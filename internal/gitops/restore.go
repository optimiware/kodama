package gitops

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// RestoreActor is the audit actor of the restore reconciliation.
const RestoreActor = "gitops-restore"

// RestoreSource says what to recover from. A snapshot restores the volume; a
// backup replays its bundles on top (or alone, onto a volume that lost the
// repositories). At least one is required. BackupID "latest" selects the newest
// complete backup when the recovery begins; the recovery stores the concrete ID.
type RestoreSource struct {
	SnapshotID string
	BackupID   string
}

// RepositoryStatus is the reconciliation result of one repository.
type RepositoryStatus struct {
	ID string
	// InFlight and Unknown are the push entries after Reconcile.
	InFlight, Unknown int
	// KnownHead is the highest journal head the control plane saw, JournalHead the
	// restored volume's. A lower JournalHead is a volume restored from an older
	// snapshot; its in-flight entries settled unknown, never refused.
	KnownHead, JournalHead int64
	Regressed              bool
	// Acknowledged is true when the restored journal became the new baseline.
	Acknowledged      bool
	JournalUnreadable bool
	// Replayed is true when the recovery's bundle replay of the repository (when
	// the backup has one) completed.
	Replayed bool
	// Discrepancies are the unresolved differences between the recovery point and
	// the volume.
	Discrepancies []Discrepancy
	// PinsRepaired are retained refs recreated from the run records; LostPins are
	// pinned commits the volume no longer has (an operator decision).
	PinsRepaired []string
	LostPins     []PinnedRef
	// Blockers keep the tenant's Git access closed.
	Blockers []string
}

// Report is the result of a restore reconciliation.
type Report struct {
	Tenant       domain.TenantID
	RecoveryID   string
	Repositories []RepositoryStatus
	// Pending are blockers of the recovery as a whole (a volume restore or a
	// manifest that is not complete or readable).
	Pending []string
	// Replays are the completed bundle replays of the recovery, by repository.
	Replays map[string]ReplayResult
	// Unlisted are repositories of the backup the catalog no longer lists (deleted
	// or archived out of the internal set since the backup): they are not
	// initialized, replayed or reconciled, and are reported for the operator.
	Unlisted []string
	// Open is true when every repository reconciled and Git access reopened.
	Open bool
}

// Blockers lists every reason access is still closed.
func (r Report) Blockers() []string {
	out := append([]string(nil), r.Pending...)
	for _, s := range r.Repositories {
		for _, b := range s.Blockers {
			out = append(out, s.ID+": "+b)
		}
	}
	return out
}

// RecoveryOptions tune Recovery.
type RecoveryOptions struct {
	Clock func() time.Time
	// NewID creates the recovery ID (default rcv_<UTC time>_<random>).
	NewID func(now time.Time) string
}

// RecoveryConfig holds the collaborators. Worker, Catalog, Fence, Pushes, Holds,
// Objects and Store are required; Snapshots is needed to restore from a
// snapshot, and Backups to replay bundles.
type RecoveryConfig struct {
	Worker    Worker
	Catalog   Catalog
	Fence     AccessFence
	Pushes    Pushes
	Holds     Holds
	Objects   Objects
	Store     RecoveryStore
	Snapshots Snapshots
	Backups   *Backups
	Options   RecoveryOptions
}

// Recovery restores a tenant's repositories and keeps Git access closed until
// they reconcile (GR-15, GR-A10).
type Recovery struct{ c RecoveryConfig }

// NewRecovery validates the configuration.
func NewRecovery(c RecoveryConfig) (*Recovery, error) {
	if c.Worker == nil || c.Catalog == nil || c.Fence == nil || c.Pushes == nil || c.Holds == nil || c.Objects == nil || c.Store == nil {
		return nil, fmt.Errorf("%w: a worker, a catalog, a fence, pushes, holds, objects and a recovery store are required", ErrNotConfigured)
	}
	if c.Options.Clock == nil {
		c.Options.Clock = time.Now
	}
	if c.Options.NewID == nil {
		c.Options.NewID = func(now time.Time) string {
			var b [6]byte
			if _, err := rand.Read(b[:]); err != nil {
				panic(err)
			}
			return "rcv_" + now.UTC().Format("20060102T150405Z") + "_" + hex.EncodeToString(b[:])
		}
	}
	return &Recovery{c: c}, nil
}

func (r *Recovery) now() time.Time { return r.c.Options.Clock().UTC() }

// Restore records the recovery, closes the tenant's Git access, recovers the
// volume and/or replays the bundles and reconciles; access reopens only when
// Reconcile finds every required step complete and every repository consistent.
//
// The selected source and each completed step (volume restore, per-repository
// replay) are stored durably (RecoveryStore) before access can reopen, so a
// failed step, a restart or another instance never reopens a half-restored
// volume. Restore is resumable: repeating it with the same source continues
// where it stopped (completed steps are not repeated), and a different source
// while a recovery is active is ErrRecoveryActive. Every manifest repository is
// initialized (an empty one is recreated and only its replay is skipped), bundles
// are verified against their digest before the worker replays them, and the
// worker only creates refs the repository lacks. If the source cannot be read, or
// the fence cannot be closed, nothing is touched. The returned error joins the
// failed steps; the report says what still blocks.
func (r *Recovery) Restore(ctx context.Context, tenant domain.TenantID, src RestoreSource) (Report, error) {
	if err := tenant.Validate(); err != nil {
		return Report{}, err
	}
	if src.SnapshotID == "" && src.BackupID == "" {
		return Report{}, errors.New("gitops: a restore names a snapshot, a backup or both")
	}
	if src.SnapshotID != "" && r.c.Snapshots == nil {
		return Report{}, fmt.Errorf("%w: no snapshots", ErrNotConfigured)
	}
	// A resumed recovery keeps the backup it selected: "latest" must not drift to a
	// newer backup while the recovery is active.
	if src.BackupID == "latest" {
		cur, ok, err := r.c.Store.Current(ctx, tenant)
		if err != nil {
			return Report{}, fmt.Errorf("%w: reading the recovery: %v", ErrUnavailable, err)
		}
		if ok && cur.Status == RecoveryActive && cur.SnapshotID == src.SnapshotID && cur.BackupID != "" {
			src.BackupID = cur.BackupID
		}
	}
	// Read the source first: nothing is closed or recorded for one that cannot be used.
	var manifest *Manifest
	if src.BackupID != "" {
		if r.c.Backups == nil {
			return Report{}, fmt.Errorf("%w: no backups", ErrNotConfigured)
		}
		id := src.BackupID
		if id == "latest" {
			id = ""
		}
		m, err := r.c.Backups.Manifest(ctx, tenant, id)
		if err != nil {
			return Report{}, err
		}
		manifest = &m
		src.BackupID = m.ID
	}
	if err := r.c.Fence.Close(ctx, tenant, FenceRestore); err != nil {
		return Report{}, fmt.Errorf("%w: closing Git access: %v", ErrUnavailable, err)
	}
	now := r.now()
	rec, err := r.c.Store.Begin(ctx, tenant, r.c.Options.NewID(now), src, now)
	if err != nil {
		return Report{Tenant: tenant}, err
	}
	var errs []error
	if src.SnapshotID != "" && !rec.VolumeRestored {
		if err := r.c.Snapshots.RestoreVolume(ctx, tenant, src.SnapshotID); err != nil {
			errs = append(errs, fmt.Errorf("%w: restoring the volume: %v", ErrUnavailable, err))
		} else if err := r.c.Store.MarkVolumeRestored(ctx, tenant, rec.ID, r.now()); err != nil {
			errs = append(errs, err)
		}
	}
	if manifest != nil && len(errs) == 0 { // a bundle never lands on a volume that is not restored yet
		errs = append(errs, r.replay(ctx, tenant, rec, *manifest)...)
	}
	rep, rerr := r.Reconcile(ctx, tenant)
	if rerr != nil {
		errs = append(errs, rerr)
	}
	return rep, errors.Join(errs...)
}

// replay initializes every repository of the manifest the catalog still lists,
// empty ones included, and replays the bundle of each non-empty one that was not
// replayed yet. A repository the catalog no longer lists is skipped, not
// recreated: nothing would ever reconcile it (Report.Unlisted). The order
// per repository is Initialize, enter the worker's restore fence, replay, leave
// the fence, and only then is the replay recorded: a crash anywhere repeats the
// idempotent steps (the operation ID is derived from the recovery and the
// repository) and never leaves a repository fenced or marked replayed too early.
// The worker fence is left right after the replay because the reconciliation
// that follows repairs retained refs (server ref operations, which a fenced
// repository refuses); the tenant's access fence stays closed throughout.
func (r *Recovery) replay(ctx context.Context, tenant domain.TenantID, rec RecoveryRecord, m Manifest) []error {
	listed, err := listedRepositories(ctx, r.c.Catalog, tenant)
	if err != nil {
		return []error{fmt.Errorf("%w: listing repositories: %v", ErrUnavailable, err)}
	}
	var errs []error
	for _, e := range m.Repositories {
		if _, done := rec.Replays[e.ID]; done {
			continue
		}
		if !listed[e.ID] { // deleted since the backup: never recreated on the worker
			continue
		}
		if _, err := r.c.Worker.Initialize(ctx, tenant, gitv1.InitializeRepositoryRequest{RepositoryID: e.ID, DefaultBranch: e.DefaultBranch}); err != nil {
			errs = append(errs, fmt.Errorf("repository %s: initializing: %w", e.ID, err))
			continue
		}
		var res ReplayResult
		if !e.Empty {
			var err error
			if res, err = r.replayOne(ctx, tenant, rec, e); err != nil {
				errs = append(errs, fmt.Errorf("repository %s: %w", e.ID, err))
				continue
			}
		}
		if err := r.c.Store.MarkReplayed(ctx, tenant, rec.ID, e.ID, res, r.now()); err != nil {
			errs = append(errs, fmt.Errorf("repository %s: %w", e.ID, err))
		}
	}
	return errs
}

func (r *Recovery) replayOne(ctx context.Context, tenant domain.TenantID, rec RecoveryRecord, e ManifestRepository) (res ReplayResult, err error) {
	// The digest is verified before any byte reaches the worker.
	if err := r.c.Backups.VerifyBundle(ctx, e); err != nil {
		return ReplayResult{}, err
	}
	rc, err := r.c.Backups.c.Store.Open(ctx, e.Key)
	if err != nil {
		return ReplayResult{}, err
	}
	defer rc.Close()
	if err := r.c.Worker.SetRestoreFence(ctx, tenant, e.ID, true); err != nil {
		return ReplayResult{}, fmt.Errorf("entering the restore fence: %w", err)
	}
	defer func() {
		// Leaving the fence is repeated by the next pass if it fails, and the
		// replay is then not recorded.
		if lerr := r.c.Worker.SetRestoreFence(context.WithoutCancel(ctx), tenant, e.ID, false); lerr != nil {
			res, err = ReplayResult{}, errors.Join(err, fmt.Errorf("leaving the restore fence: %w", lerr))
		}
	}()
	res, err = r.c.Worker.ReplayBundle(ctx, tenant, ReplayRequest{
		RepositoryID: e.ID, OperationID: operationID("rpl", string(tenant), rec.ID, e.ID), SHA256: e.SHA256,
	}, rc)
	if err != nil {
		return ReplayResult{}, fmt.Errorf("replaying the bundle of backup %s: %w", rec.BackupID, err)
	}
	return res, nil
}

// Reconcile reconciles the tenant's repositories after a restore and reopens
// Git access only when every required step completed and every repository is
// consistent. It runs only while a recovery is active and access is closed for
// it (ErrNotRestoring otherwise), because it lowers journal heads.
//
// Required for the whole recovery: the volume snapshot restore, when the
// recovery has one, completed, and the backup's manifest is readable. For each
// repository of the backup: its initialization and bundle replay completed (an
// empty repository only its initialization), and the volume matches the
// recovery point: every ref the manifest holds is there at that commit and no
// other ref is, except those the operator accepted (ResolveDiscrepancy). A
// snapshot older than the bundle, a branch deleted before the bundle that the
// snapshot resurrects and a rewrite the replay cannot apply are each a
// discrepancy that blocks reopening until an audited operator resolution.
//
// For every repository: gitpush settles what it can from the restored worker's
// evidence (an entry whose journal is older than the head already seen is
// unknown, never refused); entries still in flight, an unreadable journal or an
// unreadable worker keep access closed, while unknown entries stay visible and
// keep blocking their writers without keeping access closed; each retained ref
// the run records expect is checked (a missing one whose commit exists is
// recreated, one whose commit is gone or at another commit is a blocker); and a
// repository with nothing in flight whose journal head is below the one seen is
// acknowledged, so the restored journal becomes the baseline.
//
// Reconcile is idempotent and may be repeated, by any instance, until it opens.
func (r *Recovery) Reconcile(ctx context.Context, tenant domain.TenantID) (Report, error) {
	rep := Report{Tenant: tenant}
	closed, err := r.c.Fence.ClosedFor(ctx, tenant, FenceRestore)
	if err != nil {
		return rep, fmt.Errorf("%w: reading the access fence: %v", ErrUnavailable, err)
	}
	if !closed {
		return rep, ErrNotRestoring
	}
	rec, ok, err := r.c.Store.Current(ctx, tenant)
	if err != nil {
		return rep, fmt.Errorf("%w: reading the recovery: %v", ErrUnavailable, err)
	}
	if !ok {
		return rep, ErrNotRestoring
	}
	rep.RecoveryID, rep.Replays = rec.ID, rec.Replays
	if rec.Status == RecoveryComplete { // a crash between completing and opening: finish the opening
		if err := r.c.Fence.Open(ctx, tenant, FenceRestore); err != nil {
			return rep, fmt.Errorf("%w: reopening Git access: %v", ErrUnavailable, err)
		}
		rep.Open = true
		return rep, nil
	}
	if !rec.VolumeRestored {
		rep.Pending = append(rep.Pending, "the volume snapshot restore has not completed")
	}
	var manifest *Manifest
	if rec.BackupID != "" {
		if r.c.Backups == nil {
			rep.Pending = append(rep.Pending, "the backup of the recovery cannot be read: no backups are configured")
		} else if m, err := r.c.Backups.Manifest(ctx, tenant, rec.BackupID); err != nil {
			rep.Pending = append(rep.Pending, fmt.Sprintf("the manifest of backup %s cannot be read: %v", rec.BackupID, err))
		} else {
			manifest = &m
		}
	}
	expected := map[string]ManifestRepository{}
	if manifest != nil {
		for _, e := range manifest.Repositories {
			expected[e.ID] = e
		}
	}
	err = repositories(ctx, r.c.Catalog, tenant, func(repo Repository) error {
		rep.Repositories = append(rep.Repositories, r.reconcileRepository(ctx, repo, rec, expected))
		return nil
	})
	if err != nil {
		return rep, fmt.Errorf("%w: listing repositories: %v", ErrUnavailable, err)
	}
	sort.Slice(rep.Repositories, func(i, j int) bool { return rep.Repositories[i].ID < rep.Repositories[j].ID })
	for id := range expected {
		found := false
		for _, st := range rep.Repositories {
			found = found || st.ID == id
		}
		if !found {
			rep.Unlisted = append(rep.Unlisted, id)
		}
	}
	sort.Strings(rep.Unlisted)
	if len(rep.Blockers()) > 0 {
		return rep, nil
	}
	if err := r.c.Store.Complete(ctx, tenant, rec.ID, r.now()); err != nil {
		return rep, fmt.Errorf("%w: completing the recovery: %v", ErrUnavailable, err)
	}
	if err := r.c.Fence.Open(ctx, tenant, FenceRestore); err != nil {
		return rep, fmt.Errorf("%w: reopening Git access: %v", ErrUnavailable, err)
	}
	rep.Open = true
	return rep, nil
}

func (r *Recovery) reconcileRepository(ctx context.Context, repo Repository, rec RecoveryRecord, expected map[string]ManifestRepository) RepositoryStatus {
	tenant := repo.Scope.TenantID
	st := RepositoryStatus{ID: repo.ID}
	block := func(format string, args ...any) { st.Blockers = append(st.Blockers, fmt.Sprintf(format, args...)) }

	want, inBackup := expected[repo.ID]
	if inBackup {
		_, st.Replayed = rec.Replays[repo.ID]
		if !st.Replayed {
			block("the bundle replay of backup %s has not completed", rec.BackupID)
		}
	}

	if err := r.c.Pushes.Reconcile(ctx, tenant, repo.ID); err != nil {
		block("push records could not be reconciled: %v", err)
	}
	settlement, err := r.c.Pushes.Settlement(ctx, tenant, repo.ID)
	if err != nil {
		block("push records could not be read: %v", err)
		return st
	}
	st.InFlight, st.Unknown, st.KnownHead = settlement.InFlight, settlement.Unknown, settlement.KnownHead
	if st.InFlight > 0 {
		block("%d push entries are still in flight", st.InFlight)
	}

	defaultRef := gitv1.HeadsPrefix + repo.DefaultBranch
	j, err := r.c.Worker.ReadJournal(ctx, tenant, gitv1.ReadJournalRequest{RepositoryID: repo.ID, Ref: &defaultRef, SinceSequence: 0, Limit: 1})
	var ce *gitv1.ContractError
	switch {
	case errors.As(err, &ce) && ce.Code == gitv1.CodeJournalUnreadable:
		st.JournalUnreadable = true
		block("the worker's ref journal is unreadable")
	case err != nil:
		block("the worker's journal could not be read: %v", err)
	default:
		st.JournalHead = j.HeadSequence
		st.Regressed = st.JournalHead < st.KnownHead
	}

	pinned := r.reconcilePins(ctx, repo, &st, block)

	if inBackup && st.Replayed {
		actual, err := allRefs(ctx, r.c.Worker, tenant, repo.ID)
		if err != nil {
			block("the refs could not be listed: %v", err)
		} else {
			for _, d := range diffRefs(repo.ID, want.Refs, actual, pinned) {
				if !resolved(rec, d) {
					st.Discrepancies = append(st.Discrepancies, d)
					block("%s", describe(d))
				}
			}
		}
	}

	if len(st.Blockers) == 0 && st.Regressed {
		reason := fmt.Sprintf("restored volume: journal head %d replaces %d", st.JournalHead, st.KnownHead)
		if _, err := r.c.Pushes.AcknowledgeRestore(ctx, repo.Scope, repo.ID, st.JournalHead, RestoreActor, reason); err != nil {
			block("the restored journal could not be acknowledged: %v", err)
		} else {
			st.Acknowledged = true
		}
	}
	return st
}

func describe(d Discrepancy) string {
	switch d.Kind {
	case DiscrepancyMissing:
		return fmt.Sprintf("the recovery point has %s at %s, the volume lacks it", d.Ref, d.Expected)
	case DiscrepancyExtra:
		return fmt.Sprintf("the volume has %s at %s, the recovery point does not", d.Ref, d.Actual)
	}
	return fmt.Sprintf("%s is at %s, the recovery point has %s", d.Ref, d.Actual, d.Expected)
}

// allRefs lists the refs of the namespaces the worker exposes.
func allRefs(ctx context.Context, w Worker, tenant domain.TenantID, repositoryID string) ([]gitv1.RefTip, error) {
	var out []gitv1.RefTip
	for _, p := range gitv1.RefPrefixes {
		refs, err := listRefs(ctx, w, tenant, repositoryID, p)
		if err != nil {
			return nil, err
		}
		out = append(out, refs...)
	}
	return out, nil
}

func inNamespaces(ref string) bool {
	for _, p := range gitv1.RefPrefixes {
		if strings.HasPrefix(ref, p) {
			return true
		}
	}
	return false
}

// diffRefs compares the recovery point's refs with the volume's, ignoring refs
// outside the namespaces the worker lists and the retained refs the run records
// own (they are reconciled against those records).
func diffRefs(repositoryID string, expected, actual []gitv1.RefTip, pinned map[string]bool) []Discrepancy {
	want, have := map[string]string{}, map[string]string{}
	for _, t := range expected {
		if inNamespaces(t.Ref) && !pinned[t.Ref] {
			want[t.Ref] = t.Commit
		}
	}
	for _, t := range actual {
		if inNamespaces(t.Ref) && !pinned[t.Ref] {
			have[t.Ref] = t.Commit
		}
	}
	var out []Discrepancy
	for ref, c := range want {
		switch got, ok := have[ref]; {
		case !ok:
			out = append(out, Discrepancy{Repository: repositoryID, Ref: ref, Kind: DiscrepancyMissing, Expected: c})
		case got != c:
			out = append(out, Discrepancy{Repository: repositoryID, Ref: ref, Kind: DiscrepancyDiffers, Expected: c, Actual: got})
		}
	}
	for ref, c := range have {
		if _, ok := want[ref]; !ok {
			out = append(out, Discrepancy{Repository: repositoryID, Ref: ref, Kind: DiscrepancyExtra, Actual: c})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out
}

func resolved(rec RecoveryRecord, d Discrepancy) bool {
	for _, x := range rec.Resolutions {
		if x.Repository == d.Repository && x.Ref == d.Ref && x.Kind == d.Kind && x.Actual == d.Actual {
			return true
		}
	}
	return false
}

// reconcilePins checks the retained refs the run records expect and returns the
// set of refs it owns.
func (r *Recovery) reconcilePins(ctx context.Context, repo Repository, st *RepositoryStatus, block func(string, ...any)) map[string]bool {
	owned := map[string]bool{}
	tenant := repo.Scope.TenantID
	held, err := r.c.Holds.Held(ctx, repo.Scope, repo.ID)
	if err != nil {
		block("the pins the runs expect could not be read: %v", err)
		return owned
	}
	for _, p := range held.Pins {
		owned[p.Ref] = true
	}
	if len(held.Pins) == 0 {
		return owned
	}
	server, err := listRefs(ctx, r.c.Worker, tenant, repo.ID, gitv1.ServerRefPrefix)
	if err != nil {
		block("the retained refs could not be listed: %v", err)
		return owned
	}
	have := map[string]string{}
	for _, t := range server {
		have[t.Ref] = t.Commit
	}
	for _, p := range held.Pins {
		cur, present := have[p.Ref]
		switch {
		case present && cur == p.Commit:
		case present:
			block("retained ref %s is at %s, the run pinned %s", p.Ref, cur, p.Commit)
		default:
			ok, err := r.c.Objects.HasCommit(ctx, tenant, repo.ID, p.Commit)
			switch {
			case err != nil:
				block("the pinned commit %s could not be checked: %v", p.Commit, err)
			case !ok:
				st.LostPins = append(st.LostPins, p)
				block("the commit %s pinned by %s is not in the restored repository", p.Commit, p.Ref)
			default:
				if _, err := r.c.Worker.SetRetainedRef(ctx, tenant, gitv1.SetRetainedRefRequest{
					RepositoryID: repo.ID, OperationID: operationID("rpn", string(tenant), repo.ID, p.Ref, p.Commit), Ref: p.Ref, Commit: p.Commit,
				}); err != nil {
					block("the retained ref %s could not be recreated: %v", p.Ref, err)
				} else {
					st.PinsRepaired = append(st.PinsRepaired, p.Ref)
				}
			}
		}
	}
	return owned
}

// Discrepancies lists the unresolved differences between the active recovery's
// recovery point and the volume (what an operator inspects before
// ResolveDiscrepancy). It runs a Reconcile, so it fails like one.
func (r *Recovery) Discrepancies(ctx context.Context, tenant domain.TenantID) ([]Discrepancy, error) {
	rep, err := r.Reconcile(ctx, tenant)
	if err != nil {
		return nil, err
	}
	var out []Discrepancy
	for _, s := range rep.Repositories {
		out = append(out, s.Discrepancies...)
	}
	return out, nil
}

// ResolveDiscrepancy is the audited operator action that accepts one difference
// between the recovery point and the volume: the volume's state for that ref
// stands. The operator names the discrepancy exactly as Discrepancies showed it
// (including the observed commit) and gives a reason; the difference must still
// exist (ErrNotDiscrepancy otherwise, so a stale or invented resolution records
// nothing), and the resolution and its audit row are stored in one transaction.
// Authorization of the operator is the caller's (the administrative route,
// T-132); the actor is recorded. The resolution never changes a ref: replay stays
// non-destructive, and a ref that changes again needs a new resolution.
func (r *Recovery) ResolveDiscrepancy(ctx context.Context, tenant domain.TenantID, by, reason string, d Discrepancy) error {
	if by == "" || reason == "" || len(reason) > 1024 {
		return errors.New("gitops: a resolution names its operator and a reason of 1 to 1024 bytes")
	}
	closed, err := r.c.Fence.ClosedFor(ctx, tenant, FenceRestore)
	if err != nil {
		return fmt.Errorf("%w: reading the access fence: %v", ErrUnavailable, err)
	}
	rec, ok, err := r.c.Store.Current(ctx, tenant)
	if err != nil {
		return fmt.Errorf("%w: reading the recovery: %v", ErrUnavailable, err)
	}
	if !closed || !ok || rec.Status != RecoveryActive {
		return ErrNotRestoring
	}
	current, err := r.Discrepancies(ctx, tenant)
	if err != nil {
		return err
	}
	found := false
	for _, c := range current {
		found = found || c == d
	}
	if !found {
		return ErrNotDiscrepancy
	}
	return r.c.Store.Resolve(ctx, tenant, rec.ID, DiscrepancyResolution{Discrepancy: d, By: by, Reason: reason, At: r.now()})
}
