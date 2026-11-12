//go:build component

package postgrescomponent

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"strconv"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/service"
	storepg "github.com/optimiware/kodama/internal/shell/store/postgres"
	identityunits "github.com/optimiware/kodama/internal/shell/store/units/identity"
	"github.com/optimiware/kodama/internal/shell/store/units/readers"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
	"github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

// TestIdentityPrincipalDecodeOnPostgreSQL proves the identity adapter decodes
// every principal field from its durable row, including enablement and roles.
func TestIdentityPrincipalDecodeOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	shard.SeedExecution(ctx, scope, "execution-run", "running", 3, "", "")
	shard.SeedExecution(ctx, scope, "execution-queued", "queued", 1, "", "")
	for id, principal := range map[string]string{
		"execution-run": "execution-principal-1", "execution-queued": "execution-principal-2",
	} {
		if _, err := pgtest.ExecSQL(ctx, shard.Admin(),
			`UPDATE executions SET principal_id = $2 WHERE id = $1`, id, principal); err != nil {
			t.Fatalf("set execution principal: %v", err)
		}
	}
	shard.SeedService(ctx, scope, "service-on", "on", string(tenancy.RoleEngineer))
	shard.SeedService(ctx, scope, "service-off", "off", string(tenancy.RoleEngineer))
	if _, err := pgtest.ExecSQL(ctx, shard.Admin(),
		`UPDATE services SET state = 'disabled' WHERE id = 'service-off'`); err != nil {
		t.Fatalf("disable service: %v", err)
	}
	shard.SeedMembership(ctx, scope, "membership-human", "principal-human", 4,
		string(tenancy.RoleEngineer), string(tenancy.RoleReviewer))
	runtime := shard.Runtime(4)
	defer runtime.Close()
	adapter, err := identityunits.New(identityunits.Config{
		Executor: storepg.New(runtime), Scanner: storepg.New(runtime),
		Clock: integrationClock{now: pgtest.FixtureTime}, IDs: &identityIDs{},
	})
	if err != nil {
		t.Fatalf("identity.New: %v", err)
	}
	cases := []struct {
		id          id.PrincipalID
		kind        identity.PrincipalKind
		enabled     bool
		version     uint64
		roles       []tenancy.Role
		executionID id.ExecutionID
	}{
		{"execution-principal-1", identity.KindExecution, true, 3, nil, "execution-run"},
		{"execution-principal-2", identity.KindExecution, false, 1, nil, "execution-queued"},
		{"service-on", identity.KindService, true, 1, []tenancy.Role{tenancy.RoleEngineer}, ""},
		{"service-off", identity.KindService, false, 1, []tenancy.Role{tenancy.RoleEngineer}, ""},
		{"principal-human", identity.KindHuman, true, 4, []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleReviewer}, ""},
	}
	for _, testCase := range cases {
		principal, err := adapter.ReadPrincipal(ctx, identity.PrincipalRequest{Scope: scope, PrincipalID: testCase.id})
		if err != nil {
			t.Fatalf("ReadPrincipal(%s): %v%s", testCase.id, err, pgtest.CauseText(err))
		}
		if principal.ID != testCase.id || principal.Kind != testCase.kind || principal.Scope != scope ||
			principal.Enabled != testCase.enabled || principal.Version != testCase.version ||
			principal.ExecutionID != testCase.executionID || !sameRoles(principal.Roles, testCase.roles) {
			t.Fatalf("principal %s = %+v; want kind=%s enabled=%v version=%d roles=%v execution=%q",
				testCase.id, principal, testCase.kind, testCase.enabled, testCase.version, testCase.roles, testCase.executionID)
		}
	}
}

// sameRoles compares two ordered role sets.
func sameRoles(left, right []tenancy.Role) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

type recordingServiceInvalidator struct{}

func (recordingServiceInvalidator) InvalidateService(context.Context, service.ServiceInvalidation) error {
	return nil
}

type recordingTokenInvalidator struct{}

func (recordingTokenInvalidator) Invalidate(context.Context, token.Invalidation) error { return nil }

type recordingDeliverer struct{}

func (recordingDeliverer) Deliver(context.Context, token.Delivery) error { return nil }

type plainResolver struct{}

func (plainResolver) ResolveDerivationKey(context.Context, token.TenantKey) ([]byte, error) {
	return bytes.Repeat([]byte{0x2a}, 32), nil
}

// credentialCountingBytes returns bytes [offset, offset+n) of the deterministic
// non-zero stream whose byte i is i%251+1. No byte is zero, so a zeroed stored
// nonce can never equal a drawn one, and consecutive 32-byte draws all differ.
func credentialCountingBytes(offset, n int) []byte {
	block := make([]byte, n)
	for index := range block {
		block[index] = byte((offset+index)%251 + 1)
	}
	return block
}

