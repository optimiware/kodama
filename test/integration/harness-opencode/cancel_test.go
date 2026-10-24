package harnessopencode_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	wspec "github.com/optimiware/kodama/images/harness-opencode/wrapper/spec"
	"github.com/optimiware/kodama/internal/agents"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/test/support/layer"
)

// openCodeSHA is the checksum of the OpenCode 2.1.2 candidate this task
// qualified. Another build is a different, unqualified artifact.
const openCodeSHA = "22636c800ba6c8dc5353e264183bb3927725d824cba412f007dea9c694740f52"

// Test_cancel_wrapper: cancellation stops the wrapper and everything under it
// and revokes new provider and tool access. Two real-adapter views of it:
//
//   - cluster: a run whose harness is mid model stream, with background
//     processes the agent started, is cancelled through the execution service;
//     the upstream request is cancelled, the Job disappears, the attempt's own
//     token is refused by the gateway and the MCP proxy, and nothing is published.
//   - process_tree: the real wrapper and the real OpenCode on the host under a
//     process group signalled the way the entrypoint signals it; every descendant,
//     including a `setsid` escapee and OpenCode's private server (neither is in the
//     process group), is gone. (AR-09, AR-A06, GM-06.)
func Test_cancel_wrapper(t *testing.T) {
	t.Run("cluster", func(t *testing.T) {
		w := newWorld(t)
		o, _ := w.standard("cancel", commitA, false)
		p := w.register(o)
		hg := newHang()
		w.models.Steps(
			constant(call("shell", map[string]any{"command": "(setsid sleep 4747 >/dev/null 2>&1 &); sleep 4848 >/dev/null 2>&1 & echo started"})),
			constant(reply{hang: hg}),
		)
		h := w.submit(p, false)
		attempt := h.Intent.AttemptID
		w.until(8*time.Minute, "the model stream to stall", func() bool {
			select {
			case <-hg.started:
				return true
			default:
				return false
			}
		})
		tokens := w.runtimeTokens(attempt)
		if len(tokens) == 0 {
			t.Fatal("no token was issued")
		}
		token := tokens[len(tokens)-1]
		before := w.models.Cancelled()
		if _, err := w.svc.Cancel(context.Background(), execution.Caller{PrincipalID: executiontest.Engineer, RequestID: "req_t031"}, scope, h.Run.ID); err != nil {
			t.Fatalf("cancel: %v", err)
		}

		at := w.await(h, 4*time.Minute)
		if at.Status != execution.AttemptCancelled {
			t.Fatalf("attempt %s", at.Status)
		}
		w.runEnds(h, execution.RunCancelled)
		// The upstream request was cancelled with the attempt, not left streaming.
		eventually(t, 60*time.Second, "the upstream stream to be cancelled", func() (bool, string) {
			return w.models.Cancelled() > before, fmt.Sprintf("cancelled=%d", w.models.Cancelled())
		})
		// The gateway kept the conservative charge and the unresolved state; it
		// never recorded a success for the interrupted request.
		var state, code string
		w.one(&state, `SELECT state FROM model_requests WHERE attempt_id = $1 ORDER BY ordinal DESC LIMIT 1`, string(attempt))
		w.one(&code, `SELECT coalesce(outcome_code, '') FROM model_requests WHERE attempt_id = $1 ORDER BY ordinal DESC LIMIT 1`, string(attempt))
		if state == "succeeded" || state == "forwarding" || state == "admitted" {
			t.Fatalf("the interrupted request is %s (%s)", state, code)
		}
		// New provider access is revoked with the attempt's token.
		body := `{"model":"` + modelName + `","stream":true,"messages":[{"role":"user","content":"late"}]}`
		req, _ := http.NewRequest(http.MethodPost, w.plat.URL+"/gateway/v1/attempts/"+string(attempt)+"/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		hc := &http.Client{Timeout: 20 * time.Second, Transport: hostTransport(w.ca, w.c.hostIP)}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatalf("late gateway call: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatal("the gateway admitted a model request after cancellation")
		}
		calls := len(w.model.Calls())
		// New tool access is revoked as well.
		if res, err := w.callTool(token, attempt, "echo", map[string]any{"text": "late"}); err == nil && (res == nil || !res.IsError) {
			t.Fatal("the MCP proxy served a tool call after cancellation")
		}
		if n := w.mcpfx.Executed("echo"); n != 0 {
			t.Fatalf("a tool executed after cancellation: %d", n)
		}
		if got := len(w.model.Calls()); got != calls {
			t.Fatalf("the late request reached the upstream (%d -> %d)", calls, got)
		}
		// The Job and its Pod are gone, and nothing was published.
		w.until(3*time.Minute, "the Pod to be gone", func() bool {
			out, _ := run("kubectl", "--kubeconfig", w.c.kubeconfig, "-n", w.nsA, "get", "pods", "-l", kube.LabelJob+"="+h.Intent.WorkloadName, "-o", "name")
			return strings.TrimSpace(out) == ""
		})
		if tip := w.branchTip(h); tip != w.repo.Initial {
			t.Fatalf("a cancelled attempt moved the branch to %s", tip)
		}
		if at.Result != nil && at.Result.Outcome == runtimev1.OutcomeCompleted {
			t.Fatal("a cancelled attempt completed")
		}
	})

	t.Run("process_tree", func(t *testing.T) { cancelProcessTree(t) })
}

