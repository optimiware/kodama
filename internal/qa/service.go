package qa

import (
	"context"
	"errors"
	"fmt"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// Config wires a Service. Every collaborator is required: there is no default
// that grants access or trusts a report.
type Config struct {
	Attempts    Attempts
	Deployments Deployments
	Artifacts   Artifacts
	Defects     Defects
	// ReportOutput is the declared artifact output name under which the QA step
	// returns its report. It is installation/profile configuration agreed with
	// the QA step's owner (T-069); empty identifies nothing and fails
	// construction.
	ReportOutput string
	// DeploymentInput is the name of the pinned run input that carries the
	// identifier of the deployment the QA step is to inspect. The promotion or
	// QA pipeline that admitted the attempt set it; it is read from the attempt's
	// immutable pinned context, never from the report, so a report cannot choose
	// which deployment it claims to have inspected. Empty identifies nothing and
	// fails construction.
	DeploymentInput string
	// MaxReportBytes bounds the report read (default MaxReportBytes).
	MaxReportBytes int64
}

// Service maps a finished QA attempt's report to defect work items. It has no
// operation that approves a gate, completes work or transitions any work item.
type Service struct {
	cfg Config
}

// NewService validates cfg.
func NewService(cfg Config) (*Service, error) {
	switch {
	case cfg.Attempts == nil, cfg.Deployments == nil, cfg.Artifacts == nil, cfg.Defects == nil:
		return nil, errors.New("qa: attempts, deployments, artifacts and defects are required")
	case cfg.ReportOutput == "":
		return nil, errors.New("qa: the declared report output name is required")
	case cfg.DeploymentInput == "":
		return nil, errors.New("qa: the pinned deployment input name is required")
	case cfg.MaxReportBytes < 0 || cfg.MaxReportBytes > MaxReportBytes:
		return nil, fmt.Errorf("qa: the report bound must be within 0..%d", MaxReportBytes)
	}
	if cfg.MaxReportBytes == 0 {
		cfg.MaxReportBytes = MaxReportBytes
	}
	return &Service{cfg: cfg}, nil
}

// RecordCommand asks to map one finished QA attempt's report.
type RecordCommand struct {
	Scope   domain.Scope
	Attempt domain.AttemptID
	Caller  Caller
}

// ReportRef identifies the report that was mapped.
type ReportRef struct {
	ArtifactID domain.ArtifactID
	// ArtifactDigest is the digest of the stored report bytes.
	ArtifactDigest runtimev1.Digest
	// Digest is the digest of the canonical report.
	Digest runtimev1.Digest
}

// DefectResult is the outcome for one observation.
type DefectResult struct {
	// Index is the observation's position in the report.
	Index int
	Title string
	// Key is the deterministic deduplication key and ID the defect's identity.
	Key string
	ID  domain.WorkItemID
	// Created is true when this call observed the creation commit; a replay, a
	// retry after a lost acknowledgment or a repeated pass reports false.
	Created bool
	State   string
	// EvidenceAdded is the number of artifact links this call added to an
	// existing defect (zero for an unchanged or terminal one).
	EvidenceAdded int
}

// Result is what Record established. On a partial failure Record returns the
// defects already filed together with the error; repeating the command files
// the rest and duplicates nothing.
type Result struct {
	Attempt    domain.AttemptID
	Report     ReportRef
	Deployment DeploymentRef
	Defects    []DefectResult
}

// authorize admits only a trusted control-plane caller. Mapping is a
// post-completion control-plane operation, not a runtime tool action: the QA
// attempt is finished by then and the runtime grant model has no action for it,
// so a workload identity (the attempt's own included), a person or an operator
// holds no authority to file defects here. The work module's authority for the
// defects themselves is separate and verified on every mutation by the Defects
// adapter (see adapters.NewWork); this check only decides who may ask.
func (c Caller) authorize() error {
	a := c.Actor
	if a.ID == "" || len(a.ID) > 256 || len(c.RequestID) > 256 {
		return failf(ErrDenied, "the caller cannot be attributed")
	}
	if a.Type != ActorSystem {
		return failf(ErrDenied, "only the control plane maps QA findings")
	}
	return nil
}

// Record maps the completed QA attempt's report to defect work items.
//
// Order: authorize the caller; read the attempt's trusted facts and require an
// accepted completed result; read and strictly decode the declared report;
// bind the report to the attempt's pinned acceptance context and environment;
// verify the deployment against the deployment module; verify every evidence
// artifact; and only then pin the evidence and file defects. Nothing is written
// before every check passed, so a refused report leaves no defect and no pin.
func (s *Service) Record(ctx context.Context, cmd RecordCommand) (Result, error) {
	if err := cmd.Scope.Validate(); err != nil {
		return Result{}, invalidf("%v", err)
	}
	if err := cmd.Attempt.Validate(); err != nil {
		return Result{}, invalidf("%v", err)
	}
	if err := cmd.Caller.authorize(); err != nil {
		return Result{}, err
	}

	facts, err := s.cfg.Attempts.Facts(ctx, cmd.Scope, cmd.Attempt)
	if err != nil {
		return Result{}, passThrough(err, "the attempt could not be read")
	}
	if facts.Scope != cmd.Scope || facts.Attempt != cmd.Attempt || facts.Context.Scope() != cmd.Scope || facts.Context.AttemptID != cmd.Attempt {
		return Result{}, failf(ErrNotFound, "attempt not found")
	}
	pin, err := s.pinnedContext(facts)
	if err != nil {
		return Result{}, err
	}

	rep, reportMeta, err := s.readReport(ctx, facts)
	if err != nil {
		return Result{}, err
	}
	repDigest, err := rep.Digest()
	if err != nil {
		return Result{}, invalidf("the report cannot be canonicalized")
	}
	ref := ReportRef{ArtifactID: reportMeta.ID, ArtifactDigest: reportMeta.Digest, Digest: repDigest}
	out := Result{Attempt: cmd.Attempt, Report: ref}

	if err := s.bindContext(facts, rep); err != nil {
		return out, err
	}
	trusted, err := s.trustedDeployment(facts)
	if err != nil {
		return out, err
	}
	dep, err := s.checkDeployment(ctx, cmd.Scope, rep.Deployment, trusted, pin)
	if err != nil {
		return out, err
	}
	out.Deployment = rep.Deployment
	if err := s.checkSubject(ctx, cmd.Scope, rep); err != nil {
		return out, err
	}
	if err := s.verifyEvidence(ctx, facts, rep); err != nil {
		return out, err
	}

	for i, o := range rep.Observations {
		plan := plan(cmd, facts, rep, o, dep, reportMeta, ref)
		res := DefectResult{Index: i, Title: o.Title, Key: plan.draft.Key, ID: plan.draft.ID}
		if err := s.file(ctx, plan, &res); err != nil {
			out.Defects = append(out.Defects, res)
			return out, err
		}
		out.Defects = append(out.Defects, res)
	}
	return out, nil
}

// passThrough keeps this module's classes and converts anything else into
// ErrUnavailable: an unknown failure is never success and never leaks detail.
func passThrough(err error, what string) error {
	if Classified(err) {
		return err
	}
	return Unavailablef(err, "%s", what)
}

// pinnedContext requires the attempt to have been admitted with an acceptance
// context (a work item) and a declared, pinned environment, and to have
// finished with an accepted completed result.
func (s *Service) pinnedContext(f AttemptFacts) (runtimev1.EnvironmentPin, error) {
	if !f.Completed {
		return runtimev1.EnvironmentPin{}, failf(ErrNotReady, "the attempt has no accepted completed result")
	}
	if f.Context.WorkItem == nil {
		return runtimev1.EnvironmentPin{}, failf(ErrContext, "the attempt was not pinned to a work item")
	}
	if f.Context.Environment == nil {
		return runtimev1.EnvironmentPin{}, failf(ErrContext, "the attempt was not pinned to an environment")
	}
	return *f.Context.Environment, nil
}

// readReport resolves the declared report output of the attempt: it must be a
// declared artifact output whose accepted value names a usable file produced by
// this attempt. The bytes are read through the artifact module (size and digest
// verified) and strictly decoded.
func (s *Service) readReport(ctx context.Context, f AttemptFacts) (Report, EvidenceMeta, error) {
	declared := false
	for _, o := range f.Context.Outputs {
		if o.Name == s.cfg.ReportOutput && o.Type == runtimev1.OutputArtifact {
			declared = true
		}
	}
	id, ok := f.Outputs[s.cfg.ReportOutput]
	if !declared || !ok || id == "" {
		return Report{}, EvidenceMeta{}, failf(ErrNotReady, "the attempt returned no declared report")
	}
	meta, err := s.describe(ctx, f, domain.ArtifactID(id))
	if err != nil {
		return Report{}, EvidenceMeta{}, err
	}
	data, err := s.cfg.Artifacts.Read(ctx, f.Scope, meta, s.cfg.MaxReportBytes)
	if err != nil {
		return Report{}, EvidenceMeta{}, passThrough(err, "the report could not be read")
	}
	rep, err := Decode(data)
	if err != nil {
		return Report{}, EvidenceMeta{}, err
	}
	return rep, meta, nil
}

// describe loads an artifact's metadata and requires it to be a usable file
// produced by the QA attempt itself. Absent, foreign and unusable artifacts are
// one class.
func (s *Service) describe(ctx context.Context, f AttemptFacts, id domain.ArtifactID) (EvidenceMeta, error) {
	reject := func() (EvidenceMeta, error) {
		return EvidenceMeta{}, failf(ErrEvidence, "an artifact is not a verified output of the attempt")
	}
	if id.Validate() != nil {
		return reject()
	}
	meta, err := s.cfg.Artifacts.Describe(ctx, f.Scope, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return reject()
		}
		return EvidenceMeta{}, passThrough(err, "the artifact store could not answer")
	}
	if meta.ID != id || meta.Scope != f.Scope || meta.Attempt != f.Attempt || meta.Run != f.Run || !meta.File || !meta.Usable {
		return reject()
	}
	return meta, nil
}

