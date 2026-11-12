//go:build component

// This file proves the installable `test` profile end to end: the real
// `kodama` binary runs `migrate`, `bootstrap-admin --profile test
// --init-keyfile` with a signed tenant-wide OIDC token, and `server --profile
// test` over real PostgreSQL, JetStream NATS and a private Podman API service.
// One inline pipeline runs in a real Podman container that reaches the server
// through the workload URL with its mounted credential and submits a declared
// output; a graceful restart keeps the service credential and the terminal
// execution readable. The signed OIDC fixture stands in for OAuthSonas;
// interactive sign-in is outside this test.
package composition

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/id"
	runtimepodman "github.com/optimiware/kodama/internal/shell/runtime/podman"
	"github.com/optimiware/kodama/test/support/boundaries/identity"
	"github.com/optimiware/kodama/test/support/infrastructure/messaging"
	pgtest "github.com/optimiware/kodama/test/support/infrastructure/postgres"
	runtimesupport "github.com/optimiware/kodama/test/support/infrastructure/runtime"
)

const (
	testProfileTenant     = "tenant-profile"
	testProfileProject    = "project-profile"
	testProfileAdmin      = "profile-admin"
	testProfileService    = "profile-engineer"
	testProfileOutput     = "profile_result"
	testProfileOutputVal  = "profile-output-ok"
	testProfileLogMarker  = "profile-workload-log-marker"
	testProfileKeyRef     = "keyfile:kek-v1"
	testProfileInstall    = "installation-profile"
	testProfileDatabase   = "kodama_profile"
	testProfileHostAlias  = "host.containers.internal"
	testProfileAPITimeout = 10 * time.Second
)

// testProfileDefaultRouteHost returns the host's IPv4 default-route address:
// rootless Podman (pasta) maps host.containers.internal to it, never to
// loopback.
func testProfileDefaultRouteHost(t *testing.T) string {
	t.Helper()
	connection, err := net.Dial("udp4", "1.1.1.1:53")
	if err != nil {
		t.Fatalf("resolve the default-route address for the workload edge: %v", err)
	}
	defer connection.Close()
	address, ok := connection.LocalAddr().(*net.UDPAddr)
	if !ok || address.IP == nil || address.IP.IsLoopback() || address.IP.IsUnspecified() {
		t.Fatalf("default-route address %v is not a specific interface address", connection.LocalAddr())
	}
	return address.IP.String()
}

func testProfileFreePort(t *testing.T, host string) string {
	t.Helper()
	listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatalf("reserve a server port: %v", err)
	}
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	_ = listener.Close()
	return port
}

// testProfileTLS writes one private CA and a server certificate for the
// workload alias and the host address; it returns the CA PEM.
func testProfileTLS(t *testing.T, dir, host string) []byte {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Kodama Test Profile CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(6 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: testProfileHostAlias},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(6 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{testProfileHostAlias},
		IPAddresses: []net.IP{net.ParseIP(host)},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(leafKey)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	for name, data := range map[string][]byte{
		"ca.crt":  caPEM,
		"tls.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		"tls.key": pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return caPEM
}

func testProfileSecret(t *testing.T) string {
	t.Helper()
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(buffer)
}

// testProfileRun runs one short kodama command and returns its streams and
// exit code.
func testProfileRun(t *testing.T, binary string, env map[string]string, timeout time.Duration, args ...string) (string, string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, binary, args...)
	command.Env = commandEnvironment(env)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	if ctx.Err() != nil {
		t.Fatalf("kodama %s timed out after %s", args[0], timeout)
	}
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("run kodama %s: %v", args[0], err)
		}
		code = exit.ExitCode()
	}
	return stdout.String(), stderr.String(), code
}

// testProfileServer is one running `kodama server --profile test` process.
type testProfileServer struct {
	command *exec.Cmd
	output  *lockedOutput
	done    chan error
	exited  bool
	exitErr error
}

type lockedOutput struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (output *lockedOutput) Write(data []byte) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.buffer.Write(data)
}

