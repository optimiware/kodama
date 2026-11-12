//go:build component

// The broker authorization callout Secret is a hard stage prerequisite: the
// integrated chart defaults to nats.auth.mode=callout and refuses to render
// without kodama-nats-auth. These tests run the real scripts/bootstrap-secrets
// against fake Kubernetes/cloud binaries and compare what it writes with the
// rendered chart's contract.
//
// The fake kubectl records the content of each --from-file source only when
// KUBECTL_SECRET_CONTENT_LOG is set, so the tests can prove which values were
// reused and which were rotated without printing secret material.
package deploy

import (
	"crypto/ed25519"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// natsAuthKeyToolPath is the compiled tools/nats-auth-key binary. Building it
// once keeps the fake-cluster tests fast while still exercising the real
// generator the production script invokes.
var natsAuthKeyToolPath string

const natsAuthSecretName = "kodama-nats-auth"

// TestMain builds the NKey tool once for every test in this package.
func TestMain(m *testing.M) {
	root, err := componentRepoRoot()
	if err != nil {
		os.Stderr.WriteString("locate repository root: " + err.Error() + "\n")
		os.Exit(1)
	}
	dir, err := os.MkdirTemp("", "kodama-nats-auth-key-")
	if err != nil {
		os.Stderr.WriteString("create tool directory: " + err.Error() + "\n")
		os.Exit(1)
	}
	natsAuthKeyToolPath = filepath.Join(dir, "nats-auth-key")
	build := exec.Command("go", "build", "-o", natsAuthKeyToolPath, "./tools/nats-auth-key")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		os.Stderr.WriteString("build ./tools/nats-auth-key: " + err.Error() + "\n" + string(output))
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// componentRepoRoot walks up from the test working directory to the module
// root without requiring a *testing.T (TestMain runs before any test).
func componentRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}

// natsAuthContract reads the chart values that define the Secret name, key
// names and callout account. The bootstrap script and the chart must agree on
// every one of these.
func natsAuthContract(t *testing.T) (secret string, account string, keys []string) {
	t.Helper()
	body, err := os.ReadFile(stackChartFile(t, "values.yaml"))
	if err != nil {
		t.Fatalf("read stack values: %v", err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(body, &values); err != nil {
		t.Fatalf("parse stack values: %v", err)
	}
	nats := mapChild(t, values, "nats")
	auth := mapChild(t, nats, "auth")
	callout := mapChild(t, auth, "callout")
	secret = stringChild(t, callout, "existingSecret")
	account = stringChild(t, callout, "account")
	keys = []string{
		stringChild(t, callout, "issuerKey"),
		stringChild(t, callout, "issuerPubKey"),
		stringChild(t, callout, "shellUserKey"),
		stringChild(t, callout, "shellPasswordKey"),
		stringChild(t, callout, "userKey"),
		stringChild(t, callout, "passwordKey"),
		"account",
	}

	kodama := mapChild(t, values, "kodama")
	secrets := mapChild(t, kodama, "secrets")
	natsAuth := mapChild(t, secrets, "natsAuth")
	if got := stringChild(t, natsAuth, "existingSecret"); got != secret {
		t.Fatalf("kodama.secrets.natsAuth.existingSecret = %q; nats.auth.callout.existingSecret = %q; want one name", got, secret)
	}
	if got := stringChild(t, natsAuth, "account"); got != account {
		t.Fatalf("kodama.secrets.natsAuth.account = %q; nats.auth.callout.account = %q; want one value", got, account)
	}
	if secret != natsAuthSecretName {
		t.Fatalf("chart callout Secret = %q; scripts/bootstrap-secrets creates %q", secret, natsAuthSecretName)
	}
	return secret, account, keys
}

func mapChild(t *testing.T, parent map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := parent[key].(map[string]any)
	if !ok {
		t.Fatalf("chart values have no object %q", key)
	}
	return value
}

func stringChild(t *testing.T, parent map[string]any, key string) string {
	t.Helper()
	value, ok := parent[key].(string)
	if !ok || value == "" {
		t.Fatalf("chart values have no non-empty string %q", key)
	}
	return value
}

// natsAuthFromFiles returns the key -> content map recorded for the
// kodama-nats-auth creation and the keys from that create argv. It proves the
// script hands kubectl exactly the chart's key set.
func natsAuthFromFiles(t *testing.T, box *sandbox) (map[string]string, []kubectlFileRecord, []string) {
	t.Helper()
	content := readKubectlContent(t, filepath.Join(box.recordsDir, "kubectl.content"))
	records := box.kubectlFileRecords(t)

	var createdKeys []string
	found := false
	for _, line := range box.readKubectlArgv(t) {
		fields := strings.Split(line, "\t")
		for index := 0; index+3 < len(fields); index++ {
			if fields[index] != "create" || fields[index+1] != "secret" || fields[index+2] != "generic" || fields[index+3] != natsAuthSecretName {
				continue
			}
			found = true
			for _, field := range fields {
				if after, ok := strings.CutPrefix(field, "--from-file="); ok {
					spec := after
					createdKeys = append(createdKeys, strings.SplitN(spec, "=", 2)[0])
				}
			}
		}
	}
	if !found {
		t.Fatalf("bootstrap-secrets never created %s:\n%s", natsAuthSecretName, strings.Join(box.readKubectlArgv(t), "\n"))
	}
	return content, records, createdKeys
}

// readKubectlContent parses the optional --from-file content log. The value
// may contain newlines, so the begin/end markers delimit it.
func readKubectlContent(t *testing.T, path string) map[string]string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read kubectl content log: %v", err)
	}
	values := map[string]string{}
	lines := strings.Split(string(body), "\n")
	currentKey := ""
	collecting := false
	var collected []string
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "from_file_key="):
			currentKey = strings.TrimPrefix(line, "from_file_key=")
		case line == "from_file_content_begin":
			collecting = true
			collected = nil
		case line == "from_file_content_end":
			values[currentKey] = strings.Join(collected, "\n")
			collecting = false
		default:
			if collecting {
				collected = append(collected, line)
			}
		}
	}
	return values
}

