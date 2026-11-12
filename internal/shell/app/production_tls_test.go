package app

// Production PostgreSQL transport.
//
// Production requires TLS on every PostgreSQL connection. The runtime DSN is
// Installation.DatabaseURL; the controller, publisher and auth DSNs are the
// three ProductionConfig URLs. Each of the four must carry exactly one
// sslmode=verify-full option in URL form, and every refusal is
// fault.InvalidInput and never echoes the DSN or its password.

import (
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/installation"
	"github.com/optimiware/kodama/internal/shell/secret"
)

// r14PostgresBase is a complete URL-form DSN with a password. A refusal must
// never echo either the DSN or the password.
const r14PostgresBase = "postgres://kodama:pg-hunter2@db.example:5432/kodama"

// r14NATSTLS is the only accepted production NATS transport.
const r14NATSTLS = "tls://nats.example:4222"

// productionDSNField addresses one of the four production PostgreSQL DSNs.
type productionDSNField struct {
	name string
	url  func(*Config) *string
}

// productionDSNFields returns the four production PostgreSQL DSNs.
func productionDSNFields() []productionDSNField {
	return []productionDSNField{
		{name: "runtime", url: func(config *Config) *string { return &config.Installation.DatabaseURL }},
		{name: "controller", url: func(config *Config) *string { return &config.Production.ControllerURL }},
		{name: "publisher", url: func(config *Config) *string { return &config.Production.PublisherURL }},
		{name: "auth", url: func(config *Config) *string { return &config.Production.AuthURL }},
	}
}

