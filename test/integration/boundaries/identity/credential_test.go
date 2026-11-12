//go:build integration

package identity_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/app/drivers/userlandfs"
	"github.com/optimiware/kodama/internal/userland/cli"
	idfix "github.com/optimiware/kodama/test/support/boundaries/identity"
)

// sessionFixture builds one human-session login fixture. The config directory
// is deliberately left absent: cli.FileSessionStore.Save persists through
// FileSystem.ReplaceFile, so a first login creates it with mode 0700 and writes
// the session file with mode 0600.
type sessionFixtureResult struct {
	root       string
	directory  string
	path       string
	store      *cli.FileSessionStore
	credential client.Credential
	response   idfix.TokenResponse
}

func sessionFixture(t *testing.T, provider *idfix.Provider) sessionFixtureResult {
	t.Helper()
	claims := map[string]any{
		"sub":                   "auth0|human-1",
		provider.TenantClaim():  fixtureTenant,
		provider.ProjectClaim(): fixtureProject,
	}
	if err := provider.RegisterClientWithTTL("kodactl", "client-secret", claims, time.Hour); err != nil {
		t.Fatalf("RegisterClient: %v", err)
	}
	code, err := provider.IssueAuthorizationCode("kodactl", "https://kodama.example/callback", claims)
	if err != nil {
		t.Fatalf("IssueAuthorizationCode: %v", err)
	}
	response, err := provider.ExchangeCode(context.Background(), code, "kodactl", "client-secret", "https://kodama.example/callback")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	root := t.TempDir()
	directory := filepath.Join(root, "kodama")
	path := filepath.Join(directory, "session.json")
	store := &cli.FileSessionStore{Path: path, Files: userlandfs.FileSystem{}, Now: provider.Clock().Now}
	credential := client.Credential{
		Kind:      client.KindSession,
		Token:     response.AccessToken,
		ExpiresAt: response.ExpiresAt(provider.Clock().Now()),
	}
	return sessionFixtureResult{
		root:       root,
		directory:  directory,
		path:       path,
		store:      store,
		credential: credential,
		response:   response,
	}
}

func runLogin(t *testing.T, provider *idfix.Provider, store cli.SessionStore, credential client.Credential) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	ports := cli.Ports{
		Files:      userlandfs.FileSystem{},
		Session:    store,
		Credential: staticCredentialSource{credential: credential},
		Out:        &out,
		Err:        &errOut,
		Now:        provider.Clock().Now,
	}
	config := cli.Config{ServerURL: "https://factory.example", Output: cli.OutputJSON}
	code := cli.Run(context.Background(), config, []string{"login"}, ports)
	return code, out.String(), errOut.String()
}

func TestLoginPersistsSessionWithRestrictivePermissions(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	fixture := sessionFixture(t, provider)
	if _, err := os.Stat(fixture.directory); !os.IsNotExist(err) {
		t.Fatalf("fixture config directory unexpectedly exists: %v", err)
	}

	code, out, errOut := runLogin(t, provider, fixture.store, fixture.credential)
	if code != cli.ExitSuccess {
		t.Fatalf("exit code = %d (stderr %q)", code, errOut)
	}
	if out != "logged in\n" || errOut != "" {
		t.Fatalf("stdout/stderr = %q/%q; want only the login acknowledgement", out, errOut)
	}
	if strings.Contains(out, fixture.credential.Token) || strings.Contains(errOut, fixture.credential.Token) {
		t.Fatal("login rendered the session token")
	}
	directoryInfo, err := os.Stat(fixture.directory)
	if err != nil {
		t.Fatalf("Stat directory: %v", err)
	}
	if directoryInfo.Mode().Perm() != 0o700 {
		t.Fatalf("session directory mode = %o; want 700", directoryInfo.Mode().Perm())
	}
	info, err := os.Stat(fixture.path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	// Save persists through FileSystem.ReplaceFile: a missing config directory
	// is created 0700 and the session file is written 0600.
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("session file mode = %o; want 600", info.Mode().Perm())
	}
	loaded, err := fixture.store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Token != fixture.credential.Token || loaded.Kind != client.KindSession {
		t.Fatalf("loaded session = %+v", loaded)
	}
	requirePersistedSecretsOnly(t, fixture.root, secretRule{
		label: "user token", value: fixture.credential.Token, allowed: fixture.path,
	})
}

