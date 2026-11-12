package pipeline_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func TestAdmissionBuildsImmutableSnapshotAndActions(t *testing.T) {
	yaml := validPipelineYAML()
	resolver := &fakeResolver{}
	changes := &fakeChangePort{}
	admission := newAdmission(t, resolver, nil)

	result, err := admission.Admit(t.Context(), pipeline.AdmitRequest{
		Scope:          projectScope("tenant-a", "project-a"),
		Definition:     pipeline.Definition{Name: "change", YAML: yaml},
		Inputs:         map[id.InputKey]string{"repository": "repo-1"},
		Requirements:   []string{"network"},
		DefaultTimeout: 5 * time.Minute,
	})
	requireNoError(t, err, "Admit valid pipeline")

	if result.PipelineID != "" || result.PipelineVersion != 0 {
		t.Fatalf("inline admission identity = %q version %d; want empty/0", result.PipelineID, result.PipelineVersion)
	}
	if result.Snapshot.YAMLSHA256 != sha256.Sum256(yaml) {
		t.Fatalf("snapshot hash = %x; want %x", result.Snapshot.YAMLSHA256, sha256.Sum256(yaml))
	}
	if !bytes.Equal(result.Snapshot.YAMLBytes(), yaml) {
		t.Fatalf("snapshot YAML = %q; want %q", result.Snapshot.YAMLBytes(), yaml)
	}
	if result.Snapshot.Inputs["repository"] != "repo-1" {
		t.Fatalf("snapshot inputs = %v", result.Snapshot.Inputs)
	}
	if len(result.Snapshot.Images) != 1 {
		t.Fatalf("snapshot images = %v; want one pinned image", result.Snapshot.Images)
	}
	digest := digestFor("registry.example.com/build:latest")
	if result.Snapshot.Images[0].Reference != "registry.example.com/build:latest" || result.Snapshot.Images[0].Digest != digest {
		t.Fatalf("pinned image = %+v; want reference/digest", result.Snapshot.Images[0])
	}
	if len(result.Snapshot.SecretNames) != 1 || result.Snapshot.SecretNames[0] != "DEPLOY_TOKEN" {
		t.Fatalf("snapshot secret names = %v", result.Snapshot.SecretNames)
	}
	if len(result.Snapshot.Requirements) != 1 || result.Snapshot.Requirements[0] != "network" {
		t.Fatalf("snapshot requirements = %v", result.Snapshot.Requirements)
	}

	if joinNames(result.Actions) != "build,review" {
		t.Fatalf("admitted actions = %v; want build,review", actionNames(result.Actions))
	}
	build := requireAction(t, result.Actions, 0, "build", definition.ActionKindContainer, 10*time.Minute)
	if build.Container == nil {
		t.Fatal("build action has no container declaration")
	}
	if build.Container.Image != "registry.example.com/build@"+digest {
		t.Fatalf("admitted image = %q; want digest-pinned", build.Container.Image)
	}
	review := requireAction(t, result.Actions, 1, "review", definition.ActionKindRequest, 72*time.Hour)
	if review.Request == nil || review.Request.Assignment != "role/reviewer" {
		t.Fatalf("review action = %+v", review)
	}

	if resolver.callCount() != 1 {
		t.Fatalf("resolver calls = %d; want 1", resolver.callCount())
	}
	if requests := resolver.requests(); requests[0].Reference != "registry.example.com/build:latest" {
		t.Fatalf("resolved reference = %q", requests[0].Reference)
	}
	if changes.callCount() != 0 {
		t.Fatalf("admission invoked a commit port %d times; resolution must stay outside commits", changes.callCount())
	}
}

func TestAdmissionExactByteHashAndMutationResistance(t *testing.T) {
	yamlA := []byte(`inputs: [repository]
actions:
  - name: review
    request: role/reviewer
    subject:
      resource:
        from: inputs.repository
`)
	yamlB := []byte(`inputs:
- repository
actions:
- name: review
  request: role/reviewer
  subject:
    resource:
      from: inputs.repository
`)
	if bytes.Equal(yamlA, yamlB) {
		t.Fatal("fixtures are not byte-distinct")
	}
	admission := newAdmission(t, &fakeResolver{}, nil)

	resultA, err := admission.Admit(t.Context(), pipeline.AdmitRequest{
		Scope:          projectScope("tenant-a", "project-a"),
		Definition:     pipeline.Definition{Name: "change", YAML: yamlA},
		Inputs:         map[id.InputKey]string{"repository": "repo-1"},
		DefaultTimeout: 5 * time.Minute,
	})
	requireNoError(t, err, "Admit first YAML")
	resultB, err := admission.Admit(t.Context(), pipeline.AdmitRequest{
		Scope:          projectScope("tenant-a", "project-a"),
		Definition:     pipeline.Definition{Name: "change", YAML: yamlB},
		Inputs:         map[id.InputKey]string{"repository": "repo-1"},
		DefaultTimeout: 5 * time.Minute,
	})
	requireNoError(t, err, "Admit second YAML")

	wantA := sha256.Sum256(yamlA)
	wantB := sha256.Sum256(yamlB)
	if resultA.Snapshot.YAMLSHA256 != wantA || resultB.Snapshot.YAMLSHA256 != wantB {
		t.Fatalf("hashes = %x/%x; want %x/%x", resultA.Snapshot.YAMLSHA256, resultB.Snapshot.YAMLSHA256, wantA, wantB)
	}
	if resultA.Snapshot.YAMLSHA256 == resultB.Snapshot.YAMLSHA256 {
		t.Fatal("byte-distinct equivalent YAML produced the same hash")
	}
	if hex.EncodeToString(resultA.Snapshot.YAMLSHA256[:]) != resultA.Snapshot.HashHex() {
		t.Fatalf("HashHex = %q; want %x", resultA.Snapshot.HashHex(), resultA.Snapshot.YAMLSHA256)
	}

	// Later edits to the supplied bytes cannot mutate the admitted snapshot.
	yamlA[0] = 'X'
	if !bytes.Equal(resultA.Snapshot.YAMLBytes(), []byte(`inputs: [repository]
actions:
  - name: review
    request: role/reviewer
    subject:
      resource:
        from: inputs.repository
`)) {
		t.Fatal("snapshot YAML was mutated through the caller's slice")
	}
	returned := resultA.Snapshot.YAMLBytes()
	returned[0] = 'Y'
	if resultA.Snapshot.YAMLBytes()[0] == 'Y' {
		t.Fatal("YAMLBytes returned the internal slice")
	}
}

