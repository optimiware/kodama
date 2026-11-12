//go:build integration

package services

// Shared fixtures for the delivery-recipe scenarios.
//
// The recipes are ordinary pipelines: container actions use the runtime fixture
// image, declared outputs cross the served execution.action.outputs operation
// with the workload's own mounted credential, and requests cross the served
// execution.request.resolve operation as a real reviewer. The artifact is only
// an output string; the bytes and their immutability are owned by the real
// registry fixture (test/support/infrastructure/messaging/registry.go). There
// is no artifact table, registration operation or catalog anywhere on this
// path.
//
// Publication is performed by test-side image tooling: every scenario builds its
// own disposable image from the pinned workload base, and only that per-test
// image is tagged and pushed to the fixture registry. The build action itself
// never touches the registry; it only records the digest-pinned reference the
// test tool published. The shared workload image is read (FROM/inspect) and is
// never tagged, pushed or otherwise mutated. The fixture image has no
// container-image tool. Only the protected deploy scenarios in
// delivery_deploy_test.go compose the git-capable fixture image and the
// factory's Git service (factory.WithGitService). Every digest the
// recipes record is resolved back through the real registry and, where the
// recipe runs it, through the real Podman image store. Nothing here fabricates
// a registry success.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/test/support/factory"
	support "github.com/optimiware/kodama/test/support/infrastructure/messaging"
	runtimesupport "github.com/optimiware/kodama/test/support/infrastructure/runtime"
)

// deliveryActionTimeout keeps every recipe action clear of the transaction clock
// for the duration of one scenario.
const deliveryActionTimeout = "1h"

// deliveryPipelineDoc is one API-posted delivery pipeline. It deliberately
// carries no repository, service or on.push binding: these recipes are plain
// task-free pipelines and must run with the optional services disabled.
type deliveryPipelineDoc struct {
	Inputs  []string            `yaml:"inputs,omitempty"`
	Env     map[string]string   `yaml:"env,omitempty"`
	Actions []deliveryActionDoc `yaml:"actions"`
}

// deliveryActionDoc is one container or request action of a delivery pipeline.
type deliveryActionDoc struct {
	Name     string         `yaml:"name"`
	Image    string         `yaml:"image,omitempty"`
	Commands []string       `yaml:"commands,omitempty"`
	Request  string         `yaml:"request,omitempty"`
	Subject  map[string]any `yaml:"subject,omitempty"`
	Outputs  []string       `yaml:"outputs,omitempty"`
	Timeout  string         `yaml:"timeout,omitempty"`
}

// deliveryMarshal renders one exact pipeline YAML byte sequence.
func deliveryMarshal(t *testing.T, document deliveryPipelineDoc) []byte {
	t.Helper()
	encoded, err := yaml.Marshal(document)
	if err != nil {
		t.Fatalf("marshal delivery pipeline yaml: %v", err)
	}
	return encoded
}

// deliveryApply posts one delivery pipeline through the public API.
func deliveryApply(t *testing.T, harness *factory.Harness, name string, document deliveryPipelineDoc) api.PipelineApplyResult {
	t.Helper()
	result := harness.MustREST(harness.ProjectToken, "pipeline.apply", api.PipelineApplyRequest{
		ProjectID: factory.ProjectID,
		Name:      name,
		YAML:      deliveryMarshal(t, document),
	}, nil, "apply-"+name)
	var applied api.PipelineApplyResult
	harness.Decode(result, &applied)
	return applied
}

// deliveryStart starts one stored delivery pipeline with its admitted inputs at
// the currently observed stored version.
func deliveryStart(t *testing.T, harness *factory.Harness, pipelineID string, inputs map[string]string, key string) api.PipelineStartResult {
	t.Helper()
	stored := readPipeline(t, harness, pipelineID)
	if stored.Version == 0 {
		t.Fatalf("pipeline.read %s version = 0", pipelineID)
	}
	result := harness.MustREST(harness.ProjectToken, "pipeline.start", api.PipelineStartRequest{
		ProjectID:  factory.ProjectID,
		PipelineID: pipelineID,
		Inputs:     inputs,
	}, &stored.Version, key)
	var started api.PipelineStartResult
	harness.Decode(result, &started)
	return started
}

