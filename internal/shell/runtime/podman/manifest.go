// Manifest construction and host-layout rules for the Podman runtime adapter.
//
// Everything here is pure: it turns the admitted activation declaration into
// confined host paths, an activation snapshot, one preparation record and one
// pinned container spec. No file is written and no libpod call is made.
package podman

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/kernel/script"
	"github.com/optimiware/kodama/internal/shell/workspace"
)

// Host layout constants. Every materialized logical path P under /kodama is
// written at filepath.Join(<root>/<workload>, P); the activation directory is
// bind-mounted read-only at /kodama and the retained workspace volume at
// /workspace.
const (
	// activationRoot is the read-only bind destination of the activation data.
	activationRoot = "/kodama"
	// entrypointScriptPath is the rendered fail-fast script inside the bind.
	entrypointScriptPath = "/kodama/script"
	// readOnlyMode is the explicit read-only mode of every prepared file.
	readOnlyMode = os.FileMode(0o444)
	// directoryMode is the explicit mode of every prepared directory.
	directoryMode = os.FileMode(0o755)
	// preparationRecordName is the durable preparation marker. It lives beside
	// (never inside) the read-only bind, so it is not visible to the workload
	// and is removed with the identity's materialization.
	preparationRecordName = "prepared.json"
	// startAttemptRecordName is the durable start-attempt marker. It is written
	// before any StartContainer request and lives beside (never inside) the
	// read-only bind, so a start whose outcome was lost is never reported as
	// never-started and is never replayed. It is removed with the identity's
	// materialization.
	startAttemptRecordName = "start-attempted.json"
)

// preparedFile is one complete activation file with its confined logical path.
type preparedFile struct {
	logical string
	data    []byte
}

// preparedEnvVar is one admitted literal environment entry persisted with the
// preparation so a reconstructed adapter builds the identical container spec.
type preparedEnvVar struct {
	Name  id.EnvName `json:"name"`
	Value string     `json:"value"`
}

// preparationRecord is the durable evidence that one workload was completely
// prepared. It is written last, so its presence authorizes a start and its
// absence (a partial or unsafe preparation) never does.
type preparationRecord struct {
	WorkspaceID id.WorkspaceID   `json:"workspace_id"`
	ActionName  id.ActionName    `json:"action_name"`
	TaskID      id.TaskID        `json:"task_id,omitempty"`
	Image       string           `json:"image"`
	Commands    []string         `json:"commands"`
	Env         []preparedEnvVar `json:"env,omitempty"`
	Secrets     []id.SecretName  `json:"secrets,omitempty"`
	Files       []string         `json:"files"`
}

// workloadRoot returns the per-identity adapter root.
func workloadRoot(root string, workload id.WorkloadID) string {
	return filepath.Join(root, string(workload))
}

// activationDir returns the daemon-visible directory bind-mounted at /kodama.
func activationDir(root string, workload id.WorkloadID) string {
	return filepath.Join(workloadRoot(root, workload), activationRoot)
}

// hostPath maps one confined logical path to its host path.
func hostPath(root string, workload id.WorkloadID, logical string) string {
	return filepath.Join(workloadRoot(root, workload), logical)
}

// preparationRecordPath returns the durable preparation marker path.
func preparationRecordPath(root string, workload id.WorkloadID) string {
	return filepath.Join(workloadRoot(root, workload), preparationRecordName)
}

// startAttemptPath returns the durable start-attempt marker path.
func startAttemptPath(root string, workload id.WorkloadID) string {
	return filepath.Join(workloadRoot(root, workload), startAttemptRecordName)
}

// validatePinnedImage refuses an image that is not pinned to an immutable
// digest. A tag or a bare name could resolve to different bytes on a retry.
func validatePinnedImage(image string) error {
	if strings.TrimSpace(image) == "" {
		return fault.New(fault.InvalidInput, "container image is empty")
	}
	const marker = "@sha256:"
	index := strings.LastIndex(image, marker)
	if index <= 0 {
		return fault.New(fault.InvalidInput, "container image is not pinned to a digest")
	}
	digest := image[index+len(marker):]
	if len(digest) != 64 {
		return fault.New(fault.InvalidInput, "container image digest is not a sha256 digest")
	}
	for _, character := range digest {
		switch {
		case character >= '0' && character <= '9':
		case character >= 'a' && character <= 'f':
		case character >= 'A' && character <= 'F':
		default:
			return fault.New(fault.InvalidInput, "container image digest is not a sha256 digest")
		}
	}
	return nil
}

