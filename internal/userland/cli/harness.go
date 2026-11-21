package cli

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/optimiware/kodema/api"
)

const (
	defaultHarnessTokenFile        = "/kodema/secrets/KODEMA_TOKEN"
	maxHarnessTokenFileBytes       = 64 << 10
	harnessProxyPathPrefix         = "/llm/"
	harnessProxyEnvURL             = "KODEMA_HARNESS_PROXY_URL"
	harnessProxyShutdownTimeout    = 5 * time.Second
	harnessChildGracefulStopWindow = 5 * time.Second
)

// runHarness dispatches `kodema harness <subcommand>`.
func runHarness(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if len(args) == 0 {
		return usage(ports.Err, "harness requires a subcommand (exec)")
	}
	if args[0] == "-h" || args[0] == "--help" {
		return runHelp(ports.Out, ports.Err, []string{"harness"})
	}
	switch args[0] {
	case "exec":
		if hasHarnessHelpFlag(args[1:]) {
			return runHelp(ports.Out, ports.Err, []string{"harness", "exec"})
		}
		return runHarnessExec(ctx, config, args[1:], ports, deps)
	default:
		return usage(ports.Err, fmt.Sprintf("unknown harness subcommand %q (expected exec)", args[0]))
	}
}

// hasHarnessHelpFlag checks for -h or --help only before "--" and before the
// first positional child command argument.
func hasHarnessHelpFlag(args []string) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return false
		}
		if arg == "-h" || arg == "--help" {
			return true
		}
		if !strings.HasPrefix(arg, "-") {
			return false
		}
		if !strings.Contains(arg, "=") && isHarnessExecValueFlag(arg) {
			i++
		}
	}
	return false
}

func isHarnessExecValueFlag(flagName string) bool {
	switch flagName {
	case "--server", "--server-url", "--url", "--ca-file", "--token-file":
		return true
	default:
		return false
	}
}

type harnessExecOptions struct {
	serverURL string
	caFile    string
	caFlagSet bool
	tokenFile string
	cmdArgs   []string
}

func parseHarnessExecArgs(args []string) (harnessExecOptions, error) {
	var opts harnessExecOptions
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			opts.cmdArgs = args[i+1:]
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			opts.cmdArgs = args[i:]
			break
		}
		name, value, hasEq := strings.Cut(arg, "=")
		switch name {
		case "--server", "--server-url", "--url":
			if !hasEq {
				if i+1 >= len(args) {
					return opts, fmt.Errorf("flag %s requires a value", name)
				}
				i++
				value = args[i]
			}
			if strings.TrimSpace(value) == "" {
				return opts, fmt.Errorf("flag %s requires a non-empty value", name)
			}
			opts.serverURL = value
		case "--ca-file":
			if !hasEq {
				if i+1 >= len(args) {
					return opts, fmt.Errorf("flag %s requires a value", name)
				}
				i++
				value = args[i]
			}
			if strings.TrimSpace(value) == "" {
				return opts, fmt.Errorf("flag %s requires a non-empty value", name)
			}
			opts.caFile = value
			opts.caFlagSet = true
		case "--token-file":
			if !hasEq {
				if i+1 >= len(args) {
					return opts, fmt.Errorf("flag %s requires a value", name)
				}
				i++
				value = args[i]
			}
			if strings.TrimSpace(value) == "" {
				return opts, fmt.Errorf("flag %s requires a non-empty value", name)
			}
			opts.tokenFile = value
		default:
			return opts, fmt.Errorf("unknown flag %q", arg)
		}
	}
	if len(opts.cmdArgs) == 0 {
		return opts, errors.New("harness exec requires a command after -- (for example: kodema harness exec -- <cmd...>)")
	}
	return opts, nil
}