// nkeyPublicFromSeed decodes an account seed using an independent copy of the
// NKey framing and returns the encoded public key. It proves the pair written
// by the script is a real Ed25519 account key pair, not two random strings.
func nkeyPublicFromSeed(t *testing.T, seed string) string {
	t.Helper()
	decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(seed)
	if err != nil {
		t.Fatalf("issuer.seed is not base32: %v", err)
	}
	if len(decoded) < 4 {
		t.Fatalf("issuer.seed is truncated")
	}
	payload, checksum := decoded[:len(decoded)-2], binary.LittleEndian.Uint16(decoded[len(decoded)-2:])
	if nkeyCRC16(payload) != checksum {
		t.Fatalf("issuer.seed checksum is invalid")
	}
	if payload[0]&248 != 18<<3 || len(payload) != 2+ed25519.SeedSize {
		t.Fatalf("issuer.seed is not an account seed")
	}
	publicPrefix := (payload[0]&7)<<5 | (payload[1]&248)>>3
	if publicPrefix != 0 {
		t.Fatalf("issuer.seed has public prefix %d, want the account prefix 0", publicPrefix)
	}
	private := ed25519.NewKeyFromSeed(payload[2:])
	public, ok := private.Public().(ed25519.PublicKey)
	if !ok {
		t.Fatalf("issuer.seed has no public key")
	}
	raw := append([]byte{0}, public...)
	var value [2]byte
	binary.LittleEndian.PutUint16(value[:], nkeyCRC16(raw))
	raw = append(raw, value[:]...)
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
}

func nkeyCRC16(data []byte) uint16 {
	var crc uint16
	for _, value := range data {
		crc ^= uint16(value) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// natsAuthSecretFixture renders an existing Secret's data as the fake kubectl
// would return it for `get secret -o json`.
func natsAuthSecretFixture(t *testing.T, values map[string]string) string {
	t.Helper()
	data := make(map[string]string, len(values))
	for key, value := range values {
		data[key] = base64.StdEncoding.EncodeToString([]byte(value))
	}
	body, err := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]string{"name": natsAuthSecretName},
		"data":       data,
	})
	if err != nil {
		t.Fatalf("marshal Secret fixture: %v", err)
	}
	return string(body)
}

func writeNATSAuthFixture(t *testing.T, box *sandbox, values map[string]string) {
	t.Helper()
	path := filepath.Join(box.kubectlStateDir, natsAuthSecretName+".json")
	if err := os.WriteFile(path, []byte(natsAuthSecretFixture(t, values)), 0o600); err != nil {
		t.Fatalf("write Secret fixture: %v", err)
	}
}