// TestLoginCreatesMissingConfigDirectory pins the first-login behavior: a
// login at a non-existent ~/.config/kodama creates the directory with mode
// 0700 and writes the session file with mode 0600.
func TestLoginCreatesMissingConfigDirectory(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	fixture := sessionFixture(t, provider)
	if _, err := os.Stat(fixture.directory); !os.IsNotExist(err) {
		t.Fatalf("fixture config directory unexpectedly exists: %v", err)
	}

	code, out, errOut := runLogin(t, provider, fixture.store, fixture.credential)
	if code != cli.ExitSuccess {
		t.Fatalf("missing-directory login exit code = %d; stderr=%q", code, errOut)
	}
	if out != "logged in\n" || errOut != "" {
		t.Fatalf("stdout/stderr = %q/%q; want only the login acknowledgement", out, errOut)
	}
	directoryInfo, err := os.Stat(fixture.directory)
	if err != nil {
		t.Fatalf("Stat directory: %v", err)
	}
	if directoryInfo.Mode().Perm() != 0o700 {
		t.Fatalf("created directory mode = %o; want 700", directoryInfo.Mode().Perm())
	}
	fileInfo, err := os.Stat(fixture.path)
	if err != nil {
		t.Fatalf("Stat session: %v", err)
	}
	if fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("created session file mode = %o; want 600", fileInfo.Mode().Perm())
	}
	requireNoLeakedSecrets(t, "missing-directory login", []string{out, errOut}, secretRule{
		label: "user token", value: fixture.credential.Token,
	})
}

// TestFileSessionStoreSaveCreatesOwnerOnlyDirectory exercises the real
// userlandfs driver directly: Save creates the missing parent 0700 and the
// session file 0600.
func TestFileSessionStoreSaveCreatesOwnerOnlyDirectory(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "kodama")
	path := filepath.Join(directory, "session.json")
	store := &cli.FileSessionStore{Path: path, Files: userlandfs.FileSystem{}, Now: time.Now}
	credential := client.Credential{Kind: client.KindSession, Token: "human-session-token", ExpiresAt: time.Now().Add(time.Hour)}

	if err := store.Save(context.Background(), credential); err != nil {
		t.Fatalf("Save: %v", err)
	}
	directoryInfo, err := os.Stat(directory)
	if err != nil {
		t.Fatalf("Stat directory: %v", err)
	}
	if directoryInfo.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode = %o; want 700", directoryInfo.Mode().Perm())
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat session: %v", err)
	}
	if fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("session file mode = %o; want 600", fileInfo.Mode().Perm())
	}
	loaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Token != credential.Token {
		t.Fatalf("loaded token = %q; want the saved token", loaded.Token)
	}
}

