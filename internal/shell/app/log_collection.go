package app

// Retained-log collection before workload cleanup.
//
// The composition serves the read side of retained logs, but the Kernel
// removes the workload container directly, which would discard the
// container's output. This file adds a composition-only decorator around the
// Kernel runtime port: before the runtime removes a workload it drains the
// adapter's log read seam into the retained sink, and before a destructive stop
// (Kubernetes suspension terminates and deletes the active Pod) it makes the
// currently readable prefix durable first.
//
// Collection is read-only. It never prepares or starts a workload, and it never
// invents log content beyond the one explicit unavailability marker.
//
// Ordering is the whole safety argument:
//
//   - before Stop, after the cursor already says completion, delegate;
//   - otherwise resolve the execution's immutable owning scope, read a bounded
//     number of pages from the runtime after the saved opaque cursor, append
//     only newline-terminated output under that scope, save the adapter's
//     NextOffset, and only then delegate the destructive stop;
//   - a live trailing partial line is re-read with its continuation at Remove
//     instead of being stored at the checkpoint, so a credential split across
//     the boundary never reaches the sink in plaintext;
//   - a pre-stop temporary EOF or budget exhaustion is not completion: no
//     completion token is written and Remove resumes from the saved cursor to
//     drain any still-readable tail;
//   - on Remove, resolve the execution's immutable owning scope first;
//   - read one bounded page from the runtime after the saved opaque cursor;
//   - append every bounded chunk under that scope;
//   - only then save the adapter's NextOffset as the durable cursor;
//   - and only after every page is stored record the per-workload-identity
//     completion token in the same opaque cursor slot;
//   - and only after collection is durably complete call the inner Remove.
//
// Append-before-save means a lost cursor write can only re-read chunks the sink
// already stores, and the sink deduplicates them by identity and request bytes,
// so a retry neither gaps nor duplicates. The completion token makes a retry
// after the destructive removal idempotent: once the underlying Remove deleted
// the credential Secret, a release-pending retry must not read the redaction
// source again. Any read, append, cursor or scope fault returns a retryable
// fault before the inner Remove: the cleanup obligation stays pending and
// visible instead of losing the output. A Logs NotFound means the container is
// already gone; it records one explicit stdout marker only when the sink holds
// no chunk for the action, marks the workload identity complete, then allows
// removal.
//
// Cursor and completion state is keyed by the full effect identity, including
// the workload id. One action can own several workload identities over its life
// (a never-started workload is retired and a replacement allocated for the same
// action), and the replacement starts its own log at offset 0. Keying by
// (execution, action) alone would let the retired identity's completion token
// suppress the replacement's collection or let its position skip the
// replacement's first bytes. A cursor file named by (execution, action) alone
// is never read and there is no migration, so a record under that keying
// cannot be misapplied to a workload-scoped key.

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	kernelengine "github.com/optimiware/kodama/internal/kernel/engine"
	shelllog "github.com/optimiware/kodama/internal/shell/log"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// logCollectionUnavailableMarker is the fixed, secret-free stdout chunk stored
// for an action whose container is already gone and that has no retained chunk.
// It carries the fixed identity (offset 0, sequence 0).
const logCollectionUnavailableMarker = "[logs unavailable]\n"

// logCollectionComplete is the durable per-workload-identity completion token
// stored in the existing opaque cursor slot. Every real runtime cursor is a
// non-negative position well below this value (the Kubernetes continuation
// token tops out more than four billion bytes earlier), so a completed identity
// can be told apart from a resumable one without a new persisted field. The
// token is written only after the final cursor and any unavailability marker are
// already durable, and before the destructive inner Remove.
const logCollectionComplete int64 = math.MaxInt64

// logCollectionStopSnapshotPages bounds the pre-stop live snapshot. Kubernetes
// suspension terminates and deletes the active Pod, so the only chance to keep
// output the workload already produced is to read it before delegating Stop.
// A workload that never stops writing must not postpone cancellation: the
// snapshot is a bounded prefix, Remove still resumes from the saved cursor and
// drains any readable tail, and a missing source keeps what is already durable.
// The page bound is in units of the bounded runtime page, so the byte budget is
// at most this many pages (2 MiB at the 32 KiB chunk bound).
const logCollectionStopSnapshotPages = 64

// logCollectionRedactionMarker is the marker both runtime adapters and
// the retained sink substitute for a complete configured value. It lets the
// live checkpoint notice that a page was redacted, so its emitted byte length no
// longer maps one-to-one onto raw log offsets and the page's line boundary
// cannot be advanced to safely.
const logCollectionRedactionMarker = "[REDACTED]"

// logCollectionStopWithholdLimit bounds how many consecutive checkpoint faults
// may withhold the destructive stop for one workload identity. A persistent
// sink, cursor or log-source fault must not keep a live workload running past
// the kernel's cleanup bound: once the limit is spent, Stop is delegated even
// though the live output could not be made durable, and Remove records what it
// still can. A later checkpoint that succeeds resets the count.
const logCollectionStopWithholdLimit = 1

