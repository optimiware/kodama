package devenv

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/shell/serve"
)

func composeServices(t *testing.T) map[string]map[string]any {
	t.Helper()
	data, err := os.ReadFile(ComposeFile())
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Services
}

func stringsOf(v any) []string {
	var out []string
	items, _ := v.([]any)
	for _, item := range items {
		out = append(out, item.(string))
	}
	return out
}

func TestUnitGitHostingSelection(t *testing.T) {
	for _, hosting := range []string{GitFixture, GitNative} {
		if err := ValidateGitHosting(hosting); err != nil {
			t.Fatalf("%s: %v", hosting, err)
		}
	}
	for _, value := range []string{"", "internal", "Native", "native ", "fixture,native"} {
		if err := ValidateGitHosting(value); err == nil {
			t.Fatalf("%q was accepted", value)
		}
	}
	if DefaultGitHosting != GitFixture {
		t.Fatalf("the default hosting is %q: native hosting must be selected explicitly", DefaultGitHosting)
	}
	if value, err := RequestedGitHosting("native", []string{"DEV_GIT=fixture"}); err != nil || value != "native" {
		t.Fatalf("argument selection = %q %v", value, err)
	}
	if value, err := RequestedGitHosting("", []string{"DEV_GIT=native"}); err != nil || value != "native" {
		t.Fatalf("environment selection = %q %v", value, err)
	}
	if value, err := RequestedGitHosting("", nil); err != nil || value != "" {
		t.Fatalf("unset selection = %q %v", value, err)
	}
	if _, err := RequestedGitHosting("", []string{"DEV_GIT="}); err == nil {
		t.Fatal("an explicit empty DEV_GIT was accepted")
	}
	for _, tenant := range []string{"t_acme", "t_0f3a5c9e1b7d4a6e8c2b4d6f", "acme-1"} {
		if err := ValidateGitTenant(tenant); err != nil {
			t.Fatalf("%s: %v", tenant, err)
		}
	}
	for _, tenant := range []string{"", "T_acme", "_acme", "t acme", "t/acme", "t.acme", "../x", strings.Repeat("a", 41)} {
		if err := ValidateGitTenant(tenant); err == nil {
			t.Fatalf("tenant %q was accepted", tenant)
		}
	}
}

// The Compose file selects the two Git hostings by profile: nothing Git-related
// starts unless a profile asks for it, the compatibility fixture is one profile and
// the native worker the other, and the worker is confined like the fixture.
func TestUnitComposeGitProfiles(t *testing.T) {
	services := composeServices(t)
	if got := stringsOf(services["git"]["profiles"]); len(got) != 1 || got[0] != "git-fixture" {
		t.Fatalf("the git-http-backend fixture profiles = %v", got)
	}
	worker := services["git-worker"]
	if worker == nil {
		t.Fatal("no git-worker service")
	}
	if got := stringsOf(worker["profiles"]); len(got) != 1 || got[0] != "git-native" {
		t.Fatalf("the native worker profiles = %v", got)
	}
	for name, service := range services {
		if name != "git" && name != "git-worker" && service["profiles"] != nil {
			t.Errorf("%s is profiled: only the two Git hostings are optional", name)
		}
	}
	if got := stringsOf(worker["command"]); len(got) != 2 || got[0] != "git" || got[1] != "serve" {
		t.Fatalf("the worker command = %v: it is `kodama git serve`", got)
	}
	if worker["user"] != "65532:65532" || strings.Join(stringsOf(worker["cap_drop"]), ",") != "ALL" ||
		strings.Join(stringsOf(worker["security_opt"]), ",") != "no-new-privileges:true" {
		t.Fatalf("the worker is not confined: %v", worker)
	}
	if ports := stringsOf(worker["ports"]); len(ports) != 1 || !strings.HasPrefix(ports[0], "127.0.0.1:") {
		t.Fatalf("the worker publishes %v: only a loopback port", ports)
	}
	// What the worker is given: its volume and its own secrets, never the scope's
	// fixtures (which hold the signing keys) and never a database or broker URL.
	for _, volume := range stringsOf(worker["volumes"]) {
		if strings.Contains(volume, "KODAMA_FIXTURES") {
			t.Errorf("the worker mounts the scope fixtures: %s", volume)
		}
	}
	env, _ := worker["environment"].(map[string]any)
	for _, required := range []string{"KODAMA_GIT_TENANT_ID", "KODAMA_GIT_DATA_ROOT", "KODAMA_GIT_CONTROL_URL", "KODAMA_GIT_WORKER_IDENTITY_FILE",
		"KODAMA_GIT_FORWARDING_PUBLIC_KEYS_FILE", "KODAMA_GIT_CONTROL_TOKEN_FILE"} {
		if env[required] == nil {
			t.Errorf("the worker does not set %s, which `kodama git serve` requires", required)
		}
	}
	for name := range env {
		if strings.Contains(name, "DATABASE") || strings.Contains(name, "NATS") || strings.Contains(name, "SECRETS") {
			t.Errorf("the worker is given %s: it holds no database or broker credential", name)
		}
	}
	labels, _ := worker["labels"].(map[string]any)
	if labels["dev.kodama.owner"] == nil {
		t.Fatal("the worker does not carry the ownership label: teardown would not find it")
	}
}

