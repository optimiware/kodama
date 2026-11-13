// Podman runtime adapter operations.
//
// Every operation consumes the persisted engine.EffectIdentity. Start may only
// create the one stable container; a lost create or start response stays unknown
// and a retry reuses the same container. Stop acknowledges a stop request but
// never fabricates a stop barrier. Remove is idempotent, identity-scoped and
// bounded, writes a durable removal fence outside the removed root and never
// touches the retained workspace volume.
package podman

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/workspace"
)

// fenceDirectory holds the durable removal fences. It is a sibling of the
// per-identity roots, so a fence survives the materialization it forbids.
const fenceDirectory = ".removed"

// tempSequence makes each atomic replacement use a distinct temporary name.
var tempSequence atomic.Uint64

// preparationPath is the durable marker path for one identity.
func preparationPath(root string, workload id.WorkloadID) string {
	return preparationRecordPath(root, workload)
}

// fencePath returns the durable removal fence path for one identity.
func fencePath(root string, workload id.WorkloadID) string {
	return filepath.Join(root, fenceDirectory, string(workload)+".json")
}

// isFenced reports whether the identity's removal fence exists. A fence refuses
// every later Prepare, Start, Logs and credential refresh, including after an
// adapter reconstruction.
func (a *Adapter) isFenced(ctx context.Context, workload id.WorkloadID) (bool, error) {
	_, err := a.files.Lstat(ctx, fencePath(a.root, workload))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fault.Wrap(fault.DependencyUnavailable, "removal fence is unreachable", err)
}

// writeFence persists the durable removal fence for one identity.
func (a *Adapter) writeFence(ctx context.Context, identity engine.EffectIdentity, workspaceID id.WorkspaceID) error {
	dir := filepath.Join(a.root, fenceDirectory)
	if err := a.files.MkdirAll(ctx, dir, directoryMode); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "removal fence directory write outcome is unknown", err)
	}
	payload, err := json.Marshal(struct {
		WorkloadID  id.WorkloadID  `json:"workload_id"`
		WorkspaceID id.WorkspaceID `json:"workspace_id"`
		RemovedAt   string         `json:"removed_at"`
	}{
		WorkloadID:  identity.WorkloadID,
		WorkspaceID: workspaceID,
		RemovedAt:   a.clock.Now().UTC().Format("2006-01-02T15:04:05Z07:00"),
	})
	if err != nil {
		return fault.Wrap(fault.InternalFailure, "removal fence is not encodable", err)
	}
	if err := a.files.WriteFile(ctx, fencePath(a.root, identity.WorkloadID), payload, readOnlyMode); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "removal fence write outcome is unknown", err)
	}
	return nil
}

// startAttemptRecord is the durable evidence that a StartContainer request may
// have been delivered for one identity. A libpod client cannot know whether a
// start is still in flight, so this marker — written before the request — is
// the only affirmative evidence that a start may have run.
type startAttemptRecord struct {
	WorkloadID  id.WorkloadID  `json:"workload_id"`
	WorkspaceID id.WorkspaceID `json:"workspace_id,omitempty"`
	ExecutionID id.ExecutionID `json:"execution_id"`
	ActionID    id.ActionID    `json:"action_id"`
	AttemptedAt string         `json:"attempted_at"`
}

// hasStartAttempt reports whether a StartContainer request may have been
// delivered for this identity. An unreadable marker is an error, never absence.
func (a *Adapter) hasStartAttempt(ctx context.Context, workload id.WorkloadID) (bool, error) {
	_, err := a.files.Lstat(ctx, startAttemptPath(a.root, workload))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fault.Wrap(fault.DependencyUnavailable, "start attempt state is unreachable", err)
}

// writeStartAttempt durably records the start attempt before the libpod call.
// A failed write prevents the effect: the caller stays unknown and no
// StartContainer is sent without the marker.
func (a *Adapter) writeStartAttempt(ctx context.Context, identity engine.EffectIdentity, workspaceID id.WorkspaceID) error {
	payload, err := json.Marshal(startAttemptRecord{
		WorkloadID:  identity.WorkloadID,
		WorkspaceID: workspaceID,
		ExecutionID: identity.ExecutionID,
		ActionID:    identity.ActionID,
		AttemptedAt: a.clock.Now().UTC().Format("2006-01-02T15:04:05Z07:00"),
	})
	if err != nil {
		return fault.Wrap(fault.InternalFailure, "start attempt marker is not encodable", err)
	}
	if err := a.files.WriteFile(ctx, startAttemptPath(a.root, identity.WorkloadID), payload, readOnlyMode); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "start attempt marker write outcome is unknown", err)
	}
	return nil
}

