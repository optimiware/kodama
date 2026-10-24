package observation

import "fmt"

// Phase is the lifecycle of one stream session.
type Phase string

const (
	// PhasePreparing: the scope is validated and the starting position is chosen
	// (a resume position or a snapshot's watermark); nothing is scanned yet.
	PhasePreparing Phase = "preparing"
	// PhaseBacklog: draining retained observations after the starting position.
	PhaseBacklog Phase = "backlog"
	// PhaseLive: caught up with the committed high-water mark and polling for
	// new observations.
	PhaseLive Phase = "live"
	// PhaseClosed is final: no further scan or emission is accepted.
	PhaseClosed Phase = "closed"
)

// Session is the position bookkeeping of one stream. It distinguishes three
// positions that only ever move forward:
//
//   - the start: the position the stream continues from;
//   - scanned: the highest retained observation read and decided on, whether it
//     was forwarded or excluded;
//   - emitted: the highest position known to have been handed to the client,
//     which is the only position a reconnecting client can safely resume from.
//
// emitted never exceeds scanned, and a closed session accepts nothing.
type Session struct {
	scope   Scope
	phase   Phase
	start   int64
	scanned int64
	emitted int64
	cause   error
}

// NewSession starts a preparing session at a nonnegative position.
func NewSession(scope Scope, start int64) (*Session, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if start < 0 {
		return nil, invalid("position must not be negative")
	}
	return &Session{scope: scope, phase: PhasePreparing, start: start, scanned: start, emitted: start}, nil
}

// Scope returns the observed scope.
func (s *Session) Scope() Scope { return s.scope }

// Phase returns the current phase.
func (s *Session) Phase() Phase { return s.phase }

// Start returns the position the session continues from.
func (s *Session) Start() int64 { return s.start }

// Scanned returns the highest position read and decided on.
func (s *Session) Scanned() int64 { return s.scanned }

// Emitted returns the highest position handed to the client.
func (s *Session) Emitted() int64 { return s.emitted }

// Pending reports whether scanned observations were decided on without any
// emission covering them (for example a page of excluded events).
func (s *Session) Pending() bool { return s.scanned > s.emitted }

// Cause returns why the session closed, or nil while it is open or closed
// without error.
func (s *Session) Cause() error { return s.cause }

func (s *Session) transition(from []Phase, to Phase) error {
	for _, f := range from {
		if s.phase == f {
			s.phase = to
			return nil
		}
	}
	if s.phase == PhaseClosed {
		return ErrClosed
	}
	return fmt.Errorf("%w: cannot move from %s to %s", ErrInvalid, s.phase, to)
}

// StartBacklog moves preparing to backlog.
func (s *Session) StartBacklog() error {
	return s.transition([]Phase{PhasePreparing}, PhaseBacklog)
}

// CaughtUp moves backlog to live once everything up to the committed high-water
// mark has been scanned. It refuses while observations remain unread.
func (s *Session) CaughtUp(highWater int64) error {
	if s.phase == PhaseClosed {
		return ErrClosed
	}
	if s.phase == PhaseLive {
		return nil
	}
	if s.scanned < highWater {
		return invalid("observations up to %d remain unread (scanned %d)", highWater, s.scanned)
	}
	return s.transition([]Phase{PhaseBacklog}, PhaseLive)
}

// Scan records that the observation at seq was read and decided on. Positions
// are strictly increasing: a repeat or a step back is a programming error that
// would duplicate or skip observations.
func (s *Session) Scan(seq int64) error {
	if s.phase == PhaseClosed {
		return ErrClosed
	}
	if s.phase == PhasePreparing {
		return invalid("scanning before the backlog started")
	}
	if seq <= s.scanned {
		return invalid("position %d does not advance scanned position %d", seq, s.scanned)
	}
	s.scanned = seq
	return nil
}

// Emit records that everything up to seq reached the client. seq may not pass
// what was scanned and may not move backwards.
func (s *Session) Emit(seq int64) error {
	if s.phase == PhaseClosed {
		return ErrClosed
	}
	if s.phase == PhasePreparing {
		return invalid("emitting before the backlog started")
	}
	if seq > s.scanned {
		return invalid("cannot emit position %d beyond scanned position %d", seq, s.scanned)
	}
	if seq <= s.emitted {
		return invalid("position %d does not advance emitted position %d", seq, s.emitted)
	}
	s.emitted = seq
	return nil
}

// Close ends the session. It is idempotent and final; the first cause is kept.
func (s *Session) Close(cause error) {
	if s.phase == PhaseClosed {
		return
	}
	s.phase, s.cause = PhaseClosed, cause
}
