//go:build component

// Adoption tests for the reference bootstrap-secrets candidate (6289127) on
// master. They pin the custody split that the candidate introduces:
//
//   - kodama-keys is created once and carries the KMS-wrapped KEK only; the
//     KMS IAM credentials live in kodama-kms-credentials, reapplied every run;
//   - kodama-database-admin is applied only when TF_VAR_postgres_admin_password
//     is in the environment, and never reaches argv, the log or the disk;
//   - the provider CA is required (no internal-CA fallback);
//   - the four database DSNs are built from percent-encoded passwords, so a
//     password with URL/YAML metacharacters or a newline cannot inject another
//     env-file key or break the DSN;
//   - descriptor-controlled tenant names, namespace and ssl_mode are validated
//     before they reach a kubectl manifest;
//   - the OAuthSonas access code is created from a file, not --from-literal.
package deploy

import (
	"bytes"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// accessCodeStub is the deterministic value fakeOpenssl prints for
// `openssl rand -hex 16`.
const accessCodeStub = "00112233445566778899aabbccddeeff0011223344556677"

const kmsAccessStub = "SCWACCESSKEY0123456789"
const kmsSecretStub = "11111111-2222-3333-4444-555555555555"

// adminPasswordSentinel exercises the full URL-safe set the production
// composition accepts: letters, digits, dot, underscore, tilde and hyphen.
const adminPasswordSentinel = "P28-ADMIN-p.ss~w-rd_100"

// createSecretArgv returns the argv fields of the most recent
// `create secret generic <name>` call, or nil.
func createSecretArgv(lines []string, name string) []string {
	var found []string
	for _, line := range lines {
		fields := strings.Split(line, "\t")
		if strings.Contains(strings.Join(fields, " "), "create secret generic "+name) {
			found = fields
		}
	}
	return found
}

// hasArgPrefix reports whether one argv field starts with prefix.
func hasArgPrefix(fields []string, prefix string) bool {
	for _, field := range fields {
		if strings.HasPrefix(field, prefix) {
			return true
		}
	}
	return false
}

// logHasArgvSequence reports whether any recorded argv line contains want as a
// contiguous sequence of fields. The fake command logs join arguments with tabs
// (kubectl, tofu, openssl, ...), so a whole-line strings.Contains with spaces
// can never match and silently makes an absence check vacuous. Comparing
// strings.Fields keeps the check independent of the separator and of the
// program name that precedes the arguments.
func logHasArgvSequence(body string, want ...string) bool {
	return countArgvSequence(body, want...) > 0
}

// countArgvSequence returns how many recorded argv lines contain want as a
// contiguous field sequence.
func countArgvSequence(body string, want ...string) int {
	count := 0
	for line := range strings.SplitSeq(body, "\n") {
		fields := strings.Fields(line)
		for index := 0; index+len(want) <= len(fields); index++ {
			matched := true
			for offset := range want {
				if fields[index+offset] != want[offset] {
					matched = false
					break
				}
			}
			if matched {
				count++
				break
			}
		}
	}
	return count
}

// kubectlCreatedSecrets returns the names of every Secret the recorded kubectl
// argv created, whatever the Secret type (`generic` or `tls`). It matches on
// fields so a tab/space mismatch cannot hide a create.
func kubectlCreatedSecrets(lines []string) []string {
	var names []string
	for _, line := range lines {
		fields := strings.Fields(line)
		for index := 0; index+3 < len(fields); index++ {
			if fields[index] == "create" && fields[index+1] == "secret" {
				names = append(names, fields[index+3])
				break
			}
		}
	}
	return names
}

// kubectlCreatedSecret reports whether the recorded kubectl argv created the
// named Secret.
func kubectlCreatedSecret(lines []string, name string) bool {
	return slices.Contains(kubectlCreatedSecrets(lines), name)
}

// kubectlCreatedSecretType returns the resource type of the recorded
// `create secret <type> <name>` call for name ("generic", "tls", ...), or "".
func kubectlCreatedSecretType(lines []string, name string) string {
	for _, line := range lines {
		fields := strings.Fields(line)
		for index := 0; index+3 < len(fields); index++ {
			if fields[index] == "create" && fields[index+1] == "secret" && fields[index+3] == name {
				return fields[index+2]
			}
		}
	}
	return ""
}

// kubectlCreatedSecretArgv returns the argv fields of the recorded
// `create secret <type> <name>` call for name, whatever its type.
func kubectlCreatedSecretArgv(lines []string, name string) []string {
	for _, line := range lines {
		fields := strings.Split(line, "\t")
		for index := 0; index+3 < len(fields); index++ {
			if fields[index] == "create" && fields[index+1] == "secret" && fields[index+3] == name {
				return fields
			}
		}
	}
	return nil
}

// clearPKIFixture removes the sandbox's pre-seeded fake PKI files so a run has
// to generate the installation CA and every leaf with the real openssl.
func clearPKIFixture(t *testing.T, box *sandbox) {
	t.Helper()
	pki := filepath.Join(box.workDir, "pki")
	entries, err := os.ReadDir(pki)
	if err != nil {
		t.Fatalf("read PKI fixture dir: %v", err)
	}
	for _, entry := range entries {
		if err := os.Remove(filepath.Join(pki, entry.Name())); err != nil {
			t.Fatalf("remove PKI fixture %s: %v", entry.Name(), err)
		}
	}
}

// installOAuthSonasPKI writes a real installation CA and a leaf signed by it
// into the sandbox PKI directory. Tests that pre-issue the OAuthSonas
// certificate to exercise the reuse path must still hand
// ensure_oauthsonas_cert's staleness check a parsable chain: the script
// verifies a stored leaf against the CA currently on disk with real openssl.
func installOAuthSonasPKI(t *testing.T, box *sandbox) {
	t.Helper()
	pki := filepath.Join(box.workDir, "pki")
	run := func(args ...string) {
		t.Helper()
		output, err := exec.Command(box.realOpenssl, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("openssl %s: %v\n%s", strings.Join(args, " "), err, output)
		}
	}
	run("req", "-x509", "-newkey", "rsa:2048", "-nodes", "-sha256", "-days", "3650",
		"-keyout", filepath.Join(pki, "ca.key"), "-out", filepath.Join(pki, "ca.crt"),
		"-subj", "/CN=kodama-internal-ca")
	run("req", "-newkey", "rsa:2048", "-nodes",
		"-keyout", filepath.Join(pki, "oauthsonas.key"),
		"-out", filepath.Join(pki, "oauthsonas.csr"),
		"-subj", "/CN=oauthsonas.kodama-system.svc.cluster.local")
	writeFile(t, filepath.Join(pki, "oauthsonas-ext.cnf"), "subjectAltName = DNS:oauthsonas\n")
	run("x509", "-req", "-in", filepath.Join(pki, "oauthsonas.csr"),
		"-CA", filepath.Join(pki, "ca.crt"), "-CAkey", filepath.Join(pki, "ca.key"),
		"-CAcreateserial", "-out", filepath.Join(pki, "oauthsonas.crt"),
		"-days", "825", "-sha256", "-extfile", filepath.Join(pki, "oauthsonas-ext.cnf"))
}

// opensslOutput runs the real openssl from the sandbox and returns its output.
func opensslOutput(t *testing.T, box *sandbox, args ...string) string {
	t.Helper()
	output, err := exec.Command(box.realOpenssl, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("openssl %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

// verifyCertificate reports whether certFile verifies against caFile.
func verifyCertificate(t *testing.T, box *sandbox, caFile, certFile string) error {
	t.Helper()
	_, err := exec.Command(box.realOpenssl, "verify", "-CAfile", caFile, certFile).CombinedOutput()
	return err
}

// certExtensionValue returns the value of one certificate extension, with the
// "X509v3 ...:" heading removed.
func certExtensionValue(t *testing.T, box *sandbox, certFile, extension string) string {
	t.Helper()
	body := opensslOutput(t, box, "x509", "-in", certFile, "-noout", "-ext", extension)
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if len(lines) < 2 {
		return ""
	}
	return strings.TrimSpace(strings.Join(lines[1:], " "))
}

// assertArgvCount fails when the recorded argv does not contain want
// occurrences of the field sequence.
func assertArgvCount(t *testing.T, label, body string, want int, sequence ...string) {
	t.Helper()
	if got := countArgvSequence(body, sequence...); got != want {
		t.Errorf("%s: recorded %d %q calls, want %d:\n%s", label, got, sequence, want, body)
	}
}

// assertOAuthSonasCertificate proves the OAuthSonas server certificate carries
// exactly the required names and the serverAuth EKU, and was signed by the
// installation CA rather than another leaf.
func assertOAuthSonasCertificate(t *testing.T, box *sandbox, certFile string) {
	t.Helper()
	const wantCNF = "subjectAltName = DNS:oauthsonas,DNS:oauthsonas.kodama-system,DNS:oauthsonas.kodama-system.svc,DNS:oauthsonas.kodama-system.svc.cluster.local,IP:127.0.0.1\n" +
		"extendedKeyUsage = serverAuth\n"
	cnfPath := filepath.Join(box.workDir, "pki", "oauthsonas-ext.cnf")
	if body, err := os.ReadFile(cnfPath); err != nil {
		t.Fatalf("read OAuthSonas extension config: %v", err)
	} else if string(body) != wantCNF {
		t.Errorf("OAuthSonas extension config =\n%s\nwant exactly\n%s", body, wantCNF)
	}

	san := certExtensionValue(t, box, certFile, "subjectAltName")
	want := []string{
		"DNS:oauthsonas",
		"DNS:oauthsonas.kodama-system",
		"DNS:oauthsonas.kodama-system.svc",
		"DNS:oauthsonas.kodama-system.svc.cluster.local",
		"IP Address:127.0.0.1",
	}
	var got []string
	for part := range strings.SplitSeq(san, ",") {
		got = append(got, strings.TrimSpace(part))
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("certificate SANs %v, want exactly %v", got, want)
	}

	eku := certExtensionValue(t, box, certFile, "extendedKeyUsage")
	if eku != "TLS Web Server Authentication" {
		t.Errorf("certificate EKU %q, want exactly serverAuth (TLS Web Server Authentication)", eku)
	}

	subject := opensslOutput(t, box, "x509", "-in", certFile, "-noout", "-subject")
	if !strings.Contains(subject, "CN=oauthsonas.kodama-system.svc.cluster.local") {
		t.Errorf("certificate subject %q does not name the OAuthSonas service", subject)
	}
	issuer := opensslOutput(t, box, "x509", "-in", certFile, "-noout", "-issuer")
	if !strings.Contains(issuer, "CN=kodama-internal-ca") {
		t.Errorf("certificate issuer %q is not the installation CA", issuer)
	}
}

// assertOAuthSonasSecretTLS proves oauthsonas-tls is created as a Kubernetes
// tls Secret from the generated certificate and key, not as a generic Secret.
func assertOAuthSonasSecretTLS(t *testing.T, box *sandbox) {
	t.Helper()
	lines := box.readKubectlArgv(t)
	if got := kubectlCreatedSecretType(lines, "oauthsonas-tls"); got != "tls" {
		t.Fatalf("oauthsonas-tls was created as type %q, want a tls Secret", got)
	}
	argv := kubectlCreatedSecretArgv(lines, "oauthsonas-tls")
	pki := filepath.Join(box.workDir, "pki")
	if !hasArgPrefix(argv, "--cert="+filepath.Join(pki, "oauthsonas.crt")) {
		t.Errorf("oauthsonas-tls is not created from the generated certificate: %v", argv)
	}
	if !hasArgPrefix(argv, "--key="+filepath.Join(pki, "oauthsonas.key")) {
		t.Errorf("oauthsonas-tls is not created from the generated key: %v", argv)
	}
}

// assertFormsAbsentFromArgv fails when any labelled credential form appears in
// a recorded command line, whole or split across elements.
func assertFormsAbsentFromArgv(t *testing.T, lines []string, forms []credentialForm) {
	t.Helper()
	if len(lines) == 0 {
		t.Fatal("fake kubectl recorded no argv lines")
	}
	for _, line := range lines {
		if hits := argvCredentialHits(line, forms); len(hits) != 0 {
			t.Errorf("%v appears on kubectl argv: %s", hits, line)
		}
	}
}

// assertFormsAbsentFromOutput fails when a credential form appears in the
// script's combined stdout+stderr.
func assertFormsAbsentFromOutput(t *testing.T, output string, forms []credentialForm) {
	t.Helper()
	for _, form := range forms {
		if form.secret != "" && strings.Contains(output, form.secret) {
			t.Errorf("%s appears in the combined stdout+stderr", form.label)
		}
	}
}

// assertFormsAbsentFromTrees fails when a credential form survives on disk.
func assertFormsAbsentFromTrees(t *testing.T, dirs []string, forms []credentialForm) {
	t.Helper()
	needles := make(map[string][]byte, len(forms))
	for _, form := range forms {
		needles[form.label] = []byte(form.secret)
	}
	assertAbsentFromTrees(t, dirs, needles)
}

// assertNoTempFiles fails when a registered temporary path survives the run.
func assertNoTempFiles(t *testing.T, box *sandbox, prefixes ...string) {
	t.Helper()
	for _, dir := range []string{box.workDir, box.tempDir} {
		err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			for _, prefix := range prefixes {
				if strings.HasPrefix(entry.Name(), prefix) {
					t.Errorf("temporary credential file left behind: %s", path)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scan %s: %v", dir, err)
		}
	}
}

// TestBootstrapSecretsSplitsKMSSecretAndCreatesKEKOnce proves the candidate's
// custody split: kodama-keys is created with the KEK ciphertext only and is
// never touched on a second run, while kodama-kms-credentials is applied on
// every run from a file so the IAM key never reaches argv. The KMS credentials
// do not ride in kodama-keys, whose contents the application mounts.
func TestBootstrapSecretsSplitsKMSSecretAndCreatesKEKOnce(t *testing.T) {
	box := newSandbox(t)
	code, output := box.run(t, false)
	if code != 0 {
		t.Fatalf("bootstrap-secrets failed: exit %d\n%s", code, output)
	}

	lines := box.readKubectlArgv(t)
	keys := createSecretArgv(lines, "kodama-keys")
	if keys == nil {
		t.Fatal("no `create secret generic kodama-keys` argv was recorded")
	}
	if !hasArgPrefix(keys, "--from-file=kek-v1.blob=") {
		t.Errorf("kodama-keys is not created from the KEK ciphertext file: %v", keys)
	}
	if hasArgPrefix(keys, "--from-env-file=") {
		t.Errorf("kodama-keys is combined with an env-file; kubectl refuses that combination and the KEK Secret must carry the KEK only: %v", keys)
	}
	if hasArgPrefix(keys, "--from-literal=") {
		t.Errorf("kodama-keys carries a literal value on argv: %v", keys)
	}

	creds := createSecretArgv(lines, "kodama-kms-credentials")
	if creds == nil {
		t.Fatal("no `create secret generic kodama-kms-credentials` argv was recorded")
	}
	if !hasArgPrefix(creds, "--from-env-file=") {
		t.Errorf("kodama-kms-credentials does not read its values from a file: %v", creds)
	}
	assertFormsAbsentFromArgv(t, lines, []credentialForm{
		{label: "KMS access key", secret: kmsAccessStub},
		{label: "KMS secret key", secret: kmsSecretStub},
	})

	body, err := os.ReadFile(box.kubectlSecretLog)
	if err != nil {
		t.Fatalf("read fake kubectl secret log: %v", err)
	}
	if !strings.Contains(string(body), "SCW_ACCESS_KEY="+kmsAccessStub) {
		t.Errorf("fake kubectl never received SCW_ACCESS_KEY from the credentials env-file")
	}
	if !strings.Contains(string(body), "SCW_SECRET_KEY="+kmsSecretStub) {
		t.Errorf("fake kubectl never received SCW_SECRET_KEY from the credentials env-file")
	}
	// The database Secret is created in one request: the provider CA read from
	// the file is merged client-side into the dry-run manifest produced from
	// the env-file, because kubectl refuses --from-env-file together with
	// --from-file. The env-file itself reached kubectl through that dry-run
	// call (asserted above); here the merged request must carry the CA.
	stdin := box.readKubectlStdin(t)
	for _, want := range []string{`"name": "kodama-database"`, `"ca.crt"`} {
		if !strings.Contains(stdin, want) {
			t.Errorf("the combined kodama-database request is missing %s:\n%s", want, stdin)
		}
	}

	assertFormsAbsentFromOutput(t, output, []credentialForm{
		{label: "KMS access key", secret: kmsAccessStub},
		{label: "KMS secret key", secret: kmsSecretStub},
	})
	assertNoTempFiles(t, box, "creds-env.", "admin-env.")

	// Positive control for the second-run check below: the fake tofu log is
	// tab-separated, so the CA read can only be recognized through field
	// comparison. Prove the first run recorded it; otherwise the absence
	// assertion after the second run would prove nothing.
	firstTofu := box.readLogForTest(box.tofuLog)
	if !logHasArgvSequence(firstTofu, "output", "-raw", "database_ca_certificate") {
		t.Fatalf("the first run's tofu log never recorded `output -raw database_ca_certificate`; the second-run absence check would be vacuous:\n%s", firstTofu)
	}

	// Second run against a cluster where every Secret already exists: the KEK
	// Secret is never recreated, the KMS credentials are reapplied.
	box.withEnv("KUBECTL_SECRETS_PRESENT=1")
	box.resetLogs(t)
	code, output = box.run(t, false)
	if code != 0 {
		t.Fatalf("second bootstrap-secrets run failed: exit %d\n%s", code, output)
	}
	lines = box.readKubectlArgv(t)
	if createSecretArgv(lines, "kodama-keys") != nil {
		t.Errorf("kodama-keys was recreated on a second run; a new KEK makes every existing value unreadable")
	}
	if createSecretArgv(lines, "kodama-kms-credentials") == nil {
		t.Errorf("kodama-kms-credentials was not reapplied on a second run")
	}
	tofuBody := box.readLogForTest(box.tofuLog)
	if logHasArgvSequence(tofuBody, "output", "-raw", "database_ca_certificate") {
		t.Errorf("a preserved kodama-database still read the provider CA; the database Secret should be left alone")
	}
	// Positive control for the absence check just above: the KMS credentials
	// are still read on every run, so the log must contain that call and the
	// check cannot pass against an empty or unreadable log.
	if !logHasArgvSequence(tofuBody, "output", "-json", "kms_credentials") {
		t.Errorf("the second run's tofu log does not record the KMS credentials read; the CA absence check would be vacuous:\n%s", tofuBody)
	}
}

// TestBootstrapSecretsAdminSecretOnlyWithPassword proves kodama-database-admin
// is created only when TF_VAR_postgres_admin_password is set, is applied from
// a file, never echoes the password, and works with master's descriptor shape
// (no postgres.admin_database field).
func TestBootstrapSecretsAdminSecretOnlyWithPassword(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		box := newSandbox(t)
		code, output := box.run(t, false)
		if code != 0 {
			t.Fatalf("bootstrap-secrets failed: exit %d\n%s", code, output)
		}
		if argv := createSecretArgv(box.readKubectlArgv(t), "kodama-database-admin"); argv != nil {
			t.Errorf("kodama-database-admin was created without TF_VAR_postgres_admin_password: %v", argv)
		}
		if !strings.Contains(output, "TF_VAR_postgres_admin_password not set") {
			t.Errorf("the run does not record that the admin Secret was left alone: %s", output)
		}
		assertNoTempFiles(t, box, "admin-env.")
	})

	t.Run("present", func(t *testing.T) {
		box := newSandbox(t).withEnv("TF_VAR_postgres_admin_password=" + adminPasswordSentinel)
		code, output := box.run(t, false)
		if code != 0 {
			t.Fatalf("bootstrap-secrets with an admin password failed: exit %d\n%s", code, output)
		}
		lines := box.readKubectlArgv(t)
		argv := createSecretArgv(lines, "kodama-database-admin")
		if argv == nil {
			t.Fatal("kodama-database-admin was not created although TF_VAR_postgres_admin_password is set")
		}
		if !hasArgPrefix(argv, "--from-env-file=") {
			t.Errorf("kodama-database-admin does not read its URL from a file: %v", argv)
		}
		if hasArgPrefix(argv, "--from-literal=") {
			t.Errorf("kodama-database-admin carries a literal on argv: %v", argv)
		}
		forms := []credentialForm{{label: "postgres admin password", secret: adminPasswordSentinel}}
		assertFormsAbsentFromArgv(t, lines, forms)
		assertFormsAbsentFromOutput(t, output, forms)
		assertFormsAbsentFromTrees(t, []string{box.workDir, box.tempDir}, forms)

		body, err := os.ReadFile(box.kubectlSecretLog)
		if err != nil {
			t.Fatalf("read fake kubectl secret log: %v", err)
		}
		secretLog := string(body)
		if strings.Contains(secretLog, "%") {
			t.Errorf("a URL-safe administrator password was percent-encoded: %s", secretLog)
		}
		if !strings.Contains(secretLog, "admin_url=postgres://kodama_admin:"+adminPasswordSentinel+"@") {
			t.Errorf("the administrator password does not reach the env-file unchanged: %s", secretLog)
		}
		if !strings.Contains(secretLog, "admin_url=postgres://kodama_admin:") {
			t.Errorf("admin_url does not use the descriptor's postgres.admin_user: %s", secretLog)
		}
		if !strings.Contains(secretLog, "@db.example:5432/kodama?sslmode=verify-full&sslrootcert=/etc/kodama/pg-ca/ca.crt") {
			t.Errorf("admin_url does not fall back to postgres.database_name or does not use verify-full: %s", secretLog)
		}
		assertNoTempFiles(t, box, "admin-env.")
	})

	t.Run("present_when_database_secret_exists", func(t *testing.T) {
		box := newSandbox(t).
			withEnv("TF_VAR_postgres_admin_password="+adminPasswordSentinel, "KUBECTL_SECRETS_PRESENT=1")
		code, output := box.run(t, false)
		if code != 0 {
			t.Fatalf("bootstrap-secrets failed: exit %d\n%s", code, output)
		}
		if createSecretArgv(box.readKubectlArgv(t), "kodama-database-admin") == nil {
			t.Errorf("kodama-database-admin was not applied although the admin password is set; it must not depend on kodama-database being new")
		}
	})
}

// TestBootstrapSecretsRefusesNonURLSafePasswords proves the production
// composition's password grammar [A-Za-z0-9._~-] is enforced by the script.
// A password that would need percent-encoding (and every URL delimiter or
// newline) is refused before any kubectl or KMS call instead of being written
// into a DSN the server later rejects.
func TestBootstrapSecretsRefusesNonURLSafePasswords(t *testing.T) {
	unsafe := []struct {
		name  string
		value string
	}{
		{name: "userinfo and query delimiters", value: "P28-DB-p@ss:w/rd?#&=1% ok"},
		{name: "newline injection", value: "P28-DB-line1\nINJECTED_KEY=1"},
		{name: "unicode", value: "Pässwörd€-P28"},
		{name: "single quote", value: "P28-DB-p'ass"},
	}
	for _, testCase := range unsafe {
		t.Run(testCase.name, func(t *testing.T) {
			box := newSandbox(t)
			box.env = append(box.env, "KODAMA_DB_RUNTIME_PASSWORD="+testCase.value)
			code, output := box.run(t, false)
			if code == 0 {
				t.Fatalf("bootstrap-secrets accepted a non-URL-safe runtime password:\n%s", output)
			}
			if !strings.Contains(output, "KODAMA_DB_RUNTIME_PASSWORD") {
				t.Errorf("the refusal does not name the offending variable:\n%s", output)
			}
			if !strings.Contains(output, "[A-Za-z0-9._~-]") {
				t.Errorf("the refusal does not name the allowed set:\n%s", output)
			}
			if lines := box.readKubectlArgv(t); len(lines) != 0 {
				t.Errorf("a refused password still reached kubectl: %s", strings.Join(lines, "\n"))
			}
			if stdin := box.readKubectlStdin(t); stdin != "" {
				t.Errorf("a refused password still reached a kubectl manifest:\n%s", stdin)
			}
		})
	}

	for _, variable := range []string{
		"KODAMA_DB_CONTROLLER_PASSWORD",
		"KODAMA_DB_PUBLISHER_PASSWORD",
		"KODAMA_DB_AUTH_PASSWORD",
		"TF_VAR_postgres_admin_password",
	} {
		t.Run(variable, func(t *testing.T) {
			box := newSandbox(t).withEnv(variable + "=" + "P28-DB-p@ss:w/rd")
			code, output := box.run(t, false)
			if code == 0 {
				t.Fatalf("bootstrap-secrets accepted a non-URL-safe %s:\n%s", variable, output)
			}
			if !strings.Contains(output, variable) {
				t.Errorf("the refusal does not name %s:\n%s", variable, output)
			}
			if lines := box.readKubectlArgv(t); len(lines) != 0 {
				t.Errorf("a refused %s still reached kubectl: %s", variable, strings.Join(lines, "\n"))
			}
		})
	}

	// Positive control: the full allowed set is accepted and reaches the
	// env-file verbatim, so the refusals above are not a blanket rejection.
	safe := "P28-DB.p.ss~w-rd_100"
	box := newSandbox(t)
	box.env = append(box.env, "KODAMA_DB_RUNTIME_PASSWORD="+safe)
	code, output := box.run(t, false)
	if code != 0 {
		t.Fatalf("bootstrap-secrets refused a URL-safe password: exit %d\n%s", code, output)
	}
	body, err := os.ReadFile(box.kubectlSecretLog)
	if err != nil {
		t.Fatalf("read fake kubectl secret log: %v", err)
	}
	if !strings.Contains(string(body), "runtime:"+safe+"@") && !strings.Contains(string(body), "kodama_runtime:"+safe+"@") {
		t.Errorf("the URL-safe runtime password did not reach the DSN unchanged: %s", body)
	}
}

// TestBootstrapSecretsRequiresProviderCA proves a missing provider CA is a
// failure, never a fallback to the internal installation CA: trusting the
// internal root for the database would accept the wrong peer certificate.
func TestBootstrapSecretsRequiresProviderCA(t *testing.T) {
	box := newSandbox(t).withEnv("TOFU_FAIL_CA=1")
	code, output := box.run(t, false)
	if code == 0 {
		t.Fatalf("bootstrap-secrets succeeded without the provider CA output:\n%s", output)
	}
	if createSecretArgv(box.readKubectlArgv(t), "kodama-database") != nil {
		t.Errorf("kodama-database was created even though the provider CA was unavailable")
	}
	assertNoTempFiles(t, box, "db-env.", "db-ca.")
	assertDatabaseSecretsCleanedNeedsNoSecrets(t, box)
}

// assertDatabaseSecretsCleanedNeedsNoSecrets proves the fail-closed CA path
// left none of the four passwords on disk, even though the Secret was never
// created. It reads the passwords from the sentinels, not from the env-file.
func assertDatabaseSecretsCleanedNeedsNoSecrets(t *testing.T, box *sandbox) {
	t.Helper()
	needles := make(map[string][]byte, len(dbSentinels))
	for _, sentinel := range dbSentinels {
		needles[sentinel] = []byte(sentinel)
	}
	assertAbsentFromTrees(t, []string{box.workDir, box.tempDir}, needles)
}

// TestBootstrapSecretsValidatesDescriptorBeforeKubectl proves a tenant name or
// release namespace with YAML metacharacters is refused before any manifest is
// applied, instead of being interpolated into a Namespace object. The
// descriptor is produced from operator tfvars; a quote or newline there must
// not become cluster content.
func TestBootstrapSecretsValidatesDescriptorBeforeKubectl(t *testing.T) {
	cases := []struct {
		name        string
		old         string
		replacement string
		want        string
	}{
		{
			name:        "tenant",
			old:         `"tenants": ["tenant-a"]`,
			replacement: "\"tenants\": [\"tenant-a\\\"\\n  evil: true\"]",
			want:        "tenant",
		},
		{
			name:        "namespace",
			old:         `"release_namespace": "kodama-system"`,
			replacement: `"release_namespace": "kodama-system\n---\nevil: true"`,
			want:        "namespace",
		},
	}
	base := `{
  "cloud": "scw",
  "cluster_name": "stage",
  "environment": "production",
  "account_or_project_id": "project-1",
  "region": "fr-par",
  "release_namespace": "kodama-system",
  "postgres": {"host": "db.example", "port": 5432, "database_name": "kodama", "admin_user": "kodama_admin"},
  "kms": {"key_id": "fr-par/00000000-0000-0000-0000-000000000000", "key_ref": "scw-kms:fr-par/00000000-0000-0000-0000-000000000000", "key_adapter": "scw-kms"},
  "kubernetes": {"cluster_name": "stage", "storage_class": "sbs-default"},
  "tenants": ["tenant-a"]
}
`
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			box := newSandbox(t)
			box.writeDescriptor(t, strings.Replace(base, testCase.old, testCase.replacement, 1))
			code, output := box.run(t, false)
			if code == 0 {
				t.Fatalf("bootstrap-secrets accepted an injectable %s:\n%s", testCase.name, output)
			}
			lines := box.readKubectlArgv(t)
			if len(lines) != 0 {
				t.Errorf("an injectable %s reached kubectl before being refused: %s", testCase.name, strings.Join(lines, "\n"))
			}
			for _, line := range lines {
				if strings.Contains(line, "evil") {
					t.Errorf("an injectable %s reached kubectl argv: %s", testCase.name, line)
				}
			}
			if stdin := box.readKubectlStdin(t); stdin != "" {
				t.Errorf("an injectable %s reached a kubectl manifest:\n%s", testCase.name, stdin)
			}
			if !strings.Contains(strings.ToLower(output), testCase.want) {
				t.Errorf("the refusal does not name the invalid %s: %s", testCase.want, output)
			}
		})
	}
}

