package postgres_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/mcpproxy"
	"github.com/optimiware/kodama/internal/mcpproxy/mcptest"
)

func admitReq(t *testing.T, s *mcptest.Stack, sess mcpproxy.RemoteSession, id string, max *int64) mcpproxy.AdmitRequest {
	t.Helper()
	now := s.RT.Clock.Now()
	inv, err := mcpproxy.NewInvocation(mcpproxy.RequestID(id), sess, "echo", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := inv.Dispatch(now); err != nil {
		t.Fatal(err)
	}
	return mcpproxy.AdmitRequest{Invocation: inv, Incarnation: s.Opts.Incarnation, MaxCalls: max, Now: now}
}

func count(t *testing.T, s *mcptest.Stack, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := s.RT.Owner.QueryRow(bg, sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Test_concurrent_admission: concurrent services cannot duplicate a call receipt
// or exceed a claimed ceiling; registration changes serialize with admission.
func Test_concurrent_admission(t *testing.T) {
	s := mcptest.New(t, mcptest.Config{Conns: 40})
	r := s.Setup([]string{"echo"}, []string{"echo"}, nil)
	id, err := s.Service.OpenSession(bg, r.W.Caller, r.Reg.ID)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := s.Store.GetSession(bg, r.W.Scope, id)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("one request identifier is admitted exactly once", func(t *testing.T) {
		const n = 12
		var wg sync.WaitGroup
		results := make([]mcpproxy.Admission, n)
		errs := make([]error, n)
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				results[i], errs[i] = s.Store.Admit(bg, admitReq(t, s, sess, "req_t033_same", nil))
			}(i)
		}
		close(start)
		wg.Wait()
		fresh := 0
		for i := range results {
			if errs[i] != nil {
				t.Fatalf("admit %d: %v", i, errs[i])
			}
			if !results[i].Duplicate {
				fresh++
			}
		}
		if fresh != 1 {
			t.Fatalf("%d admissions dispatched, want exactly 1", fresh)
		}
		if n := count(t, s, `SELECT count(*) FROM mcp_invocations WHERE id = 'req_t033_same'`); n != 1 {
			t.Fatalf("%d receipts", n)
		}
		if n := count(t, s, `SELECT calls FROM mcp_run_calls WHERE run_id = $1`, string(r.W.Run)); n != 1 {
			t.Fatalf("duplicates were charged: counter %d", n)
		}
	})

	t.Run("the claimed ceiling holds across concurrent admissions", func(t *testing.T) {
		const n, ceiling = 14, 4 // one call was already charged above
		max := int64(ceiling)
		var wg sync.WaitGroup
		var mu sync.Mutex
		admitted, limited := 0, 0
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, err := s.Store.Admit(bg, admitReq(t, s, sess, fmt.Sprintf("req_t033_cap_%02d", i), &max))
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					admitted++
				case errors.Is(err, mcpproxy.ErrLimitReached):
					limited++
				default:
					t.Errorf("admit %d: %v", i, err)
				}
			}(i)
		}
		close(start)
		wg.Wait()
		if admitted != ceiling-1 || limited != n-(ceiling-1) {
			t.Fatalf("admitted %d limited %d, want %d and %d", admitted, limited, ceiling-1, n-(ceiling-1))
		}
		if c := count(t, s, `SELECT calls FROM mcp_run_calls WHERE run_id = $1`, string(r.W.Run)); c != ceiling {
			t.Fatalf("counter %d, want %d", c, ceiling)
		}
		if c := count(t, s, `SELECT count(*) FROM mcp_invocations WHERE id LIKE 'req_t033_cap_%'`); c != ceiling-1 {
			t.Fatalf("%d receipts", c)
		}
	})

	t.Run("a registration change serializes with admission", func(t *testing.T) {
		const n = 16
		var wg sync.WaitGroup
		start := make(chan struct{})
		editDone := make(chan time.Time, 1)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, _ = s.Store.Admit(bg, admitReq(t, s, sess, fmt.Sprintf("req_t033_race_%02d", i), nil))
			}(i)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			cur, _ := s.Store.GetRegistration(bg, r.W.Scope, r.Reg.ID)
			next, _ := cur.Edit(cur.Version, mcpproxy.Draft{Endpoint: cur.Endpoint, Secret: cur.Secret, Tools: []string{"echo", "add"}}, mcpproxy.Actor{ID: mcptest.Admin}, s.RT.Clock.Now())
			if err := s.Store.ReplaceRegistration(bg, mcpproxy.Mutation{Actor: mcpproxy.Actor{ID: mcptest.Admin}, Now: s.RT.Clock.Now()}, next, cur.Version); err != nil {
				t.Errorf("edit: %v", err)
			}
			editDone <- time.Now()
		}()
		close(start)
		wg.Wait()
		<-editDone
		// Whatever interleaving happened, nothing is admitted against the old
		// revision once the edit committed.
		_, err := s.Store.Admit(bg, admitReq(t, s, sess, "req_t033_after_edit", nil))
		if !errors.Is(err, mcpproxy.ErrSessionInvalid) && !errors.Is(err, mcpproxy.ErrBindingInvalid) {
			t.Fatalf("admission after the edit: %v", err)
		}
		if st := sessionStateOf(t, s, string(id)); st != "invalid" {
			t.Fatalf("session state %s, want invalid", st)
		}
	})
}

func sessionStateOf(t *testing.T, s *mcptest.Stack, id string) string {
	t.Helper()
	var st string
	if err := s.RT.Owner.QueryRow(bg, `SELECT state FROM mcp_sessions WHERE id = $1`, id).Scan(&st); err != nil {
		t.Fatal(err)
	}
	return st
}
