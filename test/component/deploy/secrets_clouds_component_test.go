//go:build component

// Multi-cloud secrets qualification without cloud credentials.
//
// AWS, GCP and Scaleway all stay supported. These tests exercise the real
// scripts/bootstrap-secrets path for each cloud with fake `aws`, `gcloud`,
// `scw`, `kubectl` and `tofu` executables (no credentials, no network, no
// cluster), the installation key-reference validator for each provider, and the
// descriptor -> Helm values -> schema-validated render path for each cloud.
// The fake tofu only serves outputs the real roots declare, so a root that
// stops exporting database_ca_certificate fails the subtest instead of passing
// against a friendlier fake; see descriptor_outputs_component_test.go.
//
// Custody is checked with the same planted canaries the existing secrets tests
// use: the four KODAMA_DB_* sentinels, the deterministic 32-byte KEK
// (stubKEKRaw) in raw/hex/base64 form, and the KMS access/secret values. None
// may appear in argv, stdout/stderr or a leftover file.
package deploy

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/internal/installation"
)

// cloudKMSAccessStub is the fake Scaleway IAM access key the fake tofu emits.
const cloudKMSAccessStub = "SCWACCESSKEY0123456789"

// fakeAWSKMS records its argv and, for the documented fileb:// form, records
// the mode and content of the plaintext file rather than reading a command-line
// value. It prints the base64 ciphertext blob the real CLI returns.
const fakeAWSKMS = `#!/usr/bin/env bash
set -u
{
  printf 'aws'
  for arg in "$@"; do printf '\t%s' "$arg"; done
  printf '\n'
} | tee -a "$AWS_ARGV_LOG" >> "$ALL_ARGV_LOG"

for arg in "$@"; do
  case "$arg" in
    fileb://*)
      file="${arg#fileb://}"
      {
        printf 'plaintext_mode=%s\n' "$(stat -c '%a' "$file" 2>/dev/null || printf 'missing')"
        printf 'plaintext_dir_mode=%s\n' "$(stat -c '%a' "$(dirname "$file")" 2>/dev/null || printf 'missing')"
        printf 'plaintext=%s\n' "$(cat "$file" 2>/dev/null || true)"
      } >>"$AWS_SECRET_LOG"
      ;;
  esac
done
printf 'd3JhcHBlZC1rZWs=\n'
`

// fakeGCloudKMS records its argv and copies the ciphertext file the helper
// names. The plaintext is read from --plaintext-file, never from argv.
const fakeGCloudKMS = `#!/usr/bin/env bash
set -u
{
  printf 'gcloud'
  for arg in "$@"; do printf '\t%s' "$arg"; done
  printf '\n'
} | tee -a "$GCLOUD_ARGV_LOG" >> "$ALL_ARGV_LOG"

plaintext_file=""
ciphertext_file=""
for arg in "$@"; do
  case "$arg" in
    --plaintext-file=*) plaintext_file="${arg#--plaintext-file=}" ;;
    --ciphertext-file=*) ciphertext_file="${arg#--ciphertext-file=}" ;;
  esac
done
if [[ -z "$ciphertext_file" ]]; then
  echo 'fake gcloud: --ciphertext-file is required' >&2
  exit 1
fi
if [[ -n "$plaintext_file" ]]; then
  {
    printf 'plaintext_mode=%s\n' "$(stat -c '%a' "$plaintext_file" 2>/dev/null || printf 'missing')"
    printf 'plaintext_dir_mode=%s\n' "$(stat -c '%a' "$(dirname "$plaintext_file")" 2>/dev/null || printf 'missing')"
    printf 'plaintext=%s\n' "$(cat "$plaintext_file" 2>/dev/null || true)"
  } >>"$GCLOUD_SECRET_LOG"
fi
printf 'wrapped-kek' >"$ciphertext_file"
`

// cloudSecretsCase is one supported cloud's descriptor coordinates. The values
// mirror the real `deployment_descriptor` produced by deploy/<cloud>/outputs.tf:
// the adapter and reference prefix come from deploy/modules/context/main.tf.
type cloudSecretsCase struct {
	cloud            string
	adapter          string
	keyID            string
	keyRef           string
	region           string
	zone             string
	account          string
	storageClass     string
	workloadIdentity string
}

var cloudSecretsCases = []cloudSecretsCase{
	{
		cloud:            "scw",
		adapter:          "scw-kms",
		keyID:            "fr-par/00000000-0000-0000-0000-000000000000",
		keyRef:           "scw-kms:fr-par/00000000-0000-0000-0000-000000000000",
		region:           "fr-par",
		zone:             "fr-par-1",
		account:          "project-1",
		storageClass:     "sbs-default",
		workloadIdentity: "scw-iam-application-id",
	},
	{
		cloud:            "aws",
		adapter:          "aws-kms",
		keyID:            "arn:aws:kms:eu-west-1:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab",
		keyRef:           "aws-kms:arn:aws:kms:eu-west-1:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab",
		region:           "eu-west-1",
		zone:             "eu-west-1a",
		account:          "123456789012",
		storageClass:     "gp3",
		workloadIdentity: "arn:aws:iam::123456789012:role/kodama-controller",
	},
	{
		cloud:            "gcp",
		adapter:          "gcp-kms",
		keyID:            "projects/demo-project/locations/europe-west1/keyRings/kodama/cryptoKeys/kek",
		keyRef:           "gcp-kms:projects/demo-project/locations/europe-west1/keyRings/kodama/cryptoKeys/kek",
		region:           "europe-west1",
		zone:             "europe-west1-b",
		account:          "demo-project",
		storageClass:     "premium-rwo",
		workloadIdentity: "kodama-controller@demo-project.iam.gserviceaccount.com",
	},
}

