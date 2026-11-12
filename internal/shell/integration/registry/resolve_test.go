package registry_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/integration/registry"
	"github.com/optimiware/kodama/internal/shell/pipeline"
)

// TestResolveTagToDigest pins the core admission read: a mutable tag is a
// lookup input only, and the result carries the repository plus the immutable
// sha256 of the exact manifest bytes.
func TestResolveTagToDigest(t *testing.T) {
	body := singleManifestBody
	transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
		return registryResponse(request, http.StatusOK, dockerManifestMediaType, body, digestOf(body)), nil
	}}
	resolver := newResolver(t, registry.Config{Transport: transport})

	image, err := resolveWith(t, resolver, tagReference)
	if err != nil {
		t.Fatalf("Resolve refused a tag reference: %v", err)
	}
	requireResolved(t, image, repository, digestOf(body))

	requests := transport.captured()
	if len(requests) != 1 {
		t.Fatalf("tag resolution issued %d requests, want exactly 1", len(requests))
	}
	request := requests[0]
	if request.Method != http.MethodGet {
		t.Fatalf("manifest method = %q, want GET", request.Method)
	}
	if want := manifestURL("https", registryOrigin, repositoryPath, imageTag); request.URL != want {
		t.Fatalf("manifest URL = %q, want %q", request.URL, want)
	}
	if got := request.Header.Get("Authorization"); got != "" {
		t.Fatalf("anonymous resolution sent Authorization %q", got)
	}
	accept := request.Header.Get("Accept")
	for _, mediaType := range registry.DefaultAllowedMediaTypes() {
		if !strings.Contains(accept, mediaType) {
			t.Fatalf("Accept header %q does not advertise %q", accept, mediaType)
		}
	}
}

// TestResolveTagWithoutDigestHeader shows the body hash is the primary
// evidence; Docker-Content-Digest is only a cross-check when present.
func TestResolveTagWithoutDigestHeader(t *testing.T) {
	body := singleManifestBody
	transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
		return registryResponse(request, http.StatusOK, dockerManifestMediaType, body, ""), nil
	}}
	resolver := newResolver(t, registry.Config{Transport: transport})

	image, err := resolveWith(t, resolver, tagReference)
	if err != nil {
		t.Fatalf("Resolve refused a manifest without Docker-Content-Digest: %v", err)
	}
	requireResolved(t, image, repository, digestOf(body))
}

// TestResolveIndexKeepsIndexDigest pins multi-platform semantics: an index
// resolves to the digest of the index bytes, never to one child platform
// manifest digest.
func TestResolveIndexKeepsIndexDigest(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		body        string
	}{
		{"oci index", ociIndexMediaType, ociIndexBody},
		{"docker index", dockerIndexMediaType, dockerIndexBody},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
				return registryResponse(request, http.StatusOK, testCase.contentType, testCase.body, digestOf(testCase.body)), nil
			}}
			resolver := newResolver(t, registry.Config{Transport: transport})

			image, err := resolveWith(t, resolver, tagReference)
			if err != nil {
				t.Fatalf("Resolve refused a %s: %v", testCase.name, err)
			}
			requireResolved(t, image, repository, digestOf(testCase.body))
			if image.Digest == childDigest || image.Digest == otherDigest {
				t.Fatalf("index resolved to a child manifest digest %q instead of the index digest", image.Digest)
			}
		})
	}
}

// TestResolveHostWithPort pins origin parsing for a registry that carries a
// port, including the origin string handed to the credential source.
func TestResolveHostWithPort(t *testing.T) {
	const token = "port-scoped-token"
	body := singleManifestBody
	source := &staticCredentialSource{
		origin:     "https://" + registryPortHost,
		credential: registry.Credential{Scheme: registry.CredentialBearer, Token: token},
	}
	transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
		return registryResponse(request, http.StatusOK, dockerManifestMediaType, body, digestOf(body)), nil
	}}
	resolver := newResolver(t, registry.Config{Transport: transport, Credentials: source})

	reference := registryPortHost + "/" + repositoryPath + ":" + imageTag
	image, err := resolveWith(t, resolver, reference)
	if err != nil {
		t.Fatalf("Resolve refused a registry host with a port: %v", err)
	}
	requireResolved(t, image, registryPortHost+"/"+repositoryPath, digestOf(body))

	requests := transport.captured()
	if len(requests) != 1 {
		t.Fatalf("resolution issued %d requests, want exactly 1", len(requests))
	}
	if want := manifestURL("https", registryPortHost, repositoryPath, imageTag); requests[0].URL != want {
		t.Fatalf("manifest URL = %q, want %q", requests[0].URL, want)
	}
	if got := requests[0].Header.Get("Authorization"); got != "Bearer "+token {
		t.Fatalf("Authorization = %q, want %q", got, "Bearer "+token)
	}
	requireOrigins(t, source, "https://"+registryPortHost)
}

