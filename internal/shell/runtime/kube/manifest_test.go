// Manifest and preparation behavior: confined activation data, tenant
// ownership, pinned image, fail-fast script, limits and retained workspace.
package kube

import (
	"context"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/kernel/script"
	"github.com/optimiware/kodama/internal/shell/workspace"
)

func testSetup(t *testing.T) (*fakeCluster, *fakeClock, *Adapter) {
	t.Helper()
	fake := newFakeCluster()
	addTestNamespace(fake)
	addTestClaim(t, fake, testWorkspaceID())
	clock := newFakeClock()
	return fake, clock, newTestAdapter(t, fake, clock)
}

func prepareTestWorkload(t *testing.T, adapter *Adapter, request engine.PrepareRequest) engine.Preparation {
	t.Helper()
	preparation, err := adapter.Prepare(context.Background(), request)
	if err != nil {
		t.Fatalf("Prepare returned error: %v", err)
	}
	return preparation
}

func startTestWorkload(t *testing.T, adapter *Adapter, identity engine.EffectIdentity, workspaceID id.WorkspaceID) engine.StartResult {
	t.Helper()
	result, err := adapter.Start(context.Background(), engine.StartRequest{Identity: identity, WorkspaceID: workspaceID})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	return result
}

// TestPrepareMaterializesConfinedActivationSnapshot proves one complete,
// all-or-nothing snapshot outside /workspace: exact input bytes, earlier
// succeeded outputs, the optional task file, and declared secrets plus the
// credential only in the mutable Secret.
func TestPrepareMaterializesConfinedActivationSnapshot(t *testing.T) {
	fake, _, adapter := testSetup(t)
	request := testPrepareRequest()

	preparation := prepareTestWorkload(t, adapter, request)
	if preparation.WorkspaceID != request.WorkspaceID {
		t.Fatalf("preparation workspace = %q; want %q", preparation.WorkspaceID, request.WorkspaceID)
	}
	if preparation.Detail == "" {
		t.Fatal("preparation detail is empty")
	}

	configMaps := fake.configMapsSnapshot()
	if len(configMaps) != 1 {
		t.Fatalf("prepared %d configmaps; want exactly 1", len(configMaps))
	}
	configMap := configMaps[0]
	if configMap.Namespace != testNamespace {
		t.Fatalf("activation configmap namespace = %q; want %q", configMap.Namespace, testNamespace)
	}
	if !configMap.Immutable {
		t.Fatal("activation snapshot configmap is mutable")
	}
	if configMap.Labels[LabelTenant] != string(testTenant()) {
		t.Errorf("activation configmap tenant label = %q", configMap.Labels[LabelTenant])
	}
	if configMap.Labels[LabelWorkspace] != string(request.WorkspaceID) {
		t.Errorf("activation configmap workspace label = %q", configMap.Labels[LabelWorkspace])
	}
	if configMap.Labels[LabelWorkload] != string(request.Identity.WorkloadID) {
		t.Errorf("activation configmap workload label = %q", configMap.Labels[LabelWorkload])
	}
	if len(configMap.Labels[LabelPreparation]) != 48 {
		t.Errorf("activation configmap preparation label = %q (len %d); want 48 hex chars",
			configMap.Labels[LabelPreparation], len(configMap.Labels[LabelPreparation]))
	}

	wantFiles := map[string]string{
		"/kodama/inputs/input-a":      "line1\nline2",
		"/kodama/outputs/build/out-a": "out",
		"/kodama/task.json":           `{"id":"task-1"}`,
	}
	if len(configMap.Data) != len(wantFiles) {
		t.Fatalf("activation configmap has %d files; want %d: %v", len(configMap.Data), len(wantFiles), configMap.Data)
	}
	for path, want := range wantFiles {
		got, ok := configMap.Data[path]
		if !ok {
			t.Errorf("activation configmap is missing %q", path)
			continue
		}
		if string(got) != want {
			t.Errorf("activation file %q = %q; want %q (no appended newline or reinterpretation)", path, got, want)
		}
		if err := workspace.ValidatePath(path); err != nil {
			t.Errorf("activation file %q is not confined: %v", path, err)
		}
		if strings.HasPrefix(path, workspace.WorkspaceRoot) {
			t.Errorf("activation file %q was materialized inside the writable workspace", path)
		}
		if strings.Contains(string(got), request.Credential) {
			t.Errorf("activation file %q contains the execution credential", path)
		}
	}

	secrets := fake.secretsSnapshot()
	if len(secrets) != 1 {
		t.Fatalf("prepared %d secrets; want exactly 1", len(secrets))
	}
	secret := secrets[0]
	if secret.Immutable {
		t.Fatal("credential secret is immutable; credential refresh must be possible without a restart")
	}
	if got := string(secret.Data[id.SecretTokenName]); got != request.Credential {
		t.Errorf("credential secret KODAMA_TOKEN = %q; want the execution credential", got)
	}
	if got := string(secret.Data["API_KEY"]); got != "s3cr3t" {
		t.Errorf("credential secret API_KEY = %q; want %q", got, "s3cr3t")
	}
	if len(secret.Data) != 2 {
		t.Errorf("credential secret has %d keys; want exactly the declared secret and the credential", len(secret.Data))
	}
	if secret.Labels[LabelWorkspace] != string(request.WorkspaceID) {
		t.Errorf("credential secret workspace label = %q", secret.Labels[LabelWorkspace])
	}

	if jobs := fake.jobCount(); jobs != 0 {
		t.Fatalf("Prepare created %d jobs; preparation must never start a workload", jobs)
	}
}

