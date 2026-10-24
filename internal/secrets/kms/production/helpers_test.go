package production

import (
	gcpkms "cloud.google.com/go/kms/apiv1"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"errors"
	"google.golang.org/api/option"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/pkg/secretkeys"
)

const (
	awsKeyA = "11111111-1111-4111-8111-111111111111"
	awsKeyB = "22222222-2222-4222-8222-222222222222"
	gcpKeyA = "projects/p/locations/europe-west1/keyRings/r/cryptoKeys/a"
	gcpKeyB = "projects/p/locations/europe-west1/keyRings/r/cryptoKeys/b"
	scwKeyA = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	scwKeyB = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
)

// fakeInner is an in-memory secretkeys.Provider with the contract of the cloud
// providers: it authenticates the binding and key, and refuses unconfigured
// keys, foreign providers and formats before "contacting" the KMS.
type fakeInner struct {
	cloud, format, active string
	allowed               map[string]bool

	mu      sync.Mutex
	err     error
	calls   atomic.Int64
	last    []byte // last binding seen
	forceKR string // overrides the key reference returned by Wrap
	block   chan struct{}
}

func newFake(cloud, format, active string, retained ...string) *fakeInner {
	f := &fakeInner{cloud: cloud, format: format, active: active, allowed: map[string]bool{active: true}}
	for _, r := range retained {
		f.allowed[r] = true
	}
	return f
}

func (f *fakeInner) aead(keyRef string) cipher.AEAD {
	k := sha256.Sum256([]byte("fake-kek|" + keyRef))
	b, _ := aes.NewCipher(k[:])
	a, _ := cipher.NewGCMWithRandomNonce(b)
	return a
}

func (f *fakeInner) enter(ctx context.Context, binding []byte) error {
	f.calls.Add(1)
	f.mu.Lock()
	f.last = append([]byte(nil), binding...)
	err, block := f.err, f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return secretkeys.ErrUnavailable
		}
	}
	return err
}

func (f *fakeInner) Wrap(ctx context.Context, key, binding []byte) (secretkeys.Wrapped, error) {
	if err := f.enter(ctx, binding); err != nil {
		return secretkeys.Wrapped{}, err
	}
	if len(key) != 32 {
		return secretkeys.Wrapped{}, secretkeys.ErrInvalid
	}
	kr := f.active
	if f.forceKR != "" {
		kr = f.forceKR
	}
	return secretkeys.Wrapped{Provider: f.cloud, KeyRef: kr, Format: f.format, Ciphertext: f.aead(f.active).Seal(nil, nil, key, binding)}, nil
}

func (f *fakeInner) Unwrap(ctx context.Context, w secretkeys.Wrapped, binding []byte) ([]byte, error) {
	if w.Provider != f.cloud || w.Format != f.format || !f.allowed[w.KeyRef] {
		return nil, secretkeys.ErrCiphertext
	}
	if err := f.enter(ctx, binding); err != nil {
		return nil, err
	}
	key, err := f.aead(w.KeyRef).Open(nil, nil, w.Ciphertext, binding)
	if err != nil {
		return nil, secretkeys.ErrCiphertext
	}
	return key, nil
}

func (f *fakeInner) fail(err error) { f.mu.Lock(); f.err = err; f.mu.Unlock() }

func wctx() secrets.WrapContext {
	return secrets.WrapContext{InstallationID: "inst-1", TenantID: "t_acme", Generation: 3, KeyID: "sk_abc"}
}

func scopeKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i + 1)
	}
	return k
}

func awsConfig() Config {
	return Config{Provider: AWS, KEKID: "kodama-prod", Current: "v2", Resources: map[string]string{"v1": awsKeyA, "v2": awsKeyB}}
}

// bridgeWith builds a Provider over fake without the startup probe.
func bridgeWith(t *testing.T, cfg Config, f *fakeInner) *Provider {
	t.Helper()
	p, err := cfg.validate()
	if err != nil {
		t.Fatal(err)
	}
	return newBridge(p, f)
}

func keyClass(t *testing.T, err error) secrets.KeyErrorClass {
	t.Helper()
	var ke *secrets.KeyError
	if !errors.As(err, &ke) {
		t.Fatalf("not a classified key error: %v", err)
	}
	return ke.Class
}

func gcpClient(t *testing.T) *gcpkms.KeyManagementClient {
	t.Helper()
	c, err := gcpkms.NewKeyManagementRESTClient(context.Background(), option.WithEndpoint("http://127.0.0.1:1"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
