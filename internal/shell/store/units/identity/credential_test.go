package identity_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

var testExpiry = testNow.Add(time.Hour)

func testNonce(fill byte) []byte {
	nonce := make([]byte, token.MinNonceBytes)
	for index := range nonce {
		nonce[index] = fill
	}
	return nonce
}

func (h *harness) seedExecution(scope tenancy.Scope, executionID, principal string, version uint64, state string, nonce []byte, expiry *time.Time) {
	h.t.Helper()
	values := map[string]any{
		"principal_id": principal,
		"state":        state,
		"version":      int64(version),
		"yaml_sha256":  []byte("sha256"),
		"snapshot":     []byte(`{}`),
		"created_at":   testNow,
	}
	if nonce != nil {
		values["token_nonce"] = nonce
	}
	if expiry != nil {
		values["token_expires_at"] = *expiry
	}
	h.seed(scope, "executions", executionID, values)
}

func (h *harness) seedService(scope tenancy.Scope, serviceID string, version uint64, state string, nonce []byte, expiry *time.Time) {
	h.t.Helper()
	values := map[string]any{
		"name":       serviceID,
		"state":      state,
		"roles":      rolesJSON(h.t, tenancy.RoleEngineer),
		"version":    int64(version),
		"created_at": testNow,
	}
	if nonce != nil {
		values["token_nonce"] = nonce
	}
	if expiry != nil {
		values["token_expires_at"] = *expiry
	}
	h.seed(scope, "services", serviceID, values)
}

func (h *harness) executionMutation(scope tenancy.Scope, actor, executionID, principal string, kind token.MutationKind, expected uint64, nonce []byte, expiry *time.Time, result token.MutationResult) token.MutationUnit {
	unit := token.MutationUnit{
		Owner:           scope,
		PrincipalID:     id.PrincipalID(principal),
		ExecutionID:     id.ExecutionID(executionID),
		Kind:            kind,
		ExpectedVersion: expected,
		Receipt:         h.receipt(scope, id.PrincipalID(actor), "service.credential."+string(kind), "caller-"+executionID, result),
		Audit:           []event.Audit{h.audit(scope, id.PrincipalID(actor), "service.credential."+string(kind))},
		Events:          []event.Envelope{h.envelope(scope, id.PrincipalID(actor), executionID, expected+1)},
		Authority: []authorize.AuthorityRead{
			membershipRead(scope, id.PrincipalID(actor)),
			principalEnabledRead(scope, id.PrincipalID(principal)),
		},
	}
	if nonce != nil || expiry != nil {
		credential := token.Credential{Nonce: nonce}
		if expiry != nil {
			credential.Expiry = *expiry
		}
		unit.Credential = credential
	}
	return unit
}

func TestMutateExecutionIssueRoundTrip(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	h.seedExecution(scope, "execution-a", "principal-exec", 1, "running", nil, nil)
	unit := h.executionMutation(scope, "principal-admin", "execution-a", "principal-exec", token.MutationIssue, 1, testNonce(0x42), &testExpiry, token.MutationResult{State: token.StateCurrent, Version: 2})
	result, err := h.adapter.Mutate(context.Background(), unit)
	if err != nil {
		t.Fatalf("Mutate: %v", err)
	}
	if result.State != token.StateCurrent || result.Version != 2 || result.Replayed {
		t.Fatalf("result = %+v; want current version 2", result)
	}
	fact, err := h.adapter.ReadCredential(context.Background(), token.CredentialRequest{
		Scope: scope, PrincipalID: "principal-exec", ExecutionID: "execution-a",
	})
	if err != nil {
		t.Fatalf("ReadCredential: %v", err)
	}
	if !fact.Enabled || fact.Version != 2 {
		t.Fatalf("fact = %+v; want enabled version 2", fact)
	}
	if string(fact.Credential.Nonce) != string(testNonce(0x42)) {
		t.Fatalf("nonce = %x; want the committed nonce", fact.Credential.Nonce)
	}
	if !fact.Credential.Expiry.Equal(testExpiry) {
		t.Fatalf("expiry = %v; want %v", fact.Credential.Expiry, testExpiry)
	}
	if _, found := h.store.Receipt(unit.Receipt.Key); !found {
		t.Fatal("the credential receipt was not committed")
	}
}

