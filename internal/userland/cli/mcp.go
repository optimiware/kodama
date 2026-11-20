package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/client"
)

const maxMCPStdioMessageBytes = 4 << 20

var errMCPRedirectRefused = errors.New("MCP redirect refused")

// runMCP dispatches `kodema mcp <stdio|install>`.
func runMCP(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if len(args) == 0 {
		return usage(ports.Err, "mcp requires a subcommand (stdio|install)")
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		return runHelp(ports.Out, ports.Err, append([]string{"mcp"}, args[1:]...))
	}
	switch args[0] {
	case "stdio":
		return runMCPStdio(ctx, config, args[1:], ports, deps)
	case "install":
		return runMCPInstall(ctx, config, args[1:], ports, deps)
	default:
		return usage(ports.Err, fmt.Sprintf("unknown mcp subcommand %q", args[0]))
	}
}

// runMCPStdio bridges newline-delimited JSON-RPC 2.0 messages on Ports.In to
// <ServerURL>/mcp with Bearer authentication from Ports.Credential and writes
// non-empty JSON-RPC responses as single lines to Ports.Out.
func runMCPStdio(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"mcp", "stdio"})
	}
	if len(args) > 0 {
		return usage(ports.Err, fmt.Sprintf("unexpected argument %q", args[0]))
	}
	if err := config.Validate(); err != nil {
		return usage(ports.Err, err.Error())
	}
	parsedURL, err := parseServerURL(config.ServerURL)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if ports.Credential == nil {
		return failure(ports.Err, fmt.Errorf("no credential source is configured"))
	}
	if ports.In == nil {
		return ExitSuccess
	}

	endpoint := *parsedURL
	endpoint.Path = strings.TrimSuffix(parsedURL.Path, "/") + "/mcp"
	endpoint.RawPath = ""
	endpointURL := endpoint.String()

	roundTripper := deps.HTTP
	if roundTripper == nil {
		roundTripper = http.DefaultTransport
	}
	httpClient := &http.Client{
		Transport: roundTripper,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errMCPRedirectRefused
		},
	}

	// fail reports one undeliverable message and answers its request id, if it
	// has one, with a JSON-RPC internal error so the MCP client is not left
	// waiting. The bridge keeps serving.
	fail := func(payload []byte, err error) {
		fmt.Fprintf(ports.Err, "error: %v\n", err)
		var request api.MCPRequest
		if json.Unmarshal(payload, &request) != nil || len(request.ID) == 0 {
			return
		}
		reply, _ := json.Marshal(api.MCPResponse{
			JSONRPC: api.MCPJSONRPC,
			ID:      request.ID,
			Error:   &api.MCPError{Code: -32603, Message: err.Error()},
		})
		_, _ = ports.Out.Write(append(reply, '\n'))
	}

	scanner := bufio.NewScanner(ports.In)
	scanner.Buffer(make([]byte, 64*1024), maxMCPStdioMessageBytes)

	for scanner.Scan() {
		if ctx.Err() != nil {
			return failure(ports.Err, ctx.Err())
		}
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		payload := append([]byte(nil), line...)

		authHeader, err := client.Bearer(ctx, ports.Credential, ports.Now())
		if err != nil {
			fail(payload, err)
			continue
		}

		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL, bytes.NewReader(payload))
		if err != nil {
			return failure(ports.Err, fmt.Errorf("cannot build MCP request"))
		}
		request.Header.Set("Content-Type", api.MediaType)
		request.Header.Set("Accept", api.MediaType)
		request.Header.Set("Authorization", authHeader)
		request.ContentLength = int64(len(payload))

		response, err := httpClient.Do(request)
		if err != nil {
			if errors.Is(err, errMCPRedirectRefused) {
				fail(payload, fmt.Errorf("MCP endpoint attempted a redirect"))
				continue
			}
			if ctx.Err() != nil {
				return failure(ports.Err, ctx.Err())
			}
			fail(payload, fmt.Errorf("MCP endpoint is unavailable"))
			continue
		}

		respBody, readErr := readBoundedMCP(response.Body, api.MaxResponseBytes)
		_ = response.Body.Close()
		if readErr != nil {
			fail(payload, fmt.Errorf("MCP response is unreadable or oversized"))
			continue
		}

		if response.StatusCode >= 300 && response.StatusCode < 400 {
			fail(payload, fmt.Errorf("MCP endpoint attempted a redirect"))
			continue
		}
		// Ignore 202 Accepted / 204 No Content and empty notification responses.
		if response.StatusCode == http.StatusAccepted || response.StatusCode == http.StatusNoContent {
			continue
		}
		trimmedResp := bytes.TrimSpace(respBody)
		if response.StatusCode < 200 || response.StatusCode > 299 {
			if pubErr := decodePublicErrorSummary(trimmedResp); pubErr != "" {
				fail(payload, fmt.Errorf("MCP endpoint returned HTTP %d: %s", response.StatusCode, pubErr))
				continue
			}
			fail(payload, fmt.Errorf("MCP endpoint returned HTTP %d", response.StatusCode))
			continue
		}
		if len(trimmedResp) == 0 {
			continue
		}

		var singleLine bytes.Buffer
		if err := json.Compact(&singleLine, trimmedResp); err != nil {
			// Preserve single-line framing even if an upstream sent non-compact bytes.
			collapsed := bytes.ReplaceAll(trimmedResp, []byte("\r"), nil)
			collapsed = bytes.ReplaceAll(collapsed, []byte("\n"), []byte(" "))
			singleLine.Write(collapsed)
		}
		singleLine.WriteByte('\n')
		if _, err := ports.Out.Write(singleLine.Bytes()); err != nil {
			return failure(ports.Err, err)
		}
	}

	if err := scanner.Err(); err != nil {
		return failure(ports.Err, fmt.Errorf("stdin read failed: %w", err))
	}
	return ExitSuccess
}

