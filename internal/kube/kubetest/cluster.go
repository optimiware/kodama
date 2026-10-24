// Package kubetest holds fixtures for the Kubernetes executor's tests: a client
// for the task's kind cluster and an in-memory cluster with fault injection. It
// contains no production behavior and is imported only by tests. Resource names
// carry the task prefix t021.
package kubetest

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/kube/kubernetes"
	"github.com/optimiware/kodama/test/support/layer"
)

// Prefix is this task's resource prefix.
const Prefix = "t021"

// KubeconfigPath returns the kubeconfig of the task's kind cluster:
// KODAMA_TEST_KUBECONFIG, else dev/.state/environments/<scope>/kubeconfig.
func KubeconfigPath() string {
	if p := os.Getenv("KODAMA_TEST_KUBECONFIG"); p != "" {
		return p
	}
	_, file, _, _ := goruntime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..", "..")
	prefix := os.Getenv("DEV_SCOPE")
	if prefix == "" {
		prefix = os.Getenv("KODAMA_RESOURCE_PREFIX")
	}
	if prefix == "" {
		prefix = Prefix
	}
	return filepath.Join(root, "dev", ".state", "environments", prefix, "kubeconfig")
}

// Require reports whether the cluster is configured. A missing cluster skips the
// test unless KODAMA_REQUIRE_KUBERNETES=1, in which case it fails; the task test
// runner counts skips as missing acceptance. The unit layer always skips, before
// any inspection of the kubeconfig or the environment.
func Require(t testing.TB) string {
	t.Helper()
	layer.SkipService(t, "Kubernetes")
	p := KubeconfigPath()
	if _, err := os.Stat(p); err != nil {
		if os.Getenv("KODAMA_REQUIRE_KUBERNETES") == "1" {
			t.Fatalf("kubernetes cluster required but %s is missing", p)
		}
		t.Skipf("no kind cluster (%s); set KODAMA_TEST_KUBECONFIG or run the task-local runner", p)
	}
	return p
}

type kubeconfigDoc struct {
	Clusters []struct {
		Cluster struct {
			Server string `json:"server"`
			CAData string `json:"certificate-authority-data"`
		} `json:"cluster"`
	} `json:"clusters"`
	Users []struct {
		User struct {
			CertData string `json:"client-certificate-data"`
			KeyData  string `json:"client-key-data"`
		} `json:"user"`
	} `json:"users"`
}

// Kubectl runs the cluster's kubectl.
func Kubectl(kubeconfig string, args ...string) ([]byte, error) {
	bin := os.Getenv("KUBECTL")
	if bin == "" {
		bin = "kubectl"
	}
	cmd := exec.Command(bin, append([]string{"--kubeconfig", kubeconfig}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("kubectl %v: %w: %s", args, err, out)
	}
	return out, nil
}

// NewClient builds an authenticated client from the kubeconfig's first cluster
// and user (client certificate). It is test infrastructure: production
// authentication is supplied by T-027.
func NewClient(kubeconfig string) (*kubernetes.Client, error) {
	raw, err := Kubectl(kubeconfig, "config", "view", "--raw", "-o", "json", "--minify")
	if err != nil {
		return nil, err
	}
	var doc kubeconfigDoc
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc.Clusters) == 0 || len(doc.Users) == 0 {
		return nil, errors.New("kubeconfig: no cluster or user")
	}
	dec := func(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }
	ca, err := dec(doc.Clusters[0].Cluster.CAData)
	if err != nil {
		return nil, err
	}
	crt, err := dec(doc.Users[0].User.CertData)
	if err != nil {
		return nil, err
	}
	key, err := dec(doc.Users[0].User.KeyData)
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(crt, key)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}}}
	return kubernetes.NewClient(kubernetes.Config{Origin: doc.Clusters[0].Cluster.Server, HTTP: hc, RequestTimeout: 30 * time.Second})
}
