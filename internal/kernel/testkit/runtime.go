package testkit

import (
	"context"
	"fmt"
	"sync"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
)

// MaxLogChunkBytes bounds every chunk the fake runtime stores and returns. A
// larger append is split into ordered chunks; a read never returns more than
// this many bytes in one chunk.
const MaxLogChunkBytes = 4096

// Workload is a read-only snapshot of one fake runtime resource.
type Workload struct {
	Prepared            bool
	Started             bool
	StartCount          int
	StopRequests        int
	StopConfirmed       bool
	LeaseExpired        bool
	Removed             bool
	CredentialRefreshes int
}

// Runtime is a deterministic engine.Runtime. Resources are stable per workload
// identity: a retry observes the same object instead of creating a new one, and
// a replayed start is refused rather than silently applied twice.
//
// The fake separates an effect from its response. Failure, blocking and
// lost-response injections are one-shot and explicitly staged by the test, so
// every crash boundary in K-RECOVER can be reproduced without sleeps.
type Runtime struct {
	mu sync.Mutex

	capabilities []string
	workloads    map[id.WorkloadID]*workloadRecord
	secrets      *Secrets

	prepares int
	starts   int

	failPrepare           error
	failStartBeforeEffect error
	refuseStart           *string
	loseStartResponse     *string
	blockStartBefore      *Barrier
	blockStartAfter       *Barrier

	failStopBeforeEffect error
	loseStopResponse     *string
	blockStopAfter       *Barrier
	confirmStop          *string

	failRemove error
	removals   []engine.RemoveRequest

	failCredential error
}

type workloadRecord struct {
	workspaceID         id.WorkspaceID
	started             bool
	startCount          int
	stopRequests        int
	stopConfirmed       bool
	leaseExpired        bool
	removed             bool
	credentialRefreshes int
	observations        []kernel.Observation
	logs                []engine.LogChunk
	nextOffset          int64
	nextSequence        int64
}

// NewRuntime returns an empty runtime fake with no declared capabilities.
func NewRuntime() *Runtime {
	return &Runtime{
		workloads: make(map[id.WorkloadID]*workloadRecord),
		secrets:   NewSecrets(),
	}
}

// SetCapabilities declares the adapter capability set.
func (r *Runtime) SetCapabilities(names ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.capabilities = append([]string(nil), names...)
}

