package kube

import (
	"context"
	"encoding/json"
	"net/http"
	"path"
	"strings"

	"github.com/optimiware/kodama/internal/fault"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
)

// Job request bodies. The shape is the pinned batch/v1 manifest the adapter
// declares; nothing is inferred from a server response.

type jobManifestBody struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Metadata   objectMetaBody `json:"metadata"`
	Spec       jobSpecBody    `json:"spec"`
}

type jobSpecBody struct {
	BackoffLimit            int             `json:"backoffLimit"`
	PodReplacementPolicy    string          `json:"podReplacementPolicy,omitempty"`
	TTLSecondsAfterFinished *int            `json:"ttlSecondsAfterFinished,omitempty"`
	Template                podTemplateBody `json:"template"`
}

type podTemplateBody struct {
	Metadata podTemplateMetaBody `json:"metadata"`
	Spec     podSpecBody         `json:"spec"`
}

type podTemplateMetaBody struct {
	Labels map[string]string `json:"labels,omitempty"`
}

type podSpecBody struct {
	RestartPolicy                string                     `json:"restartPolicy,omitempty"`
	AutomountServiceAccountToken bool                       `json:"automountServiceAccountToken"`
	HostNetwork                  bool                       `json:"hostNetwork"`
	ImagePullSecrets             []localObjectReferenceBody `json:"imagePullSecrets,omitempty"`
	Containers                   []containerBody            `json:"containers"`
	Volumes                      []volumeBody               `json:"volumes,omitempty"`
}

// localObjectReferenceBody is the pod-level name reference to a pull Secret.
type localObjectReferenceBody struct {
	Name string `json:"name"`
}

type containerBody struct {
	Name         string            `json:"name"`
	Image        string            `json:"image"`
	Command      []string          `json:"command,omitempty"`
	Env          []envVarBody      `json:"env,omitempty"`
	Resources    resourcesBody     `json:"resources"`
	VolumeMounts []volumeMountBody `json:"volumeMounts,omitempty"`
}

type envVarBody struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type resourcesBody struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

type volumeMountBody struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

type volumeBody struct {
	Name                  string               `json:"name"`
	PersistentVolumeClaim *claimVolumeBody     `json:"persistentVolumeClaim,omitempty"`
	ConfigMap             *configMapVolumeBody `json:"configMap,omitempty"`
	Secret                *secretVolumeBody    `json:"secret,omitempty"`
}

type claimVolumeBody struct {
	ClaimName string `json:"claimName"`
}

type configMapVolumeBody struct {
	Name  string          `json:"name"`
	Items []keyToPathBody `json:"items,omitempty"`
}

type secretVolumeBody struct {
	SecretName string          `json:"secretName"`
	Items      []keyToPathBody `json:"items,omitempty"`
}

type keyToPathBody struct {
	Key  string `json:"key"`
	Path string `json:"path"`
	Mode int64  `json:"mode,omitempty"`
}

// Job response bodies.

type jobWire struct {
	Metadata objectMeta    `json:"metadata"`
	Spec     jobSpecWire   `json:"spec"`
	Status   jobStatusWire `json:"status"`
}

type jobSpecWire struct {
	Suspend *bool `json:"suspend"`
	// TTLSecondsAfterFinished is presence-preserving: an absent or null field
	// decodes as nil (no finished-Job TTL), an explicit value as a pointer.
	// Stop clears it with the suspension PATCH, so a corrected observation can
	// prove the Job no longer carries a TTL.
	TTLSecondsAfterFinished *int `json:"ttlSecondsAfterFinished"`
}

type jobStatusWire struct {
	Active         int                `json:"active"`
	Succeeded      int                `json:"succeeded"`
	Failed         int                `json:"failed"`
	StartTime      *string            `json:"startTime"`
	CompletionTime *string            `json:"completionTime"`
	Terminating    *int               `json:"terminating"`
	Conditions     []jobConditionWire `json:"conditions"`
}

