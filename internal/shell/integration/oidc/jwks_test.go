package oidc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
)

func TestJWKSKeyRotationRefetchesOncePerUnknownKid(t *testing.T) {
	first := primaryRSAKey(t)
	second := secondaryRSAKey(t)
	now := testNow()

	transport := &fakeJWKS{handler: staticJSON(jwksJSON(t, rsaJWK("key-1", first)))}
	config := defaultTestConfig()
	config.httpClient = &http.Client{Transport: transport}
	verifier := newVerifier(t, config)

	firstToken := makeJWT(t, "RS256", "key-1", testClaimsMap(now), rsaSHA256Signer(first))
	if _, err := verifyRequest(verifier, firstToken, now); err != nil {
		t.Fatalf("Verify first key: %v", err)
	}
	if got := transport.count(); got != 1 {
		t.Fatalf("JWKS fetches after first verification = %d; want 1", got)
	}

	// Rotate the provider: the next unknown kid must trigger exactly one
	// refetch and then succeed.
	transport.setHandler(staticJSON(jwksJSON(t, rsaJWK("key-1", first), rsaJWK("key-2", second))))
	secondToken := makeJWT(t, "RS256", "key-2", testClaimsMap(now), rsaSHA256Signer(second))
	if _, err := verifyRequest(verifier, secondToken, now); err != nil {
		t.Fatalf("Verify rotated key: %v", err)
	}
	if got := transport.count(); got != 2 {
		t.Fatalf("JWKS fetches after rotation = %d; want 2", got)
	}
	if _, err := verifyRequest(verifier, secondToken, now); err != nil {
		t.Fatalf("Verify cached rotated key: %v", err)
	}
	if got := transport.count(); got != 2 {
		t.Fatalf("cached rotated key refetched: fetches = %d; want 2", got)
	}

	unknownToken := makeJWT(t, "RS256", "key-3", testClaimsMap(now), rsaSHA256Signer(first))
	if _, err := verifyRequest(verifier, unknownToken, now); err == nil {
		t.Fatal("unknown kid was accepted")
	} else {
		requireFaultClass(t, err, fault.Unauthenticated)
	}
	if got := transport.count(); got != 3 {
		t.Fatalf("unknown kid fetches = %d; want exactly one refetch (3)", got)
	}

	// An unknown kid is not cached as a negative result: a later verification
	// may refetch once, but never loops within one Verify.
	if _, err := verifyRequest(verifier, unknownToken, now); err == nil {
		t.Fatal("unknown kid was accepted on retry")
	} else {
		requireFaultClass(t, err, fault.Unauthenticated)
	}
	if got := transport.count(); got != 4 {
		t.Fatalf("second unknown kid fetches = %d; want one more refetch (4)", got)
	}
}

func TestJWKSCacheHonorsTTLAndClock(t *testing.T) {
	key := primaryRSAKey(t)
	now := testNow()
	clock := newFakeClock(now)
	transport := &fakeJWKS{handler: staticJSON(jwksJSON(t, rsaJWK("key-1", key)))}
	config := defaultTestConfig()
	config.httpClient = &http.Client{Transport: transport}
	config.clock = clock.Now
	config.cacheTTL = time.Hour
	verifier := newVerifier(t, config)

	claims := testClaimsMap(now)
	claims["exp"] = now.Add(24 * time.Hour).Unix()
	token := makeJWT(t, "RS256", "key-1", claims, rsaSHA256Signer(key))

	if _, err := verifyRequest(verifier, token, now); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got := transport.count(); got != 1 {
		t.Fatalf("initial fetches = %d; want 1", got)
	}

	clock.Advance(59 * time.Minute)
	if _, err := verifyRequest(verifier, token, now); err != nil {
		t.Fatalf("Verify inside cache TTL: %v", err)
	}
	if got := transport.count(); got != 1 {
		t.Fatalf("fetches inside TTL = %d; want 1", got)
	}

	clock.Advance(time.Minute) // exactly fetchedAt+TTL
	if _, err := verifyRequest(verifier, token, now); err != nil {
		t.Fatalf("Verify at cache TTL boundary: %v", err)
	}
	if got := transport.count(); got != 2 {
		t.Fatalf("fetches at TTL boundary = %d; want 2", got)
	}

	clock.Advance(59 * time.Minute)
	if _, err := verifyRequest(verifier, token, now); err != nil {
		t.Fatalf("Verify inside refreshed TTL: %v", err)
	}
	if got := transport.count(); got != 2 {
		t.Fatalf("fetches inside refreshed TTL = %d; want 2", got)
	}
}

