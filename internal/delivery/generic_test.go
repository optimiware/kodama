package delivery_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	artifactsv1 "github.com/optimiware/kodama/contracts/artifacts/v1"
	deliveryv1 "github.com/optimiware/kodama/contracts/delivery/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/delivery"
	deliveryadapters "github.com/optimiware/kodama/internal/delivery/adapters"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

type mapCredentials struct {
	versions map[string]int64
	revoked  map[string]bool
}

func (m *mapCredentials) Current(_ context.Context, _ domain.Scope, ref string) (delivery.CredentialInfo, error) {
	if m.revoked[ref] {
		return delivery.CredentialInfo{}, delivery.ErrCredentialUnavailable
	}
	v, ok := m.versions[ref]
	if !ok {
		return delivery.CredentialInfo{}, delivery.ErrCredentialUnavailable
	}
	return delivery.CredentialInfo{Ref: ref, Kind: "generic", Version: v}, nil
}

type mapGenericRecords struct {
	byAction map[string]delivery.GenericDeployment
}

func (m *mapGenericRecords) FindGenericByAction(_ context.Context, scope domain.Scope, actionKey string) (delivery.GenericDeployment, error) {
	d, ok := m.byAction[string(scope.TenantID)+"|"+string(scope.ProjectID)+"|"+actionKey]
	if !ok {
		return delivery.GenericDeployment{}, delivery.ErrNotFound
	}
	return d, nil
}

func Test_unit_tool_neutral_deployment(t *testing.T) {
	scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

	// 1. Non-Kubernetes target (Wrangler / Cloudflare Workers) with release + archive artifact, no kubeconfig or namespace.
	snap, err := deliveryv1.NewEnvironmentSnapshotV2(deliveryv1.EnvironmentSnapshotV2Input{
		Scope:       scope,
		Environment: "env_cf_prod",
		Name:        "cf-prod",
		Target:      "cloudflare.workers",
		Config:      map[string]string{"WORKER": "inventory-api"},
		Secrets: []deliveryv1.LogicalSecretBinding{
			{Name: "CF_API_TOKEN", Ref: "secret://project/cf-token", Delivery: runtimev1.DeliveryEnv},
		},
	})
	if err != nil {
		t.Fatalf("NewEnvironmentSnapshotV2: %v", err)
	}

	relPin := deliveryv1.ReleasePin{
		ID:             "rel_100",
		ManifestDigest: runtimev1.Digest("sha256:" + strings.Repeat("a", 64)),
	}
	intent := deliveryv1.GenericDeploymentIntent{
		Scope:       scope,
		ActionKey:   "run_01.deploy",
		RunID:       "run_01",
		StepID:      "deploy",
		AttemptID:   "att_01",
		Environment: snap.Pin(),
		Release:     &relPin,
		Artifacts: []artifactsv1.ArtifactDigestRef{
			{ID: "art_worker_bundle", Digest: runtimev1.Digest("sha256:" + strings.Repeat("b", 64))},
		},
		Sources: []artifactsv1.SourceRef{
			{RepositoryURL: "https://git.example.invalid/acme/inventory.git", Commit: strings.Repeat("1", 40)},
		},
		Script: deliveryv1.ScriptIdentity{
			Image:   "registry.example.invalid/wrangler@sha256:" + strings.Repeat("c", 64),
			Command: []string{"wrangler", "deploy"},
		},
	}

	actor := delivery.Actor{Type: delivery.ActorWorkload, ID: "att_01"}
	dep, err := delivery.NewGenericDeployment(intent, actor, 0, now)
	if err != nil {
		t.Fatalf("NewGenericDeployment: %v", err)
	}
	if !dep.MatchesIntent(intent) {
		t.Fatal("MatchesIntent must be true for identical intent")
	}

	// Pre-apply transition -> running.
	if err := dep.ApplyOutcome(deliveryv1.GenericDeploymentOutcome{
		Status:      "applying",
		Attribution: deliveryv1.AttributionCommandReported,
	}, actor, "att_01", now.Add(time.Second)); err != nil {
		t.Fatalf("ApplyOutcome applying: %v", err)
	}
	if dep.Status() != delivery.StatusRunning || dep.Reason() != delivery.ReasonApplying {
		t.Fatalf("unexpected state after applying: %s (%s)", dep.Status(), dep.Reason())
	}

	// Succeeded transition with tool-observed external references.
	if err := dep.ApplyOutcome(deliveryv1.GenericDeploymentOutcome{
		Status:       "succeeded",
		Attribution:  deliveryv1.AttributionToolObserved,
		ExternalRefs: map[string]string{"worker_id": "w_99", "deployment_url": "https://inventory.acme.workers.dev"},
	}, actor, "att_01", now.Add(2*time.Second)); err != nil {
		t.Fatalf("ApplyOutcome succeeded: %v", err)
	}
	if dep.Status() != delivery.StatusSucceeded || dep.ExternalRefs()["worker_id"] != "w_99" {
		t.Fatalf("unexpected settled deployment: %+v", dep.Record())
	}

	// Late conflicting report on a settled deployment is rejected.
	if err := dep.ApplyOutcome(deliveryv1.GenericDeploymentOutcome{
		Status:      "failed",
		Attribution: deliveryv1.AttributionCommandReported,
	}, actor, "att_01", now.Add(3*time.Second)); !errors.Is(err, delivery.ErrConflict) {
		t.Fatalf("expected ErrConflict on conflicting report after settlement, got %v", err)
	}
}

