// Workload API trust: installation-provided CA projection.
//
// This file pins the adapter-local configuration and manifest shape of the
// workload CA projection, declared in api.go and manifest.go:
//
//	const WorkloadCAKey       = "ca.crt"          // literal Secret key, never encoded
//	const WorkloadCAMountPath = "/etc/kodama/ca"  // fixed read-only mount root
//	const WorkloadCAPath      = "/etc/kodama/ca/ca.crt"
//
//	type WorkloadCAConfig struct {
//	    SecretName string // tenant-local, CA-only Secret; empty disables the mount
//	}
//	Config.WorkloadCA WorkloadCAConfig
//
// The trust bundle is an ordinary Secret volume: exactly one item (the literal
// key ca.crt) projected read-only at the fixed path, outside /workspace, the
// activation snapshot (/kodama) and the credential mount (/kodama/secrets). The
// adapter neither reads nor re-materializes the Secret, and adds no environment
// variable: smoke workloads pass --cacert explicitly. The serving private key
// stays in the control-plane Secret and is never projected into a workload.
package kube

import (
	"context"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/workspace"
)

const (
	// testWorkloadCASecret is the installation-provided, tenant-local CA-only
	// Secret name used by the fixtures.
	testWorkloadCASecret = "kodama-workload-ca"
	// testCAPEM is an obviously fake CA certificate. The adapter projects the
	// bytes; it never parses them.
	testCAPEM = "-----BEGIN CERTIFICATE-----\nMIIB-fake-workload-ca\n-----END CERTIFICATE-----\n"
	// testPrivateKeyPEM is an obviously fake serving private key. It models a
	// misconfigured CA bundle that also carries the control-plane key: the
	// adapter must project only ca.crt.
	testPrivateKeyPEM = "-----BEGIN PRIVATE KEY-----\nMIIE-fake-serving-key\n-----END PRIVATE KEY-----\n"
)

// testSetupWithWorkloadCA is testSetup plus the configured CA-only Secret.
func testSetupWithWorkloadCA(t *testing.T, secretName string) (*fakeCluster, *fakeClock, *Adapter) {
	t.Helper()
	fake := newFakeCluster()
	addTestNamespace(fake)
	addTestClaim(t, fake, testWorkspaceID())
	clock := newFakeClock()
	return fake, clock, newWorkloadCAAdapter(t, fake, clock, secretName)
}

func newWorkloadCAAdapter(t *testing.T, fake *fakeCluster, clock *fakeClock, secretName string) *Adapter {
	t.Helper()
	config := testConfig(fake, clock)
	config.WorkloadCA = WorkloadCAConfig{SecretName: secretName}
	adapter, err := New(config)
	if err != nil {
		t.Fatalf("New with workload CA %q returned error: %v", secretName, err)
	}
	return adapter
}

