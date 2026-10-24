package logs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/logs/contract"
)

// RandomIDs generates identifiers and claim tokens from crypto/rand.
type RandomIDs struct{}

func (RandomIDs) NewRecordID() RecordID { return RecordID("logrec_" + randomHex(12)) }
func (RandomIDs) NewToken() string      { return randomHex(24) }

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("logs: random source failed: %v", err)) // never continue with predictable IDs
	}
	return hex.EncodeToString(b)
}

// ControllerConfig wires the server-side fallback. It is an internal system
// operation: it carries no principal and grants no runtime authority. The
// Retained port must be restricted to trusted, attempt-bound log captures.
type ControllerConfig struct {
	Attempts   Attempts
	Workloads  Workloads
	Live       LiveReader
	Retained   Retained
	Store      Store
	Redactions Redactions
	IDs        IDs
	Clock      Clock
	// CaptureBytes bounds the bytes one capture keeps; more is truncation, which
	// is recorded as a partial capture. Required.
	CaptureBytes int64
	// CaptureTimeout bounds the Kubernetes read. Required.
	CaptureTimeout time.Duration
	// Lease bounds one collector's hold on a capture. Required.
	Lease time.Duration
	// SpoolDir holds the temporary copy of a capture ("" selects os.TempDir).
	SpoolDir string
}

// Controller captures logs server-side when the wrapper could not finalize, and
// observes wrapper uploads. Nothing here changes an attempt's outcome.
type Controller struct {
	cfg ControllerConfig
	now func() time.Time
}

// NewController requires every port and bound; there is no permissive default
// and no automatically started worker.
func NewController(cfg ControllerConfig) (*Controller, error) {
	missing := func(n string) error { return fmt.Errorf("%w: %s is required", ErrNotConfigured, n) }
	switch {
	case cfg.Attempts == nil:
		return nil, missing("attempts")
	case cfg.Workloads == nil:
		return nil, missing("workloads")
	case cfg.Live == nil:
		return nil, missing("live reader")
	case cfg.Retained == nil:
		return nil, missing("retained logs")
	case cfg.Store == nil:
		return nil, missing("store")
	case cfg.CaptureBytes <= 0 || cfg.CaptureTimeout <= 0 || cfg.Lease <= 0:
		return nil, fmt.Errorf("%w: CaptureBytes, CaptureTimeout and Lease must be positive", ErrNotConfigured)
	}
	if cfg.IDs == nil {
		cfg.IDs = RandomIDs{}
	}
	c := &Controller{cfg: cfg, now: cfg.Clock}
	if c.now == nil {
		c.now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.SpoolDir == "" {
		c.cfg.SpoolDir = os.TempDir()
	}
	return c, nil
}

func (c *Controller) clock() time.Time { return c.now().UTC().Truncate(time.Microsecond) }

func (c *Controller) attempt(ctx context.Context, scope domain.Scope, id domain.AttemptID) (AttemptInfo, error) {
	if scope.Validate() != nil || id.Validate() != nil {
		return AttemptInfo{}, ErrNotFound
	}
	a, err := c.cfg.Attempts.ByID(ctx, scope, id)
	return a, classifyLookup(err)
}

func (c *Controller) ensure(ctx context.Context, info AttemptInfo, src Source) (*Record, error) {
	tmpl, err := NewRecord(info.Scope, c.cfg.IDs.NewRecordID(), info.ID, info.Run, info.Step, src, 1, c.clock())
	if err != nil {
		return nil, err
	}
	r, err := c.cfg.Store.Ensure(ctx, *tmpl)
	if err != nil {
		return nil, storeError(err)
	}
	return r, nil
}

func storeError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrBusy), errors.Is(err, ErrClaimLost), errors.Is(err, ErrConflict),
		errors.Is(err, ErrInvalid), errors.Is(err, ErrInvalidTransition), errors.Is(err, ErrGenerationsExhausted):
		return err
	}
	return Unavailablef(err, "log record store")
}

// Observe records the verified linkage of the wrapper's uploaded command log.
// It is idempotent: the identical linkage again succeeds, and a changed one is
// ErrConflict, so a racing or repeated observation never replaces evidence.
// ErrNoLog when the attempt has no usable wrapper log.
func (c *Controller) Observe(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (*Record, error) {
	info, err := c.attempt(ctx, scope, attempt)
	if err != nil {
		return nil, err
	}
	link, err := c.cfg.Retained.WrapperLog(ctx, scope, attempt)
	if err != nil {
		if errors.Is(err, ErrNoLog) {
			return nil, ErrNoLog
		}
		return nil, Unavailablef(err, "wrapper log")
	}
	rec, err := c.ensure(ctx, info, SourceWrapper)
	if err != nil {
		return nil, err
	}
	out, err := c.cfg.Store.Update(ctx, scope, rec.ID, func(r *Record) (bool, Audit, error) {
		changed, err := r.Observe(link, c.clock())
		return changed, Audit{Action: "log.linked"}, err
	})
	return out, storeError(err)
}

// Reconcile marks retained captures whose content retention removed. It changes
// availability only; the recorded completeness stays.
func (c *Controller) Reconcile(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) error {
	if scope.Validate() != nil || attempt.Validate() != nil {
		return ErrNotFound
	}
	recs, err := c.cfg.Store.ForAttempt(ctx, scope, attempt)
	if err != nil {
		return storeError(err)
	}
	for _, r := range recs {
		if !r.Available() {
			continue
		}
		ok, err := c.cfg.Retained.Usable(ctx, scope, attempt, r.Link.Log)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				ok = false
			} else {
				return Unavailablef(err, "retained log")
			}
		}
		if ok {
			continue
		}
		if _, err := c.cfg.Store.Update(ctx, scope, r.ID, func(r *Record) (bool, Audit, error) {
			changed, err := r.MarkRemoved(c.clock())
			return changed, Audit{Action: "log.removed"}, err
		}); err != nil {
			return storeError(err)
		}
	}
	return nil
}