// readPreparation loads the durable preparation marker for one identity.
func (a *Adapter) readPreparation(ctx context.Context, workload id.WorkloadID) (preparationRecord, []byte, bool, error) {
	path := preparationPath(a.root, workload)
	info, err := a.files.Lstat(ctx, path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return preparationRecord{}, nil, false, nil
		}
		return preparationRecord{}, nil, false, fault.Wrap(fault.DependencyUnavailable, "preparation state is unreachable", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return preparationRecord{}, nil, false, fault.New(fault.Forbidden, "preparation marker is not a regular file")
	}
	data, err := a.files.ReadFile(ctx, path)
	if err != nil {
		return preparationRecord{}, nil, false, fault.Wrap(fault.DependencyUnavailable, "preparation state is unreadable", err)
	}
	var record preparationRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return preparationRecord{}, nil, false, fault.Wrap(fault.DependencyUnavailable, "preparation marker is not decodable", err)
	}
	return record, data, true, nil
}

// ensureDirectory creates one prepared directory, refusing a symlink or a
// non-directory at any component.
func (a *Adapter) ensureDirectory(ctx context.Context, path string) error {
	info, err := a.files.Lstat(ctx, path)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 {
			return fault.New(fault.Forbidden, "preparation path is a symlink")
		}
		if !info.IsDir() {
			return fault.New(fault.Forbidden, "preparation path is not a directory")
		}
		return nil
	case errors.Is(err, os.ErrNotExist):
		if err := a.files.MkdirAll(ctx, path, directoryMode); err != nil {
			return fault.Wrap(fault.DependencyUnavailable, "preparation directory write outcome is unknown", err)
		}
		return nil
	default:
		return fault.Wrap(fault.DependencyUnavailable, "preparation path is unreachable", err)
	}
}

// ensureConfinedDirectories creates every prepared directory one component at a
// time so a symlink anywhere below the adapter root is refused instead of
// followed.
func (a *Adapter) ensureConfinedDirectories(ctx context.Context, workload id.WorkloadID, files []preparedFile) error {
	base := workloadRoot(a.root, workload)
	if err := a.ensureDirectory(ctx, base); err != nil {
		return err
	}
	logicalPaths := make([]string, 0, len(files)+1)
	for _, file := range files {
		logicalPaths = append(logicalPaths, file.logical)
	}
	logicalPaths = append(logicalPaths, workspace.ReservedCredentialPath)

	created := map[string]bool{base: true}
	for _, logical := range logicalPaths {
		dir := filepath.Dir(hostPath(a.root, workload, logical))
		relative, err := filepath.Rel(base, dir)
		if err != nil {
			return fault.Wrap(fault.InvalidInput, "prepared path escapes the workload root", err)
		}
		if relative == "." {
			continue
		}
		current := base
		for segment := range strings.SplitSeq(relative, string(filepath.Separator)) {
			current = filepath.Join(current, segment)
			if created[current] {
				continue
			}
			if err := a.ensureDirectory(ctx, current); err != nil {
				return err
			}
			created[current] = true
		}
	}
	return nil
}

// replaceFile atomically replaces one host file: write a temporary sibling, then
// rename it over the target. A failure leaves the previous bytes intact.
func (a *Adapter) replaceFile(ctx context.Context, target string, data []byte) error {
	temp := fmt.Sprintf("%s.tmp-%d", target, tempSequence.Add(1))
	if err := a.files.WriteFile(ctx, temp, data, readOnlyMode); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "replacement write outcome is unknown", err)
	}
	if err := a.files.Rename(ctx, temp, target); err != nil {
		// The temporary file is ours and carries only the new value; removing it
		// keeps the directory clean without touching the previous token.
		_ = a.files.RemoveAll(ctx, temp)
		return fault.Wrap(fault.UncertainExternalEffect, "replacement outcome is unknown", err)
	}
	return nil
}

// writeCredential creates or atomically replaces only the reserved credential
// file. The previous token survives a failed replacement.
func (a *Adapter) writeCredential(ctx context.Context, workload id.WorkloadID, credential string) error {
	target := hostPath(a.root, workload, workspace.ReservedCredentialPath)
	existing, err := a.files.ReadFile(ctx, target)
	if err == nil && string(existing) == credential {
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fault.Wrap(fault.DependencyUnavailable, "credential file is unreachable", err)
	}
	return a.replaceFile(ctx, target, []byte(credential))
}

