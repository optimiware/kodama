package eventstest_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/events/eventstest"
	"github.com/optimiware/kodama/test/support/layer"
)

// TestPostgresEventsContract runs the same contract on the PostgreSQL adapter.
// It is recorded in the `test` environment (T-026) and skips in the unit layer
// and without PostgreSQL.
func TestPostgresEventsContract(t *testing.T) {
	layer.SkipService(t, "PostgreSQL")
	eventstest.Contract(t, eventstest.OpenPostgres)
}

// TestJetStreamBrokerContract runs the broker contract on the JetStream
// adapter in the `test` environment.
func TestJetStreamBrokerContract(t *testing.T) {
	layer.SkipService(t, "NATS")
	eventstest.BrokerContract(t, eventstest.OpenJetStream)
}