// TestBootstrapSecretsValidatesSSLMode proves the descriptor's ssl_mode is
// validated and defaults to verify-full. Every production PostgreSQL producer
// must use verified TLS, so the script accepts exactly verify-full; verify-ca
// and every weaker mode are refused before kubectl.
func TestBootstrapSecretsValidatesSSLMode(t *testing.T) {
	t.Run("default_verify_full", func(t *testing.T) {
		box := newSandbox(t)
		code, output := box.run(t, false)
		if code != 0 {
			t.Fatalf("bootstrap-secrets failed: exit %d\n%s", code, output)
		}
		body, err := os.ReadFile(box.kubectlSecretLog)
		if err != nil {
			t.Fatalf("read fake kubectl secret log: %v", err)
		}
		if !strings.Contains(string(body), "sslmode=verify-full&sslrootcert=") {
			t.Errorf("a descriptor without ssl_mode did not default to verify-full: %s", body)
		}
	})

	for _, mode := range []string{"disable", "allow", "prefer", "require", "verify-ca"} {
		t.Run("refuses_"+mode, func(t *testing.T) {
			box := newSandbox(t)
			box.writeDescriptor(t, strings.Replace(readDescriptorForTest(t, box), `"database_name": "kodama"`,
				`"database_name": "kodama", "ssl_mode": "`+mode+`"`, 1))
			code, output := box.run(t, false)
			if code == 0 {
				t.Fatalf("bootstrap-secrets accepted ssl_mode %q:\n%s", mode, output)
			}
			if !strings.Contains(output, "verify-full") {
				t.Errorf("the refusal does not name verify-full:\n%s", output)
			}
			if createSecretArgv(box.readKubectlArgv(t), "kodama-database") != nil {
				t.Errorf("kodama-database was created with ssl_mode %q", mode)
			}
			if stdin := box.readKubectlStdin(t); stdin != "" {
				t.Errorf("ssl_mode %q reached a Secret manifest:\n%s", mode, stdin)
			}
		})
	}

	t.Run("explicit_verify_full", func(t *testing.T) {
		box := newSandbox(t)
		box.writeDescriptor(t, strings.Replace(readDescriptorForTest(t, box), `"database_name": "kodama"`,
			`"database_name": "kodama", "ssl_mode": "verify-full"`, 1))
		code, output := box.run(t, false)
		if code != 0 {
			t.Fatalf("bootstrap-secrets failed: exit %d\n%s", code, output)
		}
		body, err := os.ReadFile(box.kubectlSecretLog)
		if err != nil {
			t.Fatalf("read fake kubectl secret log: %v", err)
		}
		if !strings.Contains(string(body), "sslmode=verify-full&sslrootcert=") {
			t.Errorf("the descriptor's verify-full ssl_mode was not honoured: %s", body)
		}
	})

	t.Run("invalid_refused", func(t *testing.T) {
		box := newSandbox(t)
		box.writeDescriptor(t, strings.Replace(readDescriptorForTest(t, box), `"database_name": "kodama"`,
			`"database_name": "kodama", "ssl_mode": "verify-full&sslrootcert=/x\nEVIL=1"`, 1))
		code, output := box.run(t, false)
		if code == 0 {
			t.Fatalf("bootstrap-secrets accepted an injectable ssl_mode:\n%s", output)
		}
		if createSecretArgv(box.readKubectlArgv(t), "kodama-database") != nil {
			t.Errorf("kodama-database was created with an invalid ssl_mode")
		}
		if lines := box.readKubectlArgv(t); len(lines) != 0 {
			t.Errorf("an invalid ssl_mode reached kubectl before being refused: %s", strings.Join(lines, "\n"))
		}
		if stdin := box.readKubectlStdin(t); stdin != "" {
			t.Errorf("an invalid ssl_mode reached a Secret manifest:\n%s", stdin)
		}
	})
}

