// Credential-lifecycle API pinned by these tests.
//
// The declared commit unit is token.MutationUnit. This file pins the
// orchestration around it:
//
//	type DerivationKeyResolver interface {
//	    ResolveDerivationKey(context.Context, TenantKey) ([]byte, error)
//	}
//	type Clock interface { Now() time.Time }
//
//	type Delivery struct {
//	    Scope       tenancy.Scope
//	    PrincipalID id.PrincipalID
//	    ExecutionID id.ExecutionID
//	    ServiceID   service.ServiceID
//	    Kind        MutationKind
//	    Token       string
//	    Target      string
//	    Refresh     bool
//	}
//	func (Delivery) Validate() error
//	type Deliverer interface { Deliver(context.Context, Delivery) error }
//
//	type ManagerConfig struct {
//	    Keys          TenantKeyReader
//	    Resolver      DerivationKeyResolver
//	    Credentials   CredentialReader
//	    Principals    identity.PrincipalReader
//	    Services      service.ServiceReader
//	    Mutations     MutationPort
//	    Receipts      ReceiptLookup
//	    Deliverer     Deliverer
//	    Invalidations InvalidationPort
//	    Clock         Clock
//	    Random        io.Reader
//	    Bounds        Bounds
//	}
//	func NewManager(ManagerConfig) (*Manager, error)
//
//	type IssueRequest struct {
//	    Scope           tenancy.Scope
//	    PrincipalID     id.PrincipalID
//	    ExecutionID     id.ExecutionID
//	    ServiceID       service.ServiceID
//	    TTL             time.Duration
//	    Deadline        time.Time
//	    ExpectedVersion uint64
//	    Service         *ServiceChange
//	    Receipt         receipt.Record
//	    Audit           []event.Audit
//	    Events          []event.Envelope
//	    Authority       []authorize.AuthorityRead
//	}
//	func (IssueRequest) Validate() error
//	type IssueResult struct {
//	    State     State
//	    Version   uint64
//	    Replayed  bool
//	    Token     string
//	    Delivery  *Delivery
//	    Delivered bool
//	}
//	func (*Manager) Issue(context.Context, IssueRequest) (IssueResult, error)
//	func (*Manager) Rotate(context.Context, IssueRequest) (IssueResult, error)
//
//	type EnsureResult struct {
//	    State     State
//	    Version   uint64
//	    Reused    bool
//	    Token     string
//	    Delivery  *Delivery
//	    Delivered bool
//	}
//	func (*Manager) EnsureExecution(context.Context, IssueRequest) (EnsureResult, error)
//
//	type RevokeRequest struct {
//	    Scope           tenancy.Scope
//	    PrincipalID     id.PrincipalID
//	    ExecutionID     id.ExecutionID
//	    ServiceID       service.ServiceID
//	    ExpectedVersion uint64
//	    Reason          InvalidationReason
//	    Receipt         receipt.Record
//	    Audit           []event.Audit
//	    Events          []event.Envelope
//	    Authority       []authorize.AuthorityRead
//	}
//	func (RevokeRequest) Validate() error
//	type RevokeResult struct {
//	    Mutation     MutationResult
//	    Invalidation Invalidation
//	    Invalidated  bool
//	}
//	func (*Manager) Revoke(context.Context, RevokeRequest) (RevokeResult, error)
//
// Rules pinned by these tests (S04 §2, S05 §3):
//   - A credential mutation commits through MutationPort before any delivery;
//     a failed commit produces no token and no delivery. A committed mutation
//     with a failed delivery returns the same token and a retryable Delivery
//     without mutating again or restarting work. A replayed commit (an equal
//     receipt already committed by a lost-response attempt) returns the
//     committed state/version with no token and no delivery because the nonce
//     derived by the retry was never committed.
//   - Every operation re-reads current principal/service state and the current
//     tenant key; a stale expected version, disabled principal/service or
//     cross-scope read is refused before the mutation. Issuing or rotating a
//     credential while disabling the service is refused as invalid input;
//     disablement is the clearing path with an empty credential pair and the
//     declared disabled invalidation reason.
//   - Execution TTL is within [1 minute, 8 hours] and never exceeds the action
//     deadline. A current execution credential is reused without mutation.
//   - Revocation clears the pair atomically and only then invalidates live
//     connections. A failed mutation never invalidates.
//   - The commit unit and public results never carry a token hash or token
//     plaintext; plaintext exists only on the Delivery returned for authorized
//     delivery.
package token_test

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

func sampleScope() tenancy.Scope {
	tenantID, _, _ := testScope()
	return tenancy.Scope{TenantID: tenantID, ProjectID: id.ProjectID("project-1")}
}

func sampleReceipt(scope tenancy.Scope, principal id.PrincipalID, operation string, now time.Time) receipt.Record {
	return receipt.Record{
		ID: id.ReceiptID("receipt-1"),
		Key: receipt.Key{
			TenantID:    scope.TenantID,
			ProjectID:   scope.ProjectID,
			PrincipalID: principal,
			Operation:   operation,
			CallerKey:   "caller-key-1",
		},
		At: now,
	}
}

func sampleAudit(scope tenancy.Scope, principal id.PrincipalID, operation string, now time.Time) []event.Audit {
	return []event.Audit{{
		ID:        id.EventID("audit-1"),
		Scope:     scope,
		Actor:     principal,
		Operation: operation,
		At:        now,
	}}
}

func sampleEvents(scope tenancy.Scope, principal id.PrincipalID, subject string, now time.Time) []event.Envelope {
	return []event.Envelope{{
		ID:             id.EventID("event-1"),
		Type:           event.TypeServiceChanged,
		Source:         event.Source{Kind: event.SourceShell, PrincipalID: principal},
		At:             now,
		Scope:          scope,
		SubjectID:      subject,
		SubjectVersion: 3,
	}}
}

