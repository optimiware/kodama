//go:build integration

package identity_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/userland/cli"
	idfix "github.com/optimiware/kodama/test/support/boundaries/identity"
)

// fakeBrowserHelperEnv marks the re-executed test binary as the fake browser.
const fakeBrowserHelperEnv = "KODAMA_BROWSER_HELPER"

// fakeBrowserBinaryEnv names the test binary the launcher re-executes.
const fakeBrowserBinaryEnv = "KODAMA_BROWSER_BINARY"

// fakeBrowserModeEnv selects the hostile browser behavior.
const fakeBrowserModeEnv = "KODAMA_BROWSER_MODE"

// fakeBrowserLogEnv names the observation file the helper writes.
const fakeBrowserLogEnv = "KODAMA_BROWSER_LOG"

// loginHarness drives the real kodactl binary as a child process against the
// in-process OIDC and discovery fixtures. Everything the child can observe is
// isolated in one temporary root so a tree-wide secret scan is meaningful.
type loginHarness struct {
	t    *testing.T
	root string
	home string
	// configDir is XDG_CONFIG_HOME; sessionDir is the default 0700 session
	// directory and sessionPath the default session file.
	configDir   string
	sessionDir  string
	sessionPath string
	tmpDir      string
	caPath      string
	browserDir  string
	browserLog  string
	binary      string
	helperPath  string
	provider    *idfix.Provider
	discovery   *idfix.DiscoveryServer
	clientID    string
}

// newLoginHarness builds kodactl, starts the fixtures and prepares one isolated
// child environment. The fake browser is a re-exec of the test binary placed
// first on PATH as xdg-open (and open for macOS).
func newLoginHarness(t *testing.T) *loginHarness {
	t.Helper()
	root := t.TempDir()
	harness := &loginHarness{
		t:          t,
		root:       root,
		home:       filepath.Join(root, "home"),
		configDir:  filepath.Join(root, "home", ".config"),
		tmpDir:     filepath.Join(root, "tmp"),
		browserDir: filepath.Join(root, "bin"),
		browserLog: filepath.Join(root, "browser.json"),
		clientID:   "kodactl",
	}
	harness.sessionDir = filepath.Join(harness.configDir, "kodama")
	harness.sessionPath = filepath.Join(harness.sessionDir, "session.json")
	for _, directory := range []string{harness.home, harness.configDir, harness.sessionDir, harness.tmpDir, harness.browserDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("MkdirAll(%s): %v", directory, err)
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			t.Fatalf("Chmod(%s): %v", directory, err)
		}
	}

	provider, err := idfix.NewProvider(idfix.OIDCOptions{Clock: idfix.NewClock(time.Now().Truncate(time.Second))})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	t.Cleanup(provider.Close)
	harness.provider = provider
	claims := map[string]any{
		"sub":                   "auth0|e2e-human",
		provider.TenantClaim():  "tenant-a",
		provider.ProjectClaim(): "project-1",
	}
	if err := provider.RegisterPublicClient(harness.clientID, claims, time.Hour); err != nil {
		t.Fatalf("RegisterPublicClient: %v", err)
	}

	discovery, err := idfix.NewDiscoveryServer(idfix.DiscoveryOptions{
		Document: idfix.DiscoveryDocumentFor(provider, "", harness.clientID),
	})
	if err != nil {
		t.Fatalf("NewDiscoveryServer: %v", err)
	}
	t.Cleanup(discovery.Close)
	harness.discovery = discovery

	harness.caPath = filepath.Join(root, "ca.pem")
	bundle := append(append([]byte(nil), provider.CACertPEM()...), discovery.CACertPEM()...)
	if err := os.WriteFile(harness.caPath, bundle, 0o600); err != nil {
		t.Fatalf("WriteFile CA: %v", err)
	}

	harness.binary = buildKodactl(t)
	if err := harness.installFakeBrowser(); err != nil {
		t.Fatalf("install fake browser: %v", err)
	}
	return harness
}

