package pipeline_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/pipeline"
)

func TestDecodeAcceptsSupportedPipelineForms(t *testing.T) {
	decoded, err := pipeline.Decode(validPipelineYAML(), pipeline.DefaultLimits)
	requireNoError(t, err, "Decode valid pipeline")

	wantInputs := []id.InputKey{"repository", "retries"}
	if len(decoded.Inputs) != len(wantInputs) {
		t.Fatalf("inputs = %v; want %v", decoded.Inputs, wantInputs)
	}
	for index := range wantInputs {
		if decoded.Inputs[index] != wantInputs[index] {
			t.Fatalf("inputs = %v; want %v", decoded.Inputs, wantInputs)
		}
	}
	if len(decoded.Env) != 1 || decoded.Env[0].Name != "LOG_LEVEL" || decoded.Env[0].Value != "info" {
		t.Fatalf("pipeline env = %+v; want LOG_LEVEL=info", decoded.Env)
	}
	if len(decoded.Actions) != 2 {
		t.Fatalf("actions = %d; want 2", len(decoded.Actions))
	}

	build := decoded.Actions[0]
	if build.Name != "build" || build.Timeout.String() != "10m0s" {
		t.Fatalf("build action = %+v; want name build timeout 10m", build)
	}
	if build.Container == nil || build.Request != nil {
		t.Fatalf("build action kind = %+v; want exactly one container", build)
	}
	if build.Container.Image != "registry.example.com/build:latest" {
		t.Fatalf("build image = %q", build.Container.Image)
	}
	if len(build.Container.Commands) != 2 || build.Container.Commands[0] != "make" {
		t.Fatalf("build commands = %v", build.Container.Commands)
	}
	if !strings.Contains(build.Container.Commands[1], "echo built") {
		t.Fatalf("block scalar command = %q; want echo built", build.Container.Commands[1])
	}
	if len(build.Container.Env) != 1 || build.Container.Env[0].Name != "APP_ENV" || build.Container.Env[0].Value != "test" {
		t.Fatalf("build env = %+v", build.Container.Env)
	}
	if len(build.Container.Secrets) != 1 || build.Container.Secrets[0] != "DEPLOY_TOKEN" {
		t.Fatalf("build secrets = %v", build.Container.Secrets)
	}
	if len(build.Container.Outputs) != 1 || build.Container.Outputs[0] != "candidate" {
		t.Fatalf("build outputs = %v", build.Container.Outputs)
	}

	review := decoded.Actions[1]
	if review.Name != "review" || review.Timeout.String() != "72h0m0s" {
		t.Fatalf("review action = %+v; want name review timeout 72h", review)
	}
	if review.Request == nil || review.Container != nil {
		t.Fatalf("review action kind = %+v; want exactly one request", review)
	}
	if review.Request.Assignment != "role/reviewer" {
		t.Fatalf("review assignment = %q", review.Request.Assignment)
	}
	if len(review.Request.Outputs) != 1 || review.Request.Outputs[0] != "approved" {
		t.Fatalf("review outputs = %v", review.Request.Outputs)
	}
	fields := review.Request.Subject.Fields()
	if len(fields) != 3 {
		t.Fatalf("subject fields = %v; want 3", fields)
	}
	if fields["operation"].Kind != definition.SubjectLiteral || fields["operation"].Literal != "git.ref.update" {
		t.Fatalf("operation subject = %+v", fields["operation"])
	}
	if fields["resource"].Kind != definition.SubjectReference || !fields["resource"].Reference.Input || fields["resource"].Reference.Key != "repository" {
		t.Fatalf("resource subject = %+v", fields["resource"])
	}
	if fields["candidate"].Kind != definition.SubjectReference || fields["candidate"].Reference.Action != "build" || fields["candidate"].Reference.Key != "candidate" {
		t.Fatalf("candidate subject = %+v", fields["candidate"])
	}
}

