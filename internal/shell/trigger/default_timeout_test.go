package trigger_test

// These tests wire the real pipeline admission service. The other trigger tests
// use an admission fake that ignores AdmitRequest.DefaultTimeout, which hides
// what these cases pin: a push whose actions declare no timeout must resolve
// the installation default instead of reaching admission with a zero fallback
// and being permanently refused. The start unit's admitted actions are exactly
// what the real work adapter persists as actions.timeout_ms, so asserting them
// here proves the resolved deadline reaches the durable start commit without
// importing a Shell adapter into this capability package.

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/trigger"
)

const (
	// resolvedDigest is a syntactically valid immutable sha256 digest.
	resolvedDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	// invalidResolvedDigest is not a valid immutable digest, so admission
	// refuses the pipeline as an unsupported capability.
	invalidResolvedDigest = "not-a-digest"
)

// staticPipelineReader returns the one current definition the processor's
// admission call asks for.
type staticPipelineReader struct {
	definition pipeline.Definition
}

func (reader staticPipelineReader) Get(_ context.Context, _ tenancy.Scope, _ pipeline.PipelineID) (pipeline.Definition, error) {
	return reader.definition, nil
}

func (reader staticPipelineReader) ByName(_ context.Context, _ tenancy.Scope, _ string) (pipeline.Definition, error) {
	return reader.definition, nil
}

func (reader staticPipelineReader) List(_ context.Context, _ tenancy.Scope) ([]pipeline.Definition, error) {
	return []pipeline.Definition{reader.definition}, nil
}

// fixedResolver resolves every reference to the configured digest.
type fixedResolver struct {
	digest string
}

func (resolver fixedResolver) Resolve(_ context.Context, request pipeline.ResolveRequest) (pipeline.ResolvedImage, error) {
	return pipeline.ResolvedImage{Reference: request.Reference, Digest: resolver.digest}, nil
}

// newRealAdmission wires the real admission service over the supplied current
// definition and registry answer.
func newRealAdmission(t *testing.T, definition pipeline.Definition, digest string) *pipeline.AdmissionService {
	t.Helper()
	admission, err := pipeline.NewAdmissionService(pipeline.AdmissionConfig{
		Resolver: fixedResolver{digest: digest},
		Reader:   staticPipelineReader{definition: definition},
	})
	if err != nil {
		t.Fatalf("NewAdmissionService: %v", err)
	}
	return admission
}

// newRealAdmissionProcessor wires one processor whose admission seam is the
// real service and whose other ports are the deterministic fakes.
func newRealAdmissionProcessor(t *testing.T, fixture *pushFixture, admission trigger.AdmissionPort, defaultTimeout time.Duration) *trigger.Processor {
	t.Helper()
	processor, err := trigger.NewProcessor(trigger.ProcessorConfig{
		Events:         fixture.events,
		Candidates:     fixture.candidates,
		Selections:     fixture.selections,
		Select:         fixture.selections,
		Admission:      admission,
		Start:          fixture.start,
		Completion:     fixture.completion,
		Clock:          fakeClock{},
		IDs:            &fakeIDs{},
		BatchSize:      16,
		DefaultTimeout: defaultTimeout,
	})
	if err != nil {
		t.Fatalf("NewProcessor: %v", err)
	}
	return processor
}

// pushYAMLWithTimeout renders a valid push pipeline whose single container
// action optionally declares an explicit timeout.
func pushYAMLWithTimeout(timeout string) string {
	yaml := "repository: repo-1\n" +
		"service: svc-a\n" +
		"on:\n" +
		"  push: [refs/heads/main]\n" +
		"actions:\n" +
		"  - name: test\n" +
		"    image: registry.example.com/tools:1\n"
	if timeout != "" {
		yaml += "    timeout: " + timeout + "\n"
	}
	yaml += "    commands:\n" +
		"      - echo ok\n"
	return yaml
}

// pushFixtureWithYAML prepares one accepted push against a candidate with the
// exact supplied YAML.
func pushFixtureWithYAML(t *testing.T, yaml string) (*pushFixture, pipeline.Definition) {
	t.Helper()
	fixture := newPushFixture(t)
	scope := testScope()
	definition := testDefinition("pipeline-a", yaml, 1, "repo-1", "svc-a", []string{"refs/heads/main"})
	fixture.candidates.set(testCandidate(definition, testBinding(scope, "repo-1", "svc-a", 1)))
	fixture.events.setRows(pushEvent(1, "event-1", "repo-1", "svc-a", "refs/heads/main", "base-1", "cand-1"))
	return fixture, definition
}

