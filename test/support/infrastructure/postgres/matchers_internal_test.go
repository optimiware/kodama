package pgtest

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/shell/receipt"
)

func TestGoldenNullMismatchIsTyped(t *testing.T) {
	err := goldenCompare(goldenNull{}, goldenText(""))
	if !errors.Is(err, errGoldenNullMismatch) {
		t.Fatalf("NULL vs empty text error = %v; want errGoldenNullMismatch", err)
	}
	err = goldenCompare(goldenText(""), goldenNull{})
	if !errors.Is(err, errGoldenNullMismatch) {
		t.Fatalf("empty text vs NULL error = %v; want errGoldenNullMismatch", err)
	}
	if err := goldenCompare(goldenText("a"), goldenText("b")); err == nil || errors.Is(err, errGoldenNullMismatch) {
		t.Fatalf("plain mismatch error = %v; want a non-NULL mismatch", err)
	}
}

func TestGoldenDBSequence(t *testing.T) {
	matcher := GoldenDBSequence()
	if err := goldenCompare(goldenInt(1), matcher); err != nil {
		t.Fatalf("positive sequence rejected: %v", err)
	}
	for _, bad := range []any{goldenInt(0), goldenInt(-3), goldenText("1"), goldenNull{}} {
		if err := goldenCompare(bad, matcher); err == nil {
			t.Fatalf("value %#v accepted as a sequence", bad)
		}
	}
}

func TestGoldenGeneratedID(t *testing.T) {
	matcher := GoldenGeneratedID("membership-", "minted by the identity adapter's secure randomness")
	if err := goldenCompare(goldenText("membership-ab12"), matcher); err != nil {
		t.Fatalf("matching id rejected: %v", err)
	}
	for _, bad := range []any{goldenText("grant-ab12"), goldenText(""), goldenInt(7), goldenNull{}} {
		if err := goldenCompare(bad, matcher); err == nil {
			t.Fatalf("value %#v accepted as a generated id", bad)
		}
	}
	// An empty prefix still refuses an empty value.
	if err := goldenCompare(goldenText(""), GoldenGeneratedID("", "reason")); err == nil {
		t.Fatal("empty text accepted by an empty-prefix generated id")
	}
	if err := goldenCompare(goldenText("x"), GoldenGeneratedID("", "")); err == nil {
		t.Fatal("generated id without a reason accepted")
	}
}

func TestGoldenOneOf(t *testing.T) {
	matcher := GoldenOneOf("alpha", "beta", int64(7))
	for _, good := range []any{goldenText("alpha"), goldenText("beta"), goldenInt(7)} {
		if err := goldenCompare(good, matcher); err != nil {
			t.Fatalf("candidate %#v rejected: %v", good, err)
		}
	}
	for _, bad := range []any{goldenText("gamma"), goldenInt(8), goldenNull{}} {
		if err := goldenCompare(bad, matcher); err == nil {
			t.Fatalf("value %#v accepted by one-of", bad)
		}
	}
	if err := goldenCompare(goldenText("x"), GoldenOneOf()); err == nil {
		t.Fatal("empty one-of accepted a value")
	}
	if err := goldenCompare(goldenText("x"), GoldenOneOf(GoldenDBSequence())); err == nil {
		t.Fatal("one-of accepted a loose candidate")
	}
}

func TestGoldenRandomBytes(t *testing.T) {
	matcher := GoldenRandomBytes(4)
	if err := goldenCompare(goldenBytes("0a0b0c0d"), matcher); err != nil {
		t.Fatalf("random bytes rejected: %v", err)
	}
	if err := goldenCompare(goldenBytes("01000000"), matcher); err != nil {
		t.Fatalf("one non-zero byte rejected: %v", err)
	}
	for name, bad := range map[string]any{
		"all zero":  goldenBytes("00000000"),
		"too short": goldenBytes("0a0b0c"),
		"too long":  goldenBytes("0a0b0c0d0e"),
		"not hex":   goldenBytes("zz0b0c0d"),
		"text":      goldenText("0a0b0c0d"),
		"NULL":      goldenNull{},
	} {
		if err := goldenCompare(bad, matcher); err == nil {
			t.Fatalf("%s accepted by random bytes", name)
		}
	}
}

