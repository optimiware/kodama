// REST mapping and error-classification specification for the production
// Kubernetes Cluster driver. Every test drives a TLS httptest API server and
// asserts the exact request the driver sends and the exact value it returns.
package kube_test

import (
	"context"
	"errors"
	"io"
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	driverkube "github.com/optimiware/kodama/internal/shell/app/drivers/kube"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
)

const (
	testImage = "registry.example.com/tools/build@sha256:" +
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	// testConfigMapKey is base64url("/kodama/inputs/input-a") without padding.
	testConfigMapKey = "L2tvZGFtYS9pbnB1dHMvaW5wdXQtYQ"
)

func namespacePath() string {
	return "/api/v1/namespaces/" + testNamespace
}

func jobsPath() string {
	return "/apis/batch/v1/namespaces/" + testNamespace + "/jobs"
}

func jobPath(name string) string {
	return jobsPath() + "/" + name
}

func podsPath() string {
	return namespacePath() + "/pods"
}

func podLogPath(pod string) string {
	return podsPath() + "/" + pod + "/log"
}

func configMapsPath() string {
	return namespacePath() + "/configmaps"
}

func configMapPath(name string) string {
	return configMapsPath() + "/" + name
}

func secretsPath() string {
	return namespacePath() + "/secrets"
}

func secretPath(name string) string {
	return secretsPath() + "/" + name
}

func claimsPath() string {
	return namespacePath() + "/persistentvolumeclaims"
}

func claimPath(name string) string {
	return claimsPath() + "/" + name
}

func testJobLabels() map[string]string {
	return map[string]string{
		runtimekube.LabelTenant:    testNamespace,
		runtimekube.LabelExecution: "exec-1",
		runtimekube.LabelAction:    "build",
		runtimekube.LabelWorkload:  "workload-1",
		runtimekube.LabelWorkspace: "workspace-1",
	}
}

// testJob is the exact seam Job the adapter builds for one prepared workload.
func testJob() runtimekube.Job {
	ttl := 600
	labels := testJobLabels()
	podLabels := make(map[string]string, len(labels))
	maps.Copy(podLabels, labels)
	return runtimekube.Job{
		Name:        "kodama-job-abc",
		Namespace:   testNamespace,
		Labels:      labels,
		Annotations: map[string]string{runtimekube.AnnotationIdempotencyKey: "idem-1"},
		Spec: runtimekube.JobSpec{
			Image:   testImage,
			Command: []string{"/bin/sh", "-ec", "echo hi"},
			Env: []runtimekube.EnvVar{
				{Name: "BUILD_MODE", Value: "release"},
				{Name: "KODAMA_URL", Value: "https://kodama.example.com"},
			},
			Resources: runtimekube.Resources{
				CPURequest:    "100m",
				CPULimit:      "1",
				MemoryRequest: "64Mi",
				MemoryLimit:   "256Mi",
			},
			Volumes: []runtimekube.Volume{
				{Name: "workspace", MountPath: "/workspace", ClaimName: "kodama-workspace-abc"},
				{
					Name:          "activation",
					MountPath:     "/kodama",
					ConfigMapName: "kodama-activation-abc",
					ReadOnly:      true,
					Items: []runtimekube.VolumeItem{
						{Key: "/kodama/inputs/input-a", Path: "/kodama/inputs/input-a", Mode: 0o444},
					},
				},
				{
					Name:       "credential",
					MountPath:  "/kodama/secrets",
					SecretName: "kodama-credential-abc",
					ReadOnly:   true,
					Items: []runtimekube.VolumeItem{
						{Key: "API_KEY", Path: "/kodama/secrets/API_KEY", Mode: 0o444},
					},
				},
			},
			RestartPolicy:                "Never",
			BackoffLimit:                 0,
			PodReplacementPolicy:         "Failed",
			TTLSecondsAfterFinished:      &ttl,
			AutomountServiceAccountToken: false,
			HostNetwork:                  false,
			PodLabels:                    podLabels,
		},
	}
}

// testJobJSON is the exact REST body the driver must send for testJob.
const testJobJSON = `{
  "apiVersion": "batch/v1",
  "kind": "Job",
  "metadata": {
    "name": "kodama-job-abc",
    "namespace": "tenant-acme",
    "labels": {
      "kodama.dev/tenant": "tenant-acme",
      "kodama.dev/execution": "exec-1",
      "kodama.dev/action": "build",
      "kodama.dev/workload": "workload-1",
      "kodama.dev/workspace": "workspace-1"
    },
    "annotations": {"kodama.dev/idempotency-key": "idem-1"}
  },
  "spec": {
    "backoffLimit": 0,
    "podReplacementPolicy": "Failed",
    "ttlSecondsAfterFinished": 600,
    "template": {
      "metadata": {
        "labels": {
          "kodama.dev/tenant": "tenant-acme",
          "kodama.dev/execution": "exec-1",
          "kodama.dev/action": "build",
          "kodama.dev/workload": "workload-1",
          "kodama.dev/workspace": "workspace-1"
        }
      },
      "spec": {
        "restartPolicy": "Never",
        "automountServiceAccountToken": false,
        "hostNetwork": false,
        "containers": [
          {
            "name": "workload",
            "image": "` + testImage + `",
            "command": ["/bin/sh", "-ec", "echo hi"],
            "env": [
              {"name": "BUILD_MODE", "value": "release"},
              {"name": "KODAMA_URL", "value": "https://kodama.example.com"}
            ],
            "resources": {
              "requests": {"cpu": "100m", "memory": "64Mi"},
              "limits": {"cpu": "1", "memory": "256Mi"}
            },
            "volumeMounts": [
              {"name": "workspace", "mountPath": "/workspace"},
              {"name": "activation", "mountPath": "/kodama", "readOnly": true},
              {"name": "credential", "mountPath": "/kodama/secrets", "readOnly": true}
            ]
          }
        ],
        "volumes": [
          {"name": "workspace", "persistentVolumeClaim": {"claimName": "kodama-workspace-abc"}},
          {
            "name": "activation",
            "configMap": {
              "name": "kodama-activation-abc",
              "items": [{"key": "` + testConfigMapKey + `", "path": "inputs/input-a", "mode": 292}]
            }
          },
          {
            "name": "credential",
            "secret": {
              "secretName": "kodama-credential-abc",
              "items": [{"key": "API_KEY", "path": "API_KEY", "mode": 292}]
            }
          }
        ]
      }
    }
  }
}`

