// Package observation implements Kodama's authorized durable observation
// streams: the session entity that keeps continuation positions monotonic, the
// projection that decides which retained events a stream may carry, and the
// use cases that open a stream or take a snapshot-plus-cursor under current
// authorization.
//
// PostgreSQL observations (internal/events) are authoritative for public
// continuation. NATS delivery and broker sequence are never consulted here, so a
// slow client, a restart or a broker outage cannot change what a cursor means.
// The package holds no pgx, net/http, NATS or identity types; the PostgreSQL
// store, the authority adapter and the SSE/JSON handlers are adapters in
// sub-packages wired by the composition root.
//
// Delivery is at-least-once at the network boundary: after a disconnect a client
// resumes from the last position it received and may see an event again. Event
// IDs are stable, so clients deduplicate by event ID.
package observation

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// MaxPageSize is the largest page a Log read may be asked for (the event
// store's bound).
const MaxPageSize = 1000

// Options bounds a stream. Every value is explicit and validated; there are no
// production defaults here, and the retention period belongs to the event store.
type Options struct {
	// PageSize bounds the observations read per durable read (1..MaxPageSize).
	PageSize int
	// MaxPageBytes bounds the encoded bytes of observations processed per page.
	// A page is cut at the last whole observation within the bound (at least
	// one), and the remainder is read again from the durable log.
	MaxPageBytes int64
	// PollInterval is how often an idle stream polls the durable log.
	PollInterval time.Duration
	// HeartbeatInterval is the longest an idle stream stays silent.
	HeartbeatInterval time.Duration
	// AccessInterval is how often an idle stream rechecks current access.
	AccessInterval time.Duration
	// MaxSnapshotResources bounds a complete snapshot; a larger state fails
	// explicitly instead of being truncated.
	MaxSnapshotResources int
}

// Validate checks every bound.
func (o Options) Validate() error {
	switch {
	case o.PageSize < 1 || o.PageSize > MaxPageSize:
		return misconfigured("page size must be 1..%d", MaxPageSize)
	case o.MaxPageBytes < 1:
		return misconfigured("the page byte bound must be positive")
	case o.PollInterval <= 0 || o.HeartbeatInterval <= 0 || o.AccessInterval <= 0:
		return misconfigured("poll, heartbeat and access intervals must be positive")
	case o.MaxSnapshotResources < 1:
		return misconfigured("the snapshot bound must be positive")
	}
	return nil
}

// Config wires a Service.
type Config struct {
	Log     Log
	State   State
	Access  Access
	Options Options
	// Clock defaults to time.Now.
	Clock func() time.Time
}

// Service opens streams and takes snapshots.
type Service struct {
	log    Log
	state  State
	access Access
	opts   Options
	now    func() time.Time
}

// NewService requires every port and valid options.
func NewService(cfg Config) (*Service, error) {
	if cfg.Log == nil || cfg.State == nil || cfg.Access == nil {
		return nil, misconfigured("a durable log, a state reader and an access checker are required")
	}
	if err := cfg.Options.Validate(); err != nil {
		return nil, err
	}
	now := cfg.Clock
	if now == nil {
		now = time.Now
	}
	return &Service{log: cfg.Log, state: cfg.State, access: cfg.Access, opts: cfg.Options, now: now}, nil
}

// Options returns the validated bounds.
func (s *Service) Options() Options { return s.opts }

// allowedFamilies asks current access for every family of the scope and returns
// those the actor may observe. No family allowed is ErrHidden when any answer
// was hidden, else ErrDenied; an outage is ErrUnavailable and never access.
func (s *Service) allowedFamilies(ctx context.Context, actor Actor, scope Scope) (map[Family]bool, error) {
	allowed := map[Family]bool{}
	hidden := false
	for _, f := range scope.Families() {
		err := s.access.Authorize(ctx, actor, scope, f)
		switch {
		case err == nil:
			allowed[f] = true
		case errors.Is(err, ErrHidden):
			hidden = true
		case errors.Is(err, ErrDenied):
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case errors.Is(err, ErrUnavailable):
			return nil, err
		default:
			return nil, Unavailablef(err, "checking current access")
		}
	}
	if len(allowed) > 0 {
		return allowed, nil
	}
	if hidden {
		return nil, ErrHidden
	}
	return nil, ErrDenied
}

