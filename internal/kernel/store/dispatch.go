package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/optimiware/kodama/internal/domain"
)

// ---- outbox -----------------------------------------------------------------

// OutboxEvent is one audit_events row that is, or was, waiting to be published.
// TenantKey and ProjectKey name the event's subject; ProjectKey is empty for a
// tenant-wide event.
type OutboxEvent struct {
	TenantID     domain.TenantID
	ProjectID    domain.ProjectID
	TenantKey    string
	ProjectKey   string
	ID           string
	ActorType    string
	ActorID      string
	Type         string
	ResourceType string
	ResourceID   string
	Payload      json.RawMessage
	CreatedAt    time.Time
	Attempts     int
}

// OutboxBackend is the persistence the publisher and the dispatcher use: the
// PostgreSQL *Store implements it, and MemoryGit implements it for unit tests
// with the same semantics.
type OutboxBackend interface {
	// PendingEvents lists up to limit unpublished events of one tenant whose
	// next attempt is due at now, ordered by next attempt and ID.
	PendingEvents(ctx context.Context, tenant domain.TenantID, now time.Time, limit int) ([]OutboxEvent, error)
	// MarkPublished records a successful publication; it is idempotent and never
	// moves an already published event.
	MarkPublished(ctx context.Context, tenant domain.TenantID, id string, at time.Time) error
	// PublishFailed counts a failed attempt and defers the next one to next. The
	// event stays in the outbox.
	PublishFailed(ctx context.Context, tenant domain.TenantID, id string, next time.Time) error
}

var _ OutboxBackend = (*Store)(nil)

func (s *Store) PendingEvents(ctx context.Context, tenant domain.TenantID, now time.Time, limit int) ([]OutboxEvent, error) {
	var out []OutboxEvent
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT e.id, coalesce(e.project_id,''), e.actor_type, e.actor_id, e.event_type, e.resource_type,
			e.resource_id, e.payload::jsonb, e.created_at, e.publish_attempts, t.key, coalesce(p.key,'')
			FROM audit_events e
			JOIN tenants t ON t.id = e.tenant_id
			LEFT JOIN projects p ON p.tenant_id = e.tenant_id AND p.id = e.project_id
			WHERE e.tenant_id = $1 AND e.published_at IS NULL AND e.next_attempt_at <= $2
			ORDER BY e.next_attempt_at, e.id LIMIT $3`, tenant, now, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e := OutboxEvent{TenantID: tenant}
			var project string
			if err := rows.Scan(&e.ID, &project, &e.ActorType, &e.ActorID, &e.Type, &e.ResourceType, &e.ResourceID,
				&e.Payload, &e.CreatedAt, &e.Attempts, &e.TenantKey, &e.ProjectKey); err != nil {
				return err
			}
			e.ProjectID = domain.ProjectID(project)
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, translate(err)
}

func (s *Store) MarkPublished(ctx context.Context, tenant domain.TenantID, id string, at time.Time) error {
	return translate(s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE audit_events SET published_at = $3
			WHERE tenant_id = $1 AND id = $2 AND published_at IS NULL`, tenant, id, at)
		return err
	}))
}

