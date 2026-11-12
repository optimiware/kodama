//go:build integration

package services

// Delivery recipe: a workload builds and publishes its own image through the
// real registry, the recorded digest is the registry's answer rather than the
// workload's claim, and a lost output response converges without a rebuild or a
// second publication.
//
// The build action runs the pinned SCM workload base (the same deterministic
// base the factory fixture is built from, resolved from the real image store
// before the pipeline is authored). It assembles a real OCI image - a gzipped
// layer plus config and manifest - pushes the blobs and manifest to the fixture
// registry through the narrow workload edge, and reads the manifest digest from
// the registry's Docker-Content-Digest. The action then holds on a workspace
// barrier, so the test owns the submission sequence deterministically.
//
// The test never trusts the digest the workload wrote down. It asks the
// registry for the tag the workload pushed, requires the two to agree, and
// submits the registry-observed digest as the declared output. The first
// submission reaches the server and commits, but its response is lost; the
// identical retry at the stale expected version must converge on the staged
// value and commit nothing. The workload is only released afterwards, and the
// relay's observed registry traffic proves exactly one manifest was published.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/test/support/factory"
	support "github.com/optimiware/kodama/test/support/infrastructure/messaging"
)

// deliveryWorkloadEvidence is the publication record the workload writes after
// it has built and pushed the image. It is the workload's claim; the test
// compares it against the registry's own answer and never uses it as truth.
type deliveryWorkloadEvidence struct {
	Digest       string `json:"digest"`
	Reference    string `json:"reference"`
	Marker       string `json:"marker"`
	BuildCount   int    `json:"build_count"`
	PublishCount int    `json:"publish_count"`
	LayerDigest  string `json:"layer_digest"`
	ConfigDigest string `json:"config_digest"`
}

