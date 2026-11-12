//go:build integration

package factorytest

// Shared protected golden-path scenario and evidence assertions.
//
// The scenario drives the whole protected flow through public operations only
// and returns the durable observations the golden-path test and the single
// no-substitute pin assert together. Nothing here injects authority, approval,
// a verification row or a Git ref: the served Shell and the managed native
// hooks produce every fact, and the test reads those facts back through the
// scoped runtime role, the public execution read and the real repository.
//
// Three Shell-launched workload containers share one execution and one retained
// workspace. Each is gated on a file the test creates only after it has
// independently observed the predecessor's durable state, so no assertion
// depends on a fixed sleep:
//
//   - implement: task context, exact base, one candidate C on the granted work
//     ref, declared candidate output;
//   - test: retained checkout at exactly C, clean tracked files, real test
//     script, declared candidate/test_result outputs;
//   - integrate: both settled outputs, retained marker, exact C, base B as an
//     ancestor, `--force-with-lease=refs/heads/main:B` and no new commit.
//
// The frozen factory TestClock advances only while a terminal execution's
// scheduled cleanup is pending with attempts, in the bounded 2 s step the
// harness note requires. The helper name is file-local so it cannot collide
// with another suite's clock helper.

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/api"
	shelloidc "github.com/optimiware/kodama/internal/shell/integration/oidc"
	"github.com/optimiware/kodama/internal/userland/git"
	identityfixture "github.com/optimiware/kodama/test/support/boundaries/identity"
	"github.com/optimiware/kodama/test/support/factory"
)

const (
	// protectedGoldenWorkRef is the granted work branch the implement action
	// pushes. The fixture policy grants it to the engineer and an execution.
	protectedGoldenWorkRef = "refs/heads/work/protected-golden"
	// protectedGoldenTaskTitle names the one task this scenario creates.
	protectedGoldenTaskTitle = "protected golden path"
	// protectedGoldenReviewerSubject is the unique OIDC subject of the one
	// human granted exactly engineer+reviewer for this scenario.
	protectedGoldenReviewerSubject = "protected-golden-reviewer"

	// protectedGoldenImplementGate, TestGate and IntegrateGate are the
	// per-action release files. The test creates one only after it observed the
	// predecessor's durable state, so the third container cannot race the
	// review decision or the exact-candidate assertions.
	protectedGoldenImplementGate = "/workspace/protected-golden-implement-ready"
	protectedGoldenTestGate      = "/workspace/protected-golden-test-ready"
	protectedGoldenIntegrateGate = "/workspace/protected-golden-integrate-ready"

	// protectedGoldenMarkerPath is the execution-owned workspace marker the
	// authored commands write and later actions require.
	protectedGoldenMarkerPath = "/workspace/.kodama-workspace-marker"
	// protectedGoldenRepositoryDirectory is the retained checkout inside the
	// shared workspace. It mirrors the authored command's fixed path.
	protectedGoldenRepositoryDirectory = "/workspace/git-protected-repository"
	// protectedGoldenImplementCandidatePath and the test paths are the
	// product-materialized declared outputs.
	protectedGoldenImplementCandidatePath = "/kodama/outputs/implement/candidate"
	protectedGoldenTestCandidatePath      = "/kodama/outputs/test/candidate"
	protectedGoldenTestResultPath         = "/kodama/outputs/test/test_result"
	// protectedGoldenTaskFile is the product task snapshot path.
	protectedGoldenTaskFile = "/kodama/task.json"

	// protectedGoldenActionTimeout keeps the real PostgreSQL statement clock
	// far from the action deadline while a gate holds a container open.
	protectedGoldenActionTimeout = "20m"
	// protectedGoldenRequestTimeout keeps the request well beyond the same
	// clock while the human inspects and resolves it.
	protectedGoldenRequestTimeout = "1h"

	// protectedGoldenTimeout bounds one wait for a durable fact of this flow.
	protectedGoldenTimeout = 10 * time.Minute
)

// protectedGoldenPipelineDoc is the API-posted golden pipeline document. It is
// a separate type from the preliminary pipelineDoc because this scenario
// declares inputs.
type protectedGoldenPipelineDoc struct {
	Inputs  []string            `yaml:"inputs"`
	Actions []pipelineActionDoc `yaml:"actions"`
}

// protectedGoldenEvidence is every durable observation the shared assertions
// need. It is populated by the scenario and never synthesizes a fact.
type protectedGoldenEvidence struct {
	fixture *gitHarness

	pipelineID  string
	taskID      string
	executionID string
	base        string
	candidate   string
	workRef     string

	engineerPrincipal string
	reviewerToken     string
	reviewerPrincipal string
	// executionToken, testToken and integrateToken are the mounted execution
	// credentials each workload container actually used. Request entry clears
	// the credential and the container after the human pass receives a newly
	// minted one, so the integrate token that signs the push to main is a
	// different value the leak scans must search for as well.
	executionToken string
	testToken      string
	integrateToken string

	implement api.ExecutionAction
	test      api.ExecutionAction
	review    api.ExecutionAction
	integrate api.ExecutionAction

	workJournal      git.JournalEntry
	protectedJournal git.JournalEntry
	mainJournalCount int

	verification    factory.VerificationRow
	approval        factory.ApprovalAssignment
	executionActor  string
	reportEvent     factory.EventRow
	reportReplayed  bool
	replayEvent     factory.EventRow
	mainAfterReplay string

	mainBeforeIntegrate string
	task                api.TaskReadResult
	finished            api.ExecutionInspectResult
	logs                string
}

