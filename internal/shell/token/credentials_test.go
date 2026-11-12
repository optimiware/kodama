// Registry-aware service credential operation tests.
//
// service.credential.issue/rotate/revoke are registry operations, not raw
// Manager calls: CredentialAdmin authorizes the human Admin, re-reads the
// current principal and the target service/credential state, builds the
// receipt/audit/event/authority-read intent and delegates the atomic mutation
// to token.Manager. The issuing response is the only place a freshly derived
// token may appear; receipts, audit rows, events and errors never carry a
// stored secret.
package token_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

type credentialAuthorizer struct {
	mu       sync.Mutex
	decision authorize.Decision
	err      error
	calls    int
}

func (fake *credentialAuthorizer) Authorize(_ context.Context, _ authorize.Principal, _ authorize.OperationName, _ tenancy.Scope) (authorize.Decision, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.calls++
	if fake.err != nil {
		return "", fake.err
	}
	if fake.decision == "" {
		return authorize.DecisionAllow, nil
	}
	return fake.decision, nil
}

type credentialAuthority struct {
	mu    sync.Mutex
	err   error
	deny  authorize.AuthorityCheck
	calls int
	reads []authorize.AuthorityRead
}

func (fake *credentialAuthority) Read(_ context.Context, reads []authorize.AuthorityRead) ([]authorize.AuthorityFact, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.calls++
	fake.reads = append(fake.reads, reads...)
	if fake.err != nil {
		return nil, fake.err
	}
	facts := make([]authorize.AuthorityFact, len(reads))
	for index, read := range reads {
		facts[index] = authorize.AuthorityFact{
			Check:       read.Check,
			Scope:       read.Scope,
			PrincipalID: read.PrincipalID,
			TargetID:    read.TargetID,
			Version:     read.ExpectedVersion,
			Allowed:     read.Check != fake.deny,
		}
	}
	return facts, nil
}

type credentialIDs struct{ next int }

func (source *credentialIDs) NewEventID() (id.EventID, error) {
	source.next++
	return id.EventID(fmt.Sprintf("event-%d", source.next)), nil
}

func (source *credentialIDs) NewReceiptID() (id.ReceiptID, error) {
	source.next++
	return id.ReceiptID(fmt.Sprintf("receipt-%d", source.next)), nil
}

var (
	_ token.Clock    = (*fakeClock)(nil)
	_ token.IDSource = (*credentialIDs)(nil)
)

type credentialFixture struct {
	env        *testEnv
	authorizer *credentialAuthorizer
	authority  *credentialAuthority
	admin      *token.CredentialAdmin
}

func newCredentialFixture(t *testing.T) *credentialFixture {
	t.Helper()
	env := newTestEnv(t)
	env.principals.principal = credentialActor(env.scope)
	env.creds.fact = serviceCredentialFact(env, false)
	fixture := &credentialFixture{
		env:        env,
		authorizer: &credentialAuthorizer{},
		authority:  &credentialAuthority{},
	}
	fixture.rebuild(t, env.manager)
	return fixture
}

func (fixture *credentialFixture) rebuild(t *testing.T, manager token.CredentialManager) {
	t.Helper()
	admin, err := token.NewCredentialAdmin(token.CredentialAdminConfig{
		Authorizer:  fixture.authorizer,
		Authority:   fixture.authority,
		Principals:  fixture.env.principals,
		Services:    fixture.env.services,
		Credentials: fixture.env.creds,
		Manager:     manager,
		Receipts:    fixture.env.receipts,
		Clock:       fixture.env.clock,
		IDs:         &credentialIDs{},
	})
	if err != nil {
		t.Fatalf("NewCredentialAdmin: %v", err)
	}
	fixture.admin = admin
}

func credentialActor(scope tenancy.Scope) authorize.Principal {
	return authorize.Principal{
		ID:      id.PrincipalID("principal-admin"),
		Kind:    identity.KindHuman,
		Scope:   scope,
		Roles:   []tenancy.Role{tenancy.RoleAdmin},
		Enabled: true,
		Version: 1,
	}
}

func serviceCredentialFact(env *testEnv, present bool) token.CredentialFact {
	fact := token.CredentialFact{
		Scope:       env.scope,
		PrincipalID: id.PrincipalID(env.serviceID),
		ServiceID:   env.serviceID,
		Enabled:     true,
		Version:     3,
	}
	if present {
		fact.Credential = token.Credential{Nonce: testNonce(), Expiry: env.now.Add(time.Hour)}
	}
	return fact
}

