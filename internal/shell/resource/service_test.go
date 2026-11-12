package resource_test

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/resource"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

type replayBindingApply struct {
	commits     int
	replays     int
	stored      resource.BindingResult
	key         receipt.Key
	contentHash string
}

func (fake *replayBindingApply) Apply(_ context.Context, unit resource.BindingUnit) (resource.BindingResult, error) {
	if fake.commits == 0 {
		fake.commits++
		fake.stored = resource.BindingResult{Version: unit.Binding.Version}
		fake.key = unit.Receipt.Key
		fake.contentHash = unit.Receipt.ContentHash
		return fake.stored, nil
	}
	if unit.Receipt.Key != fake.key || unit.Receipt.ContentHash != fake.contentHash {
		return resource.BindingResult{}, fault.New(fault.ChangedIdempotencyContent, "changed idempotency content")
	}
	result := fake.stored
	result.Replayed = true
	fake.replays++
	return result, nil
}

type versionedBindingStore struct {
	current uint64
	commits int
}

func (fake *versionedBindingStore) Apply(_ context.Context, unit resource.BindingUnit) (resource.BindingResult, error) {
	if unit.ExpectedVersion != fake.current {
		return resource.BindingResult{}, fault.New(fault.VersionConflict, "stale binding version")
	}
	fake.current = unit.Binding.Version
	fake.commits++
	return resource.BindingResult{Version: unit.Binding.Version}, nil
}

func TestBindingRegistrationDoesNotTrustStaleActorRoles(t *testing.T) {
	ports := &fakeBindingPorts{}
	var captured resource.BindingUnit
	ports.apply = func(_ context.Context, unit resource.BindingUnit) (resource.BindingResult, error) {
		captured = unit
		return resource.BindingResult{Version: unit.Binding.Version}, nil
	}
	service := newBindingService(t, ports)
	command := validRegisterCommand()
	command.Actor.Roles = nil
	if _, err := service.RegisterBinding(context.Background(), command); err != nil {
		t.Fatalf("RegisterBinding: %v", err)
	}
	read := requireBindingAuthority(t, captured.Authority, "resource.binding.register", authorize.CheckMembership, command.Scope, command.Actor.ID)
	if !containsRole(read.Required.HumanRoles, tenancy.RoleAdmin) {
		t.Fatalf("authority grant = %v; want the registry Admin grant, not a stale actor role snapshot", read.Required.HumanRoles)
	}
}

func TestInjectedBindingFailureLeavesNoCommit(t *testing.T) {
	ports := &fakeBindingPorts{}
	calls := 0
	committed := false
	ports.apply = func(context.Context, resource.BindingUnit) (resource.BindingResult, error) {
		calls++
		return resource.BindingResult{}, fault.New(fault.DependencyUnavailable, "store unavailable")
	}
	service := newBindingService(t, ports)
	result, err := service.RegisterBinding(context.Background(), validRegisterCommand())
	requireFaultClass(t, err, fault.DependencyUnavailable)
	if calls != 1 {
		t.Fatalf("Apply calls = %d; want 1 attempted unit", calls)
	}
	if committed {
		t.Fatal("a failed port call reported a committed binding")
	}
	if result != (resource.BindingResult{}) {
		t.Fatalf("result = %+v; want zero result on refusal", result)
	}
}

func TestBindingCrashBeforeEffectThenRetryCommitsOnce(t *testing.T) {
	ports := &fakeBindingPorts{}
	calls := 0
	committed := 0
	ports.apply = func(_ context.Context, unit resource.BindingUnit) (resource.BindingResult, error) {
		calls++
		if calls == 1 {
			return resource.BindingResult{}, fault.New(fault.DependencyUnavailable, "commit did not start")
		}
		committed++
		return resource.BindingResult{Version: unit.Binding.Version}, nil
	}
	service := newBindingService(t, ports)
	command := validRegisterCommand()
	if _, err := service.RegisterBinding(context.Background(), command); err == nil {
		t.Fatal("first attempt unexpectedly succeeded")
	}
	if committed != 0 {
		t.Fatalf("commits after failed attempt = %d; want 0", committed)
	}
	if _, err := service.RegisterBinding(context.Background(), command); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if committed != 1 {
		t.Fatalf("commits after retry = %d; want exactly 1", committed)
	}
}

