package pipeline_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/shell/pipeline"
	"github.com/optimiware/kodema/internal/shell/tenancy"
)

func TestPipelineDecodeAndAdmitCachesAndBudget(t *testing.T) {
	validYAML := []byte(`
budget:
  max_tokens: 25000
  max_cost_usd: 1.25
actions:
  - name: build
    image: registry.example.com/builder:1.0
    commands:
      - go test ./...
    caches:
      - go-mod
      - npm
    budget:
      max_tokens: 10000
      max_cost_usd: 0.5
`)

	decoded, err := pipeline.Decode(validYAML, pipeline.DefaultLimits)
	if err != nil {
		t.Fatalf("Decode valid caches+budget YAML: %v", err)
	}
	if decoded.Budget == nil || decoded.Budget.MaxTokens != 25000 || decoded.Budget.MaxCostUSD != 1.25 {
		t.Fatalf("pipeline budget = %+v; want max_tokens=25000 max_cost_usd=1.25", decoded.Budget)
	}
	c := decoded.Actions[0].Container
	if c == nil || len(c.Caches) != 2 || c.Caches[0] != "go-mod" || c.Caches[1] != "npm" {
		t.Fatalf("container caches = %+v; want [go-mod npm]", c)
	}
	if c.Budget == nil || c.Budget.MaxTokens != 10000 || c.Budget.MaxCostUSD != 0.5 {
		t.Fatalf("action budget = %+v; want max_tokens=10000 max_cost_usd=0.5", c.Budget)
	}

	resolver := &fakeResolver{}
	svc, err := pipeline.NewAdmissionService(pipeline.AdmissionConfig{Resolver: resolver})
	if err != nil {
		t.Fatalf("NewAdmissionService: %v", err)
	}
	admitted, err := svc.Admit(context.Background(), pipeline.AdmitRequest{
		Scope:          tenancy.Scope{TenantID: id.TenantID("t1"), ProjectID: id.ProjectID("p1")},
		Definition:     pipeline.Definition{YAML: validYAML},
		DefaultTimeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Admit valid caches+budget: %v", err)
	}
	if admitted.Snapshot.Budget == nil || admitted.Snapshot.Budget.MaxTokens != 25000 {
		t.Fatalf("admitted snapshot budget = %+v", admitted.Snapshot.Budget)
	}
	if len(admitted.Actions[0].Container.Caches) != 2 || admitted.Actions[0].Container.Caches[0] != "go-mod" {
		t.Fatalf("admitted action caches = %+v", admitted.Actions[0].Container.Caches)
	}
	if admitted.Actions[0].Container.Budget == nil || admitted.Actions[0].Container.Budget.MaxTokens != 10000 {
		t.Fatalf("admitted action budget = %+v", admitted.Actions[0].Container.Budget)
	}

	refusals := []struct {
		name string
		yaml string
	}{
		{
			name: "request action declares caches",
			yaml: `
actions:
  - name: review
    request: role/reviewer
    caches: [go-mod]
    subject:
      item: "x"
`,
		},
		{
			name: "request action declares budget",
			yaml: `
actions:
  - name: review
    request: role/reviewer
    budget:
      max_tokens: 1000
    subject:
      item: "x"
`,
		},
		{
			name: "cache path traversal",
			yaml: `
actions:
  - name: build
    image: registry.example.com/builder:1.0
    commands: ["true"]
    caches: ["../escape"]
`,
		},
		{
			name: "cache slash",
			yaml: `
actions:
  - name: build
    image: registry.example.com/builder:1.0
    commands: ["true"]
    caches: ["a/b"]
`,
		},
		{
			name: "duplicate caches",
			yaml: `
actions:
  - name: build
    image: registry.example.com/builder:1.0
    commands: ["true"]
    caches: ["npm", "npm"]
`,
		},
		{
			name: "negative budget",
			yaml: `
budget:
  max_tokens: -10
actions:
  - name: build
    image: registry.example.com/builder:1.0
    commands: ["true"]
`,
		},
		{
			name: "empty budget mapping",
			yaml: `
budget: {}
actions:
  - name: build
    image: registry.example.com/builder:1.0
    commands: ["true"]
`,
		},
		{
			name: "unknown budget field",
			yaml: `
budget:
  max_dollars: 5
actions:
  - name: build
    image: registry.example.com/builder:1.0
    commands: ["true"]
`,
		},
	}

	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pipeline.Decode([]byte(strings.TrimSpace(tc.yaml)), pipeline.DefaultLimits)
			if err == nil {
				t.Fatalf("%s: Decode succeeded; want invalid_input", tc.name)
			}
			if class, ok := fault.ClassOf(err); !ok || class != fault.InvalidInput {
				t.Fatalf("%s: fault class = %q (%v); want invalid_input (err=%v)", tc.name, class, ok, err)
			}
		})
	}
}
