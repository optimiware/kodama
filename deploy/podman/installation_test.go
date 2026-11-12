package podman

// These tests pin the installation-access prerequisites of the local
// Podman bundle: the private installation descriptor and its identity, the
// derived principal ids, the four OAuthSonas personas with explicit membership
// grants (including the upgrade path for an existing install), the Git HTTPS
// front end and its owned process lifecycle, both pinned workload images, and
// the disposable smoke runner's safety rules. They exercise pure shell logic
// and fake commands in temporary directories; no test starts Podman.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sourceLibScript(body string) string {
	return "set -euo pipefail\n. \"$1\"\nshift\n" + body
}

// derivedPrincipal is the product derivation the descriptor must reproduce:
// "oidc-" plus the first 16 SHA-256 bytes of issuer, NUL and subject.
func derivedPrincipal(issuer, subject string) string {
	sum := sha256.Sum256([]byte(issuer + "\x00" + subject))
	return "oidc-" + hex.EncodeToString(sum[:16])
}

func TestPrincipalIDDerivationMatchesProduct(t *testing.T) {
	lib := filepath.Join(bundleRoot(t), "lib.sh")
	env := shellEnv(t, "principal-test")
	script := sourceLibScript(`principal_id "$1" "$2"`)
	cases := []struct{ issuer, subject string }{
		{"https://localhost:18444", "admin"},
		{"https://localhost:18444", "engineer"},
		{"https://localhost:18444", "reviewer"},
		{"https://localhost:18444", "operator"},
		{"https://oidc.example.test:19444", "person with spaces"},
	}
	for _, testCase := range cases {
		out, errOut, code := runBash(t, env, script, lib, testCase.issuer, testCase.subject)
		if code != 0 {
			t.Fatalf("principal_id %q/%q failed: %s %s", testCase.issuer, testCase.subject, out, errOut)
		}
		want := derivedPrincipal(testCase.issuer, testCase.subject)
		if strings.TrimSpace(out) != want {
			t.Fatalf("principal_id(%q, %q) = %q; want %q", testCase.issuer, testCase.subject, strings.TrimSpace(out), want)
		}
	}
	for _, empty := range [][2]string{{"", "admin"}, {"https://localhost:18444", ""}} {
		if _, _, code := runBash(t, env, script, lib, empty[0], empty[1]); code == 0 {
			t.Fatalf("principal_id accepted the empty issuer/subject pair %q", empty)
		}
	}
}

type installationDescriptor struct {
	SchemaVersion     int               `json:"schema_version"`
	InstallationID    string            `json:"installation_id"`
	Profile           string            `json:"profile"`
	Runtime           string            `json:"runtime"`
	Revision          string            `json:"revision"`
	ServerImageDigest string            `json:"server_image_digest"`
	TenantID          string            `json:"tenant_id"`
	ProjectID         string            `json:"project_id"`
	API               descriptorAPI     `json:"api"`
	Images            descriptorImages  `json:"images"`
	OAuthSonas        descriptorOAuth   `json:"oauthsonas"`
	Credentials       map[string]string `json:"credentials"`
	Git               descriptorGit     `json:"git"`
}

type descriptorAPI struct {
	URL            string `json:"url"`
	CAFile         string `json:"ca_file"`
	TLSServerName  string `json:"tls_server_name"`
	WorkloadURL    string `json:"workload_url"`
	WorkloadCAFile string `json:"workload_ca_file"`
}

type descriptorImages struct {
	Tools           string `json:"tools"`
	ExternalHarness string `json:"external_harness"`
}

type descriptorPersona struct {
	ID          string `json:"id"`
	Subject     string `json:"subject"`
	PrincipalID string `json:"principal_id"`
}

type descriptorOAuth struct {
	Issuer         string                       `json:"issuer"`
	Audience       string                       `json:"audience"`
	CAFile         string                       `json:"ca_file"`
	ClientID       string                       `json:"client_id"`
	RedirectURI    string                       `json:"redirect_uri"`
	Resolve        string                       `json:"resolve"`
	AccessCodeFile string                       `json:"access_code_file"`
	Personas       map[string]descriptorPersona `json:"personas"`
}

type descriptorRepository struct {
	Name       string `json:"name"`
	ResourceID string `json:"resource_id"`
	BindingID  string `json:"binding_id"`
}

