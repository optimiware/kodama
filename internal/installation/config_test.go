package installation

// Configuration parsing and preflight tests. Every document is an isolated
// in-memory reader: parsing never opens a file, environment variable, socket,
// database or cloud client, and no adapter is auto-detected.

import (
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
)

// docFields renders one explicit installation document. Empty fields are
// omitted so a test can prove a missing setting is refused rather than
// defaulted.
type docFields struct {
	environment string
	state       string
	messaging   string
	runtime     string
	keys        string
	databaseURL string
	natsURL     string
	publicURL   string
	keyRef      string
	optional    string
}

func (fields docFields) String() string {
	lines := []string{"# explicit installation document"}
	add := func(key, value string) {
		if value != "" {
			lines = append(lines, key+" = "+value)
		}
	}
	add("environment", fields.environment)
	add("state", fields.state)
	add("messaging", fields.messaging)
	add("runtime", fields.runtime)
	add("keys", fields.keys)
	add("database_url", fields.databaseURL)
	add("nats_url", fields.natsURL)
	add("public_url", fields.publicURL)
	add("key_ref", fields.keyRef)
	add("optional", fields.optional)
	return strings.Join(lines, "\n") + "\n"
}

// developmentDoc is the declared development composition.
func developmentDoc() docFields {
	return docFields{
		environment: "development",
		state:       "memory",
		messaging:   "memory",
		runtime:     "process",
		keys:        "ephemeral",
	}
}

// testDoc is the declared test composition with complete settings.
func testDoc() docFields {
	return docFields{
		environment: "test",
		state:       "postgres",
		messaging:   "nats",
		runtime:     "podman",
		keys:        "keyfile",
		databaseURL: "postgres://kodama@database:5432/kodama",
		natsURL:     "nats://nats:4222",
		publicURL:   "https://kodama.test",
		keyRef:      "keyfile:kodama",
	}
}

// productionDoc is the declared production composition with complete settings.
func productionDoc(keys, keyRef string) docFields {
	return docFields{
		environment: "production",
		state:       "postgres",
		messaging:   "nats",
		runtime:     "kubernetes",
		keys:        keys,
		databaseURL: "postgres://kodama@database:5432/kodama?sslmode=verify-full",
		natsURL:     "tls://nats:4222",
		publicURL:   "https://kodama.example",
		keyRef:      keyRef,
	}
}

func mustParse(t *testing.T, fields docFields) Config {
	t.Helper()
	config, err := ParseConfig(strings.NewReader(fields.String()))
	if err != nil {
		t.Fatalf("ParseConfig refused %q: %v", fields.String(), err)
	}
	return config
}

// assertFaultClass fails unless err carries want as its fault class.
func assertFaultClass(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s fault, got nil", want)
	}
	class, ok := fault.ClassOf(err)
	if !ok {
		t.Fatalf("expected %s fault, got non-fault error %v", want, err)
	}
	if class != want {
		t.Fatalf("fault class = %s, want %s (%v)", class, want, err)
	}
}

func TestParseConfigDevelopment(t *testing.T) {
	config := mustParse(t, developmentDoc())
	if config.Environment != EnvironmentDevelopment {
		t.Errorf("environment = %q", config.Environment)
	}
	if config.State != StateMemory || config.Messaging != MessagingMemory {
		t.Errorf("state/messaging = %q/%q", config.State, config.Messaging)
	}
	if config.Runtime != RuntimeProcess || config.Keys != KeyEphemeral {
		t.Errorf("runtime/keys = %q/%q", config.Runtime, config.Keys)
	}
	if config.KeyRef != "" {
		t.Errorf("development invented key reference %q", config.KeyRef)
	}
	if len(config.Optional) != 0 {
		t.Errorf("optional capabilities = %v, want none", config.Optional)
	}
	if err := config.Preflight(); err != nil {
		t.Fatalf("Preflight refused the parsed development config: %v", err)
	}
}

func TestParseConfigTestComposition(t *testing.T) {
	fields := testDoc()
	config := mustParse(t, fields)
	if config.Environment != EnvironmentTest {
		t.Errorf("environment = %q", config.Environment)
	}
	if config.State != StatePostgres || config.Messaging != MessagingNATS {
		t.Errorf("state/messaging = %q/%q", config.State, config.Messaging)
	}
	if config.Runtime != RuntimePodman || config.Keys != KeyKeyfile {
		t.Errorf("runtime/keys = %q/%q", config.Runtime, config.Keys)
	}
	if config.DatabaseURL != fields.databaseURL || config.NATSURL != fields.natsURL {
		t.Errorf("endpoints = %q/%q", config.DatabaseURL, config.NATSURL)
	}
	if config.PublicURL != fields.publicURL || config.KeyRef != fields.keyRef {
		t.Errorf("public/key = %q/%q", config.PublicURL, config.KeyRef)
	}
}

