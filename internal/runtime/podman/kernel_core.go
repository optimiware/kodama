package podman

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	kr "github.com/optimiware/kodama/internal/kernel/runtime"
)

// This file is the shared core of the kernel-contract adapters (KernelRuntime
// and KernelResources). The kernel files of this package import only the
// standard library, internal/domain and internal/kernel/runtime: no shell, SQL,
// broker, Git, task, identity or agent package.
//
// The adapter keeps one durable record per workload or resource under
// StateDir/kernel/NAME/record.json. The record is written BEFORE the engine
// object exists and is kept as a tombstone after removal, so that
//
//   - a creation whose answer was lost is found again by its name and instance
//     UID and adopted, never created a second time;
//   - a stop fences the identity: a delayed create carrying the same or an older
//     fence is rejected and nothing is created;
//   - the fence of every accepted writer ratchets up, so a writer superseded by
//     a takeover cannot start, stop or remove anything;
//   - engine objects are acted on by the ID the record bound, never by their
//     reusable name, and only after their labels, UID and scope are verified.
//
// NAME is derived from the tenant, project and stable identity, so two scopes
// can never reach each other's objects, and an identity unknown to this scope
// is simply absent.

// Labels and class values of the kernel adapters' engine objects.
const (
	// ValueKernelManagedBy marks the objects of the kernel adapters, distinct
	// from the legacy adapter's value.
	ValueKernelManagedBy = "podman-kernel-runtime"
	// LabelClass names what an object is for.
	LabelClass = "kodama.optimi.com/class"
	// LabelSpecDigest carries the immutable specification digest.
	LabelSpecDigest = "kodama.optimi.com/spec-digest"

	classWorkload    = "workload"
	classWorkspace   = "workspace"
	classService     = "service"
	classPreparation = "preparation"
	classNetwork     = "network"
)

// KernelCapabilities are the guarantees the Podman backend enforces and the
// real-engine tests hold it to. It deliberately omits network_isolation (an
// internal bridge is not a filtered egress path) and persistent_services
// (project- and tenant-lifetime services are refused).
var KernelCapabilities = kr.CapabilitySet{
	kr.CapNonRoot, kr.CapResourceLimits, kr.CapImagePinning, kr.CapAdoptWorkload, kr.CapFencedStop, kr.CapRunServices,
}

// Preparer supplies the container that fills a run workspace from pinned
// sources. The adapter owns the volume and the container lifecycle; the
// preparer owns what "fetch this source" means, so this package never speaks
// Git. Without a Preparer a workspace with sources is refused before any
// effect.
type Preparer interface {
	// Preparation returns the one-shot container that prepares /workspace for
	// the pinned inputs. It must be deterministic for one Digest.
	Preparation(ctx context.Context, in kr.PreparationInputs) (PreparationJob, error)
}

// PreparationJob is a one-shot preparation container. It runs non-root on a
// read-only root filesystem with the workspace at /workspace; it succeeds by
// exiting 0.
type PreparationJob struct {
	// Image is the pinned image (it must carry a digest).
	Image   string
	Command []string
	Env     map[string]string
	// Network overrides the configured workload network.
	Network string
	// SecretsDir, when set, is mounted read-only at /kodama/secrets.
	SecretsDir string
}

// KernelConfig configures KernelRuntime and KernelResources. Both are opened
// over the same StateDir so a restarted process adopts what the previous one
// left.
type KernelConfig struct {
	// StateDir holds the durable records. Required; with a Podman machine it
	// must be a path the machine shares.
	StateDir string
	// Binary is the podman executable (default "podman").
	Binary string
	// CPUs, Memory and PIDs bound every container (defaults 1, 512m, 256).
	CPUs   string
	Memory string
	PIDs   int
	// Network is the network of workloads without services and of preparation
	// (default "none").
	Network string
	// StopTimeout is the grace between SIGTERM and SIGKILL (default 10s).
	StopTimeout time.Duration
	// WorkloadUID and WorkloadGID are the non-root identity every container runs
	// as (defaults 65532).
	WorkloadUID, WorkloadGID int
	// Preparer prepares workspaces that have sources (optional).
	Preparer Preparer
	// Clock supplies observation time (default the wall clock).
	Clock kr.Clock
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now().UTC() }

