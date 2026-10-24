package acceptance

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestERPDuplicateResolution proves duplicate resolutions create one durable
// intent and one ERP effect, a changed payload under a used key conflicts,
// acknowledgment loss and worker restarts reconcile the same operation, and an
// unresolved outcome stays visible instead of being reported as resolved.
func TestERPDuplicateResolution(t *testing.T) {
	path := func(id string) string { return fmt.Sprintf("/api/v1/tenants/%s/exceptions/%s", tenantA, id) }

	t.Run("concurrent duplicates create one intent and one effect", func(t *testing.T) {
		s := newSystem(t)
		id := s.createException(tenantA, "rita", "WH1/SKU-9")
		var accepted, replayed, refused atomic.Int32
		var wg sync.WaitGroup
		for i := 0; i < 24; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				key := "key-same-000001"
				if i%4 == 3 {
					key = fmt.Sprintf("key-other-%06d", i) // competing commands with their own keys
				}
				code, _, hdr := s.do("POST", path(id)+"/resolution", s.token("rita"), map[string]any{"adjustedQuantity": 7, "reason": "recount confirmed"},
					map[string]string{"Idempotency-Key": key, "If-Match": `"1"`})
				switch {
				case code == 202 && hdr.Get("Idempotent-Replayed") == "true":
					replayed.Add(1)
				case code == 202:
					accepted.Add(1)
				case code == 409 || code == 412:
					refused.Add(1)
				default:
					t.Errorf("unexpected status %d", code)
				}
			}(i)
		}
		wg.Wait()
		if accepted.Load() != 1 {
			t.Fatalf("exactly one command may be accepted: accepted=%d replayed=%d refused=%d", accepted.Load(), replayed.Load(), refused.Load())
		}
		if _, err := s.worker(tenantA, "svc-worker-a"); err != nil {
			t.Fatal(err)
		}
		s.eventually("resolution", 10*time.Second, func() bool { st, _ := s.state(tenantA, "rita", id); return st == "resolved" })
		time.Sleep(500 * time.Millisecond) // a duplicate effect would show up now
		if e, ok := s.erp.Effect(tenantA, "inv-adj-"+id); !ok || e.Applied != 1 {
			t.Fatalf("one ERP effect expected: %+v %v", e, ok)
		}
		if s.erp.TotalEffects() != 1 {
			t.Fatalf("total effects %d", s.erp.TotalEffects())
		}
		_, body := s.state(tenantA, "rita", id)
		if body["resolution"].(map[string]any)["erpReference"] == "" {
			t.Fatal("a resolved exception carries the ERP reference")
		}
	})

	t.Run("a changed payload under a used key conflicts and nothing is replaced", func(t *testing.T) {
		s := newSystem(t)
		id := s.createException(tenantA, "rita", "WH1/SKU-9")
		if code, _ := s.resolve(tenantA, "rita", id, "key-fixed-0001", 1, 7); code != 202 {
			t.Fatal(code)
		}
		if code, body := s.resolve(tenantA, "rita", id, "key-fixed-0001", 1, 5); code != 409 || body["error"].(map[string]any)["code"] != "idempotency_conflict" {
			t.Fatalf("changed quantity: %d %v", code, body)
		}
		if code, _ := s.resolve(tenantA, "rita", id, "key-fixed-0001", 1, 7); code != 202 {
			t.Fatalf("the identical request returns the existing operation: %d", code)
		}
		if _, err := s.worker(tenantA, "svc-worker-a"); err != nil {
			t.Fatal(err)
		}
		s.eventually("resolution", 10*time.Second, func() bool { st, _ := s.state(tenantA, "rita", id); return st == "resolved" })
		if e, _ := s.erp.Effect(tenantA, "inv-adj-"+id); e.Payload.Quantity != 7 {
			t.Fatalf("the ERP applied the reviewed quantity: %+v", e)
		}
	})

	t.Run("acknowledgment loss is reconciled by the same operation key", func(t *testing.T) {
		s := newSystem(t)
		id := s.createException(tenantA, "rita", "WH1/SKU-9")
		s.erp.LoseAcknowledgments(1)
		if code, _ := s.resolve(tenantA, "rita", id, "key-lost-ack-01", 1, 7); code != 202 {
			t.Fatal(code)
		}
		if _, err := s.worker(tenantA, "svc-worker-a"); err != nil {
			t.Fatal(err)
		}
		s.eventually("resolved after reconciliation", 15*time.Second, func() bool { st, _ := s.state(tenantA, "rita", id); return st == "resolved" })
		e, _ := s.erp.Effect(tenantA, "inv-adj-"+id)
		if e.Applied != 1 {
			t.Fatalf("a lost acknowledgment must never produce a second effect: %+v", e)
		}
		_, body, _ := s.do("GET", path(id)+"/history", s.token("rita"), nil, nil)
		var actions []string
		for _, it := range body["items"].([]any) {
			actions = append(actions, it.(map[string]any)["action"].(string))
		}
		if fmt.Sprint(actions) != "[created resolution_requested erp_unknown erp_acknowledged]" {
			t.Fatalf("the uncertainty stayed visible in the audit trail: %v", actions)
		}
	})

	t.Run("a worker killed after the effect leaves a recoverable operation", func(t *testing.T) {
		s := newSystem(t)
		id := s.createException(tenantA, "rita", "WH1/SKU-9")
		if code, _ := s.resolve(tenantA, "rita", id, "key-crash-0001", 1, 7); code != 202 {
			t.Fatal(code)
		}
		s.erp.HoldAfterEffect(true)
		stop, err := s.worker(tenantA, "svc-worker-a")
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-s.erp.Applied:
		case <-time.After(10 * time.Second):
			t.Fatal("the worker never reached the ERP")
		}
		stop() // killed between the remote effect and recording the acknowledgment
		if st, _ := s.state(tenantA, "rita", id); st != "pending" {
			t.Fatalf("an unrecorded outcome is pending, never resolved: %s", st)
		}
		s.erp.HoldAfterEffect(false)
		if _, err := s.worker(tenantA, "svc-worker-a"); err != nil {
			t.Fatal(err)
		}
		s.eventually("recovery", 15*time.Second, func() bool { st, _ := s.state(tenantA, "rita", id); return st == "resolved" })
		if e, _ := s.erp.Effect(tenantA, "inv-adj-"+id); e.Applied != 1 {
			t.Fatalf("recovery reconciled instead of writing again: %+v", e)
		}
	})

	t.Run("an unreachable ERP stays visibly unknown, then reconciles when it returns", func(t *testing.T) {
		s := newSystem(t)
		id := s.createException(tenantA, "rita", "WH1/SKU-9")
		s.erp.SetApplyDown(true)
		s.erp.SetLookupDown(true)
		if code, _ := s.resolve(tenantA, "rita", id, "key-down-000001", 1, 7); code != 202 {
			t.Fatal(code)
		}
		if _, err := s.worker(tenantA, "svc-worker-a"); err != nil {
			t.Fatal(err)
		}
		s.eventually("unknown to be visible", 10*time.Second, func() bool { st, _ := s.state(tenantA, "rita", id); return st == "unknown" })
		_, body := s.state(tenantA, "rita", id)
		if d := body["resolution"].(map[string]any)["detail"]; d == nil || d == "" {
			t.Fatal("the uncertainty carries an explanation")
		}
		if s.erp.TotalEffects() != 0 {
			t.Fatal("nothing may be written while the ERP is down")
		}
		s.erp.SetApplyDown(false)
		s.erp.SetLookupDown(false)
		s.eventually("reconciliation", 15*time.Second, func() bool { st, _ := s.state(tenantA, "rita", id); return st == "resolved" })
		if e, _ := s.erp.Effect(tenantA, "inv-adj-"+id); e.Applied != 1 {
			t.Fatalf("one effect after recovery: %+v", e)
		}
	})

	t.Run("a rejection fails the exception with the ERP's reason", func(t *testing.T) {
		s := newSystem(t)
		id := s.createException(tenantA, "rita", "WH1/UNKNOWN-ITEM")
		s.erp.RejectNext("unknown item")
		if code, _ := s.resolve(tenantA, "rita", id, "key-reject-0001", 1, 7); code != 202 {
			t.Fatal(code)
		}
		if _, err := s.worker(tenantA, "svc-worker-a"); err != nil {
			t.Fatal(err)
		}
		s.eventually("failed", 10*time.Second, func() bool { st, _ := s.state(tenantA, "rita", id); return st == "failed" })
		_, body := s.state(tenantA, "rita", id)
		if body["resolution"].(map[string]any)["detail"] != "unknown item" {
			t.Fatalf("%v", body["resolution"])
		}
		if code, _ := s.resolve(tenantA, "rita", id, "key-reject-0002", 3, 7); code != 409 {
			t.Fatalf("a failed exception cannot be resolved again: %d", code)
		}
	})
}