// TestWorkloadCAMountUsesTrustedConfiguration pins the exact read-only
// projection: one Secret volume carrying only the literal ca.crt key at the
// fixed trust path, with no extra environment variable and no projection when
// the configuration is absent.
func TestWorkloadCAMountUsesTrustedConfiguration(t *testing.T) {
	if WorkloadCAKey != "ca.crt" {
		t.Fatalf("WorkloadCAKey = %q; want the literal Secret key %q", WorkloadCAKey, "ca.crt")
	}
	if WorkloadCAMountPath != "/etc/kodama/ca" {
		t.Fatalf("WorkloadCAMountPath = %q; want the fixed trust root %q", WorkloadCAMountPath, "/etc/kodama/ca")
	}
	if WorkloadCAPath != "/etc/kodama/ca/ca.crt" {
		t.Fatalf("WorkloadCAPath = %q; want %q", WorkloadCAPath, "/etc/kodama/ca/ca.crt")
	}

	fake, _, adapter := testSetupWithWorkloadCA(t, testWorkloadCASecret)
	request := testPrepareRequest()
	prepareTestWorkload(t, adapter, request)
	startTestWorkload(t, adapter, request.Identity, request.WorkspaceID)

	job := fake.jobsSnapshot()[0]
	volume := testVolumeAt(t, job.Spec, WorkloadCAMountPath)
	if volume.SecretName != testWorkloadCASecret {
		t.Errorf("CA volume secret = %q; want the configured Secret %q", volume.SecretName, testWorkloadCASecret)
	}
	if volume.ConfigMapName != "" || volume.ClaimName != "" {
		t.Errorf("CA volume source = {configMap:%q claim:%q}; want exactly one Secret source", volume.ConfigMapName, volume.ClaimName)
	}
	if !volume.ReadOnly {
		t.Error("CA volume is writable; installation trust must be read-only")
	}
	if len(volume.Items) != 1 {
		t.Fatalf("CA volume has %d items; want exactly the single ca.crt item: %+v", len(volume.Items), volume.Items)
	}
	item := volume.Items[0]
	if item.Key != WorkloadCAKey {
		t.Errorf("CA volume item key = %q; want the literal Secret key %q (Secret keys are not encoded)", item.Key, WorkloadCAKey)
	}
	if item.Path != WorkloadCAPath {
		t.Errorf("CA volume item path = %q; want the fixed path %q", item.Path, WorkloadCAPath)
	}
	if item.Mode != readOnlyMode || item.Mode&0o222 != 0 {
		t.Errorf("CA volume item mode = %#o; want read-only %#o", item.Mode, readOnlyMode)
	}

	// The trust bundle must not become an unshadowable standard environment
	// variable; workloads opt in with an explicit --cacert.
	for _, name := range []string{
		"KODAMA_CA",
		"KODAMA_CA_PATH",
		"SSL_CERT_FILE",
		"SSL_CERT_DIR",
		"REQUESTS_CA_BUNDLE",
		"NODE_EXTRA_CA_CERTS",
	} {
		if value, ok := testEnvValue(job.Spec.Env, name); ok {
			t.Errorf("workload CA injected environment %s=%q; smoke workloads use an explicit --cacert", name, value)
		}
	}

	// Without configuration the adapter mounts no trust bundle at all.
	plainFake, _, plainAdapter := testSetup(t)
	plainRequest := testPrepareRequest()
	prepareTestWorkload(t, plainAdapter, plainRequest)
	startTestWorkload(t, plainAdapter, plainRequest.Identity, plainRequest.WorkspaceID)
	plainJob := plainFake.jobsSnapshot()[0]
	for _, candidate := range plainJob.Spec.Volumes {
		if candidate.MountPath == WorkloadCAMountPath {
			t.Fatalf("unconfigured adapter mounted a workload CA at %q: %+v", WorkloadCAMountPath, candidate)
		}
	}
}