// ---- process tree ------------------------------------------------------------

func openCodeBinary(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("OPENCODE_BIN")
	if bin == "" {
		bin = filepath.Join(os.Getenv("HOME"), ".opencode", "bin", "opencode")
	}
	raw, err := os.ReadFile(bin)
	if err != nil {
		t.Fatalf("the OpenCode candidate is required (OPENCODE_BIN): %v", err)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != openCodeSHA {
		t.Fatalf("OPENCODE_BIN %s has checksum %s, the qualified candidate is %s", bin, got, openCodeSHA)
	}
	return bin
}

func procCmdline(pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return ""
	}
	return strings.ReplaceAll(strings.TrimRight(string(b), "\x00"), "\x00", " ")
}

func procState(pid int) byte {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || len(s) < i+3 {
		return 0
	}
	return s[i+2]
}

func alive(pid int) bool { st := procState(pid); return st != 0 && st != 'Z' }

// procTable maps every pid to its parent and process group.
func procTable() (parent, pgrp map[int]int) {
	parent, pgrp = map[int]int{}, map[int]int{}
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		s := string(b)
		i := strings.LastIndexByte(s, ')')
		f := strings.Fields(s[i+1:])
		if i < 0 || len(f) < 3 {
			continue
		}
		pp, _ := strconv.Atoi(f[1])
		pg, _ := strconv.Atoi(f[2])
		parent[pid], pgrp[pid] = pp, pg
	}
	return
}

func treeOf(root int) []int {
	parent, _ := procTable()
	var out []int
	for pid := range parent {
		for p, hops := parent[pid], 0; p > 1 && hops < 4096; p, hops = parent[p], hops+1 {
			if p == root {
				out = append(out, pid)
				break
			}
		}
	}
	return out
}

// hostRun is the real wrapper and the real OpenCode running on the host against
// a stub platform whose model route runs a shell command that spawns background
// processes (one in its own session) and then stalls its stream.
type hostRun struct {
	pid     int
	exited  chan error
	log     *bytes.Buffer
	results string
	outs    string
	stalled chan struct{}
}

