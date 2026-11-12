package testkit

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/kernel/engine"
)

func TestLogsAreOrderedAndBounded(t *testing.T) {
	runtime, identity, _ := testRuntimeWithStartedWorkload(t)

	if err := runtime.AppendLog(identity.WorkloadID, engine.LogStdout, []byte("hello")); err != nil {
		t.Fatalf("AppendLog(stdout) error = %v", err)
	}
	if err := runtime.AppendLog(identity.WorkloadID, engine.LogStderr, []byte("oops")); err != nil {
		t.Fatalf("AppendLog(stderr) error = %v", err)
	}

	batch, err := runtime.Logs(context.Background(), engine.LogsRequest{
		Identity:    identity,
		AfterOffset: 0,
		Limit:       10,
	})
	if err != nil {
		t.Fatalf("Logs() error = %v", err)
	}
	if len(batch.Chunks) != 2 {
		t.Fatalf("chunk count = %d, want 2", len(batch.Chunks))
	}
	if batch.Chunks[0].Stream != engine.LogStdout || string(batch.Chunks[0].Data) != "hello" {
		t.Fatalf("first chunk = %q %q, want stdout hello", batch.Chunks[0].Stream, batch.Chunks[0].Data)
	}
	if batch.Chunks[1].Stream != engine.LogStderr || string(batch.Chunks[1].Data) != "oops" {
		t.Fatalf("second chunk = %q %q, want stderr oops", batch.Chunks[1].Stream, batch.Chunks[1].Data)
	}
	if batch.Chunks[0].Offset != 0 || batch.Chunks[1].Offset != 5 {
		t.Fatalf("offsets = %d/%d, want 0/5", batch.Chunks[0].Offset, batch.Chunks[1].Offset)
	}
	if batch.Chunks[0].Sequence >= batch.Chunks[1].Sequence {
		t.Fatalf("sequences = %d/%d, want strictly increasing", batch.Chunks[0].Sequence, batch.Chunks[1].Sequence)
	}
	if batch.NextOffset != 9 {
		t.Fatalf("NextOffset = %d, want 9", batch.NextOffset)
	}

	empty, err := runtime.Logs(context.Background(), engine.LogsRequest{
		Identity:    identity,
		AfterOffset: batch.NextOffset,
		Limit:       10,
	})
	if err != nil {
		t.Fatalf("Logs(after end) error = %v", err)
	}
	if len(empty.Chunks) != 0 || empty.NextOffset != batch.NextOffset {
		t.Fatalf("Logs(after end) = %d chunks, next %d; want none, next %d", len(empty.Chunks), empty.NextOffset, batch.NextOffset)
	}
}

func TestLogsSplitsBoundedChunks(t *testing.T) {
	runtime, identity, _ := testRuntimeWithStartedWorkload(t)

	payload := bytes.Repeat([]byte("x"), MaxLogChunkBytes*2+7)
	if err := runtime.AppendLog(identity.WorkloadID, engine.LogStdout, payload); err != nil {
		t.Fatalf("AppendLog(large) error = %v", err)
	}

	batch, err := runtime.Logs(context.Background(), engine.LogsRequest{
		Identity:    identity,
		AfterOffset: 0,
		Limit:       100,
	})
	if err != nil {
		t.Fatalf("Logs() error = %v", err)
	}
	if len(batch.Chunks) < 2 {
		t.Fatalf("large append produced %d chunks, want at least 2", len(batch.Chunks))
	}

	var joined []byte
	var offset int64
	for index, chunk := range batch.Chunks {
		if len(chunk.Data) > MaxLogChunkBytes {
			t.Fatalf("chunk %d length = %d, want <= %d", index, len(chunk.Data), MaxLogChunkBytes)
		}
		if chunk.Offset != offset {
			t.Fatalf("chunk %d offset = %d, want %d", index, chunk.Offset, offset)
		}
		offset += int64(len(chunk.Data))
		joined = append(joined, chunk.Data...)
	}
	if !bytes.Equal(joined, payload) {
		t.Fatalf("joined log length = %d, want %d", len(joined), len(payload))
	}
	if batch.NextOffset != int64(len(payload)) {
		t.Fatalf("NextOffset = %d, want %d", batch.NextOffset, len(payload))
	}
}

