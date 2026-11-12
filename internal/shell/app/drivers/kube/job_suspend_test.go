// Behavioural specification of the UID-bound Job suspension seam.
//
// An uncertain Kubernetes stop is confirmed through Job suspension rather than
// a pod finalizer. The driver therefore exposes exactly
// one bounded operation:
//
//	SuspendJob(ctx, namespace, name, expectedUID) (Job, error)
//
// It sends one merge PATCH that carries only the expected UID, the suspension
// intent and the explicit clear of the finished-Job TTL, decodes the answer
// through the common Job decoder, and preserves the existing REST failure
// classification. It never falls back to a DELETE, never PUTs a full object and
// never retries internally.
//
// The Job decoder must keep the evidence the confirmation predicate needs:
// metadata.uid, metadata.deletionTimestamp, spec.suspend, status.active, a
// presence-preserving (nullable) status.terminating and every condition with
// its exact status value. An absent terminating field is unknown, not zero.
package kube_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	driverkube "github.com/optimiware/kodama/internal/shell/app/drivers/kube"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
)

// suspendedJobDocument is a complete Job response carrying every piece of
// suspension evidence the seam must preserve.
func suspendedJobDocument() map[string]any {
	return map[string]any{
		"metadata": map[string]any{
			"name":              "kodama-job-abc",
			"namespace":         testNamespace,
			"uid":               "uid-abc",
			"resourceVersion":   "51",
			"deletionTimestamp": "2026-11-06T10:02:00Z",
			"labels":            testJobLabels(),
			"annotations":       map[string]string{runtimekube.AnnotationIdempotencyKey: "idem-1"},
		},
		"spec": map[string]any{"suspend": true},
		"status": map[string]any{
			"active":      0,
			"succeeded":   0,
			"failed":      0,
			"terminating": 0,
			"conditions": []any{
				map[string]any{"type": "Suspended", "status": "True", "reason": "Suspended"},
				map[string]any{"type": "Complete", "status": "False", "reason": "Suspended"},
				map[string]any{"type": "Failed", "status": "Unknown"},
			},
		},
	}
}

// TestSuspendJobPatchesOnlySuspendAndExpectedUID proves the request is one
// merge PATCH that carries only the expected UID, the suspension intent and the
// explicit TTL clear, and that the decoded answer keeps the same UID and the
// acknowledgment evidence.
func TestSuspendJobPatchesOnlySuspendAndExpectedUID(t *testing.T) {
	api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != jobPath("kodama-job-abc") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if contentType := r.Header.Get("Content-Type"); contentType != "application/merge-patch+json" {
			t.Errorf("Content-Type = %q; want application/merge-patch+json", contentType)
		}
		writeJSON(w, http.StatusOK, suspendedJobDocument())
	})
	cluster := newClusterFor(t, api)

	job, err := cluster.SuspendJob(context.Background(), testNamespace, "kodama-job-abc", "uid-abc")
	if err != nil {
		t.Fatalf("SuspendJob: %v", err)
	}

	const wantBody = `{
	  "metadata": {"uid": "uid-abc"},
	  "spec": {"suspend": true, "ttlSecondsAfterFinished": null}
	}`
	call := api.lastCall(t)
	assertJSONEqual(t, call.Body, wantBody)

	calls := api.snapshot()
	if len(calls) != 1 || calls[0].Method != http.MethodPatch {
		t.Fatalf("calls = %s; want exactly one PATCH (no DELETE, PUT or retry)", formatCalls(calls))
	}

	if job.UID != "uid-abc" {
		t.Fatalf("returned Job UID = %q; want the patched UID", job.UID)
	}
	if job.Spec.Suspend == nil || !*job.Spec.Suspend {
		t.Fatalf("returned Job spec.suspend = %v; want true", job.Spec.Suspend)
	}
	if job.Status.Terminating == nil || *job.Status.Terminating != 0 {
		t.Fatalf("returned Job status.terminating = %v; want a present zero", job.Status.Terminating)
	}
	if len(job.Status.Conditions) != 3 || job.Status.Conditions[0].Type != "Suspended" || job.Status.Conditions[0].Status != "True" {
		t.Fatalf("returned Job conditions = %+v; want the Suspended=True acknowledgment", job.Status.Conditions)
	}
}

