//go:build integration

package services

// Delivery recipe: readiness gates success and an uncertain deploy is observed
// before it is repeated.
//
// These recipes treat the real fixture registry as the deployed target:
//
//   - a deploy action exits zero but does not prove target health; the later
//     bounded readiness action queries the exact digest from inside a workload
//     and only then promotes a ready output;
//   - when readiness targets a digest the registry does not serve, the deploy
//     action still shows succeeded but the execution fails, so an exit zero is
//     never conflated with a healthy target;
//   - an action that records a deploy intent and then fails post-effect leaves
//     no promoted success; a fresh execution observes the real target before it
//     may verify/repeat, and an unobservable target fails the observation and
//     cancels the later verification action.
//
// Fixture limit: the workload image cannot push image bytes (no skopeo/crane),
// so the test-side image tool builds and publishes a disposable per-test target
// before the action models the deploy. These recipes compose neither the
// git-capable fixture image nor the factory's Git service; the real protected
// deploy update is proved by delivery_deploy_test.go. The observation itself
// is real: a workload resolves the exact digest from the real fixture registry
// through a narrow host relay, because the runtime's pasta network cannot
// reach the registry fixture's isolated shard network. A deploy action that
// performs the external rollout from inside the workload is not covered here.

import (
	"strings"
	"testing"

	"github.com/optimiware/kodama/test/support/factory"
	support "github.com/optimiware/kodama/test/support/infrastructure/messaging"
)

// TestDeliveryReadinessGatesTargetHealth proves readiness is a separate bounded
// observation of the real target and that its failure fails the execution even
// though the deploy action exited zero.
func TestDeliveryReadinessGatesTargetHealth(t *testing.T) {
	harness := factory.Start(t)
	registry := support.StartRegistry(t, false)
	published := deliveryPublishImage(t, registry, harness.Image,
		"kodama/delivery-readiness", "build-1")
	edgePort := deliveryRegistryEdge(t, registry)

	positive := deliveryApply(t, harness, "delivery-readiness", deliveryPipelineDoc{
		Actions: []deliveryActionDoc{
			{
				Name:    "deploy",
				Image:   harness.Image.Reference,
				Timeout: deliveryActionTimeout,
				Commands: []string{
					// A deploy step that exits zero. It proves only that the
					// command ran, never that the target is healthy.
					`echo deploy-command-exited-zero`,
				},
			},
			{
				Name:    "readiness",
				Image:   harness.Image.Reference,
				Timeout: deliveryActionTimeout,
				Outputs: []string{"ready"},
				Commands: []string{
					deliveryRegistryProbeCommand(edgePort, published.Repository, published.Digest),
					`READY='ready:` + published.Digest + `'`,
					deliverySubmitScript("ready", "READY"),
				},
			},
		},
	})
	started := deliveryStart(t, harness, positive.PipelineID, nil, "start-delivery-readiness")
	healthy := deliveryRequireSucceeded(t, harness, started.ExecutionID)
	if got := actionByName(t, healthy, "deploy").State; got != "succeeded" {
		t.Fatalf("deploy action state = %q; want succeeded", got)
	}
	if got := actionByName(t, healthy, "readiness").Outputs["ready"]; got != "ready:"+published.Digest {
		t.Fatalf("readiness output = %q; want the observed exact digest", got)
	}

	// Negative control: the deploy action still exits zero, but readiness checks
	// a digest the registry does not serve. The execution must fail at the
	// readiness action rather than treat the deploy exit as health.
	absent := "sha256:" + strings.Repeat("0", 64)
	negative := deliveryApply(t, harness, "delivery-readiness-missing", deliveryPipelineDoc{
		Actions: []deliveryActionDoc{
			{
				Name:     "deploy",
				Image:    harness.Image.Reference,
				Timeout:  deliveryActionTimeout,
				Commands: []string{`echo deploy-command-exited-zero`},
			},
			{
				Name:    "readiness",
				Image:   harness.Image.Reference,
				Timeout: deliveryActionTimeout,
				Outputs: []string{"ready"},
				Commands: []string{
					deliveryRegistryProbeCommand(edgePort, published.Repository, absent),
					`READY='ready:` + absent + `'`,
					deliverySubmitScript("ready", "READY"),
				},
			},
		},
	})
	missingStarted := deliveryStart(t, harness, negative.PipelineID, nil, "start-delivery-readiness-missing")
	failed := deliveryRequireFailed(t, harness, missingStarted.ExecutionID)
	if failed.Failure == nil || failed.Failure.Kind != "runtime" || failed.Failure.Reason != "workload_exited" {
		t.Fatalf("failure = %+v; want the failed readiness workload", failed.Failure)
	}
	if got := actionByName(t, failed, "deploy").State; got != "succeeded" {
		t.Fatalf("deploy action state = %q; want succeeded even though the target was unhealthy", got)
	}
	readiness := actionByName(t, failed, "readiness")
	if readiness.State != "failed" {
		t.Fatalf("readiness action state = %q; want failed", readiness.State)
	}
	if len(readiness.Outputs) != 0 {
		t.Fatalf("failed readiness published %+v; a failed observation must not claim readiness", readiness.Outputs)
	}

	// The positive target is still exactly served; the negative digest is not.
	deliveryRequireRegistryDigest(t, registry, published)
	deliveryAssertNoArtifactEntity(t)
}