// TestResolvePinnedReference pins already-immutable input: the registry answer
// must match the requested digest exactly and the tag/digest suffix is dropped
// from the resulting repository.
func TestResolvePinnedReference(t *testing.T) {
	body := singleManifestBody
	pin := digestOf(body)
	transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
		return registryResponse(request, http.StatusOK, dockerManifestMediaType, body, pin), nil
	}}
	resolver := newResolver(t, registry.Config{Transport: transport})

	image, err := resolveWith(t, resolver, repository+"@"+pin)
	if err != nil {
		t.Fatalf("Resolve refused an already-pinned reference: %v", err)
	}
	requireResolved(t, image, repository, pin)

	requests := transport.captured()
	if len(requests) != 1 {
		t.Fatalf("pinned resolution issued %d requests, want exactly 1", len(requests))
	}
	if want := manifestURL("https", registryOrigin, repositoryPath, pin); requests[0].URL != want {
		t.Fatalf("manifest URL = %q, want %q", requests[0].URL, want)
	}
}

// TestResolvePinnedMismatchRefused pins the changed-content refusal: the
// registry returning a different valid digest than the pin can never be
// accepted as that pin.
func TestResolvePinnedMismatchRefused(t *testing.T) {
	pin := digestOf(secondManifestBody)
	body := singleManifestBody
	cases := []struct {
		name      string
		headerPin string
	}{
		{"header agrees with body", digestOf(body)},
		{"header absent", ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
				return registryResponse(request, http.StatusOK, dockerManifestMediaType, body, testCase.headerPin), nil
			}}
			resolver := newResolver(t, registry.Config{Transport: transport})

			image, err := resolveWith(t, resolver, repository+"@"+pin)
			requireFault(t, err, fault.ChangedIdempotencyContent)
			requireNoDigest(t, image)
		})
	}
}

// TestResolveDigestHeaderMismatchRefused pins digest evidence: a header that
// disagrees with the returned bytes is a registry self-inconsistency, never a
// usable pin.
func TestResolveDigestHeaderMismatchRefused(t *testing.T) {
	body := singleManifestBody
	transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
		return registryResponse(request, http.StatusOK, dockerManifestMediaType, body, otherDigest), nil
	}}
	resolver := newResolver(t, registry.Config{Transport: transport})

	image, err := resolveWith(t, resolver, tagReference)
	requireFault(t, err, fault.InvalidInput)
	requireNoDigest(t, image)
}

// TestResolveInvalidReferenceRefused pins input grammar: a bare repository is
// not "latest", and only a full sha256 pin is a pin.
func TestResolveInvalidReferenceRefused(t *testing.T) {
	cases := []struct {
		name      string
		reference string
	}{
		{"bare repository", repository},
		{"bare repository with port", registryPortHost + "/" + repositoryPath},
		{"host only", registryOrigin},
		{"empty reference", ""},
		{"short digest pin", repository + "@sha256:abcd"},
		{"non sha256 pin", repository + "@md5:" + strings.Repeat("a", 32)},
		{"empty digest pin", repository + "@"},
		{"empty tag", repository + ":"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			transport := &fakeTransport{}
			resolver := newResolver(t, registry.Config{Transport: transport})

			image, err := resolveWith(t, resolver, testCase.reference)
			requireFault(t, err, fault.InvalidInput)
			requireNoDigest(t, image)
			if !errors.Is(err, pipeline.ErrImageReference) {
				t.Fatalf("refused reference %q is not marked as an image reference fault: %v", testCase.reference, err)
			}
			if requests := transport.captured(); len(requests) != 0 {
				t.Fatalf("refused reference %q still issued %d registry requests", testCase.reference, len(requests))
			}
		})
	}
}