// TestSessionSaveTightensExistingLooseFile pins that Save replaces a
// pre-existing 0644 session file with a 0600 file instead of leaving the
// looser mode in place.
func TestSessionSaveTightensExistingLooseFile(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	fixture := sessionFixture(t, provider)
	if err := os.MkdirAll(fixture.directory, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(fixture.path, []byte(`{"kind":"session","token":"stale"}`), 0o644); err != nil {
		t.Fatalf("WriteFile stale session: %v", err)
	}
	if err := os.Chmod(fixture.path, 0o644); err != nil {
		t.Fatalf("Chmod stale session: %v", err)
	}

	if err := fixture.store.Save(context.Background(), fixture.credential); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(fixture.path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("pre-existing session file mode = %o; want the replaced 600", info.Mode().Perm())
	}
	loaded, err := fixture.store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Token != fixture.credential.Token {
		t.Fatalf("saved token was not replaced")
	}
}

func TestExpiredSessionAndCredentialAreRefused(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	fixture := sessionFixture(t, provider)
	if err := fixture.store.Save(context.Background(), fixture.credential); err != nil {
		t.Fatalf("Save: %v", err)
	}
	provider.Clock().Advance(2 * time.Hour)
	loaded, err := fixture.store.Load(context.Background())
	requireFaultClass(t, err, fault.Unauthenticated)
	if loaded.Token != "" {
		t.Fatal("Load returned token material for an expired session")
	}
	requireNoLeakedSecrets(t, "expired-session load", []string{err.Error()}, secretRule{
		label: "user token", value: fixture.credential.Token,
	})

	expiredFixture := sessionFixture(t, provider)
	expired := expiredFixture.credential
	expired.ExpiresAt = provider.Clock().Now().Add(-time.Second)
	code, out, errOut := runLogin(t, provider, expiredFixture.store, expired)
	if code != cli.ExitFailure {
		t.Fatalf("expired login exit code = %d; want failure", code)
	}
	if out != "" {
		t.Fatalf("expired login stdout = %q; want empty", out)
	}
	if _, err := os.Stat(expiredFixture.path); !os.IsNotExist(err) {
		t.Fatalf("expired login wrote a session file (stat err %v)", err)
	}
	requireNoLeakedSecrets(t, "expired-login diagnostic", []string{out, errOut}, secretRule{
		label: "user token", value: expired.Token,
	})

	service := client.Credential{Kind: client.KindService, Token: "kodsvc.service-1.fixture-token", ExpiresAt: provider.Clock().Now().Add(time.Hour)}
	code, out, errOut = runLogin(t, provider, expiredFixture.store, service)
	if code != cli.ExitFailure {
		t.Fatalf("service login exit code = %d; want failure", code)
	}
	if out != "" {
		t.Fatalf("service login stdout = %q; want empty", out)
	}
	if _, err := os.Stat(expiredFixture.path); !os.IsNotExist(err) {
		t.Fatalf("service login wrote a session file (stat err %v)", err)
	}
	requireNoLeakedSecrets(t, "service-login diagnostic", []string{out, errOut}, secretRule{
		label: "service token", value: service.Token,
	})
}

// TestSessionSecretsArePersistedOnlyInTheSessionFile walks the whole temp tree
// after the login, renewal and helper flows. The human token may appear only in
// the session file; the refresh token and client secret may appear nowhere.
func TestSessionSecretsArePersistedOnlyInTheSessionFile(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	fixture := sessionFixture(t, provider)

	code, out, errOut := runLogin(t, provider, fixture.store, fixture.credential)
	if code != cli.ExitSuccess || errOut != "" {
		t.Fatalf("login = %d/%q", code, errOut)
	}
	if !strings.Contains(out, "logged in") {
		t.Fatalf("login stdout = %q", out)
	}

	provider.Clock().Advance(30 * time.Minute)
	renewed, err := provider.ExchangeRefreshToken(context.Background(), fixture.response.RefreshToken, "kodactl", "client-secret")
	if err != nil {
		t.Fatalf("ExchangeRefreshToken: %v", err)
	}
	renewedCredential := client.Credential{
		Kind:      client.KindSession,
		Token:     renewed.AccessToken,
		ExpiresAt: renewed.ExpiresAt(provider.Clock().Now()),
	}
	code, out, errOut = runLogin(t, provider, fixture.store, renewedCredential)
	if code != cli.ExitSuccess || errOut != "" {
		t.Fatalf("renewal login = %d/%q", code, errOut)
	}
	if !strings.Contains(out, "logged in") {
		t.Fatalf("renewal login stdout = %q", out)
	}

	var helperOut, helperErr bytes.Buffer
	helperCode := cli.Run(context.Background(), cli.Config{
		ServerURL:      "https://factory.example",
		TrustedOrigins: []string{"https://git.example.com/shop"},
	}, []string{"helper", "get", "https://git.example.com/shop/repo.git"}, cli.Ports{
		Credential: staticCredentialSource{credential: renewedCredential},
		Out:        &helperOut,
		Err:        &helperErr,
		Now:        provider.Clock().Now,
	})
	if helperCode != cli.ExitSuccess || helperErr.Len() != 0 {
		t.Fatalf("helper = %d/%q", helperCode, helperErr.String())
	}
	if !strings.Contains(helperOut.String(), renewedCredential.Token) {
		t.Fatalf("helper did not return the current token")
	}

	requirePersistedSecretsOnly(t, fixture.root,
		secretRule{label: "initial user token", value: fixture.credential.Token, allowed: fixture.path},
		secretRule{label: "renewed user token", value: renewedCredential.Token, allowed: fixture.path},
		secretRule{label: "initial refresh token", value: fixture.response.RefreshToken},
		secretRule{label: "renewed refresh token", value: renewed.RefreshToken},
		secretRule{label: "client secret", value: "client-secret"},
	)

	data, err := os.ReadFile(fixture.path)
	if err != nil {
		t.Fatalf("ReadFile session: %v", err)
	}
	if bytes.Contains(data, []byte(fixture.credential.Token)) {
		t.Fatal("the renewed session file still carries the replaced token")
	}
	if !bytes.Contains(data, []byte(renewedCredential.Token)) {
		t.Fatal("the renewed session file does not carry the current token")
	}
}

func TestSessionRenewalBeforeExpiryReplacesTheToken(t *testing.T) {
	provider := newOIDC(t, idfix.OIDCOptions{})
	fixture := sessionFixture(t, provider)
	if err := fixture.store.Save(context.Background(), fixture.credential); err != nil {
		t.Fatalf("Save first: %v", err)
	}

	// Recover the refresh token through a second code exchange so the test
	// owns the rotation handle.
	claims := map[string]any{
		"sub":                   "auth0|human-1",
		provider.TenantClaim():  fixtureTenant,
		provider.ProjectClaim(): fixtureProject,
	}
	code, err := provider.IssueAuthorizationCode("kodactl", "", claims)
	if err != nil {
		t.Fatalf("IssueAuthorizationCode: %v", err)
	}
	initial, err := provider.ExchangeCode(context.Background(), code, "kodactl", "client-secret", "")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	provider.Clock().Advance(30 * time.Minute) // renewal before expiry
	refreshed, err := provider.ExchangeRefreshToken(context.Background(), initial.RefreshToken, "kodactl", "client-secret")
	if err != nil {
		t.Fatalf("ExchangeRefreshToken: %v", err)
	}
	if refreshed.AccessToken == initial.AccessToken {
		t.Fatal("renewal returned the same access token")
	}

	renewed := client.Credential{
		Kind:      client.KindSession,
		Token:     refreshed.AccessToken,
		ExpiresAt: refreshed.ExpiresAt(provider.Clock().Now()),
	}
	codeRun, out, errOut := runLogin(t, provider, fixture.store, renewed)
	if codeRun != cli.ExitSuccess || out != "logged in\n" || errOut != "" {
		t.Fatalf("renewal login = %d/%q/%q", codeRun, out, errOut)
	}
	loaded, err := fixture.store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load renewed: %v", err)
	}
	if loaded.Token != renewed.Token || loaded.Token == fixture.credential.Token {
		t.Fatalf("renewed session token = %q; want the refreshed value", loaded.Token)
	}
	info, err := os.Stat(fixture.path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("renewed session mode = %o; want 600", info.Mode().Perm())
	}
	if _, err := provider.ExchangeRefreshToken(context.Background(), initial.RefreshToken, "kodactl", "client-secret"); err == nil {
		t.Fatal("the consumed refresh token was accepted again")
	}
	requirePersistedSecretsOnly(t, fixture.root,
		secretRule{label: "renewed user token", value: renewed.Token, allowed: fixture.path},
		secretRule{label: "initial refresh token", value: initial.RefreshToken},
		secretRule{label: "renewed refresh token", value: refreshed.RefreshToken},
		secretRule{label: "client secret", value: "client-secret"},
	)
}

