package adaptercontract

// Production-transport drift guard.
//
// The production PostgreSQL validator in internal/installation is a strict
// allowlist, not a general URL parser, because the driver that consumes the DSN
// is pgx. pgx v5's pgconn parser follows libpq: it treats a fragment marker as
// query text, supports the ssl=true alias, accepts host/hostaddr and unix
// socket overrides, supports comma host lists and matches its prefix
// case-sensitively. A net/url-based validator accepts forms that pgx then
// connects without verified TLS.
//
// This test is the differential between the two parsers. For every DSN the
// production validator accepts, pgconn.ParseConfig must produce a config whose
// primary and every fallback carries a non-nil TLSConfig with verification on
// and ServerName set to the host, and no host may resolve to a Unix socket.
// A DSN the validator refuses may be anything; that freedom is what lets the
// validator stay conservative. Test files may import pgx even though
// internal/installation production files may not.

import (
	"crypto/tls"
	"math/rand"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/optimiware/kodama/internal/installation"
)

// productionPostgresDifferentialCorpusSize is the number of hand-written
// vectors below; the generated grid adds thousands more.
const productionPostgresDifferentialCorpusSize = 115

// productionPostgresDifferentialCorpus is the differential corpus. It contains
// every accepted and refused vector from internal/installation and
// internal/shell/app, six known parser-differential forms,
// and fuzz-like variants around keys, fragments, hosts, ports and userinfo.
// DSNs carry a test password; the failure messages never echo a corpus entry.
func productionPostgresDifferentialCorpus() []string {
	const base = "postgres://kodama:pg-hunter2@database.example:5432/kodama"
	return []string{
		// Accepted forms.
		base + "?sslmode=verify-full",
		base + "?connect_timeout=10",
		base + "?sslmode=verify%2Dfull",
		base + "?connect_timeout=10&sslmode=verify-full",
		base + "?connect_timeout=999999999999999999999&sslmode=verify-full",
		base + "?connect_timeout=ten&sslmode=verify-full",
		base + "?application_name=kodama_runtime&sslmode=verify-full",
		base + "?sslmode=verify-full&sslrootcert=system",
		"postgres://kodama:pg-hunter2@[2001:db8::1]:5432/kodama?sslmode=verify-full",
		"postgres://kodama:pg-hunter2@database.example/kodama?sslmode=verify-full",
		"postgres://kodama:pg-hunter2@database.example:65535/kodama?sslmode=verify-full",
		"postgres://kodama:pg-hunter2@db-1.example/kodama?sslmode=verify-full",
		"postgres://database.example/kodama?sslmode=verify-full",

		// Six parser-differential forms: a strict validator refuses them, and
		// a mutant that accepts one must fail the pgx assertions below.
		"postgres://h/db?sslmode=verify-full#&sslmode=disable",
		"postgres://h/db?sslmode=verify-full&ssl=true",
		"postgres://h/db?sslmode=verify-full&host=/var/run/postgresql",
		"postgres://h/db?sslmode=verify-full&host=",
		"POSTGRES://h/db?sslmode=verify-full",
		"postgres://h1,/var/run/db?sslmode=verify-full",

		// sslmode spellings and aliases.
		base + "?sslmode=disable",
		base + "?sslmode=allow",
		base + "?sslmode=prefer",
		base + "?sslmode=require",
		base + "?sslmode=verify-ca",
		base + "?sslmode=",
		base + "?sslmode=verify-full&sslmode=verify-full",
		base + "?sslmode=verify-full&sslmode=require",
		base + "?sslmode=require&sslmode=verify-full",
		base + "?sslmode=VERIFY-FULL",
		base + "?SSLMODE=verify-full",
		base + "?sslmode=verify-full-x",
		base + "?sslmode=xverify-full",
		base + "?sslmode=verify-full%00",
		base + "?sslmode=verify-full%20",
		base + "?sslmode=verify-ful",
		base + "?sslmode=verify-fullx",
		base + "?sslmode=verify%zzfull",
		base + "?ssl=true",
		base + "?ssl=false&sslmode=verify-full",
		base + "?sslmode=verify-full&ssl=true",
		base + "?ssl=true&sslmode=verify-full",
		base + "?ssl=true&sslmode=require",
		base + "?ssl=true&sslmode=disable",
		base + "?sslmode=verify-full#&sslmode=disable",
		base + "?sslmode=verify-full#",
		base + "?sslmode=verify-full#fragment",
		base + "?sslmode=verify-full#&ssl=true",

		// Host and authority overrides.
		base + "?sslmode=verify-full&host=/var/run/postgresql",
		base + "?sslmode=verify-full&host=",
		base + "?sslmode=verify-full&host=database.example",
		base + "?sslmode=verify-full&hostaddr=10.0.0.1",
		base + "?sslmode=verify-full&port=5432",
		base + "?sslmode=verify-full&service=kodama",
		base + "?sslmode=verify-full&passfile=/tmp/pgpass",
		base + "?sslmode=verify-full&sslcert=/tmp/client.crt",
		base + "?sslmode=verify-full&sslkey=/tmp/client.key",
		base + "?sslmode=verify-full&target_session_attrs=any",
		base + "?sslmode=verify-full&options=-csearch_path%3Dpublic",
		base + "?sslmode=verify-full&sslnegotiation=direct",
		base + "?sslmode=verify-full&sslsni=0",
		base + "?sslmode=verify-full&channel_binding=disable",
		base + "?%73slmode=verify-full",
		base + "?%53SLMODE=verify-full",
		base + "?sslmode=verify-full&%68ost=/tmp",
		base + "?application_name=%zz&sslmode=verify-full",
		base + "?%zz=value&sslmode=verify-full",
		base + "?sslmode=verify-full&connect_timeout=10&connect_timeout=20",
		base + "?application_name=a&application_name=b&sslmode=verify-full",
		base + "?sslrootcert=/a&sslrootcert=/b&sslmode=verify-full",

		// Host lists, empty elements and Unix sockets.
		"postgres:///db?sslmode=verify-full",
		"postgres:///db?host=/sock&sslmode=verify-full",
		"postgres:///db?host=/var/run/postgresql&sslmode=verify-full",
		"postgres://%2Fvar%2Frun%2Fpostgresql/db?sslmode=verify-full",
		"postgres://%2Ftmp/db?sslmode=verify-full",
		"postgres:///var/run/db?sslmode=verify-full",
		"postgres://,database.example/db?sslmode=verify-full",
		"postgres://database.example,/var/run/db?sslmode=verify-full",
		"postgres://h1,/var/run/db?sslmode=verify-full",
		"postgres://h1,,h2/db?sslmode=verify-full",
		"postgres://a.example,b.example/db?sslmode=verify-full",
		"postgres://@/db?sslmode=verify-full",
		"postgres://@,/db?sslmode=verify-full",

		// Scheme spellings.
		"postgresql://kodama:pg-hunter2@database.example:5432/kodama?sslmode=verify-full",
		"Postgres://kodama:pg-hunter2@database.example:5432/kodama?sslmode=verify-full",
		"POSTGRES://kodama:pg-hunter2@database.example:5432/kodama?sslmode=verify-full",
		"postgres:/kodama?sslmode=verify-full",
		"postgres:database.example/kodama?sslmode=verify-full",
		"foo://database.example/kodama?sslmode=verify-full",
		"http://database.example/kodama?sslmode=verify-full",
		"host=database.example dbname=kodama sslmode=verify-full",
		"",
		"   ",
		" " + base + "?sslmode=verify-full",
		base + "?sslmode=verify-full ",
		base + "?sslmode=verify full",
		base + "\t?sslmode=verify-full",
		base + "?sslmode=verify-full\n",

		// Paths, ports and userinfo oddities.
		"postgres://database.example?sslmode=verify-full",
		"postgres://database.example/?sslmode=verify-full",
		"postgres://database.example/kodama/extra?sslmode=verify-full",
		"postgres://database.example/%6Bodama?sslmode=verify-full",
		"postgres://database.example:0/kodama?sslmode=verify-full",
		"postgres://database.example:65536/kodama?sslmode=verify-full",
		"postgres://database.example:99999/kodama?sslmode=verify-full",
		"postgres://database.example:-1/kodama?sslmode=verify-full",
		"postgres://database.example:abc/kodama?sslmode=verify-full",
		"postgres://database.example:/kodama?sslmode=verify-full",
		"postgres://user%40x:pg-hunter2@database.example/kodama?sslmode=verify-full",
		"postgres://user:p%3Ahunter2@database.example/kodama?sslmode=verify-full",
		"postgres://a@b:pg-hunter2@database.example/kodama?sslmode=verify-full",
		"postgres://@database.example/kodama?sslmode=verify-full",
		"postgres://:pg-hunter2@database.example/kodama?sslmode=verify-full",
		"postgres://[2001:db8::1/kodama?sslmode=verify-full",
		"postgres://[]/kodama?sslmode=verify-full",
		"postgres://[2001:db8::1]:0/kodama?sslmode=verify-full",
	}
}