// Bounded, secret-free causes named in a retryable collection fault. The
// vocabulary is fixed so a stuck cleanup's LastError identifies the failed inner
// operation without a response body, credential, token or object name.
const (
	logCauseScopeUnavailable       = "scope_unavailable"
	logCausePodsListFailed         = "pods_list_failed"
	logCausePodLogNotFound         = "pod_log_not_found"
	logCausePodLogBadRequest       = "pod_log_bad_request"
	logCausePodLogServerError      = "pod_log_server_error"
	logCauseLogReadFailed          = "log_read_failed"
	logCauseRedactionSourceMissing = "redaction_source_missing"
	logCauseSinkUnavailable        = "sink_unavailable"
	logCauseCursorUnavailable      = "cursor_unavailable"
)

// executionScopeReader resolves the immutable owning scope of one execution.
// The collector must never guess a scope from the request or from ambient
// state.
type executionScopeReader interface {
	ExecutionScope(context.Context, id.ExecutionID) (tenancy.Scope, error)
}

// logCursorStore stores the opaque runtime log cursor of one workload identity
// durably. A saved cursor is the last offset whose chunks the sink already
// stored, and the completion token is stored in the same workload-scoped slot.
// The workload identity is part of the key: an action may own several workloads
// over its life, each with its own position numbering, so one identity's state
// must never apply to another.
type logCursorStore interface {
	Load(context.Context, id.ExecutionID, id.ActionID, id.WorkloadID) (int64, bool, error)
	Save(context.Context, id.ExecutionID, id.ActionID, id.WorkloadID, int64) error
}

// logCollectionConfig supplies the retained-log collection collaborators.
type logCollectionConfig struct {
	// Inner performs the real removal after collection succeeds.
	Inner kernelengine.Runtime
	// Sink is the durable retained-log sink that redacts and bounds storage.
	Sink shelllog.RetainedSink
	// Live publishes bounded live progress while an action still runs. A nil
	// value keeps live publication disabled; retained collection is unchanged.
	Live shelllog.LivePublisher
	// Scopes resolves the execution's immutable owning tenant/project scope.
	Scopes executionScopeReader
	// Cursors is the durable opaque runtime log cursor store.
	Cursors logCursorStore
	// PageBytes bounds one runtime log read. Zero uses the default chunk bound.
	PageBytes int
	// LiveInterval is the live poll cadence. Zero uses the default cadence; a
	// negative value is refused.
	LiveInterval time.Duration
}

// logCollectingRuntime decorates the Kernel runtime port so every removal first
// drains the workload's retained output into the sink.
type logCollectingRuntime struct {
	inner     kernelengine.Runtime
	sink      shelllog.RetainedSink
	live      shelllog.LivePublisher
	scopes    executionScopeReader
	cursors   logCursorStore
	pageBytes int

	// liveInterval is the bounded live-poll cadence. liveMu guards the pump set
	// and the closed flag so a Start racing Close cannot leave an unowned
	// goroutine reading a runtime the composition already stopped.
	liveInterval time.Duration
	liveMu       sync.Mutex
	liveClosed   bool
	livePumps    map[string]*liveLogPump

	// stopMu guards the in-process consecutive checkpoint-fault count used to
	// bound how long a persistent fault may withhold the destructive stop.
	stopMu       sync.Mutex
	stopWithheld map[string]int
}

var _ kernelengine.Runtime = (*logCollectingRuntime)(nil)

// newLogCollectingRuntime validates the collection boundary. Every collaborator
// is required; a missing one is refused instead of silently skipping
// collection.
func newLogCollectingRuntime(config logCollectionConfig) (*logCollectingRuntime, error) {
	switch {
	case config.Inner == nil:
		return nil, fault.New(fault.InvalidInput, "log collection requires a runtime")
	case config.Sink == nil:
		return nil, fault.New(fault.InvalidInput, "log collection requires a retained log sink")
	case config.Scopes == nil:
		return nil, fault.New(fault.InvalidInput, "log collection requires an execution scope reader")
	case config.Cursors == nil:
		return nil, fault.New(fault.InvalidInput, "log collection requires a durable log cursor store")
	}
	pageBytes := config.PageBytes
	if pageBytes == 0 {
		pageBytes = shelllog.DefaultBounds.MaxChunkBytes
	}
	if pageBytes < 0 {
		return nil, fault.New(fault.InvalidInput, "log collection page size must not be negative")
	}
	liveInterval := config.LiveInterval
	if liveInterval == 0 {
		liveInterval = defaultLiveLogInterval
	}
	if liveInterval < 0 {
		return nil, fault.New(fault.InvalidInput, "live log poll interval must not be negative")
	}
	return &logCollectingRuntime{
		inner:        config.Inner,
		sink:         config.Sink,
		live:         config.Live,
		scopes:       config.Scopes,
		cursors:      config.Cursors,
		pageBytes:    pageBytes,
		liveInterval: liveInterval,
		livePumps:    map[string]*liveLogPump{},
	}, nil
}