// bindContext applies the pinned acceptance context: the report judged this
// work item at this content revision, and cites only criteria the context holds.
func (s *Service) bindContext(f AttemptFacts, r Report) error {
	wi := f.Context.WorkItem
	if r.Subject.WorkItemID != wi.ID || r.Subject.ContentRevision != wi.ContentRevision {
		return failf(ErrContext, "the report judged another work item or content revision than the attempt was pinned to")
	}
	known := make(map[string]struct{}, len(wi.AcceptanceCriteria))
	for _, c := range wi.AcceptanceCriteria {
		known[c.ID] = struct{}{}
	}
	for i, o := range r.Observations {
		if o.CriterionID == nil {
			continue
		}
		if _, ok := known[*o.CriterionID]; !ok {
			return failf(ErrContext, "observation %d cites a criterion the pinned context does not hold", i)
		}
	}
	return nil
}

// trustedDeployment reads the deployment the attempt was pinned to inspect: a
// string input of the attempt's immutable context under the configured name.
func (s *Service) trustedDeployment(f AttemptFacts) (string, error) {
	v, ok := f.Context.Inputs[s.cfg.DeploymentInput]
	if !ok || !v.IsString() || v.String() == "" {
		return "", failf(ErrContext, "the attempt was not pinned to a deployment")
	}
	return v.String(), nil
}

