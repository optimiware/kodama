# 0011 — Target-agnostic deployment through profiles and pinned drivers

**Status:** Proposed (no implementation claimed)

## Context

Kodama's first delivery contract is Kubernetes-shaped. An environment names a
namespace and a namespace-scoped kubeconfig, a deployment carries an OCI image
and Kubernetes object references, and cleanup deletes recorded objects. That is
the right first target, but it is not a general deployment model: a Cloudflare
Worker, a static/Pages bundle or another provider cannot be represented without
a special case in the control plane.

Two constraints shape the decision:

- Kodama's first deployment contract is "ordinary scripts plus a durable
  deployment record", not a provider executor. Build and deploy steps already
  run as restricted containers and report structured outcomes.
- Long-lived edge/cloud configuration — zone-level WAF, DNS and certificate
  policy — is owned by Platform or a separate infrastructure-as-code workflow.
  Adding those concerns to the application deployment step would give every
  deploy an unnecessarily broad credential and no state or drift model.

A website still needs a concrete deployment target now. The design must let one
application select a target by configuration without turning the server into a
collection of provider SDKs, and without breaking the qualified Kubernetes
path.

## Decision

Deployment targets are **configuration**, selected by a versioned installation
**target profile**.

A target profile declares:

- the provider and a human-readable description;
- the artifact kinds it accepts (`ociImage` or `bundle`);
- the credential bindings it requires, including each binding's secret kind and
  delivery form (`file` or `env`);
- the named egress profiles it may use;
- a scalar configuration schema (required fields, types, patterns, enums and
  bounds);
- a digest-pinned **driver**: either the existing project deploy-script mode or
  a driver image implementing the `kodama-deploy/v1` protocol with fixed
  commands (`register`, `apply`, `verify`, `observe`, `delete`).

The profiles are installation configuration in strict YAML, reviewed and mounted
like the chart's settings document. Unknown fields, duplicate profile names,
unpinned driver images and invalid configuration schemas refuse startup.

An environment stores the selected profile name, its digest, the target
configuration and the required credential references. Admission validates the
profile and configuration, then pins the profile digest and the resolved
provider, artifact kinds, egress profiles and target configuration into an
immutable configuration snapshot. A later profile edit does not silently change
an admitted run; a changed profile refuses resolution until the environment is
explicitly updated.

A deployment records:

- a provider-neutral **artifact**: an OCI image reference/digest or an immutable
  bundle artifact ID/digest/media type;
- provider-neutral **external resources**: provider, type, stable ID, name,
  version/etag, URL and readiness, rather than Kubernetes-only object identities;
- the pinned environment/configuration digest and the verified artifact
  provenance.

Provider mechanics live in the pinned driver. The server never calls a provider
API and holds no provider credential. The driver receives only the credentials
the profile requires, and reports structured outcomes through the existing
scoped deployment callback interface. Duplicate or unknown outcomes are observed
before another apply, exactly as the current Kubernetes scripts do.

The builtin `kubernetes-namespace` profile keeps the existing namespace,
kubeconfig, deploy-script and object-reference behavior. Existing environments,
retained v1 snapshots and deployments remain valid. The generic model is
additive and is accepted only if the existing Kubernetes deployment and cleanup
suites continue to pass unchanged.

## Alternatives considered

**Compile provider SDKs into the server.** Strong typing and direct observation,
but it makes the control plane depend on every provider's API, credentials and
release cycle, and contradicts the ownership boundary that Platform owns
long-lived edge/cloud configuration. Rejected for the first version.

**A separate environment type per provider.** Faster for one provider, but it
multiplies API, persistence and authorization shapes and makes a third target a
new domain model. Rejected.

**Arbitrary project scripts with no profile.** Maximum flexibility, but no
validation of target configuration or artifact kind, no required credential
contract and no way to select a qualified driver. Retained only as the builtin
script mode for Kubernetes.

## Consequences

Positive:

- Adding a provider is a new profile and a pinned driver image, not a new
  control-plane executor or database shape.
- Target configuration, artifact kind and credential requirements are validated
  and pinned before any external change.
- The Kubernetes path remains the compatibility baseline and the first qualified
  target.

Negative:

- Drivers are a real contract: they must implement idempotent register, apply,
  observe and delete semantics, and they must be qualified per provider.
- Bundle deployments need an attempt-scoped artifact download/materialization
  path; the OCI path does not require it.
- The server cannot observe a provider directly without a driver run or a
  read-only observer. Unknown outcomes therefore depend on the driver and the
  cleanup/settlement run, not on a server-side API call.
- A target profile is installation-global configuration. Project-specific
  provider accounts or long-lived zone operations still need explicit
  authorization and, for WAF/DNS/certificates, a separate Platform or IaC
  workflow.

## Scope boundary

This decision covers application artifact deployment. It does not make WAF
rules, DNS zones, certificates or other long-lived edge configuration a
deployment step. Those remain with Platform or a dedicated
infrastructure-as-code pipeline with its own state, plan/approval, drift and
expiry controls.

## Follow-up decisions

- Whether Platform performs a provider mutation through a supported HTTP/MCP
  operation or Kodama runs the pinned driver directly.
- The exact bundle media type and download contract.
- The first production target profile and its credential scopes.
- Whether protected environments and promotion/rollback are part of the first
  provider implementation or a separate delivery increment.