// Remove collects the workload's retained output and then removes it. Any
// collection fault leaves the removal to a later cleanup attempt.
//
// A durably completed collection is the one exception: the completion token is
// written before the destructive removal, so a retry whose inner Remove already
// deleted the credential Secret and whose workspace Release is still pending
// does not need the log source (or its redaction values) again. The inner
// Remove stays idempotent, so replaying it is safe.
func (runtime *logCollectingRuntime) Remove(ctx context.Context, request kernelengine.RemoveRequest) (kernelengine.Removal, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// Live progress is for a running action; Remove is the destructive
	// boundary, so stop it before the final retained drain reads the same
	// source again.
	runtime.stopLiveLogs(ctx, request.Identity)
	cursor, complete, err := runtime.loadCollectionCursor(ctx, request.Identity)
	if err != nil {
		return kernelengine.Removal{}, collectionFault(ctx, logCauseCursorUnavailable, err)
	}
	if !complete {
		scope, err := runtime.scopes.ExecutionScope(ctx, request.Identity.ExecutionID)
		if err != nil {
			// A scope failure is never "nothing left to remove": it must stay a
			// retryable dependency fault so cleanup does not treat it as absence.
			return kernelengine.Removal{}, collectionFault(ctx, logCauseScopeUnavailable, err)
		}
		if err := runtime.collect(ctx, request.Identity, scope, cursor); err != nil {
			return kernelengine.Removal{}, err
		}
	}
	return runtime.inner.Remove(ctx, request)
}

// loadCollectionCursor reads the durable per-workload-identity collection state.
// A complete identity needs no scope lookup and no log read; an absent cursor
// starts at the beginning.
func (runtime *logCollectingRuntime) loadCollectionCursor(ctx context.Context, identity kernelengine.EffectIdentity) (int64, bool, error) {
	cursor, ok, err := runtime.cursors.Load(ctx, identity.ExecutionID, identity.ActionID, identity.WorkloadID)
	if err != nil {
		return 0, false, err
	}
	if !ok {
		return 0, false, nil
	}
	if cursor == logCollectionComplete {
		return 0, true, nil
	}
	if cursor < 0 {
		return 0, false, fault.New(fault.InternalFailure, "saved retained log cursor is negative")
	}
	return cursor, false, nil
}

// collect drains every currently available log page for one action. It returns
// nil only when the sink durably holds the collected pages up to the adapter's
// final cursor, or when the container is already gone and the action is marked,
// and it then records the completion token so a later retry is idempotent.
func (runtime *logCollectingRuntime) collect(ctx context.Context, identity kernelengine.EffectIdentity, scope tenancy.Scope, cursor int64) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		batch, err := runtime.inner.Logs(ctx, kernelengine.LogsRequest{
			Identity:    identity,
			AfterOffset: cursor,
			Limit:       runtime.pageBytes,
		})
		if err != nil {
			if isNotFound(err) {
				return runtime.markUnavailable(ctx, identity, scope)
			}
			return collectionFault(ctx, retainedLogFailureCause(err), err)
		}
		if err := runtime.appendBatch(ctx, identity, scope, batch.Chunks); err != nil {
			return err
		}
		next := batch.NextOffset
		if next < cursor {
			return fault.New(fault.InternalFailure, "runtime log read returned a cursor behind the saved position")
		}
		if len(batch.Chunks) == 0 {
			// An empty page is the end of the currently available output. A
			// forward-only empty page is still saved so a retry does not read
			// the same gap again.
			if next > cursor {
				if err := runtime.cursors.Save(ctx, identity.ExecutionID, identity.ActionID, identity.WorkloadID, next); err != nil {
					return collectionFault(ctx, logCauseCursorUnavailable, err)
				}
			}
			return runtime.markCollectionComplete(ctx, identity)
		}
		if next == cursor {
			return fault.New(fault.InternalFailure, "runtime log read returned chunks without advancing the cursor")
		}
		// Append-before-save: the chunks above are already durable, so a lost
		// cursor write can only re-read them for sink deduplication.
		if err := runtime.cursors.Save(ctx, identity.ExecutionID, identity.ActionID, identity.WorkloadID, next); err != nil {
			return collectionFault(ctx, logCauseCursorUnavailable, err)
		}
		cursor = next
	}
}