// TestDeliveryWorkloadDigestConverges proves the
// delivery build/publish contract on the real stack.
func TestDeliveryWorkloadDigestConverges(t *testing.T) {
	harness := factory.Start(t)
	registry := support.StartRegistry(t, false)
	ctx := registry.Shard.Context()

	// The publisher image is the committed deterministic fixture base. Resolve
	// its tag through the real image store and require the committed pin, so the
	// admitted action image is a runtime-resolved digest and not a literal.
	baseDigest := registry.Shard.RequireImage(ctx, support.FixtureBaseTag)
	if baseDigest != support.FixtureBaseDigest {
		t.Fatalf("publisher base %s resolved to %s; the committed pin is %s",
			support.FixtureBaseTag, baseDigest, support.FixtureBaseDigest)
	}
	base, err := support.NewFixtureBase(support.FixtureBaseTag, baseDigest)
	if err != nil {
		t.Fatalf("resolve the publisher base reference: %v", err)
	}

	// Observe the exact bytes the workload sends to the registry. This is the
	// registry-side proof that the retry below did not publish again.
	var observed struct {
		sync.Mutex
		data []byte
	}
	edgePort := deliveryRegistryEdgeObserved(t, registry, func(chunk []byte) {
		observed.Lock()
		observed.data = append(observed.data, chunk...)
		observed.Unlock()
	})

	prefix := deliveryUniqueName(t)
	repository := "kodama/delivery-workload-" + prefix
	tag := "build-1"
	marker := "kodama-" + prefix
	const (
		evidencePath = "/workspace/delivery-evidence.json"
		releasePath  = "/workspace/release"
	)

	applied := deliveryApply(t, harness, "delivery-workload", deliveryPipelineDoc{
		Env: map[string]string{
			"DELIVERY_REGISTRY":       net.JoinHostPort("host.containers.internal", strconv.Itoa(edgePort)),
			"DELIVERY_REPOSITORY":     repository,
			"DELIVERY_TAG":            tag,
			"DELIVERY_MARKER":         marker,
			"DELIVERY_REFERENCE_HOST": registry.Host(),
			"DELIVERY_EVIDENCE":       evidencePath,
		},
		Actions: []deliveryActionDoc{{
			Name:    "build",
			Image:   base.Reference,
			Timeout: deliveryActionTimeout,
			Outputs: []string{"image"},
			Commands: []string{
				`test ! -e /kodama/task.json`,
				`test -z "${KODAMA_TASK:-}"`,
				"cat > /workspace/delivery-publish.py <<'KODAMA_DELIVERY_PUBLISH'\n" +
					deliveryWorkloadPublishScript + "\nKODAMA_DELIVERY_PUBLISH",
				`python3 /workspace/delivery-publish.py`,
				// Hold the action open until the test has driven the submission
				// sequence. The workload has already built and published once.
				`while [ ! -f /workspace/release ]; do sleep 1; done`,
				`test "$(cat /workspace/release)" = go`,
			},
		}},
	})
	started := deliveryStart(t, harness, applied.PipelineID, nil, "start-delivery-workload")
	running := deliveryWaitActionRunningStable(t, harness, started.ExecutionID, "build")
	credential := deliveryExecutionCredential(t, harness, started.ExecutionID, "build")
	action := actionByName(t, running, "build")

	evidence := deliveryWaitWorkloadEvidence(t, harness, started.ExecutionID, evidencePath)
	if evidence.BuildCount != 1 || evidence.PublishCount != 1 {
		t.Fatalf("workload evidence build/publish counts = %d/%d; want 1/1",
			evidence.BuildCount, evidence.PublishCount)
	}
	if evidence.Marker != marker {
		t.Fatalf("workload published marker %q; want %q", evidence.Marker, marker)
	}

	// The registry, not the workload's evidence file, owns the digest. Read the
	// tag back from the real registry and require the workload's claim to match.
	registryDigest, found := deliveryManifestDigest(registry.BaseURL, repository, tag)
	if !found {
		t.Fatalf("registry does not serve the tag %s:%s the workload published", repository, tag)
	}
	if registryDigest != evidence.Digest {
		t.Fatalf("workload claims digest %q; the registry serves %q for %s:%s",
			evidence.Digest, registryDigest, repository, tag)
	}
	pinned := registry.Host() + "/" + repository + "@" + registryDigest
	if evidence.Reference != pinned {
		t.Fatalf("workload claims reference %q; the registry-observed reference is %q",
			evidence.Reference, pinned)
	}

	// The registry really stores the image the workload built: the config blob
	// carries the unique marker and the manifest/config digests agree.
	deliveryRequireRegistryConfigLabel(t, registry.BaseURL, repository, registryDigest, marker)

	before := inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	lostErr := deliveryLoseOutputSubmission(t, harness, credential, started.ExecutionID, action.ActionID,
		map[string]string{"image": pinned}, before.Version, "delivery-workload-lost")
	if lostErr == nil {
		t.Fatal("the lost-response submission reported a response; the loss was not simulated")
	}
	committed := inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	if committed.Version != before.Version+1 {
		t.Fatalf("lost response left the execution at version %d; want exactly one committed staging at %d",
			committed.Version, before.Version+1)
	}

	// Retry the identical value with a fresh idempotency key at the stale
	// expected version. It must converge on the committed staging, not conflict
	// and not commit a second time.
	retriedResult, publicError := harness.REST(credential, "execution.action.outputs", api.ExecutionActionOutputsRequest{
		ExecutionID: started.ExecutionID,
		ActionID:    action.ActionID,
		Outputs:     map[string]string{"image": pinned},
	}, &before.Version, "delivery-workload-retry")
	if publicError.Code != "" {
		t.Fatalf("identical retry after a lost response refused: %s (%s)", publicError.Code, publicError.Message)
	}
	var retried api.ExecutionActionOutputsResult
	harness.Decode(retriedResult, &retried)
	if retried.ExecutionVersion != committed.Version {
		t.Fatalf("retry result version = %d; want the committed version %d",
			retried.ExecutionVersion, committed.Version)
	}
	afterRetry := inspectExecution(t, harness, harness.ProjectToken, started.ExecutionID)
	if afterRetry.Version != committed.Version {
		t.Fatalf("identical retry advanced the execution from %d to %d; want no second commit",
			committed.Version, afterRetry.Version)
	}

	// Release the action; settlement promotes the staged registry digest.
	deliveryReleaseWorkload(t, harness, started.ExecutionID)
	execution := deliveryRequireSucceeded(t, harness, started.ExecutionID)
	if len(execution.Actions) != 1 {
		t.Fatalf("execution has %d actions; want the single build action", len(execution.Actions))
	}
	build := actionByName(t, execution, "build")
	if build.State != "succeeded" {
		t.Fatalf("build action state = %q; want succeeded", build.State)
	}
	if build.Outputs["image"] != pinned {
		t.Fatalf("settled output = %q; want the registry-observed digest %q",
			build.Outputs["image"], pinned)
	}
	if build.ActivatedAt == nil {
		t.Fatal("build action has no activation time")
	}

	// Two committed action.transition events remain for the one action: the
	// staging commit the lost response produced and the settlement transition.
	// The identical retry added none; the version checks above pinned exactly
	// one staging commit.
	if count := deliveryCountActionTransitions(t, harness, build.ActionID); count != 2 {
		t.Fatalf("build action %s has %d action.transition events; want 2 (one staging, settled)",
			build.ActionID, count)
	}

	// The registry still serves exactly the recorded digest, and the tag did not
	// move: nothing republished after the lost response.
	deliveryRequireRegistryDigest(t, registry, deliveryPublished{
		Host:       registry.Host(),
		Repository: repository,
		Tag:        tag,
		Digest:     registryDigest,
		Pinned:     pinned,
	})
	tagDigest, found := deliveryManifestDigest(registry.BaseURL, repository, tag)
	if !found || tagDigest != registryDigest {
		t.Fatalf("registry tag %s:%s = %q (found=%t); want the published digest %q unchanged",
			repository, tag, tagDigest, found, registryDigest)
	}

	// The relay observed exactly one manifest PUT for this repository. A rebuild
	// or republish after the lost response would have made this two.
	observed.Lock()
	sent := append([]byte(nil), observed.data...)
	observed.Unlock()
	manifestPuts := bytes.Count(sent, []byte("PUT /v2/"+repository+"/manifests/"))
	if manifestPuts != 1 {
		t.Fatalf("registry relay observed %d manifest PUTs for %s; want exactly one publication",
			manifestPuts, repository)
	}

	deliveryAssertNoArtifactEntity(t)
}

