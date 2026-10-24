package secrets_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/internal/secrets/envelope"
)

var (
	scopeA = domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	t0     = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
)

func mustVariable(t *testing.T, name string, v runtimev1.Value) secrets.Variable {
	t.Helper()
	x, err := secrets.NewVariable(scopeA, "var_1", name, v, "", t0)
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func mustSecret(t *testing.T) secrets.Secret {
	t.Helper()
	s, err := secrets.NewSecret(scopeA, "sec_1", "erp-token", secrets.KindGeneric, t0)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func version(t *testing.T, s secrets.Secret, n int64) secrets.SecretVersion {
	t.Helper()
	v, err := secrets.NewSecretVersion(s, n, "sk_1", 1, envelope.Envelope{FormatVersion: 1, Algorithm: envelope.AlgorithmAES256GCM}, "alice", t0)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func conflict(err error) *secrets.ConflictError {
	var c *secrets.ConflictError
	if errors.As(err, &c) {
		return c
	}
	return nil
}

// Test_domain_transitions covers accepted/rejected updates, immutable identity,
// monotonic versions, irreversible revocation and logical deletion (VS-01,
// VS-07, VS-08, VS-12).
func Test_domain_transitions(t *testing.T) {
	t.Run("variable validation and immutability", func(t *testing.T) {
		for _, name := range []string{"", "Upper", "1abc", "a b", strings.Repeat("a", 64)} {
			if _, err := secrets.NewVariable(scopeA, "var_1", name, runtimev1.StringValue("x"), "", t0); !errors.Is(err, secrets.ErrInvalid) {
				t.Errorf("name %q accepted: %v", name, err)
			}
		}
		if _, err := secrets.NewVariable(scopeA, "var_1", "ok", runtimev1.Value{}, "", t0); err == nil {
			t.Error("empty value accepted")
		}
		if _, err := secrets.NewVariable(domain.Scope{TenantID: "t_acme"}, "var_1", "ok", runtimev1.StringValue("x"), "", t0); err == nil {
			t.Error("scope without a project accepted")
		}
		long := strings.Repeat("x", 4097)
		if _, err := secrets.NewVariable(scopeA, "var_1", "ok", runtimev1.StringValue(long), "", t0); err == nil {
			t.Error("over-long value accepted")
		}

		v := mustVariable(t, "region", runtimev1.StringValue("eu"))
		if v.ValueVersion() != 1 || v.Version() != 1 || v.Type() != secrets.TypeString {
			t.Fatalf("initial state %d/%d/%s", v.ValueVersion(), v.Version(), v.Type())
		}
		// Type is immutable.
		if _, _, err := v.Edit(1, secrets.VariableEdit{Value: ptr(runtimev1.IntValue(3))}, t0); !errors.Is(err, secrets.ErrInvalid) {
			t.Fatalf("type change accepted: %v", err)
		}
		// Stale versions are conflicts.
		if _, _, err := v.Edit(7, secrets.VariableEdit{Value: ptr(runtimev1.StringValue("us"))}, t0); conflict(err) == nil {
			t.Fatalf("stale edit: %v", err)
		}
		// Empty edits are rejected; identical values change nothing.
		if _, _, err := v.Edit(1, secrets.VariableEdit{}, t0); !errors.Is(err, secrets.ErrInvalid) {
			t.Fatalf("empty edit: %v", err)
		}
		vc, changed, err := v.Edit(1, secrets.VariableEdit{Value: ptr(runtimev1.StringValue("eu"))}, t0)
		if err != nil || vc || changed || v.Version() != 1 {
			t.Fatalf("no-op edit changed state: %v %v %v", vc, changed, err)
		}
		// A value change bumps both versions; a description change only the resource version.
		vc, changed, err = v.Edit(1, secrets.VariableEdit{Value: ptr(runtimev1.StringValue("us"))}, t0.Add(time.Minute))
		if err != nil || !vc || !changed || v.ValueVersion() != 2 || v.Version() != 2 {
			t.Fatalf("value edit: %v %v %v %d/%d", vc, changed, err, v.ValueVersion(), v.Version())
		}
		vc, changed, err = v.Edit(2, secrets.VariableEdit{Description: ptr("region of the ERP")}, t0.Add(2*time.Minute))
		if err != nil || vc || !changed || v.ValueVersion() != 2 || v.Version() != 3 {
			t.Fatalf("description edit: %v %v %v %d/%d", vc, changed, err, v.ValueVersion(), v.Version())
		}
		if v.Name() != "region" || v.ID() != "var_1" {
			t.Fatal("identity changed")
		}
		// Logical deletion: stale delete conflicts, delete bumps, edits are refused, delete repeats are no-ops.
		if _, err := v.Delete(1, t0); conflict(err) == nil {
			t.Fatalf("stale delete: %v", err)
		}
		if ch, err := v.Delete(3, t0); err != nil || !ch || !v.Deleted() || v.Version() != 4 {
			t.Fatalf("delete: %v %v", ch, err)
		}
		if ch, err := v.Delete(4, t0); err != nil || ch {
			t.Fatalf("repeated delete: %v %v", ch, err)
		}
		if _, _, err := v.Edit(4, secrets.VariableEdit{Description: ptr("x")}, t0); !errors.Is(err, secrets.ErrDeleted) {
			t.Fatalf("edit after delete: %v", err)
		}
	})

	t.Run("variable scalar round trips", func(t *testing.T) {
		for _, v := range []runtimev1.Value{runtimev1.StringValue("a"), runtimev1.BoolValue(true), runtimev1.BoolValue(false), runtimev1.IntValue(-42), runtimev1.IntValue(domain.MaxSafeInteger)} {
			x := mustVariable(t, "v", v)
			back, err := secrets.RestoreVariable(x.Record())
			if err != nil || back.Value() != v {
				t.Fatalf("round trip of %v: %v", v, err)
			}
		}
		rec := mustVariable(t, "v", runtimev1.IntValue(1)).Record()
		rec.ValueText = "01"
		if _, err := secrets.RestoreVariable(rec); err == nil {
			t.Error("non-canonical integer restored")
		}
		rec.ValueText = "9007199254740993"
		if _, err := secrets.RestoreVariable(rec); err == nil {
			t.Error("unsafe integer restored")
		}
	})

	t.Run("secret identity, kinds and names", func(t *testing.T) {
		for _, k := range secrets.Kinds() {
			if _, err := secrets.NewSecret(scopeA, "sec_1", "n", k, t0); err != nil {
				t.Errorf("kind %s: %v", k, err)
			}
		}
		if _, err := secrets.NewSecret(scopeA, "sec_1", "n", "oauth", t0); !errors.Is(err, secrets.ErrInvalid) {
			t.Errorf("unknown kind accepted: %v", err)
		}
		for _, n := range []string{"", "Erp", "erp_token", "1erp", "erp/token", strings.Repeat("a", 64)} {
			if _, err := secrets.NewSecret(scopeA, "sec_1", n, secrets.KindGeneric, t0); err == nil {
				t.Errorf("name %q accepted", n)
			}
		}
		s := mustSecret(t)
		if s.Ref() != "secret://project/erp-token" {
			t.Fatalf("ref %s", s.Ref())
		}
		for _, bad := range []string{"secret://tenant/erp-token", "secret://project/Erp", "secret://project/a/b", "erp-token", "secret://project/"} {
			if _, err := secrets.ParseSecretRef(bad); err == nil {
				t.Errorf("reference %q accepted", bad)
			}
		}
		if n, err := secrets.ParseSecretRef(s.Ref()); err != nil || n != "erp-token" {
			t.Fatalf("parse: %q %v", n, err)
		}
	})

	t.Run("monotonic value versions", func(t *testing.T) {
		s := mustSecret(t)
		if n, _ := s.NextVersion(); n != 1 || s.CurrentVersion() != 0 {
			t.Fatal("a new secret has no value yet")
		}
		// Skipping, repeating and moving backwards are refused.
		if err := s.AcceptVersion(1, version(t, s, 2), t0); conflict(err) == nil {
			t.Fatalf("skipped first version: %v", err)
		}
		if err := s.AcceptVersion(1, version(t, s, 1), t0); err != nil {
			t.Fatal(err)
		}
		if s.CurrentVersion() != 1 || s.Version() != 1 {
			t.Fatalf("first version bumped the resource version: %d/%d", s.CurrentVersion(), s.Version())
		}
		if err := s.AcceptVersion(1, version(t, s, 1), t0); conflict(err) == nil {
			t.Fatalf("repeated version: %v", err)
		}
		if err := s.AcceptVersion(9, version(t, s, 2), t0); conflict(err) == nil {
			t.Fatalf("stale resource version: %v", err)
		}
		if err := s.AcceptVersion(1, version(t, s, 2), t0); err != nil {
			t.Fatal(err)
		}
		if s.CurrentVersion() != 2 || s.Version() != 2 {
			t.Fatalf("second version: %d/%d", s.CurrentVersion(), s.Version())
		}
		// A version of another secret is refused.
		other, _ := secrets.NewSecret(scopeA, "sec_2", "other", secrets.KindGit, t0)
		if err := s.AcceptVersion(2, version(t, other, 3), t0); err == nil {
			t.Fatal("foreign version accepted")
		}
		if s.Name() != "erp-token" || s.Kind() != secrets.KindGeneric {
			t.Fatal("identity changed")
		}
	})

	t.Run("irreversible revocation", func(t *testing.T) {
		s := mustSecret(t)
		v1 := version(t, s, 1)
		if err := s.AcceptVersion(1, v1, t0); err != nil {
			t.Fatal(err)
		}
		if _, err := s.RevokeVersion(&v1, "", t0); err == nil {
			t.Fatal("anonymous revocation accepted")
		}
		at := t0.Add(time.Hour)
		changed, err := s.RevokeVersion(&v1, "alice", at)
		if err != nil || !changed || !v1.Revoked() || s.Version() != 2 {
			t.Fatalf("revoke: %v %v %d", changed, err, s.Version())
		}
		// Repeating (by anyone, later) keeps the first time and actor.
		changed, err = s.RevokeVersion(&v1, "bob", at.Add(time.Hour))
		if err != nil || changed || *v1.RevokedAt() != at || v1.Record().RevokedBy != "alice" || s.Version() != 2 {
			t.Fatalf("repeated revoke: %v %v", changed, err)
		}
		// There is no way back: restoring a record that has a time but no actor is invalid.
		rec := v1.Record()
		rec.RevokedBy = ""
		if _, err := secrets.RestoreSecretVersion(rec); err == nil {
			t.Fatal("half-revoked record restored")
		}
	})

	t.Run("logical deletion keeps the tombstone", func(t *testing.T) {
		s := mustSecret(t)
		if err := s.AcceptVersion(1, version(t, s, 1), t0); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Delete(5, t0); conflict(err) == nil {
			t.Fatalf("stale delete: %v", err)
		}
		if ch, err := s.Delete(1, t0); err != nil || !ch || !s.Deleted() || s.Version() != 2 {
			t.Fatalf("delete: %v %v", ch, err)
		}
		if ch, err := s.Delete(2, t0); err != nil || ch {
			t.Fatalf("repeated delete: %v %v", ch, err)
		}
		if err := s.AcceptVersion(2, version(t, s, 2), t0); !errors.Is(err, secrets.ErrDeleted) {
			t.Fatalf("new version after delete: %v", err)
		}
		if s.Name() != "erp-token" {
			t.Fatal("tombstone lost its name")
		}
	})

	t.Run("scope key rewrap changes only wrapping metadata", func(t *testing.T) {
		w := secrets.WrappedKey{Bytes: []byte{1, 2, 3}, KEK: secrets.KEKRef{Provider: "p", ID: "k", Version: "v1"}, Algorithm: "a"}
		k, err := secrets.NewScopeKey("t_acme", "sk_1", 1, w, t0)
		if err != nil {
			t.Fatal(err)
		}
		w2 := w
		w2.KEK.Version = "v2"
		if err := k.Rewrap(5, w2, t0); conflict(err) == nil {
			t.Fatalf("stale rewrap: %v", err)
		}
		if err := k.Rewrap(1, secrets.WrappedKey{}, t0); err == nil {
			t.Fatal("empty wrap accepted")
		}
		if err := k.Rewrap(1, w2, t0.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if k.Revision() != 2 || k.ID() != "sk_1" || k.Generation() != 1 || k.Wrapped().KEK.Version != "v2" {
			t.Fatalf("rewrapped key: rev %d id %s gen %d", k.Revision(), k.ID(), k.Generation())
		}
	})

	t.Run("consumer handling is acknowledged exactly once", func(t *testing.T) {
		h, err := secrets.NewConsumerHandling(scopeA, "ch_1", "sec_1", 1, secrets.ConsumerRef{Kind: "run", ID: "run_1"}, secrets.EffectTerminateConsumer, true, t0)
		if err != nil {
			t.Fatal(err)
		}
		if !h.Pending() {
			t.Fatal("new handling is not pending")
		}
		if _, err := h.Acknowledge(secrets.HandlingPending, "ops", t0); err == nil {
			t.Fatal("acknowledging as pending accepted")
		}
		if _, err := h.Acknowledge(secrets.HandlingCompleted, "", t0); err == nil {
			t.Fatal("anonymous acknowledgement accepted")
		}
		if ch, err := h.Acknowledge(secrets.HandlingCompleted, "ops", t0); err != nil || !ch {
			t.Fatalf("acknowledge: %v %v", ch, err)
		}
		if ch, err := h.Acknowledge(secrets.HandlingCompleted, "ops2", t0); err != nil || ch {
			t.Fatalf("repeat: %v %v", ch, err)
		}
		if _, err := h.Acknowledge(secrets.HandlingNotApplicable, "ops", t0); !errors.Is(err, secrets.ErrInvalidTransition) {
			t.Fatalf("rewriting an acknowledgement: %v", err)
		}
		if _, err := secrets.NewConsumerHandling(scopeA, "ch_1", "sec_1", 1, secrets.ConsumerRef{Kind: "job", ID: "x"}, secrets.EffectTerminateConsumer, true, t0); err == nil {
			t.Fatal("unsupported consumer kind accepted")
		}
	})
}

func ptr[T any](v T) *T { return &v }
