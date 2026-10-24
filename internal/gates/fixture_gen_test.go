package gates

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
)

// TestGenerateCanonicalFixture rewrites testdata/canonical_subject.json when
// KODAMA_UPDATE_GOLDEN=1. The checked-in file is the contract; review its diff.
func TestGenerateCanonicalFixture(t *testing.T) {
	if os.Getenv("KODAMA_UPDATE_GOLDEN") != "1" {
		t.Skip("set KODAMA_UPDATE_GOLDEN=1 to regenerate")
	}
	type in = map[string]any
	cases := []struct {
		name string
		in   in
	}{
		{"specification example", in{"tenantId": "t_acme", "projectId": "p_inventory", "workItemId": "wi_1042", "contentRevision": 2, "candidateCommit": commitA, "definitionId": "pd_01",
			"evidence": []EvidenceRef{{ID: "art_tests", Digest: digA}}}},
		{"unsorted evidence", in{"tenantId": "t_acme", "projectId": "p_inventory", "workItemId": "wi_1042", "contentRevision": 2, "candidateCommit": commitA, "definitionId": "pd_01",
			"evidence": []EvidenceRef{{ID: "art_z", Digest: digC}, {ID: "art_a", Digest: digB}, {ID: "art_a", Digest: digB}, {ID: "art_m", Digest: digA}}}},
		{"no evidence", in{"tenantId": "t_acme", "projectId": "p_inventory", "workItemId": "wi_1042", "contentRevision": 1, "candidateCommit": commitB, "definitionId": "pd_01", "evidence": []EvidenceRef{}}},
		{"deployment configuration", in{"tenantId": "t_acme", "projectId": "p_inventory", "workItemId": "wi_1042", "contentRevision": 9007199254740991, "candidateCommit": commitA, "definitionId": "pd_02",
			"evidence": []EvidenceRef{{ID: "art_tests", Digest: digA}}, "deploymentId": "dep_7", "deploymentConfigDigest": digB}},
	}
	var out []fixtureCase
	for _, c := range cases {
		raw, _ := json.Marshal(c.in)
		var w struct {
			WorkItemID             string        `json:"workItemId"`
			ContentRevision        int64         `json:"contentRevision"`
			CandidateCommit        string        `json:"candidateCommit"`
			DefinitionID           string        `json:"definitionId"`
			Evidence               []EvidenceRef `json:"evidence"`
			DeploymentID           string        `json:"deploymentId"`
			DeploymentConfigDigest string        `json:"deploymentConfigDigest"`
		}
		_ = json.Unmarshal(raw, &w)
		s, err := NewSubject(SubjectInput{Scope: scopeA, WorkItemID: domainWorkItem(w.WorkItemID), ContentRevision: domainRevision(w.ContentRevision),
			CandidateCommit: w.CandidateCommit, DefinitionID: domainDefinition(w.DefinitionID), Evidence: w.Evidence,
			DeploymentID: w.DeploymentID, DeploymentConfigDigest: w.DeploymentConfigDigest})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, fixtureCase{Name: c.name, Subject: raw, Canonical: string(s.Canonical()), Digest: s.Digest()})
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	if err := os.WriteFile(filepath.Join("testdata", "canonical_subject.json"), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func domainWorkItem(s string) domain.WorkItemID     { return domain.WorkItemID(s) }
func domainRevision(n int64) domain.ContentRevision { return domain.ContentRevision(n) }
func domainDefinition(s string) domain.DefinitionID { return domain.DefinitionID(s) }
