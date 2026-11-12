package pipeline_test

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func applyCommand(actor authorize.Principal, name string, yaml []byte) pipeline.ApplyCommand {
	return pipeline.ApplyCommand{
		Actor:     actor,
		Scope:     actor.Scope,
		Name:      name,
		YAML:      yaml,
		CallerKey: "caller-apply",
		Content:   receiptContent(`{"name":"` + name + `"}`),
	}
}

func updateCommand(actor authorize.Principal, pipelineID pipeline.PipelineID, name string, yaml []byte, expected uint64) pipeline.UpdateCommand {
	return pipeline.UpdateCommand{
		Actor:           actor,
		Scope:           actor.Scope,
		PipelineID:      pipelineID,
		Name:            name,
		YAML:            yaml,
		ExpectedVersion: expected,
		CallerKey:       "caller-update",
		Content:         receiptContent(`{"pipeline":"` + string(pipelineID) + `"}`),
	}
}

func deleteCommand(actor authorize.Principal, pipelineID pipeline.PipelineID, expected uint64) pipeline.DeleteCommand {
	return pipeline.DeleteCommand{
		Actor:           actor,
		Scope:           actor.Scope,
		PipelineID:      pipelineID,
		ExpectedVersion: expected,
		CallerKey:       "caller-delete",
		Content:         receiptContent(`{"pipeline":"` + string(pipelineID) + `"}`),
	}
}

func seedPipeline(store *fakeStore, scope tenancy.Scope, pipelineID pipeline.PipelineID, name string, version uint64) {
	store.defs[pipelineID] = pipeline.Definition{ID: pipelineID, Name: name, YAML: requestOnlyYAML(), Version: version}
	store.owners[pipelineID] = scope
	store.names[nameKey(scope, name)] = pipelineID
}

func TestApplyBuildsCompleteChangeUnit(t *testing.T) {
	reader := &fakeReader{byNameFn: func(context.Context, tenancy.Scope, string) (pipeline.Definition, error) {
		return pipeline.Definition{}, fault.New(fault.NotFound, "pipeline is not present")
	}}
	changes := &fakeChangePort{change: func(context.Context, pipeline.ChangeUnit) (pipeline.ChangeResult, error) {
		return pipeline.ChangeResult{PipelineID: "pipeline-1", Version: 1}, nil
	}}
	management, authorizer, authority := newManagement(t, changes, reader)
	actor := humanActor("tenant-a", "project-a", "principal-engineer", tenancy.RoleEngineer)
	yaml := validPipelineYAML()
	command := applyCommand(actor, "change", yaml)

	result, err := management.Apply(t.Context(), command)
	requireNoError(t, err, "Apply")
	if result.PipelineID != "pipeline-1" || result.Version != 1 || result.Replayed {
		t.Fatalf("result = %+v; want pipeline-1 version 1 not replayed", result)
	}
	if changes.callCount() != 1 {
		t.Fatalf("change port calls = %d; want 1", changes.callCount())
	}
	unit := capturedUnit(t, changes)
	if unit.Owner != actor.Scope {
		t.Fatalf("unit owner = %+v; want %+v", unit.Owner, actor.Scope)
	}
	if unit.Kind != pipeline.ChangeApply {
		t.Fatalf("unit kind = %q; want apply", unit.Kind)
	}
	if unit.PipelineID != "" || unit.Name != "change" || unit.ExpectedVersion != 0 {
		t.Fatalf("unit identity = %+v; want create by name", unit)
	}
	if string(unit.YAML) != string(yaml) {
		t.Fatalf("unit YAML = %q; want exact admitted bytes", unit.YAML)
	}
	requireReceipt(t, unit.Receipt, actor.Scope, actor.ID, "pipeline.apply", command.CallerKey, command.Content, testNow)
	requireAudit(t, unit.Audit, actor.Scope, actor.ID, "pipeline.apply", testNow)
	requireConfigEvent(t, unit.Events, actor.Scope, "change", 1, actor.ID, testNow)
	requirePublishPostCommit(t, unit.PostCommit)

	membership := requireAuthority(t, unit.Authority, "pipeline.apply", authorize.CheckMembership)
	if membership.Scope != actor.Scope || membership.PrincipalID != actor.ID {
		t.Fatalf("membership read = %+v", membership)
	}
	version := requireAuthority(t, unit.Authority, "pipeline.apply", authorize.CheckPipelineVersion)
	if version.TargetID != "" || version.ExpectedVersion != 0 {
		t.Fatalf("pipeline version read = %+v; want create at version 0", version)
	}
	requireNoAuthority(t, unit.Authority, authorize.CheckPushServiceAdmin)

	calls := authorizer.captured()
	if len(calls) != 1 || calls[0].Operation != "pipeline.apply" || calls[0].Scope != actor.Scope {
		t.Fatalf("authorizer calls = %+v", calls)
	}
	if authority.callCount() != 1 {
		t.Fatalf("authority reader calls = %d; want 1", authority.callCount())
	}
	if get, byName, _ := reader.counts(); get != 0 || byName != 1 {
		t.Fatalf("reader calls get/byName = %d/%d; want 0/1", get, byName)
	}

	// The commit unit owns a copy of the caller's bytes.
	yaml[0] = 'X'
	if string(unit.YAML)[0] == 'X' {
		t.Fatal("change unit YAML changed through the caller's slice")
	}
}

func TestApplyExistingNameUsesCurrentVersion(t *testing.T) {
	current := pipeline.Definition{ID: "pipeline-1", Name: "change", YAML: requestOnlyYAML(), Version: 2}
	reader := &fakeReader{byNameFn: func(context.Context, tenancy.Scope, string) (pipeline.Definition, error) {
		return cloneDefinition(current), nil
	}}
	changes := &fakeChangePort{change: func(context.Context, pipeline.ChangeUnit) (pipeline.ChangeResult, error) {
		return pipeline.ChangeResult{PipelineID: "pipeline-1", Version: 3}, nil
	}}
	management, _, _ := newManagement(t, changes, reader)
	actor := humanActor("tenant-a", "project-a", "principal-engineer", tenancy.RoleEngineer)

	result, err := management.Apply(t.Context(), applyCommand(actor, "change", typedInputsYAML()))
	requireNoError(t, err, "Apply existing name")
	if result.PipelineID != "pipeline-1" || result.Version != 3 {
		t.Fatalf("result = %+v", result)
	}
	unit := capturedUnit(t, changes)
	if unit.Kind != pipeline.ChangeApply || unit.PipelineID != "pipeline-1" || unit.ExpectedVersion != 2 {
		t.Fatalf("unit = %+v; want apply pipeline-1 expected version 2", unit)
	}
	requireConfigEvent(t, unit.Events, actor.Scope, "pipeline-1", 3, actor.ID, testNow)
}