// TestProductionRequiresVerifyFullOnEveryPostgresDSN pins the TLS rule on each
// of the four production DSNs individually: a missing, weaker, repeated,
// conflicting or differently spelled sslmode is refused, and a keyword-form DSN
// is refused.
func TestProductionRequiresVerifyFullOnEveryPostgresDSN(t *testing.T) {
	refusedSuffixes := []struct {
		name   string
		suffix string
	}{
		{name: "missing sslmode", suffix: ""},
		{name: "query without sslmode", suffix: "?connect_timeout=10"},
		{name: "disable", suffix: "?sslmode=disable"},
		{name: "allow", suffix: "?sslmode=allow"},
		{name: "prefer", suffix: "?sslmode=prefer"},
		{name: "require", suffix: "?sslmode=require"},
		{name: "verify-ca", suffix: "?sslmode=verify-ca"},
		{name: "empty sslmode", suffix: "?sslmode="},
		{name: "duplicated sslmode", suffix: "?sslmode=verify-full&sslmode=verify-full"},
		{name: "conflicting sslmode", suffix: "?sslmode=verify-full&sslmode=require"},
		{name: "conflicting sslmode reversed", suffix: "?sslmode=require&sslmode=verify-full"},
		{name: "uppercase sslmode key", suffix: "?SSLMODE=verify-full"},
		{name: "uppercase sslmode value", suffix: "?sslmode=VERIFY-FULL"},
		{name: "sslmode suffix", suffix: "?sslmode=verify-full-x"},
		{name: "sslmode prefix", suffix: "?sslmode=xverify-full"},
		{name: "sslmode percent nul", suffix: "?sslmode=verify-full%00"},
		{name: "fragment with weaker sslmode", suffix: "?sslmode=verify-full#&sslmode=disable"},
		{name: "fragment marker", suffix: "?sslmode=verify-full#"},
		{name: "ssl true alias", suffix: "?sslmode=verify-full&ssl=true"},
		{name: "host override unix socket", suffix: "?sslmode=verify-full&host=/var/run/postgresql"},
		{name: "host override empty", suffix: "?sslmode=verify-full&host="},
		{name: "unsupported sslcert", suffix: "?sslmode=verify-full&sslcert=/tmp/client.crt"},
		{name: "percent encoded key", suffix: "?%73slmode=verify-full"},
		{name: "connect timeout overflow", suffix: "?sslmode=verify-full&connect_timeout=999999999999999999999"},
		{name: "malformed query percent", suffix: "?sslmode=verify-full&application_name=%zz"},
	}

	for _, field := range productionDSNFields() {
		for _, testCase := range refusedSuffixes {
			t.Run(field.name+" refused "+testCase.name, func(t *testing.T) {
				config := validProductionConfig(t)
				*field.url(&config) = r14PostgresBase + testCase.suffix
				err := config.resolved().validate()
				requireInvalidInput(t, err, field.name+" DSN "+testCase.name)
				if strings.Contains(err.Error(), r14PostgresBase+testCase.suffix) {
					t.Fatalf("refusal echoes the configured %s DSN: %v", field.name, err)
				}
				if strings.Contains(err.Error(), "pg-hunter2") {
					t.Fatalf("refusal echoes the %s DSN password: %v", field.name, err)
				}
			})
		}

		// Whole-DSN vectors are refused on every pool individually: a scheme,
		// host or authority rule that only the runtime DSN reached would let
		// the controller, publisher or auth pool connect without verified TLS.
		refusedDSNs := []struct {
			name string
			dsn  string
		}{
			{name: "other scheme", dsn: "foo://kodama:pg-hunter2@db.example:5432/kodama?sslmode=verify-full"},
			{name: "postgresql scheme", dsn: "postgresql://kodama:pg-hunter2@db.example:5432/kodama?sslmode=verify-full"},
			{name: "uppercase scheme", dsn: "POSTGRES://kodama:pg-hunter2@db.example:5432/kodama?sslmode=verify-full"},
			{name: "empty host", dsn: "postgres:///kodama?sslmode=verify-full"},
			{name: "empty host with socket override", dsn: "postgres:///kodama?host=/var/run/postgresql&sslmode=verify-full"},
			{name: "percent encoded unix host", dsn: "postgres://%2Fvar%2Frun%2Fpostgresql/kodama?sslmode=verify-full"},
			{name: "comma host list", dsn: "postgres://db.example,/var/run/postgresql/kodama?sslmode=verify-full"},
			{name: "empty host element", dsn: "postgres://,db.example/kodama?sslmode=verify-full"},
			{name: "port zero", dsn: "postgres://db.example:0/kodama?sslmode=verify-full"},
			{name: "port too large", dsn: "postgres://db.example:65536/kodama?sslmode=verify-full"},
			{name: "missing database path", dsn: "postgres://db.example?sslmode=verify-full"},
			{name: "keyword form", dsn: "host=db.example user=kodama password=pg-hunter2 dbname=kodama sslmode=verify-full"},
		}
		for _, testCase := range refusedDSNs {
			t.Run(field.name+" refused "+testCase.name, func(t *testing.T) {
				config := validProductionConfig(t)
				*field.url(&config) = testCase.dsn
				err := config.resolved().validate()
				requireInvalidInput(t, err, field.name+" DSN "+testCase.name)
				if strings.Contains(err.Error(), testCase.dsn) {
					t.Fatalf("refusal echoes the configured %s DSN: %v", field.name, err)
				}
				if strings.Contains(err.Error(), "pg-hunter2") {
					t.Fatalf("refusal echoes the %s DSN password: %v", field.name, err)
				}
			})
		}

		accepted := []struct {
			name string
			dsn  string
		}{
			{name: "verify-full", dsn: r14PostgresBase + "?sslmode=verify-full"},
			{name: "verify-full with other options", dsn: r14PostgresBase + "?connect_timeout=10&sslmode=verify-full"},
			{name: "percent-encoded sslmode", dsn: r14PostgresBase + "?sslmode=verify%2Dfull"},
			{name: "ipv6 host", dsn: "postgres://kodama:pg-hunter2@[2001:db8::1]:5432/kodama?sslmode=verify-full"},
			{name: "port omitted", dsn: "postgres://kodama:pg-hunter2@db.example/kodama?sslmode=verify-full"},
		}
		for _, testCase := range accepted {
			t.Run(field.name+" accepted "+testCase.name, func(t *testing.T) {
				config := validProductionConfig(t)
				*field.url(&config) = testCase.dsn
				if err := config.resolved().validate(); err != nil {
					t.Fatalf("validate refused the %s DSN %q: %v", field.name, testCase.dsn, err)
				}
			})
		}
	}
}

