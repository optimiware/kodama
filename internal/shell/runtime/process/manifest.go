// Manifest and activation-snapshot construction for the Process runtime
// adapter.
//
// Everything here is pure or goes through the injected Files boundary: it turns
// the persisted effect identity and the admitted container declaration into a
// confined activation snapshot, the generated fail-fast script, the merged
// reserved environment and the small amount of bookkeeping a restart needs.
package process

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/kernel/script"
	"github.com/optimiware/kodama/internal/shell/workspace"
)

const (
	// readOnlyMode is the explicit read-only mode for every prepared file.
	readOnlyMode os.FileMode = 0o444
	// bookkeepingMode is the mode for adapter-owned mutable bookkeeping.
	bookkeepingMode os.FileMode = 0o644
	// directoryMode is the explicit mode for adapter-owned directories.
	directoryMode os.FileMode = 0o755

	// manifestName is the completed-preparation marker and activation summary.
	manifestName = "manifest.json"
	// scriptName is the generated fail-fast entrypoint.
	scriptName = "script"
	// groupName is the persisted spawn intent/bookkeeping file.
	groupName = "group"
	// logsDirectoryName holds the process group's two log streams.
	logsDirectoryName = "logs"
	// stdoutName and stderrName are the two stream files.
	stdoutName = "stdout"
	stderrName = "stderr"
	// tombstoneDirectoryName holds durable removal markers outside the removed
	// identity resource root.
	tombstoneDirectoryName = "tombstones"
)

// groupStateKind is the persisted spawn state of one stable identity.
type groupStateKind string

const (
	// groupSpawning means the spawn intent was persisted before the effect.
	groupSpawning groupStateKind = "spawning"
	// groupStarted means the executor accepted the spawn.
	groupStarted groupStateKind = "started"
	// groupNeverStarted means the executor affirmatively created no process.
	groupNeverStarted groupStateKind = "never_started"
	// groupUnknown means the spawn outcome cannot be classified.
	groupUnknown groupStateKind = "unknown"
)

// manifest is the completed-preparation summary a fresh adapter reloads. It
// deliberately excludes the mutable execution credential.
type manifest struct {
	Version     int                   `json:"version"`
	Identity    engine.EffectIdentity `json:"identity"`
	WorkspaceID id.WorkspaceID        `json:"workspace_id"`
	ActionName  id.ActionName         `json:"action_name"`
	TaskID      id.TaskID             `json:"task_id,omitempty"`
	Image       string                `json:"image,omitempty"`
	Commands    []string              `json:"commands"`
	Env         []string              `json:"env"`
	SecretNames []id.SecretName       `json:"secret_names,omitempty"`
	Digest      string                `json:"digest"`
}

// groupState is the persisted spawn intent and outcome of one stable identity.
type groupState struct {
	Group    GroupID               `json:"group"`
	Identity engine.EffectIdentity `json:"identity"`
	State    groupStateKind        `json:"state"`
	Detail   string                `json:"detail,omitempty"`
}

// manifestPath returns the completed-preparation marker path.
func manifestPath(resourceRoot string) string {
	return filepath.Join(resourceRoot, manifestName)
}

// scriptPath returns the generated fail-fast script path.
func scriptPath(resourceRoot string) string {
	return filepath.Join(resourceRoot, scriptName)
}

// groupPath returns the persisted spawn bookkeeping path.
func groupPath(resourceRoot string) string {
	return filepath.Join(resourceRoot, groupName)
}

// stdoutPath and stderrPath return the two stream paths.
func stdoutPath(resourceRoot string) string {
	return filepath.Join(resourceRoot, logsDirectoryName, stdoutName)
}

func stderrPath(resourceRoot string) string {
	return filepath.Join(resourceRoot, logsDirectoryName, stderrName)
}

// hostPath maps one confined logical /kodama path onto the host resource root.
func hostPath(resourceRoot, logical string) string {
	return filepath.Join(resourceRoot, logical)
}

// tombstonePath returns the durable removal marker for one workload identity.
func (a *Adapter) tombstonePath(workload id.WorkloadID) string {
	return filepath.Join(a.root, tombstoneDirectoryName, string(workload))
}

// readManifest loads the completed-preparation summary. A missing manifest is
// reported as found=false; any other error is returned.
func (a *Adapter) readManifest(ctx context.Context, resourceRoot string) (manifest, bool, error) {
	data, err := a.files.ReadFile(ctx, manifestPath(resourceRoot))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return manifest{}, false, nil
		}
		return manifest{}, false, fault.Wrap(fault.DependencyUnavailable, "activation snapshot is unreadable", err)
	}
	var loaded manifest
	if err := json.Unmarshal(data, &loaded); err != nil {
		return manifest{}, false, fault.Wrap(fault.InternalFailure, "activation snapshot is corrupt", err)
	}
	return loaded, true, nil
}

