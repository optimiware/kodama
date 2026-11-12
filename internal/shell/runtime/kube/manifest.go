// Manifest construction for the Kubernetes runtime adapter.
//
// Everything here is pure: it turns the persisted effect identity and the
// admitted container declaration into names, an activation snapshot and one Job
// manifest. No object is written and no cluster is contacted.
package kube

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"maps"
	"sort"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/kernel/script"
	"github.com/optimiware/kodama/internal/shell/workspace"
)

// activationRoot is the read-only mount of the immutable activation snapshot.
const activationRoot = "/kodama"

// readOnlyMode is the explicit read-only mode for every projected file.
const readOnlyMode = 0o444

// workloadCAVolumeName is the pinned name of the installation CA volume.
const workloadCAVolumeName = "workload-ca"

// activationConfigMapName returns the stable activation snapshot name.
func activationConfigMapName(workload id.WorkloadID) string {
	return "kodama-activation-" + stableDigest(string(workload))
}

// credentialSecretName returns the stable credential Secret name.
func credentialSecretName(workload id.WorkloadID) string {
	return "kodama-credential-" + stableDigest(string(workload))
}

// workloadJobName returns the stable Job name. A retry must find exactly this
// Job, never a generated replacement.
func workloadJobName(workload id.WorkloadID) string {
	return "kodama-job-" + stableDigest(string(workload))
}

// labels returns the pinned tenant/execution/action/workload/workspace labels
// shared by every owned object.
func (a *Adapter) labels(identity engine.EffectIdentity, workspaceID id.WorkspaceID) map[string]string {
	return map[string]string{
		LabelTenant:    string(a.tenant),
		LabelExecution: string(identity.ExecutionID),
		LabelAction:    string(identity.ActionID),
		LabelWorkload:  string(identity.WorkloadID),
		LabelWorkspace: string(workspaceID),
	}
}

