package workspace_test

import (
	"bytes"
	"context"
	"slices"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/workspace"
	wt "github.com/optimiware/kodama/internal/workspace/workspacetest"
)

var bg = context.Background()

// taskBranch is the task branch of a work item, as the contract names it.
func taskBranch(item domain.WorkItemID) string { return "refs/heads/kodama/work/" + string(item) }

// assoc returns the association of a repository, failing the test without one.
func assoc(t *testing.T, list []*workspace.RepositoryAssociation, repo workspace.RepositoryID) *workspace.RepositoryAssociation {
	t.Helper()
	a := workspace.FindAssociation(list, repo)
	if a == nil {
		t.Fatalf("no association for %s in %v", repo, list)
	}
	return a
}

// associations reloads the workspace's associations through the public read.
func associations(t *testing.T, e *wt.InternalEnv, w *workspace.Workspace) []*workspace.RepositoryAssociation {
	t.Helper()
	list, err := e.Svc.Repositories(bg, wt.Engineer, w.Scope, w.ID)
	if err != nil {
		t.Fatalf("repositories: %v", err)
	}
	return list
}

func reload(t *testing.T, e *wt.InternalEnv, w *workspace.Workspace) *workspace.Workspace {
	t.Helper()
	got, err := e.Svc.Get(bg, wt.Engineer, w.Scope, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func accept(e *wt.InternalEnv, w *workspace.Workspace, attempt domain.AttemptID, repo workspace.RepositoryID, ref string, commit workspace.Commit, push string) (workspace.CheckpointResult, error) {
	return e.Svc.AcceptCheckpoint(bg, wt.ExecutionService, workspace.CheckpointInput{
		Scope: w.Scope, AttemptID: attempt, RepositoryID: repo, Ref: ref, Commit: commit, PushID: push})
}

// pushed simulates the writer's push: the host's task branch moves to new and
// the push record of the receive is applied.
func pushed(e *wt.InternalEnv, w *workspace.Workspace, attempt domain.AttemptID, run domain.RunID, repo workspace.RepositoryID, old, new workspace.Commit) string {
	e.T.Helper()
	ref := taskBranch(w.WorkItemID)
	e.Hosts.SetRef(w.Scope.TenantID, string(repo), ref, new)
	return e.AppliedPush(w.Scope, repo, attempt, run, ref, old, new)
}

func settle(t *testing.T, e *wt.InternalEnv, w *workspace.Workspace, attempt domain.AttemptID, outcome workspace.WriterOutcome) {
	t.Helper()
	if _, err := e.Svc.SettleWriter(bg, workspace.SettleWriterInput{Scope: w.Scope, WorkspaceID: w.ID, AttemptID: attempt, Outcome: outcome}); err != nil {
		t.Fatalf("settle %s: %v", attempt, err)
	}
}

func auditHas(t *testing.T, e *wt.InternalEnv, w *workspace.Workspace, action string) int {
	t.Helper()
	actions, err := e.Store.AuditActions(bg, w.Scope.TenantID, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, a := range actions {
		if a == action {
			n++
		}
	}
	return n
}

func distinct(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}

func checkpointEventCount(t *testing.T, e *wt.InternalEnv, w *workspace.Workspace) int {
	t.Helper()
	return len(e.Recorder.Committed(t, w.Scope.TenantID, "workspace.checkpoint_created"))
}

func workspaceAttempt(id string) domain.AttemptID { return domain.AttemptID(id) }

func contains(list []string, v string) bool { return slices.Contains(list, v) }

func jsonHas(raw []byte, fragment string) bool { return bytes.Contains(raw, []byte(fragment)) }
