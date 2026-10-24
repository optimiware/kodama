package kit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/optimiware/kodama/api/client"
)

// AgentInputs are the declared inputs of every stub-agent pipeline: the
// behavior knobs of the deterministic agent (see agent.go).
func AgentInputs() map[string]any {
	in := map[string]any{}
	for _, n := range []string{"outcome", "model_calls", "tool_calls", "tool", "max_tokens", "registration", "probe", "hold"} {
		in[n] = map[string]any{"type": "string", "default": ""}
	}
	return in
}

// AgentStep is the agent step of the ERP pipeline: a profile reference, no
// workspace (the Podman runtime issues no checkout credential, so a step that
// consumes source is refused at launch: ErrNoCheckout) and the stub's outputs.
func (e *Env) AgentStep(id string, needs ...string) map[string]any {
	return e.AgentStepFor(id, e.Profile, needs...)
}

// AgentStepFor is AgentStep naming another immutable profile reference.
func (e *Env) AgentStepFor(id, profile string, needs ...string) map[string]any {
	st := map[string]any{
		"id": id, "kind": "agent", "agent": map[string]any{"profile": profile}, "timeoutSeconds": 240,
		"workspace": map[string]any{"access": "none"},
		"outputs":   map[string]any{"trace": map[string]any{"type": "string"}, "summary": map[string]any{"type": "string"}},
	}
	if len(needs) > 0 {
		st["needs"] = needs
	}
	return st
}

// ContainerStep is a workspace-free container step in the pinned image running
// a shell script, with string and artifact outputs.
func ContainerStep(id, script string, outputs map[string]any, needs ...string) map[string]any {
	st := map[string]any{
		"id": id, "kind": "container", "timeoutSeconds": 120, "image": PodmanImage,
		"command": []string{"/bin/sh", "-ec"}, "args": []string{script}, "workspace": map[string]any{"access": "none"},
	}
	if len(outputs) > 0 {
		st["outputs"] = outputs
	}
	if len(needs) > 0 {
		st["needs"] = needs
	}
	return st
}

// GateStep is a human gate over evidence expressions.
func GateStep(id string, evidence []string, needs ...string) map[string]any {
	ev := make([]any, 0, len(evidence))
	for _, x := range evidence {
		ev = append(ev, x)
	}
	st := map[string]any{"id": id, "kind": "gate", "gate": map[string]any{"evidence": ev}}
	if len(needs) > 0 {
		st["needs"] = needs
	}
	return st
}

// Register registers a pipeline as the engineer.
func (e *Env) Register(name string, inputs map[string]any, steps ...any) client.PipelineDefinition {
	t := e.T
	t.Helper()
	doc := map[string]any{
		"apiVersion": "kodama/v1alpha1", "kind": "Pipeline", "metadata": map[string]any{"name": name},
		"spec": map[string]any{"inputs": inputs, "steps": steps},
	}
	code, b := e.Do("engineer", "POST", e.ProjectPath()+"/pipeline-definitions", doc, nil)
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("register %s: HTTP %d: %s", name, code, b)
	}
	var d client.PipelineDefinition
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// TryRun submits a run of the definition for the item at its current content
// revision and the repository's initial commit and reports the raw answer.
func (e *Env) TryRun(key string, item client.WorkItem, def string, inputs map[string]any) (int, []byte) {
	req := map[string]any{"workItemId": item.Id, "definitionId": def, "contentRevision": int64(item.ContentRevision), "sourceCommit": e.Repo.Initial}
	if len(inputs) > 0 {
		req["inputs"] = inputs
	}
	return e.Do("engineer", "POST", e.ProjectPath()+"/runs", req, map[string]string{"Idempotency-Key": key})
}

