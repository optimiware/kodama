//go:build qualification

package production

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
)

// TestHarnessResilienceReplicaRestoreAfterMidwayFailure is the safety-critical
// test of the fault-injection harness: a probe saves the original statefulset
// replica count, scales it to zero, and fails before it can restore. The
// t.Cleanup path must still restore the saved count and verify it with a read.
func TestHarnessResilienceReplicaRestoreAfterMidwayFailure(t *testing.T) {
	cluster, state, _ := resilienceFakeCluster(t, resilienceFakeOptions{
		Replicas:        3,
		FailScaleToZero: true,
	})
	ctx := context.Background()

	guard := resilienceNewReplicaGuard(cluster)
	saved, err := guard.Save(ctx, ReleaseNamespace, "statefulsets", "nats")
	if err != nil {
		t.Fatalf("save replicas: %v", err)
	}
	if saved != 3 {
		t.Fatalf("saved replicas = %d, want 3", saved)
	}
	if guard.AllRestored() {
		t.Fatal("a saved but not yet restored target reports restoration")
	}
	// The probe fails here, after the scale effect, before it can run its own
	// restore. The cleanup restoration must still reach the saved count.
	if err := guard.Scale(ctx, ReleaseNamespace, "statefulsets", "nats", 0); err == nil {
		t.Fatal("the fake scale to zero unexpectedly succeeded")
	}
	restoration, err := resilienceRestoreSafely(ctx, guard)
	if err != nil {
		t.Fatalf("replica cleanup did not restore the saved count after the mid-way failure: %v", err)
	}
	if restoration != "restored" || !guard.AllRestored() {
		t.Fatalf("restoration = %q, all restored = %v; want restored", restoration, guard.AllRestored())
	}

	if got := state.replicas(t); got != 3 {
		t.Fatalf("restored statefulset replicas = %d, want the saved 3", got)
	}
	if count := state.argCount(t, "--replicas=3"); count != 1 {
		t.Fatalf("restore scale arguments = %d, want exactly one scale back to 3: %s", count, state.argv(t))
	}
}

// TestHarnessResilienceReplicaRestoreRetries proves a transient restore failure
// is retried and a permanent one is bounded and attributed instead of claimed
// as success.
func TestHarnessResilienceReplicaRestoreRetries(t *testing.T) {
	cluster, state, _ := resilienceFakeCluster(t, resilienceFakeOptions{
		Replicas:        3,
		FailRestoreOnce: true,
	})
	guard := resilienceNewReplicaGuard(cluster)
	ctx := context.Background()
	if _, err := guard.Save(ctx, ReleaseNamespace, "statefulsets", "nats"); err != nil {
		t.Fatalf("save replicas: %v", err)
	}
	if err := guard.RestoreAll(ctx); err != nil {
		t.Fatalf("a transient restore failure was not retried: %v", err)
	}
	if !guard.AllRestored() || guard.Restoration() != "restored" {
		t.Fatalf("restoration = %q, all restored = %v; want restored", guard.Restoration(), guard.AllRestored())
	}
	if got := state.replicas(t); got != 3 {
		t.Fatalf("statefulset replicas after retried restore = %d, want 3", got)
	}
	if count := state.argCount(t, "--replicas=3"); count != 2 {
		t.Fatalf("restore attempts = %d, want 2 (one failed, one succeeded)", count)
	}
}

// TestHarnessResilienceReplicaRestoreIsBounded proves a restore that never
// reaches the saved count stays unrestored, is attempted the bounded number of
// times, and names the target.
func TestHarnessResilienceReplicaRestoreIsBounded(t *testing.T) {
	cluster, state, _ := resilienceFakeCluster(t, resilienceFakeOptions{
		Replicas:          3,
		FailRestoreAlways: true,
	})
	guard := resilienceNewReplicaGuard(cluster)
	ctx := context.Background()
	if _, err := guard.Save(ctx, ReleaseNamespace, "statefulsets", "nats"); err != nil {
		t.Fatalf("save replicas: %v", err)
	}
	restoration, err := resilienceRestoreSafely(ctx, guard)
	if err == nil {
		t.Fatal("a permanently failing restore was reported as success")
	}
	if restoration != "failed" {
		t.Fatalf("restoration = %q, want failed", restoration)
	}
	if !strings.Contains(err.Error(), "3 attempts") {
		t.Fatalf("restore error %q does not name the attempt bound", err)
	}
	if !strings.Contains(err.Error(), "statefulsets") || !strings.Contains(err.Error(), "nats") {
		t.Fatalf("restore error %q does not name the target", err)
	}
	if guard.AllRestored() || guard.Restoration() != "unrestored" {
		t.Fatalf("restoration = %q, all restored = %v; want unrestored", guard.Restoration(), guard.AllRestored())
	}
	if count := state.argCount(t, "--replicas=3"); count != resilienceRestoreAttempts {
		t.Fatalf("restore attempts = %d, want the bound %d", count, resilienceRestoreAttempts)
	}
}

// TestHarnessResilienceReplicaRestoreVerifiesWithRead proves a scale that
// returns success but does not reach the saved count is not accepted as a
// restored replica count.
func TestHarnessResilienceReplicaRestoreVerifiesWithRead(t *testing.T) {
	cluster, _, _ := resilienceFakeCluster(t, resilienceFakeOptions{
		Replicas:   3,
		LieOnScale: true,
	})
	guard := resilienceNewReplicaGuard(cluster)
	ctx := context.Background()
	if _, err := guard.Save(ctx, ReleaseNamespace, "statefulsets", "nats"); err != nil {
		t.Fatalf("save replicas: %v", err)
	}
	err := guard.RestoreAll(ctx)
	if err == nil {
		t.Fatal("a restore whose read-back did not reach the saved count was accepted")
	}
	if !strings.Contains(err.Error(), "observed 4") {
		t.Fatalf("restore error %q does not name the observed count", err)
	}
	if guard.AllRestored() {
		t.Fatal("an unverified restore reports restoration")
	}
}

// TestHarnessResilienceReplicaGuardRefusesUnsafeTargets proves the guard only
// scales a target it saved, and refuses duplicates and malformed inputs before
// any kubectl process starts.
func TestHarnessResilienceReplicaGuardRefusesUnsafeTargets(t *testing.T) {
	cluster, state, _ := resilienceFakeCluster(t, resilienceFakeOptions{Replicas: 2})
	guard := resilienceNewReplicaGuard(cluster)
	ctx := context.Background()

	for _, testCase := range []struct {
		name      string
		namespace string
		resource  string
		target    string
	}{
		{name: "empty namespace", resource: "statefulsets", target: "nats"},
		{name: "empty resource", namespace: ReleaseNamespace, target: "nats"},
		{name: "empty name", namespace: ReleaseNamespace, resource: "statefulsets"},
		{name: "flag name", namespace: ReleaseNamespace, resource: "statefulsets", target: "--replicas=0"},
		{name: "slash name", namespace: ReleaseNamespace, resource: "statefulsets", target: "nats/other"},
		{name: "unknown resource", namespace: ReleaseNamespace, resource: "secrets", target: "nats"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := guard.Save(ctx, testCase.namespace, testCase.resource, testCase.target); err == nil {
				t.Fatalf("saved unsafe target %q/%q/%q", testCase.namespace, testCase.resource, testCase.target)
			}
		})
	}
	if got := state.argv(t); got != "" {
		t.Fatalf("refused targets reached kubectl: %s", got)
	}

	if _, err := guard.Save(ctx, ReleaseNamespace, "statefulsets", "nats"); err != nil {
		t.Fatalf("save replicas: %v", err)
	}
	if _, err := guard.Save(ctx, ReleaseNamespace, "statefulsets", "nats"); err == nil {
		t.Fatal("the same target was saved twice")
	}
	if err := guard.Scale(ctx, ReleaseNamespace, "statefulsets", "missing", 0); err == nil {
		t.Fatal("a target that was never saved was scaled")
	}
	if err := guard.Scale(ctx, ReleaseNamespace, "statefulsets", "nats", 11); err == nil {
		t.Fatal("an out-of-bounds replica count was scaled")
	}
	if len(guard.Targets()) != 1 || guard.Targets()[0].Replicas != 2 {
		t.Fatalf("saved targets = %+v, want exactly nats@2", guard.Targets())
	}
	if err := guard.Scale(ctx, ReleaseNamespace, "statefulsets", "nats", 0); err != nil {
		t.Fatalf("scale the saved target: %v", err)
	}
	if got := state.replicas(t); got != 0 {
		t.Fatalf("replicas after scale = %d, want 0", got)
	}
}

// TestHarnessResilienceClusterCommands proves the fault-injection commands use
// only the hardened helper's allowlisted options and that malformed selectors
// and resources never reach kubectl.
func TestHarnessResilienceClusterCommands(t *testing.T) {
	ctx := context.Background()
	t.Run("allowed commands", func(t *testing.T) {
		cluster, argv := resilienceRecordingCluster(t)
		if err := cluster.Scale(ctx, ReleaseNamespace, "statefulsets", "nats", 0); err != nil {
			t.Fatalf("scale: %v", err)
		}
		if err := cluster.DeletePods(ctx, ReleaseNamespace, "kodama.dev/execution=exec-1"); err != nil {
			t.Fatalf("delete pods: %v", err)
		}
		if err := cluster.DeletePod(ctx, ReleaseNamespace, "nats-0"); err != nil {
			t.Fatalf("delete pod: %v", err)
		}
		if err := cluster.WaitPodReady(ctx, ReleaseNamespace, "kodama-abc", 90*time.Second); err != nil {
			t.Fatalf("wait pod ready: %v", err)
		}
		if _, err := cluster.ResourceNames(ctx, "kodama-tenant-tenant-a", "jobs", "kodama.dev/execution=exec-1"); err != nil {
			t.Fatalf("list jobs: %v", err)
		}
		if _, err := cluster.PodNames(ctx, ReleaseNamespace, "app.kubernetes.io/name=nats"); err != nil {
			t.Fatalf("list pods: %v", err)
		}
		if _, err := cluster.PodUID(ctx, ReleaseNamespace, "nats-0"); err != nil {
			t.Fatalf("read pod uid: %v", err)
		}
		recorded := resilienceReadFileString(t, argv)
		for _, want := range []string{
			"scale statefulsets nats --replicas=0",
			"delete pods -l kodama.dev/execution=exec-1",
			"delete pods nats-0",
			"wait pods kodama-abc --for=condition=Ready --timeout=90s",
			"get jobs -l kodama.dev/execution=exec-1 -o name",
			"get pods -l app.kubernetes.io/name=nats -o jsonpath={.items[*].metadata.name}",
			"get pods nats-0 -o jsonpath={.metadata.uid}",
		} {
			if !strings.Contains(recorded, want) {
				t.Fatalf("argv does not contain %q: %s", want, recorded)
			}
		}
	})

	t.Run("refused commands", func(t *testing.T) {
		cluster, argv := resilienceRecordingCluster(t)
		cases := []struct {
			name string
			run  func() error
		}{
			{name: "selector injection", run: func() error {
				return cluster.DeletePods(ctx, ReleaseNamespace, "app=x;rm -rf /")
			}},
			{name: "empty selector", run: func() error {
				return cluster.DeletePods(ctx, ReleaseNamespace, "")
			}},
			{name: "forbidden resource", run: func() error {
				_, err := cluster.ResourceNames(ctx, ReleaseNamespace, "secrets", "app=x")
				return err
			}},
			{name: "unsafe pod name", run: func() error {
				return cluster.DeletePod(ctx, ReleaseNamespace, "nats-0/other")
			}},
			{name: "unsafe namespace", run: func() error {
				return cluster.DeletePods(ctx, "kube-system", "app=x")
			}},
			{name: "negative replicas", run: func() error {
				return cluster.Scale(ctx, ReleaseNamespace, "statefulsets", "nats", -1)
			}},
			{name: "too many replicas", run: func() error {
				return cluster.Scale(ctx, ReleaseNamespace, "statefulsets", "nats", 11)
			}},
		}
		for _, testCase := range cases {
			if err := testCase.run(); err == nil {
				t.Fatalf("%s was admitted", testCase.name)
			}
		}
		if got := resilienceReadFileString(t, argv); got != "" {
			t.Fatalf("refused commands reached kubectl: %s", got)
		}
	})
}

