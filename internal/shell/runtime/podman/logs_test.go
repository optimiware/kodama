// Log behavior: bounded ordered stdout/stderr chunks, a shared continuation
// token that never skips the quieter stream, redaction of configured values and
// fail-closed reads without a redaction source. Logs carry no lifecycle
// authority.
package podman

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/workspace"
)

func logsRequest(fx *fixture, after int64, limit int) engine.LogsRequest {
	return engine.LogsRequest{
		Identity:    fx.request.Identity,
		AfterOffset: after,
		Limit:       limit,
	}
}

func logs(t *testing.T, fx *fixture, after int64, limit int) engine.LogBatch {
	t.Helper()
	batch, err := fx.adapter.Logs(context.Background(), logsRequest(fx, after, limit))
	if err != nil {
		t.Fatalf("Logs returned error: %v", err)
	}
	return batch
}

func packCursor(stdout, stderr int64) int64 {
	return stdout<<32 | stderr
}

func TestLogsAreOrderedBoundedAndRedacted(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	fx.seedRunning(t)
	stdout := []byte("out-1\ns3cr3t\n")
	stderr := []byte("err-1\ntok-abc\n")
	fx.libpod.setLogs(fx.containerName, engine.LogStdout, stdout)
	fx.libpod.setLogs(fx.containerName, engine.LogStderr, stderr)

	batch := logs(t, fx, 0, 64)
	if len(batch.Chunks) != 2 {
		t.Fatalf("chunks = %d, want stdout and stderr", len(batch.Chunks))
	}
	if batch.Chunks[0].Stream != engine.LogStdout || batch.Chunks[0].Sequence != 1 {
		t.Fatalf("chunk[0] = %+v, want stdout sequence 1", batch.Chunks[0])
	}
	if batch.Chunks[1].Stream != engine.LogStderr || batch.Chunks[1].Sequence != 2 {
		t.Fatalf("chunk[1] = %+v, want stderr sequence 2", batch.Chunks[1])
	}
	if batch.Chunks[0].Offset != 0 || batch.Chunks[1].Offset != 0 {
		t.Fatalf("chunk offsets = %d/%d, want 0/0", batch.Chunks[0].Offset, batch.Chunks[1].Offset)
	}
	if len(batch.Chunks[0].Data) > 64 || len(batch.Chunks[1].Data) > 64 {
		t.Fatalf("chunks exceed the bounded limit: %d/%d", len(batch.Chunks[0].Data), len(batch.Chunks[1].Data))
	}
	if !strings.Contains(string(batch.Chunks[0].Data), Redacted) {
		t.Fatalf("stdout chunk was not redacted: %q", batch.Chunks[0].Data)
	}
	if !strings.Contains(string(batch.Chunks[1].Data), Redacted) {
		t.Fatalf("stderr chunk was not redacted: %q", batch.Chunks[1].Data)
	}
	for _, chunk := range batch.Chunks {
		for _, secret := range []string{"s3cr3t", "tok-abc"} {
			if strings.Contains(string(chunk.Data), secret) {
				t.Fatalf("log chunk leaked %q: %q", secret, chunk.Data)
			}
		}
	}
	if want := packCursor(int64(len(stdout)), int64(len(stderr))); batch.NextOffset != want {
		t.Fatalf("NextOffset = %d, want %d", batch.NextOffset, want)
	}

	for _, operation := range []string{"CreateContainer", "StartContainer", "StopContainer", "RemoveContainer"} {
		if calls := fx.libpod.callsFor(operation); calls != 0 {
			t.Fatalf("Logs called %s %d times", operation, calls)
		}
	}
}

