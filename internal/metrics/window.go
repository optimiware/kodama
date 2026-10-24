package metrics

import (
	"time"
)

// Window is an observation window [From, To) in UTC with microsecond precision,
// the precision of recorded history. Both bounds are required: a summary of
// "everything so far" would silently change meaning as time passes.
type Window struct {
	From time.Time
	To   time.Time
}

// minTime bounds how far back a window may start; it keeps arithmetic and the
// database's timestamp range far from their limits.
var minTime = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// NewWindow validates and normalizes the bounds. now is the observation instant:
// a window that ends in the future is rejected because open intervals would be
// capped at an instant that has not happened. Bounds with sub-microsecond
// precision are rejected rather than silently rounded.
func NewWindow(from, to, now time.Time) (Window, error) {
	if from.IsZero() || to.IsZero() {
		return Window{}, Invalidf("an observation window needs both a start and an end")
	}
	from, to = from.UTC(), to.UTC()
	for name, t := range map[string]time.Time{"start": from, "end": to} {
		if t.Nanosecond()%1000 != 0 {
			return Window{}, Invalidf("the window %s must have at most microsecond precision", name)
		}
		if t.Before(minTime) {
			return Window{}, Invalidf("the window %s is before %s", name, minTime.Format(time.RFC3339))
		}
	}
	if !from.Before(to) {
		return Window{}, Invalidf("the window start must be before its end")
	}
	if to.After(now.UTC()) {
		return Window{}, Invalidf("the window must not end in the future")
	}
	return Window{From: from, To: to}, nil
}

// Valid reports whether w is a window NewWindow could have produced, apart from
// the future check.
func (w Window) Valid() bool {
	return !w.From.IsZero() && !w.To.IsZero() && w.From.Before(w.To) &&
		w.From.Location() == time.UTC && w.To.Location() == time.UTC &&
		w.From.Nanosecond()%1000 == 0 && w.To.Nanosecond()%1000 == 0 && !w.From.Before(minTime)
}

// Duration is the length of the window.
func (w Window) Duration() time.Duration { return w.To.Sub(w.From) }