// deliveryWaitActionRunning waits for one named action to be the running action
// of a running execution and returns that observation.
func deliveryWaitActionRunning(t *testing.T, harness *factory.Harness, executionID, actionName string) api.ExecutionInspectResult {
	t.Helper()
	var last api.ExecutionInspectResult
	defer func() {
		if t.Failed() {
			t.Logf("action %q never became the running action of execution %s; last inspect: state=%q version=%d failure=%+v actions=%+v",
				actionName, executionID, last.State, last.Version, last.Failure, last.Actions)
			deliveryDumpLogs(t, harness, last)
		}
	}()
	harness.Eventually(t, 2*time.Minute, func() bool {
		result, publicError := harness.REST(harness.ProjectToken, "execution.inspect", api.ExecutionInspectRequest{
			ExecutionID: executionID,
		}, nil, "")
		if publicError.Code != "" {
			return false
		}
		harness.Decode(result, &last)
		if last.State != "running" {
			return false
		}
		for _, action := range last.Actions {
			if action.Name == actionName {
				return action.State == "running"
			}
		}
		return false
	})
	return last
}

// deliveryWaitActionRunningStable waits for one named action to be running, for
// its first runtime observation to be persisted and for the served execution
// version to stop advancing, so a submission made at the returned version
// cannot race a late activation, credential or observation commit. The first
// Running observation is the engine's last commit until the workload exits, and
// it lands only after the container start returned: an unchanged version alone
// is also seen for the whole duration of that start. It is a barrier, never a
// fixed delay.
func deliveryWaitActionRunningStable(t *testing.T, harness *factory.Harness, executionID, actionName string) api.ExecutionInspectResult {
	t.Helper()
	var last api.ExecutionInspectResult
	var lastVersion uint64
	haveBaseline := false
	defer func() {
		if t.Failed() {
			t.Logf("execution %s never reached a stable running action %q; last inspect: state=%q version=%d failure=%+v actions=%+v",
				executionID, actionName, last.State, last.Version, last.Failure, last.Actions)
		}
	}()
	harness.Eventually(t, 2*time.Minute, func() bool {
		result, publicError := harness.REST(harness.ProjectToken, "execution.inspect", api.ExecutionInspectRequest{
			ExecutionID: executionID,
		}, nil, "")
		if publicError.Code != "" {
			haveBaseline = false
			return false
		}
		var current api.ExecutionInspectResult
		harness.Decode(result, &current)
		if current.State != "running" {
			haveBaseline = false
			last = current
			return false
		}
		running := false
		for _, action := range current.Actions {
			if action.Name == actionName && action.State == "running" {
				running = true
			}
		}
		if !running {
			haveBaseline = false
			last = current
			return false
		}
		if row, err := harness.ActionRow(t.Context(), executionID, actionName); err != nil || !row.ObservationSet {
			haveBaseline = false
			last = current
			return false
		}
		if haveBaseline && current.Version == lastVersion {
			last = current
			return true
		}
		haveBaseline = true
		lastVersion = current.Version
		last = current
		return false
	})
	return last
}

// deliveryExecutionCredential waits for the mounted execution credential of one
// running container action and returns it. The value is never logged.
func deliveryExecutionCredential(t *testing.T, harness *factory.Harness, executionID, actionName string) string {
	t.Helper()
	var credential string
	harness.Eventually(t, 30*time.Second, func() bool {
		value, err := harness.ExecutionCredential(t.Context(), executionID, actionName)
		if err != nil || value == "" {
			return false
		}
		credential = value
		return true
	})
	return credential
}

// deliveryStageOutputs submits container outputs through the served operation
// with the workload credential, re-reading the execution version and retrying a
// version conflict exactly like the workload-side submission script. It returns
// the committed result and the execution version the successful attempt was
// submitted against, so a caller can prove whether the submission added a
// commit.
func deliveryStageOutputs(t *testing.T, harness *factory.Harness, credential, executionID, actionID string, outputs map[string]string, key string) (api.ExecutionActionOutputsResult, uint64) {
	t.Helper()
	var last api.Error
	for attempt := 1; attempt <= 20; attempt++ {
		current := inspectExecution(t, harness, harness.ProjectToken, executionID)
		result, publicError := harness.REST(credential, "execution.action.outputs", api.ExecutionActionOutputsRequest{
			ExecutionID: executionID,
			ActionID:    actionID,
			Outputs:     outputs,
		}, &current.Version, fmt.Sprintf("%s-%d", key, attempt))
		if publicError.Code == "" {
			var staged api.ExecutionActionOutputsResult
			harness.Decode(result, &staged)
			return staged, current.Version
		}
		if publicError.Code != api.CodeVersionConflict {
			t.Fatalf("execution.action.outputs %s/%s refused: %s (%s)",
				executionID, actionID, publicError.Code, publicError.Message)
		}
		last = publicError
	}
	t.Fatalf("execution.action.outputs %s/%s did not converge: %s (%s)",
		executionID, actionID, last.Code, last.Message)
	return api.ExecutionActionOutputsResult{}, 0
}

