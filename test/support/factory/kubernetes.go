package factory

// Kubernetes variant of the factory harness.
//
// The Podman variant (Start) proves the composed factory over a real Podman
// runtime. This variant proves the same composed server over a real rootless
// kind cluster: the workload is a Kubernetes Job that mounts a
// PersistentVolumeClaim workspace, and the composed runtime/workspace adapters
// are the production Kubernetes ones. Everything else (PostgreSQL, NATS,
// keyfile, retained log sink, REST/MCP edges, credentials) is the shared
// factory composition.
//
// Measured host reachability from a pod inside the pinned kind cluster
// (Podman 5.4.2 rootless, pasta):
//
//   - the host is reachable only at 169.254.1.2 (pasta --map-guest-addr);
//   - that alias reaches a listener bound to the host's default-route IPv4
//     address (the same address the Podman variant's forwarder binds);
//   - the direct host address, the kind network gateway 10.89.1.1 and
//     host.containers.internal (pods have no DNS) all fail;
//   - a listener bound only to loopback is refused.
//
// The Job's KODAMA_URL is therefore the literal http://169.254.1.2:<port>
// address, and the edge forwarder keeps its host-only peer filter: pasta
// presents the pod's connection as the host's own address.

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	driverkube "github.com/optimiware/kodama/internal/shell/app/drivers/kube"
	runtimekube "github.com/optimiware/kodama/internal/shell/runtime/kube"
	"github.com/optimiware/kodama/test/support/infrastructure/harness"
	kubesupport "github.com/optimiware/kodama/test/support/infrastructure/kubernetes"
)

const (
	// KubernetesNamespace is the tenant namespace the Kubernetes harness creates
	// in its private kind cluster. It is labelled with TenantID.
	KubernetesNamespace = "tenant-i04"
	// KubernetesHostAlias is the only address a pod in the pinned rootless kind
	// cluster can use to reach the host (see the package comment).
	KubernetesHostAlias = "169.254.1.2"
	// KubernetesStorageClass is the StorageClass the pinned kind node
	// provisions; it matches kubesupport.StorageClassName.
	KubernetesStorageClass = kubesupport.StorageClassName
	// KubernetesWorkspaceSize is the trusted workspace claim size used by the
	// harness composition.
	KubernetesWorkspaceSize = "64Mi"
	// KubernetesWorkspaceAccessMode is the trusted workspace claim access mode.
	KubernetesWorkspaceAccessMode = "ReadWriteOnce"

	// kubernetesReadyTimeout bounds one cluster's readiness barrier. It matches
	// the component fixture's bound, not the shorter default, because several
	// Podman lanes may run at once.
	kubernetesReadyTimeout = 10 * time.Minute
)

// runtimeVariant selects the workload runtime one factory harness builds.
type runtimeVariant int

const (
	runtimeVariantPodman runtimeVariant = iota
	runtimeVariantKubernetes
)

// kubernetesRuntime is one created kind cluster and the production
// driver/adapter bound to its tenant namespace.
type kubernetesRuntime struct {
	Cluster *kubesupport.Cluster
	Client  *driverkube.Cluster
	Adapter *runtimekube.Adapter
}

// newKubernetesRuntime creates a uniquely named rootless kind cluster, waits
// for its readiness barrier, exports its isolated kubeconfig, creates the
// tenant namespace and builds the production driver and runtime adapter.
//
// Missing infrastructure follows the shared KODAMA_REQUIRE_INFRA policy: a
// missing kind binary or a cluster that cannot start skips unless the
// environment requires infrastructure, in which case it fails. A broken
// composition fixture always fails.
func newKubernetesRuntime(t *testing.T, ctx context.Context, namespace string, tenant id.TenantID, kodamaURL string, clock *TestClock) *kubernetesRuntime {
	t.Helper()

	kind, err := kubesupport.NewKind(kubesupport.WithLogger(t.Logf))
	if err != nil {
		harness.SkipOrFail(t, requireKubernetesInfra(), err)
	}
	cluster, err := kind.CreateCluster(ctx, kubesupport.ClusterOptions{})
	if err != nil {
		harness.SkipOrFail(t, requireKubernetesInfra(), err)
	}
	// Record that this process created a cluster: a later cleanup that cannot
	// resolve kind must fail instead of silently skipping the leak check.
	markKubernetesClusterCreated()
	// The cluster is owned by this test and is deleted even when a later
	// fixture step fails. A cluster this test created must never leak
	// silently, so a failed deletion fails the test and the post-run sweep
	// retries it.
	t.Cleanup(func() {
		if err := cluster.Close(); err != nil {
			t.Errorf("kind cluster %s cleanup: %v", cluster.Name, err)
		}
	})
	kubesupport.RecordCluster(t, cluster.EvidenceRecord())

	readyCtx, cancelReady := context.WithTimeout(ctx, kubernetesReadyTimeout)
	defer cancelReady()
	if err := cluster.WaitReady(readyCtx); err != nil {
		harness.SkipOrFail(t, requireKubernetesInfra(),
			fmt.Errorf("cluster %s is not ready: %w", cluster.Name, err))
	}
	kubeconfig, err := cluster.KubeconfigJSON(readyCtx)
	if err != nil {
		t.Fatalf("factory: export kind kubeconfig: %v", err)
	}
	client, err := driverkube.NewCluster(driverkube.ClusterConfig{
		Kubeconfig: kubeconfig,
		Timeout:    2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("factory: build Kubernetes driver: %v", err)
	}
	if _, err := cluster.Apply(readyCtx, kubernetesNamespaceYAML(namespace, tenant)); err != nil {
		t.Fatalf("factory: create tenant namespace %s: %v", namespace, err)
	}
	adapter, err := runtimekube.New(runtimekube.Config{
		Cluster:   client,
		Namespace: namespace,
		TenantID:  tenant,
		KodamaURL: kodamaURL,
		Policy: runtimekube.Policy{
			CPURequest:    "50m",
			CPULimit:      "200m",
			MemoryRequest: "32Mi",
			MemoryLimit:   "128Mi",
			JobTTLSeconds: 120,
		},
		Cleanup: runtimekube.CleanupPolicy{Attempts: 3, Backoff: 500 * time.Millisecond},
		Clock:   clock,
	})
	if err != nil {
		t.Fatalf("factory: build Kubernetes runtime adapter: %v", err)
	}
	return &kubernetesRuntime{Cluster: cluster, Client: client, Adapter: adapter}
}

// kubernetesNamespaceYAML renders the tenant namespace with the ownership
// label the runtime adapter requires. The namespace name is trusted harness
// configuration, never request-derived.
func kubernetesNamespaceYAML(namespace string, tenant id.TenantID) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %s
  labels:
    %s: %s
`, namespace, runtimekube.LabelTenant, tenant)
}

// requireKubernetesInfra reports whether missing kind/Podman infrastructure
// must fail instead of skipping, matching the shared harness policy.
func requireKubernetesInfra() bool {
	return os.Getenv("KODAMA_REQUIRE_INFRA") == "1"
}
