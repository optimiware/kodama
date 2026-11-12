package kube

import (
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
)

// TestImagePullSecretsAppearOnlyWhenConfigured pins the optional private
// registry reference: the generated Job carries the configured Secret names in
// pod.spec.imagePullSecrets, and an installation with no configured pull secret
// renders no field at all (never a hard-coded or empty entry).
func TestImagePullSecretsAppearOnlyWhenConfigured(t *testing.T) {
	t.Run("unconfigured", func(t *testing.T) {
		fake, _, adapter := testSetup(t)
		request := testPrepareRequest()
		prepareTestWorkload(t, adapter, request)
		startTestWorkload(t, adapter, request.Identity, request.WorkspaceID)

		job := fake.jobsSnapshot()[0]
		if len(job.Spec.ImagePullSecrets) != 0 {
			t.Fatalf("unconfigured Job carries imagePullSecrets %v", job.Spec.ImagePullSecrets)
		}
	})

	t.Run("configured", func(t *testing.T) {
		fake := newFakeCluster()
		addTestNamespace(fake)
		addTestClaim(t, fake, testWorkspaceID())
		clock := newFakeClock()
		config := testConfig(fake, clock)
		config.ImagePullSecrets = []string{"kodama-registry", "backup-registry"}
		adapter, err := New(config)
		if err != nil {
			t.Fatalf("New with image pull secrets returned error: %v", err)
		}
		request := testPrepareRequest()
		prepareTestWorkload(t, adapter, request)
		startTestWorkload(t, adapter, request.Identity, request.WorkspaceID)

		job := fake.jobsSnapshot()[0]
		want := []string{"kodama-registry", "backup-registry"}
		if len(job.Spec.ImagePullSecrets) != len(want) {
			t.Fatalf("Job imagePullSecrets = %v, want %v", job.Spec.ImagePullSecrets, want)
		}
		for index, name := range want {
			if job.Spec.ImagePullSecrets[index] != name {
				t.Fatalf("Job imagePullSecrets[%d] = %q, want %q", index, job.Spec.ImagePullSecrets[index], name)
			}
		}
	})
}

// TestImagePullSecretsRefuseInvalidConfiguration pins fail-closed adapter
// construction: an unusable Secret reference is refused before any cluster
// call, and a duplicate is an ambiguous configuration rather than silently
// deduplicated.
func TestImagePullSecretsRefuseInvalidConfiguration(t *testing.T) {
	cases := []struct {
		name    string
		secrets []string
	}{
		{"uppercase", []string{"Kodama-Registry"}},
		{"underscore", []string{"kodama_registry"}},
		{"empty name", []string{""}},
		{"leading hyphen", []string{"-registry"}},
		{"duplicate", []string{"registry", "registry"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fake := newFakeCluster()
			clock := newFakeClock()
			config := testConfig(fake, clock)
			config.ImagePullSecrets = testCase.secrets
			adapter, err := New(config)
			requireFault(t, err, fault.InvalidInput)
			if adapter != nil {
				t.Fatal("New returned an adapter for an invalid pull secret list")
			}
		})
	}

	// A DNS-1123 subdomain (for example a canonical registry secret name with
	// dots) remains usable.
	fake := newFakeCluster()
	config := testConfig(fake, newFakeClock())
	config.ImagePullSecrets = []string{"kodama.registry"}
	if _, err := New(config); err != nil {
		t.Fatalf("New refused a valid DNS-1123 secret name: %v", err)
	}
}

// TestImagePullSecretsNeverBecomeEnvironment proves the names stay a pod-level
// reference: they are not copied into the workload environment or activation
// snapshot.
func TestImagePullSecretsNeverBecomeEnvironment(t *testing.T) {
	fake := newFakeCluster()
	addTestNamespace(fake)
	addTestClaim(t, fake, testWorkspaceID())
	config := testConfig(fake, newFakeClock())
	config.ImagePullSecrets = []string{"private-registry-credential"}
	adapter, err := New(config)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	request := testPrepareRequest()
	prepareTestWorkload(t, adapter, request)
	startTestWorkload(t, adapter, request.Identity, request.WorkspaceID)

	job := fake.jobsSnapshot()[0]
	for _, env := range job.Spec.Env {
		if strings.Contains(env.Value, "private-registry-credential") {
			t.Fatalf("image pull secret name leaked into environment %q", env.Name)
		}
	}
	for _, configMap := range fake.configMapsSnapshot() {
		for path, data := range configMap.Data {
			if strings.Contains(string(data), "private-registry-credential") {
				t.Fatalf("image pull secret name leaked into activation file %q", path)
			}
		}
	}
}