func Test_unit_generic_delivery_unknown_effect(t *testing.T) {
	scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

	snap, err := deliveryv1.NewEnvironmentSnapshotV2(deliveryv1.EnvironmentSnapshotV2Input{
		Scope:       scope,
		Environment: "env_tofu",
		Name:        "infra-prod",
		Target:      "opentofu.cloud",
		Config:      map[string]string{"TF_WORKSPACE": "prod"},
	})
	if err != nil {
		t.Fatal(err)
	}

	records := &mapGenericRecords{byAction: map[string]delivery.GenericDeployment{}}
	reconciler, err := deliveryadapters.NewGenericEffects(records)
	if err != nil {
		t.Fatal(err)
	}
	q := execution.EffectQuery{
		Scope:     scope,
		RunID:     "run_tf",
		StepID:    "apply",
		AttemptID: "att_tf_1",
		ActionKey: "run_tf.apply",
	}

	// 1. Absent deployment record NEVER authorizes automatic replay (VerdictStillUnknown).
	ev, err := reconciler.Reconcile(t.Context(), q)
	if err != nil || ev.Verdict != execution.VerdictStillUnknown {
		t.Fatalf("expected VerdictStillUnknown when no deployment record exists, got %+v err=%v", ev, err)
	}

	// 2. Infrastructure-only deployment (no OCI image or build artifact) enters unknown outcome.
	actor := delivery.Actor{Type: delivery.ActorWorkload, ID: "att_tf_1"}
	dep, err := delivery.NewGenericDeployment(deliveryv1.GenericDeploymentIntent{
		Scope:       scope,
		ActionKey:   q.ActionKey,
		RunID:       q.RunID,
		StepID:      q.StepID,
		AttemptID:   q.AttemptID,
		Environment: snap.Pin(),
		Sources: []artifactsv1.SourceRef{
			{RepositoryURL: "https://git.example.invalid/acme/infra.git", Commit: strings.Repeat("d", 40)},
		},
		Script: deliveryv1.ScriptIdentity{
			Image:   "registry.example.invalid/opentofu@sha256:" + strings.Repeat("e", 64),
			Command: []string{"tofu", "apply"},
		},
	}, actor, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := dep.ApplyOutcome(deliveryv1.GenericDeploymentOutcome{
		Status:      "unknown",
		Attribution: deliveryv1.AttributionCommandReported,
	}, actor, "att_tf_1", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	records.byAction[string(scope.TenantID)+"|"+string(scope.ProjectID)+"|"+q.ActionKey] = dep

	// Re-applying while outcome is unknown is refused with ErrOutcomeUnknown.
	if err := dep.ApplyOutcome(deliveryv1.GenericDeploymentOutcome{
		Status:      "applying",
		Attribution: deliveryv1.AttributionCommandReported,
	}, actor, "att_tf_2", now.Add(2*time.Second)); !errors.Is(err, delivery.ErrOutcomeUnknown) {
		t.Fatalf("expected ErrOutcomeUnknown on blind re-apply, got %v", err)
	}

	// Reconciler still reports VerdictStillUnknown while outcome is unknown.
	if ev, err := reconciler.Reconcile(t.Context(), q); err != nil || ev.Verdict != execution.VerdictStillUnknown {
		t.Fatalf("expected VerdictStillUnknown while deployment outcome is unknown, got %+v err=%v", ev, err)
	}

	// 3. Explicit operator/reconciliation settlement transitions the deployment
	//    and yields VerdictSettled (ReplaySafe=false, never automatic replay).
	opActor := delivery.Actor{Type: delivery.ActorUser, ID: "usr_eng"}
	if err := dep.SettleUnknown(opActor, delivery.StatusSucceeded, "ready", map[string]string{"state_serial": "18"}, now.Add(3*time.Second)); err != nil {
		t.Fatalf("SettleUnknown: %v", err)
	}
	records.byAction[string(scope.TenantID)+"|"+string(scope.ProjectID)+"|"+q.ActionKey] = dep

	evSettled, err := reconciler.Reconcile(t.Context(), q)
	if err != nil || evSettled.Verdict != execution.VerdictSettled {
		t.Fatalf("expected VerdictSettled after explicit settlement, got %+v err=%v", evSettled, err)
	}
}

func Test_unit_rollback_credentials_and_config(t *testing.T) {
	scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	snap, err := deliveryv1.NewEnvironmentSnapshotV2(deliveryv1.EnvironmentSnapshotV2Input{
		Scope:       scope,
		Environment: "env_prod",
		Name:        "prod",
		Target:      "helm.cluster",
		Config:      map[string]string{"RELEASE_NAME": "inventory"},
		Secrets: []deliveryv1.LogicalSecretBinding{
			{Name: "KUBECONFIG", Ref: "secret://project/prod-kubeconfig", Delivery: runtimev1.DeliveryFile},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	creds := &mapCredentials{
		versions: map[string]int64{"secret://project/prod-kubeconfig": 1, "secret://project/registry": 2},
		revoked:  map[string]bool{},
	}

	// Initial deployment run pins version 1 of prod-kubeconfig + version 2 of step registry secret.
	run1Secrets, err := delivery.ResolveRunCredentials(t.Context(), scope, snap, creds, []runtimev1.SecretBinding{
		{Name: "REGISTRY_AUTH", Ref: "secret://project/registry", Version: 2, Delivery: runtimev1.DeliveryFile},
	})
	if err != nil || len(run1Secrets) != 2 {
		t.Fatalf("ResolveRunCredentials run1: %v", err)
	}

	// Secret rotates to version 3 before rollback run: ConfigDigest on snap is
	// unchanged, and the new rollback run pins version 3 without altering run1Secrets!
	creds.versions["secret://project/prod-kubeconfig"] = 3
	rollbackSecrets, err := delivery.ResolveRunCredentials(t.Context(), scope, snap, creds, nil)
	if err != nil || len(rollbackSecrets) != 1 || rollbackSecrets[0].Version != 3 {
		t.Fatalf("ResolveRunCredentials rollback: %+v err=%v", rollbackSecrets, err)
	}
	if run1Secrets[0].Version != 1 {
		t.Fatalf("historical run1 secret pin was mutated: %+v", run1Secrets)
	}

	// If the credential is revoked, rollback admission fails closed rather than
	// resurrecting the old version.
	creds.revoked["secret://project/prod-kubeconfig"] = true
	if _, err := delivery.ResolveRunCredentials(t.Context(), scope, snap, creds, nil); !errors.Is(err, delivery.ErrCredentialUnavailable) {
		t.Fatalf("expected ErrCredentialUnavailable when credential is revoked, got %v", err)
	}
}

func Test_unit_legacy_delivery_read(t *testing.T) {
	scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

	env, err := delivery.NewEnvironment(delivery.EnvironmentRecord{
		Scope:      scope,
		ID:         "env_legacy",
		Name:       "legacy-preview",
		Kind:       delivery.KindPreview,
		Target:     "k8s-stage",
		Namespace:  "acme-preview",
		Kubeconfig: delivery.Credential{Name: "KUBECONFIG", Ref: "secret://project/kubeconfig", Delivery: runtimev1.DeliveryFile},
		Config:     map[string]string{"REPLICAS": "2"},
		PreviewTTL: 24 * time.Hour,
	}, now)
	if err != nil {
		t.Fatalf("NewEnvironment legacy: %v", err)
	}

	legacySnap, err := delivery.NewSnapshot(env, map[string]int64{"secret://project/kubeconfig": 5}, now)
	if err != nil {
		t.Fatalf("NewSnapshot legacy: %v", err)
	}
	origDigest := legacySnap.Digest

	projected, err := delivery.ProjectLegacySnapshot(legacySnap)
	if err != nil {
		t.Fatalf("ProjectLegacySnapshot: %v", err)
	}
	if projected.LegacySnapshotDigest() == nil || *projected.LegacySnapshotDigest() != origDigest {
		t.Fatalf("projected snapshot did not link legacy digest %s: %+v", origDigest, projected.LegacySnapshotDigest())
	}
	// Legacy snapshot's own digest and validation remain intact.
	if err := legacySnap.Validate(); err != nil || legacySnap.Digest != origDigest {
		t.Fatalf("legacy snapshot mutated or invalid: %v", err)
	}
}