// readDescriptorForTest returns the sandbox descriptor as written.
func readDescriptorForTest(t *testing.T, box *sandbox) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(box.workDir, "descriptor.json"))
	if err != nil {
		t.Fatalf("read descriptor: %v", err)
	}
	return string(body)
}

// TestBootstrapSecretsKMSValuesCannotInjectEnvFile proves a KMS credential
// value that decodes to a newline is refused instead of adding a line to the
// credentials env-file. The fake tofu emits a JSON escape, so jq decodes it
// into a real newline.
func TestBootstrapSecretsKMSValuesCannotInjectEnvFile(t *testing.T) {
	box := newSandbox(t).withEnv(`TOFU_KMS_ACCESS=ok\nEVIL=1`)
	code, output := box.run(t, false)
	if code == 0 {
		t.Fatalf("bootstrap-secrets accepted a KMS access key containing a newline:\n%s", output)
	}
	if createSecretArgv(box.readKubectlArgv(t), "kodama-kms-credentials") != nil {
		t.Errorf("kodama-kms-credentials was applied from an injectable value")
	}
	if lines := box.readKubectlArgv(t); len(lines) != 0 {
		t.Errorf("an injectable KMS credential reached kubectl before being refused: %s", strings.Join(lines, "\n"))
	}
	body, err := os.ReadFile(box.kubectlSecretLog)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read fake kubectl secret log: %v", err)
	}
	if strings.Contains(string(body), "EVIL=1") {
		t.Errorf("a newline in a KMS credential injected an env-file key")
	}
}