// TestPrepareRefusesMissingWorkspaceClaim proves a missing retained workspace
// is a failure, never permission to create a replacement.
func TestPrepareRefusesMissingWorkspaceClaim(t *testing.T) {
	fake := newFakeCluster()
	addTestNamespace(fake)
	adapter := newTestAdapter(t, fake, newFakeClock())

	_, err := adapter.Prepare(context.Background(), testPrepareRequest())
	if err == nil {
		t.Fatal("Prepare succeeded without the retained workspace claim")
	}
	if len(fake.configMapsSnapshot()) != 0 || len(fake.secretsSnapshot()) != 0 {
		t.Fatal("Prepare materialized activation data without an attached workspace")
	}
	if fake.jobCount() != 0 {
		t.Fatal("Prepare started a workload without a workspace")
	}
}

// TestPrepareRefusesForeignTenantNamespace proves the adapter cannot place a
// tenant's workload in another tenant's namespace.
func TestPrepareRefusesForeignTenantNamespace(t *testing.T) {
	fake := newFakeCluster()
	fake.addNamespace(Namespace{Name: testNamespace, Labels: map[string]string{LabelTenant: "tenant-other"}})
	addTestClaim(t, fake, testWorkspaceID())
	adapter := newTestAdapter(t, fake, newFakeClock())

	_, err := adapter.Prepare(context.Background(), testPrepareRequest())
	if err == nil {
		t.Fatal("Prepare accepted a namespace owned by another tenant")
	}
	class, ok := fault.ClassOf(err)
	if !ok || class != fault.Forbidden {
		t.Fatalf("foreign namespace refusal class = %v (present=%v); want %q", class, ok, fault.Forbidden)
	}
	if len(fake.configMapsSnapshot()) != 0 || len(fake.secretsSnapshot()) != 0 || fake.jobCount() != 0 {
		t.Fatal("Prepare wrote resources into a foreign tenant namespace")
	}
}

