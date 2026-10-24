package metrics_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/metrics"
)

func window(t testing.TB) metrics.Window {
	t.Helper()
	w, err := metrics.NewWindow(now.Add(-time.Hour), now, now)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// spyAuth records calls and answers with err.
type spyAuth struct {
	calls int
	err   error
}

func (a *spyAuth) AuthorizeRead(context.Context, domain.Scope, string) error {
	a.calls++
	return a.err
}

// spyReader returns an ok summary for every requested item whose identifier is
// not "wi_missing", and counts its calls.
type spyReader struct {
	calls int
	asked []domain.WorkItemID
	err   error
	// mutate lets a test corrupt the answer.
	mutate func([]metrics.WorkSummary) []metrics.WorkSummary
}

func (r *spyReader) ReadSummaries(_ context.Context, scope domain.Scope, ids []domain.WorkItemID, w metrics.Window) ([]metrics.WorkSummary, error) {
	r.calls++
	r.asked = append([]domain.WorkItemID(nil), ids...)
	if r.err != nil {
		return nil, r.err
	}
	var out []metrics.WorkSummary
	for _, id := range ids {
		if id == "wi_missing" {
			continue
		}
		out = append(out, metrics.WorkSummary{
			Scope: scope, ItemID: id, Window: w, Status: metrics.StatusOK, ObservedVersion: 1,
			Lead: time.Minute, OpenAtEnd: true, StateAtEnd: "backlog",
		})
	}
	if r.mutate != nil {
		out = r.mutate(out)
	}
	return out, nil
}

func newService(t testing.TB, r metrics.SummaryReader, a metrics.ReadAuthorizer) *metrics.Service {
	t.Helper()
	s, err := metrics.NewService(metrics.Config{Reader: r, Authorizer: a, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Test_summary_authorization: a denied, hidden or unavailable authority never
// reaches persistence, every request is authorized again, and missing
// collaborators fail closed. IT-03.
func Test_summary_authorization(t *testing.T) {
	scope := domain.Scope{TenantID: "t_a", ProjectID: "p_a"}
	ids := []domain.WorkItemID{"wi_1"}

	for name, c := range map[string]struct {
		authErr error
		want    error
	}{
		"denied":       {metrics.ErrDenied, metrics.ErrDenied},
		"hidden":       {metrics.ErrHidden, metrics.ErrHidden},
		"outage":       {errors.New("authority down"), metrics.ErrUnavailable},
		"cancelled":    {context.Canceled, metrics.ErrUnavailable},
		"unclassified": {errors.New("denied?"), metrics.ErrUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			reader := &spyReader{}
			svc := newService(t, reader, &spyAuth{err: c.authErr})
			got, err := svc.Summarize(t.Context(), scope, "u_1", ids, window(t))
			if !errors.Is(err, c.want) || got != nil {
				t.Fatalf("got %v err %v, want %v", got, err, c.want)
			}
			if reader.calls != 0 {
				t.Fatal("persistence was reached without authorization")
			}
		})
	}

	t.Run("every_request_reauthorizes", func(t *testing.T) {
		reader, auth := &spyReader{}, &spyAuth{}
		svc := newService(t, reader, auth)
		for i := 0; i < 3; i++ {
			if _, err := svc.Summarize(t.Context(), scope, "u_1", ids, window(t)); err != nil {
				t.Fatal(err)
			}
		}
		if auth.calls != 3 || reader.calls != 3 {
			t.Fatalf("authorizations %d reads %d", auth.calls, reader.calls)
		}
		// Access revoked between two identical requests.
		auth.err = metrics.ErrDenied
		if _, err := svc.Summarize(t.Context(), scope, "u_1", ids, window(t)); !errors.Is(err, metrics.ErrDenied) || reader.calls != 3 {
			t.Fatalf("revoked access still read: %v (%d reads)", err, reader.calls)
		}
	})

	t.Run("empty_batches_are_still_authorized", func(t *testing.T) {
		auth := &spyAuth{err: metrics.ErrDenied}
		svc := newService(t, &spyReader{}, auth)
		if _, err := svc.Summarize(t.Context(), scope, "u_1", nil, window(t)); !errors.Is(err, metrics.ErrDenied) || auth.calls != 1 {
			t.Fatalf("empty batch: %v (%d authorizations)", err, auth.calls)
		}
	})

	t.Run("missing_collaborators_fail_closed", func(t *testing.T) {
		if _, err := metrics.NewService(metrics.Config{Authorizer: &spyAuth{}}); err == nil {
			t.Error("a service without a reader was built")
		}
		if _, err := metrics.NewService(metrics.Config{Reader: &spyReader{}}); err == nil {
			t.Error("a service without an authorizer was built")
		}
		var zero *metrics.Service
		if _, err := zero.Summarize(t.Context(), scope, "u_1", ids, window(t)); !errors.Is(err, metrics.ErrUnavailable) {
			t.Errorf("nil service: %v", err)
		}
		if _, err := (&metrics.Service{}).Summarize(t.Context(), scope, "u_1", ids, window(t)); !errors.Is(err, metrics.ErrUnavailable) {
			t.Errorf("zero service: %v", err)
		}
	})

	t.Run("reader_failures_return_nothing", func(t *testing.T) {
		boom := errors.New("connection reset")
		svc := newService(t, &spyReader{err: boom}, &spyAuth{})
		got, err := svc.Summarize(t.Context(), scope, "u_1", ids, window(t))
		if got != nil || !errors.Is(err, metrics.ErrUnavailable) || !errors.Is(err, boom) {
			t.Fatalf("got %v err %v", got, err)
		}
	})

	t.Run("an_adapter_cannot_widen_the_answer", func(t *testing.T) {
		other := domain.Scope{TenantID: "t_b", ProjectID: "p_a"}
		for name, mutate := range map[string]func([]metrics.WorkSummary) []metrics.WorkSummary{
			"an item that was not requested": func(s []metrics.WorkSummary) []metrics.WorkSummary {
				x := s[0]
				x.ItemID = "wi_other"
				return append(s, x)
			},
			"another scope": func(s []metrics.WorkSummary) []metrics.WorkSummary { s[0].Scope = other; return s },
			"a duplicate":   func(s []metrics.WorkSummary) []metrics.WorkSummary { return append(s, s[0]) },
			"another window": func(s []metrics.WorkSummary) []metrics.WorkSummary {
				s[0].Window.From = s[0].Window.From.Add(time.Second)
				return s
			},
			"negative durations": func(s []metrics.WorkSummary) []metrics.WorkSummary { s[0].Active = -time.Second; return s },
			"figures that exceed lead": func(s []metrics.WorkSummary) []metrics.WorkSummary {
				s[0].Blocked = 2 * time.Minute
				return s
			},
		} {
			svc := newService(t, &spyReader{mutate: mutate}, &spyAuth{})
			if got, err := svc.Summarize(t.Context(), scope, "u_1", ids, window(t)); got != nil || !errors.Is(err, metrics.ErrUnavailable) {
				t.Errorf("%s: got %v err %v", name, got, err)
			}
		}
	})
}

// Test_summary_values: the read model rejects incoherent combinations.
func Test_summary_values(t *testing.T) {
	base := metrics.WorkSummary{
		Scope: domain.Scope{TenantID: "t_a", ProjectID: "p_a"}, ItemID: "wi_1", Window: window(t),
		Status: metrics.StatusOK, ObservedVersion: 2, Lead: time.Hour, Active: time.Minute, Review: time.Minute,
		Blocked: time.Minute, OpenAtEnd: true, StateAtEnd: "active",
	}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*metrics.WorkSummary){
		"unknown status":           func(s *metrics.WorkSummary) { s.Status = "maybe" },
		"ok without a state":       func(s *metrics.WorkSummary) { s.StateAtEnd = "" },
		"open at a finished state": func(s *metrics.WorkSummary) { s.StateAtEnd = "done" },
		"closed at an open state":  func(s *metrics.WorkSummary) { s.OpenAtEnd = false },
		"duration beyond window":   func(s *metrics.WorkSummary) { s.Lead = 3 * time.Hour },
		"parts beyond lead":        func(s *metrics.WorkSummary) { s.Active, s.Review = time.Hour, time.Hour },
		"negative count":           func(s *metrics.WorkSummary) { s.ReopenCount = -1 },
		"invalid with figures":     func(s *metrics.WorkSummary) { s.Status, s.Anomaly = metrics.StatusInvalidHistory, "version_gap" },
		"not started with figures": func(s *metrics.WorkSummary) {
			s.Status, s.ObservedVersion, s.StateAtEnd = metrics.StatusNotStarted, 0, ""
		},
		"scope missing": func(s *metrics.WorkSummary) { s.Scope.ProjectID = "" },
		"unknown state": func(s *metrics.WorkSummary) { s.StateAtEnd = "archived" },
	} {
		s := base
		mutate(&s)
		if err := s.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	for _, st := range []metrics.State{"done", "cancelled"} {
		if !st.Finished() {
			t.Errorf("%s is finished", st)
		}
	}
	if metrics.State("review").Finished() {
		t.Error("review is not finished")
	}
}
