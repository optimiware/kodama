package kubetest

import (
	"encoding/json"
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/kube"
)

// ContextSpec selects the identity and image of a fixture context.
type ContextSpec struct {
	Tenant  domain.TenantID
	Project domain.ProjectID
	Run     domain.RunID
	Step    string
	Attempt domain.AttemptID
	Image   string
	// Access is the workspace access; "" means none (no checkout).
	Access runtimev1.WorkspaceAccess
}

// Context derives a valid pinned context from the frozen happy-path fixture of
// the runtime contract.
func Context(t testing.TB, s ContextSpec) runtimev1.Context {
	t.Helper()
	_, file, _, _ := goruntime.Caller(0)
	path := filepath.Join(filepath.Dir(file), "..", "..", "..", "contracts", "runtime", "v1", "fixtures", "scenarios.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var all []struct {
		Name  string `json:"name"`
		Steps []struct {
			Body json.RawMessage `json:"body"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(data, &all); err != nil {
		t.Fatal(err)
	}
	var c runtimev1.Context
	for _, sc := range all {
		if sc.Name != "happy_path" {
			continue
		}
		var resp runtimev1.AdmitResponse
		if err := json.Unmarshal(sc.Steps[0].Body, &resp); err != nil {
			t.Fatal(err)
		}
		c = resp.Context
	}
	c.TenantID, c.ProjectID, c.RunID, c.StepID, c.AttemptID = s.Tenant, s.Project, s.Run, s.Step, s.Attempt
	c.ActionKey = execution.ActionKey(s.Run, s.Step)
	if s.Image != "" {
		c.Command.Image = s.Image
	}
	access := s.Access
	if access == "" {
		access = runtimev1.AccessNone
	}
	c.Workspace = runtimev1.Workspace{Access: access}
	if access != runtimev1.AccessNone {
		c.Workspace.Source = &runtimev1.Source{RepositoryURL: "https://git.example.invalid/t021/repo.git", Commit: "1111111111111111111111111111111111111111"}
		if access == runtimev1.AccessWrite {
			br, tip := "refs/heads/kodama/work", runtimev1.GitCommit("1111111111111111111111111111111111111111")
			c.Workspace.Source.Branch, c.Workspace.Source.ExpectedTip = &br, &tip
		}
	}
	c.Secrets, c.Outputs = []runtimev1.SecretBinding{}, []runtimev1.OutputDecl{}
	c.Inputs = map[string]runtimev1.Value{}
	if err := c.Validate(); err != nil {
		t.Fatalf("fixture context: %v", err)
	}
	return c
}

// Workspace is the workspace ID of fixture attempts that consume source.
const Workspace = "ws_t079"

// Record plans the context with the policy's planner and returns the committed
// attempt view execution would persist (a live, running, unconfirmed attempt).
func Record(t testing.TB, p kube.ExecutionPolicy, c runtimev1.Context) kube.AttemptRecord {
	t.Helper()
	plan, err := kube.NewPlanner(p).Plan(c.TenantID, c)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	workspace := ""
	if c.Workspace.Access != runtimev1.AccessNone {
		workspace = Workspace
	}
	return kube.AttemptRecord{
		WorkspaceID: workspace,
		Scope:       c.Scope(), RunID: c.RunID, StepID: c.StepID, AttemptID: c.AttemptID, ActionKey: c.ActionKey,
		Status: execution.AttemptRunning, Live: true, JobName: plan.WorkloadName, SpecFingerprint: plan.SpecFingerprint,
		ContextDigest: plan.ContextDigest, Context: plan.Context, Access: execution.Access(c.Workspace.Access),
		Launch: execution.LaunchUnknown,
	}
}

// Intent is the dispatch intent execution would hand out for the record.
func Intent(r kube.AttemptRecord) execution.AttemptSpec {
	return execution.AttemptSpec{
		Scope: r.Scope, RunID: r.RunID, StepID: r.StepID, AttemptID: r.AttemptID, Ordinal: 1, ActionKey: r.ActionKey,
		WorkloadName: r.JobName, SpecFingerprint: r.SpecFingerprint, ContextDigest: r.ContextDigest, Access: r.Access, Purpose: execution.PurposeWork,
	}
}