// TestSuspendJobClearsFinishedJobTTLWithExplicitNull proves the suspension merge
// PATCH clears spec.ttlSecondsAfterFinished with an explicit JSON null in the
// same request while keeping the UID precondition and the suspension intent. A
// Job that finishes while suspended is otherwise removed by the TTL controller
// (with foreground propagation, taking its pods) before Remove can delete it, so
// the stop barrier would lose its only durable evidence and stay pending
// forever. Only these three fields may be sent.
func TestSuspendJobClearsFinishedJobTTLWithExplicitNull(t *testing.T) {
	api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("method = %s; want PATCH", r.Method)
		}
		writeJSON(w, http.StatusOK, suspendedJobDocument())
	})
	cluster := newClusterFor(t, api)

	if _, err := cluster.SuspendJob(context.Background(), testNamespace, "kodama-job-abc", "uid-abc"); err != nil {
		t.Fatalf("SuspendJob: %v", err)
	}

	call := api.lastCall(t)
	var body map[string]any
	if err := json.Unmarshal(call.Body, &body); err != nil {
		t.Fatalf("PATCH body is not JSON: %v (%s)", err, call.Body)
	}
	metadata, ok := body["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("PATCH metadata = %v; want an object carrying the UID guard", body["metadata"])
	}
	if len(metadata) != 1 || metadata["uid"] != "uid-abc" {
		t.Fatalf("PATCH metadata = %v; want only the expected UID guard", metadata)
	}
	spec, ok := body["spec"].(map[string]any)
	if !ok {
		t.Fatalf("PATCH spec = %v; want an object", body["spec"])
	}
	ttl, present := spec["ttlSecondsAfterFinished"]
	if !present {
		t.Fatalf("PATCH spec = %v; want an explicit ttlSecondsAfterFinished:null", spec)
	}
	if ttl != nil {
		t.Fatalf("PATCH ttlSecondsAfterFinished = %v; want an explicit JSON null", ttl)
	}
	if suspend, ok := spec["suspend"].(bool); !ok || !suspend {
		t.Fatalf("PATCH spec.suspend = %v; want true", spec["suspend"])
	}
	if len(spec) != 2 {
		t.Fatalf("PATCH spec = %v; want only suspend and the TTL clear", spec)
	}
}

// TestJobReadsDecodeFinishedJobTTL proves the common Job decoder preserves
// spec.ttlSecondsAfterFinished, so a corrected test can observe both the created
// TTL and its removal after the suspension PATCH. An absent field stays nil.
func TestJobReadsDecodeFinishedJobTTL(t *testing.T) {
	cases := []struct {
		name string
		spec map[string]any
		want *int
	}{
		{name: "present", spec: map[string]any{"ttlSecondsAfterFinished": 30}, want: new(30)},
		{name: "absent", spec: map[string]any{}, want: nil},
		{name: "explicit null", spec: map[string]any{"ttlSecondsAfterFinished": nil}, want: nil},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			api := newAPI(t, func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, map[string]any{
					"metadata": map[string]any{
						"name":      "kodama-job-abc",
						"namespace": testNamespace,
						"uid":       "uid-abc",
					},
					"spec": testCase.spec,
				})
			})
			cluster := newClusterFor(t, api)

			job, err := cluster.GetJob(context.Background(), testNamespace, "kodama-job-abc")
			if err != nil {
				t.Fatalf("GetJob: %v", err)
			}
			switch {
			case testCase.want == nil && job.Spec.TTLSecondsAfterFinished != nil:
				t.Fatalf("ttlSecondsAfterFinished decoded as %d; want absent", *job.Spec.TTLSecondsAfterFinished)
			case testCase.want != nil && job.Spec.TTLSecondsAfterFinished == nil:
				t.Fatalf("ttlSecondsAfterFinished decoded as absent; want %d", *testCase.want)
			case testCase.want != nil && *job.Spec.TTLSecondsAfterFinished != *testCase.want:
				t.Fatalf("ttlSecondsAfterFinished = %d; want %d", *job.Spec.TTLSecondsAfterFinished, *testCase.want)
			}
		})
	}
}

