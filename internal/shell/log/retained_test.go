package log_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/log"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

var testNow = time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)

func TestFileSinkAppendReadRoundTrip(t *testing.T) {
	clock := &fakeClock{now: testNow}
	sink := newFileSink(t, defaultSinkConfig(t, clock))
	scope := testScope("tenant-a", "project-a")

	first, err := sink.Append(context.Background(), scope, testChunk("exec-1", "action-1", log.StreamStdout, 0, 1, "a"))
	if err != nil {
		t.Fatalf("Append first: %v", err)
	}
	if first.Offset != 0 || first.Sequence != 1 {
		t.Fatalf("first result = %+v; want offset 0 sequence 1", first)
	}
	if _, err := sink.Append(context.Background(), scope, testChunk("exec-1", "action-1", log.StreamStdout, 10, 2, "b")); err != nil {
		t.Fatalf("Append second: %v", err)
	}
	if _, err := sink.Append(context.Background(), scope, testChunk("exec-1", "action-1", log.StreamStdout, 20, 3, "c")); err != nil {
		t.Fatalf("Append third: %v", err)
	}
	if _, err := sink.Append(context.Background(), scope, testChunk("exec-1", "action-1", log.StreamStderr, 0, 1, "err")); err != nil {
		t.Fatalf("Append stderr: %v", err)
	}

	page, err := sink.Read(context.Background(), testReadRequest(scope, "exec-1", "action-1", log.StreamStdout, 0, 2))
	if err != nil {
		t.Fatalf("Read first page: %v", err)
	}
	if len(page.Chunks) != 2 || string(page.Chunks[0].Data) != "a" || string(page.Chunks[1].Data) != "b" {
		t.Fatalf("first page = %+v; want a then b", page.Chunks)
	}
	if page.NextOffset != 10 || page.Truncated {
		t.Fatalf("first page next = %d truncated = %v; want 10 false", page.NextOffset, page.Truncated)
	}

	page, err = sink.Read(context.Background(), testReadRequest(scope, "exec-1", "action-1", log.StreamStdout, 10, 2))
	if err != nil {
		t.Fatalf("Read second page: %v", err)
	}
	if len(page.Chunks) != 1 || string(page.Chunks[0].Data) != "c" {
		t.Fatalf("second page = %+v; want c", page.Chunks)
	}
	if page.NextOffset != 20 || page.Truncated {
		t.Fatalf("second page next = %d truncated = %v; want 20 false", page.NextOffset, page.Truncated)
	}

	page, err = sink.Read(context.Background(), testReadRequest(scope, "exec-1", "action-1", log.StreamStdout, 20, 2))
	if err != nil {
		t.Fatalf("Read third page: %v", err)
	}
	if len(page.Chunks) != 0 || page.NextOffset != 20 || page.Truncated {
		t.Fatalf("third page = %+v; want empty at 20", page)
	}

	stderr, err := sink.Read(context.Background(), testReadRequest(scope, "exec-1", "action-1", log.StreamStderr, 0, 2))
	if err != nil {
		t.Fatalf("Read stderr: %v", err)
	}
	if len(stderr.Chunks) != 1 || string(stderr.Chunks[0].Data) != "err" {
		t.Fatalf("stderr page = %+v; want the stderr chunk", stderr.Chunks)
	}
}

func TestFileSinkRejectsOversizedChunk(t *testing.T) {
	clock := &fakeClock{now: testNow}
	sink := newFileSink(t, defaultSinkConfig(t, clock))
	scope := testScope("tenant-a", "project-a")
	max := log.DefaultBounds.MaxChunkBytes

	if _, err := sink.Append(context.Background(), scope, testChunk("exec-1", "action-1", log.StreamStdout, 0, 1, strings.Repeat("x", max))); err != nil {
		t.Fatalf("Append at the exact chunk bound: %v", err)
	}
	_, err := sink.Append(context.Background(), scope, testChunk("exec-1", "action-1", log.StreamStdout, uint64(max), 2, strings.Repeat("x", max+1)))
	requireClass(t, err, fault.InvalidInput)
}

