package app

import (
	"context"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/httpapi"
)

// maxLimiterKeys bounds the limiter's memory: past it, buckets that have been
// full for a minute are dropped, and if none qualify the oldest is refused
// service rather than the table growing without bound.
const maxLimiterKeys = 100_000

// rateLimiter is a per-principal, per-scope, per-action token bucket kept in
// process memory (the single-replica profile). It refills perMinute tokens per
// minute up to a burst of the same size. It runs after authentication, so an
// unauthenticated caller spends no budget, and it fails closed: a table that
// cannot admit another key refuses the request.
type rateLimiter struct {
	perMinute float64
	now       func() time.Time

	mu      sync.Mutex
	buckets map[httpapi.RateKey]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

var _ httpapi.RateLimiter = (*rateLimiter)(nil)

func newRateLimiter(perMinute int, now func() time.Time) *rateLimiter {
	if now == nil {
		now = time.Now
	}
	return &rateLimiter{perMinute: float64(perMinute), now: now, buckets: map[httpapi.RateKey]*bucket{}}
}

func (l *rateLimiter) Allow(_ context.Context, key httpapi.RateKey) (httpapi.RateDecision, error) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= maxLimiterKeys {
			l.sweep(now)
			if len(l.buckets) >= maxLimiterKeys {
				return httpapi.RateDecision{Allowed: false, RetryAfter: time.Minute}, nil
			}
		}
		b = &bucket{tokens: l.perMinute, last: now}
		l.buckets[key] = b
	}
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens += elapsed.Minutes() * l.perMinute
		if b.tokens > l.perMinute {
			b.tokens = l.perMinute
		}
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return httpapi.RateDecision{Allowed: true}, nil
	}
	wait := time.Duration((1 - b.tokens) / l.perMinute * float64(time.Minute))
	return httpapi.RateDecision{Allowed: false, RetryAfter: wait}, nil
}

func (l *rateLimiter) sweep(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.last) >= time.Minute {
			delete(l.buckets, k)
		}
	}
}
