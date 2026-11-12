package installation

// URL and composition rules.
//
// Three behaviours are pinned in this package:
//
//  1. the test environment accepts the kubernetes runtime in addition to the
//     podman runtime (no fourth environment);
//  2. production requires TLS: the NATS URL must be a single tls:// URL without
//     userinfo, a comma list or a fragment, and every PostgreSQL DSN must be a
//     strict URL-form postgres:// DSN carrying exactly one
//     sslmode=verify-full option. The production validators are strict
//     allowlists, not net/url parsing, because pgx's pgconn parser follows
//     libpq and accepts fragments, the ssl=true alias, host overrides, unix
//     sockets and comma host lists that net/url would not attribute to TLS;
//  3. Config.Preflight itself refuses a key reference outside the declared key
//     adapter family.
//
// Every vector is parsed by installation.ParseConfig or checked by
// Config.Preflight directly. Refusals are fault.InvalidInput and never echo the
// configured DSN, URL or a password.

import (
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
)

// productionURLDocument renders a complete production document over the given
// database and NATS URLs. Every other setting is valid, so only the URL under
// test can cause a refusal.
func productionURLDocument(databaseURL, natsURL string) docFields {
	fields := productionDoc("aws-kms", "aws-kms:alias/kodama")
	fields.databaseURL = databaseURL
	fields.natsURL = natsURL
	return fields
}

// environmentRuntimeDocument renders one complete document for the environment and
// runtime runtime under test. The test environment deliberately keeps a
// plaintext nats:// URL: only production requires TLS.
func environmentRuntimeDocument(environment, runtime string) docFields {
	var fields docFields
	switch environment {
	case "development":
		fields = developmentDoc()
	case "test":
		fields = testDoc()
	case "production":
		fields = productionDoc("aws-kms", "aws-kms:alias/kodama")
	default:
		fields = docFields{environment: environment, runtime: runtime}
	}
	fields.runtime = runtime
	return fields
}

// TestPreflightEnvironmentRuntimeMatrix pins the environment x runtime matrix.
// test+kubernetes is accepted; production+kubernetes stays required and
// every weaker pairing stays refused. No fourth environment exists.
func TestPreflightEnvironmentRuntimeMatrix(t *testing.T) {
	if got := len([]Environment{EnvironmentDevelopment, EnvironmentTest, EnvironmentProduction}); got != 3 {
		t.Fatalf("declared environments = %d; want exactly development, test and production (no fourth environment)", got)
	}
	for _, environment := range []Environment{EnvironmentDevelopment, EnvironmentTest, EnvironmentProduction} {
		if !environment.Valid() {
			t.Errorf("declared environment %q is not valid", environment)
		}
	}
	if Environment("staging").Valid() {
		t.Error("an undeclared fourth environment reported valid")
	}

	cases := []struct {
		name        string
		environment string
		runtime     string
		wantErr     string
	}{
		{name: "development process", environment: "development", runtime: "process"},
		{name: "development podman refused", environment: "development", runtime: "podman", wantErr: "process runtime"},
		{name: "development kubernetes refused", environment: "development", runtime: "kubernetes", wantErr: "process runtime"},
		{name: "test podman", environment: "test", runtime: "podman"},
		{name: "test kubernetes accepted", environment: "test", runtime: "kubernetes"},
		{name: "test process refused", environment: "test", runtime: "process", wantErr: "podman runtime"},
		{name: "production kubernetes", environment: "production", runtime: "kubernetes"},
		{name: "production process refused", environment: "production", runtime: "process", wantErr: "kubernetes runtime"},
		{name: "production podman refused", environment: "production", runtime: "podman", wantErr: "kubernetes runtime"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fields := environmentRuntimeDocument(testCase.environment, testCase.runtime)
			config, err := ParseConfig(strings.NewReader(fields.String()))
			if testCase.wantErr == "" {
				if err != nil {
					t.Fatalf("ParseConfig refused %s %s composition: %v", testCase.environment, testCase.runtime, err)
				}
				if string(config.Runtime) != testCase.runtime {
					t.Errorf("runtime = %q; want %q", config.Runtime, testCase.runtime)
				}
				return
			}
			assertFaultClass(t, err, fault.InvalidInput)
			if !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("refusal = %q; want substring %q", err.Error(), testCase.wantErr)
			}
		})
	}
}