// TestHarnessResilienceEventDelta proves the committed-event comparison detects
// loss, duplication and new events across a broker failure, independent of
// order.
func TestHarnessResilienceEventDelta(t *testing.T) {
	cases := []struct {
		name     string
		before   []string
		after    []string
		lost     []string
		dup      []string
		added    []string
		afterLen int
	}{
		{name: "identical", before: []string{"e1", "e2"}, after: []string{"e2", "e1"}},
		{name: "lost", before: []string{"e1", "e2", "e3"}, after: []string{"e1", "e3"}, lost: []string{"e2"}},
		{name: "duplicated", before: []string{"e1"}, after: []string{"e1", "e1"}, dup: []string{"e1"}},
		{name: "added", before: []string{"e1"}, after: []string{"e1", "e2"}, added: []string{"e2"}},
		{name: "all added", after: []string{"e1", "e2"}, added: []string{"e1", "e2"}},
		{name: "both empty"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			delta := resilienceEventDelta(testCase.before, testCase.after)
			if !resilienceSameStrings(delta.Lost, testCase.lost) {
				t.Fatalf("lost = %v, want %v", delta.Lost, testCase.lost)
			}
			if !resilienceSameStrings(delta.Duplicated, testCase.dup) {
				t.Fatalf("duplicated = %v, want %v", delta.Duplicated, testCase.dup)
			}
			if !resilienceSameStrings(delta.Added, testCase.added) {
				t.Fatalf("added = %v, want %v", delta.Added, testCase.added)
			}
			if delta.Before != len(testCase.before) || delta.After != len(testCase.after) {
				t.Fatalf("counts = %d/%d, want %d/%d", delta.Before, delta.After, len(testCase.before), len(testCase.after))
			}
		})
	}
}

// TestHarnessResilienceStopBarrier proves the replacement queue invariant: a
// replacement stays queued while the predecessor's stop is unconfirmed, and
// may run only after confirmation.
func TestHarnessResilienceStopBarrier(t *testing.T) {
	activated := time.Date(2026, 11, 8, 1, 0, 0, 0, time.UTC)
	action := func(name, state string, activatedAt *time.Time) api.ExecutionAction {
		return api.ExecutionAction{Name: name, Kind: "container", State: state, Version: 1, ActivatedAt: activatedAt}
	}
	cases := []struct {
		name      string
		stopState string
		actions   []api.ExecutionAction
		wantErr   bool
	}{
		{name: "pending action is preparation", stopState: "unconfirmed", actions: []api.ExecutionAction{action("resist", "pending", nil)}},
		{name: "no actions yet", stopState: "unconfirmed"},
		{name: "running action", stopState: "unconfirmed", actions: []api.ExecutionAction{action("resist", "running", &activated)}, wantErr: true},
		{name: "succeeded action was activated", stopState: "unconfirmed", actions: []api.ExecutionAction{action("resist", "succeeded", &activated)}, wantErr: true},
		{name: "activated pending action", stopState: "unconfirmed", actions: []api.ExecutionAction{action("resist", "pending", &activated)}, wantErr: true},
		{name: "confirmed running", stopState: "confirmed", actions: []api.ExecutionAction{action("resist", "running", &activated)}},
		{name: "confirmed pending", stopState: "confirmed", actions: []api.ExecutionAction{action("resist", "pending", nil)}},
		{name: "no stop yet pending", stopState: "", actions: []api.ExecutionAction{action("resist", "pending", nil)}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			formatted := resilienceFormatActions(testCase.actions)
			if testCase.stopState == "unconfirmed" && strings.Contains(formatted, "activated_at=") {
				// The failure message carries every action state and activation
				// time, so a real violation is distinguishable from a state name.
				if !strings.Contains(formatted, "state=pending") && !strings.Contains(formatted, "state=running") && !strings.Contains(formatted, "state=succeeded") {
					t.Fatalf("barrier action facts %q do not carry a state", formatted)
				}
			}
			err := resilienceCheckStopBarrier(testCase.stopState, testCase.actions)
			if testCase.wantErr && err == nil {
				t.Fatalf("stop %q actions %+v were accepted", testCase.stopState, testCase.actions)
			}
			if !testCase.wantErr && err != nil {
				t.Fatalf("stop %q actions %+v were refused: %v", testCase.stopState, testCase.actions, err)
			}
		})
	}
}

// TestHarnessResiliencePaginatedReads proves the committed-event and log reads
// follow the opaque cursor, stop at the last page, and refuse a non-advancing
// cursor or an unbounded read.
func TestHarnessResiliencePaginatedReads(t *testing.T) {
	eventPages := []api.EventReadResult{
		{Events: []api.Event{{ID: "e1"}, {ID: "e2"}}, Next: "c1", HasMore: true},
		{Events: []api.Event{{ID: "e3"}}, Next: "c2"},
	}
	events, err := resiliencePaginateEvents(func(cursor string) (api.EventReadResult, error) {
		switch cursor {
		case "":
			return eventPages[0], nil
		case "c1":
			return eventPages[1], nil
		default:
			t.Fatalf("unexpected event cursor %q", cursor)
			return api.EventReadResult{}, nil
		}
	}, 5)
	if err != nil {
		t.Fatalf("paginate events: %v", err)
	}
	if got := resilienceEventIDs(events); !resilienceSameStrings(got, []string{"e1", "e2", "e3"}) {
		t.Fatalf("events = %v, want e1,e2,e3", got)
	}

	if _, err := resiliencePaginateEvents(func(string) (api.EventReadResult, error) {
		return api.EventReadResult{Next: "c1", HasMore: true}, nil
	}, 5); err == nil || !strings.Contains(err.Error(), "did not advance") {
		t.Fatalf("non-advancing event cursor error = %v, want a refusal", err)
	}
	if _, err := resiliencePaginateEvents(func(string) (api.EventReadResult, error) {
		return api.EventReadResult{Next: "c1", HasMore: true}, nil
	}, 1); err == nil {
		t.Fatal("an event read beyond its page bound was accepted")
	}

	logPages := []api.ExecutionLogsReadResult{
		{Chunks: []api.LogChunk{{Data: []byte("one")}}, NextCursor: "l1"},
		{Chunks: []api.LogChunk{{Data: []byte("two")}}},
	}
	chunks, err := resiliencePaginateLogs(func(cursor string) (api.ExecutionLogsReadResult, error) {
		switch cursor {
		case "":
			return logPages[0], nil
		case "l1":
			return logPages[1], nil
		default:
			t.Fatalf("unexpected log cursor %q", cursor)
			return api.ExecutionLogsReadResult{}, nil
		}
	}, 5)
	if err != nil {
		t.Fatalf("paginate logs: %v", err)
	}
	if len(chunks) != 2 || string(chunks[0].Data) != "one" || string(chunks[1].Data) != "two" {
		t.Fatalf("log chunks = %+v, want one,two", chunks)
	}
}

