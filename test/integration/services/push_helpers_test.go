//go:build integration

package services

// Shared fixtures for the push-trigger scenarios.
//
// Every scenario composes the real factory with the optional host Git service
// and the real OIDC verifier, then drives the public edge: a real Git push
// verifies, journals and reports a ref update, the Shell commits a
// resource.verified event, and the real trigger processor selects and starts
// the subscribed pipeline. Nothing here synthesizes a report row, a selection,
// a receipt or an execution.
//
// Two explicitly labelled fault-injection seams support the scheduled cases:
//
//   - reportHold wraps only the Git service's outbound HTTP transport and
//     holds one /api/v1/reports call, so a report is genuinely late without
//     losing the verified effect.
//   - attemptResolver wraps the composition's image
//     resolver, so one admission attempt can fail transiently or pause at the
//     registry boundary. It never resolves an image the product would refuse.
//
// All other observation is durable state read through the served operations or
// the fixture's documented read-only evidence helpers.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/shell/integration/oidc"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/userland/git"
	identityfixture "github.com/optimiware/kodama/test/support/boundaries/identity"
	"github.com/optimiware/kodama/test/support/factory"
)

const (
	// pushEngineerSubject is the OIDC subject of the one human these push
	// scenarios exercise. Authority is the committed project membership.
	pushEngineerSubject = "push-engineer"

	// pushWorkflowTimeout bounds one wait for a durable fact the served push
	// path must produce.
	pushWorkflowTimeout = 5 * time.Minute

	// pushCommandTimeout bounds one Git client command. The late-report case
	// holds the post-receive report inside this budget.
	pushCommandTimeout = 3 * time.Minute
)

// pushHarness is one real factory composition with the host Git service, the
// OIDC engineer and the public read helpers the push scenarios need.
type pushHarness struct {
	*factory.Harness

	git               *factory.GitFixture
	engineerToken     string
	engineerPrincipal string
}

// startPushHarness starts the Git composition and grants its OIDC engineer the
// project engineer role through the served membership operation.
func startPushHarness(t *testing.T, options ...factory.Option) *pushHarness {
	t.Helper()
	selected := append([]factory.Option{factory.WithGitService(), factory.WithOIDC()}, options...)
	harness := factory.Start(t, selected...)
	gitFixture := harness.Git
	if gitFixture == nil {
		t.Fatalf("factory.WithGitService did not expose a Git fixture")
	}
	token := harness.OIDC.Mint(t, identityfixture.TokenSpec{Subject: pushEngineerSubject})
	principal, err := oidc.PrincipalIDFor(harness.OIDC.Issuer, pushEngineerSubject)
	if err != nil {
		t.Fatalf("derive engineer principal: %v", err)
	}
	harness.MustREST(harness.AdminToken, "membership.grant", api.MembershipGrantRequest{
		ProjectID:   factory.ProjectID,
		PrincipalID: string(principal),
		Roles:       []string{"engineer"},
	}, nil, "grant-"+pushEngineerSubject)
	return &pushHarness{
		Harness:           harness,
		git:               gitFixture,
		engineerToken:     token,
		engineerPrincipal: string(principal),
	}
}

// createPushService creates one enabled automatic-start service with the
// engineer role through the served operation.
func (fixture *pushHarness) createPushService(t *testing.T, serviceID string) {
	t.Helper()
	fixture.MustREST(fixture.ProjectToken, "service.create", api.ServiceCreateRequest{
		ProjectID: factory.ProjectID,
		ServiceID: serviceID,
		Roles:     []string{"engineer"},
	}, nil, "create-"+serviceID)
}

// clone clones the fixture project repository with one actor credential.
func (fixture *pushHarness) clone(t *testing.T, token, name string) string {
	t.Helper()
	directory := filepath.Join(t.TempDir(), name)
	stderr, code := fixture.git.Clone(t, token, factory.GitProjectRepository, directory)
	if code != 0 {
		t.Fatalf("clone %s: exit %d: %s", factory.GitProjectRepository, code, stderr)
	}
	return directory
}

// commit writes one deterministic commit on the checked-out branch.
func (fixture *pushHarness) commit(t *testing.T, directory, message string) string {
	t.Helper()
	return fixture.git.Commit(t, directory, message)
}

// push pushes one refspec with the given credential and returns the stderr and
// exit code so a refusal can be asserted without failing the test.
func (fixture *pushHarness) push(directory, token string, refspecs ...string) (string, int) {
	ctx, cancel := context.WithTimeout(context.Background(), pushCommandTimeout)
	defer cancel()
	return fixture.git.Push(ctx, directory, token, refspecs...)
}