// TestGetNamespaceMapsMetadataAndNotFound pins the namespace GET path, the
// label mapping and the 404 sentinel.
func TestGetNamespaceMapsMetadataAndNotFound(t *testing.T) {
	status := http.StatusOK
	api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != namespacePath() {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.URL.RawQuery != "" {
			t.Errorf("namespace GET carries query %q; want none", r.URL.RawQuery)
		}
		if status == http.StatusNotFound {
			writeJSON(w, http.StatusNotFound, map[string]any{"kind": "Status", "reason": "NotFound"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"metadata": map[string]any{
				"name":   testNamespace,
				"labels": map[string]string{runtimekube.LabelTenant: testNamespace},
			},
		})
	})
	cluster := newClusterFor(t, api)

	namespace, err := cluster.GetNamespace(context.Background(), testNamespace)
	if err != nil {
		t.Fatalf("GetNamespace: %v", err)
	}
	if namespace.Name != testNamespace || namespace.Labels[runtimekube.LabelTenant] != testNamespace {
		t.Fatalf("namespace = %+v; want the labelled tenant namespace", namespace)
	}

	status = http.StatusNotFound
	if _, err := cluster.GetNamespace(context.Background(), testNamespace); !errors.Is(err, runtimekube.ErrNotFound) {
		t.Fatalf("404 error = %v; want ErrNotFound", err)
	}
}

// TestCreateJobSendsPinnedManifest proves the complete Job REST body, including
// the encoded activation keys, relative item paths, security defaults and
// resource policy, and parses the created object.
func TestCreateJobSendsPinnedManifest(t *testing.T) {
	api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != jobsPath() {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if contentType := r.Header.Get("Content-Type"); contentType != "application/json" {
			t.Errorf("Content-Type = %q; want application/json", contentType)
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"metadata": map[string]any{
				"name":            "kodama-job-abc",
				"namespace":       testNamespace,
				"resourceVersion": "42",
				"labels":          testJobLabels(),
				"annotations":     map[string]string{runtimekube.AnnotationIdempotencyKey: "idem-1"},
			},
			"status": map[string]any{
				"active":    1,
				"succeeded": 0,
				"failed":    0,
				"startTime": "2026-11-06T10:00:00Z",
			},
		})
	})
	cluster := newClusterFor(t, api)

	job := testJob()
	created, err := cluster.CreateJob(context.Background(), job)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	call := api.lastCall(t)
	assertJSONEqual(t, call.Body, testJobJSON)

	if created.Name != "kodama-job-abc" || created.ResourceVersion != "42" {
		t.Fatalf("created job = %+v; want the server identity", created)
	}
	if created.Status.Active != 1 || created.Status.Succeeded != 0 || created.Status.Failed != 0 {
		t.Fatalf("created status = %+v; want active 1", created.Status)
	}
	start, err := time.Parse(time.RFC3339, "2026-11-06T10:00:00Z")
	if err != nil {
		t.Fatalf("parse fixture time: %v", err)
	}
	if created.Status.StartTime == nil || !created.Status.StartTime.Equal(start) {
		t.Fatalf("created start time = %v; want %s", created.Status.StartTime, start)
	}
	if len(api.snapshot()) != 1 {
		t.Fatalf("create made %d requests; want exactly 1", len(api.snapshot()))
	}
}

// TestCreateJobConflictIsAlreadyExistsWithoutRetry proves a create conflict is
// mapped to the sentinel the adapter uses to reuse the stable Job, and that the
// driver never retries a create on its own.
func TestCreateJobConflictIsAlreadyExistsWithoutRetry(t *testing.T) {
	api := newAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusConflict, map[string]any{"kind": "Status", "reason": "AlreadyExists"})
	})
	cluster := newClusterFor(t, api)

	if _, err := cluster.CreateJob(context.Background(), testJob()); !errors.Is(err, runtimekube.ErrAlreadyExists) {
		t.Fatalf("409 error = %v; want ErrAlreadyExists", err)
	}
	if calls := api.callCount(); calls != 1 {
		t.Fatalf("create made %d requests; want exactly 1 (no internal retry)", calls)
	}
}

// TestCreateJobRetrySendsIdenticalImmutableRequest proves a retry serializes the
// same request and never mutates the caller's Job value.
func TestCreateJobRetrySendsIdenticalImmutableRequest(t *testing.T) {
	api := newAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusConflict, map[string]any{"kind": "Status", "reason": "AlreadyExists"})
	})
	cluster := newClusterFor(t, api)

	job := testJob()
	originalLabels := make(map[string]string, len(job.Labels))
	maps.Copy(originalLabels, job.Labels)
	if _, err := cluster.CreateJob(context.Background(), job); !errors.Is(err, runtimekube.ErrAlreadyExists) {
		t.Fatalf("first create error = %v; want ErrAlreadyExists", err)
	}
	if _, err := cluster.CreateJob(context.Background(), job); !errors.Is(err, runtimekube.ErrAlreadyExists) {
		t.Fatalf("second create error = %v; want ErrAlreadyExists", err)
	}

	calls := api.snapshot()
	if len(calls) != 2 {
		t.Fatalf("create made %d requests; want 2", len(calls))
	}
	if string(calls[0].Body) != string(calls[1].Body) {
		t.Fatalf("retry body differs from the first body\nfirst: %s\nsecond: %s", calls[0].Body, calls[1].Body)
	}
	if job.ResourceVersion != "" {
		t.Fatalf("CreateJob mutated the caller's resource version to %q", job.ResourceVersion)
	}
	for key, value := range originalLabels {
		if job.Labels[key] != value {
			t.Fatalf("CreateJob mutated caller label %q", key)
		}
	}
	if len(job.Labels) != len(originalLabels) {
		t.Fatalf("CreateJob mutated the caller's label set")
	}
}