// TestHarnessResilienceWaiter proves the polling helper measures elapsed time
// and bounds both a never-satisfied condition and a failed observation.
func TestHarnessResilienceWaiter(t *testing.T) {
	now := time.Unix(0, 0)
	waiter := resilienceWaiter{
		Interval: time.Second,
		Timeout:  5 * time.Second,
		Now:      func() time.Time { return now },
		Sleep:    func(interval time.Duration) { now = now.Add(interval) },
	}
	calls := 0
	elapsed, err := waiter.Wait(context.Background(), func(context.Context) (bool, error) {
		calls++
		return calls >= 3, nil
	})
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if calls != 3 || elapsed != 2*time.Second {
		t.Fatalf("calls/elapsed = %d/%s, want 3/2s", calls, elapsed)
	}

	now = time.Unix(0, 0)
	elapsed, err = waiter.Wait(context.Background(), func(context.Context) (bool, error) {
		return false, nil
	})
	if err == nil || !strings.Contains(err.Error(), "within 5s") {
		t.Fatalf("timeout error = %v, want a bounded refusal", err)
	}
	if elapsed != 5*time.Second {
		t.Fatalf("timeout elapsed = %s, want 5s", elapsed)
	}

	now = time.Unix(0, 0)
	sentinel := context.Canceled
	if _, err := waiter.Wait(context.Background(), func(context.Context) (bool, error) {
		return false, sentinel
	}); err != sentinel {
		t.Fatalf("observation error = %v, want the sentinel", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := waiter.Wait(cancelled, func(context.Context) (bool, error) {
		t.Fatal("a cancelled wait still observed")
		return false, nil
	}); err == nil {
		t.Fatal("a cancelled wait returned success")
	}

	broken := resilienceWaiter{Interval: 0, Timeout: 0}
	if _, err := broken.Wait(context.Background(), func(context.Context) (bool, error) {
		return true, nil
	}); err == nil {
		t.Fatal("a waiter without a positive timeout was accepted")
	}
}

// TestHarnessResilienceEvidenceFields proves the summary builder emits only
// allowlisted evidence: identities, timestamps, durations and outcomes, never
// log text or credentials.
func TestHarnessResilienceEvidenceFields(t *testing.T) {
	started := time.Date(2026, 11, 8, 1, 2, 3, 0, time.UTC)
	finished := started.Add(1500 * time.Millisecond)
	fields, err := resilienceEvidenceFields(resilienceEvidenceInput{
		Probe:       "TestStageResilienceControllerPodDeletion",
		Outcome:     "passed",
		Restoration: "restored",
		Started:     started,
		Finished:    finished,
		Fields: map[string]any{
			"execution_id": "exec-1",
			"resource_id":  "kodama-abc",
		},
	})
	if err != nil {
		t.Fatalf("build evidence fields: %v", err)
	}
	for key := range fields {
		if !evidenceFields[key] {
			t.Fatalf("evidence carries non-allowlisted field %q", key)
		}
	}
	if fields["duration_ms"] != int64(1500) {
		t.Fatalf("duration_ms = %v, want 1500", fields["duration_ms"])
	}
	if fields["outcome"] != "passed" || fields["restoration"] != "restored" {
		t.Fatalf("outcome/restoration = %v/%v", fields["outcome"], fields["restoration"])
	}
	if _, ok := fields["execution_id"]; !ok {
		t.Fatal("evidence lost the execution identity")
	}

	for _, testCase := range []struct {
		name  string
		input resilienceEvidenceInput
		want  string
	}{
		{
			name:  "unknown outcome",
			input: resilienceEvidenceInput{Probe: "probe", Outcome: "probably", Restoration: "not_applicable", Started: started, Finished: finished},
			want:  "outcome",
		},
		{
			name:  "unknown restoration",
			input: resilienceEvidenceInput{Probe: "probe", Outcome: "passed", Restoration: "maybe", Started: started, Finished: finished},
			want:  "restoration",
		},
		{
			name:  "unknown field",
			input: resilienceEvidenceInput{Probe: "probe", Outcome: "passed", Restoration: "not_applicable", Started: started, Finished: finished, Fields: map[string]any{"logs": "raw log text"}},
			want:  "allowlisted",
		},
		{
			name:  "reserved field override",
			input: resilienceEvidenceInput{Probe: "probe", Outcome: "passed", Restoration: "not_applicable", Started: started, Finished: finished, Fields: map[string]any{"outcome": "failed"}},
			want:  "reserved",
		},
		{
			name:  "negative interval",
			input: resilienceEvidenceInput{Probe: "probe", Outcome: "passed", Restoration: "not_applicable", Started: finished, Finished: started},
			want:  "before",
		},
		{
			name:  "empty probe",
			input: resilienceEvidenceInput{Outcome: "passed", Restoration: "not_applicable", Started: started, Finished: finished},
			want:  "probe",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := resilienceEvidenceFields(testCase.input); err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error = %v, want one containing %q", err, testCase.want)
			}
		})
	}
}

// TestHarnessResilienceInlineStartEnvelope proves the inline pipeline start
// uses the frozen alternate route and the frozen envelope, including an absent
// expected_version and the required idempotency key.
func TestHarnessResilienceInlineStartEnvelope(t *testing.T) {
	type captured struct {
		method string
		path   string
		header string
		body   api.Request
	}
	got := make(chan captured, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		data, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		var envelope api.Request
		if err := json.Unmarshal(data, &envelope); err != nil {
			t.Errorf("decode envelope: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		got <- captured{method: request.Method, path: request.URL.Path, header: request.Header.Get("Authorization"), body: envelope}
		resilienceWriteResult(t, writer, "pipeline.start", map[string]any{"execution_id": "exec-1", "version": 1})
	}))
	defer server.Close()

	client := resilienceTestAPIClient(t, server.URL)
	result, err := resilienceInlineStart(context.Background(), client, Secret("admin-token"), "project-1", []byte("actions: []\n"), "inline-key-1")
	if err != nil {
		t.Fatalf("inline start: %v", err)
	}
	if result.ExecutionID != "exec-1" || result.Version != 1 {
		t.Fatalf("inline start result = %+v", result)
	}
	request := <-got
	if request.method != http.MethodPost {
		t.Fatalf("method = %s, want POST", request.method)
	}
	if request.path != "/api/v1/projects/project-1/executions" {
		t.Fatalf("path = %q, want the frozen inline executions route", request.path)
	}
	if request.header != "Bearer admin-token" {
		t.Fatalf("authorization header = %q", request.header)
	}
	if request.body.ExpectedVersion != nil {
		t.Fatalf("inline envelope carries expected_version %v", *request.body.ExpectedVersion)
	}
	if request.body.IdempotencyKey != "inline-key-1" {
		t.Fatalf("idempotency key = %q", request.body.IdempotencyKey)
	}
	var input api.PipelineStartRequest
	if err := json.Unmarshal(request.body.Input, &input); err != nil {
		t.Fatalf("decode inline input: %v", err)
	}
	if input.ProjectID != "project-1" || string(input.YAML) != "actions: []\n" {
		t.Fatalf("inline input = %+v", input)
	}
}

// TestHarnessResilienceVersionedEnvelope proves a version-guarded public
// mutation carries the caller's expected_version, which is how the handoff
// probe asks and replies at the exact task lifecycle version.
func TestHarnessResilienceVersionedEnvelope(t *testing.T) {
	got := make(chan api.Request, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		data, _ := io.ReadAll(request.Body)
		var envelope api.Request
		if err := json.Unmarshal(data, &envelope); err != nil {
			t.Errorf("decode envelope: %v", err)
		}
		got <- envelope
		resilienceWriteResult(t, writer, "task.ask", map[string]any{"lifecycle_version": 4, "question_version": 2})
	}))
	defer server.Close()

	client := resilienceTestAPIClient(t, server.URL)
	result, err := resilienceMutateVersioned(context.Background(), client, Secret("admin-token"), "task.ask", api.TaskAskRequest{TaskID: "task-1"}, 7, "ask-key")
	if err != nil {
		t.Fatalf("versioned post: %v", err)
	}
	if result.Operation != "task.ask" {
		t.Fatalf("operation = %q", result.Operation)
	}
	envelope := <-got
	if envelope.ExpectedVersion == nil || *envelope.ExpectedVersion != 7 {
		t.Fatalf("expected_version = %v, want 7", envelope.ExpectedVersion)
	}
	if envelope.IdempotencyKey != "ask-key" {
		t.Fatalf("idempotency key = %q", envelope.IdempotencyKey)
	}
	if _, err := resilienceMutateVersioned(context.Background(), client, Secret("admin-token"), "task.ask", api.TaskAskRequest{TaskID: "task-1"}, 0, "ask-key"); err == nil {
		t.Fatal("an expected version of zero was accepted")
	}
}

// TestHarnessResilienceMarkersAndParsers proves the log-marker counter and the
// bounded parsers used by the live probes.
func TestHarnessResilienceMarkersAndParsers(t *testing.T) {
	count, err := resilienceCountMarker([]byte("x RESILIENCE y\nRESILIENCE\n"), "RESILIENCE")
	if err != nil {
		t.Fatalf("count marker: %v", err)
	}
	if count != 2 {
		t.Fatalf("marker count = %d, want 2", count)
	}
	if _, err := resilienceCountMarker([]byte("x"), ""); err == nil {
		t.Fatal("an empty marker was counted")
	}

	names, err := resilienceParseNameList([]byte("pod/a\njob/b\n\n"))
	if err != nil {
		t.Fatalf("parse names: %v", err)
	}
	if !resilienceSameStrings(names, []string{"a", "b"}) {
		t.Fatalf("names = %v, want a,b", names)
	}
	if _, err := resilienceParseNameList([]byte("pod/a/other\n")); err == nil {
		t.Fatal("a name with two separators was accepted")
	}

	replicas, err := resilienceParseReplicas([]byte(" 3\n"))
	if err != nil || replicas != 3 {
		t.Fatalf("replicas = %d, err = %v, want 3", replicas, err)
	}
	for _, raw := range []string{"", "x", "-1", "3\n4"} {
		if _, err := resilienceParseReplicas([]byte(raw)); err == nil {
			t.Fatalf("replicas %q was accepted", raw)
		}
	}

	elapsed, err := resilienceElapsed(time.Unix(10, 0), time.Unix(12, 0))
	if err != nil || elapsed != 2*time.Second {
		t.Fatalf("elapsed = %s, err = %v, want 2s", elapsed, err)
	}
	if _, err := resilienceElapsed(time.Unix(12, 0), time.Unix(10, 0)); err == nil {
		t.Fatal("a negative interval was accepted")
	}
}

// TestHarnessResiliencePipelineYAML proves the generated pipeline document is
// valid YAML with the pinned image, timeout and shell commands the probes rely
// on.
func TestHarnessResiliencePipelineYAML(t *testing.T) {
	image := "rg.example.test/kodama@sha256:" + repeat("a", 64)
	document := resiliencePipelineYAML(image, "resist", "90s", []string{"echo marker\nsleep 5"})
	if len(document) == 0 {
		t.Fatal("pipeline yaml is empty")
	}
	if err := (api.PipelineApplyRequest{Name: "resilience-yaml", YAML: document}).Validate(); err != nil {
		t.Fatalf("generated pipeline yaml is not admissible: %v", err)
	}
	if err := (api.PipelineStartRequest{YAML: document}).Validate(); err != nil {
		t.Fatalf("generated inline pipeline yaml is not admissible: %v", err)
	}
	text := string(document)
	for _, want := range []string{image, "timeout: 90s", `name: "resist"`, "echo marker", "sleep 5"} {
		if !strings.Contains(text, want) {
			t.Fatalf("pipeline yaml does not contain %q:\n%s", want, text)
		}
	}
}

