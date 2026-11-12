package log_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/log"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func testPublisherConfig(downstream log.LivePublisher, queued int) log.PublisherConfig {
	return log.PublisherConfig{
		Downstream: downstream,
		Limits: log.Limits{
			MaxChunkBytes:   log.DefaultBounds.MaxChunkBytes,
			MaxReadChunks:   10,
			MaxStoredChunks: 10,
			MaxQueuedChunks: queued,
		},
	}
}

func TestAsyncPublisherDoesNotBlockOnBackpressure(t *testing.T) {
	downstream := newBlockingPublisher()
	publisher, err := log.NewAsyncPublisher(testPublisherConfig(downstream, 2))
	if err != nil {
		t.Fatalf("NewAsyncPublisher: %v", err)
	}
	scope := testScope("tenant-a", "project-a")
	ctx := context.Background()

	if err := publisher.PublishLive(ctx, scope, testChunk("exec-1", "action-1", log.StreamStdout, 0, 1, "first")); err != nil {
		t.Fatalf("PublishLive first: %v", err)
	}
	select {
	case <-downstream.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the live worker never reached the downstream publisher")
	}

	done := make(chan error, 1)
	go func() {
		for index := 1; index <= 20; index++ {
			if err := publisher.PublishLive(ctx, scope, testChunk("exec-1", "action-1", log.StreamStdout, uint64(index), uint64(index+1), "chunk")); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("PublishLive under backpressure: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PublishLive blocked on downstream backpressure")
	}

	downstream.releaseAll()
	if err := publisher.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	calls := downstream.callSnapshot()
	if len(calls) < 1 {
		t.Fatal("the live worker published nothing")
	}
	if len(calls) > 3 {
		t.Fatalf("published %d chunks with a queue bound of 2; want at most 3", len(calls))
	}
}

func TestAsyncPublisherSurvivesDownstreamFailure(t *testing.T) {
	downstream := newBlockingPublisher()
	downstream.failFirst = true
	publisher, err := log.NewAsyncPublisher(testPublisherConfig(downstream, 8))
	if err != nil {
		t.Fatalf("NewAsyncPublisher: %v", err)
	}
	scope := testScope("tenant-a", "project-a")
	ctx := context.Background()

	if err := publisher.PublishLive(ctx, scope, testChunk("exec-1", "action-1", log.StreamStdout, 0, 1, "failed")); err != nil {
		t.Fatalf("PublishLive failed chunk: %v", err)
	}
	if err := publisher.PublishLive(ctx, scope, testChunk("exec-1", "action-1", log.StreamStdout, 10, 2, "succeeded")); err != nil {
		t.Fatalf("PublishLive after failure: %v", err)
	}
	select {
	case <-downstream.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the live worker did not continue after a downstream failure")
	}
	downstream.releaseAll()
	if err := publisher.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if calls := downstream.callSnapshot(); len(calls) != 2 {
		t.Fatalf("downstream call count = %d; want 2 attempts", len(calls))
	}
}

func TestAsyncPublisherCloseJoinsAndRefusesAfter(t *testing.T) {
	downstream := &recordingPublisher{}
	publisher, err := log.NewAsyncPublisher(testPublisherConfig(downstream, 8))
	if err != nil {
		t.Fatalf("NewAsyncPublisher: %v", err)
	}
	scope := testScope("tenant-a", "project-a")
	ctx := context.Background()

	for index := range 3 {
		if err := publisher.PublishLive(ctx, scope, testChunk("exec-1", "action-1", log.StreamStdout, uint64(index*10), uint64(index+1), "chunk")); err != nil {
			t.Fatalf("PublishLive %d: %v", index, err)
		}
	}
	if err := publisher.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if calls := downstream.snapshot(); len(calls) != 3 {
		t.Fatalf("downstream call count after Close = %d; want 3 joined publications", len(calls))
	}
	if err := publisher.PublishLive(ctx, scope, testChunk("exec-1", "action-1", log.StreamStdout, 100, 10, "late")); err == nil {
		t.Fatal("PublishLive after Close returned nil")
	}
	if err := publisher.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestAsyncPublisherValidatesConfigAndChunks(t *testing.T) {
	if _, err := log.NewAsyncPublisher(log.PublisherConfig{}); err == nil {
		t.Fatal("NewAsyncPublisher accepted a nil downstream publisher")
	}
	config := testPublisherConfig(&recordingPublisher{}, 4)
	config.Redact = []string{""}
	if _, err := log.NewAsyncPublisher(config); err == nil {
		t.Fatal("NewAsyncPublisher accepted an empty redaction value")
	}

	downstream := &recordingPublisher{}
	publisher, err := log.NewAsyncPublisher(testPublisherConfig(downstream, 4))
	if err != nil {
		t.Fatalf("NewAsyncPublisher: %v", err)
	}
	scope := testScope("tenant-a", "project-a")
	cases := []struct {
		name  string
		scope tenancy.Scope
		chunk log.Chunk
	}{
		{name: "invalid scope", scope: tenancy.Scope{}, chunk: testChunk("exec-1", "action-1", log.StreamStdout, 0, 1, "data")},
		{name: "invalid stream", scope: scope, chunk: testChunk("exec-1", "action-1", log.Stream("stdin"), 0, 1, "data")},
		{name: "invalid execution", scope: scope, chunk: testChunk("../exec", "action-1", log.StreamStdout, 0, 1, "data")},
		{name: "oversized", scope: scope, chunk: testChunk("exec-1", "action-1", log.StreamStdout, 0, 1, strings.Repeat("x", log.DefaultBounds.MaxChunkBytes+1))},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := publisher.PublishLive(context.Background(), testCase.scope, testCase.chunk)
			requireClass(t, err, fault.InvalidInput)
		})
	}
	if err := publisher.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if calls := downstream.snapshot(); len(calls) != 0 {
		t.Fatalf("downstream received %d invalid chunks; want none", len(calls))
	}
}

