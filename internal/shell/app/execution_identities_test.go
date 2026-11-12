package app

import (
	"context"
	"maps"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	kernelengine "github.com/optimiware/kodama/internal/kernel/engine"
	"github.com/optimiware/kodama/internal/shell/store"
)

// actionScanController is a controller seam over an in-memory action table. It
// applies the requested equality filters, ordering and limit the way the real
// adapter does, so a resolver that stops naming one of them reads a different
// row instead of silently passing. Every scan is recorded for the exact-query
// assertion.
type actionScanController struct {
	rows    []store.Row
	scanErr error
	scans   []store.ControllerScan
}

func (controller *actionScanController) Read(context.Context, string, string) (store.Row, error) {
	return store.Row{}, fault.New(fault.InternalFailure, "actionScanController does not read by identity")
}

func (controller *actionScanController) Execute(context.Context, string, store.Unit) (store.UnitResult, error) {
	return store.UnitResult{}, fault.New(fault.InternalFailure, "actionScanController does not execute")
}

func (controller *actionScanController) Scan(ctx context.Context, scan store.ControllerScan) (store.QueryPage, error) {
	if err := ctx.Err(); err != nil {
		return store.QueryPage{}, err
	}
	controller.scans = append(controller.scans, scan)
	if controller.scanErr != nil {
		return store.QueryPage{}, controller.scanErr
	}
	filtered := make([]store.Row, 0, len(controller.rows))
	for _, row := range controller.rows {
		if scan.Table != "" && row.Table != scan.Table {
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
			compared := compareActionColumn(
				filtered[left].Values[order.Column],
				filtered[right].Values[order.Column],
			)
			if compared == 0 {
				continue
			}
			if order.Desc {
				return compared > 0
			}
			return compared < 0
		}
		return false
	})
	if scan.Limit > 0 && len(filtered) > scan.Limit {
		filtered = filtered[:scan.Limit]
	}
	return store.QueryPage{Rows: filtered}, nil
}

func compareActionColumn(left, right any) int {
	leftInt, leftIsInt := left.(int64)
	rightInt, rightIsInt := right.(int64)
	if leftIsInt && rightIsInt {
		switch {
		case leftInt < rightInt:
			return -1
		case leftInt > rightInt:
			return 1
		default:
			return 0
		}
	}
	leftText, _ := left.(string)
	rightText, _ := right.(string)
	return strings.Compare(leftText, rightText)
}

func actionRow(index int64, values map[string]any) store.Row {
	row := store.Row{Table: "actions", Values: map[string]any{"index": index}}
	maps.Copy(row.Values, values)
	return row
}

// runningAppContainer is one running container action of an execution.
func runningAppContainer(idValue, executionID string) map[string]any {
	return map[string]any{
		"id":           idValue,
		"execution_id": executionID,
		"state":        "running",
		"kind":         "container",
		"workload_id":  "workload-1",
		"launch_key":   "launch-" + idValue,
	}
}

