package serve

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/shell/bus"
)

func TestConnectBusWithoutNATSDeliversInProcessAndSaysSo(t *testing.T) {
	var logs bytes.Buffer
	b, closeBus, err := connectBus(t.Context(), Config{}, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer closeBus()
	if _, ok := b.(*bus.Memory); !ok {
		t.Fatalf("bus = %T, want the in-process bus", b)
	}
	if n := strings.Count(logs.String(), "delivered in process"); n != 1 {
		t.Fatalf("the in-process delivery was logged %d times:\n%s", n, logs.String())
	}
}

func TestConnectBusFailsOnAnUnusableNATSURL(t *testing.T) {
	for name, url := range map[string]string{
		"nothing listens": "nats://127.0.0.1:1",
		"wrong scheme":    "http://127.0.0.1:4222",
		"no host":         "nats://",
	} {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		b, closeBus, err := connectBus(ctx, Config{NATSURL: url}, slog.New(slog.DiscardHandler))
		cancel()
		if err == nil {
			closeBus()
			t.Errorf("%s: connected, got %T", name, b)
		}
	}
	if err := ValidateNATSURL(""); err != nil {
		t.Fatal(err)
	}
}