// buildPreparation validates the admitted declaration and returns every
// activation file plus the resolved declared secrets. Validation is complete
// before any effect so an unsafe declaration never reaches the filesystem.
func buildPreparation(request engine.PrepareRequest) ([]preparedFile, []engine.SecretValue, error) {
	return buildPreparationWithCA(request, nil)
}

func buildPreparationWithCA(request engine.PrepareRequest, workloadCAPEM []byte) ([]preparedFile, []engine.SecretValue, error) {
	if err := validatePinnedImage(request.Container.Image); err != nil {
		return nil, nil, err
	}
	for index, entry := range request.Container.Env {
		if _, err := id.ParseEnvName(string(entry.Name)); err != nil {
			return nil, nil, fault.Wrap(fault.InvalidInput,
				fmt.Sprintf("container environment entry %d is invalid", index), err)
		}
	}
	renderedScript, err := script.Render(request.Container.Commands)
	if err != nil {
		return nil, nil, err
	}

	files := make([]preparedFile, 0, len(request.Inputs)+len(request.Outputs)+len(request.Secrets)+2)
	workspaceFiles := make([]workspace.File, 0, len(request.Inputs)+len(request.Outputs)+1)

	inputKeys := make([]string, 0, len(request.Inputs))
	for key := range request.Inputs {
		inputKeys = append(inputKeys, string(key))
	}
	sort.Strings(inputKeys)
	for _, raw := range inputKeys {
		path, err := workspace.InputPath(id.InputKey(raw))
		if err != nil {
			return nil, nil, err
		}
		data := []byte(request.Inputs[id.InputKey(raw)])
		workspaceFiles = append(workspaceFiles, workspace.File{Path: path, Data: data, Mode: readOnlyMode})
		files = append(files, preparedFile{logical: path, data: data})
	}

	actionNames := make([]string, 0, len(request.Outputs))
	for action := range request.Outputs {
		actionNames = append(actionNames, string(action))
	}
	sort.Strings(actionNames)
	for _, rawAction := range actionNames {
		action := id.ActionName(rawAction)
		outputs := request.Outputs[action]
		outputKeys := make([]string, 0, len(outputs))
		for key := range outputs {
			outputKeys = append(outputKeys, string(key))
		}
		sort.Strings(outputKeys)
		for _, rawKey := range outputKeys {
			path, err := workspace.OutputPath(action, id.OutputKey(rawKey))
			if err != nil {
				return nil, nil, err
			}
			data := []byte(outputs[id.OutputKey(rawKey)])
			workspaceFiles = append(workspaceFiles, workspace.File{Path: path, Data: data, Mode: readOnlyMode})
			files = append(files, preparedFile{logical: path, data: data})
		}
	}

	if request.Task != nil {
		data := append([]byte(nil), request.Task...)
		workspaceFiles = append(workspaceFiles, workspace.File{Path: workspace.TaskPath, Data: data, Mode: readOnlyMode})
		files = append(files, preparedFile{logical: workspace.TaskPath, data: data})
	}

	secrets := make([]engine.SecretValue, 0, len(request.Secrets))
	for _, secret := range request.Secrets {
		path, err := workspace.SecretPath(secret.Name)
		if err != nil {
			return nil, nil, err
		}
		secrets = append(secrets, engine.SecretValue{Name: secret.Name, Value: secret.Value})
		files = append(files, preparedFile{logical: path, data: []byte(secret.Value)})
	}

	materialization := workspace.Materialization{
		WorkspaceID: request.WorkspaceID,
		Files:       workspaceFiles,
		Secrets:     secrets,
		Credential:  request.Credential,
		Task:        request.Task,
	}
	if err := materialization.Validate(); err != nil {
		return nil, nil, err
	}

	files = append(files, preparedFile{logical: entrypointScriptPath, data: []byte(renderedScript)})
	if len(workloadCAPEM) > 0 {
		files = append(files, preparedFile{logical: WorkloadCAPath, data: append([]byte(nil), workloadCAPEM...)})
	}
	sort.Slice(files, func(left, right int) bool { return files[left].logical < files[right].logical })
	return files, secrets, nil
}

