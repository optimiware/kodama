package app

// Readiness at the composition boundary: app.New installs the
// operational handler wrapper around the composed public handler, and the
// readiness checks are the production dependencies themselves.
//
// A KMS-backed provider caches the unwrapped key for its lifetime, so a
// readiness check written as a normal Wrap would keep answering 200 after KMS
// went away (the mutant reproduction: warm the cache, fail KMS, ordinary Wrap
// still succeeds). The readiness check therefore asks the configured KMS
// client to decrypt the configured blob directly, validates the data-key
// length and clears the plaintext. A successful decrypt (and only a success)
// is cached for a bounded time, so a burst of probes does not decrypt once per
// probe; an expired entry re-probes and a failure is never cached.
//
// The NATS check dials with the probe budget as ConnectTimeout because
// natsgo.Connect is not bounded by its context, and it closes the connection it
// opened. The PostgreSQL checks use Begin/Rollback on each of the four role
// pools, so a role whose credentials or grants broke is visible.
//
// Liveness never runs any of this: a dependency outage must not restart a
// healthy process.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	pgdriver "github.com/optimiware/kodama/internal/shell/app/drivers/postgres"
	"github.com/optimiware/kodama/internal/shell/integration/keys"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	storepostgres "github.com/optimiware/kodama/internal/shell/store/postgres"
	"github.com/optimiware/kodama/internal/shell/transport/httpapi"
)

// readinessProbePool is one role pool observable without a live server. Begin
// records the transaction and either fails or returns a connection whose
// Rollback is recorded, so a readiness probe that forgets either half is
// observable. block makes Begin wait for cancellation, modelling a saturated
// or unresponsive pool.
type readinessProbePool struct {
	mu sync.Mutex

	begins      int
	rollbacks   int
	beginErr    error
	rollbackErr error
	block       bool
}

func (pool *readinessProbePool) Begin(ctx context.Context) (storepostgres.Conn, error) {
	pool.mu.Lock()
	pool.begins++
	beginErr := pool.beginErr
	block := pool.block
	pool.mu.Unlock()
	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if beginErr != nil {
		return nil, beginErr
	}
	return &readinessProbeConn{pool: pool}, nil
}

func (pool *readinessProbePool) counts() (int, int) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return pool.begins, pool.rollbacks
}

func (pool *readinessProbePool) setBeginError(err error) {
	pool.mu.Lock()
	pool.beginErr = err
	pool.mu.Unlock()
}

func (pool *readinessProbePool) setRollbackError(err error) {
	pool.mu.Lock()
	pool.rollbackErr = err
	pool.mu.Unlock()
}

func (pool *readinessProbePool) setBlock(block bool) {
	pool.mu.Lock()
	pool.block = block
	pool.mu.Unlock()
}

type readinessProbeConn struct {
	pool *readinessProbePool
}

func (conn *readinessProbeConn) Exec(context.Context, string, ...any) (store.Result, error) {
	return store.Result{}, errors.New("readiness probe never executes a statement")
}

func (conn *readinessProbeConn) Query(context.Context, string, ...any) (store.Rows, error) {
	return nil, errors.New("readiness probe never queries")
}

func (conn *readinessProbeConn) Commit(context.Context) error {
	return errors.New("readiness probe never commits")
}

func (conn *readinessProbeConn) Rollback(context.Context) error {
	conn.pool.mu.Lock()
	conn.pool.rollbacks++
	err := conn.pool.rollbackErr
	conn.pool.mu.Unlock()
	return err
}

// readinessKMS is the configured cloud KMS client the readiness check calls
// directly. It returns one fresh buffer per call (retained so the test can see
// whether the readiness check cleared it) and can be told to fail.
type readinessKMS struct {
	mu     sync.Mutex
	calls  int
	err    error
	length int
	last   []byte
}

func (kms *readinessKMS) Decrypt(ctx context.Context, _ secret.KeyRef, _ []byte) ([]byte, error) {
	kms.mu.Lock()
	kms.calls++
	err := kms.err
	length := kms.length
	kms.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	if length == 0 {
		length = secret.DataKeyBytes
	}
	material := make([]byte, length)
	for index := range material {
		material[index] = 0x5a
	}
	kms.mu.Lock()
	kms.last = material
	kms.mu.Unlock()
	return material, nil
}