func TestApplyRefusesInvalidAuthoringBeforePorts(t *testing.T) {
	reader := &fakeReader{}
	changes := &fakeChangePort{}
	management, authorizer, _ := newManagement(t, changes, reader)
	actor := humanActor("tenant-a", "project-a", "principal-engineer", tenancy.RoleEngineer)

	duplicate := []byte(`actions:
  - name: build
    image: registry.example.com/build:latest
actions:
  - name: test
    image: registry.example.com/test:latest
`)
	_, err := management.Apply(t.Context(), applyCommand(actor, "change", duplicate))
	requireFaultClass(t, err, fault.InvalidInput)
	if reader.byName != 0 || changes.callCount() != 0 {
		t.Fatalf("ports called after invalid YAML: byName=%d changes=%d", reader.byName, changes.callCount())
	}

	_, err = management.Apply(t.Context(), applyCommand(actor, "", validPipelineYAML()))
	requireFaultClass(t, err, fault.InvalidInput)
	_, err = management.Apply(t.Context(), applyCommand(actor, "bad/name", validPipelineYAML()))
	requireFaultClass(t, err, fault.InvalidInput)
	if changes.callCount() != 0 {
		t.Fatalf("change port calls = %d; want 0", changes.callCount())
	}
	if authorizer.callCount() == 0 {
		t.Fatal("authorizer was not consulted for invalid authoring")
	}
}

func TestUpdateBuildsCompleteChangeUnit(t *testing.T) {
	current := pipeline.Definition{ID: "pipeline-1", Name: "old", YAML: requestOnlyYAML(), Version: 2}
	reader := &fakeReader{getFn: func(context.Context, tenancy.Scope, pipeline.PipelineID) (pipeline.Definition, error) {
		return cloneDefinition(current), nil
	}}
	changes := &fakeChangePort{change: func(context.Context, pipeline.ChangeUnit) (pipeline.ChangeResult, error) {
		return pipeline.ChangeResult{PipelineID: "pipeline-1", Version: 3}, nil
	}}
	management, _, _ := newManagement(t, changes, reader)
	actor := humanActor("tenant-a", "project-a", "principal-engineer", tenancy.RoleEngineer)
	yaml := typedInputsYAML()

	result, err := management.Update(t.Context(), updateCommand(actor, "pipeline-1", "new", yaml, 2))
	requireNoError(t, err, "Update")
	if result.PipelineID != "pipeline-1" || result.Version != 3 {
		t.Fatalf("result = %+v", result)
	}
	unit := capturedUnit(t, changes)
	if unit.Kind != pipeline.ChangeUpdate || unit.PipelineID != "pipeline-1" || unit.Name != "new" || unit.ExpectedVersion != 2 {
		t.Fatalf("unit = %+v; want update pipeline-1 version 2", unit)
	}
	if string(unit.YAML) != string(yaml) {
		t.Fatalf("unit YAML = %q", unit.YAML)
	}
	requireReceipt(t, unit.Receipt, actor.Scope, actor.ID, "pipeline.update", "caller-update", receiptContent(`{"pipeline":"pipeline-1"}`), testNow)
	requireAudit(t, unit.Audit, actor.Scope, actor.ID, "pipeline.update", testNow)
	requireConfigEvent(t, unit.Events, actor.Scope, "pipeline-1", 3, actor.ID, testNow)
	requirePublishPostCommit(t, unit.PostCommit)
	version := requireAuthority(t, unit.Authority, "pipeline.update", authorize.CheckPipelineVersion)
	if version.TargetID != "pipeline-1" || version.ExpectedVersion != 2 {
		t.Fatalf("pipeline version read = %+v", version)
	}
	requireNoAuthority(t, unit.Authority, authorize.CheckPushServiceAdmin)
}

func TestDeleteBuildsCompleteChangeUnit(t *testing.T) {
	current := pipeline.Definition{ID: "pipeline-1", Name: "change", YAML: requestOnlyYAML(), Version: 2}
	reader := &fakeReader{getFn: func(context.Context, tenancy.Scope, pipeline.PipelineID) (pipeline.Definition, error) {
		return cloneDefinition(current), nil
	}}
	changes := &fakeChangePort{change: func(context.Context, pipeline.ChangeUnit) (pipeline.ChangeResult, error) {
		return pipeline.ChangeResult{PipelineID: "pipeline-1", Version: 3}, nil
	}}
	management, _, _ := newManagement(t, changes, reader)
	actor := humanActor("tenant-a", "project-a", "principal-engineer", tenancy.RoleEngineer)

	result, err := management.Delete(t.Context(), deleteCommand(actor, "pipeline-1", 2))
	requireNoError(t, err, "Delete")
	if result.PipelineID != "pipeline-1" || result.Version != 3 {
		t.Fatalf("result = %+v", result)
	}
	unit := capturedUnit(t, changes)
	if unit.Kind != pipeline.ChangeDelete || unit.PipelineID != "pipeline-1" || unit.Name != "change" || unit.ExpectedVersion != 2 {
		t.Fatalf("unit = %+v; want delete pipeline-1 version 2", unit)
	}
	if len(unit.YAML) != 0 {
		t.Fatalf("delete unit YAML = %q; want empty", unit.YAML)
	}
	requireReceipt(t, unit.Receipt, actor.Scope, actor.ID, "pipeline.delete", "caller-delete", receiptContent(`{"pipeline":"pipeline-1"}`), testNow)
	requireAudit(t, unit.Audit, actor.Scope, actor.ID, "pipeline.delete", testNow)
	requireConfigEvent(t, unit.Events, actor.Scope, "pipeline-1", 3, actor.ID, testNow)
	requirePublishPostCommit(t, unit.PostCommit)
	version := requireAuthority(t, unit.Authority, "pipeline.delete", authorize.CheckPipelineVersion)
	if version.TargetID != "pipeline-1" || version.ExpectedVersion != 2 {
		t.Fatalf("pipeline version read = %+v", version)
	}
}

func TestManagementReadAndList(t *testing.T) {
	current := pipeline.Definition{ID: "pipeline-1", Name: "change", YAML: requestOnlyYAML(), Version: 2}
	reader := &fakeReader{
		getFn: func(context.Context, tenancy.Scope, pipeline.PipelineID) (pipeline.Definition, error) {
			return cloneDefinition(current), nil
		},
		listFn: func(context.Context, tenancy.Scope) ([]pipeline.Definition, error) {
			return []pipeline.Definition{cloneDefinition(current)}, nil
		},
	}
	management, authorizer, authority := newManagement(t, &fakeChangePort{}, reader)
	actor := humanActor("tenant-a", "project-a", "principal-engineer", tenancy.RoleEngineer)

	got, err := management.Read(t.Context(), pipeline.ReadCommand{Actor: actor, Scope: actor.Scope, PipelineID: "pipeline-1"})
	requireNoError(t, err, "Read")
	requireDefinition(t, got, current)
	calls := authorizer.captured()
	if len(calls) != 1 || calls[0].Operation != "pipeline.read" {
		t.Fatalf("authorizer calls = %+v; want pipeline.read", calls)
	}
	if authority.callCount() != 1 {
		t.Fatalf("authority reader calls = %d; want 1", authority.callCount())
	}
	reads := authority.captured()[0]
	requireAuthority(t, reads, "pipeline.read", authorize.CheckMembership)

	list, err := management.List(t.Context(), pipeline.ListCommand{Actor: actor, Scope: actor.Scope})
	requireNoError(t, err, "List")
	if len(list) != 1 {
		t.Fatalf("list = %v; want one definition", list)
	}
	requireDefinition(t, list[0], current)

	admin := humanActor("tenant-a", "project-a", "principal-admin", tenancy.RoleAdmin)
	before := reader.getCalls
	_, err = management.Read(t.Context(), pipeline.ReadCommand{Actor: admin, Scope: admin.Scope, PipelineID: "pipeline-1"})
	requireFaultClass(t, err, fault.Forbidden)
	if reader.getCalls != before {
		t.Fatalf("Reader.Get called for a denied principal")
	}
}

