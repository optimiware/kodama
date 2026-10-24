package kit

import (
	"crypto/rand"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/artifacts"
)

func rand_Read(b []byte) (int, error) { return rand.Read(b) }

// Image is a real image pushed to the scope's registry: what the OCI verifier
// fetches by digest, what the host-networked Podman containers run and what the
// kind nodes pull through their containerd mirror.
type Image struct {
	// Repository is host:port/path as repository references spell it.
	Repository string
	Digest     artifacts.Digest
	// NodeRef is the digest-pinned reference the cluster's nodes pull.
	NodeRef string
	// Path is the repository path under the registry (without the host).
	Path string
}

// Ref is the digest-pinned reference a step names.
func (i Image) Ref() string { return i.Repository + "@" + string(i.Digest) }

var (
	toolsOnce sync.Once
	toolsErr  error

	cleanupMu sync.Mutex
	cleanups  []func()
)

// OnCleanup registers what Main removes after the tests: local image tags this
// process created.
func OnCleanup(f func()) {
	cleanupMu.Lock()
	cleanups = append(cleanups, f)
	cleanupMu.Unlock()
}

func runCleanups() {
	cleanupMu.Lock()
	fs := cleanups
	cleanups = nil
	cleanupMu.Unlock()
	for i := len(fs) - 1; i >= 0; i-- {
		fs[i]()
	}
}

// ToolsImage is the image every workspace-consuming step of the M2 scenarios
// runs in: the real deployment-tools image with its `USER` set to root and
// nothing else changed (see rootDerivative). Shipped is the unmodified image.
func ToolsImage(t testing.TB) Image { t.Helper(); return toolsPair(t).root }

// ShippedToolsImage is the real deployment-tools image exactly as built (it runs
// as uid 65532, as its README requires).
func ShippedToolsImage(t testing.TB) Image { t.Helper(); return toolsPair(t).shipped }

type toolsImages struct{ shipped, root Image }

var pairVal toolsImages

func prepareTools() {
	toolsOnce.Do(func() {
		var s Image
		s, toolsErr = buildAndPushTools()
		if toolsErr != nil {
			return
		}
		var r Image
		r, toolsErr = rootDerivative(s)
		pairVal = toolsImages{shipped: s, root: r}
	})
}

func toolsPair(t testing.TB) toolsImages {
	t.Helper()
	prepareTools()
	if toolsErr != nil {
		t.Fatalf("the tools image: %v", toolsErr)
	}
	return pairVal
}

// rootDerivative is the shipped image with `USER 0:0` and nothing else (but a
// per-process label, so two test processes of one scope never share an image
// and one process's cleanup never removes the other's), pushed
// beside it. The Podman runtime mounts the attempt's bootstrap material (the
// admission credential, the runtime CA, the checkout credential) as host-owned
// 0600 files and runs the image's own user, so an image that runs as uid 65532,
// as the deployment tools image does, cannot read them and its entrypoint stops
// ("cannot create runtime client: read CA bundle ... permission denied"; see
// docs/acceptance/m2.md, finding 8). The scripts, packages and every other
// property of the image are the shipped ones; in a rootless container root is
// the host user, with every capability dropped and a read-only root filesystem.
func rootDerivative(shipped Image) (Image, error) {
	provider := os.Getenv("DEV_PROVIDER")
	if provider == "" {
		provider = "podman"
	}
	local := "localhost/kodama-deployment-tools-root:t038-" + randHex(4)
	from := shipped.Repository + "@" + string(shipped.Digest)
	dir, err := os.MkdirTemp(os.TempDir(), "t038-root-")
	if err != nil {
		return Image{}, err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "Containerfile"), []byte("FROM "+from+"\nUSER 0:0\nLABEL kodama.invalid/t038-process="+randHex(8)+"\n"), 0o600); err != nil {
		return Image{}, err
	}
	if _, err := shell(provider, "build", "-q", "--tls-verify=false", "-f", filepath.Join(dir, "Containerfile"), "-t", local, dir); err != nil {
		return Image{}, err
	}
	OnCleanup(func() { _, _ = shell(provider, "rmi", local) })
	return pushImage(provider, local, shipped.Path+"-root")
}

// buildAndPushTools builds the real deployment-tools image with its shipped build
// script (KODAMA_TOOLS_IMAGE names a prebuilt local image and skips the build),
// pushes it to the scope registry and pulls it by digest, so the Podman
// runtime (which speaks https to its registry) finds the digest reference
// locally. It holds git, curl, jq and the apply/delete scripts: every
// workspace-consuming step of the M2 pipelines runs in it.
func buildAndPushTools() (Image, error) {
	raw := os.Getenv("KODAMA_REGISTRY_URL")
	if u, err := url.Parse(raw); err != nil || u.Host == "" || u.Scheme != "http" {
		return Image{}, fmt.Errorf("KODAMA_REGISTRY_URL %q is not the scope's http registry", raw)
	}
	provider := os.Getenv("DEV_PROVIDER")
	if provider == "" {
		provider = "podman"
	}
	local := os.Getenv("KODAMA_TOOLS_IMAGE")
	if local == "" {
		local = "localhost/kodama-deployment-tools:t038-" + randHex(4)
		evidence, err := os.MkdirTemp(os.TempDir(), "t038-evidence-")
		if err != nil {
			return Image{}, err
		}
		OnCleanup(func() { _ = os.RemoveAll(evidence) })
		OnCleanup(func() { _, _ = shell(provider, "rmi", local) })
		cmd := exec.Command(filepath.Join(repoRoot(), "images", "deployment-tools", "build.sh"), "--tag", local, "--out", evidence)
		if b, err := cmd.CombinedOutput(); err != nil {
			return Image{}, fmt.Errorf("build.sh: %w: %s", err, strings.TrimSpace(string(b)))
		}
	}
	return pushImage(provider, local, RegistryRepositoryPrefix()+"/deployment-tools")
}

// pushImage pushes the local image to the scope registry under path and pulls
// it back by digest, so the Podman runtime finds the digest reference locally.
func pushImage(provider, local, path string) (Image, error) {
	u, err := url.Parse(os.Getenv("KODAMA_REGISTRY_URL"))
	if err != nil || u.Host == "" {
		return Image{}, fmt.Errorf("KODAMA_REGISTRY_URL is not an origin")
	}
	remote := u.Host + "/" + path + ":t038"
	if _, err := shell(provider, "tag", local, remote); err != nil {
		return Image{}, err
	}
	OnCleanup(func() { _, _ = shell(provider, "rmi", remote) })
	digestFile := filepath.Join(os.TempDir(), "t038-digest-"+randHex(4))
	defer os.Remove(digestFile)
	if _, err := shell(provider, "push", "--tls-verify=false", "--digestfile", digestFile, remote); err != nil {
		return Image{}, err
	}
	b, err := os.ReadFile(digestFile)
	if err != nil {
		return Image{}, err
	}
	digest := artifacts.Digest(strings.TrimSpace(string(b)))
	if err := digest.Validate(); err != nil {
		return Image{}, fmt.Errorf("push reported digest %q: %w", digest, err)
	}
	if _, err := shell(provider, "pull", "--quiet", "--tls-verify=false", u.Host+"/"+path+"@"+string(digest)); err != nil {
		return Image{}, err
	}
	OnCleanup(func() { _, _ = shell(provider, "rmi", u.Host+"/"+path+"@"+string(digest)) })
	return Image{Repository: u.Host + "/" + path, Path: path, Digest: digest, NodeRef: "localhost:" + u.Port() + "/" + path + "@" + string(digest)}, nil
}
