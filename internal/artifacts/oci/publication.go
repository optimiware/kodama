package oci

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	artifactsv1 "github.com/optimiware/kodama/contracts/artifacts/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
)

const (
	DefaultMaxGraphManifests = 32
	DefaultMaxGraphBlobs     = 256
	DefaultMaxBlobBytes      = 512 << 20
)

type graphDescriptor struct {
	MediaType string   `json:"mediaType"`
	Digest    string   `json:"digest"`
	Size      *int64   `json:"size"`
	URLs      []string `json:"urls"`
}

type manifestMeta struct {
	size      int64
	mediaType string
}

// VerifyCompleteGraph verifies that an OCI destination repository holds the
// root manifest or index at dest.Digest AND every referenced child manifest,
// config blob, and layer/chart blob required for a complete pull. External
// descriptor URLs, missing blobs, size mismatches, media-type mismatches, and
// digest mismatches fail.
func (v *Verifier) VerifyCompleteGraph(ctx context.Context, scope domain.Scope, attempt domain.AttemptID, dest artifactsv1.DestinationRef) error {
	if dest.Kind != artifactsv1.DestinationOCI {
		return fmt.Errorf("%w: not an OCI destination", artifacts.ErrInvalid)
	}
	ref, err := artifacts.ParseRegistryRef(dest.Reference)
	if err != nil {
		return artifacts.ErrRegistryDenied
	}
	wantDigest := artifacts.Digest(dest.Digest)
	if err := wantDigest.Validate(); err != nil {
		return artifacts.ErrRegistryDenied
	}
	ep, err := v.cfg.Policy.Authorize(ctx, scope, attempt, ref)
	if err != nil {
		if errors.Is(err, artifacts.ErrRegistryDenied) {
			return artifacts.ErrRegistryDenied
		}
		return fmt.Errorf("%w: repository policy", artifacts.ErrRegistryUnavailable)
	}
	seenManifests := map[artifacts.Digest]manifestMeta{}
	seenBlobs := map[artifacts.Digest]int64{}
	return v.verifyManifestNode(ctx, ep, ref, wantDigest, nil, "", 0, seenManifests, seenBlobs)
}

