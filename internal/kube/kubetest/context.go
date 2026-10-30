package kubetest

import (
	"encoding/json"
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"

	"crypto/sha256"
	"encoding/hex"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
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

// ContextV2 derives a valid pinned run_shared context from the frozen scenario of
// runtime v2 (a source-backed reader with one earlier writer in its lineage),
// carrying the given identity, image and pinned policy.
func ContextV2(t testing.TB, s ContextSpec, policy execution.PolicyPin) runtimev2.ContextV2 {
	t.Helper()
	_, file, _, _ := goruntime.Caller(0)
	data, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "..", "contracts", "runtime", "v2", "fixtures", "scenarios.json"))
	if err != nil {
		t.Fatal(err)
	}
	var all []struct {
		Name  string `json:"name"`
		Steps []struct {
			Op           string          `json:"op"`
			ResourceKind string          `json:"resourceKind"`
			Body         json.RawMessage `json:"body"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(data, &all); err != nil {
		t.Fatal(err)
	}
	var c runtimev2.ContextV2
	for _, sc := range all {
		if sc.Name != "begin_clean_then_result_hash_is_the_v1_hash" {
			continue
		}
		var resp runtimev2.AdmitResponseV2
		if err := json.Unmarshal(sc.Steps[0].Body, &resp); err != nil {
			t.Fatal(err)
		}
		c = resp.Context
	}
	c.TenantID, c.ProjectID, c.RunID, c.StepID, c.AttemptID = s.Tenant, s.Project, s.Run, s.Step, s.Attempt
	c.ActionKey = execution.ActionKey(s.Run, s.Step)
	c.Policy = runtimev2.PolicyPin{ID: policy.ID, Digest: runtimev2.Digest(policy.Digest)}
	if s.Image != "" {
		c.Command.Image = s.Image
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("fixture v2 context: %v", err)
	}
	return c
}

// RecordShared is the committed attempt view execution persists for a run_shared
// attempt: the canonical v2 context, its digest and execution's v2 fingerprint.
func RecordShared(t testing.TB, c runtimev2.ContextV2, policy execution.PolicyPin) kube.AttemptRecord {
	t.Helper()
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := runtimev1.Canonicalize(raw)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canonical)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	name := execution.WorkloadName(c.TenantID, c.AttemptID)
	return kube.AttemptRecord{
		WorkspaceID: Workspace,
		Scope:       domain.Scope{TenantID: c.TenantID, ProjectID: c.ProjectID}, RunID: c.RunID, StepID: c.StepID, AttemptID: c.AttemptID, ActionKey: c.ActionKey,
		Status: execution.AttemptRunning, Live: true, JobName: name, SpecFingerprint: kube.SharedFingerprint(name, digest, c.Command.Image, c.ActionKey, policy.Digest),
		ContextDigest: digest, Context: canonical, Access: execution.Access(c.Workspace.Access), Launch: execution.LaunchUnknown,
	}
}
