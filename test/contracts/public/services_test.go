package publiccontract

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/userland/services"
)

// TestEffectStateAndMergeResult pins the independent-service effect vocabulary:
// an uncertain effect is never released on lease expiry alone, and a merge
// result reports only observed state, never review authority.
func TestEffectStateAndMergeResult(t *testing.T) {
	for _, state := range []services.EffectState{services.EffectNotStarted, services.EffectApplied, services.EffectUncertain} {
		if !state.Valid() {
			t.Errorf("EffectState(%q).Valid() = false", state)
		}
	}
	if services.EffectState("maybe").Valid() {
		t.Error("EffectState(maybe).Valid() = true")
	}
	if !services.EffectNotStarted.Releasable() {
		t.Error("not_started must be releasable: the effect provably never started")
	}
	if !services.EffectApplied.Releasable() {
		t.Error("applied must be releasable: the effect provably settled")
	}
	if services.EffectUncertain.Releasable() {
		t.Error("uncertain must not be releasable: it needs reconciliation, not lease expiry")
	}
	if services.EffectState("maybe").Releasable() {
		t.Error("an unknown state reported releasable")
	}

	key := services.MergeKey{Repository: "shop/web", Ref: "refs/heads/main"}
	if err := key.Validate(); err != nil {
		t.Fatalf("valid merge key refused: %v", err)
	}
	for _, invalid := range []services.MergeKey{
		{Repository: "", Ref: "refs/heads/main"},
		{Repository: "shop/web", Ref: ""},
		{Repository: "shop/web", Ref: "heads/main"},
		{Repository: "shop/web", Ref: "refs/heads/*"},
		{Repository: "/shop/web", Ref: "refs/heads/main"},
		{Repository: "shop/../web", Ref: "refs/heads/main"},
		{Repository: "shop web", Ref: "refs/heads/main"},
	} {
		if err := invalid.Validate(); err == nil {
			t.Errorf("MergeKey.Validate accepted %+v", invalid)
		}
	}

	// A claim may expire, but expiry is not evidence about the effect.
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	claim := services.Claim{Key: key, Owner: "merge-worker-1", ExpiresAt: now.Add(-time.Minute)}
	if !claim.Expired(now) {
		t.Fatal("Expired(now) = false for an expired claim")
	}
	if services.EffectUncertain.Releasable() {
		t.Fatal("an expired claim released an uncertain effect")
	}

	applied := services.MergeResult{Key: key, State: services.EffectApplied, ObservedBase: "base-1", ObservedCandidate: "cand-1"}
	if err := applied.Validate(); err != nil {
		t.Fatalf("valid applied merge result refused: %v", err)
	}
	notStarted := services.MergeResult{Key: key, State: services.EffectNotStarted}
	if err := notStarted.Validate(); err != nil {
		t.Fatalf("valid not_started merge result refused: %v", err)
	}
	uncertain := services.MergeResult{Key: key, State: services.EffectUncertain, ObservedBase: "base-1"}
	if err := uncertain.Validate(); err != nil {
		t.Fatalf("valid uncertain merge result refused: %v", err)
	}
	for _, invalid := range []services.MergeResult{
		{Key: key, State: "maybe"},
		{Key: services.MergeKey{}, State: services.EffectApplied, ObservedBase: "a", ObservedCandidate: "b"},
		{Key: key, State: services.EffectApplied, ObservedBase: "a"},
		{Key: key, State: services.EffectApplied, ObservedCandidate: "b"},
		{Key: key, State: services.EffectNotStarted, ObservedBase: "a"},
	} {
		if err := invalid.Validate(); err == nil {
			t.Errorf("MergeResult.Validate accepted %+v", invalid)
		}
	}

	// A merge result cannot claim human-role review or approval.
	assertNoFieldMatching(t, reflect.TypeOf(services.MergeResult{}), "approval", "role", "reviewer", "authority")
}

type fakeServiceAdapter struct {
	kind services.ServiceKind
}

func (adapter fakeServiceAdapter) Kind() services.ServiceKind { return adapter.kind }

func (adapter fakeServiceAdapter) Run(context.Context) error { return nil }

var _ services.ServiceAdapter = fakeServiceAdapter{}

// fakeMergeCoordinator enforces the release rule: an uncertain effect is
// refused and must be reconciled first.
type fakeMergeCoordinator struct {
	claims   map[services.MergeKey]services.Claim
	releases []services.EffectState
}

