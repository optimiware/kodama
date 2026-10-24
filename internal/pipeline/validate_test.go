package pipeline_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/pipeline"
)

func Test_cycle(t *testing.T) {
	e := newEnv(t)
	for name, src := range map[string]string{
		"self":       doc(container("a", "needs: [a]")),
		"two nodes":  doc(container("a", "needs: [b]"), container("b", "needs: [a]")),
		"long":       doc(container("a", "needs: [c]"), container("b", "needs: [a]"), container("c", "needs: [b]"), container("d")),
		"downstream": doc(container("a", "needs: [b]"), container("b", "needs: [a]"), container("c", "needs: [a]")),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := e.validate(src)
			expectCode(t, err, pipeline.CodeCycle)
			var ve *pipeline.ValidationError
			if !errors.As(err, &ve) || ve.Stage != pipeline.StageSemantic {
				t.Fatalf("a cycle is a semantic failure: %v", err)
			}
		})
	}
	t.Run("cycle message names the members", func(t *testing.T) {
		_, err := e.validate(doc(container("a", "needs: [b]"), container("b", "needs: [a]")))
		var ve *pipeline.ValidationError
		if !errors.As(err, &ve) || !strings.Contains(ve.Diagnostics[0].Message, "a -> b -> a") && !strings.Contains(ve.Diagnostics[0].Message, "b -> a -> b") {
			t.Fatalf("message does not name the cycle: %v", err)
		}
	})
	t.Run("diamond is acyclic", func(t *testing.T) {
		e.mustValidate(t, doc(
			container("root"),
			container("left", "needs: [root]"), container("right", "needs: [root]"),
			container("join", "needs: [left, right]"),
		))
	})
	t.Run("unknown dependency", func(t *testing.T) {
		_, err := e.validate(doc(container("a", "needs: [ghost]")))
		expectCode(t, err, pipeline.CodeUnknownStep)
	})
}

func Test_conditional_gate(t *testing.T) {
	gateOnly := doc(container("build"), "{id: review, kind: gate, needs: [build]}")
	gateEnabled := newEnv(t) // gate capability enabled by the fixture
	t.Run("valid gate is accepted when enabled", func(t *testing.T) {
		gateEnabled.mustValidate(t, gateOnly)
	})
	t.Run("valid gate is unavailable under M1 capabilities", func(t *testing.T) {
		m1 := newEnv(t, pipeline.KindContainer)
		_, err := m1.validate(gateOnly)
		expectCode(t, err, pipeline.CodeKindDisabled)
		if !hasDiag(err, pipeline.CodeKindDisabled, "steps[1]") || hasDiag(err, pipeline.CodeKindDisabled, "steps[0]") {
			t.Fatalf("only the gate step should be disabled: %v", err)
		}
	})
	for name, field := range map[string]string{
		"when":        "when: {input: deploy, equals: true}",
		"image":       "image: " + img,
		"command":     "command: [x]",
		"workspace":   "workspace: {access: read, from: run}",
		"retry":       "retry: {maxAttempts: 2, on: [transient]}",
		"outputs":     "outputs: {r: {type: string}}",
		"timeout":     "timeoutSeconds: 10",
		"env":         "env: {A: b}",
		"secretFiles": "secretFiles: {a: secret://project/token}",
		"environment": "environment: env_staging",
		"agent":       "agent: {profile: coding-v1}",
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			src := docWith("{deploy: {type: boolean, default: false}}", container("build"), "{id: review, kind: gate, needs: [build], gate: {}, "+field+"}")
			_, err := gateEnabled.validate(src)
			want := pipeline.CodeGateField
			if name == "when" {
				want = pipeline.CodeConditionalGate
			}
			expectCode(t, err, want)
		})
	}
	t.Run("a gate is not skippable by a skipped ancestor's condition being copied", func(t *testing.T) {
		// A condition on the gate is the only way an author could make it
		// skippable; the conditional step before it remains legal.
		gateEnabled.mustValidate(t, docWith("{deploy: {type: boolean, default: true}}",
			container("build", "when: {input: deploy, equals: true}"), "{id: review, kind: gate, needs: [build]}"))
	})
	t.Run("a Document built in code cannot smuggle a condition onto a gate", func(t *testing.T) {
		d, err := gateEnabled.v.ParseYAML([]byte(gateOnly))
		if err != nil {
			t.Fatal(err)
		}
		d.Spec.Steps[1].When = &pipeline.Condition{Input: "x", Equals: pipeline.BoolScalar(true)}
		_, err = gateEnabled.v.Validate(t.Context(), scope, actor, d)
		expectCode(t, err, pipeline.CodeConditionalGate)
	})
}