// runProtectedGoldenScenario executes the complete golden path against one
// real fixture and returns the evidence both callers assert.
func runProtectedGoldenScenario(t *testing.T, fixture *gitHarness) protectedGoldenEvidence {
	t.Helper()
	if fixture == nil || fixture.git == nil {
		t.Fatalf("the protected golden path needs the Git fixture")
	}
	image := fixture.git.EnsureWorkloadImage(t)
	if image.RunnableReference == "" || !strings.Contains(image.RunnableReference, "@sha256:") {
		t.Fatalf("workload image reference %q is not digest-pinned", image.RunnableReference)
	}
	if !strings.HasPrefix(fixture.git.WorkloadURL, "https://host.containers.internal:") {
		t.Fatalf("workload Git URL = %q; want the host workload alias through the relay", fixture.git.WorkloadURL)
	}
	base := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef)
	if base == "" {
		t.Fatalf("the seeded project main ref is missing")
	}

	evidence := protectedGoldenEvidence{
		fixture:           fixture,
		base:              base,
		workRef:           protectedGoldenWorkRef,
		engineerPrincipal: fixture.engineerPrincipal,
	}
	evidence.reviewerToken, evidence.reviewerPrincipal = grantProtectedReviewer(t, fixture, protectedGoldenReviewerSubject)
	if evidence.reviewerPrincipal == evidence.engineerPrincipal {
		t.Fatalf("the reviewer must be a separate human from the engineer")
	}

	spec := factory.WorkloadCandidateSpec{
		WorkRef:      protectedGoldenWorkRef,
		ExpectedBase: base,
		RequireTask:  true,
	}
	implement, err := fixture.git.WorkloadCandidateCommands(t, spec)
	if err != nil {
		t.Fatalf("author the implement workload commands: %v", err)
	}
	tested, err := fixture.git.WorkloadTestCandidateCommands(t, spec)
	if err != nil {
		t.Fatalf("author the test workload commands: %v", err)
	}
	integrated, err := fixture.git.WorkloadIntegrateCandidateCommands(t, spec)
	if err != nil {
		t.Fatalf("author the integrate workload commands: %v", err)
	}

	inputs := map[string]string{
		"repository":    fixture.git.ProjectResourceID,
		"target_ref":    gitMainRef,
		"expected_base": base,
	}
	document := protectedGoldenPipelineDoc{
		Inputs: []string{"repository", "target_ref", "expected_base"},
		Actions: []pipelineActionDoc{
			{
				Name:     "implement",
				Image:    image.RunnableReference,
				Timeout:  protectedGoldenActionTimeout,
				Commands: protectedGoldenGated(protectedGoldenImplementGate, implement),
				Outputs:  []string{"candidate"},
			},
			{
				Name:     "test",
				Image:    image.RunnableReference,
				Timeout:  protectedGoldenActionTimeout,
				Commands: protectedGoldenGated(protectedGoldenTestGate, tested),
				Outputs:  []string{"candidate", "test_result"},
			},
			{
				Name:    "review",
				Request: "role/reviewer",
				Timeout: protectedGoldenRequestTimeout,
				Subject: protectedGoldenReviewSubject(),
			},
			{
				Name:     "integrate",
				Image:    image.RunnableReference,
				Timeout:  protectedGoldenActionTimeout,
				Commands: protectedGoldenGated(protectedGoldenIntegrateGate, integrated),
			},
		},
	}
	applied := fixture.applyPipeline("protected-golden-path", marshalProtectedGoldenPipeline(t, document))
	if applied.PipelineID == "" || applied.Version == 0 {
		t.Fatalf("pipeline.apply returned %+v; want an admitted pipeline and version", applied)
	}
	evidence.pipelineID = applied.PipelineID

	// Step 1: create an open task with the declared inputs, read it back, then
	// start it at the observed lifecycle version.
	created := api.TaskCreateResult{}
	fixture.Decode(fixture.MustREST(fixture.engineerToken, "task.create", api.TaskCreateRequest{
		ProjectID:  factory.ProjectID,
		Title:      protectedGoldenTaskTitle,
		PipelineID: applied.PipelineID,
		Inputs:     inputs,
	}, nil, "create-protected-golden-path"), &created)
	evidence.taskID = created.TaskID
	if created.TaskID == "" || created.ContentVersion == 0 || created.LifecycleVersion == 0 {
		t.Fatalf("task.create returned %+v; want a committed task", created)
	}
	createdRead := readProtectedGoldenTask(t, fixture, fixture.engineerToken, evidence.taskID)
	if createdRead.State != "open" {
		t.Fatalf("created task state = %q; want open", createdRead.State)
	}
	if createdRead.CurrentExecution != "" || len(createdRead.ExecutionHistory) != 0 {
		t.Fatalf("created task carries execution %q and history %v; want none before start",
			createdRead.CurrentExecution, createdRead.ExecutionHistory)
	}
	if createdRead.SelectedPipeline != applied.PipelineID {
		t.Fatalf("created task pipeline = %q; want %q", createdRead.SelectedPipeline, applied.PipelineID)
	}
	if !reflect.DeepEqual(createdRead.Inputs, inputs) {
		t.Fatalf("created task inputs = %v; want %v", createdRead.Inputs, inputs)
	}
	if createdRead.Initiator != fixture.engineerPrincipal {
		t.Fatalf("created task initiator = %q; want the OIDC engineer %q",
			createdRead.Initiator, fixture.engineerPrincipal)
	}
	if createdRead.LifecycleVersion != created.LifecycleVersion {
		t.Fatalf("task.read lifecycle version = %d; task.create committed %d",
			createdRead.LifecycleVersion, created.LifecycleVersion)
	}

	started := api.TaskStartResult{}
	fixture.Decode(fixture.MustREST(fixture.engineerToken, "task.start", api.TaskStartRequest{
		TaskID: evidence.taskID,
	}, &createdRead.LifecycleVersion, "start-protected-golden-path"), &started)
	evidence.executionID = started.ExecutionID
	if started.ExecutionID == "" || started.LifecycleVersion <= createdRead.LifecycleVersion {
		t.Fatalf("task.start returned %+v; want a new execution and an advanced lifecycle version", started)
	}
	startedRead := waitProtectedGoldenTask(t, fixture, fixture.engineerToken, evidence.taskID,
		"task execution="+started.ExecutionID+" after task.start", func(read api.TaskReadResult) bool {
			return read.CurrentExecution == started.ExecutionID
		})
	if startedRead.State != "open" {
		t.Fatalf("started task state = %q; want open while its execution runs", startedRead.State)
	}
	if len(startedRead.ExecutionHistory) != 1 || startedRead.ExecutionHistory[0] != started.ExecutionID {
		t.Fatalf("started task history = %v; want exactly the one started execution %q",
			startedRead.ExecutionHistory, started.ExecutionID)
	}

	// Step 2: the implement container. Its commands verify KODAMA_TASK and the
	// task snapshot themselves; the test independently observes the same task
	// context, proves the execution credential is usable before the first push,
	// and only then releases the gate.
	waitProtectedActionRunning(t, fixture, evidence.executionID, "implement")
	implementContainer := waitProtectedGoldenContainer(t, fixture, evidence.executionID, "implement")
	evidence.executionToken = waitProtectedGoldenCredential(t, fixture, evidence.executionID, "implement")
	if got := protectedGoldenExec(t, fixture, implementContainer, "printenv", "KODAMA_TASK"); got != evidence.taskID {
		t.Fatalf("implement KODAMA_TASK = %q; want the task %q", got, evidence.taskID)
	}
	taskSnapshot := protectedGoldenRead(t, fixture, implementContainer, protectedGoldenTaskFile)
	if !json.Valid([]byte(taskSnapshot)) {
		t.Fatalf("implement /kodama/task.json is not valid JSON: %q", taskSnapshot)
	}
	requireProtectedGoldenCredentialUsable(t, fixture, evidence.executionID, evidence.executionToken)
	releaseProtectedGoldenGate(t, fixture, implementContainer, protectedGoldenImplementGate)

	// Step 3: the test container reuses the retained checkout. Read the marker,
	// HEAD and the materialized implement output from its own workspace before
	// releasing it; the committed test script then exercises the real program.
	waitProtectedActionRunning(t, fixture, evidence.executionID, "test")
	testContainer := waitProtectedGoldenContainer(t, fixture, evidence.executionID, "test")
	evidence.testToken = waitProtectedGoldenCredential(t, fixture, evidence.executionID, "test")
	requireProtectedGoldenMarker(t, fixture, testContainer, evidence.executionID)
	candidate := protectedGoldenRead(t, fixture, testContainer, protectedGoldenImplementCandidatePath)
	if candidate == "" || candidate == base {
		t.Fatalf("materialized implement candidate = %q; want a commit different from the base %q", candidate, base)
	}
	evidence.candidate = candidate
	requireProtectedGoldenHead(t, fixture, testContainer, candidate)
	releaseProtectedGoldenGate(t, fixture, testContainer, protectedGoldenTestGate)

	// Step 4: the request action runs without a workload. The task stays open,
	// the protected ref stays at the reviewed base and integrate is not active.
	waitProtectedActionRunning(t, fixture, evidence.executionID, "review")
	requireProtectedGoldenNoWorkload(t, fixture, evidence.executionID)
	pending := inspectProtectedGoldenExecution(t, fixture, evidence.reviewerToken, evidence.executionID)
	pendingReview := actionByName(t, pending, "review")
	pendingIntegrate := actionByName(t, pending, "integrate")
	if pendingIntegrate.State == "running" || pendingIntegrate.State == "succeeded" {
		t.Fatalf("integrate action state = %q while the request is pending; want it unstarted", pendingIntegrate.State)
	}
	evidence.mainBeforeIntegrate = fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef)
	if evidence.mainBeforeIntegrate != base {
		t.Fatalf("protected main = %q while the request is pending; want the reviewed base %q",
			evidence.mainBeforeIntegrate, base)
	}
	pendingTask := readProtectedGoldenTask(t, fixture, evidence.reviewerToken, evidence.taskID)
	if pendingTask.State != "open" {
		t.Fatalf("task state = %q while the request is pending; want open", pendingTask.State)
	}
	if pendingTask.CurrentExecution != evidence.executionID {
		t.Fatalf("task current execution = %q; want %q", pendingTask.CurrentExecution, evidence.executionID)
	}
	request := waitProtectedGoldenRequest(t, fixture, evidence.reviewerToken, evidence.executionID)
	requireProtectedGoldenRequest(t, request, evidence, pendingReview)

	resolveProtectedGoldenRequest(t, fixture, evidence, pendingReview)

	// Step 5: the integrate container. Read the retained workspace and both
	// settled outputs, prove the protected ref is still the reviewed base, then
	// release the exact-candidate push.
	waitProtectedActionRunning(t, fixture, evidence.executionID, "integrate")
	integrateContainer := waitProtectedGoldenContainer(t, fixture, evidence.executionID, "integrate")
	evidence.integrateToken = waitProtectedGoldenCredential(t, fixture, evidence.executionID, "integrate")
	requireProtectedGoldenMarker(t, fixture, integrateContainer, evidence.executionID)
	requireProtectedGoldenHead(t, fixture, integrateContainer, evidence.candidate)
	if got := protectedGoldenRead(t, fixture, integrateContainer, protectedGoldenTestCandidatePath); got != evidence.candidate {
		t.Fatalf("materialized test candidate = %q; want %q", got, evidence.candidate)
	}
	if got := protectedGoldenRead(t, fixture, integrateContainer, protectedGoldenTestResultPath); got != "pass:"+evidence.candidate {
		t.Fatalf("materialized test_result = %q; want pass:%s", got, evidence.candidate)
	}
	if main := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef); main != base {
		t.Fatalf("protected main = %q before the integrate push; want the reviewed base %q", main, base)
	}
	// The request entry cleared the prior credential, so this container carries
	// a freshly minted token. Prove the served edge accepts it before the push is
	// released, exactly like the implement credential: one plain bounded wait for
	// the current nonce and one clone, never a retry around a known race.
	requireProtectedGoldenCredentialUsable(t, fixture, evidence.executionID, evidence.integrateToken)
	releaseProtectedGoldenGate(t, fixture, integrateContainer, protectedGoldenIntegrateGate)

	// Step 6: wait for a successful terminal execution with completed cleanup,
	// then read the task, execution, retained logs and every durable fact.
	finished := waitProtectedGoldenTerminal(t, fixture, evidence.executionID)
	if finished.State != "succeeded" {
		final := fixture.executionLogs(t, finished)
		t.Fatalf("golden execution state = %q; want succeeded\n%s", finished.State, final)
	}
	evidence.finished = finished
	evidence.implement = actionByName(t, finished, "implement")
	evidence.test = actionByName(t, finished, "test")
	evidence.review = actionByName(t, finished, "review")
	evidence.integrate = actionByName(t, finished, "integrate")
	evidence.logs = fixture.executionLogs(t, finished)

	evidence.workJournal = waitProtectedGoldenJournal(t, fixture, factory.GitProjectRepository, protectedGoldenWorkRef, evidence.candidate)
	evidence.protectedJournal = waitProtectedGoldenJournal(t, fixture, factory.GitProjectRepository, gitMainRef, evidence.candidate)
	evidence.mainJournalCount = protectedGoldenJournalCount(t, fixture, factory.GitProjectRepository, gitMainRef)
	evidence.verification = fixture.verificationByID(t, evidence.protectedJournal.VerificationID)
	approval, err := fixture.git.RequestedApproval(t.Context(), evidence.verification.ID)
	if err != nil {
		t.Fatalf("read the protected verification's stored requested approval: %v", err)
	}
	evidence.approval = approval
	evidence.executionActor = protectedGoldenExecutionActor(t, fixture, evidence.executionID)
	evidence.reportEvent = fixture.waitReportEvent(t, evidence.verification.ID)

	evidence.task = waitProtectedGoldenTask(t, fixture, fixture.engineerToken, evidence.taskID,
		"task execution="+evidence.executionID+" succeeded", func(read api.TaskReadResult) bool {
			return read.State == "done"
		})

	// Replay the exact accepted resource.report with the Git service credential
	// and the original source-event idempotency key; then re-read every identity.
	evidence.reportReplayed, evidence.replayEvent = replayProtectedGoldenReport(t, fixture, evidence)
	evidence.mainAfterReplay = fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef)
	return evidence
}

