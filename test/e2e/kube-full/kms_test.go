package kube_full_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/internal/secrets/kms/production"
	"github.com/optimiware/kodama/test/support/stage"
)

// TestKubeSecretsKMS is kube_secrets_kms: the real-provider procedure with the deployed workload
// identity and throwaway Key Manager keys. Plaintext and ciphertext are never logged. Only the
// throwaway keys are deleted, and only after use.
func TestKubeSecretsKMS(t *testing.T) {
	if os.Getenv("KODAMA_KMS_OUTAGE_CHILD") == "1" {
		outageChild()
		return
	}
	i := stage.New(t)
	ctx := i.Context(8 * time.Minute)
	token := workloadToken(t, i)
	op := operatorToken(t)
	a := createKey(t, op, "t028-a-"+i.S.Name)
	b := createKey(t, op, "t028-b-"+i.S.Name)
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		deleteKey(c, op, a)
		deleteKey(c, op, b)
	})
	t.Logf("throwaway keys created provider=scw region=fr-par")

	install := i.S.InstallationID
	wc := secrets.WrapContext{InstallationID: install, TenantID: domain.TenantID(i.S.TenantA().TenantID), Generation: 1, KeyID: "sk_t104"}
	cfg := production.Config{Provider: production.Scaleway, KEKID: "t104-throwaway", Current: "v1", Resources: map[string]string{"v1": a}, ProbeTimeout: 20 * time.Second}
	p, err := production.New(ctx, cfg, production.Clients{ScalewayToken: token})
	if err != nil {
		t.Fatalf("startup probe: %v", err)
	}
	material := make([]byte, 32)
	if _, err := rand.Read(material); err != nil {
		t.Fatal(err)
	}
	wrapped, err := p.Wrap(ctx, wc, material)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	got, err := p.Unwrap(ctx, wc, wrapped)
	if err != nil || !bytes.Equal(got, material) {
		t.Fatal("round trip failed")
	}

	both := production.Config{Provider: production.Scaleway, KEKID: cfg.KEKID, Current: "v1", Resources: map[string]string{"v1": a, "v2": b}, ProbeTimeout: 20 * time.Second}
	p2, err := production.New(ctx, both, production.Clients{ScalewayToken: token})
	if err != nil {
		t.Fatalf("both keys: %v", err)
	}
	for _, mutate := range []func(*secrets.WrapContext){
		func(w *secrets.WrapContext) { w.InstallationID = "other" },
		func(w *secrets.WrapContext) { w.TenantID = "t_other" },
		func(w *secrets.WrapContext) { w.KeyID = "sk_other" },
		func(w *secrets.WrapContext) { w.Generation = 9 },
	} {
		bad := wc
		mutate(&bad)
		if _, err := p2.Unwrap(ctx, bad, wrapped); keyClass(err) != secrets.KeyCorrupt {
			t.Fatalf("tamper class %v, want corrupt", err)
		}
	}
	flipped := wrapped
	flipped.Bytes = append([]byte(nil), wrapped.Bytes...)
	if len(flipped.Bytes) == 0 {
		t.Fatal("wrap produced no bytes")
	}
	flipped.Bytes[0] ^= 0xff
	if _, err := p2.Unwrap(ctx, wc, flipped); keyClass(err) != secrets.KeyCorrupt {
		t.Fatal("flipped ciphertext was not corrupt")
	}

	rot := production.Config{Provider: production.Scaleway, KEKID: cfg.KEKID, Current: "v2", Resources: map[string]string{"v1": a, "v2": b}, ProbeTimeout: 20 * time.Second}
	p3, err := production.New(ctx, rot, production.Clients{ScalewayToken: token})
	if err != nil {
		t.Fatalf("rotation: %v", err)
	}
	if recovered, err := p3.Unwrap(ctx, wc, wrapped); err != nil || !bytes.Equal(recovered, material) {
		t.Fatal("retained v1 unwrap failed")
	}
	again, err := p3.Wrap(ctx, wc, material)
	if err != nil || again.KEK.Version != "v2" {
		t.Fatal("new wraps must record v2")
	}

	fresh, err := production.New(ctx, rot, production.Clients{ScalewayToken: token})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.Unwrap(ctx, wc, wrapped); err != nil {
		t.Fatal("fresh process could not unwrap the retained wrap")
	}
	dropped := production.Config{Provider: production.Scaleway, KEKID: cfg.KEKID, Current: "v2", Resources: map[string]string{"v2": b}, ProbeTimeout: 20 * time.Second}
	p4, err := production.New(ctx, dropped, production.Clients{ScalewayToken: token})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p4.Unwrap(ctx, wc, wrapped); keyClass(err) != secrets.KeyNotFound {
		t.Fatal("removing v1 was not KeyNotFound")
	}

	if _, err := production.New(ctx, cfg, production.Clients{ScalewayToken: "00000000-0000-4000-8000-000000000000"}); keyClass(err) != secrets.KeyDenied && keyClass(err) != secrets.KeyUnavailable {
		t.Fatalf("denied identity class %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run", "^TestKubeSecretsKMS$", "-test.count=1")
	cmd.Env = append(os.Environ(), "KODAMA_KMS_OUTAGE_CHILD=1", "HTTPS_PROXY=http://127.0.0.1:1", "HTTP_PROXY=http://127.0.0.1:1", "KODAMA_KMS_KEY="+a, "KODAMA_KMS_TOKEN="+token, "KODAMA_KMS_INSTALL="+install)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("outage child: %v %s", err, redact(out))
	}
	t.Log("kms procedure passed provider=scw")
}