// installFakeBrowser writes the xdg-open/open launchers that re-execute this
// test binary as the fake browser helper.
func (harness *loginHarness) installFakeBrowser() error {
	helper, err := os.Executable()
	if err != nil {
		return err
	}
	script := "#!/bin/sh\nexec \"$" + fakeBrowserBinaryEnv + "\" -test.run=^TestFakeBrowserProcess$ \"$@\"\n"
	for _, name := range []string{"xdg-open", "open"} {
		path := filepath.Join(harness.browserDir, name)
		if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
			return err
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return err
		}
	}
	harness.helperPath = helper
	return nil
}

// environment builds the child environment. It deliberately omits DISPLAY,
// SSH_CONNECTION and any developer KODAMA_SERVER value unless a test adds them.
func (harness *loginHarness) environment(extra map[string]string) []string {
	environment := []string{
		"PATH=" + harness.browserDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + harness.home,
		"XDG_CONFIG_HOME=" + harness.configDir,
		"TMPDIR=" + harness.tmpDir,
		"SSL_CERT_FILE=" + harness.caPath,
		"KODAMA_SERVER=" + harness.discovery.URL(),
		fakeBrowserHelperEnv + "=1",
		fakeBrowserBinaryEnv + "=" + harness.helperPath,
		fakeBrowserLogEnv + "=" + harness.browserLog,
	}
	for name, value := range extra {
		environment = append(environment, name+"="+value)
	}
	return environment
}

// run executes the real binary with the isolated environment.
func (harness *loginHarness) run(extra map[string]string, args ...string) (int, string, string) {
	code, stdout, stderr, _ := harness.runTimed(extra, args...)
	return code, stdout, stderr
}

// runTimed executes the real binary and also reports the wall-clock duration.
func (harness *loginHarness) runTimed(extra map[string]string, args ...string) (int, string, string, time.Duration) {
	harness.t.Helper()
	command := exec.Command(harness.binary, args...)
	command.Env = harness.environment(extra)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	started := time.Now()
	err := command.Run()
	elapsed := time.Since(started)
	if err == nil {
		return 0, stdout.String(), stderr.String(), elapsed
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		harness.t.Fatalf("run kodactl: %v", err)
	}
	return exitErr.ExitCode(), stdout.String(), stderr.String(), elapsed
}

// start launches the real binary and returns a wait function.
func (harness *loginHarness) start(extra map[string]string, args ...string) func() (int, string, string) {
	harness.t.Helper()
	command := exec.Command(harness.binary, args...)
	command.Env = harness.environment(extra)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		harness.t.Fatalf("start kodactl: %v", err)
	}
	return func() (int, string, string) {
		err := command.Wait()
		if err == nil {
			return 0, stdout.String(), stderr.String()
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			harness.t.Fatalf("wait kodactl: %v", err)
		}
		return exitErr.ExitCode(), stdout.String(), stderr.String()
	}
}

// browserObservation is the fake browser's safe result record.
type browserObservation struct {
	AuthorizationStatus int    `json:"authorization_status"`
	CallbackStatus      int    `json:"callback_status"`
	SecondStatus        int    `json:"second_status"`
	HostileStatus       int    `json:"hostile_status"`
	Location            string `json:"location"`
	Error               string `json:"error"`
}

// readBrowserObservation reads the helper's record.
func (harness *loginHarness) readBrowserObservation() browserObservation {
	harness.t.Helper()
	data, err := os.ReadFile(harness.browserLog)
	if err != nil {
		harness.t.Fatalf("ReadFile browser log: %v", err)
	}
	var observation browserObservation
	if err := json.Unmarshal(data, &observation); err != nil {
		harness.t.Fatalf("decode browser log: %v", err)
	}
	return observation
}

// requireSession reads and strictly parses the persisted session.
func (harness *loginHarness) requireSession() cli.SessionRecord {
	harness.t.Helper()
	data, err := os.ReadFile(harness.sessionPath)
	if err != nil {
		harness.t.Fatalf("ReadFile session: %v", err)
	}
	record, err := cli.ParseSessionRecord(data)
	if err != nil {
		harness.t.Fatalf("ParseSessionRecord: %v", err)
	}
	return record
}

// writeSession durably writes one record with the restrictive modes.
func (harness *loginHarness) writeSession(record cli.SessionRecord) {
	harness.t.Helper()
	if err := os.Chmod(harness.sessionDir, 0o700); err != nil {
		harness.t.Fatalf("Chmod session directory: %v", err)
	}
	data, err := json.Marshal(record)
	if err != nil {
		harness.t.Fatalf("marshal session: %v", err)
	}
	if err := os.WriteFile(harness.sessionPath, data, 0o600); err != nil {
		harness.t.Fatalf("WriteFile session: %v", err)
	}
	if err := os.Chmod(harness.sessionPath, 0o600); err != nil {
		harness.t.Fatalf("Chmod session: %v", err)
	}
}

