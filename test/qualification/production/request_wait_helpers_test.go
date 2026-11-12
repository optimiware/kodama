//go:build qualification

package production

// Request-wait revocation probe helpers. Everything here is test-only: the
// qualification package exists only under the qualification build tag and this
// file is never compiled into a product binary.
//
// The probe proves the request-wait lifecycle on the live stage with public
// API calls and guarded kubectl reads only:
//
//  1. an admin bootstraps a project-scoped viewer service and a versioned
//     900-second credential;
//  2. an inline pipeline runs a container action that authenticates with its
//     mounted execution credential, submits a declared fingerprint output and
//     then sleeps long enough for the harness to capture that credential;
//  3. a declared service/<id> request action waits on the immutable subject;
//  4. the harness captures ONLY the running action's credential Secret through
//     guarded reads and proves execution.inspect succeeds with it;
//  5. after the request activates the captured execution credential is refused
//     401 before its recorded deadline;
//  6. the admin revokes the service credential; the stale service token is
//     refused 401 and the request facts do not change;
//  7. a fresh service credential rediscovers and resolves the same request
//     action-only, with an idempotency key and no execution version;
//  8. an identical authorized replay returns the same result while the revoked
//     token is still refused;
//  9. the same execution succeeds, records the service actor and answer, a
//     post-wait action authenticates with a fresh mounted credential, exactly
//     one completion marker is retained, and cleanup converges.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/api"
)

// Pinned request-wait probe vocabulary.
const (
	requestWaitBeforeName   = "before"
	requestWaitDecisionName = "decision"
	requestWaitAfterName    = "after"

	requestWaitServicePrefix   = "kodsvc."
	requestWaitExecutionPrefix = "kodex."
	requestWaitServiceRole     = "viewer"
	requestWaitServiceTTL      = 900

	requestWaitAnswerOutput     = "answer"
	requestWaitFingerprint      = "fingerprint"
	requestWaitRunField         = "run"
	requestWaitFingerprintField = "fingerprint"

	requestWaitRetainedLabel      = "kodama.dev/retained"
	requestWaitRetainedAnnotation = "kodama.dev/retained"
	requestWaitSecretMountPath    = "/kodama/secrets"
	requestWaitMaxPages           = 4
	requestWaitOutputRetries      = 20
	requestWaitPollInterval       = 500 * time.Millisecond
	requestWaitCleanupPoll        = 500 * time.Millisecond

	// The product's pinned object vocabulary. The keys mirror
	// internal/shell/runtime/kube (LabelExecution, LabelAction, LabelWorkload,
	// LabelWorkspace, LabelTenant) and the credential data key mirrors
	// internal/id.SecretTokenName; TestHarnessRequestWaitPinsProductVocabulary
	// refuses drift from the product declarations.
	requestWaitLabelTenant    = "kodama.dev/tenant"
	requestWaitLabelExecution = "kodama.dev/execution"
	requestWaitLabelAction    = "kodama.dev/action"
	requestWaitLabelWorkload  = "kodama.dev/workload"
	requestWaitLabelWorkspace = "kodama.dev/workspace"
	requestWaitTokenKey       = "KODAMA_TOKEN"

	// requestWaitInboxPageLimit is the page size the probe asks task.inbox for.
	// The served inbox handler bound a page at the old package-local 100 in the
	// first Q02 run; it now enforces api.MaxPageLimit (200), the same bound as
	// api.Page. The probe keeps asking for the product default (50), which is
	// inside every accepted bound and never depends on the maximum.
	requestWaitInboxPageLimit = api.DefaultPageLimit
	// requestWaitRefusalDetailBytes bounds the public refusal message the probe
	// renders, and requestWaitEvidenceDetailBytes bounds a public cleanup error.
	requestWaitRefusalDetailBytes  = 512
	requestWaitEvidenceDetailBytes = 256
)

// Timing pins. The container deadlines stay well above the Shell's one-minute
// minimum credential lifetime, so preparation never fails for a short action.
var (
	requestWaitBeforeTimeout   = 5 * time.Minute
	requestWaitDecisionTimeout = 5 * time.Minute
	requestWaitAfterTimeout    = 2 * time.Minute
	requestWaitCaptureWindow   = 30 * time.Second
	// requestWaitCredentialWait bounds how long the pre-wait action may take to
	// become running. A pending action is not a failure: the pinned curl image
	// may still be pulling. The action state, activation, deadline and stop
	// evidence are reported when the bound expires.
	requestWaitCredentialWait  = 3 * time.Minute
	requestWaitBarrierTimeout  = 4 * time.Minute
	requestWaitTerminalTimeout = 3 * time.Minute
	requestWaitProbeTimeout    = 8 * time.Minute
	// requestWaitCleanupTimeout bounds the whole cleanup in the live stage run.
	// It leaves the authority-first teardown its own 45 s AND the workload
	// waits their full windows, so a stuck workload cleanup can never spend the
	// budget before the service credential is revoked and the service disabled.
	requestWaitCleanupTimeout = 3 * time.Minute
	// requestWaitCleanupAuthority bounds only the service-authority teardown
	// (revoke, disable, revoked-token refusal) that runs FIRST. It is
	// deliberately independent of the workload cleanup: a cleanup that never
	// converges (F-4) must not leave a live service credential behind.
	requestWaitCleanupAuthority = 45 * time.Second
	requestWaitCleanupSettle    = 90 * time.Second
	// requestWaitMaterializationWait bounds how long the workload's credential
	// Secret may lag the running action state. The adapter writes the Secret at
	// activation, but inspect can report the action running a moment earlier, so
	// the capture polls instead of trusting one read.
	requestWaitMaterializationWait = 60 * time.Second
	// requestWaitMaterializationPoll is the interval of that bounded poll.
	requestWaitMaterializationPoll = time.Second
	// requestWaitCleanupPendingWait bounds how long cleanup waits for a Pending
	// workload pod to start before it cancels the execution. Cancelling a Pod
	// that never ran deletes it before it can record a confirmed stop (F-4), so
	// cleanup gives the pending pod a bounded chance to start first.
	requestWaitCleanupPendingWait = 30 * time.Second
	// requestWaitSuccessorIdleWait bounds the independent cluster check that the
	// refused resolution advanced no successor workload.
	requestWaitSuccessorIdleWait = time.Minute
	// requestWaitRemovalWait bounds the post-cleanup workload-removal wait.
	requestWaitRemovalWait = time.Minute
)

// requestWaitStep* name every bounded wait in the probe. A live timeout renders
// the step name and its last secret-free observation, so a failure is
// attributable to one predicate instead of a bare "did not hold".
const (
	requestWaitStepBeforeStart      = "pre-wait-action-running"
	requestWaitStepCredential       = "pre-wait-credential-secret"
	requestWaitStepBarrier          = "request-wait-barrier"
	requestWaitStepSuccessorIdle    = "refused-resolution-successor-idle"
	requestWaitStepTerminal         = "request-wait-terminal"
	requestWaitStepCleanupSettle    = "request-wait-cleanup-settle"
	requestWaitStepExecutionCleanup = "execution-cleanup-settle"
	requestWaitStepCleanupRemoval   = "cleanup-workload-removal"
	requestWaitStepPendingWorkload  = "cleanup-pending-workload"
)

// requestWaitPaths are the fixed workload mount paths the probe reads.
type requestWaitPaths struct {
	TokenFile    string
	CAFile       string
	BeforeOutput string
	AnswerOutput string
}

// requestWaitDefaultPaths returns the frozen workload mounts.
func requestWaitDefaultPaths() requestWaitPaths {
	return requestWaitPaths{
		TokenFile:    "/kodama/secrets/KODAMA_TOKEN",
		CAFile:       "/etc/kodama/ca/ca.crt",
		BeforeOutput: "/kodama/outputs/" + requestWaitBeforeName + "/" + requestWaitFingerprint,
		AnswerOutput: "/kodama/outputs/" + requestWaitDecisionName + "/" + requestWaitAnswerOutput,
	}
}

// requestWaitSubject is the immutable request subject shape.
type requestWaitSubject struct {
	Run         string               `yaml:"run"`
	Fingerprint requestWaitReference `yaml:"fingerprint"`
}

// requestWaitReference is one direct subject lookup.
type requestWaitReference struct {
	From string `yaml:"from"`
}

// requestWaitActionDoc is one inline action declaration.
type requestWaitActionDoc struct {
	Name     string              `yaml:"name"`
	Image    string              `yaml:"image,omitempty"`
	Request  string              `yaml:"request,omitempty"`
	Timeout  string              `yaml:"timeout"`
	Subject  *requestWaitSubject `yaml:"subject,omitempty"`
	Commands []string            `yaml:"commands,omitempty"`
	Outputs  []string            `yaml:"outputs,omitempty"`
}

// requestWaitPipeline is the probe document shape.
type requestWaitPipeline struct {
	Actions []requestWaitActionDoc `yaml:"actions"`
}

// requestWaitProbe is one request-wait revocation probe. Every credential it
// holds is a redacting Secret and is never rendered.
type requestWaitProbe struct {
	Client          *API
	Cluster         requestWaitCluster
	AdminToken      Secret
	ProjectID       string
	TenantNamespace string
	ServiceID       string
	RunMarker       string
	Answer          string
	Image           string
	Paths           requestWaitPaths

	BeforeTimeout   time.Duration
	DecisionTimeout time.Duration
	AfterTimeout    time.Duration
	CaptureWindow   time.Duration
	// StartWait bounds how long the pre-wait action may take to become running.
	StartWait time.Duration
	// MaterializationWait bounds how long the credential capture polls for the
	// workload Secret after the action is already running.
	MaterializationWait time.Duration
	// MaterializationPoll overrides the capture poll interval in tests.
	MaterializationPoll time.Duration
	// SuccessorIdleWait bounds the independent cluster check that a refused
	// resolution advanced no successor workload; zero uses the pinned value.
	SuccessorIdleWait time.Duration

	Cleanup *requestWaitCleanup

	serviceVersion uint64
	serviceToken   Secret
	revokedToken   Secret
	freshToken     Secret
	credential     Secret
	workspaceID    string

	// refusals keeps the bounded public detail of the most recent refusal so a
	// probe diagnostic can name the field the server refused.
	refusals *requestWaitRefusals

	BarrierInspection api.ExecutionInspectResult
	BarrierItem       api.InboxItem
	FinalInspection   api.ExecutionInspectResult
	ResolveResult     api.ExecutionRequestResolveResult
	ReplayResult      api.ExecutionRequestResolveResult
	MarkerCount       int
	replayKey         string
}

// requestWaitCluster is the guarded read surface the probe needs. The live
// implementation wraps the hardened Kubectl helper; unit tests use a fake.
type requestWaitCluster interface {
	ObjectList(ctx context.Context, namespace, resource, selector string) ([]byte, error)
}

// requestWaitKube is the production cluster reader: every call is one guarded,
// namespaced kubectl read with a validated label selector.
type requestWaitKube struct {
	client    *Kubectl
	namespace string
}

// ObjectList reads one labelled object list as JSON.
func (kube *requestWaitKube) ObjectList(ctx context.Context, namespace, resource, selector string) ([]byte, error) {
	if kube == nil || kube.client == nil {
		return nil, fmt.Errorf("the request-wait cluster reader has no kubectl helper")
	}
	if kube.namespace != "" && namespace != kube.namespace {
		return nil, fmt.Errorf("namespace %q is outside the probe's assigned namespace", namespace)
	}
	return kube.client.Run(ctx, namespace, "get", resource, "-l", selector, "-o", "json")
}

