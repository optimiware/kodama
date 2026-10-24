package production

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/kms"

	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/pkg/secretkeys"
)

// recorder is a builder that counts which clouds were constructed.
type recorder struct {
	built map[string]*atomic.Int64
	fake  *fakeInner
}

func newRecorder() *recorder {
	return &recorder{built: map[string]*atomic.Int64{AWS: {}, GCP: {}, Scaleway: {}}}
}

func (r *recorder) build(p plan, _ Clients) (secretkeys.Provider, error) {
	r.built[p.provider].Add(1)
	r.fake = newFake(p.provider, p.format, p.current.keyRef, keyRefs(p.retained)...)
	return r.fake, nil
}

func keyRefs(rs []resource) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.keyRef)
	}
	return out
}

func (r *recorder) total() int64 {
	var n int64
	for _, c := range r.built {
		n += c.Load()
	}
	return n
}

func clientsFor(cloud string) Clients {
	switch cloud {
	case AWS:
		return Clients{AWS: kms.New(kms.Options{Region: "eu-west-1"})}
	case Scaleway:
		return Clients{ScalewayToken: "scw-secret-token"}
	}
	return Clients{}
}

func scalewayConfig() Config {
	return Config{Provider: Scaleway, KEKID: "k", Current: "v1", Resources: map[string]string{"v1": scwKeyA}}
}