// readPushBinding reads one registered binding through the public operation.
func (fixture *pushHarness) readPushBinding(t *testing.T, bindingID string) api.ResourceBindingReadResult {
	t.Helper()
	result := fixture.MustREST(fixture.ProjectToken, "resource.binding.read", api.ResourceBindingReadRequest{
		BindingID: bindingID,
	}, nil, "")
	var binding api.ResourceBindingReadResult
	fixture.Decode(result, &binding)
	return binding
}

// waitPushJournal waits for the final durable journal entry of one accepted ref
// update.
func (fixture *pushHarness) waitPushJournal(t *testing.T, ref, candidate string) git.JournalEntry {
	t.Helper()
	var found git.JournalEntry
	fixture.git.WaitFor(t,
		fmt.Sprintf("journal entry ref=%s candidate=%s state=%s", ref, candidate, git.JournalReported),
		pushWorkflowTimeout, func() bool {
			for _, entry := range fixture.git.JournalEntries(t) {
				if entry.Ref == ref && entry.Candidate == candidate && entry.State == git.JournalReported {
					found = entry
					return true
				}
			}
			return false
		})
	return found
}

// waitPushJournalState waits for one exact journal state of a candidate.
func (fixture *pushHarness) waitPushJournalState(t *testing.T, ref, candidate string, state git.JournalState) git.JournalEntry {
	t.Helper()
	var found git.JournalEntry
	fixture.git.WaitFor(t,
		fmt.Sprintf("journal entry ref=%s candidate=%s state=%s", ref, candidate, state),
		pushWorkflowTimeout, func() bool {
			for _, entry := range fixture.git.JournalEntries(t) {
				if entry.Ref == ref && entry.Candidate == candidate && entry.State == state {
					found = entry
					return true
				}
			}
			return false
		})
	return found
}

// waitPushReportEvent waits for the durable resource.verified event caused by
// one verification.
func (fixture *pushHarness) waitPushReportEvent(t *testing.T, verificationID string) factory.EventRow {
	t.Helper()
	var found factory.EventRow
	fixture.git.WaitFor(t,
		fmt.Sprintf("resource.verified event caused by verification=%s", verificationID),
		pushWorkflowTimeout, func() bool {
			event, ok, err := fixture.git.ReportEvent(t.Context(), verificationID)
			if err != nil {
				return false
			}
			if ok {
				found = event
				return true
			}
			return false
		})
	return found
}

// pushVerification returns the durable verification row of one identity.
func (fixture *pushHarness) pushVerification(t *testing.T, verificationID string) factory.VerificationRow {
	t.Helper()
	var found factory.VerificationRow
	fixture.git.WaitFor(t,
		fmt.Sprintf("verification row %s", verificationID),
		pushWorkflowTimeout, func() bool {
			rows, err := fixture.git.Verifications(t.Context())
			if err != nil {
				return false
			}
			for _, row := range rows {
				if row.ID == verificationID {
					found = row
					return true
				}
			}
			return false
		})
	return found
}

// pushPipelineDocument renders one API-posted push pipeline whose single
// container action proves the admitted repository, ref and commit values.
func pushPipelineDocument(repository, service, image, ref, commit string) []byte {
	return pushPipelineDocumentRefs(repository, service, image, []string{ref}, commit)
}

// pushPipelineDocumentRefs renders the same proving pipeline with several
// on.push refs.
func pushPipelineDocumentRefs(repository, service, image string, refs []string, commit string) []byte {
	document := pushPipelineDoc{
		Repository: repository,
		Service:    service,
		On:         &pushTriggerDoc{Push: append([]string(nil), refs...)},
		Inputs:     []string{"repository", "ref", "commit", "previous_commit"},
		Actions: []pushActionDoc{{
			Name:    "check",
			Image:   image,
			Timeout: "10m",
			Commands: []string{
				fmt.Sprintf(`test "$(cat /kodama/inputs/repository)" = %s`, repository),
				fmt.Sprintf(`test "$(cat /kodama/inputs/ref)" = %s`, refs[0]),
				fmt.Sprintf(`test "$(cat /kodama/inputs/commit)" = %s`, commit),
			},
		}},
	}
	return pushMarshal(document)
}

// pushPipelineDoc is one API-posted push pipeline document. The repository,
// service and on members are omitted for a plain pipeline, exactly like the
// authored documents delivery and pusher fixtures use.
type pushPipelineDoc struct {
	Repository string          `yaml:"repository,omitempty"`
	Service    string          `yaml:"service,omitempty"`
	On         *pushTriggerDoc `yaml:"on,omitempty"`
	Inputs     []string        `yaml:"inputs,omitempty"`
	Actions    []pushActionDoc `yaml:"actions"`
}