// TestPreflightProductionNATSTLSVectors pins the production NATS rule: exactly
// tls:// without userinfo and without a fragment. Plain nats://, plain tcp, ws,
// wss and every other scheme are refused.
func TestPreflightProductionNATSTLSVectors(t *testing.T) {
	const databaseURL = "postgres://kodama:pg-hunter2@database.example:5432/kodama?sslmode=verify-full"

	accepted := []struct {
		name string
		url  string
	}{
		{name: "tls", url: "tls://nats.example:4222"},
		{name: "tls ipv6", url: "tls://[2001:db8::1]:4222"},
		{name: "tls uppercase scheme", url: "TLS://nats.example:4222"},
		{name: "tls without port", url: "tls://nats.example"},
		{name: "tls ipv6 without port", url: "tls://[2001:db8::1]"},
		{name: "tls max port", url: "tls://nats.example:65535"},
	}
	for _, testCase := range accepted {
		t.Run("accepted "+testCase.name, func(t *testing.T) {
			fields := productionURLDocument(databaseURL, testCase.url)
			if _, err := ParseConfig(strings.NewReader(fields.String())); err != nil {
				t.Fatalf("ParseConfig refused production NATS URL %q: %v", testCase.url, err)
			}
		})
	}

	refused := []struct {
		name   string
		url    string
		secret string
	}{
		{name: "plain nats", url: "nats://nats.example:4222"},
		{name: "plain tcp", url: "tcp://nats.example:4222"},
		{name: "websocket ws", url: "ws://nats.example:4222"},
		{name: "websocket wss", url: "wss://nats.example:4222"},
		{name: "http", url: "http://nats.example:4222"},
		{name: "userinfo", url: "tls://user:nats-hunter2@nats.example:4222", secret: "nats-hunter2"},
		{name: "userinfo without password", url: "tls://user@nats.example:4222"},
		{name: "empty userinfo", url: "tls://@nats.example:4222"},
		{name: "fragment", url: "tls://nats.example:4222#fragment"},
		{name: "empty fragment marker", url: "tls://nats.example:4222#"},
		{name: "path and fragment", url: "tls://nats.example:4222/stream#fragment"},
		{name: "no host", url: "tls://:4222"},
		{name: "comma list", url: "tls://a:4222,b:4222"},
		{name: "comma list with scheme", url: "tls://a:4222,tls://b:4222"},
		{name: "comma list with userinfo", url: "tls://a,nats://user:pw@b:4222", secret: "pw"},
		{name: "comma list with per-element userinfo", url: "tls://h:4222,tls://u:p@i", secret: "p@"},
		// The comma guard is the whole post-scheme string, not only the host
		// part: nats.go splits a comma list anywhere, so a comma after the
		// path or query separator would smuggle a plaintext second broker past
		// a host-only check.
		{name: "comma after path", url: "tls://nats.example:4222/,nats://evil:4222"},
		{name: "comma after query marker", url: "tls://nats.example:4222?x,nats://evil:4222"},
		{name: "comma after path with scheme", url: "tls://nats.example:4222/p,tls://other"},
		{name: "comma in query value", url: "tls://nats.example:4222/?brokers=a,b"},
		// The '#' guard is the whole URL, so a fragment after the host, path
		// or query separator is refused too.
		{name: "fragment after path", url: "tls://nats.example:4222/x#frag"},
		{name: "empty fragment after query", url: "tls://nats.example:4222/?a=b#"},
		// '@' is refused anywhere, including after the path or in the query,
		// because nats.go applies per-element userinfo.
		{name: "at after path", url: "tls://nats.example:4222/x@evil"},
		{name: "at in query", url: "tls://nats.example:4222/?user=a@b"},
		{name: "port zero", url: "tls://nats.example:0"},
		{name: "port too large", url: "tls://nats.example:65536"},
		{name: "port far too large", url: "tls://nats.example:99999"},
		{name: "port not numeric", url: "tls://nats.example:abc"},
		{name: "empty port", url: "tls://nats.example:"},
		{name: "empty host element", url: "tls://,nats.example:4222"},
		{name: "unix socket", url: "tls:///var/run/nats.sock"},
		{name: "embedded space", url: "tls://nats example:4222"},
	}
	for _, testCase := range refused {
		t.Run("refused "+testCase.name, func(t *testing.T) {
			fields := productionURLDocument(databaseURL, testCase.url)
			_, err := ParseConfig(strings.NewReader(fields.String()))
			assertFaultClass(t, err, fault.InvalidInput)
			if strings.Contains(err.Error(), testCase.url) {
				t.Fatalf("refusal echoes the configured NATS URL: %v", err)
			}
			if testCase.secret != "" && strings.Contains(err.Error(), testCase.secret) {
				t.Fatalf("refusal echoes the NATS credential: %v", err)
			}
		})
	}
}

