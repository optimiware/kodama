// Combined-log redaction boundaries: nested/overlapping configured values,
// end-of-log flush, tiny values, page metadata and continuation-token range.
package kube

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/kernel/engine"
)

// observedLogPage is one chunk the adapter emitted.
type observedLogPage struct {
	data      []byte
	truncated bool
}

// collectLogPages drives the adapter with the real continuation cursor until a
// read makes no progress. It asserts the per-page contract: one ordered stdout
// chunk, the requested bound, and a strictly advancing or complete cursor.
func collectLogPages(t *testing.T, adapter *Adapter, identity engine.EffectIdentity, limit int) []observedLogPage {
	t.Helper()
	var pages []observedLogPage
	cursor := int64(0)
	for step := range 10_000 {
		batch, err := adapter.Logs(context.Background(), engine.LogsRequest{
			Identity:    identity,
			AfterOffset: cursor,
			Limit:       limit,
		})
		if err != nil {
			t.Fatalf("page %d Logs returned error: %v", step, err)
		}
		for index, chunk := range batch.Chunks {
			if chunk.Stream != engine.LogStdout {
				t.Fatalf("page %d chunk %d stream = %q; want stdout", step, index, chunk.Stream)
			}
			if chunk.Sequence != 1 {
				t.Fatalf("page %d chunk %d sequence = %d; want 1", step, index, chunk.Sequence)
			}
			if len(chunk.Data) > limit {
				t.Fatalf("page %d emitted %d bytes; over the bound %d", step, len(chunk.Data), limit)
			}
			pages = append(pages, observedLogPage{data: chunk.Data, truncated: chunk.Truncated})
		}
		if batch.NextOffset == cursor {
			return pages
		}
		if batch.NextOffset < cursor {
			t.Fatalf("page %d cursor moved backwards: %d -> %d", step, cursor, batch.NextOffset)
		}
		cursor = batch.NextOffset
	}
	t.Fatal("log pagination did not terminate")
	return nil
}

func joinPages(pages []observedLogPage) []byte {
	var joined []byte
	for _, page := range pages {
		joined = append(joined, page.data...)
	}
	return joined
}

// TestLogsRedactsNestedValueAtPageBoundary pins the credential-tail leak: a
// page ending inside a longer configured value must not match the shorter value
// that is its prefix, emit that marker, and leak the longer value's tail from
// the following page. The held bytes must be re-read whole.
func TestLogsRedactsNestedValueAtPageBoundary(t *testing.T) {
	short := "abcdefghijklmnop"
	long := short + "QRSTUVWX"
	for _, limit := range []int{24, 25} {
		t.Run(fmt.Sprintf("limit_%d", limit), func(t *testing.T) {
			fake, _, adapter := testSetup(t)
			request := testPrepareRequest()
			request.Secrets = []engine.SecretValue{
				{Name: "NESTED_SHORT", Value: short},
				{Name: "NESTED_LONG", Value: long},
			}
			prepareTestWorkload(t, adapter, request)
			startTestWorkload(t, adapter, request.Identity, request.WorkspaceID)
			pod := testRunningPod(request.Identity)
			fake.addPod(pod)
			fake.setPodLogs(pod.Name, LogStreamCombined, []byte("00"+long+"-end"))

			pages := collectLogPages(t, adapter, request.Identity, limit)
			got := joinPages(pages)
			if want := "00" + Redacted + "-end"; string(got) != want {
				t.Fatalf("paged output = %q; want %q", got, want)
			}
			for _, leak := range []string{short, long, "QRSTUVWX"} {
				if bytes.Contains(got, []byte(leak)) {
					t.Fatalf("output leaked %q: %q", leak, got)
				}
			}
		})
	}
}

