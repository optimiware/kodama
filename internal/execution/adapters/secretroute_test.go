package adapters_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution/adapters"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
	"github.com/optimiware/kodama/internal/secrets"
)

// versionSource is the secrets owner's encrypted store as a controlled double:
// it knows the values of the pinned versions of one scope, records every read
// and every audit entry, and can fail its finalization.
type versionSource struct {
	mu         sync.Mutex
	scope      domain.Scope
	values     map[string]string // name@version -> value
	opens      []secrets.VersionRequest
	audits     []secrets.FinalizeInfo
	finalizeAt error
	openErr    error
	discarded  int
}

func (s *versionSource) Open(_ context.Context, req secrets.VersionRequest) (secrets.PendingValue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opens = append(s.opens, req)
	if s.openErr != nil {
		return nil, s.openErr
	}
	if req.Scope != s.scope {
		return nil, secrets.ErrNotFound
	}
	v, ok := s.values[fmt.Sprintf("%s@%d", req.SecretName, req.Version)]
	if !ok {
		return nil, secrets.ErrNotFound
	}
	return &pendingValue{s: s, value: []byte(v)}, nil
}

func (s *versionSource) auditCount() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.audits) }

type pendingValue struct {
	s     *versionSource
	value []byte
}

func (p *pendingValue) Value() []byte { return p.value }
func (p *pendingValue) Finalize(_ context.Context, info secrets.FinalizeInfo) error {
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	if p.s.finalizeAt != nil {
		return p.s.finalizeAt
	}
	p.s.audits = append(p.s.audits, info)
	return nil
}
func (p *pendingValue) Discard() {
	p.s.mu.Lock()
	p.s.discarded++
	p.s.mu.Unlock()
	for i := range p.value {
		p.value[i] = 0
	}
}

// snapshots is the delivery store's retained snapshots as a controlled double.
type snapshots struct {
	mu    sync.Mutex
	byPin map[string]delivery.ConfigurationSnapshot
	err   error
	reads []domain.Scope
}

func pinKey(env domain.EnvironmentID, d runtimev1.Digest) string {
	return string(env) + "|" + string(d)
}

func (s *snapshots) GetSnapshot(_ context.Context, sc domain.Scope, env domain.EnvironmentID, d runtimev1.Digest) (delivery.ConfigurationSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads = append(s.reads, sc)
	if s.err != nil {
		return delivery.ConfigurationSnapshot{}, s.err
	}
	snap, ok := s.byPin[pinKey(env, d)]
	if !ok || snap.Scope != sc {
		// Like the store: another scope's row is not found.
		return delivery.ConfigurationSnapshot{}, delivery.Fail(delivery.ErrNotFound, nil, "not found")
	}
	return snap, nil
}

type secretWorld struct {
	stack     *rtest.MemStack
	attempt   rtest.Attempt
	token     string
	source    *versionSource
	snapshots *snapshots
	snap      delivery.ConfigurationSnapshot
	route     *adapters.SecretRoute
	// pinned is the merged list of the pinned context: the environment's
	// credentials and the step-only binding.
	pinned []runtimev1.SecretBinding
	// stepOnly is the binding the step declared itself.
	stepOnly runtimev1.SecretBinding
}

const (
	kubeSecret  = "secret://project/kubeconfig"
	tokenSecret = "secret://project/token"
	otherSecret = "secret://project/not-pinned"
	stepSecret  = "secret://project/step-only"
)

func newSecretWorld(t *testing.T) *secretWorld { return newSecretWorldWith(t, nil) }