func TestAsyncPublisherRedactsConfiguredValue(t *testing.T) {
	downstream := &recordingPublisher{}
	config := testPublisherConfig(downstream, 8)
	config.Redact = []string{"supersecret"}
	publisher, err := log.NewAsyncPublisher(config)
	if err != nil {
		t.Fatalf("NewAsyncPublisher: %v", err)
	}
	scope := testScope("tenant-a", "project-a")
	ctx := context.Background()

	if err := publisher.PublishLive(ctx, scope, testChunk("exec-1", "action-1", log.StreamStdout, 0, 1, "token=supersecret done")); err != nil {
		t.Fatalf("PublishLive: %v", err)
	}
	if err := publisher.PublishLive(ctx, scope, testChunk("exec-1", "action-1", log.StreamStdout, 10, 2, "tail")); err != nil {
		t.Fatalf("PublishLive tail: %v", err)
	}
	if err := publisher.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	calls := downstream.snapshot()
	if len(calls) == 0 {
		t.Fatal("the live publisher published nothing")
	}
	text := concatChunks(calls)
	if strings.Contains(text, "supersecret") {
		t.Fatalf("live chunks contain the configured value: %q", text)
	}
}

// chunksForStream keeps only the downstream chunks of one stream, in call
// order, so a test can prove per-stream redaction state without the other
// stream's bytes.
func chunksForStream(chunks []log.Chunk, stream log.Stream) []log.Chunk {
	var selected []log.Chunk
	for _, chunk := range chunks {
		if chunk.Stream == stream {
			selected = append(selected, chunk)
		}
	}
	return selected
}

