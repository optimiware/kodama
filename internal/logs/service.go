// Package logs makes attempt logs available without requiring Loki: it
// authorizes one specific attempt, reads the live Kubernetes log through a
// server-constructed query, bounds and redacts the output, serves retained log
// artifacts, and keeps explicit completeness evidence about each capture
// (docs/operations/observability-and-audit.md).
//
// The package holds the capture entity (Record), pure redaction and bounded
// streaming, the use cases (Service for reads, Controller for the server-side
// fallback capture) and small consumer-owned ports. It imports neither pgx,
// net/http, Kubernetes nor the artifact module's types: PostgreSQL (postgres/),
// Kubernetes (kubernetes/), HTTP (http/) and the identity, execution and
// artifact bridges (adapters/) are adapters wired by the composition root
// (T-071). Log completeness is explicit and never promised after node loss.
package logs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// Limits are the explicit ceilings of one read. They are constructor
// configuration: no environment variable or project setting changes them.
type Limits struct {
	// DefaultTail applies when the caller gives no tail (default 100).
	DefaultTail int
	// MaxTail bounds the caller's tail (default MaxLines).
	MaxTail int
	// MaxLines and MaxBytes bound the output of one read. Required.
	MaxLines int
	MaxBytes int64
	// MaxLineBytes bounds one emitted line; longer lines are cut (default 8 KiB).
	MaxLineBytes int
	// MaxScanBytes bounds the bytes consumed from the source (default 16*MaxBytes).
	MaxScanBytes int64
	// MaxDuration bounds the lifetime of one read, follow included. Required.
	MaxDuration time.Duration
	// MaxStreams bounds concurrent reads. Required.
	MaxStreams int
	// WriteTimeout is the longest a write to the client may block. Required;
	// the HTTP adapter enforces it.
	WriteTimeout time.Duration
	// ReauthInterval is how often a long follow rechecks the caller's access
	// (default 30s).
	ReauthInterval time.Duration
}

// DefaultTailLines is the documented conservative default tail.
const DefaultTailLines = 100

func (l Limits) normalized() (Limits, error) {
	if l.MaxLines <= 0 || l.MaxBytes <= 0 || l.MaxDuration <= 0 || l.MaxStreams <= 0 || l.WriteTimeout <= 0 {
		return l, fmt.Errorf("%w: MaxLines, MaxBytes, MaxDuration, MaxStreams and WriteTimeout must be positive", ErrNotConfigured)
	}
	if l.MaxTail == 0 {
		l.MaxTail = l.MaxLines
	}
	if l.DefaultTail == 0 {
		l.DefaultTail = min(DefaultTailLines, l.MaxTail)
	}
	if l.MaxLineBytes == 0 {
		l.MaxLineBytes = int(min(int64(8<<10), l.MaxBytes))
	}
	if l.MaxScanBytes == 0 {
		l.MaxScanBytes = 16 * l.MaxBytes
	}
	if l.ReauthInterval == 0 {
		l.ReauthInterval = 30 * time.Second
	}
	switch {
	case l.DefaultTail < 1 || l.DefaultTail > l.MaxTail || l.MaxTail > l.MaxLines:
		return l, fmt.Errorf("%w: tail bounds must satisfy 1 <= DefaultTail <= MaxTail <= MaxLines", ErrNotConfigured)
	case l.MaxLineBytes < 1 || int64(l.MaxLineBytes) > l.MaxBytes || l.MaxScanBytes < l.MaxBytes:
		return l, fmt.Errorf("%w: byte bounds are inconsistent", ErrNotConfigured)
	case l.ReauthInterval < time.Second:
		return l, fmt.Errorf("%w: ReauthInterval is too short", ErrNotConfigured)
	}
	return l, nil
}

// Config wires a Service. Every security-critical port is required.
type Config struct {
	Authority Authority
	Attempts  Attempts
	Workloads Workloads
	Live      LiveReader
	Retained  Retained
	Store     Store
	// Redactions is optional: without it no known value is removed. An error
	// from it fails the read closed.
	Redactions Redactions
	Limits     Limits
	Clock      Clock
}

// Service implements the read use cases.
type Service struct {
	cfg Config
	lim Limits
	sem chan struct{}
	now func() time.Time
}