// protectedGoldenPipelineDocument renders one exact pipeline document. The
// review subject uses the five canonical fields; resource is the registered
// resource identity, never the repository alias or URL.
func protectedGoldenReviewSubject() map[string]any {
	return map[string]any{
		"operation": approvalOperation,
		"resource": map[string]any{
			"from": "inputs.repository",
		},
		"ref": map[string]any{
			"from": "inputs.target_ref",
		},
		"expected_base": map[string]any{
			"from": "inputs.expected_base",
		},
		"candidate": map[string]any{
			"from": "implement.outputs.candidate",
		},
	}
}

// marshalProtectedGoldenPipeline renders one exact pipeline YAML byte sequence.
func marshalProtectedGoldenPipeline(t *testing.T, document protectedGoldenPipelineDoc) []byte {
	t.Helper()
	encoded, err := yaml.Marshal(document)
	if err != nil {
		t.Fatalf("marshal protected golden pipeline: %v", err)
	}
	return encoded
}

// protectedGoldenGated prepends the release wait to one authored command list.
func protectedGoldenGated(gate string, commands []string) []string {
	gated := make([]string, 0, len(commands)+1)
	gated = append(gated, "while [ ! -e "+gate+" ]; do sleep 1; done")
	return append(gated, commands...)
}

// grantProtectedReviewer mints one OIDC human under the given subject and
// grants exactly engineer+reviewer through the public membership operation.
// The token is only an identity hint: the resolver re-reads the committed
// membership per request, and the read-back is the committed row, not the
// request.
func grantProtectedReviewer(t *testing.T, fixture *gitHarness, subject string) (string, string) {
	t.Helper()
	token := fixture.OIDC.Mint(t, identityfixture.TokenSpec{Subject: subject})
	principal, err := shelloidc.PrincipalIDFor(fixture.OIDC.Issuer, subject)
	if err != nil {
		t.Fatalf("derive the reviewer principal for %q: %v", subject, err)
	}
	fixture.MustREST(fixture.AdminToken, "membership.grant", api.MembershipGrantRequest{
		ProjectID:   factory.ProjectID,
		PrincipalID: string(principal),
		Roles:       []string{"engineer", "reviewer"},
	}, nil, "grant-"+subject)
	requireProtectedGoldenReviewerMembership(t, fixture, string(principal))
	return token, string(principal)
}

