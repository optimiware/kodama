package execution

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/pipeline"
)

// This file holds the pure-domain evidence of the run_shared seam: capability
// identity, names and fingerprints, UID binding, the preparation-receipt digest
// object, the policy, requirement derivation, pins and the resource entity's
// state machine. Persistence and lifecycle evidence is the shared contract
// suite (executiontest.SharedContract, memory and PostgreSQL).

func unitPolicy() ExecutionPolicy {
	shape := RoleShape{User: "nonroot", UserNamespace: "pod", CapabilitiesAdd: []string{}, Seccomp: "RuntimeDefault", Devices: []string{}}
	prep := shape
	prep.Image = "registry.example.invalid/kodama-prepare@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	return ExecutionPolicy{
		ID: "run-unit", Admission: PolicyAdmission{LegacyFormat: PolicyOpen, RunShared: PolicyOpen},
		Limits: PolicyLimits{MaxRunSeconds: 21600, MaxServices: 8, MaxRunWorkspaceMiB: 8192, DefaultRunWorkspaceMiB: 2048, MaxServiceCPUMillis: 4000,
			MaxServiceMemoryMiB: 8192, MaxServiceEphemeralMiB: 8192, MaxServiceStorageMiB: 8192, PreparationTimeoutSeconds: 600, PreparationAttempts: 2,
			PreparationMaxBytes: 1 << 30, StopGraceSeconds: 30, PreparationCPUMillis: 500, PreparationMemoryMiB: 256, StepCPUMillis: 1000, StepMemoryMiB: 1024,
			StepEphemeralMiB: 512},
		Workspace: PolicyWorkspace{Access: "ReadWriteOncePod", Reclaim: "Delete", Idmapped: true, StorageClass: "unit"},
		Roles:     map[string]RoleShape{RoleStep: shape, RoleService: shape, RoleBuilderService: shape, RolePreparation: prep},
		Builders:  PolicyBuilders{Service: PolicyBuilderS{Mode: "none"}},
		EgressProfiles: map[string]EgressProfile{
			"registry": {Destinations: []EgressDestination{{Host: "registry.example.invalid", Ports: []int64{443}}}},
		},
	}
}

var unitScope = domain.Scope{TenantID: "t_acme", ProjectID: "p_inv"}

// Test_unit_run_resources_marker_uses_one_spare_bit: PX-21. The attempt
// runtime's six-method port and Guarantees keep their six used bits; the
// companion marker takes ONE spare bit of the uint8, the last bit stays
// reserved, AllGuarantees is unchanged and a persisted binding listing the
// marker is refused by a decoder that does not know it.
func Test_unit_run_resources_marker_uses_one_spare_bit(t *testing.T) {
	if AllGuarantees != 0x3f || RunResourcesMarker != 1<<6 || reservedGuarantee != 1<<7 {
		t.Fatalf("bits: all=%#x marker=%#x reserved=%#x", uint8(AllGuarantees), uint8(RunResourcesMarker), uint8(reservedGuarantee))
	}
	if AllGuarantees&RunResourcesMarker != 0 || (AllGuarantees|RunResourcesMarker)&reservedGuarantee != 0 {
		t.Fatal("the marker overlaps the six used bits or the reserved bit")
	}
	g := AllGuarantees.WithRunResources()
	if !strings.Contains(g.String(), "run_resources") || len(g.Names()) != 7 {
		t.Fatalf("names %v", g.Names())
	}
	raw, err := json.Marshal(RuntimeBinding{Backend: "b", Requires: OCIImage | RunResourcesMarker, ResourceRequires: NormalResourceRequirements})
	if err != nil {
		t.Fatal(err)
	}
	var back RuntimeBinding
	if err := json.Unmarshal(raw, &back); err != nil || back.Requires != OCIImage|RunResourcesMarker || back.ResourceRequires != NormalResourceRequirements {
		t.Fatalf("round trip %s: %+v %v", raw, back, err)
	}
	// A legacy binding keeps its exact stored form (no resourceRequires key).
	legacy, _ := json.Marshal(RuntimeBinding{Backend: "b", Requires: OCIImage | Isolation})
	if strings.Contains(string(legacy), "resourceRequires") || strings.Contains(string(legacy), "run_resources") {
		t.Fatalf("a legacy binding changed: %s", legacy)
	}
	// The older decoder's view: an unknown name is an error (simulated by a name outside the table).
	var gs Guarantees
	if err := json.Unmarshal([]byte(`["oci_image","run_resources_v9"]`), &gs); err == nil {
		t.Fatal("an unknown guarantee name must be refused")
	}
	// Optional launch-history proof stays a separate flag, never a guarantee.
	if strings.Contains(strings.Join(g.Names(), ","), "never_started") {
		t.Fatal("ProvesNeverStarted must not be a guarantee")
	}
}

// Test_unit_resource_capability_set_is_a_separate_named_set: the finer
// capabilities have room for at least 16 members, decode strictly and never
// share bits with Guarantees.
func Test_unit_resource_capability_set_is_a_separate_named_set(t *testing.T) {
	if len(capabilityNames) != 13 {
		t.Fatalf("members %d", len(capabilityNames))
	}
	var s ResourceCapabilitySet = ResourceCapabilitySet(CapBuilderIsolatedWorker)
	if s.Names()[0] != "builder_isolated_worker" || uint32(s)>>16 != 0 {
		t.Fatalf("%v", s.Names())
	}
	raw, _ := json.Marshal(NormalResourceRequirements)
	var back ResourceCapabilitySet
	if err := json.Unmarshal(raw, &back); err != nil || back != NormalResourceRequirements {
		t.Fatalf("%s %v", raw, err)
	}
	if err := json.Unmarshal([]byte(`["run_workspace","teleport"]`), &back); err == nil {
		t.Fatal("an unknown capability must be refused")
	}
	missing := Set(CapRunWorkspace).Missing(NormalResourceRequirements)
	if missing.Has(CapRunWorkspace) || !missing.Has(CapRunNetworkIsolation) || !missing.Has(CapEgressDefaultDeny) {
		t.Fatalf("missing %v", missing)
	}
	for _, want := range []string{"run_network_isolation", "egress_default_deny", "workspace_fence", "resource_recovery", "run_workspace"} {
		if !strings.Contains(NormalResourceRequirements.String(), want) {
			t.Errorf("the normal requirements lack %s", want)
		}
	}
}