func TestTrustedOriginExactMatching(t *testing.T) {
	// Mutation-sweep note: a mutation that drops the scheme comparison inside
	// Origin.Matches is equivalent, because Matches parses the target through
	// ParseOrigin first and ParseOrigin already refuses a non-https target.
	origin, err := cli.ParseOrigin("https://git.example.com/shop")
	if err != nil {
		t.Fatalf("ParseOrigin: %v", err)
	}
	if origin.Scheme != "https" || origin.Host != "git.example.com" || origin.Path != "/shop" {
		t.Fatalf("parsed origin = %+v; want https://git.example.com/shop", origin)
	}
	allowed := []string{
		"https://git.example.com/shop",
		"https://git.example.com/shop/repo.git",
		"https://git.example.com/shop/team/repo.git",
	}
	for _, target := range allowed {
		if !origin.Matches(target) {
			t.Errorf("origin did not match %q", target)
		}
	}
	refused := []string{
		"https://git.example.com/shopkeeper",
		"https://git.example.com/shop.evil/repo.git",
		"https://git.example.com.evil/shop/repo.git",
		"https://evil.example/shop/repo.git",
		"https://user@git.example.com/shop/repo.git",
		"http://git.example.com/shop/repo.git",
		"https://git.example.com/shop/repo.git?redirect=https://evil.example",
		"https://git.example.com/shop/repo.git#fragment",
		"https://git.example.com/shop/../other/repo.git",
		"https://git.example.com/shop/%2e%2e/other",
	}
	for _, target := range refused {
		if origin.Matches(target) {
			t.Errorf("origin matched refused target %q", target)
		}
	}

	rootOrigin, err := cli.ParseOrigin("https://git.example.com")
	if err != nil {
		t.Fatalf("ParseOrigin(root): %v", err)
	}
	if rootOrigin.Path != "" {
		t.Fatalf("root origin path = %q; want empty", rootOrigin.Path)
	}
	for _, target := range []string{"https://git.example.com", "https://git.example.com/shop/"} {
		if !rootOrigin.Matches(target) {
			t.Errorf("root origin did not match %q", target)
		}
	}
	for _, raw := range []string{"", "http://git.example.com/shop", "https://user@git.example.com/shop", "https://git.example.com/shop?x=1"} {
		if _, err := cli.ParseOrigin(raw); err == nil {
			t.Errorf("ParseOrigin accepted %q", raw)
		}
	}
}

