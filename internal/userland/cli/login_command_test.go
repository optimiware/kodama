// Tests for the kodactl login/logout/refresh/status command logic. Every
// collaborator is a deterministic fake; the login library itself is covered
// by the contract vectors.
package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/userland/cli"
)

const (
	commandIssuer = "https://issuer.example"
	commandAPI    = "https://kodama.example.com"
	commandClient = "kodactl"
	commandAud    = "kodama-api"

	accessCanary  = "access-token-canary-1"
	refreshCanary = "refresh-token-canary-1"
	deviceCanary  = "device-code-canary-1"
)

// commandDocument is the discovery document the fake OIDC server returns.
func commandDocument() api.DiscoveryDocument {
	return api.DiscoveryDocument{
		Issuer:                      commandIssuer,
		ClientID:                    commandClient,
		Audience:                    commandAud,
		AuthorizationEndpoint:       commandIssuer + "/oauth/authorize",
		TokenEndpoint:               commandIssuer + "/oauth/token",
		DeviceAuthorizationEndpoint: commandIssuer + "/oauth/device",
		RevocationEndpoint:          commandIssuer + "/oauth/revoke",
		Scopes:                      []string{"openid", "profile"},
		APIURL:                      commandAPI,
	}
}

// commandConfig is the kodactl configuration bound to commandDocument.
func commandConfig() cli.Config {
	config := testConfig()
	config.ServerURL = commandAPI
	return config
}

// commandRecord is one current version 2 session.
func commandRecord(expiresAt time.Time) cli.SessionRecord {
	return cli.SessionRecord{
		Version:            cli.SessionFileVersion,
		Kind:               "session",
		Token:              "old-access-canary",
		ExpiresAt:          expiresAt,
		RefreshToken:       refreshCanary,
		Issuer:             commandIssuer,
		ClientID:           commandClient,
		Audience:           commandAud,
		APIURL:             commandAPI,
		TokenEndpoint:      commandIssuer + "/oauth/token",
		RevocationEndpoint: commandIssuer + "/oauth/revoke",
	}
}

// changedBindingDocument is a valid discovery document for a DIFFERENT
// issuer, client and audience than commandRecord. Only the API URL is shared.
func changedBindingDocument() api.DiscoveryDocument {
	document := commandDocument()
	document.Issuer = "https://changed.example"
	document.AuthorizationEndpoint = document.Issuer + "/oauth/authorize"
	document.TokenEndpoint = document.Issuer + "/oauth/token"
	document.DeviceAuthorizationEndpoint = document.Issuer + "/oauth/device"
	document.RevocationEndpoint = document.Issuer + "/oauth/revoke"
	document.ClientID = "changed-client"
	document.Audience = "changed-audience"
	return document
}

// otherAPIDocument is a valid document bound to another API URL.
func otherAPIDocument() api.DiscoveryDocument {
	document := commandDocument()
	document.APIURL = "https://other-api.example"
	return document
}

// eventLog records ordered collaborator events.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (log *eventLog) add(event string) {
	if log == nil {
		return
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	log.events = append(log.events, event)
}

func (log *eventLog) snapshot() []string {
	if log == nil {
		return nil
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	return append([]string(nil), log.events...)
}

// positions returns every index of one event.
func (log *eventLog) positions(event string) []int {
	var found []int
	for index, candidate := range log.snapshot() {
		if candidate == event {
			found = append(found, index)
		}
	}
	return found
}

// commandOIDC is a deterministic cli.OIDCClient.
type commandOIDC struct {
	mu            sync.Mutex
	document      api.DiscoveryDocument
	discoveryErr  error
	tokenFn       func(cli.TokenRequest) (cli.TokenResponse, error)
	deviceFn      func(cli.DeviceAuthorizationRequest) (cli.DeviceAuthorizationResponse, error)
	revokeFn      func(cli.RevocationRequest) error
	jwksFn        func(string) (json.RawMessage, error)
	tokenCalls    []cli.TokenRequest
	deviceCalls   []cli.DeviceAuthorizationRequest
	revokeCalls   []cli.RevocationRequest
	jwksCalls     []string
	discoveredURL []string
	events        *eventLog
}

func (server *commandOIDC) Discover(_ context.Context, discoveryURL string) (api.DiscoveryDocument, error) {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.discoveredURL = append(server.discoveredURL, discoveryURL)
	if server.discoveryErr != nil {
		return api.DiscoveryDocument{}, server.discoveryErr
	}
	return server.document, nil
}

func (server *commandOIDC) Token(_ context.Context, request cli.TokenRequest) (cli.TokenResponse, error) {
	server.mu.Lock()
	server.tokenCalls = append(server.tokenCalls, request)
	tokenFn := server.tokenFn
	server.mu.Unlock()
	if tokenFn != nil {
		return tokenFn(request)
	}
	return cli.TokenResponse{
		AccessToken:  accessCanary,
		RefreshToken: refreshCanary,
		IDToken:      "id-token-canary",
		TokenType:    "Bearer",
		ExpiresIn:    time.Hour,
	}, nil
}

func (server *commandOIDC) DeviceAuthorize(_ context.Context, request cli.DeviceAuthorizationRequest) (cli.DeviceAuthorizationResponse, error) {
	server.mu.Lock()
	server.deviceCalls = append(server.deviceCalls, request)
	deviceFn := server.deviceFn
	server.mu.Unlock()
	if deviceFn != nil {
		return deviceFn(request)
	}
	return cli.DeviceAuthorizationResponse{
		DeviceCode:      deviceCanary,
		UserCode:        "USER-CODE-1",
		VerificationURI: commandIssuer + "/device",
		ExpiresIn:       10 * time.Minute,
		Interval:        5 * time.Second,
	}, nil
}

func (server *commandOIDC) Revoke(_ context.Context, request cli.RevocationRequest) error {
	server.mu.Lock()
	server.revokeCalls = append(server.revokeCalls, request)
	revokeFn := server.revokeFn
	events := server.events
	server.mu.Unlock()
	events.add("revoke")
	if revokeFn != nil {
		return revokeFn(request)
	}
	return nil
}

func (server *commandOIDC) JWKS(_ context.Context, issuer string) (json.RawMessage, error) {
	server.mu.Lock()
	server.jwksCalls = append(server.jwksCalls, issuer)
	jwksFn := server.jwksFn
	server.mu.Unlock()
	if jwksFn != nil {
		return jwksFn(issuer)
	}
	return json.RawMessage(`{"keys":[]}`), nil
}

func (server *commandOIDC) tokenRequests() []cli.TokenRequest {
	server.mu.Lock()
	defer server.mu.Unlock()
	return append([]cli.TokenRequest(nil), server.tokenCalls...)
}

func (server *commandOIDC) deviceRequests() []cli.DeviceAuthorizationRequest {
	server.mu.Lock()
	defer server.mu.Unlock()
	return append([]cli.DeviceAuthorizationRequest(nil), server.deviceCalls...)
}

func (server *commandOIDC) revokeRequests() []cli.RevocationRequest {
	server.mu.Lock()
	defer server.mu.Unlock()
	return append([]cli.RevocationRequest(nil), server.revokeCalls...)
}

func (server *commandOIDC) discovered() []string {
	server.mu.Lock()
	defer server.mu.Unlock()
	return append([]string(nil), server.discoveredURL...)
}

// commandStore is an in-memory cli.SessionRecordStore.
type commandStore struct {
	mu        sync.Mutex
	record    cli.SessionRecord
	present   bool
	loadErr   error
	saveErr   error
	deleteErr error
	events    *eventLog
}

func (store *commandStore) LoadRecord(ctx context.Context) (cli.SessionRecord, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return cli.SessionRecord{}, err
		}
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	store.events.add("load")
	if store.loadErr != nil {
		return cli.SessionRecord{}, store.loadErr
	}
	if !store.present {
		return cli.SessionRecord{}, fs.ErrNotExist
	}
	return store.record, nil
}