// Test_unit_resource_names_endpoints_and_fingerprint_are_frozen: golden vectors
// derived independently (Python hashlib) freeze the name, endpoint host and
// framing; the fingerprint excludes the UID and covers the spec.
func Test_unit_resource_names_endpoints_and_fingerprint_are_frozen(t *testing.T) {
	scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_inv"}
	if got := ResourceName(scope, "run_01", KindService, "service.db"); got != "kr-bffbd351027c3945eef52eea" {
		t.Fatalf("service name %s", got)
	}
	if got := ResourceName(scope, "run_01", KindNetwork, KeyNetwork); got != "kr-3bd86bde24e06941668e1755" {
		t.Fatalf("network name %s", got)
	}
	if got := EndpointHost(scope, "run_01", "db"); got != "s-88e8f793896d39a8-db" || len(EndpointHost(scope, "run_01", strings.Repeat("a", 30))) > 49 {
		t.Fatalf("host %s", got)
	}
	// Framing: moving a character across the field boundary changes the name.
	a := ResourceName(domain.Scope{TenantID: "ab", ProjectID: "c"}, "r", KindNetwork, KeyNetwork)
	b := ResourceName(domain.Scope{TenantID: "a", ProjectID: "bc"}, "r", KindNetwork, KeyNetwork)
	if a == b {
		t.Fatal("ambiguous framing")
	}
	spec := ResourceSpec{
		Scope: scope, RunID: "run_01", Backend: "b", Kind: KindWorkspace, Key: KeyWorkspace, Name: ResourceName(scope, "run_01", KindWorkspace, KeyWorkspace),
		Policy:    PolicyPin{ID: "run-unit", Digest: "sha256:" + strings.Repeat("a", 64)},
		RoleShape: "sha256:" + strings.Repeat("b", 64), DeadlineAt: "2026-10-26T11:00:00Z", Workspace: &WorkspaceSpec{SizeMiB: 2048, Initial: "git"},
	}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	fp, err := spec.Fingerprint()
	if err != nil || len(fp) != 71 {
		t.Fatal(err)
	}
	again, _ := spec.Fingerprint()
	if fp != again {
		t.Fatal("the fingerprint is not stable")
	}
	if spec.Handle("uid-1") == spec.Handle("") || spec.Handle("uid-1").Validate() != nil {
		t.Fatal("handle")
	}
	changed := spec
	changed.Workspace = &WorkspaceSpec{SizeMiB: 2049, Initial: "git"}
	if other, _ := changed.Fingerprint(); other == fp {
		t.Fatal("the fingerprint ignores the spec body")
	}
	canonical, _ := spec.Canonical()
	for _, forbidden := range []string{"uid", "password", "token", "secretValue"} {
		if strings.Contains(strings.ToLower(string(canonical)), forbidden) {
			t.Fatalf("the canonical spec mentions %q: %s", forbidden, canonical)
		}
	}
	// A spec naming two bodies, a wrong name or a mutable image is invalid.
	bad := spec
	bad.Preparation = &PreparationSpec{}
	if bad.Validate() == nil {
		t.Fatal("two bodies")
	}
	bad = spec
	bad.Name = "kr-000000000000000000000000"
	if bad.Validate() == nil {
		t.Fatal("a name that is not the run's deterministic name")
	}
	svc := ResourceSpec{Scope: scope, RunID: "run_01", Backend: "b", Kind: KindService, Key: ServiceKey("db"), Name: ResourceName(scope, "run_01", KindService, "service.db"),
		Policy: spec.Policy, RoleShape: spec.RoleShape, DeadlineAt: spec.DeadlineAt, Service: &ServiceSpec{Name: "db", Image: "registry.example.invalid/db:latest"}}
	if svc.Validate() == nil {
		t.Fatal("a mutable service image must be invalid")
	}
}

