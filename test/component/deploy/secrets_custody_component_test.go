//go:build component

// Credential-custody regression tests:
//
//   - no secret is an argument of any process (jq --arg, --argjson, printf in
//     a command's argv, expect, curl -d, kubectl --from-literal ...);
//   - all four database DSNs and the administrator URL encode every URL
//     metacharacter and decode back to the exact password;
//   - every secret-bearing temporary file is 0600 inside a 0700 directory at
//     the moment kubectl reads it, and is removed on failure and on a signal;
//   - every descriptor-controlled value is validated before the first kubectl
//     call, including absent ("null") fields and values that would rewrite the
//     DSN (a database name such as "kodama?sslmode=disable");
//   - the image build context keeps key, certificate, kubeconfig, tfvars and
//     environment files out of the image.
package deploy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// jqURIEncode is jq's @uri: everything except ALPHA / DIGIT / "-" / "." / "_"
// / "~" is percent-encoded with uppercase hex.
func jqURIEncode(value string) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	var encoded strings.Builder
	for index := 0; index < len(value); index++ {
		character := value[index]
		if strings.IndexByte(unreserved, character) >= 0 {
			encoded.WriteByte(character)
			continue
		}
		fmt.Fprintf(&encoded, "%%%02X", character)
	}
	return encoded.String()
}

// TestURLEncodeIsExactAndNewlineSafe drives the real url_encode shell function
// and proves the replacement for `jq -rn --arg` produces exactly the
// percent-encoding of its input with no extra line terminator, including
// embedded and trailing newlines, spaces and non-ASCII bytes. A leak through
// --arg is covered separately by the all-argv assertions.
func TestURLEncodeIsExactAndNewlineSafe(t *testing.T) {
	script := filepath.Join(repoRoot(t), "scripts", "bootstrap-secrets")
	program := fmt.Sprintf(`
source <(sed -n '/^url_encode() {/,/^}/p' %q)
printf '%%s' "$(url_encode "$1")"
`, script)
	for _, input := range []string{
		"P28-DB-p@ss:w/rd?#&=1% ok",
		"line1\nline2",
		"trailing-newline\n",
		"trailing-double-newline\n\n",
		"\nleading-newline",
		"\n",
		"pässwörd€",
		"abc-._~XYZ019",
		"!$'()*+,;[]{}|\\^`\"<>",
	} {
		command := exec.Command("bash", "-c", program, "url-encode-test", input)
		output, err := command.Output()
		if err != nil {
			t.Fatalf("url_encode(%q) failed: %v", input, err)
		}
		if got, want := string(output), jqURIEncode(input); got != want {
			t.Errorf("url_encode(%q) = %q, want %q", input, got, want)
		}
	}
}

// envFileRecord is one --from-env-file the fake kubectl consumed, including
// the file and directory modes observed at read time.
type envFileRecord struct {
	path    string
	mode    string
	dirMode string
	values  map[string]string
}

// envFileRecords parses the fake kubectl secret log.
func (b *sandbox) envFileRecords(t *testing.T) []envFileRecord {
	t.Helper()
	body, err := os.ReadFile(b.kubectlSecretLog)
	if err != nil {
		t.Fatalf("read fake kubectl secret log: %v", err)
	}
	var records []envFileRecord
	var current envFileRecord
	inRecord := false
	for line := range strings.SplitSeq(string(body), "\n") {
		switch {
		case strings.HasPrefix(line, "env_file_begin="):
			current = envFileRecord{
				path:   strings.TrimPrefix(line, "env_file_begin="),
				values: map[string]string{},
			}
			inRecord = true
		case strings.HasPrefix(line, "env_file_mode="):
			current.mode = strings.TrimPrefix(line, "env_file_mode=")
		case strings.HasPrefix(line, "env_file_dir_mode="):
			current.dirMode = strings.TrimPrefix(line, "env_file_dir_mode=")
		case line == "env_file_end":
			records = append(records, current)
			inRecord = false
		default:
			if !inRecord {
				continue
			}
			key, value, found := strings.Cut(line, "=")
			if found {
				current.values[key] = value
			}
		}
	}
	return records
}

// dsnRecord returns the env-file that carries key, or fails the test.
func dsnRecord(t *testing.T, records []envFileRecord, key string) envFileRecord {
	t.Helper()
	for _, record := range records {
		if _, found := record.values[key]; found {
			return record
		}
	}
	t.Fatalf("no env-file carried %s", key)
	return envFileRecord{}
}