// checkDeployment is DQ-10: the report must identify the deployment and
// configuration inspected, and that identification must be the deployment
// module's succeeded record of exactly the environment and immutable
// configuration the attempt was pinned to, for the reported candidate and
// image, and it must be the very deployment the attempt's pinned input names:
// another succeeded deployment of the same environment and configuration (for
// example of another candidate) cannot be substituted by the report. A mismatch
// in any field, an absent deployment and a deployment of
// another project or tenant are the same refusal.
func (s *Service) checkDeployment(ctx context.Context, scope domain.Scope, d DeploymentRef, trusted string, pin runtimev1.EnvironmentPin) (DeploymentFacts, error) {
	wrong := func() (DeploymentFacts, error) {
		return DeploymentFacts{}, failf(ErrWrongDeployment, "the report does not identify the deployment the attempt was pinned to")
	}
	if d.ID != trusted || d.EnvironmentID != pin.ID || d.ConfigDigest != pin.ConfigDigest {
		return wrong()
	}
	facts, err := s.cfg.Deployments.Deployment(ctx, scope, d.ID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return wrong()
		}
		return DeploymentFacts{}, passThrough(err, "the deployment could not be read")
	}
	switch {
	case facts.Scope != scope, facts.ID != d.ID:
		return wrong()
	case facts.Environment != pin.ID, facts.ConfigDigest != pin.ConfigDigest:
		return wrong()
	case facts.Candidate != d.CandidateCommit, facts.Image != d.ImageDigest:
		return wrong()
	case !facts.Succeeded:
		return wrong()
	}
	return facts, nil
}

