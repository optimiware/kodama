package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validConfig() CoreConfig {
	return CoreConfig{
		Process: Config{HTTPAddr: "127.0.0.1:0", ShutdownTimeout: time.Second, Assembly: AssemblyCore},
		Env:     Test,
		Runtime: RuntimeConfig{
			ServerURL: "http://127.0.0.1:8080", StateDir: "/tmp/rt", EntrypointBinary: "/usr/bin/kodama",
			SigningKeyFile: "/k/runtime", SigningGeneration: 1,
		},
		Database: DatabaseConfig{RuntimeURL: "postgres://kodama_runtime:x@db/kodama"},
		NATS:     NATSConfig{URL: "nats://token@nats:4222", Stream: "s", Namespace: "ns"},
		S3:       S3Config{Endpoint: "http://s3", Region: "us-east-1", Bucket: "b", AccessKey: "a", SecretKey: "s"},
		OIDC:     OIDCConfig{Issuer: "https://issuer.example", Audience: "https://api.example"},
		Git:      GitConfig{ScratchRoot: "/tmp/x"},
		Secrets:  SecretsConfig{Adapter: "keyfile", Keyfile: &KeyfileConfig{Current: "v1", Files: map[string]string{"v1": "/k"}}},

		InstallationID: "inst", CursorKey: make([]byte, 32), IdempotencyHashKey: make([]byte, 32),
		Tuning: DefaultTuning(),
	}
}

func TestCoreConfigValidation(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("the baseline must be valid: %v", err)
	}
	for name, tc := range map[string]struct {
		mutate func(*CoreConfig)
		want   string
	}{
		"no database":           {func(c *CoreConfig) { c.Database.RuntimeURL = "" }, "database"},
		"no broker":             {func(c *CoreConfig) { c.NATS.URL = "" }, "NATS"},
		"no stream":             {func(c *CoreConfig) { c.NATS.Stream = "" }, "stream"},
		"no bucket":             {func(c *CoreConfig) { c.S3.Bucket = "" }, "S3"},
		"no s3 credentials":     {func(c *CoreConfig) { c.S3.SecretKey = "" }, "credentials"},
		"no region":             {func(c *CoreConfig) { c.S3.Region = "" }, "region"},
		"no issuer":             {func(c *CoreConfig) { c.OIDC.Issuer = "" }, "OIDC"},
		"no audience":           {func(c *CoreConfig) { c.OIDC.Audience = "" }, "OIDC"},
		"no installation":       {func(c *CoreConfig) { c.InstallationID = "" }, "installation"},
		"short cursor key":      {func(c *CoreConfig) { c.CursorKey = make([]byte, 31) }, "cursor"},
		"short hash key":        {func(c *CoreConfig) { c.IdempotencyHashKey = nil }, "idempotency"},
		"no scratch":            {func(c *CoreConfig) { c.Git.ScratchRoot = "" }, "scratch"},
		"no key provider":       {func(c *CoreConfig) { c.Secrets = SecretsConfig{} }, "key provider"},
		"missing keyfile":       {func(c *CoreConfig) { c.Secrets.Keyfile = nil }, "keyfile configuration"},
		"keyfile without files": {func(c *CoreConfig) { c.Secrets.Keyfile.Files = nil }, "key files"},
		"missing aws region":    {func(c *CoreConfig) { c.Secrets = SecretsConfig{Adapter: "kms", KMS: &CloudKMSConfig{Provider: "aws"}} }, "aws.region"},
		"missing gcp key":       {func(c *CoreConfig) { c.Secrets = SecretsConfig{Adapter: "kms", KMS: &CloudKMSConfig{Provider: "gcp"}} }, "KEK identity"},
		"unknown provider": {func(c *CoreConfig) {
			c.Secrets = SecretsConfig{Adapter: "kms", KMS: &CloudKMSConfig{Provider: "vault"}}
		}, "kms.provider"},
		"invalid limits":        {func(c *CoreConfig) { c.Tuning.Limit.TenantConcurrency = 0 }, "execution limits"},
		"no rate limit":         {func(c *CoreConfig) { c.Tuning.RateLimitPerMinute = 0 }, "rate limit"},
		"no reconcile interval": {func(c *CoreConfig) { c.Tuning.ReconcileInterval = 0 }, "reconcile"},
		"no shutdown timeout":   {func(c *CoreConfig) { c.Process.ShutdownTimeout = 0 }, "shutdown"},
		"credential without prefix": {func(c *CoreConfig) {
			c.Git.Credentials = []GitCredential{{Ref: "secret://installation/a", Username: "u", Password: "p", Tenants: []string{"*"}}}
		}, "https"},
		"credential over http": {func(c *CoreConfig) {
			c.Git.Credentials = []GitCredential{{Ref: "secret://installation/a", Username: "u", Password: "p", URLPrefix: "http://h/", Tenants: []string{"*"}}}
		}, "https"},
		"credential for no tenant": {func(c *CoreConfig) {
			c.Git.Credentials = []GitCredential{{Ref: "secret://installation/a", Username: "u", Password: "p", URLPrefix: "https://h/"}}
		}, "tenants"},
		"credential outside scheme": {func(c *CoreConfig) {
			c.Git.Credentials = []GitCredential{{Ref: "secret://project/a", Username: "u", Password: "p", URLPrefix: "https://h/", Tenants: []string{"*"}}}
		}, "secret://installation"},
		"credential with user-info": {func(c *CoreConfig) {
			c.Git.Credentials = []GitCredential{{Ref: "secret://installation/a", Username: "u", Password: "p", URLPrefix: "https://x:y@h/", Tenants: []string{"*"}}}
		}, "https"},
	} {
		c := validConfig()
		tc.mutate(&c)
		err := c.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v (want %q)", name, err, tc.want)
		}
	}
}