// pushTriggerDoc is the authored on.push subscription.
type pushTriggerDoc struct {
	Push []string `yaml:"push"`
}

// pushActionDoc is one container action of a push pipeline.
type pushActionDoc struct {
	Name     string   `yaml:"name"`
	Image    string   `yaml:"image"`
	Commands []string `yaml:"commands"`
	Timeout  string   `yaml:"timeout,omitempty"`
}

// pushMarshal renders one exact pipeline YAML byte sequence.
func pushMarshal(document pushPipelineDoc) []byte {
	encoded, err := yaml.Marshal(document)
	if err != nil {
		panic(fmt.Sprintf("marshal push pipeline yaml: %v", err))
	}
	return encoded
}

// applyPushPipeline posts one pipeline through the public API.
func (fixture *pushHarness) applyPushPipeline(t *testing.T, name string, document []byte) api.PipelineApplyResult {
	t.Helper()
	result := fixture.MustREST(fixture.ProjectToken, "pipeline.apply", api.PipelineApplyRequest{
		ProjectID: factory.ProjectID,
		Name:      name,
		YAML:      document,
	}, nil, "apply-"+name)
	var applied api.PipelineApplyResult
	fixture.Decode(result, &applied)
	return applied
}

// updatePushPipeline updates one stored pipeline at an explicit version. It
// returns the committed version.
func (fixture *pushHarness) updatePushPipeline(t *testing.T, pipelineID, name string, document []byte, version uint64, key string) uint64 {
	t.Helper()
	result := fixture.MustREST(fixture.ProjectToken, "pipeline.update", api.PipelineUpdateRequest{
		ProjectID:  factory.ProjectID,
		PipelineID: pipelineID,
		Name:       name,
		YAML:       document,
	}, &version, key)
	var updated api.PipelineUpdateResult
	fixture.Decode(result, &updated)
	return updated.Version
}

// pushPipelineExecutions lists the executions of one pipeline.
func (fixture *pushHarness) pushPipelineExecutions(t *testing.T, pipelineID string) []api.ExecutionSummary {
	t.Helper()
	result := fixture.MustREST(fixture.ProjectToken, "execution.list", api.ExecutionListRequest{
		ProjectID:  factory.ProjectID,
		PipelineID: pipelineID,
	}, nil, "")
	var list api.ExecutionListResult
	fixture.Decode(result, &list)
	return list.Executions
}

// waitPushExecutionCount waits until one pipeline has exactly the expected
// number of executions and returns them.
func (fixture *pushHarness) waitPushExecutionCount(t *testing.T, pipelineID string, want int) []api.ExecutionSummary {
	t.Helper()
	var found []api.ExecutionSummary
	fixture.git.WaitFor(t,
		fmt.Sprintf("pipeline %s has %d executions", pipelineID, want),
		pushWorkflowTimeout, func() bool {
			current := fixture.pushPipelineExecutions(t, pipelineID)
			if len(current) != want {
				return false
			}
			found = current
			return true
		})
	return found
}

// waitPushExecutionSucceeded waits for one execution to reach the succeeded
// state and returns its final inspection.
func (fixture *pushHarness) waitPushExecutionSucceeded(t *testing.T, executionID string) api.ExecutionInspectResult {
	t.Helper()
	var found api.ExecutionInspectResult
	fixture.git.WaitFor(t,
		fmt.Sprintf("execution %s succeeded", executionID),
		pushWorkflowTimeout, func() bool {
			result, publicError := fixture.REST(fixture.ProjectToken, "execution.inspect", api.ExecutionInspectRequest{
				ExecutionID: executionID,
			}, nil, "")
			if publicError.Code != "" {
				return false
			}
			var current api.ExecutionInspectResult
			fixture.Decode(result, &current)
			switch current.State {
			case "succeeded":
				found = current
				return true
			case "failed", "cancelled":
				t.Fatalf("execution %s ended %s: %+v", executionID, current.State, current.Failure)
			}
			return false
		})
	return found
}

// startPushExecution starts one blocking container action and returns the
// execution identity and its live credential.
func (fixture *pushHarness) startPushExecution(t *testing.T, name string) (string, string) {
	t.Helper()
	applied := fixture.applyPushPipeline(t, name, pushMarshal(pushPipelineDoc{
		Actions: []pushActionDoc{{
			Name:    "work",
			Image:   fixture.Image.Reference,
			Timeout: "1h",
			Commands: []string{
				`while [ ! -e /workspace/release ]; do sleep 1; done`,
			},
		}},
	}))
	started := fixture.pushStart(t, applied.PipelineID, "start-"+name)
	t.Cleanup(func() { fixture.stopPushExecution(t, started.ExecutionID) })

	var credential string
	fixture.git.WaitFor(t,
		fmt.Sprintf("execution credential for execution=%s action=work", started.ExecutionID),
		2*time.Minute, func() bool {
			value, err := fixture.ExecutionCredential(t.Context(), started.ExecutionID, "work")
			if err != nil {
				return false
			}
			credential = strings.TrimSpace(value)
			return credential != ""
		})
	return started.ExecutionID, credential
}

