package secretstest

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/eventstest"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/secrets"
)

// EventAppender is the transaction-local event append of the in-memory event bus
// (eventstest.Mem.Append), the counterpart of the PostgreSQL adapter's
// pgx-based appender.
type EventAppender interface {
	Append(tx memdb.Tx, env eventsv1.Envelope) (eventstest.AppendResult, error)
}

// MemConfig configures a MemStore.
type MemConfig struct {
	Store  *memdb.Store
	Events EventAppender
	IDs    secrets.IDGenerator
	// Clock defaults to the UTC wall clock.
	Clock func() time.Time
}

type variableRec struct{ R secrets.VariableRecord }

type secretRec struct{ R secrets.SecretRecord }

type versionRec struct{ R secrets.SecretVersionRecord }

type scopeKeyRec struct{ R secrets.ScopeKeyRecord }

type wrapRec struct {
	KeyID    string
	Revision int64
	Wrapped  secrets.WrappedKey
	At       time.Time
}

type pinRec struct {
	Project         domain.ProjectID
	Consumer        secrets.ConsumerRef
	SecretID        string
	Version         int64
	CreatedAt       time.Time
	ReleasedAt      *time.Time
	FirstResolvedAt *time.Time
	LastResolvedAt  *time.Time
	Resolutions     int
}

type handlingRec struct {
	R secrets.ConsumerHandlingRecord
}

type auditRec struct {
	ID       string
	Action   string
	Resource string
	Version  int64
	Actor    secrets.Actor
	Metadata map[string]any
}

func pinKey(c secrets.ConsumerRef, secretID string, version int64) string {
	return fmt.Sprintf("%s|%s|%s|%020d", c.Kind, c.ID, secretID, version)
}

var (
	variablesT = memdb.NewTable("variables",
		func(r variableRec) string { return string(r.R.Scope.ProjectID) + "/" + r.R.ID },
		// A name is reserved for the project's lifetime, deleted or not.
		func(r variableRec) string { return string(r.R.Scope.ProjectID) + "/" + r.R.Name })
	secretsT = memdb.NewTable("secrets",
		func(r secretRec) string { return string(r.R.Scope.ProjectID) + "/" + r.R.ID },
		func(r secretRec) string { return string(r.R.Scope.ProjectID) + "/" + r.R.Name })
	versionsT = memdb.NewTable("secret_versions", func(r versionRec) string {
		return fmt.Sprintf("%s/%s/%020d", r.R.Scope.ProjectID, r.R.SecretID, r.R.Version)
	})
	scopeKeysT = memdb.NewTable("secret_scope_keys",
		func(r scopeKeyRec) string { return r.R.ID },
		func(r scopeKeyRec) string { return fmt.Sprintf("gen/%d", r.R.Generation) })
	wrapsT    = memdb.NewTable("secret_scope_key_wraps", func(r wrapRec) string { return fmt.Sprintf("%s/%020d", r.KeyID, r.Revision) })
	pinsT     = memdb.NewTable("secret_pins", func(r pinRec) string { return pinKey(r.Consumer, r.SecretID, r.Version) })
	handlingT = memdb.NewTable("secret_consumer_handling",
		func(r handlingRec) string { return r.R.ID },
		func(r handlingRec) string {
			return fmt.Sprintf("%s|%d|%s|%s|%s", r.R.SecretID, r.R.SecretVersion, r.R.Consumer.Kind, r.R.Consumer.ID, r.R.Effect)
		})
	auditT = memdb.NewTable("secret_audit", func(r auditRec) string { return r.ID })
)

// MemStore implements secrets.Store, secrets.VariableCatalog and
// secrets.SecretCatalog over memdb. Atomically is one memdb.Update: every write
// of a unit, with its audit rows and its configuration.changed events, commits
// together or not at all. Key-provider calls never happen inside it: the service
// prepares ciphertext first, exactly as with PostgreSQL.
//
// MemStore is development-only. See MEMORY.md for what it does not model.
type MemStore struct {
	cfg MemConfig
}

var (
	_ secrets.Store           = (*MemStore)(nil)
	_ secrets.VariableCatalog = (*MemStore)(nil)
	_ secrets.SecretCatalog   = (*MemStore)(nil)
)

// NewMemStore validates cfg.
func NewMemStore(cfg MemConfig) (*MemStore, error) {
	if cfg.Store == nil || cfg.Events == nil || cfg.IDs == nil {
		return nil, errors.New("secretstest: a memdb store, an event appender and an ID generator are required")
	}
	if cfg.Clock == nil {
		cfg.Clock = func() time.Time { return time.Now().UTC() }
	}
	return &MemStore{cfg: cfg}, nil
}

// DevelopmentOnly marks the adapter for the composition root: production
// refuses it (SD-12).
func (*MemStore) DevelopmentOnly() bool { return true }

func (s *MemStore) now() time.Time { return s.cfg.Clock().UTC() }

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := t.UTC()
	return &c
}

func cloneWrapped(w secrets.WrappedKey) secrets.WrappedKey {
	w.Bytes = append([]byte(nil), w.Bytes...)
	return w
}

// ---- audit and events ----------------------------------------------------------

type auditEntry struct {
	Scope           domain.Scope
	Action          string
	ResourceType    string
	ResourceID      string
	ResourceVersion int64
	Metadata        map[string]any
}

func (s *MemStore) audit(tx memdb.Tx, m secrets.Mutation, e auditEntry) error {
	id, err := s.cfg.IDs.NewID("aud")
	if err != nil {
		return err
	}
	meta := map[string]any{}
	for k, v := range e.Metadata {
		meta[k] = v
	}
	return auditT.Put(tx, auditRec{ID: id, Action: e.Action, Resource: e.ResourceType + "/" + e.ResourceID, Version: e.ResourceVersion, Actor: m.Actor, Metadata: meta})
}

type eventSpec struct {
	Scope      domain.Scope
	Kind       string
	ResourceID string
	Version    int64
	Change     string
	ScopeName  string
}