// buildMaterialization validates and builds the complete all-or-nothing
// activation snapshot. Validation happens before any cluster call so a partial
// or unsafe snapshot never reaches the API.
func buildMaterialization(request engine.PrepareRequest) (workspace.Materialization, error) {
	if _, err := id.ParseWorkspaceID(string(request.WorkspaceID)); err != nil {
		return workspace.Materialization{}, fault.Wrap(fault.InvalidInput, "preparation workspace id is invalid", err)
	}
	files := make([]workspace.File, 0, len(request.Inputs)+len(request.Task))

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

// activationConfigMap builds the immutable activation snapshot.
func (a *Adapter) activationConfigMap(identity engine.EffectIdentity, workspaceID id.WorkspaceID, materialization workspace.Materialization, digest string) ConfigMap {
	data := make(map[string][]byte, len(materialization.Files))
	paths := make([]string, 0, len(materialization.Files))
	for _, file := range materialization.Files {
		data[file.Path] = append([]byte(nil), file.Data...)
		paths = append(paths, file.Path)
	}
	sort.Strings(paths)
	labels := a.labels(identity, workspaceID)
	labels[LabelPreparation] = digest
	return ConfigMap{
		Name:      activationConfigMapName(identity.WorkloadID),
		Namespace: a.namespace,
		Labels:    labels,
		Data:      data,
		Immutable: true,
	}
}

// preparationDigest computes a deterministic 48-hex-character SHA-256 prefix
// (a valid Kubernetes label value) over the full activation declaration,
// excluding only the mutable execution credential.
func (a *Adapter) preparationDigest(request engine.PrepareRequest, materialization workspace.Materialization) string {
	digest := sha256.New()
	writeDigestString(digest, string(request.Identity.ExecutionID))
	writeDigestString(digest, string(request.Identity.ActionID))
	writeDigestString(digest, string(request.Identity.WorkloadID))
	writeDigestString(digest, request.Identity.IdempotencyKey)
	writeDigestString(digest, string(request.WorkspaceID))
	writeDigestString(digest, string(request.ActionName))
	writeDigestString(digest, string(request.TaskID))
	writeDigestString(digest, request.Container.Image)
	writeDigestCount(digest, len(request.Container.Commands))
	for _, command := range request.Container.Commands {
		writeDigestString(digest, command)
	}
	writeDigestCount(digest, len(request.Container.Env))
	for _, entry := range request.Container.Env {
		writeDigestString(digest, string(entry.Name))
		writeDigestString(digest, entry.Value)
	}
	writeDigestString(digest, a.kodamaURL)
	writeDigestCount(digest, len(materialization.Files))
	for _, file := range materialization.Files {
		writeDigestString(digest, file.Path)
		writeDigestBytes(digest, file.Data)
	}
	secrets := append([]engine.SecretValue(nil), materialization.Secrets...)
	sort.Slice(secrets, func(left, right int) bool { return secrets[left].Name < secrets[right].Name })
	writeDigestCount(digest, len(secrets))
	for _, secret := range secrets {
		writeDigestString(digest, string(secret.Name))
		writeDigestString(digest, secret.Value)
	}
	if request.Task != nil {
		writeDigestString(digest, "task:present")
		writeDigestBytes(digest, request.Task)
	} else {
		writeDigestString(digest, "task:absent")
	}
	return hex.EncodeToString(digest.Sum(nil))[:48]
}

// writeDigestCount encodes one slice length unambiguously into the digest.
func writeDigestCount(digest hash.Hash, count int) {
	fmt.Fprintf(digest, "%d#", count)
}

// writeDigestString length-prefixes one string value so concatenation is
// unambiguous.
func writeDigestString(digest hash.Hash, value string) {
	fmt.Fprintf(digest, "%d:", len(value))
	digest.Write([]byte(value))
}

// writeDigestBytes length-prefixes one byte slice so concatenation is
// unambiguous.
func writeDigestBytes(digest hash.Hash, value []byte) {
	fmt.Fprintf(digest, "%d:", len(value))
	digest.Write(value)
}

// credentialSecret builds the mutable credential and declared-secret mount.
func (a *Adapter) credentialSecret(identity engine.EffectIdentity, workspaceID id.WorkspaceID, materialization workspace.Materialization) Secret {
	data := make(map[string][]byte, len(materialization.Secrets)+1)
	for _, secret := range materialization.Secrets {
		data[string(secret.Name)] = []byte(secret.Value)
	}
	data[id.SecretTokenName] = []byte(materialization.Credential)
	return Secret{
		Name:      credentialSecretName(identity.WorkloadID),
		Namespace: a.namespace,
		Labels:    a.labels(identity, workspaceID),
		Data:      data,
	}
}

// jobManifest builds the pinned, fail-fast, non-replaying Job manifest.
func (a *Adapter) jobManifest(prepared preparedWorkload, identity engine.EffectIdentity) Job {
	labels := a.labels(identity, prepared.workspace)
	podLabels := make(map[string]string, len(labels))
	maps.Copy(podLabels, labels)

	env := make([]EnvVar, 0, len(prepared.container.env)+4)
	for _, entry := range prepared.container.env {
		env = append(env, EnvVar{Name: string(entry.Name), Value: entry.Value})
	}
	env = append(env,
		EnvVar{Name: id.EnvURL, Value: a.kodamaURL},
		EnvVar{Name: id.EnvExecution, Value: string(identity.ExecutionID)},
		EnvVar{Name: id.EnvAction, Value: string(prepared.actionName)},
	)
	if prepared.taskID != "" {
		env = append(env, EnvVar{Name: id.EnvTask, Value: string(prepared.taskID)})
	}

	activationItems := make([]VolumeItem, 0, len(prepared.activationPaths))
	for _, path := range prepared.activationPaths {
		activationItems = append(activationItems, VolumeItem{Key: path, Path: path, Mode: readOnlyMode})
	}

	credentialItems := make([]VolumeItem, 0, len(prepared.secretNames)+1)
	for _, name := range prepared.secretNames {
		path, err := workspace.SecretPath(name)
		if err != nil {
			continue
		}
		credentialItems = append(credentialItems, VolumeItem{Key: string(name), Path: path, Mode: readOnlyMode})
	}
	credentialItems = append(credentialItems, VolumeItem{
		Key:  id.SecretTokenName,
		Path: workspace.ReservedCredentialPath,
		Mode: readOnlyMode,
	})

	ttl := a.policy.JobTTLSeconds
	volumes := []Volume{
		{
			Name:      "workspace",
			MountPath: workspace.WorkspaceRoot,
			ClaimName: prepared.claimName,
			ReadOnly:  false,
		},
		{
			Name:          "activation",
			MountPath:     activationRoot,
			ConfigMapName: prepared.configMap,
			ReadOnly:      true,
			Items:         activationItems,
		},
		{
			Name:       "credential",
			MountPath:  workspace.SecretRoot,
			SecretName: prepared.secret,
			ReadOnly:   true,
			Items:      credentialItems,
		},
	}
	// The installation CA is an optional fourth, read-only Secret volume. It
	// projects exactly the literal ca.crt key at the fixed trust path, outside
	// the workspace, activation and credential mounts, and adds no environment
	// variable: workloads opt in with an explicit --cacert.
	if a.workloadCA.SecretName != "" {
		volumes = append(volumes, Volume{
			Name:       workloadCAVolumeName,
			MountPath:  WorkloadCAMountPath,
			SecretName: a.workloadCA.SecretName,
			ReadOnly:   true,
			Items: []VolumeItem{{
				Key:  WorkloadCAKey,
				Path: WorkloadCAPath,
				Mode: readOnlyMode,
			}},
		})
	}
	return Job{
		Name:      prepared.job,
		Namespace: a.namespace,
		Labels:    labels,
		Annotations: map[string]string{
			AnnotationIdempotencyKey: identity.IdempotencyKey,
		},
		Spec: JobSpec{
			Image:   prepared.container.image,
			Command: []string{script.ShellPath, "-ec", prepared.container.script},
			Env:     env,
			Resources: Resources{
				CPURequest:    a.policy.CPURequest,
				CPULimit:      a.policy.CPULimit,
				MemoryRequest: a.policy.MemoryRequest,
				MemoryLimit:   a.policy.MemoryLimit,
			},
			Volumes:                      volumes,
			RestartPolicy:                "Never",
			BackoffLimit:                 0,
			PodReplacementPolicy:         "Failed",
			TTLSecondsAfterFinished:      &ttl,
			AutomountServiceAccountToken: false,
			HostNetwork:                  false,
			PodLabels:                    podLabels,
			ImagePullSecrets:             append([]string(nil), a.imagePullSecrets...),
		},
	}
}

// describeIdentity renders a stable, non-secret diagnostic identity.
func describeIdentity(identity engine.EffectIdentity) string {
	return fmt.Sprintf("workload %q (execution %q action %q)", identity.WorkloadID, identity.ExecutionID, identity.ActionID)
}
