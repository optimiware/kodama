// Log behavior: ordered identified chunks, bounded reads, configured redaction
// and no lifecycle authority.
package kube

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/kernel/engine"
)

// TestLogsAreOrderedBoundedAndRedacted proves a read returns ordered identified
// chunks of the combined log and removes configured credential/secret values.
func TestLogsAreOrderedBoundedAndRedacted(t *testing.T) {
	fake, _, adapter, request := setupStarted(t)
	pod := testRunningPod(request.Identity)
	fake.addPod(pod)
	fake.setPodLogs(pod.Name, LogStreamCombined, []byte("hello\nsecret s3cr3t\ncredential tok-abc\n"))

	batch, err := adapter.Logs(context.Background(), engine.LogsRequest{
		Identity:    request.Identity,
		AfterOffset: 0,
		Limit:       4096,
	})
	if err != nil {
		t.Fatalf("Logs returned error: %v", err)
	}
	if len(batch.Chunks) == 0 {
		t.Fatal("Logs returned no chunks")
	}

	streams := map[engine.LogStream]bool{}
	var sequence int64
	for index, chunk := range batch.Chunks {
		if chunk.Sequence <= sequence && index > 0 {
			t.Fatalf("chunk %d sequence = %d after %d; chunks are not ordered", index, chunk.Sequence, sequence)
		}
		sequence = chunk.Sequence
		streams[chunk.Stream] = true
		if chunk.Offset < 0 {
			t.Errorf("chunk %d has negative offset", index)
		}
		if len(chunk.Data) > 4096 {
			t.Errorf("chunk %d has %d bytes; over the requested bound", index, len(chunk.Data))
		}
		text := string(chunk.Data)
		for _, secret := range []string{"s3cr3t", "tok-abc"} {
			if strings.Contains(text, secret) {
				t.Errorf("chunk %d leaked configured value %q: %q", index, secret, text)
			}
		}
	}
	if !streams[engine.LogStdout] || streams[engine.LogStderr] {
		t.Fatalf("streams = %v; the combined log is attributed to stdout only", streams)
	}

	combined := ""
	for _, chunk := range batch.Chunks {
		combined += string(chunk.Data)
	}
	if !strings.Contains(combined, Redacted) {
		t.Fatalf("redacted output %q has no %q marker", combined, Redacted)
	}
	if !strings.Contains(combined, "hello") {
		t.Fatalf("redacted output %q lost ordinary content", combined)
	}
}

// TestLogsPaginationUsesSharedCursor proves bounded pagination advances the one
// continuation token per stream and never duplicates bytes.
func TestLogsPaginationUsesSharedCursor(t *testing.T) {
	fake, _, adapter, request := setupStarted(t)
	pod := testRunningPod(request.Identity)
	fake.addPod(pod)
	stdout := []byte(strings.Repeat("a", 100))
	fake.setPodLogs(pod.Name, LogStreamCombined, stdout)

	var collected []byte
	offset := int64(0)
	wantStdout := []int64{32, 64, 96, 100}
	for step, wantOffset := range wantStdout {
		batch, err := adapter.Logs(context.Background(), engine.LogsRequest{
			Identity:    request.Identity,
			AfterOffset: offset,
			Limit:       32,
		})
		if err != nil {
			t.Fatalf("Logs step %d returned error: %v", step, err)
		}
		for _, chunk := range batch.Chunks {
			if chunk.Sequence != 1 {
				t.Fatalf("step %d chunk sequence = %d; want 1", step, chunk.Sequence)
			}
			if wantTruncated := wantOffset < int64(len(stdout)); chunk.Truncated != wantTruncated {
				t.Fatalf("step %d truncated = %v; want %v", step, chunk.Truncated, wantTruncated)
			}
			if chunk.Stream == engine.LogStdout {
				collected = append(collected, chunk.Data...)
			}
		}
		wantToken := wantOffset << 32
		if batch.NextOffset != wantToken {
			t.Fatalf("step %d NextOffset = %d; want position %d token %d",
				step, batch.NextOffset, wantOffset, wantToken)
		}
		offset = batch.NextOffset
	}
	if !bytes.Equal(collected, stdout) {
		t.Fatalf("paged stdout = %q; want the original 100 bytes exactly once", collected)
	}
}