// NewService requires the ports whose absence would make a check unsafe.
func NewService(cfg Config) (*Service, error) {
	missing := func(name string) error { return fmt.Errorf("%w: %s is required", ErrNotConfigured, name) }
	switch {
	case cfg.Authority == nil:
		return nil, missing("authority")
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
	}
	lim, err := cfg.Limits.normalized()
	if err != nil {
		return nil, err
	}
	s := &Service{cfg: cfg, lim: lim, sem: make(chan struct{}, lim.MaxStreams), now: cfg.Clock}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	return s, nil
}

// Limits returns the effective, normalized ceilings.
func (s *Service) Limits() Limits { return s.lim }

// Mode selects where a read comes from.
type Mode string

const (
	// ModeAuto serves the live log of a running attempt and the retained log of
	// a finished one, falling back to the other when the first is unavailable.
	ModeAuto Mode = "auto"
	// ModeLive serves only the Kubernetes log.
	ModeLive Mode = "live"
	// ModeWrapper and ModeController serve only that source's retained capture.
	ModeWrapper    Mode = "wrapper"
	ModeController Mode = "controller"
)

// Valid reports whether m is a known mode.
func (m Mode) Valid() bool {
	switch m {
	case ModeAuto, ModeLive, ModeWrapper, ModeController:
		return true
	}
	return false
}

// Request is one read. The caller supplies none of namespace, Pod, container,
// selector or backend URL: those are resolved server-side.
type Request struct {
	Principal string
	Scope     domain.Scope
	Run       domain.RunID
	// Attempt may be empty when the run has exactly one attempt; otherwise it
	// must be explicit.
	Attempt domain.AttemptID
	// Tail is the number of trailing lines; nil selects the default.
	Tail   *int
	Follow bool
	Mode   Mode
}

// Origin says where the served bytes came from.
type Origin string

const (
	OriginLive       Origin = "live"
	OriginWrapper    Origin = "wrapper"
	OriginController Origin = "controller"
)

// Meta is the first record of a read.
type Meta struct {
	Run     domain.RunID
	Attempt domain.AttemptID
	Step    string
	Origin  Origin
	// Completeness is the retained capture's evidence. A live read is Unknown:
	// stream end, rotation and node loss never manufacture complete evidence.
	Completeness Completeness
	Reasons      []Reason
	Tail         int
	Follow       bool
	// Terminal reports that the attempt had finished when the read began.
	Terminal bool
	Limits   QueryLimits
}

// QueryLimits echo the bounds applied to this read.
type QueryLimits struct {
	MaxBytes        int64
	MaxLines        int
	MaxLineBytes    int
	MaxDurationSecs int64
}

// Chunk is one line of output.
type Chunk struct {
	Text string
	// LineTruncated is true when the line was cut at the line bound.
	LineTruncated bool
}

// StopReason says why a read ended.
type StopReason string

const (
	StopEnd           StopReason = "end"
	StopByteLimit     StopReason = "byte_limit"
	StopLineLimit     StopReason = "line_limit"
	StopDurationLimit StopReason = "duration_limit"
	StopScanLimit     StopReason = "scan_limit"
	StopInterrupted   StopReason = "interrupted"
	StopAuthorization StopReason = "authorization_changed"
	StopUnavailable   StopReason = "unavailable"
)

// Summary is the last record of a read. Its absence means the stream was
// interrupted.
type Summary struct {
	Lines int
	Bytes int64
	// Truncated reports that a bound, not the end of the source, ended or cut the
	// output.
	Truncated bool
	Stop      StopReason
}

// Sink receives the records of a read. An error from any method ends the read
// (a slow or gone client) and the upstream is closed.
type Sink interface {
	Begin(Meta) error
	Chunk(Chunk) error
	End(Summary) error
}

// Session is one prepared read. Everything that can fail before output starts
// (authorization, attempt resolution, workload verification, opening the
// source) has happened in Open, so a transport can still answer with an
// ordinary error.
type Session struct {
	Meta Meta

	svc       *Service
	req       Request
	src       io.ReadCloser
	cancel    context.CancelFunc
	runCtx    context.Context
	release   func()
	redact    *Redactor
	live      bool
	lastCheck time.Time
	closed    bool
}

