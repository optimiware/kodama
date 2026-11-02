package podman

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/runtime/bootstrap"
)

// Labels of the run resources' engine objects. They select and describe the
// objects; ownership is decided by the durable record, the labels together with
// the instance UID, and the engine identity pinned in the record.
const (
	// LabelProbeNonce marks the requests that create a service's tcp-probe container.
	LabelProbeNonce         = "kodama.optimi.com/probe-nonce"
	ValueManagedByResources = "podman-run-resources"
	LabelRun                = "kodama.optimi.com/run"
	LabelKind               = "kodama.optimi.com/resource-kind"
	LabelKey                = "kodama.optimi.com/resource-key"
)

// ResourceCapabilities is the named set the Podman backend enforces for every
// run, each member held by the fake-engine program assertions and the real
// `make test-podman` suite: a run-owned named volume mounted by successive
// attempts (run_workspace), released only when no container of the engine still
// references it (workspace_fence), a controller-selected preparation container
// (workspace_preparation), adoption from durable records and engine labels
// (resource_recovery), services with readiness and run-unique endpoint aliases
// (run_services) and separate container filesystems, identities and credentials
// without any /workspace mount (service_isolation).
//
// It deliberately does not advertise run_network_isolation, declared_edges,
// named_egress, egress_default_deny or any builder member: the run network is an
// internal bridge, which is not an owned filtered egress path and is not
// traffic-tested against foreign runs, so a policy needing those is refused at
// admission. Together with the missing network_policy guarantee of the attempt
// runtime no run_shared admission accepts this backend today; local evidence
// never certifies Kubernetes.
var ResourceCapabilities = execution.Set(
	execution.CapRunWorkspace, execution.CapWorkspaceFence, execution.CapWorkspacePreparation,
	execution.CapResourceRecovery, execution.CapRunServices, execution.CapServiceIsolation,
)

// PreparationCheckout issues the read-only checkout material of a run's
// preparation: the repository credential and the Git CA. Like
// bootstrap.CheckoutProvider it owns scope and issue; the adapter owns only the
// delivery, as read-only files in the preparation's private bootstrap
// directory, never an argument, environment variable or label. It answers only
// for the repository the preparation spec names. An error matching
// bootstrap.ErrCheckoutDenied is a definite refusal; any other error is an
// outage and Ensure is repeated.
type PreparationCheckout interface {
	Checkout(ctx context.Context, spec execution.ResourceSpec) (bootstrap.CheckoutMaterial, error)
}

// ResourcesConfig configures Resources.
type ResourcesConfig struct {
	// StateDir holds the durable records and bootstrap files (under
	// StateDir/resources). Share it with Config.StateDir of the attempt Runtime.
	// With a Podman machine it must be a path the machine shares. Required.
	StateDir string
	// Binary is the podman executable (default "podman").
	Binary string
	// EntrypointBinary is the host path of a static Linux kodama binary,
	// bind-mounted read-only at /kodama/bin/kodama into a preparation (it runs
	// `kodama prepare`) and into a service with secret bindings (it runs
	// `kodama service-run`). Required for those.
	EntrypointBinary string
	// ServerURL is the runtime API origin as the containers reach it and CA its
	// optional PEM trust roots.
	ServerURL string
	CA        []byte
	// ControlNetwork is the engine network that reaches ServerURL (and, for a
	// preparation, the Git host). Preparation and secret-bound service containers
	// join it beside the run network; any other service sees the run network
	// only. Required for those.
	ControlNetwork string
	// Registrar registers each workload's admission credential (seam S5). nil
	// issues none.
	Registrar ResourceRegistrar
	// Checkout issues a preparation's checkout material; nil refuses a
	// preparation with bootstrap.ErrNoCheckout.
	Checkout PreparationCheckout
	// ProbeImage is a digest-pinned image with `nc` (BusyBox is enough) that
	// probes a service's tcp readiness from inside the run network. Required for
	// tcp readiness.
	ProbeImage string
	// WorkloadUID and WorkloadGID are the installation's workload identity
	// (defaults 65532); preparation and attempts run as this non-root user.
	WorkloadUID, WorkloadGID int
	// CPUs, Memory and PIDs bound a preparation (defaults 1, 512m, 256); services
	// take their spec's limits.
	CPUs   string
	Memory string
	PIDs   int
	// StopTimeout is the grace between SIGTERM and SIGKILL (default 10s).
	StopTimeout time.Duration
}

// Resources implements execution.RunResources with Podman objects: a run network,
// a run volume, a preparation container and service containers. Every object
// carries ownership labels and the adapter acts on it only after verifying its
// durable record, labels and engine identity, by container ID and never by the
// reusable name alone. There is no host engine socket in any container, and a
// workload joins only the run network (plus, when it must reach the Kodama
// server, the configured control network).
type Resources struct {
	cfg   ResourcesConfig
	e     engine
	st    *resStore
	locks *lockSet
	// hook, when set by a test, is called at named points of a creation request;
	// an error aborts it there, as a controller crash would (nothing after is
	// recorded).
	hook func(point string) error
}

// InjectCrash installs a hook that tests use to simulate a controller crash: it
// is called with "sent" right after a creation request was sent and before its
// answer is recorded, and an error it returns aborts the call there with nothing
// of the answer recorded. It is never set in production.
func (r *Resources) InjectCrash(hook func(point string) error) { r.hook = hook }

func (r *Resources) at(point string) error {
	if r.hook == nil {
		return nil
	}
	return r.hook(point)
}

var _ execution.RunResources = (*Resources)(nil)

