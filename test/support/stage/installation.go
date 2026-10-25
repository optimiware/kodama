package stage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// Installation is the Kodama release of a qualification session as the tests see it: its public address, its
// cluster objects, its private database (through the probe relay) and the personas that sign in at its issuer.
type Installation struct {
	T    testing.TB
	S    *Session
	Kube Kube
	Auth *Auth

	mu sync.Mutex
	db *DB
}

// New loads the prepared session. It does not install anything.
func New(t testing.TB) *Installation {
	t.Helper()
	s := LoadSession(t)
	k := Kube{Kubeconfig: s.Kubeconfig}
	i := &Installation{T: t, S: s, Kube: k, Auth: NewAuth(k, s.Issuer)}
	t.Cleanup(i.close)
	t.Cleanup(func() {
		if t.Failed() {
			i.Diagnose()
		}
	})
	return i
}

func (i *Installation) close() {
	i.mu.Lock()
	db := i.db
	i.db = nil
	i.mu.Unlock()
	if db != nil {
		db.Close()
	}
}

// Context returns a context bounded by d that is cancelled with the test.
func (i *Installation) Context(d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	i.T.Cleanup(cancel)
	return ctx
}

// ---- values -------------------------------------------------------------------------------------------------

// Values is the part of the session's Helm values the fixture reads.
type Values struct {
	Image struct {
		Repository string `yaml:"repository"`
		Tag        string `yaml:"tag"`
		Digest     string `yaml:"digest"`
	} `yaml:"image"`
	Execution struct {
		ExampleJob struct {
			Image struct {
				Repository string `yaml:"repository"`
				Tag        string `yaml:"tag"`
				Digest     string `yaml:"digest"`
			} `yaml:"image"`
		} `yaml:"exampleJob"`
	} `yaml:"execution"`
}

// Values reads the rendered values the release is installed with.
func (i *Installation) Values() Values {
	var v Values
	i.ReadValues(&v)
	return v
}

func (i *Installation) ReadValues(v any) {
	i.T.Helper()
	data, err := os.ReadFile(i.S.ValuesFile())
	if err != nil {
		i.T.Fatal(err)
	}
	if err := yaml.Unmarshal(data, v); err != nil {
		i.T.Fatalf("values: %v", err)
	}
}

// ServerImage is the digest-pinned release image under test.
func (i *Installation) ServerImage() string {
	v := i.Values()
	return v.Image.Repository + "@" + v.Image.Digest
}

// WorkloadImage is the digest-pinned image pipeline steps run in.
func (i *Installation) WorkloadImage() string {
	v := i.Values()
	return v.Execution.ExampleJob.Image.Repository + "@" + v.Execution.ExampleJob.Image.Digest
}

// ---- lifecycle -------------------------------------------------------------------------------------------

// Installed reports whether the release exists.
func (i *Installation) Installed(ctx context.Context) bool {
	_, err := i.Kube.Helm(ctx, "status", i.S.Release, "-n", i.S.Namespace)
	return err == nil
}

// Install installs (or upgrades to) the committed chart with the session's values and waits for the server's
// Deployment to roll out. Migration is the chart's own Job; its completion is part of a successful install.
func (i *Installation) Install(ctx context.Context) error {
	_, err := i.Kube.Helm(ctx, "upgrade", "--install", i.S.Release, i.S.ChartDir, "-n", i.S.Namespace, "-f", i.S.ValuesFile(), "--wait", "--timeout", "8m")
	return err
}

// Uninstall removes the release. Tenant namespaces, databases, objects, Git refs and keys are retained.
func (i *Installation) Uninstall(ctx context.Context) error {
	_, err := i.Kube.Helm(ctx, "uninstall", i.S.Release, "-n", i.S.Namespace, "--wait", "--timeout", "5m")
	return err
}

// ensureInstalled installs the release when it is missing and waits for readiness.
func (i *Installation) EnsureReady(ctx context.Context) {
	i.T.Helper()
	if !i.Installed(ctx) {
		if err := i.Install(ctx); err != nil {
			i.T.Fatalf("install: %v", err)
		}
	}
	i.WaitReady(ctx)
}

