package integration_test

import (
	"errors"
	"math/rand"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/gateway"
	"github.com/optimiware/kodama/internal/gateway/gatewaytest"
	"github.com/optimiware/kodama/internal/gateway/openai"
)

const (
	allowance = 100
	maxInput  = 200
	maxOutput = 64
)

func tightLimits(ed *gateway.SettingsEdit) {
	ed.Limits = gateway.Limits{MaxRequests: 50, TokenAllowance: allowance, MaxRequestBytes: 2048, MaxInputTokens: maxInput, MaxOutputTokens: maxOutput}
}

func pad(n int) string { return strings.Repeat("a", n) }

// inputBound computes the gateway's conservative bound of a request body.
func inputBound(t *testing.T, e *gatewaytest.Env, raw []byte) int64 {
	t.Helper()
	c, err := openai.NewDecoder().Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	b, err := gatewaytest.Estimator(t).InputBound(c)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Test_bounded_token_overshoot: GM-03, GM-04, GM-A02. Input and output bounds
// are enforced before provider traffic, observed usage is charged exactly,
// missing or invalid usage is charged the request bound, admission stops once
// the charge reaches the allowance, and the charge never exceeds the published
// bound allowance - 1 + (maxInput + maxOutput).
func Test_bounded_token_overshoot(t *testing.T) {
	t.Run("size, input and output bounds refuse before any provider traffic", func(t *testing.T) {
		e := newWorld(t, tightLimits)
		a := e.NewAttempt(scopeA)
		cases := map[string][]byte{
			"input bound":  gatewaytest.Body("gpt-a", pad(400), 32),
			"output bound": gatewaytest.Body("gpt-a", "hi", maxOutput+1),
			"body size":    gatewaytest.Body("gpt-a", pad(3000), 32),
		}
		for name, raw := range cases {
			if _, err := e.Invoke(e.Replica, a, raw, &gatewaytest.Sink{}); !errors.Is(err, gateway.ErrLimitExceeded) {
				t.Errorf("%s: %v, want ErrLimitExceeded", name, err)
			}
		}
		if n := len(e.Provider.Calls()); n != 0 {
			t.Fatalf("provider calls %d", n)
		}
		// Nothing was consumed: the binding exists (first admission attempt) but no request did.
		if n := e.Count(`SELECT count(*) FROM model_requests WHERE run_id = $1`, string(a.RunID)); n != 0 {
			t.Fatalf("%d requests were consumed by refusals", n)
		}
		if _, err := e.Invoke(e.Replica, a, gatewaytest.Body("gpt-a", "hi", maxOutput), &gatewaytest.Sink{}); err != nil {
			t.Fatalf("a request at the output bound: %v", err)
		}
	})

	t.Run("observed usage is charged and admission stops at the allowance", func(t *testing.T) {
		e := newWorld(t, tightLimits)
		a := e.NewAttempt(scopeA)
		raw := gatewaytest.Body("gpt-a", pad(100), 64)
		e.Provider.Script(gatewaytest.OK("gpt-a-2026", 50, 10, "x"), gatewaytest.OK("gpt-a-2026", 50, 10, "x"), gatewaytest.OK("gpt-a-2026", 50, 10, "x"))
		for i := 0; i < 2; i++ {
			res, err := e.Invoke(e.Replica, a, raw, &gatewaytest.Sink{})
			if err != nil || res.Usage.Charged != 60 || res.Usage.Basis != gateway.BasisObserved || res.Usage.Uncertainty != gateway.UncertaintyNone {
				t.Fatalf("request %d: %v %+v", i+1, err, res.Usage)
			}
		}
		// 120 >= 100: the allowance is used; no third request, no provider traffic.
		if _, err := e.Invoke(e.Replica, a, raw, &gatewaytest.Sink{}); !errors.Is(err, gateway.ErrLimitExceeded) {
			t.Fatalf("after the allowance: %v", err)
		}
		adm, charged := e.Budget(a.RunID)
		if adm != 2 || charged != 120 || len(e.Provider.Calls()) != 2 {
			t.Fatalf("admitted=%d charged=%d calls=%d", adm, charged, len(e.Provider.Calls()))
		}
	})

	t.Run("a request is admitted while charged tokens are below the allowance", func(t *testing.T) {
		e := newWorld(t, tightLimits)
		a := e.NewAttempt(scopeA)
		raw := gatewaytest.Body("gpt-a", pad(100), 64)
		e.Provider.Script(gatewaytest.OK("gpt-a-2026", 80, 19, "x"), gatewaytest.OK("gpt-a-2026", 3, 2, "x"))
		if _, err := e.Invoke(e.Replica, a, raw, &gatewaytest.Sink{}); err != nil {
			t.Fatal(err)
		}
		// charged = 99 < 100: one more request is admitted, and it overshoots.
		if _, err := e.Invoke(e.Replica, a, raw, &gatewaytest.Sink{}); err != nil {
			t.Fatalf("admission at 99/100: %v", err)
		}
		if _, err := e.Invoke(e.Replica, a, raw, &gatewaytest.Sink{}); !errors.Is(err, gateway.ErrLimitExceeded) {
			t.Fatalf("admission at 104/100: %v", err)
		}
	})

	t.Run("missing and invalid usage are charged the request bound", func(t *testing.T) {
		for name, behavior := range map[string]gatewaytest.Behavior{
			"missing": gatewaytest.NoUsage("gpt-a-2026", "x"), "invalid": gatewaytest.InvalidUsage("gpt-a-2026"),
		} {
			t.Run(name, func(t *testing.T) {
				e := newWorld(t, tightLimits)
				a := e.NewAttempt(scopeA)
				raw := gatewaytest.Body("gpt-a", pad(100), 64)
				want := inputBound(t, e, raw) + 64
				e.Provider.Script(behavior)
				sink := &gatewaytest.Sink{}
				res, err := e.Invoke(e.Replica, a, raw, sink)
				if err != nil {
					t.Fatal(err)
				}
				u := res.Usage
				wantCode := map[string]string{"missing": gateway.UncertaintyUsageMissing, "invalid": gateway.UncertaintyUsageInvalid}[name]
				if u.Charged != want || u.Basis != gateway.BasisEstimated || u.Uncertainty != wantCode || u.InputTokens != nil {
					t.Fatalf("usage %+v, want charge %d uncertainty %s", u, want, wantCode)
				}
				if _, fin, _ := sink.Snapshot(); !fin {
					t.Fatal("a complete stream with unusable usage is still delivered completely")
				}
				_, charged := e.Budget(a.RunID)
				if charged != want {
					t.Fatalf("run charged %d, want %d", charged, want)
				}
				// The bound exceeds the allowance, so nothing more is admitted.
				if _, err := e.Invoke(e.Replica, a, raw, &gatewaytest.Sink{}); !errors.Is(err, gateway.ErrLimitExceeded) {
					t.Fatalf("after an uncertain charge of %d: %v", want, err)
				}
			})
		}
	})

	t.Run("usage above the verified bounds is a protocol failure charged at least the bound", func(t *testing.T) {
		e := newWorld(t, tightLimits)
		a := e.NewAttempt(scopeA)
		raw := gatewaytest.Body("gpt-a", "hi", 64)
		e.Provider.Script(gatewaytest.OK("gpt-a-2026", 100000, 5, "x"))
		sink := &gatewaytest.Sink{}
		res, err := e.Invoke(e.Replica, a, raw, sink)
		if !errors.Is(err, gateway.ErrProvider) {
			t.Fatalf("err %v", err)
		}
		_, fin, failed := sink.Snapshot()
		if fin || failed != "bound_exceeded" {
			t.Fatalf("finished=%v failCode=%q: an out-of-bounds stream must not look successful", fin, failed)
		}
		if res.State != gateway.StateFailed || res.Usage.Charged != 100005 || res.Usage.Uncertainty != gateway.UncertaintyBoundExceeded {
			t.Fatalf("result %+v usage %+v", res, res.Usage)
		}
	})

	t.Run("the published overshoot bound holds under randomized usage and concurrent runs", func(t *testing.T) {
		e := newWorld(t, tightLimits)
		const runs = 3
		bmax := int64(maxInput + maxOutput)
		raw := gatewaytest.Body("gpt-a", pad(150), 64)
		bound := inputBound(t, e, raw)
		var mu sync.Mutex
		rng := rand.New(rand.NewSource(7))
		e.Provider.Default(func(w httpWriter, r *httpRequest) {
			mu.Lock()
			missing := rng.Intn(4) == 0
			in, out := rng.Int63n(bound+1), rng.Int63n(maxOutput+1)
			mu.Unlock()
			if missing {
				gatewaytest.NoUsage("gpt-a-2026", "x")(w, r)
				return
			}
			gatewaytest.OK("gpt-a-2026", in, out, "x")(w, r)
		})
		var wg sync.WaitGroup
		var ids []string
		for r := 0; r < runs; r++ {
			att := e.NewAttempt(scopeA)
			ids = append(ids, string(att.RunID))
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 80; i++ {
					_, err := e.Invoke(e.Replica, att, raw, &gatewaytest.Sink{})
					if errors.Is(err, gateway.ErrLimitExceeded) {
						return
					}
					if err != nil {
						t.Errorf("run %d request %d: %v", r, i, err)
						return
					}
				}
				t.Errorf("run %d was never stopped", r)
			}()
		}
		wg.Wait()
		for _, run := range ids {
			var charged, sum int64
			e.Scalar(&charged, `SELECT tokens_charged FROM model_run_bindings WHERE run_id = $1`, run)
			e.Scalar(&sum, `SELECT coalesce(sum(charged_tokens), 0) FROM model_usage WHERE run_id = $1`, run)
			if charged != sum {
				t.Errorf("run charged %d but its usage records sum to %d", charged, sum)
			}
			if charged < allowance || charged > allowance-1+bmax {
				t.Errorf("run charged %d, outside [%d, %d]", charged, allowance, allowance-1+bmax)
			}
		}
	})
}