func hexPassword(t *testing.T, label, value string) {
	t.Helper()
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(value) {
		t.Errorf("%s is not a 32-byte hex password (length %d)", label, len(value))
	}
}

// TestBootstrapSecretsNATSAuthMatchesChartContract proves a first run creates
// the chart's Secret with exactly the chart's key names and account, a valid
// account NKey pair, strong passwords, private source files and no secret on
// argv or in output.
func TestBootstrapSecretsNATSAuthMatchesChartContract(t *testing.T) {
	secret, account, chartKeys := natsAuthContract(t)
	box := newSandbox(t)
	contentLog := filepath.Join(box.recordsDir, "kubectl.content")
	box.withEnv("KUBECTL_SECRET_CONTENT_LOG=" + contentLog)

	code, output := box.run(t, false)
	if code != 0 {
		t.Fatalf("bootstrap-secrets failed: exit %d\n%s", code, output)
	}
	if secret != natsAuthSecretName {
		t.Fatalf("chart Secret %q differs from the bootstrap Secret %q", secret, natsAuthSecretName)
	}

	content, records, createdKeys := natsAuthFromFiles(t, box)
	if got, want := strings.Join(createdKeys, ","), strings.Join(chartKeys, ","); got != want {
		t.Errorf("bootstrap key set = %s; chart contract = %s", got, want)
	}
	for _, key := range chartKeys {
		value, ok := content[key]
		if !ok || value == "" {
			t.Errorf("Secret has no non-empty %q", key)
		}
	}

	if got := content["account"]; got != account {
		t.Errorf("Secret account = %q; chart callout account = %q", got, account)
	}
	if got := content["shell-user"]; got != "shell" {
		t.Errorf("shell-user = %q, want shell", got)
	}
	if got := content["callout-user"]; got != "callout" {
		t.Errorf("callout-user = %q, want callout", got)
	}
	hexPassword(t, "shell-password", content["shell-password"])
	hexPassword(t, "callout-password", content["callout-password"])
	if content["shell-password"] == content["callout-password"] {
		t.Errorf("both bypass users share one password")
	}
	if public := nkeyPublicFromSeed(t, content["issuer.seed"]); public != content["issuer.pub"] {
		t.Errorf("issuer.pub = %q; the seed's public key = %q", content["issuer.pub"], public)
	}

	// The source files must be 0600 inside the 0700 temporary directory.
	for _, record := range records {
		if _, ok := content[record.key]; !ok {
			continue
		}
		if !containsString(chartKeys, record.key) {
			continue
		}
		if record.mode != "600" {
			t.Errorf("%s source %s mode = %s, want 600", record.key, record.path, record.mode)
		}
		if record.dirMode != "700" {
			t.Errorf("%s source directory mode = %s, want 700", record.key, record.dirMode)
		}
	}

	// No secret material may reach stdout/stderr or any command argv.
	forms := make([]credentialForm, 0, 5)
	for _, key := range []string{"issuer.seed", "issuer.pub", "shell-password", "callout-password"} {
		forms = append(forms, credentialForm{label: key, secret: content[key]})
	}
	for _, form := range forms {
		if strings.Contains(output, form.secret) {
			t.Errorf("%s appears in bootstrap output", form.label)
		}
	}
	assertSecretFormsAbsentFromAllArgv(t, box, forms)

	// The first run generated both passwords; this positive control makes the
	// rerun's zero-generation assertion meaningful.
	if got := countArgvSequence(strings.Join(box.readAllArgv(t), "\n"), "openssl", "rand", "-hex", "32"); got != 2 {
		t.Errorf("first run generated %d callout passwords, want 2", got)
	}
}

