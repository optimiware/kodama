// Behavior tests for the fail-closed observation sink and the service
// invalidation bridge.
package bridges_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/app/bridges"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/integration/nats"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

func TestObservationSinkRefusesEveryObservation(t *testing.T) {
	sink := bridges.NewObservationSink()
	observations := []nats.Observation{
		{Scope: testScope(), Source: event.Source{Kind: event.SourceExecution, PrincipalID: "exec-1"}, Type: event.TypeCustomObservation, Data: []byte("one")},
		{Scope: tenancy.Scope{TenantID: "tenant-b"}, Source: event.Source{Kind: event.SourceService, PrincipalID: "svc-1"}, Type: event.TypeCustomObservation},
	}
	for _, observation := range observations {
		err := sink.Observe(context.Background(), observation)
		requireFaultClass(t, err, fault.UnsupportedCapability)
	}
}

func TestObservationSinkPropagatesCancellation(t *testing.T) {
	sink := bridges.NewObservationSink()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := sink.Observe(ctx, nats.Observation{Scope: testScope(), Type: event.TypeCustomObservation})
	requireFaultClass(t, err, fault.DeadlineExpired)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error does not wrap context.Canceled: %v", err)
	}
}

func TestInvalidatorMapsServiceDisablementToBrokerInvalidation(t *testing.T) {
	port := &fakeInvalidationPort{}
	invalidator, err := bridges.NewInvalidator(port)
	if err != nil {
		t.Fatalf("NewInvalidator: %v", err)
	}
	at := time.Unix(1_700_000_000, 0).UTC()
	command := service.ServiceInvalidation{Scope: testScope(), ServiceID: service.ServiceID("svc-1"), At: at}
	if err := invalidator.InvalidateService(context.Background(), command); err != nil {
		t.Fatalf("InvalidateService: %v", err)
	}
	if len(port.invalidations) != 1 {
		t.Fatalf("broker invalidations = %d; want 1", len(port.invalidations))
	}
	got := port.invalidations[0]
	if got.Scope != command.Scope || got.ServiceID != command.ServiceID || got.At != at {
		t.Fatalf("broker invalidation = %+v; want scope/service/time from command", got)
	}
	if got.PrincipalID != id.PrincipalID(command.ServiceID) {
		t.Fatalf("broker invalidation principal = %q; want service id", got.PrincipalID)
	}
	if got.Reason != token.InvalidationDisabled {
		t.Fatalf("broker invalidation reason = %q; want disabled", got.Reason)
	}
}

func TestInvalidatorRefusesInvalidCommandWithoutPublishing(t *testing.T) {
	port := &fakeInvalidationPort{}
	invalidator, err := bridges.NewInvalidator(port)
	if err != nil {
		t.Fatalf("NewInvalidator: %v", err)
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	cases := []service.ServiceInvalidation{
		{Scope: tenancy.Scope{}, ServiceID: "svc-1", At: now},
		{Scope: testScope(), ServiceID: "", At: now},
		{Scope: testScope(), ServiceID: "bad service", At: now},
		{Scope: testScope(), ServiceID: "svc-1", At: time.Time{}},
	}
	for _, command := range cases {
		requireFaultClass(t, invalidator.InvalidateService(context.Background(), command), fault.InvalidInput)
	}
	if len(port.invalidations) != 0 {
		t.Fatalf("broker invalidations = %d; want 0", len(port.invalidations))
	}
}

func TestInvalidatorPropagatesPortFailureAndCancellation(t *testing.T) {
	sentinel := errors.New("broker down")
	port := &fakeInvalidationPort{err: sentinel}
	invalidator, err := bridges.NewInvalidator(port)
	if err != nil {
		t.Fatalf("NewInvalidator: %v", err)
	}
	command := service.ServiceInvalidation{Scope: testScope(), ServiceID: "svc-1", At: time.Unix(1_700_000_000, 0).UTC()}
	if err := invalidator.InvalidateService(context.Background(), command); !errors.Is(err, sentinel) {
		t.Fatalf("InvalidateService error = %v; want sentinel", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	port.err = nil
	err = invalidator.InvalidateService(ctx, command)
	requireFaultClass(t, err, fault.DeadlineExpired)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error does not wrap context.Canceled: %v", err)
	}
}

func TestInvalidatorRefusesNilPort(t *testing.T) {
	if _, err := bridges.NewInvalidator(nil); err == nil {
		t.Fatal("NewInvalidator accepted a nil port")
	}
}

type fakeInvalidationPort struct {
	invalidations []token.Invalidation
	err           error
}

func (port *fakeInvalidationPort) Invalidate(_ context.Context, invalidation token.Invalidation) error {
	port.invalidations = append(port.invalidations, invalidation)
	return port.err
}