// assertDSN parses raw and proves the userinfo password survives the
// percent-encoding exactly, and that the encoding did not move the host, port,
// database or TLS parameters.
func assertDSN(t *testing.T, label, raw, wantUser, wantPassword, wantHost, wantPort, wantDatabase string) {
	t.Helper()
	if strings.ContainsAny(raw, "\n\r") {
		t.Errorf("%s contains a raw line break: %q", label, raw)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("%s does not parse as a URL: %v (%q)", label, err, raw)
	}
	if parsed.Scheme != "postgres" {
		t.Errorf("%s scheme = %q, want postgres", label, parsed.Scheme)
	}
	if got := parsed.User.Username(); got != wantUser {
		t.Errorf("%s user = %q, want %q", label, got, wantUser)
	}
	password, hasPassword := parsed.User.Password()
	if !hasPassword {
		t.Fatalf("%s has no password component: %q", label, raw)
	}
	if password != wantPassword {
		t.Errorf("%s password decodes to %q, want the original password", label, password)
	}
	if got := parsed.Hostname(); got != wantHost {
		t.Errorf("%s host = %q, want %q", label, got, wantHost)
	}
	if got := parsed.Port(); got != wantPort {
		t.Errorf("%s port = %q, want %q", label, got, wantPort)
	}
	if got := parsed.Path; got != "/"+wantDatabase {
		t.Errorf("%s path = %q, want %q", label, got, "/"+wantDatabase)
	}
	query := parsed.Query()
	if got := query.Get("sslmode"); got != "verify-full" {
		t.Errorf("%s sslmode = %q, want verify-full", label, got)
	}
	if got := query.Get("sslrootcert"); got != "/etc/kodama/pg-ca/ca.crt" {
		t.Errorf("%s sslrootcert = %q, want the mounted provider CA", label, got)
	}
	if len(query) != 2 {
		t.Errorf("%s has %d query parameters, want exactly sslmode and sslrootcert: %q", label, len(query), raw)
	}
}

// TestBootstrapSecretsAcceptsURLSafePasswordsEverywhere proves an operator
// password from the URL-safe set [A-Za-z0-9._~-] reaches every database URL
// unchanged and still parses with the intended coordinates. The production
// composition refuses a password that needs percent-encoding, so
// bootstrap-secrets refuses those instead (the refusal is pinned in
// secrets_adoption_component_test.go).
func TestBootstrapSecretsAcceptsURLSafePasswordsEverywhere(t *testing.T) {
	passwords := []struct {
		name  string
		value string
	}{
		{name: "allowed punctuation", value: "P28-DB.p.ss~w-rd_100"},
		{name: "hex", value: "4c1f9a7e8e2b5d106a7d3f921b9e4c65"},
		{name: "digits", value: "0123456789"},
	}
	roles := []struct {
		key  string
		user string
	}{
		{key: "database_url", user: "kodama_runtime"},
		{key: "controller_url", user: "kodama_controller"},
		{key: "publisher_url", user: "kodama_publisher"},
		{key: "auth_url", user: "kodama_auth"},
	}
	for _, password := range passwords {
		t.Run(password.name, func(t *testing.T) {
			box := newSandbox(t).
				withEnv(
					"KODAMA_DB_RUNTIME_PASSWORD="+password.value,
					"KODAMA_DB_CONTROLLER_PASSWORD="+password.value,
					"KODAMA_DB_PUBLISHER_PASSWORD="+password.value,
					"KODAMA_DB_AUTH_PASSWORD="+password.value,
					"TF_VAR_postgres_admin_password="+password.value,
				)
			code, output := box.run(t, false)
			if code != 0 {
				t.Fatalf("bootstrap-secrets failed: exit %d\n%s", code, output)
			}
			records := box.envFileRecords(t)
			for _, role := range roles {
				record := dsnRecord(t, records, role.key)
				assertDSN(t, role.key, record.values[role.key], role.user, password.value, "db.example", "5432", "kodama")
				if record.mode != "600" {
					t.Errorf("%s env-file %s mode = %s, want 600", role.key, record.path, record.mode)
				}
				if record.dirMode != "700" {
					t.Errorf("%s env-file directory mode = %s, want 700", role.key, record.dirMode)
				}
			}
			admin := dsnRecord(t, records, "admin_url")
			assertDSN(t, "admin_url", admin.values["admin_url"], "kodama_admin", password.value, "db.example", "5432", "kodama")
			if admin.mode != "600" {
				t.Errorf("admin env-file %s mode = %s, want 600", admin.path, admin.mode)
			}
			if admin.dirMode != "700" {
				t.Errorf("admin env-file directory mode = %s, want 700", admin.dirMode)
			}
			if got := strings.Count(admin.values["admin_url"], "\n"); got != 0 {
				t.Errorf("admin_url contains a line break")
			}
			assertFormsAbsentFromOutput(t, output, []credentialForm{{label: "database password", secret: password.value}})
		})
	}
}

