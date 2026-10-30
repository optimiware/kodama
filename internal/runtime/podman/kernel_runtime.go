package podman

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	kr "github.com/optimiware/kodama/internal/kernel/runtime"
)

// KernelRuntime implements the kernel's Runtime port with Podman containers:
// one workload per binding, the step's digest-pinned image on a read-only root
// filesystem as a non-root user with every capability dropped, under CPU,
// memory and process limits. See kernel_core.go for the durable-identity,
// adoption and fencing rules it shares with KernelResources.
//
// Start is idempotent on Binding.WorkloadID. An error that proves the command
// never reached the engine wraps kr.ErrNotReached; every other error is
// uncertain and is reconciled by Observe, which finds the container by its
// name and instance UID, never by replaying the create.
type KernelRuntime struct{ core *kernelCore }

var _ kr.Runtime = (*KernelRuntime)(nil)

// NewKernelRuntime opens the runtime over cfg.StateDir.
func NewKernelRuntime(cfg KernelConfig) (*KernelRuntime, error) {
	c, err := newKernelCore(cfg)
	if err != nil {
		return nil, err
	}
	return &KernelRuntime{core: c}, nil
}

var envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Capabilities implements kr.Runtime. It declares only what the real-engine
// tests hold the backend to.
func (k *KernelRuntime) Capabilities(context.Context) (kr.CapabilitySet, error) {
	return append(kr.CapabilitySet(nil), KernelCapabilities...), nil
}

func notReached(err error) error { return fmt.Errorf("%w: %w", kr.ErrNotReached, err) }

func (k *KernelRuntime) workloadRec(b kr.RuntimeBinding) krec {
	return krec{
		Name: objectName("kw-", b.Scope, b.WorkloadID), Class: classWorkload, Scope: b.Scope, RunID: b.RunID,
		Identity: b.WorkloadID, SpecDigest: b.SpecDigest, Fence: b.Fence,
	}
}

// Start implements kr.Runtime. Everything that can be refused is refused before
// the engine is asked: an unmet capability, an unpinned image, a workspace or
// service that is not this run's, a secret that was not materialized.
func (k *KernelRuntime) Start(ctx context.Context, req kr.StartRequest) (kr.WorkloadObservation, error) {
	b := req.Binding
	if err := b.Validate(); err != nil {
		return kr.WorkloadObservation{}, notReached(err)
	}
	if b.Backend != Backend {
		return kr.WorkloadObservation{}, refuse("the binding names backend %q, not %q", b.Backend, Backend)
	}
	if err := req.Spec.Validate(); err != nil {
		return kr.WorkloadObservation{}, notReached(err)
	}
	if err := KernelCapabilities.Satisfies(req.Spec.Requires); err != nil {
		return kr.WorkloadObservation{}, notReached(err)
	}
	if !pinned(req.Spec.Image) {
		return kr.WorkloadObservation{}, refuse("image %q is not pinned by digest", req.Spec.Image)
	}
	for key := range req.Spec.Env {
		if !envKey.MatchString(key) {
			return kr.WorkloadObservation{}, refuse("environment name %q", key)
		}
	}
	secretsDir, err := k.secretsDir(req)
	if err != nil {
		return kr.WorkloadObservation{}, err
	}
	if err := k.checkHandles(req); err != nil {
		return kr.WorkloadObservation{}, err
	}

	want := k.workloadRec(b)
	want.TimeoutNanos = int64(req.Spec.Timeout)
	c := k.core
	unlock, err := c.locks.lock(ctx, want.Name)
	if err != nil {
		return kr.WorkloadObservation{}, notReached(err)
	}
	defer unlock()
	rec, err := c.admit(want)
	if err != nil {
		return kr.WorkloadObservation{}, err
	}
	ct, err := c.ensureContainer(ctx, &rec, func() (ctrSpec, error) { return k.containerSpec(ctx, req, secretsDir) })
	if err != nil {
		return kr.WorkloadObservation{}, err
	}
	if _, err := c.startContainer(ctx, rec, ct); err != nil {
		return kr.WorkloadObservation{}, err
	}
	return k.observe(ctx, &rec, b)
}

// secretsDir validates the step's private secret materialization. A step may
// only receive what its spec declared, for its own run and step.
func (k *KernelRuntime) secretsDir(req kr.StartRequest) (string, error) {
	m, b := req.Secrets, req.Binding
	if m.Directory == "" {
		if len(req.Spec.Secrets) > 0 {
			return "", refuse("the step declares secrets but none were materialized")
		}
		return "", nil
	}
	if m.Scope != b.Scope || m.RunID != b.RunID || m.StepKey != b.StepKey {
		return "", refuse("the secret materialization belongs to another step")
	}
	if !filepath.IsAbs(m.Directory) {
		return "", refuse("the secret directory must be absolute")
	}
	if st, err := os.Stat(m.Directory); err != nil || !st.IsDir() {
		return "", refuse("the secret directory is not readable")
	}
	return m.Directory, nil
}