func (output *lockedOutput) String() string {
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.buffer.String()
}

func testProfileStartServer(t *testing.T, binary string, env map[string]string) *testProfileServer {
	t.Helper()
	command := exec.Command(binary, "server", "--profile", "test")
	command.Env = commandEnvironment(env)
	output := &lockedOutput{}
	command.Stdout, command.Stderr = output, output
	if err := command.Start(); err != nil {
		t.Fatalf("start kodama server: %v", err)
	}
	server := &testProfileServer{command: command, output: output, done: make(chan error, 1)}
	go func() { server.done <- command.Wait() }()
	t.Cleanup(func() { server.stop(t, false) })
	return server
}

// stop sends SIGTERM and joins within the graceful bound, killing and
// joining as a fallback. A graceful stop must exit 0.
func (server *testProfileServer) stop(t *testing.T, requireGraceful bool) {
	t.Helper()
	if !server.exited {
		_ = server.command.Process.Signal(syscall.SIGTERM)
		select {
		case server.exitErr = <-server.done:
		case <-time.After(15 * time.Second):
			_ = server.command.Process.Kill()
			server.exitErr = <-server.done
			if requireGraceful {
				server.exited = true
				t.Fatalf("server did not drain within 15s\n%s", server.output.String())
			}
		}
		server.exited = true
	}
	if requireGraceful && server.exitErr != nil {
		t.Fatalf("server did not exit cleanly on SIGTERM: %v\n%s", server.exitErr, server.output.String())
	}
}

func testProfileWaitReady(t *testing.T, server *testProfileServer, httpClient *http.Client, base string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-server.done:
			server.exited, server.exitErr = true, err
			t.Fatalf("kodama server exited before ready: %v\n%s", err, server.output.String())
		default:
		}
		response, err := httpClient.Get(base + "/readyz")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("kodama server was not ready within 60s\n%s", server.output.String())
}

type testProfileCredential struct {
	kind  client.CredentialKind
	token string
}

