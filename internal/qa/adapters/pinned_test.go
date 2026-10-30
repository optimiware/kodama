package adapters_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/optimiware/kodama/internal/qa/adapters"
)

// fixture returns the payload of a published runtime contract fixture, verbatim.
func fixture(t *testing.T, path, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name    string          `json:"name"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if c.Name == name {
			return c.Payload
		}
	}
	t.Fatalf("no fixture %q in %s", name, path)
	return nil
}

const (
	v1Cases = "../../../contracts/runtime/v1/fixtures/cases.json"
	v2Cases = "../../../contracts/runtime/v2/fixtures/cases.json"
	v3Cases = "../../../contracts/runtime/v3/fixtures/cases.json"
)

// Test_unit_qa_context_projection: the execution adapter reads the frozen
// context by version, each with its own strict decoder. Version 1 is exactly the
// original reading; version 3 yields the members the mapping reads and the
// pinned source set, both from the immutable document only.
func Test_unit_qa_context_projection(t *testing.T) {
	t.Run("version 1 has no version member and no source set", func(t *testing.T) {
		p, err := adapters.DecodePinned(fixture(t, v1Cases, "context_reader"))
		if err != nil {
			t.Fatal(err)
		}
		if p.Version != 1 || !p.Sources.Empty() || p.Context.AttemptID == "" {
			t.Fatalf("%+v", p)
		}
	})

	t.Run("version 3 projects the identity members and the source set", func(t *testing.T) {
		raw := fixture(t, v3Cases, "context_v3_work_item_writer_two_repositories")
		p, err := adapters.DecodePinned(raw)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Workspace struct {
				SourceSetDigest string `json:"sourceSetDigest"`
			} `json:"workspace"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		c := p.Context
		if p.Version != 3 || c.TenantID != "t_acme" || c.ProjectID != "p_inventory" || c.RunID != "run_01" || c.StepID != "generate" || c.AttemptID != "att_01" {
			t.Fatalf("identity: %+v", p)
		}
		if c.WorkItem == nil || c.WorkItem.ID != "wi_1042" || c.WorkItem.ContentRevision != 2 || len(c.Outputs) != 1 || c.Outputs[0].Name != "report" {
			t.Fatalf("members the mapping reads: %+v", c)
		}
		if string(p.Sources.Digest) != doc.Workspace.SourceSetDigest || len(p.Sources.Sources) != 2 {
			t.Fatalf("source set: %+v", p.Sources)
		}
		back, front := p.Sources.Sources[0], p.Sources.Sources[1]
		if back.Path != "backend" || back.RepositoryID != "repo_01" || back.Access != "write" || back.Commit != "1111111111111111111111111111111111111111" ||
			front.Path != "frontend" || front.Access != "read" || front.HeadCommit != "2222222222222222222222222222222222222222" {
			t.Fatalf("sources: %+v", p.Sources.Sources)
		}
		if err := p.Sources.Validate(); err != nil {
			t.Fatal(err)
		}
		if !p.Sources.Holds("2222222222222222222222222222222222222222") || p.Sources.Holds("3333333333333333333333333333333333333333") {
			t.Fatal("Holds must be exactly the pinned and head commits")
		}
	})

	t.Run("the lineage's published commits are carried with their repositories", func(t *testing.T) {
		p, err := adapters.DecodePinned(fixture(t, v3Cases, "context_v3_reader_lineage_with_agent_branch_push"))
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Sources.Published) == 0 {
			t.Fatalf("the lineage published a commit: %+v", p.Sources)
		}
		for _, pub := range p.Sources.Published {
			if pub.RepositoryID == "" || pub.Commit == "" || !p.Sources.Holds(pub.Commit) {
				t.Fatalf("%+v", pub)
			}
		}
		if err := p.Sources.Validate(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a source-free version 3 context has an empty set", func(t *testing.T) {
		p, err := adapters.DecodePinned(fixture(t, v3Cases, "context_v3_cleanup_run_manual_origin_source_free_without_git"))
		if err != nil {
			t.Fatal(err)
		}
		if p.Version != 3 || !p.Sources.Empty() {
			t.Fatalf("%+v", p)
		}
	})

	t.Run("a version this reader does not know is an error, never read as v1", func(t *testing.T) {
		for name, raw := range map[string][]byte{
			"version 2":         fixture(t, v2Cases, "context_v2_reader_first_attempt"),
			"version 4":         []byte(`{"contextVersion":4}`),
			"an explicit one":   []byte(`{"contextVersion":1}`),
			"not an object":     []byte(`[]`),
			"a v3 body as v1":   stripVersion(t, fixture(t, v3Cases, "context_v3_work_item_writer_two_repositories")),
			"a truncated body":  []byte(`{"contextVersion":3`),
			"an invalid source": breakSource(t, fixture(t, v3Cases, "context_v3_work_item_writer_two_repositories")),
		} {
			if _, err := adapters.DecodePinned(raw); err == nil {
				t.Errorf("%s: accepted", name)
			}
		}
	})
}

func stripVersion(t *testing.T, raw []byte) []byte {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "contextVersion")
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func breakSource(t *testing.T, raw []byte) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	ws := m["workspace"].(map[string]any)
	ws["sources"].([]any)[0].(map[string]any)["path"] = "../escape"
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
