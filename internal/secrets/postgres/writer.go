package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/secrets"
)

// writer is the secrets.Writer bound to one tenant transaction. It never begins,
// commits or rolls back the transaction and makes no network call.
type writer struct {
	s      *Store
	tx     pgx.Tx
	tenant domain.TenantID
}

var _ secrets.Writer = (*writer)(nil)

// ---- audit and events ------------------------------------------------------

type auditEntry struct {
	Scope           domain.Scope // ProjectID may be empty for tenant-wide entries
	Action          string
	ResourceType    string
	ResourceID      string
	ResourceVersion int64
	Metadata        map[string]any
}

// audit appends one safe-metadata row. Metadata must never hold values; callers
// pass identifiers, names, versions and operation names only.
func (s *Store) audit(ctx context.Context, tx pgx.Tx, m secrets.Mutation, e auditEntry) error {
	id, err := s.ids.NewID("aud")
	if err != nil {
		return err
	}
	meta := e.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	mj, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	var project *string
	if e.Scope.ProjectID != "" {
		p := string(e.Scope.ProjectID)
		project = &p
	}
	var reason, request *string
	if m.Reason != "" {
		reason = &m.Reason
	}
	if m.RequestID != "" {
		request = &m.RequestID
	}
	_, err = tx.Exec(ctx, `INSERT INTO audit
(tenant_id, id, project_id, occurred_at, actor_type, actor_id, action, resource_type, resource_id, resource_version, reason, request_id, metadata)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13::jsonb)`,
		string(e.Scope.TenantID), id, project, m.Now.UTC(), string(m.Actor.Type), m.Actor.ID, e.Action, e.ResourceType, e.ResourceID,
		e.ResourceVersion, reason, request, string(mj))
	return err
}

// eventSpec describes one configuration.changed event. The payload carries
// references and versions only, never a value.
type eventSpec struct {
	Scope      domain.Scope // ProjectID empty for tenant-wide events
	Kind       string       // "variable" or "secret"
	ResourceID string
	Version    int64
	Change     string // created, updated, deleted, rotated
	ScopeName  string // project or tenant
}

func (s *Store) emit(ctx context.Context, tx pgx.Tx, m secrets.Mutation, e eventSpec) error {
	id, err := s.ids.NewID("evt")
	if err != nil {
		return err
	}
	data, err := json.Marshal(eventsv1.ConfigurationChanged{Kind: e.Kind, Version: e.Version, Change: e.Change, Scope: e.ScopeName})
	if err != nil {
		return err
	}
	actor := eventsv1.Actor{ID: m.Actor.ID}
	switch m.Actor.Type {
	case secrets.ActorUser:
		actor.Type = eventsv1.ActorUser
	case secrets.ActorWorkload:
		actor.Type = eventsv1.ActorWorkload
	case secrets.ActorOperator:
		actor.Type = eventsv1.ActorOperator
	default:
		actor.Type = eventsv1.ActorService
	}
	corr := m.RequestID
	if corr == "" {
		corr = id
	}
	env := eventsv1.Envelope{
		ID: id, SchemaVersion: eventsv1.SchemaVersion, Type: eventsv1.TypeConfigurationChanged, OccurredAt: m.Now.UTC(),
		TenantID: e.Scope.TenantID, ProjectID: e.Scope.ProjectID,
		Resource: eventsv1.Resource{Type: "configuration", ID: e.ResourceID, Version: e.Version},
		Actor:    actor, CorrelationID: corr, Data: data,
	}
	return s.events(ctx, tx, env)
}

func (w *writer) checkScope(scope domain.Scope) error {
	if scope.TenantID != w.tenant {
		return fmt.Errorf("%w: scope is outside the transaction's tenant", secrets.ErrNotFound)
	}
	return nil
}

// ---- Reader on the writer's transaction -------------------------------------

func (w *writer) GetVariable(ctx context.Context, scope domain.Scope, id string) (secrets.Variable, error) {
	if err := w.checkScope(scope); err != nil {
		return secrets.Variable{}, err
	}
	return getVariable(ctx, w.tx, scope, id, false)
}

func (w *writer) ListVariables(ctx context.Context, scope domain.Scope, afterName string, limit int) ([]secrets.Variable, error) {
	if err := w.checkScope(scope); err != nil {
		return nil, err
	}
	return listVariables(ctx, w.tx, scope, afterName, limit)
}