func (store *commandStore) SaveRecord(ctx context.Context, record cli.SessionRecord) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	store.events.add("save")
	if store.saveErr != nil {
		return store.saveErr
	}
	store.record = record
	store.present = true
	return nil
}

func (store *commandStore) DeleteRecord(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	store.events.add("delete")
	if store.deleteErr != nil {
		return store.deleteErr
	}
	store.present = false
	return nil
}

func (store *commandStore) current() (cli.SessionRecord, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.record, store.present
}

// commandLock is a process-wide fake cli.SessionLock.
type commandLock struct {
	gate       sync.Mutex
	mu         sync.Mutex
	acquires   int
	releases   int
	acquireErr error
	events     *eventLog
}

func (lock *commandLock) Acquire(ctx context.Context) (func() error, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	if lock.acquireErr != nil {
		return nil, lock.acquireErr
	}
	lock.gate.Lock()
	lock.mu.Lock()
	lock.acquires++
	lock.mu.Unlock()
	lock.events.add("lock")
	var once sync.Once
	return func() error {
		once.Do(func() {
			lock.events.add("unlock")
			lock.mu.Lock()
			lock.releases++
			lock.mu.Unlock()
			lock.gate.Unlock()
		})
		return nil
	}, nil
}

func (lock *commandLock) counts() (acquires, releases int) {
	lock.mu.Lock()
	defer lock.mu.Unlock()
	return lock.acquires, lock.releases
}

// commandClock is a deterministic cli.Clock.
type commandClock struct {
	now time.Time
}

func (clock commandClock) Now() time.Time { return clock.now }

func (clock commandClock) Sleep(ctx context.Context, _ time.Duration) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

// commandBrowser records the authorization URLs it was asked to open.
type commandBrowser struct {
	mu   sync.Mutex
	urls []string
	err  error
}

func (browser *commandBrowser) Open(_ context.Context, url string) error {
	browser.mu.Lock()
	defer browser.mu.Unlock()
	browser.urls = append(browser.urls, url)
	return browser.err
}

func (browser *commandBrowser) opened() []string {
	browser.mu.Lock()
	defer browser.mu.Unlock()
	return append([]string(nil), browser.urls...)
}

// commandLoopback is a deterministic cli.LoopbackListener.
type commandLoopback struct {
	mu        sync.Mutex
	waits     int
	waitErr   error
	closed    int
	delivered *cli.CallbackParams
}

func (loopback *commandLoopback) Listen(context.Context) (string, func(context.Context) (cli.CallbackParams, error), func() error, error) {
	return "127.0.0.1:43210", func(context.Context) (cli.CallbackParams, error) {
			loopback.mu.Lock()
			defer loopback.mu.Unlock()
			loopback.waits++
			if loopback.delivered != nil {
				return *loopback.delivered, nil
			}
			if loopback.waitErr != nil {
				return cli.CallbackParams{}, loopback.waitErr
			}
			return cli.CallbackParams{}, errors.New("no callback")
		}, func() error {
			loopback.mu.Lock()
			defer loopback.mu.Unlock()
			loopback.closed++
			return nil
		}, nil
}

