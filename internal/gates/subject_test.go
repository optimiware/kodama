package gates

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
)

var scopeA = domain.Scope{TenantID: "t_acme", ProjectID: "p_inventory"}

const (
	commitA = "1111111111111111111111111111111111111111"
	commitB = "2222222222222222222222222222222222222222"
	digA    = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digB    = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	digC    = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

func baseInput() SubjectInput {
	return SubjectInput{
		Scope: scopeA, WorkItemID: "wi_1042", ContentRevision: 2, CandidateCommit: commitA, DefinitionID: "pd_01",
		Evidence: []EvidenceRef{{ID: "art_tests", Digest: digA}},
	}
}

type fixtureCase struct {
	Name      string          `json:"name"`
	Subject   json.RawMessage `json:"subject"`
	Canonical string          `json:"canonical"`
	Digest    string          `json:"digest"`
}

// TestCanonicalSubject (DQ-02, DQ-03): the fixed canonical bytes and hashes of
// the subject, independent of evidence order, with explicit nulls and empty
// arrays, and distinct digests for every changed field.
func TestCanonicalSubject(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "canonical_subject.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []fixtureCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 4 {
		t.Fatalf("fixture has %d cases", len(cases))
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			var in struct {
				WorkItemID             string        `json:"workItemId"`
				ContentRevision        int64         `json:"contentRevision"`
				CandidateCommit        string        `json:"candidateCommit"`
				DefinitionID           string        `json:"definitionId"`
				Evidence               []EvidenceRef `json:"evidence"`
				DeploymentID           string        `json:"deploymentId"`
				DeploymentConfigDigest string        `json:"deploymentConfigDigest"`
			}
			if err := json.Unmarshal(c.Subject, &in); err != nil {
				t.Fatal(err)
			}
			s, err := NewSubject(SubjectInput{Scope: scopeA, WorkItemID: domain.WorkItemID(in.WorkItemID), ContentRevision: domain.ContentRevision(in.ContentRevision),
				CandidateCommit: in.CandidateCommit, DefinitionID: domain.DefinitionID(in.DefinitionID), Evidence: in.Evidence,
				DeploymentID: in.DeploymentID, DeploymentConfigDigest: in.DeploymentConfigDigest})
			if err != nil {
				t.Fatal(err)
			}
			if string(s.Canonical()) != c.Canonical {
				t.Fatalf("canonical bytes\n got %s\nwant %s", s.Canonical(), c.Canonical)
			}
			sum := sha256.Sum256([]byte(c.Canonical))
			if want := "sha256:" + hex.EncodeToString(sum[:]); c.Digest != want || s.Digest() != want {
				t.Fatalf("digest %s, fixture %s, independent %s", s.Digest(), c.Digest, want)
			}
			back, err := ParseSubject([]byte(c.Canonical))
			if err != nil || back.Digest() != s.Digest() {
				t.Fatalf("round trip: %v", err)
			}
		})
	}
}

func TestSubjectEvidencePermutationStable(t *testing.T) {
	ev := []EvidenceRef{{"art_b", digB}, {"art_a", digA}, {"art_c", digC}, {"art_a", digA}}
	base := baseInput()
	var want string
	for _, perm := range [][]int{{0, 1, 2, 3}, {3, 2, 1, 0}, {1, 3, 0, 2}, {2, 0, 3, 1}} {
		in := base
		in.Evidence = nil
		for _, i := range perm {
			in.Evidence = append(in.Evidence, ev[i])
		}
		s, err := NewSubject(in)
		if err != nil {
			t.Fatal(err)
		}
		if want == "" {
			want = s.Digest()
		} else if s.Digest() != want {
			t.Fatalf("evidence order changed the digest")
		}
		if got := s.Evidence(); len(got) != 3 || got[0].ID != "art_a" || got[2].ID != "art_c" {
			t.Fatalf("evidence not sorted and deduplicated: %+v", got)
		}
	}
}