func sampleAuthority(scope tenancy.Scope, principal id.PrincipalID, target string) []authorize.AuthorityRead {
	return []authorize.AuthorityRead{{
		Operation:       authorize.OperationName("service.credential.rotate"),
		Check:           authorize.CheckMembership,
		Scope:           scope,
		PrincipalID:     principal,
		TargetID:        target,
		ExpectedVersion: 3,
	}}
}

func executionIssueRequest(env *testEnv) token.IssueRequest {
	return token.IssueRequest{
		Scope:           env.scope,
		PrincipalID:     env.principalID,
		ExecutionID:     env.executionID,
		TTL:             time.Hour,
		ExpectedVersion: 3,
		Receipt:         sampleReceipt(env.scope, env.principalID, "service.credential.issue", env.now),
		Audit:           sampleAudit(env.scope, env.principalID, "service.credential.issue", env.now),
		Events:          sampleEvents(env.scope, env.principalID, string(env.executionID), env.now),
		Authority:       sampleAuthority(env.scope, env.principalID, string(env.executionID)),
	}
}

func serviceIssueRequest(env *testEnv) token.IssueRequest {
	return token.IssueRequest{
		Scope:           env.scope,
		PrincipalID:     env.principalID,
		ServiceID:       env.serviceID,
		TTL:             time.Hour,
		ExpectedVersion: 3,
		Receipt:         sampleReceipt(env.scope, env.principalID, "service.credential.issue", env.now),
		Audit:           sampleAudit(env.scope, env.principalID, "service.credential.issue", env.now),
		Events:          sampleEvents(env.scope, env.principalID, string(env.serviceID), env.now),
		Authority:       sampleAuthority(env.scope, env.principalID, string(env.serviceID)),
	}
}

func revokeRequest(env *testEnv) token.RevokeRequest {
	return token.RevokeRequest{
		Scope:           env.scope,
		PrincipalID:     env.principalID,
		ExecutionID:     env.executionID,
		ExpectedVersion: 3,
		Reason:          token.InvalidationRevoked,
		Receipt:         sampleReceipt(env.scope, env.principalID, "service.credential.revoke", env.now),
		Audit:           sampleAudit(env.scope, env.principalID, "service.credential.revoke", env.now),
		Events:          sampleEvents(env.scope, env.principalID, string(env.executionID), env.now),
		Authority:       sampleAuthority(env.scope, env.principalID, string(env.executionID)),
	}
}

// TestManagerIssueExecutionCommitsBeforeDelivery pins the ordering: the
// complete unit is committed first, then the plaintext token is delivered once.
func TestManagerIssueExecutionCommitsBeforeDelivery(t *testing.T) {
	env := newTestEnv(t)
	env.setAbsentCredential()

	result, err := env.manager.Issue(context.Background(), executionIssueRequest(env))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if len(env.mutations.units) != 1 {
		t.Fatalf("mutation calls = %d; want 1", len(env.mutations.units))
	}
	unit := env.mutations.units[0]
	if unit.Kind != token.MutationIssue {
		t.Fatalf("mutation kind = %q; want issue", unit.Kind)
	}
	if unit.Owner != env.scope || unit.PrincipalID != env.principalID || unit.ExecutionID != env.executionID {
		t.Fatalf("unit target = %+v; want execution in scope", unit)
	}
	if unit.ExpectedVersion != 3 {
		t.Fatalf("ExpectedVersion = %d; want 3", unit.ExpectedVersion)
	}
	if !unit.Credential.Present() || len(unit.Credential.Nonce) < token.MinNonceBytes {
		t.Fatalf("unit credential = %+v; want a complete pair", unit.Credential)
	}
	if unit.Receipt.ID != "receipt-1" || len(unit.Audit) != 1 || len(unit.Events) != 1 || len(unit.Authority) != 1 {
		t.Fatalf("unit lost commit payload: %+v", unit)
	}
	if !hasPostCommit(unit.PostCommit, event.PostDeliverCredential) {
		t.Fatalf("post-commit = %v; want deliver_credential", unit.PostCommit)
	}
	if result.Delivered != true || result.Delivery == nil {
		t.Fatalf("result = %+v; want a delivered credential", result)
	}
	if result.Token == "" || result.Token != result.Delivery.Token {
		t.Fatalf("result token/delivery token mismatch: %+v", result)
	}
	if result.Delivery.ExecutionID != env.executionID || result.Delivery.Kind != token.MutationIssue {
		t.Fatalf("delivery = %+v; want execution issue", result.Delivery)
	}
	if len(env.deliverer.deliveries) != 1 || env.deliverer.deliveries[0].Token != result.Token {
		t.Fatalf("deliveries = %+v; want exactly the committed token", env.deliverer.deliveries)
	}
	if strings.Contains(result.Token, "=") {
		t.Fatalf("token = %q; base64url must be unpadded", result.Token)
	}
	if !strings.HasPrefix(result.Token, token.ExecutionPrefix) {
		t.Fatalf("token = %q; want execution prefix", result.Token)
	}
}

// TestManagerIssueDoesNotDeliverWhenMutationFails proves a crash/failure before
// commit yields no token and no delivery, and the same operation is safe to
// retry without a blind delivery.
func TestManagerIssueDoesNotDeliverWhenMutationFails(t *testing.T) {
	env := newTestEnv(t)
	env.setAbsentCredential()
	env.mutations.err = fault.New(fault.DependencyUnavailable, "store unavailable")

	result, err := env.manager.Issue(context.Background(), executionIssueRequest(env))
	if err == nil {
		t.Fatal("Issue succeeded despite a failed commit")
	}
	if result.Token != "" || result.Delivery != nil || result.Delivered {
		t.Fatalf("failed commit returned delivery material: %+v", result)
	}
	if len(env.deliverer.deliveries) != 0 {
		t.Fatalf("delivery happened before commit: %+v", env.deliverer.deliveries)
	}
}

