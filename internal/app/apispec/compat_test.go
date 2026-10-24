package apispec

import (
	"os"
	"strings"
	"testing"
)

const baselineDoc = `
openapi: 3.0.3
paths:
  /things/{id}:
    parameters:
      - {name: id, in: path, required: true, schema: {type: string}}
    get:
      operationId: getThing
      parameters:
        - {$ref: '#/components/parameters/Expand'}
      responses:
        "200": {description: ok, content: {application/json: {schema: {$ref: '#/components/schemas/Thing'}}}}
        "404": {$ref: '#/components/responses/NotFound'}
    put:
      operationId: putThing
      requestBody:
        required: true
        content: {application/json: {schema: {$ref: '#/components/schemas/ThingEdit'}}}
      responses:
        "200": {description: ok, content: {application/json: {schema: {$ref: '#/components/schemas/Thing'}}}}
components:
  parameters:
    Expand: {name: expand, in: query, schema: {type: string}}
  responses:
    NotFound: {description: nf, content: {application/json: {schema: {$ref: '#/components/schemas/Error'}}}}
  schemas:
    Error: {type: object, required: [code], properties: {code: {type: string}}}
    Thing:
      type: object
      required: [id, state]
      properties:
        id: {type: string}
        state: {$ref: '#/components/schemas/State'}
        tags: {type: array, items: {type: string}}
    State: {type: string, enum: [open, closed]}
    ThingEdit:
      type: object
      required: [name]
      properties:
        name: {type: string}
`