func TestAdmissionValidatesInputs(t *testing.T) {
	admission := newAdmission(t, &fakeResolver{}, nil)
	scope := projectScope("tenant-a", "project-a")
	base := pipeline.AdmitRequest{
		Scope:          scope,
		Definition:     pipeline.Definition{Name: "change", YAML: typedInputsYAML()},
		DefaultTimeout: 5 * time.Minute,
	}

	valid := base
	valid.Inputs = map[id.InputKey]string{"repository": "repo-1", "retries": "3", "dry_run": "true"}
	result, err := admission.Admit(t.Context(), valid)
	requireNoError(t, err, "Admit valid typed inputs")
	if result.Snapshot.Inputs["retries"] != "3" || result.Snapshot.Inputs["dry_run"] != "true" {
		t.Fatalf("snapshot inputs = %v", result.Snapshot.Inputs)
	}

	invalid := map[string]map[id.InputKey]string{
		"missing required": {"retries": "3"},
		"extra undeclared": {"repository": "repo-1", "unknown": "x"},
		"invalid number":   {"repository": "repo-1", "retries": "many"},
		"invalid boolean":  {"repository": "repo-1", "dry_run": "yes"},
	}
	for name, inputs := range invalid {
		t.Run(name, func(t *testing.T) {
			request := base
			request.Inputs = inputs
			_, err := admission.Admit(t.Context(), request)
			requireFaultClass(t, err, fault.InvalidInput)
		})
	}
}

func TestAdmissionInlineTaskFree(t *testing.T) {
	resolver := &fakeResolver{}
	admission := newAdmission(t, resolver, nil)
	result, err := admission.Admit(t.Context(), pipeline.AdmitRequest{
		Scope:          projectScope("tenant-a", "project-a"),
		Definition:     pipeline.Definition{Name: "inline", YAML: requestOnlyYAML()},
		Inputs:         map[id.InputKey]string{"repository": "repo-1"},
		DefaultTimeout: 5 * time.Minute,
	})
	requireNoError(t, err, "Admit inline task-free pipeline")
	if result.Snapshot.TaskID != "" {
		t.Fatalf("snapshot task id = %q; want empty", result.Snapshot.TaskID)
	}
	if len(result.Snapshot.References) != 0 || len(result.Snapshot.Metadata) != 0 {
		t.Fatalf("task-free snapshot carries task context: refs=%v metadata=%v", result.Snapshot.References, result.Snapshot.Metadata)
	}
	if len(result.Actions) != 1 || result.Actions[0].Kind != definition.ActionKindRequest {
		t.Fatalf("actions = %+v; want one request action", result.Actions)
	}
	if resolver.callCount() != 0 {
		t.Fatalf("resolver calls = %d; a request-only pipeline has no image", resolver.callCount())
	}
}

func TestAdmissionTaskContextBounds(t *testing.T) {
	admission := newAdmission(t, &fakeResolver{}, nil)
	base := pipeline.AdmitRequest{
		Scope:          projectScope("tenant-a", "project-a"),
		Definition:     pipeline.Definition{Name: "inline", YAML: requestOnlyYAML()},
		Inputs:         map[id.InputKey]string{"repository": "repo-1"},
		DefaultTimeout: 5 * time.Minute,
	}

	withTask := base
	withTask.Task = pipeline.TaskContext{
		TaskID:       "task-1",
		Conversation: []byte("please review"),
		References:   []string{"refs/heads/main"},
		Metadata:     []byte(`{"source":"git","ref":"refs/heads/main"}`),
	}
	result, err := admission.Admit(t.Context(), withTask)
	requireNoError(t, err, "Admit with task context")
	if result.Snapshot.TaskID != "task-1" {
		t.Fatalf("snapshot task id = %q", result.Snapshot.TaskID)
	}
	if len(result.Snapshot.References) != 1 || result.Snapshot.References[0] != "refs/heads/main" {
		t.Fatalf("snapshot references = %v", result.Snapshot.References)
	}
	if result.Snapshot.Metadata["source"] != "git" || result.Snapshot.Metadata["ref"] != "refs/heads/main" {
		t.Fatalf("snapshot metadata = %v", result.Snapshot.Metadata)
	}

	metadata := func(size int) []byte {
		value := size - len(`{"k":""}`)
		if value < 0 {
			t.Fatalf("cannot build %d-byte metadata", size)
		}
		return []byte(`{"k":"` + strings.Repeat("x", value) + `"}`)
	}
	exact := base
	exact.Task = pipeline.TaskContext{Metadata: metadata(16 << 10)}
	if _, err := admission.Admit(t.Context(), exact); err != nil {
		t.Fatalf("Admit exactly 16 KiB metadata: %v", err)
	}
	oversized := base
	oversized.Task = pipeline.TaskContext{Metadata: metadata(16<<10 + 1)}
	_, err = admission.Admit(t.Context(), oversized)
	requireFaultClass(t, err, fault.InvalidInput)

	invalidMetadata := map[string][]byte{
		"not an object":    []byte(`[]`),
		"null":             []byte(`null`),
		"non-string value": []byte(`{"n":1}`),
		"invalid JSON":     []byte(`{`),
	}
	for name, value := range invalidMetadata {
		t.Run(name, func(t *testing.T) {
			request := base
			request.Task = pipeline.TaskContext{Metadata: value}
			_, err := admission.Admit(t.Context(), request)
			requireFaultClass(t, err, fault.InvalidInput)
		})
	}

	exactConversation := base
	exactConversation.Task = pipeline.TaskContext{Conversation: bytes.Repeat([]byte("x"), 64<<10)}
	if _, err := admission.Admit(t.Context(), exactConversation); err != nil {
		t.Fatalf("Admit exactly 64 KiB conversation: %v", err)
	}
	oversizedConversation := base
	oversizedConversation.Task = pipeline.TaskContext{Conversation: bytes.Repeat([]byte("x"), 64<<10+1)}
	_, err = admission.Admit(t.Context(), oversizedConversation)
	requireFaultClass(t, err, fault.InvalidInput)

	badTask := base
	badTask.Task = pipeline.TaskContext{TaskID: "bad/id"}
	_, err = admission.Admit(t.Context(), badTask)
	requireFaultClass(t, err, fault.InvalidInput)
}

func TestAdmissionSnapshotCarriesTaskConversation(t *testing.T) {
	conversation := []byte("please review the change")
	admission := newAdmission(t, &fakeResolver{}, nil)
	base := pipeline.AdmitRequest{
		Scope:          projectScope("tenant-a", "project-a"),
		Definition:     pipeline.Definition{Name: "inline", YAML: requestOnlyYAML()},
		Inputs:         map[id.InputKey]string{"repository": "repo-1"},
		Task:           pipeline.TaskContext{TaskID: "task-1", Conversation: conversation},
		DefaultTimeout: 5 * time.Minute,
	}
	result, err := admission.Admit(t.Context(), base)
	requireNoError(t, err, "Admit with task conversation")
	if !bytes.Equal(result.Snapshot.ConversationBytes(), conversation) {
		t.Fatalf("snapshot conversation = %q; want %q", result.Snapshot.ConversationBytes(), conversation)
	}

	// The snapshot owns a copy: editing the caller's bytes cannot change it.
	conversation[0] = 'X'
	if !bytes.Equal(result.Snapshot.ConversationBytes(), []byte("please review the change")) {
		t.Fatal("snapshot conversation changed through the caller's slice")
	}
	returned := result.Snapshot.ConversationBytes()
	returned[0] = 'Y'
	if result.Snapshot.ConversationBytes()[0] == 'Y' {
		t.Fatal("ConversationBytes returned the internal slice")
	}

	// A task-free admission carries no conversation.
	taskFree := base
	taskFree.Task = pipeline.TaskContext{}
	free, err := admission.Admit(t.Context(), taskFree)
	requireNoError(t, err, "Admit task-free")
	if free.Snapshot.Conversation != nil || free.Snapshot.ConversationBytes() != nil {
		t.Fatalf("task-free snapshot conversation = %q; want nil", free.Snapshot.Conversation)
	}
}

