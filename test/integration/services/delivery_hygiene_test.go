//go:build integration

package services

// Delivery hygiene: a delivery test must never mutate a shared image and must
// leave no image name behind.
//
// The failure this pins: a publish helper that tags and pushes the shared
// workload image docker.io/library/alpine:3.24 into the per-test fixture
// registries re-encodes the shared manifest, so the host's reported local
// digest does not exist on Docker Hub, and the lifecycle/kind harness, which
// pins that local digest, fails with ErrImagePull. Every created delivery tag
// also stays in the host Podman store.
//
// This proof records the shared base image identity before and after one real
// publish-helper path (build, tag, push, digest pull) and its cleanup, and
// asserts the identity is byte-identical and that no name carrying the test's
// unique prefix remains. It fails against a tag-and-push helper, which mutates
// the base image and removes nothing.

import (
	"reflect"
	"testing"

	"github.com/optimiware/kodama/test/support/factory"
	support "github.com/optimiware/kodama/test/support/infrastructure/messaging"
)

// TestDeliveryPublishHelperLeavesSharedBaseImageUnchanged proves the publish
// path only reads the shared base image and that cleanup removes every exact
// name the path created.
func TestDeliveryPublishHelperLeavesSharedBaseImageUnchanged(t *testing.T) {
	harness := factory.Start(t)
	registry := support.StartRegistry(t, false)
	ctx := registry.Shard.Context()

	base := harness.Image.Tag
	before := deliveryInspectIdentity(t, ctx, base)
	if before.ID == "" || before.Digest == "" {
		t.Fatalf("shared base image %s has no recorded identity: %+v", base, before)
	}

	published := deliveryPublishImage(t, registry, harness.Image,
		"kodama/delivery-hygiene", "build-1")

	// The published image must be this test's own image, not the shared base
	// under another name.
	if published.SourceImageID == before.ID {
		t.Fatalf("published source image ID %s equals the shared base image ID; the helper published the shared image",
			published.SourceImageID)
	}
	if published.Marker == "" || published.LocalImage == "" || published.Pinned == "" {
		t.Fatalf("publish helper returned an incomplete publication: %+v", published)
	}

	// Run the same cleanup the helper registered, so the assertions below cover
	// the post-cleanup store rather than waiting for test teardown.
	deliveryCleanupPublished(t, published)

	after := deliveryInspectIdentity(t, ctx, base)
	if before != after {
		t.Fatalf("shared base image %s changed across the publish path and cleanup:\n before: id=%s digest=%s repo_digests=%s repo_tags=%s\n after:  id=%s digest=%s repo_digests=%s repo_tags=%s",
			base,
			before.ID, before.Digest, before.RepoDigests, before.RepoTags,
			after.ID, after.Digest, after.RepoDigests, after.RepoTags)
	}

	leftovers, err := deliveryLocalImageNames(ctx, published.Prefix)
	if err != nil {
		t.Fatalf("list local images with the test prefix %q: %v", published.Prefix, err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("cleanup left %d local image name(s) with prefix %q: %v", len(leftovers), published.Prefix, leftovers)
	}
}

// TestDeliveryCleanupNeverForcesRemoval pins the cleanup command: every exact
// name this helper owns is removed with a plain `rmi`. A forced removal could
// delete an image a concurrent lane still uses and would hide the real owner.
func TestDeliveryCleanupNeverForcesRemoval(t *testing.T) {
	name := "localhost/delivery-cleanup-pin:source"
	if got, want := deliveryRemoveImageArgs(name), []string{"rmi", name}; !reflect.DeepEqual(got, want) {
		t.Fatalf("delivery removal args = %v; want %v", got, want)
	}
}
