package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// maxGitCredentialRequestBytes bounds the request Git writes on standard input.
const maxGitCredentialRequestBytes = 64 << 10

// runGit dispatches the client's `kodema git <configure|credential>`.
func runGit(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if len(args) == 0 {
		return usage(ports.Err, "git requires a subcommand (configure|credential)")
	}
	switch args[0] {
	case "configure":
		if hasHelpFlag(args[1:]) {
			return runHelp(ports.Out, ports.Err, []string{"git", "configure"})
		}
		return runGitConfigure(ctx, config, args[1:], ports, deps)
	case "credential":
		if hasHelpFlag(args[1:]) {
			return runHelp(ports.Out, ports.Err, []string{"git", "credential"})
		}
		return runGitCredential(ctx, config, args[1:], ports)
	case "help", "--help", "-h":
		return runHelp(ports.Out, ports.Err, append([]string{"git"}, args[1:]...))
	default:
		return usage(ports.Err, fmt.Sprintf("unknown git subcommand %q", args[0]))
	}
}

// runGitConfigure configures per-repository (default) or --global Git
// credential helper and optional TLS CA settings for a Kodema Git origin.
// With --print, it writes the exact git config commands without executing git.
func runGitConfigure(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	var (
		originFlag  string
		caFileFlag  string
		sessionFlag string
		globalFlag  bool
		localFlag   bool
		printFlag   bool
		outputFlag  string
	)
	spec := newArgSpec()
	spec.stringFlag(&originFlag, "--origin")
	spec.stringFlag(&caFileFlag, "--ca-file")
	spec.stringFlag(&sessionFlag, "--session", "--session-file")
	spec.boolFlag(&globalFlag, "--global")
	spec.boolFlag(&localFlag, "--local")
	spec.boolFlag(&printFlag, "--print")
	spec.stringFlag(&outputFlag, "--output")
	positionals, _, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if len(positionals) > 1 {
		return usage(ports.Err, fmt.Sprintf("unexpected argument %q", positionals[1]))
	}
	if globalFlag && localFlag {
		return usage(ports.Err, "specify only one of --local or --global")
	}
	format, err := resolveOutputFormat(outputFlag, config)
	if err != nil {
		return usage(ports.Err, err.Error())
	}

	rawOrigin := strings.TrimSpace(originFlag)
	if rawOrigin == "" && len(positionals) == 1 {
		rawOrigin = strings.TrimSpace(positionals[0])
	}
	if rawOrigin == "" && len(config.TrustedOrigins) > 0 {
		rawOrigin = strings.TrimSpace(config.TrustedOrigins[0])
	}
	if rawOrigin == "" && strings.TrimSpace(config.ServerURL) != "" {
		rawOrigin = strings.TrimSpace(config.ServerURL)
	}
	if rawOrigin == "" {
		return usage(ports.Err, "git configure requires --origin <https://...> (or --server / --trusted-origin)")
	}
	parsedOrigin, err := ParseOrigin(rawOrigin)
	if err != nil {
		return usage(ports.Err, fmt.Sprintf("invalid origin %q: %v", rawOrigin, err))
	}
	origin := parsedOrigin.String()

	caFile := strings.TrimSpace(caFileFlag)
	if caFile == "" {
		caFile = strings.TrimSpace(config.CAFile)
	}
	sessionFile := strings.TrimSpace(sessionFlag)

	scopeFlag := "--local"
	if globalFlag {
		scopeFlag = "--global"
	}

	binary := strings.TrimSpace(deps.CommandPath)
	if binary == "" {
		binary = "kodema"
	}
	if caFile != "" {
		absCAFile, err := filepath.Abs(caFile)
		if err != nil {
			return failure(ports.Err, fmt.Errorf("cannot resolve CA file %q: %w", caFile, err))
		}
		caFile = absCAFile
	}
	if sessionFile != "" {
		absSessionFile, err := filepath.Abs(sessionFile)
		if err != nil {
			return failure(ports.Err, fmt.Errorf("cannot resolve session file %q: %w", sessionFile, err))
		}
		sessionFile = absSessionFile
	}
	helperParts := []string{shellQuoteGit(binary)}
	if server := strings.TrimSpace(config.ServerURL); server != "" {
		helperParts = append(helperParts, "--server", shellQuoteGit(server))
	}
	if caFile != "" {
		helperParts = append(helperParts, "--ca-file", shellQuoteGit(caFile))
	}
	if sessionFile != "" {
		helperParts = append(helperParts, "--session", shellQuoteGit(sessionFile))
	}
	helperParts = append(helperParts, "--trusted-origin", shellQuoteGit(origin), "git", "credential")
	helperValue := "!" + strings.Join(helperParts, " ")

	credentialKey := "credential." + origin + ".helper"
	commands := [][]string{
		{"config", scopeFlag, "--replace-all", "--", credentialKey, ""},
		{"config", scopeFlag, "--add", "--", credentialKey, helperValue},
	}
	if caFile != "" {
		commands = append(commands, []string{"config", scopeFlag, "--", "http." + origin + ".sslCAInfo", caFile})
	}

	if printFlag {
		if format == OutputJSON {
			payload := map[string]any{
				"origin":   origin,
				"scope":    strings.TrimPrefix(scopeFlag, "--"),
				"helper":   helperValue,
				"ca_file":  caFile,
				"commands": formatGitCommands(commands),
			}
			if err := json.NewEncoder(ports.Out).Encode(payload); err != nil {
				return failure(ports.Err, err)
			}
			return ExitSuccess
		}
		for _, line := range formatGitCommands(commands) {
			fmt.Fprintln(ports.Out, line)
		}
		return ExitSuccess
	}

	if deps.GitConfig == nil {
		return failure(ports.Err, fmt.Errorf("git config runner is not configured (use --print to output commands)"))
	}
	for _, cmdArgs := range commands {
		if err := deps.GitConfig(ctx, cmdArgs); err != nil {
			return failure(ports.Err, fmt.Errorf("git %s failed: %w", strings.Join(cmdArgs[:2], " "), err))
		}
	}

	if format == OutputJSON {
		payload := map[string]any{
			"origin":  origin,
			"scope":   strings.TrimPrefix(scopeFlag, "--"),
			"helper":  helperValue,
			"ca_file": caFile,
		}
		if err := json.NewEncoder(ports.Out).Encode(payload); err != nil {
			return failure(ports.Err, err)
		}
		return ExitSuccess
	}
	fmt.Fprintf(ports.Out, "configured git %s credentials for %s\n", strings.TrimPrefix(scopeFlag, "--"), origin)
	if caFile != "" {
		fmt.Fprintf(ports.Out, "configured git %s sslCAInfo %s for %s\n", strings.TrimPrefix(scopeFlag, "--"), caFile, origin)
	}
	return ExitSuccess
}

