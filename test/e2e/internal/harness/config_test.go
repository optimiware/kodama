//go:build e2e

package harness

import (
	"fmt"
	"strings"
	"testing"
)

// TestHarnessMissingInputsAreNamed proves that an unconfigured harness reports
// every absent input by its environment name, and never counts absence as a
// pass. A live test uses the same Config.Missing result to skip with the names
// in the skip reason.
func TestHarnessMissingInputsAreNamed(t *testing.T) {
	config, err := LoadConfig(func(string) string { return "" })
	if err != nil {
		t.Fatalf("an unconfigured environment must not be malformed: %v", err)
	}

	missing := config.Missing(InputAPIURL, InputCAPEM, InputToken, InputProjectID)
	if missing.Empty() {
		t.Fatal("an unconfigured environment reported no missing inputs")
	}
	reason := missing.String()
	for _, name := range []string{"Q01_API_URL", "Q01_CA_PEM", "Q01_TOKEN", "Q01_PROJECT_ID"} {
		if !strings.Contains(reason, name) {
			t.Errorf("missing-input reason %q does not name %s", reason, name)
		}
	}
	if strings.Contains(reason, "Q01_EVIDENCE_DIR") {
		t.Errorf("missing-input reason %q names an input the caller did not require", reason)
	}

	config.APIURL = "https://127.0.0.1:18443"
	config.CAPEM = "/tmp/ca.pem"
	config.Token = Secret("project-token-value")
	config.ProjectID = "tenant-a"
	if still := config.Missing(InputAPIURL, InputCAPEM, InputToken, InputProjectID); !still.Empty() {
		t.Fatalf("configured inputs still reported missing: %s", still)
	}
	if reason := config.Missing(InputEvidenceDir).String(); !strings.Contains(reason, "Q01_EVIDENCE_DIR") {
		t.Fatalf("unconfigured evidence reason %q does not name Q01_EVIDENCE_DIR", reason)
	}
}

// TestHarnessNATSURLIsOptionalAndValidated pins the live-subscription broker
// input: an absent value stays empty (a named skip for tests that need it),
// while a malformed or credential-bearing value fails with the input name.
func TestHarnessNATSURLIsOptionalAndValidated(t *testing.T) {
	config, err := LoadConfig(func(string) string { return "" })
	if err != nil {
		t.Fatalf("an unconfigured broker input must not be malformed: %v", err)
	}
	if config.NATSURL != "" {
		t.Fatalf("unconfigured NATS URL = %q; want empty", config.NATSURL)
	}

	for _, valid := range []string{"nats://127.0.0.1:20222", "tls://nats.kodama-system.svc:4222"} {
		env := validConfigEnv(t)
		env[string(InputNATSURL)] = valid
		loaded, err := LoadConfig(envGetter(env))
		if err != nil {
			t.Fatalf("valid NATS URL %q was refused: %v", valid, err)
		}
		if loaded.NATSURL != valid {
			t.Fatalf("NATS URL = %q; want %q", loaded.NATSURL, valid)
		}
	}

	for _, invalid := range []string{
		"http://127.0.0.1:20222",
		"nats://127.0.0.1",
		"nats://user:pass@127.0.0.1:20222",
		"nats://127.0.0.1:20222/extra",
		"nats://127.0.0.1:20222?token=value",
	} {
		env := validConfigEnv(t)
		env[string(InputNATSURL)] = invalid
		if _, err := LoadConfig(envGetter(env)); err == nil {
			t.Fatalf("invalid NATS URL %q was accepted", invalid)
		} else if !strings.Contains(err.Error(), string(InputNATSURL)) {
			t.Fatalf("invalid NATS URL error %q does not name %s", err, InputNATSURL)
		}
	}
}

