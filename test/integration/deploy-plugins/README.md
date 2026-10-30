# Deploy plugin integration

`TestKubernetesPluginDeploysDummyApp` starts or reuses a kind cluster, loads the
dummy nginx image, starts an in-process state API, builds the deploy binary and
runs the real Kubernetes driver against the cluster. It then reads status and
cleans up the Deployment.

```sh
KODAMA_REQUIRE_KIND=1 \
KIND_BIN=/tmp/kind \
KODAMA_TEST_CLUSTER=kodama-plugin-test \
KODAMA_TEST_KUBECONFIG=/tmp/kodama-plugin-kubeconfig \
CGO_ENABLED=0 GOTOOLCHAIN=go1.27.2 \
go test -count=1 -v ./test/integration/deploy-plugins/...
```

The test requires Podman (for loading the image), kind and kubectl. A missing
cluster, image, state API or provider command fails the test; it does not pass by
skip when `KODAMA_REQUIRE_KIND=1`.

The state API in this test uses the in-memory state adapter. It proves the
plugin/runtime state contract and the Kubernetes provider path, not PostgreSQL
RLS or the full Kodama server composition.
