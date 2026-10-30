package kubernetes

import "encoding/json"

// The types below are the bounded subset of the Kubernetes API this module
// reads and writes. They are wire types: they never leave the adapter's
// consumers except as opaque values handed back to it. Fields the module does
// not create are absent here; objects read back keep their raw document so a
// verifier can compare every field the server returned, not only the ones
// modeled here.

// ObjectMeta is the common object metadata.
type ObjectMeta struct {
	Name              string            `json:"name,omitempty"`
	Namespace         string            `json:"namespace,omitempty"`
	UID               string            `json:"uid,omitempty"`
	ResourceVersion   string            `json:"resourceVersion,omitempty"`
	CreationTimestamp string            `json:"creationTimestamp,omitempty"`
	DeletionTimestamp string            `json:"deletionTimestamp,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
	OwnerReferences   []OwnerReference  `json:"ownerReferences,omitempty"`
	Finalizers        []string          `json:"finalizers,omitempty"`
}

// OwnerReference names an object's owner.
type OwnerReference struct {
	APIVersion         string `json:"apiVersion"`
	Kind               string `json:"kind"`
	Name               string `json:"name"`
	UID                string `json:"uid"`
	Controller         *bool  `json:"controller,omitempty"`
	BlockOwnerDeletion *bool  `json:"blockOwnerDeletion,omitempty"`
}

// Controlled reports whether ref is the controlling reference.
func (r OwnerReference) Controlled() bool { return r.Controller != nil && *r.Controller }

// ---- Job ----

// Job is a batch/v1 Job.
type Job struct {
	APIVersion string     `json:"apiVersion,omitempty"`
	Kind       string     `json:"kind,omitempty"`
	Metadata   ObjectMeta `json:"metadata"`
	Spec       JobSpec    `json:"spec"`
	Status     JobStatus  `json:"status,omitempty"`
	// Raw is the document as the server returned it (empty for objects built
	// locally).
	Raw json.RawMessage `json:"-"`
}

// JobSpec is the part of a Job spec the executor controls.
type JobSpec struct {
	Parallelism  *int32 `json:"parallelism,omitempty"`
	Completions  *int32 `json:"completions,omitempty"`
	BackoffLimit *int32 `json:"backoffLimit,omitempty"`
	// ActiveDeadlineSeconds bounds the Job's run time (set for preparation only).
	ActiveDeadlineSeconds *int64      `json:"activeDeadlineSeconds,omitempty"`
	Template              PodTemplate `json:"template"`
}

// PodTemplate is a Job's Pod template.
type PodTemplate struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     PodSpec    `json:"spec"`
}

// JobCondition is one Job status condition.
type JobCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason,omitempty"`
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
}

// JobStatus is the observed Job state.
type JobStatus struct {
	Conditions     []JobCondition `json:"conditions,omitempty"`
	Active         int32          `json:"active,omitempty"`
	Succeeded      int32          `json:"succeeded,omitempty"`
	Failed         int32          `json:"failed,omitempty"`
	StartTime      string         `json:"startTime,omitempty"`
	CompletionTime string         `json:"completionTime,omitempty"`
}

// Condition returns the named true condition.
func (s JobStatus) Condition(kind string) (JobCondition, bool) {
	for _, c := range s.Conditions {
		if c.Type == kind && c.Status == "True" {
			return c, true
		}
	}
	return JobCondition{}, false
}

// ---- Pod ----

// PodSpec is the Pod spec subset the executor creates and verifies.
type PodSpec struct {
	ServiceAccountName           string `json:"serviceAccountName,omitempty"`
	AutomountServiceAccountToken *bool  `json:"automountServiceAccountToken,omitempty"`
	EnableServiceLinks           *bool  `json:"enableServiceLinks,omitempty"`
	RestartPolicy                string `json:"restartPolicy,omitempty"`
	DNSPolicy                    string `json:"dnsPolicy,omitempty"`
	SchedulerName                string `json:"schedulerName,omitempty"`
	HostNetwork                  bool   `json:"hostNetwork,omitempty"`
	HostPID                      bool   `json:"hostPID,omitempty"`
	HostIPC                      bool   `json:"hostIPC,omitempty"`
	// HostUsers false selects a Pod user namespace.
	HostUsers                     *bool             `json:"hostUsers,omitempty"`
	ShareProcessNamespace         *bool             `json:"shareProcessNamespace,omitempty"`
	TerminationGracePeriodSeconds *int64            `json:"terminationGracePeriodSeconds,omitempty"`
	SecurityContext               *PodSecurity      `json:"securityContext,omitempty"`
	InitContainers                []Container       `json:"initContainers,omitempty"`
	Containers                    []Container       `json:"containers"`
	Volumes                       []Volume          `json:"volumes,omitempty"`
	NodeName                      string            `json:"nodeName,omitempty"`
	NodeSelector                  map[string]string `json:"nodeSelector,omitempty"`
}

