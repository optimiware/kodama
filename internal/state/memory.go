package state

import (
	"context"
	"errors"
	"time"

	statev1 "github.com/optimiware/kodama/contracts/state/v1"
	"github.com/optimiware/kodama/internal/domain"
)

var (
	ErrNotFound    = errors.New("state: not found")
	ErrInvalid     = errors.New("state: invalid")
	ErrConflict    = errors.New("state: version conflict")
	ErrExists      = errors.New("state: key already exists")
	ErrDenied      = errors.New("state: denied")
	ErrUnavailable = errors.New("state: unavailable")
)

// Scope is the tenant/project ownership of a state namespace.
type Scope struct {
	TenantID  domain.TenantID
	ProjectID domain.ProjectID
}

func (s Scope) Validate() error {
	return domain.Scope{TenantID: s.TenantID, ProjectID: s.ProjectID}.Validate()
}

// Binding is the server-authorized view of one state alias. The server derives
// it from current runtime authority; a caller never supplies it directly.
type Binding struct {
	Namespace string
	Prefix    string
	Read      bool
	Write     bool
}

// Store is the small persistence port used by the state use cases. It owns
// conditional mutation and never exposes SQL or driver values.
type Store interface {
	Get(ctx context.Context, scope Scope, namespace, key string) (statev1.Entry, error)
	List(ctx context.Context, scope Scope, namespace, prefix, cursor string, limit int) ([]statev1.Entry, string, error)
	Create(ctx context.Context, scope Scope, namespace, key, actor string, req statev1.WriteRequest) (statev1.Entry, error)
	Update(ctx context.Context, scope Scope, namespace, key, actor, expected string, req statev1.WriteRequest) (statev1.Entry, error)
	Delete(ctx context.Context, scope Scope, namespace, key, expected string) error
}

// Memory is the in-memory Store used by unit tests and development. Its
// conditional semantics deliberately match the PostgreSQL contract.
type Memory struct {
	now         func() time.Time
	entries     map[memKey]statev1.Entry
	generations map[memKey]int64
}

type memKey struct {
	scope     Scope
	namespace string
	key       string
}

// NewMemory constructs an empty memory store.
func NewMemory(now func() time.Time) *Memory {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Memory{now: now, entries: map[memKey]statev1.Entry{}, generations: map[memKey]int64{}}
}

func (m *Memory) Get(_ context.Context, scope Scope, namespace, key string) (statev1.Entry, error) {
	if err := validateLookup(scope, namespace, key); err != nil {
		return statev1.Entry{}, err
	}
	e, ok := m.entries[memKey{scope, namespace, key}]
	if !ok {
		return statev1.Entry{}, ErrNotFound
	}
	return cloneEntry(e), nil
}

func (m *Memory) List(_ context.Context, scope Scope, namespace, prefix, cursor string, limit int) ([]statev1.Entry, string, error) {
	if err := scope.Validate(); err != nil {
		return nil, "", invalid(err)
	}
	if namespace == "" {
		return nil, "", invalid(errors.New("namespace is required"))
	}
	if limit <= 0 || limit > statev1.MaxListLimit {
		limit = statev1.DefaultListLimit
	}
	keys := make([]string, 0)
	for k := range m.entries {
		if k.scope != scope || k.namespace != namespace || !statev1.InPrefix(prefix, k.key) {
			continue
		}
		if cursor != "" && k.key <= cursor {
			continue
		}
		keys = append(keys, k.key)
	}
	sortStrings(keys)
	items := make([]statev1.Entry, 0, limit)
	next := ""
	for _, key := range keys {
		if len(items) == limit {
			next = key
			break
		}
		items = append(items, cloneEntry(m.entries[memKey{scope, namespace, key}]))
	}
	return items, next, nil
}

func (m *Memory) Create(_ context.Context, scope Scope, namespace, key, actor string, req statev1.WriteRequest) (statev1.Entry, error) {
	if err := validateWrite(scope, namespace, key, req); err != nil {
		return statev1.Entry{}, err
	}
	mk := memKey{scope, namespace, key}
	if _, ok := m.entries[mk]; ok {
		return statev1.Entry{}, ErrExists
	}
	now := m.now().UTC()
	generation := m.generations[mk] + 1
	m.generations[mk] = generation
	e := statev1.Entry{
		Namespace: namespace, Key: key, Value: cloneRaw(req.Value),
		ArtifactReferences: cloneRefs(req.ArtifactReferences), Generation: generation, Revision: 1,
		UpdatedAt: now, UpdatedBy: actor,
	}
	m.entries[mk] = e
	return cloneEntry(e), nil
}

func (m *Memory) Update(_ context.Context, scope Scope, namespace, key, actor, expected string, req statev1.WriteRequest) (statev1.Entry, error) {
	if err := validateWrite(scope, namespace, key, req); err != nil {
		return statev1.Entry{}, err
	}
	mk := memKey{scope, namespace, key}
	old, ok := m.entries[mk]
	if !ok {
		return statev1.Entry{}, ErrNotFound
	}
	if expected == "" || expected != old.ETag() {
		return statev1.Entry{}, ErrConflict
	}
	e := old
	e.Value = cloneRaw(req.Value)
	e.ArtifactReferences = cloneRefs(req.ArtifactReferences)
	e.Revision++
	e.UpdatedAt = m.now().UTC()
	e.UpdatedBy = actor
	m.entries[mk] = e
	return cloneEntry(e), nil
}

func (m *Memory) Delete(_ context.Context, scope Scope, namespace, key, expected string) error {
	if err := validateLookup(scope, namespace, key); err != nil {
		return err
	}
	mk := memKey{scope, namespace, key}
	old, ok := m.entries[mk]
	if !ok {
		return ErrNotFound
	}
	if expected == "" || expected != old.ETag() {
		return ErrConflict
	}
	delete(m.entries, mk)
	return nil
}

func validateLookup(scope Scope, namespace, key string) error {
	if err := scope.Validate(); err != nil {
		return invalid(err)
	}
	if namespace == "" {
		return invalid(errors.New("namespace is required"))
	}
	if err := statev1.ValidateKey(key); err != nil {
		return invalid(err)
	}
	return nil
}

func validateWrite(scope Scope, namespace, key string, req statev1.WriteRequest) error {
	if err := validateLookup(scope, namespace, key); err != nil {
		return err
	}
	if err := req.Validate(); err != nil {
		return invalid(err)
	}
	return nil
}

func invalid(err error) error { return errors.Join(ErrInvalid, err) }

func cloneEntry(e statev1.Entry) statev1.Entry {
	e.Value = cloneRaw(e.Value)
	e.ArtifactReferences = cloneRefs(e.ArtifactReferences)
	return e
}

func cloneRaw(v []byte) []byte { return append([]byte(nil), v...) }

func cloneRefs(in []statev1.ArtifactReference) []statev1.ArtifactReference {
	if len(in) == 0 {
		return nil
	}
	out := make([]statev1.ArtifactReference, len(in))
	copy(out, in)
	return out
}

func sortStrings(v []string) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}
