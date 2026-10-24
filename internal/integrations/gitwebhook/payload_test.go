package gitwebhook_test

import (
	"fmt"
	"strings"
	"testing"

	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
)

const (
	commitA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	zero    = "0000000000000000000000000000000000000000"
)

func valid() string {
	return fmt.Sprintf(`{"schemaVersion":1,"repository":"repo-main","ref":"refs/heads/main","before":"%s","after":"%s"}`, zero, commitA)
}

func TestParsePayload(t *testing.T) {
	p, err := gw.ParsePayload([]byte(valid()))
	if err != nil || p.After != commitA || p.Repository != "repo-main" || p.IsDeletion() {
		t.Fatalf("got %+v, %v", p, err)
	}
	del, err := gw.ParsePayload([]byte(strings.Replace(valid(), `"after":"`+commitA, `"after":"`+zero, 1)))
	if err != nil || !del.IsDeletion() {
		t.Fatalf("deletion: %+v, %v", del, err)
	}
	bad := map[string]string{
		"empty":                ``,
		"not json":             `push`,
		"array":                `[]`,
		"trailing":             valid() + `{}`,
		"unknown member":       strings.Replace(valid(), `"ref"`, `"extra":1,"ref"`, 1),
		"wrong version":        strings.Replace(valid(), `"schemaVersion":1`, `"schemaVersion":2`, 1),
		"short commit":         strings.Replace(valid(), commitA, "abc123", 1),
		"uppercase commit":     strings.Replace(valid(), commitA, strings.ToUpper(commitA), 1),
		"branch name as after": strings.Replace(valid(), commitA, "main", 1),
		"bad repository":       strings.Replace(valid(), "repo-main", "../repo", 1),
		"newline ref":          strings.Replace(valid(), "refs/heads/main", `refs/heads/main\nx`, 1),
		"huge":                 `{"schemaVersion":1,"repository":"` + strings.Repeat("a", gw.MaxBodyBytes) + `"}`,
	}
	for name, body := range bad {
		if _, err := gw.ParsePayload([]byte(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestBranchFilters(t *testing.T) {
	for _, ok := range [][]string{{"main"}, {"release/*"}, {"main", "release/*", "feature/a.b-c_d"}} {
		if err := gw.ValidateBranchPatterns(ok); err != nil {
			t.Errorf("%v: %v", ok, err)
		}
	}
	for _, bad := range [][]string{nil, {}, {"*"}, {"/*"}, {"**"}, {"a*"}, {"*/x"}, {"../x"}, {"a b"}, {"main", "main"}, {"-x"}, {"x.lock"}, {"x/"}, {"refs/heads/main/*/y"}} {
		if err := gw.ValidateBranchPatterns(bad); err == nil {
			t.Errorf("%v: accepted", bad)
		}
	}
	tr := gw.Trigger{Branches: []string{"main", "release/*"}}
	for ref, want := range map[string]bool{
		"refs/heads/main": true, "refs/heads/release/1.0": true, "refs/heads/release/a/b": true,
		"refs/heads/release/": false, "refs/heads/release": false, "refs/heads/main/x": false,
		"refs/tags/main": false, "main": false, "refs/heads/../main": false,
	} {
		if got := tr.MatchesRef(ref); got != want {
			t.Errorf("%s: got %v, want %v", ref, got, want)
		}
	}
}

func TestSignatureIsBoundToEveryInput(t *testing.T) {
	secret := []byte(strings.Repeat("k", 32))
	base := gw.Sign(secret, "t1", "d1", 100, []byte("body"))
	for name, other := range map[string]string{
		"trigger":   gw.Sign(secret, "t2", "d1", 100, []byte("body")),
		"delivery":  gw.Sign(secret, "t1", "d2", 100, []byte("body")),
		"timestamp": gw.Sign(secret, "t1", "d1", 101, []byte("body")),
		"body":      gw.Sign(secret, "t1", "d1", 100, []byte("bodx")),
		"secret":    gw.Sign([]byte(strings.Repeat("j", 32)), "t1", "d1", 100, []byte("body")),
	} {
		if other == base {
			t.Errorf("signature ignores %s", name)
		}
	}
	// Field boundaries cannot be shifted: delivery "a\nb" is not a valid ID, but
	// the digest-and-newline framing also keeps trigger/delivery distinct.
	if gw.SigningString("ab", "c", 1, nil) == gw.SigningString("a", "bc", 1, nil) {
		t.Error("ambiguous framing")
	}
}
