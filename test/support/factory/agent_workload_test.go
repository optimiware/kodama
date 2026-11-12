//go:build integration

package factory

// Real-boundary coverage for the executable agent workloads and the external
// durable checkpoint service.
//
// Each mode runs in its own container built from the pinned workload base. The
// native mode calls agentapp.Run with the default PublicModel; the external mode
// drives the public model contract directly. Neither injects a model port: both
// requests cross the real served edge, the real authorizer, the real configured
// model gateway and the scripted upstream provider. Neither fabricates a
// settlement: these are ordinary container commands driven by the fixture, and
// no served execution is claimed. The checkpoint transcript lives in a host
// directory served over HTTP outside the container, and the service is rebuilt
// between attempts so only durable bytes can restore the conversation.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/userland/session"
	runtimesupport "github.com/optimiware/kodama/test/support/infrastructure/runtime"
)

// agentWorkloadTestExchange is one plain final-content provider exchange.
func agentWorkloadTestExchange(name, answer string) ModelExchange {
	return ModelExchange{
		Name:   name,
		Expect: ModelExpectation{},
		Reply: ModelReply{
			Content:      answer,
			FinishReason: "stop",
			Usage:        api.ChatUsage{PromptTokens: 9, CompletionTokens: 3, TotalTokens: 12},
		},
	}
}

// agentWorkloadServiceCredential creates one project service with the engineer
// role and issues its credential. The workload container receives only that
// service credential; no human token enters the container.
func agentWorkloadServiceCredential(t *testing.T, harness *Harness, name string) (string, string) {
	t.Helper()
	serviceID := name + "-" + harness.FixtureID()[:8]
	created := harness.MustREST(harness.ProjectToken, "service.create", api.ServiceCreateRequest{
		ProjectID: ProjectID,
		ServiceID: serviceID,
		Roles:     []string{"engineer"},
	}, nil, "create-"+serviceID)
	var service api.ServiceCreateResult
	harness.Decode(created, &service)
	issued := harness.MustREST(harness.ProjectToken, "service.credential.issue", api.ServiceCredentialIssueRequest{
		ServiceID:  serviceID,
		TTLSeconds: 3600,
	}, &service.Version, "issue-"+serviceID)
	var credential api.ServiceCredentialIssueResult
	harness.Decode(issued, &credential)
	if credential.Token == nil || *credential.Token == "" {
		t.Fatalf("service %s issued no credential", serviceID)
	}
	return serviceID, *credential.Token
}

// agentWorkloadCounter returns the committed usage rows attributed to one
// service principal. It is the independent evidence that the container really
// crossed the served model gateway.
func agentWorkloadUsageCount(t *testing.T, harness *Harness, principal string) int {
	t.Helper()
	rows, err := harness.ModelUsageRows(context.Background())
	if err != nil {
		t.Fatalf("read model usage rows: %v", err)
	}
	count := 0
	for _, row := range rows {
		if row.Values["principal_id"] == principal {
			count++
		}
	}
	return count
}

