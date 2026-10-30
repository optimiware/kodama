package deliveryv1

import (
	"bytes"
	"strings"
	"testing"

	artifactsv1 "github.com/optimiware/kodama/contracts/artifacts/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gates"
)

func Test_unit_generic_delivery_contract(t *testing.T) {
	scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	ttl := int64(3600)
	cleanupDef := "kodama-cleanup"

	// Tool-neutral environment snapshot: no mandatory namespace, kubeconfig or preview|stage kind.
	snap1, err := NewEnvironmentSnapshotV2(EnvironmentSnapshotV2Input{
		Scope:             scope,
		Environment:       "env_cf_workers",
		Name:              "cloudflare-prod",
		Target:            "cloudflare.workers",
		Config:            map[string]string{"ACCOUNT_ID": "cf_123", "WORKER_NAME": "inventory-edge"},
		PreviewTTLSeconds: &ttl,
		CleanupDefinition: &cleanupDef,
		Secrets: []LogicalSecretBinding{
			{Name: "REGISTRY_AUTH", Ref: "secret://project/prod-registry", Delivery: runtimev1.DeliveryFile},
			{Name: "CF_API_TOKEN", Ref: "secret://project/cf-token", Delivery: runtimev1.DeliveryEnv},
		},
	})
	if err != nil {
		t.Fatalf("NewEnvironmentSnapshotV2: %v", err)
	}

	// Rotating a secret version in a later rollback run keeps the exact same ConfigDigest!
	mergedRun1, err := MergeStepAndEnvironmentSecrets(
		snap1.Secrets(),
		map[string]int64{"secret://project/cf-token": 1, "secret://project/prod-registry": 2},
		[]runtimev1.SecretBinding{{Name: "EXTRA_STEP_TOKEN", Ref: "secret://project/step-tok", Version: 4, Delivery: runtimev1.DeliveryEnv}},
	)
	if err != nil {
		t.Fatalf("MergeStepAndEnvironmentSecrets run 1: %v", err)
	}
	if len(mergedRun1) != 3 {
		t.Fatalf("expected 3 merged bindings (2 env + 1 step), got %d", len(mergedRun1))
	}

	mergedRollback, err := MergeStepAndEnvironmentSecrets(
		snap1.Secrets(),
		map[string]int64{"secret://project/cf-token": 2, "secret://project/prod-registry": 2},
		nil,
	)
	if err != nil {
		t.Fatalf("MergeStepAndEnvironmentSecrets rollback: %v", err)
	}
	if mergedRollback[0].Version != 2 {
		t.Fatalf("expected rotated version 2 in new rollback run pin, got %d", mergedRollback[0].Version)
	}

	// Ambiguous collision between step secret and environment secret fails closed.
	if _, err := MergeStepAndEnvironmentSecrets(
		snap1.Secrets(),
		map[string]int64{"secret://project/cf-token": 1, "secret://project/prod-registry": 2},
		[]runtimev1.SecretBinding{{Name: "CF_API_TOKEN", Ref: "secret://project/other-token", Version: 1, Delivery: runtimev1.DeliveryEnv}},
	); err == nil {
		t.Fatal("expected conflicting secret binding name to fail")
	}

	// Infrastructure-only deployment (Terraform/OpenTofu) needs no OCI image or build artifact.
	infraIntent := GenericDeploymentIntent{
		Scope:       scope,
		ActionKey:   "run_tf.apply",
		RunID:       "run_tf",
		StepID:      "apply",
		AttemptID:   "att_tf_1",
		Environment: snap1.Pin(),
		Release:     nil,
		Artifacts:   nil,
		Sources: []artifactsv1.SourceRef{
			{RepositoryURL: "https://git.example.invalid/acme/infra.git", Commit: strings.Repeat("a", 40)},
		},
		Script: ScriptIdentity{
			Image:   "registry.example.invalid/opentofu@sha256:" + strings.Repeat("3", 64),
			Command: []string{"tofu", "apply", "-auto-approve"},
		},
	}
	if err := infraIntent.Validate(); err != nil {
		t.Fatalf("infrastructure-only deployment rejected: %v", err)
	}

	outcome := GenericDeploymentOutcome{
		Status:       "succeeded",
		Attribution:  AttributionToolObserved,
		ExternalRefs: map[string]string{"worker_version": "v42", "route": "inventory.example.com/*"},
	}
	if err := outcome.Validate(); err != nil {
		t.Fatalf("valid generic outcome rejected: %v", err)
	}
}

func Test_unit_sdlc_contract_compatibility(t *testing.T) {
	scope := domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}
	dEv := runtimev1.Digest("sha256:" + strings.Repeat("a", 64))
	dRel := runtimev1.Digest("sha256:" + strings.Repeat("b", 64))

	// 1. Legacy v1 gate subject canonical bytes and digest remain byte-for-byte unchanged.
	legacySubj, err := gates.NewSubject(gates.SubjectInput{
		Scope:           scope,
		WorkItemID:      "wi_1042",
		ContentRevision: 2,
		CandidateCommit: strings.Repeat("1", 40),
		DefinitionID:    "pd_01",
		Evidence:        []gates.EvidenceRef{{ID: "art_tests", Digest: string(dEv)}},
	})
	if err != nil {
		t.Fatalf("gates.NewSubject: %v", err)
	}
	restoredLegacy, err := gates.ParseSubject(legacySubj.Canonical())
	if err != nil || restoredLegacy.Digest() != legacySubj.Digest() {
		t.Fatalf("legacy gate subject failed round-trip: %v", err)
	}

	// 2. V2 gate subject supports release-only / artifact-only review without fake workItemId or commit.
	v2Subj, err := NewGateSubjectV2(GateSubjectV2Input{
		Scope:        scope,
		Release:      &ReleasePin{ID: "rel_01", ManifestDigest: dRel},
		DefinitionID: "pd_release",
		Evidence:     []artifactsv1.ArtifactDigestRef{{ID: "art_tests", Digest: dEv}},
	})
	if err != nil {
		t.Fatalf("NewGateSubjectV2: %v", err)
	}
	parsedV2, err := ParseGateSubjectV2(v2Subj.Canonical())
	if err != nil || parsedV2.Digest() != v2Subj.Digest() || !bytes.Equal(parsedV2.Canonical(), v2Subj.Canonical()) {
		t.Fatalf("ParseGateSubjectV2 round-trip failed: %v", err)
	}

	// 3. Cross-version refusal: legacy parser rejects v2 canonical bytes and v2 parser rejects v1 bytes.
	if _, err := gates.ParseSubject(v2Subj.Canonical()); err == nil {
		t.Fatal("legacy gates.ParseSubject must reject v2 subject bytes")
	}
	if _, err := ParseGateSubjectV2(legacySubj.Canonical()); err == nil {
		t.Fatal("ParseGateSubjectV2 must reject legacy v1 subject bytes")
	}
}