// appendBatch stores every chunk of one runtime page under the resolved scope.
// An oversized runtime chunk is split into bounded pieces with stable offsets;
// append-before-save means a lost cursor write can only re-read chunks the sink
// already deduplicates. Each stored piece is also offered to the live publisher
// after it is durable; live delivery is best-effort and never fails collection.
//
// Offering the piece to the live publisher here can republish an offset the
// live pump already sent while the action was running (Stop's checkpoint and
// Remove's final drain read from a cursor the pump also advanced). Live offsets
// are therefore not strictly monotonic across a lifecycle boundary, and a
// consumer must deduplicate by the (execution, action, stream, offset) identity
// instead of assuming each offset arrives exactly once.
func (runtime *logCollectingRuntime) appendBatch(ctx context.Context, identity kernelengine.EffectIdentity, scope tenancy.Scope, chunks []kernelengine.LogChunk) error {
	for _, chunk := range chunks {
		pieces, err := splitLogChunk(identity, chunk, runtime.pageBytes)
		if err != nil {
			return err
		}
		for _, piece := range pieces {
			if _, err := runtime.sink.Append(ctx, scope, piece); err != nil {
				return collectionFault(ctx, logCauseSinkUnavailable, err)
			}
			if runtime.live != nil {
				_ = runtime.live.PublishLive(ctx, scope, piece)
			}
		}
	}
	return nil
}

// checkpointBeforeStop makes the currently readable output durable before the
// destructive stop. Kubernetes suspension terminates and deletes the active
// Pod, so a Remove-time collection is too late; this is the first retention
// boundary for that adapter.
//
// It is a bounded snapshot, not a completed collection: it never writes the
// completion token, it stops at a temporary EOF or after the page budget, and
// Remove resumes from the saved cursor to drain any still-readable tail. A
// source that is already gone and a container that has not started yet are not
// faults; Remove records the explicit unavailability marker only if the sink
// holds nothing. Any other read, append or cursor fault is returned, so the
// destructive stop is not delegated before durable retention succeeded.
func (runtime *logCollectingRuntime) checkpointBeforeStop(ctx context.Context, identity kernelengine.EffectIdentity) error {
	cursor, complete, err := runtime.loadCollectionCursor(ctx, identity)
	if err != nil {
		return collectionFault(ctx, logCauseCursorUnavailable, err)
	}
	if complete {
		return nil
	}
	scope, err := runtime.scopes.ExecutionScope(ctx, identity.ExecutionID)
	if err != nil {
		return collectionFault(ctx, logCauseScopeUnavailable, err)
	}
	budget := logCollectionStopSnapshotPages
	for budget > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		budget--
		batch, err := runtime.inner.Logs(ctx, kernelengine.LogsRequest{
			Identity:    identity,
			AfterOffset: cursor,
			Limit:       runtime.pageBytes,
		})
		if err != nil {
			if stopSourceAbsent(err) {
				return nil
			}
			return collectionFault(ctx, retainedLogFailureCause(err), err)
		}
		if batch.NextOffset < cursor {
			return fault.New(fault.InternalFailure, "runtime log read returned a cursor behind the saved position")
		}
		if len(batch.Chunks) == 0 {
			// A temporary EOF is not collection complete: the workload may
			// still write while it handles termination.
			if batch.NextOffset > cursor {
				if err := runtime.cursors.Save(ctx, identity.ExecutionID, identity.ActionID, identity.WorkloadID, batch.NextOffset); err != nil {
					return collectionFault(ctx, logCauseCursorUnavailable, err)
				}
			}
			return nil
		}
		if batch.NextOffset == cursor {
			return fault.New(fault.InternalFailure, "runtime log read returned chunks without advancing the cursor")
		}

		// A live checkpoint must never advance past a trailing partial line: a
		// credential split between this poll and a later one is complete only
		// after the boundary, and at a temporary EOF the adapter cannot know
		// that more bytes may still arrive. Store only newline-terminated
		// output; leave the trailing partial for the final Remove drain.
		safe, prefix := checkpointPagePrefix(batch)
		if !safe {
			if prefix <= 0 || budget <= 0 {
				return nil
			}
			// Re-read exactly the line-terminated prefix. The second read ends
			// on a line boundary, so its opaque NextOffset is a safe cursor
			// without decoding the adapter's cursor token.
			budget--
			batch, err = runtime.inner.Logs(ctx, kernelengine.LogsRequest{
				Identity:    identity,
				AfterOffset: cursor,
				Limit:       prefix,
			})
			if err != nil {
				if stopSourceAbsent(err) {
					return nil
				}
				return collectionFault(ctx, retainedLogFailureCause(err), err)
			}
			if !checkpointBatchEndsOnLine(batch) || batch.NextOffset <= cursor {
				// The adapter held a possible value at the boundary. Leave the
				// bytes unread for the final drain.
				return nil
			}
		}
		if err := runtime.appendBatch(ctx, identity, scope, batch.Chunks); err != nil {
			return err
		}
		// Append-before-save, exactly as in Remove-time collection.
		if err := runtime.cursors.Save(ctx, identity.ExecutionID, identity.ActionID, identity.WorkloadID, batch.NextOffset); err != nil {
			return collectionFault(ctx, logCauseCursorUnavailable, err)
		}
		cursor = batch.NextOffset
	}
	// The bounded snapshot budget is spent. The durable prefix is retained and
	// Remove resumes from its saved cursor.
	return nil
}

