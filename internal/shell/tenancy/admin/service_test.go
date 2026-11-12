package tenancyadmin_test

import (
	"context"
	"slices"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/tenancy/admin"
)

// replayProjectCreate is a deterministic in-memory commit/replay fake. It keys
// idempotency on the receipt identity and content fingerprint the service
// produced, so a retry with equal content converges on the original result and a
// changed body is refused.
type replayProjectCreate struct {
	commits     int
	replays     int
	stored      tenancyadmin.ProjectCreateResult
	key         receipt.Key
	contentHash string
}

func (fake *replayProjectCreate) CreateProject(_ context.Context, unit tenancyadmin.ProjectCreateUnit) (tenancyadmin.ProjectCreateResult, error) {
	if fake.commits == 0 {
		fake.commits++
		fake.stored = tenancyadmin.ProjectCreateResult{ProjectID: unit.Project.ID, Version: unit.Project.Version}
		fake.key = unit.Receipt.Key
		fake.contentHash = unit.Receipt.ContentHash
		return fake.stored, nil
	}
	if unit.Receipt.Key != fake.key || unit.Receipt.ContentHash != fake.contentHash {
		return tenancyadmin.ProjectCreateResult{}, fault.New(fault.ChangedIdempotencyContent, "changed idempotency content")
	}
	result := fake.stored
	result.Replayed = true
	fake.replays++
	return result, nil
}

// versionedGrant enforces the membership expected-version precondition the same
// way a persistence adapter must.
type versionedGrant struct {
	current uint64
	commits int
}

func (fake *versionedGrant) GrantMembership(_ context.Context, unit tenancyadmin.MembershipGrantUnit) (tenancyadmin.MembershipGrantResult, error) {
	if unit.ExpectedVersion != fake.current {
		return tenancyadmin.MembershipGrantResult{}, fault.New(fault.VersionConflict, "stale membership version")
	}
	fake.current = unit.Membership.Version
	fake.commits++
	return tenancyadmin.MembershipGrantResult{Version: unit.Membership.Version}, nil
}