type descriptorGit struct {
	URL              string                 `json:"url"`
	WorkloadURL      string                 `json:"workload_url"`
	CAFile           string                 `json:"ca_file"`
	WorkloadCAFile   string                 `json:"workload_ca_file"`
	ServiceID        string                 `json:"service_id"`
	ServiceTokenFile string                 `json:"service_token_file"`
	AdminBindingID   string                 `json:"admin_binding_id"`
	AdminResourceID  string                 `json:"admin_resource_id"`
	Repositories     []descriptorRepository `json:"repositories"`
}

// writeDescriptor runs the shell descriptor builder and returns the file it
// wrote plus its raw bytes.
func writeDescriptor(t *testing.T, env map[string]string, revision, serverDigest string) (string, []byte) {
	t.Helper()
	lib := filepath.Join(bundleRoot(t), "lib.sh")
	directory := t.TempDir()
	out := filepath.Join(directory, "installation.json")
	state := env["KODAMA_LOCAL_STATE_DIR"]
	if err := os.MkdirAll(filepath.Join(state, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A token value must never reach the descriptor.
	if err := os.WriteFile(filepath.Join(state, "secrets", "admin-token"), []byte("SENTINEL-TOKEN-VALUE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := sourceLibScript(`
json="$(installation_descriptor_json "$1" "$2")"
write_private_file "$3" "$json"
printf '%s' "$3"`)
	stdout, stderr, code := runBash(t, env, script, lib, revision, serverDigest, out)
	if code != 0 {
		t.Fatalf("writing the installation descriptor failed: %s %s", stdout, stderr)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return out, data
}

func TestInstallationDescriptorIsCompleteAndPrivate(t *testing.T) {
	env := shellEnv(t, "descriptor-test")
	env["KODAMA_LOCAL_GIT_RESOURCES"] = "admin=git-admin,demo=git-demo"
	revision := strings.Repeat("a", 40)
	serverDigest := "sha256:" + strings.Repeat("b", 64)
	path, raw := writeDescriptor(t, env, revision, serverDigest)

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("installation descriptor mode = %04o; want 0600", mode)
	}
	if strings.Contains(string(raw), "SENTINEL-TOKEN-VALUE") {
		t.Fatal("the installation descriptor contains a raw token value")
	}
	for _, forbidden := range []string{"ca.key", "postgres://", "podman.sock", "PRIVATE KEY", "BEGIN EC"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("the installation descriptor contains %q", forbidden)
		}
	}

	var descriptor installationDescriptor
	if err := json.Unmarshal(raw, &descriptor); err != nil {
		t.Fatalf("descriptor is not JSON: %v\n%s", err, raw)
	}
	if descriptor.SchemaVersion != 1 || descriptor.Profile != "test" || descriptor.Runtime != "podman" {
		t.Fatalf("descriptor header = schema %d profile %q runtime %q", descriptor.SchemaVersion, descriptor.Profile, descriptor.Runtime)
	}
	if descriptor.InstallationID != "installation-descriptor-test" {
		t.Fatalf("installation_id = %q", descriptor.InstallationID)
	}
	if descriptor.Revision != revision {
		t.Fatalf("revision = %q; want the full source revision %q", descriptor.Revision, revision)
	}
	if descriptor.ServerImageDigest != serverDigest {
		t.Fatalf("server_image_digest = %q; want observed %q", descriptor.ServerImageDigest, serverDigest)
	}
	state := env["KODAMA_LOCAL_STATE_DIR"]
	if descriptor.TenantID != "tenant-local" || descriptor.ProjectID != "tenant-local" {
		t.Fatalf("tenant/project = %q/%q", descriptor.TenantID, descriptor.ProjectID)
	}
	if descriptor.API.URL != "https://localhost:18443" ||
		descriptor.API.WorkloadURL != "https://host.containers.internal:18443" ||
		descriptor.API.TLSServerName != "localhost" ||
		descriptor.API.CAFile != filepath.Join(state, "tls", "ca.crt") ||
		descriptor.API.WorkloadCAFile != descriptor.API.CAFile {
		t.Fatalf("api descriptor = %+v", descriptor.API)
	}
	if descriptor.Images.Tools != "docker.io/library/buildpack-deps:trixie-scm@sha256:712f969c9d81a35f9f00cce701faae521891d42a20b592b61bd0e6c5328fad7c" {
		t.Fatalf("tools image = %q", descriptor.Images.Tools)
	}
	if descriptor.Images.ExternalHarness != "docker.io/library/alpine:3.24@sha256:0f49f331d9212aff11b932863be4eb01cdf962969aa86dab7d1ee21ac6f5f52b" {
		t.Fatalf("external harness image = %q", descriptor.Images.ExternalHarness)
	}
	if descriptor.OAuthSonas.Issuer != "https://localhost:18444" ||
		descriptor.OAuthSonas.Audience != "https://localhost:18443" ||
		descriptor.OAuthSonas.ClientID != "kodactl" ||
		descriptor.OAuthSonas.AccessCodeFile != filepath.Join(state, "secrets", "oauth-access-code") {
		t.Fatalf("oauthsonas descriptor = %+v", descriptor.OAuthSonas)
	}
	if len(descriptor.OAuthSonas.Personas) != 4 {
		t.Fatalf("personas = %v; want admin, engineer, reviewer and operator", descriptor.OAuthSonas.Personas)
	}
	for _, persona := range []string{"admin", "engineer", "reviewer", "operator"} {
		entry, ok := descriptor.OAuthSonas.Personas[persona]
		if !ok {
			t.Fatalf("descriptor is missing the %s persona", persona)
		}
		if entry.ID != persona || entry.Subject != persona {
			t.Fatalf("%s persona = %+v", persona, entry)
		}
		if want := derivedPrincipal("https://localhost:18444", persona); entry.PrincipalID != want {
			t.Fatalf("%s principal_id = %q; want derived %q", persona, entry.PrincipalID, want)
		}
	}
	if len(descriptor.Credentials) != 4 {
		t.Fatalf("credentials = %v; want the four persona file keys", descriptor.Credentials)
	}
	for _, persona := range []string{"admin", "engineer", "reviewer", "operator"} {
		if value, ok := descriptor.Credentials[persona+"_file"]; !ok || value != "" {
			t.Fatalf("credentials[%s_file] = %q; want an empty operator-supplied path", persona, value)
		}
	}
	if descriptor.Git.URL != "https://localhost:18445" ||
		descriptor.Git.WorkloadURL != "https://host.containers.internal:18445" ||
		descriptor.Git.ServiceID != "git" ||
		descriptor.Git.AdminBindingID != "git-admin" ||
		descriptor.Git.ServiceTokenFile != filepath.Join(state, "git-credential", "token") {
		t.Fatalf("git descriptor = %+v", descriptor.Git)
	}
	if len(descriptor.Git.Repositories) != 2 {
		t.Fatalf("repositories = %v; want admin and demo", descriptor.Git.Repositories)
	}
	if descriptor.Git.Repositories[0].Name != "admin" ||
		descriptor.Git.Repositories[0].ResourceID != "git-admin" ||
		descriptor.Git.Repositories[1].Name != "demo" ||
		descriptor.Git.Repositories[1].ResourceID != "git-demo" ||
		descriptor.Git.Repositories[1].BindingID != "git-demo" {
		t.Fatalf("repositories = %+v", descriptor.Git.Repositories)
	}

	// A re-run replaces the descriptor atomically and leaves no temporary file.
	first := append([]byte(nil), raw...)
	secondRevision := strings.Repeat("c", 40)
	_, second := writeDescriptor(t, env, secondRevision, "sha256:"+strings.Repeat("d", 64))
	if string(first) == string(second) {
		t.Fatal("a re-run did not refresh the installation descriptor")
	}
	if strings.Contains(string(second), revision) {
		t.Fatal("the refreshed descriptor still carries the previous revision")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp") {
			t.Fatalf("descriptor write left a temporary file: %s", entry.Name())
		}
	}
}

func TestInstallationDescriptorValidationDetectsDrift(t *testing.T) {
	env := shellEnv(t, "descriptor-validate")
	path, raw := writeDescriptor(t, env, strings.Repeat("e", 40), "sha256:"+strings.Repeat("f", 64))
	lib := filepath.Join(bundleRoot(t), "lib.sh")
	script := sourceLibScript(`
if reason="$(validate_installation_descriptor "$1")"; then echo ok; else printf '%s\n' "$reason"; exit 1; fi`)
	if _, errOut, code := runBash(t, env, script, lib, path); code != 0 {
		t.Fatalf("a freshly written descriptor was refused: %s", errOut)
	}

	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	document["installation_id"] = "installation-somebody-else"
	mutated, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	drifted := filepath.Join(t.TempDir(), "drifted.json")
	if err := os.WriteFile(drifted, mutated, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, code := runBash(t, env, script, lib, drifted); code == 0 {
		t.Fatal("a descriptor for another installation was accepted")
	}

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, code := runBash(t, env, script, lib, path); code == 0 {
		t.Fatal("a world-readable descriptor was accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, _, code := runBash(t, env, script, lib, path); code == 0 {
		t.Fatal("a missing descriptor was accepted")
	}
}

// membershipHarness runs grant_persona_membership against a fake API that
// records every call. The mode selects an absent membership, an existing one
// (version 7) or one conflict followed by a fresh observed version.
func membershipHarness(t *testing.T, mode, principal string) ([]string, string) {
	t.Helper()
	env := shellEnv(t, "membership-test")
	state := env["KODAMA_LOCAL_STATE_DIR"]
	if err := os.MkdirAll(filepath.Join(state, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	list := filepath.Join(directory, "list.json")
	calls := filepath.Join(directory, "calls")
	initial := `{"body":{"memberships":[],"next_cursor":""}}`
	if mode == "existing" {
		initial = `{"body":{"memberships":[{"principal_id":"` + principal + `","roles":[],"version":7}],"next_cursor":""}}`
	}
	if err := os.WriteFile(list, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join(bundleRoot(t), "lib.sh")
	script := sourceLibScript(`
list="$1"
calls="$2"
mode="$3"
principal="$4"
roles="$5"
api_call() {
  local method=$1 path=$2 input=$3 key=$4 body_out=$5 expected=${6:-0}
  printf '%s|%s|%s\n' "$method" "$path" "$expected" >>"$calls"
  if [ "$method" = "GET" ]; then
    cp -- "$list" "$body_out"
    printf '200' >"${body_out}.status"
    return 0
  fi
  if [ "$mode" = "conflict" ] && [ "$expected" = "0" ]; then
    jq -n --arg p "$principal" '{body:{memberships:[{principal_id:$p,roles:[],version:3}],next_cursor:""}}' >"$list.tmp"
    mv -- "$list.tmp" "$list"
    printf '{"code":"version_conflict","message":"conflict"}' >"$body_out"
    printf '409' >"${body_out}.status"
    return 1
  fi
  local next
  next=$((expected + 1))
  jq -n --arg p "$principal" --argjson roles "$(jq '.roles' <<<"$input")" --argjson version "$next" \
    '{body:{memberships:[{principal_id:$p,roles:$roles,version:$version}],next_cursor:""}}' >"$list.tmp"
  mv -- "$list.tmp" "$list"
  jq -n --argjson version "$next" '{body:{version:$version}}' >"$body_out"
  printf '200' >"${body_out}.status"
  return 0
}
grant_persona_membership "$principal" "$roles" test
`)
	_, errOut, code := runBash(t, env, script, lib, list, calls, mode, principal, `["engineer"]`)
	if code != 0 {
		t.Fatalf("grant_persona_membership failed in mode %s: %s", mode, errOut)
	}
	callData, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(callData)), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	final, err := os.ReadFile(list)
	if err != nil {
		t.Fatal(err)
	}
	return lines, string(final)
}

func membershipRoles(t *testing.T, document string) []string {
	t.Helper()
	var response struct {
		Body struct {
			Memberships []struct {
				Roles []string `json:"roles"`
			} `json:"memberships"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(document), &response); err != nil {
		t.Fatalf("membership.list response is not JSON: %v\n%s", err, document)
	}
	if len(response.Body.Memberships) != 1 {
		t.Fatalf("membership.list returned %d memberships; want 1", len(response.Body.Memberships))
	}
	return response.Body.Memberships[0].Roles
}

func TestMembershipGrantsUseObservedVersionAndVerify(t *testing.T) {
	fresh, final := membershipHarness(t, "fresh", "oidc-fresh")
	if !strings.Contains(strings.Join(fresh, "\n"), "PUT|/api/v1/projects/tenant-local/memberships/oidc-fresh|0") {
		t.Fatalf("a first grant did not omit the expected version: %v", fresh)
	}
	if roles := membershipRoles(t, final); len(roles) != 1 || roles[0] != "engineer" {
		t.Fatalf("the first grant was not verified from membership.list: %v", roles)
	}

	existing, _ := membershipHarness(t, "existing", "oidc-existing")
	if !strings.Contains(strings.Join(existing, "\n"), "PUT|/api/v1/projects/tenant-local/memberships/oidc-existing|7") {
		t.Fatalf("an existing membership did not use its observed version: %v", existing)
	}

	conflict, final := membershipHarness(t, "conflict", "oidc-conflict")
	joined := strings.Join(conflict, "\n")
	if !strings.Contains(joined, "PUT|/api/v1/projects/tenant-local/memberships/oidc-conflict|0") ||
		!strings.Contains(joined, "PUT|/api/v1/projects/tenant-local/memberships/oidc-conflict|3") {
		t.Fatalf("a version conflict was not retried from a fresh observation: %v", conflict)
	}
	if roles := membershipRoles(t, final); len(roles) != 1 || roles[0] != "engineer" {
		t.Fatalf("the retried grant was not verified: %v", roles)
	}
}

func TestInstallShapesFourPersonasAndUpgradePath(t *testing.T) {
	install := readFile(t, filepath.Join(bundleRoot(t), "install.sh"))
	for _, persona := range []string{"engineer", "reviewer", "operator"} {
		if !strings.Contains(install, `grant_persona_membership "$`+persona+`_principal"`) {
			t.Fatalf("install.sh does not grant the %s persona its membership", persona)
		}
	}
	// The upgrade path must not hide the grant behind the first-run roles marker.
	markerIndex := strings.Index(install, `if [ ! -s "$roles_marker" ]; then`)
	grantIndex := strings.Index(install, `grant_persona_membership "$engineer_principal"`)
	if markerIndex < 0 || grantIndex < 0 {
		t.Fatal("install.sh is missing the roles marker block or the engineer grant")
	}
	blockEnd := strings.Index(install[markerIndex:], "\nfi")
	if blockEnd < 0 || grantIndex < markerIndex+blockEnd {
		t.Fatal("the persona grants are hidden behind the first-run roles marker")
	}
	operatorPersona := `- id: operator`
	if !strings.Contains(install, operatorPersona) {
		t.Fatal("the generated OAuthSonas configuration has no operator persona")
	}
	// The upgrade path must not rely on the old roles.done marker either.
	if strings.Contains(install, `[ -s "$roles_marker" ] &&`) {
		t.Fatal("install.sh conditions the persona grants on the old roles marker")
	}
}

func TestGitHTTPSPortCollisionAndLeafSAN(t *testing.T) {
	lib := filepath.Join(bundleRoot(t), "lib.sh")
	base := shellEnv(t, "git-https")

	print := sourceLibScript(`printf '%s' "$GIT_HTTPS_PORT"`)
	out, errOut, code := runBash(t, base, print, lib)
	if code != 0 {
		t.Fatalf("sourcing lib.sh failed: %s", errOut)
	}
	if strings.TrimSpace(out) != "18445" {
		t.Fatalf("default KODAMA_LOCAL_GIT_HTTPS_PORT = %q; want 18445", strings.TrimSpace(out))
	}

	collision := map[string]string{}
	maps.Copy(collision, base)
	collision["KODAMA_LOCAL_GIT_PORT"] = "19445"
	collision["KODAMA_LOCAL_GIT_HTTPS_PORT"] = "19445"
	if _, _, code := runBash(t, collision, print, lib); code == 0 {
		t.Fatal("a colliding Git HTTPS port was accepted")
	}

	san := sourceLibScript(`forwarder_san "$1" "$2"`)
	out, errOut, code = runBash(t, base, san, lib, "host.containers.internal", "10.9.8.7")
	if code != 0 {
		t.Fatalf("forwarder_san failed: %s", errOut)
	}
	want := "DNS:localhost,DNS:host.containers.internal,IP:127.0.0.1,IP:10.9.8.7"
	if strings.TrimSpace(out) != want {
		t.Fatalf("forwarder SAN = %q; want %q", strings.TrimSpace(out), want)
	}

	install := readFile(t, filepath.Join(bundleRoot(t), "install.sh"))
	for _, phrase := range []string{"KODAMA_LOCAL_GIT_HTTPS_PORT", "forwarder_san", "git_socat_pid_file", "GIT_WORKLOAD_URL"} {
		if !strings.Contains(install, phrase) {
			t.Fatalf("install.sh does not implement the Git HTTPS front end: missing %q", phrase)
		}
	}
}

func startStandIn(t *testing.T, args []string) *os.Process {
	t.Helper()
	process, err := os.StartProcess("/bin/sleep", args, &os.ProcAttr{
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	})
	if err != nil {
		t.Fatalf("start stand-in process: %v", err)
	}
	return process
}

func waitWithTimeout(t *testing.T, process *os.Process, timeout time.Duration) {
	t.Helper()
	done := make(chan error, 1)
	go func() { _, err := process.Wait(); done <- err }()
	select {
	case <-done:
	case <-time.After(timeout):
		_ = process.Kill()
		<-done
		t.Fatal("the stand-in process did not exit within the bound")
	}
}

func TestStopForwarderNeverSignalsStalePidFile(t *testing.T) {
	lib := filepath.Join(bundleRoot(t), "lib.sh")
	env := shellEnv(t, "forwarder-test")
	directory := t.TempDir()
	pidFile := filepath.Join(directory, "forwarder.pid")
	confFile := filepath.Join(directory, "forwarder.conf")
	script := sourceLibScript(`stop_forwarder "$1" "$2" "$3" "test forwarder"`)

	// A pid file naming a live process that is not our forwarder must not be
	// signalled, and the stale files are removed.
	unrelated := startStandIn(t, []string{"sleep", "30"})
	defer func() {
		_ = unrelated.Kill()
		_, _ = unrelated.Wait()
	}()
	if err := os.WriteFile(pidFile, []byte(strings.TrimSpace(itoa(unrelated.Pid))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(confFile, []byte("listen=0.0.0.0:19445\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := runBash(t, env, script, lib, pidFile, confFile, "OPENSSL-LISTEN:19445,"); code != 0 {
		t.Fatalf("stop_forwarder failed on a stale pid file: %s", errOut)
	}
	if _, err := os.Stat(filepath.Join("/proc", itoa(unrelated.Pid))); err != nil {
		t.Fatal("stop_forwarder signalled a process that does not match its exact command")
	}
	for _, path := range []string{pidFile, confFile} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("stop_forwarder did not remove the stale file %s", path)
		}
	}

	// A live process whose exact command carries this installation's listen
	// port is the forwarder and is terminated and joined.
	owned := startStandIn(t, []string{"socat OPENSSL-LISTEN:19445,", "30"})
	if err := os.WriteFile(pidFile, []byte(strings.TrimSpace(itoa(owned.Pid))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(confFile, []byte("listen=0.0.0.0:19445\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := runBash(t, env, script, lib, pidFile, confFile, "OPENSSL-LISTEN:19445,"); code != 0 {
		t.Fatalf("stop_forwarder failed on the owned process: %s", errOut)
	}
	waitWithTimeout(t, owned, 15*time.Second)
}

// itoa avoids importing strconv for one call.
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

func TestFreshStateCheckRefusesForeignContent(t *testing.T) {
	lib := filepath.Join(bundleRoot(t), "lib.sh")
	env := shellEnv(t, "fresh-state")
	script := sourceLibScript(`require_fresh_state_dir "$1" "$2"`)
	root := t.TempDir()

	absent := filepath.Join(root, "absent")
	if _, _, code := runBash(t, env, script, lib, absent, "fresh-state"); code != 0 {
		t.Fatal("an absent state directory was refused")
	}
	empty := filepath.Join(root, "empty")
	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, code := runBash(t, env, script, lib, empty, "fresh-state"); code != 0 {
		t.Fatal("an empty state directory was refused")
	}
	foreign := filepath.Join(root, "foreign")
	if err := os.Mkdir(foreign, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreign, ".kodama-local"), []byte("prefix=other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, code := runBash(t, env, script, lib, foreign, "fresh-state"); code == 0 {
		t.Fatal("a foreign state directory was accepted")
	}
	unmarked := filepath.Join(root, "unmarked")
	if err := os.Mkdir(unmarked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unmarked, "data"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, code := runBash(t, env, script, lib, unmarked, "fresh-state"); code == 0 {
		t.Fatal("a non-empty unmarked state directory was accepted")
	}
	owned := filepath.Join(root, "owned")
	if err := os.Mkdir(owned, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(owned, ".kodama-local"), []byte("prefix=fresh-state\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A stopped installation keeps its CA, keyfile and volumes under this
	// directory; a marker that matches the prefix is not a reuse licence.
	if _, _, code := runBash(t, env, script, lib, owned, "fresh-state"); code == 0 {
		t.Fatal("a non-empty state directory with a matching marker was accepted")
	}
}

func TestCombineStatusPreservesTheTestFailure(t *testing.T) {
	lib := filepath.Join(bundleRoot(t), "lib.sh")
	env := shellEnv(t, "status-test")
	script := sourceLibScript(`combine_status "$1" "$2"`)
	cases := []struct {
		testStatus    string
		cleanupStatus string
		want          string
	}{
		{"0", "0", "0"},
		{"0", "1", "1"},
		{"7", "0", "7"},
		{"7", "1", "7"},
		{"3", "9", "3"},
	}
	for _, testCase := range cases {
		out, errOut, code := runBash(t, env, script, lib, testCase.testStatus, testCase.cleanupStatus)
		if code != 0 {
			t.Fatalf("combine_status failed: %s", errOut)
		}
		if strings.TrimSpace(out) != testCase.want {
			t.Fatalf("combine_status(%s, %s) = %q; want %q", testCase.testStatus, testCase.cleanupStatus, strings.TrimSpace(out), testCase.want)
		}
	}
}

func TestSmokeRequiresSlotAndCompletePortSet(t *testing.T) {
	smoke := filepath.Join(bundleRoot(t), "smoke.sh")
	ports := map[string]string{
		"KODAMA_LOCAL_HTTPS_PORT":          "20443",
		"KODAMA_LOCAL_OIDC_PORT":           "20444",
		"KODAMA_LOCAL_GIT_PORT":            "20081",
		"KODAMA_LOCAL_GIT_HTTPS_PORT":      "20445",
		"KODAMA_LOCAL_POSTGRES_PORT":       "20432",
		"KODAMA_LOCAL_NATS_PORT":           "20222",
		"KODAMA_LOCAL_NATS_MONITOR_PORT":   "20282",
		"KODAMA_LOCAL_OAUTH_UPSTREAM_PORT": "20181",
	}
	env := shellEnv(t, "smoke-test")
	maps.Copy(env, ports)
	_, errOut, code := runBash(t, env, `bash "$1"`, smoke)
	if code == 0 {
		t.Fatal("smoke.sh ran without KODAMA_SLOT")
	}
	if !strings.Contains(errOut, "KODAMA_SLOT") {
		t.Fatalf("smoke.sh did not name the missing slot: %s", errOut)
	}

	env["KODAMA_SLOT"] = "stack-1"
	delete(env, "KODAMA_LOCAL_NATS_PORT")
	_, errOut, code = runBash(t, env, `bash "$1"`, smoke)
	if code == 0 {
		t.Fatal("smoke.sh ran with an incomplete port set")
	}
	if !strings.Contains(errOut, "KODAMA_LOCAL_NATS_PORT") {
		t.Fatalf("smoke.sh did not name the missing port: %s", errOut)
	}
}

func TestSmokeScriptOwnsItsOrchestration(t *testing.T) {
	smoke := readFile(t, filepath.Join(bundleRoot(t), "smoke.sh"))
	for _, phrase := range []string{
		"KODAMA_SLOT",
		"--preflight",
		"run-installation.sh",
		"git provision-repository",
		"resource.binding.register",
		"down.sh",
		"--purge",
		"trap ",
		"installation_file",
		"Q01_INSTALLATION_FILE",
		"combine_status",
		"require_fresh_state_dir",
		"KODAMA_LOCAL_GIT_RESOURCES",
		"smoke_repository",
		`add_smoke_repository "$installation_file" "$repository" "$resource" "$resource"`,
	} {
		if !strings.Contains(smoke, phrase) {
			t.Fatalf("smoke.sh omits %q", phrase)
		}
	}
	if strings.Contains(smoke, "pipeline.start") {
		t.Fatal("smoke.sh implements a public scenario instead of invoking the common runner")
	}
}

func TestProjectImageCarriesTheGitHTTPBackend(t *testing.T) {
	containerfile := readFile(t, filepath.Join(repoRoot(t), "deploy", "images", "kodama", "Containerfile"))
	if !strings.Contains(containerfile, "git-daemon") {
		t.Fatal("deploy/images/kodama/Containerfile does not install Alpine's git-daemon package")
	}
	if !strings.Contains(containerfile, "git-http-backend") {
		t.Fatal("the Containerfile does not explain why git-daemon is required")
	}
	install := readFile(t, filepath.Join(bundleRoot(t), "install.sh"))
	for _, phrase := range []string{"git_backend_present", "git-http-backend"} {
		if !strings.Contains(install, phrase) {
			t.Fatalf("install.sh does not verify the Git smart-HTTP backend: missing %q", phrase)
		}
	}
	if strings.Contains(install, "Containerfile.runtime") {
		t.Fatal("install.sh still builds the removed bundle overlay")
	}
	runbook := readFile(t, filepath.Join(repoRoot(t), "docs", "runbooks", "podman-quickstart.md"))
	if strings.Contains(runbook, "Containerfile.runtime") {
		t.Fatal("the quickstart still documents the removed bundle overlay")
	}
}

func TestSmokeAddsRunRepositoryToDescriptor(t *testing.T) {
	env := shellEnv(t, "descriptor-smoke")
	path, raw := writeDescriptor(t, env, strings.Repeat("1", 40), "sha256:"+strings.Repeat("2", 64))
	var before map[string]any
	if err := json.Unmarshal(raw, &before); err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join(bundleRoot(t), "lib.sh")
	script := sourceLibScript(`add_smoke_repository "$1" "$2" "$3" "$4"`)
	if _, errOut, code := runBash(t, env, script, lib, path, "smoke/run-1", "w49-repo-run-1", "w49-repo-run-1"); code != 0 {
		t.Fatalf("adding the smoke repository failed: %s", errOut)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("descriptor mode after the smoke repository = %04o; want 0600", info.Mode().Perm())
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(after, &document); err != nil {
		t.Fatal(err)
	}
	repository, ok := document["smoke_repository"].(map[string]any)
	if !ok {
		t.Fatalf("descriptor has no smoke_repository: %s", after)
	}
	if repository["name"] != "smoke/run-1" || repository["resource_id"] != "w49-repo-run-1" || repository["binding_id"] != "w49-repo-run-1" {
		t.Fatalf("smoke_repository = %v", repository)
	}
	if document["installation_id"] != before["installation_id"] {
		t.Fatal("adding the smoke repository disturbed the installation identity")
	}
}

func TestStatusAndDownCoverBothForwardersAndDescriptor(t *testing.T) {
	status := readFile(t, filepath.Join(bundleRoot(t), "status.sh"))
	down := readFile(t, filepath.Join(bundleRoot(t), "down.sh"))
	for _, phrase := range []string{"installation_file", "validate_installation_descriptor", "git_socat_pid_file", "GIT_HTTPS_PORT"} {
		if !strings.Contains(status, phrase) {
			t.Fatalf("status.sh omits %q", phrase)
		}
	}
	for _, phrase := range []string{"git_socat_pid_file", "git_socat_conf_file", "OPENSSL-LISTEN:${GIT_HTTPS_PORT},", "OPENSSL-LISTEN:${OIDC_PORT},"} {
		if !strings.Contains(down, phrase) {
			t.Fatalf("down.sh omits %q", phrase)
		}
	}
}

func TestBundlePrechecksBothWorkloadImages(t *testing.T) {
	lib := readFile(t, filepath.Join(bundleRoot(t), "lib.sh"))
	install := readFile(t, filepath.Join(bundleRoot(t), "install.sh"))
	runbook := readFile(t, filepath.Join(repoRoot(t), "docs", "runbooks", "podman-quickstart.md"))
	for _, reference := range []string{
		"docker.io/library/buildpack-deps:trixie-scm@sha256:712f969c9d81a35f9f00cce701faae521891d42a20b592b61bd0e6c5328fad7c",
		"docker.io/library/alpine:3.24@sha256:0f49f331d9212aff11b932863be4eb01cdf962969aa86dab7d1ee21ac6f5f52b",
	} {
		if !strings.Contains(lib, reference) {
			t.Fatalf("lib.sh does not default to the pinned workload image %q", reference)
		}
		if !strings.Contains(install, "EXTERNAL_HARNESS_IMAGE") || !strings.Contains(install, "WORKLOAD_IMAGE") {
			t.Fatal("install.sh does not precheck both workload images")
		}
		if !strings.Contains(runbook, strings.Split(reference, "@")[0]) {
			t.Fatalf("the quickstart does not list the workload image %q", reference)
		}
	}
}

func TestQuickstartDocumentsGitHTTPSAndPersonas(t *testing.T) {
	runbook := readFile(t, filepath.Join(repoRoot(t), "docs", "runbooks", "podman-quickstart.md"))
	for _, phrase := range []string{
		"KODAMA_LOCAL_GIT_HTTPS_PORT",
		"18445",
		"host.containers.internal:18445",
		"git provision-repository",
		"operator",
		"membership",
		"installation.json",
		"smoke.sh",
	} {
		if !strings.Contains(runbook, phrase) {
			t.Fatalf("the quickstart omits %q", phrase)
		}
	}
	if strings.Contains(runbook, "A trusted origin must be the installation's absolute HTTPS origin") {
		t.Fatal("the quickstart still teaches the API origin as the Git binding origin")
	}
}
