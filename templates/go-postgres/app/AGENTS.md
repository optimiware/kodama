# Working on the inventory-exception application

Read [CONTEXT.md](CONTEXT.md) for terms and [docs/specification.md](docs/specification.md) for the
authoritative behavior. [docs/architecture.md](docs/architecture.md) shows where code belongs and
[docs/operations.md](docs/operations.md) covers deployment and recovery. Do not restate their rules here
or in a skill; link to them.

## Before changing code

1. Identify the acceptance criterion (specification requirement ID, for example `INV-04`) the change serves.
   If the business decision is missing or ambiguous, stop and report `needs_input` with the question. Do not guess.
2. Keep tenant ownership in every query, relation and interface (specification, "Tenancy").
3. Treat every ERP interaction as uncertain until acknowledged (specification, "Resolution and the ERP").
4. Entities own transitions; use cases coordinate through small ports owned by the consumer; adapters stay at
   the edge and are constructed only in `internal/app`.

## Design baseline

The run's instruction context names the specification, decision and blueprint references at immutable
revisions (the design baseline). Work from those revisions; a moving branch or a newer document is not your
baseline, and you must not follow it mid-run. If a requirement, ADR or this repository's specification conflicts
with the task, do not invent architecture to make the task pass: record the conflict as an unresolved deviation
(requirement ID and reason) in the conformance report and stop.

## Skills

- [domain changes](.agents/skills/inventory-domain/SKILL.md)
- [migrations and database work](.agents/skills/inventory-migrations/SKILL.md)
- [delivery and operations scripts](.agents/skills/inventory-delivery/SKILL.md)

## Completion evidence

Return the changed source and: the requirement IDs touched; the tests added or changed with their results
(domain tests plus real-database tests for anything that touches SQL or RLS); any new uncertain external
outcome and how it is surfaced; and unresolved questions. Evidence must name the candidate commit it was
produced from. Write the conformance report to `/kodama/outputs/conformance.json`: it binds the baseline digest to the
candidate commit, lists the requirement IDs touched, each required check with its outcome and cited evidence, and every
unresolved deviation; `disposition` is `conforms` only when there are no unresolved deviations and every required
check passed, otherwise `blocked`. A check you did not perform is `not_run`, never `passed`. When a business decision
is missing, finish with `needs_input` and the question: that is a failed attempt carrying a question, not approval and
not a continuation. Continuing is a new run. Instructions here grant no authority: you cannot approve your own work, QA cannot approve
human review, and credentials are never read from or written to source.