func TestUnitGitHostingDefaultIsTheFixture(t *testing.T) {
	host, environment := newTestEnvironment(t, StackServices)
	if err := environment.Up(); err != nil {
		t.Fatal(err)
	}
	if environment.State.GitHosting != "" || environment.GitMode() != GitFixture {
		t.Fatalf("hosting = %q (%q)", environment.State.GitHosting, environment.GitMode())
	}
	for _, call := range host.verbs("compose", "-p") {
		if !strings.Contains(strings.Join(call, " "), "--profile git-fixture") || strings.Contains(strings.Join(call, " "), "git-native") {
			t.Fatalf("compose call without the fixture profile only: %v", call)
		}
	}
	values, err := readStringMap(filepath.Join(environment.Dir, "host-env.json"))
	if err != nil {
		t.Fatal(err)
	}
	if values["DEV_GIT"] != GitFixture || values["KODAMA_GIT_CONTROL_TOKEN"] != "" || values["KODAMA_GIT_FORWARDING_KEY_FILE"] != "" {
		t.Fatalf("the fixture scope exposes native settings: %v", values)
	}
	workload, _ := readStringMap(filepath.Join(environment.Dir, "endpoints.json"))
	if workload["KODAMA_GIT_URL"] == "" || workload["KODAMA_GIT_WORKER_URL"] != "" {
		t.Fatalf("the fixture scope's endpoints = %v", workload)
	}
	if exists(filepath.Join(environment.Dir, gitServerDir)) || exists(filepath.Join(environment.Dir, "fixtures", gitForwardingKey)) {
		t.Fatal("a fixture scope generated Git signing keys")
	}
	if !strings.Contains(environment.probeChecks(), "https_git") {
		t.Fatalf("the fixture scope does not probe its Git fixture: %s", environment.probeChecks())
	}
	// A scope created before the selection existed is a fixture scope.
	environment.State.GitHosting = ""
	if environment.GitMode() != GitFixture {
		t.Fatal("a legacy scope is not a fixture scope")
	}
	if err := environment.GitWorker("t_acme", ""); err == nil || !strings.Contains(err.Error(), "fixture") {
		t.Fatalf("a native worker was started in a fixture scope: %v", err)
	}
}