func (coordinator *fakeMergeCoordinator) Claim(_ context.Context, key services.MergeKey) (services.Claim, error) {
	if err := key.Validate(); err != nil {
		return services.Claim{}, err
	}
	claim, ok := coordinator.claims[key]
	if ok {
		return claim, nil
	}
	claim = services.Claim{Key: key, Owner: "merge-worker-1", ExpiresAt: time.Date(2026, 1, 2, 4, 4, 5, 0, time.UTC)}
	coordinator.claims[key] = claim
	return claim, nil
}

func (coordinator *fakeMergeCoordinator) Release(_ context.Context, claim services.Claim, evidence services.EffectState) error {
	if !evidence.Valid() || !evidence.Releasable() {
		return errors.New("effect is not settled; reconcile before release")
	}
	coordinator.releases = append(coordinator.releases, evidence)
	delete(coordinator.claims, claim.Key)
	return nil
}

var _ services.MergeCoordinator = (*fakeMergeCoordinator)(nil)

// TestIndependentServiceAdapters pins the service adapter and merge
// coordination ports and their release semantics with fakes only.
func TestIndependentServiceAdapters(t *testing.T) {
	for _, kind := range []services.ServiceKind{
		services.KindWebhook, services.KindEmail, services.KindTimer,
		services.KindResponder, services.KindMergeCoordinator,
	} {
		if !kind.Valid() {
			t.Errorf("ServiceKind(%q).Valid() = false", kind)
		}
		adapter := fakeServiceAdapter{kind: kind}
		if adapter.Kind() != kind {
			t.Errorf("adapter kind = %q; want %q", adapter.Kind(), kind)
		}
		if err := adapter.Run(context.Background()); err != nil {
			t.Errorf("adapter Run: %v", err)
		}
	}
	if services.ServiceKind("artifact_publisher").Valid() {
		t.Error("ServiceKind(artifact_publisher).Valid() = true")
	}

	coordinator := &fakeMergeCoordinator{claims: make(map[services.MergeKey]services.Claim)}
	key := services.MergeKey{Repository: "shop/deploy", Ref: "refs/heads/release/2026"}
	claim, err := coordinator.Claim(context.Background(), key)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if claim.Key != key || claim.Owner == "" {
		t.Fatalf("claim = %+v", claim)
	}
	if err := coordinator.Release(context.Background(), claim, services.EffectUncertain); err == nil {
		t.Fatal("Release accepted an uncertain effect")
	}
	if err := coordinator.Release(context.Background(), claim, services.EffectApplied); err != nil {
		t.Fatalf("Release(applied): %v", err)
	}
	if len(coordinator.releases) != 1 || coordinator.releases[0] != services.EffectApplied {
		t.Fatalf("releases = %+v", coordinator.releases)
	}
	if _, stillClaimed := coordinator.claims[key]; stillClaimed {
		t.Fatal("applied effect did not release the claim")
	}
	if strings.Contains(strings.ToLower(claim.Owner), "token") {
		t.Fatal("claim owner carries a credential")
	}
}

