// Package podman is the runtime that runs attempt workloads as Podman
// containers: the digest-pinned image of the step, on a read-only root
// filesystem with every capability dropped, under CPU, memory and process
// limits. It is the backend of local end-to-end runs. It does not enforce a
// tenant's network policy or aggregate quota; Kubernetes does.
//
// What runs inside the container is an explicit Program: Entrypoint
// bind-mounts a Linux kodama binary and runs the real step entrypoint, which
// is admitted, runs the declared command and submits the result through the
// runtime API; DeclaredCommand runs the declared command directly.
//
// Every decision rests on durable state: a record per workload in StateDir
// holds its instance UID, the container ID once created, and whether it was
// stopped. Containers are acted on by verified ID only, never by their
// reusable name, so a restarted runtime adopts its own containers and never
// touches a foreign one holding the name.
package podman

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/runtime/bootstrap"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// Backend is the identity of the Podman runtime.
const Backend = "podman"

// Namespace is where runtime admission expects this runtime's workloads.
const Namespace = "kodama-podman"

// Container labels. They select and describe managed containers; ownership
// is decided by the durable record and the verified container ID.
const (
	LabelManagedBy   = "kodama.optimi.com/managed-by"
	ValueManagedBy   = "podman-runtime"
	LabelTenant      = "kodama.optimi.com/tenant"
	LabelProject     = "kodama.optimi.com/project"
	LabelAttempt     = "kodama.optimi.com/attempt"
	LabelUID         = "kodama.optimi.com/uid"
	LabelFingerprint = "kodama.optimi.com/spec-fingerprint"
)

// Config configures a Runtime.
type Config struct {
	// Program decides what the container runs. Required.
	Program Program
	// StateDir holds the durable workload records and the files mounted into
	// containers. Required; with a Podman machine it must be a path the
	// machine shares (on macOS, under the home directory).
	StateDir string
	// Binary is the podman executable (default "podman").
	Binary string
	// CPUs, Memory and PIDs bound every container (defaults 1, 512m, 256).
	CPUs   string
	Memory string
	PIDs   int
	// Network is the container network (default "none"). An Entrypoint
	// workload needs one that reaches the runtime API.
	Network string
	// StopTimeout is the grace between SIGTERM and SIGKILL (default 10s).
	StopTimeout time.Duration
	// RunResources enables run_shared attempts (AttemptSpec.Shared): the run's
	// workspace volume, created by Resources over the same StateDir, is verified
	// and mounted at /workspace. The Runtime then declares the run-resources
	// marker. Without it such an attempt is refused (execution.ErrNotConfigured).
	RunResources bool
	// WorkloadUID and WorkloadGID are the installation's workload identity
	// (defaults 65532): a run_shared attempt runs as this non-root user.
	WorkloadUID, WorkloadGID int
}

// Runtime implements execution.Runtime with Podman containers, and
// runtimeauth.JobObserver for the workloads it runs.
type Runtime struct {
	cfg Config

	fileLocks *lockSet
	// resLocks and res serve run_shared attempts: the run resources' locks and
	// records under StateDir/resources (nil without Config.RunResources).
	resLocks *lockSet
	res      *resStore
}

var (
	_ execution.Runtime       = (*Runtime)(nil)
	_ runtimeauth.JobObserver = (*Runtime)(nil)
)