func TestFileSinkExpiresByRetention(t *testing.T) {
	clock := &fakeClock{now: testNow}
	root := t.TempDir()
	config := defaultSinkConfig(t, clock)
	config.Root = root
	sink := newFileSink(t, config)
	scope := testScope("tenant-a", "project-a")

	if _, err := sink.Append(context.Background(), scope, testChunk("exec-1", "action-1", log.StreamStdout, 10, 1, "chunk")); err != nil {
		t.Fatalf("Append: %v", err)
	}

	clock.Advance(log.DefaultRetention().RetainFor - time.Nanosecond)
	page, err := sink.Read(context.Background(), testReadRequest(scope, "exec-1", "action-1", log.StreamStdout, 0, 10))
	if err != nil {
		t.Fatalf("Read before expiry: %v", err)
	}
	if len(page.Chunks) != 1 || page.Truncated {
		t.Fatalf("page before expiry = %+v; want the retained chunk", page)
	}

	clock.Advance(time.Nanosecond)
	page, err = sink.Read(context.Background(), testReadRequest(scope, "exec-1", "action-1", log.StreamStdout, 0, 10))
	if err != nil {
		t.Fatalf("Read at expiry: %v", err)
	}
	if len(page.Chunks) != 0 || !page.Truncated {
		t.Fatalf("page at expiry = %+v; want empty and truncated", page)
	}
	if page.NextOffset != 0 {
		t.Fatalf("expired page next offset = %d; want the request offset", page.NextOffset)
	}

	page, err = sink.Read(context.Background(), testReadRequest(scope, "exec-1", "action-1", log.StreamStdout, 10, 10))
	if err != nil {
		t.Fatalf("Read after the expired chunk: %v", err)
	}
	if len(page.Chunks) != 0 || page.Truncated {
		t.Fatalf("page after the expired chunk = %+v; want empty and not truncated", page)
	}

	reopened := newFileSink(t, config)
	page, err = reopened.Read(context.Background(), testReadRequest(scope, "exec-1", "action-1", log.StreamStdout, 0, 10))
	if err != nil {
		t.Fatalf("Read from reopened sink: %v", err)
	}
	if len(page.Chunks) != 0 || !page.Truncated {
		t.Fatalf("reopened page = %+v; want expiry persisted", page)
	}
}

func TestFileSinkBoundsStoredChunks(t *testing.T) {
	clock := &fakeClock{now: testNow}
	config := defaultSinkConfig(t, clock)
	config.Limits = log.Limits{
		MaxChunkBytes:   1024,
		MaxReadChunks:   10,
		MaxStoredChunks: 2,
		MaxQueuedChunks: 2,
	}
	sink := newFileSink(t, config)
	scope := testScope("tenant-a", "project-a")

	for index, offset := range []uint64{10, 20, 30} {
		if _, err := sink.Append(context.Background(), scope, testChunk("exec-1", "action-1", log.StreamStdout, offset, uint64(index+1), "chunk")); err != nil {
			t.Fatalf("Append %d: %v", index, err)
		}
	}

	page, err := sink.Read(context.Background(), testReadRequest(scope, "exec-1", "action-1", log.StreamStdout, 5, 10))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(page.Chunks) != 2 || page.Chunks[0].Offset != 20 || page.Chunks[1].Offset != 30 {
		t.Fatalf("bounded page = %+v; want offsets 20 and 30", page.Chunks)
	}
	if !page.Truncated {
		t.Fatal("storage eviction was not reported as truncation")
	}

	page, err = sink.Read(context.Background(), testReadRequest(scope, "exec-1", "action-1", log.StreamStdout, 10, 10))
	if err != nil {
		t.Fatalf("Read after the dropped offset: %v", err)
	}
	if len(page.Chunks) != 2 || page.Truncated {
		t.Fatalf("page after the dropped offset = %+v; want no truncation", page)
	}

	page, err = sink.Read(context.Background(), testReadRequest(scope, "exec-1", "action-1", log.StreamStdout, 30, 10))
	if err != nil {
		t.Fatalf("Read after the last chunk: %v", err)
	}
	if len(page.Chunks) != 0 || page.Truncated || page.NextOffset != 30 {
		t.Fatalf("last page = %+v; want empty at 30", page)
	}
}