// TestProductionAcceptedCredentialGrammar pins the DSN shapes the real boot
// flow emits on every one of the four production pools: the generated password
// is 24 random bytes rendered as 48 hex characters (scripts/bootstrap-secrets
// `openssl rand -hex 24`) and the chart/script suffix is
// `sslmode=verify-full&sslrootcert=/etc/kodama/pg-ca/ca.crt`. The credential
// grammar is deliberately percent-free: letters, digits, `.`, `_`, `~` and `-`
// are accepted and a percent-encoded credential is refused.
func TestProductionAcceptedCredentialGrammar(t *testing.T) {
	const hex48 = "0123456789abcdef0123456789abcdef0123456789abcdef"
	const suffix = "?sslmode=verify-full&sslrootcert=/etc/kodama/pg-ca/ca.crt"
	roleUser := map[string]string{
		"runtime":    "kodama_runtime",
		"controller": "kodama_controller",
		"publisher":  "kodama_publisher",
		"auth":       "kodama_auth",
	}
	for _, field := range productionDSNFields() {
		user := roleUser[field.name]
		if user == "" {
			t.Fatalf("no role user for %s", field.name)
		}
		for _, password := range []struct {
			name  string
			value string
		}{
			{name: "generated hex password", value: hex48},
			{name: "url-safe alphabet", value: "a-b.c_d~e"},
		} {
			t.Run(field.name+" accepted "+password.name, func(t *testing.T) {
				config := validProductionConfig(t)
				*field.url(&config) = "postgres://" + user + ":" + password.value + "@database:5432/kodama" + suffix
				if err := config.resolved().validate(); err != nil {
					t.Fatalf("validate refused the %s DSN with %s: %v", field.name, password.name, err)
				}
			})
		}
		for _, password := range []string{"p%40ss", "p%23x", "p%25", "p%40ss%2Fw%3Ard"} {
			t.Run(field.name+" refused percent credential "+password, func(t *testing.T) {
				config := validProductionConfig(t)
				*field.url(&config) = "postgres://" + user + ":" + password + "@database:5432/kodama" + suffix
				err := config.resolved().validate()
				requireInvalidInput(t, err, field.name+" percent-encoded credential")
				if strings.Contains(err.Error(), password) {
					t.Fatalf("refusal echoes the %s credential: %v", field.name, err)
				}
			})
		}
	}
}

// TestProductionPostgresDSNIsNotTrimmedAtCallSite pins that the production
// composition validates the exact configured bytes. A leading/trailing space,
// tab or newline must be refused; a TrimSpace-before-validate call site would
// silently accept a padded DSN.
func TestProductionPostgresDSNIsNotTrimmedAtCallSite(t *testing.T) {
	const dsn = r14PostgresBase + "?sslmode=verify-full"
	padded := []struct {
		name  string
		value string
	}{
		{name: "leading space", value: " " + dsn},
		{name: "trailing space", value: dsn + " "},
		{name: "trailing tab", value: dsn + "\t"},
		{name: "trailing newline", value: dsn + "\n"},
		{name: "leading tab", value: "\t" + dsn},
	}
	for _, field := range productionDSNFields() {
		for _, testCase := range padded {
			t.Run(field.name+" refused "+testCase.name, func(t *testing.T) {
				config := validProductionConfig(t)
				*field.url(&config) = testCase.value
				err := config.resolved().validate()
				requireInvalidInput(t, err, field.name+" padded DSN "+testCase.name)
				if strings.Contains(err.Error(), dsn) {
					t.Fatalf("refusal echoes the configured %s DSN: %v", field.name, err)
				}
				if strings.Contains(err.Error(), "pg-hunter2") {
					t.Fatalf("refusal echoes the %s DSN password: %v", field.name, err)
				}
			})
		}
	}
}