// TestSuspendJobRejectsEmptyUIDBeforeRequest proves an empty expected UID never
// reaches the API: an UID-less patch could suspend a reused Job name.
func TestSuspendJobRejectsEmptyUIDBeforeRequest(t *testing.T) {
	for _, uid := range []string{"", "   "} {
		api := newAPI(t, func(http.ResponseWriter, *http.Request) {
			t.Error("a request with no expected UID reached the API")
		})
		cluster := newClusterFor(t, api)

		_, err := cluster.SuspendJob(context.Background(), testNamespace, "kodama-job-abc", uid)
		assertFaultClass(t, err, fault.InvalidInput)
		if calls := api.callCount(); calls != 0 {
			t.Fatalf("empty UID %q made %d API calls; want 0", uid, calls)
		}
	}
}

// TestSuspendJobPreservesFailureClassification proves the seam keeps the
// existing transport classification: an absent Job is NotFound, a conflict or
// rejection is a failure, and a lost transport or a 5xx response stays an
// uncertain effect. No failure is retried or converted into DELETE.
func TestSuspendJobPreservesFailureClassification(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		want     fault.Class
		sentinel error
	}{
		{name: "absent Job", status: http.StatusNotFound, want: fault.NotFound, sentinel: runtimekube.ErrNotFound},
		{name: "stale conflict", status: http.StatusConflict, want: fault.VersionConflict, sentinel: runtimekube.ErrConflict},
		{name: "rejected patch", status: http.StatusBadRequest, want: fault.DependencyUnavailable},
		{name: "forbidden patch", status: http.StatusForbidden, want: fault.DependencyUnavailable},
		{name: "server failure", status: http.StatusInternalServerError, want: fault.UncertainExternalEffect, sentinel: driverkube.ErrAmbiguous},
		{name: "server unavailable", status: http.StatusServiceUnavailable, want: fault.UncertainExternalEffect, sentinel: driverkube.ErrAmbiguous},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPatch || r.URL.Path != jobPath("kodama-job-abc") {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				writeJSON(w, testCase.status, map[string]any{"kind": "Status", "reason": "Failure"})
			})
			cluster := newClusterFor(t, api)

			job, err := cluster.SuspendJob(context.Background(), testNamespace, "kodama-job-abc", "uid-abc")
			assertFaultClass(t, err, testCase.want)
			if testCase.sentinel != nil && !errors.Is(err, testCase.sentinel) {
				t.Fatalf("error %v does not wrap %v", err, testCase.sentinel)
			}
			if job.Name != "" || job.UID != "" || job.Spec.Suspend != nil {
				t.Fatalf("failed suspend returned job %+v; want the zero value", job)
			}
			calls := api.snapshot()
			if len(calls) != 1 || calls[0].Method != http.MethodPatch {
				t.Fatalf("calls = %s; want exactly one PATCH", formatCalls(calls))
			}
		})
	}
}

