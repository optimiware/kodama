package repositorytest

import (
	"context"
	"slices"
	"sync"
	"testing"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/eventstest"
	"github.com/optimiware/kodama/internal/memdb"
	"github.com/optimiware/kodama/internal/repository"
)

// MemEnv is the repository persistence in memory: one memdb kernel shared by
// the repository store and the event bus.
type MemEnv struct {
	DB     *memdb.Store
	Events *eventstest.Mem
	Mem    *MemStore

	mu    sync.Mutex
	fault error
}

var _ Env = (*MemEnv)(nil)

type memFaultAppender struct{ env *MemEnv }

func (f memFaultAppender) Append(tx memdb.Tx, env eventsv1.Envelope) (eventstest.AppendResult, error) {
	res, err := f.env.Events.Append(tx, env)
	if err != nil {
		return res, err
	}
	f.env.mu.Lock()
	defer f.env.mu.Unlock()
	return res, f.env.fault
}

// NewMemEnv wires the memory repository store over a fresh kernel.
func NewMemEnv(t testing.TB, opts ...memdb.Option) *MemEnv {
	t.Helper()
	e := &MemEnv{DB: memdb.New(opts...)}
	var err error
	if e.Events, err = eventstest.NewMem(e.DB, eventstest.DefaultMemOptions()); err != nil {
		t.Fatal(err)
	}
	if e.Mem, err = NewMemStore(e.DB, memFaultAppender{env: e}, MemOptions{}); err != nil {
		t.Fatal(err)
	}
	return e
}

// OpenMem is the OpenEnv of the memory adapter.
func OpenMem(t testing.TB) Env { return NewMemEnv(t) }

// Store implements Env.
func (e *MemEnv) Store() repository.Store { return e.Mem }

// EventTypes implements Env.
func (e *MemEnv) EventTypes(t testing.TB, scope domain.Scope, id repository.ID) []string {
	t.Helper()
	var out []string
	err := e.DB.View(context.Background(), scope.TenantID, func(tx memdb.Tx) error {
		obs, err := e.Events.ReadObservations(tx, events.Scope{TenantID: scope.TenantID, ProjectID: scope.ProjectID}, 0, 1000)
		if err != nil {
			return err
		}
		for _, o := range obs {
			if o.Envelope.Resource.ID == string(id) {
				out = append(out, o.Envelope.Type)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

// AuditActions implements Env.
func (e *MemEnv) AuditActions(t testing.TB, scope domain.Scope, id repository.ID) []string {
	t.Helper()
	out, err := e.Mem.AuditActions(context.Background(), scope.TenantID, id)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// FailEvents implements Env.
func (e *MemEnv) FailEvents(err error) {
	e.mu.Lock()
	e.fault = err
	e.mu.Unlock()
}