// descriptor returns the deployment descriptor shape deploy/<cloud>/outputs.tf
// writes: one object consumed by scripts/deploy and scripts/bootstrap-secrets.
func (c cloudSecretsCase) descriptor() map[string]any {
	return map[string]any{
		"cloud":                 c.cloud,
		"cluster_name":          "stage",
		"installation_id":       "kodama-" + c.cloud + "-stage",
		"environment":           "production",
		"cost_profile":          "cost-conscious",
		"account_or_project_id": c.account,
		"region":                c.region,
		"zone":                  c.zone,
		"release_namespace":     "kodama-system",
		"tenants":               []string{"tenant-a"},
		"postgres": map[string]any{
			"instance_id":   "pg-" + c.cloud,
			"instance_name": "kodama-" + c.cloud,
			"database_name": "kodama",
			"admin_user":    "kodama_admin",
			"host":          "db.example",
			"port":          5432,
			"ssl_mode":      "verify-full",
		},
		"kms": map[string]any{
			"key_adapter":  c.adapter,
			"key_id":       c.keyID,
			"key_ref":      c.keyRef,
			"iam_identity": c.workloadIdentity,
		},
		"kubernetes": map[string]any{
			"cluster_name":          "stage",
			"storage_class":         c.storageClass,
			"service_account_name":  "kodama",
			"workload_identity_ref": c.workloadIdentity,
		},
	}
}

func (c cloudSecretsCase) descriptorJSON(t *testing.T) string {
	t.Helper()
	body, err := json.MarshalIndent(c.descriptor(), "", "  ")
	if err != nil {
		t.Fatalf("encode %s descriptor: %v", c.cloud, err)
	}
	return string(body) + "\n"
}

// addCloudKMSFakes installs the missing cloud CLIs in the sandbox PATH and
// points their recorders at fresh logs. newSandbox already supplies kubectl,
// scw, tofu, openssl and the argv recorders for text tools.
func addCloudKMSFakes(t *testing.T, box *sandbox) {
	t.Helper()
	writeExecutable(t, filepath.Join(box.root, "bin", "aws"), fakeAWSKMS)
	writeExecutable(t, filepath.Join(box.root, "bin", "gcloud"), fakeGCloudKMS)
	box.withEnv(
		"AWS_ARGV_LOG="+filepath.Join(box.recordsDir, "aws.argv"),
		"AWS_SECRET_LOG="+filepath.Join(box.recordsDir, "aws.secret"),
		"GCLOUD_ARGV_LOG="+filepath.Join(box.recordsDir, "gcloud.argv"),
		"GCLOUD_SECRET_LOG="+filepath.Join(box.recordsDir, "gcloud.secret"),
	)
}

// prepareCloudWorkDir pre-creates reusable PKI so the tests do not spend time
// generating RSA certificates under the fake openssl wrapper. bootstrap-secrets
// reuses a complete PKI and the fake kubectl never validates its contents.
func prepareCloudWorkDir(t *testing.T, box *sandbox, cloud string) {
	t.Helper()
	pki := filepath.Join(box.stateDir, cloud, "stage", "pki")
	if err := os.MkdirAll(pki, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", pki, err)
	}
	for _, name := range []string{"ca.crt", "kodama.crt", "nats.crt", "client.crt"} {
		writeFile(t, filepath.Join(pki, name), "test-"+name+"\n")
	}
}

// runBootstrapCloud runs the real script for one cloud and descriptor.
func runBootstrapCloud(t *testing.T, box *sandbox, cloud, descriptorPath, kubeconfigPath string) (int, string) {
	t.Helper()
	script := filepath.Join(repoRoot(t), "scripts", "bootstrap-secrets")
	command := exec.Command("bash", script, cloud,
		"--cluster", "stage",
		"--descriptor", descriptorPath,
		"--kubeconfig", kubeconfigPath)
	command.Env = append(append([]string(nil), box.env...), box.extraEnv...)
	output, err := command.CombinedOutput()
	code := 0
	if err != nil {
		exitError, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run bootstrap-secrets %s: %v\n%s", cloud, err, output)
		}
		code = exitError.ExitCode()
	}
	return code, string(output)
}

