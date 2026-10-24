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
	"sync"
	"time"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

// Backend is the identity of the Podman runtime.
const Backend = "podman"

// Namespace is where runtime admission expects this runtime's workloads.
const Namespace = "kodama-podman"

// Container labels. They select and describe managed containers; ownership
// is decided by the durable record and the verified container ID.
const (
	LabelManagedBy   = "kodama.optimi.dev/managed-by"
	ValueManagedBy   = "podman-runtime"
	LabelTenant      = "kodama.optimi.dev/tenant"
	LabelProject     = "kodama.optimi.dev/project"
	LabelAttempt     = "kodama.optimi.dev/attempt"
	LabelUID         = "kodama.optimi.dev/uid"
	LabelFingerprint = "kodama.optimi.dev/spec-fingerprint"
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
}

// Runtime implements execution.Runtime with Podman containers, and
// runtimeauth.JobObserver for the workloads it runs.
type Runtime struct {
	cfg Config

	mu    sync.Mutex
	locks map[string]chan struct{}
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
	if cfg.StopTimeout <= 0 {
		cfg.StopTimeout = 10 * time.Second
	}
	return &Runtime{cfg: cfg, locks: map[string]chan struct{}{}}, nil
}

// Capabilities implements execution.Runtime.
func (r *Runtime) Capabilities() execution.Capabilities {
	return execution.Capabilities{
		Backend:    Backend,
		Guarantees: execution.OCIImage | execution.Isolation | execution.ResourceLimits | execution.CrashRecovery,
		// A stop before the start is recorded durably and fences the launch.
		ProvesNeverStarted: true,
	}
}

// Start implements execution.Runtime. The record is written, with a fresh
// instance UID, before any container exists; the container is created under
// the workload name with that UID, its ID recorded, and only then started.
// A stopped or removed workload is never started again (ErrConflict).
func (r *Runtime) Start(ctx context.Context, spec execution.AttemptSpec) (execution.Workload, error) {
	c, err := spec.DecodeContext()
	if err != nil {
		return execution.Workload{}, err
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
		default:
			cmd, err := r.cfg.Program.Container(ctx, Launch{Spec: spec, Context: c, Workload: w, Dir: r.records().dir(spec.WorkloadName)})
			if err != nil {
				return execution.Workload{}, err
			}
			if rec.ContainerID, err = r.create(ctx, rec, c.Command.Image, cmd); err != nil {
				return execution.Workload{}, err
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
		if err := r.podman(ctx, nil, "start", rec.ContainerID); err != nil {
			return execution.Workload{}, err
		}
	}
	return w, nil
}

func (r *Runtime) create(ctx context.Context, rec record, image string, cmd Container) (string, error) {
	args := []string{
		"create", "--name", rec.Name,
		"--label", LabelManagedBy + "=" + ValueManagedBy, "--label", LabelTenant + "=" + string(rec.Scope.TenantID),
		"--label", LabelProject + "=" + string(rec.Scope.ProjectID), "--label", LabelAttempt + "=" + string(rec.AttemptID),
		"--label", LabelUID + "=" + rec.UID, "--label", LabelFingerprint + "=" + rec.Fingerprint,
		"--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--pull=missing",
		"--cpus", r.cfg.CPUs, "--memory", r.cfg.Memory, "--pids-limit", strconv.Itoa(r.cfg.PIDs),
		"--network", r.cfg.Network, "--stop-timeout", strconv.Itoa(int(r.cfg.StopTimeout.Seconds())),
	}
	for _, t := range cmd.Tmpfs {
		args = append(args, "--tmpfs", t)
	}
	for _, m := range cmd.Mounts {
		opt := m.Source + ":" + m.Target
		if m.ReadOnly {
			opt += ":ro"
		}
		args = append(args, "--volume", opt)
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

// container is the part of `podman inspect` the runtime decides on.
type container struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	State struct {
		Status   string `json:"Status"`
		Running  bool   `json:"Running"`
		ExitCode int    `json:"ExitCode"`
	} `json:"State"`
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