// New validates the configuration.
func New(cfg Config) (*Runtime, error) {
	if cfg.Program == nil || cfg.StateDir == "" {
		return nil, fmt.Errorf("%w: the podman runtime needs a program and a state directory", execution.ErrNotConfigured)
	}
	dir, err := filepath.Abs(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	cfg.StateDir = dir
	def := func(p *string, v string) {
		if *p == "" {
			*p = v
		}
	}
	def(&cfg.Binary, "podman")
	def(&cfg.CPUs, "1")
	def(&cfg.Memory, "512m")
	def(&cfg.Network, "none")
	if cfg.PIDs <= 0 {
		cfg.PIDs = 256
	}
	if cfg.WorkloadUID <= 0 {
		cfg.WorkloadUID = 65532
	}
	if cfg.WorkloadGID <= 0 {
		cfg.WorkloadGID = 65532
	}
	if cfg.StopTimeout <= 0 {
		cfg.StopTimeout = 10 * time.Second
	}
	r := &Runtime{cfg: cfg, fileLocks: newLockSet(cfg.StateDir)}
	if cfg.RunResources {
		rs, err := newResStore(cfg.StateDir)
		if err != nil {
			return nil, err
		}
		r.res, r.resLocks = rs, newLockSet(rs.dir())
	}
	return r, nil
}

// Capabilities implements execution.Runtime.
//
// It never declares NetworkPolicy or ResourceQuotas: a private bridge or an
// internal run network is not the tenant's egress policy. With RunResources it
// also declares the companion-port marker, which adds no guarantee.
func (r *Runtime) Capabilities() execution.Capabilities {
	c := execution.Capabilities{
		Backend:    Backend,
		Guarantees: execution.OCIImage | execution.Isolation | execution.ResourceLimits | execution.CrashRecovery,
		// A stop before the start is recorded durably and fences the launch.
		ProvesNeverStarted: true,
	}
	if r.cfg.RunResources {
		c.Guarantees = c.Guarantees.WithRunResources()
	}
	return c
}

// Start implements execution.Runtime. The record is written, with a fresh
// instance UID, before any container exists; the container is created under
// the workload name with that UID, its ID recorded, and only then started.
// A stopped or removed workload is never started again (ErrConflict).
func (r *Runtime) Start(ctx context.Context, spec execution.AttemptSpec) (execution.Workload, error) {
	c, err := bootstrap.LaunchContext(spec)
	if err != nil {
		return execution.Workload{}, err
	}
	var shared *sharedMount
	if spec.Shared != nil {
		var release func()
		if shared, release, err = r.holdWorkspace(ctx, spec); err != nil {
			return execution.Workload{}, err
		}
		defer release()
	}
	unlock, err := r.lock(ctx, spec.WorkloadName)
	if err != nil {
		return execution.Workload{}, err
	}
	defer unlock()
	rec, err := r.records().begin(spec)
	if err != nil {
		return execution.Workload{}, err
	}
	w := spec.Workload(rec.UID)
	if rec.ContainerID == "" {
		ct, found, err := r.inspect(ctx, spec.WorkloadName)
		switch {
		case err != nil:
			return execution.Workload{}, err
		case found:
			// A create whose ID was never recorded, or a foreign container.
			if err := ct.owned(rec); err != nil {
				return execution.Workload{}, err
			}
			rec.ContainerID = ct.ID
			if shared != nil {
				// Adopted: the holder intent is bound to the container it made.
				if err := r.bindAttemptHolder(shared.volume, rec); err != nil {
					return execution.Workload{}, err
				}
			}
		default:
			if shared != nil {
				// At most one create is in flight for the attempt: an earlier one whose
				// answer was not seen may still land, so no replacement is issued. The
				// intent stays (see holderMarker) until the container is observed or an
				// operator retired the request (RetireUnresolvedRequest).
				if err := r.priorAttemptCreate(shared.volume, rec); err != nil {
					return execution.Workload{}, err
				}
			}
			cmd, err := r.cfg.Program.Container(ctx, Launch{Spec: spec, Context: c, Workload: w, Dir: r.records().dir(spec.WorkloadName)})
			if err != nil {
				return execution.Workload{}, err
			}
			if shared != nil {
				// The attempt holds the run workspace: its intent is durable, under the
				// workspace's lock, before the create is issued, and survives an ambiguous
				// answer (see holderMarker).
				epoch, err := r.res.epoch(shared.volume)
				if err != nil {
					return execution.Workload{}, err // nothing is announced, nothing is sent
				}
				if err := r.res.saveHolder(shared.volume, holderMarker{
					Workload: rec.Name, Kind: "attempt", RunID: rec.RunID, Labels: attemptLabels(rec, true),
					Epoch: epoch,
				}); err != nil {
					return execution.Workload{}, err
				}
			}
			if rec.ContainerID, err = r.create(ctx, rec, c.Command.Image, cmd, shared); err != nil {
				return execution.Workload{}, err
			}
			if shared != nil {
				// The answer was seen: the marker is bound to the container.
				if err := r.bindAttemptHolder(shared.volume, rec); err != nil {
					return execution.Workload{}, err
				}
			}
		}
		if err := r.records().save(rec); err != nil {
			return execution.Workload{}, err
		}
	}
	ct, found, err := r.inspect(ctx, rec.ContainerID)
	switch {
	case err != nil:
		return execution.Workload{}, err
	case !found:
		return execution.Workload{}, fmt.Errorf("%w: the created container is gone and is not recreated", execution.ErrConflict)
	}
	if err := ct.owned(rec); err != nil {
		return execution.Workload{}, err
	}
	if ct.State.Status == "created" {
		if shared != nil && shared.userns {
			// S3: the role's user namespace is verified on the created container; one
			// the engine did not isolate that way is removed, never started.
			if err := ct.userns(); err != nil {
				_ = r.podman(ctx, nil, "rm", "--force", "--time", "0", rec.ContainerID)
				return execution.Workload{}, err
			}
		}
		if err := r.podman(ctx, nil, "start", rec.ContainerID); err != nil {
			return execution.Workload{}, err
		}
	}
	return w, nil
}

// priorAttemptCreate refuses a second create for a shared attempt while an earlier
// one is unresolved. The holder marker is the durable intent written before the
// first create: unbound, the create may still land (the attempt is unknown, a
// retry finds the container by name once it exists); bound, the container was
// created and is gone, which is never recreated. A retirement the operator
// recorded after the intent was written ends it, and the attempt may create anew.
func (r *Runtime) priorAttemptCreate(ws string, rec record) error {
	ms, err := r.res.holders(ws)
	if err != nil {
		return err
	}
	for _, m := range ms {
		if m.Workload != rec.Name {
			continue
		}
		if m.ID != "" {
			return fmt.Errorf("%w: the created container is gone and is not recreated", execution.ErrConflict)
		}
		epoch, err := r.res.epoch(ws)
		if err != nil {
			return err // unreadable: neither retired nor resolved
		}
		if retiredBy(epoch, m.Epoch) {
			return r.res.dropHolder(ws, m.Workload) // retired by the operator
		}
		return fmt.Errorf("%w: an earlier create of this attempt is unresolved; no second is issued", execution.ErrUnavailable)
	}
	return nil
}

// bindAttemptHolder pins the attempt's container in its holder marker, keeping the
// epoch of the intent.
func (r *Runtime) bindAttemptHolder(ws string, rec record) error {
	epoch, err := r.res.epoch(ws)
	if err != nil {
		return err
	}
	m := holderMarker{Workload: rec.Name, Kind: "attempt", RunID: rec.RunID, Labels: attemptLabels(rec, true), Epoch: epoch}
	ms, err := r.res.holders(ws)
	if err != nil {
		return err
	}
	for _, old := range ms {
		if old.Workload == rec.Name {
			m.Epoch = old.Epoch
		}
	}
	m.ID = rec.ContainerID
	return r.res.saveHolder(ws, m)
}

// create builds and runs the container create. shared is non-nil for a
// run_shared attempt: its verified workspace volume, user, role capabilities and
// networks replace the attempt-private ones, and a Program that tries to supply
// /workspace itself is refused.
func (r *Runtime) create(ctx context.Context, rec record, image string, cmd Container, shared *sharedMount) (string, error) {
	args := append([]string{"create", "--name", rec.Name}, labelArgsOf(attemptLabels(rec, shared != nil))...)
	args = append(args, "--read-only", "--cap-drop=ALL")
	if shared == nil || !shared.escalate {
		args = append(args, "--security-opt=no-new-privileges")
	}
	args = append(args, "--pull=missing",
		"--cpus", r.cfg.CPUs, "--memory", r.cfg.Memory, "--pids-limit", strconv.Itoa(r.cfg.PIDs))
	if shared == nil {
		args = append(args, "--network", r.cfg.Network)
	} else {
		args = append(args, "--user", shared.user)
		if shared.userns {
			args = append(args, "--userns=auto")
		}
		for _, c := range shared.capAdd {
			args = append(args, "--cap-add", c)
		}
		for _, n := range shared.networks {
			args = append(args, "--network", n)
		}
	}
	args = append(args, "--stop-timeout", strconv.Itoa(int(r.cfg.StopTimeout.Seconds())))
	for _, t := range cmd.Tmpfs {
		if shared != nil && underWorkspace(t) {
			return "", fmt.Errorf("%w: the program provides /workspace, which a run_shared attempt takes from the run volume", execution.ErrInvalid)
		}
		args = append(args, "--tmpfs", t)
	}
	for _, m := range cmd.Mounts {
		if shared != nil && underWorkspace(m.Target) {
			return "", fmt.Errorf("%w: the program provides /workspace, which a run_shared attempt takes from the run volume", execution.ErrInvalid)
		}
		opt := m.Source + ":" + m.Target
		if m.ReadOnly {
			opt += ":ro"
		}
		args = append(args, "--volume", opt)
	}
	if shared != nil {
		// The run volume, read-write, owned by the workload user whatever image
		// the attempt runs.
		args = append(args, "--volume", shared.volume+":/workspace:U")
	}
	for _, e := range cmd.Env {
		args = append(args, "--env", e)
	}
	entrypoint, err := json.Marshal(cmd.Entrypoint)
	if err != nil {
		return "", err
	}
	args = append(append(args, "--entrypoint", string(entrypoint), image), cmd.Args...)
	var out bytes.Buffer
	if err := r.podman(ctx, &out, args...); err != nil {
		return "", err
	}
	id := strings.TrimSpace(out.String())
	if !containerID.MatchString(id) {
		return "", fmt.Errorf("%w: podman create returned no container ID", execution.ErrUnavailable)
	}
	return id, nil
}

func underWorkspace(target string) bool {
	return target == "/workspace" || strings.HasPrefix(target, "/workspace/")
}

// container is the part of `podman inspect` the runtime decides on.
type container struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Config struct {
		Labels      map[string]string `json:"Labels"`
		Annotations map[string]string `json:"Annotations"`
	} `json:"Config"`
	HostConfig struct {
		IDMappings struct {
			UidMap []string `json:"UidMap"`
			GidMap []string `json:"GidMap"`
		} `json:"IDMappings"`
	} `json:"HostConfig"`
	State struct {
		Status   string `json:"Status"`
		Running  bool   `json:"Running"`
		ExitCode int    `json:"ExitCode"`
	} `json:"State"`
}