// readLogLines returns the non-empty lines of an optional recorded log.
func readLogLines(t *testing.T, path string) []string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read %s: %v", path, err)
	}
	var lines []string
	for line := range strings.SplitSeq(string(body), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// canaryForms is the full secret composition checked against output, argv and
// leftover files.
func canaryForms() []credentialForm {
	forms := kubectlCredentialForms(stubKEKMaterial(), dbForms())
	forms = append(forms,
		credentialForm{label: "KMS access key", secret: cloudKMSAccessStub},
		credentialForm{label: "KMS secret key", secret: kmsSecretStub},
	)
	return forms
}

func assertCanariesAbsentFromOutput(t *testing.T, output string, forms []credentialForm) {
	t.Helper()
	for _, form := range forms {
		if form.secret != "" && strings.Contains(output, form.secret) {
			t.Errorf("%s appears in the combined stdout+stderr of bootstrap-secrets", form.label)
		}
	}
}

func assertCanariesAbsentFromTrees(t *testing.T, dirs []string, forms []credentialForm) {
	t.Helper()
	needles := make(map[string][]byte, len(forms))
	for _, form := range forms {
		if form.secret != "" {
			needles[form.label] = []byte(form.secret)
		}
	}
	assertAbsentFromTrees(t, dirs, needles)
}

// assertKEKSecretCreatedOnce checks the fake kubectl received exactly one
// `kodama-keys` creation carrying kek-v1.blob from a 0600 file in a 0700
// directory (the private temporary KEK directory).
func assertKEKSecretCreatedOnce(t *testing.T, box *sandbox) {
	t.Helper()
	body, err := os.ReadFile(box.kubectlSecretLog)
	if err != nil {
		t.Fatalf("fake kubectl recorded no secret file metadata: %v", err)
	}
	var blocks []string
	var current []string
	inBlock := false
	for line := range strings.SplitSeq(string(body), "\n") {
		switch {
		case line == "from_file_end":
			inBlock = false
			blocks = append(blocks, strings.Join(current, "\n"))
			current = nil
		case strings.HasPrefix(line, "from_file_key="):
			inBlock = true
			current = append(current, line)
		case inBlock:
			current = append(current, line)
		}
	}
	matches := 0
	for _, block := range blocks {
		if !strings.Contains(block, "from_file_key=kek-v1.blob") {
			continue
		}
		matches++
		if !strings.Contains(block, "from_file_mode=600") {
			t.Errorf("kek-v1.blob was not read from a 0600 file:\n%s", block)
		}
		if !strings.Contains(block, "from_file_dir_mode=700") {
			t.Errorf("kek-v1.blob was not read from a 0700 directory:\n%s", block)
		}
	}
	if matches != 1 {
		t.Fatalf("kubectl created kodama-keys %d times, want exactly once (the KEK is never regenerated)", matches)
	}
}

// TestBootstrapSecretsSelectsKMSProviderPerCloud runs the real
// scripts/bootstrap-secrets for scw, aws and gcp with the cloud CLIs replaced by
// recorders. The descriptor fixture first has to agree with the real roots'
// statically parsed adapter and key-reference prefix. It then proves the cloud
// argument selects exactly one KMS helper, that the KEK reaches it through a
// private file (never argv), that the rendered Kubernetes Secrets are the
// expected ones, and that no canary reaches argv, output or a leftover file.
//
// Every supported root must export database_ca_certificate: a cloud whose root
// lost it fails here rather than getting a friendlier fake. The explicit
// missing-output refusal is pinned separately in aws_ca_component_test.go.
func TestBootstrapSecretsSelectsKMSProviderPerCloud(t *testing.T) {
	facts := readDeploymentFacts(t)
	for _, testCase := range cloudSecretsCases {
		t.Run(testCase.cloud, func(t *testing.T) {
			deployment, ok := facts[testCase.cloud]
			if !ok {
				t.Fatalf("no static deployment facts for %s", testCase.cloud)
			}
			// The fixture must not drift from what deploy/<cloud>/outputs.tf
			// and deploy/modules/context/main.tf really emit.
			if testCase.adapter != deployment.keyAdapter {
				t.Fatalf("descriptor fixture adapter %q diverged from deploy/%s/outputs.tf adapter %q", testCase.adapter, testCase.cloud, deployment.keyAdapter)
			}
			if !strings.HasPrefix(testCase.keyRef, deployment.keyRefPrefix) {
				t.Fatalf("descriptor fixture key_ref %q is not in the deploy/%s family %q", testCase.keyRef, testCase.cloud, deployment.keyRefPrefix)
			}

			box := newSandbox(t)
			addCloudKMSFakes(t, box)
			prepareCloudWorkDir(t, box, testCase.cloud)

			descriptorPath := filepath.Join(box.workDir, "descriptor-"+testCase.cloud+".json")
			writeFile(t, descriptorPath, testCase.descriptorJSON(t))
			kubeconfigPath := filepath.Join(box.workDir, "kubeconfig-"+testCase.cloud+".yaml")
			writeFile(t, kubeconfigPath, "")

			code, output := runBootstrapCloud(t, box, testCase.cloud, descriptorPath, kubeconfigPath)
			if !deployment.databaseCACertificate {
				t.Fatalf("deploy/%s/outputs.tf does not declare database_ca_certificate; the fake tofu cannot serve an output the real root lacks", testCase.cloud)
			}
			if code != 0 {
				t.Fatalf("bootstrap-secrets %s failed with exit %d:\n%s", testCase.cloud, code, output)
			}
			t.Logf("bootstrap-secrets %s completed against fake cloud/Kubernetes CLIs; no real %s readiness is implied", testCase.cloud, testCase.cloud)

			forms := canaryForms()
			assertCanariesAbsentFromOutput(t, output, forms)
			assertSecretFormsAbsentFromAllArgv(t, box, forms)

			scwLines := readLogLines(t, box.argvLog)
			awsLines := readLogLines(t, filepath.Join(box.recordsDir, "aws.argv"))
			gcloudLines := readLogLines(t, filepath.Join(box.recordsDir, "gcloud.argv"))

			// Provider selection: exactly the matching helper runs.
			switch testCase.cloud {
			case "scw":
				if len(awsLines) != 0 || len(gcloudLines) != 0 {
					t.Errorf("scw selected another cloud's KMS client: aws=%v gcloud=%v", awsLines, gcloudLines)
				}
				rawKey := strings.TrimPrefix(testCase.keyID, testCase.region+"/")
				want := "scw\tkeymanager\tkey\tencrypt\t" + rawKey
				found := false
				for _, line := range scwLines {
					if strings.HasPrefix(line, want) && strings.Contains(line, "region="+testCase.region) && strings.Contains(line, "plaintext=@") {
						found = true
					}
				}
				if !found {
					t.Errorf("scw KMS call did not use key encrypt %s with region=%s and plaintext=@file:\n%s", rawKey, testCase.region, strings.Join(scwLines, "\n"))
				}
			case "aws":
				if len(scwLines) != 0 || len(gcloudLines) != 0 {
					t.Errorf("aws selected another cloud's KMS client: scw=%v gcloud=%v", scwLines, gcloudLines)
				}
				found := false
				for _, line := range awsLines {
					if strings.Contains(line, "kms\tencrypt") &&
						strings.Contains(line, "--key-id\t"+testCase.keyID) &&
						strings.Contains(line, "--region\t"+testCase.region) &&
						strings.Contains(line, "fileb://") {
						found = true
					}
				}
				if !found {
					t.Errorf("aws KMS call did not encrypt with --key-id %s --region %s and a fileb:// plaintext:\n%s", testCase.keyID, testCase.region, strings.Join(awsLines, "\n"))
				}
				body, err := os.ReadFile(filepath.Join(box.recordsDir, "aws.secret"))
				if err != nil {
					t.Fatalf("aws KMS never recorded the plaintext file: %v", err)
				}
				if !strings.Contains(string(body), "plaintext_mode=600") || !strings.Contains(string(body), "plaintext_dir_mode=700") {
					t.Errorf("aws plaintext file was not 0600 in a 0700 directory:\n%s", body)
				}
			case "gcp":
				if len(scwLines) != 0 || len(awsLines) != 0 {
					t.Errorf("gcp selected another cloud's KMS client: scw=%v aws=%v", scwLines, awsLines)
				}
				found := false
				for _, line := range gcloudLines {
					if strings.Contains(line, "kms\tencrypt") &&
						strings.Contains(line, "--key="+testCase.keyID) &&
						strings.Contains(line, "--plaintext-file=") &&
						strings.Contains(line, "--ciphertext-file=") {
						found = true
					}
				}
				if !found {
					t.Errorf("gcp KMS call did not encrypt with --key=%s and file arguments:\n%s", testCase.keyID, strings.Join(gcloudLines, "\n"))
				}
				body, err := os.ReadFile(filepath.Join(box.recordsDir, "gcloud.secret"))
				if err != nil {
					t.Fatalf("gcloud KMS never recorded the plaintext file: %v", err)
				}
				if !strings.Contains(string(body), "plaintext_mode=600") || !strings.Contains(string(body), "plaintext_dir_mode=700") {
					t.Errorf("gcp plaintext file was not 0600 in a 0700 directory:\n%s", body)
				}
			}

			assertKEKSecretCreatedOnce(t, box)

			// The database env-file reached kubectl with the four planted
			// passwords, so the custody check below is not vacuous.
			secretLog, err := os.ReadFile(box.kubectlSecretLog)
			if err != nil {
				t.Fatalf("fake kubectl recorded no secret file content: %v", err)
			}
			for _, sentinel := range dbSentinels {
				if !strings.Contains(string(secretLog), sentinel) {
					t.Errorf("the fake kubectl never received database password %q; the env-file path was not exercised", sentinel)
				}
			}

			// Only Scaleway needs the separate KMS credential Secret.
			kubectlArgv := box.readKubectlArgv(t)
			hasCredentialsSecret := false
			for _, line := range kubectlArgv {
				if strings.Contains(line, "kodama-kms-credentials") {
					hasCredentialsSecret = true
				}
			}
			if testCase.cloud == "scw" && !hasCredentialsSecret {
				t.Error("scw did not reconcile kodama-kms-credentials")
			}
			if testCase.cloud != "scw" && hasCredentialsSecret {
				t.Errorf("%s created a Scaleway-only kodama-kms-credentials Secret", testCase.cloud)
			}

			// No canary survives on disk in the cloud work directory or the
			// short-lived temp directory.
			assertCanariesAbsentFromTrees(t, []string{filepath.Join(box.stateDir, testCase.cloud), box.tempDir}, forms)
		})
	}
}

// mutateDescriptor parses the sandbox's valid scw descriptor, applies mutate
// and writes it back as box work's descriptor.json.
func mutateDescriptor(t *testing.T, box *sandbox, mutate func(map[string]any)) {
	t.Helper()
	var descriptor map[string]any
	if err := json.Unmarshal([]byte(readDescriptorForTest(t, box)), &descriptor); err != nil {
		t.Fatalf("parse sandbox descriptor: %v", err)
	}
	mutate(descriptor)
	body, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatalf("encode mutated descriptor: %v", err)
	}
	box.writeDescriptor(t, string(body))
}