// PodSecurity is the Pod-level security context.
type PodSecurity struct {
	RunAsNonRoot       *bool           `json:"runAsNonRoot,omitempty"`
	RunAsUser          *int64          `json:"runAsUser,omitempty"`
	RunAsGroup         *int64          `json:"runAsGroup,omitempty"`
	FSGroup            *int64          `json:"fsGroup,omitempty"`
	SupplementalGroups []int64         `json:"supplementalGroups,omitempty"`
	SeccompProfile     *SeccompProfile `json:"seccompProfile,omitempty"`
}

// SeccompProfile selects a seccomp profile.
type SeccompProfile struct {
	Type string `json:"type"`
	// LocalhostProfile is the profile file relative to the kubelet's seccomp
	// directory; set exactly when Type is Localhost.
	LocalhostProfile string `json:"localhostProfile,omitempty"`
}

// AppArmorProfile selects an AppArmor profile (RuntimeDefault or Localhost).
type AppArmorProfile struct {
	Type             string `json:"type"`
	LocalhostProfile string `json:"localhostProfile,omitempty"`
}

// Container is one container.
type Container struct {
	Name                     string             `json:"name"`
	Image                    string             `json:"image"`
	ImagePullPolicy          string             `json:"imagePullPolicy,omitempty"`
	Command                  []string           `json:"command,omitempty"`
	Args                     []string           `json:"args,omitempty"`
	WorkingDir               string             `json:"workingDir,omitempty"`
	Env                      []EnvVar           `json:"env,omitempty"`
	Ports                    []ContainerPort    `json:"ports,omitempty"`
	Resources                *Resources         `json:"resources,omitempty"`
	VolumeMounts             []VolumeMount      `json:"volumeMounts,omitempty"`
	ReadinessProbe           *Probe             `json:"readinessProbe,omitempty"`
	SecurityContext          *ContainerSecurity `json:"securityContext,omitempty"`
	TerminationMessagePolicy string             `json:"terminationMessagePolicy,omitempty"`
}

// ContainerSecurity is the container-level security context.
type ContainerSecurity struct {
	Privileged               *bool            `json:"privileged,omitempty"`
	AllowPrivilegeEscalation *bool            `json:"allowPrivilegeEscalation,omitempty"`
	ReadOnlyRootFilesystem   *bool            `json:"readOnlyRootFilesystem,omitempty"`
	RunAsNonRoot             *bool            `json:"runAsNonRoot,omitempty"`
	RunAsUser                *int64           `json:"runAsUser,omitempty"`
	RunAsGroup               *int64           `json:"runAsGroup,omitempty"`
	Capabilities             *Capabilities    `json:"capabilities,omitempty"`
	SeccompProfile           *SeccompProfile  `json:"seccompProfile,omitempty"`
	AppArmorProfile          *AppArmorProfile `json:"appArmorProfile,omitempty"`
	// ProcMount is "Unmasked" only for a user-namespaced builder profile.
	ProcMount string `json:"procMount,omitempty"`
}

// Capabilities adds and drops Linux capabilities.
type Capabilities struct {
	Add  []string `json:"add,omitempty"`
	Drop []string `json:"drop,omitempty"`
}

// EnvVar is an environment variable with a literal or a Downward API value.
type EnvVar struct {
	Name      string        `json:"name"`
	Value     string        `json:"value,omitempty"`
	ValueFrom *EnvVarSource `json:"valueFrom,omitempty"`
}

// EnvVarSource is a Downward API field reference.
type EnvVarSource struct {
	FieldRef *FieldRef `json:"fieldRef,omitempty"`
}

// FieldRef selects a Pod field.
type FieldRef struct {
	APIVersion string `json:"apiVersion,omitempty"`
	FieldPath  string `json:"fieldPath"`
}

// Resources are compute and storage quantities (Kubernetes quantity strings).
type Resources struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

// VolumeMount mounts a volume.
type VolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