func TestAdmissionResolvesImagesAndRefusesMutableOrUnavailable(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	base := pipeline.AdmitRequest{
		Scope:          scope,
		Definition:     pipeline.Definition{Name: "change", YAML: validPipelineYAML()},
		Inputs:         map[id.InputKey]string{"repository": "repo-1"},
		DefaultTimeout: 5 * time.Minute,
	}

	resolver := &fakeResolver{}
	admission := newAdmission(t, resolver, nil)
	result, err := admission.Admit(t.Context(), base)
	requireNoError(t, err, "Admit mutable tag")
	if len(result.Snapshot.Images) != 1 {
		t.Fatalf("snapshot images = %v; want one pinned image", result.Snapshot.Images)
	}
	digest := digestFor("registry.example.com/build:latest")
	if result.Snapshot.Images[0].Digest != digest {
		t.Fatalf("pinned digest = %q; want %q", result.Snapshot.Images[0].Digest, digest)
	}

	mutable := &fakeResolver{resolve: func(_ context.Context, request pipeline.ResolveRequest) (pipeline.ResolvedImage, error) {
		return pipeline.ResolvedImage{Reference: request.Reference, Digest: "latest"}, nil
	}}
	_, err = newAdmission(t, mutable, nil).Admit(t.Context(), base)
	requireFaultClass(t, err, fault.UnsupportedCapability)

	shortDigest := &fakeResolver{resolve: func(_ context.Context, request pipeline.ResolveRequest) (pipeline.ResolvedImage, error) {
		return pipeline.ResolvedImage{Reference: request.Reference, Digest: "sha256:short"}, nil
	}}
	_, err = newAdmission(t, shortDigest, nil).Admit(t.Context(), base)
	requireFaultClass(t, err, fault.UnsupportedCapability)

	unavailable := &fakeResolver{resolve: func(context.Context, pipeline.ResolveRequest) (pipeline.ResolvedImage, error) {
		return pipeline.ResolvedImage{}, fault.New(fault.DependencyUnavailable, "registry unavailable")
	}}
	_, err = newAdmission(t, unavailable, nil).Admit(t.Context(), base)
	requireFaultClass(t, err, fault.DependencyUnavailable)

	plainError := &fakeResolver{resolve: func(context.Context, pipeline.ResolveRequest) (pipeline.ResolvedImage, error) {
		return pipeline.ResolvedImage{}, errPlain
	}}
	_, err = newAdmission(t, plainError, nil).Admit(t.Context(), base)
	requireFaultClass(t, err, fault.DependencyUnavailable)

	// Resolution is outside any commit: a commit port is never touched.
	changes := &fakeChangePort{}
	if _, err := newAdmission(t, &fakeResolver{}, nil).Admit(t.Context(), base); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if changes.callCount() != 0 {
		t.Fatalf("commit port calls = %d; want 0", changes.callCount())
	}
}

var errPlain = &plainError{}

type plainError struct{}

func (*plainError) Error() string { return "network down" }

func TestAdmissionPinsSelectedDefinitionVersionAcrossResolution(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	yaml := validPipelineYAML()
	currentVersion := uint64(3)
	reader := &fakeReader{}
	reader.getFn = func(context.Context, tenancy.Scope, pipeline.PipelineID) (pipeline.Definition, error) {
		return pipeline.Definition{ID: "pipeline-1", Name: "change", YAML: yaml, Version: currentVersion}, nil
	}
	resolver := &fakeResolver{resolve: func(_ context.Context, request pipeline.ResolveRequest) (pipeline.ResolvedImage, error) {
		// A concurrent definition edit lands while the registry call is in
		// flight. Admission must still describe the definition it selected;
		// the caller's commit rechecks ExpectedPipelineVersion.
		currentVersion = 4
		return pipeline.ResolvedImage{Reference: request.Reference, Digest: digestFor(request.Reference)}, nil
	}}
	admission := newAdmission(t, resolver, reader)
	result, err := admission.Admit(t.Context(), pipeline.AdmitRequest{
		Scope:          scope,
		Definition:     pipeline.Definition{ID: "pipeline-1"},
		Inputs:         map[id.InputKey]string{"repository": "repo-1"},
		DefaultTimeout: 5 * time.Minute,
	})
	requireNoError(t, err, "Admit stored pipeline")
	if currentVersion != 4 {
		t.Fatalf("definition version = %d; want the concurrent edit to have happened", currentVersion)
	}
	if result.PipelineID != "pipeline-1" || result.PipelineVersion != 3 {
		t.Fatalf("admitted identity = %q version %d; want pipeline-1 version 3", result.PipelineID, result.PipelineVersion)
	}
	if result.Snapshot.YAMLSHA256 != sha256.Sum256(yaml) {
		t.Fatalf("stored snapshot hash = %x; want the selected v3 bytes", result.Snapshot.YAMLSHA256)
	}
	if got := result.Snapshot.Inputs["repository"]; got != "repo-1" {
		t.Fatalf("snapshot inputs = %v", result.Snapshot.Inputs)
	}
}

// TestAdmissionUsesSuppliedFirstSelectionBytes pins the push-replay contract:
// when a caller supplies the stored identity together with exact YAML bytes,
// those bytes are authoritative and the current definition is never re-read. A
// definition edit after the first selection must not change the admitted
// snapshot.
func TestAdmissionUsesSuppliedFirstSelectionBytes(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	firstYAML := validPipelineYAML()
	editedYAML := bytes.Replace(firstYAML, []byte("make"), []byte("edited-command"), 1)
	if bytes.Equal(firstYAML, editedYAML) {
		t.Fatal("test yaml has no editable command")
	}
	reader := &fakeReader{}
	reader.getFn = func(context.Context, tenancy.Scope, pipeline.PipelineID) (pipeline.Definition, error) {
		return pipeline.Definition{ID: "pipeline-1", Name: "change", YAML: editedYAML, Version: 2}, nil
	}
	admission := newAdmission(t, &fakeResolver{}, reader)
	result, err := admission.Admit(t.Context(), pipeline.AdmitRequest{
		Scope: scope,
		Definition: pipeline.Definition{
			ID:      "pipeline-1",
			Name:    "change",
			YAML:    firstYAML,
			Version: 1,
		},
		Inputs:         map[id.InputKey]string{"repository": "repo-1"},
		DefaultTimeout: 5 * time.Minute,
	})
	requireNoError(t, err, "Admit supplied first selection")
	if get, byName, list := reader.counts(); get != 0 || byName != 0 || list != 0 {
		t.Fatalf("admission consulted the current definition: get=%d byName=%d list=%d", get, byName, list)
	}
	if result.PipelineID != "pipeline-1" || result.PipelineVersion != 1 {
		t.Fatalf("admitted identity = %q version %d; want the supplied pipeline-1 version 1",
			result.PipelineID, result.PipelineVersion)
	}
	if result.Snapshot.YAMLSHA256 != sha256.Sum256(firstYAML) {
		t.Fatalf("snapshot hash = %x; want the supplied first-selection bytes", result.Snapshot.YAMLSHA256)
	}
	if bytes.Contains(result.Snapshot.YAMLBytes(), []byte("edited-command")) {
		t.Fatalf("snapshot = %q; an edit must never change the first selection", result.Snapshot.YAMLBytes())
	}
	// The supplied slice is copied, so later caller mutation cannot change the
	// admitted snapshot.
	want := append([]byte(nil), firstYAML...)
	firstYAML[0] = 'X'
	if !bytes.Equal(result.Snapshot.YAMLBytes(), want) {
		t.Fatalf("snapshot = %q; want the copied pre-mutation bytes %q", result.Snapshot.YAMLBytes(), want)
	}
}