type jobConditionWire struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type jobListWire struct {
	Items []jobWire `json:"items"`
}

// buildJobManifest validates the seam Job and renders the exact REST body. Every
// path and item is checked before any request, so an unconfined or incomplete
// manifest never reaches the API.
func buildJobManifest(job runtimekube.Job) (jobManifestBody, error) {
	if strings.TrimSpace(job.Name) == "" {
		return jobManifestBody{}, fault.New(fault.InvalidInput, "kubernetes Job requires a name")
	}
	if strings.TrimSpace(job.Namespace) == "" {
		return jobManifestBody{}, fault.New(fault.InvalidInput, "kubernetes Job requires a namespace")
	}
	if strings.TrimSpace(job.Spec.Image) == "" {
		return jobManifestBody{}, fault.New(fault.InvalidInput, "kubernetes Job requires an image")
	}

	mounts := make([]volumeMountBody, 0, len(job.Spec.Volumes))
	volumes := make([]volumeBody, 0, len(job.Spec.Volumes))
	for _, volume := range job.Spec.Volumes {
		source, err := buildVolume(volume)
		if err != nil {
			return jobManifestBody{}, err
		}
		source.Name = volume.Name
		volumes = append(volumes, source)
		mounts = append(mounts, volumeMountBody{
			Name:      volume.Name,
			MountPath: volume.MountPath,
			ReadOnly:  volume.ReadOnly,
		})
	}

	env := make([]envVarBody, 0, len(job.Spec.Env))
	for _, entry := range job.Spec.Env {
		env = append(env, envVarBody{Name: entry.Name, Value: entry.Value})
	}

	container := containerBody{
		Name:         workloadContainerName,
		Image:        job.Spec.Image,
		Command:      append([]string(nil), job.Spec.Command...),
		Env:          env,
		Resources:    buildResources(job.Spec.Resources),
		VolumeMounts: mounts,
	}

	imagePullSecrets, err := buildImagePullSecrets(job.Spec.ImagePullSecrets)
	if err != nil {
		return jobManifestBody{}, err
	}

	labels := copyStringMap(job.Labels)
	podLabels := copyStringMap(job.Spec.PodLabels)
	manifest := jobManifestBody{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Metadata: objectMetaBody{
			Name:        job.Name,
			Namespace:   job.Namespace,
			Labels:      labels,
			Annotations: copyStringMap(job.Annotations),
		},
		Spec: jobSpecBody{
			BackoffLimit:            job.Spec.BackoffLimit,
			PodReplacementPolicy:    job.Spec.PodReplacementPolicy,
			TTLSecondsAfterFinished: job.Spec.TTLSecondsAfterFinished,
			Template: podTemplateBody{
				Metadata: podTemplateMetaBody{Labels: podLabels},
				Spec: podSpecBody{
					RestartPolicy:                job.Spec.RestartPolicy,
					AutomountServiceAccountToken: job.Spec.AutomountServiceAccountToken,
					HostNetwork:                  job.Spec.HostNetwork,
					ImagePullSecrets:             imagePullSecrets,
					Containers:                   []containerBody{container},
					Volumes:                      volumes,
				},
			},
		},
	}
	return manifest, nil
}

// buildImagePullSecrets renders the configured pull Secret names as the pod's
// imagePullSecrets list. An empty list renders no field. Every name is checked
// before any request so an unnamed, malformed or duplicated reference never
// reaches the API server.
func buildImagePullSecrets(names []string) ([]localObjectReferenceBody, error) {
	if len(names) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(names))
	references := make([]localObjectReferenceBody, 0, len(names))
	for _, name := range names {
		if !validImagePullSecretName(name) {
			return nil, fault.New(fault.InvalidInput, "kubernetes Job image pull secret name is not a DNS-1123 subdomain")
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, fault.New(fault.InvalidInput, "kubernetes Job image pull secret name is duplicated")
		}
		seen[name] = struct{}{}
		references = append(references, localObjectReferenceBody{Name: name})
	}
	return references, nil
}

