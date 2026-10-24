package observation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// memLog is an in-memory durable log with the semantics the Log port promises.
type memLog struct {
	mu    sync.Mutex
	recs  []Record
	floor int64
	reads int
	fail  error
}

func (l *memLog) add(env eventsv1.Envelope) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	seq := int64(len(l.recs)) + 1 + l.floor
	l.recs = append(l.recs, Record{Sequence: seq, Envelope: env, Size: 100})
	return seq
}

func (l *memLog) prune(upTo int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var keep []Record
	for _, r := range l.recs {
		if r.Sequence > upTo {
			keep = append(keep, r)
		}
	}
	l.recs, l.floor = keep, upTo
}

func (l *memLog) wm() Watermark {
	hw := l.floor
	if n := len(l.recs); n > 0 {
		hw = l.recs[n-1].Sequence
	}
	return Watermark{HighWater: hw, Floor: l.floor}
}

func (l *memLog) Watermark(context.Context, Scope) (Watermark, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.wm(), l.fail
}

func (l *memLog) Read(_ context.Context, _ Scope, after int64, limit int) ([]Record, Watermark, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reads++
	if l.fail != nil {
		return nil, Watermark{}, l.fail
	}
	if after < l.floor {
		return nil, l.wm(), ErrCursorExpired
	}
	var out []Record
	for _, r := range l.recs {
		if r.Sequence > after && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, l.wm(), nil
}

type memState struct {
	snap    func() (Snapshot, error)
	verify  error
	verifys int
}

func (s *memState) Verify(context.Context, Scope) error { s.verifys++; return s.verify }
func (s *memState) Snapshot(context.Context, Scope, int) (Snapshot, error) {
	if s.snap == nil {
		return Snapshot{}, nil
	}
	return s.snap()
}

type funcAccess func(ctx context.Context, f Family) error

func (a funcAccess) Authorize(ctx context.Context, _ Actor, _ Scope, f Family) error {
	return a(ctx, f)
}

func allow(context.Context, Family) error { return nil }

type recSink struct {
	mu        sync.Mutex
	frames    []string
	positions []int64
	onEvent   func(d Delivery) error
	onFlush   func() error
	flushes   int
}

func (s *recSink) add(f string) { s.mu.Lock(); s.frames = append(s.frames, f); s.mu.Unlock() }
func (s *recSink) Snapshot(_ context.Context, sn Snapshot) error {
	s.add(fmt.Sprintf("snapshot@%d/%d", sn.Position, len(sn.Resources)))
	return nil
}
func (s *recSink) Event(_ context.Context, d Delivery) error {
	if s.onEvent != nil {
		if err := s.onEvent(d); err != nil {
			return err
		}
	}
	s.add(fmt.Sprintf("event@%d", d.Position))
	s.mu.Lock()
	s.positions = append(s.positions, d.Position)
	s.mu.Unlock()
	return nil
}
func (s *recSink) Checkpoint(_ context.Context, p int64) error {
	s.add(fmt.Sprintf("checkpoint@%d", p))
	return nil
}
func (s *recSink) Heartbeat(context.Context) error { s.add("heartbeat"); return nil }
func (s *recSink) Flush(context.Context) error {
	s.mu.Lock()
	s.flushes++
	s.mu.Unlock()
	if s.onFlush != nil {
		return s.onFlush()
	}
	return nil
}
func (s *recSink) Resync(_ context.Context, cause error) error {
	s.add("resync:" + cause.Error())
	return nil
}
func (s *recSink) nPos() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.positions)
}
func (s *recSink) has(frame string) bool { return countOf(s.got(), frame) > 0 }
func (s *recSink) got() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.frames...)
}

func testOptions() Options {
	return Options{PageSize: 3, MaxPageBytes: 1 << 20, PollInterval: 5 * time.Millisecond,
		HeartbeatInterval: 30 * time.Millisecond, AccessInterval: 10 * time.Millisecond, MaxSnapshotResources: 10}
}