func kmsSection(t *testing.T, descriptor map[string]any) map[string]any {
	t.Helper()
	section, ok := descriptor["kms"].(map[string]any)
	if !ok {
		t.Fatal("descriptor has no kms object")
	}
	return section
}

// TestBootstrapSecretsRefusesMissingOrForeignProvider pins the refusal paths.
// The last three cases guard the provider check: a script that trusted the
// descriptor's key adapter would, for a descriptor whose cloud is scw but whose
// kms.key_adapter/key_ref names another provider, create cluster Secrets and
// then call the wrong KMS API (or a foreign key id). It must refuse before the
// first kubectl call.
func TestBootstrapSecretsRefusesMissingOrForeignProvider(t *testing.T) {
	t.Run("missing cloud argument", func(t *testing.T) {
		box := newSandbox(t)
		command := exec.Command("bash", filepath.Join(repoRoot(t), "scripts", "bootstrap-secrets"))
		command.Env = box.env
		output, err := command.CombinedOutput()
		if err == nil {
			t.Fatalf("bootstrap-secrets accepted a missing cloud argument:\n%s", output)
		}
		if !strings.Contains(string(output), "Usage:") {
			t.Errorf("missing-cloud refusal did not print usage:\n%s", output)
		}
	})

	t.Run("unknown cloud", func(t *testing.T) {
		box := newSandbox(t)
		command := exec.Command("bash", filepath.Join(repoRoot(t), "scripts", "bootstrap-secrets"), "azure")
		command.Env = box.env
		output, err := command.CombinedOutput()
		if err == nil {
			t.Fatalf("bootstrap-secrets accepted an unknown cloud:\n%s", output)
		}
		if !strings.Contains(string(output), "invalid cloud target") {
			t.Errorf("unknown-cloud refusal does not name the target:\n%s", output)
		}
	})

	t.Run("missing descriptor", func(t *testing.T) {
		box := newSandbox(t)
		code, output := runBootstrapCloud(t, box, "scw", filepath.Join(box.workDir, "does-not-exist.json"), filepath.Join(box.workDir, "kubeconfig.yaml"))
		if code == 0 {
			t.Fatalf("bootstrap-secrets accepted a missing descriptor:\n%s", output)
		}
		if !strings.Contains(output, "deployment descriptor not found") {
			t.Errorf("missing-descriptor refusal does not name the descriptor:\n%s", output)
		}
		if len(readLogLines(t, box.argvLog)) != 0 {
			t.Error("a missing descriptor reached the KMS client")
		}
	})

	t.Run("foreign descriptor cloud", func(t *testing.T) {
		box := newSandbox(t)
		mutateDescriptor(t, box, func(descriptor map[string]any) { descriptor["cloud"] = "aws" })
		code, output := box.run(t, false)
		if code == 0 {
			t.Fatalf("bootstrap-secrets accepted an aws descriptor for the scw cloud:\n%s", output)
		}
		if !strings.Contains(output, "descriptor cloud mismatch") {
			t.Errorf("foreign-cloud refusal does not name the mismatch:\n%s", output)
		}
		if len(readLogLines(t, box.argvLog)) != 0 {
			t.Error("a foreign descriptor reached the KMS client")
		}
	})

	for _, testCase := range []struct {
		name   string
		mutate func(map[string]any)
		want   string
	}{
		{
			name: "missing key adapter",
			mutate: func(descriptor map[string]any) {
				delete(kmsSection(t, descriptor), "key_adapter")
			},
			want: "kms.key_adapter",
		},
		{
			name: "foreign key adapter",
			mutate: func(descriptor map[string]any) {
				section := kmsSection(t, descriptor)
				section["key_adapter"] = "aws-kms"
				section["key_ref"] = "aws-kms:arn:aws:kms:eu-west-1:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab"
			},
			want: "does not match the selected cloud",
		},
		{
			name: "foreign key reference family",
			mutate: func(descriptor map[string]any) {
				kmsSection(t, descriptor)["key_ref"] = "aws-kms:arn:aws:kms:eu-west-1:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab"
			},
			want: "key reference family",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			box := newSandbox(t)
			mutateDescriptor(t, box, testCase.mutate)
			code, output := box.run(t, false)
			if code == 0 {
				t.Fatalf("bootstrap-secrets accepted %s:\n%s", testCase.name, output)
			}
			if !strings.Contains(strings.ToLower(output), strings.ToLower(testCase.want)) {
				t.Errorf("refusal of %s does not name %q:\n%s", testCase.name, testCase.want, output)
			}
			if lines := box.readKubectlArgv(t); len(lines) != 0 {
				t.Errorf("%s reached kubectl before being refused:\n%s", testCase.name, strings.Join(lines, "\n"))
			}
			if len(readLogLines(t, box.argvLog)) != 0 {
				t.Errorf("%s reached the KMS client before being refused", testCase.name)
			}
			assertCanariesAbsentFromOutput(t, output, canaryForms())
		})
	}
}

