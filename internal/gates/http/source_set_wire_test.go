package gateshttp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/gates"
)

func wireGate(t *testing.T, in gates.SubjectInput) *gates.Gate {
	t.Helper()
	subject, err := gates.NewSubject(in)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	g, err := gates.Request(gates.RequestInput{ID: "gate_wire", Scope: in.Scope, RunID: "run_1", StepID: "review", Subject: subject,
		Required: gates.Required{Evidence: []gates.RequiredEvidence{}}, Now: now, Deadline: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func subjectJSON(t *testing.T, g *gates.Gate) (string, map[string]json.RawMessage) {
	t.Helper()
	raw, err := json.Marshal(gateDTO(g, g.RequestedAt).Subject)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return string(raw), m
}

// TestSubjectWireCandidateCommit (AC-A08): candidateCommit stays a required,
// non-null commit. For a single-repository subject the wire is exactly what it
// always was (no sourceSet member); for a source-set subject it presents the
// primary source, the first writable source in repository order, else the first,
// and sourceSet is an additive member that defines the subject.
func TestSubjectWireCandidateCommit(t *testing.T) {
	scope := domain.Scope{TenantID: "t_a", ProjectID: "p_1"}
	const (
		a = "1111111111111111111111111111111111111111"
		b = "2222222222222222222222222222222222222222"
		c = "3333333333333333333333333333333333333333"
	)

	t.Run("a single-repository subject has the earlier wire byte for byte", func(t *testing.T) {
		g := wireGate(t, gates.SubjectInput{Scope: scope, WorkItemID: "wi_1", ContentRevision: 2, CandidateCommit: a, DefinitionID: "pd_1"})
		got, m := subjectJSON(t, g)
		want := `{"candidateCommit":"` + a + `","contentRevision":2,"definitionId":"pd_1","deploymentConfigDigest":null,"deploymentId":null,"evidence":[],` +
			`"projectId":"p_1","tenantId":"t_a","workItemId":"wi_1"}`
		if got != want {
			t.Fatalf("wire\n got %s\nwant %s", got, want)
		}
		if _, ok := m["sourceSet"]; ok {
			t.Fatal("a single-source subject has no sourceSet member")
		}
	})

	set := func(sources ...gates.SubjectSource) *gates.SourceSetSubject {
		s, err := gates.NewSourceSetSubject("sha256:"+strings.Repeat("d", 64), sources)
		if err != nil {
			t.Fatal(err)
		}
		return &s
	}
	base := &gates.Base{Ref: "refs/heads/main", Commit: c}
	for name, tc := range map[string]struct {
		set  *gates.SourceSetSubject
		want string
	}{
		"the first writable source, not the first source": {set(gates.SubjectSource{RepositoryID: "repo_a", Commit: a}, gates.SubjectSource{RepositoryID: "repo_b", Commit: b, Base: base}), b},
		"the first of several writable sources":           {set(gates.SubjectSource{RepositoryID: "repo_b", Commit: b, Base: base}, gates.SubjectSource{RepositoryID: "repo_a", Commit: a, Base: base}), a},
		"the first source when none is writable":          {set(gates.SubjectSource{RepositoryID: "repo_b", Commit: b}, gates.SubjectSource{RepositoryID: "repo_a", Commit: a}), a},
	} {
		t.Run(name, func(t *testing.T) {
			g := wireGate(t, gates.SubjectInput{Scope: scope, WorkItemID: "wi_1", ContentRevision: 2, SourceSet: tc.set, DefinitionID: "pd_1"})
			_, m := subjectJSON(t, g)
			var got string
			if err := json.Unmarshal(m["candidateCommit"], &got); err != nil || got != tc.want {
				t.Fatalf("candidateCommit %s, want %q (%v)", m["candidateCommit"], tc.want, err)
			}
			if _, ok := m["sourceSet"]; !ok {
				t.Fatal("the set is present")
			}
			// The identity is the whole set, never the presented commit.
			other := wireGate(t, gates.SubjectInput{Scope: scope, WorkItemID: "wi_1", ContentRevision: 2, CandidateCommit: tc.want, DefinitionID: "pd_1"})
			if other.SubjectDigest() == g.SubjectDigest() {
				t.Fatal("the presented commit stands for the set")
			}
		})
	}
}