func runHarnessExec(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	opts, err := parseHarnessExecArgs(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}

	getenv := deps.Env
	if getenv == nil {
		getenv = os.Getenv
	}

	serverRaw := strings.TrimSpace(opts.serverURL)
	if serverRaw == "" {
		serverRaw = strings.TrimSpace(config.ServerURL)
	}
	if serverRaw == "" {
		for _, key := range []string{"KODEMA_URL", "KODEMA_SERVER", "KODEMA_SERVER_URL"} {
			if v := strings.TrimSpace(getenv(key)); v != "" {
				serverRaw = v
				break
			}
		}
	}
	if serverRaw == "" {
		return usage(ports.Err, "harness exec requires --server or KODEMA_URL")
	}
	upstreamBase, err := parseHarnessServerURL(serverRaw)
	if err != nil {
		return usage(ports.Err, err.Error())
	}

	caFile := strings.TrimSpace(opts.caFile)
	if !opts.caFlagSet {
		if v := strings.TrimSpace(config.CAFile); v != "" {
			caFile = v
		} else if v := strings.TrimSpace(getenv("KODEMA_CA_FILE")); v != "" {
			caFile = v
		}
	}

	tokenFile := strings.TrimSpace(opts.tokenFile)
	if tokenFile == "" {
		if v := strings.TrimSpace(getenv("KODEMA_TOKEN_FILE")); v != "" {
			tokenFile = v
		} else if v := strings.TrimSpace(config.CredentialFile); v != "" {
			tokenFile = v
		} else if v := strings.TrimSpace(getenv("KODEMA_CREDENTIAL_FILE")); v != "" {
			tokenFile = v
		} else {
			tokenFile = defaultHarnessTokenFile
		}
	}

	var transport http.RoundTripper
	if deps.HarnessHTTP != nil && !opts.caFlagSet {
		transport = deps.HarnessHTTP
	} else {
		built, buildErr := newHarnessUpstreamTransport(caFile)
		if buildErr != nil {
			return usage(ports.Err, buildErr.Error())
		}
		defer built.CloseIdleConnections()
		transport = built
	}

	if err := ctx.Err(); err != nil {
		return failure(ports.Err, fmt.Errorf("harness exec cancelled: %w", err))
	}

	localKey, err := generateHarnessLocalKey()
	if err != nil {
		return failure(ports.Err, fmt.Errorf("failed to initialize local harness proxy key: %w", err))
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return failure(ports.Err, fmt.Errorf("failed to listen on loopback: %w", err))
	}

	proxyHandler := &harnessLoopbackProxy{
		upstreamBase: upstreamBase,
		tokenFile:    tokenFile,
		localKey:     localKey,
		client: &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}

	server := &http.Server{
		Handler:           proxyHandler,
		ReadHeaderTimeout: 30 * time.Second,
	}

	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = server.Serve(listener)
	}()

	shutdownServer := func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), harnessProxyShutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
		}
		<-serveDone
	}
	defer shutdownServer()

	if err := ctx.Err(); err != nil {
		return failure(ports.Err, fmt.Errorf("harness exec cancelled: %w", err))
	}

	proxyOrigin := "http://" + listener.Addr().String()
	childEnv := buildHarnessChildEnv(collectHarnessParentEnv(deps), proxyOrigin, localKey)

	cmd := exec.Command(opts.cmdArgs[0], opts.cmdArgs[1:]...)
	cmd.Env = childEnv
	cmd.Stdin = ports.In
	cmd.Stdout = ports.Out
	cmd.Stderr = ports.Err

	if err := cmd.Start(); err != nil {
		return failure(ports.Err, fmt.Errorf("failed to start %q: %w", opts.cmdArgs[0], err))
	}

	waitDone := make(chan error, 1)
	go func() {
		waitDone <- cmd.Wait()
	}()

	var waitErr error
	select {
	case waitErr = <-waitDone:
	case <-ctx.Done():
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
		timer := time.NewTimer(harnessChildGracefulStopWindow)
		select {
		case waitErr = <-waitDone:
			timer.Stop()
		case <-timer.C:
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			waitErr = <-waitDone
		}
	}

	return childExitCode(waitErr, ports.Err)
}

