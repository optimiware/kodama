package publiccontract

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/userland/cli"
)

// TestCLIConfigInputs pins the kodactl configuration inputs: a trusted server
// URL, an optional session file, an output format and registered trusted Git
// origins. Insecure, ambiguous and unknown configuration is refused.
func TestCLIConfigInputs(t *testing.T) {
	valid := cli.Config{
		ServerURL:      "https://kodama.example.com",
		CredentialFile: "/home/engineer/.kodama/session.json",
		Output:         cli.OutputJSON,
		TrustedOrigins: []string{"https://git.example.com/shop", "https://git.example.com:8443/other"},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid config refused: %v", err)
	}

	body, err := json.Marshal(valid)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	for _, key := range []string{"server_url", "credential_file", "output", "trusted_origins"} {
		if _, ok := keys[key]; !ok {
			t.Errorf("config JSON is missing %q: %s", key, body)
		}
	}
	parsed, err := cli.ParseConfig(body)
	if err != nil {
		t.Fatalf("ParseConfig(round trip): %v", err)
	}
	if parsed.ServerURL != valid.ServerURL || parsed.Output != valid.Output || len(parsed.TrustedOrigins) != 2 {
		t.Fatalf("parsed config = %+v; want %+v", parsed, valid)
	}

	// A loopback development server may use plain HTTP; anything else may not.
	if err := (cli.Config{ServerURL: "http://127.0.0.1:8080"}).Validate(); err != nil {
		t.Errorf("loopback HTTP config refused: %v", err)
	}
	if err := (cli.Config{ServerURL: "http://localhost:8080"}).Validate(); err != nil {
		t.Errorf("localhost HTTP config refused: %v", err)
	}
	for _, invalid := range []cli.Config{
		{},
		{ServerURL: "http://kodama.example.com"},
		{ServerURL: "https://user:pass@kodama.example.com"},
		{ServerURL: "https://kodama.example.com/#fragment"},
		{ServerURL: "https://kodama.example.com/?query=1"},
		{ServerURL: "kodama.example.com"},
		{ServerURL: "https://kodama.example.com", Output: "yaml"},
		{ServerURL: "https://kodama.example.com", CredentialFile: "relative/session.json"},
		{ServerURL: "https://kodama.example.com", TrustedOrigins: []string{"http://git.example.com/shop"}},
		{ServerURL: "https://kodama.example.com", TrustedOrigins: []string{"https://user@git.example.com/shop"}},
		{ServerURL: "https://kodama.example.com", TrustedOrigins: []string{"git.example.com/shop"}},
	} {
		if err := invalid.Validate(); err == nil {
			t.Errorf("Config.Validate accepted %+v", invalid)
		}
	}
	if !cli.OutputTable.Valid() || !cli.OutputJSON.Valid() {
		t.Error("table/json output formats are not valid")
	}
	if cli.OutputFormat("yaml").Valid() {
		t.Error("OutputFormat(yaml).Valid() = true")
	}

	for _, body := range []string{
		`{"server_url":"https://kodama.example.com","extra":1}`,
		`{"server_url":"https://kodama.example.com","trusted_origins":"https://git.example.com"}`,
		`{"server_url":"https://kodama.example.com","output":"yaml"}`,
		`not json`,
		`[]`,
	} {
		if _, err := cli.ParseConfig([]byte(body)); err == nil {
			t.Errorf("ParseConfig accepted %s", body)
		}
	}
}

// TestTrustedOriginMatching pins the Git credential-helper trust boundary:
// exact scheme/host, path-prefix matching only at a segment boundary, no
// userinfo, no insecure scheme and no unregistered target.
func TestTrustedOriginMatching(t *testing.T) {
	origin, err := cli.ParseOrigin("https://git.example.com/shop")
	if err != nil {
		t.Fatalf("ParseOrigin: %v", err)
	}
	if origin.Scheme != "https" || origin.Host != "git.example.com" || origin.Path != "/shop" {
		t.Fatalf("origin = %+v", origin)
	}
	if origin.String() != "https://git.example.com/shop" {
		t.Fatalf("origin String() = %q", origin.String())
	}

	cases := []struct {
		target string
		want   bool
	}{
		{"https://git.example.com/shop", true},
		{"https://git.example.com/shop/", true},
		{"https://git.example.com/shop/repo.git", true},
		{"https://git.example.com/shop/team/repo.git", true},
		{"https://git.example.com/shopkeeper", false},
		{"https://git.example.com/shopping/repo.git", false},
		{"https://git.example.com/other/repo.git", false},
		{"https://git.example.com", false},
		{"http://git.example.com/shop/repo.git", false},
		{"https://user@git.example.com/shop/repo.git", false},
		{"https://git.example.com.evil/shop/repo.git", false},
		{"https://git.example.com:8443/shop/repo.git", false},
		{"https://git.example.com/shop/repo.git?x=1", false},
		{"https://git.example.com/shop/../secret", false},
		{"ssh://git.example.com/shop", false},
		{"git@git.example.com:shop/repo.git", false},
	}
	for _, testCase := range cases {
		if got := origin.Matches(testCase.target); got != testCase.want {
			t.Errorf("Matches(%q) = %v; want %v", testCase.target, got, testCase.want)
		}
	}

	withPort, err := cli.ParseOrigin("https://git.example.com:8443/shop/")
	if err != nil {
		t.Fatalf("ParseOrigin(with port): %v", err)
	}
	if withPort.Host != "git.example.com:8443" || withPort.Path != "/shop" {
		t.Fatalf("origin with port = %+v", withPort)
	}
	if !withPort.Matches("https://git.example.com:8443/shop/repo.git") {
		t.Error("origin with port did not match its own port")
	}
	if withPort.Matches("https://git.example.com/shop/repo.git") {
		t.Error("origin with port matched the default port")
	}

	root, err := cli.ParseOrigin("https://git.example.com")
	if err != nil {
		t.Fatalf("ParseOrigin(root): %v", err)
	}
	if !root.Matches("https://git.example.com/any/repo.git") {
		t.Error("root origin did not match an arbitrary path")
	}

	for _, invalid := range []string{
		"",
		"http://git.example.com/shop",
		"https://user:pass@git.example.com/shop",
		"https://git.example.com/shop?x=1",
		"https://git.example.com/shop#frag",
		"https://git.example.com/shop/../secret",
		"https://git.example.com//shop",
		"ssh://git.example.com/shop",
		"git@git.example.com:shop/repo.git",
		"/shop",
	} {
		if parsed, err := cli.ParseOrigin(invalid); err == nil {
			t.Errorf("ParseOrigin(%q) = %+v; want refusal", invalid, parsed)
		}
	}
}