func TestAdministrationDoesNotTrustStaleActorRoles(t *testing.T) {
	ports := &fakeAdminPorts{}
	var captured tenancyadmin.ProjectCreateUnit
	ports.createProject = func(_ context.Context, unit tenancyadmin.ProjectCreateUnit) (tenancyadmin.ProjectCreateResult, error) {
		captured = unit
		return tenancyadmin.ProjectCreateResult{ProjectID: unit.Project.ID, Version: unit.Project.Version}, nil
	}
	service := newAdminService(t, ports)

	actor := adminActor("tenant-a", "", "principal-admin")
	actor.Roles = nil
	_, err := service.CreateProject(context.Background(), tenancyadmin.ProjectCreateCommand{
		Actor:     actor,
		ProjectID: "project-a",
		Name:      "Project A",
		CallerKey: "caller-1",
		Content:   receipt.Content{MediaType: "application/json", Bytes: []byte(`{}`)},
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	read := requireAuthority(t, captured.Authority, "project.create", authorize.CheckMembership, tenantScope("tenant-a"), actor.ID)
	if !containsRole(read.Required.HumanRoles, tenancy.RoleAdmin) {
		t.Fatalf("authority grant = %v; want the registry Admin grant, not a stale actor role snapshot", read.Required.HumanRoles)
	}
}

func TestInjectedPortFailureLeavesNoCommit(t *testing.T) {
	ports := &fakeAdminPorts{}
	calls := 0
	committed := false
	ports.createProject = func(context.Context, tenancyadmin.ProjectCreateUnit) (tenancyadmin.ProjectCreateResult, error) {
		calls++
		return tenancyadmin.ProjectCreateResult{}, fault.New(fault.DependencyUnavailable, "store unavailable")
	}
	service := newAdminService(t, ports)

	result, err := service.CreateProject(context.Background(), tenancyadmin.ProjectCreateCommand{
		Actor:     adminActor("tenant-a", "", "principal-admin"),
		ProjectID: "project-a",
		Name:      "Project A",
		CallerKey: "caller-1",
		Content:   receipt.Content{MediaType: "application/json", Bytes: []byte(`{}`)},
	})
	requireFaultClass(t, err, fault.DependencyUnavailable)
	if calls != 1 {
		t.Fatalf("CreateProject port calls = %d; want 1 attempted unit", calls)
	}
	if committed {
		t.Fatal("a failed port call reported a committed unit")
	}
	if result != (tenancyadmin.ProjectCreateResult{}) {
		t.Fatalf("result = %+v; want zero result on refusal", result)
	}
}

func TestCrashBeforeEffectThenRetryCommitsOnce(t *testing.T) {
	ports := &fakeAdminPorts{}
	calls := 0
	committed := 0
	ports.createProject = func(_ context.Context, unit tenancyadmin.ProjectCreateUnit) (tenancyadmin.ProjectCreateResult, error) {
		calls++
		if calls == 1 {
			return tenancyadmin.ProjectCreateResult{}, fault.New(fault.DependencyUnavailable, "commit did not start")
		}
		committed++
		return tenancyadmin.ProjectCreateResult{ProjectID: unit.Project.ID, Version: unit.Project.Version}, nil
	}
	service := newAdminService(t, ports)
	command := tenancyadmin.ProjectCreateCommand{
		Actor:     adminActor("tenant-a", "", "principal-admin"),
		ProjectID: "project-a",
		Name:      "Project A",
		CallerKey: "caller-1",
		Content:   receipt.Content{MediaType: "application/json", Bytes: []byte(`{}`)},
	}

	if _, err := service.CreateProject(context.Background(), command); err == nil {
		t.Fatal("first attempt unexpectedly succeeded")
	}
	if committed != 0 {
		t.Fatalf("commits after failed attempt = %d; want 0", committed)
	}
	if _, err := service.CreateProject(context.Background(), command); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if committed != 1 {
		t.Fatalf("commits after retry = %d; want exactly 1", committed)
	}
}

func TestReceiptReplayReturnsOriginalResult(t *testing.T) {
	fake := &replayProjectCreate{}
	ports := &fakeAdminPorts{createProject: fake.CreateProject}
	service := newAdminService(t, ports)
	command := tenancyadmin.ProjectCreateCommand{
		Actor:     adminActor("tenant-a", "", "principal-admin"),
		ProjectID: "project-a",
		Name:      "Project A",
		CallerKey: "caller-1",
		Content:   receipt.Content{MediaType: "application/json", Bytes: []byte(`{"name":"Project A"}`)},
	}

	first, err := service.CreateProject(context.Background(), command)
	if err != nil {
		t.Fatalf("first CreateProject: %v", err)
	}
	if first.Replayed {
		t.Fatal("first commit reported a replay")
	}
	second, err := service.CreateProject(context.Background(), command)
	if err != nil {
		t.Fatalf("replayed CreateProject: %v", err)
	}
	if !second.Replayed {
		t.Fatal("identical retry did not report a replay")
	}
	if second.ProjectID != first.ProjectID || second.Version != first.Version {
		t.Fatalf("replay result = %+v; want original %+v", second, first)
	}
	if fake.commits != 1 || fake.replays != 1 {
		t.Fatalf("commits/replays = %d/%d; want 1/1", fake.commits, fake.replays)
	}
}

func TestChangedReceiptContentIsRefused(t *testing.T) {
	fake := &replayProjectCreate{}
	ports := &fakeAdminPorts{createProject: fake.CreateProject}
	service := newAdminService(t, ports)
	first := tenancyadmin.ProjectCreateCommand{
		Actor:     adminActor("tenant-a", "", "principal-admin"),
		ProjectID: "project-a",
		Name:      "Project A",
		CallerKey: "caller-1",
		Content:   receipt.Content{MediaType: "application/json", Bytes: []byte(`{"name":"Project A"}`)},
	}
	if _, err := service.CreateProject(context.Background(), first); err != nil {
		t.Fatalf("first CreateProject: %v", err)
	}
	changed := first
	changed.Content = receipt.Content{MediaType: "application/json", Bytes: []byte(`{"name":"Different"}`)}
	_, err := service.CreateProject(context.Background(), changed)
	requireFaultClass(t, err, fault.ChangedIdempotencyContent)
	if fake.commits != 1 || fake.replays != 0 {
		t.Fatalf("commits/replays = %d/%d; want 1/0", fake.commits, fake.replays)
	}
}

func TestStaleMembershipVersionPropagatesConflict(t *testing.T) {
	fake := &versionedGrant{}
	ports := &fakeAdminPorts{grantMembership: fake.GrantMembership}
	service := newAdminService(t, ports)

	grant := func(expected uint64) (tenancyadmin.MembershipGrantResult, error) {
		return service.GrantMembership(context.Background(), tenancyadmin.MembershipGrantCommand{
			Actor: adminActor("tenant-a", "", "principal-admin"),
			Membership: tenancy.Membership{
				Scope:       tenantScope("tenant-a"),
				PrincipalID: "principal-b",
				Roles:       []tenancy.Role{tenancy.RoleViewer},
			},
			ExpectedVersion: expected,
			CallerKey:       "caller-grant",
			Content:         receipt.Content{MediaType: "application/json", Bytes: []byte(`{}`)},
		})
	}

	if _, err := grant(0); err != nil {
		t.Fatalf("first grant: %v", err)
	}
	if _, err := grant(1); err != nil {
		t.Fatalf("second grant: %v", err)
	}
	_, err := grant(1)
	requireFaultClass(t, err, fault.VersionConflict)
	if fake.commits != 2 {
		t.Fatalf("commits = %d; want 2 after the stale attempt", fake.commits)
	}
}

func containsRole(roles []tenancy.Role, want tenancy.Role) bool {
	return slices.Contains(roles, want)
}
