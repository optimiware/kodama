package executiontest

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	eventspg "github.com/optimiware/kodama/internal/events/postgres"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	"github.com/optimiware/kodama/internal/runtime/runtimetest"
)

// This file holds the controlled doubles of the run_shared lifecycle: a fake
// companion port with provider-neutral semantics (idempotent, scope/fingerprint/
// UID verified, fenced after stop) that tests inject EXPLICITLY, a mutable
// policy source and the policy/document fixtures. Nothing here is a production
// default or a permissive constructor; the admission opt-in exists only through
// execution.SharedConfig composed by a test.

// SharedImage and the digests below are placeholders (the .invalid domain).
const (
	SharedImage = "registry.example.invalid/tools@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	PrepImage   = "registry.example.invalid/kodama-prepare@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

// FullResourceSet is every resource capability a fully capable fake offers.
var FullResourceSet = execution.Set(
	execution.CapRunWorkspace, execution.CapWorkspaceFence, execution.CapWorkspacePreparation, execution.CapResourceRecovery,
	execution.CapRunServices, execution.CapServiceIsolation, execution.CapRunNetworkIsolation, execution.CapDeclaredEdges,
	execution.CapNamedEgress, execution.CapEgressDefaultDeny)

// TestPolicy is a short, explicit TEST policy. It is open for run_shared; none
// of its values is a qualified profile.
func TestPolicy() execution.ExecutionPolicy {
	shape := execution.RoleShape{User: "nonroot", UserNamespace: "pod", CapabilitiesAdd: []string{}, Seccomp: "RuntimeDefault", Devices: []string{}}
	step := shape
	step.CapabilitiesAdd = []string{"SETUID", "SETGID"}
	prep := shape
	prep.Image = PrepImage
	return execution.ExecutionPolicy{
		ID:        "run-test",
		Admission: execution.PolicyAdmission{LegacyFormat: execution.PolicyOpen, RunShared: execution.PolicyOpen},
		Limits: execution.PolicyLimits{
			MaxRunSeconds: 21600, MaxServices: 8, MaxRunWorkspaceMiB: 8192, DefaultRunWorkspaceMiB: 2048, MaxServiceCPUMillis: 4000,
			MaxServiceMemoryMiB: 8192, MaxServiceEphemeralMiB: 8192, MaxServiceStorageMiB: 8192, PreparationTimeoutSeconds: 600,
			PreparationAttempts: 2, PreparationMaxBytes: 1 << 30, StopGraceSeconds: 30, PreparationCPUMillis: 500, PreparationMemoryMiB: 256,
			StepCPUMillis: 1000, StepMemoryMiB: 1024, StepEphemeralMiB: 512,
		},
		Workspace: execution.PolicyWorkspace{Access: "ReadWriteOncePod", Reclaim: "Delete", Idmapped: true, StorageClass: "test-scratch"},
		Roles: map[string]execution.RoleShape{
			execution.RoleStep: step, execution.RoleService: shape, execution.RoleBuilderService: shape, execution.RolePreparation: prep,
		},
		PodSecurity: execution.PolicyPodSecurity{Level: "baseline", Version: "test"},
		Builders:    execution.PolicyBuilders{Rootless: []string{"ko"}, Service: execution.PolicyBuilderS{Mode: "none"}},
		EgressProfiles: map[string]execution.EgressProfile{
			"registry": {Destinations: []execution.EgressDestination{{Host: "registry.example.invalid", Ports: []int64{443}}}},
		},
		// The strict model keeps the per-run isolation and default-deny refusals
		// observable; TenantPolicy is the O-30 stage model.
		Network: execution.PolicyNetwork{Isolation: execution.IsolationRun, PublicEgress: false},
	}
}

// TenantPolicy is TestPolicy under the O-30 network model: tenant-level
// isolation and public HTTP/HTTPS egress.
func TenantPolicy() execution.ExecutionPolicy {
	p := TestPolicy()
	p.ID = "run-test-tenant"
	p.Network = execution.PolicyNetwork{Isolation: execution.IsolationTenant, PublicEgress: true}
	return p
}

// MutablePolicies is an execution.Policies whose current policy a test replaces.
type MutablePolicies struct {
	mu  sync.Mutex
	pol execution.ExecutionPolicy
	err error
}

// NewPolicies starts with TestPolicy.
func NewPolicies() *MutablePolicies { return &MutablePolicies{pol: TestPolicy()} }

// Set replaces the current policy.
func (p *MutablePolicies) Set(pol execution.ExecutionPolicy) { p.mu.Lock(); p.pol = pol; p.mu.Unlock() }

// Fail makes the policy source unavailable.
func (p *MutablePolicies) Fail(err error) { p.mu.Lock(); p.err = err; p.mu.Unlock() }

// Current implements execution.Policies.
func (p *MutablePolicies) Current(context.Context) (execution.ExecutionPolicy, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pol, p.err
}

// SharedLimit is a test limit with explicit run_shared capacity.
func SharedLimit() execution.Limit {
	l := DefaultLimit()
	l.SharedTenant = execution.CapacityUnits{Services: 20, CPUMillis: 40000, MemoryMiB: 1 << 17, EphemeralMiB: 1 << 17, StorageMiB: 1 << 17}
	l.SharedProject = execution.CapacityUnits{Services: 10, CPUMillis: 20000, MemoryMiB: 1 << 16, EphemeralMiB: 1 << 16, StorageMiB: 1 << 16}
	return l
}

// ---- fake companion port ----

type fakeObject struct {
	foreign bool
	spec    execution.ResourceSpec
	fp      string
	uid     string
	phase   execution.ResourcePhase
	stopped bool
	removed bool
}

// FakeResources is the controlled companion port of one backend. It follows the
// contract: Ensure creates or adopts by scope, name and fingerprint (collision
// otherwise) and refuses a stopped object; Observe never creates and reports
// absent or unknown honestly; Stop is verified and fences later creates;
// Remove needs a prior stop. Every call is recorded.
type FakeResources struct {
	mu      sync.Mutex
	backend string
	set     execution.ResourceCapabilitySet
	objs    map[string]*fakeObject
	fenced  map[string]bool
	late    map[string]execution.ResourceSpec
	seq     int
	calls   []string

	// AutoReady makes a created resource ready at once (preparation completes
	// without a receipt unless the test records one).
	AutoReady bool
	// HoldVolume makes the workspace report not released.
	HoldVolume bool

	collide     map[string]bool // the object at the name stopped being ours (a bound instance replaced)
	hold        map[string]*observeHold
	holdEnsure  map[string]*observeHold
	misUID      map[string]string
	misFP       map[string]string
	failEnsure  map[string]error // returned before creating
	loseEnsure  map[string]error // creates, then returns the error (lost response)
	lateEnsure  map[string]error // returns the error, creating the object only on Deliver
	failObserve map[string]error
	failStop    map[string]int
	holdStop    map[string]int // Stop reports not stopped this many times
	failRemove  map[string]int
	holdRemove  map[string]int // Remove reports not removed this many times
}

// observeHold blocks one Observe call AFTER its answer was computed, so a test
// can deliver that answer late (an answer that left the adapter before a newer
// one was committed).
type observeHold struct {
	entered chan struct{}
	release chan struct{}
}

// NewFakeResources is a fully capable fake of the given backend.
func NewFakeResources(backend string) *FakeResources {
	return &FakeResources{
		backend: backend, set: FullResourceSet, objs: map[string]*fakeObject{}, fenced: map[string]bool{}, late: map[string]execution.ResourceSpec{},
		collide: map[string]bool{}, hold: map[string]*observeHold{}, holdEnsure: map[string]*observeHold{}, misUID: map[string]string{}, misFP: map[string]string{}, failEnsure: map[string]error{}, loseEnsure: map[string]error{}, lateEnsure: map[string]error{}, failObserve: map[string]error{},
		failStop: map[string]int{}, holdStop: map[string]int{}, failRemove: map[string]int{}, holdRemove: map[string]int{},
	}
}

// SetBackend replaces the backend identity the port reports.
func (f *FakeResources) SetBackend(b string) { f.mu.Lock(); f.backend = b; f.mu.Unlock() }

// SetCapabilities replaces the declared set.
func (f *FakeResources) SetCapabilities(set execution.ResourceCapabilitySet) {
	f.mu.Lock()
	f.set = set
	f.mu.Unlock()
}

// Capabilities implements execution.RunResources.
func (f *FakeResources) Capabilities(context.Context) execution.ResourceCapabilities {
	f.mu.Lock()
	defer f.mu.Unlock()
	return execution.ResourceCapabilities{Backend: f.backend, Set: f.set}
}

func (f *FakeResources) record(op, key string) { f.calls = append(f.calls, op+":"+key) }

// Calls returns the recorded calls ("ensure:service.db", ...), oldest first.
func (f *FakeResources) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// Count counts the recorded calls equal to call.
func (f *FakeResources) Count(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == call {
			n++
		}
	}
	return n
}