// verifyPreparedFiles confirms the on-disk preparation is complete before a
// start. A missing or unsafe file means the preparation never completed.
func (a *Adapter) verifyPreparedFiles(ctx context.Context, workload id.WorkloadID, record preparationRecord) error {
	paths := append([]string(nil), record.Files...)
	paths = append(paths, workspace.ReservedCredentialPath)
	for _, logical := range paths {
		host := hostPath(a.root, workload, logical)
		info, err := a.files.Lstat(ctx, host)
		if err != nil {
			return fault.Wrap(fault.DependencyUnavailable, "prepared activation file is missing", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fault.New(fault.Forbidden, "prepared activation file is not a regular file")
		}
	}
	return nil
}

// Prepare materializes the activation snapshot once and attaches the existing
// retained workspace volume. It never starts a container and never recreates a
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
	files, secrets, err := buildPreparationWithCA(request, a.workloadCAPEM)
	if err != nil {
		return engine.Preparation{}, err
	}
	if err := ctx.Err(); err != nil {
		return engine.Preparation{}, fault.Wrap(fault.DependencyUnavailable, "preparation cancelled before any effect", err)
	}

	lock := a.lockFor(request.Identity.WorkloadID)
	lock.Lock()
	defer lock.Unlock()
	workspaceLock := a.lockForWorkspace(request.WorkspaceID)
	workspaceLock.Lock()
	defer workspaceLock.Unlock()

	fenced, err := a.isFenced(ctx, request.Identity.WorkloadID)
	if err != nil {
		return engine.Preparation{}, err
	}
	if fenced {
		return engine.Preparation{}, fault.New(fault.NotFound, "workload identity has a durable removal fence")
	}

	volumeName, err := WorkspaceVolumeName(request.WorkspaceID)
	if err != nil {
		return engine.Preparation{}, err
	}
	volume, err := a.libpod.InspectVolume(ctx, volumeName)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return engine.Preparation{}, fault.New(fault.NotFound,
				"retained workspace volume is missing; preparation never recreates it")
		}
		return engine.Preparation{}, fault.Wrap(fault.DependencyUnavailable, "retained workspace volume is unreachable", err)
	}
	if volume.Labels[LabelWorkspace] != string(request.WorkspaceID) {
		return engine.Preparation{}, fault.New(fault.Forbidden, "workspace volume is not owned by this workspace")
	}

	if err := a.libpod.InspectImage(ctx, request.Container.Image); err != nil {
		if errors.Is(err, ErrNotFound) {
			return engine.Preparation{}, fault.New(fault.DependencyUnavailable,
				fmt.Sprintf("workload image %q is not present in the local container store; pull it before starting the execution", request.Container.Image))
		}
		return engine.Preparation{}, fault.Wrap(fault.DependencyUnavailable, "workload image state is unreachable", err)
	}

	if err := a.ensureConfinedDirectories(ctx, request.Identity.WorkloadID, files); err != nil {
		return engine.Preparation{}, err
	}

	record := preparationRecordFor(request, files, secrets)
	recordBytes, err := encodePreparationRecord(record)
	if err != nil {
		return engine.Preparation{}, err
	}
	_, existingRecordBytes, found, err := a.readPreparation(ctx, request.Identity.WorkloadID)
	if err != nil {
		return engine.Preparation{}, err
	}
	if found && !bytes.Equal(existingRecordBytes, recordBytes) {
		return engine.Preparation{}, fault.New(fault.ChangedIdempotencyContent,
			"preparation content changed for the same workload identity")
	}

	pending := make([]preparedFile, 0, len(files))
	for _, file := range files {
		host := hostPath(a.root, request.Identity.WorkloadID, file.logical)
		info, err := a.files.Lstat(ctx, host)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				pending = append(pending, file)
				continue
			}
			return engine.Preparation{}, fault.Wrap(fault.DependencyUnavailable, "activation snapshot is unreachable", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return engine.Preparation{}, fault.New(fault.Forbidden, "activation snapshot path is not a regular file")
		}
		existing, err := a.files.ReadFile(ctx, host)
		if err != nil {
			return engine.Preparation{}, fault.Wrap(fault.DependencyUnavailable, "activation snapshot is unreadable", err)
		}
		if !bytes.Equal(existing, file.data) {
			return engine.Preparation{}, fault.New(fault.ChangedIdempotencyContent,
				"preparation content changed for the same workload identity")
		}
	}
	for _, file := range pending {
		host := hostPath(a.root, request.Identity.WorkloadID, file.logical)
		if err := a.files.WriteFile(ctx, host, file.data, readOnlyMode); err != nil {
			return engine.Preparation{}, fault.Wrap(fault.DependencyUnavailable,
				"activation snapshot write outcome is unknown", err)
		}
	}

	if err := a.writeCredential(ctx, request.Identity.WorkloadID, request.Credential); err != nil {
		return engine.Preparation{}, err
	}
	if !found {
		if err := a.files.WriteFile(ctx, preparationPath(a.root, request.Identity.WorkloadID), recordBytes, readOnlyMode); err != nil {
			return engine.Preparation{}, fault.Wrap(fault.DependencyUnavailable,
				"preparation marker write outcome is unknown", err)
		}
	}

	return engine.Preparation{
		WorkspaceID: request.WorkspaceID,
		Detail: fmt.Sprintf("materialized %d activation files for %s",
			len(files), describeIdentity(request.Identity)),
	}, nil
}