// TestPrepareRefusesIncompleteOrUnsafeSnapshot proves partial, credential-less
// and traversal-shaped snapshots never authorize a workload.
func TestPrepareRefusesIncompleteOrUnsafeSnapshot(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*engine.PrepareRequest)
	}{
		{name: "missing credential", mutate: func(request *engine.PrepareRequest) { request.Credential = "" }},
		{name: "traversal input key", mutate: func(request *engine.PrepareRequest) {
			request.Inputs = map[id.InputKey]string{"../escape": "x"}
		}},
		{name: "invalid secret name", mutate: func(request *engine.PrepareRequest) {
			request.Secrets = []engine.SecretValue{{Name: "bad name", Value: "x"}}
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fake, _, adapter := testSetup(t)
			request := testPrepareRequest()
			testCase.mutate(&request)

			if _, err := adapter.Prepare(context.Background(), request); err == nil {
				t.Fatalf("Prepare accepted a snapshot with %s", testCase.name)
			}
			if len(fake.configMapsSnapshot()) != 0 || len(fake.secretsSnapshot()) != 0 {
				t.Fatalf("Prepare partially materialized a snapshot with %s", testCase.name)
			}
			if fake.jobCount() != 0 {
				t.Fatalf("Prepare started a workload with %s", testCase.name)
			}
		})
	}
}

// TestPrepareRefusesInvalidActionContext proves an unusable action slug or task
// identity is refused with InvalidInput before any cluster read or write.
func TestPrepareRefusesInvalidActionContext(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*engine.PrepareRequest)
	}{
		{name: "empty action name", mutate: func(request *engine.PrepareRequest) { request.ActionName = "" }},
		{name: "invalid action name", mutate: func(request *engine.PrepareRequest) { request.ActionName = "Build Mode" }},
		{name: "invalid task id", mutate: func(request *engine.PrepareRequest) { request.TaskID = "../escape" }},
		{name: "empty image", mutate: func(request *engine.PrepareRequest) { request.Container.Image = "   " }},
		{name: "invalid env name", mutate: func(request *engine.PrepareRequest) {
			request.Container.Env = []definition.EnvVar{{Name: "BAD NAME", Value: "x"}}
		}},
		{name: "empty commands", mutate: func(request *engine.PrepareRequest) { request.Container.Commands = nil }},
		{name: "nul byte in command", mutate: func(request *engine.PrepareRequest) {
			request.Container.Commands = []string{"echo\x00bad"}
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fake, _, adapter := testSetup(t)
			request := testPrepareRequest()
			testCase.mutate(&request)

			_, err := adapter.Prepare(context.Background(), request)
			class, ok := fault.ClassOf(err)
			if !ok || class != fault.InvalidInput {
				t.Fatalf("Prepare error class = (%v, %v) for %v; want %q", class, ok, err, fault.InvalidInput)
			}
			if len(fake.configMapsSnapshot()) != 0 || len(fake.secretsSnapshot()) != 0 || fake.jobCount() != 0 {
				t.Fatalf("Prepare performed an effect with %s", testCase.name)
			}
		})
	}
}