// TestBootstrapSecretsNATSAuthRerunPreservesValues proves an existing Secret is
// reused without regeneration, and that --rotate-nats-auth is the explicit
// path that replaces every secret value.
func TestBootstrapSecretsNATSAuthRerunPreservesValues(t *testing.T) {
	_, account, _ := natsAuthContract(t)
	box := newSandbox(t)
	contentLog := filepath.Join(box.recordsDir, "kubectl.content")
	box.withEnv("KUBECTL_SECRET_CONTENT_LOG=" + contentLog)

	code, output := box.run(t, false)
	if code != 0 {
		t.Fatalf("first bootstrap-secrets run failed: exit %d\n%s", code, output)
	}
	first, _, _ := natsAuthFromFiles(t, box)
	if first["issuer.seed"] == "" || first["shell-password"] == "" {
		t.Fatalf("first run did not create the callout material")
	}
	writeNATSAuthFixture(t, box, first)
	box.withEnv("KUBECTL_SECRETS_PRESENT=1")

	// Plain rerun: every value is preserved and no password/issuer is generated.
	box.resetLogs(t)
	os.Remove(contentLog)
	code, output = box.run(t, false)
	if code != 0 {
		t.Fatalf("plain bootstrap-secrets rerun failed: exit %d\n%s", code, output)
	}
	if got := countArgvSequence(strings.Join(box.readAllArgv(t), "\n"), "openssl", "rand", "-hex", "32"); got != 0 {
		t.Errorf("plain rerun generated %d callout passwords, want 0", got)
	}
	if kubectlCreatedSecret(box.readKubectlArgv(t), natsAuthSecretName) {
		t.Errorf("plain rerun recreated %s", natsAuthSecretName)
	}
	if !strings.Contains(output, "preserving existing Secret") {
		t.Errorf("plain rerun does not report preservation:\n%s", output)
	}

	// Explicit rotation replaces the issuer pair and both passwords, keeps the
	// account, and is the only path that does so.
	box.resetLogs(t)
	os.Remove(contentLog)
	code, output = box.runArgs(t, false, "022", "--rotate-nats-auth")
	if code != 0 {
		t.Fatalf("--rotate-nats-auth failed: exit %d\n%s", code, output)
	}
	if !kubectlCreatedSecret(box.readKubectlArgv(t), natsAuthSecretName) {
		t.Fatalf("--rotate-nats-auth did not recreate %s", natsAuthSecretName)
	}
	rotated, _, _ := natsAuthFromFiles(t, box)
	for _, key := range []string{"issuer.seed", "issuer.pub", "shell-password", "callout-password"} {
		if rotated[key] == "" {
			t.Errorf("rotation did not write %s", key)
		}
		if rotated[key] == first[key] {
			t.Errorf("rotation kept the old %s", key)
		}
	}
	if rotated["account"] != account {
		t.Errorf("rotation changed the account to %q, want %q", rotated["account"], account)
	}
	if public := nkeyPublicFromSeed(t, rotated["issuer.seed"]); public != rotated["issuer.pub"] {
		t.Errorf("rotated issuer.pub does not match the rotated seed")
	}
}

// TestBootstrapSecretsNATSAuthFillsMissingKeysOnly proves a partially created
// Secret is repaired without rotating the values that are still present.
func TestBootstrapSecretsNATSAuthFillsMissingKeysOnly(t *testing.T) {
	_, account, _ := natsAuthContract(t)
	existing := map[string]string{
		"shell-user":       "shell",
		"shell-password":   "existing-shell-password-0123456789abcdef0123456789abcdef",
		"callout-user":     "callout",
		"callout-password": "existing-callout-password-0123456789abcdef0123456789ab",
		"account":          account,
	}
	box := newSandbox(t).withEnv("KUBECTL_SECRETS_PRESENT=1")
	contentLog := filepath.Join(box.recordsDir, "kubectl.content")
	box.withEnv("KUBECTL_SECRET_CONTENT_LOG=" + contentLog)
	writeNATSAuthFixture(t, box, existing)

	code, output := box.run(t, false)
	if code != 0 {
		t.Fatalf("bootstrap-secrets failed: exit %d\n%s", code, output)
	}
	content, _, _ := natsAuthFromFiles(t, box)
	for key, want := range existing {
		if got := content[key]; got != want {
			t.Errorf("missing-key repair rotated existing %s", key)
		}
	}
	if content["issuer.seed"] == "" || content["issuer.pub"] == "" {
		t.Fatalf("missing-key repair did not create the issuer pair")
	}
	if public := nkeyPublicFromSeed(t, content["issuer.seed"]); public != content["issuer.pub"] {
		t.Errorf("repaired issuer pair does not match")
	}
}