// Start creates and starts the one stable container for a prepared identity. A
// lost create or start response stays unknown; a retry inspects and reuses the
// same container instead of replaying a fresh command.
func (a *Adapter) Start(ctx context.Context, request engine.StartRequest) (engine.StartResult, error) {
	unknown := func(detail string, err error) (engine.StartResult, error) {
		return engine.StartResult{
			Outcome:    engine.StartUnknown,
			WorkloadID: request.Identity.WorkloadID,
			Detail:     detail,
		}, err
	}
	accepted := func(detail string) (engine.StartResult, error) {
		return engine.StartResult{
			Outcome:    engine.StartAccepted,
			WorkloadID: request.Identity.WorkloadID,
			Detail:     detail,
		}, nil
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

	lock := a.lockFor(request.Identity.WorkloadID)
	lock.Lock()
	defer lock.Unlock()
	workspaceLock := a.lockForWorkspace(request.WorkspaceID)
	workspaceLock.Lock()
	defer workspaceLock.Unlock()

	fenced, err := a.isFenced(ctx, request.Identity.WorkloadID)
	if err != nil {
		return unknown("removal fence is unreachable", err)
	}
	if fenced {
		return unknown("workload identity has a durable removal fence", fault.New(fault.NotFound, "workload identity has a durable removal fence"))
	}

	prepared, _, found, err := a.readPreparation(ctx, request.Identity.WorkloadID)
	if err != nil {
		return unknown("preparation state is unreachable", err)
	}
	if !found {
		return unknown("start requires a completed preparation",
			fault.New(fault.InvalidInput, "start requires a completed preparation"))
	}
	if prepared.WorkspaceID != request.WorkspaceID {
		return unknown("start workspace does not match the prepared workspace",
			fault.New(fault.Forbidden, "start workspace does not match the prepared workspace"))
	}

	volumeName, err := WorkspaceVolumeName(request.WorkspaceID)
	if err != nil {
		return unknown("workspace volume name is invalid", err)
	}
	volume, err := a.libpod.InspectVolume(ctx, volumeName)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return unknown("retained workspace volume is missing",
				fault.New(fault.NotFound, "retained workspace volume is missing; start never recreates it"))
		}
		return unknown("retained workspace volume is unreachable",
			fault.Wrap(fault.DependencyUnavailable, "retained workspace volume is unreachable", err))
	}
	if volume.Labels[LabelWorkspace] != string(request.WorkspaceID) {
		return unknown("workspace volume is not owned by this workspace",
			fault.New(fault.Forbidden, "workspace volume is not owned by this workspace"))
	}
	if err := a.verifyPreparedFiles(ctx, request.Identity.WorkloadID, prepared); err != nil {
		return unknown("preparation is incomplete", err)
	}

	spec, err := a.containerSpec(prepared, request.Identity)
	if err != nil {
		return unknown("container specification is invalid", err)
	}
	attempted, err := a.hasStartAttempt(ctx, request.Identity.WorkloadID)
	if err != nil {
		return unknown("start attempt state is unreachable", err)
	}

	state, err := a.libpod.InspectContainer(ctx, spec.Name)
	switch {
	case err == nil:
		if err := checkExistingContainer(state, request.Identity, request.WorkspaceID, prepared.Image); err != nil {
			return unknown("existing container does not match this identity", err)
		}
		return a.startExisting(ctx, request.Identity, request.WorkspaceID, spec.Name, state, attempted, unknown, accepted)
	case errors.Is(err, ErrNotFound):
		if attempted {
			// A start was recorded but the container it targeted is gone.
			// Creating a fresh container would replay a command that may have
			// run, so the outcome stays unknown until cleanup fences it.
			return unknown("a start was attempted and the container is gone; refusing to replay",
				fault.New(fault.UncertainExternalEffect, "a start was attempted and the container is gone"))
		}
		// Create below.
	default:
		return unknown("container state is unreachable",
			fault.Wrap(fault.DependencyUnavailable, "container state is unreachable", err))
	}

	created, err := a.libpod.CreateContainer(ctx, spec)
	if err != nil {
		if errors.Is(err, ErrAlreadyExists) {
			if existing, inspectErr := a.libpod.InspectContainer(ctx, spec.Name); inspectErr == nil {
				if ownershipErr := checkExistingContainer(existing, request.Identity, request.WorkspaceID, prepared.Image); ownershipErr != nil {
					return unknown("existing container does not match this identity", ownershipErr)
				}
				return a.startExisting(ctx, request.Identity, request.WorkspaceID, spec.Name, existing, attempted, unknown, accepted)
			}
		}
		return unknown("container creation outcome is unknown",
			fault.Wrap(fault.UncertainExternalEffect, "container creation outcome is unknown", err))
	}
	if err := ctx.Err(); err != nil {
		return unknown("start cancelled after container creation",
			fault.Wrap(fault.DependencyUnavailable, "start cancelled after container creation", err))
	}
	// Persist the start attempt before the effect: a libpod client cannot later
	// prove whether a start request was delivered, so without this marker a
	// created container would be misreported as never-started and relaunched.
	if err := a.writeStartAttempt(ctx, request.Identity, request.WorkspaceID); err != nil {
		return unknown("start attempt could not be durably recorded; no start was issued", err)
	}
	if err := a.libpod.StartContainer(ctx, created.Name); err != nil {
		return unknown("container start outcome is unknown",
			fault.Wrap(fault.UncertainExternalEffect, "container start outcome is unknown", err))
	}
	return accepted(fmt.Sprintf("created and started stable container %q", created.Name))
}