// checkpointPagePrefix reports whether one live page can be stored whole. When
// it cannot, safePrefix is the byte length of the page's newline-terminated
// prefix that may be stored without crossing a possible partial credential.
//
// A page ending on a newline is safe: no credential can complete across the
// boundary. A trailing line without a newline may be a partial credential and
// must be re-read with its continuation, so it is left for the final drain.
// Once the adapter has substituted the redaction marker, the emitted length no
// longer maps one-to-one onto raw offsets, so a page with a marker is held whole
// rather than advanced to a cursor that could skip a partial value.
func checkpointPagePrefix(batch kernelengine.LogBatch) (safe bool, safePrefix int) {
	if len(batch.Chunks) == 0 {
		return true, 0
	}
	if len(batch.Chunks) != 1 {
		// Several streams cannot share one opaque cursor prefix: only a batch
		// whose streams all end on line boundaries is stored whole.
		return checkpointBatchEndsOnLine(batch), 0
	}
	data := batch.Chunks[0].Data
	if len(data) == 0 || data[len(data)-1] == '\n' {
		return true, 0
	}
	newline := bytes.LastIndexByte(data, '\n')
	if newline < 0 {
		return false, 0
	}
	if bytes.Contains(data, []byte(logCollectionRedactionMarker)) {
		return false, 0
	}
	return false, newline + 1
}

// checkpointBatchEndsOnLine reports whether every emitted stream ends on a line
// boundary, so the adapter's opaque continuation token is safe to persist.
func checkpointBatchEndsOnLine(batch kernelengine.LogBatch) bool {
	if len(batch.Chunks) == 0 {
		return false
	}
	for _, chunk := range batch.Chunks {
		if len(chunk.Data) > 0 && chunk.Data[len(chunk.Data)-1] != '\n' {
			return false
		}
	}
	return true
}

// stopSourceAbsent reports whether a live checkpoint read failed because there
// is no readable source yet or any more. Such a source cannot have produced
// readable output, so the destructive stop proceeds instead of waiting.
func stopSourceAbsent(err error) bool {
	return isNotFound(err) || errors.Is(err, runtimekube.ErrPodLogBadRequest)
}

// withholdStop records one consecutive checkpoint fault and reports whether the
// destructive stop should still be withheld. After the bound is spent the caller
// delegates Stop so a persistent fault cannot leave the workload running; a
// successful checkpoint clears the count.
func (runtime *logCollectingRuntime) withholdStop(identity kernelengine.EffectIdentity) bool {
	runtime.stopMu.Lock()
	defer runtime.stopMu.Unlock()
	if runtime.stopWithheld == nil {
		runtime.stopWithheld = map[string]int{}
	}
	key := string(identity.ExecutionID) + "\x00" + string(identity.ActionID) + "\x00" + string(identity.WorkloadID)
	runtime.stopWithheld[key]++
	return runtime.stopWithheld[key] <= logCollectionStopWithholdLimit
}

// clearStopWithhold forgets a workload's consecutive checkpoint faults after a
// checkpoint succeeded.
func (runtime *logCollectingRuntime) clearStopWithhold(identity kernelengine.EffectIdentity) {
	runtime.stopMu.Lock()
	defer runtime.stopMu.Unlock()
	if runtime.stopWithheld == nil {
		return
	}
	delete(runtime.stopWithheld, string(identity.ExecutionID)+"\x00"+string(identity.ActionID)+"\x00"+string(identity.WorkloadID))
}

// markCollectionComplete records the durable per-workload-identity completion
// token after the final cursor (and any unavailable marker) is already stored.
// It runs before the destructive inner Remove, so the retry that follows a
// successful removal never depends on a log source the removal deleted.
func (runtime *logCollectingRuntime) markCollectionComplete(ctx context.Context, identity kernelengine.EffectIdentity) error {
	if err := runtime.cursors.Save(ctx, identity.ExecutionID, identity.ActionID, identity.WorkloadID, logCollectionComplete); err != nil {
		return collectionFault(ctx, logCauseCursorUnavailable, err)
	}
	return nil
}

// markUnavailable records exactly one explicit unavailability marker when the
// container is already gone and the action has no retained chunk at all. An
// already collected action keeps its output and gains no marker. The marker is
// durable before the completion token, so a retry never needs the vanished log
// source again.
func (runtime *logCollectingRuntime) markUnavailable(ctx context.Context, identity kernelengine.EffectIdentity, scope tenancy.Scope) error {
	held, err := runtime.holdsChunks(ctx, identity, scope)
	if err != nil {
		// The marker identity cannot be decided safely without the sink state,
		// so a sink read fault keeps cleanup pending rather than risking a
		// duplicate marker or a silent gap.
		return collectionFault(ctx, logCauseSinkUnavailable, err)
	}
	if !held {
		_, err = runtime.sink.Append(ctx, scope, shelllog.Chunk{
			ExecutionID: identity.ExecutionID,
			ActionID:    identity.ActionID,
			Stream:      shelllog.StreamStdout,
			Offset:      0,
			Sequence:    0,
			Data:        []byte(logCollectionUnavailableMarker),
		})
		if err != nil {
			return collectionFault(ctx, logCauseSinkUnavailable, err)
		}
	}
	return runtime.markCollectionComplete(ctx, identity)
}