// envMap builds an injected environment lookup.
func envMap(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

// zeroRandom is a deterministic cli.Random.
type zeroRandom struct {
	mu sync.Mutex
}

func (random *zeroRandom) Read(buffer []byte) (int, error) {
	random.mu.Lock()
	defer random.mu.Unlock()
	for index := range buffer {
		buffer[index] = byte(index)
	}
	return len(buffer), nil
}

// commandRuntime builds one complete login runtime around the fakes.
func commandRuntime(oidc *commandOIDC, store *commandStore, lock *commandLock, clock commandClock, env map[string]string) cli.LoginRuntime {
	return cli.LoginRuntime{
		OIDC:        oidc,
		Browser:     &commandBrowser{},
		Loopback:    &commandLoopback{},
		Random:      &zeroRandom{},
		Clock:       clock,
		Lock:        lock,
		Store:       store,
		Environment: envMap(env),
		Timeout:     time.Second,
	}
}

func runWithRuntime(t *testing.T, config cli.Config, args []string, ports cli.Ports, runtime cli.LoginRuntime) (int, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	out := ports.Out.(*bytes.Buffer)
	errOut := ports.Err.(*bytes.Buffer)
	code := cli.RunWithDeps(context.Background(), config, args, ports, cli.CommandDeps{Login: runtime})
	return code, out, errOut
}

func TestRunWithDepsLoginDeviceFlow(t *testing.T) {
	oidc := &commandOIDC{document: commandDocument()}
	store := &commandStore{}
	lock := &commandLock{}
	clock := commandClock{now: testNow()}
	ports, _, _ := testPorts(&scriptedTransport{})
	code, out, errOut := runWithRuntime(t, commandConfig(), []string{"login", "--device"}, ports,
		commandRuntime(oidc, store, lock, clock, nil))
	if code != cli.ExitSuccess {
		t.Fatalf("exit = %d; stderr=%q", code, errOut.String())
	}
	record, present := store.current()
	if !present {
		t.Fatal("the login did not persist a session")
	}
	if record.Version != cli.SessionFileVersion || record.Kind != "session" {
		t.Fatalf("persisted record = %+v", record)
	}
	if record.Token != accessCanary || record.RefreshToken != refreshCanary {
		t.Fatalf("persisted tokens = %q/%q", record.Token, record.RefreshToken)
	}
	if record.Issuer != commandIssuer || record.ClientID != commandClient || record.Audience != commandAud {
		t.Fatalf("persisted binding = %+v", record)
	}
	if record.APIURL != commandAPI || record.TokenEndpoint != commandIssuer+"/oauth/token" ||
		record.RevocationEndpoint != commandIssuer+"/oauth/revoke" {
		t.Fatalf("persisted endpoints = %+v", record)
	}
	if !record.ExpiresAt.Equal(commandClock{now: testNow()}.now.Add(time.Hour)) {
		t.Fatalf("expiry = %s; want one hour", record.ExpiresAt)
	}
	devices := oidc.deviceRequests()
	if len(devices) != 1 || devices[0].Endpoint != commandIssuer+"/oauth/device" ||
		devices[0].ClientID != commandClient {
		t.Fatalf("device requests = %+v", devices)
	}
	tokens := oidc.tokenRequests()
	if len(tokens) != 1 || tokens[0].GrantType != "urn:ietf:params:oauth:grant-type:device_code" ||
		tokens[0].Endpoint != commandIssuer+"/oauth/token" ||
		tokens[0].DeviceCode != deviceCanary {
		t.Fatalf("token requests = %+v", tokens)
	}
	output := out.String()
	if !strings.Contains(output, commandIssuer+"/device") || !strings.Contains(output, "USER-CODE-1") {
		t.Fatalf("device instructions missing: %q", output)
	}
	if !strings.Contains(output, "logged in") || !strings.Contains(output, commandIssuer) {
		t.Fatalf("login confirmation missing: %q", output)
	}
	for _, buffer := range []*bytes.Buffer{out, errOut} {
		for _, secret := range []string{accessCanary, refreshCanary, deviceCanary, "id-token-canary"} {
			if strings.Contains(buffer.String(), secret) {
				t.Fatalf("login output leaked %q: %q", secret, buffer.String())
			}
		}
	}
}

func TestRunWithDepsLoginModeSelection(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		env         map[string]string
		wantBrowser bool
		wantDevice  bool
		wantCode    int
	}{
		{
			name:       "forced device wins over display",
			args:       []string{"login", "--device"},
			env:        map[string]string{"DISPLAY": ":0"},
			wantDevice: true,
			wantCode:   cli.ExitSuccess,
		},
		{
			name:       "no browser forces device",
			args:       []string{"login", "--no-browser"},
			env:        map[string]string{"DISPLAY": ":0", "WAYLAND_DISPLAY": "wayland-0"},
			wantDevice: true,
			wantCode:   cli.ExitSuccess,
		},
		{
			name:        "display selects browser",
			args:        []string{"login"},
			env:         map[string]string{"DISPLAY": ":0"},
			wantBrowser: true,
			wantCode:    cli.ExitFailure,
		},
		{
			name:        "wayland display selects browser",
			args:        []string{"login"},
			env:         map[string]string{"WAYLAND_DISPLAY": "wayland-0"},
			wantBrowser: true,
			wantCode:    cli.ExitFailure,
		},
		{
			name:       "ssh selects device",
			args:       []string{"login"},
			env:        map[string]string{"DISPLAY": ":0", "SSH_CONNECTION": "10.0.0.1 22 10.0.0.2 22"},
			wantDevice: true,
			wantCode:   cli.ExitSuccess,
		},
		{
			name:       "headless selects device",
			args:       []string{"login"},
			wantDevice: true,
			wantCode:   cli.ExitSuccess,
		},
		{
			name:     "unknown flag",
			args:     []string{"login", "--token"},
			wantCode: cli.ExitUsage,
		},
		{
			name:     "repeated flag",
			args:     []string{"login", "--device", "--device"},
			wantCode: cli.ExitUsage,
		},
		{
			name:     "value form",
			args:     []string{"login", "--device=true"},
			wantCode: cli.ExitUsage,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			oidc := &commandOIDC{document: commandDocument()}
			store := &commandStore{}
			lock := &commandLock{}
			browser := &commandBrowser{}
			runtime := commandRuntime(oidc, store, lock, commandClock{now: testNow()}, testCase.env)
			runtime.Browser = browser
			ports, _, errOut := testPorts(&scriptedTransport{})
			code, out, _ := runWithRuntime(t, commandConfig(), testCase.args, ports, runtime)
			if code != testCase.wantCode {
				t.Fatalf("exit = %d; want %d (stderr=%q)", code, testCase.wantCode, errOut.String())
			}
			if testCase.wantCode == cli.ExitUsage {
				if len(oidc.deviceRequests()) != 0 || len(oidc.tokenRequests()) != 0 || len(browser.opened()) != 0 {
					t.Fatal("a usage refusal performed a login exchange")
				}
				return
			}
			if got := len(browser.opened()); (got > 0) != testCase.wantBrowser {
				t.Fatalf("browser opens = %d; want browser=%t", got, testCase.wantBrowser)
			}
			if got := len(oidc.deviceRequests()); (got > 0) != testCase.wantDevice {
				t.Fatalf("device requests = %d; want device=%t", got, testCase.wantDevice)
			}
			if testCase.wantBrowser {
				opened := browser.opened()[0]
				if !strings.HasPrefix(opened, commandIssuer+"/oauth/authorize?") ||
					!strings.Contains(opened, "response_type=code") ||
					!strings.Contains(opened, "code_challenge_method=S256") {
					t.Fatalf("authorization URL = %q", opened)
				}
				if !strings.Contains(out.String(), opened) {
					t.Fatalf("the fallback URL was not printed: %q", out.String())
				}
			}
			for _, buffer := range []*bytes.Buffer{out, errOut} {
				for _, secret := range []string{accessCanary, refreshCanary, deviceCanary, "id-token-canary"} {
					if strings.Contains(buffer.String(), secret) {
						t.Fatalf("login output leaked %q: %q", secret, buffer.String())
					}
				}
			}
		})
	}
}

