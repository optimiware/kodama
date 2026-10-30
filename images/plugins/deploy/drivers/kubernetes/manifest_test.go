package kubernetes

import (
	"strings"
	"testing"
)

const testManifest = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
  namespace: stage
spec:
  replicas: 1
  selector:
    matchLabels: {app: app}
  template:
    metadata:
      labels: {app: app}
    spec:
      containers:
        - name: app
          image: registry.example.invalid/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
`

func TestManifestValidationAndOwnershipRendering(t *testing.T) {
	objects, err := parseObjects([]byte(testManifest), "stage")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Cluster: "cluster-a", Namespace: "stage", Release: "app",
		Image: "registry.example.invalid/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	if err := validateImages(objects, cfg); err != nil {
		t.Fatal(err)
	}
	rendered, err := render(objects, resourceKey(cfg), "app", "op-1")
	if err != nil {
		t.Fatal(err)
	}
	got := string(rendered)
	for _, want := range []string{"kodama.optimi.com/resource-key: kubernetes/cluster-a/stage/app", "kodama.optimi.com/operation: op-1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered manifest missing %q:\n%s", want, got)
		}
	}
}

func TestManifestRejectsForeignOrClusterResources(t *testing.T) {
	cases := []string{
		strings.Replace(testManifest, "namespace: stage", "namespace: other", 1),
		strings.Replace(testManifest, "kind: Deployment", "kind: ClusterRole", 1),
		strings.Replace(testManifest, "name: app\n  namespace: stage", "generateName: app-\n  namespace: stage", 1),
		strings.Replace(testManifest, "image: registry.example.invalid/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "image: registry.example.invalid/other@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", 1),
	}
	for _, manifest := range cases {
		objects, err := parseObjects([]byte(manifest), "stage")
		if err == nil {
			cfg := Config{Image: "registry.example.invalid/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
			err = validateImages(objects, cfg)
		}
		if err == nil {
			t.Fatalf("accepted invalid manifest:\n%s", manifest)
		}
	}
}

func TestMutableImageRequiresExplicitOptIn(t *testing.T) {
	manifest := strings.Replace(testManifest,
		"registry.example.invalid/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"registry.example.invalid/app:latest", 1)
	objects, err := parseObjects([]byte(manifest), "stage")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Image: "registry.example.invalid/app:latest"}
	if err := validateImages(objects, cfg); err == nil {
		t.Fatal("accepted a mutable image")
	}
	cfg.AllowMutableImage = true
	if err := validateImages(objects, cfg); err != nil {
		t.Fatal(err)
	}
}