// TestLogsHoldsEveryConfiguredValueNotJustTheLongest proves the holdback checks
// every configured value, not only the first (longest) one: a shorter value can
// be the only one that a page tail extends.
func TestLogsHoldsEveryConfiguredValueNotJustTheLongest(t *testing.T) {
	fake, _, adapter := testSetup(t)
	request := testPrepareRequest()
	request.Secrets = []engine.SecretValue{
		{Name: "API_KEY", Value: "ab"},
		{Name: "LONGER_UNRELATED", Value: "zzz"},
	}
	prepareTestWorkload(t, adapter, request)
	startTestWorkload(t, adapter, request.Identity, request.WorkspaceID)
	pod := testRunningPod(request.Identity)
	fake.addPod(pod)
	prefix := strings.Repeat("x", 9)
	fake.setPodLogs(pod.Name, LogStreamCombined, []byte(prefix+"ab"+"y"))

	pages := collectLogPages(t, adapter, request.Identity, 10)
	got := joinPages(pages)
	if want := prefix + Redacted + "y"; string(got) != want {
		t.Fatalf("paged output = %q; want %q", got, want)
	}
}

// TestLogsFlushesPartialValuePrefixAtEndOfLog proves a partial prefix of a
// configured value is emitted when the page is the complete log tail: nothing
// can extend it, so holding it would lose bytes or stall the cursor forever.
func TestLogsFlushesPartialValuePrefixAtEndOfLog(t *testing.T) {
	cases := []struct {
		name  string
		log   string
		limit int
	}{
		{name: "prefix after text", log: "00ab", limit: 32},
		{name: "whole page is a prefix", log: "ab", limit: 32},
		{name: "short page is a prefix", log: "00ab", limit: 2},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fake, _, adapter := testSetup(t)
			request := testPrepareRequest()
			request.Secrets = []engine.SecretValue{{Name: "API_KEY", Value: "abcdef"}}
			prepareTestWorkload(t, adapter, request)
			startTestWorkload(t, adapter, request.Identity, request.WorkspaceID)
			pod := testRunningPod(request.Identity)
			fake.addPod(pod)
			fake.setPodLogs(pod.Name, LogStreamCombined, []byte(testCase.log))

			pages := collectLogPages(t, adapter, request.Identity, testCase.limit)
			if got := string(joinPages(pages)); got != testCase.log {
				t.Fatalf("paged output = %q; want the complete log %q", got, testCase.log)
			}
		})
	}
}

// TestLogsHoldsWholePagePrefixWithoutDroppingBytes documents the live-log
// boundary: when the log may still grow and the held run fills the page, the
// read returns no progress and no data rather than emitting a value's prefix or
// skipping bytes. Raising the limit resolves the same cursor.
func TestLogsHoldsWholePagePrefixWithoutDroppingBytes(t *testing.T) {
	fake, _, adapter := testSetup(t)
	request := testPrepareRequest()
	request.Secrets = []engine.SecretValue{{Name: "API_KEY", Value: "abcdef"}}
	prepareTestWorkload(t, adapter, request)
	startTestWorkload(t, adapter, request.Identity, request.WorkspaceID)
	pod := testRunningPod(request.Identity)
	fake.addPod(pod)
	fake.setPodLogs(pod.Name, LogStreamCombined, []byte("abcdefg"))

	stalled, err := adapter.Logs(context.Background(), engine.LogsRequest{
		Identity: request.Identity,
		Limit:    2,
	})
	if err != nil {
		t.Fatalf("held Logs returned error: %v", err)
	}
	if len(stalled.Chunks) != 0 {
		t.Fatalf("held page emitted %d chunks; want no data before the value is complete", len(stalled.Chunks))
	}
	if stalled.NextOffset != 0 {
		t.Fatalf("held page advanced the cursor to %d; want no progress", stalled.NextOffset)
	}

	// The same cursor with a bound that holds the whole value makes progress and
	// still redacts it.
	resolved, err := adapter.Logs(context.Background(), engine.LogsRequest{
		Identity: request.Identity,
		Limit:    16,
	})
	if err != nil {
		t.Fatalf("resolved Logs returned error: %v", err)
	}
	if len(resolved.Chunks) != 1 || string(resolved.Chunks[0].Data) != Redacted+"g" {
		t.Fatalf("resolved chunks = %+v; want %q", resolved.Chunks, Redacted+"g")
	}
}