func TestDecodeAcceptsPushDefinition(t *testing.T) {
	decoded, err := pipeline.Decode(pushPipelineYAML(), pipeline.DefaultLimits)
	requireNoError(t, err, "Decode push pipeline")
	wantInputs := []id.InputKey{"repository", "ref", "commit", "previous_commit"}
	if len(decoded.Inputs) != len(wantInputs) {
		t.Fatalf("inputs = %v; want %v", decoded.Inputs, wantInputs)
	}
	for index := range wantInputs {
		if decoded.Inputs[index] != wantInputs[index] {
			t.Fatalf("inputs = %v; want %v", decoded.Inputs, wantInputs)
		}
	}
	if len(decoded.Actions) != 1 || decoded.Actions[0].Name != "test" {
		t.Fatalf("actions = %+v; want one test action", decoded.Actions)
	}

	flow := []byte(`repository: repo-1
service: git-service
on: {push: [refs/heads/main]}
inputs: [repository, ref, commit, previous_commit]
actions:
  - name: test
    image: registry.example.com/test:latest
    commands: [test-commit]
`)
	if _, err := pipeline.Decode(flow, pipeline.DefaultLimits); err != nil {
		t.Fatalf("Decode flow-style push definition: %v", err)
	}
}

func TestDecodeAcceptsTypedInputSpecifications(t *testing.T) {
	decoded, err := pipeline.Decode(typedInputsYAML(), pipeline.DefaultLimits)
	requireNoError(t, err, "Decode typed inputs")
	want := []id.InputKey{"repository", "retries", "dry_run"}
	if len(decoded.Inputs) != len(want) {
		t.Fatalf("inputs = %v; want %v", decoded.Inputs, want)
	}
	for index := range want {
		if decoded.Inputs[index] != want[index] {
			t.Fatalf("inputs = %v; want %v", decoded.Inputs, want)
		}
	}
}

func TestDecodeRefusesDuplicateKeys(t *testing.T) {
	cases := map[string]string{
		"duplicate top-level": `inputs: [repository]
actions:
  - name: build
    image: registry.example.com/build:latest
actions:
  - name: test
    image: registry.example.com/test:latest
`,
		"duplicate pipeline env": `env:
  A: one
  A: two
actions:
  - name: build
    image: registry.example.com/build:latest
`,
		"duplicate action field": `actions:
  - name: build
    name: other
    image: registry.example.com/build:latest
`,
		"duplicate action env": `actions:
  - name: build
    image: registry.example.com/build:latest
    env:
      A: one
      A: two
`,
		"duplicate input key": `inputs:
  - repository
  - repository
actions:
  - name: build
    image: registry.example.com/build:latest
`,
		"duplicate input spec field": `inputs:
  - key: repository
    key: other
actions:
  - name: build
    image: registry.example.com/build:latest
`,
		"duplicate subject field": `inputs: [repository]
actions:
  - name: review
    request: role/reviewer
    subject:
      resource:
        from: inputs.repository
      resource:
        from: inputs.repository
`,
		"duplicate output": `actions:
  - name: build
    image: registry.example.com/build:latest
    outputs: [candidate, candidate]
`,
		"duplicate secret": `actions:
  - name: build
    image: registry.example.com/build:latest
    secrets: [DEPLOY_TOKEN, DEPLOY_TOKEN]
`,
	}
	for name, document := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := pipeline.Decode([]byte(document), pipeline.DefaultLimits)
			requireFaultClass(t, err, fault.InvalidInput)
		})
	}
}

