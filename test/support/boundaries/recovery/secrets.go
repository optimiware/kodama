package recoveryfix

import (
	"os"
	"path/filepath"
	"strings"
)

// negativeControl is a value that must never appear anywhere the scan looks.
// Its absence proves the scan is not matching everything.
const negativeControl = "kodama-i03d-negative-control-value"

// RequireNoSecrets proves that the durable journal, the app server log, the
// contract/OIDC fixture logs, the captured git stdout/stderr and any extra
// diagnostic text never render a credential or an opaque actor handle.
//
// The scan is deliberately independent of the product's own field encoding: it
// reads raw journal bytes. A planted canary proves the scanner can fail, and a
// known-absent negative control proves it is not matching everything.
func (harness *Harness) RequireNoSecrets(tb TB, extraTexts ...string) {
	tb.Helper()
	tokens := []string{ServiceToken, ExecutionToken, harness.HumanToken}

	// Positive control: the scanner must find a planted canary.
	canary := "kodama-i03d-scan-canary-value"
	canaryPath := filepath.Join(harness.Root, "scan-canary.txt")
	if err := os.WriteFile(canaryPath, []byte(canary), 0o600); err != nil {
		tb.Fatalf("write scan canary: %v", err)
	}
	hits, err := ScanTree(harness.Root, canary)
	if err != nil {
		tb.Fatalf("scan canary: %v", err)
	}
	if len(hits) != 1 || hits[0] != canaryPath {
		tb.Fatalf("scan canary hits = %v; want exactly %q", hits, canaryPath)
	}
	if err := os.Remove(canaryPath); err != nil {
		tb.Fatalf("remove scan canary: %v", err)
	}

	// Negative control: a value that was never used must not be found.
	for _, root := range []string{harness.Root, harness.ClientHome, harness.SocketBase} {
		hits, err := ScanTree(root, negativeControl)
		if err != nil {
			tb.Fatalf("scan negative control under %q: %v", root, err)
		}
		if len(hits) != 0 {
			tb.Fatalf("negative control found under %q: %v", root, hits)
		}
	}

	// A credential is never persisted in the fixture tree (repositories,
	// journal, client home, socket directory, hook probe).
	for _, token := range tokens {
		for _, root := range []string{harness.Root, harness.ClientHome, harness.SocketBase} {
			hits, err := ScanTree(root, token)
			if err != nil {
				tb.Fatalf("scan credential under %q: %v", root, err)
			}
			if len(hits) != 0 {
				tb.Fatalf("credential persisted under %q: %v", root, hits)
			}
		}
	}

	logs := []string{
		harness.LogText(),
		harness.Contract.LogText(),
		harness.Contract.RecordedJSON(),
		harness.Provider.LogText(),
		harness.Transcript(),
	}
	logs = append(logs, extraTexts...)
	for index, text := range logs {
		for _, token := range tokens {
			if strings.Contains(text, token) {
				tb.Fatalf("log %d rendered credential %q", index, token)
			}
		}
	}

	// The journal never stores the forwarded credential or the opaque actor
	// handle, and neither does any log.
	handles := harness.RecordedHandles()
	journal := harness.JournalRaw(tb)
	for name, raw := range journal {
		for _, token := range tokens {
			if strings.Contains(raw, token) {
				tb.Fatalf("journal file %q contains credential %q", name, token)
			}
		}
		if strings.Contains(raw, "kah_") {
			tb.Fatalf("journal file %q contains an actor handle", name)
		}
		for _, handle := range handles {
			if strings.Contains(raw, handle) {
				tb.Fatalf("journal file %q contains actor handle %q", name, handle)
			}
		}
	}
	for index, text := range logs {
		if strings.Contains(text, "kah_") {
			tb.Fatalf("log %d rendered an actor handle", index)
		}
		for _, handle := range handles {
			if strings.Contains(text, handle) {
				tb.Fatalf("log %d rendered actor handle %q", index, handle)
			}
		}
	}
}