// attemptLabels are the labels of an attempt's container: the one place they are
// built, so the holder marker of a run_shared attempt names exactly what its
// create gives the container.
func attemptLabels(rec record, shared bool) map[string]string {
	l := map[string]string{
		LabelManagedBy: ValueManagedBy, LabelTenant: string(rec.Scope.TenantID), LabelProject: string(rec.Scope.ProjectID),
		LabelAttempt: string(rec.AttemptID), LabelUID: rec.UID, LabelFingerprint: rec.Fingerprint,
	}
	if shared {
		l[LabelRun] = string(rec.RunID)
	}
	return l
}

// owned reports whether the container is the record's instance.
func (c container) owned(rec record) error {
	l := c.Config.Labels
	if strings.TrimPrefix(c.Name, "/") != rec.Name || l[LabelManagedBy] != ValueManagedBy || l[LabelUID] != rec.UID ||
		l[LabelAttempt] != string(rec.AttemptID) || l[LabelTenant] != string(rec.Scope.TenantID) || l[LabelFingerprint] != rec.Fingerprint ||
		(rec.ContainerID != "" && c.ID != rec.ContainerID) {
		return fmt.Errorf("%w: the container is not the workload's instance", execution.ErrConflict)
	}
	return nil
}

// userns verifies that the engine gave the container a user namespace of its own
// (--userns=auto): the engine records the choice as an annotation and the
// mapping it allocated. A container without both runs in the engine's shared
// namespace, which the pinned role does not allow.
func (c container) userns() error {
	if c.Config.Annotations["io.podman.annotations.userns"] != "auto" || len(c.HostConfig.IDMappings.UidMap) == 0 || len(c.HostConfig.IDMappings.GidMap) == 0 {
		return fmt.Errorf("%w: the engine did not give the container a user namespace of its own", execution.ErrNotConfigured)
	}
	return nil
}

