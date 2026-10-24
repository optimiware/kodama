package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/postgres"
)

// Test_poison_scope_and_text: deliveries that can never be processed because
// of what they claim (a project the trusted tenant does not own, in either of
// its two forms) or because of what they contain (an escaped NUL that
// PostgreSQL text cannot hold, in display metadata or a handler's error text)
// are quarantined durably, before acknowledgment, without running a handler
// and without being redelivered forever. Real PostgreSQL and NATS. PX-05.
func Test_poison_scope_and_text(t *testing.T) {
	var handled atomic.Int64
	handler := func(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) error {
		handled.Add(1)
		if env.ID == "evt_nulerr" {
			return events.Permanent(errors.New("downstream said: bad\x00text \xff"))
		}
		_, err := tx.Exec(ctx, `INSERT INTO t005_effects (tenant_id, consumer, event_id) VALUES ($1, 'ledger', $2)`, string(env.TenantID), env.ID)
		return err
	}
	ledgerBinding := postgres.Binding{Spec: events.ConsumerSpec{Name: "ledger", Mode: events.ModeHistorical, Effect: events.EffectLocal}, Handle: handler}
	b := newBus(t, busOptions{bindings: []postgres.Binding{ledgerBinding}})
	b.env.SeedTenant(t, "t_globex", "p_billing")
	ctx := context.Background()
	_ = b.jsPublisher(b.broker.Addr()) // provisions the stream
	token, err := b.store.SubjectToken(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	publish := func(env eventsv1.Envelope) {
		t.Helper()
		raw, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		rawPublish(t, b, token, env.Type, raw)
	}

	missing := workItemEvent("evt_noproject", "wi_1", 1)
	missing.ProjectID = "p_nonexistent"
	foreign := workItemEvent("evt_foreign", "wi_2", 1)
	foreign.ProjectID = "p_billing"                             // owned by another tenant
	nulErr := b.append2(workItemEvent("evt_nulerr", "wi_3", 1)) // a real event: it has an outbox row
	publish(missing)
	publish(foreign)
	publish(nulErr)
	// A future-version message whose type contains an escaped NUL.
	future := workItemEvent("evt_nultype", "wi_4", 1)
	futureJSON, _ := json.Marshal(future)
	nulType := strings.Replace(strings.Replace(string(futureJSON), `"schemaVersion":1`, `"schemaVersion":2`, 1),
		`"type":"`+future.Type+`"`, `"type":"bad\u0000type"`, 1)
	if !strings.Contains(nulType, `\u0000`) {
		t.Fatalf("test payload lacks the escaped NUL: %s", nulType)
	}
	rawPublish(t, b, token, future.Type, []byte(nulType))
	// The same, current version: an unsupported type with a NUL.
	current := strings.Replace(strings.Replace(string(futureJSON), `"type":"`+future.Type+`"`, `"type":"bad\u0000type"`, 1), "evt_nultype", "evt_nultype2", 1)
	rawPublish(t, b, token, future.Type, []byte(current))

	verify := &verifyingProcessor{t: t, b: b, inner: b.store}
	consumer := b.consumer(b.broker.Addr(), verify, "ledger")
	const total = 5
	pump(t, consumer, 20*time.Second, "all deliveries to be settled", func() bool { return consumer.Counters().Acked >= total })

	if got := verify.seen.Load(); got != total {
		t.Fatalf("quarantine was durable before acknowledgment for %d of %d deliveries", got, total)
	}
	if c := consumer.Counters(); c.Retried != 0 || c.Errors != 0 || c.Acked != total {
		t.Fatalf("poison must be acknowledged once, not retried: %+v", c)
	}
	entries, err := b.store.ListQuarantine(ctx, tenantA, "ledger", 100)
	if err != nil || len(entries) != total {
		t.Fatalf("quarantine entries = %d, %v", len(entries), err)
	}
	reasons := map[string]int{}
	for _, e := range entries {
		reasons[e.Reason]++
		if strings.ContainsRune(e.Diagnostic, 0) || strings.ContainsRune(e.EventType, 0) {
			t.Errorf("unrepresentable text stored: %+v", e)
		}
	}
	if reasons[postgres.ReasonScopeMismatch] != 2 || reasons[postgres.ReasonUnsupportedVersion] != 1 || reasons[postgres.ReasonUnsupportedType] != 1 || reasons[postgres.ReasonPermanentFailure] != 1 {
		t.Fatalf("reasons: %v", reasons)
	}
	// Only the event whose handler ran far enough to fail was handled; the
	// scope violations and unsupported messages never reached a handler.
	if got := handled.Load(); got != 1 {
		t.Fatalf("handler invocations = %d, want 1 (evt_nulerr)", got)
	}
	for _, id := range []string{"evt_noproject", "evt_foreign", "evt_nulerr", "evt_nultype"} {
		if b.effects("ledger", id) != 0 {
			t.Fatalf("%s left an effect", id)
		}
	}
	// No inbox entry refers to a project the tenant does not own.
	if got := b.count(`SELECT count(*) FROM event_inbox WHERE event_id IN ('evt_noproject', 'evt_foreign')`); got != 0 {
		t.Fatalf("scope violations left %d inbox entries", got)
	}
	// The original bytes are retained for operators even though the display
	// metadata could not carry them.
	if got := b.count(`SELECT count(*) FROM event_quarantine WHERE strpos(convert_from(payload, 'UTF8'), $1) > 0 AND reason_code = 'unsupported_version' AND event_type IS NULL`, `\u0000`); got != 1 {
		t.Fatalf("raw NUL-type entry: %d", got)
	}
	// A duplicate redelivery of any of them is absorbed without a second entry.
	publish(missing)
	pump(t, consumer, 20*time.Second, "the duplicate", func() bool { return consumer.Counters().Acked >= total+1 })
	if got := b.count(`SELECT count(*) FROM event_quarantine WHERE consumer = 'ledger'`); got != total {
		t.Fatalf("quarantine rows after a duplicate = %d, want %d", got, total)
	}
}
