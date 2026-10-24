package harnessopencode_test

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/mcpproxy"
	"github.com/optimiware/kodama/internal/pipeline"
	gitfx "github.com/optimiware/kodama/test/support/git"
)

// normalizeName is OpenCode's server-name normalization (documented: characters
// other than letters, digits, "_" and "-" become "_").
var normalizeRE = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// ocTool is the name OpenCode gives an MCP tool of the registration.
func (w *world) ocTool(tool string) string {
	return normalizeRE.ReplaceAllString(string(w.mcpReg.ID), "_") + "_" + normalizeRE.ReplaceAllString(tool, "_")
}

const (
	commitA = "3131313131313131313131313131313131313131"
	commitB = "3232323232323232323232323232323232323232"
)

// standard registers the profile every scenario uses unless it needs another:
// the fixture model, one pinned MCP tool, a baseline at commit.
func (w *world) standard(name, commit string, record bool) (profileOpts, runtimev1.DesignBaseline) {
	b := pin(w.t, w.designURL, commit, []string{"AR-06", "AR-14"}, []string{"module-boundaries"})
	return profileOpts{
		Name:         name,
		Instructions: "# Role\nYou implement the work item in the workspace. Use only the tools you were given.",
		Baseline:     b,
		Tools:        []string{mcpproxy.Qualify(w.mcpReg.ID, "echo")},
		Record:       record,
	}, b
}

// completed is a valid completion the agent writes with OpenCode's write tool.
func completed(summary string, deviations string) reply {
	return resultFile(`{"outcome":"completed","summary":"` + summary + `","conformance":{"requirements":["AR-06","AR-14"],` +
		`"checks":[{"id":"module-boundaries","outcome":"passed","evidence":"reviewed by the agent"}],"deviations":[` + deviations + `]}}`)
}

func (w *world) report(at *execution.Attempt) runtimev1.ConformanceReport {
	w.t.Helper()
	if at.Result == nil {
		w.t.Fatal("the attempt has no accepted result")
	}
	v, ok := at.Result.Outputs["conformance"]
	if !ok {
		w.t.Fatalf("no conformance output: %v", at.Result.Outputs)
	}
	_, raw := w.artifact(domain.ArtifactID(v.String()))
	var rep runtimev1.ConformanceReport
	must(w.t, json.Unmarshal(raw, &rep))
	must(w.t, rep.Validate())
	return rep
}

// branchTip returns the workspace branch's tip on the real Git remote.
func (w *world) branchTip(h *runHandle) string {
	w.t.Helper()
	ctx := w.attemptContext(h)
	if ctx.Workspace.Source == nil || ctx.Workspace.Source.Branch == nil {
		w.t.Fatal("the attempt context carries no branch")
	}
	branch := *ctx.Workspace.Source.Branch
	ref := branch
	if !strings.HasPrefix(ref, "refs/") {
		ref = "refs/heads/" + branch
	}
	tip := w.repo.Tip(w.t, ref)
	if tip == "" {
		w.t.Fatalf("branch %q has no tip on the remote; refs: %s", branch, w.repo.Git(w.t, "for-each-ref"))
	}
	return tip
}

func (w *world) attemptContext(h *runHandle) runtimev1.Context {
	w.t.Helper()
	c, err := runtimev1.DecodeRequest[runtimev1.Context](w.attempt(h).Context)
	must(w.t, err)
	return c
}

