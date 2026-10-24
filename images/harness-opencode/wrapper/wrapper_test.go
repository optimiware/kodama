package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/images/harness-opencode/wrapper/spec"
)

func TestServersMapQualifiedToolsAndFailClosed(t *testing.T) {
	got, err := servers([]string{"mcp_a/echo", "mcp_a/mutate", "mcp_b/lookup"})
	if err != nil || len(got) != 2 || got[0].Name != "mcp_a" || len(got[0].Tools) != 2 {
		t.Fatalf("servers: %+v %v", got, err)
	}
	for name, tools := range map[string][]string{
		"bare name":                    {"echo"},
		"wildcard":                     {"mcp_a/*"},
		"two registrations, one name":  {"a.b/x", "a_b/x"},
		"two tools, one OpenCode name": {"a/b_c", "a_b/c"},
		"empty tool":                   {"a/"},
	} {
		if _, err := servers(tools); err == nil {
			t.Errorf("%s: accepted %v", name, tools)
		}
	}
}

func TestPermissionsDenyEverythingThenAllowTheProfile(t *testing.T) {
	srv, _ := servers([]string{"mcp_a/echo"})
	rules := permissions(srv)
	if rules[0] != (rule{"*", "*", "deny"}) {
		t.Fatalf("the first rule must deny everything: %+v", rules[0])
	}
	var allowed []string
	for _, r := range rules {
		if r.Effect == "ask" {
			t.Fatalf("an ask rule would wait for a human: %+v", r)
		}
		if r.Effect == "allow" {
			allowed = append(allowed, r.Action)
		}
	}
	for _, want := range []string{"read", "edit", "shell", "mcp_a_echo"} {
		found := false
		for _, a := range allowed {
			found = found || a == want
		}
		if !found {
			t.Errorf("%s is not allowed: %v", want, allowed)
		}
	}
	for _, a := range allowed {
		if a == "webfetch" || a == "websearch" || a == "question" || a == "subagent" || a == "skill" || strings.HasSuffix(a, "_*") {
			t.Errorf("%s must not be allowed", a)
		}
	}
	last := rules[len(rules)-1]
	if last.Action != "mcp_a_echo" {
		t.Errorf("the deny-all must precede every allow, last rule %+v", last)
	}
}

func TestRedactorRemovesExactAndShapedCredentials(t *testing.T) {
	r := &redactor{}
	r.Add("abc.def-ghi_SECRET0123")
	in := "token abc.def-ghi_SECRET0123 and Authorization: Bearer zzzzzzzzzzzz and eyJhbGciOiJFUzI1NiJ9.eyJzdWIiOiJ4eHh4eHgifQ.c2lnbmF0dXJlc2lnbmF0dXJl end"
	out := r.String(in)
	for _, leak := range []string{"abc.def-ghi_SECRET0123", "zzzzzzzzzzzz", "eyJhbGciOiJFUzI1NiJ9"} {
		if strings.Contains(out, leak) {
			t.Errorf("%q survived: %s", leak, out)
		}
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("nothing was redacted: %s", out)
	}
}