func credentialCommand(env *testEnv) token.ServiceCredentialCommand {
	return token.ServiceCredentialCommand{
		Actor:           credentialActor(env.scope),
		Scope:           env.scope,
		ServiceID:       env.serviceID,
		TTL:             time.Hour,
		ExpectedVersion: 3,
		CallerKey:       "caller-1",
		Content:         receipt.Content{MediaType: "application/json", Bytes: []byte(`{"ttl":"1h"}`)},
	}
}

func revokeCommand(env *testEnv) token.ServiceCredentialRevokeCommand {
	return token.ServiceCredentialRevokeCommand{
		Actor:           credentialActor(env.scope),
		Scope:           env.scope,
		ServiceID:       env.serviceID,
		ExpectedVersion: 3,
		CallerKey:       "caller-1",
		Content:         receipt.Content{MediaType: "application/json", Bytes: []byte(`{}`)},
	}
}

func requireCredentialClass(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil; want %s", want)
	}
	class, ok := fault.ClassOf(err)
	if !ok || class != want {
		t.Fatalf("error class = %v (ok=%v); want %s (err=%v)", class, ok, want, err)
	}
}

func TestCredentialAdminIssueCommitsCompleteUnit(t *testing.T) {
	fixture := newCredentialFixture(t)
	result, err := fixture.admin.Issue(context.Background(), credentialCommand(fixture.env))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if result.Token == "" || result.Replayed || result.Delivery != nil || result.Delivered {
		t.Fatalf("result = %+v; want one fresh token and no delivery", result)
	}
	units := fixture.env.mutations.units
	if len(units) != 1 {
		t.Fatalf("mutation units = %d; want 1", len(units))
	}
	unit := units[0]
	if unit.Owner != fixture.env.scope || unit.PrincipalID != id.PrincipalID(fixture.env.serviceID) || unit.ServiceID != fixture.env.serviceID {
		t.Fatalf("unit identity = %+v", unit)
	}
	if unit.Kind != token.MutationIssue || unit.ExpectedVersion != 3 {
		t.Fatalf("kind/version = %q/%d; want issue/3", unit.Kind, unit.ExpectedVersion)
	}
	if len(unit.Credential.Nonce) < token.MinNonceBytes || !unit.Credential.Expiry.Equal(fixture.env.now.Add(time.Hour)) {
		t.Fatalf("credential = %+v; want a fresh 256-bit nonce and 1h expiry", unit.Credential)
	}
	if unit.Receipt.Key.Operation != "service.credential.issue" || unit.Receipt.Key.CallerKey != "caller-1" {
		t.Fatalf("receipt key = %+v", unit.Receipt.Key)
	}
	if unit.Receipt.Key.PrincipalID != id.PrincipalID("principal-admin") || unit.Receipt.ContentHash == "" {
		t.Fatalf("receipt = %+v; want the caller principal and content hash", unit.Receipt)
	}
	if len(unit.Audit) != 1 || unit.Audit[0].Operation != "service.credential.issue" || unit.Audit[0].Detail != "" {
		t.Fatalf("audit = %+v", unit.Audit)
	}
	if len(unit.Events) != 1 {
		t.Fatalf("events = %+v; want one service.changed envelope", unit.Events)
	}
	changed := unit.Events[0]
	if changed.Type != event.TypeServiceChanged || changed.SubjectID != string(fixture.env.serviceID) || changed.SubjectVersion != 4 {
		t.Fatalf("event = %+v; want service.changed v4", changed)
	}
	if len(changed.Data) != 0 {
		t.Fatalf("event carries data %q", changed.Data)
	}
	if !credentialReadHeld(unit.Authority, "service.credential.issue", authorize.CheckMembership) ||
		!credentialReadHeld(unit.Authority, "service.credential.issue", authorize.CheckServiceEnabled) {
		t.Fatalf("authority reads = %+v; want membership and service_enabled", unit.Authority)
	}
	if len(unit.PostCommit) != 0 {
		t.Fatalf("service credential post-commit = %+v; want none (never mounted)", unit.PostCommit)
	}
	nonceHex := hex.EncodeToString(unit.Credential.Nonce)
	for name, body := range map[string][]byte{
		"receipt": unit.Receipt.Result,
		"event":   changed.Data,
	} {
		if strings.Contains(string(body), nonceHex) || strings.Contains(string(body), result.Token) {
			t.Fatalf("%s leaked credential material: %s", name, body)
		}
	}
	if strings.Contains(unit.Audit[0].Detail, nonceHex) {
		t.Fatalf("audit leaked credential material: %q", unit.Audit[0].Detail)
	}
}