func TestManagementScopeIsolation(t *testing.T) {
	reader := &fakeReader{}
	changes := &fakeChangePort{}
	management, authorizer, _ := newManagement(t, changes, reader)
	actor := humanActor("tenant-a", "project-a", "principal-engineer", tenancy.RoleEngineer)
	command := applyCommand(actor, "change", validPipelineYAML())
	command.Scope = projectScope("tenant-a", "project-b")

	_, err := management.Apply(t.Context(), command)
	requireFaultClass(t, err, fault.Forbidden)
	if authorizer.callCount() != 0 || reader.byName != 0 || changes.callCount() != 0 {
		t.Fatalf("ports called for out-of-scope mutation: auth=%d reader=%d changes=%d", authorizer.callCount(), reader.byName, changes.callCount())
	}
}

func TestManagementAuthorizationMatrix(t *testing.T) {
	type testCase struct {
		name      string
		operation authorize.OperationName
		kind      identity.PrincipalKind
		roles     []tenancy.Role
		allowed   bool
	}
	cases := []testCase{
		{"apply engineer", "pipeline.apply", identity.KindHuman, []tenancy.Role{tenancy.RoleEngineer}, true},
		{"apply admin", "pipeline.apply", identity.KindHuman, []tenancy.Role{tenancy.RoleAdmin}, false},
		{"apply reviewer", "pipeline.apply", identity.KindHuman, []tenancy.Role{tenancy.RoleReviewer}, false},
		{"apply viewer", "pipeline.apply", identity.KindHuman, []tenancy.Role{tenancy.RoleViewer}, false},
		{"apply service engineer", "pipeline.apply", identity.KindService, []tenancy.Role{tenancy.RoleEngineer}, true},
		{"apply service viewer", "pipeline.apply", identity.KindService, []tenancy.Role{tenancy.RoleViewer}, false},
		{"update engineer", "pipeline.update", identity.KindHuman, []tenancy.Role{tenancy.RoleEngineer}, true},
		{"update viewer", "pipeline.update", identity.KindHuman, []tenancy.Role{tenancy.RoleViewer}, false},
		{"delete engineer", "pipeline.delete", identity.KindHuman, []tenancy.Role{tenancy.RoleEngineer}, true},
		{"delete reviewer", "pipeline.delete", identity.KindHuman, []tenancy.Role{tenancy.RoleReviewer}, false},
		{"read engineer", "pipeline.read", identity.KindHuman, []tenancy.Role{tenancy.RoleEngineer}, true},
		{"read viewer", "pipeline.read", identity.KindHuman, []tenancy.Role{tenancy.RoleViewer}, true},
		{"read admin", "pipeline.read", identity.KindHuman, []tenancy.Role{tenancy.RoleAdmin}, false},
		{"read reviewer", "pipeline.read", identity.KindHuman, []tenancy.Role{tenancy.RoleReviewer}, false},
		{"read service viewer", "pipeline.read", identity.KindService, []tenancy.Role{tenancy.RoleViewer}, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			current := pipeline.Definition{ID: "pipeline-1", Name: "change", YAML: requestOnlyYAML(), Version: 2}
			reader := &fakeReader{
				byNameFn: func(context.Context, tenancy.Scope, string) (pipeline.Definition, error) {
					return pipeline.Definition{}, fault.New(fault.NotFound, "pipeline is not present")
				},
				getFn: func(context.Context, tenancy.Scope, pipeline.PipelineID) (pipeline.Definition, error) {
					return cloneDefinition(current), nil
				},
			}
			changes := &fakeChangePort{}
			management, _, _ := newManagement(t, changes, reader)
			var actor authorize.Principal
			if testCase.kind == identity.KindHuman {
				actor = humanActor("tenant-a", "project-a", "principal-actor", testCase.roles...)
			} else {
				actor = serviceActor("tenant-a", "project-a", "principal-actor", testCase.roles...)
			}

			var err error
			switch testCase.operation {
			case "pipeline.apply":
				_, err = management.Apply(t.Context(), applyCommand(actor, "change", validPipelineYAML()))
			case "pipeline.update":
				_, err = management.Update(t.Context(), updateCommand(actor, "pipeline-1", "change", typedInputsYAML(), 2))
			case "pipeline.delete":
				_, err = management.Delete(t.Context(), deleteCommand(actor, "pipeline-1", 2))
			case "pipeline.read":
				_, err = management.Read(t.Context(), pipeline.ReadCommand{Actor: actor, Scope: actor.Scope, PipelineID: "pipeline-1"})
			default:
				t.Fatalf("unhandled operation %q", testCase.operation)
			}

			if testCase.allowed {
				if err != nil {
					t.Fatalf("%s should be allowed: %v", testCase.name, err)
				}
				return
			}
			requireFaultClass(t, err, fault.Forbidden)
			if changes.callCount() != 0 {
				t.Fatalf("%s reached the change port %d times", testCase.name, changes.callCount())
			}
		})
	}
}

func TestManagementRefusesDeniedAuthorizerBeforeReads(t *testing.T) {
	reader := &fakeReader{}
	changes := &fakeChangePort{}
	authority := &fakeAuthorityReader{}
	authorizer := &fakeAuthorizer{authorize: func(context.Context, authorize.Principal, authorize.OperationName, tenancy.Scope) (authorize.Decision, error) {
		return authorize.DecisionDeny, nil
	}}
	management, err := pipeline.NewManagement(pipeline.ManagementConfig{
		Clock:      fixedClock{now: testNow},
		IDs:        &sequentialIDs{},
		Authorizer: authorizer,
		Authority:  authority,
		Changes:    changes,
		Reader:     reader,
	})
	if err != nil {
		t.Fatalf("NewManagement: %v", err)
	}
	actor := humanActor("tenant-a", "project-a", "principal-engineer", tenancy.RoleEngineer)
	_, err = management.Apply(t.Context(), applyCommand(actor, "change", validPipelineYAML()))
	requireFaultClass(t, err, fault.Forbidden)
	if reader.byName != 0 || authority.callCount() != 0 || changes.callCount() != 0 {
		t.Fatalf("denied authorizer still reached ports: reader=%d authority=%d changes=%d", reader.byName, authority.callCount(), changes.callCount())
	}
}

