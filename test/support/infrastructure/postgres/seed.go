package pgtest

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
)

// FixtureTime is the deterministic timestamp fixtures use.
var FixtureTime = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

// Scope returns a project scope.
func Scope(tenant, project string) tenancy.Scope {
	return tenancy.Scope{TenantID: id.TenantID(tenant), ProjectID: id.ProjectID(project)}
}

// TenantScope returns a tenant-wide scope.
func TenantScope(tenant string) tenancy.Scope {
	return tenancy.Scope{TenantID: id.TenantID(tenant)}
}

// SeedTenant inserts one tenant row.
func (s *Shard) SeedTenant(ctx context.Context, tenant string, version int64) {
	s.T.Helper()
	key, _ := json.Marshal(token.TenantKey{
		TenantID: id.TenantID(tenant), Version: uint64(version),
		Ciphertext: []byte("ciphertext"), WrappedKey: []byte("wrapped-key"),
		WrapNonce: []byte("wrap-nonce"), KeyRef: "kms/tenant-key", KeyVersion: 1,
	})
	s.exec(ctx, `INSERT INTO tenants (id, name, version, derivation_key, created_at) VALUES ($1,$2,$3,$4::jsonb,$5)`,
		tenant, "Tenant "+tenant, version, string(key), FixtureTime)
}

// SeedProject inserts one project row.
func (s *Shard) SeedProject(ctx context.Context, tenant, project string) {
	s.T.Helper()
	s.exec(ctx, `INSERT INTO projects (id, tenant_id, name, version, created_at) VALUES ($1,$2,$3,1,$4)`,
		project, tenant, "Project "+project, FixtureTime)
}

// SeedScope inserts a tenant and project.
func (s *Shard) SeedScope(ctx context.Context, tenant, project string) {
	s.T.Helper()
	s.SeedTenant(ctx, tenant, 1)
	s.SeedProject(ctx, tenant, project)
}

// SeedMembership inserts one membership row.
func (s *Shard) SeedMembership(ctx context.Context, scope tenancy.Scope, id, principal string, version int64, roles ...string) {
	s.T.Helper()
	encoded, _ := json.Marshal(roles)
	s.exec(ctx, `INSERT INTO memberships (id, tenant_id, project_id, principal_id, roles, version, created_at)
		VALUES ($1,$2,$3,$4,$5::jsonb,$6,$7)`,
		id, scope.TenantID, NullString(string(scope.ProjectID)), principal, string(encoded), version, FixtureTime)
}

// SeedService inserts one service row.
func (s *Shard) SeedService(ctx context.Context, scope tenancy.Scope, id, name string, roles ...string) {
	s.T.Helper()
	encoded, _ := json.Marshal(roles)
	s.exec(ctx, `INSERT INTO services (id, tenant_id, project_id, name, state, roles, token_nonce, token_expires_at, version, created_at)
		VALUES ($1,$2,$3,$4,'enabled',$5::jsonb,NULL,NULL,1,$6)`,
		id, scope.TenantID, NullString(string(scope.ProjectID)), name, string(encoded), FixtureTime)
}

// SeedBinding inserts one resource binding row.
func (s *Shard) SeedBinding(ctx context.Context, scope tenancy.Scope, id, resourceID, serviceID string) {
	s.T.Helper()
	s.exec(ctx, `INSERT INTO resource_bindings (id, tenant_id, project_id, resource_id, service_id, origins, operations, reportable_events, metadata, version, created_at)
		VALUES ($1,$2,$3,$4,$5,'[]'::jsonb,'[]'::jsonb,'[]'::jsonb,'{}'::jsonb,1,$6)`,
		id, scope.TenantID, scope.ProjectID, resourceID, serviceID, FixtureTime)
}

// SeedPipeline inserts one pipeline row.
func (s *Shard) SeedPipeline(ctx context.Context, scope tenancy.Scope, id, name string, yaml []byte, version int64, pushTriggers string) {
	s.T.Helper()
	if pushTriggers == "" {
		pushTriggers = "[]"
	}
	digest := sha256.Sum256(yaml)
	s.exec(ctx, `INSERT INTO pipelines (id, tenant_id, project_id, name, yaml, yaml_sha256, push_triggers, version, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$9)`,
		id, scope.TenantID, scope.ProjectID, name, yaml, digest[:], pushTriggers, version, FixtureTime)
}

