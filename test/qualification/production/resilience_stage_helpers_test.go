//go:build qualification

package production

// Live stage support for the Q02 resilience probes. Nothing here contacts a
// cluster unless a TestStageResilience* probe runs with every named input
// configured; the unit tests in resilience_unit_test.go exercise the same
// helpers against fake kubectl scripts and local httptest servers.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
)

// resilienceEvidenceRecord is one deferred evidence record of a probe.
type resilienceEvidenceRecord struct {
	Name     string
	Started  time.Time
	Finished time.Time
	Fields   map[string]any
}

// resilienceProbe owns one probe's evidence lifecycle. The evidence cleanup is
// registered first so a replica-restoration cleanup registered later runs
// before it and can record the restoration outcome.
type resilienceProbe struct {
	t           *testing.T
	config      Config
	name        string
	started     time.Time
	writer      *EvidenceWriter
	records     []resilienceEvidenceRecord
	restoration string
}

// resilienceBeginProbe starts one probe and registers its evidence write.
func resilienceBeginProbe(t *testing.T, config Config, name string) *resilienceProbe {
	t.Helper()
	probe := &resilienceProbe{
		t:           t,
		config:      config,
		name:        name,
		started:     time.Now().UTC(),
		restoration: "not_applicable",
	}
	if config.EvidenceDir != "" {
		writer, err := NewEvidenceWriter(config.EvidenceDir)
		if err != nil {
			t.Fatalf("the configured evidence directory is unusable: %v", err)
		}
		writer.Secrets = config.tokens()
		probe.writer = writer
	}
	t.Cleanup(probe.finish)
	return probe
}

// Record appends one evidence record. Names and fields are validated now so an
// invalid record fails the probe at its source, not silently at cleanup.
func (probe *resilienceProbe) Record(name string, started, finished time.Time, fields map[string]any) {
	probe.t.Helper()
	if !evidenceNamePattern.MatchString(name) {
		probe.t.Errorf("resilience evidence name %q is not a bounded lower-case file name", name)
		return
	}
	for key := range fields {
		if !evidenceFields[key] {
			probe.t.Errorf("resilience evidence field %q is not allowlisted", key)
			return
		}
		switch key {
		case "probe", "outcome", "restoration", "started_at", "finished_at", "duration_ms", "recorded_at":
			probe.t.Errorf("resilience evidence field %q is reserved", key)
			return
		}
	}
	probe.records = append(probe.records, resilienceEvidenceRecord{
		Name:     name,
		Started:  started,
		Finished: finished,
		Fields:   fields,
	})
}

// SetRestoration records the replica restoration outcome for the evidence.
func (probe *resilienceProbe) SetRestoration(result string) {
	if probe == nil {
		return
	}
	if !evidenceRestorations[result] {
		probe.t.Errorf("resilience restoration %q is not a known result", result)
		return
	}
	probe.restoration = result
}

// finish writes every collected record, or one summary record when the probe
// failed before it could record a phase. It never writes prose, logs or
// credential material.
func (probe *resilienceProbe) finish() {
	if probe == nil || probe.writer == nil {
		return
	}
	finished := time.Now().UTC()
	outcome := "passed"
	switch {
	case probe.t.Failed():
		outcome = "failed"
	case probe.t.Skipped():
		// A probe that skipped after it started (for example the named
		// resilience-cleanup-crash-vacuous reason) must not be recorded as a
		// pass: the evidence outcome has to match what the test reported.
		outcome = "skipped"
	}
	records := probe.records
	if len(records) == 0 {
		records = []resilienceEvidenceRecord{{Name: resilienceStageProbePrefix(probe.name)}}
	}
	for _, record := range records {
		started := record.Started
		if started.IsZero() {
			started = probe.started
		}
		recordFinished := record.Finished
		if recordFinished.IsZero() {
			recordFinished = finished
		}
		fields := map[string]any{}
		if probe.config.HarnessRevision != "" {
			fields["harness_revision"] = probe.config.HarnessRevision
		}
		if probe.config.DeployedRevision != "" {
			fields["deployed_revision"] = probe.config.DeployedRevision
		}
		if digest := probe.config.ImageDigest(); digest != "" {
			fields["image_digest"] = digest
		}
		maps.Copy(fields, record.Fields)
		evidence, err := resilienceEvidenceFields(resilienceEvidenceInput{
			Probe:       probe.name,
			Outcome:     outcome,
			Restoration: probe.restoration,
			Started:     started,
			Finished:    recordFinished,
			Fields:      fields,
		})
		if err != nil {
			probe.t.Errorf("build resilience evidence %q: %v", record.Name, err)
			continue
		}
		if _, err := probe.writer.Write(record.Name, evidence); err != nil {
			probe.t.Errorf("write resilience evidence %q: %v", record.Name, err)
		}
	}
}