// validImagePullSecretName reports whether name is a Kubernetes DNS-1123
// subdomain. The pull Secret is referenced by name only; no credential value is
// ever part of the manifest.
func validImagePullSecretName(name string) bool {
	const maxSubdomainLength = 253
	const maxLabelLength = 63
	if len(name) == 0 || len(name) > maxSubdomainLength {
		return false
	}
	for label := range strings.SplitSeq(name, ".") {
		if len(label) == 0 || len(label) > maxLabelLength {
			return false
		}
		for index := 0; index < len(label); index++ {
			character := label[index]
			alphanumeric := character >= 'a' && character <= 'z' || character >= '0' && character <= '9'
			if !alphanumeric && character != '-' {
				return false
			}
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
	}
	return true
}

// buildVolume maps one seam volume to exactly one source.
func buildVolume(volume runtimekube.Volume) (volumeBody, error) {
	switch {
	case strings.TrimSpace(volume.ClaimName) != "":
		return volumeBody{PersistentVolumeClaim: &claimVolumeBody{ClaimName: volume.ClaimName}}, nil
	case strings.TrimSpace(volume.ConfigMapName) != "":
		items, err := buildVolumeItems(volume, true)
		if err != nil {
			return volumeBody{}, err
		}
		return volumeBody{ConfigMap: &configMapVolumeBody{Name: volume.ConfigMapName, Items: items}}, nil
	case strings.TrimSpace(volume.SecretName) != "":
		items, err := buildVolumeItems(volume, false)
		if err != nil {
			return volumeBody{}, err
		}
		return volumeBody{Secret: &secretVolumeBody{SecretName: volume.SecretName, Items: items}}, nil
	default:
		return volumeBody{}, fault.New(fault.InvalidInput, "kubernetes Job volume has no source")
	}
}

// buildVolumeItems confines every item to its mount. ConfigMap keys are encoded
// because an absolute activation path is not a valid Kubernetes key; Secret keys
// are already valid names and stay unchanged.
func buildVolumeItems(volume runtimekube.Volume, encodeKeys bool) ([]keyToPathBody, error) {
	items := make([]keyToPathBody, 0, len(volume.Items))
	for _, item := range volume.Items {
		relative, err := relativeItemPath(volume.MountPath, item.Path)
		if err != nil {
			return nil, err
		}
		key := item.Key
		if encodeKeys {
			key = encodeConfigMapKey(item.Key)
		}
		items = append(items, keyToPathBody{
			Key:  key,
			Path: relative,
			Mode: int64(item.Mode.Perm()),
		})
	}
	return items, nil
}

// relativeItemPath returns the item path relative to its mount and refuses a
// path outside that mount.
func relativeItemPath(mountPath, itemPath string) (string, error) {
	if !path.IsAbs(mountPath) || !path.IsAbs(itemPath) {
		return "", fault.New(fault.InvalidInput, "kubernetes volume item paths must be absolute")
	}
	mount := path.Clean(mountPath)
	item := path.Clean(itemPath)
	prefix := mount
	if prefix == "/" {
		prefix = ""
	}
	if !strings.HasPrefix(item, prefix+"/") {
		return "", fault.New(fault.InvalidInput, "kubernetes volume item path is outside its mount path")
	}
	relative := strings.TrimPrefix(item, prefix+"/")
	if relative == "" || relative == "." || relative == ".." || strings.HasPrefix(relative, "../") {
		return "", fault.New(fault.InvalidInput, "kubernetes volume item path is outside its mount path")
	}
	return relative, nil
}

// buildResources renders only the limits and requests that were admitted.
func buildResources(resources runtimekube.Resources) resourcesBody {
	requests := map[string]string{}
	if resources.CPURequest != "" {
		requests["cpu"] = resources.CPURequest
	}
	if resources.MemoryRequest != "" {
		requests["memory"] = resources.MemoryRequest
	}
	limits := map[string]string{}
	if resources.CPULimit != "" {
		limits["cpu"] = resources.CPULimit
	}
	if resources.MemoryLimit != "" {
		limits["memory"] = resources.MemoryLimit
	}
	body := resourcesBody{}
	if len(requests) > 0 {
		body.Requests = requests
	}
	if len(limits) > 0 {
		body.Limits = limits
	}
	return body
}

// CreateJob creates exactly one stable Job. A create conflict is ErrAlreadyExists
// and is never retried or rewritten by the driver.
func (c *Cluster) CreateJob(ctx context.Context, job runtimekube.Job) (runtimekube.Job, error) {
	manifest, err := buildJobManifest(job)
	if err != nil {
		return runtimekube.Job{}, err
	}
	payload, err := json.Marshal(manifest)
	if err != nil {
		return runtimekube.Job{}, fault.New(fault.InternalFailure, "kubernetes Job manifest is not encodable")
	}
	data, status, err := c.exchange(ctx, http.MethodPost, jobsPath(job.Namespace), nil, payload, jsonContentType)
	if err != nil {
		return runtimekube.Job{}, err
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return runtimekube.Job{}, statusError("create Job", status, true)
	}
	return decodeJob(data, "create Job")
}

// GetJob returns one observed Job. A missing Job is ErrNotFound, never a
// nonexecution proof by itself.
func (c *Cluster) GetJob(ctx context.Context, namespace, name string) (runtimekube.Job, error) {
	data, status, err := c.exchange(ctx, http.MethodGet, jobPath(namespace, name), nil, nil, "")
	if err != nil {
		return runtimekube.Job{}, err
	}
	if status != http.StatusOK {
		return runtimekube.Job{}, statusError("get Job", status, false)
	}
	return decodeJob(data, "get Job")
}

// ListJobs returns every Job matching the selector.
func (c *Cluster) ListJobs(ctx context.Context, namespace string, labels map[string]string) ([]runtimekube.Job, error) {
	data, status, err := c.exchange(ctx, http.MethodGet, jobsPath(namespace), labelSelector(labels), nil, "")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, statusError("list Jobs", status, false)
	}
	var list jobListWire
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, undecodable("list Jobs")
	}
	jobs := make([]runtimekube.Job, 0, len(list.Items))
	for _, item := range list.Items {
		job, err := item.job("list Jobs")
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

// DeleteJob deletes one Job and, with an explicit background propagation
// policy, its dependent pods. A raw Job DELETE defaults to orphaning pods, so
// the policy is always sent; a missing Job is ErrNotFound.
func (c *Cluster) DeleteJob(ctx context.Context, namespace, name string) error {
	payload, err := json.Marshal(deleteOptionsBody{
		APIVersion:        "v1",
		Kind:              "DeleteOptions",
		PropagationPolicy: "Background",
	})
	if err != nil {
		return fault.New(fault.InternalFailure, "kubernetes delete options are not encodable")
	}
	_, status, err := c.exchange(ctx, http.MethodDelete, jobPath(namespace, name), nil, payload, jsonContentType)
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusAccepted {
		return statusError("delete Job", status, false)
	}
	return nil
}

// suspendJobPatchBody is the exact merge PATCH body for one suspension. It
// carries only the expected UID, the suspension intent and the explicit clear of
// any finished-Job TTL: no status, no resource version and no full spec, so the
// server can never be asked to delete, replace or rewrite the Job from this path.
type suspendJobPatchBody struct {
	Metadata suspendJobPatchMetadata `json:"metadata"`
	Spec     suspendJobPatchSpec     `json:"spec"`
}

type suspendJobPatchMetadata struct {
	UID string `json:"uid"`
}

type suspendJobPatchSpec struct {
	Suspend bool `json:"suspend"`
	// TTLSecondsAfterFinished is always serialized as an explicit JSON null: a
	// JSON merge patch with null removes the field, so a Job that finishes while
	// suspended is never removed by the TTL controller (which deletes with
	// foreground propagation, taking its pods) before Remove deletes it. The
	// field is not optional in this struct precisely so the null is always sent.
	TTLSecondsAfterFinished *int `json:"ttlSecondsAfterFinished"`
}

// SuspendJob sends exactly one UID-guarded merge PATCH that suspends the Job and
// clears its finished-Job TTL. An empty expected UID is refused before any
// request: without it a reused name could suspend an unrelated Job. The answer
// is decoded through the same Job decoder as every read, so the caller can
// decide whether the controller has acknowledged the intent. A lost response or
// a 5xx stays an uncertain effect and is never replayed or converted into a
// DELETE.
func (c *Cluster) SuspendJob(ctx context.Context, namespace, name, expectedUID string) (runtimekube.Job, error) {
	uid := strings.TrimSpace(expectedUID)
	if uid == "" {
		return runtimekube.Job{}, fault.New(fault.InvalidInput, "kubernetes suspend Job requires the expected UID")
	}
	payload, err := json.Marshal(suspendJobPatchBody{
		Metadata: suspendJobPatchMetadata{UID: uid},
		Spec:     suspendJobPatchSpec{Suspend: true},
	})
	if err != nil {
		return runtimekube.Job{}, fault.New(fault.InternalFailure, "kubernetes suspend Job patch is not encodable")
	}
	data, status, err := c.exchange(ctx, http.MethodPatch, jobPath(namespace, name), nil, payload, mergePatchContentType)
	if err != nil {
		return runtimekube.Job{}, err
	}
	if status != http.StatusOK {
		return runtimekube.Job{}, statusError("suspend Job", status, false)
	}
	return decodeJob(data, "suspend Job")
}

// decodeJob parses one Job response.
func decodeJob(data []byte, operation string) (runtimekube.Job, error) {
	var wire jobWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return runtimekube.Job{}, undecodable(operation)
	}
	return wire.job(operation)
}