func (f *FakeResources) obs(o *fakeObject) execution.ResourceObservation {
	h := o.spec.Handle(o.uid)
	phase := o.phase
	if o.stopped {
		phase = execution.PhaseStopped
	}
	r := execution.ResourceObservation{Handle: h, Fingerprint: o.fp, Phase: phase}
	if uid, ok := f.misUID[o.spec.Key]; ok {
		r.Handle.UID = uid
	}
	if fp, ok := f.misFP[o.spec.Key]; ok {
		r.Fingerprint = fp
	}
	if phase == execution.PhaseReady {
		t := time.Now().UTC()
		r.ReadyAt = &t
		if o.spec.Kind == execution.KindService {
			r.ProbeKind = o.spec.Service.Readiness.Probe
		}
	}
	if o.spec.Kind == execution.KindWorkspace {
		r.Released = !f.HoldVolume
	}
	return r
}

// Ensure implements execution.RunResources.
func (f *FakeResources) Ensure(_ context.Context, spec execution.ResourceSpec) (execution.ResourceObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	obs, err := f.ensureLocked(spec)
	if hold := f.holdEnsure[spec.Key]; hold != nil {
		delete(f.holdEnsure, spec.Key)
		f.mu.Unlock()
		close(hold.entered)
		<-hold.release
		f.mu.Lock()
	}
	return obs, err
}