func TestMutateRevokeClearsPair(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	h.seedExecution(scope, "execution-a", "principal-exec", 2, "running", testNonce(0x11), &testExpiry)
	unit := h.executionMutation(scope, "principal-admin", "execution-a", "principal-exec", token.MutationRevoke, 2, nil, nil, token.MutationResult{State: token.StateAbsent, Version: 3})
	result, err := h.adapter.Mutate(context.Background(), unit)
	if err != nil {
		t.Fatalf("Mutate: %v", err)
	}
	if result.State != token.StateAbsent || result.Version != 3 {
		t.Fatalf("result = %+v; want absent version 3", result)
	}
	fact, err := h.adapter.ReadCredential(context.Background(), token.CredentialRequest{
		Scope: scope, PrincipalID: "principal-exec", ExecutionID: "execution-a",
	})
	if err != nil {
		t.Fatalf("ReadCredential: %v", err)
	}
	if fact.Credential.Present() {
		t.Fatalf("credential = %+v; want a cleared pair", fact.Credential)
	}
	if fact.Credential.State(testNow) != token.StateAbsent {
		t.Fatalf("credential state = %q; want absent", fact.Credential.State(testNow))
	}
}

func TestMutateStaleVersionLeavesNoPartialState(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	h.seedExecution(scope, "execution-a", "principal-exec", 1, "running", nil, nil)
	unit := h.executionMutation(scope, "principal-admin", "execution-a", "principal-exec", token.MutationIssue, 4, testNonce(0x42), &testExpiry, token.MutationResult{State: token.StateCurrent, Version: 5})
	_, err := h.adapter.Mutate(context.Background(), unit)
	requireFaultClass(t, err, fault.VersionConflict)
	fact, err := h.adapter.ReadCredential(context.Background(), token.CredentialRequest{
		Scope: scope, PrincipalID: "principal-exec", ExecutionID: "execution-a",
	})
	if err != nil {
		t.Fatalf("ReadCredential: %v", err)
	}
	if fact.Credential.Present() || fact.Version != 1 {
		t.Fatalf("fact = %+v; want the untouched version 1 with no pair", fact)
	}
	if got := len(h.store.Events(scope)); got != 0 {
		t.Fatalf("event rows = %d; want 0", got)
	}
	if _, found := h.store.Receipt(unit.Receipt.Key); found {
		t.Fatal("the refused mutation left a receipt")
	}
}

func TestMutateServiceIssueAndDisable(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	h.seedService(scope, "service-a", 1, string(service.StateEnabled), nil, nil)
	enabled := service.StateEnabled
	issue := token.MutationUnit{
		Owner:           scope,
		PrincipalID:     "service-a",
		ServiceID:       "service-a",
		Kind:            token.MutationIssue,
		ExpectedVersion: 1,
		Credential:      token.Credential{Nonce: testNonce(0x55), Expiry: testExpiry},
		Service:         &token.ServiceChange{State: &enabled, Roles: []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleReviewer}},
		Receipt:         h.receipt(scope, "principal-admin", "service.credential.issue", "caller-service", token.MutationResult{State: token.StateCurrent, Version: 2}),
		Audit:           []event.Audit{h.audit(scope, "principal-admin", "service.credential.issue")},
		Events:          []event.Envelope{h.envelope(scope, "principal-admin", "service-a", 2)},
		Authority: []authorize.AuthorityRead{
			membershipRead(scope, "principal-admin"),
			{Check: authorize.CheckServiceEnabled, Scope: scope, PrincipalID: "principal-admin", TargetID: "service-a"},
		},
	}
	if _, err := h.adapter.Mutate(context.Background(), issue); err != nil {
		t.Fatalf("service issue: %v", err)
	}
	read, err := h.adapter.ReadService(context.Background(), service.ServiceReadRequest{Scope: scope, ServiceID: "service-a"})
	if err != nil {
		t.Fatalf("ReadService: %v", err)
	}
	if read.Version != 2 || read.TokenNonce == nil || read.TokenExpiry == nil {
		t.Fatalf("service = %+v; want version 2 with a pair", read)
	}
	if len(read.Roles) != 2 {
		t.Fatalf("service roles = %v; want the committed role set", read.Roles)
	}

	disabled := service.StateDisabled
	disable := issue
	disable.Kind = token.MutationRevoke
	disable.ExpectedVersion = 2
	disable.Credential = token.Credential{}
	disable.Service = &token.ServiceChange{State: &disabled}
	disable.Receipt = h.receipt(scope, "principal-admin", "service.credential.revoke", "caller-disable", token.MutationResult{State: token.StateAbsent, Version: 3})
	disable.Audit = []event.Audit{h.audit(scope, "principal-admin", "service.credential.revoke")}
	disable.Events = []event.Envelope{h.envelope(scope, "principal-admin", "service-a", 3)}
	result, err := h.adapter.Mutate(context.Background(), disable)
	if err != nil {
		t.Fatalf("service disable: %v", err)
	}
	if result.State != token.StateAbsent {
		t.Fatalf("disable result = %+v; want absent", result)
	}
	read, err = h.adapter.ReadService(context.Background(), service.ServiceReadRequest{Scope: scope, ServiceID: "service-a"})
	if err != nil {
		t.Fatalf("ReadService: %v", err)
	}
	if read.State != service.StateDisabled || read.TokenNonce != nil || read.TokenExpiry != nil {
		t.Fatalf("service = %+v; want disabled with a cleared pair", read)
	}
}