// TestBootstrapSecretsEnvFilesArePrivateAtReadTime proves every secret-bearing
// env-file (database DSNs, administrator URL, KMS credentials) is 0600 inside a
// 0700 directory at the moment kubectl reads it, not merely after the script's
// later chmod. A chmod regression to 0644 is observable here.
func TestBootstrapSecretsEnvFilesArePrivateAtReadTime(t *testing.T) {
	box := newSandbox(t).withEnv("TF_VAR_postgres_admin_password=" + adminPasswordSentinel)
	code, output := box.run(t, false)
	if code != 0 {
		t.Fatalf("bootstrap-secrets failed: exit %d\n%s", code, output)
	}
	seen := map[string]bool{}
	for _, record := range box.envFileRecords(t) {
		base := filepath.Base(record.path)
		for _, prefix := range []string{"db-env.", "admin-env.", "creds-env."} {
			if !strings.HasPrefix(base, prefix) {
				continue
			}
			seen[prefix] = true
			if record.mode != "600" {
				t.Errorf("%s source %s mode = %s when kubectl read it, want 600", prefix, record.path, record.mode)
			}
			if record.dirMode != "700" {
				t.Errorf("%s source directory mode = %s, want 700", prefix, record.dirMode)
			}
		}
	}
	for _, prefix := range []string{"db-env.", "admin-env.", "creds-env."} {
		if !seen[prefix] {
			t.Errorf("fake kubectl never read a %s file; the mode check would be vacuous", prefix)
		}
	}
}

// TestBootstrapSecretsNeverPassesAnySecretOnArgv runs the script with every
// credential source enabled (four database passwords, the administrator
// password, the KMS IAM key and the OAuthSonas access code) and scans the argv
// of every wrapped command. The KEK is checked in base64, hex and raw form.
func TestBootstrapSecretsNeverPassesAnySecretOnArgv(t *testing.T) {
	box := newSandbox(t).
		withEnv("TF_VAR_postgres_admin_password="+adminPasswordSentinel, "KODAMA_WITH_OAUTHSONAS=1")
	installOAuthSonasPKI(t, box)
	code, output := box.run(t, false)
	if code != 0 {
		t.Fatalf("bootstrap-secrets failed: exit %d\n%s", code, output)
	}
	forms := kubectlCredentialForms(stubKEKMaterial(), dbForms())
	// create_secret_mixed merges the CA file into the kodama-database manifest.
	// Its bytes must reach jq on stdin: an `--arg v "$(base64 ...)"`
	// form puts the base64 value on jq's argv. The CA content is known (fake
	// tofu emits TOFU_CA_PEM plus a newline), so both the raw and base64 forms
	// are needles for the all-argv scan.
	caContent := "fake-provider-ca\n"
	forms = append(forms,
		credentialForm{label: "database CA (raw)", secret: "fake-provider-ca"},
		credentialForm{label: "database CA (base64)", secret: base64.StdEncoding.EncodeToString([]byte(caContent))},
	)
	forms = append(forms,
		credentialForm{label: "postgres admin password", secret: adminPasswordSentinel},
		credentialForm{label: "KMS access key", secret: kmsAccessStub},
		credentialForm{label: "KMS secret key", secret: kmsSecretStub},
		credentialForm{label: "OAuthSonas access code", secret: accessCodeStub},
	)
	assertEveryRecorderRan(t, box)
	assertSecretFormsAbsentFromAllArgv(t, box, forms)
	assertFormsAbsentFromOutput(t, output, forms)
}

// TestBootstrapSecretsCleansCredentialFilesWhenKubectlFails proves the three
// credential temp files are registered for cleanup:
// a kubectl failure after the file exists but before the Secret is applied must
// leave neither admin-env.*, creds-env.* nor oauth-access.* behind.
func TestBootstrapSecretsCleansCredentialFilesWhenKubectlFails(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		prefix  string
		admin   bool
		oauth   bool
	}{
		{name: "admin_env_file", pattern: "create secret generic kodama-database-admin", prefix: "admin-env.", admin: true},
		{name: "kms_credentials_env_file", pattern: "create secret generic kodama-kms-credentials", prefix: "creds-env."},
		{name: "oauth_access_file", pattern: "create secret generic oauthsonas-access", prefix: "oauth-access.", oauth: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			box := newSandbox(t)
			if testCase.admin {
				box.withEnv("TF_VAR_postgres_admin_password=" + adminPasswordSentinel)
			}
			if testCase.oauth {
				box.withEnv("KODAMA_WITH_OAUTHSONAS=1")
				installOAuthSonasPKI(t, box)
			}
			box.withEnv("KUBECTL_FAIL_PATTERN=" + testCase.pattern)
			code, output := box.run(t, false)
			if code == 0 {
				t.Fatalf("bootstrap-secrets succeeded although kubectl failed on %s:\n%s", testCase.pattern, output)
			}
			assertNoTempFiles(t, box, testCase.prefix)
			assertFormsAbsentFromTrees(t, []string{box.workDir, box.tempDir}, []credentialForm{
				{label: "KMS secret key", secret: kmsSecretStub},
				{label: "OAuthSonas access code", secret: accessCodeStub},
			})
			assertSecretFormsAbsentFromAllArgv(t, box, []credentialForm{
				{label: "postgres admin password", secret: adminPasswordSentinel},
				{label: "KMS secret key", secret: kmsSecretStub},
				{label: "OAuthSonas access code", secret: accessCodeStub},
			})
		})
	}
}

