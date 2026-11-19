package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/optimiware/kodema/api"
)

// pipelineApplyInput is the pipeline.apply input for one local pipeline file.
// YAML is []byte so the public DTO's base64 wire encoding is preserved; a
// string field would send raw YAML and the REST edge would refuse it.
type pipelineApplyInput struct {
	Project  string `json:"project_id"`
	Pipeline string `json:"name"`
	YAML     []byte `json:"yaml"`
}

// pipelineFile is one accepted directory entry.
type pipelineFile struct {
	name string
	base string
	path string
}

// runPipeline applies every local pipeline file. It never claims directory
// atomicity: each file is attempted in sorted order and every failure is
// visible on stderr.
func runPipeline(ctx context.Context, config Config, args []string, ports Ports) int {
	if len(args) != 2 || args[0] != "apply" {
		return usage(ports.Err, "pipeline requires 'apply <directory|file>'")
	}
	if err := config.Validate(); err != nil {
		return usage(ports.Err, err.Error())
	}
	if config.Project == "" {
		return usage(ports.Err, "pipeline apply requires a configured project")
	}
	directory := args[1]
	if ports.Files == nil {
		return failure(ports.Err, fmt.Errorf("no filesystem is configured"))
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}
	entries, err := ports.Files.ReadDir(directory)
	if err != nil {
		// Not a readable directory: try it as one pipeline file.
		entries = []DirEntry{{Name: filepath.Base(directory)}}
		directory = filepath.Dir(directory)
	}
	files := acceptedPipelineFiles(directory, entries)
	if err != nil && len(files) == 0 {
		return failure(ports.Err, fmt.Errorf("cannot read pipeline directory: %w", err))
	}
	counts := make(map[string]int, len(files))
	for _, file := range files {
		counts[file.base]++
	}
	failed := false
	for _, file := range files {
		if counts[file.base] > 1 {
			fmt.Fprintf(ports.Err, "failed %s: duplicate pipeline mapping for %s\n", file.name, file.base)
			failed = true
			continue
		}
		data, err := ports.Files.ReadFile(file.path)
		if err != nil {
			fmt.Fprintf(ports.Err, "failed %s: pipeline file is unreadable\n", file.name)
			failed = true
			continue
		}
		if !utf8.Valid(data) {
			fmt.Fprintf(ports.Err, "failed %s: pipeline YAML is not valid UTF-8\n", file.name)
			failed = true
			continue
		}
		input, err := json.Marshal(pipelineApplyInput{
			Project:  config.Project,
			Pipeline: file.base,
			YAML:     data,
		})
		if err != nil {
			fmt.Fprintf(ports.Err, "failed %s: pipeline input is not encodable\n", file.name)
			failed = true
			continue
		}
		invocation := api.Invocation{
			Operation: "pipeline.apply",
			Request: api.Request{
				Input:          input,
				IdempotencyKey: pipelineKey(config.Project, file.base, data),
			},
		}
		if err := invocation.Validate(); err != nil {
			fmt.Fprintf(ports.Err, "failed %s: %s\n", file.name, publicMessage(err))
			failed = true
			continue
		}
		result, err := ports.Client.Invoke(ctx, invocation)
		if err != nil {
			fmt.Fprintf(ports.Err, "failed %s: %v\n", file.name, err)
			failed = true
			continue
		}
		var applied api.PipelineApplyResult
		if json.Unmarshal(result.Body, &applied) == nil && applied.PipelineID != "" {
			fmt.Fprintf(ports.Out, "applied %s (pipeline_id=%s)\n", file.base, applied.PipelineID)
			continue
		}
		fmt.Fprintf(ports.Out, "applied %s\n", file.base)
	}
	if failed {
		return ExitFailure
	}
	return ExitSuccess
}