// requireProtectedGoldenReviewerMembership reads the committed membership back
// through the public API and requires the complete role set to be exactly
// engineer+reviewer for the scenario's one human. The project admin credential
// performs the read; the membership row, not the grant request, is the evidence.
func requireProtectedGoldenReviewerMembership(t *testing.T, fixture *gitHarness, principal string) {
	t.Helper()
	var listed api.MembershipListResult
	fixture.Decode(fixture.MustREST(fixture.AdminToken, "membership.list", api.MembershipListRequest{
		ProjectID: factory.ProjectID,
	}, nil, ""), &listed)
	for _, membership := range listed.Memberships {
		if membership.PrincipalID != principal {
			continue
		}
		roles := map[string]bool{}
		for _, role := range membership.Roles {
			roles[role] = true
		}
		if len(membership.Roles) != 2 || !roles["engineer"] || !roles["reviewer"] {
			t.Fatalf("committed membership for %q = %v; want exactly engineer+reviewer",
				principal, membership.Roles)
		}
		return
	}
	t.Fatalf("membership.list returned no committed membership for the golden reviewer %q", principal)
}

// readProtectedGoldenTask reads one task through the public operation.
func readProtectedGoldenTask(t *testing.T, fixture *gitHarness, token, taskID string) api.TaskReadResult {
	t.Helper()
	var read api.TaskReadResult
	fixture.Decode(fixture.MustREST(token, "task.read", api.TaskReadRequest{
		TaskID: taskID,
	}, nil, ""), &read)
	return read
}

// waitProtectedGoldenTask waits for one named task condition with a bounded
// deadline and fails with the last observed task summary.
func waitProtectedGoldenTask(t *testing.T, fixture *gitHarness, token, taskID, name string, condition func(api.TaskReadResult) bool) api.TaskReadResult {
	t.Helper()
	var last api.TaskReadResult
	fixture.git.WaitFor(t, name, protectedGoldenTimeout, func() bool {
		last = readProtectedGoldenTask(t, fixture, token, taskID)
		return condition(last)
	})
	return last
}

// inspectProtectedGoldenExecution reads one execution through the public
// operation with the given credential.
func inspectProtectedGoldenExecution(t *testing.T, fixture *gitHarness, token, executionID string) api.ExecutionInspectResult {
	t.Helper()
	var inspected api.ExecutionInspectResult
	fixture.Decode(fixture.MustREST(token, "execution.inspect", api.ExecutionInspectRequest{
		ExecutionID: executionID,
	}, nil, ""), &inspected)
	return inspected
}

// waitProtectedGoldenContainer waits for the one running workload container of
// the current action.
func waitProtectedGoldenContainer(t *testing.T, fixture *gitHarness, executionID, action string) string {
	t.Helper()
	var container string
	fixture.git.WaitFor(t,
		"running workload container execution="+executionID+" action="+action,
		protectedGoldenTimeout, func() bool {
			found, err := fixture.RunningExecutionContainer(t.Context(), executionID)
			if err != nil || found == "" {
				return false
			}
			container = found
			return true
		})
	return container
}

// waitProtectedGoldenCredential reads the product-materialized execution
// credential of one running action. It is never rendered in a failure message.
func waitProtectedGoldenCredential(t *testing.T, fixture *gitHarness, executionID, action string) string {
	t.Helper()
	var credential string
	fixture.git.WaitFor(t,
		"mounted execution credential execution="+executionID+" action="+action,
		protectedGoldenTimeout, func() bool {
			value, err := fixture.ExecutionCredential(t.Context(), executionID, action)
			if err != nil {
				return false
			}
			credential = strings.TrimSpace(value)
			return credential != ""
		})
	return credential
}

// requireProtectedGoldenCredentialUsable proves the mounted execution
// credential is accepted by the served Git edge before the first workload push
// runs. A current credential is not refused right after start, so the only wait
// is a plain bounded wait for the product to publish the execution's current
// credential nonce. The clone itself runs once; a refusal is final, never
// retried past the authority defect it would hide.
func requireProtectedGoldenCredentialUsable(t *testing.T, fixture *gitHarness, executionID, credential string) {
	t.Helper()
	fixture.git.WaitFor(t, "current execution credential execution="+executionID,
		2*time.Minute, func() bool {
			present, err := fixture.ExecutionCredentialNoncePresent(t.Context(), executionID)
			return err == nil && present
		})
	directory := filepath.Join(t.TempDir(), "readiness")
	stderr, code := fixture.git.Clone(t, credential, factory.GitProjectRepository, directory)
	if code != 0 {
		t.Fatalf("served Git edge refused the current execution credential: exit %d: %s", code, stderr)
	}
}

// protectedGoldenRead reads one file from a running workload container.
func protectedGoldenRead(t *testing.T, fixture *gitHarness, container, path string) string {
	t.Helper()
	value, err := fixture.ReadContainerFile(t.Context(), container, path)
	if err != nil {
		t.Fatalf("read %s from the running workload: %v", path, err)
	}
	return strings.TrimSpace(value)
}

// protectedGoldenExec runs one command inside a running workload container.
func protectedGoldenExec(t *testing.T, fixture *gitHarness, container string, args ...string) string {
	t.Helper()
	value, err := fixture.ExecContainer(t.Context(), container, args...)
	if err != nil {
		t.Fatalf("exec %v in the running workload: %v", args, err)
	}
	return strings.TrimSpace(value)
}

// requireProtectedGoldenMarker requires the retained workspace marker to name
// exactly this execution, so a workspace replaced between actions cannot pass.
func requireProtectedGoldenMarker(t *testing.T, fixture *gitHarness, container, executionID string) {
	t.Helper()
	if got := protectedGoldenRead(t, fixture, container, protectedGoldenMarkerPath); got != executionID {
		t.Fatalf("retained workspace marker = %q; want execution %q", got, executionID)
	}
}