func resilienceFakeCluster(t *testing.T, options resilienceFakeOptions) (resilienceCluster, *resilienceFakeState, string) {
	t.Helper()
	state := &resilienceFakeState{dir: t.TempDir()}
	argv := filepath.Join(state.dir, "argv")
	if err := os.WriteFile(filepath.Join(state.dir, "replicas"), []byte(strconv.Itoa(options.Replicas)), 0o600); err != nil {
		t.Fatalf("seed fake replicas: %v", err)
	}
	for marker, enabled := range map[string]bool{
		"fail-zero":         options.FailScaleToZero,
		"fail-restore-once": options.FailRestoreOnce,
		"fail-restore":      options.FailRestoreAlways,
		"fail-read":         options.FailReplicasRead,
		"lie":               options.LieOnScale,
	} {
		if !enabled {
			continue
		}
		if err := os.WriteFile(filepath.Join(state.dir, marker), nil, 0o600); err != nil {
			t.Fatalf("seed fake marker %s: %v", marker, err)
		}
	}
	t.Setenv("RES_FAKE_STATE", state.dir)
	t.Setenv("RES_FAKE_ARGV", argv)
	installFakeExecutable(t, "kubectl", `#!/bin/sh
printf '%s\n' "$*" >> "$RES_FAKE_ARGV"
case "$*" in
  *" get statefulsets nats "*)
    if [ -f "$RES_FAKE_STATE/fail-read" ]; then exit 1; fi
    cat "$RES_FAKE_STATE/replicas"
    exit 0
    ;;
  *" scale statefulsets nats "*)
    replicas=""
    for arg in "$@"; do
      case "$arg" in --replicas=*) replicas="${arg#--replicas=}";; esac
    done
    [ -n "$replicas" ] || exit 2
    if [ "$replicas" = "0" ] && [ -f "$RES_FAKE_STATE/fail-zero" ]; then exit 1; fi
    if [ "$replicas" = "3" ] && [ -f "$RES_FAKE_STATE/fail-restore-once" ]; then rm -f "$RES_FAKE_STATE/fail-restore-once"; exit 1; fi
    if [ "$replicas" = "3" ] && [ -f "$RES_FAKE_STATE/fail-restore" ]; then exit 1; fi
    if [ "$replicas" = "3" ] && [ -f "$RES_FAKE_STATE/lie" ]; then echo 4 > "$RES_FAKE_STATE/replicas"; exit 0; fi
    printf '%s' "$replicas" > "$RES_FAKE_STATE/replicas"
    exit 0
    ;;
esac
exit 0
`)
	guard, err := NewGuard("kodama-stage-k8s", "kodama-tenant-tenant-a")
	if err != nil {
		t.Fatalf("build guard: %v", err)
	}
	return resilienceNewCluster(&Kubectl{Guard: guard, Path: "kubectl", Timeout: 5 * time.Second}), state, argv
}

func resilienceRecordingCluster(t *testing.T) (resilienceCluster, string) {
	t.Helper()
	argv := filepath.Join(t.TempDir(), "argv")
	t.Setenv("RES_FAKE_ARGV", argv)
	installFakeExecutable(t, "kubectl", `#!/bin/sh
printf '%s\n' "$*" >> "$RES_FAKE_ARGV"
case "$*" in
  *" get pods nats-0 -o jsonpath={.metadata.uid}"*)
    printf '2f1c7b9e-0f5a-4a5c-9a54-0f6f4a1c8d21'
    ;;
esac
exit 0
`)
	guard, err := NewGuard("kodama-stage-k8s", "kodama-tenant-tenant-a")
	if err != nil {
		t.Fatalf("build guard: %v", err)
	}
	return resilienceNewCluster(&Kubectl{Guard: guard, Path: "kubectl", Timeout: 5 * time.Second}), argv
}

type resilienceFakeState struct {
	dir string
}

func (state *resilienceFakeState) replicas(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(state.dir, "replicas"))
	if err != nil {
		t.Fatalf("read fake replicas: %v", err)
	}
	value, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse fake replicas %q: %v", data, err)
	}
	return value
}

func (state *resilienceFakeState) argv(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(state.dir, "argv"))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatalf("read fake argv: %v", err)
	}
	return string(data)
}

func (state *resilienceFakeState) argCount(t *testing.T, token string) int {
	t.Helper()
	return strings.Count(state.argv(t), token)
}

func resilienceReadFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func resilienceTestAPIClient(t *testing.T, rawURL string) *API {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse test URL: %v", err)
	}
	transport := &http.Transport{Proxy: nil}
	return &API{
		base:           parsed,
		client:         &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		maxResponse:    api.MaxResponseBytes,
		requestTimeout: 5 * time.Second,
	}
}

func TestHarnessClaimGoneOrTerminatingDistinguishesErrorsFromAbsence(t *testing.T) {
	response := filepath.Join(t.TempDir(), "pvc-response.json")
	failFile := filepath.Join(t.TempDir(), "pvc-fail")
	argv := filepath.Join(t.TempDir(), "argv")
	t.Setenv("RES_PVC_RESPONSE", response)
	t.Setenv("RES_PVC_FAIL", failFile)
	t.Setenv("RES_PVC_ARGV", argv)
	installFakeExecutable(t, "kubectl", `#!/bin/sh
printf '%s\n' "$*" >> "$RES_PVC_ARGV"
if [ -f "$RES_PVC_FAIL" ]; then exit 1; fi
cat "$RES_PVC_RESPONSE"
`)
	guard, err := NewGuard("kodama-stage-k8s", "kodama-tenant-tenant-a")
	if err != nil {
		t.Fatalf("build guard: %v", err)
	}
	cluster := resilienceNewCluster(&Kubectl{Guard: guard, Path: "kubectl", Timeout: 5 * time.Second})
	ctx := context.Background()
	const ns = "kodama-tenant-tenant-a"

	if gone, err := cluster.ClaimGoneOrTerminating(ctx, ns, "../bad-name"); err == nil || gone {
		t.Fatalf("unsafe claim name = %v, %v; want false, error", gone, err)
	}

	if err := os.WriteFile(failFile, nil, 0o600); err != nil {
		t.Fatalf("write fail marker: %v", err)
	}
	if gone, err := cluster.ClaimGoneOrTerminating(ctx, ns, "workspace-claim"); err == nil || gone {
		t.Fatalf("kubectl failure = %v, %v; want false, error (never mistaken for absence)", gone, err)
	}
	_ = os.Remove(failFile)

	for _, testCase := range []struct {
		name string
		body string
		want bool
	}{
		{"empty items means gone", `{"items":[]}`, true},
		{"live claim without deletionTimestamp", `{"items":[{"metadata":{"name":"workspace-claim"}}]}`, false},
		{"terminating claim with deletionTimestamp", `{"items":[{"metadata":{"name":"workspace-claim","deletionTimestamp":"2026-11-11T00:00:00Z"}}]}`, true},
	} {
		if err := os.WriteFile(response, []byte(testCase.body), 0o600); err != nil {
			t.Fatalf("write response: %v", err)
		}
		got, err := cluster.ClaimGoneOrTerminating(ctx, ns, "workspace-claim")
		if err != nil || got != testCase.want {
			t.Fatalf("%s: got (%v, %v); want (%v, nil)", testCase.name, got, err, testCase.want)
		}
	}
	if recorded := resilienceReadFileString(t, argv); !strings.Contains(recorded, "--field-selector=metadata.name=workspace-claim") {
		t.Fatalf("kubectl argv = %q; want --field-selector=metadata.name=workspace-claim", recorded)
	}
}

func resilienceWriteError(t *testing.T, writer http.ResponseWriter, status int, code string) {
	t.Helper()
	data, err := json.Marshal(api.Error{Code: code, Message: "the caller does not hold the assigned role"})
	if err != nil {
		t.Errorf("encode fake public error: %v", err)
		return
	}
	writer.Header().Set("Content-Type", api.MediaType)
	writer.WriteHeader(status)
	if _, err := writer.Write(data); err != nil {
		t.Errorf("write fake public error: %v", err)
	}
}

func resilienceWriteResult(t *testing.T, writer http.ResponseWriter, operation string, body map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Errorf("encode result body: %v", err)
		return
	}
	result := api.Result{Operation: operation, Body: encoded}
	data, err := json.Marshal(result)
	if err != nil {
		t.Errorf("encode result: %v", err)
		return
	}
	writer.Header().Set("Content-Type", api.MediaType)
	writer.WriteHeader(http.StatusOK)
	if _, err := writer.Write(data); err != nil {
		t.Errorf("write result: %v", err)
	}
}

func resilienceEventIDs(events []api.Event) []string {
	ids := make([]string, 0, len(events))
	for _, event := range events {
		ids = append(ids, event.ID)
	}
	return ids
}

func resilienceSameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// TestHarnessResilienceFreshAccessService proves the fresh controller-deletion
// access path uses a bounded service name and the documented stage default.
func TestHarnessResilienceFreshAccessService(t *testing.T) {
	t.Setenv("Q02_KODAMA_SERVICE", "")
	service, err := resilienceKodamaService()
	if err != nil || service != "kodama" {
		t.Fatalf("default service = %q, err = %v; want kodama", service, err)
	}
	t.Setenv("Q02_KODAMA_SERVICE", "kodama-api")
	service, err = resilienceKodamaService()
	if err != nil || service != "kodama-api" {
		t.Fatalf("configured service = %q, err = %v; want kodama-api", service, err)
	}
	for _, value := range []string{"Bad_Service", "kodama/other", "--namespace=other"} {
		t.Setenv("Q02_KODAMA_SERVICE", value)
		if _, err := resilienceKodamaService(); err == nil {
			t.Fatalf("service %q was accepted", value)
		}
	}
}

// TestHarnessResilienceEvidenceNames proves a probe without phase records still
// writes a lower-case, bounded evidence file name.
func TestHarnessResilienceEvidenceNames(t *testing.T) {
	name := resilienceStageProbePrefix("TestStageResilienceControllerPodDeletion")
	if !evidenceNamePattern.MatchString(name) {
		t.Fatalf("fallback evidence name %q is not a bounded lower-case file name", name)
	}
	if !strings.HasPrefix(name, "resilience-") {
		t.Fatalf("fallback evidence name %q does not carry the resilience prefix", name)
	}
	for _, record := range []string{
		"resilience-controller-api-availability",
		"resilience-controller-pod-ready",
		"resilience-nats-restart",
		"resilience-broker-outage",
		"resilience-handoff-pending",
		"resilience-cleanup-terminal",
	} {
		if !evidenceNamePattern.MatchString(record) {
			t.Fatalf("evidence record name %q is not bounded", record)
		}
	}
}

// resilienceFakeTesting records the cleanup registrations and errors of the
// real resilience cleanup path without a *testing.T.
type resilienceFakeTesting struct {
	cleanups []func()
	errors   []string
}

func (fake *resilienceFakeTesting) Helper() {}

func (fake *resilienceFakeTesting) Cleanup(cleanup func()) {
	fake.cleanups = append(fake.cleanups, cleanup)
}

func (fake *resilienceFakeTesting) Errorf(format string, args ...any) {
	fake.errors = append(fake.errors, fmt.Sprintf(format, args...))
}

