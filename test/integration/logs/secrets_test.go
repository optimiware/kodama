package logs_test

// Delivered secrets: a real secret is sealed and pinned for the run, resolved
// through the real secrets resolver behind a runtime route authorized by the
// attempt's session, and delivered by the real entrypoint as a file and as an
// environment value. The test-local pieces are the route adapter (the secrets
// owner registers no route; the runtime authenticator does) and the attempt
// authority, which reads the same durable grant the artifact routes use.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/runtimeauth"
	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/internal/secrets/keyfile"
	secretspg "github.com/optimiware/kodama/internal/secrets/postgres"
	secretsruntime "github.com/optimiware/kodama/internal/secrets/runtime"
)

const installation = "inst-t064"

type vault struct {
	svc      *secrets.Service
	resolver *secrets.Resolver
	adapter  *secretsruntime.Adapter
}

type sessionKey struct{}

type sessionCtx struct {
	tenant domain.TenantID
	id     runtimeauth.SessionID
	gen    int64
}

// grantAuthority is the secrets module's attempt authority over the runtime
// grant: current session, current signing generation, active attempt, and the
// bindings of the attempt's pinned context.
type grantAuthority struct{ s *stack }

func (g grantAuthority) Grant(ctx context.Context, attempt domain.AttemptID) (secrets.AttemptGrant, error) {
	sc, ok := ctx.Value(sessionKey{}).(sessionCtx)
	if !ok {
		return secrets.AttemptGrant{}, secrets.ErrAuthorityMissing
	}
	var out secrets.AttemptGrant
	err := g.s.grants.Do(ctx, sc.tenant, func(ctx context.Context, tx runtimeauth.GrantTx) error {
		gr, _, err := runtimeauth.CheckSession(ctx, tx, runtimeauth.SessionCheck{
			Route: attempt, Session: sc.id, TokenGeneration: sc.gen, CurrentGeneration: g.s.ring.Generation(),
			Action: runtimeauth.ActionSecretResolve, Now: g.s.x.Clock.Now(),
		})
		if err != nil {
			return err
		}
		pinned, err := runtimev1.DecodeRequest[runtimev1.Context](gr.Context)
		if err != nil {
			return err
		}
		out = secrets.AttemptGrant{Scope: gr.Scope, RunID: gr.RunID, AttemptID: gr.AttemptID, Bindings: pinned.Secrets}
		return nil
	})
	switch {
	case err == nil:
		return out, nil
	case isErr(err, runtimeauth.ErrAttemptRevoked):
		return secrets.AttemptGrant{}, secrets.ErrAttemptInactive
	case isErr(err, runtimeauth.ErrInvalidCredential), isErr(err, runtimeauth.ErrNotFound), isErr(err, runtimeauth.ErrForbidden):
		return secrets.AttemptGrant{}, secrets.ErrAuthorityMissing
	}
	return secrets.AttemptGrant{}, err
}

func (s *stack) newVault() *vault {
	t := s.t
	dir := t.TempDir()
	key := make([]byte, keyfile.KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "master-key-v1")
	must(t, os.WriteFile(p, key, 0o600))
	wrapper, err := keyfile.New(keyfile.Config{Development: true, Current: "v1", Files: map[string]string{"v1": p}})
	must(t, err)
	store, err := secretspg.NewStore(secretspg.Config{DB: s.x.Runtime, Events: secretspg.EventsFrom(s.x.Events), IDs: secrets.RandomIDs{}, Clock: s.x.Clock.Now})
	must(t, err)
	v := &vault{}
	v.svc, err = secrets.NewService(secrets.ServiceConfig{InstallationID: installation, Store: store, Wrapper: wrapper, IDs: secrets.RandomIDs{}})
	must(t, err)
	src, err := secretspg.NewVersionSource(store, wrapper, installation)
	must(t, err)
	v.resolver, err = secrets.NewResolver(grantAuthority{s}, src)
	must(t, err)
	v.adapter, err = secretsruntime.NewAdapter(v.resolver)
	must(t, err)
	return v
}

// create seals a secret in the scope.
func (v *vault) create(t testing.TB, s *stack, scope domain.Scope, name, value string) {
	t.Helper()
	ctx := context.Background()
	m := secrets.Mutation{Actor: secrets.Actor{Type: secrets.ActorUser, ID: executiontest.Engineer}, RequestID: "req_t064", Now: s.x.Clock.Now()}
	p, err := v.svc.PrepareSecret(ctx, m, scope, name, secrets.KindGeneric, []byte(value))
	must(t, err)
	must(t, v.svc.Atomically(ctx, scope.TenantID, func(ctx context.Context, w secrets.Writer) error {
		_, err := v.svc.CreateSecret(ctx, w, m, p)
		return err
	}))
}

// route serves POST /runtime/v1/attempts/{id}/secrets/resolve: the bearer is
// verified under the signing ring, then the resolver re-checks the grant.
func (s *stack) secretRoute(v *vault) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt, _ := classify(r.URL.Path)
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		claims, err := s.verifier.Verify(r.Context(), tok)
		if err != nil {
			contractFault(w, http.StatusUnauthorized)
			return
		}
		body := make([]byte, 0, 1024)
		buf := make([]byte, 1024)
		for {
			n, rerr := r.Body.Read(buf)
			body = append(body, buf[:n]...)
			if rerr != nil || len(body) > 16<<10 {
				break
			}
		}
		req, err := runtimev1.DecodeRequest[runtimev1.SecretResolveRequest](body)
		if err != nil {
			contractFault(w, http.StatusBadRequest)
			return
		}
		ctx := context.WithValue(r.Context(), sessionKey{}, sessionCtx{tenant: claims.Tenant, id: claims.Session, gen: claims.Generation})
		resp, err := v.adapter.Resolve(ctx, attempt, req, "req_t064secret")
		if err != nil {
			status := http.StatusServiceUnavailable
			if ce, ok := err.(*runtimev1.ContractError); ok {
				if st, ok := runtimev1.Status(ce.Code); ok {
					status = st
				}
			}
			contractFault(w, status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(resp)
	})
}