// TestBootstrapSecretsCleansCredentialFilesOnSignal proves the INT/TERM trap
// removes the administrator, KMS credential and OAuthSonas access-code files
// when the operator interrupts the script while kubectl is creating the Secret.
func TestBootstrapSecretsCleansCredentialFilesOnSignal(t *testing.T) {
	cases := []struct {
		name      string
		pattern   string
		logNeedle string
		prefix    string
		admin     bool
		oauth     bool
	}{
		{name: "admin_env_file", pattern: "create secret generic kodama-database-admin", logNeedle: "create\tsecret\tgeneric\tkodama-database-admin", prefix: "admin-env.", admin: true},
		{name: "kms_credentials_env_file", pattern: "create secret generic kodama-kms-credentials", logNeedle: "create\tsecret\tgeneric\tkodama-kms-credentials", prefix: "creds-env."},
		{name: "oauth_access_file", pattern: "create secret generic oauthsonas-access", logNeedle: "create\tsecret\tgeneric\toauthsonas-access", prefix: "oauth-access.", oauth: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			box := newSandbox(t)
			if testCase.admin {
				box.withEnv("TF_VAR_postgres_admin_password=" + adminPasswordSentinel)
			}
			if testCase.oauth {
				box.withEnv("KODAMA_WITH_OAUTHSONAS=1")
				installOAuthSonasPKI(t, box)
			}
			box.withEnv("KUBECTL_HANG_PATTERN=" + testCase.pattern)
			command := box.bashCommand(t)
			command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			var captured bytes.Buffer
			command.Stdout = &captured
			command.Stderr = &captured
			if err := command.Start(); err != nil {
				t.Fatalf("start bootstrap-secrets: %v", err)
			}

			deadline := time.Now().Add(20 * time.Second)
			for {
				if strings.Contains(box.readLogForTest(box.kubectlLog), testCase.logNeedle) {
					break
				}
				if time.Now().After(deadline) {
					_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
					_ = command.Wait()
					t.Fatalf("the script never reached %s:\n%s", testCase.pattern, captured.String())
				}
				time.Sleep(20 * time.Millisecond)
			}
			if err := syscall.Kill(-command.Process.Pid, syscall.SIGTERM); err != nil {
				t.Fatalf("signal the script: %v", err)
			}

			done := make(chan error, 1)
			go func() { done <- command.Wait() }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatalf("interrupted bootstrap-secrets exited zero\n%s", captured.String())
				}
			case <-time.After(20 * time.Second):
				_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
				<-done
				t.Fatalf("interrupted bootstrap-secrets did not exit:\n%s", captured.String())
			}
			assertNoTempFiles(t, box, testCase.prefix)
		})
	}
}

// readLogForTest returns a log's contents, treating a missing log as empty.
func (b *sandbox) readLogForTest(path string) string {
	body, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(body)
}

// TestBootstrapSecretsFailsWhenPasswordGenerationFails proves a failing
// `openssl rand` aborts the run instead of building a DSN with an empty
// password. A `local x="${VAR:-$(openssl rand -hex 24)}"` swallows the
// exit status, so the test fails on its own evidence: the fake openssl fails
// only the "-hex" password calls and lets `rand 32` (the KEK) succeed, so the
// exit code cannot come from a later unrelated failure. One role is unset at a
// time (the other three keep their sentinels), which kills each of the four
// declarations merged back into `local` individually as well as all four at
// once: a swallowed failure then reaches `kubectl create secret kodama-database`
// with an empty password and exits zero.
func TestBootstrapSecretsFailsWhenPasswordGenerationFails(t *testing.T) {
	roles := []struct {
		name string
		env  string
		dsn  string
	}{
		{name: "runtime", env: "KODAMA_DB_RUNTIME_PASSWORD", dsn: "database_url=postgres://kodama_runtime:@"},
		{name: "controller", env: "KODAMA_DB_CONTROLLER_PASSWORD", dsn: "controller_url=postgres://kodama_controller:@"},
		{name: "publisher", env: "KODAMA_DB_PUBLISHER_PASSWORD", dsn: "publisher_url=postgres://kodama_publisher:@"},
		{name: "auth", env: "KODAMA_DB_AUTH_PASSWORD", dsn: "auth_url=postgres://kodama_auth:@"},
	}
	for _, role := range roles {
		t.Run(role.name, func(t *testing.T) {
			box := newSandbox(t).
				unsetEnv(role.env).
				withEnv("OPENSSL_FAIL_RAND=1")
			code, output := box.run(t, false)
			if code == 0 {
				// Keep going: the empty-password evidence below is the direct
				// proof that the swallowed failure reached the Secret, so both
				// assertions must report independently.
				t.Errorf("bootstrap-secrets succeeded although the %s password generation failed:\n%s", role.name, output)
			}
			for _, line := range box.readKubectlArgv(t) {
				if strings.Contains(line, "create\tsecret\tgeneric\tkodama-database") {
					t.Errorf("a failed %s password generation still reached `kubectl create secret kodama-database`: %s", role.name, line)
				}
			}
			secretLog := box.readLogForTest(box.kubectlSecretLog)
			// Positive control: the fake kubectl's secret log is captured in
			// this run (the earlier PKI Secrets are created with --from-file),
			// so the empty-password absence checks below cannot be vacuous.
			if !strings.Contains(secretLog, "from_file_key=") {
				t.Fatalf("the fake kubectl recorded no secret file content; the empty-password check would be vacuous:\n%s", secretLog)
			}
			for _, empty := range []string{
				"database_url=postgres://kodama_runtime:@",
				"controller_url=postgres://kodama_controller:@",
				"publisher_url=postgres://kodama_publisher:@",
				"auth_url=postgres://kodama_auth:@",
			} {
				if strings.Contains(secretLog, empty) {
					t.Errorf("the fake kubectl received a DSN with an empty password (%s) although openssl failed:\n%s", role.dsn, secretLog)
				}
			}
			assertNoTempFiles(t, box, "db-env.", "db-ca.")
		})
	}
}