func workEvent(id string) eventsv1.Envelope {
	d, _ := json.Marshal(eventsv1.WorkItemCreated{Kind: "story", State: "backlog"})
	return eventsv1.Envelope{ID: id, SchemaVersion: 1, Type: eventsv1.TypeWorkItemCreated, TenantID: "t_acme", ProjectID: "p_inventory",
		Resource: eventsv1.Resource{Type: "work_item", ID: "wi_" + id, Version: 1}, Data: d}
}

func runEvent(id, run string) eventsv1.Envelope {
	d, _ := json.Marshal(eventsv1.RunStateChanged{})
	return eventsv1.Envelope{ID: id, SchemaVersion: 1, Type: eventsv1.TypeRunStateChanged, TenantID: "t_acme", ProjectID: "p_inventory",
		Resource: eventsv1.Resource{Type: "run", ID: run, Version: 1}, Data: d}
}

type rig struct {
	log   *memLog
	state *memState
	svc   *Service
}

func newRig(t *testing.T, access func(context.Context, Family) error, mutate ...func(*Config)) *rig {
	t.Helper()
	r := &rig{log: &memLog{}, state: &memState{}}
	cfg := Config{Log: r.log, State: r.state, Access: funcAccess(access), Options: testOptions()}
	for _, m := range mutate {
		m(&cfg)
	}
	svc, err := NewService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.svc = svc
	return r
}

func (r *rig) open(t *testing.T, scope Scope, resume *int64) *Stream {
	t.Helper()
	st, err := r.svc.Open(context.Background(), Request{Actor: Actor{ID: "usr_1"}, Scope: scope, Resume: resume})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return st
}

// runUntil runs the stream until cond holds, then cancels and returns its error.
func runUntil(t *testing.T, st *Stream, sink Sink, cond func() bool) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- st.Run(ctx, sink) }()
	deadline := time.After(3 * time.Second)
	for !cond() {
		select {
		case err := <-done:
			return err
		case <-deadline:
			cancel()
			<-done
			t.Fatal("condition not reached")
		case <-time.After(2 * time.Millisecond):
		}
	}
	cancel()
	return <-done
}

func ptr(v int64) *int64 { return &v }

func TestNewServiceRequiresEverything(t *testing.T) {
	good := Config{Log: &memLog{}, State: &memState{}, Access: funcAccess(allow), Options: testOptions()}
	if _, err := NewService(good); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Config){
		"log":             func(c *Config) { c.Log = nil },
		"state":           func(c *Config) { c.State = nil },
		"access":          func(c *Config) { c.Access = nil },
		"page":            func(c *Config) { c.Options.PageSize = 0 },
		"big":             func(c *Config) { c.Options.PageSize = MaxPageSize + 1 },
		"bytes":           func(c *Config) { c.Options.MaxPageBytes = 0 },
		"poll":            func(c *Config) { c.Options.PollInterval = 0 },
		"hb":              func(c *Config) { c.Options.HeartbeatInterval = 0 },
		"access-interval": func(c *Config) { c.Options.AccessInterval = 0 },
		"snapshot":        func(c *Config) { c.Options.MaxSnapshotResources = 0 },
	} {
		c := good
		mut(&c)
		if _, err := NewService(c); !errors.Is(err, ErrInvalidConfiguration) {
			t.Errorf("%s: err = %v, want ErrInvalidConfiguration", name, err)
		}
	}
}

func TestBacklogThenLive(t *testing.T) {
	r := newRig(t, allow)
	for i := 1; i <= 7; i++ {
		r.log.add(workEvent(fmt.Sprint("e", i)))
	}
	st := r.open(t, mustScope(t, ""), ptr(0))
	sink := &recSink{}
	go func() {
		time.Sleep(60 * time.Millisecond)
		r.log.add(workEvent("live1"))
	}()
	err := runUntil(t, st, sink, func() bool { return sink.nPos() == 8 })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("run ended with %v, want cancellation", err)
	}
	for i, p := range sink.positions {
		if p != int64(i+1) {
			t.Fatalf("positions = %v: out of order or skipped", sink.positions)
		}
	}
	if st.Session().Phase() != PhaseClosed || st.Session().Emitted() != 8 {
		t.Fatalf("phase=%s emitted=%d", st.Session().Phase(), st.Session().Emitted())
	}
	if hb := countOf(sink.got(), "heartbeat"); hb == 0 {
		t.Fatalf("an idle stream must heartbeat: %v", sink.got())
	}
}

