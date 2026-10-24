---
status: accepted
---

# Platform core and Kodama agent layer

The owner has clarified that Optimi Platform is the company core and Kodama is the agent layer and autonomous SDLC for its tenants. Preserve a single authority for core tenant membership, entitlements, domain operations, and core credentials while Kodama owns work, source, agent execution, evidence, and reviewed software delivery.

The earlier standalone factory design did not name this upstream boundary. Reimplementing its own authoritative hosted tenant directory or forwarding broad credentials would create competing policy and software design drift. Hosted operation therefore adapts supported core contracts through narrow consumer-owned ports. Standalone/on-premises operation may supply explicit local adapters without becoming a fallback during core failure.

This decision establishes responsibility, not process/database colocation, a shared Go package, or an OAuth exchange. Kodama runs on its own Kubernetes cluster on AWS, GCP, or Scaleway and requires PostgreSQL and that cloud's KMS. Shared library choices in P-16 follow platform without importing its module. The current platform MCP contract is read-only/user-authorized, and its secret store retains the current value rather than Kodama's pinned versions. Those gaps require focused owner decisions and integration evidence before hosted support is claimed.

The trade-off is an explicit hosted integration checkpoint instead of treating a local factory demo as a company-platform integration. [Platform integration](../specs/platform-integration.md) owns behavior and acceptance; [O-18/O-19](../decisions.md#open-decisions-and-resolved-selections) own remaining interface choices. Existing execution, human-review, and isolation decisions remain applicable.

## Deployment clarification — 2026-10-20

[C-19](../decisions.md#confirmed-direction) supersedes the separate-cluster assumption above for the first staging installation: Kodama will use Platform's stage cluster with a separate control-plane namespace and scoped execution namespaces. Independent deployability does not require a separate physical cluster. The appliance may also run in other clusters, connected through its public API. The authority, database/credential separation and hosted-integration requirements in this ADR still apply.

## Authority and product clarification — 2026-10-20

The subsequent owner selections resolve the direction of O-18/O-19: use the Optimi OAuth server's verified UUID tenant and role claims for hosted resource/RLS authority, and the existing KMS-encrypted PostgreSQL secrets module with immutable version pins, audited read-for-use and proxy-first agent delivery. Explicit secret-to-ENV disclosure is permitted for ordinary script/build children. T-061 still qualifies the actual supported claims, current-access/revocation and version interfaces; historical upstream gaps above are not evidence those capabilities have since shipped.

Platform owns subscription billing, commercial usage/access and entitlements, and is the only intended host of an optional, deferred UI. Kodama's primary surfaces are CLI/API/MCP/webhooks and its business logic stays focused on SDLC. Application chassis is tenant/project Git-versioned Markdown guidance rather than a runtime library. These selections preserve the separate authority/deployability boundary in this ADR.