func outageChild() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cfg := production.Config{Provider: production.Scaleway, KEKID: "t104-throwaway", Current: "v1", Resources: map[string]string{"v1": os.Getenv("KODAMA_KMS_KEY")}, ProbeTimeout: 5 * time.Second}
	_, err := production.New(ctx, cfg, production.Clients{ScalewayToken: os.Getenv("KODAMA_KMS_TOKEN")})
	if keyClass(err) == secrets.KeyUnavailable {
		os.Exit(0)
	}
	os.Exit(1)
}

func workloadToken(t *testing.T, i *stage.Installation) string {
	t.Helper()
	tok, err := i.Kube.SecretValue(i.Context(time.Minute), i.S.Namespace, i.S.ProbeSecret, "owner-url")
	_ = tok
	// The workload Key Manager credential is the session's kodama-kms-scw secret, not the probe URL.
	secret := "kodama-kms-scw"
	val, err := i.Kube.SecretValue(i.Context(time.Minute), i.S.Namespace, secret, "secret-key")
	if err != nil || val == "" {
		t.Fatal("workload key manager credential is not mounted")
	}
	return strings.TrimSpace(val)
}

func operatorToken(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(os.Getenv("KUBECONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "token:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "token:"))
		}
	}
	t.Fatal("operator credential is not in the kubeconfig")
	return ""
}

func createKey(t *testing.T, token, name string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"name": name, "project_id": "943031a5-cc3c-4202-aea1-697e4e55863c",
		"usage": map[string]string{"symmetric_encryption": "aes_256_gcm"}, "protected": false,
	})
	req, _ := http.NewRequest(http.MethodPost, "https://api.scaleway.com/key-manager/v1alpha1/regions/fr-par/keys", bytes.NewReader(body))
	req.Header.Set("X-Auth-Token", token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode >= 300 || out.ID == "" {
		t.Fatalf("create throwaway key: HTTP %d", resp.StatusCode)
	}
	return out.ID
}

func deleteKey(ctx context.Context, token, id string) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, "https://api.scaleway.com/key-manager/v1alpha1/regions/fr-par/keys/"+id, nil)
	req.Header.Set("X-Auth-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

func keyClass(err error) secrets.KeyErrorClass {
	var ke *secrets.KeyError
	if errors.As(err, &ke) {
		return ke.Class
	}
	return ""
}

func redact(b []byte) string {
	s := string(b)
	if i := strings.Index(s, "KODAMA_KMS_TOKEN"); i >= 0 {
		s = s[:i] + "[redacted]"
	}
	if len(s) > 400 {
		s = s[:400]
	}
	return s
}
