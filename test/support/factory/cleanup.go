package factory

// Post-run cleanup accounting shared by the integration test binaries.
//
// A test binary creates two families of resources: the ones it owns (its Podman
// run labels and its own kind clusters) and the ones a best-effort sweep may
// touch because a previous owner died. Only the first family is a leak this
// process caused. The second family is shared with every other lane running
// concurrently, so another lane can remove one of those resources between the
// sweep's list and its inspect. A sweep error therefore must never turn a green
// test run red; it is logged instead.

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"

	kubesupport "github.com/optimiware/kodama/test/support/infrastructure/kubernetes"
)

// CleanupOutcome separates the post-run cleanup errors this process caused
// (Own) from best-effort sweeps of resources that may belong to another live
// lane (Sweep).
type CleanupOutcome struct {
	Own   error
	Sweep error
}

// CleanupExitCode returns the exit code for a finished test run. A leak this
// process caused fails a passing run. A sweep error never changes the result:
// the caller logs it, because it can be another lane's resource removed between
// the sweep's list and inspect.
func CleanupExitCode(code int, outcome CleanupOutcome) int {
	if code == 0 && outcome.Own != nil {
		return 1
	}
	return code
}

// kubernetesClusterCreated records whether this process created a kind
// cluster. When no cluster was ever created, a kind-resolution failure during
// cleanup cannot hide a leak and stays non-fatal.
var kubernetesClusterCreated atomic.Bool

// markKubernetesClusterCreated records that this process created a kind
// cluster. The harness calls it once per created cluster.
func markKubernetesClusterCreated() { kubernetesClusterCreated.Store(true) }

// KubernetesClusterCreated reports whether this process created a kind
// cluster.
func KubernetesClusterCreated() bool { return kubernetesClusterCreated.Load() }

// OwnClusterNames returns the generated kind cluster names owned by pid. It
// matches exactly the harness naming contract (`<ClusterPrefix><pid>-<8 hex>`);
// a foreign, protected or malformed name is never returned.
func OwnClusterNames(names []string, pid int) []string {
	if pid <= 0 {
		return nil
	}
	pattern := regexp.MustCompile(`^` + regexp.QuoteMeta(kubesupport.ClusterPrefix) + strconv.Itoa(pid) + `-[0-9a-f]{8}$`)
	var own []string
	for _, name := range names {
		if pattern.MatchString(name) {
			own = append(own, name)
		}
	}
	return own
}

// ClassifyKindProbe decides how the post-run own-cluster probe contributes to
// the cleanup outcome. A probe failure after this process created a cluster can
// hide a leak, so it is fatal. A probe failure without a created cluster (a
// missing kind binary, for example) and a foreign-resource sweep error are
// logged, not fatal. A cluster owned by this pid that survived the sweep is a
// fatal leak.
//
// The classification is pure so the integration TestMain glue stays a thin
// call and the rules are unit-tested without kind.
func ClassifyKindProbe(probeErr error, leaked []string, created bool, pid int) (own error, sweep error) {
	switch {
	case probeErr != nil && created:
		return fmt.Errorf("kind cleanup: cannot verify clusters after this process created one: %w", probeErr), nil
	case probeErr != nil && !errors.Is(probeErr, kubesupport.ErrKindUnavailable):
		return nil, fmt.Errorf("kind cluster probe: %w", probeErr)
	case probeErr == nil && len(leaked) > 0:
		return fmt.Errorf("kind cleanup: %d cluster(s) owned by pid %d remain: %s",
			len(leaked), pid, strings.Join(leaked, ", ")), nil
	}
	return nil, nil
}
