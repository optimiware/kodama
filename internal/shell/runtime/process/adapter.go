// Process runtime adapter operations.
//
// Every operation consumes the persisted engine.EffectIdentity. Prepare
// materializes one confined all-or-nothing activation snapshot outside the
// execution workspace. Start may only spawn the one stable process group; a
// lost response stays unknown and a retry never replays the command. Stop
// acknowledges a signal but confirms only affirmative whole-group termination.
// Remove is idempotent, attributed and bounded.
package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/kernel/script"
	"github.com/optimiware/kodama/internal/shell/workspace"
)

// Prepare materializes the activation snapshot once and attaches the existing
// execution workspace. It never starts a process and never recreates a missing
// workspace.
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
	renderedScript, err := prepareScript(request.Container.Commands)
	if err != nil {
		return engine.Preparation{}, err
	}
	env, err := finalEnvironment(request, a.kodamaURL)
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

	a.mu.Lock()
	defer a.mu.Unlock()

	resourceRoot, err := ResourceRoot(a.root, request.Identity.WorkloadID)
	if err != nil {
		return engine.Preparation{}, err
	}
	tombstoned, err := a.tombstoned(ctx, request.Identity.WorkloadID)
	if err != nil {
		return engine.Preparation{}, err
	}
	if tombstoned {
		return engine.Preparation{}, fault.New(fault.Forbidden, "workload identity was removed and cannot be prepared again")
	}
	workspaceDir, err := WorkspaceDir(a.workspaceRoot, request.WorkspaceID)
	if err != nil {
		return engine.Preparation{}, err
	}
	if err := a.requireWorkspace(ctx, workspaceDir); err != nil {
		return engine.Preparation{}, err
	}

	digest := snapshotDigest(request, env, materialization)
	existing, found, err := a.readManifest(ctx, resourceRoot)
	if err != nil {
		return engine.Preparation{}, err
	}
	if found {
		if existing.WorkspaceID != request.WorkspaceID {
			return engine.Preparation{}, fault.New(fault.Forbidden, "workspace does not match the prepared workspace")
		}
		if existing.Identity != request.Identity || existing.Digest != digest {
			return engine.Preparation{}, fault.New(fault.ChangedIdempotencyContent,
				"activation snapshot content changed for the same workload identity")
		}
		return engine.Preparation{
			WorkspaceID: request.WorkspaceID,
			Detail:      fmt.Sprintf("reused activation snapshot for %s", describeIdentity(request.Identity)),
		}, nil
	}

	if err := a.materialize(ctx, resourceRoot, materialization, renderedScript); err != nil {
		return engine.Preparation{}, err
	}
	secretNames := make([]id.SecretName, 0, len(materialization.Secrets))
	for _, secret := range materialization.Secrets {
		secretNames = append(secretNames, secret.Name)
	}
	loaded := manifest{
		Version:     1,
		Identity:    request.Identity,
		WorkspaceID: request.WorkspaceID,
		ActionName:  request.ActionName,
		TaskID:      request.TaskID,
		Image:       request.Container.Image,
		Commands:    append([]string(nil), request.Container.Commands...),
		Env:         env,
		SecretNames: secretNames,
		Digest:      digest,
	}
	if err := a.writeManifest(ctx, resourceRoot, loaded); err != nil {
		return engine.Preparation{}, err
	}
	return engine.Preparation{
		WorkspaceID: request.WorkspaceID,
		Detail:      fmt.Sprintf("materialized activation snapshot for %s", describeIdentity(request.Identity)),
	}, nil
}

