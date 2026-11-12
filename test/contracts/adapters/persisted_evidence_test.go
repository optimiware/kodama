package adaptercontract

import (
	"reflect"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/kernel"
	"github.com/optimiware/kodama/internal/shell/gateway"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/tenancy"
	"github.com/optimiware/kodama/internal/shell/trigger"
)

// TestSchemaPersistsKernelAndGatewayEvidence proves every exported field of the
// Kernel execution/action evidence and the gateway usage/content records has a
// declared column home (or an explicitly documented derivation). A field with
// no mapping fails the test rather than being silently dropped from the durable
// contract.
func TestSchemaPersistsKernelAndGatewayEvidence(t *testing.T) {
	schema := store.SchemaDesign()

	mappings := map[string][]columnRef{
		"kernel.Execution.ID":          {{"executions", "id"}},
		"kernel.Execution.Version":     {{"executions", "version"}},
		"kernel.Execution.State":       {{"executions", "state"}},
		"kernel.Execution.WorkspaceID": {{"executions", "workspace_id"}},
		"kernel.Execution.Snapshot":    {{"executions", "snapshot"}},
		"kernel.Execution.Actions":     {{"actions", "id"}},
		"kernel.Execution.Failure":     {{"executions", "failure"}},
		"kernel.Execution.Cleanup":     {{"cleanup", "state"}},

		"kernel.Action.ID":               {{"actions", "id"}},
		"kernel.Action.Index":            {{"actions", "index"}},
		"kernel.Action.Name":             {{"actions", "name"}},
		"kernel.Action.Kind":             {{"actions", "kind"}},
		"kernel.Action.State":            {{"actions", "state"}},
		"kernel.Action.Version":          {{"actions", "version"}},
		"kernel.Action.Timeout":          {{"actions", "timeout_ms"}},
		"kernel.Action.ActivatedAt":      {{"actions", "activated_at"}},
		"kernel.Action.Deadline":         {{"actions", "deadline"}},
		"kernel.Action.Outputs":          {{"actions", "outputs"}},
		"kernel.Action.StagedOutputs":    {{"actions", "staged_outputs"}},
		"kernel.Action.StagedOutputsSet": {{"actions", "staged_outputs_set"}},
		"kernel.Action.Stop":             {{"actions", "stop"}},
		"kernel.Action.Container":        {{"actions", "workload_id"}, {"actions", "launch_state"}, {"actions", "launch_key"}, {"actions", "observation"}},
		"kernel.Action.Request":          {{"actions", "request_subject"}, {"actions", "request_decision"}, {"actions", "request_actor"}, {"actions", "request_resolved_at"}, {"actions", "answer_provenance"}},

		"kernel.ContainerAction.Definition":  {{"executions", "snapshot"}},
		"kernel.ContainerAction.WorkloadID":  {{"actions", "workload_id"}},
		"kernel.ContainerAction.WorkspaceID": {{"executions", "workspace_id"}},
		"kernel.ContainerAction.Launch":      {{"actions", "launch_state"}, {"actions", "launch_key"}},
		"kernel.ContainerAction.Observation": {{"actions", "observation"}},

		"kernel.LaunchIntent.WorkloadID":     {{"actions", "workload_id"}},
		"kernel.LaunchIntent.State":          {{"actions", "launch_state"}},
		"kernel.LaunchIntent.IdempotencyKey": {{"actions", "launch_key"}},

		"kernel.Answer.Decision":   {{"actions", "request_decision"}},
		"kernel.Answer.Outputs":    {{"actions", "outputs"}},
		"kernel.Answer.Actor":      {{"actions", "request_actor"}},
		"kernel.Answer.At":         {{"actions", "request_resolved_at"}},
		"kernel.Answer.Provenance": {{"actions", "answer_provenance"}},

		"kernel.Failure.Kind":   {{"executions", "failure"}},
		"kernel.Failure.Reason": {{"executions", "failure"}},
		"kernel.Failure.Detail": {{"executions", "failure"}},

		"kernel.Observation.Kind":     {{"actions", "observation"}},
		"kernel.Observation.Proof":    {{"actions", "observation"}},
		"kernel.Observation.ExitCode": {{"actions", "observation"}},
		"kernel.Observation.Stop":     {{"actions", "observation"}},

		"kernel.StopEvidence.State":  {{"actions", "stop"}},
		"kernel.StopEvidence.Detail": {{"actions", "stop"}},

		"gateway.UsageRecord.ID":            {{"model_usage", "id"}},
		"gateway.UsageRecord.Scope":         {{"model_usage", "tenant_id"}, {"model_usage", "project_id"}},
		"gateway.UsageRecord.PrincipalID":   {{"model_usage", "principal_id"}},
		"gateway.UsageRecord.ExecutionID":   {{"model_usage", "execution_id"}},
		"gateway.UsageRecord.Model":         {{"model_usage", "model"}},
		"gateway.UsageRecord.UpstreamModel": {{"model_usage", "upstream_model"}},
		"gateway.UsageRecord.Outcome":       {{"model_usage", "outcome"}},
		"gateway.UsageRecord.UsageKnown":    {{"model_usage", "usage_known"}},
		"gateway.UsageRecord.Usage":         {{"model_usage", "prompt_tokens"}, {"model_usage", "completion_tokens"}, {"model_usage", "total_tokens"}},
		"gateway.UsageRecord.At":            {{"model_usage", "recorded_at"}},

		"gateway.ContentRecord.Scope":       {{"content_records", "tenant_id"}, {"content_records", "project_id"}},
		"gateway.ContentRecord.PrincipalID": {{"content_records", "principal_id"}},
		"gateway.ContentRecord.ExecutionID": {{"content_records", "execution_id"}},
		"gateway.ContentRecord.Model":       {{"content_records", "model"}},
		"gateway.ContentRecord.Request":     {{"content_records", "request_body"}},
		"gateway.ContentRecord.Response":    {{"content_records", "response_body"}},
		"gateway.ContentRecord.At":          {{"content_records", "recorded_at"}},
		"gateway.ContentRecord.RetainUntil": {{"content_records", "retain_until"}},

		"api.ChatUsage.PromptTokens":     {{"model_usage", "prompt_tokens"}},
		"api.ChatUsage.CompletionTokens": {{"model_usage", "completion_tokens"}},
		"api.ChatUsage.TotalTokens":      {{"model_usage", "total_tokens"}},

		"trigger.CompletionUnit.Owner":   {{"event_consumptions", "tenant_id"}, {"event_consumptions", "project_id"}},
		"trigger.CompletionUnit.EventID": {{"event_consumptions", "id"}},
	}

	// Derived fields have no column of their own because they are reconstructed
	// from another persisted row. The reason is part of the contract: a future
	// adapter may not drop the evidence, only derive it.
	derived := map[string]string{
		"kernel.Execution.CurrentAction":  "derived from the single running action row; there is no second active-action pointer column",
		"trigger.CompletionUnit.Sequence": "the completion marker is keyed by event id; the sequence orders the scan and is not stored twice",
	}

	records := []struct {
		name  string
		value any
	}{
		{"kernel.Execution", kernel.Execution{}},
		{"kernel.Action", kernel.Action{}},
		{"kernel.ContainerAction", kernel.ContainerAction{}},
		{"kernel.LaunchIntent", kernel.LaunchIntent{}},
		{"kernel.Answer", kernel.Answer{}},
		{"kernel.Failure", kernel.Failure{}},
		{"kernel.Observation", kernel.Observation{}},
		{"kernel.StopEvidence", kernel.StopEvidence{}},
		{"gateway.UsageRecord", gateway.UsageRecord{}},
		{"gateway.ContentRecord", gateway.ContentRecord{}},
		{"api.ChatUsage", api.ChatUsage{}},
		{"trigger.CompletionUnit", trigger.CompletionUnit{}},
	}

	usedMappings := make(map[string]bool, len(mappings))
	usedDerived := make(map[string]bool, len(derived))
	for _, record := range records {
		recordType := reflect.TypeOf(record.value)
		for index := 0; index < recordType.NumField(); index++ {
			field := recordType.Field(index)
			if field.PkgPath != "" {
				continue
			}
			key := record.name + "." + field.Name
			if refs, ok := mappings[key]; ok {
				usedMappings[key] = true
				checkColumnRefs(t, schema, key, refs)
				continue
			}
			if reason, ok := derived[key]; ok {
				if reason == "" {
					t.Errorf("derived field %s has no documented reason", key)
				}
				usedDerived[key] = true
				continue
			}
			t.Errorf("persisted field %s has no declared column mapping or documented derivation", key)
		}
	}
	for key := range mappings {
		if !usedMappings[key] {
			t.Errorf("column mapping %s does not name an exported persisted field", key)
		}
	}
	for key := range derived {
		if !usedDerived[key] {
			t.Errorf("derived mapping %s does not name an exported persisted field", key)
		}
	}
}