// TestHarnessResilienceCleanupRestoresAfterSuccessfulScale drives the real
// registration and cleanup path after a successful scale to zero. A probe that
// dies mid-outage must still restore the saved count with a read-back.
func TestHarnessResilienceCleanupRestoresAfterSuccessfulScale(t *testing.T) {
	cluster, state, _ := resilienceFakeCluster(t, resilienceFakeOptions{Replicas: 3})
	guard := resilienceNewReplicaGuard(cluster)
	ctx := context.Background()
	if _, err := guard.Save(ctx, ReleaseNamespace, "statefulsets", "nats"); err != nil {
		t.Fatalf("save replicas: %v", err)
	}
	if err := guard.Scale(ctx, ReleaseNamespace, "statefulsets", "nats", 0); err != nil {
		t.Fatalf("scale to zero: %v", err)
	}
	if got := state.replicas(t); got != 0 {
		t.Fatalf("replicas after the successful scale = %d, want 0", got)
	}
	if guard.AllRestored() {
		t.Fatal("a successful scale was reported as a verified restoration")
	}

	fake := &resilienceFakeTesting{}
	resilienceRestoreOnCleanup(fake, nil, guard)
	if len(fake.cleanups) != 1 {
		t.Fatalf("cleanup registrations = %d, want exactly one", len(fake.cleanups))
	}
	fake.cleanups[0]()
	if len(fake.errors) != 0 {
		t.Fatalf("the real cleanup path reported errors: %v", fake.errors)
	}
	if got := state.replicas(t); got != 3 {
		t.Fatalf("replicas after cleanup = %d, want the saved 3", got)
	}
	if !guard.AllRestored() {
		t.Fatal("cleanup did not record the verified restoration")
	}
	if count := state.argCount(t, "--replicas=3"); count != 1 {
		t.Fatalf("restore scale calls = %d, want exactly one scale back to 3", count)
	}
}

// TestHarnessResilienceOutageRestoreSurvivesCancellation proves an already
// cancelled or expired outage context still ends in a verified restoration.
func TestHarnessResilienceOutageRestoreSurvivesCancellation(t *testing.T) {
	for _, testCase := range []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
	}{
		{
			name: "cancelled",
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, cancel
			},
		},
		{
			name: "expired",
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			cluster, state, _ := resilienceFakeCluster(t, resilienceFakeOptions{Replicas: 3})
			guard := resilienceNewReplicaGuard(cluster)
			if _, err := guard.Save(context.Background(), ReleaseNamespace, "statefulsets", "nats"); err != nil {
				t.Fatalf("save replicas: %v", err)
			}
			if err := guard.Scale(context.Background(), ReleaseNamespace, "statefulsets", "nats", 0); err != nil {
				t.Fatalf("scale to zero: %v", err)
			}
			if got := state.replicas(t); got != 0 {
				t.Fatalf("replicas before the %s outage restore = %d, want 0", testCase.name, got)
			}
			outageCtx, cancel := testCase.ctx()
			defer cancel()
			restoration, err := resilienceCloseOutage(outageCtx, guard, 10*time.Second)
			if err != nil {
				t.Fatalf("restore after a %s outage context: %v", testCase.name, err)
			}
			if restoration != "restored" {
				t.Fatalf("restoration = %q, want restored", restoration)
			}
			if got := state.replicas(t); got != 3 {
				t.Fatalf("replicas after the %s outage restore = %d, want 3", testCase.name, got)
			}
			if !guard.AllRestored() {
				t.Fatalf("the %s outage restore is not verified", testCase.name)
			}
		})
	}
}

// TestHarnessResilienceBrokerEntryGuard proves the outage refuses to scale a
// statefulset already left at zero and prints the exact manual restore command.
func TestHarnessResilienceBrokerEntryGuard(t *testing.T) {
	command := "kubectl --context kodama-stage-k8s -n kodama-system scale statefulset nats --replicas=<n>"
	if err := resilienceCheckBrokerEntry(1, "kodama-stage-k8s"); err != nil {
		t.Fatalf("a healthy statefulset was refused: %v", err)
	}
	err := resilienceCheckBrokerEntry(0, "kodama-stage-k8s")
	if err == nil {
		t.Fatal("a statefulset already at zero replicas was accepted")
	}
	if !strings.Contains(err.Error(), command) {
		t.Fatalf("entry refusal %q does not print the manual restore command %q", err, command)
	}
	if !strings.Contains(err.Error(), "already at 0") {
		t.Fatalf("entry refusal %q does not name the zero-replica state", err)
	}

	cluster, state, _ := resilienceFakeCluster(t, resilienceFakeOptions{Replicas: 0})
	guard := resilienceNewReplicaGuard(cluster)
	saved, saveErr := guard.Save(context.Background(), ReleaseNamespace, "statefulsets", "nats")
	if saveErr != nil {
		t.Fatalf("save zero replicas: %v", saveErr)
	}
	if err := resilienceCheckBrokerEntry(saved, "kodama-stage-k8s"); err == nil {
		t.Fatal("the probe entry accepted a statefulset already at zero replicas")
	}
	if got := state.argCount(t, "scale "); got != 0 {
		t.Fatalf("entry refusal executed %d scale command(s)", got)
	}
}

// TestHarnessResilienceBrokerOutagePlan proves a short go test deadline
// refuses before any scale, and a sufficient one returns the bounded window.
func TestHarnessResilienceBrokerOutagePlan(t *testing.T) {
	now := time.Date(2026, 11, 8, 1, 0, 0, 0, time.UTC)
	window, err := resiliencePlanBrokerOutage(now, now.Add(7*time.Minute), true)
	if err != nil {
		t.Fatalf("a sufficient deadline was refused: %v", err)
	}
	if window != 4*time.Minute {
		t.Fatalf("outage window = %s, want 4m", window)
	}
	for _, testCase := range []struct {
		name        string
		deadline    time.Time
		hasDeadline bool
	}{
		{name: "no go test deadline"},
		{name: "short deadline", deadline: now.Add(6*time.Minute + 59*time.Second), hasDeadline: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := resiliencePlanBrokerOutage(now, testCase.deadline, testCase.hasDeadline); err == nil {
				t.Fatal("an insufficient deadline was accepted")
			} else if !strings.Contains(err.Error(), "refusing to scale") {
				t.Fatalf("deadline refusal %q does not refuse the scale", err)
			} else if !strings.Contains(err.Error(), "kubectl --context") {
				t.Fatalf("deadline refusal %q does not print the manual restore command", err)
			}
		})
	}
}

// TestHarnessResilienceReplicaSaveReadFailure proves a failed original-count
// read saves no target and issues no scale.
func TestHarnessResilienceReplicaSaveReadFailure(t *testing.T) {
	cluster, state, _ := resilienceFakeCluster(t, resilienceFakeOptions{
		Replicas:         3,
		FailReplicasRead: true,
	})
	guard := resilienceNewReplicaGuard(cluster)
	saved, err := guard.Save(context.Background(), ReleaseNamespace, "statefulsets", "nats")
	if err == nil {
		t.Fatalf("a failed replica read returned saved=%d", saved)
	}
	if len(guard.Targets()) != 0 {
		t.Fatalf("a failed replica read saved targets: %+v", guard.Targets())
	}
	if got := state.argCount(t, " scale "); got != 0 {
		t.Fatalf("a failed replica read issued %d scale command(s)", got)
	}
	if err := guard.Scale(context.Background(), ReleaseNamespace, "statefulsets", "nats", 0); err == nil {
		t.Fatal("an unsaved target was scaled after the failed read")
	}
}

// TestHarnessResilienceControllerSelector proves the narrowed controller
// selector is admitted by the guard's selector grammar and excludes a
// non-control-plane pod.
func TestHarnessResilienceControllerSelector(t *testing.T) {
	if !selectorPattern.MatchString(resilienceKodamaSelector) {
		t.Fatalf("controller selector %q is not admitted by the guard selector grammar", resilienceKodamaSelector)
	}
	if !strings.Contains(resilienceKodamaSelector, "app.kubernetes.io/component=control-plane") {
		t.Fatalf("controller selector %q does not narrow to the control-plane component", resilienceKodamaSelector)
	}
	if err := resilienceValidateSelector(resilienceKodamaSelector); err != nil {
		t.Fatalf("controller selector %q is refused by the harness: %v", resilienceKodamaSelector, err)
	}
}

// TestHarnessResilienceRetainedClaim proves retained-claim parsing tolerates
// the documented retention marker and refuses an unreadable object.
func TestHarnessResilienceRetainedClaim(t *testing.T) {
	retained, err := resilienceClaimRetained([]byte(`{"metadata":{"labels":{"kodama.dev/retained":"true"}}}`))
	if err != nil || !retained {
		t.Fatalf("retained claim = %v, err = %v; want true", retained, err)
	}
	retained, err = resilienceClaimRetained([]byte(`{"metadata":{"labels":{"kodama.dev/workspace":"ws-1"}}}`))
	if err != nil || retained {
		t.Fatalf("unretained claim = %v, err = %v; want false", retained, err)
	}
	retained, err = resilienceClaimRetained([]byte(`{"metadata":{"labels":{"kodama.dev/workspace":"ws-1"},"annotations":{"kodama.dev/retained":"true"}}}`))
	if err != nil || !retained {
		t.Fatalf("annotation-retained claim = %v, err = %v; want true", retained, err)
	}
	if _, err := resilienceClaimRetained([]byte("not json")); err == nil {
		t.Fatal("an unreadable claim document was accepted")
	}
}

// TestHarnessResilienceBrokerProbeOrder pins the broker probe's safety order:
// the restore is registered in cleanup BEFORE the first scale, so a test that
// dies mid-outage still restores NATS.
func TestHarnessResilienceBrokerProbeOrder(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "resilience_broker_test.go", nil, 0)
	if err != nil {
		t.Fatalf("parse the broker probe: %v", err)
	}
	restoreRegistration := -1
	firstScale := -1
	outageBody := -1
	closeOutage := -1
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch function := call.Fun.(type) {
		case *ast.Ident:
			if function.Name == "resilienceRestoreOnCleanup" && restoreRegistration < 0 {
				restoreRegistration = int(call.Pos())
			}
			if function.Name == "resilienceRunBrokerOutage" && outageBody < 0 {
				outageBody = int(call.Pos())
			}
			if function.Name == "resilienceCloseOutage" && closeOutage < 0 {
				closeOutage = int(call.Pos())
			}
		case *ast.SelectorExpr:
			guard, ok := function.X.(*ast.Ident)
			if ok && guard.Name == "guard" && function.Sel.Name == "Scale" && firstScale < 0 {
				firstScale = int(call.Pos())
			}
		}
		return true
	})
	if restoreRegistration < 0 {
		t.Fatal("the broker probe never registers resilienceRestoreOnCleanup")
	}
	if firstScale < 0 {
		t.Fatal("the broker probe never scales a saved target")
	}
	if outageBody < 0 {
		t.Fatal("the broker probe never runs the outage body")
	}
	if closeOutage < 0 {
		t.Fatal("the broker probe never restores in body")
	}
	if restoreRegistration > outageBody || restoreRegistration > firstScale {
		t.Fatal("the broker probe registers its restore AFTER it enters the scale-bearing outage body")
	}
	if closeOutage < outageBody {
		t.Fatal("the broker probe restores before the outage body ends")
	}
}