// TestHarnessCompleteConfigLoads pins the accepted shape and proves the token
// value never appears in the ordinary config renderings.
func TestHarnessCompleteConfigLoads(t *testing.T) {
	env := validConfigEnv(t)
	config, err := LoadConfig(envGetter(env))
	if err != nil {
		t.Fatalf("complete configuration refused: %v", err)
	}
	if config.Token.Reveal() != env[string(InputToken)] {
		t.Fatal("the project token was not loaded")
	}
	for _, rendered := range []string{config.String(), fmt.Sprintf("%#v", config)} {
		if strings.Contains(rendered, env[string(InputToken)]) {
			t.Fatalf("config rendering leaked the token: %s", rendered)
		}
	}
	if config.ImageReference() != env[string(InputImage)] {
		t.Fatalf("image reference = %q; want the configured image", config.ImageReference())
	}
	legacy := []Input{
		InputAPIURL, InputCAPEM, InputTLSServerName, InputToken, InputProjectID,
		InputKubeContext, InputImage, InputEvidenceDir, InputHarnessRevision, InputDeployedRevision,
	}
	if missing := config.Missing(legacy...); !missing.Empty() {
		t.Fatalf("complete configuration reports missing inputs: %s", missing)
	}
}

// TestHarnessImageDefaults proves an unconfigured Q01_IMAGE falls back to a
// digest-pinned default instead of the mutable stage tag, and that a digest is
// reported only when the reference carries one.
func TestHarnessImageDefaults(t *testing.T) {
	config := Config{}
	if config.ImageReference() != DefaultImage {
		t.Fatalf("default image = %q; want %q", config.ImageReference(), DefaultImage)
	}
	if config.ImageDigest() == "" {
		t.Fatal("the default image is not digest-pinned")
	}
	digest := "sha256:" + repeat("a", 64)
	config.Image = "rg.example.test/kodama@" + digest
	if config.ImageDigest() != digest {
		t.Fatalf("pinned image digest = %q; want %q", config.ImageDigest(), digest)
	}
	config.Image = "rg.example.test/kodama:latest"
	if config.ImageDigest() != "" {
		t.Fatalf("tag-referenced image reported digest %q", config.ImageDigest())
	}
}

// TestHarnessMalformedConfigFails proves every configured-but-invalid input is
// a failure, not a skip, and that the failure names the input.
func TestHarnessMalformedConfigFails(t *testing.T) {
	cases := []struct {
		name   string
		input  Input
		mutate func(map[string]string)
	}{
		{
			name:  "api url must use https",
			input: InputAPIURL,
			mutate: func(env map[string]string) {
				env[string(InputAPIURL)] = "http://127.0.0.1:18443"
			},
		},
		{
			name:  "api url must not carry credentials",
			input: InputAPIURL,
			mutate: func(env map[string]string) {
				env[string(InputAPIURL)] = "https://user:password@127.0.0.1:18443"
			},
		},
		{
			name:  "api url must be absolute",
			input: InputAPIURL,
			mutate: func(env map[string]string) {
				env[string(InputAPIURL)] = "/api/v1"
			},
		},
		{
			name:  "tls server name must be a dns name",
			input: InputTLSServerName,
			mutate: func(env map[string]string) {
				env[string(InputTLSServerName)] = "kodama.test:8443"
			},
		},
		{
			name:  "a configured ca path must exist",
			input: InputCAPEM,
			mutate: func(env map[string]string) {
				env[string(InputCAPEM)] = env[string(InputCAPEM)] + ".missing"
			},
		},
		{
			name:  "a ca file must contain a certificate",
			input: InputCAPEM,
			mutate: func(env map[string]string) {
				env[string(InputCAPEM)] = writeTempFile(t, "not a certificate\n")
			},
		},
		{
			name:  "a token must not contain whitespace",
			input: InputToken,
			mutate: func(env map[string]string) {
				env[string(InputToken)] = "project token"
			},
		},
		{
			name:  "a project id must be bounded ascii",
			input: InputProjectID,
			mutate: func(env map[string]string) {
				env[string(InputProjectID)] = "../../etc"
			},
		},
		{
			name:  "a kube context must not look like a flag",
			input: InputKubeContext,
			mutate: func(env map[string]string) {
				env[string(InputKubeContext)] = "--namespace=other"
			},
		},
		{
			name:  "an image must be a bounded reference",
			input: InputImage,
			mutate: func(env map[string]string) {
				env[string(InputImage)] = "node image with spaces"
			},
		},
		{
			name:  "an evidence directory must be absolute",
			input: InputEvidenceDir,
			mutate: func(env map[string]string) {
				env[string(InputEvidenceDir)] = "evidence/Q01"
			},
		},
		{
			name:  "a harness revision must be a git revision",
			input: InputHarnessRevision,
			mutate: func(env map[string]string) {
				env[string(InputHarnessRevision)] = "working-tree"
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			env := validConfigEnv(t)
			testCase.mutate(env)
			_, err := LoadConfig(envGetter(env))
			if err == nil {
				t.Fatalf("malformed %s was accepted", testCase.input)
			}
			if !strings.Contains(err.Error(), string(testCase.input)) {
				t.Fatalf("malformed %s error %q does not name the input", testCase.input, err)
			}
		})
	}
}

