package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/gates"
)

// loadRunView reads the run and its steps without locking, enough to resolve a
// gate step's execution inputs. A decision uses it (it never takes the run
// lock); consumption and claims use the locked aggregate instead.
func (s *Store) loadRunView(ctx context.Context, tx pgx.Tx, scope domain.Scope, id domain.RunID) (*execution.Aggregate, error) {
	var (
		purpose, definition, retention, status string
		workItem, commit                       *string
		revision                               *int64
		inputs                                 []byte
		cancelAt, failAt                       any
	)
	err := tx.QueryRow(ctx, `SELECT purpose, work_item_id, definition_id, content_revision, source_commit, inputs, source_retention, status, cancel_requested_at, failure_intent_at
		FROM runs WHERE tenant_id = $1 AND project_id = $2 AND id = $3`, string(scope.TenantID), string(scope.ProjectID), string(id)).
		Scan(&purpose, &workItem, &definition, &revision, &commit, &inputs, &retention, &status, &cancelAt, &failAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, gates.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	run := &execution.Run{
		Scope: scope, ID: id, Purpose: execution.Purpose(purpose), DefinitionID: domain.DefinitionID(definition),
		SourceRetention: execution.SourceRetention(retention), Status: execution.RunStatus(status),
	}
	if workItem != nil {
		run.WorkItemID = domain.WorkItemID(*workItem)
	}
	if commit != nil {
		run.SourceCommit = *commit
	}
	if revision != nil {
		run.ContentRevision = domain.ContentRevision(*revision)
	}
	if err := json.Unmarshal(inputs, &run.Snapshot); err != nil {
		return nil, fmt.Errorf("gates: stored run inputs: %w", err)
	}
	if cancelAt != nil {
		t := s.clock()
		run.CancelRequestedAt = &t
	}
	if failAt != nil {
		t := s.clock()
		run.FailureIntentAt = &t
	}
	agg := &execution.Aggregate{Run: run}
	if s.sets {
		if err := s.loadSourceView(ctx, tx, agg); err != nil {
			return nil, err
		}
	}
	rows, err := tx.Query(ctx, `SELECT step_id, ordinal, spec, status, gate_ref, attempts_started, output_commit, outputs FROM step_runs
		WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3 ORDER BY ordinal`, string(scope.TenantID), string(scope.ProjectID), string(id))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			stepID, st string
			ordinal, n int
			spec, out  []byte
			ref, oc    *string
		)
		if err := rows.Scan(&stepID, &ordinal, &spec, &st, &ref, &n, &oc, &out); err != nil {
			return nil, err
		}
		decoded, err := execution.DecodeStep(spec)
		if err != nil {
			return nil, fmt.Errorf("gates: stored step: %w", err)
		}
		step := &execution.StepRun{RunID: id, ID: stepID, Ordinal: ordinal, Spec: decoded, Status: execution.StepStatus(st), AttemptsStarted: n}
		if ref != nil {
			step.GateRef = *ref
		}
		if oc != nil {
			step.OutputCommit = *oc
		}
		if len(out) > 0 && string(out) != "{}" {
			if err := json.Unmarshal(out, &step.Outputs); err != nil {
				return nil, fmt.Errorf("gates: stored outputs: %w", err)
			}
		}
		agg.Steps = append(agg.Steps, step)
	}
	return agg, rows.Err()
}

