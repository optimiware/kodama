package integration_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/delivery/adapters"
	"github.com/optimiware/kodama/internal/delivery/deliverytest"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/secrets"
)

var engineer = delivery.Actor{Type: delivery.ActorUser, ID: deliverytest.Engineer}

// Test_environment_snapshot_resolution proves the provider contract of the
// environment-pinning checkpoint with real delivery and secrets persistence:
// deterministic scoped snapshots with exact bindings, immutability across edits
// and rotation, and denial of new resolution for revoked or missing credentials.
func Test_environment_snapshot_resolution(t *testing.T) {
	ctx := context.Background()

	t.Run("snapshots are deterministic, scoped and carry exact bindings", func(t *testing.T) {
		e := deliverytest.New(t, deliverytest.Options{})
		kube := e.Kubeconfig(scopeA, "kubeconfig", "KUBE-SENTINEL")
		tok := e.Generic(scopeA, "registry-token", "TOKEN-SENTINEL")
		env := e.Environment(scopeA, "preview", "kubeconfig", delivery.Credential{Name: "REGISTRY_TOKEN", Ref: tok.Ref(), Delivery: runtimev1.DeliveryEnv})

		a, err := e.Svc.ResolveSnapshot(ctx, scopeA, engineer, env.ID())
		if err != nil {
			t.Fatal(err)
		}
		b, err := e.Svc.ResolveSnapshot(ctx, scopeA, engineer, env.ID())
		if err != nil {
			t.Fatal(err)
		}
		if a.Digest != b.Digest || a.Scope != scopeA || a.Environment != env.ID() {
			t.Fatalf("snapshots differ: %s vs %s", a.Digest, b.Digest)
		}
		want := []runtimev1.SecretBinding{
			{Ref: kube.Ref(), Version: 1, Name: "KUBECONFIG", Delivery: runtimev1.DeliveryFile},
			{Ref: tok.Ref(), Version: 1, Name: "REGISTRY_TOKEN", Delivery: runtimev1.DeliveryEnv},
		}
		if len(a.Secrets) != 2 || a.Secrets[0] != want[0] || a.Secrets[1] != want[1] {
			t.Fatalf("bindings: %+v", a.Secrets)
		}
		if n := e.X.Count(`SELECT count(*) FROM environment_snapshots WHERE tenant_id = $1`, string(scopeA.TenantID)); n != 1 {
			t.Fatalf("an identical resolution stored %d snapshots", n)
		}
		if err := a.Validate(); err != nil {
			t.Fatal(err)
		}
		// No plaintext, kubeconfig content or secret value is persisted.
		var dump string
		e.X.Scalar(&dump, `SELECT string_agg(config::text || secrets::text || kubeconfig, '') FROM environment_snapshots`)
		var envDump string
		e.X.Scalar(&envDump, `SELECT string_agg(kubeconfig::text || secrets::text || config::text, '') FROM environments`)
		if strings.Contains(dump+envDump, "SENTINEL") {
			t.Fatal("a credential value reached the delivery tables")
		}

		// The execution seam adapter returns the same pin and exact bindings.
		resolver, err := adapters.NewEnvironments(e.Svc)
		if err != nil {
			t.Fatal(err)
		}
		snap, err := resolver.ResolveEnvironment(ctx, execution.EnvironmentRequest{Scope: scopeA, Actor: execution.Actor{Type: execution.ActorUser, ID: deliverytest.Engineer}, EnvironmentID: env.ID()})
		if err != nil {
			t.Fatal(err)
		}
		if snap.Scope != scopeA || snap.Pin != a.Pin() || len(snap.Secrets) != 2 || snap.Secrets[0] != want[0] || snap.Secrets[1] != want[1] {
			t.Fatalf("execution snapshot: %+v", snap)
		}

		// A foreign tenant's engineer cannot resolve, and learns nothing.
		_, err = e.Svc.ResolveSnapshot(ctx, deliverytest.ScopeB, engineer, env.ID())
		is(t, err, delivery.ErrNotFound)
		_, err = e.Svc.ResolveSnapshot(ctx, scopeA, delivery.Actor{Type: delivery.ActorUser, ID: "u_stranger"}, env.ID())
		is(t, err, delivery.ErrNotFound)
		_, err = e.Svc.ResolveSnapshot(ctx, scopeA, delivery.Actor{Type: delivery.ActorUser, ID: deliverytest.Viewer}, env.ID())
		is(t, err, delivery.ErrDenied)
		_, err = e.Svc.ResolveSnapshot(ctx, scopeA, delivery.Actor{Type: delivery.ActorWorkload, ID: "att_1"}, env.ID())
		is(t, err, delivery.ErrDenied)
	})

	t.Run("edits and rotation never alter a retained snapshot or an admitted pin", func(t *testing.T) {
		e := deliverytest.New(t, deliverytest.Options{})
		kube := e.Kubeconfig(scopeA, "kubeconfig", "KUBE-SENTINEL")
		env := e.Environment(scopeA, "preview", "kubeconfig")
		d := e.NewDeploy(scopeA, env, deliverytest.FlowOptions{})
		pin := *d.Deploy.Context.Environment
		retained, err := e.Svc.RetainedSnapshot(ctx, scopeA, pin.ID, pin.ConfigDigest)
		if err != nil {
			t.Fatal(err)
		}
		if len(d.Deploy.Context.Secrets) != 1 || d.Deploy.Context.Secrets[0].Version != 1 {
			t.Fatalf("admitted bindings: %+v", d.Deploy.Context.Secrets)
		}

		cfg := map[string]string{"REPLICAS": "5"}
		ns := "other-namespace"
		if _, err := e.Svc.UpdateEnvironment(ctx, deliverytest.Caller(deliverytest.Engineer), scopeA, env.ID(), env.Version(), delivery.EnvironmentEdit{Config: &cfg, Namespace: &ns}); err != nil {
			t.Fatal(err)
		}
		e.X.RotateSecret(scopeA, kube.ID(), "ROTATED-SENTINEL")

		after, err := e.Svc.RetainedSnapshot(ctx, scopeA, pin.ID, pin.ConfigDigest)
		if err != nil {
			t.Fatal(err)
		}
		if after.Digest != retained.Digest || after.Namespace != deliverytest.TargetNamespace || after.Config["REPLICAS"] != "1" || after.Secrets[0].Version != 1 {
			t.Fatalf("a retained snapshot changed: %+v", after)
		}
		// New resolution selects the new immutable snapshot with the rotated version.
		next, err := e.Svc.ResolveSnapshot(ctx, scopeA, engineer, env.ID())
		if err != nil {
			t.Fatal(err)
		}
		if next.Digest == retained.Digest || next.Namespace != ns || next.Secrets[0].Version != 2 {
			t.Fatalf("new snapshot: %+v", next)
		}
		if n := e.X.Count(`SELECT count(*) FROM environment_snapshots WHERE tenant_id = $1`, string(scopeA.TenantID)); n != 2 {
			t.Fatalf("snapshots: %d", n)
		}
		// The database refuses to rewrite or delete a retained snapshot, even for the owner.
		if err := e.X.OwnerExecErr(`UPDATE environment_snapshots SET namespace = 'x' WHERE tenant_id = $1`, string(scopeA.TenantID)); err == nil {
			t.Fatal("a snapshot was updated")
		}
		if err := e.X.OwnerExecErr(`DELETE FROM environment_snapshots WHERE tenant_id = $1`, string(scopeA.TenantID)); err == nil {
			t.Fatal("a snapshot was deleted")
		}
		// Registration still uses the pinned snapshot, not the edited environment.
		req := delivery.RegistrationRequest{Source: d.Source, Image: delivery.Image{ArtifactID: d.Image, Digest: d.ImageDigest}}
		rc, err := e.Svc.RegisterRuntime(ctx, d.Deploy.ID(), d.Deploy.Token, req)
		if err != nil {
			t.Fatal(err)
		}
		if rc.Deployment.Record().ConfigDigest != pin.ConfigDigest || rc.Deployment.Record().Namespace != deliverytest.TargetNamespace {
			t.Fatalf("registration followed the edited environment: %+v", rc.Deployment.Record())
		}
	})

	t.Run("revoked, missing, foreign and mistyped credentials deny", func(t *testing.T) {
		e := deliverytest.New(t, deliverytest.Options{})
		kube := e.Kubeconfig(scopeA, "kubeconfig", "KUBE-SENTINEL")
		generic := e.Generic(scopeA, "not-a-kubeconfig", "X")
		e.Kubeconfig(deliverytest.ScopeB, "foreign", "FOREIGN-SENTINEL")
		env := e.Environment(scopeA, "preview", "kubeconfig")

		create := func(name, ref string, extra ...delivery.Credential) error {
			_, err := e.Svc.CreateEnvironment(ctx, deliverytest.Caller(deliverytest.Engineer), scopeA, delivery.EnvironmentInput{
				Name: name, Kind: delivery.KindPreview, Target: "kind", Namespace: "ns",
				Kubeconfig: delivery.Credential{Name: "KUBECONFIG", Ref: ref, Delivery: runtimev1.DeliveryFile}, Secrets: extra,
			})
			return err
		}
		is(t, create("missing", secrets.FormatSecretRef("nonexistent")), delivery.ErrCredentialUnavailable)
		is(t, create("foreign", secrets.FormatSecretRef("foreign")), delivery.ErrCredentialUnavailable)
		is(t, create("generic", generic.Ref()), delivery.ErrCredentialUnavailable)
		is(t, create("badref", "secret://tenant/kubeconfig"), delivery.ErrInvalid)
		is(t, create("badextra", kube.Ref(), delivery.Credential{Name: "X", Ref: secrets.FormatSecretRef("nonexistent"), Delivery: runtimev1.DeliveryEnv}), delivery.ErrCredentialUnavailable)
		is(t, create("alias", kube.Ref(), delivery.Credential{Name: "OTHER", Ref: kube.Ref(), Delivery: runtimev1.DeliveryEnv}), delivery.ErrInvalid)
		is(t, create("dup-name", kube.Ref(), delivery.Credential{Name: "KUBECONFIG", Ref: generic.Ref(), Delivery: runtimev1.DeliveryEnv}), delivery.ErrInvalid)
		if n := e.X.Count(`SELECT count(*) FROM environments WHERE tenant_id = $1`, string(scopeA.TenantID)); n != 1 {
			t.Fatalf("a refused environment was stored: %d", n)
		}

		if _, err := e.Svc.ResolveSnapshot(ctx, scopeA, engineer, env.ID()); err != nil {
			t.Fatal(err)
		}
		if err := e.X.RevokeSecret(scopeA, kube.ID(), 1); err != nil {
			t.Fatal(err)
		}
		_, err := e.Svc.ResolveSnapshot(ctx, scopeA, engineer, env.ID())
		is(t, err, delivery.ErrCredentialUnavailable)
		// Admission through the real provider refuses; it is not an outage.
		resolver, _ := adapters.NewEnvironments(e.Svc)
		_, err = resolver.ResolveEnvironment(ctx, execution.EnvironmentRequest{Scope: scopeA, Actor: execution.Actor{Type: execution.ActorUser, ID: deliverytest.Engineer}, EnvironmentID: env.ID()})
		if !errors.Is(err, execution.ErrAdmissionRefused) {
			t.Fatalf("revoked credential admitted: %v", err)
		}
		// A new version restores new resolution; the revoked version stays unusable.
		e.X.RotateSecret(scopeA, kube.ID(), "NEW-SENTINEL")
		snap, err := e.Svc.ResolveSnapshot(ctx, scopeA, engineer, env.ID())
		if err != nil || snap.Secrets[0].Version != 2 {
			t.Fatalf("rotation after revocation: %v %+v", err, snap.Secrets)
		}
	})

	t.Run("admission pins the delivered snapshot through the real provider", func(t *testing.T) {
		e := deliverytest.New(t, deliverytest.Options{})
		e.Kubeconfig(scopeA, "kubeconfig", "KUBE-SENTINEL")
		env := e.Environment(scopeA, "preview", "kubeconfig")
		d := e.NewDeploy(scopeA, env, deliverytest.FlowOptions{})
		if len(d.Run.Snapshot.Environments) != 1 || d.Run.Snapshot.Environments[0].Pin.ID != env.ID() {
			t.Fatalf("admission did not pin the environment: %+v", d.Run.Snapshot.Environments)
		}
		if n := e.X.Count(`SELECT count(*) FROM secret_pins WHERE consumer_id = $1 AND released_at IS NULL`, string(d.Run.ID)); n != 1 {
			t.Fatalf("the kubeconfig credential was not pinned with the run: %d", n)
		}
	})

	t.Run("environment edits are versioned and audited", func(t *testing.T) {
		e := deliverytest.New(t, deliverytest.Options{})
		e.Kubeconfig(scopeA, "kubeconfig", "KUBE-SENTINEL")
		env := e.Environment(scopeA, "preview", "kubeconfig")
		short := 2 * time.Hour
		upd, err := e.Svc.UpdateEnvironment(ctx, deliverytest.Caller(deliverytest.Engineer), scopeA, env.ID(), env.Version(), delivery.EnvironmentEdit{PreviewTTL: &short})
		if err != nil || upd.Version() != 2 {
			t.Fatalf("update: %v", err)
		}
		_, err = e.Svc.UpdateEnvironment(ctx, deliverytest.Caller(deliverytest.Engineer), scopeA, env.ID(), env.Version(), delivery.EnvironmentEdit{PreviewTTL: &short})
		is(t, err, delivery.ErrVersionConflict)
		// An edit that changes nothing leaves the version alone.
		same, err := e.Svc.UpdateEnvironment(ctx, deliverytest.Caller(deliverytest.Engineer), scopeA, env.ID(), upd.Version(), delivery.EnvironmentEdit{PreviewTTL: &short})
		if err != nil || same.Version() != 2 {
			t.Fatalf("no-op edit: %v v%d", err, same.Version())
		}
		if n := e.X.Count(`SELECT count(*) FROM audit WHERE tenant_id = $1 AND resource_type = 'environment'`, string(scopeA.TenantID)); n != 2 {
			t.Fatalf("environment audit entries: %d", n)
		}
		// The catalog has no environment event: only deployments emit events.
		if n := e.X.CountTenant(scopeA.TenantID, `SELECT count(*) FROM outbox WHERE event_type = 'deployment.state_changed'`); n != 0 {
			t.Fatalf("an environment change was published as %d deployment events", n)
		}
		_, err = e.Svc.CreateEnvironment(ctx, deliverytest.Caller(deliverytest.Engineer), scopeA, delivery.EnvironmentInput{
			Name: "preview", Kind: delivery.KindPreview, Target: "kind", Namespace: "ns2",
			Kubeconfig: delivery.Credential{Name: "KUBECONFIG", Ref: secrets.FormatSecretRef("kubeconfig"), Delivery: runtimev1.DeliveryFile},
		})
		is(t, err, delivery.ErrNameTaken)
		// Default preview TTL: configurable, 24 hours unless set.
		if env.Record().PreviewTTL != delivery.DefaultPreviewTTL || delivery.DefaultPreviewTTL != 24*time.Hour {
			t.Fatalf("default preview TTL %s", env.Record().PreviewTTL)
		}
		if _, err := e.Svc.GetEnvironment(ctx, deliverytest.Caller(deliverytest.Viewer), scopeA, env.ID()); err != nil {
			t.Fatal(err)
		}
	})
}
