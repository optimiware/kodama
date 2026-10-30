package oci_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	artifactsv1 "github.com/optimiware/kodama/contracts/artifacts/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/artifacts/oci"
)

func Test_unit_publication_complete_oci_graph(t *testing.T) {
	cfgBytes := []byte(`{"architecture":"amd64","os":"linux"}`)
	cfgDigest := digestOf(cfgBytes)
	layerBytes := []byte("layer-tar-content-bytes")
	layerDigest := digestOf(layerBytes)

	childManifest := []byte(fmt.Sprintf(
		`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":%q,"size":%d}]}`,
		ociManifest, cfgDigest, len(cfgBytes), layerDigest, len(layerBytes),
	))
	childDigest := digestOf(childManifest)

	indexMediaType := "application/vnd.oci.image.index.v1+json"
	indexBytes := []byte(fmt.Sprintf(
		`{"schemaVersion":2,"mediaType":%q,"manifests":[{"mediaType":%q,"digest":%q,"size":%d}]}`,
		indexMediaType, ociManifest, childDigest, len(childManifest),
	))
	indexDigest := digestOf(indexBytes)

	missingLayer := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/acme/inventory/manifests/" + string(indexDigest):
			serveManifest(w, indexBytes, indexMediaType, string(indexDigest))
		case "/v2/acme/inventory/manifests/" + string(childDigest):
			serveManifest(w, childManifest, ociManifest, string(childDigest))
		case "/v2/acme/inventory/blobs/" + string(cfgDigest):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(cfgBytes)
		case "/v2/acme/inventory/blobs/" + string(layerDigest):
			if missingLayer {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(layerBytes)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	v, _ := verifier(t, srv.URL, true, "Bearer prod-token", &provenance{prov: prov}, nil)
	dest := artifactsv1.DestinationRef{
		Kind:      artifactsv1.DestinationOCI,
		Reference: hostOf(t, srv.URL) + "/acme/inventory",
		Digest:    runtimev1.Digest(indexDigest),
	}

	// Complete multi-node index -> manifest -> config + layer graph passes.
	if err := v.VerifyCompleteGraph(t.Context(), scope, "att_pub", dest); err != nil {
		t.Fatalf("VerifyCompleteGraph complete graph failed: %v", err)
	}

	// Missing layer blob in destination registry fails verification.
	missingLayer = true
	if err := v.VerifyCompleteGraph(t.Context(), scope, "att_pub", dest); !errors.Is(err, artifacts.ErrRegistryContent) {
		t.Fatalf("expected ErrRegistryContent when a layer blob is missing, got %v", err)
	}
}

func Test_unit_publication_destination_authority(t *testing.T) {
	manifest := manifestJSON("")
	dg := digestOf(manifest)
	srv := newFake(t, manifest)

	pol := &policy{endpoint: func(ref artifacts.RegistryRef) (oci.Endpoint, error) {
		if !strings.HasPrefix(ref.Repository, "acme/") {
			return oci.Endpoint{}, artifacts.ErrRegistryDenied
		}
		return oci.Endpoint{BaseURL: srv.srv.URL, Insecure: true}, nil
	}}
	v, err := oci.NewVerifier(oci.Config{Policy: pol, Provenance: &provenance{prov: prov}})
	if err != nil {
		t.Fatal(err)
	}

	// Unapproved destination repository is denied before any network fetch.
	unapproved := artifactsv1.DestinationRef{
		Kind:      artifactsv1.DestinationOCI,
		Reference: hostOf(t, srv.srv.URL) + "/other-tenant/inventory",
		Digest:    runtimev1.Digest(dg),
	}
	if err := v.VerifyCompleteGraph(t.Context(), scope, "att_pub", unapproved); !errors.Is(err, artifacts.ErrRegistryDenied) {
		t.Fatalf("expected ErrRegistryDenied for unapproved destination, got %v", err)
	}
	if srv.hits.Load() != 0 {
		t.Fatalf("expected 0 network requests for denied destination, got %d", srv.hits.Load())
	}
}
