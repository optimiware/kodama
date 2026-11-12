package kube

import (
	"encoding/json"
	"strings"
	"testing"

	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
)

// w31Job is the minimal seam Job the driver renders for the pull-secret test.
func w31Job() runtimekube.Job {
	return runtimekube.Job{
		Name:      "kodama-job-w31",
		Namespace: "tenant-acme",
		Spec: runtimekube.JobSpec{
			Image:   "registry.example.com/tools/build@sha256:" + strings.Repeat("a", 64),
			Command: []string{"/bin/sh", "-ec", "echo hi"},
		},
	}
}

// TestBuildJobManifestImagePullSecrets pins the batched pod reference: the
// generated REST body omits pod.spec.imagePullSecrets when the adapter has no
// configured secret, and otherwise carries exactly the configured names in
// order. The reference is a name, never a credential value.
func TestBuildJobManifestImagePullSecrets(t *testing.T) {
	job := w31Job()
	manifest, err := buildJobManifest(job)
	if err != nil {
		t.Fatalf("buildJobManifest returned error: %v", err)
	}
	if manifest.Spec.Template.Spec.ImagePullSecrets != nil {
		t.Fatalf("unconfigured Job body carries imagePullSecrets %v", manifest.Spec.Template.Spec.ImagePullSecrets)
	}
	without, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal unconfigured manifest: %v", err)
	}
	if strings.Contains(string(without), "imagePullSecrets") {
		t.Fatalf("unconfigured Job body contains imagePullSecrets: %s", without)
	}

	job.Spec.ImagePullSecrets = []string{"kodama-registry", "backup-registry"}
	manifest, err = buildJobManifest(job)
	if err != nil {
		t.Fatalf("buildJobManifest with pull secrets returned error: %v", err)
	}
	got := manifest.Spec.Template.Spec.ImagePullSecrets
	if len(got) != 2 {
		t.Fatalf("imagePullSecrets = %v, want two configured names", got)
	}
	for index, name := range []string{"kodama-registry", "backup-registry"} {
		if got[index].Name != name {
			t.Fatalf("imagePullSecrets[%d].name = %q, want %q", index, got[index].Name, name)
		}
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal configured manifest: %v", err)
	}
	var decoded struct {
		Spec struct {
			Template struct {
				Spec struct {
					ImagePullSecrets []struct {
						Name string `json:"name"`
					} `json:"imagePullSecrets"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode configured manifest: %v", err)
	}
	if len(decoded.Spec.Template.Spec.ImagePullSecrets) != 2 ||
		decoded.Spec.Template.Spec.ImagePullSecrets[0].Name != "kodama-registry" {
		t.Fatalf("encoded imagePullSecrets = %+v", decoded.Spec.Template.Spec.ImagePullSecrets)
	}
}

// TestBuildJobManifestRefusesEmptyPullSecretName pins that the driver never
// renders an unnamed reference even if a caller bypasses adapter validation.
func TestBuildJobManifestRefusesEmptyPullSecretName(t *testing.T) {
	job := w31Job()
	job.Spec.ImagePullSecrets = []string{""}
	if _, err := buildJobManifest(job); err == nil {
		t.Fatal("buildJobManifest accepted an empty image pull secret name")
	}
	job.Spec.ImagePullSecrets = []string{"registry", "registry"}
	if _, err := buildJobManifest(job); err == nil {
		t.Fatal("buildJobManifest accepted a duplicate image pull secret name")
	}
}
