//go:build component

package messaging_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/app/drivers/nats"
	"github.com/optimiware/kodama/internal/shell/event"
	broker "github.com/optimiware/kodama/internal/shell/integration/nats"
	"github.com/optimiware/kodama/internal/shell/service"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/token"
	support "github.com/optimiware/kodama/test/support/infrastructure/messaging"
)

func tenantScope() tenancy.Scope {
	return tenancy.Scope{TenantID: id.TenantID("tenant-a")}
}

func projectScope() tenancy.Scope {
	return tenancy.Scope{TenantID: id.TenantID("tenant-a"), ProjectID: id.ProjectID("project-1")}
}

func testNow() time.Time {
	return time.Unix(1_700_000_000, 0).UTC()
}

func eventEnvelope(t *testing.T, scope tenancy.Scope, eventID id.EventID) event.Envelope {
	t.Helper()
	envelope := event.Envelope{
		ID:             eventID,
		Type:           event.TypeTaskCreated,
		Source:         event.Source{Kind: event.SourceShell, PrincipalID: id.PrincipalID("human-1")},
		At:             testNow(),
		Scope:          scope,
		SubjectID:      "task-1",
		SubjectVersion: 1,
		Data:           []byte(`{"task":"task-1"}`),
	}
	if err := envelope.Validate(); err != nil {
		t.Fatalf("test envelope is invalid: %v", err)
	}
	return envelope
}

// newBroker builds the concrete broker adapter over the real NATS driver.
func newBroker(t *testing.T, instance *support.NATS, directory *support.Directory, connectionName string) *broker.Broker {
	t.Helper()
	return newBrokerWithIdleValidation(t, instance, directory, connectionName, time.Hour)
}

func newBrokerWithIdleValidation(t *testing.T, instance *support.NATS, directory *support.Directory, connectionName string, idleValidation time.Duration) *broker.Broker {
	t.Helper()
	return newBrokerWithConnectAndIdle(t, directory, func(ctx context.Context) (broker.Client, error) {
		return natsdriver.Dial(ctx, instance.DriverConfig(connectionName))
	}, idleValidation)
}

func newBrokerWithConnect(t *testing.T, directory *support.Directory, connect broker.Connect) *broker.Broker {
	t.Helper()
	return newBrokerWithConnectAndIdle(t, directory, connect, time.Hour)
}

func newBrokerWithConnectAndIdle(t *testing.T, directory *support.Directory, connect broker.Connect, idleValidation time.Duration) *broker.Broker {
	t.Helper()
	adapter, err := broker.NewBroker(broker.BrokerConfig{
		Connect:             connect,
		Clock:               support.RealClock{},
		Principal:           directory,
		Authorizer:          directory,
		Sink:                support.NoopSink{},
		MaxReconnectBackoff: 40 * time.Millisecond,
		IdleValidation:      idleValidation,
		MaxPublishAttempts:  2,
	})
	if err != nil {
		t.Fatalf("build broker: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close(context.Background()) })
	return adapter
}

func serviceIdentity(principalID id.PrincipalID, scope tenancy.Scope, reasons ...token.InvalidationReason) (*support.Directory, string, token.Invalidation) {
	reason := token.InvalidationRevoked
	if len(reasons) > 0 {
		reason = reasons[0]
	}
	directory := support.NewDirectory()
	credential := "credential-" + string(principalID)
	directory.Add(credential, support.ServicePrincipal(principalID, scope))
	invalidation := support.ServiceInvalidation(scope, principalID, service.ServiceID("service-1"), reason, testNow())
	return directory, credential, invalidation
}

// delivered is one fetched JetStream message.
type delivered struct {
	Subject  string
	Data     []byte
	Sequence uint64
	Stream   string
	ack      func() error
}

// fetch pulls up to batch messages from a fresh consumer configuration.
func fetch(t *testing.T, js jetstream.JetStream, stream string, config jetstream.ConsumerConfig, batch int, wait time.Duration) []delivered {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait+5*time.Second)
	defer cancel()
	consumer, err := js.CreateOrUpdateConsumer(ctx, stream, config)
	if err != nil {
		t.Fatalf("create consumer %q: %v", config.Durable, err)
	}
	messageBatch, err := consumer.Fetch(batch, jetstream.FetchMaxWait(wait))
	if err != nil {
		t.Fatalf("fetch from %q: %v", config.Durable, err)
	}
	out := make([]delivered, 0, batch)
	for message := range messageBatch.Messages() {
		metadata, metadataErr := message.Metadata()
		if metadataErr != nil {
			t.Fatalf("message metadata: %v", metadataErr)
		}
		message := message
		out = append(out, delivered{
			Subject:  message.Subject(),
			Data:     append([]byte(nil), message.Data()...),
			Sequence: metadata.Sequence.Stream,
			Stream:   metadata.Stream,
			ack:      message.Ack,
		})
	}
	if err := messageBatch.Error(); err != nil {
		t.Fatalf("fetch batch: %v", err)
	}
	return out
}

func decodeEvent(t *testing.T, data []byte) event.Envelope {
	t.Helper()
	var envelope event.Envelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	return envelope
}

// requireClass asserts the fault class of a non-nil error.
func requireClass(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil; want fault class %s", want)
	}
	class, ok := fault.ClassOf(err)
	if !ok || class != want {
		t.Fatalf("error class = %v (ok=%v); want %s (err=%v)", class, ok, want, err)
	}
}

// receive waits for one value on ch within timeout.
func receive[T any](t *testing.T, ch <-chan T, timeout time.Duration, what string) T {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case value := <-ch:
		return value
	case <-timer.C:
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

// requireNoReceive proves nothing arrives on ch within window.
func requireNoReceive[T any](t *testing.T, ch <-chan T, window time.Duration, what string) {
	t.Helper()
	timer := time.NewTimer(window)
	defer timer.Stop()
	select {
	case value := <-ch:
		t.Fatalf("unexpected %s: %+v", what, value)
	case <-timer.C:
	}
}