func TestFileSinkDeduplicatesRetriedAppend(t *testing.T) {
	clock := &fakeClock{now: testNow}
	sink := newFileSink(t, defaultSinkConfig(t, clock))
	scope := testScope("tenant-a", "project-a")
	chunk := testChunk("exec-1", "action-1", log.StreamStdout, 10, 1, "same")

	first, err := sink.Append(context.Background(), scope, chunk)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	second, err := sink.Append(context.Background(), scope, chunk)
	if err != nil {
		t.Fatalf("retried Append: %v", err)
	}
	if second != first {
		t.Fatalf("retried result = %+v; want %+v", second, first)
	}
	page, err := sink.Read(context.Background(), testReadRequest(scope, "exec-1", "action-1", log.StreamStdout, 0, 10))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(page.Chunks) != 1 {
		t.Fatalf("chunk count after retry = %d; want 1", len(page.Chunks))
	}

	changed := chunk
	changed.Data = []byte("different")
	_, err = sink.Append(context.Background(), scope, changed)
	requireClass(t, err, fault.ChangedIdempotencyContent)

	otherSequence := chunk
	otherSequence.Sequence = 2
	if _, err := sink.Append(context.Background(), scope, otherSequence); err != nil {
		t.Fatalf("Append with a new sequence: %v", err)
	}
	page, err = sink.Read(context.Background(), testReadRequest(scope, "exec-1", "action-1", log.StreamStdout, 0, 10))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(page.Chunks) != 2 {
		t.Fatalf("chunk count = %d; want 2 distinct sequence chunks", len(page.Chunks))
	}

	otherScope := testScope("tenant-a", "project-b")
	if _, err := sink.Append(context.Background(), otherScope, chunk); err != nil {
		t.Fatalf("Append in another scope: %v", err)
	}
	page, err = sink.Read(context.Background(), testReadRequest(otherScope, "exec-1", "action-1", log.StreamStdout, 0, 10))
	if err != nil {
		t.Fatalf("Read in another scope: %v", err)
	}
	if len(page.Chunks) != 1 {
		t.Fatalf("cross-scope chunk count = %d; want the scope-local chunk", len(page.Chunks))
	}
}

func TestFileSinkOrdersByOffsetThenSequence(t *testing.T) {
	clock := &fakeClock{now: testNow}
	sink := newFileSink(t, defaultSinkConfig(t, clock))
	scope := testScope("tenant-a", "project-a")

	if _, err := sink.Append(context.Background(), scope, testChunk("exec-1", "action-1", log.StreamStdout, 10, 2, "second")); err != nil {
		t.Fatalf("Append second: %v", err)
	}
	if _, err := sink.Append(context.Background(), scope, testChunk("exec-1", "action-1", log.StreamStdout, 10, 1, "first")); err != nil {
		t.Fatalf("Append first: %v", err)
	}
	page, err := sink.Read(context.Background(), testReadRequest(scope, "exec-1", "action-1", log.StreamStdout, 0, 10))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(page.Chunks) != 2 || string(page.Chunks[0].Data) != "first" || string(page.Chunks[1].Data) != "second" {
		t.Fatalf("ordered chunks = %+v; want first then second", page.Chunks)
	}
}