func startHostWrapper(t *testing.T, extraEnv ...string) *hostRun {
	layer.SkipService(t, "the OpenCode binary and host processes")
	bin := openCodeBinary(t)
	dir := t.TempDir()
	ca := newCA(t)
	wrapperBin := filepath.Join(dir, "kodama-opencode-wrapper")
	build := exec.Command("go", "build", "-trimpath", "-o", wrapperBin, "./images/harness-opencode/wrapper")
	build.Dir = repoRoot()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the wrapper: %v\n%s", err, out)
	}

	// The wrapper's payload and the pinned context it must match.
	base := pin(t, "https://git.example.invalid/kodama.git", commitA, []string{"AR-06", "AR-14"}, []string{"module-boundaries"})
	draft := agents.Draft{
		Name: "tree", Image: "registry.invalid/harness@sha256:" + strings.Repeat("ab", 32),
		Wrapper: agents.Wrapper{ID: wspec.WrapperID, Version: wspec.WrapperVersion, Command: []string{wspec.WrapperPath},
			Harness: agents.Harness{ID: wspec.HarnessID, Version: wspec.HarnessVersion}},
		Instructions: "# Role\nWork.", DesignBaseline: base, Model: agents.Model{Name: modelName, Endpoint: "https://gateway.example.invalid/v1"},
		Limits: agents.Limits{WallSeconds: 300},
	}
	content, err := agents.NewContent(draft)
	must(t, err)
	cfg := agents.WrapperConfig{Version: agents.PayloadVersion, Profile: agents.ProfilePin{Name: "tree", Version: 1, Digest: content.Digest()}, Content: content.Draft()}
	args, err := cfg.Args()
	must(t, err)
	prof, bl := cfg.Pins()
	branch := "refs/heads/work"
	tip := runtimev1.GitCommit(strings.Repeat("1", 40))
	conf := wspec.DeclaredOutputPath(wspec.ConformanceFile)
	pctx := runtimev1.Context{
		TenantID: "t_t031", ProjectID: "p_t031", RunID: "run_t031", StepID: "implement", AttemptID: "att_t031_tree", ActionKey: "run_t031.implement",
		Definition:     runtimev1.Definition{ID: "pd_t031", Digest: runtimev1.Digest("sha256:" + strings.Repeat("d", 64))},
		DesignBaseline: &bl, Profile: &prof,
		Command:   runtimev1.Command{Image: draft.Image, Command: []string{wspec.WrapperPath}, Args: args, Env: map[string]string{}},
		Workspace: runtimev1.Workspace{Access: runtimev1.AccessWrite, Source: &runtimev1.Source{RepositoryURL: "https://git.example.invalid/x.git", Commit: tip, Branch: &branch, ExpectedTip: &tip}},
		Inputs:    map[string]runtimev1.Value{}, Outputs: []runtimev1.OutputDecl{{Name: wspec.ConformanceOutput, Type: runtimev1.OutputArtifact, Path: &conf}},
		Secrets: []runtimev1.SecretBinding{},
		Limits:  runtimev1.Limits{TimeoutSeconds: 300, MaxOutputBytes: 1 << 20, MaxOutputFiles: 10, MaxDiagnosticBytes: 1 << 20},
	}
	ctxJSON, err := json.Marshal(pctx)
	must(t, err)
	if _, err := runtimev1.DecodeRequest[runtimev1.Context](ctxJSON); err != nil {
		t.Fatalf("the stub context is not a valid pinned context: %v", err)
	}

	// A stub platform: the context route and a model route that runs a shell
	// command spawning background processes (one in its own session), then stalls.
	var mu sync.Mutex
	toolful := 0
	stalled := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /runtime/v1/attempts/att_t031_tree/context", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(ctxJSON)
	})
	mux.HandleFunc("POST /gateway/v1/attempts/att_t031_tree/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tools []json.RawMessage `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		f := w.(http.Flusher)
		f.Flush()
		write := func(s string) { _, _ = w.Write([]byte(s)); f.Flush() }
		mu.Lock()
		var n int
		if len(body.Tools) > 0 {
			toolful++
			n = toolful
		}
		mu.Unlock()
		switch {
		case len(body.Tools) == 0:
			write(chunk(map[string]any{"role": "assistant", "content": "title"}, nil) + chunk(map[string]any{}, "stop") + usageChunk(4, 1) + "data: [DONE]\n\n")
		case n == 1:
			args, _ := json.Marshal(map[string]any{"command": "(setsid sleep 4242 >/dev/null 2>&1 &); sleep 4343 >/dev/null 2>&1 & echo started"})
			write(chunk(map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "call_1", "type": "function",
				"function": map[string]any{"name": "shell", "arguments": string(args)}}}}, nil) + chunk(map[string]any{}, "tool_calls") + usageChunk(4, 1) + "data: [DONE]\n\n")
		default:
			write(chunk(map[string]any{"role": "assistant", "content": "working"}, nil))
			close(stalled)
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Minute):
			}
		}
	})
	l := serveTLSLoopback(t, ca, 7, mux)

	token := filepath.Join(dir, "token")
	must(t, os.WriteFile(token, []byte("tree-token-"+randHex(8)), 0o600))
	ws, outs := filepath.Join(dir, "workspace"), filepath.Join(dir, "outputs")
	must(t, os.MkdirAll(ws, 0o755))
	must(t, os.MkdirAll(outs, 0o755))
	gi := exec.Command("git", "init", "-q", ws)
	if out, err := gi.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	results := filepath.Join(dir, "results.json")
	logbuf := &bytes.Buffer{}
	cmd := exec.Command(wrapperBin, args...)
	cmd.Dir = ws
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "TMPDIR=" + dir,
		wspec.EnvPlatformURL + "=" + l.URL, wspec.EnvTokenFile + "=" + token, wspec.EnvCAFile + "=" + ca.File, wspec.EnvOpenCodeBin + "=" + bin,
		wspec.EnvAttemptID + "=att_t031_tree", wspec.EnvWorkspace + "=" + ws, wspec.EnvOutputsDir + "=" + outs, wspec.EnvResultsFile + "=" + results,
	}
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.Stdout, cmd.Stderr = logbuf, logbuf
	// Its own process group, exactly as the entrypoint starts its command.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	must(t, cmd.Start())
	pid := cmd.Process.Pid
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		// Only this test's own process group and descendants are ever signalled.
		for _, p := range treeOf(pid) {
			_ = syscall.Kill(p, syscall.SIGKILL)
		}
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	})
	return &hostRun{pid: pid, exited: exited, log: logbuf, results: results, outs: outs, stalled: stalled}
}