// TestPrepareIsIdempotentAndRefusesChangedContent proves the same persisted
// identity may be retried with identical content but never silently rebound to
// different activation data.
func TestPrepareIsIdempotentAndRefusesChangedContent(t *testing.T) {
	fake, _, adapter := testSetup(t)
	request := testPrepareRequest()
	prepareTestWorkload(t, adapter, request)

	if _, err := adapter.Prepare(context.Background(), request); err != nil {
		t.Fatalf("identical Prepare retry returned error: %v", err)
	}
	if len(fake.configMapsSnapshot()) != 1 || len(fake.secretsSnapshot()) != 1 {
		t.Fatal("identical Prepare retry duplicated activation resources")
	}

	changed := request
	changed.Inputs = map[id.InputKey]string{"input-a": "different"}
	_, err := adapter.Prepare(context.Background(), changed)
	if err == nil {
		t.Fatal("Prepare accepted different content for the same workload identity")
	}
	class, ok := fault.ClassOf(err)
	if !ok || class != fault.ChangedIdempotencyContent {
		t.Fatalf("changed-content refusal class = %v (present=%v); want %q", class, ok, fault.ChangedIdempotencyContent)
	}
	configMap := fake.configMapsSnapshot()[0]
	if string(configMap.Data["/kodama/inputs/input-a"]) != "line1\nline2" {
		t.Fatal("changed Prepare overwrote the original activation snapshot")
	}

	declarationMutations := []struct {
		name      string
		wantClass fault.Class
		mutate    func(*fakeCluster, *engine.PrepareRequest)
	}{
		{
			name:      "changed commands",
			wantClass: fault.ChangedIdempotencyContent,
			mutate: func(_ *fakeCluster, req *engine.PrepareRequest) {
				req.Container.Commands = []string{"echo changed"}
			},
		},
		{
			name:      "changed env",
			wantClass: fault.ChangedIdempotencyContent,
			mutate: func(_ *fakeCluster, req *engine.PrepareRequest) {
				req.Container.Env = []definition.EnvVar{{Name: "BUILD_MODE", Value: "debug"}}
			},
		},
		{
			name:      "changed image",
			wantClass: fault.ChangedIdempotencyContent,
			mutate: func(_ *fakeCluster, req *engine.PrepareRequest) {
				req.Container.Image = "alpine:3.24"
			},
		},
		{
			name:      "changed idempotency key",
			wantClass: fault.ChangedIdempotencyContent,
			mutate: func(_ *fakeCluster, req *engine.PrepareRequest) {
				req.Identity.IdempotencyKey = "idem-changed"
			},
		},
		{
			name:      "changed workspace",
			wantClass: fault.Forbidden,
			mutate: func(cluster *fakeCluster, req *engine.PrepareRequest) {
				other := id.WorkspaceID("workspace-other")
				addTestClaim(t, cluster, other)
				req.WorkspaceID = other
			},
		},
	}

	for _, mode := range []string{"in_memory", "after_restart"} {
		for _, tc := range declarationMutations {
			t.Run(mode+"_"+tc.name, func(t *testing.T) {
				cluster, clock, first := testSetup(t)
				base := testPrepareRequest()
				prepareTestWorkload(t, first, base)

				target := first
				if mode == "after_restart" {
					target = newTestAdapter(t, cluster, clock)
				}
				secretPutsBefore := cluster.callCount("PutSecret")

				mutated := testPrepareRequest()
				mutated.Credential = "tok-rotated-attempt"
				tc.mutate(cluster, &mutated)

				_, retryErr := target.Prepare(context.Background(), mutated)
				gotClass, present := fault.ClassOf(retryErr)
				if !present || gotClass != tc.wantClass {
					t.Fatalf("Prepare retry class = (%v, %v) for %v; want %q", gotClass, present, retryErr, tc.wantClass)
				}
				if cluster.callCount("PutSecret") != secretPutsBefore {
					t.Fatal("refused Prepare retry mutated the credential Secret")
				}
				if got := string(cluster.secretsSnapshot()[0].Data[id.SecretTokenName]); got != base.Credential {
					t.Fatalf("credential Secret after refused retry = %q; want original %q", got, base.Credential)
				}
			})
		}
	}

	t.Run("accepts tag image and clears prepared entry on confirmed remove", func(t *testing.T) {
		cluster, _, localAdapter := testSetup(t)
		tagged := testPrepareRequest()
		tagged.Container.Image = "alpine:3.24"
		prepareTestWorkload(t, localAdapter, tagged)

		removal, removeErr := localAdapter.Remove(context.Background(), engine.RemoveRequest{
			Identity:    tagged.Identity,
			WorkspaceID: tagged.WorkspaceID,
		})
		if removeErr != nil || !removal.Removed {
			t.Fatalf("Remove = (%+v, %v); want Removed=true", removal, removeErr)
		}
		localAdapter.mu.Lock()
		_, stillPrepared := localAdapter.prepared[tagged.Identity.WorkloadID]
		localAdapter.mu.Unlock()
		if stillPrepared {
			t.Fatal("confirmed Remove left the workload in Adapter.prepared")
		}
		if len(cluster.configMapsSnapshot()) != 0 {
			t.Fatal("confirmed Remove left ConfigMap behind")
		}
	})
}

