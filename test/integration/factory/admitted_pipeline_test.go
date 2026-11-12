//go:build integration

package factorytest

// Factory S11 row AC2: a Git push cannot mutate an admitted pipeline.
//
// The scenario admits one push-triggered pipeline while the first evidence is
// durable but the admission has not committed. A pipeline edit and a second Git
// push to the same ref then occur. Releasing the admission must run the first
// push's exact persisted first-selection YAML with its exact inputs, while the
// second push admits the edited definition for a different execution. The
// durable execution snapshots, ordered action names, pushed inputs, journal
// entries and stored pipeline bytes are all asserted; nothing is synthesized.
//
// The injected resolver is the only non-production seam. It fails the first
// admission attempt transiently, holds the second attempt after the selection
// commit, and delegates to the real digest resolver afterwards. That schedule is
// what makes "edit and a later push happen strictly after the first selection and
// strictly before its admission" observable instead of racy.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/test/support/factory"
)

const (
	admittedPipelineName = "admitted-pipeline"
	admittedPipelineRef  = "refs/heads/work/admitted-pipeline"
	admittedPipelineSvc  = "admitted-pipeline-starter"
	admittedPipelineWait = 5 * time.Minute
)

// TestGitPushCannotMutateAdmittedPipeline proves a Git push and a pipeline edit
// cannot rewrite an already persisted push admission: the replay admits the
// first selection's exact bytes and inputs, while a later push admits the
// edited definition as a separate execution.
func TestGitPushCannotMutateAdmittedPipeline(t *testing.T) {
	resolver := newDeferredAdmissionResolver(factory.DigestResolver{})
	fixture := startGitHarnessWith(t, factory.WithImageResolver(resolver))

	fixture.MustREST(fixture.ProjectToken, "service.create", api.ServiceCreateRequest{
		ProjectID: factory.ProjectID,
		ServiceID: admittedPipelineSvc,
		Roles:     []string{"engineer"},
	}, nil, "create-"+admittedPipelineSvc)

	directory := fixture.clone(t, fixture.engineerToken, factory.GitProjectRepository, "admitted-pipeline")
	firstCommit := fixture.commit(t, directory, "admitted first push")
	secondCommit := fixture.commit(t, directory, "admitted second push")
	if firstCommit == secondCommit {
		t.Fatalf("fixture commits are identical")
	}

	firstDocument := admittedPipelineDocument(t, fixture, "admit-v1", firstCommit, "admitted-pipeline-v1")
	applied := fixture.applyPipeline(admittedPipelineName, firstDocument)
	if applied.PipelineID == "" || applied.Version == 0 {
		t.Fatalf("pipeline.apply returned %+v; want an admitted pipeline and version", applied)
	}

	// The first push selects the current definition durably. The first admission
	// attempt fails transiently and the second holds at the resolver, so the edit
	// and the second push below happen after the selection commit and before the
	// replay admission.
	if stderr, code := fixture.push(directory, fixture.engineerToken,
		firstCommit+":"+admittedPipelineRef); code != 0 {
		t.Fatalf("first admitted push: exit %d: %s", code, stderr)
	}
	resolver.waitHeld(t)
	if executions := fixture.pipelineExecutions(t, applied.PipelineID); len(executions) != 0 {
		t.Fatalf("pipeline admitted %d executions before the held replay", len(executions))
	}

	// Edit the definition and push again while the first admission is held. The
	// edited action asserts the second pushed commit; the first selection's
	// action asserts the first commit. A replay that re-read the edited bytes
	// would therefore fail the first execution's own command.
	secondDocument := admittedPipelineDocument(t, fixture, "admit-v2", secondCommit, "admitted-pipeline-v2")
	updated := fixture.MustREST(fixture.ProjectToken, "pipeline.update", api.PipelineUpdateRequest{
		ProjectID:  factory.ProjectID,
		PipelineID: applied.PipelineID,
		Name:       admittedPipelineName,
		YAML:       secondDocument,
	}, &applied.Version, "update-"+admittedPipelineName)
	var updateResult api.PipelineUpdateResult
	fixture.Decode(updated, &updateResult)
	if updateResult.Version <= applied.Version {
		t.Fatalf("pipeline.update version = %d; want an advance past %d", updateResult.Version, applied.Version)
	}
	if stderr, code := fixture.push(directory, fixture.engineerToken,
		secondCommit+":"+admittedPipelineRef); code != 0 {
		t.Fatalf("second admitted push: exit %d: %s", code, stderr)
	}
	if remote := fixture.git.RemoteRef(t, factory.GitProjectRepository, admittedPipelineRef); remote != secondCommit {
		t.Fatalf("admitted work ref = %q after the second push; want %q", remote, secondCommit)
	}
	if executions := fixture.pipelineExecutions(t, applied.PipelineID); len(executions) != 0 {
		t.Fatalf("pipeline admitted %d executions while the first admission was held", len(executions))
	}

	// Release the held admission. The first event replays the persisted first
	// selection; only afterwards is the second push's separate selection made
	// from the edited current definition.
	resolver.releaseAll()
	executions := waitAdmittedPipelineExecutions(t, fixture, applied.PipelineID, 2)
	for _, execution := range executions {
		t.Cleanup(func() { fixture.stopExecution(t, execution.ExecutionID) })
	}

	// Read the stored pipeline back: the edit is current for future admissions,
	// so the assertion below is not "the edit failed".
	stored := readAdmittedPipeline(t, fixture, applied.PipelineID)
	if stored.Version != updateResult.Version {
		t.Fatalf("stored pipeline version = %d; want the edited %d", stored.Version, updateResult.Version)
	}
	if !bytes.Equal(stored.YAML, secondDocument) {
		t.Fatalf("stored pipeline YAML = %q; want the edited definition %q", stored.YAML, secondDocument)
	}

	seen := make(map[string]bool, 2)
	for _, execution := range executions {
		finished := fixture.waitExecutionSucceeded(t, execution.ExecutionID)
		admitted, digest, err := fixture.ExecutionRow(t.Context(), execution.ExecutionID)
		if err != nil {
			t.Fatalf("read admitted YAML of execution %s: %v", execution.ExecutionID, err)
		}
		names := admittedActionNames(t, finished)
		switch {
		case bytes.Equal(admitted, firstDocument):
			wantSHA := sha256.Sum256(firstDocument)
			if !bytes.Equal(digest, wantSHA[:]) {
				t.Fatalf("first-push execution digest = %x; want %x", digest, wantSHA)
			}
			if seen["first"] {
				t.Fatalf("two executions admitted the first selection")
			}
			seen["first"] = true
			if len(names) != 1 || names[0] != "admit-v1" {
				t.Fatalf("first-push execution actions = %v; want [admit-v1]", names)
			}
			logs := waitAdmittedExecutionLog(t, fixture, finished, "admitted-pipeline-v1")
			if !strings.Contains(logs, "admitted-pipeline-v1") {
				t.Fatalf("first-push execution did not run the first selection's action:\n%s", logs)
			}
			if strings.Contains(logs, "admitted-pipeline-v2") {
				t.Fatalf("first-push execution ran the edited action:\n%s", logs)
			}
		case bytes.Equal(admitted, secondDocument):
			wantSHA := sha256.Sum256(secondDocument)
			if !bytes.Equal(digest, wantSHA[:]) {
				t.Fatalf("second-push execution digest = %x; want %x", digest, wantSHA)
			}
			if seen["second"] {
				t.Fatalf("two executions admitted the edited definition")
			}
			seen["second"] = true
			if len(names) != 1 || names[0] != "admit-v2" {
				t.Fatalf("second-push execution actions = %v; want [admit-v2]", names)
			}
			logs := waitAdmittedExecutionLog(t, fixture, finished, "admitted-pipeline-v2")
			if !strings.Contains(logs, "admitted-pipeline-v2") {
				t.Fatalf("second-push execution did not run the edited action:\n%s", logs)
			}
			if strings.Contains(logs, "admitted-pipeline-v1") {
				t.Fatalf("second-push execution ran the first selection's action:\n%s", logs)
			}
		default:
			t.Fatalf("execution %s admitted neither exact definition:\n%s", execution.ExecutionID, admitted)
		}
	}
	if !seen["first"] || !seen["second"] {
		t.Fatalf("admitted definitions seen = %v; want one first selection and one edited definition", seen)
	}

	// Both pushes produced real served journal facts. The first push's fact
	// cannot have been replaced by the second: each candidate has its own
	// reported entry.
	firstJournal := waitProtectedGoldenJournal(t, fixture, factory.GitProjectRepository,
		admittedPipelineRef, firstCommit)
	secondJournal := waitProtectedGoldenJournal(t, fixture, factory.GitProjectRepository,
		admittedPipelineRef, secondCommit)
	if firstJournal.SourceEventID == secondJournal.SourceEventID {
		t.Fatalf("both pushes share source event %q", firstJournal.SourceEventID)
	}
	if count := protectedGoldenJournalCount(t, fixture, factory.GitProjectRepository, admittedPipelineRef); count != 2 {
		t.Fatalf("reported journal entries for %s = %d; want exactly the two pushes", admittedPipelineRef, count)
	}
}

