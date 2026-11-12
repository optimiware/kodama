package adaptercontract

import (
	"reflect"
	"testing"

	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/runtime"
)

// TestRuntimeAdapterKinds pins the three explicit runtime families.
func TestRuntimeAdapterKinds(t *testing.T) {
	want := []runtime.AdapterKind{
		runtime.AdapterProcess,
		runtime.AdapterPodman,
		runtime.AdapterKubernetes,
	}
	if got := runtime.AdapterKinds(); !reflect.DeepEqual(got, want) {
		t.Fatalf("adapter kinds = %v; want %v", got, want)
	}
	for _, kind := range want {
		if !kind.Valid() {
			t.Errorf("adapter kind %q is not valid", kind)
		}
	}
	if runtime.AdapterKind("docker").Valid() {
		t.Error("unknown adapter kind is valid")
	}
}

// TestRuntimePinnedAPIVersions pins the external API surface each runtime
// adapter targets. These are API versions, not product release claims.
func TestRuntimePinnedAPIVersions(t *testing.T) {
	var document struct {
		APIVersions map[string]string `json:"api_versions"`
	}
	readTestJSON(t, "runtime.json", &document)
	actual := make(map[string]string)
	for _, version := range runtime.PinnedAPIVersions() {
		if version.Version == "" {
			t.Errorf("adapter %q has an empty API version", version.Adapter)
		}
		actual[string(version.Adapter)] = version.Version
	}
	if !reflect.DeepEqual(actual, document.APIVersions) {
		t.Fatalf("pinned API versions = %v; want %v", actual, document.APIVersions)
	}
}

// TestRuntimeCapabilities pins the explicit capability sets. Process makes no
// container claim; Kubernetes marks Job deletion as not being stop proof.
func TestRuntimeCapabilities(t *testing.T) {
	var document struct {
		Capabilities map[string][]string `json:"capabilities"`
	}
	readTestJSON(t, "runtime.json", &document)
	for name, want := range document.Capabilities {
		kind := runtime.AdapterKind(name)
		actual := make([]string, 0)
		for _, capability := range runtime.CapabilitiesFor(kind) {
			actual = append(actual, string(capability))
		}
		if !sameStringSet(actual, want) {
			t.Errorf("capabilities for %q = %v; want %v", name, actual, want)
		}
	}
	if hasString(capabilityStrings(runtime.CapabilitiesFor(runtime.AdapterProcess)), "container") {
		t.Error("process adapter claims container isolation")
	}
	if !hasString(capabilityStrings(runtime.CapabilitiesFor(runtime.AdapterKubernetes)), "job-deletion-not-stop-proof") {
		t.Error("kubernetes adapter does not mark Job deletion as non-proof of stop")
	}
}

// TestRuntimeClientShape proves the client seam is exactly the Kernel runtime
// operations plus a pinned external version, so an adapter cannot invent a
// parallel start/observe vocabulary.
func TestRuntimeClientShape(t *testing.T) {
	client := reflect.TypeOf((*runtime.Client)(nil)).Elem()
	want := []string{
		"Capabilities",
		"Logs",
		"Observe",
		"Prepare",
		"RefreshCredential",
		"Remove",
		"Start",
		"Stop",
		"Version",
	}
	if got := methodNames(client); !reflect.DeepEqual(got, want) {
		t.Fatalf("runtime.Client methods = %v; want %v", got, want)
	}
	for index := 0; index < client.NumMethod(); index++ {
		method := client.Method(index)
		if method.Name == "Version" {
			continue
		}
		if method.Type.NumOut() < 2 {
			t.Errorf("runtime.Client.%s does not return evidence and error", method.Name)
		}
	}
}

// TestRuntimeRequestsCarryStableIdentity proves every runtime operation
// receives the same persisted effect identity, so a crash before or after an
// effect cannot cause the adapter to derive a different workload or replay it.
func TestRuntimeRequestsCarryStableIdentity(t *testing.T) {
	identityType := reflect.TypeOf(engine.EffectIdentity{})
	requests := []reflect.Type{
		reflect.TypeOf(engine.PrepareRequest{}),
		reflect.TypeOf(engine.StartRequest{}),
		reflect.TypeOf(engine.ObserveRequest{}),
		reflect.TypeOf(engine.StopRequest{}),
		reflect.TypeOf(engine.RemoveRequest{}),
		reflect.TypeOf(engine.CredentialRequest{}),
	}
	for _, request := range requests {
		field, ok := request.FieldByName("Identity")
		if !ok {
			t.Errorf("%s has no Identity field", request)
			continue
		}
		if field.Type != identityType {
			t.Errorf("%s.Identity = %v; want engine.EffectIdentity", request, field.Type)
		}
	}
	identity := engine.EffectIdentity{
		ExecutionID:    "exec-1",
		ActionID:       "action-1",
		WorkloadID:     "workload-1",
		IdempotencyKey: "key-1",
	}
	if identity.WorkloadID == "" || identity.IdempotencyKey == "" {
		t.Fatal("effect identity must carry a stable workload id and idempotency key")
	}
}

// TestKernelUncertaintyAndStopVocabulary proves the runtime seam consumes the
// Kernel evidence vocabulary instead of collapsing uncertainty.
func TestKernelUncertaintyAndStopVocabulary(t *testing.T) {
	wantOutcomes := []engine.StartOutcome{
		engine.StartAccepted,
		engine.StartProvenNotStarted,
		engine.StartUnknown,
	}
	if got := engine.StartOutcomes(); !reflect.DeepEqual(got, wantOutcomes) {
		t.Fatalf("start outcomes = %v; want %v", got, wantOutcomes)
	}
	wantKinds := []kernel.ObservationKind{
		kernel.ObservationUnknown,
		kernel.ObservationAbsent,
		kernel.ObservationNeverStarted,
		kernel.ObservationRunning,
		kernel.ObservationExited,
	}
	if got := kernel.ObservationKinds(); !reflect.DeepEqual(got, wantKinds) {
		t.Fatalf("observation kinds = %v; want %v", got, wantKinds)
	}
	wantStops := []kernel.StopState{kernel.StopStateConfirmed, kernel.StopStateUnconfirmed}
	if got := kernel.StopStates(); !reflect.DeepEqual(got, wantStops) {
		t.Fatalf("stop states = %v; want %v", got, wantStops)
	}

	cases := []struct {
		name          string
		observation   kernel.Observation
		wantNonexec   bool
		wantConfirmed bool
	}{
		{"unknown", kernel.UnknownObservation(), false, false},
		{"absent", kernel.AbsentObservation(), false, false},
		{"never started without proof", kernel.NeverStarted(""), false, false},
		{"never started with proof", kernel.NeverStarted("cgroup absent"), true, false},
		{"running", kernel.RunningObservation(), false, false},
		{"exited pending stop", kernel.ExitedObservation(137, kernel.PendingStop("barrier pending")), false, false},
		{"exited confirmed stop", kernel.ExitedObservation(0, kernel.ConfirmedStop("terminated")), false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.observation.ProofOfNonexecution(); got != tc.wantNonexec {
				t.Errorf("ProofOfNonexecution = %v; want %v", got, tc.wantNonexec)
			}
			if got := tc.observation.StopConfirmed(); got != tc.wantConfirmed {
				t.Errorf("StopConfirmed = %v; want %v", got, tc.wantConfirmed)
			}
		})
	}
}

func capabilityStrings(capabilities []runtime.Capability) []string {
	values := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		values = append(values, string(capability))
	}
	return values
}
