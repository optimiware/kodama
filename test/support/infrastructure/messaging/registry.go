package messaging

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/shell/integration/registry"
	"github.com/optimiware/kodama/test/support/infrastructure/harness"
)

// Registry ports and media types used by the conformance suite.
const (
	RegistryPort = 5000

	OCIManifestMediaType = "application/vnd.oci.image.manifest.v1+json"
	OCIConfigMediaType   = "application/vnd.oci.image.config.v1+json"
	OCILayerMediaType    = "application/vnd.oci.image.layer.v1.tar"
)

// Registry is one real registry started through the infrastructure harness.
type Registry struct {
	Shard     *harness.Shard
	Container *harness.Container
	BaseURL   string
	Auth      bool
	Digest    string
}

// StartRegistry starts the pinned registry image. When auth is true the
// registry enables its bundled "silly" test access controller: an
// unauthenticated request gets 401 and any presented credential is accepted,
// which exercises the adapter's real authorization path without inventing an
// htpasswd file the harness cannot mount.
func StartRegistry(t *testing.T, auth bool) *Registry {
	t.Helper()
	shard := harness.NewShard(t)
	ctx := shard.Context()
	digest := shard.RequireImage(ctx, harness.ImageRegistry)
	spec := harness.RunSpec{
		Image: harness.ImageRegistry,
		Ports: []harness.Port{{Container: RegistryPort}},
	}
	if auth {
		spec.Env = map[string]string{
			"REGISTRY_AUTH":               "silly",
			"REGISTRY_AUTH_SILLY_REALM":   "kodama-test",
			"REGISTRY_AUTH_SILLY_SERVICE": "registry",
			"REGISTRY_HTTP_SECRET":        "i02c-registry-secret",
		}
	}
	container, err := shard.Run(ctx, spec)
	if err != nil {
		t.Fatalf("start registry: %v", err)
	}
	addr, err := container.Addr(ctx, RegistryPort)
	if err != nil {
		t.Fatalf("registry address: %v", err)
	}
	instance := &Registry{
		Shard:     shard,
		Container: container,
		BaseURL:   "http://" + addr,
		Auth:      auth,
		Digest:    digest,
	}
	if err := shard.WaitReady(ctx, "registry", harness.RegistryProbe(instance.BaseURL)); err != nil {
		if logs, logErr := container.Logs(ctx); logErr == nil {
			t.Logf("registry logs:\n%s", logs)
		}
		t.Fatalf("registry readiness: %v", err)
	}
	RecordImage(harness.ImageRegistry, digest, "registry 3.1")
	return instance
}

// Host returns the host:port the registry is reachable at.
func (instance *Registry) Host() string {
	parsed, err := url.Parse(instance.BaseURL)
	if err != nil {
		return strings.TrimPrefix(instance.BaseURL, "http://")
	}
	return parsed.Host
}

// Reference builds one adapter image reference for a repository and tag.
func (instance *Registry) Reference(repository, tag string) string {
	return instance.Host() + "/" + repository + ":" + tag
}

// BasicCredential is the installation credential the silly controller accepts.
func BasicCredential() registry.Credential {
	return registry.Credential{Scheme: registry.CredentialBasic, Username: "kodama", Password: "test-only"}
}

// Resolver builds the registry adapter against this real registry. The
// transport is the standard one; insecure HTTP is explicitly allowed because
// the pinned registry serves plain HTTP.
func (instance *Registry) Resolver(t *testing.T, credential registry.Credential) *registry.Resolver {
	t.Helper()
	resolver, err := registry.New(registry.Config{
		Transport:         http.DefaultTransport,
		Credentials:       StaticCredential{Value: credential},
		AllowInsecureHTTP: true,
	})
	if err != nil {
		t.Fatalf("build registry resolver: %v", err)
	}
	return resolver
}

