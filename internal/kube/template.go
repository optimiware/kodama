package kube

import "github.com/optimiware/kodama/internal/kube/kubernetes"

// Pod layout (docs/implementation/step-entrypoint.md). The results file lives in
// its own volume because the root of /kodama is read-only image content.
const (
	pathTools     = "/kodama/bin"
	pathWorkspace = "/workspace"
	pathOutputs   = "/kodama/outputs"
	pathResults   = "/kodama/results"
	pathSecrets   = "/kodama/secrets"
	pathIdentity  = "/kodama/identity"
	pathBootstrap = "/kodama/bootstrap"
	pathTmp       = "/tmp"

	// KeyAdmission and KeyCA are the files of the bootstrap Secret.
	KeyAdmission = "admission"
	KeyCA        = "ca.crt"
)

func ptr[T any](v T) *T { return &v }

func resources(b ResourceBounds) *kubernetes.Resources {
	return &kubernetes.Resources{
		Requests: map[string]string{"cpu": b.CPURequest, "memory": b.MemoryRequest, "ephemeral-storage": b.EphemeralRequest},
		Limits:   map[string]string{"cpu": b.CPULimit, "memory": b.MemoryLimit, "ephemeral-storage": b.EphemeralLimit},
	}
}

func (r Recipe) containerSecurity(readOnlyRoot bool) *kubernetes.ContainerSecurity {
	c := r.policy.cfg
	return &kubernetes.ContainerSecurity{
		Privileged: ptr(false), AllowPrivilegeEscalation: ptr(false), ReadOnlyRootFilesystem: ptr(readOnlyRoot),
		RunAsNonRoot: ptr(true), RunAsUser: ptr(c.RunAsUser), RunAsGroup: ptr(c.RunAsGroup),
		Capabilities:   &kubernetes.Capabilities{Drop: []string{"ALL"}},
		SeccompProfile: &kubernetes.SeccompProfile{Type: "RuntimeDefault"},
	}
}

// ObjectLabels are the labels of the Job and its Pods. A run_shared Job adds the
// labels the run's network policies select its Pods by.
func (r Recipe) ObjectLabels() map[string]string {
	l := r.identity().Labels()
	if r.shared != nil {
		for k, v := range r.sharedPodLabels() {
			l[k] = v
		}
	}
	return l
}

// Annotations record ownership evidence on the Job. They are checked against
// committed data, never trusted on their own.
func (r Recipe) Annotations() map[string]string {
	a := r.identity().Annotations()
	if r.shared != nil {
		a[AnnRunModel], a[AnnSharedDigest] = ValueRunShared, sharedDigest(*r.shared)
	}
	return a
}

func (r Recipe) identity() Identity {
	return Identity{Scope: r.Scope, RunID: r.RunID, StepID: r.StepID, AttemptID: r.AttemptID, JobName: r.JobName, ContextDigest: r.ContextDigest}
}

