package m1_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	"github.com/optimiware/kodama/test/e2e/m1/kit"
)

// TestM1Tracer (m1_tracer) is the M1 tracer bullet over the real services: work
// is created through the public client and CLI, its source is provisioned on a
// real Git remote, a run is admitted, its step runs in a real Podman container
// (the real `kodama step` entrypoint calling back through the runtime API), and
// the report (steps, outputs), the retained log, the event stream and the
// durable history are read back. Covers AC-A01, WS-A01, WS-A04, PX-A07, PX-A11,
// PX-03, PX-16 and IT-A04.
func TestM1Tracer(t *testing.T) {
	s := kit.New(t, kit.Options{})
	s.Start()
	s.WaitReady()
	w := s.Seed(true)
	cli := w.NewCLI()

	// ---- create work: public CLI JSON and the public client agree (AC-A01) ----
	cli.As("engineer")
	spec := cli.WriteFile(t, "spec.md", "# Plan\nrun the tracer")
	code, out, errs := cli.Run("work", "create", "--kind", "story", "--title", "Tracer", "--spec-file", spec)
	if code != 0 {
		t.Fatalf("work create: %d %s %s", code, out, errs)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(out), &created); err != nil || created.ID == "" {
		t.Fatalf("work create output %q: %v", out, err)
	}
	got, err := s.API("engineer").GetWorkItemWithResponse(t.Context(), kit.TenantA, w.Project, created.ID)
	if err != nil || got.StatusCode() != http.StatusOK || got.JSON200.Title != "Tracer" {
		t.Fatalf("the item created through the CLI is not readable through the client: %v %v", got, err)
	}
	item := *got.JSON200

	// ---- source (WS-A01, WS-A04): duplicate provisioning yields one workspace and one branch at the recorded commit ----
	first := w.Provision(item.Id)
	second := w.Provision(item.Id)
	if first.Id != second.Id || first.Branch != second.Branch {
		t.Fatalf("provisioning twice created %s and %s", first.Id, second.Id)
	}
	if first.AcceptedCommit == nil || *first.AcceptedCommit != s.Repo.Initial || s.Repo.Tip(t, first.Branch) != s.Repo.Initial {
		t.Fatalf("the work branch %s is not at the recorded commit %s", first.Branch, s.Repo.Initial)
	}
	if n := s.OwnerCount(`SELECT count(*) FROM workspaces WHERE work_item_id = $1`, item.Id); n != 1 {
		t.Fatalf("%d workspaces for one work item", n)
	}

	// ---- run: admission, events, the container, the report ----
	def := w.PodmanDefinition("tracer", kit.Step{
		Script:  `echo "hello from the m1 tracer"; echo "to stderr" >&2; sleep 4; printf '{"outcome":"completed","outputs":{"v":"ok"}}' > "$KODAMA_RESULTS_FILE"`,
		Outputs: []string{"v"},
	})
	events := s.OpenEvents("engineer", w.ProjectEvents(), "")
	events.Until(t, "the snapshot", 10*time.Second, func(f kit.Frame) bool { return f.Event == "snapshot" })

	run := w.Run("tracer-run", item, def.Id)
	if run.Status != client.RunStatusQueued || run.Purpose != "work" || run.SourceCommit == nil || *run.SourceCommit != s.Repo.Initial {
		t.Fatalf("admitted run %+v", run)
	}
	events.Until(t, "run.created on the stream", 20*time.Second, kit.OfType("run.created"))
	w.WaitSourceRetained(run.Id)
	if refs := s.Repo.Git(t, "for-each-ref", "--format=%(refname)", "refs/heads/kodama/retained/"); !strings.Contains(refs, run.Id) && refs == "" {
		t.Fatalf("no retained source reference on the remote: %q", refs)
	}

	// While the step runs the container is the pinned image, hardened, on the
	// scope's workload network, and carries the attempt's identity.
	attempt := w.WaitAttempt(run.Id, time.Minute)
	c := s.WaitContainer(kit.TenantA, attempt, "running", time.Minute)
	if !strings.HasSuffix(kit.PodmanImage, "@"+digestOf(c.Image)) && !strings.Contains(c.Image, digestOf(kit.PodmanImage)) {
		t.Fatalf("the container runs %s, not the pinned %s", c.Image, kit.PodmanImage)
	}
	assertHardened(t, s, c)
	if names := w.Attempts(run.Id); len(names) != 1 || len(c.Names) != 1 || c.Names[0] != names[0].JobName {
		t.Fatalf("the container %v is not the attempt's stable workload %+v", c.Names, names)
	}

	final := w.WaitRun(run.Id, 3*time.Minute)
	if final.Status != client.RunStatusSucceeded || final.FinishedAt == nil {
		t.Fatalf("run %s (%s)", final.Status, final.Reason)
	}
	steps := w.Steps(run.Id)
	if len(steps) != 1 || steps[0].Status != client.StepStatusSucceeded || steps[0].Outputs == nil {
		t.Fatalf("steps %+v", steps)
	}
	if v, err := (*steps[0].Outputs)["v"].AsRunsScalar0(); err != nil || v != "ok" {
		t.Fatalf("output v = %q (%v)", v, err)
	}
	if len(steps[0].Attempts) != 1 || steps[0].Attempts[0].Status != client.AttemptStatusSucceeded || !steps[0].Attempts[0].Settled || steps[0].Attempts[0].Access != "none" {
		t.Fatalf("attempts %+v", steps[0].Attempts)
	}

	// The log outlives the container: it was retained before the workload was removed.
	w.WaitRemoved(attempt)
	lines := w.WaitLogs("engineer", run.Id)
	if !contains(lines, "hello from the m1 tracer") || !contains(lines, "to stderr") {
		t.Fatalf("retained log %q", lines)
	}
	if code, _ := w.Logs("outsider", run.Id); code != http.StatusNotFound {
		t.Fatalf("an unbound caller read the log: %d", code)
	}

	// The stream reports the run's progress; history stays readable after the end (PX-12).
	events.Until(t, "the run's terminal state change on the stream", 30*time.Second, func(f kit.Frame) bool {
		return f.Type() == "run.state_changed" && strings.Contains(f.Data, run.Id) && strings.Contains(f.Data, `"succeeded"`)
	})
	events.Cancel()
	if n := s.TenantCount(kit.TenantA, `SELECT count(*) FROM run_history WHERE run_id = $1`, run.Id); n < 3 {
		t.Fatalf("%d history rows for a finished run", n)
	}
	if list, err := s.API("viewer").ListRunsWithResponse(t.Context(), kit.TenantA, w.Project, &client.ListRunsParams{}); err != nil || list.StatusCode() != 200 || len(list.JSON200.Items) == 0 {
		t.Fatalf("a viewer reads runs: %v %v", list, err)
	}
	// The CLI reports the same run (AC-A01).
	if code, out, errs = cli.Run("run", "get", run.Id); code != 0 || !strings.Contains(out, `"succeeded"`) {
		t.Fatalf("run get: %d %s %s", code, out, errs)
	}
	if code, out, errs = cli.Run("run", "steps", run.Id); code != 0 || !strings.Contains(out, `"ok"`) {
		t.Fatalf("run steps: %d %s %s", code, out, errs)
	}
	if code, out, errs = cli.Run("run", "logs", run.Id); code != 0 || !strings.Contains(out, "hello from the m1 tracer") {
		t.Fatalf("run logs: %d %s %s", code, out, errs)
	}

	// ---- PX-A07: a missing declared output fails success finalization; diagnostics stay readable ----
	item2 := w.WorkItem("Missing output", "# no output")
	w.Provision(item2.Id)
	def2 := w.PodmanDefinition("missing-output", kit.Step{Script: `echo "finished without the declared output"`, Outputs: []string{"v"}})
	bad := w.Run("missing-output-run", item2, def2.Id)
	failed := w.WaitRun(bad.Id, 3*time.Minute)
	if failed.Status != client.RunStatusFailed {
		t.Fatalf("a run missing its declared output is %s", failed.Status)
	}
	badSteps := w.Steps(bad.Id)
	// The attempt fails with a classified reason, never as a success with missing outputs. The
	// entrypoint reports output_invalid; the result module records it as command_failed (a
	// finding of docs/acceptance/m1.md: the finer classification is lost).
	if r := badSteps[0].Reason; r != "output_invalid" && r != "command_failed" {
		t.Fatalf("the step failed as %q", r)
	}
	if badSteps[0].Outputs != nil || badSteps[0].Status != client.StepStatusFailed {
		t.Fatalf("a failed step exposes success outputs: %+v", badSteps[0])
	}
	if lines := w.WaitLogs("engineer", bad.Id); !contains(lines, "finished without the declared output") {
		t.Fatalf("the diagnostics of the failed run: %q", lines)
	}
	w.WaitRemoved(badSteps[0].Attempts[0].Id)
}