func countOf(frames []string, f string) int {
	n := 0
	for _, x := range frames {
		if x == f {
			n++
		}
	}
	return n
}

func TestSnapshotStartsStreamAtItsPosition(t *testing.T) {
	r := newRig(t, allow)
	for i := 1; i <= 4; i++ {
		r.log.add(workEvent(fmt.Sprint("e", i)))
	}
	r.state.snap = func() (Snapshot, error) {
		return Snapshot{Position: 2, Resources: []Resource{{Type: FamilyWork, ID: "wi_1", Version: 1, State: "backlog"}}}, nil
	}
	st := r.open(t, mustScope(t, ""), nil)
	if st.Initial() == nil || st.Initial().Position != 2 {
		t.Fatalf("initial = %+v", st.Initial())
	}
	sink := &recSink{}
	_ = runUntil(t, st, sink, func() bool { return sink.nPos() == 2 })
	frames := sink.got()
	if frames[0] != "snapshot@2/1" || sink.positions[0] != 3 || sink.positions[1] != 4 {
		t.Fatalf("frames = %v", frames)
	}
}

func TestFilteredStreamAdvancesWithCheckpoints(t *testing.T) {
	r := newRig(t, allow)
	for i := 1; i <= 6; i++ { // work events only, filtered out by the run-type stream
		r.log.add(workEvent(fmt.Sprint("e", i)))
	}
	scope := mustScope(t, "", "run.state_changed")
	st := r.open(t, scope, ptr(0))
	sink := &recSink{}
	_ = runUntil(t, st, sink, func() bool { return sink.has("checkpoint@6") })
	if len(sink.positions) != 0 {
		t.Fatalf("forwarded filtered events: %v", sink.positions)
	}
	frames := sink.got()
	if countOf(frames, "checkpoint@3") != 1 || countOf(frames, "checkpoint@6") != 1 {
		t.Fatalf("a quiet filtered stream must checkpoint each page: %v", frames)
	}
	// A matching event after the checkpoint is delivered exactly once.
	r.log.add(runEvent("r1", "run_1"))
	st2 := r.open(t, scope, ptr(6))
	sink2 := &recSink{}
	_ = runUntil(t, st2, sink2, func() bool { return sink2.nPos() == 1 })
	if sink2.positions[0] != 7 {
		t.Fatalf("positions = %v", sink2.positions)
	}
}

func TestOpenResumeValidation(t *testing.T) {
	r := newRig(t, allow)
	for i := 1; i <= 5; i++ {
		r.log.add(workEvent(fmt.Sprint("e", i)))
	}
	r.log.prune(3)
	scope := mustScope(t, "")
	ctx := context.Background()
	open := func(pos int64) error {
		_, err := r.svc.Open(ctx, Request{Actor: Actor{ID: "usr_1"}, Scope: scope, Resume: ptr(pos)})
		return err
	}
	if err := open(2); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("below the floor: %v", err)
	}
	if err := open(3); err != nil {
		t.Fatalf("exactly at the floor must stay valid: %v", err)
	}
	if err := open(5); err != nil {
		t.Fatalf("at the high-water mark: %v", err)
	}
	if err := open(6); !errors.Is(err, ErrBadPosition) {
		t.Fatalf("ahead of the head: %v", err)
	}
	if err := open(-1); !errors.Is(err, ErrBadPosition) {
		t.Fatalf("negative: %v", err)
	}
	r.state.verify = fmt.Errorf("%w: gone", ErrNotFound)
	if err := open(4); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing resource: %v", err)
	}
	if _, err := r.svc.Open(ctx, Request{Scope: scope, Resume: ptr(4)}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no actor: %v", err)
	}
}

