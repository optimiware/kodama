package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/client"
)

type eventsJSONResult struct {
	Events  []api.StreamEvent `json:"events"`
	Next    string            `json:"next"`
	HasMore bool              `json:"has_more"`
}

type watchJSONRecord struct {
	Kind       string                 `json:"kind"`
	ProjectID  string                 `json:"project_id,omitempty"`
	Tasks      []api.TaskSummaryView  `json:"tasks,omitempty"`
	Executions []api.ExecutionSummary `json:"executions,omitempty"`
	Inbox      []api.InboxItem        `json:"inbox,omitempty"`
	Event      *api.StreamEvent       `json:"event,omitempty"`
}

// runEvents reads or follows decoded project events.
func runEvents(ctx context.Context, config Config, args []string, ports Ports, deps CommandDeps) int {
	if hasHelpFlag(args) {
		return runHelp(ports.Out, ports.Err, []string{"events"})
	}
	var (
		projectFlag string
		typeFlags   []string
		sinceFlag   string
		limitRaw    string
		followFlag  bool
		outputFlag  string
	)
	spec := newArgSpec()
	spec.stringFlag(&projectFlag, "--project", "-p")
	spec.listFlag(&typeFlags, "--type", "-t")
	spec.stringFlag(&sinceFlag, "--since", "--after")
	spec.stringFlag(&limitRaw, "--limit", "-n")
	spec.boolFlag(&followFlag, "--follow", "-f")
	spec.stringFlag(&outputFlag, "--output")
	positionals, seen, err := spec.parse(args)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if len(positionals) > 0 {
		return usage(ports.Err, fmt.Sprintf("unexpected argument %q", positionals[0]))
	}
	format, err := resolveOutputFormat(outputFlag, config)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if err := config.Validate(); err != nil {
		return usage(ports.Err, err.Error())
	}
	projectID := resolveProject(projectFlag, config, deps)
	if projectID == "" {
		return usage(ports.Err, "events requires --project <id> or KODEMA_PROJECT")
	}

	types := parseEventTypeFlags(typeFlags)
	if err := api.ValidateEventTypes(types); err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	since := strings.TrimSpace(sinceFlag)
	if since != "" {
		if _, err := api.DecodeEventCursor(since); err != nil {
			return usage(ports.Err, publicMessage(err))
		}
	}
	limit := 0
	if seen["--limit"] || seen["-n"] {
		parsed, err := strconv.Atoi(strings.TrimSpace(limitRaw))
		if err != nil || parsed <= 0 || parsed > api.MaxPageLimit {
			return usage(ports.Err, fmt.Sprintf("--limit must be an integer between 1 and %d", api.MaxPageLimit))
		}
		limit = parsed
	}
	if ports.Client == nil {
		return failure(ports.Err, fmt.Errorf("no public client is configured"))
	}

	if followFlag {
		return runEventsFollow(ctx, ports, projectID, since, types, limit, format)
	}
	return runEventsOnce(ctx, ports, projectID, since, types, limit, format)
}

func runEventsOnce(ctx context.Context, ports Ports, projectID, since string, types []string, limit int, format OutputFormat) int {
	req := api.EventReadRequest{
		ProjectID: projectID,
		Types:     types,
	}
	if since != "" || limit > 0 {
		req.Page = &api.Page{
			Cursor: since,
			Limit:  limit,
		}
	}
	if err := req.Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	input, err := json.Marshal(req)
	if err != nil {
		return failure(ports.Err, err)
	}
	invocation := api.Invocation{
		Operation: "event.read",
		Request:   api.Request{Input: input},
	}
	if err := invocation.Validate(); err != nil {
		return usage(ports.Err, publicMessage(err))
	}
	result, err := ports.Client.Invoke(ctx, invocation)
	if err != nil {
		return failure(ports.Err, err)
	}
	var page api.EventReadResult
	if len(result.Body) > 0 {
		if err := json.Unmarshal(result.Body, &page); err != nil {
			return failure(ports.Err, fmt.Errorf("invalid event.read response: %w", err))
		}
	}

	decoded := make([]api.StreamEvent, 0, len(page.Events))
	for _, evt := range page.Events {
		decoded = append(decoded, decodePublicEvent(evt))
	}

	if format == OutputJSON {
		out := eventsJSONResult{
			Events:  decoded,
			Next:    page.Next,
			HasMore: page.HasMore,
		}
		if err := json.NewEncoder(ports.Out).Encode(out); err != nil {
			return failure(ports.Err, err)
		}
		return ExitSuccess
	}

	tw := tabwriter.NewWriter(ports.Out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AT\tTYPE\tSUBJECT\tSUMMARY")
	for _, evt := range decoded {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			formatEventTime(evt.At),
			formatEventType(evt),
			formatEventSubject(evt),
			summarizeEventData(evt),
		)
	}
	if err := tw.Flush(); err != nil {
		return failure(ports.Err, err)
	}
	return ExitSuccess
}