// Volume is a Pod volume. Only the sources the executor creates are modeled.
type Volume struct {
	Name      string        `json:"name"`
	EmptyDir  *EmptyDir     `json:"emptyDir,omitempty"`
	Secret    *SecretVolume `json:"secret,omitempty"`
	ConfigMap *ConfigVolume `json:"configMap,omitempty"`
	// PersistentVolumeClaim mounts a claim of the Pod's namespace.
	PersistentVolumeClaim *PVCVolume `json:"persistentVolumeClaim,omitempty"`
}

// PVCVolume mounts a PersistentVolumeClaim.
type PVCVolume struct {
	ClaimName string `json:"claimName"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

// EmptyDir is node-local scratch; Medium "Memory" backs it with tmpfs.
type EmptyDir struct {
	Medium    string `json:"medium,omitempty"`
	SizeLimit string `json:"sizeLimit,omitempty"`
}

// SecretVolume projects a Secret.
type SecretVolume struct {
	SecretName  string `json:"secretName"`
	DefaultMode *int32 `json:"defaultMode,omitempty"`
	Optional    *bool  `json:"optional,omitempty"`
	// Items projects only the listed keys (absent: every key).
	Items []KeyToPath `json:"items,omitempty"`
}

// KeyToPath projects one Secret key to a relative file path.
type KeyToPath struct {
	Key  string `json:"key"`
	Path string `json:"path"`
}

// ConfigVolume projects a ConfigMap.
type ConfigVolume struct {
	Name        string `json:"name"`
	DefaultMode *int32 `json:"defaultMode,omitempty"`
	Optional    *bool  `json:"optional,omitempty"`
}

// Pod is a core/v1 Pod.
type Pod struct {
	APIVersion string          `json:"apiVersion,omitempty"`
	Kind       string          `json:"kind,omitempty"`
	Metadata   ObjectMeta      `json:"metadata"`
	Spec       PodSpec         `json:"spec"`
	Status     PodStatus       `json:"status,omitempty"`
	Raw        json.RawMessage `json:"-"`
}

// PodStatus is the observed Pod state.
type PodStatus struct {
	Phase                 string            `json:"phase,omitempty"`
	Reason                string            `json:"reason,omitempty"`
	Conditions            []PodCondition    `json:"conditions,omitempty"`
	InitContainerStatuses []ContainerStatus `json:"initContainerStatuses,omitempty"`
	ContainerStatuses     []ContainerStatus `json:"containerStatuses,omitempty"`
}

// PodCondition is one Pod condition.
type PodCondition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// ContainerStatus is the observed state of one container.
type ContainerStatus struct {
	Name    string         `json:"name"`
	Image   string         `json:"image,omitempty"`
	Started *bool          `json:"started,omitempty"`
	Ready   bool           `json:"ready,omitempty"`
	State   ContainerState `json:"state,omitempty"`
	// LastState keeps the previous termination.
	LastState ContainerState `json:"lastState,omitempty"`
}

// ContainerState is a container's waiting, running or terminated state.
type ContainerState struct {
	Waiting    *ContainerWaiting    `json:"waiting,omitempty"`
	Running    *ContainerRunning    `json:"running,omitempty"`
	Terminated *ContainerTerminated `json:"terminated,omitempty"`
}

// ContainerWaiting is a waiting container.
type ContainerWaiting struct {
	Reason string `json:"reason,omitempty"`
}

// ContainerRunning is a running container.
type ContainerRunning struct {
	StartedAt string `json:"startedAt,omitempty"`
}

// ContainerTerminated is a terminated container.
type ContainerTerminated struct {
	ExitCode int32  `json:"exitCode"`
	Reason   string `json:"reason,omitempty"`
}

// ---- Secret, ConfigMap, Namespace and policy objects ----

// Secret is a core/v1 Secret. Data values are base64 on the wire; Value and
// SetValue handle the encoding.
type Secret struct {
	Metadata   ObjectMeta        `json:"metadata"`
	Type       string            `json:"type,omitempty"`
	Immutable  *bool             `json:"immutable,omitempty"`
	Data       map[string][]byte `json:"data,omitempty"`
	APIVersion string            `json:"apiVersion,omitempty"`
	Kind       string            `json:"kind,omitempty"`
}

// ConfigMap is a core/v1 ConfigMap.
type ConfigMap struct {
	Metadata ObjectMeta        `json:"metadata"`
	Data     map[string]string `json:"data,omitempty"`
}

// Namespace is a core/v1 Namespace.
type Namespace struct {
	Metadata ObjectMeta `json:"metadata"`
	Status   struct {
		Phase string `json:"phase,omitempty"`
	} `json:"status,omitempty"`
}

// ServiceAccount is a core/v1 ServiceAccount.
type ServiceAccount struct {
	Metadata                     ObjectMeta `json:"metadata"`
	AutomountServiceAccountToken *bool      `json:"automountServiceAccountToken,omitempty"`
}

// ResourceQuota is a core/v1 ResourceQuota.
type ResourceQuota struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		Hard map[string]string `json:"hard,omitempty"`
		// Scopes and ScopeSelector restrict the quota to some Pods. A quota with
		// either does not bound every Pod in the namespace.
		Scopes        []string       `json:"scopes,omitempty"`
		ScopeSelector *ScopeSelector `json:"scopeSelector,omitempty"`
	} `json:"spec"`
}

// Scoped reports whether the quota applies only to a subset of Pods.
func (q ResourceQuota) Scoped() bool {
	return len(q.Spec.Scopes) > 0 || q.Spec.ScopeSelector != nil
}

// ScopeSelector selects the Pods a ResourceQuota covers.
type ScopeSelector struct {
	MatchExpressions []ScopedResourceSelectorRequirement `json:"matchExpressions,omitempty"`
}

// ScopedResourceSelectorRequirement is one scope requirement.
type ScopedResourceSelectorRequirement struct {
	ScopeName string   `json:"scopeName"`
	Operator  string   `json:"operator"`
	Values    []string `json:"values,omitempty"`
}

// NetworkPolicy is a networking.k8s.io/v1 NetworkPolicy. Rules are kept as
// generic documents: the policy check inspects their content.
type NetworkPolicy struct {
	APIVersion string            `json:"apiVersion,omitempty"`
	Kind       string            `json:"kind,omitempty"`
	Metadata   ObjectMeta        `json:"metadata"`
	Spec       NetworkPolicySpec `json:"spec"`
	// Raw is the document as the server returned it (set by the selecting read).
	Raw json.RawMessage `json:"-"`
}

// NetworkPolicySpec is a NetworkPolicy's spec.
type NetworkPolicySpec struct {
	PodSelector LabelSelector    `json:"podSelector"`
	PolicyTypes []string         `json:"policyTypes,omitempty"`
	Ingress     []map[string]any `json:"ingress,omitempty"`
	Egress      []NetworkEgress  `json:"egress,omitempty"`
}

// LabelSelector is a Kubernetes label selector.
type LabelSelector struct {
	MatchLabels      map[string]string `json:"matchLabels,omitempty"`
	MatchExpressions []map[string]any  `json:"matchExpressions,omitempty"`
}

// Empty reports whether the selector selects every Pod.
func (s LabelSelector) Empty() bool { return len(s.MatchLabels) == 0 && len(s.MatchExpressions) == 0 }

// NetworkEgress is one egress rule.
type NetworkEgress struct {
	To    []NetworkPeer `json:"to,omitempty"`
	Ports []NetworkPort `json:"ports,omitempty"`
}

// NetworkPeer is an egress destination.
type NetworkPeer struct {
	IPBlock           *IPBlock       `json:"ipBlock,omitempty"`
	NamespaceSelector *LabelSelector `json:"namespaceSelector,omitempty"`
	PodSelector       *LabelSelector `json:"podSelector,omitempty"`
}

// IPBlock is a CIDR range.
type IPBlock struct {
	CIDR   string   `json:"cidr"`
	Except []string `json:"except,omitempty"`
}

// NetworkPort is a protocol and port.
type NetworkPort struct {
	Protocol string `json:"protocol,omitempty"`
	Port     any    `json:"port,omitempty"`
	// EndPort extends Port to a range; a range admits more than one port.
	EndPort *int32 `json:"endPort,omitempty"`
}

// AccessReview is an authorization.k8s.io/v1 SubjectAccessReview, used to ask
// the API server what a ServiceAccount may do (never to grant anything).
type AccessReview struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Spec       struct {
		User               string              `json:"user"`
		Groups             []string            `json:"groups,omitempty"`
		ResourceAttributes *ResourceAttributes `json:"resourceAttributes,omitempty"`
	} `json:"spec"`
	Status struct {
		Allowed bool   `json:"allowed"`
		Denied  bool   `json:"denied,omitempty"`
		Reason  string `json:"reason,omitempty"`
	} `json:"status,omitempty"`
}

// ResourceAttributes is the verb/resource of an access question.
type ResourceAttributes struct {
	Namespace   string `json:"namespace,omitempty"`
	Verb        string `json:"verb"`
	Group       string `json:"group,omitempty"`
	Resource    string `json:"resource"`
	Subresource string `json:"subresource,omitempty"`
}