func TestUnitNativeGitHosting(t *testing.T) {
	t.Run("only the services stack hosts native workers", func(t *testing.T) {
		host, environment := newTestEnvironment(t, StackCluster)
		environment.RequestedGit = GitNative
		if err := environment.Up(); err == nil || !strings.Contains(err.Error(), "needs DEV_STACK=services") {
			t.Fatalf("up on the cluster stack = %v", err)
		}
		if len(host.calls) != 0 {
			t.Fatalf("commands ran before the refusal: %v", host.calls)
		}
	})

	t.Run("a scope keeps the hosting it was created with", func(t *testing.T) {
		_, environment := newTestEnvironment(t, StackServices)
		if err := environment.Up(); err != nil {
			t.Fatal(err)
		}
		again, err := Load(environment.Scope, environment.Provider, StackServices)
		if err != nil {
			t.Fatal(err)
		}
		again.RequestedGit = GitNative
		if err := again.Up(); err == nil || !strings.Contains(err.Error(), "created with DEV_GIT=fixture") {
			t.Fatalf("up = %v", err)
		}
	})

	host, environment := newTestEnvironment(t, StackServices)
	environment.RequestedGit = GitNative
	if err := environment.GitWorker("t_acme", ""); err == nil || !strings.Contains(err.Error(), "make dev-up") {
		t.Fatalf("a worker was started before the scope exists: %v", err)
	}
	if err := environment.Up(); err != nil {
		t.Fatal(err)
	}
	if environment.State.GitHosting != GitNative {
		t.Fatalf("state hosting = %q", environment.State.GitHosting)
	}
	for _, call := range host.verbs("compose", "-p") {
		if strings.Contains(strings.Join(call, " "), "git-fixture") {
			t.Fatalf("the compatibility fixture was started: %v", call)
		}
	}
	if strings.Contains(environment.probeChecks(), "https_git") {
		t.Fatalf("the native scope probes a Git fixture it does not run: %s", environment.probeChecks())
	}

	fixtures := filepath.Join(environment.Dir, gitServerDir)
	// The signing keys and the control token are not in the fixtures directory that
	// PostgreSQL, NATS and oauthsonas mount, and only the host user can read them.
	entries, _ := os.ReadDir(filepath.Join(environment.Dir, "fixtures"))
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".pem") || strings.HasPrefix(entry.Name(), "git-forwarding") || strings.HasPrefix(entry.Name(), "git-identity") ||
			entry.Name() == gitControlToken || entry.Name() == gitWorkersDir {
			t.Errorf("the shared fixtures directory holds %s", entry.Name())
		}
	}
	if info, err := os.Stat(fixtures); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("the server-only directory = %v %v", info, err)
	}
	for _, name := range []string{gitForwardingKey, gitIdentityKey, gitControlToken} {
		if info, err := os.Stat(filepath.Join(fixtures, name)); err != nil || info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("%s is readable by others: %v %v", name, info, err)
		}
	}
	for _, name := range []string{gitForwardingKey, gitIdentityKey, gitForwardingPublic, gitControlToken} {
		if !exists(filepath.Join(fixtures, name)) {
			t.Fatalf("fixture %s was not generated", name)
		}
	}
	public, _ := os.ReadFile(filepath.Join(fixtures, gitForwardingPublic))
	if !strings.HasPrefix(string(public), "-----BEGIN PUBLIC KEY-----") || strings.Contains(string(public), "PRIVATE") {
		t.Fatalf("the forwarding public key file = %q", public)
	}
	hostEnv, err := readStringMap(filepath.Join(environment.Dir, "host-env.json"))
	if err != nil {
		t.Fatal(err)
	}
	if hostEnv["DEV_GIT"] != GitNative || hostEnv["KODAMA_GIT_FORWARDING_KEY_FILE"] != filepath.Join(fixtures, gitForwardingKey) ||
		hostEnv["KODAMA_GIT_IDENTITY_KEY_FILE"] != filepath.Join(fixtures, gitIdentityKey) || len(hostEnv["KODAMA_GIT_CONTROL_TOKEN"]) != 64 {
		t.Fatalf("host env = %v", hostEnv)
	}
	workload, err := readStringMap(filepath.Join(environment.Dir, "endpoints.json"))
	if err != nil {
		t.Fatal(err)
	}
	wantTemplate := "http://" + environment.State.Cluster + "-git-{tenantId}:8443"
	if workload["KODAMA_GIT_WORKER_URL"] != wantTemplate || workload["KODAMA_GIT_URL"] != "" {
		t.Fatalf("workload env = %v, want the worker template %s and no fixture URL", workload, wantTemplate)
	}
	if !strings.Contains(workload["KODAMA_GIT_WORKER_URL"], "{tenantId}") {
		t.Fatal("the worker URL template has no {tenantId}, which the server requires")
	}
	keyBefore, _ := os.ReadFile(filepath.Join(fixtures, gitIdentityKey))

	// One container per tenant, each in its own project, with a tenant-bound identity.
	for _, tenant := range []string{"t_acme", "t_globex"} {
		if err := environment.GitWorker(tenant, ""); err != nil {
			t.Fatal(err)
		}
	}
	project := environment.State.Cluster + "-git-t_acme"
	started := host.verbs("compose", "-p", project)
	if len(started) != 1 || !strings.Contains(strings.Join(started[0], " "), "--profile git-native up -d git-worker") {
		t.Fatalf("worker compose calls = %v", started)
	}
	if len(host.verbs("compose", "-p", environment.State.Cluster+"-git-t_globex")) != 1 {
		t.Fatal("the second tenant did not get its own project")
	}
	if got := strings.Join(environment.State.GitTenants, ","); got != "t_acme,t_globex" {
		t.Fatalf("journaled tenants = %s", got)
	}
	if after, _ := os.ReadFile(filepath.Join(fixtures, gitIdentityKey)); string(after) != string(keyBefore) {
		t.Fatal("starting a worker replaced the identity key")
	}
	signer, err := serve.NewGitSigner("test", filepath.Join(fixtures, gitForwardingKey), filepath.Join(fixtures, gitIdentityKey))
	if err != nil {
		t.Fatal(err)
	}
	read := func(tenant, name string) string {
		data, err := os.ReadFile(filepath.Join(environment.gitWorkerDir(tenant), name))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	identity, err := signer.Verifier().VerifyWorkerIdentity(context.Background(), read("t_acme", "identity-token"), domain.TenantID("t_acme"), time.Now())
	if err != nil || identity.TenantID != "t_acme" || identity.Audience != gitv1.ControlAudience {
		t.Fatalf("the control plane does not accept the worker's identity: %+v %v", identity, err)
	}
	if _, err := signer.Verifier().VerifyWorkerIdentity(context.Background(), read("t_acme", "identity-token"), domain.TenantID("t_globex"), time.Now()); err == nil {
		t.Fatal("one tenant's worker identity was accepted for another tenant")
	}
	if read("t_acme", "identity-token") == read("t_globex", "identity-token") {
		t.Fatal("two tenants' workers share an identity")
	}
	if read("t_acme", "control-token") != string(mustRead(t, filepath.Join(fixtures, gitControlToken))) {
		t.Fatal("the worker does not hold the control token the server presents")
	}
	if entries, _ := os.ReadDir(environment.gitWorkerDir("t_acme")); len(entries) != 3 {
		t.Fatalf("the worker directory holds %d files, want exactly the public key, the control token and the identity", len(entries))
	}
	if strings.Contains(read("t_acme", "forwarding-public.pem"), "PRIVATE") {
		t.Fatal("a signing key reached a worker")
	}

	// Refreshing renews the identity and keeps the journal and the project.
	before := read("t_acme", "identity-token")
	if err := environment.GitWorker("t_acme", "http://host.containers.internal:9090"); err != nil {
		t.Fatal(err)
	}
	if read("t_acme", "identity-token") == before {
		t.Fatal("refreshing a worker did not renew its identity")
	}
	if len(environment.State.GitTenants) != 2 {
		t.Fatalf("journaled tenants = %v", environment.State.GitTenants)
	}
	inventory, err := os.ReadFile(filepath.Join(environment.Dir, "git-workers.json"))
	if err != nil || !strings.Contains(string(inventory), "t_globex") || !strings.Contains(string(inventory), "9090") {
		t.Fatalf("inventory = %s %v", inventory, err)
	}

	// Refusals start nothing.
	calls := len(host.calls)
	for _, bad := range []struct{ tenant, control string }{
		{"T_ACME", ""}, {"../escape", ""}, {"", ""},
		{"t_acme", "ftp://x"}, {"t_acme", "http://user:pw@host:8080"}, {"t_acme", "http://host:8080/path?x=1"},
	} {
		if err := environment.GitWorker(bad.tenant, bad.control); err == nil {
			t.Fatalf("%+v was accepted", bad)
		}
	}
	if len(host.calls) != calls {
		t.Fatalf("refused requests ran commands: %v", host.calls[calls:])
	}

	// Teardown removes every tenant's project and volume, then the scope.
	if err := environment.Down(); err != nil {
		t.Fatal(err)
	}
	for _, tenant := range []string{"t_acme", "t_globex"} {
		found := false
		for _, call := range host.calls {
			line := strings.Join(call, " ")
			if strings.Contains(line, "-git-"+tenant+" ") && strings.Contains(line, "down -v") {
				found = true
			}
		}
		if !found {
			t.Errorf("the worker of %s was not torn down with its volume", tenant)
		}
	}
	if exists(environment.Dir) {
		t.Fatal("the scope remains")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