func TestAdmissionRequiresDefinitionSource(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	admission := newAdmission(t, &fakeResolver{}, nil)
	_, err := admission.Admit(t.Context(), pipeline.AdmitRequest{
		Scope:          scope,
		DefaultTimeout: 5 * time.Minute,
	})
	requireFaultClass(t, err, fault.InvalidInput)

	readerless := newAdmission(t, &fakeResolver{}, nil)
	_, err = readerless.Admit(t.Context(), pipeline.AdmitRequest{
		Scope:          scope,
		Definition:     pipeline.Definition{ID: "pipeline-1"},
		DefaultTimeout: 5 * time.Minute,
	})
	requireFaultClass(t, err, fault.InvalidInput)

	tenantScoped := newAdmission(t, &fakeResolver{}, nil)
	_, err = tenantScoped.Admit(t.Context(), pipeline.AdmitRequest{
		Scope:          tenantScope("tenant-a"),
		Definition:     pipeline.Definition{Name: "inline", YAML: requestOnlyYAML()},
		Inputs:         map[id.InputKey]string{"repository": "repo-1"},
		DefaultTimeout: 5 * time.Minute,
	})
	requireFaultClass(t, err, fault.InvalidInput)
}

func TestAdmissionRefusesUnavailableSubjectInput(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	admission := newAdmission(t, &fakeResolver{}, nil)
	_, err := admission.Admit(t.Context(), pipeline.AdmitRequest{
		Scope:          scope,
		Definition:     pipeline.Definition{Name: "inline", YAML: requestOnlyYAML()},
		DefaultTimeout: 5 * time.Minute,
	})
	requireFaultClass(t, err, fault.InvalidInput)

	result, err := admission.Admit(t.Context(), pipeline.AdmitRequest{
		Scope:          scope,
		Definition:     pipeline.Definition{Name: "inline", YAML: requestOnlyYAML()},
		Inputs:         map[id.InputKey]string{"repository": "repo-1"},
		DefaultTimeout: 5 * time.Minute,
	})
	requireNoError(t, err, "Admit supplied subject input")
	if result.Snapshot.Inputs["repository"] != "repo-1" {
		t.Fatalf("snapshot inputs = %v", result.Snapshot.Inputs)
	}
}

func TestAdmissionRefusesRequestActionWithoutSubject(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	withoutSubject := []byte(`actions:
  - name: review
    request: role/reviewer
`)
	emptySubject := []byte(`actions:
  - name: review
    request: role/reviewer
    subject: {}
`)
	withSubject := []byte(`actions:
  - name: review
    request: role/reviewer
    subject:
      operation: git.ref.update
`)

	sources := []struct {
		name   string
		stored bool
	}{
		{name: "inline"},
		{name: "stored", stored: true},
	}
	for _, source := range sources {
		t.Run(source.name, func(t *testing.T) {
			admissionFor := func(yaml []byte) *pipeline.AdmissionService {
				if !source.stored {
					return newAdmission(t, &fakeResolver{}, nil)
				}
				reader := &fakeReader{}
				reader.getFn = func(context.Context, tenancy.Scope, pipeline.PipelineID) (pipeline.Definition, error) {
					return pipeline.Definition{ID: "pipeline-1", Name: "change", YAML: yaml, Version: 1}, nil
				}
				return newAdmission(t, &fakeResolver{}, reader)
			}
			requestFor := func(yaml []byte) pipeline.AdmitRequest {
				selected := pipeline.Definition{Name: "change", YAML: yaml}
				if source.stored {
					selected = pipeline.Definition{ID: "pipeline-1"}
				}
				return pipeline.AdmitRequest{
					Scope:          scope,
					Definition:     selected,
					DefaultTimeout: 5 * time.Minute,
				}
			}

			refusals := []struct {
				name string
				yaml []byte
			}{
				{name: "omitted subject", yaml: withoutSubject},
				{name: "empty subject", yaml: emptySubject},
			}
			for _, refusal := range refusals {
				t.Run(refusal.name, func(t *testing.T) {
					_, err := admissionFor(refusal.yaml).Admit(t.Context(), requestFor(refusal.yaml))
					requireFaultClass(t, err, fault.InvalidInput)
					message, ok := fault.MessageOf(err)
					if !ok || !strings.Contains(message, `"review"`) || !strings.Contains(message, "no subject") {
						t.Fatalf("message = %q; want it to name action %q and report no subject", message, "review")
					}
				})
			}

			result, err := admissionFor(withSubject).Admit(t.Context(), requestFor(withSubject))
			requireNoError(t, err, "Admit request action with subject")
			if len(result.Actions) != 1 || result.Actions[0].Request == nil {
				t.Fatalf("actions = %+v; want one request action", result.Actions)
			}
			operation := result.Actions[0].Request.Subject.Fields()["operation"]
			if operation.Kind != definition.SubjectLiteral || operation.Literal != "git.ref.update" {
				t.Fatalf("admitted subject operation = %+v; want literal git.ref.update", operation)
			}
		})
	}
}

func TestAdmissionSnapshotMutationResistance(t *testing.T) {
	yaml := validPipelineYAML()
	wantYAML := append([]byte(nil), yaml...)
	inputs := map[id.InputKey]string{"repository": "repo-1"}
	requirements := []string{"network"}
	references := []string{"refs/heads/main"}
	metadata := []byte(`{"source":"git"}`)
	admission := newAdmission(t, &fakeResolver{}, nil)
	result, err := admission.Admit(t.Context(), pipeline.AdmitRequest{
		Scope:          projectScope("tenant-a", "project-a"),
		Definition:     pipeline.Definition{Name: "change", YAML: yaml},
		Inputs:         inputs,
		Requirements:   requirements,
		Task:           pipeline.TaskContext{TaskID: "task-1", References: references, Metadata: metadata},
		DefaultTimeout: 5 * time.Minute,
	})
	requireNoError(t, err, "Admit")

	yaml[0] = 'X'
	inputs["repository"] = "repo-2"
	requirements[0] = "gpu"
	references[0] = "refs/heads/other"
	metadata[0] = '['

	if !bytes.Equal(result.Snapshot.YAMLBytes(), wantYAML) {
		t.Fatal("snapshot YAML changed through the caller's slice")
	}
	if result.Snapshot.Inputs["repository"] != "repo-1" {
		t.Fatalf("snapshot inputs changed: %v", result.Snapshot.Inputs)
	}
	if len(result.Snapshot.Requirements) != 1 || result.Snapshot.Requirements[0] != "network" {
		t.Fatalf("snapshot requirements changed: %v", result.Snapshot.Requirements)
	}
	if len(result.Snapshot.References) != 1 || result.Snapshot.References[0] != "refs/heads/main" {
		t.Fatalf("snapshot references changed: %v", result.Snapshot.References)
	}
	if result.Snapshot.Metadata["source"] != "git" {
		t.Fatalf("snapshot metadata changed: %v", result.Snapshot.Metadata)
	}
}

