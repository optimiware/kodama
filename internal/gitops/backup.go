package gitops

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// ManifestVersion is the version of the backup manifest.
const ManifestVersion = 1

// BackupPrefix is the key prefix of a tenant's backups: every backup is the
// objects under <BackupPrefix><tenant>/<backupId>/, and the manifest
// (manifest.json) is written last, so a backup without a manifest is incomplete
// and never used.
func BackupPrefix(tenant domain.TenantID) string { return "git-backups/" + string(tenant) + "/" }

func backupDir(tenant domain.TenantID, id string) string { return BackupPrefix(tenant) + id + "/" }

// ManifestKey is the key of a backup's manifest.
func ManifestKey(tenant domain.TenantID, id string) string {
	return backupDir(tenant, id) + "manifest.json"
}

// ManifestRepository describes one repository's bundle.
type ManifestRepository struct {
	ID            string `json:"id"`
	ProjectID     string `json:"projectId"`
	DefaultBranch string `json:"defaultBranch"`
	// Empty repositories have no refs and no bundle.
	Empty bool   `json:"empty"`
	Key   string `json:"key,omitempty"`
	Size  int64  `json:"size,omitempty"`
	// SHA256 is the hex digest of the stored bundle bytes.
	SHA256 string `json:"sha256,omitempty"`
	// JournalHead is the worker's journal head read just before the bundle: the
	// bundle is at least that new.
	JournalHead int64 `json:"journalHead"`
	// Refs are exactly the refs the bundle holds.
	Refs []gitv1.RefTip `json:"refs"`
}

// ManifestSnapshot is the volume snapshot the backup is coordinated with.
type ManifestSnapshot struct {
	ID      string    `json:"id"`
	TakenAt time.Time `json:"takenAt"`
}

// Manifest describes one complete backup of a tenant's repositories: the volume
// snapshot taken just before the bundles (so every bundle is at least as new as
// the snapshot) and one verified bundle per non-empty repository.
type Manifest struct {
	Version      int                  `json:"version"`
	Tenant       string               `json:"tenant"`
	ID           string               `json:"id"`
	CreatedAt    time.Time            `json:"createdAt"`
	Snapshot     ManifestSnapshot     `json:"snapshot"`
	Repositories []ManifestRepository `json:"repositories"`
}

// Validate checks the manifest against the tenant whose store location it was
// read from: a manifest naming another tenant is corrupt, never used.
func (m Manifest) Validate(tenant domain.TenantID) error {
	switch {
	case m.Version != ManifestVersion:
		return fmt.Errorf("%w: manifest version %d", ErrBackupCorrupt, m.Version)
	case m.Tenant != string(tenant):
		return fmt.Errorf("%w: the manifest belongs to another tenant", ErrBackupCorrupt)
	case m.Snapshot.ID == "":
		return fmt.Errorf("%w: the manifest names no snapshot", ErrBackupCorrupt)
	}
	for _, r := range m.Repositories {
		if r.Empty {
			continue
		}
		if r.Key != backupDir(tenant, m.ID)+r.ID+".bundle" || len(r.SHA256) != 64 || r.Size <= 0 {
			return fmt.Errorf("%w: repository %s", ErrBackupCorrupt, r.ID)
		}
	}
	return nil
}

// BackupOptions tune Backups.
type BackupOptions struct {
	// Keep is the number of complete backups kept per tenant (default 7).
	Keep int
	// LeaseTTL is how long a backup run may go without renewing its lease before
	// another run may abandon it (default 15 minutes).
	LeaseTTL time.Duration
	// RenewEvery is the period of the background renewal that keeps the lease
	// alive while a bundle streams and uploads (default LeaseTTL/3). The lease is
	// also renewed after each repository and before the manifest. Leases expire on
	// the clock of the lease store: the PostgreSQL store uses the database's
	// clock unless it is given one, so instances need no synchronized clocks;
	// otherwise they must agree to within a small fraction of LeaseTTL.
	RenewEvery time.Duration
	Clock      func() time.Time
	// NewID creates the backup ID; it must sort by time (default
	// bk_<UTC time>_<random>).
	NewID func(now time.Time) string
}

