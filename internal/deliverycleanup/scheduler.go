// Package deliverycleanup schedules deployment-bound cleanup (task T-066). It
// turns the deployment records' expiry (DQ-09) and cleanup requests into
// ordinary internal cleanup runs admitted by execution, independently of whether
// the originating work item is done, cancelled or gone. It is a policy over two
// consumer-owned ports and nothing more: it deploys nothing, deletes nothing
// itself, touches no work item, workspace or business WIP, and can name no
// resource except a deployment the record owner reports.
//
// Identity. A cleanup run's identity is derived from (tenant, project,
// deployment, round) by execution (CleanupRunID), so duplicate claims of one
// round - two schedulers, a retry after a lost response, a restart - converge on
// one run. Another round exists only after the previous one ended.
//
// Outcomes. A round that failed with every effect accounted for is retried with
// a backoff, a bounded number of times (PX-07). A round whose external effect
// is unknown, or a succeeded round the record never saw complete, blocks
// automatic retry: Scheduler.Reconcile is the explicit step that admits the
// next round, whose cleanup script observes the recorded objects first.
package deliverycleanup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// Policy is the explicit scheduling policy. Zero fields select the documented
// defaults; they never loosen a safety check.
type Policy struct {
	// RetryBackoff is the base delay after a failed round; round n waits
	// n*RetryBackoff from its end. Default 5 minutes.
	RetryBackoff time.Duration
	// MaxRounds bounds the automatic rounds of one deployment (the first run and
	// its retries). Default 3, at most execution.MaxCleanupRounds. A further round
	// needs the explicit Reconcile.
	MaxRounds int
	// Batch is the page size of record scans. Default 100, at most 200.
	Batch int
	// ScanPages bounds the pages of the deployment scan one sweep reads; the
	// cursor resumes on the next sweep, so every record is reached in turn.
	// Default 4.
	ScanPages int
}

func (p Policy) withDefaults() (Policy, error) {
	if p.RetryBackoff == 0 {
		p.RetryBackoff = 5 * time.Minute
	}
	if p.MaxRounds == 0 {
		p.MaxRounds = 3
	}
	if p.Batch == 0 {
		p.Batch = 100
	}
	if p.ScanPages == 0 {
		p.ScanPages = 4
	}
	if p.RetryBackoff < 0 || p.MaxRounds < 1 || p.MaxRounds > execution.MaxCleanupRounds || p.Batch < 1 || p.Batch > 200 || p.ScanPages < 1 || p.ScanPages > 100 {
		return p, errors.New("deliverycleanup: the policy is out of range")
	}
	return p, nil
}

// Config are the collaborators and policy of a Scheduler. Every port is
// required: a missing one is a construction error, never an allow-all default.
type Config struct {
	Scopes      Scopes
	Records     Records
	Runs        Runs
	Definitions Definitions
	Policy      Policy
	// Clock is the controlled time source (default UTC wall time).
	Clock  func() time.Time
	Logger *slog.Logger
}

// Scheduler admits cleanup runs for expired and requested deployments.
type Scheduler struct {
	cfg Config
	p   Policy
	now func() time.Time
	log *slog.Logger

	mu      sync.Mutex
	cursors map[domain.Scope]string
}

// New validates the configuration.
func New(cfg Config) (*Scheduler, error) {
	switch {
	case cfg.Scopes == nil:
		return nil, errors.New("deliverycleanup: scopes are required")
	case cfg.Records == nil:
		return nil, errors.New("deliverycleanup: the deployment records are required")
	case cfg.Runs == nil:
		return nil, errors.New("deliverycleanup: cleanup run admission is required")
	case cfg.Definitions == nil:
		return nil, errors.New("deliverycleanup: a cleanup definition source is required")
	}
	p, err := cfg.Policy.withDefaults()
	if err != nil {
		return nil, err
	}
	s := &Scheduler{cfg: cfg, p: p, now: cfg.Clock, log: cfg.Logger, cursors: map[domain.Scope]string{}}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	return s, nil
}

// Outcome is what one pass decided for one deployment.
type Outcome string

