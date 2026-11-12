//go:build component

package kubernetesconformance

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/kernel/engine"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
)

// workloadCAImage is the ordinary smoke workload used by this test. It is the
// upstream curl image so the workload can pass an explicit --cacert; the digest
// is resolved and recorded when the real run happens, like the other pinned
// component images.
const (
	workloadCAImage       = "docker.io/curlimages/curl:8.10.1"
	workloadCASecret      = "kodama-workload-ca"
	workloadCAWrongSecret = "kodama-workload-ca-wrong"
)

// TestWorkloadMountedCAHTTPS proves the mounted installation CA is usable by an
// ordinary workload and that TLS verification actually depends on both the
// intended CA and the intended hostname.
//
// The cluster API server is used as the trusted HTTPS endpoint: it is the one
// TLS service every kind fixture guarantees, its CA is available as the
// namespace's kube-root-ca.crt, and its serving certificate carries the
// in-cluster name kubernetes.default.svc. The tenant-local CA-only Secret is
// built from that certificate and configured through the adapter, exactly as an
// installation would provide its API trust bundle. The test is compiled and
// vetted in the test-writing stage; it runs only in the real-infrastructure
// stage.
func TestWorkloadMountedCAHTTPS(t *testing.T) {
	t.Parallel()
	workspaceID := id.WorkspaceID("workspace-kube-ca")
	fx, ctx := newFixture(t, workspaceID, fixtureOptions{withClaim: true})

	trustCA := clusterTrustCA(t, ctx, fx)
	putWorkloadCASecret(t, ctx, fx, workloadCASecret, trustCA)
	putWorkloadCASecret(t, ctx, fx, workloadCAWrongSecret, generateWrongCA(t))

	intended := newWorkloadCAAdapter(t, fx, workloadCASecret)
	script := fmt.Sprintf(
		`curl -fsS --cacert %s https://kubernetes.default.svc/version`,
		runtimekube.WorkloadCAPath,
	)

	// The intended CA and hostname: ordinary HTTPS succeeds.
	identity := testIdentity("exec-ca-ok", "action-ca-ok", "workload-ca-ok", "idem-ca-ok")
	observation, stdout, _ := runCAWorkload(t, ctx, fx, intended, identity, workspaceID, script)
	if code, ok := observation.ExitStatus(); !ok || code != 0 {
		t.Fatalf("workload with the intended CA exit = %d/%v; want 0/true (%+v)", code, ok, observation)
	}
	if !strings.Contains(string(stdout), `"major"`) {
		t.Fatalf("workload HTTPS response = %q; want the API server version document", stdout)
	}
	job := oneJob(t, ctx, fx.Client, identity)
	assertLiveCAVolume(t, ctx, fx, job.Name)
	assertJobSecurityPolicy(t, ctx, fx, job.Name)

	// A different CA: the same request must fail certificate verification.
	wrongCA := newWorkloadCAAdapter(t, fx, workloadCAWrongSecret)
	wrongIdentity := testIdentity("exec-ca-wrong", "action-ca-wrong", "workload-ca-wrong", "idem-ca-wrong")
	observation, combined, stderr := runCAWorkload(t, ctx, fx, wrongCA, wrongIdentity, workspaceID, script)
	if code, ok := observation.ExitStatus(); !ok || code == 0 {
		t.Fatalf("workload with an unintended CA exit = %d/%v; want a nonzero curl exit (%+v)", code, ok, observation)
	}
	// Stock clusters serve one combined log: curl's stderr arrives there,
	// attributed to stdout, and the split stderr stream stays empty.
	if !strings.Contains(strings.ToLower(string(combined)), "certificate") || len(stderr) != 0 {
		t.Fatalf("unintended-CA combined log = %q (stderr %q); want a certificate verification failure",
			combined, stderr)
	}

	// The intended CA with a hostname the certificate does not name must also
	// fail: trusting the CA is not trusting every host.
	clusterIP := kubernetesServiceIP(t, ctx, fx)
	wrongHostIdentity := testIdentity("exec-ca-host", "action-ca-host", "workload-ca-host", "idem-ca-host")
	wrongHostScript := fmt.Sprintf(
		`curl -fsS --cacert %s --resolve not-the-apiserver.invalid:443:%s https://not-the-apiserver.invalid/version`,
		runtimekube.WorkloadCAPath,
		clusterIP,
	)
	observation, combined, stderr = runCAWorkload(t, ctx, fx, intended, wrongHostIdentity, workspaceID, wrongHostScript)
	if code, ok := observation.ExitStatus(); !ok || code == 0 {
		t.Fatalf("workload with a mismatched hostname exit = %d/%v; want a nonzero curl exit (%+v)", code, ok, observation)
	}
	if !strings.Contains(strings.ToLower(string(combined)), "certificate") || len(stderr) != 0 {
		t.Fatalf("mismatched-hostname combined log = %q (stderr %q); want a certificate verification failure",
			combined, stderr)
	}
}

