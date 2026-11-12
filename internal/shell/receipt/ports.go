package receipt

// This package declares no effectful receipt port.
//
// A receipt is committed inside the business commit unit it protects
// (BindingUnit, TransitionUnit, ResolveUnit, ...), never by a separate receipt
// store call that could leave a durable result without its effect or an effect
// without its result. Crash-after-commit convergence, rollback and receipt
// uniqueness are store-conformance concerns.