func TestLogsContinuationDoesNotSkipQuieterStream(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	fx.seedRunning(t)
	stdout := []byte("0123456789abcdefghij")
	stderr := []byte("err")
	fx.libpod.setLogs(fx.containerName, engine.LogStdout, stdout)
	fx.libpod.setLogs(fx.containerName, engine.LogStderr, stderr)

	var gotStdout, gotStderr []byte
	cursor := int64(0)
	steps := []struct {
		wantStdout int
		wantStderr int
	}{
		{wantStdout: 8, wantStderr: 3},
		{wantStdout: 8, wantStderr: 0},
		{wantStdout: 4, wantStderr: 0},
		{wantStdout: 0, wantStderr: 0},
	}
	for index, step := range steps {
		batch := logs(t, fx, cursor, 8)
		var stdoutChunk, stderrChunk []byte
		for _, chunk := range batch.Chunks {
			if len(chunk.Data) > 8 {
				t.Fatalf("step %d chunk exceeds limit: %d", index, len(chunk.Data))
			}
			switch chunk.Stream {
			case engine.LogStdout:
				stdoutChunk = append(stdoutChunk, chunk.Data...)
			case engine.LogStderr:
				stderrChunk = append(stderrChunk, chunk.Data...)
			}
		}
		if len(stdoutChunk) != step.wantStdout || len(stderrChunk) != step.wantStderr {
			t.Fatalf("step %d delivered stdout=%d stderr=%d, want %d/%d",
				index, len(stdoutChunk), len(stderrChunk), step.wantStdout, step.wantStderr)
		}
		gotStdout = append(gotStdout, stdoutChunk...)
		gotStderr = append(gotStderr, stderrChunk...)
		cursor = batch.NextOffset
	}
	if string(gotStdout) != string(stdout) {
		t.Fatalf("stdout reassembled = %q, want %q", gotStdout, stdout)
	}
	if string(gotStderr) != string(stderr) {
		t.Fatalf("stderr reassembled = %q, want %q", gotStderr, stderr)
	}
}

func TestLogsRefusesUnboundedOrUnpreparedReads(t *testing.T) {
	t.Run("unbounded limit", func(t *testing.T) {
		fx := newFixture(t)
		fx.prepare(t)
		fx.seedRunning(t)
		fx.libpod.setLogs(fx.containerName, engine.LogStdout, []byte("data"))
		for _, limit := range []int{0, -1, MaxLogBytes + 1} {
			_, err := fx.adapter.Logs(context.Background(), logsRequest(fx, 0, limit))
			assertFaultClass(t, err, fault.InvalidInput)
		}
		if calls := len(fx.libpod.seenLogRequests()); calls != 0 {
			t.Fatalf("unbounded read reached libpod %d times", calls)
		}
	})

	t.Run("negative offset", func(t *testing.T) {
		fx := newFixture(t)
		fx.prepare(t)
		fx.seedRunning(t)
		_, err := fx.adapter.Logs(context.Background(), logsRequest(fx, -1, 16))
		assertFaultClass(t, err, fault.InvalidInput)
		if calls := len(fx.libpod.seenLogRequests()); calls != 0 {
			t.Fatalf("negative offset reached libpod %d times", calls)
		}
	})

	t.Run("absent container", func(t *testing.T) {
		fx := newFixture(t)
		fx.prepare(t)
		_, err := fx.adapter.Logs(context.Background(), logsRequest(fx, 0, 16))
		assertFaultClass(t, err, fault.NotFound)
		if calls := len(fx.libpod.seenLogRequests()); calls != 0 {
			t.Fatalf("absent container reached libpod logs %d times", calls)
		}
	})

	t.Run("removed identity", func(t *testing.T) {
		fx := newFixture(t)
		fx.prepare(t)
		fx.seedExited(t, 0, true)
		if removal := fx.remove(t); !removal.Removed {
			t.Fatalf("removal = %+v, want success", removal)
		}
		before := len(fx.libpod.seenLogRequests())
		_, err := fx.adapter.Logs(context.Background(), logsRequest(fx, 0, 16))
		assertFaultClass(t, err, fault.NotFound)
		if after := len(fx.libpod.seenLogRequests()); after != before {
			t.Fatalf("removed identity reached libpod logs %d times", after-before)
		}
	})
}