// installationDocument renders one explicit production installation document.
// An empty adapter or reference omits the line, so a missing provider and a
// missing reference are distinct refusals.
func installationDocument(adapter, keyRef string) string {
	lines := []string{
		"environment = production",
		"state = postgres",
		"messaging = nats",
		"runtime = kubernetes",
		"database_url = postgres://kodama@database:5432/kodama?sslmode=verify-full",
		"nats_url = tls://nats:4222",
		"public_url = https://kodama.example",
	}
	if adapter != "" {
		lines = append(lines, "keys = "+adapter)
	}
	if keyRef != "" {
		lines = append(lines, "key_ref = "+keyRef)
	}
	return strings.Join(lines, "\n") + "\n"
}

// TestInstallationKeyReferenceGrammarPerCloud exercises the production
// validator (internal/installation Config.Preflight via ParseConfig) for the
// three cloud adapters. It accepts each provider's real reference shape and
// refuses a foreign family, a bare prefix, whitespace-padded names and a
// missing provider.
//
// The scw-kms region/id grammar is reserved and is not implemented: the
// validator only owns the family prefix and a non-empty, whitespace/control-free
// key name. This test pins exactly that implemented rule and deliberately does
// not assert a Scaleway-specific structure that does not exist yet.
func TestInstallationKeyReferenceGrammarPerCloud(t *testing.T) {
	for _, testCase := range cloudSecretsCases {
		t.Run("accepts "+testCase.cloud, func(t *testing.T) {
			if _, err := installation.ParseConfig(strings.NewReader(installationDocument(testCase.adapter, testCase.keyRef))); err != nil {
				t.Fatalf("production %s reference %q was refused: %v", testCase.cloud, testCase.keyRef, err)
			}
		})
	}

	refusals := []struct {
		name    string
		adapter string
		keyRef  string
	}{
		{name: "aws names gcp", adapter: "aws-kms", keyRef: cloudSecretsCases[2].keyRef},
		{name: "gcp names scw", adapter: "gcp-kms", keyRef: cloudSecretsCases[0].keyRef},
		{name: "scw names aws", adapter: "scw-kms", keyRef: cloudSecretsCases[1].keyRef},
		{name: "aws bare prefix", adapter: "aws-kms", keyRef: "aws-kms:"},
		{name: "gcp bare prefix", adapter: "gcp-kms", keyRef: "gcp-kms:"},
		{name: "scw bare prefix", adapter: "scw-kms", keyRef: "scw-kms:"},
		{name: "space after prefix", adapter: "scw-kms", keyRef: "scw-kms: fr-par/key"},
		{name: "uppercase prefix", adapter: "aws-kms", keyRef: "AWS-KMS:alias/kodama"},
		{name: "unknown adapter", adapter: "azure-kms", keyRef: "aws-kms:alias/kodama"},
		{name: "missing provider", adapter: "", keyRef: "aws-kms:alias/kodama"},
		{name: "missing reference", adapter: "aws-kms", keyRef: ""},
	}
	for _, testCase := range refusals {
		t.Run("refuses "+testCase.name, func(t *testing.T) {
			if _, err := installation.ParseConfig(strings.NewReader(installationDocument(testCase.adapter, testCase.keyRef))); err == nil {
				t.Fatalf("production configuration accepted %s (%q / %q)", testCase.name, testCase.adapter, testCase.keyRef)
			}
		})
	}
}

