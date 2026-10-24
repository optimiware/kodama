package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	"github.com/optimiware/kodama/internal/events"
	"github.com/optimiware/kodama/internal/events/eventstest"
	"github.com/optimiware/kodama/internal/events/postgres"
)

// instant reads one timestamp from the tenant's rows.
func (f *fixture) instant(sql string, args ...any) time.Time {
	f.t.Helper()
	var at time.Time
	if err := f.tx(tenantA, func(ctx context.Context, tx pgx.Tx) error { return tx.QueryRow(ctx, sql, args...).Scan(&at) }); err != nil {
		f.t.Fatalf("read timestamp: %v\n%s", err, sql)
	}
	return at
}

// TestCompletionNeverPrecedesRetainedOrigin: the timestamps that bound an
// event's lifetime come from different clocks (outbox.created_at from the
// appending process, observation.recorded_at from the database, completion
// from the consumer). A completion earlier than a retained origin would let
// pruning forget the inbox entry while that origin still looks "after the
// boundary", so a late delivery, repair or replay would run the effect again.
// Each case skews two clocks against each other, prunes between the completion
// the consumer's own clock gave and the origin, and requires one effect on
// every path. Real PostgreSQL.
func TestCompletionNeverPrecedesRetainedOrigin(t *testing.T) {
	const skew = 10 * time.Second
	cases := []struct {
		name string
		// consumerBehind moves the consumer's clock behind the database clock;
		// appenderAhead moves the appending process's clock ahead of the
		// consumer's.
		consumerBehind, appenderAhead bool
		retention                     func(o *postgres.Options)
		// origin is the retained timestamp the prune cutoff is placed before.
		origin string
		// observationsSurvive: the observation outlives the first prune.
		observationsSurvive bool
	}{
		{
			name: "database clock ahead of the consumer, observation retained", consumerBehind: true,
			retention: func(o *postgres.Options) {
				o.Retention.Inbox, o.Retention.Observation = 10*time.Hour, 20*time.Hour
				o.Retention.PublishedOutbox, o.Retention.BrokerRedelivery, o.Retention.ReplayWindow = time.Hour, time.Hour, time.Hour
			},
			origin: `SELECT recorded_at FROM event_observations WHERE event_id = 'oc_evt'`, observationsSurvive: true,
		},
		{
			name: "appender clock ahead of the consumer, outbox row retained", appenderAhead: true,
			retention: func(o *postgres.Options) {
				o.Retention.Inbox, o.Retention.PublishedOutbox = 10*time.Hour, 20*time.Hour
				o.Retention.Observation, o.Retention.BrokerRedelivery, o.Retention.ReplayWindow = 2*time.Hour, time.Hour, time.Hour
			},
			origin: `SELECT created_at FROM outbox WHERE event_id = 'oc_evt'`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newFixture(t, tc.retention, ledgerBinding())
			effectTable(f)
			appender := f.store
			if tc.appenderAhead {
				ac := &eventstest.Clock{}
				ac.Advance(skew)
				opts := eventstestDefaults(f)
				tc.retention(&opts)
				opts.Clock = ac.Now
				appender = f.env.NewStore(t, opts, ledgerBinding())
			}
			if tc.consumerBehind {
				f.clock.Advance(-skew)
			}
			env := f.fresh(eventA("oc_evt", 1))
			if err := f.tx(tenantA, func(ctx context.Context, tx pgx.Tx) error {
				_, err := appender.Append(ctx, tx, env)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			// Delivered at once (a copy that reached the consumer, as with a
			// publish-ack crash), published once every clock has passed the row's
			// due time.
			if out := deliver(t, f, env); out.Kind != events.OutcomeCompleted {
				t.Fatalf("first delivery: %+v", out)
			}
			f.clock.Advance(2 * skew)
			markPublished(t, f)

			// The stored invariant: completion is not before any retained origin.
			if n := f.count(tenantA, `SELECT count(*) FROM event_inbox i
				LEFT JOIN outbox o ON o.tenant_id = i.tenant_id AND o.event_id = i.event_id
				LEFT JOIN event_observations ob ON ob.tenant_id = i.tenant_id AND ob.event_id = i.event_id
				WHERE i.completed_at < o.created_at OR i.completed_at < ob.recorded_at`); n != 0 {
				t.Fatalf("%d completion(s) precede a retained origin", n)
			}

			// Prune with the cutoff 5s after the consumer-clock completion the
			// unclamped rule would have stored, 5s before the retained origin.
			origin := f.instant(tc.origin)
			f.clock.Advance(origin.Add(10*time.Hour - 5*time.Second).Sub(f.clock.Now()))
			pr, err := f.store.Prune(ctx, tenantA, f.clock.Now())
			if err != nil {
				t.Fatal(err)
			}
			if pr.Inbox != 0 {
				t.Fatalf("the deduplication entry was forgotten before its origin expired: %+v", pr)
			}
			if tc.observationsSurvive && pr.Observations != 0 || !tc.observationsSurvive && pr.Observations != 1 {
				t.Fatalf("prune: %+v", pr)
			}
			// Every path stays single-effect while the evidence stands.
			if out := deliver(t, f, env); out.Kind != events.OutcomeDuplicate {
				t.Fatalf("stray delivery with evidence: %+v", out)
			}
			if res, err := f.store.Repair(ctx, tenantA, "ledger", 0, 10); err != nil || res.Backfilled != 0 || res.Retried != 0 {
				t.Fatalf("repair: %+v %v", res, err)
			}

			// Once the entry is forgotten, no retained origin may look unprocessed.
			f.clock.Advance(time.Minute)
			if pr, err := f.store.Prune(ctx, tenantA, f.clock.Now()); err != nil || pr.Inbox != 1 {
				t.Fatalf("final prune: %+v %v", pr, err)
			}
			if out := deliver(t, f, env); out.Kind != events.OutcomeQuarantined || out.Reason != postgres.ReasonBeyondHorizon {
				t.Fatalf("stray delivery after the evidence expired: %+v", out)
			}
			if res, err := f.store.Repair(ctx, tenantA, "ledger", 0, 10); err != nil || res.Backfilled != 0 || res.Refused != 0 {
				t.Fatalf("repair after the evidence expired: %+v %v", res, err)
			}
			if tc.observationsSurvive {
				c, err := f.store.DryRun(ctx, replayReq("ledger", 1, 1))
				if !errors.Is(err, events.ErrReplayBeyondHorizon) || c.Undelivered != 0 || c.BeyondHorizon != 1 {
					t.Fatalf("replay classification: %+v %v", c, err)
				}
			}
			if n := f.effects("ledger", "oc_evt"); n != 1 {
				t.Fatalf("effects = %d, want 1", n)
			}
		})
	}
}

// conflicting returns env with changed content under the same event ID.
func conflicting(env eventsv1.Envelope, change func(*eventsv1.Envelope)) eventsv1.Envelope {
	c := env
	change(&c)
	return c
}

func deliverRaw(t *testing.T, f *fixture, env eventsv1.Envelope) (events.Outcome, error) {
	t.Helper()
	payload, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return f.store.Process(context.Background(), events.Delivery{Tenant: tenantA, Consumer: "ledger", Payload: payload})
}

// TestConflictingRedeliveryKeepsOriginalContent: the inbox entry identifies the
// content it was created for. A later delivery of the same event ID with other
// project, resource or payload values is quarantined under its own key; the
// original entry, its schedule and its handler outcome are unchanged, and the
// original event can still be completed or repaired afterwards. Real PostgreSQL.
func TestConflictingRedeliveryKeepsOriginalContent(t *testing.T) {
	ctx := context.Background()
	changes := map[string]func(*eventsv1.Envelope){
		"resource": func(e *eventsv1.Envelope) { e.Resource.Version += 5 },
		"payload": func(e *eventsv1.Envelope) {
			e.Data = json.RawMessage(`{"from":"ready","to":"closed","transitionId":"tr_other"}`)
		},
		"project": func(e *eventsv1.Envelope) { e.ProjectID = "p_other" },
		"actor":   func(e *eventsv1.Envelope) { e.Actor.ID = "someone_else" },
	}

	t.Run("pending entry after a transient failure", func(t *testing.T) {
		for name, change := range changes {
			t.Run(name, func(t *testing.T) {
				fail := true
				var seen []string
				f := newFixture(t, nil, postgres.Binding{
					Spec: events.ConsumerSpec{Name: "ledger", Mode: events.ModeHistorical, Effect: events.EffectLocal},
					Handle: func(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) error {
						seen = append(seen, string(env.Data))
						if fail {
							return events.Transient(errors.New("down"))
						}
						_, err := tx.Exec(ctx, `INSERT INTO t005_eff (tenant_id, consumer, event_id) VALUES ($1, 'ledger', $2)`, string(env.TenantID), env.ID)
						return err
					},
				})
				effectTable(f)
				env := eventA("cf_evt", 1)
				f.append(env)
				if out := deliver(t, f, env); out.Kind != events.OutcomeRetry {
					t.Fatalf("first delivery: %+v", out)
				}
				before := f.count(tenantA, `SELECT attempts FROM event_inbox WHERE event_id = 'cf_evt'`)
				fail = false
				f.clock.Advance(time.Minute) // backoff elapsed: the handler would run
				handled := len(seen)

				out, err := deliverRaw(t, f, conflicting(env, change))
				if err != nil || out.Kind != events.OutcomeQuarantined || out.Reason != postgres.ReasonConflictingContent {
					t.Fatalf("conflicting delivery: %+v %v", out, err)
				}
				if len(seen) != handled || f.effects("ledger", "cf_evt") != 0 {
					t.Fatalf("the conflicting content reached the handler: %v", seen[handled:])
				}
				if got := f.count(tenantA, `SELECT attempts FROM event_inbox WHERE event_id = 'cf_evt' AND state = 'pending'`); got != before {
					t.Fatalf("original entry changed: attempts %d, was %d", got, before)
				}
				if f.count(tenantA, `SELECT count(*) FROM event_quarantine WHERE reason_code = 'conflicting_content' AND delivery_key LIKE 'raw:%'`) != 1 {
					t.Fatal("conflicting delivery was not quarantined under its own key")
				}
				// The original is intact: repair completes it with its own content.
				res, err := f.store.Repair(ctx, tenantA, "ledger", 0, 10)
				if err != nil || res.Retried != 1 || res.Failed != 0 {
					t.Fatalf("repair of the original: %+v %v", res, err)
				}
				if f.effects("ledger", "cf_evt") != 1 || seen[len(seen)-1] != string(env.Data) {
					t.Fatalf("original not completed with its own content: %v", seen)
				}
				if out := deliver(t, f, env); out.Kind != events.OutcomeDuplicate {
					t.Fatalf("redelivery of the original: %+v", out)
				}
			})
		}
	})

	t.Run("completed entry", func(t *testing.T) {
		for name, change := range changes {
			t.Run(name, func(t *testing.T) {
				f := newFixture(t, nil, ledgerBinding())
				effectTable(f)
				env := eventA("cc_evt", 1)
				f.append(env)
				if out := deliver(t, f, env); out.Kind != events.OutcomeCompleted {
					t.Fatalf("first delivery: %+v", out)
				}
				out, err := deliverRaw(t, f, conflicting(env, change))
				if err != nil || out.Kind != events.OutcomeQuarantined || out.Reason != postgres.ReasonConflictingContent {
					t.Fatalf("conflicting delivery: %+v %v", out, err)
				}
				if f.effects("ledger", "cc_evt") != 1 {
					t.Fatal("effect repeated or lost")
				}
				if f.count(tenantA, `SELECT count(*) FROM event_inbox WHERE event_id = 'cc_evt' AND state = 'completed'`) != 1 {
					t.Fatal("completed entry changed")
				}
				if res, err := f.store.Repair(ctx, tenantA, "ledger", 0, 10); err != nil || res.Backfilled != 0 {
					t.Fatalf("repair: %+v %v", res, err)
				}
				if out := deliver(t, f, env); out.Kind != events.OutcomeDuplicate || f.effects("ledger", "cc_evt") != 1 {
					t.Fatalf("original redelivery: %+v", out)
				}
			})
		}
	})

	t.Run("conflicting copy does not overwrite the original's quarantine record", func(t *testing.T) {
		f := newFixture(t, nil, postgres.Binding{
			Spec: events.ConsumerSpec{Name: "ledger", Mode: events.ModeHistorical, Effect: events.EffectLocal},
			Handle: func(ctx context.Context, tx pgx.Tx, env eventsv1.Envelope) error {
				return events.Permanent(errors.New("poison"))
			},
		})
		env := eventA("cq_evt", 1)
		f.append(env)
		if out := deliver(t, f, env); out.Kind != events.OutcomeQuarantined || out.Reason != postgres.ReasonPermanentFailure {
			t.Fatalf("original: %+v", out)
		}
		out, err := deliverRaw(t, f, conflicting(env, changes["project"]))
		if err != nil || out.Reason != postgres.ReasonConflictingContent {
			t.Fatalf("conflicting delivery: %+v %v", out, err)
		}
		if f.count(tenantA, `SELECT count(*) FROM event_quarantine WHERE delivery_key = 'cq_evt' AND reason_code = 'permanent_failure'`) != 1 {
			t.Fatal("the original's quarantine record was altered")
		}
	})
}
