package kit

import (
	"os/exec"
	"testing"
)

func TestShQuoteRoundTrips(t *testing.T) {
	for _, in := range []string{"plain", "with space", "it's", `a"b$c`, "x'; echo pwned; '"} {
		out, err := exec.Command("sh", "-c", "printf %s "+shQuote(in)).Output()
		if err != nil || string(out) != in {
			t.Errorf("shQuote(%q) round-trips to %q (%v)", in, out, err)
		}
	}
}