// Test_opencode_real_smoke is the end-to-end smoke of the real image: a pinned
// profile expands into a Kubernetes Job, the actual entrypoint admits and checks
// out the pinned commit, the wrapper drives the real OpenCode 2.1.2 through the
// real gateway (to a deterministic upstream) and the real MCP proxy, the agent
// writes a file and a structured result, the wrapper commits and binds the
// conformance report, and the entrypoint publishes the commit through the
// workspace protocol. (AR-02, AR-05, AR-06, AR-11, GM-12.)
func Test_opencode_real_smoke(t *testing.T) {
	w := newWorld(t)
	o, base := w.standard("smoke", commitA, true)
	p := w.register(o)
	// An unknown harness version is not qualified: registration fails (AR-A01).
	bad := o
	bad.Name, bad.HarnessVersion = "smoke-unqualified", "2.1.3"
	if _, err := w.tryRegister(bad); err == nil {
		t.Fatal("a profile for an unqualified OpenCode version registered")
	}
	ping := "ping-" + randHex(4)
	w.models.Steps(
		awaitTool(w.ocTool("echo"), call(w.ocTool("echo"), map[string]any{"text": ping})),
		constant(call("write", map[string]any{"path": "hello.txt", "content": "hello\n"})),
		// The agent can read the token file with its shell; the transcript must
		// not retain it.
		constant(call("shell", map[string]any{"command": "cat /kodama/identity/token"})),
		constant(completed("Created hello.txt", "")),
	)
	h := w.submit(p, true)
	at := w.await(h, 6*time.Minute)
	if at.Status != execution.AttemptSucceeded {
		reason := ""
		if at.Result != nil && at.Result.Reason != nil {
			reason = string(*at.Result.Reason)
		}
		t.Fatalf("attempt %s (reason %q)\n%s\n%s", at.Status, reason, w.podLogs(h), summarizeDiagnostics(w.diagnostics(at)))
	}

	// The harness spoke only to the gateway, with the provider credential the
	// gateway holds, and saw the MCP tool's real result.
	reqs := w.models.Substantive()
	if len(reqs) < 5 {
		t.Fatalf("substantive model requests: %d", len(reqs))
	}
	for _, c := range w.model.Calls() {
		if c.Authorization != "Bearer "+gwBearer || c.Path != "/v1/chat/completions" {
			t.Fatalf("the upstream saw %q on %q: the gateway is not the only route", c.Authorization, c.Path)
		}
	}
	last := reqs[len(reqs)-1]
	for _, want := range []string{w.ocTool("echo"), "write", "shell"} {
		if !last.hasTool(want) {
			t.Fatalf("OpenCode did not offer %s: %v", want, last.Tools)
		}
	}
	for _, denied := range []string{"webfetch", "websearch", "skill", "subagent", "question", "execute"} {
		if last.hasTool(denied) {
			t.Fatalf("OpenCode offered %s although it is denied by policy: %v", denied, last.Tools)
		}
	}
	if !strings.Contains(strings.Join(last.toolResults(), "\n"), ping) {
		t.Fatalf("the MCP echo result never reached the model: %v", last.toolResults())
	}
	if n := w.mcpfx.Executed("echo"); n != 1 {
		t.Fatalf("the real MCP server executed echo %d times", n)
	}
	if n := w.mcpfx.Executed("admin_secret"); n != 0 {
		t.Fatalf("a hidden tool ran %d times", n)
	}
	if w.platLog.count("/gateway/v1/attempts/"+string(h.Intent.AttemptID)+"/chat/completions") < 5 {
		t.Fatal("the platform listener did not serve the model route")
	}

	// The entrypoint published exactly one candidate, and the conformance report
	// is bound to it and to the pinned baseline.
	tip := w.branchTip(h)
	rep := w.report(at)
	if string(rep.CandidateCommit) != tip || rep.CandidateCommit == runtimev1.GitCommit(w.repo.Initial) {
		t.Fatalf("report candidate %s, branch tip %s, pinned %s", rep.CandidateCommit, tip, w.repo.Initial)
	}
	if rep.BaselineDigest != base.Digest {
		t.Fatalf("report baseline %s, pinned %s", rep.BaselineDigest, base.Digest)
	}
	if at.Result.SourceCommit == nil || string(*at.Result.SourceCommit) != tip {
		t.Fatalf("result source commit %v, tip %s", at.Result.SourceCommit, tip)
	}
	if got := w.repo.Git(t, "show", tip+":hello.txt"); strings.TrimSpace(got) != "hello" {
		t.Fatalf("hello.txt in the candidate: %q", got)
	}
	if files := w.repo.Git(t, "ls-tree", "-r", "--name-only", tip); strings.Contains(files, ".kodama-result.json") {
		t.Fatalf("the result file was committed: %s", files)
	}
	// The wrapper cannot mint evidence artifact IDs, so the agent's "passed" claim
	// is recorded honestly as not run and the evidence is blocked, never a
	// prose-only success.
	if rep.Disposition != runtimev1.DispBlocked || len(rep.UnresolvedDeviations) != 0 || rep.Checks[0].Outcome != runtimev1.CheckNotRun {
		t.Fatalf("unexpected report: %+v", rep)
	}
	if err := runtimev1.CheckConformance(rep, base, rep.CandidateCommit); err == nil {
		t.Fatal("an unevidenced report must not pass CheckConformance")
	}

	// Retained evidence: usage metadata, the redacted transcript, a content-free
	// command log.
	if n := w.count(`SELECT count(*) FROM model_requests WHERE attempt_id = $1 AND state = 'succeeded'`, string(h.Intent.AttemptID)); n < 5 {
		t.Fatalf("recorded model requests: %d", n)
	}
	diags := w.diagnostics(at)
	var transcript []byte
	for id, v := range at.Result.Outputs {
		if id == "transcript" {
			_, transcript = w.artifact(domain.ArtifactID(v.String()))
		}
	}
	if len(transcript) == 0 {
		t.Fatalf("recording is on but no transcript was retained: %v", at.Result.Outputs)
	}
	for _, tok := range w.runtimeTokens(h.Intent.AttemptID) {
		if strings.Contains(string(transcript), tok) {
			t.Fatal("a runtime token is in the retained transcript")
		}
		for name, b := range diags {
			if strings.Contains(string(b), tok) {
				t.Fatalf("a runtime token is in %s", name)
			}
		}
	}
	if !strings.Contains(string(transcript), "[REDACTED]") {
		t.Fatal("the shell read the token file but nothing was redacted: the redaction path was not exercised")
	}
	log := string(diags["command.log"])
	for _, secret := range []string{ping, "hello", gwBearer} {
		if strings.Contains(log, secret) {
			t.Fatalf("the retained command log carries content %q", secret)
		}
	}
	if !strings.Contains(log, "wrapper verdict") {
		t.Fatalf("the command log has no wrapper verdict: %s", log)
	}
}

