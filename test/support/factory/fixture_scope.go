package factory

// Per-fixture resource scope.
//
// One test process can run several factory fixtures (for example two real
// compositions in one test). The process-wide run labels (`kodama.test` and
// `kodama.owner-pid`) are still what crash recovery sweeps, but they select
// every fixture in the process. A fixture identifier is minted before any
// resource exists and added next to those labels on every container, workspace
// volume and positive-control probe, so closing one fixture removes only its
// own resources while the sibling fixture keeps running.
//
// Selection is fail-closed: an empty fixture identifier is refused before any
// Podman invocation, and a resource is removed only when the process labels AND
// the fixture label are present. Resource enumeration inspects the labels rather
// than trusting a possibly-misapplied CLI filter, and a listing or inspection
// failure is an error, never an empty (therefore "clean") selection.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/optimiware/kodama/test/support/infrastructure/harness"
	runtimesupport "github.com/optimiware/kodama/test/support/infrastructure/runtime"
)

// FixtureLabel scopes one fixture's resources inside a test process. It is a
// test-support label; the process labels above it remain the crash-recovery
// scope.
const FixtureLabel = "kodama.fixture"

// fixtureIDBytes is the entropy of one fixture identifier. Eight random bytes
// make a collision between two fixtures in one process negligible.
const fixtureIDBytes = 8

// FixtureScope is one factory fixture's resource identity. The zero value is
// invalid on purpose: it must never silently degrade to a process-wide sweep.
type FixtureScope struct {
	id string
}

// newFixtureScope mints an independent fixture identifier. Test support cannot
// select its own resources without randomness; substituting a fixed value would
// reintroduce same-process collisions.
func newFixtureScope() FixtureScope {
	raw := make([]byte, fixtureIDBytes)
	if _, err := rand.Read(raw); err != nil {
		panic(fmt.Sprintf("factory: mint fixture identifier: %v", err))
	}
	return FixtureScope{id: hex.EncodeToString(raw)}
}

// fixtureScopeWithID reconstructs a scope from a known identifier. It exists so
// tests can prove selection deterministically; production fixtures always mint
// through newFixtureScope.
func fixtureScopeWithID(id string) FixtureScope { return FixtureScope{id: id} }

// ID returns the fixture identifier.
func (scope FixtureScope) ID() string { return scope.id }

// valid reports whether the scope can safely select resources.
func (scope FixtureScope) valid() bool { return strings.TrimSpace(scope.id) != "" }

// labels returns the complete cleanup label set for this fixture: the
// process-wide run labels used by crash recovery plus the fixture selector.
func (scope FixtureScope) labels() map[string]string {
	labels := runtimesupport.RunLabels()
	labels[FixtureLabel] = scope.id
	return labels
}

// owns reports whether labels identify a resource this exact fixture created.
// Any missing label refuses ownership; a sibling fixture or a foreign process
// therefore never matches.
func (scope FixtureScope) owns(labels map[string]string) bool {
	return scope.valid() &&
		labels[harness.LabelTest] == "1" &&
		labels[harness.LabelOwnerPID] == runtimesupport.RunOwnerPID() &&
		labels[FixtureLabel] == scope.id
}

// fixtureResourceKind is the Podman resource family a fixture lists or removes.
type fixtureResourceKind string

const (
	fixtureContainers fixtureResourceKind = "container"
	fixtureVolumes    fixtureResourceKind = "volume"
)

// fixtureCommandRunner executes one Podman CLI command. The production runner
// is harness.ExecRunner; unit tests substitute a deterministic stub.
type fixtureCommandRunner interface {
	Run(context.Context, harness.Command) (harness.Result, error)
}

// newFixtureCommandRunner returns the production Podman CLI runner.
func newFixtureCommandRunner() fixtureCommandRunner { return harness.ExecRunner{} }

// fixtureFilters returns the complete ownership selector. An invalid scope is
// refused before Podman sees a filter, so it cannot fall back to the
// process-wide selector.
func fixtureFilters(scope FixtureScope) ([]string, error) {
	if !scope.valid() {
		return nil, errors.New("factory: fixture resource selection needs a fixture identifier")
	}
	return []string{
		"label=" + harness.LabelTest + "=1",
		"label=" + harness.LabelOwnerPID + "=" + runtimesupport.RunOwnerPID(),
		"label=" + FixtureLabel + "=" + scope.id,
	}, nil
}

// fixtureFilterArgs renders filters as Podman CLI arguments.
func fixtureFilterArgs(filters []string) []string {
	args := make([]string, 0, len(filters)*2)
	for _, filter := range filters {
		args = append(args, "--filter", filter)
	}
	return args
}

