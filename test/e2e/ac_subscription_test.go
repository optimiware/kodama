//go:build e2e

package e2e

// The live subscription proofs. They subscribe to the installation's real
// broker through the served event.subscribe grant and the documented live-log
// subject, then prove that a newly committed event and newly produced log
// chunks arrive while the subscription is open. A real broker connection is
// required: an unconfigured Q01_NATS_URL is a named skip outside a required
// run and a named failure inside one, never a fake broker.
//
// Scope is proved twice: the served event.subscribe operation refuses a foreign
// project, and a foreign-scope broker subject receives nothing while the
// in-scope subject receives the same published event or log chunk.
//
// Revocation/expiry closure is not exercised by a local installation whose
// loopback broker has no per-credential authorization; that subclaim is not
// credited here.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/optimiware/kodama/api"

	"github.com/optimiware/kodama/test/e2e/internal/harness"
	"github.com/optimiware/kodama/test/e2e/internal/smoke"
)

// liveBrokerInputs returns the extra inputs a live subscription probe needs
// beyond the common installation. A tls:// broker (or an explicitly configured
// CA) also needs the broker CA bundle: the stage broker only issues a
// short-lived scoped user JWT after the client presents its current Kodama
// credential as the NATS auth token, and the probe mints that credential itself
// (see liveSubscriberToken). Q01_NATS_TOKEN_FILE stays an optional override for
// an installation that supplies its own store-backed service credential. A
// plain local broker keeps its unchanged, anonymous connection.
func liveBrokerInputs(config harness.Config) []harness.Input {
	inputs := []harness.Input{harness.InputNATSURL}
	if config.NATSCAFile != "" || strings.HasPrefix(config.NATSURL, "tls://") {
		inputs = append(inputs, harness.InputNATSCAFile)
	}
	return inputs
}

// requireLiveBrokerInputs gates a live subscription probe before it makes any
// public API call, keeping the named skip/fail contract when broker inputs are
// absent.
func requireLiveBrokerInputs(t *testing.T, instance *live) {
	t.Helper()
	if missing := instance.config.Missing(liveBrokerInputs(instance.config)...); !missing.Empty() {
		// A required run must not go green with every live subscription probe
		// skipped: the broker inputs are installation inputs like any other.
		if instance.config.RequireInstallation() || harness.RequireStage() {
			t.Fatalf("required installation input(s) are missing: %s", missing)
		}
		t.Skipf("unconfigured installation input(s): %s", missing)
	}
}

// connectLiveBroker opens one connection to the installation broker with the
// given credential, or with the configured token file when Q01_NATS_TOKEN_FILE
// is explicitly set or fallbackToken is empty. It never reconnects: a probe
// must observe the installation's live connection, not a client retry schedule.
//
// A tls:// broker with the per-credential authorization callout only accepts a
// store-backed credential (service or execution): a human OIDC token has no
// store-backed expiry and is refused by the served surface by design
// (AC07). Callers therefore pass the same run-unique service credential
// they use for the served subscription grant.
func connectLiveBroker(t *testing.T, instance *live, fallbackToken harness.Secret) *natsgo.Conn {
	t.Helper()
	requireLiveBrokerInputs(t, instance)
	options := []natsgo.Option{
		natsgo.Name("q01-" + instance.runID),
		natsgo.Timeout(15 * time.Second),
		natsgo.NoReconnect(),
		// Foreign-scope subscription checks trigger an asynchronous broker
		// permissions violation under auth_callout; suppress the default stderr
		// logger while keeping connection-level errors observable.
		natsgo.ErrorHandler(func(*natsgo.Conn, *natsgo.Subscription, error) {}),
	}
	tlsConfig, err := instance.config.LiveBrokerTLS()
	if err != nil {
		t.Fatalf("the live subscription cannot prepare the broker TLS trust: %v", err)
	}
	if tlsConfig != nil {
		token := fallbackToken
		if !token.IsSet() {
			configured, err := instance.config.LiveBrokerToken()
			if err != nil {
				t.Fatalf("the live subscription cannot read the broker token file: %v", err)
			}
			token = configured
		}
		options = append(options, natsgo.Secure(tlsConfig), natsgo.Token(token.Reveal()))
	}
	connection, err := natsgo.Connect(instance.config.NATSURL, options...)
	if err != nil {
		t.Fatalf("%s is unreachable: the live subscription needs the installation broker: %v", harness.InputNATSURL, err)
	}
	t.Cleanup(func() { connection.Close() })
	return connection
}

