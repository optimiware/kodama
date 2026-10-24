package postgres_test

import (
	"bytes"
	"errors"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/secrets"
)

// Test_pinned_rotation is acceptance pinned_rotation (VS-07, VS-A05): rotation
// preserves old pins, rewrap changes neither pinned bytes nor value versions,
// and a missing pinned version never resolves to the current one.
func Test_pinned_rotation(t *testing.T) {
	e := newEnv(t)
	ctx := e.ctx()
	const v1, v2, v3 = "value-one-SENTINEL", "value-two-SENTINEL", "value-three-SENTINEL"

	sec := e.createSecret(scopeA1, "erp-token", v1)
	if sec.CurrentVersion() != 1 || sec.Version() != 1 {
		t.Fatalf("created at %d/%d", sec.CurrentVersion(), sec.Version())
	}
	// Run 1 pins version 1 at admission.
	if err := e.pin(scopeA1, "run_1", "erp-token", 1); err != nil {
		t.Fatal(err)
	}
	// Rotation: new versions never replace a pinned one.
	sec = e.rotate(scopeA1, sec.ID(), v2)
	if sec.CurrentVersion() != 2 || sec.Version() != 2 {
		t.Fatalf("rotated to %d/%d", sec.CurrentVersion(), sec.Version())
	}
	if err := e.pin(scopeA1, "run_2", "erp-token", 2); err != nil {
		t.Fatal(err)
	}

	auth := newAuthority()
	auth.set(secrets.AttemptGrant{Scope: scopeA1, RunID: "run_1", AttemptID: "att_1", Bindings: []runtimeBinding{binding("erp-token", 1)}})
	auth.set(secrets.AttemptGrant{Scope: scopeA1, RunID: "run_2", AttemptID: "att_2", Bindings: []runtimeBinding{binding("erp-token", 2)}})
	// att_3 declares version 1 but its run never pinned anything.
	auth.set(secrets.AttemptGrant{Scope: scopeA1, RunID: "run_3", AttemptID: "att_3", Bindings: []runtimeBinding{binding("erp-token", 1)}})
	r := e.resolver(auth)

	resolve := func(attempt string, name string, version int64) (string, error) {
		resp, err := r.Resolve(ctx, domainAttempt(attempt), runtimev1.SecretResolveRequest{Ref: "secret://project/" + name, Version: version}, "req_1")
		if err != nil {
			return "", err
		}
		return decode(t, resp), nil
	}
	snapshot := func() []byte {
		var b []byte
		rows, err := e.base.Migration.Query(ctx, `SELECT secret_id, version, nonce || ciphertext, scope_key_id, key_generation FROM secret_versions ORDER BY secret_id, version`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var id, key string
			var ver int64
			var blob []byte
			var gen int
			if err := rows.Scan(&id, &ver, &blob, &key, &gen); err != nil {
				t.Fatal(err)
			}
			b = append(b, id...)
			b = append(b, byte(ver), byte(gen))
			b = append(b, key...)
			b = append(b, blob...)
		}
		return b
	}

	if got, err := resolve("att_1", "erp-token", 1); err != nil || got != v1 {
		t.Fatalf("pinned run resolved %q, %v; want the pinned old value", got, err)
	}
	if got, err := resolve("att_2", "erp-token", 2); err != nil || got != v2 {
		t.Fatalf("run on the new version resolved %q, %v", got, err)
	}
	if _, err := resolve("att_3", "erp-token", 1); !errors.Is(err, secrets.ErrNotPinned) {
		t.Fatalf("a run without a pin resolved: %v", err)
	}
	// A declared version that does not exist never falls back to the current one.
	auth.set(secrets.AttemptGrant{Scope: scopeA1, RunID: "run_1", AttemptID: "att_1", Bindings: []runtimeBinding{binding("erp-token", 1), binding("erp-token", 99)}})
	if got, err := resolve("att_1", "erp-token", 99); !errors.Is(err, secrets.ErrNotFound) || got != "" {
		t.Fatalf("missing version resolved %q, %v", got, err)
	}
	if got, err := resolve("att_1", "erp-token", 1); err != nil || got != v1 {
		t.Fatalf("pinned run after another declaration: %q %v", got, err)
	}
	// A version the run declares but did not pin (the current one) is refused too.
	auth.set(secrets.AttemptGrant{Scope: scopeA1, RunID: "run_1", AttemptID: "att_1", Bindings: []runtimeBinding{binding("erp-token", 2)}})
	if got, err := resolve("att_1", "erp-token", 2); !errors.Is(err, secrets.ErrNotPinned) || got != "" {
		t.Fatalf("an unpinned current version resolved %q, %v", got, err)
	}
	auth.set(secrets.AttemptGrant{Scope: scopeA1, RunID: "run_1", AttemptID: "att_1", Bindings: []runtimeBinding{binding("erp-token", 1)}})

	// ---- rewrap under a new KEK, with the old one retained ----
	before := snapshot()
	e.keys.add("v2")
	e.wrapper.set(func(f *faultyWrapper) { f.inner = e.keys.provider("v2", "v1", "v2") })
	sk, err := e.store.ActiveScopeKey(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	if sk.Revision() != 1 || sk.Wrapped().KEK.Version != "v1" {
		t.Fatalf("scope key before rewrap: rev %d kek %s", sk.Revision(), sk.Wrapped().KEK.Version)
	}
	rw, err := e.svc.RewrapScopeKey(ctx, e.mutation("ops"), tenantA, "")
	if err != nil {
		t.Fatal(err)
	}
	if rw.Revision() != 2 || rw.Wrapped().KEK.Version != "v2" || rw.ID() != sk.ID() || rw.Generation() != sk.Generation() {
		t.Fatalf("rewrapped key: rev %d kek %s id %s gen %d", rw.Revision(), rw.Wrapped().KEK.Version, rw.ID(), rw.Generation())
	}
	if !bytes.Equal(before, snapshot()) {
		t.Fatal("rewrap changed value ciphertext, nonces, value versions or key identity")
	}
	if n := e.count(`SELECT count(*) FROM secret_scope_key_wraps`); n != 2 {
		t.Fatalf("wrap history has %d rows, want both revisions retained", n)
	}
	var kek1, kek2 string
	if err := e.base.Migration.QueryRow(ctx, `SELECT (SELECT kek_version FROM secret_scope_key_wraps WHERE revision = 1), (SELECT kek_version FROM secret_scope_key_wraps WHERE revision = 2)`).Scan(&kek1, &kek2); err != nil || kek1 != "v1" || kek2 != "v2" {
		t.Fatalf("retained wrapping references: %q %q %v", kek1, kek2, err)
	}
	// Pinned bytes are identical after rewrap; the version space is untouched.
	if got, err := resolve("att_1", "erp-token", 1); err != nil || got != v1 {
		t.Fatalf("after rewrap the pinned value is %q, %v", got, err)
	}
	if got, err := resolve("att_2", "erp-token", 2); err != nil || got != v2 {
		t.Fatalf("after rewrap the current value is %q, %v", got, err)
	}
	after, err := e.store.GetSecret(ctx, scopeA1, sec.ID())
	if err != nil || after.CurrentVersion() != 2 || after.Version() != 2 {
		t.Fatalf("rewrap touched the secret: %v %v", after.CurrentVersion(), err)
	}
	// The retired KEK is no longer needed for current wraps, but rotating the key file away
	// from the *current* configuration must fail closed, not fall back.
	e.wrapper.set(func(f *faultyWrapper) { f.inner = e.keys.provider("v1", "v1") })
	if _, err := resolve("att_1", "erp-token", 1); err == nil {
		t.Fatal("resolution succeeded without the KEK version that wraps the scope key")
	} else {
		var ke *secrets.KeyError
		if !errors.As(err, &ke) || ke.Class != secrets.KeyNotFound {
			t.Fatalf("missing KEK was not a classified key error: %v", err)
		}
	}
	e.wrapper.set(func(f *faultyWrapper) { f.inner = e.keys.provider("v2", "v1", "v2") })

	// ---- a new version after rewrap derives its key from the same scope key ----
	sec = e.rotate(scopeA1, sec.ID(), v3)
	if sec.CurrentVersion() != 3 {
		t.Fatalf("version after rewrap: %d", sec.CurrentVersion())
	}
	if err := e.pin(scopeA1, "run_4", "erp-token", 3); err != nil {
		t.Fatal(err)
	}
	auth.set(secrets.AttemptGrant{Scope: scopeA1, RunID: "run_4", AttemptID: "att_4", Bindings: []runtimeBinding{binding("erp-token", 3)}})
	if got, err := resolve("att_4", "erp-token", 3); err != nil || got != v3 {
		t.Fatalf("version 3: %q %v", got, err)
	}
	// Old pins still resolve their own bytes.
	if got, err := resolve("att_1", "erp-token", 1); err != nil || got != v1 {
		t.Fatalf("version 1 after two rotations: %q %v", got, err)
	}

	// Events and audit describe the operations without values.
	actions := e.auditActions()
	for _, want := range []string{"secret.create", "secret.rotate", "secret_scope_key.rewrap", "secret_scope_key.create", "secret.resolve"} {
		if !contains(actions, want) {
			t.Errorf("audit lacks %s: %v", want, actions)
		}
	}
	notContains(t, "persisted metadata", e.dumpAll(), v1, v2, v3, "SENTINEL")
	if e.outboxTypes() != 4 { // created, rotated, rewrap(updated), rotated
		t.Fatalf("configuration.changed events: %d", e.outboxTypes())
	}
}