// TestProductionKeyReferenceFamilyIsExact pins the same key-family rule at the
// production composition: the cloud reference and the installation reference
// each need an exact case-sensitive family prefix and a whitespace-free key
// name, so a Contains, lower-case or Trim-style match cannot accept a foreign
// family or a padded value.
func TestProductionKeyReferenceFamilyIsExact(t *testing.T) {
	families := []struct {
		name    string
		adapter installation.KeyAdapter
		prefix  string
		ref     func(string) string
		foreign string
	}{
		{name: "aws", adapter: installation.KeyAWSKMS, prefix: installation.KeyRefAWSKMSPrefix, ref: func(name string) string {
			return installation.KeyRefAWSKMSPrefix + "alias/" + name
		}, foreign: installation.KeyRefGCPKMSPrefix},
		{name: "gcp", adapter: installation.KeyGCPKMS, prefix: installation.KeyRefGCPKMSPrefix, ref: func(name string) string {
			return installation.KeyRefGCPKMSPrefix + "projects/kodama/locations/global/keyRings/kodama/cryptoKeys/" + name
		}, foreign: installation.KeyRefAWSKMSPrefix},
		{name: "scaleway", adapter: installation.KeyScalewayKMS, prefix: installation.KeyRefScalewayKMSPrefix, ref: func(name string) string {
			return installation.KeyRefScalewayKMSPrefix + name
		}, foreign: installation.KeyRefAWSKMSPrefix},
	}
	for _, family := range families {
		accepted := family.ref("key-1")
		refused := []string{
			strings.ToUpper(family.prefix) + "key-1",
			" " + accepted,
			accepted + " ",
			family.prefix + " key-1",
			strings.TrimSuffix(family.prefix, ":") + "-x:y",
			family.foreign + family.prefix + "key-1",
		}
		for _, value := range refused {
			t.Run(family.name+" installation reference "+value, func(t *testing.T) {
				config := validProductionConfig(t)
				config.Installation.Keys = family.adapter
				config.Installation.KeyRef = accepted
				config.Production.Cloud.KeyRef = secret.KeyRef(accepted)
				config.Installation.KeyRef = value
				requireInvalidInput(t, config.resolved().validate(), family.name+" installation key reference")
			})
			t.Run(family.name+" cloud reference "+value, func(t *testing.T) {
				config := validProductionConfig(t)
				config.Installation.Keys = family.adapter
				config.Installation.KeyRef = accepted
				config.Production.Cloud.KeyRef = secret.KeyRef(value)
				requireInvalidInput(t, config.resolved().validate(), family.name+" cloud key reference")
			})
		}
		// The key name after the exact prefix must be non-empty and free of
		// whitespace, control bytes and DEL on BOTH the installation reference
		// (Preflight) and the composition's cloud reference
		// (exactKeyReference). Each mutant that drops the empty-name, IsSpace or
		// IsControl term must be killed here.
		invalidNames := []struct {
			name  string
			value string
		}{
			{name: "empty name", value: ""},
			{name: "interior space", value: "key 1"},
			{name: "interior tab", value: "key\t1"},
			{name: "control byte", value: "key\x011"},
			{name: "nul byte", value: "key\x001"},
			{name: "del byte", value: "key\x7f1"},
		}
		for _, invalid := range invalidNames {
			installationReference := family.ref(invalid.value)
			// The composition's own family check is the exact-prefix
			// rule; the full provider grammar is owned by Preflight and the
			// provider constructors. Build the composition reference the same
			// way that check expects so this test keeps pinning it.
			cloudReference := family.prefix + invalid.value
			t.Run(family.name+" installation reference "+invalid.name, func(t *testing.T) {
				config := validProductionConfig(t)
				config.Installation.Keys = family.adapter
				config.Installation.KeyRef = accepted
				config.Production.Cloud.KeyRef = secret.KeyRef(accepted)
				config.Installation.KeyRef = installationReference
				err := config.resolved().validate()
				requireInvalidInput(t, err, family.name+" installation key name "+invalid.name)
				if invalid.value != "" && strings.Contains(err.Error(), invalid.value) {
					t.Fatalf("refusal echoes the installation key name: %v", err)
				}
			})
			t.Run(family.name+" cloud reference "+invalid.name, func(t *testing.T) {
				config := validProductionConfig(t)
				config.Installation.Keys = family.adapter
				config.Installation.KeyRef = accepted
				config.Production.Cloud.KeyRef = secret.KeyRef(cloudReference)
				err := config.resolved().validate()
				requireInvalidInput(t, err, family.name+" cloud key name "+invalid.name)
				if invalid.value != "" && strings.Contains(err.Error(), invalid.value) {
					t.Fatalf("refusal echoes the cloud key name: %v", err)
				}
			})
		}
		t.Run(family.name+" accepted", func(t *testing.T) {
			config := validProductionConfig(t)
			config.Installation.Keys = family.adapter
			config.Installation.KeyRef = accepted
			config.Production.Cloud.KeyRef = secret.KeyRef(accepted)
			if err := config.resolved().validate(); err != nil {
				t.Fatalf("validate refused the %s family reference: %v", family.name, err)
			}
		})
	}
}