// login runs the browser flow and requires success, returning the record.
func (harness *loginHarness) login() (cli.SessionRecord, string, string) {
	harness.t.Helper()
	code, stdout, stderr := harness.run(map[string]string{"DISPLAY": ":0"}, "login")
	if code != 0 {
		harness.t.Fatalf("login exit = %d; stderr=%q", code, stderr)
	}
	return harness.requireSession(), stdout, stderr
}

// requireNoTokenOutput fails when any stream renders a token.
func requireNoTokenOutput(t *testing.T, streams []string, values ...string) {
	t.Helper()
	for _, value := range values {
		if value == "" {
			continue
		}
		for index, stream := range streams {
			if strings.Contains(stream, value) {
				t.Fatalf("stream %d rendered a token", index)
			}
		}
	}
}

// refreshTokenCalls counts the provider refresh-token grants.
func refreshTokenCalls(provider *idfix.Provider) int {
	count := 0
	for _, request := range provider.Requests() {
		if request.Path == "/oauth/token" && request.GrantType == "refresh_token" {
			count++
		}
	}
	return count
}

// authorizationCodeCalls counts the provider authorization-code grants.
func authorizationCodeCalls(provider *idfix.Provider) int {
	count := 0
	for _, request := range provider.Requests() {
		if request.Path == "/oauth/token" && request.GrantType == "authorization_code" {
			count++
		}
	}
	return count
}