// preservedSecretNames are the Secrets a second run against an existing
// cluster must leave untouched. kodama-kms-credentials is deliberately absent:
// it is reapplied on every run so an IAM key can be rotated.
var preservedSecretNames = []string{
	"kodama-database",
	"kodama-keys",
	"kodama-tls",
	"kodama-broker",
	"kodama-broker-tls",
	"kodama-workload-ca",
	"oauthsonas-tls",
	"oauthsonas-access",
}

// TestBootstrapSecretsSecondRunPreservesSecrets proves every adopter-owned
// Secret is reconciled once: with KUBECTL_SECRETS_PRESENT=1 (the fake kubectl
// models an existing cluster) a second run creates none of them, generates no
// password, KEK or access code, and never reads the provider CA for a
// preserved kodama-database. Each absence check has a first-run positive
// control, so a space/tab or record-format regression cannot make it vacuous.
func TestBootstrapSecretsSecondRunPreservesSecrets(t *testing.T) {
	box := newSandbox(t).withEnv("KODAMA_WITH_OAUTHSONAS=1")
	// Pre-issue the OAuthSonas certificate so the script stays on its reuse
	// path; the fixture is a real chain because the script verifies it.
	installOAuthSonasPKI(t, box)
	// Let the script generate all four database passwords itself, so an
	// accidentally re-entered database branch is observable both as a create
	// and as openssl work.
	box.unsetEnv(
		"KODAMA_DB_RUNTIME_PASSWORD",
		"KODAMA_DB_CONTROLLER_PASSWORD",
		"KODAMA_DB_PUBLISHER_PASSWORD",
		"KODAMA_DB_AUTH_PASSWORD",
	)

	code, output := box.run(t, false)
	if code != 0 {
		t.Fatalf("first bootstrap-secrets run failed: exit %d\n%s", code, output)
	}
	firstLines := box.readKubectlArgv(t)
	for _, name := range preservedSecretNames {
		if !kubectlCreatedSecret(firstLines, name) {
			t.Fatalf("the first run never created %s; the second-run preservation check for it would be vacuous:\n%s", name, strings.Join(firstLines, "\n"))
		}
	}
	firstTofu := box.readLogForTest(box.tofuLog)
	if !logHasArgvSequence(firstTofu, "output", "-raw", "database_ca_certificate") {
		t.Fatalf("the first run's tofu log never recorded the provider CA read; the second-run absence check would be vacuous:\n%s", firstTofu)
	}
	firstOpenssl := strings.Join(box.readAllArgv(t), "\n")
	if got := countArgvSequence(firstOpenssl, "openssl", "rand", "-hex", "24"); got != 4 {
		t.Fatalf("the first run generated %d database passwords, want 4 (the openssl recorder must observe them for the second-run check to be meaningful):\n%s", got, firstOpenssl)
	}
	if countArgvSequence(firstOpenssl, "openssl", "rand", "32") == 0 {
		t.Fatalf("the first run did not record the KEK generation:\n%s", firstOpenssl)
	}

	box.withEnv("KUBECTL_SECRETS_PRESENT=1")
	box.resetLogs(t)
	code, output = box.run(t, false)
	if code != 0 {
		t.Fatalf("second bootstrap-secrets run failed: exit %d\n%s", code, output)
	}

	lines := box.readKubectlArgv(t)
	created := kubectlCreatedSecrets(lines)
	sort.Strings(created)
	if got := strings.Join(created, ","); got != "kodama-kms-credentials" {
		t.Errorf("a second run against an existing cluster created Secrets %v; only kodama-kms-credentials may be reapplied", created)
	}
	for _, name := range preservedSecretNames {
		if kubectlCreatedSecret(lines, name) {
			t.Errorf("%s was recreated on a second run", name)
		}
	}
	tofuBody := box.readLogForTest(box.tofuLog)
	if logHasArgvSequence(tofuBody, "output", "-raw", "database_ca_certificate") {
		t.Errorf("a preserved kodama-database still read the provider CA:\n%s", tofuBody)
	}
	if !logHasArgvSequence(tofuBody, "output", "-json", "kms_credentials") {
		t.Errorf("the second run's tofu log does not record the KMS credentials read; the CA absence check would be vacuous:\n%s", tofuBody)
	}
	opensslBody := strings.Join(box.readAllArgv(t), "\n")
	if got := countArgvSequence(opensslBody, "openssl", "rand"); got != 0 {
		t.Errorf("a second run invoked openssl rand %d times; no password, KEK or access code may be regenerated:\n%s", got, opensslBody)
	}
	secretLog := box.readLogForTest(box.kubectlSecretLog)
	for _, built := range []string{"database_url=postgres://", "controller_url=postgres://", "publisher_url=postgres://", "auth_url=postgres://"} {
		if strings.Contains(secretLog, built) {
			t.Errorf("a preserved kodama-database env-file was rebuilt (%s)", built)
		}
	}
	if !strings.Contains(secretLog, "SCW_ACCESS_KEY="+kmsAccessStub) {
		t.Errorf("the KMS credentials were not reapplied from their env-file; the database absence check would be vacuous:\n%s", secretLog)
	}
}