// deliveryRejectOutputs submits a deliberately changed output value and returns
// the public refusal, retrying only a version conflict so the refusal under test
// is the content refusal.
func deliveryRejectOutputs(t *testing.T, harness *factory.Harness, credential, executionID, actionID string, outputs map[string]string, key string) api.Error {
	t.Helper()
	var last api.Error
	for attempt := 1; attempt <= 20; attempt++ {
		current := inspectExecution(t, harness, harness.ProjectToken, executionID)
		_, publicError := harness.REST(credential, "execution.action.outputs", api.ExecutionActionOutputsRequest{
			ExecutionID: executionID,
			ActionID:    actionID,
			Outputs:     outputs,
		}, &current.Version, fmt.Sprintf("%s-%d", key, attempt))
		if publicError.Code == "" {
			t.Fatalf("changed output submission %s/%s was accepted; want refusal", executionID, actionID)
		}
		if publicError.Code != api.CodeVersionConflict {
			return publicError
		}
		last = publicError
	}
	t.Fatalf("changed output submission %s/%s did not reach a content decision: %s (%s)",
		executionID, actionID, last.Code, last.Message)
	return api.Error{}
}

// deliveryResolveRequest answers one outstanding request through the served
// resolve operation. It re-reads the served execution immediately before every
// attempt, so a background commit between inspection and submission is retried
// at the fresh version instead of turning into a flaky failure. A repeated
// identical answer converges.
func deliveryResolveRequest(t *testing.T, harness *factory.Harness, token string, waiting api.ExecutionInspectResult, actionName, decision string, outputs map[string]string, key string) api.ExecutionRequestResolveResult {
	t.Helper()
	var last api.Error
	for attempt := 1; attempt <= 20; attempt++ {
		current := inspectExecution(t, harness, harness.ProjectToken, waiting.ExecutionID)
		action := actionByName(t, current, actionName)
		result, publicError := harness.REST(token, "execution.request.resolve", api.ExecutionRequestResolveRequest{
			ExecutionID:           current.ExecutionID,
			ActionID:              action.ActionID,
			ExpectedActionVersion: action.Version,
			Decision:              decision,
			Outputs:               outputs,
		}, &current.Version, fmt.Sprintf("%s-%d", key, attempt))
		if publicError.Code == "" {
			var resolved api.ExecutionRequestResolveResult
			harness.Decode(result, &resolved)
			return resolved
		}
		if publicError.Code != api.CodeVersionConflict {
			t.Fatalf("execution.request.resolve %s/%s refused: %s (%s)",
				waiting.ExecutionID, actionName, publicError.Code, publicError.Message)
		}
		last = publicError
	}
	t.Fatalf("execution.request.resolve %s/%s did not converge: %s (%s)",
		waiting.ExecutionID, actionName, last.Code, last.Message)
	return api.ExecutionRequestResolveResult{}
}

// deliveryRequireSucceeded asserts one settled execution succeeded and returns
// it.
func deliveryRequireSucceeded(t *testing.T, harness *factory.Harness, executionID string) api.ExecutionInspectResult {
	t.Helper()
	execution := waitExecutionSettled(t, harness, executionID)
	if execution.State != "succeeded" {
		t.Logf("execution %s state = %q; want succeeded (failure=%+v actions=%+v)",
			executionID, execution.State, execution.Failure, execution.Actions)
		deliveryDumpLogs(t, harness, execution)
		t.FailNow()
	}
	return execution
}

// deliveryRequireFailed asserts one settled execution failed and returns it.
func deliveryRequireFailed(t *testing.T, harness *factory.Harness, executionID string) api.ExecutionInspectResult {
	t.Helper()
	execution := waitExecutionSettled(t, harness, executionID)
	if execution.State != "failed" {
		t.Logf("execution %s state = %q; want failed (failure=%+v actions=%+v)",
			executionID, execution.State, execution.Failure, execution.Actions)
		deliveryDumpLogs(t, harness, execution)
		t.FailNow()
	}
	return execution
}

// deliveryPublished is one exact digest published through the real registry.
type deliveryPublished struct {
	Host       string
	Repository string
	Tag        string
	Digest     string
	// Pinned is the digest-pinned reference an admitted container action uses.
	Pinned string

	// LocalImage is the unique local build name, Target the registry tag and
	// Names every exact local name this test created. Cleanup removes exactly
	// these names and reports any that survive.
	LocalImage string
	Target     string
	Names      []string
	// SourceImageID is the local image ID of the per-test image. It must differ
	// from the shared base image ID.
	SourceImageID string
	// Marker is the unique file content baked into the built image, so a
	// scenario can prove it ran these bytes and not the shared base.
	Marker string
	// Prefix is the per-test name component shared by the local build tag and
	// the registry repository. The hygiene proof lists local images containing
	// it after cleanup.
	Prefix string
}