// Job builds the Job without its fingerprint annotation (the fingerprint is
// computed over this document).
func (r Recipe) Job() kubernetes.Job {
	c := r.policy.cfg
	v := c.Volumes
	mem := func(size string) *kubernetes.EmptyDir { return &kubernetes.EmptyDir{Medium: "Memory", SizeLimit: size} }
	disk := func(size string) *kubernetes.EmptyDir { return &kubernetes.EmptyDir{SizeLimit: size} }
	volumes := []kubernetes.Volume{
		{Name: "tools", EmptyDir: disk(v.Tools)},
		{Name: "workspace", EmptyDir: disk(v.Workspace)},
		{Name: "outputs", EmptyDir: disk(v.Outputs)},
		{Name: "results", EmptyDir: disk(v.Results)},
		{Name: "tmp", EmptyDir: disk(v.Scratch)},
		{Name: "secrets", EmptyDir: mem(v.Secrets)},
		{Name: "identity", EmptyDir: mem(v.Identity)},
		{Name: "bootstrap", Secret: &kubernetes.SecretVolume{SecretName: SecretName(r.JobName), DefaultMode: ptr(int32(0o440)), Optional: ptr(false)}},
	}
	inject := kubernetes.Container{
		Name: InjectContainer, Image: c.InjectorImage, ImagePullPolicy: c.ImagePullPolicy,
		Command:         append([]string(nil), c.InjectorCommand...),
		Resources:       resources(c.Injector),
		VolumeMounts:    []kubernetes.VolumeMount{{Name: "tools", MountPath: pathTools}},
		SecurityContext: r.containerSecurity(true),
	}
	args := []string{
		"step",
		"--server-url=" + c.RuntimeURL,
		"--attempt-id=" + string(r.AttemptID),
		"--identity-dir=" + pathIdentity,
		"--admission-credential-file=" + pathBootstrap + "/" + KeyAdmission,
		"--ca-file=" + pathBootstrap + "/" + KeyCA,
		"--workspace-dir=" + pathWorkspace,
		"--outputs-dir=" + pathOutputs,
		"--secrets-dir=" + pathSecrets,
		"--results-file=" + pathResults + "/results.json",
		"--scratch-dir=" + pathTmp,
	}
	if sourceAccess(r.Access) && r.shared == nil {
		// A run_shared attempt receives no checkout credential: the run's one
		// preparation checked the source out into the shared workspace (seam S2).
		// The checkout material lives in the same read-only bootstrap volume as the
		// admission credential. The paths are fixed text: nothing secret is in the
		// Job document.
		args = append(args,
			"--checkout-credential-file="+pathBootstrap+"/"+KeyCheckoutCredential,
			"--git-ca-file="+pathBootstrap+"/"+KeyGitCA,
		)
	}
	fieldRef := func(path string) *kubernetes.EnvVarSource {
		return &kubernetes.EnvVarSource{FieldRef: &kubernetes.FieldRef{APIVersion: "v1", FieldPath: path}}
	}
	primary := kubernetes.Container{
		Name: PrimaryContainer, Image: r.Image, ImagePullPolicy: c.ImagePullPolicy,
		Command: []string{pathTools + "/kodama"}, Args: args,
		WorkingDir: pathTmp,
		Env: []kubernetes.EnvVar{
			{Name: "KODAMA_JOB_NAME", Value: r.JobName},
			{Name: "KODAMA_JOB_UID", ValueFrom: fieldRef("metadata.labels['" + PodLabelJobUID + "']")},
			{Name: "KODAMA_POD_NAME", ValueFrom: fieldRef("metadata.name")},
			{Name: "KODAMA_POD_UID", ValueFrom: fieldRef("metadata.uid")},
			{Name: "HOME", Value: pathTmp},
			{Name: "TMPDIR", Value: pathTmp},
		},
		Resources: resources(c.Workload),
		VolumeMounts: []kubernetes.VolumeMount{
			{Name: "tools", MountPath: pathTools, ReadOnly: true},
			{Name: "workspace", MountPath: pathWorkspace},
			{Name: "outputs", MountPath: pathOutputs},
			{Name: "results", MountPath: pathResults},
			{Name: "tmp", MountPath: pathTmp},
			{Name: "secrets", MountPath: pathSecrets},
			{Name: "identity", MountPath: pathIdentity},
			{Name: "bootstrap", MountPath: pathBootstrap, ReadOnly: true},
		},
		SecurityContext: r.containerSecurity(false),
	}
	podSecurity := &kubernetes.PodSecurity{
		RunAsNonRoot: ptr(true), RunAsUser: ptr(c.RunAsUser), RunAsGroup: ptr(c.RunAsGroup), FSGroup: ptr(c.RunAsGroup),
		SeccompProfile: &kubernetes.SeccompProfile{Type: "RuntimeDefault"},
	}
	var hostUsers *bool
	if r.shared != nil {
		// The run_shared shape (seams S1-S3): the primary container takes exactly
		// the pinned step role; the injector keeps its restricted shape. HOME, the
		// XDG runtime directory and TMPDIR are attempt-private and bounded; the
		// workspace is the run's claim.
		primary.SecurityContext = r.sharedSec.container
		podSecurity, hostUsers = r.sharedSec.pod, r.sharedSec.hostUsers
		primary.Env = []kubernetes.EnvVar{
			{Name: "KODAMA_JOB_NAME", Value: r.JobName},
			{Name: "KODAMA_JOB_UID", ValueFrom: fieldRef("metadata.labels['" + PodLabelJobUID + "']")},
			{Name: "KODAMA_POD_NAME", ValueFrom: fieldRef("metadata.name")},
			{Name: "KODAMA_POD_UID", ValueFrom: fieldRef("metadata.uid")},
			{Name: "KODAMA_RUNTIME_API", Value: "v2"},
			{Name: "KODAMA_WORKSPACE_DIR", Value: pathWorkspace},
			{Name: "HOME", Value: pathHome},
			{Name: "TMPDIR", Value: pathTmp},
			{Name: "XDG_RUNTIME_DIR", Value: pathRun},
		}
		primary.VolumeMounts = append(primary.VolumeMounts,
			kubernetes.VolumeMount{Name: "home", MountPath: pathHome},
			kubernetes.VolumeMount{Name: "run", MountPath: pathRun},
		)
		volumes = r.sharedVolumes(volumes)
	}
	grace := int64(c.TerminationGrace.Seconds())
	return kubernetes.Job{
		APIVersion: "batch/v1", Kind: "Job",
		Metadata: kubernetes.ObjectMeta{Name: r.JobName, Namespace: r.Target.Namespace, Labels: r.ObjectLabels(), Annotations: r.Annotations()},
		Spec: kubernetes.JobSpec{
			Parallelism: ptr(int32(1)), Completions: ptr(int32(1)), BackoffLimit: ptr(int32(0)),
			Template: kubernetes.PodTemplate{
				Metadata: kubernetes.ObjectMeta{Labels: r.ObjectLabels()},
				Spec: kubernetes.PodSpec{
					ServiceAccountName: r.Target.ServiceAccount, AutomountServiceAccountToken: ptr(false), EnableServiceLinks: ptr(false),
					RestartPolicy: "Never", TerminationGracePeriodSeconds: &grace, HostUsers: hostUsers,
					SecurityContext: podSecurity,
					InitContainers:  []kubernetes.Container{inject},
					Containers:      []kubernetes.Container{primary},
					Volumes:         volumes,
				},
			},
		},
	}
}

// JobObject is the Job to create: the recipe's Job plus the fingerprint
// annotation that the verification compares against committed data.
func (r Recipe) JobObject(fingerprint string) kubernetes.Job {
	j := r.Job()
	j.Metadata.Annotations[AnnFingerprint] = fingerprint
	return j
}

// BootstrapSecretData is the content of the attempt's admission Secret for a
// credential value. The CA is public trust material. A source-consuming attempt
// also carries its read-only checkout credential and Git CA; checkout must be
// nil exactly for attempts without a workspace, which keep the two-key layout.
func (r Recipe) BootstrapSecretData(credential string, checkout *CheckoutMaterial) map[string][]byte {
	data := map[string][]byte{KeyAdmission: []byte(credential), KeyCA: r.policy.RuntimeCA()}
	if checkout != nil {
		data[KeyCheckoutCredential] = append([]byte(nil), checkout.Credential...)
		data[KeyGitCA] = append([]byte(nil), checkout.GitCA...)
	}
	return data
}