// TestBootstrapSecretsRotatePKIRecreatesOnlyPKI proves --rotate-pki is the
// explicit rotation path: with OAuthSonas enabled it recreates the five
// in-cluster PKI Secrets (including oauthsonas-tls) and leaves kodama-database,
// kodama-keys and oauthsonas-access alone, so passwords, the KEK and the
// access code are never regenerated by rotation.
func TestBootstrapSecretsRotatePKIRecreatesOnlyPKI(t *testing.T) {
	box := newSandbox(t).withEnv("KUBECTL_SECRETS_PRESENT=1", "KODAMA_WITH_OAUTHSONAS=1")
	// Start from an existing, parsable leaf so the run cannot pass merely by
	// finding no certificate to rotate.
	installOAuthSonasPKI(t, box)
	box.resetLogs(t)
	// Run under a real deployment umask so the PKI files written by the
	// rotation are subject to the script's own umask handling.
	code, output := box.runArgs(t, false, "022", "--rotate-pki")
	if code != 0 {
		t.Fatalf("bootstrap-secrets --rotate-pki failed: exit %d\n%s", code, output)
	}
	lines := box.readKubectlArgv(t)
	for _, name := range []string{"kodama-tls", "kodama-broker-tls", "kodama-broker", "kodama-workload-ca", "oauthsonas-tls"} {
		if !kubectlCreatedSecret(lines, name) {
			t.Errorf("--rotate-pki did not recreate the PKI Secret %s", name)
		}
	}
	for _, name := range []string{"kodama-database", "kodama-keys", "oauthsonas-access"} {
		if kubectlCreatedSecret(lines, name) {
			t.Errorf("--rotate-pki recreated %s; rotation must not touch passwords, the KEK or the OAuthSonas access code", name)
		}
	}
	created := kubectlCreatedSecrets(lines)
	sort.Strings(created)
	want := []string{
		"kodama-broker",
		"kodama-broker-tls",
		"kodama-kms-credentials",
		"kodama-tls",
		"kodama-workload-ca",
		"oauthsonas-tls",
	}
	if strings.Join(created, ",") != strings.Join(want, ",") {
		t.Errorf("--rotate-pki created Secrets %v, want exactly %v (the five PKI Secrets plus the reapplied KMS credentials)", created, want)
	}
	if got := kubectlCreatedSecretType(lines, "oauthsonas-tls"); got != "tls" {
		t.Errorf("oauthsonas-tls was recreated as type %q, want a tls Secret", got)
	}
	body := strings.Join(box.readAllArgv(t), "\n")
	assertArgvCount(t, "--rotate-pki", body, 0, "openssl", "rand")
	// The existing leaf is replaced even though a plain run would reuse it.
	assertArgvCount(t, "--rotate-pki", body, 1, "openssl", "x509", "-req", "-in", filepath.Join(box.workDir, "pki", "oauthsonas.csr"))
}