// startExisting reuses one existing container without replaying a fresh
// command. A created container is started again only when no start attempt was
// ever recorded: an attempted start whose outcome was lost stays unknown.
func (a *Adapter) startExisting(
	ctx context.Context,
	identity engine.EffectIdentity,
	workspaceID id.WorkspaceID,
	name string,
	state ContainerState,
	attempted bool,
	unknown func(string, error) (engine.StartResult, error),
	accepted func(string) (engine.StartResult, error),
) (engine.StartResult, error) {
	switch state.Status {
	case ContainerStatusRunning:
		return accepted(fmt.Sprintf("reused running stable container %q", name))
	case ContainerStatusExited:
		return accepted(fmt.Sprintf("reused exited stable container %q", name))
	case ContainerStatusCreated:
		if attempted {
			return unknown("container start was already attempted; a delayed start may still run",
				fault.New(fault.UncertainExternalEffect, "container start was already attempted"))
		}
		if state.StartPending {
			return unknown("container start is pending; a delayed start may still run",
				fault.New(fault.UncertainExternalEffect, "container start is pending"))
		}
		if state.Started {
			return unknown("container status is ambiguous after an accepted start",
				fault.New(fault.UncertainExternalEffect, "container status is ambiguous"))
		}
		// Record the attempt before the effect for the same reason as a fresh
		// create: a lost start response must never be replayed.
		if err := a.writeStartAttempt(ctx, identity, workspaceID); err != nil {
			return unknown("start attempt could not be durably recorded; no start was issued", err)
		}
		if err := a.libpod.StartContainer(ctx, name); err != nil {
			return unknown("container start outcome is unknown",
				fault.Wrap(fault.UncertainExternalEffect, "container start outcome is unknown", err))
		}
		return accepted(fmt.Sprintf("started stable container %q", name))
	default:
		return unknown("container status is unrecognized",
			fault.New(fault.UncertainExternalEffect, "container status is unrecognized"))
	}
}

// Observe reports the declared Kernel vocabulary for the stable identity. It
// takes the same per-workload lock as the lifecycle effects, so an observation
// can never run between CreateContainer and the durable start attempt. An
// unreachable daemon stays uncertain. An absent container is never-started only
// when the adapter's own durable facts prove no StartContainer request could
// have been issued; otherwise absence is not proof and stays merely absent.
// never-started also requires that no delayed start is pending.
func (a *Adapter) Observe(ctx context.Context, request engine.ObserveRequest) (kernel.Observation, error) {
	if err := validateIdentity(request.Identity); err != nil {
		return kernel.UnknownObservation(), err
	}
	if err := validateWorkspaceID(request.WorkspaceID); err != nil {
		return kernel.UnknownObservation(), err
	}

	lock := a.lockFor(request.Identity.WorkloadID)
	lock.Lock()
	defer lock.Unlock()

	fenced, err := a.isFenced(ctx, request.Identity.WorkloadID)
	if err != nil {
		return kernel.UnknownObservation(), err
	}
	if fenced {
		return kernel.AbsentObservation(), nil
	}
	name, err := ContainerName(request.Identity.WorkloadID)
	if err != nil {
		return kernel.UnknownObservation(), err
	}
	state, err := a.libpod.InspectContainer(ctx, name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return a.observeAbsent(ctx, request.Identity.WorkloadID)
		}
		return kernel.UnknownObservation(), fault.Wrap(fault.DependencyUnavailable, "container state is unreachable", err)
	}
	if err := checkContainerIdentity(state, request.Identity); err != nil {
		return kernel.UnknownObservation(), err
	}
	if err := checkContainerWorkspace(state, request.WorkspaceID); err != nil {
		return kernel.UnknownObservation(), err
	}
	attempted := false
	if state.Status == ContainerStatusCreated {
		attempted, err = a.hasStartAttempt(ctx, request.Identity.WorkloadID)
		if err != nil {
			return kernel.UnknownObservation(), err
		}
	}
	return observationFor(state, attempted), nil
}