// TestDeliveryUncertainDeployIsObservedBeforeRepetition proves a post-effect
// failure claims no success, and a fresh attempt first observes the real target
// state; when that observation cannot confirm the target, the verification step
// never activates.
func TestDeliveryUncertainDeployIsObservedBeforeRepetition(t *testing.T) {
	harness := factory.Start(t)
	registry := support.StartRegistry(t, false)
	published := deliveryPublishManifest(t, registry, "kodama/delivery-reconcile", "deploy-1")
	edgePort := deliveryRegistryEdge(t, registry)

	// The deploy records its intent and then fails after the effect: the target
	// bytes are already published, but the execution must not report success.
	uncertain := deliveryApply(t, harness, "delivery-uncertain-deploy", deliveryPipelineDoc{
		Actions: []deliveryActionDoc{{
			Name:    "deploy",
			Image:   harness.Image.Reference,
			Timeout: deliveryActionTimeout,
			Outputs: []string{"deployed"},
			Commands: []string{
				`DEPLOYED='deployed:` + published.Pinned + `'`,
				deliverySubmitScript("deployed", "DEPLOYED"),
				`exit 9`,
			},
		}},
	})
	uncertainStarted := deliveryStart(t, harness, uncertain.PipelineID, nil, "start-delivery-uncertain")
	failed := deliveryRequireFailed(t, harness, uncertainStarted.ExecutionID)
	if failed.Failure == nil || failed.Failure.Kind != "runtime" || failed.Failure.Reason != "workload_exited" {
		t.Fatalf("uncertain deploy failure = %+v; want runtime/workload_exited", failed.Failure)
	}
	if !strings.Contains(failed.Failure.Detail, "status 9") {
		t.Fatalf("uncertain deploy detail = %q; want the post-effect exit status", failed.Failure.Detail)
	}
	deploy := actionByName(t, failed, "deploy")
	if deploy.State != "failed" {
		t.Fatalf("deploy action state = %q; want failed", deploy.State)
	}
	if len(deploy.Outputs) != 0 {
		t.Fatalf("failed deploy published %+v; an uncertain effect must not be recorded as success", deploy.Outputs)
	}
	row, err := harness.ActionRow(t.Context(), uncertainStarted.ExecutionID, "deploy")
	if err != nil {
		t.Fatalf("read failed deploy row: %v", err)
	}
	if row.Outputs != "" || row.StagedOutputs != "" || row.StagedOutputsSet {
		t.Fatalf("failed deploy kept durable outputs=%q staged=%q set=%v; want discarded",
			row.Outputs, row.StagedOutputs, row.StagedOutputsSet)
	}

	// Reconciliation observes the real target before it verifies/repeats: the
	// observation runs first, and only a successful observation lets the later
	// verification action run.
	reconcile := deliveryApply(t, harness, "delivery-reconcile", deliveryPipelineDoc{
		Inputs: []string{"image"},
		Actions: []deliveryActionDoc{
			{
				Name:    "observe",
				Image:   harness.Image.Reference,
				Timeout: deliveryActionTimeout,
				Outputs: []string{"observed"},
				Commands: []string{
					`test "$(cat /kodama/inputs/image)" = '` + published.Pinned + `'`,
					deliveryRegistryProbeCommand(edgePort, published.Repository, published.Digest),
					`OBSERVED='observed:` + published.Digest + `'`,
					deliverySubmitScript("observed", "OBSERVED"),
				},
			},
			{
				Name:    "verify",
				Image:   harness.Image.Reference,
				Timeout: deliveryActionTimeout,
				Outputs: []string{"reconciled"},
				Commands: []string{
					`test "$(cat /kodama/outputs/observe/observed)" = 'observed:` + published.Digest + `'`,
					`RECONCILED='reconciled:` + published.Digest + `'`,
					deliverySubmitScript("reconciled", "RECONCILED"),
				},
			},
		},
	})
	reconcileStarted := deliveryStart(t, harness, reconcile.PipelineID,
		map[string]string{"image": published.Pinned}, "start-delivery-reconcile")
	reconciled := deliveryRequireSucceeded(t, harness, reconcileStarted.ExecutionID)
	if got := actionByName(t, reconciled, "observe").Outputs["observed"]; got != "observed:"+published.Digest {
		t.Fatalf("observe output = %q; want the live exact digest", got)
	}
	if got := actionByName(t, reconciled, "verify").Outputs["reconciled"]; got != "reconciled:"+published.Digest {
		t.Fatalf("verify output = %q; want the reconciled digest", got)
	}

	// Negative control: the same reconcile recipe with an unobservable target
	// fails at observation and never activates verification, so a repeat cannot
	// proceed on an unconfirmed effect.
	absent := "sha256:" + strings.Repeat("0", 64)
	blocked := deliveryApply(t, harness, "delivery-reconcile-missing", deliveryPipelineDoc{
		Inputs: []string{"image"},
		Actions: []deliveryActionDoc{
			{
				Name:    "observe",
				Image:   harness.Image.Reference,
				Timeout: deliveryActionTimeout,
				Outputs: []string{"observed"},
				Commands: []string{
					deliveryRegistryProbeCommand(edgePort, published.Repository, absent),
				},
			},
			{
				Name:    "verify",
				Image:   harness.Image.Reference,
				Timeout: deliveryActionTimeout,
				Outputs: []string{"reconciled"},
				Commands: []string{
					`echo must-not-run`,
				},
			},
		},
	})
	blockedStarted := deliveryStart(t, harness, blocked.PipelineID,
		map[string]string{"image": published.Host + "/" + published.Repository + "@" + absent},
		"start-delivery-reconcile-missing")
	blockedFailed := deliveryRequireFailed(t, harness, blockedStarted.ExecutionID)
	if got := actionByName(t, blockedFailed, "observe").State; got != "failed" {
		t.Fatalf("missing-target observation state = %q; want failed", got)
	}
	verify := actionByName(t, blockedFailed, "verify")
	if verify.State != "cancelled" {
		t.Fatalf("verification state = %q; want cancelled after an unconfirmed observation", verify.State)
	}
	if verify.ActivatedAt != nil {
		t.Fatalf("verification was activated at %v despite an unconfirmed observation", verify.ActivatedAt)
	}

	deliveryRequireRegistryDigest(t, registry, published)
	deliveryAssertNoArtifactEntity(t)
}