func TestManagementRefusesCurrentAuthorityFailureBeforeCommit(t *testing.T) {
	current := pipeline.Definition{ID: "pipeline-1", Name: "change", YAML: requestOnlyYAML(), Version: 2}
	reader := &fakeReader{getFn: func(context.Context, tenancy.Scope, pipeline.PipelineID) (pipeline.Definition, error) {
		return cloneDefinition(current), nil
	}}
	changes := &fakeChangePort{}
	actor := humanActor("tenant-a", "project-a", "principal-engineer", tenancy.RoleEngineer)

	denied := &fakeAuthorityReader{read: func(_ context.Context, reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
		facts := allowedFacts(reads)
		facts[0].Allowed = false
		return facts, nil
	}}
	management, err := pipeline.NewManagement(pipeline.ManagementConfig{
		Clock: fixedClock{now: testNow}, IDs: &sequentialIDs{}, Authorizer: &fakeAuthorizer{},
		Authority: denied, Changes: changes, Reader: reader,
	})
	if err != nil {
		t.Fatalf("NewManagement: %v", err)
	}
	_, err = management.Update(t.Context(), updateCommand(actor, "pipeline-1", "change", typedInputsYAML(), 2))
	requireFaultClass(t, err, fault.Forbidden)
	if changes.callCount() != 0 {
		t.Fatalf("change port called after denied current authority")
	}
}

// TestManagementStaleVersionIsEnforcedByTheChangePort pins that the manager
// does not confirm the pipeline version before the receipt-first change port:
// the port refuses a genuinely stale version atomically and commits nothing.
func TestManagementStaleVersionIsEnforcedByTheChangePort(t *testing.T) {
	store := newFakeStore()
	scope := projectScope("tenant-a", "project-a")
	seedPipeline(store, scope, "pipeline-1", "change", 2)
	authority := newVersionAwareAuthority(store)
	management := newManagementWithAuthority(t, store, store, authority)
	actor := humanActor("tenant-a", "project-a", "principal-engineer", tenancy.RoleEngineer)

	_, err := management.Update(t.Context(), updateCommand(actor, "pipeline-1", "change", typedInputsYAML(), 1))
	requireFaultClass(t, err, fault.VersionConflict)
	if _, commits, _ := store.counts(); commits != 0 {
		t.Fatalf("stale update committed %d times", commits)
	}
	requireNoAuthority(t, authority.lastReads(), authorize.CheckPipelineVersion)

	_, err = management.Delete(t.Context(), deleteCommand(actor, "pipeline-1", 1))
	requireFaultClass(t, err, fault.VersionConflict)
	if _, commits, _ := store.counts(); commits != 0 {
		t.Fatalf("stale delete committed %d times", commits)
	}
	requireNoAuthority(t, authority.lastReads(), authorize.CheckPipelineVersion)
}

func TestManagementPropagatesStaleVersionConflict(t *testing.T) {
	store := newFakeStore()
	store.defs["pipeline-1"] = pipeline.Definition{ID: "pipeline-1", Name: "change", YAML: requestOnlyYAML(), Version: 2}
	store.owners["pipeline-1"] = projectScope("tenant-a", "project-a")
	store.names[nameKey(projectScope("tenant-a", "project-a"), "change")] = "pipeline-1"
	management, _, _ := newManagement(t, store, store)
	actor := humanActor("tenant-a", "project-a", "principal-engineer", tenancy.RoleEngineer)

	_, err := management.Update(t.Context(), updateCommand(actor, "pipeline-1", "change", typedInputsYAML(), 1))
	requireFaultClass(t, err, fault.VersionConflict)
	if _, commits, _ := store.counts(); commits != 0 {
		t.Fatalf("stale update committed %d times", commits)
	}

	_, err = management.Delete(t.Context(), deleteCommand(actor, "pipeline-1", 1))
	requireFaultClass(t, err, fault.VersionConflict)
	if _, commits, _ := store.counts(); commits != 0 {
		t.Fatalf("stale delete committed %d times", commits)
	}
}

func TestManagementReceiptReplayAndChangedContent(t *testing.T) {
	store := newFakeStore()
	management, _, _ := newManagement(t, store, store)
	actor := humanActor("tenant-a", "project-a", "principal-engineer", tenancy.RoleEngineer)
	command := applyCommand(actor, "change", validPipelineYAML())

	first, err := management.Apply(t.Context(), command)
	requireNoError(t, err, "first Apply")
	if first.Replayed {
		t.Fatal("first commit reported a replay")
	}
	second, err := management.Apply(t.Context(), command)
	requireNoError(t, err, "replayed Apply")
	if !second.Replayed {
		t.Fatal("identical retry did not report a replay")
	}
	if second.PipelineID != first.PipelineID || second.Version != first.Version {
		t.Fatalf("replay = %+v; want original %+v", second, first)
	}
	if _, commits, replays := store.counts(); commits != 1 || replays != 1 {
		t.Fatalf("commits/replays = %d/%d; want 1/1", commits, replays)
	}

	changed := command
	changed.Content = receiptContent(`{"name":"changed"}`)
	_, err = management.Apply(t.Context(), changed)
	requireFaultClass(t, err, fault.ChangedIdempotencyContent)
	if _, commits, replays := store.counts(); commits != 1 || replays != 1 {
		t.Fatalf("commits/replays after changed content = %d/%d; want 1/1", commits, replays)
	}
}

func TestManagementLostResponseRetryReturnsOriginal(t *testing.T) {
	store := newFakeStore()
	store.loseFirst = true
	management, _, _ := newManagement(t, store, store)
	actor := humanActor("tenant-a", "project-a", "principal-engineer", tenancy.RoleEngineer)
	command := applyCommand(actor, "change", validPipelineYAML())

	_, err := management.Apply(t.Context(), command)
	requireFaultClass(t, err, fault.DependencyUnavailable)
	if _, commits, _ := store.counts(); commits != 1 {
		t.Fatalf("commits after lost response = %d; want 1", commits)
	}

	replayed, err := management.Apply(t.Context(), command)
	requireNoError(t, err, "retry after lost response")
	if !replayed.Replayed {
		t.Fatal("retry did not converge on the original result")
	}
	if _, commits, replays := store.counts(); commits != 1 || replays != 1 {
		t.Fatalf("commits/replays = %d/%d; want 1/1", commits, replays)
	}
}