// TestServiceAdapterWireTypesAreServedDeclarations pins the service package's
// wire alignment: every adapter request/result name is an alias of the exact
// served api declaration, never a private lookalike with its own members. The
// task.inbox case matters because a private InboxInput that marshals
// tenant_id, limit and cursor is refused by the strict task.inbox codec (the
// served input carries project_id, kinds and page).
func TestServiceAdapterWireTypesAreServedDeclarations(t *testing.T) {
	pairs := map[string][2]reflect.Type{
		"inbox input":    {reflect.TypeOf(services.InboxInput{}), reflect.TypeOf(api.TaskInboxRequest{})},
		"inbox result":   {reflect.TypeOf(services.InboxResult{}), reflect.TypeOf(api.TaskInboxResult{})},
		"inspect input":  {reflect.TypeOf(services.InspectInput{}), reflect.TypeOf(api.ExecutionInspectRequest{})},
		"inspect result": {reflect.TypeOf(services.InspectResult{}), reflect.TypeOf(api.ExecutionInspectResult{})},
		"resolve input":  {reflect.TypeOf(services.ResolveInput{}), reflect.TypeOf(api.ExecutionRequestResolveRequest{})},
	}
	for name, pair := range pairs {
		if pair[0] != pair[1] {
			t.Errorf("%s type = %v; want the exact served declaration %v", name, pair[0], pair[1])
		}
	}

	var codec api.OperationCodec
	for _, candidate := range api.OperationCodecs() {
		if candidate.Operation == "task.inbox" {
			codec = candidate
			break
		}
	}
	if codec.Operation == "" {
		t.Fatal("task.inbox codec is missing")
	}
	encoded, err := codec.EncodeRequest(services.InboxInput{ProjectID: "project-1"})
	if err != nil {
		t.Fatalf("task.inbox refused the service inbox input: %v", err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatalf("task.inbox input is not a JSON object: %v", err)
	}
	for _, legacy := range []string{"tenant_id", "limit", "cursor"} {
		if _, present := wire[legacy]; present {
			t.Errorf("task.inbox input carries the private member %q: %s", legacy, encoded)
		}
	}
	if string(wire["project_id"]) != `"project-1"` {
		t.Errorf("task.inbox project_id = %s; want \"project-1\"", wire["project_id"])
	}
	decoded, err := codec.DecodeRequest(encoded)
	if err != nil {
		t.Fatalf("task.inbox refused its own encoded service input: %v", err)
	}
	request, ok := decoded.(api.TaskInboxRequest)
	if !ok || request.ProjectID != "project-1" {
		t.Fatalf("decoded task.inbox request = %#v; want project-1", decoded)
	}
}

// fakeMergeRunner is the compile-only merge dependency of the adapter.
type fakeMergeRunner struct{}

func (fakeMergeRunner) Merge(context.Context, services.MergeRequest) (services.MergeResult, error) {
	return services.MergeResult{}, nil
}

func (fakeMergeRunner) Settle(context.Context, services.MergeRequest, services.EffectState) error {
	return nil
}

func (fakeMergeRunner) Pending(context.Context) ([]services.PendingMerge, error) { return nil, nil }

var _ services.MergeRunner = fakeMergeRunner{}

// emptyEventSource is a compile-only retained-notification source.
type emptyEventSource struct{}

func (emptyEventSource) Poll(context.Context, string) (services.NotificationPage, error) {
	return services.NotificationPage{}, nil
}

// TestMergeAssignmentMapping pins the typed merge subject and declared output
// mapping: malformed mappings are refused before any effect, and only observed
// values may fill a declared observation.
func TestMergeAssignmentMapping(t *testing.T) {
	key := services.MergeKey{Repository: "shop/web", Ref: "refs/heads/work/merge-1"}
	valid := &api.Subject{Fields: []api.SubjectField{
		{Name: services.MergeFieldRepository, Kind: "string", Value: "shop/web"},
		{Name: services.MergeFieldRef, Kind: "string", Value: "refs/heads/work/merge-1"},
		{Name: services.MergeFieldBase, Kind: "string", Value: "base-1"},
		{Name: services.MergeFieldCandidate, Kind: "string", Value: "candidate-1"},
	}}
	mapped, err := services.ParseMergeSubject(valid)
	if err != nil {
		t.Fatalf("valid merge subject refused: %v", err)
	}
	if mapped.Key != key || mapped.Base != "base-1" || mapped.Candidate != "candidate-1" {
		t.Fatalf("mapped subject = %+v; want %+v", mapped, key)
	}

	for name, subject := range map[string]*api.Subject{
		"nil":            nil,
		"empty":          {},
		"missing field":  {Fields: valid.Fields[:3]},
		"extra field":    {Fields: append(append([]api.SubjectField(nil), valid.Fields...), api.SubjectField{Name: "approval", Kind: "string", Value: "role/reviewer"})},
		"wrong kind":     {Fields: []api.SubjectField{{Name: services.MergeFieldRepository, Kind: "string", Value: "shop/web"}, {Name: services.MergeFieldRef, Kind: "string", Value: "refs/heads/work/merge-1"}, {Name: services.MergeFieldBase, Kind: "string", Value: "base-1"}, {Name: services.MergeFieldCandidate, Kind: "null", Value: ""}}},
		"empty value":    {Fields: []api.SubjectField{{Name: services.MergeFieldRepository, Kind: "string", Value: ""}, {Name: services.MergeFieldRef, Kind: "string", Value: "refs/heads/work/merge-1"}, {Name: services.MergeFieldBase, Kind: "string", Value: "base-1"}, {Name: services.MergeFieldCandidate, Kind: "string", Value: "candidate-1"}}},
		"invalid ref":    {Fields: []api.SubjectField{{Name: services.MergeFieldRepository, Kind: "string", Value: "shop/web"}, {Name: services.MergeFieldRef, Kind: "string", Value: "heads/main"}, {Name: services.MergeFieldBase, Kind: "string", Value: "base-1"}, {Name: services.MergeFieldCandidate, Kind: "string", Value: "candidate-1"}}},
		"traversal repo": {Fields: []api.SubjectField{{Name: services.MergeFieldRepository, Kind: "string", Value: "../escape"}, {Name: services.MergeFieldRef, Kind: "string", Value: "refs/heads/work/merge-1"}, {Name: services.MergeFieldBase, Kind: "string", Value: "base-1"}, {Name: services.MergeFieldCandidate, Kind: "string", Value: "candidate-1"}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := services.ParseMergeSubject(subject); err == nil {
				t.Fatalf("ParseMergeSubject accepted %+v", subject)
			}
		})
	}

	for _, outputs := range [][]string{
		nil,
		{},
		{services.MergeOutputBase, "approval"},
		{services.MergeOutputBase, services.MergeOutputBase},
		{""},
	} {
		if err := services.ValidateMergeOutputs(outputs); err == nil {
			t.Errorf("ValidateMergeOutputs accepted %v", outputs)
		}
	}
	declared := []string{services.MergeOutputBase, services.MergeOutputCandidate, services.MergeOutputResult}
	if err := services.ValidateMergeOutputs(declared); err != nil {
		t.Fatalf("valid declared outputs refused: %v", err)
	}

	applied := services.MergeResult{Key: key, State: services.EffectApplied, ObservedBase: "base-1", ObservedCandidate: "candidate-1", ObservedOutput: "Fast-forward"}
	values, err := services.MergeOutputValues(applied, declared)
	if err != nil {
		t.Fatalf("MergeOutputValues: %v", err)
	}
	want := map[string]string{services.MergeOutputBase: "base-1", services.MergeOutputCandidate: "candidate-1", services.MergeOutputResult: "Fast-forward"}
	if !reflect.DeepEqual(values, want) {
		t.Fatalf("values = %v; want the observed %v", values, want)
	}
	partial := applied
	partial.ObservedOutput = ""
	values, err = services.MergeOutputValues(partial, []string{services.MergeOutputBase, services.MergeOutputCandidate})
	if err != nil {
		t.Fatalf("MergeOutputValues without the optional output: %v", err)
	}
	if !reflect.DeepEqual(values, map[string]string{services.MergeOutputBase: "base-1", services.MergeOutputCandidate: "candidate-1"}) {
		t.Fatalf("undeclared output leaked into the answer: %v", values)
	}
	for _, invalid := range []services.MergeResult{
		{Key: key, State: services.EffectUncertain},
		{Key: key, State: services.EffectNotStarted},
	} {
		if _, err := services.MergeOutputValues(invalid, declared); err == nil {
			t.Fatalf("MergeOutputValues accepted a non-applied result %+v", invalid)
		}
	}
}

// TestMergeAdapterRequiresItsDependency pins the missing-dependency refusal and
// the configured positive control.
func TestMergeAdapterRequiresItsDependency(t *testing.T) {
	config := services.AdapterConfig{
		Kind:      services.KindMergeCoordinator,
		ServiceID: "merge-svc",
		TenantID:  "tenant-a",
		ProjectID: "project-a",
		Handler:   &recordingHandler{},
		Events:    emptyEventSource{},
		Clock:     fakeClock{now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
	}
	if _, err := services.NewAdapter(config); err == nil {
		t.Fatal("NewAdapter accepted a merge coordinator without a merge dependency")
	}
	config.Merges = fakeMergeRunner{}
	if _, err := services.NewAdapter(config); err != nil {
		t.Fatalf("NewAdapter refused a configured merge coordinator: %v", err)
	}
}

// TestMergeRequestRequiresTheExactAssignment pins the durable replay identity.
func TestMergeRequestRequiresTheExactAssignment(t *testing.T) {
	key := services.MergeKey{Repository: "shop/web", Ref: "refs/heads/work/merge-1"}
	request := services.MergeRequest{
		Key:        key,
		Assignment: services.MergeAssignment{ExecutionID: "exec-1", ActionID: "action-1"},
		Base:       "base-1",
		Candidate:  "candidate-1",
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("valid assignment-keyed merge request refused: %v", err)
	}
	for name, assignment := range map[string]services.MergeAssignment{
		"missing execution": {ActionID: "action-1"},
		"missing action":    {ExecutionID: "exec-1"},
		"traversal":         {ExecutionID: "../escape", ActionID: "action-1"},
	} {
		t.Run(name, func(t *testing.T) {
			invalid := request
			invalid.Assignment = assignment
			if err := invalid.Validate(); err == nil {
				t.Fatalf("MergeRequest.Validate accepted assignment %+v", assignment)
			}
		})
	}
}
