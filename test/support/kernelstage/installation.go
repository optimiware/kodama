// Package kernelstage is the fixture of the ADR 0013 kernel qualification suite.
// It reuses the stage cluster access, OIDC sign-in and database inspection of
// test/support/stage, and adds the kernel's API, seed procedure and Git client.
package kernelstage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/test/support/stage"
)

// Installation is a prepared qualification session of the kernel chart.
type Installation struct {
	T testing.TB
	S *stage.Session
	K stage.Kube

	stage *stage.Installation
}

// New loads the session and points the fixture at the kernel chart.
func New(t testing.TB) *Installation {
	t.Helper()
	base := stage.New(t)
	root, err := goModRoot()
	if err != nil {
		t.Fatal(err)
	}
	base.S.ChartDir = filepath.Join(root, "deploy", "charts", "kodama-kernel")
	return &Installation{T: t, S: base.S, K: base.Kube, stage: base}
}

func goModRoot() (string, error) {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return "", err
	}
	return filepath.Dir(strings.TrimSpace(string(out))), nil
}

func (i *Installation) Context(d time.Duration) context.Context { return i.stage.Context(d) }

func (i *Installation) Installed(ctx context.Context) bool { return i.stage.Installed(ctx) }

func (i *Installation) Install(ctx context.Context) error { return i.stage.Install(ctx) }

func (i *Installation) Uninstall(ctx context.Context) error { return i.stage.Uninstall(ctx) }

func (i *Installation) WaitReady(ctx context.Context) { i.stage.WaitReady(ctx) }

func (i *Installation) Deployment() string {
	if strings.Contains(i.S.Release, "kodama-kernel") {
		return i.S.Release
	}
	return i.S.Release + "-kodama-kernel"
}

func (i *Installation) Scale(ctx context.Context, replicas int) error {
	if _, err := i.K.Run(ctx, "-n", i.S.Namespace, "scale", "deployment/"+i.Deployment(), "--replicas="+fmt.Sprint(replicas)); err != nil {
		return err
	}
	if replicas == 0 {
		_, err := i.K.Run(ctx, "-n", i.S.Namespace, "wait", "--for=delete", "pod", "-l", "app.kubernetes.io/component=server", "--timeout=3m")
		return err
	}
	_, err := i.K.Run(ctx, "-n", i.S.Namespace, "rollout", "status", "deployment/"+i.Deployment(), "--timeout=5m")
	return err
}

func (i *Installation) Start(ctx context.Context) {
	i.T.Helper()
	if err := i.Scale(ctx, 1); err != nil {
		i.T.Fatalf("start the server: %v", err)
	}
	i.WaitReady(ctx)
}

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

func (i *Installation) Restart(ctx context.Context) {
	i.T.Helper()
	i.Stop(ctx)
	i.Start(ctx)
}
func (i *Installation) RelayDown(ctx context.Context) { i.stage.RelayDown(ctx) }
func (i *Installation) RelayUp(ctx context.Context)   { i.stage.RelayUp(ctx) }
func (i *Installation) DB() *stage.DB                 { return i.stage.DB() }
func (i *Installation) OwnerCount(sql string, args ...any) int {
	return i.stage.OwnerCount(sql, args...)
}
func (i *Installation) MustScan(dst any, sql string, args ...any) {
	i.stage.MustScan(dst, sql, args...)
}

// Token returns a persona's access token.
func (i *Installation) Token(persona string) string {
	i.T.Helper()
	tok, err := i.stage.Auth.Token(i.Context(2*time.Minute), persona)
	if err != nil {
		i.T.Fatalf("sign in %s: %v", persona, err)
	}
	return tok
}

// Probe reads one unauthenticated endpoint. A transport error is returned, so a
// test can observe a dependency outage without failing on the first attempt.
func (i *Installation) Probe(ctx context.Context, path string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, i.S.URL+path, nil)
	if err != nil {
		return 0, err
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// ServerImage is the digest-pinned release image under test. The same image is
// a usable shell step image (it carries git and ca-certificates).
func (i *Installation) ServerImage() string {
	var v struct {
		Image struct {
			Repository string `yaml:"repository"`
			Digest     string `yaml:"digest"`
		} `yaml:"image"`
	}
	i.stage.ReadValues(&v)
	return v.Image.Repository + "@" + v.Image.Digest
}

// Seed creates one tenant key and one persona binding with the kernel's own
// owner command, run in a short-lived Pod of the control-plane namespace.
func (i *Installation) Seed(ctx context.Context, tenantKey, persona string, roles ...string) {
	i.T.Helper()
	if len(roles) == 0 {
		roles = []string{"admin"}
	}
	name := "kq-seed-" + random(4)
	args := []string{"seed", "--tenant", tenantKey, "--user-id", "u_" + persona,
		"--issuer", i.S.Issuer, "--subject", stage.Subject(persona), "--name", persona}
	for _, r := range roles {
		args = append(args, "--role", r)
	}
	manifest := map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "namespace": i.S.Namespace},
		"spec": map[string]any{
			"restartPolicy": "Never", "automountServiceAccountToken": false,
			"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": 65532, "fsGroup": 65532,
				"seccompProfile": map[string]any{"type": "RuntimeDefault"}},
			"volumes": []any{map[string]any{"name": "secrets", "secret": map[string]any{
				"secretName": "kodama-kernel-migration", "defaultMode": 0o440}}},
			"containers": []any{map[string]any{
				"name": "seed", "image": i.ServerImage(), "args": args,
				"env":             []any{map[string]any{"name": "KODAMA_SECRETS_DIR", "value": "/secrets"}},
				"volumeMounts":    []any{map[string]any{"name": "secrets", "mountPath": "/secrets", "readOnly": true}},
				"securityContext": map[string]any{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": map[string]any{"drop": []string{"ALL"}}},
				"resources":       map[string]any{"limits": map[string]any{"memory": "128Mi"}},
			}},
		},
	}
	raw, _ := json.Marshal(manifest)
	if _, err := i.K.RunInput(ctx, raw, "create", "-f", "-"); err != nil {
		i.T.Fatalf("create seed Pod: %v", err)
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = i.K.Run(c, "-n", i.S.Namespace, "delete", "pod", name, "--ignore-not-found", "--now")
	}()
	var phase string
	for deadline := time.Now().Add(3 * time.Minute); time.Now().Before(deadline); time.Sleep(time.Second) {
		phase, _ = i.K.Run(ctx, "-n", i.S.Namespace, "get", "pod", name, "-o", "jsonpath={.status.phase}")
		if phase == "Succeeded" || phase == "Failed" {
			break
		}
	}
	if phase != "Succeeded" {
		i.T.Fatalf("seed Pod for %s ended %q", tenantKey, phase)
	}
}

func (i *Installation) JSON(ctx context.Context, method, path, token string, body any, out any) (int, []byte) {
	i.T.Helper()
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			i.T.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, i.S.URL+path, rd)
	if err != nil {
		i.T.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		i.T.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			i.T.Fatalf("%s %s: decode: %v: %s", method, path, err, raw)
		}
	}
	return resp.StatusCode, raw
}

func random(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", b)[:n]
}