// writeManifest persists the completed-preparation marker last, so a partial
// snapshot never authorizes a start.
func (a *Adapter) writeManifest(ctx context.Context, resourceRoot string, loaded manifest) error {
	data, err := json.Marshal(loaded)
	if err != nil {
		return fault.Wrap(fault.InternalFailure, "activation snapshot cannot be encoded", err)
	}
	if err := a.files.WriteFile(ctx, manifestPath(resourceRoot), data, readOnlyMode); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "activation snapshot write failed", err)
	}
	return nil
}

// readGroupState loads the persisted spawn state. A missing file means the
// identity is only prepared.
func (a *Adapter) readGroupState(ctx context.Context, resourceRoot string) (groupState, bool, error) {
	data, err := a.files.ReadFile(ctx, groupPath(resourceRoot))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return groupState{}, false, nil
		}
		return groupState{}, false, fault.Wrap(fault.DependencyUnavailable, "process-group bookkeeping is unreadable", err)
	}
	var loaded groupState
	if err := json.Unmarshal(data, &loaded); err != nil {
		return groupState{}, false, fault.Wrap(fault.InternalFailure, "process-group bookkeeping is corrupt", err)
	}
	return loaded, true, nil
}

// writeGroupState persists the spawn intent or outcome.
func (a *Adapter) writeGroupState(ctx context.Context, resourceRoot string, state groupState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fault.Wrap(fault.InternalFailure, "process-group bookkeeping cannot be encoded", err)
	}
	if err := a.files.WriteFile(ctx, groupPath(resourceRoot), data, bookkeepingMode); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "process-group bookkeeping write failed", err)
	}
	return nil
}

// tombstoned reports whether a completed removal durably forbids reuse.
func (a *Adapter) tombstoned(ctx context.Context, workload id.WorkloadID) (bool, error) {
	_, err := a.files.Stat(ctx, a.tombstonePath(workload))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fault.Wrap(fault.DependencyUnavailable, "removal tombstone is unreachable", err)
}

// writeTombstone persists the removal marker outside the removed resource root
// so a removed identity is never prepared or started again.
func (a *Adapter) writeTombstone(ctx context.Context, workload id.WorkloadID) error {
	path := a.tombstonePath(workload)
	if err := a.ensureConfinedDir(ctx, filepath.Dir(path)); err != nil {
		return err
	}
	if err := a.files.WriteFile(ctx, path, []byte("removed\n"), readOnlyMode); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "removal tombstone write failed", err)
	}
	return nil
}

// ensureConfinedDir creates a directory below the adapter root without ever
// following a symlink or escaping the root.
func (a *Adapter) ensureConfinedDir(ctx context.Context, path string) error {
	if !pathWithin(a.root, path) {
		return fault.New(fault.Forbidden, "resource path is outside the adapter root")
	}
	if err := a.ensureOneDir(ctx, a.root); err != nil {
		return err
	}
	relative, err := filepath.Rel(a.root, path)
	if err != nil {
		return fault.Wrap(fault.Forbidden, "resource path is not confined", err)
	}
	if relative == "." {
		return nil
	}
	current := a.root
	for segment := range strings.SplitSeq(relative, string(os.PathSeparator)) {
		if segment == "" || segment == "." || segment == ".." {
			return fault.New(fault.Forbidden, "resource path is not confined")
		}
		current = filepath.Join(current, segment)
		if err := a.ensureOneDir(ctx, current); err != nil {
			return err
		}
	}
	return nil
}

// ensureOneDir refuses an existing symlink or non-directory and creates a
// missing directory.
func (a *Adapter) ensureOneDir(ctx context.Context, path string) error {
	info, err := a.files.Stat(ctx, path)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fault.New(fault.Forbidden, "resource path is not a confined directory")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fault.Wrap(fault.DependencyUnavailable, "resource directory is unreachable", err)
	}
	if err := a.files.MkdirAll(ctx, path, directoryMode); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "resource directory creation failed", err)
	}
	return nil
}

// requireWorkspace attaches an existing execution workspace and never recreates
// a missing one.
func (a *Adapter) requireWorkspace(ctx context.Context, workspaceDir string) error {
	info, err := a.files.Stat(ctx, workspaceDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fault.New(fault.NotFound, "execution workspace is missing; it is never recreated")
		}
		return fault.Wrap(fault.DependencyUnavailable, "execution workspace is unreachable", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fault.New(fault.NotFound, "execution workspace is not an existing directory")
	}
	return nil
}

