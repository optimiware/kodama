package keys_test

// The three cloud KMS providers share one local key-wrap implementation. They
// differ only in their key-reference prefix and configuration validation. A
// fake KMS client proves unwrap-once caching, rotation, fault mapping and
// cancellation without any network access.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/integration/keys"
	"github.com/optimiware/kodama/internal/shell/secret"
)

type cloudFactory struct {
	name   string
	prefix string
	new    func(keys.KMSClient, keys.CloudConfig, io.Reader) (keys.Provider, error)
}

func cloudFactories() []cloudFactory {
	return []cloudFactory{
		{name: "aws", prefix: keys.AWSKeyRefPrefix, new: keys.NewAWS},
		{name: "gcp", prefix: keys.GCPKeyRefPrefix, new: keys.NewGCP},
		{name: "scaleway", prefix: keys.ScalewayKeyRefPrefix, new: keys.NewScaleway},
	}
}

func (factory cloudFactory) build(t *testing.T, client keys.KMSClient, config keys.CloudConfig, random io.Reader) keys.Provider {
	t.Helper()
	provider, err := factory.new(client, config, random)
	return mustProvider(t, provider, err)
}

// cloudReference returns a provider-valid reference for the factory family.
// Construction validates the full AWS/GCP grammar, so a generic
// "<prefix>alias/kodama" is not a valid reference for every provider.
func cloudReference(factory cloudFactory, name string) secret.KeyRef {
	switch factory.name {
	case "aws":
		return secret.KeyRef(factory.prefix + "alias/" + name)
	case "gcp":
		return secret.KeyRef("gcp-kms:projects/kodama/locations/global/keyRings/kodama/cryptoKeys/" + name)
	default:
		return secret.KeyRef(factory.prefix + name)
	}
}

func cloudBlob(name string, version secret.KeyVersion) []byte {
	return []byte(fmt.Sprintf("%s-encrypted-kek-v%d", name, version))
}

func cloudConfig(ref secret.KeyRef, current secret.KeyVersion, blobs map[secret.KeyVersion][]byte, retained ...secret.KeyVersion) keys.CloudConfig {
	return keys.CloudConfig{
		KeyRef:   ref,
		Current:  current,
		Versions: blobs,
		Retained: retained,
	}
}

func TestCloudProviderRoundTrip(t *testing.T) {
	for _, factory := range cloudFactories() {
		t.Run(factory.name, func(t *testing.T) {
			ref := cloudReference(factory, "kodama")
			blob := cloudBlob(factory.name, 1)
			client := newFakeKMS(map[string][]byte{string(blob): testKEK()})
			provider := factory.build(t, client, cloudConfig(ref, 1, map[secret.KeyVersion][]byte{1: blob}), deterministicRandom(0x11))
			if provider.KeyRef() != ref {
				t.Fatalf("key reference = %q; want %q", provider.KeyRef(), ref)
			}

			associatedData := testAssociatedBytes(t, testAssociated(1))
			wrapped, err := provider.Wrap(context.Background(), secret.WrapRequest{
				KeyRef:         ref,
				KeyVersion:     1,
				Plaintext:      testDEK(),
				AssociatedData: associatedData,
			})
			if err != nil {
				t.Fatalf("Wrap: %v", err)
			}
			if err := wrapped.Validate(); err != nil {
				t.Fatalf("wrapped key refused: %v", err)
			}
			plaintext, err := provider.Unwrap(context.Background(), secret.UnwrapRequest{
				KeyRef:         ref,
				KeyVersion:     1,
				Wrapped:        wrapped.Wrapped,
				Nonce:          wrapped.Nonce,
				AssociatedData: associatedData,
			})
			if err != nil {
				t.Fatalf("Unwrap: %v", err)
			}
			if !bytes.Equal(plaintext, testDEK()) {
				t.Fatal("unwrapped data key does not match the wrapped data key")
			}

			cipher := newCipher(t, provider, deterministicRandom(0x42))
			envelope, err := cipher.Seal(context.Background(), testAssociated(1), []byte("cloud-value"))
			if err != nil {
				t.Fatalf("Seal: %v", err)
			}
			opened, err := cipher.Open(context.Background(), testAssociated(1), envelope)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if string(opened) != "cloud-value" {
				t.Fatalf("opened plaintext = %q; want %q", opened, "cloud-value")
			}
			if calls := client.calls(blob); calls != 1 {
				t.Fatalf("KMS calls for one version = %d; want 1", calls)
			}
		})
	}
}

