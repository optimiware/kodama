package app

// Durable activation-fact tests for the real WorkspaceActivation.
//
// The decorator must never infer a fresh workspace from volume presence or
// process memory. The real implementation reads the persisted executions row
// that owns the workspace and the ordered action rows, so a controller restart
// cannot turn a lost workspace of an already-activated execution into a fresh
// one.

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/installation"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/shell/store"
)

// activationTestTenant is the durable owning tenant every execution row carries
// in these fixtures.
const activationTestTenant = id.TenantID("tenant-1")

// activationFactsStore is a deterministic store.Controller fake over the two
// durable tables the activation fact reads. It validates the query against the
// schema so the test also proves the production queries are legal.
type activationFactsStore struct {
	mu      sync.Mutex
	rows    []store.Row
	scanErr error
	queries []store.ControllerScan
	// forcedHasMore simulates a scanner that reports a further page for a
	// table even though the requested limit already returned every matching
	// row. It isolates the ambiguity guard's HasMore half from its row-count
	// half: the fake can return a single row while still claiming more exist.
	forcedHasMore map[string]bool
}

func (facts *activationFactsStore) Read(_ context.Context, table, rowID string) (store.Row, error) {
	facts.mu.Lock()
	defer facts.mu.Unlock()
	for _, row := range facts.rows {
		if row.Table == table && row.Values["id"] == rowID {
			return row, nil
		}
	}
	return store.Row{}, fault.New(fault.NotFound, "activation facts: row is missing")
}

func (facts *activationFactsStore) Execute(context.Context, string, store.Unit) (store.UnitResult, error) {
	return store.UnitResult{}, errors.New("activation facts: execute is not expected")
}

func (facts *activationFactsStore) Scan(ctx context.Context, scan store.ControllerScan) (store.QueryPage, error) {
	if err := ctx.Err(); err != nil {
		return store.QueryPage{}, err
	}
	if err := scan.Validate(); err != nil {
		return store.QueryPage{}, err
	}
	facts.mu.Lock()
	defer facts.mu.Unlock()
	facts.queries = append(facts.queries, scan)
	if facts.scanErr != nil {
		return store.QueryPage{}, facts.scanErr
	}
	filtered := make([]store.Row, 0, len(facts.rows))
	for _, row := range facts.rows {
		if row.Table != scan.Table {
			continue
		}
		matches := true
		for column, value := range scan.Equals {
			if row.Values[column] != value {
				matches = false
				break
			}
		}
		if matches {
			filtered = append(filtered, row)
		}
	}
	sort.SliceStable(filtered, func(left, right int) bool {
		for _, order := range scan.OrderBy {
			leftValue := filtered[left].Values[order.Column]
			rightValue := filtered[right].Values[order.Column]
			comparison := compareActivationFact(leftValue, rightValue)
			if comparison == 0 {
				continue
			}
			if order.Desc {
				return comparison > 0
			}
			return comparison < 0
		}
		return false
	})
	page := store.QueryPage{Rows: filtered}
	if len(filtered) > scan.Limit {
		page.Rows = filtered[:scan.Limit]
		page.HasMore = true
		page.Next = "activation-facts-next-page"
	}
	if facts.forcedHasMore[scan.Table] {
		page.HasMore = true
		if page.Next == "" {
			page.Next = "activation-facts-forced-next-page"
		}
	}
	return page, nil
}

func compareActivationFact(left, right any) int {
	switch leftValue := left.(type) {
	case int:
		rightValue, ok := right.(int)
		if !ok {
			return 0
		}
		switch {
		case leftValue < rightValue:
			return -1
		case leftValue > rightValue:
			return 1
		default:
			return 0
		}
	case string:
		rightValue, ok := right.(string)
		if !ok {
			return 0
		}
		switch {
		case leftValue < rightValue:
			return -1
		case leftValue > rightValue:
			return 1
		default:
			return 0
		}
	default:
		return 0
	}
}

func (facts *activationFactsStore) recordedQueries() []store.ControllerScan {
	facts.mu.Lock()
	defer facts.mu.Unlock()
	return append([]store.ControllerScan(nil), facts.queries...)
}

func activationExecutionRow(executionID id.ExecutionID, workspaceID id.WorkspaceID) store.Row {
	return store.Row{Table: "executions", Values: map[string]any{
		"id":           string(executionID),
		"workspace_id": string(workspaceID),
		"tenant_id":    string(activationTestTenant),
	}}
}