// admittedPipelineDocument renders one exact push-triggered pipeline. The action
// succeeds only when the admitted input is the given commit and prints a marker
// that distinguishes the two definitions in retained logs.
func admittedPipelineDocument(t *testing.T, fixture *gitHarness, action, commit, marker string) []byte {
	t.Helper()
	document := fmt.Sprintf(`repository: %s
service: %s
on:
  push: [%s]
inputs: [repository, ref, commit, previous_commit]
actions:
  - name: %s
    image: %s
    timeout: 10m
    commands:
      - 'test "$(cat /kodama/inputs/commit)" = %s'
      - "printf '%s\\n'"
`, fixture.git.ProjectResourceID, admittedPipelineSvc, admittedPipelineRef,
		action, fixture.Image.Reference, commit, marker)
	return []byte(document)
}

// readAdmittedPipeline reads one stored pipeline through the public operation.
func readAdmittedPipeline(t *testing.T, fixture *gitHarness, pipelineID string) api.PipelineReadResult {
	t.Helper()
	var read api.PipelineReadResult
	fixture.Decode(fixture.MustREST(fixture.ProjectToken, "pipeline.read", api.PipelineReadRequest{
		ProjectID:  factory.ProjectID,
		PipelineID: pipelineID,
	}, nil, ""), &read)
	return read
}