func (w *writer) GetSecret(ctx context.Context, scope domain.Scope, id string) (secrets.Secret, error) {
	if err := w.checkScope(scope); err != nil {
		return secrets.Secret{}, err
	}
	return getSecret(ctx, w.tx, scope, id, "")
}

func (w *writer) ListSecrets(ctx context.Context, scope domain.Scope, afterName string, limit int) ([]secrets.Secret, error) {
	if err := w.checkScope(scope); err != nil {
		return nil, err
	}
	return listSecrets(ctx, w.tx, scope, afterName, limit)
}

func (w *writer) GetSecretVersion(ctx context.Context, scope domain.Scope, secretID string, version int64) (secrets.SecretVersion, error) {
	if err := w.checkScope(scope); err != nil {
		return secrets.SecretVersion{}, err
	}
	return getVersion(ctx, w.tx, scope, secretID, version, "")
}

func (w *writer) ListSecretVersions(ctx context.Context, scope domain.Scope, secretID string, after int64, limit int) ([]secrets.SecretVersion, error) {
	if err := w.checkScope(scope); err != nil {
		return nil, err
	}
	return listVersions(ctx, w.tx, scope, secretID, after, limit)
}

func (w *writer) ActiveScopeKey(ctx context.Context, tenant domain.TenantID) (secrets.ScopeKey, error) {
	if tenant != w.tenant {
		return secrets.ScopeKey{}, secrets.ErrNotFound
	}
	return activeScopeKey(ctx, w.tx, tenant)
}

func (w *writer) GetScopeKey(ctx context.Context, tenant domain.TenantID, id string) (secrets.ScopeKey, error) {
	if tenant != w.tenant {
		return secrets.ScopeKey{}, secrets.ErrNotFound
	}
	return getScopeKey(ctx, w.tx, tenant, id, false)
}

func (w *writer) RetentionReport(ctx context.Context, scope domain.Scope, secretID string) (secrets.RetentionReport, error) {
	if err := w.checkScope(scope); err != nil {
		return secrets.RetentionReport{}, err
	}
	if _, err := getSecret(ctx, w.tx, scope, secretID, ""); err != nil {
		return secrets.RetentionReport{}, err
	}
	return retentionReport(ctx, w.tx, scope, secretID)
}

func (w *writer) PendingHandling(ctx context.Context, scope domain.Scope, afterID string, limit int) ([]secrets.ConsumerHandling, error) {
	if err := w.checkScope(scope); err != nil {
		return nil, err
	}
	return pendingHandling(ctx, w.tx, scope, afterID, limit)
}

// ---- variables --------------------------------------------------------------