// NewResources validates the configuration.
func NewResources(cfg ResourcesConfig) (*Resources, error) {
	if cfg.StateDir == "" {
		return nil, fmt.Errorf("%w: the podman resources need a state directory", execution.ErrNotConfigured)
	}
	dir, err := filepath.Abs(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	cfg.StateDir = dir
	st, err := newResStore(dir)
	if err != nil {
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
	return &Resources{cfg: cfg, e: engine{binary: cfg.Binary}, st: st, locks: newLockSet(st.dir())}, nil
}

// Capabilities implements execution.RunResources.
func (r *Resources) Capabilities(context.Context) execution.ResourceCapabilities {
	return execution.ResourceCapabilities{Backend: Backend, Set: ResourceCapabilities}
}

// engineKind is the podman noun of a resource kind's object.
func engineKind(k execution.ResourceKind) string {
	switch k {
	case execution.KindNetwork:
		return "network"
	case execution.KindWorkspace:
		return "volume"
	}
	return "container"
}

func (rec resRecord) labels() map[string]string {
	return map[string]string{
		LabelManagedBy: ValueManagedByResources, LabelTenant: string(rec.Scope.TenantID), LabelProject: string(rec.Scope.ProjectID),
		LabelRun: string(rec.RunID), LabelKind: string(rec.Kind), LabelKey: rec.Key, LabelUID: rec.UID, LabelFingerprint: rec.Fingerprint,
	}
}

func (rec resRecord) labelArgs() []string {
	l := rec.labels()
	var args []string
	for _, k := range slices.Sorted(maps.Keys(l)) {
		args = append(args, "--label", k+"="+l[k])
	}
	return args
}

// ours reports whether an engine object's labels are this record's.
func (rec resRecord) ours(labels map[string]string) bool {
	return labelsMatch(rec.labels(), labels)
}

// find returns the record a handle names. found is false when nothing of ours
// exists at the name; an engine object without a record is not ours, and a
// handle bound to an instance meeting one, or a record of another owner or
// instance, is a collision.
func (r *Resources) find(ctx context.Context, h execution.ResourceHandle) (rec resRecord, found bool, err error) {
	rec, found, err = r.st.load(h.Name)
	if err != nil {
		return resRecord{}, false, err
	}
	if found {
		if !rec.owns(h) {
			return resRecord{}, false, execution.ErrResourceCollision
		}
		return rec, true, nil
	}
	exists, err := r.e.exists(ctx, engineKind(h.Kind), h.Name)
	switch {
	case err != nil:
		return resRecord{}, false, err
	case exists && h.UID != "":
		return resRecord{}, false, execution.ErrResourceCollision
	}
	return resRecord{}, false, nil
}

// Ensure implements execution.RunResources. The record, with the instance UID,
// is written before anything exists; an engine object at the name without our
// record is foreign (collision); a stopped or removed record is fenced.
func (r *Resources) Ensure(ctx context.Context, spec execution.ResourceSpec) (execution.ResourceObservation, error) {
	if err := spec.Validate(); err != nil {
		return execution.ResourceObservation{}, err
	}
	if spec.Backend != Backend {
		return execution.ResourceObservation{}, fmt.Errorf("%w: the spec is for backend %q", execution.ErrInvalid, spec.Backend)
	}
	fp, err := spec.Fingerprint()
	if err != nil {
		return execution.ResourceObservation{}, err
	}
	plan, err := r.plan(spec)
	if err != nil {
		return execution.ResourceObservation{}, err
	}
	unlock, err := r.locks.lock(ctx, spec.Name)
	if err != nil {
		return execution.ResourceObservation{}, err
	}
	defer unlock()
	rec, found, err := r.st.load(spec.Name)
	if err != nil {
		return execution.ResourceObservation{}, err
	}
	switch {
	case found:
		if rec.Scope != spec.Scope || rec.RunID != spec.RunID || rec.Kind != spec.Kind || rec.Key != spec.Key {
			return execution.ResourceObservation{}, execution.ErrResourceCollision
		}
		// A stopped or removed name is fenced whatever the spec (a tombstone left
		// by a stop before the create has no fingerprint).
		if rec.fenced() {
			return execution.ResourceObservation{}, execution.ErrResourceFenced
		}
		if rec.Fingerprint != fp {
			return execution.ResourceObservation{}, execution.ErrResourceCollision
		}
	default:
		exists, err := r.e.exists(ctx, engineKind(spec.Kind), spec.Name)
		switch {
		case err != nil:
			return execution.ResourceObservation{}, err
		case exists:
			return execution.ResourceObservation{}, execution.ErrResourceCollision
		}
		rec = resRecord{
			Name: spec.Name, Scope: spec.Scope, RunID: spec.RunID, Kind: spec.Kind, Key: spec.Key, UID: newUID(), Fingerprint: fp, Probe: plan.probe,
		}
		if err := r.st.create(rec); err != nil {
			if errors.Is(err, fs.ErrExist) {
				return execution.ResourceObservation{}, fmt.Errorf("%w: a concurrent create recorded the resource", execution.ErrUnavailable)
			}
			return execution.ResourceObservation{}, err
		}
	}
	switch spec.Kind {
	case execution.KindNetwork:
		err = r.ensureNetwork(ctx, &rec)
	case execution.KindWorkspace:
		err = r.ensureVolume(ctx, &rec)
	default:
		err = r.ensureContainer(ctx, &rec, spec, plan)
	}
	if err != nil {
		return execution.ResourceObservation{}, err
	}
	if err := r.st.save(rec); err != nil {
		return execution.ResourceObservation{}, err
	}
	return r.observe(ctx, rec)
}

// verified reports whether an engine object is the record's: labels (with the
// instance UID and fingerprint) and, once pinned, the engine identity.
func (rec resRecord) verified(labels map[string]string, objectID string) error {
	if !rec.ours(labels) || (rec.ObjectID != "" && objectID != rec.ObjectID) {
		return execution.ErrResourceCollision
	}
	return nil
}

// errUnresolved: an earlier creation request has not been resolved.
var errUnresolved = fmt.Errorf("%w: an earlier creation request for the name is unresolved", execution.ErrUnavailable)

// issue makes one creation request. It is announced durably before the engine is
// asked and counted unanswered; the caller records the answer TOGETHER with the
// identity of the object it made in one write (see answer), so an answer is never
// recorded without the object it belongs to. No request is issued while an
// earlier one is unresolved: at most one is ever in flight for a name, and it is
// resolved only by observation (the object is found and ours) or by an operator's
// explicit retirement of the request (RetireUnresolvedRequest).
func (r *Resources) issue(ctx context.Context, rec *resRecord, args ...string) (result, error) {
	if rec.Unanswered > 0 {
		return result{}, errUnresolved
	}
	epoch, err := r.st.epoch(rec.Name)
	if err != nil {
		return result{}, err // nothing is announced, nothing is sent
	}
	rec.announce(time.Now().UTC(), epoch)
	if err := r.st.save(*rec); err != nil {
		return result{}, err
	}
	res, err := r.e.run(ctx, args...)
	if herr := r.at("sent"); herr != nil {
		return res, herr // simulated death: nothing of the answer is recorded
	}
	if err == nil && !res.ok() {
		err = fmt.Errorf("%w: podman %s: %s", execution.ErrUnavailable, args[0], strings.TrimSpace(res.stderr))
	}
	return res, err // on error the request stays unanswered
}

// bind records, in one write, the answer of the request that made the object (the
// one just issued, or the one whose answer was lost and whose object was found by
// name) and the identity of the object. A bound object counts no request twice.
func (r *Resources) bind(rec *resRecord, objectID string) error {
	if !rec.Created {
		rec.answered()
	}
	rec.Created, rec.ObjectID = true, objectID
	if err := r.st.save(*rec); err != nil {
		return err
	}
	// A preparation's holder intent is bound with it, wherever the object was found
	// (a stop or removal that adopts it must not leave the intent unresolved).
	r.bindHolder(rec)
	return nil
}

func (r *Resources) ensureNetwork(ctx context.Context, rec *resRecord) error {
	nw, found, err := r.e.network(ctx, rec.Name)
	switch {
	case err != nil:
		return err
	case !found && rec.Created:
		return nil // created once and gone: never replaced; Observe reports it absent
	case !found:
		// Internal: no route out of the run network. A workload that needs the
		// Kodama server also joins the control network; this is isolation by
		// construction of the attachments, not an egress policy.
		args := append([]string{"network", "create", "--internal"}, rec.labelArgs()...)
		if _, err := r.issue(ctx, rec, append(args, rec.Name)...); err != nil {
			// The request may still land: a retry finds it by name.
			return err
		}
		if nw, found, err = r.e.network(ctx, rec.Name); err != nil {
			return err
		} else if !found {
			return fmt.Errorf("%w: the created network is not visible", execution.ErrUnavailable)
		}
	default:
	}
	if err := rec.verified(nw.Labels, nw.ID); err != nil {
		return err
	}
	// The answer and the object it made are recorded in one write.
	return r.bind(rec, nw.ID)
}

func (r *Resources) ensureVolume(ctx context.Context, rec *resRecord) error {
	vol, found, err := r.e.volume(ctx, rec.Name)
	switch {
	case err != nil:
		return err
	case !found && rec.Created:
		return nil
	case !found:
		args := append([]string{"volume", "create"}, rec.labelArgs()...)
		if _, err := r.issue(ctx, rec, append(args, rec.Name)...); err != nil {
			return err
		}
		if vol, found, err = r.e.volume(ctx, rec.Name); err != nil {
			return err
		} else if !found {
			return fmt.Errorf("%w: the created volume is not visible", execution.ErrUnavailable)
		}
	default:
	}
	if err := rec.verified(vol.Labels, vol.CreatedAt); err != nil {
		return err
	}
	return r.bind(rec, vol.CreatedAt)
}

// plan is what Ensure derives from a spec before any effect: the probe the
// record keeps and whether the workload needs the entrypoint binary.
type plan struct {
	probe *probeRec
	// supervised: the workload runs through the injected entrypoint binary
	// (a preparation, or a service with secret bindings).
	supervised bool
}

// plan validates everything the adapter must be able to enforce and refuses
// the rest before the first effect: an unprobeable readiness, a missing
// entrypoint or control network, a secret-bound service without a command.
func (r *Resources) plan(spec execution.ResourceSpec) (plan, error) {
	var p plan
	switch spec.Kind {
	case execution.KindPreparation:
		p.supervised = true
	case execution.KindService:
		s := spec.Service
		p.supervised = len(s.Secrets) > 0
		if p.supervised && len(s.Command) == 0 {
			return p, fmt.Errorf("%w: a service with secret bindings must declare its command", execution.ErrInvalid)
		}
		rd := s.Readiness
		p.probe = &probeRec{Kind: rd.Probe, TimeoutSecs: rd.ProbeTimeout}
		switch rd.Probe {
		case "tcp":
			port, ok := s.Ports[rd.Port]
			if !ok || port < 1 || port > 65535 {
				return p, fmt.Errorf("%w: the readiness port %q is not declared", execution.ErrInvalid, rd.Port)
			}
			if r.cfg.ProbeImage == "" {
				return p, fmt.Errorf("%w: tcp readiness needs a probe image", execution.ErrNotConfigured)
			}
			p.probe.Host, p.probe.Port = s.Host, port
		case "exec":
			if len(rd.Exec) == 0 {
				return p, fmt.Errorf("%w: the exec probe has no command", execution.ErrInvalid)
			}
			p.probe.Exec = slices.Clone(rd.Exec)
		default:
			return p, fmt.Errorf("%w: the podman backend probes tcp and exec readiness only", execution.ErrInvalid)
		}
		if s.Host == "" {
			return p, fmt.Errorf("%w: the service has no endpoint host", execution.ErrInvalid)
		}
	}
	if p.supervised {
		switch {
		case r.cfg.EntrypointBinary == "":
			return p, fmt.Errorf("%w: no entrypoint binary is configured for %s", execution.ErrNotConfigured, spec.Kind)
		case r.cfg.ControlNetwork == "":
			return p, fmt.Errorf("%w: no control network is configured for %s", execution.ErrNotConfigured, spec.Kind)
		}
	}
	return p, nil
}

// ---- containers ----

// Container paths of a preparation or service workload.
const (
	pathWorkspace = "/workspace"
	pathKodama    = pathTools + "/kodama"
)

func (r *Resources) ensureContainer(ctx context.Context, rec *resRecord, spec execution.ResourceSpec, p plan) error {
	if rec.cid() != "" {
		ct, found, err := r.e.container(ctx, rec.cid())
		switch {
		case err != nil:
			return err
		case !found:
			return nil // created once and gone: never replaced; Observe reports it absent
		}
		return r.startIfCreated(ctx, rec, ct)
	}
	ct, found, err := r.e.container(ctx, rec.Name)
	if err != nil {
		return err
	}
	if found {
		// A create whose ID was never recorded, or a foreign container.
		if err := rec.verified(ct.Config.Labels, ""); err != nil {
			return err
		}
		if err := r.bind(rec, ct.ID); err != nil {
			return err
		}
		return r.startIfCreated(ctx, rec, ct)
	}
	args, release, err := r.createArgs(ctx, rec, spec, p)
	if err != nil {
		return err
	}
	defer release()
	// A preparation holds the run workspace: its intent is durable, under the
	// workspace's lock, before the create is issued, and survives an ambiguous
	// answer (see holderMarker).
	if err := r.markHolder(rec, spec); err != nil {
		return err
	}
	res, err := r.issue(ctx, rec, args...)
	if err != nil {
		return err
	}
	id := strings.TrimSpace(res.stdout)
	if !containerID.MatchString(id) {
		// An answer that names nothing is no answer: the request stays unanswered.
		return fmt.Errorf("%w: podman create returned no container ID", execution.ErrUnavailable)
	}
	// The answer and the container's ID are durable, in one write, before it starts.
	if err := r.bind(rec, id); err != nil {
		return err
	}
	ct, found, err = r.e.container(ctx, id)
	switch {
	case err != nil:
		return err
	case !found:
		return fmt.Errorf("%w: the created container is not visible", execution.ErrUnavailable)
	}
	return r.startIfCreated(ctx, rec, ct)
}

// wsOf is the name of the run workspace of a resource's run.
func (rec resRecord) wsName() string {
	return execution.ResourceName(rec.Scope, rec.RunID, execution.KindWorkspace, execution.KeyWorkspace)
}

// markHolder records, before its create is issued, that a preparation is about to
// use the run workspace.
func (r *Resources) markHolder(rec *resRecord, spec execution.ResourceSpec) error {
	if rec.Kind != execution.KindPreparation {
		return nil
	}
	epoch, err := r.st.epoch(rec.wsName())
	if err != nil {
		return err
	}
	return r.st.saveHolder(rec.wsName(), holderMarker{
		Workload: rec.Name, Kind: "preparation", RunID: rec.RunID, Labels: rec.labels(),
		Epoch: epoch,
	})
}

// bindHolder pins the container's ID in the preparation's holder marker once its
// creation is known.
func (r *Resources) bindHolder(rec *resRecord) {
	if rec.Kind != execution.KindPreparation {
		return
	}
	ms, err := r.st.holders(rec.wsName())
	if err != nil {
		return
	}
	for _, m := range ms {
		if m.Workload == rec.Name && m.ID == "" {
			m.ID = rec.ObjectID
			_ = r.st.saveHolder(rec.wsName(), m)
		}
	}
}

// cid is the pinned container ID of a workload record.
func (rec resRecord) cid() string {
	if rec.Kind.HasIdentity() {
		return rec.ObjectID
	}
	return ""
}

func (r *Resources) startIfCreated(ctx context.Context, rec *resRecord, ct container) error {
	if err := rec.verified(ct.Config.Labels, ct.ID); err != nil {
		return err
	}
	if ct.State.Status == "created" || ct.State.Status == "configured" {
		// The role says the workload runs in a user namespace of its own: that is
		// verified on the created container before it starts, and a container the
		// engine did not isolate that way is removed, never started.
		if err := ct.userns(); err != nil {
			_, _ = r.e.run(ctx, "rm", "--force", "--time", "0", ct.ID)
			return err
		}
		if _, err := r.e.must(ctx, "start", ct.ID); err != nil {
			return err
		}
		rec.Launch = execution.LaunchStarted
	}
	return nil
}

// createArgs builds the create command of a preparation or service. The
// returned release removes what a refused or failed create left that must not
// survive (nothing is left on success).
func (r *Resources) createArgs(ctx context.Context, rec *resRecord, spec execution.ResourceSpec, p plan) ([]string, func(), error) {
	nop := func() {}
	user := strconv.Itoa(r.cfg.WorkloadUID) + ":" + strconv.Itoa(r.cfg.WorkloadGID)
	runNet := execution.ResourceName(spec.Scope, spec.RunID, execution.KindNetwork, execution.KeyNetwork)
	if err := r.requireRunNetwork(ctx, spec, runNet); err != nil {
		return nil, nop, err
	}
	args := []string{"create", "--name", rec.Name}
	args = append(args, rec.labelArgs()...)
	args = append(args, "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--pull=missing", "--userns=auto",
		"--user", user, "--stop-timeout", strconv.Itoa(int(r.cfg.StopTimeout.Seconds())), "--network", runNet)
	if p.supervised {
		args = append(args, "--network", r.cfg.ControlNetwork)
	}

	env := []string{"HOME=/tmp", "TMPDIR=/tmp"}
	var cmdline []string // the entrypoint and arguments after the image
	var entrypoint []string
	var image string
	mounts := []string{}
	bootstrapDir := r.st.bootstrap(rec.Name)

	switch spec.Kind {
	case execution.KindPreparation:
		prep := spec.Preparation
		image = prep.Image
		material, err := r.checkout(ctx, spec)
		if err != nil {
			return nil, nop, err
		}
		if err := r.writeBootstrap(ctx, spec, bootstrapDir); err != nil {
			return nil, nop, err
		}
		if err := bootstrap.WriteCheckout(bootstrapDir, *material); err != nil {
			return nil, nop, err
		}
		if err := shareWithWorkload(bootstrapDir); err != nil {
			return nil, nop, err
		}
		wsName := execution.ResourceName(spec.Scope, spec.RunID, execution.KindWorkspace, execution.KeyWorkspace)
		unlock, err := r.holdWorkspace(ctx, spec, wsName)
		if err != nil {
			return nil, nop, err
		}
		nop = unlock
		args = append(args, "--cpus", r.cfg.CPUs, "--memory", r.cfg.Memory, "--pids-limit", strconv.Itoa(r.cfg.PIDs),
			"--tmpfs", "/tmp:rw,size=256m,mode=1777")
		mounts = append(mounts, wsName+":"+pathWorkspace+":U")
		env = append(env, r.identityEnv(spec, rec.UID)...)
		env = append(env, "KODAMA_WORKSPACE_DIR="+pathWorkspace,
			"KODAMA_CHECKOUT_CREDENTIAL_FILE="+pathBootstrap+"/"+bootstrap.CheckoutCredentialFile,
			"KODAMA_GIT_CA_FILE="+pathBootstrap+"/"+bootstrap.GitCAFile)
		entrypoint, cmdline = []string{pathKodama}, []string{"prepare"}
	case execution.KindService:
		s := spec.Service
		image = s.Image
		if err := r.writeBootstrap(ctx, spec, bootstrapDir); err != nil {
			return nil, nop, err
		}
		memory := s.Resources.MemoryMiB
		for _, st := range s.Storage {
			memory += st.SizeMiB
		}
		args = append(args, "--network-alias", s.Host, "--pids-limit", strconv.Itoa(r.cfg.PIDs),
			"--cpus", cpuArg(s.Resources.CPUMillis, r.cfg.CPUs), "--memory", memArg(memory, r.cfg.Memory),
			"--tmpfs", "/tmp:rw,size="+sizeArg(s.Resources.EphemeralMiB, 64)+",mode=1777")
		if s.Resources.ShmMiB > 0 {
			args = append(args, "--shm-size", strconv.FormatInt(s.Resources.ShmMiB, 10)+"m")
		}
		for _, st := range s.Storage {
			args = append(args, "--tmpfs", fmt.Sprintf("%s:rw,size=%dm,mode=0700,uid=%d,gid=%d", st.Path, st.SizeMiB, r.cfg.WorkloadUID, r.cfg.WorkloadGID))
		}
		for _, k := range slices.Sorted(maps.Keys(s.Env)) {
			env = append(env, k+"="+s.Env[k])
		}
		env = append(env, r.identityEnv(spec, rec.UID)...)
		if p.supervised {
			entrypoint = []string{pathKodama}
			cmdline = append([]string{"service-run", "--"}, slices.Concat(s.Command, s.Args)...)
		} else {
			entrypoint, cmdline = s.Command, s.Args
		}
	}
	if p.supervised {
		mounts = append(mounts, r.cfg.EntrypointBinary+":"+pathKodama+":ro")
	}
	if _, err := os.Stat(bootstrapDir); err == nil {
		mounts = append(mounts, bootstrapDir+":"+pathBootstrap+":ro")
	}
	for _, m := range mounts {
		args = append(args, "--volume", m)
	}
	for _, e := range env {
		args = append(args, "--env", e)
	}
	if len(entrypoint) > 0 {
		raw, err := json.Marshal(entrypoint)
		if err != nil {
			return nil, nop, err
		}
		args = append(args, "--entrypoint", string(raw))
	}
	args = append(args, image)
	return append(args, cmdline...), nop, nil
}

func cpuArg(millis int64, def string) string {
	if millis <= 0 {
		return def
	}
	return fmt.Sprintf("%d.%03d", millis/1000, millis%1000)
}

func memArg(mib int64, def string) string {
	if mib <= 0 {
		return def
	}
	return strconv.FormatInt(mib, 10) + "m"
}

func sizeArg(mib, def int64) string {
	if mib <= 0 {
		mib = def
	}
	return strconv.FormatInt(mib, 10) + "m"
}

// identityEnv is the S4 environment of a workload outside Kubernetes: no Pod
// variables, since no Pod exists. The credential and CA are files in the
// read-only bootstrap mount.
func (r *Resources) identityEnv(spec execution.ResourceSpec, uid string) []string {
	env := []string{
		"KODAMA_RUN_ID=" + string(spec.RunID), "KODAMA_RESOURCE_KEY=" + spec.Key,
		"KODAMA_WORKLOAD_NAME=" + spec.Name, "KODAMA_WORKLOAD_UID=" + uid,
	}
	if r.cfg.ServerURL != "" {
		env = append(env, "KODAMA_SERVER_URL="+r.cfg.ServerURL)
	}
	if len(r.cfg.CA) > 0 {
		env = append(env, "KODAMA_CA_FILE="+pathBootstrap+"/ca.crt")
	}
	if r.cfg.Registrar != nil {
		env = append(env, "KODAMA_ADMISSION_CREDENTIAL_FILE="+pathBootstrap+"/"+ResourceCredentialFile)
	}
	return env
}

// writeBootstrap fills the workload's private read-only mount: the admission
// credential (persisted, then registered) and the runtime CA.
func (r *Resources) writeBootstrap(ctx context.Context, spec execution.ResourceSpec, dir string) error {
	if len(r.cfg.CA) == 0 && r.cfg.Registrar == nil {
		return nil // nothing to deliver
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if len(r.cfg.CA) > 0 {
		if err := writeDurably(dir, filepath.Join(dir, "ca.crt"), r.cfg.CA, os.Rename); err != nil {
			return err
		}
	}
	if _, err := r.admit(ctx, spec, dir); err != nil {
		return err
	}
	return shareWithWorkload(dir)
}

// checkout resolves and validates a preparation's checkout material before
// anything is created or registered.
func (r *Resources) checkout(ctx context.Context, spec execution.ResourceSpec) (*bootstrap.CheckoutMaterial, error) {
	if r.cfg.Checkout == nil {
		return nil, bootstrap.ErrNoCheckout
	}
	m, err := r.cfg.Checkout.Checkout(ctx, spec)
	switch {
	case err == nil:
	case errors.Is(err, bootstrap.ErrCheckoutDenied):
		return nil, fmt.Errorf("%w: the provider refused the request", bootstrap.ErrCheckoutDenied)
	default:
		// A provider's error text is not trusted to keep credentials out of logs.
		return nil, fmt.Errorf("%w: the checkout credential provider failed", execution.ErrUnavailable)
	}
	if m.Scope != spec.Scope || m.RepositoryURL != spec.Preparation.RepositoryURL {
		return nil, fmt.Errorf("%w: the provider answered for another scope or repository", bootstrap.ErrCheckoutDenied)
	}
	if err := validCredential(m.Credential); err != nil {
		return nil, fmt.Errorf("%w: %v", bootstrap.ErrCheckoutDenied, err)
	}
	if err := validCerts(m.GitCA); err != nil {
		return nil, fmt.Errorf("%w: %v", bootstrap.ErrCheckoutDenied, err)
	}
	return &bootstrap.CheckoutMaterial{
		Scope: m.Scope, RepositoryURL: m.RepositoryURL, Credential: append([]byte(nil), m.Credential...), GitCA: append([]byte(nil), m.GitCA...),
	}, nil
}

// validCredential accepts exactly what the entrypoint parses: one line
// "username:password" with both parts non-empty and no control characters.
func validCredential(b []byte) error {
	s := string(b)
	user, pass, ok := strings.Cut(s, ":")
	if !ok || user == "" || pass == "" || len(b) > 4096 || strings.TrimSpace(s) != s {
		return errors.New("the checkout credential must be one username:password line")
	}
	for _, c := range s {
		if c < 0x20 || c == 0x7f {
			return errors.New("the checkout credential has control characters")
		}
	}
	return nil
}

// validCerts requires a bounded PEM bundle of at least one certificate.
func validCerts(b []byte) error {
	if len(b) == 0 || len(b) > 64<<10 || !x509.NewCertPool().AppendCertsFromPEM(b) {
		return errors.New("the Git CA must be a certificate bundle")
	}
	return nil
}

// requireRunNetwork verifies that the run's network is ours, ensured and not
// stopping: a workload never attaches to a foreign or fenced network.
func (r *Resources) requireRunNetwork(ctx context.Context, spec execution.ResourceSpec, name string) error {
	return requireRunNetwork(ctx, r.e, r.st, spec.Scope, spec.RunID, name)
}

// holdWorkspace verifies the run workspace a preparation mounts and takes its
// lock until the returned release: a Stop of the workspace waits for the create
// and a create that follows a confirmed Stop is fenced.
func (r *Resources) holdWorkspace(ctx context.Context, spec execution.ResourceSpec, name string) (func(), error) {
	unlock, err := r.locks.lock(ctx, name)
	if err != nil {
		return nil, err
	}
	rec, found, err := r.st.load(name)
	if err == nil {
		err = verifyWorkspaceRecord(rec, found, spec.Scope, spec.RunID, "")
	}
	if err == nil {
		err = verifyVolume(ctx, r.e, rec)
	}
	if err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}

// ---- observe ----

// Observe implements execution.RunResources. It never creates. An object that
// podman verifiably does not have is absent; anything the adapter cannot
// determine is unknown, never absent.
func (r *Resources) Observe(ctx context.Context, h execution.ResourceHandle) (execution.ResourceObservation, error) {
	if err := h.Validate(); err != nil {
		return execution.ResourceObservation{}, err
	}
	unlock, err := r.locks.lock(ctx, h.Name)
	if err != nil {
		return execution.ResourceObservation{}, err
	}
	defer unlock()
	rec, found, err := r.find(ctx, h)
	switch {
	case errors.Is(err, execution.ErrResourceCollision):
		return execution.ResourceObservation{}, err
	case ctx.Err() != nil:
		return execution.ResourceObservation{}, ctx.Err()
	case err != nil:
		return unknown(h), nil
	case !found:
		return execution.ResourceObservation{Handle: h, Phase: execution.PhaseAbsent, ObservedAt: time.Now().UTC()}, nil
	}
	return r.observe(ctx, rec)
}

const reasonUnobservable execution.ResourceReason = "resource_unobservable"

func unknown(h execution.ResourceHandle) execution.ResourceObservation {
	return execution.ResourceObservation{Handle: h, Phase: execution.PhaseUnknown, Reason: reasonUnobservable, ObservedAt: time.Now().UTC()}
}

func absent(h execution.ResourceHandle) execution.ResourceObservation {
	return execution.ResourceObservation{Handle: h, Phase: execution.PhaseAbsent, ObservedAt: time.Now().UTC()}
}

// observe describes the record's resource. The caller holds its name lock. An
// engine failure is an unknown observation; an object that is not the record's
// is a collision.
func (r *Resources) observe(ctx context.Context, rec resRecord) (execution.ResourceObservation, error) {
	h := rec.handle()
	if rec.Removed {
		return absent(h), nil
	}
	obs := execution.ResourceObservation{Handle: h, Fingerprint: rec.Fingerprint, ObservedAt: time.Now().UTC()}
	// An announced creation not yet recorded is adopted if its object is there; one
	// whose request may still land is unresolved, never absent, however long ago it
	// was issued, unless an operator acknowledged that none can arrive.
	err := r.applyRetirement(&rec)
	if err == nil {
		err = r.reconcile(ctx, &rec)
	}
	switch {
	case errors.Is(err, execution.ErrResourceCollision):
		return execution.ResourceObservation{}, err
	case ctx.Err() != nil:
		return execution.ResourceObservation{}, ctx.Err()
	case err != nil:
		return unknown(h), nil
	case !rec.Created && rec.outstanding():
		u := unknown(h)
		u.Reason = "create_unresolved"
		return u, nil
	}
	switch rec.Kind {
	case execution.KindNetwork:
		err = r.observeNetwork(ctx, rec, &obs)
	case execution.KindWorkspace:
		err = r.observeVolume(ctx, rec, &obs)
	default:
		err = r.observeContainer(ctx, rec, &obs)
	}
	switch {
	case errors.Is(err, execution.ErrResourceCollision):
		return execution.ResourceObservation{}, err
	case ctx.Err() != nil:
		return execution.ResourceObservation{}, ctx.Err()
	case err != nil:
		return unknown(h), nil
	}
	return obs, nil
}

func ready(obs *execution.ResourceObservation) {
	obs.Phase = execution.PhaseReady
	t := obs.ObservedAt
	obs.ReadyAt = &t
}

func (r *Resources) observeNetwork(ctx context.Context, rec resRecord, obs *execution.ResourceObservation) error {
	nw, found, err := r.e.network(ctx, rec.Name)
	switch {
	case err != nil:
		return err
	case !found:
		*obs = absent(obs.Handle)
		return nil
	}
	if err := rec.verified(nw.Labels, nw.ID); err != nil {
		return err
	}
	return simplePhase(rec, obs)
}

// simplePhase is the phase of a network or volume: ready, stopped once the stop
// is verified, pending while one was started but not completed.
func simplePhase(rec resRecord, obs *execution.ResourceObservation) error {
	switch {
	case rec.Stopped:
		obs.Phase = execution.PhaseStopped
	case rec.Stopping:
		obs.Phase, obs.Reason = execution.PhasePending, execution.ReasonStopUnconfirmed
	default:
		ready(obs)
	}
	return nil
}

func (r *Resources) observeVolume(ctx context.Context, rec resRecord, obs *execution.ResourceObservation) error {
	vol, found, err := r.e.volume(ctx, rec.Name)
	switch {
	case err != nil:
		return err
	case !found:
		*obs = absent(obs.Handle)
		return nil
	}
	if err := rec.verified(vol.Labels, vol.CreatedAt); err != nil {
		return err
	}
	released, err := r.released(ctx, rec)
	if err != nil {
		return err
	}
	obs.Released = released
	return simplePhase(rec, obs)
}

func (r *Resources) observeContainer(ctx context.Context, rec resRecord, obs *execution.ResourceObservation) error {
	h := obs.Handle
	ref := rec.cid()
	if ref == "" {
		ref = rec.Name
	}
	ct, found, err := r.e.container(ctx, ref)
	switch {
	case err != nil:
		return err
	case !found && ref == rec.Name && !rec.Created && rec.IssuedAt == nil:
		obs.Phase = execution.PhasePending // recorded, not created yet: Ensure creates it
		return nil
	case !found:
		*obs = absent(h)
		return nil
	}
	if err := rec.verified(ct.Config.Labels, ct.ID); err != nil {
		return err
	}
	obs.LogRef = "resource-log:" + rec.Name
	status := ct.State.Status
	running := ct.State.Running
	switch {
	case rec.Stopping && running:
		// The stop was requested but the container is not verified stopped.
		obs.Phase, obs.Reason = execution.PhasePending, execution.ReasonStopUnconfirmed
	case rec.Stopping:
		obs.Phase = execution.PhaseStopped
	case status == "created" || status == "configured" || status == "initialized" || (status != "running" && running):
		obs.Phase = execution.PhasePending
	case running && rec.Kind == execution.KindPreparation:
		obs.Phase = execution.PhasePending
	case running:
		return r.probe(ctx, &rec, ct, obs)
	case status == "exited" || status == "stopped" || status == "dead":
		code := int64(ct.State.ExitCode)
		if rec.Kind == execution.KindPreparation && code == 0 {
			// The preparation completed; the receipt is the server's evidence and
			// execution stops the finished workload.
			ready(obs)
			return nil
		}
		obs.Phase, obs.ExitCode, obs.Reason = execution.PhaseFailed, &code, failureReason(rec.Kind)
	default:
		obs.Phase = execution.PhasePending
	}
	return nil
}

func failureReason(k execution.ResourceKind) execution.ResourceReason {
	if k == execution.KindPreparation {
		return execution.ReasonPreparationFailed
	}
	return execution.ReasonServiceLost
}

// probe runs the running service's readiness probe once. A failing probe is
// "not ready"; only an inability to probe (podman itself failing, an unresolved
// probe container) is unknown.
func (r *Resources) probe(ctx context.Context, rec *resRecord, ct container, obs *execution.ResourceObservation) error {
	pr := rec.Probe
	if pr == nil {
		return fmt.Errorf("%w: the service has no recorded readiness", execution.ErrUnavailable)
	}
	timeout := time.Duration(pr.TimeoutSecs) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var code int
	switch pr.Kind {
	case "exec":
		res, err := r.e.run(pctx, append([]string{"exec", ct.ID}, pr.Exec...)...)
		switch {
		case err != nil && ctx.Err() != nil:
			return ctx.Err()
		case err != nil && errors.Is(pctx.Err(), context.DeadlineExceeded):
			res = result{code: 1} // the probe timed out: not ready
		case err != nil:
			return err
		}
		if res.code == 125 {
			return fmt.Errorf("%w: podman exec: %s", execution.ErrUnavailable, strings.TrimSpace(res.stderr))
		}
		code = res.code
	case "tcp":
		var err error
		if code, err = r.probeTCP(ctx, pctx, rec, timeout); err != nil {
			return err
		}
	}
	if code != 0 {
		obs.Phase = execution.PhasePending
		return nil
	}
	ready(obs)
	obs.ProbeKind = pr.Kind
	if rec.ReadyAt != nil {
		obs.ReadyAt = rec.ReadyAt
		return nil
	}
	rec.ReadyAt = obs.ReadyAt
	return r.st.save(*rec)
}

// ---- logs ----

// Logs implements execution.RunResources: the bounded tail of a workload's
// container output; workspace and network have none.
func (r *Resources) Logs(ctx context.Context, h execution.ResourceHandle, bound execution.LogBound) (execution.ResourceLogs, error) {
	if err := h.Validate(); err != nil {
		return execution.ResourceLogs{}, err
	}
	if !h.Kind.HasIdentity() {
		return execution.ResourceLogs{}, fmt.Errorf("%w: a %s has no logs", execution.ErrDenied, h.Kind)
	}
	if bound.MaxBytes <= 0 {
		return execution.ResourceLogs{}, fmt.Errorf("%w: the log bound must be positive", execution.ErrInvalid)
	}
	unlock, err := r.locks.lock(ctx, h.Name)
	if err != nil {
		return execution.ResourceLogs{}, err
	}
	rec, found, err := r.find(ctx, h)
	unlock()
	switch {
	case err != nil:
		return execution.ResourceLogs{}, err
	case !found || rec.Removed || rec.cid() == "":
		return execution.ResourceLogs{}, fmt.Errorf("%w: no container for %s", execution.ErrNotFound, h.Key)
	}
	ct, ok, err := r.e.container(ctx, rec.cid())
	switch {
	case err != nil:
		return execution.ResourceLogs{}, err
	case !ok:
		return execution.ResourceLogs{}, fmt.Errorf("%w: the container is gone", execution.ErrNotFound)
	}
	if err := rec.verified(ct.Config.Labels, ct.ID); err != nil {
		return execution.ResourceLogs{}, err
	}
	res, err := r.e.must(ctx, "logs", "--tail", "2000", ct.ID)
	if err != nil {
		return execution.ResourceLogs{}, err
	}
	out := res.stdout + res.stderr
	logs := execution.ResourceLogs{}
	if int64(len(out)) > bound.MaxBytes {
		out, logs.Truncated = out[int64(len(out))-bound.MaxBytes:], true
	}
	logs.Content = out
	return logs, nil
}

// ---- probe containers ----

// probeName is the deterministic name of a service's tcp-probe container: one
// probe at a time, and a name that a late creation cannot take twice.
func probeName(rec resRecord) string { return rec.Name + "-probe" }

func probeLabels(rec resRecord, nonce string) map[string]string {
	return map[string]string{
		LabelManagedBy: ValueManagedByResources, LabelKind: "probe", LabelRun: string(rec.RunID),
		LabelUID: rec.UID, LabelKey: rec.Key, LabelProbeNonce: nonce,
	}
}

// resolveProbe settles the service's probe container. A bound container is
// deleted by the ID recorded for it, never by name or labels. An outstanding
// creation (answer not seen) is resolved only by observation (the name together
// with the recorded nonce: the container is then deleted by the ID found) or by an
// operator's retirement. Another object at the name is a collision and is never
// touched, and no placeholder is created. It reports whether no probe request is
// outstanding any more.
func (r *Resources) resolveProbe(ctx context.Context, rec *resRecord) (bool, error) {
	ps := rec.ProbeSlot
	if ps == nil {
		return true, nil
	}
	if ps.ID != "" {
		if _, found, err := r.e.container(ctx, ps.ID); err != nil {
			return false, err
		} else if found {
			if _, err := r.e.must(ctx, "rm", "--force", "--time", "0", ps.ID); err != nil {
				return false, err
			}
			if exists, err := r.e.exists(ctx, "container", ps.ID); err != nil {
				return false, err
			} else if exists {
				return false, fmt.Errorf("%w: the probe container is still there", execution.ErrUnavailable)
			}
		}
		ps.ID = ""
		if err := r.st.save(*rec); err != nil {
			return false, err
		}
	}
	ct, found, err := r.e.container(ctx, probeName(*rec))
	if err != nil {
		return false, err
	}
	switch {
	case !found:
	case labelsMatch(probeLabels(*rec, ps.Nonce), ct.Config.Labels) && ps.outstanding():
		// A creation request is outstanding and this is what landed: found by name
		// and nonce, deleted by the ID it has now. With no request outstanding the
		// same labels prove nothing: a container whose ID is not the one bound is a
		// replacement, never deleted (see the default case).
		if _, err := r.e.must(ctx, "rm", "--force", "--time", "0", ct.ID); err != nil {
			return false, err
		}
		ps.answered()
		if err := r.st.save(*rec); err != nil {
			return false, err
		}
	default:
		return false, execution.ErrResourceCollision
	}
	return !ps.outstanding(), nil
}

// probeTCP runs one tcp probe: a container created (its intent announced first,
// its ID bound as soon as the create is answered), started and waited for, and
// deleted by that ID. It returns the probe's exit code.
func (r *Resources) probeTCP(ctx, pctx context.Context, rec *resRecord, timeout time.Duration) (int, error) {
	if rec.ProbeSlot == nil {
		rec.ProbeSlot = &probeSlot{Nonce: newUID()}
	}
	ps := rec.ProbeSlot
	if _, err := r.resolveProbe(ctx, rec); err != nil {
		return 0, err
	}
	if ps.outstanding() {
		return 0, errUnresolved // an earlier probe create may still land: no other is issued
	}
	runNet := execution.ResourceName(rec.Scope, rec.RunID, execution.KindNetwork, execution.KeyNetwork)
	secs := max(int64(timeout/time.Second), 1)
	args := append([]string{"create", "--name", probeName(*rec)}, labelArgsOf(probeLabels(*rec, ps.Nonce))...)
	args = append(args,
		"--network", runNet, "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--pull=missing", "--userns=auto",
		"--user", strconv.Itoa(r.cfg.WorkloadUID)+":"+strconv.Itoa(r.cfg.WorkloadGID), "--pids-limit", "32", "--memory", "32m",
		r.cfg.ProbeImage, "nc", "-z", "-w", strconv.FormatInt(secs, 10), rec.Probe.Host, strconv.FormatInt(rec.Probe.Port, 10))
	epoch, err := r.st.epoch(rec.Name)
	if err != nil {
		return 0, err // nothing is announced, nothing is sent
	}
	ps.announce(time.Now().UTC(), epoch)
	if err := r.st.save(*rec); err != nil {
		return 0, err
	}
	res, err := r.e.run(pctx, args...)
	if err == nil && !res.ok() {
		err = fmt.Errorf("%w: podman create: %s", execution.ErrUnavailable, strings.TrimSpace(res.stderr))
	}
	id := strings.TrimSpace(res.stdout)
	if err == nil && !containerID.MatchString(id) {
		err = fmt.Errorf("%w: podman create returned no container ID", execution.ErrUnavailable)
	}
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, err
	}
	ps.ID = id
	ps.answered()
	if err := r.st.save(*rec); err != nil {
		return 0, err
	}
	// Whatever happens next, the container is deleted by its bound ID, with a
	// context of its own (the probe's may be cancelled).
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = r.resolveProbe(cctx, rec)
	}()
	run, err := r.e.run(pctx, "start", "--attach", id)
	switch {
	case err != nil && ctx.Err() != nil:
		return 0, ctx.Err()
	case err != nil && errors.Is(pctx.Err(), context.DeadlineExceeded):
		return 1, nil // the probe timed out: not ready
	case err != nil:
		return 0, err
	case run.code >= 125:
		return 0, fmt.Errorf("%w: podman probe: %s", execution.ErrUnavailable, strings.TrimSpace(run.stderr))
	}
	return run.code, nil
}

// ---- stop and remove ----

// createdEvidence is what the record proves about a launch: the object was
// created, or its creation was announced and never seen (unknown), or it was
// never issued (not started).
func createdEvidence(rec resRecord) execution.LaunchEvidence {
	switch {
	case rec.Created:
		return execution.LaunchStarted
	case rec.issued():
		return execution.LaunchUnknown
	}
	return execution.LaunchNotStarted
}

// Stop implements execution.RunResources. The stop intent is recorded first (a
// pending create can no longer be issued and the workload's credential files are
// destroyed), then an announced creation is reconciled: the object is adopted if
// it landed; if the request is still outstanding the stop is NOT confirmed
// (Stopped false, launch unknown) until the object is observed or an operator
// retired the request. Stopped, the verified stop, is recorded and reported only
// once nothing of the resource can run or arrive; a workspace reports released
// only after its recorded volume instance is verified (a replacement is a
// collision, an uninspectable volume an error) and neither a container nor an
// unresolved holder intent still uses it.
func (r *Resources) Stop(ctx context.Context, h execution.ResourceHandle) (execution.ResourceStopEvidence, error) {
	if err := h.Validate(); err != nil {
		return execution.ResourceStopEvidence{}, err
	}
	unlock, err := r.locks.lock(ctx, h.Name)
	if err != nil {
		return execution.ResourceStopEvidence{}, err
	}
	defer unlock()
	rec, found, err := r.find(ctx, h)
	if err != nil {
		return execution.ResourceStopEvidence{}, err
	}
	if !found {
		// Nothing of ours exists: leave the tombstone that fences a late create. It
		// keeps the identity the caller supplied and proves nothing about launch.
		if err := r.st.create(tombstone(h, false)); err != nil && !errors.Is(err, fs.ErrExist) {
			return execution.ResourceStopEvidence{}, err
		}
		return execution.ResourceStopEvidence{Stopped: true, Launch: execution.LaunchUnknown, Released: true}, nil
	}
	if !rec.Stopping {
		rec.Stopping = true
		if err := r.st.save(rec); err != nil {
			return execution.ResourceStopEvidence{}, err
		}
	}
	unconfirmed := execution.ResourceStopEvidence{Stopped: false, Launch: execution.LaunchUnknown}
	if err := r.applyRetirement(&rec); err != nil {
		return execution.ResourceStopEvidence{}, err
	}
	if err := r.reconcile(ctx, &rec); err != nil {
		return execution.ResourceStopEvidence{}, err
	}
	if !rec.Created && rec.outstanding() {
		return unconfirmed, nil
	}
	ev := execution.ResourceStopEvidence{Stopped: true}
	switch rec.Kind {
	case execution.KindNetwork:
		if rec.Launch == "" {
			rec.Launch = createdEvidence(rec)
		}
	case execution.KindWorkspace:
		released, err := r.released(ctx, rec)
		if err != nil {
			return execution.ResourceStopEvidence{}, err
		}
		ev.Released = released
		if rec.Launch == "" {
			rec.Launch = createdEvidence(rec)
		}
	default:
		// Revoke first: the credential files are destroyed before the container is
		// terminated, and a failure to destroy them is a failure to stop.
		if err := os.RemoveAll(r.st.bootstrap(rec.Name)); err != nil {
			return execution.ResourceStopEvidence{}, fmt.Errorf("%w: removing the credential files: %w", execution.ErrUnavailable, err)
		}
		conclusive, err := r.resolveProbe(ctx, &rec)
		if err != nil {
			if errors.Is(err, execution.ErrResourceCollision) {
				return execution.ResourceStopEvidence{}, err
			}
			return execution.ResourceStopEvidence{Launch: rec.Launch}, err
		}
		if !conclusive {
			_ = r.st.save(rec)
			return unconfirmed, nil
		}
		stopped, err := r.stopContainer(ctx, &rec)
		if err != nil {
			return execution.ResourceStopEvidence{Launch: rec.Launch}, err
		}
		ev.Stopped = stopped
		if !ev.Stopped {
			_ = r.st.save(rec)
			return unconfirmed, nil
		}
	}
	if ev.Stopped {
		rec.Stopped = true
	}
	if err := r.st.save(rec); err != nil {
		return execution.ResourceStopEvidence{}, err
	}
	ev.Launch = rec.Launch
	return ev, nil
}

// released decides whether a workspace is free of holders from verified identity and
// durable evidence: the recorded volume is inspected (a replacement or an
// uninspectable volume is an error, never "released"; a volume podman says is
// absent is no mount at all), every holder intent must be resolved (found and
// finished, or retired by an operator: an unresolved one means not released), and
// no container may still reference the volume.
func (r *Resources) released(ctx context.Context, rec resRecord) (bool, error) {
	if rec.Created || rec.issued() {
		vol, found, err := r.e.volume(ctx, rec.Name)
		if err != nil {
			return false, err
		}
		if found {
			if err := rec.verified(vol.Labels, vol.CreatedAt); err != nil {
				return false, err
			}
		}
	}
	held, err := r.holderHeld(ctx, rec.Name)
	if err != nil || held {
		return false, err
	}
	refs, err := r.e.holders(ctx, rec.Name)
	if err != nil {
		return false, err
	}
	for _, state := range refs {
		if usesVolume(state) {
			return false, nil
		}
	}
	return true, nil
}

// stopContainer terminates the resource's container by verified ID and reports
// whether it is verifiably not running. rec.Launch is set once.
func (r *Resources) stopContainer(ctx context.Context, rec *resRecord) (bool, error) {
	ref := rec.cid()
	if ref == "" {
		ref = rec.Name
	}
	ct, found, err := r.e.container(ctx, ref)
	switch {
	case err != nil:
		return false, err
	case !found:
		if rec.Launch == "" {
			// Gone, or never seen: not_started only if its creation was never
			// announced; otherwise there is no trace of whether it ran.
			rec.Launch = execution.LaunchUnknown
			if !rec.Created && !rec.issued() {
				rec.Launch = execution.LaunchNotStarted
			}
		}
		return true, nil
	}
	if err := rec.verified(ct.Config.Labels, ct.ID); err != nil {
		return false, err
	}
	rec.ObjectID, rec.Created = ct.ID, true
	started := ct.State.Status != "created" && ct.State.Status != "configured"
	switch {
	case !started:
		if _, err := r.e.must(ctx, "rm", "--force", ct.ID); err != nil {
			return false, err
		}
		if rec.Launch == "" {
			rec.Launch = execution.LaunchNotStarted
		}
		return true, nil
	case ct.State.Running:
		if _, err := r.e.must(ctx, "stop", "--time", strconv.Itoa(int(r.cfg.StopTimeout.Seconds())), ct.ID); err != nil {
			return false, err
		}
		after, found, err := r.e.container(ctx, ct.ID)
		if err != nil {
			return false, err
		}
		if found && after.State.Running {
			return false, nil
		}
	}
	rec.Launch = execution.LaunchStarted
	return true, nil
}

// Remove implements execution.RunResources. It requires a prior VERIFIED stop
// (one that was only requested is not). It deletes the object by verified
// identity. While a creation request may still land (outstanding, its object not
// observed, no operator retirement) the removal is refused: Removed is never
// reported over a request that can still arrive, and no placeholder is created to
// make it safe. A volume or network still referenced, a holder that is unresolved
// or a container that is still running is not removed (Removed false keeps
// cleanup pending), never forced.
func (r *Resources) Remove(ctx context.Context, h execution.ResourceHandle) (execution.ResourceRemoveEvidence, error) {
	if err := h.Validate(); err != nil {
		return execution.ResourceRemoveEvidence{}, err
	}
	unlock, err := r.locks.lock(ctx, h.Name)
	if err != nil {
		return execution.ResourceRemoveEvidence{}, err
	}
	defer unlock()
	rec, found, err := r.find(ctx, h)
	if err != nil {
		return execution.ResourceRemoveEvidence{}, err
	}
	if !found {
		if err := r.st.create(tombstone(h, true)); err != nil && !errors.Is(err, fs.ErrExist) {
			return execution.ResourceRemoveEvidence{}, err
		}
		return execution.ResourceRemoveEvidence{Removed: true}, nil
	}
	if rec.Removed {
		return execution.ResourceRemoveEvidence{Removed: true}, nil
	}
	if !rec.Stopped {
		return execution.ResourceRemoveEvidence{}, fmt.Errorf("%w: %s is removed only after a verified stop", execution.ErrResourceFenced, rec.Key)
	}
	pending := execution.ResourceRemoveEvidence{Removed: false}
	if err := r.applyRetirement(&rec); err != nil {
		return pending, err
	}
	if err := r.reconcile(ctx, &rec); err != nil {
		return pending, err
	}
	if rec.Kind.HasIdentity() {
		conclusive, err := r.resolveProbe(ctx, &rec)
		if err != nil || !conclusive {
			return pending, err
		}
	}
	if rec.Kind == execution.KindWorkspace {
		released, err := r.released(ctx, rec)
		if err != nil || !released {
			return pending, err
		}
	}
	if rec.Kind.HasIdentity() && rec.Created {
		// An object a late create made after the stop was confirmed is stopped now.
		if stopped, err := r.stopContainer(ctx, &rec); err != nil || !stopped {
			return pending, err
		}
	}
	// Only an object this adapter announced or created is deleted, verified by
	// labels and engine identity: a tombstone made without a record owns nothing, and
	// whatever sits at its name is not ours.
	if rec.Created || rec.issued() {
		if done, err := r.removeObject(ctx, rec); err != nil || !done {
			return pending, err
		}
	}
	// A creation request that is still outstanding (its object never observed, no
	// operator retirement) keeps the cleanup pending: it may still land.
	if rec.outstanding() {
		return pending, nil
	}
	if err := os.RemoveAll(r.st.bootstrap(rec.Name)); err != nil {
		return pending, fmt.Errorf("%w: removing the credential files: %w", execution.ErrUnavailable, err)
	}
	rec.Removed = true
	if err := r.st.save(rec); err != nil {
		return pending, err
	}
	return execution.ResourceRemoveEvidence{Removed: true}, nil
}

// removeObject deletes the record's engine object after verifying it and
// confirms that nothing of the real object remains at its name. done is false while the engine still retains the object.
func (r *Resources) removeObject(ctx context.Context, rec resRecord) (done bool, err error) {
	kind := engineKind(rec.Kind)
	switch rec.Kind {
	case execution.KindNetwork:
		nw, found, err := r.e.network(ctx, rec.Name)
		if err != nil {
			return false, err
		}
		if found {
			if err := rec.verified(nw.Labels, nw.ID); err != nil {
				return false, err
			}
			if done, err := r.rm(ctx, kind, "rm", rec.Name); err != nil || !done {
				return false, err
			}
		}
	case execution.KindWorkspace:
		vol, found, err := r.e.volume(ctx, rec.Name)
		if err != nil {
			return false, err
		}
		if found {
			if err := rec.verified(vol.Labels, vol.CreatedAt); err != nil {
				return false, err
			}
			held, err := r.e.holders(ctx, rec.Name)
			if err != nil {
				return false, err
			}
			if len(held) > 0 {
				// Even an exited container still references the volume; never forced.
				return false, nil
			}
			if done, err := r.rm(ctx, kind, "rm", rec.Name); err != nil || !done {
				return false, err
			}
		}
	default:
		ref := rec.cid()
		if ref == "" {
			ref = rec.Name
		}
		ct, found, err := r.e.container(ctx, ref)
		if err != nil {
			return false, err
		}
		if found {
			if err := rec.verified(ct.Config.Labels, ct.ID); err != nil {
				return false, err
			}
			if ct.State.Running {
				return false, nil
			}
			if _, err := r.e.must(ctx, "rm", "--force", "--volumes", ct.ID); err != nil {
				return false, err
			}
			if exists, err := r.e.exists(ctx, kind, ct.ID); err != nil || exists {
				return false, err
			}
		}
	}
	// Confirm that the real object is not at the name. An object that is not ours
	// (never touched) may be: it is not the object.
	switch h, _, err := r.lookupName(ctx, rec); {
	case err != nil:
		return false, err
	case h == heldReal:
		return false, nil
	}
	return true, nil
}

// rm removes a network or volume by name without force: podman refuses while it
// is in use, which is a pending cleanup, not an error.
func (r *Resources) rm(ctx context.Context, kind, verb, name string) (bool, error) {
	res, err := r.e.run(ctx, kind, verb, name)
	if err != nil {
		return false, err
	}
	if res.ok() {
		return true, nil
	}
	msg := strings.ToLower(res.stderr)
	if strings.Contains(msg, "in use") || strings.Contains(msg, "being used") || strings.Contains(msg, "has containers") {
		return false, nil
	}
	if strings.Contains(msg, "no such") {
		return true, nil
	}
	return false, fmt.Errorf("%w: podman %s %s: %s", execution.ErrUnavailable, kind, verb, strings.TrimSpace(res.stderr))
}