// materialize writes every prepared file, the generated script and the mutable
// credential. The caller writes the completion manifest last.
func (a *Adapter) materialize(ctx context.Context, resourceRoot string, materialization workspace.Materialization, renderedScript string) error {
	for _, file := range materialization.Files {
		path := hostPath(resourceRoot, file.Path)
		if err := a.ensureConfinedDir(ctx, filepath.Dir(path)); err != nil {
			return err
		}
		if err := a.files.WriteFile(ctx, path, file.Data, readOnlyMode); err != nil {
			return fault.Wrap(fault.DependencyUnavailable, "activation file write failed", err)
		}
	}
	for _, secret := range materialization.Secrets {
		path := hostPath(resourceRoot, workspace.SecretRoot+"/"+string(secret.Name))
		if err := a.ensureConfinedDir(ctx, filepath.Dir(path)); err != nil {
			return err
		}
		if err := a.files.WriteFile(ctx, path, []byte(secret.Value), readOnlyMode); err != nil {
			return fault.Wrap(fault.DependencyUnavailable, "declared secret write failed", err)
		}
	}
	if err := a.ensureConfinedDir(ctx, resourceRoot); err != nil {
		return err
	}
	if err := a.files.WriteFile(ctx, scriptPath(resourceRoot), []byte(renderedScript), readOnlyMode); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "fail-fast script write failed", err)
	}
	credentialPath := hostPath(resourceRoot, workspace.ReservedCredentialPath)
	if err := a.ensureConfinedDir(ctx, filepath.Dir(credentialPath)); err != nil {
		return err
	}
	if err := a.files.WriteFile(ctx, credentialPath, []byte(materialization.Credential), readOnlyMode); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "execution credential write failed", err)
	}
	if err := a.ensureConfinedDir(ctx, filepath.Join(resourceRoot, logsDirectoryName)); err != nil {
		return err
	}
	return nil
}

