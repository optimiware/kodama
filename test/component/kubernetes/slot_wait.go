package kubernetesconformance

import (
	"flag"
	"time"
)

const (
	// minClusterSlotWait is the floor for one test's wait for a cluster slot.
	minClusterSlotWait = 5 * time.Minute
	// maxClusterSlotWait is the ceiling for one test's wait for a cluster slot.
	maxClusterSlotWait = 15 * time.Minute
)

// clusterSlotWaitBound bounds one test's wait for a kind cluster slot. It is
// derived from the test binary's outer -timeout so a queued test cannot consume
// the whole package budget; slotWaitBound pins the pure mapping.
func clusterSlotWaitBound() time.Duration {
	if value := flag.Lookup("test.timeout"); value != nil {
		if outer, err := time.ParseDuration(value.Value.String()); err == nil {
			return slotWaitBound(outer)
		}
	}
	return slotWaitBound(0)
}

// slotWaitBound maps the outer test timeout to a slot wait: half the outer
// bound, clamped to [5m, 15m]. An unreadable, non-positive or absent outer bound
// uses the ceiling.
func slotWaitBound(outer time.Duration) time.Duration {
	if outer <= 0 {
		return maxClusterSlotWait
	}
	bound := outer / 2
	if bound < minClusterSlotWait {
		return minClusterSlotWait
	}
	if bound > maxClusterSlotWait {
		return maxClusterSlotWait
	}
	return bound
}