// generatedProductionPostgresCorpusSeed is the fixed seed of the generated
// differential grid. The grid is deterministic: the same seed always produces
// the same order, so a failure is reproducible.
const generatedProductionPostgresCorpusSeed int64 = 0xC02B201

// generatedProductionPostgresCorpus builds a deterministic grid over authority
// variants (plain host, bracketed IPv6, userinfo forms, port forms), path
// variants and a bounded query-part set. The query keys are drawn from the
// union of the production allowlist and the hostile overrides the validator
// must refuse; values include odd spellings. The corpus is shuffled with the
// fixed seed so the accepted/refused mix is not grouped, but the bytes are
// reproducible.
//
// The assertions in the differential test are what matter: for every DSN the
// strict validator accepts, pgconn.ParseConfig must build a primary config,
// and every fallback, with verification-on TLS, ServerName equal to the host
// and a non-Unix network address.
func generatedProductionPostgresCorpus() []string {
	authorities := []string{
		"database.example",
		"database.example:5432",
		"database.example:1",
		"database.example:65535",
		"db-1.example",
		"db_1.example",
		"1.2.3.4",
		"1.2.3.4:5432",
		"[2001:db8::1]",
		"[2001:db8::1]:5432",
		"[::1]",
		"[::1]:5432",
		"user@database.example",
		"user:pw@database.example",
		"user:pw@database.example:5432",
		"user@database.example:65535",
		"u.ser:p~w@database.example",
		"database.example:0",
		"database.example:65536",
		"database.example:abc",
		"database.example:-1",
		"database.example:+5",
		"[2001:db8::1]x",
		"[::1]x",
		"[::1]:0",
		"",
		",database.example",
		"database.example,",
		"user%40x@database.example",
		"user:p%3Aw@database.example",
		"@database.example",
		"user@",
		"database.example:",
	}
	paths := []string{
		"/kodama",
		"/",
		"",
		"/kodama/extra",
		"/%6Bodama",
		"/kodama-x.y_z",
		"/KoDaMa",
	}
	queries := []string{
		"sslmode=verify-full",
		"sslmode=verify%2Dfull",
		"connect_timeout=10&sslmode=verify-full",
		"sslmode=verify-full&sslrootcert=system",
		"application_name=kodama_runtime&sslmode=verify-full",
		"sslmode=verify-full&connect_timeout=0",
		"sslmode=disable",
		"sslmode=",
		"sslmode=verify-full&ssl=true",
		"sslmode=verify-full&host=/var/run/postgresql",
		"sslmode=verify-full&service=kodama",
		"sslmode=verify-full&hostaddr=10.0.0.1",
		"connect_timeout=ten&sslmode=verify-full",
		"connect_timeout=-1&sslmode=verify-full",
		"connect_timeout=&sslmode=verify-full",
		"application_name=a=b&sslmode=verify-full",
		"application_name=%20&sslmode=verify-full",
		"application_name=a%2&sslmode=verify-full",
		"sslrootcert=/a#b&sslmode=verify-full",
		"SSLMODE=verify-full",
		"%73slmode=verify-full",
		"sslmode=verify-full&sslmode=verify-full",
		"sslmode=verify-full&x",
		"sslmode=verify-full&",
		"",
	}

	corpus := make([]string, 0, len(authorities)*len(paths)*len(queries))
	for _, authority := range authorities {
		for _, path := range paths {
			for _, query := range queries {
				corpus = append(corpus, "postgres://"+authority+path+"?"+query)
			}
		}
	}
	source := rand.New(rand.NewSource(generatedProductionPostgresCorpusSeed))
	source.Shuffle(len(corpus), func(first, second int) {
		corpus[first], corpus[second] = corpus[second], corpus[first]
	})
	return corpus
}

