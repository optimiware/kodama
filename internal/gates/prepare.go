package gates

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/pipeline"
)

// GateIDFor derives the stable identity of the gate of a run step. Creation is
// idempotent under the run/step identity, so a crash and retry yields the same
// gate and the same evidence pins.
func GateIDFor(scope domain.Scope, run domain.RunID, step string) ID {
	sum := sha256.Sum256([]byte("kodama/gate/" + string(scope.TenantID) + "/" + string(scope.ProjectID) + "/" + string(run) + "/" + step))
	return ID("gate_" + hex.EncodeToString(sum[:16]))
}

// RequestForStep builds, verifies and persists the gate of a ready gate step.
// It is idempotent: an existing gate of the step is returned unchanged. All
// verification (evidence ownership/provenance/digest, conformance, candidate
// and deployment) and all pinning happen outside any SQL transaction; the
// transaction that creates the gate and binds it to its step recomputes the
// execution inputs under the run lock and refuses a mismatch.
//
// An outage anywhere returns ErrUnavailable and leaves no gate. Evidence that
// cannot be verified returns ErrEvidence and leaves no gate. A candidate's
// conformance report that is missing or blocked still produces a gate whose
// approval is refused, so the evidence is preserved for review.
func (s *Service) RequestForStep(ctx context.Context, scope domain.Scope, run domain.RunID, step string) (*Gate, bool, error) {
	if err := scope.Validate(); err != nil {
		return nil, false, ErrNotFound
	}
	if g, err := s.d.Store.ByStep(ctx, scope, run, step); err == nil {
		return g, false, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, false, err
	}
	agg, err := s.d.Runs.Get(ctx, scope, run)
	if err != nil {
		return nil, false, mapRunError(err)
	}
	st := agg.Step(step)
	if st == nil {
		return nil, false, ErrNotFound
	}
	if !agg.GateReady(st) {
		return nil, false, fmt.Errorf("%w: gate step %s is not waiting for a gate", ErrInvalidTransition, step)
	}
	in, set, err := ResolveInputs(agg, step)
	if err != nil {
		return nil, false, mapRunError(err)
	}
	cand := candidate{in: in, set: set}
	var tips map[string]Tip
	if set != nil {
		// The commits of a source-set candidate are the run's own immutable
		// pins and accepted results (verified at admission and acceptance); the
		// integration bases are observed now, outside any transaction.
		if tips, err = s.tips(ctx, scope, set.Writes()); err != nil {
			return nil, false, err
		}
	} else {
		src, err := s.d.Candidates.Verify(ctx, scope, in.WorkItemID, in.Candidate.Commit)
		if err != nil {
			return nil, false, mapPeer(err)
		}
		if src.Repository == "" {
			return nil, false, fmt.Errorf("%w: the candidate was verified without a repository", ErrEvidence)
		}
		cand.src = src
	}
	id := GateIDFor(scope, run, step)

	// Resolved evidence is normalized once: one entry per (artifact, role), the
	// first expression naming it kept, so persistence cannot collide on the
	// reference key. Two digests for one artifact are a conflict, not a choice.
	required := Required{Evidence: []RequiredEvidence{}}
	var refs []EvidenceRef
	type refKey struct {
		id   domain.ArtifactID
		role EvidenceRole
	}
	seen := map[refKey]string{}
	digests := map[domain.ArtifactID]string{}
	add := func(r RequiredEvidence) error {
		if d, ok := digests[r.ID]; ok && d != r.Digest {
			return fmt.Errorf("%w: the artifact %s resolved to two different digests", ErrEvidence, r.ID)
		}
		digests[r.ID] = r.Digest
		k := refKey{r.ID, r.Role}
		if _, ok := seen[k]; ok {
			return nil
		}
		seen[k] = r.Digest
		required.Evidence = append(required.Evidence, r)
		refs = append(refs, EvidenceRef{ID: r.ID, Digest: r.Digest})
		return nil
	}
	// Every expression is validated before any is normalized away.
	for _, e := range in.Evidence {
		meta, err := s.d.Evidence.Describe(ctx, scope, e.ArtifactID)
		if err != nil {
			return nil, false, mapEvidenceError(err)
		}
		if meta.ID != e.ArtifactID {
			return nil, false, fmt.Errorf("%w: the evidence store answered for another artifact", ErrEvidence)
		}
		if err := verifyEvidenceFor(agg, cand, meta, e.Producer); err != nil {
			return nil, false, err
		}
		if err := add(RequiredEvidence{Role: RoleEvidence, ID: meta.ID, Digest: meta.Digest, Expression: e.Expression, Producer: e.Producer}); err != nil {
			return nil, false, err
		}
	}

	var deployment *DeploymentSnapshot
	if in.DeploymentID != "" {
		if s.d.Deployments == nil {
			return nil, false, fmt.Errorf("%w: a deployment adapter is required for a deployment gate", ErrNotConfigured)
		}
		snap, err := s.d.Deployments.Snapshot(ctx, scope, in.DeploymentID)
		if err != nil {
			return nil, false, mapPeer(err)
		}
		if err := verifyDeploymentFor(snap, scope, in, set); err != nil {
			return nil, false, err
		}
		deployment = &snap
	}

	if in.Candidate.ProducerKind == pipeline.KindAgent {
		snap, extra, err := s.conformance(ctx, scope, agg, cand)
		if err != nil {
			return nil, false, err
		}
		required.Conformance = snap
		for _, r := range extra {
			if err := add(r); err != nil {
				return nil, false, err
			}
		}
	}

	sin := SubjectInput{
		Scope: scope, WorkItemID: in.WorkItemID, ContentRevision: in.ContentRevision, CandidateCommit: in.Candidate.Commit,
		DefinitionID: in.DefinitionID, Evidence: refs,
	}
	if set != nil {
		sources := make([]SubjectSource, len(set.Sources))
		for i, x := range set.Sources {
			sources[i] = SubjectSource{RepositoryID: x.RepositoryID, Commit: x.Commit}
			if x.Write {
				t := tips[x.RepositoryID]
				sources[i].Base = &Base{Ref: t.Ref, Commit: t.Commit}
			}
		}
		subjectSet, err := NewSourceSetSubject(set.Digest, sources)
		if err != nil {
			return nil, false, err
		}
		sin.SourceSet = &subjectSet
	}
	if deployment != nil {
		sin.DeploymentID, sin.DeploymentConfigDigest = deployment.ID, deployment.ConfigDigest
	}
	subject, err := NewSubject(sin)
	if err != nil {
		return nil, false, err
	}
	sort.SliceStable(required.Evidence, func(i, j int) bool {
		a, b := required.Evidence[i], required.Evidence[j]
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return a.Role < b.Role
	})
	now := s.now()
	g, err := Request(RequestInput{ID: id, Scope: scope, RunID: run, StepID: step, Subject: subject, Required: required, Now: now, Deadline: now.Add(s.o.GateTTL)})
	if err != nil {
		return nil, false, err
	}
	for _, r := range required.Evidence {
		if err := s.d.Pins.Pin(ctx, scope, r.ID, id); err != nil {
			return nil, false, mapEvidenceError(err)
		}
	}
	out, created, err := s.d.Store.Request(ctx, &RequestPlan{Gate: g, Inputs: in, SourceSet: set, Actor: System})
	if err != nil {
		return nil, false, err
	}
	return out, created, nil
}

