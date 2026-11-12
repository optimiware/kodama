// Kubernetes runtime adapter API pinned by these tests.
//
// This file is the test-side declaration of the minimal Kubernetes API seam
// that the production adapter under internal/shell/runtime/kube implements. It
// is deliberately much smaller than client-go, imports no Kubernetes module and
// performs no network, process or cluster call: every test drives the
// deterministic in-memory fake in fake_test.go.
//
// The adapter implements the declared runtime.Client seam exactly:
//
//	Capabilities(context.Context) (engine.Capabilities, error)
//	Prepare(context.Context, engine.PrepareRequest) (engine.Preparation, error)
//	Start(context.Context, engine.StartRequest) (engine.StartResult, error)
//	Observe(context.Context, engine.ObserveRequest) (kernel.Observation, error)
//	Logs(context.Context, engine.LogsRequest) (engine.LogBatch, error)
//	Stop(context.Context, engine.StopRequest) (kernel.StopEvidence, error)
//	Remove(context.Context, engine.RemoveRequest) (engine.Removal, error)
//	RefreshCredential(context.Context, engine.CredentialRequest) (engine.CredentialRefresh, error)
//	Version(context.Context) (runtime.APIVersion, error)
//
// It also implements the declared Kernel retained-workspace port:
//
//	Status(context.Context, id.WorkspaceID) (engine.WorkspaceObservation, error)
//	Retain(context.Context, id.WorkspaceID) error
//	Release(context.Context, id.WorkspaceID) error
//
// Pinned package declarations (in api.go, manifest.go, adapter.go, evidence.go,
// workspace.go and doc.go):
//
//	func New(Config) (*Adapter, error)
//	type Adapter struct{ ... }                      // implements runtime.Client
//	func CheckRequirements([]string) error          // fault.UnsupportedCapability
//	func WorkspaceClaimName(id.WorkspaceID) (string, error)
//
//	type Config struct {
//	    Cluster   Cluster
//	    Namespace string       // the tenant namespace this adapter is bound to
//	    TenantID  id.TenantID  // tenant ownership label expected on that namespace
//	    KodamaURL string       // reserved KODAMA_URL value
//	    Policy    Policy
//	    Cleanup   CleanupPolicy
//	    Clock     Clock        // optional; nil means a real clock
//	}
//	type Policy struct {
//	    CPURequest, CPULimit, MemoryRequest, MemoryLimit string
//	    JobTTLSeconds int
//	}
//	type CleanupPolicy struct {
//	    Attempts int           // total attempts per Remove call, >= 1
//	    Backoff  time.Duration // delay between attempts via Clock.Sleep
//	}
//	type Clock interface {
//	    Now() time.Time
//	    Sleep(context.Context, time.Duration) error
//	}
//
//	type Cluster interface {                // the only external effect surface
//	    GetNamespace(context.Context, string) (Namespace, error)
//	    CreateJob(context.Context, Job) (Job, error)
//	    GetJob(context.Context, string, string) (Job, error)
//	    DeleteJob(context.Context, string, string) error
//	    ListPods(context.Context, string, map[string]string) ([]Pod, error)
//	    ReadPodLogs(context.Context, string, string, PodLogRequest) (PodLogPage, error)
//	    PutConfigMap(context.Context, ConfigMap) (ConfigMap, error)
//	    GetConfigMap(context.Context, string, string) (ConfigMap, error)
//	    DeleteConfigMap(context.Context, string, string) error
//	    PutSecret(context.Context, Secret) (Secret, error)
//	    GetSecret(context.Context, string, string) (Secret, error)
//	    DeleteSecret(context.Context, string, string) error
//	    GetClaim(context.Context, string, string) (Claim, error)
//	    UpdateClaim(context.Context, Claim) (Claim, error)
//	    DeleteClaim(context.Context, string, string) error
//	    DeleteClaimVersioned(context.Context, string, string, string) error
//	    ListJobs(context.Context, string, map[string]string) ([]Job, error)
//	}
//
// Value types: Namespace, Job, JobSpec, JobStatus, EnvVar, Resources, Volume,
// VolumeItem, Pod, ContainerStatus, Secret, ConfigMap, Claim, PodLogRequest and
// PodLogPage. Jobs carry a stable Name derived from the persisted
// engine.EffectIdentity.WorkloadID (never GenerateName), the tenant/execution/
// action/workload/workspace labels below, and the idempotency-key annotation.
// A Job mounts the pre-existing retained workspace claim at /workspace, the
// immutable activation ConfigMap at /kodama and the credential Secret at
// /kodama/secrets. Pods carry the workload label so ListPods can select them.
//
// Sentinel errors: ErrNotFound, ErrAlreadyExists and ErrConflict. They are
// wrapped by the fake and interpreted by the adapter; an object that is absent
// is never converted into proof that a command never ran or that a process
// stopped.
//
// Constants: MaxLogBytes (bounded log read), Redacted ("[REDACTED]"), the label
// keys LabelTenant, LabelExecution, LabelAction, LabelWorkload, LabelWorkspace,
// LabelRetained and the annotations AnnotationIdempotencyKey and
// AnnotationRetained. Claim carries labels, annotations, finalizers, a
// deletion timestamp and a resource version so retention and release can be
// conditional and terminating claims stay observable.
//
// Behavior rules these tests pin (all from S02 §5–6, S07 and S10 §2):
//   - Prepare attaches the existing workspace claim, materializes the activation
//     snapshot once (immutable ConfigMap plus mutable credential Secret), and
//     refuses a missing claim, a foreign-tenant namespace, a partial or
//     credential-less snapshot and changed content for the same identity.
//   - Start creates one stable Job with a pinned image, a fail-fast script,
//     RestartPolicy Never, BackoffLimit zero, TTL, tenant limits and labels, and
//     no service-account token or host network. It never retries or replaces a
//     Job internally and never reports proven-not-started without affirmative
//     evidence.
//   - Observe reports absent/unknown/running/exited with the declared Kernel
//     vocabulary; absence and node loss stay uncertain, a Job with no pod yet is
//     running, and two pods for one identity are unknown (never a silent replay).
//   - Stop acknowledges a deletion request but returns unconfirmed until pod
//     termination is affirmatively observed. Job deletion, pod disappearance,
//     lease/elapsed time and transport loss never confirm a stop. Deleting an
//     already-absent Job is an acknowledged request, not an error.
//   - Remove is idempotent, attributed and bounded: a still-live pod or an
//     unreachable pod listing retains the workspace claim and names it in
//     Detail, failures name the retained resource, and Cleanup.Attempts bounds
//     the deletion attempts per call with Clock.Sleep(Backoff) between them.
//   - RefreshCredential updates only the credential Secret, never restarts or
//     replaces the Job, and survives a lost update response and a stale
//     resource version.
//   - Logs are ordered, bounded and redact configured credential/secret values;
//     they carry no lifecycle authority. NextOffset packs the stdout and stderr
//     positions into one token so a later call resumes both without skipping the
//     quieter stream.
package kube

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/runtime"
)