func TestRunWithDepsLoginRefusals(t *testing.T) {
	t.Run("discovery failure", func(t *testing.T) {
		oidc := &commandOIDC{document: commandDocument(), discoveryErr: errors.New("discovery down")}
		ports, out, errOut := testPorts(&scriptedTransport{})
		code, _, _ := runWithRuntime(t, commandConfig(), []string{"login", "--device"}, ports,
			commandRuntime(oidc, &commandStore{}, &commandLock{}, commandClock{now: testNow()}, nil))
		if code != cli.ExitFailure || out.Len() != 0 || errOut.Len() == 0 {
			t.Fatalf("exit/stdout/stderr = %d/%q/%q", code, out.String(), errOut.String())
		}
	})
	t.Run("api url mismatch", func(t *testing.T) {
		document := commandDocument()
		document.APIURL = "https://other.example"
		oidc := &commandOIDC{document: document}
		ports, _, errOut := testPorts(&scriptedTransport{})
		code, _, _ := runWithRuntime(t, commandConfig(), []string{"login", "--device"}, ports,
			commandRuntime(oidc, &commandStore{}, &commandLock{}, commandClock{now: testNow()}, nil))
		if code != cli.ExitFailure || !strings.Contains(errOut.String(), "API URL") {
			t.Fatalf("exit/stderr = %d/%q", code, errOut.String())
		}
	})
	t.Run("token failure", func(t *testing.T) {
		oidc := &commandOIDC{document: commandDocument()}
		oidc.tokenFn = func(cli.TokenRequest) (cli.TokenResponse, error) {
			return cli.TokenResponse{}, errors.New("token endpoint down")
		}
		store := &commandStore{}
		ports, out, errOut := testPorts(&scriptedTransport{})
		code, _, _ := runWithRuntime(t, commandConfig(), []string{"login", "--device"}, ports,
			commandRuntime(oidc, store, &commandLock{}, commandClock{now: testNow()}, nil))
		if code != cli.ExitFailure {
			t.Fatalf("exit = %d; want failure", code)
		}
		if _, present := store.current(); present {
			t.Fatal("a failed token exchange persisted a session")
		}
		if strings.Contains(out.String(), accessCanary) || strings.Contains(errOut.String(), refreshCanary) {
			t.Fatal("token failure rendered a secret")
		}
	})
	t.Run("save failure", func(t *testing.T) {
		store := &commandStore{saveErr: faultsUnavailable()}
		ports, out, errOut := testPorts(&scriptedTransport{})
		code, _, _ := runWithRuntime(t, commandConfig(), []string{"login", "--device"}, ports,
			commandRuntime(&commandOIDC{document: commandDocument()}, store, &commandLock{}, commandClock{now: testNow()}, nil))
		if code != cli.ExitFailure || errOut.Len() == 0 {
			t.Fatalf("exit/stderr = %d/%q", code, errOut.String())
		}
		if strings.Contains(out.String(), "logged in") {
			t.Fatalf("a failed save printed the confirmation: %q", out.String())
		}
	})
	t.Run("partial runtime", func(t *testing.T) {
		ports, _, errOut := testPorts(&scriptedTransport{})
		code, _, _ := runWithRuntime(t, commandConfig(), []string{"login", "--device"}, ports,
			cli.LoginRuntime{Store: &commandStore{}, Lock: &commandLock{}})
		if code != cli.ExitFailure || !strings.Contains(errOut.String(), "OIDC") {
			t.Fatalf("exit/stderr = %d/%q", code, errOut.String())
		}
	})
	t.Run("invalid config", func(t *testing.T) {
		config := commandConfig()
		config.ServerURL = "http://example.com"
		ports, _, errOut := testPorts(&scriptedTransport{})
		code, _, _ := runWithRuntime(t, config, []string{"login", "--device"}, ports,
			commandRuntime(&commandOIDC{document: commandDocument()}, &commandStore{}, &commandLock{}, commandClock{now: testNow()}, nil))
		if code != cli.ExitUsage || errOut.Len() == 0 {
			t.Fatalf("exit/stderr = %d/%q", code, errOut.String())
		}
	})
}

