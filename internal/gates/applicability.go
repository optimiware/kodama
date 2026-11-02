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
	Inputs execution.GateInputs
	// SourceSet is the source-set candidate recomputed from the run, nil for a
	// run without one.
	SourceSet       *SetInputs
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
// candidate (the commit, or the source set and the commit of each repository),
// definition, evidence, then deployment configuration. It ignores the gate's
// decision, deadline and invalidation (see Applicable).
func (g *Gate) CheckCurrent(f Facts) error {
	if !f.StepWaiting {
		return stale(ReasonStepNotWaiting)
	}
	return g.checkFacts(f)
}

// CheckSubjectCurrent is CheckCurrent for a gate whose step already completed:
// it compares the subject with the current facts without requiring the step to
// still be waiting. An integration approval is read after the gate step
// succeeded, and must still describe the run.
func (g *Gate) CheckSubjectCurrent(f Facts) error { return g.checkFacts(f) }

func (g *Gate) checkFacts(f Facts) error {
	if f.Cancelled {
		return stale(ReasonWorkCancelled)
	}
	s := g.Subject
	if f.ContentRevision != s.ContentRevision() {
		e := definitive(ReasonContentChanged)
		e.SupersedingRevision = int64(f.ContentRevision)
		return e
	}
	if err := g.checkCandidate(f); err != nil {
		return err
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
		f.Deployment.Scope != g.Scope || !deploymentRecordsCandidate(*f.Deployment, s)) {
		return definitive(ReasonDeploymentChanged)
	}
	return nil
}

// checkCandidate compares the candidate the subject names with the current one.
// A single-repository subject compares its commit; a source-set subject compares
// the set and the commit of every repository, and a run that no longer has a set
// (or a single-repository subject of a run that now has one) is a different
// candidate.
func (g *Gate) checkCandidate(f Facts) error {
	s := g.Subject
	set, isSet := s.SourceSet()
	if !isSet {
		if f.SourceSet != nil || f.Inputs.Candidate.Commit != s.CandidateCommit() {
			e := definitive(ReasonCandidateChanged)
			e.SupersedingCommit = f.Inputs.Candidate.Commit
			return e
		}
		return nil
	}
	if f.SourceSet == nil || !set.matches(*f.SourceSet) {
		return definitive(ReasonCandidateChanged)
	}
	return nil
}

// deploymentRecordsCandidate reports whether the deployment's own record names
// the candidate of the subject: the commit for a single-repository subject, the
// digest and the commit of every repository for a source-set subject (the
// record's own commit is then its primary source's and is not compared).
func deploymentRecordsCandidate(d DeploymentSnapshot, s Subject) bool {
	set, isSet := s.SourceSet()
	if !isSet {
		return d.SourceSet.IsZero() && d.Candidate == s.CandidateCommit()
	}
	if d.SourceSet.Digest != set.Digest {
		return false
	}
	got := d.SourceSet.Commits()
	if len(got) != len(set.Sources) {
		return false
	}
	for i, x := range set.Sources {
		if got[i].RepositoryID != x.RepositoryID || got[i].Commit != x.Commit {
			return false
		}
	}
	return true
}

// CheckBases compares the integration bases the subject names with the default
// branch tips observed now. A repository whose default branch moved (or is a
// different branch) makes the subject not applicable: the approval was given over
// another base, and integration must start again from a new candidate (GR-12).
// A subject without bases (a single-repository subject, or a source set the run
// only reads) is unaffected. A base with no observed tip is never assumed.
func (g *Gate) CheckBases(tips map[string]Tip) error {
	set, isSet := g.Subject.SourceSet()
	if !isSet {
		return nil
	}
	for _, x := range set.Sources {
		if x.Base == nil {
			continue
		}
		t, ok := tips[x.RepositoryID]
		if !ok {
			return stale(ReasonBaseMoved)
		}
		if t.Ref != x.Base.Ref || t.Commit != x.Base.Commit {
			return definitive(ReasonBaseMoved)
		}
	}
	return nil
}

// BaseRepositories lists the repositories whose integration base the subject
// names, in repository order.
func (g *Gate) BaseRepositories() []string {
	set, isSet := g.Subject.SourceSet()
	if !isSet {
		return nil
	}
	var out []string
	for _, x := range set.Sources {
		if x.Base != nil {
			out = append(out, x.RepositoryID)
		}
	}
	return out
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
