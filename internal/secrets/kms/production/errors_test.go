package production

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/pkg/secretkeys"
)

// Companion of kms_outage: provider failures map to the consumer's classes,
// only unavailability is retryable, and no provider text survives.
func Test_kms_outage_classification(t *testing.T) {
	const leakText = "PROVIDER-DIAGNOSTIC-LEAK token=abc"
	tests := []struct {
		name      string
		err       error
		class     secrets.KeyErrorClass
		retryable bool
	}{
		{"unavailable", secretkeys.ErrUnavailable, secrets.KeyUnavailable, true},
		{"wrapped unavailable", fmt.Errorf("%s: %w", leakText, secretkeys.ErrUnavailable), secrets.KeyUnavailable, true},
		{"deadline", context.DeadlineExceeded, secrets.KeyUnavailable, true},
		{"denied", secretkeys.ErrDenied, secrets.KeyDenied, false},
		{"not found", secretkeys.ErrNotFound, secrets.KeyNotFound, false},
		{"ciphertext", secretkeys.ErrCiphertext, secrets.KeyCorrupt, false},
		{"invalid", secretkeys.ErrInvalid, secrets.KeyCorrupt, false},
		{"unknown", errors.New(leakText), secrets.KeyUnavailable, true},
	}
	for _, tc := range tests {
		for _, op := range []string{"wrap", "unwrap"} {
			f := newFake(AWS, secretkeys.FormatAWS, awsKeyB, awsKeyA)
			p := bridgeWith(t, awsConfig(), f)
			good, err := p.Wrap(context.Background(), wctx(), scopeKey())
			if err != nil {
				t.Fatal(err)
			}
			f.fail(tc.err)
			var out error
			if op == "wrap" {
				var w secrets.WrappedKey
				w, out = p.Wrap(context.Background(), wctx(), scopeKey())
				if w.Bytes != nil {
					t.Fatalf("%s/%s: wrap output on failure", tc.name, op)
				}
			} else {
				var key []byte
				key, out = p.Unwrap(context.Background(), wctx(), good)
				if key != nil {
					t.Fatalf("%s/%s: plaintext on failure", tc.name, op)
				}
			}
			var ke *secrets.KeyError
			if !errors.As(out, &ke) || ke.Class != tc.class || ke.Retryable() != tc.retryable || ke.Op != op {
				t.Fatalf("%s/%s: %v", tc.name, op, out)
			}
			if strings.Contains(out.Error(), "LEAK") || strings.Contains(out.Error(), "token") {
				t.Fatalf("%s/%s leaked provider text: %v", tc.name, op, out)
			}
		}
	}
	// A cancelled caller is unavailable even if the provider reports denial.
	f := newFake(AWS, secretkeys.FormatAWS, awsKeyB)
	p := bridgeWith(t, awsConfig(), f)
	ctx, cancel := context.WithCancel(context.Background())
	f.fail(secretkeys.ErrDenied)
	cancel()
	if _, err := p.Wrap(ctx, wctx(), scopeKey()); keyClass(t, err) != secrets.KeyUnavailable {
		t.Fatalf("cancelled: %v", err)
	}
}