// pushStart starts one stored pipeline at its currently observed version.
func (fixture *pushHarness) pushStart(t *testing.T, pipelineID, key string) api.PipelineStartResult {
	t.Helper()
	stored := readPipeline(t, fixture.Harness, pipelineID)
	if stored.Version == 0 {
		t.Fatalf("pipeline.read %s version = 0", pipelineID)
	}
	result := fixture.MustREST(fixture.ProjectToken, "pipeline.start", api.PipelineStartRequest{
		ProjectID:  factory.ProjectID,
		PipelineID: pipelineID,
	}, &stored.Version, key)
	var started api.PipelineStartResult
	fixture.Decode(result, &started)
	return started
}

// stopPushExecution cancels one execution through the public operation and
// waits for a terminal state. It never fails the test from cleanup.
func (fixture *pushHarness) stopPushExecution(t *testing.T, executionID string) {
	t.Helper()
	for range 40 {
		result, publicError := fixture.REST(fixture.AdminToken, "execution.inspect", api.ExecutionInspectRequest{
			ExecutionID: executionID,
		}, nil, "")
		if publicError.Code != "" {
			return
		}
		var current api.ExecutionInspectResult
		fixture.Decode(result, &current)
		switch current.State {
		case "succeeded", "failed", "cancelled":
			return
		}
		_, publicError = fixture.REST(fixture.AdminToken, "execution.cancel", api.ExecutionCancelRequest{
			ExecutionID: executionID,
		}, &current.Version, "cancel-"+executionID)
		if publicError.Code == api.CodeVersionConflict {
			continue
		}
		if publicError.Code != "" {
			return
		}
	}
}

// queuedEvents returns the committed execution.queued events for one execution.
func (fixture *pushHarness) queuedEvents(t *testing.T, executionID string) []api.Event {
	t.Helper()
	var found []api.Event
	fixture.git.WaitFor(t,
		fmt.Sprintf("execution.queued event for execution=%s", executionID),
		pushWorkflowTimeout, func() bool {
			events := readProjectEvents(t, fixture.Harness, "execution.queued")
			found = nil
			for _, event := range events.Events {
				if event.SubjectID == executionID {
					found = append(found, event)
				}
			}
			return len(found) > 0
		})
	return found
}

// executionSnapshotYAML reads one execution's exact admitted YAML bytes.
func (fixture *pushHarness) executionSnapshotYAML(t *testing.T, executionID string) []byte {
	t.Helper()
	yamlBytes, _, err := fixture.ExecutionRow(t.Context(), executionID)
	if err != nil {
		t.Fatalf("read admitted yaml of execution %s: %v", executionID, err)
	}
	return yamlBytes
}

// reportHold is a labelled transport seam over the Git service's report call.
// It holds the first /api/v1/reports request until Release or context
// cancellation. It never rewrites a request or fabricates a response.
type reportHold struct {
	mu          sync.Mutex
	armed       bool
	reached     chan struct{}
	release     chan struct{}
	reachedOnce sync.Once
	releaseOnce sync.Once
}

// newReportHold returns one unarmed hold.
func newReportHold() *reportHold {
	return &reportHold{reached: make(chan struct{}), release: make(chan struct{})}
}

// arm holds the next report request.
func (hold *reportHold) arm() {
	hold.mu.Lock()
	hold.armed = true
	hold.mu.Unlock()
}

// transport wraps one inner transport with the hold.
func (hold *reportHold) transport(inner http.RoundTripper) http.RoundTripper {
	return reportHoldTransport{inner: inner, hold: hold}
}

// waitReached waits until one report request is held.
func (hold *reportHold) waitReached(t *testing.T) {
	t.Helper()
	select {
	case <-hold.reached:
	case <-time.After(pushWorkflowTimeout):
		t.Fatalf("no verified report reached the transport hold within %s", pushWorkflowTimeout)
	}
}

// releaseAll releases the held report request.
func (hold *reportHold) releaseAll() {
	hold.releaseOnce.Do(func() { close(hold.release) })
}

type reportHoldTransport struct {
	inner http.RoundTripper
	hold  *reportHold
}

