//go:build integration

package composition

import (
	"fmt"
	"testing"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
)

// TestTaskInboxServesThePublicPageLimit proves the served composition accepts
// task.inbox page limits through api.MaxPageLimit (200) on both public edges,
// keeps the unchanged default for an omitted or zero limit, and refuses 201
// with the exact declared contract error. The inbox handler must not refuse a
// limit the public api.Page validator accepts.
func TestTaskInboxServesThePublicPageLimit(t *testing.T) {
	harness := newHarness(t)
	environment := harness.start(newFakeRuntime(modeComplete))
	environment.grantEngineer("admin-1", projectID)

	const totalItems = api.MaxPageLimit + 1
	seededTasks := make(map[string]bool, totalItems)
	for index := range totalItems {
		created := environment.mustInvoke(projectToken, "task.create", api.TaskCreateRequest{
			ProjectID: projectID,
			Title:     fmt.Sprintf("inbox-page-%03d", index),
		}, nil, fmt.Sprintf("inbox-page-create-%03d", index))
		var task api.TaskCreateResult
		environment.decode(created, &task)
		environment.mustInvoke(projectToken, "task.ask", api.TaskAskRequest{
			TaskID:     task.TaskID,
			Question:   fmt.Sprintf("question-%03d", index),
			Assignment: "role/engineer",
		}, &task.LifecycleVersion, fmt.Sprintf("inbox-page-ask-%03d", index))
		seededTasks[task.TaskID] = true
	}

	for _, limit := range []int{100, 101, api.MaxPageLimit} {
		result := environment.mustInvoke(projectToken, "task.inbox", api.TaskInboxRequest{
			ProjectID: projectID,
			Page:      &api.Page{Limit: limit},
		}, nil, "")
		var inbox api.TaskInboxResult
		environment.decode(result, &inbox)
		if len(inbox.Items) != limit {
			t.Fatalf("task.inbox page.limit %d returned %d items; want %d", limit, len(inbox.Items), limit)
		}
		if inbox.NextCursor == "" {
			t.Fatalf("task.inbox page.limit %d omitted next_cursor with %d items remaining", limit, totalItems-limit)
		}
	}

	// The MCP edge shares the handler, returns 200 items with a continuation
	// cursor, and yields the 201st item with an empty cursor on the next page.
	mcpFirst := environment.mcpMustCall(projectToken, "task.inbox", api.TaskInboxRequest{
		ProjectID: projectID,
		Page:      &api.Page{Limit: api.MaxPageLimit},
	}, nil, "")
	var firstPage api.TaskInboxResult
	environment.decode(mcpFirst, &firstPage)
	if len(firstPage.Items) != api.MaxPageLimit {
		t.Fatalf("MCP task.inbox page.limit %d returned %d items; want %d", api.MaxPageLimit, len(firstPage.Items), api.MaxPageLimit)
	}
	if firstPage.NextCursor == "" {
		t.Fatalf("MCP task.inbox page.limit %d omitted next_cursor", api.MaxPageLimit)
	}
	seen := make(map[string]bool, totalItems)
	for _, item := range firstPage.Items {
		if !seededTasks[item.TaskID] || seen[item.TaskID] {
			t.Fatalf("MCP first page carried unexpected or duplicate task %q", item.TaskID)
		}
		seen[item.TaskID] = true
	}

	mcpSecond := environment.mcpMustCall(projectToken, "task.inbox", api.TaskInboxRequest{
		ProjectID: projectID,
		Page:      &api.Page{Limit: api.MaxPageLimit, Cursor: firstPage.NextCursor},
	}, nil, "")
	var secondPage api.TaskInboxResult
	environment.decode(mcpSecond, &secondPage)
	if len(secondPage.Items) != 1 {
		t.Fatalf("MCP task.inbox second page returned %d items; want 1", len(secondPage.Items))
	}
	if secondPage.NextCursor != "" {
		t.Fatalf("MCP task.inbox final page next_cursor = %q; want empty", secondPage.NextCursor)
	}
	lastTaskID := secondPage.Items[0].TaskID
	if !seededTasks[lastTaskID] || seen[lastTaskID] {
		t.Fatalf("MCP second page carried unexpected or duplicate task %q", lastTaskID)
	}
	seen[lastTaskID] = true
	if len(seen) != totalItems {
		t.Fatalf("MCP paged inbox visited %d unique tasks; want %d", len(seen), totalItems)
	}

	// An omitted page and an explicit zero limit keep the default page size.
	for _, request := range []api.TaskInboxRequest{
		{ProjectID: projectID},
		{ProjectID: projectID, Page: &api.Page{Limit: 0}},
	} {
		result := environment.mustInvoke(projectToken, "task.inbox", request, nil, "")
		var inbox api.TaskInboxResult
		environment.decode(result, &inbox)
		if len(inbox.Items) != api.DefaultPageLimit {
			t.Fatalf("task.inbox default page (%+v) returned %d items; want %d", request.Page, len(inbox.Items), api.DefaultPageLimit)
		}
		if inbox.NextCursor == "" {
			t.Fatalf("task.inbox default page (%+v) omitted next_cursor", request.Page)
		}
	}

	// 201 is outside the contract bound and must carry the exact public error.
	_, publicError := environment.call(projectToken, "task.inbox", api.TaskInboxRequest{
		ProjectID: projectID,
		Page:      &api.Page{Limit: api.MaxPageLimit + 1},
	}, nil, "")
	if publicError.Code != api.CodeInvalidInput {
		t.Fatalf("task.inbox page.limit %d code = %q; want %q", api.MaxPageLimit+1, publicError.Code, api.CodeInvalidInput)
	}
	want := "page limit 201 is outside 0..200"
	if publicError.Message != want {
		t.Fatalf("task.inbox page.limit %d message = %q; want %q", api.MaxPageLimit+1, publicError.Message, want)
	}
	if class, ok := fault.ClassFor(fault.Code(publicError.Code)); !ok || class != fault.InvalidInput {
		t.Fatalf("task.inbox page.limit %d class = %v; want invalid_input", api.MaxPageLimit+1, class)
	}
}