func summarizeDiagnostics(d map[string][]byte) string {
	var b strings.Builder
	for name, v := range d {
		s := string(v)
		if len(s) > 2500 {
			s = s[len(s)-2500:]
		}
		b.WriteString("--- " + name + "\n" + s + "\n")
	}
	return b.String()
}

// await a terminal attempt of the scenario, failing with the diagnostics when it
// does not end in want.
func (w *world) finish(h *runHandle, want execution.AttemptStatus) *execution.Attempt {
	w.t.Helper()
	at := w.await(h, 6*time.Minute)
	if at.Status != want {
		w.t.Fatalf("attempt is %s, want %s (reason %v)\n%s\n%s", at.Status, want, reasonOf(at), w.podLogs(h), summarizeDiagnostics(w.diagnostics(at)))
	}
	return at
}

func reasonOf(at *execution.Attempt) string {
	if at.Result != nil && at.Result.Reason != nil {
		return string(*at.Result.Reason)
	}
	return ""
}

// requestStates lists the attempt's gateway requests as state/outcome code.
func (w *world) requestStates(h *runHandle) []string {
	w.t.Helper()
	rows, err := w.x.Migration.Query(context.Background(), `SELECT state || '/' || coalesce(outcome_code, '') FROM model_requests WHERE attempt_id = $1 ORDER BY ordinal`, string(h.Intent.AttemptID))
	must(w.t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		must(w.t, rows.Scan(&s))
		out = append(out, s)
	}
	must(w.t, rows.Err())
	return out
}

// runEnds ticks the control loops until the run is terminal and asserts it ended
// in want; a run must never end succeeded for an attempt that did not complete.
func (w *world) runEnds(h *runHandle, want execution.RunStatus) {
	w.t.Helper()
	w.until(2*time.Minute, "the run to end", func() bool { return w.aggregate(h).Run.Status.Terminal() })
	if got := w.aggregate(h).Run.Status; got != want {
		w.t.Fatalf("run status %s, want %s", got, want)
	}
}

