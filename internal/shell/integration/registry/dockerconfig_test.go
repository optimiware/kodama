package registry_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/integration/registry"
)

// dockerConfigBytes renders a Docker config.json with the supplied auths
// object. Values stay explicit so a test can name exactly what it configures.
func dockerConfigBytes(auths string) []byte {
	return []byte(`{"auths":{` + auths + `}}`)
}

// writeDockerConfig writes one credential file into the test's temporary
// directory and returns its path.
func writeDockerConfig(t *testing.T, body []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write docker config: %v", err)
	}
	return path
}

func basicCredential(user, password string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
}

// TestDockerConfigCredentialFile pins the docker-config contract: auths keys
// name registries (with or without a scheme or Docker Hub's /v1/ path), each
// entry becomes a Basic or Bearer credential, and an origin without an entry is
// anonymous rather than refused.
func TestDockerConfigCredentialFile(t *testing.T) {
	const (
		user     = "kodama"
		password = "registry-password-do-not-leak"
		token    = "hub-identity-token-do-not-leak"
	)
	body := fmt.Sprintf(
		`"https://registry.example.com":{"auth":%q},`+
			`"registry.example.com:5000":{"username":"port-user","password":"port-password"},`+
			`"https://index.docker.io/v1/":{"identitytoken":%q}`,
		basicCredential(user, password), token,
	)
	source, err := registry.LoadDockerConfig(writeDockerConfig(t, dockerConfigBytes(body)))
	if err != nil {
		t.Fatalf("LoadDockerConfig refused a valid file: %v", err)
	}

	cases := []struct {
		name   string
		origin string
		want   registry.Credential
	}{
		{
			name:   "scheme key",
			origin: "https://registry.example.com",
			want:   registry.Credential{Scheme: registry.CredentialBasic, Username: user, Password: password},
		},
		{
			name:   "host and port key",
			origin: "https://registry.example.com:5000",
			want:   registry.Credential{Scheme: registry.CredentialBasic, Username: "port-user", Password: "port-password"},
		},
		{
			name:   "docker hub alias",
			origin: "https://registry-1.docker.io",
			want:   registry.Credential{Scheme: registry.CredentialBearer, Token: token},
		},
		{
			name:   "docker hub token realm",
			origin: "https://auth.docker.io",
			want:   registry.Credential{Scheme: registry.CredentialBearer, Token: token},
		},
		{
			name:   "unconfigured origin",
			origin: "https://other.example.com",
			want:   registry.Credential{Scheme: registry.CredentialAnonymous},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := source.Credential(context.Background(), testCase.origin)
			if err != nil {
				t.Fatalf("Credential(%q) returned error: %v", testCase.origin, err)
			}
			if got != testCase.want {
				t.Fatalf("Credential(%q) = %#v, want %#v", testCase.origin, got, testCase.want)
			}
		})
	}
}

// TestDockerConfigCredentialFileRefusals pins fail-closed loading: a missing
// file, malformed JSON, unusable auth encoding, an entry with no credential and
// two equivalent keys with different credentials are all configuration
// refusals. No refusal renders the configured credential.
func TestDockerConfigCredentialFileRefusals(t *testing.T) {
	const sentinel = "credential-sentinel-do-not-leak"
	cases := []struct {
		name string
		path func(t *testing.T) string
	}{
		{
			name: "missing file",
			path: func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent.json") },
		},
		{
			name: "invalid json",
			path: func(t *testing.T) string { return writeDockerConfig(t, []byte(`{`)) },
		},
		{
			name: "no auths",
			path: func(t *testing.T) string { return writeDockerConfig(t, []byte(`{"credentialHelpers":{}}`)) },
		},
		{
			name: "auths is not an object",
			path: func(t *testing.T) string { return writeDockerConfig(t, []byte(`{"auths":[]}`)) },
		},
		{
			name: "invalid base64 auth",
			path: func(t *testing.T) string {
				return writeDockerConfig(t, dockerConfigBytes(`"registry.example.com":{"auth":"`+sentinel+`"}`))
			},
		},
		{
			name: "auth without separator",
			path: func(t *testing.T) string {
				return writeDockerConfig(t, dockerConfigBytes(
					fmt.Sprintf(`"registry.example.com":{"auth":%q}`, base64.StdEncoding.EncodeToString([]byte(sentinel)))))
			},
		},
		{
			name: "entry without credential",
			path: func(t *testing.T) string {
				return writeDockerConfig(t, dockerConfigBytes(`"registry.example.com":{"email":"operator@example.com"}`))
			},
		},
		{
			name: "empty registry key",
			path: func(t *testing.T) string {
				return writeDockerConfig(t, dockerConfigBytes(
					fmt.Sprintf(`"":{"auth":%q}`, basicCredential(sentinel, sentinel))))
			},
		},
		{
			name: "duplicate registry",
			path: func(t *testing.T) string {
				return writeDockerConfig(t, dockerConfigBytes(
					fmt.Sprintf(`"https://registry.example.com":{"auth":%q},`,
						basicCredential(sentinel, "first"))+
						fmt.Sprintf(`"registry.example.com":{"auth":%q}`, basicCredential(sentinel, "second"))))
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			source, err := registry.LoadDockerConfig(testCase.path(t))
			requireFault(t, err, fault.InvalidInput)
			if source != nil {
				t.Fatal("a refused credential file returned a source")
			}
			requireNoLeak(t, err, sentinel)
		})
	}
}