// WaitReady waits for the public /readyz over HTTPS (and so for the certificate, the ingress and the server).
func (i *Installation) WaitReady(ctx context.Context) {
	i.T.Helper()
	deadline := time.Now().Add(4 * time.Minute)
	var last string
	for time.Now().Before(deadline) {
		code, body, err := i.get(ctx, "/readyz")
		if err == nil && code == http.StatusOK {
			return
		}
		last = fmt.Sprintf("%d %s %v", code, strings.TrimSpace(body), err)
		select {
		case <-ctx.Done():
			i.T.Fatalf("readiness: %v (%s)", ctx.Err(), last)
		case <-time.After(2 * time.Second):
		}
	}
	i.T.Fatalf("the installation was not ready within 4m: %s", last)
}

func (i *Installation) get(ctx context.Context, path string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, i.S.URL+path, nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

// Deployment is the server's Deployment name (the chart names it after the release).
func (i *Installation) Deployment() string { return i.S.Release }

// Scale sets the server's replica count and waits for the Deployment to settle.
func (i *Installation) Scale(ctx context.Context, replicas int) error {
	if _, err := i.Kube.Run(ctx, "-n", i.S.Namespace, "scale", "deployment/"+i.Deployment(), fmt.Sprintf("--replicas=%d", replicas)); err != nil {
		return err
	}
	if replicas == 0 {
		_, err := i.Kube.Run(ctx, "-n", i.S.Namespace, "wait", "--for=delete", "pod", "-l", "app.kubernetes.io/component=server", "--timeout=3m")
		return err
	}
	_, err := i.Kube.Run(ctx, "-n", i.S.Namespace, "rollout", "status", "deployment/"+i.Deployment(), "--timeout=5m")
	return err
}

// Stop gracefully drains the server. Workload preservation is not guaranteed; use Crash when
// testing adoption after abrupt process loss. Start starts the server again.
func (i *Installation) Stop(ctx context.Context) {
	i.T.Helper()
	i.T.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer cancel()
		if err := i.Scale(c, 1); err != nil {
			i.T.Errorf("restore server after stop: %v", err)
		}
	})
	if err := i.Scale(ctx, 0); err != nil {
		i.T.Fatalf("stop the server: %v", err)
	}
}

// Start starts the server and waits for readiness.
func (i *Installation) Start(ctx context.Context) {
	i.T.Helper()
	if err := i.Scale(ctx, 1); err != nil {
		i.T.Fatalf("start the server: %v", err)
	}
	i.WaitReady(ctx)
}

// Restart replaces the server process over the same state.
func (i *Installation) Restart(ctx context.Context) {
	i.T.Helper()
	i.Stop(ctx)
	i.Start(ctx)
}

// RelayDown severs the server's database path (every connection dies, new ones are refused); RelayUp restores it.
// The database itself and the test's own inspection path are untouched.
func (i *Installation) RelayDown(ctx context.Context) {
	i.T.Helper()
	i.T.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if err := i.setRelay(c, 1); err != nil {
			i.T.Errorf("restore database relay: %v", err)
		}
	})
	i.scaleRelay(ctx, 0)
}

// RelayUp restores the server's database path.
func (i *Installation) RelayUp(ctx context.Context) {
	i.T.Helper()
	i.scaleRelay(ctx, 1)
}

func (i *Installation) scaleRelay(ctx context.Context, n int) {
	i.T.Helper()
	if err := i.setRelay(ctx, n); err != nil {
		i.T.Fatalf("database relay: %v", err)
	}
}

func (i *Installation) setRelay(ctx context.Context, n int) error {
	if _, err := i.Kube.Run(ctx, "-n", i.S.Namespace, "scale", "deployment/"+i.S.RelayService, fmt.Sprintf("--replicas=%d", n)); err != nil {
		return err
	}
	if n == 0 {
		if _, err := i.Kube.Run(ctx, "-n", i.S.Namespace, "wait", "--for=delete", "pod", "-l", "app.kubernetes.io/name="+i.S.RelayService, "--timeout=2m"); err != nil {
			return err
		}
		return nil
	}
	if _, err := i.Kube.Run(ctx, "-n", i.S.Namespace, "rollout", "status", "deployment/"+i.S.RelayService, "--timeout=2m"); err != nil {
		return err
	}
	return nil
}