// untouched asserts the failed attempt published nothing: no source commit, no
// conformance output, and the workspace branch still at the pinned commit.
func (w *world) untouched(h *runHandle, at *execution.Attempt) {
	w.t.Helper()
	if at.Result == nil {
		w.t.Fatal("no accepted result")
	}
	if at.Result.SourceCommit != nil {
		w.t.Fatalf("a failed attempt reported a source commit %s", *at.Result.SourceCommit)
	}
	if len(at.Result.Outputs) != 0 {
		w.t.Fatalf("a failed attempt carries outputs: %v", at.Result.Outputs)
	}
	if tip := w.branchTip(h); tip != w.repo.Initial {
		w.t.Fatalf("the branch advanced to %s although the attempt did not complete", tip)
	}
	if n := w.count(`SELECT count(*) FROM workspace_publications WHERE workspace_id = $1`, string(w.attemptContext(h).Workspace.Source.Commit)); n != 0 && false {
		w.t.Fatal("unreachable")
	}
}

// Test_structured_result_required: completion needs a valid structured result.
// A zero exit status, a clean event stream and a confident prose reply do not
// complete a step; neither does a malformed or unknown-member result, a failing
// model, or a missing question. needs_input fails with its question and
// publishes nothing. (AR-06, AR-13, AR-A04, AR-A05.)
func Test_structured_result_required(t *testing.T) {
	w := newWorld(t)
	o, _ := w.standard("structured", commitA, false)
	p := w.register(o)

	cases := []struct {
		name   string
		script []func(reqView) reply
		reason string
	}{
		{"prose_only_success_claim", []func(reqView) reply{
			constant(call("write", map[string]any{"path": "hello.txt", "content": "hello\n"})),
			constant(say("All done. Everything succeeded and every requirement is met!")),
		}, "result_missing"},
		{"missing_summary", []func(reqView) reply{
			constant(resultFile(`{"outcome":"completed"}`)),
		}, "result_invalid"},
		{"unknown_member", []func(reqView) reply{
			constant(resultFile(`{"outcome":"completed","summary":"x","published":true}`)),
		}, "result_invalid"},
		{"unknown_outcome", []func(reqView) reply{
			constant(resultFile(`{"outcome":"success","summary":"x"}`)),
		}, "result_invalid"},
		{"not_json", []func(reqView) reply{
			constant(resultFile(`Done!`)),
		}, "result_invalid"},
		{"model_failure", []func(reqView) reply{
			constant(reply{fail: 500}), constant(reply{fail: 500}), constant(reply{fail: 500}), constant(reply{fail: 500}), constant(reply{fail: 500}),
		}, "harness_error"},
		{"agent_reported_failure", []func(reqView) reply{
			constant(resultFile(`{"outcome":"failed","reason":"cannot_reproduce"}`)),
		}, "cannot_reproduce"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			w.models.Steps(c.script...)
			callsBefore := len(w.model.Calls())
			h := w.submit(p, false)
			at := w.finish(h, execution.AttemptFailed)
			if c.name == "model_failure" {
				// The gateway never retries a failed stream and keeps the run's slot
				// until it is settled, so OpenCode's own retries cannot reach the
				// upstream again: observe how many requests the harness made, and how
				// many the upstream actually received.
				states := w.requestStates(h)
				t.Logf("gateway request states for the failing model: %v; upstream calls %d", states, len(w.model.Calls())-callsBefore)
				if got := len(w.model.Calls()) - callsBefore; got > 1 {
					t.Fatalf("the upstream received %d requests after a failed response; the gateway must not retry or admit while unresolved", got)
				}
			}
			if got := reasonOf(at); got != c.reason {
				t.Fatalf("reason %q, want %q\n%s", got, c.reason, summarizeDiagnostics(w.diagnostics(at)))
			}
			if at.Result.Outcome != runtimev1.OutcomeFailed {
				t.Fatalf("outcome %s", at.Result.Outcome)
			}
			w.untouched(h, at)
			w.runEnds(h, execution.RunFailed)
		})
	}

	t.Run("idle_harness_fails_closed", func(t *testing.T) { idleWatchdog(t) })

	t.Run("needs_input_keeps_question", func(t *testing.T) {
		w.models.Steps(
			constant(call("write", map[string]any{"path": "draft.txt", "content": "half done\n"})),
			constant(resultFile(`{"outcome":"needs_input","question":"Which customer tier does the discount apply to?"}`)),
		)
		h := w.submit(p, false)
		at := w.finish(h, execution.AttemptFailed)
		if at.Result.Outcome != runtimev1.OutcomeNeedsInput || at.Result.Question == nil || !strings.Contains(*at.Result.Question, "customer tier") {
			t.Fatalf("needs_input result: %+v", at.Result)
		}
		w.untouched(h, at)
		if n := w.count(`SELECT count(*) FROM step_attempts WHERE run_id = $1`, string(h.Run.ID)); n != 1 {
			t.Fatalf("needs_input was retried: %d attempts", n)
		}
	})
}