func (f *FakeResources) ensureLocked(spec execution.ResourceSpec) (execution.ResourceObservation, error) {
	f.record("ensure", spec.Key)
	if err := spec.Validate(); err != nil {
		return execution.ResourceObservation{}, err
	}
	if err := f.failEnsure[spec.Key]; err != nil {
		return execution.ResourceObservation{}, err
	}
	fp, _ := spec.Fingerprint()
	if o, ok := f.objs[spec.Name]; ok {
		if o.spec.Scope != spec.Scope || o.fp != fp {
			return execution.ResourceObservation{}, execution.ErrResourceCollision
		}
		if o.stopped || o.removed {
			return execution.ResourceObservation{}, execution.ErrResourceFenced
		}
		return f.obs(o), nil
	}
	if f.fenced[spec.Name] {
		return execution.ResourceObservation{}, execution.ErrResourceFenced
	}
	if err := f.lateEnsure[spec.Key]; err != nil {
		f.late[spec.Name] = spec
		return execution.ResourceObservation{}, err
	}
	f.seq++
	o := &fakeObject{spec: spec, fp: fp, uid: fmt.Sprintf("uid-%s-%d", spec.Key, f.seq), phase: execution.PhasePending}
	if f.AutoReady {
		o.phase = execution.PhaseReady
	}
	f.objs[spec.Name] = o
	if err := f.loseEnsure[spec.Key]; err != nil {
		return execution.ResourceObservation{}, err
	}
	return f.obs(o), nil
}

// Deliver materializes a late create (a lost Ensure that arrives after the
// caller gave up) unless Stop fenced its name. It reports whether it created.
func (f *FakeResources) Deliver(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for name, spec := range f.late {
		if spec.Key != key {
			continue
		}
		delete(f.late, name)
		if f.fenced[name] {
			return false
		}
		fp, _ := spec.Fingerprint()
		f.seq++
		f.objs[name] = &fakeObject{spec: spec, fp: fp, uid: fmt.Sprintf("uid-%s-%d", spec.Key, f.seq), phase: execution.PhasePending}
		return true
	}
	return false
}

func (f *FakeResources) find(h execution.ResourceHandle) (*fakeObject, error) {
	o, ok := f.objs[h.Name]
	if !ok {
		return nil, nil
	}
	if o.foreign && h.UID == "" {
		// A never-owned foreign object at the name of a resource with no bound
		// instance: nothing of ours exists there. It is not touched.
		return nil, nil
	}
	if o.foreign || f.collide[h.Key] || o.spec.Scope != h.Scope || o.spec.RunID != h.RunID || (h.UID != "" && o.uid != h.UID) {
		return nil, execution.ErrResourceCollision
	}
	return o, nil
}

// Observe implements execution.RunResources.
func (f *FakeResources) Observe(_ context.Context, h execution.ResourceHandle) (execution.ResourceObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("observe", h.Key)
	if err := f.failObserve[h.Key]; err != nil {
		return execution.ResourceObservation{}, err
	}
	o, err := f.find(h)
	var out execution.ResourceObservation
	switch {
	case err != nil:
	case o == nil || o.removed:
		out = execution.ResourceObservation{Handle: h, Phase: execution.PhaseAbsent}
	default:
		out = f.obs(o)
	}
	if hold := f.hold[h.Key]; hold != nil {
		delete(f.hold, h.Key)
		f.mu.Unlock()
		close(hold.entered)
		<-hold.release
		f.mu.Lock()
	}
	return out, err
}

// HoldNextObserve makes the next Observe of the key compute its answer and then
// wait: entered closes when it is waiting, release delivers the answer.
func (f *FakeResources) HoldNextObserve(key string) (entered <-chan struct{}, release func()) {
	h := &observeHold{entered: make(chan struct{}), release: make(chan struct{})}
	f.Script(func(f *FakeResources) { f.hold[key] = h })
	return h.entered, func() { close(h.release) }
}