// TestFakeBrowserProcess is the re-executed fake browser. It is skipped during
// a normal test run. It follows the authorization redirect to the loopback
// callback and can tamper with state, nonce and redirect, replay the callback,
// or forge the Host header.
func TestFakeBrowserProcess(t *testing.T) {
	if os.Getenv(fakeBrowserHelperEnv) != "1" {
		t.Skip("not the fake browser helper")
	}
	target := lastHTTPArgument(os.Args)
	if target == "" {
		os.Exit(2)
	}
	mode := os.Getenv(fakeBrowserModeEnv)
	observation := browserObservation{}

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	get := func(raw string, host string) (*http.Response, error) {
		request, err := http.NewRequest(http.MethodGet, raw, nil)
		if err != nil {
			return nil, err
		}
		if host != "" {
			request.Host = host
		}
		return client.Do(request)
	}

	switch mode {
	case "state":
		location, providerStatus, err := authorizationLocation(client, target)
		observation.AuthorizationStatus = providerStatus
		if err != nil || location == "" {
			observation.Error = "no location"
			break
		}
		observation.Location = location
		tampered := mutateQuery(location, "state", "attacker-state")
		response, err := get(tampered, "")
		if err != nil {
			observation.Error = err.Error()
			break
		}
		observation.CallbackStatus = response.StatusCode
		drain(response)
	case "nonce":
		tampered := mutateQuery(target, "nonce", "attacker-nonce")
		location, providerStatus, err := authorizationLocation(client, tampered)
		observation.AuthorizationStatus = providerStatus
		if err != nil || location == "" {
			observation.Error = "no location"
			break
		}
		observation.Location = location
		response, err := get(location, "")
		if err != nil {
			observation.Error = err.Error()
			break
		}
		observation.CallbackStatus = response.StatusCode
		drain(response)
	case "redirect":
		original, _ := url.Parse(target)
		port := original.Query().Get("redirect_uri")
		evil := target
		if parsed, err := url.Parse(port); err == nil {
			evil = mutateQuery(target, "redirect_uri", "http://localhost:"+parsed.Port()+"/callback")
		}
		location, providerStatus, _ := authorizationLocation(client, evil)
		observation.AuthorizationStatus = providerStatus
		observation.Location = location
		// Unblock the waiting product with an explicit refusal at the real
		// callback instead of leaving it until its own timeout.
		state := original.Query().Get("state")
		callback := port + "?error=access_denied&state=" + url.QueryEscape(state)
		response, err := get(callback, "")
		if err != nil {
			observation.Error = err.Error()
			break
		}
		observation.CallbackStatus = response.StatusCode
		drain(response)
	case "second_callback":
		location, providerStatus, err := authorizationLocation(client, target)
		observation.AuthorizationStatus = providerStatus
		if err != nil || location == "" {
			observation.Error = "no location"
			break
		}
		observation.Location = location
		// Send both callback requests on one keep-alive TCP connection so the
		// replay deterministically reaches the still-open handler instead of
		// depending on connection pooling.
		firstStatus, secondStatus, err := sendCallbackTwice(location)
		observation.CallbackStatus = firstStatus
		observation.SecondStatus = secondStatus
		if err != nil {
			observation.Error = err.Error()
		}
	case "host":
		location, providerStatus, err := authorizationLocation(client, target)
		observation.AuthorizationStatus = providerStatus
		if err != nil || location == "" {
			observation.Error = "no location"
			break
		}
		observation.Location = location
		hostile, err := get(location, "evil.example")
		if err != nil {
			observation.Error = err.Error()
			break
		}
		observation.HostileStatus = hostile.StatusCode
		drain(hostile)
		response, err := get(location, "")
		if err != nil {
			observation.Error = err.Error()
			break
		}
		observation.CallbackStatus = response.StatusCode
		drain(response)
	default:
		location, providerStatus, err := authorizationLocation(client, target)
		observation.AuthorizationStatus = providerStatus
		if err != nil || location == "" {
			observation.Error = "no location"
			break
		}
		observation.Location = location
		response, err := get(location, "")
		if err != nil {
			observation.Error = err.Error()
			break
		}
		observation.CallbackStatus = response.StatusCode
		drain(response)
	}

	encoded, _ := json.Marshal(observation)
	if err := os.WriteFile(os.Getenv(fakeBrowserLogEnv), encoded, 0o600); err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

// authorizationLocation performs the authorization GET without following the
// redirect and returns the Location header and the provider status.
func authorizationLocation(client *http.Client, target string) (string, int, error) {
	response, err := client.Get(target)
	if err != nil {
		return "", 0, err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.Header.Get("Location"), response.StatusCode, nil
}

// mutateQuery replaces one query member of raw.
func mutateQuery(raw, name, value string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	query := parsed.Query()
	query.Set(name, value)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// lastHTTPArgument returns the last argv element that parses as an http(s) URL.
func lastHTTPArgument(args []string) string {
	for _, candidate := range slices.Backward(args) {

		if strings.HasPrefix(candidate, "http://") || strings.HasPrefix(candidate, "https://") {
			return candidate
		}
	}
	return ""
}

// drain reads and discards one bounded response body.
func drain(response *http.Response) {
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
}

// sendCallbackTwice sends both GET /callback requests over one keep-alive TCP
// connection. It makes the replay deterministic: the second request reaches the
// still-open handler instead of racing connection pooling or listener close.
func sendCallbackTwice(location string) (int, int, error) {
	parsed, err := url.Parse(location)
	if err != nil {
		return 0, 0, err
	}
	if parsed.Scheme != "http" || parsed.Host == "" {
		return 0, 0, fmt.Errorf("callback URL %q is not a plain loopback URL", location)
	}
	conn, err := net.DialTimeout("tcp", parsed.Host, 5*time.Second)
	if err != nil {
		return 0, 0, err
	}
	defer conn.Close()
	// Bound both responses so a mutation that accepts the replay cannot hang
	// the helper: it records a refused/timeout status and exits.
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(conn)
	statuses := [2]int{}
	request := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nConnection: keep-alive\r\n\r\n", parsed.RequestURI(), parsed.Host)
	for index := range statuses {
		if _, err := io.WriteString(conn, request); err != nil {
			return statuses[0], statuses[1], err
		}
		response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
		if err != nil {
			return statuses[0], statuses[1], err
		}
		statuses[index] = response.StatusCode
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
	return statuses[0], statuses[1], nil
}

// discoveryDocument returns the current fixture document.
func (harness *loginHarness) discoveryDocument() api.DiscoveryDocument {
	return harness.discovery.Document()
}

// waitFor polls until condition is true or the deadline passes.
func waitFor(t *testing.T, timeout time.Duration, condition func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return condition()
}
