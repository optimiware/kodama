package snapshotdoc_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/store/units/execution/snapshotdoc"
)

// TestInlineSnapshotOmitsPipelineBinding pins the pipeline-less document
// shape: an inline admission encodes no pipeline_id/pipeline_version members,
// decodes back to the empty binding, and still round-trips the exact admitted
// YAML, digest and ordered actions. The stored binding keeps its members.
func TestInlineSnapshotOmitsPipelineBinding(t *testing.T) {
	snapshot, err := pipeline.NewSnapshot(pipeline.SnapshotInput{
		YAML:   []byte("name: inline\non: push\n"),
		Inputs: map[id.InputKey]string{"target": "linux"},
	})
	if err != nil {
		t.Fatalf("pipeline.NewSnapshot: %v", err)
	}
	actions := admittedActions()
	inline := snapshotdoc.Admitted{
		Snapshot: snapshot,
		Actions:  actions,
	}

	encoded, err := snapshotdoc.Encode(inline)
	if err != nil {
		t.Fatalf("Encode inline: %v", err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatalf("decode inline document: %v", err)
	}
	if _, present := document["pipeline_id"]; present {
		t.Fatal("inline document carries pipeline_id")
	}
	if _, present := document["pipeline_version"]; present {
		t.Fatal("inline document carries pipeline_version")
	}

	decoded, err := snapshotdoc.Decode(encoded)
	if err != nil {
		t.Fatalf("Decode inline: %v", err)
	}
	if decoded.PipelineID != "" || decoded.PipelineVersion != 0 {
		t.Fatalf("decoded inline binding = %q/%d; want empty/0", decoded.PipelineID, decoded.PipelineVersion)
	}
	if !bytes.Equal(decoded.Snapshot.YAMLBytes(), snapshot.YAMLBytes()) {
		t.Fatalf("decoded inline yaml = %q; want %q", decoded.Snapshot.YAMLBytes(), snapshot.YAMLBytes())
	}
	if decoded.Snapshot.YAMLSHA256 != snapshot.YAMLSHA256 {
		t.Fatal("decoded inline snapshot digest changed")
	}
	if !reflect.DeepEqual(decoded.Actions, actions) {
		t.Fatalf("decoded inline actions = %+v; want %+v", decoded.Actions, actions)
	}

	stored, err := snapshotdoc.Encode(snapshotdoc.Admitted{
		PipelineID:      "pipe-1",
		PipelineVersion: 3,
		Snapshot:        snapshot,
		Actions:         actions,
	})
	if err != nil {
		t.Fatalf("Encode stored: %v", err)
	}
	var storedDocument map[string]json.RawMessage
	if err := json.Unmarshal(stored, &storedDocument); err != nil {
		t.Fatalf("decode stored document: %v", err)
	}
	if _, present := storedDocument["pipeline_id"]; !present {
		t.Fatal("stored document lost pipeline_id")
	}
	if _, present := storedDocument["pipeline_version"]; !present {
		t.Fatal("stored document lost pipeline_version")
	}
}
