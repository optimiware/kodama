//go:build component

// Package kubernetesconformance is the Kubernetes runtime component suite.
// Every test drives a real rootless kind cluster (Podman provider) and the real
// internal/shell/runtime/kube adapter through the production
// internal/shell/app/drivers/kube REST client.
//
// NetworkPolicy allow/deny and resource-limit enforcement are explicitly out of
// scope for this suite; they belong to cluster qualification. Do not add them
// here.
//
// The tests are independent: each one owns a uniquely named kind cluster and no
// state is shared between tests. Every test calls t.Parallel and clusterSlots
// bounds how many clusters exist at once, so the package finishes in a fraction
// of its sequential duration without oversubscribing Podman. Run it with an
// explicit outer bound:
//
//	KODAMA_REQUIRE_INFRA=1 KODAMA_KIND_BIN=/path/to/kind \
//	KIND_EXPERIMENTAL_PROVIDER=podman \
//	go test -race -tags=component -count=1 -timeout=40m ./test/component/kubernetes/...
//
// 40 minutes is a safe upper bound for the suite on an 8-core machine while
// other Podman lanes run; it is not the target duration. Do not lower it below
// the per-fixture and per-barrier bounds defined here.
package kubernetesconformance

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/engine"
	driverkube "github.com/optimiware/kodama/internal/shell/app/drivers/kube"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
	"github.com/optimiware/kodama/test/support/infrastructure/harness"
	kubesupport "github.com/optimiware/kodama/test/support/infrastructure/kubernetes"
)

const (
	testKodamaURL = "https://kodama.example.com"
	testNamespace = "tenant-acme"
	// workloadImage is the pinned workload image. The digest is resolved by the
	// kind node's own registry pull; the tag is fixed, never "latest".
	workloadImage = "docker.io/library/alpine:3.24"
	// observationTimeout bounds one test body after its fixture is ready. The
	// suite runs three clusters at once, so a body is several times slower than
	// in a sequential run; measured worst case is about 5m20s on an 8-core
	// machine with other Podman lanes active. Fixture creation and the queue for
	// a cluster slot are excluded: newFixture starts this deadline only after
	// both have completed.
	observationTimeout = 8 * time.Minute
	// fixtureTimeout bounds one cluster's readiness barrier.
	fixtureTimeout = 10 * time.Minute
	// maxConcurrentClusters bounds how many kind clusters this package runs at
	// once. Each test owns exactly one cluster, so a slot is held from before
	// creation until after deletion.
	maxConcurrentClusters = 3
	// releaseTimeout bounds the eventual-consistency Release barrier.
	releaseTimeout = 2 * time.Minute
	// releasePollInterval is the short poll between Release attempts. It is a
	// poll interval, never a fixed settle sleep.
	releasePollInterval = 200 * time.Millisecond
)

// clusterSlots bounds concurrent kind clusters. It is deliberately shared
// package state: a buffered channel is the concurrency-safe semaphore, and the
// support cluster registry it protects is already mutex-guarded.
var clusterSlots = make(chan struct{}, maxConcurrentClusters)

// acquireClusterSlot blocks until this test may create its cluster and registers
// the matching release. The release is registered before the cluster exists, so
// a creation failure or skip still frees the slot. Cleanups run last-in
// first-out, so registering the release before the cluster's own Close means the
// slot is freed only after the cluster is deleted.
//
// The wait has its own bound (clusterSlotWaitBound), independent of the test
// body's observation deadline; cancellation by the test context fails with that
// cause instead of looking like a timeout.
func acquireClusterSlot(t *testing.T) {
	t.Helper()
	wait := clusterSlotWaitBound()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case clusterSlots <- struct{}{}:
	case <-t.Context().Done():
		t.Fatalf("waiting for a kind cluster slot: %v", t.Context().Err())
	case <-timer.C:
		t.Fatalf("waited %s for a kind cluster slot; %d are in use", wait, cap(clusterSlots))
	}
	t.Cleanup(func() { <-clusterSlots })
}

func testTenant() id.TenantID { return testNamespace }

// requireInfra reports whether missing infrastructure must fail instead of
// skipping, matching the infrastructure harness policy.
func requireInfra() bool { return os.Getenv("KODAMA_REQUIRE_INFRA") == "1" }