func childExitCode(waitErr error, errOut io.Writer) int {
	if waitErr == nil {
		return ExitSuccess
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		if code := exitErr.ExitCode(); code >= 0 {
			return code
		}
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ExitFailure
	}
	if errOut != nil {
		fmt.Fprintf(errOut, "error: %v\n", waitErr)
	}
	return ExitFailure
}

func parseHarnessServerURL(raw string) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, errors.New("server URL is empty")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("invalid server URL %q: %w", trimmed, err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("server URL %q must use http or https", trimmed)
	}
	if parsed.Host == "" || parsed.Opaque != "" || parsed.User != nil {
		return nil, fmt.Errorf("server URL %q must specify a host without userinfo", trimmed)
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, fmt.Errorf("server URL %q must not include a query or fragment", trimmed)
	}
	parsed.Scheme = scheme
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	return parsed, nil
}

func newHarnessUpstreamTransport(caFile string) (*http.Transport, error) {
	tlsConfig := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: false,
	}
	if caFile != "" {
		info, err := os.Stat(caFile)
		if err != nil {
			return nil, fmt.Errorf("CA file %q is unreadable", caFile)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("CA file %q is a directory", caFile)
		}
		pemBytes, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("CA file %q is unreadable", caFile)
		}
		pool, err := ParseCABundle(pemBytes)
		if err != nil {
			return nil, fmt.Errorf("CA file %q is invalid: %w", caFile, err)
		}
		tlsConfig.RootCAs = pool
	}
	return &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true,
		TLSClientConfig:       tlsConfig,
	}, nil
}

func generateHarnessLocalKey() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return "kodema-local-" + hex.EncodeToString(buf[:]), nil
}

func collectHarnessParentEnv(deps CommandDeps) []string {
	if deps.Environ != nil {
		return deps.Environ()
	}
	base := os.Environ()
	if deps.Env == nil {
		return base
	}
	out := make([]string, 0, len(base)+3)
	seen := make(map[string]bool, len(base))
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		seen[key] = true
		if val := deps.Env(key); val != "" {
			out = append(out, key+"="+val)
		} else if strings.HasPrefix(key, "KODEMA_LLM_") && strings.HasSuffix(key, "_BASE_URL") {
			continue
		} else {
			out = append(out, entry)
		}
	}
	for _, key := range []string{
		"KODEMA_LLM_OPENAI_BASE_URL",
		"KODEMA_LLM_ANTHROPIC_BASE_URL",
		"KODEMA_LLM_OPENCODE_BASE_URL",
	} {
		if seen[key] {
			continue
		}
		if val := deps.Env(key); val != "" {
			out = append(out, key+"="+val)
		}
	}
	return out
}

// buildHarnessChildEnv constructs the child process environment with provider
// base URLs and local dummy API keys pointing at the loopback proxy. Any
// existing KODEMA_LLM_<NAME>_BASE_URL variables with /llm/ paths are rewritten
// to point at the loopback proxy while preserving their provider path suffix.
func buildHarnessChildEnv(parentEnv []string, proxyOrigin, localKey string) []string {
	origin := strings.TrimRight(proxyOrigin, "/")
	rewrittenBase := make(map[string]string)

	filtered := make([]string, 0, len(parentEnv)+8)
	for _, entry := range parentEnv {
		key, val, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		switch key {
		case harnessProxyEnvURL,
			"OPENAI_BASE_URL", "OPENAI_API_KEY",
			"ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY",
			"OPENCODE_BASE_URL", "OPENCODE_API_KEY":
			continue
		}
		if strings.HasPrefix(key, "KODEMA_LLM_") && strings.HasSuffix(key, "_BASE_URL") {
			if rewritten, ok := rewriteProviderBaseURLToLoopback(val, origin); ok {
				rewrittenBase[key] = rewritten
				filtered = append(filtered, key+"="+rewritten)
				continue
			}
		}
		filtered = append(filtered, entry)
	}

	openAIBase := origin + "/llm/openai/v1"
	if v, ok := rewrittenBase["KODEMA_LLM_OPENAI_BASE_URL"]; ok {
		openAIBase = v
	}
	anthropicBase := origin + "/llm/anthropic"
	if v, ok := rewrittenBase["KODEMA_LLM_ANTHROPIC_BASE_URL"]; ok {
		anthropicBase = v
	}
	openCodeBase := origin + "/llm/opencode/v1"
	if v, ok := rewrittenBase["KODEMA_LLM_OPENCODE_BASE_URL"]; ok {
		openCodeBase = v
	}

	filtered = append(filtered,
		harnessProxyEnvURL+"="+origin,
		"OPENAI_BASE_URL="+openAIBase,
		"OPENAI_API_KEY="+localKey,
		"ANTHROPIC_BASE_URL="+anthropicBase,
		"ANTHROPIC_API_KEY="+localKey,
		"OPENCODE_BASE_URL="+openCodeBase,
		"OPENCODE_API_KEY="+localKey,
	)
	return filtered
}