// Open authorizes the caller for diagnostics of the attempt, resolves it,
// acquires a stream slot and opens the source. The caller must Close the
// session. The authority is asked on every Open, so a revoked membership
// denies reconnects.
func (s *Service) Open(ctx context.Context, req Request) (*Session, error) {
	if err := s.validate(&req); err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, req); err != nil {
		return nil, err
	}
	att, err := s.attempt(ctx, req)
	if err != nil {
		return nil, err
	}
	if req.Follow && att.Terminal {
		return nil, invalidf("follow is only available for a running attempt")
	}
	select {
	case s.sem <- struct{}{}:
	default:
		return nil, ErrTooManyStreams
	}
	var once bool
	release := func() {
		if !once {
			once = true
			<-s.sem
		}
	}
	ok := false
	defer func() {
		if !ok {
			release()
		}
	}()

	red, err := s.redactor(ctx, att)
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithTimeout(ctx, s.lim.MaxDuration)
	defer func() {
		if !ok {
			cancel()
		}
	}()
	sess := &Session{svc: s, req: req, cancel: cancel, runCtx: runCtx, release: release, redact: red, lastCheck: s.now()}
	if err := sess.openSource(runCtx, att); err != nil {
		return nil, err
	}
	sess.Meta.Run, sess.Meta.Attempt, sess.Meta.Step, sess.Meta.Follow, sess.Meta.Terminal = att.Run, att.ID, att.Step, req.Follow, att.Terminal
	sess.Meta.Limits = QueryLimits{MaxBytes: s.lim.MaxBytes, MaxLines: s.lim.MaxLines, MaxLineBytes: s.lim.MaxLineBytes, MaxDurationSecs: int64(s.lim.MaxDuration / time.Second)}
	ok = true
	return sess, nil
}

func (s *Service) validate(req *Request) error {
	if req.Scope.Validate() != nil || req.Run.Validate() != nil || req.Principal == "" {
		return ErrNotFound
	}
	if req.Attempt != "" && req.Attempt.Validate() != nil {
		return invalidf("attemptId is not valid")
	}
	if req.Mode == "" {
		req.Mode = ModeAuto
	}
	if !req.Mode.Valid() {
		return invalidf("unknown source")
	}
	if req.Follow && req.Mode != ModeAuto && req.Mode != ModeLive {
		return invalidf("follow is only available for the live log")
	}
	if req.Tail != nil && (*req.Tail < 1 || *req.Tail > s.lim.MaxTail) {
		return invalidf("tail must be 1..%d", s.lim.MaxTail)
	}
	return nil
}

func (s *Service) tail(req Request) int {
	if req.Tail != nil {
		return *req.Tail
	}
	return s.lim.DefaultTail
}

func (s *Service) authorize(ctx context.Context, req Request) error {
	err := s.cfg.Authority.AuthorizeRead(ctx, req.Principal, req.Scope)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrDenied):
		return err
	}
	return Unavailablef(err, "the authority is unavailable")
}

// attempt resolves the attempt, binding it to the run in the caller's scope.
func (s *Service) attempt(ctx context.Context, req Request) (AttemptInfo, error) {
	if req.Attempt != "" {
		a, err := s.cfg.Attempts.Attempt(ctx, req.Scope, req.Run, req.Attempt)
		return a, classifyLookup(err)
	}
	all, err := s.cfg.Attempts.Attempts(ctx, req.Scope, req.Run)
	if err != nil {
		return AttemptInfo{}, classifyLookup(err)
	}
	switch len(all) {
	case 0:
		return AttemptInfo{}, ErrNotFound
	case 1:
		return all[0], nil
	}
	return AttemptInfo{}, invalidf("the run has several attempts: attemptId is required")
}

func classifyLookup(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrInvalid):
		return err
	}
	return Unavailablef(err, "attempt lookup")
}

func (s *Service) redactor(ctx context.Context, att AttemptInfo) (*Redactor, error) {
	if s.cfg.Redactions == nil {
		return nil, nil
	}
	vals, err := s.cfg.Redactions.Values(ctx, att.Scope, att.ID)
	if err != nil {
		return nil, Unavailablef(err, "redaction values")
	}
	r, err := NewRedactor(vals)
	if err != nil {
		return nil, Unavailablef(err, "redaction values")
	}
	return r, nil
}