// TestManagerIssueReturnsRetryableDeliveryWhenDeliveryFails proves a lost
// response after commit is recoverable: the token and the same delivery command
// are returned, and retrying delivery does not mutate again.
func TestManagerIssueReturnsRetryableDeliveryWhenDeliveryFails(t *testing.T) {
	env := newTestEnv(t)
	env.setAbsentCredential()
	env.deliverer.err = fault.New(fault.UncertainExternalEffect, "mount update lost")

	result, err := env.manager.Issue(context.Background(), executionIssueRequest(env))
	if err == nil {
		t.Fatal("Issue hid a delivery failure")
	}
	if result.Token == "" || result.Delivery == nil || result.Delivered {
		t.Fatalf("result = %+v; want committed token plus retryable delivery", result)
	}
	if len(env.mutations.units) != 1 {
		t.Fatalf("mutation calls = %d; want 1", len(env.mutations.units))
	}

	// Retrying the returned command is a delivery-only operation: it does not
	// invoke the mutation port and must not restart any workload.
	env.deliverer.err = nil
	if err := env.deliverer.Deliver(context.Background(), *result.Delivery); err != nil {
		t.Fatalf("retry delivery: %v", err)
	}
	if len(env.mutations.units) != 1 {
		t.Fatalf("delivery retry mutated again: %d units", len(env.mutations.units))
	}
	if len(env.deliverer.deliveries) != 2 {
		t.Fatalf("deliveries = %d; want the failed attempt plus retry", len(env.deliverer.deliveries))
	}
}

// TestManagerReplayReturnsCommittedResultWithoutToken proves a replayed commit
// (an equal receipt already committed by an earlier attempt whose response was
// lost) reports the committed state/version with no token and no delivery. The
// nonce derived by the replay was never committed, so handing it out would
// corrupt the mount with a credential that cannot authenticate.
func TestManagerReplayReturnsCommittedResultWithoutToken(t *testing.T) {
	type outcome struct {
		state     token.State
		version   uint64
		replayed  bool
		reused    bool
		token     string
		delivery  *token.Delivery
		delivered bool
	}
	executionOutcome := func(result token.IssueResult, err error) (outcome, error) {
		return outcome{
			state:     result.State,
			version:   result.Version,
			replayed:  result.Replayed,
			token:     result.Token,
			delivery:  result.Delivery,
			delivered: result.Delivered,
		}, err
	}

	cases := []struct {
		name    string
		ensure  bool
		prepare func(*testEnv)
		run     func(*testEnv) (outcome, error)
	}{
		{
			name:    "issue execution",
			prepare: func(env *testEnv) { env.setAbsentCredential() },
			run: func(env *testEnv) (outcome, error) {
				return executionOutcome(env.manager.Issue(context.Background(), executionIssueRequest(env)))
			},
		},
		{
			name:    "issue service",
			prepare: func(env *testEnv) { env.setServiceCredentialAbsent() },
			run: func(env *testEnv) (outcome, error) {
				return executionOutcome(env.manager.Issue(context.Background(), serviceIssueRequest(env)))
			},
		},
		{
			name: "rotate execution",
			run: func(env *testEnv) (outcome, error) {
				return executionOutcome(env.manager.Rotate(context.Background(), executionIssueRequest(env)))
			},
		},
		{
			name: "rotate service",
			prepare: func(env *testEnv) {
				env.setServiceCredentialAbsent()
				env.creds.fact.Credential = token.Credential{Nonce: testNonce(), Expiry: env.now.Add(time.Hour)}
			},
			run: func(env *testEnv) (outcome, error) {
				return executionOutcome(env.manager.Rotate(context.Background(), serviceIssueRequest(env)))
			},
		},
		{
			name:    "ensure execution",
			ensure:  true,
			prepare: func(env *testEnv) { env.setAbsentCredential() },
			run: func(env *testEnv) (outcome, error) {
				result, err := env.manager.EnsureExecution(context.Background(), executionIssueRequest(env))
				return outcome{
					state:     result.State,
					version:   result.Version,
					reused:    result.Reused,
					token:     result.Token,
					delivery:  result.Delivery,
					delivered: result.Delivered,
				}, err
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			env := newTestEnv(t)
			if testCase.prepare != nil {
				testCase.prepare(env)
			}
			env.mutations.result = token.MutationResult{State: token.StateCurrent, Version: 4, Replayed: true}

			got, err := testCase.run(env)
			if err != nil {
				t.Fatalf("replayed mutation: %v", err)
			}
			if testCase.ensure {
				if !got.reused {
					t.Fatal("EnsureExecution did not report the replayed commit as reused")
				}
			} else if !got.replayed {
				t.Fatal("result did not report the replayed commit")
			}
			if got.state != token.StateCurrent || got.version != 4 {
				t.Fatalf("result state/version = %q/%d; want current/4", got.state, got.version)
			}
			if got.token != "" || got.delivery != nil || got.delivered {
				t.Fatalf("replayed commit returned uncommitted delivery material: %+v", got)
			}
			if len(env.deliverer.deliveries) != 0 {
				t.Fatalf("replayed commit delivered a token: %+v", env.deliverer.deliveries)
			}
			if len(env.mutations.units) != 1 {
				t.Fatalf("mutation calls = %d; want the single replayed attempt", len(env.mutations.units))
			}
		})
	}
}