// TestBootstrapSecretsRotatePKIWithOAuthSonasRegeneratesPKI pins what
// --rotate-pki does to the local PKI and the OAuthSonas Secrets, run by run:
// a fresh work directory generates the CA once and every leaf, a plain re-run
// generates nothing and keeps every byte, and rotation generates a new CA and
// new leaves, republishes oauthsonas-tls, and never touches the access code.
func TestBootstrapSecretsRotatePKIWithOAuthSonasRegeneratesPKI(t *testing.T) {
	box := newSandbox(t).withEnv("KODAMA_WITH_OAUTHSONAS=1")
	clearPKIFixture(t, box)
	pki := filepath.Join(box.workDir, "pki")
	caPath := filepath.Join(pki, "ca.crt")
	leafPath := filepath.Join(pki, "oauthsonas.crt")
	leafCSR := filepath.Join(pki, "oauthsonas.csr")

	// First run: no CA and no leaf exist, so both are generated.
	code, output := box.run(t, false)
	if code != 0 {
		t.Fatalf("first bootstrap-secrets run failed: exit %d\n%s", code, output)
	}
	first := strings.Join(box.readAllArgv(t), "\n")
	assertArgvCount(t, "first run", first, 1, "openssl", "req", "-x509")
	assertArgvCount(t, "first run", first, 4, "openssl", "x509", "-req", "-in")
	// Positive control for the signing-CA pin: the OAuthSonas leaf alone must
	// be signed with the installation CA, not with another leaf's key.
	assertArgvCount(t, "first run", first, 1,
		"openssl", "x509", "-req", "-in", leafCSR,
		"-CA", caPath, "-CAkey", filepath.Join(pki, "ca.key"))
	lines := box.readKubectlArgv(t)
	if !kubectlCreatedSecret(lines, "oauthsonas-access") {
		t.Fatalf("the first run did not create oauthsonas-access; the later absence checks would be vacuous:\n%s", strings.Join(lines, "\n"))
	}
	assertOAuthSonasCertificate(t, box, leafPath)
	assertOAuthSonasSecretTLS(t, box)
	if err := verifyCertificate(t, box, caPath, leafPath); err != nil {
		t.Errorf("the first-run OAuthSonas certificate does not verify against the generated CA: %v", err)
	}
	caFirst, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatalf("read first-run CA: %v", err)
	}
	leafFirst, err := os.ReadFile(leafPath)
	if err != nil {
		t.Fatalf("read first-run OAuthSonas certificate: %v", err)
	}

	// Plain re-run against an existing cluster: nothing is regenerated.
	box.withEnv("KUBECTL_SECRETS_PRESENT=1")
	box.resetLogs(t)
	code, output = box.run(t, false)
	if code != 0 {
		t.Fatalf("plain re-run failed: exit %d\n%s", code, output)
	}
	rerun := strings.Join(box.readAllArgv(t), "\n")
	assertArgvCount(t, "plain re-run", rerun, 0, "openssl", "req", "-x509")
	assertArgvCount(t, "plain re-run", rerun, 0, "openssl", "x509", "-req", "-in")
	assertArgvCount(t, "plain re-run", rerun, 0, "openssl", "rand")
	// Positive control for the zero verify count on rotation below: a plain
	// re-run does consult the staleness check once, which proves the recorder
	// observes openssl verify calls at all.
	assertArgvCount(t, "plain re-run", rerun, 1,
		"openssl", "verify", "-CAfile", caPath, leafPath)
	if body, err := os.ReadFile(caPath); err != nil {
		t.Fatalf("read CA after re-run: %v", err)
	} else if !bytes.Equal(body, caFirst) {
		t.Errorf("a plain re-run changed the installation CA")
	}
	if body, err := os.ReadFile(leafPath); err != nil {
		t.Fatalf("read OAuthSonas certificate after re-run: %v", err)
	} else if !bytes.Equal(body, leafFirst) {
		t.Errorf("a plain re-run regenerated the OAuthSonas certificate")
	}
	lines = box.readKubectlArgv(t)
	if kubectlCreatedSecret(lines, "oauthsonas-tls") {
		t.Errorf("a plain re-run recreated oauthsonas-tls")
	}
	if kubectlCreatedSecret(lines, "oauthsonas-access") {
		t.Errorf("a plain re-run recreated oauthsonas-access; the access code must stay stable")
	}

	// Rotation: a new CA and new leaves, and oauthsonas-tls is republished.
	box.resetLogs(t)
	code, output = box.runArgs(t, false, "022", "--rotate-pki")
	if code != 0 {
		t.Fatalf("bootstrap-secrets --rotate-pki failed: exit %d\n%s", code, output)
	}
	rotated := strings.Join(box.readAllArgv(t), "\n")
	assertArgvCount(t, "--rotate-pki", rotated, 1, "openssl", "req", "-x509")
	assertArgvCount(t, "--rotate-pki", rotated, 4, "openssl", "x509", "-req", "-in")
	assertArgvCount(t, "--rotate-pki", rotated, 0, "openssl", "rand")
	// Rotation is unconditional: it must not have to inspect the old leaf
	// before deciding to re-issue, so no verify call is made.
	assertArgvCount(t, "--rotate-pki", rotated, 0,
		"openssl", "verify", "-CAfile", caPath, leafPath)
	caSecond, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatalf("read rotated CA: %v", err)
	}
	leafSecond, err := os.ReadFile(leafPath)
	if err != nil {
		t.Fatalf("read rotated OAuthSonas certificate: %v", err)
	}
	if bytes.Equal(caFirst, caSecond) {
		t.Errorf("--rotate-pki left the installation CA unchanged")
	}
	if bytes.Equal(leafFirst, leafSecond) {
		t.Errorf("--rotate-pki left the OAuthSonas certificate unchanged")
	}
	if err := verifyCertificate(t, box, caPath, leafPath); err != nil {
		t.Errorf("the rotated OAuthSonas certificate does not verify against the rotated CA: %v", err)
	}
	previousCA := filepath.Join(box.tempDir, "previous-ca.crt")
	writeFile(t, previousCA, string(caFirst))
	if err := verifyCertificate(t, box, previousCA, leafPath); err == nil {
		t.Errorf("the rotated OAuthSonas certificate still verifies against the previous CA; it was not re-issued")
	}
	assertOAuthSonasCertificate(t, box, leafPath)
	lines = box.readKubectlArgv(t)
	if !kubectlCreatedSecret(lines, "oauthsonas-tls") {
		t.Errorf("--rotate-pki did not republish oauthsonas-tls; the cluster would keep the stale certificate")
	}
	if kubectlCreatedSecret(lines, "oauthsonas-access") {
		t.Errorf("--rotate-pki recreated oauthsonas-access; the access code is not PKI and must stay stable")
	}
	assertOAuthSonasSecretTLS(t, box)
}

// TestBootstrapSecretsOAuthSonasReissuesLeafSignedByStaleCA pins the recovery
// path for a CA rotated while OAuthSonas was disabled: enabling OAuthSonas
// later without --rotate-pki must re-issue a leaf signed by the current CA and
// republish oauthsonas-tls, instead of serving a certificate no peer trusting
// the current CA can verify.
func TestBootstrapSecretsOAuthSonasReissuesLeafSignedByStaleCA(t *testing.T) {
	box := newSandbox(t).withEnv("KODAMA_WITH_OAUTHSONAS=1")
	clearPKIFixture(t, box)
	pki := filepath.Join(box.workDir, "pki")
	caPath := filepath.Join(pki, "ca.crt")
	leafPath := filepath.Join(pki, "oauthsonas.crt")

	code, output := box.run(t, false)
	if code != 0 {
		t.Fatalf("first bootstrap-secrets run failed: exit %d\n%s", code, output)
	}
	caFirst, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatalf("read first-run CA: %v", err)
	}
	leafFirst, err := os.ReadFile(leafPath)
	if err != nil {
		t.Fatalf("read first-run OAuthSonas certificate: %v", err)
	}

	// Rotate the installation CA with OAuthSonas disabled: the leaf on disk is
	// deliberately left signed by the previous CA.
	box.unsetEnv("KODAMA_WITH_OAUTHSONAS")
	box.withEnv("KUBECTL_SECRETS_PRESENT=1")
	box.resetLogs(t)
	code, output = box.runArgs(t, false, "022", "--rotate-pki")
	if code != 0 {
		t.Fatalf("bootstrap-secrets --rotate-pki without OAuthSonas failed: exit %d\n%s", code, output)
	}
	if body, readErr := os.ReadFile(caPath); readErr != nil {
		t.Fatalf("read rotated CA: %v", readErr)
	} else if bytes.Equal(body, caFirst) {
		t.Fatalf("--rotate-pki without OAuthSonas did not rotate the CA; the stale-leaf scenario is not set up")
	}
	if body, readErr := os.ReadFile(leafPath); readErr != nil {
		t.Fatalf("read OAuthSonas certificate after CA rotation: %v", readErr)
	} else if !bytes.Equal(body, leafFirst) {
		t.Errorf("a rotation with OAuthSonas disabled replaced the stored leaf")
	}
	lines := box.readKubectlArgv(t)
	if kubectlCreatedSecret(lines, "oauthsonas-tls") {
		t.Errorf("a rotation with OAuthSonas disabled touched oauthsonas-tls")
	}

	// Enable OAuthSonas again without rotating: the stale leaf must be replaced
	// and the in-cluster tls Secret republished.
	box.withEnv("KODAMA_WITH_OAUTHSONAS=1")
	box.resetLogs(t)
	code, output = box.run(t, false)
	if code != 0 {
		t.Fatalf("enabling OAuthSonas after a CA rotation failed: exit %d\n%s", code, output)
	}
	body := strings.Join(box.readAllArgv(t), "\n")
	assertArgvCount(t, "stale-leaf recovery", body, 1,
		"openssl", "verify", "-CAfile", caPath, leafPath)
	assertArgvCount(t, "stale-leaf recovery", body, 0, "openssl", "req", "-x509")
	assertArgvCount(t, "stale-leaf recovery", body, 1,
		"openssl", "x509", "-req", "-in", filepath.Join(pki, "oauthsonas.csr"))
	assertArgvCount(t, "stale-leaf recovery", body, 0, "openssl", "rand")
	leafSecond, err := os.ReadFile(leafPath)
	if err != nil {
		t.Fatalf("read recovered OAuthSonas certificate: %v", err)
	}
	if bytes.Equal(leafFirst, leafSecond) {
		t.Errorf("the stale OAuthSonas certificate was reused after the CA changed")
	}
	if err := verifyCertificate(t, box, caPath, leafPath); err != nil {
		t.Errorf("the recovered OAuthSonas certificate does not verify against the current CA: %v", err)
	}
	previousCA := filepath.Join(box.tempDir, "previous-ca.crt")
	writeFile(t, previousCA, string(caFirst))
	if err := verifyCertificate(t, box, previousCA, leafPath); err == nil {
		t.Errorf("the recovered OAuthSonas certificate still verifies against the previous CA")
	}
	assertOAuthSonasCertificate(t, box, leafPath)
	lines = box.readKubectlArgv(t)
	if !kubectlCreatedSecret(lines, "oauthsonas-tls") {
		t.Errorf("enabling OAuthSonas after a CA rotation did not republish oauthsonas-tls")
	}
	if kubectlCreatedSecret(lines, "oauthsonas-access") {
		t.Errorf("enabling OAuthSonas after a CA rotation recreated oauthsonas-access; the access code must stay stable")
	}
	assertOAuthSonasSecretTLS(t, box)
}