// BackupConfig holds the collaborators; all but Options are required.
type BackupConfig struct {
	Worker    Worker
	Objects   Objects
	Catalog   Catalog
	Store     BackupStore
	Snapshots Snapshots
	// Leases serializes backups and expiry per tenant across instances.
	Leases BackupLeases
	// Recoveries and Fence are required: a backup is refused (ErrTenantClosed)
	// while the tenant has an active recovery or closed Git access, and the backup
	// an active recovery reads is protected from expiry.
	Recoveries RecoveryStore
	Fence      AccessFence
	Options    BackupOptions
}

// Backups takes and reads per-tenant backups.
type Backups struct {
	c BackupConfig
	o BackupOptions
}

// NewBackups validates the configuration.
func NewBackups(c BackupConfig) (*Backups, error) {
	if c.Worker == nil || c.Objects == nil || c.Catalog == nil || c.Store == nil || c.Snapshots == nil || c.Leases == nil || c.Recoveries == nil || c.Fence == nil {
		return nil, fmt.Errorf("%w: a worker, objects, a catalog, a store, snapshots, leases, recoveries and a fence are required", ErrNotConfigured)
	}
	o := c.Options
	if o.Keep == 0 {
		o.Keep = 7
	}
	if o.Keep < 1 {
		return nil, errors.New("gitops: keep at least one backup")
	}
	if o.LeaseTTL == 0 {
		o.LeaseTTL = 15 * time.Minute
	}
	if o.RenewEvery == 0 {
		o.RenewEvery = o.LeaseTTL / 3
	}
	if o.RenewEvery <= 0 || o.RenewEvery >= o.LeaseTTL {
		return nil, errors.New("gitops: the lease renewal period is positive and shorter than the lease")
	}
	if o.Clock == nil {
		o.Clock = time.Now
	}
	if o.NewID == nil {
		o.NewID = func(now time.Time) string {
			var b [6]byte
			if _, err := rand.Read(b[:]); err != nil {
				panic(err)
			}
			return "bk_" + now.UTC().Format("20060102T150405Z") + "_" + hex.EncodeToString(b[:])
		}
	}
	return &Backups{c: c, o: o}, nil
}

// lazyPut starts the store's Put on the first byte, so a repository without
// refs (nothing written) stores no object.
type lazyPut struct {
	put     func(r io.Reader) error
	pw      *io.PipeWriter
	pr      *io.PipeReader
	hash    interface{ Write([]byte) (int, error) }
	started bool
	n       int64
	done    chan error
	mu      sync.Mutex
}

func (l *lazyPut) Write(p []byte) (int, error) {
	l.mu.Lock()
	if !l.started {
		l.started = true
		l.pr, l.pw = io.Pipe()
		l.done = make(chan error, 1)
		go func() {
			err := l.put(io.TeeReader(l.pr, l.hash))
			l.pr.CloseWithError(errors.Join(err, io.ErrClosedPipe)) // unblocks a writer if Put stopped early
			l.done <- err
		}()
	}
	l.mu.Unlock()
	n, err := l.pw.Write(p)
	l.n += int64(n)
	return n, err
}

// finish ends the stream: with the bundle's error (the upload is abandoned) or
// cleanly (the upload completes). It reports whether anything was written and
// the upload's error.
func (l *lazyPut) finish(bundleErr error) (written bool, err error) {
	l.mu.Lock()
	started := l.started
	l.mu.Unlock()
	if !started {
		return false, nil
	}
	if bundleErr != nil {
		l.pw.CloseWithError(bundleErr)
	} else {
		l.pw.Close()
	}
	return true, <-l.done
}

