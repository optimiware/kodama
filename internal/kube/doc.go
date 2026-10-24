// Package kube is Kodama's Kubernetes runtime (task T-021). It turns committed
// execution attempts into restricted, observable Jobs and keeps the cluster
// consistent with them. It is the staging and production backend.
//
// The package implements three seams owned by other modules and nothing else:
// execution.Planner and execution.Runtime (T-020, T-086) and
// runtimeauth.JobObserver (T-019). The runtime requires the Planner: a spec
// fingerprint is the full recipe fingerprint of the same policy.
// Execution owns every durable outcome: attempt status, deadlines,
// reservations, cancellation and settlement. This package performs external
// effects (Jobs, Pods and attempt-scoped admission Secrets in the tenant
// namespace), verifies what it finds against committed data, and reports
// evidence through execution.Service.ObserveAttempt. It never writes execution
// or grant tables and holds no SQL transaction while calling Kubernetes.
//
// Layout:
//
//   - policy.go, quantity.go: the immutable ExecutionPolicy (tenant namespace and
//     service account, exact injector image, resources, callback origin).
//   - recipe.go, template.go: the pure, deterministic Job recipe, its restricted
//     Pod template and the spec fingerprint that binds both.
//   - verify.go: ownership and controlled-spec verification before adoption.
//   - workload.go: the external-effect state machine, kept apart from execution
//     statuses.
//   - runtime.go, credential.go: crash-safe start, observation, conservative
//     termination, retention-aware removal and admission provisioning.
//   - observer.go, reconcile.go, preflight.go: runtime-admission observation,
//     reconciliation and orphan cleanup, and namespace policy checks.
//   - kubernetes/: the narrow standard-library HTTP adapter (wire types stay there
//     and in the template/verification code that must speak them).
//   - adapters/: bridges to the PostgreSQL execution store and runtimeauth.
package kube
