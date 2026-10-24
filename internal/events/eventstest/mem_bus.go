package eventstest

import (
	"context"
	"testing"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/memdb"
)

// memBus adapts Mem to the contract's Bus: Update opens a memdb.Update and the
// Unit's Append joins its Tx.
type memBus struct{ *Mem }

var _ Bus = memBus{}

type memUnit struct {
	m  *Mem
	tx memdb.Tx
}

func (u memUnit) Append(_ context.Context, env eventsv1.Envelope) (AppendResult, error) {
	return u.m.Append(u.tx, env)
}

func (u memUnit) Note(_ context.Context, key string) error { return Note(u.tx, key) }

func (u memUnit) Watermark(_ context.Context, scope events.Scope) (int64, error) {
	return u.m.Watermark(u.tx, scope)
}

func (u memUnit) Observations(_ context.Context, scope events.Scope, after int64, limit int) ([][]byte, error) {
	obs, err := u.m.ReadObservations(u.tx, scope, after, limit)
	out := make([][]byte, len(obs))
	for i, o := range obs {
		out[i] = o.Payload
	}
	return out, err
}

// Update implements Bus.
func (b memBus) Update(ctx context.Context, tenant domain.TenantID, fn func(Unit) error) error {
	return b.Mem.update(ctx, tenant, func(tx memdb.Tx) error { return fn(memUnit{b.Mem, tx}) })
}

// OpenMem is the Open of the memory adapter: a fresh memdb store, TenantA and
// TenantB known, OperatorID authorized, the clock as its only time source.
func OpenMem(t testing.TB, clock *Clock, consumers ...Consumer) Bus {
	t.Helper()
	opts := DefaultMemOptions()
	opts.Clock = clock.Now
	opts.Authorizer = Operators{OperatorID}
	var mem *Mem
	bindings := make([]MemBinding, len(consumers))
	for i, c := range consumers {
		c := c
		bindings[i] = MemBinding{Spec: c.Spec, Handle: func(ctx context.Context, tx memdb.Tx, env eventsv1.Envelope) error {
			return c.Handle(ctx, env, memUnit{mem, tx})
		}}
	}
	mem, err := NewMem(memdb.New(), opts, bindings...)
	if err != nil {
		t.Fatal(err)
	}
	mem.AddTenant(TenantA)
	mem.AddTenant(TenantB)
	return memBus{mem}
}