// requestWaitInputs is the environment input set the live probe requires.
func requestWaitInputs() []Input {
	return []Input{
		InputAPIURL,
		InputCAPEM,
		InputTLSServerName,
		InputAdminToken,
		InputProjectID,
		InputTenantNamespace,
		InputKubeContext,
		InputImage,
		InputEvidenceDir,
	}
}

// newRequestWaitProbe builds one probe with the pinned timings and mounts.
func newRequestWaitProbe(config Config, client *API, cluster requestWaitCluster, serviceID, runMarker string) *requestWaitProbe {
	probe := &requestWaitProbe{
		Client:          client,
		Cluster:         cluster,
		AdminToken:      config.AdminToken,
		ProjectID:       config.ProjectID,
		TenantNamespace: config.TenantNamespace,
		ServiceID:       serviceID,
		RunMarker:       runMarker,
		Answer:          "q02-request-wait-answer-" + runMarker,
		Image:           config.Image,
		Paths:           requestWaitDefaultPaths(),
		BeforeTimeout:   requestWaitBeforeTimeout,
		DecisionTimeout: requestWaitDecisionTimeout,
		AfterTimeout:    requestWaitAfterTimeout,
		CaptureWindow:   requestWaitCaptureWindow,
		StartWait:       requestWaitCredentialWait,
	}
	probe.observeRefusals(client)
	return probe
}

// startWait returns the bounded pre-wait start deadline.
func (probe *requestWaitProbe) startWait() time.Duration {
	if probe != nil && probe.StartWait > 0 {
		return probe.StartWait
	}
	return requestWaitCredentialWait
}

// materializationWait returns the bounded credential-Secret wait.
func (probe *requestWaitProbe) materializationWait() time.Duration {
	if probe != nil && probe.MaterializationWait > 0 {
		return probe.MaterializationWait
	}
	return requestWaitMaterializationWait
}

// materializationPoll returns the capture poll interval.
func (probe *requestWaitProbe) materializationPoll() time.Duration {
	if probe != nil && probe.MaterializationPoll > 0 {
		return probe.MaterializationPoll
	}
	return requestWaitMaterializationPoll
}

// successorIdleWait returns the bounded successor-idle wait.
func (probe *requestWaitProbe) successorIdleWait() time.Duration {
	if probe != nil && probe.SuccessorIdleWait > 0 {
		return probe.SuccessorIdleWait
	}
	return requestWaitSuccessorIdleWait
}

// requestWaitRefusalRecord is one captured public refusal detail.
type requestWaitRefusalRecord struct {
	Code   string
	Detail string
}

// requestWaitRefusals observes the public refusals of one probe client. The
// shared public helper keeps only the code, status and request id of a refusal,
// so this transport wrapper also keeps the bounded, secret-free public message
// of the most recent refused response: that message is the field pointer the
// server refused, which is what makes an invalid_input attributable to one
// operation. It never keeps a request body, a credential or a successful
// response, and every response reaches the caller unchanged.
type requestWaitRefusals struct {
	client *API
	next   http.RoundTripper
	bound  int

	mu   sync.Mutex
	last requestWaitRefusalRecord
}

// observeRefusals installs the refusal observer on one probe client, once.
func (probe *requestWaitProbe) observeRefusals(client *API) {
	if probe == nil || client == nil || client.client == nil || client.client.Transport == nil {
		return
	}
	if _, ok := client.client.Transport.(*requestWaitRefusals); ok {
		return
	}
	bound := client.maxResponse
	if bound <= 0 {
		bound = api.MaxResponseBytes
	}
	observer := &requestWaitRefusals{client: client, next: client.client.Transport, bound: bound}
	client.client.Transport = observer
	probe.refusals = observer
}

// RoundTrip passes one response through unchanged and records the bounded public
// message of a refusal. The refusal body is replayed byte for byte, and an
// oversize body is left to the caller's own bound so that refusal is unchanged.
func (observer *requestWaitRefusals) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := observer.next.RoundTrip(request)
	if err != nil || response == nil || response.Body == nil || response.StatusCode == api.HTTPStatusSuccess {
		return response, err
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, int64(observer.bound)+1))
	_ = response.Body.Close()
	response.Body = io.NopCloser(bytes.NewReader(data))
	if readErr != nil || len(data) > observer.bound {
		observer.store(requestWaitRefusalRecord{})
		return response, nil
	}
	var public api.Error
	if json.Unmarshal(data, &public) != nil || public.Validate() != nil {
		observer.store(requestWaitRefusalRecord{})
		return response, nil
	}
	observer.store(requestWaitRefusalRecord{Code: public.Code, Detail: requestWaitPublicDetail(public.Message, requestWaitRefusalDetailBytes)})
	return response, nil
}

// store keeps one refusal record for the current call.
func (observer *requestWaitRefusals) store(record requestWaitRefusalRecord) {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	observer.last = record
}