// started reports whether the container's command ever ran.
func (c container) started() bool {
	return c.State.Status != "created" && c.State.Status != "configured"
}

// inspect reads a container by ID or name; found is false only when Podman
// definitely has none.
func (r *Runtime) inspect(ctx context.Context, ref string) (container, bool, error) {
	var out bytes.Buffer
	err := r.podman(ctx, &out, "container", "inspect", "--format", "json", ref)
	if errors.Is(err, errNoContainer) {
		return container{}, false, nil
	}
	if err != nil {
		return container{}, false, err
	}
	var cs []container
	if err := json.Unmarshal(out.Bytes(), &cs); err != nil || len(cs) != 1 {
		return container{}, false, fmt.Errorf("%w: unreadable podman inspect output", execution.ErrUnavailable)
	}
	return cs[0], true, nil
}

// workload returns the recorded instance a handle names and its container:
// ErrNotFound when nothing remains, ErrConflict for another attempt or
// instance.
func (r *Runtime) workload(ctx context.Context, h execution.Workload) (record, container, error) {
	if err := h.Validate(); err != nil {
		return record{}, container{}, err
	}
	rec, err := r.records().load(h.Name)
	if err != nil {
		return record{}, container{}, err
	}
	if err := rec.matches(h); err != nil {
		return record{}, container{}, err
	}
	if rec.ContainerID == "" || rec.Removed {
		return rec, container{}, fmt.Errorf("%w: no container", execution.ErrNotFound)
	}
	ct, found, err := r.inspect(ctx, rec.ContainerID)
	switch {
	case err != nil:
		return rec, container{}, err
	case !found:
		return rec, container{}, fmt.Errorf("%w: the container is gone", execution.ErrNotFound)
	}
	return rec, ct, ct.owned(rec)
}