// TestLogsHoldsOneAndTwoByteValueBoundaries pins the smallest holdback cases: a
// one-byte value can never be a proper prefix, so it is redacted immediately; a
// two-byte value whose first byte ends a truncated page is held.
func TestLogsHoldsOneAndTwoByteValueBoundaries(t *testing.T) {
	t.Run("two byte value", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		request := testPrepareRequest()
		request.Secrets = []engine.SecretValue{{Name: "API_KEY", Value: "ab"}}
		prepareTestWorkload(t, adapter, request)
		startTestWorkload(t, adapter, request.Identity, request.WorkspaceID)
		pod := testRunningPod(request.Identity)
		fake.addPod(pod)
		// The page ends after the value's first byte, so the two-byte value is
		// held; the bound still fits the whole marker for the next page.
		prefix := strings.Repeat("x", 9)
		fake.setPodLogs(pod.Name, LogStreamCombined, []byte(prefix+"ab"+"y"))

		pages := collectLogPages(t, adapter, request.Identity, 10)
		got := joinPages(pages)
		if want := prefix + Redacted + "y"; string(got) != want {
			t.Fatalf("paged output = %q; want %q", got, want)
		}
	})
	t.Run("one byte value full bound", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		request := testPrepareRequest()
		request.Secrets = []engine.SecretValue{{Name: "API_KEY", Value: "a"}}
		prepareTestWorkload(t, adapter, request)
		startTestWorkload(t, adapter, request.Identity, request.WorkspaceID)
		pod := testRunningPod(request.Identity)
		fake.addPod(pod)
		fake.setPodLogs(pod.Name, LogStreamCombined, []byte("xay"))

		pages := collectLogPages(t, adapter, request.Identity, 16)
		if got, want := string(joinPages(pages)), "x"+Redacted+"y"; got != want {
			t.Fatalf("paged output = %q; want %q", got, want)
		}
	})
	t.Run("one byte value tiny bound clips only the marker", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		request := testPrepareRequest()
		request.Secrets = []engine.SecretValue{{Name: "API_KEY", Value: "a"}}
		prepareTestWorkload(t, adapter, request)
		startTestWorkload(t, adapter, request.Identity, request.WorkspaceID)
		pod := testRunningPod(request.Identity)
		fake.addPod(pod)
		fake.setPodLogs(pod.Name, LogStreamCombined, []byte("xay"))

		pages := collectLogPages(t, adapter, request.Identity, 2)
		got := joinPages(pages)
		want := "x" + Redacted[:2] + "y"
		if string(got) != want {
			t.Fatalf("paged output = %q; want %q", got, want)
		}
		if bytes.Contains(got, []byte("a")) {
			t.Fatalf("output leaked the one-byte configured value: %q", got)
		}
	})
}

// TestLogsPagesCarryTruncationSequenceAndForwardedLimit proves every chunk
// reports the driver's truncation, the ordered sequence number, and that the
// adapter forwards the requested bound unchanged rather than widening the read.
func TestLogsPagesCarryTruncationSequenceAndForwardedLimit(t *testing.T) {
	fake, _, adapter, request := setupStarted(t)
	pod := testRunningPod(request.Identity)
	fake.addPod(pod)
	fake.setPodLogs(pod.Name, LogStreamCombined, []byte(strings.Repeat("z", 100)))
	fake.resetCallLog()

	const limit = 32
	cursor := int64(0)
	for step, wantTruncated := range []bool{true, true, true, false} {
		batch, err := adapter.Logs(context.Background(), engine.LogsRequest{
			Identity:    request.Identity,
			AfterOffset: cursor,
			Limit:       limit,
		})
		if err != nil {
			t.Fatalf("page %d Logs returned error: %v", step, err)
		}
		if len(batch.Chunks) != 1 {
			t.Fatalf("page %d emitted %d chunks; want 1", step, len(batch.Chunks))
		}
		chunk := batch.Chunks[0]
		if chunk.Sequence != 1 {
			t.Fatalf("page %d sequence = %d; want 1", step, chunk.Sequence)
		}
		if chunk.Truncated != wantTruncated {
			t.Fatalf("page %d truncated = %v; want %v", step, chunk.Truncated, wantTruncated)
		}
		cursor = batch.NextOffset
	}

	requests := fake.logRequestsSnapshot()
	if len(requests) != 4 {
		t.Fatalf("adapter issued %d log reads; want 4", len(requests))
	}
	for index, forwarded := range requests {
		if forwarded.Limit != limit {
			t.Fatalf("read %d forwarded limit = %d; want %d", index, forwarded.Limit, limit)
		}
		if forwarded.Stream != LogStreamCombined {
			t.Fatalf("read %d forwarded stream = %q; want %q", index, forwarded.Stream, LogStreamCombined)
		}
	}
}

