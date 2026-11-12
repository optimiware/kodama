package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/trigger"
	"github.com/optimiware/kodama/internal/shell/verify"
)

type metricsOutbox struct {
	rows []event.PendingEvent
	err  error
}

func (outbox metricsOutbox) Pending(context.Context, event.Sequence, int) ([]event.PendingEvent, error) {
	return outbox.rows, outbox.err
}

func (metricsOutbox) MarkPublished(context.Context, id.EventID, event.Sequence) error { return nil }

type metricsEvents struct {
	rows []event.PendingEvent
	err  error
}

func (events metricsEvents) Pending(context.Context, event.Sequence, int) ([]event.PendingEvent, error) {
	return events.rows, events.err
}

type metricsController struct {
	rows []store.Row
	err  error
}

func (controller metricsController) Read(context.Context, string, string) (store.Row, error) {
	return store.Row{}, fault.New(fault.NotFound, "absent")
}

func (controller metricsController) Scan(context.Context, store.ControllerScan) (store.QueryPage, error) {
	return store.QueryPage{Rows: controller.rows}, controller.err
}

func (controller metricsController) Execute(context.Context, string, store.Unit) (store.UnitResult, error) {
	return store.UnitResult{}, nil
}

func TestMetricsCollectRendersEveryNamedSeries(t *testing.T) {
	counters := newOperationsMetrics()
	counters.countRefusedPush()
	counters.countRefusedPush()
	counters.countRefusedReport()
	counters.countDependencyError()
	source := newMetricsSource(
		metricsOutbox{rows: make([]event.PendingEvent, 2)},
		metricsEvents{rows: make([]event.PendingEvent, 3)},
		metricsController{rows: make([]store.Row, 4)},
		counters,
	)
	body, err := source.collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, want := range []string{
		"kodama_publication_backlog 2\n",
		"kodama_consumer_lag 3\n",
		"kodama_pending_cleanup 4\n",
		"kodama_refused_pushes_total 2\n",
		"kodama_refused_reports_total 1\n",
		"kodama_dependency_errors_total 1\n",
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("exposition has no %q:\n%s", want, body)
		}
	}
}

func TestMetricsCollectRefusesADependencyFailure(t *testing.T) {
	counters := newOperationsMetrics()
	source := newMetricsSource(
		metricsOutbox{err: fault.New(fault.DependencyUnavailable, "outbox unavailable")},
		metricsEvents{},
		metricsController{},
		counters,
	)
	if _, err := source.collect(context.Background()); err == nil {
		t.Fatal("collect accepted a failed outbox read")
	}
	if got := counters.dependencyError.Load(); got != 1 {
		t.Fatalf("dependency errors after a failed sample = %d; want 1", got)
	}
}

type metricsPublisherInner struct{ err error }

func (inner metricsPublisherInner) Publish(context.Context, event.Envelope) error { return inner.err }

func TestMetricsPublisherCountsOnlyDependencyFailures(t *testing.T) {
	counters := newOperationsMetrics()
	publisher := &metricsPublisher{inner: metricsPublisherInner{err: fault.New(fault.DependencyUnavailable, "broker down")}, metrics: counters}
	_ = publisher.Publish(context.Background(), event.Envelope{})
	if got := counters.dependencyError.Load(); got != 1 {
		t.Fatalf("dependency errors = %d; want 1", got)
	}
	publisher.inner = metricsPublisherInner{err: errors.New("plain error")}
	_ = publisher.Publish(context.Background(), event.Envelope{})
	if got := counters.dependencyError.Load(); got != 1 {
		t.Fatalf("dependency errors after an unclassified error = %d; want 1", got)
	}
}

type metricsStartInner struct {
	result trigger.Result
	err    error
}

func (inner metricsStartInner) Start(context.Context, trigger.StartUnit) (trigger.Result, error) {
	return inner.result, inner.err
}

func TestMetricsStartCountsOnlyDurableRefusals(t *testing.T) {
	counters := newOperationsMetrics()
	start := &metricsStart{inner: metricsStartInner{result: trigger.Result{Status: trigger.StatusRefused}}, metrics: counters}
	if _, err := start.Start(context.Background(), trigger.StartUnit{}); err != nil {
		t.Fatalf("start: %v", err)
	}
	if got := counters.refusedPushes.Load(); got != 1 {
		t.Fatalf("refused pushes = %d; want 1", got)
	}
	start.inner = metricsStartInner{result: trigger.Result{Status: trigger.StatusSelected}}
	_, _ = start.Start(context.Background(), trigger.StartUnit{})
	if got := counters.refusedPushes.Load(); got != 1 {
		t.Fatalf("refused pushes after a selected start = %d; want 1", got)
	}
}

type metricsReportInner struct {
	result verify.ReportResult
	err    error
}

func (inner metricsReportInner) Report(context.Context, verify.ReportCommand) (verify.ReportResult, error) {
	return inner.result, inner.err
}

func TestMetricsReportsCountOnlyPermanentRefusals(t *testing.T) {
	counters := newOperationsMetrics()
	reports := &metricsReports{inner: metricsReportInner{err: fault.New(fault.NotFound, "no binding")}, metrics: counters}
	if _, err := reports.Report(context.Background(), verify.ReportCommand{}); err == nil {
		t.Fatal("report accepted a not-found refusal")
	}
	if got := counters.refusedReports.Load(); got != 1 {
		t.Fatalf("refused reports = %d; want 1", got)
	}
	reports.inner = metricsReportInner{err: fault.New(fault.DependencyUnavailable, "store down")}
	_, _ = reports.Report(context.Background(), verify.ReportCommand{})
	if got := counters.refusedReports.Load(); got != 1 {
		t.Fatalf("refused reports after a dependency error = %d; want 1", got)
	}
}