func (kms *readinessKMS) callCount() int {
	kms.mu.Lock()
	defer kms.mu.Unlock()
	return kms.calls
}

func (kms *readinessKMS) setError(err error) {
	kms.mu.Lock()
	kms.err = err
	kms.mu.Unlock()
}

func (kms *readinessKMS) setLength(length int) {
	kms.mu.Lock()
	kms.length = length
	kms.mu.Unlock()
}

func (kms *readinessKMS) lastPlaintext() []byte {
	kms.mu.Lock()
	defer kms.mu.Unlock()
	return append([]byte(nil), kms.last...)
}

// installReadinessPools replaces the production pool constructor for one test
// and returns the four pools in composition order (runtime, controller,
// publisher, auth).
func installReadinessPools(t *testing.T) [4]*readinessProbePool {
	t.Helper()
	pools := [4]*readinessProbePool{{}, {}, {}, {}}
	previous := newPostgresPool
	index := 0
	newPostgresPool = func(pgdriver.Config) (postgresPool, error) {
		if index >= len(pools) {
			return nil, errors.New("production composition constructed more than four role pools")
		}
		pool := pools[index]
		index++
		return pool, nil
	}
	t.Cleanup(func() { newPostgresPool = previous })
	return pools
}

type readinessFixture struct {
	pools  [4]*readinessProbePool
	kms    *readinessKMS
	server *fakeNATSServer
	app    *App
	config Config
}

func newReadinessFixture(t *testing.T) *readinessFixture {
	t.Helper()
	pools := installReadinessPools(t)
	kms := &readinessKMS{}
	server := &fakeNATSServer{}
	config := productionConfigWithServer(t, server).resolved()
	config.Production.KMS = kms
	app, err := New(context.Background(), config)
	if err != nil {
		t.Fatalf("New production composition: %v", err)
	}
	t.Cleanup(func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := app.Close(shutdown); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	markAppStarted(t, app)
	return &readinessFixture{pools: pools, kms: kms, server: server, app: app, config: config}
}

// markAppStarted makes the composed app report started without launching the
// background loops. Readiness and shutdown state are otherwise only reachable
// through a full Run/Serve; the state machine itself is the subject here.
func markAppStarted(t *testing.T, app *App) {
	t.Helper()
	app.mu.Lock()
	app.started = true
	app.mu.Unlock()
}

func readinessStatus(t *testing.T, handler http.Handler) (int, string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, httpapi.ReadinessPath, nil)
	handler.ServeHTTP(recorder, request)
	return recorder.Code, recorder.Body.String()
}

func livenessStatus(t *testing.T, handler http.Handler) (int, string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, httpapi.HealthPath, nil)
	handler.ServeHTTP(recorder, request)
	return recorder.Code, recorder.Body.String()
}

