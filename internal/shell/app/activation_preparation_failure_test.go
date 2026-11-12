package app

// A preparation failure that happens AFTER the activation wrapper issued its
// first execution credential must still settle the execution as failed. The
// credential commit advances the shared execution row version
// inside the Kernel's Prepare call, so the Kernel's immediately following
// preparation-failure commit is based on a stale snapshot. The composition
// rebases exactly that one commit onto the version its own credential commit
// produced; every other transition keeps the ordinary compare-and-commit
// boundary, so a genuine concurrent transition still wins.
//
// The tests drive the composed app through the public edge and a recording
// runtime whose adapter refuses Prepare. No process, container, database or
// broker is started.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	kernelengine "github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/kernel/flow"
)

// TestActivationPreparationFailureAfterCredentialIssue covers the failure paths
// where the activation wrapper already delivered an execution credential.
func TestActivationPreparationFailureAfterCredentialIssue(t *testing.T) {
	t.Run("first credential issue then inner preparation failure fails the execution", func(t *testing.T) {
		fixture := newActivationFixture(t, nil)
		adapterFailure := errors.New("runtime adapter refused the preparation")
		fixture.runtime.failPrepare("inspect", adapterFailure)
		app := fixture.environment.startApp(t)
		handler := app.Handler()
		grantProjectEngineer(t, handler, "dev-admin-token")
		applied := applyActivationPipeline(t, handler, "dev-project-token", activationPipeline(
			activationAction{name: "inspect"},
		), "preparation-failure-pipeline")
		started := startActivationTask(t, handler, "dev-project-token", applied.PipelineID, "preparation-failure-task")

		prepares := waitForActivationPrepares(t, fixture.runtime, 1)
		credential := prepares[0].Credential
		if credential == "" {
			t.Fatal("the runtime received no execution credential before its preparation failure")
		}

		inspected := waitForExecutionState(t, handler, "dev-project-token", started.ExecutionID, "failed")
		if inspected.Failure == nil || inspected.Failure.Kind != "preparation" {
			t.Fatalf("failure = %+v; want kind preparation", inspected.Failure)
		}
		if fixture.runtime.startCount() != 0 {
			t.Fatalf("runtime started %d workloads; want none", fixture.runtime.startCount())
		}
		if fixture.runtime.prepareCount() != 1 {
			t.Fatalf("runtime saw %d preparations; want exactly one", fixture.runtime.prepareCount())
		}

		// The credential issued before the failure was cleared by the failure
		// commit and no longer authenticates.
		recorder, _, _ := rawExecutionInspect(t, handler, credential, started.ExecutionID)
		if recorder.Code == api.HTTPStatusSuccess {
			t.Fatal("the credential issued before the preparation failure still authenticates")
		}
		if strings.Contains(recorder.Body.String(), credential) {
			t.Fatal("the revoked credential refusal echoed the presented credential")
		}
		if containsMaterial(fixture.environment.logs.String(), credential) {
			t.Fatal("composition logs contain the failed credential")
		}
		if strings.Contains(inspected.Failure.Detail, credential) {
			t.Fatal("the preparation failure detail contains the credential")
		}
	})

	t.Run("second activation reuses the committed credential then fails preparation", func(t *testing.T) {
		fixture := newActivationFixture(t, nil, "first")
		adapterFailure := errors.New("runtime adapter refused the second preparation")
		fixture.runtime.failPrepare("second", adapterFailure)
		app := fixture.environment.startApp(t)
		handler := app.Handler()
		grantProjectEngineer(t, handler, "dev-admin-token")
		applied := applyActivationPipeline(t, handler, "dev-project-token", activationPipeline(
			activationAction{name: "first"},
			activationAction{name: "second"},
		), "reuse-preparation-failure-pipeline")
		started := startActivationTask(t, handler, "dev-project-token", applied.PipelineID, "reuse-preparation-failure-task")

		prepares := waitForActivationPrepares(t, fixture.runtime, 2)
		if prepares[0].Credential == "" || prepares[1].Credential == "" {
			t.Fatal("an activation received an empty execution credential")
		}
		if prepares[0].Credential != prepares[1].Credential {
			t.Fatal("the second activation did not reuse the committed credential")
		}
		credential := prepares[1].Credential

		inspected := waitForExecutionState(t, handler, "dev-project-token", started.ExecutionID, "failed")
		if inspected.Failure == nil || inspected.Failure.Kind != "preparation" {
			t.Fatalf("failure = %+v; want kind preparation", inspected.Failure)
		}
		if fixture.runtime.startCount() != 1 {
			t.Fatalf("runtime started %d workloads; want only the first action", fixture.runtime.startCount())
		}
		recorder, _, _ := rawExecutionInspect(t, handler, credential, started.ExecutionID)
		if recorder.Code == api.HTTPStatusSuccess {
			t.Fatal("the reused credential still authenticates after the failed preparation")
		}
	})

	t.Run("declared secret resolves then inner preparation failure fails the execution", func(t *testing.T) {
		fixture := newActivationFixture(t, activationRules(true, activationSecretName))
		adapterFailure := errors.New("runtime adapter refused the preparation")
		fixture.runtime.failPrepare("inspect", adapterFailure)
		app := fixture.environment.startApp(t)
		handler := app.Handler()
		grantProjectEngineer(t, handler, "dev-admin-token")
		writeActivationSecret(t, handler, "dev-project-token", activationSecretName, activationSecretValue, "failure-secret-write")
		applied := applyActivationPipeline(t, handler, "dev-project-token", activationPipeline(
			activationAction{name: "inspect", secrets: []id.SecretName{activationSecretName}},
		), "secret-preparation-failure-pipeline")
		started := startActivationTask(t, handler, "dev-project-token", applied.PipelineID, "secret-preparation-failure-task")

		prepares := waitForActivationPrepares(t, fixture.runtime, 1)
		credential := prepares[0].Credential
		if credential == "" {
			t.Fatal("the runtime received no execution credential before its preparation failure")
		}
		if len(prepares[0].Secrets) != 1 || prepares[0].Secrets[0].Value != activationSecretValue {
			t.Fatal("the declared secret value was not delivered before the adapter refused the preparation")
		}

		inspected := waitForExecutionState(t, handler, "dev-project-token", started.ExecutionID, "failed")
		if inspected.Failure == nil || inspected.Failure.Kind != "preparation" {
			t.Fatalf("failure = %+v; want kind preparation", inspected.Failure)
		}
		if fixture.runtime.startCount() != 0 {
			t.Fatalf("runtime started %d workloads; want none", fixture.runtime.startCount())
		}
		recorder, _, _ := rawExecutionInspect(t, handler, credential, started.ExecutionID)
		if recorder.Code == api.HTTPStatusSuccess {
			t.Fatal("the credential issued before the preparation failure still authenticates")
		}
		if containsMaterial(fixture.environment.logs.String(), activationSecretValue, credential) {
			t.Fatal("composition logs contain activation secret or credential material")
		}
		if containsMaterial(recorder.Body.String(), activationSecretValue, credential) {
			t.Fatal("the execution result rendered activation secret or credential material")
		}
		if containsMaterial(inspected.Failure.Detail, activationSecretValue, credential) {
			t.Fatal("the preparation failure detail contains activation secret or credential material")
		}
	})

	t.Run("second activation reissues an expired credential then fails preparation", func(t *testing.T) {
		fixture := newActivationFixture(t, nil, "first")
		fixture.runtime.pauseExits()
		adapterFailure := errors.New("runtime adapter refused the second preparation")
		fixture.runtime.failPrepare("second", adapterFailure)
		app := fixture.environment.startApp(t)
		handler := app.Handler()
		grantProjectEngineer(t, handler, "dev-admin-token")
		applied := applyActivationPipeline(t, handler, "dev-project-token", activationPipeline(
			activationAction{name: "first"},
			activationAction{name: "second"},
		), "reissue-preparation-failure-pipeline")
		started := startActivationTask(t, handler, "dev-project-token", applied.PipelineID, "reissue-preparation-failure-task")

		first := waitForActivationPrepares(t, fixture.runtime, 1)[0]
		if first.Credential == "" {
			t.Fatal("the first activation received an empty execution credential")
		}
		// The 8h credential issued at the first activation is expired by the
		// second activation, so the wrapper must issue a replacement.
		fixture.environment.clock.Advance(9 * time.Hour)
		fixture.runtime.releaseExits()

		prepares := waitForActivationPrepares(t, fixture.runtime, 2)
		if prepares[1].Credential == "" {
			t.Fatal("the second activation received no replacement credential")
		}
		if prepares[1].Credential == first.Credential {
			t.Fatal("the second activation reused an expired credential")
		}
		credential := prepares[1].Credential

		inspected := waitForExecutionState(t, handler, "dev-project-token", started.ExecutionID, "failed")
		if inspected.Failure == nil || inspected.Failure.Kind != "preparation" {
			t.Fatalf("failure = %+v; want kind preparation", inspected.Failure)
		}
		if fixture.runtime.startCount() != 1 {
			t.Fatalf("runtime started %d workloads; want only the first action", fixture.runtime.startCount())
		}
		recorder, _, _ := rawExecutionInspect(t, handler, credential, started.ExecutionID)
		if recorder.Code == api.HTTPStatusSuccess {
			t.Fatal("the replacement credential still authenticates after the failed preparation")
		}
	})

	t.Run("a concurrent version transition still wins over the failure commit", func(t *testing.T) {
		fixture := newActivationFixture(t, nil)
		adapterFailure := errors.New("runtime adapter refused the preparation")
		fixture.runtime.failPrepare("inspect", adapterFailure)
		app := fixture.environment.startApp(t)
		handler := app.Handler()
		grantProjectEngineer(t, handler, "dev-admin-token")
		applied := applyActivationPipeline(t, handler, "dev-project-token", activationPipeline(
			activationAction{name: "inspect"},
		), "concurrent-preparation-failure-pipeline")

		reached, release := fixture.runtime.holdPrepare()
		started := startActivationTask(t, handler, "dev-project-token", applied.PipelineID, "concurrent-preparation-failure-task")
		select {
		case <-reached:
		case <-time.After(5 * time.Second):
			t.Fatal("the runtime adapter never reached the blocked preparation")
		}
		defer release()

		credential := fixture.runtime.prepareSnapshot()[0].Credential
		if credential == "" {
			t.Fatal("the runtime received no execution credential before the concurrent transition")
		}
		// The credential commit advanced the execution row. A genuine concurrent
		// transition now wins the next commit boundary; the rebased preparation
		// failure must conflict rather than overwrite it.
		cancelExecution(t, handler, started.ExecutionID)
		release()

		inspected := waitForExecutionState(t, handler, "dev-project-token", started.ExecutionID, "cancelled")
		if inspected.Failure != nil {
			t.Fatalf("the concurrent cancellation was replaced by failure %+v", inspected.Failure)
		}
		if fixture.runtime.startCount() != 0 {
			t.Fatalf("runtime started %d workloads; want none", fixture.runtime.startCount())
		}
		recorder, _, _ := rawExecutionInspect(t, handler, credential, started.ExecutionID)
		if recorder.Code == api.HTTPStatusSuccess {
			t.Fatal("the credential still authenticates after the winning cancellation")
		}
	})
}

