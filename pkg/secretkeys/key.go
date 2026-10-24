package secretkeys

import "context"

// Provider names and wrap formats recorded in Wrapped by the cloud providers.
// Adapters compare against them so a wrap of another cloud is never accepted.
const (
	ProviderAWS      = "aws"
	ProviderGCP      = "gcp"
	ProviderScaleway = "scaleway"

	FormatAWS      = awsFormat
	FormatGCP      = gcpFormat
	FormatScaleway = scalewayFormat

	// ScalewayRefPrefix is the region-qualified prefix of Scaleway key
	// references; the provider supports fr-par only.
	ScalewayRefPrefix = "fr-par/"
)

// Wrapped is a scope key encrypted by one KEK. It never contains key material.
type Wrapped struct {
	Provider   string
	KeyRef     string
	Format     string
	Ciphertext []byte
}

// Provider wraps and unwraps one 32-byte scope key. binding is authenticated
// data; a different binding must not unwrap.
type Provider interface {
	Wrap(ctx context.Context, key, binding []byte) (Wrapped, error)
	Unwrap(ctx context.Context, w Wrapped, binding []byte) ([]byte, error)
}

// Ephemeral reports whether restart loses the key. Durable stores must reject it.
type Ephemeral interface {
	Ephemeral() bool
}