// job maps one Job response onto the seam type. Every evidence field is decoded
// by this one function so GET, LIST and PATCH answers cannot diverge.
func (wire jobWire) job(operation string) (runtimekube.Job, error) {
	start, err := parseTimestamp(wire.Status.StartTime)
	if err != nil {
		return runtimekube.Job{}, undecodable(operation)
	}
	completion, err := parseTimestamp(wire.Status.CompletionTime)
	if err != nil {
		return runtimekube.Job{}, undecodable(operation)
	}
	deleted, err := wire.Metadata.parseDeletionTimestamp()
	if err != nil {
		return runtimekube.Job{}, undecodable(operation)
	}
	conditions := make([]runtimekube.JobCondition, 0, len(wire.Status.Conditions))
	for _, condition := range wire.Status.Conditions {
		conditions = append(conditions, runtimekube.JobCondition{
			Type:    condition.Type,
			Status:  condition.Status,
			Reason:  condition.Reason,
			Message: condition.Message,
		})
	}
	return runtimekube.Job{
		Name:              wire.Metadata.Name,
		Namespace:         wire.Metadata.Namespace,
		UID:               wire.Metadata.UID,
		Labels:            wire.Metadata.Labels,
		Annotations:       wire.Metadata.Annotations,
		DeletionTimestamp: deleted,
		ResourceVersion:   wire.Metadata.ResourceVersion,
		Spec: runtimekube.JobSpec{
			Suspend:                 wire.Spec.Suspend,
			TTLSecondsAfterFinished: wire.Spec.TTLSecondsAfterFinished,
		},
		Status: runtimekube.JobStatus{
			Active:         wire.Status.Active,
			Succeeded:      wire.Status.Succeeded,
			Failed:         wire.Status.Failed,
			StartTime:      start,
			CompletionTime: completion,
			Terminating:    wire.Status.Terminating,
			Conditions:     conditions,
		},
	}, nil
}
