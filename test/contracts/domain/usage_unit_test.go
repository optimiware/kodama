// The usage unit and its nullable execution identity:
// gateway.UsageRecord.Unit writes SQL NULL for a record that names no
// execution and the execution identity as text otherwise. The gateway commits
// that unit as built.

package domaincontract

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/gateway"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

var recordedAt = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)

func usageRecord(execution id.ExecutionID) gateway.UsageRecord {
	return gateway.UsageRecord{
		ID:            gateway.UsageID("usage-1"),
		Scope:         tenancy.Scope{TenantID: "tenant-1", ProjectID: "project-1"},
		PrincipalID:   id.PrincipalID("principal-1"),
		ExecutionID:   execution,
		Model:         "configured-model",
		UpstreamModel: "upstream-model",
		Outcome:       gateway.UsageSucceeded,
		UsageKnown:    true,
		Usage:         api.ChatUsage{PromptTokens: 7, CompletionTokens: 5, TotalTokens: 12},
		At:            recordedAt,
	}
}

// TestUsageUnitExecutionIdentityVectors pins the NULL rule on the pinned
// builder with whole-value vectors: the execution_id key is always present, an
// absent identity is an untyped nil and a set identity is its text. Every
// other written value is identical in both cases.
func TestUsageUnitExecutionIdentityVectors(t *testing.T) {
	vectors := []struct {
		name      string
		execution id.ExecutionID
		want      any
	}{
		{name: "no execution is SQL NULL", execution: "", want: nil},
		{name: "an execution is its text", execution: "execution-1", want: "execution-1"},
	}
	for _, vector := range vectors {
		t.Run(vector.name, func(t *testing.T) {
			unit, err := usageRecord(vector.execution).Unit()
			if err != nil {
				t.Fatalf("UsageRecord.Unit() = %v", err)
			}
			if unit.Receipt != nil || len(unit.Locks) != 0 || len(unit.Audit) != 0 || len(unit.Events) != 0 {
				t.Fatalf("usage unit carries receipt/lock/audit/event intent: %+v", unit)
			}
			if len(unit.Writes) != 1 {
				t.Fatalf("usage unit writes = %d; want exactly 1", len(unit.Writes))
			}
			write := unit.Writes[0]
			if write.Table != gateway.UsageTable || write.ID != "usage-1" || !write.Insert || write.Delete {
				t.Fatalf("usage write = table %q id %q insert %v delete %v; want one %s insert of usage-1",
					write.Table, write.ID, write.Insert, write.Delete, gateway.UsageTable)
			}
			want := map[string]any{
				"principal_id":      "principal-1",
				"execution_id":      vector.want,
				"model":             "configured-model",
				"upstream_model":    "upstream-model",
				"outcome":           "succeeded",
				"usage_known":       true,
				"prompt_tokens":     int64(7),
				"completion_tokens": int64(5),
				"total_tokens":      int64(12),
				"recorded_at":       recordedAt,
				"version":           uint64(1),
			}
			if !reflect.DeepEqual(write.Values, want) {
				t.Fatalf("usage values = %#v; want %#v", write.Values, want)
			}
			value, present := write.Values["execution_id"]
			if !present {
				t.Fatal("usage unit omits execution_id; the nullable column must always be present")
			}
			if vector.want == nil && value != nil {
				t.Fatalf("execution_id = %#v (%T); want an untyped nil, not an empty or typed value", value, value)
			}
		})
	}
}

// TestUsageUnitRefusesAnInvalidExecutionIdentity pins that the NULL rule is not
// a normalisation of bad input: only the empty identity means "no execution";
// a non-empty identity that is not a valid execution id is still refused.
func TestUsageUnitRefusesAnInvalidExecutionIdentity(t *testing.T) {
	for _, execution := range []id.ExecutionID{" ", "execution 1", id.ExecutionID(strings.Repeat("e", id.MaxIDLength+1))} {
		if _, err := usageRecord(execution).Unit(); err == nil {
			t.Errorf("UsageRecord.Unit() accepted the invalid execution id %q", execution)
		}
	}
}

// TestUsageExecutionColumnIsNullableWithItsForeignKey is the cross-contract
// reason for the rule: the model_usage.execution_id column is nullable
// text under the composite model_usage_execution foreign key, so an absent
// execution can only be stored as NULL. An empty string is a value the key
// evaluates and refuses.
func TestUsageExecutionColumnIsNullableWithItsForeignKey(t *testing.T) {
	var usage *store.Table
	schema := store.SchemaDesign()
	for index := range schema.Tables {
		if schema.Tables[index].Name == gateway.UsageTable {
			usage = &schema.Tables[index]
		}
	}
	if usage == nil {
		t.Fatalf("table %q is not in the declared schema", gateway.UsageTable)
	}
	found := false
	for _, column := range usage.Columns {
		if column.Name != "execution_id" {
			continue
		}
		found = true
		if !column.Nullable || column.Type != "text" {
			t.Errorf("model_usage.execution_id = type %q nullable %v; want nullable text", column.Type, column.Nullable)
		}
	}
	if !found {
		t.Fatal("model_usage has no execution_id column")
	}
	found = false
	for _, key := range usage.ForeignKeys {
		if key.Name != "model_usage_execution" {
			continue
		}
		found = true
		if key.References != "executions" ||
			!reflect.DeepEqual(key.Columns, []string{"tenant_id", "project_id", "execution_id"}) ||
			!reflect.DeepEqual(key.ReferencedColumns, []string{"tenant_id", "project_id", "id"}) {
			t.Errorf("model_usage_execution = %v -> %s%v; want (tenant_id, project_id, execution_id) -> executions(tenant_id, project_id, id)",
				key.Columns, key.References, key.ReferencedColumns)
		}
	}
	if !found {
		t.Fatal("model_usage has no model_usage_execution foreign key")
	}
}