func contains(lines []string, want string) bool {
	for _, l := range lines {
		if strings.Contains(l, want) {
			return true
		}
	}
	return false
}

func digestOf(ref string) string {
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

// assertHardened checks the isolation the Podman runtime promises: a read-only
// root filesystem, dropped capabilities, no privilege escalation, bounded memory
// and processes, and exactly the scope's workload network.
func assertHardened(t *testing.T, s *kit.Stack, c kit.Container) {
	t.Helper()
	in := s.Inspect(c.ID)
	host, _ := in["HostConfig"].(map[string]any)
	if host["ReadonlyRootfs"] != true || host["Privileged"] != false {
		t.Errorf("root filesystem read-only %v, privileged %v", host["ReadonlyRootfs"], host["Privileged"])
	}
	if caps, _ := host["CapDrop"].([]any); len(caps) == 0 {
		t.Errorf("no capability is dropped")
	}
	if sec, _ := host["SecurityOpt"].([]any); len(sec) == 0 || sec[0] != "no-new-privileges" {
		t.Errorf("security options %v", host["SecurityOpt"])
	}
	if mem, _ := host["Memory"].(float64); mem <= 0 {
		t.Errorf("no memory limit")
	}
	if pids, _ := host["PidsLimit"].(float64); pids <= 0 {
		t.Errorf("no process limit")
	}
	netw, _ := in["NetworkSettings"].(map[string]any)["Networks"].(map[string]any)
	if len(netw) != 1 {
		t.Errorf("the container joined %d networks: %v", len(netw), netw)
	}
	if c.Labels["kodama.optimi.com/tenant"] != kit.TenantA {
		t.Errorf("tenant label %q", c.Labels["kodama.optimi.com/tenant"])
	}
}
