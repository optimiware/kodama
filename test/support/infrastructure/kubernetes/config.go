// Pinned kind cluster configuration.
package kubesupport

import "fmt"

// kindClusterConfig is the exact configuration passed to `kind create cluster`.
//
//   - KubeletInUserNamespace is required for a rootless (Podman provider)
//     control plane.
//   - no alpha feature gate beyond that: pod logs are read as the one combined
//     log a stock cluster serves, so the harness proves the behavior real
//     clusters have.
//   - the node image is digest-pinned, so a tag cannot move under a run.
const kindClusterConfig = `kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
featureGates:
  KubeletInUserNamespace: true
nodes:
  - role: control-plane
    image: %s
`

// clusterConfig renders the pinned configuration for one node image.
func clusterConfig(image string) string {
	return fmt.Sprintf(kindClusterConfig, image)
}