// TestModelUsageUnitTargetsDeclaredColumns proves the gateway's own usage unit
// writes only columns the model_usage table declares, so the adapter and the
// contract cannot drift.
func TestModelUsageUnitTargetsDeclaredColumns(t *testing.T) {
	record := gateway.UsageRecord{
		ID:            gateway.UsageID("usage-1"),
		Scope:         testScope(),
		PrincipalID:   "principal-1",
		Model:         "configured-model",
		UpstreamModel: "upstream-model",
		Outcome:       gateway.UsageSucceeded,
		UsageKnown:    true,
		Usage:         api.ChatUsage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3},
		At:            testTime(),
	}
	unit, err := record.Unit()
	if err != nil {
		t.Fatalf("UsageRecord.Unit() = %v", err)
	}
	table, ok := findTable(store.SchemaDesign(), gateway.UsageTable)
	if !ok {
		t.Fatalf("usage table %q is not in the declared schema", gateway.UsageTable)
	}
	for _, write := range unit.Writes {
		if write.Table != gateway.UsageTable {
			t.Errorf("usage unit writes table %q; want %q", write.Table, gateway.UsageTable)
		}
		for column := range write.Values {
			if !hasColumn(table, column) {
				t.Errorf("usage unit writes column %q that model_usage does not declare", column)
			}
		}
	}
}

func testScope() tenancy.Scope {
	return tenancy.Scope{TenantID: "tenant-1", ProjectID: "project-1"}
}

func testTime() time.Time {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
}