// TestActivationCredentialRespectsActionDeadline pins that the activation
// credential lifetime is bounded by the persisted action deadline even when the
// eight-hour maximum is larger. The composed action timeout is two hours
// so the deadline, not the cap, must decide the committed expiry.
func TestActivationCredentialRespectsActionDeadline(t *testing.T) {
	shared := newActivationShared(t)
	config := shared.config(t, nil)
	config.DefaultActionTimeout = 2 * time.Hour
	app := startComposedApp(t, config)
	t.Cleanup(func() { closeApp(t, app) })
	handler := app.Handler()
	grantProjectEngineer(t, handler, "dev-admin-token")
	applied := applyActivationPipeline(t, handler, "dev-project-token", activationPipeline(
		activationAction{name: "inspect"},
	), "deadline-pipeline")
	started := startActivationTask(t, handler, "dev-project-token", applied.PipelineID, "deadline-task")

	prepares := waitForActivationPrepares(t, shared.runtime, 1)
	credential := prepares[0].Credential
	if credential == "" {
		t.Fatal("the workload received no execution credential")
	}

	issuedAt := shared.clock.Now()
	expiry := committedCredentialExpiry(t, shared, started.ExecutionID)
	if !expiry.Equal(issuedAt.Add(2 * time.Hour)) {
		t.Fatalf("credential expiry = %v; want the two-hour action deadline %v", expiry, issuedAt.Add(2*time.Hour))
	}
	if expiry.Sub(issuedAt) >= 8*time.Hour {
		t.Fatalf("credential lifetime = %v; want it bounded below the eight-hour cap", expiry.Sub(issuedAt))
	}

	// Just before the action deadline the credential still authenticates.
	shared.clock.Advance(2*time.Hour - time.Minute)
	recorder, _, _ := rawExecutionInspect(t, handler, credential, started.ExecutionID)
	if recorder.Code != api.HTTPStatusSuccess {
		t.Fatalf("the activation credential did not authenticate just before the action deadline: status=%d", recorder.Code)
	}

	// Past the action deadline it stops authenticating.
	shared.clock.Advance(2 * time.Minute)
	recorder, _, _ = rawExecutionInspect(t, handler, credential, started.ExecutionID)
	if recorder.Code == api.HTTPStatusSuccess {
		t.Fatal("the activation credential authenticated past the action deadline")
	}
}

