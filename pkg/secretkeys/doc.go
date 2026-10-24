// Package secretkeys wraps a 32-byte scope key. It does not encrypt secret values.
//
// Providers are ephemeral memory, a local keyfile, or one cloud KMS (Scaleway,
// AWS, or GCP). A missing or failed provider is an error. Nothing falls back
// to another provider.
package secretkeys