// TestGetJobMapsStatusAndNotFound proves the Job GET path, status mapping and
// the 404 sentinel.
func TestGetJobMapsStatusAndNotFound(t *testing.T) {
	status := http.StatusOK
	api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != jobPath("kodama-job-abc") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if status == http.StatusNotFound {
			writeJSON(w, http.StatusNotFound, map[string]any{"kind": "Status", "reason": "NotFound"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"metadata": map[string]any{
				"name":            "kodama-job-abc",
				"namespace":       testNamespace,
				"resourceVersion": "43",
				"labels":          testJobLabels(),
				"annotations":     map[string]string{runtimekube.AnnotationIdempotencyKey: "idem-1"},
			},
			"status": map[string]any{
				"active":         0,
				"succeeded":      1,
				"failed":         0,
				"startTime":      "2026-11-06T10:00:00Z",
				"completionTime": "2026-11-06T10:00:09Z",
			},
		})
	})
	cluster := newClusterFor(t, api)

	job, err := cluster.GetJob(context.Background(), testNamespace, "kodama-job-abc")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Name != "kodama-job-abc" || job.ResourceVersion != "43" {
		t.Fatalf("job identity = %+v", job)
	}
	if job.Status.Succeeded != 1 || job.Status.Active != 0 {
		t.Fatalf("job status = %+v; want succeeded 1", job.Status)
	}
	if job.Annotations[runtimekube.AnnotationIdempotencyKey] != "idem-1" {
		t.Fatalf("job annotations = %v; want the idempotency key", job.Annotations)
	}
	if job.Status.CompletionTime == nil || !job.Status.CompletionTime.Equal(mustTime(t, "2026-11-06T10:00:09Z")) {
		t.Fatalf("completion time = %v; want the observed completion", job.Status.CompletionTime)
	}

	status = http.StatusNotFound
	if _, err := cluster.GetJob(context.Background(), testNamespace, "kodama-job-abc"); !errors.Is(err, runtimekube.ErrNotFound) {
		t.Fatalf("404 error = %v; want ErrNotFound", err)
	}
}

// TestListJobsEncodesSelectorAndMapsItems proves the label selector encoding and
// the Job list mapping.
func TestListJobsEncodesSelectorAndMapsItems(t *testing.T) {
	api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != jobsPath() {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"items": []any{
				map[string]any{
					"metadata": map[string]any{
						"name":            "kodama-job-abc",
						"namespace":       testNamespace,
						"resourceVersion": "44",
						"labels":          testJobLabels(),
					},
					"status": map[string]any{"active": 1},
				},
			},
		})
	})
	cluster := newClusterFor(t, api)

	jobs, err := cluster.ListJobs(context.Background(), testNamespace, map[string]string{
		runtimekube.LabelExecution: "exec-1",
		runtimekube.LabelWorkload:  "workload-1",
	})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	call := api.lastCall(t)
	if want := "labelSelector=kodama.dev%2Fexecution%3Dexec-1%2Ckodama.dev%2Fworkload%3Dworkload-1"; call.RawQuery != want {
		t.Fatalf("selector query = %q; want %q", call.RawQuery, want)
	}
	if len(jobs) != 1 || jobs[0].Name != "kodama-job-abc" || jobs[0].Status.Active != 1 {
		t.Fatalf("jobs = %+v; want one active Job", jobs)
	}
}

// TestDeleteJobMapsNotFound pins the Job DELETE path and the 404 sentinel.
func TestDeleteJobMapsNotFound(t *testing.T) {
	status := http.StatusOK
	api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != jobPath("kodama-job-abc") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if status == http.StatusNotFound {
			writeJSON(w, http.StatusNotFound, map[string]any{"kind": "Status", "reason": "NotFound"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"kind": "Status", "status": "Success"})
	})
	cluster := newClusterFor(t, api)

	if err := cluster.DeleteJob(context.Background(), testNamespace, "kodama-job-abc"); err != nil {
		t.Fatalf("DeleteJob: %v", err)
	}
	status = http.StatusNotFound
	if err := cluster.DeleteJob(context.Background(), testNamespace, "kodama-job-abc"); !errors.Is(err, runtimekube.ErrNotFound) {
		t.Fatalf("404 error = %v; want ErrNotFound", err)
	}
}

// TestListPodsEncodesSelectorAndMapsEvidence proves pod selection and the exact
// phase, node, label, deletion-timestamp and terminated-container mapping.
func TestListPodsEncodesSelectorAndMapsEvidence(t *testing.T) {
	api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != podsPath() {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"items": []any{
				map[string]any{
					"metadata": map[string]any{
						"name":              "pod-1",
						"namespace":         testNamespace,
						"labels":            map[string]string{runtimekube.LabelWorkload: "workload-1"},
						"deletionTimestamp": "2026-11-06T10:01:00Z",
					},
					"spec": map[string]any{"nodeName": "node-a"},
					"status": map[string]any{
						"phase": "Failed",
						"containerStatuses": []any{
							map[string]any{
								"name":  "workload",
								"state": map[string]any{"terminated": map[string]any{"exitCode": 7}},
							},
						},
					},
				},
				map[string]any{
					"metadata": map[string]any{
						"name":      "pod-2",
						"namespace": testNamespace,
						"labels":    map[string]string{runtimekube.LabelWorkload: "workload-1"},
					},
					"status": map[string]any{"phase": "Pending"},
				},
			},
		})
	})
	cluster := newClusterFor(t, api)

	pods, err := cluster.ListPods(context.Background(), testNamespace, map[string]string{
		runtimekube.LabelWorkload: "workload-1",
	})
	if err != nil {
		t.Fatalf("ListPods: %v", err)
	}
	call := api.lastCall(t)
	if want := "labelSelector=kodama.dev%2Fworkload%3Dworkload-1"; call.RawQuery != want {
		t.Fatalf("selector query = %q; want %q", call.RawQuery, want)
	}
	if len(pods) != 2 {
		t.Fatalf("pods = %d; want 2", len(pods))
	}
	first := pods[0]
	if first.Name != "pod-1" || first.Phase != "Failed" || first.NodeName != "node-a" {
		t.Fatalf("first pod = %+v", first)
	}
	if first.DeletionTimestamp == nil || !first.DeletionTimestamp.Equal(mustTime(t, "2026-11-06T10:01:00Z")) {
		t.Fatalf("first pod deletion timestamp = %v", first.DeletionTimestamp)
	}
	if len(first.Containers) != 1 || first.Containers[0].Name != "workload" ||
		!first.Containers[0].Terminated || first.Containers[0].ExitCode != 7 {
		t.Fatalf("first pod containers = %+v; want the terminated workload exit 7", first.Containers)
	}
	if pods[1].Phase != "Pending" || len(pods[1].Containers) != 0 {
		t.Fatalf("second pod = %+v; want pending with no container status", pods[1])
	}
}