// release filters a snapshot to the families the actor may currently observe.
// It is called after the snapshot was read, immediately before release, so a
// revocation between the read and the response is honored.
func (s *Service) release(ctx context.Context, actor Actor, scope Scope, snap Snapshot) (Snapshot, error) {
	allowed, err := s.allowedFamilies(ctx, actor, scope)
	if err != nil {
		return Snapshot{}, err
	}
	out := Snapshot{Position: snap.Position, Resources: make([]Resource, 0, len(snap.Resources))}
	for _, r := range snap.Resources {
		if allowed[r.Type] && scope.IncludesFamily(r.Type) {
			out.Resources = append(out.Resources, r)
		}
	}
	return out, nil
}

func (s *Service) acquireSnapshot(ctx context.Context, actor Actor, scope Scope) (Snapshot, error) {
	if _, err := s.allowedFamilies(ctx, actor, scope); err != nil {
		return Snapshot{}, err
	}
	snap, err := s.state.Snapshot(ctx, scope, s.opts.MaxSnapshotResources)
	if err != nil {
		return Snapshot{}, err
	}
	if snap.Position < 0 || len(snap.Resources) > s.opts.MaxSnapshotResources {
		return Snapshot{}, ErrSnapshotTooLarge
	}
	return snap, nil
}

// Snapshot returns the complete current state of the scope's projection and the
// position it is consistent with. Access is checked before the read and again
// before the result is released; a state larger than the configured bound fails
// with ErrSnapshotTooLarge and no position is issued.
func (s *Service) Snapshot(ctx context.Context, actor Actor, scope Scope) (Snapshot, error) {
	if err := scope.Validate(); err != nil {
		return Snapshot{}, err
	}
	snap, err := s.acquireSnapshot(ctx, actor, scope)
	if err != nil {
		return Snapshot{}, err
	}
	return s.release(ctx, actor, scope, snap)
}

// Request opens a stream. Resume is nil for a stream that starts with a snapshot.
type Request struct {
	Actor Actor
	Scope Scope
	// Resume is the continuation position decoded from a verified cursor.
	Resume *int64
}

// Stream is a prepared session. Preparation fails before any response is
// committed, so the transport can still answer with a proper status.
type Stream struct {
	svc     *Service
	actor   Actor
	session *Session
	initial *Snapshot
}

// Open validates the request, checks current access and either validates the
// resume position against the retained floor and committed high-water mark or
// acquires the atomic snapshot the stream will start with.
func (s *Service) Open(ctx context.Context, req Request) (*Stream, error) {
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if req.Actor.ID == "" {
		return nil, invalid("an actor is required")
	}
	if _, err := s.allowedFamilies(ctx, req.Actor, req.Scope); err != nil {
		return nil, err
	}
	st := &Stream{svc: s, actor: req.Actor}
	var start int64
	if req.Resume != nil {
		start = *req.Resume
		if start < 0 {
			return nil, ErrBadPosition
		}
		if err := s.state.Verify(ctx, req.Scope); err != nil {
			return nil, err
		}
		wm, err := s.log.Watermark(ctx, req.Scope)
		if err != nil {
			return nil, err
		}
		if start < wm.Floor {
			return nil, fmt.Errorf("%w: position %d is below the retention floor %d", ErrCursorExpired, start, wm.Floor)
		}
		if start > wm.HighWater {
			return nil, fmt.Errorf("%w: position %d is above the committed high-water mark", ErrBadPosition, start)
		}
	} else {
		snap, err := s.acquireSnapshot(ctx, req.Actor, req.Scope)
		if err != nil {
			return nil, err
		}
		st.initial, start = &snap, snap.Position
	}
	sess, err := NewSession(req.Scope, start)
	if err != nil {
		return nil, err
	}
	st.session = sess
	return st, nil
}

// Initial returns the snapshot a resume-less stream starts with, or nil.
func (st *Stream) Initial() *Snapshot { return st.initial }

// Session exposes the position bookkeeping (read-only use).
func (st *Stream) Session() *Session { return st.session }

