package deliverycleanup_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/deliverycleanup"
	"github.com/optimiware/kodama/internal/domain"
)

// fakeDelivery is a record double at delivery's service boundary.
type fakeDelivery struct {
	deps    []delivery.Deployment
	reqErr  error
	reqSeen []delivery.Caller
}

func (f *fakeDelivery) ListDeployments(_ context.Context, c delivery.Caller, _ domain.Scope, _ delivery.DeploymentFilter, _ delivery.DeploymentID, _ int) ([]delivery.Deployment, error) {
	f.reqSeen = append(f.reqSeen, c)
	return f.deps, nil
}

func (f *fakeDelivery) GetDeployment(_ context.Context, _ delivery.Caller, _ domain.Scope, id delivery.DeploymentID) (delivery.Deployment, error) {
	for _, d := range f.deps {
		if d.ID() == id {
			return d, nil
		}
	}
	return delivery.Deployment{}, delivery.Fail(delivery.ErrNotFound, nil, "not found")
}

func (f *fakeDelivery) RequestCleanup(_ context.Context, c delivery.Caller, _ domain.Scope, _ delivery.DeploymentID) (delivery.Receipt, error) {
	f.reqSeen = append(f.reqSeen, c)
	return delivery.Receipt{}, f.reqErr
}

func deployment(t *testing.T, key string, requested bool) delivery.Deployment {
	t.Helper()
	now := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	exp := now.Add(time.Hour)
	d, err := delivery.NewDeployment(delivery.Registration{
		Scope: scopeA, Environment: "env_1", ConfigDigest: runtimev1.Digest("sha256:" + strings.Repeat("a", 64)), Namespace: "preview",
		Source:   delivery.Source{RepositoryURL: "https://git.example.invalid/r.git", Commit: strings.Repeat("1", 40)},
		Image:    delivery.Image{ArtifactID: "art_1", Digest: runtimev1.Digest("sha256:" + strings.Repeat("b", 64))},
		Producer: delivery.Producer{Attempt: "att_build", Run: "run_build", Step: "build", ToolImage: "registry.example.invalid/tools@sha256:" + strings.Repeat("c", 64)},
		Run:      "run_deploy", Step: "deploy", ActionKey: key, Attempt: "att_deploy", ExpiresAt: &exp,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if requested {
		objs := []delivery.ObjectRef{{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "preview", Name: "web"}}
		if _, err := d.BeginApply(objs, now); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Settle(delivery.Settlement{Reason: "apply_failed"}, now); err != nil {
			t.Fatal(err)
		}
		if _, err := d.RequestCleanup(now); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func TestDeliveryAdaptersMapTheRecord(t *testing.T) {
	ctx := context.Background()
	if _, err := deliverycleanup.NewRecords(nil); err == nil {
		t.Fatal("a nil delivery service")
	}
	pending, requested := deployment(t, "act:0123456789abcdef", false), deployment(t, "act:fedcba9876543210", true)
	f := &fakeDelivery{deps: []delivery.Deployment{pending, requested}}
	recs, err := deliverycleanup.NewRecords(f)
	if err != nil {
		t.Fatal(err)
	}

	// Every undeleted record is scanned, requested or not; a full page continues.
	page, next, err := recs.Scan(ctx, scopeA, "", 2)
	if err != nil || len(page) != 2 || page[0].ExpiresAt == nil || page[0].RequestedAt != nil || page[1].RequestedAt == nil ||
		page[1].State != deliverycleanup.RecordRequested || next != string(requested.ID()) {
		t.Fatalf("scan %+v %q %v", page, next, err)
	}
	if _, next, _ = recs.Scan(ctx, scopeA, "", 5); next != "" {
		t.Fatalf("a short page continued: %q", next)
	}
	// The scheduler acts as the system, never as a human.
	for _, c := range f.reqSeen {
		if c.Actor.Type != delivery.ActorSystem {
			t.Fatalf("caller %+v", c)
		}
	}
	if got, err := recs.Get(ctx, scopeA, string(pending.ID())); err != nil || got.Deployment != string(pending.ID()) {
		t.Fatalf("get %+v %v", got, err)
	}
	if _, err := recs.Get(ctx, scopeA, "dep_missing"); !errors.Is(err, deliverycleanup.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	for in, want := range map[error]error{
		delivery.Fail(delivery.ErrInvalidTransition, nil, "x"): deliverycleanup.ErrNotSettled,
		delivery.Fail(delivery.ErrDenied, nil, "x"):            deliverycleanup.ErrNotFound,
	} {
		f.reqErr = in
		if err := recs.RequestCleanup(ctx, scopeA, "dep_1"); !errors.Is(err, want) {
			t.Fatalf("%v maps to %v, want %v", in, err, want)
		}
	}
	outage := delivery.Fail(delivery.ErrUnavailable, nil, "down")
	f.reqErr = outage
	if err := recs.RequestCleanup(ctx, scopeA, "dep_1"); !errors.Is(err, delivery.ErrUnavailable) {
		t.Fatalf("an outage was classified: %v", err)
	}
}

type bindingReader struct {
	dep   string
	found bool
	err   error
}

func (b bindingReader) Binding(context.Context, domain.Scope, domain.RunID) (string, bool, error) {
	return b.dep, b.found, b.err
}

func TestDeliveryBindingsNeverInvent(t *testing.T) {
	ctx := context.Background()
	if _, err := deliverycleanup.NewBindings(nil); err == nil {
		t.Fatal("a nil reader")
	}
	b, _ := deliverycleanup.NewBindings(bindingReader{dep: "dep_1", found: true})
	if id, ok, err := b.Binding(ctx, scopeA, "run_1"); err != nil || !ok || id != "dep_1" {
		t.Fatalf("%q %v %v", id, ok, err)
	}
	b, _ = deliverycleanup.NewBindings(bindingReader{})
	if id, ok, err := b.Binding(ctx, scopeA, "run_1"); err != nil || ok || id != "" {
		t.Fatalf("%q %v %v", id, ok, err)
	}
	b, _ = deliverycleanup.NewBindings(bindingReader{dep: "dep_1", found: true, err: errors.New("down")})
	if id, ok, err := b.Binding(ctx, scopeA, "run_1"); err == nil || ok || id != "" {
		t.Fatalf("an outage produced a binding: %q %v %v", id, ok, err)
	}
}