func Test_mutable_image(t *testing.T) {
	e := newEnv(t)
	step := func(image string) string {
		return "{id: a, kind: container, image: \"" + image + "\", command: [x], timeoutSeconds: 5}"
	}
	for name, image := range map[string]string{
		"tag only":           "registry.example.invalid/go-tools:1.24",
		"latest":             "registry.example.invalid/go-tools:latest",
		"no tag":             "registry.example.invalid/go-tools",
		"short digest":       "registry.example.invalid/go-tools@sha256:1111",
		"uppercase digest":   "registry.example.invalid/go-tools@sha256:" + strings.Repeat("A", 64),
		"other algorithm":    "registry.example.invalid/go-tools@sha512:" + strings.Repeat("1", 64),
		"digest then suffix": "registry.example.invalid/go-tools@sha256:" + strings.Repeat("1", 64) + ":latest",
		"whitespace":         "registry.example.invalid/go-tools @sha256:" + strings.Repeat("1", 64),
		"empty repository":   "@sha256:" + strings.Repeat("1", 64),
		"expression":         "${{ inputs.image }}",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := e.validate(doc(step(image)))
			expectCode(t, err, pipeline.CodeMutableImage)
		})
	}
	t.Run("pinned images are accepted without a registry", func(t *testing.T) {
		for _, image := range []string{
			img,
			"go-tools@" + digest1,
			"registry.example.invalid:5000/team/go-tools:1.24@" + digest1, // a tag next to the digest is still immutable
		} {
			e.mustValidate(t, doc(step(image)))
		}
	})
	t.Run("a Document built in code is checked too", func(t *testing.T) {
		d, _ := e.v.ParseYAML([]byte(doc(container("a"))))
		d.Spec.Steps[0].Image = "repo:tag"
		_, err := e.v.Validate(t.Context(), scope, actor, d)
		expectCode(t, err, pipeline.CodeMutableImage)
	})
}