// TestManagerNonReplayedCommitDeliversToken is the control for the replay
// behavior: a genuine first commit still returns and delivers its token.
func TestManagerNonReplayedCommitDeliversToken(t *testing.T) {
	env := newTestEnv(t)
	env.setAbsentCredential()

	result, err := env.manager.Issue(context.Background(), executionIssueRequest(env))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if result.Replayed {
		t.Fatal("first commit reported as replayed")
	}
	if result.Token == "" || result.Delivery == nil || !result.Delivered {
		t.Fatalf("result = %+v; want a delivered token", result)
	}
	if len(env.deliverer.deliveries) != 1 {
		t.Fatalf("deliveries = %d; want 1", len(env.deliverer.deliveries))
	}
}

// TestManagerSerializedRetryAfterLostDeliveryResponseNeverDeliversUncommittedToken
// pins the recovery path end to end: the first attempt commits and loses its
// delivery response, the caller retries the same command, and the store replays
// the committed receipt. The retry must return no token and must not deliver:
// its freshly derived nonce was never committed.
func TestManagerSerializedRetryAfterLostDeliveryResponseNeverDeliversUncommittedToken(t *testing.T) {
	env := newTestEnv(t)
	env.setAbsentCredential()
	replaying := &replayingMutations{committed: map[receipt.Key]token.Credential{}}
	env.manager = newManager(t, env, replaying)
	env.deliverer.err = fault.New(fault.UncertainExternalEffect, "mount update lost")

	first, err := env.manager.Issue(context.Background(), executionIssueRequest(env))
	if err == nil {
		t.Fatal("first Issue hid the lost delivery response")
	}
	if first.Token == "" || first.Delivery == nil || first.Delivered {
		t.Fatalf("first result = %+v; want the committed token plus a retryable delivery", first)
	}
	if len(replaying.units) != 1 {
		t.Fatalf("first mutation calls = %d; want 1", len(replaying.units))
	}
	committed := replaying.units[0].Credential
	expected, err := token.DeriveExecutionToken(env.keyBytes, token.ExecutionClaims{
		TenantID:    env.tenantID,
		ExecutionID: env.executionID,
		Nonce:       committed.Nonce,
		Expiry:      committed.Expiry,
	})
	if err != nil {
		t.Fatalf("derive committed token: %v", err)
	}
	if first.Token != expected {
		t.Fatal("the first attempt delivered a token that does not match the committed nonce")
	}

	env.deliverer.err = nil
	second, err := env.manager.Issue(context.Background(), executionIssueRequest(env))
	if err != nil {
		t.Fatalf("replayed Issue: %v", err)
	}
	if !second.Replayed {
		t.Fatal("retry did not report the replayed commit")
	}
	if second.Token != "" || second.Delivery != nil || second.Delivered {
		t.Fatalf("retry delivered an uncommitted token: %+v", second)
	}
	if len(env.deliverer.deliveries) != 1 {
		t.Fatalf("deliveries = %d; want only the first failed attempt", len(env.deliverer.deliveries))
	}
	if len(replaying.units) != 2 {
		t.Fatalf("mutation calls = %d; want the first commit plus the replayed attempt", len(replaying.units))
	}
	if bytes.Equal(replaying.units[1].Credential.Nonce, committed.Nonce) {
		t.Fatal("retry reused the committed nonce instead of deriving a fresh uncommitted one")
	}
}

// TestManagerIssueServiceReturnsTokenWithoutDelivery pins the service path:
// the token is returned for secure delivery to the owner, not mounted, and the
// unit carries the service identity.
func TestManagerIssueServiceReturnsTokenWithoutDelivery(t *testing.T) {
	env := newTestEnv(t)
	env.setServiceCredentialAbsent()

	result, err := env.manager.Issue(context.Background(), serviceIssueRequest(env))
	if err != nil {
		t.Fatalf("Issue service: %v", err)
	}
	if result.Token == "" || !strings.HasPrefix(result.Token, token.ServicePrefix) {
		t.Fatalf("token = %q; want a service token", result.Token)
	}
	if result.Delivery != nil || result.Delivered {
		t.Fatalf("service issue attempted a mount delivery: %+v", result)
	}
	if len(env.deliverer.deliveries) != 0 {
		t.Fatalf("service issue used the execution deliverer: %+v", env.deliverer.deliveries)
	}
	if len(env.mutations.units) != 1 {
		t.Fatalf("mutation calls = %d; want 1", len(env.mutations.units))
	}
	unit := env.mutations.units[0]
	if unit.ServiceID != env.serviceID || unit.ExecutionID != "" {
		t.Fatalf("unit target = %+v; want service target", unit)
	}
	if unit.Kind != token.MutationIssue {
		t.Fatalf("kind = %q; want issue", unit.Kind)
	}
}

// TestManagerIssueCommitsEnabledServiceChange pins that an enabled state
// and/or role change rides in the same atomic credential mutation unit.
func TestManagerIssueCommitsEnabledServiceChange(t *testing.T) {
	enabled := service.StateEnabled
	cases := []struct {
		name   string
		change *token.ServiceChange
	}{
		{name: "enabled state", change: &token.ServiceChange{State: &enabled}},
		{name: "roles only", change: &token.ServiceChange{Roles: []tenancy.Role{tenancy.RoleViewer}}},
		{name: "enabled state and roles", change: &token.ServiceChange{State: &enabled, Roles: []tenancy.Role{tenancy.RoleEngineer}}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			env := newTestEnv(t)
			env.setServiceCredentialAbsent()
			request := serviceIssueRequest(env)
			request.Service = testCase.change

			result, err := env.manager.Issue(context.Background(), request)
			if err != nil {
				t.Fatalf("Issue with service change: %v", err)
			}
			if result.Token == "" {
				t.Fatal("Issue with an enabled service change returned no token")
			}
			if len(env.mutations.units) != 1 {
				t.Fatalf("mutation calls = %d; want 1", len(env.mutations.units))
			}
			unit := env.mutations.units[0]
			if unit.Service == nil {
				t.Fatal("mutation unit lost the service change")
			}
			if testCase.change.State != nil {
				if unit.Service.State == nil || *unit.Service.State != *testCase.change.State {
					t.Fatalf("service state = %v; want %q", unit.Service.State, *testCase.change.State)
				}
			} else if unit.Service.State != nil {
				t.Fatalf("service state = %v; want unchanged", unit.Service.State)
			}
			if testCase.change.Roles != nil && !reflect.DeepEqual(unit.Service.Roles, testCase.change.Roles) {
				t.Fatalf("service roles = %v; want %v", unit.Service.Roles, testCase.change.Roles)
			}
		})
	}
}

