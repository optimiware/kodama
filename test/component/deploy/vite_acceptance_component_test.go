//go:build component

// Hermetic shape tests for deploy/stage/vite-acceptance.yaml: the checked-in
// inline pipeline must be admitted by the product parser and carry the
// digest-pinned Node image, the 8.3.6 Vite build and the build marker.
package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/shell/pipeline"
)

const viteAcceptanceImage = "docker.io/library/node@sha256:8996bb5821d62a8fd9bb27c1cb07462f3fdce6e47d640c6a881ead0b064ce216"

func TestViteAcceptancePipelineParsesAndPinsTheImage(t *testing.T) {
	path := filepath.Join(repoRoot(t), "deploy", "stage", "vite-acceptance.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	decoded, err := pipeline.Decode(body, pipeline.DefaultLimits)
	if err != nil {
		t.Fatalf("the product pipeline decoder refused the checked-in YAML: %v", err)
	}
	if len(decoded.Actions) != 1 {
		t.Fatalf("actions = %d; want one vite-build action", len(decoded.Actions))
	}
	action := decoded.Actions[0]
	if string(action.Name) != "vite-build" {
		t.Errorf("action name = %q; want vite-build", action.Name)
	}
	if action.Container == nil {
		t.Fatal("vite-build is not a container action")
	}
	if action.Container.Image != viteAcceptanceImage {
		t.Errorf("image = %q; want the digest-pinned node 26.13.0-alpine reference", action.Container.Image)
	}
	if strings.Contains(action.Container.Image, ":26.13.0-alpine") {
		t.Errorf("image carries a tag next to (or instead of) the digest: %q", action.Container.Image)
	}
	if action.Timeout != 10*time.Minute {
		t.Errorf("timeout = %s; want 10m", action.Timeout)
	}

	script := strings.Join(action.Container.Commands, "\n")
	for _, wanted := range []string{
		"/workspace",
		"package.json",
		"vite.config.js",
		"index.html",
		"npm install --no-audit --no-fund",
		"npx vite build",
		"test -s /workspace/dist/index.html",
		"KODAMA_VITE_OK",
		"npx vite --version",
		"8.3.6",
	} {
		if !strings.Contains(script, wanted) {
			t.Errorf("vite-build commands are missing %q:\n%s", wanted, script)
		}
	}
	for _, forbidden := range []string{"kodex.", "kodsvc."} {
		if strings.Contains(script, forbidden) {
			t.Errorf("vite-build commands contain the credential marker %q", forbidden)
		}
	}
}