// waitStalled returns the wrapper's descendants once the model stream stalled
// after the agent's shell command ran, having checked that the scenario really
// exercises tree containment: processes outside the wrapper's process group, a
// `setsid` escapee, and OpenCode's private server.
func (h *hostRun) waitStalled(t *testing.T) []int {
	t.Helper()
	select {
	case <-h.stalled:
	case err := <-h.exited:
		t.Fatalf("the wrapper ended early: %v\n%s", err, h.log.String())
	case <-time.After(90 * time.Second):
		t.Fatalf("the harness never stalled\n%s", h.log.String())
	}
	var sleepers []int
	eventually(t, 30*time.Second, "the agent's background processes", func() (bool, string) {
		sleepers = nil
		for _, p := range treeOf(h.pid) {
			if c := procCmdline(p); strings.Contains(c, "sleep 4242") || strings.Contains(c, "sleep 4343") {
				sleepers = append(sleepers, p)
			}
		}
		return len(sleepers) == 2, fmt.Sprint(sleepers)
	})
	before := treeOf(h.pid)
	_, pgrp := procTable()
	outOfGroup := 0
	sawServer := false
	for _, p := range before {
		if pgrp[p] != h.pid {
			outOfGroup++
		}
		if c := procCmdline(p); strings.Contains(c, "serve") && strings.Contains(c, "--stdio") {
			sawServer = true
		}
	}
	if !sawServer {
		t.Fatalf("OpenCode's private server is not among the wrapper's descendants: %v", before)
	}
	if outOfGroup < 3 {
		t.Fatalf("the process group alone would miss only %d of %d descendants; the scenario does not exercise tree containment", outOfGroup, len(before))
	}
	escapee := 0
	for _, p := range sleepers {
		if pgrp[p] != h.pid {
			escapee++
		}
	}
	if escapee < 1 {
		t.Fatal("no background process left the wrapper's process group")
	}
	return before
}

func (h *hostRun) allGone(t *testing.T, before []int) {
	t.Helper()
	eventually(t, 10*time.Second, "every descendant to be gone", func() (bool, string) {
		var survivors []string
		for _, p := range before {
			if alive(p) {
				survivors = append(survivors, fmt.Sprintf("%d:%s", p, procCmdline(p)))
			}
		}
		return len(survivors) == 0, strings.Join(survivors, "; ")
	})
}

func (h *hostRun) manifest(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(h.results)
	must(t, err)
	return string(raw)
}

func cancelProcessTree(t *testing.T) {
	h := startHostWrapper(t)
	before := h.waitStalled(t)
	start := time.Now()
	// The entrypoint signals the command's process group.
	if err := syscall.Kill(-h.pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-h.exited:
		ee, ok := err.(*exec.ExitError)
		if !ok || ee.ExitCode() != 143 {
			t.Fatalf("wrapper exit %v, want 143\n%s", err, h.log.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the wrapper did not stop within 30s of SIGTERM\n%s", h.log.String())
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("cancellation took %s", d)
	}
	h.allGone(t, before)
	// Cancellation never reports a verdict: the provisional failed manifest stays.
	if m := h.manifest(t); !strings.Contains(m, `"failed"`) || !strings.Contains(m, wspec.ReasonIncomplete) {
		t.Fatalf("manifest after cancel: %s", m)
	}
	if _, err := os.Stat(filepath.Join(h.outs, wspec.ConformanceFile)); err == nil {
		t.Fatal("a cancelled wrapper produced a conformance artifact")
	}
}

// idleWatchdog: a harness that goes silent (a stream that stalls, as an
// unanswered prompt would) does not wait for a human or for the whole wall time:
// the wrapper kills the tree after its idle bound and fails closed.
func idleWatchdog(t *testing.T) {
	h := startHostWrapper(t, wspec.EnvIdleSeconds+"=8")
	before := h.waitStalled(t)
	select {
	case err := <-h.exited:
		if err != nil {
			t.Fatalf("the wrapper exited with %v after reporting a verdict\n%s", err, h.log.String())
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("the idle watchdog never fired\n%s", h.log.String())
	}
	if m := h.manifest(t); !strings.Contains(m, `"failed"`) || !strings.Contains(m, wspec.ReasonIdle) {
		t.Fatalf("manifest after an idle harness: %s", m)
	}
	h.allGone(t, before)
	if _, err := os.Stat(filepath.Join(h.outs, wspec.ConformanceFile)); err == nil {
		t.Fatal("an idle harness produced a conformance artifact")
	}
}
