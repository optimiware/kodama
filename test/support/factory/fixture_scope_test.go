package factory

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	runtimepodman "github.com/optimiware/kodama/internal/shell/runtime/podman"
	"github.com/optimiware/kodama/test/support/infrastructure/harness"
	runtimesupport "github.com/optimiware/kodama/test/support/infrastructure/runtime"
)

// captureCreateLibpod records the labels of the last container it was asked to
// create. It embeds the interface so only CreateContainer needs an
// implementation: the fixture-wrapper test never calls another seam method.
type captureCreateLibpod struct {
	runtimepodman.Libpod
	labels map[string]string
}

func (client *captureCreateLibpod) CreateContainer(_ context.Context, spec runtimepodman.ContainerSpec) (runtimepodman.ContainerState, error) {
	client.labels = cloneFixtureLabels(spec.Labels)
	return runtimepodman.ContainerState{}, nil
}

// captureCreateVolume records the labels of the last volume it was asked to
// create.
type captureCreateVolume struct {
	labels map[string]string
}

func (provisioner *captureCreateVolume) CreateVolume(_ context.Context, volume runtimepodman.Volume) (runtimepodman.Volume, error) {
	provisioner.labels = cloneFixtureLabels(volume.Labels)
	return volume, nil
}

func cloneFixtureLabels(labels map[string]string) map[string]string {
	copied := make(map[string]string, len(labels))
	maps.Copy(copied, labels)
	return copied
}

// fixtureRunnerStub records every Podman CLI invocation and answers through one
// handler so a test can present a mixed resource set (including resources that
// belong to another fixture, another process or no process at all).
type fixtureRunnerStub struct {
	calls  []harness.Command
	handle func(harness.Command) (harness.Result, error)
}

func (stub *fixtureRunnerStub) Run(_ context.Context, command harness.Command) (harness.Result, error) {
	stub.calls = append(stub.calls, command)
	return stub.handle(command)
}

func fixtureNamesResult(names ...string) harness.Result {
	return harness.Result{Stdout: []byte(strings.Join(names, "\n") + "\n")}
}

func fixtureLabelsResult(labels map[string]string) harness.Result {
	encoded, err := json.Marshal(labels)
	if err != nil {
		panic(err)
	}
	return harness.Result{Stdout: append(encoded, '\n')}
}

// isFixtureListCommand reports whether command is the fixture's resource
// enumeration for one kind.
func isFixtureListCommand(command harness.Command, kind fixtureResourceKind) bool {
	switch kind {
	case fixtureContainers:
		return len(command.Args) > 0 && command.Args[0] == "ps"
	case fixtureVolumes:
		return len(command.Args) > 1 && command.Args[0] == "volume" && command.Args[1] == "ls"
	default:
		return false
	}
}

// isFixtureInspectCommand reports whether command inspects one resource's
// labels for one kind.
func isFixtureInspectCommand(command harness.Command, kind fixtureResourceKind) bool {
	switch kind {
	case fixtureContainers:
		return len(command.Args) > 0 && command.Args[0] == "inspect"
	case fixtureVolumes:
		return len(command.Args) > 1 && command.Args[0] == "volume" && command.Args[1] == "inspect"
	default:
		return false
	}
}

func lastFixtureArgument(command harness.Command) string {
	if len(command.Args) == 0 {
		return ""
	}
	return command.Args[len(command.Args)-1]
}

// TestFixtureScopeIsUniqueAndLabelled proves one fixture identifier is minted
// independently per harness and that its label set keeps the process-wide run
// labels used by crash recovery.
func TestFixtureScopeIsUniqueAndLabelled(t *testing.T) {
	first := newFixtureScope()
	second := newFixtureScope()
	if first.ID() == "" || second.ID() == "" {
		t.Fatalf("fixture identifiers are empty: %q and %q", first.ID(), second.ID())
	}
	if first.ID() == second.ID() {
		t.Fatalf("two fixtures minted the same identifier %q", first.ID())
	}
	for _, scope := range []FixtureScope{first, second} {
		labels := scope.labels()
		if labels[FixtureLabel] != scope.ID() {
			t.Fatalf("fixture label = %q; want %q", labels[FixtureLabel], scope.ID())
		}
		if labels[harness.LabelTest] != "1" {
			t.Fatalf("fixture labels dropped the process test label: %v", labels)
		}
		if labels[harness.LabelOwnerPID] != runtimesupport.RunOwnerPID() {
			t.Fatalf("fixture labels dropped the process owner label: %v", labels)
		}
	}
}