func TestAdmissionResolvesDefaultAndExplicitTimeouts(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	admission := newAdmission(t, &fakeResolver{}, nil)

	defaulted, err := admission.Admit(t.Context(), pipeline.AdmitRequest{
		Scope:          scope,
		Definition:     pipeline.Definition{Name: "inline", YAML: requestOnlyYAML()},
		Inputs:         map[id.InputKey]string{"repository": "repo-1"},
		DefaultTimeout: 5 * time.Minute,
	})
	requireNoError(t, err, "Admit default timeout")
	requireActions(t, defaulted.Actions, 1)
	if defaulted.Actions[0].Timeout != 5*time.Minute {
		t.Fatalf("default timeout = %s; want 5m", defaulted.Actions[0].Timeout)
	}

	explicitYAML := []byte(`actions:
  - name: review
    request: role/reviewer
    timeout: 1m
    subject:
      operation: git.ref.update
`)
	explicit, err := admission.Admit(t.Context(), pipeline.AdmitRequest{
		Scope:          scope,
		Definition:     pipeline.Definition{Name: "inline", YAML: explicitYAML},
		DefaultTimeout: 5 * time.Minute,
	})
	requireNoError(t, err, "Admit explicit timeout")
	requireActions(t, explicit.Actions, 1)
	if explicit.Actions[0].Timeout != time.Minute {
		t.Fatalf("explicit timeout = %s; want 1m", explicit.Actions[0].Timeout)
	}

	for _, timeout := range []time.Duration{0, 59*time.Second + 999999999*time.Nanosecond, 720*time.Hour + time.Nanosecond} {
		_, err := admission.Admit(t.Context(), pipeline.AdmitRequest{
			Scope:          scope,
			Definition:     pipeline.Definition{Name: "inline", YAML: requestOnlyYAML()},
			Inputs:         map[id.InputKey]string{"repository": "repo-1"},
			DefaultTimeout: timeout,
		})
		requireFaultClass(t, err, fault.InvalidInput)
	}
	for _, timeout := range []time.Duration{time.Minute, 720 * time.Hour} {
		if _, err := admission.Admit(t.Context(), pipeline.AdmitRequest{
			Scope:          scope,
			Definition:     pipeline.Definition{Name: "inline", YAML: requestOnlyYAML()},
			Inputs:         map[id.InputKey]string{"repository": "repo-1"},
			DefaultTimeout: timeout,
		}); err != nil {
			t.Fatalf("Admit default %s: %v", timeout, err)
		}
	}

	actionType := reflect.TypeFor[pipeline.AdmittedAction]()
	if _, ok := actionType.FieldByName("Deadline"); ok {
		t.Fatal("AdmittedAction carries an admission-time deadline")
	}
	if _, ok := reflect.TypeFor[pipeline.AdmissionResult]().FieldByName("Deadline"); ok {
		t.Fatal("AdmissionResult carries an admission-time deadline")
	}
	if _, ok := reflect.TypeFor[pipeline.Snapshot]().FieldByName("Deadline"); ok {
		t.Fatal("Snapshot carries an admission-time deadline")
	}
}

func TestAdmissionCollectsSecretNamesOnly(t *testing.T) {
	yaml := []byte(`actions:
  - name: first
    image: registry.example.com/first:latest
    commands: [make]
    secrets: [DEPLOY_TOKEN, API_KEY]
  - name: second
    image: registry.example.com/second:latest
    commands: [make]
    secrets: [API_KEY, DB_PASSWORD]
`)
	admission := newAdmission(t, &fakeResolver{}, nil)
	result, err := admission.Admit(t.Context(), pipeline.AdmitRequest{
		Scope:          projectScope("tenant-a", "project-a"),
		Definition:     pipeline.Definition{Name: "change", YAML: yaml},
		DefaultTimeout: 5 * time.Minute,
	})
	requireNoError(t, err, "Admit secrets")
	want := []id.SecretName{"DEPLOY_TOKEN", "API_KEY", "DB_PASSWORD"}
	if len(result.Snapshot.SecretNames) != len(want) {
		t.Fatalf("secret names = %v; want %v", result.Snapshot.SecretNames, want)
	}
	for index := range want {
		if result.Snapshot.SecretNames[index] != want[index] {
			t.Fatalf("secret names = %v; want %v", result.Snapshot.SecretNames, want)
		}
	}

	reserved := []byte(`actions:
  - name: first
    image: registry.example.com/first:latest
    secrets: [KODAMA_TOKEN]
`)
	_, err = admission.Admit(t.Context(), pipeline.AdmitRequest{
		Scope:          projectScope("tenant-a", "project-a"),
		Definition:     pipeline.Definition{Name: "change", YAML: reserved},
		DefaultTimeout: 5 * time.Minute,
	})
	requireFaultClass(t, err, fault.InvalidInput)
}

func TestAdmissionRequirements(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	admission := newAdmission(t, &fakeResolver{}, nil)
	requirements := []string{"network", "gpu"}
	result, err := admission.Admit(t.Context(), pipeline.AdmitRequest{
		Scope:          scope,
		Definition:     pipeline.Definition{Name: "inline", YAML: requestOnlyYAML()},
		Inputs:         map[id.InputKey]string{"repository": "repo-1"},
		Requirements:   requirements,
		DefaultTimeout: 5 * time.Minute,
	})
	requireNoError(t, err, "Admit requirements")
	if len(result.Snapshot.Requirements) != 2 || result.Snapshot.Requirements[0] != "network" || result.Snapshot.Requirements[1] != "gpu" {
		t.Fatalf("requirements = %v", result.Snapshot.Requirements)
	}
	requirements[0] = "changed"
	if result.Snapshot.Requirements[0] != "network" {
		t.Fatal("snapshot requirements changed through the caller's slice")
	}

	_, err = admission.Admit(t.Context(), pipeline.AdmitRequest{
		Scope:          scope,
		Definition:     pipeline.Definition{Name: "inline", YAML: requestOnlyYAML()},
		Inputs:         map[id.InputKey]string{"repository": "repo-1"},
		Requirements:   []string{""},
		DefaultTimeout: 5 * time.Minute,
	})
	requireFaultClass(t, err, fault.InvalidInput)
}