// TestBootstrapSecretsOAuthSonasAccessCodeUsesFileForm proves the opt-in
// OAuthSonas access code is never placed on argv with --from-literal and its
// 0600 file is removed.
func TestBootstrapSecretsOAuthSonasAccessCodeUsesFileForm(t *testing.T) {
	box := newSandbox(t).withEnv("KODAMA_WITH_OAUTHSONAS=1")
	// Pre-issue a real certificate so ensure_oauthsonas_cert returns early on
	// its reuse path.
	installOAuthSonasPKI(t, box)
	code, output := box.run(t, false)
	if code != 0 {
		t.Fatalf("bootstrap-secrets with OAuthSonas enabled failed: exit %d\n%s", code, output)
	}
	lines := box.readKubectlArgv(t)
	argv := createSecretArgv(lines, "oauthsonas-access")
	if argv == nil {
		t.Fatal("oauthsonas-access was not created")
	}
	for _, field := range argv {
		if strings.HasPrefix(field, "--from-literal=") {
			t.Errorf("oauthsonas-access passes the access code on argv: %s", field)
		}
	}
	if !hasArgPrefix(argv, "--from-file=access_code=") {
		t.Errorf("oauthsonas-access does not read the access code from a file: %v", argv)
	}
	forms := []credentialForm{{label: "OAuthSonas access code", secret: accessCodeStub}}
	assertFormsAbsentFromArgv(t, lines, forms)
	assertFormsAbsentFromOutput(t, output, forms)
	assertFormsAbsentFromTrees(t, []string{box.workDir, box.tempDir}, forms)

	for _, record := range box.kubectlFileRecords(t) {
		if record.key != "access_code" {
			continue
		}
		if record.mode != "600" {
			t.Errorf("access-code source %s mode = %s, want 600", record.path, record.mode)
		}
		if record.dirMode != "700" {
			t.Errorf("access-code source directory mode = %s, want 700", record.dirMode)
		}
	}
}

// publishedOAuthSonasCert returns the certificate the fake kubectl models as
// published in the oauthsonas-tls Secret.
func (b *sandbox) publishedOAuthSonasCert(t *testing.T) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(b.kubectlStateDir, "oauthsonas-tls.tls.crt"))
	if err != nil {
		t.Fatalf("read modelled oauthsonas-tls Secret: %v", err)
	}
	decoded, decodeErr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(body)))
	if decodeErr != nil {
		t.Fatalf("decode modelled oauthsonas-tls tls.crt: %v", decodeErr)
	}
	return decoded
}

// TestBootstrapSecretsOAuthSonasReissuesMissingLeafPieces pins the
// "missing leaf, key or CA = stale" rule of oauthsonas_leaf_matches_ca. A
// plain re-run (no --rotate-pki) against a cluster that already carries the
// PKI Secrets must re-issue the local leaf and republish oauthsonas-tls;
// re-issuing the leaf without republishing leaves the cluster serving a
// certificate that only the local disk has.
//
// Removing ca.crt is different: generate_cluster_pki's reuse gate requires it,
// so the whole PKI (CA and three leaves) is regenerated first and the local
// leaf is stale against the new CA. That case pins the full-regeneration
// counts. The other in-cluster PKI Secrets are preserved by their own gate on
// a plain run even though the local CA changed; that behaviour is not asserted
// here.
func TestBootstrapSecretsOAuthSonasReissuesMissingLeafPieces(t *testing.T) {
	cases := []struct {
		name          string
		remove        string
		wantCAChanged bool
		wantPKIGen    int
		wantCSR       int
		wantSign      int
		wantVerify    int
	}{
		{name: "leaf", remove: "oauthsonas.crt", wantCSR: 1, wantSign: 1, wantVerify: 0},
		{name: "key", remove: "oauthsonas.key", wantCSR: 1, wantSign: 1, wantVerify: 0},
		{name: "ca", remove: "ca.crt", wantCAChanged: true, wantPKIGen: 1, wantCSR: 4, wantSign: 4, wantVerify: 1},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			box := newSandbox(t).withEnv("KODAMA_WITH_OAUTHSONAS=1")
			clearPKIFixture(t, box)
			pki := filepath.Join(box.workDir, "pki")
			caPath := filepath.Join(pki, "ca.crt")
			leafPath := filepath.Join(pki, "oauthsonas.crt")

			// First run: a fresh work directory and a fresh cluster.
			code, output := box.run(t, false)
			if code != 0 {
				t.Fatalf("first bootstrap-secrets run failed: exit %d\n%s", code, output)
			}
			firstLines := box.readKubectlArgv(t)
			for _, name := range []string{"oauthsonas-tls", "oauthsonas-access"} {
				if !kubectlCreatedSecret(firstLines, name) {
					t.Fatalf("the first run never created %s; the reissue check would be vacuous:\n%s", name, strings.Join(firstLines, "\n"))
				}
			}
			caFirst, err := os.ReadFile(caPath)
			if err != nil {
				t.Fatalf("read first-run CA: %v", err)
			}
			leafFirst, err := os.ReadFile(leafPath)
			if err != nil {
				t.Fatalf("read first-run OAuthSonas certificate: %v", err)
			}
			if published := box.publishedOAuthSonasCert(t); !bytes.Equal(published, leafFirst) {
				t.Fatalf("the first run published a certificate other than the local leaf; the later republish check would be meaningless")
			}

			if err := os.Remove(filepath.Join(pki, testCase.remove)); err != nil {
				t.Fatalf("remove %s: %v", testCase.remove, err)
			}
			box.withEnv("KUBECTL_SECRETS_PRESENT=1")
			box.resetLogs(t)
			code, output = box.run(t, false)
			if code != 0 {
				t.Fatalf("plain re-run after removing %s failed: exit %d\n%s", testCase.remove, code, output)
			}

			caSecond, err := os.ReadFile(caPath)
			if err != nil {
				t.Fatalf("read CA after re-run: %v", err)
			}
			leafSecond, err := os.ReadFile(leafPath)
			if err != nil {
				t.Fatalf("read OAuthSonas certificate after re-run: %v", err)
			}
			if bytes.Equal(leafFirst, leafSecond) {
				t.Errorf("removing %s did not re-issue the OAuthSonas certificate", testCase.remove)
			}
			if testCase.wantCAChanged {
				if bytes.Equal(caFirst, caSecond) {
					t.Errorf("removing %s did not regenerate the installation CA", testCase.remove)
				}
				previousCA := filepath.Join(box.tempDir, "previous-ca.crt")
				writeFile(t, previousCA, string(caFirst))
				if err := verifyCertificate(t, box, previousCA, leafPath); err == nil {
					t.Errorf("the re-issued OAuthSonas certificate still verifies against the previous CA")
				}
			}
			if err := verifyCertificate(t, box, caPath, leafPath); err != nil {
				t.Errorf("the re-issued OAuthSonas certificate does not verify against the current CA: %v", err)
			}
			assertOAuthSonasCertificate(t, box, leafPath)
			assertOAuthSonasSecretTLS(t, box)
			if published := box.publishedOAuthSonasCert(t); !bytes.Equal(published, leafSecond) {
				t.Errorf("removing %s re-issued the local leaf but left the cluster Secret carrying a different certificate", testCase.remove)
			}

			lines := box.readKubectlArgv(t)
			if kubectlCreatedSecret(lines, "oauthsonas-access") {
				t.Errorf("removing %s recreated oauthsonas-access; the access code must stay stable", testCase.remove)
			}
			all := strings.Join(box.readAllArgv(t), "\n")
			assertArgvCount(t, "missing "+testCase.remove, all, testCase.wantPKIGen, "openssl", "req", "-x509")
			assertArgvCount(t, "missing "+testCase.remove, all, testCase.wantCSR, "openssl", "req", "-newkey")
			assertArgvCount(t, "missing "+testCase.remove, all, testCase.wantSign, "openssl", "x509", "-req", "-in")
			assertArgvCount(t, "missing "+testCase.remove, all, testCase.wantVerify,
				"openssl", "verify", "-CAfile", caPath, leafPath)
		})
	}
}

// Known limitation, not pinned: a publish that fails after the OAuthSonas
// leaf was re-issued leaves the cluster serving the previous certificate. The
// local leaf still verifies against the current CA, so a plain re-run preserves
// the in-cluster Secret and the divergence persists. Recovery is to re-run with
// --rotate-pki, which re-issues and republishes oauthsonas-tls. The plain
// re-run deliberately compares no cluster leaf with the local one; it keeps
// its preservation behaviour.

