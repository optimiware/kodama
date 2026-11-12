package work

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"sort"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/inbox"
	"github.com/optimiware/kodama/internal/shell/request"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/units/execution/snapshotdoc"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// defaultInboxLimit is the page size when the caller asks for zero.
const defaultInboxLimit = 50

// inboxCursor is the opaque keyset token of one merged inbox page.
type inboxCursor struct {
	TenantID  string    `json:"tenant_id"`
	ProjectID string    `json:"project_id"`
	At        time.Time `json:"at"`
	Kind      string    `json:"kind"`
	ID        string    `json:"id"`
}

// inboxEntry is one merged inbox item with its deterministic sort key.
type inboxEntry struct {
	item     inbox.Item
	identity string
}

// Query returns one authoritative, stably paginated inbox page. It is a pure
// read: the three sources are the authoritative task, execution and action rows,
// so a stale event can never strand or resurrect an item.
func (adapter *Inbox) Query(ctx context.Context, query inbox.Query) (inbox.Page, error) {
	if err := validProjectScope(query.Scope); err != nil {
		return inbox.Page{}, err
	}
	if query.Principal.Scope != query.Scope {
		return inbox.Page{}, fault.New(fault.Forbidden, "inbox principal scope does not match the query scope")
	}
	if query.Limit < 0 || query.Limit > inbox.MaxPageSize {
		return inbox.Page{}, fault.New(fault.InvalidInput, "inbox limit is outside the allowed range")
	}
	limit := query.Limit
	if limit == 0 {
		limit = defaultInboxLimit
	}
	var cursor *inboxCursor
	if query.Cursor != "" {
		decoded, err := decodeInboxCursor(query.Cursor, query.Scope)
		if err != nil {
			return inbox.Page{}, err
		}
		cursor = &decoded
	}
	selected, err := selectedKinds(query.Kinds)
	if err != nil {
		return inbox.Page{}, err
	}
	entries := make([]inboxEntry, 0)
	if selected[inbox.KindRequest] {
		requestItems, err := adapter.requestItems(ctx, query.Scope, query.Principal)
		if err != nil {
			return inbox.Page{}, err
		}
		entries = append(entries, requestItems...)
	}
	if selected[inbox.KindWaitingTask] {
		waiting, err := adapter.waitingTaskItems(ctx, query.Scope, query.Principal)
		if err != nil {
			return inbox.Page{}, err
		}
		entries = append(entries, waiting...)
	}
	if selected[inbox.KindFailedTask] {
		failed, err := adapter.failedTaskItems(ctx, query.Scope, query.Principal)
		if err != nil {
			return inbox.Page{}, err
		}
		entries = append(entries, failed...)
	}
	sort.SliceStable(entries, func(left, right int) bool {
		return inboxLess(entries[left], entries[right])
	})
	filtered := entries
	if cursor != nil {
		filtered = make([]inboxEntry, 0, len(entries))
		for _, entry := range entries {
			if inboxAfter(entry, *cursor) {
				filtered = append(filtered, entry)
			}
		}
	}
	page := inbox.Page{}
	if len(filtered) > limit {
		filtered = filtered[:limit]
		last := filtered[len(filtered)-1]
		next, err := encodeInboxCursor(query.Scope, last)
		if err != nil {
			return inbox.Page{}, err
		}
		page.NextCursor = next
	}
	page.Items = make([]inbox.Item, 0, len(filtered))
	for _, entry := range filtered {
		page.Items = append(page.Items, entry.item)
	}
	return page, nil
}

// selectedKinds validates the requested kinds. An empty request means every
// kind.
func selectedKinds(kinds []inbox.Kind) (map[inbox.Kind]bool, error) {
	selected := make(map[inbox.Kind]bool, len(kinds))
	if len(kinds) == 0 {
		for _, kind := range inbox.Kinds() {
			selected[kind] = true
		}
		return selected, nil
	}
	for _, kind := range kinds {
		if !kind.Valid() {
			return nil, fault.New(fault.InvalidInput, "unknown inbox kind")
		}
		selected[kind] = true
	}
	return selected, nil
}