// openSource picks and opens the source according to the mode.
func (sess *Session) openSource(ctx context.Context, att AttemptInfo) error {
	s, mode := sess.svc, sess.req.Mode
	tail := s.tail(sess.req)
	sess.Meta.Tail = tail
	tryLive := mode == ModeLive || (mode == ModeAuto && !att.Terminal)
	tryRetained := mode != ModeLive
	var liveErr error
	if tryLive {
		liveErr = sess.openLive(ctx, att, tail)
		if liveErr == nil {
			return nil
		}
		// Only a missing or not-yet-existing workload falls back to retained
		// logs; an outage or integrity failure is reported, not hidden.
		if mode != ModeAuto || !(errors.Is(liveErr, ErrWorkloadMissing) || errors.Is(liveErr, ErrWorkloadNotObserved)) {
			return liveErr
		}
	}
	if tryRetained {
		err := sess.openRetained(ctx, att)
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrNoLog) && mode == ModeAuto && !att.Terminal {
			return liveErr
		}
		if errors.Is(err, ErrNoLog) && mode == ModeAuto && att.Terminal {
			// A finished attempt's Job may still exist: try the live log last.
			if lerr := sess.openLive(ctx, att, tail); lerr == nil {
				return nil
			}
		}
		return err
	}
	return liveErr
}

func (sess *Session) openLive(ctx context.Context, att AttemptInfo, tail int) error {
	s := sess.svc
	target, err := s.cfg.Workloads.Target(ctx, att)
	if err != nil {
		switch {
		case errors.Is(err, ErrWorkloadNotObserved), errors.Is(err, ErrWorkloadMissing), errors.Is(err, ErrNotFound):
			if errors.Is(err, ErrNotFound) {
				return ErrWorkloadMissing
			}
			return err
		}
		return Unavailablef(err, "workload placement")
	}
	rc, err := s.cfg.Live.Open(ctx, target, LiveQuery{TailLines: int64(tail), LimitBytes: s.lim.MaxScanBytes, Follow: sess.req.Follow})
	if err != nil {
		switch {
		case errors.Is(err, ErrWorkloadMissing):
			return ErrWorkloadMissing
		case errors.Is(err, ErrWorkloadMismatch):
			// The object found is not this attempt's: never serve it, never say why.
			return Unavailablef(nil, "workload verification")
		}
		return Unavailablef(err, "live log")
	}
	sess.src, sess.live = rc, true
	sess.Meta.Origin, sess.Meta.Completeness, sess.Meta.Reasons = OriginLive, Unknown, nil
	return nil
}

func (sess *Session) openRetained(ctx context.Context, att AttemptInfo) error {
	s, mode := sess.svc, sess.req.Mode
	recs, err := s.cfg.Store.ForAttempt(ctx, att.Scope, att.ID)
	if err != nil {
		return Unavailablef(err, "log records")
	}
	var candidates []*Record
	for _, r := range recs {
		if mode == ModeWrapper && r.Source != SourceWrapper || mode == ModeController && r.Source != SourceController {
			continue
		}
		candidates = append(candidates, r)
	}
	var link Link
	var origin Origin
	if rec := Select(candidates); rec != nil {
		link, origin = *rec.Link, Origin(rec.Source)
	} else if mode == ModeAuto || mode == ModeWrapper {
		// A wrapper upload nobody recorded yet: read it from its artifacts. Its
		// completeness is the verified sidecar's, or unknown without one.
		l, err := s.cfg.Retained.WrapperLog(ctx, att.Scope, att.ID)
		if err != nil {
			if errors.Is(err, ErrNoLog) || errors.Is(err, ErrNotFound) {
				return ErrNoLog
			}
			return Unavailablef(err, "retained log")
		}
		link, origin = l, OriginWrapper
	} else {
		return ErrNoLog
	}
	rc, err := s.cfg.Retained.Open(ctx, att.Scope, att.ID, link.Log)
	if err != nil {
		switch {
		case errors.Is(err, ErrNoLog), errors.Is(err, ErrNotFound):
			return ErrNoLog
		}
		return Unavailablef(err, "retained log")
	}
	sess.src = rc
	sess.Meta.Origin, sess.Meta.Completeness, sess.Meta.Reasons = origin, link.Completeness, append([]Reason(nil), link.Reasons...)
	return nil
}