// TestFixtureCleanupRequiresCompleteOwnershipLabels proves fixture selection
// requires the process labels AND the fixture label. A sibling fixture, a
// resource owned by another pid and an unlabelled resource survive selection.
// A list failure and an inspection failure remain errors instead of becoming an
// empty (and therefore "clean") result, and an empty fixture identifier is
// refused before Podman is touched.
func TestFixtureCleanupRequiresCompleteOwnershipLabels(t *testing.T) {
	scope := fixtureScopeWithID("fixture-a")
	pid := runtimesupport.RunOwnerPID()
	ownLabels := map[string]string{
		harness.LabelTest:     "1",
		harness.LabelOwnerPID: pid,
		FixtureLabel:          scope.ID(),
	}
	siblingLabels := map[string]string{
		harness.LabelTest:     "1",
		harness.LabelOwnerPID: pid,
		FixtureLabel:          "fixture-b",
	}
	foreignLabels := map[string]string{
		harness.LabelTest:     "1",
		harness.LabelOwnerPID: "2147480000",
		FixtureLabel:          scope.ID(),
	}
	unlabelled := map[string]string{"kodama.dev/workspace": "workspace-1"}
	missingFixture := map[string]string{
		harness.LabelTest:     "1",
		harness.LabelOwnerPID: pid,
	}
	// A sibling whose fixture id merely extends ours (same prefix, same pid and
	// process labels) and one whose id is a prefix of ours must both stay
	// unselected: ownership is exact equality, never a prefix or substring.
	extendedLabels := map[string]string{
		harness.LabelTest:     "1",
		harness.LabelOwnerPID: pid,
		FixtureLabel:          scope.ID() + "2",
	}
	prefixLabels := map[string]string{
		harness.LabelTest:     "1",
		harness.LabelOwnerPID: pid,
		FixtureLabel:          "fixture-",
	}
	// A leading character is the mirror case: "xfixture-a" ends with our
	// fixture id, so a suffix match would wrongly select it. Ownership is exact
	// equality, so it stays unselected.
	leadingLabels := map[string]string{
		harness.LabelTest:     "1",
		harness.LabelOwnerPID: pid,
		FixtureLabel:          "x" + scope.ID(),
	}
	// The process test label is part of ownership: a resource carrying the
	// exact fixture id and pid but not the process test label is not ours.
	missingTestLabel := map[string]string{
		harness.LabelOwnerPID: pid,
		FixtureLabel:          scope.ID(),
	}

	for _, kind := range []fixtureResourceKind{fixtureContainers, fixtureVolumes} {
		t.Run(string(kind), func(t *testing.T) {
			listed := []string{"own", "sibling", "foreign", "unlabelled", "missing-fixture", "extended", "prefix", "leading", "missing-test-label"}
			labels := map[string]map[string]string{
				"own":                ownLabels,
				"sibling":            siblingLabels,
				"foreign":            foreignLabels,
				"unlabelled":         unlabelled,
				"missing-fixture":    missingFixture,
				"extended":           extendedLabels,
				"prefix":             prefixLabels,
				"leading":            leadingLabels,
				"missing-test-label": missingTestLabel,
			}
			stub := &fixtureRunnerStub{handle: func(command harness.Command) (harness.Result, error) {
				switch {
				case isFixtureListCommand(command, kind):
					return fixtureNamesResult(listed...), nil
				case isFixtureInspectCommand(command, kind):
					name := lastFixtureArgument(command)
					resourceLabels, ok := labels[name]
					if !ok {
						return harness.Result{}, fmt.Errorf("unexpected inspect target %q", name)
					}
					return fixtureLabelsResult(resourceLabels), nil
				default:
					return harness.Result{}, fmt.Errorf("unexpected command %v", command.Args)
				}
			}}

			selected, err := listFixtureOwned(context.Background(), stub, kind, scope, nil)
			if err != nil {
				t.Fatalf("list fixture-owned %ss: %v", kind, err)
			}
			if !reflect.DeepEqual(selected, []string{"own"}) {
				t.Fatalf("selected %ss = %v; want only the fixture's own resource", kind, selected)
			}

			// The enumeration must carry all three ownership filters, not just
			// the process scope. A PID-only selector would return the sibling
			// fixture's resources too.
			var filters []string
			for index, argument := range stub.calls[0].Args {
				if argument == "--filter" && index+1 < len(stub.calls[0].Args) {
					filters = append(filters, stub.calls[0].Args[index+1])
				}
			}
			for _, want := range []string{
				"label=" + harness.LabelTest + "=1",
				"label=" + harness.LabelOwnerPID + "=" + pid,
				"label=" + FixtureLabel + "=" + scope.ID(),
			} {
				if !containsString(filters, want) {
					t.Fatalf("fixture %s enumeration filters = %v; want %q", kind, filters, want)
				}
			}
		})
	}

	t.Run("list failure remains an error", func(t *testing.T) {
		stub := &fixtureRunnerStub{handle: func(harness.Command) (harness.Result, error) {
			return harness.Result{}, fmt.Errorf("podman ps failed")
		}}
		if _, err := listFixtureOwned(context.Background(), stub, fixtureContainers, scope, nil); err == nil {
			t.Fatalf("a failed fixture listing returned no error; a probe failure must never look like an empty selection")
		}
	})

	t.Run("inspection failure remains an error", func(t *testing.T) {
		stub := &fixtureRunnerStub{handle: func(command harness.Command) (harness.Result, error) {
			if isFixtureListCommand(command, fixtureContainers) {
				return fixtureNamesResult("unreadable"), nil
			}
			return harness.Result{}, fmt.Errorf("podman inspect failed")
		}}
		selected, err := listFixtureOwned(context.Background(), stub, fixtureContainers, scope, nil)
		if err == nil {
			t.Fatalf("an unreadable resource was treated as absent; selected = %v", selected)
		}
	})

	t.Run("empty selector refuses before listing", func(t *testing.T) {
		stub := &fixtureRunnerStub{handle: func(harness.Command) (harness.Result, error) {
			return fixtureNamesResult("own"), nil
		}}
		if _, err := listFixtureOwned(context.Background(), stub, fixtureContainers, fixtureScopeWithID(""), nil); err == nil {
			t.Fatalf("an empty fixture selector did not refuse")
		}
		if len(stub.calls) != 0 {
			t.Fatalf("an empty fixture selector reached Podman: %v", stub.calls)
		}
	})

	// Ownership must independently refuse an invalid scope. Even with labels
	// that would exactly match the zero value (empty fixture id, process labels
	// present), an invalid scope must never claim a resource: dropping valid()
	// from owns() would let the zero-value scope select a resource whose
	// fixture label is empty.
	t.Run("invalid scope refuses ownership even when labels match", func(t *testing.T) {
		invalid := fixtureScopeWithID("")
		if invalid.valid() {
			t.Fatalf("an empty fixture identifier reported a valid scope")
		}
		matching := map[string]string{
			harness.LabelTest:     "1",
			harness.LabelOwnerPID: runtimesupport.RunOwnerPID(),
			FixtureLabel:          invalid.ID(),
		}
		if invalid.owns(matching) {
			t.Fatalf("an invalid fixture scope claimed ownership of matching labels %v", matching)
		}
		stub := &fixtureRunnerStub{handle: func(harness.Command) (harness.Result, error) {
			return fixtureNamesResult("own"), nil
		}}
		if _, err := listFixtureOwned(context.Background(), stub, fixtureContainers, invalid, nil); err == nil {
			t.Fatalf("an invalid fixture scope selected resources")
		}
		if len(stub.calls) != 0 {
			t.Fatalf("an invalid fixture scope reached Podman: %v", stub.calls)
		}
	})
}

