//go:build component

// AWS database trust output.
//
// The proof that deploy/aws really exports the operator-supplied RDS CA bundle
// runs the root's HCL with every provider mocked (`tofu test`); a fake
// `tofu output` that always printed a certificate would conceal a missing
// output. The consumer proof then runs the real scripts/bootstrap-secrets AWS
// path against the same tree with fake cloud and Kubernetes CLIs, and shows the
// fail-closed refusal when the output is missing.
package deploy

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// awsDatabaseCAFixture extracts the PEM bundle embedded in the OpenTofu test
// file. The same fixture must be a real CA certificate, otherwise the output
// equality assertion would only compare placeholders.
func awsDatabaseCAFixture(t *testing.T) []byte {
	t.Helper()
	body := readRepoFile(t, repoRoot(t), "deploy/aws/tests/database_ca.tftest.hcl")
	const begin = "-----BEGIN CERTIFICATE-----"
	const end = "-----END CERTIFICATE-----"
	start := strings.Index(body, begin)
	stop := strings.Index(body, end)
	if start < 0 || stop < start {
		t.Fatalf("deploy/aws/tests/database_ca.tftest.hcl has no PEM certificate fixture")
	}
	bundle := body[start : stop+len(end)]
	block, rest := pem.Decode([]byte(bundle + "\n"))
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatalf("the tftest PEM fixture is not a parseable certificate")
	}
	if strings.TrimSpace(string(rest)) != "" {
		t.Fatalf("the tftest PEM fixture has trailing undecoded material: %q", rest)
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse the tftest CA fixture: %v", err)
	}
	if !certificate.IsCA {
		t.Fatalf("the tftest PEM fixture is a certificate but not a CA")
	}
	return []byte(bundle + "\n")
}

// TestAWSExportsDatabaseCACertificate runs deploy/aws with mocked providers and
// proves:
//
//   - database_ca_certificate is the operator-supplied PEM bundle exactly,
//   - it is not the RDS CA identifier,
//   - aws_db_instance.ca_cert_identifier uses the explicit identifier,
//   - empty/whitespace-only and identifier-shaped bundles are refused.
func TestAWSExportsDatabaseCACertificate(t *testing.T) {
	tofuBinary(t)
	root := repoRoot(t)
	awsDatabaseCAFixture(t)

	copied := copyDeployTree(t, root)
	dir := filepath.Join(copied, "aws")
	env := append(tofuProcessEnv(tofuPluginCacheDir(t)), "NO_COLOR=1")

	initOutput, initCode := runTofu(t, dir, env, 15*time.Minute, "init", "-backend=false", "-input=false")
	if initCode != 0 {
		t.Fatalf("tofu init -backend=false failed for the mocked AWS CA test; if the sandbox has no registry access, set TF_CLI_CONFIG_FILE to a provider mirror or KODAMA_TOFU_PLUGIN_CACHE_DIR to a pre-populated cache:\n%s", initOutput)
	}

	output, code := runTofu(t, dir, env, 10*time.Minute, "test", "-test-directory=tests", "-verbose")
	if code != 0 {
		t.Fatalf("the mocked AWS database CA test failed; deploy/aws no longer exports the operator-supplied bundle:\n%s", output)
	}
	for _, run := range []string{
		"exports_operator_supplied_bundle",
		"rejects_empty_bundle",
		"rejects_identifier_as_bundle",
		"rejects_private_key_bundle",
		"rejects_reversed_markers",
		"rejects_misordered_markers",
		"rejects_duplicate_begin_marker",
		"exports_multiple_certificates",
		"derives_postgres_parameter_group_and_eks_api_cidrs",
	} {
		if !strings.Contains(output, run) {
			t.Errorf("tofu test output never mentions run %q; a skipped or deleted case must not pass silently:\n%s", run, output)
		}
	}
}

// awsCloudCase returns the AWS descriptor fixture that the multi-cloud secrets
// tests use, which is checked against the real root's adapter and prefix.
func awsCloudCase(t *testing.T) cloudSecretsCase {
	t.Helper()
	for _, testCase := range cloudSecretsCases {
		if testCase.cloud == "aws" {
			return testCase
		}
	}
	t.Fatal("cloudSecretsCases has no aws entry")
	return cloudSecretsCase{}
}

// prepareAWSSecretsRun writes the AWS descriptor and kubeconfig and adds the
// fake AWS CLI plus reusable PKI.
func prepareAWSSecretsRun(t *testing.T) (*sandbox, cloudSecretsCase, string, string) {
	t.Helper()
	testCase := awsCloudCase(t)
	box := newSandbox(t)
	addCloudKMSFakes(t, box)
	prepareCloudWorkDir(t, box, testCase.cloud)
	descriptorPath := filepath.Join(box.workDir, "descriptor-"+testCase.cloud+".json")
	writeFile(t, descriptorPath, testCase.descriptorJSON(t))
	kubeconfigPath := filepath.Join(box.workDir, "kubeconfig-"+testCase.cloud+".yaml")
	writeFile(t, kubeconfigPath, "")
	return box, testCase, descriptorPath, kubeconfigPath
}