// SeedResource inserts one resources row.
func (s *Shard) SeedResource(ctx context.Context, scope tenancy.Scope, id string) {
	s.T.Helper()
	s.exec(ctx, `INSERT INTO resources (id, tenant_id, project_id, kind, version, metadata, created_at)
		VALUES ($1,$2,$3,'repository',1,'{}'::jsonb,$4)`,
		id, scope.TenantID, scope.ProjectID, FixtureTime)
}

// SeedTask inserts one task row with the given state and lifecycle version.
func (s *Shard) SeedTask(ctx context.Context, scope tenancy.Scope, id, state string, lifecycleVersion int64, pipelineID string) {
	s.T.Helper()
	s.exec(ctx, `INSERT INTO tasks (id, tenant_id, project_id, content_version, lifecycle_version, state, pipeline_id,
		current_execution_id, initiator_id, title, description, metadata, inputs, execution_history, created_at)
		VALUES ($1,$2,$3,1,$4,$5,$6,NULL,$7,$8,NULL,'{}'::jsonb,'{}'::jsonb,'[]'::jsonb,$9)`,
		id, scope.TenantID, scope.ProjectID, lifecycleVersion, state, NullString(pipelineID), "principal-1", "Task "+id, FixtureTime)
}

// SeedExecution inserts one execution row. taskID and pipelineID may be empty.
func (s *Shard) SeedExecution(ctx context.Context, scope tenancy.Scope, id, state string, version int64, taskID, pipelineID string) {
	s.T.Helper()
	digest := sha256.Sum256([]byte("snapshot-" + id))
	s.exec(ctx, `INSERT INTO executions (id, tenant_id, project_id, pipeline_id, task_id, principal_id, state, version,
		yaml_sha256, snapshot, token_nonce, token_expires_at, workspace_id, created_at)
		VALUES ($1,$2,$3,$4,$5,'principal-1',$6,$7,$8,'{}'::jsonb,NULL,NULL,$9,$10)`,
		id, scope.TenantID, scope.ProjectID, NullString(pipelineID), NullString(taskID), state, version,
		digest[:], "workspace-"+id, FixtureTime)
}

// SeedAction inserts one container action row.
func (s *Shard) SeedAction(ctx context.Context, scope tenancy.Scope, id, executionID string, index int, state string, version int64) {
	s.T.Helper()
	s.SeedActionNamed(ctx, scope, id, executionID, "build", index, state, version)
}

// SeedActionNamed inserts one container action row with an explicit name.
func (s *Shard) SeedActionNamed(ctx context.Context, scope tenancy.Scope, id, executionID, name string, index int, state string, version int64) {
	s.T.Helper()
	s.exec(ctx, `INSERT INTO actions (id, execution_id, tenant_id, project_id, index, name, kind, state, version,
		timeout_ms, deadline, outputs, staged_outputs, staged_outputs_set, stop, request_subject, request_assignment,
		request_decision, request_actor, request_resolved_at, workload_id, launch_state, launch_key, observation,
		activated_at, answer_provenance)
		VALUES ($1,$2,$3,$4,$5,$6,'container',$7,$8,60000,NULL,NULL,NULL,false,NULL,NULL,NULL,NULL,NULL,NULL,NULL,'not_issued',NULL,NULL,NULL,NULL)`,
		id, executionID, scope.TenantID, scope.ProjectID, index, name, state, version)
}

// SeedCleanup inserts one cleanup row.
func (s *Shard) SeedCleanup(ctx context.Context, scope tenancy.Scope, id, executionID, state string) {
	s.T.Helper()
	s.exec(ctx, `INSERT INTO cleanup (id, tenant_id, project_id, execution_id, workload_id, workspace_id, state, attempts, last_error, next_retry_at)
		VALUES ($1,$2,$3,$4,NULL,NULL,$5,0,NULL,NULL)`,
		id, scope.TenantID, scope.ProjectID, executionID, state)
}

// SeedTaskMessage inserts one task message row.
func (s *Shard) SeedTaskMessage(ctx context.Context, scope tenancy.Scope, id, taskID, author, body string) {
	s.T.Helper()
	s.exec(ctx, `INSERT INTO task_messages (id, task_id, tenant_id, project_id, author_id, at, body, refs)
		VALUES ($1,$2,$3,$4,$5,$6,$7,NULL)`,
		id, taskID, scope.TenantID, scope.ProjectID, author, FixtureTime, body)
}

