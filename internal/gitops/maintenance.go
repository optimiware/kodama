package gitops

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Options tune the scheduler. The zero value selects the defaults.
type Options struct {
	// MaintenanceInterval is the cadence of git maintenance per repository
	// (default 24h, at least 1 minute).
	MaintenanceInterval time.Duration
	// BranchRetention is how old a merged branch's tip commit must be before the
	// branch is pruned (default 30 days).
	BranchRetention time.Duration
	// MaxPrunePerRepository bounds the branches deleted per pass (default 100).
	MaxPrunePerRepository int
	// IntentTTL is how long a prune reservation lasts before it expires on its own
	// (default 5 minutes); it only matters after a crash or an unknown outcome.
	IntentTTL time.Duration
	// Owner identifies this scheduler's prune reservations (PruneIntents): another
	// owner's live reservation of a ref makes this pruner leave the ref alone. It
	// defaults to a random per-process value; a restarted process therefore waits
	// for its predecessor's unreleased reservation to expire (IntentTTL).
	Owner string
	// PollInterval is the period of Run (default 5 minutes, at most the
	// maintenance interval).
	PollInterval time.Duration
	// Clock defaults to time.Now.
	Clock func() time.Time
}

// SchedulerConfig holds the collaborators. Worker, Catalog, Fence and Recoveries
// are required; pruning additionally needs Holds, Objects and Intents (without
// any of them a pass maintains and does not prune). Maintenance and pruning skip a
// tenant that has an active recovery or closed Git access (the fence and the
// recovery store say so), and fail closed when those cannot be read.
type SchedulerConfig struct {
	Worker  Worker
	Catalog Catalog
	Holds   Holds
	Objects Objects
	// Intents is the reservation shared with hold acquisition and with other
	// pruners; pruning without it is refused.
	Intents    PruneIntents
	Fence      AccessFence
	Recoveries RecoveryStore
	Options    Options
}

// Scheduler runs git maintenance and branch pruning for every repository
// through the tenants' workers.
type Scheduler struct {
	c SchedulerConfig
	o Options

	mu   sync.Mutex
	lock map[string]*sync.Mutex
	done map[string]int64 // repository -> maintenance slot completed
}

// NewScheduler validates the configuration.
func NewScheduler(c SchedulerConfig) (*Scheduler, error) {
	if c.Worker == nil || c.Catalog == nil || c.Fence == nil || c.Recoveries == nil {
		return nil, fmt.Errorf("%w: a worker, a catalog, a fence and recoveries are required", ErrNotConfigured)
	}
	o := c.Options
	if o.MaintenanceInterval == 0 {
		o.MaintenanceInterval = 24 * time.Hour
	}
	if o.MaintenanceInterval < time.Minute {
		return nil, errors.New("gitops: the maintenance interval is at least one minute")
	}
	if o.BranchRetention == 0 {
		o.BranchRetention = 30 * 24 * time.Hour
	}
	if o.BranchRetention < 0 {
		return nil, errors.New("gitops: the branch retention must not be negative")
	}
	if o.MaxPrunePerRepository == 0 {
		o.MaxPrunePerRepository = 100
	}
	if o.IntentTTL == 0 {
		o.IntentTTL = 5 * time.Minute
	}
	if o.PollInterval == 0 {
		o.PollInterval = 5 * time.Minute
	}
	o.PollInterval = min(o.PollInterval, o.MaintenanceInterval)
	if o.Clock == nil {
		o.Clock = time.Now
	}
	if o.Owner == "" {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			panic(err)
		}
		o.Owner = "pruner_" + hex.EncodeToString(b[:])
	}
	return &Scheduler{c: c, o: o, lock: map[string]*sync.Mutex{}, done: map[string]int64{}}, nil
}

func (s *Scheduler) now() time.Time { return s.o.Clock().UTC() }

func (s *Scheduler) repoLock(tenant domain.TenantID, id string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := string(tenant) + "/" + id
	l, ok := s.lock[k]
	if !ok {
		l = &sync.Mutex{}
		s.lock[k] = l
	}
	return l
}