// credentialCountingRandom serves the first n bytes of that stream.
func credentialCountingRandom(n int) io.Reader {
	return bytes.NewReader(credentialCountingBytes(0, n))
}

// TestRealCredentialAdminOnPostgreSQL drives the real service admin and token
// credential admin/manager through service create, issue, rotate and revoke over
// the PostgreSQL identity adapter, including replays.
func TestRealCredentialAdminOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	scope := pgtest.Scope("tenant-a", "project-a")
	shard.SeedTenant(ctx, "tenant-a", 1)
	shard.SeedProject(ctx, "tenant-a", "project-a")
	shard.SeedMembership(ctx, scope, "membership-admin", "principal-admin", 1, string(tenancy.RoleAdmin))
	runtime := shard.Runtime(4)
	defer runtime.Close()
	scoped := storepg.New(runtime)
	adapter, err := identityunits.New(identityunits.Config{
		Executor: scoped, Scanner: scoped,
		Clock: integrationClock{now: pgtest.FixtureTime}, IDs: &identityIDs{},
	})
	if err != nil {
		t.Fatalf("identity.New: %v", err)
	}
	ids := &integrationIDs{}
	receipts, err := readers.NewReceiptLookup(scoped)
	if err != nil {
		t.Fatalf("NewReceiptLookup: %v", err)
	}
	serviceAdmin, err := service.NewAdmin(service.AdminConfig{
		Authorizer: allowAuthorizer{}, Authority: adapter, Principals: adapter,
		Services: adapter, Port: adapter, Invalidations: recordingServiceInvalidator{},
		Clock: integrationClock{now: pgtest.FixtureTime}, IDs: ids,
	})
	if err != nil {
		t.Fatalf("service.NewAdmin: %v", err)
	}
	manager, err := token.NewManager(token.ManagerConfig{
		Keys: adapter, Resolver: plainResolver{}, Credentials: adapter,
		Principals: adapter, Services: adapter, Mutations: adapter, Receipts: receipts,
		Deliverer: recordingDeliverer{}, Invalidations: recordingTokenInvalidator{},
		Clock:  integrationClock{now: pgtest.FixtureTime},
		Random: credentialCountingRandom(8192), Bounds: token.DefaultBounds,
	})
	if err != nil {
		t.Fatalf("token.NewManager: %v", err)
	}
	credentialAdmin, err := token.NewCredentialAdmin(token.CredentialAdminConfig{
		Authorizer: allowAuthorizer{}, Authority: adapter, Principals: adapter,
		Services: adapter, Credentials: adapter, Manager: manager, Receipts: receipts,
		Clock: integrationClock{now: pgtest.FixtureTime}, IDs: ids,
	})
	if err != nil {
		t.Fatalf("token.NewCredentialAdmin: %v", err)
	}
	actor := humanAdmin(scope, "principal-admin")
	// The Authenticate helpers and the nonce read are read-only: every call
	// re-reads current state and writes nothing.
	requireAuthenticated := func(label, presented string, version uint64) {
		t.Helper()
		authenticated, err := manager.Authenticate(ctx, token.AuthenticateRequest{
			Scope: scope, Presented: presented, PrincipalID: "service-a",
			ServiceID: "service-a", Now: pgtest.FixtureTime,
		})
		if err != nil {
			t.Fatalf("%s: Authenticate: %v%s", label, err, pgtest.CauseText(err))
		}
		if authenticated.PrincipalID != "service-a" || authenticated.ServiceID != "service-a" ||
			authenticated.Kind != identity.KindService || authenticated.Scope != scope ||
			authenticated.ExecutionID != "" || !authenticated.Enabled || authenticated.Version != version ||
			!sameRoles(authenticated.Roles, []tenancy.Role{tenancy.RoleEngineer}) {
			t.Fatalf("%s: authenticated = %+v; want enabled service-a at version %d with the engineer role",
				label, authenticated, version)
		}
	}
	requireRefused := func(label, presented string) {
		t.Helper()
		authenticated, err := manager.Authenticate(ctx, token.AuthenticateRequest{
			Scope: scope, Presented: presented, PrincipalID: "service-a",
			ServiceID: "service-a", Now: pgtest.FixtureTime,
		})
		if class, ok := fault.ClassOf(err); !ok || class != fault.Unauthenticated {
			t.Fatalf("%s: Authenticate = %+v, %v; want the unauthenticated refusal", label, authenticated, err)
		}
	}
	readServiceNonce := func(label string) []byte {
		t.Helper()
		encoded, err := pgtest.ScalarString(ctx, shard.Admin(),
			`SELECT COALESCE(encode(token_nonce,'hex'),'') FROM services WHERE id = 'service-a'`)
		if err != nil {
			t.Fatalf("%s: read token_nonce: %v", label, err)
		}
		nonce, err := hex.DecodeString(encoded)
		if err != nil || len(nonce) != 32 {
			t.Fatalf("%s: token_nonce = %q (err=%v); want 32 bytes", label, encoded, err)
		}
		if bytes.Equal(nonce, make([]byte, 32)) {
			t.Fatalf("%s: stored token_nonce is all zero", label)
		}
		return nonce
	}
	serviceCreateObserver := shard.ObserveUnit(t, "identity", "service.create")
	created, err := serviceAdmin.Create(ctx, service.CreateCommand{
		Actor: actor, ID: "service-a", Scope: scope, State: service.StateEnabled,
		Roles: []tenancy.Role{tenancy.RoleEngineer}, CallerKey: "service-create",
		Content: adminContent("service-create"),
	})
	if err != nil || created.Version != 1 {
		t.Fatalf("service Create: %v (version=%d)", err, created.Version)
	}
	createdRoles, err := pgtest.ScalarString(ctx, shard.Admin(),
		`SELECT roles::text FROM services WHERE id = 'service-a'`)
	if err != nil || createdRoles != `["engineer"]` {
		t.Fatalf("durable service roles = %q, err=%v; want [\"engineer\"]", createdRoles, err)
	}
	shard.RequireGoldenRow(t, ctx, runtime, scope, "services", map[string]any{"id": "service-a"}, map[string]any{
		"id": "service-a", "tenant_id": "tenant-a", "project_id": "project-a",
		"name": "service-a", "state": "enabled", "roles": `["engineer"]`,
		"token_nonce": pgtest.NullValue, "token_expires_at": pgtest.NullValue,
		"version": int64(1), "created_at": pgtest.FixtureTime,
	})
	shard.RequireGoldenRow(t, ctx, runtime, scope, "receipts", map[string]any{"id": "receipt-1"}, map[string]any{
		"id": "receipt-1", "tenant_id": "tenant-a", "project_id": "project-a",
		"principal_id": "principal-admin", "operation": "service.create", "caller_key": "service-create",
		"content_sha256": pgtest.ContentSHA256(t, adminContent("service-create")),
		"result":         pgtest.GoldenJSONOf(service.CreateResult{ServiceID: "service-a", Version: 1}),
		"completed_at":   pgtest.FixtureTime, "execution_id": pgtest.NullValue,
		"snapshot_sha256": pgtest.NullValue, "created_at": pgtest.FixtureTime,
	})
	shard.RequireGoldenRow(t, ctx, runtime, scope, "audit", map[string]any{"id": "event-1"}, map[string]any{
		"id": "event-1", "tenant_id": "tenant-a", "project_id": "project-a",
		"actor": "principal-admin", "operation": "service.create", "at": pgtest.FixtureTime, "detail": pgtest.NullValue,
	})
	shard.RequireGoldenRow(t, ctx, runtime, scope, "events", map[string]any{"id": "event-2"}, map[string]any{
		"id": "event-2", "tenant_id": "tenant-a", "project_id": "project-a",
		"type": "service.changed", "report_type": pgtest.NullValue,
		"subject_id": "service-a", "subject_version": int64(1),
		"source_kind": "shell", "source_principal": "principal-admin",
		"correlation_id": pgtest.NullValue, "causation_id": pgtest.NullValue,
		"at": pgtest.FixtureTime, "data": pgtest.NullValue, "publish_state": "pending",
		"publish_sequence": pgtest.GoldenDBSequence(),
	})
	serviceCreateObserver.Finish(map[string]int{"services": 1, "receipts": 1, "audit": 1, "events": 1})
	issueObserver := shard.ObserveUnit(t, "identity", "credential.issue")
	issued, err := credentialAdmin.Issue(ctx, token.ServiceCredentialCommand{
		Actor: actor, Scope: scope, ServiceID: "service-a", TTL: time.Hour,
		ExpectedVersion: 1, CallerKey: "credential-issue", Content: adminContent("credential-issue"),
	})
	if err != nil || issued.Version != 2 || issued.Token == "" {
		t.Fatalf("credential Issue: %v (version=%d token=%d bytes)", err, issued.Version, len(issued.Token))
	}
	issuedPair := scopedRow(t, ctx, runtime, scope, 2,
		`SELECT octet_length(token_nonce)::text, extract(epoch FROM token_expires_at)::bigint::text
		 FROM services WHERE id = 'service-a'`)
	if issuedPair[0] != "32" || issuedPair[1] != strconv.FormatInt(pgtest.FixtureTime.Add(time.Hour).Unix(), 10) {
		t.Fatalf("issued credential pair = %v; want a 32-byte nonce and expiry %d", issuedPair, pgtest.FixtureTime.Add(time.Hour).Unix())
	}
	issuedNonce := readServiceNonce("after issue")
	if !bytes.Equal(issuedNonce, credentialCountingBytes(0, 32)) {
		t.Fatalf("issued token_nonce = %x; want the first 32 bytes of the injected random stream", issuedNonce)
	}
	shard.RequireGoldenRow(t, ctx, runtime, scope, "services", map[string]any{"id": "service-a"}, map[string]any{
		"id": "service-a", "tenant_id": "tenant-a", "project_id": "project-a",
		"name": "service-a", "state": "enabled", "roles": `["engineer"]`,
		"token_nonce": pgtest.GoldenRandomBytes(32), "token_expires_at": pgtest.FixtureTime.Add(time.Hour),
		"version": int64(2), "created_at": pgtest.FixtureTime,
	})
	shard.RequireGoldenRow(t, ctx, runtime, scope, "receipts", map[string]any{"id": "receipt-2"}, map[string]any{
		"id": "receipt-2", "tenant_id": "tenant-a", "project_id": "project-a",
		"principal_id": "principal-admin", "operation": "service.credential.issue", "caller_key": "credential-issue",
		"content_sha256": pgtest.ContentSHA256(t, adminContent("credential-issue")),
		"result":         pgtest.GoldenJSONOf(token.IssueResult{State: token.StateCurrent, Version: 2}),
		"completed_at":   pgtest.FixtureTime, "execution_id": pgtest.NullValue,
		"snapshot_sha256": pgtest.NullValue, "created_at": pgtest.FixtureTime,
	})
	shard.RequireGoldenRow(t, ctx, runtime, scope, "audit", map[string]any{"id": "event-3"}, map[string]any{
		"id": "event-3", "tenant_id": "tenant-a", "project_id": "project-a",
		"actor": "principal-admin", "operation": "service.credential.issue", "at": pgtest.FixtureTime, "detail": pgtest.NullValue,
	})
	shard.RequireGoldenRow(t, ctx, runtime, scope, "events", map[string]any{"id": "event-4"}, map[string]any{
		"id": "event-4", "tenant_id": "tenant-a", "project_id": "project-a",
		"type": "service.changed", "report_type": pgtest.NullValue,
		"subject_id": "service-a", "subject_version": int64(2),
		"source_kind": "shell", "source_principal": "principal-admin",
		"correlation_id": pgtest.NullValue, "causation_id": pgtest.NullValue,
		"at": pgtest.FixtureTime, "data": pgtest.NullValue, "publish_state": "pending",
		"publish_sequence": pgtest.GoldenDBSequence(),
	})
	issueObserver.Finish(map[string]int{"services": 1, "receipts": 1, "audit": 1, "events": 1})
	requireAuthenticated("issued token", issued.Token, 2)
	replayed, err := credentialAdmin.Issue(ctx, token.ServiceCredentialCommand{
		Actor: actor, Scope: scope, ServiceID: "service-a", TTL: time.Hour,
		ExpectedVersion: 1, CallerKey: "credential-issue", Content: adminContent("credential-issue"),
	})
	if err != nil || !replayed.Replayed || replayed.Token != "" {
		t.Fatalf("credential Issue replay: %v (replayed=%v token=%d)", err, replayed.Replayed, len(replayed.Token))
	}
	// A replay returns the committed secret-free result: the original value
	// without its one-time token, flagged Replayed.
	issuedWithoutToken := issued
	issuedWithoutToken.Token = ""
	requireReplayOf(t, "credential issue replay", issuedWithoutToken, replayed)
	rotateObserver := shard.ObserveUnit(t, "identity", "credential.rotate")
	rotated, err := credentialAdmin.Rotate(ctx, token.ServiceCredentialCommand{
		Actor: actor, Scope: scope, ServiceID: "service-a", TTL: time.Hour,
		ExpectedVersion: 2, CallerKey: "credential-rotate", Content: adminContent("credential-rotate"),
	})
	if err != nil || rotated.Version != 3 || rotated.Token == "" {
		t.Fatalf("credential Rotate: %v (version=%d)", err, rotated.Version)
	}
	rotatedNonce := readServiceNonce("after rotate")
	if bytes.Equal(rotatedNonce, issuedNonce) {
		t.Fatalf("rotated token_nonce %x equals the issued nonce", rotatedNonce)
	}
	if !bytes.Equal(rotatedNonce, credentialCountingBytes(32, 32)) {
		t.Fatalf("rotated token_nonce = %x; want the second 32 bytes of the injected random stream", rotatedNonce)
	}
	shard.RequireGoldenRow(t, ctx, runtime, scope, "services", map[string]any{"id": "service-a"}, map[string]any{
		"id": "service-a", "tenant_id": "tenant-a", "project_id": "project-a",
		"name": "service-a", "state": "enabled", "roles": `["engineer"]`,
		"token_nonce": pgtest.GoldenRandomBytes(32), "token_expires_at": pgtest.FixtureTime.Add(time.Hour),
		"version": int64(3), "created_at": pgtest.FixtureTime,
	})
	shard.RequireGoldenRow(t, ctx, runtime, scope, "receipts", map[string]any{"id": "receipt-4"}, map[string]any{
		"id": "receipt-4", "tenant_id": "tenant-a", "project_id": "project-a",
		"principal_id": "principal-admin", "operation": "service.credential.rotate", "caller_key": "credential-rotate",
		"content_sha256": pgtest.ContentSHA256(t, adminContent("credential-rotate")),
		"result":         pgtest.GoldenJSONOf(token.IssueResult{State: token.StateCurrent, Version: 3}),
		"completed_at":   pgtest.FixtureTime, "execution_id": pgtest.NullValue,
		"snapshot_sha256": pgtest.NullValue, "created_at": pgtest.FixtureTime,
	})
	shard.RequireGoldenRow(t, ctx, runtime, scope, "audit", map[string]any{"id": "event-7"}, map[string]any{
		"id": "event-7", "tenant_id": "tenant-a", "project_id": "project-a",
		"actor": "principal-admin", "operation": "service.credential.rotate", "at": pgtest.FixtureTime, "detail": pgtest.NullValue,
	})
	shard.RequireGoldenRow(t, ctx, runtime, scope, "events", map[string]any{"id": "event-8"}, map[string]any{
		"id": "event-8", "tenant_id": "tenant-a", "project_id": "project-a",
		"type": "service.changed", "report_type": pgtest.NullValue,
		"subject_id": "service-a", "subject_version": int64(3),
		"source_kind": "shell", "source_principal": "principal-admin",
		"correlation_id": pgtest.NullValue, "causation_id": pgtest.NullValue,
		"at": pgtest.FixtureTime, "data": pgtest.NullValue, "publish_state": "pending",
		"publish_sequence": pgtest.GoldenDBSequence(),
	})
	rotateObserver.Finish(map[string]int{"services": 1, "receipts": 1, "audit": 1, "events": 1})
	requireRefused("old token after rotate", issued.Token)
	requireAuthenticated("rotated token", rotated.Token, 3)
	revokeObserver := shard.ObserveUnit(t, "identity", "credential.revoke")
	revoked, err := credentialAdmin.Revoke(ctx, token.ServiceCredentialRevokeCommand{
		Actor: actor, Scope: scope, ServiceID: "service-a",
		ExpectedVersion: 3, CallerKey: "credential-revoke", Content: adminContent("credential-revoke"),
	})
	if err != nil || revoked.Mutation.Version != 4 || revoked.Mutation.State != token.StateAbsent {
		t.Fatalf("credential Revoke: %v (version=%d state=%s)", err, revoked.Mutation.Version, revoked.Mutation.State)
	}
	nonce, err := pgtest.ScalarString(ctx, shard.Admin(), `SELECT COALESCE(encode(token_nonce,'hex'),'') FROM services WHERE id = 'service-a'`)
	if err != nil || nonce != "" {
		t.Fatalf("service token_nonce after revoke = %q, err=%v; want empty", nonce, err)
	}
	shard.RequireGoldenRow(t, ctx, runtime, scope, "services", map[string]any{"id": "service-a"}, map[string]any{
		"id": "service-a", "tenant_id": "tenant-a", "project_id": "project-a",
		"name": "service-a", "state": "enabled", "roles": `["engineer"]`,
		"token_nonce": pgtest.NullValue, "token_expires_at": pgtest.NullValue,
		"version": int64(4), "created_at": pgtest.FixtureTime,
	})
	shard.RequireGoldenRow(t, ctx, runtime, scope, "receipts", map[string]any{"id": "receipt-5"}, map[string]any{
		"id": "receipt-5", "tenant_id": "tenant-a", "project_id": "project-a",
		"principal_id": "principal-admin", "operation": "service.credential.revoke", "caller_key": "credential-revoke",
		"content_sha256": pgtest.ContentSHA256(t, adminContent("credential-revoke")),
		// The receipt keeps the secret-free mutation; the post-commit
		// invalidation is not part of the durable result.
		"result": pgtest.GoldenJSONOf(token.RevokeResult{
			Mutation: token.MutationResult{State: token.StateAbsent, Version: 4},
		}),
		"completed_at": pgtest.FixtureTime, "execution_id": pgtest.NullValue,
		"snapshot_sha256": pgtest.NullValue, "created_at": pgtest.FixtureTime,
	})
	shard.RequireGoldenRow(t, ctx, runtime, scope, "audit", map[string]any{"id": "event-9"}, map[string]any{
		"id": "event-9", "tenant_id": "tenant-a", "project_id": "project-a",
		"actor": "principal-admin", "operation": "service.credential.revoke", "at": pgtest.FixtureTime, "detail": pgtest.NullValue,
	})
	shard.RequireGoldenRow(t, ctx, runtime, scope, "events", map[string]any{"id": "event-10"}, map[string]any{
		"id": "event-10", "tenant_id": "tenant-a", "project_id": "project-a",
		"type": "service.changed", "report_type": pgtest.NullValue,
		"subject_id": "service-a", "subject_version": int64(4),
		"source_kind": "shell", "source_principal": "principal-admin",
		"correlation_id": pgtest.NullValue, "causation_id": pgtest.NullValue,
		"at": pgtest.FixtureTime, "data": pgtest.NullValue, "publish_state": "pending",
		"publish_sequence": pgtest.GoldenDBSequence(),
	})
	revokeObserver.Finish(map[string]int{"services": 1, "receipts": 1, "audit": 1, "events": 1})
	requireRefused("old token after revoke", issued.Token)
	requireRefused("rotated token after revoke", rotated.Token)
	// A disable commits the disabled state together with the cleared pair.
	if _, err := serviceAdmin.Create(ctx, service.CreateCommand{
		Actor: actor, ID: "service-b", Scope: scope, State: service.StateEnabled,
		Roles: []tenancy.Role{tenancy.RoleEngineer}, CallerKey: "service-b-create",
		Content: adminContent("service-b-create"),
	}); err != nil {
		t.Fatalf("service-b Create: %v", err)
	}
	if _, err := credentialAdmin.Issue(ctx, token.ServiceCredentialCommand{
		Actor: actor, Scope: scope, ServiceID: "service-b", TTL: time.Hour,
		ExpectedVersion: 1, CallerKey: "credential-b-issue", Content: adminContent("credential-b-issue"),
	}); err != nil {
		t.Fatalf("service-b Issue: %v", err)
	}
	disableObserver := shard.ObserveUnit(t, "identity", "service.disable")
	disabled, err := serviceAdmin.Disable(ctx, service.StatusCommand{
		Actor: actor, Scope: scope, ServiceID: "service-b",
		ExpectedVersion: 2, CallerKey: "service-b-disable", Content: adminContent("service-b-disable"),
	})
	if err != nil || disabled.State != service.StateDisabled {
		t.Fatalf("service-b Disable: %v (state=%s)", err, disabled.State)
	}
	durable := scopedRow(t, ctx, runtime, scope, 3,
		`SELECT state, (token_nonce IS NULL)::text, (token_expires_at IS NULL)::text
		 FROM services WHERE id = 'service-b'`)
	shard.RequireGoldenRow(t, ctx, runtime, scope, "services", map[string]any{"id": "service-b"}, map[string]any{
		"id": "service-b", "tenant_id": "tenant-a", "project_id": "project-a",
		"name": "service-b", "state": "disabled", "roles": `["engineer"]`,
		"token_nonce": pgtest.NullValue, "token_expires_at": pgtest.NullValue,
		"version": int64(3), "created_at": pgtest.FixtureTime,
	})
	shard.RequireGoldenRow(t, ctx, runtime, scope, "receipts", map[string]any{"id": "receipt-8"}, map[string]any{
		"id": "receipt-8", "tenant_id": "tenant-a", "project_id": "project-a",
		"principal_id": "principal-admin", "operation": "service.disable", "caller_key": "service-b-disable",
		"content_sha256": pgtest.ContentSHA256(t, adminContent("service-b-disable")),
		"result": pgtest.GoldenJSONOf(service.StatusResult{
			State: service.StateDisabled, Version: 3, RevokedCredentials: 1,
		}),
		"completed_at": pgtest.FixtureTime, "execution_id": pgtest.NullValue,
		"snapshot_sha256": pgtest.NullValue, "created_at": pgtest.FixtureTime,
	})
	shard.RequireGoldenRow(t, ctx, runtime, scope, "audit", map[string]any{"id": "event-15"}, map[string]any{
		"id": "event-15", "tenant_id": "tenant-a", "project_id": "project-a",
		"actor": "principal-admin", "operation": "service.disable", "at": pgtest.FixtureTime, "detail": pgtest.NullValue,
	})
	shard.RequireGoldenRow(t, ctx, runtime, scope, "events", map[string]any{"id": "event-16"}, map[string]any{
		"id": "event-16", "tenant_id": "tenant-a", "project_id": "project-a",
		"type": "service.changed", "report_type": pgtest.NullValue,
		"subject_id": "service-b", "subject_version": int64(3),
		"source_kind": "shell", "source_principal": "principal-admin",
		"correlation_id": pgtest.NullValue, "causation_id": pgtest.NullValue,
		"at": pgtest.FixtureTime, "data": pgtest.NullValue, "publish_state": "pending",
		"publish_sequence": pgtest.GoldenDBSequence(),
	})
	disableObserver.Finish(map[string]int{"services": 1, "receipts": 1, "audit": 1, "events": 1})
	if durable[0] != "disabled" || durable[1] != "true" || durable[2] != "true" {
		t.Fatalf("disabled service durable row = %v; want disabled and a cleared token pair", durable)
	}
	updateObserver := shard.ObserveUnit(t, "identity", "service.update")
	updatedService, err := serviceAdmin.Update(ctx, service.UpdateCommand{
		Actor: actor, Scope: scope, ServiceID: "service-a",
		Roles:           []tenancy.Role{tenancy.RoleEngineer, tenancy.RoleViewer},
		ExpectedVersion: 4, CallerKey: "service-update", Content: adminContent("service-update"),
	})
	if err != nil {
		t.Fatalf("service Update: %v", err)
	}
	if updatedService != (service.UpdateResult{Version: 5}) {
		t.Fatalf("service Update result = %+v; want version 5, not replayed", updatedService)
	}
	shard.RequireGoldenRow(t, ctx, runtime, scope, "services", map[string]any{"id": "service-a"}, map[string]any{
		"id": "service-a", "tenant_id": "tenant-a", "project_id": "project-a",
		"name": "service-a", "state": "enabled", "roles": `["engineer","viewer"]`,
		"token_nonce": pgtest.NullValue, "token_expires_at": pgtest.NullValue,
		"version": int64(5), "created_at": pgtest.FixtureTime,
	})
	shard.RequireGoldenRow(t, ctx, runtime, scope, "receipts", map[string]any{"id": "receipt-9"}, map[string]any{
		"id": "receipt-9", "tenant_id": "tenant-a", "project_id": "project-a",
		"principal_id": "principal-admin", "operation": "service.update", "caller_key": "service-update",
		"content_sha256": pgtest.ContentSHA256(t, adminContent("service-update")),
		"result":         pgtest.GoldenJSONOf(service.UpdateResult{Version: 5}),
		"completed_at":   pgtest.FixtureTime, "execution_id": pgtest.NullValue,
		"snapshot_sha256": pgtest.NullValue, "created_at": pgtest.FixtureTime,
	})
	shard.RequireGoldenRow(t, ctx, runtime, scope, "audit", map[string]any{"id": "event-17"}, map[string]any{
		"id": "event-17", "tenant_id": "tenant-a", "project_id": "project-a",
		"actor": "principal-admin", "operation": "service.update", "at": pgtest.FixtureTime, "detail": pgtest.NullValue,
	})
	shard.RequireGoldenRow(t, ctx, runtime, scope, "events", map[string]any{"id": "event-18"}, map[string]any{
		"id": "event-18", "tenant_id": "tenant-a", "project_id": "project-a",
		"type": "service.changed", "report_type": pgtest.NullValue,
		"subject_id": "service-a", "subject_version": int64(5),
		"source_kind": "shell", "source_principal": "principal-admin",
		"correlation_id": pgtest.NullValue, "causation_id": pgtest.NullValue,
		"at": pgtest.FixtureTime, "data": pgtest.NullValue, "publish_state": "pending",
		"publish_sequence": pgtest.GoldenDBSequence(),
	})
	updateObserver.Finish(map[string]int{"services": 1, "receipts": 1, "audit": 1, "events": 1})
	enableObserver := shard.ObserveUnit(t, "identity", "service.enable")
	enabled, err := serviceAdmin.Enable(ctx, service.StatusCommand{
		Actor: actor, Scope: scope, ServiceID: "service-b", ExpectedVersion: 3,
		CallerKey: "service-b-enable", Content: adminContent("service-b-enable"),
	})
	if err != nil {
		t.Fatalf("service Enable: %v", err)
	}
	if enabled != (service.StatusResult{State: service.StateEnabled, Version: 4}) {
		t.Fatalf("service Enable result = %+v; want enabled at version 4 with no revoked credentials", enabled)
	}
	shard.RequireGoldenRow(t, ctx, runtime, scope, "services", map[string]any{"id": "service-b"}, map[string]any{
		"id": "service-b", "tenant_id": "tenant-a", "project_id": "project-a",
		"name": "service-b", "state": "enabled", "roles": `["engineer"]`,
		"token_nonce": pgtest.NullValue, "token_expires_at": pgtest.NullValue,
		"version": int64(4), "created_at": pgtest.FixtureTime,
	})
	shard.RequireGoldenRow(t, ctx, runtime, scope, "receipts", map[string]any{"id": "receipt-10"}, map[string]any{
		"id": "receipt-10", "tenant_id": "tenant-a", "project_id": "project-a",
		"principal_id": "principal-admin", "operation": "service.enable", "caller_key": "service-b-enable",
		"content_sha256": pgtest.ContentSHA256(t, adminContent("service-b-enable")),
		"result":         pgtest.GoldenJSONOf(service.StatusResult{State: service.StateEnabled, Version: 4}),
		"completed_at":   pgtest.FixtureTime, "execution_id": pgtest.NullValue,
		"snapshot_sha256": pgtest.NullValue, "created_at": pgtest.FixtureTime,
	})
	shard.RequireGoldenRow(t, ctx, runtime, scope, "audit", map[string]any{"id": "event-19"}, map[string]any{
		"id": "event-19", "tenant_id": "tenant-a", "project_id": "project-a",
		"actor": "principal-admin", "operation": "service.enable", "at": pgtest.FixtureTime, "detail": pgtest.NullValue,
	})
	shard.RequireGoldenRow(t, ctx, runtime, scope, "events", map[string]any{"id": "event-20"}, map[string]any{
		"id": "event-20", "tenant_id": "tenant-a", "project_id": "project-a",
		"type": "service.changed", "report_type": pgtest.NullValue,
		"subject_id": "service-b", "subject_version": int64(4),
		"source_kind": "shell", "source_principal": "principal-admin",
		"correlation_id": pgtest.NullValue, "causation_id": pgtest.NullValue,
		"at": pgtest.FixtureTime, "data": pgtest.NullValue, "publish_state": "pending",
		"publish_sequence": pgtest.GoldenDBSequence(),
	})
	enableObserver.Finish(map[string]int{"services": 1, "receipts": 1, "audit": 1, "events": 1})
	_ = identity.KindService
	_ = authorize.DecisionAllow
	_ = receipt.Key{}
}

