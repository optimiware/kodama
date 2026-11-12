//go:build e2e

package harness

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/pipeline"
)

// TestHarnessPipelineDocumentBuildsExactYAML proves the builder renders the
// ordered actions and literal environment as stable, exact YAML bytes.
func TestHarnessPipelineDocumentBuildsExactYAML(t *testing.T) {
	document := Document{
		Env: map[string]string{"AC11_ENV": "ac11-value"},
		Actions: []Action{
			{Name: "first", Image: "docker.io/library/node:26.13.0-alpine", Commands: []string{"echo first"}, Timeout: "5m"},
			{Name: "second", Image: "docker.io/library/node:26.13.0-alpine", Commands: []string{"echo second"}, Outputs: []string{"artifact"}},
		},
	}
	first, err := document.Build()
	if err != nil {
		t.Fatalf("build pipeline: %v", err)
	}
	second, err := document.Build()
	if err != nil {
		t.Fatalf("rebuild pipeline: %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("two builds of the same document differ:\n%s\n%s", first, second)
	}
	text := string(first)
	for _, wanted := range []string{
		"actions:",
		"name: first",
		"name: second",
		"image: docker.io/library/node:26.13.0-alpine",
		"- echo first",
		"- echo second",
		"timeout: 5m",
		"outputs:",
		"- artifact",
		"AC11_ENV: ac11-value",
	} {
		if !strings.Contains(text, wanted) {
			t.Errorf("pipeline yaml does not contain %q:\n%s", wanted, text)
		}
	}
	if strings.Index(text, "name: first") > strings.Index(text, "name: second") {
		t.Fatalf("pipeline yaml reordered actions:\n%s", text)
	}
	if got := DocumentDigest(first); !strings.HasPrefix(got, "sha256:") || len(got) != len("sha256:")+64 {
		t.Fatalf("document digest %q is not a sha256 identity", got)
	}
	if DocumentDigest(first) == DocumentDigest(second) && string(first) != string(second) {
		t.Fatal("equal digests for different documents")
	}
}

// TestHarnessPipelineBuilderMatchesRealDecoder proves the builder accepts a
// document the real admission parser accepts, including an uppercase secret
// name and a subject reference to an earlier declared output.
func TestHarnessPipelineBuilderMatchesRealDecoder(t *testing.T) {
	document := Document{
		Env: map[string]string{"LOG_LEVEL": "info"},
		Actions: []Action{
			{
				Name:     "prepare",
				Image:    DefaultImage,
				Timeout:  "10m",
				Outputs:  []string{"artifact"},
				Secrets:  []string{"DEPLOY_TOKEN"},
				Commands: []string{"echo prepare"},
			},
			{
				Name:    "approve",
				Request: "role/reviewer",
				Timeout: "5m",
				Subject: map[string]any{
					"operation": "git.ref.update",
					"artifact":  map[string]any{"from": "prepare.outputs.artifact"},
				},
			},
		},
	}
	encoded, err := document.Build()
	if err != nil {
		t.Fatalf("builder refused a document the real parser accepts: %v", err)
	}
	decoded, err := pipeline.Decode(encoded, pipeline.DefaultLimits)
	if err != nil {
		t.Fatalf("real pipeline decoder refused the built document: %v", err)
	}
	if len(decoded.Actions) != 2 || decoded.Actions[0].Name != "prepare" || decoded.Actions[1].Name != "approve" {
		t.Fatalf("decoded actions = %+v; want prepare/approve", decoded.Actions)
	}
	if got := decoded.Actions[0].Container.Secrets; len(got) != 1 || got[0] != "DEPLOY_TOKEN" {
		t.Fatalf("decoded secrets = %v; want DEPLOY_TOKEN", got)
	}
	if decoded.Actions[1].Request == nil {
		t.Fatal("decoded approve action is not a request")
	}
	fields := decoded.Actions[1].Request.Subject.Fields()
	if fields["artifact"].Kind != definition.SubjectReference || fields["artifact"].Reference.Action != "prepare" || fields["artifact"].Reference.Key != "artifact" {
		t.Fatalf("decoded subject reference = %+v; want prepare.outputs.artifact", fields["artifact"])
	}
}

// TestHarnessPipelineBuilderMatchesRealDecoderRefusals proves the builder
// refuses the same secret-name and subject-reference shapes the real parser
// refuses, instead of passing by accident.
func TestHarnessPipelineBuilderMatchesRealDecoderRefusals(t *testing.T) {
	realRefusals := []struct {
		name     string
		yaml     string
		document Document
	}{
		{
			name: "lowercase secret name",
			yaml: `actions:
  - name: build
    image: docker.io/library/node:26.13.0-alpine
    secrets: [deploy_token]
    commands: [true]
`,
			document: Document{Actions: []Action{{
				Name: "build", Image: DefaultImage, Secrets: []string{"deploy_token"}, Commands: []string{"true"},
			}}},
		},
		{
			name: "reserved secret name",
			yaml: `actions:
  - name: build
    image: docker.io/library/node:26.13.0-alpine
    secrets: [KODAMA_TOKEN]
    commands: [true]
`,
			document: Document{Actions: []Action{{
				Name: "build", Image: DefaultImage, Secrets: []string{"KODAMA_TOKEN"}, Commands: []string{"true"},
			}}},
		},
		{
			name: "undeclared output reference",
			yaml: `actions:
  - name: prepare
    image: docker.io/library/node:26.13.0-alpine
    commands: [true]
  - name: approve
    request: role/reviewer
    subject:
      artifact: {from: prepare.outputs.missing}
`,
			document: Document{Actions: []Action{
				{Name: "prepare", Image: DefaultImage, Commands: []string{"true"}},
				{Name: "approve", Request: "role/reviewer", Subject: map[string]any{
					"artifact": map[string]any{"from": "prepare.outputs.missing"},
				}},
			}},
		},
	}
	for _, testCase := range realRefusals {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := pipeline.Decode([]byte(testCase.yaml), pipeline.DefaultLimits); err == nil {
				t.Fatalf("real decoder accepted the %s regression fixture", testCase.name)
			}
			if _, err := testCase.document.Build(); err == nil {
				t.Fatalf("builder accepted the %s shape the real decoder refuses", testCase.name)
			}
		})
	}
}