// TestHarnessRequireStageModes proves both require-stage spellings turn an
// unconfigured run into a failure and that anything else keeps the named skip.
func TestHarnessRequireStageModes(t *testing.T) {
	t.Setenv(RequireStageEnv, "")
	t.Setenv(RequireStageEnvCompat, "")
	if RequireStage() {
		t.Fatal("require-stage is active without the variable")
	}
	t.Setenv(RequireStageEnv, "1")
	if !RequireStage() {
		t.Fatal("Q01_REQUIRE_STAGE=1 was ignored")
	}
	t.Setenv(RequireStageEnv, "")
	t.Setenv(RequireStageEnvCompat, "1")
	if !RequireStage() {
		t.Fatal("Q02_REQUIRE_STAGE=1 compatibility was ignored")
	}
	t.Setenv(RequireStageEnvCompat, "true")
	if RequireStage() {
		t.Fatal("a non-1 require-stage value was treated as required")
	}
}

// TestHarnessSmokeGitSwitch proves the release-smoke Git switch is on by
// default, can be turned off explicitly, and rejects any other value instead of
// guessing.
func TestHarnessSmokeGitSwitch(t *testing.T) {
	if !(Config{}).GitSmokeEnabled() {
		t.Fatal("the zero configuration disabled the Git smoke scenarios; the default must be on")
	}

	env := validConfigEnv(t)
	env[string(InputSmokeGit)] = "0"
	config, err := LoadConfig(envGetter(env))
	if err != nil {
		t.Fatalf("Q01_SMOKE_GIT=0 was refused: %v", err)
	}
	if config.GitSmokeEnabled() {
		t.Fatal("Q01_SMOKE_GIT=0 did not disable the Git smoke scenarios")
	}

	env[string(InputSmokeGit)] = "1"
	config, err = LoadConfig(envGetter(env))
	if err != nil {
		t.Fatalf("Q01_SMOKE_GIT=1 was refused: %v", err)
	}
	if !config.GitSmokeEnabled() {
		t.Fatal("Q01_SMOKE_GIT=1 did not enable the Git smoke scenarios")
	}

	for _, bogus := range []string{"yes", "true", "2", "off"} {
		env[string(InputSmokeGit)] = bogus
		if _, err := LoadConfig(envGetter(env)); err == nil {
			t.Fatalf("bogus %s=%q was accepted", InputSmokeGit, bogus)
		} else if !strings.Contains(err.Error(), string(InputSmokeGit)) {
			t.Fatalf("bogus switch error %q does not name %s", err, InputSmokeGit)
		}
	}
}

// TestHarnessOptionalEvidenceInputs proves the evidence writer and revision
// inputs are optional, while a present-but-empty value is simply unconfigured.
func TestHarnessOptionalEvidenceInputs(t *testing.T) {
	env := validConfigEnv(t)
	delete(env, string(InputEvidenceDir))
	delete(env, string(InputHarnessRevision))
	config, err := LoadConfig(envGetter(env))
	if err != nil {
		t.Fatalf("optional inputs refused: %v", err)
	}
	if config.Missing(InputEvidenceDir).Empty() {
		t.Fatal("an absent evidence directory was not reported as missing")
	}
	if config.EvidenceDir != "" {
		t.Fatalf("an absent evidence directory fabricated %q", config.EvidenceDir)
	}
	if config.ImageReference() != env[string(InputImage)] {
		t.Fatalf("a configured image was not kept: %q", config.ImageReference())
	}
}