func readBoundedMCP(reader io.Reader, limit int) ([]byte, error) {
	if reader == nil {
		return nil, nil
	}
	data, err := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return data, nil
}

func decodePublicErrorSummary(data []byte) string {
	var pub api.Error
	if err := json.Unmarshal(data, &pub); err == nil && pub.Message != "" {
		return pub.Error()
	}
	var mcpResp api.MCPResponse
	if err := json.Unmarshal(data, &mcpResp); err == nil && mcpResp.Error != nil {
		if len(mcpResp.Error.Data) > 0 {
			if dataErr, err := api.DecodeMCPErrorData(mcpResp.Error.Data); err == nil && dataErr.Message != "" {
				return dataErr.Error()
			}
		}
		if mcpResp.Error.Message != "" {
			return mcpResp.Error.Message
		}
	}
	return ""
}

// runMCPInstall prints or writes the MCP server configuration entry for
// Claude Code, Cursor, or Codex.
func runMCPInstall(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"mcp", "install"})
	}
	var (
		nameFlag       string
		configFileFlag string
		outputFlag     string
		printOnly      bool
		localScope     bool
	)
	spec := newArgSpec()
	spec.stringFlag(&nameFlag, "--name")
	spec.stringFlag(&configFileFlag, "--config-file", "--file")
	spec.stringFlag(&outputFlag, "--output")
	spec.boolFlag(&printOnly, "--print")
	spec.boolFlag(&localScope, "--local", "--project-scope")
	positionals, _, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if len(positionals) > 1 {
		return usage(ports.Err, fmt.Sprintf("unexpected extra argument %q", positionals[1]))
	}

	target := "claude"
	if len(positionals) == 1 {
		target = strings.ToLower(strings.TrimSpace(positionals[0]))
	}
	switch target {
	case "claude", "cursor", "codex", "opencode":
	default:
		return usage(ports.Err, fmt.Sprintf("unknown mcp install target %q; want claude, cursor, codex, or opencode", target))
	}
	if localScope && target != "claude" && target != "opencode" {
		return usage(ports.Err, "--local is only supported for the claude and opencode targets")
	}

	serverName := strings.TrimSpace(nameFlag)
	if serverName == "" {
		serverName = "kodema"
	}
	if strings.ContainsAny(serverName, " \t\r\n") {
		return usage(ports.Err, fmt.Sprintf("invalid MCP server name %q", serverName))
	}

	format, err := resolveOutputFormat(outputFlag, config)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if err := config.Validate(); err != nil {
		return usage(ports.Err, err.Error())
	}

	cmdName := strings.TrimSpace(deps.CommandPath)
	if cmdName == "" {
		cmdName = "kodema"
	}
	mcpArgs := []string{"--url", config.ServerURL}
	if caPath := strings.TrimSpace(config.CAFile); caPath != "" {
		// The editor starts kodema from its own working directory.
		absCAPath, err := filepath.Abs(caPath)
		if err != nil {
			return failure(ports.Err, fmt.Errorf("cannot resolve CA file %q: %w", caPath, err))
		}
		mcpArgs = append(mcpArgs, "--ca-file", absCAPath)
	}
	sessionPath := strings.TrimSpace(config.SessionFile)
	credPath := strings.TrimSpace(config.CredentialFile)
	if deps.ExplicitSessionFile && deps.ExplicitCredentialFile {
		return usage(ports.Err, "mcp install accepts either --session or --credential-file, not both")
	}
	switch {
	case sessionPath != "" && (credPath == "" || !deps.ExplicitCredentialFile):
		// Preserve an explicit custom session path; prefer it over an ambient
		// KODEMA_CREDENTIAL_FILE when --credential-file was not explicitly set.
		absSessionPath, err := filepath.Abs(sessionPath)
		if err != nil {
			return failure(ports.Err, fmt.Errorf("cannot resolve session file %q: %w", sessionPath, err))
		}
		mcpArgs = append(mcpArgs, "--session", absSessionPath)
	case credPath != "" && !deps.ExplicitCredentialFile && hasUsableHumanSession(ctx, config.ServerURL, deps.Login.Store, ports.Now):
		// Prefer an already-logged-in refreshing human session over an ambient
		// KODEMA_CREDENTIAL_FILE (such as env.sh's 24h admin-token).
		if ports.Err != nil {
			fmt.Fprintf(ports.Err, "note: using existing human session instead of KODEMA_CREDENTIAL_FILE (%s); pass --credential-file explicitly to embed a static token file\n", credPath)
		}
	case credPath != "":
		absCredPath, err := filepath.Abs(credPath)
		if err != nil {
			return failure(ports.Err, fmt.Errorf("cannot resolve credential file %q: %w", credPath, err))
		}
		mcpArgs = append(mcpArgs, "--credential-file", absCredPath)
		warnExpiringCredentialFile(ports.Err, absCredPath, ports.Files)
	}
	mcpArgs = append(mcpArgs, "mcp", "stdio")

	if printOnly {
		rendered, renderErr := renderMCPSnippet(target, serverName, cmdName, mcpArgs)
		if renderErr != nil {
			return failure(ports.Err, renderErr)
		}
		if _, err := ports.Out.Write(rendered); err != nil {
			return failure(ports.Err, err)
		}
		return ExitSuccess
	}

	if ports.Files == nil {
		return failure(ports.Err, fmt.Errorf("no filesystem is configured"))
	}
	destPath, err := resolveMCPConfigPath(target, strings.TrimSpace(configFileFlag), localScope, deps.HomeDir, ports.Files)
	if err != nil {
		return failure(ports.Err, err)
	}

	existing, readErr := ports.Files.ReadFile(destPath)
	if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		return failure(ports.Err, fmt.Errorf("cannot read %s: %w", destPath, readErr))
	}

	var updated []byte
	switch target {
	case "claude", "cursor":
		updated, err = mergeMCPJSONConfig(existing, serverName, cmdName, mcpArgs)
	case "codex":
		updated, err = mergeMCPTOMLConfig(existing, serverName, cmdName, mcpArgs)
	case "opencode":
		updated, err = mergeMCPOpenCodeConfig(existing, serverName, cmdName, mcpArgs)
	}
	if err != nil {
		return failure(ports.Err, fmt.Errorf("cannot update %s: %w", destPath, err))
	}

	if err := ports.Files.ReplaceFile(destPath, updated, 0o600); err != nil {
		return failure(ports.Err, fmt.Errorf("cannot write %s: %w", destPath, err))
	}

	if format == OutputJSON {
		summary := map[string]any{
			"target":      target,
			"name":        serverName,
			"config_file": destPath,
			"command":     cmdName,
			"args":        mcpArgs,
		}
		if err := json.NewEncoder(ports.Out).Encode(summary); err != nil {
			return failure(ports.Err, err)
		}
		return ExitSuccess
	}

	fmt.Fprintf(ports.Out, "installed MCP server %q for %s in %s\n", serverName, target, destPath)
	return ExitSuccess
}