// TestPreparationFailureRebaseIsNarrow pins the safety boundary of the
// preparation-failure rebase: it moves only the preparation-failure commit that
// follows this composition's own credential commit, and it never overrides a
// version the row no longer has.
func TestPreparationFailureRebaseIsNarrow(t *testing.T) {
	executionID := id.ExecutionID("execution-1")
	preparationFailure := func(expected uint64) flow.Commit {
		return flow.Commit{
			ExecutionID:              executionID,
			ExpectedExecutionVersion: expected,
			Failure:                  &kernel.Failure{Kind: kernel.FailurePreparation, Reason: "preparation_failed"},
		}
	}

	t.Run("rebases exactly the post-credential revision", func(t *testing.T) {
		inner := &rebaseState{version: 10}
		versions := &fixedIssuedVersions{version: 10, ok: true}
		state := newPreparationState(inner, versions)
		if _, err := state.Commit(context.Background(), preparationFailure(9)); err != nil {
			t.Fatalf("rebased preparation failure: %v", err)
		}
		if len(inner.commits) != 1 || inner.commits[0].ExpectedExecutionVersion != 10 {
			t.Fatalf("committed expected versions = %+v; want one commit at 10", inner.commits)
		}
		if versions.ok {
			t.Fatal("the consumed credential revision was not forgotten")
		}
	})

	t.Run("does not rewrite an unrelated failure commit", func(t *testing.T) {
		inner := &rebaseState{version: 10}
		state := newPreparationState(inner, &fixedIssuedVersions{version: 10, ok: true})
		commit := preparationFailure(7)
		_, err := state.Commit(context.Background(), commit)
		if class, ok := fault.ClassOf(err); !ok || class != fault.VersionConflict {
			t.Fatalf("unrelated failure commit error = %v; want version conflict", err)
		}
		if len(inner.commits) != 0 {
			t.Fatal("an unrelated failure commit was applied")
		}
	})

	t.Run("does not rewrite a non-preparation failure", func(t *testing.T) {
		inner := &rebaseState{version: 10}
		state := newPreparationState(inner, &fixedIssuedVersions{version: 10, ok: true})
		commit := preparationFailure(9)
		commit.Failure = &kernel.Failure{Kind: kernel.FailureRuntime, Reason: "workload_exited"}
		_, err := state.Commit(context.Background(), commit)
		if class, ok := fault.ClassOf(err); !ok || class != fault.VersionConflict {
			t.Fatalf("non-preparation failure commit error = %v; want version conflict", err)
		}
		if len(inner.commits) != 0 {
			t.Fatal("a non-preparation failure commit was applied")
		}
	})

	t.Run("a concurrent transition still wins", func(t *testing.T) {
		inner := &rebaseState{version: 11}
		state := newPreparationState(inner, &fixedIssuedVersions{version: 10, ok: true})
		_, err := state.Commit(context.Background(), preparationFailure(9))
		if class, ok := fault.ClassOf(err); !ok || class != fault.VersionConflict {
			t.Fatalf("concurrent transition error = %v; want version conflict", err)
		}
		if len(inner.commits) != 0 {
			t.Fatal("the preparation failure overwrote a concurrent transition")
		}
	})
}

