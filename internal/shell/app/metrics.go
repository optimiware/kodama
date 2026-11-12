package app

// Operations metrics for the health/operations listener (S10 §3).
//
// The exposition is deliberately small and bounded: every gauge samples at
// most metricsSampleLimit rows through an existing store or consumer seam, and
// every counter is an in-memory atomic incremented by composition decorators
// around the real push start and verified report ports. No label carries a
// tenant, project, principal, resource or secret value, so one scrape cannot
// identify a tenant or leak credential material.

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/trigger"
	"github.com/optimiware/kodama/internal/shell/verify"
)

// metricsSampleLimit bounds one gauge sample. It matches the store's maximum
// page size, so a scrape can never ask for an unbounded scan; a larger backlog
// is reported as the sample cap.
const metricsSampleLimit = store.MaxQueryLimit

// operationsMetrics owns the process-local refusal and dependency counters. The
// gauges are sampled from durable state on every scrape instead, so a restart
// does not reset the backlog or lag they report.
type operationsMetrics struct {
	refusedPushes   atomic.Uint64
	refusedReports  atomic.Uint64
	dependencyError atomic.Uint64
}

func newOperationsMetrics() *operationsMetrics {
	return &operationsMetrics{}
}

func (metrics *operationsMetrics) countRefusedPush() {
	if metrics != nil {
		metrics.refusedPushes.Add(1)
	}
}

func (metrics *operationsMetrics) countRefusedReport() {
	if metrics != nil {
		metrics.refusedReports.Add(1)
	}
}

func (metrics *operationsMetrics) countDependencyError() {
	if metrics != nil {
		metrics.dependencyError.Add(1)
	}
}

// metricsSource samples the durable gauges and renders one exposition.
type metricsSource struct {
	outbox     event.Outbox
	events     trigger.EventReader
	controller store.Controller
	counters   *operationsMetrics
}

func newMetricsSource(outbox event.Outbox, events trigger.EventReader, controller store.Controller, counters *operationsMetrics) *metricsSource {
	return &metricsSource{outbox: outbox, events: events, controller: controller, counters: counters}
}

// collect renders the complete, fixed metric set. A dependency failure returns
// the error so the route answers the fixed 503 body; it is never rendered.
func (source *metricsSource) collect(ctx context.Context) ([]byte, error) {
	if source == nil || source.counters == nil {
		return nil, fault.New(fault.InternalFailure, "operations metrics are not configured")
	}
	publication, err := source.publicationBacklog(ctx)
	if err != nil {
		source.counters.countDependencyError()
		return nil, err
	}
	lag, err := source.consumerLag(ctx)
	if err != nil {
		source.counters.countDependencyError()
		return nil, err
	}
	cleanup, err := source.pendingCleanup(ctx)
	if err != nil {
		source.counters.countDependencyError()
		return nil, err
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "# HELP kodama_publication_backlog Committed events not yet acknowledged by the broker; sampled up to %d.\n", metricsSampleLimit)
	builder.WriteString("# TYPE kodama_publication_backlog gauge\n")
	fmt.Fprintf(&builder, "kodama_publication_backlog %d\n", publication)
	builder.WriteString("# HELP kodama_consumer_lag Committed events the push consumer has not completed; sampled up to the same bound.\n")
	builder.WriteString("# TYPE kodama_consumer_lag gauge\n")
	fmt.Fprintf(&builder, "kodama_consumer_lag %d\n", lag)
	builder.WriteString("# HELP kodama_pending_cleanup Executions whose declared cleanup is still owed; sampled up to the same bound.\n")
	builder.WriteString("# TYPE kodama_pending_cleanup gauge\n")
	fmt.Fprintf(&builder, "kodama_pending_cleanup %d\n", cleanup)
	builder.WriteString("# HELP kodama_refused_pushes_total Permanent push starts refused by the trigger processor.\n")
	builder.WriteString("# TYPE kodama_refused_pushes_total counter\n")
	fmt.Fprintf(&builder, "kodama_refused_pushes_total %d\n", source.counters.refusedPushes.Load())
	builder.WriteString("# HELP kodama_refused_reports_total Verified resource reports permanently refused.\n")
	builder.WriteString("# TYPE kodama_refused_reports_total counter\n")
	fmt.Fprintf(&builder, "kodama_refused_reports_total %d\n", source.counters.refusedReports.Load())
	builder.WriteString("# HELP kodama_dependency_errors_total Dependency failures observed by readiness and publication.\n")
	builder.WriteString("# TYPE kodama_dependency_errors_total counter\n")
	fmt.Fprintf(&builder, "kodama_dependency_errors_total %d\n", source.counters.dependencyError.Load())
	return []byte(builder.String()), nil
}