// resilienceTesting is the subset of *testing.T the cleanup path needs. The
// fake test can therefore drive the real registration and cleanup code.
type resilienceTesting interface {
	Helper()
	Cleanup(func())
	Errorf(format string, args ...any)
}

// resilienceRestoreOnCleanup registers the safety-critical restoration. It runs
// on every path, including a probe that fails after scaling, and verifies the
// restored count with a read.
func resilienceRestoreOnCleanup(t resilienceTesting, probe *resilienceProbe, guard *resilienceReplicaGuard) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), resilienceRestoreTimeout)
		defer cancel()
		restoration, err := resilienceRestoreSafely(ctx, guard)
		if err != nil {
			probe.SetRestoration(restoration)
			t.Errorf("resilience probe did not restore its saved replica count: %v", err)
			return
		}
		probe.SetRestoration(restoration)
	})
}

// resilienceStage bundles the public, kubectl and configuration handles one
// live probe uses.
type resilienceStage struct {
	Config  Config
	API     *API
	Cluster resilienceCluster
}

// resilienceResilienceInputs is the input set every live probe needs: the
// public API, the guarded kubectl scope, the assigned tenant namespace and the
// digest-pinned workload image. Q02_TENANT_NAMESPACE is the DERIVED namespace
// kodama-tenant-<tenant> (stage: kodama-tenant-tenant-a), never the bare tenant
// id. The workload image needs a POSIX sh, printf, grep and sleep, and must run
// as root or have a writable /workspace.
func resilienceResilienceInputs() []Input {
	return []Input{
		InputAPIURL,
		InputCAPEM,
		InputTLSServerName,
		InputAdminToken,
		InputProjectID,
		InputTenantNamespace,
		InputKubeContext,
		InputEvidenceDir,
		InputImage,
	}
}

// resilienceConnect builds the TLS public client and the guarded kubectl
// helper.
func resilienceConnect(t *testing.T, config Config) *resilienceStage {
	t.Helper()
	client, err := NewAPI(config)
	if err != nil {
		t.Fatalf("build the stage API helper: %v", err)
	}
	guard, err := NewGuard(config.KubeContext, config.TenantNamespace)
	if err != nil {
		t.Fatalf("build the confinement guard: %v", err)
	}
	return &resilienceStage{
		Config: config,
		API:    client,
		Cluster: resilienceNewCluster(&Kubectl{
			Guard:   guard,
			Path:    "kubectl",
			Timeout: 3 * time.Minute,
		}),
	}
}

// resilienceKodamaService returns the release's public Service name. The stage
// runbook forwards svc/kodama; an operator that renamed it can export the same
// variable run-stage.sh reads.
func resilienceKodamaService() (string, error) {
	service := strings.TrimSpace(os.Getenv("Q02_KODAMA_SERVICE"))
	if service == "" {
		service = "kodama"
	}
	if !dnsLabelPattern.MatchString(service) {
		return "", fmt.Errorf("Q02_KODAMA_SERVICE %q is not a kubernetes service name", service)
	}
	return service, nil
}

// resilienceAPIPort extracts the loopback port of the public API URL. The
// probes may only re-open a port-forward to a loopback endpoint.
func resilienceAPIPort(rawURL string) (int, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return 0, fmt.Errorf("public API URL %q is not an absolute https URL", rawURL)
	}
	host, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		return 0, fmt.Errorf("public API URL %q has no host:port", rawURL)
	}
	switch host {
	case "127.0.0.1", "localhost", "::1":
	default:
		return 0, fmt.Errorf("public API host %q is not loopback; a guarded port-forward cannot restore it", host)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("public API port %q is not a valid port", portText)
	}
	return port, nil
}

// resilienceAPIReady performs one bounded authenticated read.
func resilienceAPIReady(ctx context.Context, stage *resilienceStage) error {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := resilienceGetResult(probeCtx, stage, "execution.list", api.ExecutionListRequest{
		ProjectID: stage.Config.ProjectID,
		Page:      &api.Page{Limit: 1},
	})
	return err
}