// requestItems lists the active request actions assigned to the principal.
//
// The item is self-sufficient: beside the exact assignment it carries the
// immutable resolved subject persisted at activation, the declared response
// output names from the authoritative admitted snapshot and the action
// deadline. A stored lookup, a missing/empty subject, an unreadable snapshot or
// a declaration that no longer matches the action row is corruption and fails
// the read closed; it is never dropped, guessed or leaked.
func (adapter *Inbox) requestItems(ctx context.Context, scope tenancy.Scope, principal identity.Principal) ([]inboxEntry, error) {
	rows, err := scanAll(ctx, adapter.scanner, scope, store.ScanQuery{
		Table: "actions",
		Equals: map[string]any{
			"kind":  string(definition.ActionKindRequest),
			"state": string(kernel.ActionRunning),
		},
		OrderBy: []store.Order{{Column: "id"}},
		Limit:   store.MaxQueryLimit,
	})
	if err != nil {
		return nil, err
	}
	admitted := make(map[id.ExecutionID]snapshotdoc.Admitted)
	entries := make([]inboxEntry, 0, len(rows))
	for _, row := range rows {
		assignment, err := decodeJSON[request.Assignment](bytesValue(row, "request_assignment"))
		if err != nil {
			return nil, err
		}
		if !assignment.Valid() {
			return nil, fault.New(fault.InternalFailure, "a running request action has no readable assignment")
		}
		// Decode and validate the complete projection BEFORE the assignedTo
		// filter: one corrupt request row fails every principal's inbox read
		// instead of being silently skipped as unauthorised. That is
		// fail-closed by design; a corruption must be loud, never an invisible
		// item. This loop only reads.
		executionID := id.ExecutionID(textValue(row, "execution_id"))
		snapshot, ok := admitted[executionID]
		if !ok {
			executionRow, err := adapter.executor.Read(ctx, scope, "executions", string(executionID))
			if err != nil {
				if class, found := fault.ClassOf(err); found && class == fault.NotFound {
					return nil, fault.New(fault.InternalFailure, "a request action references a missing execution")
				}
				return nil, err
			}
			snapshot, err = snapshotdoc.Decode(bytesValue(executionRow, "snapshot"))
			if err != nil {
				return nil, err
			}
			admitted[executionID] = snapshot
		}
		subject, err := resolvedRequestSubject(row)
		if err != nil {
			return nil, err
		}
		outputs, err := requestOutputNames(snapshot, row)
		if err != nil {
			return nil, err
		}
		if !assignedTo(assignment, principal) {
			continue
		}
		item := inbox.Item{
			Kind:        inbox.KindRequest,
			ExecutionID: executionID,
			ActionID:    id.ActionID(textValue(row, "id")),
			Assignment:  assignment,
			Subject:     subject,
			Outputs:     outputs,
			Version:     versionValue(row, "version"),
			// actions has no created_at; the deadline is the one durable action
			// timestamp and is set at activation.
			At: timeValue(row, "deadline"),
		}
		entries = append(entries, inboxEntry{item: item, identity: string(item.ActionID)})
	}
	return entries, nil
}

// resolvedRequestSubject decodes the immutable actions.request_subject column.
// A running request is answerable only with a non-empty subject of resolved
// literals; a stored reference is an unresolved runtime lookup and fails
// closed instead of leaking the declaration.
func resolvedRequestSubject(row store.Row) (*definition.Subject, error) {
	data := bytesValue(row, "request_subject")
	if len(data) == 0 {
		return nil, fault.New(fault.InternalFailure, "a running request action has no persisted subject")
	}
	var subject definition.Subject
	if err := decodeJSONInto(data, &subject); err != nil {
		return nil, err
	}
	if subject.Len() == 0 {
		return nil, fault.New(fault.InternalFailure, "a running request action has an empty subject")
	}
	for name, value := range subject.Fields() {
		if name == "" {
			return nil, fault.New(fault.InternalFailure, "a running request subject has an empty field name")
		}
		if value.Kind != definition.SubjectLiteral {
			return nil, fault.New(fault.InternalFailure, "a running request subject carries an unresolved lookup")
		}
	}
	resolved := subject
	return &resolved, nil
}