// newWorkloadCAAdapter builds an adapter bound to the fixture with the
// installation CA-only Secret configured.
func newWorkloadCAAdapter(t *testing.T, fx *fixture, secretName string) *runtimekube.Adapter {
	t.Helper()
	adapter, err := runtimekube.New(runtimekube.Config{
		Cluster:   fx.Client,
		Namespace: testNamespace,
		TenantID:  testTenant(),
		KodamaURL: testKodamaURL,
		Policy: runtimekube.Policy{
			CPURequest:    "50m",
			CPULimit:      "200m",
			MemoryRequest: "32Mi",
			MemoryLimit:   "128Mi",
			JobTTLSeconds: 120,
		},
		Cleanup:    runtimekube.CleanupPolicy{Attempts: 3, Backoff: 500 * time.Millisecond},
		WorkloadCA: runtimekube.WorkloadCAConfig{SecretName: secretName},
	})
	if err != nil {
		t.Fatalf("build Kubernetes adapter with CA Secret %q: %v", secretName, err)
	}
	return adapter
}

// runCAWorkload prepares and starts one ordinary curl workload, waits for its
// terminal observation and returns the observed status plus both log streams.
func runCAWorkload(
	t *testing.T,
	ctx context.Context,
	fx *fixture,
	adapter *runtimekube.Adapter,
	identity engine.EffectIdentity,
	workspaceID id.WorkspaceID,
	script string,
) (kernel.Observation, []byte, []byte) {
	t.Helper()
	request := engine.PrepareRequest{
		Identity:    identity,
		WorkspaceID: workspaceID,
		ActionName:  "https",
		Container: definition.Container{
			Image:    workloadCAImage,
			Commands: []string{script},
		},
		Credential: "tok-" + string(identity.WorkloadID),
	}
	if _, err := adapter.Prepare(ctx, request); err != nil {
		t.Fatalf("Prepare %s: %v", identity.WorkloadID, err)
	}
	start, err := adapter.Start(ctx, engine.StartRequest{Identity: identity, WorkspaceID: workspaceID})
	if err != nil {
		t.Fatalf("Start %s: %v", identity.WorkloadID, err)
	}
	if start.Outcome != engine.StartAccepted {
		t.Fatalf("start outcome = %q; want accepted (%s)", start.Outcome, start.Detail)
	}
	observation := waitForObservation(t, ctx, adapter, engine.ObserveRequest{
		Identity: identity, WorkspaceID: workspaceID,
	}, kernel.ObservationExited)
	stdout, stderr := readAllLogs(t, ctx, adapter, identity, 1<<20)
	return observation, stdout, stderr
}

// clusterTrustCA reads the namespace's cluster CA, which signs the in-cluster
// API server's serving certificate.
func clusterTrustCA(t *testing.T, ctx context.Context, fx *fixture) string {
	t.Helper()
	document, err := fx.Cluster.GetJSON(ctx, "get", "configmap", "kube-root-ca.crt", "-n", testNamespace)
	if err != nil {
		t.Fatalf("read kube-root-ca.crt: %v", err)
	}
	data, _ := document["data"].(map[string]any)
	ca, _ := data["ca.crt"].(string)
	if strings.TrimSpace(ca) == "" {
		t.Fatalf("kube-root-ca.crt has no ca.crt data: %v", document)
	}
	return ca
}