func runEventsFollow(ctx context.Context, ports Ports, projectID, since string, types []string, limit int, format OutputFormat) int {
	stream, err := openProjectEventStream(ctx, ports.Client, client.EventStreamRequest{
		ProjectID: projectID,
		Since:     since,
		Types:     types,
	})
	if err != nil {
		if ctx.Err() != nil {
			fmt.Fprintln(ports.Err, "events canceled")
			return ExitFailure
		}
		return failure(ports.Err, err)
	}
	defer func() { _ = stream.Close() }()

	var (
		lastSeq uint64
		seen    = make(map[string]struct{})
		emitted int
	)
	for {
		evt, err := stream.Next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				fmt.Fprintln(ports.Err, "events canceled")
				return ExitFailure
			}
			if errors.Is(err, io.EOF) {
				return ExitSuccess
			}
			return failure(ports.Err, err)
		}
		if evt.Sequence > 0 && evt.Sequence <= lastSeq {
			continue
		}
		if evt.ID != "" {
			if _, dup := seen[evt.ID]; dup {
				continue
			}
			if len(seen) >= 4096 {
				clear(seen)
			}
			seen[evt.ID] = struct{}{}
		}
		if evt.Sequence > lastSeq {
			lastSeq = evt.Sequence
		}

		if format == OutputJSON {
			if err := json.NewEncoder(ports.Out).Encode(evt); err != nil {
				return failure(ports.Err, err)
			}
		} else {
			fmt.Fprintln(ports.Out, formatStreamEventLine(evt))
		}
		emitted++
		if limit > 0 && emitted >= limit {
			return ExitSuccess
		}
	}
}

// runProjectWatch shows an initial active snapshot for a project and then
// streams live status transitions via the project SSE event stream. When
// until == "request", it exits 0 as soon as a pending assigned request or
// waiting task question is present or arrives on the stream.
func runProjectWatch(ctx context.Context, ports Ports, projectID, until string, format OutputFormat) int {
	tasks, executions, inboxItems, err := fetchProjectWatchSnapshot(ctx, ports, projectID)
	if err != nil {
		if ctx.Err() != nil {
			return watchCanceled(ports.Err)
		}
		return failure(ports.Err, err)
	}

	if err := renderProjectWatchSnapshot(ports.Out, format, projectID, tasks, executions, inboxItems); err != nil {
		return failure(ports.Err, err)
	}

	if until == "request" {
		if attention := filterAttentionInboxItems(inboxItems); len(attention) > 0 {
			if err := renderProjectWatchAttention(ports.Out, format, projectID, attention); err != nil {
				return failure(ports.Err, err)
			}
			return ExitSuccess
		}
	}

	stream, err := openProjectEventStream(ctx, ports.Client, client.EventStreamRequest{
		ProjectID: projectID,
	})
	if err != nil {
		if ctx.Err() != nil {
			return watchCanceled(ports.Err)
		}
		return failure(ports.Err, err)
	}
	defer func() { _ = stream.Close() }()

	// Re-check inbox immediately after opening the live stream so an attention
	// item committed between the initial snapshot and stream subscription is
	// never missed.
	if until == "request" {
		freshInbox, _, _, inboxErr := fetchInboxPages(ctx, ports, projectID, nil)
		if inboxErr != nil {
			if ctx.Err() != nil {
				return watchCanceled(ports.Err)
			}
			return failure(ports.Err, inboxErr)
		}
		if attention := filterAttentionInboxItems(freshInbox); len(attention) > 0 {
			if err := renderProjectWatchAttention(ports.Out, format, projectID, attention); err != nil {
				return failure(ports.Err, err)
			}
			return ExitSuccess
		}
	}

	for {
		evt, err := stream.Next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return watchCanceled(ports.Err)
			}
			if errors.Is(err, io.EOF) && until == "" {
				return ExitSuccess
			}
			return failure(ports.Err, err)
		}

		if format == OutputJSON {
			record := watchJSONRecord{
				Kind:      "event",
				ProjectID: projectID,
				Event:     &evt,
			}
			if err := json.NewEncoder(ports.Out).Encode(record); err != nil {
				return failure(ports.Err, err)
			}
		} else {
			fmt.Fprintln(ports.Out, formatStreamEventLine(evt))
		}

		if until == "request" && isRequestAttentionEvent(evt.Type) {
			freshInbox, _, _, inboxErr := fetchInboxPages(ctx, ports, projectID, nil)
			if inboxErr != nil {
				if ctx.Err() != nil {
					return watchCanceled(ports.Err)
				}
				return failure(ports.Err, inboxErr)
			}
			if attention := filterAttentionInboxItems(freshInbox); len(attention) > 0 {
				if err := renderProjectWatchAttention(ports.Out, format, projectID, attention); err != nil {
					return failure(ports.Err, err)
				}
				return ExitSuccess
			}
		}
	}
}