// TestAsyncPublisherRedactsValueSplitAcrossLiveChunks proves that a configured
// value split over two live chunks of the same stream is never published: the
// publisher holds the possible-secret suffix of the first chunk and completes
// it with the next chunk before redaction.
func TestAsyncPublisherRedactsValueSplitAcrossLiveChunks(t *testing.T) {
	downstream := &recordingPublisher{}
	config := testPublisherConfig(downstream, 8)
	config.Redact = []string{"supersecret"}
	publisher, err := log.NewAsyncPublisher(config)
	if err != nil {
		t.Fatalf("NewAsyncPublisher: %v", err)
	}
	scope := testScope("tenant-a", "project-a")
	ctx := context.Background()

	if err := publisher.PublishLive(ctx, scope, testChunk("exec-1", "action-1", log.StreamStdout, 10, 1, "prefix super")); err != nil {
		t.Fatalf("PublishLive first half: %v", err)
	}
	if err := publisher.PublishLive(ctx, scope, testChunk("exec-1", "action-1", log.StreamStdout, 20, 2, "secret suffix")); err != nil {
		t.Fatalf("PublishLive second half: %v", err)
	}
	if err := publisher.PublishLive(ctx, scope, testChunk("exec-1", "action-1", log.StreamStdout, 30, 3, "end")); err != nil {
		t.Fatalf("PublishLive tail: %v", err)
	}
	if err := publisher.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	text := concatChunks(downstream.snapshot())
	if strings.Contains(text, "supersecret") {
		t.Fatalf("live chunks contain the split configured value: %q", text)
	}
	if strings.Contains(text, "super") {
		t.Fatalf("live chunks contain a held fragment of the configured value: %q", text)
	}
	if !strings.Contains(text, "prefix ") {
		t.Fatalf("live chunks lost the text before the held suffix: %q", text)
	}
	if !strings.Contains(text, "[REDACTED]") {
		t.Fatalf("live chunks never completed and redacted the split value: %q", text)
	}
}

// TestAsyncPublisherKeepsPendingSuffixPerStream proves that the held suffix is
// scoped to one (scope, execution, action, stream) identity: another stream's
// chunk is published unchanged and never receives it.
func TestAsyncPublisherKeepsPendingSuffixPerStream(t *testing.T) {
	downstream := &recordingPublisher{}
	config := testPublisherConfig(downstream, 8)
	config.Redact = []string{"supersecret"}
	publisher, err := log.NewAsyncPublisher(config)
	if err != nil {
		t.Fatalf("NewAsyncPublisher: %v", err)
	}
	scope := testScope("tenant-a", "project-a")
	ctx := context.Background()

	if err := publisher.PublishLive(ctx, scope, testChunk("exec-1", "action-1", log.StreamStdout, 10, 1, "prefix super")); err != nil {
		t.Fatalf("PublishLive stdout first half: %v", err)
	}
	if err := publisher.PublishLive(ctx, scope, testChunk("exec-1", "action-1", log.StreamStderr, 10, 1, "unrelated")); err != nil {
		t.Fatalf("PublishLive stderr: %v", err)
	}
	if err := publisher.PublishLive(ctx, scope, testChunk("exec-1", "action-1", log.StreamStdout, 20, 2, "secret suffix")); err != nil {
		t.Fatalf("PublishLive stdout second half: %v", err)
	}
	if err := publisher.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	calls := downstream.snapshot()
	stderr := concatChunks(chunksForStream(calls, log.StreamStderr))
	if stderr != "unrelated" {
		t.Fatalf("stderr stream = %q; want its own unmodified chunk", stderr)
	}
	stdout := concatChunks(chunksForStream(calls, log.StreamStdout))
	if strings.Contains(stdout, "supersecret") {
		t.Fatalf("stdout stream contains the split configured value: %q", stdout)
	}
	if !strings.Contains(stdout, "prefix ") {
		t.Fatalf("stdout stream lost the text before the held suffix: %q", stdout)
	}
}

// TestAsyncPublisherCloseDropsHeldSuffixWithoutHanging proves that Close joins
// the worker while a possible-secret suffix is still held, and that the held
// bytes are never published on their own.
func TestAsyncPublisherCloseDropsHeldSuffixWithoutHanging(t *testing.T) {
	downstream := &recordingPublisher{}
	config := testPublisherConfig(downstream, 8)
	config.Redact = []string{"supersecret"}
	publisher, err := log.NewAsyncPublisher(config)
	if err != nil {
		t.Fatalf("NewAsyncPublisher: %v", err)
	}
	scope := testScope("tenant-a", "project-a")
	if err := publisher.PublishLive(context.Background(), scope, testChunk("exec-1", "action-1", log.StreamStdout, 10, 1, "prefix super")); err != nil {
		t.Fatalf("PublishLive: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := publisher.Close(ctx); err != nil {
		t.Fatalf("Close with a held suffix: %v", err)
	}
	text := concatChunks(downstream.snapshot())
	if text != "prefix " {
		t.Fatalf("published text = %q; want only the safe prefix %q", text, "prefix ")
	}
}