// requireProtectedGoldenHead requires the retained checkout to be exactly the
// expected commit in the running container.
func requireProtectedGoldenHead(t *testing.T, fixture *gitHarness, container, want string) {
	t.Helper()
	got := protectedGoldenExec(t, fixture, container, "/bin/sh", "-c",
		"cd "+protectedGoldenRepositoryDirectory+" && git rev-parse HEAD")
	if got != want {
		t.Fatalf("retained checkout HEAD = %q; want %q", got, want)
	}
}

// releaseProtectedGoldenGate creates the release file that lets one gated
// authored command list continue.
func releaseProtectedGoldenGate(t *testing.T, fixture *gitHarness, container, gate string) {
	t.Helper()
	if err := fixture.TouchContainerFile(t.Context(), container, gate); err != nil {
		t.Fatalf("release the gated workload action: %v", err)
	}
}

// requireProtectedGoldenNoWorkload proves a request action runs with no
// workload container. The previous action's container is removed before the
// request activates, so this is a bounded barrier, not a fixed sleep.
func requireProtectedGoldenNoWorkload(t *testing.T, fixture *gitHarness, executionID string) {
	t.Helper()
	fixture.git.WaitFor(t, "request action execution="+executionID+" runs without a workload",
		time.Minute, func() bool {
			container, err := fixture.RunningExecutionContainer(t.Context(), executionID)
			return err == nil && container == ""
		})
}

// waitProtectedGoldenRequest waits for this execution's request inbox item.
func waitProtectedGoldenRequest(t *testing.T, fixture *gitHarness, token, executionID string) api.InboxItem {
	t.Helper()
	var found api.InboxItem
	fixture.git.WaitFor(t, "task.inbox request execution="+executionID,
		protectedGoldenTimeout, func() bool {
			result, publicError := fixture.REST(token, "task.inbox", api.TaskInboxRequest{
				ProjectID: factory.ProjectID,
				Kinds:     []string{"request"},
			}, nil, "")
			if publicError.Code != "" {
				return false
			}
			var inbox api.TaskInboxResult
			fixture.Decode(result, &inbox)
			for _, item := range inbox.Items {
				if item.Kind == "request" && item.ExecutionID == executionID {
					found = item
					return true
				}
			}
			return false
		})
	return found
}

// requireProtectedGoldenRequest asserts the discovered request is the exact
// role/reviewer assignment with the canonical resolved subject. Request inbox
// items deliberately carry execution and action identities, not a task id; the
// task link is the execution's task, already observed through task.read and
// execution.inspect.
func requireProtectedGoldenRequest(t *testing.T, item api.InboxItem, evidence protectedGoldenEvidence, review api.ExecutionAction) {
	t.Helper()
	if item.ExecutionID != evidence.executionID {
		t.Fatalf("inbox request execution = %q; want %q", item.ExecutionID, evidence.executionID)
	}
	if item.ActionID != review.ActionID {
		t.Fatalf("inbox request action = %q; want the running review action %q", item.ActionID, review.ActionID)
	}
	if item.Assignment != "role/reviewer" {
		t.Fatalf("inbox request assignment = %q; want role/reviewer", item.Assignment)
	}
	if item.Version == 0 || item.At.IsZero() {
		t.Fatalf("inbox request is missing its version or deadline: %+v", item)
	}
	requireProtectedGoldenSubject(t, "inbox request", item.Subject, evidence)
}

// resolveProtectedGoldenRequest answers one running request with the exact
// human pass. The envelope carries the current execution version and the body
// carries the observed action version; only an explicit version conflict is
// retried.
func resolveProtectedGoldenRequest(t *testing.T, fixture *gitHarness, evidence protectedGoldenEvidence, initial api.ExecutionAction) {
	t.Helper()
	for range 30 {
		current := inspectProtectedGoldenExecution(t, fixture, evidence.reviewerToken, evidence.executionID)
		if current.State != "running" {
			t.Fatalf("execution %s state = %q before resolve; want running", evidence.executionID, current.State)
		}
		action := actionByName(t, current, "review")
		result, publicError := fixture.REST(evidence.reviewerToken, "execution.request.resolve", api.ExecutionRequestResolveRequest{
			ExecutionID:           evidence.executionID,
			ActionID:              action.ActionID,
			ExpectedActionVersion: action.Version,
			Decision:              "pass",
		}, &current.Version, "resolve-protected-golden-path")
		if publicError.Code == api.CodeVersionConflict {
			continue
		}
		if publicError.Code != "" {
			t.Fatalf("execution.request.resolve refused: %s (%s)", publicError.Code, publicError.Message)
		}
		var resolved api.ExecutionRequestResolveResult
		fixture.Decode(result, &resolved)
		if resolved.ExecutionVersion <= current.Version || resolved.ActionVersion <= action.Version {
			t.Fatalf("resolve versions = %+v; want both advanced past execution=%d action=%d",
				resolved, current.Version, action.Version)
		}
		if initial.ActionID != action.ActionID {
			t.Fatalf("review action identity changed from %q to %q", initial.ActionID, action.ActionID)
		}
		return
	}
	t.Fatalf("execution.request.resolve did not converge on a current version")
}

// waitProtectedGoldenTerminal waits for a terminal execution whose retained-log
// collection and scheduled cleanup have completed. The factory clock is frozen,
// so it advances by the bounded 2 s step only while the inspected cleanup is
// pending with attempts.
func waitProtectedGoldenTerminal(t *testing.T, fixture *gitHarness, executionID string) api.ExecutionInspectResult {
	t.Helper()
	var found api.ExecutionInspectResult
	fixture.git.WaitFor(t, "terminal execution="+executionID+" cleanup=complete",
		protectedGoldenTimeout, func() bool {
			execution, ok := fixture.inspectExecution(t, executionID)
			if !ok {
				return false
			}
			switch execution.State {
			case "succeeded", "failed", "cancelled":
			default:
				return false
			}
			if execution.Cleanup.State == "pending" && execution.Cleanup.Attempts > 0 {
				fixture.Clock.Advance(2 * time.Second)
				return false
			}
			if execution.Cleanup.State != "complete" {
				return false
			}
			found = execution
			return true
		})
	return found
}

// waitProtectedGoldenJournal waits for one reported journal entry of a ref and
// optional candidate.
func waitProtectedGoldenJournal(t *testing.T, fixture *gitHarness, repository, ref, candidate string) git.JournalEntry {
	t.Helper()
	var found git.JournalEntry
	fixture.git.WaitFor(t,
		"reported journal entry repository="+repository+" ref="+ref+" candidate="+candidate,
		protectedGoldenTimeout, func() bool {
			for _, entry := range fixture.git.JournalEntries(t) {
				if entry.Repository != repository || entry.Ref != ref || entry.State != git.JournalReported {
					continue
				}
				if candidate != "" && entry.Candidate != candidate {
					continue
				}
				found = entry
				return true
			}
			return false
		})
	return found
}