// Start spawns the one stable process group for a prepared identity. A retry,
// lost response or concurrent duplicate never spawns a second process.
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
		return unknown("start cancelled before any effect",
			fault.Wrap(fault.DependencyUnavailable, "start cancelled before any effect", err))
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	resourceRoot, err := ResourceRoot(a.root, request.Identity.WorkloadID)
	if err != nil {
		return unknown("start request identity is invalid", err)
	}
	tombstoned, err := a.tombstoned(ctx, request.Identity.WorkloadID)
	if err != nil {
		return unknown("removal tombstone is unreachable", err)
	}
	if tombstoned {
		return unknown("workload identity was removed and cannot be started again",
			fault.New(fault.Forbidden, "workload identity was removed and cannot be started again"))
	}
	loaded, found, err := a.readManifest(ctx, resourceRoot)
	if err != nil {
		return unknown("activation snapshot is unreadable", err)
	}
	if !found {
		return unknown("start requires a completed preparation",
			fault.New(fault.InvalidInput, "start requires a completed preparation"))
	}
	if loaded.WorkspaceID != request.WorkspaceID {
		return unknown("start workspace does not match the prepared workspace",
			fault.New(fault.Forbidden, "start workspace does not match the prepared workspace"))
	}
	if loaded.Identity != request.Identity {
		return unknown("activation snapshot belongs to a different identity",
			fault.New(fault.ChangedIdempotencyContent, "activation snapshot belongs to a different identity"))
	}
	workspaceDir, err := WorkspaceDir(a.workspaceRoot, request.WorkspaceID)
	if err != nil {
		return unknown("start request workspace is invalid", err)
	}
	if err := a.requireWorkspace(ctx, workspaceDir); err != nil {
		return unknown("execution workspace is unavailable", err)
	}
	group, err := GroupName(request.Identity.WorkloadID)
	if err != nil {
		return unknown("process group name is invalid", err)
	}

	state, stateFound, err := a.readGroupState(ctx, resourceRoot)
	if err != nil {
		return unknown("process-group bookkeeping is unreadable", err)
	}
	if stateFound {
		if state.Identity != request.Identity {
			return unknown("process-group bookkeeping belongs to a different identity",
				fault.New(fault.ChangedIdempotencyContent, "process-group bookkeeping belongs to a different identity"))
		}
		switch state.State {
		case groupStarted:
			if _, err := a.observer.Status(ctx, group); err != nil {
				return unknown("process-group state is unreachable",
					fault.Wrap(fault.DependencyUnavailable, "process-group state is unreachable", err))
			}
			return engine.StartResult{
				Outcome:    engine.StartAccepted,
				WorkloadID: request.Identity.WorkloadID,
				Detail:     fmt.Sprintf("reused existing process group %q", group),
			}, nil
		case groupNeverStarted:
			return engine.StartResult{
				Outcome:    engine.StartProvenNotStarted,
				WorkloadID: request.Identity.WorkloadID,
				Detail:     nonEmptyProof(state.Detail),
			}, nil
		case groupSpawning, groupUnknown:
			status, statusErr := a.observer.Status(ctx, group)
			if statusErr != nil {
				return unknown("process-group state is unreachable",
					fault.Wrap(fault.DependencyUnavailable, "process-group state is unreachable", statusErr))
			}
			if status.NeverCreated {
				proof := nonEmptyProof(state.Detail)
				neverStarted := groupState{
					Group:    group,
					Identity: request.Identity,
					State:    groupNeverStarted,
					Detail:   proof,
				}
				if err := a.writeGroupState(ctx, resourceRoot, neverStarted); err != nil {
					return unknown("affirmative never-created evidence could not be persisted", err)
				}
				return engine.StartResult{
					Outcome:    engine.StartProvenNotStarted,
					WorkloadID: request.Identity.WorkloadID,
					Detail:     proof,
				}, nil
			}
			if status.Present || status.MembersAlive || status.LeaderExited || status.GroupGone {
				return engine.StartResult{
					Outcome:    engine.StartAccepted,
					WorkloadID: request.Identity.WorkloadID,
					Detail:     fmt.Sprintf("reused existing process group %q", group),
				}, nil
			}
			return unknown("spawn intent exists but the outcome is unknown",
				fault.New(fault.UncertainExternalEffect, "spawn intent exists but the outcome is unknown"))
		}
	}

	// Persist the spawn intent before the effect so a crash cannot produce a
	// second process group.
	intent := groupState{
		Group:    group,
		Identity: request.Identity,
		State:    groupSpawning,
		Detail:   "spawn intent persisted",
	}
	if err := a.writeGroupState(ctx, resourceRoot, intent); err != nil {
		return unknown("spawn intent could not be persisted", err)
	}

	result, spawnErr := a.executor.Spawn(ctx, SpawnRequest{
		Identity:   request.Identity,
		Group:      group,
		Command:    []string{script.ShellPath, scriptPath(resourceRoot)},
		Env:        append([]string(nil), loaded.Env...),
		Dir:        workspaceDir,
		Root:       resourceRoot,
		StdoutPath: stdoutPath(resourceRoot),
		StderrPath: stderrPath(resourceRoot),
	})

	switch {
	case spawnErr == nil && result.Outcome == SpawnStarted:
		started := groupState{
			Group:    group,
			Identity: request.Identity,
			State:    groupStarted,
			Detail:   "process group started",
		}
		if err := a.writeGroupState(ctx, resourceRoot, started); err != nil {
			return unknown("spawn outcome could not be persisted", err)
		}
		return engine.StartResult{
			Outcome:    engine.StartAccepted,
			WorkloadID: request.Identity.WorkloadID,
			Detail:     fmt.Sprintf("started process group %q", group),
		}, nil
	case spawnErr == nil && result.Outcome == SpawnRefused:
		proof := nonEmptyProof(result.Detail)
		neverStarted := groupState{
			Group:    group,
			Identity: request.Identity,
			State:    groupNeverStarted,
			Detail:   proof,
		}
		if err := a.writeGroupState(ctx, resourceRoot, neverStarted); err != nil {
			return unknown("affirmative spawn refusal could not be persisted", err)
		}
		return engine.StartResult{
			Outcome:    engine.StartProvenNotStarted,
			WorkloadID: request.Identity.WorkloadID,
			Detail:     proof,
		}, nil
	default:
		detail := strings.TrimSpace(result.Detail)
		if detail == "" {
			detail = "process spawn outcome is unknown"
		}
		uncertain := groupState{
			Group:    group,
			Identity: request.Identity,
			State:    groupUnknown,
			Detail:   detail,
		}
		_ = a.writeGroupState(ctx, resourceRoot, uncertain)
		if spawnErr != nil {
			return unknown(detail, fault.Wrap(fault.UncertainExternalEffect, "process spawn outcome is unknown", spawnErr))
		}
		return unknown(detail, fault.New(fault.UncertainExternalEffect, "process spawn outcome is unknown"))
	}
}