// TestHarnessPipelineDocumentRefusals proves malformed builder input fails
// closed instead of reaching the installation.
func TestHarnessPipelineDocumentRefusals(t *testing.T) {
	image := "docker.io/library/node:26.13.0-alpine"
	cases := []struct {
		name     string
		document Document
	}{
		{name: "no actions"},
		{
			name:     "duplicate action",
			document: Document{Actions: []Action{{Name: "build", Image: image}, {Name: "build", Image: image}}},
		},
		{
			name:     "invalid action name",
			document: Document{Actions: []Action{{Name: "Build", Image: image}}},
		},
		{
			name:     "mixed kinds",
			document: Document{Actions: []Action{{Name: "mixed", Image: image, Request: "role/reviewer", Subject: map[string]any{"question": "q"}}}},
		},
		{
			name:     "no kind",
			document: Document{Actions: []Action{{Name: "empty"}}},
		},
		{
			name:     "request with container secret",
			document: Document{Actions: []Action{{Name: "approve", Request: "role/reviewer", Subject: map[string]any{"question": "q"}, Secrets: []string{"credential"}}}},
		},
		{
			name:     "forward subject reference",
			document: Document{Actions: []Action{{Name: "approve", Request: "role/reviewer", Subject: map[string]any{"artifact": map[string]any{"from": "later.outputs.artifact"}}}}},
		},
		{
			name:     "reserved environment",
			document: Document{Env: map[string]string{"KODAMA_URL": "x"}, Actions: []Action{{Name: "build", Image: image}}},
		},
		{
			name:     "bad timeout",
			document: Document{Actions: []Action{{Name: "build", Image: image, Timeout: "30s"}}},
		},
		{
			name:     "reserved secret name",
			document: Document{Actions: []Action{{Name: "build", Image: image, Secrets: []string{"KODAMA_TOKEN"}}}},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := testCase.document.Build(); err == nil {
				t.Fatal("malformed pipeline document was accepted")
			}
		})
	}
}