// hasUsableHumanSession reports whether store currently holds a refreshing or
// non-expired version 2 human session bound to serverURL.
func hasUsableHumanSession(ctx context.Context, serverURL string, store SessionRecordStore, now func() time.Time) bool {
	if store == nil {
		return false
	}
	apiURL, err := normalizeAPIURL(serverURL)
	if err != nil {
		return false
	}
	record, err := store.LoadRecord(ctx)
	if err != nil || record.Legacy() {
		return false
	}
	if record.APIURL != apiURL {
		return false
	}
	if record.RefreshToken != "" {
		return true
	}
	if now == nil {
		now = time.Now
	}
	return record.Usable(now())
}

// warnExpiringCredentialFile emits a diagnostic on errOut when mcp install
// embeds a short-lived local admin token (such as kodema up's 24h admin-token)
// or a credential file carrying a JWT with an explicit exp claim. Token bytes
// are never rendered.
func warnExpiringCredentialFile(errOut io.Writer, credPath string, files FileSystem) {
	if errOut == nil {
		return
	}
	switch filepath.Base(credPath) {
	case "admin-token", "tenant-admin-token":
		fmt.Fprintf(errOut,
			"warning: embedding --credential-file %s (short-lived local admin token expires in 24h and carries broad admin roles); run 'kodema login' and re-run without KODEMA_CREDENTIAL_FILE to use a refreshing human session\n",
			credPath)
		return
	}
	if files == nil {
		return
	}
	data, err := files.ReadRegularFile(credPath, api.MaxCredentialBytes+64)
	if err != nil {
		return
	}
	if expiresAt, ok := credentialTokenExpiry(strings.TrimSpace(string(data))); ok {
		fmt.Fprintf(errOut,
			"warning: embedding --credential-file %s which contains an expiring token (expires %s); prefer 'kodema login' with a refreshing human session\n",
			credPath, formatSessionTime(expiresAt))
	}
}