func TestParseConfigProductionCloudKMS(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		keys   string
		keyRef string
	}{
		{name: "aws", keys: "aws-kms", keyRef: "aws-kms:alias/kodama"},
		{name: "gcp", keys: "gcp-kms", keyRef: "gcp-kms:projects/kodama/locations/global/keyRings/kodama/cryptoKeys/kek"},
		{name: "scaleway", keys: "scw-kms", keyRef: "scw-kms:fr-par/kodama"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			config := mustParse(t, productionDoc(testCase.keys, testCase.keyRef))
			if config.Environment != EnvironmentProduction {
				t.Errorf("environment = %q", config.Environment)
			}
			if config.Keys != KeyAdapter(testCase.keys) {
				t.Errorf("keys = %q", config.Keys)
			}
			if config.KeyRef != testCase.keyRef {
				t.Errorf("key reference = %q", config.KeyRef)
			}
		})
	}
}

func TestParseConfigExplicitOptionalCapabilities(t *testing.T) {
	fields := developmentDoc()
	fields.optional = "git, agents"
	config := mustParse(t, fields)
	if !config.Optional[OptionalGit] {
		t.Error("git capability was not enabled")
	}
	if !config.Optional[OptionalAgents] {
		t.Error("agents capability was not enabled")
	}
	if config.Optional[OptionalGateways] {
		t.Error("gateways capability was enabled without being declared")
	}
	if config.Optional[OptionalServices] {
		t.Error("services capability was enabled without being declared")
	}

	want := []OptionalCapability{
		OptionalGit,
		OptionalGateways,
		OptionalAgents,
		OptionalServices,
	}
	got := OptionalCapabilities()
	if len(got) != len(want) {
		t.Fatalf("OptionalCapabilities() = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("OptionalCapabilities() = %v, want %v", got, want)
		}
	}
}

func TestParseConfigRefusesMalformedDocuments(t *testing.T) {
	base := developmentDoc().String()
	for _, testCase := range []struct {
		name string
		body string
	}{
		{name: "unknown key", body: base + "state_file = /var/lib/kodama\n"},
		{name: "duplicate key", body: base + "state = memory\n"},
		{name: "missing separator", body: base + "public_url https://kodama.example\n"},
		{name: "empty value", body: base + "database_url =\n"},
		{name: "empty key", body: base + " = value\n"},
		{name: "unknown environment", body: strings.Replace(base, "environment = development", "environment = staging", 1)},
		{name: "missing environment", body: strings.Replace(base, "environment = development\n", "", 1)},
		{name: "missing state", body: strings.Replace(base, "state = memory\n", "", 1)},
		{name: "missing messaging", body: strings.Replace(base, "messaging = memory\n", "", 1)},
		{name: "missing runtime", body: strings.Replace(base, "runtime = process\n", "", 1)},
		{name: "missing keys", body: strings.Replace(base, "keys = ephemeral\n", "", 1)},
		{name: "unknown optional", body: base + "optional = telepathy\n"},
		{name: "duplicate optional", body: base + "optional = git,git\n"},
		{name: "empty optional item", body: base + "optional = git,,agents\n"},
		{name: "blank document", body: "\n# only a comment\n"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			config, err := ParseConfig(strings.NewReader(testCase.body))
			if err == nil {
				t.Fatalf("ParseConfig accepted %q as %+v", testCase.body, config)
			}
			assertFaultClass(t, err, fault.InvalidInput)
		})
	}
}

func TestParseConfigNeverAutodetects(t *testing.T) {
	// A document that names only the environment must not infer adapters,
	// endpoints or a key reference.
	_, err := ParseConfig(strings.NewReader("environment = production\n"))
	assertFaultClass(t, err, fault.InvalidInput)

	// A document that names only adapters must not infer the environment.
	body := "state = postgres\nmessaging = nats\nruntime = kubernetes\nkeys = aws-kms\n"
	_, err = ParseConfig(strings.NewReader(body))
	assertFaultClass(t, err, fault.InvalidInput)
}