const (
	// OutcomeAdmitted: this pass created the round's cleanup run.
	OutcomeAdmitted Outcome = "admitted"
	// OutcomeDuplicate: another claimant created the round's run first; nothing
	// was started.
	OutcomeDuplicate Outcome = "duplicate"
	// OutcomeInFlight: the latest round has not ended.
	OutcomeInFlight Outcome = "in_flight"
	// OutcomeBackoff: the latest round failed and its retry is not due.
	OutcomeBackoff Outcome = "backoff"
	// OutcomeUnknown: an effect is unknown; an explicit Reconcile is required.
	OutcomeUnknown Outcome = "unknown_outcome"
	// OutcomeExhausted: the automatic rounds are used; the failure stays a visible
	// exception.
	OutcomeExhausted Outcome = "exhausted"
	// OutcomeNotDue: the deployment is not past its expiry and cleanup was not
	// requested.
	OutcomeNotDue Outcome = "not_due"
	// OutcomeNotSettled: the deployment is still applying; cleanup waits for it.
	OutcomeNotSettled Outcome = "not_settled"
	// OutcomeDeleted: the record already says the objects are deleted.
	OutcomeDeleted Outcome = "deleted"
	// OutcomeDenied: the record owner no longer authorizes a cleanup run.
	OutcomeDenied Outcome = "denied"
	// OutcomeRefused: admission refused the run (for instance an unusable
	// definition); it is visible and repeated on the next pass.
	OutcomeRefused Outcome = "refused"
	// OutcomeError: a dependency was unavailable; the next pass repeats it.
	OutcomeError Outcome = "error"
)

// Item is the decision for one deployment.
type Item struct {
	Scope      domain.Scope
	Deployment string
	Outcome    Outcome
	Round      int
	RunID      domain.RunID
	Err        error
}

// Report lists the decisions of a pass.
type Report struct{ Items []Item }

// Count returns the number of items with the outcome.
func (r Report) Count(o Outcome) int {
	n := 0
	for _, it := range r.Items {
		if it.Outcome == o {
			n++
		}
	}
	return n
}

// Tick sweeps every scope the Scopes port lists. A scope that fails is reported
// and does not stop the others; the joined error is returned beside the report.
func (s *Scheduler) Tick(ctx context.Context) (Report, error) {
	var rep Report
	var errs []error
	var after *domain.Scope
	for {
		page, err := s.cfg.Scopes.Scopes(ctx, after, s.p.Batch)
		if err != nil {
			errs = append(errs, fmt.Errorf("list scopes: %w", err))
			break
		}
		for _, sc := range page {
			r, err := s.Sweep(ctx, sc)
			rep.Items = append(rep.Items, r.Items...)
			if err != nil {
				errs = append(errs, err)
			}
			if ctx.Err() != nil {
				return rep, errors.Join(append(errs, ctx.Err())...)
			}
		}
		if len(page) < s.p.Batch {
			break
		}
		last := page[len(page)-1]
		after = &last
	}
	return rep, errors.Join(errs...)
}

