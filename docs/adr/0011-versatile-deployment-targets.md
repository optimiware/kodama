# 0011 — Versatile deployment targets through profiles and pinned drivers

**Status:** Proposed (implementation started; not yet qualified against a live Cloudflare account)

## Context

The first delivery contract is Kubernetes-shaped: an environment names a
namespace and a namespace-scoped kubeconfig, a deployment carries an OCI image
and Kubernetes object references, and cleanup deletes recorded objects. That is
a good fit for the first staging target, but it cannot represent a Cloudflare
Worker, a static/Pages bundle or another provider without a special case in the
control plane.

`docs/specs/platform-integration.md` records that Platform owns long-lived
edge/cloud delivery and that Cloudflare/Fastly/GCP are later Platform
operations, not new Kodama executors. A single website still needs a concrete
target now, and adding a provider SDK to the server would make the control plane
depend on every provider's API and credentials.

## Decision

Deployment targets are **configuration**, selected by a versioned installation
**target profile**:

- a profile names a provider, accepted artifact kinds, required credential
  bindings and delivery forms, named egress profiles, a configuration schema and
  a pinned **driver**;
- an environment stores the profile name/version, its target configuration and
  the required credential references; admission pins the profile digest, the
  resolved provider/artifact/egress values and the configuration into a v2
  configuration snapshot;
- a deployment records a provider-neutral **artifact** (`ociImage` or `bundle`)
  and provider-neutral **external resources** (provider, type, id, name, version,
  etag, URL, readiness), not Kubernetes-only object identities;
- provider mechanics live in a digest-pinned driver image implementing the
  `kodama-deploy/v1` protocol. The server never calls a provider API and holds no
  provider credential. The Kubernetes path keeps its existing deploy scripts and
  is expressed as the builtin `kubernetes-namespace` profile;
- a profile change is explicit: an environment pins the profile digest and
  refuses admission when the installed profile no longer matches.

Target profiles are YAML, because they are installation configuration reviewed
and mounted like the chart's settings document. The file is strict: unknown
fields and invalid profiles refuse startup.

## Consequences

- Adding a provider is a new profile plus a pinned driver image, not a new
  control-plane executor or database shape.
- The environment/deployment API grows additively: legacy `namespace`,
  `kubeconfig` and `image` members remain for released clients; new
  `targetProfile`, `targetConfig`, `artifact` and `resources` members carry the
  new shape. For a profile environment the legacy `kubeconfig` member is a
  projection of the primary credential and `namespace` is empty.
- Deployable artifacts are either OCI images or immutable bundles. A bundle
  needs an attempt-scoped download/materialization path (the runtime contract's
  next additive step); the first implementation keeps the existing OCI path and
  the server-side model.
- Production promotion, protected environments and provider rollback remain
  separate work. This ADR does not make a WAF, DNS or certificate resource a
  deployment step: those are zone-level operations and stay with Platform or a
  separate IaC workflow.
- The Kubernetes delivery qualification remains the compatibility gate. The
  generic model is not accepted until the existing M2 deployment and cleanup
  suites pass unchanged.