func TestHelperDecisionOriginAndCredentialRules(t *testing.T) {
	now := idfix.DefaultClockTime()
	human := client.Credential{Kind: client.KindSession, Token: "human-token", ExpiresAt: now.Add(time.Hour)}
	execution := client.Credential{Kind: client.KindExecution, Token: "execution-token"}
	service := client.Credential{Kind: client.KindService, Token: "service-token", ExpiresAt: now.Add(time.Hour)}
	expired := human
	expired.ExpiresAt = now
	config := cli.Config{ServerURL: "https://factory.example", TrustedOrigins: []string{"https://git.example.com/shop"}}

	cases := []struct {
		name       string
		protocol   string
		url        string
		credential client.Credential
		want       string
	}{
		{"exact origin", "get", "https://git.example.com/shop", human, "human-token"},
		{"subpath", "get", "https://git.example.com/shop/repo.git", human, "human-token"},
		{"execution subpath", "get", "https://git.example.com/shop/repo.git", execution, "execution-token"},
		{"lookalike path", "get", "https://git.example.com/shopkeeper", human, ""},
		{"lookalike host", "get", "https://git.example.com.evil/shop/repo.git", human, ""},
		{"userinfo", "get", "https://user@git.example.com/shop/repo.git", human, ""},
		{"insecure scheme", "get", "http://git.example.com/shop/repo.git", human, ""},
		{"query", "get", "https://git.example.com/shop/repo.git?x=1", human, ""},
		{"fragment", "get", "https://git.example.com/shop/repo.git#x", human, ""},
		{"traversal", "get", "https://git.example.com/shop/../other", human, ""},
		{"service credential", "get", "https://git.example.com/shop/repo.git", service, ""},
		{"expired credential", "get", "https://git.example.com/shop/repo.git", expired, ""},
		{"store", "store", "https://git.example.com/shop/repo.git", human, ""},
		{"erase", "erase", "https://git.example.com/shop/repo.git", human, ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			response, err := cli.HelperDecision(config, cli.HelperRequest{Protocol: testCase.protocol, URL: testCase.url}, testCase.credential, now)
			if err != nil {
				t.Fatalf("HelperDecision: %v", err)
			}
			if response.Password != testCase.want {
				t.Fatalf("password = %q; want %q", response.Password, testCase.want)
			}
			if response.Password != "" && response.Username != "kodama" {
				t.Fatalf("username = %q; want kodama", response.Username)
			}
			if response.Password == "" && response.Username != "" {
				t.Fatalf("refusal username = %q; want empty", response.Username)
			}
		})
	}
}