func fetchProjectWatchSnapshot(ctx context.Context, ports Ports, projectID string) ([]api.TaskSummaryView, []api.ExecutionSummary, []api.InboxItem, error) {
	tasks, err := fetchProjectTasks(ctx, ports, projectID)
	if err != nil {
		return nil, nil, nil, err
	}
	activeTasks := make([]api.TaskSummaryView, 0, len(tasks))
	for _, item := range tasks {
		if item.State == "open" || item.State == "waiting" {
			activeTasks = append(activeTasks, item)
		}
	}

	executions, err := fetchProjectExecutions(ctx, ports, projectID)
	if err != nil {
		return nil, nil, nil, err
	}
	activeExecutions := make([]api.ExecutionSummary, 0, len(executions))
	for _, item := range executions {
		if item.State == "queued" || item.State == "running" {
			activeExecutions = append(activeExecutions, item)
		}
	}

	inboxItems, _, _, err := fetchInboxPages(ctx, ports, projectID, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	return activeTasks, activeExecutions, inboxItems, nil
}

func fetchProjectTasks(ctx context.Context, ports Ports, projectID string) ([]api.TaskSummaryView, error) {
	var (
		all    []api.TaskSummaryView
		cursor string
	)
	for {
		req := api.TaskListRequest{ProjectID: projectID}
		if cursor != "" {
			req.Page = &api.Page{Cursor: cursor}
		}
		input, err := json.Marshal(req)
		if err != nil {
			return nil, err
		}
		result, err := ports.Client.Invoke(ctx, api.Invocation{
			Operation: "task.list",
			Request:   api.Request{Input: input},
		})
		if err != nil {
			return nil, err
		}
		var page api.TaskListResult
		if len(result.Body) > 0 {
			if err := json.Unmarshal(result.Body, &page); err != nil {
				return nil, fmt.Errorf("invalid task.list response: %w", err)
			}
		}
		all = append(all, page.Tasks...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return all, nil
}

func fetchProjectExecutions(ctx context.Context, ports Ports, projectID string) ([]api.ExecutionSummary, error) {
	var (
		all    []api.ExecutionSummary
		cursor string
	)
	for {
		req := api.ExecutionListRequest{ProjectID: projectID}
		if cursor != "" {
			req.Page = &api.Page{Cursor: cursor}
		}
		input, err := json.Marshal(req)
		if err != nil {
			return nil, err
		}
		result, err := ports.Client.Invoke(ctx, api.Invocation{
			Operation: "execution.list",
			Request:   api.Request{Input: input},
		})
		if err != nil {
			return nil, err
		}
		var page api.ExecutionListResult
		if len(result.Body) > 0 {
			if err := json.Unmarshal(result.Body, &page); err != nil {
				return nil, fmt.Errorf("invalid execution.list response: %w", err)
			}
		}
		all = append(all, page.Executions...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return all, nil
}

func renderProjectWatchSnapshot(
	out io.Writer,
	format OutputFormat,
	projectID string,
	tasks []api.TaskSummaryView,
	executions []api.ExecutionSummary,
	inbox []api.InboxItem,
) error {
	if format == OutputJSON {
		if tasks == nil {
			tasks = []api.TaskSummaryView{}
		}
		if executions == nil {
			executions = []api.ExecutionSummary{}
		}
		if inbox == nil {
			inbox = []api.InboxItem{}
		}
		return json.NewEncoder(out).Encode(watchJSONRecord{
			Kind:       "snapshot",
			ProjectID:  projectID,
			Tasks:      tasks,
			Executions: executions,
			Inbox:      inbox,
		})
	}

	fmt.Fprintf(out, "watching project %s (active_tasks=%d active_executions=%d inbox=%d)\n",
		projectID, len(tasks), len(executions), len(inbox))
	for _, item := range tasks {
		fmt.Fprintf(out, "  task       %s [%s] %s\n", orDash(item.TaskID), orDash(item.State), singleLine(item.Title, 100))
	}
	for _, item := range executions {
		detail := ""
		if item.TaskID != "" {
			detail += " task=" + item.TaskID
		}
		if item.PipelineID != "" {
			detail += " pipeline=" + item.PipelineID
		}
		fmt.Fprintf(out, "  execution  %s [%s]%s\n", orDash(item.ExecutionID), orDash(item.State), detail)
	}
	for _, item := range inbox {
		target := item.ExecutionID
		if target == "" {
			target = item.TaskID
		}
		if item.ActionID != "" {
			target += "/" + item.ActionID
		}
		fmt.Fprintf(out, "  inbox      %s %s %s\n", orDash(item.Kind), orDash(target), summarizeInboxItem(item, false))
	}
	return nil
}

func renderProjectWatchAttention(out io.Writer, format OutputFormat, projectID string, items []api.InboxItem) error {
	if format == OutputJSON {
		return json.NewEncoder(out).Encode(watchJSONRecord{
			Kind:      "attention",
			ProjectID: projectID,
			Inbox:     items,
		})
	}
	fmt.Fprintln(out, "attention needed:")
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KIND\tTASK\tEXECUTION\tACTION\tVERSION\tPROMPT/SUMMARY")
	for _, item := range items {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			orDash(item.Kind),
			orDash(item.TaskID),
			orDash(item.ExecutionID),
			orDash(item.ActionID),
			formatInboxVersion(item),
			summarizeInboxItem(item, false),
		)
	}
	return tw.Flush()
}

func filterAttentionInboxItems(items []api.InboxItem) []api.InboxItem {
	var attention []api.InboxItem
	for _, item := range items {
		if item.Kind == "request" || item.Kind == "waiting_task" {
			attention = append(attention, item)
		}
	}
	return attention
}

func isRequestAttentionEvent(eventType string) bool {
	switch eventType {
	case "action.requested", "request.activated", "task.question":
		return true
	default:
		return false
	}
}

func isInboxRelevantEvent(eventType string) bool {
	switch eventType {
	case "action.requested",
		"request.activated",
		"request.resolved",
		"action.transition",
		"task.question",
		"task.reply",
		"task.created",
		"task.cancelled",
		"execution.failed",
		"execution.succeeded",
		"execution.cancelled",
		"resource.verified":
		return true
	default:
		return false
	}
}

func openProjectEventStream(ctx context.Context, transport client.Transport, req client.EventStreamRequest) (client.EventStream, error) {
	streamer, ok := transport.(client.EventStreamer)
	if !ok {
		return nil, fmt.Errorf("the configured client does not support live event streaming")
	}
	return streamer.StreamEvents(ctx, req)
}

func parseEventTypeFlags(rawFlags []string) []string {
	var (
		out  []string
		seen = make(map[string]struct{})
	)
	for _, raw := range rawFlags {
		for _, part := range strings.Split(raw, ",") {
			trimmed := strings.TrimSpace(part)
			if trimmed == "" {
				continue
			}
			if _, dup := seen[trimmed]; dup {
				continue
			}
			seen[trimmed] = struct{}{}
			out = append(out, trimmed)
		}
	}
	return out
}

func decodePublicEvent(evt api.Event) api.StreamEvent {
	return api.StreamEvent{
		ID:             evt.ID,
		Type:           evt.Type,
		ReportType:     evt.ReportType,
		Source:         evt.Source,
		At:             evt.At,
		Scope:          evt.Scope,
		SubjectID:      evt.SubjectID,
		SubjectVersion: evt.SubjectVersion,
		CorrelationID:  evt.CorrelationID,
		CausationID:    evt.CausationID,
		Data:           api.DecodeEventData(evt.Data),
	}
}

func formatEventTime(at time.Time) string {
	if at.IsZero() {
		return "-"
	}
	return at.UTC().Format(time.RFC3339)
}

func formatEventType(evt api.StreamEvent) string {
	if evt.ReportType != "" {
		return fmt.Sprintf("%s (%s)", evt.Type, evt.ReportType)
	}
	return orDash(evt.Type)
}

func formatEventSubject(evt api.StreamEvent) string {
	if evt.SubjectID == "" {
		return "-"
	}
	if evt.SubjectVersion > 0 {
		return fmt.Sprintf("%s@%d", evt.SubjectID, evt.SubjectVersion)
	}
	return evt.SubjectID
}

func formatStreamEventLine(evt api.StreamEvent) string {
	return fmt.Sprintf("%s  %-22s  %-24s  %s",
		formatEventTime(evt.At),
		formatEventType(evt),
		formatEventSubject(evt),
		summarizeEventData(evt),
	)
}

func summarizeEventData(evt api.StreamEvent) string {
	if len(evt.Data) == 0 {
		return "-"
	}
	var object map[string]any
	if err := json.Unmarshal(evt.Data, &object); err == nil && len(object) > 0 {
		keys := make([]string, 0, len(object))
		for key := range object {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, fmt.Sprintf("%s=%s", key, formatSummaryValue(object[key])))
		}
		return singleLine(strings.Join(parts, " "), 160)
	}
	var str string
	if err := json.Unmarshal(evt.Data, &str); err == nil {
		return singleLine(str, 160)
	}
	return singleLine(string(evt.Data), 160)
}

func formatSummaryValue(value any) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case string:
		return singleLine(v, 80)
	case bool:
		return strconv.FormatBool(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return singleLine(string(encoded), 80)
	}
}
