// Receipt-replay regression proofs for credential administration.
//
// Fakes whose state never advances cannot detect credential-presence and
// expected-version preflights that run before the durable receipt. These proofs
// use one stateful store that is the service reader, credential reader, receipt
// lookup and mutation port. The first commit advances the service version and
// installs/clears the credential pair, exactly as a real adapter does; an
// equal-content retry must therefore reach the receipt replay instead of the
// stale-state preflights.
package token_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/token"
)

// statefulCredentialStore is a replay-capable service/credential store and
// mutation port. It checks the durable receipt first, replays equal content
// without applying another mutation, refuses changed content, and applies the
// expected-version guard and credential replacement in one step.
type statefulCredentialStore struct {
	mu       sync.Mutex
	service  service.Service
	fact     token.CredentialFact
	receipts map[receipt.Key]receipt.Record
	units    []token.MutationUnit
	commits  int
	replays  int
}

func newStatefulCredentialStore(env *testEnv, present bool) *statefulCredentialStore {
	return &statefulCredentialStore{
		service:  env.services.service,
		fact:     serviceCredentialFact(env, present),
		receipts: map[receipt.Key]receipt.Record{},
	}
}

func (store *statefulCredentialStore) ReadService(_ context.Context, request service.ServiceReadRequest) (service.Service, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if request.Scope != store.service.Scope || request.ServiceID != store.service.ID {
		return service.Service{}, fault.New(fault.Unauthenticated, "service is outside the request scope")
	}
	return store.service, nil
}

func (store *statefulCredentialStore) ReadCredential(_ context.Context, request token.CredentialRequest) (token.CredentialFact, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if request.Scope != store.fact.Scope || request.PrincipalID != store.fact.PrincipalID || request.ServiceID != store.fact.ServiceID {
		return token.CredentialFact{}, fault.New(fault.Unauthenticated, "credential is outside the request scope")
	}
	return store.fact, nil
}

func (store *statefulCredentialStore) Lookup(_ context.Context, key receipt.Key) (receipt.Record, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	record, found := store.receipts[key]
	return record, found, nil
}

func (store *statefulCredentialStore) Mutate(_ context.Context, unit token.MutationUnit) (token.MutationResult, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.units = append(store.units, unit)
	if existing, found := store.receipts[unit.Receipt.Key]; found {
		if existing.ContentHash != unit.Receipt.ContentHash {
			return token.MutationResult{}, fault.New(fault.ChangedIdempotencyContent, "changed idempotency content")
		}
		store.replays++
		return token.MutationResult{State: mutationState(unit.Kind), Version: store.service.Version, Replayed: true}, nil
	}
	if unit.ExpectedVersion != store.service.Version {
		return token.MutationResult{}, fault.New(fault.VersionConflict, "service version changed")
	}
	store.service.Version++
	store.fact.Version = store.service.Version
	if unit.Kind == token.MutationRevoke {
		store.fact.Credential = token.Credential{}
	} else {
		store.fact.Credential = unit.Credential
	}
	store.receipts[unit.Receipt.Key] = unit.Receipt
	store.commits++
	return token.MutationResult{State: mutationState(unit.Kind), Version: store.service.Version}, nil
}

func (store *statefulCredentialStore) setState(state service.State) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.service.State = state
}

func mutationState(kind token.MutationKind) token.State {
	if kind == token.MutationRevoke {
		return token.StateAbsent
	}
	return token.StateCurrent
}

type replayFixture struct {
	env     *testEnv
	store   *statefulCredentialStore
	admin   *token.CredentialAdmin
	manager *token.Manager
}

