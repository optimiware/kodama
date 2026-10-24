package integration_test

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/gateway"
	"github.com/optimiware/kodama/internal/gateway/gatewaytest"
)

// Test_cross_replica_admission: GM-03, GM-A02. Two independently constructed
// replicas and several attempts of one run cannot hold two unresolved requests
// at once, cannot exceed the request cap, and a rolled-back admission consumes
// nothing.
func Test_cross_replica_admission(t *testing.T) {
	t.Run("one in-flight request per run across replicas and attempts", func(t *testing.T) {
		e := newWorld(t)
		r2 := e.NewReplica(gatewaytest.ReplicaOptions{})
		atts := e.NewRun(scopeA, 3)

		started := make(chan struct{}, 1)
		release := make(chan struct{})
		e.Provider.Script(gatewaytest.Gate(started, release, gatewaytest.OK("gpt-a-2026", 10, 3, "first")))

		first := make(chan error, 1)
		go func() {
			_, err := e.Invoke(e.Replica, atts[0], body("one"), &gatewaytest.Sink{})
			first <- err
		}()
		<-started // the first request is open at the provider

		for i, rep := range []*gatewaytest.Replica{r2, e.Replica} {
			res, err := e.Invoke(rep, atts[i+1], body("two"), &gatewaytest.Sink{})
			if !errors.Is(err, gateway.ErrBusy) || res.RequestID != "" {
				t.Fatalf("attempt %d on replica %d: %v, want ErrBusy", i+1, i, err)
			}
		}
		if n := len(e.Provider.Calls()); n != 1 {
			t.Fatalf("provider saw %d requests while one was unresolved", n)
		}
		if adm, _ := e.Budget(atts[0].RunID); adm != 1 {
			t.Fatalf("a refused admission consumed a request: admitted=%d", adm)
		}
		close(release)
		if err := <-first; err != nil {
			t.Fatalf("first request: %v", err)
		}
		// The slot is free again, for any replica and attempt of the run.
		if _, err := e.Invoke(r2, atts[2], body("three"), &gatewaytest.Sink{}); err != nil {
			t.Fatalf("after settlement: %v", err)
		}
	})

	t.Run("concurrent callers never exceed the cap or the single slot", func(t *testing.T) {
		const limitN = 3
		e := newWorld(t, func(ed *gateway.SettingsEdit) { ed.Limits.MaxRequests = limitN })
		replicas := []*gatewaytest.Replica{e.Replica, e.NewReplica(gatewaytest.ReplicaOptions{}), e.NewReplica(gatewaytest.ReplicaOptions{})}
		atts := e.NewRun(scopeA, 4)
		e.Provider.Default(func(w httpWriter, r *httpRequest) {
			time.Sleep(15 * time.Millisecond)
			gatewaytest.OK("gpt-a-2026", 10, 3, "x")(w, r)
		})

		var ok, limit atomic.Int64
		var wg sync.WaitGroup
		for g := 0; g < 16; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for try := 0; try < 200; try++ {
					_, err := e.Invoke(replicas[g%3], atts[g%4], body("c"), &gatewaytest.Sink{})
					switch {
					case err == nil:
						ok.Add(1)
						return
					case errors.Is(err, gateway.ErrBusy):
						time.Sleep(time.Duration(5+g) * time.Millisecond)
					case errors.Is(err, gateway.ErrLimitExceeded):
						limit.Add(1)
						return
					default:
						t.Errorf("unexpected: %v", err)
						return
					}
				}
				t.Errorf("caller %d never got an answer", g)
			}()
		}
		wg.Wait()
		adm, charged := e.Budget(atts[0].RunID)
		if ok.Load() != limitN || adm != limitN || charged != limitN*13 {
			t.Fatalf("ok=%d admitted=%d charged=%d, want %d/%d/%d", ok.Load(), adm, charged, limitN, limitN, limitN*13)
		}
		if int64(len(e.Provider.Calls())) != ok.Load() {
			t.Fatalf("provider calls %d != successes %d", len(e.Provider.Calls()), ok.Load())
		}
		if m := e.Provider.MaxConcurrent(); m != 1 {
			t.Fatalf("the provider saw %d concurrent requests of one run", m)
		}
		if limit.Load() != 16-limitN {
			t.Fatalf("refused by the cap: %d, want %d", limit.Load(), 16-limitN)
		}
		if got := e.Count(`SELECT count(*) FROM model_requests WHERE state IN ('admitted','forwarding','unknown')`); got != 0 {
			t.Fatalf("%d requests still hold the slot", got)
		}
	})

	t.Run("a rolled-back first admission consumes nothing", func(t *testing.T) {
		e := newWorld(t)
		failing := e.NewReplica(gatewaytest.ReplicaOptions{Pins: failingPins{}})
		a := e.NewAttempt(scopeA)

		_, err := e.Invoke(failing, a, body("x"), &gatewaytest.Sink{})
		if !errors.Is(err, gateway.ErrUnavailable) {
			t.Fatalf("admission with a failing pin registrar: %v", err)
		}
		if n := e.Count(`SELECT count(*) FROM model_run_bindings WHERE run_id = $1`, string(a.RunID)); n != 0 {
			t.Fatalf("a rolled-back admission left %d bindings", n)
		}
		if n := e.Count(`SELECT count(*) FROM model_requests WHERE run_id = $1`, string(a.RunID)); n != 0 {
			t.Fatalf("a rolled-back admission left %d requests", n)
		}
		if n := len(e.Provider.Calls()); n != 0 {
			t.Fatalf("provider calls: %d", n)
		}
		// A healthy replica then admits the run's first request normally.
		if _, err := e.Invoke(e.Replica, a, body("x"), &gatewaytest.Sink{}); err != nil {
			t.Fatal(err)
		}
		if adm, _ := e.Budget(a.RunID); adm != 1 {
			t.Fatalf("admitted=%d, want 1", adm)
		}
	})
}