// newSecretWorldWith seeds the attempt with its pinned context altered by
// mutate (nil keeps it).
func newSecretWorldWith(t *testing.T, mutate func(*runtimev1.Context)) *secretWorld {
	t.Helper()
	s := rtest.NewMemStack(t)
	c, frozen := s.HappyContext()
	// The environment's retained snapshot names the kubeconfig and the token;
	// the step additionally declares a secret of its own. The pinned context
	// merges both lists, exactly as execution builds it.
	snap := snapshot(t, c.Scope(), map[string]string{"HOST": "staging.example.test"}, 7)
	stepOnly := runtimev1.SecretBinding{Ref: stepSecret, Version: 1, Name: "STEP_ONLY", Delivery: runtimev1.DeliveryEnv}
	pinned := append(append([]runtimev1.SecretBinding(nil), snap.Secrets...), stepOnly)
	pin := snap.Pin()
	c.Secrets, c.Environment = pinned, &pin
	if mutate != nil {
		mutate(&c)
	}
	a := s.SeedAttempt(c, rtest.AllActions())
	cred, _, err := s.Register(a, rtest.AllActions())
	if err != nil {
		t.Fatal(err)
	}
	req := frozen
	req.AdmissionCredential = cred.Reveal()
	resp, err := s.Service.Admit(context.Background(), a.Context.AttemptID, req)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	src := &versionSource{scope: a.Scope, values: map[string]string{
		"kubeconfig@7": "kube-bytes", "token@3": "token-bytes", "not-pinned@1": "never-released", "token@4": "newer-than-pinned",
		"step-only@1": "step-only-bytes",
	}}
	snaps := &snapshots{byPin: map[string]delivery.ConfigurationSnapshot{pinKey(snap.Environment, snap.Digest): snap}}
	route, err := adapters.NewSecretRoute(s.Verifier, s.Ring, s.Env.Grants, snaps, src, s.Clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	return &secretWorld{stack: s, attempt: a, token: resp.Token.Token, source: src, snapshots: snaps, snap: snap, route: route, pinned: pinned, stepOnly: stepOnly}
}

func (w *secretWorld) resolve(ref string, version int64) (runtimev1.SecretResolveResponse, error) {
	return w.route.Resolve(context.Background(), w.attempt.Context.AttemptID, w.token, runtimev1.SecretResolveRequest{Ref: ref, Version: version}, "req_secret")
}

func code(t *testing.T, err error) domain.ErrorCode {
	t.Helper()
	var ce *runtimev1.ContractError
	if !errors.As(err, &ce) {
		t.Fatalf("%v is not a contract error", err)
	}
	return ce.Code
}

// Test_runtime_secret_route proves the production resolveSecret route: only the
// secrets the attempt's pinned context names are released, in the grant's
// scope, each resolution is audited, and nothing is released once the attempt
// is no longer authorized.
func Test_runtime_secret_route(t *testing.T) {
	t.Run("resolves exactly the pinned environment's bindings with a read-for-use audit", func(t *testing.T) {
		w := newSecretWorld(t)
		for _, b := range w.snap.Secrets {
			got, err := w.resolve(b.Ref, b.Version)
			if err != nil {
				t.Fatalf("%s: %v", b.Name, err)
			}
			if got.Ref != b.Ref || got.Version != b.Version || got.Name != b.Name || got.Delivery != b.Delivery {
				t.Fatalf("response %+v does not echo the binding %+v", got, b)
			}
			if v, _ := base64.StdEncoding.DecodeString(got.ValueBase64); len(v) == 0 {
				t.Fatalf("%s: no value", b.Name)
			}
		}
		if n := w.source.auditCount(); n != len(w.snap.Secrets) {
			t.Fatalf("%d audit entries, want one read-for-use per resolution (%d)", n, len(w.snap.Secrets))
		}
		for _, sc := range w.snapshots.reads {
			if sc != w.attempt.Scope {
				t.Fatalf("a snapshot was read in scope %+v, want the grant's own", sc)
			}
		}
		for _, o := range w.source.opens {
			if o.Scope != w.attempt.Scope || o.RunID != w.attempt.Context.RunID {
				t.Fatalf("a value was read in scope %+v run %q, want the grant's own", o.Scope, o.RunID)
			}
		}
		for _, a := range w.source.audits {
			if a.AttemptID != w.attempt.Context.AttemptID || a.RunID != w.attempt.Context.RunID || a.RequestID != "req_secret" {
				t.Fatalf("audit %+v does not name the attempt, run and server request", a)
			}
		}
	})

	t.Run("a secret the pinned context does not name is never read", func(t *testing.T) {
		w := newSecretWorld(t)
		for name, req := range map[string]runtimev1.SecretResolveRequest{
			"an unpinned secret":            {Ref: otherSecret, Version: 1},
			"a step-only secret":            {Ref: stepSecret, Version: 1},
			"a newer version of a pinned":   {Ref: tokenSecret, Version: 4},
			"a pinned ref at another value": {Ref: kubeSecret, Version: 6},
		} {
			_, err := w.resolve(req.Ref, req.Version)
			if code(t, err) != domain.CodeForbidden {
				t.Errorf("%s: %v, want forbidden", name, err)
			}
		}
		if len(w.source.opens) != 0 || w.source.auditCount() != 0 {
			t.Fatalf("an undeclared reference reached the store: opens %v audits %d", w.source.opens, w.source.auditCount())
		}
	})

	t.Run("a step-only secret never resolves even though the context lists it", func(t *testing.T) {
		w := newSecretWorld(t)
		listed := false
		for _, b := range w.pinned {
			listed = listed || b == w.stepOnly
		}
		if !listed {
			t.Fatal("fixture: the pinned context must list the step-only binding")
		}
		_, err := w.resolve(w.stepOnly.Ref, w.stepOnly.Version)
		if c := code(t, err); c != domain.CodeForbidden {
			t.Fatalf("a step-only secret: %s, want forbidden", c)
		}
		for _, o := range w.source.opens {
			if o.SecretName == "step-only" {
				t.Fatal("a step-only secret was read from the store")
			}
		}
		// The environment's own credentials still resolve beside it.
		if _, err := w.resolve(kubeSecret, 7); err != nil {
			t.Fatalf("an environment credential: %v", err)
		}
	})

	t.Run("a context binding the environment's snapshot does not name is not resolved", func(t *testing.T) {
		extra := runtimev1.SecretBinding{Ref: otherSecret, Version: 1, Name: "OTHER", Delivery: runtimev1.DeliveryEnv}
		w := newSecretWorldWith(t, func(c *runtimev1.Context) { c.Secrets = append(c.Secrets, extra) })
		if _, err := w.resolve(extra.Ref, extra.Version); code(t, err) != domain.CodeForbidden {
			t.Fatalf("a binding outside the environment's credentials: %v, want forbidden", err)
		}
		if len(w.source.opens) != 0 {
			t.Fatal("it reached the store")
		}
	})

	t.Run("an attempt that pins no environment resolves exactly the bindings its step declared", func(t *testing.T) {
		w := newSecretWorldWith(t, func(c *runtimev1.Context) { c.Environment = nil })
		for _, b := range w.pinned {
			if _, err := w.resolve(b.Ref, b.Version); err != nil {
				t.Errorf("%s: a declared binding was refused: %v", b.Name, err)
			}
		}
		if len(w.snapshots.reads) != 0 {
			t.Fatal("an attempt without an environment read an environment snapshot")
		}
		before := len(w.source.opens)
		for name, req := range map[string]runtimev1.SecretResolveRequest{
			"an undeclared secret":          {Ref: otherSecret, Version: 1},
			"a newer version of a declared": {Ref: tokenSecret, Version: 4},
			"a declared ref at a different": {Ref: kubeSecret, Version: 6},
		} {
			if _, err := w.resolve(req.Ref, req.Version); code(t, err) != domain.CodeForbidden {
				t.Errorf("%s: %v, want forbidden", name, err)
			}
		}
		if len(w.source.opens) != before {
			t.Fatal("an undeclared reference reached the store")
		}
	})

	t.Run("a missing, foreign, altered or unreadable snapshot fails closed", func(t *testing.T) {
		foreignScope := domain.Scope{TenantID: "t_foreign", ProjectID: "p_foreign"}
		for name, tc := range map[string]struct {
			arrange func(w *secretWorld)
			want    domain.ErrorCode
		}{
			"missing": {func(w *secretWorld) { w.snapshots.byPin = map[string]delivery.ConfigurationSnapshot{} }, domain.CodeNotFound},
			"held only by another scope": {func(w *secretWorld) {
				foreign := snapshot(t, foreignScope, map[string]string{"HOST": "staging.example.test"}, 7)
				w.snapshots.byPin = map[string]delivery.ConfigurationSnapshot{pinKey(w.snap.Environment, w.snap.Digest): foreign}
			}, domain.CodeNotFound},
			"answered with another scope's row": {func(w *secretWorld) {
				foreign := w.snap
				foreign.Scope = foreignScope
				w.snapshots.byPin[pinKey(w.snap.Environment, w.snap.Digest)] = foreign
				w.route = routeOver(t, w, anyScope{w.snapshots})
			}, domain.CodeNotFound},
			"carrying another pin": {func(w *secretWorld) {
				other := snapshot(t, w.attempt.Scope, map[string]string{"HOST": "other.example.test"}, 7)
				w.snapshots.byPin[pinKey(w.snap.Environment, w.snap.Digest)] = other
			}, domain.CodeNotFound},
			"content not covered by its digest": {func(w *secretWorld) {
				bad := w.snap
				bad.Namespace = "tampered"
				w.snapshots.byPin[pinKey(w.snap.Environment, w.snap.Digest)] = bad
			}, domain.CodeNotFound},
			"unreadable": {func(w *secretWorld) { w.snapshots.err = errors.New("connection reset") }, domain.CodeUnavailable},
		} {
			w := newSecretWorld(t)
			tc.arrange(w)
			resp, err := w.resolve(kubeSecret, 7)
			if c := code(t, err); c != tc.want || resp.ValueBase64 != "" {
				t.Errorf("%s: %v %+v, want %s and no value", name, err, resp, tc.want)
			}
			if len(w.source.opens) != 0 || w.source.auditCount() != 0 {
				t.Errorf("%s: a secret was read without a trustworthy environment snapshot", name)
			}
		}
	})

	t.Run("a repeated resolution is a new audited release of the same pinned value", func(t *testing.T) {
		w := newSecretWorld(t)
		first, err := w.resolve(kubeSecret, 7)
		if err != nil {
			t.Fatal(err)
		}
		second, err := w.resolve(kubeSecret, 7)
		if err != nil {
			t.Fatalf("the secrets module treats a retry as a new authorized resolution: %v", err)
		}
		if first != second {
			t.Fatal("a repeated resolution returned a different value")
		}
		if n := w.source.auditCount(); n != 2 {
			t.Fatalf("%d audit entries, want one per resolution (2)", n)
		}
		if w.source.discarded != 2 {
			t.Fatalf("%d decrypted buffers discarded, want 2", w.source.discarded)
		}
	})

	t.Run("nothing is returned for another attempt, tenant or a bad credential", func(t *testing.T) {
		w := newSecretWorld(t)
		ctx := context.Background()
		req := runtimev1.SecretResolveRequest{Ref: kubeSecret, Version: 7}
		for name, tc := range map[string]struct {
			attempt domain.AttemptID
			bearer  string
		}{
			"another attempt's path": {attempt: "att_other", bearer: w.token},
			"a malformed attempt":    {attempt: "../att", bearer: w.token},
			"no bearer":              {attempt: w.attempt.Context.AttemptID, bearer: ""},
			"a garbage bearer":       {attempt: w.attempt.Context.AttemptID, bearer: "not-a-token"},
			"another tenant's token": {attempt: w.attempt.Context.AttemptID, bearer: w.stack.MintToken(rtest.MintClaims{Subject: "ses_x", Tenant: "t_foreign"})},
		} {
			_, err := w.route.Resolve(ctx, tc.attempt, tc.bearer, req, "req_x")
			if c := code(t, err); c != domain.CodeInvalidCredential {
				t.Errorf("%s: %s, want invalid_credential", name, c)
			}
		}
		if len(w.source.opens) != 0 {
			t.Fatalf("a rejected request reached the store: %v", w.source.opens)
		}
		// The store itself finds nothing in another scope: even a binding the
		// attempt pinned resolves nothing there.
		w.source.scope = domain.Scope{TenantID: "t_foreign", ProjectID: w.attempt.Scope.ProjectID}
		_, err := w.resolve(kubeSecret, 7)
		if c := code(t, err); c != domain.CodeNotFound {
			t.Fatalf("a value of another scope: %s, want not_found", c)
		}
		if w.source.auditCount() != 0 {
			t.Fatal("a failed resolution was audited as a release")
		}
	})

	t.Run("a cancelled or revoked attempt resolves nothing", func(t *testing.T) {
		w := newSecretWorld(t)
		if _, err := w.resolve(tokenSecret, 3); err != nil {
			t.Fatal(err)
		}
		if err := w.stack.Env.Cancel(context.Background(), w.attempt.Scope, w.attempt.Context.AttemptID, true, w.stack.Clock.Now()); err != nil {
			t.Fatal(err)
		}
		before := len(w.source.opens)
		_, err := w.resolve(tokenSecret, 3)
		if c := code(t, err); c != runtimev1.CodeAttemptRevoked {
			t.Fatalf("after cancellation: %s, want attempt_revoked", c)
		}
		if len(w.source.opens) != before {
			t.Fatal("a cancelled attempt reached the store")
		}
	})

	t.Run("an action the attempt was not granted is forbidden", func(t *testing.T) {
		w := newSecretWorld(t)
		narrow, err := runtimeauth.NewActionSet(runtimeauth.ActionContextRead)
		if err != nil {
			t.Fatal(err)
		}
		w.stack.Env.Narrow(t, w.attempt.Scope, w.attempt.Context.AttemptID, 1, narrow)
		_, err = w.resolve(tokenSecret, 3)
		if c := code(t, err); c != domain.CodeForbidden && c != runtimev1.CodeAttemptRevoked {
			t.Fatalf("a narrowed attempt: %s, want forbidden or attempt_revoked", c)
		}
		if len(w.source.opens) != 0 {
			t.Fatal("a narrowed attempt reached the store")
		}
	})

	t.Run("an outage or an unrecorded audit releases nothing", func(t *testing.T) {
		w := newSecretWorld(t)
		w.source.finalizeAt = secrets.ErrFinalizationUncertain
		resp, err := w.resolve(tokenSecret, 3)
		if c := code(t, err); c != domain.CodeUnavailable || resp.ValueBase64 != "" {
			t.Fatalf("an uncertain audit: %v %+v, want unavailable and no value", err, resp)
		}
		w.source.finalizeAt = nil
		w.source.openErr = &secrets.KeyError{Class: secrets.KeyUnavailable}
		resp, err = w.resolve(tokenSecret, 3)
		if c := code(t, err); c != domain.CodeUnavailable || resp.ValueBase64 != "" {
			t.Fatalf("a key outage: %v %+v, want unavailable and no value", err, resp)
		}
		if w.source.auditCount() != 0 {
			t.Fatal("a release that failed was audited")
		}
		w.source.openErr = nil
		if _, err := w.resolve(tokenSecret, 3); err != nil {
			t.Fatalf("after recovery: %v", err)
		}
	})

	t.Run("the grant store being down is unavailable, never a pass", func(t *testing.T) {
		w := newSecretWorld(t)
		down, err := adapters.NewSecretRoute(w.stack.Verifier, w.stack.Ring, downStore{w.stack.Env.Grants}, w.snapshots, w.source, w.stack.Clock.Now)
		if err != nil {
			t.Fatal(err)
		}
		w.route = down
		_, err = w.resolve(tokenSecret, 3)
		if c := code(t, err); c != domain.CodeUnavailable {
			t.Fatalf("a grant store outage: %s, want unavailable", c)
		}
		if len(w.source.opens) != 0 {
			t.Fatal("a store reached the version source without authority")
		}
	})

	t.Run("a route without its collaborators cannot be built", func(t *testing.T) {
		w := newSecretWorld(t)
		s := w.stack
		if _, err := adapters.NewSecretRoute(nil, s.Ring, s.Env.Grants, w.snapshots, w.source, nil); err == nil {
			t.Error("built without a verifier")
		}
		if _, err := adapters.NewSecretRoute(s.Verifier, nil, s.Env.Grants, w.snapshots, w.source, nil); err == nil {
			t.Error("built without a signing generation")
		}
		if _, err := adapters.NewSecretRoute(s.Verifier, s.Ring, nil, w.snapshots, w.source, nil); err == nil {
			t.Error("built without a grant store")
		}
		if _, err := adapters.NewSecretRoute(s.Verifier, s.Ring, s.Env.Grants, nil, w.source, nil); err == nil {
			t.Error("built without environment snapshots")
		}
		if _, err := adapters.NewSecretRoute(s.Verifier, s.Ring, s.Env.Grants, w.snapshots, nil, nil); err == nil {
			t.Error("built without a version source")
		}
	})
}

// downStore is a grant store whose transactions cannot start.
type downStore struct{ runtimeauth.Store }

func (downStore) Do(context.Context, domain.TenantID, func(context.Context, runtimeauth.GrantTx) error) error {
	return errors.New("database unavailable")
}

// anyScope answers the recorded snapshot whatever scope was asked, like a peer
// that does not filter: the route's own scope check must hold.
type anyScope struct{ s *snapshots }

func (a anyScope) GetSnapshot(_ context.Context, _ domain.Scope, env domain.EnvironmentID, d runtimev1.Digest) (delivery.ConfigurationSnapshot, error) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	if snap, ok := a.s.byPin[pinKey(env, d)]; ok {
		return snap, nil
	}
	return delivery.ConfigurationSnapshot{}, delivery.Fail(delivery.ErrNotFound, nil, "not found")
}

func routeOver(t *testing.T, w *secretWorld, pins adapters.EnvironmentSnapshots) *adapters.SecretRoute {
	t.Helper()
	r, err := adapters.NewSecretRoute(w.stack.Verifier, w.stack.Ring, w.stack.Env.Grants, pins, w.source, w.stack.Clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