// TestBootstrapSecretsNATSAuthRegeneratesIncompleteIssuerPair proves the seed
// and its public half are one unit. A Secret that carries only one half must
// get a new pair: keeping the old half would leave the broker verifying with a
// key the Shell cannot sign for (or the Shell signing with a key the broker
// does not know), and pairing the old half with an empty value would apply a
// Secret the workloads cannot use.
func TestBootstrapSecretsNATSAuthRegeneratesIncompleteIssuerPair(t *testing.T) {
	// The reference account vector from tools/nats-auth-key's known-vector
	// test. Using a real seed makes "the old half is preserved" a meaningful
	// outcome; garbage would be regenerated for the wrong reason.
	const (
		oldSeed   = "SAAAAAICAMCAKBQHBAEQUCYMBUHA6EARCIJRIFIWC4MBSGQ3DQOR4HYMHI"
		oldPublic = "AAB2CB576PHBBPQ5ODORRZ2LYCMWPZGWGCN2KDK7DXOIMZASKUY3QPZV"
	)
	cases := []struct {
		name     string
		existing map[string]string
		oldHalf  string
	}{
		{
			name:     "seed-only",
			existing: map[string]string{"issuer.seed": oldSeed},
			oldHalf:  oldSeed,
		},
		{
			name:     "public-only",
			existing: map[string]string{"issuer.pub": oldPublic},
			oldHalf:  oldPublic,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, account, _ := natsAuthContract(t)
			existing := map[string]string{
				"shell-user":       "shell",
				"shell-password":   "existing-shell-password-0123456789abcdef0123456789abcdef",
				"callout-user":     "callout",
				"callout-password": "existing-callout-password-0123456789abcdef0123456789ab",
				"account":          account,
			}
			maps.Copy(existing, testCase.existing)

			box := newSandbox(t).withEnv("KUBECTL_SECRETS_PRESENT=1")
			contentLog := filepath.Join(box.recordsDir, "kubectl.content")
			box.withEnv("KUBECTL_SECRET_CONTENT_LOG=" + contentLog)
			writeNATSAuthFixture(t, box, existing)

			code, output := box.run(t, false)
			if code != 0 {
				t.Fatalf("bootstrap-secrets failed: exit %d\n%s", code, output)
			}
			if !kubectlCreatedSecret(box.readKubectlArgv(t), natsAuthSecretName) {
				t.Fatalf("an incomplete issuer pair did not reconcile %s", natsAuthSecretName)
			}

			content, _, _ := natsAuthFromFiles(t, box)
			if content["issuer.seed"] == "" {
				t.Fatal("repair applied an empty issuer.seed")
			}
			if content["issuer.pub"] == "" {
				t.Fatal("repair applied an empty issuer.pub")
			}
			if content["issuer.seed"] == testCase.oldHalf || content["issuer.pub"] == testCase.oldHalf {
				t.Errorf("repair kept the incomplete half %q instead of regenerating the pair", testCase.oldHalf)
			}
			// The surviving half must be the new one, not the old value on the
			// other side: an unchanged seed with a new pub (or the reverse) is
			// the split pair this branch exists to prevent.
			if testCase.name == "seed-only" && content["issuer.pub"] == oldPublic {
				t.Errorf("seed-only repair kept the old issuer.pub, which does not match the regenerated seed")
			}
			if testCase.name == "public-only" && content["issuer.seed"] == oldSeed {
				t.Errorf("public-only repair kept the old issuer.seed, which does not match the regenerated public half")
			}
			if public := nkeyPublicFromSeed(t, content["issuer.seed"]); public != content["issuer.pub"] {
				t.Errorf("repaired issuer.pub = %q; regenerated seed's public key = %q", content["issuer.pub"], public)
			}
			for key, want := range existing {
				switch key {
				case "issuer.seed", "issuer.pub":
					continue
				}
				if got := content[key]; got != want {
					t.Errorf("issuer repair rotated existing %s", key)
				}
			}
		})
	}
}

// TestBootstrapSecretsNATSAuthFailsOnSecretProbeError proves a probe failure
// that is not an explicit NotFound is fatal. kubectl exits non-zero for a
// missing object and for a transient API failure alike; treating the latter as
// absence would generate a fresh issuer and fresh bypass passwords and apply
// them over a Secret that still exists.
func TestBootstrapSecretsNATSAuthFailsOnSecretProbeError(t *testing.T) {
	box := newSandbox(t).withEnv("KUBECTL_FAIL_PATTERN=get secret kodama-nats-auth")
	code, output := box.run(t, false)
	if code == 0 {
		t.Fatalf("bootstrap-secrets accepted a failed Secret probe as absence:\n%s", output)
	}
	if !strings.Contains(output, "could not determine whether Secret") {
		t.Errorf("the run failed for a reason other than the Secret probe:\n%s", output)
	}
	if kubectlCreatedSecret(box.readKubectlArgv(t), natsAuthSecretName) {
		t.Errorf("a failed Secret probe still applied %s", natsAuthSecretName)
	}
}