// fixture is one isolated kind cluster, tenant namespace and adapter.
type fixture struct {
	Cluster   *kubesupport.Cluster
	Client    *driverkube.Cluster
	Adapter   *runtimekube.Adapter
	Namespace string
	Tenant    id.TenantID
}

type fixtureOptions struct {
	// withClaim creates the retained workspace claim before returning.
	withClaim bool
	// claimFinalizer adds a hold finalizer so deletion stays pending.
	claimFinalizer string
}

// newFixture creates a unique kind cluster, waits for readiness, creates the
// tenant namespace and, when requested, the retained workspace claim. Missing
// infrastructure follows the skip/fail policy; a broken cluster fails the test.
//
// It acquires the cluster slot and finishes fixture creation before returning
// the test body's observation context, so the per-test deadline never includes
// the queue wait or fixture creation.
func newFixture(t *testing.T, workspaceID id.WorkspaceID, options fixtureOptions) (*fixture, context.Context) {
	t.Helper()
	acquireClusterSlot(t)
	kind, err := kubesupport.NewKind(kubesupport.WithLogger(t.Logf))
	if err != nil {
		harness.SkipOrFail(t, requireInfra(), err)
	}
	cluster, err := kind.CreateCluster(context.Background(), kubesupport.ClusterOptions{})
	if err != nil {
		harness.SkipOrFail(t, requireInfra(), err)
	}
	t.Cleanup(func() {
		if err := cluster.Close(); err != nil {
			// A cluster this test created was not confirmed deleted. Never let a
			// passing test hide a leak: the failure is reported even when the
			// rest of the body passed, and Main's post-run sweep retries it.
			t.Errorf("kind cluster %s cleanup: %v", cluster.Name, err)
		}
	})
	kubesupport.RecordCluster(t, cluster.EvidenceRecord())

	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	if err := cluster.WaitReady(ctx); err != nil {
		harness.SkipOrFail(t, requireInfra(), fmt.Errorf("cluster %s is not ready: %w", cluster.Name, err))
	}
	kubeconfig, err := cluster.KubeconfigJSON(ctx)
	if err != nil {
		t.Fatalf("export kubeconfig: %v", err)
	}
	client, err := driverkube.NewCluster(driverkube.ClusterConfig{
		Kubeconfig: kubeconfig,
		Timeout:    2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("build Kubernetes driver: %v", err)
	}
	if _, err := cluster.Apply(ctx, namespaceYAML()); err != nil {
		t.Fatalf("create tenant namespace: %v", err)
	}
	fx := &fixture{
		Cluster:   cluster,
		Client:    client,
		Namespace: testNamespace,
		Tenant:    testTenant(),
	}
	if options.withClaim {
		claimName, err := runtimekube.WorkspaceClaimName(workspaceID)
		if err != nil {
			t.Fatalf("workspace claim name: %v", err)
		}
		if _, err := cluster.Apply(ctx, claimYAML(claimName, workspaceID, options.claimFinalizer)); err != nil {
			t.Fatalf("create workspace claim: %v", err)
		}
		if _, err := waitForClaim(ctx, client, claimName); err != nil {
			t.Fatalf("workspace claim never appeared: %v", err)
		}
	}
	adapter, err := runtimekube.New(runtimekube.Config{
		Cluster:   client,
		Namespace: testNamespace,
		TenantID:  testTenant(),
		KodamaURL: testKodamaURL,
		Policy: runtimekube.Policy{
			CPURequest:    "50m",
			CPULimit:      "200m",
			MemoryRequest: "32Mi",
			MemoryLimit:   "128Mi",
			JobTTLSeconds: 120,
		},
		Cleanup: runtimekube.CleanupPolicy{Attempts: 3, Backoff: 500 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("build Kubernetes adapter: %v", err)
	}
	fx.Adapter = adapter

	bodyCtx, bodyCancel := context.WithTimeout(t.Context(), observationTimeout)
	t.Cleanup(bodyCancel)
	return fx, bodyCtx
}

func namespaceYAML() string {
	return fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %s
  labels:
    %s: %s
`, testNamespace, runtimekube.LabelTenant, testTenant())
}

func claimYAML(name string, workspaceID id.WorkspaceID, finalizer string) string {
	finalizers := ""
	if finalizer != "" {
		finalizers = "  finalizers:\n    - " + finalizer + "\n"
	}
	return fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  namespace: %s
  labels:
    %s: %s
    %s: %s
%s  annotations: {}
spec:
  accessModes:
    - ReadWriteOnce
  storageClassName: %s
  resources:
    requests:
      storage: 64Mi
`, name, testNamespace, runtimekube.LabelTenant, testTenant(),
		runtimekube.LabelWorkspace, workspaceID, finalizers, kubesupport.StorageClassName)
}

// testIdentity builds one persisted effect identity.
func testIdentity(execution, action, workload, key string) engine.EffectIdentity {
	return engine.EffectIdentity{
		ExecutionID:    id.ExecutionID(execution),
		ActionID:       id.ActionID(action),
		WorkloadID:     id.WorkloadID(workload),
		IdempotencyKey: key,
	}
}

// testContainer builds one admitted container declaration.
func testContainer(commands []string, env []definition.EnvVar) definition.Container {
	return definition.Container{Image: workloadImage, Commands: commands, Env: env}
}

// prepareRequest builds one complete preparation with inputs, an earlier
// output, a task snapshot, one declared secret and the execution credential.
func prepareRequest(
	identity engine.EffectIdentity,
	workspaceID id.WorkspaceID,
	actionName id.ActionName,
	taskID id.TaskID,
	container definition.Container,
	secrets []engine.SecretValue,
	credential string,
) engine.PrepareRequest {
	return engine.PrepareRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
		ActionName:  actionName,
		TaskID:      taskID,
		Container:   container,
		Secrets:     secrets,
		Inputs: map[id.InputKey]string{
			"input-a": "line1\nline2\n",
		},
		Outputs: map[id.ActionName]kernel.Outputs{
			"earlier": {"out-a": "earlier-output"},
		},
		Task:       []byte(`{"id":"task-1"}`),
		Credential: credential,
	}
}

// startPrepared prepares and starts one workload, requiring an accepted start.
func startPrepared(t *testing.T, ctx context.Context, fx *fixture, request engine.PrepareRequest) {
	t.Helper()
	if _, err := fx.Adapter.Prepare(ctx, request); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	start, err := fx.Adapter.Start(ctx, engine.StartRequest{
		Identity:    request.Identity,
		WorkspaceID: request.WorkspaceID,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if start.Outcome != engine.StartAccepted {
		t.Fatalf("start outcome = %q; want accepted (%s)", start.Outcome, start.Detail)
	}
}

// waitForObservation blocks until Observe reports want and returns that
// observation.
func waitForObservation(
	t *testing.T,
	ctx context.Context,
	adapter *runtimekube.Adapter,
	request engine.ObserveRequest,
	want kernel.ObservationKind,
) kernel.Observation {
	t.Helper()
	var last kernel.Observation
	probe := func(ctx context.Context) error {
		observation, err := adapter.Observe(ctx, request)
		if err != nil {
			return err
		}
		last = observation
		if observation.Kind != want {
			return fmt.Errorf("observation = %s; want %s", observation.Kind, want)
		}
		return nil
	}
	options := harness.DefaultWaitOptions()
	options.Timeout = observationTimeout
	options.AttemptTimeout = 15 * time.Second
	options.InitialBackoff = 200 * time.Millisecond
	options.MaxBackoff = 2 * time.Second
	if err := harness.WaitReady(ctx, harness.RealClock{}, "workload observation", probe, options); err != nil {
		t.Fatalf("wait for %s observation: %v (last %+v)", want, err, last)
	}
	return last
}

// onePod returns the single pod carrying the workload label.
func onePod(t *testing.T, ctx context.Context, client *driverkube.Cluster, identity engine.EffectIdentity) runtimekube.Pod {
	t.Helper()
	pods, err := client.ListPods(ctx, testNamespace, map[string]string{
		runtimekube.LabelWorkload: string(identity.WorkloadID),
	})
	if err != nil {
		t.Fatalf("ListPods: %v", err)
	}
	if len(pods) != 1 {
		t.Fatalf("workload pods = %d; want exactly 1 (%+v)", len(pods), pods)
	}
	return pods[0]
}

// waitForPod blocks until exactly one workload pod is observable. Observe can
// report Running from the Job alone (the pod is not scheduled yet), so a test
// that needs the pod must wait for it instead of assuming it exists.
func waitForPod(t *testing.T, ctx context.Context, client *driverkube.Cluster, identity engine.EffectIdentity) runtimekube.Pod {
	t.Helper()
	var last runtimekube.Pod
	probe := func(ctx context.Context) error {
		pods, err := client.ListPods(ctx, testNamespace, map[string]string{
			runtimekube.LabelWorkload: string(identity.WorkloadID),
		})
		if err != nil {
			return err
		}
		if len(pods) != 1 {
			return fmt.Errorf("%d workload pods observed; want exactly 1", len(pods))
		}
		last = pods[0]
		return nil
	}
	options := harness.DefaultWaitOptions()
	options.Timeout = 60 * time.Second
	options.AttemptTimeout = 10 * time.Second
	if err := harness.WaitReady(ctx, harness.RealClock{}, "workload pod", probe, options); err != nil {
		t.Fatalf("wait for workload pod: %v", err)
	}
	return last
}

// waitForJobSettled blocks until the single workload Job reports a terminal
// status. The Job controller updates the Job asynchronously after the pod
// terminates, so a test must not read the Job status once and assume it settled.
func waitForJobSettled(t *testing.T, ctx context.Context, client *driverkube.Cluster, identity engine.EffectIdentity) runtimekube.Job {
	t.Helper()
	var last runtimekube.Job
	probe := func(ctx context.Context) error {
		jobs, err := client.ListJobs(ctx, testNamespace, map[string]string{
			runtimekube.LabelWorkload: string(identity.WorkloadID),
		})
		if err != nil {
			return err
		}
		if len(jobs) != 1 {
			return fmt.Errorf("%d workload jobs observed; want exactly 1", len(jobs))
		}
		last = jobs[0]
		status := jobs[0].Status
		if status.Active != 0 || (status.Succeeded == 0 && status.Failed == 0) {
			return fmt.Errorf("job status is not terminal: %+v", status)
		}
		return nil
	}
	options := harness.DefaultWaitOptions()
	options.Timeout = 60 * time.Second
	options.AttemptTimeout = 10 * time.Second
	if err := harness.WaitReady(ctx, harness.RealClock{}, "workload job status", probe, options); err != nil {
		t.Fatalf("wait for workload job status: %v", err)
	}
	return last
}

// oneJob returns the single Job carrying the workload label.
func oneJob(t *testing.T, ctx context.Context, client *driverkube.Cluster, identity engine.EffectIdentity) runtimekube.Job {
	t.Helper()
	jobs, err := client.ListJobs(ctx, testNamespace, map[string]string{
		runtimekube.LabelWorkload: string(identity.WorkloadID),
	})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("workload jobs = %d; want exactly 1 (%+v)", len(jobs), jobs)
	}
	return jobs[0]
}

// waitForClaim blocks until the named claim is observable and returns it.
func waitForClaim(ctx context.Context, client *driverkube.Cluster, name string) (runtimekube.Claim, error) {
	var last runtimekube.Claim
	probe := func(ctx context.Context) error {
		claim, err := client.GetClaim(ctx, testNamespace, name)
		if err != nil {
			return err
		}
		last = claim
		return nil
	}
	options := harness.DefaultWaitOptions()
	options.Timeout = 60 * time.Second
	options.AttemptTimeout = 10 * time.Second
	if err := harness.WaitReady(ctx, harness.RealClock{}, "claim "+name, probe, options); err != nil {
		return runtimekube.Claim{}, err
	}
	return last, nil
}

// waitForClaimGone blocks until GetClaim reports ErrNotFound.
func waitForClaimGone(ctx context.Context, client *driverkube.Cluster, name string) error {
	probe := func(ctx context.Context) error {
		_, err := client.GetClaim(ctx, testNamespace, name)
		if errors.Is(err, runtimekube.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		return errors.New("claim is still present")
	}
	options := harness.DefaultWaitOptions()
	options.Timeout = 90 * time.Second
	options.AttemptTimeout = 10 * time.Second
	return harness.WaitReady(ctx, harness.RealClock{}, "claim removal "+name, probe, options)
}

// releaseWorkspaceEventually models the Kernel cleanup loop around the runtime
// adapter's Release.
//
// Release issues one accepted, resourceVersion-preconditioned delete and then
// reads the claim back. A real PVC always keeps the kubernetes.io/pvc-protection
// finalizer, so the claim stays terminating for about a second after the delete
// is accepted: that accepted deletion in progress is an in-progress success
// (nil), never a failed attempt. The helper therefore requires Release to
// return nil and then waits for the API server to finish the deletion.
//
// A retry is needed only for a genuine uncertainty, for example a lost delete
// response; those keep the documented fault classes and are retried here. Any
// other fault class, a live claim without a deletion timestamp, or a timeout
// still fails the test.
func releaseWorkspaceEventually(t *testing.T, ctx context.Context, fx *fixture, workspaceID id.WorkspaceID) {
	t.Helper()
	claimName, err := runtimekube.WorkspaceClaimName(workspaceID)
	if err != nil {
		t.Fatalf("WorkspaceClaimName: %v", err)
	}
	deadline := time.Now().Add(releaseTimeout)
	var last error
	for attempt := 1; ; attempt++ {
		last = fx.Adapter.Release(ctx, workspaceID)
		if last == nil {
			if err := waitForClaimGone(ctx, fx.Client, claimName); err != nil {
				t.Fatalf("Release confirmed but claim %s is still present: %v", claimName, err)
			}
			return
		}
		if err := ctx.Err(); err != nil {
			t.Fatalf("Release attempt %d interrupted: %v (last error %v)", attempt, err, last)
		}
		requireRetryableRelease(t, last)
		claim, getErr := fx.Client.GetClaim(ctx, testNamespace, claimName)
		switch {
		case errors.Is(getErr, runtimekube.ErrNotFound):
			// The deletion completed between the adapter's post-delete read and
			// this observation; the next attempt observes ErrNotFound and
			// confirms.
		case getErr != nil:
			t.Fatalf("GetClaim after retryable Release: %v", getErr)
		case claim.DeletionTimestamp == nil:
			t.Fatalf("Release returned retryable %v while claim %s is live with no deletion timestamp", last, claimName)
		}
		if time.Now().After(deadline) {
			t.Fatalf("Release did not converge within %s after %d attempts: last error %v", releaseTimeout, attempt, last)
		}
		timer := time.NewTimer(releasePollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			t.Fatalf("Release retry interrupted: %v (last error %v)", ctx.Err(), last)
		case <-timer.C:
		}
	}
}

// requireRetryableRelease accepts only the documented uncertain Release
// outcome. Any other fault class, or a different message under that class,
// fails: it would mean the adapter reports something the Kernel cleanup loop is
// not specified to retry. An accepted deletion in progress must be nil, not an
// error, so a "terminating" error here is a regression.
func requireRetryableRelease(t *testing.T, err error) {
	t.Helper()
	class, ok := fault.ClassOf(err)
	if !ok {
		t.Fatalf("Release error %v carries no fault class; want a documented retryable outcome", err)
	}
	if class != fault.UncertainExternalEffect {
		t.Fatalf("Release error class %s is not retryable; want UncertainExternalEffect (an accepted deletion in progress is nil) (%v)", class, err)
	}
	if message := err.Error(); !strings.Contains(message, "still present after deletion") {
		t.Fatalf("UncertainExternalEffect Release error %q is not the documented still-present outcome", message)
	}
}

// readAllLogs paginates adapter.Logs with a small limit and returns each stream.
func readAllLogs(
	t *testing.T,
	ctx context.Context,
	adapter *runtimekube.Adapter,
	identity engine.EffectIdentity,
	limit int,
) (stdout, stderr []byte) {
	t.Helper()
	var offset int64
	for page := 0; page < 10000; page++ {
		batch, err := adapter.Logs(ctx, engine.LogsRequest{
			Identity:    identity,
			AfterOffset: offset,
			Limit:       limit,
		})
		if err != nil {
			t.Fatalf("Logs at offset %d: %v", offset, err)
		}
		for _, chunk := range batch.Chunks {
			switch chunk.Stream {
			case engine.LogStdout:
				stdout = append(stdout, chunk.Data...)
			case engine.LogStderr:
				stderr = append(stderr, chunk.Data...)
			}
		}
		if batch.NextOffset == offset {
			return stdout, stderr
		}
		offset = batch.NextOffset
	}
	t.Fatal("log pagination did not terminate")
	return nil, nil
}

// requireFaultClass asserts the stable public fault class.
func requireFaultClass(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil; want fault class %s", want)
	}
	class, ok := fault.ClassOf(err)
	if !ok {
		t.Fatalf("error %v carries no fault class; want %s", err, want)
	}
	if class != want {
		t.Fatalf("fault class = %s; want %s (error %v)", class, want, err)
	}
}

// containsLine reports whether text contains want as a complete line.
func containsLine(text, want string) bool {
	for _, line := range strings.Split(text, "\n") {
		if line == want {
			return true
		}
	}
	return false
}

// runReader runs one short read-only workload in the same retained workspace and
// returns its stdout. It is how tests inspect /workspace without exec.
func runReader(t *testing.T, ctx context.Context, fx *fixture, workspaceID id.WorkspaceID, name string, commands []string) string {
	t.Helper()
	identity := testIdentity("exec-"+name, "action-"+name, "workload-"+name, "idem-"+name)
	startPrepared(t, ctx, fx, prepareRequest(
		identity, workspaceID, id.ActionName("read"), "",
		testContainer(commands, nil), nil, "tok-reader-"+name,
	))
	observation := waitForObservation(t, ctx, fx.Adapter, engine.ObserveRequest{
		Identity: identity, WorkspaceID: workspaceID,
	}, kernel.ObservationExited)
	if code, ok := observation.ExitStatus(); !ok || code != 0 {
		t.Fatalf("reader %q exit = %d/%v; want 0/true", name, code, ok)
	}
	stdout, _ := readAllLogs(t, ctx, fx.Adapter, identity, 1<<20)
	return string(stdout)
}

// waitForWorkspaceUnreferenced blocks until no Job or Pod carries the workspace
// label, so Release has no outstanding reference to refuse.
func waitForWorkspaceUnreferenced(t *testing.T, ctx context.Context, client *driverkube.Cluster, workspaceID id.WorkspaceID) {
	t.Helper()
	selector := map[string]string{runtimekube.LabelWorkspace: string(workspaceID)}
	probe := func(ctx context.Context) error {
		jobs, err := client.ListJobs(ctx, testNamespace, selector)
		if err != nil {
			return err
		}
		if len(jobs) > 0 {
			return fmt.Errorf("%d Job(s) still reference the workspace", len(jobs))
		}
		pods, err := client.ListPods(ctx, testNamespace, selector)
		if err != nil {
			return err
		}
		if len(pods) > 0 {
			return fmt.Errorf("%d Pod(s) still reference the workspace", len(pods))
		}
		return nil
	}
	options := harness.DefaultWaitOptions()
	options.Timeout = 2 * time.Minute
	options.AttemptTimeout = 10 * time.Second
	if err := harness.WaitReady(ctx, harness.RealClock{}, "workspace references", probe, options); err != nil {
		t.Fatalf("workspace still referenced: %v", err)
	}
}

// waitForClaimTerminating blocks until the claim carries a deletion timestamp.
func waitForClaimTerminating(ctx context.Context, client *driverkube.Cluster, name string) (runtimekube.Claim, error) {
	var last runtimekube.Claim
	probe := func(ctx context.Context) error {
		claim, err := client.GetClaim(ctx, testNamespace, name)
		if err != nil {
			return err
		}
		last = claim
		if claim.DeletionTimestamp == nil {
			return errors.New("claim has no deletion timestamp yet")
		}
		return nil
	}
	options := harness.DefaultWaitOptions()
	options.Timeout = 60 * time.Second
	options.AttemptTimeout = 10 * time.Second
	if err := harness.WaitReady(ctx, harness.RealClock{}, "claim deletion "+name, probe, options); err != nil {
		return runtimekube.Claim{}, err
	}
	return last, nil
}
