//go:build qualification

package production

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestHarnessMissingInputsAreNamed proves that an unconfigured harness reports
// every absent input by its environment name, and never counts absence as a
// pass. The stage probe uses the same Config.Missing result to skip with the
// names in the skip reason.
func TestHarnessMissingInputsAreNamed(t *testing.T) {
	config, err := LoadConfig(func(string) string { return "" })
	if err != nil {
		t.Fatalf("an unconfigured environment must not be malformed: %v", err)
	}

	missing := config.Missing(InputAPIURL, InputCAPEM, InputAdminToken, InputProjectID)
	if missing.Empty() {
		t.Fatal("an unconfigured environment reported no missing inputs")
	}
	reason := missing.String()
	for _, name := range []string{"Q02_API_URL", "Q02_CA_PEM", "Q02_ADMIN_TOKEN", "Q02_PROJECT_ID"} {
		if !strings.Contains(reason, name) {
			t.Errorf("missing-input reason %q does not name %s", reason, name)
		}
	}
	if strings.Contains(reason, "Q02_DEVELOPER_TOKEN") {
		t.Errorf("missing-input reason %q names an input the caller did not require", reason)
	}

	config.APIURL = "https://127.0.0.1:18443"
	config.CAPEM = "/tmp/ca.pem"
	config.AdminToken = Secret("admin-token-value")
	config.ProjectID = "tenant-a"
	if still := config.Missing(InputAPIURL, InputCAPEM, InputAdminToken, InputProjectID); !still.Empty() {
		t.Fatalf("configured inputs still reported missing: %s", still)
	}
	if reason := config.Missing(InputDeveloperToken).String(); !strings.Contains(reason, "Q02_DEVELOPER_TOKEN") {
		t.Fatalf("unconfigured developer token reason %q does not name Q02_DEVELOPER_TOKEN", reason)
	}
}