// deliveryWaitWorkloadEvidence waits for the workload's publication record and
// decodes it. A partially written file is retried like an absent one.
func deliveryWaitWorkloadEvidence(t *testing.T, harness *factory.Harness, executionID, path string) deliveryWorkloadEvidence {
	t.Helper()
	var container string
	harness.Eventually(t, 60*time.Second, func() bool {
		name, err := harness.RunningExecutionContainer(t.Context(), executionID)
		if err != nil || name == "" {
			return false
		}
		container = name
		return true
	})
	var evidence deliveryWorkloadEvidence
	harness.Eventually(t, 2*time.Minute, func() bool {
		output, err := harness.ReadContainerFile(t.Context(), container, path)
		if err != nil {
			return false
		}
		var parsed deliveryWorkloadEvidence
		if json.Unmarshal([]byte(output), &parsed) != nil {
			return false
		}
		evidence = parsed
		return true
	})
	return evidence
}

// deliveryLoseOutputSubmission sends one real output submission whose response
// is discarded after the server has answered. The underlying round trip is a
// genuine request to the served edge; only the client-visible answer is lost.
func deliveryLoseOutputSubmission(t *testing.T, harness *factory.Harness, credential, executionID, actionID string, outputs map[string]string, expected uint64, key string) error {
	t.Helper()
	input, err := json.Marshal(api.ExecutionActionOutputsRequest{
		ExecutionID: executionID,
		ActionID:    actionID,
		Outputs:     outputs,
	})
	if err != nil {
		t.Fatalf("encode the lost output submission: %v", err)
	}
	transport := harness.Client(credential)
	transport.RoundTripper = deliveryLostResponse{inner: transport.RoundTripper}
	_, err = transport.Invoke(context.Background(), api.Invocation{
		Operation: "execution.action.outputs",
		Request: api.Request{
			Input:           input,
			ExpectedVersion: &expected,
			IdempotencyKey:  key,
		},
	})
	return err
}

// deliveryLostResponse performs the real round trip and then drops the response
// unread. The server has already processed and committed the request; the
// caller only observes a transport failure, exactly like a lost response.
type deliveryLostResponse struct {
	inner http.RoundTripper
}

func (lost deliveryLostResponse) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := lost.inner.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	_ = response.Body.Close()
	return nil, errors.New("delivery: output response lost after the server answered")
}

// deliveryRequireRegistryConfigLabel fetches the manifest and config blobs from
// the real registry and requires the published config to carry the unique
// marker. This proves the registry stores the image the workload built rather
// than a manifest the test synthesized.
func deliveryRequireRegistryConfigLabel(t *testing.T, baseURL, repository, digest, marker string) {
	t.Helper()
	manifestBytes := deliveryRegistryGet(t, baseURL+"/v2/"+repository+"/manifests/"+digest,
		support.OCIManifestMediaType)
	var manifest struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatalf("decode the registry manifest %s: %v", digest, err)
	}
	if !strings.HasPrefix(manifest.Config.Digest, "sha256:") {
		t.Fatalf("registry manifest %s has no config digest: %s", digest, string(manifestBytes))
	}
	configBytes := deliveryRegistryGet(t, baseURL+"/v2/"+repository+"/blobs/"+manifest.Config.Digest, "")
	var config struct {
		Config struct {
			Labels map[string]string `json:"labels"`
		} `json:"config"`
	}
	if err := json.Unmarshal(configBytes, &config); err != nil {
		t.Fatalf("decode the registry config blob %s: %v", manifest.Config.Digest, err)
	}
	if got := config.Config.Labels["kodama.test/delivery"]; got != marker {
		t.Fatalf("registry config label kodama.test/delivery = %q; want the workload marker %q", got, marker)
	}
}