// Backup takes a volume snapshot and then one bundle per non-empty repository of
// the tenant into object storage and writes the manifest last (GR-15).
//
// It is refused (ErrTenantClosed, retryable) while the tenant has an active
// recovery or closed Git access, before anything is recorded or written, and
// refused again before it publishes if a recovery began (or its fence closed)
// since it started: a backup of a half-restored volume must never become a
// complete backup, which could be the next restore's source.
//
// It holds the tenant's backup lease (BackupLeases) from the start: backups and
// expiry of one tenant never run concurrently, across instances, and the durable
// active marker protects the run's objects from expiry until its lease lapses
// and another run abandons it. A background renewer extends the lease every
// Options.RenewEvery while bundles stream and upload (a long upload must not
// outlast the lease), and cancels the run when the lease is lost; it is also
// renewed after each repository and before the manifest. The run is published
// (Complete) only if it is still active, otherwise the manifest is withdrawn and
// the run fails (ErrLeaseLost). Any failure leaves no manifest: the objects
// written so far are removed (best effort) and the earlier backups stay
// untouched. If Complete returns an error other than a lost lease its outcome is
// unknown: the run's state is read again, a run recorded complete is returned as
// published, and nothing is deleted unless the run is recorded not complete (if
// the state cannot be read the objects stay, and the run, whose lease then
// lapses, is abandoned and expired like any failed one). Only after the run is
// complete are backups beyond Options.Keep removed (manifest first, so a partial
// removal leaves no manifest without its bundles), and only complete backups or
// runs recorded failed are ever removed, never an upload merely because a newer
// manifest exists.
func (b *Backups) Backup(ctx context.Context, tenant domain.TenantID) (m Manifest, err error) {
	if err := tenant.Validate(); err != nil {
		return Manifest{}, err
	}
	startRecovery, err := requireQuiet(ctx, b.c.Fence, b.c.Recoveries, tenant)
	if err != nil {
		return Manifest{}, err
	}
	now := b.o.Clock().UTC()
	id := b.o.NewID(now)
	if err := domain.RunID(id).Validate(); err != nil { // the opaque-ID syntax
		return Manifest{}, fmt.Errorf("gitops: the backup ID is not an ID: %w", err)
	}
	if err := b.c.Leases.Acquire(ctx, tenant, id, b.o.LeaseTTL); err != nil {
		return Manifest{}, err
	}
	parent := ctx
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	stopRenewer := b.renewLease(ctx, cancel, tenant, id)
	var written []string
	published, keep := false, false
	defer func() {
		lost := stopRenewer()
		if lost != nil && err != nil && !errors.Is(err, ErrLeaseLost) {
			err = errors.Join(err, lost) // the cancellation's cause, not just "context canceled"
		}
		if published || keep {
			return
		}
		bg := context.WithoutCancel(parent)
		for _, k := range written {
			_ = b.c.Store.Delete(bg, k)
		}
		_ = b.c.Leases.Fail(bg, tenant, id)
	}()
	snap, err := b.c.Snapshots.Snapshot(ctx, tenant)
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: the volume snapshot: %v", ErrUnavailable, err)
	}
	m = Manifest{Version: ManifestVersion, Tenant: string(tenant), ID: id, CreatedAt: now, Snapshot: ManifestSnapshot{ID: snap.ID, TakenAt: snap.TakenAt.UTC()}}
	err = repositories(ctx, b.c.Catalog, tenant, func(r Repository) error {
		entry, key, err := b.bundle(ctx, tenant, id, r)
		if key != "" {
			written = append(written, key)
		}
		if err != nil {
			return fmt.Errorf("repository %s: %w", r.ID, err)
		}
		m.Repositories = append(m.Repositories, entry)
		return b.c.Leases.Renew(ctx, tenant, id, b.o.LeaseTTL)
	})
	if err != nil {
		return Manifest{}, err
	}
	sort.Slice(m.Repositories, func(i, j int) bool { return m.Repositories[i].ID < m.Repositories[j].ID })
	raw, err := json.Marshal(m)
	if err != nil {
		return Manifest{}, err
	}
	if err := b.c.Leases.Renew(ctx, tenant, id, b.o.LeaseTTL); err != nil {
		return Manifest{}, err
	}
	written = append(written, ManifestKey(tenant, id))
	if err := b.c.Store.Put(ctx, ManifestKey(tenant, id), strings.NewReader(string(raw))); err != nil {
		return Manifest{}, fmt.Errorf("%w: writing the manifest: %v", ErrUnavailable, err)
	}
	// A recovery that began (or a fence that closed) while the bundles were taken
	// makes them a half-restored volume: nothing is published.
	endRecovery, err := requireQuiet(ctx, b.c.Fence, b.c.Recoveries, tenant)
	if err != nil {
		return Manifest{}, err
	}
	if endRecovery != startRecovery {
		return Manifest{}, fmt.Errorf("%w: recovery %s ran during the backup", ErrTenantClosed, endRecovery)
	}
	if err := b.c.Leases.Complete(ctx, tenant, id); err != nil {
		if errors.Is(err, ErrLeaseLost) {
			return Manifest{}, err // the deferred cleanup withdraws the manifest and the bundles
		}
		// The outcome of Complete is unknown: never delete the objects of a run that
		// may be recorded complete.
		bg := context.WithoutCancel(parent)
		state, known, serr := b.c.Leases.State(bg, tenant, id)
		switch {
		case serr != nil:
			keep = true
			return Manifest{}, fmt.Errorf("%w: publishing the backup (outcome unknown, objects kept): %v", ErrUnavailable, errors.Join(err, serr))
		case known && state == BackupComplete:
			published = true // it committed; the error was in the answer
		default:
			return Manifest{}, err
		}
	} else {
		published = true
	}
	_ = b.expire(context.WithoutCancel(parent), tenant, id) // best effort: the next backup repeats it
	return m, nil
}