func newReplayFixture(t *testing.T, present bool) *replayFixture {
	t.Helper()
	env := newTestEnv(t)
	env.principals.principal = credentialActor(env.scope)
	store := newStatefulCredentialStore(env, present)
	manager, err := token.NewManager(token.ManagerConfig{
		Keys:          env.keys,
		Resolver:      env.resolver,
		Credentials:   store,
		Principals:    env.principals,
		Services:      store,
		Mutations:     store,
		Receipts:      store,
		Deliverer:     env.deliverer,
		Invalidations: env.invalidator,
		Clock:         env.clock,
		Random:        env.random,
		Bounds:        token.DefaultBounds,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	admin, err := token.NewCredentialAdmin(token.CredentialAdminConfig{
		Authorizer:  &credentialAuthorizer{},
		Authority:   &credentialAuthority{},
		Principals:  env.principals,
		Services:    store,
		Credentials: store,
		Manager:     manager,
		Receipts:    store,
		Clock:       env.clock,
		IDs:         &credentialIDs{},
	})
	if err != nil {
		t.Fatalf("NewCredentialAdmin: %v", err)
	}
	return &replayFixture{env: env, store: store, admin: admin, manager: manager}
}

// TestCredentialAdminLostResponseRetryReachesReceiptReplay proves an
// equal-content retry after a commit whose response was lost returns the
// original state/version with Replayed=true and no token, and the store commits
// exactly once.
func TestCredentialAdminLostResponseRetryReachesReceiptReplay(t *testing.T) {
	cases := []struct {
		name    string
		present bool
		call    func(*replayFixture, token.ServiceCredentialCommand) (token.IssueResult, error)
	}{
		{
			name:    "issue",
			present: false,
			call: func(fixture *replayFixture, command token.ServiceCredentialCommand) (token.IssueResult, error) {
				return fixture.admin.Issue(context.Background(), command)
			},
		},
		{
			name:    "rotate",
			present: true,
			call: func(fixture *replayFixture, command token.ServiceCredentialCommand) (token.IssueResult, error) {
				return fixture.admin.Rotate(context.Background(), command)
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newReplayFixture(t, testCase.present)
			command := credentialCommand(fixture.env)

			first, err := testCase.call(fixture, command)
			if err != nil {
				t.Fatalf("first %s: %v", testCase.name, err)
			}
			if first.Token == "" || first.Replayed || first.Delivery != nil || first.Delivered {
				t.Fatalf("first result = %+v; want one fresh token and no delivery", first)
			}
			if fixture.store.commits != 1 || len(fixture.store.units) != 1 {
				t.Fatalf("commits/units = %d/%d; want 1/1", fixture.store.commits, len(fixture.store.units))
			}
			committed := fixture.store.units[0]

			second, err := testCase.call(fixture, command)
			if err != nil {
				t.Fatalf("replayed %s: %v", testCase.name, err)
			}
			if !second.Replayed || second.Token != "" || second.Delivery != nil || second.Delivered {
				t.Fatalf("replay result = %+v; want the committed result with no token", second)
			}
			if second.State != token.StateCurrent || second.Version != 4 {
				t.Fatalf("replay state/version = %q/%d; want current/4", second.State, second.Version)
			}
			if fixture.store.commits != 1 || len(fixture.store.units) != 1 {
				t.Fatalf("replay mutated: commits/units = %d/%d", fixture.store.commits, len(fixture.store.units))
			}

			changed := command
			changed.Content = receipt.Content{MediaType: "application/json", Bytes: []byte(`{"ttl":"2h"}`)}
			_, err = testCase.call(fixture, changed)
			requireCredentialClass(t, err, fault.ChangedIdempotencyContent)
			if fixture.store.commits != 1 || len(fixture.store.units) != 1 {
				t.Fatalf("changed content mutated: commits/units = %d/%d", fixture.store.commits, len(fixture.store.units))
			}

			nonceHex := hex.EncodeToString(committed.Credential.Nonce)
			serialized, marshalErr := json.Marshal(second)
			if marshalErr != nil {
				t.Fatalf("marshal replay result: %v", marshalErr)
			}
			record := fixture.store.receipts[committed.Receipt.Key]
			requireNoSecretMaterial(t, nonceHex,
				string(serialized),
				string(record.Result),
				err.Error(),
			)
			for _, audit := range committed.Audit {
				requireNoSecretMaterial(t, nonceHex, audit.Detail)
			}
			for _, envelope := range committed.Events {
				requireNoSecretMaterial(t, nonceHex, string(envelope.Data))
			}
		})
	}
}

// TestCredentialAdminStaleVersionFreshCallerKeyIsRefused proves the replay
// exception does not weaken the optimistic guard: a fresh caller key with a
// stale expected version is still version_conflict with no commit.
func TestCredentialAdminStaleVersionFreshCallerKeyIsRefused(t *testing.T) {
	t.Run("rotate", func(t *testing.T) {
		fixture := newReplayFixture(t, true)
		command := credentialCommand(fixture.env)
		if _, err := fixture.admin.Rotate(context.Background(), command); err != nil {
			t.Fatalf("first Rotate: %v", err)
		}

		stale := command
		stale.CallerKey = "caller-2"
		_, err := fixture.admin.Rotate(context.Background(), stale)
		requireCredentialClass(t, err, fault.VersionConflict)
		if fixture.store.commits != 1 || len(fixture.store.units) != 1 {
			t.Fatalf("stale rotate mutated: commits/units = %d/%d", fixture.store.commits, len(fixture.store.units))
		}
	})

	t.Run("revoke", func(t *testing.T) {
		fixture := newReplayFixture(t, true)
		command := revokeCommand(fixture.env)
		if _, err := fixture.admin.Revoke(context.Background(), command); err != nil {
			t.Fatalf("first Revoke: %v", err)
		}

		stale := command
		stale.CallerKey = "caller-2"
		_, err := fixture.admin.Revoke(context.Background(), stale)
		requireCredentialClass(t, err, fault.VersionConflict)
		if fixture.store.commits != 1 || len(fixture.store.units) != 1 {
			t.Fatalf("stale revoke mutated: commits/units = %d/%d", fixture.store.commits, len(fixture.store.units))
		}
	})
}

// TestCredentialAdminRevokeRetryReplaysAndReRunsInvalidation proves a lost
// Revoke response retries the committed result and re-runs the invalidation
// without a second mutation.
func TestCredentialAdminRevokeRetryReplaysAndReRunsInvalidation(t *testing.T) {
	fixture := newReplayFixture(t, true)
	command := revokeCommand(fixture.env)

	first, err := fixture.admin.Revoke(context.Background(), command)
	if err != nil {
		t.Fatalf("first Revoke: %v", err)
	}
	if first.Mutation.Replayed || !first.Invalidated || first.Mutation.State != token.StateAbsent || first.Mutation.Version != 4 {
		t.Fatalf("first result = %+v; want an invalidated absent credential", first)
	}
	if fixture.store.commits != 1 || len(fixture.env.invalidator.invalidations) != 1 {
		t.Fatalf("commits/invalidations = %d/%d; want 1/1", fixture.store.commits, len(fixture.env.invalidator.invalidations))
	}

	second, err := fixture.admin.Revoke(context.Background(), command)
	if err != nil {
		t.Fatalf("replayed Revoke: %v", err)
	}
	if !second.Mutation.Replayed || !second.Invalidated || second.Mutation.State != token.StateAbsent || second.Mutation.Version != 4 {
		t.Fatalf("replay result = %+v; want the committed revocation with a replayed invalidation", second)
	}
	if fixture.store.commits != 1 || len(fixture.store.units) != 1 {
		t.Fatalf("replay mutated: commits/units = %d/%d", fixture.store.commits, len(fixture.store.units))
	}
	if len(fixture.env.invalidator.invalidations) != 2 {
		t.Fatalf("invalidations = %d; want the original plus the retry", len(fixture.env.invalidator.invalidations))
	}
}

// TestCredentialAdminRevokeRetryRecoversLostInvalidation proves a failing
// invalidation after the commit is retryable: the retry does not mutate again,
// returns the committed result, and a later invalidation succeeds.
func TestCredentialAdminRevokeRetryRecoversLostInvalidation(t *testing.T) {
	fixture := newReplayFixture(t, true)
	command := revokeCommand(fixture.env)
	fixture.env.invalidator.err = fault.New(fault.UncertainExternalEffect, "connection registry unavailable")

	first, err := fixture.admin.Revoke(context.Background(), command)
	if err == nil {
		t.Fatal("Revoke hid the lost invalidation")
	}
	requireCredentialClass(t, err, fault.UncertainExternalEffect)
	if first.Invalidated || first.Mutation.State != token.StateAbsent || first.Mutation.Version != 4 {
		t.Fatalf("first result = %+v; want a committed clear with pending invalidation", first)
	}
	if fixture.store.commits != 1 || len(fixture.store.units) != 1 {
		t.Fatalf("first commits/units = %d/%d; want 1/1", fixture.store.commits, len(fixture.store.units))
	}
	if len(fixture.env.invalidator.invalidations) != 1 {
		t.Fatalf("first invalidations = %d; want 1", len(fixture.env.invalidator.invalidations))
	}

	fixture.env.invalidator.err = nil
	second, err := fixture.admin.Revoke(context.Background(), command)
	if err != nil {
		t.Fatalf("retried Revoke: %v", err)
	}
	if !second.Mutation.Replayed || !second.Invalidated || second.Mutation.State != token.StateAbsent || second.Mutation.Version != 4 {
		t.Fatalf("retry result = %+v; want the committed revocation with a successful invalidation", second)
	}
	if fixture.store.commits != 1 || len(fixture.store.units) != 1 {
		t.Fatalf("retry mutated: commits/units = %d/%d", fixture.store.commits, len(fixture.store.units))
	}
	if len(fixture.env.invalidator.invalidations) != 2 {
		t.Fatalf("retry invalidations = %d; want the failed attempt plus the successful retry", len(fixture.env.invalidator.invalidations))
	}
}

// TestCredentialAdminDisabledServiceRetryIsRefused proves the disabled-service
// refusal still runs before the receipt replay: a retry after the service was
// disabled is refused and does not return a committed credential.
func TestCredentialAdminDisabledServiceRetryIsRefused(t *testing.T) {
	fixture := newReplayFixture(t, false)
	command := credentialCommand(fixture.env)
	if _, err := fixture.admin.Issue(context.Background(), command); err != nil {
		t.Fatalf("first Issue: %v", err)
	}

	fixture.store.setState(service.StateDisabled)
	_, err := fixture.admin.Issue(context.Background(), command)
	requireCredentialClass(t, err, fault.Forbidden)
	if fixture.store.commits != 1 || len(fixture.store.units) != 1 {
		t.Fatalf("disabled-service retry mutated: commits/units = %d/%d", fixture.store.commits, len(fixture.store.units))
	}
}

// TestManagerServiceRetryReachesReceiptReplay proves the Manager itself
// consults the durable receipt before the expected-version preflight, so a
// direct caller's equal-content retry also replays instead of conflicting.
func TestManagerServiceRetryReachesReceiptReplay(t *testing.T) {
	fixture := newReplayFixture(t, false)
	command := credentialCommand(fixture.env)
	if _, err := fixture.admin.Issue(context.Background(), command); err != nil {
		t.Fatalf("first Issue: %v", err)
	}
	committed := fixture.store.units[0]
	record := fixture.store.receipts[committed.Receipt.Key]

	request := token.IssueRequest{
		Scope:           fixture.env.scope,
		PrincipalID:     id.PrincipalID(fixture.env.serviceID),
		ServiceID:       fixture.env.serviceID,
		TTL:             time.Hour,
		ExpectedVersion: 3,
		Receipt:         record,
	}
	result, err := fixture.manager.Issue(context.Background(), request)
	if err != nil {
		t.Fatalf("replayed Manager Issue: %v", err)
	}
	if !result.Replayed || result.Token != "" || result.Delivery != nil || result.Delivered {
		t.Fatalf("Manager replay result = %+v; want the committed result with no token", result)
	}
	if result.State != token.StateCurrent || result.Version != 4 {
		t.Fatalf("Manager replay state/version = %q/%d; want current/4", result.State, result.Version)
	}
	if fixture.store.commits != 1 || len(fixture.store.units) != 1 {
		t.Fatalf("Manager replay mutated: commits/units = %d/%d", fixture.store.commits, len(fixture.store.units))
	}
}

func requireNoSecretMaterial(t *testing.T, nonceHex string, values ...string) {
	t.Helper()
	if nonceHex == "" {
		t.Fatal("committed credential has no nonce to check")
	}
	for _, value := range values {
		if strings.Contains(value, nonceHex) {
			t.Fatalf("secret material leaked into replay output: %q", value)
		}
	}
}