// TestAdapterSatisfiesRuntimeClient is a compile-time proof that the adapter
// exposes exactly the declared runtime seam and no parallel vocabulary.
var _ runtime.Client = (*Adapter)(nil)

// TestAdapterSatisfiesWorkspace is a compile-time proof that the adapter also
// implements the declared Kernel retained-workspace port.
var _ engine.Workspace = (*Adapter)(nil)

// TestAdapterPinsKubernetesAPIVersion pins batch/v1 and the Kubernetes family.
func TestAdapterPinsKubernetesAPIVersion(t *testing.T) {
	adapter := newTestAdapter(t, newFakeCluster(), newFakeClock())

	version, err := adapter.Version(context.Background())
	if err != nil {
		t.Fatalf("Version returned error: %v", err)
	}
	want := runtime.APIVersion{Adapter: runtime.AdapterKubernetes, Version: "batch/v1"}
	if version != want {
		t.Fatalf("Version = %+v; want %+v", version, want)
	}

	var pinned bool
	for _, candidate := range runtime.PinnedAPIVersions() {
		if candidate.Adapter == runtime.AdapterKubernetes && candidate.Version == version.Version {
			pinned = true
		}
	}
	if !pinned {
		t.Fatalf("adapter version %q is not the declared Kubernetes API version", version.Version)
	}
}

// TestAdapterCapabilitiesAreExplicit pins the Kubernetes capability set and
// proves it makes no process or process-group claim.
func TestAdapterCapabilitiesAreExplicit(t *testing.T) {
	fake := newFakeCluster()
	adapter := newTestAdapter(t, fake, newFakeClock())

	capabilities, err := adapter.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities returned error: %v", err)
	}
	want := map[string]bool{}
	for _, capability := range runtime.CapabilitiesFor(runtime.AdapterKubernetes) {
		want[string(capability)] = true
	}
	if len(capabilities.Names) != len(want) {
		t.Fatalf("capabilities = %v; want the %d declared Kubernetes capabilities", capabilities.Names, len(want))
	}
	for _, name := range capabilities.Names {
		if !want[name] {
			t.Errorf("capabilities declare %q, which the declared Kubernetes family does not have", name)
		}
	}
	for _, forbidden := range []string{
		string(runtime.CapabilityProcess),
		string(runtime.CapabilityProcessGroupStop),
	} {
		if capabilities.Supports(forbidden) {
			t.Errorf("kubernetes adapter claims %q", forbidden)
		}
	}
	if !capabilities.Supports(string(runtime.CapabilityJobDeletionNotStopProof)) {
		t.Error("kubernetes adapter does not mark Job deletion as non-proof of stop")
	}
	if calls := fake.callTotal(); calls != 0 {
		t.Fatalf("Capabilities made %d external calls; it must be declaration-only", calls)
	}
}

