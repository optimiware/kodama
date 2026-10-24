package events_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/events/postgres"
)

// Test_quarantine_database_outage: a poison delivery whose quarantine cannot be
// persisted (the database refuses the write) is never settled and never
// dropped by the broker, however many times it is redelivered. When the
// database recovers, the same delivery is quarantined durably and only then
// acknowledged. The adapter's consumer has no finite broker delivery limit
// that could strand the message before a durable outcome exists. Real
// PostgreSQL and NATS. PX-05.
func Test_quarantine_database_outage(t *testing.T) {
	b := newBus(t, busOptions{bindings: []postgres.Binding{ledger("ledger")}})
	ctx := context.Background()
	_ = b.jsPublisher(b.broker.Addr())
	token, err := b.store.SubjectToken(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	consumer := b.consumer(b.broker.Addr(), nil, "ledger")

	runtime := pgxIdent(b.env.DB.RuntimeRole)
	// The database outage: the runtime role cannot persist a quarantine.
	b.ownerExec(fmt.Sprintf(`REVOKE INSERT ON event_quarantine FROM %s`, runtime))
	restored := false
	restore := func() {
		if !restored {
			restored = true
			b.ownerExec(fmt.Sprintf(`GRANT INSERT ON event_quarantine TO %s`, runtime))
		}
	}
	defer restore()

	rawPublish(t, b, token, "work_item.transitioned", []byte(`{not json`))

	// Far more redeliveries than any plausible broker delivery limit.
	const redeliveries = 6
	pump(t, consumer, 30*time.Second, "repeated redelivery during the outage", func() bool {
		return consumer.Counters().Errors >= redeliveries
	})
	if c := consumer.Counters(); c.Acked != 0 || c.Terminated != 0 {
		t.Fatalf("the delivery was settled without a durable outcome: %+v", c)
	}
	if got := b.count(`SELECT count(*) FROM event_quarantine`); got != 0 {
		t.Fatalf("quarantine rows during the outage = %d", got)
	}

	restore()
	pump(t, consumer, 30*time.Second, "the recovered quarantine to be acknowledged", func() bool {
		return consumer.Counters().Acked >= 1
	})
	entries, err := b.store.ListQuarantine(ctx, tenantA, "ledger", 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("quarantine entries = %d, %v", len(entries), err)
	}
	e := entries[0]
	if e.Reason != postgres.ReasonMalformed || e.State != "quarantined" {
		t.Fatalf("entry: %+v", e)
	}
	if e.DeliveryCount < redeliveries {
		t.Errorf("entry recorded delivery %d, want at least %d (the outage redeliveries)", e.DeliveryCount, redeliveries)
	}
	// Settled: further pumping finds nothing to redeliver.
	before := consumer.Counters()
	for i := 0; i < 3; i++ {
		rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, _ = consumer.RunOnce(rctx)
		cancel()
	}
	if after := consumer.Counters(); after.Acked != before.Acked || after.Errors != before.Errors {
		t.Fatalf("the settled delivery came back: %+v -> %+v", before, after)
	}
}
