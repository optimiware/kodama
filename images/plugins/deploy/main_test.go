package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestArtifactDescriptorUsesPinnedDigestAndVerifiesBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bundle.tar.gz")
	if err := os.WriteFile(path, []byte("verified bundle"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum, err := sha256File(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := contextOutputs{ArtifactFiles: []struct {
		Name       string `json:"name"`
		ArtifactID string `json:"artifactId"`
		Digest     string `json:"digest"`
	}{{Name: "bundle", ArtifactID: "art_bundle", Digest: sum}}}

	got, err := artifactDescriptor(ctx, "bundle", path, "fallback")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "art_bundle" || got.Digest != sum || got.Path != path {
		t.Fatalf("descriptor = %+v", got)
	}

	ctx.ArtifactFiles[0].Digest = "sha256:" + "0" + sum[7:]
	if _, err := artifactDescriptor(ctx, "bundle", path, "fallback"); err == nil {
		t.Fatal("accepted a pinned digest that does not match the local bytes")
	}
}

func TestArtifactDescriptorWithoutContextUsesRealContentDigest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.yaml")
	if err := os.WriteFile(path, []byte("kind: Deployment\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := artifactDescriptor(contextOutputs{}, "manifest", path, "manifest")
	if err != nil {
		t.Fatal(err)
	}
	sum, _ := sha256File(path)
	if got.Digest != sum || got.ID != "manifest" {
		t.Fatalf("descriptor = %+v, want digest %s", got, sum)
	}
}