// TestPreflightProductionPostgresTLSVectors pins the production PostgreSQL rule
// for the runtime DSN: exactly one sslmode=verify-full option in a URL-form
// DSN. Every weaker mode, a missing mode, repeated or conflicting options,
// uppercase spellings and keyword-form DSNs are refused.
func TestPreflightProductionPostgresTLSVectors(t *testing.T) {
	const natsURL = "tls://nats.example:4222"
	const base = "postgres://kodama:pg-hunter2@database.example:5432/kodama"

	accepted := []struct {
		name string
		dsn  string
	}{
		{name: "verify full", dsn: base + "?sslmode=verify-full"},
		{name: "verify full with other options", dsn: base + "?connect_timeout=10&sslmode=verify-full"},
		{name: "verify full with application name", dsn: base + "?application_name=kodama_runtime&sslmode=verify-full"},
		{name: "verify full with root cert", dsn: base + "?sslmode=verify-full&sslrootcert=system"},
		{name: "verify full connect timeout zero", dsn: base + "?sslmode=verify-full&connect_timeout=0"},
		{name: "verify full ipv6", dsn: "postgres://kodama:pg-hunter2@[2001:db8::1]:5432/kodama?sslmode=verify-full"},
		{name: "verify full percent encoded", dsn: base + "?sslmode=verify%2Dfull"},
		{name: "port omitted", dsn: "postgres://kodama:pg-hunter2@database.example/kodama?sslmode=verify-full"},
		{name: "max port", dsn: "postgres://kodama:pg-hunter2@database.example:65535/kodama?sslmode=verify-full"},
		{name: "hyphenated host", dsn: "postgres://kodama:pg-hunter2@db-1.example/kodama?sslmode=verify-full"},
	}
	for _, testCase := range accepted {
		t.Run("accepted "+testCase.name, func(t *testing.T) {
			fields := productionURLDocument(testCase.dsn, natsURL)
			if _, err := ParseConfig(strings.NewReader(fields.String())); err != nil {
				t.Fatalf("ParseConfig refused production DSN %q: %v", testCase.dsn, err)
			}
		})
	}

	refused := []struct {
		name   string
		dsn    string
		secret string
	}{
		{name: "disable", dsn: base + "?sslmode=disable", secret: "pg-hunter2"},
		{name: "allow", dsn: base + "?sslmode=allow", secret: "pg-hunter2"},
		{name: "prefer", dsn: base + "?sslmode=prefer", secret: "pg-hunter2"},
		{name: "require", dsn: base + "?sslmode=require", secret: "pg-hunter2"},
		{name: "verify ca", dsn: base + "?sslmode=verify-ca", secret: "pg-hunter2"},
		{name: "missing sslmode", dsn: base, secret: "pg-hunter2"},
		{name: "query without sslmode", dsn: base + "?connect_timeout=10", secret: "pg-hunter2"},
		{name: "empty sslmode", dsn: base + "?sslmode=", secret: "pg-hunter2"},
		{name: "duplicated sslmode", dsn: base + "?sslmode=verify-full&sslmode=verify-full", secret: "pg-hunter2"},
		{name: "conflicting sslmode", dsn: base + "?sslmode=verify-full&sslmode=require", secret: "pg-hunter2"},
		{name: "conflicting sslmode reversed", dsn: base + "?sslmode=require&sslmode=verify-full", secret: "pg-hunter2"},
		{name: "uppercase sslmode key", dsn: base + "?SSLMODE=verify-full", secret: "pg-hunter2"},
		{name: "uppercase sslmode value", dsn: base + "?sslmode=VERIFY-FULL", secret: "pg-hunter2"},
		{name: "sslmode suffix", dsn: base + "?sslmode=verify-full-x", secret: "pg-hunter2"},
		{name: "sslmode prefix", dsn: base + "?sslmode=xverify-full", secret: "pg-hunter2"},
		{name: "sslmode percent nul", dsn: base + "?sslmode=verify-full%00", secret: "pg-hunter2"},
		{name: "sslmode percent space", dsn: base + "?sslmode=verify-full%20", secret: "pg-hunter2"},
		{name: "sslmode missing letter", dsn: base + "?sslmode=verify-ful", secret: "pg-hunter2"},
		{name: "sslmode extra letter", dsn: base + "?sslmode=verify-fullx", secret: "pg-hunter2"},
		{name: "keyword form", dsn: "host=database.example user=kodama password=pg-hunter2 dbname=kodama sslmode=verify-full", secret: "pg-hunter2"},
		{name: "keyword form without password", dsn: "host=database.example dbname=kodama sslmode=verify-full"},
		{name: "uppercase scheme", dsn: "POSTGRES://kodama:pg-hunter2@database.example:5432/kodama?sslmode=verify-full", secret: "pg-hunter2"},
		{name: "postgresql scheme", dsn: "postgresql://kodama:pg-hunter2@database.example:5432/kodama?sslmode=verify-full", secret: "pg-hunter2"},
		{name: "other scheme", dsn: "foo://kodama:pg-hunter2@database.example:5432/kodama?sslmode=verify-full", secret: "pg-hunter2"},
		{name: "fragment with weaker sslmode", dsn: base + "?sslmode=verify-full#&sslmode=disable", secret: "pg-hunter2"},
		{name: "fragment marker and query", dsn: base + "?sslmode=verify-full#fragment", secret: "pg-hunter2"},
		{name: "empty fragment marker", dsn: base + "?sslmode=verify-full#", secret: "pg-hunter2"},
		// The '#' guard is the whole raw string, so a '#' inside an allowed
		// query value (not only after the sslmode pair) is refused too: without
		// the guard pgx would treat the remainder as query text.
		{name: "fragment inside root cert value", dsn: base + "?sslrootcert=/etc/kodama/pg-ca/ca.crt#x&sslmode=verify-full", secret: "pg-hunter2"},
		{name: "fragment inside application name", dsn: base + "?application_name=app#x&sslmode=verify-full", secret: "pg-hunter2"},
		// A query pair without '=' must be refused without a slice panic.
		{name: "query pair without equals", dsn: base + "?sslmode=verify-full&host", secret: "pg-hunter2"},
		// A truncated percent escape must be refused, not indexed past.
		{name: "truncated percent escape", dsn: base + "?application_name=a%2&sslmode=verify-full", secret: "pg-hunter2"},
		// pgx refuses a second '=' inside a query value; the allowlist does too.
		{name: "equals inside application name", dsn: base + "?application_name=a=b&sslmode=verify-full", secret: "pg-hunter2"},
		// The port must be a plain decimal run: a signed port is refused.
		{name: "signed port", dsn: "postgres://database.example:+5/kodama?sslmode=verify-full", secret: "pg-hunter2"},
		// pgx refuses bytes after the closing IPv6 bracket.
		{name: "ipv6 trailing byte", dsn: "postgres://[::1]x/kodama?sslmode=verify-full"},
		// A percent-encoded database name is refused; it could smuggle '/'.
		{name: "percent encoded database name", dsn: "postgres://database.example/%6Bodama?sslmode=verify-full"},
		// A percent escape that decodes to whitespace is refused even though
		// the raw bytes are printable.
		{name: "decoded whitespace application name", dsn: base + "?application_name=%20&sslmode=verify-full"},
		{name: "ssl true alias after sslmode", dsn: base + "?sslmode=verify-full&ssl=true", secret: "pg-hunter2"},
		{name: "ssl true alias before sslmode", dsn: base + "?ssl=true&sslmode=verify-full", secret: "pg-hunter2"},
		{name: "ssl non-true alias", dsn: base + "?sslmode=verify-full&ssl=false", secret: "pg-hunter2"},
		{name: "host override unix socket", dsn: base + "?sslmode=verify-full&host=/var/run/postgresql", secret: "pg-hunter2"},
		{name: "host override empty", dsn: base + "?sslmode=verify-full&host=", secret: "pg-hunter2"},
		{name: "hostaddr override", dsn: base + "?sslmode=verify-full&hostaddr=10.0.0.1", secret: "pg-hunter2"},
		{name: "service", dsn: base + "?sslmode=verify-full&service=kodama", secret: "pg-hunter2"},
		{name: "passfile", dsn: base + "?sslmode=verify-full&passfile=/tmp/pgpass", secret: "pg-hunter2"},
		{name: "sslcert", dsn: base + "?sslmode=verify-full&sslcert=/tmp/client.crt", secret: "pg-hunter2"},
		{name: "sslkey", dsn: base + "?sslmode=verify-full&sslkey=/tmp/client.key", secret: "pg-hunter2"},
		{name: "target session attrs", dsn: base + "?sslmode=verify-full&target_session_attrs=any", secret: "pg-hunter2"},
		{name: "options", dsn: base + "?sslmode=verify-full&options=-csearch_path%3Dpublic", secret: "pg-hunter2"},
		{name: "sslnegotiation", dsn: base + "?sslmode=verify-full&sslnegotiation=direct", secret: "pg-hunter2"},
		{name: "repeated connect timeout", dsn: base + "?connect_timeout=10&connect_timeout=20&sslmode=verify-full", secret: "pg-hunter2"},
		{name: "connect timeout overflow", dsn: base + "?connect_timeout=999999999999999999999&sslmode=verify-full", secret: "pg-hunter2"},
		{name: "connect timeout not numeric", dsn: base + "?connect_timeout=ten&sslmode=verify-full", secret: "pg-hunter2"},
		{name: "repeated application name", dsn: base + "?application_name=a&application_name=b&sslmode=verify-full", secret: "pg-hunter2"},
		{name: "repeated root cert", dsn: base + "?sslrootcert=/a&sslrootcert=/b&sslmode=verify-full", secret: "pg-hunter2"},
		{name: "percent encoded key", dsn: base + "?%73slmode=verify-full", secret: "pg-hunter2"},
		{name: "malformed percent value", dsn: base + "?sslmode=verify%zzfull", secret: "pg-hunter2"},
		{name: "malformed percent key", dsn: base + "?%zz=value&sslmode=verify-full", secret: "pg-hunter2"},
		{name: "malformed percent other value", dsn: base + "?application_name=%zz&sslmode=verify-full", secret: "pg-hunter2"},
		{name: "empty host", dsn: "postgres:///kodama?sslmode=verify-full", secret: "pg-hunter2"},
		{name: "query host unix socket", dsn: "postgres:///kodama?host=/var/run/postgresql&sslmode=verify-full", secret: "pg-hunter2"},
		{name: "percent encoded unix host", dsn: "postgres://%2Fvar%2Frun%2Fpostgresql/kodama?sslmode=verify-full", secret: "pg-hunter2"},
		{name: "comma host list", dsn: "postgres://database.example,/var/run/postgresql/kodama?sslmode=verify-full", secret: "pg-hunter2"},
		{name: "empty host element", dsn: "postgres://,database.example/kodama?sslmode=verify-full", secret: "pg-hunter2"},
		{name: "two host elements", dsn: "postgres://a.example,b.example/kodama?sslmode=verify-full", secret: "pg-hunter2"},
		{name: "missing database path", dsn: "postgres://database.example?sslmode=verify-full", secret: "pg-hunter2"},
		{name: "empty database path", dsn: "postgres://database.example/?sslmode=verify-full", secret: "pg-hunter2"},
		{name: "nested database path", dsn: "postgres://database.example/kodama/extra?sslmode=verify-full", secret: "pg-hunter2"},
		{name: "port zero", dsn: "postgres://database.example:0/kodama?sslmode=verify-full", secret: "pg-hunter2"},
		{name: "port too large", dsn: "postgres://database.example:65536/kodama?sslmode=verify-full", secret: "pg-hunter2"},
		{name: "port not numeric", dsn: "postgres://database.example:abc/kodama?sslmode=verify-full", secret: "pg-hunter2"},
		{name: "empty port", dsn: "postgres://database.example:/kodama?sslmode=verify-full", secret: "pg-hunter2"},
		{name: "encoded at in userinfo", dsn: "postgres://user%40x:pg-hunter2@database.example:5432/kodama?sslmode=verify-full", secret: "pg-hunter2"},
		{name: "encoded colon in userinfo", dsn: "postgres://user:p%3Ahunter2@database.example:5432/kodama?sslmode=verify-full", secret: "hunter2"},
		{name: "two at signs", dsn: "postgres://a@b:pg-hunter2@database.example:5432/kodama?sslmode=verify-full", secret: "pg-hunter2"},
		{name: "empty userinfo", dsn: "postgres://@database.example:5432/kodama?sslmode=verify-full", secret: "pg-hunter2"},
		{name: "unterminated ipv6", dsn: "postgres://[2001:db8::1/kodama?sslmode=verify-full"},
		{name: "empty ipv6", dsn: "postgres://[]/kodama?sslmode=verify-full"},
		{name: "embedded space", dsn: base + "?sslmode=verify full", secret: "pg-hunter2"},
	}
	for _, testCase := range refused {
		t.Run("refused "+testCase.name, func(t *testing.T) {
			fields := productionURLDocument(testCase.dsn, natsURL)
			_, err := ParseConfig(strings.NewReader(fields.String()))
			assertFaultClass(t, err, fault.InvalidInput)
			if strings.Contains(err.Error(), testCase.dsn) {
				t.Fatalf("refusal echoes the configured DSN: %v", err)
			}
			if testCase.secret != "" && strings.Contains(err.Error(), testCase.secret) {
				t.Fatalf("refusal echoes the DSN password: %v", err)
			}
		})
	}
}

