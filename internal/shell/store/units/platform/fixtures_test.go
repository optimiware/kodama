package platform_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/memory"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

const (
	testTenant       = id.TenantID("tenant-a")
	testProject      = id.ProjectID("project-a")
	otherTenant      = id.TenantID("tenant-b")
	otherProject     = id.ProjectID("project-b")
	testPrincipal    = id.PrincipalID("principal-exec")
	testActor        = id.PrincipalID("principal-human")
	testService      = id.PrincipalID("svc-git")
	testExecution    = id.ExecutionID("exec-1")
	testAction       = id.ActionID("action-1")
	testBinding      = "binding-1"
	testResource     = "repo-1"
	testEventID      = id.EventID("event-1")
	testReceiptID    = id.ReceiptID("receipt-1")
	testSecretID     = "secret-1"
	testSecretName   = id.SecretName("API_KEY")
	testMembership   = "membership-1"
	testVerification = "verification-1"
)

var fixedNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func testScope() tenancy.Scope {
	return tenancy.Scope{TenantID: testTenant, ProjectID: testProject}
}

func otherScope() tenancy.Scope {
	return tenancy.Scope{TenantID: otherTenant, ProjectID: otherProject}
}

func newStore() *memory.Store {
	return memory.NewWithClock(func() time.Time { return fixedNow })
}

// storedRequestSubject is the exact bytes the request-activation adapter writes
// for a resolved definition.Subject. Tests seed this shape, never a
// verify.Subject, so a decoder that accepts the wrong shape fails. A request
// used as approval evidence carries its operation and resource in the immutable
// subject alongside the effect fields (KODAMA.md, S05 §4).
func storedRequestSubject() []byte {
	return storedRequestSubjectFields(map[string]string{
		"operation": "git.ref.update",
		"resource":  testResource,
		"ref":       "refs/heads/main",
	})
}

// storedRequestSubjectFields builds the stored definition.Subject object from
// literal field values so a test can omit or change any approval field.
func storedRequestSubjectFields(fields map[string]string) []byte {
	values := make(map[string]definition.SubjectValue, len(fields))
	for name, value := range fields {
		values[name] = definition.SubjectValue{Kind: definition.SubjectLiteral, Literal: value}
	}
	subject := definition.NewSubject(values)
	data, err := json.Marshal(subject)
	if err != nil {
		panic("marshal definition subject: " + err.Error())
	}
	return data
}

// storedReferencedRequestSubject is a definition.Subject that still carries an
// unresolved reference, which a committed request action must never hold.
func storedReferencedRequestSubject() []byte {
	subject := definition.NewSubject(map[string]definition.SubjectValue{
		"ref": {Kind: definition.SubjectReference, Reference: definition.Reference{Input: true, Key: "ref"}},
	})
	data, err := json.Marshal(subject)
	if err != nil {
		panic("marshal definition subject: " + err.Error())
	}
	return data
}

type memoryStoreAlias = memory.Store

func mustExecute(t *testing.T, s *memory.Store, scope tenancy.Scope, unit store.Unit) store.UnitResult {
	t.Helper()
	result, err := s.Execute(context.Background(), scope, unit)
	if err != nil {
		t.Fatalf("seed execute: %v", err)
	}
	return result
}

func mustInsert(t *testing.T, s *memory.Store, scope tenancy.Scope, table, identity string, values map[string]any) {
	t.Helper()
	mustExecute(t, s, scope, store.Unit{Writes: []store.Write{{Table: table, ID: identity, Insert: true, Values: values}}})
}

func seedMembership(t *testing.T, s *memory.Store, scope tenancy.Scope, identity string, principal id.PrincipalID) {
	t.Helper()
	seedMembershipRoles(t, s, scope, identity, principal, []tenancy.Role{tenancy.RoleEngineer})
}

func seedMembershipRoles(t *testing.T, s *memory.Store, scope tenancy.Scope, identity string, principal id.PrincipalID, roles []tenancy.Role) {
	t.Helper()
	encoded, err := json.Marshal(roles)
	if err != nil {
		t.Fatalf("marshal membership roles: %v", err)
	}
	mustInsert(t, s, scope, "memberships", identity, map[string]any{
		"principal_id": string(principal),
		"roles":        encoded,
		"version":      uint64(1),
		"created_at":   fixedNow,
	})
}

func seedService(t *testing.T, s *memory.Store, scope tenancy.Scope, identity id.PrincipalID, state string) {
	t.Helper()
	seedServiceRoles(t, s, scope, identity, state, []tenancy.Role{tenancy.RoleEngineer})
}

