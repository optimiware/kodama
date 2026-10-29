package delivery

import (
	"regexp"
	"sort"
	"strings"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// A deployable artifact is what a target receives: an OCI image for a container
// target, or an immutable file bundle for a static/Worker/Pages target. The
// legacy Image field remains the OCI projection so released clients keep
// working; new deployments may name either shape.
type DeployableArtifact struct {
	Kind       ArtifactKind      `json:"kind"`
	ArtifactID domain.ArtifactID `json:"artifactId"`
	Digest     runtimev1.Digest  `json:"digest"`
	MediaType  string            `json:"mediaType,omitempty"`
}

// Validate checks the shape. The kind is always required; a bundle names its
// media type so a driver can refuse an incompatible archive.
func (a DeployableArtifact) Validate() error {
	if !a.Kind.Valid() {
		return invalidf("a deployable artifact is an OCI image or a bundle")
	}
	if err := a.ArtifactID.Validate(); err != nil {
		return invalidf("%v", err)
	}
	if err := a.Digest.Validate(); err != nil {
		return invalidf("an artifact digest is sha256:<64 hex>")
	}
	if a.Kind == ArtifactBundle {
		if len(a.MediaType) < 1 || len(a.MediaType) > 128 || strings.ContainsAny(a.MediaType, " \t\r\n\x00") {
			return invalidf("a bundle names its media type")
		}
	} else if a.MediaType != "" {
		return invalidf("an OCI artifact carries no media type here")
	}
	return nil
}

// FromImage projects the legacy OCI image.
func FromImage(i Image) DeployableArtifact {
	return DeployableArtifact{Kind: ArtifactOCIImage, ArtifactID: i.ArtifactID, Digest: i.Digest}
}

// Image projects an OCI artifact to the legacy member; it is zero for a bundle.
func (a DeployableArtifact) Image() Image {
	if a.Kind != ArtifactOCIImage {
		return Image{}
	}
	return Image{ArtifactID: a.ArtifactID, Digest: a.Digest}
}

// ResourceRef identifies one external resource a deployment owns. It is
// provider-neutral: Kubernetes objects, Workers versions, routes and secret
// versions are all resources. Provider stays explicit so two providers cannot
// collide on a resource id.
type ResourceRef struct {
	Provider string            `json:"provider"`
	Type     string            `json:"type"`
	ID       string            `json:"id"`
	Name     string            `json:"name,omitempty"`
	Version  string            `json:"version,omitempty"`
	ETag     string            `json:"etag,omitempty"`
	URL      string            `json:"url,omitempty"`
	Ready    bool              `json:"ready,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

var (
	resourceProvider = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	resourceType     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._/-]{0,127}$`)
	resourceID       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)
)

// MaxResources bounds the external resources a deployment owns.
const MaxResources = 64

// Validate checks the identity and the safe metadata bounds.
func (r ResourceRef) Validate() error {
	switch {
	case !resourceProvider.MatchString(r.Provider):
		return invalidf("a resource provider must match %s", resourceProvider)
	case !resourceType.MatchString(r.Type):
		return invalidf("a resource type must match %s", resourceType)
	case !resourceID.MatchString(r.ID):
		return invalidf("a resource id must match %s", resourceID)
	case len(r.Name) > 256 || len(r.Version) > 256 || len(r.ETag) > 256 || len(r.URL) > 2048:
		return invalidf("a resource member is too long")
	case len(r.Metadata) > 16:
		return invalidf("a resource carries at most 16 metadata entries")
	}
	for k, v := range r.Metadata {
		if !configKey.MatchString(k) || len(v) > MaxConfigValue || strings.ContainsRune(v, 0) {
			return invalidf("a resource metadata entry is invalid")
		}
	}
	return nil
}

func (r ResourceRef) key() string { return r.Provider + "|" + r.Type + "|" + r.ID }

func checkResources(in []ResourceRef) error {
	if len(in) > MaxResources {
		return invalidf("a deployment owns at most %d resources", MaxResources)
	}
	seen := map[string]bool{}
	for _, r := range in {
		if err := r.Validate(); err != nil {
			return err
		}
		if seen[r.key()] {
			return invalidf("a resource is listed once")
		}
		seen[r.key()] = true
	}
	return nil
}

func sortedResources(in []ResourceRef) []ResourceRef {
	out := append([]ResourceRef{}, in...)
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out
}

func sameResources(a, b []ResourceRef) bool {
	a, b = sortedResources(a), sortedResources(b)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !sameResource(a[i], b[i]) {
			return false
		}
	}
	return true
}

func sameResource(a, b ResourceRef) bool {
	if a.Provider != b.Provider || a.Type != b.Type || a.ID != b.ID || a.Name != b.Name || a.Version != b.Version ||
		a.ETag != b.ETag || a.URL != b.URL || a.Ready != b.Ready || len(a.Metadata) != len(b.Metadata) {
		return false
	}
	for k, v := range a.Metadata {
		if w, ok := b.Metadata[k]; !ok || w != v {
			return false
		}
	}
	return true
}