func TestLogsFailClosedWithoutRedactionSource(t *testing.T) {
	fx := newFixture(t)
	fx.prepare(t)
	fx.seedRunning(t)
	fx.libpod.setLogs(fx.containerName, engine.LogStdout, []byte("s3cr3t"))
	secretPath := fx.hostPath(filepath.Join(workspace.SecretRoot, "API_KEY"))
	if err := os.Remove(secretPath); err != nil {
		t.Fatalf("removing redaction source: %v", err)
	}

	batch, err := fx.adapter.Logs(context.Background(), logsRequest(fx, 0, 16))
	if err == nil {
		t.Fatal("Logs read output without a redaction source")
	}
	if len(batch.Chunks) != 0 {
		t.Fatalf("Logs emitted %d chunks without a redaction source", len(batch.Chunks))
	}
	if calls := len(fx.libpod.seenLogRequests()); calls != 0 {
		t.Fatalf("Logs read the daemon before proving redaction: %d calls", calls)
	}
}

func collectPodmanLogStreams(t *testing.T, fx *fixture, limit int) (stdout, stderr []byte) {
	t.Helper()
	cursor := int64(0)
	for step := range 1000 {
		batch := logs(t, fx, cursor, limit)
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
	t.Fatal("podman log pagination did not terminate")
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
	fx.prepare(t)
	fx.seedRunning(t)

	fx.libpod.setLogs(fx.containerName, engine.LogStdout, []byte("0123456789s3c-TAIL"))
	fx.libpod.setLogs(fx.containerName, engine.LogStderr, []byte("abcdefghijtok4-END"))

	first := logs(t, fx, 0, 18)
	for _, chunk := range first.Chunks {
		if !chunk.Truncated {
			t.Fatalf("first %s chunk Truncated = false; want true when redaction expansion leaves unconsumed bytes", chunk.Stream)
		}
	}

	gotOut, gotErr := collectPodmanLogStreams(t, fx, 18)
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

func TestLogsSplitSecretAcrossPageBoundary(t *testing.T) {
	fx := newFixture(t)
	fx.request.Secrets = []engine.SecretValue{{Name: "API_KEY", Value: "s3cr3t"}}
	fx.prepare(t)
	fx.seedRunning(t)

	prefix := strings.Repeat("x", 10)
	fx.libpod.setLogs(fx.containerName, engine.LogStdout, []byte(prefix+"s3cr3t-tail"))
	gotOut, _ := collectPodmanLogStreams(t, fx, 13)
	if want := prefix + Redacted + "-tail"; string(gotOut) != want {
		t.Fatalf("stdout = %q; want %q", gotOut, want)
	}
}

func TestLogsCursorOverflowBounds(t *testing.T) {
	if pos, err := nextStreamLogPosition(engine.LogStdout, maxStdoutLogCursorPosition, 0); err != nil || pos != maxStdoutLogCursorPosition {
		t.Fatalf("stdout boundary = (%d, %v); want (%d, nil)", pos, err, maxStdoutLogCursorPosition)
	}
	if _, err := nextStreamLogPosition(engine.LogStdout, maxStdoutLogCursorPosition, 1); err == nil {
		t.Fatal("stdout accepted position past 2^31-1")
	} else {
		assertFaultClass(t, err, fault.InvalidInput)
	}
	if pos, err := nextStreamLogPosition(engine.LogStderr, maxStderrLogCursorPosition, 0); err != nil || pos != maxStderrLogCursorPosition {
		t.Fatalf("stderr boundary = (%d, %v); want (%d, nil)", pos, err, maxStderrLogCursorPosition)
	}
	if _, err := nextStreamLogPosition(engine.LogStderr, maxStderrLogCursorPosition, 1); err == nil {
		t.Fatal("stderr accepted position past 2^32-1")
	} else {
		assertFaultClass(t, err, fault.InvalidInput)
	}
}