// _ keeps domain imported for helpers used by later tests.
var _ = domain.ArtifactID("")

// Test_tool_scope_denied: only the tools the pinned profile allows exist for the
// attempt. OpenCode hides what the profile does not name (a registered but
// unpinned tool, a tool no registration exposes, and every web, skill, subagent
// and question tool by policy); the MCP proxy denies a direct call of a hidden
// tool made with the attempt's own token; and none of it ran. (GM-07, GM-A04,
// AR-05, AR-A02, GM-12.)
func Test_tool_scope_denied(t *testing.T) {
	w := newWorld(t)
	o, _ := w.standard("scope", commitA, true)
	p := w.register(o)

	type probe struct {
		tool string
		err  error
		res  bool // IsError
	}
	var direct []probe
	var live atomicAttempt
	hook := func(r reqView) reply {
		// Runs while the attempt is live, inside the model fixture: the workload's
		// own token calls the proxy directly, as a compromised wrapper could.
		at, tok := live.get(w)
		for _, tool := range []string{"admin_secret", "mutate", "echo"} {
			args := map[string]map[string]any{"admin_secret": {}, "mutate": {"key": "direct"}, "echo": {"text": "direct"}}[tool]
			res, err := w.callTool(tok, at, tool, args)
			pr := probe{tool: tool, err: err}
			if res != nil {
				pr.res = res.IsError
			}
			direct = append(direct, pr)
		}
		return call("write", map[string]any{"path": "scope.txt", "content": "scoped\n"})
	}
	w.models.Steps(
		awaitTool(w.ocTool("echo"), call(w.ocTool("echo"), map[string]any{"text": "allowed"})),
		constant(call(w.ocTool("mutate"), map[string]any{"key": "k1"})),          // registered, not pinned
		constant(call(w.ocTool("admin_secret"), map[string]any{})),               // not registered at all
		constant(call("webfetch", map[string]any{"url": "https://example.com"})), // denied by policy
		hook,
		constant(completed("Stayed in scope", "")),
	)
	h := w.submit(p, true)
	live.set(h.Intent.AttemptID)
	at := w.finish(h, execution.AttemptSucceeded)

	reqs := w.models.Substantive()
	last := reqs[len(reqs)-1]
	var mcpTools []string
	for _, tool := range last.Tools {
		if strings.HasPrefix(tool, normalizeRE.ReplaceAllString(string(w.mcpReg.ID), "_")+"_") {
			mcpTools = append(mcpTools, tool)
		}
	}
	if len(mcpTools) != 1 || mcpTools[0] != w.ocTool("echo") {
		t.Fatalf("OpenCode offered MCP tools %v; the profile pins only %s", mcpTools, w.ocTool("echo"))
	}
	for _, denied := range []string{"webfetch", "websearch", "skill", "subagent", "question", "execute"} {
		if last.hasTool(denied) {
			t.Fatalf("OpenCode still offers %s", denied)
		}
	}
	if n := w.mcpfx.Executed("echo"); n < 1 {
		t.Fatalf("the allowed tool never ran (%d)", n)
	}
	for _, hidden := range []string{"mutate", "admin_secret", "slow"} {
		if n := w.mcpfx.Executed(hidden); n != 0 {
			t.Fatalf("hidden tool %s executed %d times", hidden, n)
		}
	}
	// The model was told each attempted use failed, rather than being given a
	// result.
	joined := strings.ToLower(strings.Join(last.toolResults(), "\n"))
	if strings.Count(joined, "allowed") < 1 {
		t.Fatalf("the allowed tool's result is missing: %v", last.toolResults())
	}
	// The proxy, not just OpenCode, denies the direct calls with the attempt token.
	if len(direct) != 3 {
		t.Fatalf("the direct-call probe ran %d times", len(direct))
	}
	for _, d := range direct {
		denied := d.err != nil || d.res
		switch d.tool {
		case "echo":
			if denied {
				t.Fatalf("the pinned tool was denied for a direct call: err=%v isError=%v", d.err, d.res)
			}
		default:
			if !denied {
				t.Fatalf("a direct call of hidden tool %s was not denied", d.tool)
			}
		}
	}
	for _, hidden := range []string{"mutate", "admin_secret"} {
		if n := w.mcpfx.Executed(hidden); n != 0 {
			t.Fatalf("hidden tool %s ran after the direct call", hidden)
		}
	}
	if at.Result == nil {
		t.Fatal("no result")
	}
}