// TryRunAt is TryRun at an explicit source commit.
func (e *Env) TryRunAt(key string, item client.WorkItem, def, commit string, inputs map[string]any) (int, []byte) {
	req := map[string]any{"workItemId": item.Id, "definitionId": def, "contentRevision": int64(item.ContentRevision), "sourceCommit": commit}
	if len(inputs) > 0 {
		req["inputs"] = inputs
	}
	return e.Do("engineer", "POST", e.ProjectPath()+"/runs", req, map[string]string{"Idempotency-Key": key})
}

// StartRunAt submits a run at the source commit and requires admission.
func (e *Env) StartRunAt(key string, item client.WorkItem, def, commit string, inputs map[string]any) client.Run {
	t := e.T
	t.Helper()
	code, b := e.TryRunAt(key, item, def, commit, inputs)
	if code != http.StatusCreated {
		t.Fatalf("admit run: HTTP %d: %s", code, b)
	}
	var r client.Run
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// StartRun submits a run and requires admission.
func (e *Env) StartRun(key string, item client.WorkItem, def string, inputs map[string]any) client.Run {
	t := e.T
	t.Helper()
	code, b := e.TryRun(key, item, def, inputs)
	if code != http.StatusCreated {
		t.Fatalf("admit run: HTTP %d: %s", code, b)
	}
	var r client.Run
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// Item creates and provisions a work item.
func (e *Env) Item(title, spec string) client.WorkItem {
	e.T.Helper()
	item := e.World.WorkItem(title, spec)
	e.World.Provision(item.Id)
	return item
}

// StepOf returns the run's step by identifier.
func (e *Env) StepOf(run, id string) client.Step {
	e.T.Helper()
	for _, s := range e.World.Steps(run) {
		if s.Id == id {
			return s
		}
	}
	e.T.Fatalf("run %s has no step %s", run, id)
	return client.Step{}
}

// WaitStep waits until the step has the status.
func (e *Env) WaitStep(run, id string, status client.StepStatus, within time.Duration) client.Step {
	e.T.Helper()
	var last client.Step
	WaitFor(e.T, fmt.Sprintf("step %s of %s to be %s", id, run, status), within, func() bool {
		for _, s := range e.World.Steps(run) {
			if s.Id == id {
				last = s
				return s.Status == status
			}
		}
		return false
	})
	return last
}

// Output reads a string output of a step ("" when absent).
func Output(s client.Step, name string) string {
	if s.Outputs == nil {
		return ""
	}
	raw, ok := (*s.Outputs)[name]
	if !ok {
		return ""
	}
	b, _ := json.Marshal(raw)
	var v string
	_ = json.Unmarshal(b, &v)
	return v
}

// AttemptLogs returns the retained log lines of one attempt (waiting for the
// capture, which precedes the removal of the workload).
func (e *Env) AttemptLogs(persona, run, attempt string) []string {
	t := e.T
	t.Helper()
	var lines []string
	WaitFor(t, "the retained log of "+attempt, 60*time.Second, func() bool {
		resp, body := e.Raw("GET", fmt.Sprintf("/api/v1/tenants/%s/projects/%s/runs/%s/logs?attemptId=%s&tail=200", TenantA, e.World.Project, run, attempt), e.Token(persona), nil, nil)
		if resp.StatusCode != http.StatusOK {
			return false
		}
		lines = lines[:0]
		for _, l := range strings.Split(string(body), "\n") {
			var rec struct{ Type, Text string }
			if json.Unmarshal([]byte(l), &rec) == nil && rec.Type == "chunk" {
				lines = append(lines, rec.Text)
			}
		}
		return true
	})
	return lines
}

// ReviewPipeline is the nearest runnable form of the ERP pipeline on the Podman
// runtime: the agent step (profile pinned), a workspace-free test step with a
// scalar report, a human review gate with no evidence (an artifact produced
// without source cannot be bound to a candidate: see docs/acceptance/m2.md),
// and the build step the gate protects.
func (e *Env) ReviewPipeline(name string) client.PipelineDefinition {
	return e.Register(name, AgentInputs(),
		e.AgentStep("implement"),
		ContainerStep("test", `printf '{"outcome":"completed","outputs":{"passed":"yes"}}' > "$KODAMA_RESULTS_FILE"`,
			map[string]any{"passed": map[string]any{"type": "string"}}, "implement"),
		GateStep("review", nil, "test"),
		ContainerStep("build", `echo "building the reviewed candidate"; printf '{"outcome":"completed","outputs":{"built":"yes"}}' > "$KODAMA_RESULTS_FILE"`,
			map[string]any{"built": map[string]any{"type": "string"}}, "review"),
	)
}

// AgentRunInputs are the run inputs of a stub-agent pipeline that makes no model
// or tool call (the assembled core grants no attempt model.invoke or
// tool.invoke: docs/acceptance/m2.md, finding 1).
func (e *Env) AgentRunInputs(outcome string) map[string]any {
	return map[string]any{"registration": e.Registration, "model_calls": "0", "tool_calls": "0", "outcome": outcome}
}

// EditSpec edits the work item's specification as the engineer and returns the
// item at its new content revision.
func (e *Env) EditSpec(item client.WorkItem, spec string) client.WorkItem {
	t := e.T
	t.Helper()
	r, err := e.API("engineer").UpdateWorkItemWithResponse(context.Background(), TenantA, e.World.Project, item.Id,
		&client.UpdateWorkItemParams{IfMatch: fmt.Sprintf(`"%d"`, item.Version)}, client.UpdateWorkItem{Spec: &spec})
	if err != nil {
		t.Fatal(err)
	}
	Expect(t, r, r.Body, http.StatusOK, "edit the specification")
	return *r.JSON200
}

// GetItem reads the work item.
func (e *Env) GetItem(id string) client.WorkItem {
	t := e.T
	t.Helper()
	r, err := e.API("engineer").GetWorkItemWithResponse(context.Background(), TenantA, e.World.Project, id)
	if err != nil {
		t.Fatal(err)
	}
	Expect(t, r, r.Body, http.StatusOK, "get the work item")
	return *r.JSON200
}

// AttemptQuestion is the needs_input question recorded on the attempt.
func (e *Env) AttemptQuestion(attempt string) string {
	var q *string
	e.MustScan(&q, `SELECT result->>'question' FROM step_attempts WHERE id = $1`, attempt)
	if q == nil {
		return ""
	}
	return *q
}

// Attempts counts the run's attempts (all steps).
func (e *Env) Attempts(run string) int {
	return e.OwnerCount(`SELECT count(*) FROM step_attempts WHERE run_id = $1`, run)
}

// GetRunStatus is the run's current status.
func (e *Env) GetRunStatus(id string) string { return string(e.World.GetRun(id).Status) }

// Live is a running agent attempt and what an operator with access to its
// container can read: the workload's own runtime token.
type Live struct {
	Run, Attempt, Container, Token string
}

// WaitLive waits for the run's first attempt to be running and reads its token.
func (e *Env) WaitLive(run string) Live {
	e.T.Helper()
	att := e.World.WaitAttempt(run, time.Minute)
	c := e.WaitContainer(TenantA, att, "running", time.Minute)
	return Live{Run: run, Attempt: att, Container: c.ID, Token: e.RuntimeToken(c.ID)}
}

// ReadInContainer reads a file the live attempt wrote (the stub's /tmp files).
func (e *Env) ReadInContainer(l Live, path string) string {
	out, err := e.Exec(l.Container, "cat", path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// Workload issues a request to the server (primary or the given base) with a
// runtime token, as the workload would, and returns the status and body.
func (e *Env) Workload(base, method, path, token string, body []byte, headers map[string]string) (int, []byte) {
	e.T.Helper()
	req, err := http.NewRequest(method, base+path, bytes.NewReader(body))
	if err != nil {
		e.T.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		e.T.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}