// detail returns the captured detail for one refusal code.
func (observer *requestWaitRefusals) detail(code string) string {
	if observer == nil {
		return ""
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if observer.last.Code != code {
		return ""
	}
	return observer.last.Detail
}

// requestWaitPublicDetail bounds one server-rendered public message to printable
// ASCII so a diagnostic never carries a control byte or an unbounded value. The
// public message is secret-free by contract: the edge renders a fault's public
// message and never its private cause.
func requestWaitPublicDetail(detail string, limit int) string {
	if detail == "" || len(detail) > limit {
		return ""
	}
	for index := 0; index < len(detail); index++ {
		if detail[index] < 0x20 || detail[index] > 0x7e {
			return ""
		}
	}
	return detail
}

// requestWaitCallError is one operation-labelled public refusal. It unwraps to
// the shared APIError so a refusal-class assertion keeps working.
type requestWaitCallError struct {
	Operation string
	Refusal   *APIError
	Detail    string
}

// Error renders the operation, the frozen public code, its status, the public
// request identity and the server's field detail. It never renders a request or
// response body, a token or a secret.
func (refusal *requestWaitCallError) Error() string {
	message := fmt.Sprintf("%s was refused: %s (status %d", refusal.Operation, refusal.Refusal.Code, refusal.Refusal.Status)
	if requestID := requestWaitPublicDetail(refusal.Refusal.RequestID, requestWaitRefusalDetailBytes); requestID != "" {
		message += ", request_id " + requestID
	}
	message += ")"
	if refusal.Detail != "" {
		message += ": " + refusal.Detail
	}
	return message
}

// Unwrap exposes the shared refusal so errors.As still classifies it.
func (refusal *requestWaitCallError) Unwrap() error {
	return refusal.Refusal
}

// refused labels one failed probe call with its operation identity and the
// public refusal evidence.
func (probe *requestWaitProbe) refused(operation string, err error) error {
	if err == nil {
		return nil
	}
	var refusal *APIError
	if !errors.As(err, &refusal) || refusal == nil {
		return fmt.Errorf("%s failed: %w", operation, err)
	}
	detail := ""
	if probe.refusals != nil && probe.refusals.client == probe.Client {
		detail = probe.refusals.detail(refusal.Code)
	}
	return &requestWaitCallError{Operation: operation, Refusal: refusal, Detail: detail}
}

// validate refuses a probe that cannot prove the spec's behavior.
func (probe *requestWaitProbe) validate() error {
	if probe == nil {
		return fmt.Errorf("the request-wait probe is nil")
	}
	if probe.Client == nil {
		return fmt.Errorf("the request-wait probe has no public client")
	}
	if probe.Cluster == nil {
		return fmt.Errorf("the request-wait probe has no guarded cluster reader")
	}
	if !probe.AdminToken.IsSet() {
		return fmt.Errorf("the request-wait probe has no admin credential")
	}
	return probe.validateDocument()
}

// validateDocument refuses a document that cannot prove the spec's behavior.
func (probe *requestWaitProbe) validateDocument() error {
	if probe == nil {
		return fmt.Errorf("the request-wait probe is nil")
	}
	if probe.ProjectID == "" {
		return fmt.Errorf("the request-wait probe has no project")
	}
	if !identityPattern.MatchString(probe.ProjectID) {
		return fmt.Errorf("the request-wait project identity is invalid")
	}
	if !identityPattern.MatchString(probe.ServiceID) {
		return fmt.Errorf("the request-wait service identity is invalid")
	}
	if probe.RunMarker == "" || len(probe.RunMarker) > 128 {
		return fmt.Errorf("the request-wait run marker is empty or too long")
	}
	if probe.Answer == "" || len(probe.Answer) > api.MaxSubjectValueBytes {
		return fmt.Errorf("the request-wait answer is empty or too long")
	}
	if !pinnedImagePattern.MatchString(probe.Image) {
		return fmt.Errorf("the request-wait probe requires a digest-pinned image")
	}
	if probe.BeforeTimeout != requestWaitBeforeTimeout ||
		probe.DecisionTimeout != requestWaitDecisionTimeout ||
		probe.AfterTimeout != requestWaitAfterTimeout ||
		probe.CaptureWindow != requestWaitCaptureWindow ||
		probe.startWait() != requestWaitCredentialWait ||
		probe.successorIdleWait() != requestWaitSuccessorIdleWait ||
		(probe.MaterializationWait != 0 && probe.MaterializationWait != requestWaitMaterializationWait) ||
		(probe.MaterializationPoll != 0 && probe.MaterializationPoll != requestWaitMaterializationPoll) {
		return fmt.Errorf("the request-wait probe timings are not the pinned values")
	}
	if probe.CaptureWindow <= 0 || probe.BeforeTimeout <= time.Minute {
		return fmt.Errorf("the request-wait capture window and container deadline are not usable")
	}
	if probe.Paths != requestWaitDefaultPaths() {
		return fmt.Errorf("the request-wait probe paths are not the frozen workload mounts")
	}
	return nil
}

// document builds the ordered inline pipeline: authenticate and fingerprint in
// a container, ask a declared service assignment about that immutable subject,
// then require the answer and a fresh credential in a second container.
func (probe *requestWaitProbe) document() ([]byte, error) {
	if err := probe.validateDocument(); err != nil {
		return nil, err
	}
	subject := &requestWaitSubject{
		Run:         probe.RunMarker,
		Fingerprint: requestWaitReference{From: requestWaitBeforeName + ".outputs." + requestWaitFingerprint},
	}
	return yaml.Marshal(requestWaitPipeline{Actions: []requestWaitActionDoc{
		{
			Name:     requestWaitBeforeName,
			Image:    probe.Image,
			Timeout:  probe.BeforeTimeout.String(),
			Commands: probe.beforeCommands(),
			Outputs:  []string{requestWaitFingerprint},
		},
		{
			Name:    requestWaitDecisionName,
			Request: "service/" + probe.ServiceID,
			Timeout: probe.DecisionTimeout.String(),
			Subject: subject,
			Outputs: []string{requestWaitAnswerOutput},
		},
		{
			Name:     requestWaitAfterName,
			Image:    probe.Image,
			Timeout:  probe.AfterTimeout.String(),
			Commands: probe.afterCommands(),
		},
	}})
}

// beforeCommands prove the fresh mounted execution credential authenticates,
// submit only its sha256 fingerprint through the public output operation with
// a 409 retry from a fresh inspection, then sleep so the harness can capture
// the very credential this action used. Nothing but the fingerprint and public
// status codes is printed.
func (probe *requestWaitProbe) beforeCommands() []string {
	token := shellQuote(probe.Paths.TokenFile)
	ca := shellQuote(probe.Paths.CAFile)
	return []string{
		`for tool in curl sha256sum cut sleep grep head cat; do command -v "$tool" >/dev/null 2>&1 || { echo "q02: the workload image must provide $tool" >&2; exit 1; }; done`,
		`test -s ` + token,
		`FIRST_SHA=$(sha256sum ` + token + ` | cut -d' ' -f1)`,
		`test -n "$FIRST_SHA"`,
		`attempt=0`,
		`submitted=0`,
		fmt.Sprintf(`while [ "$attempt" -lt %d ]; do attempt=$((attempt+1)); INSPECT_CODE=$(curl -sS -X GET -o /dev/null -w '%%{http_code}' --cacert %s -H "Authorization: Bearer $(cat %s)" -H 'Content-Type: application/json' --data-binary '{}' "$KODAMA_URL/api/v1/executions/$KODAMA_EXECUTION"); if [ "$INSPECT_CODE" != "200" ]; then echo "q02: the mounted execution credential was refused with status $INSPECT_CODE" >&2; exit 1; fi; INSPECT=$(curl -sS -X GET --cacert %s -H "Authorization: Bearer $(cat %s)" -H 'Content-Type: application/json' --data-binary '{}' "$KODAMA_URL/api/v1/executions/$KODAMA_EXECUTION"); case "$INSPECT" in *'"operation":"execution.inspect"'*) ;; *) echo "q02: the execution inspection did not return a public result" >&2; exit 1 ;; esac; VERSION=$(printf '%%s' "$INSPECT" | grep -o '"version":[0-9]*' | head -n 1 | cut -d: -f2); ACTION=$(printf '%%s' "$INSPECT" | grep -o '"action_id":"[^"]*"' | head -n 1 | cut -d'"' -f4); test -n "$VERSION" || { echo "q02: the inspection carried no execution version" >&2; exit 1; }; test -n "$ACTION" || { echo "q02: the inspection carried no action id" >&2; exit 1; }; BODY=$(printf '{"input":{"outputs":{"%s":"%%s"}},"expected_version":%%s,"idempotency_key":"q02-out-%%s-%%s"}' "$FIRST_SHA" "$VERSION" "$attempt" "$FIRST_SHA"); OUTPUT_CODE=$(curl -sS -X PUT -o /dev/null -w '%%{http_code}' --cacert %s -H "Authorization: Bearer $(cat %s)" -H 'Content-Type: application/json' --data-binary "$BODY" "$KODAMA_URL/api/v1/executions/$KODAMA_EXECUTION/actions/$ACTION/outputs"); case "$OUTPUT_CODE" in 200) submitted=1; break ;; 409) continue ;; *) echo "q02: the fingerprint output submission was refused with status $OUTPUT_CODE" >&2; exit 1 ;; esac; done`,
			requestWaitOutputRetries, ca, token, ca, token, requestWaitFingerprint, ca, token),
		`test "$submitted" = "1" || { echo "q02: the fingerprint output submission did not converge" >&2; exit 1; }`,
		fmt.Sprintf(`sleep %d`, int(probe.CaptureWindow.Seconds())),
	}
}

// afterCommands require the materialized answer, a mounted credential whose
// fingerprint differs from the pre-wait one, a successful authenticated
// inspection with that fresh credential, and emit exactly one completion
// marker.
func (probe *requestWaitProbe) afterCommands() []string {
	token := shellQuote(probe.Paths.TokenFile)
	ca := shellQuote(probe.Paths.CAFile)
	answer := shellQuote(probe.Paths.AnswerOutput)
	before := shellQuote(probe.Paths.BeforeOutput)
	return []string{
		`for tool in curl sha256sum cut grep head cat; do command -v "$tool" >/dev/null 2>&1 || { echo "q02: the workload image must provide $tool" >&2; exit 1; }; done`,
		`test -s ` + answer,
		`test "$(cat ` + answer + `)" = ` + shellQuote(probe.Answer),
		`test -s ` + token,
		`test -s ` + before,
		`AFTER_SHA=$(sha256sum ` + token + ` | cut -d' ' -f1)`,
		`FIRST_SHA=$(cat ` + before + `)`,
		`test -n "$AFTER_SHA"`,
		`test -n "$FIRST_SHA"`,
		`if [ "$AFTER_SHA" = "$FIRST_SHA" ]; then echo "q02: the post-wait action reused the old mounted credential" >&2; exit 1; fi`,
		`NOW_CODE=$(curl -sS -X GET -o /dev/null -w '%{http_code}' --cacert ` + ca + ` -H "Authorization: Bearer $(cat ` + token + `)" -H 'Content-Type: application/json' --data-binary '{}' "$KODAMA_URL/api/v1/executions/$KODAMA_EXECUTION")`,
		`if [ "$NOW_CODE" != "200" ]; then echo "q02: the fresh mounted credential was refused with status $NOW_CODE" >&2; exit 1; fi`,
		`echo ` + shellQuote(requestWaitMarker(probe.RunMarker)),
	}
}

// requestWaitMarker is the single fixed completion marker.
func requestWaitMarker(runMarker string) string {
	return "q02-request-wait-complete:" + runMarker
}

// run drives the ordered probe: bootstrap, start, credential capture, barrier,
// stale execution credential refusal, service revocation, fresh resolution,
// authorized replay, terminal success and retained log evidence.
func (probe *requestWaitProbe) run(ctx context.Context) error {
	if err := probe.validate(); err != nil {
		return err
	}
	if probe.Cleanup == nil {
		return fmt.Errorf("the request-wait probe has no registered cleanup")
	}
	if err := probe.createService(ctx); err != nil {
		return err
	}
	if err := probe.issueInitialCredential(ctx); err != nil {
		return err
	}
	document, err := probe.document()
	if err != nil {
		return err
	}
	startKey, err := probeIdempotencyKey("q02-rw-start")
	if err != nil {
		return err
	}
	executionID, err := startInlineExecution(ctx, probe.Client, probe.AdminToken, probe.ProjectID, document, startKey)
	if err != nil {
		return probe.refused("pipeline.start", err)
	}
	probe.Cleanup.executionID = executionID

	before, err := probe.awaitBeforeRunning(ctx, executionID)
	if err != nil {
		return err
	}
	credential, err := probe.captureCredential(ctx, executionID, before.ActionID)
	if err != nil {
		return err
	}
	probe.credential = credential.Token
	probe.workspaceID = credential.WorkspaceID
	probe.Cleanup.workspaceID = credential.WorkspaceID
	if err := probe.proveExecutionCredential(ctx, executionID); err != nil {
		return err
	}

	inspection, item, err := probe.awaitBarrier(ctx, executionID)
	if err != nil {
		return err
	}
	probe.BarrierInspection = inspection
	probe.BarrierItem = item

	if err := probe.assertCapturedCredentialRefused(ctx, executionID, inspection); err != nil {
		return err
	}
	if err := probe.revokeServiceCredential(ctx); err != nil {
		return err
	}
	if err := probe.assertStaleServiceCredentialRefused(ctx, executionID, item); err != nil {
		return err
	}
	if err := probe.assertRequestUnchanged(ctx, executionID, inspection, item); err != nil {
		return err
	}
	if err := probe.issueFreshCredential(ctx); err != nil {
		return err
	}
	rediscovered, err := probe.rediscover(ctx, executionID, item.ActionID)
	if err != nil {
		return err
	}
	resolveKey, err := probeIdempotencyKey("q02-rw-resolve")
	if err != nil {
		return err
	}
	result, err := probe.resolveWithKey(ctx, probe.serviceToken, executionID, rediscovered, resolveKey)
	if err != nil {
		return err
	}
	probe.replayKey = resolveKey
	probe.ResolveResult = result
	replay, err := probe.resolveWithKey(ctx, probe.serviceToken, executionID, rediscovered, resolveKey)
	if err != nil {
		return fmt.Errorf("the identical authorized replay was refused: %w", err)
	}
	probe.ReplayResult = replay
	if replay != result {
		return fmt.Errorf("the identical authorized replay returned a different result")
	}
	if err := probe.assertRevokedReplayRefused(ctx, executionID, rediscovered); err != nil {
		return err
	}
	if err := probe.assertCompleted(ctx, executionID); err != nil {
		return err
	}
	return nil
}

// createService creates the project-scoped viewer service.
func (probe *requestWaitProbe) createService(ctx context.Context) error {
	key, err := probeIdempotencyKey("q02-rw-service-create")
	if err != nil {
		return err
	}
	result, err := probe.Client.Post(ctx, probe.AdminToken, "service.create",
		api.ServiceCreateRequest{ProjectID: probe.ProjectID, ServiceID: probe.ServiceID, Roles: []string{requestWaitServiceRole}}, key)
	if err != nil {
		return probe.refused("service.create", err)
	}
	created, err := decodeStageResult[api.ServiceCreateResult](result, "service.create")
	if err != nil {
		return fmt.Errorf("service.create returned an invalid result: %w", err)
	}
	if created.ServiceID != probe.ServiceID {
		return fmt.Errorf("service.create returned service %q, want %q", created.ServiceID, probe.ServiceID)
	}
	probe.serviceVersion = created.Version
	probe.Cleanup.bindService(probe)
	return nil
}

// issueInitialCredential issues the first 900-second service credential.
func (probe *requestWaitProbe) issueInitialCredential(ctx context.Context) error {
	token, version, err := probe.issueServiceCredential(ctx)
	if err != nil {
		return err
	}
	probe.serviceToken = token
	probe.serviceVersion = version
	probe.Cleanup.bindService(probe)
	return nil
}

// issueServiceCredential issues one fresh versioned service credential.
func (probe *requestWaitProbe) issueServiceCredential(ctx context.Context) (Secret, uint64, error) {
	key, err := probeIdempotencyKey("q02-rw-service-issue")
	if err != nil {
		return "", 0, err
	}
	result, err := probe.Client.PostVersioned(ctx, probe.AdminToken, "service.credential.issue",
		api.ServiceCredentialIssueRequest{ServiceID: probe.ServiceID, TTLSeconds: requestWaitServiceTTL}, key, probe.serviceVersion)
	if err != nil {
		return "", 0, probe.refused("service.credential.issue", err)
	}
	issued, err := decodeStageResult[api.ServiceCredentialIssueResult](result, "service.credential.issue")
	if err != nil {
		return "", 0, fmt.Errorf("service.credential.issue returned an invalid result: %w", err)
	}
	token, err := stageCredential(issued.Token, requestWaitServicePrefix)
	if err != nil {
		return "", 0, fmt.Errorf("service.credential.issue: %w", err)
	}
	return token, issued.Version, nil
}

// revokeServiceCredential revokes the stale service credential at its current
// version and requires affirmative invalidation.
func (probe *requestWaitProbe) revokeServiceCredential(ctx context.Context) error {
	key, err := probeIdempotencyKey("q02-rw-service-revoke")
	if err != nil {
		return err
	}
	result, err := probe.Client.DeleteVersioned(ctx, probe.AdminToken, "service.credential.revoke",
		api.ServiceCredentialRevokeRequest{ServiceID: probe.ServiceID}, key, probe.serviceVersion)
	if err != nil {
		return probe.refused("service.credential.revoke", err)
	}
	revoked, err := decodeStageResult[api.ServiceCredentialRevokeResult](result, "service.credential.revoke")
	if err != nil {
		return fmt.Errorf("service.credential.revoke returned an invalid result: %w", err)
	}
	if revoked.Invalidated == nil || !*revoked.Invalidated {
		return fmt.Errorf("service.credential.revoke reported no invalidation")
	}
	probe.revokedToken = probe.serviceToken
	probe.serviceToken = ""
	probe.serviceVersion = revoked.Version
	probe.Cleanup.bindService(probe)
	return nil
}

// issueFreshCredential issues the post-revocation service credential.
func (probe *requestWaitProbe) issueFreshCredential(ctx context.Context) error {
	token, version, err := probe.issueServiceCredential(ctx)
	if err != nil {
		return err
	}
	probe.freshToken = token
	probe.serviceToken = token
	probe.serviceVersion = version
	probe.Cleanup.bindService(probe)
	return nil
}

// errRequestWaitBeforeEnded marks a pre-wait action that ended before the probe
// could observe it running. A pending action is not a failure (the pinned image
// may still be pulling), but an action that has already stopped can never yield
// the mounted credential this probe must capture.
var errRequestWaitBeforeEnded = errors.New("the pre-wait action ended before it ran")

// awaitBeforeRunning waits until the before action is running and returns it. A
// not-yet-started workload is tolerated to the bounded start deadline; an action
// that reaches a terminal state first fails immediately. Both failures report the
// secret-free action evidence: state, version, activation, deadline and stop.
func (probe *requestWaitProbe) awaitBeforeRunning(ctx context.Context, executionID string) (api.ExecutionAction, error) {
	var (
		before   api.ExecutionAction
		found    bool
		observed api.ExecutionInspectResult
		seen     bool
	)
	deadline := probe.startWait()
	started := time.Now()
	step := requestWaitStep{
		Name: requestWaitStepBeforeStart,
		Observe: func() string {
			return requestWaitExecutionEvidence(observed, seen) + "; " + requestWaitActionEvidence(before, found)
		},
	}
	_, err := requestWaitPoll(ctx, step, deadline, func() (bool, error) {
		inspection, err := probe.inspect(ctx, probe.AdminToken, executionID)
		if err != nil {
			return false, err
		}
		observed, seen = inspection, true
		action, ok := resilienceAction(inspection, requestWaitBeforeName)
		if !ok {
			return false, nil
		}
		before, found = action, true
		if action.State == "running" {
			return true, nil
		}
		if terminalExecutionState(action.State) {
			return false, fmt.Errorf("%w: %s (execution %s %s)", errRequestWaitBeforeEnded,
				requestWaitActionEvidence(action, true), executionID, requestWaitExecutionEvidence(inspection, true))
		}
		return false, nil
	})
	if err == nil {
		return before, nil
	}
	if errors.Is(err, errRequestWaitBeforeEnded) {
		return api.ExecutionAction{}, err
	}
	return api.ExecutionAction{}, fmt.Errorf(
		"the pre-wait action did not start within %s (waited %s; execution %s %s; %s): %w",
		deadline, time.Since(started).Round(time.Millisecond), executionID,
		requestWaitExecutionEvidence(observed, seen), requestWaitActionEvidence(before, found), err)
}

// requestWaitActionEvidence renders one action's secret-free lifecycle evidence.
func requestWaitActionEvidence(action api.ExecutionAction, found bool) string {
	if !found {
		return "the pre-wait action is absent"
	}
	return fmt.Sprintf("action %q state %q version %d activated_at %s deadline %s stop %s",
		action.Name, action.State, action.Version,
		requestWaitTimeEvidence(action.ActivatedAt), requestWaitTimeEvidence(action.Deadline),
		requestWaitStopEvidence(action.Stop))
}

// requestWaitTimeEvidence renders one optional timestamp without a zero value.
func requestWaitTimeEvidence(value *time.Time) string {
	if value == nil || value.IsZero() {
		return "none"
	}
	return value.UTC().Format(time.RFC3339)
}

// requestWaitStopEvidence renders the stop state and its public detail.
func requestWaitStopEvidence(stop *api.ExecutionStopEvidence) string {
	if stop == nil {
		return "none"
	}
	detail := requestWaitPublicDetail(stop.Detail, requestWaitEvidenceDetailBytes)
	if detail == "" {
		return stop.State
	}
	return stop.State + " (" + detail + ")"
}

// requestWaitExecutionEvidence renders the execution state and version.
func requestWaitExecutionEvidence(inspection api.ExecutionInspectResult, seen bool) string {
	if !seen {
		return "state unknown"
	}
	return fmt.Sprintf("state %q version %d", inspection.State, inspection.Version)
}

// awaitBarrier waits for the wait barrier and validates every decision fact.
func (probe *requestWaitProbe) awaitBarrier(ctx context.Context, executionID string) (api.ExecutionInspectResult, api.InboxItem, error) {
	var (
		observed   api.ExecutionInspectResult
		seen       bool
		item       api.InboxItem
		inboxCount int
		inboxFound bool
	)
	step := requestWaitStep{
		Name: requestWaitStepBarrier,
		Observe: func() string {
			return fmt.Sprintf("execution %s %s actions %s; inbox items %d request_item %t",
				executionID, requestWaitExecutionEvidence(observed, seen),
				requestWaitActionStates(observed), inboxCount, inboxFound)
		},
	}
	_, err := requestWaitPoll(ctx, step, requestWaitBarrierTimeout, func() (bool, error) {
		inspection, err := probe.inspect(ctx, probe.AdminToken, executionID)
		if err != nil {
			return false, err
		}
		observed, seen = inspection, true
		items, err := probe.readInbox(ctx, probe.serviceToken)
		if err != nil {
			return false, err
		}
		inboxCount = len(items)
		found, ok := requestWaitFindRequest(items, executionID)
		inboxFound = ok
		if !ok {
			return false, nil
		}
		item = found
		return requestWaitBarrierReached(inspection), nil
	})
	if err != nil {
		return api.ExecutionInspectResult{}, api.InboxItem{}, fmt.Errorf("the request-wait barrier was not reached: %w", err)
	}
	if err := requestWaitValidateBarrier(observed, item, probe.ServiceID, probe.RunMarker); err != nil {
		return api.ExecutionInspectResult{}, api.InboxItem{}, err
	}
	return observed, item, nil
}

// requestWaitActionStates renders one observation's ordered action states.
func requestWaitActionStates(inspection api.ExecutionInspectResult) string {
	if len(inspection.Actions) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(inspection.Actions))
	for _, action := range inspection.Actions {
		parts = append(parts, action.Name+"="+action.State)
	}
	return strings.Join(parts, ",")
}

