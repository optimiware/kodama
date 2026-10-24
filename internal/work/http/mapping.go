package workhttp

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/work"
)

// Optional is a JSON member that distinguishes absent (Set false), null (Set and
// Null) and a value. The generated UpdateWorkItem uses it for every patch field,
// so PATCH can clear nullable fields explicitly while omitted fields stay
// unchanged.
type Optional[T any] struct {
	Set   bool
	Null  bool
	Value T
}

// UnmarshalJSON records presence and nullness and decodes the value strictly:
// unknown members of a nested object are rejected like top-level ones.
func (o *Optional[T]) UnmarshalJSON(b []byte) error {
	o.Set = true
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		o.Null = true
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&o.Value); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data")
	}
	return nil
}

// MarshalJSON writes the value, or null when absent.
func (o Optional[T]) MarshalJSON() ([]byte, error) {
	if !o.Set || o.Null {
		return []byte("null"), nil
	}
	return json.Marshal(o.Value)
}

func ts(t time.Time) time.Time { return t.UTC() }

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func actorDTO(a work.Actor) Actor { return Actor{Id: a.ID, Type: ActorType(a.Type)} }

func evidenceDTO(es []work.EvidenceRef) []EvidenceRef {
	out := make([]EvidenceRef, 0, len(es))
	for _, e := range es {
		out = append(out, EvidenceRef{Id: e.ID, Type: EvidenceRefType(e.Type), Note: strPtr(e.Note)})
	}
	return out
}

func evidenceDomain(es []EvidenceRef) []work.EvidenceRef {
	out := make([]work.EvidenceRef, 0, len(es))
	for _, e := range es {
		n := ""
		if e.Note != nil {
			n = *e.Note
		}
		out = append(out, work.EvidenceRef{Type: work.EvidenceType(e.Type), ID: e.Id, Note: n})
	}
	return out
}

func criteriaDTO(cs []work.Criterion) []AcceptanceCriterion {
	out := make([]AcceptanceCriterion, 0, len(cs))
	for _, c := range cs {
		out = append(out, AcceptanceCriterion{Id: c.ID, Text: c.Text})
	}
	return out
}

func criteriaDomain(cs []AcceptanceCriterion) []work.Criterion {
	out := make([]work.Criterion, 0, len(cs))
	for _, c := range cs {
		out = append(out, work.Criterion{ID: c.Id, Text: c.Text})
	}
	return out
}

func findingsDTO(fs []work.Finding) *[]Finding {
	if len(fs) == 0 {
		return nil
	}
	out := make([]Finding, 0, len(fs))
	for _, f := range fs {
		out = append(out, Finding{Id: f.ID, Summary: strPtr(f.Summary)})
	}
	return &out
}

func childSummaryDTO(c *work.ChildSummary) *ChildSummary {
	if c == nil {
		return nil
	}
	return &ChildSummary{Total: c.Total, Done: c.Done, Cancelled: c.Cancelled}
}

func reviewDTO(r *work.ReviewSubject) *ReviewSubject {
	if r == nil {
		return nil
	}
	out := &ReviewSubject{CandidateCommit: strPtr(r.CandidateCommit), ChildSummary: childSummaryDTO(r.ChildSummary),
		ContentRevision: int64(r.ContentRevision), RecordedAt: ts(r.RecordedAt)}
	if len(r.Evidence) > 0 {
		ev := evidenceDTO(r.Evidence)
		out.Evidence = &ev
	}
	return out
}

func completionDTO(c *work.CompletionRecord) *CompletionRecord {
	if c == nil {
		return nil
	}
	accepted := append([]string{}, c.AcceptedCriteria...)
	return &CompletionRecord{
		AcceptedCriteria: accepted, AcknowledgedFindings: findingsDTO(c.AcknowledgedFindings),
		CandidateCommit: strPtr(c.CandidateCommit), ChildSummary: childSummaryDTO(c.ChildSummary),
		CompletedAt: ts(c.CompletedAt), CompletedBy: actorDTO(c.CompletedBy),
		ContentRevision: int64(c.ContentRevision), Reason: strPtr(c.Reason),
	}
}

// itemDTO is a total mapping: it has no failure path, so a successfully
// committed mutation can always be rendered.
func itemDTO(it work.Item) WorkItem {
	return WorkItem{
		Id: string(it.ID), Kind: WorkKind(it.Kind), Title: it.Title, Spec: it.Spec,
		AcceptanceCriteria: criteriaDTO(it.Criteria), EvidenceLinks: evidenceDTO(it.EvidenceLinks),
		AssigneeId: strPtr(it.AssigneeID), ParentId: strPtr(string(it.ParentID)),
		State: WorkState(it.State), BlockedReason: strPtr(it.BlockedReason),
		Version: int64(it.Version), ContentRevision: int64(it.ContentRevision),
		ReviewSubject: reviewDTO(it.ReviewSubject), Completion: completionDTO(it.Completion),
		CreatedBy: actorDTO(it.CreatedBy), CreatedAt: ts(it.CreatedAt), UpdatedAt: ts(it.UpdatedAt),
	}
}