// descriptorCase is one invalid descriptor value. mutate edits the parsed
// descriptor, env supplies credential inputs and want is the field name the
// refusal must contain.
type descriptorCase struct {
	name   string
	mutate func(descriptor map[string]any)
	env    []string
	want   string
}

func postgresMap(t *testing.T, descriptor map[string]any) map[string]any {
	t.Helper()
	section, ok := descriptor["postgres"].(map[string]any)
	if !ok {
		t.Fatal("descriptor has no postgres object")
	}
	return section
}

// TestBootstrapSecretsRefusesInvalidDescriptorValues is a table test for every
// descriptor-controlled value that is interpolated into a manifest, env-file,
// command line or DSN. Each invalid value must fail before the first kubectl
// call, and the refusal must not echo a credential.
func TestBootstrapSecretsRefusesInvalidDescriptorValues(t *testing.T) {
	setPostgres := func(key string, value any) func(map[string]any) {
		return func(descriptor map[string]any) { postgresMap(t, descriptor)[key] = value }
	}
	setTop := func(key string, value any) func(map[string]any) {
		return func(descriptor map[string]any) { descriptor[key] = value }
	}
	deletePostgres := func(key string) func(map[string]any) {
		return func(descriptor map[string]any) { delete(postgresMap(t, descriptor), key) }
	}
	deleteTop := func(key string) func(map[string]any) {
		return func(descriptor map[string]any) { delete(descriptor, key) }
	}
	setKMS := func(key string, value any) func(map[string]any) {
		return func(descriptor map[string]any) {
			section, ok := descriptor["kms"].(map[string]any)
			if !ok {
				t.Fatal("descriptor has no kms object")
			}
			section[key] = value
		}
	}
	deleteKMS := func(key string) func(map[string]any) {
		return func(descriptor map[string]any) {
			section, ok := descriptor["kms"].(map[string]any)
			if !ok {
				t.Fatal("descriptor has no kms object")
			}
			delete(section, key)
		}
	}

	adminEnv := []string{"TF_VAR_postgres_admin_password=" + adminPasswordSentinel}
	var cases []descriptorCase
	add := func(name, want string, mutate func(map[string]any), env ...string) {
		cases = append(cases, descriptorCase{name: name, want: want, mutate: mutate, env: env})
	}

	for _, invalid := range []struct{ name, value string }{
		{"newline", "db.example\nEVIL=1"},
		{"question", "db?sslmode=disable"},
		{"hash", "db#fragment"},
		{"ampersand", "db&sslmode=disable"},
		{"slash", "db/path"},
		{"space", "db example"},
		// DNS-name label rules shared with the production validator
		// (internal/installation/url_validation.go validHostName): a trailing
		// dot, a leading dot, an empty (double-dot) label and a hyphen at
		// either edge of a label are all refused.
		{"trailing_dot", "db.example."},
		{"leading_dot", ".example"},
		{"double_dot", "db..example"},
		{"label_hyphen_end", "db-.example"},
		{"label_hyphen_start", "db.-example"},
		{"label_too_long", strings.Repeat("a", 64) + ".example"},
		{"name_too_long", strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 63)},
	} {
		add("host_"+invalid.name, "host", setPostgres("host", invalid.value))
	}
	add("host_empty", "host", setPostgres("host", ""))
	add("host_bare_ipv6", "host", setPostgres("host", "::1"))
	add("host_missing", "host", deletePostgres("host"))
	add("host_null", "host", setPostgres("host", nil))
	for _, invalid := range []struct{ name, value string }{
		{"question", "[db?x=1]"},
		{"path", "[x]/db?sslmode=disable#"},
		{"double", "[::1, ::1]"},
		{"suffix", "[::1]x"},
		{"empty", "[]"},
		{"triple_colon", "[:::]"},
		{"dot_only", "[.]"},
		{"bare_ipv4", "[1.2.3.4]"},
		{"bare_single_colon", "[::1]:5432"},
	} {
		add("host_bracket_"+invalid.name, "host", setPostgres("host", invalid.value))
	}
	// One single-character metacharacter per otherwise valid value, for every
	// field that reaches a manifest, env-file or URL. Mixing each
	// metacharacter with '=' would prove nothing, because the identifier regex
	// rejects '=' anyway; these cases pin the metacharacter itself, so adding
	// '?', '@' or '%' to a class is observable even when no '=' is present.
	metacharacters := []struct{ name, value string }{
		{"question", "?"},
		{"at", "@"},
		{"percent", "%"},
		{"ampersand", "&"},
		{"hash", "#"},
		{"slash", "/"},
		{"backslash", `\`},
		{"colon", ":"},
		{"space", " "},
		{"tab", "\t"},
		{"newline", "\n"},
		{"quote", `"`},
		{"apostrophe", "'"},
		{"backtick", "`"},
		{"dollar", "$"},
		{"semicolon", ";"},
		{"pipe", "|"},
	}
	for _, meta := range metacharacters {
		add("host_single_"+meta.name, "host", setPostgres("host", "db"+meta.value+"x"))
		add("database_name_single_"+meta.name, "database", setPostgres("database_name", "db"+meta.value+"x"))
		add("admin_user_single_"+meta.name, "admin_user", setPostgres("admin_user", "db"+meta.value+"x"), adminEnv...)
		add("admin_database_single_"+meta.name, "admin_database", setPostgres("admin_database", "db"+meta.value+"x"), adminEnv...)
	}

	for _, invalid := range []struct{ name, value string }{
		{"newline", "kodama\nEVIL=1"},
		{"question", "kodama?sslmode=disable"},
		{"hash", "kodama#fragment"},
		{"ampersand", "kodama&sslmode=disable"},
		{"slash", "kodama/path"},
		{"space", "kodama db"},
	} {
		add("database_name_"+invalid.name, "database", setPostgres("database_name", invalid.value))
	}
	add("database_name_empty", "database", setPostgres("database_name", ""))
	add("database_name_missing", "database", deletePostgres("database_name"))
	add("database_name_null", "database", setPostgres("database_name", nil))

	for _, invalid := range []struct{ name, value string }{
		{"newline", "kodama_admin\nEVIL=1"},
		{"question", "kodama?admin"},
		{"space", "kodama admin"},
	} {
		add("admin_user_"+invalid.name, "admin_user", setPostgres("admin_user", invalid.value), adminEnv...)
	}
	add("admin_user_empty", "admin_user", setPostgres("admin_user", ""), adminEnv...)
	add("admin_user_missing", "admin_user", deletePostgres("admin_user"), adminEnv...)

	for _, invalid := range []struct{ name, value string }{
		{"newline", "kodama\nEVIL=1"},
		{"question", "kodama?sslmode=disable"},
		{"space", "kodama db"},
	} {
		add("admin_database_"+invalid.name, "admin_database", setPostgres("admin_database", invalid.value), adminEnv...)
	}
	add("admin_database_empty", "admin_database", setPostgres("admin_database", ""), adminEnv...)

	for _, invalid := range []struct {
		name  string
		value any
	}{
		{"newline", "5432\nEVIL=1"},
		{"nondigit", "abc"},
		{"question", "5432?sslmode=disable"},
		{"empty", ""},
		{"zero", "0"},
		{"too_high", "65536"},
		{"way_too_high", "99999"},
		{"leading_zero", "08"},
	} {
		add("port_"+invalid.name, "port", setPostgres("port", invalid.value))
	}
	add("port_missing", "port", deletePostgres("port"))
	add("port_null", "port", setPostgres("port", nil))

	add("release_namespace_missing", "release_namespace", deleteTop("release_namespace"))
	add("release_namespace_null", "release_namespace", setTop("release_namespace", nil))
	add("release_namespace_empty", "release_namespace", setTop("release_namespace", ""))
	add("kms_key_id_missing", "kms.key_id", deleteKMS("key_id"))
	add("kms_key_id_null", "kms.key_id", setKMS("key_id", nil))
	add("kms_key_id_empty", "kms.key_id", setKMS("key_id", ""))
	add("kms_key_ref_missing", "kms.key_ref", deleteKMS("key_ref"))
	add("kms_key_ref_null", "kms.key_ref", setKMS("key_ref", nil))
	add("kms_key_ref_empty", "kms.key_ref", setKMS("key_ref", ""))

	add("region_newline", "region", setTop("region", "fr-par\nEVIL=1"))
	add("region_missing", "region", deleteTop("region"))
	add("region_null", "region", setTop("region", nil))

	add("project_id_newline", "account_or_project_id", setTop("account_or_project_id", "project-1\nEVIL=1"))
	add("project_id_missing", "account_or_project_id", deleteTop("account_or_project_id"))
	add("project_id_null", "account_or_project_id", setTop("account_or_project_id", nil))

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			box := newSandbox(t)
			var descriptor map[string]any
			if err := json.Unmarshal([]byte(readDescriptorForTest(t, box)), &descriptor); err != nil {
				t.Fatalf("parse descriptor: %v", err)
			}
			testCase.mutate(descriptor)
			encoded, err := json.Marshal(descriptor)
			if err != nil {
				t.Fatalf("encode descriptor: %v", err)
			}
			box.writeDescriptor(t, string(encoded))
			if len(testCase.env) != 0 {
				box.withEnv(testCase.env...)
			}
			code, output := box.run(t, false)
			if code == 0 {
				t.Fatalf("bootstrap-secrets accepted an invalid %s:\n%s", testCase.name, output)
			}
			if lines := box.readKubectlArgv(t); len(lines) != 0 {
				t.Errorf("an invalid %s reached kubectl before being refused: %s", testCase.name, strings.Join(lines, "\n"))
			}
			if !strings.Contains(strings.ToLower(output), testCase.want) {
				t.Errorf("the refusal does not name %q: %s", testCase.want, output)
			}
			for _, sentinel := range append(append([]string(nil), dbSentinels...), kmsSecretStub, adminPasswordSentinel) {
				if strings.Contains(output, sentinel) {
					t.Errorf("the refusal of %s echoes a credential", testCase.name)
				}
			}
			assertSecretFormsAbsentFromAllArgv(t, box, kubectlCredentialForms(stubKEKMaterial(), dbForms()))
		})
	}

	t.Run("kms_secret_key_newline", func(t *testing.T) {
		box := newSandbox(t).withEnv(`TOFU_KMS_SECRET=11111111-2222\nEVIL=1`)
		code, output := box.run(t, false)
		if code == 0 {
			t.Fatalf("bootstrap-secrets accepted a KMS secret key containing a newline:\n%s", output)
		}
		if lines := box.readKubectlArgv(t); len(lines) != 0 {
			t.Errorf("a newline in the KMS secret key reached kubectl before being refused: %s", strings.Join(lines, "\n"))
		}
		if !strings.Contains(output, "secret_key") {
			t.Errorf("the refusal does not name the KMS secret_key: %s", output)
		}
		if strings.Contains(output, "EVIL=1") {
			t.Errorf("the refusal echoes the KMS credential: %s", output)
		}
	})
}