// requestWaitBarrierReached reports whether the observation has the shape the
// probe validates: succeeded predecessor, running request, pending successor.
func requestWaitBarrierReached(inspection api.ExecutionInspectResult) bool {
	before, okBefore := resilienceAction(inspection, requestWaitBeforeName)
	decision, okDecision := resilienceAction(inspection, requestWaitDecisionName)
	after, okAfter := resilienceAction(inspection, requestWaitAfterName)
	if !okBefore || !okDecision || !okAfter {
		return false
	}
	return before.State == "succeeded" && decision.State == "running" &&
		after.State == "pending" && inspection.State == "running"
}

// requestWaitValidateBarrier checks the immutable subject, assignment,
// declaration, deadline, predecessor stop, and unactivated successor.
func requestWaitValidateBarrier(inspection api.ExecutionInspectResult, item api.InboxItem, serviceID, runMarker string) error {
	if inspection.State != "running" {
		return fmt.Errorf("the execution state = %q; want running while the request waits", inspection.State)
	}
	if len(inspection.Actions) != 3 {
		return fmt.Errorf("the execution has %d actions; want the three ordered actions", len(inspection.Actions))
	}
	before, ok := resilienceAction(inspection, requestWaitBeforeName)
	if !ok {
		return fmt.Errorf("the execution has no pre-wait action")
	}
	if before.State != "succeeded" {
		return fmt.Errorf("the pre-wait action state = %q; want succeeded", before.State)
	}
	if before.Stop == nil || before.Stop.State != "confirmed" {
		return fmt.Errorf("the pre-wait action stop is not confirmed")
	}
	fingerprint := before.Outputs[requestWaitFingerprint]
	if fingerprint == "" {
		return fmt.Errorf("the pre-wait fingerprint output is missing")
	}
	decision, ok := resilienceAction(inspection, requestWaitDecisionName)
	if !ok {
		return fmt.Errorf("the execution has no request action")
	}
	if decision.Kind != "request" || decision.State != "running" {
		return fmt.Errorf("the request action kind/state = %q/%q; want request/running", decision.Kind, decision.State)
	}
	if decision.Assignment != "service/"+serviceID {
		return fmt.Errorf("the request assignment = %q; want service/%s", decision.Assignment, serviceID)
	}
	if decision.Subject == nil {
		return fmt.Errorf("the request action carries no immutable subject")
	}
	fields := map[string]string{}
	for _, field := range decision.Subject.Fields {
		fields[field.Name] = field.Value
	}
	if fields[requestWaitRunField] != runMarker {
		return fmt.Errorf("the request subject run marker is not the probe's marker")
	}
	if fields[requestWaitFingerprintField] != fingerprint {
		return fmt.Errorf("the request subject fingerprint is not the settled pre-wait fingerprint")
	}
	if decision.Deadline == nil || decision.Deadline.IsZero() {
		return fmt.Errorf("the request action has no deadline")
	}
	after, ok := resilienceAction(inspection, requestWaitAfterName)
	if !ok {
		return fmt.Errorf("the execution has no post-wait action")
	}
	if after.State != "pending" {
		return fmt.Errorf("the successor state = %q; want pending before resolution", after.State)
	}
	if after.ActivatedAt != nil {
		return fmt.Errorf("the successor was activated before the request was answered")
	}
	if item.Kind != "request" || item.ExecutionID != inspection.ExecutionID || item.ActionID != decision.ActionID {
		return fmt.Errorf("the inbox item does not name the running request")
	}
	if item.Assignment != decision.Assignment {
		return fmt.Errorf("the inbox assignment = %q; want %q", item.Assignment, decision.Assignment)
	}
	if item.Version != decision.Version {
		return fmt.Errorf("the inbox version = %d; want the action version %d", item.Version, decision.Version)
	}
	if len(item.Outputs) != 1 || item.Outputs[0] != requestWaitAnswerOutput {
		return fmt.Errorf("the declared response outputs = %v; want only %q", item.Outputs, requestWaitAnswerOutput)
	}
	if item.Subject == nil {
		return fmt.Errorf("the inbox request carries no immutable subject")
	}
	itemFields := map[string]string{}
	for _, field := range item.Subject.Fields {
		itemFields[field.Name] = field.Value
	}
	if itemFields[requestWaitRunField] != runMarker || itemFields[requestWaitFingerprintField] != fingerprint {
		return fmt.Errorf("the inbox subject does not match the inspection subject")
	}
	if !item.At.Equal(*decision.Deadline) {
		return fmt.Errorf("the inbox deadline does not match the request action deadline")
	}
	return nil
}

// proveExecutionCredential proves the captured credential authenticates while
// the pre-wait action still owns it.
func (probe *requestWaitProbe) proveExecutionCredential(ctx context.Context, executionID string) error {
	if !probe.credential.IsSet() {
		return fmt.Errorf("the request-wait probe captured no execution credential")
	}
	if _, err := probe.inspect(ctx, probe.credential, executionID); err != nil {
		return fmt.Errorf("the captured execution credential's positive inspection was refused: %w", err)
	}
	return nil
}

// assertCapturedCredentialRefused ties the captured Secret to the pre-wait
// action's settled mounted-token fingerprint, then requires 401 unauthenticated
// while the original credential is still inside its recorded deadline.
func (probe *requestWaitProbe) assertCapturedCredentialRefused(ctx context.Context, executionID string, inspection api.ExecutionInspectResult) error {
	before, ok := resilienceAction(inspection, requestWaitBeforeName)
	if !ok || before.Deadline == nil {
		return fmt.Errorf("the pre-wait action carries no deadline to bound the refusal")
	}
	if !probe.credential.IsSet() {
		return fmt.Errorf("the request-wait probe captured no execution credential")
	}
	fingerprint := before.Outputs[requestWaitFingerprint]
	if fingerprint == "" {
		return fmt.Errorf("the pre-wait action recorded no mounted-credential fingerprint")
	}
	digest := sha256.Sum256([]byte(probe.credential.Reveal()))
	if hex.EncodeToString(digest[:]) != fingerprint {
		return fmt.Errorf("the captured execution credential is not the pre-wait action's mounted credential")
	}
	if !time.Now().UTC().Before(*before.Deadline) {
		return fmt.Errorf("the captured credential deadline already passed; the refusal would not prove wait-time revocation")
	}
	_, err := probe.inspect(ctx, probe.credential, executionID)
	if err := publicRefusalError(err, 401, api.CodeUnauthenticated); err != nil {
		return fmt.Errorf("the captured execution credential after request activation: %w", err)
	}
	return nil
}