// TestHarnessResilienceHandoffReadOrder pins the stop-barrier read order: the
// replacement is read before the predecessor so a confirmation landing between
// the reads cannot raise a false violation.
func TestHarnessResilienceHandoffReadOrder(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "resilience_handoff_test.go", nil, 0)
	if err != nil {
		t.Fatalf("parse the handoff probe: %v", err)
	}
	replacementRead := -1
	predecessorRead := -1
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) < 3 {
			return true
		}
		function, ok := call.Fun.(*ast.Ident)
		if !ok || function.Name != "resilienceInspect" {
			return true
		}
		target, ok := call.Args[2].(*ast.Ident)
		if !ok {
			return true
		}
		switch target.Name {
		case "replacementExecution":
			if replacementRead < 0 {
				replacementRead = int(call.Pos())
			}
		case "firstExecution":
			if predecessorRead < 0 {
				predecessorRead = int(call.Pos())
			}
		}
		return true
	})
	if replacementRead < 0 || predecessorRead < 0 {
		t.Fatalf("handoff reads not found: replacement=%d predecessor=%d", replacementRead, predecessorRead)
	}
	if replacementRead > predecessorRead {
		t.Fatal("the handoff probe reads the predecessor before the replacement")
	}
}

// TestHarnessResilienceProbePipelineTimeouts proves every generated probe
// pipeline carries an action timeout above the frozen credential minimum with
// margin. A 60s cleanup action failed live preparation because the credential
// lifetime was shorter than token.DefaultBounds.MinTTL.
func TestHarnessResilienceProbePipelineTimeouts(t *testing.T) {
	image := "rg.example.test/kodama@sha256:" + repeat("a", 64)
	documents := resilienceProbePipelineDocuments(image)
	for name, document := range documents {
		timeouts, err := resiliencePipelineTimeouts(document)
		if err != nil {
			t.Fatalf("%s pipeline: %v", name, err)
		}
		if len(timeouts) == 0 {
			t.Fatalf("%s pipeline has no action timeout", name)
		}
		for _, raw := range timeouts {
			timeout, err := time.ParseDuration(raw)
			if err != nil {
				t.Fatalf("%s pipeline timeout %q is not a duration: %v", name, raw, err)
			}
			if timeout < resilienceCredentialMinimumTTL+time.Minute {
				t.Fatalf("%s pipeline timeout %s leaves no margin above the %s credential minimum",
					name, timeout, resilienceCredentialMinimumTTL)
			}
		}
	}
	controller := string(documents["controller"])
	if !strings.Contains(controller, "q02: /workspace is not writable by the workload user") {
		t.Fatalf("controller pipeline lacks the fixed non-writable /workspace diagnostic:\n%s", controller)
	}
	if !strings.Contains(controller, "/workspace/resilience-workspace") {
		t.Fatalf("controller pipeline does not probe the workspace mount:\n%s", controller)
	}
}

// resilienceFakeEdge is a whole-probe fake public edge. It routes by the frozen
// method and path through api.Match, so a wrong method is a 404 rather than a
// silently accepted request.
type resilienceFakeEdge struct {
	t            *testing.T
	mu           sync.Mutex
	operations   []string
	asked        bool
	assignment   string
	roles        []string
	requestCount int
}

func (edge *resilienceFakeEdge) record(operation string) {
	edge.mu.Lock()
	defer edge.mu.Unlock()
	edge.operations = append(edge.operations, operation)
	edge.requestCount++
}

func (edge *resilienceFakeEdge) count() int {
	edge.mu.Lock()
	defer edge.mu.Unlock()
	return edge.requestCount
}

