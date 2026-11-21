package cli_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/optimiware/kodema/internal/userland/cli"
)

// TestHarnessExecHelperProcess acts as the child command executed by
// `kodema harness exec -- <cmd...>` in tests, keeping tests fast and hermetic
// without relying on external binaries like curl or node.
func TestHarnessExecHelperProcess(t *testing.T) {
	if os.Getenv("KODEMA_TEST_HARNESS_HELPER") != "1" {
		return
	}
	mode := os.Getenv("KODEMA_TEST_HARNESS_MODE")
	switch mode {
	case "exit":
		code, _ := strconv.Atoi(os.Getenv("KODEMA_TEST_HARNESS_EXIT_CODE"))
		os.Exit(code)
	case "sleep":
		// Wait until signalled.
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "proxy-checks":
		runHarnessHelperProxyChecks()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
		os.Exit(99)
	}
}

func runHarnessHelperProxyChecks() {
	// Verify zero CA boilerplate env vars were needed or injected on the child.
	for _, caEnv := range []string{"NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "CURL_CA_BUNDLE"} {
		if v := os.Getenv(caEnv); v != "" {
			fmt.Fprintf(os.Stderr, "unexpected CA env var %s=%q\n", caEnv, v)
			os.Exit(10)
		}
	}
	// Verify real token is never leaked into child environment.
	tokenV1 := os.Getenv("KODEMA_TEST_TOKEN_V1_CHECK")
	tokenV2 := os.Getenv("KODEMA_TEST_TOKEN_V2_CHECK")
	for _, entry := range os.Environ() {
		key, val, _ := strings.Cut(entry, "=")
		if key == "KODEMA_TEST_TOKEN_V1_CHECK" || key == "KODEMA_TEST_TOKEN_V2_CHECK" {
			continue
		}
		if (tokenV1 != "" && strings.Contains(val, tokenV1)) || (tokenV2 != "" && strings.Contains(val, tokenV2)) {
			fmt.Fprintf(os.Stderr, "real token leaked into child env %s\n", key)
			os.Exit(11)
		}
	}

	openAIBase := os.Getenv("OPENAI_BASE_URL")
	openAIKey := os.Getenv("OPENAI_API_KEY")
	anthropicBase := os.Getenv("ANTHROPIC_BASE_URL")
	anthropicKey := os.Getenv("ANTHROPIC_API_KEY")
	openCodeBase := os.Getenv("OPENCODE_BASE_URL")
	openCodeKey := os.Getenv("OPENCODE_API_KEY")
	customBase := os.Getenv("KODEMA_LLM_CUSTOM_BASE_URL")
	proxyURL := os.Getenv("KODEMA_HARNESS_PROXY_URL")

	if !strings.HasPrefix(proxyURL, "http://127.0.0.1:") {
		fmt.Fprintf(os.Stderr, "KODEMA_HARNESS_PROXY_URL=%q not loopback\n", proxyURL)
		os.Exit(12)
	}
	if openAIKey == "" || openAIKey != anthropicKey || openAIKey != openCodeKey {
		fmt.Fprintf(os.Stderr, "mismatched or empty local dummy keys\n")
		os.Exit(13)
	}
	if !strings.HasPrefix(customBase, proxyURL+"/llm/custom/v1") {
		fmt.Fprintf(os.Stderr, "custom provider base URL not rewritten: %q\n", customBase)
		os.Exit(14)
	}

	httpClient := &http.Client{Timeout: 10 * time.Second}

	// 1. Missing local dummy key must be rejected with 401.
	reqNoAuth, _ := http.NewRequest(http.MethodPost, openAIBase+"/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
	respNoAuth, err := httpClient.Do(reqNoAuth)
	if err != nil {
		fmt.Fprintf(os.Stderr, "no-auth request error: %v\n", err)
		os.Exit(15)
	}
	_ = respNoAuth.Body.Close()
	if respNoAuth.StatusCode != http.StatusUnauthorized {
		fmt.Fprintf(os.Stderr, "no-auth status = %d; want 401\n", respNoAuth.StatusCode)
		os.Exit(16)
	}

	// 2. Wrong local dummy key must be rejected with 401.
	reqWrongAuth, _ := http.NewRequest(http.MethodPost, openAIBase+"/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
	reqWrongAuth.Header.Set("Authorization", "Bearer wrong-key")
	respWrongAuth, err := httpClient.Do(reqWrongAuth)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wrong-auth request error: %v\n", err)
		os.Exit(17)
	}
	_ = respWrongAuth.Body.Close()
	if respWrongAuth.StatusCode != http.StatusUnauthorized {
		fmt.Fprintf(os.Stderr, "wrong-auth status = %d; want 401\n", respWrongAuth.StatusCode)
		os.Exit(18)
	}

	// 3. Non-/llm/ path must be rejected with 404.
	reqAdmin, _ := http.NewRequest(http.MethodGet, proxyURL+"/api/v1/projects", nil)
	reqAdmin.Header.Set("Authorization", "Bearer "+openAIKey)
	respAdmin, err := httpClient.Do(reqAdmin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "non-llm request error: %v\n", err)
		os.Exit(19)
	}
	_ = respAdmin.Body.Close()
	if respAdmin.StatusCode != http.StatusNotFound {
		fmt.Fprintf(os.Stderr, "non-llm status = %d; want 404\n", respAdmin.StatusCode)
		os.Exit(20)
	}

	// 4. OpenAI JSON call (uses token v1).
	reqOpenAI, _ := http.NewRequest(http.MethodPost, openAIBase+"/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
	reqOpenAI.Header.Set("Authorization", "Bearer "+openAIKey)
	reqOpenAI.Header.Set("Content-Type", "application/json")
	respOpenAI, err := httpClient.Do(reqOpenAI)
	if err != nil {
		fmt.Fprintf(os.Stderr, "openai request error: %v\n", err)
		os.Exit(21)
	}
	bodyOpenAI, _ := io.ReadAll(respOpenAI.Body)
	_ = respOpenAI.Body.Close()
	if respOpenAI.StatusCode != http.StatusOK || !strings.Contains(string(bodyOpenAI), `"provider":"openai"`) {
		fmt.Fprintf(os.Stderr, "openai unexpected status=%d body=%s\n", respOpenAI.StatusCode, string(bodyOpenAI))
		os.Exit(22)
	}

	// 5. Anthropic SSE streaming call (uses x-api-key local dummy key; verifies immediate streaming).
	reqAnth, _ := http.NewRequest(http.MethodPost, anthropicBase+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4","stream":true}`))
	reqAnth.Header.Set("x-api-key", anthropicKey)
	reqAnth.Header.Set("anthropic-version", "2023-06-01")
	reqAnth.Header.Set("Content-Type", "application/json")
	respAnth, err := httpClient.Do(reqAnth)
	if err != nil {
		fmt.Fprintf(os.Stderr, "anthropic request error: %v\n", err)
		os.Exit(23)
	}
	if respAnth.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(respAnth.Body)
		_ = respAnth.Body.Close()
		fmt.Fprintf(os.Stderr, "anthropic status=%d body=%s\n", respAnth.StatusCode, string(body))
		os.Exit(24)
	}
	scanner := bufio.NewScanner(respAnth.Body)
	var sseLines []string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") {
			sseLines = append(sseLines, line)
		}
	}
	_ = respAnth.Body.Close()
	if len(sseLines) < 2 {
		fmt.Fprintf(os.Stderr, "anthropic sse lines = %v; want >= 2\n", sseLines)
		os.Exit(25)
	}

	// 6. Trigger live token rotation on disk via signal file, wait for ack, then
	//    call OpenCode + Custom provider and verify zero 401s with the rotated token!
	rotateTrigger := os.Getenv("KODEMA_TEST_ROTATE_TRIGGER")
	rotateAck := os.Getenv("KODEMA_TEST_ROTATE_ACK")
	if err := os.WriteFile(rotateTrigger, []byte("rotate"), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "write rotate trigger: %v\n", err)
		os.Exit(26)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(rotateAck); err == nil {
			break
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "timed out waiting for token rotation ack\n")
			os.Exit(27)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 7. OpenCode call after token rotation.
	reqOC, _ := http.NewRequest(http.MethodPost, openCodeBase+"/chat/completions", strings.NewReader(`{"model":"opencode-1"}`))
	reqOC.Header.Set("Authorization", "Bearer "+openCodeKey)
	reqOC.Header.Set("Content-Type", "application/json")
	respOC, err := httpClient.Do(reqOC)
	if err != nil {
		fmt.Fprintf(os.Stderr, "opencode request error: %v\n", err)
		os.Exit(28)
	}
	bodyOC, _ := io.ReadAll(respOC.Body)
	_ = respOC.Body.Close()
	if respOC.StatusCode != http.StatusOK || !strings.Contains(string(bodyOC), `"provider":"opencode"`) || !strings.Contains(string(bodyOC), `"token_gen":"v2"`) {
		fmt.Fprintf(os.Stderr, "opencode post-rotation status=%d body=%s\n", respOC.StatusCode, string(bodyOC))
		os.Exit(29)
	}

	// 8. Custom provider call after token rotation.
	reqCustom, _ := http.NewRequest(http.MethodPost, customBase+"/chat/completions?foo=bar", strings.NewReader(`{"model":"vllm-local"}`))
	reqCustom.Header.Set("Authorization", "Bearer "+openAIKey)
	respCustom, err := httpClient.Do(reqCustom)
	if err != nil {
		fmt.Fprintf(os.Stderr, "custom request error: %v\n", err)
		os.Exit(30)
	}
	bodyCustom, _ := io.ReadAll(respCustom.Body)
	_ = respCustom.Body.Close()
	if respCustom.StatusCode != http.StatusOK || !strings.Contains(string(bodyCustom), `"provider":"custom"`) || !strings.Contains(string(bodyCustom), `"query":"foo=bar"`) {
		fmt.Fprintf(os.Stderr, "custom post-rotation status=%d body=%s\n", respCustom.StatusCode, string(bodyCustom))
		os.Exit(31)
	}

	fmt.Fprintln(os.Stdout, "all-harness-checks-passed")
}