func TestMutateReplayReturnsOriginalResult(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	h.seedExecution(scope, "execution-a", "principal-exec", 1, "running", nil, nil)
	unit := h.executionMutation(scope, "principal-admin", "execution-a", "principal-exec", token.MutationIssue, 1, testNonce(0x42), &testExpiry, token.MutationResult{State: token.StateCurrent, Version: 2})
	first, err := h.adapter.Mutate(context.Background(), unit)
	if err != nil {
		t.Fatalf("first Mutate: %v", err)
	}
	second, err := h.adapter.Mutate(context.Background(), unit)
	if err != nil {
		t.Fatalf("replayed Mutate: %v", err)
	}
	if !second.Replayed || second.State != first.State || second.Version != first.Version {
		t.Fatalf("replayed result = %+v; want %+v replayed", second, first)
	}
	if got := len(h.store.Events(scope)); got != 1 {
		t.Fatalf("event rows after replay = %d; want 1", got)
	}
}

func TestReadTenantKeyRoundTripsEncryptedMembers(t *testing.T) {
	h := newHarness(t)
	key := testTenantKey("tenant-a", 3)
	encoded, err := json.Marshal(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	h.seed(tenantScope("tenant-a"), "tenants", "tenant-a", map[string]any{
		"name":           "Tenant a",
		"version":        int64(3),
		"derivation_key": encoded,
		"created_at":     testNow,
	})
	read, err := h.adapter.ReadTenantKey(context.Background(), token.TenantKeyRequest{TenantID: "tenant-a"})
	if err != nil {
		t.Fatalf("ReadTenantKey: %v", err)
	}
	if read.KeyRef != key.KeyRef || read.KeyVersion != key.KeyVersion || string(read.WrappedKey) != string(key.WrappedKey) {
		t.Fatalf("read key = %+v; want the stored encrypted key", read)
	}
	_, err = h.adapter.ReadTenantKey(context.Background(), token.TenantKeyRequest{TenantID: "tenant-b"})
	requireFaultClass(t, err, fault.NotFound)
}

func TestConcurrentCredentialRotationOneWinner(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	h.seedExecution(scope, "execution-a", "principal-exec", 1, "running", testNonce(0x01), &testExpiry)
	base := h.executionMutation(scope, "principal-admin", "execution-a", "principal-exec", token.MutationRotate, 1, testNonce(0x02), &testExpiry, token.MutationResult{State: token.StateCurrent, Version: 2})
	other := h.executionMutation(scope, "principal-admin", "execution-a", "principal-exec", token.MutationRotate, 1, testNonce(0x03), &testExpiry, token.MutationResult{State: token.StateCurrent, Version: 2})
	other.Receipt = h.receipt(scope, "principal-admin", "service.credential.rotate", "caller-other", token.MutationResult{State: token.StateCurrent, Version: 2})
	other.Audit = []event.Audit{h.audit(scope, "principal-admin", "service.credential.rotate")}
	other.Events = []event.Envelope{h.envelope(scope, "principal-admin", "execution-a", 2)}

	var wait sync.WaitGroup
	results := make([]error, 2)
	wait.Add(2)
	for index, unit := range []token.MutationUnit{base, other} {
		go func(index int, unit token.MutationUnit) {
			defer wait.Done()
			_, results[index] = h.adapter.Mutate(context.Background(), unit)
		}(index, unit)
	}
	wait.Wait()
	winners := 0
	for _, err := range results {
		if err == nil {
			winners++
			continue
		}
		requireFaultClass(t, err, fault.VersionConflict)
	}
	if winners != 1 {
		t.Fatalf("winners = %d; want exactly one", winners)
	}
	fact, err := h.adapter.ReadCredential(context.Background(), token.CredentialRequest{Scope: scope, PrincipalID: "principal-exec", ExecutionID: "execution-a"})
	if err != nil {
		t.Fatalf("ReadCredential: %v", err)
	}
	if fact.Version != 2 {
		t.Fatalf("credential version = %d; want 2", fact.Version)
	}
}

func TestMutateServicePrincipalMustBeServiceIdentity(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	h.seedService(scope, "service-a", 1, string(service.StateEnabled), nil, nil)
	enabled := service.StateEnabled
	unit := token.MutationUnit{
		Owner:           scope,
		PrincipalID:     "principal-other",
		ServiceID:       "service-a",
		Kind:            token.MutationIssue,
		ExpectedVersion: 1,
		Credential:      token.Credential{Nonce: testNonce(0x55), Expiry: testExpiry},
		Service:         &token.ServiceChange{State: &enabled},
		Receipt:         h.receipt(scope, "principal-admin", "service.credential.issue", "caller-mismatch", token.MutationResult{State: token.StateCurrent, Version: 2}),
		Audit:           []event.Audit{h.audit(scope, "principal-admin", "service.credential.issue")},
		Events:          []event.Envelope{h.envelope(scope, "principal-admin", "service-a", 2)},
		Authority:       []authorize.AuthorityRead{membershipRead(scope, "principal-admin")},
	}
	_, err := h.adapter.Mutate(context.Background(), unit)
	requireFaultClass(t, err, fault.InvalidInput)
	read, err := h.adapter.ReadService(context.Background(), service.ServiceReadRequest{Scope: scope, ServiceID: "service-a"})
	if err != nil {
		t.Fatalf("ReadService: %v", err)
	}
	if read.Version != 1 || read.TokenNonce != nil {
		t.Fatalf("service = %+v; want the untouched version 1 with no pair", read)
	}
	if got := len(h.store.Events(scope)); got != 0 {
		t.Fatalf("event rows = %d; want 0", got)
	}
}

func TestMutateExecutionPrincipalMismatchRefused(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	h.seedExecution(scope, "execution-a", "principal-exec", 1, "running", nil, nil)
	unit := h.executionMutation(scope, "principal-admin", "execution-a", "principal-other", token.MutationIssue, 1, testNonce(0x42), &testExpiry, token.MutationResult{State: token.StateCurrent, Version: 2})
	_, err := h.adapter.Mutate(context.Background(), unit)
	requireFaultClass(t, err, fault.VersionConflict)
	fact, err := h.adapter.ReadCredential(context.Background(), token.CredentialRequest{Scope: scope, PrincipalID: "principal-exec", ExecutionID: "execution-a"})
	if err != nil {
		t.Fatalf("ReadCredential: %v", err)
	}
	if fact.Credential.Present() || fact.Version != 1 {
		t.Fatalf("fact = %+v; want the untouched version 1 with no pair", fact)
	}
}

func TestReadCredentialReturnsCommittedPrincipal(t *testing.T) {
	h := newHarness(t)
	scope := projectScope("tenant-a", "project-a")
	h.seedMembership(scope, "membership-admin", "principal-admin", 1, tenancy.RoleAdmin)
	h.seedExecution(scope, "execution-a", "principal-exec", 1, "running", testNonce(0x21), &testExpiry)
	h.seedService(scope, "service-a", 1, string(service.StateEnabled), testNonce(0x22), &testExpiry)
	execution, err := h.adapter.ReadCredential(context.Background(), token.CredentialRequest{
		Scope: scope, PrincipalID: "principal-claimed", ExecutionID: "execution-a",
	})
	if err != nil {
		t.Fatalf("execution ReadCredential: %v", err)
	}
	if execution.PrincipalID != "principal-exec" {
		t.Fatalf("execution principal = %q; want the committed principal-exec", execution.PrincipalID)
	}
	serviceFact, err := h.adapter.ReadCredential(context.Background(), token.CredentialRequest{
		Scope: scope, PrincipalID: "principal-claimed", ServiceID: "service-a",
	})
	if err != nil {
		t.Fatalf("service ReadCredential: %v", err)
	}
	if serviceFact.PrincipalID != "service-a" {
		t.Fatalf("service principal = %q; want the service identity", serviceFact.PrincipalID)
	}
}
