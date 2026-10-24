package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
	awskms "github.com/aws/aws-sdk-go-v2/service/kms"
	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/internal/secrets/kms/production"
)

const startupKey = "11111111-1111-4111-8111-111111111111"

func cloudSecrets(provider string) SecretsConfig {
	k := &CloudKMSConfig{Provider: provider, KEKID: "installation", Current: "v1", Resources: map[string]string{"v1": startupKey}}
	switch provider {
	case "aws":
		k.AWS = &AWSKMSConfig{Region: "eu-west-1"}
	case "gcp":
		k.Resources["v1"] = "projects/p/locations/global/keyRings/r/cryptoKeys/k"
	case "scw":
		k.SCW = &SCWKMSConfig{CredentialsFile: "/secrets/scw"}
	}
	return SecretsConfig{Adapter: "kms", KMS: k}
}

func TestUnitSecretsConfig(t *testing.T) {
	for _, provider := range []string{"aws", "gcp", "scw"} {
		c := cloudSecrets(provider)
		if err := c.Validate(Production); err != nil {
			t.Fatal(err)
		}
		// The full YAML -> chart -> JSON environment -> typed config bridge.
		b, err := yaml.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		var doc ChartConfig
		if err := yaml.Unmarshal(append([]byte("secrets:\n"), []byte(indentText(2, string(b)))...), &doc); err != nil {
			t.Fatal(err)
		}
		doc.Authority, doc.S3.Credentials = "standalone", "static"
		env, err := doc.Environment("/secrets")
		if err != nil {
			t.Fatal(err)
		}
		got, err := loadSecretsConfig(func(k string) string { return env[k] }, "/secrets")
		if err != nil || got.Validate(Production) != nil || got.KMS.Resources["v1"] != c.KMS.Resources["v1"] {
			t.Fatalf("roundtrip: %+v %v", got, err)
		}
	}
	for name, mutate := range map[string]func(*SecretsConfig){
		"missing adapter":   func(c *SecretsConfig) { c.Adapter = "" },
		"mixed adapters":    func(c *SecretsConfig) { c.Keyfile = &KeyfileConfig{} },
		"old cloud name":    func(c *SecretsConfig) { c.KMS.Provider = "scaleway" },
		"foreign config":    func(c *SecretsConfig) { c.KMS.SCW = &SCWKMSConfig{CredentialsFile: "/x"} },
		"mutable key alias": func(c *SecretsConfig) { c.KMS.Resources["v1"] = "alias/kodama" },
		"missing current":   func(c *SecretsConfig) { c.KMS.Current = "v2" },
		"unbounded startup": func(c *SecretsConfig) { c.KMS.StartupTimeout = 3 * time.Minute },
	} {
		t.Run(name, func(t *testing.T) {
			c := cloudSecrets("aws")
			mutate(&c)
			called := false
			_, _, err := buildSecretsWrapper(context.Background(), Production, c, func(context.Context, CloudKMSConfig) (production.Clients, func(), error) {
				called = true
				return production.Clients{}, nil, nil
			})
			if err == nil || called {
				t.Fatalf("invalid config reached client creation: %v %v", called, err)
			}
		})
	}
	for _, raw := range []string{`{"adapter":"keyfile","typo":1}`, `{} {}`, `{"adapter":"kms","kms":{"token":"do-not-print"}}`} {
		_, err := loadSecretsConfig(func(k string) string {
			if k == "KODAMA_SECRETS_CONFIG" {
				return raw
			}
			return ""
		}, "/secrets")
		if err == nil || strings.Contains(err.Error(), "do-not-print") {
			t.Fatalf("strict/redacted decoding: %v", err)
		}
	}
}

type kmsTransport func(*http.Request) (*http.Response, error)

func (f kmsTransport) Do(r *http.Request) (*http.Response, error) { return f(r) }

