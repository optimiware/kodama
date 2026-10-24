// Package integration_test holds the delivery module's component tests over
// real PostgreSQL: the real delivery store and migrations, the real execution,
// secrets, artifact and runtime-authority adapters, and a bounded HTTP OCI
// registry fixture. They prove the record boundary only: no NATS, S3, Git or
// Kubernetes is used, and they do not qualify production wiring (T-072), the real
// run/secret-pin transaction (T-077) or the end-to-end path (T-067).
//
// Resource prefix t035.
package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/delivery/adapters"
	"github.com/optimiware/kodama/internal/delivery/deliverytest"
	"github.com/optimiware/kodama/internal/secrets"
)

var scopeA = deliverytest.ScopeA

// stack is one environment with a kubeconfig secret, a preview environment and
// a started deploy attempt.
type stack struct {
	*deliverytest.Env
	D    *deliverytest.Deploy
	Req  delivery.RegistrationRequest
	Kube secrets.Secret
}

func newStack(t *testing.T, o deliverytest.Options, f deliverytest.FlowOptions) *stack {
	t.Helper()
	e := deliverytest.New(t, o)
	kube := e.Kubeconfig(scopeA, "kubeconfig", "KUBE-SENTINEL")
	env := e.Environment(scopeA, "preview", "kubeconfig")
	d := e.NewDeploy(scopeA, env, f)
	return &stack{Env: e, D: d, Req: delivery.RegistrationRequest{Source: d.Source, Image: delivery.Image{ArtifactID: d.Image, Digest: d.ImageDigest}}, Kube: kube}
}

func (s *stack) register(a *deliverytest.Attempt, req delivery.RegistrationRequest) (delivery.Receipt, error) {
	return s.Svc.RegisterRuntime(context.Background(), a.ID(), a.Token, req)
}

func (s *stack) mustRegister(t *testing.T) delivery.Receipt {
	t.Helper()
	rc, err := s.register(s.D.Deploy, s.Req)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	return rc
}

func (s *stack) count(sql string, args ...any) int { return s.X.Count(sql, args...) }

func (s *stack) deployments() int {
	return s.count(`SELECT count(*) FROM deployments WHERE tenant_id = $1`, string(scopeA.TenantID))
}

func (s *stack) history(id delivery.DeploymentID) int {
	return s.count(`SELECT count(*) FROM deployment_history WHERE tenant_id = $1 AND deployment_id = $2`, string(scopeA.TenantID), string(id))
}

func (s *stack) events() int {
	return s.X.CountTenant(scopeA.TenantID, `SELECT count(*) FROM outbox WHERE event_type = 'deployment.state_changed'`)
}

func (s *stack) audits(action string) int {
	return s.count(`SELECT count(*) FROM audit WHERE tenant_id = $1 AND action = $2`, string(scopeA.TenantID), action)
}

func is(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

func objects(ns string, names ...string) []delivery.ObjectRef {
	out := make([]delivery.ObjectRef, 0, len(names))
	for _, n := range names {
		out = append(out, delivery.ObjectRef{APIVersion: "apps/v1", Kind: "Deployment", Namespace: ns, Name: n})
	}
	return out
}

// observed marks the objects ready with deterministic UIDs.
func observed(objs []delivery.ObjectRef) []delivery.ObjectRef {
	out := append([]delivery.ObjectRef(nil), objs...)
	for i := range out {
		out[i].UID = uid(i)
		out[i].Ready = true
	}
	return out
}

func uid(i int) string {
	const hex = "0123456789abcdef"
	b := []byte("00000000-0000-4000-8000-000000000000")
	b[35] = hex[i%16]
	return string(b)
}

// shimOf exposes the fixture's artifact owner lookup for direct pin calls.
func shimOf(s *stack) adapters.ArtifactLookup { return s.ArtifactLookup() }
