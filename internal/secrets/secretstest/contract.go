package secretstest

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/secrets"
)

// The scopes of the contract. Every implementation has these tenants and
// projects.
var (
	ScopeA  = domain.Scope{TenantID: "t088_acme", ProjectID: "p088_web"}
	ScopeA2 = domain.Scope{TenantID: "t088_acme", ProjectID: "p088_api"}
	ScopeB  = domain.Scope{TenantID: "t088_globex", ProjectID: "p088_ops"}
)

// Installation is the installation identity every Env binds into wraps and values.
const Installation = "inst-t088"

// Env is the secrets module under contract: the persistence port, the use cases
// over it, the resolution adapter, the pin seam the run admission joins and the
// observation hooks the contract needs.
type Env interface {
	Store() secrets.Store
	Variables() secrets.VariableCatalog
	Secrets() secrets.SecretCatalog
	// Wrapper is the healthy key wrapper values are sealed with by default.
	Wrapper() secrets.KeyWrapper
	// Service builds the use cases over this adapter's store with wrapper.
	Service(t testing.TB, wrapper secrets.KeyWrapper) *secrets.Service
	// Source builds the resolution adapter over this adapter's store with wrapper.
	Source(t testing.TB, wrapper secrets.KeyWrapper) secrets.VersionSource
	// RegisterPins and ReleasePins run the pin seam in a transaction of their
	// own, the stand-in for the run admission and termination transactions that
	// join it.
	RegisterPins(ctx context.Context, scope domain.Scope, consumer secrets.ConsumerRef, bindings []runtimev1.SecretBinding) error
	ReleasePins(ctx context.Context, scope domain.Scope, consumer secrets.ConsumerRef) (int64, error)
	HasPin(ctx context.Context, scope domain.Scope, consumer secrets.ConsumerRef, secretID string, version int64) (found, released bool, err error)
	// AuditActions lists the tenant's audit actions, sorted.
	AuditActions(t testing.TB, tenant domain.TenantID) []string
	// EventIDs lists the tenant's outbox event IDs.
	EventIDs(t testing.TB, tenant domain.TenantID) []string
	// WrapRevisions lists the retained wrap revisions of a scope key.
	WrapRevisions(t testing.TB, tenant domain.TenantID, keyID string) []int64
	// FailEvents makes every event append fail with err after the event was
	// written in the unit; nil restores it.
	FailEvents(err error)
}

// Open builds an empty Env.
type Open func(testing.TB) Env