// TestDeployScriptSelectsKeyAdapterByCloud exercises the shared deploy helper
// that both scripts/deploy and scripts/bootstrap-secrets use to turn a cloud
// into its key adapter family, and the matching cloud refusal.
func TestDeployScriptSelectsKeyAdapterByCloud(t *testing.T) {
	root := repoRoot(t)
	deployLib := filepath.Join(root, "scripts", "lib", "deploy.sh")

	command := exec.Command("bash", "-c", `source "$1"; printf '%s\n' "$(key_adapter_for_cloud scw)" "$(key_adapter_for_cloud aws)" "$(key_adapter_for_cloud gcp)"`, "bash", deployLib)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("key_adapter_for_cloud failed: %v\n%s", err, output)
	}
	if got, want := string(output), "scw-kms\naws-kms\ngcp-kms\n"; got != want {
		t.Errorf("key_adapter_for_cloud output = %q, want %q", got, want)
	}

	for _, testCase := range []struct {
		name    string
		snippet string
		want    string
	}{
		{name: "unknown adapter", snippet: `key_adapter_for_cloud azure`, want: "unknown cloud: azure"},
		{name: "unknown cloud target", snippet: `validate_cloud azure`, want: "invalid cloud target"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			command := exec.Command("bash", "-c", `source "$1"; eval "$2"`, "bash", deployLib, testCase.snippet)
			output, err := command.CombinedOutput()
			if err == nil {
				t.Fatalf("%s was accepted:\n%s", testCase.name, output)
			}
			if !strings.Contains(string(output), testCase.want) {
				t.Errorf("%s refusal does not contain %q:\n%s", testCase.name, testCase.want, output)
			}
		})
	}
}