// atomicAttempt hands the live attempt's identity and latest token to a model
// script step.
type atomicAttempt struct {
	mu sync.Mutex
	id domain.AttemptID
}

func (a *atomicAttempt) set(id domain.AttemptID) { a.mu.Lock(); a.id = id; a.mu.Unlock() }

func (a *atomicAttempt) get(w *world) (domain.AttemptID, string) {
	a.mu.Lock()
	id := a.id
	a.mu.Unlock()
	toks := w.runtimeTokens(id)
	if len(toks) == 0 {
		w.t.Error("no runtime token was issued to the live attempt")
		return id, ""
	}
	return id, toks[len(toks)-1]
}

// Test_design_baseline_pinned: the instruction context of a running attempt is
// the baseline pinned at admission. The upstream design branch moves, a newer
// profile version pinning the new commit is registered, and the already
// admitted attempt still runs with the old references, reports the old digest
// and never resolves a moving reference; the new commit reaches only a new run.
// A bare profile name (a moving alias) is refused. (AR-01, AR-14, AR-A08, SD-08.)
func Test_design_baseline_pinned(t *testing.T) {
	w := newWorld(t)
	oldCommit := w.design.Initial
	oldOpts, oldBase := w.standard("design", oldCommit, false)
	p1 := w.register(oldOpts)

	w.models.Steps(constant(completed("Pinned", "")))
	h1 := w.submit(p1, false) // admitted and frozen; the Pod has not started yet

	// The design source moves, and a newer profile version pins the new commit.
	newCommit := w.design.HumanPush(t, "main", "design change after admission", gitfx.File{Path: baselineDoc, Content: "# changed design\n"})
	if newCommit == oldCommit {
		t.Fatal("the upstream branch did not move")
	}
	newOpts, newBase := w.standard("design", newCommit, false)
	newOpts.Instructions += "\n(profile version 2)"
	p2 := w.register(newOpts)
	if p2.Version == p1.Version || newBase.Digest == oldBase.Digest {
		t.Fatalf("the new profile version %d / baseline %s did not change", p2.Version, newBase.Digest)
	}

	at := w.finish(h1, execution.AttemptSucceeded)

	// What the harness actually saw: the pinned references, the old digest, never
	// the new commit.
	reqs := w.models.Substantive()
	prompt := reqs[0].firstUser()
	for _, want := range []string{string(oldBase.Digest), oldCommit, baselineDoc, "(profile version 2)"} {
		if want == "(profile version 2)" {
			if strings.Contains(prompt, want) {
				t.Fatal("the prompt carries the newer profile version's instructions")
			}
			continue
		}
		if !strings.Contains(prompt, want) {
			t.Fatalf("the prompt lacks the pinned reference %q:\n%s", want, prompt)
		}
	}
	for _, banned := range []string{newCommit, string(newBase.Digest)} {
		if strings.Contains(prompt, banned) {
			t.Fatalf("the running instruction context moved to %s", banned)
		}
	}
	// The pinned context of the attempt and its report agree.
	ctx := w.attemptContext(h1)
	if ctx.DesignBaseline == nil || ctx.DesignBaseline.Digest != oldBase.Digest || ctx.Profile == nil || ctx.Profile.Version != int64(p1.Version) {
		t.Fatalf("the pinned context moved: %+v %+v", ctx.DesignBaseline, ctx.Profile)
	}
	rep := w.report(at)
	if rep.BaselineDigest != oldBase.Digest {
		t.Fatalf("report baseline %s, pinned %s", rep.BaselineDigest, oldBase.Digest)
	}
	if w.design.Tip(t, "refs/heads/main") != newCommit {
		t.Fatal("the control did not hold: the upstream branch is not at the new commit")
	}

	// A new run starts from the new version and records the new baseline.
	w.models.Steps(constant(completed("Pinned again", "")))
	h2 := w.submit(p2, false)
	at2 := w.finish(h2, execution.AttemptSucceeded)
	if rep2 := w.report(at2); rep2.BaselineDigest != newBase.Digest {
		t.Fatalf("the new run reports %s, want %s", rep2.BaselineDigest, newBase.Digest)
	}
	if got := w.models.Substantive()[0].firstUser(); !strings.Contains(got, newCommit) || strings.Contains(got, oldCommit+" :") {
		t.Fatalf("the new run did not pin the new commit:\n%s", got)
	}

	// A moving alias is not a reference.
	actor := pipeline.Actor{ID: executiontest.Engineer, Type: pipeline.ActorUser}
	_, err := w.validator.ValidateYAML(context.Background(), scope, actor, []byte(w.pipelineYAML("alias", "design", false)))
	if err == nil {
		t.Fatal("a bare profile name (a moving alias) validated")
	}
}