// StoreContract is the behavior every secrets.Store adapter shares, exercised
// through the use cases: variables and secrets scoped to their project and
// tenant, reserved names, expected-version writes, immutable value versions
// that hold ciphertext only, one scope key per tenant generation that rewrap
// leaves in place, one-way revocation with the consumer handling it owes, pins
// that order against deletion and revocation, exact-version resolution that
// rechecks revocation and release before it commits its audit, and units that
// commit all of their writes, audit rows and events or none. It asserts
// outcomes, not lock order or isolation levels.
func StoreContract(t *testing.T, open Open) {
	t.Helper()
	ctx := context.Background()
	now := func() time.Time { return time.Now().UTC() }
	mutation := func(actor string) secrets.Mutation {
		return secrets.Mutation{Actor: secrets.Actor{Type: secrets.ActorUser, ID: actor}, RequestID: "req_t088", Now: now()}
	}

	type rig struct {
		env Env
		svc *secrets.Service
		st  secrets.Store
	}
	fresh := func(t *testing.T) *rig {
		t.Helper()
		env := open(t)
		return &rig{env: env, svc: env.Service(t, env.Wrapper()), st: env.Store()}
	}
	atomically := func(t *testing.T, r *rig, tenant domain.TenantID, fn func(w secrets.Writer) error) error {
		t.Helper()
		return r.svc.Atomically(ctx, tenant, func(ctx context.Context, w secrets.Writer) error { return fn(w) })
	}
	createVariable := func(t *testing.T, r *rig, scope domain.Scope, name string, v runtimev1.Value) secrets.Variable {
		t.Helper()
		var out secrets.Variable
		if err := atomically(t, r, scope.TenantID, func(w secrets.Writer) (err error) {
			out, err = r.svc.CreateVariable(ctx, w, mutation("alice"), secrets.CreateVariableInput{Scope: scope, Name: name, Value: v, Description: "d"})
			return err
		}); err != nil {
			t.Fatalf("create variable %s: %v", name, err)
		}
		return out
	}
	createSecret := func(t *testing.T, r *rig, scope domain.Scope, name, value string) (secrets.Secret, error) {
		t.Helper()
		m := mutation("alice")
		p, err := r.svc.PrepareSecret(ctx, m, scope, name, secrets.KindGeneric, []byte(value))
		if err != nil {
			return secrets.Secret{}, err
		}
		var out secrets.Secret
		err = atomically(t, r, scope.TenantID, func(w secrets.Writer) (err error) {
			out, err = r.svc.CreateSecret(ctx, w, m, p)
			return err
		})
		return out, err
	}
	mustSecret := func(t *testing.T, r *rig, scope domain.Scope, name, value string) secrets.Secret {
		t.Helper()
		s, err := createSecret(t, r, scope, name, value)
		if err != nil {
			t.Fatalf("create secret %s: %v", name, err)
		}
		return s
	}
	rotate := func(t *testing.T, r *rig, scope domain.Scope, id string, expected domain.Version, value string) (secrets.Secret, error) {
		t.Helper()
		m := mutation("alice")
		p, err := r.svc.PrepareVersion(ctx, m, scope, id, expected, []byte(value))
		if err != nil {
			return secrets.Secret{}, err
		}
		var out secrets.Secret
		err = atomically(t, r, scope.TenantID, func(w secrets.Writer) (err error) {
			out, err = r.svc.AddVersion(ctx, w, m, p)
			return err
		})
		return out, err
	}
	binding := func(name string, version int64) runtimev1.SecretBinding {
		return runtimev1.SecretBinding{Ref: secrets.FormatSecretRef(name), Version: version, Name: "tok", Delivery: runtimev1.SecretDelivery("env")}
	}
	run := func(id string) secrets.ConsumerRef { return secrets.ConsumerRef{Kind: secrets.ConsumerKindRun, ID: id} }
	finalize := func(t *testing.T, src secrets.VersionSource, scope domain.Scope, runID, name string, version int64) (string, error) {
		t.Helper()
		pending, err := src.Open(ctx, secrets.VersionRequest{Scope: scope, RunID: domain.RunID(runID), SecretName: name, Version: version})
		if err != nil {
			return "", err
		}
		defer pending.Discard()
		got := string(pending.Value())
		return got, pending.Finalize(ctx, secrets.FinalizeInfo{AttemptID: "att_t088", RunID: domain.RunID(runID), Delivery: runtimev1.SecretDelivery("env"), RequestID: "req_t088"})
	}

	t.Run("variables are scoped, typed, versioned and keep their names", func(t *testing.T) {
		r := fresh(t)
		a := createVariable(t, r, ScopeA, "region", runtimev1.StringValue("eu"))
		if a.Version() != 1 || a.ValueVersion() != 1 || a.Name() != "region" || a.Value() != runtimev1.StringValue("eu") {
			t.Fatalf("created: %+v", a.Record())
		}
		createVariable(t, r, ScopeA, "retries", runtimev1.IntValue(3))
		createVariable(t, r, ScopeA2, "region", runtimev1.StringValue("us")) // the same name in a sibling project
		createVariable(t, r, ScopeB, "region", runtimev1.StringValue("ap"))  // and in another tenant

		err := atomically(t, r, ScopeA.TenantID, func(w secrets.Writer) error {
			_, err := r.svc.CreateVariable(ctx, w, mutation("alice"), secrets.CreateVariableInput{Scope: ScopeA, Name: "region", Value: runtimev1.StringValue("dup")})
			return err
		})
		if !errors.Is(err, secrets.ErrNameTaken) {
			t.Fatalf("a duplicate name: %v", err)
		}
		list, err := r.st.ListVariables(ctx, ScopeA, "", 10)
		if err != nil || len(list) != 2 || list[0].Name() != "region" || list[1].Name() != "retries" {
			t.Fatalf("list: %v %v", list, err)
		}
		if page, err := r.st.ListVariables(ctx, ScopeA, "region", 10); err != nil || len(page) != 1 || page[0].Name() != "retries" {
			t.Fatalf("page: %v %v", page, err)
		}
		if one, err := r.st.ListVariables(ctx, ScopeA, "", 1); err != nil || len(one) != 1 {
			t.Fatalf("limit: %v %v", one, err)
		}
		if av, err := r.env.Variables().ActiveVariable(ctx, ScopeA, "region"); err != nil || av.ID() != a.ID() {
			t.Fatalf("catalog: %v %v", av, err)
		}
		for name, err := range map[string]error{
			"get from the sibling project": second(r.st.GetVariable(ctx, ScopeA2, a.ID())),
			"get from another tenant":      second(r.st.GetVariable(ctx, ScopeB, a.ID())),
			"get an unknown variable":      second(r.st.GetVariable(ctx, ScopeA, "var_unknown")),
			"catalog in the sibling":       second(r.env.Variables().ActiveVariable(ctx, ScopeA2, "retries")),
			"edit from another tenant": atomically(t, r, ScopeB.TenantID, func(w secrets.Writer) error {
				_, err := r.svc.EditVariable(ctx, w, mutation("alice"), ScopeA, a.ID(), 1, secrets.VariableEdit{Description: ptr("x")})
				return err
			}),
		} {
			if !errors.Is(err, secrets.ErrNotFound) {
				t.Errorf("%s: %v", name, err)
			}
		}

		edit := func(expected domain.Version, e secrets.VariableEdit) (secrets.Variable, error) {
			var out secrets.Variable
			err := atomically(t, r, ScopeA.TenantID, func(w secrets.Writer) (err error) {
				out, err = r.svc.EditVariable(ctx, w, mutation("alice"), ScopeA, a.ID(), expected, e)
				return err
			})
			return out, err
		}
		v2, err := edit(1, secrets.VariableEdit{Value: ptr(runtimev1.StringValue("eu-west"))})
		if err != nil || v2.Version() != 2 || v2.ValueVersion() != 2 || v2.Value() != runtimev1.StringValue("eu-west") {
			t.Fatalf("value edit: %+v %v", v2.Record(), err)
		}
		v3, err := edit(2, secrets.VariableEdit{Description: ptr("a note")})
		if err != nil || v3.Version() != 3 || v3.ValueVersion() != 2 || v3.Description() != "a note" {
			t.Fatalf("description edit keeps the value version: %+v %v", v3.Record(), err)
		}
		var conflict *secrets.ConflictError
		if _, err := edit(1, secrets.VariableEdit{Description: ptr("stale")}); !errors.As(err, &conflict) {
			t.Fatalf("a stale version: %v", err)
		}
		if _, err := edit(3, secrets.VariableEdit{Value: ptr(runtimev1.IntValue(1))}); !errors.Is(err, secrets.ErrInvalid) {
			t.Fatalf("a value of another type: %v", err)
		}
		if same, err := edit(3, secrets.VariableEdit{Description: ptr("a note")}); err != nil || same.Version() != 3 {
			t.Fatalf("an edit that changes nothing: %+v %v", same.Record(), err)
		}
		got, _ := r.st.GetVariable(ctx, ScopeA, a.ID())
		if got.Version() != 3 || got.ValueVersion() != 2 {
			t.Fatalf("stored: %+v", got.Record())
		}

		// Deletion is logical: the tombstone stays, the name stays taken.
		var deleted secrets.Variable
		if err := atomically(t, r, ScopeA.TenantID, func(w secrets.Writer) (err error) {
			deleted, err = r.svc.DeleteVariable(ctx, w, mutation("alice"), ScopeA, a.ID(), 3)
			return err
		}); err != nil || !deleted.Deleted() || deleted.Version() != 4 {
			t.Fatalf("delete: %+v %v", deleted.Record(), err)
		}
		if tomb, err := r.st.GetVariable(ctx, ScopeA, a.ID()); err != nil || !tomb.Deleted() {
			t.Fatalf("tombstone: %+v %v", tomb.Record(), err)
		}
		if list, _ := r.st.ListVariables(ctx, ScopeA, "", 10); len(list) != 1 || list[0].Name() != "retries" {
			t.Fatalf("a deleted variable is listed: %v", list)
		}
		if _, err := r.env.Variables().ActiveVariable(ctx, ScopeA, "region"); !errors.Is(err, secrets.ErrNotFound) {
			t.Fatalf("a deleted variable is active: %v", err)
		}
		if _, err := edit(4, secrets.VariableEdit{Description: ptr("late")}); !errors.Is(err, secrets.ErrDeleted) {
			t.Fatalf("editing a deleted variable: %v", err)
		}
		err = atomically(t, r, ScopeA.TenantID, func(w secrets.Writer) error {
			_, err := r.svc.CreateVariable(ctx, w, mutation("alice"), secrets.CreateVariableInput{Scope: ScopeA, Name: "region", Value: runtimev1.StringValue("reuse")})
			return err
		})
		if !errors.Is(err, secrets.ErrNameTaken) {
			t.Fatalf("reusing a deleted name: %v", err)
		}
	})

	t.Run("an identity collision never replaces a variable or a secret", func(t *testing.T) {
		r := fresh(t)
		v := createVariable(t, r, ScopeA, "region", runtimev1.StringValue("eu"))
		clash, err := secrets.NewVariable(ScopeA, v.ID(), "other", runtimev1.StringValue("clash"), "x", now())
		if err != nil {
			t.Fatal(err)
		}
		if err := atomically(t, r, ScopeA.TenantID, func(w secrets.Writer) error { return w.CreateVariable(ctx, mutation("alice"), clash) }); err == nil {
			t.Fatal("a colliding variable identity was created")
		}
		got, err := r.st.GetVariable(ctx, ScopeA, v.ID())
		if err != nil || got.Name() != "region" || got.Value() != runtimev1.StringValue("eu") || got.Version() != 1 {
			t.Fatalf("the stored variable changed: %+v %v", got.Record(), err)
		}
		if list, _ := r.st.ListVariables(ctx, ScopeA, "", 10); len(list) != 1 {
			t.Fatalf("variables: %v", list)
		}

		m := mutation("alice")
		p, err := r.svc.PrepareSecret(ctx, m, ScopeA, "api-key", secrets.KindGeneric, []byte("first"))
		if err != nil {
			t.Fatal(err)
		}
		if err := atomically(t, r, ScopeA.TenantID, func(w secrets.Writer) error { _, err := r.svc.CreateSecret(ctx, w, m, p); return err }); err != nil {
			t.Fatal(err)
		}
		other, err := secrets.NewSecret(ScopeA, p.Secret.ID(), "other-name", secrets.KindGeneric, now())
		if err != nil {
			t.Fatal(err)
		}
		if err := other.AcceptVersion(other.Version(), p.Version, now()); err != nil {
			t.Fatal(err)
		}
		if err := atomically(t, r, ScopeA.TenantID, func(w secrets.Writer) error { return w.CreateSecret(ctx, m, other, p.Version) }); err == nil {
			t.Fatal("a colliding secret identity was created")
		}
		sec, err := r.st.GetSecret(ctx, ScopeA, p.Secret.ID())
		if err != nil || sec.Name() != "api-key" || sec.Version() != 1 || sec.CurrentVersion() != 1 {
			t.Fatalf("the stored secret changed: %+v %v", sec.Record(), err)
		}
		if vs, err := r.st.ListSecretVersions(ctx, ScopeA, p.Secret.ID(), 0, 10); err != nil || len(vs) != 1 {
			t.Fatalf("versions: %v %v", vs, err)
		}
		if _, err := r.env.Secrets().CurrentVersion(ctx, ScopeA, "other-name"); !errors.Is(err, secrets.ErrNotFound) {
			t.Fatalf("the colliding name resolves: %v", err)
		}
		if n := len(r.env.EventIDs(t, ScopeA.TenantID)); n != 2 { // the variable and the secret
			t.Fatalf("events: %d", n)
		}
	})

	t.Run("a secret holds ciphertext, one value version at a time", func(t *testing.T) {
		r := fresh(t)
		s := mustSecret(t, r, ScopeA, "api-key", "first-value")
		if s.Version() != 1 || s.CurrentVersion() != 1 || s.Name() != "api-key" || s.Kind() != secrets.KindGeneric || s.Ref() != "secret://project/api-key" {
			t.Fatalf("created: %+v", s.Record())
		}
		v1, err := r.st.GetSecretVersion(ctx, ScopeA, s.ID(), 1)
		if err != nil || v1.Revoked() || len(v1.Envelope().Ciphertext) == 0 || string(v1.Envelope().Ciphertext) == "first-value" {
			t.Fatalf("version 1: %+v %v", v1.Record(), err)
		}
		if _, err := createSecret(t, r, ScopeA, "api-key", "again"); !errors.Is(err, secrets.ErrNameTaken) {
			t.Fatalf("a duplicate name: %v", err)
		}
		mustSecret(t, r, ScopeA2, "api-key", "sibling")
		mustSecret(t, r, ScopeB, "api-key", "foreign")
		mustSecret(t, r, ScopeA, "db-password", "other")

		list, err := r.st.ListSecrets(ctx, ScopeA, "", 10)
		if err != nil || len(list) != 2 || list[0].Name() != "api-key" || list[1].Name() != "db-password" {
			t.Fatalf("list: %v %v", list, err)
		}
		if page, err := r.st.ListSecrets(ctx, ScopeA, "api-key", 10); err != nil || len(page) != 1 || page[0].Name() != "db-password" {
			t.Fatalf("page: %v %v", page, err)
		}
		for name, err := range map[string]error{
			"get from the sibling project": second(r.st.GetSecret(ctx, ScopeA2, s.ID())),
			"get from another tenant":      second(r.st.GetSecret(ctx, ScopeB, s.ID())),
			"version from the sibling":     second(r.st.GetSecretVersion(ctx, ScopeA2, s.ID(), 1)),
			"version from another tenant":  second(r.st.GetSecretVersion(ctx, ScopeB, s.ID(), 1)),
			"an unknown version":           second(r.st.GetSecretVersion(ctx, ScopeA, s.ID(), 9)),
			"retention of a foreign":       second(r.st.RetentionReport(ctx, ScopeB, s.ID())),
			"versions from the sibling": func() error {
				v, err := r.st.ListSecretVersions(ctx, ScopeA2, s.ID(), 0, 10)
				if err == nil && len(v) != 0 {
					t.Errorf("versions of another project's secret: %v", v)
				}
				return secrets.ErrNotFound
			}(),
		} {
			if !errors.Is(err, secrets.ErrNotFound) {
				t.Errorf("%s: %v", name, err)
			}
		}

		var conflict *secrets.ConflictError
		if _, err := rotate(t, r, ScopeA, s.ID(), 7, "stale"); !errors.As(err, &conflict) {
			t.Fatalf("a stale resource version: %v", err)
		}
		s2, err := rotate(t, r, ScopeA, s.ID(), 1, "second-value")
		if err != nil || s2.Version() != 2 || s2.CurrentVersion() != 2 {
			t.Fatalf("rotate: %+v %v", s2.Record(), err)
		}
		// A value sealed against an old resource version cannot be added.
		m := mutation("alice")
		stale, err := r.svc.PrepareVersion(ctx, m, ScopeA, s.ID(), 2, []byte("third"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rotate(t, r, ScopeA, s.ID(), 2, "winner"); err != nil {
			t.Fatal(err)
		}
		err = atomically(t, r, ScopeA.TenantID, func(w secrets.Writer) error { _, err := r.svc.AddVersion(ctx, w, m, stale); return err })
		if !errors.As(err, &conflict) {
			t.Fatalf("a prepared version after a concurrent rotation: %v", err)
		}
		versions, err := r.st.ListSecretVersions(ctx, ScopeA, s.ID(), 0, 10)
		if err != nil || len(versions) != 3 || versions[0].Version() != 1 || versions[1].Version() != 2 || versions[2].Version() != 3 {
			t.Fatalf("versions: %v %v", versions, err)
		}
		if after, err := r.st.ListSecretVersions(ctx, ScopeA, s.ID(), 2, 10); err != nil || len(after) != 1 || after[0].Version() != 3 {
			t.Fatalf("versions after 2: %v %v", after, err)
		}
		if cur, err := r.env.Secrets().CurrentVersion(ctx, ScopeA, "api-key"); err != nil || cur != 3 {
			t.Fatalf("catalog: %d %v", cur, err)
		}
		if _, err := r.env.Secrets().CurrentVersion(ctx, ScopeA2, "db-password"); !errors.Is(err, secrets.ErrNotFound) {
			t.Fatalf("catalog in the sibling project: %v", err)
		}
		if _, err := r.svc.PrepareSecret(ctx, mutation("alice"), ScopeA, "empty", secrets.KindGeneric, nil); !errors.Is(err, secrets.ErrInvalid) {
			t.Fatalf("an empty value: %v", err)
		}
		if _, err := r.svc.PrepareSecret(ctx, mutation("alice"), ScopeA, "Bad Name", secrets.KindGeneric, []byte("x")); !errors.Is(err, secrets.ErrInvalid) {
			t.Fatalf("a bad name: %v", err)
		}
	})

	t.Run("one scope key per tenant generation, rewrapped in place", func(t *testing.T) {
		r := fresh(t)
		if _, err := r.st.ActiveScopeKey(ctx, ScopeA.TenantID); !errors.Is(err, secrets.ErrNotFound) {
			t.Fatalf("a tenant with no key: %v", err)
		}
		s := mustSecret(t, r, ScopeA, "api-key", "value-one")
		other := mustSecret(t, r, ScopeA2, "api-key", "value-two") // the same tenant reuses its key
		sk, err := r.st.ActiveScopeKey(ctx, ScopeA.TenantID)
		if err != nil || sk.Generation() != 1 || sk.Revision() != 1 || sk.TenantID() != ScopeA.TenantID {
			t.Fatalf("active key: %+v %v", sk.Record(), err)
		}
		v1, _ := r.st.GetSecretVersion(ctx, ScopeA, s.ID(), 1)
		v2, _ := r.st.GetSecretVersion(ctx, ScopeA2, other.ID(), 1)
		if v1.Record().ScopeKeyID != sk.ID() || v2.Record().ScopeKeyID != sk.ID() {
			t.Fatalf("values of one tenant use different keys: %s %s vs %s", v1.Record().ScopeKeyID, v2.Record().ScopeKeyID, sk.ID())
		}
		skB := func() secrets.ScopeKey {
			mustSecret(t, r, ScopeB, "api-key", "foreign")
			k, err := r.st.ActiveScopeKey(ctx, ScopeB.TenantID)
			if err != nil {
				t.Fatal(err)
			}
			return k
		}()
		if skB.ID() == sk.ID() {
			t.Fatal("two tenants share a scope key")
		}
		if _, err := r.st.GetScopeKey(ctx, ScopeB.TenantID, sk.ID()); !errors.Is(err, secrets.ErrNotFound) {
			t.Fatalf("another tenant's key: %v", err)
		}

		// The creator that loses the generation is told so and gets the committed key.
		wc := secrets.WrapContext{InstallationID: Installation, TenantID: ScopeA.TenantID, Generation: 1, KeyID: "sk_loser"}
		wrapped, err := r.env.Wrapper().Wrap(ctx, wc, make([]byte, 32))
		if err != nil {
			t.Fatal(err)
		}
		loser, err := secrets.NewScopeKey(ScopeA.TenantID, "sk_loser", 1, wrapped, now())
		if err != nil {
			t.Fatal(err)
		}
		committed, created, err := r.st.EnsureScopeKey(ctx, mutation("alice"), loser)
		if err != nil || created || committed.ID() != sk.ID() {
			t.Fatalf("a second creator of generation 1: %+v created %v %v", committed.Record(), created, err)
		}
		if _, err := r.st.GetScopeKey(ctx, ScopeA.TenantID, "sk_loser"); !errors.Is(err, secrets.ErrNotFound) {
			t.Fatalf("the losing candidate was stored: %v", err)
		}

		// Rewrap changes the wrap, not the key or the values.
		wrapper := r.env.Wrapper()
		if rot, ok := wrapper.(interface{ Rotate() (string, error) }); ok {
			if _, err := rot.Rotate(); err != nil {
				t.Fatal(err)
			}
		}
		rewrapped, err := r.svc.RewrapScopeKey(ctx, mutation("ops"), ScopeA.TenantID, "")
		if err != nil || rewrapped.ID() != sk.ID() || rewrapped.Revision() != 2 || rewrapped.Generation() != 1 {
			t.Fatalf("rewrap: %+v %v", rewrapped.Record(), err)
		}
		if revs := r.env.WrapRevisions(t, ScopeA.TenantID, sk.ID()); !slices.Equal(revs, []int64{1, 2}) {
			t.Fatalf("retained wraps: %v", revs)
		}
		after, _ := r.st.ActiveScopeKey(ctx, ScopeA.TenantID)
		if after.Revision() != 2 || string(after.Wrapped().Bytes) == string(sk.Wrapped().Bytes) {
			t.Fatalf("the active wrap did not change: %+v", after.Record())
		}
		if v, _ := r.st.GetSecretVersion(ctx, ScopeA, s.ID(), 1); string(v.Envelope().Ciphertext) != string(v1.Envelope().Ciphertext) {
			t.Fatal("rewrap touched value ciphertext")
		}
		// A rewrap decided on an older revision loses.
		err = atomically(t, r, ScopeA.TenantID, func(w secrets.Writer) error {
			_, err := w.RewrapScopeKey(ctx, mutation("ops"), ScopeA.TenantID, sk.ID(), 1, after.Wrapped())
			return err
		})
		var conflict *secrets.ConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("a stale wrap revision: %v", err)
		}
	})

	t.Run("revocation is one way and records what consumers owe", func(t *testing.T) {
		r := fresh(t)
		s := mustSecret(t, r, ScopeA, "api-key", "value-one")
		if _, err := rotate(t, r, ScopeA, s.ID(), 1, "value-two"); err != nil {
			t.Fatal(err)
		}
		b1 := []runtimev1.SecretBinding{binding("api-key", 1)}
		// Two runs pin version 1; run_two has already resolved it.
		for _, id := range []string{"run_one", "run_two"} {
			if err := r.env.RegisterPins(ctx, ScopeA, run(id), b1); err != nil {
				t.Fatalf("pin %s: %v", id, err)
			}
		}
		src := r.env.Source(t, r.env.Wrapper())
		if got, err := finalize(t, src, ScopeA, "run_two", "api-key", 1); err != nil || got != "value-one" {
			t.Fatalf("resolution: %q %v", got, err)
		}

		var res secrets.RevocationResult
		revoke := func() error {
			return atomically(t, r, ScopeA.TenantID, func(w secrets.Writer) (err error) {
				res, err = r.svc.RevokeVersion(ctx, w, mutation("alice"), ScopeA, s.ID(), 1)
				return err
			})
		}
		if err := revoke(); err != nil {
			t.Fatal(err)
		}
		if !res.Changed || !res.Version.Revoked() || res.Secret.Version() != 3 || len(res.Handling) != 4 || res.PendingHandlingCount() != 4 {
			t.Fatalf("revocation: changed %v, secret v%d, %d effects", res.Changed, res.Secret.Version(), len(res.Handling))
		}
		delivered := 0
		for _, h := range res.Handling {
			if h.Record().Delivered {
				delivered++
			}
		}
		if delivered != 2 { // both effects of run_two, which had the value
			t.Fatalf("delivered effects: %d", delivered)
		}
		first := res.Version.Record().RevokedAt
		// Revoking again changes nothing and keeps the first revocation.
		if err := revoke(); err != nil || res.Changed || res.Secret.Version() != 3 || len(res.Handling) != 4 {
			t.Fatalf("a repeated revocation: changed %v, v%d, %d effects, %v", res.Changed, res.Secret.Version(), len(res.Handling), err)
		}
		if got, _ := r.st.GetSecretVersion(ctx, ScopeA, s.ID(), 1); !got.Revoked() || !got.Record().RevokedAt.Equal(*first) || got.Record().RevokedBy != "alice" {
			t.Fatalf("stored revocation: %+v", got.Record())
		}
		if got, _ := r.st.GetSecretVersion(ctx, ScopeA, s.ID(), 2); got.Revoked() {
			t.Fatal("revoking version 1 revoked version 2")
		}
		// Nothing resolves a revoked version, and no new pin may name it.
		if _, err := finalize(t, src, ScopeA, "run_one", "api-key", 1); !errors.Is(err, secrets.ErrRevoked) {
			t.Fatalf("resolving a revoked version: %v", err)
		}
		if err := r.env.RegisterPins(ctx, ScopeA, run("run_late"), b1); !errors.Is(err, secrets.ErrRevoked) {
			t.Fatalf("pinning a revoked version: %v", err)
		}
		// The effects are tracked until each owner acknowledges them, exactly once.
		pending, err := r.st.PendingHandling(ctx, ScopeA, "", 10)
		if err != nil || len(pending) != 4 {
			t.Fatalf("pending: %v %v", pending, err)
		}
		if !slices.IsSortedFunc(pending, func(a, b secrets.ConsumerHandling) int { return compare(a.ID(), b.ID()) }) {
			t.Fatalf("pending handling is not ordered by ID: %v", pending)
		}
		if page, err := r.st.PendingHandling(ctx, ScopeA, pending[1].ID(), 10); err != nil || len(page) != 2 {
			t.Fatalf("pending after the second: %v %v", page, err)
		}
		if foreign, err := r.st.PendingHandling(ctx, ScopeA2, "", 10); err != nil || len(foreign) != 0 {
			t.Fatalf("the sibling project sees effects: %v %v", foreign, err)
		}
		ack := func(id string, outcome secrets.HandlingState) (secrets.ConsumerHandling, error) {
			var h secrets.ConsumerHandling
			err := atomically(t, r, ScopeA.TenantID, func(w secrets.Writer) (err error) {
				h, err = w.AcknowledgeHandling(ctx, mutation("ops"), ScopeA, id, outcome)
				return err
			})
			return h, err
		}
		h, err := ack(pending[0].ID(), secrets.HandlingCompleted)
		if err != nil || h.State() != secrets.HandlingCompleted {
			t.Fatalf("ack: %v %v", h, err)
		}
		if again, err := ack(pending[0].ID(), secrets.HandlingCompleted); err != nil || again.State() != secrets.HandlingCompleted {
			t.Fatalf("the same acknowledgment again: %v %v", again, err)
		}
		if _, err := ack(pending[0].ID(), secrets.HandlingNotApplicable); !errors.Is(err, secrets.ErrInvalidTransition) {
			t.Fatalf("rewriting an acknowledgment: %v", err)
		}
		if _, err := ack("ch_unknown", secrets.HandlingCompleted); !errors.Is(err, secrets.ErrNotFound) {
			t.Fatalf("an unknown effect: %v", err)
		}
		if rest, _ := r.st.PendingHandling(ctx, ScopeA, "", 10); len(rest) != 3 {
			t.Fatalf("pending after one acknowledgment: %d", len(rest))
		}
		for _, h := range pending[1:] {
			if _, err := ack(h.ID(), secrets.HandlingNotApplicable); err != nil {
				t.Fatal(err)
			}
		}
		if rest, _ := r.st.PendingHandling(ctx, ScopeA, "", 10); len(rest) != 0 {
			t.Fatalf("pending after every acknowledgment: %d", len(rest))
		}
		if cur, err := r.env.Secrets().CurrentVersion(ctx, ScopeA, "api-key"); err != nil || cur != 2 {
			t.Fatalf("current version after revoking an old one: %d %v", cur, err)
		}
		// Revoking the current version leaves the secret with nothing to pin.
		if err := atomically(t, r, ScopeA.TenantID, func(w secrets.Writer) (err error) {
			_, err = r.svc.RevokeVersion(ctx, w, mutation("alice"), ScopeA, s.ID(), 2)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := r.env.Secrets().CurrentVersion(ctx, ScopeA, "api-key"); !errors.Is(err, secrets.ErrNoActiveVersion) {
			t.Fatalf("a secret whose current version is revoked: %v", err)
		}
		if err := atomically(t, r, ScopeB.TenantID, func(w secrets.Writer) error {
			_, err := r.svc.RevokeVersion(ctx, w, mutation("alice"), ScopeA, s.ID(), 1)
			return err
		}); !errors.Is(err, secrets.ErrNotFound) {
			t.Fatalf("revoking from another tenant: %v", err)
		}
	})

	t.Run("pins order against deletion, and deletion reports what outlives it", func(t *testing.T) {
		r := fresh(t)
		s := mustSecret(t, r, ScopeA, "api-key", "value")
		b := []runtimev1.SecretBinding{binding("api-key", 1)}
		for name, bs := range map[string][]runtimev1.SecretBinding{
			"an unknown secret":  {binding("no-such", 1)},
			"an unknown version": {binding("api-key", 9)},
		} {
			if err := r.env.RegisterPins(ctx, ScopeA, run("run_x"), bs); !errors.Is(err, secrets.ErrNotFound) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if err := r.env.RegisterPins(ctx, ScopeA2, run("run_x"), b); !errors.Is(err, secrets.ErrNotFound) {
			t.Fatalf("pinning another project's secret: %v", err)
		}
		if err := r.env.RegisterPins(ctx, ScopeB, run("run_x"), b); !errors.Is(err, secrets.ErrNotFound) {
			t.Fatalf("pinning another tenant's secret: %v", err)
		}
		if found, _, _ := r.env.HasPin(ctx, ScopeA, run("run_x"), s.ID(), 1); found {
			t.Fatal("a refused pin registration left a pin")
		}
		for i := 0; i < 2; i++ { // registering twice is a no-op
			if err := r.env.RegisterPins(ctx, ScopeA, run("run_a"), b); err != nil {
				t.Fatalf("pin: %v", err)
			}
		}
		if err := r.env.RegisterPins(ctx, ScopeA, run("run_b"), b); err != nil {
			t.Fatal(err)
		}
		if found, released, err := r.env.HasPin(ctx, ScopeA, run("run_a"), s.ID(), 1); err != nil || !found || released {
			t.Fatalf("pin: %v %v %v", found, released, err)
		}
		if n, err := r.env.ReleasePins(ctx, ScopeA, run("run_b")); err != nil || n != 1 {
			t.Fatalf("release: %d %v", n, err)
		}
		if n, err := r.env.ReleasePins(ctx, ScopeA, run("run_b")); err != nil || n != 0 {
			t.Fatalf("releasing twice: %d %v", n, err)
		}
		if found, released, _ := r.env.HasPin(ctx, ScopeA, run("run_b"), s.ID(), 1); !found || !released {
			t.Fatalf("released pin: %v %v", found, released)
		}
		var report secrets.RetentionReport
		var deleted secrets.Secret
		del := func(expected domain.Version) error {
			return atomically(t, r, ScopeA.TenantID, func(w secrets.Writer) (err error) {
				deleted, report, err = r.svc.DeleteSecret(ctx, w, mutation("alice"), ScopeA, s.ID(), expected)
				return err
			})
		}
		var conflict *secrets.ConflictError
		if err := del(5); !errors.As(err, &conflict) {
			t.Fatalf("a stale delete: %v", err)
		}
		if err := del(1); err != nil || !deleted.Deleted() || deleted.Version() != 2 {
			t.Fatalf("delete: %+v %v", deleted.Record(), err)
		}
		if report.PinnedVersions != 2 || report.ActivePins != 1 || report.PendingHandling != 0 || report.BackupRetention != secrets.BackupRetentionUnresolved {
			t.Fatalf("retention: %+v", report)
		}
		// Deleting again reports the same accounting and changes nothing.
		if err := del(2); err != nil || deleted.Version() != 2 || report.PinnedVersions != 2 {
			t.Fatalf("a repeated delete: %+v %+v %v", deleted.Record(), report, err)
		}
		if got, err := r.st.RetentionReport(ctx, ScopeA, s.ID()); err != nil || got != report {
			t.Fatalf("retention read: %+v %v", got, err)
		}
		if err := r.env.RegisterPins(ctx, ScopeA, run("run_c"), b); !errors.Is(err, secrets.ErrDeleted) {
			t.Fatalf("pinning a deleted secret: %v", err)
		}
		if list, _ := r.st.ListSecrets(ctx, ScopeA, "", 10); len(list) != 0 {
			t.Fatalf("a deleted secret is listed: %v", list)
		}
		if tomb, err := r.st.GetSecret(ctx, ScopeA, s.ID()); err != nil || !tomb.Deleted() {
			t.Fatalf("tombstone: %+v %v", tomb.Record(), err)
		}
		if _, err := createSecret(t, r, ScopeA, "api-key", "reuse"); !errors.Is(err, secrets.ErrNameTaken) {
			t.Fatalf("reusing a deleted name: %v", err)
		}
		if _, err := rotate(t, r, ScopeA, s.ID(), 2, "late"); !errors.Is(err, secrets.ErrDeleted) {
			t.Fatalf("rotating a deleted secret: %v", err)
		}
		// The pinned version stays readable to a run that still holds its pin, and
		// to nobody else.
		src := r.env.Source(t, r.env.Wrapper())
		if got, err := finalize(t, src, ScopeA, "run_a", "api-key", 1); err != nil || got != "value" {
			t.Fatalf("resolution after deletion by a run that kept its pin: %q %v", got, err)
		}
		if _, err := finalize(t, src, ScopeA, "run_b", "api-key", 1); !errors.Is(err, secrets.ErrAttemptInactive) {
			t.Fatalf("resolution after the pin was released: %v", err)
		}
	})

	t.Run("resolution opens exactly the pinned version and audits before it releases", func(t *testing.T) {
		r := fresh(t)
		s := mustSecret(t, r, ScopeA, "api-key", "value-one")
		if _, err := rotate(t, r, ScopeA, s.ID(), 1, "value-two"); err != nil {
			t.Fatal(err)
		}
		if err := r.env.RegisterPins(ctx, ScopeA, run("run_one"), []runtimev1.SecretBinding{binding("api-key", 1)}); err != nil {
			t.Fatal(err)
		}
		src := r.env.Source(t, r.env.Wrapper())
		audit0 := len(r.env.AuditActions(t, ScopeA.TenantID))
		if got, err := finalize(t, src, ScopeA, "run_one", "api-key", 1); err != nil || got != "value-one" {
			t.Fatalf("the pinned version: %q %v", got, err)
		}
		if n := len(r.env.AuditActions(t, ScopeA.TenantID)); n != audit0+1 {
			t.Fatalf("a resolution wrote %d audit rows", n-audit0)
		}
		if _, err := finalize(t, src, ScopeA, "run_one", "api-key", 2); !errors.Is(err, secrets.ErrNotPinned) {
			t.Fatalf("a newer version that was never pinned: %v", err)
		}
		if _, err := finalize(t, src, ScopeA, "run_other", "api-key", 1); !errors.Is(err, secrets.ErrNotPinned) {
			t.Fatalf("a run that never pinned: %v", err)
		}
		for name, err := range map[string]error{
			"an unknown secret":  second(finalize(t, src, ScopeA, "run_one", "no-such", 1)),
			"another project":    second(finalize(t, src, ScopeA2, "run_one", "api-key", 1)),
			"another tenant":     second(finalize(t, src, ScopeB, "run_one", "api-key", 1)),
			"an unknown version": second(finalize(t, src, ScopeA, "run_one", "api-key", 9)),
		} {
			if !errors.Is(err, secrets.ErrNotFound) {
				t.Errorf("%s: %v", name, err)
			}
		}
		// A key-provider outage releases nothing, and a recovered provider does.
		down := &flakyWrapper{inner: r.env.Wrapper()}
		flaky := r.env.Source(t, down)
		down.setUnwrap(&secrets.KeyError{Class: secrets.KeyUnavailable, Op: "unwrap", Err: errors.New("kms down")})
		pending, err := flaky.Open(ctx, secrets.VersionRequest{Scope: ScopeA, RunID: "run_one", SecretName: "api-key", Version: 1})
		var ke *secrets.KeyError
		if err == nil || pending != nil || !errors.As(err, &ke) || !ke.Retryable() {
			t.Fatalf("a provider outage: %v", err)
		}
		down.setUnwrap(nil)
		if got, err := finalize(t, flaky, ScopeA, "run_one", "api-key", 1); err != nil || got != "value-one" {
			t.Fatalf("after the provider recovered: %q %v", got, err)
		}
		// A revocation that lands between decryption and finalization wins.
		pending, err = src.Open(ctx, secrets.VersionRequest{Scope: ScopeA, RunID: "run_one", SecretName: "api-key", Version: 1})
		if err != nil {
			t.Fatal(err)
		}
		if err := atomically(t, r, ScopeA.TenantID, func(w secrets.Writer) (err error) {
			_, err = r.svc.RevokeVersion(ctx, w, mutation("alice"), ScopeA, s.ID(), 1)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		audit1 := len(r.env.AuditActions(t, ScopeA.TenantID))
		err = pending.Finalize(ctx, secrets.FinalizeInfo{AttemptID: "att_t088", RunID: "run_one", Delivery: runtimev1.SecretDelivery("env"), RequestID: "req_t088"})
		pending.Discard()
		if !errors.Is(err, secrets.ErrRevoked) {
			t.Fatalf("finalizing after a revocation: %v", err)
		}
		if n := len(r.env.AuditActions(t, ScopeA.TenantID)); n != audit1 {
			t.Fatalf("a refused finalization wrote audit: %d -> %d", audit1, n)
		}
		// A pin released between decryption and finalization wins too.
		if err := r.env.RegisterPins(ctx, ScopeA, run("run_two"), []runtimev1.SecretBinding{binding("api-key", 2)}); err != nil {
			t.Fatal(err)
		}
		pending, err = src.Open(ctx, secrets.VersionRequest{Scope: ScopeA, RunID: "run_two", SecretName: "api-key", Version: 2})
		if err != nil {
			t.Fatal(err)
		}
		if n, err := r.env.ReleasePins(ctx, ScopeA, run("run_two")); err != nil || n != 1 {
			t.Fatalf("release: %d %v", n, err)
		}
		err = pending.Finalize(ctx, secrets.FinalizeInfo{AttemptID: "att_t088", RunID: "run_two", Delivery: runtimev1.SecretDelivery("env"), RequestID: "req_t088"})
		pending.Discard()
		if !errors.Is(err, secrets.ErrAttemptInactive) {
			t.Fatalf("finalizing after the pins were released: %v", err)
		}
		if err := pending.Finalize(ctx, secrets.FinalizeInfo{AttemptID: "att_t088", RunID: "run_another", Delivery: runtimev1.SecretDelivery("env")}); !errors.Is(err, secrets.ErrInvalid) {
			t.Fatalf("finalizing for another run: %v", err)
		}
	})

	t.Run("a unit commits all of its writes, audit rows and events or none", func(t *testing.T) {
		r := fresh(t)
		a := createVariable(t, r, ScopeA, "region", runtimev1.StringValue("eu"))
		events0, audit0 := len(r.env.EventIDs(t, ScopeA.TenantID)), len(r.env.AuditActions(t, ScopeA.TenantID))
		if events0 != 1 || audit0 != 1 {
			t.Fatalf("a created variable wrote %d events and %d audit rows", events0, audit0)
		}

		// A unit that fails after several writes leaves none of them.
		boom := errors.New("the unit failed after its writes")
		err := atomically(t, r, ScopeA.TenantID, func(w secrets.Writer) error {
			if _, err := r.svc.CreateVariable(ctx, w, mutation("alice"), secrets.CreateVariableInput{Scope: ScopeA, Name: "second", Value: runtimev1.BoolValue(true)}); err != nil {
				return err
			}
			if _, err := r.svc.EditVariable(ctx, w, mutation("alice"), ScopeA, a.ID(), 1, secrets.VariableEdit{Description: ptr("changed")}); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("unit: %v", err)
		}
		if list, _ := r.st.ListVariables(ctx, ScopeA, "", 10); len(list) != 1 || list[0].Version() != 1 || list[0].Description() != "d" {
			t.Fatalf("a failed unit left writes behind: %v", list)
		}
		if len(r.env.EventIDs(t, ScopeA.TenantID)) != events0 || len(r.env.AuditActions(t, ScopeA.TenantID)) != audit0 {
			t.Fatal("a failed unit left events or audit rows behind")
		}

		// A failing event append undoes the secret, its first version, its scope key and its audit.
		r.env.FailEvents(errors.New("event bus unavailable"))
		if _, err := createSecret(t, r, ScopeB, "api-key", "value"); err == nil {
			t.Fatal("a secret was created although its event could not be appended")
		}
		r.env.FailEvents(nil)
		if list, _ := r.st.ListSecrets(ctx, ScopeB, "", 10); len(list) != 0 {
			t.Fatalf("a failed unit left a secret: %v", list)
		}
		if got := r.env.EventIDs(t, ScopeB.TenantID); len(got) != 0 {
			t.Fatalf("a failed unit left events: %v", got)
		}
		if _, err := createSecret(t, r, ScopeB, "api-key", "value"); err != nil {
			t.Fatalf("the name stayed taken after a failed unit: %v", err)
		}
		// Nothing was replayed: the one successful create wrote one event.
		if got := len(r.env.EventIDs(t, ScopeB.TenantID)); got != 1 {
			t.Fatalf("events of the retried create: %d", got)
		}
		// A key-provider outage fails before any write.
		down := &flakyWrapper{inner: r.env.Wrapper()}
		svc := r.env.Service(t, down)
		down.setWrap(&secrets.KeyError{Class: secrets.KeyUnavailable, Op: "wrap", Err: errors.New("kms down")})
		if _, err := svc.PrepareSecret(ctx, mutation("alice"), ScopeA2, "api-key", secrets.KindGeneric, []byte("v")); err == nil {
			t.Fatal("a secret was sealed while the key provider was down")
		}
		if _, err := r.st.ActiveScopeKey(ctx, ScopeA2.TenantID); !errors.Is(err, secrets.ErrNotFound) {
			t.Fatalf("a failed wrap left a scope key: %v", err)
		}
		if got := r.env.AuditActions(t, ScopeA.TenantID); !slices.Contains(got, "variable.create") {
			t.Fatalf("audit: %v", got)
		}
	})
}

func ptr[T any](v T) *T { return &v }

func second[T any](_ T, err error) error { return err }

func compare(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