// Observe reports the declared Kernel vocabulary for the stable identity.
func (a *Adapter) Observe(ctx context.Context, request engine.ObserveRequest) (kernel.Observation, error) {
	if err := validateIdentity(request.Identity); err != nil {
		return kernel.UnknownObservation(), err
	}
	if err := validateWorkspaceID(request.WorkspaceID); err != nil {
		return kernel.UnknownObservation(), err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	resourceRoot, err := ResourceRoot(a.root, request.Identity.WorkloadID)
	if err != nil {
		return kernel.UnknownObservation(), err
	}
	loaded, found, err := a.readManifest(ctx, resourceRoot)
	if err != nil {
		return kernel.UnknownObservation(), err
	}
	if !found {
		// An unprepared or already removed identity is absent. Absence is never
		// proof of nonexecution.
		return kernel.AbsentObservation(), nil
	}
	if loaded.WorkspaceID != request.WorkspaceID {
		return kernel.UnknownObservation(), fault.New(fault.Forbidden, "observation workspace does not match the prepared workspace")
	}
	if loaded.Identity != request.Identity {
		return kernel.UnknownObservation(), fault.New(fault.ChangedIdempotencyContent, "observation identity does not match the prepared identity")
	}
	group, err := GroupName(request.Identity.WorkloadID)
	if err != nil {
		return kernel.UnknownObservation(), err
	}
	state, _, err := a.readGroupState(ctx, resourceRoot)
	if err != nil {
		return kernel.UnknownObservation(), err
	}
	status, err := a.observer.Status(ctx, group)
	if err != nil {
		return kernel.UnknownObservation(), fault.Wrap(fault.DependencyUnavailable, "process-group state is unreachable", err)
	}
	return observationFor(state, status), nil
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

	a.mu.Lock()
	defer a.mu.Unlock()

	resourceRoot, err := ResourceRoot(a.root, request.Identity.WorkloadID)
	if err != nil {
		return engine.LogBatch{}, err
	}
	loaded, found, err := a.readManifest(ctx, resourceRoot)
	if err != nil {
		return engine.LogBatch{}, err
	}
	if !found {
		return engine.LogBatch{}, fault.New(fault.NotFound, "log read requires a prepared activation snapshot")
	}
	if loaded.Identity != request.Identity {
		return engine.LogBatch{}, fault.New(fault.ChangedIdempotencyContent, "log request identity does not match the prepared identity")
	}
	values, err := a.redactionSource(ctx, resourceRoot, loaded)
	if err != nil {
		return engine.LogBatch{}, err
	}
	group, err := GroupName(request.Identity.WorkloadID)
	if err != nil {
		return engine.LogBatch{}, err
	}

	stdoutOffset, stderrOffset := decodeLogCursor(request.AfterOffset)
	positions := map[engine.LogStream]int64{
		engine.LogStdout: stdoutOffset,
		engine.LogStderr: stderrOffset,
	}
	batch := engine.LogBatch{}
	sequence := int64(0)
	for _, stream := range []engine.LogStream{engine.LogStdout, engine.LogStderr} {
		offset := positions[stream]
		page, err := a.observer.Logs(ctx, GroupLogsRequest{
			Group:  group,
			Stream: stream,
			Offset: offset,
			Limit:  request.Limit,
		})
		if err != nil {
			return engine.LogBatch{}, fault.Wrap(fault.DependencyUnavailable, "process log read is unavailable", err)
		}
		mayGrow := page.Truncated && len(page.Data) == request.Limit
		consumed, data := redactLogPage(page.Data, values, request.Limit, mayGrow)
		nextPosition, err := nextStreamLogPosition(stream, offset, len(consumed))
		if err != nil {
			return engine.LogBatch{}, err
		}
		positions[stream] = nextPosition
		if len(consumed) == 0 {
			continue
		}
		sequence++
		batch.Chunks = append(batch.Chunks, engine.LogChunk{
			Stream:    stream,
			Offset:    offset,
			Sequence:  sequence,
			Data:      data,
			Truncated: page.Truncated || len(consumed) < len(page.Data),
		})
	}
	batch.NextOffset = encodeLogCursor(positions[engine.LogStdout], positions[engine.LogStderr])
	return batch, nil
}

// redactionSource loads the current credential and every declared secret value.
// A missing source fails the whole read closed.
func (a *Adapter) redactionSource(ctx context.Context, resourceRoot string, loaded manifest) ([]string, error) {
	values := make([]string, 0, len(loaded.SecretNames)+1)
	for _, name := range loaded.SecretNames {
		data, err := a.files.ReadFile(ctx, hostPath(resourceRoot, workspace.SecretRoot+"/"+string(name)))
		if err != nil {
			return nil, fault.Wrap(fault.DependencyUnavailable, "redaction source is unavailable; log read refused", err)
		}
		values = append(values, string(data))
	}
	credential, err := a.files.ReadFile(ctx, hostPath(resourceRoot, workspace.ReservedCredentialPath))
	if err != nil {
		return nil, fault.Wrap(fault.DependencyUnavailable, "redaction source is unavailable; log read refused", err)
	}
	values = append(values, string(credential))
	return redactionValues(values), nil
}

// Stop requests whole-process-group termination. A signal acknowledgment is
// not confirmation; only affirmative whole-group termination confirms.
func (a *Adapter) Stop(ctx context.Context, request engine.StopRequest) (kernel.StopEvidence, error) {
	if err := validateIdentity(request.Identity); err != nil {
		return kernel.PendingStop("stop request identity is invalid"), err
	}
	if err := validateWorkspaceID(request.WorkspaceID); err != nil {
		return kernel.PendingStop("stop request workspace is invalid"), err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	resourceRoot, err := ResourceRoot(a.root, request.Identity.WorkloadID)
	if err != nil {
		return kernel.PendingStop("stop request identity is invalid"), err
	}
	tombstoned, err := a.tombstoned(ctx, request.Identity.WorkloadID)
	if err != nil {
		return kernel.PendingStop("stop unconfirmed: removal tombstone is unreachable"), err
	}
	if tombstoned {
		return kernel.PendingStop("stop unconfirmed: workload identity was already removed"),
			fault.New(fault.NotFound, "workload identity was already removed")
	}
	loaded, found, err := a.readManifest(ctx, resourceRoot)
	if err != nil {
		return kernel.PendingStop("stop unconfirmed: activation snapshot is unreadable"), err
	}
	if !found {
		return kernel.PendingStop("stop unconfirmed: workload identity is not prepared"),
			fault.New(fault.NotFound, "workload identity is not prepared")
	}
	if loaded.WorkspaceID != request.WorkspaceID {
		return kernel.PendingStop("stop unconfirmed: workspace does not match the prepared workspace"),
			fault.New(fault.Forbidden, "workspace does not match the prepared workspace")
	}
	group, err := GroupName(request.Identity.WorkloadID)
	if err != nil {
		return kernel.PendingStop("stop request identity is invalid"), err
	}
	if err := a.executor.SignalGroup(ctx, group); err != nil {
		return kernel.PendingStop("stop unconfirmed: process-group signal outcome is unknown"),
			fault.Wrap(fault.UncertainExternalEffect, "process-group signal outcome is unknown", err)
	}
	status, err := a.observer.Status(ctx, group)
	if err != nil {
		return kernel.PendingStop("stop unconfirmed: process-group state is unreachable"),
			fault.Wrap(fault.DependencyUnavailable, "process-group state is unreachable", err)
	}
	if status.GroupGone {
		return kernel.ConfirmedStop(fmt.Sprintf("process group %q affirmatively terminated", group)), nil
	}
	return kernel.PendingStop(fmt.Sprintf("stop pending: process group %q has not affirmatively terminated", group)), nil
}

// Remove is idempotent, identity-scoped and bounded cleanup. It never touches
// the execution workspace and refuses to remove resources while the process
// group may still run.
func (a *Adapter) Remove(ctx context.Context, request engine.RemoveRequest) (engine.Removal, error) {
	if err := validateIdentity(request.Identity); err != nil {
		return engine.Removal{Detail: "removal request identity is invalid"}, err
	}
	if err := validateWorkspaceID(request.WorkspaceID); err != nil {
		return engine.Removal{Detail: "removal request workspace is invalid"}, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	resourceRoot, err := ResourceRoot(a.root, request.Identity.WorkloadID)
	if err != nil {
		return engine.Removal{Detail: "removal request identity is invalid"}, err
	}
	group, err := GroupName(request.Identity.WorkloadID)
	if err != nil {
		return engine.Removal{Detail: "removal request identity is invalid"}, err
	}
	rootExists, err := a.pathExists(ctx, resourceRoot)
	if err != nil {
		return engine.Removal{Detail: retainedDetail(group, "resource root is unreachable")}, nil
	}
	tombstoned, err := a.tombstoned(ctx, request.Identity.WorkloadID)
	if err != nil {
		return engine.Removal{Detail: retainedDetail(group, "removal tombstone is unreachable")}, nil
	}
	if tombstoned {
		if !rootExists {
			return engine.Removal{
				Removed: true,
				Detail:  fmt.Sprintf("owned resources for %s were already removed", describeIdentity(request.Identity)),
			}, nil
		}
		return a.cleanupRoot(ctx, resourceRoot, group)
	}

	loaded, found, err := a.readManifest(ctx, resourceRoot)
	if err != nil {
		return engine.Removal{Detail: retainedDetail(group, "activation snapshot is unreadable")}, nil
	}
	if !found {
		if !rootExists {
			return engine.Removal{
				Removed: true,
				Detail:  fmt.Sprintf("no owned resources exist for %s", describeIdentity(request.Identity)),
			}, nil
		}
		if err := a.writeTombstone(ctx, request.Identity.WorkloadID); err != nil {
			return engine.Removal{Detail: retainedDetail(group, "removal tombstone write failed")}, nil
		}
		return a.cleanupRoot(ctx, resourceRoot, group)
	}
	if loaded.WorkspaceID != request.WorkspaceID {
		return engine.Removal{Detail: retainedDetail(group, "workspace does not match the prepared workspace")},
			fault.New(fault.Forbidden, "workspace does not match the prepared workspace")
	}
	if loaded.Identity != request.Identity {
		return engine.Removal{Detail: retainedDetail(group, "identity does not match the prepared identity")},
			fault.New(fault.ChangedIdempotencyContent, "identity does not match the prepared identity")
	}

	state, _, err := a.readGroupState(ctx, resourceRoot)
	if err != nil {
		return engine.Removal{Detail: retainedDetail(group, "process-group bookkeeping is unreadable")}, nil
	}
	switch state.State {
	case groupStarted:
		status, statusErr := a.observer.Status(ctx, group)
		if statusErr != nil {
			return engine.Removal{Detail: retainedDetail(group, "process-group state is unreachable")}, nil
		}
		if !status.GroupGone {
			return engine.Removal{Detail: retainedDetail(group, "the process group has not affirmatively terminated")}, nil
		}
		if err := a.executor.RemoveGroup(ctx, group); err != nil {
			return engine.Removal{Detail: retainedDetail(group, "process-group bookkeeping removal failed")}, nil
		}
	case groupSpawning, groupUnknown:
		status, statusErr := a.observer.Status(ctx, group)
		if statusErr != nil {
			return engine.Removal{Detail: retainedDetail(group, "process-group state is unreachable")}, nil
		}
		switch {
		case status.NeverCreated:
			// Affirmative no-effect evidence: there is no group to signal or
			// remove, so the tombstone and cleanup may proceed.
		case status.GroupGone:
			if err := a.executor.RemoveGroup(ctx, group); err != nil {
				return engine.Removal{Detail: retainedDetail(group, "process-group bookkeeping removal failed")}, nil
			}
		default:
			// Present, alive or zero evidence: the group may still run.
			return engine.Removal{Detail: retainedDetail(group, "the process-group spawn outcome is unknown")}, nil
		}
	}

	if err := a.writeTombstone(ctx, request.Identity.WorkloadID); err != nil {
		return engine.Removal{Detail: retainedDetail(group, "removal tombstone write failed")}, nil
	}
	return a.cleanupRoot(ctx, resourceRoot, group)
}

// cleanupRoot removes only the identity's resource root with bounded retries.
func (a *Adapter) cleanupRoot(ctx context.Context, resourceRoot string, group GroupID) (engine.Removal, error) {
	attempts := max(a.cleanup.Attempts, 1)
	var lastErr error
	for attempt := range attempts {
		if attempt > 0 {
			if err := a.clock.Sleep(ctx, a.cleanup.Backoff); err != nil {
				return engine.Removal{Detail: retainedDetail(group, "cleanup backoff interrupted")}, err
			}
		}
		if err := a.files.RemoveAll(ctx, resourceRoot); err != nil {
			lastErr = err
			continue
		}
		return engine.Removal{
			Removed: true,
			Detail:  fmt.Sprintf("removed owned resources and process group %q", group),
		}, nil
	}
	reason := "resource removal failed"
	if lastErr != nil {
		reason = "resource removal failed: " + lastErr.Error()
	}
	return engine.Removal{Detail: retainedDetail(group, reason)}, nil
}

// pathExists reports whether one host path exists.
func (a *Adapter) pathExists(ctx context.Context, path string) (bool, error) {
	_, err := a.files.Stat(ctx, path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// retainedDetail names the retained process group so pending cleanup is
// attributable.
func retainedDetail(group GroupID, reason string) string {
	return fmt.Sprintf("cleanup pending: retained process group %q: %s", group, reason)
}

// RefreshCredential replaces only the mutable mounted credential. It never
// restarts or re-signals the process group and never revives a removed
// identity.
func (a *Adapter) RefreshCredential(ctx context.Context, request engine.CredentialRequest) (engine.CredentialRefresh, error) {
	if err := validateIdentity(request.Identity); err != nil {
		return engine.CredentialRefresh{}, err
	}
	if strings.TrimSpace(request.Credential) == "" {
		return engine.CredentialRefresh{}, fault.New(fault.InvalidInput, "credential refresh requires a non-empty credential")
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	resourceRoot, err := ResourceRoot(a.root, request.Identity.WorkloadID)
	if err != nil {
		return engine.CredentialRefresh{}, err
	}
	tombstoned, err := a.tombstoned(ctx, request.Identity.WorkloadID)
	if err != nil {
		return engine.CredentialRefresh{}, err
	}
	if tombstoned {
		return engine.CredentialRefresh{Detail: "credential refresh refused for a removed identity"},
			fault.New(fault.NotFound, "credential refresh refused for a removed identity")
	}
	loaded, found, err := a.readManifest(ctx, resourceRoot)
	if err != nil {
		return engine.CredentialRefresh{}, err
	}
	if !found {
		return engine.CredentialRefresh{Detail: "credential refresh requires a prepared identity"},
			fault.New(fault.NotFound, "credential refresh requires a prepared identity")
	}
	if loaded.Identity != request.Identity {
		return engine.CredentialRefresh{Detail: "credential refresh identity does not match the prepared identity"},
			fault.New(fault.ChangedIdempotencyContent, "credential refresh identity does not match the prepared identity")
	}
	credentialPath := hostPath(resourceRoot, workspace.ReservedCredentialPath)
	if err := a.files.WriteFile(ctx, credentialPath, []byte(request.Credential), readOnlyMode); err != nil {
		return engine.CredentialRefresh{Detail: "credential update outcome is unknown"},
			fault.Wrap(fault.UncertainExternalEffect, "credential update outcome is unknown", err)
	}
	return engine.CredentialRefresh{
		Delivered: true,
		Detail:    fmt.Sprintf("replaced credential for %s without restarting the process group", describeIdentity(request.Identity)),
	}, nil
}