// deliveryUniqueName returns a random component that keeps every name one test
// creates disjoint from concurrent lanes and from other tests in this package.
func deliveryUniqueName(t *testing.T) string {
	t.Helper()
	var random [6]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatalf("read randomness for a unique delivery image name: %v", err)
	}
	return "delivery-" + hex.EncodeToString(random[:])
}

// deliveryBuildImage builds one disposable image FROM the pinned workload base
// with a unique label and a unique marker file. The build only reads the shared
// base; it never tags or pushes it. The returned image ID is asserted to differ
// from the base image ID, so a scenario cannot accidentally publish the shared
// image under a new name.
func deliveryBuildImage(t *testing.T, ctx context.Context, base runtimesupport.PinnedImage, prefix string) (localTag, imageID, marker string) {
	t.Helper()
	marker = "kodama-" + prefix
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "marker"), []byte(marker), 0o644); err != nil {
		t.Fatalf("write per-test delivery image marker: %v", err)
	}
	containerfile := "FROM " + base.Reference + "\n" +
		"LABEL kodama.test/delivery=\"" + prefix + "\"\n" +
		"COPY marker /kodama-delivery-marker\n"
	if err := os.WriteFile(filepath.Join(directory, "Containerfile"), []byte(containerfile), 0o644); err != nil {
		t.Fatalf("write per-test delivery Containerfile: %v", err)
	}
	localTag = "localhost/" + prefix + ":source"
	result, err := runtimesupport.RunPodman(ctx, "build",
		"--pull=never", "--network=none", "--no-cache", "--rm",
		"--format", "oci", "-t", localTag,
		"-f", filepath.Join(directory, "Containerfile"), directory)
	if err != nil {
		t.Fatalf("build a per-test delivery image from %s: %v (%s)",
			base.Reference, err, strings.TrimSpace(string(result.Stderr)))
	}
	imageID = deliveryImageField(t, ctx, localTag, "{{.Id}}")
	baseID := deliveryImageField(t, ctx, base.Tag, "{{.Id}}")
	if imageID == baseID {
		t.Fatalf("per-test delivery image %s has the shared base image ID %s; the build did not create a distinct image",
			localTag, baseID)
	}
	return localTag, imageID, marker
}

// deliveryImageField reads one field of one local image through the Podman CLI.
func deliveryImageField(t *testing.T, ctx context.Context, reference, format string) string {
	t.Helper()
	result, err := runtimesupport.RunPodman(ctx, "image", "inspect", "--format", format, reference)
	if err != nil {
		t.Fatalf("inspect image %s: %v (%s)", reference, err, strings.TrimSpace(string(result.Stderr)))
	}
	value := strings.TrimSpace(string(result.Stdout))
	if value == "" {
		t.Fatalf("image %s inspect field %q is empty", reference, format)
	}
	return value
}

// deliveryPublishImage builds one disposable per-test image, tags and pushes only
// that image to the local registry, then pulls the exact digest back into the
// Podman store so a container action can run those exact bytes. The shared base
// image is only read through the Containerfile's FROM. The returned digest is
// read from the registry's manifest response, not assumed from local state.
func deliveryPublishImage(t *testing.T, registry *support.Registry, base runtimesupport.PinnedImage, repository, tag string) deliveryPublished {
	t.Helper()
	ctx := registry.Shard.Context()
	prefix := deliveryUniqueName(t)
	host := registry.Host()
	published := deliveryPublished{
		Host:       host,
		Repository: repository + "-" + prefix,
		Tag:        tag,
		Prefix:     prefix,
	}
	published.Target = host + "/" + published.Repository + ":" + tag
	// Register cleanup before the first mutating command: a failed build, tag,
	// push or pull must not leave a name behind either.
	t.Cleanup(func() { deliveryCleanupPublished(t, published) })

	published.LocalImage, published.SourceImageID, published.Marker =
		deliveryBuildImage(t, ctx, base, prefix)
	published.Names = []string{published.Target, published.LocalImage}
	if _, err := runtimesupport.RunPodman(ctx, "tag", published.LocalImage, published.Target); err != nil {
		t.Fatalf("tag the per-test delivery image %s as %s: %v", published.LocalImage, published.Target, err)
	}
	if _, err := runtimesupport.RunPodman(ctx, "push", "--tls-verify=false", "--quiet", published.Target); err != nil {
		t.Fatalf("push the per-test delivery image %s to the local registry: %v", published.Target, err)
	}
	digest, found := deliveryManifestDigest(registry.BaseURL, published.Repository, tag)
	if !found {
		t.Fatalf("registry does not serve the manifest just pushed as %s:%s", published.Repository, tag)
	}
	published.Digest = digest
	published.Pinned = host + "/" + published.Repository + "@" + digest
	published.Names = []string{published.Target, published.Pinned, published.LocalImage}
	if _, err := runtimesupport.RunPodman(ctx, "pull", "--tls-verify=false", "--quiet", published.Pinned); err != nil {
		t.Fatalf("pull the exact published digest %s from the local registry: %v", published.Pinned, err)
	}
	return published
}

