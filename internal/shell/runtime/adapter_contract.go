// Package runtime declares the runtime-adapter seam shared by the Process,
// Podman and Kubernetes adapters.
//
// Every adapter implements exactly the Kernel runtime operations plus a pinned
// external API version. Start outcomes and observations stay in the declared
// Kernel vocabulary: absent/unknown never prove nonexecution, and deleting a
// Kubernetes Job alone is not stop confirmation. Capabilities are explicit so
// admitted requirements can be checked before dispatch, and the Process adapter
// makes no container-isolation claim.
//
// This is a declaration seam. No process, container, Podman socket, Kubernetes
// client or cloud API is started or contacted here.
package runtime

import (
	"context"

	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/engine"
)

// AdapterKind names one explicit runtime family.
type AdapterKind string

const (
	// AdapterProcess is the development host-process executor.
	AdapterProcess AdapterKind = "process"
	// AdapterPodman is the test Podman container executor.
	AdapterPodman AdapterKind = "podman"
	// AdapterKubernetes is the production Kubernetes Job executor.
	AdapterKubernetes AdapterKind = "kubernetes"
)

// AdapterKinds returns every runtime family in declaration order.
func AdapterKinds() []AdapterKind {
	return []AdapterKind{AdapterProcess, AdapterPodman, AdapterKubernetes}
}

// Valid reports whether the kind is one of the declared runtime families.
func (kind AdapterKind) Valid() bool {
	return kind == AdapterProcess || kind == AdapterPodman || kind == AdapterKubernetes
}

// APIVersion is the pinned external API surface one adapter targets. It is an
// API version, not a product release claim.
type APIVersion struct {
	Adapter AdapterKind
	Version string
}

// PinnedAPIVersions returns the declared external API version of each adapter.
func PinnedAPIVersions() []APIVersion {
	return []APIVersion{
		{Adapter: AdapterProcess, Version: "process/v1"},
		{Adapter: AdapterPodman, Version: "libpod/v4"},
		{Adapter: AdapterKubernetes, Version: "batch/v1"},
	}
}

// Capability is one explicit runtime capability an admitted pipeline may
// require. It is never inferred from the adapter family.
type Capability string

const (
	// CapabilityProcess reports host-process execution.
	CapabilityProcess Capability = "process"
	// CapabilityProcessGroupStop reports whole-process-group stop evidence.
	CapabilityProcessGroupStop Capability = "process-group-stop"
	// CapabilityCredentialRefresh reports in-place credential replacement.
	CapabilityCredentialRefresh Capability = "credential-refresh"
	// CapabilityContainer reports container execution.
	CapabilityContainer Capability = "container"
	// CapabilityRetainedWorkspace reports a retained workspace volume.
	CapabilityRetainedWorkspace Capability = "retained-workspace"
	// CapabilityNetworkIsolation reports tenant network isolation.
	CapabilityNetworkIsolation Capability = "network-isolation"
	// CapabilityJobDeletionNotStopProof marks that deleting the Job object is
	// not proof the process stopped.
	CapabilityJobDeletionNotStopProof Capability = "job-deletion-not-stop-proof"
)

// CapabilitiesFor returns the explicit capability set of one adapter family.
// An unknown family declares no capabilities.
func CapabilitiesFor(kind AdapterKind) []Capability {
	switch kind {
	case AdapterProcess:
		return []Capability{
			CapabilityProcess,
			CapabilityProcessGroupStop,
			CapabilityCredentialRefresh,
		}
	case AdapterPodman:
		return []Capability{
			CapabilityContainer,
			CapabilityRetainedWorkspace,
			CapabilityCredentialRefresh,
			CapabilityProcessGroupStop,
		}
	case AdapterKubernetes:
		return []Capability{
			CapabilityContainer,
			CapabilityRetainedWorkspace,
			CapabilityCredentialRefresh,
			CapabilityNetworkIsolation,
			CapabilityJobDeletionNotStopProof,
		}
	default:
		return nil
	}
}

// Client is the complete runtime workload seam. It is the Kernel runtime
// operations plus a pinned external API version, so an adapter cannot invent a
// parallel start/observe vocabulary.
//
// Every operation receives the persisted engine.EffectIdentity so a crash
// before or after an effect cannot make the adapter derive a different workload
// or replay a fresh command. Only Start may report an accepted, proven-not-
// started or unknown outcome; only an affirmative never-started observation
// permits a launch retry.
type Client interface {
	Capabilities(context.Context) (engine.Capabilities, error)
	Prepare(context.Context, engine.PrepareRequest) (engine.Preparation, error)
	Start(context.Context, engine.StartRequest) (engine.StartResult, error)
	Observe(context.Context, engine.ObserveRequest) (kernel.Observation, error)
	Logs(context.Context, engine.LogsRequest) (engine.LogBatch, error)
	Stop(context.Context, engine.StopRequest) (kernel.StopEvidence, error)
	Remove(context.Context, engine.RemoveRequest) (engine.Removal, error)
	RefreshCredential(context.Context, engine.CredentialRequest) (engine.CredentialRefresh, error)
	Version(context.Context) (APIVersion, error)
}
