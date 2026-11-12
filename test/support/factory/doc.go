// Package factory is the reusable lifecycle harness.
//
// It builds the real Shell/Kernel composition (app.New, ProfileTest) over one
// isolated real PostgreSQL server (role-separated adapters), one real
// JetStream-enabled NATS broker and one real rootless Podman API service. The
// workload runtime is the real Podman adapter with the real *drivers/podman
// Libpod client, the real retained file log sink and the fresh-only workspace
// volume provisioner. It starts no workload on its own: tests drive the public
// REST/MCP edges and the public Go client, and the composition runs the
// containers.
//
// This package is test support, never product code. It owns no behavior under
// test: every assertion about admission, materialization, settlement and
// receipts belongs to the integration test package.
//
// WithGitService adds one optional host-only Git smart-HTTP composition beside
// the factory: real tenant provisioning (gitapp.ProvisionTenant), the real
// native pre/post-receive hooks execing a fixture-built ./cmd/kodama, a private
// hook socket, the public file-credential transport and the shared actor
// registry, all verified by the served Shell. The harness exposes it as
// Harness.Git. The optional service always selects the private-CA TLS edge.
//
// Container consumption sits beside the host path: the hosted Git listener
// stays on loopback and a host-only relay advertises it as
// host.containers.internal, the only alias a rootless pasta workload reaches.
// The workload URL is registered on both bindings. EnsureWorkloadImage builds
// the deterministic workload image from its pinned SCM base, publishes it by
// digest through the real local registry and materializes it locally; the
// fixture never tags, pushes or removes that shared base.
// WorkloadClonePushCommands authors the CA materialization, the
// per-authentication --config-env token supply, the protected-ref refusal and
// the fail-closed whole-workspace credential scan the container action runs.
//
// WithModelFixture adds the opt-in deterministic upstream model provider. It
// starts one bounded loopback TLS provider with scripted exchanges and wires
// the real configured model gateway to it with a separate gateway secret-name
// policy and a fixture CA-verifying transport rebuilt for every controller. The
// fixture replaces only the upstream model: routing, authority, credential
// custody, gateway validation and usage persistence stay real. A factory
// without the option keeps the model-disabled composition. The provider
// credential is generated per run, written through the public secret.write
// operation and never captured in evidence or logs.
//
// WithToolFixture adds the opt-in external MCP tool proxy. It starts one real
// bounded loopback HTTP MCP upstream and wires the real configured proxy, the
// real mcphttp driver, the real authorizer and the project-scoped secret store
// to it. The fixture replaces only the upstream tool: routing, current
// authority, the configured allow-list, credential custody and usage
// persistence stay real. A factory without the option keeps the
// proxy-disabled composition. The upstream credential is generated per run,
// written through the public secret.write operation and never captured in
// evidence or logs.
package factory