// TestJobReadsPreserveSuspensionEvidence proves GET, LIST and PATCH all decode
// the common Job fields through the same decoder: UID, deletion timestamp,
// spec.suspend, active, a present terminating zero and the exact condition
// statuses. A GET-only implementation would leave LIST and PATCH blind.
func TestJobReadsPreserveSuspensionEvidence(t *testing.T) {
	api := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == jobsPath():
			writeJSON(w, http.StatusOK, map[string]any{"items": []any{suspendedJobDocument()}})
		case r.Method == http.MethodGet && r.URL.Path == jobPath("kodama-job-abc"):
			writeJSON(w, http.StatusOK, suspendedJobDocument())
		case r.Method == http.MethodPatch && r.URL.Path == jobPath("kodama-job-abc"):
			writeJSON(w, http.StatusOK, suspendedJobDocument())
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
		}
	})
	cluster := newClusterFor(t, api)

	reads := []struct {
		name string
		read func() (runtimekube.Job, error)
	}{
		{
			name: "GET",
			read: func() (runtimekube.Job, error) {
				return cluster.GetJob(context.Background(), testNamespace, "kodama-job-abc")
			},
		},
		{
			name: "LIST",
			read: func() (runtimekube.Job, error) {
				jobs, err := cluster.ListJobs(context.Background(), testNamespace, map[string]string{
					runtimekube.LabelWorkload: "workload-1",
				})
				if err != nil {
					return runtimekube.Job{}, err
				}
				if len(jobs) != 1 {
					t.Fatalf("ListJobs returned %d Jobs; want 1", len(jobs))
				}
				return jobs[0], nil
			},
		},
		{
			name: "PATCH",
			read: func() (runtimekube.Job, error) {
				return cluster.SuspendJob(context.Background(), testNamespace, "kodama-job-abc", "uid-abc")
			},
		},
	}
	for _, readCase := range reads {
		t.Run(readCase.name, func(t *testing.T) {
			job, err := readCase.read()
			if err != nil {
				t.Fatalf("%s decode: %v", readCase.name, err)
			}
			if job.UID != "uid-abc" {
				t.Fatalf("UID = %q; want uid-abc", job.UID)
			}
			if job.DeletionTimestamp == nil || !job.DeletionTimestamp.Equal(mustTime(t, "2026-11-06T10:02:00Z")) {
				t.Fatalf("deletion timestamp = %v; want the observed timestamp", job.DeletionTimestamp)
			}
			if job.Spec.Suspend == nil || !*job.Spec.Suspend {
				t.Fatalf("spec.suspend = %v; want true", job.Spec.Suspend)
			}
			if job.Status.Active != 0 {
				t.Fatalf("status.active = %d; want 0", job.Status.Active)
			}
			if job.Status.Terminating == nil || *job.Status.Terminating != 0 {
				t.Fatalf("status.terminating = %v; want a present zero (absent is unknown)", job.Status.Terminating)
			}
			if len(job.Status.Conditions) != 3 {
				t.Fatalf("conditions = %+v; want the three observed conditions", job.Status.Conditions)
			}
			want := []struct{ kind, status string }{
				{"Suspended", "True"},
				{"Complete", "False"},
				{"Failed", "Unknown"},
			}
			for index, expected := range want {
				condition := job.Status.Conditions[index]
				if condition.Type != expected.kind || condition.Status != expected.status {
					t.Fatalf("condition %d = %+v; want %s=%s", index, condition, expected.kind, expected.status)
				}
			}
		})
	}
}

// TestJobTerminatingAbsentIsNotZero proves the terminating counter is
// presence-preserving. An omitted or null field is unknown (nil), while an
// explicit zero is affirmative and survives as a present zero.
func TestJobTerminatingAbsentIsNotZero(t *testing.T) {
	cases := []struct {
		name  string
		value any
		null  bool
		want  int
	}{
		{name: "absent", value: nil, null: true},
		{name: "explicit null", value: nil, null: true},
		{name: "explicit zero", value: 0, want: 0},
		{name: "nonzero", value: 2, want: 2},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			status := map[string]any{"active": 0}
			switch {
			case testCase.name == "explicit null":
				status["terminating"] = nil
			case !testCase.null:
				status["terminating"] = testCase.value
			}
			api := newAPI(t, func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, map[string]any{
					"metadata": map[string]any{
						"name":      "kodama-job-abc",
						"namespace": testNamespace,
						"uid":       "uid-abc",
					},
					"status": status,
				})
			})
			cluster := newClusterFor(t, api)

			job, err := cluster.GetJob(context.Background(), testNamespace, "kodama-job-abc")
			if err != nil {
				t.Fatalf("GetJob: %v", err)
			}
			if testCase.null {
				if job.Status.Terminating != nil {
					t.Fatalf("absent terminating decoded as %d; want unknown (nil)", *job.Status.Terminating)
				}
				return
			}
			if job.Status.Terminating == nil {
				t.Fatal("present terminating decoded as unknown (nil)")
			}
			if *job.Status.Terminating != testCase.want {
				t.Fatalf("terminating = %d; want %d", *job.Status.Terminating, testCase.want)
			}
		})
	}
}