// SeedVerification inserts one verification row.
func (s *Shard) SeedVerification(ctx context.Context, scope tenancy.Scope, id, bindingID, resourceID string) {
	s.T.Helper()
	s.exec(ctx, `INSERT INTO verifications (id, tenant_id, project_id, service_id, actor_id, execution_id, action_id,
		binding_id, resource_id, operation, subject, requested_approval, result, approved, created_at)
		VALUES ($1,$2,$3,'service-1','principal-1',NULL,NULL,$4,$5,'git.ref.update','{}'::jsonb,NULL,'approved',true,$6)`,
		id, scope.TenantID, scope.ProjectID, bindingID, resourceID, FixtureTime)
}

// SeedReceipt inserts one completed receipt row.
func (s *Shard) SeedReceipt(ctx context.Context, scope tenancy.Scope, id, principal, operation, callerKey, content, result string) {
	s.T.Helper()
	contentHash := sha256.Sum256([]byte(content))
	s.exec(ctx, `INSERT INTO receipts (id, tenant_id, project_id, principal_id, operation, caller_key, content_sha256,
		result, completed_at, execution_id, snapshot_sha256, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NULL,NULL,$9)`,
		id, scope.TenantID, NullString(string(scope.ProjectID)), principal, operation, callerKey,
		contentHash[:], []byte(result), FixtureTime)
}

// SeedEvent inserts one pending event row.
func (s *Shard) SeedEvent(ctx context.Context, scope tenancy.Scope, id, eventType, subject string, version int64) {
	s.T.Helper()
	s.exec(ctx, `INSERT INTO events (id, tenant_id, project_id, type, report_type, subject_id, subject_version,
		source_kind, source_principal, correlation_id, causation_id, at, data, publish_state)
		VALUES ($1,$2,$3,$4,NULL,$5,$6,'shell','principal-1',NULL,NULL,$7,NULL,'pending')`,
		id, scope.TenantID, NullString(string(scope.ProjectID)), eventType, subject, version, FixtureTime)
}

// SeedAudit inserts one audit row.
func (s *Shard) SeedAudit(ctx context.Context, scope tenancy.Scope, id, operation string) {
	s.T.Helper()
	s.exec(ctx, `INSERT INTO audit (id, tenant_id, project_id, actor, operation, at, detail)
		VALUES ($1,$2,$3,'principal-1',$4,$5,NULL)`,
		id, scope.TenantID, NullString(string(scope.ProjectID)), operation, FixtureTime)
}

// SeedSecret inserts one stored secret envelope with deterministic bytes.
func (s *Shard) SeedSecret(ctx context.Context, scope tenancy.Scope, id, name string, version int64) {
	s.T.Helper()
	ciphertext := []byte("ciphertext")
	wrappedKey := make([]byte, 48)
	wrapNonce := make([]byte, 12)
	nonce := make([]byte, 12)
	for index := range wrappedKey {
		wrappedKey[index] = 7
	}
	for index := range wrapNonce {
		wrapNonce[index] = 8
	}
	for index := range nonce {
		nonce[index] = 9
	}
	s.exec(ctx, `INSERT INTO secrets (id, tenant_id, project_id, name, ciphertext, wrapped_key, wrap_nonce,
		key_ref, key_version, nonce, version, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'kms/secret-key',3,$8,$9,$10,$10)`,
		id, scope.TenantID, NullString(string(scope.ProjectID)), name, ciphertext, wrappedKey, wrapNonce, nonce, version, FixtureTime)
}

// SeedExecutionCredential installs a live token pair on one execution row.
func (s *Shard) SeedExecutionCredential(ctx context.Context, id string, nonce []byte, expiresAt time.Time) {
	s.T.Helper()
	s.exec(ctx, `UPDATE executions SET token_nonce = $2, token_expires_at = $3 WHERE id = $1`,
		id, nonce, expiresAt)
}

// SeedServiceCredential installs a live token pair on one service row.
func (s *Shard) SeedServiceCredential(ctx context.Context, id string, nonce []byte, expiresAt time.Time) {
	s.T.Helper()
	s.exec(ctx, `UPDATE services SET token_nonce = $2, token_expires_at = $3 WHERE id = $1`,
		id, nonce, expiresAt)
}

// SeedRunningAction inserts one running container action with a deadline.
func (s *Shard) SeedRunningAction(ctx context.Context, scope tenancy.Scope, id, executionID string, index int, deadline time.Time) {
	s.T.Helper()
	s.exec(ctx, `INSERT INTO actions (id, execution_id, tenant_id, project_id, index, name, kind, state, version,
		timeout_ms, deadline, outputs, staged_outputs, staged_outputs_set, stop, request_subject, request_assignment,
		request_decision, request_actor, request_resolved_at, workload_id, launch_state, launch_key, observation,
		activated_at, answer_provenance)
		VALUES ($1,$2,$3,$4,$5,'build','container','running',1,60000,$6,NULL,NULL,false,NULL,NULL,NULL,NULL,NULL,NULL,NULL,'not_issued',NULL,NULL,NULL,NULL)`,
		id, executionID, scope.TenantID, scope.ProjectID, index, deadline)
}

