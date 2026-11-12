package log

// This package declares no effectful log port.
//
// Live and retained log delivery is an adapter/NATS contract and the runtime
// capture seam belongs to the Kernel adapter. This package declares only the
// chunk identity and size bound so producers and consumers agree without
// sharing a mutable log store.