// TestPreflightProductionCredentialGrammar pins the userinfo grammar.
//
// The production PostgreSQL validator is percent-free on purpose. pgx would
// percent-decode userinfo, so p%40ss%2Fw%3Ard is refused ("userinfo has
// unsupported characters") even though the driver would decode it. Generated
// passwords are hex and pass. The accepted DSN shapes below are exactly what
// the real flow emits (hex password plus the chart/script root cert) for each
// of the four production roles, plus the full URL-unreserved credential
// alphabet.
func TestPreflightProductionCredentialGrammar(t *testing.T) {
	const hex48 = "0123456789abcdef0123456789abcdef0123456789abcdef"
	const rootCert = "?sslmode=verify-full&sslrootcert=/etc/kodama/pg-ca/ca.crt"

	accepted := []struct {
		name string
		dsn  string
	}{
		{
			name: "runtime role with generated hex password",
			dsn:  "postgres://kodama_runtime:" + hex48 + "@database:5432/kodama" + rootCert,
		},
		{
			name: "controller role with generated hex password",
			dsn:  "postgres://kodama_controller:" + hex48 + "@database:5432/kodama" + rootCert,
		},
		{
			name: "publisher role with generated hex password",
			dsn:  "postgres://kodama_publisher:" + hex48 + "@database:5432/kodama" + rootCert,
		},
		{
			name: "auth role with generated hex password",
			dsn:  "postgres://kodama_auth:" + hex48 + "@database:5432/kodama" + rootCert,
		},
		{
			name: "url-safe password alphabet",
			dsn:  "postgres://kodama_runtime:a-b.c_d~e@database:5432/kodama?sslmode=verify-full",
		},
	}
	for _, testCase := range accepted {
		t.Run("accepted "+testCase.name, func(t *testing.T) {
			if err := ValidateProductionPostgresURL(testCase.dsn); err != nil {
				t.Fatalf("ValidateProductionPostgresURL refused %s: %v", testCase.name, err)
			}
		})
	}

	refused := []struct {
		name string
		dsn  string
	}{
		// Percent-encoded userinfo is refused on purpose (see the test
		// comment above); pgx would decode each of these.
		{name: "percent encoded at sign", dsn: "postgres://kodama_runtime:p%40ss@database:5432/kodama?sslmode=verify-full"},
		{name: "percent encoded hash", dsn: "postgres://kodama_runtime:p%23x@database:5432/kodama?sslmode=verify-full"},
		{name: "percent encoded percent", dsn: "postgres://kodama_runtime:p%25@database:5432/kodama?sslmode=verify-full"},
		{name: "percent encoded slash and colon", dsn: "postgres://kodama_runtime:p%40ss%2Fw%3Ard@database:5432/kodama?sslmode=verify-full"},
	}
	for _, testCase := range refused {
		t.Run("refused "+testCase.name, func(t *testing.T) {
			err := ValidateProductionPostgresURL(testCase.dsn)
			assertFaultClass(t, err, fault.InvalidInput)
			if strings.Contains(err.Error(), testCase.dsn) {
				t.Fatalf("refusal echoes the configured DSN: %v", err)
			}
			if strings.Contains(err.Error(), "p%") || strings.Contains(err.Error(), "p@") {
				t.Fatalf("refusal echoes the configured credential: %v", err)
			}
		})
	}
}

