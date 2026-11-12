package session_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/userland/session"
)

// fakeStorage is a deterministic in-memory checkpoint backend. It is the only
// durable state: no process, network, database, broker or filesystem is
// involved. Failure, uncertainty, invalid-reference and gating seams let the
// tests separate crash-before-effect from lost-response-after-effect.
type fakeStorage struct {
	mu      sync.Mutex
	records map[session.Ref]session.Record
	heads   map[session.SessionID]uint64
	latest  map[session.SessionID]session.Ref
	seq     int
	casCall int
	lastCAS session.Record
	reads   []session.Ref

	failBefore error
	failAfter  error
	badRef     bool

	enter   chan struct{}
	release chan struct{}
}

func newFakeStorage() *fakeStorage {
	return &fakeStorage{
		records: make(map[session.Ref]session.Record),
		heads:   make(map[session.SessionID]uint64),
		latest:  make(map[session.SessionID]session.Ref),
	}
}

func (storage *fakeStorage) Read(_ context.Context, ref session.Ref) (session.Record, error) {
	storage.mu.Lock()
	defer storage.mu.Unlock()
	storage.reads = append(storage.reads, ref)
	record, ok := storage.records[ref]
	if !ok {
		return session.Record{}, fmt.Errorf("checkpoint %s: %w", ref, session.ErrCheckpointMissing)
	}
	return record, nil
}

func (storage *fakeStorage) CompareAndSwap(_ context.Context, record session.Record, expectedRevision uint64) (session.Ref, error) {
	storage.mu.Lock()
	storage.casCall++
	storage.lastCAS = record
	enter, release := storage.enter, storage.release
	failBefore := storage.failBefore
	storage.mu.Unlock()

	if enter != nil {
		select {
		case enter <- struct{}{}:
		default:
		}
	}
	if release != nil {
		<-release
	}

	storage.mu.Lock()
	defer storage.mu.Unlock()
	if failBefore != nil {
		return "", failBefore
	}
	if head := storage.heads[record.SessionID]; head != expectedRevision {
		return "", fault.New(fault.VersionConflict, "stale session revision")
	}
	storage.seq++
	ref, err := session.ParseRef(fmt.Sprintf("kodama-checkpoint/v1/ctx-%04d", storage.seq))
	if err != nil {
		return "", err
	}
	committed := record
	committed.Revision = expectedRevision + 1
	storage.records[ref] = committed
	storage.heads[record.SessionID] = committed.Revision
	storage.latest[record.SessionID] = ref
	if storage.badRef {
		return session.Ref("not-a-checkpoint"), nil
	}
	if storage.failAfter != nil {
		return "", storage.failAfter
	}
	return ref, nil
}

func (storage *fakeStorage) casCount() int {
	storage.mu.Lock()
	defer storage.mu.Unlock()
	return storage.casCall
}

func (storage *fakeStorage) head(id session.SessionID) uint64 {
	storage.mu.Lock()
	defer storage.mu.Unlock()
	return storage.heads[id]
}

func (storage *fakeStorage) refFor(id session.SessionID) (session.Ref, bool) {
	storage.mu.Lock()
	defer storage.mu.Unlock()
	ref, ok := storage.latest[id]
	return ref, ok
}

// put injects a stored record directly, bypassing the store, to model a
// corrupted or unsupported-version checkpoint on retained storage.
func (storage *fakeStorage) put(ref session.Ref, record session.Record) {
	storage.mu.Lock()
	defer storage.mu.Unlock()
	storage.records[ref] = record
}

func (storage *fakeStorage) lastRecord() session.Record {
	storage.mu.Lock()
	defer storage.mu.Unlock()
	return storage.lastCAS
}

// fakeClock is the injected time boundary.
type fakeClock struct {
	now time.Time
}

func (clock *fakeClock) Now() time.Time { return clock.now }

// newTestStore builds the durable store over the fake boundaries.
func newTestStore(t *testing.T, storage *fakeStorage, clock *fakeClock, secrets ...string) session.Store {
	t.Helper()
	store, err := session.NewStore(session.StoreConfig{Storage: storage, Clock: clock, Secrets: secrets})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

// assertNoFieldMatching refuses any struct field whose lower-case name contains
// one of the fragments. It proves a durable record never carries credentials,
// approvals or process-local state.
func assertNoFieldMatching(t *testing.T, structType reflect.Type, fragments ...string) {
	t.Helper()
	for field := range structType.Fields() {
		name := strings.ToLower(field.Name)
		for _, fragment := range fragments {
			if strings.Contains(name, fragment) {
				t.Errorf("%s carries a %q field", structType, fragment)
			}
		}
	}
}
