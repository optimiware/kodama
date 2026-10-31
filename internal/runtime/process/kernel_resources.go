package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/optimiware/kodama/internal/domain"
	kr "github.com/optimiware/kodama/internal/kernel/runtime"
)

// reasonUnobservable marks an unknown state whose cause is the backend's
// inability to look, not a lifecycle fact.
const reasonUnobservable kr.Reason = "resource_unobservable"

func resourceType(k kr.ResourceKind) string {
	if k == kr.ResourceWorkspace {
		return typeWorkspace
	}
	return typeService
}

// checkResource verifies that the record is the resource the handle names.
func checkResource(rec kRecord, h kr.ResourceHandle) error {
	if rec.Untracked {
		return nil
	}
	if rec.RunID != h.RunID || rec.Name != h.Name {
		return fmt.Errorf("%w: resource %s belongs to another run", domain.ErrConflict, h.Identity)
	}
	return nil
}

// admitResource refuses what the backend cannot enforce, before anything exists.
func admitResource(spec kr.ResourceSpec, hasPrepare bool) error {
	requires := slices.Clone(spec.Requires)
	if spec.Lifetime != kr.LifetimePipeline {
		requires = append(requires, kr.CapPersistentService)
	}
	if err := admit("resource "+spec.Identity, requires); err != nil {
		return err
	}
	switch spec.Kind {
	case kr.ResourceWorkspace:
		in := spec.Preparation
		if (len(in.Sources) > 0 || len(in.Credentials) > 0) && !hasPrepare {
			return notReached("%w: process fetches no sources: no preparation is configured", kr.ErrCapability)
		}
	default:
		s := spec.Service
		switch {
		case len(s.Secrets) > 0:
			return notReached("%w: process materializes no service secrets", kr.ErrCapability)
		case len(s.Commands) == 0:
			return notReached("%w: process does not interpret the image of service %s: it declares no commands", kr.ErrCapability, s.Name)
		}
	}
	return nil
}

// Ensure implements kr.ResourceManager. The record, with the writer's fence, is
// written before anything exists; it is idempotent on the identity: the same
// specification digest adopts, another digest is ErrSpecMismatch, an older
// fence is ErrStaleFence and a stopped or removed identity is ErrFenced. A
// workspace is created once and its filesystem identity is verified afterwards:
// lost scratch is never recreated, a replacement is a collision. A service
// launch that was ever announced is adopted, never repeated.
func (r *KernelResources) Ensure(ctx context.Context, spec kr.ResourceSpec) (kr.ResourceObservation, error) {
	h := r.h
	if err := spec.Validate(); err != nil {
		return kr.ResourceObservation{}, notReached("%w", err)
	}
	if err := admitResource(spec, h.cfg.Prepare != nil); err != nil {
		return kr.ResourceObservation{}, err
	}
	typ := resourceType(spec.Kind)
	key := objectKey(spec.Scope, typ, spec.Identity)
	handle := kr.ResourceHandle{Scope: spec.Scope, RunID: spec.RunID, Kind: spec.Kind, Name: spec.Name, Identity: spec.Identity, Fence: spec.Fence}
	unlock := h.st.lock(typ, key)
	defer unlock()

	rec, found, err := h.st.load(typ, key)
	if err != nil {
		return kr.ResourceObservation{}, err
	}
	if found {
		if err := checkResource(rec, handle); err != nil {
			return kr.ResourceObservation{}, notReached("%w", err)
		}
		if err := h.admitFence(&rec, spec.Fence); err != nil {
			return kr.ResourceObservation{}, notReached("%w", err)
		}
		if rec.fenced() {
			return kr.ResourceObservation{}, notReached("%w: resource %s", ErrFenced, spec.Identity)
		}
		if rec.Digest != spec.SpecDigest {
			return kr.ResourceObservation{}, notReached("%w: resource %s holds digest %s", kr.ErrSpecMismatch, spec.Identity, rec.Digest)
		}
	} else {
		if exists, err := h.st.dirExists(typ, key); err != nil {
			return kr.ResourceObservation{}, notReached("%w", err)
		} else if exists {
			return kr.ResourceObservation{}, notReached("%w: a directory not recorded by this adapter holds resource %s", errCollision, spec.Identity)
		}
		rec = kRecord{
			Type: typ, Key: key, Scope: spec.Scope, RunID: spec.RunID, ID: spec.Identity, Name: spec.Name,
			Digest: spec.SpecDigest, Fence: spec.Fence, UID: newUID(),
		}
		if spec.Service != nil {
			rec.Service, rec.Ports = spec.Service.Name, slices.Clone(spec.Service.Ports)
		}
		if err := h.st.create(rec); err != nil {
			if errors.Is(err, fs.ErrExist) {
				return kr.ResourceObservation{}, notReached("%w: a concurrent create recorded the resource", domain.ErrConflict)
			}
			return kr.ResourceObservation{}, notReached("%w", err)
		}
	}
	if spec.Kind == kr.ResourceWorkspace {
		err = h.ensureWorkspace(ctx, &rec, spec)
	} else if rec.IssuedAt == nil {
		err = h.launchService(ctx, &rec, spec)
	}
	if err != nil {
		return kr.ResourceObservation{}, err
	}
	if err := h.st.save(rec); err != nil {
		return kr.ResourceObservation{}, err
	}
	return h.observeResource(ctx, handle, &rec)
}