func TestClockWindowBindsOnceAndMatchesOffsetsExactly(t *testing.T) {
	low := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	high := low.Add(time.Hour)
	reading := low.Add(10 * time.Minute)
	window := NewClockWindow(low, high)
	if err := goldenCompare(goldenTime(reading), window.At(0)); err != nil {
		t.Fatalf("first column rejected: %v", err)
	}
	if got, ok := window.Reading(); !ok || !got.Equal(reading) {
		t.Fatalf("bound reading = %s,%v; want %s", got, ok, reading)
	}
	if err := goldenCompare(goldenTime(reading), window.At(0)); err != nil {
		t.Fatalf("same reading rejected: %v", err)
	}
	if err := goldenCompare(goldenTime(reading.Add(5*time.Minute)), window.At(5*time.Minute)); err != nil {
		t.Fatalf("exact offset rejected: %v", err)
	}
	for name, bad := range map[string]time.Time{
		"+1ns":        reading.Add(time.Nanosecond),
		"-1ns":        reading.Add(-time.Nanosecond),
		"other clock": reading.Add(time.Minute),
	} {
		if err := goldenCompare(goldenTime(bad), window.At(0)); err == nil {
			t.Fatalf("%s accepted at offset 0", name)
		}
	}
	if err := goldenCompare(goldenTime(reading.Add(5*time.Minute+time.Nanosecond)), window.At(5*time.Minute)); err == nil {
		t.Fatal("offset +1ns accepted")
	}
	if err := goldenCompare(goldenText("x"), window.At(0)); err == nil {
		t.Fatal("non-timestamp accepted")
	}
}

func TestClockWindowRejectsAReadingOutsideTheWindowAndDoesNotBind(t *testing.T) {
	low := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	high := low.Add(time.Hour)
	window := NewClockWindow(low, high)
	if err := goldenCompare(goldenTime(low.Add(-time.Nanosecond)), window.At(0)); err == nil {
		t.Fatal("reading before the window accepted")
	}
	if err := goldenCompare(goldenTime(high.Add(time.Nanosecond)), window.At(0)); err == nil {
		t.Fatal("reading after the window accepted")
	}
	if _, ok := window.Reading(); ok {
		t.Fatal("a rejected reading bound the window")
	}
	// The offset participates in the window check: value-offset must be inside.
	if err := goldenCompare(goldenTime(high.Add(30*time.Minute)), window.At(time.Hour)); err != nil {
		t.Fatalf("value whose reading is inside rejected: %v", err)
	}
	if err := goldenCompare(goldenTime(low), window.At(0)); err == nil {
		t.Fatal("a second, different reading was accepted after binding")
	}
}

func TestGoldenJSONOfIsExactOnBothJSONBAndBytea(t *testing.T) {
	type result struct {
		Version uint64 `json:"version"`
		Name    string `json:"name"`
	}
	matcher := GoldenJSONOf(result{Version: 3, Name: "a"})
	if err := goldenCompare(normalizeGoldenValue([]byte(`{"name":"a","version":3}`), "jsonb"), matcher); err != nil {
		t.Fatalf("jsonb key order rejected: %v", err)
	}
	if err := goldenCompare(normalizeGoldenValue([]byte(`{"version":3,"name":"a"}`), "bytea"), matcher); err != nil {
		t.Fatalf("bytea document rejected: %v", err)
	}
	for name, actual := range map[string]any{
		"changed value":  normalizeGoldenValue([]byte(`{"name":"b","version":3}`), "jsonb"),
		"changed number": normalizeGoldenValue([]byte(`{"name":"a","version":4}`), "bytea"),
		"missing key":    normalizeGoldenValue([]byte(`{"version":3}`), "jsonb"),
		"extra key":      normalizeGoldenValue([]byte(`{"name":"a","version":3,"x":1}`), "jsonb"),
		"empty object":   normalizeGoldenValue([]byte(`{}`), "jsonb"),
		"not json":       normalizeGoldenValue([]byte(`nope`), "bytea"),
		"trailing":       normalizeGoldenValue([]byte(`{"name":"a","version":3}{}`), "bytea"),
		"NULL":           normalizeGoldenValue(nil, "jsonb"),
		"text":           goldenText(`{"name":"a","version":3}`),
	} {
		if err := goldenCompare(actual, matcher); err == nil {
			t.Fatalf("%s accepted by GoldenJSONOf", name)
		}
	}
	// A large integer must not lose precision through float64.
	big := GoldenJSONOf(map[string]any{"n": int64(9007199254740993)})
	if err := goldenCompare(normalizeGoldenValue([]byte(`{"n":9007199254740992}`), "jsonb"), big); err == nil {
		t.Fatal("a number differing beyond float64 precision was accepted")
	}
	if err := goldenCompare(normalizeGoldenValue([]byte(`{"n":9007199254740993}`), "jsonb"), big); err != nil {
		t.Fatalf("exact large integer rejected: %v", err)
	}
	if err := goldenCompare(normalizeGoldenValue([]byte(`{}`), "jsonb"), GoldenJSONOf(make(chan int))); err == nil {
		t.Fatal("unencodable expectation accepted a value")
	}
}

