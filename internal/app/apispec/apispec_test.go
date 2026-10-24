package apispec

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const apiDir = "../../../api"

func TestCommittedAggregateIsCurrent(t *testing.T) {
	got, err := Bundle(apiDir, CoreFragments, CoreInfo)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Bundle(apiDir, CoreFragments, CoreInfo)
	if err != nil || !bytes.Equal(got, again) {
		t.Fatalf("assembly is not deterministic: %v", err)
	}
	want, err := os.ReadFile(apiDir + "/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("api/openapi.yaml is stale: run `make api-generate` and commit the result")
	}
}

func TestAggregateContainsOnlyTheCoreSubset(t *testing.T) {
	data, err := os.ReadFile(apiDir + "/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		OpenAPI    string                    `yaml:"openapi"`
		Servers    []map[string]string       `yaml:"servers"`
		Security   []map[string][]string     `yaml:"security"`
		Paths      map[string]map[string]any `yaml:"paths"`
		Components map[string]map[string]any `yaml:"components"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.OpenAPI != "3.0.3" || len(doc.Servers) != 1 || doc.Servers[0]["url"] != "/api/v1" || len(doc.Security) != 1 {
		t.Fatalf("header: %+v", doc)
	}
	for p := range doc.Paths {
		for _, banned := range []string{"/runtime", "/attempts", "/gateway", "/internal", "/mcp/", "/webhooks", "/git-hooks"} {
			if strings.Contains(p, banned) {
				t.Errorf("path %s is outside the core subset", p)
			}
		}
	}
	for _, want := range []string{
		"/me", "/tenants/{tenantId}/projects/{projectId}/work-items", "/tenants/{tenantId}/projects/{projectId}/runs",
		"/tenants/{tenantId}/projects/{projectId}/runs/{id}/events", "/tenants/{tenantId}/projects/{projectId}/events",
		"/tenants/{tenantId}/projects/{projectId}/pipeline-definitions", "/tenants/{tenantId}/projects/{projectId}/secrets",
		"/tenants/{tenantId}/projects/{projectId}/workspaces/{id}", "/tenants/{tenantId}/projects/{projectId}/artifacts/{id}",
		"/tenants/{tenantId}/projects/{projectId}/runs/{id}/logs",
		"/tenants/{tenantId}/projects/{projectId}/agent-profiles", "/tenants/{tenantId}/projects/{projectId}/gates/{id}/decisions",
		"/tenants/{tenantId}/projects/{projectId}/environments", "/tenants/{tenantId}/projects/{projectId}/deployments/{id}/cleanup",
		"/tenants/{tenantId}/projects/{projectId}/model-settings", "/tenants/{tenantId}/projects/{projectId}/mcp-servers",
		"/tenants/{tenantId}/projects/{projectId}/mcp-servers/{id}",
		"/tenants/{tenantId}/projects/{projectId}/work-items/{id}/dependencies",
		"/tenants/{tenantId}/projects/{projectId}/work-items/{id}/dependencies/{dependencyId}",
		"/tenants/{tenantId}/projects/{projectId}/chatops-destinations", "/tenants/{tenantId}/projects/{projectId}/chatops-destinations/{id}",
		"/tenants/{tenantId}/projects/{projectId}/chatops-deliveries", "/tenants/{tenantId}/projects/{projectId}/chatops-deliveries/{id}",
		"/tenants/{tenantId}/projects/{projectId}/chatops-deliveries/{id}/replay",
	} {
		if _, ok := doc.Paths[want]; !ok {
			t.Errorf("missing %s", want)
		}
	}
	text := string(data)
	for _, banned := range []string{"x-go-type", "common.yaml#", "kodama/contracts", "runtimev1"} {
		if strings.Contains(text, banned) {
			t.Errorf("the public document mentions %q", banned)
		}
	}
	if _, ok := doc.Components["securitySchemes"]["bearerAuth"]; !ok {
		t.Error("no bearer scheme")
	}
	// The outbound ChatOps payload is a receiver contract and the flow-summary
	// options belong to a list contribution nothing serves yet: a public client
	// must not grow types for an interface it cannot call.
	for section, names := range map[string][]string{
		"schemas":    {"WebhookPayload", "ChatopsWebhookPayload", "FlowSummary", "FlowWindow"},
		"parameters": {"IncludeFlowSummary", "FlowFrom", "FlowTo"},
	} {
		for _, n := range names {
			if _, ok := doc.Components[section][n]; ok {
				t.Errorf("components/%s/%s is not reachable from a public route and must not be published", section, n)
			}
		}
	}
	if _, ok := doc.Components["schemas"]["ChatopsEventType"]; !ok {
		t.Error("the ChatOps event type must not share a generated type name with the observation parameter EventType")
	}
}

// A schema that shares its name with a parameter of an earlier fragment would
// generate one Go type name twice; the fragments of typeNamespaced are renamed
// (with their references), the earlier ones keep their released names.
func TestTypeNameCollisionsAcrossSectionsAreRenamed(t *testing.T) {
	docs := map[string]map[string]any{
		"common": frag(t, commonDoc),
		"obs": frag(t, `
paths:
  /o:
    get:
      operationId: getO
      parameters: [{$ref: '#/components/parameters/Kind'}]
      responses: {"200": {description: ok}}
components:
  parameters:
    Kind: {name: kind, in: query, schema: {type: string}}
`),
		"chatops": frag(t, `
paths:
  /c:
    get:
      operationId: getC
      responses: {"200": {description: ok, content: {application/json: {schema: {$ref: '#/components/schemas/Holder'}}}}}
components:
  schemas:
    Kind: {type: string, enum: [a, b]}
    Holder: {type: object, properties: {kind: {$ref: '#/components/schemas/Kind'}}}
`),
	}
	out, err := bundle([]string{"common", "obs", "chatops"}, docs, CoreInfo)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Components map[string]map[string]any `yaml:"components"`
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc.Components["parameters"]["Kind"]; !ok {
		t.Fatalf("the earlier parameter keeps its name: %v", doc.Components)
	}
	if _, ok := doc.Components["schemas"]["ChatopsKind"]; !ok {
		t.Fatalf("the colliding schema is renamed with its fragment prefix: %v", doc.Components["schemas"])
	}
	if _, ok := doc.Components["schemas"]["Kind"]; ok {
		t.Fatal("the colliding schema also kept its name")
	}
	if !strings.Contains(string(out), "#/components/schemas/ChatopsKind") || strings.Contains(string(out), "#/components/schemas/Kind'") {
		t.Fatalf("references did not follow the rename:\n%s", out)
	}

	// A fragment outside typeNamespaced keeps the released behavior.
	docs["chatops"], docs["legacy"] = nil, docs["chatops"]
	delete(docs, "chatops")
	out, err = bundle([]string{"common", "obs", "legacy"}, docs, CoreInfo)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "    Kind:") || strings.Contains(string(out), "LegacyKind") {
		t.Fatalf("an exempt fragment was renamed:\n%s", out)
	}
}

func frag(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

const commonDoc = `
paths: {}
components:
  securitySchemes:
    bearerAuth: {type: http, scheme: bearer, description: common}
  schemas:
    ID: {type: string}
    Shared: {type: object}
`

func TestComponentCollisionsAreRenamedWithTheirReferences(t *testing.T) {
	docs := map[string]map[string]any{
		"common": frag(t, commonDoc),
		"a": frag(t, `
paths:
  /a:
    get:
      operationId: getA
      responses: {"200": {description: ok, content: {application/json: {schema: {$ref: '#/components/schemas/Thing'}}}}}
      parameters: [{name: x, in: query, schema: {$ref: 'common.yaml#/components/schemas/ID'}}]
components:
  securitySchemes:
    bearerAuth: {type: http, scheme: bearer, description: another words}
  schemas:
    Thing: {type: object, properties: {id: {$ref: 'common.yaml#/components/schemas/ID'}}}
    Shared: {type: object}
`),
		"bee": frag(t, `
paths:
  /b:
    get:
      operationId: getB
      responses: {"200": {description: ok, content: {application/json: {schema: {$ref: '#/components/schemas/Thing'}}}}}
components:
  schemas:
    Thing: {type: string}
    Shared: {type: object}
`),
	}
	out, err := bundle([]string{"common", "a", "bee"}, docs, CoreInfo)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths      map[string]map[string]map[string]any `yaml:"paths"`
		Components map[string]map[string]any            `yaml:"components"`
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	schemas := doc.Components["schemas"]
	if _, ok := schemas["Thing"]; !ok {
		t.Fatalf("the first definition keeps its name: %v", schemas)
	}
	if _, ok := schemas["BeeThing"]; !ok {
		t.Fatalf("the colliding definition is renamed with its fragment prefix: %v", schemas)
	}
	if n := len(schemas); n != 4 { // ID, Shared (shared, identical), Thing, BeeThing
		t.Fatalf("%d schemas: %v", n, schemas)
	}
	ref := func(path string) string {
		return doc.Paths[path]["get"]["responses"].(map[string]any)["200"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"].(map[string]any)["$ref"].(string)
	}
	if ref("/a") != "#/components/schemas/Thing" || ref("/b") != "#/components/schemas/BeeThing" {
		t.Fatalf("references did not follow the rename: %s %s", ref("/a"), ref("/b"))
	}
	if !strings.Contains(string(out), "#/components/schemas/ID") || strings.Contains(string(out), "common.yaml") {
		t.Fatal("references to the common fragment must become local")
	}
	if got := doc.Components["securitySchemes"]["bearerAuth"].(map[string]any)["description"]; got != "common" {
		t.Fatalf("the shared security scheme comes from the common fragment: %v", got)
	}
}

func TestAssemblyRejectsAmbiguousOrBrokenInput(t *testing.T) {
	op := func(id string) string {
		return `{get: {operationId: ` + id + `, responses: {"200": {description: ok}}}}`
	}
	for name, tc := range map[string]struct {
		docs map[string]string
		want string
	}{
		"a path defined twice":  {map[string]string{"a": "paths: {/x: " + op("a1") + "}", "b": "paths: {/x: " + op("b1") + "}"}, "more than one fragment"},
		"an operation ID twice": {map[string]string{"a": "paths: {/x: " + op("same") + "}", "b": "paths: {/y: " + op("same") + "}"}, "operationId same"},
		"no operation ID":       {map[string]string{"a": "paths: {/x: {get: {responses: {\"200\": {description: ok}}}}}"}, "no operationId"},
		"a dangling reference":  {map[string]string{"a": "paths: {/x: {get: {operationId: a1, responses: {\"200\": {$ref: '#/components/responses/Nope'}}}}}"}, "unresolved"},
		"a foreign file":        {map[string]string{"a": "paths: {/x: {get: {operationId: a1, responses: {\"200\": {$ref: 'other.yaml#/components/responses/X'}}}}}"}, "unsupported reference"},
	} {
		docs := map[string]map[string]any{"common": frag(t, commonDoc)}
		order := []string{"common"}
		for n, d := range tc.docs {
			docs[n] = frag(t, d)
			order = append(order, n)
		}
		// Deterministic order for the table.
		if len(order) == 3 && order[1] > order[2] {
			order[1], order[2] = order[2], order[1]
		}
		if _, err := bundle(order, docs, CoreInfo); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v (want %q)", name, err, tc.want)
		}
	}
}