// TestLogsCursorDeliversAppendedBytesExactlyOnce proves the continuation token
// keeps the read position as the log grows: bytes appended after a read are
// delivered by the next one, and every byte arrives exactly once.
func TestLogsCursorDeliversAppendedBytesExactlyOnce(t *testing.T) {
	fake, _, adapter, request := setupStarted(t)
	pod := testRunningPod(request.Identity)
	fake.addPod(pod)
	head := []byte(strings.Repeat("o", 40))
	fake.setPodLogs(pod.Name, LogStreamCombined, head)

	first, err := adapter.Logs(context.Background(), engine.LogsRequest{Identity: request.Identity, Limit: 32})
	if err != nil {
		t.Fatalf("first Logs returned error: %v", err)
	}
	chunk, ok := testChunkForStream(first, engine.LogStdout)
	if !ok || chunk.Offset != 0 || len(chunk.Data) != 32 {
		t.Fatalf("first chunk = %+v; want offset 0 length 32", chunk)
	}

	fake.setPodLogs(pod.Name, LogStreamCombined, append(append([]byte(nil), head...), []byte("BBBBB")...))
	second, err := adapter.Logs(context.Background(), engine.LogsRequest{Identity: request.Identity, AfterOffset: first.NextOffset, Limit: 32})
	if err != nil {
		t.Fatalf("follow-up Logs returned error: %v", err)
	}
	next, ok := testChunkForStream(second, engine.LogStdout)
	if !ok || next.Offset != 32 || string(next.Data) != strings.Repeat("o", 8)+"BBBBB" {
		t.Fatalf("follow-up chunk = %+v; want offset 32 and the remaining 13 bytes", next)
	}
	if again, _ := adapter.Logs(context.Background(), engine.LogsRequest{Identity: request.Identity, AfterOffset: second.NextOffset, Limit: 32}); len(again.Chunks) != 0 {
		t.Fatalf("a caught-up read returned %d chunks", len(again.Chunks))
	}
}

// TestLogsReadsOnlyTheCombinedStream proves the adapter never asks the cluster
// for one split stream: the pod log API separates stdout from stderr only
// behind an alpha gate that stock clusters leave off and reject the query
// parameter for. The one combined log is the portable read.
func TestLogsReadsOnlyTheCombinedStream(t *testing.T) {
	fake, _, adapter, request := setupStarted(t)
	pod := testRunningPod(request.Identity)
	fake.addPod(pod)
	fake.setPodLogs(pod.Name, LogStreamCombined, []byte("combined stdout and stderr\n"))

	batch, err := adapter.Logs(context.Background(), engine.LogsRequest{
		Identity: request.Identity,
		Limit:    1024,
	})
	if err != nil {
		t.Fatalf("Logs returned error: %v", err)
	}
	streams := fake.logStreamsSnapshot()
	if len(streams) != 1 || streams[0] != LogStreamCombined {
		t.Fatalf("requested log streams = %v; want only %q", streams, LogStreamCombined)
	}
	if len(batch.Chunks) != 1 || string(batch.Chunks[0].Data) != "combined stdout and stderr\n" {
		t.Fatalf("chunks = %+v; want the combined log in one chunk", batch.Chunks)
	}
}

// TestLogsCursorHighHalfAdvancesAndStderrHalfStaysZero proves the combined log
// keeps one byte position in the high half of the continuation token and never
// advances the retired stderr half.
func TestLogsCursorHighHalfAdvancesAndStderrHalfStaysZero(t *testing.T) {
	fake, _, adapter, request := setupStarted(t)
	pod := testRunningPod(request.Identity)
	fake.addPod(pod)
	fake.setPodLogs(pod.Name, LogStreamCombined, []byte(strings.Repeat("z", 40)))

	cursor := int64(0)
	for step, want := range []int64{16, 32, 40} {
		batch, err := adapter.Logs(context.Background(), engine.LogsRequest{
			Identity:    request.Identity,
			AfterOffset: cursor,
			Limit:       16,
		})
		if err != nil {
			t.Fatalf("step %d Logs returned error: %v", step, err)
		}
		if batch.NextOffset>>logCursorStreamBits != want {
			t.Fatalf("step %d high half = %d; want the stdout position %d",
				step, batch.NextOffset>>logCursorStreamBits, want)
		}
		if batch.NextOffset&logCursorStreamMask != 0 {
			t.Fatalf("step %d stderr half = %d; want 0",
				step, batch.NextOffset&logCursorStreamMask)
		}
		cursor = batch.NextOffset
	}
}