// loadSourceView adds what a source-set subject is resolved from to an unlocked
// view of the run: the pinned set (digest and entries), and for every attempt its
// step, status and the commits it reported. A run without a source set is left
// unchanged, so its gates resolve exactly as before.
func (s *Store) loadSourceView(ctx context.Context, tx pgx.Tx, agg *execution.Aggregate) error {
	run := agg.Run
	scope := run.Scope
	var kind, digest *string
	if err := tx.QueryRow(ctx, `SELECT origin_kind, source_set_digest FROM runs WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
		string(scope.TenantID), string(scope.ProjectID), string(run.ID)).Scan(&kind, &digest); err != nil {
		return err
	}
	if kind == nil || digest == nil {
		return nil
	}
	run.Origin = execution.Origin{Kind: execution.OriginKind(*kind), WorkItemID: run.WorkItemID}
	run.Sources = &execution.RunSources{Digest: *digest, Entries: []execution.RunSource{}}
	rows, err := tx.Query(ctx, `SELECT repository_id, repository_name, repository_url, commit, path, access, branch
		FROM run_sources WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3 ORDER BY ordinal`,
		string(scope.TenantID), string(scope.ProjectID), string(run.ID))
	if err != nil {
		return err
	}
	for rows.Next() {
		var e execution.RunSource
		var access string
		if err := rows.Scan(&e.RepositoryID, &e.Repository, &e.URL, &e.Commit, &e.Path, &access, &e.Branch); err != nil {
			rows.Close()
			return err
		}
		e.Access = gitv1.SourceAccess(access)
		run.Sources.Entries = append(run.Sources.Entries, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	rows, err = tx.Query(ctx, `SELECT id, step_id, ordinal, status FROM step_attempts
		WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3 ORDER BY step_id, ordinal`,
		string(scope.TenantID), string(scope.ProjectID), string(run.ID))
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, step, status string
		var ordinal int
		if err := rows.Scan(&id, &step, &ordinal, &status); err != nil {
			rows.Close()
			return err
		}
		agg.Attempts = append(agg.Attempts, &execution.Attempt{Scope: scope, ID: domain.AttemptID(id), RunID: run.ID, StepID: step, Ordinal: ordinal, Status: execution.AttemptStatus(status)})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	rows, err = tx.Query(ctx, `SELECT attempt_id, repository_id, ref, commit, push_id FROM attempt_commits
		WHERE tenant_id = $1 AND project_id = $2 AND run_id = $3 ORDER BY attempt_id, ordinal`,
		string(scope.TenantID), string(scope.ProjectID), string(run.ID))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var attempt string
		var c execution.CommitOutput
		if err := rows.Scan(&attempt, &c.RepositoryID, &c.Ref, &c.Commit, &c.PushID); err != nil {
			return err
		}
		if at := agg.Attempt(domain.AttemptID(attempt)); at != nil {
			at.Commits = append(at.Commits, c)
		}
	}
	return rows.Err()
}

// loadFacts reads the current facts a gate's subject is compared with: the
// execution inputs recomputed from agg, the work item's current content
// revision, the current digest of every artifact the subject binds and the
// deployment configuration. forceWaiting treats the step as waiting (used for
// currency checks of a consumed approval).
func (s *Store) loadFacts(ctx context.Context, tx pgx.Tx, g *gates.Gate, agg *execution.Aggregate, forceWaiting bool) (gates.Facts, error) {
	var f gates.Facts
	step := agg.Step(g.StepID)
	if step == nil {
		return f, gates.ErrNotFound
	}
	in, set, err := gates.ResolveInputs(agg, g.StepID)
	if err != nil {
		// A bound gate whose inputs cannot be resolved is not applicable; the
		// reason is not a definitive change of the subject.
		return f, fmt.Errorf("%w: the gate inputs cannot be resolved: %v", gates.ErrStaleSubject, err)
	}
	f.Inputs, f.SourceSet = in, set
	f.StepWaiting = forceWaiting || (step.Status == execution.StepPending && agg.Run.AcceptsDispatch())
	var state string
	var rev int64
	err = tx.QueryRow(ctx, `SELECT content_revision, state FROM work_items WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
		string(g.Scope.TenantID), string(g.Scope.ProjectID), string(in.WorkItemID)).Scan(&rev, &state)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return f, gates.ErrNotFound
	case err != nil:
		return f, err
	}
	f.ContentRevision, f.Cancelled = domain.ContentRevision(rev), state == "cancelled"

	f.Evidence = map[domain.ArtifactID]gates.EvidenceFacts{}
	for _, ref := range g.Subject.Evidence() {
		var digest, st, disposition string
		err := tx.QueryRow(ctx, `SELECT digest, state, disposition FROM artifacts WHERE tenant_id = $1 AND project_id = $2 AND id = $3`,
			string(g.Scope.TenantID), string(g.Scope.ProjectID), string(ref.ID)).Scan(&digest, &st, &disposition)
		if errors.Is(err, pgx.ErrNoRows) {
			f.Evidence[ref.ID] = gates.EvidenceFacts{}
			continue
		}
		if err != nil {
			return f, err
		}
		f.Evidence[ref.ID] = gates.EvidenceFacts{Found: true, Usable: (st == "usable" || st == "registered") && disposition == "active", Digest: digest, Scope: g.Scope}
	}

	if id, _ := g.Subject.Deployment(); id != "" || in.DeploymentID != "" {
		want := in.DeploymentID
		if want == "" {
			want = id
		}
		if s.dep == nil {
			return f, fmt.Errorf("%w: a deployment adapter is required for a deployment gate", gates.ErrNotConfigured)
		}
		snap, err := s.dep(tx).Snapshot(ctx, g.Scope, want)
		switch {
		case err == nil:
			f.Deployment = &snap
		case errors.Is(err, gates.ErrNotFound):
			f.Deployment = nil
		case gates.IsBusinessError(err):
			return f, err
		default:
			return f, gates.Unavailablef(err, "the deployment configuration cannot be read")
		}
	}
	return f, nil
}

// authorizeReviewer checks, through the transaction, that actor still holds the
// reviewer authority. A refusal is ErrReviewerAuthority for consumption and
// ErrDenied for a decision (callers choose).
func (s *Store) authorizeReviewer(ctx context.Context, tx pgx.Tx, actor gates.Actor, scope domain.Scope) error {
	err := s.auth(tx).Authorize(ctx, actor, scope, gates.PermDecide)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, gates.ErrHidden):
		return gates.ErrHidden
	case errors.Is(err, gates.ErrDenied):
		return gates.ErrDenied
	}
	return gates.Unavailablef(err, "the authority is unavailable")
}
