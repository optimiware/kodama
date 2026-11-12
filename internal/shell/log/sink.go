// File-backed retained log sink.
//
// The sink stores bounded scoped chunks under one root. Append deduplicates an
// equal (execution, action, stream, offset, sequence) retry, refuses changed
// bytes for the same identity, applies configured redaction before storage and
// drops the oldest chunks beyond the per-stream bound. Read serves bounded
// ordered pages, expires chunks at or after their stored time plus retention and
// reports truncation when the caller's continuation is older than the newest
// dropped chunk.
package log

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

const sinkStateFileName = "retained-log.json"

// FileSink appends and reads bounded retained chunks under Root. It is safe for
// concurrent use.
type FileSink struct {
	mu        sync.Mutex
	root      string
	path      string
	clock     Clock
	retention Retention
	limits    Limits
	redactor  redactor
	state     sinkState
}

// persistedChunk is one retained chunk as stored on disk. Data is the redacted
// byte sequence, so a configured sensitive value is never part of the durable
// state. RawHash is the SHA-256 fingerprint of the request bytes, which lets a
// retry be recognized by identity without storing or comparing plaintext.
type persistedChunk struct {
	ExecutionID string    `json:"execution_id"`
	ActionID    string    `json:"action_id"`
	Stream      string    `json:"stream"`
	Offset      uint64    `json:"offset"`
	Sequence    uint64    `json:"sequence"`
	StoredAt    time.Time `json:"stored_at"`
	Data        []byte    `json:"data"`
	RawHash     []byte    `json:"raw_sha256"`
}

// persistedStream is one scoped stream with its retained chunks, the newest
// dropped offset and any redaction suffix held for the next append.
type persistedStream struct {
	Chunks         []persistedChunk `json:"chunks"`
	DroppedThrough uint64           `json:"dropped_through"`
	Pending        []byte           `json:"pending,omitempty"`
}

type sinkState struct {
	Streams map[string]persistedStream `json:"streams"`
}

// NewFileSink validates the sink configuration and loads any persisted chunks
// under Root.
func NewFileSink(config SinkConfig) (*FileSink, error) {
	if config.Root == "" {
		return nil, fault.New(fault.InvalidInput, "retained log sink requires a root")
	}
	if config.Clock == nil {
		return nil, fault.New(fault.InvalidInput, "retained log sink requires a clock")
	}
	limits, err := resolveLimits(config.Limits)
	if err != nil {
		return nil, err
	}
	retention, err := resolveRetention(config.Retention)
	if err != nil {
		return nil, err
	}
	redactor, err := newRedactor(config.Redact)
	if err != nil {
		return nil, err
	}
	sink := &FileSink{
		root:      config.Root,
		path:      filepath.Join(config.Root, sinkStateFileName),
		clock:     config.Clock,
		retention: retention,
		limits:    limits,
		redactor:  redactor,
		state:     sinkState{Streams: map[string]persistedStream{}},
	}
	if err := sink.load(); err != nil {
		return nil, err
	}
	return sink, nil
}