// TestStartBuildsPinnedFailFastJob pins the generated Job manifest: pinned
// image, fail-fast script, tenant ownership/limits, no replay settings, the
// retained workspace claim and confined read-only activation mounts.
func TestStartBuildsPinnedFailFastJob(t *testing.T) {
	fake, _, adapter := testSetup(t)
	request := testPrepareRequest()
	prepareTestWorkload(t, adapter, request)

	result := startTestWorkload(t, adapter, request.Identity, request.WorkspaceID)
	if result.Outcome != engine.StartAccepted {
		t.Fatalf("start outcome = %q; want %q", result.Outcome, engine.StartAccepted)
	}
	if result.WorkloadID != request.Identity.WorkloadID {
		t.Fatalf("start workload id = %q; want %q", result.WorkloadID, request.Identity.WorkloadID)
	}

	jobs := fake.jobsSnapshot()
	if len(jobs) != 1 {
		t.Fatalf("Start created %d jobs; want exactly 1", len(jobs))
	}
	job := jobs[0]
	spec := job.Spec

	if job.Namespace != testNamespace {
		t.Errorf("job namespace = %q; want the tenant namespace %q", job.Namespace, testNamespace)
	}
	if job.Name == "" {
		t.Error("job has no stable name")
	}
	wantLabels := map[string]string{
		LabelTenant:    string(testTenant()),
		LabelExecution: string(request.Identity.ExecutionID),
		LabelAction:    string(request.Identity.ActionID),
		LabelWorkload:  string(request.Identity.WorkloadID),
		LabelWorkspace: string(request.WorkspaceID),
	}
	for key, want := range wantLabels {
		if got := job.Labels[key]; got != want {
			t.Errorf("job label %q = %q; want %q", key, got, want)
		}
		if got := spec.PodLabels[key]; got != want {
			t.Errorf("pod label %q = %q; want %q (the tenant network policy selects it)", key, got, want)
		}
	}
	if got := job.Annotations[AnnotationIdempotencyKey]; got != request.Identity.IdempotencyKey {
		t.Errorf("job idempotency annotation = %q; want %q", got, request.Identity.IdempotencyKey)
	}

	if spec.Image != testImage {
		t.Errorf("job image = %q; want the pinned digest %q", spec.Image, testImage)
	}
	rendered := testJobScript(t, spec.Command)
	wantScript, err := script.Render(request.Container.Commands)
	if err != nil {
		t.Fatalf("script.Render returned error: %v", err)
	}
	if rendered != wantScript {
		t.Fatalf("fail-fast script = %q; want %q", rendered, wantScript)
	}
	if strings.Contains(rendered, "set -x") {
		t.Error("fail-fast script enables tracing")
	}

	if spec.RestartPolicy != "Never" {
		t.Errorf("restart policy = %q; want Never", spec.RestartPolicy)
	}
	if spec.BackoffLimit != 0 {
		t.Errorf("backoff limit = %d; a Job must not retry the command", spec.BackoffLimit)
	}
	if spec.PodReplacementPolicy != "Failed" {
		t.Errorf("pod replacement policy = %q; want Failed so a running pod is never replaced", spec.PodReplacementPolicy)
	}
	if spec.TTLSecondsAfterFinished == nil {
		t.Error("job has no TTL; finished Jobs must not accumulate")
	} else if *spec.TTLSecondsAfterFinished != 600 {
		t.Errorf("job TTL = %d; want the configured 600", *spec.TTLSecondsAfterFinished)
	}
	if spec.AutomountServiceAccountToken {
		t.Error("job automounts a service-account token")
	}
	if spec.HostNetwork {
		t.Error("job uses the host network; tenant network isolation is broken")
	}
	wantResources := Resources{
		CPURequest:    "100m",
		CPULimit:      "1",
		MemoryRequest: "64Mi",
		MemoryLimit:   "256Mi",
	}
	if spec.Resources != wantResources {
		t.Errorf("job resources = %+v; want %+v", spec.Resources, wantResources)
	}

	if got, ok := testEnvValue(spec.Env, id.EnvURL); !ok || got != testKodamaURL {
		t.Errorf("%s = %q (present=%v); want %q", id.EnvURL, got, ok, testKodamaURL)
	}
	if got, ok := testEnvValue(spec.Env, id.EnvExecution); !ok || got != string(request.Identity.ExecutionID) {
		t.Errorf("%s = %q (present=%v); want %q", id.EnvExecution, got, ok, request.Identity.ExecutionID)
	}
	if got, ok := testEnvValue(spec.Env, id.EnvAction); !ok || got != string(request.ActionName) {
		t.Errorf("%s = %q (present=%v); want the action slug %q", id.EnvAction, got, ok, request.ActionName)
	}
	if got, ok := testEnvValue(spec.Env, id.EnvAction); ok && got == string(request.Identity.ActionID) {
		t.Errorf("%s = %q; must be the slug, not the action id %q", id.EnvAction, got, request.Identity.ActionID)
	}
	if got, ok := testEnvValue(spec.Env, id.EnvTask); !ok || got != string(request.TaskID) {
		t.Errorf("%s = %q (present=%v); want the task id %q", id.EnvTask, got, ok, request.TaskID)
	}
	if got, ok := testEnvValue(spec.Env, "BUILD_MODE"); !ok || got != "release" {
		t.Errorf("literal environment BUILD_MODE = %q (present=%v); want release", got, ok)
	}

	claimName := testClaimName(t, request.WorkspaceID)
	workspaceVolume := testVolumeAt(t, spec, workspace.WorkspaceRoot)
	if workspaceVolume.ClaimName != claimName {
		t.Errorf("workspace volume claim = %q; want the retained claim %q", workspaceVolume.ClaimName, claimName)
	}
	if workspaceVolume.ReadOnly {
		t.Error("workspace volume is read-only")
	}

	configMap := fake.configMapsSnapshot()[0]
	activationVolume := testVolumeAt(t, spec, "/kodama")
	if !activationVolume.ReadOnly {
		t.Error("activation volume is writable")
	}
	if activationVolume.ConfigMapName != configMap.Name {
		t.Errorf("activation volume configmap = %q; want %q", activationVolume.ConfigMapName, configMap.Name)
	}
	if len(activationVolume.Items) != len(configMap.Data) {
		t.Fatalf("activation volume has %d items; want %d", len(activationVolume.Items), len(configMap.Data))
	}
	for _, item := range activationVolume.Items {
		if _, ok := configMap.Data[item.Path]; !ok {
			t.Errorf("activation volume item path %q is not in the snapshot", item.Path)
		}
		if err := workspace.ValidatePath(item.Path); err != nil {
			t.Errorf("activation volume item path %q is not confined: %v", item.Path, err)
		}
		if item.Mode&0o222 != 0 {
			t.Errorf("activation volume item %q mode %#o is writable", item.Path, item.Mode)
		}
		if strings.HasPrefix(item.Path, workspace.WorkspaceRoot) {
			t.Errorf("activation volume item %q mounts inside the writable workspace", item.Path)
		}
	}

	secret := fake.secretsSnapshot()[0]
	credentialVolume := testVolumeAt(t, spec, workspace.SecretRoot)
	if !credentialVolume.ReadOnly {
		t.Error("credential volume is writable")
	}
	if credentialVolume.SecretName != secret.Name {
		t.Errorf("credential volume secret = %q; want %q", credentialVolume.SecretName, secret.Name)
	}
	wantSecretPaths := map[string]string{
		"API_KEY":          workspace.SecretRoot + "/API_KEY",
		id.SecretTokenName: workspace.ReservedCredentialPath,
	}
	if len(credentialVolume.Items) != len(wantSecretPaths) {
		t.Fatalf("credential volume has %d items; want %d", len(credentialVolume.Items), len(wantSecretPaths))
	}
	for _, item := range credentialVolume.Items {
		want, ok := wantSecretPaths[item.Key]
		if !ok {
			t.Errorf("credential volume item key %q is not a declared secret", item.Key)
			continue
		}
		if item.Path != want {
			t.Errorf("credential volume item %q path = %q; want %q", item.Key, item.Path, want)
		}
		if item.Mode&0o222 != 0 {
			t.Errorf("credential volume item %q mode %#o is writable", item.Path, item.Mode)
		}
	}
}