// TestAuthenticateStoreOutageIsDependencyUnavailableOnPostgreSQL proves the
// authentication boundary over a real closed pool reports a retryable
// dependency outage, not the unauthenticated "revoked credential" refusal. A
// genuinely absent credential row still produces the uniform refusal. Closing
// the test-owned pool is the cheapest faithful outage: every current-state read
// fails with the driver's own error, exactly as it does when PostgreSQL is down.
func TestAuthenticateStoreOutageIsDependencyUnavailableOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	shard := migratedShard(t)
	scope := pgtest.Scope("tenant-1", "project-1")
	shard.SeedScope(ctx, "tenant-1", "project-1")
	shard.SeedExecution(ctx, scope, "execution-1", "running", 1, "", "")
	runtime := shard.Runtime(4)
	defer runtime.Close()
	scoped := storepg.New(runtime)
	adapter, err := identityunits.New(identityunits.Config{
		Executor: scoped, Scanner: scoped,
		Clock: integrationClock{now: pgtest.FixtureTime}, IDs: &identityIDs{},
	})
	if err != nil {
		t.Fatalf("identity.New: %v", err)
	}
	receipts, err := readers.NewReceiptLookup(scoped)
	if err != nil {
		t.Fatalf("NewReceiptLookup: %v", err)
	}
	manager, err := token.NewManager(token.ManagerConfig{
		Keys: adapter, Resolver: plainResolver{}, Credentials: adapter,
		Principals: adapter, Services: adapter, Mutations: adapter, Receipts: receipts,
		Deliverer: recordingDeliverer{}, Invalidations: recordingTokenInvalidator{},
		Clock:  integrationClock{now: pgtest.FixtureTime},
		Random: credentialCountingRandom(64), Bounds: token.DefaultBounds,
	})
	if err != nil {
		t.Fatalf("token.NewManager: %v", err)
	}
	authenticate := func(executionID string) error {
		t.Helper()
		_, err := manager.Authenticate(ctx, token.AuthenticateRequest{
			Scope:       scope,
			Presented:   token.ExecutionPrefix + executionID + ".placeholder",
			PrincipalID: "principal-1",
			ExecutionID: id.ExecutionID(executionID),
			Now:         pgtest.FixtureTime,
		})
		return err
	}
	if err := authenticate("execution-missing"); err != nil {
		if class, ok := fault.ClassOf(err); !ok || class != fault.Unauthenticated {
			t.Fatalf("absent credential error = %v; want the unauthenticated refusal", err)
		}
	} else {
		t.Fatal("absent credential authenticated")
	}
	runtime.Close()
	err = authenticate("execution-1")
	if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
		t.Fatalf("closed-pool error = %v; want the retryable dependency outage", err)
	}
}