func activationActionRow(actionID id.ActionID, executionID id.ExecutionID, index int, state kernel.ActionState) store.Row {
	return store.Row{Table: "actions", Values: map[string]any{
		"id":           string(actionID),
		"execution_id": string(executionID),
		"index":        index,
		"state":        string(state),
	}}
}

func newTestWorkspaceActivation(t *testing.T, facts *activationFactsStore) *workspaceActivation {
	t.Helper()
	activation, err := newWorkspaceActivation(facts)
	if err != nil {
		t.Fatalf("newWorkspaceActivation: %v", err)
	}
	return activation
}

// TestWorkspaceActivationFirstActionIsFresh proves an execution whose actions
// are all still pending reports no earlier activation, so the decorator
// provisions for the first activation the Kernel is about to commit.
func TestWorkspaceActivationFirstActionIsFresh(t *testing.T) {
	workspaceID := id.WorkspaceID("ws-first")
	executionID := id.ExecutionID("execution-first")
	facts := &activationFactsStore{rows: []store.Row{
		activationExecutionRow(executionID, workspaceID),
		activationActionRow("act-first-0", executionID, 0, kernel.ActionPending),
	}}
	activation := newTestWorkspaceActivation(t, facts)

	previously, err := activation.PreviouslyActivated(context.Background(), workspaceID)
	if err != nil {
		t.Fatalf("PreviouslyActivated: %v", err)
	}
	if previously {
		t.Fatal("the first activation was reported as previously activated")
	}
	queries := facts.recordedQueries()
	if len(queries) != 2 {
		t.Fatalf("store scans = %d; want the executions and actions reads", len(queries))
	}
	if queries[0].Table != "executions" || queries[0].Equals["workspace_id"] != string(workspaceID) {
		t.Fatalf("first scan = %+v; want the workspace attribution read", queries[0])
	}
	if queries[1].Table != "actions" || queries[1].Equals["execution_id"] != string(executionID) {
		t.Fatalf("second scan = %+v; want the execution action read", queries[1])
	}
}

// TestWorkspaceActivationLaterActionIsNotFresh proves a later activation of
// the same execution reports an earlier activation, so a lost workspace is
// never recreated.
func TestWorkspaceActivationLaterActionIsNotFresh(t *testing.T) {
	workspaceID := id.WorkspaceID("ws-later")
	executionID := id.ExecutionID("execution-later")
	facts := &activationFactsStore{rows: []store.Row{
		activationExecutionRow(executionID, workspaceID),
		activationActionRow("act-later-0", executionID, 0, kernel.ActionSucceeded),
		activationActionRow("act-later-1", executionID, 1, kernel.ActionPending),
	}}
	activation := newTestWorkspaceActivation(t, facts)

	previously, err := activation.PreviouslyActivated(context.Background(), workspaceID)
	if err != nil {
		t.Fatalf("PreviouslyActivated: %v", err)
	}
	if !previously {
		t.Fatal("a later action of an already-activated execution was reported as fresh")
	}
}

// TestWorkspaceActivationActivatedFirstActionIsNotFresh proves the first action
// counts as an earlier activation as soon as it left pending. A workload may
// already use the workspace, so a Retain that reaches this state never
// provisions and a lost workspace is never recreated.
func TestWorkspaceActivationActivatedFirstActionIsNotFresh(t *testing.T) {
	workspaceID := id.WorkspaceID("ws-activated")
	executionID := id.ExecutionID("execution-activated")
	for _, state := range []kernel.ActionState{
		kernel.ActionRunning, kernel.ActionSucceeded, kernel.ActionFailed, kernel.ActionCancelled,
	} {
		t.Run(string(state), func(t *testing.T) {
			facts := &activationFactsStore{rows: []store.Row{
				activationExecutionRow(executionID, workspaceID),
				activationActionRow("act-activated-0", executionID, 0, state),
				activationActionRow("act-activated-1", executionID, 1, kernel.ActionPending),
			}}
			previously, err := newTestWorkspaceActivation(t, facts).PreviouslyActivated(context.Background(), workspaceID)
			if err != nil {
				t.Fatalf("PreviouslyActivated: %v", err)
			}
			if !previously {
				t.Fatalf("a %s first action was reported as a fresh workspace", state)
			}
		})
	}
}

