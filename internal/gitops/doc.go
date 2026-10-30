// Package gitops is the operational half of the internal Git service (T-138;
// ADR 0011, docs/specs/git-repositories.md "Retention, maintenance and
// recovery", GR-13 to GR-15, GR-A09, GR-A10, seam S9 of
// docs/implementation/internal-git-service.md):
//
//   - Scheduler runs git maintenance through the tenant's worker for every
//     repository, serialized, on a configurable cadence, and prunes merged
//     task, agent and run branches under the retention rules of PruneRepository.
//   - Pins keeps a run's commits reachable with hidden retained refs
//     (refs/kodama/runs/<runId>/<n>) and releases them once retention allows.
//   - Backups pairs a volume snapshot with one bundle per repository in object
//     storage, described by a manifest that is written last, under a per-tenant
//     lease that serializes backups and expiry across instances.
//   - Recovery restores a tenant's repositories (volume snapshot, bundle replay
//     through the worker's restore fence) with Git access closed until push
//     records, refs, pins and the recovery point reconcile; the selected source
//     and every completed step are durable, and a difference from the recovery
//     point needs an audited operator resolution; a
//     volume restored from an older snapshot settles as unknown, never refused
//     (gitpush's regression latch), and the restored journal becomes the new
//     baseline only after nothing is in flight.
//   - Offboarding revokes the tenant's Git grants, settles its pushes and asks
//     the storage provider (T-129 OffboardTenant) to delete the volume only
//     after retention, recording audit first.
//
// Its state is the repositories themselves (retained refs, the journal), the
// backup manifests in object storage and the durable state of migration 016
// (package postgres: backup leases, recovery steps and resolutions, prune
// intents). Every collaborator is a small consumer-owned port; the composition
// (T-132) adapts the worker (repository.Hosts and T-160's object operations), the
// object store, the volume snapshotter, the Git endpoint's access fence and the
// tenant lifecycle. No code here changes a repository except through the worker;
// package localgit is the read-only Objects implementation for tests and for a
// process that mounts the volume. Nothing is wired yet.
package gitops