// HoldNextEnsure makes the next Ensure of the key compute its result (an outage
// included) and then wait, to deliver that result late.
func (f *FakeResources) HoldNextEnsure(key string) (entered <-chan struct{}, release func()) {
	h := &observeHold{entered: make(chan struct{}), release: make(chan struct{})}
	f.Script(func(f *FakeResources) { f.holdEnsure[key] = h })
	return h.entered, func() { close(h.release) }
}

// MisreportUID makes observations of the key carry another instance UID ("" clears it).
func (f *FakeResources) MisreportUID(key, uid string) {
	f.Script(func(f *FakeResources) {
		if uid == "" {
			delete(f.misUID, key)
		} else {
			f.misUID[key] = uid
		}
	})
}

// MisreportFingerprint makes observations of the key carry another fingerprint;
// the empty string is the adapter's "could not verify" (set true clears it).
func (f *FakeResources) MisreportFingerprint(key, fp string, clear bool) {
	f.Script(func(f *FakeResources) {
		if clear {
			delete(f.misFP, key)
		} else {
			f.misFP[key] = fp
		}
	})
}

// Collide makes the object at the key's name stop being the resource's: every
// handle that carries a bound instance now meets a collision (a bound instance
// whose fingerprint drifted or that was replaced). Clear releases it.
func (f *FakeResources) Collide(key string, v bool) {
	f.Script(func(f *FakeResources) { f.collide[key] = v })
}

// Logs implements execution.RunResources.
func (f *FakeResources) Logs(_ context.Context, h execution.ResourceHandle, _ execution.LogBound) (execution.ResourceLogs, error) {
	if h.Kind == execution.KindWorkspace || h.Kind == execution.KindNetwork {
		return execution.ResourceLogs{}, execution.ErrDenied
	}
	return execution.ResourceLogs{Content: "[redacted]"}, nil
}

// Stop implements execution.RunResources.
func (f *FakeResources) Stop(_ context.Context, h execution.ResourceHandle) (execution.ResourceStopEvidence, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("stop", h.Key)
	if f.failStop[h.Key] > 0 {
		f.failStop[h.Key]--
		return execution.ResourceStopEvidence{}, fmt.Errorf("stop unavailable")
	}
	o, err := f.find(h)
	if err != nil {
		return execution.ResourceStopEvidence{}, err
	}
	f.fenced[h.Name] = true // no pending create may arrive after a verified stop
	if f.holdStop[h.Key] > 0 {
		f.holdStop[h.Key]--
		return execution.ResourceStopEvidence{Stopped: false, Launch: execution.LaunchUnknown}, nil
	}
	if o == nil {
		return execution.ResourceStopEvidence{Stopped: true, Launch: execution.LaunchUnknown, Released: !f.HoldVolume}, nil
	}
	o.stopped = true
	return execution.ResourceStopEvidence{Stopped: true, Launch: execution.LaunchStarted, Released: !f.HoldVolume}, nil
}

// Remove implements execution.RunResources.
func (f *FakeResources) Remove(_ context.Context, h execution.ResourceHandle) (execution.ResourceRemoveEvidence, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("remove", h.Key)
	if f.failRemove[h.Key] > 0 {
		f.failRemove[h.Key]--
		return execution.ResourceRemoveEvidence{}, fmt.Errorf("remove unavailable")
	}
	o, err := f.find(h)
	if err != nil {
		return execution.ResourceRemoveEvidence{}, err
	}
	if o == nil {
		return execution.ResourceRemoveEvidence{Removed: true}, nil
	}
	if !o.stopped {
		return execution.ResourceRemoveEvidence{}, execution.ErrResourceFenced
	}
	if f.holdRemove[h.Key] > 0 {
		f.holdRemove[h.Key]--
		return execution.ResourceRemoveEvidence{Removed: false}, nil
	}
	o.removed = true
	return execution.ResourceRemoveEvidence{Removed: true}, nil
}

// ---- test controls ----

func (f *FakeResources) byKey(key string) *fakeObject {
	for _, o := range f.objs {
		if o.spec.Key == key {
			return o
		}
	}
	return nil
}

// Exists reports whether a live (not removed) object exists for the key.
func (f *FakeResources) Exists(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.byKey(key)
	return o != nil && !o.removed
}

// UID returns the provider UID of the key's object ("" if none).
func (f *FakeResources) UID(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if o := f.byKey(key); o != nil {
		return o.uid
	}
	return ""
}

// SetPhase scripts the provider-side phase of the key's object.
func (f *FakeResources) SetPhase(key string, phase execution.ResourcePhase) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if o := f.byKey(key); o != nil {
		o.phase = phase
	}
}

