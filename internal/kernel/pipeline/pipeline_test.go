package pipeline_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/pipeline"
)

func validDocument() pipeline.Document {
	return pipeline.Document{
		Name: "implement",
		Triggers: pipeline.Triggers{Git: pipeline.GitTrigger{
			Branches: []string{"main"},
			Events:   []string{"push", "merge"},
		}},
		Services: map[string]pipeline.Service{
			"db": {
				Image:   "postgres:16-alpine",
				Env:     map[string]string{"POSTGRES_USER": "kodama"},
				Secrets: []string{"TEST_DB_PASSWORD"},
				Ports:   []int{5432},
			},
		},
		Steps: []pipeline.Step{
			{
				Name:     "code",
				Image:    "ghcr.io/sst/opencode:latest",
				Commands: []string{"echo code"},
				Env:      map[string]string{"KODAMA_STEP": "code"},
				Secrets:  []string{"ANTHROPIC_API_KEY"},
			},
			{
				Name:      "test",
				Image:     "golang:1.27-alpine",
				DependsOn: []string{"code"},
				Commands:  []string{"go test ./..."},
				Timeout:   "15m",
			},
		},
	}
}

func requireDiagnostic(t *testing.T, err error, code string) *pipeline.Diagnostic {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	var diagnostic *pipeline.Diagnostic
	if !errors.As(err, &diagnostic) {
		t.Fatalf("error is %T, want *pipeline.Diagnostic: %v", err, err)
	}
	if diagnostic.Code != code {
		t.Fatalf("code = %q, want %q (%v)", diagnostic.Code, code, err)
	}
	if diagnostic.Path == "" || diagnostic.Message == "" {
		t.Fatalf("diagnostic is incomplete: %+v", diagnostic)
	}
	if !errors.Is(err, domain.ErrInvalidValue) {
		t.Fatalf("error does not wrap domain.ErrInvalidValue: %v", err)
	}
	return diagnostic
}