// TestDescriptorRendersSchemaValidatedValuesPerCloud runs the real
// `scripts/deploy <cloud> render-values` action for each cloud, asserts the
// descriptor's KMS/identity fields reach the generated values, renders the
// stack chart with helm (which validates against values.schema.json), and uses
// an invalid enum as a negative control proving schema validation is on.
func TestDescriptorRendersSchemaValidatedValuesPerCloud(t *testing.T) {
	helm, chart := stageStackChart(t)
	for _, testCase := range cloudSecretsCases {
		t.Run(testCase.cloud, func(t *testing.T) {
			stateDir := t.TempDir()
			workDir := filepath.Join(stateDir, testCase.cloud, "stage")
			if err := os.MkdirAll(workDir, 0o700); err != nil {
				t.Fatalf("mkdir %s: %v", workDir, err)
			}
			writeFile(t, filepath.Join(workDir, "descriptor.json"), testCase.descriptorJSON(t))

			command := exec.Command("bash", filepath.Join(repoRoot(t), "scripts", "deploy"), testCase.cloud, "render-values", "--cluster", "stage")
			command.Env = append(os.Environ(),
				"KODAMA_DEPLOY_STATE_DIR="+stateDir,
				"KODAMA_IMAGE_REPOSITORY=registry.example.com/kodama",
				"KODAMA_IMAGE_TAG=0.1.0",
			)
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("scripts/deploy %s render-values failed: %v\n%s", testCase.cloud, err, output)
			}

			valuesPath := filepath.Join(workDir, "values.generated.yaml")
			info, err := os.Stat(valuesPath)
			if err != nil {
				t.Fatalf("stat rendered values: %v", err)
			}
			if mode := info.Mode().Perm(); mode != 0o600 {
				t.Errorf("rendered values mode = %o, want 600", mode)
			}
			body, err := os.ReadFile(valuesPath)
			if err != nil {
				t.Fatalf("read rendered values: %v", err)
			}
			var values map[string]any
			if err := yaml.Unmarshal(body, &values); err != nil {
				t.Fatalf("parse rendered values: %v", err)
			}
			kodama, ok := values["kodama"].(map[string]any)
			if !ok {
				t.Fatalf("rendered values have no kodama section:\n%s", body)
			}
			config := kodama["config"].(map[string]any)
			if _, present := config["optionalCapabilities"]; present {
				t.Errorf("rendered values still emit dead config.optionalCapabilities:\n%s", body)
			}
			if got := config["keyAdapter"]; got != testCase.adapter {
				t.Errorf("rendered keyAdapter = %v, want %s", got, testCase.adapter)
			}
			if got := config["keyReference"]; got != testCase.keyRef {
				t.Errorf("rendered keyReference = %v, want %s", got, testCase.keyRef)
			}
			nats := values["nats"].(map[string]any)
			jetstream := nats["jetstream"].(map[string]any)
			if got := jetstream["storageClass"]; got != testCase.storageClass {
				t.Errorf("rendered NATS storageClass = %v, want %s", got, testCase.storageClass)
			}

			annotations := map[string]any{}
			if serviceAccount, ok := kodama["serviceAccount"].(map[string]any); ok {
				if recorded, ok := serviceAccount["annotations"].(map[string]any); ok {
					annotations = recorded
				}
			}
			switch testCase.cloud {
			case "gcp":
				if got := annotations["iam.gke.io/gcp-service-account"]; got != testCase.workloadIdentity {
					t.Errorf("rendered GCP workload identity annotation = %v, want %s", got, testCase.workloadIdentity)
				}
			case "aws":
				if got := annotations["eks.amazonaws.com/role-arn"]; got != testCase.workloadIdentity {
					t.Errorf("rendered AWS workload identity annotation = %v, want %s", got, testCase.workloadIdentity)
				}
			default:
				if len(annotations) != 0 {
					t.Errorf("scw rendered unexpected service account annotations: %v", annotations)
				}
			}

			render := func(valuesFile string) (string, error) {
				args := []string{"template", "kodama-stage", chart, "--namespace", "kodama-system", "--values", valuesFile}
				args = append(args, stackProductionArgs()...)
				renderCommand := exec.Command(helm, args...)
				renderCommand.Dir = chart
				rendered, err := renderCommand.CombinedOutput()
				return string(rendered), err
			}
			rendered, err := render(valuesPath)
			if err != nil {
				t.Fatalf("helm template refused the descriptor-derived values for %s: %v\n%s", testCase.cloud, err, rendered)
			}
			// keyAdapter is part of the documented configuration contract; the
			// rendered Deployment must carry the exact reference. Values alone
			// could be dropped by a template regression.
			for _, want := range []string{testCase.adapter, testCase.keyRef} {
				if !strings.Contains(rendered, want) {
					t.Errorf("rendered chart does not contain %q:\n%s", want, rendered)
				}
			}

			// Negative control: the schema rejects an undeclared environment.
			// If this render succeeds, helm validation is off and the positive
			// render above proves nothing about the schema.
			kodama["environment"] = "staging"
			mutated, err := yaml.Marshal(values)
			if err != nil {
				t.Fatalf("encode mutated values: %v", err)
			}
			mutatedPath := filepath.Join(workDir, "values.invalid.yaml")
			writeFile(t, mutatedPath, string(mutated))
			refused, err := render(mutatedPath)
			if err == nil {
				t.Fatalf("helm template accepted an invalid environment; values schema validation is not on:\n%s", refused)
			}
			if !strings.Contains(strings.ToLower(refused), "schema") && !strings.Contains(strings.ToLower(refused), "environment") {
				t.Errorf("the schema refusal does not mention the schema or the invalid environment:\n%s", refused)
			}

			// Environment-driven OIDC extraEnv and digest: render-values must
			// produce a complete production values file that helm template
			// accepts without external --set overrides.
			oidcStateDir := t.TempDir()
			oidcWorkDir := filepath.Join(oidcStateDir, testCase.cloud, "stage")
			if err := os.MkdirAll(oidcWorkDir, 0o700); err != nil {
				t.Fatalf("mkdir %s: %v", oidcWorkDir, err)
			}
			writeFile(t, filepath.Join(oidcWorkDir, "descriptor.json"), testCase.descriptorJSON(t))
			oidcCmd := exec.Command("bash", filepath.Join(repoRoot(t), "scripts", "deploy"), testCase.cloud, "render-values", "--cluster", "stage")
			oidcCmd.Env = append(os.Environ(),
				"KODAMA_DEPLOY_STATE_DIR="+oidcStateDir,
				"KODAMA_IMAGE_REPOSITORY=ghcr.io/example/kodama",
				"KODAMA_IMAGE_DIGEST=sha256:3333333333333333333333333333333333333333333333333333333333333333",
				"KODAMA_OIDC_ISSUER=https://issuer.example",
				"KODAMA_OIDC_AUDIENCE=kodama",
				"KODAMA_OIDC_JWKS_URL=https://issuer.example/jwks",
				"KODAMA_OIDC_TENANT_CLAIM=org_id",
				"KODAMA_OIDC_SUBJECT_CLAIM=sub",
			)
			if out, err := oidcCmd.CombinedOutput(); err != nil {
				t.Fatalf("render-values with OIDC env failed for %s: %v\n%s", testCase.cloud, err, out)
			}
			standaloneCmd := exec.Command(helm, "template", "kodama-stage", chart,
				"--namespace", "kodama-system",
				"--values", filepath.Join(oidcWorkDir, "values.generated.yaml"))
			standaloneCmd.Dir = chart
			if standaloneOut, err := standaloneCmd.CombinedOutput(); err != nil {
				t.Fatalf("helm template rejected standalone OIDC-rendered values for %s: %v\n%s", testCase.cloud, err, standaloneOut)
			}
		})
	}
}