func seedServiceRoles(t *testing.T, s *memory.Store, scope tenancy.Scope, identity id.PrincipalID, state string, roles []tenancy.Role) {
	t.Helper()
	encoded, err := json.Marshal(roles)
	if err != nil {
		t.Fatalf("marshal service roles: %v", err)
	}
	mustInsert(t, s, scope, "services", string(identity), map[string]any{
		"name":       "git",
		"state":      state,
		"roles":      encoded,
		"version":    uint64(1),
		"created_at": fixedNow,
	})
}

func seedBinding(t *testing.T, s *memory.Store, scope tenancy.Scope, identity string, version uint64) {
	t.Helper()
	mustInsert(t, s, scope, "resource_bindings", identity, map[string]any{
		"resource_id":       testResource,
		"service_id":        string(testService),
		"origins":           []byte(`["https://git.example.com"]`),
		"operations":        []byte(`["git.ref.update"]`),
		"reportable_events": []byte(`["git.ref.updated"]`),
		"metadata":          []byte(`{}`),
		"version":           version,
		"created_at":        fixedNow,
	})
}

func seedExecution(t *testing.T, s *memory.Store, scope tenancy.Scope, identity id.ExecutionID, principal id.PrincipalID, state string) {
	t.Helper()
	mustInsert(t, s, scope, "executions", string(identity), map[string]any{
		"pipeline_id":      nil,
		"task_id":          nil,
		"principal_id":     string(principal),
		"state":            state,
		"version":          uint64(1),
		"yaml_sha256":      []byte{1, 2, 3},
		"snapshot":         []byte(`{}`),
		"token_nonce":      nil,
		"token_expires_at": nil,
		"workspace_id":     nil,
		"created_at":       fixedNow,
	})
}

func seedAction(t *testing.T, s *memory.Store, scope tenancy.Scope, identity id.ActionID, execution id.ExecutionID, state, decision string) {
	t.Helper()
	seedActionWithDeadline(t, s, scope, identity, execution, state, decision, fixedNow.Add(time.Hour))
}

func seedActionWithDeadline(t *testing.T, s *memory.Store, scope tenancy.Scope, identity id.ActionID, execution id.ExecutionID, state, decision string, deadline time.Time) {
	t.Helper()
	seedActionAt(t, s, scope, identity, execution, 0, "approve", state, decision, deadline)
}

// seedActionAt seeds one request action at an explicit index and name. An
// execution's action index and name are each unique, so a test that seeds a
// second action under one execution names its own position.
func seedActionAt(t *testing.T, s *memory.Store, scope tenancy.Scope, identity id.ActionID, execution id.ExecutionID, index int64, name, state, decision string, deadline time.Time) {
	t.Helper()
	values := map[string]any{
		"execution_id":        string(execution),
		"index":               index,
		"name":                name,
		"kind":                "request",
		"state":               state,
		"version":             uint64(1),
		"timeout_ms":          int64(60000),
		"deadline":            deadline,
		"outputs":             nil,
		"staged_outputs":      nil,
		"staged_outputs_set":  false,
		"stop":                nil,
		"request_subject":     storedRequestSubject(),
		"request_assignment":  []byte(`{"kind":"role","role":"reviewer"}`),
		"request_decision":    nil,
		"request_actor":       nil,
		"request_resolved_at": nil,
		"workload_id":         nil,
		"launch_state":        "not_issued",
		"launch_key":          nil,
		"observation":         nil,
		"activated_at":        nil,
		"answer_provenance":   nil,
	}
	if decision != "" {
		values["request_decision"] = decision
		values["request_actor"] = string(testActor)
		values["request_resolved_at"] = fixedNow
	}
	mustInsert(t, s, scope, "actions", string(identity), values)
}

// seedActionSubject seeds a succeeded, passed request action with exactly the
// supplied request_subject bytes so a decode regression is reproduced.
func seedActionSubject(t *testing.T, s *memory.Store, scope tenancy.Scope, identity id.ActionID, execution id.ExecutionID, subject []byte) {
	t.Helper()
	mustInsert(t, s, scope, "actions", string(identity), map[string]any{
		"execution_id":        string(execution),
		"index":               int64(0),
		"name":                "approve",
		"kind":                "request",
		"state":               "succeeded",
		"version":             uint64(1),
		"timeout_ms":          int64(60000),
		"deadline":            fixedNow.Add(time.Hour),
		"outputs":             nil,
		"staged_outputs":      nil,
		"staged_outputs_set":  false,
		"stop":                nil,
		"request_subject":     subject,
		"request_assignment":  []byte(`{"kind":"role","role":"reviewer"}`),
		"request_decision":    "pass",
		"request_actor":       string(testActor),
		"request_resolved_at": fixedNow,
		"workload_id":         nil,
		"launch_state":        "not_issued",
		"launch_key":          nil,
		"observation":         nil,
		"activated_at":        nil,
		"answer_provenance":   nil,
	})
}