func (s *Store) PublishFailed(ctx context.Context, tenant domain.TenantID, id string, next time.Time) error {
	return translate(s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE audit_events SET publish_attempts = publish_attempts + 1, next_attempt_at = $3
			WHERE tenant_id = $1 AND id = $2 AND published_at IS NULL`, tenant, id, next)
		return err
	}))
}

// ---- pipeline indexing and trigger admission ---------------------------------

// Event types the dispatcher records as audit rows.
const (
	// EventPipelinesIndexed is the marker that one git.refs_updated event was
	// dispatched: its deterministic ID makes the whole dispatch idempotent.
	EventPipelinesIndexed = "git.pipelines_indexed"
	// EventPipelineRefused records a definition that was not indexed and why.
	EventPipelineRefused = "pipeline.index_refused"
)

// DispatchMarkerID is the deterministic audit_events ID of the marker of one
// dispatched event.
func DispatchMarkerID(eventID string) string { return "dx_" + digest("marker", eventID) }

func refusalID(eventID, path string) string { return "dr_" + digest("refusal", eventID, path) }

func digest(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:20])
}

// IndexedDefinition is one valid .kodama/<name>.yaml of the pushed commit:
// Definition is the canonical document.
type IndexedDefinition struct {
	Name       string
	Definition json.RawMessage
}

// IndexRefusal is one file that was not indexed. Code is a stable diagnostic
// code and DocumentPath the place in the document; neither echoes content.
type IndexRefusal struct {
	Path         string `json:"path"`
	Code         string `json:"code"`
	DocumentPath string `json:"documentPath,omitempty"`
}

// PushIndex is what the dispatcher read at the pushed commit of the default
// branch. Present lists the process name of every file there, valid or not: a
// pipeline whose file is gone is disabled, one whose file is invalid keeps its
// previous definition. Unreadable is set when the files could not be read at all
// (the commit's files are refused as a whole and nothing is disabled).
type PushIndex struct {
	Ref         string
	Commit      string
	Definitions []IndexedDefinition
	Refusals    []IndexRefusal
	Present     []string
	Unreadable  bool
}

// RunAdmission is one run the planner decided to start.
type RunAdmission struct {
	PipelineID string
	// TriggerKey is the (push event, ref) of the run: with the pipeline it is
	// unique, so a second admission of it starts nothing.
	TriggerKey string
	Run        Run
	Steps      []Step
	Sources    json.RawMessage
}

// Planner decides, inside the dispatch transaction and from the pipelines in
// force after indexing, which runs the event starts.
type Planner func(pipelines []Pipeline) ([]RunAdmission, error)

// PushDispatch is one git.refs_updated event to dispatch.
type PushDispatch struct {
	TenantID     domain.TenantID
	RepositoryID string
	// EventID identifies the event. JournalPosition is the receive's position in
	// the worker's journal (the event's journalPosition; 0: unknown): the order
	// in which pushes were applied, which decides which default-branch push's
	// definitions are the newer. The time an event was committed or reported
	// never does: a lost report is recovered after newer pushes.
	EventID         string
	JournalPosition int64
	ReceiveID       string
	// Index is set when the push moved the default branch.
	Index *PushIndex
	// Plan selects the runs; nil admits none.
	Plan Planner
}

// PushDispatchResult reports one dispatch. Replayed is true when the event was
// dispatched before (nothing changed); Stale when a default-branch push applied
// after this one had already been indexed, so this one's definitions were left
// alone; Archived when the repository was archived, so nothing was done (and no
// marker written: the event can be dispatched after a restore).
type PushDispatchResult struct {
	Archived bool
	Replayed bool
	Skipped  bool
	Stale    bool
	Indexed  []string
	Disabled []string
	Refused  int
	Runs     []string
}

type dispatchMarker struct {
	RepositoryID    string   `json:"repositoryId"`
	ReceiveID       string   `json:"receiveId"`
	EventID         string   `json:"eventId"`
	JournalPosition int64    `json:"journalPosition,omitempty"`
	Commit          string   `json:"commit,omitempty"`
	Indexed         bool     `json:"indexed"`
	Stale           bool     `json:"stale,omitempty"`
	Pipelines       []string `json:"pipelines"`
	Disabled        []string `json:"disabled"`
	Refused         int      `json:"refused"`
	Runs            []string `json:"runs"`
}

// DispatchBackend is the persistence of indexing and admission.
type DispatchBackend interface {
	// Dispatched reports whether the event already has its dispatch marker: a
	// cheap check that spares a replay the file read. DispatchPush's own check
	// under the row lock is the authority.
	Dispatched(ctx context.Context, tenant domain.TenantID, eventID string) (bool, error)
	// DispatchPush indexes the pushed definitions and admits the runs the event
	// starts, in one transaction, exactly once per event.
	DispatchPush(ctx context.Context, d PushDispatch) (PushDispatchResult, error)
}

var _ DispatchBackend = (*Store)(nil)

// DispatchPush applies one git.refs_updated event in one transaction under the
// repository row lock:
//
//  0. an archived repository is left alone, checked under the row lock after the
//     worker read, so an archive that raced the read leaves no index and no run;
//  1. an event that already has its marker is a replay and changes nothing;
//  2. when the push moved the default branch and no default-branch push applied
//     after it (by journal position) was indexed, each valid definition is upserted (source git, source_commit the
//     pushed commit), a pipeline whose file is gone is disabled, and each refused
//     file is recorded with its reason, leaving the previous definition in place;
//  3. the planner selects runs from the pipelines in force, and each is inserted
//     under (pipeline, trigger key), so a second admission starts nothing;
//  4. the marker is written with what was done.
//
// Any failure rolls all of it back, so indexing and admission are atomic.
func (s *Store) Dispatched(ctx context.Context, tenant domain.TenantID, eventID string) (bool, error) {
	var done bool
	err := s.inTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM audit_events WHERE tenant_id=$1 AND id=$2)`,
			tenant, DispatchMarkerID(eventID)).Scan(&done)
	})
	return done, translate(err)
}