// observeAbsent classifies an absent container from the adapter's own durable
// per-workload facts. The Kernel cleanup barrier cannot complete for a
// possibly-started identity whose container is absent, because Stop can only
// report a pending barrier and the workspace is never released. A command that
// never reached StartContainer is different: cleanup may remove the identity
// without inventing stop evidence.
//
// The preparation record is required before Start, and the start-attempt marker
// is written after CreateContainer but before the first StartContainer request.
// The proof here is absence of a start request, not absence of a container: a
// container may have been created and removed out of band. A missing
// preparation record proves Prepare never completed, so Start would refuse; a
// present preparation record with no start-attempt marker proves no
// StartContainer request was ever issued, so the command never ran. A recorded
// start attempt is not proof: the request may have been delivered or lost, so
// the observation stays merely absent. Any unreadable durable state is an error
// and never affirmative proof.
func (a *Adapter) observeAbsent(ctx context.Context, workload id.WorkloadID) (kernel.Observation, error) {
	_, _, prepared, err := a.readPreparation(ctx, workload)
	if err != nil {
		return kernel.UnknownObservation(), err
	}
	if !prepared {
		return kernel.NeverStarted("no preparation record exists for this workload identity"), nil
	}
	attempted, err := a.hasStartAttempt(ctx, workload)
	if err != nil {
		return kernel.UnknownObservation(), err
	}
	if !attempted {
		return kernel.NeverStarted("preparation completed but no start attempt was recorded"), nil
	}
	return kernel.AbsentObservation(), nil
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

	fenced, err := a.isFenced(ctx, request.Identity.WorkloadID)
	if err != nil {
		return engine.LogBatch{}, err
	}
	if fenced {
		return engine.LogBatch{}, fault.New(fault.NotFound, "workload identity has a durable removal fence")
	}
	prepared, _, found, err := a.readPreparation(ctx, request.Identity.WorkloadID)
	if err != nil {
		return engine.LogBatch{}, err
	}
	if !found {
		return engine.LogBatch{}, fault.New(fault.NotFound, "log read requires a completed preparation")
	}
	name, err := ContainerName(request.Identity.WorkloadID)
	if err != nil {
		return engine.LogBatch{}, err
	}
	state, err := a.libpod.InspectContainer(ctx, name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return engine.LogBatch{}, fault.New(fault.NotFound, "log read requires the workload container")
		}
		return engine.LogBatch{}, fault.Wrap(fault.DependencyUnavailable, "container state is unreachable", err)
	}
	if err := checkContainerIdentity(state, request.Identity); err != nil {
		return engine.LogBatch{}, err
	}
	values, err := a.redactionValues(ctx, request.Identity.WorkloadID, prepared)
	if err != nil {
		// Never emit output when the configured values cannot be redacted.
		return engine.LogBatch{}, fault.Wrap(fault.DependencyUnavailable, "redaction source is unavailable; log read refused", err)
	}

	positions := map[engine.LogStream]int64{}
	stdoutOffset, stderrOffset := decodeLogCursor(request.AfterOffset)
	positions[engine.LogStdout] = stdoutOffset
	positions[engine.LogStderr] = stderrOffset

	batch := engine.LogBatch{}
	sequence := int64(0)
	for _, stream := range []engine.LogStream{engine.LogStdout, engine.LogStderr} {
		offset := positions[stream]
		page, err := a.libpod.ReadLogs(ctx, LogRequest{
			Name:   name,
			Stream: stream,
			Offset: offset,
			Limit:  request.Limit,
		})
		if err != nil {
			return engine.LogBatch{}, fault.Wrap(fault.DependencyUnavailable, "container log read is unavailable", err)
		}
		mayGrow := page.Truncated && len(page.Data) == request.Limit
		consumed, data := redactLogPage(page.Data, values, request.Limit, mayGrow)
		nextPosition, err := nextStreamLogPosition(stream, offset, len(consumed))
		if err != nil {
			return engine.LogBatch{}, err
		}
		// Advance only over bytes this stream actually consumed.
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

// Stop requests container termination but returns confirmation only for
// affirmatively observed whole-container termination. A stop acknowledgment or a
// lost response stays pending.
func (a *Adapter) Stop(ctx context.Context, request engine.StopRequest) (kernel.StopEvidence, error) {
	if err := validateIdentity(request.Identity); err != nil {
		return kernel.PendingStop("stop request identity is invalid"), err
	}
	if err := validateWorkspaceID(request.WorkspaceID); err != nil {
		return kernel.PendingStop("stop request workspace is invalid"), err
	}

	lock := a.lockFor(request.Identity.WorkloadID)
	lock.Lock()
	defer lock.Unlock()

	fenced, err := a.isFenced(ctx, request.Identity.WorkloadID)
	if err != nil {
		return kernel.PendingStop("stop unconfirmed: removal fence is unreachable"), err
	}
	if fenced {
		return kernel.PendingStop("stop unconfirmed: workload identity has a durable removal fence"), nil
	}
	name, err := ContainerName(request.Identity.WorkloadID)
	if err != nil {
		return kernel.PendingStop("stop request container name is invalid"), err
	}

	state, err := a.libpod.InspectContainer(ctx, name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return kernel.PendingStop(fmt.Sprintf("stop unconfirmed: container %q is absent", name)), nil
		}
		return kernel.PendingStop(fmt.Sprintf("stop unconfirmed: container %q state is unreachable", name)),
			fault.Wrap(fault.DependencyUnavailable, "container state is unreachable", err)
	}
	if err := checkContainerIdentity(state, request.Identity); err != nil {
		return kernel.PendingStop(fmt.Sprintf("stop unconfirmed: container %q is not owned by this identity", name)), err
	}

	switch state.Status {
	case ContainerStatusRunning, ContainerStatusCreated:
		// An already exited container is never sent a stop request: only its
		// observed termination evidence may confirm the barrier.
		if err := a.libpod.StopContainer(ctx, name); err != nil && !errors.Is(err, ErrNotFound) {
			return kernel.PendingStop(fmt.Sprintf("stop unconfirmed: container %q stop outcome is unknown", name)),
				fault.Wrap(fault.UncertainExternalEffect, "container stop outcome is unknown", err)
		}
	}

	state, err = a.libpod.InspectContainer(ctx, name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return kernel.PendingStop(fmt.Sprintf("stop unconfirmed: container %q disappeared without termination evidence", name)), nil
		}
		return kernel.PendingStop(fmt.Sprintf("stop unconfirmed: container %q state is unreachable", name)),
			fault.Wrap(fault.DependencyUnavailable, "container state is unreachable", err)
	}
	if state.Status == ContainerStatusExited && state.WholeStopped {
		return kernel.ConfirmedStop(fmt.Sprintf(
			"container %q exited with code %d and every container process stopped", name, state.ExitCode)), nil
	}
	return kernel.PendingStop(fmt.Sprintf(
		"stop unconfirmed: container %q has no whole-container termination evidence (%s)",
		name, describeContainerState(state))), nil
}

