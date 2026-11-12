package keys

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"sync"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/secret"
)

// Cloud key-reference prefixes. Each provider family serves exactly one prefix;
// a configuration naming another family's reference is refused rather than
// silently treated as a different key.
const (
	AWSKeyRefPrefix      = "aws-kms:"
	GCPKeyRefPrefix      = "gcp-kms:"
	ScalewayKeyRefPrefix = "scw-kms:"
)

// KMSClient unwraps one installation-configured encrypted key blob. It is the
// only cloud dependency; the provider makes one call per configured key version
// and caches the result for its lifetime.
type KMSClient interface {
	Decrypt(ctx context.Context, keyRef secret.KeyRef, blob []byte) ([]byte, error)
}

// CloudConfig configures one KMS-backed provider. Versions holds the opaque
// encrypted key blob for the current version and for every explicitly retained
// older version; no other version is configured.
type CloudConfig struct {
	KeyRef   secret.KeyRef
	Current  secret.KeyVersion
	Versions map[secret.KeyVersion][]byte
	Retained []secret.KeyVersion
}

// NewAWS returns an AWS KMS-backed provider. Construction validates the
// configuration shape only; it never contacts the KMS.
func NewAWS(client KMSClient, config CloudConfig, random io.Reader) (Provider, error) {
	return newCloudProvider(AWSKeyRefPrefix, client, config, random)
}

// NewGCP returns a GCP KMS-backed provider. Construction validates the
// configuration shape only; it never contacts the KMS.
func NewGCP(client KMSClient, config CloudConfig, random io.Reader) (Provider, error) {
	return newCloudProvider(GCPKeyRefPrefix, client, config, random)
}

// NewScaleway returns a Scaleway KMS-backed provider. Construction validates
// the configuration shape only; it never contacts the KMS.
func NewScaleway(client KMSClient, config CloudConfig, random io.Reader) (Provider, error) {
	return newCloudProvider(ScalewayKeyRefPrefix, client, config, random)
}

// newCloudProvider builds the shared KMS-backed provider. The key-encryption
// key for a version is unwrapped on first use and cached; malformed material,
// a cancelled context and every other failure are never cached.
func newCloudProvider(prefix string, client KMSClient, config CloudConfig, random io.Reader) (Provider, error) {
	if err := validateCloudConfig(prefix, client, config); err != nil {
		return nil, err
	}
	if random == nil {
		random = rand.Reader
	}
	blobs := make(map[secret.KeyVersion][]byte, len(config.Versions))
	for version, blob := range config.Versions {
		blobs[version] = append([]byte(nil), blob...)
	}

	provider := &keyProvider{keyRef: config.KeyRef, random: random}

	var cacheMu sync.Mutex
	cache := make(map[secret.KeyVersion][]byte, len(blobs))
	provider.kekFor = func(ctx context.Context, version secret.KeyVersion) ([]byte, error) {
		blob, configured := blobs[version]
		if !configured {
			return nil, unknownKeyVersion()
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		cacheMu.Lock()
		defer cacheMu.Unlock()
		if material, cached := cache[version]; cached {
			return material, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		material, err := client.Decrypt(ctx, config.KeyRef, blob)
		if err != nil {
			return nil, mapKMSError(err)
		}
		if len(material) != secret.DataKeyBytes {
			clear(material)
			return nil, fault.New(fault.InvalidInput, "key management service returned a key of the wrong length")
		}
		cached := append([]byte(nil), material...)
		clear(material)
		cache[version] = cached
		return cached, nil
	}
	return provider, nil
}

// validateCloudConfig refuses a configuration that is unusable before any
// request or KMS call: a nil client, a foreign or empty key reference, a
// non-positive current version, a missing current blob, a retained version
// that is not configured, and any configured version that is neither current
// nor explicitly retained.
func validateCloudConfig(prefix string, client KMSClient, config CloudConfig) error {
	if client == nil {
		return fault.New(fault.InvalidInput, "key management service client is nil")
	}
	if config.KeyRef == "" {
		return fault.New(fault.InvalidInput, "cloud key reference is empty")
	}
	if !keyRefPrefix(config.KeyRef, prefix) {
		return fault.New(fault.InvalidInput, "cloud key reference does not match the provider family")
	}
	if err := validateCloudKeyReference(prefix, config.KeyRef); err != nil {
		return err
	}
	if config.Current < secret.MinKeyVersion {
		return fault.New(fault.InvalidInput, "cloud key current version is not positive")
	}
	if len(config.Versions) == 0 {
		return fault.New(fault.InvalidInput, "cloud key configuration has no versions")
	}
	if _, configured := config.Versions[config.Current]; !configured {
		return fault.New(fault.InvalidInput, "cloud key configuration has no blob for the current version")
	}
	allowed := map[secret.KeyVersion]bool{config.Current: true}
	for _, version := range config.Retained {
		if version == config.Current {
			return fault.New(fault.InvalidInput, "cloud key retained versions include the current version")
		}
		if allowed[version] {
			return fault.New(fault.InvalidInput, "cloud key retained versions repeat a version")
		}
		if _, configured := config.Versions[version]; !configured {
			return fault.New(fault.InvalidInput, "cloud key retained version has no configured blob")
		}
		allowed[version] = true
	}
	if len(allowed) != len(config.Versions) {
		return fault.New(fault.InvalidInput, "cloud key configuration lists a version that is neither current nor retained")
	}
	for _, blob := range config.Versions {
		if len(blob) == 0 {
			return fault.New(fault.InvalidInput, "cloud key configuration has an empty encrypted key blob")
		}
	}
	return nil
}

// mapKMSError converts a KMS client failure into a stable fault class without
// rendering the client's message or any key material. Context cancellation and
// deadline expiry stay reachable through errors.Is; a denial or an already
// classified dependency failure keeps its class; every unclassified or
// transport failure is DependencyUnavailable.
func mapKMSError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if class, classified := fault.ClassOf(err); classified {
		switch class {
		case fault.Forbidden, fault.Unauthenticated, fault.DependencyUnavailable, fault.NotFound, fault.DeadlineExpired:
			return err
		}
	}
	return fault.Wrap(fault.DependencyUnavailable, "key management service decrypt failed", err)
}