func (v *Verifier) verifyManifestNode(
	ctx context.Context,
	ep Endpoint,
	ref artifacts.RegistryRef,
	d artifacts.Digest,
	wantSize *int64,
	wantMediaType string,
	depth int,
	seenManifests map[artifacts.Digest]manifestMeta,
	seenBlobs map[artifacts.Digest]int64,
) error {
	if depth > 2 {
		return fmt.Errorf("%w: OCI index nesting exceeds maximum depth", artifacts.ErrRegistryContent)
	}
	if prev, ok := seenManifests[d]; ok {
		if wantSize != nil && prev.size != *wantSize {
			return fmt.Errorf("%w: child manifest %s size %d disagrees with verified size %d", artifacts.ErrRegistryContent, d, *wantSize, prev.size)
		}
		if wantMediaType != "" && prev.mediaType != wantMediaType {
			return fmt.Errorf("%w: child manifest %s mediaType %q disagrees with verified %q", artifacts.ErrRegistryContent, d, wantMediaType, prev.mediaType)
		}
		return nil
	}
	if len(seenManifests) >= DefaultMaxGraphManifests {
		return fmt.Errorf("%w: OCI manifest graph exceeds %d manifests", artifacts.ErrRegistryContent, DefaultMaxGraphManifests)
	}
	target, err := v.target(ep, ref, d)
	if err != nil {
		return err
	}
	body, mediaType, err := v.fetch(ctx, ep, target, d)
	if err != nil {
		return err
	}
	if wantSize != nil && int64(len(body)) != *wantSize {
		return fmt.Errorf("%w: child manifest %s size %d != declared %d", artifacts.ErrRegistryContent, d, len(body), *wantSize)
	}
	if wantMediaType != "" && mediaType != wantMediaType {
		return fmt.Errorf("%w: child manifest %s mediaType %q != declared %q", artifacts.ErrRegistryContent, d, mediaType, wantMediaType)
	}
	if err := validateManifest(body, mediaType); err != nil {
		return err
	}
	seenManifests[d] = manifestMeta{size: int64(len(body)), mediaType: mediaType}

	var parsed struct {
		Config    *graphDescriptor  `json:"config"`
		Layers    []graphDescriptor `json:"layers"`
		Manifests []graphDescriptor `json:"manifests"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("%w: invalid manifest JSON", artifacts.ErrRegistryContent)
	}

	switch mediaType {
	case "application/vnd.oci.image.index.v1+json", "application/vnd.docker.distribution.manifest.list.v2+json":
		for _, child := range parsed.Manifests {
			if len(child.URLs) > 0 {
				return fmt.Errorf("%w: external manifest URLs are not permitted in verified copies", artifacts.ErrRegistryContent)
			}
			cd := artifacts.Digest(child.Digest)
			if err := cd.Validate(); err != nil || child.Size == nil || *child.Size < 0 || !isManifestType(child.MediaType) {
				return fmt.Errorf("%w: invalid child manifest descriptor", artifacts.ErrRegistryContent)
			}
			if err := v.verifyManifestNode(ctx, ep, ref, cd, child.Size, child.MediaType, depth+1, seenManifests, seenBlobs); err != nil {
				return err
			}
		}
	default:
		if parsed.Config == nil {
			return fmt.Errorf("%w: manifest missing config descriptor", artifacts.ErrRegistryContent)
		}
		allBlobs := append([]graphDescriptor{*parsed.Config}, parsed.Layers...)
		for _, blob := range allBlobs {
			if len(blob.URLs) > 0 {
				return fmt.Errorf("%w: external blob URLs are not permitted in verified copies", artifacts.ErrRegistryContent)
			}
			bd := artifacts.Digest(blob.Digest)
			if err := bd.Validate(); err != nil || blob.Size == nil || *blob.Size < 0 || *blob.Size > DefaultMaxBlobBytes {
				return fmt.Errorf("%w: invalid blob descriptor", artifacts.ErrRegistryContent)
			}
			if prevSize, ok := seenBlobs[bd]; ok {
				if prevSize != *blob.Size {
					return fmt.Errorf("%w: blob %s declared with conflicting sizes (%d vs %d)", artifacts.ErrRegistryContent, bd, *blob.Size, prevSize)
				}
				continue
			}
			if len(seenBlobs) >= DefaultMaxGraphBlobs {
				return fmt.Errorf("%w: OCI graph exceeds %d blobs", artifacts.ErrRegistryContent, DefaultMaxGraphBlobs)
			}
			if err := v.verifyBlob(ctx, ep, ref, bd, *blob.Size); err != nil {
				return err
			}
			seenBlobs[bd] = *blob.Size
		}
	}
	return nil
}

func (v *Verifier) verifyBlob(ctx context.Context, ep Endpoint, ref artifacts.RegistryRef, want artifacts.Digest, wantSize int64) error {
	base, err := v.target(ep, ref, want)
	if err != nil {
		return err
	}
	blobURL := string(bytes.Replace([]byte(base), []byte("/manifests/"+string(want)), []byte("/blobs/"+string(want)), 1))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, blobURL, nil)
	if err != nil {
		return artifacts.ErrRegistryDenied
	}
	if ep.Authorization != "" {
		req.Header.Set("Authorization", ep.Authorization)
	}
	resp, err := v.client.Do(req)
	if err != nil {
		if errors.Is(err, errUnsafeRedirect) {
			return fmt.Errorf("%w: unsafe redirect", artifacts.ErrRegistryContent)
		}
		return fmt.Errorf("%w: blob request failed", artifacts.ErrRegistryUnavailable)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return artifacts.ErrRegistryDenied
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500, resp.StatusCode == http.StatusRequestTimeout:
		return fmt.Errorf("%w: blob status %d", artifacts.ErrRegistryUnavailable, resp.StatusCode)
	default:
		return fmt.Errorf("%w: missing or invalid blob %s (status %d)", artifacts.ErrRegistryContent, want, resp.StatusCode)
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(resp.Body, wantSize+1))
	if err != nil {
		return fmt.Errorf("%w: reading blob", artifacts.ErrRegistryUnavailable)
	}
	if n != wantSize {
		return fmt.Errorf("%w: blob %s size %d != %d", artifacts.ErrRegistryContent, want, n, wantSize)
	}
	if got := artifacts.DigestOfSum(h.Sum(nil)); got != want {
		return fmt.Errorf("%w: blob %s hash mismatch", artifacts.ErrRegistryContent, want)
	}
	return nil
}
