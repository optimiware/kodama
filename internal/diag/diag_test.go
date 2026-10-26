package diag

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

type safe string

func (s safe) Error() string          { return "RAW:" + string(s) }
func (s safe) SafeDiagnostic() string { return string(s) }

func TestDescribeRendersOnlyOptedInFragmentsOutermostFirst(t *testing.T) {
	inner := safe("reading the Job (HTTP 503)")
	opaque := fmt.Errorf("pgx: password=hunter2 host=10.0.0.1: %w", inner)
	outer := fmt.Errorf("execution: %w: dispatch: %w", errors.New("secret-token"), safe("preflight refused"))
	chain := fmt.Errorf("%w / %w", outer, opaque)
	got := Describe(chain)
	if got != "preflight refused <- reading the Job (HTTP 503)" {
		t.Fatalf("got %q", got)
	}
	for _, leak := range []string{"hunter2", "10.0.0.1", "secret-token", "RAW"} {
		if strings.Contains(got, leak) {
			t.Fatalf("%q leaked into %q", leak, got)
		}
	}
}

func TestDescribeIsEmptyWithoutAnOptIn(t *testing.T) {
	if got := Describe(fmt.Errorf("a: %w", errors.New("b"))); got != "" {
		t.Fatalf("got %q", got)
	}
	if Describe(nil) != "" {
		t.Fatal("nil error")
	}
}

func TestDescribeBoundsLengthAndStripsControlCharacters(t *testing.T) {
	long := safe(strings.Repeat("x", 1000) + "\n\x1b[31m")
	got := Describe(long)
	if len(got) > maxBytes || strings.ContainsAny(got, "\n\x1b") {
		t.Fatalf("len=%d %q", len(got), got[:20])
	}
	if strings.Contains(Describe(safe("a\nb")), "\n") || Describe(safe("a\nb")) != "a?b" {
		t.Fatal("control characters were not replaced")
	}
}
