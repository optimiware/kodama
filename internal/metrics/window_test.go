package metrics_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/metrics"
)

var now = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

// Test_summary_validation: scopes, identifiers, windows and batch sizes are
// rejected consistently before anything is authorized or read.
func Test_summary_validation(t *testing.T) {
	from := now.Add(-time.Hour)

	t.Run("windows", func(t *testing.T) {
		est := time.FixedZone("est", -5*3600)
		ok, err := metrics.NewWindow(from.In(est), now.In(est), now)
		if err != nil || ok.From.Location() != time.UTC || !ok.From.Equal(from) || ok.Duration() != time.Hour {
			t.Fatalf("valid window: %+v %v", ok, err)
		}
		micro := now.Add(-time.Hour).Add(7 * time.Microsecond)
		if _, err := metrics.NewWindow(micro, now, now); err != nil {
			t.Errorf("microsecond precision rejected: %v", err)
		}
		for name, c := range map[string][2]time.Time{
			"missing start":        {{}, now},
			"missing end":          {from, {}},
			"reversed":             {now, from},
			"empty":                {from, from},
			"future end":           {from, now.Add(time.Microsecond)},
			"nanosecond start":     {from.Add(1), now},
			"nanosecond end":       {from, now.Add(-1)},
			"before the epoch era": {time.Date(1999, 12, 31, 0, 0, 0, 0, time.UTC), now},
		} {
			if _, err := metrics.NewWindow(c[0], c[1], now); !errors.Is(err, metrics.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		// The end may equal the observation instant.
		if _, err := metrics.NewWindow(from, now, now); err != nil {
			t.Errorf("end at now: %v", err)
		}
		if (metrics.Window{}).Valid() || (metrics.Window{From: now, To: from}).Valid() {
			t.Error("a zero or reversed window reports valid")
		}
	})

	t.Run("requests", func(t *testing.T) {
		svc := newService(t, &spyReader{}, &spyAuth{})
		scope := domain.Scope{TenantID: "t_a", ProjectID: "p_a"}
		w := window(t)
		long := make([]domain.WorkItemID, metrics.MaxBatch+1)
		for i := range long {
			long[i] = domain.WorkItemID("wi_" + strings.Repeat("a", 3) + string(rune('a'+i%26)) + strings.Repeat("0", i/26))
		}
		for name, c := range map[string]struct {
			scope domain.Scope
			ids   []domain.WorkItemID
			w     metrics.Window
		}{
			"missing tenant":       {domain.Scope{ProjectID: "p"}, []domain.WorkItemID{"wi_1"}, w},
			"missing project":      {domain.Scope{TenantID: "t"}, []domain.WorkItemID{"wi_1"}, w},
			"malformed tenant":     {domain.Scope{TenantID: "t/x", ProjectID: "p"}, []domain.WorkItemID{"wi_1"}, w},
			"empty identifier":     {scope, []domain.WorkItemID{""}, w},
			"malformed identifier": {scope, []domain.WorkItemID{"wi_1", "a b"}, w},
			"too many items":       {scope, long, w},
			"zero window":          {scope, []domain.WorkItemID{"wi_1"}, metrics.Window{}},
			"future window":        {scope, []domain.WorkItemID{"wi_1"}, metrics.Window{From: now, To: now.Add(time.Hour)}},
		} {
			if _, err := svc.Summarize(t.Context(), c.scope, "u_1", c.ids, c.w); !errors.Is(err, metrics.ErrInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if _, err := svc.Summarize(t.Context(), scope, "", []domain.WorkItemID{"wi_1"}, w); !errors.Is(err, metrics.ErrDenied) {
			t.Errorf("anonymous caller: %v", err)
		}
	})

	t.Run("an_empty_set_is_not_all_work", func(t *testing.T) {
		reader := &spyReader{}
		svc := newService(t, reader, &spyAuth{})
		got, err := svc.Summarize(t.Context(), domain.Scope{TenantID: "t_a", ProjectID: "p_a"}, "u_1", nil, window(t))
		if err != nil || got == nil || len(got) != 0 || reader.calls != 0 {
			t.Fatalf("got %v err %v reads %d", got, err, reader.calls)
		}
	})

	t.Run("duplicates_are_read_once_in_request_order", func(t *testing.T) {
		reader := &spyReader{}
		svc := newService(t, reader, &spyAuth{})
		scope := domain.Scope{TenantID: "t_a", ProjectID: "p_a"}
		got, err := svc.Summarize(t.Context(), scope, "u_1", []domain.WorkItemID{"wi_b", "wi_a", "wi_b", "wi_missing"}, window(t))
		if err != nil || len(got) != 2 || got[0].ItemID != "wi_b" || got[1].ItemID != "wi_a" {
			t.Fatalf("got %+v err %v", got, err)
		}
		if len(reader.asked) != 3 || reader.asked[0] != "wi_b" || reader.asked[1] != "wi_a" || reader.asked[2] != "wi_missing" {
			t.Fatalf("reader was asked %v", reader.asked)
		}
	})
}
