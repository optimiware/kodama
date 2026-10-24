package work_test

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

	"github.com/optimiware/kodama/internal/cli/client"
	"github.com/optimiware/kodama/internal/cli/client/clienttest"
	"github.com/optimiware/kodama/internal/cli/root"
	"github.com/optimiware/kodama/internal/cli/work"
)

const (
	tenant  = "t_acme"
	project = "p_inventory"
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

// cli runs the real runner and client against srv with the work contribution.
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
	}, work.NewCommands())
	if err != nil {
		c.t.Fatal(err)
	}
	// Defaults first: a later explicit flag wins.
	full := append([]string{}, args[:2]...)
	full = append(full, "--factory", c.srv.URL, "--tenant", tenant, "--project", project)
	full = append(full, args[2:]...)
	code := r.Run(ctx, full)
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

func body(t *testing.T, r clienttest.Recorded) map[string]any {
	t.Helper()
	return decodeJSON(t, string(r.Body))
}

const (
	base     = "/api/v1/tenants/" + tenant + "/projects/" + project
	workPath = base + "/work-items"
)

func itemJSON(id, state string, version, revision int64) map[string]any {
	return map[string]any{
		"id": id, "kind": "story", "title": "Resolve inventory exceptions", "spec": "# Plan",
		"acceptanceCriteria": []any{}, "evidenceLinks": []any{}, "assigneeId": nil, "parentId": nil,
		"state": state, "blockedReason": nil, "version": version, "contentRevision": revision,
		"reviewSubject": nil, "completion": nil,
		"createdBy": map[string]any{"type": "user", "id": "usr_analyst"},
		"createdAt": "2026-10-19T10:05:00Z", "updatedAt": "2026-10-19T10:05:00Z",
	}
}