// TestManagementLostResponseUpdateRetryConverges pins S04 §3: a committed update
// whose response was lost must replay the original result on an equal-key retry
// even though the retry still carries the pre-commit expected version.
func TestManagementLostResponseUpdateRetryConverges(t *testing.T) {
	store := newFakeStore()
	scope := projectScope("tenant-a", "project-a")
	seedPipeline(store, scope, "pipeline-1", "change", 1)
	store.loseFirst = true
	authority := newVersionAwareAuthority(store)
	management := newManagementWithAuthority(t, store, store, authority)
	actor := humanActor("tenant-a", "project-a", "principal-engineer", tenancy.RoleEngineer)
	command := updateCommand(actor, "pipeline-1", "change", typedInputsYAML(), 1)

	_, err := management.Update(t.Context(), command)
	requireFaultClass(t, err, fault.DependencyUnavailable)
	if _, commits, _ := store.counts(); commits != 1 {
		t.Fatalf("commits after lost response = %d; want 1", commits)
	}

	replayed, err := management.Update(t.Context(), command)
	requireNoError(t, err, "retry after lost response")
	if !replayed.Replayed {
		t.Fatal("equal-key update retry did not converge on the committed result")
	}
	if replayed.PipelineID != "pipeline-1" || replayed.Version != 2 {
		t.Fatalf("replay = %+v; want pipeline-1 version 2", replayed)
	}
	if _, commits, replays := store.counts(); commits != 1 || replays != 1 {
		t.Fatalf("commits/replays = %d/%d; want 1/1", commits, replays)
	}
	// The pre-commit check re-reads current membership but leaves the pipeline
	// version to the receipt-first change port.
	requireAuthority(t, authority.lastReads(), "pipeline.update", authorize.CheckMembership)
	requireNoAuthority(t, authority.lastReads(), authorize.CheckPipelineVersion)
}

// TestManagementLostResponseDeleteRetryConverges pins S04 §3 for deletes: the
// row is already gone, so the reader returns not_found and the receipt-first
// change port must still replay the committed deletion result.
func TestManagementLostResponseDeleteRetryConverges(t *testing.T) {
	store := newFakeStore()
	scope := projectScope("tenant-a", "project-a")
	seedPipeline(store, scope, "pipeline-1", "change", 1)
	store.loseFirst = true
	authority := newVersionAwareAuthority(store)
	management := newManagementWithAuthority(t, store, store, authority)
	actor := humanActor("tenant-a", "project-a", "principal-engineer", tenancy.RoleEngineer)
	command := deleteCommand(actor, "pipeline-1", 1)

	_, err := management.Delete(t.Context(), command)
	requireFaultClass(t, err, fault.DependencyUnavailable)
	if _, commits, _ := store.counts(); commits != 1 {
		t.Fatalf("commits after lost response = %d; want 1", commits)
	}
	if _, err := store.Get(t.Context(), scope, "pipeline-1"); err == nil {
		t.Fatal("deleted pipeline is still readable")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
		t.Fatalf("deleted pipeline read class = %v; want not_found", class)
	}

	replayed, err := management.Delete(t.Context(), command)
	requireNoError(t, err, "retry after lost response")
	if !replayed.Replayed {
		t.Fatal("equal-key delete retry did not converge on the committed result")
	}
	if replayed.PipelineID != "pipeline-1" || replayed.Version != 2 {
		t.Fatalf("replay = %+v; want pipeline-1 version 2", replayed)
	}
	if _, commits, replays := store.counts(); commits != 1 || replays != 1 {
		t.Fatalf("commits/replays = %d/%d; want 1/1", commits, replays)
	}
	requireAuthority(t, authority.lastReads(), "pipeline.delete", authorize.CheckMembership)
	requireNoAuthority(t, authority.lastReads(), authorize.CheckPipelineVersion)
}

// TestManagementLostResponseRetryChangedContentIsRefused pins that convergence
// never hides an idempotency conflict: the same caller key with different
// request content is still refused for both updates and deletes.
func TestManagementLostResponseRetryChangedContentIsRefused(t *testing.T) {
	actor := humanActor("tenant-a", "project-a", "principal-engineer", tenancy.RoleEngineer)
	scope := projectScope("tenant-a", "project-a")

	t.Run("update", func(t *testing.T) {
		store := newFakeStore()
		seedPipeline(store, scope, "pipeline-1", "change", 1)
		store.loseFirst = true
		management := newManagementWithAuthority(t, store, store, newVersionAwareAuthority(store))
		command := updateCommand(actor, "pipeline-1", "change", typedInputsYAML(), 1)
		_, err := management.Update(t.Context(), command)
		requireFaultClass(t, err, fault.DependencyUnavailable)

		changed := command
		changed.Content = receiptContent(`{"pipeline":"changed"}`)
		_, err = management.Update(t.Context(), changed)
		requireFaultClass(t, err, fault.ChangedIdempotencyContent)
		if _, commits, replays := store.counts(); commits != 1 || replays != 0 {
			t.Fatalf("commits/replays = %d/%d; want 1/0", commits, replays)
		}
	})

	t.Run("delete", func(t *testing.T) {
		store := newFakeStore()
		seedPipeline(store, scope, "pipeline-1", "change", 1)
		store.loseFirst = true
		management := newManagementWithAuthority(t, store, store, newVersionAwareAuthority(store))
		command := deleteCommand(actor, "pipeline-1", 1)
		_, err := management.Delete(t.Context(), command)
		requireFaultClass(t, err, fault.DependencyUnavailable)

		changed := command
		changed.Content = receiptContent(`{"pipeline":"changed"}`)
		_, err = management.Delete(t.Context(), changed)
		requireFaultClass(t, err, fault.ChangedIdempotencyContent)
		if _, commits, replays := store.counts(); commits != 1 || replays != 0 {
			t.Fatalf("commits/replays = %d/%d; want 1/0", commits, replays)
		}
	})
}

// TestManagementLostResponseRetryRequiresCurrentMembership pins that a replay
// is exposed only to a principal whose membership still holds.
func TestManagementLostResponseRetryRequiresCurrentMembership(t *testing.T) {
	actor := humanActor("tenant-a", "project-a", "principal-engineer", tenancy.RoleEngineer)
	scope := projectScope("tenant-a", "project-a")

	t.Run("update", func(t *testing.T) {
		store := newFakeStore()
		seedPipeline(store, scope, "pipeline-1", "change", 1)
		store.loseFirst = true
		authority := newVersionAwareAuthority(store)
		management := newManagementWithAuthority(t, store, store, authority)
		command := updateCommand(actor, "pipeline-1", "change", typedInputsYAML(), 1)
		_, err := management.Update(t.Context(), command)
		requireFaultClass(t, err, fault.DependencyUnavailable)

		authority.revokeMembership(actor.ID)
		_, err = management.Update(t.Context(), command)
		requireFaultClass(t, err, fault.Forbidden)
		if _, commits, replays := store.counts(); commits != 1 || replays != 0 {
			t.Fatalf("commits/replays = %d/%d; want 1/0", commits, replays)
		}
	})

	t.Run("delete", func(t *testing.T) {
		store := newFakeStore()
		seedPipeline(store, scope, "pipeline-1", "change", 1)
		store.loseFirst = true
		authority := newVersionAwareAuthority(store)
		management := newManagementWithAuthority(t, store, store, authority)
		command := deleteCommand(actor, "pipeline-1", 1)
		_, err := management.Delete(t.Context(), command)
		requireFaultClass(t, err, fault.DependencyUnavailable)

		authority.revokeMembership(actor.ID)
		_, err = management.Delete(t.Context(), command)
		requireFaultClass(t, err, fault.Forbidden)
		if _, commits, replays := store.counts(); commits != 1 || replays != 0 {
			t.Fatalf("commits/replays = %d/%d; want 1/0", commits, replays)
		}
	})
}

