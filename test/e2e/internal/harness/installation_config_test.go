//go:build e2e

package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	pinnedToolsImage   = "docker.io/library/buildpack-deps@sha256:712f969c9d81a35f9f00cce701faae521891d42a20b592b61bd0e6c5328fad7c"
	pinnedHarnessImage = "docker.io/library/alpine@sha256:0f49f331d9212aff11b932863be4eb01cdf962969aa86dab7d1ee21ac6f5f52b"
)

// writeInstallationDescriptor writes one JSON installation descriptor.
func writeInstallationDescriptor(t *testing.T, descriptor map[string]any) string {
	t.Helper()
	data, err := json.MarshalIndent(descriptor, "", "  ")
	if err != nil {
		t.Fatalf("encode installation descriptor: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "installation.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write installation descriptor: %v", err)
	}
	return path
}

// installationDescriptor returns one complete, well-formed schema version 1
// descriptor whose values are distinct from the environment defaults so a
// precedence test can tell them apart.
func sampleInstallationDescriptor(t *testing.T) map[string]any {
	t.Helper()
	caPath, _ := testPKI(t)
	personas := map[string]any{}
	for _, persona := range Personas {
		personas[string(persona)] = map[string]any{
			"id":      string(persona),
			"subject": string(persona),
		}
	}
	return map[string]any{
		"schema_version":      1,
		"installation_id":     "installation-w49-test",
		"profile":             "test",
		"runtime":             "podman",
		"revision":            "c3275f5",
		"server_image_digest": "sha256:" + repeat("a", 64),
		"tenant_id":           "tenant-a",
		"project_id":          "tenant-a",
		"api": map[string]any{
			"url":              "https://localhost:18443",
			"ca_file":          caPath,
			"tls_server_name":  "localhost",
			"workload_url":     "https://host.containers.internal:18443",
			"workload_ca_file": caPath,
		},
		"images": map[string]any{
			"tools":            pinnedToolsImage,
			"external_harness": pinnedHarnessImage,
		},
		"oauthsonas": map[string]any{
			"issuer":           "https://localhost:18444",
			"audience":         "https://localhost:18443",
			"ca_file":          caPath,
			"client_id":        "kodactl",
			"redirect_uri":     "http://127.0.0.1:52790/callback",
			"resolve":          "",
			"access_code_file": writeTempFile(t, "oauth-access-code"),
			"personas":         personas,
		},
		"credentials": map[string]any{
			"admin_file":    writeTempFile(t, "admin-token-value"),
			"engineer_file": writeTempFile(t, "engineer-token-value"),
			"reviewer_file": writeTempFile(t, "reviewer-token-value"),
			"operator_file": writeTempFile(t, "operator-token-value"),
		},
		"git": map[string]any{
			"url":                "https://localhost:18445",
			"workload_url":       "https://host.containers.internal:18445",
			"ca_file":            caPath,
			"workload_ca_file":   caPath,
			"service_id":         "git",
			"service_token_file": writeTempFile(t, "git-service-token-value"),
			"admin_binding_id":   "git-admin",
			"admin_resource_id":  "git-admin",
			"repositories": []any{
				map[string]any{"name": "admin", "resource_id": "git-admin", "binding_id": "git-admin"},
			},
		},
	}
}

// expectedPrincipalID mirrors the product's verified human principal
// derivation: oidc- plus the first 32 lowercase hex characters of
// SHA-256(issuer + NUL + subject).
func expectedPrincipalID(issuer, subject string) string {
	sum := sha256.Sum256([]byte(issuer + "\x00" + subject))
	return "oidc-" + hex.EncodeToString(sum[:16])
}

// TestHarnessDescriptorLoadsInstallation proves a schema version 1 descriptor
// alone configures the portable installation connector: endpoints, project,
// images, persona credentials, Git metadata and the installation labels.
func TestHarnessDescriptorLoadsInstallation(t *testing.T) {
	path := writeInstallationDescriptor(t, sampleInstallationDescriptor(t))
	config, err := LoadConfig(envGetter(map[string]string{
		string(InputInstallationFile): path,
	}))
	if err != nil {
		t.Fatalf("descriptor-only configuration was refused: %v", err)
	}
	if config.InstallationFile != path {
		t.Fatalf("installation file = %q; want %q", config.InstallationFile, path)
	}
	if config.APIURL != "https://localhost:18443" || config.ProjectID != "tenant-a" {
		t.Fatalf("descriptor endpoint/project = %q/%q", config.APIURL, config.ProjectID)
	}
	if config.CAPEM == "" || config.WorkloadCAFile == "" {
		t.Fatalf("descriptor CA paths were not loaded: ca=%q workload=%q", config.CAPEM, config.WorkloadCAFile)
	}
	if config.ImageReference() != pinnedToolsImage || config.ExternalHarnessReference() != pinnedHarnessImage {
		t.Fatalf("descriptor images = %q/%q", config.ImageReference(), config.ExternalHarnessReference())
	}
	if config.Token.Reveal() != "admin-token-value" {
		t.Fatal("the descriptor admin credential was not loaded as the legacy basic-test token")
	}
	for _, persona := range Personas {
		secret, ok := config.PersonaToken(persona)
		if !ok || secret.Reveal() != string(persona)+"-token-value" {
			t.Fatalf("descriptor %s credential was not loaded", persona)
		}
	}
	if config.Installation.ID != "installation-w49-test" || config.Installation.Profile != "test" || config.Installation.Runtime != "podman" {
		t.Fatalf("installation labels = %+v", config.Installation)
	}
	if config.Installation.ServerImageDigest != "sha256:"+repeat("a", 64) {
		t.Fatalf("server image digest = %q", config.Installation.ServerImageDigest)
	}
	if config.GitURL() != "https://localhost:18445" || config.GitWorkloadURL() != "https://host.containers.internal:18445" {
		t.Fatalf("descriptor Git endpoints = %q/%q", config.GitURL(), config.GitWorkloadURL())
	}
	repository, ok := config.Repository()
	if !ok || repository.Name != "admin" || repository.ResourceID != "git-admin" {
		t.Fatalf("descriptor repository = %+v ok=%v", repository, ok)
	}
	if config.GitServiceID() != "git" || config.GitServiceTokenFile() == "" {
		t.Fatalf("descriptor Git service metadata missing")
	}
	for persona, view := range config.Installation.OAuth.Personas {
		want := expectedPrincipalID("https://localhost:18444", view.Subject)
		if view.PrincipalID != want {
			t.Fatalf("%s principal = %q; want derived %q", persona, view.PrincipalID, want)
		}
	}
	if missing := config.InstallationMissing(); !missing.Empty() {
		t.Fatalf("complete descriptor reports missing installation input(s): %s", missing)
	}
}

// TestHarnessTaggedDigestPinIsNormalized proves an installation's
// repository:tag@digest pin keeps its digest but reaches admission as the
// repository@digest form the resolver accepts.
func TestHarnessTaggedDigestPinIsNormalized(t *testing.T) {
	digest := "sha256:" + repeat("d", 64)
	config := Config{Image: "docker.io/library/buildpack-deps:trixie-scm@" + digest}
	if got := config.ImageReference(); got != "docker.io/library/buildpack-deps@"+digest {
		t.Fatalf("normalized tools image = %q", got)
	}
	if config.ImageDigest() != digest {
		t.Fatalf("digest = %q; want %q", config.ImageDigest(), digest)
	}
	config = Config{ExternalHarnessImage: "docker.io/library/alpine:3.24@" + digest}
	if got := config.ExternalHarnessReference(); got != "docker.io/library/alpine@"+digest {
		t.Fatalf("normalized harness image = %q", got)
	}
}

// TestHarnessEnvironmentOverridesDescriptor proves every declared explicit
// environment override wins over the descriptor.
func TestHarnessEnvironmentOverridesDescriptor(t *testing.T) {
	path := writeInstallationDescriptor(t, sampleInstallationDescriptor(t))
	otherCA, _ := testPKI(t)
	env := map[string]string{
		string(InputInstallationFile):     path,
		string(InputAPIURL):               "https://localhost:19999",
		string(InputProjectID):            "project-override",
		string(InputTLSServerName):        "api.example.test",
		string(InputWorkloadCAFile):       otherCA,
		string(InputExternalHarnessImage): pinnedHarnessImage,
		string(InputImage):                pinnedToolsImage,
		string(InputAdminTokenFile):       writeTempFile(t, "env-admin-token"),
		string(InputEngineerTokenFile):    writeTempFile(t, "env-engineer-token"),
	}
	config, err := LoadConfig(envGetter(env))
	if err != nil {
		t.Fatalf("environment overrides were refused: %v", err)
	}
	if config.APIURL != "https://localhost:19999" || config.ProjectID != "project-override" || config.TLSServerName != "api.example.test" {
		t.Fatalf("environment did not override the descriptor: %+v", config)
	}
	if config.WorkloadCAFile != otherCA {
		t.Fatalf("workload CA = %q; want the environment override", config.WorkloadCAFile)
	}
	if config.Token.Reveal() != "env-admin-token" || config.PersonaTokenFiles[PersonaAdmin] != env[string(InputAdminTokenFile)] {
		t.Fatal("environment admin token file did not override the descriptor credential")
	}
	if secret, ok := config.PersonaToken(PersonaEngineer); !ok || secret.Reveal() != "env-engineer-token" {
		t.Fatal("environment engineer token file did not override the descriptor credential")
	}
}

// TestHarnessAmbiguousTokenValueAndFileRefused proves a simultaneous legacy
// token value and a file for the same persona is refused before any request.
func TestHarnessAmbiguousTokenValueAndFileRefused(t *testing.T) {
	descriptor := sampleInstallationDescriptor(t)
	descriptor["credentials"].(map[string]any)["admin_file"] = ""
	path := writeInstallationDescriptor(t, descriptor)
	adminFile := writeTempFile(t, "file-admin-token")
	_, err := LoadConfig(envGetter(map[string]string{
		string(InputInstallationFile): path,
		string(InputToken):            "value-admin-token",
		string(InputAdminTokenFile):   adminFile,
	}))
	if err == nil {
		t.Fatal("a simultaneous token value and admin token file was accepted")
	}
	for _, name := range []string{string(InputToken), string(InputAdminTokenFile)} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("ambiguity error %q does not name %s", err, name)
		}
	}
}

