package kubernetes

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/internal/logs"
	"github.com/optimiware/kodama/test/support/layer"
)

// Real-cluster qualification of the Reader. It runs only when KODAMA_KUBECONFIG
// names a cluster (for example the development kind scope's private kubeconfig:
// `make dev-up DEV_SCOPE=t023`, then dev/.state/environments/<scope>/kubeconfig)
// and skips otherwise: without a cluster this test proves nothing, and the
// fake-API tests in reader_test.go do not stand in for it.
//
//	KODAMA_KUBECONFIG       path of the kubeconfig (current context is used)
//	KODAMA_KUBE_TEST_IMAGE  image with /bin/sh (default busybox:1.37, which the
//	                        cluster must be able to pull)
//
// The test creates and deletes its own namespace (prefix t023-).
const (
	kubeconfigEnv = "KODAMA_KUBECONFIG"
	imageEnv      = "KODAMA_KUBE_TEST_IMAGE"
)

type kubeconfig struct {
	CurrentContext string `yaml:"current-context"`
	Clusters       []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server string `yaml:"server"`
			CAData string `yaml:"certificate-authority-data"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
	Users []struct {
		Name string `yaml:"name"`
		User struct {
			CertData string `yaml:"client-certificate-data"`
			KeyData  string `yaml:"client-key-data"`
			Token    string `yaml:"token"`
		} `yaml:"user"`
	} `yaml:"users"`
	Contexts []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster string `yaml:"cluster"`
			User    string `yaml:"user"`
		} `yaml:"context"`
	} `yaml:"contexts"`
}

// clientFromKubeconfig builds an authenticated client, as the composition root
// will for the controller's service account.
func clientFromKubeconfig(t *testing.T, path string) (string, *http.Client) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var kc kubeconfig
	if err := yaml.Unmarshal(raw, &kc); err != nil {
		t.Fatal(err)
	}
	var cluster, user string
	for _, c := range kc.Contexts {
		if c.Name == kc.CurrentContext {
			cluster, user = c.Context.Cluster, c.Context.User
		}
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	var server string
	for _, c := range kc.Clusters {
		if c.Name != cluster {
			continue
		}
		server = c.Cluster.Server
		ca, err := base64.StdEncoding.DecodeString(c.Cluster.CAData)
		if err != nil {
			t.Fatal(err)
		}
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(ca)
		tlsCfg.RootCAs = pool
	}
	var token string
	for _, u := range kc.Users {
		if u.Name != user {
			continue
		}
		token = u.User.Token
		if u.User.CertData != "" {
			cert, _ := base64.StdEncoding.DecodeString(u.User.CertData)
			key, _ := base64.StdEncoding.DecodeString(u.User.KeyData)
			pair, err := tls.X509KeyPair(cert, key)
			if err != nil {
				t.Fatal(err)
			}
			tlsCfg.Certificates = []tls.Certificate{pair}
		}
	}
	if server == "" {
		t.Fatal("the kubeconfig names no cluster server")
	}
	var rt http.RoundTripper = &http.Transport{TLSClientConfig: tlsCfg}
	if token != "" {
		rt = bearerTransport{base: rt, token: token}
	}
	return server, &http.Client{Transport: rt}
}

type bearerTransport struct {
	base  http.RoundTripper
	token string
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

func kubectl(t *testing.T, kubeconfigPath string, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("kubectl", append([]string{"--kubeconfig", kubeconfigPath}, args...)...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestKubernetesOwnership: real Job/Pod owner verification, wrong UID and
// container, missing Job, and bounded log-subresource reads against a real API
// server.
func TestKubernetesOwnership(t *testing.T) {
	layer.SkipService(t, "Kubernetes")
	path := os.Getenv(kubeconfigEnv)
	if path == "" {
		path = os.Getenv("KODAMA_TEST_KUBECONFIG")
	}
	if path == "" {
		t.Skipf("real Kubernetes qualification skipped: set %s to a cluster's kubeconfig", kubeconfigEnv)
	}
	image := os.Getenv(imageEnv)
	if image == "" {
		image = "busybox:1.37"
	}
	endpoint, client := clientFromKubeconfig(t, path)
	r, err := NewReader(Config{Endpoint: endpoint, Client: client})
	if err != nil {
		t.Fatal(err)
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	ns := "t023-" + hex.EncodeToString(b[:])
	kubectl(t, path, "", "create", "namespace", ns)
	t.Cleanup(func() {
		_ = exec.Command("kubectl", "--kubeconfig", path, "delete", "namespace", ns, "--wait=false").Run()
	})

	const job = "t023-logs"
	kubectl(t, path, `apiVersion: batch/v1
kind: Job
metadata: {name: `+job+`}
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: step
          image: `+image+`
          command: ["sh", "-c", "i=1; while [ $i -le 50 ]; do echo line-$i; i=$((i+1)); done"]
        - name: other
          image: `+image+`
          command: ["sh", "-c", "echo other-container"]
`, "-n", ns, "apply", "-f", "-")
	kubectl(t, path, "", "-n", ns, "wait", "--for=condition=complete", "job/"+job, "--timeout=180s")
	uid := kubectl(t, path, "", "-n", ns, "get", "job", job, "-o", "jsonpath={.metadata.uid}")
	target := logs.Target{Namespace: ns, JobName: job, JobUID: uid, Container: "step"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	read := func(tg logs.Target, q logs.LiveQuery) (string, error) {
		rc, err := r.Open(ctx, tg, q)
		if err != nil {
			return "", err
		}
		defer rc.Close()
		b, err := io.ReadAll(rc)
		return string(b), err
	}

	t.Run("verified workload, tail and byte bounds", func(t *testing.T) {
		out, err := read(target, logs.LiveQuery{})
		if err != nil || !strings.Contains(out, "line-1\n") || !strings.Contains(out, "line-50\n") || strings.Contains(out, "other-container") {
			t.Fatalf("%q %v", out, err)
		}
		out, err = read(target, logs.LiveQuery{TailLines: 5})
		if err != nil || strings.Count(out, "\n") != 5 || !strings.HasSuffix(out, "line-50\n") {
			t.Fatalf("tail: %q %v", out, err)
		}
		out, err = read(target, logs.LiveQuery{LimitBytes: 20})
		if err != nil || len(out) > 20 || len(out) == 0 {
			t.Fatalf("limitBytes: %q %v", out, err)
		}
	})
	t.Run("another container of the Pod is selected only when named by composition", func(t *testing.T) {
		tg := target
		tg.Container = "other"
		out, err := read(tg, logs.LiveQuery{})
		if err != nil || !strings.Contains(out, "other-container") {
			t.Fatalf("%q %v", out, err)
		}
		tg.Container = "no-such-container"
		if _, err := read(tg, logs.LiveQuery{}); !errors.Is(err, logs.ErrWorkloadMismatch) {
			t.Fatalf("a container the Pod does not have: %v", err)
		}
	})
	t.Run("wrong UID, missing Job and foreign namespace", func(t *testing.T) {
		tg := target
		tg.JobUID = "99999999-2222-3333-4444-555555555555"
		if _, err := read(tg, logs.LiveQuery{}); !errors.Is(err, logs.ErrWorkloadMismatch) {
			t.Fatalf("a Job with another UID: %v", err)
		}
		tg = target
		tg.JobName = "t023-absent"
		if _, err := read(tg, logs.LiveQuery{}); !errors.Is(err, logs.ErrWorkloadMissing) {
			t.Fatalf("an absent Job: %v", err)
		}
		tg = target
		tg.Namespace = "kube-system"
		if _, err := read(tg, logs.LiveQuery{}); !errors.Is(err, logs.ErrWorkloadMissing) {
			t.Fatalf("the Job does not exist in another namespace: %v", err)
		}
	})
	t.Run("a deleted Job leaves no workload", func(t *testing.T) {
		kubectl(t, path, "", "-n", ns, "delete", "job", job, "--wait=true")
		if _, err := read(target, logs.LiveQuery{}); !errors.Is(err, logs.ErrWorkloadMissing) {
			t.Fatalf("after deletion: %v", err)
		}
	})
}