// Remove is idempotent, identity-scoped and bounded cleanup. It removes only the
// identity's container and materialization, writes a durable removal fence and
// never removes or writes the retained workspace volume. A live or unconfirmed
// container retains every owned resource and names the container.
func (a *Adapter) Remove(ctx context.Context, request engine.RemoveRequest) (engine.Removal, error) {
	if err := validateIdentity(request.Identity); err != nil {
		return engine.Removal{Detail: "removal request identity is invalid"}, err
	}
	if err := validateWorkspaceID(request.WorkspaceID); err != nil {
		return engine.Removal{Detail: "removal request workspace is invalid"}, err
	}

	lock := a.lockFor(request.Identity.WorkloadID)
	lock.Lock()
	defer lock.Unlock()
	workspaceLock := a.lockForWorkspace(request.WorkspaceID)
	workspaceLock.Lock()
	defer workspaceLock.Unlock()

	name, err := ContainerName(request.Identity.WorkloadID)
	if err != nil {
		return engine.Removal{Detail: "removal request container name is invalid"}, err
	}
	fenced, err := a.isFenced(ctx, request.Identity.WorkloadID)
	if err != nil {
		return engine.Removal{Detail: retainedDetail(name, "removal fence is unreachable")}, err
	}
	state, inspectErr := a.libpod.InspectContainer(ctx, name)
	containerPresent := false
	switch {
	case inspectErr == nil:
		if err := checkContainerIdentity(state, request.Identity); err != nil {
			return engine.Removal{Detail: retainedDetail(name, "container is not owned by this identity")}, nil
		}
		attempted := false
		if state.Status == ContainerStatusCreated {
			attempted, err = a.hasStartAttempt(ctx, request.Identity.WorkloadID)
			if err != nil {
				return engine.Removal{Detail: retainedDetail(name, "start attempt state is unreachable")}, err
			}
		}
		if !removable(state, attempted) {
			return engine.Removal{Detail: retainedDetail(name, describeContainerState(state))}, nil
		}
		containerPresent = true
	case errors.Is(inspectErr, ErrNotFound):
		containerPresent = false
	default:
		return engine.Removal{Detail: retainedDetail(name, "container state is unreachable")}, nil
	}

	// Persist the removal intent before any destructive effect, so a crash can
	// never leave an unfenced identity whose container was already removed and
	// whose materialization still authorizes a replaying start.
	if !fenced {
		if err := a.writeFence(ctx, request.Identity, request.WorkspaceID); err != nil {
			return engine.Removal{Detail: retainedDetail(name, "removal fence write failed")}, nil
		}
	}
	if containerPresent {
		if err := a.removeWithRetry(ctx, func(ctx context.Context) error {
			return a.libpod.RemoveContainer(ctx, name)
		}); err != nil {
			detail := retainedDetail(name, "container removal failed")
			if ctx.Err() != nil {
				return engine.Removal{Detail: detail},
					fault.Wrap(fault.DependencyUnavailable, "removal cancelled during cleanup", ctx.Err())
			}
			return engine.Removal{Detail: detail}, nil
		}
	}
	if err := a.files.RemoveAll(ctx, workloadRoot(a.root, request.Identity.WorkloadID)); err != nil {
		return engine.Removal{Detail: retainedDetail(name, "materialization removal failed")}, nil
	}
	return engine.Removal{
		Removed: true,
		Detail: fmt.Sprintf("removed container %q and materialization for workload %q",
			name, request.Identity.WorkloadID),
	}, nil
}