// TestHarnessCompleteConfigLoads pins the accepted shape and proves the token
// values never appear in the ordinary config renderings.
func TestHarnessCompleteConfigLoads(t *testing.T) {
	env := validConfigEnv(t)
	config, err := LoadConfig(envGetter(env))
	if err != nil {
		t.Fatalf("complete configuration refused: %v", err)
	}
	if config.AdminToken.Reveal() != env[string(InputAdminToken)] {
		t.Fatal("the admin token was not loaded")
	}
	if config.DeveloperToken.Reveal() != env[string(InputDeveloperToken)] {
		t.Fatal("the developer token was not loaded")
	}
	if config.SecondAdminToken.Reveal() != env[string(InputSecondAdminToken)] {
		t.Fatal("the second tenant admin token was not loaded")
	}
	for _, rendered := range []string{config.String(), fmt.Sprintf("%#v", config)} {
		for _, token := range []string{env[string(InputAdminToken)], env[string(InputDeveloperToken)], env[string(InputSecondAdminToken)]} {
			if strings.Contains(rendered, token) {
				t.Fatalf("config rendering leaked a token: %s", rendered)
			}
		}
	}
	if missing := config.Missing(Inputs...); !missing.Empty() {
		t.Fatalf("complete configuration reports missing inputs: %s", missing)
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
			input: InputAdminToken,
			mutate: func(env map[string]string) {
				env[string(InputAdminToken)] = "admin token"
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
			name:  "a tenant namespace must be a dns label",
			input: InputTenantNamespace,
			mutate: func(env map[string]string) {
				env[string(InputTenantNamespace)] = "Not_A_Namespace"
			},
		},
		{
			name:  "a tenant namespace cannot be kube-system",
			input: InputTenantNamespace,
			mutate: func(env map[string]string) {
				env[string(InputTenantNamespace)] = "kube-system"
			},
		},
		{
			name:  "a tenant namespace cannot be default",
			input: InputTenantNamespace,
			mutate: func(env map[string]string) {
				env[string(InputTenantNamespace)] = "default"
			},
		},
		{
			name:  "a tenant namespace cannot duplicate the release namespace",
			input: InputTenantNamespace,
			mutate: func(env map[string]string) {
				env[string(InputTenantNamespace)] = ReleaseNamespace
			},
		},
		{
			name:  "a second tenant namespace cannot be kube-system",
			input: InputSecondNamespace,
			mutate: func(env map[string]string) {
				env[string(InputSecondNamespace)] = "kube-system"
			},
		},
		{
			name:  "a second tenant namespace must be a dns label",
			input: InputSecondNamespace,
			mutate: func(env map[string]string) {
				env[string(InputSecondNamespace)] = "Tenant_B"
			},
		},
		{
			name:  "a second admin token must not contain whitespace",
			input: InputSecondAdminToken,
			mutate: func(env map[string]string) {
				env[string(InputSecondAdminToken)] = "second admin token"
			},
		},
		{
			name:  "a git repository must not traverse",
			input: InputGitRepositoryA,
			mutate: func(env map[string]string) {
				env[string(InputGitRepositoryA)] = "../tenant-b"
			},
		},
		{
			name:  "a git repository must not be absolute",
			input: InputGitRepositoryB,
			mutate: func(env map[string]string) {
				env[string(InputGitRepositoryB)] = "/tenant-b"
			},
		},
		{
			name:  "a git service must be a dns label",
			input: InputGitServiceA,
			mutate: func(env map[string]string) {
				env[string(InputGitServiceA)] = "kodama_git"
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
			name:  "an image must be digest pinned",
			input: InputImage,
			mutate: func(env map[string]string) {
				env[string(InputImage)] = "rg.example.test/kodama:latest"
			},
		},
		{
			name:  "an evidence directory must be absolute",
			input: InputEvidenceDir,
			mutate: func(env map[string]string) {
				env[string(InputEvidenceDir)] = "evidence/Q02"
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
}

// TestHarnessTokenFilesAreReadPerLoad proves a token file is an alternative
// credential source, that a later load observes a refreshed file, that a token
// and its file are refused together, and that a failure never renders the
// credential or the file's contents.
func TestHarnessTokenFilesAreReadPerLoad(t *testing.T) {
	env := validConfigEnv(t)
	delete(env, string(InputAdminToken))
	delete(env, string(InputSecondAdminToken))
	adminFile := writeTempFile(t, "file-admin-token\n")
	env[string(InputAdminTokenFile)] = adminFile
	env[string(InputSecondAdminTokenFile)] = writeTempFile(t, "file-second-admin-token")

	config, err := LoadConfig(envGetter(env))
	if err != nil {
		t.Fatalf("token files refused: %v", err)
	}
	if config.AdminToken.Reveal() != "file-admin-token" || config.SecondAdminToken.Reveal() != "file-second-admin-token" {
		t.Fatal("the token files were not loaded as the admin credentials")
	}
	if missing := config.Missing(Inputs...); !missing.Empty() {
		t.Fatalf("a configuration with token files reports missing inputs: %s", missing)
	}

	if err := os.WriteFile(adminFile, []byte("refreshed-admin-token"), 0o600); err != nil {
		t.Fatalf("refresh the token file: %v", err)
	}
	refreshed, err := LoadConfig(envGetter(env))
	if err != nil || refreshed.AdminToken.Reveal() != "refreshed-admin-token" {
		t.Fatalf("a later load did not read the refreshed token file (error %v)", err)
	}

	for _, refused := range []struct {
		name   string
		mutate func(map[string]string)
	}{
		{"a token and its file", func(env map[string]string) { env[string(InputAdminToken)] = "value-admin-token" }},
		{"a relative token file", func(env map[string]string) { env[string(InputAdminTokenFile)] = "admin.token" }},
		{"a missing token file", func(env map[string]string) { env[string(InputAdminTokenFile)] = adminFile + ".absent" }},
		{"an empty token file", func(env map[string]string) { env[string(InputAdminTokenFile)] = writeTempFile(t, "\n") }},
		{"a multi-line token file", func(env map[string]string) {
			env[string(InputAdminTokenFile)] = writeTempFile(t, "first-line-secret\nsecond-line-secret\n")
		}},
	} {
		mutated := validConfigEnv(t)
		delete(mutated, string(InputAdminToken))
		mutated[string(InputAdminTokenFile)] = adminFile
		refused.mutate(mutated)
		_, err := LoadConfig(envGetter(mutated))
		if err == nil {
			t.Errorf("%s was accepted", refused.name)
			continue
		}
		if !strings.Contains(err.Error(), "Q02_ADMIN_TOKEN") {
			t.Errorf("%s: error %q does not name the input", refused.name, err)
		}
		for _, secret := range []string{"value-admin-token", "refreshed-admin-token", "line-secret"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s: error rendered credential material: %q", refused.name, err)
			}
		}
	}
}

func writeTempFile(t *testing.T, contents string) string {
	t.Helper()
	path := t.TempDir() + "/contents"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return path
}
