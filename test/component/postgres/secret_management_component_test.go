//go:build component

package postgrescomponent

import (
	"context"
	"crypto/rand"
	"fmt"
	"testing"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/integration/keys"
	"github.com/optimiware/kodama/internal/shell/secret"
	storepg "github.com/optimiware/kodama/internal/shell/store/postgres"
	"github.com/optimiware/kodama/internal/shell/store/units/platform"
	"github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

// secretIDs allocates deterministic identities for the secret management unit.
type secretIDs struct {
	secret  int
	receipt int
	event   int
}

func (ids *secretIDs) NewSecretID() (secret.SecretID, error) {
	ids.secret++
	return secret.SecretID(fmt.Sprintf("secret-%d", ids.secret)), nil
}

func (ids *secretIDs) NewReceiptID() (id.ReceiptID, error) {
	ids.receipt++
	return id.ReceiptID(fmt.Sprintf("receipt-%d", ids.receipt)), nil
}

func (ids *secretIDs) NewEventID() (id.EventID, error) {
	ids.event++
	return id.EventID(fmt.Sprintf("event-%d", ids.event)), nil
}

// TestObservedSecretWriteAndDeleteOnPostgreSQL observes the guarded secret
// write and delete commit units, including the absence of the deleted row.
func TestObservedSecretWriteAndDeleteOnPostgreSQL(t *testing.T) {
	ctx := context.Background()
	env := newServiceEnv(t)
	base := storepg.New(env.runtime)
	lookup, err := platform.NewSecretLookup(base)
	if err != nil {
		t.Fatalf("NewSecretLookup: %v", err)
	}
	provider, err := keys.NewEphemeral(rand.Reader)
	if err != nil {
		t.Fatalf("NewEphemeral: %v", err)
	}
	cipher, err := keys.NewCipher(provider, rand.Reader)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	manager, err := secret.NewManager(secret.ManagerConfig{
		InstallationID: "installation-1", KeyVersion: 1,
		Clock: integrationClock{now: pgtest.FixtureTime}, IDs: &secretIDs{},
		Executor: base, Scanner: base, Cipher: cipher, Lookup: lookup,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	writeObserver := env.shard.ObserveUnit(t, "secret", "write")
	written, err := manager.Put(ctx, secret.PutCommand{
		Scope: env.scope, Name: "DEPLOY_KEY", Value: "plaintext",
		Actor: "principal-1", ActorKind: identity.KindHuman,
		CallerKey: "secret-write", Content: integrationContent("secret-write"),
	})
	if err != nil {
		t.Fatalf("secret Put: %v%s", err, pgtest.CauseText(err))
	}
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "secrets", map[string]any{"id": string(written.ID)}, map[string]any{
		"id": string(written.ID), "tenant_id": "tenant-1", "project_id": "project-1",
		"name": "DEPLOY_KEY", "ciphertext": pgtest.GoldenRandomBytes(len("plaintext") + 16),
		"wrapped_key": pgtest.GoldenRandomBytes(secret.WrappedKeyBytes), "wrap_nonce": pgtest.GoldenRandomBytes(secret.WrapNonceBytes),
		"key_ref": string(provider.KeyRef()), "key_version": int64(1), "nonce": pgtest.GoldenRandomBytes(secret.ContentNonceBytes),
		"version": int64(1), "created_at": pgtest.FixtureTime, "updated_at": pgtest.FixtureTime,
	})
	// The stored randomness is only meaningful if it decrypts: open the stored
	// envelope through the real cipher under the exact scoped identity.
	stored, err := lookup.Lookup(ctx, env.scope, "DEPLOY_KEY")
	if err != nil {
		t.Fatalf("secret Lookup: %v%s", err, pgtest.CauseText(err))
	}
	plaintext, err := cipher.Open(ctx, secret.AssociatedData{
		InstallationID: "installation-1", TenantID: env.scope.TenantID, ProjectID: env.scope.ProjectID,
		SecretID: written.ID, Name: "DEPLOY_KEY", KeyVersion: 1,
	}, stored.Envelope)
	if err != nil {
		t.Fatalf("open the stored envelope: %v%s", err, pgtest.CauseText(err))
	}
	if string(plaintext) != "plaintext" {
		t.Fatalf("stored envelope decrypts to %q; want the written value", plaintext)
	}
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "receipts", map[string]any{"id": "receipt-1"}, map[string]any{
		"id": "receipt-1", "tenant_id": "tenant-1", "project_id": "project-1",
		"principal_id": "principal-1", "operation": "secret.write", "caller_key": "secret-write",
		"content_sha256": pgtest.ContentSHA256(t, integrationContent("secret-write")), "result": pgtest.GoldenJSONOf(written),
		"completed_at": pgtest.FixtureTime, "execution_id": pgtest.NullValue,
		"snapshot_sha256": pgtest.NullValue, "created_at": pgtest.FixtureTime,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "audit", map[string]any{"id": "event-1"}, map[string]any{
		"id": "event-1", "tenant_id": "tenant-1", "project_id": "project-1",
		"actor": "principal-1", "operation": "secret.write", "at": pgtest.FixtureTime, "detail": pgtest.NullValue,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "events", map[string]any{"id": "event-2"}, map[string]any{
		"id": "event-2", "tenant_id": "tenant-1", "project_id": "project-1",
		"type": "config.changed", "report_type": pgtest.NullValue,
		"subject_id": string(written.ID), "subject_version": int64(1),
		"source_kind": "shell", "source_principal": "principal-1",
		"correlation_id": pgtest.NullValue, "causation_id": pgtest.NullValue,
		"at": pgtest.FixtureTime, "data": pgtest.NullValue, "publish_state": "pending",
		"publish_sequence": pgtest.GoldenDBSequence(),
	})
	writeObserver.Finish(map[string]int{"secrets": 1, "receipts": 1, "audit": 1, "events": 1})
	deleteObserver := env.shard.ObserveUnit(t, "secret", "delete")
	if err := manager.Delete(ctx, secret.DeleteCommand{
		Scope: env.scope, Name: "DEPLOY_KEY", ExpectedVersion: 1,
		Actor: "principal-1", ActorKind: identity.KindHuman,
		CallerKey: "secret-delete", Content: integrationContent("secret-delete"),
	}); err != nil {
		t.Fatalf("secret Delete: %v%s", err, pgtest.CauseText(err))
	}
	env.shard.RequireRowAbsent(t, ctx, env.runtime, env.scope, "secrets", map[string]any{"id": string(written.ID)})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "receipts", map[string]any{"id": "receipt-2"}, map[string]any{
		"id": "receipt-2", "tenant_id": "tenant-1", "project_id": "project-1",
		"principal_id": "principal-1", "operation": "secret.delete", "caller_key": "secret-delete",
		// derived: a delete commits a nil result, which the manager encodes as
		// the JSON document null (internal/shell/secret/manager.go commitUnit).
		"content_sha256": pgtest.ContentSHA256(t, integrationContent("secret-delete")), "result": []byte("null"),
		"completed_at": pgtest.FixtureTime, "execution_id": pgtest.NullValue,
		"snapshot_sha256": pgtest.NullValue, "created_at": pgtest.FixtureTime,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "audit", map[string]any{"id": "event-3"}, map[string]any{
		"id": "event-3", "tenant_id": "tenant-1", "project_id": "project-1",
		"actor": "principal-1", "operation": "secret.delete", "at": pgtest.FixtureTime, "detail": pgtest.NullValue,
	})
	env.shard.RequireGoldenRow(t, ctx, env.runtime, env.scope, "events", map[string]any{"id": "event-4"}, map[string]any{
		"id": "event-4", "tenant_id": "tenant-1", "project_id": "project-1",
		"type": "config.changed", "report_type": pgtest.NullValue,
		"subject_id": string(written.ID), "subject_version": int64(1),
		"source_kind": "shell", "source_principal": "principal-1",
		"correlation_id": pgtest.NullValue, "causation_id": pgtest.NullValue,
		"at": pgtest.FixtureTime, "data": pgtest.NullValue, "publish_state": "pending",
		"publish_sequence": pgtest.GoldenDBSequence(),
	})
	deleteObserver.Finish(map[string]int{"secrets": 1, "receipts": 1, "audit": 1, "events": 1})
	_ = written
}