// AcceptedAttempt returns the one successful attempt of a succeeded step. A
// step without one, or with several, is ambiguous and yields an error: the
// accepted attempt is never guessed.
func AcceptedAttempt(agg *execution.Aggregate, stepID string) (*execution.Attempt, error) {
	step := agg.Step(stepID)
	if step == nil || step.Status != execution.StepSucceeded {
		return nil, fmt.Errorf("the step %q did not succeed", stepID)
	}
	var accepted *execution.Attempt
	for _, at := range agg.AttemptsOf(stepID) {
		if at.Status != execution.AttemptSucceeded {
			continue
		}
		if accepted != nil {
			return nil, fmt.Errorf("the step %q has several successful attempts", stepID)
		}
		accepted = at
	}
	if accepted == nil {
		return nil, fmt.Errorf("the step %q has no successful attempt", stepID)
	}
	return accepted, nil
}

// candidate is the candidate under review as the request resolved it: the
// execution inputs and either the commit's verified repository (a single
// source) or the source-set candidate.
type candidate struct {
	in  execution.GateInputs
	src VerifiedSource
	set *SetInputs
}

// verify connects an artifact to the candidate through its trusted provenance.
func (c candidate) verify(agg *execution.Aggregate, meta EvidenceMeta) error {
	if c.set != nil {
		return verifySetProvenance(agg, c.in, *c.set, meta)
	}
	return verifyProvenance(agg, c.in, c.src, meta)
}