// TestRemoveFixtureResourcesRemovesOnlyOwned proves destructive cleanup issues
// a remove command for exactly the selected fixture-owned resources. A sibling
// fixture's container and volume are never named in a remove command, including
// when its fixture id only extends ours or is a prefix of ours, and a resource
// carrying the exact fixture id and pid but no process test label is not ours
// either.
func TestRemoveFixtureResourcesRemovesOnlyOwned(t *testing.T) {
	scope := fixtureScopeWithID("fixture-a")
	pid := runtimesupport.RunOwnerPID()
	labels := map[string]map[string]string{
		"own-container": {
			harness.LabelTest:     "1",
			harness.LabelOwnerPID: pid,
			FixtureLabel:          scope.ID(),
		},
		"own-volume": {
			harness.LabelTest:     "1",
			harness.LabelOwnerPID: pid,
			FixtureLabel:          scope.ID(),
		},
		"sibling-container": {
			harness.LabelTest:     "1",
			harness.LabelOwnerPID: pid,
			FixtureLabel:          "fixture-b",
		},
		"sibling-volume": {
			harness.LabelTest:     "1",
			harness.LabelOwnerPID: pid,
			FixtureLabel:          "fixture-b",
		},
		"extended-container": {
			harness.LabelTest:     "1",
			harness.LabelOwnerPID: pid,
			FixtureLabel:          scope.ID() + "2",
		},
		"extended-volume": {
			harness.LabelTest:     "1",
			harness.LabelOwnerPID: pid,
			FixtureLabel:          scope.ID() + "2",
		},
		"prefix-container": {
			harness.LabelTest:     "1",
			harness.LabelOwnerPID: pid,
			FixtureLabel:          "fixture-",
		},
		"prefix-volume": {
			harness.LabelTest:     "1",
			harness.LabelOwnerPID: pid,
			FixtureLabel:          "fixture-",
		},
		"missing-test-container": {
			harness.LabelOwnerPID: pid,
			FixtureLabel:          scope.ID(),
		},
		"missing-test-volume": {
			harness.LabelOwnerPID: pid,
			FixtureLabel:          scope.ID(),
		},
	}
	listing := map[fixtureResourceKind][]string{
		fixtureContainers: {"own-container", "sibling-container", "extended-container", "prefix-container", "missing-test-container"},
		fixtureVolumes:    {"own-volume", "sibling-volume", "extended-volume", "prefix-volume", "missing-test-volume"},
	}
	var removed []string
	stub := &fixtureRunnerStub{handle: func(command harness.Command) (harness.Result, error) {
		for _, kind := range []fixtureResourceKind{fixtureContainers, fixtureVolumes} {
			switch {
			case isFixtureListCommand(command, kind):
				return fixtureNamesResult(listing[kind]...), nil
			case isFixtureInspectCommand(command, kind):
				name := lastFixtureArgument(command)
				resourceLabels, ok := labels[name]
				if !ok {
					return harness.Result{}, fmt.Errorf("unexpected inspect target %q", name)
				}
				return fixtureLabelsResult(resourceLabels), nil
			}
		}
		switch {
		case len(command.Args) > 0 && command.Args[0] == "rm":
			removed = append(removed, lastFixtureArgument(command))
			return harness.Result{}, nil
		case len(command.Args) > 1 && command.Args[0] == "volume" && command.Args[1] == "rm":
			removed = append(removed, lastFixtureArgument(command))
			return harness.Result{}, nil
		default:
			return harness.Result{}, fmt.Errorf("unexpected command %v", command.Args)
		}
	}}

	if err := removeFixtureResources(context.Background(), stub, scope); err != nil {
		t.Fatalf("remove fixture resources: %v", err)
	}
	want := []string{"own-container", "own-volume"}
	if !reflect.DeepEqual(removed, want) {
		t.Fatalf("removed fixture resources = %v; want exactly %v", removed, want)
	}
}