// TestReadPodLogsDiscardsPrefixAndBoundsPage proves the combined read sends no
// stream query (a stock apiserver rejects it), the prefix-discard offset
// semantics, truncation detection and the short-stream boundary.
func TestReadPodLogsDiscardsPrefixAndBoundsPage(t *testing.T) {
	api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != podLogPath("pod-1") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Has("stream") {
			// A stock apiserver rejects the split-stream query because
			// PodLogsQuerySplitStreams is alpha and off. Fail the request the
			// way the real endpoint does so a re-added parameter cannot pass.
			http.Error(w, "split pod log streams are disabled", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte("0123456789"))
	})
	cluster := newClusterFor(t, api)

	first, err := cluster.ReadPodLogs(context.Background(), testNamespace, "pod-1", runtimekube.PodLogRequest{
		Stream: runtimekube.LogStreamCombined,
		Offset: 3,
		Limit:  4,
	})
	if err != nil {
		t.Fatalf("ReadPodLogs: %v", err)
	}
	if string(first.Data) != "3456" || first.NextOffset != 7 || !first.Truncated {
		t.Fatalf("first page = %+v (%q); want 3456 at 7 truncated", first, first.Data)
	}
	call := api.lastCall(t)
	if want := "container=workload&follow=false"; call.RawQuery != want {
		t.Fatalf("log query = %q; want %q", call.RawQuery, want)
	}

	last, err := cluster.ReadPodLogs(context.Background(), testNamespace, "pod-1", runtimekube.PodLogRequest{
		Stream: runtimekube.LogStreamCombined,
		Offset: 6,
		Limit:  10,
	})
	if err != nil {
		t.Fatalf("ReadPodLogs tail: %v", err)
	}
	if string(last.Data) != "6789" || last.NextOffset != 10 || last.Truncated {
		t.Fatalf("tail page = %+v (%q); want 6789 at 10 not truncated", last, last.Data)
	}

	beyond, err := cluster.ReadPodLogs(context.Background(), testNamespace, "pod-1", runtimekube.PodLogRequest{
		Stream: runtimekube.LogStreamCombined,
		Offset: 20,
		Limit:  5,
	})
	if err != nil {
		t.Fatalf("ReadPodLogs beyond end: %v", err)
	}
	if len(beyond.Data) != 0 || beyond.NextOffset != 20 || beyond.Truncated {
		t.Fatalf("beyond-end page = %+v; want no progress", beyond)
	}

	// The split streams need an alpha API gate; asking for one is refused
	// before any request.
	before := api.callCount()
	for _, stream := range []string{"stdout", "stderr"} {
		_, err = cluster.ReadPodLogs(context.Background(), testNamespace, "pod-1",
			runtimekube.PodLogRequest{Stream: stream, Limit: 4})
		assertFaultClass(t, err, fault.InvalidInput)
	}
	if api.callCount() != before {
		t.Fatal("a split-stream read reached the API")
	}
}

// TestReadPodLogsRefusesUnboundedRequests proves an out-of-range limit or a
// negative offset is refused before any request.
func TestReadPodLogsRefusesUnboundedRequests(t *testing.T) {
	api := newAPI(t, func(http.ResponseWriter, *http.Request) {
		t.Error("an invalid log request reached the API")
	})
	cluster := newClusterFor(t, api)

	for _, request := range []runtimekube.PodLogRequest{
		{Stream: runtimekube.LogStreamCombined, Limit: 0},
		{Stream: runtimekube.LogStreamCombined, Limit: runtimekube.MaxLogBytes + 1},
		{Stream: runtimekube.LogStreamCombined, Offset: -1, Limit: 10},
	} {
		_, err := cluster.ReadPodLogs(context.Background(), testNamespace, "pod-1", request)
		assertFaultClass(t, err, fault.InvalidInput)
	}
	if calls := api.callCount(); calls != 0 {
		t.Fatalf("invalid log requests made %d API calls; want 0", calls)
	}
}

// TestReadPodLogsClassifiesStatusFailures pins the bounded, secret-free mapping
// of a failed pod-log read: a vanished pod is NotFound so the collector can
// record its explicit absence marker, a rejected request stays a retryable
// dependency fault, and an API-server failure keeps the existing uncertain-effect
// class. No response body or credential value appears in the diagnostic.
func TestReadPodLogsClassifiesStatusFailures(t *testing.T) {
	const sensitiveBody = "sensitive-body-value-tok-abc"
	cases := []struct {
		name     string
		status   int
		want     fault.Class
		sentinel error
	}{
		{name: "pod disappeared", status: http.StatusNotFound, want: fault.NotFound, sentinel: runtimekube.ErrNotFound},
		{name: "request rejected", status: http.StatusBadRequest, want: fault.DependencyUnavailable, sentinel: runtimekube.ErrPodLogBadRequest},
		{name: "server failure", status: http.StatusInternalServerError, want: fault.UncertainExternalEffect, sentinel: runtimekube.ErrPodLogServerError},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != podLogPath("pod-1") {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				if want := "container=workload&follow=false"; r.URL.RawQuery != want {
					t.Errorf("log query = %q; want %q", r.URL.RawQuery, want)
				}
				http.Error(w, sensitiveBody, testCase.status)
			})
			cluster := newClusterFor(t, api)

			_, err := cluster.ReadPodLogs(context.Background(), testNamespace, "pod-1", runtimekube.PodLogRequest{
				Stream: runtimekube.LogStreamCombined,
				Limit:  16,
			})
			assertFaultClass(t, err, testCase.want)
			if !errors.Is(err, testCase.sentinel) {
				t.Fatalf("error %v does not wrap the classification sentinel %v", err, testCase.sentinel)
			}
			if strings.Contains(err.Error(), sensitiveBody) {
				t.Fatalf("diagnostic %q leaks the response body", err)
			}
			if calls := api.callCount(); calls != 1 {
				t.Fatalf("pod-log read made %d API calls; want 1", calls)
			}
		})
	}
}