// TestBootstrapSecretsAcceptsBracketedIPv6Host proves a bracketed IPv6 literal
// is a valid postgres.host and survives into the DSN with the port outside the
// brackets. The bracket branch must exist and accept real literals; a deleted
// or ^.+$-loosened branch either refuses these or admits the invalid bracket
// values pinned in the table above.
func TestBootstrapSecretsAcceptsBracketedIPv6Host(t *testing.T) {
	for _, testCase := range []struct{ name, host, hostname string }{
		{name: "loopback", host: "[::1]", hostname: "::1"},
		{name: "documentation", host: "[2001:db8::1]", hostname: "2001:db8::1"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			box := newSandbox(t)
			var descriptor map[string]any
			if err := json.Unmarshal([]byte(readDescriptorForTest(t, box)), &descriptor); err != nil {
				t.Fatalf("parse descriptor: %v", err)
			}
			postgresMap(t, descriptor)["host"] = testCase.host
			encoded, err := json.Marshal(descriptor)
			if err != nil {
				t.Fatalf("encode descriptor: %v", err)
			}
			box.writeDescriptor(t, string(encoded))
			code, output := box.run(t, false)
			if code != 0 {
				t.Fatalf("bootstrap-secrets refused the bracketed host %s: exit %d\n%s", testCase.host, code, output)
			}
			record := dsnRecord(t, box.envFileRecords(t), "database_url")
			assertDSN(t, "database_url", record.values["database_url"], "kodama_runtime", dbSentinels[0], testCase.hostname, "5432", "kodama")
			if !strings.Contains(record.values["database_url"], "@"+testCase.host+":5432/") {
				t.Errorf("DSN did not put the port outside the bracketed host: %q", record.values["database_url"])
			}
		})
	}
}

