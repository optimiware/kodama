//go:build component

package kubernetesconformance

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	driverkube "github.com/optimiware/kodama/internal/shell/app/drivers/kube"
	kubesupport "github.com/optimiware/kodama/test/support/infrastructure/kubernetes"
)

// TestServiceAccountTokenFileAuthentication proves the real API accepts the
// explicitly selected in-cluster service-account token read from a file, and
// refuses a replacement token that is invalid or revoked. It also proves the
// token file is re-read, so restoring the original token recovers the client
// without rebuilding it.
func TestServiceAccountTokenFileAuthentication(t *testing.T) {
	t.Parallel()
	fx, ctx := newFixture(t, id.WorkspaceID("workspace-kube-incluster"), fixtureOptions{})

	const (
		serviceAccount = "kodama-incluster-proof"
		configMapName  = "kodama-incluster-proof"
		proofPath      = "/kodama/incluster-proof"
		proofValue     = "service-account-token"
	)
	if _, err := fx.Cluster.Apply(ctx, inClusterFixtureYAML(serviceAccount, configMapName)); err != nil {
		t.Fatalf("apply in-cluster authentication fixture: %v", err)
	}

	server, caPEM := inClusterEndpoint(t, ctx, fx.Cluster)
	token := issueServiceAccountToken(t, ctx, fx.Cluster, serviceAccount)
	tokenPath := filepath.Join(t.TempDir(), "token")
	writeInClusterTokenFile(t, tokenPath, token)
	caPath := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatalf("write service-account CA file: %v", err)
	}

	client, err := driverkube.NewCluster(driverkube.ClusterConfig{
		InCluster: &driverkube.InClusterConfig{
			Server:    server,
			TokenFile: tokenPath,
			CAFile:    caPath,
		},
		Timeout: 2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("build in-cluster Kubernetes driver: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	read := func() error {
		configMap, err := client.GetConfigMap(ctx, testNamespace, configMapName)
		if err != nil {
			return err
		}
		if string(configMap.Data[proofPath]) != proofValue {
			t.Fatalf("ConfigMap proof = %q; want %q", configMap.Data[proofPath], proofValue)
		}
		return nil
	}

	// The real API accepts the service-account token from the file.
	if err := read(); err != nil {
		t.Fatalf("GetConfigMap with the service-account token: %v", err)
	}

	// A replacement that is not a service-account token is refused.
	writeInClusterTokenFile(t, tokenPath, "not-a-service-account-token")
	if err := read(); err == nil {
		t.Fatal("GetConfigMap with an invalid replacement token = nil; want an authentication refusal")
	} else {
		requireFaultClass(t, err, fault.DependencyUnavailable)
		requireNoCredential(t, err, token)
	}

	// Restoring the original token proves the file is re-read per request.
	writeInClusterTokenFile(t, tokenPath, token)
	if err := read(); err != nil {
		t.Fatalf("GetConfigMap after restoring the service-account token: %v", err)
	}

	// Deleting the ServiceAccount revokes its bound token. The RoleBinding
	// still names the account, so a token the API kept accepting would still
	// authorize the read; the refusal therefore proves the credential, not the
	// permission, was revoked. A bounded poll absorbs authentication cache
	// eventual consistency without a fixed settle sleep.
	if _, err := fx.Cluster.Kubectl(ctx, "delete", "serviceaccount", serviceAccount,
		"--namespace", testNamespace, "--wait=true"); err != nil {
		t.Fatalf("delete the proof ServiceAccount: %v", err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		err := read()
		if err != nil {
			requireFaultClass(t, err, fault.DependencyUnavailable)
			requireNoCredential(t, err, token)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the API still accepted a token for a deleted ServiceAccount")
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			t.Fatalf("waiting for the revoked service-account token to be refused: %v", ctx.Err())
		case <-timer.C:
		}
	}
}

// inClusterEndpoint extracts the API server URL and CA bundle from the
// cluster's isolated kubeconfig. The kind admin kubeconfig is only used to
// obtain the endpoint and trust material; authentication uses the
// service-account token file.
func inClusterEndpoint(t *testing.T, ctx context.Context, cluster *kubesupport.Cluster) (string, []byte) {
	t.Helper()
	raw, err := cluster.KubeconfigJSON(ctx)
	if err != nil {
		t.Fatalf("export kubeconfig: %v", err)
	}
	var document struct {
		Clusters []struct {
			Cluster struct {
				Server                   string `json:"server"`
				CertificateAuthorityData string `json:"certificate-authority-data"`
			} `json:"cluster"`
		} `json:"clusters"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("decode exported kubeconfig: %v", err)
	}
	if len(document.Clusters) != 1 {
		t.Fatalf("exported kubeconfig has %d clusters; want 1", len(document.Clusters))
	}
	server := strings.TrimSpace(document.Clusters[0].Cluster.Server)
	if !strings.HasPrefix(server, "https://") {
		t.Fatalf("exported cluster server %q is not https", server)
	}
	encoded := strings.TrimSpace(document.Clusters[0].Cluster.CertificateAuthorityData)
	if encoded == "" {
		t.Fatal("exported kubeconfig carries no certificate-authority-data")
	}
	caPEM, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode exported certificate authority: %v", err)
	}
	return server, caPEM
}

// issueServiceAccountToken requests one bound token for the named
// ServiceAccount through the real API.
func issueServiceAccountToken(t *testing.T, ctx context.Context, cluster *kubesupport.Cluster, name string) string {
	t.Helper()
	result, err := cluster.Kubectl(ctx, "create", "token", name,
		"--namespace", testNamespace, "--duration", "1h")
	if err != nil {
		t.Fatalf("issue service-account token: %v", err)
	}
	token := strings.TrimSpace(string(result.Stdout))
	if token == "" {
		t.Fatal("kubectl create token returned an empty token")
	}
	return token
}

// writeInClusterTokenFile atomically replaces the token file, as a projected
// volume rotation does, so a request never reads a partially written token.
func writeInClusterTokenFile(t *testing.T, path, token string) {
	t.Helper()
	staged := path + ".staged"
	if err := os.WriteFile(staged, []byte(token), 0o600); err != nil {
		t.Fatalf("stage token file: %v", err)
	}
	if err := os.Rename(staged, path); err != nil {
		t.Fatalf("publish token file: %v", err)
	}
}

// inClusterFixtureYAML creates the proof ServiceAccount, its namespace-scoped
// read Role and RoleBinding, and the ConfigMap the token is allowed to read. The
// ConfigMap uses binaryData with the driver's activation-path key encoding so
// the driver can decode it.
func inClusterFixtureYAML(serviceAccount, configMap string) string {
	key := base64.RawURLEncoding.EncodeToString([]byte("/kodama/incluster-proof"))
	value := base64.StdEncoding.EncodeToString([]byte("service-account-token"))
	return fmt.Sprintf(`apiVersion: v1
kind: ServiceAccount
metadata:
  name: %[1]s
  namespace: %[2]s
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: %[1]s
  namespace: %[2]s
rules:
  - apiGroups: [""]
    resources: ["configmaps"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: %[1]s
  namespace: %[2]s
subjects:
  - kind: ServiceAccount
    name: %[1]s
    namespace: %[2]s
roleRef:
  kind: Role
  name: %[1]s
  apiGroup: rbac.authorization.k8s.io
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: %[3]s
  namespace: %[2]s
binaryData:
  %[4]s: %[5]s
`, serviceAccount, testNamespace, configMap, key, value)
}

// requireNoCredential proves an error never renders the bearer token.
func requireNoCredential(t *testing.T, err error, values ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("error = nil; want a credential-free failure")
	}
	rendered := err.Error()
	for _, value := range values {
		if value == "" {
			continue
		}
		if strings.Contains(rendered, value) {
			t.Fatalf("error %q contains credential value %q", rendered, value)
		}
	}
}
