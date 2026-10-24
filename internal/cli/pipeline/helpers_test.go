package pipeline_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/cli/client"
	"github.com/optimiware/kodama/internal/cli/client/clienttest"
	clipipeline "github.com/optimiware/kodama/internal/cli/pipeline"
	"github.com/optimiware/kodama/internal/cli/root"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
)

const (
	tenant  = "t_acme"
	project = "p_inventory"
	base    = "/api/v1/tenants/" + tenant + "/projects/" + project
)

type tokens struct{}

func (tokens) TokenSource(root.EffectiveContext) (client.AccessTokenSource, error) {
	return clienttest.Tokens("tok-1"), nil
}

type result struct {
	code root.ExitCode
	out  string
	err  string
}

var diagnosticActor = pipeline.Actor{ID: "local-cli", Type: pipeline.ActorUser}

func validatorWith(t *testing.T, cfg pipeline.Config) *pipeline.Validator {
	t.Helper()
	v, err := pipeline.NewValidator(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func m1Validator(t *testing.T) *pipeline.Validator {
	return validatorWith(t, pipeline.Config{
		Capabilities: pipeline.EnabledKinds{pipeline.KindContainer},
		References: pipelinetest.References{
			Secrets:      map[string]bool{"registry-token": true, "netrc": true, "token": true},
			Environments: map[string]bool{"env_staging": true},
		},
	})
}

// runWith runs the contribution; factory may be empty for offline commands.
func runWith(t *testing.T, v *pipeline.Validator, factory string, stdin string, args ...string) result {
	t.Helper()
	contrib, err := clipipeline.NewCommands(clipipeline.Config{Validator: v, Actor: diagnosticActor})
	if err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	r, err := root.NewRunner(root.RunnerConfig{
		Streams: root.Streams{In: strings.NewReader(stdin), Out: &out, Err: &errb}, Tokens: tokens{}, AllowInsecureLoopback: true,
	}, contrib)
	if err != nil {
		t.Fatal(err)
	}
	full := append([]string{}, args[:2]...)
	if factory != "" {
		full = append(full, "--factory", factory)
	}
	full = append(full, "--tenant", tenant, "--project", project)
	full = append(full, args[2:]...)
	code := r.Run(context.Background(), full)
	return result{code, out.String(), errb.String()}
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func readFixture(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "pipeline", "v1", "fixtures", rel))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func definitionJSON(id, name string, version int, digest string) map[string]any {
	return map[string]any{
		"id": id, "name": name, "version": version, "digest": digest,
		"definition": map[string]any{"apiVersion": "kodama/v1alpha1", "kind": "Pipeline"},
		"createdBy":  map[string]any{"type": "user", "id": "usr_engineer"}, "createdAt": "2026-10-19T10:05:00Z",
	}
}

func scopeOf() domain.Scope { return domain.Scope{TenantID: tenant, ProjectID: project} }