// ensureWorkspace creates the workspace once and records its filesystem
// identity. The intent (Owned) is durable before the directory exists; a retry
// after an interruption adopts the directory only if it carries this instance's
// marker. Once Created nothing is recreated.
func (h *host) ensureWorkspace(ctx context.Context, rec *kRecord, spec kr.ResourceSpec) error {
	if !rec.Created {
		rec.Owned = true
		if err := h.st.save(*rec); err != nil {
			return err
		}
		if err := h.st.makeDir(*rec); err != nil {
			return err
		}
		dir := h.st.workspaceDir(rec.Key)
		if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		fi, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return errCollision
		}
		if rec.FsDev, rec.FsIno, err = fsIdentity(fi); err != nil {
			return err
		}
		rec.Created = true
		if err := h.st.save(*rec); err != nil {
			return err
		}
	} else if state, err := h.st.verifyWorkspace(*rec); err != nil || state == wsMissing {
		return err // missing scratch is reported by observation, never recreated
	}
	if rec.Prepared {
		return nil
	}
	if in := spec.Preparation; len(in.Sources) > 0 || len(in.Credentials) > 0 {
		if err := h.cfg.Prepare(ctx, h.st.workspaceDir(rec.Key), *in); err != nil {
			return fmt.Errorf("%w: preparing the workspace: %w", kr.ErrUncertain, err)
		}
	}
	rec.Prepared = true
	return nil
}

// launchService starts the service behind the launch gate. The launch is
// announced (durably) before the process is started; a start that fails proves
// nothing ran, so the announcement is withdrawn and a retry starts afresh.
func (h *host) launchService(ctx context.Context, rec *kRecord, spec kr.ResourceSpec) error {
	rec.Owned = true
	if err := h.st.save(*rec); err != nil {
		return err
	}
	if err := h.st.makeDir(*rec); err != nil {
		return notReached("%w", err)
	}
	work := filepath.Join(h.st.dir(typeService, rec.Key), "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		return notReached("%w", err)
	}
	s := spec.Service
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + work, "TMPDIR=" + work}
	for _, k := range slices.Sorted(maps.Keys(s.Env)) {
		env = append(env, k+"="+s.Env[k])
	}
	env = append(env, "KODAMA_RUN_ID="+string(spec.RunID), "KODAMA_SERVICE_NAME="+s.Name, "KODAMA_RESOURCE_NAME="+spec.Name)
	cmd := Command{Path: "/bin/sh", Args: []string{"-c", shellScript(s.Commands)}, Env: env, Dir: work}

	now := h.now()
	rec.IssuedAt = &now
	if err := h.st.save(*rec); err != nil {
		rec.IssuedAt = nil
		return notReached("%w", err)
	}
	if err := h.spawn(rec, gate(cmd), work, 0, nil); err != nil {
		// The gate never opened, so nothing ran: withdraw the announcement.
		announced := rec.IssuedAt
		rec.IssuedAt, rec.Pgid, rec.StartID, rec.ExitKnown = nil, 0, "", false
		h.forget(rec.Type, rec.Key)
		if serr := h.st.save(*rec); serr != nil {
			// Keep it announced: a restart must not repeat a launch it cannot rule out.
			rec.IssuedAt = announced
			return fmt.Errorf("%w: launching service %s: %w", kr.ErrUncertain, s.Name, err)
		}
		return notReached("launching service %s: %w", s.Name, err)
	}
	return nil
}

// serviceReady verifies that a service of the run is live and ready.
func (h *host) serviceReady(ctx context.Context, b kr.RuntimeBinding, s kr.ResourceHandle) (string, error) {
	key := objectKey(b.Scope, typeService, s.Identity)
	unlock := h.st.lock(typeService, key)
	defer unlock()
	rec, found, err := h.st.load(typeService, key)
	if err != nil {
		return "", err
	}
	if !found || rec.Untracked || rec.RunID != b.RunID || rec.fenced() {
		return "", fmt.Errorf("%w: service %s is not live", domain.ErrConflict, s.Name)
	}
	obs, err := h.observeResource(ctx, s, &rec)
	if err != nil {
		return "", err
	}
	if obs.State != kr.ResourceReady {
		return "", fmt.Errorf("%w: service %s is %s", domain.ErrConflict, s.Name, obs.State)
	}
	return rec.Service, nil
}

