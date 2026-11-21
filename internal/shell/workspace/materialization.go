// Activation materialization and reserved execution environment.
//
// BuildMaterialization validates every admitted input before it resolves any
// declared secret, then returns one complete deep-copied snapshot. Any failure
// returns the zero Materialization: a partial snapshot never authorizes a
// start. Environment merges the already action-precedence-merged literal
// environment with the reserved context and never emits a secret value or the
// execution credential.
package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"os"
	"slices"

	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/kernel/engine"
	"github.com/optimiware/kodema/internal/shell/secret"
	"github.com/optimiware/kodema/internal/shell/tenancy"
)

// materializedFileMode is the explicit regular, read-only mode of every
// immutable prepared file. Preparation never materializes a writable file.
const materializedFileMode os.FileMode = 0o444

// Relative filenames materialized under /kodema/task/ for task-associated runs.
const (
	TaskTitleFile       = "title"
	TaskDescriptionFile = "description"
	TaskMetadataFile    = "metadata.json"
	TaskSpecFile        = "spec.json"
)

type taskSpecDocument struct {
	ID          id.TaskID         `json:"id"`
	ParentID    id.TaskID         `json:"parent_id,omitempty"`
	Title       string            `json:"title"`
	Description string            `json:"description"`
	Metadata    map[string]string `json:"metadata"`
}

// BuildTaskFiles builds the deterministic read-only file set materialized under
// /kodema/task/ for a task-associated execution: title (UTF-8), description
// (UTF-8), metadata.json (deterministic JSON object, "{}" when empty), and
// spec.json (structured JSON object combining id, title, description and
// metadata). For a task-free execution (empty taskID), it returns nil.
func BuildTaskFiles(taskID id.TaskID, title, description string, metadata map[string]string) (map[string][]byte, error) {
	return BuildTaskFilesWithParent(taskID, "", title, description, metadata)
}

// BuildTaskFilesWithParent builds the deterministic read-only file set
// materialized under /kodema/task/ for a task-associated execution, including
// optional parent_id in spec.json when set. For a task-free execution (empty
// taskID), it returns nil.
func BuildTaskFilesWithParent(taskID, parentID id.TaskID, title, description string, metadata map[string]string) (map[string][]byte, error) {
	if taskID == "" {
		return nil, nil
	}
	meta := make(map[string]string, len(metadata))
	maps.Copy(meta, metadata)
	metadataJSON, err := marshalCompactJSON(meta)
	if err != nil {
		return nil, fault.Wrap(fault.InternalFailure, "task metadata cannot be encoded", err)
	}
	specJSON, err := marshalCompactJSON(taskSpecDocument{
		ID:          taskID,
		ParentID:    parentID,
		Title:       title,
		Description: description,
		Metadata:    meta,
	})
	if err != nil {
		return nil, fault.Wrap(fault.InternalFailure, "task specification cannot be encoded", err)
	}
	return map[string][]byte{
		TaskTitleFile:       []byte(title),
		TaskDescriptionFile: []byte(description),
		TaskMetadataFile:    metadataJSON,
		TaskSpecFile:        specJSON,
	}, nil
}

// TaskMaterializationFiles validates, sorts and deep-copies opaque task files
// into confined immutable /kodema/task/<name> File entries with the given
// read-only mode.
func TaskMaterializationFiles(taskFiles map[string][]byte, mode os.FileMode) ([]File, error) {
	if len(taskFiles) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(taskFiles))
	for name := range taskFiles {
		names = append(names, name)
	}
	slices.Sort(names)
	files := make([]File, 0, len(names))
	for _, name := range names {
		path, err := TaskFilePath(name)
		if err != nil {
			return nil, err
		}
		data := append([]byte(nil), taskFiles[name]...)
		if data == nil {
			data = []byte{}
		}
		files = append(files, File{
			Path: path,
			Data: data,
			Mode: mode,
		})
	}
	return files, nil
}

func marshalCompactJSON(value any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}

// MaterializationConfig is the activation-time configuration for one snapshot.
// Scope must be a valid ownership scope. Secrets may be nil only when the
// request declares no secret names.
type MaterializationConfig struct {
	Scope   tenancy.Scope
	Secrets secret.Resolver
}