// TestBootstrapSecretsAcceptsValidHostNames proves the label rules mirroring
// the production validator do not over-reject: an internal hyphen, an
// underscore and a single-label name remain valid and reach the DSN unchanged.
func TestBootstrapSecretsAcceptsValidHostNames(t *testing.T) {
	for _, host := range []string{"db-1.example", "db_1.example", "postgres", "a.b-c"} {
		t.Run(host, func(t *testing.T) {
			box := newSandbox(t)
			var descriptor map[string]any
			if err := json.Unmarshal([]byte(readDescriptorForTest(t, box)), &descriptor); err != nil {
				t.Fatalf("parse descriptor: %v", err)
			}
			postgresMap(t, descriptor)["host"] = host
			encoded, err := json.Marshal(descriptor)
			if err != nil {
				t.Fatalf("encode descriptor: %v", err)
			}
			box.writeDescriptor(t, string(encoded))
			code, output := box.run(t, false)
			if code != 0 {
				t.Fatalf("bootstrap-secrets refused the valid host %s: exit %d\n%s", host, code, output)
			}
			record := dsnRecord(t, box.envFileRecords(t), "database_url")
			if !strings.Contains(record.values["database_url"], "@"+host+":5432/") {
				t.Errorf("DSN did not use the accepted host %s: %q", host, record.values["database_url"])
			}
		})
	}
}

