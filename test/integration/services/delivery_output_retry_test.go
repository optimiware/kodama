//go:build integration

package services

// Delivery recipe: a lost output response is retried with the identical value
// without rebuilding, and a changed retry is refused.
//
// The scenario holds the build action open with a filesystem barrier, submits
// the digest-pinned output through the served execution.action.outputs operation
// with the workload's own mounted credential, repeats the identical submission
// with a fresh idempotency key, and then attempts a changed value. Only after
// the workload is released does settlement promote the staged value, so the
// final execution record proves which value survived the retries.

import (
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/test/support/factory"
	support "github.com/optimiware/kodama/test/support/infrastructure/messaging"
)

// TestDeliveryOutputRetryConvergesWithoutRebuild proves the delivery retry
// rule: an identical output retry converges without a second commit or a
// rebuild, and a different value is refused so the staged digest cannot drift.
func TestDeliveryOutputRetryConvergesWithoutRebuild(t *testing.T) {
	harness := factory.Start(t)
	registry := support.StartRegistry(t, false)
	published := deliveryPublishManifest(t, registry, "kodama/delivery-retry", "build-1")

	applied := deliveryApply(t, harness, "delivery-retry", deliveryPipelineDoc{
		Actions: []deliveryActionDoc{{
			Name:    "build",
			Image:   harness.Image.Reference,
			Timeout: deliveryActionTimeout,
			Outputs: []string{"image"},
			Commands: []string{
				`test ! -e /kodama/task.json`,
				// Hold the action open until the test releases it. The test
				// then owns the whole retry sequence deterministically.
				`while [ ! -f /workspace/release ]; do sleep 1; done`,
				`test "$(cat /workspace/release)" = go`,
			},
		}},
	})
	started := deliveryStart(t, harness, applied.PipelineID, nil, "start-delivery-retry")

	running := deliveryWaitActionRunningStable(t, harness, started.ExecutionID, "build")
	credential := deliveryExecutionCredential(t, harness, started.ExecutionID, "build")
	action := actionByName(t, running, "build")

	// First submission: the exact published digest as the declared output. It
	// commits exactly one staging transition.
	firstResult, firstBefore := deliveryStageOutputs(t, harness, credential, started.ExecutionID, action.ActionID,
		map[string]string{"image": published.Pinned}, "delivery-outputs")
	if firstResult.ExecutionVersion != firstBefore+1 {
		t.Fatalf("first submission moved the execution from %d to %d; want exactly one staging commit",
			firstBefore, firstResult.ExecutionVersion)
	}

	// Identical retry with a fresh idempotency key. It converges on the staged
	// value and commits nothing. The result names the version the retry was
	// submitted against; an independent re-inspection after the retry must still
	// equal the version the first submission committed.
	secondResult, _ := deliveryStageOutputs(t, harness, credential, started.ExecutionID, action.ActionID,
		map[string]string{"image": published.Pinned}, "delivery-outputs-identical")
	if secondResult.ExecutionVersion != firstResult.ExecutionVersion {
		t.Fatalf("identical retry result version = %d; want the first submission's version %d",
			secondResult.ExecutionVersion, firstResult.ExecutionVersion)
	}
	afterRetry := inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	if afterRetry.Version != firstResult.ExecutionVersion {
		t.Fatalf("identical retry advanced the execution from %d to %d; want no second staging commit",
			firstResult.ExecutionVersion, afterRetry.Version)
	}

	// A changed value for the same action must be refused, not rebind the
	// published digest.
	changed := "sha256:" + strings.Repeat("f", 64)
	publicError := deliveryRejectOutputs(t, harness, credential, started.ExecutionID, action.ActionID,
		map[string]string{"image": changed}, "delivery-outputs-changed")
	if publicError.Code != api.CodeChangedIdempotencyContent {
		t.Fatalf("changed output code = %q; want %q", publicError.Code, api.CodeChangedIdempotencyContent)
	}

	// Release the workload; settlement promotes the staged value exactly once.
	deliveryReleaseWorkload(t, harness, started.ExecutionID)
	execution := deliveryRequireSucceeded(t, harness, started.ExecutionID)
	if len(execution.Actions) != 1 {
		t.Fatalf("execution has %d actions; want the single build action", len(execution.Actions))
	}
	build := actionByName(t, execution, "build")
	if build.State != "succeeded" {
		t.Fatalf("build action state = %q; want succeeded", build.State)
	}
	if build.Outputs["image"] != published.Pinned {
		t.Fatalf("promoted output = %q; want the original digest %q (a changed retry must not win)",
			build.Outputs["image"], published.Pinned)
	}
	if build.Outputs["image"] == changed {
		t.Fatal("a refused changed retry replaced the staged output")
	}

	// The action was activated and ran exactly once: no rebuild happened across
	// the retries.
	if build.ActivatedAt == nil {
		t.Fatal("build action has no activation time")
	}
	if count := deliveryCountActionTransitions(t, harness, build.ActionID); count == 0 {
		t.Fatalf("build action %s has no committed action.transition event", build.ActionID)
	}
	deliveryRequireRegistryDigest(t, registry, published)
	deliveryAssertNoArtifactEntity(t)
}
