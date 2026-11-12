package adaptercontract

import (
	"reflect"
	"testing"

	"github.com/optimiware/kodama/internal/shell/event"
	"github.com/optimiware/kodama/internal/shell/receipt"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/task"
)

type columnRef struct {
	table  string
	column string
}

// TestSchemaPersistsDomainFacts proves every cited S04/S07 fact and every field
// of the persisted domain task, question, receipt, event, audit and secret
// records has a declared column home. A field with no mapping fails the test
// rather than being silently dropped from the durable contract.
func TestSchemaPersistsDomainFacts(t *testing.T) {
	schema := store.SchemaDesign()

	recordMappings := map[string][]columnRef{
		"task.Task.ID":               {{"tasks", "id"}},
		"task.Task.Scope":            {{"tasks", "tenant_id"}, {"tasks", "project_id"}},
		"task.Task.Title":            {{"tasks", "title"}},
		"task.Task.Description":      {{"tasks", "description"}},
		"task.Task.Metadata":         {{"tasks", "metadata"}},
		"task.Task.Messages":         {{"task_messages", "task_id"}, {"task_messages", "author_id"}, {"task_messages", "at"}, {"task_messages", "body"}, {"task_messages", "refs"}},
		"task.Task.ContentVersion":   {{"tasks", "content_version"}},
		"task.Task.LifecycleVersion": {{"tasks", "lifecycle_version"}},
		"task.Task.State":            {{"tasks", "state"}},
		"task.Task.SelectedPipeline": {{"tasks", "pipeline_id"}},
		"task.Task.Inputs":           {{"tasks", "inputs"}},
		"task.Task.Initiator":        {{"tasks", "initiator_id"}},
		"task.Task.ExecutionHistory": {{"tasks", "execution_history"}},
		"task.Task.CurrentExecution": {{"tasks", "current_execution_id"}},
		"task.Task.Question":         {{"tasks", "question_id"}, {"tasks", "question_body"}, {"tasks", "question_assignment"}, {"tasks", "question_references"}, {"tasks", "question_version"}, {"tasks", "question_asked_by"}, {"tasks", "question_at"}},

		"task.Question.ID":         {{"tasks", "question_id"}},
		"task.Question.Body":       {{"tasks", "question_body"}},
		"task.Question.Assignment": {{"tasks", "question_assignment"}},
		"task.Question.References": {{"tasks", "question_references"}},
		"task.Question.Version":    {{"tasks", "question_version"}},
		"task.Question.AskedBy":    {{"tasks", "question_asked_by"}},
		"task.Question.At":         {{"tasks", "question_at"}},

		"receipt.Record.ID":           {{"receipts", "id"}},
		"receipt.Record.Key":          {{"receipts", "tenant_id"}, {"receipts", "project_id"}, {"receipts", "principal_id"}, {"receipts", "operation"}, {"receipts", "caller_key"}},
		"receipt.Record.ContentHash":  {{"receipts", "content_sha256"}},
		"receipt.Record.Result":       {{"receipts", "result"}},
		"receipt.Record.ExecutionID":  {{"receipts", "execution_id"}},
		"receipt.Record.SnapshotHash": {{"receipts", "snapshot_sha256"}},
		"receipt.Record.At":           {{"receipts", "created_at"}},

		"event.Envelope.ID":             {{"events", "id"}},
		"event.Envelope.Type":           {{"events", "type"}},
		"event.Envelope.ReportType":     {{"events", "report_type"}},
		"event.Envelope.Source":         {{"events", "source_kind"}, {"events", "source_principal"}},
		"event.Envelope.At":             {{"events", "at"}},
		"event.Envelope.Scope":          {{"events", "tenant_id"}, {"events", "project_id"}},
		"event.Envelope.SubjectID":      {{"events", "subject_id"}},
		"event.Envelope.SubjectVersion": {{"events", "subject_version"}},
		"event.Envelope.CorrelationID":  {{"events", "correlation_id"}},
		"event.Envelope.CausationID":    {{"events", "causation_id"}},
		"event.Envelope.Data":           {{"events", "data"}},

		"event.Audit.ID":        {{"audit", "id"}},
		"event.Audit.Scope":     {{"audit", "tenant_id"}, {"audit", "project_id"}},
		"event.Audit.Actor":     {{"audit", "actor"}},
		"event.Audit.Operation": {{"audit", "operation"}},
		"event.Audit.At":        {{"audit", "at"}},
		"event.Audit.Detail":    {{"audit", "detail"}},

		"secret.Metadata.ID":        {{"secrets", "id"}},
		"secret.Metadata.Scope":     {{"secrets", "tenant_id"}, {"secrets", "project_id"}},
		"secret.Metadata.Name":      {{"secrets", "name"}},
		"secret.Metadata.Version":   {{"secrets", "version"}},
		"secret.Metadata.UpdatedAt": {{"secrets", "updated_at"}},
	}

	records := []struct {
		name  string
		value any
	}{
		{"task.Task", task.Task{}},
		{"task.Question", task.Question{}},
		{"receipt.Record", receipt.Record{}},
		{"event.Envelope", event.Envelope{}},
		{"event.Audit", event.Audit{}},
		{"secret.Metadata", secret.Metadata{}},
	}
	used := make(map[string]bool, len(recordMappings))
	for _, record := range records {
		recordType := reflect.TypeOf(record.value)
		for index := 0; index < recordType.NumField(); index++ {
			field := recordType.Field(index)
			if field.PkgPath != "" {
				continue
			}
			key := record.name + "." + field.Name
			refs, ok := recordMappings[key]
			if !ok {
				t.Errorf("persisted field %s has no declared column mapping", key)
				continue
			}
			used[key] = true
			checkColumnRefs(t, schema, key, refs)
		}
	}
	for key := range recordMappings {
		if !used[key] {
			t.Errorf("column mapping %s does not name an exported persisted field", key)
		}
	}

	facts := []struct {
		name string
		refs []columnRef
	}{
		{"tenant encrypted derivation key", []columnRef{{"tenants", "derivation_key"}}},
		{"secret ciphertext", []columnRef{{"secrets", "ciphertext"}}},
		{"secret wrapped key", []columnRef{{"secrets", "wrapped_key"}}},
		{"secret key reference", []columnRef{{"secrets", "key_ref"}}},
		{"secret content nonce", []columnRef{{"secrets", "nonce"}}},
		{"secret current version", []columnRef{{"secrets", "version"}}},
		{"secret created time", []columnRef{{"secrets", "created_at"}}},
		{"secret updated time", []columnRef{{"secrets", "updated_at"}}},
		{"task title", []columnRef{{"tasks", "title"}}},
		{"task description", []columnRef{{"tasks", "description"}}},
		{"task metadata", []columnRef{{"tasks", "metadata"}}},
		{"task selected inputs", []columnRef{{"tasks", "inputs"}}},
		{"task outstanding question id", []columnRef{{"tasks", "question_id"}}},
		{"task outstanding question assignment", []columnRef{{"tasks", "question_assignment"}}},
		{"task outstanding question asker", []columnRef{{"tasks", "question_asked_by"}}},
		{"task outstanding question time", []columnRef{{"tasks", "question_at"}}},
		{"task outstanding question version", []columnRef{{"tasks", "question_version"}}},
		{"action request subject", []columnRef{{"actions", "request_subject"}}},
		{"action request assignment", []columnRef{{"actions", "request_assignment"}}},
		{"action request decision", []columnRef{{"actions", "request_decision"}}},
		{"action request actor", []columnRef{{"actions", "request_actor"}}},
		{"action request time", []columnRef{{"actions", "request_resolved_at"}}},
		{"binding resource id", []columnRef{{"resource_bindings", "resource_id"}}},
		{"receipt execution id", []columnRef{{"receipts", "execution_id"}}},
		{"receipt snapshot hash", []columnRef{{"receipts", "snapshot_sha256"}}},
		{"receipt byte-safe result", []columnRef{{"receipts", "result"}}},
		{"receipt completion marker", []columnRef{{"receipts", "completed_at"}}},
		{"event report type", []columnRef{{"events", "report_type"}}},
		{"event durable publication intent", []columnRef{{"events", "publish_state"}}},
		{"event durable publication cursor", []columnRef{{"events", "publish_sequence"}}},
		{"verification requested approval", []columnRef{{"verifications", "requested_approval"}}},
		{"verification approved result", []columnRef{{"verifications", "approved"}}},
	}
	for _, fact := range facts {
		checkColumnRefs(t, schema, fact.name, fact.refs)
	}
}

func checkColumnRefs(t *testing.T, schema store.Schema, name string, refs []columnRef) {
	t.Helper()
	if len(refs) == 0 {
		t.Errorf("%s has no declared column", name)
		return
	}
	for _, ref := range refs {
		table, ok := findTable(schema, ref.table)
		if !ok {
			t.Errorf("%s maps to missing table %q", name, ref.table)
			continue
		}
		if !hasColumn(table, ref.column) {
			t.Errorf("%s maps to missing column %s.%s", name, ref.table, ref.column)
		}
	}
}
