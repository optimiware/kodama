package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// SourceInfo is the workspace evidence a dispatch needs, gathered outside the
// transaction: where the source lives and, for a writer, the freshly observed
// accepted tip.
type SourceInfo struct {
	WorkspaceID   string
	RepositoryURL string
	// Branch is the workspace's work branch (writers only).
	Branch string
	// AcceptedTip is the workspace's accepted commit, freshly refreshed.
	AcceptedTip string
}

// WorkloadPlan is the stable identity and spec evidence of one attempt's
// workload. It is computed before any workload exists and persisted with the
// attempt; an existing workload is adopted only if it matches.
type WorkloadPlan struct {
	WorkloadName    string
	SpecFingerprint string
	ContextDigest   string
	Context         []byte
}

// Planner derives a WorkloadPlan from the attempt's pinned context. It is
// pure: it performs no I/O and may run inside the claim transaction. A runtime
// whose workload shape depends on installation policy supplies a planner that
// folds it into the fingerprint (the Kubernetes runtime requires its own); the
// default covers identity and the pinned context.
type Planner interface {
	Plan(tenant domain.TenantID, ctx runtimev1.Context) (WorkloadPlan, error)
}

// DefaultPlanner computes the workload name from tenant and attempt identity, the
// context digest from its canonical document and a fingerprint binding both to
// the image and action key.
type DefaultPlanner struct{}

// Plan implements Planner.
func (DefaultPlanner) Plan(tenant domain.TenantID, c runtimev1.Context) (WorkloadPlan, error) {
	if err := c.Validate(); err != nil {
		return WorkloadPlan{}, invalidf("pinned context: %v", err)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return WorkloadPlan{}, err
	}
	canonical, err := runtimev1.Canonicalize(raw)
	if err != nil {
		return WorkloadPlan{}, invalidf("pinned context: %v", err)
	}
	sum := sha256.Sum256(canonical)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	name := WorkloadName(tenant, c.AttemptID)
	fp := sha256.Sum256([]byte(fmt.Sprintf("kodama/job-spec/v1|%s|%s|%s|%s", name, digest, c.Command.Image, c.ActionKey)))
	return WorkloadPlan{WorkloadName: name, SpecFingerprint: "sha256:" + hex.EncodeToString(fp[:]), ContextDigest: digest, Context: canonical}, nil
}

// BuildContext assembles the immutable runtime context of an attempt from the
// run's pins, the step's resolved command and the selected source. Nothing in
// it comes from a workload.
func BuildContext(run *Run, step *StepRun, attempt domain.AttemptID, rs ResolvedStep, b WorkspaceBinding, src SourceInfo, lim Limit) (runtimev1.Context, error) {
	environment, err := run.Snapshot.environmentOf(step)
	if err != nil {
		return runtimev1.Context{}, err
	}
	timeout := time.Duration(step.Spec.TimeoutSeconds) * time.Second
	if timeout == 0 {
		timeout = lim.DefaultTimeout
	}
	if timeout > lim.MaxTimeout {
		return runtimev1.Context{}, Refusedf("the step timeout exceeds the configured limit")
	}
	c := runtimev1.Context{
		TenantID: run.Scope.TenantID, ProjectID: run.Scope.ProjectID, RunID: run.ID, StepID: step.ID,
		AttemptID: attempt, ActionKey: step.ActionKey,
		Definition:    runtimev1.Definition{ID: run.DefinitionID, Digest: runtimev1.Digest(run.DefinitionDigest)},
		Command:       runtimev1.Command{Image: rs.Image, Command: rs.Command, Args: rs.Args, Env: rs.Env},
		Inputs:        run.Snapshot.Values(),
		Outputs:       rs.Outputs,
		Secrets:       run.Snapshot.SecretsOf(step.ID),
		Environment:   environment,
		State:         run.Snapshot.StatesOf(step.ID),
		ArtifactFiles: run.Snapshot.ArtifactsOf(step.ID),
		Limits: runtimev1.Limits{
			TimeoutSeconds: int64(timeout / time.Second), MaxOutputBytes: lim.MaxOutputBytes,
			MaxOutputFiles: lim.MaxOutputFiles, MaxDiagnosticBytes: lim.MaxDiagnosticBytes,
		},
	}
	if c.Secrets == nil {
		c.Secrets = []runtimev1.SecretBinding{}
	}
	if run.Specification != nil {
		crit := run.Specification.AcceptanceCriteria
		if crit == nil {
			crit = []runtimev1.AcceptanceCriterion{}
		}
		c.WorkItem = &runtimev1.WorkItem{ID: run.WorkItemID, ContentRevision: run.ContentRevision, Spec: run.Specification.Spec, AcceptanceCriteria: crit}
	}
	switch b.Access {
	case AccessNone:
		c.Workspace = runtimev1.Workspace{Access: runtimev1.AccessNone}
	case AccessRead:
		c.Workspace = runtimev1.Workspace{Access: runtimev1.AccessRead, Source: &runtimev1.Source{
			RepositoryURL: src.RepositoryURL, Commit: runtimev1.GitCommit(b.Commit)}}
	case AccessWrite:
		br, tip := src.Branch, runtimev1.GitCommit(src.AcceptedTip)
		c.Workspace = runtimev1.Workspace{Access: runtimev1.AccessWrite, Source: &runtimev1.Source{
			RepositoryURL: src.RepositoryURL, Commit: runtimev1.GitCommit(b.Commit), Branch: &br, ExpectedTip: &tip}}
	default:
		return runtimev1.Context{}, invalidf("unknown workspace access")
	}
	return c, nil
}