func TestHelperOutputShapeAndDiagnosticsRedaction(t *testing.T) {
	now := idfix.DefaultClockTime()
	human := client.Credential{Kind: client.KindSession, Token: "human-token", ExpiresAt: now.Add(time.Hour)}
	config := cli.Config{ServerURL: "https://factory.example", TrustedOrigins: []string{"https://git.example.com/shop"}}

	var out, errOut bytes.Buffer
	ports := cli.Ports{
		Credential: staticCredentialSource{credential: human},
		Out:        &out,
		Err:        &errOut,
		Now:        func() time.Time { return now },
	}
	code := cli.Run(context.Background(), config, []string{"helper", "get", "https://git.example.com/shop/repo.git"}, ports)
	if code != cli.ExitSuccess {
		t.Fatalf("helper exit code = %d", code)
	}
	if out.String() != "username=kodama\npassword=human-token\n" {
		t.Fatalf("helper stdout = %q; want the declared shape", out.String())
	}
	if errOut.Len() != 0 {
		t.Fatalf("helper stderr = %q; want empty", errOut.String())
	}

	for _, protocol := range []string{"store", "erase"} {
		out.Reset()
		errOut.Reset()
		code = cli.Run(context.Background(), config, []string{"helper", protocol, "https://git.example.com/shop/repo.git"}, ports)
		if code != cli.ExitSuccess || out.Len() != 0 || errOut.Len() != 0 {
			t.Fatalf("%s = %d/%q/%q; want an empty success", protocol, code, out.String(), errOut.String())
		}
		requireNoLeakedSecrets(t, protocol, []string{out.String(), errOut.String()}, secretRule{
			label: "human token", value: human.Token,
		})
	}

	out.Reset()
	errOut.Reset()
	code = cli.Run(context.Background(), config, []string{"helper", "get", "https://git.example.com/other/repo.git"}, ports)
	if code != cli.ExitSuccess || out.Len() != 0 {
		t.Fatalf("unregistered helper = %d/%q; want an empty success", code, out.String())
	}
	requireNoLeakedSecrets(t, "unregistered helper", []string{out.String(), errOut.String()}, secretRule{
		label: "human token", value: human.Token,
	})

	out.Reset()
	errOut.Reset()
	code = cli.Run(context.Background(), config, []string{"helper", "bogus", "https://git.example.com/shop/repo.git"}, ports)
	if code != cli.ExitUsage {
		t.Fatalf("usage exit code = %d; want %d", code, cli.ExitUsage)
	}
	requireNoLeakedSecrets(t, "helper usage diagnostic", []string{out.String(), errOut.String()}, secretRule{
		label: "human token", value: human.Token,
	})
}

func TestHelperNeverReadsCredentialForStoreOrErase(t *testing.T) {
	source := &countingSource{}
	config := cli.Config{ServerURL: "https://factory.example", TrustedOrigins: []string{"https://git.example.com/shop"}}
	for _, protocol := range []string{"store", "erase"} {
		var out, errOut bytes.Buffer
		ports := cli.Ports{Credential: source, Out: &out, Err: &errOut, Now: time.Now}
		if code := cli.Run(context.Background(), config, []string{"helper", protocol, "https://git.example.com/shop/repo.git"}, ports); code != cli.ExitSuccess {
			t.Fatalf("%s exit code = %d", protocol, code)
		}
	}
	if source.calls != 0 {
		t.Fatalf("credential reads = %d; want 0 for store/erase", source.calls)
	}
}

type countingSource struct{ calls int }

func (source *countingSource) Credential(context.Context) (client.Credential, error) {
	source.calls++
	return client.Credential{Kind: client.KindSession, Token: "human-token", ExpiresAt: time.Now().Add(time.Hour)}, nil
}