// assertStaleServiceCredentialRefused requires 401 on both the inbox and the
// action-only resolve route for the revoked credential.
func (probe *requestWaitProbe) assertStaleServiceCredentialRefused(ctx context.Context, executionID string, item api.InboxItem) error {
	if !probe.revokedToken.IsSet() {
		return fmt.Errorf("the probe has no revoked service credential to test")
	}
	_, err := probe.Client.Get(ctx, probe.revokedToken, "task.inbox",
		api.TaskInboxRequest{ProjectID: probe.ProjectID}, "")
	if err := publicRefusalError(probe.refused("task.inbox", err), 401, api.CodeUnauthenticated); err != nil {
		return fmt.Errorf("the revoked service credential's task.inbox: %w", err)
	}
	_, err = probe.Client.Post(ctx, probe.revokedToken, "execution.request.resolve",
		probe.resolveRequest(executionID, item), "q02-rw-revoked-resolve")
	if err := publicRefusalError(probe.refused("execution.request.resolve", err), 401, api.CodeUnauthenticated); err != nil {
		return fmt.Errorf("the revoked service credential's execution.request.resolve: %w", err)
	}
	return nil
}

// assertRequestUnchanged re-inspects after the failed authority and requires
// the same unanswered request and unactivated successor.
func (probe *requestWaitProbe) assertRequestUnchanged(ctx context.Context, executionID string, prior api.ExecutionInspectResult, item api.InboxItem) error {
	inspection, err := probe.inspect(ctx, probe.AdminToken, executionID)
	if err != nil {
		return fmt.Errorf("the post-revocation inspection was refused: %w", err)
	}
	if inspection.State != "running" {
		return fmt.Errorf("the execution advanced to %q after refused authority", inspection.State)
	}
	decision, ok := resilienceAction(inspection, requestWaitDecisionName)
	if !ok {
		return fmt.Errorf("the post-revocation inspection has no request action")
	}
	if decision.State != "running" || decision.Version != item.Version {
		return fmt.Errorf("the refused resolution changed the request action")
	}
	if decision.Decision != "" || decision.Actor != "" || decision.AnsweredAt != nil {
		return fmt.Errorf("the refused resolution recorded a decision")
	}
	after, ok := resilienceAction(inspection, requestWaitAfterName)
	if !ok || after.State != "pending" || after.ActivatedAt != nil {
		return fmt.Errorf("the refused resolution advanced the successor")
	}
	before, _ := resilienceAction(inspection, requestWaitBeforeName)
	if before.Stop == nil || before.Stop.State != "confirmed" {
		return fmt.Errorf("the predecessor stop is no longer confirmed")
	}
	// The product keeps the completed predecessor Job and Pod until the
	// execution's terminal cleanup (internal/kernel/engine/cleanup.go:29-31)
	// or the Job TTL (cmd/kodama/production.go:343 pins 600 s), so their
	// continued presence is not evidence that anything advanced. The refused
	// resolution is proven inert by the absence of any live workload and of any
	// workload labelled for an action other than the confirmed predecessor.
	if err := probe.awaitSuccessorIdle(ctx, executionID, before.ActionID); err != nil {
		return err
	}
	if err := requestWaitRetainedWorkspace(ctx, probe.Cluster, probe.TenantNamespace, probe.workspaceID, prior); err != nil {
		return err
	}
	return nil
}

// requestWaitWorkloadObservation is the secret-free summary of one execution's
// workload objects at one instant.
type requestWaitWorkloadObservation struct {
	Pods      int
	LivePods  []string
	Jobs      int
	OtherActs []string
	Detail    string
}

// String renders the bounded observation for a timeout diagnostic.
func (observation requestWaitWorkloadObservation) String() string {
	text := fmt.Sprintf("pods %d jobs %d", observation.Pods, observation.Jobs)
	if len(observation.LivePods) != 0 {
		text += " live " + strings.Join(observation.LivePods, ",")
	}
	if len(observation.OtherActs) != 0 {
		text += " non-predecessor actions " + strings.Join(observation.OtherActs, ",")
	}
	if observation.Detail != "" {
		text += " last_error " + observation.Detail
	}
	return text
}

// awaitSuccessorIdle polls the cluster until every workload object attributable
// to the execution is terminal and labelled for the confirmed predecessor.
// A completed Job or Pod that the product retains is tolerated; a live Pod or
// any object labelled for another action is not.
func (probe *requestWaitProbe) awaitSuccessorIdle(ctx context.Context, executionID, predecessorActionID string) error {
	var observed requestWaitWorkloadObservation
	step := requestWaitStep{
		Name:    requestWaitStepSuccessorIdle,
		Observe: func() string { return observed.String() },
	}
	_, err := requestWaitPoll(ctx, step, probe.successorIdleWait(), func() (bool, error) {
		current, idle, err := requestWaitInspectWorkloads(ctx, probe.Cluster, probe.TenantNamespace, executionID, predecessorActionID)
		observed = current
		return idle, err
	})
	if err != nil {
		return fmt.Errorf(
			"the refused resolution did not leave the successor idle (execution %s, predecessor action %s): %w",
			executionID, predecessorActionID, err)
	}
	return nil
}

// requestWaitInspectWorkloads reads the pods and jobs attributable to one
// execution and reports whether they are all terminal and all labelled for the
// confirmed predecessor. Reads are bounded and failures are attributed to the
// step, never retried silently.
func requestWaitInspectWorkloads(ctx context.Context, cluster requestWaitCluster, namespace, executionID, predecessorActionID string) (requestWaitWorkloadObservation, bool, error) {
	var observation requestWaitWorkloadObservation
	selector := requestWaitLabelExecution + "=" + executionID
	other := map[string]bool{}
	for _, resource := range []string{"pods", "jobs"} {
		data, err := cluster.ObjectList(ctx, namespace, resource, selector)
		if err != nil {
			observation.Detail = "the " + resource + " were not readable"
			return observation, false, fmt.Errorf("step %q: the %s for execution %s were not readable: %w",
				requestWaitStepSuccessorIdle, resource, executionID, err)
		}
		objects, err := requestWaitObjectList(data)
		if err != nil {
			observation.Detail = "the " + resource + " list is malformed"
			return observation, false, fmt.Errorf("step %q: the %s list for execution %s is malformed: %w",
				requestWaitStepSuccessorIdle, resource, executionID, err)
		}
		switch resource {
		case "pods":
			observation.Pods = len(objects)
		case "jobs":
			observation.Jobs = len(objects)
		}
		for _, object := range objects {
			action := object.Labels[requestWaitLabelAction]
			if action != predecessorActionID {
				other[requestWaitActionLabel(action)] = true
			}
			if resource == "pods" && object.Phase != "Succeeded" && object.Phase != "Failed" {
				observation.LivePods = append(observation.LivePods,
					requestWaitActionLabel(action)+"="+requestWaitPhaseLabel(object.Phase))
			}
		}
	}
	for action := range other {
		observation.OtherActs = append(observation.OtherActs, action)
	}
	sort.Strings(observation.OtherActs)
	idle := len(observation.LivePods) == 0 && len(observation.OtherActs) == 0
	return observation, idle, nil
}

// requestWaitActionLabel renders one optional action label.
func requestWaitActionLabel(action string) string {
	if action == "" {
		return "<unlabelled>"
	}
	return action
}

// requestWaitPhaseLabel renders one optional pod phase.
func requestWaitPhaseLabel(phase string) string {
	if phase == "" {
		return "<empty>"
	}
	return phase
}

// rediscover reads the inbox pages with the fresh credential and finds the same
// request action.
func (probe *requestWaitProbe) rediscover(ctx context.Context, executionID, actionID string) (api.InboxItem, error) {
	items, err := probe.readInbox(ctx, probe.serviceToken)
	if err != nil {
		return api.InboxItem{}, fmt.Errorf("the fresh service credential's task.inbox was refused: %w", err)
	}
	item, ok := requestWaitFindRequest(items, executionID)
	if !ok || item.ActionID != actionID {
		return api.InboxItem{}, fmt.Errorf("the fresh service credential did not rediscover the same request")
	}
	return item, nil
}

// resolveRequest is the action-only request envelope: only the expected action
// version, the decision, the declared answer and no execution version.
func (probe *requestWaitProbe) resolveRequest(executionID string, item api.InboxItem) api.ExecutionRequestResolveRequest {
	return api.ExecutionRequestResolveRequest{
		ExecutionID:           executionID,
		ActionID:              item.ActionID,
		ExpectedActionVersion: item.Version,
		Decision:              "pass",
		Outputs:               map[string]string{requestWaitAnswerOutput: probe.Answer},
	}
}

// resolveWithKey posts one resolution with an exact idempotency key.
func (probe *requestWaitProbe) resolveWithKey(ctx context.Context, token Secret, executionID string, item api.InboxItem, key string) (api.ExecutionRequestResolveResult, error) {
	result, err := probe.Client.Post(ctx, token, "execution.request.resolve", probe.resolveRequest(executionID, item), key)
	if err != nil {
		return api.ExecutionRequestResolveResult{}, probe.refused("execution.request.resolve", err)
	}
	return decodeStageResult[api.ExecutionRequestResolveResult](result, "execution.request.resolve")
}

// assertRevokedReplayRefused replays the successful idempotency key with the
// revoked credential and requires 401.
func (probe *requestWaitProbe) assertRevokedReplayRefused(ctx context.Context, executionID string, item api.InboxItem) error {
	if probe.replayKey == "" {
		return fmt.Errorf("the probe recorded no successful resolve key")
	}
	_, err := probe.Client.Post(ctx, probe.revokedToken, "execution.request.resolve",
		probe.resolveRequest(executionID, item), probe.replayKey)
	if err := publicRefusalError(probe.refused("execution.request.resolve", err), 401, api.CodeUnauthenticated); err != nil {
		return fmt.Errorf("the revoked credential's replay of the successful key: %w", err)
	}
	return nil
}