func TestJWKSProviderFailuresAreDependencyUnavailable(t *testing.T) {
	key := primaryRSAKey(t)
	now := testNow()
	token := makeJWT(t, "RS256", "key-1", testClaimsMap(now), rsaSHA256Signer(key))

	cases := []struct {
		name   string
		config func(*testConfig)
	}{
		{"transport_error", func(config *testConfig) {
			config.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("dial tcp: provider unavailable")
			})}
		}},
		{"http_500", func(config *testConfig) {
			config.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return jsonResponse("provider error", http.StatusInternalServerError), nil
			})}
		}},
		{"malformed_json", func(config *testConfig) {
			config.httpClient = &http.Client{Transport: roundTripFunc(staticJSON("not-json"))}
		}},
		{"oversized_body", func(config *testConfig) {
			config.maxJWKSBytes = 64
			body := strings.Repeat(" ", 65) + `{"keys":[]}`
			config.httpClient = &http.Client{Transport: roundTripFunc(staticJSON(body))}
		}},
		{"deadline", func(config *testConfig) {
			config.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, context.DeadlineExceeded
			})}
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := defaultTestConfig()
			testCase.config(&config)
			verifier := newVerifier(t, config)
			principal, err := verifyRequest(verifier, token, now)
			requireFaultClass(t, err, fault.DependencyUnavailable)
			if principal.ID != "" {
				t.Fatalf("provider failure returned a principal: %+v", principal)
			}
		})
	}

	t.Run("cancellation", func(t *testing.T) {
		config := defaultTestConfig()
		config.httpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, context.Canceled
		})}
		verifier := newVerifier(t, config)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := verifyRequestContext(ctx, verifier, token, now)
		requireFaultClass(t, err, fault.DependencyUnavailable)
	})

	t.Run("unknown_kid_refetch_failure", func(t *testing.T) {
		transport := &fakeJWKS{handler: staticJSON(jwksJSON(t, rsaJWK("key-1", key)))}
		config := defaultTestConfig()
		config.httpClient = &http.Client{Transport: transport}
		verifier := newVerifier(t, config)
		known := makeJWT(t, "RS256", "key-1", testClaimsMap(now), rsaSHA256Signer(key))
		if _, err := verifyRequest(verifier, known, now); err != nil {
			t.Fatalf("Verify known key: %v", err)
		}
		transport.setHandler(func(*http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("rotation fetch failed")
		})
		unknown := makeJWT(t, "RS256", "key-2", testClaimsMap(now), rsaSHA256Signer(key))
		_, err := verifyRequest(verifier, unknown, now)
		requireFaultClass(t, err, fault.DependencyUnavailable)
		if got := transport.count(); got != 2 {
			t.Fatalf("rotation fetches = %d; want 2", got)
		}
	})
}