func (edge *resilienceFakeEdge) serve(writer http.ResponseWriter, request *http.Request) {
	route, parameters, ok := api.Match(api.Method(request.Method), request.URL.Path)
	if !ok {
		edge.t.Errorf("fake edge: no frozen route for %s %s", request.Method, request.URL.Path)
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	var envelope api.Request
	if err := json.NewDecoder(request.Body).Decode(&envelope); err != nil {
		edge.t.Errorf("fake edge: decode %s envelope: %v", route.Operation, err)
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	edge.record(route.Operation)
	switch route.Operation {
	case "pipeline.apply":
		if request.Method != string(api.MethodPut) {
			edge.t.Errorf("pipeline.apply method = %s, want PUT", request.Method)
		}
		if parameters["project"] != "project-1" || parameters["pipeline"] != "pipeline-1" {
			edge.t.Errorf("pipeline.apply path parameters = %v", parameters)
		}
		if envelope.ExpectedVersion != nil {
			edge.t.Errorf("pipeline.apply expected_version = %v, want absent", *envelope.ExpectedVersion)
		}
		resilienceWriteResult(edge.t, writer, "pipeline.apply", map[string]any{"pipeline_id": "pipe-1", "version": 1})
	case "task.create_and_start":
		if request.Method != string(api.MethodPost) {
			edge.t.Errorf("task.create_and_start method = %s, want POST", request.Method)
		}
		if envelope.ExpectedVersion != nil {
			edge.t.Errorf("task.create_and_start expected_version = %v, want absent", *envelope.ExpectedVersion)
		}
		resilienceWriteResult(edge.t, writer, "task.create_and_start", map[string]any{
			"task_id": "task-1", "execution_id": "exec-1", "lifecycle_version": 1,
		})
	case "task.read":
		if request.Method != string(api.MethodGet) {
			edge.t.Errorf("task.read method = %s, want GET", request.Method)
		}
		if envelope.ExpectedVersion != nil {
			edge.t.Errorf("task.read expected_version = %v, want absent", *envelope.ExpectedVersion)
		}
		edge.mu.Lock()
		waiting := edge.asked
		edge.mu.Unlock()
		body := map[string]any{
			"task_id": "task-1", "title": "handoff", "state": "open",
			"content_version": 1, "lifecycle_version": 1, "initiator": "admin-1",
			"selected_pipeline": "pipe-1", "current_execution": "exec-1",
		}
		if waiting {
			edge.mu.Lock()
			assignment := edge.assignment
			edge.mu.Unlock()
			body["state"] = "waiting"
			body["lifecycle_version"] = 2
			body["question"] = map[string]any{
				"question_id": "question-1", "body": "continue?", "assignment": assignment, "version": 1,
			}
		}
		resilienceWriteResult(edge.t, writer, "task.read", body)
	case "task.ask":
		if request.Method != string(api.MethodPost) {
			edge.t.Errorf("task.ask method = %s, want POST", request.Method)
		}
		if envelope.ExpectedVersion == nil || *envelope.ExpectedVersion != 1 {
			edge.t.Errorf("task.ask expected_version = %v, want the task lifecycle 1", envelope.ExpectedVersion)
		}
		var ask api.TaskAskRequest
		if err := json.Unmarshal(envelope.Input, &ask); err != nil {
			edge.t.Errorf("fake edge: decode task.ask input: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		edge.mu.Lock()
		edge.asked = true
		edge.assignment = ask.Assignment
		edge.mu.Unlock()
		resilienceWriteResult(edge.t, writer, "task.ask", map[string]any{
			"question_id": "question-1", "lifecycle_version": 2, "question_version": 1,
		})
	case "task.reply":
		if request.Method != string(api.MethodPost) {
			edge.t.Errorf("task.reply method = %s, want POST", request.Method)
		}
		if envelope.ExpectedVersion == nil || *envelope.ExpectedVersion != 2 {
			edge.t.Errorf("task.reply expected_version = %v, want the task lifecycle 2", envelope.ExpectedVersion)
		}
		edge.mu.Lock()
		assignment, roles := edge.assignment, append([]string(nil), edge.roles...)
		edge.mu.Unlock()
		if role, ok := strings.CutPrefix(assignment, "role/"); ok {
			allowed := slices.Contains(roles, role)
			if !allowed {
				resilienceWriteError(edge.t, writer, http.StatusForbidden, api.CodeForbidden)
				return
			}
		}
		resilienceWriteResult(edge.t, writer, "task.reply", map[string]any{
			"execution_id": "exec-2", "lifecycle_version": 3, "question_version": 2,
		})
	case "execution.inspect":
		if request.Method != string(api.MethodGet) {
			edge.t.Errorf("execution.inspect method = %s, want GET", request.Method)
		}
		if envelope.ExpectedVersion != nil {
			edge.t.Errorf("execution.inspect expected_version = %v, want absent", *envelope.ExpectedVersion)
		}
		resilienceWriteResult(edge.t, writer, "execution.inspect", map[string]any{
			"execution_id": "exec-2", "state": "succeeded", "version": 2, "current_action": -1,
			"actions": []any{}, "cleanup": map[string]any{"state": "complete", "workspace_id": "ws-1"},
		})
	case "event.read":
		if request.Method != string(api.MethodGet) {
			edge.t.Errorf("event.read method = %s, want GET", request.Method)
		}
		if request.URL.RawQuery != "" || request.URL.ForceQuery || request.URL.Fragment != "" {
			edge.t.Errorf("event.read carried query/fragment %q", request.URL.RequestURI())
		}
		next, err := api.EncodeCursor("0")
		if err != nil {
			edge.t.Errorf("encode event cursor: %v", err)
		}
		resilienceWriteResult(edge.t, writer, "event.read", map[string]any{
			"events": []any{}, "next": next, "has_more": false,
		})
	case "execution.logs.read":
		if request.Method != string(api.MethodGet) {
			edge.t.Errorf("execution.logs.read method = %s, want GET", request.Method)
		}
		if request.URL.RawQuery != "" || request.URL.ForceQuery || request.URL.Fragment != "" {
			edge.t.Errorf("execution.logs.read carried query/fragment %q", request.URL.RequestURI())
		}
		resilienceWriteResult(edge.t, writer, "execution.logs.read", map[string]any{
			"chunks": []any{}, "truncated": false,
		})
	default:
		edge.t.Errorf("fake edge: unexpected operation %q", route.Operation)
		writer.WriteHeader(http.StatusBadRequest)
	}
}

// TestHarnessResilienceHandoffFakeEdge drives the handoff probe's request
// builders against a whole fake edge that routes by frozen method and path.
func TestHarnessResilienceHandoffFakeEdge(t *testing.T) {
	edge := &resilienceFakeEdge{t: t, roles: []string{"engineer"}}
	server := httptest.NewServer(http.HandlerFunc(edge.serve))
	defer server.Close()
	client := resilienceTestAPIClient(t, server.URL)
	stage := &resilienceStage{
		Config: Config{ProjectID: "project-1", AdminToken: Secret("admin-token")},
		API:    client,
	}
	ctx := context.Background()

	applied, err := resilienceApplyPipeline(ctx, stage, "pipeline-1", []byte("actions: []\n"), "apply-key")
	if err != nil {
		t.Fatalf("fake-edge pipeline.apply: %v", err)
	}
	if applied.PipelineID != "pipe-1" {
		t.Fatalf("pipeline.apply result = %+v", applied)
	}
	created, err := resilienceMutateResult(ctx, stage, "task.create_and_start", api.TaskCreateAndStartRequest{
		ProjectID: "project-1", Title: "handoff", PipelineID: "pipe-1",
	}, "create-key")
	if err != nil {
		t.Fatalf("fake-edge task.create_and_start: %v", err)
	}
	if _, err := resilienceDecodeResult[api.TaskCreateAndStartResult](created, "task.create_and_start"); err != nil {
		t.Fatalf("decode created task: %v", err)
	}
	if _, err := resilienceReadTask(ctx, stage, "task-1"); err != nil {
		t.Fatalf("fake-edge task.read: %v", err)
	}
	if _, err := resilienceAskCurrent(ctx, stage, "task-1", "continue?", "role/engineer", "ask-key"); err != nil {
		t.Fatalf("fake-edge task.ask: %v", err)
	}
	edge.mu.Lock()
	askedAssignment := edge.assignment
	edge.mu.Unlock()
	if askedAssignment != "role/engineer" {
		t.Fatalf("fake-edge ask assignment = %q, want role/engineer", askedAssignment)
	}
	if _, err := resilienceReplyCurrent(ctx, stage, "task-1", "answer", "reply-key"); err != nil {
		t.Fatalf("fake-edge task.reply: %v", err)
	}
	if _, err := resilienceInspect(ctx, stage, "exec-2"); err != nil {
		t.Fatalf("fake-edge execution.inspect: %v", err)
	}
	if events, err := resilienceReadAllEvents(ctx, stage); err != nil || len(events) != 0 {
		t.Fatalf("fake-edge event.read: events=%v err=%v", events, err)
	}
	if _, err := resilienceReadAllLogs(ctx, stage, "exec-2", "action-1"); err != nil {
		t.Fatalf("fake-edge execution.logs.read: %v", err)
	}
	want := []string{"pipeline.apply", "task.create_and_start", "task.read", "task.read", "task.ask", "task.read", "task.reply", "execution.inspect", "event.read", "execution.logs.read"}
	edge.mu.Lock()
	got := append([]string(nil), edge.operations...)
	edge.mu.Unlock()
	if !resilienceSameStrings(got, want) {
		t.Fatalf("fake-edge operations = %v, want %v", got, want)
	}
}

// TestHarnessResilienceProbeRequestRoutes pins every operation a live probe
// uses to its frozen method and path, including pipeline.apply's PUT.
func TestHarnessResilienceProbeRequestRoutes(t *testing.T) {
	cases := []struct {
		operation       string
		method          api.Method
		path            string
		alternate       bool
		expectedVersion bool
	}{
		{operation: "pipeline.apply", method: api.MethodPut, path: "/api/v1/projects/{project}/pipelines/{pipeline}"},
		{operation: "pipeline.start", method: api.MethodPost, path: "/api/v1/projects/{project}/executions", alternate: true},
		{operation: "task.create_and_start", method: api.MethodPost, path: "/api/v1/projects/{project}/tasks/create-and-start"},
		{operation: "task.read", method: api.MethodGet, path: "/api/v1/tasks/{task}"},
		{operation: "task.ask", method: api.MethodPost, path: "/api/v1/tasks/{task}/ask"},
		{operation: "task.reply", method: api.MethodPost, path: "/api/v1/tasks/{task}/reply"},
		{operation: "execution.inspect", method: api.MethodGet, path: "/api/v1/executions/{execution}"},
		{operation: "execution.logs.read", method: api.MethodGet, path: "/api/v1/executions/{execution}/logs"},
		{operation: "event.read", method: api.MethodGet, path: "/api/v1/projects/{project}/events"},
		{operation: "execution.list", method: api.MethodGet, path: "/api/v1/projects/{project}/executions"},
	}
	for _, testCase := range cases {
		t.Run(testCase.operation, func(t *testing.T) {
			chosen, ok := resilienceOperationMethod(testCase.operation)
			if !ok {
				t.Fatalf("operation %q is not in the frozen route table", testCase.operation)
			}
			if chosen != testCase.method {
				t.Fatalf("%s method = %s, want %s", testCase.operation, chosen, testCase.method)
			}
			route, ok := api.LookupRoute(testCase.operation)
			if !ok {
				t.Fatalf("operation %q has no canonical route", testCase.operation)
			}
			if testCase.alternate {
				found := false
				for _, alternate := range api.AlternateRoutes() {
					if alternate.Operation == testCase.operation {
						route = alternate
						found = true
					}
				}
				if !found {
					t.Fatalf("%s has no alternate route", testCase.operation)
				}
			}
			if route.Method != testCase.method || route.Path != testCase.path {
				t.Fatalf("%s route = %s %s, want %s %s", testCase.operation, route.Method, route.Path, testCase.method, testCase.path)
			}
			switch testCase.method {
			case api.MethodGet:
				if operationRequiresIdempotencyKey(testCase.operation) {
					t.Fatalf("%s is a read with a required idempotency key", testCase.operation)
				}
			default:
				if !operationRequiresIdempotencyKey(testCase.operation) {
					t.Fatalf("%s is a mutation without a required idempotency key", testCase.operation)
				}
			}
		})
	}

	// A deliberately wrong method is refused client-side with zero requests.
	edge := &resilienceFakeEdge{t: t}
	server := httptest.NewServer(http.HandlerFunc(edge.serve))
	defer server.Close()
	client := resilienceTestAPIClient(t, server.URL)
	if _, err := resilienceInvoke(context.Background(), client, api.MethodPost, Secret("admin-token"), "pipeline.apply",
		api.PipelineApplyRequest{ProjectID: "project-1", Name: "pipeline-1", YAML: []byte("actions: []\n")}, nil, "key", false); err == nil {
		t.Fatal("pipeline.apply accepted POST")
	}
	if edge.count() != 0 {
		t.Fatalf("a wrong-method request reached the fake edge %d time(s)", edge.count())
	}
}

// TestHarnessResilienceAPIEndpointPort proves the API-recovery helper only
// re-opens a guarded port-forward for a bounded loopback endpoint.
func TestHarnessResilienceAPIEndpointPort(t *testing.T) {
	for _, testCase := range []struct {
		name string
		url  string
		port int
	}{
		{name: "run-stage loopback", url: "https://127.0.0.1:18443", port: 18443},
		{name: "localhost", url: "https://localhost:18443", port: 18443},
		{name: "ipv6 loopback", url: "https://[::1]:18443", port: 18443},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			port, err := resilienceAPIPort(testCase.url)
			if err != nil || port != testCase.port {
				t.Fatalf("port = %d, err = %v; want %d", port, err, testCase.port)
			}
		})
	}
	for _, raw := range []string{
		"https://kodama.example.test:18443",
		"https://127.0.0.1",
		"http://127.0.0.1:18443",
		"https://127.0.0.1:0",
		"not-a-url",
	} {
		if _, err := resilienceAPIPort(raw); err == nil {
			t.Fatalf("API URL %q was accepted for port-forward recovery", raw)
		}
	}
}

// TestHarnessResilienceProbesEnsureAPIAccess proves every live probe verifies
// or restores the public API before its first request, and that the controller
// probe re-establishes it after the endpoint pod deletion. Without this the
// run-stage port-forward dies with the deleted pod and the next probe cannot
// start an execution.
func TestHarnessResilienceProbesEnsureAPIAccess(t *testing.T) {
	files := map[string][]string{
		"resilience_controller_test.go": {"resilienceStartInline"},
		"resilience_nats_test.go":       {"resilienceStartInline"},
		"resilience_broker_test.go":     {"resilienceRunBrokerOutage"},
		"resilience_handoff_test.go":    {"resilienceApplyPipeline"},
		"resilience_cleanup_test.go":    {"resilienceStartInline"},
	}
	for file, firstOperations := range files {
		t.Run(file, func(t *testing.T) {
			parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", file, err)
			}
			ensureCalls := 0
			firstEnsure := -1
			firstOperation := -1
			ast.Inspect(parsed, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				function, ok := call.Fun.(*ast.Ident)
				if !ok {
					return true
				}
				if function.Name == "resilienceEnsureAPIAccess" {
					ensureCalls++
					if firstEnsure < 0 {
						firstEnsure = int(call.Pos())
					}
					return true
				}
				for _, name := range firstOperations {
					if function.Name == name && firstOperation < 0 {
						firstOperation = int(call.Pos())
					}
				}
				return true
			})
			if ensureCalls == 0 {
				t.Fatalf("%s never calls resilienceEnsureAPIAccess", file)
			}
			if firstOperation < 0 {
				t.Fatalf("%s has no known first operation", file)
			}
			if firstEnsure > firstOperation {
				t.Fatalf("%s ensures API access only after its first operation", file)
			}
			if file == "resilience_controller_test.go" && ensureCalls < 2 {
				t.Fatalf("controller probe ensures API access %d times, want start plus post-deletion", ensureCalls)
			}
		})
	}
}

