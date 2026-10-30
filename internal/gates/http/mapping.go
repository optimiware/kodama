package gateshttp

import (
	"errors"
	"time"

	"github.com/optimiware/kodama/internal/gates"
	"github.com/optimiware/kodama/internal/httpapi"
)

func strp(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// gateDTO renders the gate. The applicability is derived from the gate record
// alone at read time; the decision and progression recheck current facts.
func gateDTO(g *gates.Gate, now time.Time) Gate {
	s := g.Subject
	evidence := make([]EvidenceRef, 0, len(s.Evidence()))
	for _, e := range s.Evidence() {
		evidence = append(evidence, EvidenceRef{Id: string(e.ID), Digest: e.Digest})
	}
	depID, depCfg := s.Deployment()
	req := make([]RequiredEvidence, 0, len(g.Required.Evidence))
	for _, r := range g.Required.Evidence {
		req = append(req, RequiredEvidence{Role: RequiredEvidenceRole(r.Role), Id: string(r.ID), Digest: r.Digest, Expression: strp(r.Expression), Producer: strp(r.Producer)})
	}
	subject := Subject{
		TenantId: string(s.Scope().TenantID), ProjectId: string(s.Scope().ProjectID), WorkItemId: string(s.WorkItemID()),
		ContentRevision: int64(s.ContentRevision()), CandidateCommit: s.PresentedCandidate(), DefinitionId: string(s.DefinitionID()),
		Evidence: evidence, DeploymentId: strp(depID), DeploymentConfigDigest: strp(depCfg),
	}
	if set, ok := s.SourceSet(); ok {
		dto := &SubjectSourceSet{Digest: set.Digest, Sources: make([]SubjectSource, 0, len(set.Sources))}
		for _, x := range set.Sources {
			src := SubjectSource{RepositoryId: x.RepositoryID, Commit: x.Commit}
			if x.Base != nil {
				src.Base = &IntegrationBase{Ref: x.Base.Ref, Commit: x.Base.Commit}
			}
			dto.Sources = append(dto.Sources, src)
		}
		subject.SourceSet = dto
	}
	out := Gate{
		Id: string(g.ID), RunId: string(g.RunID), StepId: g.StepID, Status: GateStatus(g.Status), SubjectDigest: g.SubjectDigest(),
		Subject:     subject,
		Required:    Required{Evidence: req},
		RequestedAt: g.RequestedAt.UTC(), Deadline: g.Deadline.UTC(), Version: int64(g.Version),
		CreatedAt: g.CreatedAt.UTC(), UpdatedAt: g.UpdatedAt.UTC(),
	}
	if c := g.Required.Conformance; c != nil {
		checks := append([]string{}, c.RequiredChecks...)
		out.Required.Conformance = &Conformance{
			Status: ConformanceStatus(c.Status), Code: strp(c.Code), ProducerStep: c.ProducerStep, BaselineDigest: strp(c.BaselineDigest),
			RequiredChecks: checks, ReportId: strp(string(c.ReportID)), ReportDigest: strp(c.ReportDigest),
		}
	}
	if d := g.Decision; d != nil {
		out.Decision = &Decision{
			DecidedBy: Actor{Type: ActorType(d.By.Type), Id: d.By.ID}, Decision: DecisionDecision(d.Decision), Reason: d.Reason, DecidedAt: d.At.UTC(),
		}
	}
	if inv := g.Invalidation; inv != nil {
		x := &Invalidation{At: inv.At.UTC(), Reason: inv.Reason, SupersedingCommit: strp(inv.SupersedingCommit)}
		if inv.SupersedingRevision != 0 {
			v := inv.SupersedingRevision
			x.SupersedingRevision = &v
		}
		out.Invalidation = x
	}
	out.Applicability = Applicability{Expired: g.Expired(now)}
	if err := g.Applicable(now); err == nil {
		out.Applicability.Applicable = true
	} else {
		var se *gates.StaleError
		var ce *gates.ConformanceError
		switch {
		case errors.As(err, &se):
			out.Applicability.Reason = strp(se.Reason)
		case errors.As(err, &ce):
			out.Applicability.Reason = strp(ce.Code)
		}
	}
	return out
}

func etagHeader(g *gates.Gate) (map[string]string, error) {
	tag, err := httpapi.FormatETag(g.Version)
	if err != nil {
		return nil, err
	}
	return map[string]string{"ETag": tag}, nil
}