// deliveryRegistryGet reads one registry resource as raw bytes. A non-200 is a
// test failure: the caller only asks for bytes the registry must serve.
func deliveryRegistryGet(t *testing.T, url, accept string) []byte {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build registry request %s: %v", url, err)
	}
	if accept != "" {
		request.Header.Set("Accept", accept)
	}
	response, err := (&http.Client{Timeout: 20 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("registry request %s: %v", url, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		t.Fatalf("read registry response %s: %v", url, err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("registry %s returned %d: %s", url, response.StatusCode, string(body))
	}
	return body
}

// deliveryWorkloadPublishScript is the workload-side build and publication. It
// assembles a real OCI image from a unique marker, pushes the blobs and the
// manifest to the registry edge, and records the digest the registry reported.
// It never invents a digest and never trusts its own tag resolution.
const deliveryWorkloadPublishScript = `import gzip
import hashlib
import http.client
import io
import json
import os
import tarfile

REGISTRY = os.environ["DELIVERY_REGISTRY"]
REPOSITORY = os.environ["DELIVERY_REPOSITORY"]
TAG = os.environ["DELIVERY_TAG"]
MARKER = os.environ["DELIVERY_MARKER"]
REFERENCE_HOST = os.environ["DELIVERY_REFERENCE_HOST"]
EVIDENCE = os.environ["DELIVERY_EVIDENCE"]


def digest(data):
    return "sha256:" + hashlib.sha256(data).hexdigest()


layer_tar = io.BytesIO()
info = tarfile.TarInfo("kodama-delivery-marker")
body = (MARKER + "\n").encode()
info.size = len(body)
info.mode = 0o644
info.mtime = 0
with tarfile.open(fileobj=layer_tar, mode="w") as archive:
    archive.addfile(info, io.BytesIO(body))
layer_plain = layer_tar.getvalue()
layer = gzip.compress(layer_plain, mtime=0)
config = json.dumps({
    "architecture": "amd64",
    "os": "linux",
    "config": {"Labels": {"kodama.test/delivery": MARKER}},
    "rootfs": {"type": "layers", "diff_ids": [digest(layer_plain)]},
}, separators=(",", ":"), sort_keys=True).encode()
manifest = json.dumps({
    "schemaVersion": 2,
    "mediaType": "application/vnd.oci.image.manifest.v1+json",
    "config": {
        "mediaType": "application/vnd.oci.image.config.v1+json",
        "digest": digest(config),
        "size": len(config),
    },
    "layers": [{
        "mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
        "digest": digest(layer),
        "size": len(layer),
    }],
}, separators=(",", ":"), sort_keys=True).encode()

host, port = REGISTRY.rsplit(":", 1)


def send(method, path, body=None, headers=None):
    connection = http.client.HTTPConnection(host, int(port), timeout=30)
    connection.request(method, path, body=body, headers=headers or {})
    response = connection.getresponse()
    payload = response.read()
    result = response.status, dict(response.getheaders()), payload
    connection.close()
    return result


def upload(blob):
    status, headers, _ = send("POST", "/v2/%s/blobs/uploads/" % REPOSITORY)
    if status != 202:
        raise SystemExit("blob upload start returned %d" % status)
    location = headers.get("Location") or headers.get("location")
    if not location:
        raise SystemExit("blob upload start returned no location")
    separator = "&" if "?" in location else "?"
    status, _, payload = send(
        "PUT", location + separator + "digest=" + digest(blob), body=blob,
        headers={"Content-Type": "application/octet-stream"})
    if status != 201:
        raise SystemExit("blob upload returned %d: %s" % (status, payload))


for blob in (config, layer):
    upload(blob)

status, headers, payload = send(
    "PUT", "/v2/%s/manifests/%s" % (REPOSITORY, TAG), body=manifest,
    headers={"Content-Type": "application/vnd.oci.image.manifest.v1+json"})
if status != 201:
    raise SystemExit("manifest publish returned %d: %s" % (status, payload))
observed = (headers.get("Docker-Content-Digest") or
            headers.get("docker-content-digest") or "").strip()
if not observed.startswith("sha256:") or len(observed) != 71:
    raise SystemExit("registry returned no manifest digest")

status, _, stored = send(
    "GET", "/v2/%s/manifests/%s" % (REPOSITORY, observed),
    headers={"Accept": "application/vnd.oci.image.manifest.v1+json"})
if status != 200:
    raise SystemExit("manifest read back returned %d" % status)
if digest(stored) != observed:
    raise SystemExit("registry manifest bytes do not hash to the reported digest")

evidence = {
    "digest": observed,
    "reference": REFERENCE_HOST + "/" + REPOSITORY + "@" + observed,
    "marker": MARKER,
    "build_count": 1,
    "publish_count": 1,
    "layer_digest": digest(layer),
    "config_digest": digest(config),
}
with open(EVIDENCE, "w") as handle:
    json.dump(evidence, handle, sort_keys=True)
print("published " + evidence["reference"])
`