func TestGoldenJSONSatisfiesRunsTheCheckOnTheDocument(t *testing.T) {
	var seen []byte
	matcher := GoldenJSONSatisfies("document names a pipeline", func(raw []byte) error {
		seen = raw
		if !strings.Contains(string(raw), `"pipeline":"p1"`) {
			return errors.New("pipeline missing")
		}
		return nil
	})
	if err := goldenCompare(normalizeGoldenValue([]byte(`{"pipeline":"p1"}`), "jsonb"), matcher); err != nil {
		t.Fatalf("satisfying document rejected: %v", err)
	}
	if string(seen) != `{"pipeline":"p1"}` {
		t.Fatalf("check saw %q", seen)
	}
	if err := goldenCompare(normalizeGoldenValue([]byte(`{"pipeline":"p2"}`), "jsonb"), matcher); err == nil {
		t.Fatal("non-satisfying document accepted")
	}
	if err := goldenCompare(normalizeGoldenValue(nil, "jsonb"), matcher); err == nil {
		t.Fatal("NULL accepted")
	}
	if err := goldenCompare(normalizeGoldenValue([]byte(`{}`), "jsonb"), GoldenJSONSatisfies("", func([]byte) error { return nil })); err == nil {
		t.Fatal("matcher without a description accepted")
	}
	if err := goldenCompare(normalizeGoldenValue([]byte(`{}`), "jsonb"), GoldenJSONSatisfies("d", nil)); err == nil {
		t.Fatal("matcher without a check accepted")
	}
}