// TestExecutionIdentityResolvesTheRunningContainerOfTheExecution pins the
// action the actor.verify seam receives. The store holds historical and foreign
// rows, and every filter (execution, state, kind) plus the requested ordering
// decides whether the CURRENT running container action is the one resolved. A
// resolver that drops any of them returns a finished or foreign action, or an
// empty identity that would later authorize the wrong target.
func TestExecutionIdentityResolvesTheRunningContainerOfTheExecution(t *testing.T) {
	t.Run("a finished earlier container action never shadows the running one", func(t *testing.T) {
		finished := runningAppContainer("action-finished", "execution-1")
		finished["state"] = "succeeded"
		controller := &actionScanController{rows: []store.Row{
			actionRow(0, finished),
			actionRow(1, runningAppContainer("action-running", "execution-1")),
		}}
		resolver := &executionIdentities{controller: controller}

		identity, err := resolver.ResolveExecutionIdentity(context.Background(), id.ExecutionID("execution-1"))
		if err != nil {
			t.Fatalf("ResolveExecutionIdentity: %v", err)
		}
		if identity.ActionID != "action-running" {
			t.Fatalf("resolved action = %q; want the current running container action %q",
				identity.ActionID, "action-running")
		}
		if identity.ActionID == "action-finished" {
			t.Fatal("resolved action = the finished earlier container action")
		}
		if identity.WorkloadID != "workload-1" || identity.IdempotencyKey != "launch-action-running" {
			t.Fatalf("resolved identity = %+v; want the running action's workload and launch key", identity)
		}
	})

	t.Run("a running non-container action is ignored", func(t *testing.T) {
		nonContainer := runningAppContainer("action-process", "execution-1")
		nonContainer["kind"] = "process"
		controller := &actionScanController{rows: []store.Row{
			actionRow(0, nonContainer),
			actionRow(1, runningAppContainer("action-running", "execution-1")),
		}}
		resolver := &executionIdentities{controller: controller}

		identity, err := resolver.ResolveExecutionIdentity(context.Background(), id.ExecutionID("execution-1"))
		if err != nil {
			t.Fatalf("ResolveExecutionIdentity: %v", err)
		}
		if identity.ActionID != "action-running" {
			t.Fatalf("resolved action = %q; want the running container action %q, never a non-container action",
				identity.ActionID, "action-running")
		}
	})

	t.Run("only a running non-container action is refused", func(t *testing.T) {
		nonContainer := runningAppContainer("action-process", "execution-1")
		nonContainer["kind"] = "process"
		controller := &actionScanController{rows: []store.Row{actionRow(0, nonContainer)}}
		resolver := &executionIdentities{controller: controller}

		identity, err := resolver.ResolveExecutionIdentity(context.Background(), id.ExecutionID("execution-1"))
		if err == nil {
			t.Fatalf("ResolveExecutionIdentity returned non-container action %q", identity.ActionID)
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
			t.Fatalf("refusal class = %v; want dependency_unavailable (%v)", class, err)
		}
		if identity != (kernelengine.EffectIdentity{}) {
			t.Fatalf("refused identity = %+v; want the zero identity", identity)
		}
	})

	t.Run("another execution's running action never crosses", func(t *testing.T) {
		controller := &actionScanController{rows: []store.Row{
			actionRow(0, runningAppContainer("action-foreign", "execution-2")),
			actionRow(1, runningAppContainer("action-own", "execution-1")),
		}}
		resolver := &executionIdentities{controller: controller}

		identity, err := resolver.ResolveExecutionIdentity(context.Background(), id.ExecutionID("execution-1"))
		if err != nil {
			t.Fatalf("ResolveExecutionIdentity: %v", err)
		}
		if identity.ActionID != "action-own" {
			t.Fatalf("resolved action = %q; want this execution's action %q", identity.ActionID, "action-own")
		}
		if identity.ActionID == "action-foreign" {
			t.Fatal("resolved action = another execution's running action")
		}
	})

	t.Run("no running container action is refused without leaking ids", func(t *testing.T) {
		finished := runningAppContainer("action-finished", "execution-1")
		finished["state"] = "succeeded"
		controller := &actionScanController{rows: []store.Row{actionRow(0, finished)}}
		resolver := &executionIdentities{controller: controller}

		identity, err := resolver.ResolveExecutionIdentity(context.Background(), id.ExecutionID("execution-1"))
		if err == nil {
			t.Fatalf("ResolveExecutionIdentity accepted an execution with no running container action and returned %q",
				identity.ActionID)
		}
		if !strings.Contains(err.Error(), "no running container workload") {
			t.Fatalf("refusal = %v; want the no-running-container workload error", err)
		}
		if identity != (kernelengine.EffectIdentity{}) {
			t.Fatalf("refused identity = %+v; want the zero identity", identity)
		}
		if strings.Contains(err.Error(), "action-finished") || strings.Contains(err.Error(), "workload-1") {
			t.Fatalf("refusal leaked an action or workload id: %v", err)
		}
	})

	t.Run("the exact scan query names the execution, running state and container kind", func(t *testing.T) {
		controller := &actionScanController{rows: []store.Row{
			actionRow(0, runningAppContainer("action-running", "execution-1")),
		}}
		resolver := &executionIdentities{controller: controller}

		if _, err := resolver.ResolveExecutionIdentity(context.Background(), id.ExecutionID("execution-1")); err != nil {
			t.Fatalf("ResolveExecutionIdentity: %v", err)
		}
		if len(controller.scans) != 1 {
			t.Fatalf("controller scans = %d; want exactly 1", len(controller.scans))
		}
		scan := controller.scans[0]
		if scan.Table != "actions" {
			t.Fatalf("scan table = %q; want actions", scan.Table)
		}
		wantEquals := map[string]any{
			"execution_id": "execution-1",
			"state":        "running",
			"kind":         "container",
		}
		if !reflect.DeepEqual(scan.Equals, wantEquals) {
			t.Fatalf("scan equals = %#v; want exactly %#v", scan.Equals, wantEquals)
		}
		if scan.Limit != 1 {
			t.Fatalf("scan limit = %d; want 1 (the current action only)", scan.Limit)
		}
		wantOrder := []store.Order{{Column: "index"}, {Column: "id"}}
		if !reflect.DeepEqual(scan.OrderBy, wantOrder) {
			t.Fatalf("scan order = %#v; want %#v", scan.OrderBy, wantOrder)
		}
	})

	t.Run("a scan failure is returned fail closed", func(t *testing.T) {
		controller := &actionScanController{
			scanErr: fault.New(fault.DependencyUnavailable, "controller scan failed"),
		}
		resolver := &executionIdentities{controller: controller}

		identity, err := resolver.ResolveExecutionIdentity(context.Background(), id.ExecutionID("execution-1"))
		if err == nil {
			t.Fatalf("ResolveExecutionIdentity swallowed a scan failure and returned %+v", identity)
		}
		if class, ok := fault.ClassOf(err); !ok || class != fault.DependencyUnavailable {
			t.Fatalf("scan failure class = %v; want dependency_unavailable (%v)", class, err)
		}
		if !strings.Contains(err.Error(), "controller scan failed") {
			t.Fatalf("scan failure = %v; want the underlying scan error rather than a synthesized refusal", err)
		}
		if identity != (kernelengine.EffectIdentity{}) {
			t.Fatalf("scan failure identity = %+v; want the zero identity", identity)
		}
	})
}
