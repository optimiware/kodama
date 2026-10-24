package adapters_test

import (
	"bytes"
	"context"
	"errors"
	"log"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
	"github.com/optimiware/kodama/internal/integrations/gitwebhook/adapters"
	"github.com/optimiware/kodama/internal/integrations/gitwebhook/gitwebhooktest"
	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/internal/secrets/secretstest"
)

// hookEnv is the secrets module in memory with a secret the hook can reference.
type hookEnv struct {
	t     *testing.T
	env   *secretstest.MemEnv
	svc   *secrets.Service
	src   *adapters.HookSecrets
	scope domain.Scope
}

func newHookEnv(t *testing.T) *hookEnv {
	t.Helper()
	env := secretstest.NewMemEnv(t)
	src, err := adapters.NewHookSecrets(adapters.HookSecretsConfig{Reader: env.Store_, Wrapper: env.Wrap, Installation: secretstest.Installation})
	if err != nil {
		t.Fatal(err)
	}
	return &hookEnv{t: t, env: env, svc: env.Service(t, env.Wrap), src: src, scope: secretstest.ScopeA}
}

func (h *hookEnv) mutation() secrets.Mutation {
	return secrets.Mutation{Actor: secrets.Actor{Type: secrets.ActorUser, ID: "u_admin"}, RequestID: "req", Now: time.Now().UTC()}
}

func (h *hookEnv) create(scope domain.Scope, name string, kind secrets.SecretKind, value string) secrets.Secret {
	h.t.Helper()
	ctx := context.Background()
	m := h.mutation()
	p, err := h.svc.PrepareSecret(ctx, m, scope, name, kind, []byte(value))
	if err != nil {
		h.t.Fatal(err)
	}
	var out secrets.Secret
	if err := h.svc.Atomically(ctx, scope.TenantID, func(ctx context.Context, w secrets.Writer) error {
		var err error
		out, err = h.svc.CreateSecret(ctx, w, m, p)
		return err
	}); err != nil {
		h.t.Fatal(err)
	}
	return out
}

func (h *hookEnv) rotate(sec secrets.Secret, value string) secrets.Secret {
	h.t.Helper()
	ctx := context.Background()
	m := h.mutation()
	p, err := h.svc.PrepareVersion(ctx, m, sec.Scope(), sec.ID(), sec.Version(), []byte(value))
	if err != nil {
		h.t.Fatal(err)
	}
	var out secrets.Secret
	if err := h.svc.Atomically(ctx, sec.Scope().TenantID, func(ctx context.Context, w secrets.Writer) error {
		var err error
		out, err = h.svc.AddVersion(ctx, w, m, p)
		return err
	}); err != nil {
		h.t.Fatal(err)
	}
	return out
}

func (h *hookEnv) revoke(sec secrets.Secret, version int64) {
	h.t.Helper()
	if err := h.svc.Atomically(context.Background(), sec.Scope().TenantID, func(ctx context.Context, w secrets.Writer) error {
		_, err := h.svc.RevokeVersion(ctx, w, h.mutation(), sec.Scope(), sec.ID(), version)
		return err
	}); err != nil {
		h.t.Fatal(err)
	}
}

const hookValue = "hook-secret-value-0123456789-abcdefghijklmnop" // 45 bytes: a strong hook secret

// captureLogs routes every standard logger to a buffer for the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	oldStd, oldFlags := log.Writer(), log.Flags()
	oldSlog := slog.Default()
	log.SetOutput(&buf)
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() {
		log.SetOutput(oldStd)
		log.SetFlags(oldFlags)
		slog.SetDefault(oldSlog)
	})
	return &buf
}