func TestMatcherAllowedRestrictsEveryLooseKindToItsColumns(t *testing.T) {
	window := NewClockWindow(time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	satisfies := GoldenJSONSatisfies("d", func([]byte) error { return nil })
	cases := []struct {
		name    string
		column  string
		colType string
		matcher GoldenMatcher
		allowed bool
	}{
		{"sequence on publish_sequence", "publish_sequence", "bigint", GoldenDBSequence(), true},
		{"sequence on subject_version", "subject_version", "bigint", GoldenDBSequence(), false},
		{"generated id on id", "id", "text", GoldenGeneratedID("", "r"), true},
		{"generated id on foreign id", "execution_id", "text", GoldenGeneratedID("", "r"), true},
		{"generated id on type", "type", "text", GoldenGeneratedID("", "r"), false},
		{"generated id on subject", "subject_id", "text", GoldenGeneratedID("", "r"), true},
		{"generated id on name", "name", "text", GoldenGeneratedID("", "r"), false},
		{"one-of anywhere", "state", "text", GoldenOneOf("a"), true},
		{"random bytes on nonce", "token_nonce", "bytea", GoldenRandomBytes(32), true},
		{"random bytes on content hash", "content_sha256", "bytea", GoldenRandomBytes(32), false},
		{"random bytes on yaml hash", "yaml_sha256", "bytea", GoldenRandomBytes(32), false},
		{"random bytes on a text nonce", "nonce", "text", GoldenRandomBytes(32), false},
		{"clock on timestamptz", "created_at", "timestamptz", window.At(0), true},
		{"clock on text", "state", "text", window.At(0), false},
		{"json satisfies on jsonb", "snapshot", "jsonb", satisfies, true},
		{"json satisfies on bytea", "result", "bytea", satisfies, true},
		{"json satisfies on text", "title", "text", satisfies, false},
		{"json of on jsonb", "failure", "jsonb", GoldenJSONOf(1), true},
		{"json of on bigint", "version", "bigint", GoldenJSONOf(1), false},
	}
	for _, testCase := range cases {
		err := matcherAllowed(testCase.column, testCase.colType, testCase.matcher)
		if testCase.allowed && err != nil {
			t.Errorf("%s: refused: %v", testCase.name, err)
		}
		if !testCase.allowed && err == nil {
			t.Errorf("%s: allowed", testCase.name)
		}
	}
}

func TestApplyGoldenRowRefusesALooseMatcherOnAForbiddenColumn(t *testing.T) {
	savedTables := goldenAsserted
	goldenAsserted = map[string]string{}
	defer func() { goldenAsserted = savedTables }()
	types := map[string]string{"content_sha256": "bytea"}
	actual := map[string]any{"content_sha256": normalizeGoldenValue([]byte{1, 2, 3, 4}, "bytea")}
	err := applyGoldenRow("receipts", actual, map[string]any{"content_sha256": GoldenRandomBytes(4)}, types)
	if err == nil || !strings.Contains(err.Error(), "random byte columns") {
		t.Fatalf("applyGoldenRow with random bytes on content_sha256 = %v; want an allowlist refusal", err)
	}
	if _, ok := goldenAsserted["receipts"]; ok {
		t.Fatal("a refused matcher registered the table")
	}
	if err := applyGoldenRow("receipts", actual, map[string]any{"content_sha256": []byte{1, 2, 3, 4}}, types); err != nil {
		t.Fatalf("exact bytes rejected: %v", err)
	}
}

func TestApplyGoldenRowReportsEveryDifferingColumn(t *testing.T) {
	types := map[string]string{"a": "text", "b": "bigint", "c": "text"}
	actual := map[string]any{
		"a": normalizeGoldenValue("x", "text"),
		"b": normalizeGoldenValue(int64(1), "bigint"),
		"c": normalizeGoldenValue("z", "text"),
	}
	err := applyGoldenRow("t", actual, map[string]any{"a": "y", "b": int64(2), "c": "z"}, types)
	if err == nil {
		t.Fatal("differing row passed")
	}
	for _, want := range []string{"2 column(s) differ", "a:", "b:"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "c:") {
		t.Fatalf("matching column c was reported: %q", err)
	}
}

func TestContentSHA256MatchesTheReceiptFingerprint(t *testing.T) {
	content := receipt.Content{MediaType: "application/json", Bytes: []byte(`{"request":"x"}`)}
	got := ContentSHA256(t, content)
	fingerprint, err := receipt.Fingerprint(content)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if want := strings.TrimPrefix(fingerprint, "sha256:"); hex.EncodeToString(got) != want {
		t.Fatalf("ContentSHA256 = %x; want %s", got, want)
	}
	// Independent vector: sha256(mediaType 0x00 normalized-body).
	normalized, err := receipt.Normalize(content)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	independent := SHA256(append(append([]byte("application/json"), 0), normalized...))
	if hex.EncodeToString(got) != hex.EncodeToString(independent) {
		t.Fatalf("ContentSHA256 = %x; independent digest %x", got, independent)
	}
	other := ContentSHA256(t, receipt.Content{MediaType: "application/json", Bytes: []byte(`{"request":"y"}`)})
	if hex.EncodeToString(other) == hex.EncodeToString(got) {
		t.Fatal("different content produced the same digest")
	}
	asBinary := ContentSHA256(t, receipt.Content{MediaType: "application/octet-stream", Bytes: []byte(`{"request":"x"}`)})
	if hex.EncodeToString(asBinary) == hex.EncodeToString(got) {
		t.Fatal("a different media type produced the same digest")
	}
}

func TestFingerprintBytesMatchesFingerprint(t *testing.T) {
	if got, want := hex.EncodeToString(FingerprintBytes("gap-a")), strings.TrimPrefix(Fingerprint("gap-a"), "sha256:"); got != want {
		t.Fatalf("FingerprintBytes = %s; want %s", got, want)
	}
	if len(FingerprintBytes("x")) != 32 {
		t.Fatal("digest is not 32 bytes")
	}
}
