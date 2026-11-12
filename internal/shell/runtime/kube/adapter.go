// Kubernetes runtime adapter operations.
//
// Every operation consumes the persisted engine.EffectIdentity. Start may only
// create the one stable Job; a lost response stays unknown and a retry reuses
// the same Job. Stop suspends that same UID-bound Job and confirms only from
// affirmative evidence; it never deletes the Job and never fabricates a stop
// barrier. Remove is idempotent, attributed and bounded.
package kube

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/kernel/script"
	"github.com/optimiware/kodama/internal/shell/workspace"
)

// Log-source classification sentinels. The adapter wraps the failing inner
// operation in one of them so a caller can name the cause without parsing a
// message or reading a response body. They stay private causes; the public
// rendering remains a bounded, secret-free message.
var (
	// ErrPodListUnavailable reports that the workload pod list could not be read.
	ErrPodListUnavailable = errors.New("kubernetes: workload pod state is unavailable")
	// ErrPodLogNotFound reports that the pod log endpoint no longer has the pod.
	ErrPodLogNotFound = errors.New("kubernetes: workload pod log is not found")
	// ErrPodLogBadRequest reports a rejected pod-log request, including a
	// container that is still waiting to start.
	ErrPodLogBadRequest = errors.New("kubernetes: workload pod log request was rejected")
	// ErrPodLogServerError reports an API-server failure (HTTP 5.x) while
	// reading the pod log; the outcome is unknown but retryable. A transport
	// failure without a status stays the generic log-read failure.
	ErrPodLogServerError = errors.New("kubernetes: workload pod log read failed")
	// ErrRedactionSourceUnavailable reports that the configured values needed to
	// redact output could not be read, so no output may be emitted.
	ErrRedactionSourceUnavailable = errors.New("kubernetes: log redaction source is unavailable")
)

// checkPreparedWorkspace refuses a request that names a different workspace
// than the one prepared in this process. It cannot prove anything after a
// restart, but it never silently rebinds a live preparation.
func (a *Adapter) checkPreparedWorkspace(identity engine.EffectIdentity, workspaceID id.WorkspaceID) error {
	a.mu.Lock()
	prepared, ok := a.prepared[identity.WorkloadID]
	a.mu.Unlock()
	if ok && prepared.workspace != workspaceID {
		return fault.New(fault.Forbidden, "workspace does not match the prepared workspace")
	}
	return nil
}

// Prepare materializes the activation snapshot once and attaches the existing
// retained workspace claim. It never starts a workload and never recreates a
// missing workspace.
func (a *Adapter) Prepare(ctx context.Context, request engine.PrepareRequest) (engine.Preparation, error) {
	if err := validateIdentity(request.Identity); err != nil {
		return engine.Preparation{}, err
	}
	if err := validateWorkspaceID(request.WorkspaceID); err != nil {
		return engine.Preparation{}, err
	}
	if err := validatePrepareContext(request.ActionName, request.TaskID); err != nil {
		return engine.Preparation{}, err
	}
	if strings.TrimSpace(request.Container.Image) == "" {
		return engine.Preparation{}, fault.New(fault.InvalidInput, "container image is empty")
	}
	for index, entry := range request.Container.Env {
		if _, err := id.ParseEnvName(string(entry.Name)); err != nil {
			return engine.Preparation{}, fault.Wrap(fault.InvalidInput,
				fmt.Sprintf("container environment entry %d is invalid", index), err)
		}
	}
	renderedScript, err := script.Render(request.Container.Commands)
	if err != nil {
		return engine.Preparation{}, err
	}
	materialization, err := buildMaterialization(request)
	if err != nil {
		return engine.Preparation{}, err
	}
	if err := ctx.Err(); err != nil {
		return engine.Preparation{}, fault.Wrap(fault.DependencyUnavailable, "preparation cancelled before any effect", err)
	}

	digest := a.preparationDigest(request, materialization)

	unlock := a.lockWorkspace(request.WorkspaceID)
	defer unlock()

	a.mu.Lock()
	existingPrepared, alreadyPrepared := a.prepared[request.Identity.WorkloadID]
	a.mu.Unlock()
	if alreadyPrepared {
		if existingPrepared.workspace != request.WorkspaceID {
			return engine.Preparation{}, fault.New(fault.Forbidden, "workspace does not match the prepared workspace")
		}
		if existingPrepared.identity != request.Identity || existingPrepared.digest != digest {
			return engine.Preparation{}, fault.New(fault.ChangedIdempotencyContent,
				"activation snapshot content changed for the same workload identity")
		}
	}

	namespace, err := a.cluster.GetNamespace(ctx, a.namespace)
	if err != nil {
		return engine.Preparation{}, fault.Wrap(fault.DependencyUnavailable, "tenant namespace is unavailable", err)
	}
	if namespace.Labels[LabelTenant] != string(a.tenant) {
		return engine.Preparation{}, fault.New(fault.Forbidden, "namespace is not owned by this tenant")
	}

	claimName, err := WorkspaceClaimName(request.WorkspaceID)
	if err != nil {
		return engine.Preparation{}, err
	}
	claim, err := a.cluster.GetClaim(ctx, a.namespace, claimName)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return engine.Preparation{}, fault.New(fault.NotFound, "retained workspace claim is missing; preparation never recreates it")
		}
		return engine.Preparation{}, fault.Wrap(fault.DependencyUnavailable, "retained workspace claim is unreachable", err)
	}
	if claim.Labels[LabelWorkspace] != string(request.WorkspaceID) {
		return engine.Preparation{}, fault.New(fault.Forbidden, "workspace claim is not owned by this workspace")
	}

	configMap := a.activationConfigMap(request.Identity, request.WorkspaceID, materialization, digest)
	if err := a.checkDeclaredSecretContent(ctx, request.Identity, materialization); err != nil {
		return engine.Preparation{}, err
	}
	storedConfigMap, err := a.cluster.PutConfigMap(ctx, configMap)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			return engine.Preparation{}, fault.New(fault.ChangedIdempotencyContent, "activation snapshot content changed for the same workload identity")
		}
		return engine.Preparation{}, fault.Wrap(fault.DependencyUnavailable, "activation snapshot write outcome is unknown", err)
	}
	if err := checkStoredActivationConfigMap(storedConfigMap, request.Identity, request.WorkspaceID, string(a.tenant), digest); err != nil {
		return engine.Preparation{}, err
	}

	secret := a.credentialSecret(request.Identity, request.WorkspaceID, materialization)
	if _, err := a.cluster.PutSecret(ctx, secret); err != nil {
		return engine.Preparation{}, fault.Wrap(fault.DependencyUnavailable, "credential secret write outcome is unknown", err)
	}

	activationPaths := make([]string, 0, len(materialization.Files))
	for _, file := range materialization.Files {
		activationPaths = append(activationPaths, file.Path)
	}
	sort.Strings(activationPaths)
	secretNames := make([]id.SecretName, 0, len(materialization.Secrets))
	for _, declared := range materialization.Secrets {
		secretNames = append(secretNames, declared.Name)
	}

	prepared := preparedWorkload{
		identity:   request.Identity,
		digest:     digest,
		workspace:  request.WorkspaceID,
		actionName: request.ActionName,
		taskID:     request.TaskID,
		container: containerDeclaration{
			image:  request.Container.Image,
			script: renderedScript,
			env:    copyContainerEnv(request.Container.Env),
		},
		claimName:       claimName,
		configMap:       configMap.Name,
		secret:          secret.Name,
		job:             workloadJobName(request.Identity.WorkloadID),
		activationPaths: activationPaths,
		secretNames:     secretNames,
	}
	a.mu.Lock()
	a.prepared[request.Identity.WorkloadID] = prepared
	a.mu.Unlock()

	return engine.Preparation{
		WorkspaceID: request.WorkspaceID,
		Detail: fmt.Sprintf("materialized activation snapshot %q and credential secret %q for %s",
			configMap.Name, secret.Name, describeIdentity(request.Identity)),
	}, nil
}