// deliveryCleanupPublished removes every exact local name this test created and
// then proves none matching its unique prefix survives. It is idempotent: an
// explicit hygiene check may run it before the registered t.Cleanup does. A
// removal failure or a surviving name is reported with t.Errorf naming it, so a
// passing run never leaks an image. Shared base names are never in the list.
func deliveryCleanupPublished(t *testing.T, published deliveryPublished) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, name := range published.Names {
		if strings.TrimSpace(name) == "" {
			continue
		}
		result, err := runtimesupport.RunPodman(ctx, deliveryRemoveImageArgs(name)...)
		if err == nil {
			continue
		}
		detail := strings.ToLower(err.Error() + " " + string(result.Stderr) + " " + string(result.Stdout))
		if strings.Contains(detail, "no such image") || strings.Contains(detail, "image not known") || strings.Contains(detail, "not found") {
			continue
		}
		t.Errorf("delivery image cleanup: remove %s: %v", name, err)
	}
	if published.Prefix == "" {
		return
	}
	leftovers, err := deliveryLocalImageNames(ctx, published.Prefix)
	if err != nil {
		t.Errorf("delivery image cleanup: list local images with prefix %q: %v", published.Prefix, err)
		return
	}
	for _, name := range leftovers {
		t.Errorf("delivery image cleanup left the name %s (prefix %q) in the local Podman store", name, published.Prefix)
	}
}

// deliveryRemoveImageArgs is the exact removal command cleanup issues. It is
// never forced: an image name this test owns is removed by name, and a name
// still in use fails loudly instead of deleting another lane's container or
// silently masking a product leak.
func deliveryRemoveImageArgs(name string) []string {
	return []string{"rmi", name}
}

// deliveryLocalImageNames lists every local image name containing prefix.
func deliveryLocalImageNames(ctx context.Context, prefix string) ([]string, error) {
	result, err := runtimesupport.RunPodman(ctx, "images", "--format", "{{.Repository}}:{{.Tag}}")
	if err != nil {
		return nil, err
	}
	var names []string
	for line := range strings.SplitSeq(string(result.Stdout), "\n") {
		name := strings.TrimSpace(line)
		if name != "" && strings.Contains(name, prefix) {
			names = append(names, name)
		}
	}
	return names, nil
}

// deliveryImageIdentity is the recorded local identity of one shared image. The
// hygiene proof compares these byte for byte before and after the publish path.
type deliveryImageIdentity struct {
	ID          string
	Digest      string
	RepoDigests string
	RepoTags    string
}

// deliveryInspectIdentity reads the ID, manifest digest, RepoDigests and
// RepoTags of one local image in a single Podman inspection.
func deliveryInspectIdentity(t *testing.T, ctx context.Context, reference string) deliveryImageIdentity {
	t.Helper()
	format := "{{.Id}}|{{.Digest}}|{{json .RepoDigests}}|{{json .RepoTags}}"
	result, err := runtimesupport.RunPodman(ctx, "image", "inspect", "--format", format, reference)
	if err != nil {
		t.Fatalf("inspect shared image %s: %v (%s)", reference, err, strings.TrimSpace(string(result.Stderr)))
	}
	parts := strings.SplitN(strings.TrimSpace(string(result.Stdout)), "|", 4)
	if len(parts) != 4 {
		t.Fatalf("inspect shared image %s returned %d fields; want id, digest, repo digests and repo tags",
			reference, len(parts))
	}
	return deliveryImageIdentity{ID: parts[0], Digest: parts[1], RepoDigests: parts[2], RepoTags: parts[3]}
}

// deliveryPublishManifest publishes one minimal OCI manifest through the real
// registry. It is used when a scenario only needs a real, immutable digest and
// not its runnable bytes.
func deliveryPublishManifest(t *testing.T, registry *support.Registry, repository, tag string) deliveryPublished {
	t.Helper()
	manifest, blobs := support.OCIManifest(
		[]byte(`{"architecture":"amd64","os":"linux","config":{"Env":["PATH=/usr/bin:/bin"]}}`),
		[]byte("delivery-layer-"+repository+"-"+tag),
	)
	digest := registry.Push(t, repository, tag, manifest, blobs)
	return deliveryPublished{
		Host:       registry.Host(),
		Repository: repository,
		Tag:        tag,
		Digest:     digest,
		Pinned:     registry.Host() + "/" + repository + "@" + digest,
	}
}

