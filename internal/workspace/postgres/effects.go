package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/workspace"
)

// writeEffects persists receipts, pins, audit records and events for a change
// whose workspace row is already current at w.Version.
func (s *Store) writeEffects(ctx context.Context, tx pgx.Tx, w *workspace.Workspace, fx workspace.Effects) error {
	if r := fx.Receipt; r != nil {
		if _, err := tx.Exec(ctx, `INSERT INTO workspace_publications
			(tenant_id, project_id, workspace_id, attempt_id, commit_oid, expected_commit, artifact_id, bundle_digest,
			 status, observed_tip, conflict_reason, outcome, created_at, settled_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
			ON CONFLICT (tenant_id, project_id, workspace_id, attempt_id) DO UPDATE SET
			  status = EXCLUDED.status, observed_tip = EXCLUDED.observed_tip, conflict_reason = EXCLUDED.conflict_reason,
			  outcome = EXCLUDED.outcome, settled_at = EXCLUDED.settled_at`,
			w.Scope.TenantID, w.Scope.ProjectID, w.ID, r.AttemptID, r.Commit, r.ExpectedCommit, r.ArtifactID, r.BundleDigest,
			r.Status, nstr(string(r.ObservedTip)), nstr(r.ConflictReason), r.Outcome, r.CreatedAt, utcPtr(r.SettledAt)); err != nil {
			return fmt.Errorf("workspace: save publication: %w", err)
		}
	}
	for _, p := range fx.Pins {
		if _, err := tx.Exec(ctx, `INSERT INTO workspace_retained_refs
			(tenant_id, project_id, workspace_id, id, commit_oid, ref_name, purpose, run_id, attempt_id, status,
			 created_at, confirmed_at, released_at, removal_requested_at, removed_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
			ON CONFLICT (tenant_id, project_id, workspace_id, id) DO UPDATE SET
			  status = EXCLUDED.status, confirmed_at = EXCLUDED.confirmed_at, released_at = EXCLUDED.released_at,
			  removal_requested_at = EXCLUDED.removal_requested_at, removed_at = EXCLUDED.removed_at`,
			w.Scope.TenantID, w.Scope.ProjectID, w.ID, p.ID, p.Commit, p.RefName, p.Purpose, p.RunID, nstr(string(p.AttemptID)), p.Status,
			p.CreatedAt, utcPtr(p.ConfirmedAt), utcPtr(p.ReleasedAt), utcPtr(p.RemovalRequestedAt), utcPtr(p.RemovedAt)); err != nil {
			return fmt.Errorf("workspace: save retained reference: %w", err)
		}
	}
	for _, a := range fx.Associations {
		// Only the mutable columns move on conflict; identity and base never change.
		if _, err := tx.Exec(ctx, `INSERT INTO workspace_repositories
			(tenant_id, project_id, workspace_id, repository_id, task_branch, base_commit, accepted_commit, state, last_error, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			ON CONFLICT (tenant_id, project_id, workspace_id, repository_id) DO UPDATE SET
			  accepted_commit = EXCLUDED.accepted_commit, state = EXCLUDED.state, last_error = EXCLUDED.last_error, updated_at = EXCLUDED.updated_at`,
			w.Scope.TenantID, w.Scope.ProjectID, w.ID, a.RepositoryID, a.TaskBranch, a.BaseCommit, nstr(string(a.AcceptedCommit)), a.State,
			nstr(a.LastError), a.CreatedAt, a.UpdatedAt); err != nil {
			return fmt.Errorf("workspace: save repository association: %w", err)
		}
	}
	for _, c := range fx.Checkpoints {
		if _, err := tx.Exec(ctx, `INSERT INTO workspace_checkpoints
			(tenant_id, project_id, workspace_id, repository_id, push_id, ref, commit_oid, previous_commit, advanced, attempt_id, run_id,
			 pin_ref, pin_status, created_at, pinned_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
			ON CONFLICT (tenant_id, project_id, workspace_id, repository_id, push_id, ref) DO UPDATE SET
			  pin_ref = EXCLUDED.pin_ref, pin_status = EXCLUDED.pin_status, pinned_at = EXCLUDED.pinned_at`,
			w.Scope.TenantID, w.Scope.ProjectID, w.ID, c.RepositoryID, c.PushID, c.Ref, c.Commit, c.PreviousCommit, c.Advanced, c.AttemptID, c.RunID,
			c.PinRef, c.PinStatus, c.CreatedAt, utcPtr(c.PinnedAt)); err != nil {
			return fmt.Errorf("workspace: save checkpoint: %w", err)
		}
	}
	for _, a := range fx.Audit {
		if err := s.writeAudit(ctx, tx, w, a); err != nil {
			return err
		}
	}
	for i, e := range fx.Events {
		if err := s.writeEvent(ctx, tx, w, i, e); err != nil {
			return err
		}
	}
	return nil
}