// renewLease extends the run's lease every Options.RenewEvery until the returned
// stop function is called, which waits for the renewer and returns the error that
// made it give up (nil if it never did). A lost lease (ErrLeaseLost) cancels the
// run at once with that error as the cause. Any other renewal error is treated as
// transient: the renewer tries again at the next tick and gives up (cancelling the
// run with the error as the cause) only when the lease, last extended by the last
// successful renewal, is within about one period of expiring, because a run that
// cannot reach the lease store must not keep writing once another run may
// abandon it. The wait is measured in real time, as the ticker is.
func (b *Backups) renewLease(ctx context.Context, cancel context.CancelCauseFunc, tenant domain.TenantID, id string) (stop func() error) {
	done := make(chan struct{})
	finished := make(chan struct{})
	var failure error
	go func() {
		defer close(finished)
		t := time.NewTicker(b.o.RenewEvery)
		defer t.Stop()
		lastOK := time.Now() // the lease was set by Acquire
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				err := b.c.Leases.Renew(ctx, tenant, id, b.o.LeaseTTL)
				switch {
				case err == nil:
					lastOK = time.Now()
				case ctx.Err() != nil:
					return
				case errors.Is(err, ErrLeaseLost), time.Since(lastOK) >= b.o.LeaseTTL-b.o.RenewEvery:
					failure = err
					cancel(err)
					return
				}
			}
		}
	}()
	return func() error {
		close(done)
		<-finished
		return failure
	}
}

// bundleAttempts is how often a bundle is retaken when a mutation ran while it
// was captured (the worker reports consistent=false) before the backup fails.
const bundleAttempts = 3

func (b *Backups) bundle(ctx context.Context, tenant domain.TenantID, id string, r Repository) (ManifestRepository, string, error) {
	var (
		entry ManifestRepository
		key   string
		err   error
	)
	for range bundleAttempts {
		entry, key, err = b.bundleOnce(ctx, tenant, id, r)
		if !errors.Is(err, errInconsistent) {
			return entry, key, err
		}
	}
	return entry, key, fmt.Errorf("%w: the repository kept changing while it was bundled", ErrUnavailable)
}

var errInconsistent = errors.New("gitops: the bundle was not taken at one boundary")

