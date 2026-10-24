---
id: T-014
milestone: M1
status: done
depends_on: [T-004, T-005]
decision_blockers: []
owns: [internal/pipeline/, api/pipeline.yaml, contracts/pipeline/]
---
# T-014 — Pipeline parser and immutable registration

## Goal
Give CLI and API one structural/semantic validator.

## Contract
Implement [pipeline format](../contracts/pipeline-format.md), with enabled-kind/profile lookup injected. Register only supported kinds; preserve schema support for planned M2 shapes without advertising enabled behavior.

## Deliverables
YAML parser/JSON Schema, DAG/source/output validation, canonical digest, immutable versions, HTTP fragment, shared validation library.

## Acceptance
`cycle`, `unknown_output`, `parallel_writers`, `conditional_gate`, `mutable_image`, and `duplicate_definition` cover PX-01/PX-02/PX-14 and AC-12.

## Out of scope
General expression engine, dynamic graph expansion, CRDs, or validation-only HTTP resource.