// TestResolveTagAndDigestRefusalNamesTheFix pins the message for the reference
// shape installers print (repository:tag@sha256:<digest>): the refusal says the
// tag must go, instead of calling the repository path invalid.
func TestResolveTagAndDigestRefusalNamesTheFix(t *testing.T) {
	transport := &fakeTransport{}
	resolver := newResolver(t, registry.Config{Transport: transport})

	image, err := resolveWith(t, resolver, tagReference+"@"+digestOf(singleManifestBody))
	requireFault(t, err, fault.InvalidInput)
	requireNoDigest(t, image)
	if !errors.Is(err, pipeline.ErrImageReference) {
		t.Fatalf("a tag-and-digest reference is not marked as an image reference fault: %v", err)
	}
	message, _ := fault.MessageOf(err)
	if !strings.Contains(message, "both a tag and a digest") || !strings.Contains(message, "repository@sha256:<digest>") {
		t.Fatalf("refusal message = %q; want it to name the tag-and-digest shape and the accepted form", message)
	}
	if requests := transport.captured(); len(requests) != 0 {
		t.Fatalf("a refused reference still issued %d registry requests", len(requests))
	}
}

// TestResolveRegistryAnomalyIsNotAReferenceFault pins the other side: an
// invalid_input fault about what the registry answered is not a refusal of the
// reference text, so admission keeps treating it as an unavailable registry.
func TestResolveRegistryAnomalyIsNotAReferenceFault(t *testing.T) {
	body := singleManifestBody
	transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
		return registryResponse(request, http.StatusOK, "application/vnd.example.unknown+json", body, digestOf(body)), nil
	}}
	resolver := newResolver(t, registry.Config{Transport: transport})

	_, err := resolveWith(t, resolver, tagReference)
	requireFault(t, err, fault.InvalidInput)
	if errors.Is(err, pipeline.ErrImageReference) {
		t.Fatalf("a registry response anomaly is marked as an image reference fault: %v", err)
	}
}

// TestResolveMediaTypePolicy pins the configured manifest/index allow-list and
// the media-type evidence rules.
func TestResolveMediaTypePolicy(t *testing.T) {
	body := singleManifestBody
	t.Run("unsupported media type refused", func(t *testing.T) {
		transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
			return registryResponse(request, http.StatusOK, "application/vnd.example.unknown+json", body, digestOf(body)), nil
		}}
		resolver := newResolver(t, registry.Config{Transport: transport})
		image, err := resolveWith(t, resolver, tagReference)
		requireFault(t, err, fault.InvalidInput)
		requireNoDigest(t, image)
	})

	t.Run("missing content type refused", func(t *testing.T) {
		transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
			return registryResponse(request, http.StatusOK, "", body, digestOf(body)), nil
		}}
		resolver := newResolver(t, registry.Config{Transport: transport})
		image, err := resolveWith(t, resolver, tagReference)
		requireFault(t, err, fault.InvalidInput)
		requireNoDigest(t, image)
	})

	t.Run("content type parameters tolerated", func(t *testing.T) {
		transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
			return registryResponse(request, http.StatusOK, ociManifestMediaType+"; charset=utf-8", ociManifestBody, digestOf(ociManifestBody)), nil
		}}
		resolver := newResolver(t, registry.Config{Transport: transport})
		image, err := resolveWith(t, resolver, tagReference)
		if err != nil {
			t.Fatalf("Resolve refused a media type with parameters: %v", err)
		}
		requireResolved(t, image, repository, digestOf(ociManifestBody))
	})

	t.Run("configured allow list refuses docker manifest", func(t *testing.T) {
		transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
			return registryResponse(request, http.StatusOK, dockerManifestMediaType, body, digestOf(body)), nil
		}}
		resolver := newResolver(t, registry.Config{
			Transport:         transport,
			AllowedMediaTypes: []string{ociManifestMediaType},
		})
		image, err := resolveWith(t, resolver, tagReference)
		requireFault(t, err, fault.InvalidInput)
		requireNoDigest(t, image)
	})

	t.Run("configured allow list accepts oci manifest", func(t *testing.T) {
		transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
			return registryResponse(request, http.StatusOK, ociManifestMediaType, ociManifestBody, digestOf(ociManifestBody)), nil
		}}
		resolver := newResolver(t, registry.Config{
			Transport:         transport,
			AllowedMediaTypes: []string{ociManifestMediaType},
		})
		image, err := resolveWith(t, resolver, tagReference)
		if err != nil {
			t.Fatalf("Resolve refused a configured media type: %v", err)
		}
		requireResolved(t, image, repository, digestOf(ociManifestBody))
		if accept := transport.captured()[0].Header.Get("Accept"); !strings.Contains(accept, ociManifestMediaType) {
			t.Fatalf("Accept header %q does not advertise the configured media type", accept)
		}
	})
}