// TestPutConfigMapEncodesPathsAndHandlesImmutableConflict proves the create
// body, the encoded absolute-path keys, the exact binary round trip and the
// immutable-snapshot retry semantics.
func TestPutConfigMapEncodesPathsAndHandlesImmutableConflict(t *testing.T) {
	configMap := runtimekube.ConfigMap{
		Name:      "kodama-activation-abc",
		Namespace: testNamespace,
		Labels:    map[string]string{runtimekube.LabelWorkload: "workload-1"},
		Data: map[string][]byte{
			"/kodama/inputs/input-a": []byte("line1\n"),
			"/kodama/task.json":      {0xff, 0x00},
		},
		Immutable: true,
	}
	createBody := `{
	  "apiVersion": "v1",
	  "kind": "ConfigMap",
	  "metadata": {
	    "name": "kodama-activation-abc",
	    "namespace": "tenant-acme",
	    "labels": {"kodama.dev/workload": "workload-1"}
	  },
	  "immutable": true,
	  "binaryData": {
	    "` + testConfigMapKey + `": "bGluZTEK",
	    "L2tvZGFtYS90YXNrLmpzb24": "/wA="
	  }
	}`

	mode := "create"
	api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == configMapsPath():
			if mode == "create" {
				writeJSON(w, http.StatusCreated, map[string]any{
					"metadata": map[string]any{
						"name":            configMap.Name,
						"namespace":       testNamespace,
						"resourceVersion": "7",
						"labels":          configMap.Labels,
					},
					"immutable":  true,
					"binaryData": map[string]string{testConfigMapKey: "bGluZTEK", "L2tvZGFtYS90YXNrLmpzb24": "/wA="},
				})
				return
			}
			writeJSON(w, http.StatusConflict, map[string]any{"kind": "Status", "reason": "AlreadyExists"})
		case r.Method == http.MethodGet && r.URL.Path == configMapPath(configMap.Name):
			if mode == "different" {
				writeJSON(w, http.StatusOK, map[string]any{
					"metadata":   map[string]any{"name": configMap.Name, "namespace": testNamespace, "resourceVersion": "8"},
					"immutable":  true,
					"binaryData": map[string]string{testConfigMapKey: "ZGlmZmVyZW50"},
				})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"metadata": map[string]any{
					"name":            configMap.Name,
					"namespace":       testNamespace,
					"resourceVersion": "7",
					"labels":          configMap.Labels,
				},
				"immutable":  true,
				"binaryData": map[string]string{testConfigMapKey: "bGluZTEK", "L2tvZGFtYS90YXNrLmpzb24": "/wA="},
			})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	})
	cluster := newClusterFor(t, api)

	created, err := cluster.PutConfigMap(context.Background(), configMap)
	if err != nil {
		t.Fatalf("PutConfigMap create: %v", err)
	}
	assertJSONEqual(t, api.lastCall(t).Body, createBody)
	if created.ResourceVersion != "7" {
		t.Fatalf("created resource version = %q; want 7", created.ResourceVersion)
	}
	if string(created.Data["/kodama/inputs/input-a"]) != "line1\n" {
		t.Fatalf("created data = %v; want the exact activation bytes", created.Data)
	}
	if len(created.Data["/kodama/task.json"]) != 2 || created.Data["/kodama/task.json"][0] != 0xff {
		t.Fatalf("binary data did not round-trip: %v", created.Data["/kodama/task.json"])
	}

	mode = "same"
	reused, err := cluster.PutConfigMap(context.Background(), configMap)
	if err != nil {
		t.Fatalf("PutConfigMap retry with identical content: %v", err)
	}
	if reused.ResourceVersion != "7" {
		t.Fatalf("reused resource version = %q; want the existing 7", reused.ResourceVersion)
	}
	calls := api.snapshot()
	if len(calls) != 3 || calls[1].Method != http.MethodPost || calls[2].Method != http.MethodGet {
		t.Fatalf("retry calls = %s; want POST then GET", formatCalls(calls))
	}

	mode = "different"
	if _, err := cluster.PutConfigMap(context.Background(), configMap); !errors.Is(err, runtimekube.ErrConflict) {
		t.Fatalf("changed immutable content error = %v; want ErrConflict", err)
	}
}

// TestPutConfigMapRefusesAnUpdateVersion proves the immutable snapshot API is
// create-only.
func TestPutConfigMapRefusesAnUpdateVersion(t *testing.T) {
	api := newAPI(t, func(http.ResponseWriter, *http.Request) {
		t.Error("an update request reached the API")
	})
	cluster := newClusterFor(t, api)
	configMap := runtimekube.ConfigMap{
		Name:            "kodama-activation-abc",
		Namespace:       testNamespace,
		Data:            map[string][]byte{"/kodama/task.json": []byte("{}")},
		Immutable:       true,
		ResourceVersion: "7",
	}
	_, err := cluster.PutConfigMap(context.Background(), configMap)
	assertFaultClass(t, err, fault.InvalidInput)
	if calls := api.callCount(); calls != 0 {
		t.Fatalf("update attempt made %d API calls; want 0", calls)
	}
}