// checkStoredActivationConfigMap verifies the durable ConfigMap labels match
// the requested identity, workspace and preparation digest before any Secret
// update or in-memory rebinding occurs.
func checkStoredActivationConfigMap(stored ConfigMap, identity engine.EffectIdentity, workspaceID id.WorkspaceID, tenant, digest string) error {
	if stored.Labels[LabelTenant] != tenant {
		return fault.New(fault.Forbidden, "activation snapshot is not owned by this tenant")
	}
	if stored.Labels[LabelWorkspace] != string(workspaceID) {
		return fault.New(fault.Forbidden, "workspace does not match the prepared workspace")
	}
	if stored.Labels[LabelWorkload] != string(identity.WorkloadID) ||
		stored.Labels[LabelExecution] != string(identity.ExecutionID) ||
		stored.Labels[LabelAction] != string(identity.ActionID) ||
		stored.Labels[LabelPreparation] != digest {
		return fault.New(fault.ChangedIdempotencyContent,
			"activation snapshot content changed for the same workload identity")
	}
	return nil
}

// copyContainerEnv copies the admitted literal environment so later mutation of
// the request cannot change an already-prepared workload.
func copyContainerEnv(env []definition.EnvVar) []EnvVar {
	copied := make([]EnvVar, len(env))
	for index, entry := range env {
		copied[index] = EnvVar{Name: string(entry.Name), Value: entry.Value}
	}
	return copied
}

// checkDeclaredSecretContent refuses different declared secret values for an
// already-prepared identity. The execution credential is deliberately excluded:
// RefreshCredential may rotate it without a restart.
func (a *Adapter) checkDeclaredSecretContent(ctx context.Context, identity engine.EffectIdentity, materialization workspace.Materialization) error {
	existing, err := a.cluster.GetSecret(ctx, a.namespace, credentialSecretName(identity.WorkloadID))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return fault.Wrap(fault.DependencyUnavailable, "credential secret is unreachable", err)
	}
	expected := make(map[string]string, len(materialization.Secrets))
	for _, declared := range materialization.Secrets {
		expected[string(declared.Name)] = declared.Value
	}
	for name, value := range existing.Data {
		if name == id.SecretTokenName {
			continue
		}
		want, ok := expected[name]
		if !ok || string(value) != want {
			return fault.New(fault.ChangedIdempotencyContent, "declared secret content changed for the same workload identity")
		}
	}
	for name, want := range expected {
		got, ok := existing.Data[name]
		if !ok || string(got) != want {
			return fault.New(fault.ChangedIdempotencyContent, "declared secret content changed for the same workload identity")
		}
	}
	return nil
}