func TestReadinessChecksReachEveryDependencyAndHealthChecksNone(t *testing.T) {
	fixture := newReadinessFixture(t)
	handler := fixture.app.Handler()

	// Liveness must not touch any dependency.
	if code, body := livenessStatus(t, handler); code != http.StatusOK || body != httpapi.HealthBody {
		t.Fatalf("healthz = %d %q; want 200 %q", code, body, httpapi.HealthBody)
	}
	for index, pool := range fixture.pools {
		if begins, rollbacks := pool.counts(); begins != 0 || rollbacks != 0 {
			t.Fatalf("healthz used pool %d: begins=%d rollbacks=%d; want no dependency call", index, begins, rollbacks)
		}
	}
	if calls := fixture.kms.callCount(); calls != 0 {
		t.Fatalf("healthz called KMS %d times; want 0", calls)
	}
	if dials := fixture.server.dialCount(); dials != 1 {
		t.Fatalf("healthz opened %d NATS dials beyond startup; want only the startup provisioning dial", dials-1)
	}

	// Readiness reaches each of the four role pools once, dials and closes one
	// authenticated broker connection and decrypts the configured KMS blob.
	if code, body := readinessStatus(t, handler); code != http.StatusOK || body != httpapi.ReadinessBody {
		t.Fatalf("readyz = %d %q; want 200 %q", code, body, httpapi.ReadinessBody)
	}
	for index, pool := range fixture.pools {
		if begins, rollbacks := pool.counts(); begins != 1 || rollbacks != 1 {
			t.Fatalf("readyz pool %d: begins=%d rollbacks=%d; want one Begin and one Rollback", index, begins, rollbacks)
		}
	}
	if calls := fixture.kms.callCount(); calls != 1 {
		t.Fatalf("readyz KMS decrypts = %d; want exactly one configured decrypt", calls)
	}
	if dials := fixture.server.dialCount(); dials != 2 {
		t.Fatalf("NATS dials after readiness = %d; want the startup dial plus one readiness dial", dials)
	}
	if !fixture.server.allClientsClosed() {
		t.Fatal("the readiness NATS connection was not closed")
	}
	dials := fixture.server.dialConfigs()
	probe := dials[len(dials)-1]
	if probe.ConnectTimeout != readinessBudget {
		t.Fatalf("readiness NATS ConnectTimeout = %s; want the probe budget %s (natsgo.Connect ignores the context)", probe.ConnectTimeout, readinessBudget)
	}
	if probe.URL != fixture.config.Installation.NATSURL || probe.TLS != fixture.config.Production.NATSTLS {
		t.Fatalf("readiness NATS dial did not reuse the authenticated TLS configuration: %+v", probe)
	}

	// A successful KMS decrypt is cached for a bounded time: a second probe
	// inside the window reuses the success instead of decrypting again. The
	// bounded expiry (pinned separately with a fake clock) is what still
	// exposes a later outage.
	if code, body := readinessStatus(t, handler); code != http.StatusOK || body != httpapi.ReadinessBody {
		t.Fatalf("second readyz = %d %q; want 200 %q", code, body, httpapi.ReadinessBody)
	}
	if calls := fixture.kms.callCount(); calls != 1 {
		t.Fatalf("KMS decrypts after two probes = %d; want 1 (a successful decrypt is cached within the TTL)", calls)
	}
}

func TestReadinessFailsWhenEachDependencyFails(t *testing.T) {
	cases := []struct {
		name            string
		breakDependency func(*readinessFixture)
	}{
		{name: "runtime pool", breakDependency: func(f *readinessFixture) { f.pools[0].setBeginError(errors.New("runtime pool unavailable")) }},
		{name: "controller pool", breakDependency: func(f *readinessFixture) { f.pools[1].setBeginError(errors.New("controller pool unavailable")) }},
		{name: "publisher pool", breakDependency: func(f *readinessFixture) { f.pools[2].setBeginError(errors.New("publisher pool unavailable")) }},
		{name: "auth pool", breakDependency: func(f *readinessFixture) { f.pools[3].setBeginError(errors.New("auth pool unavailable")) }},
		{name: "rollback", breakDependency: func(f *readinessFixture) { f.pools[0].setRollbackError(errors.New("rollback failed")) }},
		{name: "NATS dial", breakDependency: func(f *readinessFixture) { f.server.dialErr = errors.New("broker unavailable") }},
		{name: "KMS decrypt", breakDependency: func(f *readinessFixture) { f.kms.setError(errors.New("KMS unavailable")) }},
		{name: "KMS wrong length", breakDependency: func(f *readinessFixture) { f.kms.setLength(secret.DataKeyBytes - 1) }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newReadinessFixture(t)
			testCase.breakDependency(fixture)
			code, body := readinessStatus(t, fixture.app.Handler())
			if code != http.StatusServiceUnavailable {
				t.Fatalf("readyz with %s failing = %d; want 503", testCase.name, code)
			}
			if body != httpapi.ReadinessUnavailableBody {
				t.Fatalf("readyz failure body = %q; want the fixed %q", body, httpapi.ReadinessUnavailableBody)
			}
		})
	}
}