// TestGetAndDeleteConfigMapMapNotFound proves the ConfigMap read/delete paths
// and the 404 sentinel.
func TestGetAndDeleteConfigMapMapNotFound(t *testing.T) {
	api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != configMapPath("kodama-activation-abc") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, http.StatusNotFound, map[string]any{"kind": "Status", "reason": "NotFound"})
	})
	cluster := newClusterFor(t, api)

	if _, err := cluster.GetConfigMap(context.Background(), testNamespace, "kodama-activation-abc"); !errors.Is(err, runtimekube.ErrNotFound) {
		t.Fatalf("GetConfigMap 404 = %v; want ErrNotFound", err)
	}
	if err := cluster.DeleteConfigMap(context.Background(), testNamespace, "kodama-activation-abc"); !errors.Is(err, runtimekube.ErrNotFound) {
		t.Fatalf("DeleteConfigMap 404 = %v; want ErrNotFound", err)
	}
}

// TestPutSecretCreatesUpsertsAndConditionallyUpdates proves the Secret create
// body, base64 data, the retry upsert (POST 409 then GET+PUT) and the
// resourceVersion-guarded update.
func TestPutSecretCreatesUpsertsAndConditionallyUpdates(t *testing.T) {
	secret := runtimekube.Secret{
		Name:      "kodama-credential-abc",
		Namespace: testNamespace,
		Labels:    map[string]string{runtimekube.LabelWorkload: "workload-1"},
		Data: map[string][]byte{
			"API_KEY":      []byte("s3cr3t-value"),
			"KODAMA_TOKEN": []byte("tok-abc"),
		},
	}
	createBody := `{
	  "apiVersion": "v1",
	  "kind": "Secret",
	  "metadata": {
	    "name": "kodama-credential-abc",
	    "namespace": "tenant-acme",
	    "labels": {"kodama.dev/workload": "workload-1"}
	  },
	  "type": "Opaque",
	  "data": {"API_KEY": "czNjcjN0LXZhbHVl", "KODAMA_TOKEN": "dG9rLWFiYw=="}
	}`
	updateBody := `{
	  "apiVersion": "v1",
	  "kind": "Secret",
	  "metadata": {
	    "name": "kodama-credential-abc",
	    "namespace": "tenant-acme",
	    "labels": {"kodama.dev/workload": "workload-1"},
	    "resourceVersion": "9"
	  },
	  "type": "Opaque",
	  "data": {"API_KEY": "czNjcjN0LXZhbHVl", "KODAMA_TOKEN": "dG9rLWFiYw=="}
	}`

	mode := "create"
	api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == secretsPath():
			if mode == "create" {
				writeJSON(w, http.StatusCreated, map[string]any{
					"metadata": map[string]any{
						"name":            secret.Name,
						"namespace":       testNamespace,
						"resourceVersion": "5",
						"labels":          secret.Labels,
					},
					"type": "Opaque",
					"data": map[string]string{"API_KEY": "czNjcjN0LXZhbHVl", "KODAMA_TOKEN": "dG9rLWFiYw=="},
				})
				return
			}
			writeJSON(w, http.StatusConflict, map[string]any{"kind": "Status", "reason": "AlreadyExists"})
		case r.Method == http.MethodGet && r.URL.Path == secretPath(secret.Name):
			writeJSON(w, http.StatusOK, map[string]any{
				"metadata": map[string]any{
					"name":            secret.Name,
					"namespace":       testNamespace,
					"resourceVersion": "9",
					"labels":          secret.Labels,
				},
				"type": "Opaque",
				"data": map[string]string{"API_KEY": "b2xk", "KODAMA_TOKEN": "b2xk"},
			})
		case r.Method == http.MethodPut && r.URL.Path == secretPath(secret.Name):
			if mode == "stale" {
				writeJSON(w, http.StatusConflict, map[string]any{"kind": "Status", "reason": "Conflict"})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"metadata": map[string]any{
					"name":            secret.Name,
					"namespace":       testNamespace,
					"resourceVersion": "10",
					"labels":          secret.Labels,
				},
				"type": "Opaque",
				"data": map[string]string{"API_KEY": "czNjcjN0LXZhbHVl", "KODAMA_TOKEN": "dG9rLWFiYw=="},
			})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	})
	cluster := newClusterFor(t, api)

	created, err := cluster.PutSecret(context.Background(), secret)
	if err != nil {
		t.Fatalf("PutSecret create: %v", err)
	}
	assertJSONEqual(t, api.lastCall(t).Body, createBody)
	if created.ResourceVersion != "5" || string(created.Data["API_KEY"]) != "s3cr3t-value" {
		t.Fatalf("created secret = %+v; want the server identity and exact data", created)
	}

	mode = "upsert"
	upserted, err := cluster.PutSecret(context.Background(), secret)
	if err != nil {
		t.Fatalf("PutSecret upsert: %v", err)
	}
	if upserted.ResourceVersion != "10" {
		t.Fatalf("upserted resource version = %q; want 10", upserted.ResourceVersion)
	}
	calls := api.snapshot()
	if len(calls) != 4 || calls[1].Method != http.MethodPost || calls[2].Method != http.MethodGet || calls[3].Method != http.MethodPut {
		t.Fatalf("upsert calls = %s; want POST, GET, PUT", formatCalls(calls))
	}
	assertJSONEqual(t, calls[3].Body, updateBody)

	conditional := secret
	conditional.ResourceVersion = "9"
	mode = "conditional"
	updated, err := cluster.PutSecret(context.Background(), conditional)
	if err != nil {
		t.Fatalf("PutSecret conditional update: %v", err)
	}
	if updated.ResourceVersion != "10" {
		t.Fatalf("conditional update resource version = %q; want 10", updated.ResourceVersion)
	}
	calls = api.snapshot()
	if len(calls) != 5 || calls[4].Method != http.MethodPut {
		t.Fatalf("conditional calls = %s; want one PUT", formatCalls(calls))
	}
	assertJSONEqual(t, calls[4].Body, updateBody)

	mode = "stale"
	if _, err := cluster.PutSecret(context.Background(), conditional); !errors.Is(err, runtimekube.ErrConflict) {
		t.Fatalf("stale update error = %v; want ErrConflict", err)
	}
}