func Test_definition_rules(t *testing.T) {
	e := newEnv(t)
	ok := func(name, src string) {
		t.Run("accepts "+name, func(t *testing.T) { e.mustValidate(t, src) })
	}
	bad := func(name, code, src string) {
		t.Run("rejects "+name, func(t *testing.T) {
			_, err := e.validate(src)
			expectCode(t, err, code)
		})
	}
	longName := strings.Repeat("a", 64)
	bad("uppercase step id", pipeline.CodeInvalidFormat, doc("{id: Build, kind: container, image: "+img+", command: [x], timeoutSeconds: 5}"))
	bad("step id with underscore", pipeline.CodeInvalidFormat, doc("{id: a_b, kind: container, image: "+img+", command: [x], timeoutSeconds: 5}"))
	bad("step id too long", pipeline.CodeInvalidFormat, doc("{id: "+longName+", kind: container, image: "+img+", command: [x], timeoutSeconds: 5}"))
	ok("63 character id", doc(container(strings.Repeat("a", 63))))
	bad("duplicate step ids", pipeline.CodeDuplicateStep, doc(container("a"), container("a")))
	bad("run as a step id", pipeline.CodeReservedID, doc(container("run")))
	bad("pipeline name starting with a digit", pipeline.CodeInvalidFormat, strings.Replace(doc(container("a")), "name: sample", "name: 1sample", 1))

	// Inputs: typing, defaults and conditions.
	ok("typed defaults", docWith("{s: {type: string, default: x}, b: {type: boolean, default: true}, i: {type: integer, default: 3}, a: {type: artifact, required: true}}", container("a")))
	bad("string default on an integer", pipeline.CodeTypeMismatch, docWith("{i: {type: integer, default: three}}", container("a")))
	bad("integer default on a string", pipeline.CodeTypeMismatch, docWith("{s: {type: string, default: 3}}", container("a")))
	bad("artifact default", pipeline.CodeInvalidValue, docWith("{a: {type: artifact, default: x}}", container("a")))
	bad("unknown input type", pipeline.CodeInvalidValue, docWith("{a: {type: float}}", container("a")))
	bad("input name with uppercase", pipeline.CodeInvalidFormat, docWith("{Foo: {type: string}}", container("a")))
	bad("expression in a default", pipeline.CodeInterpolation, docWith("{s: {type: string, default: '${{ inputs.s }}'}}", container("a")))
	ok("condition on a matching input", docWith("{deploy: {type: boolean, default: false}}", container("a", "when: {input: deploy, equals: true}")))
	ok("condition on a string input", docWith("{mode: {type: string}}", container("a", "when: {input: mode, equals: fast}")))
	bad("condition on an undeclared input", pipeline.CodeUnknownInput, doc(container("a", "when: {input: nope, equals: true}")))
	bad("condition value of the wrong type", pipeline.CodeTypeMismatch, docWith("{deploy: {type: boolean}}", container("a", "when: {input: deploy, equals: \"true\"}")))
	bad("condition on an artifact input", pipeline.CodeInvalidWhen, docWith("{a: {type: artifact}}", container("a", "when: {input: a, equals: x}")))
	bad("condition with an extra field", pipeline.CodeUnknownField, docWith("{d: {type: boolean}}", container("a", "when: {input: d, equals: true, not: true}")))

	// Retry.
	ok("default retry", doc(container("a")))
	ok("bounded retry", doc(container("a", "retry: {maxAttempts: 3, backoffSeconds: 30, on: [infrastructure, transient]}")))
	bad("zero attempts", pipeline.CodeInvalidValue, doc(container("a", "retry: {maxAttempts: 0, on: [transient]}")))
	bad("too many attempts", pipeline.CodeInvalidRetry, doc(container("a", "retry: {maxAttempts: 11, on: [transient]}")))
	bad("negative backoff", pipeline.CodeInvalidValue, doc(container("a", "retry: {maxAttempts: 2, backoffSeconds: -1, on: [transient]}")))
	bad("backoff above the limit", pipeline.CodeInvalidRetry, doc(container("a", "retry: {maxAttempts: 2, backoffSeconds: 3601, on: [transient]}")))
	bad("command failure is not a retry class", pipeline.CodeInvalidValue, doc(container("a", "retry: {maxAttempts: 2, on: [command]}")))
	bad("retries without a class", pipeline.CodeInvalidRetry, doc(container("a", "retry: {maxAttempts: 2}")))
	bad("duplicate retry class", pipeline.CodeInvalidValue, doc(container("a", "retry: {maxAttempts: 2, on: [transient, transient]}")))

	// Timeouts.
	bad("zero timeout", pipeline.CodeInvalidValue, doc("{id: a, kind: container, image: "+img+", command: [x], timeoutSeconds: 0}"))
	bad("timeout above the limit", pipeline.CodeInvalidTimeout, doc("{id: a, kind: container, image: "+img+", command: [x], timeoutSeconds: 86401}"))
	bad("missing timeout", pipeline.CodeMissingField, doc("{id: a, kind: container, image: "+img+", command: [x]}"))
	bad("string command", pipeline.CodeInvalidType, doc("{id: a, kind: container, image: "+img+", command: 'sh -c x', timeoutSeconds: 5}"))
	bad("empty command", pipeline.CodeInvalidValue, doc("{id: a, kind: container, image: "+img+", command: [], timeoutSeconds: 5}"))

	// Output paths.
	for _, p := range []string{"/kodama/outputs/report.json", "/kodama/outputs/dir/report.json"} {
		ok("output path "+p, doc(container("a", "outputs: {r: {type: artifact, path: "+p+"}}")))
	}
	for _, p := range []string{
		"/kodama/outputs", "/kodama/outputs/", "/tmp/report.json", "/kodama/outputs/../secrets/x", "/kodama/outputs/a/../../x",
		"/kodama/outputs/./x", "/kodama/outputs//x", "kodama/outputs/x", "/kodama/outputsx/y", "/kodama/outputs/x/", "/kodama/results.json",
	} {
		bad("output path "+p, pipeline.CodeInvalidOutput, doc(container("a", "outputs: {r: {type: artifact, path: \""+p+"\"}}")))
	}
	bad("artifact without a path", pipeline.CodeMissingField, doc(container("a", "outputs: {r: {type: artifact}}")))
	bad("scalar output with a path", pipeline.CodeInvalidOutput, doc(container("a", "outputs: {r: {type: string, path: /kodama/outputs/x}}")))
	bad("duplicate output paths", pipeline.CodeInvalidOutput, doc(container("a", "outputs: {r: {type: artifact, path: /kodama/outputs/x}, s: {type: artifact, path: /kodama/outputs/x}}")))
	bad("nested output paths", pipeline.CodeInvalidOutput, doc(container("a", "outputs: {r: {type: artifact, path: /kodama/outputs/x}, s: {type: artifact, path: /kodama/outputs/x/y}}")))
	bad("unknown output type", pipeline.CodeInvalidValue, doc(container("a", "outputs: {r: {type: blob}}")))
	bad("output name with uppercase", pipeline.CodeInvalidFormat, doc(container("a", "outputs: {Report: {type: string}}")))
	ok("oci output", doc(container("a", "outputs: {image: {type: ociArtifact}}")))

	// Forbidden interpolation.
	bad("interpolation in an image", pipeline.CodeMutableImage, doc("{id: a, kind: container, image: 'x@${{ inputs.d }}', command: [x], timeoutSeconds: 5}"))
	bad("expression in a command", pipeline.CodeInterpolation, docWith("{s: {type: string}}", "{id: a, kind: container, image: "+img+", command: ['${{ inputs.s }}'], timeoutSeconds: 5}"))
	bad("interpolated argument", pipeline.CodeInterpolation, docWith("{s: {type: string}}", "{id: a, kind: container, image: "+img+", command: [x], args: ['echo ${{ inputs.s }}'], timeoutSeconds: 5}"))
	bad("interpolated environment value", pipeline.CodeInterpolation, docWith("{s: {type: string}}", container("a", "env: {A: 'x-${{ inputs.s }}'}")))
	bad("malformed expression", pipeline.CodeInterpolation, docWith("{s: {type: string}}", container("a", "env: {A: '${{ inputs.s | upper }}'}")))
	bad("expression in a workspace source", pipeline.CodeInvalidFormat, doc(container("a", "workspace: {access: read, from: '${{ inputs.s }}'}")))
	ok("whole-scalar input reference", docWith("{s: {type: string}}", container("a", "env: {A: '${{ inputs.s }}'}", "args: ['${{ inputs.s }}']")))
	ok("loose spacing in a reference", docWith("{s: {type: string}}", container("a", "env: {A: '${{inputs.s}}'}")))
	bad("expression naming an undeclared input", pipeline.CodeUnknownInput, doc(container("a", "env: {A: '${{ inputs.s }}'}")))

	// Workspace sources.
	ok("run source", doc(container("a", "workspace: {access: read, from: run}")))
	bad("none with a source", pipeline.CodeInvalidSource, doc(container("a", "workspace: {access: none, from: run}")))
	bad("read without a source", pipeline.CodeMissingField, doc(container("a", "workspace: {access: read}")))
	bad("unknown access", pipeline.CodeInvalidValue, doc(container("a", "workspace: {access: append, from: run}")))
	bad("source naming a missing step", pipeline.CodeInvalidSource, doc(container("a", "workspace: {access: read, from: ghost}")))
	bad("source naming a reader", pipeline.CodeInvalidSource, doc(container("r", "workspace: {access: read, from: run}"), container("a", "needs: [r]", "workspace: {access: read, from: r}")))
	bad("source naming a nonancestor writer", pipeline.CodeInvalidSource, doc(container("w", "workspace: {access: write, from: run}"), container("a", "workspace: {access: read, from: w}")))
	ok("source naming an ancestor writer", doc(container("w", "workspace: {access: write, from: run}"), container("a", "needs: [w]", "workspace: {access: read, from: w}")))
	ok("source naming a transitive ancestor writer", doc(container("w", "workspace: {access: write, from: run}"), container("m", "needs: [w]"), container("a", "needs: [m]", "workspace: {access: read, from: w}")))
	bad("writer sourcing from a nonancestor writer", pipeline.CodeInvalidSource, doc(container("w", "workspace: {access: write, from: run}"), container("x", "needs: [w]", "workspace: {access: write, from: x}")))

	// Structure.
	bad("missing steps", pipeline.CodeMissingField, "apiVersion: kodama/v1alpha1\nkind: Pipeline\nmetadata: {name: p}\nspec: {}\n")
	bad("empty steps", pipeline.CodeInvalidValue, "apiVersion: kodama/v1alpha1\nkind: Pipeline\nmetadata: {name: p}\nspec: {steps: []}\n")
	bad("other API version", pipeline.CodeInvalidValue, strings.Replace(doc(container("a")), "kodama/v1alpha1", "kodama/v1", 1))
	bad("other kind", pipeline.CodeInvalidValue, strings.Replace(doc(container("a")), "kind: Pipeline", "kind: Workflow", 1))
	bad("unknown kind of step", pipeline.CodeInvalidValue, doc("{id: a, kind: webhook}"))
	bad("duplicate need", pipeline.CodeInvalidValue, doc(container("a"), container("b", "needs: [a, a]")))

	t.Run("limits are configurable", func(t *testing.T) {
		small, err := pipeline.NewValidator(pipeline.Config{Capabilities: e.caps, Limits: pipeline.Limits{MaxSteps: 2}})
		if err != nil {
			t.Fatal(err)
		}
		_, err = small.ValidateYAML(t.Context(), scope, actor, []byte(doc(container("a"), container("b"), container("c"))))
		expectCode(t, err, pipeline.CodeLimitExceeded)
		if _, err := pipeline.NewValidator(pipeline.Config{Capabilities: e.caps, Limits: pipeline.Limits{MaxSteps: 1 << 20}}); err == nil {
			t.Fatal("an unbounded step limit must be refused")
		}
	})
	t.Run("a validator without capabilities cannot exist", func(t *testing.T) {
		if _, err := pipeline.NewValidator(pipeline.Config{}); err == nil {
			t.Fatal("capabilities are required")
		}
	})
	t.Run("diagnostics are bounded and never echo content", func(t *testing.T) {
		secret := "super-secret-token-value"
		var steps []string
		for i := 0; i < 60; i++ {
			steps = append(steps, "{id: s"+string(rune('a'+i%26))+string(rune('a'+i/26))+", kind: container, image: tag-only-"+secret+", command: [x], timeoutSeconds: 5}")
		}
		_, err := e.validate(doc(steps...))
		var ve *pipeline.ValidationError
		if !errors.As(err, &ve) || len(ve.Diagnostics) > pipeline.MaxDiagnostics || !ve.Truncated {
			t.Fatalf("expected bounded, truncated diagnostics: %v", err)
		}
		for _, d := range ve.Diagnostics {
			if strings.Contains(d.Message+d.Path, secret) {
				t.Fatalf("diagnostic echoes document content: %+v", d)
			}
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatal("error text echoes document content")
		}
	})
}