// TestWorkspaceActivationSurvivesRestart proves the fact comes from persisted
// state: a newly constructed fact source over the same store returns the same
// answer with no process memory involved.
func TestWorkspaceActivationSurvivesRestart(t *testing.T) {
	workspaceID := id.WorkspaceID("ws-restart")
	executionID := id.ExecutionID("execution-restart")
	facts := &activationFactsStore{rows: []store.Row{
		activationExecutionRow(executionID, workspaceID),
		activationActionRow("act-restart-0", executionID, 0, kernel.ActionSucceeded),
		activationActionRow("act-restart-1", executionID, 1, kernel.ActionPending),
	}}

	before, err := newTestWorkspaceActivation(t, facts).PreviouslyActivated(context.Background(), workspaceID)
	if err != nil {
		t.Fatalf("PreviouslyActivated before restart: %v", err)
	}
	after, err := newTestWorkspaceActivation(t, facts).PreviouslyActivated(context.Background(), workspaceID)
	if err != nil {
		t.Fatalf("PreviouslyActivated after restart: %v", err)
	}
	if !before || !after {
		t.Fatalf("activation facts = %v/%v; want the persisted later activation on both reads", before, after)
	}
}

// TestWorkspaceActivationIsScopedToTheWorkspace proves one workspace's
// activation history does not leak into another workspace's first activation.
func TestWorkspaceActivationIsScopedToTheWorkspace(t *testing.T) {
	firstWorkspace := id.WorkspaceID("ws-scope-first")
	firstExecution := id.ExecutionID("execution-scope-first")
	laterWorkspace := id.WorkspaceID("ws-scope-later")
	laterExecution := id.ExecutionID("execution-scope-later")
	facts := &activationFactsStore{rows: []store.Row{
		activationExecutionRow(firstExecution, firstWorkspace),
		activationActionRow("act-scope-first-0", firstExecution, 0, kernel.ActionPending),
		activationExecutionRow(laterExecution, laterWorkspace),
		activationActionRow("act-scope-later-0", laterExecution, 0, kernel.ActionSucceeded),
		activationActionRow("act-scope-later-1", laterExecution, 1, kernel.ActionPending),
	}}
	activation := newTestWorkspaceActivation(t, facts)

	previously, err := activation.PreviouslyActivated(context.Background(), firstWorkspace)
	if err != nil {
		t.Fatalf("PreviouslyActivated first workspace: %v", err)
	}
	if previously {
		t.Fatal("the first workspace inherited another workspace's activation history")
	}
	previously, err = activation.PreviouslyActivated(context.Background(), laterWorkspace)
	if err != nil {
		t.Fatalf("PreviouslyActivated later workspace: %v", err)
	}
	if !previously {
		t.Fatal("the later workspace activation history was not read")
	}
}

// TestWorkspaceActivationFailsClosed proves every missing, ambiguous or
// unreadable fact is an error, never a fresh-workspace inference.
func TestWorkspaceActivationFailsClosed(t *testing.T) {
	workspaceID := id.WorkspaceID("ws-unknown")
	executionID := id.ExecutionID("execution-unknown")
	cases := []struct {
		name  string
		facts *activationFactsStore
	}{
		{
			name:  "missing execution row",
			facts: &activationFactsStore{},
		},
		{
			name: "execution scan error",
			facts: &activationFactsStore{
				scanErr: errors.New("store unavailable"),
			},
		},
		{
			name: "ambiguous workspace attribution",
			facts: &activationFactsStore{rows: []store.Row{
				activationExecutionRow(executionID, workspaceID),
				activationExecutionRow("execution-unknown-2", workspaceID),
				// The lowest-id execution is the one a missing row-count
				// guard would resolve to. Its single pending action would
				// then look like a fresh first activation, so the refusal
				// is caused by the ambiguity guard itself and not by the
				// later no-action check.
				activationActionRow("act-unknown-ambiguous", executionID, 0, kernel.ActionPending),
			}},
		},
		{
			name: "ambiguous workspace attribution beyond the first page",
			facts: &activationFactsStore{rows: []store.Row{
				activationExecutionRow(executionID, workspaceID),
				activationExecutionRow("execution-unknown-2", workspaceID),
				activationExecutionRow("execution-unknown-3", workspaceID),
				activationActionRow("act-unknown-beyond", executionID, 0, kernel.ActionPending),
			}},
		},
		{
			name: "workspace attribution reports a further page",
			facts: &activationFactsStore{
				rows: []store.Row{
					activationExecutionRow(executionID, workspaceID),
					// A single pending action again makes the page's
					// HasMore flag the only reason to refuse: without that
					// half of the guard this would look like a fresh first
					// activation.
					activationActionRow("act-unknown-hasmore", executionID, 0, kernel.ActionPending),
				},
				forcedHasMore: map[string]bool{"executions": true},
			},
		},
		{
			name: "execution with no actions",
			facts: &activationFactsStore{rows: []store.Row{
				activationExecutionRow(executionID, workspaceID),
			}},
		},
		{
			name: "unknown action state",
			facts: &activationFactsStore{rows: []store.Row{
				activationExecutionRow(executionID, workspaceID),
				activationActionRow("act-unknown-1", executionID, 0, kernel.ActionState("mystery")),
			}},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			activation := newTestWorkspaceActivation(t, testCase.facts)
			previously, err := activation.PreviouslyActivated(context.Background(), workspaceID)
			if err == nil {
				t.Fatalf("PreviouslyActivated = %v/nil; want a fail-closed error", previously)
			}
			if previously {
				t.Fatal("a failed activation fact reported a previously activated workspace")
			}
		})
	}
}