// TestAWSDatabaseCAOutputFeedsBootstrapSecret runs the real bootstrap-secrets
// AWS path with fake CLIs and proves the exported output reaches the
// kodama-database Secret's ca.crt with the public 0644 mode. It also proves the
// fail-closed refusal: a missing output stops the run before any KMS call and
// before kodama-database is created.
func TestAWSDatabaseCAOutputFeedsBootstrapSecret(t *testing.T) {
	t.Run("exports to kodama-database ca.crt", func(t *testing.T) {
		box, testCase, descriptorPath, kubeconfigPath := prepareAWSSecretsRun(t)
		// Serve a real PEM bundle so the manifest assertion is about the
		// operator-supplied certificate, not an arbitrary token.
		bundle := string(awsDatabaseCAFixture(t))
		box.withEnv("TOFU_CA_PEM=" + strings.TrimSuffix(bundle, "\n"))
		code, output := runBootstrapCloud(t, box, testCase.cloud, descriptorPath, kubeconfigPath)
		if code != 0 {
			t.Fatalf("bootstrap-secrets %s failed with exit %d after the AWS root gained database_ca_certificate:\n%s", testCase.cloud, code, output)
		}

		tofuArgv := readLogLines(t, box.tofuLog)
		foundOutput := false
		for _, line := range tofuArgv {
			if strings.Contains(line, "output") && strings.Contains(line, "-raw") && strings.Contains(line, "database_ca_certificate") {
				foundOutput = true
			}
		}
		if !foundOutput {
			t.Errorf("bootstrap-secrets never read `tofu output -raw database_ca_certificate`; the AWS output is not consumed:\n%s", strings.Join(tofuArgv, "\n"))
		}

		// The database Secret is rendered client-side (env-file values plus
		// ca.crt) and piped to kubectl, so the certificate reaches kubectl on
		// stdin as base64. Requiring the exact bundle proves the output value
		// was consumed, not merely read.
		wantCA := base64.StdEncoding.EncodeToString([]byte(bundle))
		if stdin := box.readKubectlStdin(t); !strings.Contains(stdin, wantCA) {
			t.Errorf("the kodama-database Secret manifest does not carry the database_ca_certificate bundle (base64 %s...)", wantCA[:16])
		}

		modes := box.dbCAChmodRecords(t)
		if len(modes) == 0 {
			t.Fatal("bootstrap-secrets never chmodded the database CA file from the AWS output")
		}
		for _, mode := range modes {
			if mode != "644" {
				t.Errorf("database CA file requested mode %s, want 644 (public certificate)", mode)
			}
		}

		if len(readLogLines(t, filepath.Join(box.recordsDir, "aws.argv"))) == 0 {
			t.Error("the AWS KMS encrypt never ran; the CA-success path did not reach the KEK step")
		}
	})

	t.Run("missing output refuses before key wrapping", func(t *testing.T) {
		box, testCase, descriptorPath, kubeconfigPath := prepareAWSSecretsRun(t)
		box.withEnv("TOFU_FAIL_CA=1")
		code, output := runBootstrapCloud(t, box, testCase.cloud, descriptorPath, kubeconfigPath)
		if code == 0 {
			t.Fatalf("bootstrap-secrets %s succeeded although database_ca_certificate was missing:\n%s", testCase.cloud, output)
		}
		if !strings.Contains(output, databaseCACertificateUnavailable) {
			t.Fatalf("the missing-output refusal does not print the recorded message %q:\n%s", databaseCACertificateUnavailable, output)
		}
		if len(readLogLines(t, filepath.Join(box.recordsDir, "aws.argv"))) != 0 {
			t.Error("the missing-output refusal happened after the AWS KMS encrypt; it must precede key wrapping")
		}
		for _, line := range box.readKubectlArgv(t) {
			if strings.Contains(line, "create") && strings.Contains(line, "kodama-database") {
				t.Errorf("bootstrap-secrets created kodama-database without a CA output:\n%s", line)
			}
		}
	})
}

// TestAWSDatabaseCAFixtureIsPublicDocumentationMaterial keeps the fixture file
// small and public: it must not carry a private key or a credential.
func TestAWSDatabaseCAFixtureIsPublicDocumentationMaterial(t *testing.T) {
	body := readRepoFile(t, repoRoot(t), "deploy/aws/tests/database_ca.tftest.hcl")
	for _, forbidden := range []string{"AWS_SECRET_ACCESS_KEY", "password ="} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the AWS database CA tftest contains %q; only public certificate material belongs there", forbidden)
		}
	}
	// The only private-key text is the negative fixture that proves the
	// validation refuses it. The positive fixtures above stay public
	// certificate material. Check every occurrence: a private key added to any
	// later positive run must also fail this test.
	found := strings.Contains(body, "PRIVATE KEY")
	if !found {
		t.Fatal("the tftest no longer carries the private-key negative fixture")
	}
	negative := strings.Index(body, `run "rejects_private_key_bundle"`)
	if negative < 0 {
		t.Fatal("the tftest no longer carries the rejects_private_key_bundle run")
	}
	negativeEnd := strings.Index(body[negative:], "\nrun \"")
	if negativeEnd < 0 {
		negativeEnd = len(body) - negative
	}
	negativeEnd += negative
	for offset := 0; ; {
		index := strings.Index(body[offset:], "PRIVATE KEY")
		if index < 0 {
			break
		}
		position := offset + index
		if position < negative || position >= negativeEnd {
			t.Errorf("the tftest mentions PRIVATE KEY at byte %d, outside the rejects_private_key_bundle negative case", position)
		}
		offset = position + len("PRIVATE KEY")
	}
	if _, err := os.Stat(filepath.Join(repoRoot(t), "deploy", "aws", "tests", "database_ca.tftest.hcl")); err != nil {
		t.Fatalf("stat the AWS database CA tftest: %v", err)
	}
}