// TestWorkloadCAMountIsConfined proves the trust mount can never overlap the
// writable workspace, the reserved execution credential or the activation
// snapshot, and that an unusable Secret name is refused before any cluster
// call.
func TestWorkloadCAMountIsConfined(t *testing.T) {
	fake, _, adapter := testSetupWithWorkloadCA(t, testWorkloadCASecret)
	request := testPrepareRequest()
	prepareTestWorkload(t, adapter, request)
	startTestWorkload(t, adapter, request.Identity, request.WorkspaceID)
	job := fake.jobsSnapshot()[0]

	volume := testVolumeAt(t, job.Spec, WorkloadCAMountPath)
	if len(volume.Items) != 1 {
		t.Fatalf("CA volume has %d items; want exactly 1", len(volume.Items))
	}
	item := volume.Items[0]

	reserved := []string{
		workspace.WorkspaceRoot,
		activationRoot,
		workspace.SecretRoot,
		workspace.ReservedCredentialPath,
	}
	for _, path := range reserved {
		if pathContains(volume.MountPath, path) || pathContains(path, volume.MountPath) {
			t.Errorf("CA mount %q overlaps reserved path %q", volume.MountPath, path)
		}
		if pathContains(item.Path, path) || pathContains(path, item.Path) {
			t.Errorf("CA item %q overlaps reserved path %q", item.Path, path)
		}
	}
	if !pathContains(volume.MountPath, item.Path) {
		t.Errorf("CA item path %q is not under its mount %q", item.Path, volume.MountPath)
	}
	if err := workspace.ValidatePath(item.Path); err == nil {
		t.Errorf("CA item path %q is an activation-materialized path; it must live outside the /kodama roots", item.Path)
	}
	if item.Path == workspace.ReservedCredentialPath {
		t.Errorf("CA item path is the reserved execution credential %q", workspace.ReservedCredentialPath)
	}

	seen := map[string]bool{}
	for _, candidate := range job.Spec.Volumes {
		if candidate.Name == "" {
			t.Errorf("volume mounted at %q has no name", candidate.MountPath)
		}
		if seen[candidate.Name] {
			t.Errorf("duplicate volume name %q", candidate.Name)
		}
		seen[candidate.Name] = true
	}

	invalidNames := []string{
		// Path/identifier escapes and characters outside the DNS-1123
		// subdomain alphabet.
		"../escape",
		"Bad Name",
		"ca_secret",
		"/etc/passwd",
		"   ",
		" a",
		"a\n",
		"é",
		"CA-Secret",
		// Leading/trailing hyphen and empty labels.
		"-ca",
		"ca-",
		"a..b",
		".a",
		"a.",
		"a.-b",
		"a-.b",
		"..",
		// Length boundaries: a 254-character name as one label, a
		// 254-character name of individually valid labels (so only the
		// subdomain cap can reject it), and a 64-character label.
		strings.Repeat("a", 254),
		strings.Join([]string{
			strings.Repeat("a", 63),
			strings.Repeat("a", 63),
			strings.Repeat("a", 63),
			strings.Repeat("a", 62),
		}, "."),
		strings.Repeat("a", 64),
	}
	for _, name := range invalidNames {
		t.Run("invalid secret name "+name, func(t *testing.T) {
			invalidFake := newFakeCluster()
			config := testConfig(invalidFake, newFakeClock())
			config.WorkloadCA = WorkloadCAConfig{SecretName: name}
			_, err := New(config)
			if err == nil {
				t.Fatalf("New accepted CA Secret name %q", name)
			}
			class, ok := fault.ClassOf(err)
			if !ok || class != fault.InvalidInput {
				t.Fatalf("New(%q) error class = %v (present=%v); want %q", name, class, ok, fault.InvalidInput)
			}
			if calls := invalidFake.callTotal(); calls != 0 {
				t.Fatalf("New(%q) made %d cluster calls; configuration is validated locally", name, calls)
			}
		})
	}

	validNames := []string{
		"a",
		"0a",
		"a.b",
		"ca.example",
		"kodama-workload-ca",
		// 63-character label: the label length boundary.
		strings.Repeat("a", 63),
		// 253-character name with valid labels: the subdomain length boundary.
		strings.Join([]string{
			strings.Repeat("a", 63),
			strings.Repeat("a", 63),
			strings.Repeat("a", 63),
			strings.Repeat("a", 61),
		}, "."),
	}
	for _, name := range validNames {
		t.Run("valid secret name "+name, func(t *testing.T) {
			fake, _, adapter := testSetupWithWorkloadCA(t, name)
			request := testPrepareRequest()
			prepareTestWorkload(t, adapter, request)
			startTestWorkload(t, adapter, request.Identity, request.WorkspaceID)

			job := fake.jobsSnapshot()[0]
			volume := testVolumeAt(t, job.Spec, WorkloadCAMountPath)
			if volume.SecretName != name {
				t.Fatalf("CA volume secret = %q; want the configured Secret %q", volume.SecretName, name)
			}
			if len(volume.Items) != 1 || volume.Items[0].Key != WorkloadCAKey {
				t.Fatalf("CA volume items = %+v; want exactly the literal %q item", volume.Items, WorkloadCAKey)
			}
		})
	}
}

