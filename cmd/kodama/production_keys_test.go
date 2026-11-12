package main

import (
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/shell/integration/keys"
)

const (
	testAWSKeyARN   = "aws-kms:arn:aws:kms:eu-west-1:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab"
	testAWSKeyAlias = "aws-kms:alias/kodama"
	testAWSRoleARN  = "arn:aws:iam::123456789012:role/kodama-kms"
	testAWSToken    = "/var/run/secrets/eks.amazonaws.com/serviceaccount/token"
	testGCPKey      = "gcp-kms:projects/kodama/locations/europe-west1/keyRings/kodama/cryptoKeys/kek"
)

// productionKeyEnv is the production fixture with the Scaleway credential
// removed and the given settings applied, so a case states exactly what its
// adapter reads.
func productionKeyEnv(t *testing.T, settings map[string]string) (productionEnv, func() (productionBuild, error)) {
	t.Helper()
	getenv, kube, options := productionFixture(t)
	override := func(name string) string {
		if value, ok := settings[name]; ok {
			return value
		}
		if name == "SCW_SECRET_KEY" {
			return ""
		}
		return getenv(name)
	}
	return override, func() (productionBuild, error) { return buildProduction(options, override, kube) }
}

// TestBuildProductionSelectsTheKeyAdapter pins that the production entry builds
// the KMS client of the adapter KODAMA_KEY_ADAPTER names, each from its own
// credential mode, and the static Scaleway credential is required only for
// scw-kms. No client contacts its KMS at construction.
func TestBuildProductionSelectsTheKeyAdapter(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings map[string]string
		adapter  string
		isClient func(keys.KMSClient) bool
	}{
		{
			name: "aws key ARN names its own region",
			settings: map[string]string{
				"KODAMA_KEY_ADAPTER": "aws-kms", "KODAMA_KEY_REF": testAWSKeyARN,
				"AWS_ROLE_ARN": testAWSRoleARN, "AWS_WEB_IDENTITY_TOKEN_FILE": testAWSToken,
			},
			adapter:  "aws-kms",
			isClient: func(client keys.KMSClient) bool { _, ok := client.(*keys.AWSKMS); return ok },
		},
		{
			name: "aws key ARN with the matching injected region",
			settings: map[string]string{
				"KODAMA_KEY_ADAPTER": "aws-kms", "KODAMA_KEY_REF": testAWSKeyARN, "AWS_REGION": "eu-west-1",
				"AWS_ROLE_ARN": testAWSRoleARN, "AWS_WEB_IDENTITY_TOKEN_FILE": testAWSToken,
			},
			adapter:  "aws-kms",
			isClient: func(client keys.KMSClient) bool { _, ok := client.(*keys.AWSKMS); return ok },
		},
		{
			name: "aws alias with the injected region",
			settings: map[string]string{
				"KODAMA_KEY_ADAPTER": "aws-kms", "KODAMA_KEY_REF": testAWSKeyAlias, "AWS_REGION": "eu-west-1",
				"AWS_ROLE_ARN": testAWSRoleARN, "AWS_WEB_IDENTITY_TOKEN_FILE": testAWSToken,
			},
			adapter:  "aws-kms",
			isClient: func(client keys.KMSClient) bool { _, ok := client.(*keys.AWSKMS); return ok },
		},
		{
			name:     "gcp reads no credential setting",
			settings: map[string]string{"KODAMA_KEY_ADAPTER": "gcp-kms", "KODAMA_KEY_REF": testGCPKey},
			adapter:  "gcp-kms",
			isClient: func(client keys.KMSClient) bool { _, ok := client.(*keys.GCPKMS); return ok },
		},
		{
			name:     "scaleway reads the static credential",
			settings: map[string]string{"SCW_SECRET_KEY": "k"},
			adapter:  "scw-kms",
			isClient: func(client keys.KMSClient) bool { _, ok := client.(*keys.ScalewayKMS); return ok },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			getenv, build := productionKeyEnv(t, tc.settings)
			built, err := build()
			if err != nil {
				t.Fatalf("buildProduction: %v", err)
			}
			config := built.Config
			if string(config.Installation.Keys) != tc.adapter {
				t.Fatalf("installation key adapter = %q, want %q", config.Installation.Keys, tc.adapter)
			}
			if config.Installation.KeyRef != getenv("KODAMA_KEY_REF") || string(config.Production.Cloud.KeyRef) != getenv("KODAMA_KEY_REF") {
				t.Fatalf("key references = %q and %q, want the configured reference",
					config.Installation.KeyRef, config.Production.Cloud.KeyRef)
			}
			if !tc.isClient(config.Production.KMS) {
				t.Fatalf("KMS client = %T; want the client of %s", config.Production.KMS, tc.adapter)
			}

			// The one-shot bootstrap shares the loader, so it selects the same
			// adapter from the same environment.
			keyset, err := loadProductionKeys(getenv)
			if err != nil {
				t.Fatalf("loadProductionKeys: %v", err)
			}
			if !tc.isClient(keyset.kms) || keyset.provider == nil {
				t.Fatalf("bootstrap keys = %T with provider %v; want the client of %s", keyset.kms, keyset.provider, tc.adapter)
			}
		})
	}
}