// verifySetProvenance connects an artifact to a source-set candidate. The
// artifact's provenance names the digest of the run's pinned set and the commit
// of every repository its producing attempt worked from. It is compatible when
// the digest is the candidate's and the commits are exactly the candidate's, or
// the artifact is the writer exception: produced by the successful attempt of
// the candidate's own producer step, whose accepted result published the
// candidate's commits from exactly those input commits. A provenance of another
// set, of a single source or of fewer or other repositories never connects.
func verifySetProvenance(agg *execution.Aggregate, in execution.GateInputs, set SetInputs, meta EvidenceMeta) error {
	bad := func(why string) error { return fmt.Errorf("%w: %s", ErrEvidence, why) }
	if meta.SourceSet.IsZero() {
		return bad("the artifact has no source set provenance connecting it to the candidate")
	}
	if meta.SourceSet.Digest != set.Digest {
		return bad("the artifact was produced from a different source set than the candidate")
	}
	worked := map[string]string{}
	for _, c := range meta.SourceSet.Commits() {
		worked[c.RepositoryID] = c.Commit
	}
	want := set.Commits()
	if sameCommits(worked, want) {
		return nil
	}
	if in.Candidate.Producer == "" || meta.RunID != in.RunID || meta.StepID != in.Candidate.Producer {
		return bad("the artifact was produced from different source commits than the candidate")
	}
	accepted, err := AcceptedAttempt(agg, meta.StepID)
	if err != nil || accepted.ID != meta.Attempt || len(accepted.Commits) == 0 {
		return bad("the artifact was produced from different source commits than the candidate")
	}
	heads := make(map[string]string, len(worked))
	for k, v := range worked {
		heads[k] = v
	}
	applyPublished(agg.Run.Sources.Entries, heads, accepted.Commits)
	if !sameCommits(heads, want) {
		return bad("the artifact was produced from different source commits than the candidate")
	}
	return nil
}