// TestPreflightDirectKeyFamilyValidation proves Preflight itself, not only the
// later key client constructor, refuses a key reference that does not belong to
// the declared key adapter family. Development's explicit-reference case is
// covered too.
func TestPreflightDirectKeyFamilyValidation(t *testing.T) {
	accepted := []struct {
		name   string
		config Config
	}{
		{
			name: "development without reference",
			config: Config{
				Environment: EnvironmentDevelopment,
				State:       StateMemory,
				Messaging:   MessagingMemory,
				Runtime:     RuntimeProcess,
				Keys:        KeyEphemeral,
			},
		},
		{
			name: "development ephemeral reference",
			config: Config{
				Environment: EnvironmentDevelopment,
				State:       StateMemory,
				Messaging:   MessagingMemory,
				Runtime:     RuntimeProcess,
				Keys:        KeyEphemeral,
				KeyRef:      "ephemeral:dev",
			},
		},
		{
			name:   "test keyfile reference",
			config: keyFamilyTestConfig(KeyKeyfile, "keyfile:kodama"),
		},
		{
			name:   "production aws reference",
			config: keyFamilyProductionConfig(KeyAWSKMS, "aws-kms:alias/kodama"),
		},
		{
			name:   "production gcp reference",
			config: keyFamilyProductionConfig(KeyGCPKMS, "gcp-kms:projects/kodama/locations/global/keyRings/kodama/cryptoKeys/kek"),
		},
		{
			name:   "production scaleway reference",
			config: keyFamilyProductionConfig(KeyScalewayKMS, "scw-kms:fr-par/kodama"),
		},
	}
	for _, testCase := range accepted {
		t.Run("accepted "+testCase.name, func(t *testing.T) {
			if err := testCase.config.Preflight(); err != nil {
				t.Fatalf("Preflight refused %s: %v", testCase.name, err)
			}
		})
	}

	refused := []struct {
		name   string
		config Config
	}{
		{name: "production aws names gcp", config: keyFamilyProductionConfig(KeyAWSKMS, "gcp-kms:projects/kodama/locations/global/keyRings/kodama/cryptoKeys/kek")},
		{name: "production gcp names aws", config: keyFamilyProductionConfig(KeyGCPKMS, "aws-kms:alias/kodama")},
		{name: "production scaleway names gcp", config: keyFamilyProductionConfig(KeyScalewayKMS, "gcp-kms:projects/kodama/locations/global/keyRings/kodama/cryptoKeys/kek")},
		{name: "production aws legacy arn", config: keyFamilyProductionConfig(KeyAWSKMS, "arn:aws:kms:eu-west-1:111122223333:key/example")},
		// Prefix tricks that a Contains, lower-case or Trim-style family match
		// would accept: the family prefix must be an exact case-sensitive
		// prefix and the key name must carry no whitespace.
		{name: "production aws uppercase prefix", config: keyFamilyProductionConfig(KeyAWSKMS, "AWS-KMS:alias/kodama")},
		{name: "production aws leading space", config: keyFamilyProductionConfig(KeyAWSKMS, " aws-kms:alias/kodama")},
		{name: "production aws trailing space", config: keyFamilyProductionConfig(KeyAWSKMS, "aws-kms:alias/kodama ")},
		{name: "production aws space after prefix", config: keyFamilyProductionConfig(KeyAWSKMS, "aws-kms: alias/kodama")},
		{name: "production aws hyphen variant", config: keyFamilyProductionConfig(KeyAWSKMS, "aws-kms-x:y")},
		{name: "production aws tail names gcp", config: keyFamilyProductionConfig(KeyAWSKMS, "gcp-kms:aws-kms:x")},
		{name: "production gcp uppercase prefix", config: keyFamilyProductionConfig(KeyGCPKMS, "GCP-KMS:projects/kodama")},
		{name: "production gcp leading space", config: keyFamilyProductionConfig(KeyGCPKMS, " gcp-kms:projects/kodama")},
		{name: "production gcp trailing space", config: keyFamilyProductionConfig(KeyGCPKMS, "gcp-kms:projects/kodama ")},
		{name: "production gcp hyphen variant", config: keyFamilyProductionConfig(KeyGCPKMS, "gcp-kms-x:y")},
		{name: "production gcp tail names aws", config: keyFamilyProductionConfig(KeyGCPKMS, "aws-kms:gcp-kms:x")},
		{name: "production scaleway uppercase prefix", config: keyFamilyProductionConfig(KeyScalewayKMS, "SCW-KMS:fr-par/kodama")},
		{name: "production scaleway leading space", config: keyFamilyProductionConfig(KeyScalewayKMS, " scw-kms:fr-par/kodama")},
		{name: "production scaleway trailing space", config: keyFamilyProductionConfig(KeyScalewayKMS, "scw-kms:fr-par/kodama ")},
		{name: "production scaleway hyphen variant", config: keyFamilyProductionConfig(KeyScalewayKMS, "scw-kms-x:y")},
		{name: "production scaleway tail names aws", config: keyFamilyProductionConfig(KeyScalewayKMS, "aws-kms:scw-kms:x")},
		{name: "test keyfile uppercase prefix", config: keyFamilyTestConfig(KeyKeyfile, "KEYFILE:kodama")},
		{name: "test keyfile leading space", config: keyFamilyTestConfig(KeyKeyfile, " keyfile:kodama")},
		{name: "test keyfile trailing space", config: keyFamilyTestConfig(KeyKeyfile, "keyfile:kodama ")},
		{name: "test keyfile hyphen variant", config: keyFamilyTestConfig(KeyKeyfile, "keyfile-x:y")},
		{name: "test keyfile tail names ephemeral", config: keyFamilyTestConfig(KeyKeyfile, "ephemeral:keyfile:x")},
		{name: "development keyfile reference", config: Config{
			Environment: EnvironmentDevelopment,
			State:       StateMemory,
			Messaging:   MessagingMemory,
			Runtime:     RuntimeProcess,
			Keys:        KeyEphemeral,
			KeyRef:      "keyfile:kodama",
		}},
		{name: "development cloud reference", config: Config{
			Environment: EnvironmentDevelopment,
			State:       StateMemory,
			Messaging:   MessagingMemory,
			Runtime:     RuntimeProcess,
			Keys:        KeyEphemeral,
			KeyRef:      "aws-kms:alias/kodama",
		}},
		{name: "test ephemeral reference", config: keyFamilyTestConfig(KeyKeyfile, "ephemeral:test")},
	}
	for _, testCase := range refused {
		t.Run("refused "+testCase.name, func(t *testing.T) {
			err := testCase.config.Preflight()
			assertFaultClass(t, err, fault.InvalidInput)
		})
	}

	// The key name after an exact family prefix must be non-empty and free of
	// whitespace, control bytes and DEL. Each family is checked directly so a
	// removed empty-name, IsSpace or IsControl guard cannot survive.
	keyNames := []struct {
		name  string
		value string
	}{
		{name: "empty name", value: ""},
		{name: "interior space", value: "alias/kodama key"},
		{name: "interior tab", value: "alias/kodama\tkey"},
		{name: "control byte", value: "alias/kodama\x01key"},
		{name: "nul byte", value: "alias/kodama\x00key"},
		{name: "del byte", value: "alias/kodama\x7fkey"},
	}
	families := []struct {
		name   string
		config func(string) Config
	}{
		{name: "ephemeral", config: func(name string) Config {
			return Config{
				Environment: EnvironmentDevelopment,
				State:       StateMemory,
				Messaging:   MessagingMemory,
				Runtime:     RuntimeProcess,
				Keys:        KeyEphemeral,
				KeyRef:      KeyRefEphemeralPrefix + name,
			}
		}},
		{name: "keyfile", config: func(name string) Config {
			return keyFamilyTestConfig(KeyKeyfile, KeyRefKeyfilePrefix+name)
		}},
		{name: "aws", config: func(name string) Config {
			return keyFamilyProductionConfig(KeyAWSKMS, KeyRefAWSKMSPrefix+"alias/"+name)
		}},
		{name: "gcp", config: func(name string) Config {
			return keyFamilyProductionConfig(KeyGCPKMS, KeyRefGCPKMSPrefix+"projects/kodama/locations/global/keyRings/kodama/cryptoKeys/"+name)
		}},
		{name: "scaleway", config: func(name string) Config {
			return keyFamilyProductionConfig(KeyScalewayKMS, KeyRefScalewayKMSPrefix+name)
		}},
	}
	for _, family := range families {
		for _, keyName := range keyNames {
			t.Run("refused "+family.name+" "+keyName.name, func(t *testing.T) {
				err := family.config(keyName.value).Preflight()
				assertFaultClass(t, err, fault.InvalidInput)
				if keyName.value != "" && strings.Contains(err.Error(), keyName.value) {
					t.Fatalf("refusal echoes the key name: %v", err)
				}
			})
		}
	}
}