// Collect captures the attempt's live log server-side and retains it with
// explicit completeness evidence. It is safe to call repeatedly and
// concurrently, after a crash, and after a lost response:
//
//   - a retained capture returns unchanged;
//   - a live claim held by another collector is ErrBusy; an expired claim is
//     taken over with a higher fence, and the old collector can no longer commit;
//   - the snapshot of the captured bytes is recorded before any upload, so every
//     retry uploads the same declaration;
//   - if the frozen bytes are lost (the upload never happened and the process
//     restarted), that capture is fenced as unavailable and a bounded new
//     generation recaptures; the earlier failure stays recorded;
//   - a missing workload is recorded as explicit unavailable evidence.
//
// None of these changes the attempt's execution result. No SQL transaction
// spans the Kubernetes read or an object transfer.
func (c *Controller) Collect(ctx context.Context, scope domain.Scope, attempt domain.AttemptID) (*Record, error) {
	info, err := c.attempt(ctx, scope, attempt)
	if err != nil {
		return nil, err
	}
	rec, err := c.ensure(ctx, info, SourceController)
	if err != nil {
		return nil, err
	}
	for {
		switch rec.State {
		case StateRetained:
			return rec, nil
		case StateUnavailable:
			if rec.Failure == FailureFrozenLost {
				next, err := rec.Next(c.cfg.IDs.NewRecordID(), c.clock())
				if err != nil {
					return rec, err // generations exhausted: the failure stays recorded
				}
				if rec, err = c.cfg.Store.Create(ctx, next); err != nil {
					return nil, storeError(err)
				}
				continue
			}
			if !rec.Failure.Retryable() || rec.Claims >= MaxClaims {
				return rec, nil
			}
		}
		claim, started, err := c.begin(ctx, rec)
		if err != nil {
			return started, err
		}
		rec = started
		done, err := c.collect(ctx, info, rec, claim)
		if err != nil {
			return done, err
		}
		rec = done
		if rec.State == StateUnavailable && rec.Failure == FailureFrozenLost {
			continue
		}
		return rec, nil
	}
}

func (c *Controller) begin(ctx context.Context, rec *Record) (Claim, *Record, error) {
	var claim Claim
	token := c.cfg.IDs.NewToken()
	out, err := c.cfg.Store.Update(ctx, rec.Scope, rec.ID, func(r *Record) (bool, Audit, error) {
		cl, err := r.Begin(token, c.cfg.Lease, c.clock())
		claim = cl
		return err == nil, Audit{Action: "log.collecting"}, err
	})
	if err != nil {
		return Claim{}, rec, storeError(err)
	}
	return claim, out, nil
}

// fail records a classified failure under the claim. A fenced-out collector's
// failure is dropped: the current collector owns the record.
func (c *Controller) fail(ctx context.Context, rec *Record, claim Claim, class FailureClass) (*Record, error) {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	out, err := c.cfg.Store.Update(fctx, rec.Scope, rec.ID, func(r *Record) (bool, Audit, error) {
		err := r.Fail(claim, class, c.clock())
		return err == nil, Audit{Action: "log.unavailable"}, err
	})
	if err != nil {
		return rec, storeError(err)
	}
	return out, nil
}

func (c *Controller) collect(ctx context.Context, info AttemptInfo, rec *Record, claim Claim) (*Record, error) {
	snap := rec.Snapshot
	var body io.ReadSeeker
	if snap == nil {
		cap, class, err := c.capture(ctx, info, rec.Generation)
		if err != nil {
			if class != "" {
				out, ferr := c.fail(ctx, rec, claim, class)
				if ferr != nil {
					return out, ferr
				}
				if class == FailureWorkloadMissing || class == FailureLimit {
					return out, nil // explicit unavailable evidence, not an error
				}
				return out, err
			}
			return rec, err
		}
		defer cap.close()
		var changed *Record
		s := cap.snapshot
		changed, err = c.cfg.Store.Update(ctx, rec.Scope, rec.ID, func(r *Record) (bool, Audit, error) {
			ch, err := r.Freeze(claim, s, c.clock())
			return ch, Audit{Action: "log.frozen"}, err
		})
		if err != nil {
			return rec, storeError(err) // fenced out: nothing was uploaded
		}
		rec, snap, body = changed, changed.Snapshot, cap.file
	}
	link, err := c.cfg.Retained.Store(ctx, info, *snap, body)
	if err != nil {
		switch {
		case errors.Is(err, ErrFrozenBytesLost):
			return c.fail(ctx, rec, claim, FailureFrozenLost)
		case ctx.Err() != nil:
			return rec, ctx.Err()
		}
		out, ferr := c.fail(ctx, rec, claim, FailureStore)
		if ferr != nil {
			return out, ferr
		}
		return out, Unavailablef(err, "retain log")
	}
	out, err := c.cfg.Store.Update(ctx, rec.Scope, rec.ID, func(r *Record) (bool, Audit, error) {
		err := r.Retain(claim, link, c.clock())
		return err == nil, Audit{Action: "log.retained"}, err
	})
	if err != nil {
		return rec, storeError(err)
	}
	return out, nil
}