// TestManagerIssueAndRotateRefuseDisabledServiceChange proves issuing or
// rotating a credential while disabling the service is refused as invalid
// input before any commit: it would commit a non-empty pair that can never
// authenticate and leave a dormant credential behind. Disablement uses the
// clearing path instead.
func TestManagerIssueAndRotateRefuseDisabledServiceChange(t *testing.T) {
	disabled := service.StateDisabled
	operations := []struct {
		name string
		run  func(*testEnv, token.IssueRequest) (token.IssueResult, error)
	}{
		{name: "issue", run: func(env *testEnv, request token.IssueRequest) (token.IssueResult, error) {
			return env.manager.Issue(context.Background(), request)
		}},
		{name: "rotate", run: func(env *testEnv, request token.IssueRequest) (token.IssueResult, error) {
			return env.manager.Rotate(context.Background(), request)
		}},
	}

	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			env := newTestEnv(t)
			env.setServiceCredentialAbsent()
			env.creds.fact.Credential = token.Credential{Nonce: testNonce(), Expiry: env.now.Add(time.Hour)}
			request := serviceIssueRequest(env)
			request.Service = &token.ServiceChange{State: &disabled}

			result, err := operation.run(env, request)
			requireClass(t, err, fault.InvalidInput)
			if result.Token != "" || result.Delivery != nil || result.Delivered {
				t.Fatalf("refused disable returned credential material: %+v", result)
			}
			if len(env.mutations.units) != 0 {
				t.Fatalf("disabled service change reached the mutation port: %+v", env.mutations.units)
			}
			if len(env.deliverer.deliveries) != 0 {
				t.Fatalf("disabled service change delivered a token: %+v", env.deliverer.deliveries)
			}
		})
	}
}

// TestManagerRevokeDisabledServiceClearsCredential proves disablement uses the
// clearing path: the commit unit carries an empty credential pair and the
// post-commit invalidation names the declared disabled reason.
func TestManagerRevokeDisabledServiceClearsCredential(t *testing.T) {
	env := newTestEnv(t)
	request := token.RevokeRequest{
		Scope:           env.scope,
		PrincipalID:     env.principalID,
		ServiceID:       env.serviceID,
		ExpectedVersion: 3,
		Reason:          token.InvalidationDisabled,
		Receipt:         sampleReceipt(env.scope, env.principalID, "service.disable", env.now),
		Audit:           sampleAudit(env.scope, env.principalID, "service.disable", env.now),
		Events:          sampleEvents(env.scope, env.principalID, string(env.serviceID), env.now),
		Authority:       sampleAuthority(env.scope, env.principalID, string(env.serviceID)),
	}

	result, err := env.manager.Revoke(context.Background(), request)
	if err != nil {
		t.Fatalf("Revoke disabled service: %v", err)
	}
	if len(env.mutations.units) != 1 {
		t.Fatalf("mutation calls = %d; want 1", len(env.mutations.units))
	}
	unit := env.mutations.units[0]
	if unit.Kind != token.MutationRevoke || unit.ServiceID != env.serviceID || unit.ExecutionID != "" {
		t.Fatalf("disable unit = %+v; want a service revoke", unit)
	}
	if unit.Credential.Present() {
		t.Fatalf("disable unit kept a credential pair: %+v", unit.Credential)
	}
	if len(env.invalidator.invalidations) != 1 {
		t.Fatalf("invalidations = %d; want 1", len(env.invalidator.invalidations))
	}
	if invalidation := env.invalidator.invalidations[0]; invalidation.Reason != token.InvalidationDisabled || invalidation.ServiceID != env.serviceID {
		t.Fatalf("invalidation = %+v; want disabled service", invalidation)
	}
	if !result.Invalidated || result.Invalidation.Reason != token.InvalidationDisabled {
		t.Fatalf("result = %+v; want a committed disabled invalidation", result)
	}
}

// TestManagerRotateRequiresCurrentCredentialAndRefreshes pins rotation: it
// replaces a present pair, emits a refresh delivery, and refuses an absent
// credential rather than issuing one silently.
func TestManagerRotateRequiresCurrentCredentialAndRefreshes(t *testing.T) {
	env := newTestEnv(t)
	oldNonce := append([]byte(nil), env.creds.fact.Credential.Nonce...)

	result, err := env.manager.Rotate(context.Background(), executionIssueRequest(env))
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if len(env.mutations.units) != 1 {
		t.Fatalf("mutation calls = %d; want 1", len(env.mutations.units))
	}
	unit := env.mutations.units[0]
	if unit.Kind != token.MutationRotate {
		t.Fatalf("kind = %q; want rotate", unit.Kind)
	}
	if bytes.Equal(unit.Credential.Nonce, oldNonce) {
		t.Fatal("rotation reused the previous nonce")
	}
	if !hasPostCommit(unit.PostCommit, event.PostRefreshCredential) {
		t.Fatalf("post-commit = %v; want refresh_credential", unit.PostCommit)
	}
	if result.Delivery == nil || !result.Delivery.Refresh {
		t.Fatalf("delivery = %+v; want a refresh delivery", result.Delivery)
	}
	if result.Delivery.Kind != token.MutationRotate {
		t.Fatalf("delivery kind = %q; want rotate", result.Delivery.Kind)
	}

	env.setAbsentCredential()
	env.mutations.units = nil
	_, err = env.manager.Rotate(context.Background(), executionIssueRequest(env))
	requireRefusal(t, err)
	if len(env.mutations.units) != 0 {
		t.Fatalf("rotate with absent credential mutated: %+v", env.mutations.units)
	}
}