// TestLogsPaginationAcrossRestartIsExact proves the continuation token is the
// whole durable read state: a fresh adapter (a restarted process) resuming at
// the saved cursor delivers every remaining byte exactly once.
func TestLogsPaginationAcrossRestartIsExact(t *testing.T) {
	fake, clock, adapter, request := setupStarted(t)
	pod := testRunningPod(request.Identity)
	fake.addPod(pod)
	stream := []byte(strings.Repeat("0123456789", 10))
	fake.setPodLogs(pod.Name, LogStreamCombined, stream)

	var collected []byte
	cursor := int64(0)

	first, err := adapter.Logs(context.Background(), engine.LogsRequest{
		Identity:    request.Identity,
		AfterOffset: cursor,
		Limit:       32,
	})
	if err != nil {
		t.Fatalf("first Logs returned error: %v", err)
	}
	for _, chunk := range first.Chunks {
		if chunk.Stream != engine.LogStdout {
			t.Fatalf("restart fixture returned a %q chunk; the combined log is stdout", chunk.Stream)
		}
		if chunk.Sequence != 1 {
			t.Fatalf("restart fixture sequence = %d; want 1", chunk.Sequence)
		}
		if !chunk.Truncated {
			t.Fatal("restart fixture's first bounded page did not report truncation")
		}
		collected = append(collected, chunk.Data...)
	}
	cursor = first.NextOffset
	if cursor == 0 {
		t.Fatal("first read did not advance the cursor")
	}

	// A new adapter over the same cluster state models a restarted process: the
	// cursor is the only state it may rely on.
	restarted := newTestAdapter(t, fake, clock)
	for range 100 {
		batch, err := restarted.Logs(context.Background(), engine.LogsRequest{
			Identity:    request.Identity,
			AfterOffset: cursor,
			Limit:       32,
		})
		if err != nil {
			t.Fatalf("resumed Logs at %d returned error: %v", cursor, err)
		}
		for _, chunk := range batch.Chunks {
			collected = append(collected, chunk.Data...)
		}
		if batch.NextOffset == cursor {
			break
		}
		cursor = batch.NextOffset
	}
	if !bytes.Equal(collected, stream) {
		t.Fatalf("resumed log = %q; want the original %d bytes exactly once", collected, len(stream))
	}
}

// TestLogsRedactsValueSplitAcrossPages proves the adapter itself never emits a
// configured value that a page boundary splits: the read stops before the
// partial value, the next page re-reads it whole and stores only the marker.
func TestLogsRedactsValueSplitAcrossPages(t *testing.T) {
	fake, _, adapter, request := setupStarted(t)
	pod := testRunningPod(request.Identity)
	fake.addPod(pod)
	prefix := strings.Repeat("x", 10)
	// The page bound cuts the credential after its first three bytes.
	limit := len(prefix) + 3
	stream := []byte(prefix + request.Credential + "-tail")
	fake.setPodLogs(pod.Name, LogStreamCombined, stream)

	var collected []byte
	cursor := int64(0)
	pages := 0
	for ; pages < 100; pages++ {
		batch, err := adapter.Logs(context.Background(), engine.LogsRequest{
			Identity:    request.Identity,
			AfterOffset: cursor,
			Limit:       limit,
		})
		if err != nil {
			t.Fatalf("page %d Logs returned error: %v", pages, err)
		}
		for _, chunk := range batch.Chunks {
			if strings.Contains(string(chunk.Data), request.Credential) {
				t.Fatalf("page %d leaked the split credential: %q", pages, chunk.Data)
			}
			collected = append(collected, chunk.Data...)
		}
		if batch.NextOffset == cursor {
			break
		}
		cursor = batch.NextOffset
	}
	if pages == 100 {
		t.Fatal("split-page log read did not terminate")
	}
	if strings.Contains(string(collected), request.Credential) {
		t.Fatalf("adapter output leaked the value split across pages: %q", collected)
	}
	if want := prefix + Redacted + "-tail"; string(collected) != want {
		t.Fatalf("adapter output = %q; want %q", collected, want)
	}
}