// Ready marks every live object of the given keys ready.
func (f *FakeResources) Ready(keys ...string) {
	for _, k := range keys {
		f.SetPhase(k, execution.PhaseReady)
	}
}

// ReadyAll makes every live object ready.
func (f *FakeResources) ReadyAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, o := range f.objs {
		if !o.removed && !o.stopped {
			o.phase = execution.PhaseReady
		}
	}
}

// Vanish deletes the key's object without a stop (the provider lost it).
func (f *FakeResources) Vanish(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if o := f.byKey(key); o != nil {
		o.removed = true
	}
}

// Plant puts a FOREIGN object (another fingerprint) at the key's name.
func (f *FakeResources) Plant(spec execution.ResourceSpec) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	f.objs[spec.Name] = &fakeObject{foreign: true, spec: spec, fp: "sha256:" + strings.Repeat("f", 64), uid: fmt.Sprintf("foreign-%d", f.seq), phase: execution.PhaseReady}
}

// Script sets one failure mode; the maps are keyed by resource key.
func (f *FakeResources) Script(fn func(f *FakeResources)) { f.mu.Lock(); defer f.mu.Unlock(); fn(f) }

// FailEnsure/LoseEnsure/LateEnsure/FailObserve/FailStop/HoldStop/FailRemove/HoldRemove script failures.
func (f *FakeResources) FailEnsure(key string, err error) {
	f.Script(func(f *FakeResources) { f.failEnsure[key] = err })
}
func (f *FakeResources) LoseEnsure(key string, err error) {
	f.Script(func(f *FakeResources) { f.loseEnsure[key] = err })
}
func (f *FakeResources) LateEnsure(key string, err error) {
	f.Script(func(f *FakeResources) { f.lateEnsure[key] = err })
}
func (f *FakeResources) FailObserve(key string, err error) {
	f.Script(func(f *FakeResources) { f.failObserve[key] = err })
}
func (f *FakeResources) FailStop(key string, n int) {
	f.Script(func(f *FakeResources) { f.failStop[key] = n })
}
func (f *FakeResources) HoldStop(key string, n int) {
	f.Script(func(f *FakeResources) { f.holdStop[key] = n })
}
func (f *FakeResources) FailRemove(key string, n int) {
	f.Script(func(f *FakeResources) { f.failRemove[key] = n })
}
func (f *FakeResources) HoldRemove(key string, n int) {
	f.Script(func(f *FakeResources) { f.holdRemove[key] = n })
}

// Hold sets whether the workspace volume still has a user (not released).
func (f *FakeResources) Hold(v bool) { f.mu.Lock(); f.HoldVolume = v; f.mu.Unlock() }

// LiveKeys lists the keys of live (not removed) objects, sorted.
func (f *FakeResources) LiveKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, o := range f.objs {
		if !o.removed {
			out = append(out, o.spec.Key)
		}
	}
	sort.Strings(out)
	return out
}

var _ execution.RunResources = (*FakeResources)(nil)

// ---- documents ----

// SharedSpecDoc returns a kodama/v1alpha2 document with one database service,
// the given steps (YAML flow maps, each a step) and a run deadline.
func SharedSpecDoc(withService bool, steps ...string) string {
	var b strings.Builder
	b.WriteString("apiVersion: kodama/v1alpha2\nkind: Pipeline\nmetadata: {name: shared}\nspec:\n  timeoutSeconds: 3600\n")
	if withService {
		b.WriteString("  services:\n    db:\n      image: " + SharedImage + "\n      ports: {sql: {port: 5432}}\n" +
			"      resources: {cpuMillis: 100, memoryMiB: 64, ephemeralMiB: 64}\n      storage: [{path: /var/lib/data, sizeMiB: 128}]\n" +
			"      readiness: {tcp: sql, timeoutSeconds: 30}\n")
	}
	b.WriteString("  steps:\n")
	for _, s := range steps {
		b.WriteString("    - " + s + "\n")
	}
	return b.String()
}

// SharedStep is one container step of a shared document.
func SharedStep(id string, extra ...string) string {
	parts := []string{"id: " + id, "kind: container", "image: " + SharedImage, "command: [/bin/true]", "timeoutSeconds: 60"}
	parts = append(parts, extra...)
	return "{" + strings.Join(parts, ", ") + "}"
}

// SharedBackend is the backend identity of the shared fixtures.
const SharedBackend = runtimetest.ManualBackend

// SharedScope is the scope the shared suites use.
var SharedScope = ScopeA

var _ = domain.Scope{}

// ---- subjects ----