// holdsChunks reports whether the sink already stores any chunk for the action
// in either stream.
func (runtime *logCollectingRuntime) holdsChunks(ctx context.Context, identity kernelengine.EffectIdentity, scope tenancy.Scope) (bool, error) {
	for _, stream := range shelllog.Streams() {
		result, err := runtime.sink.Read(ctx, shelllog.ReadRequest{
			Scope:       scope,
			ExecutionID: identity.ExecutionID,
			ActionID:    identity.ActionID,
			Stream:      stream,
			AfterOffset: 0,
			Limit:       1,
		})
		if err != nil {
			return false, err
		}
		if len(result.Chunks) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// splitLogChunk converts one runtime chunk into sink chunks no larger than the
// page bound. An oversized chunk keeps its starting offset and advances the
// offset of every following piece by one page, so the sink identity of each
// piece is stable across retries. The runtime's own sequence is preserved: the
// pieces are told apart by their distinct offsets.
func splitLogChunk(identity kernelengine.EffectIdentity, chunk kernelengine.LogChunk, pageBytes int) ([]shelllog.Chunk, error) {
	stream, err := retainedStream(chunk.Stream)
	if err != nil {
		return nil, err
	}
	if chunk.Offset < 0 || chunk.Sequence < 0 {
		return nil, fault.New(fault.InternalFailure, "runtime log chunk has a negative offset or sequence")
	}
	pieces := 1
	if len(chunk.Data) > pageBytes {
		pieces = (len(chunk.Data) + pageBytes - 1) / pageBytes
	}
	output := make([]shelllog.Chunk, 0, pieces)
	for index := 0; index < pieces; index++ {
		start := index * pageBytes
		end := min(start+pageBytes, len(chunk.Data))
		offset := chunk.Offset + int64(start)
		if offset < chunk.Offset {
			return nil, fault.New(fault.InternalFailure, "runtime log chunk offset overflows")
		}
		output = append(output, shelllog.Chunk{
			ExecutionID: identity.ExecutionID,
			ActionID:    identity.ActionID,
			Stream:      stream,
			Offset:      uint64(offset),
			Sequence:    uint64(chunk.Sequence),
			Data:        append([]byte(nil), chunk.Data[start:end]...),
		})
	}
	return output, nil
}

// retainedStream maps a Kernel log stream to the retained sink stream.
func retainedStream(stream kernelengine.LogStream) (shelllog.Stream, error) {
	switch stream {
	case kernelengine.LogStdout:
		return shelllog.StreamStdout, nil
	case kernelengine.LogStderr:
		return shelllog.StreamStderr, nil
	default:
		return "", fault.New(fault.InternalFailure, "runtime log chunk has an unknown stream")
	}
}

// retainedLogFailureCause maps a log-read failure onto the bounded, secret-free
// cause vocabulary. The runtime adapter wraps one classification sentinel per
// inner operation; an unrecognized failure (for example a transport error) uses
// the generic read-failure token instead of echoing the cause.
func retainedLogFailureCause(err error) string {
	switch {
	case errors.Is(err, runtimekube.ErrPodListUnavailable):
		return logCausePodsListFailed
	case errors.Is(err, runtimekube.ErrPodLogNotFound):
		return logCausePodLogNotFound
	case errors.Is(err, runtimekube.ErrPodLogBadRequest):
		return logCausePodLogBadRequest
	case errors.Is(err, runtimekube.ErrPodLogServerError):
		return logCausePodLogServerError
	case errors.Is(err, runtimekube.ErrRedactionSourceUnavailable):
		return logCauseRedactionSourceMissing
	default:
		return logCauseLogReadFailed
	}
}

// collectionFault turns any collection failure into a retryable dependency
// fault whose public message names a bounded cause. The outer class is always
// dependency_unavailable so cleanup never mistakes a collection failure for an
// absent removal and skips the workspace release guard. Context cancellation is
// propagated unchanged.
func collectionFault(ctx context.Context, cause string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return fault.Wrap(fault.DependencyUnavailable, "retained log collection failed: "+cause, err)
}

// controllerExecutionScopes resolves an execution's immutable owning scope
// through the cross-scope controller seam. The Kernel calls Remove with only
// the effect identity, so the composition must read the execution row to learn
// the tenant/project the retained chunks belong to.
type controllerExecutionScopes struct {
	controller store.Controller
}

var _ executionScopeReader = (*controllerExecutionScopes)(nil)

func newControllerExecutionScopes(controller store.Controller) (*controllerExecutionScopes, error) {
	if controller == nil {
		return nil, fault.New(fault.InvalidInput, "execution scope reader requires a store controller")
	}
	return &controllerExecutionScopes{controller: controller}, nil
}

func (reader *controllerExecutionScopes) ExecutionScope(ctx context.Context, executionID id.ExecutionID) (tenancy.Scope, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return tenancy.Scope{}, err
	}
	if _, err := id.ParseExecutionID(string(executionID)); err != nil {
		return tenancy.Scope{}, fault.Wrap(fault.DependencyUnavailable, "execution scope is unavailable", err)
	}
	row, err := reader.controller.Read(ctx, "executions", string(executionID))
	if err != nil {
		return tenancy.Scope{}, fault.Wrap(fault.DependencyUnavailable, "execution scope is unavailable", err)
	}
	tenant, err := rowText(row, "tenant_id")
	if err != nil {
		return tenancy.Scope{}, fault.Wrap(fault.DependencyUnavailable, "execution scope is unavailable", err)
	}
	project, err := rowText(row, "project_id")
	if err != nil {
		return tenancy.Scope{}, fault.Wrap(fault.DependencyUnavailable, "execution scope is unavailable", err)
	}
	scope := tenancy.Scope{TenantID: id.TenantID(tenant), ProjectID: id.ProjectID(project)}
	if err := scope.Validate(); err != nil {
		return tenancy.Scope{}, fault.Wrap(fault.DependencyUnavailable, "execution scope is unavailable", err)
	}
	return scope, nil
}

// fileLogCursorStore persists one opaque runtime log cursor per workload
// identity under DataDir/logs/cursors. Each save is staged in the same directory
// and committed with an atomic rename, so a reader never observes a partial
// cursor.
//
// The file name includes the workload id, so a file keyed by only
// (execution, action) is never opened: the names cannot collide and no
// migration is attempted. A record under that keying is therefore ignored
// rather than misapplied to a workload identity it does not describe.
type fileLogCursorStore struct {
	root string
	mu   sync.Mutex
}

var _ logCursorStore = (*fileLogCursorStore)(nil)

func newFileLogCursorStore(dataDir string) (*fileLogCursorStore, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, fault.New(fault.InvalidInput, "retained log collection requires a data directory")
	}
	return &fileLogCursorStore{root: filepath.Join(dataDir, "logs", "cursors")}, nil
}

