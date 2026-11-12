//go:build integration

package services

// Every shipped pipeline example and the documented subject example pass the
// served admission schema, and every shipped invalid fixture is refused by it.
//
// This is the server-side half of example validation: decoding a pipeline with
// the internal parser proves the static grammar; applying it through the served
// pipeline.apply operation proves the composition accepts the exact documented
// bytes. The documented subject block is embedded verbatim, so its flow-style
// reference syntax is validated rather than paraphrased.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/test/support/factory"
)

// TestExamplePipelinesPassServedAdmission applies every shipped pipeline
// example through the served pipeline.apply operation and reads back the exact
// committed bytes.
func TestExamplePipelinesPassServedAdmission(t *testing.T) {
	harness := factory.Start(t)
	for _, name := range examplePipelineFiles(t) {
		t.Run(name, func(t *testing.T) {
			rel := "examples/pipelines/" + name
			document := exampleRead(t, rel)
			pipelineName := exampleStem(name)
			applied := harness.MustREST(harness.ProjectToken, "pipeline.apply", api.PipelineApplyRequest{
				ProjectID: factory.ProjectID,
				Name:      pipelineName,
				YAML:      document,
			}, nil, "example-apply-"+name)
			var result api.PipelineApplyResult
			harness.Decode(applied, &result)
			if result.PipelineID == "" {
				t.Fatalf("%s applied with no pipeline_id", rel)
			}
			if result.Version == 0 {
				t.Fatalf("%s applied with no committed version", rel)
			}

			stored := harness.MustREST(harness.ProjectToken, "pipeline.read", api.PipelineReadRequest{
				ProjectID:  factory.ProjectID,
				PipelineID: result.PipelineID,
			}, nil, "")
			var read api.PipelineReadResult
			harness.Decode(stored, &read)
			if read.Name != pipelineName {
				t.Fatalf("%s stored as pipeline name %q; want %q", rel, read.Name, pipelineName)
			}
			if !bytes.Equal(read.YAML, document) {
				t.Fatalf("%s was not stored byte for byte through the served admission", rel)
			}
		})
	}
}

// TestExampleInvalidFixturesAreRefusedByServedAdmission applies every shipped
// invalid fixture through the served pipeline.apply operation and requires the
// public invalid_input refusal. These remain testdata, never shipped examples.
func TestExampleInvalidFixturesAreRefusedByServedAdmission(t *testing.T) {
	harness := factory.Start(t)
	for _, name := range exampleInvalidFixtureFiles(t) {
		t.Run(name, func(t *testing.T) {
			rel := "examples/testdata/invalid/" + name
			_, publicError := harness.REST(harness.ProjectToken, "pipeline.apply", api.PipelineApplyRequest{
				ProjectID: factory.ProjectID,
				Name:      "invalid-" + exampleStem(name),
				YAML:      exampleRead(t, rel),
			}, nil, "example-refuse-"+name)
			if publicError.Code == "" {
				t.Fatalf("%s was admitted by the served API; want an invalid_input refusal", rel)
			}
			if publicError.Code != api.CodeInvalidInput {
				t.Fatalf("%s refusal code = %q; want %q (%s)", rel, publicError.Code, api.CodeInvalidInput, publicError.Message)
			}
		})
	}
}

// TestDocumentedSubjectExamplePassesServedAdmission embeds the subject example
// from docs/specs/03-admission.md verbatim and admits the pipeline through the
// served edge.
func TestDocumentedSubjectExamplePassesServedAdmission(t *testing.T) {
	harness := factory.Start(t)
	subject := exampleFencedBlock(t, "docs/specs/03-admission.md", "yaml")
	for _, marker := range []string{
		"operation: git.ref.update",
		"resource: {from: inputs.repository}",
		"candidate: {from: build.outputs.candidate}",
	} {
		if !strings.Contains(subject, marker) {
			t.Fatalf("docs/specs/03-admission.md subject example no longer contains %q:\n%s", marker, subject)
		}
	}

	build := exampleDecodedPipeline(t, "examples/pipelines/build.yaml", exampleRead(t, "examples/pipelines/build.yaml"))
	image := build.Actions[0].Container.Image
	document := strings.Join([]string{
		"inputs:",
		"  - repository",
		"actions:",
		"  - name: build",
		"    image: " + image,
		"    commands:",
		"      - build",
		"    outputs:",
		"      - candidate",
		"  - name: review",
		"    request: role/reviewer",
		"    subject:",
		exampleIndent(subject, "      "),
		"    outputs:",
		"      - approved",
		"",
	}, "\n")

	applied := harness.MustREST(harness.ProjectToken, "pipeline.apply", api.PipelineApplyRequest{
		ProjectID: factory.ProjectID,
		Name:      "documented-subject-example",
		YAML:      []byte(document),
	}, nil, "example-apply-documented-subject")
	var result api.PipelineApplyResult
	harness.Decode(applied, &result)

	stored := harness.MustREST(harness.ProjectToken, "pipeline.read", api.PipelineReadRequest{
		ProjectID:  factory.ProjectID,
		PipelineID: result.PipelineID,
	}, nil, "")
	var read api.PipelineReadResult
	harness.Decode(stored, &read)
	decoded := exampleDecodedPipeline(t, "docs/specs/03-admission.md (rendered)", read.YAML)

	var review *definition.Action
	for index := range decoded.Actions {
		if decoded.Actions[index].Name == "review" {
			review = &decoded.Actions[index]
			break
		}
	}
	if review == nil || review.Request == nil {
		t.Fatalf("the documented subject example did not admit a request action: %+v", decoded.Actions)
	}
	fields := review.Request.Subject.Fields()
	if got := fields["operation"]; got.Kind != definition.SubjectLiteral || got.Literal != "git.ref.update" {
		t.Fatalf("documented subject operation = %+v; want literal git.ref.update", got)
	}
	resource := fields["resource"]
	if resource.Kind != definition.SubjectReference || !resource.Reference.Input || string(resource.Reference.Key) != "repository" {
		t.Fatalf("documented subject resource = %+v; want a reference to inputs.repository", resource)
	}
	candidate := fields["candidate"]
	if candidate.Kind != definition.SubjectReference || candidate.Reference.Input ||
		string(candidate.Reference.Action) != "build" || string(candidate.Reference.Key) != "candidate" {
		t.Fatalf("documented subject candidate = %+v; want a reference to build.outputs.candidate", candidate)
	}
}

// exampleIndent prefixes every non-empty line of one block.
func exampleIndent(block, prefix string) string {
	lines := strings.Split(block, "\n")
	for index := range lines {
		if lines[index] != "" {
			lines[index] = prefix + lines[index]
		}
	}
	return strings.Join(lines, "\n")
}