// assertCompleted waits for the same execution to succeed and validates the
// recorded decision, the fresh post-wait action, the marker and cleanup.
func (probe *requestWaitProbe) assertCompleted(ctx context.Context, executionID string) error {
	var inspection api.ExecutionInspectResult
	step := requestWaitStep{
		Name: requestWaitStepTerminal,
		Observe: func() string {
			return "execution " + executionID + " " + requestWaitExecutionEvidence(inspection, true)
		},
	}
	_, err := requestWaitPoll(ctx, step, requestWaitTerminalTimeout, func() (bool, error) {
		current, err := probe.inspect(ctx, probe.AdminToken, executionID)
		if err != nil {
			return false, err
		}
		inspection = current
		return terminalExecutionState(current.State), nil
	})
	if err != nil {
		return fmt.Errorf("the request-wait execution did not reach a terminal state: %w", err)
	}
	probe.FinalInspection = inspection
	if inspection.State != "succeeded" {
		detail := "no failure was recorded"
		if inspection.Failure != nil {
			detail = inspection.Failure.Kind + ": " + inspection.Failure.Reason + " " + inspection.Failure.Detail
		}
		return fmt.Errorf("the request-wait execution state = %q (%s); want succeeded", inspection.State, detail)
	}
	decision, ok := resilienceAction(inspection, requestWaitDecisionName)
	if !ok {
		return fmt.Errorf("the terminal inspection has no request action")
	}
	if decision.State != "succeeded" || decision.Decision != "pass" {
		return fmt.Errorf("the request action state/decision = %q/%q; want succeeded/pass", decision.State, decision.Decision)
	}
	if decision.Actor != probe.ServiceID {
		return fmt.Errorf("the recorded request actor = %q; want the resolving service %q", decision.Actor, probe.ServiceID)
	}
	if decision.AnsweredAt == nil || decision.AnsweredAt.IsZero() {
		return fmt.Errorf("the request decision has no answered_at")
	}
	if decision.Outputs[requestWaitAnswerOutput] != probe.Answer {
		return fmt.Errorf("the recorded request answer is not the declared answer")
	}
	after, ok := resilienceAction(inspection, requestWaitAfterName)
	if !ok || after.State != "succeeded" {
		return fmt.Errorf("the post-wait action did not succeed with its fresh credential")
	}
	_, err = requestWaitPoll(ctx, requestWaitStep{
		Name: requestWaitStepCleanupSettle,
		Observe: func() string {
			return fmt.Sprintf("execution %s %s cleanup state %q attempts %d",
				executionID, requestWaitExecutionEvidence(inspection, true), inspection.Cleanup.State, inspection.Cleanup.Attempts)
		},
	}, requestWaitCleanupSettle, func() (bool, error) {
		current, err := probe.inspect(ctx, probe.AdminToken, executionID)
		if err != nil {
			return false, err
		}
		inspection = current
		return current.Cleanup.State == "complete", nil
	})
	if err != nil {
		return fmt.Errorf("the execution cleanup did not converge: %w", err)
	}
	if inspection.Cleanup.State != "complete" {
		return fmt.Errorf("the execution cleanup state = %q; want complete", inspection.Cleanup.State)
	}
	probe.FinalInspection = inspection
	marker := requestWaitMarker(probe.RunMarker)
	logs, err := probe.readLogs(ctx, executionID, after.ActionID)
	if err != nil {
		return fmt.Errorf("the post-wait logs were unreadable: %w", err)
	}
	probe.MarkerCount = strings.Count(string(logs), marker)
	if probe.MarkerCount != 1 {
		return fmt.Errorf("the completion marker occurs %d times; want exactly one", probe.MarkerCount)
	}
	for _, token := range []Secret{probe.credential, probe.serviceToken, probe.revokedToken} {
		if token.IsSet() && strings.Contains(string(logs), token.Reveal()) {
			return fmt.Errorf("the retained logs carry credential material")
		}
	}
	return nil
}

// inspect reads one execution with an explicit credential.
func (probe *requestWaitProbe) inspect(ctx context.Context, token Secret, executionID string) (api.ExecutionInspectResult, error) {
	result, err := probe.Client.Get(ctx, token, "execution.inspect", api.ExecutionInspectRequest{ExecutionID: executionID}, "")
	if err != nil {
		return api.ExecutionInspectResult{}, probe.refused("execution.inspect", err)
	}
	return decodeStageResult[api.ExecutionInspectResult](result, "execution.inspect")
}

// readInbox reads every inbox page with a bounded page count.
func (probe *requestWaitProbe) readInbox(ctx context.Context, token Secret) ([]api.InboxItem, error) {
	var items []api.InboxItem
	cursor := ""
	for range requestWaitMaxPages {
		request := api.TaskInboxRequest{ProjectID: probe.ProjectID, Page: &api.Page{Limit: requestWaitInboxPageLimit, Cursor: cursor}}
		result, err := probe.Client.Get(ctx, token, "task.inbox", request, "")
		if err != nil {
			return nil, probe.refused("task.inbox", err)
		}
		decoded, err := decodeStageResult[api.TaskInboxResult](result, "task.inbox")
		if err != nil {
			return nil, err
		}
		items = append(items, decoded.Items...)
		if decoded.NextCursor == "" {
			return items, nil
		}
		if decoded.NextCursor == cursor {
			return nil, fmt.Errorf("the inbox cursor did not advance")
		}
		cursor = decoded.NextCursor
	}
	return nil, fmt.Errorf("the inbox exceeded %d pages", requestWaitMaxPages)
}

// readLogs reads every retained stdout chunk for one action.
func (probe *requestWaitProbe) readLogs(ctx context.Context, executionID, actionID string) ([]byte, error) {
	var data []byte
	cursor := ""
	for range requestWaitMaxPages {
		request := api.ExecutionLogsReadRequest{
			ExecutionID: executionID,
			ActionID:    actionID,
			Stream:      "stdout",
			Page:        &api.Page{Limit: api.MaxPageLimit, Cursor: cursor},
		}
		result, err := probe.Client.Get(ctx, probe.AdminToken, "execution.logs.read", request, "")
		if err != nil {
			return nil, probe.refused("execution.logs.read", err)
		}
		decoded, err := decodeStageResult[api.ExecutionLogsReadResult](result, "execution.logs.read")
		if err != nil {
			return nil, err
		}
		for _, chunk := range decoded.Chunks {
			data = append(data, chunk.Data...)
		}
		if decoded.NextCursor == "" {
			return data, nil
		}
		if decoded.NextCursor == cursor {
			return nil, fmt.Errorf("the log cursor did not advance")
		}
		cursor = decoded.NextCursor
	}
	return nil, fmt.Errorf("the logs exceeded %d pages", requestWaitMaxPages)
}

// captureCredential captures exactly one credential Secret for the observed
// execution/action and validates its labels and mounted-secret association.
//
// The adapter writes the credential Secret at action activation, but
// execution.inspect can report the action running a moment before that write
// lands, so the capture polls a bounded window instead of failing on the first
// empty list. Only an execution/action-labelled Secret is accepted; the
// diagnostic names the selector keys and the observed counts, never a value.
func (probe *requestWaitProbe) captureCredential(ctx context.Context, executionID, actionID string) (requestWaitCredential, error) {
	selector := requestWaitLabelExecution + "=" + executionID + "," + requestWaitLabelAction + "=" + actionID
	var (
		objects  []requestWaitObject
		attempts int
		observed int
	)
	wait := probe.materializationWait()
	var lastErr string
	step := requestWaitStep{
		Name: requestWaitStepCredential,
		Observe: func() string {
			text := fmt.Sprintf("attempts %d matched %d", attempts, observed)
			if lastErr != "" {
				text += " last_error " + lastErr
			}
			return text
		},
	}
	_, err := requestWaitPollEvery(ctx, step, wait, probe.materializationPoll(), func() (bool, error) {
		attempts++
		data, err := probe.Cluster.ObjectList(ctx, probe.TenantNamespace, "secrets", selector)
		if err != nil {
			lastErr = "the execution credential Secret was not readable"
			return false, fmt.Errorf("the execution credential Secret was not readable: %w", err)
		}
		listed, err := requestWaitObjectList(data)
		if err != nil {
			return false, fmt.Errorf("the execution credential Secret list is malformed: %w", err)
		}
		observed = len(listed)
		switch observed {
		case 0:
			return false, nil
		case 1:
			objects = listed
			return true, nil
		default:
			return false, fmt.Errorf(
				"the execution/action selector matched %d Secrets; want exactly one (selector keys %s,%s)",
				observed, requestWaitLabelExecution, requestWaitLabelAction)
		}
	})
	if err != nil {
		if errors.Is(err, errRequestWaitPollExpired) {
			return requestWaitCredential{}, fmt.Errorf(
				"at step %q the execution/action selector matched %d Secrets after %d attempts within %s; want exactly one (execution %s action %s, selector keys %s,%s)",
				requestWaitStepCredential, observed, attempts, wait, executionID, actionID,
				requestWaitLabelExecution, requestWaitLabelAction)
		}
		return requestWaitCredential{}, fmt.Errorf("the execution credential Secret was not captured: %w", err)
	}
	if len(objects) != 1 {
		return requestWaitCredential{}, fmt.Errorf(
			"the execution/action selector matched %d Secrets after %d attempts; want exactly one (selector keys %s,%s)",
			len(objects), attempts, requestWaitLabelExecution, requestWaitLabelAction)
	}
	secret := objects[0]
	if err := requestWaitRequireLabels(secret.Labels, executionID, actionID); err != nil {
		return requestWaitCredential{}, err
	}
	raw, ok := secret.Data[requestWaitTokenKey]
	if !ok || len(raw) == 0 {
		return requestWaitCredential{}, fmt.Errorf("the captured Secret carries no %s execution credential", requestWaitTokenKey)
	}
	value := string(raw)
	token, err := stageCredential(&value, requestWaitExecutionPrefix)
	if err != nil {
		return requestWaitCredential{}, fmt.Errorf("the captured execution credential: %w", err)
	}
	workspace := secret.Labels[requestWaitLabelWorkspace]
	if workspace == "" {
		return requestWaitCredential{}, fmt.Errorf("the captured Secret carries no workspace label")
	}
	if err := probe.validateMountedSecret(ctx, executionID, actionID, secret); err != nil {
		return requestWaitCredential{}, err
	}
	return requestWaitCredential{Token: token, WorkspaceID: workspace, SecretName: secret.Name}, nil
}

// validateMountedSecret proves the pod of the same execution/action mounts the
// captured Secret at the reserved credential path, and that the Secret and pod
// agree on the workload and workspace labels the product writes for the same
// workload. The public action state exposes no workload identity, so the pod is
// the attributable source that validates the Secret's workload label.
func (probe *requestWaitProbe) validateMountedSecret(ctx context.Context, executionID, actionID string, secret requestWaitObject) error {
	workload := secret.Labels[requestWaitLabelWorkload]
	if workload == "" {
		return fmt.Errorf("the captured Secret carries no workload label")
	}
	selector := requestWaitLabelExecution + "=" + executionID + "," + requestWaitLabelAction + "=" + actionID
	data, err := probe.Cluster.ObjectList(ctx, probe.TenantNamespace, "pods", selector)
	if err != nil {
		return fmt.Errorf("the workload pod was not readable: %w", err)
	}
	objects, err := requestWaitObjectList(data)
	if err != nil {
		return fmt.Errorf("the workload pod list is malformed: %w", err)
	}
	if len(objects) != 1 {
		return fmt.Errorf("the execution/action selector matched %d pods; want exactly one (selector keys %s,%s)",
			len(objects), requestWaitLabelExecution, requestWaitLabelAction)
	}
	pod := objects[0]
	if err := requestWaitRequireLabels(pod.Labels, executionID, actionID); err != nil {
		return fmt.Errorf("the workload pod labels: %v", err)
	}
	if pod.Labels[requestWaitLabelWorkload] != workload {
		return fmt.Errorf("the credential Secret and workload pod name different workloads")
	}
	if pod.Labels[requestWaitLabelWorkspace] != secret.Labels[requestWaitLabelWorkspace] {
		return fmt.Errorf("the credential Secret and workload pod name different workspaces")
	}
	var (
		volumeName string
		mounted    bool
	)
	for _, volume := range pod.Volumes {
		if volume.SecretName == secret.Name {
			volumeName = volume.Name
		}
	}
	if volumeName == "" {
		return fmt.Errorf("the workload pod does not mount the captured credential Secret")
	}
	for _, mount := range pod.VolumeMounts {
		if mount.Name == volumeName && mount.MountPath == requestWaitSecretMountPath {
			mounted = true
		}
	}
	if !mounted {
		return fmt.Errorf("the workload does not mount the credential Secret at %s", requestWaitSecretMountPath)
	}
	return nil
}

