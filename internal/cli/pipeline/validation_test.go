package pipeline_test

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/cli/client/clienttest"
	clipipeline "github.com/optimiware/kodama/internal/cli/pipeline"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/pipeline"
)

func diagnosticsOf(t *testing.T, stderr string) []map[string]any {
	t.Helper()
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				Diagnostics []map[string]any `json:"diagnostics"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stderr), &env); err != nil {
		t.Fatalf("stderr %q: %v", stderr, err)
	}
	return env.Error.Details.Diagnostics
}

// Test_pipeline_validation_parity: the CLI reports the shared validator's own
// diagnostics and digests, and contacts no endpoint (no factory is configured).
func Test_pipeline_validation_parity(t *testing.T) {
	v := m1Validator(t)
	raw := readFixture(t, "cases.json")
	var cases []struct {
		Name     string `json:"name"`
		Document string `json:"document"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("no cases")
	}
	scope := domain.Scope{TenantID: tenant, ProjectID: project}
	for _, c := range cases {
		_, direct := v.ValidateYAML(t.Context(), scope, diagnosticActor, []byte(c.Document))
		var ve *pipeline.ValidationError
		if !errorsAs(direct, &ve) {
			t.Errorf("%s: the shared validator accepted a case that must fail: %v", c.Name, direct)
			continue
		}
		file := writeFile(t, "p.yaml", c.Document)
		r := runWith(t, v, "", "", "pipeline", "validate", "--file", file, "--output", "json")
		if r.code != 2 || r.out != "" {
			t.Errorf("%s: exit %d out %q", c.Name, r.code, r.out)
			continue
		}
		got := diagnosticsOf(t, r.err)
		if len(got) != len(ve.Diagnostics) {
			t.Errorf("%s: %d diagnostics, validator %d", c.Name, len(got), len(ve.Diagnostics))
			continue
		}
		for i, d := range ve.Diagnostics {
			if got[i]["code"] != d.Code || got[i]["path"] != d.Path || got[i]["message"] != d.Message {
				t.Errorf("%s: diagnostic %d is %v, validator says %+v", c.Name, i, got[i], d)
			}
		}
	}

	for _, name := range []string{"minimal-test", "build-and-publish"} {
		src := readFixture(t, filepath.Join("valid", name+".yaml"))
		want := strings.TrimSpace(string(readFixture(t, filepath.Join("canonical", name+".digest"))))
		file := writeFile(t, name+".yaml", string(src))
		r := runWith(t, v, "", "", "pipeline", "validate", "--file", file, "--output", "json")
		if r.code != 0 || r.err != "" {
			t.Fatalf("%s: exit %d err %q", name, r.code, r.err)
		}
		var out map[string]any
		direct, derr := v.ValidateYAML(t.Context(), scope, diagnosticActor, src)
		if derr != nil {
			t.Fatalf("%s: the shared validator rejects its own fixture: %v", name, derr)
		}
		if err := json.Unmarshal([]byte(r.out), &out); err != nil || out["digest"] != want || out["digest"] != string(direct.Digest()) ||
			out["valid"] != true || out["name"] != direct.Name() {
			t.Errorf("%s: output %q, want digest %s", name, r.out, want)
		}
	}
	// JSON fixture and stdin take the same path.
	js := readFixture(t, filepath.Join("valid", "minimal-test.json"))
	r := runWith(t, v, "", "", "pipeline", "validate", "--file", writeFile(t, "m.json", string(js)), "--output", "json")
	if r.code != 0 {
		t.Fatalf("json: exit %d %s", r.code, r.err)
	}
	yml := readFixture(t, filepath.Join("valid", "minimal-test.yaml"))
	if r2 := runWith(t, v, "", string(yml), "pipeline", "validate", "--file", "-", "--output", "json"); r2.code != 0 || r2.out != r.out {
		t.Errorf("stdin: exit %d out %q, file out %q", r2.code, r2.out, r.out)
	}
}