func summarize(t *testing.T, doc string) (Summary, []byte) {
	t.Helper()
	s, err := Summarize([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return s, []byte(doc)
}

func problems(t *testing.T, current string) []string {
	t.Helper()
	base, _ := summarize(t, baselineDoc)
	cur, data := summarize(t, current)
	return cur.CompatibleWith(base, data)
}

func TestSummaryOfTheBaselineIsCompatibleWithItself(t *testing.T) {
	if p := problems(t, baselineDoc); len(p) != 0 {
		t.Fatalf("a document is not compatible with itself: %v", p)
	}
}

func TestAdditiveChangesAreCompatible(t *testing.T) {
	cur := strings.Replace(baselineDoc, "components:", "  /others:\n    get:\n      operationId: listOthers\n      responses: {\"200\": {description: ok}}\ncomponents:", 1)
	cur = strings.NewReplacer(
		"state: {$ref: '#/components/schemas/State'}", "state: {$ref: '#/components/schemas/State'}\n        owner: {type: string}",
		"enum: [open, closed]", "enum: [open, closed, archived]",
		"Expand: {name: expand, in: query, schema: {type: string}}", "Expand: {name: expand, in: query, schema: {type: string}}\n    Limit: {name: limit, in: query, schema: {type: integer}}",
		"operationId: getThing\n      parameters:\n", "operationId: getThing\n      parameters:\n        - {$ref: '#/components/parameters/Limit'}\n",
		`"404": {$ref: '#/components/responses/NotFound'}`, "\"404\": {$ref: '#/components/responses/NotFound'}\n        \"429\": {description: slow}",
	).Replace(cur)
	if p := problems(t, cur); len(p) != 0 {
		t.Fatalf("additive changes were reported: %v", p)
	}
}

func TestBreakingChangesAreReported(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(string) string
		want string
	}{
		"a removed operation": {func(s string) string {
			return strings.Replace(s, "    put:\n      operationId: putThing\n      requestBody:\n        required: true\n        content: {application/json: {schema: {$ref: '#/components/schemas/ThingEdit'}}}\n      responses:\n        \"200\": {description: ok, content: {application/json: {schema: {$ref: '#/components/schemas/Thing'}}}}\n", "", 1)
		}, "PUT /things/{id} was removed"},
		"a renamed operation id": {func(s string) string { return strings.Replace(s, "getThing", "fetchThing", 1) }, "changed its identifier"},
		"a removed response status": {func(s string) string {
			return strings.Replace(s, `"404": {$ref: '#/components/responses/NotFound'}`, "", 1)
		}, "no longer answers 404"},
		"a removed member": {func(s string) string {
			return strings.Replace(s, "        tags: {type: array, items: {type: string}}\n", "", 1)
		}, "lost member tags"},
		"a retyped member": {func(s string) string {
			return strings.Replace(s, "id: {type: string}\n        state", "id: {type: integer}\n        state", 1)
		}, "member id changed"},
		"a removed enum value": {func(s string) string { return strings.Replace(s, "enum: [open, closed]", "enum: [open]", 1) }, "lost the enum value"},
		"a new required request member": {func(s string) string {
			return strings.Replace(s, "required: [name]\n      properties:\n        name: {type: string}", "required: [name, color]\n      properties:\n        name: {type: string}\n        color: {type: string}", 1)
		}, "gained the required member color"},
		"a removed schema": {func(s string) string {
			return strings.Replace(s, "    State: {type: string, enum: [open, closed]}\n", "", 1)
		}, "schema State was removed"},
	} {
		p := problems(t, tc.edit(baselineDoc))
		found := false
		for _, line := range p {
			if strings.Contains(line, tc.want) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: not reported (%v)", name, p)
		}
	}
}

func TestNewRequiredParameterAndBodyAreReported(t *testing.T) {
	withParam := strings.Replace(baselineDoc, "      parameters:\n        - {$ref: '#/components/parameters/Expand'}\n",
		"      parameters:\n        - {$ref: '#/components/parameters/Expand'}\n        - {name: tenant, in: header, required: true, schema: {type: string}}\n", 1)
	if p := problems(t, withParam); len(p) != 1 || !strings.Contains(p[0], "gained the required parameter header:tenant") {
		t.Fatalf("%v", p)
	}
	relaxed := strings.Replace(baselineDoc, "        required: true\n        content", "        required: false\n        content", 1)
	if p := problems(t, relaxed); len(p) != 0 {
		t.Fatalf("a relaxed body is compatible: %v", p)
	}
	// Optional before, required now.
	base := strings.Replace(baselineDoc, "        required: true\n        content", "        content", 1)
	b, _ := summarize(t, base)
	cur, data := summarize(t, baselineDoc)
	if p := cur.CompatibleWith(b, data); len(p) != 1 || !strings.Contains(p[0], "now requires its request body") {
		t.Fatalf("%v", p)
	}
}

func TestCommittedBaselineIsStillHonoured(t *testing.T) {
	raw, err := os.ReadFile("../testdata/api-baseline-t072.json")
	if err != nil {
		t.Fatal(err)
	}
	base, err := ParseSummary(raw)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(apiDir + "/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cur, err := Summarize(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(base.Operations) < 50 || len(base.Schemas) < 100 {
		t.Fatalf("the baseline is too small to mean anything: %d operations, %d schemas", len(base.Operations), len(base.Schemas))
	}
	if p := cur.CompatibleWith(base, data); len(p) != 0 {
		t.Fatalf("the aggregate breaks the committed baseline:\n%s", strings.Join(p, "\n"))
	}
}

func TestPrunedFragmentsKeepOnlyPublicComponents(t *testing.T) {
	docs := map[string]map[string]any{
		"common": frag(t, commonDoc),
		"runtimey": frag(t, `
paths:
  /pub:
    get:
      operationId: getPub
      responses: {"200": {description: ok, content: {application/json: {schema: {$ref: '#/components/schemas/Public'}}}}}
  /attempts/{id}/stream:
    post:
      operationId: stream
      requestBody: {content: {application/json: {schema: {$ref: '#/components/schemas/StreamRequest'}}}}
      responses: {"200": {description: ok}}
components:
  securitySchemes:
    attemptToken: {type: http, scheme: bearer}
  schemas:
    Public: {type: object, properties: {inner: {$ref: '#/components/schemas/Inner'}}}
    Inner: {type: object}
    StreamRequest: {type: object}
`),
	}
	prunedFragments["runtimey"] = true
	defer delete(prunedFragments, "runtimey")
	out, err := bundle([]string{"common", "runtimey"}, docs, CoreInfo)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, kept := range []string{"Public:", "Inner:", "/pub"} {
		if !strings.Contains(text, kept) {
			t.Errorf("%s was pruned", kept)
		}
	}
	for _, dropped := range []string{"StreamRequest", "attemptToken", "/attempts/"} {
		if strings.Contains(text, dropped) {
			t.Errorf("%s reached the public document", dropped)
		}
	}
}