// TestPreflightRefusalsNeverEchoCredentials proves every refusal is
// secret-free: the DSN, the NATS credential and the key reference are never
// echoed.
func TestPreflightRefusalsNeverEchoCredentials(t *testing.T) {
	cases := []struct {
		name   string
		config Config
		secret string
	}{
		{
			name: "plaintext NATS with credential",
			config: Config{
				Environment: EnvironmentProduction,
				State:       StatePostgres,
				Messaging:   MessagingNATS,
				Runtime:     RuntimeKubernetes,
				Keys:        KeyAWSKMS,
				DatabaseURL: "postgres://kodama@database.example:5432/kodama?sslmode=verify-full",
				NATSURL:     "nats://user:nats-hunter2@nats.example:4222",
				PublicURL:   "https://kodama.example",
				KeyRef:      "aws-kms:alias/kodama",
			},
			secret: "nats-hunter2",
		},
		{
			name: "weak DSN with credential",
			config: Config{
				Environment: EnvironmentProduction,
				State:       StatePostgres,
				Messaging:   MessagingNATS,
				Runtime:     RuntimeKubernetes,
				Keys:        KeyAWSKMS,
				DatabaseURL: "postgres://kodama:pg-hunter2@database.example:5432/kodama?sslmode=require",
				NATSURL:     "tls://nats.example:4222",
				PublicURL:   "https://kodama.example",
				KeyRef:      "aws-kms:alias/kodama",
			},
			secret: "pg-hunter2",
		},
		{
			name:   "foreign key family reference",
			config: keyFamilyProductionConfig(KeyAWSKMS, "gcp-kms:key-hunter2"),
			secret: "key-hunter2",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.config.Preflight()
			assertFaultClass(t, err, fault.InvalidInput)
			if strings.Contains(err.Error(), testCase.secret) {
				t.Fatalf("refusal echoes the configured credential: %v", err)
			}
		})
	}
}

