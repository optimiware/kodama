//go:build component

package kubernetesconformance

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/kernel"
	"github.com/optimiware/kodema/internal/kernel/engine"
	runtimekube "github.com/optimiware/kodema/internal/shell/runtime/kube"
	"github.com/optimiware/kodema/internal/shell/workspace"
)

// TestConfigMapAndSecretRoundTrip proves the real API accepts the driver's
// activation ConfigMap encoding (absolute paths become valid keys), the exact
// binary round trip, the immutable-snapshot conflict and Secret CRUD.
func TestConfigMapAndSecretRoundTrip(t *testing.T) {
	t.Parallel()
	fx, ctx := newFixture(t, id.WorkspaceID("workspace-kube-3"), fixtureOptions{})

	configMap := runtimekube.ConfigMap{
		Name:      "kodema-activation-component",
		Namespace: testNamespace,
		Labels:    map[string]string{runtimekube.LabelWorkload: "workload-k3"},
		Data: map[string][]byte{
			"/kodema/inputs/input-a": []byte("line1\nline2\n"),
			"/kodema/task.json":      {0xff, 0x00, 0x01},
		},
		Immutable: true,
	}
	created, err := fx.Client.PutConfigMap(ctx, configMap)
	if err != nil {
		t.Fatalf("PutConfigMap: %v", err)
	}
	if created.ResourceVersion == "" {
		t.Fatal("created ConfigMap has no resource version")
	}
	if !bytes.Equal(created.Data["/kodema/inputs/input-a"], configMap.Data["/kodema/inputs/input-a"]) ||
		!bytes.Equal(created.Data["/kodema/task.json"], configMap.Data["/kodema/task.json"]) {
		t.Fatalf("ConfigMap data did not round-trip: %v", created.Data)
	}
	read, err := fx.Client.GetConfigMap(ctx, testNamespace, configMap.Name)
	if err != nil {
		t.Fatalf("GetConfigMap: %v", err)
	}
	if !bytes.Equal(read.Data["/kodema/task.json"], []byte{0xff, 0x00, 0x01}) {
		t.Fatalf("binary data changed through the API: %v", read.Data["/kodema/task.json"])
	}
	reused, err := fx.Client.PutConfigMap(ctx, configMap)
	if err != nil {
		t.Fatalf("PutConfigMap identical retry: %v", err)
	}
	if reused.ResourceVersion != created.ResourceVersion {
		t.Fatalf("identical retry created a new object: %q vs %q", reused.ResourceVersion, created.ResourceVersion)
	}
	changed := configMap
	changed.Data = map[string][]byte{"/kodema/task.json": []byte("different")}
	if _, err := fx.Client.PutConfigMap(ctx, changed); !errors.Is(err, runtimekube.ErrConflict) {
		t.Fatalf("changed immutable content error = %v; want ErrConflict", err)
	}
	if err := fx.Client.DeleteConfigMap(ctx, testNamespace, configMap.Name); err != nil {
		t.Fatalf("DeleteConfigMap: %v", err)
	}
	if _, err := fx.Client.GetConfigMap(ctx, testNamespace, configMap.Name); !errors.Is(err, runtimekube.ErrNotFound) {
		t.Fatalf("deleted ConfigMap read = %v; want ErrNotFound", err)
	}

	secret := runtimekube.Secret{
		Name:      "kodema-credential-component",
		Namespace: testNamespace,
		Labels:    map[string]string{runtimekube.LabelWorkload: "workload-k3"},
		Data: map[string][]byte{
			"API_KEY":      []byte("s3cr3t-value"),
			"KODEMA_TOKEN": []byte("tok-component-1"),
		},
	}
	createdSecret, err := fx.Client.PutSecret(ctx, secret)
	if err != nil {
		t.Fatalf("PutSecret: %v", err)
	}
	if string(createdSecret.Data["API_KEY"]) != "s3cr3t-value" {
		t.Fatalf("secret data did not round-trip: %v", createdSecret.Data)
	}
	readSecret, err := fx.Client.GetSecret(ctx, testNamespace, secret.Name)
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	readSecret.Data["KODEMA_TOKEN"] = []byte("tok-component-2")
	updated, err := fx.Client.PutSecret(ctx, readSecret)
	if err != nil {
		t.Fatalf("PutSecret conditional update: %v", err)
	}
	if updated.ResourceVersion == readSecret.ResourceVersion {
		t.Fatal("conditional update did not advance the resource version")
	}
	stale := readSecret
	if _, err := fx.Client.PutSecret(ctx, stale); !errors.Is(err, runtimekube.ErrConflict) {
		t.Fatalf("stale Secret update = %v; want ErrConflict", err)
	}
	if err := fx.Client.DeleteSecret(ctx, testNamespace, secret.Name); err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}
	if _, err := fx.Client.GetSecret(ctx, testNamespace, secret.Name); !errors.Is(err, runtimekube.ErrNotFound) {
		t.Fatalf("deleted Secret read = %v; want ErrNotFound", err)
	}
}