// Test_unit_uid_binding_never_adopts_a_foreign_instance: a different UID or
// fingerprint is a collision.
func Test_unit_uid_binding_never_adopts_a_foreign_instance(t *testing.T) {
	h := ResourceHandle{UID: "uid-1"}
	fp := "sha256:" + strings.Repeat("1", 64)
	if err := CheckUIDBinding("uid-1", fp, ResourceObservation{Handle: h, Fingerprint: fp}); err != nil {
		t.Fatal(err)
	}
	if err := CheckUIDBinding("", fp, ResourceObservation{Handle: h, Fingerprint: fp}); err != nil {
		t.Fatalf("an unbound resource adopts the first observed instance: %v", err)
	}
	for name, o := range map[string]ResourceObservation{
		"another UID":         {Handle: ResourceHandle{UID: "uid-2"}, Fingerprint: fp},
		"another fingerprint": {Handle: h, Fingerprint: "sha256:" + strings.Repeat("2", 64)},
	} {
		if err := CheckUIDBinding("uid-1", fp, o); !errors.Is(err, ErrResourceCollision) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Test_unit_preparation_receipt_digest_object_is_frozen: the digest covers the
// request plus the run, the resource key and the bound workspace UID. The golden
// vector was derived independently (Python json/hashlib).
func Test_unit_preparation_receipt_digest_object_is_frozen(t *testing.T) {
	req := runtimev2.PreparationReceiptRequest{Commit: runtimev2.GitCommit(strings.Repeat("1", 40)), TreeID: runtimev2.GitCommit(strings.Repeat("3", 40)), HooksDisabled: true,
		Submodules: runtimev2.SubmodulesSkipped, CredentialsRemoved: true, Attempts: 1}
	o := PreparationReceiptObject{Request: req, RunID: "run_01", ResourceKey: KeyPreparation, WorkspaceUID: "uid-ws-1"}
	d, err := o.Digest()
	const golden = "sha256:5cee48feb65e3fd754049790057ac563f60c313362c0468498078d36f75158ff"
	if err != nil || d != golden {
		t.Fatalf("digest %s %v", d, err)
	}
	other := o
	other.WorkspaceUID = "uid-ws-2"
	if d2, _ := other.Digest(); d2 == d {
		t.Fatal("the digest does not bind the workspace UID")
	}
	other = o
	other.RunID = "run_02"
	if d2, _ := other.Digest(); d2 == d {
		t.Fatal("the digest does not bind the run")
	}
	for name, bad := range map[string]PreparationReceiptObject{
		"no workspace UID": {Request: req, RunID: "run_01", ResourceKey: KeyPreparation},
		"another resource": {Request: req, RunID: "run_01", ResourceKey: "service.db", WorkspaceUID: "u"},
	} {
		if _, err := bad.Digest(); err == nil {
			t.Errorf("%s: a receipt digest was computed", name)
		}
	}
}

// Test_unit_execution_policy_digest_roles_and_ceilings: identity is {id, digest}
// of the canonical body; the roles and fence invariants are validated.
func Test_unit_execution_policy_digest_roles_and_ceilings(t *testing.T) {
	p := unitPolicy()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	d1, err := p.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if d2, _ := unitPolicy().Digest(); d1 != d2 {
		t.Fatal("the policy digest is not stable")
	}
	q := unitPolicy()
	q.Limits.StopGraceSeconds++
	if d3, _ := q.Digest(); d3 == d1 {
		t.Fatal("the digest ignores the body")
	}
	for name, mut := range map[string]func(*ExecutionPolicy){
		"capability other than the mapping helpers": func(p *ExecutionPolicy) {
			s := p.Roles[RoleStep]
			s.CapabilitiesAdd = []string{"SYS_ADMIN"}
			p.Roles[RoleStep] = s
		},
		"a device other than /dev/fuse": func(p *ExecutionPolicy) {
			s := p.Roles[RoleService]
			s.Devices = []string{"/dev/mem"}
			p.Roles[RoleService] = s
		},
		"ReadWriteOnce as the fence": func(p *ExecutionPolicy) { p.Workspace.Access = "ReadWriteOnce" },
		"retain reclaim":             func(p *ExecutionPolicy) { p.Workspace.Reclaim = "Retain" },
		"a missing role":             func(p *ExecutionPolicy) { delete(p.Roles, RoleBuilderService) },
		"a tag-only preparation image": func(p *ExecutionPolicy) {
			s := p.Roles[RolePreparation]
			s.Image = "registry.example.invalid/prep:latest"
			p.Roles[RolePreparation] = s
		},
		"more than eight services": func(p *ExecutionPolicy) { p.Limits.MaxServices = 9 },
		"a zero ceiling":           func(p *ExecutionPolicy) { p.Limits.MaxRunSeconds = 0 },
	} {
		bad := unitPolicy()
		mut(&bad)
		if bad.Validate() == nil {
			t.Errorf("%s: the policy was accepted", name)
		}
	}
	if sd, ok := p.EgressDigest("registry"); !ok || sd == "" {
		t.Fatal("egress digest")
	}
	if _, ok := p.EgressDigest("unknown"); ok {
		t.Fatal("an undefined profile has a digest")
	}
	if rd, err := p.RoleDigest(RoleStep); err != nil || rd == "" {
		t.Fatal(err)
	}
}

// Test_unit_required_resource_capabilities_derive_from_the_document: a
// service-free, edge-free, egress-free run still needs the network members.
func Test_unit_required_resource_capabilities_derive_from_the_document(t *testing.T) {
	pol := unitPolicy()
	doc := func(name string) pipelineDoc { return mustDoc(t, v2Documents[name]) }
	plain := RequiredResourceCapabilities(doc("ordinary work run").doc, true, pol)
	for _, c := range []ResourceCapability{CapRunWorkspace, CapWorkspaceFence, CapResourceRecovery, CapRunNetworkIsolation, CapEgressDefaultDeny, CapWorkspacePreparation} {
		if !plain.Has(c) {
			t.Errorf("a service-free run needs %v", ResourceCapabilitySet(c))
		}
	}
	for _, c := range []ResourceCapability{CapRunServices, CapServiceIsolation, CapDeclaredEdges, CapNamedEgress, CapBuilderService} {
		if plain.Has(c) {
			t.Errorf("a service-free run does not need %v", ResourceCapabilitySet(c))
		}
	}
	free := RequiredResourceCapabilities(doc("ordinary work run").doc, false, pol)
	if free.Has(CapWorkspacePreparation) || !free.Has(CapRunNetworkIsolation) {
		t.Fatalf("source-free: %v", free)
	}
	withSvc := RequiredResourceCapabilities(doc("ordinary with services").doc, true, pol)
	if !withSvc.Has(CapRunServices) || !withSvc.Has(CapServiceIsolation) || !withSvc.Has(CapNamedEgress) {
		t.Fatalf("services and egress: %v", withSvc)
	}
	pol.Requires = Set(CapBuilderRootless)
	if !RequiredResourceCapabilities(doc("ordinary work run").doc, true, pol).Has(CapBuilderRootless) {
		t.Fatal("the policy's requires are added")
	}
	g := RunGuarantees(doc("ordinary work run").doc, DefaultRequirements)
	if g&RunResourcesMarker == 0 || g&OCIImage == 0 {
		t.Fatalf("run guarantees %v", g)
	}
}

// Test_unit_shared_pins_resolve_endpoints_access_order_and_capacity.
func Test_unit_shared_pins_resolve_endpoints_access_order_and_capacity(t *testing.T) {
	pol := unitPolicy()
	pin, _ := pol.Pin()
	d := mustDoc(t, v2Documents["ordinary with services"])
	sh, err := planSharedPins(sharedInput{Scope: unitScope, RunID: "run_01", Doc: d.doc, Policy: pol, PolicyPin: pin, Backend: "b", Requires: OCIImage,
		ResourceReq: NormalResourceRequirements, SourceBacked: true, RepositoryURL: "https://git.example.invalid/a/b.git"})
	if err != nil {
		t.Fatal(err)
	}
	if err := sh.Verify(); err != nil {
		t.Fatal(err)
	}
	if sh.Endpoints["db"].Host != EndpointHost(unitScope, "run_01", "db") || sh.Endpoints["db"].Ports["sql"] != 5432 || sh.WorkspaceMiB != 256 ||
		sh.Access["test"] != AccessWrite || sh.Requires&RunResourcesMarker == 0 || len(sh.Egress["test"]) != 1 {
		t.Fatalf("pins %+v", sh)
	}
	// services 1, db 100m/64MiB+64 shm, preparation 500m/256, step 1000m/1024; scratch 256 + storage.
	if sh.Capacity.Services != 1 || sh.Capacity.CPUMillis != 100+500+1000 || sh.Capacity.MemoryMiB != 64+64+256+1024 || sh.Capacity.StorageMiB != 256 {
		t.Fatalf("capacity %+v", sh.Capacity)
	}
	run := &Run{Scope: unitScope, ID: "run_01", SourceCommit: strings.Repeat("1", 40), Shared: sh}
	specs, err := BuildResourceSpecs(run, time.Date(2026, 10, 26, 11, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, s := range specs {
		keys = append(keys, s.Key)
		if s.Backend != "b" || s.Policy != pin || s.RoleShape == "" {
			t.Fatalf("spec %+v", s)
		}
	}
	if strings.Join(keys, ",") != "network,workspace,preparation,service.db" {
		t.Fatalf("order %v", keys)
	}
	// A swapped retained policy body no longer verifies: policy_unavailable.
	tampered := *sh
	tampered.PolicyBody.Limits.StopGraceSeconds++
	if err := tampered.Verify(); !errors.Is(err, ErrNotConfigured) || !strings.Contains(err.Error(), "policy_unavailable") {
		t.Fatalf("a swapped policy body was accepted: %v", err)
	}
	// Ceilings are refused, never clamped.
	tight := unitPolicy()
	tight.Limits.MaxServiceCPUMillis = 50
	tp, _ := tight.Pin()
	if _, err := planSharedPins(sharedInput{Scope: unitScope, RunID: "run_01", Doc: d.doc, Policy: tight, PolicyPin: tp, Backend: "b", SourceBacked: true}); !errors.Is(err, ErrAdmissionRefused) {
		t.Fatalf("a ceiling violation: %v", err)
	}
	unknown := unitPolicy()
	delete(unknown.EgressProfiles, "registry")
	up, _ := unknown.Pin()
	if _, err := planSharedPins(sharedInput{Scope: unitScope, RunID: "run_01", Doc: d.doc, Policy: unknown, PolicyPin: up, Backend: "b", SourceBacked: true}); !errors.Is(err, ErrAdmissionRefused) {
		t.Fatalf("an undefined egress profile: %v", err)
	}
	// Source-free defaults: an explicit read is refused, none is allowed.
	if _, err := planSharedPins(sharedInput{Scope: unitScope, RunID: "run_01", Doc: d.doc, Policy: pol, PolicyPin: pin, Backend: "b", SourceBacked: false}); !errors.Is(err, ErrAdmissionRefused) {
		t.Fatalf("access write on a source-free run: %v", err)
	}
	clean := mustDoc(t, v2Documents["source-free cleanup, absent workspace"])
	free, err := planSharedPins(sharedInput{Scope: unitScope, RunID: "run_01", Doc: clean.doc, Policy: pol, PolicyPin: pin, Backend: "b", Requires: OCIImage, SourceBacked: false})
	if err != nil || free.Access["teardown"] != AccessNone || strings.Join(free.Order, ",") != "teardown,verify" {
		t.Fatalf("source-free pins: %+v %v", free, err)
	}
}

// Test_unit_run_resource_state_machine: UID bound once, a stopped resource is
// never ready again, removal needs a verified stop, unknown is never loss,
// loss and startup rules, restart-safe backoff.
func Test_unit_run_resource_state_machine(t *testing.T) {
	now := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	pol := unitPolicy()
	pin, _ := pol.Pin()
	d := mustDoc(t, v2Documents["ordinary with services"])
	sh, err := planSharedPins(sharedInput{Scope: unitScope, RunID: "run_01", Doc: d.doc, Policy: pol, PolicyPin: pin, Backend: "b", SourceBacked: true, RepositoryURL: "https://git.example.invalid/a/b.git"})
	if err != nil {
		t.Fatal(err)
	}
	run := &Run{Scope: unitScope, ID: "run_01", SourceCommit: strings.Repeat("1", 40), Shared: sh}
	specs, err := BuildResourceSpecs(run, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	res := map[string]*RunResource{}
	for i, s := range specs {
		r, err := NewRunResource(s, i, now)
		if err != nil {
			t.Fatal(err)
		}
		res[s.Key] = r
	}
	obs := func(r *RunResource, phase ResourcePhase, uid string) ResourceObservation {
		return ResourceObservation{Handle: r.Spec.Handle(uid), Fingerprint: r.Fingerprint, Phase: phase}
	}
	db := res["service.db"]
	if db.Version != 1 || !db.Dirty() || db.Phase != PhasePending || db.UID != "" {
		t.Fatalf("intent %+v", db)
	}
	if err := db.observe(obs(db, PhasePending, "u1"), now); err != nil || db.UID != "u1" {
		t.Fatal(err)
	}
	if err := db.observe(obs(db, PhaseReady, "u2"), now); !errors.Is(err, ErrResourceCollision) {
		t.Fatalf("another UID after binding: %v", err)
	}
	if err := db.observe(ResourceObservation{Handle: db.Handle(), Fingerprint: "sha256:" + strings.Repeat("9", 64), Phase: PhaseReady}, now); !errors.Is(err, ErrResourceCollision) {
		t.Fatalf("another fingerprint: %v", err)
	}
	// Unknown is never loss; absent after binding is.
	if err := db.observe(obs(db, PhaseUnknown, "u1"), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, lost := db.Lost(now.Add(time.Hour)); lost || db.UnknownSince == nil {
		t.Fatal("unknown counted as loss")
	}
	_ = db.observe(obs(db, PhaseReady, "u1"), now.Add(2*time.Second))
	if !db.IsReady() || db.UnknownSince != nil || db.ReadyAt == nil {
		t.Fatal("ready not recorded")
	}
	first := *db.ReadyAt
	_ = db.observe(obs(db, PhasePending, "u1"), now.Add(10*time.Second))
	if _, lost := db.Lost(now.Add(10 * time.Second)); lost {
		t.Fatal("a readiness blip is not loss")
	}
	if reason, lost := db.Lost(now.Add(10*time.Second + 31*time.Second)); !lost || reason != ReasonServiceLost {
		t.Fatalf("readiness failing for lossSeconds is loss: %v %v", reason, lost)
	}
	_ = db.observe(obs(db, PhaseReady, "u1"), now.Add(3*time.Minute))
	if db.ReadyAt == nil || !db.ReadyAt.Equal(first) || db.NotReadySince != nil {
		t.Fatal("the first ready time moved")
	}
	_ = db.observe(obs(db, PhaseAbsent, "u1"), now.Add(4*time.Minute))
	if reason, lost := db.Lost(now.Add(4 * time.Minute)); !lost || reason != ReasonServiceLost {
		t.Fatalf("absent after ready: %v %v", reason, lost)
	}
	// Stop, then a stopped resource is never ready again; remove needs the stop.
	if _, err := db.recordRemove(ResourceRemoveEvidence{Removed: true}, now); err == nil {
		t.Fatal("removal before a verified stop")
	}
	db.requestStop(ReasonServiceLost, now)
	db.recordStop(ResourceStopEvidence{Stopped: false}, now)
	if db.Stopped() || db.Reason != ReasonStopUnconfirmed {
		t.Fatal("an unconfirmed stop is not a stop")
	}
	if !db.recordStop(ResourceStopEvidence{Stopped: true, Launch: LaunchStarted}, now) || !db.Stopped() || db.Phase != PhaseStopped {
		t.Fatal("verified stop")
	}
	_ = db.observe(obs(db, PhaseReady, "u1"), now)
	if db.Phase == PhaseReady || db.IsReady() {
		t.Fatal("a stopped resource became ready again")
	}
	if err := db.MarkEnsureRequested(now); !errors.Is(err, ErrResourceFenced) {
		t.Fatalf("ensure after stop: %v", err)
	}
	if ch, err := db.recordRemove(ResourceRemoveEvidence{Removed: false}, now); err != nil || !ch || db.Reason != ReasonCleanupPending || db.Removed() {
		t.Fatalf("unconfirmed removal must stay visible: %v %v %q", ch, err, db.Reason)
	}
	if _, err := db.recordRemove(ResourceRemoveEvidence{Removed: true}, now); err != nil || !db.Removed() || db.Reason != "" {
		t.Fatal("verified removal")
	}
	// The workspace is stopped only with the volume released.
	ws := res["workspace"]
	ws.requestStop("cancelled", now)
	ws.recordStop(ResourceStopEvidence{Stopped: true, Released: false}, now)
	if ws.Stopped() {
		t.Fatal("a workspace stop without a verified release")
	}
	if !ws.recordStop(ResourceStopEvidence{Stopped: true, Released: true}, now) || !ws.Released {
		t.Fatal("workspace stop")
	}
	// Startup budgets.
	net, prep := res["network"], res["preparation"]
	for _, r := range []*RunResource{prep} {
		t2 := now
		r.EnsureRequestedAt = &t2
	}
	if _, expired := prep.StartupExpired(now.Add(599 * time.Second)); expired {
		t.Fatal("preparation expired early")
	}
	if reason, expired := prep.StartupExpired(now.Add(601 * time.Second)); !expired || reason != ReasonPreparationFailed {
		t.Fatal("preparation without a receipt past its timeout")
	}
	_ = net
	// Restart-safe backoff: exponential, capped.
	b := res["network"]
	for i, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second} {
		b.noteFailure(ReasonCleanupPending, time.Second, 8*time.Second, now)
		if got := b.NextActionAt.Sub(now); got != want || b.Failures != i+1 {
			t.Fatalf("failure %d: backoff %v, want %v", i+1, got, want)
		}
	}
	if b.Due(now) || !b.Due(now.Add(9*time.Second)) {
		t.Fatal("due")
	}
	b.clearFailure(now)
	if b.Failures != 0 || b.NextActionAt != nil {
		t.Fatal("clear")
	}
}

func Test_unit_capacity_units_fit_every_quantity(t *testing.T) {
	limit := CapacityUnits{Services: 2, CPUMillis: 1000, MemoryMiB: 1000, EphemeralMiB: 1000, StorageMiB: 1000}
	held := CapacityUnits{Services: 1, CPUMillis: 500, MemoryMiB: 500, EphemeralMiB: 500, StorageMiB: 500}
	if !(CapacityUnits{Services: 1, CPUMillis: 500, MemoryMiB: 500, EphemeralMiB: 500, StorageMiB: 500}).Fits(held, limit) {
		t.Fatal("an exactly fitting reservation")
	}
	for name, u := range map[string]CapacityUnits{
		"services": {Services: 2}, "cpu": {CPUMillis: 501}, "memory": {MemoryMiB: 501}, "ephemeral": {EphemeralMiB: 501}, "storage": {StorageMiB: 501},
	} {
		if u.Fits(held, limit) {
			t.Errorf("%s over the limit fits", name)
		}
	}
	if !(CapacityUnits{}).IsZero() || (CapacityUnits{StorageMiB: 1}).IsZero() {
		t.Fatal("IsZero")
	}
}

type pipelineDoc struct{ doc pipeline.Document }

func mustDoc(t *testing.T, yaml string) pipelineDoc {
	t.Helper()
	d, err := v2Definition(t, yaml).Content.Document()
	if err != nil {
		t.Fatal(err)
	}
	return pipelineDoc{doc: d}
}

// Test_unit_execution_order_gate_occupies_its_position: the pinned order serializes
// dispatch; a gate holds every later node (dependent or not) until it resolves, a
// step in backoff keeps its slot, and a predecessor that is terminal but not yet
// settled still blocks the next dispatch. PX-17.
func Test_unit_execution_order_gate_occupies_its_position(t *testing.T) {
	now := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	steps := []*StepRun{
		NewStepRun("run_01", 0, pipeline.Step{ID: "test", Kind: pipeline.KindContainer, Image: v2Image, Command: []string{"/bin/true"}}, now),
		NewStepRun("run_01", 1, pipeline.Step{ID: "review", Kind: pipeline.KindGate, Needs: []string{"test"}, Gate: &pipeline.GateSpec{}}, now),
		NewStepRun("run_01", 2, pipeline.Step{ID: "build", Kind: pipeline.KindContainer, Needs: []string{"review"}, Image: v2Image, Command: []string{"/bin/true"}}, now),
		NewStepRun("run_01", 3, pipeline.Step{ID: "lint", Kind: pipeline.KindContainer, Image: v2Image, Command: []string{"/bin/true"}}, now),
	}
	run := &Run{Scope: unitScope, ID: "run_01", Status: RunRunning, SourceRetention: RetentionConfirmed,
		Shared: &RunShared{Order: []string{"test", "review", "build", "lint"}}}
	agg := &Aggregate{Run: run, Steps: steps, Controls: map[domain.AttemptID]*Control{}}
	hooks := EnabledKinds{pipeline.KindContainer}
	if got := agg.NextSharedRunnable(now, hooks); got == nil || got.ID != "test" {
		t.Fatalf("the first node of the order: %v", got)
	}
	steps[0].Status = StepSucceeded
	if got := agg.NextSharedRunnable(now, hooks); got != nil {
		t.Fatalf("the gate occupies its position: lint must wait although it has no dependency, got %s", got.ID)
	}
	steps[1].Status = StepSucceeded
	if got := agg.NextSharedRunnable(now, hooks); got == nil || got.ID != "build" {
		t.Fatalf("after the gate: %v", got)
	}
	// Backoff keeps the slot: lint does not overtake a waiting build.
	later := now.Add(time.Hour)
	steps[2].NextAttemptAt = &later
	if got := agg.NextSharedRunnable(now, hooks); got != nil {
		t.Fatalf("a step in backoff keeps its slot: %s", got.ID)
	}
	steps[2].NextAttemptAt = nil
	steps[2].Status = StepSucceeded
	// A terminal predecessor with an unsettled attempt still blocks the next dispatch.
	at := &Attempt{Scope: unitScope, ID: "att_1", RunID: "run_01", StepID: "build", Status: AttemptSucceeded}
	agg.Attempts = []*Attempt{at}
	agg.Controls["att_1"] = NewControl("att_1")
	if got := agg.NextSharedRunnable(now, hooks); got != nil {
		t.Fatalf("an unsettled predecessor blocks the next node: %s", got.ID)
	}
	t0 := now
	agg.Controls["att_1"].SettledAt = &t0
	if got := agg.NextSharedRunnable(now, hooks); got == nil || got.ID != "lint" {
		t.Fatalf("lint after a settled predecessor: %v", got)
	}
	// The run deadline covers the gate wait: an approval after it no longer applies.
	steps[2].Status, steps[3].Status = StepPending, StepPending
	steps[1].Status, steps[1].GateRef = StepPending, "gate_1"
	deadline := now.Add(time.Minute)
	run.DeadlineAt = &deadline
	if err := agg.CompleteGate("review", now.Add(2*time.Minute)); !errors.Is(err, ErrStale) {
		t.Fatalf("a gate approval past the run deadline must be refused: %v", err)
	}
	if err := agg.CompleteGate("review", now.Add(30*time.Second)); err != nil {
		t.Fatalf("an approval within the deadline: %v", err)
	}
	if !agg.CheckRunDeadline(now.Add(2 * time.Minute)) {
		t.Fatal("the deadline must record the failure intent")
	}
	if run.FailureReason != Reason(ReasonRunDeadlineExceeded) {
		t.Fatalf("reason %s", run.FailureReason)
	}
	if agg.CheckRunDeadline(now.Add(3 * time.Minute)) {
		t.Fatal("the deadline intent is recorded once")
	}
}

// Test_unit_resource_audit_id_satisfies_the_audit_identifier_domain: the audit
// and event resource ID is the T-119 eventsv1.RunResourceID (found by the real
// SQL suite: the audit identifier domain admits neither "/" nor "."). The
// underscore flattening of the first candidate aliased distinct resources
// (run_x/service.network and run_x_service/network); the framed, hashed ID does
// not, and the execution helper is the single derivation.
func Test_unit_resource_audit_id_satisfies_the_audit_identifier_domain(t *testing.T) {
	re := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
	scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	for _, key := range []string{"network", "workspace", "preparation", "service.db", "service.a-b"} {
		id, err := ResourceAuditID(scope, "run_01", key)
		if err != nil || !re.MatchString(id) || len(id) != 43 || id[:3] != "rr-" {
			t.Errorf("%s -> %q %v", key, id, err)
		}
		if want, _ := eventsv1.RunResourceID(scope.TenantID, scope.ProjectID, "run_01", key); id != want {
			t.Errorf("%s: not the contract's single implementation", key)
		}
	}
	if id, _ := ResourceAuditID(scope, "run_01", "network"); id != "rr-b6db60ce0677ae6f334067ebe0214c4795279151" {
		t.Fatalf("golden vector %s", id)
	}
	// The flattened aliases of the first candidate are distinct identities now.
	a, _ := ResourceAuditID(scope, "run_x", "service.network")
	b, _ := ResourceAuditID(scope, "run_x_service", "network")
	c, _ := ResourceAuditID(domain.Scope{TenantID: "t_a", ProjectID: "p"}, "run_01", "network")
	d, _ := ResourceAuditID(domain.Scope{TenantID: "t", ProjectID: "a_p"}, "run_01", "network")
	if a == b || c == d || a == "" || c == "" {
		t.Fatalf("aliasing: %s %s / %s %s", a, b, c, d)
	}
	if _, err := ResourceAuditID(scope, "run_01", "service"); err == nil {
		t.Fatal("a non-canonical key has an ID")
	}
	if _, err := ResourceAuditID(scope, "run/01", "network"); err == nil {
		t.Fatal("an invalid run ID has an ID")
	}
	if ResourceAuditID(scope, "run_01", "service.db"); eventsv1.CheckRunResourceKey("service", "service.db") != nil {
		t.Fatal("the contract's key check rejects a service key")
	}
}

// unitAggregate is a started run_shared aggregate (all resources, no attempt) for
// pure-domain tests.
func unitAggregate(t *testing.T, now time.Time) (*Aggregate, map[string]*RunResource) {
	t.Helper()
	pol := unitPolicy()
	pin, _ := pol.Pin()
	d := mustDoc(t, v2Documents["ordinary with services"])
	sh, err := planSharedPins(sharedInput{Scope: unitScope, RunID: "run_01", Doc: d.doc, Policy: pol, PolicyPin: pin, Backend: "b", SourceBacked: true, RepositoryURL: "https://git.example.invalid/a/b.git"})
	if err != nil {
		t.Fatal(err)
	}
	run := &Run{Scope: unitScope, ID: "run_01", SourceCommit: strings.Repeat("1", 40), Shared: sh, Status: RunRunning, SourceRetention: RetentionConfirmed}
	dl := now.Add(time.Hour)
	run.DeadlineAt = &dl
	specs, err := BuildResourceSpecs(run, dl)
	if err != nil {
		t.Fatal(err)
	}
	agg := &Aggregate{Run: run, Controls: map[domain.AttemptID]*Control{}}
	res := map[string]*RunResource{}
	for i, s := range specs {
		r, err := NewRunResource(s, i, now)
		if err != nil {
			t.Fatal(err)
		}
		agg.Resources = append(agg.Resources, r)
		res[s.Key] = r
	}
	return agg, res
}

func unitObs(r *RunResource, phase ResourcePhase, uid string, at time.Time) ResourceObservation {
	return ResourceObservation{Handle: r.Spec.Handle(uid), Fingerprint: r.Fingerprint, Phase: phase, ObservedAt: at}
}

// Test_unit_observation_generations_fence_stale_answers: an answer is applied
// only if its server-side generation is newer than every applied one, whatever
// the arrival order, and its credible age is the adapter's own observation time
// (or the start of the request), never the arrival time.
func Test_unit_observation_generations_fence_stale_answers(t *testing.T) {
	t0 := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	agg, res := unitAggregate(t, t0)
	svc := res["service.db"]
	g1, req1 := svc.RequestObservation(t0)
	g2, req2 := svc.RequestObservation(t0.Add(time.Second))
	if g1 != 1 || g2 != 2 || !req2.After(req1) {
		t.Fatalf("generations %d %d", g1, g2)
	}
	ready := ObservationAnswer{Obs: unitObs(svc, PhaseReady, "u1", time.Time{})}
	unknown := ObservationAnswer{Obs: unitObs(svc, PhaseUnknown, "u1", time.Time{})}
	late := t0.Add(time.Minute)
	if ok, err := agg.ApplyObservation("service.db", g2, req2, unknown, late); err != nil || !ok {
		t.Fatalf("newer unknown: %v %v", ok, err)
	}
	if ok, err := agg.ApplyObservation("service.db", g1, req1, ready, late.Add(time.Second)); err != nil || ok {
		t.Fatalf("the older ready must be discarded: %v %v", ok, err)
	}
	if svc.Phase != PhaseUnknown || svc.UnknownSince == nil || svc.IsReady() || svc.AppliedGen != g2 {
		t.Fatalf("the older answer changed state: %+v", svc)
	}
	// The same generation is not applied twice either.
	if ok, _ := agg.ApplyObservation("service.db", g2, req2, ready, late); ok {
		t.Fatal("a generation was applied twice")
	}
	// Credible age: with no adapter time the observation is as old as the request
	// started, not as new as its arrival; an adapter time in the future is clamped.
	g3, req3 := svc.RequestObservation(t0.Add(2 * time.Minute))
	arrival := t0.Add(10 * time.Minute)
	if _, err := agg.ApplyObservation("service.db", g3, req3, ready, arrival); err != nil {
		t.Fatal(err)
	}
	if svc.ObservedAt == nil || !svc.ObservedAt.Equal(req3) {
		t.Fatalf("the observation is stamped with its arrival, not its request: %v", svc.ObservedAt)
	}
	g4, req4 := svc.RequestObservation(t0.Add(11 * time.Minute))
	old := t0.Add(5 * time.Minute)
	if _, err := agg.ApplyObservation("service.db", g4, req4, ObservationAnswer{Obs: unitObs(svc, PhaseReady, "u1", old)}, arrival.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if !svc.ObservedAt.Equal(old) {
		t.Fatalf("the adapter's own observation time must be kept: %v", svc.ObservedAt)
	}
	g5, req5 := svc.RequestObservation(t0.Add(20 * time.Minute))
	future := t0.Add(24 * time.Hour)
	_, _ = agg.ApplyObservation("service.db", g5, req5, ObservationAnswer{Obs: unitObs(svc, PhaseReady, "u1", future)}, t0.Add(21*time.Minute))
	if svc.ObservedAt.After(t0.Add(21 * time.Minute)) {
		t.Fatalf("an observation from the future: %v", svc.ObservedAt)
	}
	// A ready claim without a bound identity is not evidence.
	net := res["network"]
	gn, rn := net.RequestObservation(t0)
	_, _ = agg.ApplyObservation("network", gn, rn, ObservationAnswer{Obs: ResourceObservation{Handle: net.Spec.Handle(""), Fingerprint: net.Fingerprint, Phase: PhaseReady}}, t0)
	if net.IsReady() || net.Phase != PhaseUnknown {
		t.Fatalf("ready without a bound identity: %+v", net)
	}
}

// Test_unit_release_holder_needs_current_fresh_evidence: the holder is released
// only on the CURRENT, fresh, UID-bound release evidence of the workspace that
// postdates the attempt's verified stop.
func Test_unit_release_holder_needs_current_fresh_evidence(t *testing.T) {
	t0 := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	agg, res := unitAggregate(t, t0)
	ws := res["workspace"]
	at := &Attempt{Scope: unitScope, ID: "att_1", RunID: "run_01", StepID: "test", Status: AttemptSucceeded}
	agg.Attempts = []*Attempt{at}
	c := NewControl("att_1")
	stopped, settled := t0.Add(10*time.Second), t0.Add(11*time.Second)
	c.TerminationConfirmedAt, c.SettledAt = &stopped, &settled
	agg.Controls["att_1"] = c
	agg.Holders = []*RunHolder{{Scope: unitScope, RunID: "run_01", AttemptID: "att_1", StepID: "test", HeldAt: t0}}
	observe := func(phase ResourcePhase, uid string, released bool, at time.Time) {
		t.Helper()
		g, req := ws.RequestObservation(at)
		o := unitObs(ws, phase, uid, at)
		o.Released = released
		if _, err := agg.ApplyObservation("workspace", g, req, ObservationAnswer{Obs: o}, at); err != nil {
			t.Fatal(err)
		}
	}
	now := t0.Add(30 * time.Second)
	fresh := 20 * time.Second
	if _, err := agg.ReleaseHolder("att_1", fresh, now); err == nil {
		t.Fatal("released with no evidence at all")
	}
	observe(PhaseReady, "u-ws", true, t0.Add(5*time.Second)) // released, but before the verified stop
	if _, err := agg.ReleaseHolder("att_1", fresh, now); err == nil {
		t.Fatal("released on evidence older than the verified stop")
	}
	observe(PhaseReady, "u-ws", false, t0.Add(20*time.Second)) // in use
	if _, err := agg.ReleaseHolder("att_1", fresh, now); err == nil {
		t.Fatal("released while the volume is in use")
	}
	observe(PhaseUnknown, "u-ws", true, t0.Add(25*time.Second)) // unknown never releases, whatever it carries
	if _, err := agg.ReleaseHolder("att_1", fresh, now); err == nil {
		t.Fatal("released on an unknown observation")
	}
	observe(PhaseReady, "u-ws", true, t0.Add(26*time.Second))
	if _, err := agg.ReleaseHolder("att_1", fresh, t0.Add(26*time.Second+fresh+time.Second)); err == nil {
		t.Fatal("released on stale evidence")
	}
	if ok, err := agg.ReleaseHolder("att_1", fresh, now); err != nil || !ok || agg.ActiveHolder() != nil {
		t.Fatalf("current, fresh, bound, post-stop evidence releases: %v %v", ok, err)
	}
	if ok, err := agg.ReleaseHolder("att_1", fresh, now); err != nil || ok {
		t.Fatalf("idempotent: %v %v", ok, err)
	}
}

// Test_unit_unsolicited_stop_is_loss_except_a_preparation_with_its_receipt.
func Test_unit_unsolicited_stop_is_loss_except_a_preparation_with_its_receipt(t *testing.T) {
	t0 := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	for key, want := range map[string]ResourceReason{"service.db": ReasonServiceNotReady, "workspace": ReasonWorkspaceLost, "network": ReasonWorkspaceLost, "preparation": ReasonPreparationFailed} {
		agg, res := unitAggregate(t, t0)
		r := res[key]
		g, req := r.RequestObservation(t0)
		if _, err := agg.ApplyObservation(key, g, req, ObservationAnswer{Obs: unitObs(r, PhaseStopped, "u1", t0)}, t0); err != nil {
			t.Fatal(err)
		}
		if agg.Run.FailureReason != Reason(want) {
			t.Errorf("%s: failure %q, want %q", key, agg.Run.FailureReason, want)
		}
	}
	{
		// A service that was ready and then stopped on its own is lost.
		agg, res := unitAggregate(t, t0)
		r := res["service.db"]
		g, req := r.RequestObservation(t0)
		_, _ = agg.ApplyObservation("service.db", g, req, ObservationAnswer{Obs: unitObs(r, PhaseReady, "u1", t0)}, t0)
		g, req = r.RequestObservation(t0.Add(time.Second))
		_, _ = agg.ApplyObservation("service.db", g, req, ObservationAnswer{Obs: unitObs(r, PhaseStopped, "u1", t0.Add(time.Second))}, t0.Add(time.Second))
		if agg.Run.FailureReason != Reason(ReasonServiceLost) {
			t.Errorf("a ready service that stopped: %q", agg.Run.FailureReason)
		}
	}
	// A preparation that already recorded its receipt stopping is its intended end.
	agg, res := unitAggregate(t, t0)
	prep := res["preparation"]
	prep.Receipt = &runtimev2.PreparationReceiptRequest{Commit: runtimev2.GitCommit(strings.Repeat("1", 40)), TreeID: runtimev2.GitCommit(strings.Repeat("3", 40)), HooksDisabled: true, Submodules: "skipped", CredentialsRemoved: true, Attempts: 1}
	g, req := prep.RequestObservation(t0)
	_, _ = agg.ApplyObservation("preparation", g, req, ObservationAnswer{Obs: unitObs(prep, PhaseStopped, "u1", t0)}, t0)
	if _, lost := prep.Lost(t0); lost || agg.Run.FailureIntentAt != nil {
		t.Fatal("a preparation that stopped after its receipt is not loss")
	}
	// Our own stop is never loss.
	svc := res["service.db"]
	svc.requestStop(ResourceReason(ReasonCancelled), t0)
	g, req = svc.RequestObservation(t0)
	_, _ = agg.ApplyObservation("service.db", g, req, ObservationAnswer{Obs: unitObs(svc, PhaseStopped, "u1", t0)}, t0)
	if _, lost := svc.Lost(t0); lost {
		t.Fatal("a requested stop is not loss")
	}
}

// Test_unit_command_authority_is_live: the command is authorized only for a
// running attempt with live authority, a run that is not draining and before the
// pinned deadline.
func Test_unit_command_authority_is_live(t *testing.T) {
	t0 := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	agg, _ := unitAggregate(t, t0)
	at := &Attempt{Scope: unitScope, ID: "att_1", RunID: "run_01", StepID: "test", Status: AttemptRunning}
	agg.Attempts = []*Attempt{at}
	agg.Controls["att_1"] = NewControl("att_1")
	if !agg.CommandAuthorized(at, t0) {
		t.Fatal("a live attempt is authorized")
	}
	if agg.CommandAuthorized(at, t0.Add(2*time.Hour)) {
		t.Fatal("after the deadline")
	}
	agg.Run.CancelRequestedAt = &t0
	if agg.CommandAuthorized(at, t0) {
		t.Fatal("a draining run")
	}
	agg.Run.CancelRequestedAt = nil
	agg.Controls["att_1"].RevokedAt = &t0
	if agg.CommandAuthorized(at, t0) {
		t.Fatal("a revoked attempt")
	}
}

// Test_unit_aggregate_verifies_identity_before_binding: applied through the
// aggregate (not the comparison helper), an unbound observation of a foreign
// instance with another fingerprint is a collision that binds nothing, an empty
// fingerprint is unknown and binds nothing, and only an affirmative matching
// fingerprint binds and becomes ready evidence. Older collision and fenced
// answers are discarded by the generation fence like any other.
func Test_unit_aggregate_verifies_identity_before_binding(t *testing.T) {
	t0 := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	foreign := func(r *RunResource, phase ResourcePhase, fp string) ObservationAnswer {
		return ObservationAnswer{Obs: ResourceObservation{Handle: r.Spec.Handle("foreign-uid"), Fingerprint: fp, Phase: phase}}
	}
	t.Run("a foreign UID with a mismatching fingerprint", func(t *testing.T) {
		agg, res := unitAggregate(t, t0)
		net := res["network"]
		g, req := net.RequestObservation(t0)
		if ok, err := agg.ApplyObservation("network", g, req, foreign(net, PhaseReady, "sha256:"+strings.Repeat("7", 64)), t0); err != nil || !ok {
			t.Fatalf("%v %v", ok, err)
		}
		if net.UID != "" || net.BoundAt != nil || net.IsReady() || net.Reason != ReasonResourceCollision || agg.Run.FailureReason != Reason(ReasonResourceCollision) {
			t.Fatalf("%+v", net)
		}
	})
	t.Run("an empty fingerprint is unknown and binds nothing", func(t *testing.T) {
		for _, phase := range []ResourcePhase{PhaseReady, PhasePending, PhaseFailed, PhaseStopped} {
			agg, res := unitAggregate(t, t0)
			ws := res["workspace"]
			g, req := ws.RequestObservation(t0)
			o := foreign(ws, phase, "")
			o.Obs.Released = true
			if _, err := agg.ApplyObservation("workspace", g, req, o, t0); err != nil {
				t.Fatal(err)
			}
			if ws.UID != "" || ws.Phase != PhaseUnknown || ws.IsReady() || ws.Released || ws.UnknownSince == nil || agg.Run.FailureIntentAt != nil {
				t.Fatalf("%s: %+v", phase, ws)
			}
		}
		// Absent and unknown claim no object and need none.
		agg, res := unitAggregate(t, t0)
		net := res["network"]
		g, req := net.RequestObservation(t0)
		_, _ = agg.ApplyObservation("network", g, req, ObservationAnswer{Obs: ResourceObservation{Handle: net.Spec.Handle(""), Phase: PhaseAbsent}}, t0)
		if net.Phase != PhaseAbsent || net.UID != "" {
			t.Fatalf("%+v", net)
		}
	})
	t.Run("a matching fingerprint binds and is ready evidence", func(t *testing.T) {
		agg, res := unitAggregate(t, t0)
		net := res["network"]
		g, req := net.RequestObservation(t0)
		_, _ = agg.ApplyObservation("network", g, req, ObservationAnswer{Obs: unitObs(net, PhaseReady, "u-net", time.Time{})}, t0)
		if !net.IsReady() || net.UID != "u-net" || net.BoundAt == nil {
			t.Fatalf("%+v", net)
		}
		// A later observation of another instance is a collision; the bound UID stays.
		g, req = net.RequestObservation(t0.Add(time.Second))
		_, _ = agg.ApplyObservation("network", g, req, ObservationAnswer{Obs: unitObs(net, PhaseReady, "u-other", time.Time{})}, t0.Add(time.Second))
		if net.UID != "u-net" || net.Reason != ReasonResourceCollision {
			t.Fatalf("%+v", net)
		}
	})
	t.Run("an older collision or fenced answer is discarded", func(t *testing.T) {
		for name, old := range map[string]ObservationAnswer{"collision": {Collision: true}, "fenced": {Fenced: true}} {
			agg, res := unitAggregate(t, t0)
			net := res["network"]
			g1, r1 := net.RequestObservation(t0)
			g2, r2 := net.RequestObservation(t0.Add(time.Second))
			_, _ = agg.ApplyObservation("network", g2, r2, ObservationAnswer{Obs: unitObs(net, PhaseReady, "u-net", time.Time{})}, t0.Add(2*time.Second))
			if ok, _ := agg.ApplyObservation("network", g1, r1, old, t0.Add(3*time.Second)); ok || !net.IsReady() || agg.Run.FailureIntentAt != nil {
				t.Fatalf("%s: an older answer superseded a newer one", name)
			}
		}
	})
}
