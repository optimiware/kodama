package pgtest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/shell/receipt"
)

// matcherKind names the closed set of non-exact comparisons a golden row may
// use. Each kind is allowed only on the columns whose value the test genuinely
// cannot predict; every other column is compared for exact equality.
type matcherKind string

const (
	kindDBSequence    matcherKind = "db-sequence"
	kindGeneratedID   matcherKind = "generated-id"
	kindOneOf         matcherKind = "one-of"
	kindRandomBytes   matcherKind = "random-bytes"
	kindClockWindow   matcherKind = "clock-window"
	kindJSONOf        matcherKind = "json-of"
	kindJSONSatisfies matcherKind = "json-satisfies"
)

// GoldenMatcher validates one normalized column value. The method set is
// unexported, so only this package can build a matcher: a component test cannot
// define its own loose comparison. A matcher rejects NULL; the comparison does
// that before the matcher runs.
type GoldenMatcher interface {
	match(value any) error
	kind() matcherKind
}

type namedMatcher struct {
	k  matcherKind
	fn func(value any) error
}

func (m namedMatcher) match(value any) error { return m.fn(value) }
func (m namedMatcher) kind() matcherKind     { return m.k }

// jsonColumnTypes are the column types that may hold a JSON document: jsonb
// directly, and bytea where the product stores the encoded document as bytes.
var jsonColumnTypes = map[string]bool{"jsonb": true, "json": true, "bytea": true}

// randomByteColumns are the only columns whose bytes are product randomness.
var randomByteColumns = map[string]bool{
	"token_nonce": true, "nonce": true, "wrap_nonce": true,
	"wrapped_key": true, "ciphertext": true,
}

// matcherAllowed refuses a non-exact matcher on a column outside its allowlist.
func matcherAllowed(column, columnType string, m GoldenMatcher) error {
	switch m.kind() {
	case kindDBSequence:
		if column != "publish_sequence" {
			return fmt.Errorf("%s matcher is allowed only on publish_sequence, not %q", m.kind(), column)
		}
	case kindGeneratedID:
		if column != "id" && !strings.HasSuffix(column, "_id") {
			return fmt.Errorf("%s matcher is allowed only on id columns, not %q", m.kind(), column)
		}
	case kindOneOf:
	case kindRandomBytes:
		if !randomByteColumns[column] || columnType != "bytea" {
			return fmt.Errorf("%s matcher is allowed only on random byte columns, not %q (%s)", m.kind(), column, columnType)
		}
	case kindClockWindow:
		if columnType != "timestamptz" && columnType != "timestamp" {
			return fmt.Errorf("%s matcher is allowed only on timestamp columns, not %q (%s)", m.kind(), column, columnType)
		}
	case kindJSONOf, kindJSONSatisfies:
		if !jsonColumnTypes[columnType] {
			return fmt.Errorf("%s matcher is allowed only on JSON-bearing columns, not %q (%s)", m.kind(), column, columnType)
		}
	default:
		return fmt.Errorf("unknown matcher kind %q on %q", m.kind(), column)
	}
	return nil
}

// GoldenDBSequence accepts a positive database sequence value. Losers and
// rolled-back statements consume sequence values, so the exact number is not
// stable; it is the only loose integer.
func GoldenDBSequence() GoldenMatcher {
	return namedMatcher{kindDBSequence, func(value any) error {
		number, ok := value.(goldenInt)
		if !ok || number <= 0 {
			return fmt.Errorf("value %v is not a positive sequence integer", value)
		}
		return nil
	}}
}

// GoldenGeneratedID accepts an identifier minted by product randomness that the
// test cannot inject. reason must say why the identifier cannot be derived; an
// empty reason makes the matcher fail so a loose site is always documented.
func GoldenGeneratedID(prefix, reason string) GoldenMatcher {
	return namedMatcher{kindGeneratedID, func(value any) error {
		if strings.TrimSpace(reason) == "" {
			return fmt.Errorf("generated-id matcher requires a reason")
		}
		text, ok := value.(goldenText)
		if !ok {
			return fmt.Errorf("value %v is not text", value)
		}
		if fmt.Sprint(value) == "" {
			return fmt.Errorf("value is empty")
		}
		if !strings.HasPrefix(string(text), prefix) {
			return fmt.Errorf("value %q does not start with %q", text, prefix)
		}
		return nil
	}}
}

// GoldenOneOf accepts exactly one of the listed values. A race winner's
// identity is one of N known values; every other value fails.
func GoldenOneOf(values ...any) GoldenMatcher {
	return namedMatcher{kindOneOf, func(actual any) error {
		if len(values) == 0 {
			return fmt.Errorf("one-of matcher has no candidates")
		}
		for _, candidate := range values {
			expected, err := normalizeGoldenExpected(candidate, "")
			if err != nil {
				return fmt.Errorf("one-of candidate %v: %w", candidate, err)
			}
			if _, loose := expected.(GoldenMatcher); loose {
				return fmt.Errorf("one-of candidate %v must be an exact value", candidate)
			}
			if goldenCompare(actual, expected) == nil {
				return nil
			}
		}
		return fmt.Errorf("value %v is none of %v", actual, values)
	}}
}

// GoldenRandomBytes accepts exactly n product-random bytes that are not all
// zero. A zeroed value of the right length is the defect it exists to catch;
// pair it with a behavioral round trip of the value it protects.
func GoldenRandomBytes(n int) GoldenMatcher {
	return namedMatcher{kindRandomBytes, func(value any) error {
		hexValue, ok := value.(goldenBytes)
		if !ok {
			return fmt.Errorf("value %v is not bytea", value)
		}
		decoded, err := hex.DecodeString(string(hexValue))
		if err != nil {
			return fmt.Errorf("value %q is not hex: %w", hexValue, err)
		}
		if len(decoded) != n {
			return fmt.Errorf("random value is %d bytes; want %d", len(decoded), n)
		}
		if bytes.Equal(decoded, make([]byte, n)) {
			return fmt.Errorf("random value is all zero")
		}
		return nil
	}}
}