// TestCredentialRefreshReplacesOnlyTheSecret proves a real refresh updates the
// mounted credential Secret while the Job and pod identity stay unchanged.
func TestCredentialRefreshReplacesOnlyTheSecret(t *testing.T) {
	t.Parallel()
	workspaceID := id.WorkspaceID("workspace-kube-4")
	fx, ctx := newFixture(t, workspaceID, fixtureOptions{withClaim: true})
	identity := testIdentity("exec-k4", "action-k4", "workload-k4", "idem-k4")
	startPrepared(t, ctx, fx, prepareRequest(
		identity, workspaceID, "build", "",
		testContainer([]string{`sleep 8`}, nil), nil, "tok-before",
	))
	waitForObservation(t, ctx, fx.Adapter, engine.ObserveRequest{
		Identity: identity, WorkspaceID: workspaceID,
	}, kernel.ObservationRunning)
	jobBefore := oneJob(t, ctx, fx.Client, identity)
	podBefore := waitForPod(t, ctx, fx.Client, identity)

	refresh, err := fx.Adapter.RefreshCredential(ctx, engine.CredentialRequest{
		Identity:   identity,
		Credential: "tok-after",
	})
	if err != nil {
		t.Fatalf("RefreshCredential: %v", err)
	}
	if !refresh.Delivered {
		t.Fatalf("refresh = %+v; want delivered", refresh)
	}

	jobAfter := oneJob(t, ctx, fx.Client, identity)
	// The Job controller advances resourceVersion on status updates while the
	// workload runs, so identity (name and the persisted idempotency
	// annotation) is the stable proof that the refresh did not replace the Job.
	if jobAfter.Name != jobBefore.Name {
		t.Fatalf("credential refresh replaced the Job %q -> %q", jobBefore.Name, jobAfter.Name)
	}
	if jobAfter.Annotations[runtimekube.AnnotationIdempotencyKey] != jobBefore.Annotations[runtimekube.AnnotationIdempotencyKey] {
		t.Fatalf("credential refresh changed the Job idempotency annotation %v -> %v", jobBefore.Annotations, jobAfter.Annotations)
	}
	podAfter := onePod(t, ctx, fx.Client, identity)
	if podAfter.Name != podBefore.Name {
		t.Fatalf("credential refresh replaced the pod %q -> %q", podBefore.Name, podAfter.Name)
	}
	data := secretDataByWorkload(t, ctx, fx, string(identity.WorkloadID))
	if string(data["KODEMA_TOKEN"]) != "tok-after" {
		t.Fatalf("mounted credential = %q; want the refreshed token", data["KODEMA_TOKEN"])
	}
	if string(data["API_KEY"]) != "" {
		t.Fatalf("unexpected declared secret appeared: %v", data)
	}
	waitForObservation(t, ctx, fx.Adapter, engine.ObserveRequest{
		Identity: identity, WorkspaceID: workspaceID,
	}, kernel.ObservationExited)
}