// preparationRecordFor builds the durable preparation marker for one request.
func preparationRecordFor(request engine.PrepareRequest, files []preparedFile, secrets []engine.SecretValue) preparationRecord {
	env := make([]preparedEnvVar, 0, len(request.Container.Env))
	for _, entry := range request.Container.Env {
		env = append(env, preparedEnvVar{Name: entry.Name, Value: entry.Value})
	}
	names := make([]id.SecretName, 0, len(secrets))
	for _, secret := range secrets {
		names = append(names, secret.Name)
	}
	slices.Sort(names)
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.logical)
	}
	return preparationRecord{
		WorkspaceID: request.WorkspaceID,
		ActionName:  request.ActionName,
		TaskID:      request.TaskID,
		Image:       request.Container.Image,
		Commands:    append([]string(nil), request.Container.Commands...),
		Env:         env,
		Secrets:     names,
		Files:       paths,
	}
}

// labels returns the pinned identity labels shared by the container and its
// stable name.
func (a *Adapter) labels(identity engine.EffectIdentity, workspaceID id.WorkspaceID) map[string]string {
	return map[string]string{
		LabelExecution:      string(identity.ExecutionID),
		LabelAction:         string(identity.ActionID),
		LabelWorkload:       string(identity.WorkloadID),
		LabelWorkspace:      string(workspaceID),
		LabelIdempotencyKey: identity.IdempotencyKey,
	}
}

// containerSpec builds the pinned, fail-fast, non-replaying container request.
// The admitted literal environment keeps its declaration order and the
// unshadowable reserved context is appended last.
func (a *Adapter) containerSpec(prepared preparationRecord, identity engine.EffectIdentity) (ContainerSpec, error) {
	name, err := ContainerName(identity.WorkloadID)
	if err != nil {
		return ContainerSpec{}, err
	}
	volumeName, err := WorkspaceVolumeName(prepared.WorkspaceID)
	if err != nil {
		return ContainerSpec{}, err
	}
	// Prepare records the CA file it wrote; the adapter's current
	// configuration may have changed since.
	hasCA := slices.Contains(prepared.Files, WorkloadCAPath)
	env := make([]string, 0, len(prepared.Env)+5)
	for _, entry := range prepared.Env {
		if hasCA && string(entry.Name) == EnvWorkloadCAFile {
			continue
		}
		env = append(env, string(entry.Name)+"="+entry.Value)
	}
	env = append(env,
		id.EnvURL+"="+a.kodamaURL,
		id.EnvExecution+"="+string(identity.ExecutionID),
		id.EnvAction+"="+string(prepared.ActionName),
	)
	if prepared.TaskID != "" {
		env = append(env, id.EnvTask+"="+string(prepared.TaskID))
	}
	if hasCA {
		env = append(env, EnvWorkloadCAFile+"="+WorkloadCAPath)
	}
	return ContainerSpec{
		Name:       name,
		Image:      prepared.Image,
		Entrypoint: []string{script.ShellPath, entrypointScriptPath},
		Env:        env,
		Labels:     a.labels(identity, prepared.WorkspaceID),
		Mounts: []Mount{
			{
				Type:        "bind",
				Source:      activationDir(a.root, identity.WorkloadID),
				Destination: activationRoot,
				ReadOnly:    true,
			},
			{
				Type:        "volume",
				Source:      volumeName,
				Destination: workspace.WorkspaceRoot,
				ReadOnly:    false,
			},
		},
		RestartPolicy: "no",
	}, nil
}

// encodePreparationRecord marshals the marker deterministically so a repeated
// Prepare can compare exact bytes instead of guessing at changed content.
func encodePreparationRecord(record preparationRecord) ([]byte, error) {
	data, err := json.Marshal(record)
	if err != nil {
		return nil, fault.Wrap(fault.InternalFailure, "preparation record is not encodable", err)
	}
	return data, nil
}

// describeIdentity renders a stable, non-secret diagnostic identity.
func describeIdentity(identity engine.EffectIdentity) string {
	return fmt.Sprintf("workload %q (execution %q action %q)",
		identity.WorkloadID, identity.ExecutionID, identity.ActionID)
}