// Start creates the one stable Job for a prepared identity. A lost create
// response stays unknown; a retry reuses the same Job and never replays a fresh
// command.
func (a *Adapter) Start(ctx context.Context, request engine.StartRequest) (engine.StartResult, error) {
	unknown := func(detail string, err error) (engine.StartResult, error) {
		return engine.StartResult{
			Outcome:    engine.StartUnknown,
			WorkloadID: request.Identity.WorkloadID,
			Detail:     detail,
		}, err
	}
	if err := validateIdentity(request.Identity); err != nil {
		return unknown("start request identity is invalid", err)
	}
	if err := validateWorkspaceID(request.WorkspaceID); err != nil {
		return unknown("start request workspace is invalid", err)
	}
	if err := ctx.Err(); err != nil {
		return unknown("start cancelled before any effect", fault.Wrap(fault.DependencyUnavailable, "start cancelled before any effect", err))
	}

	unlock := a.lockWorkspace(request.WorkspaceID)
	defer unlock()

	a.mu.Lock()
	prepared, ok := a.prepared[request.Identity.WorkloadID]
	a.mu.Unlock()

	if !ok {
		// Recovery after a process restart: an existing stable Job is accepted
		// rather than replaced. Without a Job there is no complete preparation,
		// so Start refuses instead of preparing implicitly.
		existing, err := a.cluster.GetJob(ctx, a.namespace, workloadJobName(request.Identity.WorkloadID))
		if err == nil {
			if existing.Annotations[AnnotationIdempotencyKey] != request.Identity.IdempotencyKey {
				return unknown("existing Job carries a different idempotency key", fault.New(fault.ChangedIdempotencyContent, "existing Job carries a different idempotency key"))
			}
			if existing.Labels[LabelWorkspace] != string(request.WorkspaceID) {
				return unknown("existing Job is bound to a different workspace", fault.New(fault.Forbidden, "existing Job is bound to a different workspace"))
			}
			return engine.StartResult{
				Outcome:    engine.StartAccepted,
				WorkloadID: request.Identity.WorkloadID,
				Detail:     fmt.Sprintf("reused existing stable Job %q", existing.Name),
			}, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return unknown("Job state is unreachable", fault.Wrap(fault.DependencyUnavailable, "Job state is unreachable", err))
		}
		return unknown("start requires a completed preparation", fault.New(fault.InvalidInput, "start requires a completed preparation"))
	}

	if prepared.workspace != request.WorkspaceID {
		return unknown("start workspace does not match the prepared workspace", fault.New(fault.InvalidInput, "start workspace does not match the prepared workspace"))
	}
	if prepared.identity != request.Identity {
		return unknown("start identity does not match the prepared identity", fault.New(fault.ChangedIdempotencyContent, "start identity does not match the prepared identity"))
	}
	claim, err := a.cluster.GetClaim(ctx, a.namespace, prepared.claimName)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return unknown("retained workspace claim is missing", fault.New(fault.NotFound, "retained workspace claim is missing; start never recreates it"))
		}
		return unknown("retained workspace claim is unreachable", fault.Wrap(fault.DependencyUnavailable, "retained workspace claim is unreachable", err))
	}
	if claim.Labels[LabelWorkspace] != string(request.WorkspaceID) {
		return unknown("workspace claim is not owned by this workspace", fault.New(fault.Forbidden, "workspace claim is not owned by this workspace"))
	}
	if _, err := a.cluster.GetConfigMap(ctx, a.namespace, prepared.configMap); err != nil {
		return unknown("activation snapshot is unavailable", fault.Wrap(fault.DependencyUnavailable, "activation snapshot is unavailable", err))
	}
	if _, err := a.cluster.GetSecret(ctx, a.namespace, prepared.secret); err != nil {
		return unknown("credential secret is unavailable", fault.Wrap(fault.DependencyUnavailable, "credential secret is unavailable", err))
	}

	existing, err := a.cluster.GetJob(ctx, a.namespace, prepared.job)
	switch {
	case err == nil:
		if existing.Annotations[AnnotationIdempotencyKey] != request.Identity.IdempotencyKey {
			return unknown("existing Job carries a different idempotency key", fault.New(fault.ChangedIdempotencyContent, "existing Job carries a different idempotency key"))
		}
		if existing.Labels[LabelWorkspace] != string(request.WorkspaceID) {
			return unknown("existing Job is bound to a different workspace", fault.New(fault.Forbidden, "existing Job is bound to a different workspace"))
		}
		return engine.StartResult{
			Outcome:    engine.StartAccepted,
			WorkloadID: request.Identity.WorkloadID,
			Detail:     fmt.Sprintf("reused existing stable Job %q", existing.Name),
		}, nil
	case errors.Is(err, ErrNotFound):
		// Create below.
	default:
		return unknown("Job state is unreachable", fault.Wrap(fault.DependencyUnavailable, "Job state is unreachable", err))
	}

	job := a.jobManifest(prepared, request.Identity)
	if _, err := a.cluster.CreateJob(ctx, job); err != nil {
		if errors.Is(err, ErrAlreadyExists) {
			if existing, getErr := a.cluster.GetJob(ctx, a.namespace, prepared.job); getErr == nil {
				if existing.Annotations[AnnotationIdempotencyKey] != request.Identity.IdempotencyKey {
					return unknown("existing Job carries a different idempotency key", fault.New(fault.ChangedIdempotencyContent, "existing Job carries a different idempotency key"))
				}
				if existing.Labels[LabelWorkspace] != string(request.WorkspaceID) {
					return unknown("existing Job is bound to a different workspace", fault.New(fault.Forbidden, "existing Job is bound to a different workspace"))
				}
				return engine.StartResult{
					Outcome:    engine.StartAccepted,
					WorkloadID: request.Identity.WorkloadID,
					Detail:     fmt.Sprintf("reused concurrently created stable Job %q", prepared.job),
				}, nil
			}
		}
		return unknown("Job creation outcome is unknown", fault.Wrap(fault.UncertainExternalEffect, "Job creation outcome is unknown", err))
	}
	return engine.StartResult{
		Outcome:    engine.StartAccepted,
		WorkloadID: request.Identity.WorkloadID,
		Detail:     fmt.Sprintf("created stable Job %q", job.Name),
	}, nil
}