// Loop runs Tick every interval until ctx ends: the worker hook the composition
// root starts. A failing tick is logged and repeated; it never stops the loop.
func (s *Scheduler) Loop(ctx context.Context, every time.Duration) error {
	if every <= 0 {
		return errors.New("deliverycleanup: the interval must be positive")
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if _, err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("deployment cleanup pass incomplete", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Sweep decides the deployments of one scope that are past their expiry or whose
// cleanup was requested, within the pages it scans (ScanPages of Batch records,
// continuing from where the previous sweep stopped). A decision that cannot be made (an outage) is an item
// with OutcomeError; the sweep itself fails only when the records cannot be
// listed.
func (s *Scheduler) Sweep(ctx context.Context, scope domain.Scope) (Report, error) {
	if err := scope.Validate(); err != nil {
		return Report{}, fmt.Errorf("deliverycleanup: %w", err)
	}
	var rep Report
	var errs []error
	seen := map[string]bool{}
	now := s.now()
	s.mu.Lock()
	after := s.cursors[scope]
	s.mu.Unlock()
	// One resumable scan of the scope's records finds both requested cleanups
	// and expired, not yet requested deployments. The cursor survives the sweep,
	// so a page of records that stay blocked is passed over on the next one.
	for pages := 0; pages < s.p.ScanPages; pages++ {
		page, next, err := s.cfg.Records.Scan(ctx, scope, after, s.p.Batch)
		if err != nil {
			errs = append(errs, fmt.Errorf("scan deployments: %w", err))
			break
		}
		for _, c := range page {
			// A record from another scope is never acted on, whatever a port returns.
			if c.Scope != scope || seen[c.Deployment] || c.Deleted || c.State == RecordDone {
				continue
			}
			due := c.RequestedAt != nil || (c.ExpiresAt != nil && !now.Before(*c.ExpiresAt))
			if !due {
				continue
			}
			seen[c.Deployment] = true
			rep.Items = append(rep.Items, s.handle(ctx, c))
		}
		after = next
		if next == "" {
			break
		}
	}
	s.mu.Lock()
	s.cursors[scope] = after
	s.mu.Unlock()
	return rep, errors.Join(errs...)
}

func (s *Scheduler) item(c Candidate, o Outcome) Item {
	return Item{Scope: c.Scope, Deployment: c.Deployment, Outcome: o}
}

func (s *Scheduler) fail(it Item, o Outcome, err error) Item {
	it.Outcome, it.Err = o, err
	s.log.Warn("deployment cleanup not admitted", "tenant", it.Scope.TenantID, "project", it.Scope.ProjectID,
		"deployment", it.Deployment, "outcome", string(o))
	return it
}

// handle decides one deployment.
func (s *Scheduler) handle(ctx context.Context, c Candidate) Item {
	it := s.item(c, OutcomeError)
	now := s.now()
	if c.Deleted || c.State == RecordDone {
		it.Outcome = OutcomeDeleted
		return it
	}
	if c.RequestedAt == nil {
		// Only expiry starts cleanup on its own; the clock decides, not a label.
		if c.ExpiresAt == nil || now.Before(*c.ExpiresAt) {
			it.Outcome = OutcomeNotDue
			return it
		}
		switch err := s.cfg.Records.RequestCleanup(ctx, c.Scope, c.Deployment); {
		case err == nil:
			c.RequestedAt, c.State = &now, RecordRequested
		case errors.Is(err, ErrNotSettled):
			it.Outcome = OutcomeNotSettled
			return it
		case errors.Is(err, ErrNotFound):
			return s.fail(it, OutcomeDenied, err)
		default:
			return s.fail(it, OutcomeError, err)
		}
	}
	rounds, err := s.cfg.Runs.CleanupRounds(ctx, c.Scope, c.Deployment, execution.MaxCleanupRounds)
	if err != nil {
		return s.fail(it, OutcomeError, err)
	}
	if len(rounds) == 0 {
		return s.admit(ctx, c, 1)
	}
	last := rounds[len(rounds)-1]
	it.Round, it.RunID = last.Round, last.RunID
	switch last.Disposition {
	case execution.CleanupInFlight:
		it.Outcome = OutcomeInFlight
		return it
	case execution.CleanupOutcomeUnknown:
		it.Outcome = OutcomeUnknown
		return it
	case execution.CleanupCompleted:
		// The record, not the run, says the objects are gone. The candidate may
		// predate the report, so read it again before declaring the outcome unknown.
		cur, err := s.cfg.Records.Get(ctx, c.Scope, c.Deployment)
		switch {
		case err != nil:
			return s.fail(it, OutcomeError, err)
		case cur.Deleted || cur.State == RecordDone:
			it.Outcome = OutcomeDeleted
		default:
			it.Outcome = OutcomeUnknown
		}
		return it
	}
	// A definite failure. Whether another round is a safe retry is decided on the
	// current record, not the listed one: the script may have reported an unknown
	// outcome since the list was read. A failed lookup fails closed.
	cur, err := s.cfg.Records.Get(ctx, c.Scope, c.Deployment)
	switch {
	case err != nil:
		return s.fail(it, OutcomeError, err)
	case cur.Deleted || cur.State == RecordDone:
		it.Outcome = OutcomeDeleted
		return it
	case cur.State == RecordUnknown || c.State == RecordUnknown:
		it.Outcome = OutcomeUnknown
		return it
	}
	if len(rounds) >= s.p.MaxRounds {
		it.Outcome = OutcomeExhausted
		return it
	}
	if last.FinishedAt != nil && now.Before(last.FinishedAt.Add(time.Duration(len(rounds))*s.p.RetryBackoff)) {
		it.Outcome = OutcomeBackoff
		return it
	}
	return s.admit(ctx, c, len(rounds)+1)
}

// admit claims the round's cleanup run. A concurrent claimant of the same round
// converges on the same run (created=false).
func (s *Scheduler) admit(ctx context.Context, c Candidate, round int) Item {
	it := Item{Scope: c.Scope, Deployment: c.Deployment, Round: round, RunID: execution.CleanupRunID(c.Scope, c.Deployment, round)}
	def, err := s.cfg.Definitions.CleanupDefinition(ctx, c.Scope)
	switch {
	case errors.Is(err, ErrNoDefinition):
		return s.fail(it, OutcomeRefused, err)
	case err != nil:
		return s.fail(it, OutcomeError, err)
	}
	run, created, err := s.cfg.Runs.AdmitCleanupRound(ctx, execution.CleanupCommand{Scope: c.Scope, DeploymentID: c.Deployment, DefinitionID: def}, round)
	switch {
	case err == nil:
	case errors.Is(err, execution.ErrDenied), errors.Is(err, execution.ErrHidden), errors.Is(err, execution.ErrNotFound):
		return s.fail(it, OutcomeDenied, err)
	case errors.Is(err, execution.ErrAdmissionRefused), errors.Is(err, execution.ErrInvalid), errors.Is(err, execution.ErrNotConfigured):
		return s.fail(it, OutcomeRefused, err)
	default:
		return s.fail(it, OutcomeError, err)
	}
	it.RunID = run.ID
	it.Outcome = OutcomeDuplicate
	if created {
		it.Outcome = OutcomeAdmitted
		s.log.Info("deployment cleanup run admitted", "tenant", c.Scope.TenantID, "project", c.Scope.ProjectID,
			"deployment", c.Deployment, "run", run.ID, "round", round)
	}
	return it
}

// Errors of Reconcile.
var (
	// ErrNothingToReconcile: the deployment has no ended cleanup round to follow
	// up, or its cleanup already completed.
	ErrNothingToReconcile = errors.New("deliverycleanup: nothing to reconcile")
	// ErrInFlight: a cleanup round is still running; it settles first.
	ErrInFlight = errors.New("deliverycleanup: a cleanup run is in flight")
	// ErrRoundsExhausted: the deployment used every round execution allows.
	ErrRoundsExhausted = errors.New("deliverycleanup: the cleanup rounds are exhausted")
)

// Reconcile is the explicit follow-up of a cleanup whose outcome is unknown, or
// whose automatic rounds are exhausted: it admits the next round, whose cleanup
// script observes the recorded objects (by their recorded identities and
// annotations) before it deletes or reports. The caller's authority to ask is
// the composition root's concern (it is an internal operation); the record
// owner's current authorization is still checked by admission. Reconciling is
// idempotent per round: a repeat while the round runs is ErrInFlight.
func (s *Scheduler) Reconcile(ctx context.Context, scope domain.Scope, deployment string) (Item, error) {
	if err := scope.Validate(); err != nil {
		return Item{}, fmt.Errorf("deliverycleanup: %w", err)
	}
	c, err := s.cfg.Records.Get(ctx, scope, deployment)
	if err != nil {
		return Item{Scope: scope, Deployment: deployment, Outcome: OutcomeDenied, Err: err}, err
	}
	if c.Scope != scope || c.Deployment != deployment {
		return Item{Scope: scope, Deployment: deployment, Outcome: OutcomeDenied}, ErrNotFound
	}
	it := s.item(c, OutcomeError)
	if c.Deleted || c.State == RecordDone {
		it.Outcome = OutcomeDeleted
		return it, ErrNothingToReconcile
	}
	rounds, err := s.cfg.Runs.CleanupRounds(ctx, scope, deployment, execution.MaxCleanupRounds)
	if err != nil {
		return s.fail(it, OutcomeError, err), err
	}
	if len(rounds) == 0 {
		return it, ErrNothingToReconcile
	}
	last := rounds[len(rounds)-1]
	it.Round, it.RunID = last.Round, last.RunID
	if !last.Status.Terminal() {
		// Open, or held open by an unaccounted effect that execution's own
		// reconciliation must settle first: a second deleter is never started.
		it.Outcome = OutcomeInFlight
		if last.Disposition == execution.CleanupOutcomeUnknown {
			it.Outcome = OutcomeUnknown
		}
		return it, ErrInFlight
	}
	if len(rounds) >= execution.MaxCleanupRounds {
		it.Outcome = OutcomeExhausted
		return it, ErrRoundsExhausted
	}
	next := s.admit(ctx, c, len(rounds)+1)
	return next, next.Err
}