// ---- bootstrap -----------------------------------------------------------------------------------------------

// Bootstrap provisions a tenant and its first administrator with the product's own `kodama bootstrap` command,
// run as the installation's migration identity in a short-lived Pod of the control-plane namespace.
func (i *Installation) Bootstrap(ctx context.Context, tenant Tenant, key, persona string) (string, error) {
	tok, err := i.Auth.Fresh(ctx, persona)
	if err != nil {
		return "", err
	}
	ns := i.S.Namespace
	name := "kq-bootstrap-" + random(4)
	secret := name + "-token"
	manifestSecret, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]string{"name": secret, "namespace": ns}, "data": map[string][]byte{"token": []byte(tok)}})
	if _, err := i.Kube.RunInput(ctx, manifestSecret, "create", "-f", "-"); err != nil {
		return "", err
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = i.Kube.Run(c, "-n", ns, "delete", "pod", name, "--ignore-not-found", "--now")
		_, _ = i.Kube.Run(c, "-n", ns, "delete", "secret", secret, "--ignore-not-found")
	}()
	pod := map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "namespace": ns},
		"spec": map[string]any{
			"restartPolicy": "Never", "automountServiceAccountToken": false,
			"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": 65532, "fsGroup": 65532, "seccompProfile": map[string]any{"type": "RuntimeDefault"}},
			"volumes": []any{
				map[string]any{"name": "secrets", "secret": map[string]any{"secretName": "kodama-migration", "defaultMode": 0o440}},
				map[string]any{"name": "tok", "secret": map[string]any{"secretName": secret, "defaultMode": 0o440}},
			},
			"containers": []any{map[string]any{
				"name": "bootstrap", "image": i.ServerImage(),
				"args": []string{"bootstrap", "--tenant-id", tenant.TenantID, "--tenant-key", key, "--display-name", key, "--operator-id", "operator_" + i.S.Name, "--eligible-subject", Subject(persona), "--token-file", "/tok/token"},
				"env": []any{
					map[string]any{"name": "KODAMA_ENV", "value": "production"},
					map[string]any{"name": "KODAMA_OIDC_ISSUER", "value": i.S.Issuer},
					map[string]any{"name": "KODAMA_OIDC_AUDIENCE", "value": i.S.Audience},
				},
				"volumeMounts":    []any{map[string]any{"name": "secrets", "mountPath": "/secrets", "readOnly": true}, map[string]any{"name": "tok", "mountPath": "/tok", "readOnly": true}},
				"securityContext": map[string]any{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": map[string]any{"drop": []string{"ALL"}}},
				"resources":       map[string]any{"limits": map[string]any{"memory": "128Mi"}},
			}},
		},
	}
	manifest, _ := json.Marshal(pod)
	if _, err := i.Kube.RunInput(ctx, manifest, "create", "-f", "-"); err != nil {
		return "", err
	}
	var phase string
	for deadline := time.Now().Add(3 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		phase, err = i.Kube.Run(ctx, "-n", ns, "get", "pod", name, "-o", "jsonpath={.status.phase}")
		if err != nil {
			return "", err
		}
		if phase == "Succeeded" || phase == "Failed" {
			break
		}
	}
	logs, _ := i.Kube.Run(ctx, "-n", ns, "logs", name)
	if phase != "Succeeded" {
		return "", fmt.Errorf("bootstrap Pod ended %q (logs suppressed)", phase)
	}
	return logs, nil
}

// ---- helpers shared by the fixtures ---------------------------------------------------------------------------

// Run runs a local command in dir with the operator kubeconfig and returns its combined output.
func (i *Installation) Local(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "KUBECONFIG="+i.S.Kubeconfig, "GIT_TERMINAL_PROMPT=0")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}