// TestStartOmitsTaskContextWhenTaskFree proves task-free workloads mount no
// task snapshot and inject no task environment.
func TestStartOmitsTaskContextWhenTaskFree(t *testing.T) {
	fake, _, adapter := testSetup(t)
	request := testPrepareRequest()
	request.Task = nil
	request.TaskID = ""
	prepareTestWorkload(t, adapter, request)
	startTestWorkload(t, adapter, request.Identity, request.WorkspaceID)

	configMap := fake.configMapsSnapshot()[0]
	if _, ok := configMap.Data[workspace.TaskPath]; ok {
		t.Fatalf("task-free workload mounted %q", workspace.TaskPath)
	}
	job := fake.jobsSnapshot()[0]
	if _, ok := testEnvValue(job.Spec.Env, id.EnvTask); ok {
		t.Fatal("task-free workload injected KODAMA_TASK")
	}
}

// TestStartRefusesWithoutCompletePreparation proves Start never prepares
// implicitly and never starts against a vanished workspace or a preparation
// bound to another workspace.
func TestStartRefusesWithoutCompletePreparation(t *testing.T) {
	t.Run("no preparation", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		_, err := adapter.Start(context.Background(), engine.StartRequest{
			Identity:    testIdentity(),
			WorkspaceID: testWorkspaceID(),
		})
		if err == nil {
			t.Fatal("Start succeeded without a preparation snapshot")
		}
		if fake.jobCount() != 0 {
			t.Fatal("Start created a job without a preparation snapshot")
		}
	})

	t.Run("workspace vanished after preparation", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		request := testPrepareRequest()
		prepareTestWorkload(t, adapter, request)
		fake.removeClaim(testNamespace, testClaimName(t, request.WorkspaceID))

		_, err := adapter.Start(context.Background(), engine.StartRequest{
			Identity:    request.Identity,
			WorkspaceID: request.WorkspaceID,
		})
		if err == nil {
			t.Fatal("Start succeeded after the retained workspace disappeared")
		}
		if fake.jobCount() != 0 {
			t.Fatal("Start created a job against a missing workspace")
		}
		if fake.claimExists(testNamespace, testClaimName(t, request.WorkspaceID)) {
			t.Fatal("Start recreated the missing workspace claim")
		}
	})

	t.Run("preparation bound to another workspace", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		otherWorkspace := id.WorkspaceID("workspace-2")
		addTestClaim(t, fake, otherWorkspace)
		request := testPrepareRequest()
		prepareTestWorkload(t, adapter, request)

		_, err := adapter.Start(context.Background(), engine.StartRequest{
			Identity:    request.Identity,
			WorkspaceID: otherWorkspace,
		})
		if err == nil {
			t.Fatal("Start accepted a workspace different from the prepared one")
		}
		if fake.jobCount() != 0 {
			t.Fatal("Start created a job bound to a different workspace")
		}
	})
}

// testJobScript extracts the fail-fast script from the pinned shell invocation.
func testJobScript(t *testing.T, command []string) string {
	t.Helper()
	if len(command) < 3 || command[0] != "/bin/sh" {
		t.Fatalf("job command = %v; want a /bin/sh fail-fast invocation", command)
	}
	switch command[1] {
	case "-ec":
		return command[2]
	case "-e":
		if len(command) >= 4 && command[2] == "-c" {
			return command[3]
		}
	}
	t.Fatalf("job command = %v; want a fail-fast -e/-c invocation", command)
	return ""
}

func testEnvValue(env []EnvVar, name string) (string, bool) {
	for _, entry := range env {
		if entry.Name == name {
			return entry.Value, true
		}
	}
	return "", false
}

func testVolumeAt(t *testing.T, spec JobSpec, mountPath string) Volume {
	t.Helper()
	for _, volume := range spec.Volumes {
		if volume.MountPath == mountPath {
			return volume
		}
	}
	t.Fatalf("job has no volume mounted at %q: %+v", mountPath, spec.Volumes)
	return Volume{}
}
