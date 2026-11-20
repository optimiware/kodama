package executionstore

import (
	"bytes"
	"testing"

	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/kernel"
	"github.com/optimiware/kodema/internal/kernel/definition"
	"github.com/optimiware/kodema/internal/shell/pipeline"
	"github.com/optimiware/kodema/internal/shell/store/units/execution/snapshotdoc"
)

func TestDecodeSnapshotMapsTheSharedDocument(t *testing.T) {
	yamlBytes := []byte{0x00, 0xff, 'n', 'a', 'm', 'e', ':', ' ', 'x', '\n'}
	snapshot, err := pipeline.NewSnapshot(pipeline.SnapshotInput{
		YAML:         yamlBytes,
		Inputs:       map[id.InputKey]string{"a": "1"},
		Env:          []definition.EnvVar{{Name: "MODE", Value: "fast"}},
		Requirements: []string{"process", "container"},
		Conversation: []byte{0x00, 0x01, 0xfe},
		TaskID:       "task-1",
	})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	document, err := snapshotdoc.Encode(snapshotdoc.Admitted{
		Snapshot: snapshot,
		Actions:  []pipeline.AdmittedAction{containerAction()},
	})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	decoded, err := decodeSnapshot(document)
	if err != nil {
		t.Fatalf("decodeSnapshot: %v", err)
	}
	if !bytes.Equal(decoded.YAML(), yamlBytes) {
		t.Fatalf("yaml = %v; want exact %v", decoded.YAML(), yamlBytes)
	}
	if decoded.YAMLDigest() != snapshot.YAMLSHA256 {
		t.Fatalf("digest did not round-trip")
	}
	if decoded.Inputs()["a"] != "1" {
		t.Fatalf("inputs = %v", decoded.Inputs())
	}
	if got := decoded.Runtime().Capabilities; len(got) != 2 || got[1] != "container" {
		t.Fatalf("runtime = %v", got)
	}
	if got := decoded.Pipeline().Env; len(got) != 1 || got[0].Name != "MODE" || got[0].Value != "fast" {
		t.Fatalf("env = %v; want the admitted environment default", got)
	}
	task, present := decoded.Task()
	if !present || !bytes.Equal(task, []byte{0x00, 0x01, 0xfe}) {
		t.Fatalf("task = %v present=%v; want exact bytes", task, present)
	}
	if decoded.TaskID() != "task-1" {
		t.Fatalf("task id = %q", decoded.TaskID())
	}
	actions := decoded.Pipeline().Actions
	if len(actions) != 1 || actions[0].Container == nil || actions[0].Name != "build" {
		t.Fatalf("pipeline actions = %+v", actions)
	}
}

func TestDecodeSnapshotPreservesPresentEmptyTask(t *testing.T) {
	empty, err := pipeline.NewSnapshot(pipeline.SnapshotInput{YAML: []byte("name: a\n"), Conversation: []byte{}})
	if err != nil {
		t.Fatalf("NewSnapshot present-empty: %v", err)
	}
	document, err := snapshotdoc.Encode(snapshotdoc.Admitted{Snapshot: empty, Actions: []pipeline.AdmittedAction{containerAction()}})
	if err != nil {
		t.Fatalf("encode present-empty: %v", err)
	}
	decoded, err := decodeSnapshot(document)
	if err != nil {
		t.Fatalf("decode present-empty: %v", err)
	}
	task, present := decoded.Task()
	if !present || task == nil || len(task) != 0 {
		t.Fatalf("present-empty task did not round-trip: present=%v task=%v", present, task)
	}
}

func TestDecodeSnapshotPreservesAbsentTask(t *testing.T) {
	absent, err := pipeline.NewSnapshot(pipeline.SnapshotInput{YAML: []byte("name: a\n")})
	if err != nil {
		t.Fatalf("NewSnapshot absent: %v", err)
	}
	document, err := snapshotdoc.Encode(snapshotdoc.Admitted{Snapshot: absent, Actions: []pipeline.AdmittedAction{containerAction()}})
	if err != nil {
		t.Fatalf("encode absent: %v", err)
	}
	decoded, err := decodeSnapshot(document)
	if err != nil {
		t.Fatalf("decode absent: %v", err)
	}
	if _, present := decoded.Task(); present {
		t.Fatal("an absent task must not decode as present")
	}
}

func TestDecodeSnapshotRejectsMalformedData(t *testing.T) {
	for _, data := range [][]byte{nil, []byte("{"), []byte(`{"version":1}`)} {
		if _, err := decodeSnapshot(data); err == nil {
			t.Fatalf("decodeSnapshot(%q) succeeded; want a fault", data)
		} else if class, ok := fault.ClassOf(err); !ok || class != fault.InternalFailure {
			t.Fatalf("class = %v; want internal_failure", err)
		}
	}
}

func TestCleanupIdentityIsStableAndDistinct(t *testing.T) {
	cleanup := cleanupID("exec-1")
	if cleanup == cleanupID("exec-2") {
		t.Fatal("cleanup identities collide")
	}
	if cleanup != cleanupID("exec-1") {
		t.Fatal("cleanup identity is not stable")
	}
}

func TestDecodeStopRoundTripsEvidence(t *testing.T) {
	encoded, err := encodeStop(kernel.ConfirmedStop("stopped"))
	if err != nil {
		t.Fatalf("encodeStop: %v", err)
	}
	decoded, err := decodeStop(encoded)
	if err != nil {
		t.Fatalf("decodeStop: %v", err)
	}
	if decoded != kernel.ConfirmedStop("stopped") {
		t.Fatalf("stop = %+v; want %+v", decoded, kernel.ConfirmedStop("stopped"))
	}
	absent, err := encodeStop(kernel.StopEvidence{})
	if err != nil || absent != nil {
		t.Fatalf("zero stop = %v err=%v; want nil", absent, err)
	}
}

func TestEncodeDecodeAnswerProvenanceRoundTrips(t *testing.T) {
	cases := []struct {
		name       string
		note       string
		provenance []byte
		rawExact   bool
	}{
		{name: "empty", note: "", provenance: nil},
		{name: "legacy raw bytes", note: "", provenance: []byte{0x00, 0x01, 0xfe}, rawExact: true},
		{name: "note only", note: "Security checklist incomplete", provenance: nil},
		{name: "note and provenance", note: "LGTM", provenance: []byte{0xde, 0xad, 0xbe, 0xef}},
		{name: "raw bytes starting with magic prefix", note: "", provenance: append(append([]byte(nil), answerMetaMagic...), []byte("raw")...)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := encodeAnswerProvenance(tc.note, tc.provenance)
			if err != nil {
				t.Fatalf("encodeAnswerProvenance: %v", err)
			}
			if tc.rawExact && string(encoded) != string(tc.provenance) {
				t.Fatalf("encoded raw bytes = %v; want exact %v", encoded, tc.provenance)
			}
			note, prov, err := decodeAnswerProvenance(encoded)
			if err != nil {
				t.Fatalf("decodeAnswerProvenance: %v", err)
			}
			if note != tc.note || string(prov) != string(tc.provenance) {
				t.Fatalf("decoded = (%q, %v); want (%q, %v)", note, prov, tc.note, tc.provenance)
			}
		})
	}

	malformed := append(append([]byte(nil), answerMetaMagic...), []byte("{not-json")...)
	if _, _, err := decodeAnswerProvenance(malformed); err == nil {
		t.Fatal("decodeAnswerProvenance accepted malformed envelope")
	} else if class, ok := fault.ClassOf(err); !ok || class != fault.InternalFailure {
		t.Fatalf("malformed envelope class = %v; want internal_failure", err)
	}
}
