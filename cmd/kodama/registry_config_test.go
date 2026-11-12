package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/integration/registry"
)

// writeProductionDockerConfig writes one docker config.json credential file and
// returns its path.
func writeProductionDockerConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write registry credential file: %v", err)
	}
	return path
}

// TestBuildProductionWiresRegistryCredentialFile proves the production entry
// reads KODAMA_REGISTRY_CONFIG_FILE and installs it as the registry credential
// source the immutable image resolution consumes. The credential is a name-free
// Basic pair here; the source returns it by origin.
func TestBuildProductionWiresRegistryCredentialFile(t *testing.T) {
	getenv, kube, options := productionFixture(t)
	const (
		user     = "kodama"
		password = "production-registry-password-do-not-leak"
	)
	path := writeProductionDockerConfig(t, fmt.Sprintf(
		`{"auths":{"https://registry.example.com":{"auth":%q}}}`,
		base64.StdEncoding.EncodeToString([]byte(user+":"+password))))
	env := func(name string) string {
		if name == "KODAMA_REGISTRY_CONFIG_FILE" {
			return path
		}
		return getenv(name)
	}

	build, err := buildProduction(options, env, kube)
	if err != nil {
		t.Fatalf("buildProduction refused a valid registry credential file: %v", err)
	}
	source := build.Config.Production.Registry.Credentials
	if source == nil {
		t.Fatal("production composition has no registry credential source")
	}
	credential, err := source.Credential(context.Background(), "https://registry.example.com")
	if err != nil {
		t.Fatalf("configured registry credential source failed: %v", err)
	}
	if credential.Scheme != registry.CredentialBasic ||
		credential.Username != user || credential.Password != password {
		t.Fatalf("configured registry credential = %#v; want the file's Basic pair", credential)
	}
}

// TestBuildProductionWithoutRegistryCredentialFileIsAnonymous keeps the public
// registry default: an installation that sets no credential file resolves
// anonymously instead of failing startup.
func TestBuildProductionWithoutRegistryCredentialFileIsAnonymous(t *testing.T) {
	getenv, kube, options := productionFixture(t)
	build, err := buildProduction(options, getenv, kube)
	if err != nil {
		t.Fatalf("buildProduction: %v", err)
	}
	if build.Config.Production.Registry.Credentials != nil {
		t.Fatal("production composition has a credential source without a configured file")
	}
}

// TestBuildProductionRefusesRegistryCredentialFile pins fail-closed entry
// configuration: a configured file that is missing or malformed is refused by
// name without rendering a configured credential.
func TestBuildProductionRefusesRegistryCredentialFile(t *testing.T) {
	const sentinel = "entry-credential-sentinel-do-not-leak"
	cases := []struct {
		name string
		path func(t *testing.T) string
	}{
		{
			name: "missing",
			path: func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent.json") },
		},
		{
			name: "malformed json",
			path: func(t *testing.T) string { return writeProductionDockerConfig(t, `{`) },
		},
		{
			name: "malformed credential",
			path: func(t *testing.T) string {
				return writeProductionDockerConfig(t, `{"auths":{"registry.example.com":{"auth":"`+sentinel+`"}}}`)
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			getenv, kube, options := productionFixture(t)
			path := testCase.path(t)
			env := func(name string) string {
				if name == "KODAMA_REGISTRY_CONFIG_FILE" {
					return path
				}
				return getenv(name)
			}
			build, err := buildProduction(options, env, kube)
			if err == nil {
				t.Fatal("buildProduction accepted an unusable registry credential file")
			}
			if !strings.Contains(err.Error(), "KODAMA_REGISTRY_CONFIG_FILE") {
				t.Fatalf("refusal does not name KODAMA_REGISTRY_CONFIG_FILE: %v", err)
			}
			if strings.Contains(err.Error(), sentinel) {
				t.Fatalf("refusal renders the configured credential: %v", err)
			}
			if _, ok := fault.ClassOf(err); !ok {
				t.Logf("refusal keeps the production configuration text: %v", err)
			}
			if build.Config.Production != nil {
				t.Fatal("a refused build returned a production composition")
			}
		})
	}
}

// TestBuildProductionMapsWorkloadImagePullSecrets proves the optional
// execution-pod pull secret list reaches the tenant adapter: the entry accepts a
// valid list and refuses an unusable Secret reference at adapter construction,
// which is the only way an invalid name can fail startup.
func TestBuildProductionMapsWorkloadImagePullSecrets(t *testing.T) {
	getenv, kube, options := productionFixture(t)
	withSecrets := func(value string) productionEnv {
		return func(name string) string {
			if name == "KODAMA_WORKLOAD_IMAGE_PULL_SECRETS" {
				return value
			}
			return getenv(name)
		}
	}
	if _, err := buildProduction(options, withSecrets("kodama-registry, backup-registry"), kube); err != nil {
		t.Fatalf("buildProduction refused a valid image pull secret list: %v", err)
	}
	_, err := buildProduction(options, withSecrets("Bad_Registry"), kube)
	if err == nil {
		t.Fatal("buildProduction accepted an invalid image pull secret name")
	}
	if strings.Contains(err.Error(), "Bad_Registry") {
		t.Fatalf("refusal renders the configured Secret name: %v", err)
	}
}
