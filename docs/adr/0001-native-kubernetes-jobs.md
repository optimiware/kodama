---
status: accepted
---

# Native Kubernetes Jobs for executable steps

Kodama needs container-based execution with agent work, persistent workspaces, and human gates. Use a Go orchestrator that launches native Kubernetes Jobs for executable step attempts. The owner selected this over a Tekton-backed engine so the factory can own workflow semantics directly while keeping Kubernetes as the runtime foundation.

## Consequences

Kodama owns scheduling, DAG progression, retries, cancellation, and recovery. This is meaningful implementation work and must have acceptance coverage before automatic delivery is enabled. Kubernetes provides workload execution and observations; the factory records durable intent and decisions. Human gates use persisted workflow state rather than waiting containers.

The initial design does not require pipeline CRDs. A future executor change must preserve documented run, attempt, workspace, and evidence semantics.
