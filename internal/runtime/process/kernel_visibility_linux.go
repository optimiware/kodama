//go:build linux

package process

import "os"

// tableVisibility verifies that `ps -A` lists every user's processes: on Linux
// that holds only if procfs is mounted without hidepid, which the mount table
// must show. Missing, malformed or unmatched evidence is not verification.
func tableVisibility() error {
	raw, err := os.ReadFile("/proc/mounts")
	return procfsUnrestricted(string(raw), err)
}