// TestTaskSpecMaterializedInRealPod proves that kubelet projects the immutable
// activation ConfigMap's /kodema/task/{title,description,metadata.json,spec.json}
// files read-only into a real Kubernetes Job pod alongside /kodema/task.json.
func TestTaskSpecMaterializedInRealPod(t *testing.T) {
	t.Parallel()
	workspaceID := id.WorkspaceID("workspace-kube-task-spec")
	fx, ctx := newFixture(t, workspaceID, fixtureOptions{withClaim: true})
	identity := testIdentity("exec-ktask", "action-ktask", "workload-ktask", "idem-ktask")

	taskID := id.TaskID("task-kube-spec")
	taskFiles, err := workspace.BuildTaskFiles(
		taskID,
		"Kubernetes spec title",
		"Multi-line\ndescription ✓",
		map[string]string{"release": "0.6.0", "area": "kube"},
	)
	if err != nil {
		t.Fatalf("BuildTaskFiles: %v", err)
	}
	req := prepareRequest(
		identity, workspaceID, "build", taskID,
		testContainer([]string{
			`test "$(cat /kodema/task/title)" = "Kubernetes spec title"`,
			`test "$(cat /kodema/task/description)" = "$(printf 'Multi-line\ndescription ✓')"`,
			`test "$(cat /kodema/task/metadata.json)" = '{"area":"kube","release":"0.6.0"}'`,
			`test "$(cat /kodema/task/spec.json)" = '{"id":"task-kube-spec","title":"Kubernetes spec title","description":"Multi-line\ndescription ✓","metadata":{"area":"kube","release":"0.6.0"}}'`,
			`test ! -w /kodema/task/title && test ! -w /kodema/task/spec.json`,
			`printf 'kube-task-spec-ok\n'`,
		}, nil),
		nil,
		"tok-ktask",
	)
	req.TaskFiles = taskFiles
	startPrepared(t, ctx, fx, req)

	obs := waitForObservation(t, ctx, fx.Adapter, engine.ObserveRequest{
		Identity: identity, WorkspaceID: workspaceID,
	}, kernel.ObservationExited)
	if code, ok := obs.ExitStatus(); !ok || code != 0 {
		stdout, _ := readAllLogs(t, ctx, fx.Adapter, identity, 1<<20)
		t.Fatalf("pod exit = %d/%v (want 0/true); logs:\n%s", code, ok, stdout)
	}
	stdout, _ := readAllLogs(t, ctx, fx.Adapter, identity, 1<<20)
	if !containsLine(string(stdout), "kube-task-spec-ok") {
		t.Fatalf("pod logs missing kube-task-spec-ok:\n%s", stdout)
	}
}

// secretDataByWorkload reads the single owned credential Secret through kubectl
// and decodes its data. It uses the workload label rather than an unexported
// name derivation.
func secretDataByWorkload(t *testing.T, ctx context.Context, fx *fixture, workloadID string) map[string][]byte {
	t.Helper()
	document, err := fx.Cluster.GetJSON(ctx, "get", "secrets",
		"-l", runtimekube.LabelWorkload+"="+workloadID, "-n", testNamespace)
	if err != nil {
		t.Fatalf("read credential Secret: %v", err)
	}
	items, _ := document["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("owned secrets = %d; want exactly 1", len(items))
	}
	item, _ := items[0].(map[string]any)
	encoded, _ := item["data"].(map[string]any)
	data := make(map[string][]byte, len(encoded))
	for name, value := range encoded {
		text, _ := value.(string)
		decoded, err := base64.StdEncoding.DecodeString(text)
		if err != nil {
			t.Fatalf("decode secret field %q: %v", name, err)
		}
		data[name] = decoded
	}
	return data
}
