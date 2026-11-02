package execution

import (
	"fmt"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
)

// OriginKind is how a run came to exist (PX-23-PX-25, docs/contracts/runtime-v3.md).
// It is the run's immutable record of its start and is distinct from Purpose: a
// cleanup run is recognized by Purpose and has the implicit manual origin.
type OriginKind string

// Origin kinds. The values are the runtime v3 wire values.
const (
	// OriginWorkItem: started for an actionable work item; pins the work item's
	// specification, consumes business WIP and writes the item's task branch only
	// through the work item's writer reservation.
	OriginWorkItem OriginKind = OriginKind(runtimev3.OriginWorkItem)
	// OriginPush: started by one applied push record and ref (PX-24). It has no
	// work item, consumes no WIP and pins the update's new commit.
	OriginPush OriginKind = OriginKind(runtimev3.OriginPush)
	// OriginManual: started by a person or an API call without a work item.
	OriginManual OriginKind = OriginKind(runtimev3.OriginManual)
	// OriginProvision: a provisioning run (PX-25): admitted before its repository
	// exists, its Git authority is the seed capability of the repositories it
	// creates in its own project.
	OriginProvision OriginKind = OriginKind(runtimev3.OriginProvision)
)

// Origin is the pinned origin of a source-set run. Exactly the members of its
// kind are set (runtimev3.Origin.Validate is the single rule).
type Origin struct {
	Kind         OriginKind        `json:"kind"`
	WorkItemID   domain.WorkItemID `json:"workItemId,omitempty"`
	PushID       string            `json:"pushId,omitempty"`
	RepositoryID string            `json:"repositoryId,omitempty"`
	Ref          string            `json:"ref,omitempty"`
}

// IsZero reports the origin of a run admitted before origins existed.
func (o Origin) IsZero() bool { return o == Origin{} }

// V3 is the runtime v3 wire form of the origin.
func (o Origin) V3() runtimev3.Origin {
	out := runtimev3.Origin{Kind: runtimev3.OriginKind(o.Kind)}
	if o.WorkItemID != "" {
		id := o.WorkItemID
		out.WorkItemID = &id
	}
	if o.PushID != "" {
		p, r, ref := o.PushID, o.RepositoryID, o.Ref
		out.PushID, out.RepositoryID, out.Ref = &p, &r, &ref
	}
	return out
}

// Validate checks the origin's shape: the members its kind requires are present
// and valid and no other is set.
func (o Origin) Validate() error {
	if err := o.V3().Validate(); err != nil {
		return invalidf("%v", err)
	}
	if (o.Kind != OriginWorkItem && o.WorkItemID != "") || (o.Kind != OriginPush && (o.PushID != "" || o.RepositoryID != "" || o.Ref != "")) {
		return invalidf("the origin carries members of another kind")
	}
	return nil
}

// Branch returns the branch a write source updates in a run of this origin: the
// work item's task branch, or a branch inside the run's own namespace
// (refs/heads/kodama/runs/<runId>/work). Neither is the default branch, a tag or
// another run's namespace.
func (o Origin) Branch(run domain.RunID) (string, error) {
	if o.Kind == OriginWorkItem {
		return gitv1.TaskBranch(string(o.WorkItemID))
	}
	return gitv1.RunBranch(string(run), RunBranchLabel)
}

// RunBranchLabel is the label of the branch a write source of a run without a
// work item updates.
const RunBranchLabel = "work"

// OriginOf is the effective origin of a run: the pinned one, or the implicit
// origin of a run admitted before origins existed (a work run started for its
// work item; a deployment cleanup run is none of the four kinds and is a manual
// run, docs/contracts/runtime-v3.md).
func OriginOf(r *Run) Origin {
	if !r.Origin.IsZero() {
		return r.Origin
	}
	if r.Purpose == PurposeCleanup {
		return Origin{Kind: OriginManual}
	}
	return Origin{Kind: OriginWorkItem, WorkItemID: r.WorkItemID}
}

func (o Origin) String() string {
	if o.Kind == OriginPush {
		return fmt.Sprintf("push:%s:%s", o.PushID, o.Ref)
	}
	return string(o.Kind)
}