// Run delivers the stream to sink until the context ends, access is lost, the
// client is too slow, retention overtakes the position or a dependency fails.
// It always returns a non-nil error: the reason the stream ended. The context's
// own error means the client went away or the server is shutting down.
//
// No database connection is held while the sink is written: every durable read
// is its own short transaction inside the Log adapter. When the stream ends for
// a reason the client can act on (expired position), a final resync-required
// control frame is attempted.
func (st *Stream) Run(ctx context.Context, sink Sink) (err error) {
	defer func() { st.session.Close(err) }()
	s := st.svc
	scope := st.session.Scope()

	if st.initial != nil {
		snap, err := s.release(ctx, st.actor, scope, *st.initial)
		if err != nil {
			return st.revoked(err)
		}
		if err := sink.Snapshot(ctx, snap); err != nil {
			return err
		}
		if err := sink.Flush(ctx); err != nil {
			return err
		}
	}
	if err := st.session.StartBacklog(); err != nil {
		return err
	}

	lastWrite, lastAccess := s.now(), s.now()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		recs, wm, err := s.log.Read(ctx, scope, st.session.Scanned(), s.opts.PageSize)
		if err != nil {
			if errors.Is(err, ErrCursorExpired) {
				_ = sink.Resync(ctx, err)
				_ = sink.Flush(ctx)
			}
			return err
		}
		if len(recs) > 0 {
			recs = cutPage(recs, s.opts.MaxPageBytes)
			wrote, err := st.process(ctx, sink, recs)
			if err != nil {
				return err
			}
			if err := sink.Flush(ctx); err != nil {
				return err
			}
			// Everything scanned has now reached the client or was excluded; the
			// position covers both.
			if st.session.Pending() {
				if err := st.session.Emit(st.session.Scanned()); err != nil {
					return err
				}
			}
			if wrote {
				lastAccess, lastWrite = s.now(), s.now()
			}
			if st.session.Scanned() < wm.HighWater {
				continue // more retained observations are unread
			}
		} else if st.session.Scanned() < wm.HighWater {
			// Committed sequences are gapless above the floor, so an empty page
			// below the high-water mark means the log is inconsistent. Fail closed
			// instead of spinning or skipping.
			return Unavailablef(nil, "the durable log has no observation after position %d below high-water %d", st.session.Scanned(), wm.HighWater)
		}
		if err := st.session.CaughtUp(wm.HighWater); err != nil {
			return err
		}

		// Idle: wait for new observations, keeping the connection alive and
		// rechecking access.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.opts.PollInterval):
		}
		now := s.now()
		if now.Sub(lastAccess) >= s.opts.AccessInterval {
			if _, err := s.allowedFamilies(ctx, st.actor, scope); err != nil {
				return st.revoked(err)
			}
			lastAccess = now
		}
		if now.Sub(lastWrite) >= s.opts.HeartbeatInterval {
			if err := sink.Heartbeat(ctx); err != nil {
				return err
			}
			if err := sink.Flush(ctx); err != nil {
				return err
			}
			lastWrite = now
		}
	}
}

// revoked classifies an access failure observed after the stream was
// authorized: refusal means access was lost; outages stay outages.
func (st *Stream) revoked(err error) error {
	if errors.Is(err, ErrHidden) || errors.Is(err, ErrDenied) {
		return fmt.Errorf("%w: %w", ErrAccessRevoked, err)
	}
	return err
}

// cutPage keeps the longest prefix within maxBytes, but at least one record.
func cutPage(recs []Record, maxBytes int64) []Record {
	var total int64
	for i, r := range recs {
		total += int64(r.Size)
		if total > maxBytes && i > 0 {
			return recs[:i]
		}
	}
	return recs
}

// process decides on and writes one page in sequence order. Each forwarded
// observation is authorized against current access immediately before it is
// handed to the sink; a refusal ends the stream when no family remains
// observable and otherwise only excludes that observation.
func (st *Stream) process(ctx context.Context, sink Sink, recs []Record) (bool, error) {
	s := st.svc
	scope := st.session.Scope()
	wrote := false
	var lastSent int64
	for _, rec := range recs {
		if err := ctx.Err(); err != nil {
			return wrote, err
		}
		if err := st.session.Scan(rec.Sequence); err != nil {
			return wrote, err
		}
		fam, ok := scope.Match(rec.Envelope)
		if !ok {
			continue
		}
		switch err := s.access.Authorize(ctx, st.actor, scope, fam); {
		case err == nil:
		case ctx.Err() != nil:
			return wrote, ctx.Err()
		case errors.Is(err, ErrHidden) || errors.Is(err, ErrDenied):
			if _, serr := s.allowedFamilies(ctx, st.actor, scope); serr != nil {
				return wrote, st.revoked(serr)
			}
			continue // another family is still observable: exclude only this one
		case errors.Is(err, ErrUnavailable):
			return wrote, err
		default:
			return wrote, Unavailablef(err, "checking current access")
		}
		if err := sink.Event(ctx, Delivery{Position: rec.Sequence, Family: fam, Envelope: rec.Envelope}); err != nil {
			return wrote, err
		}
		wrote, lastSent = true, rec.Sequence
	}
	// Quiet pages still advance the continuation: a checkpoint after the last
	// excluded observation lets a filtered stream resume near the head.
	if scanned := st.session.Scanned(); scanned > lastSent {
		if err := sink.Checkpoint(ctx, scanned); err != nil {
			return wrote, err
		}
		wrote = true
	}
	return wrote, nil
}