// TestHarnessOutputSubmissionScript proves the generated workload command is a
// Python standard-library HTTPS client: it reads the declared CA file, re-reads
// the mounted token for every call, uses the current action id and a distinct
// per-action idempotency key, bounds responses, refuses redirects and embeds no
// credential value.
func TestHarnessOutputSubmissionScript(t *testing.T) {
	caPath := "/kodama/secrets/W49_CA_TEST"
	script, err := OutputSubmissionScript(map[string]string{"artifact": "ac12-ok"}, caPath)
	if err != nil {
		t.Fatalf("build submission script: %v", err)
	}
	for _, wanted := range []string{
		"python3",
		"import ssl",
		"import urllib.request",
		`"/kodama/secrets/KODAMA_TOKEN"`,
		caPath,
		`"KODAMA_EXECUTION"`,
		`"KODAMA_ACTION"`,
		`"artifact": "ac12-ok"`,
		"action_id",
		"idempotency_key",
		"version_conflict",
		"redirect",
		"ssl.CERT_REQUIRED",
		"check_hostname = True",
		`startswith("https://")`,
	} {
		if !strings.Contains(script, wanted) {
			t.Errorf("submission script does not contain %q", wanted)
		}
	}
	for _, unwanted := range []string{"require(\"https\")", "node ", "process.env", "CERT_NONE", "_create_unverified_context"} {
		if strings.Contains(script, unwanted) {
			t.Errorf("submission script contains the forbidden %q", unwanted)
		}
	}
	if strings.Contains(script, "project-token-value") {
		t.Fatal("submission script embedded a credential")
	}
	// The embedded program must compile as Python, not merely look plausible.
	if python, err := exec.LookPath("python3"); err == nil {
		program := submissionProgram(t, script)
		path := filepath.Join(t.TempDir(), "submit.py")
		if err := os.WriteFile(path, []byte(program), 0o600); err != nil {
			t.Fatalf("write submission program: %v", err)
		}
		command := exec.Command(python, "-c", "import py_compile,sys; py_compile.compile(sys.argv[1], doraise=True)", path)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("submission program does not compile: %v\n%s", err, output)
		}
	}
	for _, rejected := range []map[string]string{
		{},
		{"Bad-Key": "value"},
		{"artifact": "line\nbreak"},
	} {
		if _, err := OutputSubmissionScript(rejected, caPath); err == nil {
			t.Fatalf("submission outputs %v were accepted", rejected)
		}
	}
}

// submissionProgram extracts the heredoc body from the generated command.
func submissionProgram(t *testing.T, command string) string {
	t.Helper()
	const marker = "KODAMA_E2E_SUBMIT"
	_, after, ok := strings.Cut(command, "<<'"+marker+"'\n")
	if !ok {
		t.Fatalf("submission command has no heredoc: %s", command)
	}
	body := after
	before0, _, ok0 := strings.Cut(body, "\n"+marker+"\n")
	if !ok0 {
		t.Fatalf("submission heredoc is not terminated")
	}
	return before0 + "\n"
}

// TestHarnessPipelinePushFields proves the builder carries the push trigger's
// repository, service and on.push declarations through the real decoder.
func TestHarnessPipelinePushFields(t *testing.T) {
	document := Document{
		Repository: "smoke-run-1",
		Service:    "engineer-service",
		On:         &On{Push: []string{"refs/heads/work/run-1", "refs/heads/main"}},
		Inputs:     []string{"repository", "ref", "commit", "previous_commit"},
		Actions: []Action{{
			Name:     "push",
			Image:    DefaultImage,
			Commands: []string{"echo pushed"},
		}},
	}
	encoded, err := document.Build()
	if err != nil {
		t.Fatalf("builder refused push fields: %v", err)
	}
	for _, wanted := range []string{
		"repository: smoke-run-1",
		"service: engineer-service",
		"push:",
		"- refs/heads/work/run-1",
		"- refs/heads/main",
	} {
		if !strings.Contains(string(encoded), wanted) {
			t.Errorf("push pipeline yaml does not contain %q:\n%s", wanted, encoded)
		}
	}
	decoded, err := pipeline.Decode(encoded, pipeline.DefaultLimits)
	if err != nil {
		t.Fatalf("real decoder refused the built push pipeline: %v", err)
	}
	if len(decoded.Inputs) != 4 || decoded.Inputs[0] != "repository" {
		t.Fatalf("decoded push inputs = %v", decoded.Inputs)
	}
}