// resilienceFakeEdgeHandoffToAsk drives the handoff builders through one fake
// edge up to the outstanding question, assigned to the supplied role.
func resilienceFakeEdgeHandoffToAsk(t *testing.T, stage *resilienceStage, assignment string) {
	t.Helper()
	ctx := context.Background()
	if _, err := resilienceApplyPipeline(ctx, stage, "pipeline-1", []byte("actions: []\n"), "apply-key"); err != nil {
		t.Fatalf("fake-edge pipeline.apply: %v", err)
	}
	if _, err := resilienceMutateResult(ctx, stage, "task.create_and_start", api.TaskCreateAndStartRequest{
		ProjectID: "project-1", Title: "handoff", PipelineID: "pipe-1",
	}, "create-key"); err != nil {
		t.Fatalf("fake-edge task.create_and_start: %v", err)
	}
	if _, err := resilienceReadTask(ctx, stage, "task-1"); err != nil {
		t.Fatalf("fake-edge task.read: %v", err)
	}
	if _, err := resilienceAskCurrent(ctx, stage, "task-1", "continue?", assignment, "ask-key"); err != nil {
		t.Fatalf("fake-edge task.ask: %v", err)
	}
}

// TestHarnessResilienceReplyRoleEnforcement models task.reply's in-commit
// assignment check: the persona holds engineer, so a reply to role/admin is
// forbidden while role/engineer succeeds even though the asker is the same
// persona.
func TestHarnessResilienceReplyRoleEnforcement(t *testing.T) {
	ctx := context.Background()
	forbiddenEdge := &resilienceFakeEdge{t: t, roles: []string{"engineer"}}
	forbiddenServer := httptest.NewServer(http.HandlerFunc(forbiddenEdge.serve))
	defer forbiddenServer.Close()
	forbiddenStage := &resilienceStage{
		Config: Config{ProjectID: "project-1", AdminToken: Secret("admin-token")},
		API:    resilienceTestAPIClient(t, forbiddenServer.URL),
	}
	resilienceFakeEdgeHandoffToAsk(t, forbiddenStage, "role/admin")
	_, err := resilienceReplyCurrent(ctx, forbiddenStage, "task-1", "answer", "reply-key")
	var public *APIError
	if !errors.As(err, &public) {
		t.Fatalf("reply to role/admin by an engineer returned %v, want a public refusal", err)
	}
	if public.Status != http.StatusForbidden || public.Code != api.CodeForbidden {
		t.Fatalf("reply to role/admin = status %d code %q, want 403 forbidden", public.Status, public.Code)
	}

	allowedEdge := &resilienceFakeEdge{t: t, roles: []string{"engineer"}}
	allowedServer := httptest.NewServer(http.HandlerFunc(allowedEdge.serve))
	defer allowedServer.Close()
	allowedStage := &resilienceStage{
		Config: Config{ProjectID: "project-1", AdminToken: Secret("admin-token")},
		API:    resilienceTestAPIClient(t, allowedServer.URL),
	}
	resilienceFakeEdgeHandoffToAsk(t, allowedStage, "role/engineer")
	replied, err := resilienceReplyCurrent(ctx, allowedStage, "task-1", "answer", "reply-key")
	if err != nil {
		t.Fatalf("reply to role/engineer by an engineer was refused: %v", err)
	}
	if replied.ExecutionID != "exec-2" {
		t.Fatalf("reply execution = %q, want exec-2", replied.ExecutionID)
	}
}

// TestHarnessResiliencePodFacts proves the controller probe records fixed pod
// facts that distinguish a pod loss (gone or new UID) from a workload kill
// (terminated exit 137 in the same pod), without reading logs.
func TestHarnessResiliencePodFacts(t *testing.T) {
	document := []byte(`{"items":[{"metadata":{"name":"kodama-job-abc-1","uid":"2f1c7b9e-0f5a-4a5c-9a54-0f6f4a1c8d21","deletionTimestamp":"2026-11-08T01:02:03Z"},"status":{"phase":"Failed","containerStatuses":[{"name":"workload","state":{"terminated":{"reason":"Error","exitCode":137}},"lastState":{"running":{}}}]}}]}`)
	facts, err := resilienceParsePodFacts(document)
	if err != nil {
		t.Fatalf("parse pod facts: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("pod facts = %+v, want one pod", facts)
	}
	pod := facts[0]
	if pod.Name != "kodama-job-abc-1" || pod.UID != "2f1c7b9e-0f5a-4a5c-9a54-0f6f4a1c8d21" ||
		pod.Phase != "Failed" || pod.DeletionTimestamp != "2026-11-08T01:02:03Z" {
		t.Fatalf("pod identity facts = %+v", pod)
	}
	if len(pod.Containers) != 1 || pod.Containers[0].State != "terminated" ||
		pod.Containers[0].Reason != "Error" || pod.Containers[0].ExitCode != 137 ||
		pod.Containers[0].LastState != "running" {
		t.Fatalf("container facts = %+v", pod.Containers)
	}
	formatted := resilienceFormatPodFacts("after", facts)
	for _, want := range []string{"after=name=kodama-job-abc-1", "uid=2f1c7b9e", "phase=Failed", "deletion=2026-11-08T01:02:03Z", "state=terminated", "reason=Error", "exit=137"} {
		if !strings.Contains(formatted, want) {
			t.Fatalf("formatted facts %q do not contain %q", formatted, want)
		}
	}
	if empty := resilienceFormatPodFacts("after", nil); empty != "after=no-pods" {
		t.Fatalf("empty pod facts = %q", empty)
	}
	if _, err := resilienceParsePodFacts([]byte("not json")); err == nil {
		t.Fatal("an unreadable pod list was accepted")
	}
	if _, err := resilienceParsePodFacts([]byte(`{"items":[{"metadata":{"name":"Bad_Name","uid":"uid-1"},"status":{}}]}`)); err == nil {
		t.Fatal("an unsafe pod identity was accepted")
	}
}

// TestHarnessResilienceEvidenceNamesBounded proves state-encoding evidence
// names stay lower-case and bounded for the barrier and workload fact records.
func TestHarnessResilienceEvidenceNamesBounded(t *testing.T) {
	for _, name := range []string{
		resilienceEvidenceName("resilience-handoff-barrier-action", "0", "Running"),
		resilienceEvidenceName("resilience-controller-workload", "after", "container", "0", "terminated", "Error", "137"),
		resilienceEvidenceName("resilience-handoff-barrier-predecessor-stop", "unconfirmed"),
	} {
		if !evidenceNamePattern.MatchString(name) {
			t.Fatalf("evidence name %q is not a bounded lower-case name", name)
		}
	}
}

// TestHarnessResilienceLiveEvidencePins pins the facts the extension round
// requires: engineer-assigned handoff, new-work barrier on actions, bounded task
// settlement polling, and before/after workload pod facts in the controller
// failure path.
func TestHarnessResilienceLiveEvidencePins(t *testing.T) {
	handoff, err := os.ReadFile("resilience_handoff_test.go")
	if err != nil {
		t.Fatalf("read handoff probe: %v", err)
	}
	handoffText := string(handoff)
	for _, want := range []string{
		`"role/engineer"`,
		"resilienceWaitTaskSettled(ctx, stage, created.TaskID",
		"resilienceCheckStopBarrier(stopState, replacement.Actions)",
		"resilienceRecordBarrierObservation(probe, replacement, predecessor, stopState",
	} {
		if !strings.Contains(handoffText, want) {
			t.Fatalf("handoff probe does not contain %q", want)
		}
	}
	if strings.Contains(handoffText, `"role/admin"`) {
		t.Fatal("handoff probe still assigns role/admin")
	}

	controller, err := os.ReadFile("resilience_controller_test.go")
	if err != nil {
		t.Fatalf("read controller probe: %v", err)
	}
	controllerText := string(controller)
	for _, want := range []string{
		`resilienceRecordPodFacts(probe, executionID, "before"`,
		`resilienceRecordPodFacts(probe, executionID, "after"`,
		"workload facts before:",
		"workload facts after:",
		"kodama.dev/execution=",
	} {
		if !strings.Contains(controllerText, want) {
			t.Fatalf("controller probe does not contain %q", want)
		}
	}
}

// TestHarnessResilienceCleanupPrecedesRetainedLogReads pins the retained-log
// ordering invariant across the live probes: retained chunks are appended
// during terminal cleanup (collect -> append -> remove -> release), so every
// probe that waits for cleanup completion and reads retained logs must call
// resilienceWaitCleanupComplete before resilienceReadAllLogs.
func TestHarnessResilienceCleanupPrecedesRetainedLogReads(t *testing.T) {
	targets := []struct {
		file string
		fn   string
	}{
		{file: "resilience_cleanup_test.go", fn: "TestStageResilienceCleanupAttribution"},
		{file: "resilience_controller_test.go", fn: "TestStageResilienceControllerPodDeletion"},
		{file: "resilience_failure_test.go", fn: "TestStageResilienceWorkspaceLoss"},
		{file: "resilience_failure_test.go", fn: "TestStageResilienceWorkloadPodLoss"},
		{file: "resilience_failure_test.go", fn: "TestStageResilienceCleanupCrashAttribution"},
		{file: "resilience_failure_test.go", fn: "TestStageResilienceGitDependencyOutage"},
		{file: "resilience_handoff_test.go", fn: "TestStageResilienceUncertainStopBlocksHandoff"},
		{file: "isolation_cross_tenant_test.go", fn: "TestStageCrossTenantIsolation"},
		{file: "isolation_runtime_test.go", fn: "TestStageRuntimeIsolation"},
	}
	for _, target := range targets {
		target := target
		t.Run(target.fn, func(t *testing.T) {
			parsed, err := parser.ParseFile(token.NewFileSet(), target.file, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", target.file, err)
			}
			var body *ast.BlockStmt
			for _, decl := range parsed.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if ok && fn.Name.Name == target.fn {
					body = fn.Body
					break
				}
			}
			if body == nil {
				t.Fatalf("%s has no function %s", target.file, target.fn)
			}
			lastCleanupPos := -1
			firstLogsPos := -1
			ast.Inspect(body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				ident, ok := call.Fun.(*ast.Ident)
				if !ok {
					return true
				}
				switch ident.Name {
				case "resilienceWaitCleanupComplete":
					lastCleanupPos = int(call.Pos())
				case "resilienceReadAllLogs":
					if firstLogsPos < 0 {
						firstLogsPos = int(call.Pos())
					}
				}
				return true
			})
			if lastCleanupPos < 0 || firstLogsPos < 0 {
				t.Fatalf("%s calls cleanup=%d logs=%d; want both present", target.fn, lastCleanupPos, firstLogsPos)
			}
			if lastCleanupPos > firstLogsPos {
				t.Fatalf("%s reads retained logs before waiting for terminal cleanup completion", target.fn)
			}
		})
	}
}