// SetSecrets attaches the secret-delivery fake used by Prepare. A nil fake is
// replaced by a fresh one.
func (r *Runtime) SetSecrets(secrets *Secrets) {
	if secrets == nil {
		secrets = NewSecrets()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.secrets = secrets
}

// Capabilities returns a copy of the declared capability set.
func (r *Runtime) Capabilities(ctx context.Context) (engine.Capabilities, error) {
	if err := ctx.Err(); err != nil {
		return engine.Capabilities{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return engine.Capabilities{Names: append([]string(nil), r.capabilities...)}, nil
}

// Prepare materializes one workload at most once. A second prepare for the same
// stable identity is idempotent; a conflicting workspace is caller misuse.
// Secret delivery happens before the resource exists, so a delivery failure
// leaves nothing to start.
func (r *Runtime) Prepare(ctx context.Context, request engine.PrepareRequest) (engine.Preparation, error) {
	if err := ctx.Err(); err != nil {
		return engine.Preparation{}, err
	}
	if request.Identity.WorkloadID == "" {
		return engine.Preparation{}, fault.New(fault.InvalidInput, "prepare requires a workload identity")
	}
	if request.WorkspaceID == "" {
		return engine.Preparation{}, fault.New(fault.InvalidInput, "prepare requires a workspace identity")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if record, ok := r.workloads[request.Identity.WorkloadID]; ok {
		if record.removed {
			return engine.Preparation{}, fault.New(fault.InvalidInput, fmt.Sprintf(
				"workload %q was removed; its stable identity cannot be reused",
				request.Identity.WorkloadID,
			))
		}
		if record.workspaceID != request.WorkspaceID {
			return engine.Preparation{}, fault.New(fault.InvalidInput, fmt.Sprintf(
				"workload %q is already prepared for workspace %q, not %q",
				request.Identity.WorkloadID, record.workspaceID, request.WorkspaceID,
			))
		}
		return engine.Preparation{WorkspaceID: record.workspaceID, Detail: "already prepared"}, nil
	}
	if r.failPrepare != nil {
		err := r.failPrepare
		r.failPrepare = nil
		return engine.Preparation{}, err
	}
	if len(request.Secrets) > 0 {
		if err := r.secrets.Deliver(request.Identity.WorkloadID, request.Secrets); err != nil {
			return engine.Preparation{}, err
		}
	}
	r.workloads[request.Identity.WorkloadID] = &workloadRecord{workspaceID: request.WorkspaceID}
	r.prepares++
	return engine.Preparation{WorkspaceID: request.WorkspaceID, Detail: "materialized"}, nil
}

// Start applies the start effect at most once for the stable workload identity.
// A replayed start is refused. An injected refusal reports affirmative proof of
// nonexecution without applying an effect or consuming the attempt; a pre-effect
// failure leaves no effect; a lost post-effect response reports StartUnknown
// while preserving the effect.
func (r *Runtime) Start(ctx context.Context, request engine.StartRequest) (engine.StartResult, error) {
	if err := ctx.Err(); err != nil {
		return engine.StartResult{}, err
	}

	r.mu.Lock()
	record, ok := r.workloads[request.Identity.WorkloadID]
	if !ok {
		r.mu.Unlock()
		return engine.StartResult{}, ErrNotPrepared
	}
	if request.WorkspaceID != "" && record.workspaceID != request.WorkspaceID {
		r.mu.Unlock()
		return engine.StartResult{}, fault.New(fault.InvalidInput, fmt.Sprintf(
			"workload %q belongs to workspace %q, not %q",
			request.Identity.WorkloadID, record.workspaceID, request.WorkspaceID,
		))
	}
	if record.started {
		r.mu.Unlock()
		return engine.StartResult{}, ErrReplay
	}
	if record.removed {
		r.mu.Unlock()
		return engine.StartResult{}, fault.New(fault.InvalidInput, fmt.Sprintf(
			"workload %q was removed; its stable identity cannot be reused",
			request.Identity.WorkloadID,
		))
	}
	if r.failStartBeforeEffect != nil {
		err := r.failStartBeforeEffect
		r.failStartBeforeEffect = nil
		r.mu.Unlock()
		return engine.StartResult{}, err
	}
	// An injected refusal is affirmative proof of nonexecution. It applies no
	// effect and does not mark the identity started, so a later start of the
	// same prepared identity is still permitted.
	if r.refuseStart != nil {
		detail := *r.refuseStart
		r.refuseStart = nil
		r.mu.Unlock()
		return engine.StartResult{
			Outcome:    engine.StartProvenNotStarted,
			WorkloadID: request.Identity.WorkloadID,
			Detail:     detail,
		}, nil
	}
	barrier := r.blockStartBefore
	r.blockStartBefore = nil
	r.mu.Unlock()

	if barrier != nil {
		if err := barrier.Wait(ctx); err != nil {
			return engine.StartResult{}, err
		}
	}

	r.mu.Lock()
	record, ok = r.workloads[request.Identity.WorkloadID]
	if !ok {
		r.mu.Unlock()
		return engine.StartResult{}, ErrNotPrepared
	}
	if record.started {
		r.mu.Unlock()
		return engine.StartResult{}, ErrReplay
	}
	if record.removed {
		r.mu.Unlock()
		return engine.StartResult{}, fault.New(fault.InvalidInput, fmt.Sprintf(
			"workload %q was removed; its stable identity cannot be reused",
			request.Identity.WorkloadID,
		))
	}
	record.started = true
	record.startCount++
	r.starts++
	after := r.blockStartAfter
	r.blockStartAfter = nil
	lost := r.loseStartResponse
	r.loseStartResponse = nil
	r.mu.Unlock()

	if after != nil {
		if err := after.Wait(ctx); err != nil {
			return engine.StartResult{
				Outcome:    engine.StartUnknown,
				WorkloadID: request.Identity.WorkloadID,
				Detail:     "start response lost after the effect was applied",
			}, nil
		}
	}
	if lost != nil {
		return engine.StartResult{
			Outcome:    engine.StartUnknown,
			WorkloadID: request.Identity.WorkloadID,
			Detail:     *lost,
		}, nil
	}
	return engine.StartResult{
		Outcome:    engine.StartAccepted,
		WorkloadID: request.Identity.WorkloadID,
		Detail:     "accepted",
	}, nil
}

// Observe returns the next queued observation for the identity, or a default
// derived from the fake's own knowledge. An unknown workload stays unknown; a
// prepared but never started workload is affirmative proof of nonexecution.
func (r *Runtime) Observe(ctx context.Context, request engine.ObserveRequest) (kernel.Observation, error) {
	if err := ctx.Err(); err != nil {
		return kernel.Observation{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	record, ok := r.workloads[request.Identity.WorkloadID]
	if !ok {
		return kernel.UnknownObservation(), nil
	}
	if len(record.observations) > 0 {
		observation := record.observations[0]
		record.observations = record.observations[1:]
		return cloneObservation(observation), nil
	}
	switch {
	case record.removed:
		return kernel.AbsentObservation(), nil
	case record.started:
		return kernel.RunningObservation(), nil
	default:
		return kernel.NeverStarted("runtime fake: no start request was issued for this workload"), nil
	}
}

// Logs returns ordered chunks at or after AfterOffset, bounded by Limit. Reads
// carry no lifecycle authority and return defensive copies.
func (r *Runtime) Logs(ctx context.Context, request engine.LogsRequest) (engine.LogBatch, error) {
	if err := ctx.Err(); err != nil {
		return engine.LogBatch{}, err
	}
	if request.Limit <= 0 {
		return engine.LogBatch{}, fault.New(fault.InvalidInput, "logs limit must be positive")
	}
	if request.AfterOffset < 0 {
		return engine.LogBatch{}, fault.New(fault.InvalidInput, "logs after-offset must not be negative")
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	record, ok := r.workloads[request.Identity.WorkloadID]
	if !ok {
		return engine.LogBatch{}, fault.New(fault.NotFound, fmt.Sprintf("workload %q is unknown", request.Identity.WorkloadID))
	}
	batch := engine.LogBatch{NextOffset: request.AfterOffset}
	for _, chunk := range record.logs {
		if chunk.Offset < request.AfterOffset {
			continue
		}
		if len(batch.Chunks) >= request.Limit {
			break
		}
		copied := chunk
		copied.Data = append([]byte(nil), chunk.Data...)
		batch.Chunks = append(batch.Chunks, copied)
		batch.NextOffset = chunk.Offset + int64(len(chunk.Data))
	}
	return batch, nil
}

// Stop requests termination. An acknowledgment is never confirmation; only a
// staged confirmation produces confirmed evidence.
func (r *Runtime) Stop(ctx context.Context, request engine.StopRequest) (kernel.StopEvidence, error) {
	if err := ctx.Err(); err != nil {
		return kernel.StopEvidence{}, err
	}

	r.mu.Lock()
	record, ok := r.workloads[request.Identity.WorkloadID]
	if !ok {
		r.mu.Unlock()
		return kernel.StopEvidence{}, ErrNotPrepared
	}
	if record.stopConfirmed {
		r.mu.Unlock()
		return kernel.ConfirmedStop("workload stop was already confirmed"), nil
	}
	if r.failStopBeforeEffect != nil {
		err := r.failStopBeforeEffect
		r.failStopBeforeEffect = nil
		r.mu.Unlock()
		return kernel.StopEvidence{}, err
	}
	record.stopRequests++
	if r.confirmStop != nil {
		detail := *r.confirmStop
		r.confirmStop = nil
		record.stopConfirmed = true
		r.mu.Unlock()
		return kernel.ConfirmedStop(detail), nil
	}
	barrier := r.blockStopAfter
	r.blockStopAfter = nil
	lost := r.loseStopResponse
	r.loseStopResponse = nil
	r.mu.Unlock()

	if barrier != nil {
		if err := barrier.Wait(ctx); err != nil {
			return kernel.PendingStop("stop response lost after the request; termination is not confirmed"), nil
		}
	}
	if lost != nil {
		return kernel.PendingStop(*lost), nil
	}
	return kernel.PendingStop("stop request acknowledged; termination is not confirmed"), nil
}

// Remove performs idempotent cleanup. A prepared but never started workload has
// no live process, so it is affirmatively removable without stop evidence. A
// started workload still requires a confirmed stop: an acknowledgment, absence
// or lease expiry never substitutes for confirmation. A failed attempt is
// recorded and stays retryable.
func (r *Runtime) Remove(ctx context.Context, request engine.RemoveRequest) (engine.Removal, error) {
	if err := ctx.Err(); err != nil {
		return engine.Removal{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	record, ok := r.workloads[request.Identity.WorkloadID]
	if !ok {
		return engine.Removal{}, ErrNotPrepared
	}
	if record.started && !record.stopConfirmed {
		return engine.Removal{}, ErrStopUnconfirmed
	}
	if record.removed {
		return engine.Removal{Removed: true, Detail: "already removed"}, nil
	}
	r.removals = append(r.removals, request)
	if r.failRemove != nil {
		err := r.failRemove
		r.failRemove = nil
		return engine.Removal{}, err
	}
	record.removed = true
	return engine.Removal{Removed: true, Detail: "removed"}, nil
}

// RefreshCredential replaces token material without replaying or restarting the
// workload. A failed delivery is attributable and retryable.
func (r *Runtime) RefreshCredential(ctx context.Context, request engine.CredentialRequest) (engine.CredentialRefresh, error) {
	if err := ctx.Err(); err != nil {
		return engine.CredentialRefresh{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	record, ok := r.workloads[request.Identity.WorkloadID]
	if !ok {
		return engine.CredentialRefresh{}, ErrNotPrepared
	}
	if r.failCredential != nil {
		err := r.failCredential
		r.failCredential = nil
		return engine.CredentialRefresh{Delivered: false}, err
	}
	record.credentialRefreshes++
	return engine.CredentialRefresh{Delivered: true, Detail: "delivered"}, nil
}

// AppendLog appends data to one stream, splitting it into bounded ordered
// chunks. The caller's slice is copied.
func (r *Runtime) AppendLog(workloadID id.WorkloadID, stream engine.LogStream, data []byte) error {
	if stream != engine.LogStdout && stream != engine.LogStderr {
		return fault.New(fault.InvalidInput, fmt.Sprintf("unknown log stream %q", stream))
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	record, ok := r.workloads[workloadID]
	if !ok {
		return fault.New(fault.NotFound, fmt.Sprintf("workload %q is unknown", workloadID))
	}
	for len(data) > 0 {
		size := min(len(data), MaxLogChunkBytes)
		record.logs = append(record.logs, engine.LogChunk{
			Stream:   stream,
			Offset:   record.nextOffset,
			Sequence: record.nextSequence,
			Data:     append([]byte(nil), data[:size]...),
		})
		record.nextOffset += int64(size)
		record.nextSequence++
		data = data[size:]
	}
	return nil
}

// QueueObservation stages observations consumed in order by Observe.
func (r *Runtime) QueueObservation(workloadID id.WorkloadID, observations ...kernel.Observation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.workloads[workloadID]
	if !ok {
		return
	}
	for _, observation := range observations {
		record.observations = append(record.observations, cloneObservation(observation))
	}
}

// Workload returns a snapshot of one resource record.
func (r *Runtime) Workload(workloadID id.WorkloadID) (Workload, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, ok := r.workloads[workloadID]
	if !ok {
		return Workload{}, false
	}
	return Workload{
		Prepared:            true,
		Started:             record.started,
		StartCount:          record.startCount,
		StopRequests:        record.stopRequests,
		StopConfirmed:       record.stopConfirmed,
		LeaseExpired:        record.leaseExpired,
		Removed:             record.removed,
		CredentialRefreshes: record.credentialRefreshes,
	}, true
}

// PrepareCount returns the number of successful materializations.
func (r *Runtime) PrepareCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.prepares
}

// StartCount returns the number of applied start effects.
func (r *Runtime) StartCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.starts
}

// Removals returns the recorded removal attempts in order.
func (r *Runtime) Removals() []engine.RemoveRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]engine.RemoveRequest(nil), r.removals...)
}

// OwnershipHeld reports ErrOwnershipHeld while a workload for the workspace has
// not reached a confirmed stop and has not been affirmatively removed. Lease
// expiry and absence are not confirmation; a removed workload no longer owns
// the workspace.
func (r *Runtime) OwnershipHeld(workspaceID id.WorkspaceID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, record := range r.workloads {
		if record.workspaceID == workspaceID && !record.removed && !record.stopConfirmed {
			return ErrOwnershipHeld
		}
	}
	return nil
}

// ExpireLease marks the controller lease expired. It never confirms a stop.
func (r *Runtime) ExpireLease(workloadID id.WorkloadID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if record, ok := r.workloads[workloadID]; ok {
		record.leaseExpired = true
	}
}

// ConfirmNextStop stages affirmative termination evidence for the next stop.
func (r *Runtime) ConfirmNextStop(detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.confirmStop = &detail
}

// FailNextPrepare injects one materialization failure.
func (r *Runtime) FailNextPrepare(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failPrepare = injectedError(err, "prepare")
}

// FailNextStartBeforeEffect injects one start failure before the effect.
func (r *Runtime) FailNextStartBeforeEffect(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failStartBeforeEffect = injectedError(err, "start")
}

// RefuseNextStart injects one start refusal with affirmative proof that the
// command never executed. The refusal applies no effect, leaves the workload
// unstarted and does not consume the identity, so a later start is permitted.
func (r *Runtime) RefuseNextStart(detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refuseStart = &detail
}

// LoseNextStartResponse injects one lost start response after the effect.
func (r *Runtime) LoseNextStartResponse(detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loseStartResponse = &detail
}

// BlockNextStartBeforeEffect stages a barrier before the start effect.
func (r *Runtime) BlockNextStartBeforeEffect(barrier *Barrier) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.blockStartBefore = barrier
}

// BlockNextStartAfterEffect stages a barrier after the start effect.
func (r *Runtime) BlockNextStartAfterEffect(barrier *Barrier) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.blockStartAfter = barrier
}

// FailNextStopBeforeEffect injects one stop failure before the request effect.
func (r *Runtime) FailNextStopBeforeEffect(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failStopBeforeEffect = injectedError(err, "stop")
}

// LoseNextStopResponse injects one lost stop response after the request effect.
func (r *Runtime) LoseNextStopResponse(detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loseStopResponse = &detail
}

// BlockNextStopAfterEffect stages a barrier after the stop request effect.
func (r *Runtime) BlockNextStopAfterEffect(barrier *Barrier) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.blockStopAfter = barrier
}

// FailNextRemove injects one cleanup failure. The attempt stays recorded.
func (r *Runtime) FailNextRemove(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failRemove = injectedError(err, "remove")
}

// FailNextCredential injects one credential-delivery failure.
func (r *Runtime) FailNextCredential(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failCredential = injectedError(err, "credential delivery")
}

func injectedError(err error, operation string) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("testkit: injected %s failure", operation)
}