// liveScope returns the installation's tenant/project scope tokens. The
// descriptor names both, so a probe never guesses them from a server response.
func liveScope(t *testing.T, instance *live) (tenant, project string) {
	t.Helper()
	tenant = instance.config.Installation.TenantID
	project = instance.config.ProjectID
	if tenant == "" || project == "" {
		t.Fatalf("the installation descriptor must name tenant_id and project_id for a scoped subscription")
	}
	return tenant, project
}

// liveSubscriberToken creates one run-unique engineer service and returns its
// freshly issued credential. A human OIDC token deliberately has no store-backed
// credential expiry, so the served event.subscribe setup refuses it as a
// documented composition boundary (AC07) and the broker callout refuses it
// for the same reason; a service principal is the supported subscriber on this
// installation. The same credential the probe presents to the broker is the one
// it uses for the served grant, so connection and grant share one scope. The
// token is never rendered.
func liveSubscriberToken(ctx context.Context, t *testing.T, instance *live, label string) harness.Secret {
	t.Helper()
	admin := instance.smokeToken(t, harness.PersonaAdmin)
	serviceID := "q01-sub-" + label + "-" + instance.runID
	createdResult := instance.smokeCall(ctx, t, admin, api.MethodPost, "service.create", api.ServiceCreateRequest{
		ProjectID: instance.projectID,
		ServiceID: serviceID,
		Roles:     []string{"engineer"},
	}, instance.key("ac14-service-"+label), nil)
	created := decodeResult[api.ServiceCreateResult](t, createdResult)
	issuedResult := instance.smokeCall(ctx, t, admin, api.MethodPost, "service.credential.issue",
		api.ServiceCredentialIssueRequest{ServiceID: serviceID, TTLSeconds: 300},
		instance.key("ac14-credential-"+label), &created.Version)
	issued := decodeResult[api.ServiceCredentialIssueResult](t, issuedResult)
	if issued.Token == nil || *issued.Token == "" {
		t.Fatalf("service credential issue returned no token for the live subscriber")
	}
	secret := harness.Secret(*issued.Token)
	instance.writer.Secrets = append(instance.writer.Secrets, secret)
	t.Cleanup(func() {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelCleanup()
		version := issued.Version
		_, _ = instance.invoke(cleanupCtx, admin, api.MethodPost, "service.disable",
			api.ServiceDisableRequest{ServiceID: serviceID}, instance.key("ac14-service-disable-"+label), &version)
	})
	return secret
}

// liveEventSubject is the scope-qualified committed-event subject.
func liveEventSubject(tenant, project string) string {
	return fmt.Sprintf("kodama.v1.%s.%s.events", tenant, project)
}

// liveLogSubject is the scope-qualified live-log subject for one
// execution.
func liveLogSubject(tenant, project, executionID string) string {
	return fmt.Sprintf("kodama.v1.%s.%s.logs.%s", tenant, project, executionID)
}

// liveLogChunk is the broker payload shape. It carries the execution identity
// that the public retained chunk omits, so a probe can refuse a chunk from
// another execution.
type liveLogChunk struct {
	ExecutionID string `json:"execution_id"`
	ActionID    string `json:"action_id"`
	Stream      string `json:"stream"`
	Offset      uint64 `json:"offset"`
	Sequence    uint64 `json:"sequence"`
	Data        []byte `json:"data"`
}