func seedVerification(t *testing.T, s *memory.Store, scope tenancy.Scope, identity string) {
	t.Helper()
	mustInsert(t, s, scope, "verifications", identity, map[string]any{
		"service_id":         string(testService),
		"actor_id":           string(testPrincipal),
		"execution_id":       string(testExecution),
		"action_id":          string(testAction),
		"binding_id":         testBinding,
		"resource_id":        testResource,
		"operation":          "git.ref.update",
		"subject":            []byte(`{"fields":[{"name":"ref","kind":"string","value":"refs/heads/main"}]}`),
		"requested_approval": nil,
		"result":             "approved",
		"approved":           true,
		"created_at":         fixedNow,
	})
}

func seedContainerAction(t *testing.T, s *memory.Store, scope tenancy.Scope, identity id.ActionID, execution id.ExecutionID, state string, deadline time.Time) {
	t.Helper()
	seedContainerActionAt(t, s, scope, identity, execution, 1, "run", state, deadline)
}

// seedContainerActionAt seeds one container action at an explicit index and
// name, for a test that seeds more than one action under one execution.
func seedContainerActionAt(t *testing.T, s *memory.Store, scope tenancy.Scope, identity id.ActionID, execution id.ExecutionID, index int64, name, state string, deadline time.Time) {
	t.Helper()
	mustInsert(t, s, scope, "actions", string(identity), map[string]any{
		"execution_id":        string(execution),
		"index":               index,
		"name":                name,
		"kind":                "container",
		"state":               state,
		"version":             uint64(1),
		"timeout_ms":          int64(60000),
		"deadline":            deadline,
		"outputs":             nil,
		"staged_outputs":      nil,
		"staged_outputs_set":  false,
		"stop":                nil,
		"request_subject":     nil,
		"request_assignment":  nil,
		"request_decision":    nil,
		"request_actor":       nil,
		"request_resolved_at": nil,
		"workload_id":         nil,
		"launch_state":        "issued",
		"launch_key":          nil,
		"observation":         nil,
		"activated_at":        nil,
		"answer_provenance":   nil,
	})
}

func seedSecret(t *testing.T, s *memory.Store, scope tenancy.Scope, identity string) {
	t.Helper()
	mustInsert(t, s, scope, "secrets", identity, map[string]any{
		"name":        string(testSecretName),
		"ciphertext":  []byte("ciphertext-bytes"),
		"wrapped_key": bytesOfLength(48, 7),
		"wrap_nonce":  bytesOfLength(12, 3),
		"key_ref":     "key-1",
		"key_version": int64(2),
		"nonce":       bytesOfLength(12, 9),
		"version":     uint64(4),
		"created_at":  fixedNow,
		"updated_at":  fixedNow.Add(time.Minute),
	})
}

func bytesOfLength(length int, fill byte) []byte {
	value := make([]byte, length)
	for index := range value {
		value[index] = fill
	}
	return value
}

func seedEvent(t *testing.T, s *memory.Store, scope tenancy.Scope, eventID id.EventID, eventType event.Type) {
	t.Helper()
	mustExecute(t, s, scope, store.Unit{Events: []event.Envelope{{
		ID:        eventID,
		Type:      eventType,
		Source:    event.Source{Kind: event.SourceShell, PrincipalID: testActor},
		At:        fixedNow,
		Scope:     scope,
		SubjectID: "subject-1",
	}}})
}

func requireClass(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want %s", want)
	}
	got, ok := fault.ClassOf(err)
	if !ok {
		t.Fatalf("error %v has no fault class, want %s", err, want)
	}
	if got != want {
		t.Fatalf("error class = %s, want %s (%v)", got, want, err)
	}
}

// deleteRow removes one committed row, so a later commit reproduces a revoke.
func deleteRow(t *testing.T, s *memory.Store, scope tenancy.Scope, table, identity string, expectedVersion uint64) {
	t.Helper()
	mustExecute(t, s, scope, store.Unit{Writes: []store.Write{{
		Table:         table,
		ID:            identity,
		Delete:        true,
		ExpectVersion: expectedVersion,
	}}})
}

// updateVersion advances one row's optimistic version with an explicit guarded
// update, so a later stale-version commit reproduces a concurrent change.
func updateVersion(t *testing.T, s *memory.Store, scope tenancy.Scope, table, identity string, expected uint64) {
	t.Helper()
	mustExecute(t, s, scope, store.Unit{Writes: []store.Write{{
		Table:         table,
		ID:            identity,
		ExpectVersion: expected,
		Values:        map[string]any{"created_at": fixedNow},
	}}})
}