// auditActorType maps workspace actors to the base audit vocabulary; the
// event vocabulary's "service" is the audit table's "system".
func auditActorType(t workspace.ActorType) string {
	if t == workspace.ActorService {
		return "system"
	}
	return string(t)
}

func (s *Store) writeAudit(ctx context.Context, tx pgx.Tx, w *workspace.Workspace, a workspace.Audit) error {
	md := a.Metadata
	if md == nil {
		md = map[string]any{}
	}
	raw, err := json.Marshal(md)
	if err != nil {
		return fmt.Errorf("workspace: audit metadata: %w", err)
	}
	reason := a.Reason
	if len(reason) > 2048 {
		reason = reason[:2048]
	}
	reqID := workspace.RequestID(ctx)
	if len(reqID) > 256 {
		reqID = reqID[:256]
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit (tenant_id, id, project_id, occurred_at, actor_type, actor_id, action, resource_type, resource_id, resource_version, reason, request_id, metadata)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'workspace',$8,$9,$10,$11,$12)`,
		w.Scope.TenantID, s.opts.NewID("aud"), w.Scope.ProjectID, s.now(), auditActorType(a.Actor.Type), a.Actor.ID, a.Action,
		w.ID, int64(w.Version), nstr(reason), nstr(reqID), raw)
	if err != nil {
		return fmt.Errorf("workspace: write audit: %w", err)
	}
	return nil
}

// eventID is deterministic per (workspace, version, position): a retried
// transaction can never append a second event for the same committed change.
func eventID(w *workspace.Workspace, index int) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "workspace/%s/%s/%s/%d/%d", w.Scope.TenantID, w.Scope.ProjectID, w.ID, w.Version, index))
	return "evt_ws_" + hex.EncodeToString(sum[:20])
}

func (s *Store) writeEvent(ctx context.Context, tx pgx.Tx, w *workspace.Workspace, index int, e workspace.Event) error {
	var data any
	switch e.Kind {
	case workspace.EventStateChanged:
		data = eventsv1.WorkspaceStateChanged{From: e.From, To: e.To, Reason: e.Reason}
	case workspace.EventCheckpointCreated:
		repository := w.RepositoryID
		if e.Repository != "" {
			repository = e.Repository
		}
		p := eventsv1.WorkspaceCheckpointCreated{Repository: string(repository), NewCommit: string(e.NewCommit), HumanSynchronization: e.HumanSynchronization}
		if e.PreviousCommit != "" {
			prev := string(e.PreviousCommit)
			p.PreviousCommit = &prev
		}
		if e.PublishingAttempt != "" {
			att := string(e.PublishingAttempt)
			p.PublishingAttemptID = &att
		}
		data = p
	default:
		return fmt.Errorf("workspace: unknown event kind %q", e.Kind)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	corr := workspace.RequestID(ctx)
	if corr == "" {
		corr = s.opts.NewID("corr")
	}
	env := eventsv1.Envelope{
		ID: eventID(w, index), SchemaVersion: eventsv1.SchemaVersion, Type: string(e.Kind), OccurredAt: s.now(),
		TenantID: w.Scope.TenantID, ProjectID: w.Scope.ProjectID,
		Resource:      eventsv1.Resource{Type: "workspace", ID: string(w.ID), Version: int64(w.Version)},
		Actor:         eventsv1.Actor{Type: eventsv1.ActorType(e.Actor.Type), ID: e.Actor.ID},
		CorrelationID: corr, Data: raw,
	}
	if _, err := s.events.Append(ctx, tx, env); err != nil {
		return fmt.Errorf("workspace: append event %s: %w", e.Kind, err)
	}
	return nil
}
