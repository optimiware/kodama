package delivery_test

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/test/integration/delivery/stack"
	"github.com/optimiware/kodama/test/support/layer"
)

var (
	imageOnce sync.Once
	imageVal  stack.Image
	imageErr  error
)

// scopeName is the dev scope this run belongs to (the registry namespace).
func scopeName() string {
	for _, k := range []string{"DEV_SCOPE", "KODAMA_RESOURCE_PREFIX"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return stack.Prefix
}

// toolsImage builds the real image with the shipped build script (a failed build
// fails the test; KODAMA_TOOLS_IMAGE names a prebuilt local image and skips the
// build), pushes it to the scope's registry and returns the digest-pinned
// identity the OCI verifier fetches and the kubelet pulls. The registry is the
// scope's plain-HTTP development registry (KODAMA_REGISTRY_URL): the host
// publishes it on 127.0.0.1:<port>, and the kind nodes resolve
// localhost:<port>/<repository> through their containerd mirror.
func toolsImage(t *testing.T) stack.Image {
	t.Helper()
	layer.SkipService(t, "container registry")
	imageOnce.Do(func() { imageVal, imageErr = buildAndPush() })
	if imageErr != nil {
		t.Fatalf("the tools image: %v", imageErr)
	}
	return imageVal
}

func buildAndPush() (stack.Image, error) {
	raw := os.Getenv("KODAMA_REGISTRY_URL")
	if raw == "" {
		return stack.Image{}, fmt.Errorf("KODAMA_REGISTRY_URL is unset: a scope registry is required (make dev-up for this scope)")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Scheme != "http" {
		return stack.Image{}, fmt.Errorf("KODAMA_REGISTRY_URL %q is not the scope's http registry", raw)
	}
	provider := os.Getenv("DEV_PROVIDER")
	if provider == "" {
		provider = "podman"
	}
	local := os.Getenv("KODAMA_TOOLS_IMAGE")
	if local == "" {
		local = "localhost/kodama-deployment-tools:t067-" + randHex(4)
		evidence, err := os.MkdirTemp(os.TempDir(), "t067-evidence-")
		if err != nil {
			return stack.Image{}, err
		}
		onCleanup(func() { _ = os.RemoveAll(evidence) })
		onCleanup(func() { _, _ = shell(provider, "rmi", local) })
		cmd := exec.Command(filepath.Join(toolsDir(), "build.sh"), "--tag", local, "--out", evidence)
		if b, err := cmd.CombinedOutput(); err != nil {
			return stack.Image{}, fmt.Errorf("build.sh: %w: %s", err, strings.TrimSpace(string(b)))
		}
	}
	repo := scopeName() + "/deployment-tools"
	remote := u.Host + "/" + repo + ":t067"
	if _, err := shell(provider, "tag", local, remote); err != nil {
		return stack.Image{}, err
	}
	onCleanup(func() { _, _ = shell(provider, "rmi", remote) })
	digestFile := filepath.Join(os.TempDir(), "t067-digest-"+randHex(4))
	defer os.Remove(digestFile)
	if _, err := shell(provider, "push", "--tls-verify=false", "--digestfile", digestFile, remote); err != nil {
		return stack.Image{}, err
	}
	b, err := os.ReadFile(digestFile)
	if err != nil {
		return stack.Image{}, err
	}
	digest := artifacts.Digest(strings.TrimSpace(string(b)))
	if err := digest.Validate(); err != nil {
		return stack.Image{}, fmt.Errorf("push reported digest %q: %w", digest, err)
	}
	port := u.Port()
	// The Podman runtime creates containers with --pull=missing, which speaks
	// HTTPS: the digest reference the admitted step names must already be local.
	if _, err := shell(provider, "pull", "--quiet", "--tls-verify=false", u.Host+"/"+repo+"@"+string(digest)); err != nil {
		return stack.Image{}, err
	}
	return stack.Image{
		Repository:  u.Host + "/" + repo,
		Digest:      digest,
		RegistryURL: raw,
		// The kubelet resolves localhost:<port> through the node's mirror.
		NodeRef: "localhost:" + port + "/" + repo + "@" + string(digest),
	}, nil
}