// cursorPath maps one workload identity to a traversal-safe file name. Every
// identity is hex encoded, so no identity byte can escape the cursor directory.
// The workload component is what keeps two workloads of the same action apart;
// a file named by (execution, action) alone has a different name and is never
// read.
func (store *fileLogCursorStore) cursorPath(executionID id.ExecutionID, actionID id.ActionID, workloadID id.WorkloadID) (string, error) {
	if _, err := id.ParseExecutionID(string(executionID)); err != nil {
		return "", fault.Wrap(fault.InvalidInput, "retained log cursor execution id is invalid", err)
	}
	if _, err := id.ParseActionID(string(actionID)); err != nil {
		return "", fault.Wrap(fault.InvalidInput, "retained log cursor action id is invalid", err)
	}
	if _, err := id.ParseWorkloadID(string(workloadID)); err != nil {
		return "", fault.Wrap(fault.InvalidInput, "retained log cursor workload id is invalid", err)
	}
	name := hex.EncodeToString([]byte(executionID)) + "-" + hex.EncodeToString([]byte(actionID)) +
		"-" + hex.EncodeToString([]byte(workloadID)) + ".cursor"
	return filepath.Join(store.root, name), nil
}

func (store *fileLogCursorStore) Load(ctx context.Context, executionID id.ExecutionID, actionID id.ActionID, workloadID id.WorkloadID) (int64, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	path, err := store.cursorPath(executionID, actionID, workloadID)
	if err != nil {
		return 0, false, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, false, nil
		}
		return 0, false, fault.Wrap(fault.InternalFailure, "retained log cursor could not be read", err)
	}
	cursor, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, false, fault.Wrap(fault.InternalFailure, "retained log cursor is not a valid offset", err)
	}
	if cursor < 0 {
		return 0, false, fault.New(fault.InternalFailure, "retained log cursor is negative")
	}
	return cursor, true, nil
}

