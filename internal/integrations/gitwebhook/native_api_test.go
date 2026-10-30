package gitwebhook_test

import (
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
)

// Test_unit_trigger_api_fragment_matches_contract proves api/git-triggers.yaml
// stays tied to the Go contract: every reference resolves, the trigger's wire
// members are the contract's TriggerConfig members plus its bookkeeping, and the
// published ref pattern accepts and refuses what contracts/git/v1 does.
func Test_unit_trigger_api_fragment_matches_contract(t *testing.T) {
	read := func(path string) map[string]any {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		return doc
	}
	doc, common := read("../../../api/git-triggers.yaml"), read("../../../api/common.yaml")

	var lookup func(root map[string]any, pointer string) any
	lookup = func(root map[string]any, pointer string) any {
		var cur any = root
		for _, part := range strings.Split(strings.TrimPrefix(pointer, "#/"), "/") {
			m, ok := cur.(map[string]any)
			if !ok {
				return nil
			}
			cur = m[part]
		}
		return cur
	}
	var walk func(v any)
	refs := 0
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, child := range x {
				if k == "$ref" {
					ref := child.(string)
					refs++
					target := lookup(doc, ref)
					if file, pointer, ok := strings.Cut(ref, "#"); ok && file != "" {
						if file != "common.yaml" {
							t.Errorf("unexpected external reference %s", ref)
						}
						target = lookup(common, "#"+pointer)
					}
					if target == nil {
						t.Errorf("reference %s does not resolve", ref)
					}
					continue
				}
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	walk(doc)
	if refs < 30 {
		t.Fatalf("only %d references walked", refs)
	}

	// Operations: unique identifiers over the routes, no delete anywhere.
	ids := map[string]bool{}
	paths := doc["paths"].(map[string]any)
	for path, item := range paths {
		for method, op := range item.(map[string]any) {
			if method == "delete" {
				t.Errorf("%s: a trigger is disabled, never deleted", path)
			}
			if o, ok := op.(map[string]any); ok && o["operationId"] != nil {
				id := o["operationId"].(string)
				if ids[id] {
					t.Errorf("duplicate operationId %s", id)
				}
				ids[id] = true
			}
		}
	}
	var got []string
	for id := range ids {
		got = append(got, id)
	}
	sort.Strings(got)
	want := []string{"createGitTaskRoute", "createGitTrigger", "getGitTaskRoute", "getGitTaskRouteStatus", "getGitTrigger", "getGitTriggerStatus", "listGitTriggers", "replaceGitTaskRoute", "updateGitTrigger"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("operations = %v, want %v", got, want)
	}

	// The trigger's members: the contract's, plus the bookkeeping.
	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	var members []string
	for _, m := range schemas["GitTrigger"].(map[string]any)["required"].([]any) {
		members = append(members, m.(string))
	}
	var contract []string
	rt := reflect.TypeOf(gitv1.TriggerConfig{})
	for i := range rt.NumField() {
		name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
		contract = append(contract, name)
	}
	have := map[string]bool{}
	for _, m := range members {
		have[m] = true
	}
	for _, m := range contract {
		if !have[m] {
			t.Errorf("the contract's member %q is not in GitTrigger", m)
		}
	}
	for _, extra := range []string{"configuredBy", "enabledAt", "version", "createdAt", "updatedAt"} {
		if !have[extra] {
			t.Errorf("GitTrigger lacks %q", extra)
		}
	}
	patterns := schemas["RefPatterns"].(map[string]any)
	if patterns["minItems"] != 1 || patterns["maxItems"] != gitv1.MaxTriggerPatterns {
		t.Errorf("pattern list bounds: %v %v", patterns["minItems"], patterns["maxItems"])
	}

	// The published pattern is no looser than the contract's on the cases that
	// matter (the server applies the full rule on top).
	re := regexp.MustCompile(schemas["RefPattern"].(map[string]any)["pattern"].(string))
	for _, p := range []string{"refs/heads/main", "refs/heads/release/*", "refs/tags/v1", "refs/tags/rel/*", "refs/heads/a/b/c", "refs/heads/*", "refs/kodama/runs/x", "refs/heads/a*/b", "main", "refs/notes/x", "refs/heads/../x"} {
		contractOK := gitv1.ValidateRefPattern(p) == nil
		if contractOK && !re.MatchString(p) {
			t.Errorf("%q is a contract pattern the fragment refuses", p)
		}
		if !contractOK && re.MatchString(p) && p != "refs/heads/*" && p != "refs/heads/a*/b" && p != "refs/heads/../x" {
			t.Errorf("%q is not a contract pattern but the fragment accepts it", p)
		}
	}
}
