package nats

// The deployment bootstrap obtains its account issuer pair from
// tools/nats-auth-key, while this package decodes the seed and signs callout
// grants with it. Both sides copy the NKey framing and pin it with independent
// known vectors; this test closes the loop by decoding a tool-generated seed
// with the product codec. A divergence that leaves each implementation
// self-consistent would otherwise only appear when a stage broker rejects the
// callout it is sent.

import (
	"crypto/ed25519"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// nkeyToolModuleRoot walks up from the package directory to the module root,
// because the tool is a command package outside this directory.
func nkeyToolModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// TestToolGeneratedSeedDecodesWithCodec proves the pair scripts/bootstrap-secrets
// writes is usable by the Shell callout codec: the seed decodes as an account
// seed, its public half equals the public file, and it signs a grant the
// matching public key verifies.
func TestToolGeneratedSeedDecodesWithCodec(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("the go toolchain is required to build tools/nats-auth-key: %v", err)
	}
	dir := t.TempDir()
	seedFile := filepath.Join(dir, "issuer.seed")
	publicFile := filepath.Join(dir, "issuer.pub")
	command := exec.Command("go", "run", "./tools/nats-auth-key",
		"-seed-file", seedFile, "-public-file", publicFile)
	command.Dir = nkeyToolModuleRoot(t)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("run tools/nats-auth-key: %v\n%s", err, output)
	}
	seedBytes, err := os.ReadFile(seedFile)
	if err != nil {
		t.Fatalf("read generated seed: %v", err)
	}
	publicBytes, err := os.ReadFile(publicFile)
	if err != nil {
		t.Fatalf("read generated public key: %v", err)
	}
	if len(seedBytes) == 0 || len(publicBytes) == 0 {
		t.Fatal("tools/nats-auth-key wrote an empty file")
	}

	private, public, err := decodeAccountSeed(string(seedBytes))
	if err != nil {
		t.Fatalf("decode tool-generated seed with the callout codec: %v", err)
	}
	if public != string(publicBytes) {
		t.Fatalf("codec-derived public key = %q; tool wrote %q", public, publicBytes)
	}

	_, signingKey, err := nkeyPublic(string(publicBytes))
	if err != nil {
		t.Fatalf("decode tool-generated public key with the callout codec: %v", err)
	}
	message := []byte("kodama callout grant")
	signature := ed25519.Sign(private, message)
	if !ed25519.Verify(signingKey, message, signature) {
		t.Fatal("the tool-generated seed does not sign for the public key it wrote")
	}
}