// TestLogsRedactsValueEndingAtThePageBoundary proves a page that ends with a
// complete value is redacted whole, not held back from a repeated-character
// suffix. Holding it would emit the value's first bytes raw and re-read the
// rest, reconstructing the value across two chunks.
func TestLogsRedactsValueEndingAtThePageBoundary(t *testing.T) {
	fake, _, adapter := testSetup(t)
	request := testPrepareRequest()
	request.Secrets = []engine.SecretValue{{Name: "API_KEY", Value: "aaa"}}
	prepareTestWorkload(t, adapter, request)
	startTestWorkload(t, adapter, request.Identity, request.WorkspaceID)
	pod := testRunningPod(request.Identity)
	fake.addPod(pod)
	// The page bound ends exactly on the complete value after a prefix long
	// enough for the redaction marker to fit.
	content := "1234567aaa-tail"
	fake.setPodLogs(pod.Name, LogStreamCombined, []byte(content))
	limit := len("1234567aaa")

	var collected []byte
	cursor := int64(0)
	for page := range 100 {
		batch, err := adapter.Logs(context.Background(), engine.LogsRequest{
			Identity:    request.Identity,
			AfterOffset: cursor,
			Limit:       limit,
		})
		if err != nil {
			t.Fatalf("page %d Logs returned error: %v", page, err)
		}
		for _, chunk := range batch.Chunks {
			collected = append(collected, chunk.Data...)
		}
		if batch.NextOffset == cursor {
			break
		}
		cursor = batch.NextOffset
	}
	if strings.Contains(string(collected), "aaa") {
		t.Fatalf("adapter output leaked the value ending at a page boundary: %q", collected)
	}
	if want := "1234567" + Redacted + "-tail"; string(collected) != want {
		t.Fatalf("adapter output = %q; want %q", collected, want)
	}
}

// TestLogsRefusesLegacySplitStreamCursor proves an old two-stream cursor is not
// silently reinterpreted as a combined-log position: its non-zero stderr half
// names a position the combined log does not have, so the read is refused
// before any cluster call with the existing invalid-input cursor class.
func TestLogsRefusesLegacySplitStreamCursor(t *testing.T) {
	fake, _, adapter := testSetup(t)
	request := testPrepareRequest()
	legacy := int64(7)<<logCursorStreamBits | 5

	batch, err := adapter.Logs(context.Background(), engine.LogsRequest{
		Identity:    request.Identity,
		AfterOffset: legacy,
		Limit:       1024,
	})
	if err == nil {
		t.Fatal("Logs accepted a split-stream cursor as a combined position")
	}
	class, ok := fault.ClassOf(err)
	if !ok || class != fault.InvalidInput {
		t.Fatalf("refusal class = %v (present=%v); want %q", class, ok, fault.InvalidInput)
	}
	if len(batch.Chunks) != 0 {
		t.Fatalf("refused cursor returned %d chunks", len(batch.Chunks))
	}
	for _, operation := range []string{"ListPods", "GetSecret", "ReadPodLogs"} {
		if calls := fake.callCount(operation); calls != 0 {
			t.Fatalf("refused cursor made %d %s calls; want 0", calls, operation)
		}
	}
}

// TestLogsAcceptsCombinedCursor proves a cursor produced by the combined reader
// (zero stderr half) still resumes the exact byte position.
func TestLogsAcceptsCombinedCursor(t *testing.T) {
	fake, _, adapter, request := setupStarted(t)
	pod := testRunningPod(request.Identity)
	fake.addPod(pod)
	fake.setPodLogs(pod.Name, LogStreamCombined, []byte("0123456789"))

	batch, err := adapter.Logs(context.Background(), engine.LogsRequest{
		Identity:    request.Identity,
		AfterOffset: encodeLogCursor(3, 0),
		Limit:       4,
	})
	if err != nil {
		t.Fatalf("Logs returned error: %v", err)
	}
	if len(batch.Chunks) != 1 || batch.Chunks[0].Offset != 3 || string(batch.Chunks[0].Data) != "3456" {
		t.Fatalf("resumed chunks = %+v; want offset 3 data 3456", batch.Chunks)
	}
	if want := encodeLogCursor(7, 0); batch.NextOffset != want {
		t.Fatalf("NextOffset = %d; want %d", batch.NextOffset, want)
	}
}

// testChunkForStream returns the single chunk for one stream.
func testChunkForStream(batch engine.LogBatch, stream engine.LogStream) (engine.LogChunk, bool) {
	for _, chunk := range batch.Chunks {
		if chunk.Stream == stream {
			return chunk, true
		}
	}
	return engine.LogChunk{}, false
}