// observeResource describes the record's resource. The caller holds the lock;
// an exit this instance saw is persisted first. Removal is reported only for a
// confirmed removal or an identity nothing exists at; unknown never counts as
// removed.
func (h *host) observeResource(ctx context.Context, handle kr.ResourceHandle, rec *kRecord) (kr.ResourceObservation, error) {
	obs := kr.ResourceObservation{Handle: handle, ObservedAt: h.now()}
	if h.settle(rec) {
		if err := h.st.save(*rec); err != nil {
			return kr.ResourceObservation{}, err
		}
	}
	unknown := func(reason kr.Reason) (kr.ResourceObservation, error) {
		obs.State, obs.Reason = kr.ResourceUnknown, reason
		return obs, nil
	}
	switch {
	case rec.Removed:
		obs.State = kr.ResourceRemoved
		return obs, nil
	case rec.Stopped:
		obs.State = kr.ResourceStopped
		return obs, nil
	}
	if rec.Type == typeWorkspace {
		if !rec.Created {
			obs.State = kr.ResourceStarting
			return obs, nil
		}
		state, err := h.st.verifyWorkspace(*rec)
		switch {
		case errors.Is(err, errCollision):
			return kr.ResourceObservation{}, err
		case err != nil:
			return unknown(reasonUnobservable)
		case state == wsMissing:
			obs.State, obs.Reason = kr.ResourceFailed, kr.ReasonWorkspaceLost
			return obs, nil
		case rec.Stopping:
			return unknown(kr.ReasonStopUncertain) // held: the stop is not verified
		case !rec.Prepared:
			obs.State = kr.ResourceStarting
		default:
			obs.State = kr.ResourceReady
		}
		return obs, nil
	}
	switch {
	case rec.IssuedAt == nil:
		obs.State = kr.ResourceStarting
		return obs, nil
	case rec.Stopping:
		return unknown(kr.ReasonStopUncertain)
	case rec.Pgid == 0:
		obs.State, obs.Reason = kr.ResourceFailed, kr.ReasonServiceLost
		return obs, nil
	}
	gone, err := h.groupGone(*rec)
	switch {
	case err != nil:
		return unknown(reasonUnobservable)
	case gone:
		obs.State, obs.Reason = kr.ResourceFailed, kr.ReasonServiceLost
		return obs, nil
	}
	for _, p := range rec.Ports {
		if !portReady(ctx, p) {
			obs.State = kr.ResourceStarting
			return obs, nil
		}
	}
	obs.State = kr.ResourceReady
	return obs, nil
}

// Observe implements kr.ResourceManager. It never creates. An identity nothing
// is recorded at for this tenant and project is reported removed: nothing
// exists to remove.
func (r *KernelResources) Observe(ctx context.Context, handle kr.ResourceHandle) (kr.ResourceObservation, error) {
	h := r.h
	if err := handle.Validate(); err != nil {
		return kr.ResourceObservation{}, err
	}
	typ := resourceType(handle.Kind)
	key := objectKey(handle.Scope, typ, handle.Identity)
	unlock := h.st.lock(typ, key)
	defer unlock()
	rec, found, err := h.st.load(typ, key)
	switch {
	case err != nil:
		return kr.ResourceObservation{Handle: handle, State: kr.ResourceUnknown, Reason: reasonUnobservable, ObservedAt: h.now()}, nil
	case !found:
		return kr.ResourceObservation{Handle: handle, State: kr.ResourceRemoved, ObservedAt: h.now()}, nil
	}
	if err := checkResource(rec, handle); err != nil {
		return kr.ResourceObservation{}, err
	}
	return h.observeResource(ctx, handle, &rec)
}

// Logs implements kr.ResourceManager: the bounded output of a service; a
// workspace has none.
func (r *KernelResources) Logs(_ context.Context, handle kr.ResourceHandle, opts kr.LogOptions) (io.ReadCloser, error) {
	h := r.h
	if err := handle.Validate(); err != nil {
		return nil, err
	}
	if handle.Kind != kr.ResourceService {
		return nil, fmt.Errorf("%w: a %s has no logs", domain.ErrInvalidValue, handle.Kind)
	}
	key := objectKey(handle.Scope, typeService, handle.Identity)
	unlock := h.st.lock(typeService, key)
	rec, found, err := h.st.load(typeService, key)
	unlock()
	switch {
	case err != nil:
		return nil, err
	case !found || rec.Removed || rec.Untracked:
		return nil, notFound("no resource %s", handle.Identity)
	}
	if err := checkResource(rec, handle); err != nil {
		return nil, err
	}
	return h.openLog(rec, opts)
}