// deliveryManifestDigest reads the exact manifest digest the registry serves for
// one tag or digest reference. A non-200 response reports absence.
func deliveryManifestDigest(baseURL, repository, reference string) (string, bool) {
	request, err := http.NewRequest(http.MethodGet, baseURL+"/v2/"+repository+"/manifests/"+reference, nil)
	if err != nil {
		return "", false
	}
	request.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json")
	response, err := (&http.Client{Timeout: 20 * time.Second}).Do(request)
	if err != nil {
		return "", false
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusOK {
		return "", false
	}
	digest := strings.TrimSpace(response.Header.Get("Docker-Content-Digest"))
	if digest == "" {
		return "", false
	}
	return digest, true
}

// deliveryRequireRegistryDigest proves the registry still serves exactly the
// recorded digest and that it does not serve a different one. The absence probe
// is the negative control: an implementation that always reports presence would
// not pass it.
func deliveryRequireRegistryDigest(t *testing.T, registry *support.Registry, published deliveryPublished) {
	t.Helper()
	digest, found := deliveryManifestDigest(registry.BaseURL, published.Repository, published.Digest)
	if !found {
		t.Fatalf("registry does not serve the recorded digest %s for %s", published.Digest, published.Repository)
	}
	if digest != published.Digest {
		t.Fatalf("registry serves digest %q for reference %q; want %q", digest, published.Digest, published.Digest)
	}
	absent := "sha256:" + strings.Repeat("0", 64)
	if absent == published.Digest {
		t.Fatal("fixture digest equals the all-zero control digest")
	}
	if _, found := deliveryManifestDigest(registry.BaseURL, published.Repository, absent); found {
		t.Fatalf("registry reports an unpublished digest %s as present; the digest probe is not discriminating", absent)
	}
}

// deliveryRegistryEdge exposes the real registry to a workload. The runtime
// adapter joins Podman's pasta network, which cannot reach the registry
// fixture's isolated shard network or its loopback-only published port, so a
// small host relay forwards the workload's TCP connection to the registry. It
// relays bytes to the real fixture registry; it never synthesizes a response.
// The listener binds only the single host interface address pasta assigns to
// host.containers.internal, never the unspecified wildcard, and cleanup closes
// accepted connections and joins every owned goroutine.
// The returned address is the relay's workload-reachable port.
func deliveryRegistryEdge(t *testing.T, registry *support.Registry) int {
	t.Helper()
	return deliveryRegistryEdgeObserved(t, registry, nil)
}

// deliveryRegistryEdgeObserved is deliveryRegistryEdge with a raw client-to-
// registry byte observer. The observer sees exactly the bytes the workload
// sends to the real registry; it never sees or rewrites the registry's answer.
// A scenario uses it to count what the workload actually published without
// trusting a workload-authored counter. Calls may arrive concurrently from
// separate relayed connections, so an observer must synchronize its own state.
func deliveryRegistryEdgeObserved(t *testing.T, registry *support.Registry, observe func([]byte)) int {
	t.Helper()
	host := deliveryWorkloadHost(t)
	listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatalf("listen for the registry relay on the workload host %s: %v", host, err)
	}
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok || address.Port <= 0 {
		_ = listener.Close()
		t.Fatalf("registry relay address = %v; want a TCP port", listener.Addr())
	}

	var (
		work        sync.WaitGroup
		mu          sync.Mutex
		connections = make(map[net.Conn]struct{})
		stopping    bool
	)
	track := func(connection net.Conn) bool {
		mu.Lock()
		defer mu.Unlock()
		if stopping {
			_ = connection.Close()
			return false
		}
		connections[connection] = struct{}{}
		return true
	}
	untrack := func(connection net.Conn) {
		mu.Lock()
		delete(connections, connection)
		mu.Unlock()
	}

	work.Go(func() {
		for {
			inbound, err := listener.Accept()
			if err != nil {
				return
			}
			if !track(inbound) {
				continue
			}
			work.Add(1)
			go func(in net.Conn) {
				defer work.Done()
				defer untrack(in)
				defer in.Close()
				outbound, err := net.DialTimeout("tcp", registry.Host(), 5*time.Second)
				if err != nil {
					return
				}
				if !track(outbound) {
					return
				}
				defer untrack(outbound)
				defer outbound.Close()
				closeBoth := func() {
					_ = in.Close()
					_ = outbound.Close()
				}
				var copying sync.WaitGroup
				copying.Add(2)
				go func() {
					defer copying.Done()
					_, _ = io.Copy(outbound, deliveryObservedReader{reader: in, observe: observe})
					closeBoth()
				}()
				go func() {
					defer copying.Done()
					_, _ = io.Copy(in, outbound)
					closeBoth()
				}()
				copying.Wait()
			}(inbound)
		}
	})

	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		stopping = true
		for connection := range connections {
			_ = connection.Close()
		}
		mu.Unlock()
		work.Wait()
	})
	return address.Port
}