// putWorkloadCASecret creates the tenant-local CA-only Secret the adapter
// references.
func putWorkloadCASecret(t *testing.T, ctx context.Context, fx *fixture, name, caPEM string) {
	t.Helper()
	if _, err := fx.Client.PutSecret(ctx, runtimekube.Secret{
		Name:      name,
		Namespace: testNamespace,
		Labels:    map[string]string{runtimekube.LabelTenant: string(testTenant())},
		Data:      map[string][]byte{runtimekube.WorkloadCAKey: []byte(caPEM)},
	}); err != nil {
		t.Fatalf("create CA Secret %q: %v", name, err)
	}
}

// kubernetesServiceIP returns the ClusterIP of the in-cluster API service so a
// test can connect to it under a hostname the serving certificate does not name.
func kubernetesServiceIP(t *testing.T, ctx context.Context, fx *fixture) string {
	t.Helper()
	document, err := fx.Cluster.GetJSON(ctx, "get", "service", "kubernetes", "-n", "default")
	if err != nil {
		t.Fatalf("read kubernetes service: %v", err)
	}
	spec, _ := document["spec"].(map[string]any)
	clusterIP, _ := spec["clusterIP"].(string)
	if strings.TrimSpace(clusterIP) == "" {
		t.Fatalf("kubernetes service has no clusterIP: %v", document)
	}
	return clusterIP
}

// assertLiveCAVolume proves the API server accepted the literal Secret-key
// projection: a read-only workload-ca mount whose Secret item keeps the literal
// ca.crt key and the fixed item path.
func assertLiveCAVolume(t *testing.T, ctx context.Context, fx *fixture, jobName string) {
	t.Helper()
	document, err := fx.Cluster.GetJSON(ctx, "get", "job", jobName, "-n", testNamespace)
	if err != nil {
		t.Fatalf("read live Job: %v", err)
	}
	spec, _ := document["spec"].(map[string]any)
	template, _ := spec["template"].(map[string]any)
	podSpec, _ := template["spec"].(map[string]any)
	containers, _ := podSpec["containers"].([]any)
	if len(containers) != 1 {
		t.Fatalf("containers = %d; want exactly 1", len(containers))
	}
	container, _ := containers[0].(map[string]any)
	mounts, _ := container["volumeMounts"].([]any)
	mountName := ""
	for _, raw := range mounts {
		mount, _ := raw.(map[string]any)
		if path, _ := mount["mountPath"].(string); path == runtimekube.WorkloadCAMountPath {
			mountName, _ = mount["name"].(string)
			if readOnly, _ := mount["readOnly"].(bool); !readOnly {
				t.Fatalf("workload CA mount %q is not read-only: %v", path, mount)
			}
		}
	}
	if mountName == "" {
		t.Fatalf("Job has no volume mounted at %q: %v", runtimekube.WorkloadCAMountPath, mounts)
	}
	volumes, _ := podSpec["volumes"].([]any)
	for _, raw := range volumes {
		volume, _ := raw.(map[string]any)
		if name, _ := volume["name"].(string); name != mountName {
			continue
		}
		secret, _ := volume["secret"].(map[string]any)
		if secretName, _ := secret["secretName"].(string); secretName != workloadCASecret {
			t.Fatalf("workload CA Secret = %q; want %q", secretName, workloadCASecret)
		}
		items, _ := secret["items"].([]any)
		if len(items) != 1 {
			t.Fatalf("workload CA items = %v; want exactly the literal ca.crt item", items)
		}
		item, _ := items[0].(map[string]any)
		if key, _ := item["key"].(string); key != runtimekube.WorkloadCAKey {
			t.Fatalf("workload CA item key = %q; want the literal Secret key %q", key, runtimekube.WorkloadCAKey)
		}
		if path, _ := item["path"].(string); path != "ca.crt" {
			t.Fatalf("workload CA item path = %q; want ca.crt relative to the mount", path)
		}
		return
	}
	t.Fatalf("Job has no volume named %q: %v", mountName, volumes)
}

// generateWrongCA returns a fresh self-signed CA that did not sign the API
// server certificate, so trusting it must fail verification.
func generateWrongCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate wrong-CA key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "kodama-wrong-workload-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create wrong CA: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}
