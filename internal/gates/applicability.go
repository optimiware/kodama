package gates

import (
	"reflect"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// ReasonWorkCancelled marks a gate whose work item was cancelled.
const ReasonWorkCancelled = "work_cancelled"

// EvidenceFacts is the current state of one artifact the subject binds.
type EvidenceFacts struct {
	Found  bool
	Usable bool
	Digest string
	Scope  domain.Scope
}

// Facts are the current facts a subject is compared with: the execution inputs
// recomputed from the run, the work item's current content revision, the
// current digest of every bound artifact and the deployment configuration. They
// are read in the transaction that decides or consumes, never from a request.
type Facts struct {
	Inputs          execution.GateInputs
	ContentRevision domain.ContentRevision
	// Cancelled is true when the work item was cancelled.
	Cancelled bool
	Evidence  map[domain.ArtifactID]EvidenceFacts
	// Deployment is the current deployment of a deployment-bearing gate.
	Deployment *DeploymentSnapshot
	// StepWaiting is true while the gate step is pending in a run that still
	// accepts progression.
	StepWaiting bool
}

// CheckCurrent compares the gate's subject with the current facts and returns a
// definitive *StaleError naming the first difference: content revision,
// candidate commit, definition, evidence, then deployment configuration. It
// ignores the gate's decision, deadline and invalidation (see Applicable).
func (g *Gate) CheckCurrent(f Facts) error {
	if !f.StepWaiting {
		return stale(ReasonStepNotWaiting)
	}
	if f.Cancelled {
		return stale(ReasonWorkCancelled)
	}
	s := g.Subject
	if f.ContentRevision != s.ContentRevision() {
		e := definitive(ReasonContentChanged)
		e.SupersedingRevision = int64(f.ContentRevision)
		return e
	}
	if f.Inputs.Candidate.Commit != s.CandidateCommit() {
		e := definitive(ReasonCandidateChanged)
		e.SupersedingCommit = f.Inputs.Candidate.Commit
		return e
	}
	if f.Inputs.DefinitionID != s.DefinitionID() || f.Inputs.WorkItemID != s.WorkItemID() {
		return definitive(ReasonDefinitionChanged)
	}
	if err := g.checkEvidence(f); err != nil {
		return err
	}
	id, config := s.Deployment()
	if f.Inputs.DeploymentID != id {
		return definitive(ReasonDeploymentChanged)
	}
	// The deployment must still be the project's own record of this candidate,
	// not merely one with the same identity and configuration digest.
	if id != "" && (f.Deployment == nil || f.Deployment.ID != id || f.Deployment.ConfigDigest != config ||
		f.Deployment.Scope != g.Scope || f.Deployment.Candidate != s.CandidateCommit()) {
		return definitive(ReasonDeploymentChanged)
	}
	return nil
}

func (g *Gate) checkEvidence(f Facts) error {
	// The step's evidence references must still resolve to the artifacts the
	// snapshot recorded.
	want := map[domain.ArtifactID]bool{}
	for _, r := range g.Required.Evidence {
		if r.Role == RoleEvidence {
			want[r.ID] = true
		}
	}
	got := map[domain.ArtifactID]bool{}
	for _, e := range f.Inputs.Evidence {
		got[e.ArtifactID] = true
	}
	if !reflect.DeepEqual(want, got) {
		return definitive(ReasonEvidenceChanged)
	}
	for _, ref := range g.Subject.Evidence() {
		e, ok := f.Evidence[ref.ID]
		if !ok || !e.Found || !e.Usable || e.Digest != ref.Digest || e.Scope != g.Scope {
			return definitive(ReasonEvidenceChanged)
		}
	}
	return nil
}

// Revalidate decides whether the approval may be consumed now: the gate record
// is applicable (approved, not invalidated, not expired, conformance evidence
// acceptable) and the current facts still describe its subject. Authority of
// the original reviewer is checked by the caller in the same transaction.
func (g *Gate) Revalidate(f Facts, now time.Time) error {
	if err := g.Applicable(now); err != nil {
		return err
	}
	return g.CheckCurrent(f)
}
