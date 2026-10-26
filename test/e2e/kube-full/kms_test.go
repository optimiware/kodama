package kube_full_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	"github.com/optimiware/kodama/test/support/stage"
	"github.com/optimiware/kodama/test/support/stage/probe"
)

// All subcases are required by the gate. Provider construction on the operator machine is
// deliberately not acceptance: secret consumption must pass through the installed server.
func TestKubeSecretsKMS(t *testing.T) {
	t.Run("installed_secret_restart", installedSecretRestart)
	t.Run("direct_secret_delivery", directSecretDelivery)
	t.Run("workload_provider_procedure", workloadProviderProcedure)
}

func installedSecretRestart(t *testing.T) {
	i := stage.New(t)
	w := i.Seed()
	ctx := i.Context(12 * time.Minute)
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		t.Fatal("entropy failed")
	}
	value = []byte(hex.EncodeToString(value))
	defer clear(value)
	name := "kms-" + runSuffix(t)
	var envID string
	r, err := i.API("admin").CreateSecretWithResponse(ctx, w.TenantA, w.Project, &client.CreateSecretParams{IdempotencyKey: client.IdempotencyKey(name)}, client.CreateSecretRequest{Kind: client.SecretKindGeneric, Name: client.SecretName(name), Value: &value})
	if err != nil || r.StatusCode() != http.StatusCreated || r.JSON201 == nil {
		t.Fatal("create installed encrypted secret failed (body suppressed)")
	}
	// The installed server delivers a project secret to a step only through a pinned deployment
	// environment that names it (a bare step secret is refused or answered 403). The environment
	// needs a kubeconfig-kind credential that is never used to connect: it holds a placeholder.
	kubeName := "kubeconfig-" + runSuffix(t)
	placeholder := []byte("placeholder-kubeconfig-never-used-" + runSuffix(t))
	if r, err := i.API("admin").CreateSecretWithResponse(ctx, w.TenantA, w.Project, &client.CreateSecretParams{IdempotencyKey: client.IdempotencyKey(kubeName)}, client.CreateSecretRequest{Kind: client.SecretKindKubeconfig, Name: client.SecretName(kubeName), Value: &placeholder}); err != nil || r.StatusCode() != http.StatusCreated {
		t.Fatal("create placeholder deployment credential failed (body suppressed)")
	}
	envName := "kms-" + runSuffix(t)
	if r, err := i.API("engineer").CreateEnvironmentWithResponse(ctx, w.TenantA, w.Project, client.EnvironmentCreate{
		Kind: client.Preview, Name: envName, Target: "kms-restart", Namespace: "kms-restart",
		Kubeconfig: client.CredentialBinding{Name: "KUBECONFIG", Ref: "secret://project/" + kubeName, Delivery: client.File},
		Secrets:    &[]client.CredentialBinding{{Name: "kms-value", Ref: "secret://project/" + name, Delivery: client.File}},
	}); err != nil || r.StatusCode() != http.StatusCreated || r.JSON201 == nil {
		t.Fatalf("create environment failed: HTTP %d", statusOf(r, err))
	} else {
		envID = string(r.JSON201.Id)
	}
	// Compare a digest inside the command. The random value is never in the script, output,
	// Pod specification or evidence. The real runtime injects the pinned secret just in time.
	hash := sha256.Sum256(value)
	script := fmt.Sprintf(`f=$(echo /kodama/secrets/*/kms-value); test -f "$f"; actual=$(sha256sum < "$f"); test "${actual%%%% *}" = %s; echo secret-consumed`, hex.EncodeToString(hash[:]))
	def := w.Definition("kms-consumer", stage.Step{Script: script, Environment: envID})
	consume := func(label string) {
		item := w.WorkItem(label, "# encrypted secret use")
		w.Provision(item.Id)
		run := w.Run(label, item, def.Id)
		if final := w.WaitRun(run.Id, 4*time.Minute); final.Status != client.RunStatusSucceeded {
			t.Fatalf("%s failed: %s (%s)", label, final.Status, final.Reason)
		}
		w.WaitRemoved(w.Attempts(run.Id)[0].Id)
		logs := strings.Join(w.WaitLogs("engineer", run.Id), "\n")
		if strings.Contains(logs, string(value)) || !strings.Contains(logs, "secret-consumed") {
			t.Fatal("secret use evidence missing or plaintext leaked")
		}
	}
	consume("before-kms-restart")
	i.Crash(i.Context(6 * time.Minute))
	consume("after-kms-restart")
}

func workloadProviderProcedure(t *testing.T) {
	i := stage.New(t)
	i.EnsureReady(i.Context(5 * time.Minute))
	_ = i.ProbeImage() // Fail before creating any cloud resources.
	op := newOperator(t)
	project := os.Getenv("SCW_DEFAULT_PROJECT_ID")
	if !cloudID.MatchString(project) {
		t.Fatal("SCW_DEFAULT_PROJECT_ID must identify the stage project")
	}
	suffix := runSuffix(t)
	a := createKey(t, op, project, "t028-a-"+i.S.Name+"-"+suffix)
	b := createKey(t, op, project, "t028-b-"+i.S.Name+"-"+suffix)
	denied := createDeniedIdentity(t, op, project, "t028-denied-"+i.S.Name+"-"+suffix)
	i.RunKMSProbe(i.Context(8*time.Minute), probe.KMSConfig{A: a, B: b, Installation: i.S.InstallationID, Tenant: i.S.TenantA().TenantID}, denied)
}

func runSuffix(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal("entropy failed")
	}
	return hex.EncodeToString(raw)
}

