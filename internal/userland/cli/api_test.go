// API note — kodactl declarations.
//
// The tests assume package cli declares the following (types.go,
// contract_config.go, run.go, session.go); this note records the API the
// tests pin and is not a second implementation. FileSystem.Remove,
// RunWithDeps and the logout, refresh and status commands are not repeated
// here.
//
//	const (
//	    ExitSuccess = 0
//	    ExitFailure = 1
//	    ExitUsage   = 2
//	)
//
//	type DirEntry struct {
//	    Name  string
//	    IsDir bool
//	}
//
//	type FileSystem interface {
//	    ReadFile(name string) ([]byte, error)
//	    WriteFile(name string, data []byte, perm fs.FileMode) error
//	    ReplaceFile(name string, data []byte, perm fs.FileMode) error
//	    ReadDir(name string) ([]DirEntry, error)
//	}
//
//	type SessionStore interface {
//	    Load(ctx context.Context) (client.Credential, error)
//	    Save(ctx context.Context, credential client.Credential) error
//	}
//
//	type FileSessionStore struct {
//	    Path  string
//	    Files FileSystem
//	    Now   func() time.Time
//	}
//	func (store FileSessionStore) Load(ctx context.Context) (client.Credential, error)
//	func (store FileSessionStore) Save(ctx context.Context, credential client.Credential) error
//
// The durable session file is JSON {"kind":"session","token":<token>,
// "expires_at":<RFC3339 time>}. Save requires a human KindSession with a
// non-zero, current expiry and writes the file with mode 0600. Load reads the
// file on every call, requires the same shape and a current expiry, and
// refuses a missing, malformed, non-session, empty-token, oversized-token or
// expired file with the stable unauthenticated class.
//
//	type Ports struct {
//	    Client     client.Transport
//	    Files      FileSystem
//	    Session    SessionStore
//	    Credential client.CredentialSource
//	    Out        io.Writer
//	    Err        io.Writer
//	    Now        func() time.Time
//	    Sleep      func(time.Duration)
//	}
//	func Run(ctx context.Context, config Config, args []string, ports Ports) int
//	func (session Session) Usable(now time.Time) bool
//
// Further declarations:
//   - Config carries Project string `json:"project,omitempty"`: the project used
//     by `pipeline apply`. An empty project stays valid for other commands.
//   - Session.Validate allows a zero ExpiresAt only for KindExecution (an
//     opaque mounted credential whose expiry is unknown); a session credential
//     still requires a non-zero expiry and a service source is still not a
//     helper session. Session.Usable(now) is true for the execution unknown
//     expiry, false for any explicit expiry <= now, and false for a
//     session/service credential with no expiry.
//   - HelperDecision returns an empty response for a KindService credential:
//     service automation never receives the Git helper password.
//
// Grammar (arguments after the program name):
//
//	call <operation> [--input=<json>] [--expected-version=<n>] [--idempotency-key=<k>]
//	inspect <execution>
//	watch <execution>
//	login
//	pipeline apply <directory>
//	helper <get|store|erase> <url>
//
// Run never calls os.Exit. It returns ExitSuccess, ExitFailure or ExitUsage.
// Invalid command syntax (including unknown flags or extra arguments), an
// invalid config (except for helper, which only needs trusted origins), an
// invalid invocation, or a missing project for pipeline apply is ExitUsage. A
// client, credential, persistence or file failure is ExitFailure. A missing
// Ports.Client, Ports.Credential or Ports.Session needed by a command is a
// failure, never a panic.
//
// JSON output writes the result Body bytes exactly, or "{}\n" when absent,
// followed by a newline. Table output writes "operation\t<name>\n" followed by
// one "<key>\t<compact JSON value>\n" line per top-level body field in sorted
// key order. Diagnostics go only to Err and never contain a token.
//
// `watch` polls execution.inspect with the same input, sleeping through Sleep
// between polls. A status of succeeded is ExitSuccess; failed or cancelled is
// ExitFailure after rendering with a diagnostic on Err; queued/running polls
// again; a missing or unknown status is ExitFailure. Context cancellation stops
// the loop with ExitFailure and "watch canceled" on Err.
//
// `login` reads the fresh credential from Ports.Credential, requires
// KindSession and a usable expiry, and persists it through Ports.Session. A
// source, kind, expiry or save failure is ExitFailure and writes nothing to
// Out.
//
// `pipeline apply` enumerates the directory non-recursively, ignores
// directories, dotfiles and names not ending in .yaml/.yml (case-sensitive),
// sorts by file name, and maps "<name>.yaml"/"<name>.yml" to pipeline <name>.
// A .yaml and .yml with the same base name are a duplicate mapping: neither is
// sent and both are visible failures. Each accepted file is sent as
// pipeline.apply with Input {"project_id":Config.Project,"name":<name>,
// "yaml":<exact file bytes as a JSON string>}, no expected version, and a
// stable per-pipeline caller key derived from project, name and exact YAML
// bytes. A file that is not valid UTF-8 is a visible failure and is not sent.
// Every file is attempted in sorted order even after a failure; there is no
// directory atomicity claim. Success writes "applied <name>\n" to Out; failure
// writes "failed <name>: <public message>\n" to Err. Any failure is
// ExitFailure. An empty directory is ExitSuccess with no requests. Replaying
// the same directory sends the same keys.
//
// `helper` reads Ports.Credential only for get and delegates to
// HelperDecision. A successful get writes exactly
// "username=kodama\npassword=<token>\n" to Out; store/erase and every refusal
// (including a failed or empty credential source) write nothing and exit
// ExitSuccess. Invalid syntax is ExitUsage. The token never appears on Err.
package cli_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/userland/cli"
)

