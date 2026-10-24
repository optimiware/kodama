package exceptions_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"example.invalid/inventory/internal/exceptions"
	"example.invalid/inventory/internal/exceptions/exceptionstest"
	"example.invalid/inventory/internal/tenancy"
)

const (
	tenantA = tenancy.TenantID("11111111-1111-4111-8111-111111111111")
	tenantB = tenancy.TenantID("22222222-2222-4222-8222-222222222222")
)

var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func access(t *testing.T, tenant tenancy.TenantID, subject string, role tenancy.Role) tenancy.Access {
	t.Helper()
	a, err := tenancy.NewAccess(tenant, subject, role)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func newException(t *testing.T) exceptions.Exception {
	t.Helper()
	e, err := exceptions.NewException(tenantA, exceptions.NewID(), "WH1/SKU-9", "count differs", 10, 7, "alice", t0)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func request(e exceptions.Exception, key string) exceptions.ResolveRequest {
	return exceptions.ResolveRequest{Actor: "alice", AdjustedQuantity: 7, Reason: "recount confirmed", IdempotencyKey: key, ExpectedVersion: e.Version}
}

func TestExceptionTransitions(t *testing.T) {
	t.Run("accepted transitions", func(t *testing.T) {
		e := newException(t)
		if e.State != exceptions.StateOpen || e.Version != 1 {
			t.Fatalf("new exception: %+v", e)
		}
		in, err := e.RequestResolution(request(e, "key-00000001"), t0)
		if err != nil || e.State != exceptions.StatePending || e.Version != 2 {
			t.Fatalf("request: %v %+v", err, e)
		}
		if in.OperationKey != exceptions.OperationKeyFor(e.ID) {
			t.Fatalf("operation key must derive from the exception: %q", in.OperationKey)
		}
		if err := e.MarkUnknown(in.OperationKey, "timeout", t0); err != nil || e.State != exceptions.StateUnknown {
			t.Fatalf("unknown: %v %s", err, e.State)
		}
		if err := e.Acknowledge(in.OperationKey, "erp-1", t0); err != nil || e.State != exceptions.StateResolved || e.Resolution.ERPReference != "erp-1" {
			t.Fatalf("unknown -> resolved: %v %+v", err, e)
		}
		v := e.Version
		if err := e.Acknowledge(in.OperationKey, "erp-1", t0); err != nil || e.Version != v {
			t.Fatalf("a replayed acknowledgment must change nothing: %v", err)
		}
	})

	t.Run("rejected transitions", func(t *testing.T) {
		e := newException(t)
		in, _ := e.RequestResolution(request(e, "key-00000001"), t0)
		stale := exceptions.ResolveRequest{Actor: "alice", AdjustedQuantity: 7, Reason: "r", IdempotencyKey: "key-00000002", ExpectedVersion: 1}
		if _, err := e.RequestResolution(stale, t0); !errors.Is(err, exceptions.ErrVersionConflict) {
			t.Fatalf("stale version expected: %v", err)
		}
		fresh := request(e, "key-00000002")
		if _, err := e.RequestResolution(fresh, t0); !errors.Is(err, exceptions.ErrInvalidState) {
			t.Fatalf("a pending exception cannot accept another resolution: %v", err)
		}
		if err := e.Acknowledge("inv-adj-other", "x", t0); !errors.Is(err, exceptions.ErrOperationMismatch) {
			t.Fatalf("an acknowledgment for another operation: %v", err)
		}
		if err := e.Reject(in.OperationKey, "invalid item", t0); err != nil || e.State != exceptions.StateFailed {
			t.Fatalf("reject: %v", err)
		}
		if err := e.Acknowledge(in.OperationKey, "erp-1", t0); !errors.Is(err, exceptions.ErrInvalidState) {
			t.Fatalf("failed is terminal: %v", err)
		}
		if err := e.MarkUnknown(in.OperationKey, "x", t0); !errors.Is(err, exceptions.ErrInvalidState) {
			t.Fatalf("failed is terminal: %v", err)
		}
		open := newException(t)
		if err := open.Acknowledge(exceptions.OperationKeyFor(open.ID), "x", t0); !errors.Is(err, exceptions.ErrOperationMismatch) {
			t.Fatalf("an open exception cannot be acknowledged: %v", err)
		}
	})

	t.Run("invalid values", func(t *testing.T) {
		cases := map[string]func() error{
			"negative erp quantity": func() error {
				_, err := exceptions.NewException(tenantA, exceptions.NewID(), "A", "", -1, 2, "a", t0)
				return err
			},
			"no gap": func() error {
				_, err := exceptions.NewException(tenantA, exceptions.NewID(), "A", "", 2, 2, "a", t0)
				return err
			},
			"huge quantity": func() error {
				_, err := exceptions.NewException(tenantA, exceptions.NewID(), "A", "", 1, exceptions.MaxQuantity+1, "a", t0)
				return err
			},
			"bad reference": func() error {
				_, err := exceptions.NewException(tenantA, exceptions.NewID(), "a b", "", 1, 2, "a", t0)
				return err
			},
			"no actor": func() error {
				_, err := exceptions.NewException(tenantA, exceptions.NewID(), "A", "", 1, 2, "", t0)
				return err
			},
			"bad tenant": func() error {
				_, err := exceptions.NewException("x", exceptions.NewID(), "A", "", 1, 2, "a", t0)
				return err
			},
			"bad id": func() error { _, err := exceptions.NewException(tenantA, "x", "A", "", 1, 2, "a", t0); return err },
			"negative adjustment": func() error {
				r := request(newException(t), "key-00000001")
				r.AdjustedQuantity = -1
				return r.Validate()
			},
			"empty reason":          func() error { r := request(newException(t), "key-00000001"); r.Reason = ""; return r.Validate() },
			"short idempotency key": func() error { r := request(newException(t), "short"); return r.Validate() },
			"missing expected version": func() error {
				r := request(newException(t), "key-00000001")
				r.ExpectedVersion = 0
				return r.Validate()
			},
			"missing actor": func() error { r := request(newException(t), "key-00000001"); r.Actor = ""; return r.Validate() },
		}
		for name, f := range cases {
			if err := f(); !errors.Is(err, exceptions.ErrInvalid) {
				t.Errorf("%s: want ErrInvalid, got %v", name, err)
			}
		}
	})
}

func newService(t *testing.T, store exceptions.Store, erp exceptions.ERP) *exceptions.Service {
	t.Helper()
	s, err := exceptions.NewService(exceptions.Config{Store: store, ERP: erp, Clock: func() time.Time { return t0 }, MaxAttempts: 3, RetryBase: time.Second, RetryMax: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestExceptionTransitionsAuthorization(t *testing.T) {
	svc := newService(t, exceptionstest.NewStore(), nil)
	ctx := context.Background()
	reviewer := access(t, tenantA, "rita", tenancy.RoleReviewer)
	e, err := svc.Create(ctx, reviewer, exceptions.CreateInput{ERPRef: "WH1/SKU-9", ERPQuantity: 10, CountedQuantity: 7})
	if err != nil {
		t.Fatal(err)
	}
	viewer := access(t, tenantA, "vic", tenancy.RoleViewer)
	worker := access(t, tenantA, "svc", tenancy.RoleWorker)
	if _, err := svc.Create(ctx, viewer, exceptions.CreateInput{ERPRef: "X", ERPQuantity: 1, CountedQuantity: 2}); !errors.Is(err, tenancy.ErrForbidden) {
		t.Fatalf("a viewer cannot create: %v", err)
	}
	if _, err := svc.Get(ctx, viewer, e.ID); err != nil {
		t.Fatalf("a viewer can read: %v", err)
	}
	for _, a := range []tenancy.Access{viewer, worker, {}} {
		if _, err := svc.Resolve(ctx, a, e.ID, 7, "r", "key-00000001", 1); !errors.Is(err, tenancy.ErrForbidden) {
			t.Fatalf("%q cannot resolve: %v", a.Role(), err)
		}
	}
	if _, err := svc.Process(ctx, reviewer, "w"); !errors.Is(err, tenancy.ErrForbidden) {
		t.Fatalf("a human cannot process intents: %v", err)
	}
	// The actor is the verified subject, never request content.
	r, err := svc.Resolve(ctx, reviewer, e.ID, 7, "recount", "key-00000001", 1)
	if err != nil || r.Exception.Resolution.RequestedBy != "rita" {
		t.Fatalf("resolve: %v %+v", err, r.Exception.Resolution)
	}
	// Another tenant's access reaches nothing.
	other := access(t, tenantB, "rita", tenancy.RoleAdmin)
	if _, err := svc.Get(ctx, other, e.ID); !errors.Is(err, exceptions.ErrNotFound) {
		t.Fatalf("cross-tenant read: %v", err)
	}
	if _, err := svc.Resolve(ctx, other, e.ID, 1, "r", "key-00000009", 1); !errors.Is(err, exceptions.ErrNotFound) {
		t.Fatalf("cross-tenant write: %v", err)
	}
}

func TestExceptionTransitionsIdempotency(t *testing.T) {
	svc := newService(t, exceptionstest.NewStore(), nil)
	ctx := context.Background()
	a := access(t, tenantA, "rita", tenancy.RoleReviewer)
	e, _ := svc.Create(ctx, a, exceptions.CreateInput{ERPRef: "WH1/SKU-9", ERPQuantity: 10, CountedQuantity: 7})

	first, err := svc.Resolve(ctx, a, e.ID, 7, "recount", "key-00000001", 1)
	if err != nil || first.Replayed || first.Exception.State != exceptions.StatePending {
		t.Fatalf("first: %v %+v", err, first)
	}
	again, err := svc.Resolve(ctx, a, e.ID, 7, "recount", "key-00000001", 1)
	if err != nil || !again.Replayed || again.Exception.Version != first.Exception.Version {
		t.Fatalf("identical request must return the existing operation: %v %+v", err, again)
	}
	if _, err := svc.Resolve(ctx, a, e.ID, 8, "recount", "key-00000001", 1); !errors.Is(err, exceptions.ErrIdempotencyConflict) {
		t.Fatalf("changed payload under the same key: %v", err)
	}
	if _, err := svc.Resolve(ctx, a, e.ID, 7, "recount", "key-00000002", 2); !errors.Is(err, exceptions.ErrInvalidState) {
		t.Fatalf("a different key cannot start a second resolution: %v", err)
	}
	if _, err := svc.Resolve(ctx, a, e.ID, 7, "recount", "key-00000003", 1); !errors.Is(err, exceptions.ErrVersionConflict) {
		t.Fatalf("a stale expected version: %v", err)
	}
	got, _ := svc.Get(ctx, a, e.ID)
	if got.State != exceptions.StatePending {
		t.Fatalf("resolved must wait for the ERP acknowledgment, got %s", got.State)
	}
}

func TestExceptionTransitionsConcurrentDuplicates(t *testing.T) {
	store := exceptionstest.NewStore()
	svc := newService(t, store, nil)
	ctx := context.Background()
	a := access(t, tenantA, "rita", tenancy.RoleReviewer)
	e, _ := svc.Create(ctx, a, exceptions.CreateInput{ERPRef: "WH1/SKU-9", ERPQuantity: 10, CountedQuantity: 7})

	var wg sync.WaitGroup
	var accepted, replayed, conflicts atomic.Int32
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := "key-00000001"
			if i%2 == 1 {
				key = "key-0000000" + string(rune('2'+i%5))
			}
			r, err := svc.Resolve(ctx, a, e.ID, 7, "recount", key, 1)
			switch {
			case err == nil && r.Replayed:
				replayed.Add(1)
			case err == nil:
				accepted.Add(1)
			case errors.Is(err, exceptions.ErrInvalidState), errors.Is(err, exceptions.ErrVersionConflict):
				conflicts.Add(1)
			default:
				t.Errorf("unexpected: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("exactly one resolution may be accepted, got %d (replayed %d, conflicts %d)", accepted.Load(), replayed.Load(), conflicts.Load())
	}
}

// scriptedERP is a controllable ERP double that counts effects by key.
type scriptedERP struct {
	mu        sync.Mutex
	effects   map[string]int
	applyErr  error
	lookupErr error
	loseAck   bool
	reject    string
}

func (e *scriptedERP) Apply(_ context.Context, _ tenancy.TenantID, op exceptions.Operation) (exceptions.Outcome, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.effects == nil {
		e.effects = map[string]int{}
	}
	if e.reject != "" {
		return exceptions.Outcome{Status: exceptions.OutcomeRejected, Reason: e.reject}, nil
	}
	if e.applyErr != nil {
		return exceptions.Outcome{}, e.applyErr
	}
	if e.effects[op.Key] == 0 {
		e.effects[op.Key] = 1
	}
	if e.loseAck {
		e.loseAck = false
		return exceptions.Outcome{}, errors.New("connection reset")
	}
	return exceptions.Outcome{Status: exceptions.OutcomeAcknowledged, Reference: "erp-" + op.Key}, nil
}

func (e *scriptedERP) Lookup(_ context.Context, _ tenancy.TenantID, key string) (exceptions.Outcome, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lookupErr != nil {
		return exceptions.Outcome{}, e.lookupErr
	}
	if e.effects[key] == 0 {
		return exceptions.Outcome{Status: exceptions.OutcomeNotFound}, nil
	}
	return exceptions.Outcome{Status: exceptions.OutcomeAcknowledged, Reference: "erp-" + key}, nil
}

func TestExceptionTransitionsERPOutcomes(t *testing.T) {
	ctx := context.Background()
	setup := func(erp *scriptedERP) (*exceptions.Service, *exceptionstest.Store, tenancy.Access, tenancy.Access, exceptions.Exception) {
		store := exceptionstest.NewStore()
		svc := newService(t, store, erp)
		human := access(t, tenantA, "rita", tenancy.RoleReviewer)
		worker := access(t, tenantA, "svc-worker", tenancy.RoleWorker)
		e, _ := svc.Create(ctx, human, exceptions.CreateInput{ERPRef: "WH1/SKU-9", ERPQuantity: 10, CountedQuantity: 7})
		if _, err := svc.Resolve(ctx, human, e.ID, 7, "recount", "key-00000001", 1); err != nil {
			t.Fatal(err)
		}
		return svc, store, human, worker, e
	}

	t.Run("acknowledged", func(t *testing.T) {
		erp := &scriptedERP{}
		svc, _, human, worker, e := setup(erp)
		if ok, err := svc.Process(ctx, worker, "w1"); !ok || err != nil {
			t.Fatalf("process: %v %v", ok, err)
		}
		got, _ := svc.Get(ctx, human, e.ID)
		if got.State != exceptions.StateResolved {
			t.Fatalf("want resolved, got %s", got.State)
		}
		if ok, _ := svc.Process(ctx, worker, "w1"); ok {
			t.Fatal("nothing is left to process")
		}
	})

	t.Run("lost acknowledgment is reconciled by the same key without a second effect", func(t *testing.T) {
		erp := &scriptedERP{loseAck: true}
		svc, _, human, worker, e := setup(erp)
		if _, err := svc.Process(ctx, worker, "w1"); err != nil {
			t.Fatal(err)
		}
		got, _ := svc.Get(ctx, human, e.ID)
		if got.State != exceptions.StateUnknown {
			t.Fatalf("an unacknowledged write must be visibly unknown, got %s", got.State)
		}
		if ok, _ := svc.Process(ctx, worker, "w1"); ok {
			t.Fatal("reconciliation is not due before the backoff elapses")
		}
	})

	t.Run("a rejected adjustment fails the exception", func(t *testing.T) {
		erp := &scriptedERP{reject: "unknown item"}
		svc, _, human, worker, e := setup(erp)
		if _, err := svc.Process(ctx, worker, "w1"); err != nil {
			t.Fatal(err)
		}
		got, _ := svc.Get(ctx, human, e.ID)
		if got.State != exceptions.StateFailed || got.Resolution.Detail != "unknown item" {
			t.Fatalf("got %s %+v", got.State, got.Resolution)
		}
	})

	t.Run("unreachable ERP stays unknown and is never reported resolved", func(t *testing.T) {
		erp := &scriptedERP{applyErr: errors.New("dial tcp: refused"), lookupErr: errors.New("dial tcp: refused")}
		clock := t0
		store := exceptionstest.NewStore()
		svc, _ := exceptions.NewService(exceptions.Config{Store: store, ERP: erp, Clock: func() time.Time { return clock }, MaxAttempts: 3, RetryBase: time.Second, RetryMax: time.Minute})
		human := access(t, tenantA, "rita", tenancy.RoleReviewer)
		worker := access(t, tenantA, "svc-worker", tenancy.RoleWorker)
		e, _ := svc.Create(ctx, human, exceptions.CreateInput{ERPRef: "WH1/SKU-9", ERPQuantity: 10, CountedQuantity: 7})
		_, _ = svc.Resolve(ctx, human, e.ID, 7, "recount", "key-00000001", 1)
		attempts := 0
		for i := 0; i < 10; i++ {
			clock = clock.Add(time.Hour)
			if ok, _ := svc.Process(ctx, worker, "w1"); ok {
				attempts++
			}
		}
		if attempts != 3 {
			t.Fatalf("retries must be bounded at MaxAttempts: %d", attempts)
		}
		got, _ := svc.Get(ctx, human, e.ID)
		if got.State != exceptions.StateUnknown {
			t.Fatalf("an unresolved outcome stays visible as unknown, got %s", got.State)
		}
		if erp.effects[exceptions.OperationKeyFor(e.ID)] != 0 {
			t.Fatal("no effect was possible")
		}
	})

	t.Run("a crashed worker's lease expires and the same operation is reconciled", func(t *testing.T) {
		erp := &scriptedERP{}
		clock := t0
		store := exceptionstest.NewStore()
		svc, _ := exceptions.NewService(exceptions.Config{Store: store, ERP: erp, Clock: func() time.Time { return clock }, Lease: time.Minute})
		human := access(t, tenantA, "rita", tenancy.RoleReviewer)
		worker := access(t, tenantA, "svc-worker", tenancy.RoleWorker)
		e, _ := svc.Create(ctx, human, exceptions.CreateInput{ERPRef: "WH1/SKU-9", ERPQuantity: 10, CountedQuantity: 7})
		_, _ = svc.Resolve(ctx, human, e.ID, 7, "recount", "key-00000001", 1)
		// The first worker applied the effect, then died before recording it.
		claim, _ := store.Claim(ctx, worker, "dead-worker", time.Minute, clock, 8)
		_, _ = erp.Apply(ctx, tenantA, exceptions.Operation{Key: claim.Intent.OperationKey})
		if ok, _ := svc.Process(ctx, worker, "w2"); ok {
			t.Fatal("the lease is still held")
		}
		clock = clock.Add(2 * time.Minute)
		if ok, err := svc.Process(ctx, worker, "w2"); !ok || err != nil {
			t.Fatalf("reclaim: %v %v", ok, err)
		}
		got, _ := svc.Get(ctx, human, e.ID)
		if got.State != exceptions.StateResolved || erp.effects[claim.Intent.OperationKey] != 1 {
			t.Fatalf("state %s, effects %d", got.State, erp.effects[claim.Intent.OperationKey])
		}
	})
}

func TestExceptionTransitionsAuditFailureRollsBack(t *testing.T) {
	store := exceptionstest.NewStore()
	svc := newService(t, store, nil)
	ctx := context.Background()
	a := access(t, tenantA, "rita", tenancy.RoleReviewer)
	e, _ := svc.Create(ctx, a, exceptions.CreateInput{ERPRef: "WH1/SKU-9", ERPQuantity: 10, CountedQuantity: 7})
	store.FailAudit = true
	if _, err := svc.Resolve(ctx, a, e.ID, 7, "recount", "key-00000001", 1); err == nil {
		t.Fatal("an audit failure must fail the command")
	}
	store.FailAudit = false
	got, _ := svc.Get(ctx, a, e.ID)
	if got.State != exceptions.StateOpen || got.Version != 1 {
		t.Fatalf("nothing may commit without its audit entry: %+v", got)
	}
}
