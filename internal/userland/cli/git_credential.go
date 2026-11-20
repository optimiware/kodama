package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
)

// maxGitCredentialRequestBytes bounds the request Git writes on standard input.
const maxGitCredentialRequestBytes = 64 << 10

// runGit dispatches the client's `kodema git <credential>`.
func runGit(ctx context.Context, config Config, args []string, ports Ports) int {
	if len(args) == 0 {
		return usage(ports.Err, "git requires a subcommand")
	}
	switch args[0] {
	case "credential":
		if hasHelpFlag(args[1:]) {
			return runHelp(ports.Out, ports.Err, []string{"git", "credential"})
		}
		return runGitCredential(ctx, config, args[1:], ports)
	case "help", "--help", "-h":
		return runHelp(ports.Out, ports.Err, []string{"git"})
	default:
		return usage(ports.Err, fmt.Sprintf("unknown git subcommand %q", args[0]))
	}
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