// Close releases the upstream stream, the lifetime context and the slot. It is
// safe to call more than once and on every exit path.
func (sess *Session) Close() {
	if sess == nil || sess.closed {
		return
	}
	sess.closed = true
	if sess.src != nil {
		_ = sess.src.Close()
	}
	sess.cancel()
	sess.release()
}

// Run streams the session to sink: Begin, the chunks, then End. A sink error
// (slow or departed client) ends the read at once and is returned; an upstream
// failure after output started is reported in the summary as a classified stop
// reason, never as raw provider text.
func (sess *Session) Run(ctx context.Context, sink Sink) error {
	defer sess.Close()
	// Close the source the moment the lifetime ends, even if its Read ignores
	// context cancellation.
	stop := context.AfterFunc(sess.runCtx, func() { _ = sess.src.Close() })
	defer stop()

	if err := sink.Begin(sess.Meta); err != nil {
		return err
	}
	lim := sess.svc.lim
	reader := newRedactingReader(io.Reader(sess.src), sess.redact)
	lr := newLineReader(reader, lim.MaxLineBytes, lim.MaxScanBytes)

	var sum Summary
	emit := func(text string, cut bool) (bool, error) {
		if sum.Lines >= lim.MaxLines {
			sum.Stop, sum.Truncated = StopLineLimit, true
			return false, nil
		}
		if sum.Bytes+int64(len(text)) > lim.MaxBytes {
			sum.Stop, sum.Truncated = StopByteLimit, true
			return false, nil
		}
		if err := sink.Chunk(Chunk{Text: text, LineTruncated: cut}); err != nil {
			return false, err
		}
		sum.Lines++
		sum.Bytes += int64(len(text))
		if cut {
			sum.Truncated = true
		}
		return true, nil
	}

	var win *window
	if !sess.live {
		win = &window{n: sess.Meta.Tail, maxBytes: int(lim.MaxBytes)}
	}
	for sum.Stop == "" {
		line, cut, err := lr.next()
		if err != nil {
			sum.Stop = sess.stopFor(ctx, err)
			if sum.Stop == "" {
				return ctx.Err() // the client is gone: there is nobody to summarize for
			}
			break
		}
		text := sanitize(line)
		if win != nil {
			win.push(text, cut)
			continue
		}
		if ok, err := emit(text, cut); err != nil {
			return err
		} else if !ok {
			break
		}
		if sess.Meta.Follow && !sess.recheck(ctx, &sum) {
			break
		}
	}
	if win != nil && sum.Stop != StopAuthorization {
		for _, l := range win.lines {
			ok, err := emit(l.text, l.truncated)
			if err != nil {
				return err
			}
			if !ok {
				break
			}
		}
		if win.evicted {
			sum.Truncated = true
		}
	}
	if sum.Stop == StopScanLimit {
		sum.Truncated = true
	}
	return sink.End(sum)
}

// stopFor classifies the end of the line reader. An empty result means the
// request context ended: the client left.
func (sess *Session) stopFor(ctx context.Context, err error) StopReason {
	switch {
	case errors.Is(err, io.EOF):
		return StopEnd
	case errors.Is(err, errScanLimit):
		return StopScanLimit
	case ctx.Err() != nil:
		return ""
	case errors.Is(sess.runCtx.Err(), context.DeadlineExceeded):
		return StopDurationLimit
	}
	return StopInterrupted
}

// recheck asks the authority again once per interval during a follow, so a
// revoked membership ends a long-lived stream, not only reconnects.
func (sess *Session) recheck(ctx context.Context, sum *Summary) bool {
	now := sess.svc.now()
	if now.Sub(sess.lastCheck) < sess.svc.lim.ReauthInterval {
		return true
	}
	sess.lastCheck = now
	err := sess.svc.authorize(ctx, sess.req)
	switch {
	case err == nil:
		return true
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrDenied):
		sum.Stop = StopAuthorization
	default:
		sum.Stop = StopUnavailable
	}
	return false
}
