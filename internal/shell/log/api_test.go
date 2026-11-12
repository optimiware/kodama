// Package log_test pins the retained/live log service API.
//
// Package kodama/internal/shell/log provides:
//
//	// Clock is the injected authoritative time source for retention.
//	type Clock interface {
//		Now() time.Time
//	}
//
//	// Limits bounds one retained sink and one live publisher.
//	type Limits struct {
//		MaxChunkBytes   int
//		MaxReadChunks   int
//		MaxStoredChunks int
//		MaxQueuedChunks int
//	}
//
//	func DefaultLimits() Limits
//
//	// SinkConfig configures one file-backed retained sink. Root is required.
//	// A zero Retention or Limits uses the declared default. Redact lists
//	// configured sensitive values that must never be stored, including values
//	// split across chunk boundaries.
//	type SinkConfig struct {
//		Root      string
//		Clock     Clock
//		Retention Retention
//		Limits    Limits
//		Redact    []string
//	}
//
//	func NewFileSink(SinkConfig) (*FileSink, error)
//
//	// FileSink appends and reads bounded retained chunks under Root. It is
//	// safe for concurrent use.
//	type FileSink struct{ /* ... */ }
//
//	func (*FileSink) Append(context.Context, tenancy.Scope, Chunk) (AppendResult, error)
//	func (*FileSink) Read(context.Context, ReadRequest) (ReadResult, error)
//
//	// PublisherConfig configures one bounded asynchronous live publisher.
//	// Downstream is required; PublishLive must never wait for it.
//	type PublisherConfig struct {
//		Downstream LivePublisher
//		Limits     Limits
//		Redact     []string
//	}
//
//	func NewAsyncPublisher(PublisherConfig) (*AsyncPublisher, error)
//
//	type AsyncPublisher struct{ /* ... */ }
//
//	func (*AsyncPublisher) PublishLive(context.Context, tenancy.Scope, Chunk) error
//	func (*AsyncPublisher) Close(context.Context) error
//
// Behavior these tests rely on:
//
//   - Append refuses a chunk larger than the effective MaxChunkBytes (32 KiB
//     by default) and refuses a scope-less or malformed chunk. Read refuses a
//     scope-less or malformed request and a limit above MaxReadChunks.
//   - A chunk is stored under the supplied ownership scope only. A read in
//     another scope never returns it. An equal (execution, action, stream,
//     offset, sequence) append is deduplicated; the same identity with
//     different stored bytes is refused as changed idempotency content.
//   - Reads are ordered by offset then sequence, return at most Limit chunks
//     after AfterOffset and set NextOffset to the last returned offset. A
//     chunk is expired at or after its stored time plus the retention period;
//     expiry and storage-bound eviction set Truncated when the caller's
//     AfterOffset is older than the newest dropped chunk.
//   - Storage is bounded by MaxStoredChunks per stream; the oldest chunks are
//     dropped. The sink is file-backed: a new sink over the same Root reads
//     the persisted chunks.
//   - Configured redaction removes every configured value before storage,
//     including a value split across two appended chunks. A sink without
//     redaction stores the exact chunk bytes.
//   - AsyncPublisher.PublishLive enqueues without waiting for Downstream and
//     returns nil even when Downstream is slow or fails. The queue is bounded
//     by MaxQueuedChunks. Close stops accepting, joins the worker and returns
//     after no further publication is in flight. A publish after Close is
//     refused.
package log_test

import (
	"github.com/optimiware/kodama/internal/shell/log"
)

// Compile-time pins for the declarations the behavior tests consume.
var (
	_ log.RetainedSink  = (*log.FileSink)(nil)
	_ log.LivePublisher = (*log.AsyncPublisher)(nil)
	_                   = log.NewFileSink
	_                   = log.NewAsyncPublisher
	_ log.Limits
	_ = log.DefaultLimits
)
