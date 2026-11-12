package snapshotdoc_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/store/units/execution/snapshotdoc"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

func fullSnapshot(t *testing.T, conversation []byte) pipeline.Snapshot {
	t.Helper()
	snapshot, err := pipeline.NewSnapshot(pipeline.SnapshotInput{
		YAML:         []byte{0x00, 0xff, 'n', 'a', 'm', 'e', ':', ' ', 'x', '\n'},
		Inputs:       map[id.InputKey]string{"target": "linux", "retries": "3"},
		Env:          []definition.EnvVar{{Name: "MODE", Value: "fast"}},
		Images:       []pipeline.PinnedImage{{Reference: "registry.example/build", Digest: "sha256:" + "a"}},
		SecretNames:  []id.SecretName{"DEPLOY_KEY"},
		Requirements: []string{"process", "container"},
		TaskID:       "task-1",
		Conversation: conversation,
		References:   []string{"ref-1"},
		Metadata:     map[string]string{"origin": "task"},
	})
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	return snapshot
}

func admittedActions() []pipeline.AdmittedAction {
	return []pipeline.AdmittedAction{
		{
			Index:   0,
			Name:    "build",
			Kind:    definition.ActionKindContainer,
			Timeout: 90 * time.Second,
			Container: &definition.Container{
				Image:    "registry.example/build@sha256:" + "a",
				Commands: []string{"make build"},
				Env:      []definition.EnvVar{{Name: "CGO", Value: "0"}},
				Secrets:  []id.SecretName{"DEPLOY_KEY"},
				Outputs:  []id.OutputKey{"artifact"},
			},
		},
		{
			Index:   1,
			Name:    "approve",
			Kind:    definition.ActionKindRequest,
			Timeout: 60 * time.Second,
			Request: &definition.Request{
				Assignment: "role/reviewer",
				Subject: definition.NewSubject(map[string]definition.SubjectValue{
					"artifact": {Kind: definition.SubjectReference, Reference: definition.Reference{Action: "build", Key: "artifact"}},
				}),
				Outputs: []id.OutputKey{"decision"},
			},
		},
	}
}

// TestDocumentRoundTripsTheFullAdmittedSnapshot asserts every admitted field,
// both action definitions and the pipeline revision survive Encode/Decode.
func TestDocumentRoundTripsTheFullAdmittedSnapshot(t *testing.T) {
	snapshot := fullSnapshot(t, []byte{0x00, 0x01, 0xfe})
	admitted := snapshotdoc.Admitted{
		PipelineID:      "pipe-1",
		PipelineVersion: 7,
		Snapshot:        snapshot,
		Actions:         admittedActions(),
	}
	encoded, err := snapshotdoc.Encode(admitted)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	decoded, err := snapshotdoc.Decode(encoded)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if decoded.PipelineID != "pipe-1" || decoded.PipelineVersion != 7 {
		t.Fatalf("pipeline binding = %q/%d; want pipe-1/7", decoded.PipelineID, decoded.PipelineVersion)
	}
	if !reflect.DeepEqual(decoded.Snapshot, snapshot) {
		t.Fatalf("snapshot did not round-trip:\n got %#v\nwant %#v", decoded.Snapshot, snapshot)
	}
	if !bytes.Equal(decoded.Snapshot.YAMLBytes(), snapshot.YAMLBytes()) {
		t.Fatalf("yaml = %v; want exact %v", decoded.Snapshot.YAMLBytes(), snapshot.YAMLBytes())
	}
	if !reflect.DeepEqual(decoded.Actions, admittedActions()) {
		t.Fatalf("actions did not round-trip:\n got %#v\nwant %#v", decoded.Actions, admittedActions())
	}
}

// TestDocumentPreservesConversationPresence asserts an absent conversation
// stays absent and a present empty conversation stays present.
func TestDocumentPreservesConversationPresence(t *testing.T) {
	cases := []struct {
		name         string
		conversation []byte
		present      bool
	}{
		{"absent", nil, false},
		{"present empty", []byte{}, true},
		{"present bytes", []byte{0x00, 0xff}, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			snapshot := fullSnapshot(t, testCase.conversation)
			if snapshot.HasConversation() != testCase.present {
				t.Fatalf("source snapshot presence = %v; want %v", snapshot.HasConversation(), testCase.present)
			}
			encoded, err := snapshotdoc.Encode(snapshotdoc.Admitted{Snapshot: snapshot, Actions: admittedActions()[:1]})
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			decoded, err := snapshotdoc.Decode(encoded)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if decoded.Snapshot.HasConversation() != testCase.present {
				t.Fatalf("decoded presence = %v; want %v", decoded.Snapshot.HasConversation(), testCase.present)
			}
			if testCase.present && !bytes.Equal(decoded.Snapshot.ConversationBytes(), testCase.conversation) {
				t.Fatalf("conversation = %v; want exact %v", decoded.Snapshot.ConversationBytes(), testCase.conversation)
			}
		})
	}
}