// TestLogsRefusesUnboundedReads proves a zero, negative or oversized read is
// refused before any external call.
func TestLogsRefusesUnboundedReads(t *testing.T) {
	fake, _, adapter, request := setupStarted(t)
	cases := []struct {
		name    string
		request engine.LogsRequest
	}{
		{name: "zero limit", request: engine.LogsRequest{Identity: request.Identity, Limit: 0}},
		{name: "negative offset", request: engine.LogsRequest{Identity: request.Identity, AfterOffset: -1, Limit: 1024}},
		{name: "oversized limit", request: engine.LogsRequest{Identity: request.Identity, Limit: MaxLogBytes + 1}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			readsBefore := fake.callCount("ReadPodLogs")
			_, err := adapter.Logs(context.Background(), testCase.request)
			if err == nil {
				t.Fatalf("Logs accepted %s", testCase.name)
			}
			class, ok := fault.ClassOf(err)
			if !ok || class != fault.InvalidInput {
				t.Fatalf("refusal class = %v (present=%v); want %q", class, ok, fault.InvalidInput)
			}
			if fake.callCount("ReadPodLogs") != readsBefore {
				t.Fatal("an invalid log request reached the API")
			}
		})
	}
}

// TestLogsWithoutWorkloadHasNoLifecycleAuthority proves a log read never
// changes lifecycle state and cannot invent a workload.
func TestLogsWithoutWorkloadHasNoLifecycleAuthority(t *testing.T) {
	fake, _, adapter, request := setupStarted(t)
	createsBefore := fake.callCount("CreateJob")
	deletesBefore := fake.callCount("DeleteJob")

	batch, err := adapter.Logs(context.Background(), engine.LogsRequest{
		Identity:    request.Identity,
		AfterOffset: 0,
		Limit:       1024,
	})
	if err == nil {
		t.Fatal("Logs succeeded without a pod")
	}
	if len(batch.Chunks) != 0 {
		t.Fatal("Logs returned chunks without a pod")
	}
	if fake.callCount("CreateJob") != createsBefore || fake.callCount("DeleteJob") != deletesBefore {
		t.Fatal("Logs changed workload lifecycle state")
	}
	if fake.jobCount() != 1 {
		t.Fatalf("Logs changed the job count to %d", fake.jobCount())
	}
}

// TestLogsPodLogDisappearanceIsNotFound proves a pod that disappears between the
// successful pod list and the pod-log read is reported as NotFound rather than a
// generic retryable dependency fault. The collector's existing unavailable-marker
// path can then record one explicit marker instead of parking cleanup on a log
// source that no longer exists.
func TestLogsPodLogDisappearanceIsNotFound(t *testing.T) {
	fake, _, adapter, request := setupStarted(t)
	fake.addPod(testRunningPod(request.Identity))
	fake.injectBefore("ReadPodLogs", fmt.Errorf("pod log endpoint: %w", ErrNotFound))

	batch, err := adapter.Logs(context.Background(), engine.LogsRequest{
		Identity: request.Identity,
		Limit:    1024,
	})
	if len(batch.Chunks) != 0 {
		t.Fatalf("Logs returned %d chunks for a vanished pod log", len(batch.Chunks))
	}
	if err == nil {
		t.Fatal("Logs succeeded although the pod-log endpoint reported a vanished pod")
	}
	class, ok := fault.ClassOf(err)
	if !ok || class != fault.NotFound {
		t.Fatalf("vanished pod-log class = %v (present=%v), error %v; want %q",
			class, ok, err, fault.NotFound)
	}
	if !errors.Is(err, ErrNotFound) || !errors.Is(err, ErrPodLogNotFound) {
		t.Fatalf("error %v does not classify as both ErrNotFound and ErrPodLogNotFound", err)
	}
}

