package app

// Operational readiness for the composed server. app.New installs the wrapper
// around the composed public handler; liveness stays independent of every
// dependency, while readiness probes the production dependencies themselves.
//
// The checks are the same dependencies the server uses:
//
//   - each of the four role pools opens and rolls back one transaction, so a
//     broken role credential or grant is visible;
//   - the broker is dialed with the authenticated TLS configuration and closed
//     again. natsgo.Connect is not bounded by its context, so the dial carries
//     the probe budget as ConnectTimeout;
//   - the configured cloud KMS decrypts the configured blob and the plaintext
//     is length-checked and cleared. The ordinary key provider caches its
//     unwrapped key, so a readiness check written as a normal Wrap would keep
//     passing after KMS went away. A successful decrypt (and only a success)
//     is cached for readinessKMSCacheTTL, so ordinary probe bursts do not
//     decrypt once per probe while an outage still surfaces when the entry
//     expires.
//
// Every failed check is logged by fault class and check name only; adapter
// error text can carry credential or payload material and never reaches a log.
// One readiness budget bounds every check together; it is strictly below the
// chart's minimum probe timeoutSeconds.

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	natsdriver "github.com/optimiware/kodama/internal/shell/app/drivers/nats"
	"github.com/optimiware/kodama/internal/shell/integration/keys"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/transport/httpapi"
)

// readinessBudget bounds one complete readiness probe. Every check shares it,
// and the probe's NATS ConnectTimeout is set from it. The chart's minimum
// timeoutSeconds is 5s, so the whole probe always fits below the kubelet
// timeout.
const readinessBudget = 4 * time.Second

// readinessKMSCacheTTL bounds how long one successful KMS decrypt satisfies
// readiness. It is shorter than a credential or key rotation window and long
// enough that a normal probe interval (seconds to tens of seconds) does not
// decrypt on every request. A failure is never cached.
const readinessKMSCacheTTL = 30 * time.Second

// readinessCheck is one named dependency probe. The name selects the log
// attribution only; it never carries a configured value.
type readinessCheck struct {
	name string
	run  httpapi.ReadinessCheck
}

// readinessDependencies is a durable composition's probe surface. It carries
// the four role pools in composition order (runtime, controller, publisher,
// auth) and the exact broker configuration the server uses, plus the key
// probe: production's cloud KMS configuration, or the durable test profile's
// explicitly selected local probes (keyfile and Podman API).
type readinessDependencies struct {
	pools  []postgresPool
	dial   natsDial
	broker natsdriver.Config
	kms    keys.KMSClient
	cloud  keys.CloudConfig
	// local, when set by the durable test profile, replaces the KMS probe.
	// Production never sets it, so a missing KMS client still fails readiness.
	local []readinessCheck
}

// newHealthHandler builds the operational wrapper around the composed handler
// and the composition's readiness dependencies. A nil dependency bundle (the
// development profile and the injected test form) declares no dependency check: readiness then
// only requires the app to be started and not closing.
func newHealthHandler(app *App, inner http.Handler, config Config, dependencies *readinessDependencies, metrics *metricsSource) (http.Handler, error) {
	var checks []httpapi.ReadinessCheck
	if dependencies != nil {
		now := time.Now
		if app.clock != nil {
			now = app.clock.Now
		}
		checks = readinessChecks(dependencies, app.logger, now, app.metrics)
	}
	var metricsSource httpapi.MetricsSource
	if metrics != nil {
		metricsSource = metrics.collect
	}
	return httpapi.NewHealthHandler(httpapi.HealthConfig{
		Inner: inner,
		Started: func() bool {
			started, _ := app.readinessState()
			return started
		},
		Closing: func() bool {
			_, closing := app.readinessState()
			return closing
		},
		Checks:        checks,
		Metrics:       metricsSource,
		Timeout:       readinessBudget,
		ReservedPaths: []string{config.MCPPath},
	})
}

// readinessState reports the lifecycle flags under the same mutex Close and
// start use, so a probe observes a consistent state during shutdown.
func (app *App) readinessState() (started, closing bool) {
	app.mu.Lock()
	defer app.mu.Unlock()
	return app.started, app.closed
}

func readinessChecks(dependencies *readinessDependencies, logger *slog.Logger, now func() time.Time, metrics *operationsMetrics) []httpapi.ReadinessCheck {
	checks := make([]readinessCheck, 0, len(dependencies.pools)+2+len(dependencies.local))
	for _, pool := range dependencies.pools {
		checks = append(checks, readinessCheck{name: "database", run: postgresReadinessCheck(pool)})
	}
	checks = append(checks, readinessCheck{name: "broker", run: natsReadinessCheck(dependencies.dial, dependencies.broker)})
	if dependencies.local != nil {
		checks = append(checks, dependencies.local...)
	} else {
		checks = append(checks, readinessCheck{name: "kms", run: kmsReadinessCheckWithClock(dependencies.kms, dependencies.cloud, now)})
	}
	wrapped := make([]httpapi.ReadinessCheck, 0, len(checks))
	for _, check := range checks {
		wrapped = append(wrapped, logReadinessFailure(logger, check, metrics))
	}
	return wrapped
}