// SharedSubject is one backend under the run_shared contract: the execution
// Subject plus the doubles and the few fixture controls the suite needs.
type SharedSubject struct {
	Subject
	Resources *FakeResources
	Policies  *MutablePolicies
	Manual    *runtimetest.Manual
	// DefineShared registers a kodama/v1alpha2 definition.
	DefineShared func(yaml string) pipeline.Definition
	// Closed builds a Service over the same store with NO run_shared composition,
	// as an installation that never opted in.
	Closed func() *execution.Service
	// Restart builds another Service over the same store, ports and policy, as a
	// controller after a restart (or a second replica) would be.
	Restart func() *execution.Service
	// RunReservations counts the HELD run-level reservations of the tenant.
	RunReservations func(tenant domain.TenantID) int
	// ForceTerminal models a run left terminal by an interrupted older controller
	// while its resources were never verified removed. Fixture only.
	ForceTerminal func(scope domain.Scope, run domain.RunID)
	// Project is the seeded work item and workspace of the subject.
	Project Project
	// Events lists the tenant's published events of a catalog type.
	Events func(typ string) []eventsv1.Envelope
	// FailEvent makes the append of one event type fail (nil clears it).
	FailEvent func(typ string, err error)
	// CorruptPolicy makes the run's retained policy body stop verifying against its
	// pinned digest, as a lost or damaged retained policy would. Fixture only.
	CorruptPolicy func(scope domain.Scope, run domain.RunID)
	// CorruptModel makes the run's pinned run model one this controller does not
	// implement. Fixture only.
	CorruptModel func(scope domain.Scope, run domain.RunID)
	// HistoryCount counts the run's history rows.
	HistoryCount func(run domain.RunID) int
	// EvidenceCount counts an attempt's evidence rows of a kind.
	EvidenceCount func(attempt domain.AttemptID, kind string) int
	// SourceCalls counts the calls the service made to the Git observation seam.
	SourceCalls func() int
	// Cleanup builds the Service used to admit a cleanup run with the explicit
	// run_shared composition (NewCleanup) or without it (NewClosedCleanup).
	NewCleanup, NewClosedCleanup func() *execution.Service
	// Starts lists every spec the runtime's Start received, in order.
	Starts func() []execution.AttemptSpec
}

// AllowCleanup is the deployment-bound cleanup authorization double: every
// deployment of the test scope is authorized.
type AllowCleanup struct{ Err error }

// AuthorizeCleanup implements execution.DeploymentCleanup.
func (a AllowCleanup) AuthorizeCleanup(context.Context, domain.Scope, string) error { return a.Err }

// CountingSources counts the calls to the Git observation seam.
type CountingSources struct {
	Inner execution.Sources
	mu    sync.Mutex
	n     int
}

func (c *CountingSources) bump() { c.mu.Lock(); c.n++; c.mu.Unlock() }

// Calls returns the number of calls.
func (c *CountingSources) Calls() int { c.mu.Lock(); defer c.mu.Unlock(); return c.n }

// Verify implements execution.Sources.
func (c *CountingSources) Verify(ctx context.Context, s domain.Scope, i domain.WorkItemID, commit string) (execution.SourceInfo, error) {
	c.bump()
	return c.Inner.Verify(ctx, s, i, commit)
}

// Describe implements execution.Sources.
func (c *CountingSources) Describe(ctx context.Context, s domain.Scope, w string, refresh bool) (execution.SourceInfo, error) {
	c.bump()
	return c.Inner.Describe(ctx, s, w, refresh)
}

// Retain implements execution.Sources.
func (c *CountingSources) Retain(ctx context.Context, s domain.Scope, w string, r domain.RunID, commit string) (string, error) {
	c.bump()
	return c.Inner.Retain(ctx, s, w, r, commit)
}