// The SDK wire double echoes bytes; provider cryptography and authenticated
// bindings are qualified by the existing pkg/secretkeys protocol tests.
func startupAWS(deny bool) *awskms.Client {
	return awskms.New(awskms.Options{Region: "eu-west-1", Credentials: credentials.NewStaticCredentialsProvider("fixture", "fixture", ""), HTTPClient: kmsTransport(func(r *http.Request) (*http.Response, error) {
		if deny {
			return &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"__type":"AccessDeniedException","message":"do-not-print"}`))}, nil
		}
		var in map[string]any
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return nil, err
		}
		out := map[string]any{"KeyId": in["KeyId"]}
		if strings.HasSuffix(r.Header.Get("X-Amz-Target"), ".Encrypt") {
			out["CiphertextBlob"] = in["Plaintext"]
		} else {
			out["Plaintext"] = in["CiphertextBlob"]
		}
		data, _ := json.Marshal(out)
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(data))}, nil
	})})
}

func TestUnitSecretsStartup(t *testing.T) {
	for _, deny := range []bool{false, true} {
		closed := 0
		w, cleanup, err := buildSecretsWrapper(context.Background(), Production, cloudSecrets("aws"), func(ctx context.Context, c CloudKMSConfig) (production.Clients, func(), error) {
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("client discovery has no deadline")
			}
			return production.Clients{AWS: startupAWS(deny)}, func() { closed++ }, nil
		})
		if deny {
			if err == nil || w != nil || closed != 1 || strings.Contains(err.Error(), "do-not-print") {
				t.Fatalf("failed probe cleanup/redaction: %v %d", err, closed)
			}
			continue
		}
		if err != nil || w == nil || closed != 0 {
			t.Fatalf("startup: %v %d", err, closed)
		}
		wc := secrets.WrapContext{InstallationID: "inst", TenantID: "t_one", KeyID: "sk_one", Generation: 1}
		wrapped, err := w.Wrap(context.Background(), wc, bytes.Repeat([]byte{7}, 32))
		if err != nil {
			t.Fatal(err)
		}
		plain, err := w.Unwrap(context.Background(), wc, wrapped)
		if err != nil || !bytes.Equal(plain, bytes.Repeat([]byte{7}, 32)) {
			t.Fatal("injected wrapper unusable", err)
		}
		core := &Core{closers: []func(){cleanup}}
		core.closeConnections()
		core.closeConnections()
		cleanup()
		if closed != 1 {
			t.Fatalf("cleanup called %d times", closed)
		}
	}
	for _, provider := range []string{"aws", "gcp", "scw"} {
		c := cloudSecrets(provider)
		c.KMS.StartupTimeout = 10 * time.Millisecond
		closed := false
		_, _, err := buildSecretsWrapper(context.Background(), Production, c, func(ctx context.Context, cfg CloudKMSConfig) (production.Clients, func(), error) {
			if cfg.Provider != provider {
				t.Fatal("wrong cloud selected")
			}
			<-ctx.Done()
			return production.Clients{}, func() { closed = true }, ctx.Err()
		})
		if !errors.Is(err, context.DeadlineExceeded) || !closed {
			t.Fatalf("discovery timeout: %v %v", err, closed)
		}
	}
}

func TestUnitSCWCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret-key")
	c := cloudSecrets("scw")
	c.KMS.SCW.CredentialsFile = path
	if c.KMS.productionConfig().Provider != production.Scaleway {
		t.Fatal("persisted provider identity changed")
	}
	if _, _, err := buildKMSClients(context.Background(), *c.KMS); err == nil {
		t.Fatal("missing file accepted")
	}
	if err := os.WriteFile(path, []byte("fixture-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	clients, cleanup, err := buildKMSClients(context.Background(), *c.KMS)
	defer cleanup()
	if err != nil || clients.ScalewayToken != "fixture-token" || clients.AWS != nil || clients.GCP != nil {
		t.Fatal("scw credentials", err)
	}
}