func TestCloudProviderConfigurationRefusals(t *testing.T) {
	for _, factory := range cloudFactories() {
		t.Run(factory.name, func(t *testing.T) {
			ref := cloudReference(factory, "kodama")
			foreignPrefix := keys.GCPKeyRefPrefix
			if factory.prefix == keys.GCPKeyRefPrefix {
				foreignPrefix = keys.AWSKeyRefPrefix
			}
			blobOne := cloudBlob(factory.name, 1)
			blobTwo := cloudBlob(factory.name, 2)
			client := newFakeKMS(map[string][]byte{
				string(blobOne): testKEK(),
				string(blobTwo): testKEK(),
			})
			oneVersion := func() map[secret.KeyVersion][]byte {
				return map[secret.KeyVersion][]byte{1: blobOne}
			}

			cases := []struct {
				name   string
				client keys.KMSClient
				config keys.CloudConfig
			}{
				{name: "empty key reference", client: client, config: cloudConfig("", 1, oneVersion())},
				{name: "foreign prefix", client: client, config: cloudConfig(secret.KeyRef(foreignPrefix+"alias/kodama"), 1, oneVersion())},
				{name: "missing prefix", client: client, config: cloudConfig("alias/kodama", 1, oneVersion())},
				{name: "zero current version", client: client, config: cloudConfig(ref, 0, oneVersion())},
				{name: "empty versions", client: client, config: cloudConfig(ref, 1, nil)},
				{name: "missing current blob", client: client, config: cloudConfig(ref, 2, oneVersion())},
				{name: "retained current version", client: client, config: cloudConfig(ref, 1, oneVersion(), 1)},
				{name: "retained unknown version", client: client, config: cloudConfig(ref, 1, oneVersion(), 2)},
				{name: "unlisted configured version", client: client, config: cloudConfig(ref, 1, map[secret.KeyVersion][]byte{1: blobOne, 2: blobTwo})},
				{name: "nil client", client: nil, config: cloudConfig(ref, 1, oneVersion())},
			}

			for _, testCase := range cases {
				t.Run(testCase.name, func(t *testing.T) {
					provider, err := factory.new(testCase.client, testCase.config, deterministicRandom(0x11))
					requireFaultClass(t, err, fault.InvalidInput)
					if provider != nil {
						t.Fatal("refused configuration returned a provider")
					}
				})
			}
			if calls := client.calls(blobOne); calls != 0 {
				t.Fatalf("invalid configuration contacted the KMS %d times; want 0", calls)
			}
		})
	}
}