// sharedValidator is the registration validator with the new format enabled.
func sharedValidator(t testing.TB) *pipeline.Validator {
	t.Helper()
	v, err := pipeline.NewValidator(pipeline.Config{
		Capabilities:   pipeline.EnabledKinds{pipeline.KindContainer, pipeline.KindAgent, pipeline.KindGate},
		Profiles:       pipelinetest.Profiles{"coding-v1": true},
		References:     pipelinetest.References{Secrets: map[string]bool{"token": true}, Environments: map[string]bool{"env_staging": true}},
		EnableV1Alpha2: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// SharedCaps are the attempt-runtime capabilities of a backend that also
// implements the companion port.
func SharedCaps() execution.Capabilities {
	return execution.Capabilities{Backend: SharedBackend, Guarantees: execution.AllGuarantees.WithRunResources()}
}

// NewSharedMem builds the memory fixture with the run_shared opt-in composed
// explicitly from test doubles.
func NewSharedMem(t testing.TB, mod ...func(*MemOptions)) (*Mem, SharedSubject) {
	t.Helper()
	res, pol := NewFakeResources(SharedBackend), NewPolicies()
	lim := SharedLimit()
	so := execution.Options{ClaimLease: 2 * time.Second, Batch: 10, ResourceFreshness: 20 * time.Second, ResourceRecheck: time.Second,
		ResourceBackoffBase: time.Second, ResourceBackoffMax: 8 * time.Second, TerminationDelay: -1}
	counter := &CountingSources{}
	wrap := func(in execution.Sources) execution.Sources { counter.Inner = in; return counter }
	mo := MemOptions{Shared: &execution.SharedConfig{Resources: res, Policies: pol}, Limits: &lim, ServiceOptions: &so,
		Cleanup: AllowCleanup{}, SourceWrap: wrap}
	for _, f := range mod {
		f(&mo)
	}
	m := NewMem(t, mo)
	m.GrantAll()
	m.Manual.SetCapabilities(SharedCaps())
	m.Validator = sharedValidator(t)
	p := m.NewProject(ScopeA)
	s := SharedSubject{Subject: m.Subject(), Resources: res, Policies: pol, Manual: m.Manual, Project: p}
	s.DefineShared = func(yaml string) pipeline.Definition { return m.Define(ScopeA, yaml) }
	m.Manual.SetStop(execution.StopEvidence{Stopped: true, Launch: execution.LaunchStarted})
	s.Closed = func() *execution.Service {
		return m.NewService(MemOptions{ServiceOptions: &so, Cleanup: AllowCleanup{}})
	}
	s.Restart = func() *execution.Service {
		return m.NewService(MemOptions{Shared: &execution.SharedConfig{Resources: res, Policies: pol}, ServiceOptions: &so, Cleanup: AllowCleanup{}, SourceWrap: wrap})
	}
	s.NewCleanup, s.NewClosedCleanup = s.Restart, s.Closed
	s.SourceCalls = counter.Calls
	s.Starts = m.Workloads.Starts
	s.EvidenceCount = func(attempt domain.AttemptID, kind string) int {
		rows, err := m.Mem.EvidenceOf(context.Background(), ScopeA, attempt)
		must(t, err)
		n := 0
		for _, row := range rows {
			if string(row.Kind) == kind {
				n++
			}
		}
		return n
	}
	s.CorruptModel = func(scope domain.Scope, run domain.RunID) {
		must(t, m.Mem.CorruptModelForTest(context.Background(), scope, run))
	}
	s.HistoryCount = func(run domain.RunID) int {
		h, err := m.Mem.History(context.Background(), ScopeA, run)
		must(t, err)
		return len(h)
	}
	s.FailEvent = m.World.FailEventType
	s.Events = func(typ string) []eventsv1.Envelope {
		evs, err := m.World.Events(context.Background(), ScopeA.TenantID)
		must(t, err)
		var out []eventsv1.Envelope
		for _, ev := range evs {
			if ev.Type == typ {
				out = append(out, ev)
			}
		}
		return out
	}
	s.CorruptPolicy = func(scope domain.Scope, run domain.RunID) {
		must(t, m.Mem.CorruptPolicyForTest(context.Background(), scope, run))
	}
	s.RunReservations = func(tenant domain.TenantID) int {
		rs, err := m.Mem.RunReservations(context.Background(), tenant)
		must(t, err)
		n := 0
		for _, r := range rs {
			if r.Held() {
				n++
			}
		}
		return n
	}
	s.ForceTerminal = func(scope domain.Scope, run domain.RunID) {
		must(t, m.Mem.ForceTerminalForTest(context.Background(), scope, run))
	}
	return m, s
}

// NewSharedPG builds the PostgreSQL fixture with the same explicit run_shared
// opt-in. It skips in the unit layer exactly like New.
func NewSharedPG(t testing.TB, extra ...Options) (*Env, SharedSubject) {
	t.Helper()
	res, pol := NewFakeResources(SharedBackend), NewPolicies()
	lim := SharedLimit()
	so := execution.Options{ClaimLease: 2 * time.Second, Batch: 10, ResourceFreshness: 20 * time.Second, ResourceRecheck: time.Second,
		ResourceBackoffBase: time.Second, ResourceBackoffMax: 8 * time.Second, TerminationDelay: -1}
	opt := Options{}
	if len(extra) > 0 {
		opt = extra[0]
	}
	opt.Shared, opt.ServiceOptions = &execution.SharedConfig{Resources: res, Policies: pol}, &so
	opt.Cleanup = AllowCleanup{}
	counter := &CountingSources{}
	wrap := func(in execution.Sources) execution.Sources { counter.Inner = in; return counter }
	opt.SourceWrap = wrap
	var failing *FailingAppender
	prior := opt.WrapAppender
	opt.WrapAppender = func(in execpgAppender) execpgAppender {
		if prior != nil {
			in = prior(in)
		}
		failing = &FailingAppender{Inner: in}
		return failing
	}
	if opt.Limits == nil {
		opt.Limits = &lim
	}
	e := New(t, opt)
	e.GrantAll()
	e.Manual.SetCapabilities(SharedCaps())
	e.Validator = sharedValidator(t)
	p := e.NewProject(ScopeA)
	s := SharedSubject{Subject: e.Subject(), Resources: res, Policies: pol, Manual: e.Manual, Project: p}
	s.DefineShared = func(yaml string) pipeline.Definition { return e.Define(ScopeA, yaml) }
	e.Manual.SetStop(execution.StopEvidence{Stopped: true, Launch: execution.LaunchStarted})
	s.Closed = func() *execution.Service { return e.NewService(Options{ServiceOptions: &so, Cleanup: AllowCleanup{}}) }
	s.Restart = func() *execution.Service {
		return e.NewService(Options{Shared: &execution.SharedConfig{Resources: res, Policies: pol}, ServiceOptions: &so, Cleanup: AllowCleanup{}, SourceWrap: wrap})
	}
	s.NewCleanup, s.NewClosedCleanup = s.Restart, s.Closed
	s.SourceCalls = counter.Calls
	s.Starts = e.Workloads.Starts
	s.EvidenceCount = func(attempt domain.AttemptID, kind string) int {
		return e.CountTenant(ScopeA.TenantID, `SELECT count(*) FROM attempt_evidence WHERE attempt_id = $1 AND kind = $2`, string(attempt), kind)
	}
	s.CorruptModel = func(scope domain.Scope, run domain.RunID) {
		e.OwnerExec(`ALTER TABLE runs DISABLE TRIGGER runs_shared_guard`)
		e.OwnerExec(`UPDATE runs SET run_shared = jsonb_set(run_shared, '{runModel}', '"run_shared_v9"'::jsonb) WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
			string(scope.TenantID), string(scope.ProjectID), string(run))
		e.OwnerExec(`ALTER TABLE runs ENABLE TRIGGER runs_shared_guard`)
	}
	s.HistoryCount = func(run domain.RunID) int {
		return e.CountTenant(ScopeA.TenantID, `SELECT count(*) FROM run_history WHERE run_id = $1`, string(run))
	}
	s.FailEvent = failing.Fail
	s.Events = func(typ string) []eventsv1.Envelope {
		var out []eventsv1.Envelope
		err := e.RuntimeTx(ScopeA.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT payload FROM outbox WHERE tenant_id = $1 AND event_type = $2 ORDER BY created_at, event_id`, string(ScopeA.TenantID), typ)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var raw []byte
				if err := rows.Scan(&raw); err != nil {
					return err
				}
				var env eventsv1.Envelope
				if err := json.Unmarshal(raw, &env); err != nil {
					return err
				}
				out = append(out, env)
			}
			return rows.Err()
		})
		must(t, err)
		return out
	}
	s.CorruptPolicy = func(scope domain.Scope, run domain.RunID) {
		// The guard trigger forbids any rewrite of the pins, so a lost or damaged
		// retained body can only be modeled by the owner disabling it for one statement.
		e.OwnerExec(`ALTER TABLE runs DISABLE TRIGGER runs_shared_guard`)
		e.OwnerExec(`UPDATE runs SET run_shared = jsonb_set(run_shared, '{policyBody,limits,stopGraceSeconds}', '99'::jsonb) WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
			string(scope.TenantID), string(scope.ProjectID), string(run))
		e.OwnerExec(`ALTER TABLE runs ENABLE TRIGGER runs_shared_guard`)
	}
	s.RunReservations = func(tenant domain.TenantID) int {
		return e.CountTenant(tenant, `SELECT count(*) FROM execution_run_reservations WHERE released_at IS NULL`)
	}
	s.ForceTerminal = func(scope domain.Scope, run domain.RunID) {
		e.OwnerExec(`UPDATE runs SET status = 'failed', status_reason = 'run_failed', failure_intent_at = now(), failure_reason = 'run_failed',
			finished_at = now(), version = version + 1 WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
			string(scope.TenantID), string(scope.ProjectID), string(run))
	}
	return e, s
}

// FailingAppender fails the append of one event type, to prove an outbox refusal
// rolls the whole transition back.
type FailingAppender struct {
	Inner execpgAppender
	mu    sync.Mutex
	typ   string
	err   error
}

// Fail makes the append of typ fail with err (nil clears it).
func (f *FailingAppender) Fail(typ string, err error) {
	f.mu.Lock()
	f.typ, f.err = typ, err
	f.mu.Unlock()
}

// Append implements the event appender.
func (f *FailingAppender) Append(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) (eventspg.AppendResult, error) {
	f.mu.Lock()
	typ, err := f.typ, f.err
	f.mu.Unlock()
	if err != nil && env.Type == typ {
		return eventspg.AppendResult{}, err
	}
	return f.Inner.Append(ctx, tx, env)
}