func TestReadinessRecoversAfterDependencyRecovers(t *testing.T) {
	fixture := newReadinessFixture(t)
	fixture.kms.setError(errors.New("KMS unavailable"))
	if code, _ := readinessStatus(t, fixture.app.Handler()); code != http.StatusServiceUnavailable {
		t.Fatalf("readyz while KMS is failing = %d; want 503", code)
	}
	fixture.kms.setError(nil)
	if code, body := readinessStatus(t, fixture.app.Handler()); code != http.StatusOK || body != httpapi.ReadinessBody {
		t.Fatalf("readyz after KMS recovery = %d %q; want 200 %q", code, body, httpapi.ReadinessBody)
	}
}

func TestReadinessKMSCheckBypassesProviderCacheAndClearsPlaintext(t *testing.T) {
	pools := installReadinessPools(t)
	_ = pools
	kms := &readinessKMS{}
	server := &fakeNATSServer{}
	config := productionConfigWithServer(t, server).resolved()
	config.Production.KMS = kms

	builder := &builder{cfg: config, logger: config.logger()}
	environment, err := buildEnvironment(context.Background(), builder)
	if err != nil {
		t.Fatalf("buildEnvironment: %v", err)
	}
	t.Cleanup(func() { builder.cleanup(context.Background()) })

	// Warm the provider's normal key cache with one successful Wrap. This is
	// the reproduction of the defect the uncached check avoids.
	wrap := func() error {
		_, err := environment.keys.Wrap(context.Background(), secret.WrapRequest{
			KeyRef:         config.Production.Cloud.KeyRef,
			KeyVersion:     secret.MinKeyVersion,
			Plaintext:      make([]byte, secret.DataKeyBytes),
			AssociatedData: []byte("readiness-warm-cache"),
		})
		return err
	}
	if err := wrap(); err != nil {
		t.Fatalf("warm Wrap: %v", err)
	}
	if calls := kms.callCount(); calls != 1 {
		t.Fatalf("warm Wrap KMS calls = %d; want 1", calls)
	}

	kms.setError(errors.New("KMS unavailable"))
	if err := wrap(); err != nil {
		t.Fatalf("cached Wrap after KMS failure = %v; want the warm cache to keep ordinary use working", err)
	}
	if calls := kms.callCount(); calls != 1 {
		t.Fatalf("cached Wrap called KMS again: calls = %d; want 1", calls)
	}

	check := kmsReadinessCheckWithClock(kms, config.Production.Cloud, time.Now)
	if err := check(context.Background()); err == nil {
		t.Fatal("the readiness KMS check used the cache and missed the KMS outage")
	}

	// With a healthy KMS the check validates the data-key length and clears
	// the returned plaintext before returning.
	kms.setError(nil)
	if err := check(context.Background()); err != nil {
		t.Fatalf("readiness KMS check: %v", err)
	}
	cleared := kms.lastPlaintext()
	if len(cleared) != secret.DataKeyBytes {
		t.Fatalf("readiness KMS plaintext length = %d; want %d", len(cleared), secret.DataKeyBytes)
	}
	for index, value := range cleared {
		if value != 0 {
			t.Fatalf("readiness left plaintext byte %d uncleared (%#x)", index, value)
		}
	}
}

// readinessTestCloud is the minimal configured cloud KMS result the readiness
// probe validates. The fake client ignores the reference and blob.
func readinessTestCloud() keys.CloudConfig {
	return keys.CloudConfig{
		KeyRef:   "aws-kms:key-1",
		Current:  secret.MinKeyVersion,
		Versions: map[secret.KeyVersion][]byte{secret.MinKeyVersion: make([]byte, secret.DataKeyBytes)},
	}
}