// TestGetSecretDecodesDataAndMapsNotFound proves the Secret read mapping and the
// 404 sentinel.
func TestGetSecretDecodesDataAndMapsNotFound(t *testing.T) {
	status := http.StatusOK
	api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if (r.Method != http.MethodGet && r.Method != http.MethodDelete) || r.URL.Path != secretPath("kodama-credential-abc") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if status == http.StatusNotFound {
			writeJSON(w, http.StatusNotFound, map[string]any{"kind": "Status", "reason": "NotFound"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"metadata": map[string]any{
				"name":            "kodama-credential-abc",
				"namespace":       testNamespace,
				"resourceVersion": "12",
				"labels":          map[string]string{runtimekube.LabelWorkload: "workload-1"},
			},
			"type":      "Opaque",
			"immutable": true,
			"data":      map[string]string{"API_KEY": "czNjcjN0LXZhbHVl", "KODAMA_TOKEN": "dG9rLWFiYw=="},
		})
	})
	cluster := newClusterFor(t, api)

	secret, err := cluster.GetSecret(context.Background(), testNamespace, "kodama-credential-abc")
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if secret.ResourceVersion != "12" || !secret.Immutable {
		t.Fatalf("secret metadata = %+v; want version 12 and immutable", secret)
	}
	if string(secret.Data["API_KEY"]) != "s3cr3t-value" || string(secret.Data["KODAMA_TOKEN"]) != "tok-abc" {
		t.Fatalf("secret data = %v; want the exact decoded values", secret.Data)
	}

	status = http.StatusNotFound
	if _, err := cluster.GetSecret(context.Background(), testNamespace, "kodama-credential-abc"); !errors.Is(err, runtimekube.ErrNotFound) {
		t.Fatalf("404 error = %v; want ErrNotFound", err)
	}
	if err := cluster.DeleteSecret(context.Background(), testNamespace, "kodama-credential-abc"); !errors.Is(err, runtimekube.ErrNotFound) {
		t.Fatalf("delete 404 error = %v; want ErrNotFound", err)
	}
}

// TestGetClaimMapsMetadata proves the claim read mapping, including finalizers
// and a pending deletion timestamp.
func TestGetClaimMapsMetadata(t *testing.T) {
	api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != claimPath("kodama-workspace-abc") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"metadata": map[string]any{
				"name":              "kodama-workspace-abc",
				"namespace":         testNamespace,
				"resourceVersion":   "11",
				"labels":            map[string]string{runtimekube.LabelWorkspace: "workspace-1"},
				"annotations":       map[string]string{runtimekube.AnnotationRetained: "true"},
				"finalizers":        []string{"kubernetes.io/pvc-protection"},
				"deletionTimestamp": "2026-11-06T10:01:00Z",
			},
		})
	})
	cluster := newClusterFor(t, api)

	claim, err := cluster.GetClaim(context.Background(), testNamespace, "kodama-workspace-abc")
	if err != nil {
		t.Fatalf("GetClaim: %v", err)
	}
	if claim.ResourceVersion != "11" || claim.Labels[runtimekube.LabelWorkspace] != "workspace-1" {
		t.Fatalf("claim = %+v", claim)
	}
	if claim.Annotations[runtimekube.AnnotationRetained] != "true" {
		t.Fatalf("claim annotations = %v; want the retained marker", claim.Annotations)
	}
	if len(claim.Finalizers) != 1 || claim.Finalizers[0] != "kubernetes.io/pvc-protection" {
		t.Fatalf("claim finalizers = %v", claim.Finalizers)
	}
	if claim.DeletionTimestamp == nil || !claim.DeletionTimestamp.Equal(mustTime(t, "2026-11-06T10:01:00Z")) {
		t.Fatalf("claim deletion timestamp = %v", claim.DeletionTimestamp)
	}
}

// TestUpdateClaimPatchesMetadataOnlyWithResourceVersion proves the conditional
// metadata PATCH: it preserves spec/finalizers, removes keys by explicit null
// and refuses a stale expected version without patching.
func TestUpdateClaimPatchesMetadataOnlyWithResourceVersion(t *testing.T) {
	desired := runtimekube.Claim{
		Name:            "kodama-workspace-abc",
		Namespace:       testNamespace,
		ResourceVersion: "11",
		Labels: map[string]string{
			runtimekube.LabelTenant:    testNamespace,
			runtimekube.LabelWorkspace: "workspace-1",
		},
		Annotations: map[string]string{runtimekube.AnnotationRetained: "true"},
	}
	patchBody := `{
	  "metadata": {
	    "resourceVersion": "11",
	    "labels": {
	      "kodama.dev/tenant": "tenant-acme",
	      "kodama.dev/workspace": "workspace-1",
	      "extra": null
	    },
	    "annotations": {
	      "kodama.dev/retained": "true",
	      "old-marker": null
	    }
	  }
	}`

	observedVersion := "11"
	patchStatus := http.StatusOK
	api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == claimPath(desired.Name):
			writeJSON(w, http.StatusOK, map[string]any{
				"metadata": map[string]any{
					"name":            desired.Name,
					"namespace":       testNamespace,
					"resourceVersion": observedVersion,
					"labels": map[string]string{
						runtimekube.LabelTenant:    testNamespace,
						runtimekube.LabelWorkspace: "workspace-1",
						"extra":                    "value",
					},
					"annotations": map[string]string{
						runtimekube.AnnotationRetained: "true",
						"old-marker":                   "stale",
					},
					"finalizers": []string{"kubernetes.io/pvc-protection"},
				},
			})
		case r.Method == http.MethodPatch && r.URL.Path == claimPath(desired.Name):
			if contentType := r.Header.Get("Content-Type"); contentType != "application/merge-patch+json" {
				t.Errorf("Content-Type = %q; want application/merge-patch+json", contentType)
			}
			if patchStatus != http.StatusOK {
				writeJSON(w, patchStatus, map[string]any{"kind": "Status", "reason": "Conflict"})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"metadata": map[string]any{
					"name":            desired.Name,
					"namespace":       testNamespace,
					"resourceVersion": "12",
					"labels": map[string]string{
						runtimekube.LabelTenant:    testNamespace,
						runtimekube.LabelWorkspace: "workspace-1",
					},
					"annotations": map[string]string{runtimekube.AnnotationRetained: "true"},
					"finalizers":  []string{"kubernetes.io/pvc-protection"},
				},
			})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	})
	cluster := newClusterFor(t, api)

	updated, err := cluster.UpdateClaim(context.Background(), desired)
	if err != nil {
		t.Fatalf("UpdateClaim: %v", err)
	}
	if updated.ResourceVersion != "12" || updated.Labels["extra"] != "" {
		t.Fatalf("updated claim = %+v; want version 12 without the removed label", updated)
	}
	if len(updated.Finalizers) != 1 || updated.Finalizers[0] != "kubernetes.io/pvc-protection" {
		t.Fatalf("updated finalizers = %v; want the preserved finalizer", updated.Finalizers)
	}
	patchCall := api.lastCall(t)
	assertJSONEqual(t, patchCall.Body, patchBody)
	body := decodeJSON(t, patchCall.Body).(map[string]any)
	metadata := body["metadata"].(map[string]any)
	for _, forbidden := range []string{"spec", "finalizers", "deletionTimestamp"} {
		if _, ok := metadata[forbidden]; ok {
			t.Fatalf("patch metadata contains %q; it must preserve it server-side", forbidden)
		}
	}

	observedVersion = "12"
	if _, err := cluster.UpdateClaim(context.Background(), desired); !errors.Is(err, runtimekube.ErrConflict) {
		t.Fatalf("stale update error = %v; want ErrConflict", err)
	}
	calls := api.snapshot()
	if calls[len(calls)-1].Method != http.MethodGet {
		t.Fatalf("stale update reached %s; want only the observation GET", calls[len(calls)-1].Method)
	}

	observedVersion = "11"
	patchStatus = http.StatusConflict
	if _, err := cluster.UpdateClaim(context.Background(), desired); !errors.Is(err, runtimekube.ErrConflict) {
		t.Fatalf("patch 409 error = %v; want ErrConflict", err)
	}
}