// runRun starts one local pipeline file inline through pipeline.start and
// follows the execution to its end. Container stdout goes to stdout in action
// order once the execution has ended; the execution id and the outcome go to
// stderr.
func runRun(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	var (
		projectFlag string
		inputPairs  []string
	)
	spec := newArgSpec()
	spec.stringFlag(&projectFlag, "--project", "-p")
	spec.listFlag(&inputPairs, "--input")
	positionals, _, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if len(positionals) != 1 || positionals[0] == "" {
		return usage(ports.Err, "run requires exactly one pipeline file")
	}
	inputs, err := parseKeyValues(inputPairs, "input")
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if err := config.Validate(); err != nil {
		return usage(ports.Err, err.Error())
	}
	projectID := resolveProject(projectFlag, config, deps)
	if projectID == "" {
		return usage(ports.Err, "run requires --project <id> or KODEMA_PROJECT")
	}
	if ports.Files == nil {
		return failure(ports.Err, fmt.Errorf("no filesystem is configured"))
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}
	data, err := ports.Files.ReadFile(positionals[0])
	if err != nil {
		return failure(ports.Err, fmt.Errorf("cannot read pipeline file: %w", err))
	}
	if len(data) == 0 {
		return failure(ports.Err, fmt.Errorf("pipeline file %s is empty", positionals[0]))
	}
	request := api.PipelineStartRequest{ProjectID: projectID, YAML: data, Inputs: inputs}
	if err := request.Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	input, err := json.Marshal(request)
	if err != nil {
		return failure(ports.Err, err)
	}
	key, err := makeIdempotencyKey("", "pipeline.start", deps.Login.Random)
	if err != nil {
		return failure(ports.Err, err)
	}
	result, err := ports.Client.Invoke(ctx, api.Invocation{
		Operation: "pipeline.start",
		Request:   api.Request{Input: input, IdempotencyKey: key},
	})
	if err != nil {
		return failure(ports.Err, err)
	}
	var started api.PipelineStartResult
	if err := json.Unmarshal(result.Body, &started); err != nil || started.ExecutionID == "" {
		return failure(ports.Err, fmt.Errorf("pipeline.start returned no execution id"))
	}
	executionID := started.ExecutionID
	fmt.Fprintf(ports.Err, "execution %s started\n", executionID)

	fail := func(err error) int {
		if ctx.Err() != nil {
			fmt.Fprintf(ports.Err, "run interrupted: execution %s is still running\n", executionID)
			return ExitFailure
		}
		return failure(ports.Err, err)
	}
	for settling := 0; ; {
		inspected, err := fetchExecutionInspect(ctx, ports, executionID)
		if err != nil {
			return fail(err)
		}
		state := inspected.effectiveState()
		switch state {
		case "succeeded", "failed", "cancelled":
			if logsSettling(inspected, &settling) {
				ports.Sleep(watchInterval)
				continue
			}
			for _, action := range inspected.Actions {
				if action.Kind == "request" || action.effectiveState() == "pending" {
					continue
				}
				logs := logStreamState{actionID: action.ActionID}
				if _, _, err := drainLogPages(ctx, ports, executionID, "stdout", OutputTable, false, &logs); err != nil {
					return fail(err)
				}
			}
			if state == "succeeded" {
				fmt.Fprintf(ports.Err, "execution %s succeeded\n", executionID)
				return ExitSuccess
			}
			message := fmt.Sprintf("execution %s %s", executionID, state)
			if reported := inspected.Failure; reported != nil {
				for _, part := range []string{reported.Kind, reported.Reason, reported.Detail} {
					if part != "" {
						message += ": " + part
					}
				}
			}
			fmt.Fprintln(ports.Err, message)
			return ExitFailure
		case "queued", "running":
			for _, action := range inspected.Actions {
				// A request assigned to a role waits for a human; one assigned
				// to a service is answered without this caller.
				if action.Kind == "request" && action.effectiveState() == "running" && strings.HasPrefix(action.Assignment, "role/") {
					fmt.Fprintf(ports.Err, "execution %s is waiting for %s to answer request %q; see 'kodema inbox' and 'kodema approve %s'\n",
						executionID, action.Assignment, action.Name, executionID)
					return ExitFailure
				}
			}
			ports.Sleep(watchInterval)
		default:
			return failure(ports.Err, fmt.Errorf("execution status %q is unknown", state))
		}
	}
}

// acceptedPipelineFiles filters and sorts the directory non-recursively: no
// directories, dotfiles or names outside .yaml/.yml, sorted by file name.
func acceptedPipelineFiles(directory string, entries []DirEntry) []pipelineFile {
	files := make([]pipelineFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir {
			continue
		}
		name := entry.Name
		if name == "" || strings.HasPrefix(name, ".") {
			continue
		}
		var base string
		switch {
		case strings.HasSuffix(name, ".yaml"):
			base = strings.TrimSuffix(name, ".yaml")
		case strings.HasSuffix(name, ".yml"):
			base = strings.TrimSuffix(name, ".yml")
		default:
			continue
		}
		if base == "" {
			continue
		}
		files = append(files, pipelineFile{name: name, base: base, path: filepath.Join(directory, name)})
	}
	sort.Slice(files, func(first, second int) bool {
		return files[first].name < files[second].name
	})
	return files
}

// pipelineKey derives the stable caller key for one project/pipeline/exact-YAML
// triple. Replaying the same directory sends the same key.
func pipelineKey(project, pipeline string, yaml []byte) string {
	digest := sha256.New()
	digest.Write([]byte(project))
	digest.Write([]byte{0})
	digest.Write([]byte(pipeline))
	digest.Write([]byte{0})
	digest.Write(yaml)
	return "pipeline.apply:" + hex.EncodeToString(digest.Sum(nil))
}
