package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/cli/agent"
	"github.com/optimiware/kodama/internal/cli/client"
	"github.com/optimiware/kodama/internal/cli/client/clienttest"
	"github.com/optimiware/kodama/internal/cli/root"
)

const (
	tenant   = "t_acme"
	project  = "p_inventory"
	base     = "/api/v1/tenants/" + tenant + "/projects/" + project
	profiles = base + "/agent-profiles"
)

type tokenResolver struct{ src client.AccessTokenSource }

func (r tokenResolver) TokenSource(root.EffectiveContext) (client.AccessTokenSource, error) {
	return r.src, nil
}

type result struct {
	code root.ExitCode
	out  string
	err  string
}

// cli runs the real root runner and shared HTTP client against a recording
// server with the agent contribution.
type cli struct {
	t   *testing.T
	srv *clienttest.Server
	in  io.Reader
}

func newCLI(t *testing.T, h http.Handler) *cli {
	t.Helper()
	return &cli{t: t, srv: clienttest.NewServer(t, h)}
}

func (c *cli) run(args ...string) result {
	c.t.Helper()
	return c.runWith(context.Background(), args...)
}

func (c *cli) runWith(ctx context.Context, args ...string) result {
	c.t.Helper()
	var out, errb bytes.Buffer
	r, err := root.NewRunner(root.RunnerConfig{
		Streams:               root.Streams{In: c.in, Out: &out, Err: &errb},
		Tokens:                tokenResolver{clienttest.Tokens("tok-1")},
		AllowInsecureLoopback: true,
	}, agent.NewCommands())
	if err != nil {
		c.t.Fatal(err)
	}
	full := append([]string{}, args[:2]...)
	full = append(full, "--factory", c.srv.URL, "--tenant", tenant, "--project", project)
	full = append(full, args[2:]...)
	code := r.Run(ctx, full)
	return result{code, out.String(), errb.String()}
}

func fixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "profile.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func decodeJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("not a JSON document: %v: %q", err, s)
	}
	return m
}

func profileJSON(version int) map[string]any {
	return map[string]any{
		"id": "ap_0123456789abcdef0123456789abcdef", "name": "coding", "version": version,
		"reference": "coding@" + string(rune('0'+version)),
		"digest":    "sha256:2222222222222222222222222222222222222222222222222222222222222222",
		"profile": map[string]any{
			"name": "coding", "image": "registry.example.invalid/agent@sha256:1111111111111111111111111111111111111111111111111111111111111111",
			"wrapper": map[string]any{"id": "kodama-wrapper", "version": "1.4.2", "command": []any{"/usr/local/bin/kodama-wrapper"},
				"harness": map[string]any{"id": "opencode", "version": "0.9.1"}},
			"model":          map[string]any{"name": "example-model", "endpoint": "https://gateway.example.invalid/v1"},
			"tools":          []any{"repo.read", "repo.search"},
			"limits":         map[string]any{"wallSeconds": 1800},
			"designBaseline": map[string]any{"digest": "sha256:3333333333333333333333333333333333333333333333333333333333333333"},
		},
		"createdBy": map[string]any{"type": "user", "id": "usr_engineer"}, "createdAt": "2026-10-19T10:05:00Z",
	}
}