func TestRunWithDepsLogoutRevokesThenDeletes(t *testing.T) {
	events := &eventLog{}
	store := &commandStore{present: true, record: commandRecord(testNow().Add(time.Hour)), events: events}
	lock := &commandLock{events: events}
	oidc := &commandOIDC{document: commandDocument(), events: events}
	ports, out, errOut := testPorts(&scriptedTransport{})
	code, _, _ := runWithRuntime(t, commandConfig(), []string{"logout"}, ports,
		commandRuntime(oidc, store, lock, commandClock{now: testNow()}, nil))
	if code != cli.ExitSuccess {
		t.Fatalf("exit = %d; stderr=%q", code, errOut.String())
	}
	if _, present := store.current(); present {
		t.Fatal("logout did not delete the local session")
	}
	revokes := oidc.revokeRequests()
	if len(revokes) != 1 {
		t.Fatalf("revocations = %d; want 1", len(revokes))
	}
	revoke := revokes[0]
	if revoke.Endpoint != commandIssuer+"/oauth/revoke" || revoke.Token != refreshCanary ||
		revoke.ClientID != commandClient || revoke.TokenTypeHint != "refresh_token" {
		t.Fatalf("revocation request = %+v", revoke)
	}
	acquires, _ := lock.counts()
	if acquires != 1 {
		t.Fatalf("lock acquires = %d; want 1", acquires)
	}
	if discovered := oidc.discovered(); len(discovered) != 1 || discovered[0] != commandAPI+api.DiscoveryPath {
		t.Fatalf("discoveries = %v; want exactly the configured API discovery", discovered)
	}
	locks := events.positions("lock")
	loads := events.positions("load")
	revokesAt := events.positions("revoke")
	deletes := events.positions("delete")
	unlocks := events.positions("unlock")
	if len(locks) != 1 || len(loads) < 2 || len(revokesAt) != 1 || len(deletes) != 1 || len(unlocks) != 1 {
		t.Fatalf("event sequence = %v", events.snapshot())
	}
	if !(locks[0] < loads[len(loads)-1] && loads[len(loads)-1] < revokesAt[0] &&
		revokesAt[0] < deletes[0] && deletes[0] < unlocks[0]) {
		t.Fatalf("logout did not run lock -> reload -> revoke -> delete: %v", events.snapshot())
	}
	if strings.Contains(out.String(), refreshCanary) || strings.Contains(errOut.String(), refreshCanary) {
		t.Fatal("logout output rendered the refresh token")
	}
	if !strings.Contains(out.String(), "logged out") {
		t.Fatalf("logout output = %q", out.String())
	}
}

func TestRunWithDepsLogoutKeepsDeletingWhenRevocationFails(t *testing.T) {
	store := &commandStore{present: true, record: commandRecord(testNow().Add(time.Hour))}
	lock := &commandLock{}
	oidc := &commandOIDC{document: commandDocument()}
	oidc.revokeFn = func(cli.RevocationRequest) error {
		return &cli.OAuthError{Code: "temporarily_unavailable", Description: "revocation endpoint down"}
	}
	ports, out, errOut := testPorts(&scriptedTransport{})
	code, _, _ := runWithRuntime(t, commandConfig(), []string{"logout"}, ports,
		commandRuntime(oidc, store, lock, commandClock{now: testNow()}, nil))
	if code != cli.ExitFailure {
		t.Fatalf("exit = %d; want failure", code)
	}
	if _, present := store.current(); present {
		t.Fatal("a failed revocation kept the local session file")
	}
	if errOut.Len() == 0 || !strings.Contains(errOut.String(), "revoke") {
		t.Fatalf("stderr = %q; want a revocation diagnostic", errOut.String())
	}
	for _, buffer := range []*bytes.Buffer{out, errOut} {
		if strings.Contains(buffer.String(), refreshCanary) {
			t.Fatalf("revocation failure rendered the refresh token: %q", buffer.String())
		}
	}
}