func (source testProfileCredential) Credential(context.Context) (client.Credential, error) {
	return client.Credential{Kind: source.kind, Token: source.token, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

type testProfileAPI struct {
	base      string
	transport http.RoundTripper
}

func (instance testProfileAPI) call(t *testing.T, credential testProfileCredential, operation string, input any, key string, version uint64) (api.Result, error) {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	request := api.Request{Input: raw, IdempotencyKey: key}
	if version != 0 {
		request.ExpectedVersion = &version
	}
	transport := &client.HTTPTransport{
		BaseURL:      instance.base,
		RoundTripper: instance.transport,
		Source:       credential,
		Retry:        client.RetryPolicy{MaxAttempts: 1, BaseDelay: 100 * time.Millisecond, MaxDelay: time.Second},
	}
	ctx, cancel := context.WithTimeout(context.Background(), testProfileAPITimeout)
	defer cancel()
	return transport.Invoke(ctx, api.Invocation{Operation: operation, Request: request})
}

func testProfileDo[T any](t *testing.T, instance testProfileAPI, credential testProfileCredential, operation string, input any, key string, version uint64) T {
	t.Helper()
	result, err := instance.call(t, credential, operation, input, key, version)
	return testProfileDecode[T](t, result, err, operation)
}

func testProfileDecode[T any](t *testing.T, result api.Result, err error, operation string) T {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", operation, err)
	}
	var decoded T
	if err := json.Unmarshal(result.Body, &decoded); err != nil {
		t.Fatalf("decode %s: %v", operation, err)
	}
	return decoded
}

// testProfileWorkloadCommands write the public CA inside the workspace, prove
// the mounted execution credential authenticates through $KODAMA_URL, submit
// the declared output with a 409 retry from a fresh inspection and print one
// log marker. Nothing but public status codes and the marker is printed.
func testProfileWorkloadCommands(caPEM []byte) []string {
	const token = "/kodama/secrets/KODAMA_TOKEN"
	const ca = "/workspace/profile-ca.crt"
	return []string{
		"for tool in curl grep head cut cat; do command -v \"$tool\" >/dev/null 2>&1 || { echo \"profile: missing $tool\" >&2; exit 1; }; done",
		"cat > " + ca + " <<'PROFILE_CA'\n" + string(caPEM) + "PROFILE_CA",
		"test -s " + token,
		"echo profile-workspace-data > /workspace/profile.txt",
		`attempt=0; submitted=0; while [ "$attempt" -lt 10 ]; do attempt=$((attempt+1)); ` +
			`INSPECT=$(curl -sS --fail -X GET --cacert ` + ca + ` -H "Authorization: Bearer $(cat ` + token + `)" -H 'Content-Type: application/json' --data-binary '{}' "$KODAMA_URL/api/v1/executions/$KODAMA_EXECUTION") || { echo "profile: inspection refused" >&2; exit 1; }; ` +
			`VERSION=$(printf '%s' "$INSPECT" | grep -o '"version":[0-9]*' | head -n 1 | cut -d: -f2); ` +
			`ACTION=$(printf '%s' "$INSPECT" | grep -o '"action_id":"[^"]*"' | head -n 1 | cut -d'"' -f4); ` +
			`test -n "$VERSION" && test -n "$ACTION" || { echo "profile: inspection carried no version or action" >&2; exit 1; }; ` +
			`BODY=$(printf '{"input":{"outputs":{"` + testProfileOutput + `":"` + testProfileOutputVal + `"}},"expected_version":%s,"idempotency_key":"profile-out-%s"}' "$VERSION" "$attempt"); ` +
			`CODE=$(curl -sS -X PUT -o /dev/null -w '%{http_code}' --cacert ` + ca + ` -H "Authorization: Bearer $(cat ` + token + `)" -H 'Content-Type: application/json' --data-binary "$BODY" "$KODAMA_URL/api/v1/executions/$KODAMA_EXECUTION/actions/$ACTION/outputs"); ` +
			`case "$CODE" in 200) submitted=1; break ;; 409) continue ;; *) echo "profile: output submission status $CODE" >&2; exit 1 ;; esac; done`,
		`test "$submitted" = "1" || { echo "profile: output submission did not converge" >&2; exit 1; }`,
		"echo " + testProfileLogMarker,
	}
}

func TestKodamaTestProfileRealPodman(t *testing.T) {
	shard := pgtest.Start(t)
	broker := messaging.StartNATS(t)
	podman := runtimesupport.StartService(t)
	imageCtx, cancelImage := context.WithTimeout(context.Background(), 5*time.Minute)
	image := runtimesupport.ResolveImage(imageCtx, t, podman.Podman(), messaging.FixtureBaseTag)
	cancelImage()
	provider, err := identity.NewProvider(identity.OIDCOptions{Clock: identity.NewClock(time.Now())})
	if err != nil {
		t.Fatalf("start the OIDC fixture: %v", err)
	}
	t.Cleanup(provider.Close)
	binary := kodamaCLI(t)

	// Isolated installation directories; the data directory is daemon-visible
	// because the Podman service runs on this host as this user.
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	tlsDir := filepath.Join(root, "tls")
	keyfile := filepath.Join(root, "keys", "kek-v1.key")
	for _, dir := range []string{dataDir, tlsDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	hostIP := testProfileDefaultRouteHost(t)
	port := testProfileFreePort(t, hostIP)
	caPEM := testProfileTLS(t, tlsDir, hostIP)
	oidcCA := filepath.Join(root, "oidc-ca.crt")
	if err := os.WriteFile(oidcCA, provider.CACertPEM(), 0o600); err != nil {
		t.Fatal(err)
	}

	adminPassword := testProfileSecret(t)
	if _, err := pgtest.ExecSQL(context.Background(), shard.Admin(),
		`CREATE ROLE kodama_admin LOGIN CREATEROLE CREATEDB NOSUPERUSER NOBYPASSRLS PASSWORD '`+adminPassword+`'`); err != nil {
		t.Fatalf("create the non-superuser administrator: %v", err)
	}
	dbHost, dbPort := hostPort(t, shard.Addr)
	env := map[string]string{
		"KODAMA_PROFILE":            "test",
		"KODAMA_ADMIN_DATABASE_URL": roleURL(t, dbHost, dbPort, "kodama_admin", adminPassword, "postgres"),
		"KODAMA_NATS_URL":           broker.URL,
		"KODAMA_PODMAN_SOCKET":      podman.Socket(),
		"KODAMA_KEY_ADAPTER":        "keyfile",
		"KODAMA_KEY_REF":            testProfileKeyRef,
		"KODAMA_KEYFILE":            keyfile,
		"KODAMA_TLS_DIR":            tlsDir,
		"KODAMA_OIDC_ISSUER":        provider.Issuer(),
		"KODAMA_OIDC_AUDIENCE":      provider.Audience(),
		"KODAMA_OIDC_JWKS_URL":      provider.JWKSURL(),
		"KODAMA_OIDC_CA_FILE":       oidcCA,
		"KODAMA_OIDC_TENANT_CLAIM":  provider.TenantClaim(),
		"KODAMA_OIDC_SUBJECT_CLAIM": provider.SubjectClaim(),
		"KODAMA_OIDC_PROJECT_CLAIM": provider.ProjectClaim(),
		"KODAMA_INSTALLATION_ID":    testProfileInstall,
		"KODAMA_DATA_DIR":           dataDir,
		"KODAMA_ADDRESS":            net.JoinHostPort(hostIP, port),
		"KODAMA_PUBLIC_URL":         "https://" + net.JoinHostPort(testProfileHostAlias, port),
		"KODAMA_WORKLOAD_URL":       "https://" + net.JoinHostPort(testProfileHostAlias, port),
	}
	var passwords []string
	for _, role := range []struct{ env, name string }{
		{"KODAMA_DATABASE_URL", "kodama_runtime"},
		{"KODAMA_CONTROLLER_DATABASE_URL", "kodama_controller"},
		{"KODAMA_PUBLISHER_DATABASE_URL", "kodama_publisher"},
		{"KODAMA_AUTH_DATABASE_URL", "kodama_auth"},
	} {
		password := testProfileSecret(t)
		passwords = append(passwords, password)
		env[role.env] = roleURL(t, dbHost, dbPort, role.name, password, testProfileDatabase)
	}
	assertSecretFree := func(label, text string) {
		t.Helper()
		for _, secretValue := range append([]string{adminPassword}, passwords...) {
			if strings.Contains(text, secretValue) {
				t.Fatalf("%s rendered a database password", label)
			}
		}
	}

	// 1. migrate is idempotent and never touches the keyfile.
	for run, want := range []string{"migration(s) applied", "0 migration(s) applied"} {
		stdout, stderr, code := testProfileRun(t, binary, env, 5*time.Minute, "migrate")
		if code != 0 || !strings.Contains(stdout, want) {
			t.Fatalf("migrate run %d exit %d stdout %q stderr %q", run+1, code, stdout, stderr)
		}
		assertSecretFree("migrate", stdout+stderr)
		if _, err := os.Lstat(keyfile); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("migrate created a keyfile: %v", err)
		}
	}

	// 2. bootstrap-admin initializes the key explicitly and creates the first
	// tenant from a signed tenant-wide token; a repeat is refused and keeps
	// the key.
	adminToken, err := provider.Mint(identity.TokenSpec{
		Subject: testProfileAdmin, Tenant: testProfileTenant,
		Mutate: func(claims map[string]any) { delete(claims, provider.ProjectClaim()) },
	})
	if err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(root, "admin-token")
	if err := os.WriteFile(tokenFile, []byte(adminToken.Value), 0o600); err != nil {
		t.Fatal(err)
	}
	bootstrapArgs := []string{"bootstrap-admin", "--profile", "test", "--tenant", testProfileTenant, "--tenant-name", "Test Profile Tenant", "--token-file", tokenFile}
	stdout, stderr, code := testProfileRun(t, binary, env, 30*time.Second, append(bootstrapArgs, "--init-keyfile")...)
	// The OIDC subject maps to a derived principal identity, not the raw
	// subject, so only the tenant line and a non-empty identity are pinned.
	if code != 0 || !strings.HasPrefix(stdout, "tenant "+testProfileTenant+" created; first administrator ") ||
		strings.TrimSpace(strings.TrimPrefix(stdout, "tenant "+testProfileTenant+" created; first administrator ")) == "" {
		t.Fatalf("bootstrap exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if strings.Contains(stdout+stderr, adminToken.Value) {
		t.Fatal("bootstrap rendered the access token")
	}
	info, err := os.Lstat(keyfile)
	if err != nil || info.Mode().Perm() != 0o600 || info.Size() != 32 {
		t.Fatalf("initialized keyfile = %v, %v", info, err)
	}
	key, _ := os.ReadFile(keyfile)
	_, stderr, code = testProfileRun(t, binary, env, 30*time.Second, bootstrapArgs...)
	if code != 1 {
		t.Fatalf("repeated bootstrap exit %d (stderr %q); want the existing-tenant refusal", code, stderr)
	}
	if again, _ := os.ReadFile(keyfile); !bytes.Equal(key, again) {
		t.Fatal("a repeated bootstrap changed the keyfile")
	}

	// 3. server --profile test becomes ready over HTTPS.
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	httpTransport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	httpClient := &http.Client{Transport: httpTransport, Timeout: 5 * time.Second}
	base := "https://" + net.JoinHostPort(hostIP, port)
	server := testProfileStartServer(t, binary, env)
	testProfileWaitReady(t, server, httpClient, base)
	instance := testProfileAPI{base: base, transport: httpTransport}
	admin := testProfileCredential{kind: client.KindSession, token: adminToken.Value}

	result, err := instance.call(t, admin, "project.create", api.ProjectCreateRequest{TenantID: testProfileTenant, ProjectID: testProfileProject, Name: "Test Profile Project"}, "profile-project", 0)
	testProfileDecode[api.ProjectCreateResult](t, result, err, "project.create")
	// Project commands act in project scope: the same human presents a
	// project-scoped token and holds Admin through the tenant-wide membership
	// bootstrap created.
	projectToken, err := provider.Mint(identity.TokenSpec{Subject: testProfileAdmin, Tenant: testProfileTenant, Project: testProfileProject})
	if err != nil {
		t.Fatal(err)
	}
	projectAdmin := testProfileCredential{kind: client.KindSession, token: projectToken.Value}
	created := testProfileDo[api.ServiceCreateResult](t, instance, projectAdmin, "service.create", api.ServiceCreateRequest{ProjectID: testProfileProject, ServiceID: testProfileService, Roles: []string{"engineer"}}, "profile-service", 0)
	issued := testProfileDo[api.ServiceCredentialIssueResult](t, instance, projectAdmin, "service.credential.issue", api.ServiceCredentialIssueRequest{ServiceID: testProfileService, TTLSeconds: 3600}, "profile-issue", created.Version)
	if issued.Token == nil || *issued.Token == "" {
		t.Fatal("service.credential.issue returned no token")
	}
	service := testProfileCredential{kind: client.KindService, token: *issued.Token}

	// 4. One inline pipeline runs in a real Podman container.
	document := fmt.Sprintf("actions:\n  - name: profile\n    image: %q\n    timeout: 2m\n    outputs: [%s]\n    commands:\n", image.Reference, testProfileOutput)
	for _, command := range testProfileWorkloadCommands(caPEM) {
		encoded, _ := json.Marshal(command)
		document += "      - " + string(encoded) + "\n"
	}
	started := testProfileDo[api.PipelineStartResult](t, instance, service, "pipeline.start", api.PipelineStartRequest{ProjectID: testProfileProject, YAML: []byte(document)}, "profile-start", 0)
	executionID := started.ExecutionID
	var workspaceID string
	t.Cleanup(func() { testProfileRemoveOwned(t, executionID, workspaceID) })

	inspect := func() api.ExecutionInspectResult {
		inspected := testProfileDo[api.ExecutionInspectResult](t, instance, service, "execution.inspect", api.ExecutionInspectRequest{ExecutionID: executionID}, "", 0)
		if inspected.Cleanup.WorkspaceID != "" {
			workspaceID = inspected.Cleanup.WorkspaceID
		}
		return inspected
	}
	var terminal api.ExecutionInspectResult
	deadline := time.Now().Add(2 * time.Minute)
	for {
		terminal = inspect()
		if terminal.State != "queued" && terminal.State != "running" && terminal.Cleanup.State == "complete" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("execution did not reach a terminal state with complete cleanup: state %q cleanup %q\n%s",
				terminal.State, terminal.Cleanup.State, server.output.String())
		}
		time.Sleep(time.Second)
	}
	if terminal.State != "succeeded" || len(terminal.Actions) != 1 || terminal.Actions[0].Outputs[testProfileOutput] != testProfileOutputVal {
		t.Fatalf("terminal execution = %+v; want one succeeded action with the committed output", terminal)
	}
	actionID := terminal.Actions[0].ActionID
	readLogs := func() string {
		logs := testProfileDo[api.ExecutionLogsReadResult](t, instance, service, "execution.logs.read", api.ExecutionLogsReadRequest{ExecutionID: executionID, ActionID: actionID, Stream: "stdout"}, "", 0)
		var text strings.Builder
		for _, chunk := range logs.Chunks {
			text.Write(chunk.Data)
		}
		return text.String()
	}
	if logs := readLogs(); !strings.Contains(logs, testProfileLogMarker) {
		t.Fatalf("retained stdout does not carry the workload marker: %q", logs)
	}
	if info := broker.StreamInfo(t, messaging.EventsStream); info == nil || info.State.Msgs == 0 {
		t.Fatal("no lifecycle event reached JetStream")
	}

	// 5. A graceful restart keeps the service credential and the terminal
	// execution and its retained log readable.
	server.stop(t, true)
	if !strings.Contains(server.output.String(), "test profile") {
		t.Fatalf("server output lacks the test-profile notice: %q", server.output.String())
	}
	assertSecretFree("server output", server.output.String())
	if strings.Contains(server.output.String(), adminToken.Value) || strings.Contains(server.output.String(), service.token) {
		t.Fatal("server output rendered a credential")
	}
	restarted := testProfileStartServer(t, binary, env)
	testProfileWaitReady(t, restarted, httpClient, base)
	if after := inspect(); after.State != "succeeded" || after.Actions[0].Outputs[testProfileOutput] != testProfileOutputVal {
		t.Fatalf("after restart execution = %+v", after)
	}
	if logs := readLogs(); !strings.Contains(logs, testProfileLogMarker) {
		t.Fatal("retained log is not readable after restart")
	}
	restarted.stop(t, true)
	revision, _ := exec.Command("git", "rev-parse", "HEAD").Output()
	t.Logf("test profile: revision %s image %s execution %s", strings.TrimSpace(string(revision)), image.Reference, executionID)
}

// testProfileRemoveOwned removes only this run's workload containers (by the
// execution label the runtime stamps) and its workspace volume (by its
// deterministic name); the real binary carries no harness run labels, so a
// run-label sweep would miss them.
func testProfileRemoveOwned(t *testing.T, executionID, workspaceID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if names, err := runtimesupport.ContainerNamesByLabel(ctx, runtimepodman.LabelExecution, executionID); err == nil {
		for _, name := range names {
			_ = runtimesupport.RemoveContainerByName(ctx, name)
		}
	}
	if workspaceID != "" {
		if name, err := runtimepodman.WorkspaceVolumeName(id.WorkspaceID(workspaceID)); err == nil {
			_ = runtimesupport.RemoveWorkspaceVolume(ctx, name)
		}
	}
}