// SeedRequestAction inserts one running request action with a durable
// assignment and (optionally) a resolved subject/decision.
func (s *Shard) SeedRequestAction(ctx context.Context, scope tenancy.Scope, id, executionID, name, state string, index int,
	assignment, subject []byte, decision, actor string, resolvedAt time.Time) {
	s.T.Helper()
	var subjectValue, decisionValue, actorValue, resolvedValue any
	if subject != nil {
		subjectValue = subject
	}
	if decision != "" {
		decisionValue = decision
		actorValue = actor
		resolvedValue = resolvedAt
	}
	s.exec(ctx, `INSERT INTO actions (id, execution_id, tenant_id, project_id, index, name, kind, state, version,
		timeout_ms, deadline, outputs, staged_outputs, staged_outputs_set, stop, request_subject, request_assignment,
		request_decision, request_actor, request_resolved_at, workload_id, launch_state, launch_key, observation,
		activated_at, answer_provenance)
		VALUES ($1,$2,$3,$4,$5,$6,'request',$7,1,60000,NULL,NULL,NULL,false,NULL,$8,$9,$10,$11,$12,NULL,'not_issued',NULL,NULL,NULL,NULL)`,
		id, executionID, scope.TenantID, scope.ProjectID, index, name, state, subjectValue, assignment, decisionValue, actorValue, resolvedValue)
}

// SeedWaitingTask inserts one waiting task whose question is assigned.
func (s *Shard) SeedWaitingTask(ctx context.Context, scope tenancy.Scope, id string, lifecycleVersion int64, assignment []byte) {
	s.T.Helper()
	s.exec(ctx, `INSERT INTO tasks (id, tenant_id, project_id, content_version, lifecycle_version, state, pipeline_id,
		current_execution_id, initiator_id, title, description, metadata, inputs, execution_history,
		question_id, question_body, question_assignment, question_version, question_asked_by, question_at, created_at)
		VALUES ($1,$2,$3,1,$4,'waiting',NULL,NULL,'principal-1','Task','', '{}'::jsonb,'{}'::jsonb,'[]'::jsonb,
		'question-1','Which branch?',$5,1,'principal-1',$6,$6)`,
		id, scope.TenantID, scope.ProjectID, lifecycleVersion, assignment, FixtureTime)
}

// SeedFailedTask inserts one open task whose current execution failed.
func (s *Shard) SeedFailedTask(ctx context.Context, scope tenancy.Scope, id string, lifecycleVersion int64, executionID string) {
	s.T.Helper()
	s.exec(ctx, `INSERT INTO tasks (id, tenant_id, project_id, content_version, lifecycle_version, state, pipeline_id,
		current_execution_id, initiator_id, title, description, metadata, inputs, execution_history, created_at)
		VALUES ($1,$2,$3,1,$4,'open',NULL,$5,'principal-1','Task','', '{}'::jsonb,'{}'::jsonb,'[]'::jsonb,$6)`,
		id, scope.TenantID, scope.ProjectID, lifecycleVersion, executionID, FixtureTime)
}

// TruncateAll removes every row from every declared table. It runs as the
// administrator so fixture setup is independent of row-level security.
func (s *Shard) TruncateAll(ctx context.Context) {
	s.T.Helper()
	tables := make([]string, 0, len(store.SchemaDesign().Tables))
	for _, table := range store.SchemaDesign().Tables {
		tables = append(tables, table.Name)
	}
	statement := "TRUNCATE " + joinNames(tables) + " CASCADE"
	s.exec(ctx, statement)
}

func (s *Shard) exec(ctx context.Context, statement string, args ...any) {
	s.T.Helper()
	if _, err := ExecSQL(ctx, s.Admin(), statement, args...); err != nil {
		s.T.Fatalf("seed statement failed: %v%s", err, CauseText(err))
	}
}

func joinNames(names []string) string {
	var out strings.Builder
	for index, name := range names {
		if index > 0 {
			out.WriteString(", ")
		}
		out.WriteString(name)
	}
	return out.String()
}

// Fingerprint returns the content hash form the receipt adapter expects.
func Fingerprint(content string) string {
	sum := sha256.Sum256([]byte(content))
	return fmt.Sprintf("sha256:%x", sum[:])
}