func (s *Store) DispatchPush(ctx context.Context, d PushDispatch) (PushDispatchResult, error) {
	var res PushDispatchResult
	err := s.inTenant(ctx, d.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		res = PushDispatchResult{}
		var project *string
		var status string
		if err := tx.QueryRow(ctx, `SELECT project_id, status FROM repositories WHERE tenant_id=$1 AND id=$2 FOR UPDATE`,
			d.TenantID, d.RepositoryID).Scan(&project, &status); err != nil {
			return err
		}
		if project == nil { // the tenant-level repository has no pipelines
			res.Skipped = true
			return nil
		}
		if status == RepoArchived {
			res.Archived = true
			return nil
		}
		proj := domain.ProjectID(*project)
		var done bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM audit_events WHERE tenant_id=$1 AND id=$2)`,
			d.TenantID, DispatchMarkerID(d.EventID)).Scan(&done); err != nil {
			return err
		}
		if done {
			res.Replayed = true
			return nil
		}
		marker := dispatchMarker{RepositoryID: d.RepositoryID, ReceiveID: d.ReceiveID, EventID: d.EventID,
			JournalPosition: d.JournalPosition, Pipelines: []string{}, Disabled: []string{}, Runs: []string{}}

		if ix := d.Index; ix != nil {
			marker.Commit = ix.Commit
			if d.JournalPosition > 0 {
				if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM audit_events
					WHERE tenant_id=$1 AND project_id=$2 AND resource_type='repository' AND resource_id=$3 AND event_type=$4
					AND payload::jsonb->>'indexed' = 'true'
					AND (payload::jsonb->>'journalPosition')::bigint > $5)`,
					d.TenantID, proj, d.RepositoryID, EventPipelinesIndexed, d.JournalPosition).Scan(&res.Stale); err != nil {
					return err
				}
			}
			marker.Stale = res.Stale
			if !res.Stale {
				marker.Indexed = true
				if err := indexTx(ctx, tx, d, proj, ix, &res); err != nil {
					return err
				}
				marker.Pipelines, marker.Disabled, marker.Refused = nonNil(res.Indexed), nonNil(res.Disabled), res.Refused
			}
		}

		if d.Plan != nil {
			current, err := repositoryPipelinesTx(ctx, tx, d.TenantID, proj, d.RepositoryID)
			if err != nil {
				return err
			}
			plan, err := d.Plan(current)
			if err != nil {
				return err
			}
			for _, a := range plan {
				created, err := admitTx(ctx, tx, d.TenantID, proj, a)
				if err != nil {
					return err
				}
				if created {
					res.Runs = append(res.Runs, a.Run.ID)
				}
			}
			marker.Runs = nonNil(res.Runs)
		}

		payload, err := json.Marshal(marker)
		if err != nil {
			return err
		}
		return appendEventTx(ctx, tx, d.TenantID, proj, DispatchMarkerID(d.EventID), "system", "kernel-dispatch",
			EventPipelinesIndexed, "repository", d.RepositoryID, payload)
	})
	return res, translate(err)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func indexTx(ctx context.Context, tx pgx.Tx, d PushDispatch, project domain.ProjectID, ix *PushIndex, res *PushDispatchResult) error {
	for _, def := range ix.Definitions {
		id, err := newID("pl")
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO pipelines (tenant_id,project_id,id,repo_id,name,definition,source,source_commit)
			VALUES ($1,$2,$3,$4,$5,$6::jsonb,'git',$7)
			ON CONFLICT (tenant_id,project_id,repo_id,name) DO UPDATE
			SET definition=excluded.definition, source='git', source_commit=excluded.source_commit, disabled_at=NULL,
			    version=pipelines.version+1, updated_at=now()`,
			d.TenantID, project, id, d.RepositoryID, def.Name, string(def.Definition), ix.Commit); err != nil {
			return err
		}
		res.Indexed = append(res.Indexed, def.Name)
	}
	if !ix.Unreadable {
		rows, err := tx.Query(ctx, `UPDATE pipelines SET disabled_at=now(), version=version+1, updated_at=now()
			WHERE tenant_id=$1 AND project_id=$2 AND repo_id=$3 AND source='git' AND disabled_at IS NULL
			AND NOT (name = ANY($4::text[])) RETURNING name`, d.TenantID, project, d.RepositoryID, nonNil(ix.Present))
		if err != nil {
			return err
		}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return err
			}
			res.Disabled = append(res.Disabled, name)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	for _, r := range ix.Refusals {
		payload, err := json.Marshal(map[string]any{"repositoryId": d.RepositoryID, "ref": ix.Ref, "commit": ix.Commit,
			"path": r.Path, "code": r.Code, "documentPath": nullIfEmpty(r.DocumentPath)})
		if err != nil {
			return err
		}
		if err := appendEventTx(ctx, tx, d.TenantID, project, refusalID(d.EventID, r.Path), "system", "kernel-dispatch",
			EventPipelineRefused, "repository", d.RepositoryID, payload); err != nil {
			return err
		}
		res.Refused++
	}
	return nil
}

func repositoryPipelinesTx(ctx context.Context, tx pgx.Tx, tenant domain.TenantID, project domain.ProjectID, repositoryID string) ([]Pipeline, error) {
	rows, err := tx.Query(ctx, `SELECT id,name,source,definition::jsonb,version,coalesce(source_commit,''),created_at,updated_at
		FROM pipelines WHERE tenant_id=$1 AND project_id=$2 AND repo_id=$3 AND source='git' AND disabled_at IS NULL
		ORDER BY name,id`, tenant, project, repositoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pipeline
	for rows.Next() {
		p := Pipeline{TenantID: tenant, ProjectID: project, RepositoryID: repositoryID}
		if err := rows.Scan(&p.ID, &p.Name, &p.Source, &p.Definition, &p.Version, &p.SourceCommit, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// admitTx inserts a run and its steps unless the (pipeline, trigger key) already
// has one; created reports whether it did.
func admitTx(ctx context.Context, tx pgx.Tx, tenant domain.TenantID, project domain.ProjectID, a RunAdmission) (bool, error) {
	if a.TriggerKey == "" {
		return false, fmt.Errorf("%w: a push-started run needs a trigger key", domain.ErrInvalidValue)
	}
	sources := string(a.Sources)
	if sources == "" {
		sources = "[]"
	}
	tag, err := tx.Exec(ctx, `INSERT INTO runs
		(tenant_id,project_id,id,pipeline_id,definition_snapshot,inputs,sources,trigger_key,execution_order)
		VALUES ($1,$2,$3,$4,$5::jsonb,$6::jsonb,$7::jsonb,$8,$9)
		ON CONFLICT (tenant_id,project_id,pipeline_id,trigger_key) DO NOTHING`,
		tenant, project, a.Run.ID, a.PipelineID, string(a.Run.Definition), string(orEmptyObject(a.Run.Inputs)), sources, a.TriggerKey, a.Run.ExecutionOrder)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	for _, st := range a.Steps {
		if _, err := tx.Exec(ctx, `INSERT INTO run_steps (tenant_id,project_id,run_id,id,step_key,ordinal,image)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`, tenant, project, a.Run.ID, st.ID, st.StepKey, st.Ordinal, st.Image); err != nil {
			return false, err
		}
	}
	return true, nil
}

func orEmptyObject(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	return raw
}