// TestContainersEnumeratesOnlyFixtureOwnedWorkloads proves the public
// Harness.Containers enumeration keeps the fixture selector. The fake podman
// answers every `ps` request with a sibling container that carries the same
// caller-supplied label pair (kodama.test=1), so reverting Containers to the
// process-wide runtime support enumeration would return the sibling too. Only
// the fixture's own container may be returned.
func TestContainersEnumeratesOnlyFixtureOwnedWorkloads(t *testing.T) {
	scope := fixtureScopeWithID("fixture-containers")
	pid := runtimesupport.RunOwnerPID()
	labelsDir := t.TempDir()
	writeLabels := func(name string, labels map[string]string) {
		t.Helper()
		encoded, err := json.Marshal(labels)
		if err != nil {
			t.Fatalf("encode %s labels: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(labelsDir, name), append(encoded, '\n'), 0o600); err != nil {
			t.Fatalf("write %s labels: %v", name, err)
		}
	}
	writeLabels("own", map[string]string{
		harness.LabelTest:     "1",
		harness.LabelOwnerPID: pid,
		FixtureLabel:          scope.ID(),
	})
	writeLabels("sibling", map[string]string{
		harness.LabelTest:     "1",
		harness.LabelOwnerPID: pid,
		FixtureLabel:          "fixture-sibling",
	})
	// The fake answers every listing with both names, regardless of the
	// filters. The fixture label inspection is therefore the only thing that
	// can keep the sibling out of the result.
	installFakePodman(t, fmt.Sprintf(`
case "$1" in
ps)
  printf 'own\nsibling\n'
  ;;
inspect)
  for last in "$@"; do :; done
  cat '%s'/"$last"
  ;;
esac
`, labelsDir))

	h := &Harness{T: t, fixture: scope}
	containers, err := h.Containers(context.Background(), harness.LabelTest, "1")
	if err != nil {
		t.Fatalf("Containers: %v", err)
	}
	if !reflect.DeepEqual(containers, []string{"own"}) {
		t.Fatalf("Containers = %v; want only the fixture's own container [own] (the sibling also carries %s=1)",
			containers, harness.LabelTest)
	}
}

// TestFixtureLabelsReachContainersAndVolumes proves the fixture identifier
// travels with both resource families the composition creates. Labelling only
// one family would leave the other outside fixture cleanup and let closing one
// fixture delete (or keep) a sibling's resources.
func TestFixtureLabelsReachContainersAndVolumes(t *testing.T) {
	scope := fixtureScopeWithID("fixture-labels")
	libpod := &captureCreateLibpod{}
	provisioner := &captureCreateVolume{}
	client := labelledLibpod{Libpod: libpod, provisioner: provisioner, fixture: scope}

	if _, err := client.CreateContainer(context.Background(), runtimepodman.ContainerSpec{
		Labels: map[string]string{"kodama.dev/execution": "execution-1"},
	}); err != nil {
		t.Fatalf("create container: %v", err)
	}
	assertFixtureLabelSet(t, libpod.labels, scope, "kodama.dev/execution", "execution-1")

	if _, err := client.CreateVolume(context.Background(), runtimepodman.Volume{
		Labels: map[string]string{"kodama.dev/workspace": "workspace-1"},
	}); err != nil {
		t.Fatalf("create volume: %v", err)
	}
	assertFixtureLabelSet(t, provisioner.labels, scope, "kodama.dev/workspace", "workspace-1")
}

func assertFixtureLabelSet(t *testing.T, labels map[string]string, scope FixtureScope, productKey, productValue string) {
	t.Helper()
	if labels[productKey] != productValue {
		t.Fatalf("product label %s = %q; want %q (labels=%v)", productKey, labels[productKey], productValue, labels)
	}
	if labels[FixtureLabel] != scope.ID() {
		t.Fatalf("fixture label = %q; want %q (labels=%v)", labels[FixtureLabel], scope.ID(), labels)
	}
	if labels[harness.LabelTest] != "1" || labels[harness.LabelOwnerPID] != runtimesupport.RunOwnerPID() {
		t.Fatalf("process cleanup labels missing after fixture merge: %v", labels)
	}
}

func containsString(values []string, want string) bool {
	return slices.Contains(values, want)
}
