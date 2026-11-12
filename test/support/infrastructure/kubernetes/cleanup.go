// Cleanup for kind clusters: active-cluster registry, signal handling and the
// next-run dead-owner sweep.
package kubesupport

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

	"github.com/optimiware/kodama/test/support/infrastructure/harness"
)

// registryMu guards the active cluster registry so parallel tests do not race
// on shared state.
var (
	registryMu sync.Mutex
	active     = map[*Cluster]struct{}{}
)

func registerCluster(cluster *Cluster) {
	registryMu.Lock()
	defer registryMu.Unlock()
	active[cluster] = struct{}{}
}

func unregisterCluster(cluster *Cluster) {
	registryMu.Lock()
	defer registryMu.Unlock()
	delete(active, cluster)
}

// ActiveClusters returns a snapshot of the clusters this process created.
func ActiveClusters() []*Cluster {
	registryMu.Lock()
	defer registryMu.Unlock()
	clusters := make([]*Cluster, 0, len(active))
	for cluster := range active {
		clusters = append(clusters, cluster)
	}
	return clusters
}

// CleanupActiveClusters deletes every cluster this process still owns. Every
// deletion is attempted even after a failure.
func CleanupActiveClusters(ctx context.Context) error {
	_ = ctx
	var errs []error
	for _, cluster := range ActiveClusters() {
		if err := cluster.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// newSweepKind builds the no-download kind client used by the cleanup paths.
// It reuses KODAMA_KIND_BIN, a PATH kind, or the already verified pinned cache
// file. A missing binary is reported as errKindBinaryAbsent so the caller can
// skip silently; any other resolution failure (for example a corrupted cache)
// is surfaced. A caller-supplied logger is kept; otherwise NewKind's no-op
// logger applies.
func newSweepKind(options ...Option) (*Kind, error) {
	options = append(options, WithoutDownload())
	return NewKind(options...)
}

// SweepDeadOwnersBestEffort deletes generated clusters whose owner pid is dead.
// It never downloads kind. The only silent skip is no binary anywhere, because
// then this process cannot have created a cluster; a corrupted cache or a
// version mismatch is reported.
func SweepDeadOwnersBestEffort(ctx context.Context, options ...Option) error {
	kind, err := newSweepKind(options...)
	if err != nil {
		if errors.Is(err, errKindBinaryAbsent) {
			return nil
		}
		return err
	}
	_, err = kind.SweepDeadOwners(ctx)
	return err
}

// ownClusterNames returns the names owned by pid.
func ownClusterNames(names []string, pid int) []string {
	var own []string
	for _, name := range names {
		if owner, ok := clusterOwner(name); ok && owner == pid {
			own = append(own, name)
		}
	}
	return own
}

// SweepOwnOrphansBestEffort is the post-run sweep. Every registered cluster has
// already been closed, so clusters owned by this pid are orphans too. It returns
// an error when an own-pid cluster is still listed after the sweep or when this
// process cannot verify its own cleanup.
//
// A dead-owner cluster belongs to some other (possibly concurrent) run. Another
// lane can remove it between this sweep's list and its delete, so that sweep
// error is logged, never fatal: it cannot be this process's leak.
func SweepOwnOrphansBestEffort(ctx context.Context, options ...Option) error {
	kind, err := newSweepKind(options...)
	if err != nil {
		if errors.Is(err, errKindBinaryAbsent) {
			return nil
		}
		return err
	}
	return kind.sweepOwnOrphans(ctx, os.Getpid())
}

// PostRunCleanup closes every registered cluster, sweeps dead-owner and
// own-pid clusters, and reports an error if any own-pid cluster remains. Main
// turns a non-nil result into a non-zero exit code so a leak never reports
// green. Foreign dead-owner sweep errors are logged inside the sweep and do not
// fail this run.
func PostRunCleanup(ctx context.Context, options ...Option) error {
	var errs []error
	if err := CleanupActiveClusters(ctx); err != nil {
		errs = append(errs, fmt.Errorf("kubesupport: post-run cluster cleanup: %w", err))
	}
	if err := SweepOwnOrphansBestEffort(ctx, options...); err != nil {
		errs = append(errs, fmt.Errorf("kubesupport: post-run orphan sweep: %w", err))
	}
	return errors.Join(errs...)
}

// Main is the TestMain helper for the Kubernetes component binary. It installs
// SIGINT/SIGTERM handling that deletes active clusters before exiting, runs m,
// then deletes active clusters and sweeps dead-owner and own-pid clusters. A
// cleanup failure or any own-pid cluster left behind makes the process exit
// non-zero.
func Main(m *testing.M) int {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)

	done := make(chan struct{})
	var stopOnce sync.Once
	go func() {
		select {
		case sig := <-signals:
			code := harness.HandleSignal(sig, CleanupActiveClusters)
			os.Exit(code)
		case <-done:
		}
	}()

	code := m.Run()
	stopOnce.Do(func() { close(done) })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	// A foreign dead-owner sweep error is logged inside the sweep and never
	// fails this run; own leaks and an unverifiable own cleanup still do.
	logf := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	}
	if err := PostRunCleanup(ctx, WithLogger(logf)); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}
