// Package natsbus is the deployed shell's central bus: the NATS JetStream
// adapter of the bus port in internal/shell/bus, and the one place that decides
// whether a process may run without a broker.
//
// PostgreSQL stays authoritative (ADR 0002). The outbox publisher in
// internal/shell/outbox hands committed events to this adapter; JetStream
// delivers them at least once, so every consumer deduplicates on the stable
// event identity (see outbox.EventID) or on the resource it owns and tolerates
// reordering.
//
// # Profiles
//
// Open takes the deployment profile. The "test" and "production" profiles
// require a reachable NATS server: a missing URL or connection, or an
// in-process request, is a startup failure (ErrBrokerRequired,
// ErrBrokerUnavailable) and never a silent in-process fallback. Only the
// "development" profile with no NATS configured gets the in-process
// implementation, which is a development-test double and not a deployed
// profile: it holds nothing across a restart and delivers inside Publish.
//
// # Consumers
//
// A consumer is durable and named ConsumerName(service, purpose). Deliveries are
// acknowledged explicitly and only after the handler returned nil; a handler
// error naks the delivery for redelivery after RetryDelay; an undecodable
// message is terminated because redelivery cannot repair it. Because the
// consumer is durable, a restart or an outage resumes at the first
// unacknowledged message, and a consumer created later replays the retained
// stream from its start (DeliverAll), which is the replay rule: a handler MUST
// converge when the same event, or an older one, arrives again.
package natsbus