func TestDiagnosticError(t *testing.T) {
	diagnostic := &pipeline.Diagnostic{Code: pipeline.CodeInvalidName, Path: "name", Message: "bad name"}
	if got, want := diagnostic.Error(), "invalid_name at name: bad name"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

const validYAML = `
name: implement
triggers:
  git:
    branches: [main]
services:
  db:
    image: postgres:16-alpine
    env:
      POSTGRES_USER: kodama
      POSTGRES_DB: app
    secrets: [TEST_DB_PASSWORD]
    ports: [5432]
steps:
  - name: code
    image: ghcr.io/sst/opencode:latest
    secrets: [ANTHROPIC_API_KEY]
    commands:
      - echo code

  - name: test
    depends_on: [code]
    image: golang:1.27-alpine
    env:
      DATABASE_URL: "postgres://kodama@db:5432/app?sslmode=disable"
    timeout: 15m
    commands:
      - go test -v ./...
`

func TestParseValid(t *testing.T) {
	doc, err := pipeline.Parse([]byte(validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if doc.Name != "implement" {
		t.Fatalf("name = %q", doc.Name)
	}
	if !slices.Equal(doc.Triggers.Git.Branches, []string{"main"}) {
		t.Fatalf("branches = %v", doc.Triggers.Git.Branches)
	}
	if len(doc.Services) != 1 || doc.Services["db"].Scope != "" {
		t.Fatalf("services = %+v, want one service with an empty scope", doc.Services)
	}
	if got := doc.Services["db"].Ports; !slices.Equal(got, []int{5432}) {
		t.Fatalf("ports = %v", got)
	}
	if len(doc.Steps) != 2 || doc.Steps[1].Name != "test" {
		t.Fatalf("steps = %+v", doc.Steps)
	}
	if !slices.Equal(doc.Steps[1].DependsOn, []string{"code"}) {
		t.Fatalf("depends_on = %v", doc.Steps[1].DependsOn)
	}
	if doc.Steps[1].Timeout != "15m" {
		t.Fatalf("timeout = %q", doc.Steps[1].Timeout)
	}
	if err := doc.Validate(); err != nil {
		t.Fatalf("valid document rejected: %v", err)
	}
	canonical, err := doc.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	var decoded pipeline.Document
	if err := json.Unmarshal(canonical, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Services["db"].Scope != "pipeline" {
		t.Fatalf("canonical scope = %q, want pipeline", decoded.Services["db"].Scope)
	}
}

func TestParseRejects(t *testing.T) {
	tests := []struct {
		name string
		data string
		code string
	}{
		{
			name: "unknown top-level field",
			data: "name: build\nbogus: true\nsteps:\n  - name: one\n    image: alpine\n    commands: [true]\n",
			code: pipeline.CodeUnknownField,
		},
		{
			name: "unknown step field",
			data: "name: build\nsteps:\n  - name: one\n    image: alpine\n    commands: [true]\n    script: echo\n",
			code: pipeline.CodeUnknownField,
		},
		{
			name: "unknown service field",
			data: "name: build\nservices:\n  db:\n    image: postgres\n    volume: data\nsteps:\n  - name: one\n    image: alpine\n    commands: [true]\n",
			code: pipeline.CodeUnknownField,
		},
		{
			name: "malformed yaml",
			data: "steps: [\n",
			code: pipeline.CodeInvalidYAML,
		},
		{
			name: "empty document",
			data: "",
			code: pipeline.CodeInvalidYAML,
		},
		{
			name: "multiple documents",
			data: "name: one\nsteps: []\n---\nname: two\nsteps: []\n",
			code: pipeline.CodeInvalidYAML,
		},
		{
			name: "wrong shape",
			data: "name: [build]\nsteps: []\n",
			code: pipeline.CodeInvalidYAML,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := pipeline.Parse([]byte(test.data)); err == nil {
				t.Fatal("document accepted")
			} else {
				requireDiagnostic(t, err, test.code)
			}
		})
	}
}

func TestParseAcceptsDependsOn(t *testing.T) {
	doc, err := pipeline.Parse([]byte(validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Steps) != 2 || len(doc.Steps[1].DependsOn) != 1 || doc.Steps[1].DependsOn[0] != "code" {
		t.Fatalf("depends_on was not decoded: %+v", doc.Steps)
	}
}

func TestValidate(t *testing.T) {
	manySteps := func(n int) []pipeline.Step {
		steps := make([]pipeline.Step, n)
		for i := range steps {
			steps[i] = pipeline.Step{Name: fmt.Sprintf("s%d", i), Image: "alpine", Commands: []string{"true"}}
		}
		return steps
	}
	manyServices := func(n int) map[string]pipeline.Service {
		services := make(map[string]pipeline.Service, n)
		for i := 0; i < n; i++ {
			services[fmt.Sprintf("s%d", i)] = pipeline.Service{Image: "alpine"}
		}
		return services
	}

	tests := []struct {
		name string
		edit func(*pipeline.Document)
		code string
	}{
		{"valid", func(*pipeline.Document) {}, ""},
		{
			"duplicate step",
			func(d *pipeline.Document) { d.Steps[1].Name = "code" },
			pipeline.CodeDuplicateStep,
		},
		{
			"unknown dependency",
			func(d *pipeline.Document) { d.Steps[1].DependsOn = []string{"missing"} },
			pipeline.CodeUnknownDependency,
		},
		{
			"self dependency",
			func(d *pipeline.Document) { d.Steps[0].DependsOn = []string{"code"} },
			pipeline.CodeInvalidValue,
		},
		{
			"duplicate dependency",
			func(d *pipeline.Document) { d.Steps[1].DependsOn = []string{"code", "code"} },
			pipeline.CodeInvalidValue,
		},
		{
			"dependency cycle",
			func(d *pipeline.Document) { d.Steps[0].DependsOn = []string{"test"} },
			pipeline.CodeDependencyCycle,
		},
		{
			"bad document name",
			func(d *pipeline.Document) { d.Name = "Implement" },
			pipeline.CodeInvalidName,
		},
		{
			"bad step name",
			func(d *pipeline.Document) { d.Steps[0].Name = "1code" },
			pipeline.CodeInvalidName,
		},
		{
			"bad service name",
			func(d *pipeline.Document) { d.Services["DB"] = pipeline.Service{Image: "postgres"} },
			pipeline.CodeInvalidName,
		},
		{
			"bad scope",
			func(d *pipeline.Document) {
				service := d.Services["db"]
				service.Scope = "global"
				d.Services["db"] = service
			},
			pipeline.CodeInvalidScope,
		},
		{
			"port zero",
			func(d *pipeline.Document) {
				service := d.Services["db"]
				service.Ports = []int{0}
				d.Services["db"] = service
			},
			pipeline.CodeInvalidValue,
		},
		{
			"port too large",
			func(d *pipeline.Document) {
				service := d.Services["db"]
				service.Ports = []int{65536}
				d.Services["db"] = service
			},
			pipeline.CodeInvalidValue,
		},
		{
			"duplicate port",
			func(d *pipeline.Document) {
				service := d.Services["db"]
				service.Ports = []int{5432, 5432}
				d.Services["db"] = service
			},
			pipeline.CodeInvalidValue,
		},
		{
			"too many ports",
			func(d *pipeline.Document) {
				service := d.Services["db"]
				service.Ports = []int{1, 2, 3, 4, 5, 6, 7, 8, 9}
				d.Services["db"] = service
			},
			pipeline.CodeInvalidValue,
		},
		{
			"bad step env key",
			func(d *pipeline.Document) { d.Steps[0].Env = map[string]string{"1BAD": ""} },
			pipeline.CodeInvalidEnv,
		},
		{
			"bad service env key",
			func(d *pipeline.Document) {
				service := d.Services["db"]
				service.Env = map[string]string{"BAD KEY": "x"}
				d.Services["db"] = service
			},
			pipeline.CodeInvalidEnv,
		},
		{
			"bad secret name",
			func(d *pipeline.Document) { d.Steps[0].Secrets = []string{"bad secret"} },
			pipeline.CodeInvalidSecret,
		},
		{
			"duplicate secret",
			func(d *pipeline.Document) { d.Steps[0].Secrets = []string{"TOKEN", "TOKEN"} },
			pipeline.CodeInvalidSecret,
		},
		{
			"bad service secret",
			func(d *pipeline.Document) {
				service := d.Services["db"]
				service.Secrets = []string{"1TOKEN"}
				d.Services["db"] = service
			},
			pipeline.CodeInvalidSecret,
		},
		{
			"timeout without unit",
			func(d *pipeline.Document) { d.Steps[1].Timeout = "15" },
			pipeline.CodeInvalidValue,
		},
		{
			"zero timeout",
			func(d *pipeline.Document) { d.Steps[1].Timeout = "0s" },
			pipeline.CodeInvalidValue,
		},
		{
			"negative timeout",
			func(d *pipeline.Document) { d.Steps[1].Timeout = "-1m" },
			pipeline.CodeInvalidValue,
		},
		{
			"empty step image",
			func(d *pipeline.Document) { d.Steps[0].Image = "" },
			pipeline.CodeInvalidValue,
		},
		{
			"empty step commands",
			func(d *pipeline.Document) { d.Steps[0].Commands = nil },
			pipeline.CodeInvalidValue,
		},
		{
			"empty command",
			func(d *pipeline.Document) { d.Steps[0].Commands = []string{""} },
			pipeline.CodeInvalidValue,
		},
		{
			"no steps",
			func(d *pipeline.Document) { d.Steps = nil },
			pipeline.CodeInvalidValue,
		},
		{
			"too many steps",
			func(d *pipeline.Document) { d.Steps = manySteps(65) },
			pipeline.CodeInvalidValue,
		},
		{
			"too many services",
			func(d *pipeline.Document) { d.Services = manyServices(33) },
			pipeline.CodeInvalidValue,
		},
		{
			"empty service image",
			func(d *pipeline.Document) {
				service := d.Services["db"]
				service.Image = ""
				d.Services["db"] = service
			},
			pipeline.CodeInvalidValue,
		},
		{
			"bad trigger event",
			func(d *pipeline.Document) { d.Triggers.Git.Events = []string{"pull_request"} },
			pipeline.CodeInvalidValue,
		},
		{
			"empty branch pattern",
			func(d *pipeline.Document) { d.Triggers.Git.Branches = []string{""} },
			pipeline.CodeInvalidValue,
		},
		{
			"branch pattern with whitespace",
			func(d *pipeline.Document) { d.Triggers.Git.Branches = []string{"release 1"} },
			pipeline.CodeInvalidValue,
		},
		{
			"tag pattern with whitespace",
			func(d *pipeline.Document) { d.Triggers.Git.Tags = []string{"v1\tfinal"} },
			pipeline.CodeInvalidValue,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			doc := validDocument()
			test.edit(&doc)
			err := doc.Validate()
			if test.code == "" {
				if err != nil {
					t.Fatalf("valid document rejected: %v", err)
				}
				return
			}
			requireDiagnostic(t, err, test.code)
		})
	}
}

func TestValidateBoundsAccepted(t *testing.T) {
	doc := validDocument()
	doc.Steps = make([]pipeline.Step, 64)
	for i := range doc.Steps {
		doc.Steps[i] = pipeline.Step{Name: fmt.Sprintf("s%d", i), Image: "alpine", Commands: []string{"true"}}
	}
	doc.Services = make(map[string]pipeline.Service, 32)
	for i := 0; i < 32; i++ {
		doc.Services[fmt.Sprintf("s%d", i)] = pipeline.Service{Image: "alpine", Ports: []int{1, 2, 3, 4, 5, 6, 7, 8}}
	}
	if err := doc.Validate(); err != nil {
		t.Fatalf("boundary document rejected: %v", err)
	}
}

func TestOrder(t *testing.T) {
	step := func(name string, dependsOn ...string) pipeline.Step {
		return pipeline.Step{Name: name, Image: "alpine", DependsOn: dependsOn, Commands: []string{"true"}}
	}
	tests := []struct {
		name string
		doc  pipeline.Document
		want []string
	}{
		{
			name: "declaration order breaks ties",
			doc: pipeline.Document{Name: "p", Steps: []pipeline.Step{
				step("a"),
				step("b", "a"),
				step("c"),
				step("d", "c"),
			}},
			want: []string{"a", "b", "c", "d"},
		},
		{
			name: "smallest declaration index first",
			doc: pipeline.Document{Name: "p", Steps: []pipeline.Step{
				step("b", "c"),
				step("c"),
				step("a"),
			}},
			want: []string{"c", "b", "a"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			order, err := test.doc.Order()
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(order, test.want) {
				t.Fatalf("order = %v, want %v", order, test.want)
			}
		})
	}
}

func TestOrderRejectsCycle(t *testing.T) {
	doc := pipeline.Document{Name: "p", Steps: []pipeline.Step{
		{Name: "a", Image: "alpine", DependsOn: []string{"b"}, Commands: []string{"true"}},
		{Name: "b", Image: "alpine", DependsOn: []string{"a"}, Commands: []string{"true"}},
	}}
	_, err := doc.Order()
	requireDiagnostic(t, err, pipeline.CodeDependencyCycle)
}

func TestCanonical(t *testing.T) {
	doc := validDocument()
	first, err := doc.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	second, err := doc.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("canonical bytes differ:\n%s\n%s", first, second)
	}
	if doc.Services["db"].Scope != "" {
		t.Fatalf("Canonical mutated the receiver: scope = %q", doc.Services["db"].Scope)
	}

	var decoded pipeline.Document
	if err := json.Unmarshal(first, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Services["db"].Scope != "pipeline" {
		t.Fatalf("default scope = %q, want pipeline", decoded.Services["db"].Scope)
	}
	if decoded.Name != doc.Name || len(decoded.Steps) != len(doc.Steps) {
		t.Fatalf("canonical document lost data: %+v", decoded)
	}

	// Map insertion order must not affect the canonical bytes.
	reordered := validDocument()
	reordered.Services["db"] = pipeline.Service{
		Image:   "postgres:16-alpine",
		Ports:   []int{5432},
		Secrets: []string{"TEST_DB_PASSWORD"},
		Env:     map[string]string{"POSTGRES_USER": "kodama"},
	}
	third, err := reordered.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, third) {
		t.Fatalf("canonical bytes depend on map insertion order:\n%s\n%s", first, third)
	}
}
