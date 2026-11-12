package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/fault"
)

// watchInterval is the deterministic delay between execution.inspect polls.
const watchInterval = time.Second

// Run executes one kodactl command and returns its exit code. It never calls
// os.Exit; every collaborator is injected through ports. Run is the legacy
// entrypoint: the real OAuth login/logout/refresh commands need the runtime
// collaborators that RunWithDeps accepts.
func Run(ctx context.Context, config Config, args []string, ports Ports) int {
	return RunWithDeps(ctx, config, args, ports, CommandDeps{})
}

// RunWithDeps executes one kodactl command with the optional command-runtime
// collaborators injected. It never calls os.Exit.
func RunWithDeps(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if ctx == nil {
		ctx = context.Background()
	}
	if ports.Out == nil {
		ports.Out = io.Discard
	}
	if ports.Err == nil {
		ports.Err = io.Discard
	}
	if ports.Now == nil {
		ports.Now = time.Now
	}
	if ports.Sleep == nil {
		ports.Sleep = time.Sleep
	}
	if len(args) == 0 {
		return usage(ports.Err, "a command is required")
	}
	switch args[0] {
	case "call":
		return runCall(ctx, config, args[1:], ports)
	case "inspect":
		return runInspect(ctx, config, args[1:], ports)
	case "watch":
		return runWatch(ctx, config, args[1:], ports)
	case "login":
		return runLogin(ctx, config, args[1:], ports, deps.Login)
	case "logout":
		return runLogout(ctx, config, args[1:], ports, deps.Login)
	case "refresh":
		return runRefresh(ctx, config, args[1:], ports, deps.Login)
	case "status":
		return runStatus(ctx, config, args[1:], ports, deps.Login)
	case "pipeline":
		return runPipeline(ctx, config, args[1:], ports)
	case "helper":
		return runHelper(ctx, config, args[1:], ports)
	default:
		return usage(ports.Err, fmt.Sprintf("unknown command %q", args[0]))
	}
}

// runCall dispatches one served operation with optional input, version and key.
func runCall(ctx context.Context, config Config, args []string, ports Ports) int {
	if len(args) == 0 {
		return usage(ports.Err, "call requires an operation")
	}
	if err := config.Validate(); err != nil {
		return usage(ports.Err, err.Error())
	}
	operation := args[0]
	if _, ok := api.LookupRoute(operation); !ok {
		return usage(ports.Err, fmt.Sprintf("unknown operation %q", operation))
	}
	var request api.Request
	seen := make(map[string]bool, 3)
	for _, argument := range args[1:] {
		name, value, ok := strings.Cut(argument, "=")
		if !ok || !strings.HasPrefix(name, "--") {
			return usage(ports.Err, fmt.Sprintf("unexpected argument %q", argument))
		}
		if seen[name] {
			return usage(ports.Err, fmt.Sprintf("flag %q is repeated", name))
		}
		seen[name] = true
		switch name {
		case "--input":
			request.Input = json.RawMessage(value)
		case "--expected-version":
			version, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return usage(ports.Err, fmt.Sprintf("expected version %q is not a positive integer", value))
			}
			request.ExpectedVersion = &version
		case "--idempotency-key":
			request.IdempotencyKey = value
		default:
			return usage(ports.Err, fmt.Sprintf("unknown flag %q", name))
		}
	}
	invocation := api.Invocation{Operation: operation, Request: request}
	if err := invocation.Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}
	result, err := ports.Client.Invoke(ctx, invocation)
	if err != nil {
		return failure(ports.Err, err)
	}
	if err := renderResult(ports.Out, config.Output, result); err != nil {
		return failure(ports.Err, err)
	}
	return ExitSuccess
}

// runInspect reads one execution once and renders it.
func runInspect(ctx context.Context, config Config, args []string, ports Ports) int {
	if len(args) != 1 || args[0] == "" {
		return usage(ports.Err, "inspect requires exactly one execution")
	}
	if err := config.Validate(); err != nil {
		return usage(ports.Err, err.Error())
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}
	invocation, err := executionInvocation(args[0])
	if err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	result, err := ports.Client.Invoke(ctx, invocation)
	if err != nil {
		return failure(ports.Err, err)
	}
	if err := renderResult(ports.Out, config.Output, result); err != nil {
		return failure(ports.Err, err)
	}
	return ExitSuccess
}

// runWatch polls execution.inspect until a terminal status or cancellation.
func runWatch(ctx context.Context, config Config, args []string, ports Ports) int {
	if len(args) != 1 || args[0] == "" {
		return usage(ports.Err, "watch requires exactly one execution")
	}
	if err := config.Validate(); err != nil {
		return usage(ports.Err, err.Error())
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}
	invocation, err := executionInvocation(args[0])
	if err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	for {
		if ctx.Err() != nil {
			return watchCanceled(ports.Err)
		}
		result, err := ports.Client.Invoke(ctx, invocation)
		if err != nil {
			if ctx.Err() != nil {
				return watchCanceled(ports.Err)
			}
			return failure(ports.Err, err)
		}
		if err := renderResult(ports.Out, config.Output, result); err != nil {
			return failure(ports.Err, err)
		}
		status, ok := executionStatus(result.Body)
		if !ok {
			return failure(ports.Err, fmt.Errorf("execution status is missing or unknown"))
		}
		switch status {
		case "succeeded":
			return ExitSuccess
		case "failed", "cancelled":
			fmt.Fprintf(ports.Err, "watch failed: execution status %s\n", status)
			return ExitFailure
		case "queued", "running":
			ports.Sleep(watchInterval)
		default:
			return failure(ports.Err, fmt.Errorf("execution status %q is unknown", status))
		}
	}
}