// TestDeleteClaimVersionedSendsPreconditions proves the conditional and
// unconditional claim deletion bodies and the 404/409 mapping.
func TestDeleteClaimVersionedSendsPreconditions(t *testing.T) {
	versionedBody := `{
	  "apiVersion": "v1",
	  "kind": "DeleteOptions",
	  "preconditions": {"resourceVersion": "11"}
	}`

	deleteStatus := http.StatusOK
	var deleteBody []byte
	api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != claimPath("kodama-workspace-abc") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		deleteBody, _ = io.ReadAll(r.Body)
		if contentType := r.Header.Get("Content-Type"); deleteStatus == http.StatusOK && contentType != "application/json" {
			t.Errorf("Content-Type = %q; want application/json", contentType)
		}
		switch deleteStatus {
		case http.StatusNotFound:
			writeJSON(w, http.StatusNotFound, map[string]any{"kind": "Status", "reason": "NotFound"})
		case http.StatusConflict:
			writeJSON(w, http.StatusConflict, map[string]any{"kind": "Status", "reason": "Conflict"})
		default:
			writeJSON(w, http.StatusOK, map[string]any{"kind": "Status", "status": "Success"})
		}
	})
	cluster := newClusterFor(t, api)

	if err := cluster.DeleteClaimVersioned(context.Background(), testNamespace, "kodama-workspace-abc", "11"); err != nil {
		t.Fatalf("DeleteClaimVersioned: %v", err)
	}
	assertJSONEqual(t, deleteBody, versionedBody)

	if err := cluster.DeleteClaim(context.Background(), testNamespace, "kodama-workspace-abc"); err != nil {
		t.Fatalf("DeleteClaim: %v", err)
	}
	if len(deleteBody) != 0 {
		t.Fatalf("unconditional delete body = %q; want no preconditions", deleteBody)
	}

	deleteStatus = http.StatusConflict
	if err := cluster.DeleteClaimVersioned(context.Background(), testNamespace, "kodama-workspace-abc", "11"); !errors.Is(err, runtimekube.ErrConflict) {
		t.Fatalf("stale delete error = %v; want ErrConflict", err)
	}

	deleteStatus = http.StatusNotFound
	if err := cluster.DeleteClaimVersioned(context.Background(), testNamespace, "kodama-workspace-abc", "11"); !errors.Is(err, runtimekube.ErrNotFound) {
		t.Fatalf("missing delete error = %v; want ErrNotFound", err)
	}
}

// TestCreateJobRefusesAnUnconfinedVolumeItem proves a volume item path outside
// its mount is refused before any API call.
func TestCreateJobRefusesAnUnconfinedVolumeItem(t *testing.T) {
	api := newAPI(t, func(http.ResponseWriter, *http.Request) {
		t.Error("an unconfined manifest reached the API")
	})
	cluster := newClusterFor(t, api)

	job := testJob()
	job.Spec.Volumes[1].Items = []runtimekube.VolumeItem{
		{Key: "x", Path: "/other/root/x", Mode: 0o444},
	}
	_, err := cluster.CreateJob(context.Background(), job)
	assertFaultClass(t, err, fault.InvalidInput)
	if calls := api.callCount(); calls != 0 {
		t.Fatalf("refused manifest made %d API calls; want 0", calls)
	}
}

// TestClusterMapsNonSentinelStatusesToDependency proves a refusal the server did
// apply nothing for is a dependency failure, not an absence or a conflict.
func TestClusterMapsNonSentinelStatusesToDependency(t *testing.T) {
	api := newAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusForbidden, map[string]any{"kind": "Status", "reason": "Forbidden"})
	})
	cluster := newClusterFor(t, api)

	_, err := cluster.GetNamespace(context.Background(), testNamespace)
	assertFaultClass(t, err, fault.DependencyUnavailable)
	if errors.Is(err, runtimekube.ErrNotFound) || errors.Is(err, driverkube.ErrAmbiguous) {
		t.Fatalf("403 error = %v; want a plain dependency failure", err)
	}
}

// mustTime parses one fixture timestamp.
func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse fixture time %q: %v", value, err)
	}
	return parsed
}

// formatCalls renders a call sequence without bodies.
func formatCalls(calls []apiCall) string {
	parts := make([]string, len(calls))
	for index, call := range calls {
		parts[index] = call.Method + " " + call.Path
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
