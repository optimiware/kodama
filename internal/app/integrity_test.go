package app

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/optimiware/kodama/api/client"
)

// transition moves a work item with the optimistic version of its current state.
func (d *devSlice) transition(api *client.ClientWithResponses, id string, to client.WorkState) *client.TransitionWorkItemResponse {
	d.t.Helper()
	ctx := context.Background()
	got, err := api.GetWorkItemWithResponse(ctx, string(devTenant), string(devProject), id)
	if err != nil || got.JSON200 == nil {
		d.t.Fatalf("get work item: %v %s", err, got.Body)
	}
	reason := "development composition"
	r, err := api.TransitionWorkItemWithResponse(ctx, string(devTenant), string(devProject), id,
		&client.TransitionWorkItemParams{IfMatch: client.IfMatch(`"` + strconv.FormatInt(int64(got.JSON200.Version), 10) + `"`)},
		client.TransitionRequest{To: to, Reason: &reason})
	if err != nil {
		d.t.Fatal(err)
	}
	return r
}

// An active work item cannot go back to ready while a writer attempt of its
// workspace has not settled: the composed work service consults the composed
// execution state, as the real assembly's writer guard does.
func checkWriterGuard(t *testing.T) {
	d := newDevSlice(t)
	ctx := context.Background()
	eng := d.api(devEngineerToken)
	criteria := []client.AcceptanceCriterion{{Id: "ac_1", Text: "the checks pass"}}
	w := d.seedWorldAs("m1-writer", "echo ok", "write", &criteria)
	for _, to := range []client.WorkState{"ready", "active"} {
		if r := d.transition(eng, w.item.Id, to); r.StatusCode() != http.StatusOK {
			t.Fatalf("to %s: %d %s", to, r.StatusCode(), r.Body)
		}
	}
	// With no writer attempt, leaving active is allowed (and back again).
	if r := d.transition(eng, w.item.Id, "ready"); r.StatusCode() != http.StatusOK {
		t.Fatalf("active to ready without a writer: %d %s", r.StatusCode(), r.Body)
	}
	if r := d.transition(eng, w.item.Id, "active"); r.StatusCode() != http.StatusOK {
		t.Fatalf("to active: %d %s", r.StatusCode(), r.Body)
	}

	run := d.createRun(eng, "m1-writer-run", w, string(d.Repo.Initial))
	expectStatus(t, run, run.Body, http.StatusCreated, "create run")
	d.dispatchAll()
	steps, _ := eng.ListRunStepsWithResponse(ctx, string(devTenant), string(devProject), run.JSON201.Id, &client.ListRunStepsParams{})
	if steps.JSON200 == nil || len(steps.JSON200.Items) != 1 || string(steps.JSON200.Items[0].Status) != "running" {
		t.Fatalf("the write attempt did not start: %s", steps.Body)
	}

	for _, to := range []client.WorkState{"ready", "review"} {
		r := d.transition(eng, w.item.Id, to)
		if r.StatusCode() < 400 || r.StatusCode() >= 500 || !strings.Contains(string(r.Body), "workspace") && !strings.Contains(string(r.Body), "writer") && r.StatusCode() != http.StatusConflict {
			t.Fatalf("active to %s with an unsettled writer: %d %s", to, r.StatusCode(), r.Body)
		}
	}
	if got, _ := eng.GetWorkItemWithResponse(ctx, string(devTenant), string(devProject), w.item.Id); got.JSON200 == nil || got.JSON200.State != "active" {
		t.Fatalf("the refused transition changed the item: %s", got.Body)
	}

	// Cancel the run and let the controller settle the attempt.
	if c, _ := eng.CancelRunWithResponse(ctx, string(devTenant), string(devProject), run.JSON201.Id); c.StatusCode() >= 300 {
		t.Fatalf("cancel: %d %s", c.StatusCode(), c.Body)
	}
	d.settle(run.JSON201.Id, "cancelled")
	d.settleWriters(w.item.Id)
	if r := d.transition(eng, w.item.Id, "ready"); r.StatusCode() != http.StatusOK {
		t.Fatalf("active to ready after settlement: %d %s", r.StatusCode(), r.Body)
	}
}

// A commit that fails leaves neither the mutation nor an idempotency record
// through the real handlers: the retry with the same key executes exactly once
// and is then replayed.
func checkKeyedWritesAreAllOrNothing(t *testing.T) {
	d := newDevSlice(t)
	ctx := context.Background()
	tenant, project := string(devTenant), string(devProject)
	eng := d.api(devEngineerToken)
	w := d.seedWorld("m1-atomic", "echo ok")
	commit := string(d.Repo.Initial)

	t.Run("run admission", func(t *testing.T) {
		d.Fault.Store(true)
		r := d.createRun(eng, "m1-atomic-run", w, commit)
		d.Fault.Store(false)
		if r.StatusCode() != http.StatusServiceUnavailable {
			t.Fatalf("a failed commit answered %d %s", r.StatusCode(), r.Body)
		}
		if runs, _ := d.Exec.List(ctx, devScope, listAll); len(runs) != 0 {
			t.Fatalf("the failed admission left %d runs", len(runs))
		}
		retry := d.createRun(eng, "m1-atomic-run", w, commit)
		expectStatus(t, retry, retry.Body, http.StatusCreated, "retry")
		if retry.HTTPResponse.Header.Get("Idempotent-Replayed") != "" {
			t.Fatal("the retry of a failed request was served as a replay")
		}
		again := d.createRun(eng, "m1-atomic-run", w, commit)
		if again.HTTPResponse.Header.Get("Idempotent-Replayed") != "true" || again.JSON201 == nil || again.JSON201.Id != retry.JSON201.Id {
			t.Fatalf("replay: %s", again.Body)
		}
		if runs, _ := d.Exec.List(ctx, devScope, listAll); len(runs) != 1 {
			t.Fatalf("%d runs", len(runs))
		}
	})

	t.Run("variable creation", func(t *testing.T) {
		create := func() *client.CreateVariableResponse {
			r, err := eng.CreateVariableWithBodyWithResponse(ctx, tenant, project, &client.CreateVariableParams{IdempotencyKey: "m1-atomic-var"},
				"application/json", strings.NewReader(`{"name":"region","value":"eu"}`))
			if err != nil {
				t.Fatal(err)
			}
			return r
		}
		list := func() int {
			l, err := eng.ListVariablesWithResponse(ctx, tenant, project, &client.ListVariablesParams{})
			if err != nil || l.JSON200 == nil {
				t.Fatalf("list: %v %s", err, l.Body)
			}
			return len(l.JSON200.Items)
		}
		d.Fault.Store(true)
		r := create()
		d.Fault.Store(false)
		if r.StatusCode() < 500 {
			t.Fatalf("a failed commit answered %d %s", r.StatusCode(), r.Body)
		}
		if list() != 0 {
			t.Fatal("the failed write left a variable")
		}
		retry := create()
		expectStatus(t, retry, retry.Body, http.StatusCreated, "retry")
		if retry.HTTPResponse.Header.Get("Idempotent-Replayed") != "" {
			t.Fatal("the retry of a failed request was served as a replay")
		}
		again := create()
		if again.StatusCode() != http.StatusCreated || again.HTTPResponse.Header.Get("Idempotent-Replayed") != "true" {
			t.Fatalf("replay: %d %s", again.StatusCode(), again.Body)
		}
		if list() != 1 {
			t.Fatal("the replay created another variable")
		}
	})
}
