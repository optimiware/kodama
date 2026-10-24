package gate_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/cli/client"
	"github.com/optimiware/kodama/internal/cli/client/clienttest"
	"github.com/optimiware/kodama/internal/cli/gate"
	"github.com/optimiware/kodama/internal/cli/root"
)

const (
	tenant  = "t_acme"
	project = "p_inventory"
	base    = "/api/v1/tenants/" + tenant + "/projects/" + project
	gateID  = "gate_0123456789abcdef0123456789abcdef"
	digest  = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	gatesAt = base + "/gates/" + gateID
	decides = gatesAt + "/decisions"
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
// server with the gate contribution.
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
	}, gate.NewCommands())
	if err != nil {
		c.t.Fatal(err)
	}
	full := append([]string{}, args[:2]...)
	full = append(full, "--factory", c.srv.URL, "--tenant", tenant, "--project", project)
	full = append(full, args[2:]...)
	code := r.Run(ctx, full)
	return result{code, out.String(), errb.String()}
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

// decideArgs is a complete, valid decision command.
func decideArgs(extra ...string) []string {
	args := []string{"gate", "decide", gateID, "--decision", "approve", "--subject-digest", digest,
		"--reason", "Reviewed the identified source and test evidence.", "--if-version", "1",
		"--idempotency-key", "decision-01"}
	return append(args, extra...)
}

// withFlag replaces the value of one flag in a decision command.
func withFlag(args []string, name, value string) []string {
	out := append([]string{}, args...)
	for i, a := range out {
		if a == "--"+name {
			out[i+1] = value
			return out
		}
	}
	return append(out, "--"+name, value)
}

// without removes a flag and its value.
func without(args []string, name string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--"+name {
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}

func gateJSON(status string, version int64) map[string]any {
	g := map[string]any{
		"id": gateID, "runId": "run_0123456789abcdef01234567", "stepId": "review",
		"status": status, "subjectDigest": digest,
		"subject": map[string]any{
			"tenantId": tenant, "projectId": project, "workItemId": "wi_1042", "contentRevision": 2,
			"candidateCommit": "1111111111111111111111111111111111111111", "definitionId": "pd_01",
			"evidence":     []any{map[string]any{"id": "art_tests", "digest": digest}},
			"deploymentId": nil, "deploymentConfigDigest": nil,
		},
		"required": map[string]any{
			"evidence": []any{map[string]any{"role": "evidence", "id": "art_tests", "digest": digest,
				"expression": "${{ steps.test.outputs.report }}", "producer": "test"}},
			"conformance": nil,
		},
		"requestedAt": "2026-10-19T10:05:00Z", "deadline": "2026-10-20T10:05:00Z",
		"decision": nil, "invalidation": nil,
		"applicability": map[string]any{"expired": false, "applicable": false, "reason": "not_approved"},
		"version":       version, "createdAt": "2026-10-19T10:05:00Z", "updatedAt": "2026-10-19T10:05:00Z",
	}
	if status != "pending" {
		decision := "approve"
		if status == "rejected" {
			decision = "reject"
		}
		g["decision"] = map[string]any{
			"decidedBy": map[string]any{"type": "user", "id": "usr_reviewer"}, "decision": decision,
			"reason": "Reviewed the identified source and test evidence.", "decidedAt": "2026-10-19T11:00:00Z",
		}
		g["applicability"] = map[string]any{"expired": false, "applicable": status == "approved", "reason": nil}
	}
	return g
}
