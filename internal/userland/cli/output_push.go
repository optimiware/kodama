package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/optimiware/kodema/api"
)

const (
	// outputRoot is where the runtime mounts the output directory of a
	// container action, under the action's own name.
	outputRoot = "/kodema/outputs"
	// outputPushAttempts bounds the reads after a version conflict.
	outputPushAttempts = 5
	// An action may declare at most eight output keys.
	maxOutputFiles = 8
)

// runOutput dispatches `kodema output <push>`.
func runOutput(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if len(args) == 0 {
		return usage(ports.Err, "output requires a subcommand")
	}
	switch args[0] {
	case "push":
		if hasHelpFlag(args[1:]) {
			return runHelp(ports.Out, ports.Err, []string{"output", "push"})
		}
		return runOutputPush(ctx, config, args[1:], ports, deps)
	case "help", "--help", "-h":
		return runHelp(ports.Out, ports.Err, []string{"output"})
	default:
		return usage(ports.Err, fmt.Sprintf("unknown output subcommand %q", args[0]))
	}
}

// runOutputPush submits the files of one container action's output directory
// as that action's outputs. A file name is an output key and its content the
// value, with exactly one trailing line feed removed. An absent or empty
// directory submits nothing.
func runOutputPush(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	var directory, execution, action string
	spec := newArgSpec()
	spec.stringFlag(&directory, "--dir")
	spec.stringFlag(&execution, "--execution")
	spec.stringFlag(&action, "--action")
	positionals, _, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if len(positionals) != 0 {
		return usage(ports.Err, "output push takes no arguments")
	}
	getenv := deps.Login.Environment
	if getenv == nil {
		getenv = os.Getenv
	}
	if execution = strings.TrimSpace(execution); execution == "" {
		execution = strings.TrimSpace(getenv("KODEMA_EXECUTION"))
	}
	if action = strings.TrimSpace(action); action == "" {
		action = strings.TrimSpace(getenv("KODEMA_ACTION"))
	}
	if execution == "" || action == "" {
		return usage(ports.Err, "output push requires an execution and an action (KODEMA_EXECUTION and KODEMA_ACTION, or --execution and --action)")
	}
	if directory == "" {
		directory = outputRoot + "/" + action
	}
	if ports.Files == nil {
		return failure(ports.Err, fmt.Errorf("no filesystem is configured"))
	}
	outputs, err := readOutputFiles(ports.Files, directory)
	if err != nil {
		return failure(ports.Err, err)
	}
	if len(outputs) == 0 {
		return ExitSuccess
	}
	if err := config.Validate(); err != nil {
		return usage(ports.Err, err.Error())
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}

	keys := make([]string, 0, len(outputs))
	for key := range outputs {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var conflict error
	for range outputPushAttempts {
		inspected, err := fetchExecutionInspect(ctx, ports, execution)
		if err != nil {
			return failure(ports.Err, err)
		}
		actionID := ""
		for _, candidate := range inspected.Actions {
			if candidate.Name == action || candidate.ActionID == action {
				actionID = candidate.ActionID
				break
			}
		}
		if actionID == "" {
			return failure(ports.Err, fmt.Errorf("action %q is not part of execution %q", action, execution))
		}
		err = submitOutputs(ctx, ports, execution, actionID, inspected.Version, outputs)
		if err == nil {
			fmt.Fprintf(ports.Err, "kodema: submitted outputs %s\n", strings.Join(keys, ", "))
			return ExitSuccess
		}
		if public, ok := errors.AsType[*api.Error](err); !ok || public.Code != api.CodeVersionConflict {
			return failure(ports.Err, fmt.Errorf("outputs %s were not accepted: %w", strings.Join(keys, ", "), err))
		}
		conflict = err
	}
	return failure(ports.Err, fmt.Errorf("outputs %s were not accepted after %d attempts: %w",
		strings.Join(keys, ", "), outputPushAttempts, conflict))
}

// readOutputFiles reads the files directly inside directory. An absent
// directory holds no output. A subdirectory, invalid UTF-8 and a value above
// the public bound are refused before any request is made.
func readOutputFiles(files FileSystem, directory string) (map[string]string, error) {
	entries, err := files.ReadDir(directory)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("output directory %q is unreadable: %w", directory, err)
	}
	if len(entries) > maxOutputFiles {
		return nil, fmt.Errorf("output directory contains %d entries; at most %d are allowed", len(entries), maxOutputFiles)
	}
	outputs := make(map[string]string, len(entries))
	for _, entry := range entries {
		if entry.IsDir {
			return nil, fmt.Errorf("output %q is a directory; an output is one file named for its key", entry.Name)
		}
		// Allow a single trailing LF and one more byte to distinguish an
		// oversized file from a valid upper-bound value.
		data, err := files.ReadRegularFile(directory+"/"+entry.Name, api.MaxSubjectValueBytes+2)
		if err != nil {
			return nil, fmt.Errorf("output %q is unreadable: %w", entry.Name, err)
		}
		value := strings.TrimSuffix(string(data), "\n")
		if len(value) > api.MaxSubjectValueBytes {
			return nil, fmt.Errorf("output %q exceeds %d bytes", entry.Name, api.MaxSubjectValueBytes)
		}
		if !utf8.ValidString(value) {
			return nil, fmt.Errorf("output %q is not valid UTF-8", entry.Name)
		}
		outputs[entry.Name] = value
	}
	return outputs, nil
}

// submitOutputs sends one output map at the observed execution version. The
// idempotency key is derived from the execution, action and values, so a repeated push
// of the same files converges and a retry after a conflict reuses it.
func submitOutputs(ctx context.Context, ports Ports, execution, actionID string, version uint64, outputs map[string]string) error {
	input, err := json.Marshal(api.ExecutionActionOutputsRequest{
		ExecutionID: execution,
		ActionID:    actionID,
		Outputs:     outputs,
	})
	if err != nil {
		return err
	}
	// json.Marshal orders map keys. Hash the whole logical request to include
	// both target IDs without letting their lengths inflate the caller key.
	digest := sha256.Sum256(input)
	invocation := api.Invocation{
		Operation: "execution.action.outputs",
		Request: api.Request{
			Input:          input,
			IdempotencyKey: "output-push:" + hex.EncodeToString(digest[:16]),
		},
	}
	if version > 0 {
		invocation.Request.ExpectedVersion = &version
	}
	if err := invocation.Validate(); err != nil {
		return err
	}
	_, err = ports.Client.Invoke(ctx, invocation)
	return err
}