// TestBuildProductionRefusesIncompleteKeyAdapterSettings pins the per-adapter
// refusals. Every one is a configuration error before any dependency is
// contacted, and it names the missing setting, never a value.
func TestBuildProductionRefusesIncompleteKeyAdapterSettings(t *testing.T) {
	aws := func(overrides map[string]string) map[string]string {
		settings := map[string]string{
			"KODAMA_KEY_ADAPTER": "aws-kms", "KODAMA_KEY_REF": testAWSKeyARN,
			"AWS_ROLE_ARN": testAWSRoleARN, "AWS_WEB_IDENTITY_TOKEN_FILE": testAWSToken,
		}
		for name, value := range overrides {
			settings[name] = value
		}
		return settings
	}
	for _, tc := range []struct {
		name     string
		settings map[string]string
		want     string
	}{
		{"scaleway without its credential", map[string]string{}, "SCW_SECRET_KEY"},
		{"aws without the role", aws(map[string]string{"AWS_ROLE_ARN": ""}), "AWS_ROLE_ARN"},
		{"aws without the token file", aws(map[string]string{"AWS_WEB_IDENTITY_TOKEN_FILE": ""}), "AWS_WEB_IDENTITY_TOKEN_FILE"},
		{"aws alias without a region", aws(map[string]string{"KODAMA_KEY_REF": testAWSKeyAlias}), "AWS_REGION"},
		{"aws region disagrees with the key ARN", aws(map[string]string{"AWS_REGION": "us-east-1"}), "region"},
		{"aws with a malformed role", aws(map[string]string{"AWS_ROLE_ARN": "not-a-role"}), "role"},
		{"aws adapter with a scaleway reference", aws(map[string]string{"KODAMA_KEY_REF": "scw-kms:fr-par/key"}), "key reference"},
		{"aws adapter with a gcp reference", aws(map[string]string{"KODAMA_KEY_REF": testGCPKey}), "key reference"},
		{"gcp adapter with an aws reference", map[string]string{"KODAMA_KEY_ADAPTER": "gcp-kms", "KODAMA_KEY_REF": testAWSKeyARN}, "key reference"},
		{"gcp adapter with a malformed reference", map[string]string{"KODAMA_KEY_ADAPTER": "gcp-kms", "KODAMA_KEY_REF": "gcp-kms:projects/kodama"}, "key reference"},
		{"keyfile adapter", map[string]string{"KODAMA_KEY_ADAPTER": "keyfile"}, "aws-kms"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			getenv, build := productionKeyEnv(t, tc.settings)
			if _, err := build(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("buildProduction error = %v, want it to mention %q", err, tc.want)
			}
			_, err := loadProductionKeys(getenv)
			if err == nil {
				t.Fatal("loadProductionKeys accepted the same incomplete settings")
			}
			for _, value := range []string{testAWSRoleARN, testAWSToken} {
				if strings.Contains(err.Error(), value) {
					t.Fatalf("refusal %q renders a configured value", err)
				}
			}
		})
	}
}
