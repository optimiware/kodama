package app

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/httpapi"
)

func TestRateLimiter(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	l := newRateLimiter(2, clock)
	key := func(p string) httpapi.RateKey {
		return httpapi.RateKey{Principal: httpapi.Principal{ID: p}, TenantID: "t_a", ProjectID: "p_a", Action: "work_item.list"}
	}
	allow := func(k httpapi.RateKey) httpapi.RateDecision {
		d, err := l.Allow(context.Background(), k)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	if !allow(key("u1")).Allowed || !allow(key("u1")).Allowed {
		t.Fatal("the burst equals the per-minute budget")
	}
	d := allow(key("u1"))
	if d.Allowed || d.RetryAfter <= 0 || d.RetryAfter > time.Minute {
		t.Fatalf("a spent budget must refuse with a retry delay: %+v", d)
	}
	if !allow(key("u2")).Allowed {
		t.Fatal("budgets are per principal")
	}
	other := key("u1")
	other.Action = "work_item.read"
	if !allow(other).Allowed {
		t.Fatal("budgets are per action")
	}
	now = now.Add(30 * time.Second) // one token refills (2 per minute)
	if !allow(key("u1")).Allowed {
		t.Fatal("tokens refill with time")
	}
	if allow(key("u1")).Allowed {
		t.Fatal("only the refilled token was available")
	}
	now = now.Add(time.Hour)
	if !allow(key("u1")).Allowed || !allow(key("u1")).Allowed || allow(key("u1")).Allowed {
		t.Fatal("the bucket never holds more than the burst")
	}
}

func TestRateLimiterFailsClosedWhenTheTableIsFull(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := newRateLimiter(10, func() time.Time { return now })
	for i := 0; i < maxLimiterKeys; i++ {
		l.buckets[httpapi.RateKey{Principal: httpapi.Principal{ID: "u"}, Action: httpapi.Action(fmt.Sprintf("a%d", i))}] = &bucket{tokens: 1, last: now}
	}
	d, err := l.Allow(context.Background(), httpapi.RateKey{Principal: httpapi.Principal{ID: "new"}, Action: "x"})
	if err != nil || d.Allowed {
		t.Fatalf("a full table must refuse a new key, not grow: %+v %v", d, err)
	}
	now = now.Add(2 * time.Minute) // idle buckets are swept and the key is admitted
	if d, _ := l.Allow(context.Background(), httpapi.RateKey{Principal: httpapi.Principal{ID: "new"}, Action: "x"}); !d.Allowed {
		t.Fatal("idle buckets must be reclaimed")
	}
}