// TestDecodeRefusesMalformedOrTamperedDocuments asserts no stored document is
// silently accepted when it is empty, malformed, an unknown version, has no
// yaml, or does not match its stored digest.
func TestDecodeRefusesMalformedOrTamperedDocuments(t *testing.T) {
	valid, err := snapshotdoc.Encode(snapshotdoc.Admitted{Snapshot: fullSnapshot(t, nil), Actions: admittedActions()[:1]})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(valid, &parsed); err != nil {
		t.Fatalf("unmarshal valid: %v", err)
	}

	cases := map[string][]byte{
		"empty":           nil,
		"malformed":       []byte("{"),
		"unknown version": mustJSON(t, map[string]any{"version": 2, "yaml": "eA==", "snapshot": map[string]any{}, "actions": []any{}}),
		"missing yaml":    mustJSON(t, map[string]any{"version": 1, "yaml": "", "snapshot": parsed["snapshot"], "actions": parsed["actions"]}),
		"missing digest":  mustJSON(t, map[string]any{"version": 1, "yaml": parsed["yaml"], "snapshot": map[string]any{}, "actions": parsed["actions"]}),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := snapshotdoc.Decode(data); err == nil {
				t.Fatalf("Decode(%s) succeeded; want a fault", name)
			} else if class, ok := fault.ClassOf(err); !ok || class != fault.InternalFailure {
				t.Fatalf("class = %v; want internal_failure", err)
			}
		})
	}

	tampered := map[string]any{
		"version":  1,
		"yaml":     "eA==",
		"snapshot": parsed["snapshot"],
		"actions":  parsed["actions"],
	}
	if _, err := snapshotdoc.Decode(mustJSON(t, tampered)); err == nil {
		t.Fatal("a document whose yaml no longer matches its digest must be refused")
	}
}

// TestEncodeRefusesAnInvalidAdmittedSet asserts a writer cannot persist an
// empty, unordered or invalid action list.
func TestEncodeRefusesAnInvalidAdmittedSet(t *testing.T) {
	snapshot := fullSnapshot(t, nil)
	cases := map[string][]pipeline.AdmittedAction{
		"empty":      nil,
		"non-zero":   {admittedActions()[1]},
		"duplicated": {admittedActions()[0], admittedActions()[0]},
	}
	for name, actions := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := snapshotdoc.Encode(snapshotdoc.Admitted{Snapshot: snapshot, Actions: actions}); err == nil {
				t.Fatalf("Encode(%s) succeeded; want invalid_input", name)
			}
		})
	}
}

// TestDerivedIdentitiesAreStableDistinctAndValid asserts the documented
// derivations are deterministic, collision-free for distinct inputs and valid
// identities.
func TestDerivedIdentitiesAreStableDistinctAndValid(t *testing.T) {
	first := snapshotdoc.DeriveActionID("exec-1", 0)
	second := snapshotdoc.DeriveActionID("exec-1", 1)
	other := snapshotdoc.DeriveActionID("exec-2", 0)
	if first == second || first == other || second == other {
		t.Fatalf("action identities collide: %q %q %q", first, second, other)
	}
	if first != snapshotdoc.DeriveActionID("exec-1", 0) {
		t.Fatal("action identity is not stable")
	}
	if _, err := id.ParseActionID(string(first)); err != nil {
		t.Fatalf("derived action id is invalid: %v", err)
	}

	workspace := snapshotdoc.DeriveWorkspaceID("exec-1")
	if workspace != snapshotdoc.DeriveWorkspaceID("exec-1") {
		t.Fatal("workspace identity is not stable")
	}
	if workspace == snapshotdoc.DeriveWorkspaceID("exec-2") {
		t.Fatal("workspace identities collide across executions")
	}
	if _, err := id.ParseWorkspaceID(string(workspace)); err != nil {
		t.Fatalf("derived workspace id is invalid: %v", err)
	}
}

// TestActionRowsProjectTheSchemaColumns asserts the row projection carries
// every actions column that exists, and no definition column, because the
// definition lives only in the snapshot document.
func TestActionRowsProjectTheSchemaColumns(t *testing.T) {
	rows, err := snapshotdoc.ActionRows("exec-1", admittedActions())
	if err != nil {
		t.Fatalf("ActionRows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d; want 2", len(rows))
	}
	for position, row := range rows {
		if row.ID != string(snapshotdoc.DeriveActionID("exec-1", position)) {
			t.Fatalf("row %d id = %q", position, row.ID)
		}
		if row.Values["index"] != int64(position) || row.Values["name"] != string(admittedActions()[position].Name) {
			t.Fatalf("row %d values = %v", position, row.Values)
		}
		if row.Values["kind"] != string(admittedActions()[position].Kind) || row.Values["state"] != "pending" {
			t.Fatalf("row %d kind/state = %v", position, row.Values)
		}
		if _, hasDefinition := row.Values["container"]; hasDefinition {
			t.Fatalf("row %d projects a definition column that the schema does not have", position)
		}
		action := admittedActions()[position]
		assignment, hasAssignment := row.Values["request_assignment"]
		if action.Kind == definition.ActionKindRequest {
			encoded, ok := assignment.([]byte)
			if !ok || len(encoded) == 0 {
				t.Fatalf("request row %d has no decodable assignment: %v", position, assignment)
			}
			var decoded request.Assignment
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatalf("request row %d assignment does not decode: %v", position, err)
			}
			want := request.Assignment{Kind: request.AssignmentRole, Role: tenancy.RoleReviewer}
			if decoded != want {
				t.Fatalf("request row %d assignment = %+v; want %+v", position, decoded, want)
			}
		} else if hasAssignment {
			t.Fatalf("container row %d carries a request assignment: %v", position, assignment)
		}
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return encoded
}