// TestCloudProviderUnwrapsEachVersionOnce proves the KEK cache is per version
// and race-safe: construction performs no KMS call, concurrent first uses
// unwrap once, and repeated opens of a retained version reuse the cached key.
func TestCloudProviderUnwrapsEachVersionOnce(t *testing.T) {
	for _, factory := range cloudFactories() {
		t.Run(factory.name, func(t *testing.T) {
			ref := cloudReference(factory, "kodama")
			kekOne := bytes.Repeat([]byte{0x11}, secret.DataKeyBytes)
			kekTwo := bytes.Repeat([]byte{0x22}, secret.DataKeyBytes)
			blobOne := cloudBlob(factory.name, 1)
			blobTwo := cloudBlob(factory.name, 2)

			client := newFakeKMS(map[string][]byte{
				string(blobOne): kekOne,
				string(blobTwo): kekTwo,
			})
			rotated := factory.build(t, client, cloudConfig(ref, 2, map[secret.KeyVersion][]byte{1: blobOne, 2: blobTwo}, 1), nil)
			for _, blob := range [][]byte{blobOne, blobTwo} {
				if calls := client.calls(blob); calls != 0 {
					t.Fatalf("construction unwrapped a key version %d times; want 0", calls)
				}
			}

			associatedData := testAssociatedBytes(t, testAssociated(2))
			const workers = 32
			failures := make(chan error, workers)
			var wait sync.WaitGroup
			wait.Add(workers)
			for range workers {
				go func() {
					defer wait.Done()
					_, err := rotated.Wrap(context.Background(), secret.WrapRequest{
						KeyRef:         ref,
						KeyVersion:     2,
						Plaintext:      testDEK(),
						AssociatedData: associatedData,
					})
					failures <- err
				}()
			}
			wait.Wait()
			close(failures)
			for err := range failures {
				if err != nil {
					t.Fatalf("concurrent Wrap: %v", err)
				}
			}
			if calls := client.calls(blobTwo); calls != 1 {
				t.Fatalf("concurrent first use unwrapped the current version %d times; want 1", calls)
			}

			oneClient := newFakeKMS(map[string][]byte{string(blobOne): kekOne})
			one := factory.build(t, oneClient, cloudConfig(ref, 1, map[secret.KeyVersion][]byte{1: blobOne}), nil)
			oldAssociated := testAssociatedBytes(t, testAssociated(1))
			wrapped, err := one.Wrap(context.Background(), secret.WrapRequest{
				KeyRef:         ref,
				KeyVersion:     1,
				Plaintext:      testDEK(),
				AssociatedData: oldAssociated,
			})
			if err != nil {
				t.Fatalf("Wrap retained version: %v", err)
			}
			for attempt := range 5 {
				plaintext, err := rotated.Unwrap(context.Background(), secret.UnwrapRequest{
					KeyRef:         ref,
					KeyVersion:     1,
					Wrapped:        wrapped.Wrapped,
					Nonce:          wrapped.Nonce,
					AssociatedData: oldAssociated,
				})
				if err != nil {
					t.Fatalf("Unwrap retained version attempt %d: %v", attempt, err)
				}
				if !bytes.Equal(plaintext, testDEK()) {
					t.Fatalf("retained version returned a different data key")
				}
			}
			if calls := client.calls(blobOne); calls != 1 {
				t.Fatalf("repeated retained unwraps called the KMS %d times; want 1", calls)
			}
		})
	}
}

func TestCloudProviderRotation(t *testing.T) {
	for _, factory := range cloudFactories() {
		t.Run(factory.name, func(t *testing.T) {
			ref := cloudReference(factory, "kodama")
			kekOne := bytes.Repeat([]byte{0x11}, secret.DataKeyBytes)
			kekTwo := bytes.Repeat([]byte{0x22}, secret.DataKeyBytes)
			blobOne := cloudBlob(factory.name, 1)
			blobTwo := cloudBlob(factory.name, 2)

			one := factory.build(t, newFakeKMS(map[string][]byte{string(blobOne): kekOne}), cloudConfig(ref, 1, map[secret.KeyVersion][]byte{1: blobOne}), nil)
			oneCipher := newCipher(t, one, deterministicRandom(0x42))
			oldEnvelope, err := oneCipher.Seal(context.Background(), testAssociated(1), []byte("old-value"))
			if err != nil {
				t.Fatalf("Seal old version: %v", err)
			}

			rotated := factory.build(t, newFakeKMS(map[string][]byte{string(blobOne): kekOne, string(blobTwo): kekTwo}), cloudConfig(ref, 2, map[secret.KeyVersion][]byte{1: blobOne, 2: blobTwo}, 1), nil)
			rotatedCipher := newCipher(t, rotated, deterministicRandom(0x43))

			opened, err := rotatedCipher.Open(context.Background(), testAssociated(1), oldEnvelope)
			if err != nil {
				t.Fatalf("retained version refused after rotation: %v", err)
			}
			if string(opened) != "old-value" {
				t.Fatalf("opened old value = %q; want %q", opened, "old-value")
			}

			newEnvelope, err := rotatedCipher.Seal(context.Background(), testAssociated(2), []byte("new-value"))
			if err != nil {
				t.Fatalf("Seal new version: %v", err)
			}
			if newEnvelope.KeyVersion != 2 {
				t.Fatalf("new envelope version = %d; want 2", newEnvelope.KeyVersion)
			}

			if _, err := oneCipher.Open(context.Background(), testAssociated(2), newEnvelope); err == nil {
				t.Fatal("old provider opened a new-version envelope")
			} else {
				requireFaultClass(t, err, fault.NotFound)
			}

			strict := factory.build(t, newFakeKMS(map[string][]byte{string(blobTwo): kekTwo}), cloudConfig(ref, 2, map[secret.KeyVersion][]byte{2: blobTwo}), nil)
			strictCipher := newCipher(t, strict, deterministicRandom(0x44))
			if _, err := strictCipher.Open(context.Background(), testAssociated(1), oldEnvelope); err == nil {
				t.Fatal("unconfigured old version was decrypted")
			} else {
				requireFaultClass(t, err, fault.NotFound)
			}
		})
	}
}