// Push publishes one OCI manifest and its blobs and returns the manifest
// digest. Every request is a plain net/http call; no extra dependency is used.
func (instance *Registry) Push(t *testing.T, repository, tag string, manifest []byte, blobs map[string][]byte) string {
	t.Helper()
	client := &http.Client{Timeout: 20 * time.Second}
	for digest, data := range blobs {
		instance.pushBlob(t, client, repository, digest, data)
	}
	request, err := http.NewRequest(http.MethodPut, instance.BaseURL+"/v2/"+repository+"/manifests/"+tag, bytes.NewReader(manifest))
	if err != nil {
		t.Fatalf("manifest request: %v", err)
	}
	request.Header.Set("Content-Type", OCIManifestMediaType)
	instance.authorize(request)
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("push manifest: %v", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("push manifest %s:%s returned %d", repository, tag, response.StatusCode)
	}
	digest := strings.TrimSpace(response.Header.Get("Docker-Content-Digest"))
	if digest == "" {
		digest = DigestOf(manifest)
	}
	return digest
}

func (instance *Registry) pushBlob(t *testing.T, client *http.Client, repository, digest string, data []byte) {
	t.Helper()
	start, err := http.NewRequest(http.MethodPost, instance.BaseURL+"/v2/"+repository+"/blobs/uploads/", nil)
	if err != nil {
		t.Fatalf("blob upload start: %v", err)
	}
	instance.authorize(start)
	response, err := client.Do(start)
	if err != nil {
		t.Fatalf("blob upload start: %v", err)
	}
	location := response.Header.Get("Location")
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	_ = response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("blob upload start returned %d", response.StatusCode)
	}
	if location == "" {
		t.Fatalf("blob upload start returned no location")
	}
	target, err := instance.resolveLocation(location)
	if err != nil {
		t.Fatalf("blob upload location: %v", err)
	}
	separator := "?"
	if strings.Contains(target, "?") {
		separator = "&"
	}
	put, err := http.NewRequest(http.MethodPut, target+separator+"digest="+url.QueryEscape(digest), bytes.NewReader(data))
	if err != nil {
		t.Fatalf("blob upload put: %v", err)
	}
	put.Header.Set("Content-Type", "application/octet-stream")
	instance.authorize(put)
	response, err = client.Do(put)
	if err != nil {
		t.Fatalf("blob upload put: %v", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("blob upload put returned %d", response.StatusCode)
	}
}

func (instance *Registry) resolveLocation(location string) (string, error) {
	parsed, err := url.Parse(location)
	if err != nil {
		return "", err
	}
	if parsed.IsAbs() {
		return location, nil
	}
	base, err := url.Parse(instance.BaseURL)
	if err != nil {
		return "", err
	}
	return base.ResolveReference(parsed).String(), nil
}

func (instance *Registry) authorize(request *http.Request) {
	if !instance.Auth {
		return
	}
	credential := BasicCredential()
	request.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(credential.Username+":"+credential.Password)))
}

// StaticCredential is one fixed installation credential source.
type StaticCredential struct {
	Value registry.Credential
}

// Credential implements registry.CredentialSource.
func (source StaticCredential) Credential(context.Context, string) (registry.Credential, error) {
	return source.Value, nil
}

// DigestOf returns the canonical sha256 digest of data.
func DigestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// OCIManifest builds one minimal OCI image manifest and its blobs. The config
// and layer payloads are arbitrary bytes; the registry stores them unchanged.
func OCIManifest(configData, layerData []byte) ([]byte, map[string][]byte) {
	configDigest := DigestOf(configData)
	layerDigest := DigestOf(layerData)
	type descriptor struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      int    `json:"size"`
	}
	document := struct {
		SchemaVersion int          `json:"schemaVersion"`
		MediaType     string       `json:"mediaType"`
		Config        descriptor   `json:"config"`
		Layers        []descriptor `json:"layers"`
	}{
		SchemaVersion: 2,
		MediaType:     OCIManifestMediaType,
		Config:        descriptor{MediaType: OCIConfigMediaType, Digest: configDigest, Size: len(configData)},
		Layers:        []descriptor{{MediaType: OCILayerMediaType, Digest: layerDigest, Size: len(layerData)}},
	}
	payload, err := json.Marshal(document)
	if err != nil {
		panic(fmt.Sprintf("encode OCI manifest: %v", err))
	}
	return payload, map[string][]byte{configDigest: configData, layerDigest: layerData}
}