func TestDecodeRefusesUnknownKeys(t *testing.T) {
	cases := map[string]string{
		"unknown top-level": `unknown: value
actions:
  - name: build
    image: registry.example.com/build:latest
`,
		"unsupported condition": `actions:
  - name: build
    image: registry.example.com/build:latest
    condition: always
`,
		"unsupported parallel": `actions:
  - name: build
    image: registry.example.com/build:latest
    parallel: true
`,
		"unsupported needs": `actions:
  - name: build
    image: registry.example.com/build:latest
    needs: [prepare]
`,
		"unknown container field": `actions:
  - name: build
    image: registry.example.com/build:latest
    workdir: /tmp
`,
		"unknown request field": `actions:
  - name: review
    request: role/reviewer
    response: {}
    subject: {}
`,
		"unknown input spec field": `inputs:
  - key: repository
    type: string
actions:
  - name: build
    image: registry.example.com/build:latest
`,
		"unknown on field": `repository: repo-1
service: git-service
on:
  pull: [refs/heads/main]
actions:
  - name: build
    image: registry.example.com/build:latest
`,
		"unknown subject value field": `inputs: [repository]
actions:
  - name: review
    request: role/reviewer
    subject:
      resource:
        from: inputs.repository
        literal: repo-1
`,
	}
	for name, document := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := pipeline.Decode([]byte(document), pipeline.DefaultLimits)
			requireFaultClass(t, err, fault.InvalidInput)
		})
	}
}

func TestDecodeRefusesMixedActionKinds(t *testing.T) {
	cases := map[string]string{
		"both kinds": `actions:
  - name: build
    image: registry.example.com/build:latest
    request: role/reviewer
    subject: {}
`,
		"no kind": `actions:
  - name: build
    commands: [make]
`,
	}
	for name, document := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := pipeline.Decode([]byte(document), pipeline.DefaultLimits)
			requireFaultClass(t, err, fault.InvalidInput)
		})
	}
}

func TestDecodeRefusesUnsupportedYAMLSyntax(t *testing.T) {
	cases := map[string]string{
		"tag": `actions:
  - name: build
    image: !!str registry.example.com/build:latest
`,
		"anchor": `env: &defaults
  A: one
actions:
  - name: build
    image: registry.example.com/build:latest
`,
		"alias": `inputs: &names [repository]
actions:
  - name: build
    image: registry.example.com/build:latest
    commands: *names
`,
		"merge key": `actions:
  - name: build
    image: registry.example.com/build:latest
    env: &defaults
      A: one
  - name: test
    image: registry.example.com/test:latest
    env:
      <<: *defaults
`,
		"malformed": `actions: [
`,
	}
	for name, document := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := pipeline.Decode([]byte(document), pipeline.DefaultLimits)
			requireFaultClass(t, err, fault.InvalidInput)
		})
	}
}

func TestDecodeRefusesTrailingDocuments(t *testing.T) {
	base := string(validPipelineYAML())
	cases := map[string]string{
		"second mapping":   base + "---\nfoo: bar\n",
		"second empty":     base + "---\n",
		"second malformed": base + "---\nfoo: [\n",
		"end then second":  base + "...\n---\nfoo: bar\n",
		"trailing anchor":  base + "---\n&anchor foo\n",
		"trailing alias":   base + "---\n*missing\n",
	}
	for name, document := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := pipeline.Decode([]byte(document), pipeline.DefaultLimits)
			requireFaultClass(t, err, fault.InvalidInput)
		})
	}

	// Empty input is still refused; a decoder returns io.EOF rather than a node.
	_, err := pipeline.Decode(nil, pipeline.DefaultLimits)
	requireFaultClass(t, err, fault.InvalidInput)
	_, err = pipeline.Decode([]byte("   \n# comment\n"), pipeline.DefaultLimits)
	requireFaultClass(t, err, fault.InvalidInput)
}

func TestDecodeAcceptsSingleDocumentMarkers(t *testing.T) {
	base := string(validPipelineYAML())
	cases := map[string]string{
		"leading":  "---\n" + base,
		"trailing": base + "...\n",
		"both":     "---\n" + base + "...\n",
	}
	for name, document := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := pipeline.Decode([]byte(document), pipeline.DefaultLimits); err != nil {
				t.Fatalf("Decode %s document marker: %v", name, err)
			}
		})
	}
}

