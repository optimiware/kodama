//go:build qualification

package production

import (
	"context"
	"testing"
	"time"
)

// TestStageRequestWaitRevocation proves the request-wait revocation slice on
// the live stage. A declared service/<id> request action waits inside one
// inline execution; the harness captures the pre-wait action's mounted
// execution credential through guarded reads and proves it authenticated before
// activation; after activation that credential is refused 401 inside its
// recorded deadline; the admin revokes the service credential; the stale
// service token is refused on the inbox and on action-only resolve; a fresh
// service token rediscovers and resolves the same request with only the
// expected action version, the declared answer and an idempotency key; an
// identical authorized replay returns the recorded result while the revoked
// token is still refused; and the SAME execution succeeds with the recorded
// service actor and answer, a fresh mounted credential, exactly one completion
// marker and complete cleanup.
//
// The probe context is eight minutes; the strict cleanup that was registered
// before the first mutation has its own three-minute budget and tears the
// service authority down FIRST (revoke, disable and the revoked-token refusal
// on a 45 s window of their own) before it cancels the execution and runs the
// workload-cleanup waits, so a workload cleanup that never converges (the known
// F-4 shape) can never leave a live service credential behind. The pre-wait
// action is allowed its full bounded start window (a first-time image pull is
// not a failure), and the credential capture polls the execution/action-labelled
// Secret for a bounded 60 s because the public running state can precede the
// adapter's Secret write. A refusal names its public operation: the attempt-4
// stage run failed on an unlabelled task.inbox invalid_input that turned out to
// be a page limit above the served inbox bound; a later attempt-4 run failed
// the capture on a single unlucky Secret read; the third attempt-4 run failed a
// 1-minute workload predicate that required the completed pre-wait Job and Pod
// to be gone while the execution was still waiting, although the product
// retains them until terminal cleanup or the Job TTL. Every bounded wait now
// names its step and last secret-free observation. Q02 stays incomplete: this
// is a pre-G03 test-code landing.
func TestStageRequestWaitRevocation(t *testing.T) {
	stageWave1Parallel(t)
	config := loadStageConfig(t)
	requireStageInputs(t, config, requestWaitInputs()...)

	serviceID, err := probeID("q02-rw")
	if err != nil {
		t.Fatalf("generate the request-wait service identity: %v", err)
	}
	runMarker, err := probeID("q02-rw-run")
	if err != nil {
		t.Fatalf("generate the request-wait run marker: %v", err)
	}
	client := newStageClient(t, config)
	guard, err := NewGuard(config.KubeContext, config.TenantNamespace)
	if err != nil {
		t.Fatalf("build the confinement guard: %v", err)
	}
	cluster := &requestWaitKube{
		client: &Kubectl{
			Guard:   guard,
			Path:    "kubectl",
			Timeout: 3 * time.Minute,
		},
		namespace: config.TenantNamespace,
	}
	probe := newRequestWaitProbe(config, client, cluster, serviceID, runMarker)
	cleanup := newRequestWaitCleanup(probe)

	// Register cleanup BEFORE the first mutation: a mid-probe failure still
	// cancels the execution and revokes/disables the service, and a cleanup
	// failure is reported strictly.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), requestWaitCleanupTimeout)
		defer cancel()
		if err := cleanup.run(ctx); err != nil {
			t.Errorf("the request-wait cleanup failed: %v", err)
		}
	})

	started := time.Now().UTC()
	ctx, cancel := context.WithTimeout(context.Background(), requestWaitProbeTimeout)
	defer cancel()
	if err := probe.run(ctx); err != nil {
		t.Fatal(err)
	}
	writeStageEvidence(t, config, probe.evidence(started, time.Now().UTC(), cleanup.executionID))
}
