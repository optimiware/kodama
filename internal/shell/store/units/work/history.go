package work

import (
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// historyCursor is the opaque keyset token of one task history page. It binds
// the scope and task so a token from another task or scope is refused.
type historyCursor struct {
	TenantID  string    `json:"tenant_id"`
	ProjectID string    `json:"project_id"`
	TaskID    string    `json:"task_id"`
	At        time.Time `json:"at"`
	Kind      string    `json:"kind"`
	ID        string    `json:"id"`
}

// encodeHistoryCursor renders the opaque continuation token.
func encodeHistoryCursor(scope tenancy.Scope, taskID id.TaskID, at time.Time, kind string, identity string) (string, error) {
	encoded, err := json.Marshal(historyCursor{
		TenantID:  string(scope.TenantID),
		ProjectID: string(scope.ProjectID),
		TaskID:    string(taskID),
		At:        at.UTC(),
		Kind:      kind,
		ID:        identity,
	})
	if err != nil {
		return "", fault.Wrap(fault.InternalFailure, "history cursor cannot be encoded", err)
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

// decodeHistoryCursor parses one continuation token and binds it to the caller's
// scope and task.
func decodeHistoryCursor(token string, scope tenancy.Scope, taskID id.TaskID) (historyCursor, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return historyCursor{}, fault.New(fault.InvalidInput, "history cursor is not a valid opaque token")
	}
	var cursor historyCursor
	if err := json.Unmarshal(decoded, &cursor); err != nil {
		return historyCursor{}, fault.New(fault.InvalidInput, "history cursor is not a valid opaque token")
	}
	if cursor.TenantID != string(scope.TenantID) || cursor.ProjectID != string(scope.ProjectID) || cursor.TaskID != string(taskID) {
		return historyCursor{}, fault.New(fault.Forbidden, "history cursor belongs to another task or scope")
	}
	if cursor.At.IsZero() || cursor.Kind == "" || cursor.ID == "" {
		return historyCursor{}, fault.New(fault.InvalidInput, "history cursor has no keyset")
	}
	return cursor, nil
}

// historyKindRank orders the history kinds deterministically inside one instant.
func historyKindRank(kind string) int {
	switch kind {
	case "execution":
		return 0
	case "message":
		return 1
	case "question":
		return 2
	default:
		return 3
	}
}