func (w *writer) CreateVariable(ctx context.Context, m secrets.Mutation, v secrets.Variable) error {
	rec := v.Record()
	if err := w.checkScope(rec.Scope); err != nil {
		return err
	}
	_, err := w.tx.Exec(ctx, `INSERT INTO variables
(tenant_id, project_id, id, name, type, description, value_text, value_version, version, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		string(rec.Scope.TenantID), string(rec.Scope.ProjectID), rec.ID, rec.Name, string(rec.Type), rec.Description, rec.ValueText,
		rec.ValueVersion, int64(rec.Version), rec.CreatedAt, rec.UpdatedAt)
	switch {
	case isUnique(err, "variables_name_unique"):
		return secrets.ErrNameTaken
	case isForeignKey(err):
		return fmt.Errorf("%w: project", secrets.ErrNotFound)
	case err != nil:
		return err
	}
	if err := w.insertVariableVersion(ctx, rec); err != nil {
		return err
	}
	if err := w.s.audit(ctx, w.tx, m, auditEntry{Scope: rec.Scope, Action: "variable.create", ResourceType: "variable", ResourceID: rec.ID,
		ResourceVersion: int64(rec.Version), Metadata: map[string]any{"name": rec.Name, "type": string(rec.Type), "valueVersion": rec.ValueVersion}}); err != nil {
		return err
	}
	return w.s.emit(ctx, w.tx, m, eventSpec{Scope: rec.Scope, Kind: "variable", ResourceID: rec.ID, Version: rec.ValueVersion, Change: "created", ScopeName: "project"})
}

func (w *writer) insertVariableVersion(ctx context.Context, rec secrets.VariableRecord) error {
	_, err := w.tx.Exec(ctx, `INSERT INTO variable_versions (tenant_id, project_id, variable_id, version, type, value_text, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		string(rec.Scope.TenantID), string(rec.Scope.ProjectID), rec.ID, rec.ValueVersion, string(rec.Type), rec.ValueText, rec.UpdatedAt)
	return err
}

func (w *writer) EditVariable(ctx context.Context, m secrets.Mutation, scope domain.Scope, id string, expected domain.Version, edit secrets.VariableEdit) (secrets.Variable, error) {
	if err := w.checkScope(scope); err != nil {
		return secrets.Variable{}, err
	}
	v, err := getVariable(ctx, w.tx, scope, id, true)
	if err != nil {
		return secrets.Variable{}, err
	}
	before := v.Record()
	valueChanged, changed, err := v.Edit(expected, edit, m.Now)
	if err != nil || !changed {
		return v, err
	}
	rec := v.Record()
	tag, err := w.tx.Exec(ctx, `UPDATE variables SET description = $4, value_text = $5, value_version = $6, version = $7, updated_at = $8
WHERE tenant_id = $1 AND project_id = $2 AND id = $3 AND version = $9`,
		string(scope.TenantID), string(scope.ProjectID), id, rec.Description, rec.ValueText, rec.ValueVersion, int64(rec.Version), rec.UpdatedAt, int64(before.Version))
	if err != nil {
		return secrets.Variable{}, err
	}
	if tag.RowsAffected() != 1 {
		return secrets.Variable{}, &secrets.ConflictError{Resource: "variable", Expected: int64(expected), Current: int64(before.Version) + 1}
	}
	if valueChanged {
		if err := w.insertVariableVersion(ctx, rec); err != nil {
			return secrets.Variable{}, err
		}
	}
	if err := w.s.audit(ctx, w.tx, m, auditEntry{Scope: scope, Action: "variable.update", ResourceType: "variable", ResourceID: id,
		ResourceVersion: int64(rec.Version), Metadata: map[string]any{"name": rec.Name, "valueChanged": valueChanged, "valueVersion": rec.ValueVersion}}); err != nil {
		return secrets.Variable{}, err
	}
	if err := w.s.emit(ctx, w.tx, m, eventSpec{Scope: scope, Kind: "variable", ResourceID: id, Version: rec.ValueVersion, Change: "updated", ScopeName: "project"}); err != nil {
		return secrets.Variable{}, err
	}
	return v, nil
}

func (w *writer) DeleteVariable(ctx context.Context, m secrets.Mutation, scope domain.Scope, id string, expected domain.Version) (secrets.Variable, error) {
	if err := w.checkScope(scope); err != nil {
		return secrets.Variable{}, err
	}
	v, err := getVariable(ctx, w.tx, scope, id, true)
	if err != nil {
		return secrets.Variable{}, err
	}
	before := v.Record()
	changed, err := v.Delete(expected, m.Now)
	if err != nil || !changed {
		return v, err
	}
	rec := v.Record()
	if _, err := w.tx.Exec(ctx, `UPDATE variables SET deleted_at = $4, version = $5, updated_at = $6
WHERE tenant_id = $1 AND project_id = $2 AND id = $3 AND version = $7`,
		string(scope.TenantID), string(scope.ProjectID), id, *rec.DeletedAt, int64(rec.Version), rec.UpdatedAt, int64(before.Version)); err != nil {
		return secrets.Variable{}, err
	}
	if err := w.s.audit(ctx, w.tx, m, auditEntry{Scope: scope, Action: "variable.delete", ResourceType: "variable", ResourceID: id,
		ResourceVersion: int64(rec.Version), Metadata: map[string]any{"name": rec.Name, "valueVersion": rec.ValueVersion}}); err != nil {
		return secrets.Variable{}, err
	}
	if err := w.s.emit(ctx, w.tx, m, eventSpec{Scope: scope, Kind: "variable", ResourceID: id, Version: rec.ValueVersion, Change: "deleted", ScopeName: "project"}); err != nil {
		return secrets.Variable{}, err
	}
	return v, nil
}

// ---- secrets ----------------------------------------------------------------

func (w *writer) insertVersion(ctx context.Context, v secrets.SecretVersion) error {
	r := v.Record()
	_, err := w.tx.Exec(ctx, `INSERT INTO secret_versions
(tenant_id, project_id, secret_id, version, scope_key_id, key_generation, format_version, algorithm, nonce, ciphertext, created_at, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		string(r.Scope.TenantID), string(r.Scope.ProjectID), r.SecretID, r.Version, r.ScopeKeyID, r.KeyGeneration,
		r.Envelope.FormatVersion, r.Envelope.Algorithm, r.Envelope.Nonce, r.Envelope.Ciphertext, r.CreatedAt, r.CreatedBy)
	if isForeignKey(err) {
		return fmt.Errorf("%w: unknown scope key", secrets.ErrInvalid)
	}
	return err
}

func (w *writer) CreateSecret(ctx context.Context, m secrets.Mutation, s secrets.Secret, first secrets.SecretVersion) error {
	rec := s.Record()
	if err := w.checkScope(rec.Scope); err != nil {
		return err
	}
	if rec.CurrentVersion != 1 || first.Version() != 1 || first.SecretID() != rec.ID || first.Record().Scope != rec.Scope {
		return fmt.Errorf("%w: a new secret is created with exactly its first version", secrets.ErrInvalid)
	}
	_, err := w.tx.Exec(ctx, `INSERT INTO secrets
(tenant_id, project_id, id, name, kind, current_version, version, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		string(rec.Scope.TenantID), string(rec.Scope.ProjectID), rec.ID, rec.Name, string(rec.Kind), rec.CurrentVersion, int64(rec.Version), rec.CreatedAt, rec.UpdatedAt)
	switch {
	case isUnique(err, "secrets_name_unique"):
		return secrets.ErrNameTaken
	case isForeignKey(err):
		return fmt.Errorf("%w: project", secrets.ErrNotFound)
	case err != nil:
		return err
	}
	if err := w.insertVersion(ctx, first); err != nil {
		return err
	}
	if err := w.s.audit(ctx, w.tx, m, auditEntry{Scope: rec.Scope, Action: "secret.create", ResourceType: "secret", ResourceID: rec.ID,
		ResourceVersion: int64(rec.Version), Metadata: map[string]any{"name": rec.Name, "kind": string(rec.Kind), "valueVersion": 1}}); err != nil {
		return err
	}
	return w.s.emit(ctx, w.tx, m, eventSpec{Scope: rec.Scope, Kind: "secret", ResourceID: rec.ID, Version: 1, Change: "created", ScopeName: "project"})
}

func (w *writer) AddSecretVersion(ctx context.Context, m secrets.Mutation, scope domain.Scope, id string, expected domain.Version, v secrets.SecretVersion) (secrets.Secret, error) {
	if err := w.checkScope(scope); err != nil {
		return secrets.Secret{}, err
	}
	s, err := getSecret(ctx, w.tx, scope, id, " FOR UPDATE")
	if err != nil {
		return secrets.Secret{}, err
	}
	before := s.Record()
	if err := s.AcceptVersion(expected, v, m.Now); err != nil {
		return secrets.Secret{}, err
	}
	rec := s.Record()
	if err := w.insertVersion(ctx, v); err != nil {
		return secrets.Secret{}, err
	}
	tag, err := w.tx.Exec(ctx, `UPDATE secrets SET current_version = $4, version = $5, updated_at = $6
WHERE tenant_id = $1 AND project_id = $2 AND id = $3 AND version = $7 AND current_version = $8 AND deleted_at IS NULL`,
		string(scope.TenantID), string(scope.ProjectID), id, rec.CurrentVersion, int64(rec.Version), rec.UpdatedAt, int64(before.Version), before.CurrentVersion)
	if err != nil {
		return secrets.Secret{}, err
	}
	if tag.RowsAffected() != 1 {
		return secrets.Secret{}, &secrets.ConflictError{Resource: "secret", Expected: int64(expected), Current: int64(before.Version) + 1}
	}
	if err := w.s.audit(ctx, w.tx, m, auditEntry{Scope: scope, Action: "secret.rotate", ResourceType: "secret", ResourceID: id,
		ResourceVersion: int64(rec.Version), Metadata: map[string]any{"name": rec.Name, "kind": string(rec.Kind), "valueVersion": rec.CurrentVersion}}); err != nil {
		return secrets.Secret{}, err
	}
	if err := w.s.emit(ctx, w.tx, m, eventSpec{Scope: scope, Kind: "secret", ResourceID: id, Version: rec.CurrentVersion, Change: "rotated", ScopeName: "project"}); err != nil {
		return secrets.Secret{}, err
	}
	return s, nil
}

func (w *writer) RevokeSecretVersion(ctx context.Context, m secrets.Mutation, scope domain.Scope, id string, version int64) (secrets.RevocationResult, error) {
	if err := w.checkScope(scope); err != nil {
		return secrets.RevocationResult{}, err
	}
	// Lock order: secret row, then version row (the same order pin registration
	// and resolution finalization follow).
	s, err := getSecret(ctx, w.tx, scope, id, " FOR UPDATE")
	if err != nil {
		return secrets.RevocationResult{}, err
	}
	v, err := getVersion(ctx, w.tx, scope, id, version, " FOR UPDATE")
	if err != nil {
		return secrets.RevocationResult{}, err
	}
	before := s.Record()
	changed, err := s.RevokeVersion(&v, m.Actor.ID, m.Now)
	if err != nil {
		return secrets.RevocationResult{}, err
	}
	res := secrets.RevocationResult{Secret: s, Version: v, Changed: changed}
	if !changed {
		res.Handling, err = versionHandling(ctx, w.tx, scope, id, version)
		return res, err
	}
	vr, rec := v.Record(), s.Record()
	tag, err := w.tx.Exec(ctx, `UPDATE secret_versions SET revoked_at = $5, revoked_by = $6
WHERE tenant_id = $1 AND project_id = $2 AND secret_id = $3 AND version = $4 AND revoked_at IS NULL`,
		string(scope.TenantID), string(scope.ProjectID), id, version, *vr.RevokedAt, vr.RevokedBy)
	if err != nil {
		return secrets.RevocationResult{}, err
	}
	if tag.RowsAffected() != 1 {
		return secrets.RevocationResult{}, fmt.Errorf("%w: revocation lost its row lock", secrets.ErrInconsistent)
	}
	if _, err := w.tx.Exec(ctx, `UPDATE secrets SET version = $4, updated_at = $5
WHERE tenant_id = $1 AND project_id = $2 AND id = $3 AND version = $6`,
		string(scope.TenantID), string(scope.ProjectID), id, int64(rec.Version), rec.UpdatedAt, int64(before.Version)); err != nil {
		return secrets.RevocationResult{}, err
	}
	if res.Handling, err = w.recordHandling(ctx, m, scope, id, version); err != nil {
		return secrets.RevocationResult{}, err
	}
	pending := 0
	for _, h := range res.Handling {
		if h.Pending() {
			pending++
		}
	}
	if err := w.s.audit(ctx, w.tx, m, auditEntry{Scope: scope, Action: "secret.revoke_version", ResourceType: "secret", ResourceID: id,
		ResourceVersion: int64(rec.Version), Metadata: map[string]any{"name": rec.Name, "revokedVersion": version, "pendingHandling": pending}}); err != nil {
		return secrets.RevocationResult{}, err
	}
	// The merged vocabulary has no revoked change: map it to updated and keep
	// the precise operation in the audit metadata above.
	if err := w.s.emit(ctx, w.tx, m, eventSpec{Scope: scope, Kind: "secret", ResourceID: id, Version: version, Change: "updated", ScopeName: "project"}); err != nil {
		return secrets.RevocationResult{}, err
	}
	return res, nil
}

// recordHandling creates the pending effects for every unreleased pin of the
// revoked version. Delivered bytes are not recalled by these records: they only
// track the external procedures the owners must still acknowledge.
func (w *writer) recordHandling(ctx context.Context, m secrets.Mutation, scope domain.Scope, secretID string, version int64) ([]secrets.ConsumerHandling, error) {
	rows, err := w.tx.Query(ctx, `SELECT consumer_kind, consumer_id, first_resolved_at IS NOT NULL FROM secret_pins
WHERE tenant_id = $1 AND project_id = $2 AND secret_id = $3 AND secret_version = $4 AND released_at IS NULL
ORDER BY consumer_kind, consumer_id`, string(scope.TenantID), string(scope.ProjectID), secretID, version)
	if err != nil {
		return nil, err
	}
	type pin struct {
		ref       secrets.ConsumerRef
		delivered bool
	}
	var pins []pin
	for rows.Next() {
		var p pin
		if err := rows.Scan(&p.ref.Kind, &p.ref.ID, &p.delivered); err != nil {
			rows.Close()
			return nil, err
		}
		pins = append(pins, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []secrets.ConsumerHandling
	for _, p := range pins {
		for _, effect := range secrets.Effects() {
			hid, err := w.s.ids.NewID("ch")
			if err != nil {
				return nil, err
			}
			h, err := secrets.NewConsumerHandling(scope, hid, secretID, version, p.ref, effect, p.delivered, m.Now)
			if err != nil {
				return nil, err
			}
			r := h.Record()
			if _, err := w.tx.Exec(ctx, `INSERT INTO secret_consumer_handling
(tenant_id, id, project_id, secret_id, secret_version, consumer_kind, consumer_id, effect, state, delivered, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'pending', $9, $10)
ON CONFLICT ON CONSTRAINT secret_consumer_handling_unique DO NOTHING`,
				string(scope.TenantID), r.ID, string(scope.ProjectID), secretID, version, r.Consumer.Kind, r.Consumer.ID, string(r.Effect), r.Delivered, r.CreatedAt); err != nil {
				return nil, err
			}
			out = append(out, h)
		}
	}
	return out, nil
}

func (w *writer) DeleteSecret(ctx context.Context, m secrets.Mutation, scope domain.Scope, id string, expected domain.Version) (secrets.Secret, secrets.RetentionReport, error) {
	if err := w.checkScope(scope); err != nil {
		return secrets.Secret{}, secrets.RetentionReport{}, err
	}
	// FOR UPDATE orders deletion against pin registration, which takes this row
	// FOR SHARE: a pin either committed before the report below is computed, or
	// it waits and then sees the deletion and is refused.
	s, err := getSecret(ctx, w.tx, scope, id, " FOR UPDATE")
	if err != nil {
		return secrets.Secret{}, secrets.RetentionReport{}, err
	}
	before := s.Record()
	changed, err := s.Delete(expected, m.Now)
	if err != nil {
		return secrets.Secret{}, secrets.RetentionReport{}, err
	}
	if changed {
		rec := s.Record()
		if _, err := w.tx.Exec(ctx, `UPDATE secrets SET deleted_at = $4, version = $5, updated_at = $6
WHERE tenant_id = $1 AND project_id = $2 AND id = $3 AND version = $7`,
			string(scope.TenantID), string(scope.ProjectID), id, *rec.DeletedAt, int64(rec.Version), rec.UpdatedAt, int64(before.Version)); err != nil {
			return secrets.Secret{}, secrets.RetentionReport{}, err
		}
	}
	report, err := retentionReport(ctx, w.tx, scope, id)
	if err != nil {
		return secrets.Secret{}, secrets.RetentionReport{}, err
	}
	if changed {
		rec := s.Record()
		if err := w.s.audit(ctx, w.tx, m, auditEntry{Scope: scope, Action: "secret.delete", ResourceType: "secret", ResourceID: id,
			ResourceVersion: int64(rec.Version), Metadata: map[string]any{
				"name": rec.Name, "pins": report.PinnedVersions, "activePins": report.ActivePins,
				"pendingHandling": report.PendingHandling, "backupRetention": string(report.BackupRetention),
			}}); err != nil {
			return secrets.Secret{}, secrets.RetentionReport{}, err
		}
		if err := w.s.emit(ctx, w.tx, m, eventSpec{Scope: scope, Kind: "secret", ResourceID: id, Version: rec.CurrentVersion, Change: "deleted", ScopeName: "project"}); err != nil {
			return secrets.Secret{}, secrets.RetentionReport{}, err
		}
	}
	return s, report, nil
}

// ---- scope keys --------------------------------------------------------------

func (w *writer) RewrapScopeKey(ctx context.Context, m secrets.Mutation, tenant domain.TenantID, keyID string, expectedRevision int64, wk secrets.WrappedKey) (secrets.ScopeKey, error) {
	if tenant != w.tenant {
		return secrets.ScopeKey{}, secrets.ErrNotFound
	}
	k, err := getScopeKey(ctx, w.tx, tenant, keyID, true)
	if err != nil {
		return secrets.ScopeKey{}, err
	}
	if err := k.Rewrap(expectedRevision, wk, m.Now); err != nil {
		return secrets.ScopeKey{}, err
	}
	rec := k.Record()
	// The new wrap row is added first; the previous rows stay as history.
	if err := insertWrap(ctx, w.tx, tenant, keyID, rec.CurrentRevision, rec.Wrapped, rec.WrappedAt); err != nil {
		return secrets.ScopeKey{}, err
	}
	tag, err := w.tx.Exec(ctx, `UPDATE secret_scope_keys SET current_revision = $3
WHERE tenant_id = $1 AND id = $2 AND current_revision = $4`, string(tenant), keyID, rec.CurrentRevision, expectedRevision)
	if err != nil {
		return secrets.ScopeKey{}, err
	}
	if tag.RowsAffected() != 1 {
		return secrets.ScopeKey{}, &secrets.ConflictError{Resource: "wrap revision", Expected: expectedRevision, Current: expectedRevision + 1}
	}
	scope := domain.Scope{TenantID: tenant}
	if err := w.s.audit(ctx, w.tx, m, auditEntry{Scope: scope, Action: "secret_scope_key.rewrap", ResourceType: "secret_scope_key", ResourceID: keyID,
		ResourceVersion: rec.CurrentRevision, Metadata: map[string]any{"generation": rec.Generation, "kek": kekMeta(rec.Wrapped.KEK), "previousRevision": expectedRevision}}); err != nil {
		return secrets.ScopeKey{}, err
	}
	if err := w.s.emit(ctx, w.tx, m, eventSpec{Scope: scope, Kind: "secret", ResourceID: keyID, Version: rec.CurrentRevision, Change: "updated", ScopeName: "tenant"}); err != nil {
		return secrets.ScopeKey{}, err
	}
	return k, nil
}

// ---- consumer handling --------------------------------------------------------

func (w *writer) AcknowledgeHandling(ctx context.Context, m secrets.Mutation, scope domain.Scope, id string, outcome secrets.HandlingState) (secrets.ConsumerHandling, error) {
	if err := w.checkScope(scope); err != nil {
		return secrets.ConsumerHandling{}, err
	}
	h, err := scanHandling(w.tx.QueryRow(ctx, `SELECT `+handlingColumns+` FROM secret_consumer_handling
WHERE tenant_id = $1 AND project_id = $2 AND id = $3 FOR UPDATE`, string(scope.TenantID), string(scope.ProjectID), id))
	if errors.Is(err, pgx.ErrNoRows) {
		return secrets.ConsumerHandling{}, secrets.ErrNotFound
	}
	if err != nil {
		return secrets.ConsumerHandling{}, err
	}
	changed, err := h.Acknowledge(outcome, m.Actor.ID, m.Now)
	if err != nil || !changed {
		return h, err
	}
	rec := h.Record()
	if _, err := w.tx.Exec(ctx, `UPDATE secret_consumer_handling SET state = $4, resolved_at = $5, resolved_by = $6
WHERE tenant_id = $1 AND project_id = $2 AND id = $3 AND state = 'pending'`,
		string(scope.TenantID), string(scope.ProjectID), id, string(rec.State), *rec.ResolvedAt, rec.ResolvedBy); err != nil {
		return secrets.ConsumerHandling{}, err
	}
	if err := w.s.audit(ctx, w.tx, m, auditEntry{Scope: scope, Action: "secret.handling_acknowledged", ResourceType: "secret", ResourceID: rec.SecretID,
		ResourceVersion: rec.SecretVersion, Metadata: map[string]any{"handlingId": id, "effect": string(rec.Effect), "outcome": string(rec.State),
			"consumerKind": rec.Consumer.Kind, "consumerId": rec.Consumer.ID}}); err != nil {
		return secrets.ConsumerHandling{}, err
	}
	return h, nil
}
