//go:build component

package messaging_test

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	natsdriver "github.com/optimiware/kodama/internal/shell/app/drivers/nats"
	broker "github.com/optimiware/kodama/internal/shell/integration/nats"
	support "github.com/optimiware/kodama/test/support/infrastructure/messaging"
)

// TestNATSTLSJetStream proves the NATS seam works against a real
// JetStream broker that serves verified mutual TLS from a private CA: stream
// provisioning, deduplicated publication and read-back all happen over the
// configured trust and client certificate, while an untrusted CA is refused.
func TestNATSTLSJetStream(t *testing.T) {
	support.TrackTest(t)
	instance := support.StartNATSTLS(t)
	instance.EnsureStreams(t)

	ctx := instance.Shard.Context()
	config := instance.DriverConfig("conn-tls-jetstream")
	config.TLS = &natsdriver.TLSConfig{
		CAFile:   instance.CAFile,
		CertFile: instance.ClientCertFile,
		KeyFile:  instance.ClientKeyFile,
	}
	client := instance.DialDriver(t, config)

	subject, err := broker.EventSubject(projectScope())
	if err != nil {
		t.Fatalf("event subject: %v", err)
	}
	payload := []byte(`{"tls":true}`)
	if err := client.PublishJetStream(ctx, subject, id.EventID("tls-evt-1"), payload); err != nil {
		t.Fatalf("publish over mutual TLS: %v", err)
	}
	instance.WaitForStreamMessages(t, support.EventsStream, 1)

	// The stable event id is still the deduplication identity over TLS.
	if err := client.PublishJetStream(ctx, subject, id.EventID("tls-evt-1"), payload); err != nil {
		t.Fatalf("republish over mutual TLS: %v", err)
	}
	info := instance.StreamInfo(t, support.EventsStream)
	if info.State.Msgs != 1 {
		t.Fatalf("stored messages after deduplicated republish = %d; want 1", info.State.Msgs)
	}

	messages := fetch(t, instance.JetStream(t), support.EventsStream, jetstream.ConsumerConfig{
		Durable:       "inspect-tls",
		AckPolicy:     jetstream.AckExplicitPolicy,
		DeliverPolicy: jetstream.DeliverAllPolicy,
	}, 2, 5*time.Second)
	if len(messages) != 1 {
		t.Fatalf("fetched %d messages over mutual TLS; want 1", len(messages))
	}
	if messages[0].Subject != string(subject) {
		t.Fatalf("message subject = %q; want %q", messages[0].Subject, subject)
	}
	if string(messages[0].Data) != string(payload) {
		t.Fatalf("message payload = %q; want %q", messages[0].Data, payload)
	}
	if err := messages[0].ack(); err != nil {
		t.Fatalf("ack message: %v", err)
	}

	// The same real broker rejects a client that trusts a different CA; the
	// failure stays bounded and carries no insecure fallback.
	untrusted := instance.DriverConfig("conn-tls-untrusted")
	untrusted.TLS = &natsdriver.TLSConfig{
		CAFile:   instance.UntrustedCAFile,
		CertFile: instance.ClientCertFile,
		KeyFile:  instance.ClientKeyFile,
	}
	started := time.Now()
	_, err = natsdriver.Dial(ctx, untrusted)
	requireClass(t, err, fault.DependencyUnavailable)
	if elapsed := time.Since(started); elapsed > 15*time.Second {
		t.Fatalf("untrusted TLS dial took %s; want a bounded failure", elapsed)
	}

	support.Note(t, "tls_jetstream_stored_messages", "1")
	support.Note(t, "tls_untrusted_ca_refused", "true")
}
