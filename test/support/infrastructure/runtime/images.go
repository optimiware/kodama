package runtimesupport

import (
	"context"
	"strings"
	"testing"

	"github.com/optimiware/kodama/test/support/infrastructure/harness"
)

// ImageAlpineTag is the pinned deterministic workload image. The tag is fixed;
// the digest is resolved at run time and recorded as evidence.
const ImageAlpineTag = "docker.io/library/alpine:3.24"

// PinnedImage is a tag resolved to an immutable reference.
type PinnedImage struct {
	Tag    string
	Digest string
	// Reference is the canonical digest-pinned reference passed to the
	// adapter. Podman reports exactly this value as ImageName, so a retry's
	// ownership check matches.
	Reference string
}

// ResolveImage pulls the pinned tag when needed and returns its digest-pinned
// reference. A pull failure follows the shard policy and never passes.
func ResolveImage(ctx context.Context, t testing.TB, podman *harness.Podman, tag string) PinnedImage {
	t.Helper()
	digest, err := podman.EnsureImage(ctx, tag)
	if err != nil {
		harness.SkipOrFail(t, requireInfra(), err)
	}
	repository := tag
	if index := strings.LastIndex(tag, ":"); index > strings.LastIndex(tag, "/") {
		repository = tag[:index]
	}
	reference := repository + "@" + digest
	t.Logf("runtime conformance image %s digest %s reference %s", tag, digest, reference)
	return PinnedImage{Tag: tag, Digest: digest, Reference: reference}
}
