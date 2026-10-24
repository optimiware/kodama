package qa_test

import (
	"strings"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/qa"
)

const commitA runtimev1.GitCommit = "1111111111111111111111111111111111111111"
const commitB runtimev1.GitCommit = "2222222222222222222222222222222222222222"

func obs(title, location string, criterion *string) qa.Observation {
	return qa.Observation{Title: title, Location: location, CriterionID: criterion}
}

func str(s string) *string { return &s }

func TestFingerprintNormalizesNoiseOnly(t *testing.T) {
	base := obs("Order form shows a blank page", "/orders/new", str("AC-1"))
	same := []qa.Observation{
		obs("  order FORM shows   a blank page ", "/orders/new", str("AC-1")),
		obs("Order form shows a blank page", "/orders/new/", str("AC-1")),
	}
	other := base
	other.Severity, other.Expected, other.Actual = qa.SeverityTrivial, "x", "y"
	other.Steps = []string{"different"}
	other.Evidence = []qa.EvidenceRef{{ArtifactID: "art_other"}}
	same = append(same, other)
	for i, o := range same {
		if o.Fingerprint() != base.Fingerprint() {
			t.Errorf("variant %d changes the fingerprint", i)
		}
	}
	for name, o := range map[string]qa.Observation{
		"another title":     obs("Order form shows an error page", "/orders/new", str("AC-1")),
		"another route":     obs("Order form shows a blank page", "/orders/edit", str("AC-1")),
		"another criterion": obs("Order form shows a blank page", "/orders/new", str("AC-2")),
		"no criterion":      obs("Order form shows a blank page", "/orders/new", nil),
		"root vs other":     obs("Order form shows a blank page", "/", str("AC-1")),
	} {
		if o.Fingerprint() == base.Fingerprint() {
			t.Errorf("%s must be a different finding", name)
		}
	}
	if root := obs("t", "/", nil); root.Fingerprint() != obs("t", "/", nil).Fingerprint() {
		t.Error("the root route is stable")
	}
}

func TestDefectKeyIsDeterministicAndScoped(t *testing.T) {
	scope := domain.Scope{TenantID: "t_a", ProjectID: "p_a"}
	fp := obs("x", "/a", nil).Fingerprint()
	k := qa.DefectKey(scope, "wi_1", commitA, fp)
	if k != qa.DefectKey(scope, "wi_1", commitA, fp) {
		t.Fatal("key is not deterministic")
	}
	if !strings.HasPrefix(k, qa.KeyPrefix) || len(k) != len(qa.KeyPrefix)+64 {
		t.Fatalf("key shape: %s", k)
	}
	for name, other := range map[string]string{
		"tenant":    qa.DefectKey(domain.Scope{TenantID: "t_b", ProjectID: "p_a"}, "wi_1", commitA, fp),
		"project":   qa.DefectKey(domain.Scope{TenantID: "t_a", ProjectID: "p_b"}, "wi_1", commitA, fp),
		"work item": qa.DefectKey(scope, "wi_2", commitA, fp),
		"candidate": qa.DefectKey(scope, "wi_1", commitB, fp),
		"finding":   qa.DefectKey(scope, "wi_1", commitA, obs("y", "/a", nil).Fingerprint()),
	} {
		if other == k {
			t.Errorf("the %s must change the key", name)
		}
	}
	// Components cannot be shifted across a boundary into the same key.
	if qa.DefectKey(domain.Scope{TenantID: "t_ab", ProjectID: "p"}, "wi", commitA, fp) == qa.DefectKey(domain.Scope{TenantID: "t_a", ProjectID: "bp"}, "wi", commitA, fp) {
		t.Fatal("key components are ambiguous")
	}
	id := qa.DefectIDFor(k)
	if err := id.Validate(); err != nil || id != qa.DefectIDFor(k) || !strings.HasPrefix(string(id), "wi_qa_") {
		t.Fatalf("defect id %q: %v", id, err)
	}
	if qa.DefectIDFor("qa1_short") != "" {
		t.Fatal("a malformed key has no identity")
	}
}

func TestParsePayloadMarkerReadsOnlyTheExactFinalLine(t *testing.T) {
	good, fake := strings.Repeat("a", 64), strings.Repeat("b", 64)
	marker, fakeLine := qa.PayloadMarker(good), qa.PayloadMarker(fake)
	for name, tc := range map[string]struct {
		spec string
		want string
		ok   bool
	}{
		"the marker alone":                      {marker, good, true},
		"after other lines":                     {"## Observation\n\n- x\n" + marker, good, true},
		"trailing newlines are trimmed":         {"body\n" + marker + "\n\n", good, true},
		"an earlier fake line is ignored":       {"actual\n" + fakeLine + "\nmore\n- y\n" + marker + "\n", good, true},
		"a fake first line is ignored":          {fakeLine + "\n" + marker, good, true},
		"no marker":                             {"## Observation\n\nbody\n", "", false},
		"only an earlier marker":                {"body\n" + fakeLine + "\nmore\n", "", false},
		"a line after the marker":               {marker + "\n- Deduplication key: `x`\n", "", false},
		"trailing space after the marker":       {marker + " ", "", false},
		"trailing space is not trimmed":         {marker + "\n ", "", false},
		"carriage return is not trimmed":        {marker + "\r\n", "", false},
		"a prefix before the marker":            {"x" + marker, "", false},
		"an upper-case digest":                  {"- Finding payload digest: `" + strings.Repeat("A", 64) + "`", "", false},
		"a short digest":                        {"- Finding payload digest: `abc`", "", false},
		"an empty specification":                {"", "", false},
		"only newlines":                         {"\n\n", "", false},
		"the marker inside a longer final line": {"body " + marker, "", false},
	} {
		got, ok := qa.ParsePayloadMarker(tc.spec)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", name, got, ok, tc.want, tc.ok)
		}
	}
}