func TestDecodeRefusesMalformedAndUnavailableReferences(t *testing.T) {
	cases := map[string]string{
		"unknown input": `inputs: [repository]
actions:
  - name: review
    request: role/reviewer
    subject:
      resource:
        from: inputs.missing
`,
		"self reference": `actions:
  - name: review
    request: role/reviewer
    subject:
      candidate:
        from: review.outputs.approved
    outputs: [approved]
`,
		"forward reference": `actions:
  - name: first
    request: role/reviewer
    subject:
      candidate:
        from: second.outputs.approved
  - name: second
    request: role/reviewer
    subject: {}
    outputs: [approved]
`,
		"unknown action": `actions:
  - name: review
    request: role/reviewer
    subject:
      candidate:
        from: missing.outputs.approved
`,
		"undeclared output": `actions:
  - name: build
    image: registry.example.com/build:latest
    outputs: [candidate]
  - name: review
    request: role/reviewer
    subject:
      candidate:
        from: build.outputs.approved
`,
		"missing outputs segment": `actions:
  - name: build
    image: registry.example.com/build:latest
    outputs: [candidate]
  - name: review
    request: role/reviewer
    subject:
      candidate:
        from: build.output.candidate
`,
		"empty reference": `inputs: [repository]
actions:
  - name: review
    request: role/reviewer
    subject:
      resource:
        from: inputs.
`,
		"extra path segment": `inputs: [repository]
actions:
  - name: review
    request: role/reviewer
    subject:
      resource:
        from: inputs.repository.extra
`,
		"literal and reference": `inputs: [repository]
actions:
  - name: review
    request: role/reviewer
    subject:
      resource:
        from: inputs.repository
        value: repo-1
`,
	}
	for name, document := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := pipeline.Decode([]byte(document), pipeline.DefaultLimits)
			requireFaultClass(t, err, fault.InvalidInput)
		})
	}
}

func TestDecodeRefusesUnsafeNames(t *testing.T) {
	cases := map[string]string{
		"action traversal": `actions:
  - name: ../build
    image: registry.example.com/build:latest
`,
		"action uppercase": `actions:
  - name: Build
    image: registry.example.com/build:latest
`,
		"action underscore": `actions:
  - name: build_test
    image: registry.example.com/build:latest
`,
		"input traversal": `inputs: [../repository]
actions:
  - name: build
    image: registry.example.com/build:latest
`,
		"input uppercase": `inputs: [Repository]
actions:
  - name: build
    image: registry.example.com/build:latest
`,
		"output traversal": `actions:
  - name: build
    image: registry.example.com/build:latest
    outputs: [../candidate]
`,
		"env lowercase": `env:
  log_level: info
actions:
  - name: build
    image: registry.example.com/build:latest
`,
		"reserved env": `env:
  KODAMA_URL: https://example.com
actions:
  - name: build
    image: registry.example.com/build:latest
`,
		"secret lowercase": `actions:
  - name: build
    image: registry.example.com/build:latest
    secrets: [deploy_token]
`,
		"reserved secret": `actions:
  - name: build
    image: registry.example.com/build:latest
    secrets: [KODAMA_TOKEN]
`,
		"subject field traversal": `inputs: [repository]
actions:
  - name: review
    request: role/reviewer
    subject:
      ../resource:
        from: inputs.repository
`,
		"duplicate action name": `actions:
  - name: build
    image: registry.example.com/build:latest
  - name: build
    image: registry.example.com/test:latest
`,
	}
	for name, document := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := pipeline.Decode([]byte(document), pipeline.DefaultLimits)
			requireFaultClass(t, err, fault.InvalidInput)
		})
	}
}

