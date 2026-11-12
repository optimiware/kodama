package parallel

import (
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func environment(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestGateLimit(t *testing.T) {
	gate := &Gate{Env: "TEST_LIMIT", Default: 4, Max: 8, HostPoolEnv: "TEST_POOL"}
	for _, valid := range []struct {
		name   string
		values map[string]string
		want   int
	}{
		{"default", nil, 4},
		{"explicit", map[string]string{"TEST_LIMIT": "6"}, 6},
		{"sequential", map[string]string{"TEST_LIMIT": "1"}, 1},
		{"zero is sequential", map[string]string{"TEST_LIMIT": "0"}, 0},
		{"maximum", map[string]string{"TEST_LIMIT": " 8 "}, 8},
		{"a smaller host pool lowers the default", map[string]string{"TEST_POOL": "2"}, 2},
		{"a larger host pool keeps the default", map[string]string{"TEST_POOL": "6"}, 4},
		{"a malformed host pool keeps the default", map[string]string{"TEST_POOL": "many"}, 4},
		{"an explicit limit wins over the host pool", map[string]string{"TEST_LIMIT": "6", "TEST_POOL": "1"}, 6},
	} {
		got, err := gate.Limit(environment(valid.values))
		if err != nil || got != valid.want {
			t.Errorf("%s: Limit = (%d, %v); want (%d, nil)", valid.name, got, err, valid.want)
		}
	}
	for _, invalid := range []string{"-1", "9", "many", "2.5"} {
		if _, err := gate.Limit(environment(map[string]string{"TEST_LIMIT": invalid})); err == nil {
			t.Errorf("Limit accepted %s=%q", gate.Env, invalid)
		}
	}
}

// TestGateBoundsConcurrency proves the gate admits several tests at once but
// never more than its limit.
func TestGateBoundsConcurrency(t *testing.T) {
	gate := &Gate{Env: "KODAMA_TEST_UNSET_PARALLEL_LIMIT", Default: 2, Max: 4}
	var active, peak atomic.Int32
	t.Run("group", func(t *testing.T) {
		for range 6 {
			t.Run("member", func(t *testing.T) {
				gate.Enter(t)
				now := active.Add(1)
				for {
					seen := peak.Load()
					if now <= seen || peak.CompareAndSwap(seen, now) {
						break
					}
				}
				time.Sleep(20 * time.Millisecond)
				active.Add(-1)
			})
		}
	})
	got := peak.Load()
	if got > 2 {
		t.Fatalf("peak concurrency = %d; want at most the limit 2", got)
	}
	if got < 2 && runtime.GOMAXPROCS(0) > 1 {
		t.Fatalf("peak concurrency = %d; want the gate to admit 2 tests at once", got)
	}
}

// TestGateSequentialLimitNeverParallelizes proves a limit of 1 leaves a test
// sequential: t.Setenv panics in a parallel test.
func TestGateSequentialLimitNeverParallelizes(t *testing.T) {
	gate := &Gate{Env: "KODAMA_TEST_UNSET_PARALLEL_LIMIT", Default: 1, Max: 4}
	gate.Parallel(t)
	gate.Enter(t)
	t.Setenv("KODAMA_TEST_SEQUENTIAL_PROBE", "1")
}

func TestGateRejectsAMalformedLimit(t *testing.T) {
	t.Setenv("KODAMA_TEST_MALFORMED_LIMIT", "many")
	gate := &Gate{Env: "KODAMA_TEST_MALFORMED_LIMIT", Default: 2, Max: 4}
	if err := gate.load(); err == nil {
		t.Fatal("a malformed limit was accepted")
	}
}

// TestKeyedLockSerializesOneKey proves holders of one key never overlap while
// a test takes a second key during another test's hold. The distinct-key half
// nests the holders instead of running them as parallel siblings, so it does
// not depend on how many parallel tests the binary may run (-parallel).
func TestKeyedLockSerializesOneKey(t *testing.T) {
	var lock KeyedLock
	var sameKey atomic.Int32
	t.Run("one key", func(t *testing.T) {
		for range 4 {
			t.Run("holder", func(t *testing.T) {
				t.Parallel()
				lock.Hold(t, "repository\x00refs/heads/main")
				if now := sameKey.Add(1); now != 1 {
					t.Errorf("%d tests hold the same key; want 1", now)
				}
				time.Sleep(10 * time.Millisecond)
				sameKey.Add(-1)
			})
		}
	})

	t.Run("distinct keys", func(t *testing.T) {
		lock.Hold(t, "alpha")
		t.Run("second holder", func(t *testing.T) {
			held := make(chan struct{})
			go func() {
				lock.Hold(t, "beta")
				close(held)
			}()
			select {
			case <-held:
			case <-time.After(5 * time.Second):
				t.Fatal("a holder of another key waited for the first key's holder")
			}
		})
	})
}