// TestHarnessMalformedDescriptorRefused proves a malformed configured
// descriptor is a failure even when no required mode is active, and that the
// error names the descriptor input.
func TestHarnessMalformedDescriptorRefused(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "schema version", mutate: func(d map[string]any) { d["schema_version"] = 2 }},
		{name: "unknown field", mutate: func(d map[string]any) { d["executable"] = "/bin/sh" }},
		{name: "api url scheme", mutate: func(d map[string]any) { d["api"].(map[string]any)["url"] = "http://localhost:18443" }},
		{name: "api ca missing", mutate: func(d map[string]any) {
			d["api"].(map[string]any)["ca_file"] = "/nonexistent/ca.pem"
		}},
		{name: "tools image", mutate: func(d map[string]any) { d["images"].(map[string]any)["tools"] = "bad image" }},

		{name: "project id", mutate: func(d map[string]any) { d["project_id"] = "../../etc" }},
		{name: "git userinfo", mutate: func(d map[string]any) {
			d["git"].(map[string]any)["url"] = "https://user:pass@localhost:18445"
		}},
		{name: "credential file", mutate: func(d map[string]any) {
			d["credentials"].(map[string]any)["admin_file"] = "/nonexistent/admin-token"
		}},
		{name: "principal mismatch", mutate: func(d map[string]any) {
			personas := d["oauthsonas"].(map[string]any)["personas"].(map[string]any)
			personas["engineer"] = map[string]any{"id": "engineer", "subject": "engineer", "principal_id": "oidc-" + repeat("0", 32)}
		}},
		{name: "case variant root key", mutate: func(d map[string]any) {
			d["Schema_Version"] = d["schema_version"]
			delete(d, "schema_version")
		}},
		{name: "case variant api key", mutate: func(d map[string]any) {
			api := d["api"].(map[string]any)
			api["URL"] = api["url"]
			delete(api, "url")
		}},
		{name: "case variant persona key", mutate: func(d map[string]any) {
			personas := d["oauthsonas"].(map[string]any)["personas"].(map[string]any)
			persona := personas["engineer"].(map[string]any)
			persona["Subject"] = persona["subject"]
			delete(persona, "subject")
		}},
		{name: "case variant repository key", mutate: func(d map[string]any) {
			repositories := d["git"].(map[string]any)["repositories"].([]any)
			repository := repositories[0].(map[string]any)
			repository["Name"] = repository["name"]
			delete(repository, "name")
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			descriptor := sampleInstallationDescriptor(t)
			testCase.mutate(descriptor)
			path := writeInstallationDescriptor(t, descriptor)
			_, err := LoadConfig(envGetter(map[string]string{string(InputInstallationFile): path}))
			if err == nil {
				t.Fatal("malformed descriptor was accepted")
			}
			if !strings.Contains(err.Error(), string(InputInstallationFile)) {
				t.Fatalf("malformed descriptor error %q does not name the input", err)
			}
		})
	}
}

