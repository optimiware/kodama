// Behavior tests for the token.Deliverer bridge.
//
// The bridge resolves the committed delivery target to one current runtime
// effect identity and asks the runtime to replace only the mounted credential.
// It validates before any effect, propagates cancellation, maps failures to the
// delivery vocabulary and never renders the token.
package bridges_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/app/bridges"
	"github.com/optimiware/kodama/internal/shell/token"
)

const deliveryToken = "kodex-exec-1.super-secret-token"

func validDelivery() token.Delivery {
	return token.Delivery{
		Scope:       testScope(),
		PrincipalID: id.PrincipalID("exec-1"),
		ExecutionID: id.ExecutionID("exec-1"),
		Kind:        token.MutationRotate,
		Token:       deliveryToken,
		Target:      "exec-1",
		Refresh:     true,
	}
}

func testIdentity() engine.EffectIdentity {
	return engine.EffectIdentity{
		ExecutionID:    "exec-1",
		ActionID:       "action-1",
		WorkloadID:     "workload-1",
		IdempotencyKey: "exec-1/action-1",
	}
}

func TestDelivererRefreshesResolvedExecutionCredential(t *testing.T) {
	runtime := &fakeCredentialRuntime{evidence: engine.CredentialRefresh{Delivered: true}}
	resolver := &fakeIdentityResolver{identity: testIdentity()}
	deliverer, err := bridges.NewDeliverer(runtime, resolver)
	if err != nil {
		t.Fatalf("NewDeliverer: %v", err)
	}
	if err := deliverer.Deliver(context.Background(), validDelivery()); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if resolver.executionID != "exec-1" {
		t.Fatalf("resolved execution = %q; want exec-1", resolver.executionID)
	}
	if len(runtime.requests) != 1 {
		t.Fatalf("runtime requests = %d; want 1", len(runtime.requests))
	}
	request := runtime.requests[0]
	if request.Identity != testIdentity() {
		t.Fatalf("runtime identity = %+v; want %+v", request.Identity, testIdentity())
	}
	if request.Credential != deliveryToken {
		t.Fatalf("runtime credential = %q; want the delivered token", request.Credential)
	}
}

func TestDelivererRefusesInvalidDeliveryBeforeEffect(t *testing.T) {
	runtime := &fakeCredentialRuntime{evidence: engine.CredentialRefresh{Delivered: true}}
	resolver := &fakeIdentityResolver{identity: testIdentity()}
	deliverer, err := bridges.NewDeliverer(runtime, resolver)
	if err != nil {
		t.Fatalf("NewDeliverer: %v", err)
	}
	cases := []token.Delivery{
		{},
		func() token.Delivery { d := validDelivery(); d.Token = ""; return d }(),
		func() token.Delivery { d := validDelivery(); d.Target = ""; return d }(),
		func() token.Delivery { d := validDelivery(); d.Kind = "unknown"; return d }(),
		func() token.Delivery { d := validDelivery(); d.Scope = testScope(); d.Scope.TenantID = ""; return d }(),
	}
	for _, delivery := range cases {
		err := deliverer.Deliver(context.Background(), delivery)
		if err == nil {
			t.Fatalf("Deliver(%+v) = nil; want refusal", delivery)
		}
	}
	if len(runtime.requests) != 0 || resolver.calls != 0 {
		t.Fatalf("invalid deliveries reached runtime (%d) or resolver (%d)", len(runtime.requests), resolver.calls)
	}
}

func TestDelivererMapsIdentityAndRuntimeFailures(t *testing.T) {
	runtime := &fakeCredentialRuntime{evidence: engine.CredentialRefresh{Delivered: true}}
	resolver := &fakeIdentityResolver{err: errors.New("execution not found")}
	deliverer, err := bridges.NewDeliverer(runtime, resolver)
	if err != nil {
		t.Fatalf("NewDeliverer: %v", err)
	}
	err = deliverer.Deliver(context.Background(), validDelivery())
	requireFaultClass(t, err, fault.DependencyUnavailable)
	if strings.Contains(err.Error(), deliveryToken) {
		t.Fatalf("identity failure renders the token: %v", err)
	}

	resolver.err = nil
	resolver.identity = testIdentity()
	runtime.err = errors.New("runtime unavailable")
	err = deliverer.Deliver(context.Background(), validDelivery())
	requireFaultClass(t, err, fault.UncertainExternalEffect)
	if !errors.Is(err, runtime.err) {
		t.Fatalf("runtime error is not reachable: %v", err)
	}
	if strings.Contains(err.Error(), deliveryToken) {
		t.Fatalf("runtime failure renders the token: %v", err)
	}

	runtime.err = nil
	runtime.evidence = engine.CredentialRefresh{Delivered: false, Detail: "not confirmed"}
	err = deliverer.Deliver(context.Background(), validDelivery())
	requireFaultClass(t, err, fault.UncertainExternalEffect)
}

func TestDelivererRefusesServiceDeliveryAndPropagatesCancellation(t *testing.T) {
	runtime := &fakeCredentialRuntime{evidence: engine.CredentialRefresh{Delivered: true}}
	resolver := &fakeIdentityResolver{identity: testIdentity()}
	deliverer, err := bridges.NewDeliverer(runtime, resolver)
	if err != nil {
		t.Fatalf("NewDeliverer: %v", err)
	}
	serviceDelivery := token.Delivery{
		Scope:       testScope(),
		PrincipalID: id.PrincipalID("svc-1"),
		ServiceID:   "svc-1",
		Kind:        token.MutationIssue,
		Token:       deliveryToken,
		Target:      "svc-1",
	}
	requireFaultClass(t, deliverer.Deliver(context.Background(), serviceDelivery), fault.UnsupportedCapability)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = deliverer.Deliver(ctx, validDelivery())
	requireFaultClass(t, err, fault.DeadlineExpired)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error does not wrap context.Canceled: %v", err)
	}
	if len(runtime.requests) != 0 {
		t.Fatalf("canceled delivery reached runtime %d times", len(runtime.requests))
	}
}

func TestDelivererRefusesNilDependencies(t *testing.T) {
	if _, err := bridges.NewDeliverer(nil, &fakeIdentityResolver{}); err == nil {
		t.Fatal("NewDeliverer accepted a nil runtime")
	}
	if _, err := bridges.NewDeliverer(&fakeCredentialRuntime{}, nil); err == nil {
		t.Fatal("NewDeliverer accepted a nil identity resolver")
	}
}

type fakeCredentialRuntime struct {
	requests []engine.CredentialRequest
	evidence engine.CredentialRefresh
	err      error
}

func (runtime *fakeCredentialRuntime) RefreshCredential(_ context.Context, request engine.CredentialRequest) (engine.CredentialRefresh, error) {
	runtime.requests = append(runtime.requests, request)
	return runtime.evidence, runtime.err
}

type fakeIdentityResolver struct {
	identity    engine.EffectIdentity
	err         error
	calls       int
	executionID id.ExecutionID
}

func (resolver *fakeIdentityResolver) ResolveExecutionIdentity(_ context.Context, executionID id.ExecutionID) (engine.EffectIdentity, error) {
	resolver.calls++
	resolver.executionID = executionID
	return resolver.identity, resolver.err
}
