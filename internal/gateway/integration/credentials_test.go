package integration_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/gateway"
	"github.com/optimiware/kodama/internal/gateway/adapters"
	"github.com/optimiware/kodama/internal/gateway/gatewaytest"
	"github.com/optimiware/kodama/internal/secrets"
	secretspg "github.com/optimiware/kodama/internal/secrets/postgres"
	"github.com/optimiware/kodama/internal/storage"
)

const bearerV2 = "sk-t032-SECOND-VERSION-SENTINEL"

func quietTimeouts() gateway.Timeouts {
	t := fastTimeouts(20 * time.Second)
	t.Recheck = 30 * time.Second // no heartbeat commits interleave with the injected faults
	return t
}

func lastAuth(t *testing.T, e *gatewaytest.Env) string {
	t.Helper()
	calls := e.Provider.Calls()
	if len(calls) == 0 {
		t.Fatal("the provider saw no request")
	}
	return calls[len(calls)-1].Authorization
}

func releasePins(t *testing.T, e *gatewaytest.Env, run domain.RunID) {
	t.Helper()
	err := storage.WithTenant(context.Background(), e.X.Runtime, scopeA.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := e.X.Secrets.ReleasePins(ctx, tx, scopeA, secretspg.RunConsumer(run))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Test_credential_lifecycle: GM-02, SD-01..SD-08 (secret use), GM-A01. The
// provider credential is a typed secret at an exact version pinned for the run,
// is never substituted by a newer version, is refused once revoked or released,
// and no failure discloses it.
func Test_credential_lifecycle(t *testing.T) {
	t.Run("exact version: rotation changes later runs only, through settings", func(t *testing.T) {
		e := newWorld(t)
		sec := e.SecretNamed(scopeA, gatewaytest.SecretName)
		run1 := e.NewAttempt(scopeA)
		if _, err := e.Invoke(e.Replica, run1, body("a"), &gatewaytest.Sink{}); err != nil {
			t.Fatal(err)
		}
		if got := lastAuth(t, e); got != "Bearer "+gatewaytest.Bearer {
			t.Fatalf("version 1 key expected, provider got %q", got)
		}
		var pins int
		e.Scalar(&pins, `SELECT count(*) FROM secret_pins WHERE consumer_kind = 'run' AND consumer_id = $1 AND secret_version = 1 AND released_at IS NULL`, string(run1.RunID))
		if pins != 1 {
			t.Fatalf("the run holds %d pins of the credential, want 1 (registered with the binding)", pins)
		}

		e.Rotate(scopeA, sec.ID(), e.Payload(bearerV2, e.Provider.Origin()))
		// Settings still pin version 1: a new run must not receive the newest value.
		run2 := e.NewAttempt(scopeA)
		if _, err := e.Invoke(e.Replica, run2, body("b"), &gatewaytest.Sink{}); err != nil {
			t.Fatal(err)
		}
		if got := lastAuth(t, e); got != "Bearer "+gatewaytest.Bearer {
			t.Fatalf("the newest version was substituted: %q", got)
		}
		// Moving the pin is an authorized settings change; only later runs follow it.
		ed := e.Edit()
		ed.Credential.Version = 2
		e.Configure(e.Replica, scopeA, ed)
		run3 := e.NewAttempt(scopeA)
		if _, err := e.Invoke(e.Replica, run3, body("c"), &gatewaytest.Sink{}); err != nil {
			t.Fatal(err)
		}
		if got := lastAuth(t, e); got != "Bearer "+bearerV2 {
			t.Fatalf("run 3 should use version 2: %q", got)
		}
		if _, err := e.Invoke(e.Replica, run1, body("d"), &gatewaytest.Sink{}); err != nil {
			t.Fatal(err)
		}
		if got := lastAuth(t, e); got != "Bearer "+gatewaytest.Bearer {
			t.Fatalf("a bound run changed its credential version: %q", got)
		}
	})

	t.Run("a revoked version is refused before any provider traffic", func(t *testing.T) {
		e := newWorld(t)
		sec := e.SecretNamed(scopeA, gatewaytest.SecretName)
		run1 := e.NewAttempt(scopeA)
		if _, err := e.Invoke(e.Replica, run1, body("a"), &gatewaytest.Sink{}); err != nil {
			t.Fatal(err)
		}
		calls := len(e.Provider.Calls())
		if err := e.Revoke(scopeA, sec.ID(), 1); err != nil {
			t.Fatal(err)
		}
		_, err := e.Invoke(e.Replica, run1, body("b"), &gatewaytest.Sink{})
		if !errors.Is(err, gateway.ErrCredential) || strings.Contains(err.Error(), gatewaytest.Bearer) {
			t.Fatalf("a revoked credential: %v", err)
		}
		if len(e.Provider.Calls()) != calls {
			t.Fatal("the revoked credential was sent to the provider")
		}
		// The unsent request is charged nothing and frees the slot.
		if st := reqState(t, e, run1.RunID, 2); st != [2]string{"failed", "credential_unavailable"} {
			t.Fatalf("request state %v", st)
		}
		if _, charged := e.Budget(run1.RunID); charged != 13 {
			t.Fatalf("charged %d, want only the first request's 13", charged)
		}
		// A new run cannot even bind a revoked version.
		run2 := e.NewAttempt(scopeA)
		if _, err := e.Invoke(e.Replica, run2, body("c"), &gatewaytest.Sink{}); !errors.Is(err, gateway.ErrCredential) {
			t.Fatalf("binding a revoked version: %v", err)
		}
		if n := e.Count(`SELECT count(*) FROM model_run_bindings WHERE run_id = $1`, string(run2.RunID)); n != 0 {
			t.Fatal("a binding to a revoked version was stored")
		}
		if len(e.Provider.Calls()) != calls {
			t.Fatal("provider traffic after revocation")
		}
	})

	t.Run("revocation racing decryption releases nothing", func(t *testing.T) {
		e := newWorld(t)
		sec := e.SecretNamed(scopeA, gatewaytest.SecretName)
		a := e.NewAttempt(scopeA)
		fired := false
		e.Wrapper.BeforeUnwrap(func() {
			if !fired {
				fired = true
				if err := e.Revoke(scopeA, sec.ID(), 1); err != nil {
					t.Error(err)
				}
			}
		})
		_, err := e.Invoke(e.Replica, a, body("x"), &gatewaytest.Sink{})
		if !fired || !errors.Is(err, gateway.ErrCredential) {
			t.Fatalf("fired=%v err=%v", fired, err)
		}
		if n := len(e.Provider.Calls()); n != 0 {
			t.Fatalf("provider calls %d: a version revoked during decryption was used", n)
		}
		if st := reqState(t, e, a.RunID, 1); st != [2]string{"failed", "credential_unavailable"} {
			t.Fatalf("request state %v", st)
		}
	})

	t.Run("released pins end the run's access to the credential", func(t *testing.T) {
		e := newWorld(t)
		a := e.NewAttempt(scopeA)
		if _, err := e.Invoke(e.Replica, a, body("a"), &gatewaytest.Sink{}); err != nil {
			t.Fatal(err)
		}
		releasePins(t, e, a.RunID)
		calls := len(e.Provider.Calls())
		if _, err := e.Invoke(e.Replica, a, body("b"), &gatewaytest.Sink{}); !errors.Is(err, gateway.ErrCredential) {
			t.Fatalf("after pin release: %v", err)
		}
		if len(e.Provider.Calls()) != calls {
			t.Fatal("a released pin still opened the credential")
		}
	})

	t.Run("key-provider failure discloses no credential and is retryable", func(t *testing.T) {
		e := newWorld(t)
		logs := &syncBuffer{}
		r := e.NewReplica(gatewaytest.ReplicaOptions{Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
		a := e.NewAttempt(scopeA)
		e.Wrapper.FailUnwrap(gatewaytest.ErrInjected)
		_, err := e.Invoke(r, a, body("x"), &gatewaytest.Sink{})
		if !errors.Is(err, gateway.ErrUnavailable) {
			t.Fatalf("key-provider failure: %v", err)
		}
		if strings.Contains(err.Error(), "CAUSE-SENTINEL") || strings.Contains(err.Error(), gatewaytest.Bearer) {
			t.Fatalf("the error leaks the cause: %v", err)
		}
		absent(t, "diagnostics", logs.String())
		absent(t, "stored state", e.Dump(scopeA.TenantID))
		if n := len(e.Provider.Calls()); n != 0 {
			t.Fatalf("provider calls %d", n)
		}
		e.Wrapper.FailUnwrap(nil)
		if _, err := e.Invoke(r, a, body("x"), &gatewaytest.Sink{}); err != nil {
			t.Fatalf("after the key provider recovered: %v", err)
		}
		// The failed first request was unsent: free of charge, slot released.
		if st := reqState(t, e, a.RunID, 1); st != [2]string{"failed", "credential_unavailable"} {
			t.Fatalf("request state %v", st)
		}
	})

	t.Run("a credential bound to another origin is never sent to the endpoint", func(t *testing.T) {
		e := gatewaytest.New(t)
		e.CreateSecret(scopeA, gatewaytest.SecretName, secrets.KindModel, e.Payload(gatewaytest.Bearer, "https://other.example.invalid"))
		e.Configure(e.Replica, scopeA, e.Edit())
		a := e.NewAttempt(scopeA)
		if _, err := e.Invoke(e.Replica, a, body("x"), &gatewaytest.Sink{}); !errors.Is(err, gateway.ErrCredential) {
			t.Fatalf("an audience mismatch: %v", err)
		}
		if n := len(e.Provider.Calls()); n != 0 {
			t.Fatalf("provider calls %d: the key was sent outside its audience", n)
		}
	})

	t.Run("an ambiguous finalization discloses no credential and repeats no call", func(t *testing.T) {
		e := newWorld(t)
		logs := &syncBuffer{}
		r := e.NewReplica(gatewaytest.ReplicaOptions{Timeouts: quietTimeouts(), Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
		a := e.NewAttempt(scopeA)
		r.DB.LoseCommits(2, 1) // admission, begin-forwarding, then the finalization commit is lost
		sink := &gatewaytest.Sink{}
		res, err := e.Invoke(r, a, body("x"), sink)
		if err != nil || res.Usage == nil {
			t.Fatalf("a lost finalization commit is recovered by rereading: %+v %v", res, err)
		}
		if n := len(e.Provider.Calls()); n != 1 {
			t.Fatalf("provider calls %d", n)
		}
		frames, fin, _ := sink.Snapshot()
		if !fin || strings.Contains(strings.Join(frames, ""), gatewaytest.Bearer) {
			t.Fatal("the workload's stream is incomplete or carries the credential")
		}
		absent(t, "diagnostics", logs.String())
		absent(t, "stored state", e.Dump(scopeA.TenantID))
	})

	t.Run("the gateway pin does not change the attempt's immutable context", func(t *testing.T) {
		e := newWorld(t)
		a := e.NewAttempt(scopeA)
		before, doc := e.AttemptContext(a)
		if len(doc.Secrets) != 0 {
			t.Fatalf("fixture attempt declares secrets: %+v", doc.Secrets)
		}
		if _, err := e.Invoke(e.Replica, a, body("x"), &gatewaytest.Sink{}); err != nil {
			t.Fatal(err)
		}
		var pins int
		e.Scalar(&pins, `SELECT count(*) FROM secret_pins WHERE consumer_kind = 'run' AND consumer_id = $1 AND released_at IS NULL`, string(a.RunID))
		if pins != 1 {
			t.Fatalf("the run holds %d pins, want the gateway's 1", pins)
		}
		after, doc2 := e.AttemptContext(a)
		if !bytes.Equal(before, after) {
			t.Fatal("registering the gateway pin changed the attempt's stored context")
		}
		if len(doc2.Secrets) != 0 {
			t.Fatalf("the attempt now declares secret bindings: %+v", doc2.Secrets)
		}
		for k, v := range doc2.Command.Env {
			if strings.Contains(k+v, gatewaytest.Bearer) || strings.Contains(v, "secret://") {
				t.Fatalf("the command environment carries the model credential or its reference: %s", k)
			}
		}
		absent(t, "attempt context", string(after))
	})

	t.Run("a gateway pin alone cannot authorize workload plaintext resolution", func(t *testing.T) {
		e := newWorld(t)
		a := e.NewAttempt(scopeA)
		if _, err := e.Invoke(e.Replica, a, body("x"), &gatewaytest.Sink{}); err != nil {
			t.Fatal(err)
		}
		calls := len(e.Provider.Calls())
		resolver, err := secrets.NewResolver(contextAuthority{e: e, a: a}, e.VersionSource())
		if err != nil {
			t.Fatal(err)
		}
		_, err = resolver.Resolve(context.Background(), a.ID, runtimev1.SecretResolveRequest{Ref: "secret://project/" + gatewaytest.SecretName, Version: 1}, "req_t032")
		if !errors.Is(err, secrets.ErrUndeclared) {
			t.Fatalf("resolving the gateway-pinned credential as the workload: %v, want ErrUndeclared", err)
		}
		if err != nil && strings.Contains(err.Error(), gatewaytest.Bearer) {
			t.Fatal("the denial discloses the credential")
		}
		if len(e.Provider.Calls()) != calls {
			t.Fatal("unexpected provider traffic")
		}
	})

	t.Run("version sources fail closed for missing, unpinned, rotated and foreign versions", func(t *testing.T) {
		e := newWorld(t)
		sec := e.SecretNamed(scopeA, gatewaytest.SecretName)
		a := e.NewAttempt(scopeA)
		if _, err := e.Invoke(e.Replica, a, body("x"), &gatewaytest.Sink{}); err != nil {
			t.Fatal(err)
		}
		e.Rotate(scopeA, sec.ID(), e.Payload(bearerV2, e.Provider.Origin()))
		other := e.NewAttempt(scopeA) // never bound: holds no pin
		src := e.VersionSource()
		cases := []struct {
			name string
			req  secrets.VersionRequest
		}{
			{"newer version than the pin", secrets.VersionRequest{Scope: scopeA, RunID: a.RunID, SecretName: gatewaytest.SecretName, Version: 2}},
			{"version that does not exist", secrets.VersionRequest{Scope: scopeA, RunID: a.RunID, SecretName: gatewaytest.SecretName, Version: 99}},
			{"run that holds no pin", secrets.VersionRequest{Scope: scopeA, RunID: other.RunID, SecretName: gatewaytest.SecretName, Version: 1}},
			{"another project's scope", secrets.VersionRequest{Scope: gatewayForeignScope(), RunID: a.RunID, SecretName: gatewaytest.SecretName, Version: 1}},
		}
		for _, c := range cases {
			p, err := src.Open(context.Background(), c.req)
			if err == nil {
				p.Discard()
				t.Fatalf("%s: a value was opened", c.name)
			}
			if !errors.Is(err, secrets.ErrNotFound) && !errors.Is(err, secrets.ErrNotPinned) {
				t.Fatalf("%s: %v, want not found or not pinned", c.name, err)
			}
		}
	})

	t.Run("audit finalization failure or uncertainty releases no credential and sends nothing", func(t *testing.T) {
		for _, c := range []struct {
			name      string
			err       error
			afterReal bool // the real audit commit lands before the failure is reported
		}{
			{"audit write fails", secrets.ErrUnavailable, false},
			{"audit commit outcome unknown", secrets.ErrFinalizationUncertain, false},
			{"audit committed but acknowledgement lost", secrets.ErrFinalizationUncertain, true},
		} {
			t.Run(c.name, func(t *testing.T) {
				e := newWorld(t)
				logs := &syncBuffer{}
				failing := &finalizeFailing{inner: e.VersionSource(), err: c.err, afterReal: c.afterReal}
				creds, err := adapters.NewCredentials(failing, e.X.Secrets)
				if err != nil {
					t.Fatal(err)
				}
				r := e.NewReplica(gatewaytest.ReplicaOptions{Credentials: creds, Timeouts: quietTimeouts(),
					Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
				a := e.NewAttempt(scopeA)
				sink := &gatewaytest.Sink{}
				res, err := e.Invoke(r, a, body("x"), sink)
				if !errors.Is(err, gateway.ErrUnavailable) {
					t.Fatalf("err %v, want ErrUnavailable", err)
				}
				if strings.Contains(err.Error(), "CAUSE-SENTINEL") || strings.Contains(err.Error(), gatewaytest.Bearer) {
					t.Fatalf("the error leaks: %v", err)
				}
				if n := len(e.Provider.Calls()); n != 0 {
					t.Fatalf("provider calls %d: a credential was used without a finalized audit", n)
				}
				if got := failing.valueReads(); got != 0 {
					t.Fatalf("the plaintext was read %d times before a successful finalization", got)
				}
				if sink.Started() {
					t.Fatal("the workload stream started")
				}
				if st := reqState(t, e, a.RunID, 1); st != [2]string{"failed", "credential_unavailable"} {
					t.Fatalf("request state %v", st)
				}
				if res.Usage == nil || res.Usage.Charged != 0 {
					t.Fatalf("usage %+v: an unsent request is free of charge", res.Usage)
				}
				absent(t, "diagnostics", logs.String())
				absent(t, "stored state", e.Dump(scopeA.TenantID))
				// The slot is free and no call was repeated: a later request succeeds
				// once finalization works again.
				failing.err = nil
				if _, err := e.Invoke(r, a, body("y"), &gatewaytest.Sink{}); err != nil {
					t.Fatalf("after the audit path recovered: %v", err)
				}
				if n := len(e.Provider.Calls()); n != 1 {
					t.Fatalf("provider calls %d, want exactly the one explicit retry", n)
				}
			})
		}
	})
}

// contextAuthority is the attempt authority of the secrets resolver over the
// attempt's real, immutable context: it grants exactly the bindings the step
// declared.
type contextAuthority struct {
	e *gatewaytest.Env
	a gatewaytest.Attempt
}

func (c contextAuthority) Grant(_ context.Context, attempt domain.AttemptID) (secrets.AttemptGrant, error) {
	if attempt != c.a.ID {
		return secrets.AttemptGrant{}, secrets.ErrAuthorityMissing
	}
	_, doc := c.e.AttemptContext(c.a)
	return secrets.AttemptGrant{Scope: c.a.Scope, RunID: c.a.RunID, AttemptID: c.a.ID, Bindings: doc.Secrets}, nil
}

func gatewayForeignScope() domain.Scope { return executiontest.ScopeB }

// finalizeFailing wraps the real version source so the audit finalization fails
// or reports an uncertain outcome. It counts plaintext reads: the gateway must
// not read the value of a version whose finalization did not succeed.
type finalizeFailing struct {
	inner     secrets.VersionSource
	err       error
	afterReal bool
	reads     atomic.Int64
}

func (f *finalizeFailing) valueReads() int { return int(f.reads.Load()) }

func (f *finalizeFailing) Open(ctx context.Context, req secrets.VersionRequest) (secrets.PendingValue, error) {
	p, err := f.inner.Open(ctx, req)
	if err != nil {
		return nil, err
	}
	return &failingPending{PendingValue: p, f: f}, nil
}

type failingPending struct {
	secrets.PendingValue
	f *finalizeFailing
}

func (p *failingPending) Value() []byte {
	p.f.reads.Add(1)
	return p.PendingValue.Value()
}

func (p *failingPending) Finalize(ctx context.Context, info secrets.FinalizeInfo) error {
	if p.f.err == nil {
		return p.PendingValue.Finalize(ctx, info)
	}
	if p.f.afterReal {
		_ = p.PendingValue.Finalize(ctx, info)
	}
	return fmt.Errorf("%w: %w", p.f.err, gatewaytest.ErrInjected)
}