// TestManagerIssueRefusesDisabledAndStalePrincipals pins current-state checks
// before the mutation: a disabled principal/service or a stale expected version
// never reaches the commit port.
func TestManagerIssueRefusesDisabledAndStalePrincipals(t *testing.T) {
	t.Run("disabled execution principal", func(t *testing.T) {
		env := newTestEnv(t)
		env.setAbsentCredential()
		env.principals.principal.Enabled = false
		_, err := env.manager.Issue(context.Background(), executionIssueRequest(env))
		requireRefusal(t, err)
		if len(env.mutations.units) != 0 {
			t.Fatalf("disabled principal mutated: %+v", env.mutations.units)
		}
	})

	t.Run("disabled service", func(t *testing.T) {
		env := newTestEnv(t)
		env.setServiceCredentialAbsent()
		env.services.service.State = service.StateDisabled
		_, err := env.manager.Issue(context.Background(), serviceIssueRequest(env))
		requireRefusal(t, err)
		if len(env.mutations.units) != 0 {
			t.Fatalf("disabled service mutated: %+v", env.mutations.units)
		}
	})

	t.Run("stale expected version", func(t *testing.T) {
		env := newTestEnv(t)
		env.setAbsentCredential()
		request := executionIssueRequest(env)
		request.ExpectedVersion = 2
		_, err := env.manager.Issue(context.Background(), request)
		requireClass(t, err, fault.VersionConflict)
		if len(env.mutations.units) != 0 {
			t.Fatalf("stale version mutated: %+v", env.mutations.units)
		}
	})

	t.Run("commit version conflict", func(t *testing.T) {
		env := newTestEnv(t)
		env.setAbsentCredential()
		env.mutations.err = fault.New(fault.VersionConflict, "principal version changed")
		_, err := env.manager.Issue(context.Background(), executionIssueRequest(env))
		requireClass(t, err, fault.VersionConflict)
		if len(env.deliverer.deliveries) != 0 {
			t.Fatalf("delivery followed a version conflict: %+v", env.deliverer.deliveries)
		}
	})
}

// TestManagerIssueCapsTTLByDeadlineAndBounds pins the lifetime rules: the
// credential never outlives the action deadline and TTLs outside the declared
// 1-minute/8-hour bounds are refused.
func TestManagerIssueCapsTTLByDeadlineAndBounds(t *testing.T) {
	t.Run("deadline caps ttl", func(t *testing.T) {
		env := newTestEnv(t)
		env.setAbsentCredential()
		request := executionIssueRequest(env)
		request.TTL = 4 * time.Hour
		request.Deadline = env.now.Add(10 * time.Minute)
		if _, err := env.manager.Issue(context.Background(), request); err != nil {
			t.Fatalf("Issue with deadline: %v", err)
		}
		unit := env.mutations.units[0]
		if !unit.Credential.Expiry.Equal(request.Deadline) {
			t.Fatalf("expiry = %v; want deadline %v", unit.Credential.Expiry, request.Deadline)
		}
	})

	t.Run("deadline in the past", func(t *testing.T) {
		env := newTestEnv(t)
		env.setAbsentCredential()
		request := executionIssueRequest(env)
		request.Deadline = env.now
		_, err := env.manager.Issue(context.Background(), request)
		requireClass(t, err, fault.DeadlineExpired)
		if len(env.mutations.units) != 0 {
			t.Fatalf("past deadline mutated: %+v", env.mutations.units)
		}
	})

	t.Run("ttl below minimum", func(t *testing.T) {
		env := newTestEnv(t)
		env.setAbsentCredential()
		request := executionIssueRequest(env)
		request.TTL = 30 * time.Second
		_, err := env.manager.Issue(context.Background(), request)
		requireClass(t, err, fault.InvalidInput)
	})

	t.Run("ttl above maximum", func(t *testing.T) {
		env := newTestEnv(t)
		env.setAbsentCredential()
		request := executionIssueRequest(env)
		request.TTL = 9 * time.Hour
		_, err := env.manager.Issue(context.Background(), request)
		requireClass(t, err, fault.InvalidInput)
	})
}

// TestManagerEnsureExecutionReusesCurrentCredential proves container activation
// does not rotate a still-valid pair and does not touch delivery.
func TestManagerEnsureExecutionReusesCurrentCredential(t *testing.T) {
	env := newTestEnv(t)

	result, err := env.manager.EnsureExecution(context.Background(), executionIssueRequest(env))
	if err != nil {
		t.Fatalf("EnsureExecution: %v", err)
	}
	if !result.Reused {
		t.Fatalf("result = %+v; want reuse", result)
	}
	if result.State != token.StateCurrent {
		t.Fatalf("State = %q; want current", result.State)
	}
	if result.Token != "" || result.Delivery != nil || result.Delivered {
		t.Fatalf("reuse produced delivery material: %+v", result)
	}
	if len(env.mutations.units) != 0 || len(env.deliverer.deliveries) != 0 {
		t.Fatalf("reuse mutated/delivered: units=%d deliveries=%d", len(env.mutations.units), len(env.deliverer.deliveries))
	}
}