// BuildMaterialization resolves the request's declared secrets at activation
// and returns one complete Materialization. Files carries the admitted inputs,
// the earlier succeeded outputs and the optional task snapshot; Secrets carries
// the resolved read-only secret values; Credential carries the sole mutable
// mounted credential. Any failure returns the zero Materialization and an
// error, never a partial snapshot.
func BuildMaterialization(ctx context.Context, request engine.PrepareRequest, config MaterializationConfig) (Materialization, error) {
	if err := validatePrepareRequest(request, config); err != nil {
		return Materialization{}, err
	}

	files := make([]File, 0, len(request.Inputs)+1)
	inputKeys := make([]id.InputKey, 0, len(request.Inputs))
	for key := range request.Inputs {
		inputKeys = append(inputKeys, key)
	}
	slices.Sort(inputKeys)
	for _, key := range inputKeys {
		path, err := InputPath(key)
		if err != nil {
			return Materialization{}, err
		}
		files = append(files, File{
			Path: path,
			Data: []byte(request.Inputs[key]),
			Mode: materializedFileMode,
		})
	}

	actions := make([]id.ActionName, 0, len(request.Outputs))
	for action := range request.Outputs {
		actions = append(actions, action)
	}
	slices.Sort(actions)
	for _, action := range actions {
		outputs := request.Outputs[action]
		if outputs == nil {
			continue
		}
		outputKeys := make([]id.OutputKey, 0, len(outputs))
		for key := range outputs {
			outputKeys = append(outputKeys, key)
		}
		slices.Sort(outputKeys)
		for _, key := range outputKeys {
			path, err := OutputPath(action, key)
			if err != nil {
				return Materialization{}, err
			}
			files = append(files, File{
				Path: path,
				Data: []byte(outputs[key]),
				Mode: materializedFileMode,
			})
		}
	}

	var task []byte
	if request.Task != nil {
		task = append([]byte(nil), request.Task...)
		files = append(files, File{Path: TaskPath, Data: append([]byte(nil), task...), Mode: materializedFileMode})
		taskFiles, err := TaskMaterializationFiles(request.TaskFiles, materializedFileMode)
		if err != nil {
			return Materialization{}, err
		}
		files = append(files, taskFiles...)
	}

	secrets := make([]engine.SecretValue, 0, len(request.Container.Secrets))
	if len(request.Container.Secrets) > 0 {
		names := append([]id.SecretName(nil), request.Container.Secrets...)
		resolved, err := config.Secrets.Resolve(ctx, secret.ResolveRequest{Scope: config.Scope, Names: names})
		if err != nil {
			return Materialization{}, err
		}
		if len(resolved) != len(names) {
			return Materialization{}, fault.New(fault.InvalidInput, "secret resolver returned a different number of values than declared")
		}
		for index, value := range resolved {
			if value.Name != names[index] {
				return Materialization{}, fault.New(fault.InvalidInput, "secret resolver returned names out of declared order")
			}
			if err := value.Validate(); err != nil {
				return Materialization{}, err
			}
			secrets = append(secrets, engine.SecretValue{Name: value.Name, Value: value.Value})
		}
	}

	materialization := Materialization{
		WorkspaceID: request.WorkspaceID,
		Files:       files,
		Secrets:     secrets,
		Credential:  request.Credential,
		Task:        task,
	}
	if err := materialization.Validate(); err != nil {
		return Materialization{}, err
	}
	return materialization, nil
}