// waitForLiveEvent reads the in-scope event channel until the committed event
// identity arrives or the bound expires. Other committed events on the same
// scope are tolerated and never mistaken for the target.
func waitForLiveEvent(ctx context.Context, t *testing.T, subject string, events <-chan []byte, eventID string, bound time.Duration) api.Event {
	t.Helper()
	deadline := time.Now().Add(bound)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("no live event %s arrived on %s within %s", eventID, subject, bound)
		}
		timer := time.NewTimer(remaining)
		select {
		case payload := <-events:
			timer.Stop()
			var envelope api.Event
			if err := json.Unmarshal(payload, &envelope); err != nil {
				t.Fatalf("live event payload on %s is not a public envelope: %v", subject, err)
			}
			if err := envelope.Validate(); err != nil {
				t.Fatalf("live event payload on %s is invalid: %v", subject, err)
			}
			if envelope.ID == eventID {
				return envelope
			}
		case <-timer.C:
			t.Fatalf("no live event %s arrived on %s within %s", eventID, subject, bound)
		case <-ctx.Done():
			timer.Stop()
			t.Fatalf("live event subscription ended before %s: %v", eventID, ctx.Err())
		}
	}
}

// TestE2E_AC14LiveEventDelivery proves event.subscribe returns one scope-named
// subject, that a newly committed event reaches an open broker subscription,
// and that the same publish never reaches a foreign-scope subject.
func TestE2E_AC14LiveEventDelivery(t *testing.T) {
	e2eParallel(t)
	instance := requireACInstallation(t)
	requireLiveBrokerInputs(t, instance)
	started := time.Now().UTC()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	engineer := instance.smokeToken(t, harness.PersonaEngineer)
	subscriber := liveSubscriberToken(ctx, t, instance, "event")
	connection := connectLiveBroker(t, instance, subscriber)
	tenant, project := liveScope(t, instance)

	// The served setup names exactly this project's subject and a live expiry.
	result := instance.smokeCall(ctx, t, subscriber, api.MethodPost, "event.subscribe",
		api.EventSubscribeRequest{ProjectID: instance.projectID}, "", nil)
	grant := decodeResult[api.EventSubscribeResult](t, result)
	if err := grant.Validate(); err != nil {
		t.Fatalf("event.subscribe grant is invalid: %v", err)
	}
	wantSubject := liveEventSubject(tenant, project)
	if len(grant.Subjects) != 1 || grant.Subjects[0] != wantSubject {
		t.Fatalf("event.subscribe subjects = %v; want exactly [%s]", grant.Subjects, wantSubject)
	}
	if !grant.ExpiresAt.After(started) {
		t.Fatalf("event.subscribe expiry = %s; want a live grant after %s", grant.ExpiresAt, started)
	}

	// The served surface refuses a foreign project; the grant can never widen.
	_, foreignErr := instance.invoke(ctx, subscriber, api.MethodPost, "event.subscribe",
		api.EventSubscribeRequest{ProjectID: project + "-foreign"}, "", nil)
	requirePublicRefusal(t, foreignErr, api.CodeForbidden, api.CodeNotFound)

	// Subscribe to the granted subject and a foreign subject before publishing.
	// The subject carries every committed event of the project, so the buffer
	// leaves room for the lifecycle events of concurrent scenarios.
	events := make(chan []byte, 4096)
	subscription, err := connection.Subscribe(grant.Subjects[0], func(message *natsgo.Msg) {
		select {
		case events <- append([]byte(nil), message.Data...):
		default:
		}
	})
	if err != nil {
		t.Fatalf("subscribe to the granted event subject: %v", err)
	}
	t.Cleanup(func() { _ = subscription.Unsubscribe() })
	foreignSubject := liveEventSubject(tenant, project+"-foreign")
	foreign, err := connection.SubscribeSync(foreignSubject)
	if err != nil {
		t.Fatalf("subscribe to the foreign event subject: %v", err)
	}
	t.Cleanup(func() { _ = foreign.Unsubscribe() })
	if err := connection.Flush(); err != nil {
		t.Fatalf("flush the live event subscriptions: %v", err)
	}
	connected := time.Now().UTC()

	// Publish one new event after the subscriptions are open.
	subjectID := "ac14-live-" + instance.runID
	published := instance.smokeCall(ctx, t, engineer, api.MethodPost, "event.publish", api.EventPublishRequest{
		ProjectID: instance.projectID,
		Type:      "observation.custom",
		SubjectID: subjectID,
		Data:      []byte(`{"ac14":"live-delivery"}`),
	}, instance.key("ac14-event"), nil)
	committed := decodeResult[api.EventPublishResult](t, published)
	if err := committed.Validate(); err != nil {
		t.Fatalf("event.publish result is invalid: %v", err)
	}

	envelope := waitForLiveEvent(ctx, t, subscription.Subject, events, committed.EventID, 30*time.Second)
	if envelope.Type != "observation.custom" || envelope.SubjectID != subjectID {
		t.Fatalf("live event = type %q subject %q; want observation.custom/%s", envelope.Type, envelope.SubjectID, subjectID)
	}
	if envelope.Scope.TenantID != tenant || envelope.Scope.ProjectID != project {
		t.Fatalf("live event scope = %+v; want tenant %q project %q", envelope.Scope, tenant, project)
	}
	if envelope.Source.Kind != "shell" && envelope.Source.Kind != "execution" {
		t.Fatalf("live event source kind = %q; want an authenticated source", envelope.Source.Kind)
	}
	if envelope.At.Before(connected.Add(-time.Minute)) {
		t.Fatalf("live event occurrence %s predates the open subscription", envelope.At)
	}

	// The foreign subject observed nothing while the in-scope one delivered.
	if extra, err := foreign.NextMsg(500 * time.Millisecond); err == nil {
		t.Fatalf("foreign-scope subject %s received %d bytes from another scope", foreignSubject, len(extra.Data))
	} else if err != natsgo.ErrTimeout && !errors.Is(err, natsgo.ErrBadSubscription) {
		t.Fatalf("foreign-scope subscription failed: %v", err)
	}

	finished := time.Now().UTC()
	instance.record(t, "AC14", "14.1", "TestE2E_AC14LiveEventDelivery", nil, started, finished, map[string]any{
		"event_id": committed.EventID,
	})
	t.Logf("live event %s was delivered on %s while connected (scope tenant=%s project=%s)",
		committed.EventID, subscription.Subject, tenant, project)
}