// TestConfigProjectInput pins the pipeline-apply project input.
func TestConfigProjectInput(t *testing.T) {
	config := cli.Config{
		ServerURL: "https://kodama.example.com",
		Project:   "shop",
		Output:    cli.OutputJSON,
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("config with project refused: %v", err)
	}
	body, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if _, ok := fields["project"]; !ok {
		t.Fatalf("config JSON is missing project: %s", body)
	}
	parsed, err := cli.ParseConfig(body)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if parsed.Project != "shop" {
		t.Fatalf("parsed project = %q; want shop", parsed.Project)
	}
	if err := (cli.Config{ServerURL: "https://kodama.example.com"}).Validate(); err != nil {
		t.Fatalf("config without project refused: %v", err)
	}
	if _, err := cli.ParseConfig([]byte(`{"server_url":"https://kodama.example.com","project":"shop"}`)); err != nil {
		t.Fatalf("ParseConfig(project): %v", err)
	}
}

// TestSessionUnknownExpiry pins the session expiry decision: only an opaque
// mounted execution session may omit the expiry, and an explicit expiry still
// wins.
func TestSessionUnknownExpiry(t *testing.T) {
	now := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)

	execution := cli.Session{Source: client.KindExecution, TokenPath: "/run/kodama/token"}
	if err := execution.Validate(); err != nil {
		t.Fatalf("unknown-expiry execution session refused: %v", err)
	}
	if !execution.Usable(now) {
		t.Fatal("unknown-expiry execution session is not usable")
	}

	human := cli.Session{Source: client.KindSession, TokenPath: "/home/engineer/.kodama/session.json"}
	if err := human.Validate(); err == nil {
		t.Fatal("human session with unknown expiry was accepted")
	}
	if human.Usable(now) {
		t.Fatal("human session with unknown expiry is usable")
	}

	expired := cli.Session{
		Source:    client.KindExecution,
		TokenPath: "/run/kodama/token",
		ExpiresAt: now.Add(-time.Second),
	}
	if err := expired.Validate(); err != nil {
		t.Fatalf("explicit-expiry execution session refused: %v", err)
	}
	if expired.Usable(now) {
		t.Fatal("explicitly expired execution session is usable")
	}
	atBoundary := expired
	atBoundary.ExpiresAt = now
	if atBoundary.Usable(now) {
		t.Fatal("session expiring exactly now is usable")
	}
	current := expired
	current.ExpiresAt = now.Add(time.Second)
	if !current.Usable(now) {
		t.Fatal("current execution session is not usable")
	}

	service := cli.Session{Source: client.KindService, TokenPath: "/run/kodama/token"}
	if err := service.Validate(); err == nil {
		t.Fatal("service credential was accepted as a helper session")
	}
	if err := (cli.Session{Source: client.KindExecution, TokenPath: "relative/token"}).Validate(); err == nil {
		t.Fatal("relative mounted token path was accepted")
	}
}

// TestHelperDecisionExcludesServiceCredentials pins that service automation is
// never handed the Git helper password.
func TestHelperDecisionExcludesServiceCredentials(t *testing.T) {
	now := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	config := cli.Config{TrustedOrigins: []string{"https://git.example.com/shop"}}
	request := cli.HelperRequest{Protocol: "get", URL: "https://git.example.com/shop/repo.git"}

	service := client.Credential{
		Kind:      client.KindService,
		Token:     "kodex.service-1.signature",
		ExpiresAt: now.Add(time.Hour),
	}
	response, err := cli.HelperDecision(config, request, service, now)
	if err != nil {
		t.Fatalf("HelperDecision(service): %v", err)
	}
	if response.Username != "" || response.Password != "" {
		t.Fatalf("service credential received a helper response: %+v", response)
	}

	for _, credential := range []client.Credential{
		{Kind: client.KindSession, Token: "human-token", ExpiresAt: now.Add(time.Hour)},
		{Kind: client.KindExecution, Token: "kodex.exec-1.opaque"},
	} {
		response, err := cli.HelperDecision(config, request, credential, now)
		if err != nil {
			t.Fatalf("HelperDecision(%s): %v", credential.Kind, err)
		}
		if response.Username != "kodama" || response.Password != credential.Token {
			t.Fatalf("%s helper response = %+v", credential.Kind, response)
		}
	}
	if rendered := service.String(); strings.Contains(rendered, service.Token) {
		t.Fatalf("service credential formatting leaked the token: %s", rendered)
	}
}