func TestDecodeTimeoutBounds(t *testing.T) {
	document := func(timeout string) []byte {
		return []byte(`actions:
  - name: build
    image: registry.example.com/build:latest
    commands: [make]
    timeout: ` + timeout + `
`)
	}
	for _, accepted := range []string{"1m", "720h", "30m", "1h30m"} {
		if _, err := pipeline.Decode(document(accepted), pipeline.DefaultLimits); err != nil {
			t.Errorf("Decode timeout %q: %v", accepted, err)
		}
	}
	for _, refused := range []string{"59.999999999s", "720h0.000000001s", "0s", "-1m", "1"} {
		if _, err := pipeline.Decode(document(refused), pipeline.DefaultLimits); err == nil {
			t.Errorf("Decode timeout %q accepted; want refusal", refused)
		} else {
			requireFaultClass(t, err, fault.InvalidInput)
		}
	}
	if _, err := pipeline.Decode([]byte(`actions:
  - name: build
    image: registry.example.com/build:latest
    commands: [make]
`), pipeline.DefaultLimits); err != nil {
		t.Fatalf("Decode absent timeout: %v", err)
	}
}

func TestDecodeYAMLByteBoundary(t *testing.T) {
	base := validPipelineYAML()
	if len(base) >= pipeline.DefaultLimits.MaxYAMLBytes {
		t.Fatalf("fixture is already %d bytes", len(base))
	}
	padded := func(size int) []byte {
		padding := size - len(base) - 2
		if padding < 0 {
			t.Fatalf("cannot pad %d bytes to %d", len(base), size)
		}
		out := append([]byte(nil), base...)
		out = append(out, '#')
		out = append(out, bytes.Repeat([]byte("x"), padding)...)
		out = append(out, '\n')
		return out
	}
	exact := padded(pipeline.DefaultLimits.MaxYAMLBytes)
	if len(exact) != pipeline.DefaultLimits.MaxYAMLBytes {
		t.Fatalf("padded document = %d bytes; want %d", len(exact), pipeline.DefaultLimits.MaxYAMLBytes)
	}
	if _, err := pipeline.Decode(exact, pipeline.DefaultLimits); err != nil {
		t.Fatalf("Decode exactly %d bytes: %v", len(exact), err)
	}
	over := padded(pipeline.DefaultLimits.MaxYAMLBytes + 1)
	if len(over) != pipeline.DefaultLimits.MaxYAMLBytes+1 {
		t.Fatalf("padded document = %d bytes; want %d", len(over), pipeline.DefaultLimits.MaxYAMLBytes+1)
	}
	_, err := pipeline.Decode(over, pipeline.DefaultLimits)
	requireFaultClass(t, err, fault.InvalidInput)
}

func TestDecodeDepthBoundary(t *testing.T) {
	if err := pipeline.DefaultLimits.CheckDepth(64); err != nil {
		t.Fatalf("depth 64 refused: %v", err)
	}
	if err := pipeline.DefaultLimits.CheckDepth(65); err == nil {
		t.Fatal("depth 65 accepted")
	}

	// validPipelineYAML reaches depth 5: root mapping, actions sequence, action
	// mapping, subject mapping and subject value mapping.
	exact := pipeline.DefaultLimits
	exact.MaxDepth = 5
	if _, err := pipeline.Decode(validPipelineYAML(), exact); err != nil {
		t.Fatalf("Decode at depth 5: %v", err)
	}
	over := exact
	over.MaxDepth = 4
	_, err := pipeline.Decode(validPipelineYAML(), over)
	requireFaultClass(t, err, fault.InvalidInput)
}

func TestDecodeRefusesAliasExpansion(t *testing.T) {
	limits := pipeline.DefaultLimits
	if limits.MaxAliases != 0 {
		t.Fatalf("DefaultLimits.MaxAliases = %d; want 0", limits.MaxAliases)
	}
	document := []byte(`inputs: &names [repository]
actions:
  - name: build
    image: registry.example.com/build:latest
    commands: *names
`)
	_, err := pipeline.Decode(document, limits)
	requireFaultClass(t, err, fault.InvalidInput)
}