// Test_kms_provider_required (VS-09: no fallback): a production profile without a usable selected
// provider is a startup error with no adapter, and never falls back.
func Test_kms_provider_required(t *testing.T) {
	ctx := context.Background()
	t.Run("selection", func(t *testing.T) {
		for name, tc := range map[string]struct {
			cfg  Config
			want error
		}{
			"none":          {Config{}, ErrProviderRequired},
			"none+settings": {with(awsConfig(), func(c *Config) { c.Provider = "" }), ErrProviderRequired},
			"keyfile":       {with(awsConfig(), func(c *Config) { c.Provider = "keyfile" }), ErrUnsupportedProvider},
			"development":   {with(awsConfig(), func(c *Config) { c.Provider = "keyfile-dev" }), ErrUnsupportedProvider},
			"ephemeral":     {with(awsConfig(), func(c *Config) { c.Provider = "ephemeral" }), ErrUnsupportedProvider},
			"unknown":       {with(awsConfig(), func(c *Config) { c.Provider = "azure" }), ErrUnsupportedProvider},
			"no versions":   {with(awsConfig(), func(c *Config) { c.Resources = nil }), ErrInvalidConfig},
		} {
			rec := newRecorder()
			// Even with every credential present, no cloud is chosen for the caller.
			all := Clients{AWS: clientsFor(AWS).AWS, ScalewayToken: "x"}
			p, err := construct(ctx, tc.cfg, all, rec.build)
			if p != nil || !errors.Is(err, tc.want) || rec.total() != 0 {
				t.Errorf("%s: provider=%v err=%v built=%d", name, p != nil, err, rec.total())
			}
		}
	})
	t.Run("credentials", func(t *testing.T) {
		for name, tc := range map[string]struct {
			cfg     Config
			clients Clients
		}{
			"aws without client":      {awsConfig(), Clients{}},
			"aws with scaleway token": {awsConfig(), Clients{ScalewayToken: "tok"}},
			"gcp without client":      {Config{Provider: GCP, KEKID: "k", Current: "v1", Resources: map[string]string{"v1": gcpKeyA}}, Clients{}},
			"scaleway no token":       {scalewayConfig(), Clients{}},
			"scaleway with aws":       {scalewayConfig(), Clients{AWS: clientsFor(AWS).AWS}},
			"two clouds supplied":     {awsConfig(), Clients{AWS: clientsFor(AWS).AWS, ScalewayToken: "tok"}},
		} {
			rec := newRecorder()
			p, err := construct(ctx, tc.cfg, tc.clients, rec.build)
			if p != nil || !errors.Is(err, ErrCredentials) || rec.total() != 0 {
				t.Errorf("%s: provider=%v err=%v built=%d", name, p != nil, err, rec.total())
			}
		}
	})
	t.Run("pkg constructor rejection", func(t *testing.T) {
		// A real builder with a nil client never panics or falls back.
		p, err := construct(ctx, awsConfig(), Clients{}, buildCloud)
		if p != nil || err == nil {
			t.Fatal("constructed without credentials")
		}
		bad := with(scalewayConfig(), func(c *Config) { c.Resources["v1"] = "fr-par/" + scwKeyA })
		if _, err := New(ctx, bad, Clients{}); !errors.Is(err, ErrCredentials) {
			t.Fatal(err)
		}
	})
	t.Run("startup probe", func(t *testing.T) {
		secretText := "scw-secret-token"
		for name, fail := range map[string]error{
			"denied":      secretkeys.ErrDenied,
			"unavailable": secretkeys.ErrUnavailable,
			"missing key": secretkeys.ErrNotFound,
			"rejected":    secretkeys.ErrCiphertext,
		} {
			rec := newRecorder()
			build := func(p plan, c Clients) (secretkeys.Provider, error) {
				inner, _ := rec.build(p, c)
				rec.fake.fail(fail)
				return inner, nil
			}
			p, err := construct(ctx, scalewayConfig(), clientsFor(Scaleway), build)
			var ke *secrets.KeyError
			if p != nil || !errors.Is(err, ErrStartupProbe) || !errors.As(err, &ke) {
				t.Errorf("%s: provider=%v err=%v", name, p != nil, err)
			}
			if err != nil && strings.Contains(err.Error(), secretText) {
				t.Errorf("%s leaked the credential: %v", name, err)
			}
			if rec.total() != 1 || rec.built[Scaleway].Load() != 1 {
				t.Errorf("%s: contacted clouds %v", name, rec.built)
			}
		}
		// A provider returning other key material fails the comparison.
		build := func(p plan, c Clients) (secretkeys.Provider, error) { return wrongKey{}, nil }
		if p, err := construct(ctx, scalewayConfig(), clientsFor(Scaleway), build); p != nil || !errors.Is(err, ErrStartupProbe) {
			t.Fatalf("mismatch: %v", err)
		}
		// A provider that never answers is bounded by the probe timeout.
		rec := newRecorder()
		hang := func(p plan, c Clients) (secretkeys.Provider, error) {
			inner, _ := rec.build(p, c)
			rec.fake.block = make(chan struct{})
			return inner, nil
		}
		cfg := with(scalewayConfig(), func(c *Config) { c.ProbeTimeout = 50 * time.Millisecond })
		start := time.Now()
		p, err := construct(ctx, cfg, clientsFor(Scaleway), hang)
		if p != nil || !errors.Is(err, ErrStartupProbe) || time.Since(start) > 5*time.Second {
			t.Fatalf("timeout: %v after %s", err, time.Since(start))
		}
		// A cancelled caller fails startup.
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if p, err := construct(cctx, scalewayConfig(), clientsFor(Scaleway), newRecorder().build); p != nil || !errors.Is(err, ErrStartupProbe) {
			t.Fatalf("cancelled: %v", err)
		}
	})
	t.Run("only the selected cloud is contacted", func(t *testing.T) {
		for _, tc := range []struct {
			cfg   Config
			cloud string
		}{
			{awsConfig(), AWS},
			{Config{Provider: GCP, KEKID: "k", Current: "v1", Resources: map[string]string{"v1": gcpKeyA}}, GCP},
			{scalewayConfig(), Scaleway},
		} {
			rec := newRecorder()
			clients := clientsFor(tc.cloud)
			if tc.cloud == GCP {
				clients = Clients{GCP: gcpClient(t)}
			}
			p, err := construct(ctx, tc.cfg, clients, rec.build)
			if err != nil || p == nil {
				t.Fatalf("%s: %v", tc.cloud, err)
			}
			if rec.total() != 1 || rec.built[tc.cloud].Load() != 1 {
				t.Fatalf("%s: built %v", tc.cloud, rec.built)
			}
			if got := p.CurrentRef(); got.Provider != tc.cloud {
				t.Fatalf("reference %+v", got)
			}
			if rec.fake.calls.Load() != 2 {
				t.Fatalf("%s: probe made %d calls, want one wrap and one unwrap", tc.cloud, rec.fake.calls.Load())
			}
		}
	})
	t.Run("credentials are redacted", func(t *testing.T) {
		c := Clients{ScalewayToken: "scw-secret-token"}
		for _, s := range []string{fmt.Sprint(c), fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c)} {
			if strings.Contains(s, "scw-secret-token") {
				t.Fatalf("token printed: %s", s)
			}
		}
	})
}