// TestE2E_AC14LiveLogDelivery proves the live log subject delivers real chunks
// from a running action to an open subscription in the execution's scope, and
// that a foreign-scope log subject receives nothing.
func TestE2E_AC14LiveLogDelivery(t *testing.T) {
	e2eParallel(t)
	instance := requireACInstallation(t)
	requireLiveBrokerInputs(t, instance)
	started := time.Now().UTC()
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()

	engineer := instance.smokeToken(t, harness.PersonaEngineer)
	tenant, project := liveScope(t, instance)

	// The workload waits before its marker so the subscription is open before
	// the first live chunk is produced.
	document, err := (harness.Document{Actions: []harness.Action{{
		Name:     "ac14-live-log",
		Image:    instance.image,
		Timeout:  "2m",
		Commands: []string{"sleep 12", "printf 'ac14-live-log-ok\\n'", "sleep 4"},
	}}}).Build()
	if err != nil {
		t.Fatalf("build the AC14 live-log pipeline: %v", err)
	}
	admitted := instance.smokeStartInline(ctx, t, engineer, document, instance.key("ac14-log"))
	t.Cleanup(func() {
		instance.smokeCleanupExecution(t, engineer, admitted.ExecutionID)
	})

	// Wait until the action is actually running before minting the subscriber
	// grant and connecting: auth_callout caps each user JWT to 60s and
	// connectLiveBroker disables reconnect, so dialing after pod startup keeps
	// the full grant window for the live stream.
	running := instance.smokeWaitActionRunning(ctx, t, engineer, admitted.ExecutionID, "ac14-live-log", 2*time.Minute)
	action := actionByName(t, running, "ac14-live-log")
	subscriber := liveSubscriberToken(ctx, t, instance, "log")
	connection := connectLiveBroker(t, instance, subscriber)

	logSubject := liveLogSubject(tenant, project, admitted.ExecutionID)
	chunks := make(chan liveLogChunk, 64)
	subscription, err := connection.Subscribe(logSubject, func(message *natsgo.Msg) {
		var chunk liveLogChunk
		if json.Unmarshal(message.Data, &chunk) != nil {
			return
		}
		select {
		case chunks <- chunk:
		default:
		}
	})
	if err != nil {
		t.Fatalf("subscribe to the live log subject: %v", err)
	}
	t.Cleanup(func() { _ = subscription.Unsubscribe() })
	foreignSubject := liveLogSubject(tenant, project+"-foreign", admitted.ExecutionID)
	foreign, err := connection.SubscribeSync(foreignSubject)
	if err != nil {
		t.Fatalf("subscribe to the foreign live log subject: %v", err)
	}
	t.Cleanup(func() { _ = foreign.Unsubscribe() })
	if err := connection.Flush(); err != nil {
		t.Fatalf("flush the live log subscriptions: %v", err)
	}
	connected := time.Now().UTC()

	// Read chunks until the exact marker is observed. Offset continuity is
	// checked over the observed run; a late subscription may start above zero.
	var assembled []byte
	var nextOffset *uint64
	observedChunks := 0
	deadline := time.Now().Add(45 * time.Second)
	for !strings.Contains(string(assembled), "ac14-live-log-ok") {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("no live log chunk carrying ac14-live-log-ok arrived on %s within 45s; observed %d bytes from %d chunk(s) since %s",
				logSubject, len(assembled), observedChunks, connected)
		}
		timer := time.NewTimer(remaining)
		select {
		case chunk := <-chunks:
			timer.Stop()
			observedChunks++
			if chunk.ExecutionID != admitted.ExecutionID {
				t.Fatalf("live log chunk names execution %q; want %q", chunk.ExecutionID, admitted.ExecutionID)
			}
			if chunk.ActionID != action.ActionID {
				t.Fatalf("live log chunk names action %q; want %q", chunk.ActionID, action.ActionID)
			}
			chunkView := api.LogChunk{
				ActionID: chunk.ActionID,
				Stream:   chunk.Stream,
				Offset:   chunk.Offset,
				Sequence: chunk.Sequence,
				Data:     chunk.Data,
			}
			if err := chunkView.Validate(); err != nil {
				t.Fatalf("live log chunk is invalid: %v", err)
			}
			if chunk.Stream != "stdout" {
				continue
			}
			if nextOffset != nil && chunk.Offset != *nextOffset {
				t.Fatalf("live log chunk offset = %d; want the next byte after %d", chunk.Offset, *nextOffset)
			}
			offset := chunk.Offset + uint64(len(chunk.Data))
			nextOffset = &offset
			assembled = append(assembled, chunk.Data...)
		case <-timer.C:
			t.Fatalf("no live log chunk carrying ac14-live-log-ok arrived on %s within 45s; observed %d bytes from %d chunk(s) since %s",
				logSubject, len(assembled), observedChunks, connected)
		case <-ctx.Done():
			timer.Stop()
			t.Fatalf("live log subscription ended before the marker: %v", ctx.Err())
		}
	}

	// The foreign subject observed nothing while the in-scope one delivered.
	if extra, err := foreign.NextMsg(500 * time.Millisecond); err == nil {
		t.Fatalf("foreign-scope log subject %s received %d bytes from another scope", foreignSubject, len(extra.Data))
	} else if err != natsgo.ErrTimeout && !errors.Is(err, natsgo.ErrBadSubscription) {
		t.Fatalf("foreign-scope log subscription failed: %v", err)
	}

	final := instance.smokeWaitComplete(ctx, t, engineer, admitted.ExecutionID, 2*time.Minute)
	requireSucceeded(t, final)
	if !strings.Contains(string(assembled), "ac14-live-log-ok") {
		t.Fatalf("live log reassembly = %q; want the exact marker", assembled)
	}
	if err := smoke.CheckMarker(assembled, "ac14-live-log-ok", 1); err != nil {
		t.Fatalf("live log marker: %v", err)
	}

	finished := time.Now().UTC()
	instance.record(t, "AC14", "14.1", "TestE2E_AC14LiveLogDelivery", &final, started, finished, map[string]any{
		"action_id": action.ActionID,
	})
	t.Logf("live log marker was delivered on %s while connected (execution=%s action=%s bytes=%d)",
		subscription.Subject, admitted.ExecutionID, action.ActionID, len(assembled))
}
