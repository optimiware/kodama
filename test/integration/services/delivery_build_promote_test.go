//go:build integration

package services

// Delivery recipe: test tooling publishes a disposable per-test image, the
// build action records its exact digest, and a later action tests those exact
// bytes; promotion reuses the same digest without rebuilding.
//
// The TEST side builds and publishes the image; the build action never touches
// the registry. The test-side image tool builds one disposable image FROM the
// pinned workload base, tags and pushes only that per-test image to the local
// fixture registry, reads the digest back from the registry's manifest
// response, pulls it by exact digest into the Podman store, and every created
// name is removed in cleanup. The test action then runs those exact published
// bytes. The build action records the digest-pinned reference as its declared
// output using its own mounted execution credential, and the later action
// consumes that recorded output. Promotion is a separate execution that
// receives the built digest as an admitted input, carries it into a reviewer
// request subject, and integrates only after a real reviewer resolves that
// exact request.
//
// Fixture limit: this recipe carries the Git candidate/approval value as a
// literal subject value and writes no real work-branch candidate. The real
// protected deploy-repository commit and ref update are proved by
// delivery_deploy_test.go with factory.WithGitService.

import (
	"strings"
	"testing"

	"github.com/optimiware/kodama/test/support/factory"
	support "github.com/optimiware/kodama/test/support/infrastructure/messaging"
)

// TestDeliveryRecordsPublishedDigestAndRunsExactBytes proves the build/publish/
// test recipe end to end: the test tool publishes one disposable per-test image,
// the build action only records the exact digest-pinned reference it did not
// itself publish, the next action runs those exact bytes with the unique marker
// from the built image, and the registry really serves exactly that digest.
func TestDeliveryRecordsPublishedDigestAndRunsExactBytes(t *testing.T) {
	harness := factory.Start(t)
	registry := support.StartRegistry(t, false)

	published := deliveryPublishImage(t, registry, harness.Image,
		"kodama/delivery-build", "build-1")
	if !strings.HasPrefix(published.Pinned, published.Host+"/"+published.Repository+"@sha256:") {
		t.Fatalf("published reference %q is not a digest-pinned registry reference", published.Pinned)
	}

	applied := deliveryApply(t, harness, "delivery-build", deliveryPipelineDoc{
		Actions: []deliveryActionDoc{
			{
				Name:    "build",
				Image:   harness.Image.Reference,
				Timeout: deliveryActionTimeout,
				Outputs: []string{"image"},
				Commands: []string{
					`test ! -e /kodama/task.json`,
					`test -z "${KODAMA_TASK:-}"`,
					"IMAGE='" + published.Pinned + "'",
					deliverySubmitScript("image", "IMAGE"),
				},
			},
			{
				// This action runs the published bytes themselves: the image
				// reference is the exact digest the build action recorded, and
				// the marker file is the random content baked into the per-test
				// image, so running the shared base instead would fail.
				Name:    "test",
				Image:   published.Pinned,
				Timeout: deliveryActionTimeout,
				Outputs: []string{"tested"},
				Commands: []string{
					`test -f /etc/alpine-release`,
					`test "$(cat /kodama-delivery-marker)" = '` + published.Marker + `'`,
					`test "$(cat /kodama/outputs/build/image)" = '` + published.Pinned + `'`,
					`TESTED='tested:` + published.Digest + `'`,
					deliverySubmitScript("tested", "TESTED"),
				},
			},
		},
	})
	started := deliveryStart(t, harness, applied.PipelineID, nil, "start-delivery-build")
	execution := deliveryRequireSucceeded(t, harness, started.ExecutionID)

	if len(execution.Actions) != 2 {
		t.Fatalf("execution has %d actions; want the declared build and test actions", len(execution.Actions))
	}
	build := actionByName(t, execution, "build")
	if build.State != "succeeded" {
		t.Fatalf("build action state = %q; want succeeded", build.State)
	}
	if build.Outputs["image"] != published.Pinned {
		t.Fatalf("build output image = %q; want the exact published digest %q", build.Outputs["image"], published.Pinned)
	}
	tested := actionByName(t, execution, "test")
	if tested.State != "succeeded" {
		t.Fatalf("test action state = %q; want succeeded", tested.State)
	}
	if tested.Outputs["tested"] != "tested:"+published.Digest {
		t.Fatalf("test output = %q; want the exact tested digest", tested.Outputs["tested"])
	}

	// Attribution is durable: the build action owns committed transition events
	// and the recorded output lives on that exact action in the execution record.
	if count := deliveryCountActionTransitions(t, harness, build.ActionID); count == 0 {
		t.Fatalf("build action %s has no committed action.transition event", build.ActionID)
	}

	// The registry, not the output record, owns the bytes: prove it still serves
	// exactly the recorded digest and refuses an unpublished one.
	deliveryRequireRegistryDigest(t, registry, published)

	// An artifact is an output value. The served inventory must expose no
	// artifact registration entity.
	deliveryAssertNoArtifactEntity(t)
}