// waitAdmittedPipelineExecutions waits for one pipeline to have exactly want
// executions and returns them.
func waitAdmittedPipelineExecutions(t *testing.T, fixture *gitHarness, pipelineID string, want int) []api.ExecutionSummary {
	t.Helper()
	var found []api.ExecutionSummary
	fixture.git.WaitFor(t,
		fmt.Sprintf("pipeline %s executions=%d", pipelineID, want),
		admittedPipelineWait, func() bool {
			found = fixture.pipelineExecutions(t, pipelineID)
			return len(found) == want
		})
	return found
}

// admittedActionNames returns the admitted action names in order.
func admittedActionNames(t *testing.T, execution api.ExecutionInspectResult) []string {
	t.Helper()
	names := make([]string, 0, len(execution.Actions))
	for _, action := range execution.Actions {
		names = append(names, action.Name)
	}
	return names
}

// deferredAdmissionResolver defers exactly one trigger admission, holds the next
// attempt after the durable first selection, then delegates to the real resolver.
type deferredAdmissionResolver struct {
	inner pipeline.Resolver

	mu      sync.Mutex
	calls   int
	held    chan struct{}
	release chan struct{}
	once    sync.Once
}

// newDeferredAdmissionResolver returns one unheld resolver over the real inner
// resolver.
func newDeferredAdmissionResolver(inner pipeline.Resolver) *deferredAdmissionResolver {
	return &deferredAdmissionResolver{
		inner:   inner,
		held:    make(chan struct{}),
		release: make(chan struct{}),
	}
}

// Resolve implements pipeline.Resolver.
func (resolver *deferredAdmissionResolver) Resolve(ctx context.Context, request pipeline.ResolveRequest) (pipeline.ResolvedImage, error) {
	resolver.mu.Lock()
	resolver.calls++
	call := resolver.calls
	resolver.mu.Unlock()

	switch call {
	case 1:
		return pipeline.ResolvedImage{}, errors.New("first admission attempt deferred")
	case 2:
		resolver.once.Do(func() { close(resolver.held) })
		select {
		case <-resolver.release:
		case <-ctx.Done():
			return pipeline.ResolvedImage{}, ctx.Err()
		}
		return pipeline.ResolvedImage{}, errors.New("held admission attempt deferred")
	default:
		return resolver.inner.Resolve(ctx, request)
	}
}

// waitHeld waits until the second admission attempt is blocked at the resolver.
func (resolver *deferredAdmissionResolver) waitHeld(t *testing.T) {
	t.Helper()
	select {
	case <-resolver.held:
	case <-time.After(admittedPipelineWait):
		t.Fatalf("the push admission never reached the held resolver")
	}
}

// releaseAll releases every held admission attempt.
func (resolver *deferredAdmissionResolver) releaseAll() {
	select {
	case <-resolver.release:
	default:
		close(resolver.release)
	}
}

// waitAdmittedExecutionLog waits for the action output to be collected after a
// succeeded execution. Log collection is asynchronous and can lag a fast
// execution under full-suite load, so the assertion polls for the marker
// instead of reading the logs exactly once.
func waitAdmittedExecutionLog(t *testing.T, fixture *gitHarness, execution api.ExecutionInspectResult, want string) string {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		logs := fixture.executionLogs(t, execution)
		if strings.Contains(logs, want) || time.Now().After(deadline) {
			return logs
		}
		time.Sleep(100 * time.Millisecond)
	}
}