// tombstoneResource loads the record of an identity or leaves the tombstone a
// stop or remove of an unrecorded identity records.
func (h *host) tombstoneResource(typ, key string, handle kr.ResourceHandle, removed bool) (kRecord, error) {
	rec, found, err := h.st.load(typ, key)
	if err != nil {
		return kRecord{}, err
	}
	if found {
		return rec, checkResource(rec, handle)
	}
	rec = kRecord{
		Type: typ, Key: key, Scope: handle.Scope, RunID: handle.RunID, ID: handle.Identity, Name: handle.Name,
		Fence: handle.Fence, Untracked: true, Stopping: true, Stopped: true, Removed: removed,
	}
	if err := h.st.create(rec); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return h.tombstoneResource(typ, key, handle, removed)
		}
		return kRecord{}, err
	}
	return rec, nil
}

// stopWorkspace records the stop intent, which fences any later use, and the
// verified stop once no workload holds the workspace. It reports whether the
// workspace is verifiably stopped.
func (h *host) stopWorkspace(rec *kRecord) (bool, error) {
	if rec.Stopped {
		return true, nil
	}
	if rec.Created {
		if _, err := h.st.verifyWorkspace(*rec); err != nil {
			return false, err
		}
	}
	if !rec.Stopping {
		rec.Stopping = true
		if err := h.st.save(*rec); err != nil {
			return false, err
		}
	}
	if _, held, err := h.st.holder(rec.Key, h.prober()); err != nil || held {
		return false, err
	}
	rec.Stopped = true
	return true, h.st.save(*rec)
}

// Stop implements kr.ResourceManager. The stop intent is recorded first (a
// pending create can no longer start, and a stop from a stale fence is
// refused) and the group is terminated and verified gone before Stopped is
// reported. A workspace that a workload still holds is not stopped: it reports
// unknown with stop_uncertain until the holder is gone. A duplicate stop
// returns the recorded outcome.
func (r *KernelResources) Stop(ctx context.Context, handle kr.ResourceHandle) (kr.ResourceObservation, error) {
	h := r.h
	if err := handle.Validate(); err != nil {
		return kr.ResourceObservation{}, err
	}
	typ := resourceType(handle.Kind)
	key := objectKey(handle.Scope, typ, handle.Identity)
	unlock := h.st.lock(typ, key)
	defer unlock()
	rec, err := h.tombstoneResource(typ, key, handle, false)
	if err != nil {
		return kr.ResourceObservation{}, err
	}
	if err := h.admitFence(&rec, handle.Fence); err != nil {
		return kr.ResourceObservation{}, err
	}
	switch {
	case rec.Removed:
	case typ == typeWorkspace:
		if _, err := h.stopWorkspace(&rec); err != nil {
			return kr.ResourceObservation{}, err
		}
	default:
		if err := h.stopLocked(ctx, &rec); err != nil {
			return kr.ResourceObservation{Handle: handle, State: kr.ResourceUnknown, Reason: kr.ReasonStopUncertain, ObservedAt: h.now()},
				fmt.Errorf("%w: stopping service %s: %w", kr.ErrUncertain, handle.Name, err)
		}
	}
	return h.observeResource(ctx, handle, &rec)
}

// Remove implements kr.ResourceManager. It stops the resource, deletes the
// files it created after verifying their identity, and confirms with
// ResourceRemoved. A workspace a workload still holds is not removed (the
// observation stays unknown/stop_uncertain, never removed); a workspace
// directory that is not the recorded instance is a collision that is never
// deleted. The tombstone stays, so a delayed create of the identity is refused.
func (r *KernelResources) Remove(ctx context.Context, handle kr.ResourceHandle) (kr.ResourceObservation, error) {
	h := r.h
	if err := handle.Validate(); err != nil {
		return kr.ResourceObservation{}, err
	}
	typ := resourceType(handle.Kind)
	key := objectKey(handle.Scope, typ, handle.Identity)
	unlock := h.st.lock(typ, key)
	defer unlock()
	rec, err := h.tombstoneResource(typ, key, handle, true)
	if err != nil {
		return kr.ResourceObservation{}, err
	}
	if err := h.admitFence(&rec, handle.Fence); err != nil {
		return kr.ResourceObservation{}, err
	}
	if typ == typeWorkspace && !rec.Removed {
		if stopped, err := h.stopWorkspace(&rec); err != nil || !stopped {
			if err != nil {
				return kr.ResourceObservation{}, err
			}
			return h.observeResource(ctx, handle, &rec)
		}
	}
	if err := h.removeLocked(ctx, &rec); err != nil {
		return kr.ResourceObservation{Handle: handle, State: kr.ResourceUnknown, Reason: kr.ReasonCleanupFailed, ObservedAt: h.now()},
			fmt.Errorf("%w: removing resource %s: %w", kr.ErrUncertain, handle.Name, err)
	}
	return h.observeResource(ctx, handle, &rec)
}