// ClockWindow pins a group of timestamp columns written from one clock reading.
// The first column compared binds the reading, which must lie in [low, high];
// every later column must equal that reading plus its exact offset. Use one
// window per unit invocation and only for columns a clock the test cannot inject
// wrote (the database clock or the process wall clock).
type ClockWindow struct {
	low, high time.Time
	mu        sync.Mutex
	bound     bool
	base      time.Time
}

// NewClockWindow returns a window whose reading must lie in [low, high].
func NewClockWindow(low, high time.Time) *ClockWindow {
	return &ClockWindow{low: low.UTC(), high: high.UTC()}
}

// Reading returns the bound clock reading and whether a column has bound it.
func (c *ClockWindow) Reading() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.base, c.bound
}

// At matches the bound reading plus offset exactly.
func (c *ClockWindow) At(offset time.Duration) GoldenMatcher {
	return namedMatcher{kindClockWindow, func(value any) error {
		stamp, ok := value.(goldenTime)
		if !ok {
			return fmt.Errorf("value %v is not a timestamp", value)
		}
		observed := time.Time(stamp).UTC()
		c.mu.Lock()
		defer c.mu.Unlock()
		if !c.bound {
			base := observed.Add(-offset)
			if base.Before(c.low) || base.After(c.high) {
				return fmt.Errorf("clock reading %s (value %s minus offset %s) is outside [%s,%s]",
					base, observed, offset, c.low, c.high)
			}
			c.base, c.bound = base, true
			return nil
		}
		if want := c.base.Add(offset); !observed.Equal(want) {
			return fmt.Errorf("value %s; want bound reading %s plus %s = %s", observed, c.base, offset, want)
		}
		return nil
	}}
}

// canonicalJSON returns the key-sorted, number-exact encoding of one document.
func canonicalJSON(raw []byte) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return "", fmt.Errorf("not JSON: %w", err)
	}
	if decoder.More() {
		return "", fmt.Errorf("trailing data after the JSON document")
	}
	canonical, err := json.Marshal(decoded)
	if err != nil {
		return "", err
	}
	return string(canonical), nil
}

// goldenJSONBytes returns the document bytes of a jsonb or bytea actual value.
func goldenJSONBytes(value any) ([]byte, error) {
	switch typed := value.(type) {
	case goldenJSON:
		return []byte(typed), nil
	case goldenBytes:
		decoded, err := hex.DecodeString(string(typed))
		if err != nil {
			return nil, fmt.Errorf("value %q is not hex: %w", typed, err)
		}
		return decoded, nil
	default:
		return nil, fmt.Errorf("value %v is neither jsonb nor bytea", value)
	}
}

// GoldenJSONOf accepts a jsonb or bytea value holding exactly the JSON encoding
// of want, compared as canonical documents: key order is ignored, every value is
// exact, and an extra or missing key fails.
func GoldenJSONOf(want any) GoldenMatcher {
	encoded, encodeErr := json.Marshal(want)
	return namedMatcher{kindJSONOf, func(value any) error {
		if encodeErr != nil {
			return fmt.Errorf("expectation %v is not encodable: %w", want, encodeErr)
		}
		wantCanonical, err := canonicalJSON(encoded)
		if err != nil {
			return fmt.Errorf("expectation: %w", err)
		}
		raw, err := goldenJSONBytes(value)
		if err != nil {
			return err
		}
		gotCanonical, err := canonicalJSON(raw)
		if err != nil {
			return fmt.Errorf("actual %q: %w", raw, err)
		}
		if gotCanonical != wantCanonical {
			return fmt.Errorf("json %s; want %s", gotCanonical, wantCanonical)
		}
		return nil
	}}
}

// GoldenJSONSatisfies accepts a jsonb or bytea value whose document passes
// check. It is for a document the test cannot spell as one literal (the admitted
// execution snapshot) and must assert named fields of, not mere presence.
func GoldenJSONSatisfies(description string, check func(raw []byte) error) GoldenMatcher {
	return namedMatcher{kindJSONSatisfies, func(value any) error {
		if strings.TrimSpace(description) == "" || check == nil {
			return fmt.Errorf("json-satisfies matcher requires a description and a check")
		}
		raw, err := goldenJSONBytes(value)
		if err != nil {
			return err
		}
		if err := check(raw); err != nil {
			return fmt.Errorf("%s: %w", description, err)
		}
		return nil
	}}
}

// SHA256 returns the SHA-256 digest of b.
func SHA256(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// FingerprintBytes returns the bytea form of Fingerprint(label): the digest a
// receipt stores when a test supplied ContentHash: pgtest.Fingerprint(label).
func FingerprintBytes(label string) []byte {
	return SHA256([]byte(label))
}

// ContentSHA256 returns the bytea receipts.content_sha256 holds for one request
// content: the receipt fingerprint without its sha256: prefix.
func ContentSHA256(t testing.TB, content receipt.Content) []byte {
	t.Helper()
	fingerprint, err := receipt.Fingerprint(content)
	if err != nil {
		t.Fatalf("receipt fingerprint: %v", err)
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(fingerprint, "sha256:"))
	if err != nil {
		t.Fatalf("receipt fingerprint %q is not hex: %v", fingerprint, err)
	}
	return decoded
}
