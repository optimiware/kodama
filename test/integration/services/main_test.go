//go:build integration

package services

// Post-run cleanup accounting for the services integration binary.
//
// This package drives the real factory composition (PostgreSQL, NATS and
// Podman) but never creates a kind cluster, so only the Podman family needs an
// orphan probe here. The pattern is the one from
// test/integration/lifecycle/main_test.go: resources this process created that
// survive cleanup fail a passing run; a best-effort sweep error can belong to
// another concurrent lane and is logged instead, because that lane may remove
// its resource between the sweep's list and inspect.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/optimiware/kodama/test/support/factory"
	"github.com/optimiware/kodama/test/support/infrastructure/harness"
	runtimesupport "github.com/optimiware/kodama/test/support/infrastructure/runtime"
)

// TestMain installs the shared signal/orphan cleanup for the real
// infrastructure this suite uses. Every container and volume the suite creates
// is run-labelled, so a killed test process is swept instead of leaking
// infrastructure.
func TestMain(m *testing.M) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)

	done := make(chan struct{})
	var stopOnce sync.Once
	go func() {
		select {
		case sig := <-signals:
			os.Exit(harness.HandleSignal(sig, func(ctx context.Context) error {
				outcome := cleanupAll(ctx)
				logCleanup(outcome)
				return outcome.Own
			}))
		case <-done:
		}
	}()

	code := m.Run()
	stopOnce.Do(func() { close(done) })

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	outcome := cleanupAll(ctx)
	logCleanup(outcome)
	os.Exit(factory.CleanupExitCode(code, outcome))
}

// logCleanup reports the post-run cleanup outcome. Own errors are the fatal
// ones; sweep errors are reported but never fail the run.
func logCleanup(outcome factory.CleanupOutcome) {
	if outcome.Own != nil {
		fmt.Fprintf(os.Stderr, "services: post-run cleanup: %v\n", outcome.Own)
	}
	if outcome.Sweep != nil {
		fmt.Fprintf(os.Stderr, "services: post-run sweep (logged, not fatal): %v\n", outcome.Sweep)
	}
}

// cleanupAll removes this run's Podman resources and then sweeps dead-owner
// resources. The sweep can race another live lane over the same dead-owner
// resource, so its error is separated from a leak this process caused.
func cleanupAll(ctx context.Context) factory.CleanupOutcome {
	var own, sweep []error
	if err := runtimesupport.RemoveRunContainers(ctx); err != nil {
		own = append(own, fmt.Errorf("remove run containers: %w", err))
	}
	if err := runtimesupport.RemoveRunVolumes(ctx); err != nil {
		own = append(own, fmt.Errorf("remove run volumes: %w", err))
	}
	if err := harness.CleanupOrphans(ctx); err != nil {
		sweep = append(sweep, fmt.Errorf("orphan sweep: %w", err))
	}
	return factory.CleanupOutcome{Own: errors.Join(own...), Sweep: errors.Join(sweep...)}
}