// TestLogsKeepsLogSourceFailuresRetryable proves the log source's transient and
// recoverable failures stay retryable dependency faults instead of being
// discarded or mistaken for container absence. The classification sentinel names
// the inner operation without exposing a response body.
func TestLogsKeepsLogSourceFailuresRetryable(t *testing.T) {
	cases := []struct {
		name     string
		inject   func(*fakeCluster)
		sentinel error
	}{
		{
			name: "pod list unavailable",
			inject: func(fake *fakeCluster) {
				// The real driver does not emit the classification sentinel for
				// a pod list failure; the adapter must add it.
				fake.injectBefore("ListPods", errors.New("pod list request rejected"))
			},
			sentinel: ErrPodListUnavailable,
		},
		{
			name: "pod log request rejected",
			inject: func(fake *fakeCluster) {
				fake.injectBefore("ReadPodLogs", fmt.Errorf("pod log: %w", ErrPodLogBadRequest))
			},
			sentinel: ErrPodLogBadRequest,
		},
		{
			name: "pod log server failure",
			inject: func(fake *fakeCluster) {
				fake.injectBefore("ReadPodLogs", fault.Wrap(fault.UncertainExternalEffect, "pod log server failure", ErrPodLogServerError))
			},
			sentinel: ErrPodLogServerError,
		},
		{
			name: "transport failure",
			inject: func(fake *fakeCluster) {
				fake.injectBefore("ReadPodLogs", errors.New("connection reset"))
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fake, _, adapter, request := setupStarted(t)
			fake.addPod(testRunningPod(request.Identity))
			testCase.inject(fake)

			batch, err := adapter.Logs(context.Background(), engine.LogsRequest{
				Identity: request.Identity,
				Limit:    1024,
			})
			if len(batch.Chunks) != 0 {
				t.Fatalf("failed log read returned %d chunks", len(batch.Chunks))
			}
			class, ok := fault.ClassOf(err)
			if !ok || class != fault.DependencyUnavailable {
				t.Fatalf("log source failure class = %v (present=%v), error %v; want %q",
					class, ok, err, fault.DependencyUnavailable)
			}
			if testCase.sentinel != nil && !errors.Is(err, testCase.sentinel) {
				t.Fatalf("error %v does not wrap %v", err, testCase.sentinel)
			}
		})
	}
}

// TestLogsReadsTerminatedContainerOutput proves a terminated workload container
// still serves readable log output: terminal status alone never justifies
// discarding retained output.
func TestLogsReadsTerminatedContainerOutput(t *testing.T) {
	cases := []struct {
		name string
		pod  func(engine.EffectIdentity) Pod
	}{
		{name: "succeeded pod", pod: func(identity engine.EffectIdentity) Pod {
			return testTerminatedPod(identity, "Succeeded", 0)
		}},
		{name: "terminating terminated pod", pod: func(identity engine.EffectIdentity) Pod {
			return testTerminatingTerminatedPod(identity, "Succeeded", 0)
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fake, _, adapter, request := setupStarted(t)
			pod := testCase.pod(request.Identity)
			fake.addPod(pod)
			fake.setPodLogs(pod.Name, LogStreamCombined, []byte("retained output\n"))

			batch, err := adapter.Logs(context.Background(), engine.LogsRequest{
				Identity: request.Identity,
				Limit:    1024,
			})
			if err != nil {
				t.Fatalf("Logs after termination: %v", err)
			}
			if len(batch.Chunks) != 1 || string(batch.Chunks[0].Data) != "retained output\n" {
				t.Fatalf("chunks = %+v; want the terminated container's output", batch.Chunks)
			}
		})
	}
}

// TestLogsFailClosedWhenRedactionSourceUnavailable proves configured values are
// never emitted when the adapter cannot read the values it must redact.
func TestLogsFailClosedWhenRedactionSourceUnavailable(t *testing.T) {
	fake, _, adapter, request := setupStarted(t)
	pod := testRunningPod(request.Identity)
	fake.addPod(pod)
	fake.setPodLogs(pod.Name, LogStreamCombined, []byte("credential tok-abc\n"))

	secret := fake.secretsSnapshot()[0]
	if err := fake.DeleteSecret(context.Background(), secret.Namespace, secret.Name); err != nil {
		t.Fatalf("deleting the credential secret: %v", err)
	}

	batch, err := adapter.Logs(context.Background(), engine.LogsRequest{
		Identity:    request.Identity,
		AfterOffset: 0,
		Limit:       1024,
	})
	if err == nil {
		t.Fatal("Logs emitted output without a redaction source")
	}
	class, ok := fault.ClassOf(err)
	if !ok || class != fault.DependencyUnavailable {
		t.Fatalf("missing redaction source class = %v (present=%v), error %v; want fail-closed %q",
			class, ok, err, fault.DependencyUnavailable)
	}
	if !errors.Is(err, ErrRedactionSourceUnavailable) {
		t.Fatalf("error %v does not classify the missing redaction source", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatal("a missing redaction source was mistaken for container absence")
	}
	for _, chunk := range batch.Chunks {
		if strings.Contains(string(chunk.Data), request.Credential) {
			t.Fatalf("Logs leaked the credential without its redaction source: %q", chunk.Data)
		}
	}
}
