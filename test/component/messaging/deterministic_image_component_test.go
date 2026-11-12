//go:build component

package messaging_test

import (
	"context"
	"strings"
	"testing"
	"time"

	support "github.com/optimiware/kodama/test/support/infrastructure/messaging"
)

// startFixtureEnvironment starts the real local registry and resolves the
// fixture's base image exactly once. Every later build receives that immutable
// repository@digest reference, never the mutable tag.
func startFixtureEnvironment(t *testing.T) (*support.Registry, *support.FixtureBuilder, support.FixtureBase) {
	t.Helper()
	instance := support.StartRegistry(t, false)
	ctx := instance.Shard.Context()
	// Resolve the tag exactly once (one pull at most); the committed
	// Containerfile pin must still describe the digest the registry serves, or
	// the fixture has silently moved off its Git-capable base.
	digest := instance.Shard.RequireImage(ctx, support.FixtureBaseTag)
	if digest != support.FixtureBaseDigest {
		t.Fatalf("fixture base %s resolved to %s; the Containerfile pin is %s", support.FixtureBaseTag, digest, support.FixtureBaseDigest)
	}
	base, err := support.NewFixtureBase(support.FixtureBaseTag, digest)
	if err != nil {
		t.Fatalf("build immutable fixture base: %v", err)
	}
	builder := support.NewFixtureBuilder(support.WithFixtureLogger(t.Logf))
	return instance, builder, base
}

// cleanupFixtureImages removes every name the fixture created or pulled and
// then proves, with a read-only podman images query on this shard's unique
// prefixes, that nothing is left behind. A hard removal failure is reported,
// not swallowed; only "image not known" is tolerated by Cleanup.
func cleanupFixtureImages(t *testing.T, builder *support.FixtureBuilder, prefixes ...string) {
	t.Helper()
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	err := builder.Cleanup(cleanupCtx)
	cancel()
	if err != nil {
		t.Errorf("remove fixture images: %v", err)
	}
	queryCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, prefix := range prefixes {
		names, err := builder.LocalImageNames(queryCtx, prefix)
		if err != nil {
			t.Errorf("list fixture images with prefix %q: %v", prefix, err)
			continue
		}
		if len(names) > 0 {
			t.Errorf("fixture images with prefix %q remain after cleanup: %v", prefix, names)
		}
	}
}