// Slot is the maintenance slot of an instant: maintenance is requested once per
// slot, under an operation ID derived from the repository and the slot, so a
// repeat inside the slot (a retry, a restart, a second control-plane instance)
// is the identical, idempotent request.
func (s *Scheduler) slot(t time.Time) int64 {
	return t.Unix() / int64(s.o.MaintenanceInterval/time.Second)
}

// RepositoryReport is what one pass did to one repository.
type RepositoryReport struct {
	Repository Repository
	// Maintained is true when the worker completed maintenance in this pass;
	// Skipped when this slot's maintenance had already completed.
	Maintained, Skipped bool
	// Closed is set when the tenant has an active recovery or closed Git access:
	// nothing was pruned or maintained, and the next pass repeats it. It is not a
	// failure, and Err stays nil; Err carries a failure to read the state.
	Closed error
	Prune  PruneResult
	Err    error
}

// MaintainRepository runs the repository's pruning and then its maintenance
// through the worker, serialized with every other operation on the repository
// here (the worker serializes it with pushes). It is idempotent per maintenance
// slot. The assignment of kind maintain that a worker polls (T-132) calls it. A
// failure of pruning does not skip maintenance; both errors are returned.
func (s *Scheduler) MaintainRepository(ctx context.Context, repo Repository) RepositoryReport {
	l := s.repoLock(repo.Scope.TenantID, repo.ID)
	l.Lock()
	defer l.Unlock()
	rep := RepositoryReport{Repository: repo}
	if _, err := requireQuiet(ctx, s.c.Fence, s.c.Recoveries, repo.Scope.TenantID); err != nil {
		if errors.Is(err, ErrTenantClosed) {
			rep.Closed = err
		} else {
			rep.Err = err
		}
		return rep
	}
	var errs []error
	if s.c.Holds != nil && s.c.Objects != nil && s.c.Intents != nil {
		res, err := s.PruneRepository(ctx, repo)
		rep.Prune = res
		if errors.Is(err, ErrTenantClosed) { // a recovery began during the pass: nothing more is done
			rep.Closed = err
			return rep
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("prune %s: %w", repo.ID, err))
		}
	}
	slot := s.slot(s.now())
	key := string(repo.Scope.TenantID) + "/" + repo.ID
	s.mu.Lock()
	finished := s.done[key] == slot+1 // stored as slot+1 so the zero value means "never"
	s.mu.Unlock()
	if finished {
		rep.Skipped = true
	} else {
		_, err := s.c.Worker.Maintain(ctx, repo.Scope.TenantID, gitv1.MaintainRequest{
			RepositoryID: repo.ID, OperationID: operationID("mnt", string(repo.Scope.TenantID), repo.ID, fmt.Sprint(slot)),
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("maintain %s: %w", repo.ID, err))
		} else {
			rep.Maintained = true
			s.mu.Lock()
			s.done[key] = slot + 1
			s.mu.Unlock()
		}
	}
	rep.Err = errors.Join(errs...)
	return rep
}

// RunOnce maintains every repository of every tenant, one at a time. A failing
// repository or tenant never stops the others; the returned error joins the
// failures and the pass is repeated by the next call (the operations are
// idempotent).
func (s *Scheduler) RunOnce(ctx context.Context) ([]RepositoryReport, error) {
	tenants, err := s.c.Catalog.Tenants(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: listing tenants: %v", ErrUnavailable, err)
	}
	var (
		out  []RepositoryReport
		errs []error
	)
	for _, tenant := range tenants {
		err := repositories(ctx, s.c.Catalog, tenant, func(r Repository) error {
			rep := s.MaintainRepository(ctx, r)
			out = append(out, rep)
			if rep.Err != nil {
				errs = append(errs, rep.Err)
			}
			return nil
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("tenant %s: %w", tenant, err))
		}
		if ctx.Err() != nil {
			break
		}
	}
	return out, errors.Join(errs...)
}

// Run calls RunOnce every Options.PollInterval until ctx ends. Failures are
// handed to onError (may be nil) and never stop the loop.
func (s *Scheduler) Run(ctx context.Context, onError func(error)) {
	t := time.NewTicker(s.o.PollInterval)
	defer t.Stop()
	for {
		if _, err := s.RunOnce(ctx); err != nil && onError != nil {
			onError(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