func credentialReadHeld(reads []authorize.AuthorityRead, operation authorize.OperationName, check authorize.AuthorityCheck) bool {
	for _, read := range reads {
		if read.Operation == operation && read.Check == check {
			return true
		}
	}
	return false
}

func TestCredentialAdminRefusesWithoutMutation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*credentialFixture)
		call   func(*credentialFixture) error
		want   fault.Class
	}{
		{
			name: "authorizer denies",
			mutate: func(fixture *credentialFixture) {
				fixture.authorizer.decision = authorize.DecisionDeny
			},
			want: fault.Forbidden,
		},
		{
			name: "service principal",
			mutate: func(fixture *credentialFixture) {
			},
			call: func(fixture *credentialFixture) error {
				command := credentialCommand(fixture.env)
				command.Actor = authorize.Principal{
					ID: id.PrincipalID("principal-svc"), Kind: identity.KindService,
					Scope: fixture.env.scope, Roles: []tenancy.Role{tenancy.RoleEngineer}, Enabled: true, Version: 1,
				}
				_, err := fixture.admin.Issue(context.Background(), command)
				return err
			},
			want: fault.Forbidden,
		},
		{
			name: "execution principal",
			mutate: func(fixture *credentialFixture) {
			},
			call: func(fixture *credentialFixture) error {
				command := credentialCommand(fixture.env)
				command.Actor = authorize.Principal{
					ID: id.PrincipalID("principal-exec"), Kind: identity.KindExecution,
					Scope: fixture.env.scope, ExecutionID: id.ExecutionID("exec-1"), Enabled: true, Version: 1,
				}
				_, err := fixture.admin.Issue(context.Background(), command)
				return err
			},
			want: fault.Forbidden,
		},
		{
			name: "human without admin",
			mutate: func(fixture *credentialFixture) {
				principal := credentialActor(fixture.env.scope)
				principal.Roles = []tenancy.Role{tenancy.RoleEngineer}
				fixture.env.principals.principal = principal
			},
			want: fault.Forbidden,
		},
		{
			name: "disabled principal",
			mutate: func(fixture *credentialFixture) {
				principal := credentialActor(fixture.env.scope)
				principal.Enabled = false
				fixture.env.principals.principal = principal
			},
			want: fault.Unauthenticated,
		},
		{
			name: "cross-scope actor",
			mutate: func(fixture *credentialFixture) {
			},
			call: func(fixture *credentialFixture) error {
				command := credentialCommand(fixture.env)
				command.Actor.Scope = tenancy.Scope{TenantID: id.TenantID("tenant-b"), ProjectID: id.ProjectID("project-2")}
				_, err := fixture.admin.Issue(context.Background(), command)
				return err
			},
			want: fault.Forbidden,
		},
		{
			name: "tenant-only command scope",
			mutate: func(fixture *credentialFixture) {
			},
			call: func(fixture *credentialFixture) error {
				command := credentialCommand(fixture.env)
				command.Scope = tenancy.Scope{TenantID: fixture.env.scope.TenantID}
				command.Actor.Scope = command.Scope
				_, err := fixture.admin.Issue(context.Background(), command)
				return err
			},
			want: fault.InvalidInput,
		},
		{
			name: "disabled target service",
			mutate: func(fixture *credentialFixture) {
				fixture.env.services.service.State = service.StateDisabled
				fixture.env.services.service.TokenNonce = nil
				fixture.env.services.service.TokenExpiry = nil
			},
			want: fault.Forbidden,
		},
		{
			name: "target service in another scope",
			mutate: func(fixture *credentialFixture) {
				fixture.env.services.service.Scope = tenancy.Scope{TenantID: id.TenantID("tenant-b"), ProjectID: id.ProjectID("project-2")}
			},
			want: fault.Forbidden,
		},
		{
			name: "issue with an existing credential",
			mutate: func(fixture *credentialFixture) {
				fixture.env.creds.fact = serviceCredentialFact(fixture.env, true)
			},
			want: fault.InvalidInput,
		},
		{
			name: "rotate without a credential",
			mutate: func(fixture *credentialFixture) {
				fixture.env.creds.fact = serviceCredentialFact(fixture.env, false)
			},
			call: func(fixture *credentialFixture) error {
				_, err := fixture.admin.Rotate(context.Background(), credentialCommand(fixture.env))
				return err
			},
			want: fault.Unauthenticated,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newCredentialFixture(t)
			testCase.mutate(fixture)
			var err error
			if testCase.call != nil {
				err = testCase.call(fixture)
			} else {
				_, err = fixture.admin.Issue(context.Background(), credentialCommand(fixture.env))
			}
			requireCredentialClass(t, err, testCase.want)
			if len(fixture.env.mutations.units) != 0 {
				t.Fatalf("mutation units = %d; want 0", len(fixture.env.mutations.units))
			}
		})
	}
}