// buildMaterialization validates and builds the complete all-or-nothing
// activation snapshot. Validation happens before any host call.
func buildMaterialization(request engine.PrepareRequest) (workspace.Materialization, error) {
	if _, err := id.ParseWorkspaceID(string(request.WorkspaceID)); err != nil {
		return workspace.Materialization{}, fault.Wrap(fault.InvalidInput, "preparation workspace id is invalid", err)
	}
	files := make([]workspace.File, 0, len(request.Inputs)+len(request.Outputs)+1)

	inputKeys := make([]string, 0, len(request.Inputs))
	for key := range request.Inputs {
		inputKeys = append(inputKeys, string(key))
	}
	sort.Strings(inputKeys)
	for _, raw := range inputKeys {
		path, err := workspace.InputPath(id.InputKey(raw))
		if err != nil {
			return workspace.Materialization{}, err
		}
		files = append(files, workspace.File{Path: path, Data: []byte(request.Inputs[id.InputKey(raw)]), Mode: readOnlyMode})
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
				return workspace.Materialization{}, err
			}
			files = append(files, workspace.File{Path: path, Data: []byte(outputs[id.OutputKey(rawKey)]), Mode: readOnlyMode})
		}
	}

	if request.Task != nil {
		files = append(files, workspace.File{Path: workspace.TaskPath, Data: append([]byte(nil), request.Task...), Mode: readOnlyMode})
	}

	secrets := make([]engine.SecretValue, len(request.Secrets))
	for index, secret := range request.Secrets {
		if _, err := workspace.SecretPath(secret.Name); err != nil {
			return workspace.Materialization{}, err
		}
		secrets[index] = engine.SecretValue{Name: secret.Name, Value: secret.Value}
	}

	materialization := workspace.Materialization{
		WorkspaceID: request.WorkspaceID,
		Files:       files,
		Secrets:     secrets,
		Credential:  request.Credential,
		Task:        request.Task,
	}
	if err := materialization.Validate(); err != nil {
		return workspace.Materialization{}, err
	}
	return materialization, nil
}

// finalEnvironment merges the admitted literal environment in declaration
// order and appends the unshadowable reserved context.
func finalEnvironment(request engine.PrepareRequest, kodamaURL string) ([]string, error) {
	env := make([]string, 0, len(request.Container.Env)+4)
	for _, entry := range request.Container.Env {
		if _, err := id.ParseEnvName(string(entry.Name)); err != nil {
			return nil, fault.Wrap(fault.InvalidInput, "container environment name is invalid", err)
		}
		env = append(env, string(entry.Name)+"="+entry.Value)
	}
	env = append(env,
		id.EnvURL+"="+kodamaURL,
		id.EnvExecution+"="+string(request.Identity.ExecutionID),
		id.EnvAction+"="+string(request.ActionName),
	)
	if request.TaskID != "" {
		env = append(env, id.EnvTask+"="+string(request.TaskID))
	}
	return env, nil
}

// snapshotDigest computes the deterministic activation content digest used for
// idempotent re-preparation and changed-content refusal. The mutable credential
// is deliberately excluded.
func snapshotDigest(request engine.PrepareRequest, env []string, materialization workspace.Materialization) string {
	digest := sha256.New()
	writeDigestString(digest, string(request.Identity.ExecutionID))
	writeDigestString(digest, string(request.Identity.ActionID))
	writeDigestString(digest, string(request.Identity.WorkloadID))
	writeDigestString(digest, request.Identity.IdempotencyKey)
	writeDigestString(digest, string(request.WorkspaceID))
	writeDigestString(digest, string(request.ActionName))
	writeDigestString(digest, string(request.TaskID))
	writeDigestString(digest, request.Container.Image)
	for _, command := range request.Container.Commands {
		writeDigestString(digest, command)
	}
	for _, entry := range env {
		writeDigestString(digest, entry)
	}
	for _, file := range materialization.Files {
		writeDigestString(digest, file.Path)
		writeDigestString(digest, string(file.Data))
	}
	secrets := append([]engine.SecretValue(nil), materialization.Secrets...)
	sort.Slice(secrets, func(left, right int) bool { return secrets[left].Name < secrets[right].Name })
	for _, secret := range secrets {
		writeDigestString(digest, string(secret.Name))
		writeDigestString(digest, secret.Value)
	}
	if request.Task != nil {
		writeDigestString(digest, "task")
		writeDigestString(digest, string(request.Task))
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// writeDigestString length-prefixes one value so concatenation is unambiguous.
func writeDigestString(digest hash.Hash, value string) {
	fmt.Fprintf(digest, "%d:", len(value))
	digest.Write([]byte(value))
}

// describeIdentity renders a stable, non-secret diagnostic identity.
func describeIdentity(identity engine.EffectIdentity) string {
	return fmt.Sprintf("workload %q (execution %q action %q)", identity.WorkloadID, identity.ExecutionID, identity.ActionID)
}

// prepareScript renders the fail-fast entrypoint or refuses an unusable
// command list before any effect.
func prepareScript(commands []string) (string, error) {
	rendered, err := script.Render(commands)
	if err != nil {
		return "", err
	}
	return rendered, nil
}