// runLogin parses the login flags and runs the configured OAuth runtime, or
// keeps the seeded-session compatibility path when no runtime was
// injected. The compatibility path only re-persists an already-current human
// credential; the real flow discovers the API-bound provider configuration and
// obtains a fresh version 2 session.
func runLogin(ctx context.Context, config Config, args []string, ports Ports, runtime LoginRuntime) int {
	forceDevice, err := parseLoginFlags(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if !loginRuntimeConfigured(runtime) {
		if len(args) != 0 {
			return usage(ports.Err, "login takes no arguments")
		}
		return runLegacyLogin(ctx, config, ports)
	}
	return runConfiguredLogin(ctx, config, ports, runtime, forceDevice)
}

// runLegacyLogin reads a fresh human credential and persists it as the durable
// session. It is the entrypoint compatibility path; a source, kind,
// expiry or save failure writes nothing to stdout.
func runLegacyLogin(ctx context.Context, config Config, ports Ports) int {
	if err := config.Validate(); err != nil {
		return usage(ports.Err, err.Error())
	}
	if ports.Credential == nil {
		return failure(ports.Err, fmt.Errorf("no credential source is configured"))
	}
	credential, err := ports.Credential.Credential(ctx)
	if err != nil {
		return failure(ports.Err, err)
	}
	if credential.Kind != client.KindSession {
		return failure(ports.Err, fault.New(fault.Unauthenticated, "login requires a human session credential"))
	}
	if !credential.Usable(ports.Now()) {
		return failure(ports.Err, fault.New(fault.Unauthenticated, "the session is missing, expired or unusable"))
	}
	if ports.Session == nil {
		return failure(ports.Err, fmt.Errorf("no session store is configured"))
	}
	if err := ports.Session.Save(ctx, credential); err != nil {
		return failure(ports.Err, err)
	}
	fmt.Fprintln(ports.Out, "logged in")
	return ExitSuccess
}

// runHelper answers one Git credential-helper request. Only a `get` request
// reads the credential source; every refusal is an empty success.
func runHelper(ctx context.Context, config Config, args []string, ports Ports) int {
	if len(args) != 2 {
		return usage(ports.Err, "helper requires a protocol and a URL")
	}
	request := HelperRequest{Protocol: args[0], URL: args[1]}
	if err := request.Validate(); err != nil {
		return usage(ports.Err, err.Error())
	}
	if err := validateTrustedOrigins(config); err != nil {
		return usage(ports.Err, err.Error())
	}
	if request.Protocol != "get" {
		return ExitSuccess
	}
	if ports.Credential == nil {
		return failure(ports.Err, fmt.Errorf("no credential source is configured"))
	}
	credential, err := ports.Credential.Credential(ctx)
	if err != nil {
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

// executionInvocation builds the execution.inspect request.
func executionInvocation(execution string) (api.Invocation, error) {
	input, err := json.Marshal(map[string]string{"execution_id": execution})
	if err != nil {
		return api.Invocation{}, err
	}
	invocation := api.Invocation{
		Operation: "execution.inspect",
		Request:   api.Request{Input: input},
	}
	if err := invocation.Validate(); err != nil {
		return api.Invocation{}, err
	}
	return invocation, nil
}

// executionStatus reads the top-level status string from an execution body.
func executionStatus(body json.RawMessage) (string, bool) {
	if len(body) == 0 {
		return "", false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return "", false
	}
	raw, ok := fields["status"]
	if !ok {
		return "", false
	}
	var status string
	if err := json.Unmarshal(raw, &status); err != nil {
		return "", false
	}
	return status, true
}

// validateTrustedOrigins checks only the origins a helper invocation needs.
func validateTrustedOrigins(config Config) error {
	for _, raw := range config.TrustedOrigins {
		if _, err := ParseOrigin(raw); err != nil {
			return err
		}
	}
	return nil
}

// watchCanceled reports a canceled watch without rendering a success.
func watchCanceled(errOut io.Writer) int {
	fmt.Fprintln(errOut, "watch canceled")
	return ExitFailure
}

// usage writes a usage diagnostic and returns the usage exit code.
func usage(errOut io.Writer, message string) int {
	fmt.Fprintf(errOut, "usage error: %s\n", message)
	return ExitUsage
}

// failure writes a public diagnostic and returns the failure exit code.
func failure(errOut io.Writer, err error) int {
	fmt.Fprintf(errOut, "error: %v\n", err)
	return ExitFailure
}

// publicMessage returns the public message of an api.Error or the error text.
func publicMessage(err error) string {
	if public, ok := errors.AsType[*api.Error](err); ok {
		return public.Message
	}
	return err.Error()
}