type operator struct {
	token  string
	base   string
	client *http.Client
}

func newOperator(t *testing.T) operator {
	t.Helper()
	token := os.Getenv("SCW_SECRET_KEY")
	if token == "" {
		// Resolve the selected user through kubectl, not by taking the first token in a YAML file.
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "kubectl", "config", "view", "--minify", "--raw", "-o", "jsonpath={.users[0].user.token}").Output()
		if err != nil {
			t.Fatal("operator credential unavailable")
		}
		token = strings.TrimSpace(string(out))
	}
	if token == "" {
		t.Fatal("SCW_SECRET_KEY or an operator kubeconfig token is required")
	}
	return operator{token: token, base: "https://api.scaleway.com", client: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// call returns status and decodes only into an allowlisted caller struct. No bodies/credentials
// enter errors or test logs, including transport and JSON decode failures.
func (o operator) call(ctx context.Context, method, path string, body, out any) (int, error) {
	var input io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("encode operator request")
		}
		input = bytes.NewReader(data)
	}
	r, err := http.NewRequestWithContext(ctx, method, o.base+path, input)
	if err != nil {
		return 0, fmt.Errorf("construct operator request")
	}
	r.Header.Set("X-Auth-Token", o.token)
	r.Header.Set("Content-Type", "application/json")
	resp, err := o.client.Do(r)
	if err != nil {
		return 0, fmt.Errorf("operator transport failed")
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out) != nil {
			return resp.StatusCode, fmt.Errorf("decode operator response")
		}
	}
	return resp.StatusCode, nil
}

const keyPath = "/key-manager/v1alpha1/regions/fr-par/keys"

var cloudID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func createKey(t *testing.T, o operator, project, name string) string {
	t.Helper()
	var out struct {
		ID        string `json:"id"`
		Protected *bool  `json:"protected"`
	}
	// protected is response metadata; unprotected is the provider's create field.
	body := map[string]any{"name": name, "project_id": project, "usage": map[string]string{"symmetric_encryption": "aes_256_gcm"}, "unprotected": true}
	code, err := o.call(t.Context(), http.MethodPost, keyPath, body, &out)
	if cloudID.MatchString(out.ID) {
		// Register immediately: a later create or a response-validation failure must not leak A.
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := deleteKey(ctx, o, out.ID); err != nil {
				t.Errorf("cleanup throwaway key %s: %v", out.ID, err)
			}
		})
	}
	if err != nil || code != http.StatusOK && code != http.StatusCreated || !cloudID.MatchString(out.ID) {
		t.Fatalf("create throwaway key: HTTP %d (details suppressed)", code)
	}
	if out.Protected == nil || *out.Protected {
		t.Fatal("provider created a protected throwaway key despite unprotected:true")
	}
	return out.ID
}

func deleteKey(ctx context.Context, o operator, id string) error {
	code, err := o.call(ctx, http.MethodPost, keyPath+"/"+id+"/unprotect", map[string]any{}, nil)
	if code == http.StatusNotFound {
		return nil
	}
	if err != nil || code != http.StatusOK {
		return fmt.Errorf("unprotect: HTTP %d", code)
	}
	code, err = o.call(ctx, http.MethodDelete, keyPath+"/"+id, nil, nil)
	if err != nil || code != http.StatusNoContent && code != http.StatusNotFound {
		return fmt.Errorf("delete: HTTP %d", code)
	}
	return nil
}

func createDeniedIdentity(t *testing.T, o operator, project, name string) string {
	t.Helper()
	var p struct {
		OrganizationID string `json:"organization_id"`
	}
	code, err := o.call(t.Context(), http.MethodGet, "/account/v3/projects/"+project, nil, &p)
	if err != nil || code != http.StatusOK || !cloudID.MatchString(p.OrganizationID) {
		t.Fatalf("read stage project: HTTP %d", code)
	}
	var app struct {
		ID string `json:"id"`
	}
	code, err = o.call(t.Context(), http.MethodPost, "/iam/v1alpha1/applications", map[string]string{"name": name, "organization_id": p.OrganizationID}, &app)
	if cloudID.MatchString(app.ID) {
		t.Cleanup(func() { cleanupCloud(t, o, "/iam/v1alpha1/applications/"+app.ID) })
	}
	if err != nil || code != http.StatusOK && code != http.StatusCreated || !cloudID.MatchString(app.ID) {
		t.Fatalf("create denied application: HTTP %d", code)
	}
	// No IAM policy is attached: this credential is valid, but cannot decrypt or administer keys.
	var key struct {
		Access string `json:"access_key"`
		Secret string `json:"secret_key"`
	}
	code, err = o.call(t.Context(), http.MethodPost, "/iam/v1alpha1/api-keys", map[string]any{"application_id": app.ID, "default_project_id": project, "description": name, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}, &key)
	if key.Access != "" {
		t.Cleanup(func() { cleanupCloud(t, o, "/iam/v1alpha1/api-keys/"+key.Access) })
	}
	if err != nil || code != http.StatusOK && code != http.StatusCreated || key.Access == "" || key.Secret == "" {
		t.Fatalf("create denied credential: HTTP %d", code)
	}
	return key.Secret
}

func cleanupCloud(t *testing.T, o operator, path string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	code, err := o.call(ctx, http.MethodDelete, path, nil, nil)
	if err != nil || code != http.StatusNoContent && code != http.StatusNotFound {
		t.Errorf("cleanup throwaway IAM resource: HTTP %d", code)
	}
}

func statusOf(r *client.CreateEnvironmentResponse, err error) int {
	if err != nil || r == nil {
		return 0
	}
	return r.StatusCode()
}