// protectedGoldenJournalCount counts the reported journal entries of one ref.
func protectedGoldenJournalCount(t *testing.T, fixture *gitHarness, repository, ref string) int {
	t.Helper()
	count := 0
	for _, entry := range fixture.git.JournalEntries(t) {
		if entry.Repository == repository && entry.Ref == ref && entry.State == git.JournalReported {
			count++
		}
	}
	return count
}

// protectedGoldenExecutionActor returns the one execution actor identity every
// verification row of this execution carries. It fails closed on disagreement.
func protectedGoldenExecutionActor(t *testing.T, fixture *gitHarness, executionID string) string {
	t.Helper()
	rows, err := fixture.git.Verifications(t.Context())
	if err != nil {
		t.Fatalf("read verification rows: %v", err)
	}
	actor := ""
	seen := 0
	for _, row := range rows {
		if row.ExecutionID != executionID {
			continue
		}
		seen++
		if actor == "" {
			actor = row.ActorID
			continue
		}
		if row.ActorID != actor {
			t.Fatalf("verification rows of execution %s carry actors %q and %q; want one execution actor",
				executionID, actor, row.ActorID)
		}
	}
	if seen == 0 || actor == "" {
		t.Fatalf("execution %s has no verification row naming an execution actor", executionID)
	}
	return actor
}

// replayProtectedGoldenReport resends the exact accepted resource.report with
// the Git service credential and the original source-event idempotency key.
func replayProtectedGoldenReport(t *testing.T, fixture *gitHarness, evidence protectedGoldenEvidence) (bool, factory.EventRow) {
	t.Helper()
	serviceToken, err := fixture.git.ServiceToken(t)
	if err != nil {
		t.Fatalf("read the mounted Git service credential: %v", err)
	}
	var subject api.Subject
	if err := json.Unmarshal([]byte(evidence.verification.Subject), &subject); err != nil {
		t.Fatalf("decode the accepted verification subject %s: %v", evidence.verification.Subject, err)
	}
	if len(subject.Fields) == 0 {
		t.Fatalf("the accepted verification carries no reportable subject")
	}
	var response api.ReportResponse
	fixture.Decode(fixture.MustREST(serviceToken, "resource.report", api.ReportRequest{
		ResourceID:     evidence.verification.ResourceID,
		SourceEventID:  evidence.protectedJournal.SourceEventID,
		EventType:      "git.ref.updated",
		Operation:      "git.ref.update",
		VerificationID: evidence.verification.ID,
		Subject:        subject,
	}, nil, evidence.protectedJournal.SourceEventID), &response)
	replayed, ok, err := fixture.git.ReportEvent(t.Context(), evidence.verification.ID)
	if err != nil {
		t.Fatalf("read the report event after replay: %v", err)
	}
	if !ok {
		t.Fatalf("verification %s has no linked report event after replay", evidence.verification.ID)
	}
	if response.VerificationID != evidence.verification.ID || response.EventID != replayed.ID {
		t.Fatalf("report replay named verification=%q event=%q; want verification=%q event=%q",
			response.VerificationID, response.EventID, evidence.verification.ID, replayed.ID)
	}
	return response.Replayed, replayed
}

