package metrics

import (
	"context"
	"errors"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// MaxBatch bounds how many items one call summarizes: the size of an existing
// work-list page. An empty set is never interpreted as "all work".
const MaxBatch = 200

// Config wires a Service. Reader and Authorizer are required.
type Config struct {
	Reader     SummaryReader
	Authorizer ReadAuthorizer
	Clock      func() time.Time
}

// Service enriches work-list pages with flow summaries. It authorizes every call
// before any persistence access and performs no writes.
type Service struct {
	reader SummaryReader
	auth   ReadAuthorizer
	now    func() time.Time
}

// NewService requires its collaborators: without an authorizer or reader there
// is nothing safe to do.
func NewService(cfg Config) (*Service, error) {
	if cfg.Reader == nil || cfg.Authorizer == nil {
		return nil, errors.New("metrics: a reader and an authorizer are required")
	}
	s := &Service{reader: cfg.Reader, auth: cfg.Authorizer, now: cfg.Clock}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	return s, nil
}

// Window validates an observation window against the service clock.
func (s *Service) Window(from, to time.Time) (Window, error) {
	return NewWindow(from, to, s.now())
}

// Summarize returns the summaries of the requested items over the window, in
// request order with duplicates removed. Unknown, foreign and wrong-project
// items are omitted identically. Authorization is evaluated on every call; a
// failure of the authority or the reader returns no summaries.
func (s *Service) Summarize(ctx context.Context, scope domain.Scope, principalID string, ids []domain.WorkItemID, w Window) ([]WorkSummary, error) {
	if s == nil || s.reader == nil || s.auth == nil {
		return nil, Unavailablef(nil, "the flow summary service is not configured")
	}
	if err := scope.Validate(); err != nil {
		return nil, Invalidf("%v", err)
	}
	if principalID == "" {
		return nil, ErrDenied
	}
	if !w.Valid() {
		return nil, Invalidf("an observation window is required")
	}
	if w.To.After(s.now().UTC()) {
		return nil, Invalidf("the window must not end in the future")
	}
	unique, err := normalizeIDs(ids)
	if err != nil {
		return nil, err
	}
	// Authorization precedes persistence; its failures never reach the reader.
	if err := s.auth.AuthorizeRead(ctx, scope, principalID); err != nil {
		switch {
		case errors.Is(err, ErrDenied), errors.Is(err, ErrHidden):
			return nil, err
		default:
			return nil, Unavailablef(err, "the read authority failed")
		}
	}
	if len(unique) == 0 {
		return []WorkSummary{}, nil
	}
	got, err := s.reader.ReadSummaries(ctx, scope, unique, w)
	if err != nil {
		if errors.Is(err, ErrUnavailable) || errors.Is(err, ErrInvalid) {
			return nil, err
		}
		return nil, Unavailablef(err, "flow summaries could not be read")
	}
	return s.order(scope, unique, w, got)
}

// order checks the adapter's answer and puts it in request order. Anything the
// adapter returns that was not asked for, or that contradicts the request, is a
// failure rather than a leak.
func (s *Service) order(scope domain.Scope, ids []domain.WorkItemID, w Window, got []WorkSummary) ([]WorkSummary, error) {
	asked := make(map[domain.WorkItemID]struct{}, len(ids))
	for _, id := range ids {
		asked[id] = struct{}{}
	}
	byID := make(map[domain.WorkItemID]WorkSummary, len(got))
	for _, g := range got {
		if _, ok := asked[g.ItemID]; !ok || g.Scope != scope || !g.Window.From.Equal(w.From) || !g.Window.To.Equal(w.To) {
			return nil, Unavailablef(nil, "the summary reader returned an unexpected summary")
		}
		if _, dup := byID[g.ItemID]; dup {
			return nil, Unavailablef(nil, "the summary reader returned a duplicate summary")
		}
		if err := g.Validate(); err != nil {
			return nil, Unavailablef(err, "the summary reader returned an incoherent summary")
		}
		byID[g.ItemID] = g
	}
	out := make([]WorkSummary, 0, len(byID))
	for _, id := range ids {
		if g, ok := byID[id]; ok {
			out = append(out, g)
		}
	}
	return out, nil
}

func normalizeIDs(ids []domain.WorkItemID) ([]domain.WorkItemID, error) {
	if len(ids) > MaxBatch {
		return nil, Invalidf("at most %d work items can be summarized at once", MaxBatch)
	}
	seen := make(map[domain.WorkItemID]struct{}, len(ids))
	out := make([]domain.WorkItemID, 0, len(ids))
	for _, id := range ids {
		if err := id.Validate(); err != nil {
			return nil, Invalidf("work item id: %v", err)
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}