func (b *Backups) bundleOnce(ctx context.Context, tenant domain.TenantID, id string, r Repository) (ManifestRepository, string, error) {
	entry := ManifestRepository{ID: r.ID, ProjectID: string(r.Scope.ProjectID), DefaultBranch: r.DefaultBranch, Refs: []gitv1.RefTip{}}
	key := backupDir(tenant, id) + r.ID + ".bundle"
	h := sha256.New()
	lp := &lazyPut{put: func(rd io.Reader) error { return b.c.Store.Put(ctx, key, rd) }, hash: h}
	info, berr := b.c.Objects.CreateBundle(ctx, tenant, r.ID, lp)
	wrote, perr := lp.finish(berr)
	switch {
	case errors.Is(berr, ErrEmptyRepository):
		entry.Empty = true
		// Nothing to bundle: the journal head is read on its own.
		defaultRef := gitv1.HeadsPrefix + r.DefaultBranch
		j, err := b.c.Worker.ReadJournal(ctx, tenant, gitv1.ReadJournalRequest{RepositoryID: r.ID, Ref: &defaultRef, SinceSequence: 0, Limit: 1})
		if err != nil {
			return entry, "", fmt.Errorf("reading the journal head: %w", err)
		}
		entry.JournalHead = j.HeadSequence
		return entry, "", nil
	case berr != nil:
		return entry, key, fmt.Errorf("bundling: %w", berr)
	case perr != nil:
		return entry, key, fmt.Errorf("%w: storing the bundle: %v", ErrUnavailable, perr)
	case !wrote:
		return entry, "", fmt.Errorf("gitops: the bundle of %s is empty", r.ID)
	case !info.Consistent:
		_ = b.c.Store.Delete(context.WithoutCancel(ctx), key)
		return entry, "", errInconsistent
	}
	entry.Key, entry.Size, entry.SHA256, entry.Refs, entry.JournalHead = key, lp.n, hex.EncodeToString(h.Sum(nil)), info.Refs, info.JournalHead
	return entry, key, nil
}

// expiryGuard is what expiry must not touch right now: the backup an active
// recovery selected, and everything while a restore has closed Git access but not
// yet selected its source (Restore reads the manifest before it records the
// recovery). A state that cannot be read stops the deletion of complete backups.
func (b *Backups) expiryGuard(ctx context.Context, tenant domain.TenantID) (protect string, stopAll bool, err error) {
	rec, ok, err := b.c.Recoveries.Current(ctx, tenant)
	if err != nil {
		return "", true, err
	}
	if ok && rec.Status == RecoveryActive {
		protect = rec.BackupID
	}
	closed, err := b.c.Fence.ClosedFor(ctx, tenant, FenceRestore)
	if err != nil {
		return "", true, err
	}
	return protect, closed, nil
}