// TestCheckRequirementsRefusesUnsupportedWork pins pre-dispatch capability
// refusal. A selected adapter that cannot satisfy an admitted requirement must
// refuse the work instead of degrading silently.
func TestCheckRequirementsRefusesUnsupportedWork(t *testing.T) {
	if err := CheckRequirements(nil); err != nil {
		t.Fatalf("CheckRequirements(nil) = %v; want nil", err)
	}

	supported := make([]string, 0, 5)
	for _, capability := range runtime.CapabilitiesFor(runtime.AdapterKubernetes) {
		supported = append(supported, string(capability))
	}
	if err := CheckRequirements(supported); err != nil {
		t.Fatalf("CheckRequirements(%v) = %v; want nil", supported, err)
	}

	for _, required := range []string{
		string(runtime.CapabilityProcess),
		string(runtime.CapabilityProcessGroupStop),
		"gpu",
	} {
		err := CheckRequirements([]string{string(runtime.CapabilityContainer), required})
		if err == nil {
			t.Fatalf("CheckRequirements accepted unsupported capability %q", required)
		}
		class, ok := fault.ClassOf(err)
		if !ok || class != fault.UnsupportedCapability {
			t.Fatalf("CheckRequirements(%q) error class = %v (present=%v); want %q", required, class, ok, fault.UnsupportedCapability)
		}
	}
}

// TestNewRefusesIncompleteConfiguration proves the adapter cannot be composed
// with an unknown namespace, tenant, limits or cleanup bound.
func TestNewRefusesIncompleteConfiguration(t *testing.T) {
	fake := newFakeCluster()
	clock := newFakeClock()

	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "nil cluster", mutate: func(config *Config) { config.Cluster = nil }},
		{name: "empty namespace", mutate: func(config *Config) { config.Namespace = "" }},
		{name: "empty tenant", mutate: func(config *Config) { config.TenantID = "" }},
		{name: "empty url", mutate: func(config *Config) { config.KodamaURL = "" }},
		{name: "empty cpu limit", mutate: func(config *Config) { config.Policy.CPULimit = "" }},
		{name: "zero ttl", mutate: func(config *Config) { config.Policy.JobTTLSeconds = 0 }},
		{name: "zero cleanup attempts", mutate: func(config *Config) { config.Cleanup.Attempts = 0 }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := testConfig(fake, clock)
			testCase.mutate(&config)
			if _, err := New(config); err == nil {
				t.Fatalf("New accepted configuration with %s", testCase.name)
			}
		})
	}

	if _, err := New(testConfig(fake, clock)); err != nil {
		t.Fatalf("New refused a complete configuration: %v", err)
	}
}

// TestPinnedLabelVocabulary pins the exact ownership/selector keys the manifests
// and pod selectors share, so a deployment network policy and the adapter agree.
func TestPinnedLabelVocabulary(t *testing.T) {
	pairs := map[string]string{
		LabelTenant:              "kodama.dev/tenant",
		LabelExecution:           "kodama.dev/execution",
		LabelAction:              "kodama.dev/action",
		LabelWorkload:            "kodama.dev/workload",
		LabelWorkspace:           "kodama.dev/workspace",
		AnnotationIdempotencyKey: "kodama.dev/idempotency-key",
	}
	for got, want := range pairs {
		if got != want {
			t.Errorf("pinned key = %q; want %q", got, want)
		}
	}
	if Redacted != "[REDACTED]" {
		t.Errorf("Redacted = %q; want %q", Redacted, "[REDACTED]")
	}
	if MaxLogBytes <= 0 {
		t.Errorf("MaxLogBytes = %d; want a positive bound", MaxLogBytes)
	}
}

// TestWorkspaceClaimNameIsStable pins deterministic, workspace-only naming so
// the Shell can provision the retained claim before Prepare and the adapter can
// attach exactly that claim after a restart.
func TestWorkspaceClaimNameIsStable(t *testing.T) {
	first, err := WorkspaceClaimName(testWorkspaceID())
	if err != nil {
		t.Fatalf("WorkspaceClaimName returned error: %v", err)
	}
	second, err := WorkspaceClaimName(testWorkspaceID())
	if err != nil {
		t.Fatalf("WorkspaceClaimName returned error: %v", err)
	}
	if first != second {
		t.Fatalf("WorkspaceClaimName is not stable: %q then %q", first, second)
	}
	if first == "" {
		t.Fatal("WorkspaceClaimName returned an empty name")
	}

	other, err := WorkspaceClaimName("workspace-2")
	if err != nil {
		t.Fatalf("WorkspaceClaimName returned error: %v", err)
	}
	if other == first {
		t.Fatalf("distinct workspaces share claim name %q", first)
	}

	if _, err := WorkspaceClaimName("bad workspace"); err == nil {
		t.Fatal("WorkspaceClaimName accepted an invalid workspace id")
	}
}

// testIdentity is the persisted effect identity every operation must reuse.
func testIdentity() engine.EffectIdentity {
	return engine.EffectIdentity{
		ExecutionID:    "exec-1",
		ActionID:       "action-1",
		WorkloadID:     "workload-1",
		IdempotencyKey: "idem-1",
	}
}

func testWorkspaceID() id.WorkspaceID {
	return "workspace-1"
}

// testDeadline is a fixed authoritative time; tests never read the wall clock.
var testDeadline = time.Date(2026, time.November, 3, 12, 0, 0, 0, time.UTC)