func TestManagementCrashBeforeEffectThenRetryCommitsOnce(t *testing.T) {
	store := newFakeStore()
	store.failFirst = true
	management, _, _ := newManagement(t, store, store)
	actor := humanActor("tenant-a", "project-a", "principal-engineer", tenancy.RoleEngineer)
	command := applyCommand(actor, "change", validPipelineYAML())

	_, err := management.Apply(t.Context(), command)
	requireFaultClass(t, err, fault.DependencyUnavailable)
	if _, commits, _ := store.counts(); commits != 0 {
		t.Fatalf("commits after failed attempt = %d; want 0", commits)
	}

	_, err = management.Apply(t.Context(), command)
	requireNoError(t, err, "retry")
	if _, commits, _ := store.counts(); commits != 1 {
		t.Fatalf("commits after retry = %d; want 1", commits)
	}
}

func TestManagementConcurrentApplySameNameOneWins(t *testing.T) {
	store := newFakeStore()
	store.barrier = newBarrier(2)
	management, _, _ := newManagement(t, store, store)
	actor := humanActor("tenant-a", "project-a", "principal-engineer", tenancy.RoleEngineer)

	results := make(chan error, 2)
	var wait sync.WaitGroup
	for index := range 2 {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			command := applyCommand(actor, "change", validPipelineYAML())
			command.CallerKey = "caller-" + string(rune('a'+index))
			command.Content = receiptContent(`{"caller":"` + string(rune('a'+index)) + `"}`)
			_, err := management.Apply(t.Context(), command)
			results <- err
		}(index)
	}
	wait.Wait()
	close(results)

	var allowed, conflicts int
	for err := range results {
		if err == nil {
			allowed++
			continue
		}
		if class, ok := fault.ClassOf(err); ok && class == fault.VersionConflict {
			conflicts++
			continue
		}
		t.Fatalf("unexpected concurrent apply error: %v", err)
	}
	if allowed != 1 || conflicts != 1 {
		t.Fatalf("allowed/conflicts = %d/%d; want 1/1", allowed, conflicts)
	}
	if _, commits, _ := store.counts(); commits != 1 {
		t.Fatalf("commits = %d; want 1", commits)
	}
}

func TestManagementConcurrentUpdateSameVersionOneWins(t *testing.T) {
	store := newFakeStore()
	scope := projectScope("tenant-a", "project-a")
	store.defs["pipeline-1"] = pipeline.Definition{ID: "pipeline-1", Name: "change", YAML: requestOnlyYAML(), Version: 1}
	store.owners["pipeline-1"] = scope
	store.names[nameKey(scope, "change")] = "pipeline-1"
	store.barrier = newBarrier(2)
	management, _, _ := newManagement(t, store, store)
	actor := humanActor("tenant-a", "project-a", "principal-engineer", tenancy.RoleEngineer)

	results := make(chan error, 2)
	var wait sync.WaitGroup
	for index := range 2 {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			command := updateCommand(actor, "pipeline-1", "change", typedInputsYAML(), 1)
			command.CallerKey = "caller-" + string(rune('a'+index))
			command.Content = receiptContent(`{"caller":"` + string(rune('a'+index)) + `"}`)
			_, err := management.Update(t.Context(), command)
			results <- err
		}(index)
	}
	wait.Wait()
	close(results)

	var allowed, conflicts int
	for err := range results {
		if err == nil {
			allowed++
			continue
		}
		if class, ok := fault.ClassOf(err); ok && class == fault.VersionConflict {
			conflicts++
			continue
		}
		t.Fatalf("unexpected concurrent update error: %v", err)
	}
	if allowed != 1 || conflicts != 1 {
		t.Fatalf("allowed/conflicts = %d/%d; want 1/1", allowed, conflicts)
	}
	if _, commits, _ := store.counts(); commits != 1 {
		t.Fatalf("commits = %d; want 1", commits)
	}
}

func TestManagementPushAuthorityIsConditional(t *testing.T) {
	actor := humanActor("tenant-a", "project-a", "principal-engineer", tenancy.RoleEngineer)
	push := &pipeline.PushTrigger{
		Repository: "repo-1",
		Service:    "git-service",
		Refs:       []string{"refs/heads/main"},
	}
	noPush := requestOnlyYAML()
	pushYAML := pushPipelineYAML()

	cases := []struct {
		name     string
		call     func(*pipeline.Management) error
		wantPush bool
	}{
		{
			name: "apply create with push",
			call: func(management *pipeline.Management) error {
				_, err := management.Apply(t.Context(), applyCommand(actor, "push", pushYAML))
				return err
			},
			wantPush: true,
		},
		{
			name: "apply create without push",
			call: func(management *pipeline.Management) error {
				_, err := management.Apply(t.Context(), applyCommand(actor, "change", noPush))
				return err
			},
			wantPush: false,
		},
		{
			name: "update removes push",
			call: func(management *pipeline.Management) error {
				_, err := management.Update(t.Context(), updateCommand(actor, "pipeline-1", "change", noPush, 2))
				return err
			},
			wantPush: true,
		},
		{
			name: "update keeps no push",
			call: func(management *pipeline.Management) error {
				_, err := management.Update(t.Context(), updateCommand(actor, "pipeline-1", "change", typedInputsYAML(), 2))
				return err
			},
			wantPush: false,
		},
		{
			name: "delete removes push",
			call: func(management *pipeline.Management) error {
				_, err := management.Delete(t.Context(), deleteCommand(actor, "pipeline-1", 2))
				return err
			},
			wantPush: true,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			current := pipeline.Definition{ID: "pipeline-1", Name: "change", YAML: requestOnlyYAML(), Version: 2}
			if testCase.name == "update removes push" || testCase.name == "delete removes push" {
				current.Push = push
			}
			reader := &fakeReader{
				byNameFn: func(context.Context, tenancy.Scope, string) (pipeline.Definition, error) {
					return pipeline.Definition{}, fault.New(fault.NotFound, "pipeline is not present")
				},
				getFn: func(context.Context, tenancy.Scope, pipeline.PipelineID) (pipeline.Definition, error) {
					return cloneDefinition(current), nil
				},
			}
			changes := &fakeChangePort{}
			management, _, _ := newManagement(t, changes, reader)
			if err := testCase.call(management); err != nil {
				t.Fatalf("%s: %v", testCase.name, err)
			}
			unit := capturedUnit(t, changes)
			if testCase.wantPush {
				read := requireAuthority(t, unit.Authority, operationForKind(unit.Kind), authorize.CheckPushServiceAdmin)
				if read.Scope != actor.Scope || read.PrincipalID != actor.ID || read.TargetID != "" || read.ExpectedVersion != 0 {
					t.Fatalf("push_service_admin read = %+v", read)
				}
			} else {
				requireNoAuthority(t, unit.Authority, authorize.CheckPushServiceAdmin)
			}
			requireAuthority(t, unit.Authority, operationForKind(unit.Kind), authorize.CheckMembership)
			requireAuthority(t, unit.Authority, operationForKind(unit.Kind), authorize.CheckPipelineVersion)
		})
	}
}