func TestConcurrentVerificationsShareCachedJWKS(t *testing.T) {
	key := primaryRSAKey(t)
	now := testNow()
	transport := &fakeJWKS{handler: staticJSON(jwksJSON(t, rsaJWK("key-1", key)))}
	config := defaultTestConfig()
	config.httpClient = &http.Client{Transport: transport}
	verifier := newVerifier(t, config)
	token := makeJWT(t, "RS256", "key-1", testClaimsMap(now), rsaSHA256Signer(key))

	if _, err := verifyRequest(verifier, token, now); err != nil {
		t.Fatalf("warm cache: %v", err)
	}
	if got := transport.count(); got != 1 {
		t.Fatalf("warm fetches = %d; want 1", got)
	}

	const workers = 32
	start := make(chan struct{})
	errors := make(chan error, workers)
	var wait sync.WaitGroup
	for range workers {
		wait.Go(func() {
			<-start
			if _, err := verifyRequest(verifier, token, now); err != nil {
				errors <- err
			}
		})
	}
	close(start)
	wait.Wait()
	close(errors)
	for err := range errors {
		t.Errorf("concurrent Verify: %v", err)
	}
	if got := transport.count(); got != 1 {
		t.Fatalf("concurrent verifications fetched JWKS %d times; want the single warm fetch", got)
	}
}

func TestJWKSCooldownSuppressesRepeatedForcedRefreshes(t *testing.T) {
	first := primaryRSAKey(t)
	second := secondaryRSAKey(t)
	now := testNow()
	clock := newFakeClock(now)

	transport := &fakeJWKS{handler: staticJSON(jwksJSON(t, rsaJWK("key-1", first)))}
	config := defaultTestConfig()
	config.httpClient = &http.Client{Transport: transport}
	config.clock = clock.Now
	config.cacheTTL = 10 * time.Minute
	config.minRefreshInterval = 10 * time.Second
	verifier := newVerifier(t, config)

	claims := testClaimsMap(now)
	claims["exp"] = now.Add(24 * time.Hour).Unix()

	// Cold startup fetch (force=false) must NOT set lastForcedAt, so a real key
	// rotation 1s after startup still fetches immediately.
	firstToken := makeJWT(t, "RS256", "key-1", claims, rsaSHA256Signer(first))
	if _, err := verifyRequest(verifier, firstToken, clock.Now()); err != nil {
		t.Fatalf("cold Verify: %v", err)
	}
	if got := transport.count(); got != 1 {
		t.Fatalf("fetches after cold Verify = %d; want 1", got)
	}

	// A transient provider error on a forced refresh must NOT set lastForcedAt,
	// so an immediate retry still hits the network.
	clock.Advance(time.Second)
	transport.setHandler(func(*http.Request) (*http.Response, error) {
		return jsonResponse("unavailable", http.StatusServiceUnavailable), nil
	})
	rotatedToken := makeJWT(t, "RS256", "key-2", claims, rsaSHA256Signer(second))
	_, err := verifyRequest(verifier, rotatedToken, clock.Now())
	requireFaultClass(t, err, fault.DependencyUnavailable)
	if got := transport.count(); got != 2 {
		t.Fatalf("fetches after failed forced refresh = %d; want 2", got)
	}

	// Immediate retry after transient 503 fetches right away and succeeds,
	// setting lastForcedAt.
	transport.setHandler(staticJSON(jwksJSON(t, rsaJWK("key-1", first), rsaJWK("key-2", second))))
	if _, err := verifyRequest(verifier, rotatedToken, clock.Now()); err != nil {
		t.Fatalf("Verify rotated key after 503 recovery: %v", err)
	}
	if got := transport.count(); got != 3 {
		t.Fatalf("fetches after successful forced refresh = %d; want 3", got)
	}

	// Unknown kid spam inside the 10s cooldown window returns Unauthenticated
	// from cached keys without hitting the network.
	clock.Advance(5 * time.Second)
	unknownToken := makeJWT(t, "RS256", "key-unknown", claims, rsaSHA256Signer(first))
	for range 5 {
		_, err := verifyRequest(verifier, unknownToken, clock.Now())
		requireFaultClass(t, err, fault.Unauthenticated)
	}
	if got := transport.count(); got != 3 {
		t.Fatalf("fetches during cooldown = %d; want 3 (no network calls)", got)
	}

	// Once MinRefreshInterval (10s) has elapsed since lastForcedAt, one new
	// forced refresh is permitted.
	clock.Advance(5 * time.Second)
	_, err = verifyRequest(verifier, unknownToken, clock.Now())
	requireFaultClass(t, err, fault.Unauthenticated)
	if got := transport.count(); got != 4 {
		t.Fatalf("fetches after cooldown expiry = %d; want 4", got)
	}
}