// TestReadinessKMSCacheServesSuccessWithinTheTTL then re-probes exactly at the
// bound. The cache is injected with a fake clock, so the test does not sleep.
func TestReadinessKMSCacheServesSuccessWithinTheTTL(t *testing.T) {
	clock := newFakeClock()
	kms := &readinessKMS{}
	check := kmsReadinessCheckWithClock(kms, readinessTestCloud(), clock.Now)

	if err := check(context.Background()); err != nil {
		t.Fatalf("first readiness decrypt: %v", err)
	}
	if err := check(context.Background()); err != nil {
		t.Fatalf("cached readiness decrypt: %v", err)
	}
	if calls := kms.callCount(); calls != 1 {
		t.Fatalf("KMS decrypts inside the TTL = %d; want 1", calls)
	}

	clock.Advance(readinessKMSCacheTTL - time.Millisecond)
	if err := check(context.Background()); err != nil {
		t.Fatalf("readiness decrypt just before expiry: %v", err)
	}
	if calls := kms.callCount(); calls != 1 {
		t.Fatalf("KMS decrypts just before the TTL = %d; want the cached 1", calls)
	}

	clock.Advance(time.Millisecond)
	if err := check(context.Background()); err != nil {
		t.Fatalf("readiness decrypt at expiry: %v", err)
	}
	if calls := kms.callCount(); calls != 2 {
		t.Fatalf("KMS decrypts at the TTL = %d; want a fresh 2", calls)
	}
}

// TestReadinessKMSCacheNeverCachesFailures proves a failed decrypt is retried
// on the next probe even though an earlier success was cached, and that a
// failure does not shorten or extend the success window.
func TestReadinessKMSCacheNeverCachesFailures(t *testing.T) {
	clock := newFakeClock()
	kms := &readinessKMS{}
	check := kmsReadinessCheckWithClock(kms, readinessTestCloud(), clock.Now)

	kms.setError(fault.New(fault.DependencyUnavailable, "KMS unavailable SENTINEL-RAW-KMS-DETAIL"))
	for attempt := 1; attempt <= 2; attempt++ {
		if err := check(context.Background()); err == nil {
			t.Fatalf("attempt %d succeeded against a failing KMS", attempt)
		}
		if calls := kms.callCount(); calls != attempt {
			t.Fatalf("KMS decrypts after %d failures = %d; want every failure to re-probe", attempt, calls)
		}
	}

	// A failure at the expiry must not revive the stale success: the next
	// probe after the KMS recovers decrypts again and only then caches.
	clock.Advance(readinessKMSCacheTTL)
	kms.setError(nil)
	if err := check(context.Background()); err != nil {
		t.Fatalf("readiness decrypt after recovery: %v", err)
	}
	if err := check(context.Background()); err != nil {
		t.Fatalf("cached readiness decrypt after recovery: %v", err)
	}
	if calls := kms.callCount(); calls != 3 {
		t.Fatalf("KMS decrypts after recovery = %d; want 3", calls)
	}
}

// TestReadinessKMSCacheIsNotSharedBetweenChecks proves the cache is per
// readiness check, not package state: two composed installations (or two
// checks) never serve each other's decrypt result.
func TestReadinessKMSCacheIsNotSharedBetweenChecks(t *testing.T) {
	clock := newFakeClock()
	first := &readinessKMS{}
	second := &readinessKMS{}
	firstCheck := kmsReadinessCheckWithClock(first, readinessTestCloud(), clock.Now)
	secondCheck := kmsReadinessCheckWithClock(second, readinessTestCloud(), clock.Now)

	if err := firstCheck(context.Background()); err != nil {
		t.Fatalf("first check: %v", err)
	}
	if err := secondCheck(context.Background()); err != nil {
		t.Fatalf("second check: %v", err)
	}
	if first.callCount() != 1 || second.callCount() != 1 {
		t.Fatalf("KMS decrypts per check = %d/%d; want 1/1", first.callCount(), second.callCount())
	}
}

// TestReadinessKMSProbeStaysMandatory pins that production cannot lose its KMS
// probe: a missing client or blob still fails readiness even though a
// successful decrypt is cached.
func TestReadinessKMSProbeStaysMandatory(t *testing.T) {
	now := newFakeClock().Now
	if err := kmsReadinessCheckWithClock(nil, readinessTestCloud(), now)(context.Background()); err == nil {
		t.Fatal("readiness passed without a KMS client")
	}
	if err := kmsReadinessCheckWithClock(&readinessKMS{}, keys.CloudConfig{}, now)(context.Background()); err == nil {
		t.Fatal("readiness passed without a configured KMS blob")
	}
}