// TestDockerConfigCredentialFileFeedsResolution proves the loaded file is the
// credential the resolver presents: a private registry read carries Basic
// authentication and the resolved digest is still the immutable body hash.
func TestDockerConfigCredentialFileFeedsResolution(t *testing.T) {
	const (
		user     = "kodama"
		password = "resolution-password-do-not-leak"
	)
	body := singleManifestBody
	source, err := registry.LoadDockerConfig(writeDockerConfig(t, dockerConfigBytes(
		fmt.Sprintf(`"https://%s":{"auth":%q}`, registryOrigin, basicCredential(user, password)))))
	if err != nil {
		t.Fatalf("LoadDockerConfig refused a valid file: %v", err)
	}
	transport := &fakeTransport{handle: func(_ int, request *http.Request) (*http.Response, error) {
		return registryResponse(request, http.StatusOK, dockerManifestMediaType, body, digestOf(body)), nil
	}}
	resolver := newResolver(t, registry.Config{Transport: transport, Credentials: source})

	image, err := resolveWith(t, resolver, tagReference)
	if err != nil {
		t.Fatalf("Resolve refused a credentialed read: %v", err)
	}
	requireResolved(t, image, repository, digestOf(body))

	requests := transport.captured()
	if len(requests) != 1 {
		t.Fatalf("resolution issued %d requests, want exactly 1", len(requests))
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
	if got := requests[0].Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization = %q, want the configured Basic credential", got)
	}
	if strings.Contains(requests[0].URL, password) {
		t.Fatalf("credential leaked into the request URL %q", requests[0].URL)
	}
	if strings.Contains(image.Reference, password) || strings.Contains(image.Digest, password) {
		t.Fatalf("credential leaked into the resolved image %#v", image)
	}
}

// TestDockerConfigCredentialFileEmptyAuthsIsUsable pins that a syntactically
// valid config with an empty auths object is accepted and resolves anonymously;
// only a file that cannot be understood is refused.
func TestDockerConfigCredentialFileEmptyAuthsIsUsable(t *testing.T) {
	source, err := registry.LoadDockerConfig(writeDockerConfig(t, []byte(`{"auths":{}}`)))
	if err != nil {
		t.Fatalf("LoadDockerConfig refused an empty auths object: %v", err)
	}
	credential, err := source.Credential(context.Background(), "https://registry.example.com")
	if err != nil {
		t.Fatalf("Credential returned error: %v", err)
	}
	if credential.Scheme != registry.CredentialAnonymous {
		t.Fatalf("Credential scheme = %q, want anonymous", credential.Scheme)
	}
}

// TestDockerConfigCredentialFileHonorsCancellation pins that a canceled context
// is reported as an error instead of silently returning a credential.
func TestDockerConfigCredentialFileHonorsCancellation(t *testing.T) {
	source, err := registry.LoadDockerConfig(writeDockerConfig(t, dockerConfigBytes(
		fmt.Sprintf(`"registry.example.com":{"auth":%q}`, basicCredential("user", "password")))))
	if err != nil {
		t.Fatalf("LoadDockerConfig refused a valid file: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := source.Credential(ctx, "https://registry.example.com"); err == nil {
		t.Fatal("Credential returned a credential from a canceled context")
	}
}