// checkHandles verifies that the workspace and services belong to the binding's
// scope and run.
func (k *KernelRuntime) checkHandles(req kr.StartRequest) error {
	b := req.Binding
	check := func(h kr.ResourceHandle, kind kr.ResourceKind) error {
		if err := h.Validate(); err != nil {
			return notReached(err)
		}
		if h.Kind != kind || h.Scope != b.Scope || h.RunID != b.RunID {
			return refuse("resource %s does not belong to this run", h.Name)
		}
		return nil
	}
	if req.Workspace.Identity != "" {
		if err := check(req.Workspace, kr.ResourceWorkspace); err != nil {
			return err
		}
	}
	for _, s := range req.Services {
		if err := check(s, kr.ResourceService); err != nil {
			return err
		}
	}
	return nil
}

// containerSpec builds the create of an attempt: the verified run workspace at
// /workspace, the run network when services exist, private secrets read-only.
func (k *KernelRuntime) containerSpec(ctx context.Context, req kr.StartRequest, secretsDir string) (ctrSpec, error) {
	c, b := k.core, req.Binding
	s := ctrSpec{
		image:      req.Spec.Image,
		entrypoint: []string{"/bin/sh", "-ec"},
		args:       []string{strings.Join(req.Spec.Commands, "\n")},
		env:        []string{"HOME=/tmp", "TMPDIR=/tmp"},
		tmpfs:      []string{"/tmp"},
		timeout:    req.Spec.Timeout,
	}
	for _, key := range sortedKeys(req.Spec.Env) {
		s.env = append(s.env, key+"="+req.Spec.Env[key])
	}
	if req.Workspace.Identity != "" {
		vol, err := c.verifiedWorkspace(ctx, req.Workspace, b.Scope, b.RunID)
		if err != nil {
			return ctrSpec{}, err
		}
		s.volumes = append(s.volumes, vol+":/workspace:U")
	} else {
		s.tmpfs = append(s.tmpfs, "/workspace")
	}
	if secretsDir != "" {
		s.volumes = append(s.volumes, secretsDir+":/kodama/secrets:ro")
	}
	if len(req.Services) > 0 {
		var network string
		for _, h := range req.Services {
			rec, err := c.verifiedService(h, b.Scope, b.RunID)
			if err != nil {
				return ctrSpec{}, err
			}
			network = rec.Network
		}
		s.networks = []string{network}
	} else {
		s.networks = []string{c.cfg.Network}
	}
	return s, nil
}

// observe reports the workload from the engine and the record. The caller holds
// the lock.
func (k *KernelRuntime) observe(ctx context.Context, rec *krec, b kr.RuntimeBinding) (kr.WorkloadObservation, error) {
	c := k.core
	obs := kr.WorkloadObservation{Binding: b, ObservedAt: c.now()}
	ct, found, err := c.findContainer(ctx, rec)
	if err != nil && !errors.Is(err, errForeign) {
		return kr.WorkloadObservation{}, err
	}
	switch {
	case !found && rec.Unanswered && !rec.Removed:
		// A create whose outcome was never seen and whose object is not observed:
		// not absent, and never removed.
		obs.Phase = kr.PhaseUnknown
	case !found:
		obs.Phase = kr.PhaseAbsent
	case !ct.started():
		// Created and not started: adopt it by calling Start again, or stop it.
		obs.Phase = kr.PhaseUnknown
	case ct.State.Running || ct.State.Status == "stopping" || ct.State.Status == "paused":
		obs.Phase = kr.PhaseRunning
	default:
		obs.Phase, obs.Result = kr.PhaseExited, exitResult(*rec, ct)
	}
	return obs, nil
}

// exitResult maps an exited container to the kernel's result. Success is the
// engine's zero exit; a non-zero exit after our stop is a cancellation and one
// at the declared limit is a timeout.
func exitResult(rec krec, ct kctr) kr.Result {
	code := ct.State.ExitCode
	switch {
	case code == 0:
		return kr.Result{Code: kr.ResultSucceeded}
	case rec.TimeoutNanos > 0 && (code == 137 || code == 124 || code == 143 || code == -1) &&
		ct.State.FinishedAt.Sub(ct.State.StartedAt) >= time.Duration(rec.TimeoutNanos)-time.Second:
		return kr.Result{Code: kr.ResultTimedOut, ExitCode: code, Reason: kr.ReasonStepTimeout}
	case rec.Stopping:
		return kr.Result{Code: kr.ResultCancelled, ExitCode: code, Reason: kr.ReasonCancelled}
	}
	return kr.Result{Code: kr.ResultFailed, ExitCode: code, Reason: kr.ReasonCommandFailed}
}