// credentialReplayPort keys idempotency on the receipt identity and content
// fingerprint, like the store's ExecuteUnit. It reports an equal retry as a
// replay and refuses changed content.
type credentialReplayPort struct {
	mu        sync.Mutex
	hashes    map[receipt.Key]string
	committed map[receipt.Key]token.Credential
	units     []token.MutationUnit
	commits   int
	replays   int
}

func (port *credentialReplayPort) Mutate(_ context.Context, unit token.MutationUnit) (token.MutationResult, error) {
	port.mu.Lock()
	defer port.mu.Unlock()
	port.units = append(port.units, unit)
	if hash, ok := port.hashes[unit.Receipt.Key]; ok {
		if hash != unit.Receipt.ContentHash {
			return token.MutationResult{}, fault.New(fault.ChangedIdempotencyContent, "changed idempotency content")
		}
		port.replays++
		return token.MutationResult{State: token.StateCurrent, Version: 4, Replayed: true}, nil
	}
	port.hashes[unit.Receipt.Key] = unit.Receipt.ContentHash
	port.committed[unit.Receipt.Key] = unit.Credential
	port.commits++
	return token.MutationResult{State: token.StateCurrent, Version: 4}, nil
}

func TestCredentialAdminRotateReplayHasNoToken(t *testing.T) {
	fixture := newCredentialFixture(t)
	fixture.env.creds.fact = serviceCredentialFact(fixture.env, true)
	port := &credentialReplayPort{hashes: map[receipt.Key]string{}, committed: map[receipt.Key]token.Credential{}}
	fixture.rebuild(t, newManager(t, fixture.env, port))

	command := credentialCommand(fixture.env)
	first, err := fixture.admin.Rotate(context.Background(), command)
	if err != nil {
		t.Fatalf("first Rotate: %v", err)
	}
	if first.Token == "" || first.Replayed {
		t.Fatalf("first result = %+v; want a fresh token", first)
	}
	second, err := fixture.admin.Rotate(context.Background(), command)
	if err != nil {
		t.Fatalf("replayed Rotate: %v", err)
	}
	if second.Token != "" || !second.Replayed || second.Delivery != nil {
		t.Fatalf("replay result = %+v; a replay must not return or deliver a token", second)
	}
	if port.commits != 1 || port.replays != 1 {
		t.Fatalf("commits/replays = %d/%d; want 1/1", port.commits, port.replays)
	}

	changed := command
	changed.Content = receipt.Content{MediaType: "application/json", Bytes: []byte(`{"ttl":"2h"}`)}
	_, err = fixture.admin.Rotate(context.Background(), changed)
	requireCredentialClass(t, err, fault.ChangedIdempotencyContent)
}

