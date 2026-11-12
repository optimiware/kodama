//go:build integration

package lifecycle

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
	kubesupport "github.com/optimiware/kodama/test/support/infrastructure/kubernetes"
	runtimesupport "github.com/optimiware/kodama/test/support/infrastructure/runtime"
)

// TestMain installs the shared signal/orphan cleanup for both infrastructure
// families this suite uses. Every container and volume this suite creates
// is run-labelled, and every kind cluster is uniquely named and registered by
// kubesupport, so a killed test process is swept instead of leaking
// infrastructure. The protected cluster kodama-probe and any cluster outside
// the generated kodama-t-<pid>-<random> namespace are never touched.
//
// Only a leak this process caused changes the exit code. A dead-owner sweep
// error can belong to another lane whose resources another process removed
// between the sweep's list and its inspect; it is logged and the run stays
// green when every test passed.
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
		fmt.Fprintf(os.Stderr, "lifecycle: post-run cleanup: %v\n", outcome.Own)
	}
	if outcome.Sweep != nil {
		fmt.Fprintf(os.Stderr, "lifecycle: post-run sweep (logged, not fatal): %v\n", outcome.Sweep)
	}
}

// cleanupAll removes this run's Podman resources and every kind cluster this
// process created, then sweeps dead-owner resources of both families. The
// returned outcome separates leaks this process caused from best-effort sweep
// failures over resources another lane may own.
func cleanupAll(ctx context.Context) factory.CleanupOutcome {
	var own, sweep []error
	if err := runtimesupport.RemoveRunContainers(ctx); err != nil {
		own = append(own, fmt.Errorf("remove run containers: %w", err))
	}
	if err := runtimesupport.RemoveRunVolumes(ctx); err != nil {
		own = append(own, fmt.Errorf("remove run volumes: %w", err))
	}

	// PostRunCleanup closes this process's clusters and sweeps dead-owner and
	// own-pid clusters. Its error can be a foreign cluster another lane already
	// removed, so it is logged. A cluster owned by this pid that survives the
	// sweep is detected by the probe below and fails the run.
	if err := kubesupport.PostRunCleanup(ctx); err != nil {
		sweep = append(sweep, fmt.Errorf("kind orphan sweep: %w", err))
	}
	leaked, probeErr := ownKindClusters(ctx)
	ownErr, sweepErr := factory.ClassifyKindProbe(probeErr, leaked, factory.KubernetesClusterCreated(), os.Getpid())
	if ownErr != nil {
		own = append(own, ownErr)
	}
	if sweepErr != nil {
		sweep = append(sweep, sweepErr)
	}

	// The Podman orphan sweep lists every dead-owner resource and inspects it.
	// Another live lane can remove one of them in between, so its error is a
	// foreign-resource condition: log it, never fail the run.
	if err := harness.CleanupOrphans(ctx); err != nil {
		sweep = append(sweep, fmt.Errorf("orphan sweep: %w", err))
	}
	return factory.CleanupOutcome{Own: errors.Join(own...), Sweep: errors.Join(sweep...)}
}

// ownKindClusters lists the generated kind clusters this process still owns
// after the sweep. A missing kind binary means this process never created a
// cluster; that is reported as a kind-unavailable error so the caller can keep
// it non-fatal.
func ownKindClusters(ctx context.Context) ([]string, error) {
	kind, err := kubesupport.NewKind(kubesupport.WithoutDownload())
	if err != nil {
		return nil, err
	}
	names, err := kind.ListClusters(ctx)
	if err != nil {
		return nil, err
	}
	return factory.OwnClusterNames(names, os.Getpid()), nil
}