func generateTestTLSServer(t *testing.T, handler http.Handler) (*httptest.Server, string) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ca key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Kodema Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create ca cert: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse ca cert: %v", err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatalf("write ca file: %v", err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf cert: %v", err)
	}

	srv := httptest.NewUnstartedServer(handler)
	srv.TLS = &tls.Config{
		MinVersion: tls.VersionTLS12,
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{leafDER, caDER},
			PrivateKey:  leafKey,
		}},
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, caFile
}

func TestHarnessExec_TLSStreamingAndLiveTokenRotation(t *testing.T) {
	const (
		tokenV1 = "kodex-exec-1.secret-token-v1"
		tokenV2 = "kodex-exec-1.secret-token-v2"
	)

	var (
		mu             sync.RWMutex
		activeToken    = tokenV1
		activeGen      = "v1"
		upstreamCalls  atomic.Int32
		unauthorizedCt atomic.Int32
	)

	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		mu.RLock()
		wantToken := activeToken
		gen := activeGen
		mu.RUnlock()

		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+wantToken {
			unauthorizedCt.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"unauthenticated","message":"stale or wrong token"}`))
			return
		}
		if r.Header.Get("X-Api-Key") != "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":"invalid_input","message":"unexpected x-api-key header"}`))
			return
		}

		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/llm/"), "/")
		provider := parts[0]

		if provider == "anthropic" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			flusher, ok := w.(http.Flusher)
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"token_gen\":%q}\n\n", gen)
			if ok {
				flusher.Flush()
			}
			_, _ = fmt.Fprintf(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n")
			if ok {
				flusher.Flush()
			}
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"provider":  provider,
			"path":      r.URL.Path,
			"query":     r.URL.RawQuery,
			"token_gen": gen,
		})
	})

	srv, caFile := generateTestTLSServer(t, upstream)

	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "KODEMA_TOKEN")
	if err := os.WriteFile(tokenFile, []byte(tokenV1+"\n"), 0o600); err != nil {
		t.Fatalf("write initial token: %v", err)
	}
	rotateTrigger := filepath.Join(dir, "rotate.trigger")
	rotateAck := filepath.Join(dir, "rotate.ack")

	// Watcher goroutine simulates Kodema's live atomic token rotation on disk.
	stopRotate := make(chan struct{})
	defer close(stopRotate)
	go func() {
		for {
			select {
			case <-stopRotate:
				return
			default:
			}
			if _, err := os.Stat(rotateTrigger); err == nil {
				mu.Lock()
				activeToken = tokenV2
				activeGen = "v2"
				mu.Unlock()
				tmp := tokenFile + ".tmp"
				_ = os.WriteFile(tmp, []byte(tokenV2+"\n"), 0o600)
				_ = os.Rename(tmp, tokenFile)
				_ = os.WriteFile(rotateAck, []byte("done"), 0o600)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	// Clear any ambient CA env vars so the test proves zero CA boilerplate on the child.
	t.Setenv("NODE_EXTRA_CA_CERTS", "")
	t.Setenv("SSL_CERT_FILE", "")
	t.Setenv("CURL_CA_BUNDLE", "")
	t.Setenv("KODEMA_TEST_HARNESS_HELPER", "1")
	t.Setenv("KODEMA_TEST_HARNESS_MODE", "proxy-checks")
	t.Setenv("KODEMA_TEST_TOKEN_V1_CHECK", tokenV1)
	t.Setenv("KODEMA_TEST_TOKEN_V2_CHECK", tokenV2)
	t.Setenv("KODEMA_TEST_ROTATE_TRIGGER", rotateTrigger)
	t.Setenv("KODEMA_TEST_ROTATE_ACK", rotateAck)
	t.Setenv("KODEMA_LLM_CUSTOM_BASE_URL", srv.URL+"/llm/custom/v1")

	var out, errOut bytes.Buffer
	code := cli.RunWithDeps(
		context.Background(),
		cli.Config{},
		[]string{
			"harness", "exec",
			"--server", srv.URL,
			"--ca-file", caFile,
			"--token-file", tokenFile,
			"--",
			os.Args[0], "-test.run=^TestHarnessExecHelperProcess$",
		},
		cli.Ports{Out: &out, Err: &errOut},
		cli.CommandDeps{},
	)
	if code != 0 {
		t.Fatalf("harness exec exit code = %d; stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "all-harness-checks-passed") {
		t.Fatalf("unexpected stdout: %q (stderr: %q)", out.String(), errOut.String())
	}
	if got := unauthorizedCt.Load(); got != 0 {
		t.Fatalf("upstream saw %d unauthorized requests; want 0 (zero 401s across live token rotation)", got)
	}
	if got := upstreamCalls.Load(); got != 4 {
		t.Fatalf("upstream calls = %d; want 4 (openai, anthropic, opencode, custom; rejected local requests must not hit upstream)", got)
	}
}

func TestHarnessExec_PropagatesExitCodeAndSignalShutdown(t *testing.T) {
	t.Run("propagates non-zero child exit code", func(t *testing.T) {
		t.Setenv("KODEMA_TEST_HARNESS_HELPER", "1")
		t.Setenv("KODEMA_TEST_HARNESS_MODE", "exit")
		t.Setenv("KODEMA_TEST_HARNESS_EXIT_CODE", "42")

		var out, errOut bytes.Buffer
		code := cli.RunWithDeps(
			context.Background(),
			cli.Config{ServerURL: "https://kodema.example.test"},
			[]string{
				"harness", "exec", "--",
				os.Args[0], "-test.run=^TestHarnessExecHelperProcess$",
			},
			cli.Ports{Out: &out, Err: &errOut},
			cli.CommandDeps{},
		)
		if code != 42 {
			t.Fatalf("exit code = %d; want 42 (stderr=%q)", code, errOut.String())
		}
	})

	t.Run("context cancellation shuts down child and proxy cleanly", func(t *testing.T) {
		t.Setenv("KODEMA_TEST_HARNESS_HELPER", "1")
		t.Setenv("KODEMA_TEST_HARNESS_MODE", "sleep")

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(150 * time.Millisecond)
			cancel()
		}()

		start := time.Now()
		var out, errOut bytes.Buffer
		code := cli.RunWithDeps(
			ctx,
			cli.Config{ServerURL: "https://kodema.example.test"},
			[]string{
				"harness", "exec", "--",
				os.Args[0], "-test.run=^TestHarnessExecHelperProcess$",
			},
			cli.Ports{Out: &out, Err: &errOut},
			cli.CommandDeps{},
		)
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("shutdown took %s; want prompt termination on cancel", elapsed)
		}
		if code == 0 {
			t.Fatalf("cancelled child exited 0; want non-zero signal exit code")
		}
	})

	t.Run("usage errors", func(t *testing.T) {
		cases := [][]string{
			{"harness"},
			{"harness", "unknown"},
			{"harness", "exec"},
			{"harness", "exec", "--"},
			{"harness", "exec", "--unknown-flag", "--", "true"},
		}
		for _, args := range cases {
			var out, errOut bytes.Buffer
			code := cli.RunWithDeps(
				context.Background(),
				cli.Config{ServerURL: "https://kodema.example.test"},
				args,
				cli.Ports{Out: &out, Err: &errOut},
				cli.CommandDeps{},
			)
			if code != cli.ExitUsage {
				t.Errorf("args %v: exit code = %d; want %d (stderr=%q)", args, code, cli.ExitUsage, errOut.String())
			}
		}
	})
}