// resilienceSharedAccess is the one process-wide recovery port-forward. It
// survives across probes so the controller probe leaves the API reachable for
// the probes that follow; TestMain closes it after the last test.
var resilienceSharedAccess struct {
	mu      sync.Mutex
	forward *PortForward
	port    int
	service string
}

// resilienceCloseSharedAccess joins and kills the process-wide recovery
// forward. TestMain calls it after m.Run.
func resilienceCloseSharedAccess() {
	resilienceSharedAccess.mu.Lock()
	forward := resilienceSharedAccess.forward
	resilienceSharedAccess.forward = nil
	resilienceSharedAccess.mu.Unlock()
	if forward != nil {
		_ = forward.Close()
	}
}

// resilienceEnsureAPIAccess is called at the start of every live probe and
// again after the controller probe deletes the endpoint pod. It first proves an
// authenticated GET, and when the run-stage port-forward has died it re-opens a
// guarded kubectl port-forward to svc/kodama on the SAME local port, then waits
// for the API to answer. The forward belongs to the test process and TestMain
// closes it after the final cleanup.
func resilienceEnsureAPIAccess(t *testing.T, stage *resilienceStage) *resilienceStage {
	t.Helper()
	if err := resilienceAPIReady(context.Background(), stage); err == nil {
		return stage
	}
	port, err := resilienceAPIPort(stage.Config.APIURL)
	if err != nil {
		t.Fatalf("the public API is unavailable and cannot be restored: %v", err)
	}
	service, err := resilienceKodamaService()
	if err != nil {
		t.Fatalf("%v", err)
	}
	guard := stage.Cluster.client.Guard

	resilienceSharedAccess.mu.Lock()
	if current := resilienceSharedAccess.forward; current != nil {
		// resilienceAPIReady above already failed against this forward. A
		// kubectl port-forward binds to one pod at startup and never switches
		// to a replacement pod, so close it immediately instead of stalling on
		// a deleted endpoint.
		_ = current.Close()
		resilienceSharedAccess.forward = nil
	}
	var forward *PortForward
	var lastErr error
	for attempt := 1; attempt <= 5; attempt++ {
		forward, lastErr = StartPortForward(context.Background(), "kubectl", guard, ReleaseNamespace, service, port, 8443)
		if lastErr == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if forward == nil {
		resilienceSharedAccess.mu.Unlock()
		t.Fatalf("the public API at %s is unavailable and a guarded port-forward to svc/%s on local port %d could not be opened (the earlier run-stage forward may still hold it): %v",
			stage.Config.APIURL, service, port, lastErr)
	}
	resilienceSharedAccess.forward = forward
	resilienceSharedAccess.port = port
	resilienceSharedAccess.service = service
	resilienceSharedAccess.mu.Unlock()

	if err := resilienceWaitAPIReady(stage); err != nil {
		t.Fatalf("the public API at %s did not become ready after reopening svc/%s on local port %d: %v",
			stage.Config.APIURL, service, port, err)
	}
	return stage
}

// resilienceWaitAPIReady polls one authenticated read until the API answers.
func resilienceWaitAPIReady(stage *resilienceStage) error {
	return resilienceWaitAPIReadyFor(stage, 90*time.Second)
}

// resilienceWaitAPIReadyFor bounds the authenticated read by an explicit
// timeout, so a caller can probe a possibly-dead forward before replacing it.
func resilienceWaitAPIReadyFor(stage *resilienceStage, timeout time.Duration) error {
	_, err := (resilienceWaiter{Timeout: timeout}).Wait(context.Background(), func(ctx context.Context) (bool, error) {
		if err := resilienceAPIReady(ctx, stage); err != nil {
			return false, nil
		}
		return true, nil
	})
	return err
}

// resilienceNonce is a per-run suffix that keeps markers and pipeline names
// distinct without carrying any host identity.
func resilienceNonce() string {
	return strconv.FormatInt(time.Now().UTC().UnixNano(), 10)
}

// resilienceGetResult performs one authenticated read.
func resilienceGetResult(ctx context.Context, stage *resilienceStage, operation string, input any) (api.Result, error) {
	method, ok := resilienceOperationMethod(operation)
	if !ok {
		return api.Result{}, fmt.Errorf("unknown public operation %q", operation)
	}
	return resilienceInvoke(ctx, stage.API, method, stage.Config.AdminToken, operation, input, nil, "", false)
}

// resilienceMutateResult performs one authenticated mutation with its required
// idempotency key, using the operation's frozen REST method (pipeline.apply is
// PUT, task.create_and_start and ask/reply are POST).
func resilienceMutateResult(ctx context.Context, stage *resilienceStage, operation string, input any, idempotencyKey string) (api.Result, error) {
	method, ok := resilienceOperationMethod(operation)
	if !ok {
		return api.Result{}, fmt.Errorf("unknown public operation %q", operation)
	}
	return resilienceInvoke(ctx, stage.API, method, stage.Config.AdminToken, operation, input, nil, idempotencyKey, false)
}

// resilienceInspect reads one execution observation.
func resilienceInspect(ctx context.Context, stage *resilienceStage, executionID string) (api.ExecutionInspectResult, error) {
	result, err := resilienceGetResult(ctx, stage, "execution.inspect", api.ExecutionInspectRequest{ExecutionID: executionID})
	if err != nil {
		return api.ExecutionInspectResult{}, err
	}
	return resilienceDecodeResult[api.ExecutionInspectResult](result, "execution.inspect")
}

// resilienceReadTask reads one task observation.
func resilienceReadTask(ctx context.Context, stage *resilienceStage, taskID string) (api.TaskReadResult, error) {
	result, err := resilienceGetResult(ctx, stage, "task.read", api.TaskReadRequest{TaskID: taskID})
	if err != nil {
		return api.TaskReadResult{}, err
	}
	return resilienceDecodeResult[api.TaskReadResult](result, "task.read")
}

// resilienceApplyPipeline stores one pipeline and returns its identity.
func resilienceApplyPipeline(ctx context.Context, stage *resilienceStage, name string, document []byte, idempotencyKey string) (api.PipelineApplyResult, error) {
	result, err := resilienceMutateResult(ctx, stage, "pipeline.apply", api.PipelineApplyRequest{
		ProjectID: stage.Config.ProjectID,
		Name:      name,
		YAML:      document,
	}, idempotencyKey)
	if err != nil {
		return api.PipelineApplyResult{}, err
	}
	return resilienceDecodeResult[api.PipelineApplyResult](result, "pipeline.apply")
}

// resilienceStartInline admits one task-free inline execution.
func resilienceStartInline(ctx context.Context, stage *resilienceStage, document []byte, idempotencyKey string) (api.PipelineStartResult, error) {
	return resilienceInlineStart(ctx, stage.API, stage.Config.AdminToken, stage.Config.ProjectID, document, idempotencyKey)
}

// resilienceReadAllEvents reads every committed event through the public
// cursor, bounded by the harness page bound.
func resilienceReadAllEvents(ctx context.Context, stage *resilienceStage) ([]api.Event, error) {
	return resiliencePaginateEvents(func(cursor string) (api.EventReadResult, error) {
		request := api.EventReadRequest{
			ProjectID: stage.Config.ProjectID,
			Page:      &api.Page{Limit: api.MaxPageLimit, Cursor: cursor},
		}
		result, err := resilienceGetResult(ctx, stage, "event.read", request)
		if err != nil {
			return api.EventReadResult{}, err
		}
		return resilienceDecodeResult[api.EventReadResult](result, "event.read")
	}, resilienceMaxEventPages)
}

// resilienceReadAllLogs reads every retained stdout chunk for one action.
func resilienceReadAllLogs(ctx context.Context, stage *resilienceStage, executionID, actionID string) ([]byte, error) {
	chunks, err := resiliencePaginateLogs(func(cursor string) (api.ExecutionLogsReadResult, error) {
		request := api.ExecutionLogsReadRequest{
			ExecutionID: executionID,
			ActionID:    actionID,
			Stream:      "stdout",
			Page:        &api.Page{Limit: api.MaxPageLimit, Cursor: cursor},
		}
		result, err := resilienceGetResult(ctx, stage, "execution.logs.read", request)
		if err != nil {
			return api.ExecutionLogsReadResult{}, err
		}
		return resilienceDecodeResult[api.ExecutionLogsReadResult](result, "execution.logs.read")
	}, resilienceMaxLogPages)
	if err != nil {
		return nil, err
	}
	var data []byte
	for _, chunk := range chunks {
		data = append(data, chunk.Data...)
	}
	return data, nil
}

// resilienceAskCurrent asks from the latest task lifecycle version. A version
// conflict commits nothing, so a fresh read and the same idempotency key are
// safe.
func resilienceAskCurrent(ctx context.Context, stage *resilienceStage, taskID, question, assignment, idempotencyKey string) (api.TaskAskResult, error) {
	for range 20 {
		task, err := resilienceReadTask(ctx, stage, taskID)
		if err != nil {
			return api.TaskAskResult{}, err
		}
		result, err := resilienceMutateVersioned(ctx, stage.API, stage.Config.AdminToken, "task.ask", api.TaskAskRequest{
			TaskID:     taskID,
			Question:   question,
			Assignment: assignment,
		}, task.LifecycleVersion, idempotencyKey)
		if err != nil {
			var public *APIError
			if errors.As(err, &public) && public.Code == api.CodeVersionConflict {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			return api.TaskAskResult{}, err
		}
		return resilienceDecodeResult[api.TaskAskResult](result, "task.ask")
	}
	return api.TaskAskResult{}, fmt.Errorf("task.ask did not converge on the current task lifecycle version")
}

// resilienceReplyCurrent answers the outstanding question from a fresh task
// read, retrying the same safe receipt on a version conflict.
func resilienceReplyCurrent(ctx context.Context, stage *resilienceStage, taskID, answer, idempotencyKey string) (api.TaskReplyResult, error) {
	for range 20 {
		task, err := resilienceReadTask(ctx, stage, taskID)
		if err != nil {
			return api.TaskReplyResult{}, err
		}
		if task.Question == nil {
			return api.TaskReplyResult{}, fmt.Errorf("task %q has no outstanding question", taskID)
		}
		result, err := resilienceMutateVersioned(ctx, stage.API, stage.Config.AdminToken, "task.reply", api.TaskReplyRequest{
			TaskID:                  taskID,
			QuestionID:              task.Question.QuestionID,
			ExpectedQuestionVersion: task.Question.Version,
			Answer:                  answer,
		}, task.LifecycleVersion, idempotencyKey)
		if err != nil {
			var public *APIError
			if errors.As(err, &public) && public.Code == api.CodeVersionConflict {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			return api.TaskReplyResult{}, err
		}
		return resilienceDecodeResult[api.TaskReplyResult](result, "task.reply")
	}
	return api.TaskReplyResult{}, fmt.Errorf("task.reply did not converge on the current task lifecycle version")
}

// resilienceCancelCurrent cancels one execution from a fresh version read,
// retrying the same safe receipt on a version conflict.
func resilienceCancelCurrent(ctx context.Context, stage *resilienceStage, executionID, idempotencyKey string) (api.ExecutionCancelResult, error) {
	for range 20 {
		inspection, err := resilienceInspect(ctx, stage, executionID)
		if err != nil {
			return api.ExecutionCancelResult{}, err
		}
		result, err := resilienceMutateVersioned(ctx, stage.API, stage.Config.AdminToken, "execution.cancel", api.ExecutionCancelRequest{
			ExecutionID: executionID,
		}, inspection.Version, idempotencyKey)
		if err != nil {
			var public *APIError
			if errors.As(err, &public) && public.Code == api.CodeVersionConflict {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			return api.ExecutionCancelResult{}, err
		}
		return resilienceDecodeResult[api.ExecutionCancelResult](result, "execution.cancel")
	}
	return api.ExecutionCancelResult{}, fmt.Errorf("execution.cancel did not converge on the current execution version")
}

// resilienceWaitInspect polls one execution until the predicate holds.
func resilienceWaitInspect(ctx context.Context, stage *resilienceStage, executionID string, timeout time.Duration, predicate func(api.ExecutionInspectResult) bool) (api.ExecutionInspectResult, time.Duration, error) {
	var observed api.ExecutionInspectResult
	elapsed, err := (resilienceWaiter{Timeout: timeout}).Wait(ctx, func(ctx context.Context) (bool, error) {
		inspection, err := resilienceInspect(ctx, stage, executionID)
		if err != nil {
			return false, err
		}
		observed = inspection
		return predicate(inspection), nil
	})
	return observed, elapsed, err
}

// resilienceWaitTerminal polls until an execution reaches a terminal state.
func resilienceWaitTerminal(ctx context.Context, stage *resilienceStage, executionID string, timeout time.Duration) (api.ExecutionInspectResult, time.Duration, error) {
	return resilienceWaitInspect(ctx, stage, executionID, timeout, func(inspection api.ExecutionInspectResult) bool {
		switch inspection.State {
		case "succeeded", "failed", "cancelled":
			return true
		default:
			return false
		}
	})
}

// resilienceWaitRunning polls until an execution has an activated action.
func resilienceWaitRunning(ctx context.Context, stage *resilienceStage, executionID string, timeout time.Duration) (api.ExecutionInspectResult, time.Duration, error) {
	return resilienceWaitInspect(ctx, stage, executionID, timeout, func(inspection api.ExecutionInspectResult) bool {
		return inspection.State == "running"
	})
}

// resilienceWaitCleanupComplete polls until terminal cleanup is complete.
func resilienceWaitCleanupComplete(ctx context.Context, stage *resilienceStage, executionID string, timeout time.Duration) (api.ExecutionInspectResult, time.Duration, error) {
	return resilienceWaitInspect(ctx, stage, executionID, timeout, func(inspection api.ExecutionInspectResult) bool {
		return inspection.Cleanup.State == "complete"
	})
}

// resilienceAction finds one action by name.
func resilienceAction(inspection api.ExecutionInspectResult, name string) (api.ExecutionAction, bool) {
	for _, action := range inspection.Actions {
		if action.Name == name {
			return action, true
		}
	}
	return api.ExecutionAction{}, false
}

// resilienceWaitWorkloadPod waits for the execution's one workload pod to be
// Ready in the assigned tenant namespace. It proves the command has actually
// started before a probe deletes an infrastructure pod or asks for a handoff.
func resilienceWaitWorkloadPod(ctx context.Context, stage *resilienceStage, executionID string, timeout time.Duration) (string, error) {
	pod := ""
	_, err := (resilienceWaiter{Timeout: timeout}).Wait(ctx, func(ctx context.Context) (bool, error) {
		names, err := stage.Cluster.PodNames(ctx, stage.Config.TenantNamespace, "kodama.dev/execution="+executionID)
		if err != nil || len(names) != 1 {
			return false, nil
		}
		if err := stage.Cluster.WaitPodReady(ctx, stage.Config.TenantNamespace, names[0], 2*time.Second); err != nil {
			return false, nil
		}
		pod = names[0]
		return true, nil
	})
	return pod, err
}

// resilienceAssertNoOrphans proves cleanup attribution: once terminal cleanup
// is complete, no Job, pod or workspace claim labelled with the probe's
// execution/workspace remains in the assigned tenant namespace.
func resilienceAssertNoOrphans(t *testing.T, stage *resilienceStage, executionID, workspaceID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, resource := range []string{"jobs", "pods"} {
		names, err := stage.Cluster.ResourceNames(ctx, stage.Config.TenantNamespace, resource, "kodama.dev/execution="+executionID)
		if err != nil {
			t.Errorf("read attributable %s for execution %s: %v", resource, executionID, err)
			continue
		}
		if len(names) != 0 {
			t.Errorf("execution %s left orphan %s %v in %s", executionID, resource, names, stage.Config.TenantNamespace)
		}
	}
	if workspaceID == "" {
		t.Errorf("execution %s completed cleanup without a workspace identity; orphan claims are not attributable", executionID)
		return
	}
	names, err := stage.Cluster.ResourceNames(ctx, stage.Config.TenantNamespace, "persistentvolumeclaims", "kodama.dev/workspace="+workspaceID)
	if err != nil {
		t.Errorf("read attributable claims for workspace %s: %v", workspaceID, err)
		return
	}
	for _, name := range names {
		retained, err := stage.Cluster.ClaimRetained(ctx, stage.Config.TenantNamespace, name)
		if err != nil {
			t.Errorf("read the retention marker of claim %s for workspace %s: %v", name, workspaceID, err)
			continue
		}
		if retained {
			// SH06/S02 retain a workspace across request waits and controller
			// restarts; kube/workspace.go Release clears kodama.dev/retained and
			// deletes the claim only when cleanup completes. A retained claim is
			// therefore a documented retention exception, not an orphan.
			t.Logf("workspace %s retained claim %s after cleanup (documented retention; not counted as an orphan)", workspaceID, name)
			continue
		}
		t.Errorf("workspace %s left orphan persistentvolumeclaims %v in %s", workspaceID, names, stage.Config.TenantNamespace)
	}
}

// resilienceStageProbePrefix keeps every live probe evidence name distinguishable
// when the whole package runs in one evidence directory.
func resilienceStageProbePrefix(name string) string {
	return "resilience-" + strings.ToLower(name)
}