// Environment merges the already action-precedence-merged literal container
// environment with the reserved execution context in the order KODEMA_URL,
// KODEMA_EXECUTION, KODEMA_ACTION, then KODEMA_TASK only when the request has a
// task id. It never emits the credential or a secret value and refuses a
// literal environment name that shadows reserved context.
func Environment(request engine.PrepareRequest, url string) ([]string, error) {
	environment := make([]string, 0, len(request.Container.Env)+4)
	for _, variable := range request.Container.Env {
		name, err := id.ParseEnvName(string(variable.Name))
		if err != nil {
			return nil, fault.Wrap(fault.InvalidInput, "literal environment name is invalid", err)
		}
		environment = append(environment, string(name)+"="+variable.Value)
	}
	if url == "" {
		return nil, fault.New(fault.InvalidInput, "reserved environment url is empty")
	}
	executionID, err := id.ParseExecutionID(string(request.Identity.ExecutionID))
	if err != nil {
		return nil, fault.Wrap(fault.InvalidInput, "reserved environment execution id is invalid", err)
	}
	action, err := id.ParseActionName(string(request.ActionName))
	if err != nil {
		return nil, fault.Wrap(fault.InvalidInput, "reserved environment action name is invalid", err)
	}
	taskPresent := request.TaskID != ""
	if taskPresent {
		if _, err := id.ParseTaskID(string(request.TaskID)); err != nil {
			return nil, fault.Wrap(fault.InvalidInput, "reserved environment task id is invalid", err)
		}
	}
	if (request.Task != nil) != taskPresent {
		return nil, fault.New(fault.InvalidInput, "reserved environment task id and snapshot must be present together")
	}
	if !taskPresent && len(request.TaskFiles) > 0 {
		return nil, fault.New(fault.InvalidInput, "reserved environment task files require a task snapshot")
	}
	environment = append(environment,
		id.EnvURL+"="+url,
		id.EnvExecution+"="+string(executionID),
		id.EnvAction+"="+string(action),
	)
	if taskPresent {
		environment = append(environment, id.EnvTask+"="+string(request.TaskID))
	}
	return environment, nil
}

// validatePrepareRequest refuses an incomplete or unsafe preparation before any
// resolver, cipher or file effect. Every declared input, output, secret name and
// identity is validated here.
func validatePrepareRequest(request engine.PrepareRequest, config MaterializationConfig) error {
	if err := config.Scope.Validate(); err != nil {
		return err
	}
	if _, err := id.ParseExecutionID(string(request.Identity.ExecutionID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "prepare execution id is invalid", err)
	}
	if _, err := id.ParseWorkspaceID(string(request.WorkspaceID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "prepare workspace id is invalid", err)
	}
	if _, err := id.ParseActionName(string(request.ActionName)); err != nil {
		return fault.Wrap(fault.InvalidInput, "prepare action name is invalid", err)
	}
	if request.Credential == "" {
		return fault.New(fault.InvalidInput, "prepare has no execution credential")
	}
	taskPresent := request.TaskID != ""
	if taskPresent {
		if _, err := id.ParseTaskID(string(request.TaskID)); err != nil {
			return fault.Wrap(fault.InvalidInput, "prepare task id is invalid", err)
		}
	}
	if (request.Task != nil) != taskPresent {
		return fault.New(fault.InvalidInput, "prepare task id and snapshot must be present together")
	}
	if !taskPresent && len(request.TaskFiles) > 0 {
		return fault.New(fault.InvalidInput, "prepare task files require a task snapshot")
	}
	for name := range request.TaskFiles {
		if _, err := TaskFilePath(name); err != nil {
			return err
		}
	}

	for key := range request.Inputs {
		if _, err := InputPath(key); err != nil {
			return err
		}
	}
	for action, outputs := range request.Outputs {
		if _, err := id.ParseActionName(string(action)); err != nil {
			return fault.Wrap(fault.InvalidInput, "prepare output action name is invalid", err)
		}
		for key := range outputs {
			if _, err := OutputPath(action, key); err != nil {
				return err
			}
		}
	}

	seen := make(map[id.SecretName]bool, len(request.Container.Secrets))
	for _, name := range request.Container.Secrets {
		if _, err := id.ParseSecretName(string(name)); err != nil {
			return fault.Wrap(fault.InvalidInput, "prepare secret name is invalid", err)
		}
		if slices.Contains(id.ReservedEnvNames(), string(name)) {
			return fault.New(fault.InvalidInput, "prepare secret name shadows reserved context")
		}
		if seen[name] {
			return fault.New(fault.InvalidInput, "prepare declares a duplicate secret name")
		}
		seen[name] = true
	}
	if len(request.Container.Secrets) > 0 && config.Secrets == nil {
		return fault.New(fault.InvalidInput, "prepare declares secrets without a resolver")
	}
	return nil
}