// TestProvisioningWorkspaceUsesDurableActivationByDefault proves the
// composition root wires the real store-backed activation fact when the caller
// does not inject one: a fresh workspace is provisioned while a later action of
// another workspace is never recreated.
func TestProvisioningWorkspaceUsesDurableActivationByDefault(t *testing.T) {
	config := fullTestProfileConfig(t)
	config.Installation.Environment = installation.EnvironmentTest
	config.Installation.Runtime = installation.RuntimePodman
	inner := &recordingWorkspace{}
	provisioner := &recordingProvisioner{}
	freshWorkspace := id.WorkspaceID("ws-wiring-fresh")
	freshExecution := id.ExecutionID("execution-wiring-fresh")
	laterWorkspace := id.WorkspaceID("ws-wiring-later")
	laterExecution := id.ExecutionID("execution-wiring-later")
	facts := &activationFactsStore{rows: []store.Row{
		activationExecutionRow(freshExecution, freshWorkspace),
		activationActionRow("act-wiring-fresh-0", freshExecution, 0, kernel.ActionPending),
		activationExecutionRow(laterExecution, laterWorkspace),
		activationActionRow("act-wiring-later-0", laterExecution, 0, kernel.ActionSucceeded),
		activationActionRow("act-wiring-later-1", laterExecution, 1, kernel.ActionPending),
	}}
	config.Adapters.Workspace = inner
	config.Adapters.VolumeProvisioner = provisioner
	config.Adapters.Controller = facts
	config.Adapters.WorkspaceActivation = nil

	environment, err := buildEnvironment(context.Background(), &builder{cfg: config, logger: config.logger()})
	if err != nil {
		t.Fatalf("buildEnvironment: %v", err)
	}
	if err := environment.workspace.Retain(context.Background(), freshWorkspace); err != nil {
		t.Fatalf("fresh Retain: %v", err)
	}
	if got := provisioner.count(); got != 1 {
		t.Fatalf("CreateVolume calls after the fresh activation = %d; want 1", got)
	}
	if err := environment.workspace.Retain(context.Background(), laterWorkspace); err != nil {
		t.Fatalf("later Retain: %v", err)
	}
	if got := provisioner.count(); got != 1 {
		t.Fatalf("CreateVolume calls after the later activation = %d; want the durable fact to suppress provisioning", got)
	}
	if got := inner.retainCount(); got != 2 {
		t.Fatalf("inner Retain calls = %d; want both Retain calls delegated", got)
	}
}

// TestWorkspaceActivationPropagatesCancellation proves a cancelled read performs
// no store scan.
func TestWorkspaceActivationPropagatesCancellation(t *testing.T) {
	workspaceID := id.WorkspaceID("ws-cancelled")
	facts := &activationFactsStore{rows: []store.Row{
		activationExecutionRow("execution-cancelled", workspaceID),
		activationActionRow("act-cancelled-0", "execution-cancelled", 0, kernel.ActionRunning),
	}}
	activation := newTestWorkspaceActivation(t, facts)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := activation.PreviouslyActivated(ctx, workspaceID); !errors.Is(err, context.Canceled) {
		t.Fatalf("PreviouslyActivated error = %v; want context.Canceled", err)
	}
	if got := len(facts.recordedQueries()); got != 0 {
		t.Fatalf("store scans = %d; a cancelled read must not scan", got)
	}
}

