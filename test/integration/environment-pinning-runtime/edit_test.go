package environmentpinningruntime_test

import (
	"context"
	"strings"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/test/integration/delivery/stack"
)

// Test_environment_edit_preserves_pin — environment_edit_preserves_pin
// (DQ-15/DQ-A11): editing the environment (namespace, configuration, credential
// reference) and rotating or revoking its credential after admission leaves the
// admitted run's persisted pin, its retained snapshot, its credential version
// pin, its dispatched context and every delivery callback unchanged; only a run
// admitted afterwards sees the new configuration.
func Test_environment_edit_preserves_pin(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, stackOptions())

	// A run admitted before any edit, with its build and deploy attempts ready.
	img := w.FixtureImage("app")
	flow := w.NewFlow(scopeA, w.env, stack.FlowOptions{Image: img})
	// The deploy step is claimed without a workload: the registration below is an
	// API-level call as that attempt (the scripts' own path is T-067's delivery suite).
	deploy := w.DirectAttempt(flow)
	admitted := pinOf(t, flow.Run)
	snap, err := w.Svc.RetainedSnapshot(ctx, scopeA, admitted.ID, admitted.ConfigDigest)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Namespace != namespace || snap.Secrets[0].Version != 1 {
		t.Fatalf("the admitted snapshot: %+v", snap)
	}
	if n := w.runPins(flow.Run.ID, 1); n != 1 {
		t.Fatalf("the admitted run has %d credential pins at version 1", n)
	}

	// ---- edits after admission ----
	// (1) The environment: another namespace and configuration.
	ns, cfg := "t067-edited", map[string]string{"REPLICAS": "9"}
	edited, err := w.Svc.UpdateEnvironment(ctx, stack.Caller(), scopeA, w.env.ID(), w.env.Version(), delivery.EnvironmentEdit{Namespace: &ns, Config: &cfg})
	if err != nil {
		t.Fatal(err)
	}
	// (2) The credential: a rotation to version 2, and another secret referenced.
	w.X.RotateSecret(scopeA, w.secret.ID(), kubeconfigV2)
	other := w.Kubeconfig(scopeA, "kubeconfig-other", "OTHER-CREDENTIAL-SENTINEL")
	ref := delivery.Credential{Name: delivery.DefaultKubeconfig, Ref: "secret://project/kubeconfig-other", Delivery: runtimev1.DeliveryFile}
	edited, err = w.Svc.UpdateEnvironment(ctx, stack.Caller(), scopeA, w.env.ID(), edited.Version(), delivery.EnvironmentEdit{Kubeconfig: &ref})
	if err != nil {
		t.Fatal(err)
	}
	if edited.Record().Namespace != ns || edited.Record().Kubeconfig.Ref != ref.Ref {
		t.Fatalf("the edit did not apply: %+v", edited.Record())
	}
	_ = other

	// A restart: another process sees only the rows.
	w.Restart()

	t.Run("the_admitted_run_keeps_its_pin_and_credential_version", func(t *testing.T) {
		agg, err := w.ExecStore.Get(ctx, scopeA, flow.Run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got := agg.Run.Snapshot.Environments[0].Pin; got != admitted {
			t.Fatalf("the persisted pin changed: %+v -> %+v", admitted, got)
		}
		if agg.Run.SnapshotDigest != flow.Run.SnapshotDigest {
			t.Fatal("the run's input snapshot digest changed")
		}
		again, err := w.Svc.RetainedSnapshot(ctx, scopeA, admitted.ID, admitted.ConfigDigest)
		if err != nil || again.Namespace != namespace || again.Config["REPLICAS"] != "1" || again.Secrets[0] != snap.Secrets[0] || !again.CreatedAt.Equal(snap.CreatedAt) {
			t.Fatalf("the retained snapshot changed: %v %+v", err, again)
		}
		// The credential version pin is still the admitted one, unreleased.
		if n := w.runPins(flow.Run.ID, 1); n != 1 {
			t.Fatalf("the admitted credential pin was lost: %d", n)
		}
		if n := w.runPins(flow.Run.ID, 2); n != 0 {
			t.Fatalf("the run was pinned to the rotated version: %d", n)
		}
		// The dispatched context of the attempt (persisted at claim, read back
		// by delivery on every callback) carries the admitted configuration.
		at := agg.Attempt(deploy.ID())
		c, err := runtimev1.DecodeRequest[runtimev1.Context](at.Context)
		if err != nil {
			t.Fatal(err)
		}
		if c.Environment == nil || *c.Environment != admitted || len(c.Secrets) != 1 || c.Secrets[0] != snap.Secrets[0] {
			t.Fatalf("the attempt's pinned context changed: %+v %+v", c.Environment, c.Secrets)
		}
		if strings.Contains(string(at.Context), "SENTINEL") {
			t.Fatal("credential plaintext reached the runtime context")
		}
	})

	t.Run("delivery_callbacks_use_the_retained_configuration_not_the_edited_environment", func(t *testing.T) {
		rc, err := w.Svc.RegisterRuntime(ctx, deploy.ID(), deploy.Token, delivery.RegistrationRequest{
			Source: flow.Source, Image: delivery.Image{ArtifactID: flow.Image, Digest: runtimev1.Digest(string(img.Digest))}})
		if err != nil {
			t.Fatalf("register after the edits: %v", err)
		}
		rec := rc.Deployment.Record()
		if rec.Namespace != namespace || rec.ConfigDigest != admitted.ConfigDigest || rec.Environment != admitted.ID {
			t.Fatalf("the deployment was recorded against the edited environment: ns=%s digest=%s", rec.Namespace, rec.ConfigDigest)
		}
		// The receipt's annotations carry the admitted configuration digest.
		if got := rc.Annotations[delivery.DefaultAnnotations.Configuration]; got != string(admitted.ConfigDigest) {
			t.Fatalf("the annotation carries %q", got)
		}
	})

	t.Run("a_later_run_pins_the_new_snapshot", func(t *testing.T) {
		fresh := w.admit(t)
		pin := pinOf(t, fresh)
		if pin.ConfigDigest == admitted.ConfigDigest {
			t.Fatal("a run admitted after the edits reused the old snapshot")
		}
		again, err := w.Svc.RetainedSnapshot(ctx, scopeA, pin.ID, pin.ConfigDigest)
		if err != nil || again.Namespace != ns || again.Config["REPLICAS"] != "9" || again.Secrets[0].Ref != "secret://project/kubeconfig-other" {
			t.Fatalf("the new snapshot: %v %+v", err, again)
		}
		if n := w.snapshots(); n != 2 {
			t.Fatalf("%d snapshots retained, want old and new", n)
		}
		if n := w.runPins(fresh.ID, 1); n != 1 {
			t.Fatalf("the new run's credential pin: %d", n)
		}
		// The old run still lists only its own version.
		if w.runPins(flow.Run.ID, 1) != 1 {
			t.Fatal("the old run's credential pin changed")
		}
	})

	t.Run("revoking_the_pinned_version_does_not_rewrite_the_admitted_pin", func(t *testing.T) {
		// The credential's old version is revoked after admission: the run's pin and
		// the retained snapshot keep naming it (nothing substitutes a newer
		// version into an admitted run).
		if err := w.X.RevokeSecret(scopeA, w.secret.ID(), 1); err != nil {
			t.Fatal(err)
		}
		snap2, err := w.Svc.RetainedSnapshot(ctx, scopeA, admitted.ID, admitted.ConfigDigest)
		if err != nil || snap2.Secrets[0].Version != 1 {
			t.Fatalf("the retained snapshot changed with the revocation: %v %+v", err, snap2.Secrets)
		}
		agg, err := w.ExecStore.Get(ctx, scopeA, flow.Run.ID)
		if err != nil || agg.Run.Snapshot.Secrets[0].Binding.Version != 1 {
			t.Fatalf("the run's credential binding changed: %v", err)
		}
	})

	t.Run("time_alone_never_changes_the_pin", func(t *testing.T) {
		w.X.Clock.Advance(48 * time.Hour)
		agg, err := w.ExecStore.Get(ctx, scopeA, flow.Run.ID)
		if err != nil || agg.Run.Snapshot.Environments[0].Pin != admitted {
			t.Fatalf("the pin moved: %v", err)
		}
	})
	_ = execution.PurposeWork
	_ = domain.RunID("")
}