func (transport reportHoldTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if strings.HasSuffix(request.URL.Path, "/api/v1/reports") {
		hold := transport.hold
		hold.mu.Lock()
		held := hold.armed
		if held {
			hold.armed = false
		}
		hold.mu.Unlock()
		if held {
			hold.reachedOnce.Do(func() { close(hold.reached) })
			select {
			case <-hold.release:
			case <-request.Context().Done():
				return nil, request.Context().Err()
			}
		}
	}
	return transport.inner.RoundTrip(request)
}

// attemptResolver is a labelled image-resolver seam. The default inner resolver
// is the real digest-pinned resolver; only the named attempt behavior differs.
type attemptResolver struct {
	inner pipeline.Resolver

	mu          sync.Mutex
	armed       bool
	failThrough int
	attempts    int

	first   chan struct{}
	blocked chan struct{}
	release chan struct{}
	reached sync.Once
}

// newAttemptResolver returns an unarmed resolver over the real inner resolver.
func newAttemptResolver(inner pipeline.Resolver) *attemptResolver {
	return &attemptResolver{
		inner:   inner,
		first:   make(chan struct{}),
		blocked: make(chan struct{}),
		release: make(chan struct{}),
	}
}

// armFailFirst makes the first admission attempt fail transiently and the
// second attempt block until release, then fail transiently again. The third
// attempt is a fresh processor pass: it reads the edited candidates and
// delegates to the real resolver. This is the edit-replay schedule: the first
// selection is durable, the edits happen during the held attempt, and the
// successful replay starts after a new pass has read the edited configuration.
func (resolver *attemptResolver) armFailFirst() {
	resolver.mu.Lock()
	resolver.armed = true
	resolver.failThrough = 2
	resolver.mu.Unlock()
}

// armHold makes the first armed admission attempt block until release and then
// delegate to the real resolver.
func (resolver *attemptResolver) armHold() {
	resolver.mu.Lock()
	resolver.armed = true
	resolver.failThrough = 0
	resolver.mu.Unlock()
}

// disarm lets every later admission attempt delegate to the real resolver. A
// currently blocked attempt still waits for release or its context.
func (resolver *attemptResolver) disarm() {
	resolver.mu.Lock()
	resolver.armed = false
	resolver.mu.Unlock()
}

// waitFirst waits for the first armed admission attempt.
func (resolver *attemptResolver) waitFirst(t *testing.T) {
	t.Helper()
	select {
	case <-resolver.first:
	case <-time.After(pushWorkflowTimeout):
		t.Fatalf("the first admission attempt never reached the resolver hold")
	}
}

// waitBlocked waits for the second (post-transient) admission attempt to block
// on the resolver hold.
func (resolver *attemptResolver) waitBlocked(t *testing.T) {
	t.Helper()
	select {
	case <-resolver.blocked:
	case <-time.After(pushWorkflowTimeout):
		t.Fatalf("the retried admission attempt never reached the resolver hold")
	}
}

// releaseAll releases every blocked admission attempt.
func (resolver *attemptResolver) releaseAll() {
	select {
	case <-resolver.release:
	default:
		close(resolver.release)
	}
}

// Resolve implements pipeline.Resolver.
func (resolver *attemptResolver) Resolve(ctx context.Context, request pipeline.ResolveRequest) (pipeline.ResolvedImage, error) {
	resolver.mu.Lock()
	armed := resolver.armed
	attempt := 0
	if armed {
		resolver.attempts++
		attempt = resolver.attempts
	}
	failThrough := resolver.failThrough
	resolver.mu.Unlock()
	if !armed {
		return resolver.inner.Resolve(ctx, request)
	}
	switch {
	case attempt == 1:
		resolver.reached.Do(func() { close(resolver.first) })
		if attempt <= failThrough {
			return pipeline.ResolvedImage{}, errors.New("fault injection: image registry is unavailable for one admission attempt")
		}
	case attempt == 2 && failThrough >= 2:
		resolver.reached.Do(func() { close(resolver.first) })
		select {
		case <-resolver.blocked:
		default:
			close(resolver.blocked)
		}
		select {
		case <-resolver.release:
		case <-ctx.Done():
			return pipeline.ResolvedImage{}, ctx.Err()
		}
		return pipeline.ResolvedImage{}, errors.New("fault injection: image registry is unavailable for one admission attempt")
	default:
		resolver.reached.Do(func() { close(resolver.first) })
	}
	select {
	case <-resolver.release:
	case <-ctx.Done():
		return pipeline.ResolvedImage{}, ctx.Err()
	}
	return resolver.inner.Resolve(ctx, request)
}