func TestRunWithDepsLogoutWithoutSessionIsSuccess(t *testing.T) {
	store := &commandStore{}
	oidc := &commandOIDC{document: commandDocument()}
	ports, out, errOut := testPorts(&scriptedTransport{})
	code, _, _ := runWithRuntime(t, commandConfig(), []string{"logout"}, ports,
		commandRuntime(oidc, store, &commandLock{}, commandClock{now: testNow()}, nil))
	if code != cli.ExitSuccess || errOut.Len() != 0 {
		t.Fatalf("exit/stderr = %d/%q", code, errOut.String())
	}
	if len(oidc.revokeRequests()) != 0 {
		t.Fatal("logout revoked without a session")
	}
	if !strings.Contains(out.String(), "not logged in") {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestRunWithDepsLogoutWithoutOIDCRefusesBoundSession(t *testing.T) {
	store := &commandStore{present: true, record: commandRecord(testNow().Add(time.Hour))}
	oidc := &commandOIDC{document: commandDocument()}
	ports, _, errOut := testPorts(&scriptedTransport{})
	runtime := commandRuntime(oidc, store, &commandLock{}, commandClock{now: testNow()}, nil)
	runtime.OIDC = nil
	code, _, _ := runWithRuntime(t, commandConfig(), []string{"logout"}, ports, runtime)
	if code != cli.ExitFailure || errOut.Len() == 0 {
		t.Fatalf("exit/stderr = %d/%q", code, errOut.String())
	}
	if len(oidc.revokeRequests()) != 0 {
		t.Fatal("a missing OIDC client attempted a revocation")
	}
	if _, present := store.current(); present {
		t.Fatal("a missing OIDC client kept a bound session it could not revoke")
	}
	if !strings.Contains(errOut.String(), "login required") {
		t.Fatalf("stderr = %q; want a login-required refusal", errOut.String())
	}
	if strings.Contains(errOut.String(), refreshCanary) {
		t.Fatal("the refusal rendered the refresh token")
	}
}

func TestRunWithDepsRefreshRotatesTheSession(t *testing.T) {
	store := &commandStore{present: true, record: commandRecord(testNow().Add(30 * time.Second))}
	oidc := &commandOIDC{document: commandDocument()}
	oidc.tokenFn = func(cli.TokenRequest) (cli.TokenResponse, error) {
		return cli.TokenResponse{
			AccessToken:  "fresh-access-canary",
			RefreshToken: "fresh-refresh-canary",
			ExpiresIn:    time.Hour,
			TokenType:    "Bearer",
		}, nil
	}
	ports, out, errOut := testPorts(&scriptedTransport{})
	code, _, _ := runWithRuntime(t, commandConfig(), []string{"refresh"}, ports,
		commandRuntime(oidc, store, &commandLock{}, commandClock{now: testNow()}, nil))
	if code != cli.ExitSuccess {
		t.Fatalf("exit = %d; stderr=%q", code, errOut.String())
	}
	tokens := oidc.tokenRequests()
	if len(tokens) != 1 || tokens[0].GrantType != "refresh_token" ||
		tokens[0].Endpoint != commandIssuer+"/oauth/token" || tokens[0].RefreshToken != refreshCanary {
		t.Fatalf("token requests = %+v", tokens)
	}
	if discovered := oidc.discovered(); len(discovered) != 1 || discovered[0] != commandAPI+api.DiscoveryPath {
		t.Fatalf("discoveries = %v; want exactly the configured API discovery", discovered)
	}
	record, present := store.current()
	if !present || record.Token != "fresh-access-canary" || record.RefreshToken != "fresh-refresh-canary" {
		t.Fatalf("rotated record = %+v", record)
	}
	if !record.ExpiresAt.Equal(testNow().Add(time.Hour)) {
		t.Fatalf("rotated expiry = %s", record.ExpiresAt)
	}
	if !strings.Contains(out.String(), "refreshed") {
		t.Fatalf("stdout = %q", out.String())
	}
	for _, buffer := range []*bytes.Buffer{out, errOut} {
		if strings.Contains(buffer.String(), refreshCanary) || strings.Contains(buffer.String(), "fresh-refresh-canary") {
			t.Fatalf("refresh output rendered a refresh token: %q", buffer.String())
		}
	}
}

func TestRunWithDepsLogoutRefusesForeignBinding(t *testing.T) {
	store := &commandStore{present: true, record: commandRecord(testNow().Add(time.Hour))}
	oidc := &commandOIDC{document: commandDocument()}
	ports, _, errOut := testPorts(&scriptedTransport{})
	runtime := commandRuntime(oidc, store, &commandLock{}, commandClock{now: testNow()}, nil)
	runtime.Issuer = "https://other.example"
	code, _, _ := runWithRuntime(t, commandConfig(), []string{"logout"}, ports, runtime)
	if code != cli.ExitFailure || errOut.Len() == 0 {
		t.Fatalf("exit/stderr = %d/%q", code, errOut.String())
	}
	if len(oidc.revokeRequests()) != 0 {
		t.Fatal("a foreign binding sent the refresh token to the revocation endpoint")
	}
}

func TestRunWithDepsRefreshRefusesForeignBinding(t *testing.T) {
	store := &commandStore{present: true, record: commandRecord(testNow().Add(30 * time.Second))}
	oidc := &commandOIDC{document: commandDocument()}
	ports, _, errOut := testPorts(&scriptedTransport{})
	runtime := commandRuntime(oidc, store, &commandLock{}, commandClock{now: testNow()}, nil)
	runtime.ClientID = "another-client"
	code, _, _ := runWithRuntime(t, commandConfig(), []string{"refresh"}, ports, runtime)
	if code != cli.ExitFailure || errOut.Len() == 0 {
		t.Fatalf("exit/stderr = %d/%q", code, errOut.String())
	}
	if len(oidc.tokenRequests()) != 0 {
		t.Fatal("a foreign binding sent the refresh token to the token endpoint")
	}
}

func TestRunWithDepsRefreshRefusesChangedBinding(t *testing.T) {
	store := &commandStore{present: true, record: commandRecord(testNow().Add(30 * time.Second))}
	oidc := &commandOIDC{document: changedBindingDocument()}
	ports, out, errOut := testPorts(&scriptedTransport{})
	code, _, _ := runWithRuntime(t, commandConfig(), []string{"refresh"}, ports,
		commandRuntime(oidc, store, &commandLock{}, commandClock{now: testNow()}, nil))
	if code != cli.ExitFailure {
		t.Fatalf("exit = %d; want failure", code)
	}
	if len(oidc.discovered()) != 1 {
		t.Fatalf("discoveries = %d; want 1", len(oidc.discovered()))
	}
	if len(oidc.tokenRequests()) != 0 {
		t.Fatal("a changed provider binding sent the refresh token")
	}
	if record, present := store.current(); !present || record.Token != "old-access-canary" {
		t.Fatalf("a refused refresh mutated the record: %+v", record)
	}
	for _, buffer := range []*bytes.Buffer{out, errOut} {
		if strings.Contains(buffer.String(), refreshCanary) || strings.Contains(buffer.String(), accessCanary) {
			t.Fatalf("refusal rendered a token: %q", buffer.String())
		}
	}
}

func TestRunWithDepsRefreshRefusesDiscoveryFailure(t *testing.T) {
	store := &commandStore{present: true, record: commandRecord(testNow().Add(30 * time.Second))}
	oidc := &commandOIDC{document: commandDocument(), discoveryErr: errors.New("discovery down")}
	ports, _, errOut := testPorts(&scriptedTransport{})
	code, _, _ := runWithRuntime(t, commandConfig(), []string{"refresh"}, ports,
		commandRuntime(oidc, store, &commandLock{}, commandClock{now: testNow()}, nil))
	if code != cli.ExitFailure {
		t.Fatalf("exit = %d; want failure", code)
	}
	if len(oidc.tokenRequests()) != 0 {
		t.Fatal("a failed discovery sent the refresh token")
	}
	if !strings.Contains(errOut.String(), "login required") {
		t.Fatalf("stderr = %q; want a login-required refusal", errOut.String())
	}
	if strings.Contains(errOut.String(), refreshCanary) {
		t.Fatal("the refusal rendered the refresh token")
	}
}

func TestRunWithDepsRefreshRefusesAPIURLMismatch(t *testing.T) {
	store := &commandStore{present: true, record: commandRecord(testNow().Add(30 * time.Second))}
	oidc := &commandOIDC{document: otherAPIDocument()}
	ports, _, errOut := testPorts(&scriptedTransport{})
	code, _, _ := runWithRuntime(t, commandConfig(), []string{"refresh"}, ports,
		commandRuntime(oidc, store, &commandLock{}, commandClock{now: testNow()}, nil))
	if code != cli.ExitFailure {
		t.Fatalf("exit = %d; want failure", code)
	}
	if len(oidc.tokenRequests()) != 0 {
		t.Fatal("a discovery bound to another API URL sent the refresh token")
	}
	if !strings.Contains(errOut.String(), "login required") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

func TestRunWithDepsLogoutRefusesChangedBinding(t *testing.T) {
	store := &commandStore{present: true, record: commandRecord(testNow().Add(time.Hour))}
	oidc := &commandOIDC{document: changedBindingDocument()}
	ports, out, errOut := testPorts(&scriptedTransport{})
	code, _, _ := runWithRuntime(t, commandConfig(), []string{"logout"}, ports,
		commandRuntime(oidc, store, &commandLock{}, commandClock{now: testNow()}, nil))
	if code != cli.ExitFailure {
		t.Fatalf("exit = %d; want failure", code)
	}
	if len(oidc.discovered()) != 1 {
		t.Fatalf("discoveries = %d; want 1", len(oidc.discovered()))
	}
	if len(oidc.revokeRequests()) != 0 {
		t.Fatal("a changed provider binding sent the refresh token to revocation")
	}
	if _, present := store.current(); present {
		t.Fatal("an unverifiable binding kept the local session file")
	}
	for _, buffer := range []*bytes.Buffer{out, errOut} {
		if strings.Contains(buffer.String(), refreshCanary) {
			t.Fatalf("refusal rendered the refresh token: %q", buffer.String())
		}
	}
}

func TestRunWithDepsLogoutRefusesDiscoveryFailure(t *testing.T) {
	store := &commandStore{present: true, record: commandRecord(testNow().Add(time.Hour))}
	oidc := &commandOIDC{document: commandDocument(), discoveryErr: errors.New("discovery down")}
	ports, _, errOut := testPorts(&scriptedTransport{})
	code, _, _ := runWithRuntime(t, commandConfig(), []string{"logout"}, ports,
		commandRuntime(oidc, store, &commandLock{}, commandClock{now: testNow()}, nil))
	if code != cli.ExitFailure {
		t.Fatalf("exit = %d; want failure", code)
	}
	if len(oidc.revokeRequests()) != 0 {
		t.Fatal("a failed discovery sent the refresh token to revocation")
	}
	if _, present := store.current(); present {
		t.Fatal("a failed discovery kept the local session file")
	}
	if !strings.Contains(errOut.String(), "login required") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

func TestRunWithDepsLogoutRefusesAPIURLMismatch(t *testing.T) {
	store := &commandStore{present: true, record: commandRecord(testNow().Add(time.Hour))}
	oidc := &commandOIDC{document: otherAPIDocument()}
	ports, _, errOut := testPorts(&scriptedTransport{})
	code, _, _ := runWithRuntime(t, commandConfig(), []string{"logout"}, ports,
		commandRuntime(oidc, store, &commandLock{}, commandClock{now: testNow()}, nil))
	if code != cli.ExitFailure {
		t.Fatalf("exit = %d; want failure", code)
	}
	if len(oidc.revokeRequests()) != 0 {
		t.Fatal("a discovery bound to another API URL sent the refresh token to revocation")
	}
	if _, present := store.current(); present {
		t.Fatal("an unverifiable API binding kept the local session file")
	}
	if !strings.Contains(errOut.String(), "login required") {
		t.Fatalf("stderr = %q; want a login-required refusal", errOut.String())
	}
}

func TestRunWithDepsRefreshWithoutOIDCRefuses(t *testing.T) {
	store := &commandStore{present: true, record: commandRecord(testNow().Add(30 * time.Second))}
	ports, _, errOut := testPorts(&scriptedTransport{})
	runtime := commandRuntime(&commandOIDC{document: commandDocument()}, store, &commandLock{}, commandClock{now: testNow()}, nil)
	runtime.OIDC = nil
	code, _, _ := runWithRuntime(t, commandConfig(), []string{"refresh"}, ports, runtime)
	if code != cli.ExitFailure || errOut.Len() == 0 {
		t.Fatalf("exit/stderr = %d/%q", code, errOut.String())
	}
	if _, present := store.current(); !present {
		t.Fatal("a refused refresh deleted the local session")
	}
	if strings.Contains(errOut.String(), refreshCanary) {
		t.Fatal("the refusal rendered the refresh token")
	}
}

func TestRunWithDepsRefreshFailureRequiresLogin(t *testing.T) {
	store := &commandStore{present: true, record: commandRecord(testNow().Add(-time.Minute))}
	oidc := &commandOIDC{document: commandDocument()}
	oidc.tokenFn = func(cli.TokenRequest) (cli.TokenResponse, error) {
		return cli.TokenResponse{}, &cli.OAuthError{Code: "invalid_grant", Description: "expired"}
	}
	ports, _, errOut := testPorts(&scriptedTransport{})
	code, _, _ := runWithRuntime(t, commandConfig(), []string{"refresh"}, ports,
		commandRuntime(oidc, store, &commandLock{}, commandClock{now: testNow()}, nil))
	if code != cli.ExitFailure || errOut.Len() == 0 {
		t.Fatalf("exit/stderr = %d/%q", code, errOut.String())
	}
	if _, present := store.current(); present {
		t.Fatal("a failed refresh kept the stale session")
	}
}

func TestRunWithDepsStatusRendersNoTokens(t *testing.T) {
	store := &commandStore{present: true, record: commandRecord(testNow().Add(time.Hour))}
	ports, out, errOut := testPorts(&scriptedTransport{})
	runtime := commandRuntime(&commandOIDC{document: commandDocument()}, store, &commandLock{}, commandClock{now: testNow()}, nil)
	code, _, _ := runWithRuntime(t, commandConfig(), []string{"status"}, ports, runtime)
	if code != cli.ExitSuccess || errOut.Len() != 0 {
		t.Fatalf("exit/stderr = %d/%q", code, errOut.String())
	}
	var view map[string]any
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatalf("status JSON: %v (%q)", err, out.String())
	}
	if view["issuer"] != commandIssuer {
		t.Fatalf("status issuer = %v", view["issuer"])
	}
	if !strings.Contains(fmt.Sprint(view["expires_at"]), "2026-02-03") {
		t.Fatalf("status expiry = %v", view["expires_at"])
	}
	if strings.Contains(out.String(), "old-access-canary") || strings.Contains(out.String(), refreshCanary) {
		t.Fatal("status rendered a token")
	}
}

func TestRunHelperUsesRefreshingSessionSource(t *testing.T) {
	store := &commandStore{present: true, record: commandRecord(testNow().Add(30 * time.Second))}
	lock := &commandLock{}
	oidc := &commandOIDC{document: commandDocument()}
	oidc.tokenFn = func(cli.TokenRequest) (cli.TokenResponse, error) {
		return cli.TokenResponse{AccessToken: "fresh-helper-canary", RefreshToken: refreshCanary, ExpiresIn: time.Hour}, nil
	}
	source := cli.SessionCredentialSource{
		Store:  store,
		Lock:   lock,
		OIDC:   oidc,
		APIURL: commandAPI,
		Clock:  commandClock{now: testNow()},
	}
	ports, out, errOut := testPorts(&scriptedTransport{})
	ports.Credential = source
	code := cli.Run(context.Background(), testConfig(), []string{"helper", "get", "https://git.example.com/shop/repo.git"}, ports)
	if code != cli.ExitSuccess || errOut.Len() != 0 {
		t.Fatalf("exit/stderr = %d/%q", code, errOut.String())
	}
	if out.String() != "username=kodama\npassword=fresh-helper-canary\n" {
		t.Fatalf("helper output = %q", out.String())
	}
	if len(oidc.tokenRequests()) != 1 {
		t.Fatalf("helper refreshes = %d; want 1", len(oidc.tokenRequests()))
	}

	untrusted, untrustedOut, _ := testPorts(&scriptedTransport{})
	untrusted.Credential = source
	if code := cli.Run(context.Background(), testConfig(), []string{"helper", "get", "https://evil.example/repo.git"}, untrusted); code != cli.ExitSuccess {
		t.Fatalf("untrusted helper exit = %d", code)
	}
	if untrustedOut.Len() != 0 {
		t.Fatalf("untrusted helper output = %q; want empty", untrustedOut.String())
	}

	stored, storedOut, _ := testPorts(&scriptedTransport{})
	stored.Credential = source
	if code := cli.Run(context.Background(), testConfig(), []string{"helper", "store", "https://git.example.com/shop/repo.git"}, stored); code != cli.ExitSuccess {
		t.Fatalf("store helper exit = %d", code)
	}
	if storedOut.Len() != 0 {
		t.Fatalf("store helper output = %q; want empty", storedOut.String())
	}

	// The helper port keeps the same empty refusal for a source that has to
	// report "login required".
	failing := cli.SessionCredentialSource{Store: &commandStore{}, Lock: lock, OIDC: oidc, APIURL: commandAPI, Clock: commandClock{now: testNow()}}
	missingPorts, missingOut, _ := testPorts(&scriptedTransport{})
	missingPorts.Credential = failing
	if code := cli.Run(context.Background(), testConfig(), []string{"helper", "get", "https://git.example.com/shop/repo.git"}, missingPorts); code != cli.ExitSuccess {
		t.Fatalf("missing-session helper exit = %d", code)
	}
	if missingOut.Len() != 0 {
		t.Fatalf("missing-session helper output = %q; want empty", missingOut.String())
	}
}

func faultsUnavailable() error {
	return fault.New(fault.DependencyUnavailable, "disk unavailable")
}

var _ client.CredentialSource = cli.SessionCredentialSource{}