func TestCloudProviderUnknownVersionOrReference(t *testing.T) {
	for _, factory := range cloudFactories() {
		t.Run(factory.name, func(t *testing.T) {
			ref := cloudReference(factory, "kodama")
			blob := cloudBlob(factory.name, 1)
			client := newFakeKMS(map[string][]byte{string(blob): testKEK()})
			provider := factory.build(t, client, cloudConfig(ref, 1, map[secret.KeyVersion][]byte{1: blob}), nil)

			associatedData := testAssociatedBytes(t, testAssociated(1))
			if _, err := provider.Wrap(context.Background(), secret.WrapRequest{KeyRef: ref, KeyVersion: 9, Plaintext: testDEK(), AssociatedData: associatedData}); err == nil {
				t.Fatal("unknown key version wrapped")
			} else {
				requireFaultClass(t, err, fault.NotFound)
			}
			if calls := client.calls(blob); calls != 0 {
				t.Fatalf("unknown version contacted the KMS %d times; want 0", calls)
			}

			if _, err := provider.Wrap(context.Background(), secret.WrapRequest{KeyRef: cloudReference(factory, "other"), KeyVersion: 1, Plaintext: testDEK(), AssociatedData: associatedData}); err == nil {
				t.Fatal("unknown key reference wrapped")
			} else {
				requireFaultClass(t, err, fault.NotFound)
			}
			if calls := client.calls(blob); calls != 0 {
				t.Fatalf("unknown reference contacted the KMS %d times; want 0", calls)
			}

			wrapped, err := provider.Wrap(context.Background(), secret.WrapRequest{KeyRef: ref, KeyVersion: 1, Plaintext: testDEK(), AssociatedData: associatedData})
			if err != nil {
				t.Fatalf("valid Wrap: %v", err)
			}
			if _, err := provider.Unwrap(context.Background(), secret.UnwrapRequest{KeyRef: cloudReference(factory, "other"), KeyVersion: 1, Wrapped: wrapped.Wrapped, Nonce: wrapped.Nonce, AssociatedData: associatedData}); err == nil {
				t.Fatal("unknown key reference unwrapped")
			} else {
				requireFaultClass(t, err, fault.NotFound)
			}
			if calls := client.calls(blob); calls != 1 {
				t.Fatalf("KMS calls = %d; want the single valid wrap", calls)
			}
		})
	}
}