// TestFixtureImageRebuildHasSameManifestDigest proves the fixture is a real,
// reproducible OCI artifact, not a synthetic manifest: two clean builds of the
// same normalized inputs, separated in wall-clock time, publish the same
// registry manifest digest, while changing the marker changes it. The digest
// is validated against the registry's own manifest bytes, so a local image ID
// cannot stand in for it.
func TestFixtureImageRebuildHasSameManifestDigest(t *testing.T) {
	support.TrackTest(t)
	instance, builder, base := startFixtureEnvironment(t)
	ctx := instance.Shard.Context()
	repository := support.FixtureRepository(instance.Shard.ID, "rebuild")
	inputs := support.FixtureInputs{Marker: "fixture-alpha\n"}

	firstTag := support.FixtureLocalTag(instance.Shard.ID, "first")
	secondTag := support.FixtureLocalTag(instance.Shard.ID, "second")
	changedTag := support.FixtureLocalTag(instance.Shard.ID, "changed")
	t.Cleanup(func() {
		cleanupFixtureImages(t, builder,
			support.FixtureLocalPrefix(instance.Shard.ID),
			instance.Host()+"/"+repository,
		)
	})

	first, err := builder.Build(ctx, base, inputs, firstTag)
	if err != nil {
		t.Fatalf("build fixture first time: %v", err)
	}
	firstPublished, err := builder.Publish(ctx, instance, first, repository, "first")
	if err != nil {
		t.Fatalf("publish fixture first: %v", err)
	}
	if firstPublished.ManifestDigest == "" {
		t.Fatal("first fixture publication has no manifest digest")
	}
	if want := instance.Host() + "/" + repository + "@" + firstPublished.ManifestDigest; firstPublished.RunnableReference != want {
		t.Fatalf("runnable reference = %q; want the digest-pinned %q", firstPublished.RunnableReference, want)
	}
	if first.Base.Reference != base.Reference {
		t.Fatalf("build base = %q; want the single resolved immutable base %q", first.Base.Reference, base.Reference)
	}
	if !strings.Contains(base.Reference, "@sha256:") {
		t.Fatalf("fixture base reference %q is not immutable", base.Reference)
	}
	if first.Toolchain == "" {
		t.Fatal("build did not record the build-tool version")
	}
	if firstPublished.RegistryManifestDigest != firstPublished.ManifestDigest {
		t.Fatalf("registry manifest digest = %q; want the pushed %q", firstPublished.RegistryManifestDigest, firstPublished.ManifestDigest)
	}
	if firstPublished.ManifestDigest == first.ImageID {
		t.Fatalf("manifest digest equals the local image ID %q; the digest must come from the pushed manifest", first.ImageID)
	}

	// A second clean build after a deliberate wall-clock gap: an image config
	// that carried an ambient creation time or layer mtime would differ here.
	time.Sleep(1200 * time.Millisecond)
	second, err := builder.Build(ctx, base, inputs, secondTag)
	if err != nil {
		t.Fatalf("build fixture second time: %v", err)
	}
	if second.Inputs.Descriptor != first.Inputs.Descriptor {
		t.Fatalf("normalized build inputs changed between builds: %s != %s", first.Inputs.Descriptor, second.Inputs.Descriptor)
	}
	secondPublished, err := builder.Publish(ctx, instance, second, repository, "second")
	if err != nil {
		t.Fatalf("publish fixture second: %v", err)
	}
	if secondPublished.ManifestDigest != firstPublished.ManifestDigest {
		t.Fatalf("rebuild manifest digest = %q; want the first build's %q", secondPublished.ManifestDigest, firstPublished.ManifestDigest)
	}
	if second.Toolchain != first.Toolchain {
		t.Fatalf("rebuild toolchain = %q; want the first build's %q", second.Toolchain, first.Toolchain)
	}
	if secondPublished.RegistryManifestDigest != firstPublished.ManifestDigest {
		t.Fatalf("second registry manifest digest = %q; want %q", secondPublished.RegistryManifestDigest, firstPublished.ManifestDigest)
	}
	if second.ImageID != first.ImageID {
		t.Fatalf("rebuild local image id = %q; want %q", second.ImageID, first.ImageID)
	}

	// The registry's tag manifest must hash to the recorded digest too, not
	// merely echo a Docker-Content-Digest header.
	tagDigest, err := support.RegistryManifestDigest(ctx, instance, repository, "first")
	if err != nil {
		t.Fatalf("resolve first tag manifest identity: %v", err)
	}
	if tagDigest != firstPublished.ManifestDigest {
		t.Fatalf("registry tag digest = %q; want %q", tagDigest, firstPublished.ManifestDigest)
	}

	// Fixture content is part of the artifact identity.
	changed, err := builder.Build(ctx, base, support.FixtureInputs{Marker: "fixture-beta\n"}, changedTag)
	if err != nil {
		t.Fatalf("build changed fixture: %v", err)
	}
	changedPublished, err := builder.Publish(ctx, instance, changed, repository, "changed")
	if err != nil {
		t.Fatalf("publish changed fixture: %v", err)
	}
	if changedPublished.ManifestDigest == firstPublished.ManifestDigest {
		t.Fatalf("changed marker kept manifest digest %q; fixture content was ignored", firstPublished.ManifestDigest)
	}
	if changed.ImageID == first.ImageID {
		t.Fatalf("changed marker kept local image id %q; fixture content was ignored", first.ImageID)
	}

	support.Note(t, "fixture_source", first.Inputs.Descriptor)
	support.Note(t, "fixture_base", base.Reference)
	support.Note(t, "fixture_toolchain", first.Toolchain)
	support.Note(t, "fixture_manifest_digest", firstPublished.ManifestDigest)
	support.Note(t, "fixture_runnable_reference", firstPublished.RunnableReference)
	support.Note(t, "fixture_changed_manifest_digest", changedPublished.ManifestDigest)
	support.RecordImage(firstPublished.RunnableReference, firstPublished.ManifestDigest, "deterministic fixture")
}