func TestBindingReplayReturnsOriginalResult(t *testing.T) {
	fake := &replayBindingApply{}
	ports := &fakeBindingPorts{apply: fake.Apply}
	service := newBindingService(t, ports)
	command := validRegisterCommand()
	first, err := service.RegisterBinding(context.Background(), command)
	if err != nil {
		t.Fatalf("first RegisterBinding: %v", err)
	}
	if first.Replayed {
		t.Fatal("first commit reported a replay")
	}
	second, err := service.RegisterBinding(context.Background(), command)
	if err != nil {
		t.Fatalf("replayed RegisterBinding: %v", err)
	}
	if !second.Replayed {
		t.Fatal("identical retry did not report a replay")
	}
	if second.Version != first.Version {
		t.Fatalf("replay version = %d; want original %d", second.Version, first.Version)
	}
	if fake.commits != 1 || fake.replays != 1 {
		t.Fatalf("commits/replays = %d/%d; want 1/1", fake.commits, fake.replays)
	}
}

func TestBindingChangedReceiptContentIsRefused(t *testing.T) {
	fake := &replayBindingApply{}
	ports := &fakeBindingPorts{apply: fake.Apply}
	service := newBindingService(t, ports)
	first := validRegisterCommand()
	if _, err := service.RegisterBinding(context.Background(), first); err != nil {
		t.Fatalf("first RegisterBinding: %v", err)
	}
	changed := first
	changed.Content = receiptContent(`{"resource":"other"}`)
	_, err := service.RegisterBinding(context.Background(), changed)
	requireFaultClass(t, err, fault.ChangedIdempotencyContent)
	if fake.commits != 1 || fake.replays != 0 {
		t.Fatalf("commits/replays = %d/%d; want 1/0", fake.commits, fake.replays)
	}
}

func TestBindingStaleVersionPropagatesConflict(t *testing.T) {
	fake := &versionedBindingStore{}
	ports := &fakeBindingPorts{apply: fake.Apply}
	bindingService := newBindingService(t, ports)

	register := validRegisterCommand()
	if _, err := bindingService.RegisterBinding(context.Background(), register); err != nil {
		t.Fatalf("register: %v", err)
	}
	update := resource.UpdateBindingCommand{
		Actor:            bindingActor("tenant-a", "project-a", "principal-admin"),
		Scope:            projectScope("tenant-a", "project-a"),
		BindingID:        "binding-a",
		ResourceID:       "resource-a",
		EnforcingService: service.ServiceID("git-service"),
		TrustedOrigins:   []string{"https://git.example.com/shop"},
		Operations:       []resource.Operation{resource.OperationGitRefUpdate},
		ReportableEvents: []string{"git.ref.updated"},
		ExpectedVersion:  1,
		CallerKey:        "caller-update",
		Content:          receiptContent(`{"environment":"staging"}`),
	}
	if _, err := bindingService.UpdateBinding(context.Background(), update); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := bindingService.UpdateBinding(context.Background(), update); err == nil {
		t.Fatal("stale update unexpectedly succeeded")
	} else {
		requireFaultClass(t, err, fault.VersionConflict)
	}
	if fake.commits != 2 {
		t.Fatalf("commits = %d; want 2 after the stale attempt", fake.commits)
	}
}

func TestBindingRegistrationDefensiveCopies(t *testing.T) {
	ports := &fakeBindingPorts{}
	var captured resource.BindingUnit
	ports.apply = func(_ context.Context, unit resource.BindingUnit) (resource.BindingResult, error) {
		captured = unit
		return resource.BindingResult{Version: unit.Binding.Version}, nil
	}
	service := newBindingService(t, ports)
	command := validRegisterCommand()
	if _, err := service.RegisterBinding(context.Background(), command); err != nil {
		t.Fatalf("RegisterBinding: %v", err)
	}
	command.Metadata["environment"] = "mutated"
	command.TrustedOrigins[0] = "https://evil.example.com"
	command.Operations[0] = resource.Operation("mutated")
	command.ReportableEvents[0] = "mutated"
	if captured.Binding.Metadata["environment"] != "production" {
		t.Fatal("committed metadata aliases caller-owned map")
	}
	if !reflect.DeepEqual(captured.Binding.TrustedOrigins, []string{"https://git.example.com/shop"}) {
		t.Fatalf("committed origins = %v; want an independent copy", captured.Binding.TrustedOrigins)
	}
	if !reflect.DeepEqual(captured.Binding.Operations, []resource.Operation{resource.OperationGitRefUpdate}) {
		t.Fatalf("committed operations = %v; want an independent copy", captured.Binding.Operations)
	}
	if !reflect.DeepEqual(captured.Binding.ReportableEvents, []string{"git.ref.updated"}) {
		t.Fatalf("committed reportable events = %v; want an independent copy", captured.Binding.ReportableEvents)
	}
}

func containsRole(roles []tenancy.Role, want tenancy.Role) bool {
	return slices.Contains(roles, want)
}