func TestFileSinkScopeIsolation(t *testing.T) {
	clock := &fakeClock{now: testNow}
	sink := newFileSink(t, defaultSinkConfig(t, clock))
	first := testScope("tenant-a", "project-a")
	second := testScope("tenant-a", "project-b")
	tenant := tenancy.Scope{TenantID: id.TenantID("tenant-a")}

	if _, err := sink.Append(context.Background(), first, testChunk("exec-1", "action-1", log.StreamStdout, 10, 1, "first")); err != nil {
		t.Fatalf("Append first: %v", err)
	}
	if _, err := sink.Append(context.Background(), second, testChunk("exec-1", "action-1", log.StreamStdout, 10, 1, "second")); err != nil {
		t.Fatalf("Append second: %v", err)
	}
	if _, err := sink.Append(context.Background(), tenant, testChunk("exec-1", "action-1", log.StreamStdout, 10, 1, "tenant")); err != nil {
		t.Fatalf("Append tenant: %v", err)
	}

	for _, testCase := range []struct {
		scope tenancy.Scope
		want  string
	}{
		{scope: first, want: "first"},
		{scope: second, want: "second"},
		{scope: tenant, want: "tenant"},
	} {
		page, err := sink.Read(context.Background(), testReadRequest(testCase.scope, "exec-1", "action-1", log.StreamStdout, 0, 10))
		if err != nil {
			t.Fatalf("Read %+v: %v", testCase.scope, err)
		}
		if len(page.Chunks) != 1 || string(page.Chunks[0].Data) != testCase.want {
			t.Fatalf("scope %+v chunks = %+v; want %q", testCase.scope, page.Chunks, testCase.want)
		}
	}
}

func TestFileSinkReopenPersists(t *testing.T) {
	clock := &fakeClock{now: testNow}
	root := t.TempDir()
	config := defaultSinkConfig(t, clock)
	config.Root = root
	first := newFileSink(t, config)
	scope := testScope("tenant-a", "project-a")
	if _, err := first.Append(context.Background(), scope, testChunk("exec-1", "action-1", log.StreamStdout, 10, 1, "persisted")); err != nil {
		t.Fatalf("Append: %v", err)
	}

	second := newFileSink(t, config)
	page, err := second.Read(context.Background(), testReadRequest(scope, "exec-1", "action-1", log.StreamStdout, 0, 10))
	if err != nil {
		t.Fatalf("Read from reopened sink: %v", err)
	}
	if len(page.Chunks) != 1 || string(page.Chunks[0].Data) != "persisted" {
		t.Fatalf("reopened chunks = %+v; want the persisted chunk", page.Chunks)
	}
}

func TestFileSinkReadValidation(t *testing.T) {
	clock := &fakeClock{now: testNow}
	config := defaultSinkConfig(t, clock)
	config.Limits = log.Limits{MaxChunkBytes: 1024, MaxReadChunks: 5, MaxStoredChunks: 10, MaxQueuedChunks: 2}
	sink := newFileSink(t, config)
	scope := testScope("tenant-a", "project-a")

	cases := []struct {
		name    string
		request log.ReadRequest
	}{
		{name: "zero limit", request: testReadRequest(scope, "exec-1", "action-1", log.StreamStdout, 0, 0)},
		{name: "negative limit", request: testReadRequest(scope, "exec-1", "action-1", log.StreamStdout, 0, -1)},
		{name: "limit over bound", request: testReadRequest(scope, "exec-1", "action-1", log.StreamStdout, 0, 6)},
		{name: "invalid stream", request: testReadRequest(scope, "exec-1", "action-1", log.Stream("stdin"), 0, 1)},
		{name: "invalid execution", request: testReadRequest(scope, "../exec", "action-1", log.StreamStdout, 0, 1)},
		{name: "invalid action", request: testReadRequest(scope, "exec-1", "../action", log.StreamStdout, 0, 1)},
		{name: "invalid scope", request: testReadRequest(tenancy.Scope{}, "exec-1", "action-1", log.StreamStdout, 0, 1)},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := sink.Read(context.Background(), testCase.request)
			requireClass(t, err, fault.InvalidInput)
		})
	}
}

func TestFileSinkAppendValidation(t *testing.T) {
	clock := &fakeClock{now: testNow}
	sink := newFileSink(t, defaultSinkConfig(t, clock))
	scope := testScope("tenant-a", "project-a")

	cases := []struct {
		name  string
		scope tenancy.Scope
		chunk log.Chunk
	}{
		{name: "invalid scope", scope: tenancy.Scope{}, chunk: testChunk("exec-1", "action-1", log.StreamStdout, 0, 1, "data")},
		{name: "invalid execution", scope: scope, chunk: testChunk("../exec", "action-1", log.StreamStdout, 0, 1, "data")},
		{name: "invalid action", scope: scope, chunk: testChunk("exec-1", "../action", log.StreamStdout, 0, 1, "data")},
		{name: "invalid stream", scope: scope, chunk: testChunk("exec-1", "action-1", log.Stream("stdin"), 0, 1, "data")},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := sink.Append(context.Background(), testCase.scope, testCase.chunk)
			requireClass(t, err, fault.InvalidInput)
		})
	}
}