func TestLogsRespectLimitAndNextOffset(t *testing.T) {
	runtime, identity, _ := testRuntimeWithStartedWorkload(t)

	for _, line := range []string{"a", "b", "c"} {
		if err := runtime.AppendLog(identity.WorkloadID, engine.LogStdout, []byte(line)); err != nil {
			t.Fatalf("AppendLog(%q) error = %v", line, err)
		}
	}

	first, err := runtime.Logs(context.Background(), engine.LogsRequest{
		Identity:    identity,
		AfterOffset: 0,
		Limit:       2,
	})
	if err != nil {
		t.Fatalf("Logs(limit 2) error = %v", err)
	}
	if len(first.Chunks) != 2 {
		t.Fatalf("limited chunk count = %d, want 2", len(first.Chunks))
	}
	if first.NextOffset != 2 {
		t.Fatalf("limited NextOffset = %d, want 2", first.NextOffset)
	}

	second, err := runtime.Logs(context.Background(), engine.LogsRequest{
		Identity:    identity,
		AfterOffset: first.NextOffset,
		Limit:       2,
	})
	if err != nil {
		t.Fatalf("Logs(continuation) error = %v", err)
	}
	if len(second.Chunks) != 1 || string(second.Chunks[0].Data) != "c" {
		t.Fatalf("continuation = %d chunks, data %v; want one c", len(second.Chunks), second.Chunks)
	}
	if second.NextOffset != 3 {
		t.Fatalf("continuation NextOffset = %d, want 3", second.NextOffset)
	}
}

func TestLogsReturnDefensiveCopies(t *testing.T) {
	runtime, identity, _ := testRuntimeWithStartedWorkload(t)
	if err := runtime.AppendLog(identity.WorkloadID, engine.LogStdout, []byte("hello")); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}

	batch, err := runtime.Logs(context.Background(), engine.LogsRequest{
		Identity:    identity,
		AfterOffset: 0,
		Limit:       1,
	})
	if err != nil {
		t.Fatalf("Logs() error = %v", err)
	}
	batch.Chunks[0].Data[0] = 'X'

	again, err := runtime.Logs(context.Background(), engine.LogsRequest{
		Identity:    identity,
		AfterOffset: 0,
		Limit:       1,
	})
	if err != nil {
		t.Fatalf("second Logs() error = %v", err)
	}
	if string(again.Chunks[0].Data) != "hello" {
		t.Fatalf("log data after caller mutation = %q, want hello", again.Chunks[0].Data)
	}
}

func TestLogsUnknownWorkloadIsNotFound(t *testing.T) {
	runtime := NewRuntime()
	_, err := runtime.Logs(context.Background(), engine.LogsRequest{
		Identity: testIdentity("exec-1", "act-1", "wl-missing"),
		Limit:    1,
	})
	testClass(t, err, fault.NotFound)
}

func TestLogsInvalidStreamIsRejected(t *testing.T) {
	runtime, identity, _ := testRuntimeWithStartedWorkload(t)
	err := runtime.AppendLog(identity.WorkloadID, engine.LogStream("stdin"), []byte("x"))
	testClass(t, err, fault.InvalidInput)
}

func TestLogsDoNotChangeLifecycle(t *testing.T) {
	runtime, identity, _ := testRuntimeWithStartedWorkload(t)
	before, _ := runtime.Workload(identity.WorkloadID)

	if err := runtime.AppendLog(identity.WorkloadID, engine.LogStdout, []byte("line")); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}
	if _, err := runtime.Logs(context.Background(), engine.LogsRequest{
		Identity: identity,
		Limit:    10,
	}); err != nil {
		t.Fatalf("Logs() error = %v", err)
	}

	after, _ := runtime.Workload(identity.WorkloadID)
	if after.Started != before.Started || after.StartCount != before.StartCount {
		t.Fatalf("log read changed lifecycle: before %+v after %+v", before, after)
	}
	if after.StopRequests != 0 || after.StopConfirmed || after.Removed {
		t.Fatalf("log read changed stop/cleanup state: %+v", after)
	}
}

func TestLogsConcurrentAppendIsRaceFree(t *testing.T) {
	runtime, identity, _ := testRuntimeWithStartedWorkload(t)

	const workers = 8
	var wait sync.WaitGroup
	wait.Add(workers)
	for worker := range workers {
		go func(worker int) {
			defer wait.Done()
			if err := runtime.AppendLog(identity.WorkloadID, engine.LogStdout, []byte{byte('a' + worker)}); err != nil {
				t.Errorf("AppendLog(%d) error = %v", worker, err)
			}
		}(worker)
	}
	wait.Wait()

	batch, err := runtime.Logs(context.Background(), engine.LogsRequest{
		Identity:    identity,
		AfterOffset: 0,
		Limit:       100,
	})
	if err != nil {
		t.Fatalf("Logs() error = %v", err)
	}
	if len(batch.Chunks) != workers {
		t.Fatalf("chunk count = %d, want %d", len(batch.Chunks), workers)
	}
	for index, chunk := range batch.Chunks {
		if chunk.Offset != int64(index) {
			t.Fatalf("chunk %d offset = %d, want %d", index, chunk.Offset, index)
		}
		if len(chunk.Data) != 1 {
			t.Fatalf("chunk %d data length = %d, want 1", index, len(chunk.Data))
		}
		if index > 0 && chunk.Sequence <= batch.Chunks[index-1].Sequence {
			t.Fatalf("chunk %d sequence = %d, want strictly increasing after %d", index, chunk.Sequence, batch.Chunks[index-1].Sequence)
		}
	}
	if batch.NextOffset != workers {
		t.Fatalf("NextOffset = %d, want %d", batch.NextOffset, workers)
	}
}