// bootstrapNATSAuthKeysAndAccount reads the bootstrap script's own contract:
// the exact key list and account value it writes into kodama-nats-auth.
func bootstrapNATSAuthKeysAndAccount(t *testing.T) ([]string, string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "bootstrap-secrets"))
	if err != nil {
		t.Fatalf("read scripts/bootstrap-secrets: %v", err)
	}
	script := string(body)
	keysMatch := regexp.MustCompile(`(?ms)^KODAMA_NATS_AUTH_KEYS=\((.*?)\)`).FindStringSubmatch(script)
	if keysMatch == nil {
		t.Fatal("scripts/bootstrap-secrets has no KODAMA_NATS_AUTH_KEYS array")
	}
	keys := strings.Fields(keysMatch[1])
	if len(keys) == 0 {
		t.Fatal("scripts/bootstrap-secrets has an empty KODAMA_NATS_AUTH_KEYS array")
	}
	accountMatch := regexp.MustCompile(`(?m)^KODAMA_NATS_AUTH_ACCOUNT="([^"]+)"`).FindStringSubmatch(script)
	if accountMatch == nil || accountMatch[1] == "" {
		t.Fatal("scripts/bootstrap-secrets has no KODAMA_NATS_AUTH_ACCOUNT value")
	}
	return keys, accountMatch[1]
}

// deployStageNATSAuthAccount reads the stage values renderer's callout account.
// It is the value the render test feeds to Helm, so a change here must be
// observable as a rendered/bootstrapped mismatch.
func deployStageNATSAuthAccount(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "lib", "deploy.sh"))
	if err != nil {
		t.Fatalf("read scripts/lib/deploy.sh: %v", err)
	}
	match := regexp.MustCompile(`(?m)^\s*local nats_auth_account="([^"]+)"`).FindStringSubmatch(string(body))
	if match == nil || match[1] == "" {
		t.Fatal("scripts/lib/deploy.sh has no nats_auth_account value")
	}
	return match[1]
}

// renderStackWithValuesFile renders the stack chart with a values file before
// the production overrides, the same way scripts/deploy invokes Helm.
func renderStackWithValuesFile(t *testing.T, helm, chart, valuesFile string) (string, error) {
	t.Helper()
	args := []string{"template", "kodama-stack", chart, "--namespace", "kodama-system", "-f", valuesFile}
	args = append(args, productionOIDCArgs("kodama.")...)
	command := exec.Command(helm, args...)
	command.Dir = chart
	output, err := command.CombinedOutput()
	return string(output), err
}

// secretKeyRefsFromWorkload collects every kodama-nats-auth key a workload's
// containers read through secretKeyRef, and the literal
// KODAMA_NATS_CALLOUT_ACCOUNT value it renders.
func secretKeyRefsFromWorkload(t *testing.T, object map[string]any) (map[string]bool, string) {
	t.Helper()
	keys := map[string]bool{}
	account := ""
	podSpec := objectAtKey(t, objectAtKey(t, objectAtKey(t, object, "spec"), "template"), "spec")
	for _, containerList := range []string{"initContainers", "containers"} {
		containers, ok := podSpec[containerList].([]any)
		if !ok {
			continue
		}
		for _, entry := range containers {
			container, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			env, ok := container["env"].([]any)
			if !ok {
				continue
			}
			for _, envEntry := range env {
				variable, ok := envEntry.(map[string]any)
				if !ok {
					continue
				}
				if name, _ := variable["name"].(string); name == "KODAMA_NATS_CALLOUT_ACCOUNT" {
					account, _ = variable["value"].(string)
				}
				valueFrom, ok := variable["valueFrom"].(map[string]any)
				if !ok {
					continue
				}
				secretKeyRef, ok := valueFrom["secretKeyRef"].(map[string]any)
				if !ok {
					continue
				}
				name, _ := secretKeyRef["name"].(string)
				key, _ := secretKeyRef["key"].(string)
				if name == natsAuthSecretName && key != "" {
					keys[key] = true
				}
			}
		}
	}
	return keys, account
}