// TestProductionRequiresTLSForNATS pins that the app startup path reaches the
// installation Preflight rule: production refuses a plaintext or non-TLS NATS
// URL, userinfo and fragments, and accepts tls://.
func TestProductionRequiresTLSForNATS(t *testing.T) {
	refused := []struct {
		name string
		url  string
	}{
		{name: "plain nats", url: "nats://nats.example:4222"},
		{name: "plain tcp", url: "tcp://nats.example:4222"},
		{name: "websocket ws", url: "ws://nats.example:4222"},
		{name: "userinfo", url: "tls://user:nats-hunter2@nats.example:4222"},
		{name: "empty userinfo", url: "tls://@nats.example:4222"},
		{name: "fragment", url: "tls://nats.example:4222#fragment"},
		{name: "empty fragment marker", url: "tls://nats.example:4222#"},
		{name: "comma list", url: "tls://a:4222,b:4222"},
		{name: "comma list with scheme", url: "tls://h:4222,tls://u:p@i"},
		{name: "port zero", url: "tls://nats.example:0"},
		{name: "port too large", url: "tls://nats.example:65536"},
		{name: "empty host", url: "tls://:4222"},
	}
	for _, testCase := range refused {
		t.Run("refused "+testCase.name, func(t *testing.T) {
			config := validProductionConfig(t)
			config.Installation.NATSURL = testCase.url
			err := config.resolved().validate()
			requireInvalidInput(t, err, "NATS URL "+testCase.name)
			if strings.Contains(err.Error(), testCase.url) {
				t.Fatalf("refusal echoes the configured NATS URL: %v", err)
			}
			if strings.Contains(err.Error(), "nats-hunter2") {
				t.Fatalf("refusal echoes the NATS credential: %v", err)
			}
		})
	}

	t.Run("accepted tls", func(t *testing.T) {
		config := validProductionConfig(t)
		config.Installation.NATSURL = r14NATSTLS
		if err := config.resolved().validate(); err != nil {
			t.Fatalf("validate refused production NATS URL %q: %v", r14NATSTLS, err)
		}
	})
}

// requireInvalidInput fails unless err is a fault.InvalidInput.
func requireInvalidInput(t *testing.T, err error, context string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s was accepted; want fault.InvalidInput", context)
	}
	class, ok := fault.ClassOf(err)
	if !ok {
		t.Fatalf("%s refusal is not a fault: %v", context, err)
	}
	if class != fault.InvalidInput {
		t.Fatalf("%s fault class = %s; want InvalidInput (%v)", context, class, err)
	}
}
