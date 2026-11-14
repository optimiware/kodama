// Package pipeline declares the registry-resolution seam.
//
// Admission resolves every image to an immutable sha256 digest before it
// reaches the snapshot. A mutable tag is never a pinned reference, and a short
// or non-sha256 digest is refused. Resolution is a pre-admission effect and is
// never held inside a domain transaction.
//
// No registry client, credential store or network call exists here.
package pipeline

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/optimiware/kodama/internal/fault"
)

// ImageReference is one parsed immutable image reference. Reference is the
// repository (and optional tag) portion; Digest is the exact pinned digest.
type ImageReference struct {
	Reference string
	Digest    string
}

// ParseImageReference parses one reference that must carry an immutable sha256
// digest. Mutable tags, bare repositories and short/non-sha256 digests are
// refused.
func ParseImageReference(value string) (ImageReference, error) {
	parts := strings.Split(value, "@")
	if len(parts) != 2 {
		return ImageReference{}, fault.New(fault.InvalidInput, "image reference must carry an immutable sha256 digest")
	}
	repository := parts[0]
	digest := parts[1]
	if repository == "" || strings.ContainsAny(repository, " \t\r\n") {
		return ImageReference{}, fault.New(fault.InvalidInput, "image reference repository is invalid")
	}
	if !validImageDigest(digest) {
		return ImageReference{}, fault.New(fault.InvalidInput, "image reference digest must be a full sha256 digest")
	}
	return ImageReference{Reference: repository, Digest: digest}, nil
}

// Validate checks that the reference and digest are still usable.
func (reference ImageReference) Validate() error {
	if reference.Reference == "" {
		return fault.New(fault.InvalidInput, "image reference repository is empty")
	}
	if !validImageDigest(reference.Digest) {
		return fault.New(fault.InvalidInput, "image digest must be a full sha256 digest")
	}
	return nil
}

// ResolveRequest asks the registry adapter for the immutable digest of one
// reference. It is a pre-admission read and carries no lifecycle authority.
type ResolveRequest struct {
	Reference string
}

// Validate checks that a reference is present.
func (request ResolveRequest) Validate() error {
	if request.Reference == "" {
		return fault.New(fault.InvalidInput, "resolve request reference is empty")
	}
	return nil
}

// ResolvedImage is one registry answer. Only a full sha256 digest is a valid
// immutable result.
type ResolvedImage struct {
	Reference string
	Digest    string
}

// Validate checks the resolved repository and digest.
func (resolved ResolvedImage) Validate() error {
	if resolved.Reference == "" {
		return fault.New(fault.InvalidInput, "resolved image reference is empty")
	}
	if !validImageDigest(resolved.Digest) {
		return fault.New(fault.InvalidInput, "resolved image digest must be a full sha256 digest")
	}
	return nil
}

// ErrImageReference marks a resolver refusal of the reference text itself: the
// text cannot name an image, so no retry can succeed. A resolver reports it as
// the cause of an invalid_input fault whose message says what is wrong with the
// reference. Admission treats every other resolver failure as an unavailable
// registry.
var ErrImageReference = errors.New("image reference is invalid")

// Resolver resolves a reference to an immutable sha256 digest before admission.
type Resolver interface {
	Resolve(context.Context, ResolveRequest) (ResolvedImage, error)
}

func validImageDigest(value string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	hexValue := strings.TrimPrefix(value, prefix)
	if len(hexValue) != 64 {
		return false
	}
	_, err := hex.DecodeString(hexValue)
	return err == nil
}
