package gitwebhookhttp_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"

	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
	"github.com/optimiware/kodama/internal/integrations/gitwebhook/gitwebhooktest"
)

const fragment = "../../../../api/git-webhooks.yaml"

func loadFragment(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile(fragment)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func schemaOf(t *testing.T, doc map[string]any, name string) *jsonschema.Schema {
	t.Helper()
	s := doc["components"].(map[string]any)["schemas"].(map[string]any)[name]
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	v, err := jsonschema.UnmarshalJSON(strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("mem://"+name, v); err != nil {
		t.Fatal(err)
	}
	sch, err := c.Compile("mem://" + name)
	if err != nil {
		t.Fatal(err)
	}
	return sch
}

// The published schema accepts exactly what the Go parser accepts for the
// documented example and rejects the shapes the parser rejects.
func TestFragmentMatchesPayloadContract(t *testing.T) {
	doc := loadFragment(t)
	sch := schemaOf(t, doc, "PushNotification")
	cases := map[string]bool{
		`{"schemaVersion":1,"repository":"repo-main","ref":"refs/heads/main","before":"` + gitwebhooktest.Zero + `","after":"` + string(gitwebhooktest.CommitA) + `"}`:                true,
		`{"schemaVersion":2,"repository":"repo-main","ref":"refs/heads/main","before":"` + gitwebhooktest.Zero + `","after":"` + string(gitwebhooktest.CommitA) + `"}`:                false,
		`{"schemaVersion":1,"repository":"repo-main","ref":"refs/heads/main","before":"` + gitwebhooktest.Zero + `","after":"main"}`:                                                  false,
		`{"schemaVersion":1,"repository":"repo-main","ref":"refs/heads/main","before":"` + gitwebhooktest.Zero + `","after":"` + string(gitwebhooktest.CommitA) + `","tenantId":"t"}`: false,
		`{"schemaVersion":1,"repository":"https://evil/x","ref":"refs/heads/main","before":"` + gitwebhooktest.Zero + `","after":"` + string(gitwebhooktest.CommitA) + `"}`:           false,
	}
	for body, want := range cases {
		var v any
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			t.Fatal(err)
		}
		schemaOK := sch.Validate(v) == nil
		_, perr := gw.ParsePayload([]byte(body))
		if schemaOK != want || (perr == nil) != want {
			t.Errorf("%s: schema=%v parser=%v want=%v", body, schemaOK, perr == nil, want)
		}
	}
	// The documented example is itself valid.
	ex := doc["paths"].(map[string]any)["/git-hooks/{triggerId}"].(map[string]any)["post"].(map[string]any)["requestBody"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["example"]
	if err := sch.Validate(ex); err != nil {
		t.Fatalf("example invalid: %v", err)
	}
}

func TestFragmentDocumentsEveryStatus(t *testing.T) {
	doc := loadFragment(t)
	resp := doc["paths"].(map[string]any)["/git-hooks/{triggerId}"].(map[string]any)["post"].(map[string]any)["responses"].(map[string]any)
	for _, code := range []string{"200", "202", "400", "401", "409", "413", "422", "500", "503"} {
		if _, ok := resp[code]; !ok {
			t.Errorf("status %s is not documented", code)
		}
	}
	if sec, ok := doc["security"].([]any); !ok || len(sec) != 0 {
		t.Error("the hook endpoint must not declare a bearer scheme")
	}
}