// TestBootstrapSecretsAcceptsPortBoundaries proves the 1..65535 range is
// enforced as a range with the boundary values accepted, while 0, 65536, 99999
// and a leading zero are refused by the table test above.
func TestBootstrapSecretsAcceptsPortBoundaries(t *testing.T) {
	for _, port := range []string{"1", "65535"} {
		t.Run("port_"+port, func(t *testing.T) {
			box := newSandbox(t)
			var descriptor map[string]any
			if err := json.Unmarshal([]byte(readDescriptorForTest(t, box)), &descriptor); err != nil {
				t.Fatalf("parse descriptor: %v", err)
			}
			postgresMap(t, descriptor)["port"] = port
			encoded, err := json.Marshal(descriptor)
			if err != nil {
				t.Fatalf("encode descriptor: %v", err)
			}
			box.writeDescriptor(t, string(encoded))
			code, output := box.run(t, false)
			if code != 0 {
				t.Fatalf("bootstrap-secrets refused the boundary port %s: exit %d\n%s", port, code, output)
			}
			record := dsnRecord(t, box.envFileRecords(t), "database_url")
			assertDSN(t, "database_url", record.values["database_url"], "kodama_runtime", dbSentinels[0], "db.example", port, "kodama")
		})
	}
}

// TestBootstrapSecretsRefusesNonASCIIIdentifiersUnderUTF8Locale proves the
// validators force the C locale. Under en_US.UTF-8, bash's [A-Za-z] also
// matches 'é' and the fullwidth 'ａ'; without LC_ALL=C in the script those
// values pass the host and identifier classes and reach kubectl. '²' is not a
// letter under either locale and must stay refused as well.
func TestBootstrapSecretsRefusesNonASCIIIdentifiersUnderUTF8Locale(t *testing.T) {
	type field struct {
		name   string
		mutate func(map[string]any, string)
		env    []string
	}
	postgresField := func(key string) func(map[string]any, string) {
		return func(descriptor map[string]any, value string) { postgresMap(t, descriptor)[key] = value }
	}
	fields := []field{
		{name: "host", mutate: postgresField("host")},
		{name: "database_name", mutate: postgresField("database_name")},
		{name: "admin_user", mutate: postgresField("admin_user"), env: []string{"TF_VAR_postgres_admin_password=" + adminPasswordSentinel}},
		{name: "admin_database", mutate: postgresField("admin_database"), env: []string{"TF_VAR_postgres_admin_password=" + adminPasswordSentinel}},
	}
	for _, testField := range fields {
		for _, value := range []string{"é", "²", "ａ"} {
			testField, value := testField, value
			t.Run(testField.name+"_"+fmt.Sprintf("%U", []rune(value)[0]), func(t *testing.T) {
				box := newSandbox(t).
					withEnv("LANG=en_US.UTF-8", "LC_ALL=en_US.UTF-8")
				if len(testField.env) != 0 {
					box.withEnv(testField.env...)
				}
				var descriptor map[string]any
				if err := json.Unmarshal([]byte(readDescriptorForTest(t, box)), &descriptor); err != nil {
					t.Fatalf("parse descriptor: %v", err)
				}
				testField.mutate(descriptor, value)
				encoded, err := json.Marshal(descriptor)
				if err != nil {
					t.Fatalf("encode descriptor: %v", err)
				}
				box.writeDescriptor(t, string(encoded))
				code, output := box.run(t, false)
				if code == 0 {
					t.Fatalf("bootstrap-secrets accepted the non-ASCII %s %q under en_US.UTF-8:\n%s", testField.name, value, output)
				}
				if lines := box.readKubectlArgv(t); len(lines) != 0 {
					t.Errorf("a non-ASCII %s reached kubectl: %s", testField.name, strings.Join(lines, "\n"))
				}
			})
		}
	}
}

// TestScriptsAvoidPortableBase64AndArrayTraps pins two portability rules:
// `base64 -w0` is GNU-only, and an empty array expanded with `${arr[@]}` trips
// `set -u` on bash before 4.4.
func TestScriptsAvoidPortableBase64AndArrayTraps(t *testing.T) {
	root := filepath.Join(repoRoot(t), "scripts")
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if bytes.Contains(body, []byte("base64 -w0")) {
			t.Errorf("%s uses GNU-only `base64 -w0`; use `base64 | tr -d '\\n'`", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk scripts: %v", err)
	}
	deploy, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "deploy"))
	if err != nil {
		t.Fatalf("read scripts/deploy: %v", err)
	}
	for _, array := range []string{"extra_values", "wait_args"} {
		if !strings.Contains(string(deploy), `${`+array+`[@]+"${`+array+`[@]}"}`) {
			t.Errorf("scripts/deploy expands the possibly-empty %s array without a bash-4.3-safe guard", array)
		}
	}
}
