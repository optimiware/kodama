//go:build component

package messaging_test

import (
	"context"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/integration/registry"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/test/support/infrastructure/harness"
	support "github.com/optimiware/kodama/test/support/infrastructure/messaging"
)

// TestRegistryPushResolveAndPinning pushes a real OCI manifest through the
// pinned registry and proves tag-to-digest resolution, digest pinning
// stability, not-found classification and outage classification.
func TestRegistryPushResolveAndPinning(t *testing.T) {
	support.TrackTest(t)
	instance := support.StartRegistry(t, false)
	ctx := instance.Shard.Context()

	manifest, blobs := support.OCIManifest([]byte(`{"architecture":"amd64","os":"linux"}`), []byte("layer-one"))
	digest := instance.Push(t, "kodama/test", "v1", manifest, blobs)
	if want := support.DigestOf(manifest); digest != want {
		t.Fatalf("pushed digest = %q; want %q", digest, want)
	}
	resolver := instance.Resolver(t, registry.Credential{Scheme: registry.CredentialAnonymous})

	resolved, err := resolver.Resolve(ctx, pipeline.ResolveRequest{Reference: instance.Reference("kodama/test", "v1")})
	if err != nil {
		t.Fatalf("resolve tag: %v", err)
	}
	if resolved.Digest != digest {
		t.Fatalf("resolved digest = %q; want %q", resolved.Digest, digest)
	}
	if resolved.Reference != instance.Host()+"/kodama/test" {
		t.Fatalf("resolved reference = %q; want %q", resolved.Reference, instance.Host()+"/kodama/test")
	}
	if err := resolved.Validate(); err != nil {
		t.Fatalf("resolved image is invalid: %v", err)
	}
	support.Note(t, "registry_manifest_digest", digest)

	pinned, err := resolver.Resolve(ctx, pipeline.ResolveRequest{Reference: instance.Host() + "/kodama/test@" + digest})
	if err != nil {
		t.Fatalf("resolve pinned digest: %v", err)
	}
	if pinned.Digest != digest {
		t.Fatalf("pinned digest = %q; want %q", pinned.Digest, digest)
	}

	// Move the tag to new content: the tag follows the move, the pinned digest
	// stays stable.
	movedManifest, movedBlobs := support.OCIManifest([]byte(`{"architecture":"amd64","os":"linux","moved":true}`), []byte("layer-two"))
	movedDigest := instance.Push(t, "kodama/test", "v1", movedManifest, movedBlobs)
	if movedDigest == digest {
		t.Fatal("moved tag kept the original digest; test content did not change")
	}
	moved, err := resolver.Resolve(ctx, pipeline.ResolveRequest{Reference: instance.Reference("kodama/test", "v1")})
	if err != nil {
		t.Fatalf("resolve moved tag: %v", err)
	}
	if moved.Digest != movedDigest {
		t.Fatalf("moved tag digest = %q; want %q", moved.Digest, movedDigest)
	}
	stillPinned, err := resolver.Resolve(ctx, pipeline.ResolveRequest{Reference: instance.Host() + "/kodama/test@" + digest})
	if err != nil {
		t.Fatalf("resolve original pinned digest after tag move: %v", err)
	}
	if stillPinned.Digest != digest {
		t.Fatalf("pinned digest changed to %q after tag move; want %q", stillPinned.Digest, digest)
	}

	if _, err := resolver.Resolve(ctx, pipeline.ResolveRequest{Reference: instance.Reference("kodama/test", "missing")}); err == nil {
		t.Fatal("missing tag resolved; want not_found")
	} else {
		requireClass(t, err, fault.NotFound)
	}

	if err := instance.Container.Stop(ctx); err != nil {
		t.Fatalf("stop registry: %v", err)
	}
	probe := func(ctx context.Context) error {
		_, err := resolver.Resolve(ctx, pipeline.ResolveRequest{Reference: instance.Reference("kodama/test", "v1")})
		if err == nil {
			return context.Canceled
		}
		class, ok := fault.ClassOf(err)
		if !ok || class != fault.DependencyUnavailable {
			return err
		}
		return nil
	}
	if err := harness.WaitReady(ctx, harness.RealClock{}, "registry stopped", probe, support.FastWaitOptions()); err != nil {
		t.Fatalf("stopped registry was not classified as unavailable: %v", err)
	}
}

// TestRegistryAuthenticationPath proves the adapter's real authentication path
// against the pinned registry: an unauthenticated resolve is rejected as
// unauthenticated and a presented installation credential succeeds.
func TestRegistryAuthenticationPath(t *testing.T) {
	support.TrackTest(t)
	instance := support.StartRegistry(t, true)
	ctx := instance.Shard.Context()

	manifest, blobs := support.OCIManifest([]byte(`{"architecture":"amd64","os":"linux"}`), []byte("auth-layer"))
	digest := instance.Push(t, "kodama/auth", "v1", manifest, blobs)
	reference := instance.Reference("kodama/auth", "v1")

	anonymous := instance.Resolver(t, registry.Credential{Scheme: registry.CredentialAnonymous})
	if _, err := anonymous.Resolve(ctx, pipeline.ResolveRequest{Reference: reference}); err == nil {
		t.Fatal("unauthenticated resolve succeeded; want unauthenticated")
	} else {
		requireClass(t, err, fault.Unauthenticated)
		for _, secret := range []string{"kodama", "test-only"} {
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("registry error leaks a credential value: %q", err.Error())
			}
		}
	}

	authenticated := instance.Resolver(t, support.BasicCredential())
	resolved, err := authenticated.Resolve(ctx, pipeline.ResolveRequest{Reference: reference})
	if err != nil {
		t.Fatalf("authenticated resolve: %v", err)
	}
	if resolved.Digest != digest {
		t.Fatalf("authenticated digest = %q; want %q", resolved.Digest, digest)
	}
	support.Note(t, "registry_auth_path", "anonymous 401; basic accepted")
}