// publicationBacklog counts committed unacknowledged event rows. The outbox
// seam is the same one the dispatcher drains, so an outage backlog is visible
// without touching the broker.
func (source *metricsSource) publicationBacklog(ctx context.Context) (int, error) {
	if source.outbox == nil {
		return 0, nil
	}
	rows, err := source.outbox.Pending(ctx, 0, metricsSampleLimit)
	if err != nil {
		return 0, err
	}
	return len(rows), nil
}

// consumerLag counts committed events the push consumer has not completed. The
// trigger reader already excludes rows with a completion marker.
func (source *metricsSource) consumerLag(ctx context.Context) (int, error) {
	if source.events == nil {
		return 0, nil
	}
	rows, err := source.events.Pending(ctx, 0, metricsSampleLimit)
	if err != nil {
		return 0, err
	}
	return len(rows), nil
}

// pendingCleanup counts cleanup rows still owed. cleanup is in the controller's
// cross-scope read authority, so no tenant-facing scan is added.
func (source *metricsSource) pendingCleanup(ctx context.Context) (int, error) {
	if source.controller == nil {
		return 0, nil
	}
	page, err := source.controller.Scan(ctx, store.ControllerScan{
		Table:  "cleanup",
		Equals: map[string]any{"state": string(cleanupStatePending)},
		Limit:  metricsSampleLimit})
	if err != nil {
		return 0, err
	}
	return len(page.Rows), nil
}

// cleanupStatePending mirrors the cleanup-state vocabulary without
// importing the kernel state machine into the metrics surface.
const cleanupStatePending = "pending"

// metricsPublisher counts a dependency-class publication failure. It wraps the
// dispatcher's publisher, so a broker outage increments the counter while the
// durable outbox keeps the original identities.
type metricsPublisher struct {
	inner   event.Publisher
	metrics *operationsMetrics
}

var _ event.Publisher = (*metricsPublisher)(nil)

func (publisher *metricsPublisher) Publish(ctx context.Context, envelope event.Envelope) error {
	err := publisher.inner.Publish(ctx, envelope)
	if isDependencyError(err) {
		publisher.metrics.countDependencyError()
	}
	return err
}

// metricsStart counts every durable push refusal the trigger records through
// the shared start port.
type metricsStart struct {
	inner   trigger.StartPort
	metrics *operationsMetrics
}

var _ trigger.StartPort = (*metricsStart)(nil)

func (start *metricsStart) Start(ctx context.Context, unit trigger.StartUnit) (trigger.Result, error) {
	result, err := start.inner.Start(ctx, unit)
	if err == nil && result.Status == trigger.StatusRefused && !result.Replayed {
		start.metrics.countRefusedPush()
	}
	return result, err
}

// reportService is the verified-report capability surface the composition
// binds. It is the same method set as opsapp.Reports.
type reportService interface {
	Report(context.Context, verify.ReportCommand) (verify.ReportResult, error)
}

// metricsReports counts a permanent verified-report refusal.
type metricsReports struct {
	inner   reportService
	metrics *operationsMetrics
}

func (reports *metricsReports) Report(ctx context.Context, command verify.ReportCommand) (verify.ReportResult, error) {
	result, err := reports.inner.Report(ctx, command)
	if isRefusal(err) {
		reports.metrics.countRefusedReport()
	}
	return result, err
}

// isRefusal reports the permanent fault classes the capability surfaces use to
// refuse a command. A dependency outage is not counted as a refusal.
func isRefusal(err error) bool {
	class, ok := fault.ClassOf(err)
	if !ok {
		return false
	}
	switch class {
	case fault.Forbidden,
		fault.InvalidInput,
		fault.NotFound,
		fault.VersionConflict,
		fault.ChangedIdempotencyContent,
		fault.UnsupportedCapability:
		return true
	default:
		return false
	}
}

// isDependencyError reports whether an error is a dependency outage worth
// counting.
func isDependencyError(err error) bool {
	class, ok := fault.ClassOf(err)
	if !ok {
		return false
	}
	return class == fault.DependencyUnavailable || class == fault.DeadlineExpired
}