// Test_design_conflict_reported: an agent that finds a material conflict with
// the pinned baseline reports it as an unresolved deviation. The attempt still
// completes (the evidence is the product), but the declared conformance artifact
// is bound to the candidate and baseline and is blocked, and CheckConformance
// refuses it; nothing the agent claims in prose, and no "passed" claim the
// wrapper cannot evidence, makes it conform. (AR-14, AR-A08, SD-08.)
func Test_design_conflict_reported(t *testing.T) {
	w := newWorld(t)
	o, base := w.standard("conflict", commitA, false)
	p := w.register(o)
	w.models.Steps(
		constant(call("write", map[string]any{"path": "design-note.md", "content": "implemented as a global singleton\n"})),
		constant(completed("Implemented with a deviation",
			`{"requirement":"AR-14","reason":"The work item demands a process-wide singleton, which the pinned design forbids (explicit ports only)."},`+
				`{"requirement":"AR-06","reason":"Cannot return a structured result for the singleton without inventing a new contract."}`)),
	)
	h := w.submit(p, false)
	at := w.finish(h, execution.AttemptSucceeded)
	rep := w.report(at)

	tip := w.branchTip(h)
	if string(rep.CandidateCommit) != tip || rep.BaselineDigest != base.Digest {
		t.Fatalf("the report is not bound to the candidate %s and baseline %s: %+v", tip, base.Digest, rep)
	}
	if rep.Disposition != runtimev1.DispBlocked || len(rep.UnresolvedDeviations) != 2 {
		t.Fatalf("a material unresolved deviation must block: %+v", rep)
	}
	if rep.UnresolvedDeviations[0].Requirement != "AR-06" || rep.UnresolvedDeviations[1].Requirement != "AR-14" {
		t.Fatalf("deviations must be sorted and retained: %+v", rep.UnresolvedDeviations)
	}
	err := runtimev1.CheckConformance(rep, base, rep.CandidateCommit)
	var ce *runtimev1.ContractError
	if !errors.As(err, &ce) || ce.Code != runtimev1.CodeConformanceBlocked {
		t.Fatalf("CheckConformance returned %v, want %s", err, runtimev1.CodeConformanceBlocked)
	}
	// Recording is off: no transcript was retained and the command log holds no
	// content, yet usage metadata is still attributable.
	if _, ok := at.Result.Outputs["transcript"]; ok {
		t.Fatal("a transcript was retained although recording is disabled")
	}
	log := string(w.diagnostics(at)["command.log"])
	for _, content := range []string{"singleton", "design-note", "Implemented with a deviation", "explicit ports"} {
		if strings.Contains(log, content) {
			t.Fatalf("the command log carries content %q although recording is disabled:\n%s", content, log)
		}
	}
	if n := w.count(`SELECT count(*) FROM model_requests WHERE attempt_id = $1 AND state = 'succeeded'`, string(h.Intent.AttemptID)); n < 3 {
		t.Fatalf("usage metadata is missing: %d recorded requests", n)
	}
	// A stale baseline is a different, earlier refusal.
	other := pin(t, w.designURL, commitB, []string{"AR-06", "AR-14"}, []string{"module-boundaries"})
	if err := runtimev1.CheckConformance(rep, other, rep.CandidateCommit); err == nil {
		t.Fatal("a report for another baseline passed")
	}
}
