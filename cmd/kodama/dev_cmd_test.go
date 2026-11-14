package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/userland/cli"
	"github.com/optimiware/kodama/internal/userland/up"
)

func TestKodamaDevSingleOriginOIDCRESTAndMCP(t *testing.T) {
	home := t.TempDir()
	stateDir := filepath.Join(home, ".kodama", "dev")

	start := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	readyCh := make(chan devReadyInfo, 1)
	doneCh := make(chan int, 1)
	var stdout, stderr lockedBuffer

	go func() {
		doneCh <- runDev(ctx, []string{
			"--listen", "127.0.0.1:0",
			"--state-dir", stateDir,
			"--tenant", "tenant-local",
			"--project", "project-local",
		}, &stdout, &stderr, devDeps{
			homeDir: func() (string, error) { return home, nil },
			onReady: func(info devReadyInfo) { readyCh <- info },
		})
	}()

	var ready devReadyInfo
	select {
	case ready = <-readyCh:
	case code := <-doneCh:
		t.Fatalf("kodama dev exited before readiness (code %d): %s", code, stderr.String())
	case <-time.After(5 * time.Second):
		t.Fatalf("kodama dev did not start in time; stderr=%s", stderr.String())
	}

	bootDuration := time.Since(start)
	if bootDuration >= 2*time.Second {
		t.Errorf("kodama dev boot took %s; want <2s", bootDuration)
	}

	// Verify startup banner, admin-token (0600) and env.sh (0600).
	banner := stdout.String()
	for _, want := range []string{
		ready.PublicURL,
		ready.PublicURL + "/mcp",
		"source ~/.kodama/dev/env.sh",
		"kodactl --server " + ready.PublicURL + " login",
	} {
		if !strings.Contains(banner, want) {
			t.Fatalf("banner missing %q:\n%s", want, banner)
		}
	}

	adminTokInfo, err := os.Stat(ready.AdminTokenPath)
	if err != nil {
		t.Fatalf("stat admin-token: %v", err)
	}
	if adminTokInfo.Mode().Perm() != 0o600 {
		t.Fatalf("admin-token mode = %04o; want 0600", adminTokInfo.Mode().Perm())
	}
	adminTokenBytes, err := os.ReadFile(ready.AdminTokenPath)
	if err != nil {
		t.Fatal(err)
	}
	adminToken := strings.TrimSpace(string(adminTokenBytes))
	if strings.Count(adminToken, ".") != 2 {
		t.Fatalf("admin-token is not a compact JWS: %q", adminToken)
	}

	envBytes, err := os.ReadFile(ready.EnvFilePath)
	if err != nil {
		t.Fatal(err)
	}
	tenantAdminPath := filepath.Join(filepath.Dir(ready.AdminTokenPath), "tenant-admin-token")
	tenantAdminInfo, err := os.Stat(tenantAdminPath)
	if err != nil {
		t.Fatalf("stat tenant-admin-token: %v", err)
	}
	if tenantAdminInfo.Mode().Perm() != 0o600 {
		t.Fatalf("tenant-admin-token mode = %04o; want 0600", tenantAdminInfo.Mode().Perm())
	}
	for _, key := range []string{"KODAMA_URL=", "KODAMA_CA_FILE=", "KODAMA_TOKEN=", "KODAMA_PROJECT='project-local'", "KODAMA_TENANT_ADMIN_CREDENTIAL_FILE="} {
		if !strings.Contains(string(envBytes), key) {
			t.Fatalf("env.sh missing %q:\n%s", key, envBytes)
		}
	}

	// Build TLS client trusting only the generated strict RFC 5280 CA.
	caPEM, err := os.ReadFile(ready.CACertPath)
	if err != nil {
		t.Fatal(err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		t.Fatal("generated CA PEM could not be appended")
	}
	tlsTransport := &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:    caPool,
			MinVersion: tls.VersionTLS12,
		},
	}
	defer tlsTransport.CloseIdleConnections()
	httpClient := &http.Client{Transport: tlsTransport, Timeout: 5 * time.Second}
	noRedirectClient := &http.Client{
		Transport: tlsTransport,
		Timeout:   5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// 1. Verify GET /.well-known/kodama-configuration.
	discResp, err := httpClient.Get(ready.PublicURL + api.DiscoveryPath)
	if err != nil {
		t.Fatalf("GET discovery failed: %v", err)
	}
	discBody, _ := io.ReadAll(discResp.Body)
	_ = discResp.Body.Close()
	if discResp.StatusCode != http.StatusOK {
		t.Fatalf("discovery status = %d: %s", discResp.StatusCode, discBody)
	}
	discDoc, err := api.DecodeDiscoveryDocument(discBody)
	if err != nil {
		t.Fatalf("DecodeDiscoveryDocument failed: %v\n%s", err, discBody)
	}
	if discDoc.Issuer != ready.PublicURL+"/auth" || discDoc.APIURL != ready.PublicURL {
		t.Fatalf("unexpected discovery document: %+v", discDoc)
	}

	// 2. Verify OIDC metadata, JWKS and full PKCE S256 login flow on dynamic loopback port.
	oidcClient := &cli.HTTPOIDCClient{RoundTripper: tlsTransport}
	jwksRaw, err := oidcClient.JWKS(context.Background(), discDoc.Issuer)
	if err != nil {
		t.Fatalf("OIDC JWKS discovery failed: %v", err)
	}

	verifier := "e9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM12345"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	callbackURI := "http://127.0.0.1:54987/callback"

	authURL, _ := url.Parse(discDoc.AuthorizationEndpoint)
	q := authURL.Query()
	q.Set("response_type", "code")
	q.Set("client_id", discDoc.ClientID)
	q.Set("redirect_uri", callbackURI)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", "test-state-123")
	q.Set("nonce", "test-nonce-456")
	q.Set("scope", strings.Join(discDoc.Scopes, " "))
	authURL.RawQuery = q.Encode()

	authResp, err := noRedirectClient.Get(authURL.String())
	if err != nil {
		t.Fatalf("GET /auth/authorize: %v", err)
	}
	_ = authResp.Body.Close()
	if authResp.StatusCode != http.StatusFound {
		t.Fatalf("/auth/authorize status = %d; want 302", authResp.StatusCode)
	}
	redirectLoc, err := url.Parse(authResp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	code := redirectLoc.Query().Get("code")
	if code == "" || redirectLoc.Query().Get("state") != "test-state-123" {
		t.Fatalf("unexpected callback redirect: %s", redirectLoc)
	}

	tokResp, err := oidcClient.Token(context.Background(), cli.TokenRequest{
		Endpoint:     discDoc.TokenEndpoint,
		GrantType:    "authorization_code",
		Code:         code,
		RedirectURI:  callbackURI,
		ClientID:     discDoc.ClientID,
		CodeVerifier: verifier,
	})
	if err != nil {
		t.Fatalf("OIDC Token exchange failed: %v", err)
	}
	if err := cli.VerifyIDToken(tokResp.IDToken, jwksRaw, cli.IDTokenExpectations{
		Issuer:   discDoc.Issuer,
		Audience: discDoc.ClientID,
		Nonce:    "test-nonce-456",
		Now:      time.Now(),
	}); err != nil {
		t.Fatalf("VerifyIDToken failed: %v", err)
	}

	// 3. Verify authenticated /api/v1/... calls with both admin-token and PKCE access_token.
	for _, tok := range []string{adminToken, tokResp.AccessToken} {
		req, _ := http.NewRequest(http.MethodGet, ready.PublicURL+"/api/v1/projects/project-local/executions", bytes.NewReader([]byte(`{"input":{}}`)))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		resp, err := httpClient.Do(req)
		if err != nil {
			t.Fatalf("GET /api/v1/projects/project-local/executions failed: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("executions list status = %d: %s", resp.StatusCode, body)
		}
	}

	// The default credential is project-scoped, so it can create a task.
	taskReq, _ := http.NewRequest(http.MethodPost, ready.PublicURL+"/api/v1/projects/project-local/tasks",
		strings.NewReader(`{"input":{"project_id":"project-local","title":"first task"},"idempotency_key":"dev-test-task-1"}`))
	taskReq.Header.Set("Authorization", "Bearer "+adminToken)
	taskReq.Header.Set("Content-Type", "application/json")
	taskResp, err := httpClient.Do(taskReq)
	if err != nil {
		t.Fatalf("POST tasks failed: %v", err)
	}
	taskBody, _ := io.ReadAll(taskResp.Body)
	_ = taskResp.Body.Close()
	if taskResp.StatusCode != http.StatusOK {
		t.Fatalf("task create with the default credential: status = %d: %s", taskResp.StatusCode, taskBody)
	}

	// Check pre-bootstrapped memberships (admin, engineer, reviewer, operator).
	memReq, _ := http.NewRequest(http.MethodGet, ready.PublicURL+"/api/v1/projects/project-local/memberships", bytes.NewReader([]byte(`{"input":{"project_id":"project-local"}}`)))
	// Memberships are a tenant operation: they need the tenant-scoped credential.
	tenantAdminBytes, err := os.ReadFile(tenantAdminPath)
	if err != nil {
		t.Fatal(err)
	}
	memReq.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tenantAdminBytes)))
	memReq.Header.Set("Content-Type", "application/json")
	memResp, err := httpClient.Do(memReq)
	if err != nil {
		t.Fatalf("GET memberships failed: %v", err)
	}
	memBody, _ := io.ReadAll(memResp.Body)
	_ = memResp.Body.Close()
	if memResp.StatusCode != http.StatusOK {
		t.Fatalf("memberships status = %d: %s", memResp.StatusCode, memBody)
	}
	for _, role := range []string{`"admin"`, `"engineer"`, `"reviewer"`, `"operator"`} {
		if !strings.Contains(string(memBody), role) {
			t.Fatalf("bootstrapped memberships missing role %s: %s", role, memBody)
		}
	}

	// 4. Verify authenticated /mcp JSON-RPC calls over TLS.
	mcpPayload := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"execution.list","arguments":{"input":{"project_id":"project-local"}}}}`
	mcpReq, _ := http.NewRequest(http.MethodPost, ready.PublicURL+"/mcp", strings.NewReader(mcpPayload))
	mcpReq.Header.Set("Authorization", "Bearer "+tokResp.AccessToken)
	mcpReq.Header.Set("Content-Type", "application/json")
	mcpResp, err := httpClient.Do(mcpReq)
	if err != nil {
		t.Fatalf("POST /mcp failed: %v", err)
	}
	mcpBody, _ := io.ReadAll(mcpResp.Body)
	_ = mcpResp.Body.Close()
	var mcpResult struct {
		JSONRPC string          `json:"jsonrpc"`
		Result  json.RawMessage `json:"result"`
		Error   any             `json:"error"`
	}
	if err := json.Unmarshal(mcpBody, &mcpResult); err != nil {
		t.Fatalf("decode MCP response: %v\n%s", err, mcpBody)
	}
	if mcpResp.StatusCode != http.StatusOK || mcpResult.Error != nil || len(mcpResult.Result) == 0 {
		t.Fatalf("MCP tools/call failed (%d): %s", mcpResp.StatusCode, mcpBody)
	}

	// Stop first run and verify clean exit 0 + CA/OIDC key reuse on restart.
	cancel()
	if exitCode := <-doneCh; exitCode != 0 {
		t.Fatalf("kodama dev exited with %d; stderr=%s", exitCode, stderr.String())
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	ready2Ch := make(chan devReadyInfo, 1)
	done2Ch := make(chan int, 1)
	go func() {
		done2Ch <- runDev(ctx2, []string{"--listen", "127.0.0.1:0", "--state-dir", stateDir}, io.Discard, io.Discard, devDeps{
			homeDir: func() (string, error) { return home, nil },
			onReady: func(info devReadyInfo) { ready2Ch <- info },
		})
	}()
	ready2 := <-ready2Ch
	caPEM2, err := os.ReadFile(ready2.CACertPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(caPEM, caPEM2) {
		t.Fatal("kodama dev regenerated CA instead of reusing <state-dir>/tls/ca.crt on restart")
	}
	cancel2()
	if exitCode := <-done2Ch; exitCode != 0 {
		t.Fatalf("second kodama dev exited with %d", exitCode)
	}
}

func TestUpDownStatusCLICommands(t *testing.T) {
	origUp, origDown, origStatus := runUpStack, runDownStack, runStatusStack
	t.Cleanup(func() {
		runUpStack = origUp
		runDownStack = origDown
		runStatusStack = origStatus
	})

	var capturedUp up.UpConfig
	runUpStack = func(_ context.Context, cfg up.UpConfig, out io.Writer) (up.StackDescriptor, error) {
		capturedUp = cfg
		_, _ = io.WriteString(out, "up-ok\n")
		return up.StackDescriptor{Instance: cfg.Instance}, nil
	}
	var capturedDown up.DownConfig
	runDownStack = func(_ context.Context, cfg up.DownConfig, out io.Writer) error {
		capturedDown = cfg
		_, _ = io.WriteString(out, "down-ok\n")
		return nil
	}
	runStatusStack = func(_ context.Context, cfg up.StatusConfig) (up.StatusReport, error) {
		return up.StatusReport{
			Instance:     cfg.Instance,
			GatewayAlive: true,
			Ready:        true,
			Containers: map[string]up.ContainerInfo{
				"postgres": {Running: true, Status: "running"},
				"nats":     {Running: true, Status: "running"},
				"server":   {Running: true, Status: "running"},
				"git":      {Running: true, Status: "running"},
			},
		}, nil
	}

	var out, errBuf bytes.Buffer
	if code := runUpEntry(context.Background(), []string{"--name", "demo", "--listen", "127.0.0.1:9443"}, &out, &errBuf); code != 0 {
		t.Fatalf("runUpEntry exit = %d: %s", code, errBuf.String())
	}
	if capturedUp.Instance != "demo" || capturedUp.ListenAddr != "127.0.0.1:9443" {
		t.Fatalf("capturedUp = %+v", capturedUp)
	}

	out.Reset()
	if code := runStatusEntry(context.Background(), []string{"--name", "demo", "--json"}, &out, &errBuf); code != 0 {
		t.Fatalf("runStatusEntry exit = %d: %s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), `"instance": "demo"`) {
		t.Fatalf("unexpected status JSON: %s", out.String())
	}

	out.Reset()
	if code := runDownEntry(context.Background(), []string{"--name", "demo", "--purge"}, &out, &errBuf); code != 0 {
		t.Fatalf("runDownEntry exit = %d: %s", code, errBuf.String())
	}
	if !capturedDown.Purge || capturedDown.Instance != "demo" {
		t.Fatalf("capturedDown = %+v", capturedDown)
	}
}