// TestHelperDecision pins the credential helper: username kodama plus the
// current token only for a registered trusted origin, never for store/erase,
// unregistered targets, insecure schemes or expired credentials.
func TestHelperDecision(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	config := cli.Config{
		ServerURL:      "https://kodama.example.com",
		TrustedOrigins: []string{"https://git.example.com/shop"},
	}
	credential := client.Credential{
		Kind:      client.KindExecution,
		Token:     "kodex.exec-1.signature",
		ExpiresAt: now.Add(time.Hour),
	}

	response, err := cli.HelperDecision(config, cli.HelperRequest{
		Protocol: "get",
		URL:      "https://git.example.com/shop/repo.git",
	}, credential, now)
	if err != nil {
		t.Fatalf("HelperDecision(get): %v", err)
	}
	if response.Username != "kodama" || response.Password != credential.Token {
		t.Fatalf("helper response = %+v; want username kodama and the current token", response)
	}
	if rendered := response.String(); strings.Contains(rendered, credential.Token) {
		t.Fatalf("HelperResponse.String() leaks the token: %s", rendered)
	}

	emptyCases := []struct {
		name       string
		request    cli.HelperRequest
		credential client.Credential
		at         time.Time
	}{
		{"unregistered target", cli.HelperRequest{Protocol: "get", URL: "https://git.example.com/other/repo.git"}, credential, now},
		{"path confusion", cli.HelperRequest{Protocol: "get", URL: "https://git.example.com/shopkeeper"}, credential, now},
		{"insecure target", cli.HelperRequest{Protocol: "get", URL: "http://git.example.com/shop/repo.git"}, credential, now},
		{"userinfo target", cli.HelperRequest{Protocol: "get", URL: "https://user@git.example.com/shop/repo.git"}, credential, now},
		{"unregistered host", cli.HelperRequest{Protocol: "get", URL: "https://git.example.com.evil/shop/repo.git"}, credential, now},
		{"store", cli.HelperRequest{Protocol: "store", URL: "https://git.example.com/shop/repo.git"}, credential, now},
		{"erase", cli.HelperRequest{Protocol: "erase", URL: "https://git.example.com/shop/repo.git"}, credential, now},
		{"expired credential", cli.HelperRequest{Protocol: "get", URL: "https://git.example.com/shop/repo.git"},
			client.Credential{Kind: client.KindExecution, Token: "kodex.exec-1.old", ExpiresAt: now.Add(-time.Second)}, now},
	}
	for _, testCase := range emptyCases {
		got, err := cli.HelperDecision(config, testCase.request, testCase.credential, testCase.at)
		if err != nil {
			t.Errorf("%s: HelperDecision error: %v", testCase.name, err)
			continue
		}
		if got.Username != "" || got.Password != "" {
			t.Errorf("%s: helper response = %+v; want an empty refusal", testCase.name, got)
		}
	}

	for _, request := range []cli.HelperRequest{
		{Protocol: "get", URL: ""},
		{Protocol: "foo", URL: "https://git.example.com/shop/repo.git"},
		{Protocol: "get", URL: "not a url"},
		{Protocol: "get", URL: "https://git.example.com/shop/repo.git%zz"},
	} {
		if _, err := cli.HelperDecision(config, request, credential, now); err == nil {
			t.Errorf("HelperDecision accepted %+v", request)
		}
	}

	// A human session and a mounted execution token are separate sources; a
	// service credential is not a helper session. A human session must carry
	// an expiry; only a mounted execution session may omit it.
	for _, source := range []client.CredentialKind{client.KindSession, client.KindExecution} {
		session := cli.Session{Source: source, TokenPath: "/run/kodama/token", ExpiresAt: now.Add(time.Hour)}
		if err := session.Validate(); err != nil {
			t.Errorf("valid session source %q refused: %v", source, err)
		}
	}
	if err := (cli.Session{Source: client.KindService, TokenPath: "/run/kodama/token", ExpiresAt: now}).Validate(); err == nil {
		t.Error("Session.Validate accepted a service credential as a helper session")
	}
	if err := (cli.Session{Source: client.KindExecution, TokenPath: "token", ExpiresAt: now}).Validate(); err == nil {
		t.Error("Session.Validate accepted a relative mounted token path")
	}
	if err := (cli.Session{Source: client.KindSession, TokenPath: "/tmp/session.json"}).Validate(); err == nil {
		t.Error("Session.Validate accepted a human session with a zero expiry")
	}
}
