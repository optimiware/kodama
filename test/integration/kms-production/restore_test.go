package kmsproduction

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/internal/secrets/envelope"
	"github.com/optimiware/kodama/internal/secrets/kms/production"
)

// Test_kms_restored_envelope: adapter metadata and a value envelope restored
// from backup, unchanged since before rotation, open with a freshly
// constructed adapter that still retains the old key version.
func Test_kms_restored_envelope(t *testing.T) {
	for _, c := range clouds() {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			s := &sim{}
			clients := c.clients(t, s)
			v1, err := production.New(ctx, c.config("v1", "v1"), clients)
			if err != nil {
				t.Fatal(err)
			}
			sk, material, env := newScopeKey(t, v1)

			// "Backup": persisted scope-key record and value envelope as JSON.
			recordBytes, err := json.Marshal(sk.Record())
			if err != nil {
				t.Fatal(err)
			}
			envBytes, err := json.Marshal(env)
			if err != nil {
				t.Fatal(err)
			}
			// Two rotations later, a new process starts from configuration only.
			v3, err := production.New(ctx, c.config("v3", "v1", "v2", "v3"), clients)
			if err != nil {
				t.Fatal(err)
			}
			var rec secrets.ScopeKeyRecord
			if err := json.Unmarshal(recordBytes, &rec); err != nil {
				t.Fatal(err)
			}
			restored, err := secrets.RestoreScopeKey(rec)
			if err != nil {
				t.Fatal(err)
			}
			var restoredEnv envelope.Envelope
			if err := json.Unmarshal(envBytes, &restoredEnv); err != nil {
				t.Fatal(err)
			}
			got, err := v3.Unwrap(ctx, restored.WrapContext(installation), restored.Wrapped())
			if err != nil || !bytes.Equal(got, material) {
				t.Fatal("restored wrap did not unwrap", err)
			}
			plain, err := envelope.Open(got, valueContext(), restoredEnv)
			if err != nil || string(plain) != "super-secret-value" {
				t.Fatal("restored value did not open", err)
			}

			// Another installation or tenant cannot adopt the restored wrap.
			if _, err := v3.Unwrap(ctx, secrets.WrapContext{InstallationID: "other", TenantID: restored.TenantID(), Generation: 1, KeyID: "sk_1"}, restored.Wrapped()); err == nil {
				t.Fatal("foreign installation adopted the wrap")
			}
			if _, err := v3.Unwrap(ctx, secrets.WrapContext{InstallationID: installation, TenantID: domain.TenantID("t_other"), Generation: 1, KeyID: "sk_1"}, restored.Wrapped()); err == nil {
				t.Fatal("foreign tenant adopted the wrap")
			}

			// Removing the retained version fails closed. Startup still succeeds
			// because the probe covers only the current key: retained-version
			// qualification is a separate operator check (README).
			dropped, err := production.New(ctx, c.config("v3", "v2", "v3"), clients)
			if err != nil {
				t.Fatal(err)
			}
			before := s.calls.Load()
			key, err := dropped.Unwrap(ctx, restored.WrapContext(installation), restored.Wrapped())
			var ke *secrets.KeyError
			if !errors.As(err, &ke) || ke.Class != secrets.KeyNotFound || key != nil || s.calls.Load() != before {
				t.Fatalf("dropped retained version: %v", err)
			}
		})
	}
}
