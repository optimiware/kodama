package kv

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/shell/event"
	"github.com/optimiware/kodema/internal/shell/store"
	"github.com/optimiware/kodema/internal/shell/tenancy"
)

// Clock supplies the authoritative commit timestamp.
type Clock interface {
	Now() time.Time
}

// IDSource mints audit row identities for KV writes.
type IDSource interface {
	NewEventID() (id.EventID, error)
}

// PutRecord is the validated, server-addressed object handed to ObjectStore.Put
// along with the commit-boundary authority locks and audit row.
type PutRecord struct {
	RowID    string
	Metadata Metadata
	Data     []byte
	Locks    []store.Lock
	Audit    []event.Audit
}

// ObjectStore is the pluggable project-scoped content-addressed blob storage
// port. The default StoreBackend stores objects directly inside the relational
// store.Executor (memory or PostgreSQL under forced RLS) so local DB storage
// works out of the box across development, test, and production; an external or
// local object backend can plug behind this same seam.
type ObjectStore interface {
	Put(ctx context.Context, record PutRecord) (Metadata, bool, error)
	Get(ctx context.Context, scope tenancy.Scope, key Key) (Object, error)
}

// StoreBackend is the default ObjectStore backed by store.Executor over the
// kv_objects table.
type StoreBackend struct {
	executor store.Executor
}

// NewStoreBackend constructs the default relational-store ObjectStore.
func NewStoreBackend(executor store.Executor) (*StoreBackend, error) {
	if executor == nil {
		return nil, fault.New(fault.InvalidInput, "kv store backend requires a store executor")
	}
	return &StoreBackend{executor: executor}, nil
}

// Put writes a new immutable object or returns the existing record when the
// identical (tenant_id, project_id, key) is already present.
func (backend *StoreBackend) Put(ctx context.Context, record PutRecord) (Metadata, bool, error) {
	if existing, found, err := backend.readExisting(ctx, record.Metadata.Scope, record.RowID, record.Metadata.Key, record.Data); err != nil {
		return Metadata{}, false, err
	} else if found {
		return existing, true, nil
	}

	dataCopy := make([]byte, len(record.Data))
	copy(dataCopy, record.Data)
	unit := store.Unit{
		Locks: record.Locks,
		Writes: []store.Write{{
			Table:  store.KVObjectsTable,
			ID:     record.RowID,
			Insert: true,
			Values: map[string]any{
				"key":          string(record.Metadata.Key),
				"content_type": record.Metadata.ContentType,
				"size_bytes":   record.Metadata.SizeBytes,
				"data":         dataCopy,
				"created_by":   string(record.Metadata.CreatedBy),
				"created_at":   record.Metadata.CreatedAt,
			},
		}},
		Audit: record.Audit,
	}
	if _, err := backend.executor.Execute(ctx, record.Metadata.Scope, unit); err != nil {
		return Metadata{}, false, err
	}
	return record.Metadata, false, nil
}

// Get reads one object by its canonical key inside scope and verifies content
// integrity against the requested SHA-256 key before returning.
func (backend *StoreBackend) Get(ctx context.Context, scope tenancy.Scope, key Key) (Object, error) {
	rowID, err := RowID(scope, key)
	if err != nil {
		return Object{}, err
	}
	row, err := backend.executor.Read(ctx, scope, store.KVObjectsTable, rowID)
	if err != nil {
		return Object{}, err
	}
	return objectFromRow(row, scope, key)
}

func (backend *StoreBackend) readExisting(ctx context.Context, scope tenancy.Scope, rowID string, key Key, want []byte) (Metadata, bool, error) {
	row, err := backend.executor.Read(ctx, scope, store.KVObjectsTable, rowID)
	if err != nil {
		if isNotFound(err) {
			return Metadata{}, false, nil
		}
		return Metadata{}, false, err
	}
	obj, err := objectFromRow(row, scope, key)
	if err != nil {
		return Metadata{}, false, err
	}
	if !bytes.Equal(obj.Data, want) {
		return Metadata{}, false, fault.New(fault.InternalFailure, "stored kv object bytes do not match content hash")
	}
	return obj.Metadata, true, nil
}