// TestPushWithoutActionTimeoutUsesInstallationDefault covers the lost push: the
// real admission service resolves the missing action timeout from the
// installation default, so the start unit carries the resolved deadline instead
// of a permanent refusal.
func TestPushWithoutActionTimeoutUsesInstallationDefault(t *testing.T) {
	yaml := pushYAMLWithTimeout("")
	fixture, definition := pushFixtureWithYAML(t, yaml)
	admission := newRealAdmission(t, definition, resolvedDigest)
	processor := newRealAdmissionProcessor(t, fixture, admission, 5*time.Minute)

	result, err := processor.Process(context.Background())
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(result.Refused) != 0 {
		t.Fatalf("refused = %v; a push without an explicit timeout must not be refused", result.Refused)
	}
	if len(result.Started) != 1 {
		t.Fatalf("started = %v; want one execution", result.Started)
	}
	if fixture.start.callCount() != 1 {
		t.Fatalf("Start called %d times; want one selected start", fixture.start.callCount())
	}
	unit := fixture.start.call(0)
	if unit.Status != trigger.StatusSelected || unit.NewExecutionID != result.Started[0] {
		t.Fatalf("start unit = %+v; want the selected execution %q", unit, result.Started[0])
	}
	if len(unit.Admission.Actions) != 1 {
		t.Fatalf("admitted actions = %d; want exactly one", len(unit.Admission.Actions))
	}
	if timeout := unit.Admission.Actions[0].Timeout; timeout != 5*time.Minute {
		t.Fatalf("admitted action timeout = %s; want the installation default %s", timeout, 5*time.Minute)
	}
}

// TestExplicitActionTimeoutWinsOverInstallationDefault proves the declaration
// still takes precedence over the installation fallback.
func TestExplicitActionTimeoutWinsOverInstallationDefault(t *testing.T) {
	yaml := pushYAMLWithTimeout("10m")
	fixture, definition := pushFixtureWithYAML(t, yaml)
	admission := newRealAdmission(t, definition, resolvedDigest)
	processor := newRealAdmissionProcessor(t, fixture, admission, 5*time.Minute)

	result, err := processor.Process(context.Background())
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(result.Started) != 1 {
		t.Fatalf("started = %v; want one execution", result.Started)
	}
	if timeout := fixture.start.call(0).Admission.Actions[0].Timeout; timeout != 10*time.Minute {
		t.Fatalf("admitted action timeout = %s; want the explicit %s", timeout, 10*time.Minute)
	}
}

// TestOutOfRangeInstallationDefaultFailsClosed proves a positive but
// out-of-range installation default is not silently accepted: admission
// refuses it and the processor records a permanent refusal with no execution,
// so the event is settled by an explicit decision rather than a zero deadline.
func TestOutOfRangeInstallationDefaultFailsClosed(t *testing.T) {
	fixture, definition := pushFixtureWithYAML(t, pushYAMLWithTimeout(""))
	admission := newRealAdmission(t, definition, resolvedDigest)
	processor := newRealAdmissionProcessor(t, fixture, admission, 30*time.Second)

	result, err := processor.Process(context.Background())
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(result.Started) != 0 {
		t.Fatalf("started = %v; an out-of-range installation default must queue nothing", result.Started)
	}
	if !hasEvent(result.Refused, id.EventID("event-1")) {
		t.Fatalf("refused = %v; want the permanent admission refusal recorded", result.Refused)
	}
	unit := fixture.start.call(0)
	if unit.Status != trigger.StatusRefused {
		t.Fatalf("start status = %q; want a permanent refusal", unit.Status)
	}
	if fixture.start.executionCount() != 0 {
		t.Fatalf("executions = %d; a refused admission must not queue work", fixture.start.executionCount())
	}
}

// TestInvalidPipelineStillGetsStartRefused proves the pre-existing permanent
// admission refusal path is unchanged: an unpinnable image is refused, not
// queued and not deferred.
func TestInvalidPipelineStillGetsStartRefused(t *testing.T) {
	fixture, definition := pushFixtureWithYAML(t, pushYAMLWithTimeout(""))
	admission := newRealAdmission(t, definition, invalidResolvedDigest)
	processor := newRealAdmissionProcessor(t, fixture, admission, 5*time.Minute)

	result, err := processor.Process(context.Background())
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(result.Started) != 0 {
		t.Fatalf("started = %v; an invalid pipeline must queue nothing", result.Started)
	}
	if !hasEvent(result.Refused, id.EventID("event-1")) {
		t.Fatalf("refused = %v; want the permanent admission refusal recorded", result.Refused)
	}
	if hasEvent(result.Deferred, id.EventID("event-1")) {
		t.Fatalf("deferred = %v; an unsupported capability is permanent", result.Deferred)
	}
	if fixture.start.executionCount() != 0 {
		t.Fatalf("executions = %d; an invalid pipeline must not queue work", fixture.start.executionCount())
	}
}

// Compile-time proof the real admission service implements the processor port.
var _ trigger.AdmissionPort = (*pipeline.AdmissionService)(nil)