// TestHarnessRequireInstallationModes proves Q01_REQUIRE_INSTALLATION=1 is a
// distinct required mode and is never implied by another value.
func TestHarnessRequireInstallationModes(t *testing.T) {
	t.Setenv(RequireInstallationEnv, "")
	if RequireInstallation() {
		t.Fatal("required installation mode is active without the variable")
	}
	t.Setenv(RequireInstallationEnv, "1")
	if !RequireInstallation() {
		t.Fatal("Q01_REQUIRE_INSTALLATION=1 was ignored")
	}
	t.Setenv(RequireInstallationEnv, "true")
	if RequireInstallation() {
		t.Fatal("a non-1 required installation value was treated as required")
	}
}

// TestHarnessInstallationMissingNamesRequiredInputs proves the preflight's
// required inputs are named individually instead of becoming one opaque skip.
func TestHarnessInstallationMissingNamesRequiredInputs(t *testing.T) {
	var config Config
	missing := config.InstallationMissing()
	if missing.Empty() {
		t.Fatal("an unconfigured installation reported no missing inputs")
	}
	reason := missing.String()
	for _, name := range []string{
		"Q01_API_URL", "Q01_CA_PEM", "Q01_PROJECT_ID",
		"Q01_ADMIN_TOKEN_FILE", "Q01_ENGINEER_TOKEN_FILE",
		"Q01_REVIEWER_TOKEN_FILE", "Q01_OPERATOR_TOKEN_FILE",
		"Q01_WORKLOAD_CA_FILE", "Q01_EXTERNAL_HARNESS_IMAGE",
		"git.url", "git.repositories", "image digest",
	} {
		if !strings.Contains(reason, name) {
			t.Errorf("missing installation reason %q does not name %s", reason, name)
		}
	}
}