func TestAdmissionPushInputSet(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	admission := newAdmission(t, &fakeResolver{}, nil)
	result, err := admission.Admit(t.Context(), pipeline.AdmitRequest{
		Scope:          scope,
		Definition:     pipeline.Definition{Name: "push", YAML: pushPipelineYAML()},
		Inputs:         pushInputs(),
		DefaultTimeout: 5 * time.Minute,
	})
	requireNoError(t, err, "Admit push pipeline")
	for key, value := range pushInputs() {
		if result.Snapshot.Inputs[key] != value {
			t.Fatalf("snapshot input %q = %q; want %q", key, result.Snapshot.Inputs[key], value)
		}
	}

	missing := pushInputs()
	delete(missing, "commit")
	_, err = admission.Admit(t.Context(), pipeline.AdmitRequest{
		Scope:          scope,
		Definition:     pipeline.Definition{Name: "push", YAML: pushPipelineYAML()},
		Inputs:         missing,
		DefaultTimeout: 5 * time.Minute,
	})
	requireFaultClass(t, err, fault.InvalidInput)

	extraDeclared := []byte(`repository: repo-1
service: git-service
on:
  push: [refs/heads/main]
inputs: [repository, ref, commit, previous_commit, extra]
actions:
  - name: test
    image: registry.example.com/test:latest
    commands: [test-commit]
`)
	_, err = admission.Admit(t.Context(), pipeline.AdmitRequest{
		Scope:          scope,
		Definition:     pipeline.Definition{Name: "push", YAML: extraDeclared},
		Inputs:         map[id.InputKey]string{"repository": "repo-1", "ref": "refs/heads/main", "commit": "a", "previous_commit": "b", "extra": "x"},
		DefaultTimeout: 5 * time.Minute,
	})
	requireFaultClass(t, err, fault.InvalidInput)

	patternRef := []byte(`repository: repo-1
service: git-service
on:
  push: [refs/heads/*]
inputs: [repository, ref, commit, previous_commit]
actions:
  - name: test
    image: registry.example.com/test:latest
    commands: [test-commit]
`)
	_, err = pipeline.Decode(patternRef, pipeline.DefaultLimits)
	requireFaultClass(t, err, fault.InvalidInput)
}