type captured struct {
	file     *os.File
	snapshot Snapshot
}

func (c *captured) close() {
	if c == nil || c.file == nil {
		return
	}
	name := c.file.Name()
	_ = c.file.Close()
	_ = os.Remove(name)
}

// capture reads the attempt's log from Kubernetes into bounded local scratch,
// redacting known values, and freezes its identity. A non-empty class is the
// failure class to record.
func (c *Controller) capture(ctx context.Context, info AttemptInfo, generation int) (*captured, FailureClass, error) {
	target, err := c.cfg.Workloads.Target(ctx, info)
	switch {
	case err == nil:
	case errors.Is(err, ErrWorkloadNotObserved):
		if info.Terminal {
			return nil, FailureWorkloadMissing, err // no workload ever ran
		}
		return nil, FailureBackend, err
	case errors.Is(err, ErrWorkloadMissing), errors.Is(err, ErrNotFound):
		return nil, FailureWorkloadMissing, ErrWorkloadMissing
	default:
		return nil, FailureBackend, Unavailablef(err, "workload placement")
	}
	var red *Redactor
	if c.cfg.Redactions != nil {
		vals, err := c.cfg.Redactions.Values(ctx, info.Scope, info.ID)
		if err == nil {
			red, err = NewRedactor(vals)
		}
		if err != nil {
			// Never capture unredacted bytes when the set cannot be established.
			return nil, FailureBackend, Unavailablef(err, "redaction values")
		}
	}
	rctx, cancel := context.WithTimeout(ctx, c.cfg.CaptureTimeout)
	defer cancel()
	src, err := c.cfg.Live.Open(rctx, target, LiveQuery{LimitBytes: c.cfg.CaptureBytes + 1})
	if err != nil {
		switch {
		case errors.Is(err, ErrWorkloadMissing):
			return nil, FailureWorkloadMissing, ErrWorkloadMissing
		case errors.Is(err, ErrWorkloadMismatch):
			return nil, FailureWorkloadMissing, ErrWorkloadMissing // not ours: nothing to capture
		}
		return nil, FailureBackend, Unavailablef(err, "live log")
	}
	defer src.Close()

	f, err := os.CreateTemp(c.cfg.SpoolDir, "kodama-logcapture-*")
	if err != nil {
		return nil, FailureStore, Unavailablef(err, "scratch space")
	}
	cap := &captured{file: f}
	ok := false
	defer func() {
		if !ok {
			cap.close()
		}
	}()
	h := sha256.New()
	var n int64
	var truncated, interrupted bool
	rd := newRedactingReader(src, red)
	buf := make([]byte, 32<<10)
	for {
		m, rerr := rd.Read(buf)
		if m > 0 {
			room := c.cfg.CaptureBytes - n
			if int64(m) > room {
				m, truncated = int(room), true
			}
			if m > 0 {
				if _, werr := f.Write(buf[:m]); werr != nil {
					return nil, FailureStore, Unavailablef(werr, "scratch space")
				}
				h.Write(buf[:m])
				n += int64(m)
			}
			if truncated {
				break
			}
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				interrupted = true
			}
			break
		}
	}
	if interrupted && n == 0 {
		return nil, FailureBackend, Unavailablef(nil, "live log")
	}
	if interrupted && ctx.Err() != nil {
		return nil, "", ctx.Err()
	}
	reasons := []Reason{contract.ReasonControllerCapture}
	completeness := Unknown
	if truncated {
		reasons, completeness = append(reasons, contract.ReasonTruncated), Partial
	}
	if interrupted {
		reasons, completeness = append(reasons, contract.ReasonInterrupted), Partial
	}
	logName := contract.ControllerLogName(generation)
	metaName, err := contract.MetadataName(logName)
	if err != nil {
		return nil, "", err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, FailureStore, Unavailablef(err, "scratch space")
	}
	cap.snapshot = Snapshot{LogName: logName, MetaName: metaName, Digest: "sha256:" + hex.EncodeToString(h.Sum(nil)), Size: n,
		Statement: Statement{Completeness: completeness, Reasons: reasons}}
	ok = true
	return cap, "", nil
}
