# ADR 0001: The ERP is authoritative; local intent is durable

**Status:** accepted (blueprint default; the application owner confirms).

A local transaction cannot atomically commit a remote ERP effect. The application therefore records a durable
intent with a stable operation key, calls the ERP outside any transaction, and records the observed outcome.
An exception is `resolved` only on acknowledgment; uncertainty is a visible state (`unknown`) reconciled by key.

Alternatives rejected: marking resolved optimistically (can lie), a distributed transaction (the ERP offers
none), and a message broker (an extra moving part for a bounded workflow; PostgreSQL holds the intent).