func operationForKind(kind pipeline.ChangeKind) authorize.OperationName {
	switch kind {
	case pipeline.ChangeApply:
		return "pipeline.apply"
	case pipeline.ChangeUpdate:
		return "pipeline.update"
	case pipeline.ChangeDelete:
		return "pipeline.delete"
	default:
		return ""
	}
}

func TestManagementNewRequiresBoundaries(t *testing.T) {
	base := pipeline.ManagementConfig{
		Clock:      fixedClock{now: testNow},
		IDs:        &sequentialIDs{},
		Authorizer: &fakeAuthorizer{},
		Authority:  &fakeAuthorityReader{},
		Changes:    &fakeChangePort{},
		Reader:     &fakeReader{},
	}
	if _, err := pipeline.NewManagement(base); err != nil {
		t.Fatalf("NewManagement with complete config: %v", err)
	}
	cases := map[string]func(*pipeline.ManagementConfig){
		"clock":      func(config *pipeline.ManagementConfig) { config.Clock = nil },
		"ids":        func(config *pipeline.ManagementConfig) { config.IDs = nil },
		"authorizer": func(config *pipeline.ManagementConfig) { config.Authorizer = nil },
		"authority":  func(config *pipeline.ManagementConfig) { config.Authority = nil },
		"changes":    func(config *pipeline.ManagementConfig) { config.Changes = nil },
		"reader":     func(config *pipeline.ManagementConfig) { config.Reader = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			config := base
			mutate(&config)
			_, err := pipeline.NewManagement(config)
			requireFaultClass(t, err, fault.InternalFailure)
		})
	}
}

func TestAdmissionServiceRequiresResolver(t *testing.T) {
	_, err := pipeline.NewAdmissionService(pipeline.AdmissionConfig{})
	requireFaultClass(t, err, fault.InternalFailure)
}

func TestManagementUpdateRequiresName(t *testing.T) {
	current := pipeline.Definition{ID: "pipeline-1", Name: "change", YAML: requestOnlyYAML(), Version: 2}
	reader := &fakeReader{getFn: func(context.Context, tenancy.Scope, pipeline.PipelineID) (pipeline.Definition, error) {
		return cloneDefinition(current), nil
	}}
	changes := &fakeChangePort{}
	management, _, _ := newManagement(t, changes, reader)
	actor := humanActor("tenant-a", "project-a", "principal-engineer", tenancy.RoleEngineer)

	_, err := management.Update(t.Context(), updateCommand(actor, "pipeline-1", "", typedInputsYAML(), 2))
	requireFaultClass(t, err, fault.InvalidInput)
	if changes.callCount() != 0 {
		t.Fatalf("change port called for an empty update name")
	}
}

// wantPushProjection is the expected projection for pushPipelineYAML: one
// trigger carrying the definition's repository, service and exact refs in
// declaration order.
func wantPushProjection() []pipeline.PushTrigger {
	return []pipeline.PushTrigger{{
		Repository: "repo-1",
		Service:    "git-service",
		Refs:       []string{"refs/heads/main", "refs/heads/release"},
	}}
}

// TestApplyAndUpdateCarryPushTriggerProjection pins that the change unit built
// by Apply or Update carries the validated on.push projection, in declaration
// order, so the adapter can persist pipelines.push_triggers without parsing YAML.
func TestApplyAndUpdateCarryPushTriggerProjection(t *testing.T) {
	actor := humanActor("tenant-a", "project-a", "principal-engineer", tenancy.RoleEngineer)
	t.Run("apply create", func(t *testing.T) {
		reader := &fakeReader{byNameFn: func(context.Context, tenancy.Scope, string) (pipeline.Definition, error) {
			return pipeline.Definition{}, fault.New(fault.NotFound, "pipeline is not present")
		}}
		changes := &fakeChangePort{}
		management, _, _ := newManagement(t, changes, reader)
		_, err := management.Apply(t.Context(), applyCommand(actor, "push", pushPipelineYAML()))
		requireNoError(t, err, "Apply push pipeline")
		unit := capturedUnit(t, changes)
		if !reflect.DeepEqual(unit.PushTriggers, wantPushProjection()) {
			t.Fatalf("unit push projection = %+v; want %+v", unit.PushTriggers, wantPushProjection())
		}
	})

	t.Run("update replaces the projection", func(t *testing.T) {
		current := pipeline.Definition{ID: "pipeline-1", Name: "change", YAML: requestOnlyYAML(), Version: 2}
		reader := &fakeReader{getFn: func(context.Context, tenancy.Scope, pipeline.PipelineID) (pipeline.Definition, error) {
			return cloneDefinition(current), nil
		}}
		changes := &fakeChangePort{}
		management, _, _ := newManagement(t, changes, reader)
		_, err := management.Update(t.Context(), updateCommand(actor, "pipeline-1", "push", pushPipelineYAML(), 2))
		requireNoError(t, err, "Update with push pipeline")
		unit := capturedUnit(t, changes)
		if !reflect.DeepEqual(unit.PushTriggers, wantPushProjection()) {
			t.Fatalf("unit push projection = %+v; want %+v", unit.PushTriggers, wantPushProjection())
		}
	})

	t.Run("update removes the projection", func(t *testing.T) {
		current := pipeline.Definition{
			ID:      "pipeline-1",
			Name:    "push",
			YAML:    pushPipelineYAML(),
			Version: 2,
			Push: &pipeline.PushTrigger{
				Repository: "repo-1",
				Service:    "git-service",
				Refs:       []string{"refs/heads/main", "refs/heads/release"},
			},
		}
		reader := &fakeReader{getFn: func(context.Context, tenancy.Scope, pipeline.PipelineID) (pipeline.Definition, error) {
			return cloneDefinition(current), nil
		}}
		changes := &fakeChangePort{}
		management, _, _ := newManagement(t, changes, reader)
		_, err := management.Update(t.Context(), updateCommand(actor, "pipeline-1", "change", requestOnlyYAML(), 2))
		requireNoError(t, err, "Update removing push")
		unit := capturedUnit(t, changes)
		if len(unit.PushTriggers) != 0 {
			t.Fatalf("unit push projection = %+v; want none after removal", unit.PushTriggers)
		}
	})

	t.Run("apply keeps an unchanged projection", func(t *testing.T) {
		// The reader exposes the persisted column form only; the service must
		// still compare it and carry the same projection without re-requiring
		// push-service Admin authority.
		current := pipeline.Definition{
			ID:           "pipeline-1",
			Name:         "push",
			YAML:         pushPipelineYAML(),
			Version:      2,
			PushTriggers: wantPushProjection(),
		}
		reader := &fakeReader{byNameFn: func(context.Context, tenancy.Scope, string) (pipeline.Definition, error) {
			return cloneDefinition(current), nil
		}}
		changes := &fakeChangePort{}
		management, _, _ := newManagement(t, changes, reader)
		_, err := management.Apply(t.Context(), applyCommand(actor, "push", pushPipelineYAML()))
		requireNoError(t, err, "Apply unchanged push pipeline")
		unit := capturedUnit(t, changes)
		if !reflect.DeepEqual(unit.PushTriggers, wantPushProjection()) {
			t.Fatalf("unit push projection = %+v; want %+v", unit.PushTriggers, wantPushProjection())
		}
		requireNoAuthority(t, unit.Authority, authorize.CheckPushServiceAdmin)
	})
}

