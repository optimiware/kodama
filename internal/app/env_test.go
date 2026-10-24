package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	execpg "github.com/optimiware/kodama/internal/execution/postgres"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/runtime/podman"
	"github.com/optimiware/kodama/internal/runtime/process"
	"github.com/optimiware/kodama/internal/runtime/runtimetest"
	"github.com/optimiware/kodama/internal/runtimeauth"
	rtpg "github.com/optimiware/kodama/internal/runtimeauth/postgres"
	"github.com/optimiware/kodama/internal/runtimeauth/rtest"
	"github.com/optimiware/kodama/internal/secrets/keyfile"
	"github.com/optimiware/kodama/internal/secrets/secretstest"
	"github.com/optimiware/kodama/internal/work/worktest"
)

func envOf(v string) func(string) string {
	return func(k string) string {
		if k == EnvVariable {
			return v
		}
		return ""
	}
}

// completeRuntimeSet is a runtime set with every seam present, around the given
// runtime and job observer.
func completeRuntimeSet(rt execution.Runtime, jobs runtimeauth.JobObserver) RuntimeSet {
	return RuntimeSet{
		Runtime: rt, Jobs: jobs,
		Authority: func(*execpg.Store) (rtpg.AttemptAuthority, error) { return nil, errors.New("not used") },
	}
}