// TestBootstrapSecretsFailsWhenOAuthSonasLeafSigningFails proves the
// OAuthSonas issue step is not masked: a failing `openssl x509 -req` exits the
// script non-zero and never reaches `kubectl create secret tls`. A `|| true`
// on ensure_oauthsonas_cert would otherwise publish a missing or stale
// certificate.
func TestBootstrapSecretsFailsWhenOAuthSonasLeafSigningFails(t *testing.T) {
	box := newSandbox(t).withEnv("KODAMA_WITH_OAUTHSONAS=1", "OPENSSL_FAIL_OAUTHSONAS=1")
	// A real, reusable CA and leaf: the failure must come from the deliberate
	// signing fault, not from an unparsable fixture.
	installOAuthSonasPKI(t, box)
	pki := filepath.Join(box.workDir, "pki")
	if err := os.Remove(filepath.Join(pki, "oauthsonas.crt")); err != nil {
		t.Fatalf("remove the stored leaf: %v", err)
	}

	code, output := box.run(t, false)
	if code == 0 {
		t.Fatalf("bootstrap-secrets succeeded although the OAuthSonas leaf signing failed:\n%s", output)
	}
	lines := box.readKubectlArgv(t)
	if kubectlCreatedSecret(lines, "oauthsonas-tls") {
		t.Errorf("a failed OAuthSonas leaf signing still created oauthsonas-tls:\n%s", strings.Join(lines, "\n"))
	}
	if kubectlCreatedSecret(lines, "oauthsonas-access") {
		t.Errorf("a failed OAuthSonas leaf signing still created oauthsonas-access")
	}
	all := strings.Join(box.readAllArgv(t), "\n")
	assertArgvCount(t, "failed leaf signing", all, 1,
		"openssl", "x509", "-req", "-in", filepath.Join(pki, "oauthsonas.csr"))
}

// TestBootstrapSecretsPKIFilesArePrivate pins the umask of both PKI subshells:
// under a permissive 000 or an ordinary 022 deployment umask, every file
// generate_cluster_pki and ensure_oauthsonas_cert write is private (no
// group/other bits) and every private key is exactly 0600. Removing either
// `umask 077` leaks the extension config and CSR at 0644 (the keys stay 0600
// because openssl creates them that way on its own).
func TestBootstrapSecretsPKIFilesArePrivate(t *testing.T) {
	for _, umask := range []string{"000", "022"} {
		t.Run("umask_"+umask, func(t *testing.T) {
			box := newSandbox(t).withEnv("KODAMA_WITH_OAUTHSONAS=1")
			clearPKIFixture(t, box)
			code, output := box.runArgs(t, false, umask)
			if code != 0 {
				t.Fatalf("bootstrap-secrets failed under umask %s: exit %d\n%s", umask, code, output)
			}
			pki := filepath.Join(box.workDir, "pki")
			entries, err := os.ReadDir(pki)
			if err != nil {
				t.Fatalf("read PKI directory: %v", err)
			}
			keys := 0
			for _, entry := range entries {
				info, infoErr := entry.Info()
				if infoErr != nil {
					t.Fatalf("stat PKI file %s: %v", entry.Name(), infoErr)
				}
				mode := info.Mode().Perm()
				if mode&0o077 != 0 {
					t.Errorf("umask %s: %s mode = %o; PKI files must not be group/other accessible", umask, entry.Name(), mode)
				}
				if strings.HasSuffix(entry.Name(), ".key") {
					keys++
					if mode != 0o600 {
						t.Errorf("umask %s: private key %s mode = %o, want 600", umask, entry.Name(), mode)
					}
				}
			}
			if keys != 5 {
				t.Errorf("umask %s: found %d private keys, want 5 (CA, kodama, nats, client, oauthsonas)", umask, keys)
			}
			// Positive control: the files whose modes were checked are the
			// generated PKI, not an empty directory.
			for _, name := range []string{"ca.crt", "kodama.crt", "nats.crt", "client.crt", "oauthsonas.crt"} {
				if _, statErr := os.Stat(filepath.Join(pki, name)); statErr != nil {
					t.Errorf("umask %s: %s was not generated: %v", umask, name, statErr)
				}
			}
		})
	}
}

// parseOpenSSLDate parses one `openssl x509 -dates` value, for example
// "Nov  7 20:00:00 2026 GMT".
func parseOpenSSLDate(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse("Jan _2 15:04:05 2006 MST", strings.TrimSpace(value))
	if err != nil {
		t.Fatalf("parse openssl date %q: %v", value, err)
	}
	return parsed
}

// TestBootstrapSecretsPKILeafValidityIs825Days pins the leaf lifetime both in
// the signing argv and in the certificate the run actually produces.
func TestBootstrapSecretsPKILeafValidityIs825Days(t *testing.T) {
	box := newSandbox(t).withEnv("KODAMA_WITH_OAUTHSONAS=1")
	clearPKIFixture(t, box)
	code, output := box.run(t, false)
	if code != 0 {
		t.Fatalf("bootstrap-secrets failed: exit %d\n%s", code, output)
	}
	all := strings.Join(box.readAllArgv(t), "\n")
	assertArgvCount(t, "leaf validity", all, 4, "-days", "825")

	pki := filepath.Join(box.workDir, "pki")
	for _, name := range []string{"kodama", "nats", "client", "oauthsonas"} {
		certPath := filepath.Join(pki, name+".crt")
		body := opensslOutput(t, box, "x509", "-in", certPath, "-noout", "-dates")
		var notBefore, notAfter time.Time
		for line := range strings.SplitSeq(body, "\n") {
			switch {
			case strings.HasPrefix(line, "notBefore="):
				notBefore = parseOpenSSLDate(t, strings.TrimPrefix(line, "notBefore="))
			case strings.HasPrefix(line, "notAfter="):
				notAfter = parseOpenSSLDate(t, strings.TrimPrefix(line, "notAfter="))
			}
		}
		if notBefore.IsZero() || notAfter.IsZero() {
			t.Fatalf("%s: could not read validity dates:\n%s", name, body)
		}
		want := 825 * 24 * time.Hour
		if got := notAfter.Sub(notBefore); got < want-time.Hour || got > want+time.Hour {
			t.Errorf("%s certificate validity = %v, want %v", name, got, want)
		}
	}
}

// TestBootstrapSecretsRegeneratesPKIWhenNATSCertMissing pins nats.crt as part
// of generate_cluster_pki's reuse gate. Deleting it makes a plain re-run
// regenerate the whole PKI (CA and three leaves) and re-issue the OAuthSonas
// leaf because the CA changed. The four in-cluster PKI Secrets are preserved:
// their publish gate is independent of the local leaf gate, so a plain run
// that does not rotate keeps them (pinned here so a change to that gate is
// deliberate).
func TestBootstrapSecretsRegeneratesPKIWhenNATSCertMissing(t *testing.T) {
	box := newSandbox(t).withEnv("KODAMA_WITH_OAUTHSONAS=1")
	clearPKIFixture(t, box)
	pki := filepath.Join(box.workDir, "pki")
	caPath := filepath.Join(pki, "ca.crt")
	natsPath := filepath.Join(pki, "nats.crt")

	code, output := box.run(t, false)
	if code != 0 {
		t.Fatalf("first bootstrap-secrets run failed: exit %d\n%s", code, output)
	}
	caFirst, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatalf("read first-run CA: %v", err)
	}
	natsFirst, err := os.ReadFile(natsPath)
	if err != nil {
		t.Fatalf("read first-run NATS certificate: %v", err)
	}

	if err := os.Remove(natsPath); err != nil {
		t.Fatalf("remove nats.crt: %v", err)
	}
	box.withEnv("KUBECTL_SECRETS_PRESENT=1")
	box.resetLogs(t)
	code, output = box.run(t, false)
	if code != 0 {
		t.Fatalf("plain re-run after removing nats.crt failed: exit %d\n%s", code, output)
	}

	caSecond, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatalf("read CA after re-run: %v", err)
	}
	if bytes.Equal(caFirst, caSecond) {
		t.Errorf("removing nats.crt did not regenerate the installation CA; nats.crt is missing from the reuse gate")
	}
	natsSecond, err := os.ReadFile(natsPath)
	if err != nil {
		t.Fatalf("read NATS certificate after re-run: %v", err)
	}
	if bytes.Equal(natsFirst, natsSecond) {
		t.Errorf("removing nats.crt did not re-issue it")
	}
	if err := verifyCertificate(t, box, caPath, natsPath); err != nil {
		t.Errorf("the regenerated NATS certificate does not verify against the current CA: %v", err)
	}

	all := strings.Join(box.readAllArgv(t), "\n")
	assertArgvCount(t, "nats.crt removed", all, 1, "openssl", "req", "-x509")
	assertArgvCount(t, "nats.crt removed", all, 4, "openssl", "x509", "-req", "-in")
	assertArgvCount(t, "nats.crt removed", all, 1,
		"openssl", "verify", "-CAfile", caPath, filepath.Join(pki, "oauthsonas.crt"))

	lines := box.readKubectlArgv(t)
	if !kubectlCreatedSecret(lines, "oauthsonas-tls") {
		t.Errorf("the regenerated CA did not republish oauthsonas-tls")
	}
	if kubectlCreatedSecret(lines, "oauthsonas-access") {
		t.Errorf("a plain re-run recreated oauthsonas-access; the access code must stay stable")
	}
	for _, name := range []string{"kodama-tls", "kodama-broker-tls", "kodama-broker", "kodama-workload-ca"} {
		if kubectlCreatedSecret(lines, name) {
			t.Errorf("a plain re-run republished %s although --rotate-pki was not requested", name)
		}
	}
}