// TestManagerEnsureExecutionIssuesWhenAbsentOrExpired pins activation: an
// absent or expired pair is issued, never blindly reused.
func TestManagerEnsureExecutionIssuesWhenAbsentOrExpired(t *testing.T) {
	cases := []struct {
		name       string
		credential token.Credential
	}{
		{name: "absent", credential: token.Credential{}},
		{name: "expired", credential: token.Credential{Nonce: testNonce(), Expiry: time.Unix(1_700_000_000, 0).UTC()}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			env := newTestEnv(t)
			env.creds.fact.Credential = testCase.credential

			result, err := env.manager.EnsureExecution(context.Background(), executionIssueRequest(env))
			if err != nil {
				t.Fatalf("EnsureExecution: %v", err)
			}
			if result.Reused {
				t.Fatalf("result = %+v; want a fresh issue", result)
			}
			if result.Token == "" || result.Delivery == nil || !result.Delivered {
				t.Fatalf("result = %+v; want a delivered fresh token", result)
			}
			if len(env.mutations.units) != 1 || env.mutations.units[0].Kind != token.MutationIssue {
				t.Fatalf("mutation = %+v; want one issue", env.mutations.units)
			}
		})
	}
}

// TestManagerRevokeClearsAndInvalidatesAfterCommit pins revocation ordering:
// the pair is cleared by the commit, then live connections are invalidated.
func TestManagerRevokeClearsAndInvalidatesAfterCommit(t *testing.T) {
	env := newTestEnv(t)

	result, err := env.manager.Revoke(context.Background(), revokeRequest(env))
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if len(env.mutations.units) != 1 {
		t.Fatalf("mutation calls = %d; want 1", len(env.mutations.units))
	}
	unit := env.mutations.units[0]
	if unit.Kind != token.MutationRevoke {
		t.Fatalf("kind = %q; want revoke", unit.Kind)
	}
	if unit.Credential.Present() {
		t.Fatalf("revoke unit kept a credential pair: %+v", unit.Credential)
	}
	if len(env.invalidator.invalidations) != 1 {
		t.Fatalf("invalidations = %d; want 1", len(env.invalidator.invalidations))
	}
	invalidation := env.invalidator.invalidations[0]
	if invalidation.Reason != token.InvalidationRevoked || invalidation.ExecutionID != env.executionID {
		t.Fatalf("invalidation = %+v; want revoked execution", invalidation)
	}
	if !result.Invalidated || result.Invalidation.Reason != token.InvalidationRevoked {
		t.Fatalf("result = %+v; want committed invalidation", result)
	}
	if result.Mutation.State != token.StateAbsent {
		t.Fatalf("mutation state = %q; want absent", result.Mutation.State)
	}
}

// TestManagerRevokeDoesNotInvalidateWhenMutationFails proves a failed clear
// never tells live consumers to drop a still-current credential.
func TestManagerRevokeDoesNotInvalidateWhenMutationFails(t *testing.T) {
	env := newTestEnv(t)
	env.mutations.err = fault.New(fault.DependencyUnavailable, "store unavailable")

	_, err := env.manager.Revoke(context.Background(), revokeRequest(env))
	if err == nil {
		t.Fatal("Revoke succeeded despite a failed commit")
	}
	if len(env.invalidator.invalidations) != 0 {
		t.Fatalf("invalidation happened before commit: %+v", env.invalidator.invalidations)
	}
}

// TestManagerRevokeReportsRetryableInvalidationAfterCommit proves a lost
// invalidation response is recoverable without re-mutating.
func TestManagerRevokeReportsRetryableInvalidationAfterCommit(t *testing.T) {
	env := newTestEnv(t)
	env.invalidator.err = fault.New(fault.UncertainExternalEffect, "connection registry unavailable")

	result, err := env.manager.Revoke(context.Background(), revokeRequest(env))
	if err == nil {
		t.Fatal("Revoke hid an invalidation failure")
	}
	if result.Mutation.State != token.StateAbsent || result.Invalidated {
		t.Fatalf("result = %+v; want committed clear with pending invalidation", result)
	}
	if len(env.mutations.units) != 1 {
		t.Fatalf("mutation calls = %d; want 1", len(env.mutations.units))
	}
	if result.Invalidation.Reason != token.InvalidationRevoked {
		t.Fatalf("returned invalidation = %+v; want retryable revoked command", result.Invalidation)
	}
}

// TestManagerIssueRefusesCrossScopeReads proves a reader returning another
// tenant's principal/service cannot steer a mutation in the caller's scope.
func TestManagerIssueRefusesCrossScopeReads(t *testing.T) {
	env := newTestEnv(t)
	env.setAbsentCredential()
	env.principals.principal.Scope = tenancy.Scope{TenantID: id.TenantID("tenant-b"), ProjectID: id.ProjectID("project-2")}

	_, err := env.manager.Issue(context.Background(), executionIssueRequest(env))
	requireRefusal(t, err)
	if len(env.mutations.units) != 0 {
		t.Fatalf("cross-scope read mutated: %+v", env.mutations.units)
	}
}

// TestManagerIssueRejectsInvalidRequests pins malformed lifecycle input before
// any read or write.
func TestManagerIssueRejectsInvalidRequests(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*testEnv, *token.IssueRequest)
	}{
		{name: "no target", mutate: func(_ *testEnv, request *token.IssueRequest) {
			request.ExecutionID = ""
			request.ServiceID = ""
		}},
		{name: "two targets", mutate: func(env *testEnv, request *token.IssueRequest) {
			request.ServiceID = env.serviceID
		}},
		{name: "invalid scope", mutate: func(_ *testEnv, request *token.IssueRequest) {
			request.Scope = tenancy.Scope{}
		}},
		{name: "invalid principal", mutate: func(_ *testEnv, request *token.IssueRequest) {
			request.PrincipalID = id.PrincipalID("../principal")
		}},
		{name: "zero expected version", mutate: func(_ *testEnv, request *token.IssueRequest) {
			request.ExpectedVersion = 0
		}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			env := newTestEnv(t)
			env.setAbsentCredential()
			request := executionIssueRequest(env)
			testCase.mutate(env, &request)
			_, err := env.manager.Issue(context.Background(), request)
			requireClass(t, err, fault.InvalidInput)
			if len(env.mutations.units) != 0 {
				t.Fatalf("invalid request mutated: %+v", env.mutations.units)
			}
		})
	}
}

