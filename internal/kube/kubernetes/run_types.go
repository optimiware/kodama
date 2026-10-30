package kubernetes

import "encoding/json"

// Wire types of the run-resource objects: claims, volumes, volume attachments,
// Services and probes. Like the types in types.go they are bounded subsets and
// keep the server's raw document for field-by-field verification.

// ContainerPort declares a container port. Protocol is always explicit so the
// object the server stores equals the object created.
type ContainerPort struct {
	Name          string `json:"name,omitempty"`
	ContainerPort int32  `json:"containerPort"`
	Protocol      string `json:"protocol,omitempty"`
}

// Probe is a readiness probe with exactly one handler.
type Probe struct {
	TCPSocket           *TCPSocketAction `json:"tcpSocket,omitempty"`
	HTTPGet             *HTTPGetAction   `json:"httpGet,omitempty"`
	Exec                *ExecAction      `json:"exec,omitempty"`
	TimeoutSeconds      int32            `json:"timeoutSeconds,omitempty"`
	PeriodSeconds       int32            `json:"periodSeconds,omitempty"`
	SuccessThreshold    int32            `json:"successThreshold,omitempty"`
	FailureThreshold    int32            `json:"failureThreshold,omitempty"`
	InitialDelaySeconds int32            `json:"initialDelaySeconds,omitempty"`
}

// TCPSocketAction is a TCP connect probe.
type TCPSocketAction struct {
	Port int32 `json:"port"`
}

// HTTPGetAction is an HTTP GET probe over plain HTTP.
type HTTPGetAction struct {
	Path   string `json:"path"`
	Port   int32  `json:"port"`
	Scheme string `json:"scheme,omitempty"`
}

// ExecAction runs a command inside the container.
type ExecAction struct {
	Command []string `json:"command"`
}

// ---- PersistentVolumeClaim, PersistentVolume, VolumeAttachment ----

// PersistentVolumeClaim is a core/v1 claim.
type PersistentVolumeClaim struct {
	APIVersion string          `json:"apiVersion,omitempty"`
	Kind       string          `json:"kind,omitempty"`
	Metadata   ObjectMeta      `json:"metadata"`
	Spec       PVCSpec         `json:"spec"`
	Status     PVCStatus       `json:"status,omitempty"`
	Raw        json.RawMessage `json:"-"`
}

// PVCSpec is the part of a claim the executor controls.
type PVCSpec struct {
	AccessModes      []string        `json:"accessModes,omitempty"`
	StorageClassName *string         `json:"storageClassName,omitempty"`
	Resources        VolumeResources `json:"resources"`
	VolumeMode       string          `json:"volumeMode,omitempty"`
	// VolumeName is the bound volume (set by the control plane, never by us).
	VolumeName string `json:"volumeName,omitempty"`
}

// VolumeResources are a claim's storage requests.
type VolumeResources struct {
	Requests map[string]string `json:"requests,omitempty"`
}

// PVCStatus is the observed claim state: Pending, Bound or Lost.
type PVCStatus struct {
	Phase string `json:"phase,omitempty"`
}

// PersistentVolume is a cluster-scoped core/v1 volume, read for observation only.
type PersistentVolume struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		ClaimRef                      *ObjectReference `json:"claimRef,omitempty"`
		PersistentVolumeReclaimPolicy string           `json:"persistentVolumeReclaimPolicy,omitempty"`
		StorageClassName              string           `json:"storageClassName,omitempty"`
	} `json:"spec"`
	Status struct {
		Phase string `json:"phase,omitempty"`
	} `json:"status,omitempty"`
}

// ObjectReference names the object a volume is claimed by.
type ObjectReference struct {
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
	UID       string `json:"uid,omitempty"`
}

// VolumeAttachment is a cluster-scoped storage.k8s.io/v1 attachment, read for
// observation only: an attachment of a volume means some node may still use it.
type VolumeAttachment struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		Attacher string `json:"attacher,omitempty"`
		NodeName string `json:"nodeName,omitempty"`
		Source   struct {
			PersistentVolumeName *string `json:"persistentVolumeName,omitempty"`
		} `json:"source"`
	} `json:"spec"`
	Status struct {
		Attached bool `json:"attached,omitempty"`
	} `json:"status,omitempty"`
}

// ---- Service ----

// Service is a core/v1 Service. The executor creates only ClusterIP Services.
type Service struct {
	APIVersion string          `json:"apiVersion,omitempty"`
	Kind       string          `json:"kind,omitempty"`
	Metadata   ObjectMeta      `json:"metadata"`
	Spec       ServiceSpec     `json:"spec"`
	Raw        json.RawMessage `json:"-"`
}

// ServiceSpec is the part of a Service spec the executor controls.
type ServiceSpec struct {
	Type      string            `json:"type,omitempty"`
	Selector  map[string]string `json:"selector,omitempty"`
	Ports     []ServicePort     `json:"ports,omitempty"`
	ClusterIP string            `json:"clusterIP,omitempty"`
}

// ServicePort is one Service port. TargetPort is the container port number.
type ServicePort struct {
	Name       string `json:"name,omitempty"`
	Protocol   string `json:"protocol,omitempty"`
	Port       int32  `json:"port"`
	TargetPort int32  `json:"targetPort"`
}

// StorageClass is a cluster-scoped storage.k8s.io/v1 class, read for observation
// only: whether it binds a claim on first use and what happens to a volume when
// its claim is deleted.
type StorageClass struct {
	Metadata          ObjectMeta `json:"metadata"`
	Provisioner       string     `json:"provisioner,omitempty"`
	ReclaimPolicy     string     `json:"reclaimPolicy,omitempty"`
	VolumeBindingMode string     `json:"volumeBindingMode,omitempty"`
}
