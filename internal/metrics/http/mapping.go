package metricshttp

import (
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/metrics"
)

// Options is the validated option set of the list contribution
// (includeFlowSummary, flowFrom, flowTo). T-046 binds Binding() into the page
// cursor so a cursor cannot be replayed with a different window.
type Options struct {
	Include bool
	Window  metrics.Window
}

// ParseOptions validates the three query parameters. A nil include means false.
// The window is required exactly when summaries are requested; supplying it
// otherwise is rejected rather than ignored, so the cursor binding is
// unambiguous. Errors wrap metrics.ErrInvalid (a 400 invalid_request).
func ParseOptions(include *bool, from, to *time.Time, now time.Time) (Options, error) {
	if include == nil || !*include {
		if from != nil || to != nil {
			return Options{}, metrics.Invalidf("flowFrom and flowTo apply only with includeFlowSummary=true")
		}
		return Options{}, nil
	}
	if from == nil || to == nil {
		return Options{}, metrics.Invalidf("includeFlowSummary requires flowFrom and flowTo")
	}
	w, err := metrics.NewWindow(*from, *to, now)
	if err != nil {
		return Options{}, err
	}
	return Options{Include: true, Window: w}, nil
}

// Binding is a canonical, injective rendering of the options for cursor binding.
func (o Options) Binding() string {
	if !o.Include {
		return "flow=off"
	}
	return "flow=" + o.Window.From.Format(time.RFC3339Nano) + "/" + o.Window.To.Format(time.RFC3339Nano)
}

// FromSummary maps a read model to its wire representation.
func FromSummary(s metrics.WorkSummary) FlowSummary {
	out := FlowSummary{
		Status:          FlowSummaryStatus(s.Status),
		Window:          FlowWindow{From: s.Window.From.UTC(), To: s.Window.To.UTC()},
		ObservedVersion: int64(s.ObservedVersion),
		Durations: FlowDurations{
			LeadMicros: s.Lead.Microseconds(), ActiveMicros: s.Active.Microseconds(), ReviewMicros: s.Review.Microseconds(),
			BlockedMicros: s.Blocked.Microseconds(), ReopenedMicros: s.Reopened.Microseconds(),
			CompletedLeadMicros: s.CompletedLead.Microseconds(),
		},
		Counts: FlowCounts{
			Reopens: s.ReopenCount, BlockedIntervals: s.BlockedIntervals,
			CompletedEpisodes: s.CompletedEpisodes, CancelledEpisodes: s.CancelledEpisodes,
		},
		Censoring: FlowCensoring{InFlightAtStart: s.InFlightAtStart, OpenAtEnd: s.OpenAtEnd},
	}
	if s.Anomaly != "" {
		a := s.Anomaly
		out.Anomaly = &a
	}
	if s.StateAtEnd != "" {
		st := FlowState(s.StateAtEnd)
		out.Censoring.StateAtEnd = &st
	}
	return out
}

// Contributions maps a batch to the per-item member of the list page, keyed by
// work item. Items without an entry get no flowSummary member.
func Contributions(summaries []metrics.WorkSummary) map[domain.WorkItemID]FlowSummaryContribution {
	out := make(map[domain.WorkItemID]FlowSummaryContribution, len(summaries))
	for _, s := range summaries {
		w := FromSummary(s)
		out[s.ItemID] = FlowSummaryContribution{FlowSummary: &w}
	}
	return out
}