// TestHarnessDescriptorRepositorySelection proves smoke_repository wins over
// the first installation repository when the setup wrapper provisions one.
func TestHarnessDescriptorRepositorySelection(t *testing.T) {
	descriptor := sampleInstallationDescriptor(t)
	descriptor["smoke_repository"] = map[string]any{
		"name": "smoke/run-1", "resource_id": "w49-repo-run-1", "binding_id": "w49-bind-run-1",
	}
	path := writeInstallationDescriptor(t, descriptor)
	config, err := LoadConfig(envGetter(map[string]string{string(InputInstallationFile): path}))
	if err != nil {
		t.Fatalf("descriptor with smoke_repository refused: %v", err)
	}
	repository, ok := config.Repository()
	if !ok || repository.Name != "smoke/run-1" || repository.ResourceID != "w49-repo-run-1" {
		t.Fatalf("selected repository = %+v ok=%v", repository, ok)
	}
}

// TestHarnessTokenFilesMustBeReadable proves a configured credential file that
// cannot be read is a named failure, not a silent empty credential.
func TestHarnessTokenFilesMustBeReadable(t *testing.T) {
	descriptor := sampleInstallationDescriptor(t)
	descriptor["credentials"].(map[string]any)["reviewer_file"] = "/nonexistent/reviewer-token"
	path := writeInstallationDescriptor(t, descriptor)
	_, err := LoadConfig(envGetter(map[string]string{string(InputInstallationFile): path}))
	if err == nil || !strings.Contains(err.Error(), "reviewer") {
		t.Fatalf("unreadable reviewer credential error = %v", err)
	}
}