func TestCloudProviderFaultMapping(t *testing.T) {
	for _, factory := range cloudFactories() {
		t.Run(factory.name, func(t *testing.T) {
			ref := cloudReference(factory, "kodama")
			blob := cloudBlob(factory.name, 1)
			associatedData := testAssociatedBytes(t, testAssociated(1))

			failures := []struct {
				name    string
				failure error
				want    fault.Class
			}{
				{name: "denied", failure: fault.New(fault.Forbidden, "kms denied the decrypt"), want: fault.Forbidden},
				{name: "unauthenticated", failure: fault.New(fault.Unauthenticated, "kms rejected the credentials"), want: fault.Unauthenticated},
				{name: "outage", failure: fault.New(fault.DependencyUnavailable, "kms unavailable"), want: fault.DependencyUnavailable},
				{name: "unclassified transport", failure: errors.New("connection reset by peer"), want: fault.DependencyUnavailable},
			}
			for _, testCase := range failures {
				t.Run(testCase.name, func(t *testing.T) {
					client := newFakeKMS(map[string][]byte{string(blob): testKEK()})
					client.fail(blob, testCase.failure)
					provider := factory.build(t, client, cloudConfig(ref, 1, map[secret.KeyVersion][]byte{1: blob}), deterministicRandom(0x11))
					_, err := provider.Wrap(context.Background(), secret.WrapRequest{KeyRef: ref, KeyVersion: 1, Plaintext: testDEK(), AssociatedData: associatedData})
					requireFaultClass(t, err, testCase.want)
					requireNoMaterial(t, err, testKEK(), testDEK())
				})
			}

			malformed := [][]byte{
				bytes.Repeat([]byte{0x33}, secret.DataKeyBytes-1),
				bytes.Repeat([]byte{0x33}, secret.DataKeyBytes+1),
				nil,
			}
			for index, material := range malformed {
				t.Run(fmt.Sprintf("malformed kek length %d", index), func(t *testing.T) {
					client := newFakeKMS(map[string][]byte{string(blob): material})
					provider := factory.build(t, client, cloudConfig(ref, 1, map[secret.KeyVersion][]byte{1: blob}), deterministicRandom(0x11))
					_, err := provider.Wrap(context.Background(), secret.WrapRequest{KeyRef: ref, KeyVersion: 1, Plaintext: testDEK(), AssociatedData: associatedData})
					requireFaultClass(t, err, fault.InvalidInput)
					requireNoMaterial(t, err, testDEK())
				})
			}

			t.Run("wrong kek material", func(t *testing.T) {
				wrappingClient := newFakeKMS(map[string][]byte{string(blob): bytes.Repeat([]byte{0x11}, secret.DataKeyBytes)})
				wrapping := factory.build(t, wrappingClient, cloudConfig(ref, 1, map[secret.KeyVersion][]byte{1: blob}), deterministicRandom(0x11))
				wrapped, err := wrapping.Wrap(context.Background(), secret.WrapRequest{KeyRef: ref, KeyVersion: 1, Plaintext: testDEK(), AssociatedData: associatedData})
				if err != nil {
					t.Fatalf("Wrap: %v", err)
				}

				openingClient := newFakeKMS(map[string][]byte{string(blob): bytes.Repeat([]byte{0x22}, secret.DataKeyBytes)})
				opening := factory.build(t, openingClient, cloudConfig(ref, 1, map[secret.KeyVersion][]byte{1: blob}), deterministicRandom(0x11))
				plaintext, err := opening.Unwrap(context.Background(), secret.UnwrapRequest{KeyRef: ref, KeyVersion: 1, Wrapped: wrapped.Wrapped, Nonce: wrapped.Nonce, AssociatedData: associatedData})
				requireFaultClass(t, err, fault.InvalidInput)
				if len(plaintext) != 0 {
					t.Fatalf("wrong KEK returned %d plaintext bytes", len(plaintext))
				}
				requireNoMaterial(t, err, testDEK())
			})
		})
	}
}