func TestParseConfigRefusesProductionTestAdapters(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*docFields)
	}{
		{name: "memory state", mutate: func(fields *docFields) { fields.state = "memory" }},
		{name: "memory messaging", mutate: func(fields *docFields) { fields.messaging = "memory" }},
		{name: "process runtime", mutate: func(fields *docFields) { fields.runtime = "process" }},
		{name: "podman runtime", mutate: func(fields *docFields) { fields.runtime = "podman" }},
		{name: "ephemeral keys", mutate: func(fields *docFields) {
			fields.keys = "ephemeral"
			fields.keyRef = "ephemeral:dev"
		}},
		{name: "keyfile keys", mutate: func(fields *docFields) {
			fields.keys = "keyfile"
			fields.keyRef = "keyfile:kodama"
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fields := productionDoc("aws-kms", "aws-kms:alias/kodama")
			testCase.mutate(&fields)
			_, err := ParseConfig(strings.NewReader(fields.String()))
			assertFaultClass(t, err, fault.InvalidInput)
		})
	}
}

func TestParseConfigRefusesIncompleteSettings(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		fields docFields
	}{
		{name: "test missing database", fields: func() docFields {
			fields := testDoc()
			fields.databaseURL = ""
			return fields
		}()},
		{name: "test missing nats", fields: func() docFields {
			fields := testDoc()
			fields.natsURL = ""
			return fields
		}()},
		{name: "test missing public url", fields: func() docFields {
			fields := testDoc()
			fields.publicURL = ""
			return fields
		}()},
		{name: "test missing key reference", fields: func() docFields {
			fields := testDoc()
			fields.keyRef = ""
			return fields
		}()},
		{name: "production missing database", fields: func() docFields {
			fields := productionDoc("aws-kms", "aws-kms:alias/kodama")
			fields.databaseURL = ""
			return fields
		}()},
		{name: "production missing nats", fields: func() docFields {
			fields := productionDoc("aws-kms", "aws-kms:alias/kodama")
			fields.natsURL = ""
			return fields
		}()},
		{name: "production missing public url", fields: func() docFields {
			fields := productionDoc("aws-kms", "aws-kms:alias/kodama")
			fields.publicURL = ""
			return fields
		}()},
		{name: "insecure public url", fields: func() docFields {
			fields := testDoc()
			fields.publicURL = "http://kodama.test"
			return fields
		}()},
		{name: "bare postgres scheme", fields: func() docFields {
			fields := testDoc()
			fields.databaseURL = "postgres://"
			return fields
		}()},
		{name: "unexpected nats scheme", fields: func() docFields {
			fields := testDoc()
			fields.natsURL = "http://nats:4222"
			return fields
		}()},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := ParseConfig(strings.NewReader(testCase.fields.String()))
			assertFaultClass(t, err, fault.InvalidInput)
		})
	}
}

func TestParseConfigKeyReferenceFamilies(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		fields docFields
	}{
		{name: "development ephemeral", fields: func() docFields {
			fields := developmentDoc()
			fields.keyRef = "ephemeral:dev"
			return fields
		}()},
		{name: "test keyfile", fields: testDoc()},
		{name: "production aws", fields: productionDoc("aws-kms", "aws-kms:alias/kodama")},
		{name: "production gcp", fields: productionDoc("gcp-kms", "gcp-kms:projects/kodama/locations/global/keyRings/kodama/cryptoKeys/kek")},
		{name: "production scaleway", fields: productionDoc("scw-kms", "scw-kms:fr-par/kodama")},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			mustParse(t, testCase.fields)
		})
	}

	for _, testCase := range []struct {
		name   string
		fields docFields
	}{
		{name: "development keyfile reference", fields: func() docFields {
			fields := developmentDoc()
			fields.keyRef = "keyfile:kodama"
			return fields
		}()},
		{name: "development cloud reference", fields: func() docFields {
			fields := developmentDoc()
			fields.keyRef = "aws-kms:alias/kodama"
			return fields
		}()},
		{name: "test ephemeral reference", fields: func() docFields {
			fields := testDoc()
			fields.keyRef = "ephemeral:test"
			return fields
		}()},
		{name: "test cloud reference", fields: func() docFields {
			fields := testDoc()
			fields.keyRef = "gcp-kms:projects/kodama/locations/global/keyRings/kodama/cryptoKeys/kek"
			return fields
		}()},
		{name: "production aws names gcp", fields: productionDoc("aws-kms", "gcp-kms:projects/kodama/locations/global/keyRings/kodama/cryptoKeys/kek")},
		{name: "production gcp names aws", fields: productionDoc("gcp-kms", "aws-kms:alias/kodama")},
		{name: "production scaleway names gcp", fields: productionDoc("scw-kms", "gcp-kms:projects/kodama/locations/global/keyRings/kodama/cryptoKeys/kek")},
		{name: "production empty reference", fields: productionDoc("aws-kms", "")},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := ParseConfig(strings.NewReader(testCase.fields.String()))
			assertFaultClass(t, err, fault.InvalidInput)
		})
	}
}