func TestRetentionAdvancingMidstreamEndsWithResync(t *testing.T) {
	r := newRig(t, allow)
	for i := 1; i <= 9; i++ {
		r.log.add(workEvent(fmt.Sprint("e", i)))
	}
	st := r.open(t, mustScope(t, ""), ptr(0))
	sink := &recSink{}
	sink.onEvent = func(d Delivery) error {
		if d.Position == 3 {
			r.log.prune(6) // retention overtakes the unread remainder
		}
		return nil
	}
	err := runUntil(t, st, sink, func() bool { return false })
	if !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("err = %v, want ErrCursorExpired", err)
	}
	frames := sink.got()
	if last := frames[len(frames)-1]; last[:6] != "resync" {
		t.Fatalf("the last frame must be the resync control frame: %v", frames)
	}
	for _, p := range sink.positions {
		if p > 3 {
			t.Fatalf("events past the pruned gap were delivered: %v", sink.positions)
		}
	}
}

func TestAccessChecks(t *testing.T) {
	t.Run("denied at open", func(t *testing.T) {
		r := newRig(t, func(context.Context, Family) error { return ErrDenied })
		_, err := r.svc.Open(context.Background(), Request{Actor: Actor{ID: "u"}, Scope: mustScope(t, ""), Resume: ptr(0)})
		if !errors.Is(err, ErrDenied) {
			t.Fatalf("err = %v", err)
		}
		_, err = r.svc.Snapshot(context.Background(), Actor{ID: "u"}, mustScope(t, ""))
		if !errors.Is(err, ErrDenied) {
			t.Fatalf("snapshot err = %v", err)
		}
	})
	t.Run("hidden at open", func(t *testing.T) {
		r := newRig(t, func(context.Context, Family) error { return ErrHidden })
		_, err := r.svc.Open(context.Background(), Request{Actor: Actor{ID: "u"}, Scope: mustScope(t, ""), Resume: ptr(0)})
		if !errors.Is(err, ErrHidden) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("an outage is never access", func(t *testing.T) {
		boom := errors.New("authority down")
		r := newRig(t, func(context.Context, Family) error { return boom })
		_, err := r.svc.Open(context.Background(), Request{Actor: Actor{ID: "u"}, Scope: mustScope(t, ""), Resume: ptr(0)})
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("err = %v, want ErrUnavailable", err)
		}
	})
	t.Run("revocation stops a backlog drain before the next event", func(t *testing.T) {
		var revoked bool
		var mu sync.Mutex
		r := newRig(t, func(context.Context, Family) error {
			mu.Lock()
			defer mu.Unlock()
			if revoked {
				return ErrHidden
			}
			return nil
		})
		for i := 1; i <= 9; i++ {
			r.log.add(workEvent(fmt.Sprint("e", i)))
		}
		st := r.open(t, mustScope(t, ""), ptr(0))
		sink := &recSink{onEvent: func(d Delivery) error {
			if d.Position == 4 {
				mu.Lock()
				revoked = true
				mu.Unlock()
			}
			return nil
		}}
		err := runUntil(t, st, sink, func() bool { return false })
		if !errors.Is(err, ErrAccessRevoked) {
			t.Fatalf("err = %v, want ErrAccessRevoked", err)
		}
		if sink.positions[len(sink.positions)-1] != 4 {
			t.Fatalf("delivered %v after revocation", sink.positions)
		}
	})
	t.Run("revocation is noticed while idle", func(t *testing.T) {
		var revoked bool
		var mu sync.Mutex
		r := newRig(t, func(context.Context, Family) error {
			mu.Lock()
			defer mu.Unlock()
			if revoked {
				return ErrDenied
			}
			return nil
		})
		st := r.open(t, mustScope(t, ""), ptr(0))
		go func() { time.Sleep(40 * time.Millisecond); mu.Lock(); revoked = true; mu.Unlock() }()
		err := runUntil(t, st, &recSink{}, func() bool { return false })
		if !errors.Is(err, ErrAccessRevoked) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("an authority outage mid-stream closes the stream", func(t *testing.T) {
		var down bool
		var mu sync.Mutex
		r := newRig(t, func(context.Context, Family) error {
			mu.Lock()
			defer mu.Unlock()
			if down {
				return errors.New("authority outage")
			}
			return nil
		})
		for i := 1; i <= 3; i++ {
			r.log.add(workEvent(fmt.Sprint("e", i)))
		}
		st := r.open(t, mustScope(t, ""), ptr(0))
		sink := &recSink{onEvent: func(d Delivery) error {
			if d.Position == 1 {
				mu.Lock()
				down = true
				mu.Unlock()
			}
			return nil
		}}
		err := runUntil(t, st, sink, func() bool { return false })
		if !errors.Is(err, ErrUnavailable) || len(sink.positions) != 1 {
			t.Fatalf("err=%v positions=%v", err, sink.positions)
		}
	})
	t.Run("one denied family is skipped while another stays observable", func(t *testing.T) {
		r := newRig(t, func(_ context.Context, f Family) error {
			if f == FamilyRun {
				return ErrDenied
			}
			return nil
		})
		r.log.add(workEvent("e1"))
		r.log.add(runEvent("r1", "run_1"))
		r.log.add(workEvent("e3"))
		st := r.open(t, mustScope(t, ""), ptr(0))
		sink := &recSink{}
		_ = runUntil(t, st, sink, func() bool { return sink.nPos() == 2 })
		if fmt.Sprint(sink.positions) != "[1 3]" {
			t.Fatalf("positions = %v: the run event must not be disclosed", sink.positions)
		}
	})
	t.Run("snapshot release rechecks access", func(t *testing.T) {
		calls := 0
		var mu sync.Mutex
		r := newRig(t, func(context.Context, Family) error {
			mu.Lock()
			defer mu.Unlock()
			calls++
			if calls > 3 { // the checks before the read pass; the one before release is refused
				return ErrHidden
			}
			return nil
		})
		r.state.snap = func() (Snapshot, error) {
			return Snapshot{Position: 1, Resources: []Resource{{Type: FamilyWork, ID: "wi_1", Version: 1, State: "backlog"}}}, nil
		}
		_, err := r.svc.Snapshot(context.Background(), Actor{ID: "u"}, mustScope(t, ""))
		if !errors.Is(err, ErrHidden) {
			t.Fatalf("err = %v: a snapshot read before revocation must not be released after it", err)
		}
	})
	t.Run("snapshot is filtered to readable families", func(t *testing.T) {
		r := newRig(t, func(_ context.Context, f Family) error {
			if f == FamilyWork {
				return ErrDenied
			}
			return nil
		})
		r.state.snap = func() (Snapshot, error) {
			return Snapshot{Position: 4, Resources: []Resource{
				{Type: FamilyWork, ID: "wi_1", Version: 1, State: "backlog"},
				{Type: FamilyRun, ID: "run_1", Version: 1, State: "running"},
			}}, nil
		}
		snap, err := r.svc.Snapshot(context.Background(), Actor{ID: "u"}, mustScope(t, ""))
		if err != nil || len(snap.Resources) != 1 || snap.Resources[0].Type != FamilyRun || snap.Position != 4 {
			t.Fatalf("snapshot = %+v, %v", snap, err)
		}
	})
}

func TestSnapshotFailuresIssueNoPosition(t *testing.T) {
	r := newRig(t, allow)
	r.state.snap = func() (Snapshot, error) { return Snapshot{}, ErrSnapshotTooLarge }
	if _, err := r.svc.Snapshot(context.Background(), Actor{ID: "u"}, mustScope(t, "")); !errors.Is(err, ErrSnapshotTooLarge) {
		t.Fatalf("err = %v", err)
	}
	if _, err := r.svc.Open(context.Background(), Request{Actor: Actor{ID: "u"}, Scope: mustScope(t, "")}); !errors.Is(err, ErrSnapshotTooLarge) {
		t.Fatalf("open err = %v", err)
	}
	// A binding that returns more than the bound is refused, never truncated.
	r.state.snap = func() (Snapshot, error) {
		return Snapshot{Position: 1, Resources: make([]Resource, 11)}, nil
	}
	if _, err := r.svc.Snapshot(context.Background(), Actor{ID: "u"}, mustScope(t, "")); !errors.Is(err, ErrSnapshotTooLarge) {
		t.Fatalf("oversized snapshot err = %v", err)
	}
	r.state.snap = func() (Snapshot, error) { return Snapshot{}, Unavailablef(nil, "binding down") }
	if _, err := r.svc.Snapshot(context.Background(), Actor{ID: "u"}, mustScope(t, "")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unavailable binding err = %v", err)
	}
}

func TestSlowSinkEndsStreamWithoutLoss(t *testing.T) {
	r := newRig(t, allow)
	for i := 1; i <= 9; i++ {
		r.log.add(workEvent(fmt.Sprint("e", i)))
	}
	st := r.open(t, mustScope(t, ""), ptr(0))
	sink := &recSink{onEvent: func(d Delivery) error {
		if d.Position == 5 {
			return ErrSlowClient
		}
		return nil
	}}
	err := runUntil(t, st, sink, func() bool { return false })
	if !errors.Is(err, ErrSlowClient) {
		t.Fatalf("err = %v", err)
	}
	if fmt.Sprint(sink.positions) != "[1 2 3 4]" {
		t.Fatalf("positions = %v: nothing past the stalled write may be delivered", sink.positions)
	}
	if st.Session().Phase() != PhaseClosed {
		t.Fatal("the session must be closed")
	}
	// Positions covered by a successful flush are the resume point: 3 (page one).
	if st.Session().Emitted() != 3 {
		t.Fatalf("emitted = %d, want 3 (the last flushed page)", st.Session().Emitted())
	}
}

func TestFlushFailureLeavesEmittedBehind(t *testing.T) {
	r := newRig(t, allow)
	for i := 1; i <= 3; i++ {
		r.log.add(workEvent(fmt.Sprint("e", i)))
	}
	st := r.open(t, mustScope(t, ""), ptr(0))
	sink := &recSink{onFlush: func() error { return ErrSlowClient }}
	if err := runUntil(t, st, sink, func() bool { return false }); !errors.Is(err, ErrSlowClient) {
		t.Fatalf("err = %v", err)
	}
	if st.Session().Emitted() != 0 || st.Session().Scanned() != 3 {
		t.Fatalf("scanned=%d emitted=%d: unflushed events must not count as emitted", st.Session().Scanned(), st.Session().Emitted())
	}
}

func TestPagesAreCutByBytes(t *testing.T) {
	r := newRig(t, allow, func(c *Config) { c.Options.MaxPageBytes = 150; c.Options.PageSize = 5 })
	for i := 1; i <= 6; i++ {
		r.log.add(workEvent(fmt.Sprint("e", i)))
	}
	st := r.open(t, mustScope(t, ""), ptr(0))
	sink := &recSink{}
	_ = runUntil(t, st, sink, func() bool { return sink.nPos() == 6 })
	if fmt.Sprint(sink.positions) != "[1 2 3 4 5 6]" {
		t.Fatalf("positions = %v", sink.positions)
	}
	if sink.flushes < 6 {
		t.Fatalf("flushes = %d: a 150-byte page bound over 100-byte records must flush per record", sink.flushes)
	}
}

func TestLogFailuresAreNotSwallowed(t *testing.T) {
	r := newRig(t, allow)
	r.log.add(workEvent("e1"))
	st := r.open(t, mustScope(t, ""), ptr(0))
	r.log.fail = Unavailablef(nil, "database down")
	if err := runUntil(t, st, &recSink{}, func() bool { return false }); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestInconsistentLogFailsClosed(t *testing.T) {
	r := newRig(t, allow)
	r.log.add(workEvent("e1"))
	r.log.add(workEvent("e2"))
	// Simulate a log whose rows vanished without the floor advancing.
	r.log.recs = r.log.recs[:0]
	r.log.floor = 0
	bad := &holeLog{memLog: r.log}
	r.svc.log = bad
	st := &Stream{svc: r.svc, actor: Actor{ID: "u"}}
	st.session, _ = NewSession(mustScope(t, ""), 0)
	err := runUntil(t, st, &recSink{}, func() bool { return false })
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable instead of spinning", err)
	}
}

// holeLog claims a high-water mark of 2 while holding no observations.
type holeLog struct{ *memLog }

func (h *holeLog) Read(context.Context, Scope, int64, int) ([]Record, Watermark, error) {
	return nil, Watermark{HighWater: 2}, nil
}

var _ = domain.TenantID("")