func historyDTO(h work.HistoryEntry) HistoryEntry {
	changed := h.ChangedFields
	if changed == nil {
		changed = []string{}
	}
	out := HistoryEntry{
		Id: h.ID, Version: int64(h.Version), ContentRevision: int64(h.ContentRevision), Kind: HistoryEntryKind(h.Kind),
		Actor: actorDTO(h.Actor), OccurredAt: ts(h.OccurredAt), Reason: strPtr(h.Reason), ChangedFields: changed,
		Before: orEmpty(h.Before), After: orEmpty(h.After), Review: reviewDTO(h.Review), Completion: completionDTO(h.Completion),
		RequestId: strPtr(h.RequestID),
	}
	if h.From != "" {
		f := WorkState(h.From)
		out.From = &f
	}
	if h.To != "" {
		t := WorkState(h.To)
		out.To = &t
	}
	return out
}

func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// patchDomain converts the PATCH body. Null clears only assigneeId, parentId and
// blockedReason; for every other member it is a request error.
func patchDomain(u UpdateWorkItem) (work.Patch, error) {
	var p work.Patch
	if u.Title.Set {
		if u.Title.Null {
			return p, httpapi.BadRequestError("title cannot be cleared")
		}
		v := u.Title.Value
		p.Title = &v
	}
	if u.Spec.Set {
		if u.Spec.Null {
			return p, httpapi.BadRequestError("spec cannot be null; send an empty string to clear it")
		}
		v := u.Spec.Value
		p.Spec = &v
	}
	if u.AcceptanceCriteria.Set {
		if u.AcceptanceCriteria.Null {
			return p, httpapi.BadRequestError("acceptanceCriteria cannot be null; send an empty array")
		}
		v := criteriaDomain(u.AcceptanceCriteria.Value)
		p.Criteria = &v
	}
	if u.EvidenceLinks.Set {
		if u.EvidenceLinks.Null {
			return p, httpapi.BadRequestError("evidenceLinks cannot be null; send an empty array")
		}
		v := evidenceDomain(u.EvidenceLinks.Value)
		p.EvidenceLinks = &v
	}
	if u.AssigneeId.Set {
		p.Assignee = nullable(u.AssigneeId, func(s string) string { return s })
	}
	if u.ParentId.Set {
		p.Parent = nullable(u.ParentId, func(s string) domain.WorkItemID { return domain.WorkItemID(s) })
	}
	if u.BlockedReason.Set {
		p.BlockedReason = nullable(u.BlockedReason, func(s string) string { return s })
	}
	if p.Empty() {
		return p, httpapi.BadRequestError("the patch names no field")
	}
	return p, nil
}

func nullable[A, B any](o Optional[A], conv func(A) B) work.Nullable[B] {
	if o.Null {
		return work.Clear[B]()
	}
	return work.Some(conv(o.Value))
}

func transitionDomain(t TransitionRequest) work.TransitionRequest {
	out := work.TransitionRequest{To: work.State(t.To)}
	if t.Reason != nil {
		out.Reason = *t.Reason
	}
	if t.Review != nil {
		out.Review = &work.ReviewClaim{CandidateCommit: t.Review.CandidateCommit, Evidence: evidenceDomain(t.Review.Evidence)}
	}
	if t.Completion != nil {
		c := &work.CompletionClaim{
			ContentRevision:  domain.ContentRevision(t.Completion.ContentRevision),
			AcceptedCriteria: append([]string{}, t.Completion.AcceptedCriteria...),
		}
		if t.Completion.CandidateCommit != nil {
			c.CandidateCommit = *t.Completion.CandidateCommit
		}
		if t.Completion.AcknowledgedFindings != nil {
			for _, f := range *t.Completion.AcknowledgedFindings {
				s := ""
				if f.Summary != nil {
					s = *f.Summary
				}
				c.AcknowledgedFindings = append(c.AcknowledgedFindings, work.Finding{ID: f.Id, Summary: s})
			}
		}
		out.Completion = c
	}
	return out
}

func createDomain(c CreateWorkItem, scope domain.Scope, caller work.Caller) work.CreateCommand {
	cmd := work.CreateCommand{Scope: scope, Caller: caller, Kind: work.Kind(c.Kind), Title: c.Title}
	if c.Spec != nil {
		cmd.Spec = *c.Spec
	}
	if c.AcceptanceCriteria != nil {
		cmd.Criteria = criteriaDomain(*c.AcceptanceCriteria)
	}
	if c.EvidenceLinks != nil {
		cmd.EvidenceLinks = evidenceDomain(*c.EvidenceLinks)
	}
	if c.AssigneeId != nil {
		cmd.AssigneeID = *c.AssigneeId
	}
	if c.ParentId != nil {
		cmd.ParentID = domain.WorkItemID(*c.ParentId)
	}
	return cmd
}