func TestLoadCoreConfig(t *testing.T) {
	dir := t.TempDir()
	put := func(name, value string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	put("database-runtime-url", "postgres://kodama_runtime:pw@db:5432/kodama")
	put("s3-access-key", "access")
	put("s3-secret-key", "secret")
	put("nats-token", "tok")
	put("cursor-key", strings.Repeat("c", 32))
	put("idempotency-hash-key", strings.Repeat("h", 32))
	put("master-key", strings.Repeat("m", 32))
	put("git-token", "gitpw")
	put("git-credentials.json", `[{"ref":"secret://installation/checkout","username":"kodama","passwordFile":"git-token","urlPrefix":"https://git.example/","tenants":["t_acme"]}]`)
	env := map[string]string{
		"KODAMA_SECRETS_DIR": dir, "KODAMA_NATS_URL": "nats://nats:4222", "KODAMA_S3_ENDPOINT": "http://s3:4566", "KODAMA_S3_BUCKET": "bucket",
		"KODAMA_OIDC_ISSUER": "https://issuer.example", "KODAMA_OIDC_AUDIENCE": "https://api.example", "KODAMA_INSTALLATION_ID": "inst",
		"KODAMA_SECRETS_ADAPTER": "keyfile",
		"KODAMA_ENV":             "test", "KODAMA_RUNTIME_URL": "http://127.0.0.1:8080", "KODAMA_RUNTIME_STATE_DIR": "/tmp/rt",
		"KODAMA_ENTRYPOINT_BINARY": "/usr/bin/kodama", "KODAMA_RUNTIME_KEY_GENERATION": "3",
	}
	getenv := func(k string) string { return env[k] }
	process, err := LoadConfig(func(k string) string {
		if k == "KODAMA_ASSEMBLY" {
			return "core"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadCoreConfig(getenv, process)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.RuntimeURL != "postgres://kodama_runtime:pw@db:5432/kodama" || cfg.NATS.Token != "tok" || cfg.NATS.Stream != "kodama-events" ||
		cfg.S3.Region != "us-east-1" || !cfg.S3.PathStyle || cfg.S3.AccessKey != "access" || cfg.Secrets.Keyfile.Files["v1"] != filepath.Join(dir, "master-key") {
		t.Fatalf("loaded %+v", cfg)
	}
	if cfg.Env != Test || cfg.Runtime.SigningGeneration != 3 || cfg.Runtime.SigningKeyFile != filepath.Join(dir, "runtime-signing-key") || cfg.Runtime.EntrypointBinary != "/usr/bin/kodama" {
		t.Fatalf("runtime settings %+v", cfg.Runtime)
	}
	if len(cfg.Git.Credentials) != 1 || cfg.Git.Credentials[0].Password != "gitpw" || cfg.Git.Credentials[0].Tenants[0] != "t_acme" {
		t.Fatalf("credentials %+v", cfg.Git.Credentials)
	}

	// A token in the URL means no token file is read.
	env["KODAMA_NATS_URL"] = "nats://urltoken@nats:4222"
	if err := os.Remove(filepath.Join(dir, "nats-token")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCoreConfig(getenv, process); err != nil {
		t.Fatalf("a token in the URL needs no file: %v", err)
	}

	// Every missing secret file is a configuration error naming the file.
	for _, name := range []string{"database-runtime-url", "s3-secret-key", "cursor-key", "idempotency-hash-key"} {
		orig, _ := os.ReadFile(filepath.Join(dir, name))
		_ = os.Remove(filepath.Join(dir, name))
		if _, err := LoadCoreConfig(getenv, process); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("without %s: %v", name, err)
		}
		_ = os.WriteFile(filepath.Join(dir, name), orig, 0o600)
	}
	// Unknown members of the credential file are refused.
	put("git-credentials.json", `[{"ref":"secret://installation/c","username":"u","passwordFile":"git-token","urlPrefix":"https://h/","tenants":["*"],"extra":1}]`)
	if _, err := LoadCoreConfig(getenv, process); err == nil {
		t.Error("an unknown credential member was accepted")
	}
}

// A key is raw bytes: one that starts or ends with a whitespace byte is intact.
func TestKeyFilesAreNotTrimmed(t *testing.T) {
	dir := t.TempDir()
	cursor := append([]byte{'\n'}, append([]byte(strings.Repeat("c", 30)), '\t')...)
	hash := append([]byte(strings.Repeat("h", 31)), ' ')
	for name, data := range map[string][]byte{
		"database-runtime-url": []byte("postgres://r:p@h/db"), "s3-access-key": []byte("a"), "s3-secret-key": []byte("s"), "nats-token": []byte("t"),
		"cursor-key": cursor, "idempotency-hash-key": hash, "master-key": []byte(strings.Repeat("m", 32)),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env := map[string]string{
		"KODAMA_SECRETS_DIR": dir, "KODAMA_NATS_URL": "nats://nats:4222", "KODAMA_S3_ENDPOINT": "http://s3", "KODAMA_S3_BUCKET": "b",
		"KODAMA_OIDC_ISSUER": "https://i", "KODAMA_OIDC_AUDIENCE": "https://a", "KODAMA_INSTALLATION_ID": "i",
		"KODAMA_SECRETS_ADAPTER": "keyfile",
		"KODAMA_ENV":             "test", "KODAMA_RUNTIME_URL": "http://127.0.0.1:8080", "KODAMA_RUNTIME_STATE_DIR": "/tmp/rt",
		"KODAMA_ENTRYPOINT_BINARY": "/usr/bin/kodama", "KODAMA_RUNTIME_KEY_GENERATION": "1",
	}
	cfg, err := LoadCoreConfig(func(k string) string { return env[k] }, Config{ShutdownTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if string(cfg.CursorKey) != string(cursor) || string(cfg.IdempotencyHashKey) != string(hash) {
		t.Fatal("a key file was altered while loading")
	}
}

func TestAssemblySelectionIsExplicit(t *testing.T) {
	c, err := LoadConfig(func(string) string { return "" })
	if err != nil || c.Assembly != AssemblyScaffold {
		t.Fatalf("the default must stay the scaffold: %+v %v", c, err)
	}
	if _, err := LoadConfig(func(k string) string {
		if k == "KODAMA_ASSEMBLY" {
			return "everything"
		}
		return ""
	}); err == nil {
		t.Fatal("an unknown assembly was accepted")
	}
}

// The model/tool statement (T-039) is read from the secrets directory or
// KODAMA_MODEL_TOOLS_FILE, strictly, and absent means nothing is selected.
func TestLoadCoreConfigReadsTheModelToolsStatement(t *testing.T) {
	dir := t.TempDir()
	for name, data := range map[string]string{
		"database-runtime-url": "postgres://r:p@h/db", "s3-access-key": "a", "s3-secret-key": "s", "nats-token": "t",
		"cursor-key": strings.Repeat("c", 32), "idempotency-hash-key": strings.Repeat("h", 32), "master-key": strings.Repeat("m", 32),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env := map[string]string{
		"KODAMA_SECRETS_DIR": dir, "KODAMA_NATS_URL": "nats://nats:4222", "KODAMA_S3_ENDPOINT": "http://s3", "KODAMA_S3_BUCKET": "b",
		"KODAMA_OIDC_ISSUER": "https://i", "KODAMA_OIDC_AUDIENCE": "https://a", "KODAMA_INSTALLATION_ID": "i",
		"KODAMA_SECRETS_ADAPTER": "keyfile",
		"KODAMA_ENV":             "test", "KODAMA_RUNTIME_URL": "http://127.0.0.1:8080", "KODAMA_RUNTIME_STATE_DIR": "/tmp/rt",
		"KODAMA_ENTRYPOINT_BINARY": "/usr/bin/kodama", "KODAMA_RUNTIME_KEY_GENERATION": "1",
	}
	load := func() (CoreConfig, error) {
		return LoadCoreConfig(func(k string) string { return env[k] }, Config{ShutdownTimeout: time.Second})
	}
	cfg, err := load()
	if err != nil || cfg.ModelTools.Configured() {
		t.Fatalf("absent: the installation selects no model access (%v): %+v", err, cfg.ModelTools)
	}
	if err := os.WriteFile(filepath.Join(dir, "model-tools.json"), []byte(modelToolsDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, err = load(); err != nil || !cfg.ModelTools.Configured() || len(cfg.ModelTools.Catalogue) != 1 || cfg.ModelTools.MCP.AllowedHosts[0] != "mcp.example" {
		t.Fatalf("the default file in the secrets directory: %v %+v", err, cfg.ModelTools)
	}
	if err := os.WriteFile(filepath.Join(dir, "model-tools.json"), []byte(`{"gateway":{"origins":[{"origin":"https://m.example"}],"models":{"a":{}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = load(); err == nil || !strings.Contains(err.Error(), "selected together") {
		t.Fatalf("a gateway without an MCP host: %v", err)
	}
	custom := filepath.Join(t.TempDir(), "custom.json")
	if err := os.WriteFile(custom, []byte(modelToolsDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	env["KODAMA_MODEL_TOOLS_FILE"] = custom
	if cfg, err = load(); err != nil || !cfg.ModelTools.Configured() {
		t.Fatalf("the named file wins: %v", err)
	}
	if err := os.WriteFile(custom, []byte(`{"gateway":{"origins":[],"anything":1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = load(); err == nil {
		t.Fatal("an unknown member was accepted")
	}
}