// assertProtectedGoldenEvidence asserts every durable fact of the shared
// scenario. Both the golden-path test and the single no-substitute pin call it.
func assertProtectedGoldenEvidence(t *testing.T, evidence protectedGoldenEvidence) {
	t.Helper()
	fixture := evidence.fixture
	if fixture == nil || fixture.git == nil {
		t.Fatalf("golden evidence has no fixture")
	}
	if evidence.base == "" || evidence.candidate == "" || evidence.candidate == evidence.base {
		t.Fatalf("golden evidence base=%q candidate=%q; want distinct commits", evidence.base, evidence.candidate)
	}

	// The execution is this one task's only execution, and all four actions
	// succeeded in order.
	finished := evidence.finished
	if finished.State != "succeeded" || finished.Cleanup.State != "complete" {
		t.Fatalf("execution state=%q cleanup=%q; want succeeded and complete", finished.State, finished.Cleanup.State)
	}
	if finished.TaskID != evidence.taskID {
		t.Fatalf("execution task = %q; want %q", finished.TaskID, evidence.taskID)
	}
	if finished.PipelineID != evidence.pipelineID {
		t.Fatalf("execution pipeline = %q; want %q", finished.PipelineID, evidence.pipelineID)
	}
	if len(finished.Actions) != 4 {
		t.Fatalf("execution admits %d actions; want 4", len(finished.Actions))
	}
	for _, action := range []api.ExecutionAction{evidence.implement, evidence.test, evidence.review, evidence.integrate} {
		if action.State != "succeeded" {
			t.Fatalf("action %q state = %q; want succeeded", action.Name, action.State)
		}
	}
	if !(evidence.implement.Index < evidence.test.Index &&
		evidence.test.Index < evidence.review.Index &&
		evidence.review.Index < evidence.integrate.Index) {
		t.Fatalf("action order = implement %d test %d review %d integrate %d; want the declared order",
			evidence.implement.Index, evidence.test.Index, evidence.review.Index, evidence.integrate.Index)
	}

	// The declared outputs name the exact candidate and a successful test.
	if got := evidence.implement.Outputs["candidate"]; got != evidence.candidate {
		t.Fatalf("implement candidate output = %q; want %q", got, evidence.candidate)
	}
	if got := evidence.test.Outputs["candidate"]; got != evidence.candidate {
		t.Fatalf("test candidate output = %q; want %q", got, evidence.candidate)
	}
	if got := evidence.test.Outputs["test_result"]; got != "pass:"+evidence.candidate {
		t.Fatalf("test_result output = %q; want pass:%s", got, evidence.candidate)
	}

	// The protected ref moved exactly once, to the exact candidate, and the
	// candidate still has the reviewed base as its only parent.
	main := fixture.git.RemoteRef(t, factory.GitProjectRepository, gitMainRef)
	if main != evidence.candidate {
		t.Fatalf("protected main = %q; want the candidate %q", main, evidence.candidate)
	}
	if parents := fixture.git.CommitParents(t, factory.GitProjectRepository, evidence.candidate); len(parents) != 1 || parents[0] != evidence.base {
		t.Fatalf("candidate %s parents = %v; want exactly the reviewed base %s", evidence.candidate, parents, evidence.base)
	}
	history := fixture.git.History(t, factory.GitProjectRepository)
	if len(history) != 2 || history[0] != evidence.candidate || history[1] != evidence.base {
		t.Fatalf("protected history = %v; want exactly [%s %s] with no new integrate commit", history, evidence.candidate, evidence.base)
	}

	// The implement action's push is a real reported work-ref update naming this
	// execution and action, and the reported candidate is C.
	if evidence.workJournal.Ref != evidence.workRef || evidence.workJournal.State != git.JournalReported {
		t.Fatalf("work journal = %+v; want a reported entry for %q", evidence.workJournal, evidence.workRef)
	}
	if evidence.workJournal.Candidate != evidence.candidate ||
		evidence.workJournal.Repository != factory.GitProjectRepository ||
		evidence.workJournal.ResourceID != fixture.git.ProjectResourceID {
		t.Fatalf("work journal entry = %+v; want candidate %q on the registered resource", evidence.workJournal, evidence.candidate)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, evidence.workRef); remote != evidence.candidate {
		t.Fatalf("work ref = %q; want the reported candidate %q", remote, evidence.candidate)
	}
	workUpdate := protectedGoldenVerificationBy(t, fixture, evidence.executionID, evidence.implement.ActionID, "git.ref.update", evidence.workRef)
	if workUpdate.Result != "approved" || workUpdate.Approved {
		t.Fatalf("work-ref verification = %+v; want an approved verification with no human approval fact", workUpdate)
	}

	// The protected journal, its real scoped PostgreSQL verification row and the
	// stored requested approval are the actual run's, not a harness substitute.
	journal := evidence.protectedJournal
	if journal.Ref != gitMainRef || journal.State != git.JournalReported {
		t.Fatalf("protected journal = %+v; want a reported entry for %q", journal, gitMainRef)
	}
	if journal.Repository != factory.GitProjectRepository || journal.ResourceID != fixture.git.ProjectResourceID {
		t.Fatalf("protected journal resource = repository %q resource %q; want the registered project repository",
			journal.Repository, journal.ResourceID)
	}
	if journal.ExpectedBase != evidence.base || journal.Candidate != evidence.candidate {
		t.Fatalf("protected journal base/candidate = %q/%q; want %q/%q",
			journal.ExpectedBase, journal.Candidate, evidence.base, evidence.candidate)
	}
	if journal.VerificationID == "" || journal.SourceEventID == "" {
		t.Fatalf("protected journal is missing its verification or source-event identity: %+v", journal)
	}
	if evidence.mainJournalCount != 1 {
		t.Fatalf("reported protected journal entries = %d; want exactly one", evidence.mainJournalCount)
	}
	row := evidence.verification
	if row.ID != journal.VerificationID {
		t.Fatalf("protected verification id = %q; want the journal's %q", row.ID, journal.VerificationID)
	}
	if row.Result != "approved" || !row.Approved {
		t.Fatalf("protected verification result=%q approved=%t; want approved and an approval fact", row.Result, row.Approved)
	}
	if row.Operation != "git.ref.update" {
		t.Fatalf("protected verification operation = %q; want git.ref.update", row.Operation)
	}
	if row.ServiceID != fixture.git.ServiceID {
		t.Fatalf("protected verification service = %q; want the Git service %q", row.ServiceID, fixture.git.ServiceID)
	}
	if row.ResourceID != fixture.git.ProjectResourceID {
		t.Fatalf("protected verification resource = %q; want the registered resource %q",
			row.ResourceID, fixture.git.ProjectResourceID)
	}
	if row.BindingID != fixture.git.ProjectBindingID {
		t.Fatalf("protected verification binding = %q; want the registered binding %q",
			row.BindingID, fixture.git.ProjectBindingID)
	}
	if row.ExecutionID != evidence.executionID || row.ActionID != evidence.integrate.ActionID {
		t.Fatalf("protected verification execution/action = %q/%q; want %q/%q",
			row.ExecutionID, row.ActionID, evidence.executionID, evidence.integrate.ActionID)
	}
	if row.ActorID != evidence.executionActor || row.ActorID == "" {
		t.Fatalf("protected verification actor = %q; want the execution actor %q", row.ActorID, evidence.executionActor)
	}
	var rowSubject api.Subject
	if err := json.Unmarshal([]byte(row.Subject), &rowSubject); err != nil {
		t.Fatalf("decode protected verification subject %s: %v", row.Subject, err)
	}
	requireProtectedGoldenCanonicalSubject(t, "protected verification", rowSubject, evidence)

	// The stored approval assignment is exactly role/reviewer. It is decoded
	// from the durable column, never inferred from the committed policy.
	if got := evidence.approval.String(); got != "role/reviewer" {
		t.Fatalf("stored requested approval = %q; want role/reviewer", got)
	}
	if evidence.approval.Kind != "role" || evidence.approval.Role != "reviewer" || evidence.approval.ServiceID != "" {
		t.Fatalf("stored requested approval = %+v; want exactly role/reviewer", evidence.approval)
	}

	// The earlier review action in this same execution succeeded with the exact
	// authorized human pass and the same canonical subject.
	review := evidence.review
	if review.Assignment != "role/reviewer" {
		t.Fatalf("review assignment = %q; want role/reviewer", review.Assignment)
	}
	if review.Decision != "pass" {
		t.Fatalf("review decision = %q; want pass", review.Decision)
	}
	if review.Actor != evidence.reviewerPrincipal || review.Actor == "" {
		t.Fatalf("review actor = %q; want the dual-role reviewer %q", review.Actor, evidence.reviewerPrincipal)
	}
	if review.AnsweredAt == nil {
		t.Fatalf("review action carries no answered-at fact")
	}
	requireProtectedGoldenSubject(t, "review action", review.Subject, evidence)

	// Exactly one resource.verified/git.ref.updated event is linked to the
	// verification by causation; the Git service is the source principal and
	// this execution is the correlation.
	event := evidence.reportEvent
	if event.Type != "resource.verified" || event.ReportType != "git.ref.updated" {
		t.Fatalf("report event type/report type = %q/%q; want resource.verified/git.ref.updated", event.Type, event.ReportType)
	}
	if event.CausationID != row.ID {
		t.Fatalf("report event causation = %q; want the verification %q", event.CausationID, row.ID)
	}
	if event.SubjectID != fixture.git.ProjectResourceID {
		t.Fatalf("report event subject = %q; want the resource %q", event.SubjectID, fixture.git.ProjectResourceID)
	}
	if event.CorrelationID != evidence.executionID {
		t.Fatalf("report event correlation = %q; want this execution %q", event.CorrelationID, evidence.executionID)
	}
	if event.SourceKind != "service" || event.SourcePrincipal != fixture.git.ServiceID {
		t.Fatalf("report event source = %q/%q; want the Git service principal",
			event.SourceKind, event.SourcePrincipal)
	}

	// The exact report replay is receipt-first and changes no identity: the ref,
	// journal, verification and one linked event are the same facts.
	if !evidence.reportReplayed {
		t.Fatalf("the exact resource.report replay was not reported as replayed")
	}
	if evidence.replayEvent.ID != event.ID || evidence.replayEvent.CausationID != event.CausationID {
		t.Fatalf("report replay event = %+v; want the original %+v", evidence.replayEvent, event)
	}
	if evidence.mainAfterReplay != evidence.candidate {
		t.Fatalf("protected main after replay = %q; want the unchanged candidate %q", evidence.mainAfterReplay, evidence.candidate)
	}
	afterJournal := waitProtectedGoldenJournal(t, fixture, factory.GitProjectRepository, gitMainRef, evidence.candidate)
	if afterJournal.SourceEventID != journal.SourceEventID || afterJournal.VerificationID != journal.VerificationID ||
		afterJournal.ExpectedBase != journal.ExpectedBase || afterJournal.Candidate != journal.Candidate {
		t.Fatalf("protected journal changed after replay: before %+v after %+v", journal, afterJournal)
	}
	afterRow := fixture.verificationByID(t, row.ID)
	if afterRow.Result != row.Result || afterRow.Approved != row.Approved || afterRow.Subject != row.Subject ||
		afterRow.ExecutionID != row.ExecutionID || afterRow.ActionID != row.ActionID {
		t.Fatalf("protected verification changed after replay: before %+v after %+v", row, afterRow)
	}
	if count := protectedGoldenJournalCount(t, fixture, factory.GitProjectRepository, gitMainRef); count != 1 {
		t.Fatalf("reported protected journal entries after replay = %d; want exactly one", count)
	}

	// The task completed with exactly one execution in its history.
	if evidence.task.State != "done" {
		t.Fatalf("task state = %q after success; want done", evidence.task.State)
	}
	if len(evidence.task.ExecutionHistory) != 1 || evidence.task.ExecutionHistory[0] != evidence.executionID {
		t.Fatalf("task execution history = %v; want exactly [%s]", evidence.task.ExecutionHistory, evidence.executionID)
	}
	if evidence.task.CurrentExecution != "" && evidence.task.CurrentExecution != evidence.executionID {
		t.Fatalf("task current execution = %q; want %q", evidence.task.CurrentExecution, evidence.executionID)
	}

	// Every action printed its positive control after its own fail-closed
	// checks, and no credential reached the retained logs.
	for _, marker := range []string{
		"candidate-output-submitted",
		"test-outputs-submitted",
		"protected-integration-pushed",
		"repository-config-clean",
	} {
		if !strings.Contains(evidence.logs, marker) {
			t.Fatalf("retained workload logs are missing %q:\n%s", marker, evidence.logs)
		}
	}
	serviceToken, err := fixture.git.ServiceToken(t)
	if err != nil {
		t.Fatalf("read the mounted Git service credential for the leak scans: %v", err)
	}
	// Search every credential this run actually mounted: the implement token,
	// the test container's token (the product reuses an unexpired token between
	// consecutive containers) and the integrate token freshly minted after the
	// request entry, which is the value that signs the push to main. Each is
	// searched raw, as the bare Basic credential value and as the Basic header
	// form, together with the Git service credential.
	executionTokens := []string{evidence.executionToken, evidence.testToken, evidence.integrateToken}
	for _, token := range executionTokens {
		if token == "" {
			t.Fatalf("the scenario did not observe every workload container's execution credential")
		}
	}
	// Request entry clears the credential; the next container receives a new
	// nonce. That rotation is the product guarantee, so the integrate token must
	// not equal either pre-pass token. Consecutive containers without a pass
	// deliberately reuse the same live credential, so no distinctness is
	// asserted between implement and test.
	if evidence.integrateToken == evidence.executionToken || evidence.integrateToken == evidence.testToken {
		t.Fatalf("the integrate execution credential equals a pre-pass container's; want a credential minted after the request")
	}
	secrets := make([]string, 0, 3*len(executionTokens)+3)
	for _, token := range executionTokens {
		// The raw value, the bare Basic credential value
		// (`base64("kodama:"+token)`) and its header form. The bare form matters
		// because the runtime redacts the mounted raw value from retained logs
		// but not a derived encoding, so an echoed header's base64 is the real
		// residual leak vector.
		secrets = append(secrets, token, basicValue(token), "Basic "+basicValue(token))
	}
	secrets = append(secrets, serviceToken, basicValue(serviceToken), "Basic "+basicValue(serviceToken))
	for _, secret := range secrets {
		if strings.Contains(evidence.logs, secret) {
			t.Fatalf("a credential leaked into the retained workload logs")
		}
	}
	yamlBytes, _, err := fixture.ExecutionRow(t.Context(), evidence.executionID)
	if err != nil {
		t.Fatalf("read the admitted golden pipeline: %v", err)
	}
	for _, secret := range secrets {
		if strings.Contains(string(yamlBytes), secret) {
			t.Fatalf("a credential persisted in the admitted pipeline YAML")
		}
	}
	if leaks := fixture.git.ScanCredentialLeaks(t, secrets...); len(leaks) != 0 {
		t.Fatalf("credentials leaked into host artifacts %v", leaks)
	}
}