// TestWorkspaceActivationReportsDurableTenant proves the Kubernetes claim
// decorator's tenant fact comes from the persisted execution row, not from the
// request, process memory or the configured template.
func TestWorkspaceActivationReportsDurableTenant(t *testing.T) {
	workspaceID := id.WorkspaceID("ws-tenant")
	executionID := id.ExecutionID("execution-tenant")
	facts := &activationFactsStore{rows: []store.Row{
		activationExecutionRow(executionID, workspaceID),
	}}
	activation := newTestWorkspaceActivation(t, facts)

	tenant, err := activation.OwningTenant(context.Background(), workspaceID)
	if err != nil {
		t.Fatalf("OwningTenant: %v", err)
	}
	if tenant != activationTestTenant {
		t.Fatalf("OwningTenant = %q; want the durable tenant %q", tenant, activationTestTenant)
	}
}

// TestWorkspaceActivationTenantIsScopedToTheWorkspace proves one workspace's
// execution tenant never leaks into another workspace's answer.
func TestWorkspaceActivationTenantIsScopedToTheWorkspace(t *testing.T) {
	firstWorkspace := id.WorkspaceID("ws-tenant-first")
	firstExecution := id.ExecutionID("execution-tenant-first")
	secondWorkspace := id.WorkspaceID("ws-tenant-second")
	secondExecution := id.ExecutionID("execution-tenant-second")
	facts := &activationFactsStore{rows: []store.Row{
		{Table: "executions", Values: map[string]any{
			"id":           string(firstExecution),
			"workspace_id": string(firstWorkspace),
			"tenant_id":    "tenant-a",
		}},
		{Table: "executions", Values: map[string]any{
			"id":           string(secondExecution),
			"workspace_id": string(secondWorkspace),
			"tenant_id":    "tenant-b",
		}},
	}}
	activation := newTestWorkspaceActivation(t, facts)

	for _, testCase := range []struct {
		workspace id.WorkspaceID
		want      id.TenantID
	}{
		{firstWorkspace, "tenant-a"},
		{secondWorkspace, "tenant-b"},
	} {
		tenant, err := activation.OwningTenant(context.Background(), testCase.workspace)
		if err != nil {
			t.Fatalf("OwningTenant(%q): %v", testCase.workspace, err)
		}
		if tenant != testCase.want {
			t.Fatalf("OwningTenant(%q) = %q; want %q", testCase.workspace, tenant, testCase.want)
		}
	}
}

// TestWorkspaceActivationTenantFailsClosed proves a missing execution or a row
// without a tenant is an error, never an empty fresh-workspace tenant.
func TestWorkspaceActivationTenantFailsClosed(t *testing.T) {
	t.Run("missing execution", func(t *testing.T) {
		facts := &activationFactsStore{}
		activation := newTestWorkspaceActivation(t, facts)
		tenant, err := activation.OwningTenant(context.Background(), id.WorkspaceID("ws-tenant-absent"))
		if err == nil {
			t.Fatalf("OwningTenant = %q, nil; want an error for a workspace with no execution", tenant)
		}
		if tenant != "" {
			t.Fatalf("OwningTenant = %q on error; want an empty tenant", tenant)
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.NotFound {
			t.Fatalf("OwningTenant error = %v; want not_found", err)
		}
	})

	t.Run("row without a tenant column", func(t *testing.T) {
		workspaceID := id.WorkspaceID("ws-tenant-unreadable")
		facts := &activationFactsStore{rows: []store.Row{
			{Table: "executions", Values: map[string]any{
				"id":           "execution-tenant-unreadable",
				"workspace_id": string(workspaceID),
			}},
		}}
		activation := newTestWorkspaceActivation(t, facts)
		tenant, err := activation.OwningTenant(context.Background(), workspaceID)
		if err == nil {
			t.Fatalf("OwningTenant = %q, nil; want an error for an unreadable tenant fact", tenant)
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
			t.Fatalf("OwningTenant error = %v; want dependency_unavailable", err)
		}
	})
}