func (s *MemStore) emit(tx memdb.Tx, m secrets.Mutation, e eventSpec) error {
	id, err := s.cfg.IDs.NewID("evt")
	if err != nil {
		return err
	}
	data, err := jsonRaw(eventsv1.ConfigurationChanged{Kind: e.Kind, Version: e.Version, Change: e.Change, Scope: e.ScopeName})
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
	_, err = s.cfg.Events.Append(tx, eventsv1.Envelope{
		ID: id, SchemaVersion: eventsv1.SchemaVersion, Type: eventsv1.TypeConfigurationChanged, OccurredAt: m.Now.UTC(),
		TenantID: e.Scope.TenantID, ProjectID: e.Scope.ProjectID,
		Resource: eventsv1.Resource{Type: "configuration", ID: e.ResourceID, Version: e.Version},
		Actor:    actor, CorrelationID: corr, Data: data,
	})
	if errors.Is(err, events.ErrBacklogFull) {
		return fmt.Errorf("%w: %v", secrets.ErrBackpressure, err)
	}
	return err
}

// AuditActions lists the tenant's audit actions, sorted.
func (s *MemStore) AuditActions(ctx context.Context, tenant domain.TenantID) ([]string, error) {
	var out []string
	err := s.cfg.Store.View(ctx, tenant, func(tx memdb.Tx) error {
		auditT.Scan(tx, func(r auditRec) bool { out = append(out, r.Action); return true })
		return nil
	})
	sort.Strings(out)
	return out, err
}

// WrapRevisions lists the retained wrap revisions of a scope key in order.
func (s *MemStore) WrapRevisions(ctx context.Context, tenant domain.TenantID, keyID string) ([]int64, error) {
	var out []int64
	err := s.cfg.Store.View(ctx, tenant, func(tx memdb.Tx) error {
		wrapsT.Scan(tx, func(r wrapRec) bool {
			if r.KeyID == keyID {
				out = append(out, r.Revision)
			}
			return true
		})
		return nil
	})
	return out, err
}

// ---- reads over any Tx ----------------------------------------------------------

func variableOf(r variableRec) (secrets.Variable, error) {
	rec := r.R
	rec.DeletedAt = cloneTime(rec.DeletedAt)
	v, err := secrets.RestoreVariable(rec)
	if err != nil {
		return secrets.Variable{}, fmt.Errorf("%w: variable row: %v", secrets.ErrInconsistent, err)
	}
	return v, nil
}

func getVariable(tx memdb.Tx, scope domain.Scope, id string) (secrets.Variable, error) {
	r, ok := variablesT.Get(tx, string(scope.ProjectID)+"/"+id)
	if !ok || r.R.Scope != scope {
		return secrets.Variable{}, secrets.ErrNotFound
	}
	return variableOf(r)
}

