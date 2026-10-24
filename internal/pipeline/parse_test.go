package pipeline_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/pipeline"
)

func wantParse(t *testing.T, err error, stage pipeline.Stage, code string) {
	t.Helper()
	var ve *pipeline.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected a validation error with %q, got %v", code, err)
	}
	if ve.Stage != stage || !hasCode(err, code) {
		t.Fatalf("expected %s/%s, got %s %v", stage, code, ve.Stage, codes(err))
	}
}

func Test_parser_strictness(t *testing.T) {
	step := container("a")
	base := doc(step)
	opts := pipeline.DefaultOptions()

	t.Run("a valid YAML and JSON pair parse to the same canonical content", func(t *testing.T) {
		e := newEnv(t)
		fromYAML := e.mustValidate(t, base)
		fromJSON, err := e.v.ValidateJSON(t.Context(), scope, actor, []byte(`{"apiVersion":"kodama/v1alpha1","kind":"Pipeline","metadata":{"name":"sample"},
			"spec":{"steps":[{"id":"a","kind":"container","image":"`+img+`","command":["/bin/sh","-ec"],"timeoutSeconds":60}]}}`))
		if err != nil {
			t.Fatal(err)
		}
		if fromYAML.Digest() != fromJSON.Digest() {
			t.Fatal("YAML and JSON forms of one document must share a digest")
		}
	})

	yamlCases := map[string]struct {
		src   string
		stage pipeline.Stage
		code  string
	}{
		"duplicate top-level key":   {"apiVersion: kodama/v1alpha1\napiVersion: kodama/v1alpha1\nkind: Pipeline\nmetadata: {name: p}\nspec: {steps: []}\n", pipeline.StageSyntax, pipeline.CodeDuplicateKey},
		"duplicate nested key":      {strings.Replace(base, "metadata: {name: sample}", "metadata: {name: a, name: b}", 1), pipeline.StageSyntax, pipeline.CodeDuplicateKey},
		"duplicate step field":      {doc("{id: a, id: b, kind: container}"), pipeline.StageSyntax, pipeline.CodeDuplicateKey},
		"duplicate env name":        {doc(container("a", "env: {A: x, A: y}")), pipeline.StageSyntax, pipeline.CodeDuplicateKey},
		"unknown top-level field":   {base + "extra: 1\n", pipeline.StageSyntax, pipeline.CodeUnknownField},
		"unknown spec field":        {strings.Replace(base, "spec:\n", "spec:\n  triggers: []\n", 1), pipeline.StageSyntax, pipeline.CodeUnknownField},
		"unknown step field":        {doc(container("a", "retries: 3")), pipeline.StageSyntax, pipeline.CodeUnknownField},
		"unknown metadata field":    {strings.Replace(base, "metadata: {name: sample}", "metadata: {name: sample, labels: {}}", 1), pipeline.StageSyntax, pipeline.CodeUnknownField},
		"multiple documents":        {base + "---\n" + base, pipeline.StageSyntax, pipeline.CodeMultiple},
		"trailing document marker":  {base + "---\nfoo: bar\n", pipeline.StageSyntax, pipeline.CodeMultiple},
		"empty":                     {"", pipeline.StageSyntax, pipeline.CodeInvalidDocument},
		"only comments":             {"# nothing\n", pipeline.StageSyntax, pipeline.CodeInvalidDocument},
		"malformed YAML":            {"apiVersion: [unclosed\n", pipeline.StageSyntax, pipeline.CodeInvalidDocument},
		"a list at the root":        {"- a\n- b\n", pipeline.StageSyntax, pipeline.CodeInvalidType},
		"alias":                     {strings.Replace(doc(container("a"), container("b")), "command: [/bin/sh, -ec]", "command: &c [/bin/sh, -ec]", 1) + "", pipeline.StageSyntax, pipeline.CodeUnsupportedYAML},
		"anchor and alias":          {"a: &x 1\nb: *x\n", pipeline.StageSyntax, pipeline.CodeUnsupportedYAML},
		"merge key":                 {strings.Replace(base, "metadata: {name: sample}", "metadata:\n  <<: {name: sample}", 1), pipeline.StageSyntax, pipeline.CodeUnsupportedYAML},
		"explicit tag":              {strings.Replace(base, "name: sample", "name: !!binary c2FtcGxl", 1), pipeline.StageSyntax, pipeline.CodeUnsupportedYAML},
		"custom tag":                {strings.Replace(base, "name: sample", "name: !custom sample", 1), pipeline.StageSyntax, pipeline.CodeUnsupportedYAML},
		"null value":                {strings.Replace(base, "inputs: {}", "inputs:", 1), pipeline.StageSyntax, pipeline.CodeInvalidType},
		"float timeout":             {strings.Replace(base, "timeoutSeconds: 60", "timeoutSeconds: 60.5", 1), pipeline.StageSyntax, pipeline.CodeInvalidType},
		"exponent timeout":          {strings.Replace(base, "timeoutSeconds: 60", "timeoutSeconds: 6e1", 1), pipeline.StageSyntax, pipeline.CodeInvalidType},
		"hex integer":               {strings.Replace(base, "timeoutSeconds: 60", "timeoutSeconds: 0x3c", 1), pipeline.StageSyntax, pipeline.CodeInvalidFormat},
		"octal looking integer":     {strings.Replace(base, "timeoutSeconds: 60", "timeoutSeconds: 060", 1), pipeline.StageSyntax, pipeline.CodeInvalidFormat},
		"integer beyond safe range": {strings.Replace(base, "timeoutSeconds: 60", "timeoutSeconds: 9007199254740993", 1), pipeline.StageSyntax, pipeline.CodeInvalidValue},
		"string timeout":            {strings.Replace(base, "timeoutSeconds: 60", "timeoutSeconds: '60'", 1), pipeline.StageSyntax, pipeline.CodeInvalidType},
		"capitalized boolean":       {docWith("{a: {type: string, required: True}}", step), pipeline.StageSyntax, pipeline.CodeInvalidFormat},
		"yes is not a boolean":      {docWith("{a: {type: string, required: yes}}", step), pipeline.StageSyntax, pipeline.CodeInvalidType},
		"object where text":         {doc(container("a", "args: [{x: y}]")), pipeline.StageSyntax, pipeline.CodeInvalidType},
		"number key":                {strings.Replace(base, "metadata: {name: sample}", "metadata: {1: sample}", 1), pipeline.StageSyntax, pipeline.CodeInvalidType},
		"missing name":              {strings.Replace(base, "metadata: {name: sample}", "metadata: {}", 1), pipeline.StageSyntax, pipeline.CodeMissingField},
		"missing steps":             {"apiVersion: kodama/v1alpha1\nkind: Pipeline\nmetadata: {name: p}\nspec: {}\n", pipeline.StageSyntax, pipeline.CodeMissingField},
		"invalid UTF-8":             {"apiVersion: kodama/v1alpha1\n\xff\xfe: x\n", pipeline.StageSyntax, pipeline.CodeInvalidDocument},
	}
	// "alias" above only anchors; use a real alias.
	yamlCases["alias"] = struct {
		src   string
		stage pipeline.Stage
		code  string
	}{"apiVersion: kodama/v1alpha1\nkind: Pipeline\nmetadata: {name: p}\nspec:\n  steps:\n    - &s {id: a, kind: container}\n    - *s\n", pipeline.StageSyntax, pipeline.CodeUnsupportedYAML}
	for name, c := range yamlCases {
		t.Run("yaml "+name, func(t *testing.T) {
			_, err := pipeline.ParseYAML([]byte(c.src), opts)
			wantParse(t, err, c.stage, c.code)
		})
	}

	jsonOK := `{"apiVersion":"kodama/v1alpha1","kind":"Pipeline","metadata":{"name":"p"},"spec":{"steps":[{"id":"a","kind":"container","image":"` + img + `","command":["x"],"timeoutSeconds":5}]}}`
	jsonCases := map[string]struct{ src, code string }{
		"duplicate key":         {`{"apiVersion":"kodama/v1alpha1","apiVersion":"kodama/v1alpha1"}`, pipeline.CodeDuplicateKey},
		"duplicate nested key":  {strings.Replace(jsonOK, `"metadata":{"name":"p"}`, `"metadata":{"name":"p","name":"q"}`, 1), pipeline.CodeDuplicateKey},
		"unknown field":         {strings.Replace(jsonOK, `"kind":"Pipeline"`, `"kind":"Pipeline","extra":1`, 1), pipeline.CodeUnknownField},
		"trailing document":     {jsonOK + jsonOK, pipeline.CodeMultiple},
		"trailing garbage":      {jsonOK + " x", pipeline.CodeMultiple},
		"empty":                 {"  ", pipeline.CodeInvalidDocument},
		"null":                  {strings.Replace(jsonOK, `"timeoutSeconds":5`, `"timeoutSeconds":null`, 1), pipeline.CodeInvalidType},
		"fraction":              {strings.Replace(jsonOK, `"timeoutSeconds":5`, `"timeoutSeconds":5.0`, 1), pipeline.CodeInvalidType},
		"exponent":              {strings.Replace(jsonOK, `"timeoutSeconds":5`, `"timeoutSeconds":5e0`, 1), pipeline.CodeInvalidType},
		"leading zero":          {strings.Replace(jsonOK, `"timeoutSeconds":5`, `"timeoutSeconds":05`, 1), pipeline.CodeInvalidFormat},
		"unsafe integer":        {strings.Replace(jsonOK, `"timeoutSeconds":5`, `"timeoutSeconds":9007199254740993`, 1), pipeline.CodeInvalidValue},
		"negative zero":         {strings.Replace(jsonOK, `"timeoutSeconds":5`, `"timeoutSeconds":-0`, 1), pipeline.CodeInvalidValue},
		"lone high surrogate":   {strings.Replace(jsonOK, `"name":"p"`, `"name":"\ud800"`, 1), pipeline.CodeInvalidDocument},
		"lone low surrogate":    {strings.Replace(jsonOK, `"name":"p"`, `"name":"\udc00"`, 1), pipeline.CodeInvalidDocument},
		"raw control character": {strings.Replace(jsonOK, `"name":"p"`, "\"name\":\"a\tb\"", 1), pipeline.CodeInvalidDocument},
		"trailing comma":        {`{"apiVersion":"kodama/v1alpha1",}`, pipeline.CodeInvalidDocument},
		"single quotes":         {`{'a': 1}`, pipeline.CodeInvalidDocument},
		"unquoted key":          {`{a: 1}`, pipeline.CodeInvalidDocument},
		"truncated":             {jsonOK[:40], pipeline.CodeInvalidDocument},
		"array root":            {`[]`, pipeline.CodeInvalidType},
		"invalid UTF-8":         {"{\"a\":\"\xff\"}", pipeline.CodeInvalidDocument},
		"invalid escape":        {`{"a":"\q"}`, pipeline.CodeInvalidDocument},
	}
	for name, c := range jsonCases {
		t.Run("json "+name, func(t *testing.T) {
			_, err := pipeline.ParseJSON([]byte(c.src), opts)
			wantParse(t, err, pipeline.StageSyntax, c.code)
		})
	}
	if _, err := pipeline.ParseJSON([]byte(jsonOK), opts); err != nil {
		t.Fatalf("the control JSON document must parse: %v", err)
	}

	t.Run("bounds", func(t *testing.T) {
		_, err := pipeline.ParseYAML([]byte(base), pipeline.Options{MaxBytes: 50})
		wantParse(t, err, pipeline.StageSyntax, pipeline.CodeTooLarge)
		_, err = pipeline.ParseJSON([]byte(jsonOK), pipeline.Options{MaxBytes: 50})
		wantParse(t, err, pipeline.StageSyntax, pipeline.CodeTooLarge)
		_, err = pipeline.ParseYAML([]byte(base), pipeline.Options{MaxDepth: 3})
		wantParse(t, err, pipeline.StageSyntax, pipeline.CodeTooDeep)
		_, err = pipeline.ParseJSON([]byte(jsonOK), pipeline.Options{MaxDepth: 3})
		wantParse(t, err, pipeline.StageSyntax, pipeline.CodeTooDeep)
		_, err = pipeline.ParseYAML([]byte(base), pipeline.Options{MaxNodes: 10})
		wantParse(t, err, pipeline.StageSyntax, pipeline.CodeTooManyNodes)
		_, err = pipeline.ParseJSON([]byte(jsonOK), pipeline.Options{MaxNodes: 10})
		wantParse(t, err, pipeline.StageSyntax, pipeline.CodeTooManyNodes)
		deep := strings.Repeat("[", 5000) + strings.Repeat("]", 5000)
		_, err = pipeline.ParseJSON([]byte(deep), opts)
		wantParse(t, err, pipeline.StageSyntax, pipeline.CodeTooDeep)
		_, err = pipeline.ParseYAML([]byte(deep), opts)
		wantParse(t, err, pipeline.StageSyntax, pipeline.CodeTooDeep)
	})

	t.Run("integers keep their precision", func(t *testing.T) {
		src := docWith("{big: {type: integer, default: 9007199254740991}, low: {type: integer, default: -9007199254740991}}", step)
		d, err := pipeline.ParseYAML([]byte(src), opts)
		if err != nil {
			t.Fatal(err)
		}
		if d.Spec.Inputs["big"].Default.Int != 9007199254740991 || d.Spec.Inputs["low"].Default.Int != -9007199254740991 {
			t.Fatalf("integer precision was lost: %+v", d.Spec.Inputs)
		}
		j, err := pipeline.ParseJSON([]byte(`{"apiVersion":"kodama/v1alpha1","kind":"Pipeline","metadata":{"name":"p"},"spec":{"inputs":{"big":{"type":"integer","default":9007199254740991}},"steps":[{"id":"a","kind":"container","image":"`+img+`","command":["x"],"timeoutSeconds":5}]}}`), opts)
		if err != nil || j.Spec.Inputs["big"].Default.Int != 9007199254740991 {
			t.Fatalf("JSON integer precision was lost: %v %+v", err, j.Spec.Inputs)
		}
	})

	t.Run("absent and explicit values stay distinguishable", func(t *testing.T) {
		d, err := pipeline.ParseYAML([]byte(docWith("{a: {type: boolean, default: false}, b: {type: boolean}}", step)), opts)
		if err != nil {
			t.Fatal(err)
		}
		if d.Spec.Inputs["a"].Default == nil || d.Spec.Inputs["a"].Default.Bool {
			t.Fatal("an explicit false default must be present")
		}
		if d.Spec.Inputs["b"].Default != nil {
			t.Fatal("an absent default must stay absent")
		}
		if d.Spec.Steps[0].Workspace != nil || d.Spec.Steps[0].Retry != nil {
			t.Fatal("parsing must not apply defaults; normalization does")
		}
	})

	t.Run("strings that look like other types stay strings when quoted", func(t *testing.T) {
		d, err := pipeline.ParseYAML([]byte(docWith("{a: {type: string, default: 'true'}, b: {type: string, default: '123'}, c: {type: string, default: 'null'}}", step)), opts)
		if err != nil {
			t.Fatal(err)
		}
		if d.Spec.Inputs["a"].Default.Str != "true" || d.Spec.Inputs["b"].Default.Str != "123" || d.Spec.Inputs["c"].Default.Str != "null" {
			t.Fatalf("quoted scalars changed type: %+v", d.Spec.Inputs)
		}
	})

	t.Run("syntax diagnostics never echo content", func(t *testing.T) {
		secret := "hunter2-secret-value"
		for _, src := range []string{
			strings.Replace(base, "timeoutSeconds: 60", "timeoutSeconds: "+secret, 1),
			strings.Replace(base, "metadata: {name: sample}", "metadata: {name: sample, "+secret+": 1}", 1),
			strings.Replace(base, "name: sample", "name: &"+secret+" sample", 1),
		} {
			_, err := pipeline.ParseYAML([]byte(src), opts)
			if err == nil {
				t.Fatal("expected a failure")
			}
			var ve *pipeline.ValidationError
			if !errors.As(err, &ve) {
				t.Fatal(err)
			}
			for _, d := range ve.Diagnostics {
				if strings.Contains(d.Message, secret) {
					t.Fatalf("message echoes content: %+v", d)
				}
			}
		}
	})

	t.Run("hostile keys cannot inflate paths", func(t *testing.T) {
		long := strings.Repeat("x", 5000)
		_, err := pipeline.ParseYAML([]byte(strings.Replace(base, "metadata: {name: sample}", "metadata: {name: sample, "+long+": 1}", 1)), opts)
		var ve *pipeline.ValidationError
		if !errors.As(err, &ve) {
			t.Fatal(err)
		}
		for _, d := range ve.Diagnostics {
			if len(d.Path) > 200 {
				t.Fatalf("path is unbounded: %d bytes", len(d.Path))
			}
		}
	})
}