// Observe reports the declared Kernel vocabulary for the stable identity.
// Absence and node loss stay uncertain.
func (a *Adapter) Observe(ctx context.Context, request engine.ObserveRequest) (kernel.Observation, error) {
	if err := validateIdentity(request.Identity); err != nil {
		return kernel.UnknownObservation(), err
	}
	if err := validateWorkspaceID(request.WorkspaceID); err != nil {
		return kernel.UnknownObservation(), err
	}
	if err := a.checkPreparedWorkspace(request.Identity, request.WorkspaceID); err != nil {
		return kernel.UnknownObservation(), err
	}

	jobFound := false
	_, err := a.cluster.GetJob(ctx, a.namespace, workloadJobName(request.Identity.WorkloadID))
	switch {
	case err == nil:
		jobFound = true
	case errors.Is(err, ErrNotFound):
		jobFound = false
	default:
		return kernel.UnknownObservation(), fault.Wrap(fault.DependencyUnavailable, "Job state is unreachable", err)
	}

	pods, err := a.cluster.ListPods(ctx, a.namespace, workloadSelector(request.Identity))
	if err != nil {
		return kernel.UnknownObservation(), fault.Wrap(fault.DependencyUnavailable, "pod state is unreachable", err)
	}
	return observationFor(jobFound, pods), nil
}

// Logs returns ordered, bounded and redacted chunks. It carries no lifecycle
// authority and fails closed when the redaction source is unavailable.
func (a *Adapter) Logs(ctx context.Context, request engine.LogsRequest) (engine.LogBatch, error) {
	if err := validateIdentity(request.Identity); err != nil {
		return engine.LogBatch{}, err
	}
	if request.Limit <= 0 || request.Limit > MaxLogBytes {
		return engine.LogBatch{}, fault.New(fault.InvalidInput, "log read limit is outside the bounded range")
	}
	if request.AfterOffset < 0 {
		return engine.LogBatch{}, fault.New(fault.InvalidInput, "log read offset is negative")
	}
	// The pod log is one interleaved stream: stock clusters cannot separate
	// stdout from stderr, so the combined log is attributed to stdout and the
	// stderr position of the continuation token stays zero.
	offset, stderrOffset := decodeLogCursor(request.AfterOffset)
	if stderrOffset != 0 {
		// A cursor from the retired two-stream reader carries a stderr
		// position. Reinterpreting its high half as a combined-log offset would
		// silently skip or repeat bytes, so refuse it as an invalid cursor
		// before touching the cluster.
		return engine.LogBatch{}, fault.New(fault.InvalidInput,
			"log cursor carries a stderr position; the combined log has one position")
	}

	pods, err := a.cluster.ListPods(ctx, a.namespace, workloadSelector(request.Identity))
	if err != nil {
		return engine.LogBatch{}, fault.Wrap(fault.DependencyUnavailable, "pod state is unreachable",
			errors.Join(ErrPodListUnavailable, err))
	}
	if len(pods) != 1 {
		return engine.LogBatch{}, fault.New(fault.NotFound, fmt.Sprintf("log read requires exactly one workload pod; observed %d", len(pods)))
	}
	pod := pods[0]

	secret, err := a.cluster.GetSecret(ctx, a.namespace, credentialSecretName(request.Identity.WorkloadID))
	if err != nil {
		// Never emit output when the configured values cannot be redacted. A
		// missing redaction source is not container absence: it stays a
		// retryable, fail-closed dependency fault. A NotFound cause is dropped so
		// no errors.Is(ErrNotFound) caller can mistake it for a vanished
		// container.
		cause := ErrRedactionSourceUnavailable
		if !errors.Is(err, ErrNotFound) {
			cause = errors.Join(cause, err)
		}
		return engine.LogBatch{}, fault.Wrap(fault.DependencyUnavailable, "redaction source is unavailable; log read refused", cause)
	}
	values := redactionValues(secret)

	page, err := a.cluster.ReadPodLogs(ctx, a.namespace, pod.Name, PodLogRequest{
		Stream: LogStreamCombined,
		Offset: offset,
		Limit:  request.Limit,
	})
	if err != nil {
		// A pod that vanished after the successful list keeps NotFound so the
		// collector records its explicit unavailable marker. Every other log
		// failure stays retryable: a terminated container's log can still be
		// readable, and waiting-to-start, rejection, server and transport
		// failures are not proof that output was never produced.
		if errors.Is(err, ErrNotFound) {
			return engine.LogBatch{}, fault.Wrap(fault.NotFound, "pod log is unavailable",
				errors.Join(ErrPodLogNotFound, err))
		}
		switch {
		case errors.Is(err, ErrPodLogBadRequest):
			return engine.LogBatch{}, fault.Wrap(fault.DependencyUnavailable, "pod log read was rejected",
				errors.Join(ErrPodLogBadRequest, err))
		case errors.Is(err, ErrPodLogServerError):
			return engine.LogBatch{}, fault.Wrap(fault.DependencyUnavailable, "pod log read is unavailable",
				errors.Join(ErrPodLogServerError, err))
		default:
			return engine.LogBatch{}, fault.Wrap(fault.DependencyUnavailable, "pod log read is unavailable", err)
		}
	}
	// A truncated page is a strict prefix of the log: a value that starts
	// inside it may still complete after the page. Only the final page (no
	// truncation) is known to hold the whole remaining log, so nothing there can
	// extend past its last byte.
	consumed, data := redactLogPage(page.Data, values, request.Limit, page.Truncated)
	next, err := nextLogCursor(offset, len(consumed))
	if err != nil {
		// The page itself was already read, but a position that does not fit the
		// token must never surface as a negative cursor.
		return engine.LogBatch{}, err
	}
	batch := engine.LogBatch{NextOffset: next}
	if len(consumed) == 0 {
		return batch, nil
	}
	batch.Chunks = append(batch.Chunks, engine.LogChunk{
		Stream:    engine.LogStdout,
		Offset:    offset,
		Sequence:  1,
		Data:      data,
		Truncated: page.Truncated,
	})
	return batch, nil
}