func TestCredentialAdminRevokeClearsWithoutSecret(t *testing.T) {
	fixture := newCredentialFixture(t)
	fixture.env.creds.fact = serviceCredentialFact(fixture.env, true)
	result, err := fixture.admin.Revoke(context.Background(), revokeCommand(fixture.env))
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if !result.Invalidated || result.Mutation.State != token.StateAbsent || result.Mutation.Version != 4 {
		t.Fatalf("result = %+v; want an invalidated absent credential", result)
	}
	units := fixture.env.mutations.units
	if len(units) != 1 {
		t.Fatalf("mutation units = %d; want 1", len(units))
	}
	unit := units[0]
	if unit.Kind != token.MutationRevoke || unit.ServiceID != fixture.env.serviceID || unit.ExpectedVersion != 3 {
		t.Fatalf("unit = %+v; want service revoke at version 3", unit)
	}
	if len(unit.Credential.Nonce) != 0 || !unit.Credential.Expiry.IsZero() {
		t.Fatalf("revoke unit carries a credential: %+v", unit.Credential)
	}
	if unit.Receipt.Key.Operation != "service.credential.revoke" || unit.Receipt.Key.CallerKey != "caller-1" {
		t.Fatalf("receipt key = %+v", unit.Receipt.Key)
	}
	if len(unit.Audit) != 1 || unit.Audit[0].Operation != "service.credential.revoke" {
		t.Fatalf("audit = %+v", unit.Audit)
	}
	if len(unit.Events) != 1 || unit.Events[0].Type != event.TypeServiceChanged || unit.Events[0].SubjectVersion != 4 {
		t.Fatalf("event = %+v; want service.changed v4", unit.Events)
	}
	if len(fixture.env.invalidator.invalidations) != 1 {
		t.Fatalf("invalidations = %+v; want one revoked invalidation", fixture.env.invalidator.invalidations)
	}
	invalidation := fixture.env.invalidator.invalidations[0]
	if invalidation.Reason != token.InvalidationRevoked || invalidation.ServiceID != fixture.env.serviceID || invalidation.Scope != fixture.env.scope {
		t.Fatalf("invalidation = %+v", invalidation)
	}
	nonceHex := hex.EncodeToString(serviceCredentialFact(fixture.env, true).Credential.Nonce)
	for name, body := range map[string][]byte{"receipt": unit.Receipt.Result, "event": unit.Events[0].Data} {
		if strings.Contains(string(body), nonceHex) {
			t.Fatalf("%s leaked the stored nonce: %s", name, body)
		}
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if strings.Contains(string(encoded), nonceHex) {
		t.Fatalf("revoke result leaked the stored nonce: %s", encoded)
	}
}

func TestCredentialAdminStaleVersionConflicts(t *testing.T) {
	fixture := newCredentialFixture(t)
	fixture.env.services.service.Version = 3
	command := credentialCommand(fixture.env)
	command.ExpectedVersion = 2
	_, err := fixture.admin.Issue(context.Background(), command)
	requireCredentialClass(t, err, fault.VersionConflict)
	if len(fixture.env.mutations.units) != 0 {
		t.Fatalf("mutation units = %d; a stale command must not reach the mutation port", len(fixture.env.mutations.units))
	}
}

func TestCredentialAdminDisabledTargetRefusalCarriesNoSecret(t *testing.T) {
	fixture := newCredentialFixture(t)
	fixture.env.creds.fact = serviceCredentialFact(fixture.env, true)
	fixture.env.services.service.State = service.StateDisabled
	fixture.env.services.service.TokenNonce = nil
	fixture.env.services.service.TokenExpiry = nil

	_, err := fixture.admin.Issue(context.Background(), credentialCommand(fixture.env))
	requireCredentialClass(t, err, fault.Forbidden)
	nonceHex := hex.EncodeToString(serviceCredentialFact(fixture.env, true).Credential.Nonce)
	if strings.Contains(err.Error(), nonceHex) {
		t.Fatalf("error leaked the stored nonce: %v", err)
	}
}

// TestCredentialAdminEnableDoesNotRestoreCredential proves a service enabled
// after a disablement has no credential to rotate: the enablement path carries
// no pair, so only an explicit issue can create one.
func TestCredentialAdminEnableDoesNotRestoreCredential(t *testing.T) {
	fixture := newCredentialFixture(t)
	fixture.env.services.service.State = service.StateEnabled
	fixture.env.creds.fact = serviceCredentialFact(fixture.env, false)

	_, err := fixture.admin.Rotate(context.Background(), credentialCommand(fixture.env))
	requireCredentialClass(t, err, fault.Unauthenticated)
	if len(fixture.env.mutations.units) != 0 {
		t.Fatalf("mutation units = %d; want 0", len(fixture.env.mutations.units))
	}
}
