// Package keys_test pins the secret-custody adapter API.
//
// Package kodama/internal/shell/integration/keys provides:
//
//	// Provider is one key provider plus the key reference it serves.
//	type Provider interface {
//		secret.KeyProvider
//		KeyRef() secret.KeyRef
//	}
//
//	// NewCipher returns an envelope cipher over one key provider. A nil
//	// random means crypto/rand.
//	func NewCipher(provider Provider, random io.Reader) (*Cipher, error)
//
//	// Seal encrypts plaintext under a fresh 32-byte data key and wraps that
//	// data key with the provider. The returned envelope carries no plaintext.
//	func (cipher *Cipher) Seal(ctx context.Context, associated secret.AssociatedData, plaintext []byte) (secret.Envelope, error)
//
//	// Open authenticates the envelope against associated and returns the
//	// plaintext. Every refusal returns no plaintext.
//	func (cipher *Cipher) Open(ctx context.Context, associated secret.AssociatedData, envelope secret.Envelope) ([]byte, error)
//
//	// NewEphemeral keeps one random 32-byte key in memory for this process.
//	func NewEphemeral(random io.Reader) (Provider, error)
//
//	// NewKeyfile reads one 32-byte key from a regular, non-symlink file that
//	// is not group or world accessible.
//	func NewKeyfile(keyRef secret.KeyRef, path string, random io.Reader) (Provider, error)
//
//	// KMSClient unwraps one installation-configured encrypted key blob.
//	type KMSClient interface {
//		Decrypt(ctx context.Context, keyRef secret.KeyRef, blob []byte) ([]byte, error)
//	}
//
//	// CloudConfig configures one KMS-backed provider. Versions holds the
//	// opaque encrypted key blob for the current version and for every
//	// explicitly retained older version; no other version is configured.
//	type CloudConfig struct {
//		KeyRef   secret.KeyRef
//		Current  secret.KeyVersion
//		Versions map[secret.KeyVersion][]byte
//		Retained []secret.KeyVersion
//	}
//
//	func NewAWS(client KMSClient, config CloudConfig, random io.Reader) (Provider, error)
//	func NewGCP(client KMSClient, config CloudConfig, random io.Reader) (Provider, error)
//	func NewScaleway(client KMSClient, config CloudConfig, random io.Reader) (Provider, error)
//
//	const (
//		AWSKeyRefPrefix      = "aws-kms:"
//		GCPKeyRefPrefix      = "gcp-kms:"
//		ScalewayKeyRefPrefix = "scw-kms:"
//	)
//
// Behavior these tests rely on:
//   - Suite is AES-256-GCM. A data key and a key-encryption key are 32 bytes;
//     the content nonce is 12 bytes, the wrap nonce is 12 bytes, and the
//     wrapped key is 32 ciphertext bytes plus the 16-byte tag (48 bytes).
//   - The content AAD and the wrap AAD are both the exact
//     secret.AssociatedData.Bytes() encoding.
//   - content = AES-256-GCM(DEK, nonce, plaintext, AAD) and
//     wrapped = AES-256-GCM(KEK, wrapNonce, DEK, AAD).
//   - A nil io.Reader means crypto/rand; tests pass a deterministic reader.
//     The cipher draws the data key and content nonce from its reader; the
//     provider draws the wrap nonce from its own reader.
//   - Providers never call KMS at construction; a key version is unwrapped on
//     first use and cached for the provider's lifetime, once per version.
//   - NewEphemeral and NewKeyfile serve exactly key version 1; any other
//     version is an unknown key version.
//   - Unknown key reference or key version is fault.NotFound. Authentication
//     failure, malformed material and configuration shape are
//     fault.InvalidInput. A KMS denial keeps fault.Forbidden or
//     fault.Unauthenticated; an unclassified or transport KMS failure is
//     fault.DependencyUnavailable. Context cancellation returns the context
//     error.
//   - Errors never contain a KEK, a DEK or secret plaintext.
package keys_test
