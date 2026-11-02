package kubernetes

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/optimiware/kodama/images/plugins/deploy/driver"
)

func TestObserveRequiresTheVerifiedPrimaryImageLive(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "kubectl")
	script := `#!/bin/sh
cat <<'JSON'
{"metadata":{"name":"app","namespace":"stage","uid":"uid-1","resourceVersion":"7","generation":2,"annotations":{"kodama.optimi.com/resource-key":"kubernetes/cluster-a/stage/app"}},"spec":{"replicas":1,"template":{"spec":{"containers":[{"name":"app","image":"registry.example.invalid/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}}},"status":{"observedGeneration":2,"availableReplicas":1}}
JSON
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Cluster: "cluster-a", Namespace: "stage", Release: "app",
		Image: "registry.example.invalid/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	d := &Driver{Kubectl: bin}
	resources := []driver.Resource{{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "stage", Name: "app"}}
	obs, err := d.observeStates(context.Background(), cfg, resources)
	if err != nil || obs.State != driver.Ready {
		t.Fatalf("ready observation = %+v %v", obs, err)
	}

	// A live workload that only carries an allowed auxiliary image is not the
	// verified primary release.
	script = `#!/bin/sh
cat <<'JSON'
{"metadata":{"name":"app","namespace":"stage","uid":"uid-1","resourceVersion":"7","generation":2,"annotations":{"kodama.optimi.com/resource-key":"kubernetes/cluster-a/stage/app"}},"spec":{"replicas":1,"template":{"spec":{"containers":[{"name":"sidecar","image":"registry.example.invalid/sidecar@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}]}}},"status":{"observedGeneration":2,"availableReplicas":1}}
JSON
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg.AuxiliaryImages = []string{"registry.example.invalid/sidecar@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	obs, err = d.observeStates(context.Background(), cfg, resources)
	if err != nil || obs.State != driver.Unhealthy {
		t.Fatalf("auxiliary-only observation = %+v %v", obs, err)
	}
}