func TestParseConfigKeyReferencePrefixes(t *testing.T) {
	for _, testCase := range []struct {
		name string
		got  string
		want string
	}{
		{name: "ephemeral", got: KeyRefEphemeralPrefix, want: "ephemeral:"},
		{name: "keyfile", got: KeyRefKeyfilePrefix, want: "keyfile:"},
		{name: "aws", got: KeyRefAWSKMSPrefix, want: "aws-kms:"},
		{name: "gcp", got: KeyRefGCPKMSPrefix, want: "gcp-kms:"},
		{name: "scaleway", got: KeyRefScalewayKMSPrefix, want: "scw-kms:"},
	} {
		if testCase.got != testCase.want {
			t.Errorf("%s prefix = %q, want %q", testCase.name, testCase.got, testCase.want)
		}
	}
}

func TestParseConfigReturnsReaderError(t *testing.T) {
	sentinel := errors.New("reader failed")
	_, err := ParseConfig(failingReader{err: sentinel})
	if !errors.Is(err, sentinel) {
		t.Fatalf("ParseConfig error = %v, want the reader error", err)
	}
}

// failingReader fails on the first read.
type failingReader struct {
	err error
}

func (reader failingReader) Read([]byte) (int, error) {
	return 0, reader.err
}

var _ io.Reader = failingReader{}

func TestParseConfigAcceptsCRLFAndComments(t *testing.T) {
	body := "# comment\r\n\r\nenvironment = development\r\nstate = memory\r\n" +
		"messaging = memory\r\nruntime = process\r\nkeys = ephemeral\r\n"
	config, err := ParseConfig(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ParseConfig refused CRLF and comments: %v", err)
	}
	if config.Environment != EnvironmentDevelopment {
		t.Fatalf("environment = %q", config.Environment)
	}
}

// TestParseConfigMissingSeparatorReportsLineWithoutValue proves a malformed
// line is reported by line number and declared key, never by its text.
func TestParseConfigMissingSeparatorReportsLineWithoutValue(t *testing.T) {
	base := developmentDoc().String()
	body := base + "database_url postgres://kodama@database:5432/kodama\n"
	_, err := ParseConfig(strings.NewReader(body))
	assertFaultClass(t, err, fault.InvalidInput)
	message := err.Error()
	if !strings.Contains(message, "line "+strconv.Itoa(strings.Count(base, "\n")+1)) {
		t.Errorf("error does not name the offending line: %v", err)
	}
	if !strings.Contains(message, `"database_url"`) {
		t.Errorf("error does not name the declared key: %v", err)
	}
	if strings.Contains(message, "postgres://") {
		t.Errorf("error echoes the line value: %v", err)
	}
}

// TestParseConfigErrorsOmitValues proves no ParseConfig error echoes a value
// that could be a credential.
func TestParseConfigErrorsOmitValues(t *testing.T) {
	const credential = "hunter2-do-not-leak"

	keyRefFields := testDoc()
	keyRefFields.keyRef = "aws-kms:" + credential

	for _, testCase := range []struct {
		name string
		body string
	}{
		{
			name: "missing separator",
			body: developmentDoc().String() + "database_url postgres://kodama:" + credential + "@database:5432/kodama\n",
		},
		{
			name: "unknown optional capability",
			body: developmentDoc().String() + "optional = " + credential + "\n",
		},
		{
			name: "unknown environment",
			body: strings.Replace(developmentDoc().String(), "environment = development", "environment = "+credential, 1),
		},
		{
			name: "key reference family",
			body: keyRefFields.String(),
		},
		{
			// A mistyped `key: value` line whose URL carries an `=` puts the
			// credential in the key position of the first separator.
			name: "mistyped key with embedded credential",
			body: developmentDoc().String() + "database_url: postgres://u:" + credential + "@h/db?sslmode=require\n",
		},
		{
			// The same mistyped line ending in `=` reaches the empty-value
			// path with the credential still in the key position.
			name: "unknown key with empty value carrying credential",
			body: developmentDoc().String() + "database_url: postgres://u:" + credential + "@h/db=\n",
		},
		{
			// A repeated mistyped line must not reach the duplicate path with
			// an unvalidated key either.
			name: "duplicate unknown key carrying credential",
			body: developmentDoc().String() +
				"database_url: postgres://u:" + credential + "@h/db?sslmode=require\n" +
				"database_url: postgres://u:" + credential + "@h/db?sslmode=require\n",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := ParseConfig(strings.NewReader(testCase.body))
			assertFaultClass(t, err, fault.InvalidInput)
			if strings.Contains(err.Error(), credential) {
				t.Fatalf("error echoes the configured value: %v", err)
			}
		})
	}
}
