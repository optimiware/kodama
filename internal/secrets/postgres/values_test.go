package postgres_test

import (
	"context"
	"encoding/base64"
	"sync"
	"testing"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/secrets"
	secretspg "github.com/optimiware/kodama/internal/secrets/postgres"
)

type runtimeBinding = runtimev1.SecretBinding

func strValue(s string) runtimev1.Value { return runtimev1.StringValue(s) }

// attemptAuthority is the injected execution-side authority fixture.
type attemptAuthority struct {
	mu     sync.Mutex
	grants map[domain.AttemptID]secrets.AttemptGrant
	errs   map[domain.AttemptID]error
}

func newAuthority() *attemptAuthority {
	return &attemptAuthority{grants: map[domain.AttemptID]secrets.AttemptGrant{}, errs: map[domain.AttemptID]error{}}
}

func (a *attemptAuthority) Grant(_ context.Context, id domain.AttemptID) (secrets.AttemptGrant, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.errs[id]; err != nil {
		return secrets.AttemptGrant{}, err
	}
	g, ok := a.grants[id]
	if !ok {
		return secrets.AttemptGrant{}, secrets.ErrAuthorityMissing
	}
	return g, nil
}

func (a *attemptAuthority) set(g secrets.AttemptGrant) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.grants[g.AttemptID] = g
}

func (a *attemptAuthority) fail(id domain.AttemptID, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.errs[id] = err
}

func binding(name string, version int64) runtimev1.SecretBinding {
	return runtimev1.SecretBinding{Ref: "secret://project/" + name, Version: version, Name: name, Delivery: runtimev1.DeliveryFile}
}

func (e *env) resolver(auth secrets.AttemptAuthority) *secrets.Resolver {
	e.t.Helper()
	src, err := secretspg.NewVersionSource(e.store, e.wrapper, installation)
	if err != nil {
		e.t.Fatal(err)
	}
	r, err := secrets.NewResolver(auth, src)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func decode(t testing.TB, resp runtimev1.SecretResolveResponse) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(resp.ValueBase64)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func domainAttempt(s string) domain.AttemptID { return domain.AttemptID(s) }

type eventsv1Envelope = eventsv1.Envelope