// TestBootstrapSecretsNATSAuthRenderedWiringMatchesBootstrap renders the stack
// chart with the values scripts/deploy generates for the stage descriptor and
// compares the rendered references with the bootstrap contract. A template that
// reads a key the script never writes, or a stage account that differs from the
// account the script writes, fails here instead of at pod start.
func TestBootstrapSecretsNATSAuthRenderedWiringMatchesBootstrap(t *testing.T) {
	bootstrapKeys, bootstrapAccount := bootstrapNATSAuthKeysAndAccount(t)
	stageAccount := deployStageNATSAuthAccount(t)
	if stageAccount != bootstrapAccount {
		t.Errorf("scripts/lib/deploy.sh nats_auth_account = %q; bootstrap script writes %q", stageAccount, bootstrapAccount)
	}

	box := newDeploySandbox(t)
	code, output := box.run(t, "render-values", "--cluster", "stage")
	if code != 0 {
		t.Fatalf("scripts/deploy render-values failed: exit %d\n%s", code, output)
	}
	valuesFile := filepath.Join(box.workDir, "values.generated.yaml")
	if _, err := os.Stat(valuesFile); err != nil {
		t.Fatalf("render-values did not write %s: %v", valuesFile, err)
	}

	helm, chart := stageStackChart(t)
	rendered, err := renderStackWithValuesFile(t, helm, chart, valuesFile)
	if err != nil {
		t.Fatalf("helm template with stage values failed: %v\n%s", err, rendered)
	}

	workloads := map[string]map[string]bool{}
	accounts := map[string]string{}
	for _, object := range renderedObjects(t, rendered) {
		kind, _ := object["kind"].(string)
		if kind != "StatefulSet" && kind != "Deployment" {
			continue
		}
		metadata, _ := object["metadata"].(map[string]any)
		name, _ := metadata["name"].(string)
		switch {
		case kind == "StatefulSet" && name == "nats":
		case kind == "Deployment" && name == "kodama":
		default:
			continue
		}
		label := kind + "/" + name
		keys, account := secretKeyRefsFromWorkload(t, object)
		workloads[label] = keys
		if account != "" {
			accounts[label] = account
		}
	}
	for _, label := range []string{"StatefulSet/nats", "Deployment/kodama"} {
		if _, ok := workloads[label]; !ok {
			t.Fatalf("rendered stage stack has no %s", label)
		}
		if len(workloads[label]) == 0 {
			t.Errorf("%s reads no %s key through secretKeyRef", label, natsAuthSecretName)
		}
	}
	for label, keys := range workloads {
		for key := range keys {
			if !containsString(bootstrapKeys, key) {
				t.Errorf("%s reads %s key %q; scripts/bootstrap-secrets writes only %v", label, natsAuthSecretName, key, bootstrapKeys)
			}
		}
	}

	// Both workloads must read the halves they need; a subset check over an
	// empty or unrelated set would prove nothing.
	for _, key := range []string{"shell-user", "shell-password", "callout-user", "callout-password"} {
		for _, label := range []string{"StatefulSet/nats", "Deployment/kodama"} {
			if !workloads[label][key] {
				t.Errorf("%s does not read %s key %q", label, natsAuthSecretName, key)
			}
		}
	}
	if !workloads["StatefulSet/nats"]["issuer.pub"] {
		t.Errorf("StatefulSet/nats does not read the issuer public key")
	}
	if !workloads["Deployment/kodama"]["issuer.seed"] {
		t.Errorf("Deployment/kodama does not read the issuer seed")
	}

	renderedAccount, ok := accounts["Deployment/kodama"]
	if !ok || renderedAccount == "" {
		t.Fatalf("Deployment/kodama renders no KODAMA_NATS_CALLOUT_ACCOUNT value")
	}
	if renderedAccount != bootstrapAccount {
		t.Errorf("rendered KODAMA_NATS_CALLOUT_ACCOUNT = %q; scripts/bootstrap-secrets writes %q", renderedAccount, bootstrapAccount)
	}
}

func containsString(values []string, want string) bool {
	return slices.Contains(values, want)
}
