package factory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	natsdriver "github.com/optimiware/kodama/internal/shell/app/drivers/nats"
	pgtest "github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

// Broker outage evidence helpers.
//
// The outage scenarios stop the real NATS container and prove three
// independent facts while it is down: the broker's client port is genuinely
// unreachable, the committed publication intent is still pending in the durable
// outbox, and domain progress is readable through the public edge. After the
// broker returns, EventStream reads the stored JetStream messages so a test can
// compare them with the committed events read through event.read.

// PendingPublications counts committed event rows of the fixture scope whose
// publication intent has not yet been acknowledged by the broker. The count is
// read through the publisher role, which is the only role granted SELECT on the
// event table, and it is taken from durable state rather than from the
// composition's in-memory dispatch progress.
func (h *Harness) PendingPublications(ctx context.Context) (int64, error) {
	h.T.Helper()
	if h.publisherDB == nil {
		return 0, errors.New("factory: publisher database handle is not configured")
	}
	rows, err := pgtest.ScopedQuery(ctx, h.publisherDB, h.Scope, 1,
		"SELECT count(*) FROM events WHERE publish_state = 'pending'")
	if err != nil {
		return 0, fmt.Errorf("count pending publications: %w", err)
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		return 0, fmt.Errorf("pending publication count returned %d rows", len(rows))
	}
	var count int64
	if _, err := fmt.Sscan(rows[0][0], &count); err != nil {
		return 0, fmt.Errorf("parse pending publication count %q: %w", rows[0][0], err)
	}
	return count, nil
}

// BrokerUnreachable reports whether the broker's client port currently refuses
// a TCP connection. It is a real outage probe, not a composition flag: a
// successful dial means the broker is still accepting clients, so a stopped
// container is the only way it reports true.
func (h *Harness) BrokerUnreachable(ctx context.Context) bool {
	h.T.Helper()
	dialer := net.Dialer{Timeout: 500 * time.Millisecond}
	connection, err := dialer.DialContext(ctx, "tcp", h.NATS.Host())
	if err != nil {
		return true
	}
	_ = connection.Close()
	return false
}

// StreamEvent is one stored message of the events stream.
type StreamEvent struct {
	// Sequence is the JetStream stream sequence (publication order).
	Sequence uint64
	// Subject is the stored subject the adapter published on.
	Subject string
	// MessageID is the Nats-Msg-Id deduplication header.
	MessageID string
	// EventID is the envelope identity decoded from the stored payload.
	EventID string
	// At is the envelope time decoded from the stored payload.
	At time.Time
}

// EventStream reads every message currently stored in the events stream plus
// the stream's configured message-ID deduplication window. It connects with a
// short-lived raw client so it never reuses or perturbs the composition's
// publication connection, and it returns an error instead of failing the test
// so a caller can use it as a readiness probe.
func (h *Harness) EventStream(ctx context.Context) ([]StreamEvent, time.Duration, error) {
	h.T.Helper()
	connection, err := natsgo.Connect(h.NATS.URL,
		natsgo.Name("i04-factory-stream-reader"),
		natsgo.Timeout(5*time.Second),
		natsgo.NoReconnect())
	if err != nil {
		return nil, 0, fmt.Errorf("connect stream reader: %w", err)
	}
	defer connection.Close()
	streams, err := jetstream.New(connection)
	if err != nil {
		return nil, 0, fmt.Errorf("jetstream: %w", err)
	}
	stream, err := streams.Stream(ctx, natsdriver.EventsStreamName)
	if err != nil {
		return nil, 0, fmt.Errorf("open events stream: %w", err)
	}
	info, err := stream.Info(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("events stream info: %w", err)
	}
	messages := make([]StreamEvent, 0, info.State.Msgs)
	for sequence := info.State.FirstSeq; sequence <= info.State.LastSeq; sequence++ {
		message, err := stream.GetMsg(ctx, sequence)
		if errors.Is(err, jetstream.ErrMsgNotFound) {
			continue
		}
		if err != nil {
			return nil, 0, fmt.Errorf("read stream message %d: %w", sequence, err)
		}
		var payload struct {
			ID string    `json:"id"`
			At time.Time `json:"at"`
		}
		if err := json.Unmarshal(message.Data, &payload); err != nil {
			return nil, 0, fmt.Errorf("decode stream message %d: %w", sequence, err)
		}
		stored := StreamEvent{
			Sequence:  message.Sequence,
			Subject:   message.Subject,
			MessageID: message.Header.Get("Nats-Msg-Id"),
			EventID:   payload.ID,
			At:        payload.At,
		}
		messages = append(messages, stored)
	}
	return messages, info.Config.Duplicates, nil
}