// requestWaitCredential is one validated captured execution credential.
type requestWaitCredential struct {
	Token       Secret
	WorkspaceID string
	SecretName  string
}

// requestWaitObject is the credential-free metadata of one kubectl object.
type requestWaitObject struct {
	Name         string
	Labels       map[string]string
	Annotations  map[string]string
	Data         map[string][]byte
	Phase        string
	Volumes      []requestWaitVolume
	VolumeMounts []requestWaitMount
}

// requestWaitVolume is one pod volume with its optional Secret name.
type requestWaitVolume struct {
	Name       string
	SecretName string
}

// requestWaitMount is one container volume mount.
type requestWaitMount struct {
	Name      string
	MountPath string
}

// requestWaitObjectList decodes one bounded labelled object list.
func requestWaitObjectList(data []byte) ([]requestWaitObject, error) {
	if len(data) > 1<<20 {
		return nil, fmt.Errorf("the kubectl list exceeds the bound")
	}
	var wire struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Data   map[string][]byte `json:"data"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
			Spec struct {
				Volumes []struct {
					Name   string `json:"name"`
					Secret *struct {
						SecretName string `json:"secretName"`
					} `json:"secret"`
				} `json:"volumes"`
				Containers []struct {
					VolumeMounts []struct {
						Name      string `json:"name"`
						MountPath string `json:"mountPath"`
					} `json:"volumeMounts"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, fmt.Errorf("the kubectl list is not valid JSON")
	}
	objects := make([]requestWaitObject, 0, len(wire.Items))
	for _, item := range wire.Items {
		object := requestWaitObject{
			Name:        item.Metadata.Name,
			Labels:      item.Metadata.Labels,
			Annotations: item.Metadata.Annotations,
			Data:        item.Data,
			Phase:       item.Status.Phase,
		}
		for _, volume := range item.Spec.Volumes {
			entry := requestWaitVolume{Name: volume.Name}
			if volume.Secret != nil {
				entry.SecretName = volume.Secret.SecretName
			}
			object.Volumes = append(object.Volumes, entry)
		}
		for _, container := range item.Spec.Containers {
			for _, mount := range container.VolumeMounts {
				object.VolumeMounts = append(object.VolumeMounts, requestWaitMount{Name: mount.Name, MountPath: mount.MountPath})
			}
		}
		objects = append(objects, object)
	}
	sort.Slice(objects, func(left, right int) bool { return objects[left].Name < objects[right].Name })
	return objects, nil
}

// requestWaitRequireLabels validates the pinned execution/action labels.
func requestWaitRequireLabels(labels map[string]string, executionID, actionID string) error {
	if labels["kodama.dev/execution"] != executionID {
		return fmt.Errorf("the object is not labelled for the observed execution")
	}
	if labels["kodama.dev/action"] != actionID {
		return fmt.Errorf("the object is not labelled for the observed action")
	}
	return nil
}

// requestWaitAwaitNoWorkload waits briefly for workload deletion to follow the
// confirmed predecessor stop, then fails if any attributable workload remains.
// It is used only after terminal cleanup has completed, where the product
// removes the Job and its Pod (internal/kernel/engine/cleanup.go removeOwned);
// during a request wait the completed predecessor workload is still retained,
// so that interval is checked by awaitSuccessorIdle instead.
func requestWaitAwaitNoWorkload(ctx context.Context, cluster requestWaitCluster, namespace, executionID string) error {
	var last string
	step := requestWaitStep{
		Name:    requestWaitStepCleanupRemoval,
		Observe: func() string { return last },
	}
	_, err := requestWaitPoll(ctx, step, requestWaitRemovalWait, func() (bool, error) {
		err := requestWaitNoWorkload(ctx, cluster, namespace, executionID)
		if err != nil && strings.Contains(err.Error(), "were not readable") {
			return false, err
		}
		last = requestWaitNoWorkloadDetail(err)
		return err == nil, nil
	})
	if err != nil {
		return err
	}
	return nil
}

// requestWaitNoWorkloadDetail renders one no-workload refusal for a timeout
// diagnostic. The value is already execution-scoped and secret-free.
func requestWaitNoWorkloadDetail(err error) string {
	if err == nil {
		return "no Job or Pod remains"
	}
	return err.Error()
}

// requestWaitNoWorkload requires no live Job or pod attributable to the
// execution once its predecessor stop is confirmed.
func requestWaitNoWorkload(ctx context.Context, cluster requestWaitCluster, namespace, executionID string) error {
	selector := "kodama.dev/execution=" + executionID
	for _, resource := range []string{"jobs", "pods"} {
		data, err := cluster.ObjectList(ctx, namespace, resource, selector)
		if err != nil {
			return fmt.Errorf("the %s for execution %s were not readable: %w", resource, executionID, err)
		}
		objects, err := requestWaitObjectList(data)
		if err != nil {
			return fmt.Errorf("the %s list is malformed: %w", resource, err)
		}
		if len(objects) != 0 {
			return fmt.Errorf("execution %s still has live %s after cleanup", executionID, resource)
		}
	}
	return nil
}

// requestWaitRetainedWorkspace requires the pre-wait workspace claim to still
// exist and carry the retention marker.
func requestWaitRetainedWorkspace(ctx context.Context, cluster requestWaitCluster, namespace, workspaceID string, inspection api.ExecutionInspectResult) error {
	if workspaceID == "" {
		workspaceID = inspection.Cleanup.WorkspaceID
	}
	if workspaceID == "" {
		return fmt.Errorf("the request wait carries no workspace identity to prove retention")
	}
	data, err := cluster.ObjectList(ctx, namespace, "persistentvolumeclaims", "kodama.dev/workspace="+workspaceID)
	if err != nil {
		return fmt.Errorf("the workspace claim was not readable: %w", err)
	}
	objects, err := requestWaitObjectList(data)
	if err != nil {
		return fmt.Errorf("the workspace claim list is malformed: %w", err)
	}
	if len(objects) != 1 {
		return fmt.Errorf("the workspace selector matched %d claims; want one retained claim", len(objects))
	}
	claim := objects[0]
	if claim.Labels[requestWaitRetainedLabel] != "true" && claim.Annotations[requestWaitRetainedAnnotation] != "true" {
		return fmt.Errorf("the workspace claim is not marked retained across the request wait")
	}
	return nil
}

// requestWaitFindRequest finds the request inbox item for one execution.
func requestWaitFindRequest(items []api.InboxItem, executionID string) (api.InboxItem, bool) {
	for _, item := range items {
		if item.Kind == "request" && item.ExecutionID == executionID {
			return item, true
		}
	}
	return api.InboxItem{}, false
}

// errRequestWaitPollExpired marks a bounded poll that reached its deadline.
var errRequestWaitPollExpired = errors.New("the predicate did not hold within")

// requestWaitStep names one bounded wait and renders its last secret-free
// observation. Every poll in this probe names its step, so a live timeout says
// which predicate expired and what it last saw instead of only the duration.
type requestWaitStep struct {
	Name    string
	Observe func() string
}

// lastObserved renders the bounded last observation, or nothing.
func (step requestWaitStep) lastObserved() string {
	if step.Observe == nil {
		return ""
	}
	text := step.Observe()
	if text == "" {
		return ""
	}
	return ": last observed " + text
}

// requestWaitPoll runs one named bounded predicate poll at the standard
// interval.
func requestWaitPoll(ctx context.Context, step requestWaitStep, timeout time.Duration, attempt func() (bool, error)) (time.Duration, error) {
	return requestWaitPollEvery(ctx, step, timeout, requestWaitPollInterval, attempt)
}

// requestWaitPollEvery runs one named bounded predicate poll at an explicit
// interval. The expiry error names the step and the last observation.
func requestWaitPollEvery(ctx context.Context, step requestWaitStep, timeout, interval time.Duration, attempt func() (bool, error)) (time.Duration, error) {
	started := time.Now()
	deadline := started.Add(timeout)
	if interval <= 0 {
		interval = requestWaitPollInterval
	}
	for {
		if err := ctx.Err(); err != nil {
			return time.Since(started), err
		}
		ok, err := attempt()
		if err != nil {
			return time.Since(started), err
		}
		if ok {
			return time.Since(started), nil
		}
		if !time.Now().Before(deadline) {
			return time.Since(started), fmt.Errorf("%w %s at step %q%s",
				errRequestWaitPollExpired, timeout, step.Name, step.lastObserved())
		}
		select {
		case <-ctx.Done():
			return time.Since(started), ctx.Err()
		case <-time.After(interval):
		}
	}
}

// requestWaitCleanup is the strict, bounded cleanup wrapper. Unlike the older
// service probe's best-effort cleanup it never silently ignores a failure: every
// step is attempted and every failure is reported.
type requestWaitCleanup struct {
	probe       *requestWaitProbe
	executionID string
	workspaceID string
	serviceID   string
	version     uint64
	token       Secret
	disabled    bool
	ran         bool
	// settle overrides the bounded cleanup wait when a test pins a short one.
	settle time.Duration
	// pending overrides the bounded Pending-pod wait before cancellation.
	pending time.Duration
	// authority overrides the bounded service-authority teardown window.
	authority time.Duration
	// workload overrides the bounded workload-cleanup window.
	workload time.Duration
}

// newRequestWaitCleanup builds the cleanup and binds it to the probe.
func newRequestWaitCleanup(probe *requestWaitProbe) *requestWaitCleanup {
	cleanup := &requestWaitCleanup{probe: probe}
	probe.Cleanup = cleanup
	return cleanup
}

// bindService copies the probe's current service identity, version and token.
// Between revoke and reissue it keeps the revoked token so cleanup can still
// verify the 401 refusal.
func (cleanup *requestWaitCleanup) bindService(probe *requestWaitProbe) {
	cleanup.serviceID = probe.ServiceID
	cleanup.version = probe.serviceVersion
	if probe.serviceToken.IsSet() {
		cleanup.token = probe.serviceToken
	} else if probe.revokedToken.IsSet() {
		cleanup.token = probe.revokedToken
	}
}

// settleWindow returns the bounded cleanup wait.
func (cleanup *requestWaitCleanup) settleWindow() time.Duration {
	if cleanup.settle > 0 {
		return cleanup.settle
	}
	return requestWaitCleanupSettle
}

// pendingWindow returns the bounded Pending-pod wait before cancellation.
func (cleanup *requestWaitCleanup) pendingWindow() time.Duration {
	if cleanup.pending > 0 {
		return cleanup.pending
	}
	return requestWaitCleanupPendingWait
}

// authorityWindow returns the bounded service-authority teardown window.
func (cleanup *requestWaitCleanup) authorityWindow() time.Duration {
	if cleanup.authority > 0 {
		return cleanup.authority
	}
	return requestWaitCleanupAuthority
}

// workloadWindow returns the bounded workload-cleanup window.
func (cleanup *requestWaitCleanup) workloadWindow() time.Duration {
	if cleanup.workload > 0 {
		return cleanup.workload
	}
	return requestWaitCleanupTimeout
}