func TestSubjectExplicitNullsAndEmptyEvidence(t *testing.T) {
	in := baseInput()
	in.Evidence = nil
	s, err := NewSubject(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"evidence":[]`, `"deploymentId":null`, `"deploymentConfigDigest":null`} {
		if !strings.Contains(string(s.Canonical()), want) {
			t.Fatalf("canonical form lacks %s: %s", want, s.Canonical())
		}
	}
}

func TestSubjectChangedFieldChangesDigest(t *testing.T) {
	base, _ := NewSubject(baseInput())
	seen := map[string]string{base.Digest(): "base"}
	for name, mutate := range map[string]func(*SubjectInput){
		"revision":   func(in *SubjectInput) { in.ContentRevision = 3 },
		"candidate":  func(in *SubjectInput) { in.CandidateCommit = commitB },
		"definition": func(in *SubjectInput) { in.DefinitionID = "pd_02" },
		"work item":  func(in *SubjectInput) { in.WorkItemID = "wi_9" },
		"evidence":   func(in *SubjectInput) { in.Evidence = []EvidenceRef{{"art_tests", digB}} },
		"evidence 2": func(in *SubjectInput) { in.Evidence = append(in.Evidence, EvidenceRef{"art_more", digC}) },
		"deployment": func(in *SubjectInput) { in.DeploymentID, in.DeploymentConfigDigest = "dep_1", digA },
		"config":     func(in *SubjectInput) { in.DeploymentID, in.DeploymentConfigDigest = "dep_1", digB },
		"project":    func(in *SubjectInput) { in.Scope.ProjectID = "p_other" },
		"tenant":     func(in *SubjectInput) { in.Scope.TenantID = "t_other" },
	} {
		in := baseInput()
		mutate(&in)
		s, err := NewSubject(in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if prev, dup := seen[s.Digest()]; dup {
			t.Fatalf("%s has the same digest as %s", name, prev)
		}
		seen[s.Digest()] = name
	}
}

func TestSubjectRefusals(t *testing.T) {
	for name, mutate := range map[string]func(*SubjectInput){
		"conflicting digests":    func(in *SubjectInput) { in.Evidence = append(in.Evidence, EvidenceRef{"art_tests", digB}) },
		"short commit":           func(in *SubjectInput) { in.CandidateCommit = "abc" },
		"uppercase commit":       func(in *SubjectInput) { in.CandidateCommit = strings.Repeat("A", 40) },
		"zero revision":          func(in *SubjectInput) { in.ContentRevision = 0 },
		"revision past 2^53":     func(in *SubjectInput) { in.ContentRevision = domain.MaxSafeInteger + 1 },
		"bad digest":             func(in *SubjectInput) { in.Evidence = []EvidenceRef{{"art_tests", "sha256:XYZ"}} },
		"bad artifact id":        func(in *SubjectInput) { in.Evidence = []EvidenceRef{{"art tests", digA}} },
		"deployment without cfg": func(in *SubjectInput) { in.DeploymentID = "dep_1" },
		"cfg without deployment": func(in *SubjectInput) { in.DeploymentConfigDigest = digA },
		"bad deployment config":  func(in *SubjectInput) { in.DeploymentID, in.DeploymentConfigDigest = "dep_1", "md5:1" },
		"empty definition":       func(in *SubjectInput) { in.DefinitionID = "" },
	} {
		in := baseInput()
		mutate(&in)
		if _, err := NewSubject(in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	in := baseInput()
	for i := 0; i <= MaxEvidence; i++ {
		in.Evidence = append(in.Evidence, EvidenceRef{ID: domain.ArtifactID("art_" + strings.Repeat("x", 3) + string(rune('a'+i%26)) + string(rune('a'+i/26))), Digest: digA})
	}
	if _, err := NewSubject(in); err == nil {
		t.Errorf("too much evidence accepted")
	}
}

func TestSubjectUTF8AndIntegerRules(t *testing.T) {
	// A non-ASCII identifier is refused by the identifier syntax rather than
	// canonicalized, so the canonical form is ASCII for every valid subject.
	in := baseInput()
	in.WorkItemID = "wi_é"
	if _, err := NewSubject(in); err == nil {
		t.Fatalf("non-ASCII identifier accepted")
	}
	in = baseInput()
	in.ContentRevision = domain.MaxSafeInteger
	s, err := NewSubject(in)
	if err != nil || !strings.Contains(string(s.Canonical()), `"contentRevision":9007199254740991`) {
		t.Fatalf("largest safe integer: %v %s", err, s.Canonical())
	}
	if strings.Contains(string(s.Canonical()), "e+") || strings.Contains(string(s.Canonical()), "E+") {
		t.Fatalf("floating point rendering: %s", s.Canonical())
	}
}

func TestParseSubjectRejectsNoncanonical(t *testing.T) {
	s, _ := NewSubject(baseInput())
	good := string(s.Canonical())
	for name, doc := range map[string]string{
		"whitespace":    strings.Replace(good, ",", ", ", 1),
		"reordered":     `{"tenantId":"t_acme","projectId":"p_inventory"}`,
		"unknown field": strings.Replace(good, "{", `{"extra":1,`, 1),
		"truncated":     good[:len(good)-2],
		"empty":         ``,
	} {
		if _, err := ParseSubject([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if back, err := ParseSubject([]byte(good)); err != nil || !reflect.DeepEqual(back.Canonical(), s.Canonical()) {
		t.Fatalf("canonical form not accepted: %v", err)
	}
}