// TestAdmissionSnapshotCarriesDefinitionEnvironmentDefaults pins the admitted
// environment defaults: only the pipeline-level literal `env` mapping reaches
// the snapshot, in declaration order, while action environment stays in the
// action declaration for the activation-time merge. Reserved KODAMA_* context
// is supplied at runtime and can never be admitted as a default.
func TestAdmissionSnapshotCarriesDefinitionEnvironmentDefaults(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	admission := newAdmission(t, &fakeResolver{}, nil)
	yaml := []byte(`env:
  LOG_LEVEL: info
  BUILD_MODE: pipeline-default
actions:
  - name: build
    image: registry.example.com/build:latest
    commands: [make]
    env:
      BUILD_MODE: action-release
      ACTION_ONLY: "yes"
`)
	result, err := admission.Admit(t.Context(), pipeline.AdmitRequest{
		Scope:          scope,
		Definition:     pipeline.Definition{Name: "change", YAML: yaml},
		DefaultTimeout: 5 * time.Minute,
	})
	requireNoError(t, err, "Admit pipeline with environment defaults")
	want := []definition.EnvVar{
		{Name: "LOG_LEVEL", Value: "info"},
		{Name: "BUILD_MODE", Value: "pipeline-default"},
	}
	if !reflect.DeepEqual(result.Snapshot.Env, want) {
		t.Fatalf("snapshot env = %+v; want pipeline defaults %+v in declaration order", result.Snapshot.Env, want)
	}
	for _, entry := range result.Snapshot.Env {
		if entry.Name == "ACTION_ONLY" || (entry.Name == "BUILD_MODE" && entry.Value == "action-release") {
			t.Fatalf("snapshot env carries action-level values: %+v", result.Snapshot.Env)
		}
	}

	// The serialized snapshot keeps the same defaults (round trip).
	encoded, err := json.Marshal(result.Snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	var decoded struct {
		Env []definition.EnvVar `json:"env"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if !reflect.DeepEqual(decoded.Env, want) {
		t.Fatalf("serialized env = %+v; want %+v", decoded.Env, want)
	}

	// A definition without a pipeline env mapping admits no defaults.
	noEnv, err := admission.Admit(t.Context(), pipeline.AdmitRequest{
		Scope:          scope,
		Definition:     pipeline.Definition{Name: "inline", YAML: requestOnlyYAML()},
		Inputs:         map[id.InputKey]string{"repository": "repo-1"},
		DefaultTimeout: 5 * time.Minute,
	})
	requireNoError(t, err, "Admit without environment defaults")
	if noEnv.Snapshot.Env != nil {
		t.Fatalf("snapshot env = %+v; want nil without defaults", noEnv.Snapshot.Env)
	}

	// Reserved context cannot be shadowed through the definition.
	reserved := []byte(`env:
  KODAMA_URL: https://attacker.invalid
actions:
  - name: build
    image: registry.example.com/build:latest
    commands: [make]
`)
	_, err = admission.Admit(t.Context(), pipeline.AdmitRequest{
		Scope:          scope,
		Definition:     pipeline.Definition{Name: "change", YAML: reserved},
		DefaultTimeout: 5 * time.Minute,
	})
	requireFaultClass(t, err, fault.InvalidInput)
}

// TestAdmissionPreservesConversationPresence pins the nil-versus-empty rule at
// the admission boundary: an admitted empty conversation stays present (non-nil)
// while an absent one stays absent, including in the serialized snapshot.
func TestAdmissionPreservesConversationPresence(t *testing.T) {
	admission := newAdmission(t, &fakeResolver{}, nil)
	base := pipeline.AdmitRequest{
		Scope:          projectScope("tenant-a", "project-a"),
		Definition:     pipeline.Definition{Name: "inline", YAML: requestOnlyYAML()},
		Inputs:         map[id.InputKey]string{"repository": "repo-1"},
		DefaultTimeout: 5 * time.Minute,
	}

	empty := base
	empty.Task = pipeline.TaskContext{TaskID: "task-1", Conversation: []byte{}}
	result, err := admission.Admit(t.Context(), empty)
	requireNoError(t, err, "Admit admitted empty conversation")
	if !result.Snapshot.HasConversation() {
		t.Fatal("admitted empty conversation is not reported as present")
	}
	if result.Snapshot.ConversationBytes() == nil {
		t.Fatal("admitted empty conversation collapsed to absent")
	}
	if len(result.Snapshot.ConversationBytes()) != 0 {
		t.Fatalf("admitted empty conversation = %q; want empty", result.Snapshot.ConversationBytes())
	}
	encoded, err := json.Marshal(result.Snapshot)
	if err != nil {
		t.Fatalf("marshal empty-conversation snapshot: %v", err)
	}
	if !bytes.Contains(encoded, []byte(`"conversation":""`)) {
		t.Fatalf("empty conversation serialized as %s; want an empty string", encoded)
	}

	// An absent conversation is not turned into an admitted empty one.
	absent := base
	absent.Task = pipeline.TaskContext{TaskID: "task-1"}
	result, err = admission.Admit(t.Context(), absent)
	requireNoError(t, err, "Admit absent conversation")
	if result.Snapshot.HasConversation() || result.Snapshot.ConversationBytes() != nil {
		t.Fatalf("absent conversation = %q; want nil", result.Snapshot.ConversationBytes())
	}
	encoded, err = json.Marshal(result.Snapshot)
	if err != nil {
		t.Fatalf("marshal absent-conversation snapshot: %v", err)
	}
	if !bytes.Contains(encoded, []byte(`"conversation":null`)) {
		t.Fatalf("absent conversation serialized as %s; want null", encoded)
	}

	// A task-free admission carries no conversation even if stray bytes arrive.
	taskFree := base
	taskFree.Task = pipeline.TaskContext{Conversation: []byte{}}
	result, err = admission.Admit(t.Context(), taskFree)
	requireNoError(t, err, "Admit task-free with stray conversation")
	if result.Snapshot.HasConversation() || result.Snapshot.ConversationBytes() != nil {
		t.Fatalf("task-free conversation = %q; want nil", result.Snapshot.ConversationBytes())
	}
}

// TestAdmissionInlineBypassesConfiguredReader pins the inline selection
// boundary: an inline document is admitted from its own bytes and never
// consults the configured stored-definition reader. Every reader method fails
// the test, so an accidental fallback cannot hide behind a zero-value answer.
func TestAdmissionInlineBypassesConfiguredReader(t *testing.T) {
	reader := &fakeReader{
		getFn: func(context.Context, tenancy.Scope, pipeline.PipelineID) (pipeline.Definition, error) {
			t.Fatal("inline admission called Reader.Get")
			return pipeline.Definition{}, nil
		},
		byNameFn: func(context.Context, tenancy.Scope, string) (pipeline.Definition, error) {
			t.Fatal("inline admission called Reader.ByName")
			return pipeline.Definition{}, nil
		},
		listFn: func(context.Context, tenancy.Scope) ([]pipeline.Definition, error) {
			t.Fatal("inline admission called Reader.List")
			return nil, nil
		},
	}
	yaml := validPipelineYAML()
	admission := newAdmission(t, &fakeResolver{}, reader)
	result, err := admission.Admit(t.Context(), pipeline.AdmitRequest{
		Scope: projectScope("tenant-a", "project-a"),
		// Inline admission needs neither a pipeline name nor a caller-supplied
		// version; the version is normalized away rather than echoed.
		Definition:     pipeline.Definition{YAML: yaml, Version: 9},
		Inputs:         map[id.InputKey]string{"repository": "repo-1"},
		DefaultTimeout: 5 * time.Minute,
	})
	requireNoError(t, err, "Admit inline definition with a configured reader")

	if get, byName, list := reader.counts(); get != 0 || byName != 0 || list != 0 {
		t.Fatalf("inline admission consulted the reader: get=%d byName=%d list=%d", get, byName, list)
	}
	if result.PipelineID != "" || result.PipelineVersion != 0 {
		t.Fatalf("inline admission identity = %q version %d; want empty/0", result.PipelineID, result.PipelineVersion)
	}
	if result.Snapshot.YAMLSHA256 != sha256.Sum256(yaml) {
		t.Fatalf("inline snapshot hash = %x; want %x", result.Snapshot.YAMLSHA256, sha256.Sum256(yaml))
	}
	if !bytes.Equal(result.Snapshot.YAMLBytes(), yaml) {
		t.Fatalf("inline snapshot yaml = %q; want %q", result.Snapshot.YAMLBytes(), yaml)
	}
	if result.Snapshot.TaskID != "" || result.Snapshot.HasConversation() || result.Snapshot.ConversationBytes() != nil {
		t.Fatalf("task-free inline snapshot carries task context: task=%q conversation=%q", result.Snapshot.TaskID, result.Snapshot.ConversationBytes())
	}
	if got := result.Snapshot.Inputs["repository"]; got != "repo-1" {
		t.Fatalf("inline snapshot inputs = %v", result.Snapshot.Inputs)
	}
	if len(result.Snapshot.Images) != 1 || result.Snapshot.Images[0].Digest != digestFor("registry.example.com/build:latest") {
		t.Fatalf("inline snapshot images = %+v; want one pinned image", result.Snapshot.Images)
	}
	requireAction(t, result.Actions, 0, "build", definition.ActionKindContainer, 10*time.Minute)
	requireAction(t, result.Actions, 1, "review", definition.ActionKindRequest, 72*time.Hour)
}

// TestAdmissionInlineYAMLByteBoundary pins the exact inline byte bound and the
// layer that enforces it. pipeline.DefaultLimits.MaxYAMLBytes is the admission
// bound (the public api.MaxInlineYAMLBytes mirrors it), and Admit must refuse an
// oversized document before it resolves any image.
func TestAdmissionInlineYAMLByteBoundary(t *testing.T) {
	if api.MaxInlineYAMLBytes != pipeline.DefaultLimits.MaxYAMLBytes {
		t.Fatalf("public inline bound = %d; admission bound = %d", api.MaxInlineYAMLBytes, pipeline.DefaultLimits.MaxYAMLBytes)
	}
	bound := pipeline.DefaultLimits.MaxYAMLBytes
	if bound != 1<<20 {
		t.Fatalf("inline admission bound = %d bytes; want the declared 1 MiB bound", bound)
	}
	base := validPipelineYAML()
	padded := func(size int) []byte {
		if size < len(base)+2 {
			t.Fatalf("cannot pad %d bytes to %d", len(base), size)
		}
		out := append([]byte(nil), base...)
		out = append(out, '#')
		out = append(out, bytes.Repeat([]byte("x"), size-len(base)-2)...)
		out = append(out, '\n')
		if len(out) != size {
			t.Fatalf("padded document = %d bytes; want %d", len(out), size)
		}
		return out
	}
	scope := projectScope("tenant-a", "project-a")
	inputs := map[id.InputKey]string{"repository": "repo-1"}

	exact := padded(bound)
	resolver := &fakeResolver{}
	result, err := newAdmission(t, resolver, nil).Admit(t.Context(), pipeline.AdmitRequest{
		Scope:          scope,
		Definition:     pipeline.Definition{YAML: exact},
		Inputs:         inputs,
		DefaultTimeout: 5 * time.Minute,
	})
	requireNoError(t, err, "Admit inline document at the byte bound")
	if len(result.Snapshot.YAMLBytes()) != bound {
		t.Fatalf("admitted yaml = %d bytes; want %d", len(result.Snapshot.YAMLBytes()), bound)
	}
	if result.Snapshot.YAMLSHA256 != sha256.Sum256(exact) {
		t.Fatalf("admitted hash = %x; want %x", result.Snapshot.YAMLSHA256, sha256.Sum256(exact))
	}
	if resolver.callCount() != 1 {
		t.Fatalf("resolver calls at the byte bound = %d; want 1", resolver.callCount())
	}

	over := padded(bound + 1)
	overResolver := &fakeResolver{}
	_, err = newAdmission(t, overResolver, nil).Admit(t.Context(), pipeline.AdmitRequest{
		Scope:          scope,
		Definition:     pipeline.Definition{YAML: over},
		Inputs:         inputs,
		DefaultTimeout: 5 * time.Minute,
	})
	requireFaultClass(t, err, fault.InvalidInput)
	if overResolver.callCount() != 0 {
		t.Fatalf("oversized inline document reached image resolution (%d calls); the admission byte bound must refuse first", overResolver.callCount())
	}
}

// TestAdmissionInlineResolvesLikeStoredAndPreservesExactBytes pins inline
// parity with the stored path: the same document resolves the same inputs,
// pinned images and default/explicit timeouts, while the inline result keeps no
// pipeline identity. The CRLF fixture proves the snapshot hashes the exact
// supplied bytes (no canonicalization), and mutating the caller's slices after
// admission cannot change the inline snapshot.
func TestAdmissionInlineResolvesLikeStoredAndPreservesExactBytes(t *testing.T) {
	yaml := []byte("inputs: [repository]\r\n" +
		"# exact bytes are part of the admitted content\r\n" +
		"actions:\r\n" +
		"  - name: build\r\n" +
		"    image: registry.example.com/build:latest\r\n" +
		"    commands: [make]\r\n" +
		"    timeout: 1m\r\n" +
		"  - name: review\r\n" +
		"    request: role/reviewer\r\n" +
		"    subject:\r\n" +
		"      resource:\r\n" +
		"        from: inputs.repository\r\n")
	wantYAML := append([]byte(nil), yaml...)
	scope := projectScope("tenant-a", "project-a")
	inputs := map[id.InputKey]string{"repository": "repo-1"}
	requirements := []string{"network"}

	reader := &fakeReader{}
	reader.getFn = func(context.Context, tenancy.Scope, pipeline.PipelineID) (pipeline.Definition, error) {
		return pipeline.Definition{ID: "pipeline-1", Name: "change", YAML: yaml, Version: 3}, nil
	}
	admission := newAdmission(t, &fakeResolver{}, reader)

	stored, err := admission.Admit(t.Context(), pipeline.AdmitRequest{
		Scope:          scope,
		Definition:     pipeline.Definition{ID: "pipeline-1"},
		Inputs:         inputs,
		Requirements:   requirements,
		DefaultTimeout: 5 * time.Minute,
	})
	requireNoError(t, err, "Admit stored definition")
	inline, err := admission.Admit(t.Context(), pipeline.AdmitRequest{
		Scope:          scope,
		Definition:     pipeline.Definition{Name: "inline", YAML: yaml},
		Inputs:         inputs,
		Requirements:   requirements,
		DefaultTimeout: 5 * time.Minute,
	})
	requireNoError(t, err, "Admit the same definition inline")

	if get, _, _ := reader.counts(); get != 1 {
		t.Fatalf("reader get calls = %d; want only the stored admission", get)
	}
	if stored.PipelineID != "pipeline-1" || stored.PipelineVersion != 3 {
		t.Fatalf("stored identity = %q version %d", stored.PipelineID, stored.PipelineVersion)
	}
	if inline.PipelineID != "" || inline.PipelineVersion != 0 {
		t.Fatalf("inline identity = %q version %d; want empty/0", inline.PipelineID, inline.PipelineVersion)
	}

	wantHash := sha256.Sum256(wantYAML)
	for name, result := range map[string]pipeline.AdmissionResult{"stored": stored, "inline": inline} {
		if result.Snapshot.YAMLSHA256 != wantHash {
			t.Fatalf("%s snapshot hash = %x; want the exact CRLF bytes %x", name, result.Snapshot.YAMLSHA256, wantHash)
		}
		if !bytes.Equal(result.Snapshot.YAMLBytes(), wantYAML) {
			t.Fatalf("%s snapshot yaml = %q; want exact supplied bytes", name, result.Snapshot.YAMLBytes())
		}
	}
	if !bytes.Contains(inline.Snapshot.YAMLBytes(), []byte("\r\n")) {
		t.Fatal("inline snapshot canonicalized the CRLF bytes")
	}
	if !reflect.DeepEqual(stored.Actions, inline.Actions) {
		t.Fatalf("stored actions = %+v; inline actions = %+v", stored.Actions, inline.Actions)
	}
	requireActions(t, inline.Actions, 2)
	requireAction(t, inline.Actions, 0, "build", definition.ActionKindContainer, time.Minute)
	requireAction(t, inline.Actions, 1, "review", definition.ActionKindRequest, 5*time.Minute)
	if len(inline.Snapshot.Images) != 1 || inline.Snapshot.Images[0].Digest != digestFor("registry.example.com/build:latest") {
		t.Fatalf("inline snapshot images = %+v", inline.Snapshot.Images)
	}
	if !reflect.DeepEqual(stored.Snapshot.Inputs, inline.Snapshot.Inputs) || inline.Snapshot.Inputs["repository"] != "repo-1" {
		t.Fatalf("inline inputs = %v; stored inputs = %v", inline.Snapshot.Inputs, stored.Snapshot.Inputs)
	}
	if !reflect.DeepEqual(stored.Snapshot.Requirements, inline.Snapshot.Requirements) {
		t.Fatalf("inline requirements = %v; stored requirements = %v", inline.Snapshot.Requirements, stored.Snapshot.Requirements)
	}

	// The inline snapshot owns its copies: caller edits after admission are
	// invisible, including to the hash over the exact admitted bytes.
	yaml[0] = 'X'
	inputs["repository"] = "repo-2"
	requirements[0] = "gpu"
	if !bytes.Equal(inline.Snapshot.YAMLBytes(), wantYAML) {
		t.Fatalf("inline snapshot yaml changed through the caller's slice: %q", inline.Snapshot.YAMLBytes())
	}
	if inline.Snapshot.YAMLSHA256 != wantHash {
		t.Fatalf("inline snapshot hash = %x; want %x", inline.Snapshot.YAMLSHA256, wantHash)
	}
	if inline.Snapshot.Inputs["repository"] != "repo-1" {
		t.Fatalf("inline snapshot inputs changed: %v", inline.Snapshot.Inputs)
	}
	if len(inline.Snapshot.Requirements) != 1 || inline.Snapshot.Requirements[0] != "network" {
		t.Fatalf("inline snapshot requirements changed: %v", inline.Snapshot.Requirements)
	}
}

func TestAdmissionRefusesContainerActionWithoutValidCommands(t *testing.T) {
	scope := projectScope("tenant-a", "project-a")
	cases := []struct {
		name string
		yaml []byte
	}{
		{
			name: "omitted commands",
			yaml: []byte(`actions:
  - name: build
    image: registry.example.com/build:latest
`),
		},
		{
			name: "empty commands",
			yaml: []byte(`actions:
  - name: build
    image: registry.example.com/build:latest
    commands: []
`),
		},
		{
			name: "command with NUL byte",
			yaml: []byte("actions:\n  - name: build\n    image: registry.example.com/build:latest\n    commands:\n      - \"echo\\x00bad\"\n"),
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			resolver := &fakeResolver{}
			admission := newAdmission(t, resolver, nil)
			_, err := admission.Admit(t.Context(), pipeline.AdmitRequest{
				Scope:          scope,
				Definition:     pipeline.Definition{Name: "change", YAML: testCase.yaml},
				DefaultTimeout: 5 * time.Minute,
			})
			requireFaultClass(t, err, fault.InvalidInput)
			if resolver.callCount() != 0 {
				t.Fatalf("resolver calls = %d; invalid commands must refuse before image resolution", resolver.callCount())
			}
		})
	}
}
