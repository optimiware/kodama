package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/client"
)

// runKV dispatches `kodema kv <put|get>`.
func runKV(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if len(args) == 0 {
		return usage(ports.Err, "kv requires a subcommand (put, get)")
	}
	switch args[0] {
	case "put":
		if hasHelpFlag(args[1:]) {
			return runHelp(ports.Out, ports.Err, []string{"kv", "put"})
		}
		return runKVPut(ctx, config, args[1:], ports, deps)
	case "get":
		if hasHelpFlag(args[1:]) {
			return runHelp(ports.Out, ports.Err, []string{"kv", "get"})
		}
		return runKVGet(ctx, config, args[1:], ports, deps)
	case "help", "--help", "-h":
		return runHelp(ports.Out, ports.Err, []string{"kv"})
	default:
		return usage(ports.Err, fmt.Sprintf("unknown kv subcommand %q (want put or get)", args[0]))
	}
}

func runKVPut(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	var (
		projectFlag     string
		contentTypeFlag string
		outputFlag      string
	)
	spec := newArgSpec()
	spec.stringFlag(&projectFlag, "--project", "-p")
	spec.stringFlag(&contentTypeFlag, "--content-type")
	spec.stringFlag(&outputFlag, "--output")
	positionals, _, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if len(positionals) > 1 {
		return usage(ports.Err, fmt.Sprintf("unexpected argument %q", positionals[1]))
	}
	format, err := resolveOutputFormat(outputFlag, config)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if err := config.Validate(); err != nil {
		return usage(ports.Err, err.Error())
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}

	var data []byte
	if len(positionals) == 1 && positionals[0] != "-" {
		if ports.Files == nil {
			return failure(ports.Err, fmt.Errorf("no filesystem is configured"))
		}
		data, err = ports.Files.ReadRegularFile(positionals[0], api.MaxKVValueBytes+1)
		if err != nil {
			return failure(ports.Err, fmt.Errorf("read %q: %w", positionals[0], err))
		}
	} else {
		if ports.In == nil {
			return usage(ports.Err, "kv put requires a file path or standard input")
		}
		data, err = io.ReadAll(io.LimitReader(ports.In, api.MaxKVValueBytes+1))
		if err != nil {
			return failure(ports.Err, fmt.Errorf("read standard input: %w", err))
		}
	}
	if len(data) > api.MaxKVValueBytes {
		return failure(ports.Err, fmt.Errorf("kv value exceeds maximum allowed size of %d bytes", api.MaxKVValueBytes))
	}

	projectID, err := resolveKVProject(ctx, projectFlag, config, ports, deps)
	if err != nil {
		return failure(ports.Err, err)
	}
	if projectID == "" {
		return usage(ports.Err, "kv put requires --project <id> (or KODEMA_PROJECT)")
	}

	result, err := client.KVPut(ctx, ports.Client, projectID, data, strings.TrimSpace(contentTypeFlag))
	if err != nil {
		return failure(ports.Err, err)
	}
	if format == OutputJSON {
		if err := json.NewEncoder(ports.Out).Encode(result); err != nil {
			return failure(ports.Err, err)
		}
		return ExitSuccess
	}
	if _, err := fmt.Fprintln(ports.Out, result.Key); err != nil {
		return failure(ports.Err, err)
	}
	return ExitSuccess
}

func runKVGet(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	var (
		projectFlag string
		outFlag     string
		outputFlag  string
	)
	spec := newArgSpec()
	spec.stringFlag(&projectFlag, "--project", "-p")
	spec.stringFlag(&outFlag, "--out")
	spec.stringFlag(&outputFlag, "--output")
	positionals, _, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if len(positionals) != 1 || strings.TrimSpace(positionals[0]) == "" {
		return usage(ports.Err, "kv get requires exactly one key (kv:sha256:<hex>)")
	}
	key := strings.TrimSpace(positionals[0])
	if !api.ValidKVKey(key) {
		return usage(ports.Err, fmt.Sprintf("invalid kv key %q (want %s<64-lowercase-hex>)", key, api.KVKeyPrefix))
	}
	format, err := resolveOutputFormat(outputFlag, config)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if err := config.Validate(); err != nil {
		return usage(ports.Err, err.Error())
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}

	projectID, err := resolveKVProject(ctx, projectFlag, config, ports, deps)
	if err != nil {
		return failure(ports.Err, err)
	}
	if projectID == "" {
		return usage(ports.Err, "kv get requires --project <id> (or KODEMA_PROJECT)")
	}

	obj, err := client.KVGet(ctx, ports.Client, projectID, key)
	if err != nil {
		return failure(ports.Err, err)
	}
	if outPath := strings.TrimSpace(outFlag); outPath != "" {
		if ports.Files == nil {
			return failure(ports.Err, fmt.Errorf("no filesystem is configured"))
		}
		if err := ports.Files.WriteFile(outPath, obj.Data, 0o644); err != nil {
			return failure(ports.Err, fmt.Errorf("write %q: %w", outPath, err))
		}
		if format == OutputJSON {
			if err := json.NewEncoder(ports.Out).Encode(obj.Raw); err != nil {
				return failure(ports.Err, err)
			}
		}
		return ExitSuccess
	}
	if format == OutputJSON {
		if err := json.NewEncoder(ports.Out).Encode(obj.Raw); err != nil {
			return failure(ports.Err, err)
		}
		return ExitSuccess
	}
	if _, err := ports.Out.Write(obj.Data); err != nil {
		return failure(ports.Err, err)
	}
	return ExitSuccess
}

// resolveKVProject resolves the target project from --project, config,
// KODEMA_PROJECT, or (inside a workload container where KODEMA_EXECUTION is
// set) automatically from principal.whoami.
func resolveKVProject(ctx context.Context, flagValue string, config Config, ports Ports, deps CommandDeps) (string, error) {
	if projectID := resolveProject(flagValue, config, deps); projectID != "" {
		return projectID, nil
	}
	getenv := deps.Env
	if getenv == nil {
		getenv = deps.Login.Environment
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	if projectEnv := strings.TrimSpace(getenv("KODEMA_PROJECT")); projectEnv != "" {
		return projectEnv, nil
	}
	if strings.TrimSpace(getenv("KODEMA_EXECUTION")) == "" {
		return "", nil
	}
	res, err := ports.Client.Invoke(ctx, api.Invocation{
		Operation: "principal.whoami",
		Request:   api.Request{Input: json.RawMessage(`{}`)},
	})
	if err != nil {
		return "", fmt.Errorf("resolve workload project via principal.whoami: %w", err)
	}
	var whoami api.PrincipalWhoamiResult
	if err := json.Unmarshal(res.Body, &whoami); err != nil {
		return "", fmt.Errorf("decode principal.whoami response: %w", err)
	}
	return strings.TrimSpace(whoami.ProjectID), nil
}