// Observe implements execution.Runtime. A container that exited non-zero
// without being stopped failed; one that exited zero only ran: success is an
// accepted result, never an exit code.
func (r *Runtime) Observe(ctx context.Context, h execution.Workload) (execution.Observation, error) {
	rec, ct, err := r.workload(ctx, h)
	if err != nil {
		return execution.Observation{}, err
	}
	obs := execution.Observation{
		Kind: execution.ObservedDispatched, WorkloadName: rec.Name, SpecFingerprint: rec.Fingerprint,
		WorkloadUID: rec.UID, ObservedAt: time.Now().UTC(),
	}
	switch {
	case !ct.started():
	case !ct.State.Running && ct.State.ExitCode != 0 && !rec.Stopped:
		obs.Kind, obs.Class, obs.Reason, obs.Launch = execution.ObservedFailed, execution.ClassInfrastructure, execution.ReasonWorkloadFailed, execution.LaunchStarted
	default:
		obs.Kind, obs.Launch = execution.ObservedStarted, execution.LaunchStarted
	}
	return obs, nil
}

// Logs implements execution.Runtime with the container's captured output.
func (r *Runtime) Logs(ctx context.Context, h execution.Workload, dst io.Writer) error {
	rec, _, err := r.workload(ctx, h)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, r.cfg.Binary, "logs", rec.ContainerID)
	cmd.Stdout, cmd.Stderr = dst, dst
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: podman logs: %w", execution.ErrUnavailable, err)
	}
	return nil
}

// Stop implements execution.Runtime. The record is marked stopped first, so
// no later Start launches the workload; a container that never started is
// removed, a running one is stopped. Stopped is reported once the container
// no longer runs.
func (r *Runtime) Stop(ctx context.Context, h execution.Workload) (execution.StopEvidence, error) {
	if err := h.Validate(); err != nil {
		return execution.StopEvidence{}, err
	}
	unlock, err := r.lock(ctx, h.Name)
	if err != nil {
		return execution.StopEvidence{}, err
	}
	defer unlock()
	rec, err := r.records().stop(h)
	if err != nil {
		return execution.StopEvidence{}, err
	}
	if rec.ContainerID == "" {
		// A create may have landed without its ID being recorded.
		ct, found, err := r.inspect(ctx, rec.Name)
		switch {
		case err != nil:
			return execution.StopEvidence{}, err
		case !found:
			return execution.StopEvidence{Stopped: true, Launch: execution.LaunchNotStarted}, nil
		case ct.owned(rec) != nil:
			// A foreign container holds the name: ours was never created.
			return execution.StopEvidence{Stopped: true, Launch: execution.LaunchNotStarted}, nil
		}
		rec.ContainerID = ct.ID
		if err := r.records().save(rec); err != nil {
			return execution.StopEvidence{}, err
		}
	}
	ct, found, err := r.inspect(ctx, rec.ContainerID)
	switch {
	case err != nil:
		return execution.StopEvidence{}, err
	case !found:
		return execution.StopEvidence{Stopped: true, Launch: rec.Launch}, nil
	}
	if !ct.started() {
		if err := r.podman(ctx, nil, "rm", "--force", rec.ContainerID); err != nil && !errors.Is(err, errNoContainer) {
			return execution.StopEvidence{}, err
		}
		return r.stopped(rec, execution.LaunchNotStarted)
	}
	if ct.State.Running {
		err := r.podman(ctx, nil, "stop", "--time", strconv.Itoa(int(r.cfg.StopTimeout.Seconds())), rec.ContainerID)
		if err != nil && !errors.Is(err, errNoContainer) {
			return execution.StopEvidence{}, err
		}
		if ct, found, err = r.inspect(ctx, rec.ContainerID); err != nil {
			return execution.StopEvidence{}, err
		}
		if found && ct.State.Running {
			return execution.StopEvidence{Launch: execution.LaunchStarted}, nil
		}
	}
	return r.stopped(rec, execution.LaunchStarted)
}