func objectFromRow(row store.Row, scope tenancy.Scope, wantKey Key) (Object, error) {
	tenantID, ok := row.Values["tenant_id"].(string)
	if !ok || tenantID == "" {
		return Object{}, fault.New(fault.InternalFailure, "stored kv object has no usable tenant id")
	}
	projectID, ok := row.Values["project_id"].(string)
	if !ok || projectID == "" {
		return Object{}, fault.New(fault.InternalFailure, "stored kv object has no usable project id")
	}
	storedScope := tenancy.Scope{TenantID: id.TenantID(tenantID), ProjectID: id.ProjectID(projectID)}
	if storedScope != scope {
		return Object{}, fault.New(fault.Forbidden, "stored kv object belongs to another scope")
	}
	keyText, ok := row.Values["key"].(string)
	if !ok {
		return Object{}, fault.New(fault.InternalFailure, "stored kv object has no usable key")
	}
	parsedKey, err := ParseKey(keyText)
	if err != nil {
		return Object{}, fault.Wrap(fault.InternalFailure, "stored kv object has an invalid key", err)
	}
	if wantKey != "" && parsedKey != wantKey {
		return Object{}, fault.New(fault.InternalFailure, "stored kv object key does not match requested key")
	}
	contentType, ok := row.Values["content_type"].(string)
	if !ok || contentType == "" {
		return Object{}, fault.New(fault.InternalFailure, "stored kv object has no usable content_type")
	}
	sizeBytes, err := rowInt64(row.Values["size_bytes"])
	if err != nil {
		return Object{}, err
	}
	rawData, ok := row.Values["data"].([]byte)
	if !ok {
		// An empty bytea may be decoded as nil by some drivers; treat nil as
		// empty only when size_bytes == 0.
		if row.Values["data"] == nil && sizeBytes == 0 {
			rawData = []byte{}
		} else {
			return Object{}, fault.New(fault.InternalFailure, "stored kv object has no usable data")
		}
	}
	data := make([]byte, len(rawData))
	copy(data, rawData)
	if int64(len(data)) != sizeBytes {
		return Object{}, fault.New(fault.InternalFailure, fmt.Sprintf("stored kv object size_bytes %d disagrees with data length %d", sizeBytes, len(data)))
	}
	if KeyFor(data) != parsedKey {
		return Object{}, fault.New(fault.InternalFailure, "stored kv object bytes failed SHA-256 integrity check")
	}
	createdBy, ok := row.Values["created_by"].(string)
	if !ok || createdBy == "" {
		return Object{}, fault.New(fault.InternalFailure, "stored kv object has no usable created_by")
	}
	createdAt, ok := row.Values["created_at"].(time.Time)
	if !ok || createdAt.IsZero() {
		return Object{}, fault.New(fault.InternalFailure, "stored kv object has no usable created_at")
	}
	return Object{
		Metadata: Metadata{
			Key:         parsedKey,
			Scope:       storedScope,
			ContentType: contentType,
			SizeBytes:   sizeBytes,
			CreatedBy:   id.PrincipalID(createdBy),
			CreatedAt:   createdAt,
		},
		Data: data,
	}, nil
}

func rowInt64(value any) (int64, error) {
	switch typed := value.(type) {
	case int64:
		if typed < 0 {
			return 0, fault.New(fault.InternalFailure, "stored kv size_bytes is negative")
		}
		return typed, nil
	case int:
		if typed < 0 {
			return 0, fault.New(fault.InternalFailure, "stored kv size_bytes is negative")
		}
		return int64(typed), nil
	case uint64:
		return int64(typed), nil
	default:
		return 0, fault.New(fault.InternalFailure, "stored kv object has no usable size_bytes")
	}
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	class, ok := fault.ClassOf(err)
	return ok && class == fault.NotFound
}

func isVersionConflict(err error) bool {
	if err == nil {
		return false
	}
	class, ok := fault.ClassOf(err)
	return ok && class == fault.VersionConflict
}

// errRaceResolved is an internal sentinel unused externally; kept for clarity.
var _ = errors.Is
