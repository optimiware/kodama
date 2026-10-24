# 2. Personas and user journeys

| Persona | Work |
| --- | --- |
| Analyst | Describe a business gap and acceptance; review outcomes |
| Engineer | Refine specifications, edit/push source, supervise automation |
| Reviewer | Inspect evidence and record a human decision |
| Administrator | Manage memberships, project settings, credentials, capacity |
| Operator | Install, observe, upgrade, restore |
| Agent | Execute a bounded objective under workload identity |

Roles compose. Analysts who accept work also receive reviewer authority. Automation never inherits human review permission.

## Journey J-01: an ERP inventory exception application

1. Analyst creates a story with `spec` and acceptance criteria.
2. Engineer adds the plan within the spec and selects blueprint guidance.
3. Human pulls ready work into active WIP capacity.
4. Factory provisions a Git work branch; humans clone/push using existing editors.
5. An optional prototype clarifies interaction; feedback updates the content revision.
6. Agent or engineer creates a committed candidate and tests.
7. A pipeline gathers evidence and waits for a human gate without compute.
8. Ordinary build/deploy steps publish a verified image and preview.
9. Stage promotion reuses the image with explicit runtime configuration.
10. E2E and exploratory QA produce deployment-bound evidence and defect work items.
11. Human accepts or rejects the outcome; rejection returns the work item to active work.
12. Explicit human completion closes the item. ChatOps reports meaningful events throughout.

See the [worked example](../examples/workflows/erp-gap-to-preview.md) and [staging continuation](../examples/workflows/staging-and-acceptance.md).

## Journey J-02: failure and rework

An attempt fails or asks for missing input. Its result/question and available diagnostics remain visible. The human revises intent or source and starts a new run from an accepted commit. A node loss can lose uncommitted scratch files; Git retains confirmed source. Unknown external effects are reconciled before repeat.

## Journey J-03: second tenant

Administrator grants memberships and creates a project with isolated data access, execution namespace, quotas, and credentials. Copied artifact/log/source/run identifiers cannot disclose the other tenant's resources. The declared execution trust profile governs what tenant code may run.

## Journey J-04: install and recover

Operator configures supported PostgreSQL, NATS, Git, S3, registry, OIDC, and KMS integrations. Helm installs one server. A recovery drill verifies source retention, secret decryptability, old-controller fencing, and a new safe execution after restore.