// TestReadinessFailuresLogTheFaultClassOnly proves a failed probe logs the
// stable class and the check name, never the adapter's private error text.
func TestReadinessFailuresLogTheFaultClassOnly(t *testing.T) {
	installReadinessPools(t)
	kms := &readinessKMS{}
	server := &fakeNATSServer{}
	logs := &bytes.Buffer{}
	config := productionConfigWithServer(t, server)
	config.Logger = slog.New(slog.NewTextHandler(logs, nil))
	config.Production.KMS = kms
	app, err := New(context.Background(), config)
	if err != nil {
		t.Fatalf("New production composition: %v", err)
	}
	t.Cleanup(func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = app.Close(shutdown)
	})
	markAppStarted(t, app)

	kms.setError(fault.New(fault.DependencyUnavailable, "KMS detail SENTINEL-RAW-KMS-DETAIL"))
	if code, _ := readinessStatus(t, app.Handler()); code != http.StatusServiceUnavailable {
		t.Fatalf("readyz with a failing KMS = %d; want 503", code)
	}
	logged := logs.String()
	if !strings.Contains(logged, "readiness check failed") || !strings.Contains(logged, "class=dependency_unavailable") {
		t.Fatalf("readiness failure was not logged by class: %s", logged)
	}
	if !strings.Contains(logged, "check=kms") {
		t.Fatalf("readiness failure did not name the KMS check: %s", logged)
	}
	if strings.Contains(logged, "SENTINEL-RAW-KMS-DETAIL") {
		t.Fatalf("readiness failure log rendered the raw error text: %s", logged)
	}
}

func TestReadinessFailsClosedBeforeStartAndAfterClose(t *testing.T) {
	environment := newTestEnvironment(t)
	app, err := New(context.Background(), environment.config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = app.Close(shutdown)
	}()

	if code, body := readinessStatus(t, app.Handler()); code != http.StatusServiceUnavailable || body != httpapi.ReadinessUnavailableBody {
		t.Fatalf("readyz before start = %d %q; want 503 %q", code, body, httpapi.ReadinessUnavailableBody)
	}
	markAppStarted(t, app)
	if code, body := readinessStatus(t, app.Handler()); code != http.StatusOK || body != httpapi.ReadinessBody {
		t.Fatalf("readyz after start = %d %q; want 200 %q", code, body, httpapi.ReadinessBody)
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := app.Close(shutdown); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if code, body := readinessStatus(t, app.Handler()); code != http.StatusServiceUnavailable || body != httpapi.ReadinessUnavailableBody {
		t.Fatalf("readyz after close = %d %q; want 503 %q", code, body, httpapi.ReadinessUnavailableBody)
	}
}

func TestReadinessHonorsRequestCancellation(t *testing.T) {
	fixture := newReadinessFixture(t)
	fixture.pools[0].setBlock(true)

	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodGet, httpapi.ReadinessPath, nil).WithContext(ctx)
	recorder := httptest.NewRecorder()
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	startedAt := time.Now()
	fixture.app.Handler().ServeHTTP(recorder, request)
	elapsed := time.Since(startedAt)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz with a cancelled request = %d; want 503", recorder.Code)
	}
	if elapsed > time.Second {
		t.Fatalf("readyz ignored request cancellation for %s", elapsed)
	}
}

func TestReadinessIsConcurrencySafeDuringShutdown(t *testing.T) {
	environment := newTestEnvironment(t)
	app := environment.startApp(t)
	handler := app.Handler()

	var wait sync.WaitGroup
	unexpected := atomic.Int64{}
	stop := make(chan struct{})
	for range 8 {
		wait.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				code, _ := readinessStatus(t, handler)
				if code != http.StatusOK && code != http.StatusServiceUnavailable {
					unexpected.Add(1)
				}
			}
		})
	}
	time.Sleep(10 * time.Millisecond)
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := app.Close(shutdown); err != nil {
		t.Fatalf("Close: %v", err)
	}
	close(stop)
	wait.Wait()
	if got := unexpected.Load(); got != 0 {
		t.Fatalf("%d readiness responses were neither 200 nor 503 during shutdown", got)
	}
	if code, _ := readinessStatus(t, handler); code != http.StatusServiceUnavailable {
		t.Fatalf("readyz after shutdown = %d; want 503", code)
	}
}

