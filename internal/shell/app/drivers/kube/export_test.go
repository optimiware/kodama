package kube

import "testing"

// SetServiceAccountDefaultsForTest points the conventional service-account
// fallback at tokenFile and caFile for the duration of one test and restores
// the production paths when the test finishes. The seam is package-wide, so a
// test that calls it must not run in parallel.
func SetServiceAccountDefaultsForTest(t *testing.T, tokenFile, caFile string) {
	t.Helper()
	previousToken, previousCA := defaultTokenFile, defaultCAFile
	defaultTokenFile, defaultCAFile = tokenFile, caFile
	t.Cleanup(func() {
		defaultTokenFile, defaultCAFile = previousToken, previousCA
	})
}