func errorsAs(err error, target **pipeline.ValidationError) bool {
	for err != nil {
		if ve, ok := err.(*pipeline.ValidationError); ok {
			*target = ve
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func Test_pipeline_validation_fails_closed(t *testing.T) {
	steps := readFixture(t, filepath.Join("valid", "build-and-publish.yaml"))
	file := writeFile(t, "p.yaml", string(steps))

	// A reference to a secret needs the injected reference policy: absent, the
	// document is not accepted.
	noRefs := validatorWith(t, pipeline.Config{Capabilities: pipeline.EnabledKinds{pipeline.KindContainer}})
	r := runWith(t, noRefs, "", "", "pipeline", "validate", "--file", file, "--output", "json")
	if r.code != 1 || r.out != "" || !strings.Contains(r.err, `"code":"validation_unavailable"`) {
		t.Fatalf("missing policy: exit %d out %q err %q", r.code, r.out, r.err)
	}

	// An unsupported kind is rejected by the capability binding, not by the CLI.
	agent := readFixture(t, filepath.Join("valid", "m2-agent-gate.yaml"))
	r = runWith(t, m1Validator(t), "", "", "pipeline", "validate", "--file", writeFile(t, "a.yaml", string(agent)), "--output", "json")
	if r.code != 2 || !strings.Contains(r.err, "kind_disabled") {
		t.Fatalf("agent kind under M1: exit %d err %q", r.code, r.err)
	}
	// The same validator with the kinds enabled but an unknown profile lookup fails closed.
	both := validatorWith(t, pipeline.Config{Capabilities: pipeline.EnabledKinds{pipeline.KindContainer, pipeline.KindAgent, pipeline.KindGate}})
	r = runWith(t, both, "", "", "pipeline", "validate", "--file", writeFile(t, "a.yaml", string(agent)), "--output", "json")
	if r.code != 1 || r.out != "" {
		t.Fatalf("missing profile lookup: exit %d out %q err %q", r.code, r.out, r.err)
	}
}

func Test_pipeline_validate_inputs(t *testing.T) {
	v := m1Validator(t)
	for name, args := range map[string][]string{
		"missing file flag": {"pipeline", "validate"},
		"unreadable file":   {"pipeline", "validate", "--file", filepath.Join(t.TempDir(), "absent.yaml")},
		"directory":         {"pipeline", "validate", "--file", t.TempDir()},
		"extra argument":    {"pipeline", "validate", "--file", "x", "extra"},
	} {
		if r := runWith(t, v, "", "", args...); r.code != 2 || r.out != "" {
			t.Errorf("%s: exit %d out %q err %q", name, r.code, r.out, r.err)
		}
	}
	big := writeFile(t, "big.yaml", strings.Repeat("# padding\n", clipipeline.DefaultMaxFileBytes/10+10))
	if r := runWith(t, v, "", "", "pipeline", "validate", "--file", big); r.code != 2 || !strings.Contains(r.err, "larger than") {
		t.Errorf("oversized file: exit %d err %q", r.code, r.err)
	}
	if _, err := clipipeline.NewCommands(clipipeline.Config{Actor: diagnosticActor}); err == nil {
		t.Error("a missing validator must be refused")
	}
	if _, err := clipipeline.NewCommands(clipipeline.Config{Validator: v}); err == nil {
		t.Error("a missing diagnostic actor must be refused")
	}
}

func Test_pipeline_validate_contacts_no_endpoint(t *testing.T) {
	srv := clienttest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("validate contacted %s %s", r.Method, r.URL.Path)
	}))
	src := readFixture(t, filepath.Join("valid", "minimal-test.yaml"))
	r := runWith(t, m1Validator(t), srv.URL, "", "pipeline", "validate", "--file", writeFile(t, "p.yaml", string(src)))
	if r.code != 0 || !strings.Contains(r.out, "valid:") || !strings.Contains(r.out, "local check") {
		t.Fatalf("exit %d out %q err %q", r.code, r.out, r.err)
	}
	if n := len(srv.Requests()); n != 0 {
		t.Fatalf("%d requests", n)
	}
}
