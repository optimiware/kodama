package execution

import (
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/pipeline"
)

func TestArtifactFilesResolveAncestorAndInputDescriptors(t *testing.T) {
	producer := &StepRun{ID: "build", Status: StepSucceeded, ArtifactRefs: map[string]runtimev1.ArtifactInput{
		"bundle": {Name: "bundle", ArtifactID: "art_bundle", Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}}
	consumer := &StepRun{Spec: pipeline.Step{ID: "deploy", ArtifactFiles: map[string]string{
		"bundle": "${{ steps.build.outputs.bundle }}",
		"config": "${{ inputs.config }}",
	}}}
	agg := &Aggregate{Steps: []*StepRun{producer, consumer}}
	snap := Snapshot{InputArtifacts: []runtimev1.ArtifactInput{{
		Name: "config", ArtifactID: "art_config", Digest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}}}
	got, err := resolveArtifactFiles(agg, consumer, snap)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "bundle" || got[0].ArtifactID != "art_bundle" ||
		got[1].Name != "config" || got[1].ArtifactID != "art_config" {
		t.Fatalf("descriptors = %+v", got)
	}
}

func TestArtifactFilesRefuseAnUnsuccessfulProducer(t *testing.T) {
	producer := &StepRun{ID: "build", Status: StepRunning, ArtifactRefs: map[string]runtimev1.ArtifactInput{
		"bundle": {Name: "bundle", ArtifactID: "art_bundle", Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}}
	consumer := &StepRun{Spec: pipeline.Step{ID: "deploy", ArtifactFiles: map[string]string{"bundle": "${{ steps.build.outputs.bundle }}"}}}
	agg := &Aggregate{Steps: []*StepRun{producer, consumer}}
	if _, err := resolveArtifactFiles(agg, consumer, Snapshot{}); err == nil {
		t.Fatal("accepted an artifact from an unfinished producer")
	}
}