func TestHealthWrapperPreservesAPIAuthentication(t *testing.T) {
	environment := newTestEnvironment(t)
	app := environment.startApp(t)
	handler := app.Handler()

	request := httptest.NewRequest(http.MethodGet, "/api/v1/projects/project-1/memberships", strings.NewReader(`{"input":{}}`))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("bearer-less API request = %d; want the unchanged 401 authentication refusal", recorder.Code)
	}
	if code, body := livenessStatus(t, handler); code != http.StatusOK || body != httpapi.HealthBody {
		t.Fatalf("healthz on the same composed handler = %d %q; want 200 %q", code, body, httpapi.HealthBody)
	}
}

func TestReadinessDoesNotGateControllerProgression(t *testing.T) {
	environment := newTestEnvironment(t)
	app, err := New(context.Background(), environment.config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = app.Close(shutdown)
	}()

	// A not-ready probe response is observational only: it never disables the
	// dispatch gate or stops the controller loops from starting.
	if code, _ := readinessStatus(t, app.Handler()); code != http.StatusServiceUnavailable {
		t.Fatalf("readyz before start = %d; want 503", code)
	}
	if !app.gate.Enabled() {
		t.Fatal("a not-ready probe disabled controller dispatch")
	}
	if err := app.start(context.Background()); err != nil {
		t.Fatalf("start after a not-ready probe: %v", err)
	}
	if code, _ := readinessStatus(t, app.Handler()); code != http.StatusOK {
		t.Fatalf("readyz after start = %d; want 200", code)
	}
}

func TestProductionCompositionRefusesHealthPathCollision(t *testing.T) {
	for _, path := range []string{httpapi.HealthPath, httpapi.ReadinessPath} {
		t.Run(path, func(t *testing.T) {
			installReadinessPools(t)
			server := &fakeNATSServer{}
			config := productionConfigWithServer(t, server).resolved()
			config.MCPPath = path
			if _, err := New(context.Background(), config); err == nil {
				t.Fatalf("New accepted MCPPath %q, which collides with the operational route", path)
			}
		})
	}
}

func TestReadinessBudgetFitsBelowTheChartProbeTimeout(t *testing.T) {
	// The chart requires an explicit timeoutSeconds of at least 5 and defaults
	// to 10. Every readiness check shares this one budget, so the whole probe
	// stays below what the kubelet allows.
	if readinessBudget <= 0 {
		t.Fatalf("readiness budget = %s; want a positive bound", readinessBudget)
	}
	if readinessBudget >= 5*time.Second {
		t.Fatalf("readiness budget = %s; want it below the chart's 5s minimum probe timeout", readinessBudget)
	}
}

func TestReadinessHeadParity(t *testing.T) {
	fixture := newReadinessFixture(t)
	handler := fixture.app.Handler()

	get := httptest.NewRecorder()
	handler.ServeHTTP(get, httptest.NewRequest(http.MethodGet, httpapi.ReadinessPath, nil))
	head := httptest.NewRecorder()
	handler.ServeHTTP(head, httptest.NewRequest(http.MethodHead, httpapi.ReadinessPath, nil))
	if get.Code != http.StatusOK || head.Code != get.Code {
		t.Fatalf("readyz GET/HEAD status = %d/%d; want the same 200", get.Code, head.Code)
	}
	if head.Body.Len() != 0 {
		t.Fatalf("readyz HEAD wrote %d body bytes; want none", head.Body.Len())
	}
	if get.Header().Get("Content-Length") != head.Header().Get("Content-Length") {
		t.Fatalf("readyz HEAD Content-Length differs from GET")
	}
}
