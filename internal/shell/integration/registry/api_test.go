// Package registry_test pins the registry-resolver API and the behavior these
// tests require.
//
// # Exported API
//
//	func New(config Config) (*Resolver, error)
//	func (config Config) Validate() error
//	func (resolver *Resolver) Resolve(ctx context.Context, request pipeline.ResolveRequest) (pipeline.ResolvedImage, error)
//	func DefaultAllowedMediaTypes() []string
//
//	type Config struct {
//		Transport         http.RoundTripper
//		Credentials       CredentialSource
//		AllowedMediaTypes []string
//		MaxResponseBytes  int64
//		MaxRedirects      int
//		AllowInsecureHTTP bool
//	}
//
//	const DefaultMaxResponseBytes int64 = 1 << 20
//	const DefaultMaxRedirects = 3
//
//	type CredentialSource interface {
//		Credential(ctx context.Context, origin string) (Credential, error)
//	}
//
//	type Credential struct {
//		Scheme   CredentialScheme
//		Username string
//		Password string
//		Token    string
//	}
//
//	type CredentialScheme string
//
//	const (
//		CredentialAnonymous CredentialScheme = "anonymous"
//		CredentialBasic     CredentialScheme = "basic"
//		CredentialBearer    CredentialScheme = "bearer"
//	)
//
// # Behavior
//
// Resolution is a pre-admission read. No cache exists: every call performs a
// network lookup through Config.Transport, so a changed tag resolves to its new
// digest and a failure after an earlier success is an error, never the earlier
// digest. A Resolver is safe for concurrent use.
//
// Input is one of:
//
//   - a tag reference: host[:port]/path:tag;
//   - an already pinned reference: host[:port]/path@sha256:<64 hex>.
//
// A bare repository (no tag, no digest) is refused with invalid_input; "latest"
// is never assumed. A short, empty or non-sha256 pin is refused with
// invalid_input. The result is always the repository (no tag, no digest) plus a
// full sha256 digest of the exact returned manifest/index bytes. A tag is a
// lookup input only and never appears in the result.
//
// Requests are GET {scheme}://{host}/v2/{path}/manifests/{tag-or-digest} with
// an Accept header advertising the allowed manifest/index media types. The
// scheme is https unless Config.AllowInsecureHTTP is set (a test-only flag that
// selects http).
//
// Digest evidence: the result digest is the SHA-256 of the exact response body.
// When the Docker-Content-Digest response header is present it must equal that
// digest, otherwise the response is refused with invalid_input. For a pinned
// input the computed body digest must equal the pin; a different valid digest
// is refused with changed_idempotency_content (this test file uses
// changed_idempotency_content for that case). An unsupported or missing
// Content-Type, a malformed JSON document (including a non-object document
// such as a top-level array), and an oversized body are invalid_input. A
// Content-Type carrying parameters is judged by its parsed media type.
//
// Config.AllowedMediaTypes defaults to DefaultAllowedMediaTypes() (the four OCI
// and Docker manifest and index types). A response media type outside the
// effective allow-list is invalid_input. MaxResponseBytes bounds the response
// body inclusively (a body of exactly the bound is accepted); MaxResponseBytes
// and MaxRedirects of zero select DefaultMaxResponseBytes and
// DefaultMaxRedirects, and a negative bound, a negative cap or an empty or
// unparsable allowed media type is refused by New/Validate with invalid_input.
// A refused resolution returns the zero pipeline.ResolvedImage.
//
// Credentials are installation-level and are read through Config.Credentials
// with the origin string "{scheme}://{host}" (for example
// "https://registry.example.com"). CredentialBearer sends
// "Authorization: Bearer <Token>", CredentialBasic sends
// "Authorization: Basic base64(Username:Password)", and CredentialAnonymous (or
// a nil source) sends no Authorization header. Credentials are only sent to the
// configured origin: a same-origin redirect keeps them, a cross-origin redirect
// never forwards them, and the source is consulted only for the configured
// registry origin and, when a Bearer challenge is answered, the token realm's
// own origin.
// Redirects are followed up to MaxRedirects hops; exceeding the cap, or a
// redirect that downgrades the configured scheme to http while
// AllowInsecureHTTP is unset, is refused with invalid_input. Neither errors nor
// results contain credentials or response bodies.
//
// Fault mapping: transport errors, 5xx and 429 are dependency_unavailable;
// 401 is unauthenticated; 403 is forbidden; 404 is not_found; a canceled or
// expired context is deadline_expired wrapping the context error (errors.Is
// still matches context.Canceled/context.DeadlineExceeded).
package registry_test

import (
	"github.com/optimiware/kodama/internal/shell/integration/registry"
	"github.com/optimiware/kodama/internal/shell/pipeline"
)

// Compile-time conformance: the registry resolver is one pipeline.Resolver.
var _ pipeline.Resolver = (*registry.Resolver)(nil)