// TestProductionPostgresValidatorDriverDifferential is the drift guard between
// the strict production validator and pgx's own DSN parser.
func TestProductionPostgresValidatorDriverDifferential(t *testing.T) {
	// pgconn merges PG* environment settings; clear them so the differential
	// depends only on the DSN. An explicitly empty value is ignored for every
	// settings name except sslcertmode, which this test never sets.
	for _, name := range []string{
		"PGHOST", "PGPORT", "PGDATABASE", "PGUSER", "PGPASSWORD", "PGPASSFILE",
		"PGAPPNAME", "PGCONNECT_TIMEOUT", "PGSSLMODE", "PGSSLKEY", "PGSSLCERT",
		"PGSSLSNI", "PGSSLROOTCERT", "PGSSLPASSWORD", "PGSSLNEGOTIATION",
		"PGTARGETSESSIONATTRS", "PGSERVICE", "PGSERVICEFILE", "PGTZ", "PGOPTIONS",
		"PGMINPROTOCOLVERSION", "PGMAXPROTOCOLVERSION", "PGCHANNELBINDING",
		"PGREQUIREAUTH",
	} {
		t.Setenv(name, "")
	}

	corpus := productionPostgresDifferentialCorpus()
	if len(corpus) != productionPostgresDifferentialCorpusSize {
		t.Fatalf("fixed differential corpus has %d entries; want %d", len(corpus), productionPostgresDifferentialCorpusSize)
	}
	generated := generatedProductionPostgresCorpus()
	if len(generated) < 5000 {
		t.Fatalf("generated differential corpus has %d entries; want at least 5000", len(generated))
	}
	corpus = append(corpus, generated...)
	accepted := 0
	for index, dsn := range corpus {
		if err := installation.ValidateProductionPostgresURL(dsn); err != nil {
			// The validator is allowed to be stricter than pgx; a refused DSN
			// claims nothing about the driver.
			continue
		}
		accepted++
		config, err := pgconn.ParseConfig(dsn)
		if err != nil {
			// Never echo the DSN: it can carry a password.
			t.Errorf("corpus entry %d: the production validator accepts a DSN that pgconn.ParseConfig refuses: %v", index, err)
			continue
		}
		assertDifferentialVerifiedTLS(t, index, "primary", config.Host, config.TLSConfig)
		if network, _ := pgconn.NetworkAddress(config.Host, config.Port); network == "unix" {
			t.Errorf("corpus entry %d: primary host resolves to a Unix socket", index)
		}
		for fallbackIndex, fallback := range config.Fallbacks {
			assertDifferentialVerifiedTLS(t, index, "fallback", fallback.Host, fallback.TLSConfig)
			if network, _ := pgconn.NetworkAddress(fallback.Host, fallback.Port); network == "unix" {
				t.Errorf("corpus entry %d: fallback %d resolves to a Unix socket", index, fallbackIndex)
			}
		}
	}
	if accepted < 10 {
		t.Fatalf("differential corpus exercised %d accepted DSNs; want a non-trivial accepted set", accepted)
	}
	t.Logf("differential corpus: %d fixed + %d generated DSNs, %d accepted by the production validator", productionPostgresDifferentialCorpusSize, len(generated), accepted)
}

// assertDifferentialVerifiedTLS requires verification-on TLS with the expected
// server name.
func assertDifferentialVerifiedTLS(t *testing.T, index int, role, host string, config *tls.Config) {
	t.Helper()
	if config == nil {
		t.Errorf("corpus entry %d: %s host has no TLS config; the validator accepted an unverified transport", index, role)
		return
	}
	if config.InsecureSkipVerify {
		t.Errorf("corpus entry %d: %s host has InsecureSkipVerify set", index, role)
	}
	if config.ServerName != host {
		t.Errorf("corpus entry %d: %s TLS ServerName = %q; want host %q", index, role, config.ServerName, host)
	}
}