// TestPreflightURLParserEdgeCases pins the parser edge cases:
// percent-encoded sslmode values are decoded, repeated keys are refused, the
// scheme is matched as an exact lower-case prefix (pgx's own prefix check is
// case-sensitive), and IPv6 hosts parse.
func TestPreflightURLParserEdgeCases(t *testing.T) {
	const natsURL = "tls://nats.example:4222"
	const base = "postgres://kodama:pg-hunter2@database.example:5432/kodama"

	t.Run("percent encoded sslmode accepted", func(t *testing.T) {
		fields := productionURLDocument(base+"?sslmode=verify%2Dfull", natsURL)
		if _, err := ParseConfig(strings.NewReader(fields.String())); err != nil {
			t.Fatalf("ParseConfig refused percent-encoded verify-full: %v", err)
		}
	})

	t.Run("repeated sslmode refused", func(t *testing.T) {
		fields := productionURLDocument(base+"?sslmode=verify-full&sslmode=verify-full", natsURL)
		_, err := ParseConfig(strings.NewReader(fields.String()))
		assertFaultClass(t, err, fault.InvalidInput)
	})

	t.Run("uppercase scheme refused", func(t *testing.T) {
		fields := productionURLDocument("POSTGRES://kodama@database.example:5432/kodama?sslmode=verify-full", "TLS://nats.example:4222")
		_, err := ParseConfig(strings.NewReader(fields.String()))
		assertFaultClass(t, err, fault.InvalidInput)
	})

	t.Run("ipv6 hosts accepted", func(t *testing.T) {
		fields := productionURLDocument("postgres://kodama@[2001:db8::1]:5432/kodama?sslmode=verify-full", "tls://[2001:db8::1]:4222")
		if _, err := ParseConfig(strings.NewReader(fields.String())); err != nil {
			t.Fatalf("ParseConfig refused IPv6 hosts: %v", err)
		}
	})

	t.Run("malformed query percent refused", func(t *testing.T) {
		fields := productionURLDocument(base+"?sslmode=verify-full&application_name=%zz", natsURL)
		_, err := ParseConfig(strings.NewReader(fields.String()))
		assertFaultClass(t, err, fault.InvalidInput)
	})
}

