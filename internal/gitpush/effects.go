package gitpush

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
)

// The adapters share the translation of a record's Effects into the rows they
// write, so PostgreSQL and memory produce identical audit rows and identical
// event envelopes.

// ResourceType is the audit resource type of a push record; the audit resource
// ID is the push ID.
const ResourceType = "git_push"

// Audit actions.
const (
	AuditAuthorized      = "git.push.authorized"
	AuditApplied         = "git.push.applied"
	AuditRefused         = "git.push.refused"
	AuditUnknown         = "git.push.unknown"
	AuditHistoryRewrite  = "git.history_rewrite"
	AuditOutcomeConflict = "git.push.outcome_contradicted"
)

// Bounds of the audit columns and metadata.
const (
	maxAuditReason    = 2048
	maxAuditRequestID = 256
	maxAuditRefs      = 16
)

// SystemActor is the audit actor of the reconciler.
var SystemActor = AuditActor{Type: "system", ID: "gitpush-reconciler"}

// AuditFields returns the reason and request ID of an audit record trimmed to
// the columns' bounds.
func (a Audit) AuditFields(ctx context.Context) (reason, requestID string) {
	reason, requestID = a.Reason, RequestID(ctx)
	if len(reason) > maxAuditReason {
		reason = reason[:maxAuditReason]
	}
	if len(requestID) > maxAuditRequestID {
		requestID = requestID[:maxAuditRequestID]
	}
	return reason, requestID
}

// AuditID is deterministic per (record, version, position): a retried
// transaction can never write a second row for the same committed change.
func AuditID(r *Record, index int) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "gitpush/audit/%s/%s/%d/%d", r.Scope.TenantID, r.ID, r.Version, index))
	return "aud_push_" + hex.EncodeToString(sum[:20])
}

// EventID is deterministic per record: whichever transaction settles the record
// as applied, the same event identity is appended, so the event is emitted once.
func EventID(r *Record) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "gitpush/event/%s/%s", r.Scope.TenantID, r.ID))
	return "evt_push_" + hex.EncodeToString(sum[:20])
}

// Envelope builds the git.refs_updated envelope of an applied record: the
// aggregate is the push (resource type git_push, resource ID the record's receive
// ID, which the payload's receiveId equals, resource version the record's version
// at settlement), and the payload carries the applied updates inline
// (eventsv1.NewGitRefsUpdated).
// newID supplies a correlation ID when the request carries none.
func (r *Record) Envelope(correlationID string, newID func(prefix string) string) (eventsv1.Envelope, error) {
	dto, err := r.DTO()
	if err != nil {
		return eventsv1.Envelope{}, err
	}
	payload, err := eventsv1.NewGitRefsUpdated(dto)
	if err != nil {
		return eventsv1.Envelope{}, err
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return eventsv1.Envelope{}, err
	}
	if correlationID == "" {
		correlationID = newID("corr")
	}
	return eventsv1.Envelope{
		ID: EventID(r), SchemaVersion: eventsv1.SchemaVersion, Type: eventsv1.TypeGitRefsUpdated, OccurredAt: r.UpdatedAt.UTC(),
		TenantID: r.Scope.TenantID, ProjectID: r.Scope.ProjectID,
		Resource:      eventsv1.Resource{Type: eventsv1.ResourceTypeGitPush, ID: r.ReceiveID, Version: r.Version},
		Actor:         eventsv1.Actor{Type: eventsv1.ActorType(r.Actor.Type), ID: r.Actor.ID},
		CorrelationID: correlationID, Data: raw,
	}, nil
}

// pushActor is the audit actor of a push.
func (r *Record) pushActor() AuditActor {
	if r.Actor.Type == gitv1.ActorUser {
		return AuditActor{Type: "user", ID: r.Actor.ID}
	}
	return AuditActor{Type: "workload", ID: r.Actor.ID}
}

func (r *Record) metadata() map[string]any {
	return map[string]any{
		"pushId": r.ID, "repositoryId": r.RepositoryID, "receiveId": r.ReceiveID, "grantId": r.GrantID, "grantKind": string(r.GrantKind),
		"updates": len(r.Entries),
	}
}

func (r *Record) countsAndRefs(state gitv1.EntryState) (int, []map[string]string) {
	n := 0
	var refs []map[string]string
	for _, e := range r.Entries {
		if e.State != state {
			continue
		}
		n++
		if len(refs) < maxAuditRefs {
			m := map[string]string{"ref": e.Ref}
			if e.Reason != "" {
				m["reason"] = e.Reason
			}
			refs = append(refs, m)
		}
	}
	return n, refs
}

// authorizedEffects are the audit rows of a newly created record.
func authorizedEffects(r *Record) Effects {
	md := r.metadata()
	action := AuditAuthorized
	if r.Status() == gitv1.PushRefused {
		action = AuditRefused
		n, refs := r.countsAndRefs(gitv1.EntryRefused)
		md["refused"], md["refusedRefs"] = n, refs
	}
	var rewrites []string
	for _, e := range r.Entries {
		if e.Kind == gitv1.KindRewrite && e.State == gitv1.EntryAuthorized && len(rewrites) < maxAuditRefs {
			rewrites = append(rewrites, e.Ref)
		}
	}
	if len(rewrites) > 0 {
		md["rewrites"] = rewrites
	}
	return Effects{Audit: []Audit{{Action: action, Actor: r.pushActor(), Metadata: md}}}
}

// settlementEffects are the audit rows and the event of a change that moved the
// record from status before to its current status: one audit row when it
// settled or became unknown, one row per applied history rewrite and the one
// event when the record becomes applied. A record that stays in flight writes
// nothing.
func settlementEffects(before gitv1.PushStatus, r *Record, actor AuditActor, contradicted []string) Effects {
	after := r.Status()
	var fx Effects
	if len(contradicted) > 0 {
		fx.Audit = append(fx.Audit, Audit{Action: AuditOutcomeConflict, Actor: actor, Reason: "the worker reported applied entries that were already refused",
			Metadata: map[string]any{"pushId": r.ID, "repositoryId": r.RepositoryID, "receiveId": r.ReceiveID, "refs": firstN(contradicted, maxAuditRefs)}})
	}
	if after == before || after == gitv1.PushAuthorized {
		return fx
	}
	md := r.metadata()
	applied, _ := r.countsAndRefs(gitv1.EntryApplied)
	refused, _ := r.countsAndRefs(gitv1.EntryRefused)
	unknown, unknownRefs := r.countsAndRefs(gitv1.EntryUnknown)
	md["applied"], md["refused"], md["unknown"] = applied, refused, unknown
	action := AuditApplied
	switch after {
	case gitv1.PushRefused:
		action = AuditRefused
	case gitv1.PushUnknown:
		action = AuditUnknown
		md["unknownRefs"] = unknownRefs
	}
	fx.Audit = append(fx.Audit, Audit{Action: action, Actor: actor, Metadata: md})
	if after == gitv1.PushApplied {
		for _, e := range r.Entries {
			if e.State == gitv1.EntryApplied && e.Kind == gitv1.KindRewrite {
				fx.Audit = append(fx.Audit, Audit{Action: AuditHistoryRewrite, Actor: r.pushActor(), Reason: "non-fast-forward update applied",
					Metadata: map[string]any{"pushId": r.ID, "repositoryId": r.RepositoryID, "ref": e.Ref, "old": e.Old, "new": e.New}})
			}
		}
		fx.Event = true
	}
	return fx
}

func firstN(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// OccurredAt is the time recorded on audit rows written with the record.
func (r *Record) OccurredAt() time.Time { return r.UpdatedAt.UTC() }