// listFixtureNames lists the candidate names for one resource kind. It is the
// unfiltered-by-ownership enumeration step; callers must still inspect labels.
func listFixtureNames(ctx context.Context, runner fixtureCommandRunner, kind fixtureResourceKind, filters []string) ([]string, error) {
	var args []string
	switch kind {
	case fixtureContainers:
		args = append([]string{"ps", "-a"}, fixtureFilterArgs(filters)...)
		args = append(args, "--format", "{{.Names}}")
	case fixtureVolumes:
		args = append([]string{"volume", "ls"}, fixtureFilterArgs(filters)...)
		args = append(args, "--format", "{{.Name}}")
	default:
		return nil, fmt.Errorf("factory: unknown fixture resource kind %q", kind)
	}
	result, err := runner.Run(ctx, harness.Command{Name: "podman", Args: args, Env: harness.BaseEnv()})
	if err != nil {
		return nil, fmt.Errorf("factory: list fixture %ss: %w", kind, err)
	}
	var names []string
	for line := range strings.SplitSeq(string(result.Stdout), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

// inspectFixtureLabels reads one resource's labels. A failed inspection is
// returned as an error: treating it as absence would let cleanup report success
// without checking ownership.
func inspectFixtureLabels(ctx context.Context, runner fixtureCommandRunner, kind fixtureResourceKind, name string) (map[string]string, error) {
	var args []string
	switch kind {
	case fixtureContainers:
		args = []string{"inspect", "--format", "{{json .Config.Labels}}", name}
	case fixtureVolumes:
		args = []string{"volume", "inspect", "--format", "{{json .Labels}}", name}
	default:
		return nil, fmt.Errorf("factory: unknown fixture resource kind %q", kind)
	}
	result, err := runner.Run(ctx, harness.Command{Name: "podman", Args: args, Env: harness.BaseEnv()})
	if err != nil {
		return nil, err
	}
	labels := map[string]string{}
	text := strings.TrimSpace(string(result.Stdout))
	if text == "" || text == "null" {
		return labels, nil
	}
	if err := json.Unmarshal([]byte(text), &labels); err != nil {
		return nil, fmt.Errorf("factory: decode %s %s labels: %w", kind, name, err)
	}
	return labels, nil
}

// listFixtureOwned returns exactly the resources this fixture owns, optionally
// narrowed by extra label filters. A listing or inspection failure is an error.
func listFixtureOwned(ctx context.Context, runner fixtureCommandRunner, kind fixtureResourceKind, scope FixtureScope, extraFilters []string) ([]string, error) {
	filters, err := fixtureFilters(scope)
	if err != nil {
		return nil, err
	}
	filters = append(filters, extraFilters...)
	candidates, err := listFixtureNames(ctx, runner, kind, filters)
	if err != nil {
		return nil, err
	}
	owned := make([]string, 0, len(candidates))
	for _, name := range candidates {
		labels, err := inspectFixtureLabels(ctx, runner, kind, name)
		if err != nil {
			return nil, fmt.Errorf("factory: inspect %s %s ownership: %w", kind, name, err)
		}
		if scope.owns(labels) {
			owned = append(owned, name)
		}
	}
	return owned, nil
}

// removeFixtureResources removes only resources carrying the complete
// ownership labels. Containers are removed before volumes, matching the
// process-wide cleanup ordering.
func removeFixtureResources(ctx context.Context, runner fixtureCommandRunner, scope FixtureScope) error {
	var errs []error
	for _, kind := range []fixtureResourceKind{fixtureContainers, fixtureVolumes} {
		names, err := listFixtureOwned(ctx, runner, kind, scope, nil)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, name := range names {
			var args []string
			switch kind {
			case fixtureContainers:
				args = []string{"rm", "--force", "--volumes", name}
			case fixtureVolumes:
				args = []string{"volume", "rm", name}
			}
			if _, err := runner.Run(ctx, harness.Command{Name: "podman", Args: args, Env: harness.BaseEnv()}); err != nil && !missingFixtureResource(err) {
				errs = append(errs, fmt.Errorf("factory: remove %s %s: %w", kind, name, err))
			}
		}
	}
	return errors.Join(errs...)
}

// missingFixtureResource reports whether a removal failed because another
// caller already removed the resource. It is tolerated only at removal time;
// listing and inspection failures stay errors.
func missingFixtureResource(err error) bool {
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "no such container") ||
		strings.Contains(text, "no such volume") ||
		strings.Contains(text, "no container with") ||
		strings.Contains(text, "does not exist")
}

// FixtureID returns this harness's fixture identifier. It is evidence for the
// same-process isolation test and never a credential.
func (h *Harness) FixtureID() string { return h.fixture.ID() }

// FixtureContainers lists the containers this fixture owns. It is the
// fixture-scoped positive control for resource enumeration.
func (h *Harness) FixtureContainers(ctx context.Context) ([]string, error) {
	h.T.Helper()
	return listFixtureOwned(ctx, newFixtureCommandRunner(), fixtureContainers, h.fixture, nil)
}

// FixtureVolumes lists the volumes this fixture owns.
func (h *Harness) FixtureVolumes(ctx context.Context) ([]string, error) {
	h.T.Helper()
	return listFixtureOwned(ctx, newFixtureCommandRunner(), fixtureVolumes, h.fixture, nil)
}

// removeOwnedResources removes this fixture's containers and volumes.
func (h *Harness) removeOwnedResources(ctx context.Context) error {
	return removeFixtureResources(ctx, newFixtureCommandRunner(), h.fixture)
}