func TestDropStrictFalseIsExactlyThat(t *testing.T) {
	body := `{"model":"m","stream":true,"tools":[{"type":"function","function":{"name":"a","parameters":{"type":"object"},"strict":false}},{"type":"function","function":{"name":"b","strict":true}}]}`
	out := dropStrictFalse([]byte(body))
	var doc struct {
		Tools []struct {
			Function map[string]json.RawMessage `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc.Tools[0].Function["strict"]; ok {
		t.Error("strict:false was kept")
	}
	if string(doc.Tools[1].Function["strict"]) != "true" {
		t.Error("strict:true must be forwarded for the gateway to judge")
	}
	if got := dropStrictFalse([]byte(`not json`)); string(got) != "not json" {
		t.Error("an unparsable body must pass through unchanged")
	}
}

func TestAgentResultIsStrict(t *testing.T) {
	cases := map[string]struct {
		doc   string
		state string
	}{
		"valid completed":   {`{"outcome":"completed","summary":"ok"}`, ""},
		"valid needs_input": {`{"outcome":"needs_input","question":"which tier?"}`, ""},
		"valid failed":      {`{"outcome":"failed","reason":"cannot_reproduce"}`, ""},
		"missing summary":   {`{"outcome":"completed"}`, spec.ReasonResultInvalid},
		"unknown member":    {`{"outcome":"completed","summary":"x","extra":1}`, spec.ReasonResultInvalid},
		"duplicate key":     {`{"outcome":"completed","outcome":"failed","summary":"x"}`, spec.ReasonResultInvalid},
		"bad reason":        {`{"outcome":"failed","reason":"Not A Code"}`, spec.ReasonResultInvalid},
		"unknown outcome":   {`{"outcome":"success","summary":"x"}`, spec.ReasonResultInvalid},
		"trailing data":     {`{"outcome":"completed","summary":"x"} {}`, spec.ReasonResultInvalid},
		"blank question":    {`{"outcome":"needs_input","question":"  "}`, spec.ReasonResultInvalid},
	}
	for name, c := range cases {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, spec.ResultFile), []byte(c.doc), 0o600); err != nil {
			t.Fatal(err)
		}
		_, state, _ := readAgentResult(dir)
		if state != c.state {
			t.Errorf("%s: state %q, want %q", name, state, c.state)
		}
		if _, err := os.Lstat(filepath.Join(dir, spec.ResultFile)); err == nil {
			t.Errorf("%s: the result file stayed in the workspace", name)
		}
	}
	if _, state, _ := readAgentResult(t.TempDir()); state != spec.ReasonResultMissing {
		t.Errorf("missing file: %q", state)
	}
	dir := t.TempDir()
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, spec.ResultFile)); err != nil {
		t.Fatal(err)
	}
	if _, state, _ := readAgentResult(dir); state != spec.ReasonResultInvalid {
		t.Errorf("symlink: %q", state)
	}
}

func TestBuildReportNeverBelievesAnUnevidencedPass(t *testing.T) {
	base, err := runtimev1.NewDesignBaseline([]string{"AR-14"}, nil, []runtimev1.SourceRef{{Repository: "https://git.example.invalid/x.git", Revision: runtimev1.GitCommit(strings.Repeat("a", 40)), Path: "a.md"}}, []string{"c1", "c2"})
	if err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("b", 40)
	rep, notes, err := buildReport(base, commit, &agentReport{Checks: []agentCheck{{ID: "c1", Outcome: "passed", Evidence: "x"}, {ID: "c2", Outcome: "failed"}}}, func(s string) string { return s })
	if err != nil {
		t.Fatal(err)
	}
	if rep.Disposition != runtimev1.DispBlocked || len(notes) != 1 || rep.Checks[0].Outcome != runtimev1.CheckNotRun || rep.Checks[1].Outcome != runtimev1.CheckFailed {
		t.Fatalf("report %+v notes %v", rep, notes)
	}
	if rep.BaselineDigest != base.Digest || string(rep.CandidateCommit) != commit {
		t.Fatalf("report is not bound: %+v", rep)
	}
	rep, _, err = buildReport(base, commit, &agentReport{Deviations: []agentDeviation{{Requirement: "AR-14", Reason: "token Bearer abcdefghijkl leaked"}}},
		func(s string) string { return strings.ReplaceAll(s, "abcdefghijkl", "[REDACTED]") })
	if err != nil || len(rep.UnresolvedDeviations) != 1 || strings.Contains(rep.UnresolvedDeviations[0].Reason, "abcdefghijkl") {
		t.Fatalf("deviation %+v %v", rep.UnresolvedDeviations, err)
	}
}

func TestManagedConfigIsTheFixedBackstop(t *testing.T) {
	var doc struct {
		Policies  []map[string]string `json:"policies"`
		Providers map[string]struct {
			Settings map[string]string `json:"settings"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(spec.ManagedConfig(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Providers[spec.ProviderID].Settings["baseURL"] != spec.ModelBaseURL() || !strings.HasPrefix(spec.ModelBaseURL(), "http://127.0.0.1:") {
		t.Fatalf("the managed provider endpoint is not the loopback proxy: %v", doc.Providers)
	}
	if doc.Policies[0]["action"] != "provider.use" || doc.Policies[0]["effect"] != "deny" || doc.Policies[0]["resource"] != "*" {
		t.Fatalf("the first policy must deny every provider: %v", doc.Policies[0])
	}
}