// Test_git_trigger_hook_secret_from_secrets: the hook secret resolves from the
// secrets module by reference; a missing or foreign reference is refused; the
// value never appears in an error or a log.
func Test_git_trigger_hook_secret_from_secrets(t *testing.T) {
	ctx := context.Background()

	t.Run("resolves the current version by reference", func(t *testing.T) {
		h := newHookEnv(t)
		sec := h.create(h.scope, "hook", secrets.KindGeneric, hookValue)
		got, err := h.src.Resolve(ctx, h.scope, sec.Ref())
		if err != nil || string(got) != hookValue {
			t.Fatalf("resolve = %q, %v", got, err)
		}
		// The caller owns the bytes: clearing them does not affect the next read.
		clear(got)
		again, err := h.src.Resolve(ctx, h.scope, "secret://project/hook")
		if err != nil || string(again) != hookValue {
			t.Fatalf("second resolve = %q, %v", again, err)
		}
		// Rotation: the installation's secret is the current version, resolved
		// just in time on every call.
		h.rotate(sec, hookValue+"-rotated")
		rotated, err := h.src.Resolve(ctx, h.scope, sec.Ref())
		if err != nil || string(rotated) != hookValue+"-rotated" {
			t.Fatalf("after rotation = %q, %v", rotated, err)
		}
	})

	t.Run("a missing, malformed or foreign reference is refused", func(t *testing.T) {
		h := newHookEnv(t)
		h.create(h.scope, "hook", secrets.KindGeneric, hookValue)
		h.create(secretstest.ScopeB, "hook-b", secrets.KindGeneric, hookValue+"-b")
		other := secretstest.ScopeA2
		for name, c := range map[string]struct {
			scope domain.Scope
			ref   string
		}{
			"missing":             {h.scope, "secret://project/absent"},
			"empty":               {h.scope, ""},
			"bare name":           {h.scope, "hook"},
			"another scheme":      {h.scope, "vault://project/hook"},
			"another scope":       {h.scope, "secret://tenant/hook"},
			"extra segment":       {h.scope, "secret://project/hook/extra"},
			"another tenant":      {secretstest.ScopeB, "secret://project/hook"},
			"another project":     {other, "secret://project/hook"},
			"foreign name":        {h.scope, "secret://project/hook-b"},
			"no scope":            {domain.Scope{}, "secret://project/hook"},
			"sibling name prefix": {h.scope, "secret://project/hoo"},
		} {
			got, err := h.src.Resolve(ctx, c.scope, c.ref)
			if !errors.Is(err, gw.ErrSecretNotFound) || got != nil {
				t.Errorf("%s: got %q, %v; want ErrSecretNotFound", name, got, err)
			}
		}
	})

	t.Run("a revoked, deleted or non-generic secret is refused", func(t *testing.T) {
		h := newHookEnv(t)
		revoked := h.create(h.scope, "revoked", secrets.KindGeneric, hookValue)
		h.revoke(revoked, 1)
		gitCred := h.create(h.scope, "checkout", secrets.KindGit, hookValue)
		deleted := h.create(h.scope, "gone", secrets.KindGeneric, hookValue)
		if err := h.svc.Atomically(ctx, h.scope.TenantID, func(ctx context.Context, w secrets.Writer) error {
			_, _, err := h.svc.DeleteSecret(ctx, w, h.mutation(), h.scope, deleted.ID(), deleted.Version())
			return err
		}); err != nil {
			t.Fatal(err)
		}
		for name, ref := range map[string]string{"revoked": revoked.Ref(), "git credential": gitCred.Ref(), "deleted": deleted.Ref()} {
			if got, err := h.src.Resolve(ctx, h.scope, ref); !errors.Is(err, gw.ErrSecretNotFound) || got != nil {
				t.Errorf("%s: got %q, %v; want ErrSecretNotFound", name, got, err)
			}
		}
	})

	t.Run("the value is in no error and no log", func(t *testing.T) {
		buf := captureLogs(t)
		h := newHookEnv(t)
		sec := h.create(h.scope, "hook", secrets.KindGeneric, hookValue)
		outage := errors.New("kms exploded for " + hookValue) // a provider error that echoes the value
		h.env.Wrap.SetDown(outage)
		_, err := h.src.Resolve(ctx, h.scope, sec.Ref())
		h.env.Wrap.SetDown(nil)
		if !errors.Is(err, gw.ErrUnavailable) {
			t.Fatalf("a key-provider outage = %v, want ErrUnavailable", err)
		}
		if errors.Is(err, outage) || strings.Contains(err.Error(), "kms") || strings.Contains(err.Error(), hookValue) {
			t.Fatalf("the provider's error leaked into %q", err)
		}
		var all []string
		for _, ref := range []string{sec.Ref(), "secret://project/absent", "bad"} {
			got, err := h.src.Resolve(ctx, h.scope, ref)
			if err != nil {
				all = append(all, err.Error())
			}
			clear(got)
		}
		for _, s := range append(all, err.Error(), buf.String()) {
			if strings.Contains(s, hookValue) || strings.Contains(s, "hook-secret-value") {
				t.Fatalf("the secret value appears in %q", s)
			}
		}
	})

	t.Run("a stored value that fails authentication is an outage, not a missing secret", func(t *testing.T) {
		h := newHookEnv(t)
		sec := h.create(h.scope, "hook", secrets.KindGeneric, hookValue)
		// A reader that answers for another secret name's envelope context.
		src, err := adapters.NewHookSecrets(adapters.HookSecretsConfig{Reader: h.env.Store_, Wrapper: h.env.Wrap, Installation: "another-installation"})
		if err != nil {
			t.Fatal(err)
		}
		got, err := src.Resolve(ctx, h.scope, sec.Ref())
		if !errors.Is(err, gw.ErrUnavailable) || errors.Is(err, gw.ErrSecretNotFound) || got != nil {
			t.Fatalf("got %q, %v; want ErrUnavailable", got, err)
		}
	})

	t.Run("construction fails closed", func(t *testing.T) {
		h := newHookEnv(t)
		for name, cfg := range map[string]adapters.HookSecretsConfig{
			"no reader":       {Wrapper: h.env.Wrap, Installation: "i"},
			"no wrapper":      {Reader: h.env.Store_, Installation: "i"},
			"no installation": {Reader: h.env.Store_, Wrapper: h.env.Wrap},
		} {
			if _, err := adapters.NewHookSecrets(cfg); err == nil {
				t.Errorf("%s: construction succeeded", name)
			}
		}
	})

	t.Run("the trigger installs and authenticates with a secret from the module", func(t *testing.T) {
		f := gitwebhooktest.NewFixture(t)
		h := newHookEnv(t)
		sec := h.create(f.Scope, "hook", secrets.KindGeneric, hookValue)
		foreign := h.create(f.Other, "hook-other", secrets.KindGeneric, hookValue)
		svc, err := gw.New(gw.Dependencies{
			Store: f.Store, Authorizer: f.Auth, Secrets: h.src, Definitions: f.Defs, Repositories: f.Repos,
			Git: f.Git, Runs: f.Runs, Now: f.Clock.Now,
		})
		if err != nil {
			t.Fatal(err)
		}
		install := func(ref string) (gw.Trigger, error) {
			return svc.Install(ctx, gw.InstallCommand{Scope: f.Scope, Principal: gitwebhooktest.Installer, DefinitionID: gitwebhooktest.DefinitionName, Branches: []string{"main"}, SecretRef: ref})
		}
		if _, err := install("secret://project/absent"); !errors.Is(err, gw.ErrNotFound) {
			t.Fatalf("install with a missing reference = %v", err)
		}
		if _, err := install(foreign.Ref()); !errors.Is(err, gw.ErrNotFound) {
			t.Fatalf("install with another tenant's reference = %v, want ErrNotFound", err)
		}
		tr, err := install(sec.Ref())
		if err != nil {
			t.Fatalf("install: %v", err)
		}
		body := gitwebhooktest.Marshal(t, f.Payload("refs/heads/feature/x", gitwebhooktest.Zero, string(gitwebhooktest.CommitA)))
		rec, err := svc.Receive(ctx, f.SignWith([]byte(hookValue), tr.ID, "d-ok", f.Clock.Now().Unix(), body))
		if err != nil || rec.Outcome != gw.OutcomeIgnored {
			t.Fatalf("a delivery signed with the module's secret = %+v, %v", rec, err)
		}
		if _, err := svc.Receive(ctx, f.SignWith([]byte(hookValue+"x"), tr.ID, "d-bad", f.Clock.Now().Unix(), body)); !errors.Is(err, gw.ErrUnauthenticated) {
			t.Fatalf("a wrong secret = %v, want ErrUnauthenticated", err)
		}
		// Revoking the secret ends authentication at once: no cached value.
		h.revoke(sec, 1)
		if _, err := svc.Receive(ctx, f.SignWith([]byte(hookValue), tr.ID, "d-after", f.Clock.Now().Unix(), body)); !errors.Is(err, gw.ErrUnauthenticated) {
			t.Fatalf("a revoked secret = %v, want ErrUnauthenticated", err)
		}
	})
}