// checkSubject requires the work item under test to exist in the scope and not
// to be cancelled (WM-01): defects are filed in the subject's project only.
func (s *Service) checkSubject(ctx context.Context, scope domain.Scope, r Report) error {
	sub, err := s.cfg.Defects.Subject(ctx, scope, r.Subject.WorkItemID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return failf(ErrContext, "the work item under test was not found")
		}
		return passThrough(err, "the work item could not be read")
	}
	if sub.ID != r.Subject.WorkItemID {
		return failf(ErrContext, "the work item under test was not found")
	}
	if sub.Cancelled {
		return failf(ErrContext, "the work item under test is cancelled")
	}
	return nil
}

// accepted is the set of artifacts the attempt's accepted result finalized:
// declared artifact outputs and diagnostics.
func accepted(f AttemptFacts) map[domain.ArtifactID]struct{} {
	set := map[domain.ArtifactID]struct{}{}
	for _, o := range f.Context.Outputs {
		if o.Type != runtimev1.OutputArtifact {
			continue
		}
		if v, ok := f.Outputs[o.Name]; ok && v != "" {
			set[domain.ArtifactID(v)] = struct{}{}
		}
	}
	for _, d := range f.Diagnostics {
		set[d] = struct{}{}
	}
	return set
}

// verifyEvidence requires every cited artifact to be a usable file of the QA
// attempt itself, finalized by its accepted result, with exactly the digest the
// report states; a screenshot is an image.
func (s *Service) verifyEvidence(ctx context.Context, f AttemptFacts, r Report) error {
	final := accepted(f)
	checked := map[domain.ArtifactID]EvidenceMeta{}
	for _, o := range r.Observations {
		for _, e := range o.Evidence {
			meta, ok := checked[e.ArtifactID]
			if !ok {
				if _, fin := final[e.ArtifactID]; !fin {
					return failf(ErrEvidence, "an artifact is not a verified output of the attempt")
				}
				var err error
				if meta, err = s.describe(ctx, f, e.ArtifactID); err != nil {
					return err
				}
				checked[e.ArtifactID] = meta
			}
			if meta.Digest != e.Digest {
				return failf(ErrEvidence, "an artifact does not have the stated digest")
			}
			if e.Kind == EvidenceScreenshot && !mediaImage.MatchString(meta.MediaType) {
				return failf(ErrEvidence, "a screenshot is not an image")
			}
		}
	}
	return nil
}