// TestBootstrapSecretsTenantCapPodSecurityAndMixedSecretMerge pins the 49-char
// tenant ID cap, Pod Security Admission labels on bootstrapped tenant
// namespaces, and multi-key merging in create_secret_mixed.
func TestBootstrapSecretsTenantCapPodSecurityAndMixedSecretMerge(t *testing.T) {
	t.Run("accepts_49_char_tenant_and_sets_pod_security_labels", func(t *testing.T) {
		box := newSandbox(t)
		tenant49 := strings.Repeat("a", 49)
		mutateDescriptor(t, box, func(descriptor map[string]any) {
			descriptor["tenants"] = []string{tenant49}
		})
		code, output := box.run(t, false)
		if code != 0 {
			t.Fatalf("bootstrap-secrets rejected a valid 49-char tenant ID: exit %d\n%s", code, output)
		}
		stdin := box.readKubectlStdin(t)
		for _, want := range []string{
			"name: kodama-tenant-" + tenant49,
			"pod-security.kubernetes.io/enforce: baseline",
			"pod-security.kubernetes.io/warn: restricted",
			"pod-security.kubernetes.io/audit: restricted",
		} {
			if !strings.Contains(stdin, want) {
				t.Errorf("reconciled tenant Namespace manifest is missing %q:\n%s", want, stdin)
			}
		}
	})

	t.Run("refuses_50_char_tenant_before_kubectl", func(t *testing.T) {
		box := newSandbox(t)
		mutateDescriptor(t, box, func(descriptor map[string]any) {
			descriptor["tenants"] = []string{strings.Repeat("a", 50)}
		})
		code, output := box.run(t, false)
		if code == 0 {
			t.Fatalf("bootstrap-secrets accepted a 50-char tenant ID:\n%s", output)
		}
		if lines := box.readKubectlArgv(t); len(lines) != 0 {
			t.Errorf("a 50-char tenant ID reached kubectl before being refused:\n%s", strings.Join(lines, "\n"))
		}
	})

	t.Run("create_secret_mixed_merges_multiple_file_keys_without_argv_leak", func(t *testing.T) {
		box := newSandbox(t)
		envFile := filepath.Join(box.tempDir, "mixed.env")
		fileOne := filepath.Join(box.tempDir, "one.pem")
		fileTwo := filepath.Join(box.tempDir, "two.pem")
		writeFile(t, envFile, "database_url=postgres://u@db:5432/kodama\n")
		writeFile(t, fileOne, "FIRST-FILE-SENTINEL-CONTENT-123456")
		writeFile(t, fileTwo, "SECOND-FILE-SENTINEL-CONTENT-654321")

		script := filepath.Join(repoRoot(t), "scripts", "bootstrap-secrets")
		snippet := `
source <(sed -n '/^create_secret_mixed() {/,/^}/p' "$1")
kcmd=(kubectl "--kubeconfig=$2")
create_secret_mixed "kodama-system" "kodama-mixed" "$3" "ca.crt=$4" "extra.crt=$5"
`
		cmd := exec.Command("bash", "-c", snippet, "mixed-test",
			script,
			filepath.Join(box.workDir, "kubeconfig.yaml"),
			envFile,
			fileOne,
			fileTwo,
		)
		cmd.Env = box.env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("create_secret_mixed failed: %v\n%s", err, out)
		}
		stdin := box.readKubectlStdin(t)
		var secret map[string]any
		if err := json.Unmarshal([]byte(stdin), &secret); err != nil {
			t.Fatalf("parse merged Secret JSON: %v\n%s", err, stdin)
		}
		data, ok := secret["data"].(map[string]any)
		if !ok {
			t.Fatalf("merged Secret has no data map:\n%s", stdin)
		}
		if _, ok := data["ca.crt"]; !ok {
			t.Errorf("create_secret_mixed overwrote ca.crt when merging extra.crt: %v", data)
		}
		if _, ok := data["extra.crt"]; !ok {
			t.Errorf("create_secret_mixed did not include extra.crt: %v", data)
		}
		assertSecretFormsAbsentFromAllArgv(t, box, []credentialForm{
			{label: "first file raw", secret: "FIRST-FILE-SENTINEL-CONTENT-123456"},
			{label: "second file raw", secret: "SECOND-FILE-SENTINEL-CONTENT-654321"},
		})
	})
}