// TestLogsRefusesCursorPositionOutsideTokenRange proves a position the 31-bit
// high half cannot carry is refused instead of surfacing as a negative cursor:
// 2^31-1 is accepted, 2^31 is refused.
func TestLogsRefusesCursorPositionOutsideTokenRange(t *testing.T) {
	accepted := encodeLogCursor(maxLogCursorPosition, 0)
	if accepted <= 0 {
		t.Fatalf("encodeLogCursor(2^31-1) = %d; want a positive token", accepted)
	}
	if position, stderr := decodeLogCursor(accepted); position != maxLogCursorPosition || stderr != 0 {
		t.Fatalf("decoded boundary token = (%d,%d); want (%d,0)", position, stderr, maxLogCursorPosition)
	}
	if next, err := nextLogCursor(maxLogCursorPosition, 0); err != nil || next != accepted {
		t.Fatalf("nextLogCursor(2^31-1,0) = (%d,%v); want (%d,nil)", next, err, accepted)
	}
	if _, err := nextLogCursor(maxLogCursorPosition, 1); err == nil {
		t.Fatal("nextLogCursor accepted a position at 2^31")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("overflow class = %v (present=%v); want %q", class, ok, fault.InvalidInput)
	}
	if next, err := nextLogCursor(maxLogCursorPosition-1, 1); err != nil || next != accepted {
		t.Fatalf("nextLogCursor(2^31-2,1) = (%d,%v); want (%d,nil)", next, err, accepted)
	}

	fake, _, adapter := testSetup(t)
	request := testPrepareRequest()
	prepareTestWorkload(t, adapter, request)
	startTestWorkload(t, adapter, request.Identity, request.WorkspaceID)
	pod := testRunningPod(request.Identity)
	fake.addPod(pod)
	fake.setPodLogs(pod.Name, LogStreamCombined, []byte("ignored"))

	batch, err := adapter.Logs(context.Background(), engine.LogsRequest{
		Identity:    request.Identity,
		AfterOffset: accepted,
		Limit:       16,
	})
	if err != nil {
		t.Fatalf("Logs refused the 2^31-1 cursor: %v", err)
	}
	if batch.NextOffset != accepted {
		t.Fatalf("boundary cursor advanced to %d; want %d", batch.NextOffset, accepted)
	}
	if batch.NextOffset < 0 {
		t.Fatalf("boundary cursor went negative: %d", batch.NextOffset)
	}

	readsBefore := fake.callCount("ReadPodLogs")
	overflow := encodeLogCursor(maxLogCursorPosition+1, 0)
	if overflow >= 0 {
		t.Fatalf("encodeLogCursor(2^31) = %d; expected a negative token for the test", overflow)
	}
	_, err = adapter.Logs(context.Background(), engine.LogsRequest{
		Identity:    request.Identity,
		AfterOffset: overflow,
		Limit:       16,
	})
	if err == nil {
		t.Fatal("Logs accepted a cursor that encodes an unrepresentable position")
	}
	if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
		t.Fatalf("overflow class = %v (present=%v); want %q", class, ok, fault.InvalidInput)
	}
	if fake.callCount("ReadPodLogs") != readsBefore {
		t.Fatal("an unrepresentable cursor reached the cluster")
	}
}