// TestProductionTransportValidatorsDirect pins the raw-string rules the
// installation document parser would otherwise trim: the exported PostgreSQL
// validator and the package NATS validator refuse leading/trailing whitespace,
// control bytes and a percent-encoded byte that decodes to whitespace.
func TestProductionTransportValidatorsDirect(t *testing.T) {
	const base = "postgres://kodama:pg-hunter2@database.example:5432/kodama"

	refusedDSNs := []string{
		" " + base + "?sslmode=verify-full",
		base + "?sslmode=verify-full ",
		base + "?sslmode=verify\tfull",
		base + "?sslmode=verify-full\n",
		base + "\x00?sslmode=verify-full",
		base + "?sslmode=verify%2Dfull%00",
		base + "?sslmode=verify-full&host",
		base + "?application_name=a%2&sslmode=verify-full",
		base + "?application_name=%20&sslmode=verify-full",
		base + "?application_name=a=b&sslmode=verify-full",
		base + "?application_name=app#x&sslmode=verify-full",
		"postgres://database.example:+5/kodama?sslmode=verify-full",
		"postgres://[::1]x/kodama?sslmode=verify-full",
		"postgres://database.example/%6Bodama?sslmode=verify-full",
	}
	for _, dsn := range refusedDSNs {
		if err := ValidateProductionPostgresURL(dsn); err == nil {
			t.Errorf("ValidateProductionPostgresURL accepted %q; want fault.InvalidInput", dsn)
		} else {
			assertFaultClass(t, err, fault.InvalidInput)
		}
	}

	refusedNATS := []string{
		" tls://nats.example:4222",
		"tls://nats.example:4222 ",
		"tls://nats.example:4222\t",
		"tls://nats.example:4222\x00",
		"tls://a:4222,b:4222",
		"tls://a,nats://user:pw@b:4222",
		"tls://nats.example:4222/,nats://evil:4222",
		"tls://nats.example:4222?x,nats://evil:4222",
		"tls://nats.example:4222/p,tls://other",
		"tls://nats.example:4222/x#frag",
		"tls://nats.example:4222/?a=b#",
		"tls://nats.example:4222/x@evil",
		"tls://user:pw@nats.example:4222",
		"tls://@nats.example:4222",
		"tls://nats.example:4222#",
		"tls://nats.example:4222#fragment",
		"tls://nats.example:0",
		"tls://nats.example:99999",
		"tls://:4222",
		// Same-length wrong schemes exercise the case-folded scheme comparison
		// rather than the length guard.
		"tcp://h",
		"wss://h",
		// Short values exercise the length guard before the prefix slice.
		"",
		"tls",
		"tls:",
	}
	for _, url := range refusedNATS {
		err := validateProductionNATSURL(url)
		assertFaultClass(t, err, fault.InvalidInput)
	}
}

// TestPreflightTestEnvironmentRequiresHost pins the test-environment host rule
// next to the production validators: validateEndpoints requires a host for the
// database, NATS and public URLs in the test environment too, so an empty-host
// URL with a unix-socket query override or a bare https:///path is refused.
func TestPreflightTestEnvironmentRequiresHost(t *testing.T) {
	refused := []struct {
		name   string
		mutate func(*docFields)
	}{
		{name: "database empty host with socket query", mutate: func(fields *docFields) {
			fields.databaseURL = "postgres:///kodama?host=/sock"
		}},
		{name: "database empty host", mutate: func(fields *docFields) {
			fields.databaseURL = "postgres:///kodama"
		}},
		{name: "database comma host", mutate: func(fields *docFields) {
			fields.databaseURL = "postgres://a.example,b.example:5432/kodama"
		}},
		{name: "public empty host", mutate: func(fields *docFields) {
			fields.publicURL = "https:///x"
		}},
		{name: "public comma host", mutate: func(fields *docFields) {
			fields.publicURL = "https://a.example,b.example"
		}},
		{name: "nats empty host", mutate: func(fields *docFields) {
			fields.natsURL = "nats://:4222"
		}},
		{name: "nats comma host", mutate: func(fields *docFields) {
			fields.natsURL = "nats://a:4222,b:4222"
		}},
	}
	for _, testCase := range refused {
		t.Run("refused "+testCase.name, func(t *testing.T) {
			fields := testDoc()
			testCase.mutate(&fields)
			_, err := ParseConfig(strings.NewReader(fields.String()))
			assertFaultClass(t, err, fault.InvalidInput)
		})
	}

	t.Run("accepted plain test URLs", func(t *testing.T) {
		if _, err := ParseConfig(strings.NewReader(testDoc().String())); err != nil {
			t.Fatalf("ParseConfig refused the declared test composition: %v", err)
		}
		fields := testDoc()
		fields.databaseURL = "postgres://kodama@database/kodama"
		fields.natsURL = "nats://nats"
		if _, err := ParseConfig(strings.NewReader(fields.String())); err != nil {
			t.Fatalf("ParseConfig refused portless test URLs: %v", err)
		}
	})
}

// keyFamilyTestConfig is one complete test composition with the given key
// adapter and reference.
func keyFamilyTestConfig(keys KeyAdapter, keyRef string) Config {
	return Config{
		Environment: EnvironmentTest,
		State:       StatePostgres,
		Messaging:   MessagingNATS,
		Runtime:     RuntimePodman,
		Keys:        keys,
		DatabaseURL: "postgres://kodama@database.example:5432/kodama",
		NATSURL:     "nats://nats.example:4222",
		PublicURL:   "https://kodama.test",
		KeyRef:      keyRef,
	}
}

// keyFamilyProductionConfig is one complete production composition with the
// given key adapter and reference.
func keyFamilyProductionConfig(keys KeyAdapter, keyRef string) Config {
	return Config{
		Environment: EnvironmentProduction,
		State:       StatePostgres,
		Messaging:   MessagingNATS,
		Runtime:     RuntimeKubernetes,
		Keys:        keys,
		DatabaseURL: "postgres://kodama@database.example:5432/kodama?sslmode=verify-full",
		NATSURL:     "tls://nats.example:4222",
		PublicURL:   "https://kodama.example",
		KeyRef:      keyRef,
	}
}