func TestFileSinkRedactsConfiguredValue(t *testing.T) {
	clock := &fakeClock{now: testNow}
	config := defaultSinkConfig(t, clock)
	config.Redact = []string{"supersecret"}
	sink := newFileSink(t, config)
	scope := testScope("tenant-a", "project-a")

	if _, err := sink.Append(context.Background(), scope, testChunk("exec-1", "action-1", log.StreamStdout, 10, 1, "token=supersecret done")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := sink.Append(context.Background(), scope, testChunk("exec-1", "action-1", log.StreamStdout, 20, 2, "tail")); err != nil {
		t.Fatalf("Append tail: %v", err)
	}
	page, err := sink.Read(context.Background(), testReadRequest(scope, "exec-1", "action-1", log.StreamStdout, 0, 10))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	text := concatChunks(page.Chunks)
	if strings.Contains(text, "supersecret") {
		t.Fatalf("retained log contains the configured value: %q", text)
	}
	if !strings.Contains(text, "token=") {
		t.Fatalf("retained log lost the surrounding text: %q", text)
	}
}

func TestFileSinkRedactsValueSplitAcrossChunks(t *testing.T) {
	clock := &fakeClock{now: testNow}
	config := defaultSinkConfig(t, clock)
	config.Redact = []string{"supersecret"}
	sink := newFileSink(t, config)
	scope := testScope("tenant-a", "project-a")

	if _, err := sink.Append(context.Background(), scope, testChunk("exec-1", "action-1", log.StreamStdout, 10, 1, "prefix super")); err != nil {
		t.Fatalf("Append first half: %v", err)
	}
	if _, err := sink.Append(context.Background(), scope, testChunk("exec-1", "action-1", log.StreamStdout, 20, 2, "secret suffix")); err != nil {
		t.Fatalf("Append second half: %v", err)
	}
	if _, err := sink.Append(context.Background(), scope, testChunk("exec-1", "action-1", log.StreamStdout, 30, 3, "end")); err != nil {
		t.Fatalf("Append tail: %v", err)
	}
	page, err := sink.Read(context.Background(), testReadRequest(scope, "exec-1", "action-1", log.StreamStdout, 0, 10))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	text := concatChunks(page.Chunks)
	if strings.Contains(text, "supersecret") {
		t.Fatalf("retained log contains the split configured value: %q", text)
	}
	if !strings.Contains(text, "prefix") {
		t.Fatalf("retained log lost the prefix text: %q", text)
	}
}

func TestFileSinkRedactionRejectsEmptyValue(t *testing.T) {
	clock := &fakeClock{now: testNow}
	config := defaultSinkConfig(t, clock)
	config.Redact = []string{""}
	_, err := log.NewFileSink(config)
	requireClass(t, err, fault.InvalidInput)
}

func TestFileSinkWithoutRedactionPreservesExactBytes(t *testing.T) {
	clock := &fakeClock{now: testNow}
	sink := newFileSink(t, defaultSinkConfig(t, clock))
	scope := testScope("tenant-a", "project-a")
	data := "line1\nline2\x00binary"
	if _, err := sink.Append(context.Background(), scope, testChunk("exec-1", "action-1", log.StreamStdout, 10, 1, data)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	page, err := sink.Read(context.Background(), testReadRequest(scope, "exec-1", "action-1", log.StreamStdout, 0, 10))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(page.Chunks) != 1 || string(page.Chunks[0].Data) != data {
		t.Fatalf("chunk data = %q; want exact bytes %q", concatChunks(page.Chunks), data)
	}
}

func TestFileSinkConcurrentAppendsDifferentStreams(t *testing.T) {
	clock := &fakeClock{now: testNow}
	sink := newFileSink(t, defaultSinkConfig(t, clock))
	scope := testScope("tenant-a", "project-a")

	var wait sync.WaitGroup
	errors := make(chan error, 2)
	for _, stream := range []log.Stream{log.StreamStdout, log.StreamStderr} {
		wait.Add(1)
		go func(stream log.Stream) {
			defer wait.Done()
			if _, err := sink.Append(context.Background(), scope, testChunk("exec-1", "action-1", stream, 0, 1, string(stream))); err != nil {
				errors <- err
			}
		}(stream)
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		t.Fatalf("concurrent Append: %v", err)
	}
	for _, stream := range []log.Stream{log.StreamStdout, log.StreamStderr} {
		page, err := sink.Read(context.Background(), testReadRequest(scope, "exec-1", "action-1", stream, 0, 10))
		if err != nil {
			t.Fatalf("Read %q: %v", stream, err)
		}
		if len(page.Chunks) != 1 || string(page.Chunks[0].Data) != string(stream) {
			t.Fatalf("stream %q chunks = %+v; want one chunk", stream, page.Chunks)
		}
	}
}

func TestFileSinkDefaultRetentionAndLimits(t *testing.T) {
	retention := log.DefaultRetention()
	if retention.MaxChunkBytes != 32<<10 {
		t.Fatalf("default chunk bound = %d; want 32 KiB", retention.MaxChunkBytes)
	}
	if retention.RetainFor != 30*24*time.Hour {
		t.Fatalf("default retention = %v; want 30 days", retention.RetainFor)
	}
	limits := log.DefaultLimits()
	if limits.MaxChunkBytes != log.DefaultBounds.MaxChunkBytes {
		t.Fatalf("default limit chunk bound = %d; want the declared chunk bound", limits.MaxChunkBytes)
	}
	if limits.MaxReadChunks <= 0 || limits.MaxStoredChunks <= 0 || limits.MaxQueuedChunks <= 0 {
		t.Fatalf("default limits are not positive: %+v", limits)
	}
}

func TestFileSinkInvalidConfig(t *testing.T) {
	clock := &fakeClock{now: testNow}
	cases := []struct {
		name   string
		config log.SinkConfig
	}{
		{name: "empty root", config: log.SinkConfig{Clock: clock}},
		{name: "nil clock", config: log.SinkConfig{Root: t.TempDir()}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := log.NewFileSink(testCase.config)
			requireClass(t, err, fault.InvalidInput)
		})
	}
}

// TestFileSinkDeduplicatesSplitChunksOnRawFingerprint proves that retry
// identity is the request bytes, not the redacted bytes: a chunk whose stored
// form was shortened by a held-back secret prefix is still recognized as the
// same chunk, while a genuinely different payload for that identity is
// refused.
func TestFileSinkDeduplicatesSplitChunksOnRawFingerprint(t *testing.T) {
	clock := &fakeClock{now: testNow}
	config := defaultSinkConfig(t, clock)
	config.Redact = []string{"supersecret"}
	sink := newFileSink(t, config)
	scope := testScope("tenant-a", "project-a")
	ctx := context.Background()

	first := testChunk("exec-1", "action-1", log.StreamStdout, 10, 1, "prefix super")
	second := testChunk("exec-1", "action-1", log.StreamStdout, 20, 2, "secret suffix")
	if _, err := sink.Append(ctx, scope, first); err != nil {
		t.Fatalf("Append first half: %v", err)
	}
	if _, err := sink.Append(ctx, scope, second); err != nil {
		t.Fatalf("Append second half: %v", err)
	}
	if _, err := sink.Append(ctx, scope, first); err != nil {
		t.Fatalf("retried first split chunk: %v", err)
	}
	if _, err := sink.Append(ctx, scope, second); err != nil {
		t.Fatalf("retried second split chunk: %v", err)
	}

	request := testReadRequest(scope, "exec-1", "action-1", log.StreamStdout, 0, 10)
	page, err := sink.Read(ctx, request)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(page.Chunks) != 2 {
		t.Fatalf("retained chunk count = %d; want 2 without duplicates", len(page.Chunks))
	}
	text := concatChunks(page.Chunks)
	if strings.Contains(text, "supersecret") {
		t.Fatalf("retained log contains the split configured value: %q", text)
	}

	changed := testChunk("exec-1", "action-1", log.StreamStdout, 10, 1, "prefix superX")
	if _, err := sink.Append(ctx, scope, changed); err == nil {
		t.Fatal("a changed payload for a retained chunk identity was accepted")
	} else {
		requireClass(t, err, fault.ChangedIdempotencyContent)
	}
	page, err = sink.Read(ctx, request)
	if err != nil {
		t.Fatalf("Read after refused append: %v", err)
	}
	if len(page.Chunks) != 2 {
		t.Fatalf("refused append changed the retained chunk count to %d; want 2", len(page.Chunks))
	}
}

func TestFileSinkPersistSets0600Permissions(t *testing.T) {
	clock := &fakeClock{now: testNow}
	root := t.TempDir()
	config := defaultSinkConfig(t, clock)
	config.Root = root
	sink := newFileSink(t, config)
	scope := testScope("tenant-a", "project-a")

	if _, err := sink.Append(context.Background(), scope, testChunk("exec-1", "action-1", log.StreamStdout, 0, 1, "chunk")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	info, err := os.Stat(filepath.Join(root, "retained-log.json"))
	if err != nil {
		t.Fatalf("Stat persisted state: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("persisted state mode = %04o; want 0600", got)
	}
}

// TestFileSinkAppendRollsBackStateOnPersistFailure proves that a failed disk
// commit leaves in-memory state untouched: a subsequent retry of the exact same
// chunk is not falsely deduplicated without persisting, and split-chunk
// redaction pending bytes are preserved for the retry.
func TestFileSinkAppendRollsBackStateOnPersistFailure(t *testing.T) {
	clock := &fakeClock{now: testNow}
	root := t.TempDir()
	config := defaultSinkConfig(t, clock)
	config.Root = root
	config.Redact = []string{"supersecret"}
	sink := newFileSink(t, config)
	scope := testScope("tenant-a", "project-a")
	ctx := context.Background()
	statePath := filepath.Join(root, "retained-log.json")

	first := testChunk("exec-1", "action-1", log.StreamStdout, 10, 1, "prefix super")
	second := testChunk("exec-1", "action-1", log.StreamStdout, 20, 2, "secret suffix")
	third := testChunk("exec-1", "action-1", log.StreamStdout, 30, 3, " done")

	if _, err := sink.Append(ctx, scope, first); err != nil {
		t.Fatalf("Append first: %v", err)
	}
	saved, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read saved state: %v", err)
	}

	// Replace the state file with a directory so os.Rename in persist() fails
	// deterministically regardless of effective UID.
	if err := os.Remove(statePath); err != nil {
		t.Fatalf("remove state file: %v", err)
	}
	if err := os.Mkdir(statePath, 0o700); err != nil {
		t.Fatalf("mkdir obstacle: %v", err)
	}

	_, err = sink.Append(ctx, scope, second)
	requireClass(t, err, fault.InternalFailure)

	// Also verify a brand-new stream rolls back cleanly when persist fails.
	otherScope := testScope("tenant-b", "project-b")
	freshChunk := testChunk("exec-2", "action-1", log.StreamStdout, 0, 1, "fresh")
	_, err = sink.Append(ctx, otherScope, freshChunk)
	requireClass(t, err, fault.InternalFailure)

	// Restore the persisted state file.
	if err := os.Remove(statePath); err != nil {
		t.Fatalf("remove obstacle dir: %v", err)
	}
	if err := os.WriteFile(statePath, saved, 0o600); err != nil {
		t.Fatalf("restore state file: %v", err)
	}

	// Before retrying, in-memory Read must not serve the unpersisted second chunk.
	page, err := sink.Read(ctx, testReadRequest(scope, "exec-1", "action-1", log.StreamStdout, 0, 10))
	if err != nil {
		t.Fatalf("Read after rollback: %v", err)
	}
	if len(page.Chunks) != 1 {
		t.Fatalf("in-memory chunks after failed persist = %d; want 1", len(page.Chunks))
	}

	// Retrying the exact same chunks must now persist them to disk.
	if _, err := sink.Append(ctx, scope, second); err != nil {
		t.Fatalf("retry Append second: %v", err)
	}
	if _, err := sink.Append(ctx, scope, third); err != nil {
		t.Fatalf("Append third: %v", err)
	}
	if _, err := sink.Append(ctx, otherScope, freshChunk); err != nil {
		t.Fatalf("retry Append fresh stream: %v", err)
	}

	reopened := newFileSink(t, config)
	reopenedPage, err := reopened.Read(ctx, testReadRequest(scope, "exec-1", "action-1", log.StreamStdout, 0, 10))
	if err != nil {
		t.Fatalf("Read reopened: %v", err)
	}
	if len(reopenedPage.Chunks) != 3 {
		t.Fatalf("reopened chunk count = %d; want 3 persisted chunks", len(reopenedPage.Chunks))
	}
	text := concatChunks(reopenedPage.Chunks)
	if strings.Contains(text, "supersecret") {
		t.Fatalf("reopened log leaked split secret: %q", text)
	}
	if !strings.Contains(text, "[REDACTED]") {
		t.Fatalf("reopened log lost redaction marker: %q", text)
	}

	freshPage, err := reopened.Read(ctx, testReadRequest(otherScope, "exec-2", "action-1", log.StreamStdout, 0, 10))
	if err != nil {
		t.Fatalf("Read reopened fresh stream: %v", err)
	}
	if len(freshPage.Chunks) != 1 || string(freshPage.Chunks[0].Data) != "fresh" {
		t.Fatalf("reopened fresh stream = %+v; want [fresh]", freshPage.Chunks)
	}
}

// TestFileSinkReadRollsBackEvictionOnPersistFailure proves that if persisting
// retention eviction fails during Read, the in-memory stream is restored so a
// subsequent Read still observes and persists the eviction.
func TestFileSinkReadRollsBackEvictionOnPersistFailure(t *testing.T) {
	clock := &fakeClock{now: testNow}
	root := t.TempDir()
	config := defaultSinkConfig(t, clock)
	config.Root = root
	sink := newFileSink(t, config)
	scope := testScope("tenant-a", "project-a")
	ctx := context.Background()
	statePath := filepath.Join(root, "retained-log.json")

	if _, err := sink.Append(ctx, scope, testChunk("exec-1", "action-1", log.StreamStdout, 10, 1, "expiring")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	saved, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read saved state: %v", err)
	}

	clock.Advance(log.DefaultRetention().RetainFor)
	if err := os.Remove(statePath); err != nil {
		t.Fatalf("remove state file: %v", err)
	}
	if err := os.Mkdir(statePath, 0o700); err != nil {
		t.Fatalf("mkdir obstacle: %v", err)
	}

	_, err = sink.Read(ctx, testReadRequest(scope, "exec-1", "action-1", log.StreamStdout, 0, 10))
	requireClass(t, err, fault.InternalFailure)

	if err := os.Remove(statePath); err != nil {
		t.Fatalf("remove obstacle dir: %v", err)
	}
	if err := os.WriteFile(statePath, saved, 0o600); err != nil {
		t.Fatalf("restore state file: %v", err)
	}

	page, err := sink.Read(ctx, testReadRequest(scope, "exec-1", "action-1", log.StreamStdout, 0, 10))
	if err != nil {
		t.Fatalf("retry Read after obstacle cleared: %v", err)
	}
	if len(page.Chunks) != 0 || !page.Truncated {
		t.Fatalf("retry Read page = %+v; want empty and truncated", page)
	}

	// Reopen with an earlier clock to prove the eviction itself was persisted.
	earlyClock := &fakeClock{now: testNow}
	earlyConfig := config
	earlyConfig.Clock = earlyClock
	reopened := newFileSink(t, earlyConfig)
	reopenedPage, err := reopened.Read(ctx, testReadRequest(scope, "exec-1", "action-1", log.StreamStdout, 0, 10))
	if err != nil {
		t.Fatalf("Read reopened: %v", err)
	}
	if len(reopenedPage.Chunks) != 0 || !reopenedPage.Truncated {
		t.Fatalf("reopened page = %+v; want persisted eviction", reopenedPage)
	}
}