type wrongKey struct{}

func (wrongKey) Wrap(_ context.Context, key, _ []byte) (secretkeys.Wrapped, error) {
	return secretkeys.Wrapped{Provider: Scaleway, KeyRef: "fr-par/" + scwKeyA, Format: secretkeys.FormatScaleway, Ciphertext: []byte("x")}, nil
}
func (wrongKey) Unwrap(context.Context, secretkeys.Wrapped, []byte) ([]byte, error) {
	return make([]byte, 32), nil
}

// awsEndpoint serves AWS KMS Encrypt/Decrypt with the fake's cryptography.
func awsEndpoint(t *testing.T, calls *atomic.Int64, hang bool) *kms.Client {
	t.Helper()
	f := newFake(AWS, secretkeys.FormatAWS, awsKeyB, awsKeyA)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if hang {
			_, _ = io.Copy(io.Discard, r.Body) // lets the server notice the client leaving
			<-r.Context().Done()
			return
		}
		var in struct {
			KeyID             string            `json:"KeyId"`
			Plaintext         []byte            `json:"Plaintext"`
			CiphertextBlob    []byte            `json:"CiphertextBlob"`
			EncryptionContext map[string]string `json:"EncryptionContext"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		aad := []byte(in.EncryptionContext["binding"])
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		switch r.Header.Get("X-Amz-Target") {
		case "TrentService.Encrypt":
			_ = json.NewEncoder(w).Encode(map[string]any{"CiphertextBlob": f.aead(in.KeyID).Seal(nil, nil, in.Plaintext, aad)})
		case "TrentService.Decrypt":
			pt, err := f.aead(in.KeyID).Open(nil, nil, in.CiphertextBlob, aad)
			if err != nil {
				w.Header().Set("X-Amzn-Errortype", "InvalidCiphertextException")
				w.WriteHeader(400)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Plaintext": pt})
		}
	}))
	t.Cleanup(srv.Close)
	return kms.New(kms.Options{Region: "eu-west-1", BaseEndpoint: aws.String(srv.URL), RetryMaxAttempts: 1,
		Credentials: credentials.NewStaticCredentialsProvider("AKID", "secret", ""), HTTPClient: srv.Client()})
}

func Test_constructor_real_aws_client(t *testing.T) {
	var calls atomic.Int64
	client := awsEndpoint(t, &calls, false)
	p, err := New(context.Background(), awsConfig(), Clients{AWS: client})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("probe made %d requests", calls.Load())
	}
	w, err := p.Wrap(context.Background(), wctx(), scopeKey())
	if err != nil || w.KEK.Provider != AWS {
		t.Fatal(err)
	}
	if _, err := p.Unwrap(context.Background(), wctx(), w); err != nil {
		t.Fatal(err)
	}

	// A hanging endpoint fails startup within the bound.
	var hung atomic.Int64
	cfg := with(awsConfig(), func(c *Config) { c.ProbeTimeout = 100 * time.Millisecond })
	start := time.Now()
	if p, err := New(context.Background(), cfg, Clients{AWS: awsEndpoint(t, &hung, true)}); p != nil || !errors.Is(err, ErrStartupProbe) || time.Since(start) > 5*time.Second {
		t.Fatalf("hanging endpoint: %v", err)
	}
	// An unreachable endpoint fails startup.
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	deadClient := kms.New(kms.Options{Region: "eu-west-1", BaseEndpoint: aws.String(dead.URL), RetryMaxAttempts: 1,
		Credentials: credentials.NewStaticCredentialsProvider("AKID", "secret", "")})
	if p, err := New(context.Background(), awsConfig(), Clients{AWS: deadClient}); p != nil || !errors.Is(err, ErrStartupProbe) {
		t.Fatalf("unreachable endpoint: %v", err)
	}
}