// requestOutputNames returns the declared response output names of one request
// action in declaration order from the authoritative admitted snapshot. The
// declaration must match the persisted action row exactly.
func requestOutputNames(snapshot snapshotdoc.Admitted, row store.Row) ([]id.OutputKey, error) {
	index := int(intValue(row, "index"))
	name := textValue(row, "name")
	for _, action := range snapshot.Actions {
		if action.Index != index {
			continue
		}
		if action.Kind != definition.ActionKindRequest || action.Request == nil || string(action.Name) != name {
			return nil, fault.New(fault.InternalFailure, "a request action does not match its admitted declaration")
		}
		outputs := make([]id.OutputKey, 0, len(action.Request.Outputs))
		for _, output := range action.Request.Outputs {
			if _, err := id.ParseOutputKey(string(output)); err != nil {
				return nil, fault.Wrap(fault.InternalFailure, "a declared request output name is invalid", err)
			}
			outputs = append(outputs, output)
		}
		return outputs, nil
	}
	return nil, fault.New(fault.InternalFailure, "a request action has no admitted declaration")
}

// decodeJSONInto decodes a stored JSON column into target, mapping a malformed
// row to internal_failure.
func decodeJSONInto(data []byte, target any) error {
	if err := json.Unmarshal(data, target); err != nil {
		return fault.Wrap(fault.InternalFailure, "a stored request projection column is not readable", err)
	}
	return nil
}

// waitingTaskItems lists the waiting tasks whose question is assigned to the
// principal.
func (adapter *Inbox) waitingTaskItems(ctx context.Context, scope tenancy.Scope, principal identity.Principal) ([]inboxEntry, error) {
	rows, err := scanAll(ctx, adapter.scanner, scope, store.ScanQuery{
		Table:   "tasks",
		Equals:  map[string]any{"state": string(task.StateWaiting)},
		OrderBy: []store.Order{{Column: "id"}},
		Limit:   store.MaxQueryLimit,
	})
	if err != nil {
		return nil, err
	}
	entries := make([]inboxEntry, 0, len(rows))
	for _, row := range rows {
		assignment, err := decodeJSON[request.Assignment](bytesValue(row, "question_assignment"))
		if err != nil {
			return nil, err
		}
		if !assignedTo(assignment, principal) {
			continue
		}
		references, err := decodeJSON[[]string](bytesValue(row, "question_references"))
		if err != nil {
			return nil, err
		}
		item := inbox.Item{
			Kind:             inbox.KindWaitingTask,
			TaskID:           id.TaskID(textValue(row, "id")),
			Assignment:       assignment,
			QuestionID:       task.QuestionID(textValue(row, "question_id")),
			Question:         textValue(row, "question_body"),
			References:       references,
			Version:          versionValue(row, "question_version"),
			LifecycleVersion: versionValue(row, "lifecycle_version"),
			At:               timeValue(row, "question_at"),
		}
		entries = append(entries, inboxEntry{item: item, identity: string(item.TaskID)})
	}
	return entries, nil
}

// failedTaskItems lists the open tasks whose current execution failed.
//
// Follow-up of a failed task belongs to human operators, so the principal
// filter runs before the scan: an unauthorized principal never pays for it and
// its rows can never enter the merge, the page slice or a cursor key. The
// projected item names only the task; the execution identity stays local to the
// authoritative state lookup.
func (adapter *Inbox) failedTaskItems(ctx context.Context, scope tenancy.Scope, principal identity.Principal) ([]inboxEntry, error) {
	if principal.Kind != identity.KindHuman || !principal.HasRole(tenancy.RoleOperator) {
		return nil, nil
	}
	rows, err := scanAll(ctx, adapter.scanner, scope, store.ScanQuery{
		Table:   "tasks",
		Equals:  map[string]any{"state": string(task.StateOpen)},
		OrderBy: []store.Order{{Column: "id"}},
		Limit:   store.MaxQueryLimit,
	})
	if err != nil {
		return nil, err
	}
	entries := make([]inboxEntry, 0, len(rows))
	for _, row := range rows {
		executionID := id.ExecutionID(textValue(row, "current_execution_id"))
		if executionID == "" {
			continue
		}
		execution, err := adapter.executor.Read(ctx, scope, "executions", string(executionID))
		if err != nil {
			if class, ok := fault.ClassOf(err); ok && class == fault.NotFound {
				continue
			}
			return nil, err
		}
		if textValue(execution, "state") != string(kernel.ExecutionFailed) {
			continue
		}
		item := inbox.Item{
			Kind:    inbox.KindFailedTask,
			TaskID:  id.TaskID(textValue(row, "id")),
			Version: versionValue(row, "lifecycle_version"),
			At:      timeValue(execution, "created_at"),
		}
		entries = append(entries, inboxEntry{item: item, identity: string(item.TaskID)})
	}
	return entries, nil
}