func (store *fileLogCursorStore) Save(ctx context.Context, executionID id.ExecutionID, actionID id.ActionID, workloadID id.WorkloadID, cursor int64) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if cursor < 0 {
		return fault.New(fault.InvalidInput, "retained log cursor must not be negative")
	}
	path, err := store.cursorPath(executionID, actionID, workloadID)
	if err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := os.MkdirAll(store.root, 0o700); err != nil {
		return fault.Wrap(fault.InternalFailure, "retained log cursor directory could not be created", err)
	}
	temporary, err := os.CreateTemp(store.root, ".cursor-*.tmp")
	if err != nil {
		return fault.Wrap(fault.InternalFailure, "retained log cursor could not be staged", err)
	}
	name := temporary.Name()
	committed := false
	defer func() {
		if !committed {
			os.Remove(name)
		}
	}()
	if _, err := temporary.WriteString(strconv.FormatInt(cursor, 10)); err != nil {
		temporary.Close()
		return fault.Wrap(fault.InternalFailure, "retained log cursor could not be written", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fault.Wrap(fault.InternalFailure, "retained log cursor could not be flushed", err)
	}
	if err := temporary.Close(); err != nil {
		return fault.Wrap(fault.InternalFailure, "retained log cursor could not be closed", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fault.Wrap(fault.InternalFailure, "retained log cursor could not be committed", err)
	}
	committed = true
	dir, err := os.Open(store.root)
	if err != nil {
		return fault.Wrap(fault.InternalFailure, "retained log cursor directory could not be opened", err)
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil {
		return fault.Wrap(fault.InternalFailure, "retained log cursor directory could not be synced", syncErr)
	}
	if closeErr != nil {
		return fault.Wrap(fault.InternalFailure, "retained log cursor directory could not be closed", closeErr)
	}
	return nil
}

// Capabilities delegates to the decorated runtime.
func (runtime *logCollectingRuntime) Capabilities(ctx context.Context) (kernelengine.Capabilities, error) {
	return runtime.inner.Capabilities(ctx)
}

// Prepare delegates unchanged. Collection never prepares a workload.
func (runtime *logCollectingRuntime) Prepare(ctx context.Context, request kernelengine.PrepareRequest) (kernelengine.Preparation, error) {
	return runtime.inner.Prepare(ctx, request)
}

// Start delegates the start effect unchanged and, when the runtime accepted or
// may have accepted the workload, opens a bounded live-log pump for it.
//
// A start error and an unknown outcome are not "nothing ran": the engine
// persists LaunchUnknown for both (persistUnknownLaunch) and observes instead
// of relaunching, so a workload may be producing output this decorator should
// stream. Only affirmative StartProvenNotStarted evidence leaves nothing to
// read, and only that case skips the pump. A live publication failure never
// changes the start outcome: retained collection at Stop/Remove remains the
// source of record either way.
func (runtime *logCollectingRuntime) Start(ctx context.Context, request kernelengine.StartRequest) (kernelengine.StartResult, error) {
	result, err := runtime.inner.Start(ctx, request)
	if result.Outcome != kernelengine.StartProvenNotStarted {
		runtime.startLiveLogs(request.Identity)
	}
	return result, err
}

// Observe delegates unchanged.
func (runtime *logCollectingRuntime) Observe(ctx context.Context, request kernelengine.ObserveRequest) (kernel.Observation, error) {
	return runtime.inner.Observe(ctx, request)
}

// Logs delegates unchanged. The collector only reads logs around Remove.
func (runtime *logCollectingRuntime) Logs(ctx context.Context, request kernelengine.LogsRequest) (kernelengine.LogBatch, error) {
	return runtime.inner.Logs(ctx, request)
}

// Stop makes the output the workload already produced durable before the
// destructive termination, then delegates. On Kubernetes the inner Stop
// suspends the Job and the controller deletes the active Pod, so Remove-time
// collection would only see a vanished source. Only newline-terminated live
// output is checkpointed; a trailing partial line is left for the final Remove
// drain so a credential split across the boundary is never stored in plaintext.
//
// When the retained prefix cannot be made durable the destructive stop is
// withheld as a retryable dependency fault, but only for a bounded number of
// consecutive faults: a persistent sink, cursor or source fault must not leave a
// live workload running until the kernel's cleanup bound expires. After the
// bound the stop is delegated and Remove records what it still can. A source
// that is already gone or a workload that has not started is never a fault and
// the stop proceeds.
//
// The checkpoint can republish live offsets the pump already sent; live offsets
// are not strictly monotonic across this boundary (see appendBatch).
func (runtime *logCollectingRuntime) Stop(ctx context.Context, request kernelengine.StopRequest) (kernel.StopEvidence, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// The action is no longer producing progress the live path should chase;
	// cancel the pump before the retention checkpoint reads the same source.
	runtime.stopLiveLogs(ctx, request.Identity)
	if err := runtime.checkpointBeforeStop(ctx, request.Identity); err != nil {
		if runtime.withholdStop(request.Identity) {
			return kernel.PendingStop("stop withheld: readable output could not be retained"), err
		}
		// The withholding bound is spent. Delegate so a persistent checkpoint
		// fault cannot strand a live workload; cleanup stays pending and Retry
		// continues through Remove.
		return runtime.inner.Stop(ctx, request)
	}
	runtime.clearStopWithhold(request.Identity)
	return runtime.inner.Stop(ctx, request)
}

// RefreshCredential delegates unchanged.
func (runtime *logCollectingRuntime) RefreshCredential(ctx context.Context, request kernelengine.CredentialRequest) (kernelengine.CredentialRefresh, error) {
	return runtime.inner.RefreshCredential(ctx, request)
}