// SD-A10: an unset or unknown KODAMA_ENV stops startup, and production refuses
// the in-memory store, the Process runtime, the development KEK and the OIDC
// fixture.
func TestEnvSelection(t *testing.T) {
	t.Run("the environment is required and exact", func(t *testing.T) {
		for _, v := range []string{"development", "test", "production"} {
			got, err := ParseEnv(envOf(v))
			if err != nil || got != Env(v) || !got.Valid() {
				t.Fatalf("%s: %q %v", v, got, err)
			}
		}
		if _, err := ParseEnv(envOf("")); !errors.Is(err, ErrEnvRequired) {
			t.Fatalf("an unset environment must stop startup: %v", err)
		}
		if _, err := ParseEnv(nil); !errors.Is(err, ErrEnvRequired) {
			t.Fatalf("no environment source must stop startup: %v", err)
		}
		for _, v := range []string{"Production", " production", "production ", "prod", "staging", "dev", "TEST", "local"} {
			got, err := ParseEnv(envOf(v))
			if !errors.Is(err, ErrEnvUnknown) || got != "" {
				t.Fatalf("%q must be refused, never normalized: %q %v", v, got, err)
			}
		}
		if Env("").Valid() || Env("prod").Valid() {
			t.Fatal("only the three environments are valid")
		}
	})

	t.Run("startup stops before anything is read or built", func(t *testing.T) {
		// No secrets directory exists: the only error is the environment.
		for name, tc := range map[string]struct {
			env  string
			want error
		}{"unset": {"", ErrEnvRequired}, "unknown": {"qa", ErrEnvUnknown}} {
			getenv := func(k string) string {
				if k == EnvVariable {
					return tc.env
				}
				return map[string]string{"KODAMA_SECRETS_DIR": filepath.Join(t.TempDir(), "absent")}[k]
			}
			if _, err := LoadCoreConfig(getenv, Config{}); !errors.Is(err, tc.want) {
				t.Errorf("%s: LoadCoreConfig: %v", name, err)
			}
			var stdout, stderr bytes.Buffer
			code := Main(context.Background(), []string{"server", "--assembly", "core"}, getenv, &stdout, &stderr)
			if code != 2 || !strings.Contains(stderr.String(), "KODAMA_ENV") {
				t.Errorf("%s: kodama server --assembly core exited %d: %s", name, code, stderr.String())
			}
		}
		c := validConfig()
		c.Env = ""
		if err := c.Validate(); !errors.Is(err, ErrEnvRequired) {
			t.Fatalf("a configuration without an environment is invalid: %v", err)
		}
		c.Env = "prod"
		if err := c.Validate(); !errors.Is(err, ErrEnvUnknown) {
			t.Fatalf("a configuration with an unknown environment is invalid: %v", err)
		}
		if _, err := NewCore(context.Background(), c, nil); !errors.Is(err, ErrEnvUnknown) {
			t.Fatalf("NewCore must refuse before connecting: %v", err)
		}
		if _, err := NewConstructors(""); !errors.Is(err, ErrEnvUnknown) {
			t.Fatalf("there is no fallback constructor table: %v", err)
		}
		if _, err := Admit(Env(""), PortStore, memdb.New()); !errors.Is(err, ErrEnvUnknown) {
			t.Fatalf("an unknown environment admits nothing: %v", err)
		}
		// The development composition has no real services and is assembled
		// elsewhere: NewCore never substitutes memory for them.
		d := validConfig()
		d.Env = Development
		if _, err := NewCore(context.Background(), d, nil); !errors.Is(err, errNotComposed) {
			t.Fatalf("NewCore must not compose the development environment: %v", err)
		}
	})

	t.Run("the adapter table names one adapter per port and environment", func(t *testing.T) {
		for _, env := range []Env{Development, Test, Production} {
			row, err := Selections(env)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range Ports() {
				if row[p].Adapter == "" {
					t.Errorf("%s: no adapter for %s", env, p)
				}
			}
			if len(row) != len(Ports()) {
				t.Errorf("%s: %d entries for %d ports", env, len(row), len(Ports()))
			}
		}
		prod, _ := Selections(Production)
		for p, s := range prod {
			if s.Restricted {
				t.Errorf("production selects the development or test adapter %q for %s", s.Adapter, p)
			}
		}
		dev, _ := Selections(Development)
		if dev[PortRuntime].Adapter != process.Backend || !dev[PortStore].Restricted {
			t.Fatalf("development: %+v", dev)
		}
		if _, err := Selections("prod"); !errors.Is(err, ErrEnvUnknown) {
			t.Fatalf("%v", err)
		}
	})

	t.Run("production refuses the in-memory store", func(t *testing.T) {
		m := executiontest.NewMem(t)
		work := worktest.NewMemEnv(t)
		attempts := rtest.NewMemAttempts(memdb.New())
		for name, adapter := range map[string]any{
			"memdb kernel":            memdb.New(),
			"execution memory store":  m.Mem,
			"work memory store":       work.Store,
			"runtime memory attempts": attempts,
		} {
			if _, err := Admit(Production, PortStore, adapter); !errors.Is(err, ErrDevelopmentAdapter) {
				t.Errorf("production admitted the %s: %v", name, err)
			}
			for _, env := range []Env{Development, Test} {
				if _, err := Admit(env, PortStore, adapter); err != nil {
					t.Errorf("%s refused the %s: %v", env, name, err)
				}
			}
		}
		var refusal *AdapterError
		_, err := Admit(Production, PortStore, memdb.New())
		if !errors.As(err, &refusal) || refusal.Port != PortStore || refusal.Env != Production {
			t.Fatalf("the refusal names the port: %v", err)
		}
		// A real store is not mistaken for one.
		if IsDevelopmentAdapter(nil) || IsDevelopmentAdapter(&execpg.Store{}) {
			t.Fatal("nil and the PostgreSQL store are not development adapters")
		}
	})

	t.Run("production refuses the Process runtime", func(t *testing.T) {
		rt, err := process.New(process.Config{Program: process.DeclaredCommand{}, Dir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := AdmitRuntime(Production, completeRuntimeSet(rt, rt)); !errors.Is(err, ErrDevelopmentAdapter) {
			t.Fatalf("production admitted the Process runtime: %v", err)
		}
		if _, err := AdmitRuntime(Development, completeRuntimeSet(rt, rt)); err != nil {
			t.Fatalf("development refused the Process runtime: %v", err)
		}
		// Presenting the Process runtime behind a decorator does not hide it: its
		// backend identity is what the run pins.
		dec := runtimetest.Decorate(rt)
		if _, err := AdmitRuntime(Production, completeRuntimeSet(dec, rt)); !errors.Is(err, ErrDevelopmentAdapter) {
			t.Fatalf("a decorated Process runtime was admitted: %v", err)
		}
		// The constructor of the production table builds no Process runtime even
		// when asked for one by settings that only the other environments use.
		cons, err := NewConstructors(Production)
		if err != nil {
			t.Fatal(err)
		}
		_, err = cons.Runtime(RuntimeSettings{
			Config:    RuntimeConfig{StateDir: t.TempDir(), EntrypointBinary: "/bin/kodama", ServerURL: "https://kodama.example"},
			Registrar: &LateRegistrar{},
		})
		if err == nil {
			t.Fatal("production built a runtime without a Kubernetes policy")
		}
		if got, _ := Selections(Production); got[PortRuntime].Adapter != "kubernetes" {
			t.Fatalf("production selects %q", got[PortRuntime].Adapter)
		}
	})

	t.Run("production refuses the Podman runtime built by the test constructors", func(t *testing.T) {
		test, err := NewConstructors(Test)
		if err != nil {
			t.Fatal(err)
		}
		set, err := test.Runtime(RuntimeSettings{
			Config:    RuntimeConfig{StateDir: t.TempDir(), EntrypointBinary: "/usr/bin/kodama", ServerURL: "http://127.0.0.1:8080"},
			Registrar: &LateRegistrar{},
		})
		if err != nil {
			t.Fatal(err)
		}
		if set.Runtime.Capabilities().Backend != podman.Backend {
			t.Fatalf("the test constructor built %q", set.Runtime.Capabilities().Backend)
		}
		if _, err := AdmitRuntime(Test, set); err != nil {
			t.Fatalf("test refused its own runtime: %v", err)
		}
		if _, err := AdmitRuntime(Production, set); !errors.Is(err, ErrDevelopmentAdapter) {
			t.Fatalf("production admitted the Podman runtime set: %v", err)
		}
		// Neither the runtime nor its observer alone, and not behind a wrapper.
		for name, s := range map[string]RuntimeSet{
			"runtime only":  {Runtime: set.Runtime, Jobs: &prodJobs{}, Authority: set.Authority},
			"observer only": {Runtime: &prodRuntime{}, Jobs: set.Jobs, Authority: set.Authority},
			"decorated":     {Runtime: runtimetest.Decorate(set.Runtime), Jobs: &prodJobs{}, Authority: set.Authority},
			// A wrapper claiming Kubernetes is still caught by the Podman observer.
			"renamed backend": {Runtime: backendOf{set.Runtime, "kubernetes"}, Jobs: set.Jobs, Authority: set.Authority},
		} {
			if _, err := AdmitRuntime(Production, s); !errors.Is(err, ErrDevelopmentAdapter) {
				t.Errorf("%s: production admitted the set: %v", name, err)
			}
		}
		if !IsDevelopmentAdapter(set.Jobs) || !IsDevelopmentAdapter(set.Runtime) {
			t.Fatal("the Podman runtime is a test adapter")
		}
		if _, err := Admit(Production, PortRuntime, runtimetest.Decorate(set.Runtime)); !errors.Is(err, ErrDevelopmentAdapter) {
			t.Fatalf("a decorated Podman runtime was admitted: %v", err)
		}
	})

	t.Run("production refuses a plain HTTP object store whatever the flag says", func(t *testing.T) {
		prod, _ := NewConstructors(Production)
		for name, cfg := range map[string]S3Config{
			"http, flag omitted": {Endpoint: "http://127.0.0.1:4566", Bucket: "b"},
			"http, flag set":     {Endpoint: "http://localstack:4566", Bucket: "b", AllowInsecureHTTP: true},
			"https, flag set":    {Endpoint: "https://s3.example", Bucket: "b", AllowInsecureHTTP: true},
			"scheme-less":        {Endpoint: "s3.example:443", Bucket: "b"},
		} {
			if err := prod.Objects(cfg); err == nil {
				t.Errorf("%s: production admitted the object store", name)
			}
		}
		if err := prod.Objects(S3Config{Endpoint: "http://127.0.0.1:4566", Bucket: "b"}); !errors.Is(err, ErrDevelopmentAdapter) {
			t.Errorf("an http endpoint is the fixture: %v", err)
		}
		c := validConfig()
		c.Env = Production
		c.Runtime.StateDir, c.Runtime.EntrypointBinary = "", ""
		c.S3 = S3Config{Endpoint: "http://127.0.0.1:4566", Region: "us-east-1", Bucket: "b", AccessKey: "a", SecretKey: "s"}
		c.Secrets = SecretsConfig{Adapter: "kms", KMS: &CloudKMSConfig{Provider: "aws"}}
		err := c.Validate()
		if err == nil || !errors.Is(err, ErrDevelopmentAdapter) || !strings.Contains(err.Error(), "localstack-s3") {
			t.Fatalf("configuration validation must refuse an http endpoint without the flag: %v", err)
		}
		c.S3.Endpoint = "https://s3.example"
		if err := c.Validate(); err != nil && strings.Contains(err.Error(), "localstack-s3") {
			t.Fatalf("an https endpoint was refused as a fixture: %v", err)
		}
		// Test composes the fixture over http.
		test, _ := NewConstructors(Test)
		if err := test.Objects(S3Config{Endpoint: "http://127.0.0.1:4566", Bucket: "b"}); err != nil {
			t.Fatalf("test refused the fixture: %v", err)
		}
	})

	t.Run("bootstrap needs the environment and the environment's verifier", func(t *testing.T) {
		tokenFile := filepath.Join(t.TempDir(), "admin.token")
		if err := os.WriteFile(tokenFile, []byte("token\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		args := []string{"bootstrap", "--tenant-id", "t_acme", "--tenant-key", "acme", "--display-name", "Acme", "--operator-id", "op", "--eligible-subject", "sub", "--token-file", tokenFile}
		run := func(env map[string]string) (int, string) {
			getenv := func(k string) string {
				return map[string]string{"KODAMA_DATABASE_MIGRATION_URL": "postgres://owner:p@192.0.2.1:1/db", "KODAMA_OIDC_ISSUER": "http://127.0.0.1:1", "KODAMA_OIDC_AUDIENCE": "a"}[k] + env[k]
			}
			var stdout, stderr bytes.Buffer
			code := Main(context.Background(), args, getenv, &stdout, &stderr)
			return code, stderr.String()
		}
		if code, out := run(nil); code != 2 || !strings.Contains(out, "KODAMA_ENV is required") {
			t.Errorf("an unset environment: %d %s", code, out)
		}
		if code, out := run(map[string]string{EnvVariable: "prod"}); code != 2 || !strings.Contains(out, "not a known environment") {
			t.Errorf("an unknown environment: %d %s", code, out)
		}
		// Production over the HTTP issuer fixture is refused before any connection.
		start := time.Now()
		code, out := run(map[string]string{EnvVariable: "production", "KODAMA_OIDC_ALLOW_INSECURE_HTTP": "true"})
		if code != 1 || !strings.Contains(out, "production refuses") {
			t.Errorf("the OIDC fixture in production: %d %s", code, out)
		}
		if time.Since(start) > 3*time.Second {
			t.Error("the refusal waited on the network")
		}

		ctx := context.Background()
		fixture := OIDCConfig{Issuer: "http://127.0.0.1:1", Audience: "a", AllowInsecureHTTP: true}
		base := BootstrapRequest{AdminDatabaseURL: "postgres://owner:p@192.0.2.1:1/db", OIDC: fixture, AccessToken: "t"}
		for name, tc := range map[string]struct {
			env  Env
			want error
		}{"unset": {"", ErrEnvRequired}, "unknown": {"qa", ErrEnvUnknown}, "production": {Production, ErrDevelopmentAdapter}, "development": {Development, errNotComposed}} {
			r := base
			r.Env = tc.env
			if _, err := Bootstrap(ctx, r); !errors.Is(err, tc.want) {
				t.Errorf("%s: %v", name, err)
			}
		}
	})

	t.Run("a runtime set without its seams is refused", func(t *testing.T) {
		m := runtimetest.NewManual()
		for name, set := range map[string]RuntimeSet{
			"no runtime":   {Jobs: &rtest.Jobs{}, Authority: completeRuntimeSet(m, nil).Authority},
			"no observer":  {Runtime: m, Authority: completeRuntimeSet(m, nil).Authority},
			"no authority": {Runtime: m, Jobs: &rtest.Jobs{}},
		} {
			if _, err := AdmitRuntime(Test, set); !errors.Is(err, execution.ErrNotConfigured) {
				t.Errorf("%s: %v", name, err)
			}
		}
		m.SetCapabilities(execution.Capabilities{})
		if _, err := AdmitRuntime(Test, completeRuntimeSet(m, &rtest.Jobs{})); !errors.Is(err, execution.ErrNotConfigured) {
			t.Errorf("a runtime without a backend identity was admitted: %v", err)
		}
	})

	t.Run("production refuses the development KEK", func(t *testing.T) {
		dir := t.TempDir()
		keyPath := filepath.Join(dir, "master-key")
		if err := os.WriteFile(keyPath, bytes.Repeat([]byte{7}, 32), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := SecretsConfig{Adapter: "keyfile", Keyfile: &KeyfileConfig{Current: "v1", Files: map[string]string{"v1": keyPath}}}

		prod, _ := NewConstructors(Production)
		if _, _, err := prod.KeyWrapper(context.Background(), cfg); !errors.Is(err, ErrDevelopmentAdapter) {
			t.Fatalf("production built the key file wrapper: %v", err)
		}
		// Nor does it fall back to the key file when a cloud provider is named.
		if w, _, err := prod.KeyWrapper(context.Background(), SecretsConfig{Adapter: "kms", KMS: &CloudKMSConfig{Provider: "aws"}}); err == nil || w != nil {
			t.Fatalf("production built a wrapper for an unwired cloud provider: %v", err)
		}
		test, _ := NewConstructors(Test)
		w, _, err := test.KeyWrapper(context.Background(), cfg)
		if err != nil || w == nil {
			t.Fatalf("test composes the local key file: %v", err)
		}
		// The built provider is itself recognized, wherever it is handed over.
		p, err := keyfile.New(keyfile.Config{Development: true, Current: "v1", Files: cfg.Keyfile.Files})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Admit(Production, PortKeyWrapper, p); !errors.Is(err, ErrDevelopmentAdapter) {
			t.Fatalf("production admitted the key file provider: %v", err)
		}
		eph, err := secretstest.NewEphemeralWrapper()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Admit(Production, PortKeyWrapper, eph); !errors.Is(err, ErrDevelopmentAdapter) {
			t.Fatalf("production admitted the ephemeral wrapper: %v", err)
		}
		if _, err := Admit(Development, PortKeyWrapper, eph); err != nil {
			t.Fatalf("development refused the ephemeral wrapper: %v", err)
		}
		dev, _ := NewConstructors(Development)
		if _, _, err := dev.KeyWrapper(context.Background(), cfg); !errors.Is(err, errNotComposed) {
			t.Fatalf("development builds its wrapper elsewhere: %v", err)
		}
	})

	t.Run("production refuses the OIDC and object store fixtures", func(t *testing.T) {
		prod, _ := NewConstructors(Production)
		if _, err := prod.Verifier(OIDCConfig{Issuer: "http://127.0.0.1:9000", Audience: "a", AllowInsecureHTTP: true}); !errors.Is(err, ErrDevelopmentAdapter) {
			t.Fatalf("production built the OIDC fixture verifier: %v", err)
		}
		if err := prod.Objects(S3Config{Endpoint: "http://127.0.0.1:4566", Bucket: "b", AllowInsecureHTTP: true}); !errors.Is(err, ErrDevelopmentAdapter) {
			t.Fatalf("production admitted the S3 fixture: %v", err)
		}
		if err := prod.Objects(S3Config{Endpoint: "https://s3.example", Bucket: "b"}); err != nil {
			t.Fatalf("production refused a real object store: %v", err)
		}
		test, _ := NewConstructors(Test)
		if err := test.Objects(S3Config{Endpoint: "http://127.0.0.1:4566", Bucket: "b", AllowInsecureHTTP: true}); err != nil {
			t.Fatalf("test composes the S3 fixture: %v", err)
		}
		if _, err := test.Verifier(OIDCConfig{Issuer: "http://127.0.0.1:9000", Audience: "a", AllowInsecureHTTP: true}); err != nil {
			t.Fatalf("test composes the OIDC fixture: %v", err)
		}
	})

	t.Run("production configuration refuses every development setting", func(t *testing.T) {
		c := validConfig()
		c.Env = Production
		c.Runtime.ServerURL = "https://kodama.example"
		c.Runtime.StateDir, c.Runtime.EntrypointBinary = "", ""
		c.Runtime.Kubernetes = KubernetesConfig{APIURL: "https://k8s.example", TokenFile: "/t", CAFile: "/c", PolicyFile: "/p"}
		c.Secrets = SecretsConfig{Adapter: "kms", KMS: &CloudKMSConfig{Provider: "aws"}}
		c.OIDC.Issuer = "https://issuer.example"
		c.S3.Endpoint = "https://s3.example"
		// Provider aws is refused for another reason; compare the guard alone.
		if errs := c.guardEnv(); len(errs) != 0 {
			t.Fatalf("a clean production configuration was refused: %v", errs)
		}
		c.Secrets = validConfig().Secrets
		c.OIDC.AllowInsecureHTTP, c.S3.AllowInsecureHTTP = true, true
		c.S3.Endpoint = "http://127.0.0.1:4566"
		c.Runtime.StateDir = "/var/lib/kodama"
		err := c.Validate()
		if err == nil {
			t.Fatal("production accepted development settings")
		}
		for _, want := range []string{"keyfile", "oidc-fixture", "localstack-s3", "process/podman"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not name %s: %v", want, err)
			}
		}
		if !errors.Is(err, ErrDevelopmentAdapter) {
			t.Fatalf("the refusals must be development-adapter refusals: %v", err)
		}
		// The same settings are the test environment's own.
		c.Env = Test
		c.Runtime.EntrypointBinary = "/usr/bin/kodama"
		c.Runtime.Kubernetes = KubernetesConfig{}
		if err := c.Validate(); err != nil {
			t.Fatalf("test refused its own settings: %v", err)
		}
		if _, err := NewCore(context.Background(), func() CoreConfig { p := c; p.Env = Production; return p }(), nil); !errors.Is(err, ErrDevelopmentAdapter) {
			t.Fatalf("NewCore must refuse production with development adapters before connecting: %v", err)
		}
	})
}

// backendOf reports another backend identity than its runtime's own.
type backendOf struct {
	execution.Runtime
	backend string
}

func (b backendOf) Capabilities() execution.Capabilities {
	return execution.Capabilities{Backend: b.backend, Guarantees: execution.AllGuarantees}
}

// prodRuntime and prodJobs are production-admissible stand-ins, so the guard
// under test is the other half's.
type prodRuntime struct{ runtimetest.Manual }

func (*prodRuntime) Capabilities() execution.Capabilities {
	return execution.Capabilities{Backend: "kubernetes", Guarantees: execution.AllGuarantees}
}

type prodJobs struct{}

func (*prodJobs) ObserveJob(context.Context, runtimeauth.JobObservation) (runtimeauth.JobEvidence, error) {
	return runtimeauth.JobEvidence{}, runtimeauth.ErrNotObserved
}