// expire removes complete backups beyond Options.Keep (oldest first, manifest
// first) and the objects of runs recorded failed. It runs after the backup
// completed, without its lease, so a recovery can begin while it runs: the guard
// (expiryGuard) is read when the protected set is computed and **again before each
// complete backup is deleted** (fail closed: a read failure or a closed restore
// fence keeps every remaining complete backup for the next backup's expiry). A
// backup with no run row, or one still active, is never touched, and neither is
// one an active recovery reads. The objects of a failed run are no recovery
// source and are removed regardless.
func (b *Backups) expire(ctx context.Context, tenant domain.TenantID, current string) error {
	keys, err := b.c.Store.List(ctx, BackupPrefix(tenant))
	if err != nil {
		return err
	}
	byID := map[string][]string{}
	for _, k := range keys {
		rest := strings.TrimPrefix(k, BackupPrefix(tenant))
		if id, _, ok := strings.Cut(rest, "/"); ok {
			byID[id] = append(byID[id], k)
		}
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	protected := map[string]bool{current: true}
	if p, stop, err := b.expiryGuard(ctx, tenant); err != nil {
		return err
	} else if stop {
		return fmt.Errorf("%w: restore in progress; expiry deferred", ErrTenantClosed)
	} else if p != "" {
		protected[p] = true
	}
	var complete, failed []string
	for _, id := range ids {
		state, known, err := b.c.Leases.State(ctx, tenant, id)
		if err != nil {
			return err
		}
		switch {
		case !known:
		case state == BackupComplete:
			complete = append(complete, id)
		case state == BackupFailed:
			failed = append(failed, id)
		}
	}
	keep := map[string]bool{}
	for _, id := range complete[max(0, len(complete)-b.o.Keep):] {
		keep[id] = true
	}
	var doomed []string
	for _, id := range complete {
		if !keep[id] && !protected[id] {
			doomed = append(doomed, id)
		}
	}
	isComplete := map[string]bool{}
	for _, id := range doomed {
		isComplete[id] = true
	}
	doomed = append(doomed, failed...)
	var errs []error
	for _, id := range doomed {
		if isComplete[id] {
			p, stop, err := b.expiryGuard(ctx, tenant)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if stop {
				errs = append(errs, fmt.Errorf("%w: restore in progress; expiry deferred", ErrTenantClosed))
				continue
			}
			if p == id {
				continue
			}
		}
		objs := byID[id]
		sort.Slice(objs, func(i, j int) bool {
			return strings.HasSuffix(objs[i], "/manifest.json") && !strings.HasSuffix(objs[j], "/manifest.json")
		})
		for _, k := range objs {
			if err := b.c.Store.Delete(ctx, k); err != nil {
				errs = append(errs, err)
				break // keep the rest: the next run continues
			}
		}
	}
	return errors.Join(errs...)
}

// Backups lists the IDs of the tenant's complete backups, oldest first: a
// manifest counts only when its run is recorded complete.
func (b *Backups) Backups(ctx context.Context, tenant domain.TenantID) ([]string, error) {
	keys, err := b.c.Store.List(ctx, BackupPrefix(tenant))
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, k := range keys {
		rest := strings.TrimPrefix(k, BackupPrefix(tenant))
		if id, name, ok := strings.Cut(rest, "/"); ok && name == "manifest.json" {
			if state, known, err := b.c.Leases.State(ctx, tenant, id); err != nil {
				return nil, err
			} else if known && state == BackupComplete {
				ids = append(ids, id)
			}
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// Manifest reads and validates a complete backup's manifest. An empty ID is the
// newest backup.
func (b *Backups) Manifest(ctx context.Context, tenant domain.TenantID, id string) (Manifest, error) {
	if id == "" {
		ids, err := b.Backups(ctx, tenant)
		if err != nil {
			return Manifest{}, err
		}
		if len(ids) == 0 {
			return Manifest{}, fmt.Errorf("%w: the tenant has no complete backup", ErrBackupNotFound)
		}
		id = ids[len(ids)-1]
	}
	if state, known, err := b.c.Leases.State(ctx, tenant, id); err != nil {
		return Manifest{}, err
	} else if !known || state != BackupComplete {
		return Manifest{}, fmt.Errorf("%w: backup %s is not complete", ErrBackupNotFound, id)
	}
	rc, err := b.c.Store.Open(ctx, ManifestKey(tenant, id))
	if err != nil {
		return Manifest{}, err
	}
	defer rc.Close()
	var m Manifest
	dec := json.NewDecoder(io.LimitReader(rc, 64<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("%w: manifest: %v", ErrBackupCorrupt, err)
	}
	if m.ID != id {
		return Manifest{}, fmt.Errorf("%w: the manifest names another backup", ErrBackupCorrupt)
	}
	if err := m.Validate(tenant); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// VerifyBundle re-reads the stored bundle of a manifest entry and checks its
// size and digest.
func (b *Backups) VerifyBundle(ctx context.Context, e ManifestRepository) error {
	rc, err := b.c.Store.Open(ctx, e.Key)
	if err != nil {
		return err
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, rc)
	if err != nil {
		return err
	}
	if n != e.Size || hex.EncodeToString(h.Sum(nil)) != e.SHA256 {
		return fmt.Errorf("%w: the bundle of %s does not match its manifest", ErrBackupCorrupt, e.ID)
	}
	return nil
}