func rewriteProviderBaseURLToLoopback(rawURL, proxyOrigin string) (string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Path == "" {
		return "", false
	}
	idx := strings.Index(parsed.Path, harnessProxyPathPrefix)
	if idx < 0 {
		return "", false
	}
	llmPath := strings.TrimRight(parsed.Path[idx:], "/")
	if llmPath == "" || llmPath == "/llm" {
		return "", false
	}
	return proxyOrigin + llmPath, true
}

type harnessLoopbackProxy struct {
	upstreamBase *url.URL
	tokenFile    string
	localKey     string
	client       *http.Client
}

func (p *harnessLoopbackProxy) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request == nil || request.URL == nil {
		writeHarnessJSONError(writer, http.StatusBadRequest, api.CodeInvalidInput, "invalid request")
		return
	}
	if !p.verifyLocalKey(request.Header) {
		writeHarnessJSONError(writer, http.StatusUnauthorized, api.CodeUnauthenticated, "invalid or missing local harness proxy key")
		return
	}
	if err := validateHarnessProxyPath(request.URL); err != nil {
		writeHarnessJSONError(writer, http.StatusNotFound, api.CodeNotFound, err.Error())
		return
	}

	// Critical for live token rotation survival: read the execution token fresh
	// from disk on every single upstream request; never cache it across calls.
	freshToken, err := readFreshHarnessToken(p.tokenFile)
	if err != nil {
		writeHarnessJSONError(writer, http.StatusServiceUnavailable, api.CodeDependencyUnavailable, err.Error())
		return
	}

	upstreamURL := *p.upstreamBase
	upstreamURL.Path = p.upstreamBase.Path + request.URL.Path
	upstreamURL.RawPath = ""
	upstreamURL.RawQuery = request.URL.RawQuery
	upstreamURL.Fragment = ""

	outbound, err := http.NewRequestWithContext(request.Context(), request.Method, upstreamURL.String(), request.Body)
	if err != nil {
		writeHarnessJSONError(writer, http.StatusInternalServerError, api.CodeInternalFailure, "failed to construct upstream request")
		return
	}
	if request.ContentLength >= 0 {
		outbound.ContentLength = request.ContentLength
	}
	copyHarnessHeaders(outbound.Header, request.Header, true)
	outbound.Header.Set("Authorization", "Bearer "+freshToken)

	resp, err := p.client.Do(outbound)
	if err != nil {
		if request.Context().Err() != nil {
			writeHarnessJSONError(writer, http.StatusRequestTimeout, api.CodeDeadlineExpired, "upstream request cancelled")
			return
		}
		writeHarnessJSONError(writer, http.StatusBadGateway, api.CodeDependencyUnavailable, "upstream Kodema LLM proxy is unreachable")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	copyHarnessHeaders(writer.Header(), resp.Header, false)
	flusher, canFlush := writer.(http.Flusher)
	writer.WriteHeader(resp.StatusCode)
	if canFlush {
		flusher.Flush()
	}

	buf := make([]byte, 8*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := writer.Write(buf[:n]); writeErr != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				panic(http.ErrAbortHandler)
			}
			return
		}
	}
}