// awaitCleanup waits for cleanup.state=complete. A cleanup that never converges
// is reported with the exact execution and the cleanup evidence that remained
// (state, workload, workspace, attempts, last public error), so a stuck cleanup
// is attributable to the product instead of to the probe.
func (cleanup *requestWaitCleanup) awaitCleanup(ctx context.Context, phase string) error {
	var observed api.ExecutionInspectResult
	settle := cleanup.settleWindow()
	probe := cleanup.probe
	step := requestWaitStep{
		Name: requestWaitStepExecutionCleanup,
		Observe: func() string {
			return fmt.Sprintf("execution %s %s cleanup state %q workload %q workspace %q attempts %d last_error %q",
				cleanup.executionID, requestWaitExecutionEvidence(observed, true), observed.Cleanup.State,
				observed.Cleanup.WorkloadID, observed.Cleanup.WorkspaceID, observed.Cleanup.Attempts,
				requestWaitPublicDetail(observed.Cleanup.LastError, requestWaitEvidenceDetailBytes))
		},
	}
	_, err := requestWaitPoll(ctx, step, settle, func() (bool, error) {
		current, inspectErr := probe.inspect(ctx, probe.AdminToken, cleanup.executionID)
		if inspectErr != nil {
			return false, inspectErr
		}
		observed = current
		return current.Cleanup.State == "complete", nil
	})
	if err == nil {
		return nil
	}
	return fmt.Errorf(
		"execution %s cleanup did not converge %s within %s: %w; last observed execution state %q version %d cleanup state %q workload %q workspace %q attempts %d last_error %q",
		cleanup.executionID, phase, settle, err, observed.State, observed.Version,
		observed.Cleanup.State, observed.Cleanup.WorkloadID, observed.Cleanup.WorkspaceID,
		observed.Cleanup.Attempts,
		requestWaitPublicDetail(observed.Cleanup.LastError, requestWaitEvidenceDetailBytes))
}

// run tears the service authority down FIRST on its own bounded context and
// only then cancels the execution and runs the workload-cleanup waits on their
// own context, so a cleanup that never converges (the known F-4 shape) can
// never spend the budget before the service credential is revoked and the
// service disabled. The two phases are independent: a refused revoke still
// lets cancel and the workload waits run, and a failed cancel or workload wait
// never prevents the revoke, disable and refusal check. It is idempotent and
// always attempts every step.
func (cleanup *requestWaitCleanup) run(ctx context.Context) error {
	if cleanup == nil || cleanup.probe == nil {
		return fmt.Errorf("the request-wait cleanup is not configured")
	}
	if cleanup.ran {
		return nil
	}
	cleanup.ran = true
	var failures []string
	add := func(format string, args ...any) {
		failures = append(failures, fmt.Sprintf(format, args...))
	}
	probe := cleanup.probe
	if cleanup.serviceID == "" {
		cleanup.serviceID = probe.ServiceID
	}
	if cleanup.version == 0 {
		cleanup.version = probe.serviceVersion
	}
	if cleanup.executionID == "" {
		cleanup.executionID = probe.Cleanup.executionID
	}
	if cleanup.workspaceID == "" {
		cleanup.workspaceID = probe.Cleanup.workspaceID
	}

	authorityCtx, cancelAuthority := context.WithTimeout(ctx, cleanup.authorityWindow())
	cleanup.teardownService(authorityCtx, add)
	cancelAuthority()

	workloadCtx, cancelWorkload := context.WithTimeout(ctx, cleanup.workloadWindow())
	cleanup.teardownExecution(workloadCtx, add)
	cancelWorkload()

	if len(failures) != 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}

// teardownService revokes the current service credential, disables the service
// and verifies the revoked token is refused. Both mutations are attempted even
// when one fails, and the verification runs even when the mutations failed, so
// the cleanup never reports success while a service credential stays live.
func (cleanup *requestWaitCleanup) teardownService(ctx context.Context, add func(string, ...any)) {
	probe := cleanup.probe
	if cleanup.serviceID != "" && !cleanup.disabled {
		if cleanup.version == 0 {
			add("the cleanup has no service version to revoke or disable")
		} else {
			key, err := probeIdempotencyKey("q02-rw-cleanup-revoke")
			if err == nil {
				result, revokeErr := probe.Client.DeleteVersioned(ctx, probe.AdminToken, "service.credential.revoke",
					api.ServiceCredentialRevokeRequest{ServiceID: cleanup.serviceID}, key, cleanup.version)
				if revokeErr != nil {
					add("cleanup service.credential.revoke failed: %v", probe.refused("service.credential.revoke", revokeErr))
				} else if revoked, decodeErr := decodeStageResult[api.ServiceCredentialRevokeResult](result, "service.credential.revoke"); decodeErr != nil {
					add("cleanup service.credential.revoke returned an invalid result: %v", decodeErr)
				} else {
					if revoked.Invalidated == nil || !*revoked.Invalidated {
						add("cleanup service.credential.revoke reported no invalidation")
					}
					cleanup.version = revoked.Version
				}
			} else {
				add("generate the cleanup revoke key: %v", err)
			}
			key, err = probeIdempotencyKey("q02-rw-cleanup-disable")
			if err == nil {
				result, disableErr := probe.Client.PostVersioned(ctx, probe.AdminToken, "service.disable",
					api.ServiceDisableRequest{ServiceID: cleanup.serviceID}, key, cleanup.version)
				if disableErr != nil {
					add("cleanup service.disable failed: %v", probe.refused("service.disable", disableErr))
				} else if disabled, decodeErr := decodeStageResult[api.ServiceDisableResult](result, "service.disable"); decodeErr != nil {
					add("cleanup service.disable returned an invalid result: %v", decodeErr)
				} else {
					if disabled.State != "disabled" {
						add("cleanup service.disable reported state %q", disabled.State)
					}
					cleanup.version = disabled.Version
					cleanup.disabled = true
				}
			} else {
				add("generate the cleanup disable key: %v", err)
			}
		}
	}
	if cleanup.serviceID != "" && cleanup.token.IsSet() {
		if _, err := probe.Client.Get(ctx, cleanup.token, "execution.list",
			api.ExecutionListRequest{ProjectID: probe.ProjectID}, ""); err == nil {
			add("the revoked/disabled service credential still reads execution.list")
		} else {
			labelled := probe.refused("execution.list", err)
			if refusal := publicRefusalError(labelled, 401, api.CodeUnauthenticated); refusal != nil {
				add("the cleanup credential's refusal was not 401 unauthenticated (%v): %v", labelled, refusal)
			}
		}
	}
}

// teardownExecution cancels a nonterminal execution, waits for its cleanup,
// requires the workload removed and checks any surviving workspace claim. Every
// step is attempted and every failure is reported.
func (cleanup *requestWaitCleanup) teardownExecution(ctx context.Context, add func(string, ...any)) {
	if cleanup.executionID == "" {
		return
	}
	probe := cleanup.probe
	inspection, err := probe.inspect(ctx, probe.AdminToken, cleanup.executionID)
	if err != nil {
		add("inspect before cancellation failed: %v", err)
	} else if !terminalExecutionState(inspection.State) {
		expectWorkload := false
		for _, action := range inspection.Actions {
			if action.Kind == "container" && action.State == "running" {
				expectWorkload = true
				break
			}
		}
		requestWaitAwaitPendingWorkload(ctx, probe.Cluster, probe.TenantNamespace, cleanup.executionID, expectWorkload, cleanup.pendingWindow())
		if err := requestWaitCancel(ctx, probe, cleanup.executionID, inspection.Version); err != nil {
			add("execution.cancel failed: %v", err)
		} else if err := cleanup.awaitCleanup(ctx, "after execution.cancel"); err != nil {
			add("%v", err)
		}
	} else if inspection.Cleanup.State != "complete" {
		if err := cleanup.awaitCleanup(ctx, "on the already terminal execution"); err != nil {
			add("%v", err)
		}
	}
	if err := requestWaitAwaitNoWorkload(ctx, probe.Cluster, probe.TenantNamespace, cleanup.executionID); err != nil {
		add("%v", err)
	}
	if err := requestWaitCleanupWorkspace(ctx, probe.Cluster, probe.TenantNamespace, cleanup.workspaceID); err != nil {
		add("%v", err)
	}
}

// requestWaitAwaitPendingWorkload gives a just-activated workload pod a bounded
// chance to appear and leave Pending before cleanup cancels the execution.
// Cancelling a Pod that never started deletes it inside the same second, before
// the product can record a confirmed stop, so the cleanup would report F-4's
// "0 workload pods observed" for a probe failure that was not the pod's fault.
// expectWorkload is true while the inspection shows a running container action,
// so an empty list is treated as not-yet-materialized rather than as absent.
// The wait is best-effort: a read failure or an exhausted bound proceeds to the
// cancel, and the cancel/cleanup evidence stays attributable to the execution.
func requestWaitAwaitPendingWorkload(ctx context.Context, cluster requestWaitCluster, namespace, executionID string, expectWorkload bool, timeout time.Duration) {
	if cluster == nil || executionID == "" || timeout <= 0 {
		return
	}
	selector := requestWaitLabelExecution + "=" + executionID
	var observed int
	lastError := ""
	_, _ = requestWaitPollEvery(ctx, requestWaitStep{
		Name: requestWaitStepPendingWorkload,
		Observe: func() string {
			return fmt.Sprintf("pods %d last_error %q", observed, lastError)
		},
	}, timeout, requestWaitCleanupPoll, func() (bool, error) {
		data, err := cluster.ObjectList(ctx, namespace, "pods", selector)
		if err != nil {
			lastError = "the pods were not readable"
			return true, nil
		}
		objects, err := requestWaitObjectList(data)
		if err != nil {
			return true, nil
		}
		observed = len(objects)
		if len(objects) == 0 {
			return !expectWorkload, nil
		}
		for _, pod := range objects {
			if pod.Phase != "Pending" {
				return true, nil
			}
		}
		return false, nil
	})
}

// requestWaitCancel cancels one nonterminal execution at its observed version.
func requestWaitCancel(ctx context.Context, probe *requestWaitProbe, executionID string, version uint64) error {
	key, err := probeIdempotencyKey("q02-rw-cancel")
	if err != nil {
		return err
	}
	result, err := probe.Client.PostVersioned(ctx, probe.AdminToken, "execution.cancel",
		api.ExecutionCancelRequest{ExecutionID: executionID}, key, version)
	if err != nil {
		return probe.refused("execution.cancel", err)
	}
	_, err = decodeStageResult[api.ExecutionCancelResult](result, "execution.cancel")
	return err
}

// requestWaitCleanupWorkspace requires any surviving workspace claim to be a
// retained claim, never an unattributed orphan.
func requestWaitCleanupWorkspace(ctx context.Context, cluster requestWaitCluster, namespace, workspaceID string) error {
	if workspaceID == "" {
		return nil
	}
	data, err := cluster.ObjectList(ctx, namespace, "persistentvolumeclaims", "kodama.dev/workspace="+workspaceID)
	if err != nil {
		return fmt.Errorf("the cleanup workspace claim was not readable: %w", err)
	}
	objects, err := requestWaitObjectList(data)
	if err != nil {
		return fmt.Errorf("the cleanup workspace claim list is malformed: %w", err)
	}
	for _, claim := range objects {
		if claim.Labels[requestWaitRetainedLabel] != "true" && claim.Annotations[requestWaitRetainedAnnotation] != "true" {
			return fmt.Errorf("the cleanup left an unreleased workspace claim")
		}
	}
	return nil
}

// evidence returns the allowlisted record. Every issued credential and the
// captured execution credential are registered with the writer.
func (probe *requestWaitProbe) evidence(started, finished time.Time, executionID string) stageEvidence {
	secrets := make([]Secret, 0, 4)
	seen := map[Secret]bool{}
	for _, token := range []Secret{probe.revokedToken, probe.freshToken, probe.serviceToken, probe.credential} {
		if token.IsSet() && !seen[token] {
			seen[token] = true
			secrets = append(secrets, token)
		}
	}
	return stageEvidence{
		Name:         "stage-request-wait-revocation",
		Probe:        "TestStageRequestWaitRevocation",
		Started:      started,
		Finished:     finished,
		ExecutionID:  executionID,
		ResourceID:   probe.ServiceID,
		ExtraSecrets: secrets,
	}
}