// credentialTokenExpiry extracts the unverified exp claim of a compact JWT so
// mcp install can warn about expiring static tokens without rendering any
// credential bytes. Non-JWT credentials (such as kodsvc.* tokens) return false.
func credentialTokenExpiry(raw string) (time.Time, bool) {
	if strings.HasPrefix(raw, "kodsvc.") || strings.HasPrefix(raw, "kodex.") {
		return time.Time{}, false
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp *float64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == nil || *claims.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(*claims.Exp), 0).UTC(), true
}

func resolveMCPConfigPath(target, explicit string, localScope bool, homeDir func() (string, error), files FileSystem) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if target == "claude" && localScope {
		return ".mcp.json", nil
	}
	if target == "opencode" && localScope {
		return preferExistingJSONC("opencode.json", files), nil
	}
	resolveHome := homeDir
	if resolveHome == nil {
		resolveHome = os.UserHomeDir
	}
	home, err := resolveHome()
	if err != nil || strings.TrimSpace(home) == "" {
		return "", fmt.Errorf("cannot determine home directory; pass --config-file <path>")
	}
	switch target {
	case "claude":
		return filepath.Join(home, ".claude.json"), nil
	case "cursor":
		return filepath.Join(home, ".cursor", "mcp.json"), nil
	case "codex":
		return filepath.Join(home, ".codex", "config.toml"), nil
	case "opencode":
		return preferExistingJSONC(filepath.Join(home, ".config", "opencode", "opencode.json"), files), nil
	default:
		return "", fmt.Errorf("unsupported target %q", target)
	}
}