func (p *harnessLoopbackProxy) verifyLocalKey(header http.Header) bool {
	if p.localKey == "" {
		return false
	}
	expected := []byte(p.localKey)
	var presented []string

	for _, raw := range header.Values("Authorization") {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		const bearerPrefix = "Bearer "
		if len(trimmed) <= len(bearerPrefix) || !strings.EqualFold(trimmed[:len(bearerPrefix)], bearerPrefix) {
			return false
		}
		tokenVal := strings.TrimSpace(trimmed[len(bearerPrefix):])
		if tokenVal == "" {
			return false
		}
		presented = append(presented, tokenVal)
	}
	for _, raw := range header.Values("X-Api-Key") {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		presented = append(presented, trimmed)
	}
	if len(presented) == 0 {
		return false
	}
	for _, candidate := range presented {
		if subtle.ConstantTimeCompare([]byte(candidate), expected) != 1 {
			return false
		}
	}
	return true
}

func validateHarnessProxyPath(u *url.URL) error {
	if !strings.HasPrefix(u.Path, harnessProxyPathPrefix) {
		return errors.New("harness proxy only serves /llm/<provider>/* routes")
	}
	rest := strings.TrimPrefix(u.Path, harnessProxyPathPrefix)
	provider, upstreamRel, ok := strings.Cut(rest, "/")
	if !ok || provider == "" || upstreamRel == "" {
		return errors.New("harness proxy path must be /llm/<provider>/<path>")
	}
	lowerEscaped := strings.ToLower(u.EscapedPath())
	if strings.Contains(lowerEscaped, "%2f") || strings.Contains(lowerEscaped, "%5c") || strings.Contains(lowerEscaped, "%00") {
		return errors.New("harness proxy path contains encoded separator")
	}
	for _, seg := range strings.Split(rest, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return errors.New("harness proxy path contains empty or relative segment")
		}
	}
	return nil
}

func readFreshHarnessToken(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("execution token file is unreadable")
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxHarnessTokenFileBytes+1))
	if err != nil {
		return "", errors.New("execution token file could not be read")
	}
	if len(data) > maxHarnessTokenFileBytes {
		return "", errors.New("execution token file exceeds maximum size")
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", errors.New("execution token file is empty")
	}
	if strings.ContainsAny(token, "\r\n\x00") {
		return "", errors.New("execution token contains invalid control characters")
	}
	return token, nil
}

var harnessHopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

func copyHarnessHeaders(dst, src http.Header, stripAuth bool) {
	connHop := make(map[string]bool)
	for _, val := range src.Values("Connection") {
		for item := range strings.SplitSeq(val, ",") {
			if trimmed := strings.TrimSpace(item); trimmed != "" {
				connHop[textproto.CanonicalMIMEHeaderKey(trimmed)] = true
			}
		}
	}
	for rawName, values := range src {
		canonical := textproto.CanonicalMIMEHeaderKey(rawName)
		if harnessHopByHopHeaders[canonical] || connHop[canonical] {
			continue
		}
		if stripAuth {
			if canonical == "Authorization" || canonical == "X-Api-Key" || canonical == "Host" || canonical == "Content-Length" {
				continue
			}
		}
		for _, v := range values {
			if strings.ContainsAny(v, "\r\n\x00") {
				continue
			}
			dst.Add(canonical, v)
		}
	}
}

func writeHarnessJSONError(writer http.ResponseWriter, status int, code, message string) {
	body, err := json.Marshal(api.Error{Code: code, Message: message})
	if err != nil {
		body = []byte(`{"code":"internal_failure","message":"internal failure"}`)
	}
	writer.Header().Set("Content-Type", api.MediaType)
	writer.WriteHeader(status)
	_, _ = writer.Write(body)
}
