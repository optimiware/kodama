// Package gitworker hosts each tenant's Git worker on Kubernetes (T-129, ADR
// 0011 section 3, GR-03, GR-04, IT-17).
//
// One worker Deployment and one durable volume exist per tenant, in a
// Kodama-managed service namespace (kodama-git-<tenant key>) that is separate
// from every namespace that runs tenant commands. A Reconciler renders and
// converges that set of objects from a tenant ID and a desired WorkerSpec:
//
//   - the namespace at Pod Security restricted with a pinned version, a
//     ServiceAccount without an API token, a ReadWriteOncePod claim on a
//     StorageClass whose reclaim policy is Retain, a single-replica Recreate
//     Deployment with a restricted Pod template, a ClusterIP Service, an
//     identity Secret and three NetworkPolicies (default deny, ingress from the
//     control plane only, egress to the control plane and cluster DNS only);
//   - adoption of an existing object only when its labels, its fingerprint and
//     its owner chain (the tenant, the namespace UID and, for Pods, the
//     ReplicaSet and Deployment UIDs) verify; any other object holding one of
//     the names is reported and never changed;
//   - discovery (Endpoint: tenant to Service URL), answered only for a worker the
//     last reconciliation found ready;
//   - an explicit OffboardTenant, gated by a retention check, as the only path
//     that removes a tenant's volume. Reconciling, replacing a worker, run
//     cleanup and pipeline completion never delete it.
//
// Nothing here serves Git: the worker command (kodama git serve, T-128/T-132)
// is configured through the environment documented in spec.go. The package holds
// no database, broker or signing credential; the identity it delivers to a
// worker is an issued, tenant-bound token and public verification material
// produced by the IdentityProvider port (T-136).
package gitworker