// rebaseState is a minimal Kernel state port that enforces the same
// compare-and-commit boundary the store adapter does.
type rebaseState struct {
	version uint64
	commits []flow.Commit
}

func (state *rebaseState) Load(context.Context, id.ExecutionID) (kernel.Execution, error) {
	return kernel.Execution{Version: state.version}, nil
}

func (state *rebaseState) Scan(context.Context, kernelengine.ScanRequest) ([]id.ExecutionID, error) {
	return nil, nil
}

func (state *rebaseState) Commit(_ context.Context, commit flow.Commit) (kernel.Execution, error) {
	if commit.ExpectedExecutionVersion != state.version {
		return kernel.Execution{}, fault.New(fault.VersionConflict, "stale execution version")
	}
	state.version++
	state.commits = append(state.commits, commit)
	return kernel.Execution{ID: commit.ExecutionID, Version: state.version}, nil
}

// fixedIssuedVersions is a fixed issuedCredentialVersions source.
type fixedIssuedVersions struct {
	version uint64
	ok      bool
}

func (versions *fixedIssuedVersions) issuedVersion(id.ExecutionID) (uint64, bool) {
	return versions.version, versions.ok
}

func (versions *fixedIssuedVersions) forgetIssuedVersion(id.ExecutionID) {
	versions.ok = false
}

// committedCredentialExpiry reads the expiry the activation credential commit
// persisted on the execution row.
func committedCredentialExpiry(t *testing.T, shared *activationShared, executionID string) time.Time {
	t.Helper()
	row, err := shared.store.Controller().Read(context.Background(), "executions", executionID)
	if err != nil {
		t.Fatalf("read execution row: %v", err)
	}
	value, ok := row.Values["token_expires_at"]
	if !ok || value == nil {
		t.Fatal("the execution row has no committed credential expiry")
	}
	expiry, ok := value.(time.Time)
	if !ok {
		t.Fatalf("committed credential expiry has type %T", value)
	}
	return expiry
}