// logSegment is one decoded piece of a pod-log page: a raw byte range and the
// bytes it contributes after redaction (a raw copy or the fixed marker).
type logSegment struct {
	start  int
	end    int
	marker bool
	data   []byte
}

// scanLogSegments splits a page into raw runs and configured-value matches, left
// to right and longest value first (redactionValues orders them that way). A
// value is consumed atomically, so no page boundary can be placed inside one.
func scanLogSegments(data []byte, values []string) []logSegment {
	segments := make([]logSegment, 0, len(data))
	for index := 0; index < len(data); {
		matched := false
		for _, value := range values {
			if value == "" || len(value) > len(data)-index {
				continue
			}
			if string(data[index:index+len(value)]) == value {
				segments = append(segments, logSegment{
					start:  index,
					end:    index + len(value),
					marker: true,
					data:   []byte(Redacted),
				})
				index += len(value)
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		segments = append(segments, logSegment{
			start: index,
			end:   index + 1,
			data:  []byte{data[index]},
		})
		index++
	}
	return segments
}

// couldExtend reports whether tail is a proper prefix of a configured value, so
// a longer match could still start there once the log grows past this page. The
// whole visible tail must match, not just the bytes already recognised: every
// byte the page does contain is fixed in the log.
func couldExtend(tail []byte, values []string) bool {
	for _, value := range values {
		if len(value) <= len(tail) {
			continue
		}
		if hasStringPrefix(value, tail) {
			return true
		}
	}
	return false
}

// hasStringPrefix reports whether value starts with prefix without allocating.
func hasStringPrefix(value string, prefix []byte) bool {
	if len(prefix) > len(value) {
		return false
	}
	for index := range prefix {
		if value[index] != prefix[index] {
			return false
		}
	}
	return true
}

// redactLogPage redacts one bounded page without splitting a configured value
// across the page boundary. It returns the raw prefix the cursor may advance
// over and the redacted bytes to emit. mayGrow says the page is a strict prefix
// of the log: more bytes exist past it, so a match near its end may still be
// extended by a longer configured value on a later page.
//
// The scan walks the page left to right and stops at the first segment whose
// remaining tail is a proper prefix of a configured value. That includes a
// marker for a shorter value that a longer nested value could still extend, so
// a marker is emitted only once no longer value can claim the position. The
// cursor then names the held position and the next page re-reads the run whole.
//
// When the page is the end of the log nothing can extend it, so a final partial
// prefix of a value is emitted as ordinary raw bytes instead of being lost.
// When the log may still grow and the whole page is held, the call returns no
// progress and no data: the raw bytes stay unread and the same cursor is
// retried. A value longer than the page can never be advanced past safely under
// that bound; a caller that wants progress beyond such a value must raise its
// limit.
//
// Three limits of this byte-level reader are deliberate and documented rather
// than fixed here:
//
//   - A live log that splits a value between two writes can still split it
//     across polls when the first poll's page ends inside the value: the reader
//     can only see the bytes written so far. The retained sink's cross-chunk
//     redactor remains the backstop for that case.
//   - Pages are byte-bounded, so a multi-byte UTF-8 rune can be split across
//     two chunks. That is existing transport behaviour; redaction never splits
//     a configured value.
//   - A paginated read is quadratic in the number of pages: the driver asks the
//     log API for the stream and discards the cursor prefix, so each page costs
//     one offset-sized read. Memory stays bounded at one page, limit+1 bytes.
//
// Redaction can expand a value into a longer marker. Whole segments are dropped
// from the end until the emitted bytes fit the bound, so the cursor re-reads
// them; only a marker longer than the entire bound is clipped, and its raw bytes
// are still consumed. Neither case drops raw content.
func redactLogPage(raw []byte, values []string, limit int, mayGrow bool) ([]byte, []byte) {
	segments := scanLogSegments(raw, values)
	end := len(segments)
	hold := end
	if mayGrow {
		for index, segment := range segments {
			if couldExtend(raw[segment.start:], values) {
				end = index
				hold = index
				break
			}
		}
	}

	size := 0
	for _, segment := range segments[:end] {
		size += len(segment.data)
	}
	for end > 0 && size > limit {
		end--
		size -= len(segments[end].data)
	}
	if end == 0 {
		if len(segments) == 0 || hold == 0 {
			// Nothing before the held position resolved, so no raw value bytes
			// must be emitted and no bytes may be skipped.
			return nil, nil
		}
		// Only reachable when the first segment is a marker that alone exceeds
		// the bound. A held first segment returned above, so this marker is
		// complete: its bytes may be clipped and the value consumed, while any
		// held tail after it is re-read from the cursor.
		first := segments[0]
		data := first.data
		if len(data) > limit {
			data = data[:limit]
		}
		return raw[:first.end], data
	}

	consumed := raw[:segments[end-1].end]
	data := make([]byte, 0, size)
	for _, segment := range segments[:end] {
		data = append(data, segment.data...)
	}
	return consumed, data
}

// logCursorStreamBits splits the single log continuation token into two 32-bit
// halves: the high half carries the combined-log byte position, the low half is
// the retired stderr position and is always zero for cursors this reader
// produces. The packing is unchanged so the token stays an opaque int64.
const logCursorStreamBits = 32

const logCursorStreamMask int64 = (1 << logCursorStreamBits) - 1

// decodeLogCursor returns the combined position and the retired stderr half of
// one token. A non-zero stderr half is a legacy two-stream cursor.
func decodeLogCursor(cursor int64) (stdout, stderr int64) {
	return cursor >> logCursorStreamBits, cursor & logCursorStreamMask
}

// encodeLogCursor packs the combined position into one continuation token.
func encodeLogCursor(stdout, stderr int64) int64 {
	return stdout<<logCursorStreamBits | stderr
}

// maxLogCursorPosition is the largest combined-log byte position the high half
// of the continuation token can carry. encodeLogCursor shifts the position by
// 32 bits, so a position at or above 2^31 sets the sign bit and would surface
// as a negative cursor.
const maxLogCursorPosition int64 = (1 << 31) - 1

// nextLogCursor packs the next combined-log position, refusing one the token
// cannot represent instead of returning a cursor the caller cannot resume.
func nextLogCursor(offset int64, consumed int) (int64, error) {
	if offset < 0 || offset > maxLogCursorPosition ||
		int64(consumed) > maxLogCursorPosition-offset {
		return 0, fault.New(fault.InvalidInput,
			"log position exceeds the continuation-token range")
	}
	return encodeLogCursor(offset+int64(consumed), 0), nil
}

// Stop requests suspension of the one stable Job and reports a stop barrier
// only from affirmative evidence. The PATCH is UID-guarded intent, not
// confirmation: the same Job must be observed again without a deletion
// timestamp, with spec.suspend true, a controller acknowledgment (or an already
// terminal Job), zero active pods and a present zero terminating count, and a
// fresh workload-pod list must have succeeded with no workload pod. A missing
// Job, a replaced Job, a resumed Job and a failed read all stay pending.
//
// The same UID-guarded PATCH clears spec.ttlSecondsAfterFinished, so a Job that
// finishes while suspended is not removed by the TTL controller before Remove
// deletes it. The independent affirmative terminated-container path is preserved
// unchanged, including for a Job that already carries a deletion timestamp: such
// a Job is never suspension evidence, but its workload pods are still listed and
// an affirmatively terminated container still confirms. Absence of pods is never
// stop evidence.
func (a *Adapter) Stop(ctx context.Context, request engine.StopRequest) (kernel.StopEvidence, error) {
	if err := validateIdentity(request.Identity); err != nil {
		return kernel.PendingStop("stop request identity is invalid"), err
	}
	if err := validateWorkspaceID(request.WorkspaceID); err != nil {
		return kernel.PendingStop("stop request workspace is invalid"), err
	}
	if err := a.checkPreparedWorkspace(request.Identity, request.WorkspaceID); err != nil {
		return kernel.PendingStop("stop request workspace does not match the prepared workspace"), err
	}

	jobName := workloadJobName(request.Identity.WorkloadID)
	before, err := a.cluster.GetJob(ctx, a.namespace, jobName)
	switch {
	case err == nil:
	case errors.Is(err, ErrNotFound):
		// A missing Job is never suspension evidence, but the independent
		// terminated-container evidence path still applies. An absent Job with
		// no retained termination evidence can never confirm.
		pods, listErr := a.cluster.ListPods(ctx, a.namespace, workloadSelector(request.Identity))
		if listErr != nil {
			return kernel.PendingStop("stop unconfirmed: pod state is unreachable"),
				fault.Wrap(fault.DependencyUnavailable, "pod state is unreachable", listErr)
		}
		return stopEvidenceFor(pods), nil
	default:
		return kernel.PendingStop("stop unconfirmed: Job state is unreachable"),
			fault.Wrap(fault.DependencyUnavailable, "Job state is unreachable", err)
	}

	if problem := stopJobIdentityMismatch(before, request.Identity, request.WorkspaceID, a.namespace, string(a.tenant)); problem != "" {
		return kernel.PendingStop("stop unconfirmed: " + problem),
			fault.New(fault.Forbidden, problem)
	}
	if before.DeletionTimestamp != nil {
		// A Job already being deleted (a foreground or TTL deletion that started
		// before this Stop) can never confirm a suspension, and the PATCH is
		// refused. The independent terminated-container path still applies: list
		// the workload pods and confirm only from affirmative termination.
		// Absence of pods is never stop evidence, and a running or terminating
		// pod without terminated-container evidence stays pending.
		pods, listErr := a.cluster.ListPods(ctx, a.namespace, workloadSelector(request.Identity))
		if listErr != nil {
			return kernel.PendingStop("stop unconfirmed: pod state is unreachable"),
				fault.Wrap(fault.DependencyUnavailable, "pod state is unreachable", listErr)
		}
		return stopEvidenceFor(pods), nil
	}
	expectedUID := before.UID

	if _, suspendErr := a.cluster.SuspendJob(ctx, a.namespace, jobName, expectedUID); suspendErr != nil {
		switch {
		case errors.Is(suspendErr, ErrNotFound):
			return kernel.PendingStop("stop unconfirmed: the Job disappeared before suspension"),
				fault.Wrap(fault.NotFound, "Job disappeared before suspension", suspendErr)
		case errors.Is(suspendErr, ErrConflict):
			return kernel.PendingStop("stop unconfirmed: the Job identity changed before suspension"),
				fault.Wrap(fault.VersionConflict, "Job identity changed before suspension", suspendErr)
		default:
			// A definite 4xx refusal (for example the server's immutable-UID
			// validation) is not an unknown outcome: nothing was suspended. Keep
			// that class so cleanup does not spend its bound on a retry that can
			// never succeed. Transport loss and 5xx stay uncertain.
			if class, ok := fault.ClassOf(suspendErr); ok && class == fault.DependencyUnavailable {
				return kernel.PendingStop("stop unconfirmed: Job suspension was refused"),
					fault.Wrap(fault.DependencyUnavailable, "Job suspension was refused", suspendErr)
			}
			return kernel.PendingStop("stop unconfirmed: Job suspension outcome is unknown"),
				fault.Wrap(fault.UncertainExternalEffect, "Job suspension outcome is unknown", suspendErr)
		}
	}

	// Fresh observation bracketing the pod list: the intent PATCH is not
	// evidence, so the acknowledged state must be read back. A failed read
	// never confirms.
	mid, err := a.cluster.GetJob(ctx, a.namespace, jobName)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return kernel.PendingStop("stop unconfirmed: the Job disappeared during confirmation"), nil
		}
		return kernel.PendingStop("stop unconfirmed: Job state is unreachable"),
			fault.Wrap(fault.DependencyUnavailable, "Job state is unreachable", err)
	}
	pods, err := a.cluster.ListPods(ctx, a.namespace, workloadSelector(request.Identity))
	if err != nil {
		return kernel.PendingStop("stop unconfirmed: pod state is unreachable"),
			fault.Wrap(fault.DependencyUnavailable, "pod state is unreachable", err)
	}
	if len(pods) != 0 {
		// Preserve the existing independent evidence path: one affirmatively
		// terminated workload container confirms; anything else stays pending.
		return stopEvidenceFor(pods), nil
	}
	after, err := a.cluster.GetJob(ctx, a.namespace, jobName)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return kernel.PendingStop("stop unconfirmed: the Job disappeared during confirmation"), nil
		}
		return kernel.PendingStop("stop unconfirmed: Job state is unreachable"),
			fault.Wrap(fault.DependencyUnavailable, "Job state is unreachable", err)
	}
	if suspensionConfirmed(before, mid, after, request.Identity, request.WorkspaceID, a.namespace, string(a.tenant), expectedUID) {
		return kernel.ConfirmedStop(fmt.Sprintf("Job %q is suspended with no workload pod and an acknowledged zero active/terminating count", jobName)), nil
	}
	return kernel.PendingStop("stop unconfirmed: the suspended Job has not settled into an affirmed stop"), nil
}

