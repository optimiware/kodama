//go:build integration

package composition

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/client"
)

// TestClientCredentialExpiryUsesTheClientWallClock pins the clock boundary this
// suite must not blur. Server-side development tokens are configured on the
// deterministic fixture clock because the composed authenticator evaluates them
// with the injected clock; the client-side credential handed to
// client.HTTPTransport is different. HTTPTransport.Bearer evaluates expiry
// against the transport clock, so the credential expiry must be derived from
// the same clock. Deriving it from the fixture clock made every public-client
// test fail once real time passed the fixture date.
//
// The table injects a fixed client clock in the past and far in the future, so
// the invariant is proven independently of the real date: a fixture-derived
// expiry would be refused at the future instant no matter when the suite runs.
func TestClientCredentialExpiryUsesTheClientWallClock(t *testing.T) {
	harness := newHarness(t)
	environment := &runningEnv{harness: harness}

	instants := []struct {
		name string
		now  time.Time
	}{
		{"real wall clock", time.Now()},
		{"far past", time.Date(1999, 1, 2, 3, 4, 5, 0, time.UTC)},
		{"far future", time.Date(2035, 6, 7, 8, 9, 10, 0, time.UTC)},
	}
	for _, instant := range instants {
		t.Run(instant.name, func(t *testing.T) {
			now := func() time.Time { return instant.now }
			transport := environment.transportWithClock(projectToken, now)

			credential, err := transport.Source.Credential(context.Background())
			if err != nil {
				t.Fatalf("read the client credential: %v", err)
			}
			if want := instant.now.Add(24 * time.Hour); !credential.ExpiresAt.Equal(want) {
				t.Fatalf("client credential expiry = %s; want one day after the client clock %s",
					credential.ExpiresAt.UTC(), want.UTC())
			}
			if _, err := client.Bearer(context.Background(), transport.Source, now()); err != nil {
				t.Fatalf("the client refused its own credential at its clock %s: %v",
					instant.now.UTC(), err)
			}
			// Expiry is exclusive: at the credential's own expiry the client must
			// refuse it, whatever the fixture clock says.
			if _, err := client.Bearer(context.Background(), transport.Source, credential.ExpiresAt); err == nil {
				t.Fatal("the client accepted its credential at the credential expiry")
			}
		})
	}
}