// deliveryObservedReader forwards every successful read to the observer before
// relaying it. A nil observer keeps the previous pass-through path.
type deliveryObservedReader struct {
	reader  io.Reader
	observe func([]byte)
}

func (reader deliveryObservedReader) Read(buffer []byte) (int, error) {
	read, err := reader.reader.Read(buffer)
	if read > 0 && reader.observe != nil {
		reader.observe(buffer[:read])
	}
	return read, err
}

// deliveryWorkloadHost resolves the single host interface address pasta assigns
// to host.containers.internal. It mirrors the factory's narrow workload bind:
// a connected UDP socket performs the default-route lookup without sending a
// packet, and the relay never widens to the unspecified wildcard.
func deliveryWorkloadHost(t *testing.T) string {
	t.Helper()
	connection, err := net.Dial("udp4", "1.1.1.1:53")
	if err != nil {
		t.Fatalf("resolve the host default-route address for the registry relay: %v", err)
	}
	defer func() { _ = connection.Close() }()
	address, ok := connection.LocalAddr().(*net.UDPAddr)
	if !ok || address.IP == nil || address.IP.IsUnspecified() || address.IP.IsLoopback() {
		t.Fatalf("default-route address %v is not a specific host interface address", connection.LocalAddr())
	}
	return address.IP.String()
}

// deliveryRegistryProbeCommand renders the bounded readiness command a workload
// runs against the real registry: it succeeds only when the exact manifest is
// served with the manifest media type a container runtime requests.
func deliveryRegistryProbeCommand(port int, repository, reference string) string {
	return fmt.Sprintf(
		`wget -q -O /dev/null -T 10 --header='Accept: application/vnd.oci.image.manifest.v1+json' "http://host.containers.internal:%d/v2/%s/manifests/%s"`,
		port, repository, reference)
}

// deliverySubmitTemplate is the workload-side output submission. It reads the
// mounted execution credential, inspects its own execution for the current
// version and action id, then stages the declared output with the public
// envelope. It retries a version conflict with a fresh idempotency key because
// execution.action.outputs is not idempotent across reused keys.
const deliverySubmitTemplate = `TOKEN=$(cat /kodama/secrets/KODAMA_TOKEN)
ENDPOINT=${KODAMA_URL#http://}
API_HOST=${ENDPOINT%:*}
API_PORT=${ENDPOINT##*:}
api() {
  method=$1
  path=$2
  body=$3
  length=$(printf '%s' "$body" | wc -c)
  { printf '%s %s HTTP/1.0\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %s\r\nConnection: close\r\n\r\n%s' \
      "$method" "$path" "$ENDPOINT" "$TOKEN" "$length" "$body"; sleep 2; } \
    | nc -w 10 "$API_HOST" "$API_PORT" | tr -d '\r' | sed '1,/^$/d'
}
attempt=0
submitted=0
while [ "$attempt" -lt 20 ]; do
  attempt=$((attempt+1))
  inspect=$(api GET "/api/v1/executions/$KODAMA_EXECUTION" '{}')
  case "$inspect" in
    *'"operation":"execution.inspect"'*) ;;
    *) echo "execution inspect failed: $inspect" >&2; exit 1 ;;
  esac
  version=$(printf '%s' "$inspect" | grep -o '"version":[0-9]*' | head -n 1 | cut -d: -f2)
  action=$(printf '%s' "$inspect" | sed 's/},{/}\n{/g' | grep -F "\"name\":\"$KODAMA_ACTION\"" | grep -o '"action_id":"[^"]*"' | head -n 1 | cut -d'"' -f4)
  test -n "$version"
  test -n "$action"
  response=$(api PUT "/api/v1/executions/$KODAMA_EXECUTION/actions/$action/outputs" "{\"input\":{\"outputs\":{__OUTPUT_MEMBERS__}},\"expected_version\":$version,\"idempotency_key\":\"outputs:$KODAMA_EXECUTION:$action:__OUTPUT_LABEL__:$attempt\"}")
  case "$response" in
    *'"operation":"execution.action.outputs"'*) submitted=1; break ;;
    *version_conflict*) echo "execution outputs hit a version conflict; retrying with a fresh key" >&2 ;;
    *) echo "output submission failed: $response" >&2; exit 1 ;;
  esac
done
if [ "$submitted" -ne 1 ]; then
  echo "output submission did not converge on a current execution version" >&2
  exit 1
fi`