// kernelCore is the state and engine access both adapters share.
type kernelCore struct {
	cfg   KernelConfig
	root  string
	locks *lockSet
	pm    kpodman
}

func newKernelCore(cfg KernelConfig) (*kernelCore, error) {
	if cfg.StateDir == "" {
		return nil, errors.New("podman: the kernel adapters need a state directory")
	}
	dir, err := filepath.Abs(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(dir, "kernel")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	if cfg.Binary == "" {
		cfg.Binary = "podman"
	}
	if cfg.CPUs == "" {
		cfg.CPUs = "1"
	}
	if cfg.Memory == "" {
		cfg.Memory = "512m"
	}
	if cfg.Network == "" {
		cfg.Network = "none"
	}
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
	if cfg.Clock == nil {
		cfg.Clock = wallClock{}
	}
	cfg.StateDir = dir
	return &kernelCore{cfg: cfg, root: root, locks: newLockSet(root), pm: kpodman{binary: cfg.Binary}}, nil
}

func (c *kernelCore) now() time.Time { return c.cfg.Clock.Now().UTC() }

// objectName derives the engine name of an identity inside a scope.
func objectName(prefix string, scope domain.Scope, parts ...string) string {
	h := sha256.New()
	h.Write([]byte(scope.TenantID))
	h.Write([]byte{0})
	h.Write([]byte(scope.ProjectID))
	for _, p := range parts {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return prefix + hex.EncodeToString(h.Sum(nil))[:32]
}

// pinned reports whether an image reference carries a content digest.
func pinned(image string) bool {
	i := strings.Index(image, "@sha256:")
	return i > 0 && len(image)-i-len("@sha256:") == 64
}

// refuse marks an error as raised before any engine effect.
func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: "+fmt.Sprintf(format, args...), kr.ErrNotReached)
}

// uncertain marks an error as an effect with an unknown outcome.
func uncertain(format string, args ...any) error {
	return fmt.Errorf("%w: "+fmt.Sprintf(format, args...), kr.ErrUncertain)
}

// errForeign: an engine object holds the name but is not the record's.
var errForeign = errors.New("podman: the object is not this record's instance")

// errLost: the record's object was created and is gone.
var errLost = errors.New("podman: the created object is gone")

// ---------------------------------------------------------------------------
// Durable records
// ---------------------------------------------------------------------------

// krec is the durable state of one workload or resource.
type krec struct {
	Name       string       `json:"name"`
	Class      string       `json:"class"`
	Scope      domain.Scope `json:"scope"`
	RunID      domain.RunID `json:"runId,omitempty"`
	Identity   string       `json:"identity"`
	SpecDigest string       `json:"specDigest"`
	// UID is the instance identity the engine object is labelled with. A tombstone
	// written for an identity nobody recorded has none.
	UID string `json:"uid,omitempty"`
	// Fence is the highest fence of an accepted writer.
	Fence kr.Fence `json:"fence"`
	// ObjectID pins the engine identity once the object exists: the container ID
	// or the volume's creation time.
	ObjectID string `json:"objectId,omitempty"`
	// Announced counts the creation requests issued; Unanswered is true while the
	// latest one's outcome is unresolved. Time never settles it: the object is
	// found by name and UID and adopted, or the engine's answer is seen.
	Announced  int  `json:"announced,omitempty"`
	Unanswered bool `json:"unanswered,omitempty"`
	// Stopping is the stop intent (the fence); Stopped the verified stop.
	Stopping bool `json:"stopping,omitempty"`
	Stopped  bool `json:"stopped,omitempty"`
	Removed  bool `json:"removed,omitempty"`
	// Untracked marks a tombstone made for an identity that was never recorded.
	Untracked bool `json:"untracked,omitempty"`
	// Timeout is the workload's declared runtime limit.
	TimeoutNanos int64 `json:"timeoutNanos,omitempty"`
	// Ports are a service's declared ports.
	Ports []int `json:"ports,omitempty"`
	// Prepared is the digest of the preparation inputs applied to a workspace.
	Prepared string `json:"prepared,omitempty"`
	// PrepRequired is the digest of the preparation inputs a workspace needs
	// applied before it is ready; empty for a source-free workspace.
	PrepRequired string `json:"prepRequired,omitempty"`
	// PrepFailed records a failed preparation.
	PrepFailed bool `json:"prepFailed,omitempty"`
	// Network is the run network a service or workload joined.
	Network string `json:"network,omitempty"`
	// NetPending is the durable obligation of a removed workload or resource to
	// see the run's network cleaned up: set before the cleanup is attempted and
	// cleared only when the network is confirmed removed or handed to a container
	// that still uses it. While it is set the removal is not confirmed.
	NetPending bool `json:"netPending,omitempty"`
	// Cleaning is a network's accepted cleanup intent, persisted before removal.
	Cleaning bool `json:"cleaning,omitempty"`
	// Workspace is the record name of the workspace a workload mounts: the
	// durable holder intent the workspace's release and removal consult.
	Workspace string `json:"workspace,omitempty"`
}

func (r krec) fenced() bool { return r.Stopping || r.Stopped || r.Removed }

func (c *kernelCore) dir(name string) string  { return filepath.Join(c.root, name) }
func (c *kernelCore) file(name string) string { return filepath.Join(c.root, name, "record.json") }

func (c *kernelCore) load(name string) (krec, bool, error) {
	raw, err := os.ReadFile(c.file(name))
	if errors.Is(err, fs.ErrNotExist) {
		return krec{}, false, nil
	}
	if err != nil {
		return krec{}, false, fmt.Errorf("podman: reading the record of %s: %w", name, err)
	}
	var rec krec
	if err := json.Unmarshal(raw, &rec); err != nil || rec.Name != name {
		return krec{}, false, fmt.Errorf("podman: the record of %s is unreadable", name)
	}
	return rec, true, nil
}

func (c *kernelCore) create(rec krec) error { return c.write(rec, os.Link) }
func (c *kernelCore) save(rec krec) error   { return c.write(rec, os.Rename) }

func (c *kernelCore) write(rec krec, place func(from, to string) error) error {
	dir := c.dir(rec.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return writeDurably(dir, c.file(rec.Name), raw, place)
}

// admit loads the record of an identity for a writer, or records a new one with
// a fresh instance UID. It enforces the shared rules of Start and Ensure: a
// digest other than the recorded one is ErrSpecMismatch, a fenced identity or a
// fence older than the recorded one is ErrStaleFence, and a newer fence is
// ratcheted. The caller holds the lock.
func (c *kernelCore) admit(want krec) (krec, error) {
	rec, found, err := c.load(want.Name)
	if err != nil {
		return krec{}, err
	}
	if !found {
		want.UID = newUID()
		if err := c.create(want); err != nil {
			if errors.Is(err, fs.ErrExist) {
				return c.admit(want) // a concurrent writer recorded it first
			}
			return krec{}, refuse("recording %s: %v", want.Name, err)
		}
		return want, nil
	}
	if err := recordMatches(rec, want); err != nil {
		return krec{}, err
	}
	if rec.Untracked && rec.fenced() {
		return krec{}, fmt.Errorf("%w: %s was stopped before it was ensured", kr.ErrStaleFence, want.Identity)
	}
	if rec.SpecDigest != want.SpecDigest {
		return krec{}, fmt.Errorf("%w: %s is recorded with specification %s, not %s", kr.ErrSpecMismatch, want.Identity, rec.SpecDigest, want.SpecDigest)
	}
	if rec.Workspace != want.Workspace {
		return krec{}, fmt.Errorf("%w: %s is recorded on another workspace", kr.ErrSpecMismatch, want.Identity)
	}
	if want.Fence < rec.Fence {
		return krec{}, fmt.Errorf("%w: fence %d is older than %d", kr.ErrStaleFence, want.Fence, rec.Fence)
	}
	if rec.fenced() {
		return krec{}, fmt.Errorf("%w: %s was stopped and is never created again", kr.ErrStaleFence, want.Identity)
	}
	if want.Fence > rec.Fence {
		rec.Fence = want.Fence
		if err := c.save(rec); err != nil {
			return krec{}, refuse("recording the fence of %s: %v", want.Name, err)
		}
	}
	return rec, nil
}

// recordMatches checks that a record is the identity a caller names: same
// scope, run and class. A mismatch is a collision, never an adoption.
func recordMatches(rec, want krec) error {
	if rec.Scope != want.Scope || rec.RunID != want.RunID || rec.Class != want.Class || rec.Identity != want.Identity {
		return fmt.Errorf("%w: %s belongs to another identity", kr.ErrSpecMismatch, rec.Name)
	}
	return nil
}

// fenceStop loads the record for a stop or remove and ratchets it to the stop
// intent (and, when want.NetPending is set, to the removal's network cleanup
// obligation, in the same write). A missing record leaves a tombstone, so a delayed create for the same
// identity is rejected. A stop from a stale fence is refused and changes
// nothing. The caller holds the lock.
func (c *kernelCore) fenceStop(want krec) (krec, error) {
	rec, found, err := c.load(want.Name)
	if err != nil {
		return krec{}, err
	}
	if !found {
		want.Untracked, want.Stopping, want.Stopped = true, true, true
		if err := c.create(want); err != nil {
			if errors.Is(err, fs.ErrExist) {
				return c.fenceStop(want)
			}
			return krec{}, err
		}
		return want, nil
	}
	if err := recordMatches(rec, want); err != nil {
		return krec{}, err
	}
	if want.Fence < rec.Fence {
		return krec{}, fmt.Errorf("%w: fence %d is older than %d", kr.ErrStaleFence, want.Fence, rec.Fence)
	}
	if want.Fence > rec.Fence || !rec.Stopping || (want.NetPending && !rec.NetPending) {
		rec.Fence, rec.Stopping = want.Fence, true
		// The network cleanup obligation of a removal is part of the same durable
		// write as the accepted stop intent, so it exists before any operation can
		// make the object absent.
		rec.NetPending = rec.NetPending || want.NetPending
		if err := c.save(rec); err != nil {
			return krec{}, err
		}
	}
	return rec, nil
}

// ---------------------------------------------------------------------------
// Engine access
// ---------------------------------------------------------------------------

// kpodman runs podman commands. It distinguishes what the engine SAID (an exit
// code) from what could not be asked at all.
type kpodman struct{ binary string }

type kres struct {
	stdout, stderr string
	code           int
}

// run executes podman. The error is non-nil only when the command could not be
// started (an error wrapping kr.ErrNotReached) or was interrupted after it may
// have reached the engine (an error wrapping kr.ErrUncertain); a non-zero exit
// is reported in the result.
func (p kpodman) run(ctx context.Context, args ...string) (kres, error) {
	if err := ctx.Err(); err != nil {
		return kres{}, fmt.Errorf("%w: podman %s: %w", kr.ErrNotReached, args[0], err)
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, p.binary, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return kres{stdout: stdout.String(), stderr: stderr.String()}, nil
	case ctx.Err() != nil:
		return kres{}, fmt.Errorf("%w: podman %s: %w", kr.ErrUncertain, args[0], ctx.Err())
	case errors.As(err, &exit):
		return kres{stdout: stdout.String(), stderr: stderr.String(), code: exit.ExitCode()}, nil
	}
	return kres{}, fmt.Errorf("%w: podman %s could not be run: %w", kr.ErrNotReached, args[0], err)
}

// query runs a read-only podman command; any failure is plain (retry-able, never
// evidence of absence).
func (p kpodman) query(ctx context.Context, args ...string) (kres, error) {
	res, err := p.run(ctx, args...)
	if err != nil {
		return res, err
	}
	if res.code != 0 {
		return res, fmt.Errorf("podman %s: %s", args[0], strings.TrimSpace(res.stderr))
	}
	return res, nil
}

// exists asks whether a container, volume or network exists. Only exit codes 0
// and 1 are answers; anything else is an engine failure, never absence.
func (p kpodman) exists(ctx context.Context, kind, ref string) (bool, error) {
	res, err := p.run(ctx, kind, "exists", ref)
	if err != nil {
		return false, err
	}
	switch res.code {
	case 0:
		return true, nil
	case 1:
		return false, nil
	}
	return false, fmt.Errorf("podman %s exists: %s", kind, strings.TrimSpace(res.stderr))
}

// kctr is the part of `podman container inspect` the adapter decides on.
type kctr struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	State struct {
		Status     string    `json:"Status"`
		Running    bool      `json:"Running"`
		ExitCode   int       `json:"ExitCode"`
		StartedAt  time.Time `json:"StartedAt"`
		FinishedAt time.Time `json:"FinishedAt"`
	} `json:"State"`
}

// started reports whether the container's command ever ran.
func (c kctr) started() bool { return c.State.Status != "created" && c.State.Status != "configured" }

// inspectContainer reads a container by ID or name; found is false only when
// the engine definitively has none.
func (p kpodman) inspectContainer(ctx context.Context, ref string) (kctr, bool, error) {
	ok, err := p.exists(ctx, "container", ref)
	if err != nil || !ok {
		return kctr{}, false, err
	}
	res, err := p.run(ctx, "container", "inspect", "--format", "json", ref)
	if err != nil {
		return kctr{}, false, err
	}
	if res.code != 0 {
		if strings.Contains(res.stderr, "no such container") {
			return kctr{}, false, nil
		}
		return kctr{}, false, fmt.Errorf("podman inspect: %s", strings.TrimSpace(res.stderr))
	}
	var all []kctr
	if err := json.Unmarshal([]byte(res.stdout), &all); err != nil || len(all) != 1 {
		return kctr{}, false, errors.New("podman: unreadable container inspect output")
	}
	return all[0], true, nil
}

// kvol is the part of `podman volume inspect` the adapter decides on.
type kvol struct {
	Name      string            `json:"Name"`
	Labels    map[string]string `json:"Labels"`
	CreatedAt string            `json:"CreatedAt"`
}

func (p kpodman) inspectVolume(ctx context.Context, name string) (kvol, bool, error) {
	ok, err := p.exists(ctx, "volume", name)
	if err != nil || !ok {
		return kvol{}, false, err
	}
	res, err := p.run(ctx, "volume", "inspect", "--format", "json", name)
	if err != nil {
		return kvol{}, false, err
	}
	if res.code != 0 {
		if strings.Contains(res.stderr, "no such volume") {
			return kvol{}, false, nil
		}
		return kvol{}, false, fmt.Errorf("podman volume inspect: %s", strings.TrimSpace(res.stderr))
	}
	var all []kvol
	if err := json.Unmarshal([]byte(res.stdout), &all); err != nil || len(all) != 1 {
		return kvol{}, false, errors.New("podman: unreadable volume inspect output")
	}
	return all[0], true, nil
}

// ambiguous reports a failure text that does not prove the request was never
// processed (a connection lost after it was sent).
func ambiguous(stderr string) bool {
	s := strings.ToLower(stderr)
	for _, p := range []string{"eof", "reset by peer", "broken pipe", "timeout", "timed out", "deadline", "connection closed", "unexpected"} {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// refusalEvidence are the texts by which the engine, or the CLI before sending
// anything, positively reports that a create request did not take effect: the
// name is taken, the image cannot be resolved, the request is invalid, the
// engine cannot be connected to.
var refusalEvidence = []string{
	"already in use", "already exists", "already used", "no such image", "image not known", "unable to find image",
	"manifest unknown", "invalid reference", "unauthorized", "requested access to the resource is denied",
	"cannot connect to podman", "connection refused",
}

// answered reports positive evidence that a failed create request was refused or
// never sent: a real exit status (a CLI killed by a signal has none, code -1)
// with an error text that names a known refusal. A signal, an empty or an
// unrecognized failure is not evidence of anything and leaves the request
// unresolved, as does a text that a lost connection could also produce.
func answered(res kres) bool {
	if res.code <= 0 || ambiguous(res.stderr) {
		return false
	}
	s := strings.ToLower(res.stderr)
	for _, p := range refusalEvidence {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// labelsOf are the labels every kernel-adapter object carries. Ownership is
// decided from these together with the record.
func labelsOf(rec krec) map[string]string {
	l := map[string]string{
		LabelManagedBy: ValueKernelManagedBy, LabelTenant: string(rec.Scope.TenantID), LabelProject: string(rec.Scope.ProjectID),
		LabelClass: rec.Class, LabelUID: rec.UID, LabelSpecDigest: rec.SpecDigest,
	}
	if rec.RunID != "" {
		l[LabelRun] = string(rec.RunID)
	}
	return l
}

func labelArgs(l map[string]string) []string {
	keys := slices.Sorted(maps.Keys(l))
	var out []string
	for _, k := range keys {
		out = append(out, "--label", k+"="+l[k])
	}
	return out
}

// owned reports whether labels belong to the record's instance.
func owned(labels map[string]string, rec krec) bool {
	return labels[LabelManagedBy] == ValueKernelManagedBy && rec.UID != "" && labels[LabelUID] == rec.UID &&
		labels[LabelClass] == rec.Class && labels[LabelTenant] == string(rec.Scope.TenantID) &&
		labels[LabelProject] == string(rec.Scope.ProjectID) && labels[LabelSpecDigest] == rec.SpecDigest
}

// ctrSpec is one container to create.
type ctrSpec struct {
	image      string
	entrypoint []string
	args       []string
	env        []string
	// volumes are --volume values.
	volumes  []string
	tmpfs    []string
	networks []string
	aliases  []string
	timeout  time.Duration
}

func (c *kernelCore) createArgs(rec krec, s ctrSpec) []string {
	args := append([]string{"create", "--name", rec.Name}, labelArgs(labelsOf(rec))...)
	args = append(args, "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--pull=missing",
		"--cpus", c.cfg.CPUs, "--memory", c.cfg.Memory, "--pids-limit", strconv.Itoa(c.cfg.PIDs),
		"--user", strconv.Itoa(c.cfg.WorkloadUID)+":"+strconv.Itoa(c.cfg.WorkloadGID),
		"--stop-timeout", strconv.Itoa(int(c.cfg.StopTimeout.Seconds())))
	if s.timeout > 0 {
		args = append(args, "--timeout", strconv.Itoa(max(1, int(s.timeout.Seconds()))))
	}
	for _, n := range s.networks {
		args = append(args, "--network", n)
	}
	for _, a := range s.aliases {
		args = append(args, "--network-alias", a)
	}
	for _, t := range s.tmpfs {
		args = append(args, "--tmpfs", t)
	}
	for _, v := range s.volumes {
		args = append(args, "--volume", v)
	}
	for _, e := range s.env {
		args = append(args, "--env", e)
	}
	if s.entrypoint != nil {
		ep, _ := json.Marshal(s.entrypoint)
		args = append(args, "--entrypoint", string(ep))
	}
	return append(append(args, s.image), s.args...)
}

// findContainer returns the record's container: by the bound ID, otherwise by
// name when the labels prove it is this instance (binding it). A container of
// the right name that is not ours is errForeign: never adopted, stopped or
// removed. A bound container that is gone is errLost.
func (c *kernelCore) findContainer(ctx context.Context, rec *krec) (kctr, bool, error) {
	if rec.ObjectID != "" {
		ct, found, err := c.pm.inspectContainer(ctx, rec.ObjectID)
		switch {
		case err != nil:
			return kctr{}, false, err
		case !found:
			return kctr{}, false, nil
		case ct.ID != rec.ObjectID || !owned(ct.Config.Labels, *rec):
			return kctr{}, false, errForeign
		}
		return ct, true, nil
	}
	ct, found, err := c.pm.inspectContainer(ctx, rec.Name)
	switch {
	case err != nil:
		return kctr{}, false, err
	case !found:
		return kctr{}, false, nil
	case strings.TrimPrefix(ct.Name, "/") != rec.Name || !owned(ct.Config.Labels, *rec):
		// A foreign object holds the name. It proves nothing about an outstanding
		// create: that request may still land once the name is free, so the
		// unresolved evidence is kept. It resolves only through the owned instance
		// or an answer to the request itself.
		return kctr{}, false, errForeign
	}
	rec.ObjectID, rec.Unanswered = ct.ID, false
	if err := c.save(*rec); err != nil {
		return kctr{}, false, err
	}
	return ct, true, nil
}

// ensureContainer finds or creates the record's container. A creation is
// announced durably before the engine is asked; an outcome that is not seen is
// reconciled by name and UID and never replayed: while one is unresolved no
// second create is issued. The caller holds the lock and has admitted the record.
func (c *kernelCore) ensureContainer(ctx context.Context, rec *krec, build func() (ctrSpec, error)) (kctr, error) {
	ct, found, err := c.findContainer(ctx, rec)
	switch {
	case errors.Is(err, errForeign):
		return kctr{}, fmt.Errorf("%w: a foreign container holds the name %s", kr.ErrSpecMismatch, rec.Name)
	case err != nil:
		return kctr{}, err
	case found:
		return ct, nil
	case rec.ObjectID != "":
		return kctr{}, fmt.Errorf("%w: the created container of %s", errLost, rec.Identity)
	case rec.Unanswered:
		return kctr{}, uncertain("an earlier create of %s is unresolved; none is replayed", rec.Identity)
	}
	spec, err := build()
	if err != nil {
		return kctr{}, err
	}
	args := c.createArgs(*rec, spec)
	rec.Announced++
	rec.Unanswered = true
	if err := c.save(*rec); err != nil {
		rec.Announced--
		rec.Unanswered = false
		return kctr{}, refuse("announcing the create of %s: %v", rec.Identity, err)
	}
	res, err := c.pm.run(ctx, args...)
	switch {
	case err != nil && errors.Is(err, kr.ErrNotReached):
		rec.Unanswered = false
		_ = c.save(*rec)
		return kctr{}, err
	case err != nil:
		return kctr{}, err // interrupted: unresolved, reconciled by name
	case res.code != 0:
		// The answer is an error. Look at the engine before believing it: a lost
		// response can still have created the container.
		ct, found, ierr := c.findContainer(ctx, rec)
		switch {
		case found:
			return ct, nil
		case errors.Is(ierr, errForeign):
			if !answered(res) {
				return kctr{}, uncertain("podman create (exit %d): %s", res.code, strings.TrimSpace(res.stderr))
			}
			// The engine answered the request with a refusal (the name is taken):
			// the answer, not the foreign object, resolves it.
			rec.Unanswered = false
			if err := c.save(*rec); err != nil {
				return kctr{}, uncertain("recording the rejected create of %s: %v", rec.Identity, err)
			}
			return kctr{}, fmt.Errorf("%w: a foreign container holds the name %s", kr.ErrSpecMismatch, rec.Name)
		case ierr != nil:
			return kctr{}, uncertain("podman create failed (%s) and the engine could not be asked: %v", strings.TrimSpace(res.stderr), ierr)
		case !answered(res):
			return kctr{}, uncertain("podman create (exit %d): %s", res.code, strings.TrimSpace(res.stderr))
		}
		rec.Unanswered = false
		if err := c.save(*rec); err != nil {
			return kctr{}, uncertain("recording the rejected create of %s: %v", rec.Identity, err)
		}
		return kctr{}, fmt.Errorf("%w: podman create rejected: %s", kr.ErrNotReached, strings.TrimSpace(res.stderr))
	}
	id := strings.TrimSpace(res.stdout)
	if !containerID.MatchString(id) {
		return kctr{}, uncertain("podman create returned no container ID")
	}
	rec.ObjectID, rec.Unanswered = id, false
	if err := c.save(*rec); err != nil {
		return kctr{}, uncertain("recording the container of %s: %v", rec.Identity, err)
	}
	ct, found, err = c.pm.inspectContainer(ctx, id)
	switch {
	case err != nil:
		return kctr{}, err
	case !found:
		return kctr{}, fmt.Errorf("%w: the created container of %s", errLost, rec.Identity)
	}
	return ct, nil
}

// startContainer starts a created container.
func (c *kernelCore) startContainer(ctx context.Context, rec krec, ct kctr) (kctr, error) {
	if ct.State.Status != "created" && ct.State.Status != "configured" {
		return ct, nil
	}
	res, err := c.pm.run(ctx, "start", rec.ObjectID)
	if err != nil {
		return ct, err
	}
	if res.code != 0 {
		// The start may have happened: observe, do not assume.
		if now, found, ierr := c.pm.inspectContainer(ctx, rec.ObjectID); ierr == nil && found && now.started() {
			return now, nil
		}
		return ct, uncertain("podman start: %s", strings.TrimSpace(res.stderr))
	}
	now, found, err := c.pm.inspectContainer(ctx, rec.ObjectID)
	if err != nil || !found {
		return ct, err
	}
	return now, nil
}

// stopContainer stops a container by its bound ID and reports the state after.
// A container that never started is removed: its command never ran and cannot.
func (c *kernelCore) stopContainer(ctx context.Context, rec krec, ct kctr) (kctr, bool, error) {
	if !ct.started() {
		return c.removeContainer(ctx, rec)
	}
	if ct.State.Running {
		res, err := c.pm.run(ctx, "stop", "--time", strconv.Itoa(int(c.cfg.StopTimeout.Seconds())), rec.ObjectID)
		if err != nil {
			return ct, true, err
		}
		if res.code != 0 && !strings.Contains(res.stderr, "no such container") {
			return ct, true, uncertain("podman stop: %s", strings.TrimSpace(res.stderr))
		}
		now, found, err := c.pm.inspectContainer(ctx, rec.ObjectID)
		if err != nil || !found {
			return ct, found, err
		}
		ct = now
	}
	return ct, true, nil
}

// removeContainer removes the container by ID and confirms the engine no longer
// has it.
func (c *kernelCore) removeContainer(ctx context.Context, rec krec) (kctr, bool, error) {
	res, err := c.pm.run(ctx, "rm", "--force", "--volumes", "--time", "0", rec.ObjectID)
	if err != nil {
		return kctr{}, true, err
	}
	if res.code != 0 && !strings.Contains(res.stderr, "no such container") {
		return kctr{}, true, uncertain("podman rm: %s", strings.TrimSpace(res.stderr))
	}
	ct, found, err := c.pm.inspectContainer(ctx, rec.ObjectID)
	return ct, found, err
}

// logStream streams `podman logs` of a container as one reader.
func (c *kernelCore) logStream(ctx context.Context, id string, follow bool) io.ReadCloser {
	ctx, cancel := context.WithCancel(ctx)
	args := []string{"logs"}
	if follow {
		args = append(args, "--follow")
	}
	args = append(args, id)
	pr, pw := io.Pipe()
	cmd := exec.CommandContext(ctx, c.cfg.Binary, args...)
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = pw, io.MultiWriter(pw, &stderr)
	go func() {
		err := cmd.Run()
		if err != nil && ctx.Err() == nil {
			err = fmt.Errorf("podman logs: %w: %s", err, strings.TrimSpace(stderr.String()))
		} else {
			err = nil
		}
		pw.CloseWithError(err)
	}()
	return &logReader{PipeReader: pr, cancel: cancel}
}

type logReader struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (l *logReader) Close() error {
	l.cancel()
	return l.PipeReader.Close()
}
