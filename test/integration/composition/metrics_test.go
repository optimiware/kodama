//go:build integration

package composition

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// unavailableRegistry defers every image resolution. It keeps the push
// processor's first selection durable while admission remains transient, which
// lets the test remove the pipeline and exercise the real orphan-refusal path.
type unavailableRegistry struct {
	mu    sync.Mutex
	calls int
}

func (registry *unavailableRegistry) Resolve(context.Context, pipeline.ResolveRequest) (pipeline.ResolvedImage, error) {
	registry.mu.Lock()
	registry.calls++
	registry.mu.Unlock()
	return pipeline.ResolvedImage{}, fault.New(fault.DependencyUnavailable, "registry is unavailable")
}

// TestOperationsMetricsExposeBacklogLagCleanupAndRefusals proves the operations
// listener's /metrics route against the real composed app and listener. It
// covers the five S10 §3 counters: publication backlog, consumer lag,
// pending cleanup, refused pushes and refused reports, plus dependency errors.
//
// The publication backlog and dependency counter are exercised by taking the
// broker down; the trigger and kernel continue from durable state. A refused
// report is one resource.report attempt the reporter permanently refuses. A
// refused push is one accepted git.ref.updated report whose first selection is
// durable while image admission stays transient; deleting the definition then
// makes the trigger record a durable orphan pipeline.start_refused through the
// same Start seam the metrics decorator counts.
func TestOperationsMetricsExposeBacklogLagCleanupAndRefusals(t *testing.T) {
	harness := newHarness(t)
	// Image resolution stays transient for the whole scenario: the push
	// processor persists its first selection and defers admission, so the test
	// can remove the definition and observe the real orphan-refusal path.
	harness.config.Adapters.Registry = &unavailableRegistry{}
	environment := harness.start(newFakeRuntime(modeComplete))
	environment.grantEngineer("admin-1", projectID)

	body := environment.scrapeMetrics()
	for _, line := range []string{
		"# TYPE kodama_publication_backlog gauge",
		"# TYPE kodama_consumer_lag gauge",
		"# TYPE kodama_pending_cleanup gauge",
		"# TYPE kodama_refused_pushes_total counter",
		"# TYPE kodama_refused_reports_total counter",
		"# TYPE kodama_dependency_errors_total counter",
	} {
		if !strings.Contains(body, line) {
			t.Fatalf("metrics body has no %q:\n%s", line, body)
		}
	}
	if value := metricValue(t, body, "kodama_pending_cleanup"); value != 0 {
		t.Fatalf("pending cleanup before any execution = %v; want 0", value)
	}

	// A report against a resource with no binding is a permanent refusal.
	if _, publicError := environment.call(serviceToken, "resource.report", api.ReportRequest{
		ResourceID:     "repo-metrics-missing",
		SourceEventID:  "source-metrics-missing",
		EventType:      "git.ref.updated",
		Operation:      "git.ref.update",
		VerificationID: "verification-metrics-missing",
	}, nil, "metrics-refused-report"); publicError.Code == "" {
		t.Fatal("resource.report for an unbound resource was accepted")
	}

	// One accepted push that becomes an orphan selection is a durable refusal.
	const (
		repository = "repo-metrics-1"
		bindingID  = "binding-metrics-1"
		starter    = "metrics-starter"
		ref        = "refs/heads/metrics"
	)
	environment.mustInvoke(projectToken, "service.create", api.ServiceCreateRequest{
		ProjectID: projectID,
		ServiceID: starter,
		Roles:     []string{"engineer"},
	}, nil, "metrics-create-starter")
	environment.mustInvoke(projectToken, "service.create", api.ServiceCreateRequest{
		ProjectID: projectID,
		ServiceID: "service-1",
		Roles:     []string{"operator"},
	}, nil, "metrics-create-reporter")

	environment.mustInvoke(projectToken, "resource.binding.register", api.ResourceBindingRegisterRequest{
		ProjectID:        projectID,
		BindingID:        bindingID,
		ResourceID:       repository,
		EnforcingService: "service-1",
		TrustedOrigins:   []string{"https://git.example.com"},
		Operations:       []string{"git.ref.update"},
		ReportableEvents: []string{"git.ref.updated"},
	}, nil, "metrics-binding-register")

	document := fmt.Sprintf(`repository: %s
service: %s
on:
  push: [%s]
actions:
  - name: check
    image: registry.example.com/kodama/base@sha256:cae662172fd450bb0cd710a769079c05bfc5d8e35efa6576edc7d0377afdd4a2
    commands:
      - echo "metrics push refusal"
`, repository, starter, ref)
	applied := environment.mustInvoke(projectToken, "pipeline.apply", api.PipelineApplyRequest{
		ProjectID: projectID,
		Name:      "metrics-refused-push",
		YAML:      []byte(document),
	}, nil, "metrics-pipeline-apply")
	var appliedPipeline api.PipelineApplyResult
	environment.decode(applied, &appliedPipeline)

	subject := api.Subject{Fields: []api.SubjectField{
		{Name: "operation", Kind: "string", Value: "git.ref.update"},
		{Name: "resource", Kind: "string", Value: repository},
		{Name: "ref", Kind: "string", Value: ref},
		{Name: "expected_base", Kind: "string", Value: "base-metrics"},
		{Name: "candidate", Kind: "string", Value: "candidate-metrics"},
	}}
	verified := environment.mustInvoke(serviceToken, "actor.verify", api.VerifyRequest{
		ActorCredential: projectToken,
		ResourceID:      repository,
		Operation:       "git.ref.update",
		Subject:         subject,
	}, nil, "metrics-verify")
	var verification api.VerifyResponse
	environment.decode(verified, &verification)

	source := environment.mustInvoke(projectToken, "event.publish", api.EventPublishRequest{
		ProjectID: projectID,
		Type:      "observation.custom",
		SubjectID: repository,
		Data:      []byte(`{"ref":"refs/heads/metrics"}`),
	}, nil, "metrics-source-event")
	var published api.EventPublishResult
	environment.decode(source, &published)

	environment.mustInvoke(serviceToken, "resource.report", api.ReportRequest{
		ResourceID:     repository,
		SourceEventID:  published.EventID,
		EventType:      "git.ref.updated",
		Operation:      "git.ref.update",
		VerificationID: verification.VerificationID,
		Subject:        subject,
	}, nil, "metrics-push-report")

	// Wait for the trigger's first selection to be durable (the receipt row is
	// inserted before admission), then remove the definition. The next pass sees
	// a persisted selection with no current candidate and permanently refuses
	// the push as an orphan.
	environment.eventually(10*time.Second, func() bool {
		page, err := harness.settlement.Query(context.Background(),
			tenancy.Scope{TenantID: tenantID, ProjectID: projectID},
			store.ScanQuery{
				Table:  "receipts",
				Equals: map[string]any{"operation": "pipeline.start"},
				Limit:  1,
			})
		return err == nil && len(page.Rows) > 0
	})
	environment.mustInvoke(projectToken, "pipeline.delete", api.PipelineDeleteRequest{
		ProjectID:  projectID,
		PipelineID: appliedPipeline.PipelineID,
	}, &appliedPipeline.Version, "metrics-pipeline-delete")

	environment.eventually(10*time.Second, func() bool {
		body := environment.scrapeMetrics()
		return metricValue(t, body, "kodama_refused_pushes_total") >= 1 &&
			metricValue(t, body, "kodama_refused_reports_total") >= 1
	})

	// With the broker down, a committed mutation leaves a pending publication
	// and the dispatch retry records the dependency error.
	harness.publisher.setDown(true)
	environment.applyPipeline("metrics-backlog")
	environment.eventually(10*time.Second, func() bool {
		return metricValue(t, environment.scrapeMetrics(), "kodama_publication_backlog") > 0
	})
	environment.eventually(10*time.Second, func() bool {
		return metricValue(t, environment.scrapeMetrics(), "kodama_dependency_errors_total") > 0
	})

	body = environment.scrapeMetrics()
	for _, name := range []string{
		"kodama_publication_backlog",
		"kodama_consumer_lag",
		"kodama_pending_cleanup",
		"kodama_refused_pushes_total",
		"kodama_refused_reports_total",
		"kodama_dependency_errors_total",
	} {
		metricValue(t, body, name)
	}
	for _, secret := range []string{adminToken, projectToken, serviceToken, operatorToken, secretValue} {
		if strings.Contains(body, secret) {
			t.Fatalf("metrics body leaked fixture credential %q:\n%s", secret, body)
		}
	}
}

// scrapeMetrics returns the exact operations metrics exposition from the real
// listener.
func (environment *runningEnv) scrapeMetrics() string {
	environment.harness.t.Helper()
	response, err := http.Get(environment.baseURL + "/metrics")
	if err != nil {
		environment.harness.t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		environment.harness.t.Fatalf("read /metrics: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		environment.harness.t.Fatalf("GET /metrics = %d: %s", response.StatusCode, body)
	}
	return string(body)
}

// metricValue reads one unlabelled metric sample and fails when it is absent or
// not a number.
func metricValue(t *testing.T, body, name string) float64 {
	t.Helper()
	for line := range strings.SplitSeq(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != name {
			continue
		}
		value, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			t.Fatalf("metric %s value %q: %v", name, fields[1], err)
		}
		return value
	}
	t.Fatalf("metric %s is missing:\n%s", name, body)
	return 0
}