// TestWorkloadTrustDoesNotExposeControlPlaneCredentials proves the projection
// is CA-only: even when the configured Secret also carries a serving private
// key, only ca.crt is mounted; the credential Secret keeps only declared
// secrets plus KODAMA_TOKEN; the control-plane Secret is never referenced; and
// the service-account token is still not automounted.
func TestWorkloadTrustDoesNotExposeControlPlaneCredentials(t *testing.T) {
	fake, _, adapter := testSetupWithWorkloadCA(t, testWorkloadCASecret)

	// A misconfigured CA bundle that also carries the serving key. The adapter
	// must project exactly ca.crt and nothing else.
	if _, err := fake.PutSecret(context.Background(), Secret{
		Name:      testWorkloadCASecret,
		Namespace: testNamespace,
		Data: map[string][]byte{
			WorkloadCAKey: []byte(testCAPEM),
			"tls.key":     []byte(testPrivateKeyPEM),
			"tls.crt":     []byte("-----BEGIN CERTIFICATE-----\nMIIB-fake-serving-cert\n-----END CERTIFICATE-----\n"),
		},
	}); err != nil {
		t.Fatalf("seed CA Secret: %v", err)
	}

	const controlPlaneSecret = "kodama-control-plane-tls"
	if _, err := fake.PutSecret(context.Background(), Secret{
		Name:      controlPlaneSecret,
		Namespace: testNamespace,
		Data: map[string][]byte{
			"tls.key": []byte(testPrivateKeyPEM),
			"tls.crt": []byte("-----BEGIN CERTIFICATE-----\nMIIB-fake-serving-cert\n-----END CERTIFICATE-----\n"),
		},
	}); err != nil {
		t.Fatalf("seed control-plane Secret: %v", err)
	}

	request := testPrepareRequest()
	prepareTestWorkload(t, adapter, request)
	startTestWorkload(t, adapter, request.Identity, request.WorkspaceID)
	job := fake.jobsSnapshot()[0]

	caVolume := testVolumeAt(t, job.Spec, WorkloadCAMountPath)
	if len(caVolume.Items) != 1 || caVolume.Items[0].Key != WorkloadCAKey {
		t.Fatalf("CA volume items = %+v; want exactly the literal %q item", caVolume.Items, WorkloadCAKey)
	}
	for _, item := range caVolume.Items {
		if item.Key == "tls.key" || item.Key == "tls.crt" {
			t.Fatalf("CA volume projects %q; only ca.crt may be mounted", item.Key)
		}
	}

	for _, volume := range job.Spec.Volumes {
		if volume.SecretName == controlPlaneSecret {
			t.Fatalf("job references the control-plane Secret %q", controlPlaneSecret)
		}
		if volume.Name == controlPlaneSecret || volume.ClaimName == controlPlaneSecret {
			t.Fatalf("job volume %q references the control-plane Secret", volume.Name)
		}
	}

	// The mutable credential Secret is still exactly the declared secret plus
	// the execution credential.
	var credential *Secret
	for _, secret := range fake.secretsSnapshot() {
		if secret.Name == testWorkloadCASecret || secret.Name == controlPlaneSecret {
			continue
		}
		if secret.Labels[LabelWorkload] == string(request.Identity.WorkloadID) {
			copied := secret
			credential = &copied
			break
		}
	}
	if credential == nil {
		t.Fatal("adapter did not create the workload credential Secret")
	}
	wantKeys := map[string]bool{"API_KEY": true, "KODAMA_TOKEN": true}
	if len(credential.Data) != len(wantKeys) {
		t.Fatalf("credential Secret keys = %v; want exactly %v", credentialKeys(credential.Data), wantKeys)
	}
	for key, value := range credential.Data {
		if !wantKeys[key] {
			t.Errorf("credential Secret contains unexpected key %q", key)
		}
		if strings.Contains(string(value), "fake-serving-key") || strings.Contains(string(value), "fake-workload-ca") {
			t.Errorf("credential Secret value for %q contains CA or serving-key material", key)
		}
	}

	// No serving-key material may appear anywhere in the generated Job.
	for _, field := range jobStrings(job) {
		if strings.Contains(field, "fake-serving-key") {
			t.Fatalf("generated Job contains the serving private key: %q", field)
		}
	}

	if job.Spec.AutomountServiceAccountToken {
		t.Error("job automounts a service-account token; workload trust must not widen the pod identity")
	}
	for _, volume := range job.Spec.Volumes {
		if strings.HasPrefix(volume.Name, "kube-api-access-") {
			t.Errorf("job has a service-account token volume %q", volume.Name)
		}
	}
}

// pathContains reports whether child is parent itself or lives below it.
func pathContains(parent, child string) bool {
	if parent == child {
		return true
	}
	return strings.HasPrefix(child, parent+"/")
}

func credentialKeys(data map[string][]byte) []string {
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	return keys
}

// jobStrings returns every non-secret textual field of a generated Job so a
// test can prove a marker never appears anywhere in the manifest.
func jobStrings(job Job) []string {
	values := []string{job.Name, job.Namespace}
	for key, value := range job.Labels {
		values = append(values, key, value)
	}
	for key, value := range job.Annotations {
		values = append(values, key, value)
	}
	for key, value := range job.Spec.PodLabels {
		values = append(values, key, value)
	}
	values = append(values, job.Spec.Image)
	values = append(values, job.Spec.Command...)
	for _, entry := range job.Spec.Env {
		values = append(values, entry.Name, entry.Value)
	}
	for _, volume := range job.Spec.Volumes {
		values = append(values, volume.Name, volume.MountPath, volume.ClaimName, volume.ConfigMapName, volume.SecretName)
		for _, item := range volume.Items {
			values = append(values, item.Key, item.Path)
		}
	}
	return values
}