// TestDeliveryPromotionReusesDigestWithoutRebuild proves a promotion execution
// takes the already-built digest as an input, never runs a build action, carries
// that exact digest into the reviewer request subject, and integrates only after
// the request is resolved.
func TestDeliveryPromotionReusesDigestWithoutRebuild(t *testing.T) {
	harness := factory.Start(t)
	registry := support.StartRegistry(t, false)

	published := deliveryPublishImage(t, registry, harness.Image,
		"kodama/delivery-promote", "build-1")

	buildApplied := deliveryApply(t, harness, "delivery-promote-build", deliveryPipelineDoc{
		Actions: []deliveryActionDoc{{
			Name:    "build",
			Image:   harness.Image.Reference,
			Timeout: deliveryActionTimeout,
			Outputs: []string{"image"},
			Commands: []string{
				"IMAGE='" + published.Pinned + "'",
				deliverySubmitScript("image", "IMAGE"),
			},
		}},
	})
	buildStarted := deliveryStart(t, harness, buildApplied.PipelineID, nil, "start-delivery-promote-build")
	buildExecution := deliveryRequireSucceeded(t, harness, buildStarted.ExecutionID)
	built := actionByName(t, buildExecution, "build").Outputs["image"]
	if built != published.Pinned {
		t.Fatalf("build output = %q; want %q", built, published.Pinned)
	}

	candidate := "deploy-candidate:" + published.Pinned
	expectedBase := "refs/heads/deploy@base-1"
	approved := "approved:" + published.Digest

	promoteApplied := deliveryApply(t, harness, "delivery-promote", deliveryPipelineDoc{
		Inputs: []string{"image"},
		Actions: []deliveryActionDoc{
			{
				Name:    "release",
				Image:   harness.Image.Reference,
				Timeout: deliveryActionTimeout,
				Outputs: []string{"candidate", "expected_base"},
				Commands: []string{
					// The promoted value is exactly the earlier execution's
					// recorded output, supplied as an admitted input.
					`test "$(cat /kodama/inputs/image)" = '` + built + `'`,
					"CANDIDATE='" + candidate + "'",
					"BASE='" + expectedBase + "'",
					deliverySubmitScripts(
						deliveryOutput{Key: "candidate", Variable: "CANDIDATE"},
						deliveryOutput{Key: "expected_base", Variable: "BASE"},
					),
				},
			},
			{
				Name:    "approve",
				Request: "role/reviewer",
				Timeout: deliveryActionTimeout,
				Subject: map[string]any{
					"operation":     "git.ref.update",
					"resource":      "deploy-repo",
					"ref":           "refs/heads/deploy",
					"candidate":     map[string]string{"from": "release.outputs.candidate"},
					"expected_base": map[string]string{"from": "release.outputs.expected_base"},
					"image":         map[string]string{"from": "inputs.image"},
				},
				Outputs: []string{"approved"},
			},
			{
				Name:    "integrate",
				Image:   harness.Image.Reference,
				Timeout: deliveryActionTimeout,
				Outputs: []string{"deployed"},
				Commands: []string{
					`test "$(cat /kodama/outputs/release/candidate)" = '` + candidate + `'`,
					`test "$(cat /kodama/outputs/release/expected_base)" = '` + expectedBase + `'`,
					`test "$(cat /kodama/outputs/approve/approved)" = '` + approved + `'`,
					`test "$(cat /kodama/inputs/image)" = '` + built + `'`,
					`DEPLOYED='deployed:` + published.Digest + `'`,
					deliverySubmitScript("deployed", "DEPLOYED"),
				},
			},
		},
	})
	promoteStarted := deliveryStart(t, harness, promoteApplied.PipelineID,
		map[string]string{"image": built}, "start-delivery-promote")

	waiting := deliveryWaitActionRunning(t, harness, promoteStarted.ExecutionID, "approve")
	subject, err := harness.RequestSubject(t.Context(), promoteStarted.ExecutionID, "approve")
	if err != nil {
		t.Fatalf("read the persisted promotion subject: %v", err)
	}
	for key, want := range map[string]string{
		"operation":     "git.ref.update",
		"resource":      "deploy-repo",
		"ref":           "refs/heads/deploy",
		"candidate":     candidate,
		"expected_base": expectedBase,
		"image":         built,
	} {
		value, ok := subject[key]
		if !ok {
			t.Fatalf("promotion subject has no %q field: %+v", key, subject)
		}
		if value.Literal != want {
			t.Fatalf("promotion subject %q = %q; want %q", key, value.Literal, want)
		}
	}

	deliveryResolveRequest(t, harness, harness.ReviewerToken, waiting, "approve", "pass",
		map[string]string{"approved": approved}, "resolve-delivery-promote")

	execution := deliveryRequireSucceeded(t, harness, promoteStarted.ExecutionID)
	names := make([]string, 0, len(execution.Actions))
	for _, action := range execution.Actions {
		names = append(names, action.Name)
		if action.Name == "build" {
			t.Fatalf("promotion execution ran a build action; it must only promote the supplied digest (actions=%v)", names)
		}
	}
	if len(names) != 3 || names[0] != "release" || names[1] != "approve" || names[2] != "integrate" {
		t.Fatalf("promotion execution actions = %v; want [release approve integrate]", names)
	}
	if got := actionByName(t, execution, "release").Outputs["candidate"]; got != candidate {
		t.Fatalf("release candidate = %q; want %q", got, candidate)
	}
	if got := actionByName(t, execution, "approve").Outputs["approved"]; got != approved {
		t.Fatalf("approved output = %q; want %q", got, approved)
	}
	if got := actionByName(t, execution, "integrate").Outputs["deployed"]; got != "deployed:"+published.Digest {
		t.Fatalf("deployed output = %q; want the promoted digest", got)
	}

	// The exact same bytes remain served: promotion did not republish or rebuild.
	deliveryRequireRegistryDigest(t, registry, published)
	tagDigest, found := deliveryManifestDigest(registry.BaseURL, published.Repository, published.Tag)
	if !found || tagDigest != published.Digest {
		t.Fatalf("registry tag %s:%s = %q (found=%t); want the built digest %q unchanged",
			published.Repository, published.Tag, tagDigest, found, published.Digest)
	}
	deliveryAssertNoArtifactEntity(t)
}