func formatGitCommands(commands [][]string) []string {
	lines := make([]string, 0, len(commands))
	for _, cmd := range commands {
		quoted := make([]string, 0, len(cmd)+1)
		quoted = append(quoted, "git")
		for _, arg := range cmd {
			quoted = append(quoted, shellQuoteGit(arg))
		}
		lines = append(lines, strings.Join(quoted, " "))
	}
	return lines
}

func shellQuoteGit(value string) string {
	if value == "" {
		return "''"
	}
	safe := true
	for i := 0; i < len(value); i++ {
		c := value[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '/' || c == '.' || c == '-' || c == '_' || c == ':' || c == '@') {
			safe = false
			break
		}
	}
	if safe {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// runGitCredential answers one Git credential-helper request. Git passes the
// operation as the argument and the request attributes on standard input; a
// URL argument replaces them. Only a `get` request reads the credential source;
// every refusal is an empty success, so Git falls through to its next helper.
func runGitCredential(ctx context.Context, config Config, args []string, ports Ports) int {
	if len(args) != 1 && len(args) != 2 {
		return usage(ports.Err, "git credential requires an operation and at most one URL")
	}
	request := HelperRequest{Protocol: args[0]}
	switch request.Protocol {
	case "get", "store", "erase":
	default:
		return usage(ports.Err, fmt.Sprintf("unsupported helper protocol %q", request.Protocol))
	}
	if len(args) == 2 {
		request.URL = args[1]
		if err := request.Validate(); err != nil {
			return usage(ports.Err, err.Error())
		}
	}
	if err := validateTrustedOrigins(config); err != nil {
		return usage(ports.Err, err.Error())
	}
	if request.Protocol != "get" {
		return ExitSuccess
	}
	if request.URL == "" {
		if ports.In == nil {
			return usage(ports.Err, "git credential get requires a URL or Git's request on standard input")
		}
		target, ok := gitCredentialRequestURL(ports.In)
		if !ok {
			return ExitSuccess
		}
		request.URL = target
		if request.Validate() != nil {
			return ExitSuccess
		}
	}
	if ports.Credential == nil {
		return failure(ports.Err, fmt.Errorf("no credential source is configured"))
	}
	if !matchesTrustedOrigin(config, request.URL) {
		return ExitSuccess
	}
	credential, err := ports.Credential.Credential(ctx)
	if err != nil {
		fmt.Fprintf(ports.Err, "kodema git credential: %v\n", err)
		return ExitSuccess
	}
	response, err := HelperDecision(config, request, credential, ports.Now())
	if err != nil {
		return ExitSuccess
	}
	if response.Username == "" || response.Password == "" {
		return ExitSuccess
	}
	fmt.Fprintf(ports.Out, "username=%s\npassword=%s\n", response.Username, response.Password)
	return ExitSuccess
}

// matchesTrustedOrigin reports whether targetURL is a valid https target under
// at least one registered trusted origin in config.
func matchesTrustedOrigin(config Config, targetURL string) bool {
	target, err := ParseOrigin(targetURL)
	if err != nil {
		return false
	}
	for _, raw := range config.TrustedOrigins {
		origin, err := ParseOrigin(raw)
		if err != nil {
			continue
		}
		if origin.Matches(target.String()) {
			return true
		}
	}
	return false
}

// gitCredentialRequestURL reads the `key=value` attributes of one Git
// credential request, up to the first empty line, and returns the URL they
// name. A request without a protocol or a host names nothing.
func gitCredentialRequestURL(input io.Reader) (string, bool) {
	// Read one byte past the bound so truncation cannot turn a partial
	// attribute into a successfully completed request.
	inputLimit := &io.LimitedReader{R: input, N: maxGitCredentialRequestBytes + 1}
	scanner := bufio.NewScanner(inputLimit)
	var protocol, host, path string
	complete := false
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			complete = true
			break
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			return "", false
		}
		switch key {
		case "protocol":
			protocol = value
		case "host":
			host = value
		case "path":
			path = value
		}
	}
	if scanner.Err() != nil || (!complete && inputLimit.N == 0) || protocol != "https" || host == "" || strings.ContainsAny(host, "/?#@\\") {
		return "", false
	}
	target := protocol + "://" + host
	if path != "" {
		target += "/" + strings.TrimPrefix(path, "/")
	}
	return target, true
}