func listVariables(tx memdb.Tx, scope domain.Scope, afterName string, limit int) ([]secrets.Variable, error) {
	var rows []variableRec
	variablesT.Scan(tx, func(r variableRec) bool {
		if r.R.Scope == scope && r.R.DeletedAt == nil && r.R.Name > afterName {
			rows = append(rows, r)
		}
		return true
	})
	sort.Slice(rows, func(i, j int) bool { return rows[i].R.Name < rows[j].R.Name })
	var out []secrets.Variable
	for _, r := range rows {
		if len(out) >= limit {
			break
		}
		v, err := variableOf(r)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func secretOf(r secretRec) (secrets.Secret, error) {
	rec := r.R
	rec.DeletedAt = cloneTime(rec.DeletedAt)
	s, err := secrets.RestoreSecret(rec)
	if err != nil {
		return secrets.Secret{}, fmt.Errorf("%w: secret row: %v", secrets.ErrInconsistent, err)
	}
	return s, nil
}

func getSecret(tx memdb.Tx, scope domain.Scope, id string) (secrets.Secret, error) {
	r, ok := secretsT.Get(tx, string(scope.ProjectID)+"/"+id)
	if !ok || r.R.Scope != scope {
		return secrets.Secret{}, secrets.ErrNotFound
	}
	return secretOf(r)
}

func getSecretByName(tx memdb.Tx, scope domain.Scope, name string) (secrets.Secret, error) {
	var out secretRec
	found := false
	secretsT.Scan(tx, func(r secretRec) bool {
		if r.R.Scope == scope && r.R.Name == name {
			out, found = r, true
			return false
		}
		return true
	})
	if !found {
		return secrets.Secret{}, secrets.ErrNotFound
	}
	return secretOf(out)
}

func listSecrets(tx memdb.Tx, scope domain.Scope, afterName string, limit int) ([]secrets.Secret, error) {
	var rows []secretRec
	secretsT.Scan(tx, func(r secretRec) bool {
		if r.R.Scope == scope && r.R.DeletedAt == nil && r.R.Name > afterName {
			rows = append(rows, r)
		}
		return true
	})
	sort.Slice(rows, func(i, j int) bool { return rows[i].R.Name < rows[j].R.Name })
	var out []secrets.Secret
	for _, r := range rows {
		if len(out) >= limit {
			break
		}
		sec, err := secretOf(r)
		if err != nil {
			return nil, err
		}
		out = append(out, sec)
	}
	return out, nil
}

func versionOf(r versionRec) (secrets.SecretVersion, error) {
	rec := r.R
	rec.Envelope.Nonce = append([]byte(nil), rec.Envelope.Nonce...)
	rec.Envelope.Ciphertext = append([]byte(nil), rec.Envelope.Ciphertext...)
	rec.RevokedAt = cloneTime(rec.RevokedAt)
	v, err := secrets.RestoreSecretVersion(rec)
	if err != nil {
		return secrets.SecretVersion{}, fmt.Errorf("%w: secret version row: %v", secrets.ErrInconsistent, err)
	}
	return v, nil
}

func getVersion(tx memdb.Tx, scope domain.Scope, secretID string, version int64) (secrets.SecretVersion, error) {
	r, ok := versionsT.Get(tx, fmt.Sprintf("%s/%s/%020d", scope.ProjectID, secretID, version))
	if !ok || r.R.Scope != scope {
		return secrets.SecretVersion{}, secrets.ErrNotFound
	}
	return versionOf(r)
}

func listVersions(tx memdb.Tx, scope domain.Scope, secretID string, after int64, limit int) ([]secrets.SecretVersion, error) {
	var out []secrets.SecretVersion
	var firstErr error
	versionsT.Scan(tx, func(r versionRec) bool {
		if len(out) >= limit {
			return false
		}
		if r.R.Scope == scope && r.R.SecretID == secretID && r.R.Version > after {
			v, err := versionOf(r)
			if err != nil {
				firstErr = err
				return false
			}
			out = append(out, v)
		}
		return true
	})
	return out, firstErr
}

func scopeKeyOf(r scopeKeyRec) (secrets.ScopeKey, error) {
	rec := r.R
	rec.Wrapped = cloneWrapped(rec.Wrapped)
	k, err := secrets.RestoreScopeKey(rec)
	if err != nil {
		return secrets.ScopeKey{}, fmt.Errorf("%w: scope key row: %v", secrets.ErrInconsistent, err)
	}
	return k, nil
}

func activeScopeKey(tx memdb.Tx) (secrets.ScopeKey, error) {
	var best scopeKeyRec
	found := false
	scopeKeysT.Scan(tx, func(r scopeKeyRec) bool {
		if !found || r.R.Generation > best.R.Generation {
			best, found = r, true
		}
		return true
	})
	if !found {
		return secrets.ScopeKey{}, secrets.ErrNotFound
	}
	return scopeKeyOf(best)
}

func getScopeKey(tx memdb.Tx, id string) (secrets.ScopeKey, error) {
	r, ok := scopeKeysT.Get(tx, id)
	if !ok {
		return secrets.ScopeKey{}, secrets.ErrNotFound
	}
	return scopeKeyOf(r)
}

func retentionReport(tx memdb.Tx, scope domain.Scope, secretID string) secrets.RetentionReport {
	r := secrets.RetentionReport{BackupRetention: secrets.BackupRetentionUnresolved}
	pinsT.Scan(tx, func(p pinRec) bool {
		if p.Project == scope.ProjectID && p.SecretID == secretID {
			r.PinnedVersions++
			if p.ReleasedAt == nil {
				r.ActivePins++
			}
		}
		return true
	})
	handlingT.Scan(tx, func(h handlingRec) bool {
		if h.R.Scope == scope && h.R.SecretID == secretID && h.R.State == secrets.HandlingPending {
			r.PendingHandling++
		}
		return true
	})
	return r
}

func handlingOf(r handlingRec) (secrets.ConsumerHandling, error) {
	rec := r.R
	rec.ResolvedAt = cloneTime(rec.ResolvedAt)
	h, err := secrets.RestoreConsumerHandling(rec)
	if err != nil {
		return secrets.ConsumerHandling{}, fmt.Errorf("%w: handling row: %v", secrets.ErrInconsistent, err)
	}
	return h, nil
}

func pendingHandling(tx memdb.Tx, scope domain.Scope, afterID string, limit int) ([]secrets.ConsumerHandling, error) {
	var out []secrets.ConsumerHandling
	var firstErr error
	handlingT.Scan(tx, func(r handlingRec) bool {
		if len(out) >= limit {
			return false
		}
		if r.R.Scope == scope && r.R.State == secrets.HandlingPending && r.R.ID > afterID {
			h, err := handlingOf(r)
			if err != nil {
				firstErr = err
				return false
			}
			out = append(out, h)
		}
		return true
	})
	return out, firstErr
}

func versionHandling(tx memdb.Tx, scope domain.Scope, secretID string, version int64) ([]secrets.ConsumerHandling, error) {
	var out []secrets.ConsumerHandling
	var firstErr error
	handlingT.Scan(tx, func(r handlingRec) bool {
		if r.R.Scope == scope && r.R.SecretID == secretID && r.R.SecretVersion == version {
			h, err := handlingOf(r)
			if err != nil {
				firstErr = err
				return false
			}
			out = append(out, h)
		}
		return true
	})
	return out, firstErr
}

// ---- secrets.Reader on the Store (each call is its own read-only unit) -------------

func (s *MemStore) view(ctx context.Context, tenant domain.TenantID, fn func(tx memdb.Tx) error) error {
	return s.cfg.Store.View(ctx, tenant, fn)
}

func (s *MemStore) GetVariable(ctx context.Context, scope domain.Scope, id string) (v secrets.Variable, err error) {
	err = s.view(ctx, scope.TenantID, func(tx memdb.Tx) (e error) { v, e = getVariable(tx, scope, id); return })
	return v, err
}

func (s *MemStore) ListVariables(ctx context.Context, scope domain.Scope, afterName string, limit int) (out []secrets.Variable, err error) {
	err = s.view(ctx, scope.TenantID, func(tx memdb.Tx) (e error) { out, e = listVariables(tx, scope, afterName, limit); return })
	return out, err
}

func (s *MemStore) GetSecret(ctx context.Context, scope domain.Scope, id string) (sec secrets.Secret, err error) {
	err = s.view(ctx, scope.TenantID, func(tx memdb.Tx) (e error) { sec, e = getSecret(tx, scope, id); return })
	return sec, err
}

func (s *MemStore) ListSecrets(ctx context.Context, scope domain.Scope, afterName string, limit int) (out []secrets.Secret, err error) {
	err = s.view(ctx, scope.TenantID, func(tx memdb.Tx) (e error) { out, e = listSecrets(tx, scope, afterName, limit); return })
	return out, err
}

func (s *MemStore) GetSecretVersion(ctx context.Context, scope domain.Scope, secretID string, version int64) (v secrets.SecretVersion, err error) {
	err = s.view(ctx, scope.TenantID, func(tx memdb.Tx) (e error) { v, e = getVersion(tx, scope, secretID, version); return })
	return v, err
}

func (s *MemStore) ListSecretVersions(ctx context.Context, scope domain.Scope, secretID string, after int64, limit int) (out []secrets.SecretVersion, err error) {
	err = s.view(ctx, scope.TenantID, func(tx memdb.Tx) (e error) { out, e = listVersions(tx, scope, secretID, after, limit); return })
	return out, err
}

func (s *MemStore) ActiveScopeKey(ctx context.Context, tenant domain.TenantID) (k secrets.ScopeKey, err error) {
	err = s.view(ctx, tenant, func(tx memdb.Tx) (e error) { k, e = activeScopeKey(tx); return })
	return k, err
}

func (s *MemStore) GetScopeKey(ctx context.Context, tenant domain.TenantID, id string) (k secrets.ScopeKey, err error) {
	err = s.view(ctx, tenant, func(tx memdb.Tx) (e error) { k, e = getScopeKey(tx, id); return })
	return k, err
}

func (s *MemStore) RetentionReport(ctx context.Context, scope domain.Scope, secretID string) (r secrets.RetentionReport, err error) {
	err = s.view(ctx, scope.TenantID, func(tx memdb.Tx) error {
		if _, e := getSecret(tx, scope, secretID); e != nil {
			return e
		}
		r = retentionReport(tx, scope, secretID)
		return nil
	})
	return r, err
}

func (s *MemStore) PendingHandling(ctx context.Context, scope domain.Scope, afterID string, limit int) (out []secrets.ConsumerHandling, err error) {
	err = s.view(ctx, scope.TenantID, func(tx memdb.Tx) (e error) { out, e = pendingHandling(tx, scope, afterID, limit); return })
	return out, err
}

// ActiveVariable implements secrets.VariableCatalog.
func (s *MemStore) ActiveVariable(ctx context.Context, scope domain.Scope, name string) (v secrets.Variable, err error) {
	err = s.view(ctx, scope.TenantID, func(tx memdb.Tx) error {
		var found variableRec
		ok := false
		variablesT.Scan(tx, func(r variableRec) bool {
			if r.R.Scope == scope && r.R.Name == name && r.R.DeletedAt == nil {
				found, ok = r, true
				return false
			}
			return true
		})
		if !ok {
			return secrets.ErrNotFound
		}
		var e error
		v, e = variableOf(found)
		return e
	})
	return v, err
}

// CurrentVersion implements secrets.SecretCatalog.
func (s *MemStore) CurrentVersion(ctx context.Context, scope domain.Scope, name string) (version int64, err error) {
	err = s.view(ctx, scope.TenantID, func(tx memdb.Tx) error {
		sec, e := getSecretByName(tx, scope, name)
		if e != nil {
			return e
		}
		if sec.Deleted() {
			return secrets.ErrNotFound
		}
		v, e := getVersion(tx, scope, sec.ID(), sec.CurrentVersion())
		if e != nil {
			return e
		}
		if v.Revoked() {
			return secrets.ErrNoActiveVersion
		}
		version = v.Version()
		return nil
	})
	return version, err
}

// ---- Store ---------------------------------------------------------------------

// Atomically implements secrets.Store: fn runs in one memdb.Update. A commit
// that fails after fn succeeded has an unknown outcome and never reports success.
func (s *MemStore) Atomically(ctx context.Context, tenant domain.TenantID, fn func(ctx context.Context, w secrets.Writer) error) error {
	done := false
	err := s.cfg.Store.Update(ctx, tenant, func(tx memdb.Tx) error {
		if err := fn(ctx, &memWriter{s: s, tx: tx, tenant: tenant}); err != nil {
			return err
		}
		done = true
		return nil
	})
	if err != nil && done {
		return fmt.Errorf("%w: commit outcome unknown: %v", secrets.ErrUnavailable, err)
	}
	return err
}

// EnsureScopeKey implements secrets.Store: candidate is stored unless its
// generation is already committed, in which case that key is returned.
func (s *MemStore) EnsureScopeKey(ctx context.Context, m secrets.Mutation, candidate secrets.ScopeKey) (committed secrets.ScopeKey, created bool, err error) {
	if err := m.Validate(); err != nil {
		return secrets.ScopeKey{}, false, err
	}
	rec := candidate.Record()
	if rec.CurrentRevision != 1 {
		return secrets.ScopeKey{}, false, fmt.Errorf("%w: a new scope key starts at wrapping revision 1", secrets.ErrInvalid)
	}
	done := false
	err = s.cfg.Store.Update(ctx, rec.TenantID, func(tx memdb.Tx) error {
		created = false
		var existing scopeKeyRec
		found := false
		scopeKeysT.Scan(tx, func(r scopeKeyRec) bool {
			if r.R.Generation == rec.Generation {
				existing, found = r, true
				return false
			}
			return true
		})
		if found {
			var e error
			committed, e = scopeKeyOf(existing)
			done = true
			return e
		}
		stored := rec
		stored.Wrapped = cloneWrapped(rec.Wrapped)
		if err := scopeKeysT.Put(tx, scopeKeyRec{R: stored}); err != nil {
			return err
		}
		if err := wrapsT.Put(tx, wrapRec{KeyID: rec.ID, Revision: 1, Wrapped: cloneWrapped(rec.Wrapped), At: rec.WrappedAt}); err != nil {
			return err
		}
		created, committed = true, candidate
		if err := s.audit(tx, m, auditEntry{Scope: domain.Scope{TenantID: rec.TenantID}, Action: "secret_scope_key.create", ResourceType: "secret_scope_key",
			ResourceID: rec.ID, ResourceVersion: 1, Metadata: map[string]any{"generation": rec.Generation, "kek": kekMeta(rec.Wrapped.KEK)}}); err != nil {
			return err
		}
		done = true
		return nil
	})
	if err != nil {
		if done {
			return secrets.ScopeKey{}, false, fmt.Errorf("%w: commit outcome unknown: %v", secrets.ErrUnavailable, err)
		}
		return secrets.ScopeKey{}, false, err
	}
	return committed, created, nil
}

func kekMeta(r secrets.KEKRef) map[string]any {
	return map[string]any{"provider": r.Provider, "id": r.ID, "version": r.Version}
}

// ---- Writer --------------------------------------------------------------------

type memWriter struct {
	s      *MemStore
	tx     memdb.Tx
	tenant domain.TenantID
}

var _ secrets.Writer = (*memWriter)(nil)

func (w *memWriter) checkScope(scope domain.Scope) error {
	if scope.TenantID != w.tenant {
		return fmt.Errorf("%w: scope is outside the transaction's tenant", secrets.ErrNotFound)
	}
	return nil
}

func (w *memWriter) GetVariable(_ context.Context, scope domain.Scope, id string) (secrets.Variable, error) {
	if err := w.checkScope(scope); err != nil {
		return secrets.Variable{}, err
	}
	return getVariable(w.tx, scope, id)
}

func (w *memWriter) ListVariables(_ context.Context, scope domain.Scope, afterName string, limit int) ([]secrets.Variable, error) {
	if err := w.checkScope(scope); err != nil {
		return nil, err
	}
	return listVariables(w.tx, scope, afterName, limit)
}

func (w *memWriter) GetSecret(_ context.Context, scope domain.Scope, id string) (secrets.Secret, error) {
	if err := w.checkScope(scope); err != nil {
		return secrets.Secret{}, err
	}
	return getSecret(w.tx, scope, id)
}

func (w *memWriter) ListSecrets(_ context.Context, scope domain.Scope, afterName string, limit int) ([]secrets.Secret, error) {
	if err := w.checkScope(scope); err != nil {
		return nil, err
	}
	return listSecrets(w.tx, scope, afterName, limit)
}

func (w *memWriter) GetSecretVersion(_ context.Context, scope domain.Scope, secretID string, version int64) (secrets.SecretVersion, error) {
	if err := w.checkScope(scope); err != nil {
		return secrets.SecretVersion{}, err
	}
	return getVersion(w.tx, scope, secretID, version)
}

func (w *memWriter) ListSecretVersions(_ context.Context, scope domain.Scope, secretID string, after int64, limit int) ([]secrets.SecretVersion, error) {
	if err := w.checkScope(scope); err != nil {
		return nil, err
	}
	return listVersions(w.tx, scope, secretID, after, limit)
}

func (w *memWriter) ActiveScopeKey(_ context.Context, tenant domain.TenantID) (secrets.ScopeKey, error) {
	if tenant != w.tenant {
		return secrets.ScopeKey{}, secrets.ErrNotFound
	}
	return activeScopeKey(w.tx)
}

func (w *memWriter) GetScopeKey(_ context.Context, tenant domain.TenantID, id string) (secrets.ScopeKey, error) {
	if tenant != w.tenant {
		return secrets.ScopeKey{}, secrets.ErrNotFound
	}
	return getScopeKey(w.tx, id)
}

func (w *memWriter) RetentionReport(_ context.Context, scope domain.Scope, secretID string) (secrets.RetentionReport, error) {
	if err := w.checkScope(scope); err != nil {
		return secrets.RetentionReport{}, err
	}
	if _, err := getSecret(w.tx, scope, secretID); err != nil {
		return secrets.RetentionReport{}, err
	}
	return retentionReport(w.tx, scope, secretID), nil
}

func (w *memWriter) PendingHandling(_ context.Context, scope domain.Scope, afterID string, limit int) ([]secrets.ConsumerHandling, error) {
	if err := w.checkScope(scope); err != nil {
		return nil, err
	}
	return pendingHandling(w.tx, scope, afterID, limit)
}

func (w *memWriter) CreateVariable(_ context.Context, m secrets.Mutation, v secrets.Variable) error {
	rec := v.Record()
	if err := w.checkScope(rec.Scope); err != nil {
		return err
	}
	rec.DeletedAt = cloneTime(rec.DeletedAt)
	if _, taken := variablesT.Get(w.tx, string(rec.Scope.ProjectID)+"/"+rec.ID); taken {
		// Put replaces a row with the same key: refuse before it can.
		return fmt.Errorf("%w: variable identity %s is already in use", domain.ErrConflict, rec.ID)
	}
	if err := variablesT.Put(w.tx, variableRec{R: rec}); err != nil {
		if errors.Is(err, memdb.ErrConflict) {
			return secrets.ErrNameTaken
		}
		return err
	}
	if err := w.s.audit(w.tx, m, auditEntry{Scope: rec.Scope, Action: "variable.create", ResourceType: "variable", ResourceID: rec.ID,
		ResourceVersion: int64(rec.Version), Metadata: map[string]any{"name": rec.Name, "type": string(rec.Type), "valueVersion": rec.ValueVersion}}); err != nil {
		return err
	}
	return w.s.emit(w.tx, m, eventSpec{Scope: rec.Scope, Kind: "variable", ResourceID: rec.ID, Version: rec.ValueVersion, Change: "created", ScopeName: "project"})
}

func (w *memWriter) EditVariable(_ context.Context, m secrets.Mutation, scope domain.Scope, id string, expected domain.Version, edit secrets.VariableEdit) (secrets.Variable, error) {
	if err := w.checkScope(scope); err != nil {
		return secrets.Variable{}, err
	}
	v, err := getVariable(w.tx, scope, id)
	if err != nil {
		return secrets.Variable{}, err
	}
	valueChanged, changed, err := v.Edit(expected, edit, m.Now)
	if err != nil || !changed {
		return v, err
	}
	rec := v.Record()
	if err := variablesT.Put(w.tx, variableRec{R: rec}); err != nil {
		return secrets.Variable{}, err
	}
	if err := w.s.audit(w.tx, m, auditEntry{Scope: scope, Action: "variable.update", ResourceType: "variable", ResourceID: id,
		ResourceVersion: int64(rec.Version), Metadata: map[string]any{"name": rec.Name, "valueChanged": valueChanged, "valueVersion": rec.ValueVersion}}); err != nil {
		return secrets.Variable{}, err
	}
	if err := w.s.emit(w.tx, m, eventSpec{Scope: scope, Kind: "variable", ResourceID: id, Version: rec.ValueVersion, Change: "updated", ScopeName: "project"}); err != nil {
		return secrets.Variable{}, err
	}
	return v, nil
}

func (w *memWriter) DeleteVariable(_ context.Context, m secrets.Mutation, scope domain.Scope, id string, expected domain.Version) (secrets.Variable, error) {
	if err := w.checkScope(scope); err != nil {
		return secrets.Variable{}, err
	}
	v, err := getVariable(w.tx, scope, id)
	if err != nil {
		return secrets.Variable{}, err
	}
	changed, err := v.Delete(expected, m.Now)
	if err != nil || !changed {
		return v, err
	}
	rec := v.Record()
	if err := variablesT.Put(w.tx, variableRec{R: rec}); err != nil {
		return secrets.Variable{}, err
	}
	if err := w.s.audit(w.tx, m, auditEntry{Scope: scope, Action: "variable.delete", ResourceType: "variable", ResourceID: id,
		ResourceVersion: int64(rec.Version), Metadata: map[string]any{"name": rec.Name, "valueVersion": rec.ValueVersion}}); err != nil {
		return secrets.Variable{}, err
	}
	if err := w.s.emit(w.tx, m, eventSpec{Scope: scope, Kind: "variable", ResourceID: id, Version: rec.ValueVersion, Change: "deleted", ScopeName: "project"}); err != nil {
		return secrets.Variable{}, err
	}
	return v, nil
}

func (w *memWriter) putVersion(v secrets.SecretVersion) error {
	r := v.Record()
	if _, ok := scopeKeysT.Get(w.tx, r.ScopeKeyID); !ok {
		return fmt.Errorf("%w: unknown scope key", secrets.ErrInvalid)
	}
	if key, _ := scopeKeysT.Get(w.tx, r.ScopeKeyID); key.R.Generation != r.KeyGeneration {
		return fmt.Errorf("%w: unknown scope key", secrets.ErrInvalid)
	}
	r.Envelope.Nonce = append([]byte(nil), r.Envelope.Nonce...)
	r.Envelope.Ciphertext = append([]byte(nil), r.Envelope.Ciphertext...)
	r.RevokedAt = cloneTime(r.RevokedAt)
	if _, taken := versionsT.Get(w.tx, fmt.Sprintf("%s/%s/%020d", r.Scope.ProjectID, r.SecretID, r.Version)); taken {
		return fmt.Errorf("%w: secret version %d is already stored", domain.ErrConflict, r.Version)
	}
	return versionsT.Put(w.tx, versionRec{R: r})
}

func (w *memWriter) CreateSecret(_ context.Context, m secrets.Mutation, s secrets.Secret, first secrets.SecretVersion) error {
	rec := s.Record()
	if err := w.checkScope(rec.Scope); err != nil {
		return err
	}
	if rec.CurrentVersion != 1 || first.Version() != 1 || first.SecretID() != rec.ID || first.Record().Scope != rec.Scope {
		return fmt.Errorf("%w: a new secret is created with exactly its first version", secrets.ErrInvalid)
	}
	rec.DeletedAt = cloneTime(rec.DeletedAt)
	if _, taken := secretsT.Get(w.tx, string(rec.Scope.ProjectID)+"/"+rec.ID); taken {
		return fmt.Errorf("%w: secret identity %s is already in use", domain.ErrConflict, rec.ID)
	}
	if err := secretsT.Put(w.tx, secretRec{R: rec}); err != nil {
		if errors.Is(err, memdb.ErrConflict) {
			return secrets.ErrNameTaken
		}
		return err
	}
	if err := w.putVersion(first); err != nil {
		return err
	}
	if err := w.s.audit(w.tx, m, auditEntry{Scope: rec.Scope, Action: "secret.create", ResourceType: "secret", ResourceID: rec.ID,
		ResourceVersion: int64(rec.Version), Metadata: map[string]any{"name": rec.Name, "kind": string(rec.Kind), "valueVersion": 1}}); err != nil {
		return err
	}
	return w.s.emit(w.tx, m, eventSpec{Scope: rec.Scope, Kind: "secret", ResourceID: rec.ID, Version: 1, Change: "created", ScopeName: "project"})
}

func (w *memWriter) AddSecretVersion(_ context.Context, m secrets.Mutation, scope domain.Scope, id string, expected domain.Version, v secrets.SecretVersion) (secrets.Secret, error) {
	if err := w.checkScope(scope); err != nil {
		return secrets.Secret{}, err
	}
	s, err := getSecret(w.tx, scope, id)
	if err != nil {
		return secrets.Secret{}, err
	}
	if err := s.AcceptVersion(expected, v, m.Now); err != nil {
		return secrets.Secret{}, err
	}
	rec := s.Record()
	if err := w.putVersion(v); err != nil {
		return secrets.Secret{}, err
	}
	if err := secretsT.Put(w.tx, secretRec{R: rec}); err != nil {
		return secrets.Secret{}, err
	}
	if err := w.s.audit(w.tx, m, auditEntry{Scope: scope, Action: "secret.rotate", ResourceType: "secret", ResourceID: id,
		ResourceVersion: int64(rec.Version), Metadata: map[string]any{"name": rec.Name, "kind": string(rec.Kind), "valueVersion": rec.CurrentVersion}}); err != nil {
		return secrets.Secret{}, err
	}
	if err := w.s.emit(w.tx, m, eventSpec{Scope: scope, Kind: "secret", ResourceID: id, Version: rec.CurrentVersion, Change: "rotated", ScopeName: "project"}); err != nil {
		return secrets.Secret{}, err
	}
	return s, nil
}

func (w *memWriter) RevokeSecretVersion(_ context.Context, m secrets.Mutation, scope domain.Scope, id string, version int64) (secrets.RevocationResult, error) {
	if err := w.checkScope(scope); err != nil {
		return secrets.RevocationResult{}, err
	}
	s, err := getSecret(w.tx, scope, id)
	if err != nil {
		return secrets.RevocationResult{}, err
	}
	v, err := getVersion(w.tx, scope, id, version)
	if err != nil {
		return secrets.RevocationResult{}, err
	}
	changed, err := s.RevokeVersion(&v, m.Actor.ID, m.Now)
	if err != nil {
		return secrets.RevocationResult{}, err
	}
	res := secrets.RevocationResult{Secret: s, Version: v, Changed: changed}
	if !changed {
		res.Handling, err = versionHandling(w.tx, scope, id, version)
		return res, err
	}
	vr, rec := v.Record(), s.Record()
	if err := versionsT.Put(w.tx, versionRec{R: vr}); err != nil {
		return secrets.RevocationResult{}, err
	}
	if err := secretsT.Put(w.tx, secretRec{R: rec}); err != nil {
		return secrets.RevocationResult{}, err
	}
	if res.Handling, err = w.recordHandling(m, scope, id, version); err != nil {
		return secrets.RevocationResult{}, err
	}
	pending := 0
	for _, h := range res.Handling {
		if h.Pending() {
			pending++
		}
	}
	if err := w.s.audit(w.tx, m, auditEntry{Scope: scope, Action: "secret.revoke_version", ResourceType: "secret", ResourceID: id,
		ResourceVersion: int64(rec.Version), Metadata: map[string]any{"name": rec.Name, "revokedVersion": version, "pendingHandling": pending}}); err != nil {
		return secrets.RevocationResult{}, err
	}
	// The merged vocabulary has no revoked change: map it to updated.
	if err := w.s.emit(w.tx, m, eventSpec{Scope: scope, Kind: "secret", ResourceID: id, Version: version, Change: "updated", ScopeName: "project"}); err != nil {
		return secrets.RevocationResult{}, err
	}
	return res, nil
}

// recordHandling creates the pending effects for every unreleased pin of the
// revoked version. Delivered bytes are not recalled by these records.
func (w *memWriter) recordHandling(m secrets.Mutation, scope domain.Scope, secretID string, version int64) ([]secrets.ConsumerHandling, error) {
	var pins []pinRec
	pinsT.Scan(w.tx, func(p pinRec) bool {
		if p.Project == scope.ProjectID && p.SecretID == secretID && p.Version == version && p.ReleasedAt == nil {
			pins = append(pins, p)
		}
		return true
	})
	sort.Slice(pins, func(i, j int) bool {
		if pins[i].Consumer.Kind != pins[j].Consumer.Kind {
			return pins[i].Consumer.Kind < pins[j].Consumer.Kind
		}
		return pins[i].Consumer.ID < pins[j].Consumer.ID
	})
	var out []secrets.ConsumerHandling
	for _, p := range pins {
		for _, effect := range secrets.Effects() {
			hid, err := w.s.cfg.IDs.NewID("ch")
			if err != nil {
				return nil, err
			}
			h, err := secrets.NewConsumerHandling(scope, hid, secretID, version, p.Consumer, effect, p.FirstResolvedAt != nil, m.Now)
			if err != nil {
				return nil, err
			}
			if err := handlingT.Put(w.tx, handlingRec{R: h.Record()}); err != nil && !errors.Is(err, memdb.ErrConflict) {
				return nil, err
			}
			out = append(out, h)
		}
	}
	return out, nil
}

func (w *memWriter) DeleteSecret(_ context.Context, m secrets.Mutation, scope domain.Scope, id string, expected domain.Version) (secrets.Secret, secrets.RetentionReport, error) {
	if err := w.checkScope(scope); err != nil {
		return secrets.Secret{}, secrets.RetentionReport{}, err
	}
	s, err := getSecret(w.tx, scope, id)
	if err != nil {
		return secrets.Secret{}, secrets.RetentionReport{}, err
	}
	changed, err := s.Delete(expected, m.Now)
	if err != nil {
		return secrets.Secret{}, secrets.RetentionReport{}, err
	}
	if changed {
		if err := secretsT.Put(w.tx, secretRec{R: s.Record()}); err != nil {
			return secrets.Secret{}, secrets.RetentionReport{}, err
		}
	}
	report := retentionReport(w.tx, scope, id)
	if changed {
		rec := s.Record()
		if err := w.s.audit(w.tx, m, auditEntry{Scope: scope, Action: "secret.delete", ResourceType: "secret", ResourceID: id,
			ResourceVersion: int64(rec.Version), Metadata: map[string]any{
				"name": rec.Name, "pins": report.PinnedVersions, "activePins": report.ActivePins,
				"pendingHandling": report.PendingHandling, "backupRetention": string(report.BackupRetention),
			}}); err != nil {
			return secrets.Secret{}, secrets.RetentionReport{}, err
		}
		if err := w.s.emit(w.tx, m, eventSpec{Scope: scope, Kind: "secret", ResourceID: id, Version: rec.CurrentVersion, Change: "deleted", ScopeName: "project"}); err != nil {
			return secrets.Secret{}, secrets.RetentionReport{}, err
		}
	}
	return s, report, nil
}

func (w *memWriter) RewrapScopeKey(_ context.Context, m secrets.Mutation, tenant domain.TenantID, keyID string, expectedRevision int64, wk secrets.WrappedKey) (secrets.ScopeKey, error) {
	if tenant != w.tenant {
		return secrets.ScopeKey{}, secrets.ErrNotFound
	}
	k, err := getScopeKey(w.tx, keyID)
	if err != nil {
		return secrets.ScopeKey{}, err
	}
	if err := k.Rewrap(expectedRevision, wk, m.Now); err != nil {
		return secrets.ScopeKey{}, err
	}
	rec := k.Record()
	// The new wrap is added; the previous revisions stay as history.
	if err := wrapsT.Put(w.tx, wrapRec{KeyID: keyID, Revision: rec.CurrentRevision, Wrapped: cloneWrapped(rec.Wrapped), At: rec.WrappedAt}); err != nil {
		return secrets.ScopeKey{}, err
	}
	stored := rec
	stored.Wrapped = cloneWrapped(rec.Wrapped)
	if err := scopeKeysT.Put(w.tx, scopeKeyRec{R: stored}); err != nil {
		return secrets.ScopeKey{}, err
	}
	scope := domain.Scope{TenantID: tenant}
	if err := w.s.audit(w.tx, m, auditEntry{Scope: scope, Action: "secret_scope_key.rewrap", ResourceType: "secret_scope_key", ResourceID: keyID,
		ResourceVersion: rec.CurrentRevision, Metadata: map[string]any{"generation": rec.Generation, "kek": kekMeta(rec.Wrapped.KEK), "previousRevision": expectedRevision}}); err != nil {
		return secrets.ScopeKey{}, err
	}
	if err := w.s.emit(w.tx, m, eventSpec{Scope: scope, Kind: "secret", ResourceID: keyID, Version: rec.CurrentRevision, Change: "updated", ScopeName: "tenant"}); err != nil {
		return secrets.ScopeKey{}, err
	}
	return k, nil
}

func (w *memWriter) AcknowledgeHandling(_ context.Context, m secrets.Mutation, scope domain.Scope, id string, outcome secrets.HandlingState) (secrets.ConsumerHandling, error) {
	if err := w.checkScope(scope); err != nil {
		return secrets.ConsumerHandling{}, err
	}
	r, ok := handlingT.Get(w.tx, id)
	if !ok || r.R.Scope != scope {
		return secrets.ConsumerHandling{}, secrets.ErrNotFound
	}
	h, err := handlingOf(r)
	if err != nil {
		return secrets.ConsumerHandling{}, err
	}
	changed, err := h.Acknowledge(outcome, m.Actor.ID, m.Now)
	if err != nil || !changed {
		return h, err
	}
	rec := h.Record()
	if err := handlingT.Put(w.tx, handlingRec{R: rec}); err != nil {
		return secrets.ConsumerHandling{}, err
	}
	if err := w.s.audit(w.tx, m, auditEntry{Scope: scope, Action: "secret.handling_acknowledged", ResourceType: "secret", ResourceID: rec.SecretID,
		ResourceVersion: rec.SecretVersion, Metadata: map[string]any{"handlingId": id, "effect": string(rec.Effect), "outcome": string(rec.State),
			"consumerKind": rec.Consumer.Kind, "consumerId": rec.Consumer.ID}}); err != nil {
		return secrets.ConsumerHandling{}, err
	}
	return h, nil
}

// ---- pins and resolution ---------------------------------------------------------------

// PinRequest registers the secret versions one consumer retains.
type PinRequest struct {
	Scope    domain.Scope
	Consumer secrets.ConsumerRef
	// Bindings are the pinned references of the effective snapshot.
	Bindings []runtimev1.SecretBinding
}

// RegisterPins records, in the caller's memdb.Tx, that the consumer retains each
// pinned secret version, as postgres.RegisterPins does in its pgx.Tx: the run
// admission calls it in the transaction that creates the run, so a run never
// exists with unregistered pins and a pin never exists for a run that did not
// commit. Registering the same pin again is a no-op. Refusals: ErrNotFound (no
// such secret or version in the project), ErrDeleted, ErrRevoked.
func (s *MemStore) RegisterPins(tx memdb.Tx, req PinRequest) error {
	if err := req.Scope.Validate(); err != nil {
		return err
	}
	if err := req.Consumer.Validate(); err != nil {
		return err
	}
	if tx.Tenant() != req.Scope.TenantID {
		return fmt.Errorf("%w: transaction tenant differs from the request's tenant", secrets.ErrNotFound)
	}
	bs := append([]runtimev1.SecretBinding(nil), req.Bindings...)
	sort.Slice(bs, func(i, j int) bool {
		if bs[i].Ref != bs[j].Ref {
			return bs[i].Ref < bs[j].Ref
		}
		return bs[i].Version < bs[j].Version
	})
	now := s.now()
	for _, b := range bs {
		name, err := secrets.ParseSecretRef(b.Ref)
		if err != nil {
			return err
		}
		if b.Version < 1 || b.Version > domain.MaxSafeInteger {
			return fmt.Errorf("%w: pinned version out of range", secrets.ErrInvalid)
		}
		sec, err := getSecretByName(tx, req.Scope, name)
		if err != nil {
			return err
		}
		if sec.Deleted() {
			return fmt.Errorf("%w: %s", secrets.ErrDeleted, b.Ref)
		}
		v, err := getVersion(tx, req.Scope, sec.ID(), b.Version)
		if err != nil {
			return err
		}
		if v.Revoked() {
			return fmt.Errorf("%w: %s version %d", secrets.ErrRevoked, b.Ref, b.Version)
		}
		key := pinKey(req.Consumer, sec.ID(), b.Version)
		if _, exists := pinsT.Get(tx, key); exists {
			continue
		}
		if err := pinsT.Put(tx, pinRec{Project: req.Scope.ProjectID, Consumer: req.Consumer, SecretID: sec.ID(), Version: b.Version, CreatedAt: now}); err != nil {
			return err
		}
	}
	return nil
}

// ReleasePins marks every pin of the consumer in the project released, in the
// consumer's own termination Tx, and returns the number newly released.
func (s *MemStore) ReleasePins(tx memdb.Tx, scope domain.Scope, consumer secrets.ConsumerRef) (int64, error) {
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	if err := consumer.Validate(); err != nil {
		return 0, err
	}
	if tx.Tenant() != scope.TenantID {
		return 0, fmt.Errorf("%w: transaction tenant differs from the request's tenant", secrets.ErrNotFound)
	}
	now := s.now()
	var due []pinRec
	pinsT.Scan(tx, func(p pinRec) bool {
		if p.Project == scope.ProjectID && p.Consumer == consumer && p.ReleasedAt == nil {
			due = append(due, p)
		}
		return true
	})
	for _, p := range due {
		p.ReleasedAt = &now
		if err := pinsT.Put(tx, p); err != nil {
			return 0, err
		}
	}
	return int64(len(due)), nil
}

// HasPin reports whether the consumer retains the exact version, and whether it
// was released.
func (s *MemStore) HasPin(ctx context.Context, scope domain.Scope, consumer secrets.ConsumerRef, secretID string, version int64) (found, released bool, err error) {
	err = s.view(ctx, scope.TenantID, func(tx memdb.Tx) error {
		if p, ok := pinsT.Get(tx, pinKey(consumer, secretID, version)); ok && p.Project == scope.ProjectID {
			found, released = true, p.ReleasedAt != nil
		}
		return nil
	})
	return found, released, err
}