// TestDeleteDropsPushTriggerProjection proves a deletion unit carries no
// push projection: the row and its triggers are removed together.
func TestDeleteDropsPushTriggerProjection(t *testing.T) {
	actor := humanActor("tenant-a", "project-a", "principal-engineer", tenancy.RoleEngineer)
	current := pipeline.Definition{
		ID:      "pipeline-1",
		Name:    "push",
		YAML:    pushPipelineYAML(),
		Version: 2,
		Push: &pipeline.PushTrigger{
			Repository: "repo-1",
			Service:    "git-service",
			Refs:       []string{"refs/heads/main", "refs/heads/release"},
		},
	}
	reader := &fakeReader{getFn: func(context.Context, tenancy.Scope, pipeline.PipelineID) (pipeline.Definition, error) {
		return cloneDefinition(current), nil
	}}
	changes := &fakeChangePort{}
	management, _, _ := newManagement(t, changes, reader)
	_, err := management.Delete(t.Context(), deleteCommand(actor, "pipeline-1", 2))
	requireNoError(t, err, "Delete push pipeline")
	unit := capturedUnit(t, changes)
	if len(unit.PushTriggers) != 0 {
		t.Fatalf("delete unit push projection = %+v; want none", unit.PushTriggers)
	}
}

// TestReadReturnsPushTriggerProjection pins the read side: a definition read
// through management exposes the same validated on.push projection the change
// unit persisted, whether the reader returns the column or only exact YAML.
func TestReadReturnsPushTriggerProjection(t *testing.T) {
	actor := humanActor("tenant-a", "project-a", "principal-engineer", tenancy.RoleEngineer)
	cases := map[string]pipeline.Definition{
		"reader returns exact yaml only": {
			ID: "pipeline-1", Name: "push", YAML: pushPipelineYAML(), Version: 3,
		},
		"reader returns the persisted column": {
			ID: "pipeline-1", Name: "push", YAML: pushPipelineYAML(), Version: 3,
			PushTriggers: wantPushProjection(),
		},
	}
	for name, current := range cases {
		t.Run(name, func(t *testing.T) {
			reader := &fakeReader{
				getFn: func(context.Context, tenancy.Scope, pipeline.PipelineID) (pipeline.Definition, error) {
					return cloneDefinition(current), nil
				},
				listFn: func(context.Context, tenancy.Scope) ([]pipeline.Definition, error) {
					return []pipeline.Definition{cloneDefinition(current)}, nil
				},
			}
			management, _, _ := newManagement(t, &fakeChangePort{}, reader)
			got, err := management.Read(t.Context(), pipeline.ReadCommand{Actor: actor, Scope: actor.Scope, PipelineID: "pipeline-1"})
			requireNoError(t, err, "Read")
			if !reflect.DeepEqual(got.PushTriggers, wantPushProjection()) {
				t.Fatalf("read push projection = %+v; want %+v", got.PushTriggers, wantPushProjection())
			}

			list, err := management.List(t.Context(), pipeline.ListCommand{Actor: actor, Scope: actor.Scope})
			requireNoError(t, err, "List")
			if len(list) != 1 {
				t.Fatalf("list = %v; want one definition", list)
			}
			if !reflect.DeepEqual(list[0].PushTriggers, wantPushProjection()) {
				t.Fatalf("list push projection = %+v; want %+v", list[0].PushTriggers, wantPushProjection())
			}
		})
	}
}

// TestReadKeepsDefinitionsWithoutPush proves the projection stays absent when
// the definition declares no on.push trigger.
func TestReadKeepsDefinitionsWithoutPush(t *testing.T) {
	actor := humanActor("tenant-a", "project-a", "principal-engineer", tenancy.RoleEngineer)
	current := pipeline.Definition{ID: "pipeline-1", Name: "change", YAML: requestOnlyYAML(), Version: 2}
	reader := &fakeReader{getFn: func(context.Context, tenancy.Scope, pipeline.PipelineID) (pipeline.Definition, error) {
		return cloneDefinition(current), nil
	}}
	management, _, _ := newManagement(t, &fakeChangePort{}, reader)
	got, err := management.Read(t.Context(), pipeline.ReadCommand{Actor: actor, Scope: actor.Scope, PipelineID: "pipeline-1"})
	requireNoError(t, err, "Read")
	if got.PushTriggers != nil || got.Push != nil {
		t.Fatalf("read push = %+v / %+v; want none", got.Push, got.PushTriggers)
	}
}

// TestManagementPushProjectionRoundTripThroughStore proves the committed unit's
// projection is what a later read observes: the fake store models the
// pipelines.push_triggers column written by the change unit.
func TestManagementPushProjectionRoundTripThroughStore(t *testing.T) {
	store := newFakeStore()
	management, _, _ := newManagement(t, store, store)
	actor := humanActor("tenant-a", "project-a", "principal-engineer", tenancy.RoleEngineer)
	applied, err := management.Apply(t.Context(), applyCommand(actor, "push", pushPipelineYAML()))
	requireNoError(t, err, "Apply push pipeline")
	units := store.captured()
	if len(units) != 1 {
		t.Fatalf("store captured %d units; want 1", len(units))
	}
	if !reflect.DeepEqual(units[0].PushTriggers, wantPushProjection()) {
		t.Fatalf("unit push projection = %+v; want %+v", units[0].PushTriggers, wantPushProjection())
	}

	got, err := management.Read(t.Context(), pipeline.ReadCommand{Actor: actor, Scope: actor.Scope, PipelineID: applied.PipelineID})
	requireNoError(t, err, "Read applied pipeline")
	if !reflect.DeepEqual(got.PushTriggers, wantPushProjection()) {
		t.Fatalf("read push projection = %+v; want %+v", got.PushTriggers, wantPushProjection())
	}
}
