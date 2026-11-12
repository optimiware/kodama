// Differential proof for paged combined-log redaction: the concatenation of
// every bounded page must equal one full-log longest-match-first scan, including
// nested, prefix, overlapping and self-overlapping configured values.
package kube

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/kernel/engine"
)

const differentialRandomIterations = 60_000

// TestLogsPagedOutputMatchesFullLogReference compares the real adapter cursor
// pipeline with a whole-log reference redaction. The corpus is deterministic:
// an exhaustive sweep over tiny logs and tricky value sets for every page limit
// from 1 to 32, plus a fixed-seed random sweep. At least 100k fixtures run.
func TestLogsPagedOutputMatchesFullLogReference(t *testing.T) {
	fake, _, adapter := testSetup(t)
	identity := testIdentity()
	pod := testRunningPod(identity)
	fake.addPod(pod)
	secretName := credentialSecretName(identity.WorkloadID)
	ctx := context.Background()
	fixtures := 0

	run := func(values []string, log []byte, limit int) {
		fixtures++
		secret := Secret{
			Name:      secretName,
			Namespace: testNamespace,
			Data:      make(map[string][]byte, len(values)),
		}
		for index, value := range values {
			secret.Data[fmt.Sprintf("V%d", index)] = []byte(value)
		}
		if _, err := fake.PutSecret(ctx, secret); err != nil {
			t.Fatalf("fixture %d PutSecret: %v", fixtures, err)
		}
		fake.setPodLogs(pod.Name, LogStreamCombined, log)
		fake.resetCallLog()

		cursor := int64(0)
		var got []byte
		for step := 0; ; step++ {
			if step > len(log)+8 {
				t.Fatalf("fixture %d did not terminate: values %q log %q limit %d",
					fixtures, values, log, limit)
			}
			batch, err := adapter.Logs(ctx, engine.LogsRequest{
				Identity:    identity,
				AfterOffset: cursor,
				Limit:       limit,
			})
			if err != nil {
				t.Fatalf("fixture %d Logs: %v (values %q log %q limit %d)",
					fixtures, err, values, log, limit)
			}
			requests := fake.logRequestsSnapshot()
			if len(requests) == 0 {
				t.Fatalf("fixture %d issued no log read", fixtures)
			}
			forwarded := requests[len(requests)-1]
			position, _ := decodeLogCursor(cursor)
			if forwarded.Offset != position {
				t.Fatalf("fixture %d forwarded offset %d; want %d", fixtures, forwarded.Offset, position)
			}
			if forwarded.Limit != limit {
				t.Fatalf("fixture %d forwarded limit %d; want %d", fixtures, forwarded.Limit, limit)
			}
			if forwarded.Stream != LogStreamCombined {
				t.Fatalf("fixture %d forwarded stream %q; want combined", fixtures, forwarded.Stream)
			}

			var page []byte
			for _, chunk := range batch.Chunks {
				if chunk.Sequence != 1 {
					t.Fatalf("fixture %d sequence = %d; want 1", fixtures, chunk.Sequence)
				}
				if len(chunk.Data) > limit {
					t.Fatalf("fixture %d emitted %d bytes over the bound %d",
						fixtures, len(chunk.Data), limit)
				}
				wantTruncated := position+int64(limit) < int64(len(log))
				if chunk.Truncated != wantTruncated {
					t.Fatalf("fixture %d truncated = %v; want %v", fixtures, chunk.Truncated, wantTruncated)
				}
				text := string(chunk.Data)
				for _, value := range values {
					if strings.Contains(text, value) {
						t.Fatalf("fixture %d leaked value %q in %q", fixtures, value, text)
					}
				}
				page = append(page, chunk.Data...)
			}
			got = append(got, page...)

			if batch.NextOffset == cursor {
				if position != int64(len(log)) {
					t.Fatalf("fixture %d stalled at %d of %d: values %q log %q limit %d",
						fixtures, position, len(log), values, log, limit)
				}
				break
			}
			next, _ := decodeLogCursor(batch.NextOffset)
			if next <= position || next > int64(len(log)) {
				t.Fatalf("fixture %d cursor moved %d -> %d over %d bytes", fixtures, position, next, len(log))
			}
			cursor = batch.NextOffset
		}

		want := referenceRedaction(log, values, limit)
		if string(got) != want {
			t.Fatalf("fixture %d paged output = %q; reference = %q (values %q log %q limit %d)",
				fixtures, got, want, values, log, limit)
		}
	}

	// Exhaustive: every log of length 0..6 over a two-letter alphabet against
	// trickier and trickier value panels, for every page bound 1..32.
	logs := exhaustiveLogs("ab", 6)
	for limit := 1; limit <= 32; limit++ {
		for _, values := range differentialValuePanel(limit) {
			for _, log := range logs {
				run(values, log, limit)
			}
		}
	}

	// Nested boundary: the page ends exactly after the shorter value while a
	// longer value that starts with it continues past the page. The marker still
	// fits the page bound, so a scanner that holds only raw runs emits the
	// shorter marker and leaks the longer value's tail.
	nested := []struct{ short, long string }{
		{"aaaaaaaaaa", "aaaaaaaaaaa"},
		{"aaaaaaaaaa", "aaaaaaaaaaaaaaaa"},
		{"ababababab", "abababababa"},
		{"ababababab", "abababababababab"},
		{"0123456789", "0123456789abcdef"},
		{"aaaaaaaaaaaa", "aaaaaaaaaaaaa"},
	}
	for _, pair := range nested {
		for limit := len(pair.long); limit <= 32; limit++ {
			// The filler puts the shorter value's end exactly on the page end,
			// and leaves room for its marker inside the bound.
			filler := strings.Repeat("x", limit-len(pair.short))
			log := []byte(filler + pair.long + "x")
			run([]string{pair.short, pair.long}, log, limit)
		}
	}

	// Seeded random: longer logs, random value sets built from the same tiny
	// alphabet so prefixes, overlaps and self-overlaps appear naturally.
	rng := rand.New(rand.NewSource(20261107))
	for range differentialRandomIterations {
		limit := 1 + rng.Intn(32)
		maxValue := min(limit, 6)
		seen := make(map[string]bool)
		values := make([]string, 0, 4)
		// The tiny alphabet bounds how many distinct words a length range can
		// hold, so a narrow bound must not ask for more values than exist.
		possible := (1 << (maxValue + 1)) - 2
		wantValues := min(1+rng.Intn(4), possible)
		for len(values) < wantValues {
			value := randomWord(rng, "ab", 1+rng.Intn(maxValue))
			if seen[value] {
				continue
			}
			seen[value] = true
			values = append(values, value)
		}
		log := []byte(randomWord(rng, "ab", rng.Intn(41)))
		run(values, log, limit)
	}

	if fixtures < 100_000 {
		t.Fatalf("differential corpus had %d fixtures; want at least 100000", fixtures)
	}
	t.Logf("differential corpus: %d fixtures", fixtures)
}

