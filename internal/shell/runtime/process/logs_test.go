// Log behavior: ordered identified chunks, bounded reads, the packed per-stream
// cursor, configured redaction, cancellation and no lifecycle authority.
package process

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/kernel/engine"
)

// TestLogsAreOrderedBoundedAndRedacted proves a read returns ordered identified
// chunks for both streams and removes configured credential/secret values.
func TestLogsAreOrderedBoundedAndRedacted(t *testing.T) {
	fx := newFixture(t)
	prepareTestWorkload(t, fx.adapter, fx.request)
	startTestWorkload(t, fx.adapter, fx.request.Identity, fx.request.WorkspaceID)
	group := fx.groupID(t)
	fx.observer.setLogs(group, engine.LogStdout, []byte("hello\nsecret s3cr3t\n"))
	fx.observer.setLogs(group, engine.LogStderr, []byte("credential tok-abc\n"))

	batch, err := fx.adapter.Logs(context.Background(), engine.LogsRequest{
		Identity:    fx.request.Identity,
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
		if index > 0 && chunk.Sequence <= sequence {
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
	if !streams[engine.LogStdout] || !streams[engine.LogStderr] {
		t.Fatalf("streams = %v; want both stdout and stderr", streams)
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

// TestLogsPaginationUsesSharedCursor proves the continuation token packs one
// position per stream and advances only the stream that delivered bytes.
func TestLogsPaginationUsesSharedCursor(t *testing.T) {
	fx := newFixture(t)
	prepareTestWorkload(t, fx.adapter, fx.request)
	startTestWorkload(t, fx.adapter, fx.request.Identity, fx.request.WorkspaceID)
	group := fx.groupID(t)
	stdout := []byte(strings.Repeat("a", 100))
	fx.observer.setLogs(group, engine.LogStdout, stdout)
	fx.observer.setLogs(group, engine.LogStderr, []byte("0123456789"))

	var collected []byte
	offset := int64(0)
	wantStdout := []int64{32, 64, 96, 100}
	const wantStderr int64 = 10
	for step, wantOffset := range wantStdout {
		batch, err := fx.adapter.Logs(context.Background(), engine.LogsRequest{
			Identity:    fx.request.Identity,
			AfterOffset: offset,
			Limit:       32,
		})
		if err != nil {
			t.Fatalf("Logs step %d returned error: %v", step, err)
		}
		for _, chunk := range batch.Chunks {
			if chunk.Stream == engine.LogStdout {
				collected = append(collected, chunk.Data...)
				if want := wantOffset < int64(len(stdout)); chunk.Truncated != want {
					t.Fatalf("step %d stdout Truncated = %v; want %v", step, chunk.Truncated, want)
				}
			}
		}
		wantToken := wantOffset<<32 | wantStderr
		if batch.NextOffset != wantToken {
			t.Fatalf("step %d NextOffset = %d; want stdout %d/stderr %d token %d",
				step, batch.NextOffset, wantOffset, wantStderr, wantToken)
		}
		offset = batch.NextOffset
	}
	if !bytes.Equal(collected, stdout) {
		t.Fatalf("paged stdout = %q; want the original 100 bytes exactly once", collected)
	}
}

// TestLogsCursorDoesNotSkipQuieterStream proves a stream that read fewer bytes
// is never skipped by the faster one.
func TestLogsCursorDoesNotSkipQuieterStream(t *testing.T) {
	fx := newFixture(t)
	prepareTestWorkload(t, fx.adapter, fx.request)
	startTestWorkload(t, fx.adapter, fx.request.Identity, fx.request.WorkspaceID)
	group := fx.groupID(t)
	stdout := []byte(strings.Repeat("o", 50))
	stderrHead := []byte("AAAAA")
	fx.observer.setLogs(group, engine.LogStdout, stdout)
	fx.observer.setLogs(group, engine.LogStderr, stderrHead)

	first, err := fx.adapter.Logs(context.Background(), engine.LogsRequest{
		Identity:    fx.request.Identity,
		AfterOffset: 0,
		Limit:       32,
	})
	if err != nil {
		t.Fatalf("first Logs returned error: %v", err)
	}
	if stdoutChunk, ok := testChunkForStream(first, engine.LogStdout); !ok {
		t.Fatal("first read has no stdout chunk")
	} else if stdoutChunk.Offset != 0 || len(stdoutChunk.Data) != 32 {
		t.Fatalf("first stdout chunk = offset %d length %d; want offset 0 length 32",
			stdoutChunk.Offset, len(stdoutChunk.Data))
	}
	if stderrChunk, ok := testChunkForStream(first, engine.LogStderr); !ok {
		t.Fatal("first read has no stderr chunk")
	} else if stderrChunk.Offset != 0 || string(stderrChunk.Data) != string(stderrHead) {
		t.Fatalf("first stderr chunk = offset %d data %q; want offset 0 data %q",
			stderrChunk.Offset, stderrChunk.Data, stderrHead)
	}

	// stderr grows after stdout has already advanced past its original length.
	fx.observer.setLogs(group, engine.LogStderr, append(append([]byte(nil), stderrHead...), []byte("BBBBB")...))

	second, err := fx.adapter.Logs(context.Background(), engine.LogsRequest{
		Identity:    fx.request.Identity,
		AfterOffset: first.NextOffset,
		Limit:       32,
	})
	if err != nil {
		t.Fatalf("follow-up Logs returned error: %v", err)
	}
	if stdoutChunk, ok := testChunkForStream(second, engine.LogStdout); !ok {
		t.Fatal("follow-up read has no stdout chunk")
	} else if stdoutChunk.Offset != 32 || len(stdoutChunk.Data) != len(stdout)-32 {
		t.Fatalf("follow-up stdout chunk = offset %d length %d; want offset 32 length %d",
			stdoutChunk.Offset, len(stdoutChunk.Data), len(stdout)-32)
	}
	if stderrChunk, ok := testChunkForStream(second, engine.LogStderr); !ok {
		t.Fatal("follow-up read lost the stderr bytes appended after the first read")
	} else if stderrChunk.Offset != int64(len(stderrHead)) || string(stderrChunk.Data) != "BBBBB" {
		t.Fatalf("follow-up stderr chunk = offset %d data %q; want offset %d data %q",
			stderrChunk.Offset, stderrChunk.Data, len(stderrHead), "BBBBB")
	}

	var stderrAll []byte
	for _, chunk := range first.Chunks {
		if chunk.Stream == engine.LogStderr {
			stderrAll = append(stderrAll, chunk.Data...)
		}
	}
	for _, chunk := range second.Chunks {
		if chunk.Stream == engine.LogStderr {
			stderrAll = append(stderrAll, chunk.Data...)
		}
	}
	if string(stderrAll) != "AAAAABBBBB" {
		t.Fatalf("stderr across both reads = %q; want every byte exactly once", stderrAll)
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
	fx := newFixture(t)
	prepareTestWorkload(t, fx.adapter, fx.request)
	startTestWorkload(t, fx.adapter, fx.request.Identity, fx.request.WorkspaceID)

	cases := []struct {
		name    string
		request engine.LogsRequest
	}{
		{name: "zero limit", request: engine.LogsRequest{Identity: fx.request.Identity, Limit: 0}},
		{name: "negative offset", request: engine.LogsRequest{Identity: fx.request.Identity, AfterOffset: -1, Limit: 1024}},
		{name: "oversized limit", request: engine.LogsRequest{Identity: fx.request.Identity, Limit: MaxLogBytes + 1}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			readsBefore := fx.observer.callsFor("Logs")
			_, err := fx.adapter.Logs(context.Background(), testCase.request)
			assertRefusedClass(t, err, fault.InvalidInput)
			if fx.observer.callsFor("Logs") != readsBefore {
				t.Fatal("an invalid log request reached the observer")
			}
		})
	}
}

// TestLogsCancellationHonoredWithoutLifecycleAuthority proves a cancelled read
// emits nothing and that Logs never starts, signals or removes a process.
func TestLogsCancellationHonoredWithoutLifecycleAuthority(t *testing.T) {
	fx := newFixture(t)
	prepareTestWorkload(t, fx.adapter, fx.request)
	startTestWorkload(t, fx.adapter, fx.request.Identity, fx.request.WorkspaceID)
	group := fx.groupID(t)
	fx.observer.setLogs(group, engine.LogStdout, []byte("hello\n"))
	gate := fx.observer.newGate("Logs")

	ctx, cancel := context.WithCancel(context.Background())
	type outcome struct {
		batch engine.LogBatch
		err   error
	}
	results := make(chan outcome, 1)
	go func() {
		batch, err := fx.adapter.Logs(ctx, engine.LogsRequest{
			Identity:    fx.request.Identity,
			AfterOffset: 0,
			Limit:       1024,
		})
		results <- outcome{batch: batch, err: err}
	}()

	waitFor(t, gate.entered, "the log read barrier")
	cancel()
	close(gate.release)

	got := <-results
	if got.err == nil {
		t.Fatal("cancelled Logs hid the cancellation")
	}
	if len(got.batch.Chunks) != 0 {
		t.Fatalf("cancelled Logs emitted %d chunks", len(got.batch.Chunks))
	}
	if fx.executor.spawnCount() != 1 || fx.executor.spawnRequestCount() != 1 {
		t.Fatal("Logs spawned a process")
	}
	if fx.executor.signalCount() != 0 {
		t.Fatal("Logs signalled a process group")
	}
	if fx.executor.removalCount() != 0 {
		t.Fatal("Logs removed a process group")
	}
}

// TestLogsFailClosedWithoutRedactionSource proves output is never emitted when
// the configured values cannot be redacted, including after reconstruction.
func TestLogsFailClosedWithoutRedactionSource(t *testing.T) {
	t.Run("unprepared identity", func(t *testing.T) {
		fx := newFixture(t)
		batch, err := fx.adapter.Logs(context.Background(), engine.LogsRequest{
			Identity:    fx.request.Identity,
			AfterOffset: 0,
			Limit:       1024,
		})
		if err == nil {
			t.Fatal("Logs succeeded without a prepared redaction source")
		}
		if len(batch.Chunks) != 0 {
			t.Fatal("Logs emitted chunks without a prepared redaction source")
		}
	})

	t.Run("missing credential file", func(t *testing.T) {
		fx := newFixture(t)
		prepareTestWorkload(t, fx.adapter, fx.request)
		startTestWorkload(t, fx.adapter, fx.request.Identity, fx.request.WorkspaceID)
		fx.observer.setLogs(fx.groupID(t), engine.LogStdout, []byte("credential tok-abc\n"))
		reconstructed := newTestAdapter(t, fx.config())
		if err := os.Remove(fx.credentialPath(t)); err != nil {
			t.Fatalf("removing credential file: %v", err)
		}

		batch, err := reconstructed.Logs(context.Background(), engine.LogsRequest{
			Identity:    fx.request.Identity,
			AfterOffset: 0,
			Limit:       1024,
		})
		if err == nil {
			t.Fatal("Logs emitted output without a redaction source")
		}
		for _, chunk := range batch.Chunks {
			if strings.Contains(string(chunk.Data), "tok-abc") {
				t.Fatalf("Logs leaked the credential without its redaction source: %q", chunk.Data)
			}
		}
	})
}

func collectProcessLogStreams(t *testing.T, adapter *Adapter, identity engine.EffectIdentity, limit int) (stdout, stderr []byte) {
	t.Helper()
	cursor := int64(0)
	for step := range 1000 {
		batch, err := adapter.Logs(context.Background(), engine.LogsRequest{
			Identity:    identity,
			AfterOffset: cursor,
			Limit:       limit,
		})
		if err != nil {
			t.Fatalf("step %d Logs returned error: %v", step, err)
		}
		for _, chunk := range batch.Chunks {
			if len(chunk.Data) > limit {
				t.Fatalf("step %d chunk (%s) length = %d; exceeds limit %d", step, chunk.Stream, len(chunk.Data), limit)
			}
			switch chunk.Stream {
			case engine.LogStdout:
				stdout = append(stdout, chunk.Data...)
			case engine.LogStderr:
				stderr = append(stderr, chunk.Data...)
			}
		}
		if batch.NextOffset == cursor {
			return stdout, stderr
		}
		cursor = batch.NextOffset
	}
	t.Fatal("process log pagination did not terminate")
	return nil, nil
}

// TestLogsShortSecretExpansionNeverDropsTrailingBytes proves a short secret
// (< 10 bytes) whose [REDACTED] expansion crosses request.Limit advances the
// cursor only over consumed raw bytes instead of clipping and dropping the
// trailing log bytes, and sets Truncated when unconsumed bytes remain.
func TestLogsShortSecretExpansionNeverDropsTrailingBytes(t *testing.T) {
	fx := newFixture(t)
	fx.request.Secrets = []engine.SecretValue{{Name: "API_KEY", Value: "s3c"}}
	fx.request.Credential = "tok4"
	prepareTestWorkload(t, fx.adapter, fx.request)
	startTestWorkload(t, fx.adapter, fx.request.Identity, fx.request.WorkspaceID)
	group := fx.groupID(t)

	// 18 raw bytes fit inside Limit=18 (so page.Truncated is false on the first
	// read), while redacting "s3c" (3 -> 10 bytes) expands the output to 25 bytes.
	fx.observer.setLogs(group, engine.LogStdout, []byte("0123456789s3c-TAIL"))
	fx.observer.setLogs(group, engine.LogStderr, []byte("abcdefghijtok4-END"))

	first, err := fx.adapter.Logs(context.Background(), engine.LogsRequest{
		Identity:    fx.request.Identity,
		AfterOffset: 0,
		Limit:       18,
	})
	if err != nil {
		t.Fatalf("first Logs returned error: %v", err)
	}
	for _, chunk := range first.Chunks {
		if !chunk.Truncated {
			t.Fatalf("first %s chunk Truncated = false; want true when redaction expansion leaves unconsumed bytes", chunk.Stream)
		}
	}

	gotOut, gotErr := collectProcessLogStreams(t, fx.adapter, fx.request.Identity, 18)
	if want := "0123456789" + Redacted + "-TAIL"; string(gotOut) != want {
		t.Fatalf("stdout = %q; want %q", gotOut, want)
	}
	if want := "abcdefghij" + Redacted + "-END"; string(gotErr) != want {
		t.Fatalf("stderr = %q; want %q", gotErr, want)
	}
	for _, secret := range []string{"s3c", "tok4"} {
		if strings.Contains(string(gotOut), secret) || strings.Contains(string(gotErr), secret) {
			t.Fatalf("output leaked %q: stdout=%q stderr=%q", secret, gotOut, gotErr)
		}
	}
}

type cappedEOFObserver struct {
	*fakeObserver
	stdout []byte
}

func (o *cappedEOFObserver) Logs(ctx context.Context, request GroupLogsRequest) (GroupLogPage, error) {
	if request.Stream != engine.LogStdout {
		return o.fakeObserver.Logs(ctx, request)
	}
	start := min(max(request.Offset, 0), int64(len(o.stdout)))
	end := max(min(start+int64(request.Limit), int64(len(o.stdout))), start)
	return GroupLogPage{
		Data:      append([]byte(nil), o.stdout[start:end]...),
		Truncated: true, // Models a stream-capped capture file at physical EOF.
	}, nil
}

// TestLogsFlushesTrailingPartialPrefixOnStreamCappedEOF proves a stream-capped
// file at physical EOF (Truncated=true with len(page.Data) < Limit) flushes a
// trailing partial secret prefix instead of holding it forever.
func TestLogsFlushesTrailingPartialPrefixOnStreamCappedEOF(t *testing.T) {
	fx := newFixture(t)
	capped := &cappedEOFObserver{
		fakeObserver: fx.observer,
		stdout:       []byte("hello-s3c"),
	}
	cfg := fx.config()
	cfg.Observer = capped
	adapter := newTestAdapter(t, cfg)

	fx.request.Secrets = []engine.SecretValue{{Name: "API_KEY", Value: "s3cr3t"}}
	prepareTestWorkload(t, adapter, fx.request)
	startTestWorkload(t, adapter, fx.request.Identity, fx.request.WorkspaceID)

	gotOut, _ := collectProcessLogStreams(t, adapter, fx.request.Identity, 64)
	if string(gotOut) != "hello-s3c" {
		t.Fatalf("capped stdout = %q; want %q", gotOut, "hello-s3c")
	}
}

func TestLogsCursorOverflowBounds(t *testing.T) {
	if pos, err := nextStreamLogPosition(engine.LogStdout, maxStdoutLogCursorPosition, 0); err != nil || pos != maxStdoutLogCursorPosition {
		t.Fatalf("stdout boundary = (%d, %v); want (%d, nil)", pos, err, maxStdoutLogCursorPosition)
	}
	if _, err := nextStreamLogPosition(engine.LogStdout, maxStdoutLogCursorPosition, 1); err == nil {
		t.Fatal("stdout accepted position past 2^31-1")
	} else {
		assertRefusedClass(t, err, fault.InvalidInput)
	}
	if pos, err := nextStreamLogPosition(engine.LogStderr, maxStderrLogCursorPosition, 0); err != nil || pos != maxStderrLogCursorPosition {
		t.Fatalf("stderr boundary = (%d, %v); want (%d, nil)", pos, err, maxStderrLogCursorPosition)
	}
	if _, err := nextStreamLogPosition(engine.LogStderr, maxStderrLogCursorPosition, 1); err == nil {
		t.Fatal("stderr accepted position past 2^32-1")
	} else {
		assertRefusedClass(t, err, fault.InvalidInput)
	}
}