// TestResolveResponseBound pins the bounded response size, including the exact
// boundary: a body of exactly MaxResponseBytes is accepted, one byte more is
// refused before it can become admission content.
func TestResolveResponseBound(t *testing.T) {
	const bound = 64
	accepted := padJSON(`{"schemaVersion":2}`, bound)
	oversize := padJSON(`{"schemaVersion":2}`, bound+1)
	if len(accepted) != bound || len(oversize) != bound+1 {
		t.Fatalf("fixture sizes are wrong: %d and %d", len(accepted), len(oversize))
	}

	t.Run("exact bound accepted", func(t *testing.T) {
		transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
			return registryResponse(request, http.StatusOK, dockerManifestMediaType, accepted, digestOf(accepted)), nil
		}}
		resolver := newResolver(t, registry.Config{Transport: transport, MaxResponseBytes: bound})
		image, err := resolveWith(t, resolver, tagReference)
		if err != nil {
			t.Fatalf("Resolve refused a body of exactly the bound: %v", err)
		}
		requireResolved(t, image, repository, digestOf(accepted))
	})

	t.Run("one byte over refused", func(t *testing.T) {
		transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
			return registryResponse(request, http.StatusOK, dockerManifestMediaType, oversize, digestOf(oversize)), nil
		}}
		resolver := newResolver(t, registry.Config{Transport: transport, MaxResponseBytes: bound})
		image, err := resolveWith(t, resolver, tagReference)
		requireFault(t, err, fault.InvalidInput)
		requireNoDigest(t, image)
	})
}

// TestResolveManifestBodyReadBounded pins that the manifest read itself is
// bounded, not only the size check that follows it: a registry that streams an
// endless manifest body must be cut off after the configured bound instead of
// being drained and then judged too large.
func TestResolveManifestBodyReadBounded(t *testing.T) {
	const slack = int64(64 << 10)
	body := newEndlessBody(4 << 20)
	transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
		return responseWithBody(request, http.StatusOK, dockerManifestMediaType, body), nil
	}}
	resolver := newResolver(t, registry.Config{Transport: transport})
	image, err := resolveWith(t, resolver, tagReference)

	if allowed := registry.DefaultMaxResponseBytes + 1 + slack; body.read > allowed {
		t.Fatalf("manifest body read consumed %d bytes, want at most %d: the read is not bounded", body.read, allowed)
	}
	requireFault(t, err, fault.InvalidInput)
	requireNoDigest(t, image)
	if requests := transport.captured(); len(requests) != 1 {
		t.Fatalf("resolution issued %d requests, want exactly 1", len(requests))
	}
}

// TestResolveDefaultResponseBound pins that a zero MaxResponseBytes selects
// DefaultMaxResponseBytes rather than leaving the read unbounded.
func TestResolveDefaultResponseBound(t *testing.T) {
	accepted := padJSON(`{"schemaVersion":2}`, int(registry.DefaultMaxResponseBytes))
	oversize := padJSON(`{"schemaVersion":2}`, int(registry.DefaultMaxResponseBytes)+1)

	transport := &fakeTransport{handle: func(call int, request *http.Request) (*http.Response, error) {
		if call == 0 {
			return registryResponse(request, http.StatusOK, dockerManifestMediaType, accepted, digestOf(accepted)), nil
		}
		return registryResponse(request, http.StatusOK, dockerManifestMediaType, oversize, digestOf(oversize)), nil
	}}
	resolver := newResolver(t, registry.Config{Transport: transport})

	image, err := resolveWith(t, resolver, tagReference)
	if err != nil {
		t.Fatalf("Resolve refused a body of exactly the default bound: %v", err)
	}
	requireResolved(t, image, repository, digestOf(accepted))

	image, err = resolveWith(t, resolver, tagReference)
	requireFault(t, err, fault.InvalidInput)
	requireNoDigest(t, image)
}

// TestResolveMalformedJSONRefused pins that a syntactically broken manifest is
// refused even when the media type and digest evidence look valid.
func TestResolveMalformedJSONRefused(t *testing.T) {
	bodies := map[string]string{
		"truncated":  `{"schemaVersion":`,
		"empty body": "",
		"not object": `[]`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
				return registryResponse(request, http.StatusOK, dockerManifestMediaType, body, digestOf(body)), nil
			}}
			resolver := newResolver(t, registry.Config{Transport: transport})
			image, err := resolveWith(t, resolver, tagReference)
			requireFault(t, err, fault.InvalidInput)
			requireNoDigest(t, image)
		})
	}
}