// Append stores one bounded chunk in the supplied ownership scope.
func (sink *FileSink) Append(ctx context.Context, scope tenancy.Scope, chunk Chunk) (AppendResult, error) {
	if err := validateScope(scope); err != nil {
		return AppendResult{}, err
	}
	if err := validateChunk(chunk, sink.limits.MaxChunkBytes); err != nil {
		return AppendResult{}, err
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()

	key := streamKey(scope, chunk)
	existing, hadStream := sink.state.Streams[key]
	rawHash := sha256.Sum256(chunk.Data)
	for _, stored := range existing.Chunks {
		if stored.Offset != chunk.Offset || stored.Sequence != chunk.Sequence {
			continue
		}
		// Deduplicate on the request bytes, not on the redacted stored bytes: a
		// retry of a chunk whose stored form was shortened by a held-back secret
		// prefix (or lengthened by a prepended prefix) is still the same chunk.
		if bytes.Equal(stored.RawHash, rawHash[:]) {
			return AppendResult{Offset: stored.Offset, Sequence: stored.Sequence}, nil
		}
		return AppendResult{}, fault.New(fault.ChangedIdempotencyContent, "retained log chunk identity already stores different bytes")
	}

	stream := clonePersistedStream(existing)
	data := chunk.Data
	if sink.redactor.active() {
		stored, pending := sink.redactor.stream(stream.Pending, chunk.Data)
		stream.Pending = pending
		data = stored
	}
	stream.Chunks = append(stream.Chunks, persistedChunk{
		ExecutionID: string(chunk.ExecutionID),
		ActionID:    string(chunk.ActionID),
		Stream:      string(chunk.Stream),
		Offset:      chunk.Offset,
		Sequence:    chunk.Sequence,
		StoredAt:    sink.clock.Now(),
		Data:        append([]byte(nil), data...),
		RawHash:     append([]byte(nil), rawHash[:]...),
	})
	for len(stream.Chunks) > sink.limits.MaxStoredChunks {
		dropped := stream.Chunks[0]
		stream.Chunks = stream.Chunks[1:]
		if dropped.Offset > stream.DroppedThrough {
			stream.DroppedThrough = dropped.Offset
		}
	}
	sink.state.Streams[key] = stream
	if err := sink.persist(); err != nil {
		if hadStream {
			sink.state.Streams[key] = existing
		} else {
			delete(sink.state.Streams, key)
		}
		return AppendResult{}, err
	}
	return AppendResult{Offset: chunk.Offset, Sequence: chunk.Sequence}, nil
}

// Read serves one bounded ordered page from the supplied ownership scope.
func (sink *FileSink) Read(ctx context.Context, request ReadRequest) (ReadResult, error) {
	return sink.read(ctx, request, nil)
}

// readAfterPair serves chunks strictly after the (offset, sequence) pair named
// by request.AfterOffset and afterSequence. The declared RetainedSink contract
// carries only an offset lower bound, which cannot address a later chunk that
// shares an offset; this same-package capability lets the operation page a
// shared-offset group of any size without skipping or repeating a chunk.
func (sink *FileSink) readAfterPair(ctx context.Context, request ReadRequest, afterSequence uint64) (ReadResult, error) {
	after := chunkPosition{offset: request.AfterOffset, sequence: afterSequence}
	return sink.read(ctx, request, &after)
}

// read serves one bounded ordered page. A nil after uses the contract's
// offset-only continuation; a non-nil after continues strictly after that
// exact pair.
func (sink *FileSink) read(ctx context.Context, request ReadRequest, after *chunkPosition) (ReadResult, error) {
	if err := request.Validate(); err != nil {
		return ReadResult{}, err
	}
	if request.Limit > sink.limits.MaxReadChunks {
		return ReadResult{}, fault.New(fault.InvalidInput, "retained log read limit exceeds the configured bound")
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()

	key := streamKey(request.Scope, Chunk{
		ExecutionID: request.ExecutionID,
		ActionID:    request.ActionID,
		Stream:      request.Stream,
	})
	existing, hadStream := sink.state.Streams[key]
	stream := clonePersistedStream(existing)
	now := sink.clock.Now()
	retained := make([]persistedChunk, 0, len(stream.Chunks))
	dropped := false
	for _, chunk := range stream.Chunks {
		if !now.Before(chunk.StoredAt.Add(sink.retention.RetainFor)) {
			if chunk.Offset > stream.DroppedThrough {
				stream.DroppedThrough = chunk.Offset
			}
			dropped = true
			continue
		}
		retained = append(retained, chunk)
	}
	stream.Chunks = retained
	if dropped {
		sink.state.Streams[key] = stream
		if err := sink.persist(); err != nil {
			if hadStream {
				sink.state.Streams[key] = existing
			} else {
				delete(sink.state.Streams, key)
			}
			return ReadResult{}, err
		}
	}

	candidates := make([]persistedChunk, 0, len(stream.Chunks))
	for _, chunk := range stream.Chunks {
		if after != nil {
			if chunk.Offset < after.offset {
				continue
			}
			if chunk.Offset == after.offset && chunk.Sequence <= after.sequence {
				continue
			}
			candidates = append(candidates, chunk)
			continue
		}
		if chunk.Offset > request.AfterOffset || (request.AfterOffset == 0 && chunk.Offset == 0) {
			candidates = append(candidates, chunk)
		}
	}
	sort.SliceStable(candidates, func(left, right int) bool {
		if candidates[left].Offset != candidates[right].Offset {
			return candidates[left].Offset < candidates[right].Offset
		}
		return candidates[left].Sequence < candidates[right].Sequence
	})
	if len(candidates) > request.Limit {
		candidates = candidates[:request.Limit]
	}
	chunks := make([]Chunk, 0, len(candidates))
	nextOffset := request.AfterOffset
	for _, chunk := range candidates {
		chunks = append(chunks, Chunk{
			ExecutionID: id.ExecutionID(chunk.ExecutionID),
			ActionID:    id.ActionID(chunk.ActionID),
			Stream:      Stream(chunk.Stream),
			Offset:      chunk.Offset,
			Sequence:    chunk.Sequence,
			Data:        append([]byte(nil), chunk.Data...),
		})
		nextOffset = chunk.Offset
	}
	return ReadResult{
		Chunks:     chunks,
		NextOffset: nextOffset,
		Truncated:  stream.DroppedThrough > request.AfterOffset,
	}, nil
}

func (sink *FileSink) load() error {
	data, err := os.ReadFile(sink.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fault.Wrap(fault.InternalFailure, "retained log state could not be read", err)
	}
	var state sinkState
	if err := json.Unmarshal(data, &state); err != nil {
		return fault.Wrap(fault.InternalFailure, "retained log state is not valid JSON", err)
	}
	if state.Streams == nil {
		state.Streams = map[string]persistedStream{}
	}
	sink.state = state
	return nil
}

func (sink *FileSink) persist() error {
	if err := os.MkdirAll(sink.root, 0o700); err != nil {
		return fault.Wrap(fault.InternalFailure, "retained log root could not be created", err)
	}
	data, err := json.Marshal(sink.state)
	if err != nil {
		return fault.Wrap(fault.InternalFailure, "retained log state could not be encoded", err)
	}
	temporary, err := os.CreateTemp(sink.root, "retained-log-*.tmp")
	if err != nil {
		return fault.Wrap(fault.InternalFailure, "retained log state could not be staged", err)
	}
	tempPath := temporary.Name()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		_ = os.Remove(tempPath)
		return fault.Wrap(fault.InternalFailure, "retained log state mode could not be applied", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		_ = os.Remove(tempPath)
		return fault.Wrap(fault.InternalFailure, "retained log state could not be written", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		_ = os.Remove(tempPath)
		return fault.Wrap(fault.InternalFailure, "retained log state could not be synced", err)
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(tempPath)
		return fault.Wrap(fault.InternalFailure, "retained log state could not be closed", err)
	}
	if err := os.Rename(tempPath, sink.path); err != nil {
		_ = os.Remove(tempPath)
		return fault.Wrap(fault.InternalFailure, "retained log state could not be committed", err)
	}
	dir, err := os.Open(sink.root)
	if err != nil {
		return fault.Wrap(fault.InternalFailure, "retained log root could not be opened for sync", err)
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return fault.Wrap(fault.InternalFailure, "retained log root could not be synced", err)
	}
	if err := dir.Close(); err != nil {
		return fault.Wrap(fault.InternalFailure, "retained log root could not be closed after sync", err)
	}
	return nil
}

func clonePersistedStream(stream persistedStream) persistedStream {
	var chunks []persistedChunk
	if stream.Chunks != nil {
		chunks = make([]persistedChunk, len(stream.Chunks))
		for index, chunk := range stream.Chunks {
			chunks[index] = persistedChunk{
				ExecutionID: chunk.ExecutionID,
				ActionID:    chunk.ActionID,
				Stream:      chunk.Stream,
				Offset:      chunk.Offset,
				Sequence:    chunk.Sequence,
				StoredAt:    chunk.StoredAt,
				Data:        bytes.Clone(chunk.Data),
				RawHash:     bytes.Clone(chunk.RawHash),
			}
		}
	}
	return persistedStream{
		Chunks:         chunks,
		DroppedThrough: stream.DroppedThrough,
		Pending:        bytes.Clone(stream.Pending),
	}
}

// streamKey scopes one chunk identity to the explicit ownership scope, so a
// read in another tenant or project can never see it.
func streamKey(scope tenancy.Scope, chunk Chunk) string {
	return string(scope.TenantID) + "\x00" + string(scope.ProjectID) + "\x00" +
		string(chunk.ExecutionID) + "\x00" + string(chunk.ActionID) + "\x00" + string(chunk.Stream)
}