// Remove is idempotent, attributed and bounded cleanup. It never releases or
// deletes the retained workspace claim; engine.Workspace.Release owns that
// decision. A live or unreachable workload retains the claim and names it in
// Detail.
func (a *Adapter) Remove(ctx context.Context, request engine.RemoveRequest) (engine.Removal, error) {
	if err := validateIdentity(request.Identity); err != nil {
		return engine.Removal{Detail: "removal request identity is invalid"}, err
	}
	if err := validateWorkspaceID(request.WorkspaceID); err != nil {
		return engine.Removal{Detail: "removal request workspace is invalid"}, err
	}
	if err := a.checkPreparedWorkspace(request.Identity, request.WorkspaceID); err != nil {
		return engine.Removal{Detail: "removal request workspace does not match the prepared workspace"}, err
	}

	unlock := a.lockWorkspace(request.WorkspaceID)
	defer unlock()

	claimName, err := WorkspaceClaimName(request.WorkspaceID)
	if err != nil {
		return engine.Removal{Detail: "workspace claim name is invalid"}, err
	}

	pods, err := a.cluster.ListPods(ctx, a.namespace, workloadSelector(request.Identity))
	if err != nil {
		return engine.Removal{Detail: retainedDetail(claimName, "pod state is unreachable")}, nil
	}
	if !workspaceReleasable(pods) {
		// A live, terminating or unknown workload can still write to the
		// workspace. Retain every owned resource and name the claim.
		return engine.Removal{Detail: retainedDetail(claimName, "a workload pod is still live")}, nil
	}

	if err := a.deleteWithRetry(ctx, func(ctx context.Context) error {
		return a.cluster.DeleteJob(ctx, a.namespace, workloadJobName(request.Identity.WorkloadID))
	}); err != nil {
		return engine.Removal{Detail: retainedDetail(claimName, "Job deletion failed: "+err.Error())}, nil
	}
	if err := a.deleteWithRetry(ctx, func(ctx context.Context) error {
		return a.cluster.DeleteConfigMap(ctx, a.namespace, activationConfigMapName(request.Identity.WorkloadID))
	}); err != nil {
		return engine.Removal{Detail: retainedDetail(claimName, "activation snapshot deletion failed: "+err.Error())}, nil
	}
	if err := a.deleteWithRetry(ctx, func(ctx context.Context) error {
		return a.cluster.DeleteSecret(ctx, a.namespace, credentialSecretName(request.Identity.WorkloadID))
	}); err != nil {
		return engine.Removal{Detail: retainedDetail(claimName, "credential secret deletion failed: "+err.Error())}, nil
	}
	a.mu.Lock()
	delete(a.prepared, request.Identity.WorkloadID)
	a.mu.Unlock()
	// Remove never releases the retained workspace claim. Only
	// engine.Workspace.Release may delete it, after every owned identity is
	// confirmed removed.
	return engine.Removal{
		Removed: true,
		Detail:  fmt.Sprintf("removed owned workload resources; retained workspace claim %q awaits Workspace.Release", claimName),
	}, nil
}