// TestManagerIssueNeverPersistsTokenPlaintext proves the commit unit has no
// token/token-hash field and its serialized form does not contain the token.
func TestManagerIssueNeverPersistsTokenPlaintext(t *testing.T) {
	env := newTestEnv(t)
	env.setAbsentCredential()

	result, err := env.manager.Issue(context.Background(), executionIssueRequest(env))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	unit := env.mutations.units[0]
	serialized := serializedUnit(t, unit)
	if strings.Contains(serialized, result.Token) {
		t.Fatalf("commit unit contains the plaintext token: %s", serialized)
	}
	if strings.Contains(serialized, "token_hash") || strings.Contains(serialized, "token") {
		t.Fatalf("commit unit serializes a token field: %s", serialized)
	}

	unitType := reflect.TypeFor[token.MutationUnit]()
	for field := range unitType.Fields() {
		name := strings.ToLower(field.Name)
		if name == "token" || name == "tokenhash" || name == "plaintext" {
			t.Fatalf("token.MutationUnit carries a forbidden field %q", field.Name)
		}
	}
}

func serializedUnit(t *testing.T, unit token.MutationUnit) string {
	t.Helper()
	encoded, err := json.Marshal(unit)
	if err != nil {
		t.Fatalf("marshal mutation unit: %v", err)
	}
	return string(encoded)
}

func hasPostCommit(effects []event.PostCommitEffect, kind event.PostCommitKind) bool {
	for _, effect := range effects {
		if effect.Kind == kind {
			return true
		}
	}
	return false
}

// TestManagerConcurrentRenewalUsesOptimisticVersion pins the renewal race: two
// rotations both read version 3, exactly one commits, the loser gets a version
// conflict and delivers nothing.
func TestManagerConcurrentRenewalUsesOptimisticVersion(t *testing.T) {
	env := newTestEnv(t)
	versioned := &versionedMutations{current: 3, result: token.MutationResult{State: token.StateCurrent, Version: 4}}
	env.manager = newManager(t, env, versioned)

	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			<-start
			_, err := env.manager.Rotate(context.Background(), executionIssueRequest(env))
			results <- err
		})
	}
	close(start)
	wg.Wait()
	close(results)

	var successes, conflicts int
	for err := range results {
		switch {
		case err == nil:
			successes++
		default:
			class, ok := fault.ClassOf(err)
			if !ok || class != fault.VersionConflict {
				t.Fatalf("loser error = %v; want version_conflict", err)
			}
			conflicts++
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes/conflicts = %d/%d; want 1/1", successes, conflicts)
	}
	if len(env.deliverer.deliveries) != 1 {
		t.Fatalf("deliveries = %d; want only the winning rotation", len(env.deliverer.deliveries))
	}
}

func newManager(t *testing.T, env *testEnv, mutations token.MutationPort) *token.Manager {
	t.Helper()
	manager, err := token.NewManager(token.ManagerConfig{
		Keys:          env.keys,
		Resolver:      env.resolver,
		Credentials:   env.creds,
		Principals:    env.principals,
		Services:      env.services,
		Mutations:     mutations,
		Receipts:      env.receipts,
		Deliverer:     env.deliverer,
		Invalidations: env.invalidator,
		Clock:         env.clock,
		Random:        env.random,
		Bounds:        token.DefaultBounds,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return manager
}

// replayingMutations commits the first unit per receipt key and reports an
// equal-receipt replay on later attempts, like the store's ExecuteUnit. It
// keeps the committed pair so a test can prove a replayed attempt derived a
// different, uncommitted nonce.
type replayingMutations struct {
	mu        sync.Mutex
	committed map[receipt.Key]token.Credential
	units     []token.MutationUnit
}

func (fake *replayingMutations) Mutate(_ context.Context, unit token.MutationUnit) (token.MutationResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.units = append(fake.units, unit)
	if _, ok := fake.committed[unit.Receipt.Key]; ok {
		return token.MutationResult{State: token.StateCurrent, Version: 4, Replayed: true}, nil
	}
	fake.committed[unit.Receipt.Key] = unit.Credential
	return token.MutationResult{State: token.StateCurrent, Version: 4}, nil
}

// versionedMutations serializes commits and applies the expected-version check
// that the real store owns.
type versionedMutations struct {
	mu      sync.Mutex
	current uint64
	result  token.MutationResult
}

func (fake *versionedMutations) Mutate(_ context.Context, unit token.MutationUnit) (token.MutationResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if unit.ExpectedVersion != fake.current {
		return token.MutationResult{}, fault.New(fault.VersionConflict, "principal version changed")
	}
	fake.current++
	result := fake.result
	result.Version = fake.current
	return result, nil
}

// TestManagerDeliveryStringRedactsToken proves the plaintext delivery command
// has a redacted string form for logs.
func TestManagerDeliveryStringRedactsToken(t *testing.T) {
	delivery := token.Delivery{
		Scope:       sampleScope(),
		PrincipalID: id.PrincipalID("principal-1"),
		ExecutionID: id.ExecutionID("exec-1"),
		Kind:        token.MutationIssue,
		Token:       "super-secret-token",
		Target:      "exec-1",
	}
	if err := delivery.Validate(); err != nil {
		t.Fatalf("valid delivery refused: %v", err)
	}
	if strings.Contains(delivery.String(), "super-secret-token") {
		t.Fatalf("Delivery.String leaked the token: %s", delivery.String())
	}
}