func (r *Runtime) stopped(rec record, launch execution.LaunchEvidence) (execution.StopEvidence, error) {
	rec.Launch = launch
	if err := r.records().save(rec); err != nil {
		return execution.StopEvidence{}, err
	}
	return execution.StopEvidence{Stopped: true, Launch: launch}, nil
}

// Remove implements execution.Runtime: it stops the workload, removes its
// container by ID and its mounted files, and keeps the record as a tombstone.
func (r *Runtime) Remove(ctx context.Context, h execution.Workload) error {
	ev, err := r.Stop(ctx, h)
	switch {
	case err != nil:
		return err
	case !ev.Stopped:
		return execution.ErrBusy
	}
	unlock, err := r.lock(ctx, h.Name)
	if err != nil {
		return err
	}
	defer unlock()
	rec, err := r.records().load(h.Name)
	if err != nil {
		return err
	}
	if rec.ContainerID != "" {
		if err := r.podman(ctx, nil, "rm", "--force", "--volumes", rec.ContainerID); err != nil && !errors.Is(err, errNoContainer) {
			return err
		}
	}
	return r.records().remove(rec)
}

// ObserveJob implements runtimeauth.JobObserver for the entrypoint of a
// running workload: one container stands for both the Job and its Pod.
func (r *Runtime) ObserveJob(ctx context.Context, obs runtimeauth.JobObservation) (runtimeauth.JobEvidence, error) {
	if obs.Expected.Namespace != Namespace {
		return runtimeauth.JobEvidence{Namespace: Namespace}, nil
	}
	rec, err := r.records().load(obs.Expected.Name)
	switch {
	case errors.Is(err, execution.ErrNotFound):
		return runtimeauth.JobEvidence{}, runtimeauth.ErrNotObserved
	case err != nil:
		return runtimeauth.JobEvidence{}, err
	case rec.Scope != obs.Scope || rec.AttemptID != obs.AttemptID:
		return runtimeauth.JobEvidence{Namespace: Namespace}, nil
	}
	_, ct, err := r.workload(ctx, rec.handle())
	switch {
	case errors.Is(err, execution.ErrNotFound):
		return runtimeauth.JobEvidence{}, runtimeauth.ErrNotObserved
	case errors.Is(err, execution.ErrConflict):
		return runtimeauth.JobEvidence{Namespace: Namespace}, nil
	case err != nil:
		return runtimeauth.JobEvidence{}, err
	case !ct.State.Running || rec.Stopped:
		return runtimeauth.JobEvidence{}, runtimeauth.ErrNotObserved
	}
	return evidence(rec), nil
}

// errNoContainer: Podman has no such container.
var errNoContainer = errors.New("podman: no such container")

// podman runs one podman command; its output goes to out (discarded when nil).
func (r *Runtime) podman(ctx context.Context, out io.Writer, args ...string) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, r.cfg.Binary, args...)
	cmd.Stdout, cmd.Stderr = out, &stderr
	err := cmd.Run()
	switch {
	case err == nil:
		return nil
	case strings.Contains(stderr.String(), "no such container"):
		return errNoContainer
	case errors.As(err, new(*exec.ExitError)):
		return fmt.Errorf("%w: podman %s: %s", execution.ErrUnavailable, args[0], strings.TrimSpace(stderr.String()))
	}
	return fmt.Errorf("%w: podman %s: %w", execution.ErrUnavailable, args[0], err)
}