// deleteWithRetry bounds deletion attempts per call with the injected clock.
func (a *Adapter) deleteWithRetry(ctx context.Context, remove func(context.Context) error) error {
	attempts := max(a.cleanup.Attempts, 1)
	var lastErr error
	for attempt := range attempts {
		if attempt > 0 {
			if err := a.clock.Sleep(ctx, a.cleanup.Backoff); err != nil {
				return err
			}
		}
		err := remove(ctx)
		if err == nil || errors.Is(err, ErrNotFound) {
			return nil
		}
		lastErr = err
	}
	return lastErr
}

// retainedDetail names the retained workspace claim so pending cleanup is
// attributable.
func retainedDetail(claimName, reason string) string {
	return fmt.Sprintf("cleanup pending: retained workspace claim %q: %s", claimName, reason)
}

// RefreshCredential replaces only the mutable credential Secret. It never
// restarts or replaces the Job and is recoverable after a lost update response
// or a stale resource version.
func (a *Adapter) RefreshCredential(ctx context.Context, request engine.CredentialRequest) (engine.CredentialRefresh, error) {
	if err := validateIdentity(request.Identity); err != nil {
		return engine.CredentialRefresh{}, err
	}
	if strings.TrimSpace(request.Credential) == "" {
		return engine.CredentialRefresh{}, fault.New(fault.InvalidInput, "credential refresh requires a non-empty credential")
	}

	secret, err := a.cluster.GetSecret(ctx, a.namespace, credentialSecretName(request.Identity.WorkloadID))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return engine.CredentialRefresh{}, fault.New(fault.NotFound, "credential secret is missing")
		}
		return engine.CredentialRefresh{}, fault.Wrap(fault.DependencyUnavailable, "credential secret is unreachable", err)
	}

	data := make(map[string][]byte, len(secret.Data))
	for name, value := range secret.Data {
		data[name] = append([]byte(nil), value...)
	}
	data[id.SecretTokenName] = []byte(request.Credential)
	secret.Data = data
	if _, err := a.cluster.PutSecret(ctx, secret); err != nil {
		if errors.Is(err, ErrConflict) {
			return engine.CredentialRefresh{Detail: "credential update used a stale resource version"},
				fault.Wrap(fault.VersionConflict, "credential update used a stale resource version", err)
		}
		return engine.CredentialRefresh{Detail: "credential update outcome is unknown"},
			fault.Wrap(fault.UncertainExternalEffect, "credential update outcome is unknown", err)
	}
	return engine.CredentialRefresh{
		Delivered: true,
		Detail:    fmt.Sprintf("replaced credential for %s without restarting the Job", describeIdentity(request.Identity)),
	}, nil
}