func TestJWKSConcurrentForcedRefreshesCoalesceAndRespectCallerContext(t *testing.T) {
	first := primaryRSAKey(t)
	second := secondaryRSAKey(t)
	now := testNow()

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	transport := &fakeJWKS{handler: staticJSON(jwksJSON(t, rsaJWK("key-1", first)))}
	config := defaultTestConfig()
	config.httpClient = &http.Client{Transport: transport}
	config.minRefreshInterval = 10 * time.Second
	verifier := newVerifier(t, config)

	firstToken := makeJWT(t, "RS256", "key-1", testClaimsMap(now), rsaSHA256Signer(first))
	if _, err := verifyRequest(verifier, firstToken, now); err != nil {
		t.Fatalf("warm cache: %v", err)
	}
	if got := transport.count(); got != 1 {
		t.Fatalf("warm fetches = %d; want 1", got)
	}

	rotatedBody := jwksJSON(t, rsaJWK("key-1", first), rsaJWK("key-2", second))
	transport.setHandler(func(req *http.Request) (*http.Response, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
			return jsonResponse(rotatedBody, http.StatusOK), nil
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	})

	rotatedToken := makeJWT(t, "RS256", "key-2", testClaimsMap(now), rsaSHA256Signer(second))
	concrete := verifier.(*Verifier)

	// Start one caller that will cancel mid-flight and 16 callers with live
	// contexts: wait until every caller has joined the single in-flight fetch,
	// then cancel the one caller and verify it returns DependencyUnavailable
	// promptly without aborting the shared HTTP fetch for the remaining callers.
	cancelCtx, cancel := context.WithCancel(context.Background())
	canceledErr := make(chan error, 1)
	go func() {
		_, err := verifyRequestContext(cancelCtx, verifier, rotatedToken, now)
		canceledErr <- err
	}()

	const liveWorkers = 16
	liveErrs := make(chan error, liveWorkers)
	var wait sync.WaitGroup
	for range liveWorkers {
		wait.Go(func() {
			_, err := verifyRequest(verifier, rotatedToken, now)
			liveErrs <- err
		})
	}
	<-entered
	for {
		concrete.mu.Lock()
		joined := concrete.inflight != nil && concrete.inflight.waiters == 1+liveWorkers
		concrete.mu.Unlock()
		if joined {
			break
		}
		runtime.Gosched()
	}

	cancel()
	requireFaultClass(t, <-canceledErr, fault.DependencyUnavailable)

	close(release)
	wait.Wait()
	close(liveErrs)
	for err := range liveErrs {
		if err != nil {
			t.Errorf("live concurrent Verify failed: %v", err)
		}
	}
	if got := transport.count(); got != 2 {
		t.Fatalf("total JWKS fetches = %d; want 2 (1 warm + 1 coalesced rotation)", got)
	}

	// Also prove that when the last waiting caller cancels its context, the
	// underlying HTTP request context is canceled rather than leaked.
	soloEntered := make(chan struct{}, 1)
	soloAborted := make(chan struct{})
	soloConfig := defaultTestConfig()
	soloConfig.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		select {
		case soloEntered <- struct{}{}:
		default:
		}
		<-req.Context().Done()
		close(soloAborted)
		return nil, req.Context().Err()
	})}
	soloVerifier := newVerifier(t, soloConfig)
	soloCtx, soloCancel := context.WithCancel(context.Background())
	soloErr := make(chan error, 1)
	go func() {
		_, err := verifyRequestContext(soloCtx, soloVerifier, firstToken, now)
		soloErr <- err
	}()
	<-soloEntered
	soloCancel()
	requireFaultClass(t, <-soloErr, fault.DependencyUnavailable)
	<-soloAborted
}