// assignedTo reports whether one immutable assignment selects the principal.
//
// A role assignment is answered only by a human currently holding that exact
// role: the inbox admits service principals, and a service that happens to carry
// the role must not receive a role-assigned question (its body, references and
// versions) or request. A service assignment is answered only by that exact
// service principal. Nothing widens either rule.
func assignedTo(assignment request.Assignment, principal identity.Principal) bool {
	switch assignment.Kind {
	case request.AssignmentRole:
		return principal.Kind == identity.KindHuman && principal.HasRole(assignment.Role)
	case request.AssignmentService:
		return principal.Kind == identity.KindService && principal.ID == id.PrincipalID(assignment.ServiceID)
	default:
		return false
	}
}

// inboxLess orders items by (at, kind, identity).
func inboxLess(left, right inboxEntry) bool {
	if !left.item.At.Equal(right.item.At) {
		return left.item.At.Before(right.item.At)
	}
	if left.item.Kind != right.item.Kind {
		return inboxKindRank(left.item.Kind) < inboxKindRank(right.item.Kind)
	}
	return left.identity < right.identity
}

// inboxKindRank orders kinds deterministically inside one instant.
func inboxKindRank(kind inbox.Kind) int {
	for index, candidate := range inbox.Kinds() {
		if candidate == kind {
			return index
		}
	}
	return len(inbox.Kinds())
}

// inboxAfter reports whether one entry sorts strictly after the cursor key.
func inboxAfter(entry inboxEntry, cursor inboxCursor) bool {
	switch {
	case entry.item.At.After(cursor.At):
		return true
	case entry.item.At.Before(cursor.At):
		return false
	}
	rank, cursorRank := inboxKindRank(entry.item.Kind), inboxKindRank(inbox.Kind(cursor.Kind))
	if rank != cursorRank {
		return rank > cursorRank
	}
	return entry.identity > cursor.ID
}

// encodeInboxCursor renders the opaque continuation token.
func encodeInboxCursor(scope tenancy.Scope, entry inboxEntry) (string, error) {
	encoded, err := json.Marshal(inboxCursor{
		TenantID:  string(scope.TenantID),
		ProjectID: string(scope.ProjectID),
		At:        entry.item.At.UTC(),
		Kind:      string(entry.item.Kind),
		ID:        entry.identity,
	})
	if err != nil {
		return "", fault.Wrap(fault.InternalFailure, "inbox cursor cannot be encoded", err)
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

// decodeInboxCursor parses one continuation token and binds it to the scope.
func decodeInboxCursor(token string, scope tenancy.Scope) (inboxCursor, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return inboxCursor{}, fault.New(fault.InvalidInput, "inbox cursor is not a valid opaque token")
	}
	var cursor inboxCursor
	if err := json.Unmarshal(decoded, &cursor); err != nil {
		return inboxCursor{}, fault.New(fault.InvalidInput, "inbox cursor is not a valid opaque token")
	}
	if cursor.TenantID != string(scope.TenantID) || cursor.ProjectID != string(scope.ProjectID) {
		return inboxCursor{}, fault.New(fault.Forbidden, "inbox cursor belongs to another scope")
	}
	if !inbox.Kind(cursor.Kind).Valid() || cursor.ID == "" {
		return inboxCursor{}, fault.New(fault.InvalidInput, "inbox cursor has no keyset")
	}
	return cursor, nil
}
