package execution

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// A timed-out attempt used to leave only its terminal reason. The log now carries the timing evidence that
// separates "never dispatched or observed" from "ran too long", and nothing of the step's content.
func TestDeadlineMissLogCarriesTimingEvidenceOnly(t *testing.T) {
	var out bytes.Buffer
	s := &Service{o: Options{Logger: slog.New(slog.NewTextHandler(&out, nil))}}
	now := time.Date(2026, 10, 26, 8, 0, 0, 0, time.UTC)
	at := &Attempt{ID: "att_x", StepID: "build", Reason: ReasonStartupTimeout, StartupDeadline: now.Add(-time.Second)}
	s.logDeadlineMiss(context.Background(), newDeadlineMiss(at, &Control{}, now))
	line := out.String()
	for _, want := range []string{"attempt deadline missed", "attempt=att_x", "reason=startup_timeout", "dispatchConfirmed=false", "workloadBound=false", "started=false", "startupDeadline=2026-10-26T07:59:59.000Z"} {
		if !strings.Contains(line, want) {
			t.Fatalf("%q lacks %q", line, want)
		}
	}
}