// TestHarnessDescriptorIsBounded proves an oversized descriptor is refused
// instead of read without a bound.
func TestHarnessDescriptorIsBounded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "installation.json")
	if err := os.WriteFile(path, make([]byte, 2<<20), 0o600); err != nil {
		t.Fatalf("write oversized descriptor: %v", err)
	}
	if _, err := LoadConfig(envGetter(map[string]string{string(InputInstallationFile): path})); err == nil {
		t.Fatal("an oversized descriptor was accepted")
	}
}

// TestHarnessRepositorySlots proves the Git scenarios spread over the ordinary
// repositories in a fixed order that never selects the admin repository, wrap
// around past the last one, and all share the one repository of an
// installation that has no other.
func TestHarnessRepositorySlots(t *testing.T) {
	descriptor := sampleInstallationDescriptor(t)
	git := descriptor["git"].(map[string]any)
	admin := git["admin_resource_id"]
	git["repositories"] = []map[string]any{
		{"name": "admin", "resource_id": admin, "binding_id": admin},
		{"name": "smoke/two", "resource_id": "repo-2", "binding_id": "bind-2"},
		{"name": "smoke/one", "resource_id": "repo-1", "binding_id": "bind-1"},
		{"name": "smoke/three", "resource_id": "repo-3", "binding_id": "bind-3"},
	}
	descriptor["smoke_repository"] = map[string]any{
		"name": "smoke/one", "resource_id": "repo-1", "binding_id": "bind-1",
	}
	config, err := LoadConfig(envGetter(map[string]string{
		string(InputInstallationFile): writeInstallationDescriptor(t, descriptor),
	}))
	if err != nil {
		t.Fatalf("descriptor with three ordinary repositories refused: %v", err)
	}
	for slot, want := range []string{"repo-1", "repo-2", "repo-3", "repo-1"} {
		if got, ok := config.RepositoryForSlot(slot); !ok || got.ResourceID != want {
			t.Errorf("RepositoryForSlot(%d) = %+v, %v; want %s", slot, got, ok, want)
		}
	}

	single := sampleInstallationDescriptor(t)
	config, err = LoadConfig(envGetter(map[string]string{
		string(InputInstallationFile): writeInstallationDescriptor(t, single),
	}))
	if err != nil {
		t.Fatalf("sample descriptor refused: %v", err)
	}
	want, ok := config.Repository()
	if !ok {
		t.Fatal("the sample descriptor selects no repository")
	}
	for slot := range 4 {
		if got, ok := config.RepositoryForSlot(slot); !ok || got != want {
			t.Errorf("RepositoryForSlot(%d) = %+v, %v; want the installation's one repository %+v", slot, got, ok, want)
		}
	}
}