func randomWord(rng *rand.Rand, alphabet string, length int) string {
	buffer := make([]byte, length)
	for index := range buffer {
		buffer[index] = alphabet[rng.Intn(len(alphabet))]
	}
	return string(buffer)
}

// exhaustiveLogs returns every string over alphabet of length 0..max.
func exhaustiveLogs(alphabet string, max int) [][]byte {
	logs := [][]byte{{}}
	frontier := [][]byte{{}}
	for length := 1; length <= max; length++ {
		next := make([][]byte, 0, len(frontier)*len(alphabet))
		for _, prefix := range frontier {
			for index := 0; index < len(alphabet); index++ {
				word := make([]byte, len(prefix)+1)
				copy(word, prefix)
				word[len(prefix)] = alphabet[index]
				next = append(next, word)
			}
		}
		logs = append(logs, next...)
		frontier = next
	}
	return logs
}

// differentialValuePanel returns value sets whose longest value fits limit. A
// value longer than the page bound could never be advanced past safely, so the
// paged reader deliberately reports no progress for it; that behavior is pinned
// separately rather than in a corpus that expects termination.
func differentialValuePanel(limit int) [][]string {
	candidates := [][]string{
		{"a"},
		{"b"},
		{"aa"},
		{"ab"},
		{"a", "b"},
		{"a", "aa"},
		{"aa", "aaa"},
		{"aa", "aaa", "aaaa"},
		{"ab", "abab"},
		{"abab", "ababab"},
		{"aba", "ababa"},
		{"ab", "baba"},
		{"a", "ab", "aba"},
		{"ab", "aba", "abab"},
		{"a", "b", "aa", "ab", "aba", "abab"},
	}
	panel := make([][]string, 0, len(candidates))
	for _, values := range candidates {
		fits := true
		for _, value := range values {
			if len(value) > limit {
				fits = false
				break
			}
		}
		if fits {
			panel = append(panel, values)
		}
	}
	return panel
}

// referenceRedaction is one full-log leftmost longest-match scan. When the bound
// is smaller than the marker itself the adapter clips the marker (never raw
// content), so the reference models that same bound; every other case compares
// byte for byte with the configured marker.
func referenceRedaction(log []byte, values []string, limit int) string {
	marker := []byte(Redacted)
	if len(marker) > limit {
		marker = marker[:limit]
	}
	out := make([]byte, 0, len(log))
	for index := 0; index < len(log); {
		longest := 0
		for _, value := range values {
			if len(value) <= len(log)-index &&
				string(log[index:index+len(value)]) == value &&
				len(value) > longest {
				longest = len(value)
			}
		}
		if longest > 0 {
			out = append(out, marker...)
			index += longest
			continue
		}
		out = append(out, log[index])
		index++
	}
	return string(out)
}
