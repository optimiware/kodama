package memdb

import (
	"fmt"
	"sort"

	"github.com/optimiware/kodama/internal/domain"
)

type tableID struct {
	name    string
	uniques int
}

// Table is a typed set of rows keyed by a primary key. It holds no data: rows
// live in the Store and are reached only through a Tx, so every call is scoped
// to that Tx's tenant. A Table may be shared by any number of Stores.
type Table[V any] struct {
	id      *tableID
	key     func(V) string
	uniques []func(V) string
	clone   func(V) V
}

// NewTable declares a table. key returns a row's primary key, which must be
// non-empty. Each unique function returns the value that must be unique among
// the tenant's rows; the empty string opts a row out of that constraint.
// name appears in errors only; it need not be unique.
func NewTable[V any](name string, key func(V) string, uniques ...func(V) string) *Table[V] {
	if key == nil {
		panic("memdb: NewTable " + name + ": nil key function")
	}
	for _, u := range uniques {
		if u == nil {
			panic("memdb: NewTable " + name + ": nil unique function")
		}
	}
	return &Table[V]{id: &tableID{name: name, uniques: len(uniques)}, key: key, uniques: uniques}
}

// NewClonedTable isolates mutable fields (JSON documents, slices and pointers)
// at the transaction boundary. clone must return a deep independent value.
func NewClonedTable[V any](name string, clone func(V) V, key func(V) string, uniques ...func(V) string) *Table[V] {
	if clone == nil {
		panic("memdb: NewClonedTable " + name + ": nil clone function")
	}
	t := NewTable(name, key, uniques...)
	t.clone = clone
	return t
}

func (t *Table[V]) copy(v V) V {
	if t.clone != nil {
		return t.clone(v)
	}
	return v
}

// Get returns the tenant's row with the given key, including this Tx's own
// uncommitted writes.
func (t *Table[V]) Get(tx Tx, key string) (V, bool) {
	r := txOf(tx, t.id.name).read(t.id)
	if r != nil {
		if v, ok := r.byKey[key]; ok {
			return t.copy(v.(V)), true
		}
	}
	var zero V
	return zero, false
}

// Put inserts or replaces the row with v's primary key. If another row of the
// tenant holds one of v's unique values it returns an error matching
// ErrConflict and changes nothing. On a View's Tx it returns ErrReadOnly.
func (t *Table[V]) Put(tx Tx, v V) error {
	s := txOf(tx, t.id.name)
	if !s.writable {
		return ErrReadOnly
	}
	key := t.key(v)
	if key == "" {
		return fmt.Errorf("%w: %s: empty primary key", domain.ErrInvalidValue, t.id.name)
	}
	vals := make([]string, len(t.uniques))
	cur := s.read(t.id)
	for i, u := range t.uniques {
		vals[i] = u(v)
		if vals[i] == "" || cur == nil {
			continue
		}
		if holder, taken := cur.unique[i][vals[i]]; taken && holder != key {
			return fmt.Errorf("%w: %s: unique key %d already held by another row", ErrConflict, t.id.name, i)
		}
	}
	w := s.write(t.id)
	t.unindex(w, key)
	for i, val := range vals {
		if val != "" {
			w.unique[i][val] = key
		}
	}
	w.byKey[key] = t.copy(v)
	return nil
}

// Delete removes the row with the given key; a missing key is not an error.
// It panics on a View's Tx, which cannot write.
func (t *Table[V]) Delete(tx Tx, key string) {
	s := txOf(tx, t.id.name)
	if !s.writable {
		panic("memdb: Delete on table " + t.id.name + " in a read-only Tx")
	}
	if r := s.read(t.id); r == nil {
		return
	} else if _, ok := r.byKey[key]; !ok {
		return
	}
	w := s.write(t.id)
	t.unindex(w, key)
	delete(w.byKey, key)
}

// Scan calls yield for each of the tenant's rows in primary-key order until
// yield returns false. It sees the rows as they were when Scan was called, so
// yield may Put or Delete in the same Tx.
func (t *Table[V]) Scan(tx Tx, yield func(V) bool) {
	r := txOf(tx, t.id.name).read(t.id)
	if r == nil {
		return
	}
	keys := make([]string, 0, len(r.byKey))
	for k := range r.byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	vals := make([]V, len(keys))
	for i, k := range keys {
		vals[i] = t.copy(r.byKey[k].(V))
	}
	for _, v := range vals {
		if !yield(v) {
			return
		}
	}
}

// unindex drops the unique entries of the row currently stored under key.
func (t *Table[V]) unindex(w *rows, key string) {
	old, ok := w.byKey[key]
	if !ok {
		return
	}
	for i, u := range t.uniques {
		if val := u(old.(V)); val != "" && w.unique[i][val] == key {
			delete(w.unique[i], val)
		}
	}
}
