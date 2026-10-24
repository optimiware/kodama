package observation

import (
	"errors"
	"testing"
)

func mustScope(t *testing.T, run string, types ...string) Scope {
	t.Helper()
	s, err := NewScope("t_acme", "p_inventory", domainRun(run), types)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func Test_session_transitions(t *testing.T) {
	scope := mustScope(t, "")

	t.Run("scope validation", func(t *testing.T) {
		bad := []struct {
			name string
			do   func() error
		}{
			{"empty tenant", func() error { _, err := NewScope("", "p_1", "", nil); return err }},
			{"bad project", func() error { _, err := NewScope("t_1", "p 1", "", nil); return err }},
			{"bad run", func() error { _, err := NewScope("t_1", "p_1", "bad run", nil); return err }},
			{"unsupported type", func() error { _, err := NewScope("t_1", "p_1", "", []string{"gate.requested"}); return err }},
			{"empty type", func() error { _, err := NewScope("t_1", "p_1", "", []string{""}); return err }},
			{"work type on a run stream selects nothing", func() error {
				_, err := NewScope("t_1", "p_1", "run_1", []string{"work_item.created"})
				return err
			}},
			{"negative position", func() error { _, err := NewSession(scope, -1); return err }},
			{"zero scope", func() error { _, err := NewSession(Scope{}, 0); return err }},
		}
		for _, c := range bad {
			if err := c.do(); !errors.Is(err, ErrInvalid) {
				t.Errorf("%s: err = %v, want ErrInvalid", c.name, err)
			}
		}
	})

	t.Run("filters are normalized", func(t *testing.T) {
		s := mustScope(t, "", "run.created", "work_item.created", "run.created")
		if got := s.Types; len(got) != 2 || got[0] != "run.created" || got[1] != "work_item.created" {
			t.Fatalf("types = %v", got)
		}
		if f := s.Filters()["type"]; len(f) != 2 {
			t.Fatalf("filters = %v", f)
		}
	})

	t.Run("positions never decrease", func(t *testing.T) {
		s, err := NewSession(scope, 10)
		if err != nil {
			t.Fatal(err)
		}
		if s.Phase() != PhasePreparing || s.Scanned() != 10 || s.Emitted() != 10 {
			t.Fatalf("initial state: %s %d %d", s.Phase(), s.Scanned(), s.Emitted())
		}
		if err := s.Scan(11); !errors.Is(err, ErrInvalid) {
			t.Fatalf("scan before the backlog started: %v", err)
		}
		if err := s.StartBacklog(); err != nil {
			t.Fatal(err)
		}
		if err := s.StartBacklog(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("second start: %v", err)
		}
		for _, bad := range []int64{10, 9, 0} {
			if err := s.Scan(bad); !errors.Is(err, ErrInvalid) {
				t.Errorf("scan(%d) = %v", bad, err)
			}
		}
		if err := s.Scan(12); err != nil {
			t.Fatal(err)
		}
		if err := s.Scan(12); !errors.Is(err, ErrInvalid) {
			t.Fatalf("repeat scan: %v", err)
		}
	})

	t.Run("scanned is distinct from emitted", func(t *testing.T) {
		s, _ := NewSession(scope, 0)
		_ = s.StartBacklog()
		for _, seq := range []int64{1, 2, 3} {
			if err := s.Scan(seq); err != nil {
				t.Fatal(err)
			}
		}
		if !s.Pending() || s.Scanned() != 3 || s.Emitted() != 0 {
			t.Fatalf("pending=%v scanned=%d emitted=%d", s.Pending(), s.Scanned(), s.Emitted())
		}
		if err := s.Emit(4); !errors.Is(err, ErrInvalid) {
			t.Fatalf("emitting beyond scanned: %v", err)
		}
		if err := s.Emit(2); err != nil {
			t.Fatal(err)
		}
		if err := s.Emit(2); !errors.Is(err, ErrInvalid) {
			t.Fatalf("emit must advance: %v", err)
		}
		if err := s.Emit(1); !errors.Is(err, ErrInvalid) {
			t.Fatalf("emit may not step back: %v", err)
		}
		if err := s.Emit(3); err != nil {
			t.Fatal(err)
		}
		if s.Pending() {
			t.Fatal("nothing should be pending after emitting the scanned position")
		}
	})

	t.Run("live needs the backlog drained", func(t *testing.T) {
		s, _ := NewSession(scope, 0)
		if err := s.CaughtUp(0); !errors.Is(err, ErrInvalid) {
			t.Fatalf("caught up while preparing: %v", err)
		}
		_ = s.StartBacklog()
		_ = s.Scan(1)
		if err := s.CaughtUp(3); !errors.Is(err, ErrInvalid) {
			t.Fatalf("caught up with unread observations: %v", err)
		}
		_ = s.Scan(3)
		if err := s.CaughtUp(3); err != nil || s.Phase() != PhaseLive {
			t.Fatalf("caught up: %v %s", err, s.Phase())
		}
		if err := s.CaughtUp(3); err != nil {
			t.Fatalf("idempotent in live: %v", err)
		}
		if err := s.StartBacklog(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("live cannot return to preparing: %v", err)
		}
	})

	t.Run("closure is final", func(t *testing.T) {
		s, _ := NewSession(scope, 0)
		_ = s.StartBacklog()
		_ = s.Scan(1)
		cause := errors.New("client went away")
		s.Close(cause)
		s.Close(errors.New("second cause is ignored"))
		if s.Phase() != PhaseClosed || s.Cause() != cause {
			t.Fatalf("phase=%s cause=%v", s.Phase(), s.Cause())
		}
		if err := s.Scan(2); !errors.Is(err, ErrClosed) {
			t.Fatalf("scan after close: %v", err)
		}
		if err := s.Emit(1); !errors.Is(err, ErrClosed) {
			t.Fatalf("emit after close: %v", err)
		}
		if err := s.CaughtUp(1); !errors.Is(err, ErrClosed) {
			t.Fatalf("caught up after close: %v", err)
		}
		if err := s.StartBacklog(); !errors.Is(err, ErrClosed) {
			t.Fatalf("start after close: %v", err)
		}
		if s.Scanned() != 1 || s.Emitted() != 0 {
			t.Fatal("a closed session's positions must not change")
		}
	})
}