// bound loads and verifies the record a binding names; found is false when this
// adapter never ran the workload (another backend, another scope or never
// recorded), which is reported as absent.
func (k *KernelRuntime) bound(b kr.RuntimeBinding) (krec, bool, error) {
	if err := b.Validate(); err != nil {
		return krec{}, false, err
	}
	if b.Backend != Backend {
		return krec{}, false, nil
	}
	rec, found, err := k.core.load(k.workloadRec(b).Name)
	if err != nil || !found {
		return krec{}, false, err
	}
	if err := recordMatches(rec, k.workloadRec(b)); err != nil {
		return krec{}, false, err
	}
	if rec.SpecDigest != b.SpecDigest {
		return krec{}, false, fmt.Errorf("%w: %s is recorded with specification %s", kr.ErrSpecMismatch, b.WorkloadID, rec.SpecDigest)
	}
	return rec, true, nil
}

func workloadAbsent(b kr.RuntimeBinding, at time.Time) kr.WorkloadObservation {
	return kr.WorkloadObservation{Binding: b, Phase: kr.PhaseAbsent, ObservedAt: at}
}

// Observe implements kr.Runtime.
func (k *KernelRuntime) Observe(ctx context.Context, b kr.RuntimeBinding) (kr.WorkloadObservation, error) {
	rec, found, err := k.bound(b)
	if err != nil {
		return kr.WorkloadObservation{}, err
	}
	if !found {
		return workloadAbsent(b, k.core.now()), nil
	}
	unlock, err := k.core.locks.lock(ctx, rec.Name)
	if err != nil {
		return kr.WorkloadObservation{}, err
	}
	defer unlock()
	if rec, found, err = k.bound(b); err != nil || !found {
		return workloadAbsent(b, k.core.now()), err
	}
	return k.observe(ctx, &rec, b)
}

// Logs implements kr.Runtime.
func (k *KernelRuntime) Logs(ctx context.Context, b kr.RuntimeBinding, opts kr.LogOptions) (io.ReadCloser, error) {
	rec, found, err := k.bound(b)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("%w: no workload %s", domain.ErrConflict, b.WorkloadID)
	}
	unlock, err := k.core.locks.lock(ctx, rec.Name)
	if err != nil {
		return nil, err
	}
	ct, ok, err := k.core.findContainer(ctx, &rec)
	unlock()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: the container of %s is not observed", domain.ErrConflict, b.WorkloadID)
	}
	return k.core.logStream(ctx, ct.ID, opts.Follow), nil
}

// Stop implements kr.Runtime. The stop intent is recorded first and fences the
// identity: a later Start, from any fence, creates nothing. A stop of a
// workload nobody recorded leaves a tombstone, so a delayed create is rejected.
// A stop from a stale fence is refused. The observation reports PhaseRunning
// while the container still runs.
func (k *KernelRuntime) Stop(ctx context.Context, b kr.RuntimeBinding) (kr.WorkloadObservation, error) {
	return k.settle(ctx, b, false)
}

// Remove implements kr.Runtime. It stops the workload, removes its container by
// the bound ID and confirms with PhaseAbsent only after the engine says it no
// longer exists. A create whose outcome is unresolved is never confirmed
// removed. A workload still running is reported PhaseRunning, not removed.
func (k *KernelRuntime) Remove(ctx context.Context, b kr.RuntimeBinding) (kr.WorkloadObservation, error) {
	return k.settle(ctx, b, true)
}

func (k *KernelRuntime) settle(ctx context.Context, b kr.RuntimeBinding, remove bool) (kr.WorkloadObservation, error) {
	if err := b.Validate(); err != nil {
		return kr.WorkloadObservation{}, err
	}
	c := k.core
	if b.Backend != Backend {
		return workloadAbsent(b, c.now()), nil // a backend never acts on another's workload
	}
	want := k.workloadRec(b)
	unlock, err := c.locks.lock(ctx, want.Name)
	if err != nil {
		return kr.WorkloadObservation{}, err
	}
	defer unlock()
	rec, err := c.fenceStop(want)
	if err != nil {
		return kr.WorkloadObservation{}, err
	}
	ct, found, err := c.findContainer(ctx, &rec)
	if err != nil && !errors.Is(err, errForeign) {
		return kr.WorkloadObservation{}, err
	}
	if found {
		if ct, found, err = c.stopContainer(ctx, rec, ct); err != nil {
			return kr.WorkloadObservation{}, err
		}
		if found && ct.State.Running {
			return k.observe(ctx, &rec, b) // still running: neither stopped nor removed
		}
		if found && remove {
			if _, found, err = c.removeContainer(ctx, rec); err != nil {
				return kr.WorkloadObservation{}, err
			}
			if found {
				return kr.WorkloadObservation{}, uncertain("the container of %s still exists after removal", b.WorkloadID)
			}
		}
	}
	// Nothing can run: the record is fenced and the container (if any) is not running.
	rec.Stopped = true
	if remove && !found && !rec.Unanswered {
		rec.Removed = true
	}
	if err := c.save(rec); err != nil {
		return kr.WorkloadObservation{}, err
	}
	return k.observe(ctx, &rec, b)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