// removeWithRetry bounds removal attempts per call with the injected clock.
func (a *Adapter) removeWithRetry(ctx context.Context, remove func(context.Context) error) error {
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

// retainedDetail names the retained container so pending cleanup is
// attributable.
func retainedDetail(containerName, reason string) string {
	return fmt.Sprintf("cleanup pending: retained container %q: %s", containerName, reason)
}

// RefreshCredential atomically replaces only the reserved credential file. It
// never creates, starts, stops or removes the container, and a failed or lost
// replacement leaves the previous token intact for a retry.
func (a *Adapter) RefreshCredential(ctx context.Context, request engine.CredentialRequest) (engine.CredentialRefresh, error) {
	if err := validateIdentity(request.Identity); err != nil {
		return engine.CredentialRefresh{}, err
	}
	if strings.TrimSpace(request.Credential) == "" {
		return engine.CredentialRefresh{}, fault.New(fault.InvalidInput, "credential refresh requires a non-empty credential")
	}

	lock := a.lockFor(request.Identity.WorkloadID)
	lock.Lock()
	defer lock.Unlock()

	fenced, err := a.isFenced(ctx, request.Identity.WorkloadID)
	if err != nil {
		return engine.CredentialRefresh{Detail: "credential refresh refused: removal fence is unreachable"}, err
	}
	if fenced {
		return engine.CredentialRefresh{Detail: "credential refresh refused: workload identity has a durable removal fence"},
			fault.New(fault.NotFound, "workload identity has a durable removal fence")
	}
	if _, _, found, err := a.readPreparation(ctx, request.Identity.WorkloadID); err != nil {
		return engine.CredentialRefresh{Detail: "credential refresh refused: preparation state is unreachable"}, err
	} else if !found {
		return engine.CredentialRefresh{Detail: "credential refresh requires a completed preparation"},
			fault.New(fault.NotFound, "credential refresh requires a completed preparation")
	}

	if err := a.replaceFile(ctx, hostPath(a.root, request.Identity.WorkloadID, workspace.ReservedCredentialPath), []byte(request.Credential)); err != nil {
		return engine.CredentialRefresh{Detail: "credential replacement outcome is unknown"}, err
	}
	return engine.CredentialRefresh{
		Delivered: true,
		Detail:    fmt.Sprintf("replaced the execution credential for %s without restarting the container", describeIdentity(request.Identity)),
	}, nil
}