// deliveryOutput is one declared output and the shell variable holding its
// value in a workload submission.
type deliveryOutput struct {
	Key      string
	Variable string
}

// deliverySubmitScript returns a submission script for one declared output.
func deliverySubmitScript(outputKey, outputVariable string) string {
	return deliverySubmitScripts(deliveryOutput{Key: outputKey, Variable: outputVariable})
}

// deliverySubmitScripts returns a submission script that stages every listed
// output in one map. All declared values must travel in one submission: staging
// a changed map for the same action is refused, so two separate one-key
// submissions would conflict instead of converging.
func deliverySubmitScripts(outputs ...deliveryOutput) string {
	members := make([]string, len(outputs))
	labels := make([]string, len(outputs))
	for index, output := range outputs {
		members[index] = `\"` + output.Key + `\":\"$` + output.Variable + `\"`
		labels[index] = output.Key
	}
	return strings.NewReplacer(
		"__OUTPUT_MEMBERS__", strings.Join(members, ","),
		"__OUTPUT_LABEL__", strings.Join(labels, "-"),
	).Replace(deliverySubmitTemplate)
}

// deliveryDumpLogs logs the retained stdout/stderr of every action of one
// execution. It is diagnostics for a failed wait, never an assertion.
func deliveryDumpLogs(t *testing.T, harness *factory.Harness, execution api.ExecutionInspectResult) {
	t.Helper()
	for _, action := range execution.Actions {
		for _, stream := range []string{"stdout", "stderr"} {
			result, publicError := harness.REST(harness.ProjectToken, "execution.logs.read", api.ExecutionLogsReadRequest{
				ExecutionID: execution.ExecutionID,
				ActionID:    action.ActionID,
				Stream:      stream,
			}, nil, "")
			if publicError.Code != "" {
				continue
			}
			var read api.ExecutionLogsReadResult
			harness.Decode(result, &read)
			if len(read.Chunks) == 0 {
				continue
			}
			var builder strings.Builder
			for _, chunk := range read.Chunks {
				_, _ = builder.Write(chunk.Data)
			}
			t.Logf("execution %s action %s %s logs:\n%s", execution.ExecutionID, action.Name, stream, factory.SanitizeDiagnostic(builder.String()))
		}
	}
}

// deliveryCountActionTransitions counts committed action.transition events for
// one action, which is the durable attribution record of its staged output.
func deliveryCountActionTransitions(t *testing.T, harness *factory.Harness, actionID string) int {
	t.Helper()
	events := readProjectEvents(t, harness, "action.transition")
	count := 0
	for _, event := range events.Events {
		if event.Type == "action.transition" && event.SubjectID == actionID {
			count++
		}
	}
	return count
}

// deliveryAssertNoArtifactEntity proves the served operation inventory has no
// artifact registration surface: an artifact is an output value, not a Kodama
// entity. The positive control keeps the check from passing on an empty table.
func deliveryAssertNoArtifactEntity(t *testing.T) {
	t.Helper()
	routes := api.Routes()
	if len(routes) == 0 {
		t.Fatal("api.Routes is empty; the artifact-entity check would be vacuous")
	}
	foundOutputs := false
	for _, route := range routes {
		if strings.HasPrefix(route.Operation, "artifact") {
			t.Fatalf("served operation %q exposes an artifact entity; an artifact must remain an output value", route.Operation)
		}
		if route.Operation == "execution.action.outputs" {
			foundOutputs = true
		}
	}
	if !foundOutputs {
		t.Fatal("api.Routes has no execution.action.outputs operation; the inventory under test is stale")
	}
}

// deliveryReleaseWorkload releases the named action's blocked workload by
// writing the release marker into the shared workspace through the Podman CLI.
func deliveryReleaseWorkload(t *testing.T, harness *factory.Harness, executionID string) {
	t.Helper()
	var container string
	harness.Eventually(t, 30*time.Second, func() bool {
		name, err := harness.RunningExecutionContainer(t.Context(), executionID)
		if err != nil || name == "" {
			return false
		}
		container = name
		return true
	})
	if _, err := harness.ExecContainer(t.Context(), container, "sh", "-c", "echo go > /workspace/release"); err != nil {
		t.Fatalf("release blocked workload %s: %v", container, err)
	}
}
