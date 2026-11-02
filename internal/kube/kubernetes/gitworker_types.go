package kubernetes

import "encoding/json"

// Wire types of the tenant Git hosting objects (T-129): Deployments and their
// ReplicaSets. Like the types in types.go they are bounded subsets; objects read
// back keep their raw document so a verifier can look for fields the subset does
// not model.

// Deployment is an apps/v1 Deployment.
type Deployment struct {
	APIVersion string           `json:"apiVersion,omitempty"`
	Kind       string           `json:"kind,omitempty"`
	Metadata   ObjectMeta       `json:"metadata"`
	Spec       DeploymentSpec   `json:"spec"`
	Status     DeploymentStatus `json:"status,omitempty"`
	// Generation is metadata.generation of an object read from the server (zero
	// for a locally built one). ObjectMeta does not model it.
	Generation int64 `json:"-"`
	// Raw is the document as the server returned it.
	Raw json.RawMessage `json:"-"`
}

// DeploymentSpec is the part of a Deployment spec the Git hosting controls.
type DeploymentSpec struct {
	Replicas                *int32             `json:"replicas,omitempty"`
	Selector                LabelSelector      `json:"selector"`
	Strategy                DeploymentStrategy `json:"strategy"`
	MinReadySeconds         int32              `json:"minReadySeconds,omitempty"`
	RevisionHistoryLimit    *int32             `json:"revisionHistoryLimit,omitempty"`
	ProgressDeadlineSeconds *int32             `json:"progressDeadlineSeconds,omitempty"`
	Template                DeploymentTemplate `json:"template"`
}

// DeploymentStrategy is the rollout strategy: "Recreate" stops the old Pod
// before the new one starts, so at most one Pod of the Deployment exists.
type DeploymentStrategy struct {
	Type string `json:"type,omitempty"`
}

// DeploymentTemplate is a Deployment's Pod template.
type DeploymentTemplate struct {
	Metadata ObjectMeta    `json:"metadata"`
	Spec     WorkerPodSpec `json:"spec"`
}

// WorkerPodSpec is PodSpec whose containers may carry a liveness probe.
type WorkerPodSpec struct {
	PodSpec
	// Containers shadows PodSpec.Containers.
	Containers []WorkerContainer `json:"containers"`
}

// WorkerContainer is Container with the long-running probes.
type WorkerContainer struct {
	Container
	LivenessProbe *Probe `json:"livenessProbe,omitempty"`
}

// DeploymentStatus is the observed rollout state.
type DeploymentStatus struct {
	ObservedGeneration  int64 `json:"observedGeneration,omitempty"`
	Replicas            int32 `json:"replicas,omitempty"`
	UpdatedReplicas     int32 `json:"updatedReplicas,omitempty"`
	ReadyReplicas       int32 `json:"readyReplicas,omitempty"`
	AvailableReplicas   int32 `json:"availableReplicas,omitempty"`
	UnavailableReplicas int32 `json:"unavailableReplicas,omitempty"`
}

// ReplicaSet is an apps/v1 ReplicaSet, read to prove a Pod belongs to a
// Deployment: Pod, ReplicaSet and Deployment are linked by controller UIDs.
type ReplicaSet struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		Replicas *int32 `json:"replicas,omitempty"`
	} `json:"spec"`
	Status struct {
		Replicas      int32 `json:"replicas,omitempty"`
		ReadyReplicas int32 `json:"readyReplicas,omitempty"`
	} `json:"status,omitempty"`
}