func sameCommits(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// verifyProvenance connects an artifact to the candidate under review through
// trusted source provenance. The artifact's provenance names the repository
// and the commit its producing attempt worked from. It is compatible when
// both are present, the repository is the one the candidate was verified in,
// and either the commit is the candidate or the artifact is the writer
// exception: produced by the successful attempt of the candidate's own
// producer step, whose accepted result published the candidate from exactly
// that input commit. Absent provenance never counts as compatible.
func verifyProvenance(agg *execution.Aggregate, in execution.GateInputs, src VerifiedSource, meta EvidenceMeta) error {
	bad := func(why string) error { return fmt.Errorf("%w: %s", ErrEvidence, why) }
	if meta.Repository == "" || meta.Commit == "" {
		return bad("the artifact has no source provenance connecting it to the candidate")
	}
	if meta.Repository != src.Repository {
		return bad("the artifact was produced from a different repository than the candidate")
	}
	if meta.Commit == in.Candidate.Commit {
		return nil
	}
	if in.Candidate.Producer == "" || meta.RunID != in.RunID || meta.StepID != in.Candidate.Producer {
		return bad("the artifact was produced from a different source commit than the candidate")
	}
	accepted, err := AcceptedAttempt(agg, meta.StepID)
	if err != nil || accepted.ID != meta.Attempt || accepted.InputCommit != meta.Commit ||
		accepted.Result == nil || accepted.Result.SourceCommit == nil || string(*accepted.Result.SourceCommit) != in.Candidate.Commit {
		return bad("the artifact was produced from a different source commit than the candidate")
	}
	return nil
}

// verifyEvidence checks one artifact bound into the subject: shared ownership,
// usable verified content, the named producer step of this run, and source
// provenance consistent with the candidate. An artifact that fails is wrong
// evidence; none is silently dropped.
func verifyEvidence(agg *execution.Aggregate, in execution.GateInputs, src VerifiedSource, meta EvidenceMeta, producer string) error {
	return verifyEvidenceFor(agg, candidate{in: in, src: src}, meta, producer)
}

// verifyEvidenceFor is verifyEvidence over a single-source or source-set
// candidate.
func verifyEvidenceFor(agg *execution.Aggregate, cand candidate, meta EvidenceMeta, producer string) error {
	in := cand.in
	bad := func(why string) error { return fmt.Errorf("%w: %s", ErrEvidence, why) }
	if meta.Scope != in.Scope {
		return bad("the artifact belongs to another project")
	}
	if !meta.Usable {
		return bad("the artifact is not usable")
	}
	if err := validDigest("evidence digest", meta.Digest); err != nil {
		return bad("the artifact has no verified digest")
	}
	if producer != "" {
		if meta.RunID != in.RunID || meta.StepID != producer {
			return bad("the artifact was not produced by the referenced step of this run")
		}
	}
	if meta.RunID == in.RunID && meta.StepID != "" {
		accepted, err := AcceptedAttempt(agg, meta.StepID)
		if err != nil {
			return bad("the producer step did not succeed")
		}
		if accepted.ID != meta.Attempt {
			return bad("the artifact was not produced by the successful attempt of its step")
		}
	}
	return cand.verify(agg, meta)
}

// verifyDeployment checks that the deployment the adapter answered is the one
// the step names, belongs to the gate's project and records the candidate
// under review: its commit, or, for a source-set candidate, the digest and the
// commit of every repository.
func verifyDeployment(snap DeploymentSnapshot, scope domain.Scope, in execution.GateInputs) error {
	return verifyDeploymentFor(snap, scope, in, nil)
}

// verifyDeploymentFor is verifyDeployment over a single-source or source-set
// candidate.
func verifyDeploymentFor(snap DeploymentSnapshot, scope domain.Scope, in execution.GateInputs, set *SetInputs) error {
	switch {
	case snap.ID != in.DeploymentID:
		return fmt.Errorf("%w: the deployment adapter answered for another deployment", ErrEvidence)
	case snap.Scope != scope:
		return fmt.Errorf("%w: the deployment belongs to another project", ErrEvidence)
	case set == nil && (!snap.SourceSet.IsZero() || snap.Candidate != in.Candidate.Commit):
		return fmt.Errorf("%w: the deployment records a different candidate than the one under review", ErrEvidence)
	case set != nil:
		got := map[string]string{}
		for _, c := range snap.SourceSet.Commits() {
			got[c.RepositoryID] = c.Commit
		}
		if snap.SourceSet.Digest != set.Digest || !sameCommits(got, set.Commits()) {
			return fmt.Errorf("%w: the deployment records a different candidate than the one under review", ErrEvidence)
		}
	}
	return nil
}

// conformance validates the candidate's design conformance report. It returns
// an error only for an outage or a missing adapter (no gate is created then).
// Every other failure is recorded in a blocked snapshot that refuses approval.
func (s *Service) conformance(ctx context.Context, scope domain.Scope, agg *execution.Aggregate, cand candidate) (*ConformanceSnapshot, []RequiredEvidence, error) {
	in := cand.in
	snap := &ConformanceSnapshot{Status: ConformanceBlocked, ProducerStep: in.Candidate.Producer, RequiredChecks: []string{}}
	block := func(code string) (*ConformanceSnapshot, []RequiredEvidence, error) {
		snap.Code = code
		return snap, nil, nil
	}
	if s.d.Conformance == nil {
		return nil, nil, fmt.Errorf("%w: a conformance adapter is required for an agent-produced candidate", ErrNotConfigured)
	}
	if cand.set != nil {
		// A conformance report names one candidate commit. A source set with exactly
		// one writable repository is reviewed at that repository's commit; one with
		// several has no single commit to bind, and its approval is refused (the
		// evidence is preserved for review) until the report identifies the set.
		writes := cand.set.Writes()
		if len(writes) != 1 {
			return block(CodeConformanceSourceSet)
		}
		in.Candidate.Commit = cand.set.Commits()[writes[0]]
		cand.in = in
	}
	exp, err := s.d.Conformance.Expected(ctx, agg, in.Candidate)
	if err != nil {
		if errors.Is(err, ErrUnavailable) || errors.Is(err, ErrNotConfigured) {
			return nil, nil, err
		}
		return block(CodeConformanceMissing)
	}
	// The expected baseline is trusted pinned data, but a malformed one cannot
	// define the required checks: it blocks like an absent one.
	if exp.Baseline == nil || exp.Baseline.Validate() != nil {
		return block(CodeConformanceBaselineMissing)
	}
	snap.BaselineDigest = string(exp.Baseline.Digest)
	snap.RequiredChecks = append([]string{}, exp.Baseline.RequiredChecks...)
	if exp.ReportID == "" {
		return block(CodeConformanceMissing)
	}
	var extra []RequiredEvidence
	meta, err := s.d.Evidence.Describe(ctx, scope, exp.ReportID)
	switch {
	case errors.Is(err, ErrNotFound):
		return block(CodeConformanceMissing)
	case err != nil:
		return nil, nil, mapEvidenceError(err)
	}
	// The store must answer for the report that was asked for: an answer for
	// another artifact is never bound or pinned as the requested evidence.
	if meta.ID != exp.ReportID || !meta.Usable || meta.Scope != scope || meta.RunID != in.RunID || meta.StepID != in.Candidate.Producer || meta.Attempt != exp.ProducerAttempt ||
		cand.verify(agg, meta) != nil {
		return block(CodeConformanceEvidenceInvalid)
	}
	extra = append(extra, RequiredEvidence{Role: RoleReport, ID: meta.ID, Digest: meta.Digest, Producer: in.Candidate.Producer})
	snap.ReportID, snap.ReportDigest = meta.ID, meta.Digest
	blocked := func(code string) (*ConformanceSnapshot, []RequiredEvidence, error) {
		snap.Code = code
		return snap, extra, nil
	}
	data, err := s.d.Evidence.Read(ctx, scope, meta, s.o.MaxReportBytes)
	switch {
	case errors.Is(err, ErrEvidence):
		return blocked(CodeConformanceEvidenceInvalid)
	case err != nil:
		return nil, nil, mapEvidenceError(err)
	}
	if exp.PublishedCommit != in.Candidate.Commit {
		return blocked(string(runtimev1.CodeConformanceCandidateMismatch))
	}
	report, err := runtimev1.DecodeRequest[runtimev1.ConformanceReport](data)
	if err != nil {
		return blocked(CodeConformanceMalformed)
	}
	supportCode := ""
	cited := map[domain.ArtifactID]bool{}
	for _, c := range report.Checks {
		for _, id := range c.EvidenceArtifactIDs {
			if cited[id] || id == meta.ID {
				continue
			}
			cited[id] = true
			m, err := s.d.Evidence.Describe(ctx, scope, id)
			switch {
			case errors.Is(err, ErrNotFound):
				supportCode = CodeConformanceEvidenceInvalid
				continue
			case err != nil:
				return nil, nil, mapEvidenceError(err)
			}
			if m.ID != id || !m.Usable || m.Scope != scope || m.RunID != in.RunID || m.StepID != in.Candidate.Producer || m.Attempt != exp.ProducerAttempt ||
				cand.verify(agg, m) != nil {
				supportCode = CodeConformanceEvidenceInvalid
				continue
			}
			extra = append(extra, RequiredEvidence{Role: RoleSupport, ID: m.ID, Digest: m.Digest, Producer: in.Candidate.Producer})
		}
	}
	if err := runtimev1.CheckConformance(report, *exp.Baseline, runtimev1.GitCommit(in.Candidate.Commit)); err != nil {
		var ce *runtimev1.ContractError
		if errors.As(err, &ce) {
			return blocked(string(ce.Code))
		}
		return blocked(CodeConformanceMalformed)
	}
	if supportCode != "" {
		return blocked(supportCode)
	}
	snap.Status, snap.Code = ConformanceVerified, ""
	return snap, extra, nil
}

func mapRunError(err error) error {
	switch {
	case err == nil:
		return nil
	case IsBusinessError(err):
		return err
	case errors.Is(err, execution.ErrNotFound), errors.Is(err, execution.ErrHidden):
		return ErrNotFound
	case errors.Is(err, execution.ErrStale):
		return fmt.Errorf("%w: %v", ErrInvalidTransition, err)
	case errors.Is(err, execution.ErrInvalid):
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return Unavailablef(err, "the run could not be read")
}

func mapPeer(err error) error {
	switch {
	case err == nil:
		return nil
	case IsBusinessError(err):
		return err
	}
	return Unavailablef(err, "a dependency is unavailable")
}

func mapEvidenceError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrNotFound):
		return fmt.Errorf("%w: the artifact does not exist in this project", ErrEvidence)
	case IsBusinessError(err):
		return err
	}
	return Unavailablef(err, "the evidence store is unavailable")
}