// TestAgentWorkloadsRestoreDurableCheckpoints is the H proof. It runs the
// native and external workloads, restarts the checkpoint service between
// attempts, and shows both modes restore the transcript through the real model
// path. It then proves the missing-reference failure in both modes and that a
// closed external store cannot be bypassed from inside the container.
func TestAgentWorkloadsRestoreDurableCheckpoints(t *testing.T) {
	fixture := NewModelFixture(t,
		agentWorkloadTestExchange("native-first", "native first answer"),
		agentWorkloadTestExchange("native-restored", "native restored answer"),
		agentWorkloadTestExchange("external-first", "external first answer"),
		agentWorkloadTestExchange("external-restored", "external restored answer"),
	)
	harness := Start(t, WithModelFixture(fixture), WithTLS())
	service := StartAgentCheckpointService(t, AgentCheckpointConfig{
		T:            t,
		Root:         filepath.Join(harness.DataDir, "agent-checkpoints"),
		WorkloadHost: workloadListenHost(t),
	})
	workload := EnsureAgentWorkload(t, harness)
	image := workload.Image()
	if image.ImageID == image.BaseImageID {
		t.Fatalf("workload image id equals the shared base image id %s; the fixture did not build a distinct image", image.BaseImageID)
	}
	if !strings.Contains(image.Reference, "@sha256:") || strings.Contains(image.Reference, ":workload") {
		t.Fatalf("workload image reference = %q; want a digest-pinned reference without a mutable tag", image.Reference)
	}
	if image.Marker == "" {
		t.Fatal("workload image has no marker")
	}
	// The checkpoint directory is a host directory outside the runtime root
	// and outside every workspace volume the composition provisions.
	runtimeRoot := filepath.Join(harness.DataDir, "runtime")
	if strings.HasPrefix(filepath.Clean(service.Root()), filepath.Clean(runtimeRoot)+string(filepath.Separator)) {
		t.Fatalf("checkpoint root %q is inside the runtime root %q", service.Root(), runtimeRoot)
	}

	serviceID, credential := agentWorkloadServiceCredential(t, harness, "agent-h")

	// Native attempt 1: the default PublicModel through agentapp.Run.
	first := workload.Run(t, AgentWorkloadRun{
		Mode:        AgentWorkloadNative,
		ProjectID:   ProjectID,
		TaskID:      "task-agent-h-native",
		ExecutionID: "exec-agent-h-native-1",
		Model:       ModelAlias,
		Messages:    []string{"start the native agent workload"},
		Checkpoint:  service,
		Credential:  credential,
	})
	agentWorkloadAssertSucceeded(t, first, image.Marker)
	firstRef := first.Output.CheckpointRef
	if !strings.HasPrefix(firstRef, session.RefPrefix+session.RefVersion+"/") {
		t.Fatalf("native attempt 1 reference = %q; want a versioned checkpoint reference", firstRef)
	}
	if !first.ContainerGone {
		t.Fatalf("native attempt 1 container %q still exists after exit; the proof needs a disposable container", first.ContainerName)
	}
	agentWorkloadAssertStoredTranscript(t, service, firstRef,
		"start the native agent workload", "native first answer")
	refs, err := service.Refs()
	if err != nil {
		t.Fatalf("native attempt 1 refs: %v", err)
	}
	if len(refs) != 1 || refs[0] != firstRef {
		t.Fatalf("native attempt 1 service refs = %v; want [%s]", refs, firstRef)
	}

	// Reopen the external store: a new server, listener and storage instance
	// over the same durable directory. Only the files can answer attempt 2.
	service.Restart()
	if service.Generation() != 2 {
		t.Fatalf("checkpoint service generation = %d; want 2 after the reopen", service.Generation())
	}
	agentWorkloadAssertStoredTranscript(t, service, firstRef,
		"start the native agent workload", "native first answer")

	// Native attempt 2: restore the conversation from the reopened store.
	second := workload.Run(t, AgentWorkloadRun{
		Mode:        AgentWorkloadNative,
		ProjectID:   ProjectID,
		TaskID:      "task-agent-h-native",
		ExecutionID: "exec-agent-h-native-2",
		Model:       ModelAlias,
		Messages:    []string{"continue after the checkpoint store reopened"},
		RestoreRef:  firstRef,
		Checkpoint:  service,
		Credential:  credential,
	})
	agentWorkloadAssertSucceeded(t, second, image.Marker)
	if second.Output.CheckpointRef == firstRef {
		t.Fatalf("native attempt 2 returned the restored reference %q; want a new committed revision", firstRef)
	}
	agentWorkloadAssertStoredTranscript(t, service, second.Output.CheckpointRef,
		"start the native agent workload", "native first answer", "continue after the checkpoint store reopened", "native restored answer")

	// External attempt 1: public model contract directly, no agent app.
	externalFirst := workload.Run(t, AgentWorkloadRun{
		Mode:        AgentWorkloadExternal,
		ProjectID:   ProjectID,
		TaskID:      "task-agent-h-external",
		ExecutionID: "exec-agent-h-external-1",
		Model:       ModelAlias,
		Messages:    []string{"start the external agent workload"},
		Checkpoint:  service,
		Credential:  credential,
	})
	agentWorkloadAssertSucceeded(t, externalFirst, image.Marker)
	if externalFirst.Output.Content != "external first answer" {
		t.Fatalf("external attempt 1 content = %q; want the scripted answer", externalFirst.Output.Content)
	}
	agentWorkloadAssertStoredTranscript(t, service, externalFirst.Output.CheckpointRef,
		"start the external agent workload", "external first answer")

	// Reopen again, then restore the external transcript.
	service.Restart()
	externalSecond := workload.Run(t, AgentWorkloadRun{
		Mode:        AgentWorkloadExternal,
		ProjectID:   ProjectID,
		TaskID:      "task-agent-h-external",
		ExecutionID: "exec-agent-h-external-2",
		Model:       ModelAlias,
		Messages:    []string{"continue the external workload"},
		RestoreRef:  externalFirst.Output.CheckpointRef,
		Checkpoint:  service,
		Credential:  credential,
	})
	agentWorkloadAssertSucceeded(t, externalSecond, image.Marker)
	if externalSecond.Output.Content != "external restored answer" {
		t.Fatalf("external attempt 2 content = %q; want the restored scripted answer", externalSecond.Output.Content)
	}
	if externalSecond.Output.RestoredMessages != 2 {
		t.Fatalf("external attempt 2 restored messages = %d; want 2", externalSecond.Output.RestoredMessages)
	}

	// The provider saw both restored conversations, not a fresh prompt.
	requests := fixture.Requests()
	if len(requests) != 4 {
		t.Fatalf("provider requests = %d; want one per successful workload run", len(requests))
	}
	wantCounts := []int{1, 3, 1, 3}
	for index, want := range wantCounts {
		if requests[index].MessageCount != want {
			t.Fatalf("provider request %d message count = %d; want %d (restored transcript plus the new message)",
				index, requests[index].MessageCount, want)
		}
		if requests[index].Model != ModelUpstreamModel {
			t.Fatalf("provider request %d model = %q; want the configured upstream %q",
				index, requests[index].Model, ModelUpstreamModel)
		}
	}
	fixture.AssertSatisfied(t)

	// The real gateway persisted a usage row for every container call.
	if count := agentWorkloadUsageCount(t, harness, serviceID); count != 4 {
		t.Fatalf("model usage rows for service %s = %d; want 4 real gateway settlements", serviceID, count)
	}

	// Missing reference: the restoring run fails, commits nothing, and keeps
	// the durable store unchanged.
	before, err := service.Refs()
	if err != nil {
		t.Fatalf("refs before missing-reference runs: %v", err)
	}
	missingRef := session.RefPrefix + session.RefVersion + "/" + strings.Repeat("0", 32) + "-9"
	missingNative := workload.Run(t, AgentWorkloadRun{
		Mode:        AgentWorkloadNative,
		ProjectID:   ProjectID,
		TaskID:      "task-agent-h-native",
		ExecutionID: "exec-agent-h-native-missing",
		Model:       ModelAlias,
		Messages:    []string{"restore a missing native checkpoint"},
		RestoreRef:  missingRef,
		Checkpoint:  service,
		Credential:  credential,
	})
	if missingNative.ExitCode == 0 {
		t.Fatalf("native run with a missing checkpoint exited 0; output=%+v", missingNative.Output)
	}
	if missingNative.Output.Status != "failed" {
		t.Fatalf("native missing-checkpoint status = %q; want failed", missingNative.Output.Status)
	}
	if missingNative.Output.Error == "" {
		t.Fatal("native missing-checkpoint run reported no error")
	}
	missingExternal := workload.Run(t, AgentWorkloadRun{
		Mode:        AgentWorkloadExternal,
		ProjectID:   ProjectID,
		TaskID:      "task-agent-h-external",
		ExecutionID: "exec-agent-h-external-missing",
		Model:       ModelAlias,
		Messages:    []string{"restore a missing external checkpoint"},
		RestoreRef:  missingRef,
		Checkpoint:  service,
		Credential:  credential,
	})
	if missingExternal.ExitCode == 0 {
		t.Fatalf("external run with a missing checkpoint exited 0; output=%+v", missingExternal.Output)
	}
	if !strings.Contains(missingExternal.Output.Error, "missing") {
		t.Fatalf("external missing-checkpoint error = %q; want the typed missing reason", missingExternal.Output.Error)
	}
	after, err := service.Refs()
	if err != nil {
		t.Fatalf("refs after missing-reference runs: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("refs after missing-reference runs = %v; want unchanged %v", after, before)
	}

	// A closed external store cannot be bypassed from inside the container:
	// there is no local durable copy to fall back to.
	service.Close()
	closed := workload.Run(t, AgentWorkloadRun{
		Mode:        AgentWorkloadNative,
		ProjectID:   ProjectID,
		TaskID:      "task-agent-h-native",
		ExecutionID: "exec-agent-h-native-closed",
		Model:       ModelAlias,
		Messages:    []string{"restore after the external store closed"},
		RestoreRef:  firstRef,
		Checkpoint:  service,
		Credential:  credential,
	})
	if closed.ExitCode == 0 {
		t.Fatalf("native run with the external store closed exited 0; output=%+v", closed.Output)
	}

	fixture.AssertCredentialAbsent(t, first.Stdout, first.Stderr, second.Stdout, second.Stderr,
		externalFirst.Stdout, externalFirst.Stderr, externalSecond.Stdout, externalSecond.Stderr,
		missingNative.Stdout, missingNative.Stderr, missingExternal.Stdout, missingExternal.Stderr,
		closed.Stdout, closed.Stderr)
}

// agentWorkloadAssertSucceeded checks one successful workload outcome and the
// marker that proves the built image (not the shared base) ran it.
func agentWorkloadAssertSucceeded(t *testing.T, result AgentWorkloadResult, marker string) {
	t.Helper()
	if result.ExitCode != 0 {
		t.Fatalf("agent workload %s exited %d: stdout=%s stderr=%s",
			result.Output.Mode, result.ExitCode, result.Stdout, result.Stderr)
	}
	if !result.ContainerGone {
		t.Fatalf("agent workload container %q still exists after exit", result.ContainerName)
	}
	if result.Output.Status != "succeeded" {
		t.Fatalf("agent workload status = %q; want succeeded; output=%+v stderr=%s",
			result.Output.Status, result.Output, result.Stderr)
	}
	if result.Output.ImageMarker != marker {
		t.Fatalf("agent workload image marker = %q; want the fixture-unique %q", result.Output.ImageMarker, marker)
	}
}

// agentWorkloadAssertStoredTranscript reads one committed record through the
// real file storage and asserts the ordered message contents.
func agentWorkloadAssertStoredTranscript(t *testing.T, service *AgentCheckpointService, ref string, want ...string) {
	t.Helper()
	record, err := service.Read(ref)
	if err != nil {
		t.Fatalf("read stored checkpoint %s: %v", ref, err)
	}
	if len(record.Messages) != len(want) {
		t.Fatalf("stored checkpoint %s has %d messages; want %d: %+v", ref, len(record.Messages), len(want), record.Messages)
	}
	for index, content := range want {
		if record.Messages[index].Content != content {
			t.Fatalf("stored checkpoint %s message %d = %q; want %q", ref, index, record.Messages[index].Content, content)
		}
	}
}

// TestAgentWorkloadPlanIsFixtureUnique pins the pure image identity: the
// Containerfile names only the pinned base, the build tag and marker are
// fixture-unique, and an empty identity is refused.
func TestAgentWorkloadPlanIsFixtureUnique(t *testing.T) {
	base := runtimesupport.PinnedImage{
		Tag:       "docker.io/library/alpine:3.24",
		Digest:    "sha256:" + strings.Repeat("a", 64),
		Reference: "docker.io/library/alpine@sha256:" + strings.Repeat("a", 64),
	}
	first, err := newAgentWorkloadPlan("fixture-a", base)
	if err != nil {
		t.Fatalf("plan fixture a: %v", err)
	}
	second, err := newAgentWorkloadPlan("fixture-b", base)
	if err != nil {
		t.Fatalf("plan fixture b: %v", err)
	}
	if first.Tag == second.Tag || first.Marker == second.Marker {
		t.Fatalf("plans share identity: %q/%q and %q/%q", first.Tag, first.Marker, second.Tag, second.Marker)
	}
	if first.Tag == base.Tag || first.Tag == base.Reference {
		t.Fatalf("plan tag %q collides with the shared base", first.Tag)
	}
	if !strings.HasPrefix(first.Containerfile, "FROM "+base.Reference+"\n") {
		t.Fatalf("Containerfile does not start from the pinned base: %q", first.Containerfile)
	}
	if !strings.Contains(first.Containerfile, "COPY agentworkload "+agentWorkloadImagePath+"\n") {
		t.Fatalf("Containerfile does not copy the workload binary: %q", first.Containerfile)
	}
	if !strings.Contains(first.Containerfile, "COPY marker "+agentWorkloadMarkerPath+"\n") {
		t.Fatalf("Containerfile does not copy the marker: %q", first.Containerfile)
	}
	if _, err := newAgentWorkloadPlan("", base); err == nil {
		t.Fatal("empty fixture identity was accepted")
	}
	if _, err := newAgentWorkloadPlan("fixture-a", runtimesupport.PinnedImage{Tag: "tag-only"}); err == nil {
		t.Fatal("unpinned base was accepted")
	}
}

// TestAgentWorkloadCommandCarriesNoCredentialValue pins the run command shape
// and the secret boundary: the container gets mounted credential files, never a
// credential value in argv, and every run has a distinct name.
func TestAgentWorkloadCommandCarriesNoCredentialValue(t *testing.T) {
	image := AgentWorkloadImage{
		ImageID:       "sha256:" + strings.Repeat("b", 64),
		Digest:        "sha256:" + strings.Repeat("b", 64),
		Reference:     "localhost/kodama-agent-workload-fixture@sha256:" + strings.Repeat("b", 64),
		Tag:           "localhost/kodama-agent-workload-fixture:workload",
		BaseReference: "docker.io/library/alpine@sha256:" + strings.Repeat("a", 64),
		BaseImageID:   "sha256:" + strings.Repeat("a", 64),
		Marker:        "marker",
	}
	const secret = "do-not-log-this-service-credential"
	spec := agentWorkloadRunSpec{
		Mode:                     AgentWorkloadNative,
		ProjectID:                "project-1",
		TaskID:                   "task-1",
		ExecutionID:              "exec-1",
		Model:                    "fixture-model",
		Messages:                 []string{"hello"},
		RestoreRef:               "kodama-checkpoint/v1/" + strings.Repeat("c", 32) + "-1",
		APIURL:                   "http://host.containers.internal:1234",
		CheckpointURL:            "http://host.containers.internal:5678",
		CredentialFile:           "/run/credential",
		CheckpointCredentialFile: "/run/checkpoint-credential",
		CAFile:                   "/run/fixture-ca.pem",
		Labels:                   map[string]string{"kodama.test": "1", "kodama.fixture": "fixture-a"},
		Sequence:                 1,
	}
	first, err := newAgentWorkloadCommand(image, spec, secret, secret)
	if err != nil {
		t.Fatalf("build command: %v", err)
	}
	for index, argument := range first.Args {
		if strings.Contains(argument, secret) {
			t.Fatalf("argument %d carries a credential value", index)
		}
	}
	joined := strings.Join(first.Args, " ")
	for _, required := range []string{
		"--rm",
		"--pull=never",
		"--volume /run/credential:" + AgentWorkloadCredentialPath + ":ro",
		"--volume /run/checkpoint-credential:" + AgentWorkloadCheckpointTokenPath + ":ro",
		"--volume /run/fixture-ca.pem:" + agentWorkloadCAFilePath + ":ro",
		image.Reference,
		agentWorkloadImagePath,
		"-mode native",
		"-checkpoint-url http://host.containers.internal:5678",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("command arguments are missing %q: %s", required, joined)
		}
	}
	spec.Sequence = 2
	second, err := newAgentWorkloadCommand(image, spec, secret, secret)
	if err != nil {
		t.Fatalf("build second command: %v", err)
	}
	if first.Name == second.Name {
		t.Fatalf("two attempts share the container name %q", first.Name)
	}
	spec.Mode = "unknown"
	if _, err := newAgentWorkloadCommand(image, spec, secret, secret); err == nil {
		t.Fatal("unknown workload mode was accepted")
	}
	spec.Mode = AgentWorkloadNative
	spec.CheckpointURL = ""
	if _, err := newAgentWorkloadCommand(image, spec, secret, secret); err == nil {
		t.Fatal("run without an external checkpoint URL was accepted")
	}
	spec.CheckpointURL = "http://host.containers.internal:5678"
	unpinned := image
	unpinned.Reference = image.Tag
	if _, err := newAgentWorkloadCommand(unpinned, spec, secret, secret); err == nil {
		t.Fatal("unpinned workload image reference was accepted")
	}
}

// TestAgentWorkloadCredentialFileCarriesMode pins the private credential file
// the runner mounts: owner-only permissions and no inherited value.
func TestAgentWorkloadCredentialFileCarriesMode(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "KODAMA_TOKEN")
	if err := writeAgentWorkloadCredentialFile(path, "fixture-credential"); err != nil {
		t.Fatalf("write credential: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat credential: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credential mode = %o; want 600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read credential: %v", err)
	}
	if string(data) != "fixture-credential" {
		t.Fatalf("credential content = %q", data)
	}
	if err := writeAgentWorkloadCredentialFile(filepath.Join(directory, "empty"), ""); err == nil {
		t.Fatal("empty credential was accepted")
	}
}