// preferExistingJSONC returns jsonPath+"c" when jsonPath does not exist but a
// sibling .jsonc config already exists, avoiding creating a conflicting file.
func preferExistingJSONC(jsonPath string, files FileSystem) string {
	if files == nil {
		return jsonPath
	}
	if _, err := files.ReadFile(jsonPath); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return jsonPath
	}
	jsoncPath := jsonPath + "c"
	if _, err := files.ReadFile(jsoncPath); err == nil {
		return jsoncPath
	}
	return jsonPath
}

func renderMCPSnippet(target, serverName, cmdName string, mcpArgs []string) ([]byte, error) {
	switch target {
	case "codex":
		return []byte(formatCodexTOMLSection(serverName, cmdName, mcpArgs)), nil
	case "opencode":
		return mergeMCPOpenCodeConfig(nil, serverName, cmdName, mcpArgs)
	default:
		return mergeMCPJSONConfig(nil, serverName, cmdName, mcpArgs)
	}
}

func parseJSONObject(existing []byte, allowJSONC bool) (map[string]any, error) {
	root := make(map[string]any)
	trimmed := bytes.TrimSpace(existing)
	if len(trimmed) == 0 {
		return root, nil
	}
	if allowJSONC {
		trimmed = stripJSONC(trimmed)
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil || root == nil {
		return nil, fmt.Errorf("existing file is not a valid JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("existing file is not a valid JSON object")
	}
	return root, nil
}

func ensureJSONObjectField(parent map[string]any, key, label string) (map[string]any, error) {
	if raw, ok := parent[key]; ok && raw != nil {
		cast, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("existing %s field is not a JSON object", label)
		}
		return cast, nil
	}
	child := make(map[string]any)
	parent[key] = child
	return child, nil
}

func encodeIndentedJSON(value any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// stripJSONC removes // line comments, /* */ block comments, and trailing
// commas before } or ] while preserving string literals intact.
func stripJSONC(src []byte) []byte {
	// Pass 1: strip comments outside double-quoted strings.
	var noComments bytes.Buffer
	noComments.Grow(len(src))
	inString := false
	for i := 0; i < len(src); i++ {
		c := src[i]
		if inString {
			noComments.WriteByte(c)
			if c == '\\' && i+1 < len(src) {
				i++
				noComments.WriteByte(src[i])
			} else if c == '"' {
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			noComments.WriteByte(c)
			continue
		}
		if c == '/' && i+1 < len(src) && src[i+1] == '/' {
			i += 2
			for i < len(src) && src[i] != '\n' {
				i++
			}
			if i < len(src) {
				noComments.WriteByte('\n')
			}
			continue
		}
		if c == '/' && i+1 < len(src) && src[i+1] == '*' {
			i += 2
			for i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/') {
				i++
			}
			if i+1 < len(src) {
				i++
			}
			continue
		}
		noComments.WriteByte(c)
	}

	// Pass 2: drop trailing commas immediately preceding } or ].
	cleaned := noComments.Bytes()
	var out bytes.Buffer
	out.Grow(len(cleaned))
	inString = false
	for i := 0; i < len(cleaned); i++ {
		c := cleaned[i]
		if inString {
			out.WriteByte(c)
			if c == '\\' && i+1 < len(cleaned) {
				i++
				out.WriteByte(cleaned[i])
			} else if c == '"' {
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			out.WriteByte(c)
			continue
		}
		if c == ',' {
			j := i + 1
			for j < len(cleaned) && (cleaned[j] == ' ' || cleaned[j] == '\t' || cleaned[j] == '\r' || cleaned[j] == '\n') {
				j++
			}
			if j < len(cleaned) && (cleaned[j] == '}' || cleaned[j] == ']') {
				continue
			}
		}
		out.WriteByte(c)
	}
	return out.Bytes()
}

func mergeMCPOpenCodeConfig(existing []byte, serverName, cmdName string, mcpArgs []string) ([]byte, error) {
	root, err := parseJSONObject(existing, true)
	if err != nil {
		return nil, err
	}
	if _, hasSchema := root["$schema"]; !hasSchema {
		root["$schema"] = "https://opencode.ai/config.json"
	}
	mcpObj, err := ensureJSONObjectField(root, "mcp", "mcp")
	if err != nil {
		return nil, err
	}
	servers, err := ensureJSONObjectField(mcpObj, "servers", "mcp.servers")
	if err != nil {
		return nil, err
	}
	command := make([]string, 0, len(mcpArgs)+1)
	command = append(command, cmdName)
	command = append(command, mcpArgs...)
	servers[serverName] = map[string]any{
		"type":    "local",
		"command": command,
	}
	return encodeIndentedJSON(root)
}

func mergeMCPJSONConfig(existing []byte, serverName, cmdName string, mcpArgs []string) ([]byte, error) {
	root, err := parseJSONObject(existing, false)
	if err != nil {
		return nil, err
	}
	servers, err := ensureJSONObjectField(root, "mcpServers", "mcpServers")
	if err != nil {
		return nil, err
	}
	servers[serverName] = map[string]any{
		"command": cmdName,
		"args":    mcpArgs,
	}
	return encodeIndentedJSON(root)
}

func formatCodexTOMLSection(serverName, cmdName string, mcpArgs []string) string {
	key := serverName
	if !isBareTOMLKey(serverName) {
		key = tomlString(serverName)
	}
	quotedArgs := make([]string, len(mcpArgs))
	for i, arg := range mcpArgs {
		quotedArgs[i] = tomlString(arg)
	}
	return fmt.Sprintf("[mcp_servers.%s]\ncommand = %s\nargs = [%s]\n",
		key, tomlString(cmdName), strings.Join(quotedArgs, ", "))
}

// tomlString renders a TOML basic string. strconv.Quote is not usable: TOML
// has no \a, \v or \x escape.
func tomlString(value string) string {
	var out strings.Builder
	out.WriteByte('"')
	for _, r := range value {
		switch {
		case r == '"' || r == '\\':
			out.WriteByte('\\')
			out.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&out, `\u%04X`, r)
		default:
			out.WriteRune(r)
		}
	}
	out.WriteByte('"')
	return out.String()
}

func isBareTOMLKey(key string) bool {
	if key == "" {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

func mergeMCPTOMLConfig(existing []byte, serverName, cmdName string, mcpArgs []string) ([]byte, error) {
	section := formatCodexTOMLSection(serverName, cmdName, mcpArgs)
	trimmedExisting := strings.TrimSpace(string(existing))
	if trimmedExisting == "" {
		return []byte(section), nil
	}

	key := serverName
	if !isBareTOMLKey(serverName) {
		key = tomlString(serverName)
	}
	targetTable := "[mcp_servers." + key
	altTable := "[mcp_servers." + tomlString(serverName)

	lines := strings.Split(strings.ReplaceAll(string(existing), "\r\n", "\n"), "\n")
	startIdx := -1
	endIdx := len(lines)
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "[") {
			continue
		}
		// What follows the server's table name in a header: "]", then perhaps
		// a comment, for the table itself and "." for one of its sub-tables.
		rest := strings.TrimPrefix(strings.TrimPrefix(trimmed, targetTable), altTable)
		if startIdx < 0 {
			if strings.HasPrefix(rest, "]") {
				startIdx = i
			}
			continue
		}
		if strings.HasPrefix(rest, ".") {
			continue
		}
		endIdx = i
		break
	}

	if startIdx >= 0 {
		var out strings.Builder
		prefix := strings.TrimRight(strings.Join(lines[:startIdx], "\n"), "\n")
		suffix := strings.TrimLeft(strings.Join(lines[endIdx:], "\n"), "\n")
		if prefix != "" {
			out.WriteString(prefix)
			out.WriteString("\n\n")
		}
		out.WriteString(section)
		if suffix != "" {
			out.WriteString("\n")
			out.WriteString(strings.TrimRight(suffix, "\n"))
			out.WriteString("\n")
		}
		return []byte(out.String()), nil
	}

	return []byte(strings.TrimRight(string(existing), "\r\n") + "\n\n" + section), nil
}