// requireProtectedGoldenCanonicalSubject asserts the five-field typed subject
// the protected hook supplies for one ref update.
func requireProtectedGoldenCanonicalSubject(t *testing.T, label string, subject api.Subject, evidence protectedGoldenEvidence) {
	t.Helper()
	fields := map[string]string{}
	for _, field := range subject.Fields {
		if field.Kind != "string" {
			t.Fatalf("%s subject field %q kind = %q; want a typed string", label, field.Name, field.Kind)
		}
		if field.Name == "" {
			t.Fatalf("%s subject carries an unnamed field", label)
		}
		if _, duplicate := fields[field.Name]; duplicate {
			t.Fatalf("%s subject carries duplicate field %q", label, field.Name)
		}
		fields[field.Name] = field.Value
	}
	want := map[string]string{
		"operation":     "git.ref.update",
		"resource":      evidence.fixture.git.ProjectResourceID,
		"ref":           gitMainRef,
		"expected_base": evidence.base,
		"candidate":     evidence.candidate,
	}
	if len(fields) != len(want) {
		t.Fatalf("%s subject has %d fields %v; want exactly the canonical five", label, len(fields), fields)
	}
	for name, value := range want {
		if got, ok := fields[name]; !ok || got != value {
			t.Fatalf("%s subject field %q = %q (present %t); want %q", label, name, got, ok, value)
		}
	}
}

// requireProtectedGoldenSubject asserts the resolved request subject is the
// canonical five-field subject.
func requireProtectedGoldenSubject(t *testing.T, label string, subject *api.Subject, evidence protectedGoldenEvidence) {
	t.Helper()
	if subject == nil {
		t.Fatalf("%s carries no resolved subject", label)
	}
	requireProtectedGoldenCanonicalSubject(t, label, *subject, evidence)
}

// protectedGoldenVerificationBy finds one verification row of an exact
// execution/action/operation/ref. The ref is read from the stored subject.
func protectedGoldenVerificationBy(t *testing.T, fixture *gitHarness, executionID, actionID, operation, ref string) factory.VerificationRow {
	t.Helper()
	rows, err := fixture.git.Verifications(t.Context())
	if err != nil {
		t.Fatalf("read verification rows: %v", err)
	}
	for _, row := range rows {
		if row.ExecutionID != executionID || row.ActionID != actionID || row.Operation != operation {
			continue
		}
		var subject api.Subject
		if err := json.Unmarshal([]byte(row.Subject), &subject); err != nil {
			continue
		}
		for _, field := range subject.Fields {
			if field.Name == "ref" && field.Value == ref {
				return row
			}
		}
	}
	t.Fatalf("no verification row for execution=%s action=%s operation=%s ref=%s",
		executionID, actionID, operation, ref)
	return factory.VerificationRow{}
}
