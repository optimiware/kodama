package executiontest

import (
	"encoding/json"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/runtime/runtimetest"
)

// LegacyRuns are the historical runs SeedLegacyRuns wrote with the pre-T112
// schema, as the SQL statements of the original store wrote them.
type LegacyRuns struct {
	Project  Project
	Def      pipeline.Definition
	Finished domain.RunID // succeeded: one succeeded step, one settled attempt
	Queued   domain.RunID // queued: one pending step
	Running  domain.RunID // running: one running attempt with its pinned context
}

const legacyDoc = "apiVersion: kodama/v1alpha1\nkind: Pipeline\nmetadata: {name: historical}\nspec:\n  steps:\n    - {id: job, kind: container, image: " +
	SharedImage + ", command: [/bin/true], timeoutSeconds: 60, workspace: {access: read, from: run}}\n"

// SeedLegacyRuns inserts three historical runs with raw SQL that names ONLY the
// columns of the pre-T112 schema (no run_shared, no deadline), so a store that
// reads or writes any new column unconditionally fails on an unmigrated
// database and an upgrade is proven on real rows.
func SeedLegacyRuns(e *Env) LegacyRuns {
	e.T.Helper()
	p := e.NewProject(ScopeA)
	def := e.Define(ScopeA, legacyDoc)
	doc, err := def.Content.Document()
	if err != nil {
		e.T.Fatal(err)
	}
	now := e.Clock.Now().UTC().Truncate(time.Microsecond)
	snap := execution.Snapshot{Inputs: []execution.PinnedInput{}, Secrets: []execution.StepSecret{},
		Runtime: &execution.RuntimeBinding{Backend: runtimetest.ManualBackend, Requires: execution.OCIImage | execution.Isolation | execution.ResourceLimits}}
	snapJSON, err := json.Marshal(snap)
	if err != nil {
		e.T.Fatal(err)
	}
	spec, _ := json.Marshal(execution.Specification{Spec: "# Plan\nDo the thing.", AcceptanceCriteria: []runtimev1.AcceptanceCriterion{}})
	rev := e.ContentRevision(p)
	insertRun := func(id, status, reason string, started, finished *time.Time) domain.RunID {
		digest, err := execution.Digest(def.ID, string(def.Digest), p.Item, rev, CommitA, snap)
		if err != nil {
			e.T.Fatal(err)
		}
		e.OwnerExec(`INSERT INTO runs (tenant_id, project_id, id, purpose, work_item_id, workspace_id, definition_id, definition_digest, content_revision,
			specification, source_commit, inputs, snapshot_digest, source_retention, retained_ref_id, requested_by_type, requested_by_id,
			status, status_reason, version, created_at, updated_at, started_at, finished_at)
			VALUES ($1,$2,$3,'work',$4,$5,$6,$7,$8,$9,$10,$11,$12,'confirmed',$13,'user',$14,$15,$16,1,$17,$17,$18,$19)`,
			string(ScopeA.TenantID), string(ScopeA.ProjectID), id, string(p.Item), string(p.Workspace), string(def.ID), string(def.Digest), int64(rev), spec,
			CommitA, snapJSON, digest, "pin_"+id, Engineer, status, reason, now, started, finished)
		return domain.RunID(id)
	}
	insertStep := func(run domain.RunID, status, reason string, attempts int, finished *time.Time) {
		st := execution.NewStepRun(run, 0, doc.Spec.Steps[0], now)
		raw, err := execution.EncodeStep(st.Spec)
		if err != nil {
			e.T.Fatal(err)
		}
		e.OwnerExec(`INSERT INTO step_runs (tenant_id, project_id, run_id, step_id, ordinal, spec, action_key, status, reason, attempts_started, max_attempts, backoff_seconds,
			retry_on, outputs, version, created_at, updated_at, finished_at) VALUES ($1,$2,$3,'job',0,$4,$5,$6,$7,$8,1,0,'{}','{}',1,$9,$9,$10)`,
			string(ScopeA.TenantID), string(ScopeA.ProjectID), string(run), raw, st.ActionKey, status, reason, attempts, now, finished)
	}
	insertAttempt := func(run domain.RunID, id, status, reason string, settled bool) {
		step := execution.NewStepRun(run, 0, doc.Spec.Steps[0], now)
		c := runtimev1.Context{
			TenantID: ScopeA.TenantID, ProjectID: ScopeA.ProjectID, RunID: run, StepID: "job", AttemptID: domain.AttemptID(id), ActionKey: step.ActionKey,
			Definition: runtimev1.Definition{ID: def.ID, Digest: runtimev1.Digest(def.Digest)},
			Command:    runtimev1.Command{Image: SharedImage, Command: []string{"/bin/true"}, Args: []string{}, Env: map[string]string{}},
			Workspace:  runtimev1.Workspace{Access: runtimev1.AccessRead, Source: &runtimev1.Source{RepositoryURL: "https://git.example.invalid/a/b.git", Commit: CommitA}},
			Inputs:     map[string]runtimev1.Value{}, Outputs: []runtimev1.OutputDecl{}, Secrets: []runtimev1.SecretBinding{},
			Limits: runtimev1.Limits{TimeoutSeconds: 60, MaxOutputBytes: 1 << 20, MaxOutputFiles: 100, MaxDiagnosticBytes: 1 << 20},
		}
		plan, err := execution.DefaultPlanner{}.Plan(ScopeA.TenantID, c)
		if err != nil {
			e.T.Fatal(err)
		}
		var finished *time.Time
		var result, resultHash any
		if status != "running" {
			finished = &now
			if status == "succeeded" {
				zero := int64(0)
				res := runtimev1.Result{Outcome: runtimev1.OutcomeCompleted, ExitCode: &zero, Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}}
				h, _ := runtimev1.ResultHash(res)
				b, _ := json.Marshal(res)
				result, resultHash = b, string(h)
			}
		}
		e.OwnerExec(`INSERT INTO step_attempts (tenant_id, project_id, id, run_id, step_id, ordinal, action_key, status, reason, job_name, spec_fingerprint, workspace_id, access,
			input_commit, timeout_seconds, startup_deadline_at, context_digest, context, started_at, finished_at, result_hash, result, authority_revision, version, created_at, updated_at)
			VALUES ($1,$2,$3,$4,'job',1,$5,$6,$7,$8,$9,$10,'read',$11,60,$12,$13,$14,$15,$16,$17,$18,1,1,$15,$15)`,
			string(ScopeA.TenantID), string(ScopeA.ProjectID), id, string(run), step.ActionKey, status, reason, plan.WorkloadName, plan.SpecFingerprint, string(p.Workspace),
			CommitA, now.Add(time.Hour), plan.ContextDigest, string(plan.Context), now, finished, resultHash, result)
		var settledAt *time.Time
		if settled {
			settledAt = &now
		}
		e.OwnerExec(`INSERT INTO attempt_controls (tenant_id, project_id, attempt_id, run_id, dispatch_confirmed_at, termination_confirmed_at, launch, effect, settled_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,'started',$7,$8,$5)`, string(ScopeA.TenantID), string(ScopeA.ProjectID), id, string(run), now,
			func() *time.Time {
				if settled {
					return &now
				}
				return nil
			}(), func() string {
				if settled {
					return "settled"
				}
				return "possible"
			}(), settledAt)
	}
	out := LegacyRuns{Project: p, Def: def}
	out.Finished = insertRun("run_hist_finished", "succeeded", "completed", &now, &now)
	insertStep(out.Finished, "succeeded", "completed", 1, &now)
	insertAttempt(out.Finished, "att_hist_finished", "succeeded", "completed", true)
	out.Queued = insertRun("run_hist_queued", "queued", "created", nil, nil)
	insertStep(out.Queued, "pending", "created", 0, nil)
	out.Running = insertRun("run_hist_running", "running", "running", &now, nil)
	insertStep(out.Running, "running", "running", 1, nil)
	insertAttempt(out.Running, "att_hist_running", "running", "starting", false)
	return out
}