func TestCloudProviderMalformedKEKIsNotCached(t *testing.T) {
	for _, factory := range cloudFactories() {
		t.Run(factory.name, func(t *testing.T) {
			ref := cloudReference(factory, "kodama")
			blob := cloudBlob(factory.name, 1)
			var calls atomic.Int32
			client := &functionKMS{decrypt: func(_ context.Context, _ secret.KeyRef, _ []byte) ([]byte, error) {
				if calls.Add(1) == 1 {
					return bytes.Repeat([]byte{0x33}, secret.DataKeyBytes-1), nil
				}
				return testKEK(), nil
			}}
			provider := factory.build(t, client, cloudConfig(ref, 1, map[secret.KeyVersion][]byte{1: blob}), deterministicRandom(0x11))
			associatedData := testAssociatedBytes(t, testAssociated(1))
			request := secret.WrapRequest{KeyRef: ref, KeyVersion: 1, Plaintext: testDEK(), AssociatedData: associatedData}

			if _, err := provider.Wrap(context.Background(), request); err == nil {
				t.Fatal("malformed KEK was accepted")
			} else {
				requireFaultClass(t, err, fault.InvalidInput)
			}
			if _, err := provider.Wrap(context.Background(), request); err != nil {
				t.Fatalf("second Wrap after malformed KEK: %v", err)
			}
			if got := calls.Load(); got != 2 {
				t.Fatalf("KMS calls = %d; want 2 (the malformed result must not be cached)", got)
			}
		})
	}
}

func TestCloudProviderCancellation(t *testing.T) {
	for _, factory := range cloudFactories() {
		t.Run(factory.name, func(t *testing.T) {
			ref := cloudReference(factory, "kodama")
			blob := cloudBlob(factory.name, 1)
			associatedData := testAssociatedBytes(t, testAssociated(1))
			request := secret.WrapRequest{KeyRef: ref, KeyVersion: 1, Plaintext: testDEK(), AssociatedData: associatedData}

			t.Run("during unwrap", func(t *testing.T) {
				client := newFakeKMS(map[string][]byte{string(blob): testKEK()})
				provider := factory.build(t, client, cloudConfig(ref, 1, map[secret.KeyVersion][]byte{1: blob}), deterministicRandom(0x11))
				if calls := client.calls(blob); calls != 0 {
					t.Fatalf("construction contacted the KMS %d times; want 0", calls)
				}

				entered := make(chan struct{})
				var attempts atomic.Int32
				client.before(func(ctx context.Context, _ secret.KeyRef, _ []byte) error {
					if attempts.Add(1) == 1 {
						close(entered)
						<-ctx.Done()
						return ctx.Err()
					}
					return nil
				})

				ctx, cancel := context.WithCancel(context.Background())
				result := make(chan error, 1)
				go func() {
					_, err := provider.Wrap(ctx, request)
					result <- err
				}()
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("provider did not ask the KMS to unwrap")
				}
				cancel()
				var err error
				select {
				case err = <-result:
				case <-time.After(5 * time.Second):
					t.Fatal("cancelled wrap did not return")
				}
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled unwrap error = %v; want context.Canceled", err)
				}

				if _, err := provider.Wrap(context.Background(), request); err != nil {
					t.Fatalf("Wrap after cancellation: %v", err)
				}
				if got := client.calls(blob); got != 2 {
					t.Fatalf("KMS calls = %d; want 2 (a cancelled unwrap must not be cached)", got)
				}
			})

			t.Run("before unwrap", func(t *testing.T) {
				client := newFakeKMS(map[string][]byte{string(blob): testKEK()})
				provider := factory.build(t, client, cloudConfig(ref, 1, map[secret.KeyVersion][]byte{1: blob}), deterministicRandom(0x11))

				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				if _, err := provider.Wrap(ctx, request); !errors.Is(err, context.Canceled) {
					t.Fatalf("pre-cancelled wrap error = %v; want context.Canceled", err)
				}
				if _, err := provider.Wrap(context.Background(), request); err != nil {
					t.Fatalf("Wrap after pre-cancelled attempt: %v", err)
				}
			})
		})
	}
}