// TestRegistryFixtureRunsAdmittedDigestAfterTagRetarget proves the published
// fixture is runnable and that a retained manifest digest is immune to a tag
// retarget: after the tag moves from A to B, launching A by digest still runs
// A while launching the tag runs B. The tag's registry identity is checked as
// a positive control, so a retarget that silently failed cannot make the test
// pass.
func TestRegistryFixtureRunsAdmittedDigestAfterTagRetarget(t *testing.T) {
	support.TrackTest(t)
	instance, builder, base := startFixtureEnvironment(t)
	ctx := instance.Shard.Context()
	repository := support.FixtureRepository(instance.Shard.ID, "retarget")

	firstTag := support.FixtureLocalTag(instance.Shard.ID, "a")
	secondTag := support.FixtureLocalTag(instance.Shard.ID, "b")
	t.Cleanup(func() {
		cleanupFixtureImages(t, builder,
			support.FixtureLocalPrefix(instance.Shard.ID),
			instance.Host()+"/"+repository,
		)
	})

	first, err := builder.Build(ctx, base, support.FixtureInputs{Marker: "fixture-A\n"}, firstTag)
	if err != nil {
		t.Fatalf("build fixture A: %v", err)
	}
	publishedA, err := builder.Publish(ctx, instance, first, repository, "current")
	if err != nil {
		t.Fatalf("publish fixture A: %v", err)
	}
	if !strings.Contains(publishedA.RunnableReference, "@"+publishedA.ManifestDigest) {
		t.Fatalf("runnable reference %q is not pinned to manifest digest %q", publishedA.RunnableReference, publishedA.ManifestDigest)
	}

	marker, err := builder.RunMarker(ctx, instance, publishedA.RunnableReference)
	if err != nil {
		t.Fatalf("run fixture A by admitted digest: %v", err)
	}
	if marker != "fixture-A" {
		t.Fatalf("run by digest observed marker %q; want %q", marker, "fixture-A")
	}

	second, err := builder.Build(ctx, base, support.FixtureInputs{Marker: "fixture-B\n"}, secondTag)
	if err != nil {
		t.Fatalf("build fixture B: %v", err)
	}
	publishedB, err := builder.Publish(ctx, instance, second, repository, "current")
	if err != nil {
		t.Fatalf("publish fixture B over the current tag: %v", err)
	}
	if publishedB.ManifestDigest == publishedA.ManifestDigest {
		t.Fatalf("retarget manifest digest = %q; want different content", publishedA.ManifestDigest)
	}

	// Positive control: the registry tag now really resolves to B.
	tagDigest, err := support.RegistryManifestDigest(ctx, instance, repository, "current")
	if err != nil {
		t.Fatalf("resolve retargeted tag: %v", err)
	}
	if tagDigest != publishedB.ManifestDigest {
		t.Fatalf("retargeted tag digest = %q; want B %q", tagDigest, publishedB.ManifestDigest)
	}

	// The admitted digest still runs A, through the real registry.
	marker, err = builder.RunMarker(ctx, instance, publishedA.RunnableReference)
	if err != nil {
		t.Fatalf("run retained digest A after retarget: %v", err)
	}
	if marker != "fixture-A" {
		t.Fatalf("retained digest observed marker %q after retarget; want %q", marker, "fixture-A")
	}

	// The tag, by contrast, now runs B.
	byTag, err := builder.RunMarker(ctx, instance, instance.Host()+"/"+repository+":current")
	if err != nil {
		t.Fatalf("run retargeted tag: %v", err)
	}
	if byTag != "fixture-B" {
		t.Fatalf("retargeted tag observed marker %q; want %q", byTag, "fixture-B")
	}
	if byDigestB, err := builder.RunMarker(ctx, instance, publishedB.RunnableReference); err != nil {
		t.Fatalf("run B by digest: %v", err)
	} else if byDigestB != "fixture-B" {
		t.Fatalf("B by digest observed marker %q; want %q", byDigestB, "fixture-B")
	}

	support.Note(t, "fixture_base", base.Reference)
	support.Note(t, "fixture_toolchain", first.Toolchain)
	support.Note(t, "fixture_a_digest", publishedA.ManifestDigest)
	support.Note(t, "fixture_b_digest", publishedB.ManifestDigest)
	support.Note(t, "fixture_a_runnable_reference", publishedA.RunnableReference)
	support.Note(t, "fixture_tag_reference", instance.Host()+"/"+repository+":current")
}

// TestFixtureImageProvidesGit proves the fixture really is the Git-capable
// image the protected factory will compose: a real build through the fixture
// build path, from the single digest-pinned Containerfile base, runs
// `git --version` and reports a git version. The base is resolved and pinned
// once by startFixtureEnvironment; the probe itself never pulls, so the base
// pull count stays at one.
func TestFixtureImageProvidesGit(t *testing.T) {
	support.TrackTest(t)
	instance, builder, base := startFixtureEnvironment(t)
	ctx := instance.Shard.Context()
	tag := support.FixtureLocalTag(instance.Shard.ID, "git")
	t.Cleanup(func() {
		cleanupFixtureImages(t, builder, support.FixtureLocalPrefix(instance.Shard.ID))
	})

	image, err := builder.Build(ctx, base, support.FixtureInputs{Marker: "fixture-git\n"}, tag)
	if err != nil {
		t.Fatalf("build git-capable fixture: %v", err)
	}
	if image.Base.Reference != base.Reference {
		t.Fatalf("fixture base = %q; want the single pinned %q", image.Base.Reference, base.Reference)
	}
	version, err := builder.GitVersion(ctx, image.Tag)
	if err != nil {
		t.Fatalf("run git in the fixture image: %v", err)
	}
	if !strings.HasPrefix(version, "git version ") {
		t.Fatalf("fixture git probe returned %q; want a git version line", version)
	}
	if strings.HasSuffix(version, "git version ") {
		t.Fatalf("fixture git probe returned no version number: %q", version)
	}

	support.Note(t, "fixture_base", base.Reference)
	support.Note(t, "fixture_source", image.Inputs.Descriptor)
	support.Note(t, "fixture_git_version", version)
}