// logReadinessFailure logs one failed check by fault class and check name. The
// adapter's error text is deliberately not rendered: it may quote a DSN,
// credential or payload. An error without a stable class is reported as an
// unexpected failure rather than as its text.
func logReadinessFailure(logger *slog.Logger, check readinessCheck, metrics *operationsMetrics) httpapi.ReadinessCheck {
	return func(ctx context.Context) error {
		err := check.run(ctx)
		if err == nil {
			return nil
		}
		class := "unexpected error"
		if faultClass, ok := fault.ClassOf(err); ok {
			class = string(faultClass)
		}
		metrics.countDependencyError()
		logger.Warn("readiness check failed", "check", check.name, "class", class)
		return err
	}
}

// postgresReadinessCheck opens one transaction on one role pool and rolls it
// back. A Begin failure (bad credential, unreachable server, exhausted pool)
// and a Rollback failure both make the process not ready. The rollback runs on
// a bounded context that survives caller cancellation, so a probe never leaves
// a transaction open because the request ended.
func postgresReadinessCheck(pool postgresPool) httpapi.ReadinessCheck {
	return func(ctx context.Context) error {
		if pool == nil {
			return fault.New(fault.DependencyUnavailable, "database pool is not configured")
		}
		conn, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		if conn == nil {
			return fault.New(fault.DependencyUnavailable, "database pool returned no connection")
		}
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), readinessBudget)
		defer cancel()
		return conn.Rollback(rollbackCtx)
	}
}

// natsReadinessCheck dials the authenticated TLS broker and closes the
// connection it opened. The dial gets the probe budget as ConnectTimeout
// because the driver's natsgo.Connect call cannot be cancelled by ctx alone.
func natsReadinessCheck(dial natsDial, config natsdriver.Config) httpapi.ReadinessCheck {
	return func(ctx context.Context) error {
		if dial == nil {
			return fault.New(fault.DependencyUnavailable, "broker dial is not configured")
		}
		probe := config
		probe.ConnectTimeout = readinessBudget
		client, err := dial(ctx, probe)
		if err != nil {
			return err
		}
		if client == nil {
			return fault.New(fault.DependencyUnavailable, "broker dial returned no client")
		}
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), readinessBudget)
		defer cancel()
		return client.Close(closeCtx)
	}
}

// kmsReadinessCheckWithClock asks the configured KMS client to decrypt the
// configured blob, on an injected clock. It never consults the key provider's
// cache: an ordinary Wrap can keep succeeding from a warm cache after KMS is
// gone, which is exactly the outage readiness must expose. The result is
// length-validated and cleared before returning.
//
// A successful decrypt is cached for readinessKMSCacheTTL so a burst of probes
// does not decrypt once per probe; a failed decrypt, a missing client or a
// wrong-length key is never cached. The cache is per check instance, so two
// composed installations never share a result.
func kmsReadinessCheckWithClock(client keys.KMSClient, cloud keys.CloudConfig, now func() time.Time) httpapi.ReadinessCheck {
	if now == nil {
		now = time.Now
	}
	cache := &kmsReadinessCache{now: now, ttl: readinessKMSCacheTTL}
	return func(ctx context.Context) error {
		if client == nil {
			return fault.New(fault.DependencyUnavailable, "key management service client is not configured")
		}
		blob, configured := cloud.Versions[cloud.Current]
		if !configured || len(blob) == 0 {
			return fault.New(fault.DependencyUnavailable, "key management service blob is not configured")
		}
		if cache.fresh() {
			return nil
		}
		plaintext, err := client.Decrypt(ctx, cloud.KeyRef, append([]byte(nil), blob...))
		if err != nil {
			cache.invalidate()
			return err
		}
		defer clear(plaintext)
		if len(plaintext) != secret.DataKeyBytes {
			cache.invalidate()
			return fault.New(fault.DependencyUnavailable,
				"key management service returned a key of the wrong length")
		}
		cache.store()
		return nil
	}
}

// kmsReadinessCache remembers one successful KMS decrypt until its bounded
// expiry. Failures never populate the cache, and a failure clears any entry so
// a stale success can never mask a later outage past its window.
type kmsReadinessCache struct {
	mu    sync.Mutex
	now   func() time.Time
	ttl   time.Duration
	valid bool
	until time.Time
}

func (cache *kmsReadinessCache) fresh() bool {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return cache.valid && cache.now().Before(cache.until)
}

func (cache *kmsReadinessCache) store() {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.valid = true
	cache.until = cache.now().Add(cache.ttl)
}

func (cache *kmsReadinessCache) invalidate() {
	cache.mu.Lock()
	cache.valid = false
	cache.mu.Unlock()
}
